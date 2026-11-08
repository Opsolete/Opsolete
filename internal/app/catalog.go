package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// The template catalog: TEMPLATES_DIR, or a Git repository of a VCS connection (Settings → Templates,
// see vcs.SyncCatalog) that replaces it. Admins edit templates in the builder when it's on: with
// TEMPLATE_EDITOR it writes into TEMPLATES_DIR, with a Git catalog it commits or opens a pull request
// as the catalog's editor mode says. Everyone reads the catalog (Template).

var (
	errBuilderOff = reason{ErrForbidden, "the template builder is off"}
	errNoCatalog  = invalid("connection", "choose a VCS connection")
)

// TemplateCatalog returns the Git catalog's settings and sync status; nil when the templates come
// from TEMPLATES_DIR. Admins only.
func (a *App) TemplateCatalog(ctx context.Context, actor Actor) (*storage.TemplateCatalog, error) {
	if !actor.Admin {
		return nil, errAdminsOnly
	}
	tc, err := a.Store.GetTemplateCatalog(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return &tc, err
}

// CatalogSettings is where the template catalog lives in Git, as typed.
type CatalogSettings struct {
	ConnectionID      uuid.UUID
	Repo, Branch, Dir string // Branch "" is main; Dir "" the root
	Mode              string // what saving a template in the builder does: off, commit or pr
}

// SetTemplateCatalog points the catalog at a branch of a repository the connection can read, and
// loads it. Admins only; ValidationError: mode, branch, dir, connection, repo (the branch can't be
// read), and a Message when it was saved but loading the templates failed (the sync error).
func (a *App) SetTemplateCatalog(ctx context.Context, actor Actor, in CatalogSettings) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	dir, dirOK := RepoDir(in.Dir)
	tc := storage.TemplateCatalog{ConnectionID: in.ConnectionID, Repo: strings.TrimSpace(in.Repo), Branch: strings.TrimSpace(in.Branch), Dir: dir, Mode: in.Mode}
	if tc.Branch == "" {
		tc.Branch = "main"
	}
	switch {
	case tc.Mode != "off" && tc.Mode != "commit" && tc.Mode != "pr":
		return invalid("mode", "choose what saving a template does")
	case !ValidBranch(tc.Branch):
		return invalid("branch", fmt.Sprintf("%q is not a branch name", tc.Branch))
	case !dirOK:
		return invalid("dir", fmt.Sprintf("%q is not a folder of the repository", in.Dir))
	}
	sc, err := a.Store.GetVCSConnection(ctx, tc.ConnectionID)
	if errors.Is(err, storage.ErrNotFound) {
		return errNoCatalog
	} else if err != nil {
		return err
	}
	c, err := vcs.ConnFor(a.SecretKey, sc)
	if err == nil {
		_, err = c.Head(ctx, tc.Repo, tc.Branch)
	}
	if err != nil {
		return invalid("repo", fmt.Sprintf("can't read branch %s of %q: %v", tc.Branch, tc.Repo, err))
	}
	if err := a.Store.SetTemplateCatalog(ctx, tc); err != nil {
		return err
	}
	a.Log.Info("template catalog set", "connection", sc.Name, "repo", tc.Repo, "branch", tc.Branch, "dir", tc.Dir, "mode", tc.Mode, "user", actor.Email)
	if err := vcs.SyncCatalog(ctx, a.Store, a.SecretKey, a.Templates); err != nil {
		return &ValidationError{Message: "Saved, but loading the templates failed: " + err.Error()}
	}
	return nil
}

// SyncCatalog loads the catalog branch's head now. A failure is recorded as the catalog's sync
// error (shown in its settings), not returned. Admins only.
func (a *App) SyncCatalog(ctx context.Context, actor Actor) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	a.syncCatalog(ctx)
	return nil
}

func (a *App) syncCatalog(ctx context.Context) {
	if err := vcs.SyncCatalog(ctx, a.Store, a.SecretKey, a.Templates); err != nil {
		a.Log.Warn("template catalog sync", "err", err) // recorded as the sync error
	}
}

// DisconnectCatalog goes back to TEMPLATES_DIR. Admins only.
func (a *App) DisconnectCatalog(ctx context.Context, actor Actor) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	if err := a.Store.DeleteTemplateCatalog(ctx); err != nil {
		return err
	}
	a.Log.Info("template catalog disconnected", "user", actor.Email)
	return a.Templates.Reopen()
}

// CanEditTemplates: actor is an admin and the template builder is on (TemplateEditor for
// TEMPLATES_DIR, or the Git catalog's editor mode commit or pr).
func (a *App) CanEditTemplates(actor Actor) bool {
	if !actor.Admin {
		return false
	}
	switch mode, _ := a.Templates.Source(); mode {
	case "":
		return a.TemplateEditor
	case "commit", "pr":
		return true
	}
	return false
}

func (a *App) editor(actor Actor) error {
	switch {
	case !actor.Admin:
		return errAdminsOnly
	case !a.CanEditTemplates(actor):
		return errBuilderOff
	}
	return nil
}

// CheckTemplates fails if a template of the catalog pins a module of this registry while it is
// disabled: it would only fail later, on the runner.
func (a *App) CheckTemplates() error {
	for _, t := range a.Templates.List() {
		if err := a.checkSource(t); err != nil {
			return fmt.Errorf("template %s: %w", t.ID, err)
		}
	}
	return nil
}

func (a *App) checkSource(t templates.Template) error {
	if _, _, _, ok := a.Registry.ParseSource(t.Module.Source); ok && !a.RegistryEnabled {
		return fmt.Errorf("module %s is in this registry, which is disabled (REGISTRY_ENABLED=false)", t.Module.Source)
	}
	return nil
}

// ModuleTemplate starts a template from a registry module version: its source and ref, its name,
// and inputs generated from its variables; skipped are the variables it couldn't generate.
// ErrNotFound for an unknown version.
func (a *App) ModuleTemplate(ctx context.Context, ns, name, sys, version string) (t templates.Template, skipped []string, err error) {
	m, err := a.Store.GetModule(ctx, ns, name, sys, version)
	if err != nil {
		return t, nil, err
	}
	_, _, _, source, err := a.Registry.Address(m.Namespace, m.Name, m.System)
	if err != nil {
		return t, nil, err
	}
	t.Inputs, skipped = registry.Inputs(m.Variables)
	t.Name, t.Module = m.Name, templates.Module{Source: source, Ref: m.Version}
	return t, skipped, nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`) // uuid.NewString's format

// checkTemplate gives t its id and checks it: editing keeps t.ID (the file name, it can't change)
// and needs the template to exist (ErrNotFound); a new one keeps the id an earlier save of the same
// form gave it, else gets a new one. ValidationError: a source this instance can't serve, a registry
// module version that doesn't exist (the deploy would fail later), or an invalid template.
func (a *App) checkTemplate(ctx context.Context, t *templates.Template, editing bool) error {
	if editing {
		if _, ok := a.Templates.Get(t.ID); !ok {
			return ErrNotFound
		}
	} else if !uuidRe.MatchString(t.ID) {
		t.ID = uuid.NewString()
	}
	if err := a.checkSource(*t); err != nil {
		return &ValidationError{Message: err.Error()}
	}
	if ns, name, sys, ok := a.Registry.ParseSource(t.Module.Source); ok {
		if _, err := a.Store.GetModule(ctx, ns, name, sys, t.Module.Ref); errors.Is(err, storage.ErrNotFound) {
			return &ValidationError{Message: fmt.Sprintf("registry module %s has no version %q", t.Module.Source, t.Module.Ref)}
		} else if err != nil {
			return err
		}
	}
	if _, err := templates.Encode(*t); err != nil {
		return &ValidationError{Message: err.Error()}
	}
	return nil
}

// PreviewTemplate checks t as SaveCatalogTemplate does, without saving it, and returns it resolved
// (Resolve): the deploy form as end users would get it.
func (a *App) PreviewTemplate(ctx context.Context, actor Actor, t templates.Template, editing bool) (templates.Template, error) {
	if err := a.editor(actor); err != nil {
		return t, err
	}
	if err := a.checkTemplate(ctx, &t, editing); err != nil {
		return t, err
	}
	return a.Resolve(ctx, t)
}

// SaveCatalogTemplate checks t (see checkTemplate) and saves it: into TEMPLATES_DIR, or to the Git
// catalog as its editor mode says, a commit (the catalog reloads) or a pull request (pr: its URL;
// the catalog changes once it's merged). It returns t with its id. Needs the builder on (admins);
// other errors are the directory's or the Git provider's refusal.
func (a *App) SaveCatalogTemplate(ctx context.Context, actor Actor, t templates.Template, editing bool) (_ templates.Template, pr string, err error) {
	if err := a.editor(actor); err != nil {
		return t, "", err
	}
	if err := a.checkTemplate(ctx, &t, editing); err != nil {
		return t, "", err
	}
	if mode, _ := a.Templates.Source(); mode != "" {
		pr, err = vcs.SaveTemplate(ctx, a.Store, a.SecretKey, a.Templates, t, actor.Email)
	} else {
		err = a.Templates.Save(t, editing)
	}
	if err != nil {
		return t, "", err
	}
	a.Log.Info("template saved", "id", t.ID, "pull_request", pr, "user", actor.Email)
	return t, pr, nil
}

// DeleteCatalogTemplate removes template id from the catalog, like SaveCatalogTemplate. Workspaces
// that added it keep their pinned module; they just can't be reconfigured from it. ErrNotFound.
func (a *App) DeleteCatalogTemplate(ctx context.Context, actor Actor, id string) (_ templates.Template, pr string, err error) {
	if err := a.editor(actor); err != nil {
		return templates.Template{}, "", err
	}
	t, ok := a.Templates.Get(id)
	if !ok {
		return t, "", ErrNotFound
	}
	if mode, _ := a.Templates.Source(); mode != "" {
		pr, err = vcs.DeleteTemplate(ctx, a.Store, a.SecretKey, a.Templates, t.ID, actor.Email)
	} else {
		err = a.Templates.Delete(t.ID)
	}
	if err != nil {
		return t, "", err
	}
	a.Log.Info("template deleted", "id", t.ID, "pull_request", pr, "user", actor.Email)
	return t, pr, nil
}
