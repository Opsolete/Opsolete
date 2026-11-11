package app

// Promotion: a workspace's templates (the source's) are copied to another workspace (the target), an
// existing one or a new one, which becomes an exact copy of them: the same module names, template
// ids, module versions and non-sensitive inputs; the target's templates the source lacks are removed
// (their resources destroyed by the same run). Not copied: per-environment inputs (the target's
// project value applies at run time, else the target keeps its own value), sensitive inputs (the
// target's own value is kept, else asked for), VCS links, variables and settings. One run on the
// target records it all (storage.Promote) and always waits for confirmation.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// What a promotion does with a module of the target.
const (
	PromoteAdd       = "add"       // the target doesn't have it
	PromoteUpdate    = "update"    // another version or other inputs
	PromoteReplace   = "replace"   // the module name holds another template in the target: remove + add
	PromoteRemove    = "remove"    // only in the target: its resources are destroyed
	PromoteUnchanged = "unchanged" // the same already
)

// Where a per-environment input's value comes from in the target.
const (
	EnvProject = "project" // the target's project's value, at every run
	EnvOwn     = "own"     // the target's own value, kept
)

// PromoteTarget is where a promotion goes: an existing workspace, or a new one (New) in Project.
type PromoteTarget struct {
	Name    string
	New     bool
	Project string // a new one's project, by name; "" the default
}

// PromoteInput is an input of a promoted module worth showing: a changed value, a per-environment
// or sensitive one, or one the promotion asks for.
type PromoteInput struct {
	templates.Input
	From, To string // non-sensitive values as the form says them; From "": none (or not shown)
	Env      string // per-environment: EnvProject, EnvOwn; "" asked for
	Kept     bool   // sensitive: the target's value is kept
	Field    string // asked for: the form field (module.input); "" not asked
	Value    string // asked for: the field's value (posted, else the input's default)
}

// PromoteModule is one module of the target and what the promotion does with it.
type PromoteModule struct {
	Module         string
	Change         string // Promote*
	Template       string // the template's name; its id if the catalog lacks it
	FromRef, ToRef string // the target's version and the promoted one ("" none)
	FromSrc, ToSrc string // module sources likewise
	Resources      int    // remove, replace: the resources the target's state holds in it
	Inputs         []PromoteInput
	Invalid        []string // problems with copied values that no field fixes: the promotion is refused
	template       templates.Template
	vals           map[string]any
	fieldErrs      map[string]string
}

// Promotion is a promotion's preview: what it changes in the target.
type Promotion struct {
	Source  storage.Workspace
	Target  storage.Workspace // a new one: its name and project only
	New     bool
	Modules []PromoteModule // by module name
	Changes int             // modules that change
	Digest  string          // of what the preview was worked out from: Promote refuses another (ErrChanged)
	// Destroy: the modules a first run destroys (all it removes or replaces, as one is deployed),
	// before the apply run; nil: one run. DestroyResources: how many resources they hold.
	Destroy          []string
	DestroyResources int
	read             []storage.WorkspaceTemplate
}

// Asks: the promotion asks for at least one value.
func (p Promotion) Asks() bool {
	return slices.ContainsFunc(p.Modules, func(m PromoteModule) bool {
		return slices.ContainsFunc(m.Inputs, func(in PromoteInput) bool { return in.Field != "" })
	})
}

// PromoteTargets is what the "Promote to…" picker offers.
type PromoteTargets struct {
	Source     storage.Workspace
	Workspaces []storage.Workspace // existing workspaces actor operates, remote ones, not the source
	Projects   []storage.Project   // where actor may create a new one
}

// PromoteTargets lists the targets actor may promote source to. Needs read on the source.
// shortcut: an access query per listed workspace (at most 200); one query if the list gets slow.
func (a *App) PromoteTargets(ctx context.Context, actor Actor, source string) (PromoteTargets, error) {
	var out PromoteTargets
	var err error
	if out.Source, _, err = a.Workspace(ctx, actor, source, LevelRead); err != nil {
		return out, err
	}
	list, err := a.ListWorkspaces(ctx, actor, "")
	if err != nil {
		return out, err
	}
	for _, w := range list {
		if w.ID == out.Source.ID || w.ExecutionMode != "remote" {
			continue
		}
		if _, _, err := a.access(ctx, actor, w, LevelOperate); err == nil {
			out.Workspaces = append(out.Workspaces, w)
		} else if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
			return out, err
		}
	}
	slices.SortFunc(out.Workspaces, func(x, y storage.Workspace) int { return strings.Compare(x.Name, y.Name) })
	out.Projects, err = a.CreatableProjects(ctx, actor)
	return out, err
}

// PreviewPromotion works out what promoting source to the target does; values are the asked-for
// inputs as posted, by field (nil: not posted, the inputs' defaults). Needs read on the source and operate on an existing
// target, or creating workspaces in a new one's project (ValidationError: FieldName, FieldProject).
// Conflicts: the target uses local execution, the source has no templates, a template isn't in the
// catalog anymore.
func (a *App) PreviewPromotion(ctx context.Context, actor Actor, source string, to PromoteTarget, values map[string]string) (Promotion, error) {
	var p Promotion
	var err error
	if p.Source, _, err = a.Workspace(ctx, actor, source, LevelRead); err != nil {
		return p, err
	}
	if p.Target, err = a.promoteTarget(ctx, actor, p.Source, to); err != nil {
		return p, err
	}
	p.New = to.New
	src, err := a.Store.WorkspaceTemplates(ctx, p.Source.ID)
	if err != nil {
		return p, err
	}
	if len(src) == 0 {
		return p, reason{ErrNotPromotable, p.Source.Name + " has no templates to promote"}
	}
	idx := storage.ModuleIndex{Indexed: true}
	if !p.New {
		if p.read, err = a.Store.WorkspaceTemplates(ctx, p.Target.ID); err != nil {
			return p, err
		}
		if idx, err = a.Store.ModuleIndex(ctx, p.Target.ID); err != nil {
			return p, err
		}
	}
	env, err := a.ProjectEnvironment(ctx, p.Target.ProjectID)
	if err != nil {
		return p, err
	}
	for _, s := range src {
		var cur *storage.WorkspaceTemplate
		if i := slices.IndexFunc(p.read, func(t storage.WorkspaceTemplate) bool { return t.Name == s.Name }); i >= 0 {
			cur = &p.read[i]
		}
		m, err := a.promoteModule(ctx, s, cur, env, p.Target.ID, values)
		if err != nil {
			return p, err
		}
		if m.Change == PromoteReplace {
			m.Resources = idx.Resources[m.Module]
		}
		p.Modules = append(p.Modules, m)
	}
	for _, t := range p.read {
		if !slices.ContainsFunc(src, func(s storage.WorkspaceTemplate) bool { return s.Name == t.Name }) {
			m := PromoteModule{Module: t.Name, Change: PromoteRemove, Template: t.TemplateID, FromRef: t.ModuleRef, FromSrc: t.ModuleSource, Resources: idx.Resources[t.Name]}
			if ct, ok := a.Templates.Get(t.TemplateID); ok {
				m.Template = ct.Name
			}
			p.Modules = append(p.Modules, m)
		}
	}
	slices.SortFunc(p.Modules, func(x, y PromoteModule) int { return strings.Compare(x.Module, y.Module) })
	deployed := false
	for _, m := range p.Modules {
		if m.Change != PromoteUnchanged {
			p.Changes++
		}
		if m.Change == PromoteRemove || m.Change == PromoteReplace {
			p.Destroy = append(p.Destroy, m.Module)
			p.DestroyResources += m.Resources
			deployed = deployed || idx.Deployed(m.Module)
		}
	}
	if !deployed { // see storage.Promote: the same rule
		p.Destroy, p.DestroyResources = nil, 0
	}
	digest, err := json.Marshal([]any{src, p.read, p.Target.Name, p.Target.ProjectID})
	if err != nil {
		return p, err
	}
	sum := sha256.Sum256(digest)
	p.Digest = hex.EncodeToString(sum[:16])
	return p, nil
}

// promoteTarget loads an existing target (operate; not the source; remote execution), or checks a
// new one's name and project.
func (a *App) promoteTarget(ctx context.Context, actor Actor, source storage.Workspace, to PromoteTarget) (storage.Workspace, error) {
	if to.New {
		errs := map[string]string{}
		if !ValidName(to.Name) {
			errs[FieldName] = "a workspace name is " + NameRule
		} else if _, err := a.Store.GetWorkspace(ctx, to.Name); err == nil {
			errs[FieldName] = "a workspace named " + to.Name + " exists: pick it as an existing target, or another name"
		} else if !errors.Is(err, storage.ErrNotFound) {
			return storage.Workspace{}, err
		}
		p, err := a.NewWorkspaceProject(ctx, actor, to.Project, uuid.Nil)
		if ve, ok := errors.AsType[*ValidationError](err); ok {
			maps.Copy(errs, ve.Fields)
		} else if err != nil {
			return storage.Workspace{}, err
		}
		if len(errs) > 0 {
			return storage.Workspace{}, &ValidationError{Fields: errs}
		}
		return storage.Workspace{ID: uuid.New(), Name: to.Name, ProjectID: p.ID, ProjectName: p.Name, ExecutionMode: "remote"}, nil
	}
	ws, _, err := a.Workspace(ctx, actor, to.Name, LevelOperate)
	switch {
	case err != nil:
		return ws, err
	case ws.ID == source.ID:
		return ws, invalid(FieldName, "a workspace can't be promoted to itself: pick another target")
	case ws.ExecutionMode != "remote":
		return ws, reason{storage.ErrLocalExecution, ws.Name + " uses local execution: runs can't start there, so it can't be promoted to"}
	}
	return ws, nil
}

// promoteModule works out source template s as the target gets it, next to the target's module of
// the same name (cur; nil none), with the target's project values env. Sensitive inputs are sealed to
// the target (targetID) and the module.
func (a *App) promoteModule(ctx context.Context, s storage.WorkspaceTemplate, cur *storage.WorkspaceTemplate, env EnvironmentMap,
	targetID uuid.UUID, values map[string]string) (PromoteModule, error) {
	m := PromoteModule{Module: s.Name, Template: s.TemplateID, ToRef: s.ModuleRef, ToSrc: s.ModuleSource, fieldErrs: map[string]string{}}
	ct, ok := a.Templates.Get(s.TemplateID)
	if !ok {
		return m, reason{ErrNotPromotable, fmt.Sprintf("module %s: its template %s isn't in the catalog anymore, so its inputs can't be checked", s.Name, s.TemplateID)}
	}
	ct.Module = templates.Module{Source: s.ModuleSource, Ref: s.ModuleRef} // the inputs of the version the source runs
	t, err := a.Resolve(ctx, ct)
	if err != nil {
		return m, err
	}
	m.Template, m.template = t.Name, t
	own := map[string]any{} // the target's values a promotion keeps: of the same template only
	switch {
	case cur == nil:
		m.Change = PromoteAdd
	case cur.TemplateID != s.TemplateID:
		m.Change, m.FromRef, m.FromSrc = PromoteReplace, cur.ModuleRef, cur.ModuleSource
	default:
		m.FromRef, m.FromSrc, own = cur.ModuleRef, cur.ModuleSource, cur.Inputs
	}
	locked := env.Locked(t, own)
	raw, asked := map[string]string{}, map[string]bool{}
	for _, in := range t.Inputs {
		v, has := s.Inputs[in.Name]
		o, hasOwn := own[in.Name]
		_, isLocked := locked[in.Name]
		switch {
		case in.Sensitive:
			asked[in.Name] = !hasOwn // kept: SealInputs takes it from own
		case in.PerEnvironment && isLocked: // lockInputs
		case in.PerEnvironment && hasOwn && !IsSealed(o):
			raw[in.Name] = templates.FormatValue(o)
		case in.PerEnvironment, IsSealed(v): // nothing to carry over
			asked[in.Name] = true
		case !has: // left empty in the source, or hidden there: needed if required (then it was hidden)
			asked[in.Name] = in.Required
		default:
			raw[in.Name] = templates.FormatValue(v)
		}
		if asked[in.Name] {
			field := s.Name + "." + in.Name
			if values != nil { // posted: an unchecked box isn't sent
				raw[in.Name] = values[field]
			} else if !in.Sensitive {
				raw[in.Name] = in.Default
			}
		}
	}
	raw = lockInputs(raw, locked)
	vals, errs := t.Validate(raw)
	if err := a.SealInputs(t, raw, vals, own, errs, targetID, s.Name); err != nil {
		return m, err
	}
	keepOwn(vals, errs, locked, own)
	m.vals = vals
	for _, in := range t.Inputs {
		if !t.Match(in.ShowIf, raw) {
			continue
		}
		pi := PromoteInput{Input: in}
		if asked[in.Name] {
			pi.Field = s.Name + "." + in.Name
			if !in.Sensitive {
				pi.Value = raw[in.Name]
			}
		}
		switch {
		case in.Sensitive:
			pi.Kept = !asked[in.Name]
		case in.PerEnvironment:
			if v, ok := locked[in.Name]; ok {
				pi.Env, pi.To = EnvProject, ValueText(in, v)
			} else if !asked[in.Name] {
				pi.Env, pi.To = EnvOwn, ValueText(in, raw[in.Name])
			}
		default:
			if v, ok := vals[in.Name]; ok {
				pi.To = ValueText(in, templates.FormatValue(v))
			}
			if v, ok := own[in.Name]; ok && !IsSealed(v) {
				pi.From = ValueText(in, templates.FormatValue(v))
			}
			if pi.From == pi.To && pi.Field == "" {
				continue // unchanged: not worth a line
			}
		}
		if e := errs[in.Name]; e != "" {
			if pi.Field != "" {
				m.fieldErrs[pi.Field] = e
			} else {
				m.Invalid = append(m.Invalid, in.Title()+": "+e)
			}
		}
		m.Inputs = append(m.Inputs, pi)
	}
	if m.Change == "" {
		m.Change = PromoteUnchanged
		if cur.ModuleSource != s.ModuleSource || cur.ModuleRef != s.ModuleRef || !sameInputs(vals, cur.Inputs) {
			m.Change = PromoteUpdate
		}
	}
	return m, nil
}

// sameInputs: a and b hold the same values (as stored: JSON).
func sameInputs(a, b map[string]any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// Promote promotes source to the target as the preview with digest showed it (else ErrChanged), with
// the asked-for values (by field), and returns the target's run. ValidationError: the target (see
// PreviewPromotion) and the asked-for values (Fields by field); conflicts: a run is active on the
// target, nothing would change, and PreviewPromotion's.
func (a *App) Promote(ctx context.Context, actor Actor, source string, to PromoteTarget, values map[string]string, digest string) (Promotion, storage.Run, error) {
	p, err := a.PreviewPromotion(ctx, actor, source, to, values)
	if err != nil {
		return p, storage.Run{}, err
	}
	if digest != p.Digest {
		return p, storage.Run{}, reason{ErrChanged, "the source or the target changed since this preview: check it again"}
	}
	ve := &ValidationError{Fields: map[string]string{}}
	var set []storage.WorkspaceTemplate
	for _, m := range p.Modules {
		maps.Copy(ve.Fields, m.fieldErrs)
		for _, e := range m.Invalid {
			ve.Message = strings.TrimPrefix(ve.Message+"; ", "; ") + "module " + m.Module + ", " + e
		}
		if m.Change != PromoteRemove {
			set = append(set, storage.WorkspaceTemplate{Name: m.Module, TemplateID: m.template.ID, ModuleSource: m.ToSrc, ModuleRef: m.ToRef, Inputs: m.vals})
		}
	}
	if len(ve.Fields) > 0 || ve.Message != "" {
		return p, storage.Run{}, ve
	}
	if p.Changes == 0 {
		return p, storage.Run{}, reason{ErrNotPromotable, p.Target.Name + " already runs what " + p.Source.Name + " runs: nothing to promote"}
	}
	sp := storage.Promotion{WorkspaceID: p.Target.ID, Read: p.read, Templates: set, Source: p.Source.Name, CreatedBy: actor.UserID, Actor: actor.Email}
	if p.New {
		sp.New = &storage.EmptyWorkspace{Name: p.Target.Name, Project: p.Target.ProjectName, CreatedBy: actor.UserID}
	}
	run, err := a.Store.Promote(ctx, sp)
	switch {
	case errors.Is(err, storage.ErrNameTaken):
		return p, run, invalid(FieldName, err.Error())
	case errors.Is(err, storage.ErrChanged):
		return p, run, reason{ErrChanged, "the target's templates changed since this preview: check it again"}
	case err != nil:
		return p, run, err
	}
	// chained: run is the destroy; the apply run's queueing gets its own entry ("promote apply queued")
	if err := a.Store.Audit(ctx, actor.Email, "promote", map[string]any{"source": p.Source.Name, "target": p.Target.Name,
		"new": p.New, "run": run.ID, "chained": run.Action == "destroy"}); err != nil {
		a.Log.Error("audit log write failed", "err", err, "workspace", p.Target.Name)
	}
	a.Log.Info("workspace promoted", "source", p.Source.Name, "target", p.Target.Name, "new", p.New, "run", run.ID, "user", actor.Email)
	return p, run, nil
}
