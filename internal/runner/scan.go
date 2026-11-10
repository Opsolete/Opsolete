package runner

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// A scan finds existing resources a template can adopt (its adopt entries with list), maps their attributes to
// the template's inputs, and checks the fit with one speculative plan that adopts them all, each as its own
// module, into a throwaway local state. It writes nothing: no backend, no lock, no apply.

// maxCandidates bounds a scan's fit check, one module per candidate in a single plan.
// shortcut: candidates past it are listed as unchecked; batch the fit check when scans get that large.
const maxCandidates = 50

// found is one resource terraform query listed.
type found struct {
	name string
	obj  map[string]any // its attributes (include_resource)
}

func (r *Runner) scan(ctx context.Context, tf *tfProc, dir string, job runnerapi.Job, w io.Writer,
	fail func(string, error) runnerapi.FinishRequest) runnerapi.FinishRequest {
	var t templates.Template
	if err := json.Unmarshal(job.Scan, &t); err != nil || len(job.Modules) != 1 {
		return fail("read scan", fmt.Errorf("invalid scan job: %v", err))
	}
	var lists []string
	for _, a := range t.Adopt {
		if a.List != "" && !slices.Contains(lists, a.List) {
			lists = append(lists, a.List)
		}
	}
	qdir := filepath.Join(dir, "query")
	if err := os.Mkdir(qdir, 0o700); err != nil {
		return fail("prepare scan", err)
	}
	if err := writeProviders(qdir, lists); err != nil {
		return fail("prepare scan", err)
	}
	var q strings.Builder
	for i, l := range lists { // listRe-checked types: safe in HCL
		prov, _, _ := strings.Cut(l, "_")
		fmt.Fprintf(&q, "list %q \"l%d\" {\n  provider         = %s\n  include_resource = true\n  limit            = 1000\n}\n", l, i, prov)
	}
	if err := os.WriteFile(filepath.Join(qdir, "scan.tfquery.hcl"), []byte(q.String()), 0o600); err != nil {
		return fail("prepare scan", err)
	}
	tf.dir = qdir
	fmt.Fprintf(w, "==> terraform init (providers for %s)\n", strings.Join(lists, ", "))
	if _, err := tf.run(ctx, w, w, "init", "-input=false"); err != nil {
		return fail("init", err)
	}
	fmt.Fprintln(w, "\n==> terraform query (find existing resources)")
	var out bytes.Buffer // resource attributes: never in the log
	_, qerr := tf.run(ctx, &out, w, "query", "-json", "-no-color")
	byType, err := parseQuery(&out, w) // with -json, Terraform's errors are on stdout too
	if err = cmp.Or(err, qerr); err != nil {
		return fail("query", err)
	}
	cands, vals := candidates(t, byType, func() string { return "Ops-" + rand.Text() + "-1a" })
	fmt.Fprintf(w, "\n==> %d candidates\n", len(cands))
	if checkable(cands) > 0 {
		r.fit(ctx, tf, dir, job.Modules[0].Source, t, lists, cands, vals, w)
	}
	secrets := mask.New(job.Secrets)
	for i := range cands {
		for k, v := range cands[i].Inputs {
			if secrets.Hides(v) { // a workspace secret, e.g. a credential a module echoes into a tag
				delete(cands[i].Inputs, k)
			}
		}
	}
	return runnerapi.FinishRequest{Status: "succeeded", Candidates: cands}
}

// writeProviders writes the provider requirements and configurations for the list types' providers (each
// type's prefix: azurerm_… → hashicorp/azurerm).
// shortcut: hashicorp/<prefix> with an empty configuration, plus azurerm's required features block; add a
// provider setting to templates for other namespaces or settings.
func writeProviders(dir string, lists []string) error {
	if len(lists) == 0 {
		return nil
	}
	req, conf := map[string]any{}, map[string]any{}
	for _, l := range lists {
		prov, _, _ := strings.Cut(l, "_")
		req[prov] = map[string]string{"source": "hashicorp/" + prov}
		conf[prov] = map[string]any{}
		if prov == "azurerm" {
			conf[prov] = map[string]any{"features": map[string]any{}}
		}
	}
	j, err := json.Marshal(map[string]any{"terraform": map[string]any{"required_providers": req}, "provider": conf})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "opsolete_providers.tf.json"), j, 0o600)
}

// parseQuery reads terraform query -json: the resources found by type. Error diagnostics fail it; their
// summaries go to the log (they hold no attribute values).
func parseQuery(out io.Reader, w io.Writer) (map[string][]found, error) {
	byType := map[string][]found{}
	sc := bufio.NewScanner(out)
	sc.Buffer(nil, 64<<20) // one line per resource, with all its attributes
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Level   string `json:"@level"`
			Message string `json:"@message"`
			Found   *struct {
				Type   string         `json:"resource_type"`
				Name   string         `json:"display_name"`
				Object map[string]any `json:"resource_object"`
			} `json:"list_resource_found"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		switch {
		case line.Type == "list_resource_found" && line.Found != nil:
			byType[line.Found.Type] = append(byType[line.Found.Type], found{line.Found.Name, line.Found.Object})
		case line.Type == "diagnostic" && line.Level == "error":
			return nil, fmt.Errorf("%s", line.Message)
		case line.Type == "list_complete" || line.Type == "diagnostic":
			fmt.Fprintln(w, line.Message)
		}
	}
	return byType, sc.Err()
}

// candidates maps what the query found to template t: one candidate per resource of a primary entry (that
// has Require set), its inputs and related resources (IDFrom) filled in, checked like the form checks them.
// vals are the valid candidates' Terraform values (placeholders included), by index, for the fit check.
func candidates(t templates.Template, byType map[string][]found, placeholder func() string) ([]runnerapi.Candidate, []map[string]any) {
	var out []runnerapi.Candidate
	var vals []map[string]any
	seen := map[string]bool{}
	types := map[string]string{}
	for _, in := range t.Inputs {
		types[in.Name] = in.Type
	}
	for pi, p := range t.Adopt {
		if !p.Primary() {
			continue
		}
		for _, f := range byType[p.List] {
			id, _ := f.obj["id"].(string)
			if id == "" || seen[strings.ToLower(id)] || p.Require != "" && templates.Path(f.obj, p.Require) == nil {
				continue
			}
			seen[strings.ToLower(id)] = true
			raw, ids, secret := map[string]string{}, make([]string, len(t.Adopt)), map[string]bool{}
			if c := p.ShowIf; c != nil && len(c.Equals) == 1 {
				raw[c.Input] = c.Equals[0]
			}
			fill := func(a templates.Adopt, obj map[string]any) {
				for _, in := range slices.Sorted(maps.Keys(a.Inputs)) {
					v := templates.Path(obj, a.Inputs[in])
					if types[in] == "bool" { // whether it's set, e.g. a block such as boot_diagnostics
						raw[in] = strconv.FormatBool(isSet(v))
					} else if s, ok := rawValue(v); ok {
						raw[in] = s
					}
				}
			}
			ids[pi] = id
			fill(p, f.obj)
			for j, a := range t.Adopt {
				if a.IDFrom == "" || !t.Match(a.ShowIf, raw) {
					continue
				}
				rid, _ := templates.Path(f.obj, a.IDFrom).(string)
				ids[j] = rid
				for _, rf := range byType[a.List] {
					if fid, _ := rf.obj["id"].(string); rid != "" && strings.EqualFold(fid, rid) {
						fill(a, rf.obj)
					}
				}
			}
			for j, a := range t.Adopt {
				if ids[j] == "" {
					continue
				}
				for _, in := range a.Placeholder {
					if raw[in] == "" {
						raw[in], secret[in] = placeholder(), true
					}
				}
			}
			for k := range ids {
				ids[k] = sameCase(ids[k], raw)
			}
			c := runnerapi.Candidate{Name: f.name, ID: ids[pi], Adopt: ids, Inputs: map[string]string{}}
			v, errs := t.Validate(raw)
			_, idErrs := t.AdoptIDs(raw, ids)
			maps.Copy(errs, idErrs)
			if len(errs) > 0 {
				c.Fit = "invalid"
				var msgs []string
				for _, k := range slices.Sorted(maps.Keys(errs)) {
					msgs = append(msgs, k+": "+errs[k])
				}
				c.Message = strings.Join(msgs, "; ")
			}
			for _, in := range t.Inputs {
				if _, ok := raw[in.Name]; ok && !in.Sensitive && !secret[in.Name] {
					c.Inputs[in.Name] = raw[in.Name]
				}
			}
			out = append(out, c)
			vals = append(vals, v)
		}
	}
	return out, vals
}

// rawValue is an attribute value as the form types it; false for values a form field can't hold.
func rawValue(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, x != ""
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return templates.FormatValue(x), true
	}
	return "", false
}

// isSet: v is true, or a value other than false, null, "" and an empty list or object.
func isSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// sameCase gives the path segments of id that equal an input value but for case that value's case: the cloud
// may report a name in other case in different IDs (Azure: TEST1 in a VM's ID, test1 in its NIC's), and the
// imported state keeps the ID's case, so a differently cased input would plan a replacement.
func sameCase(id string, raw map[string]string) string {
	segs := strings.Split(id, "/")
	for i, s := range segs {
		for _, v := range raw {
			if s != v && strings.EqualFold(s, v) {
				segs[i] = v
			}
		}
	}
	return strings.Join(segs, "/")
}

func checkable(cands []runnerapi.Candidate) int {
	n := 0
	for _, c := range cands {
		if c.Fit == "" {
			n++
		}
	}
	return n
}

// fit plans adopting every valid candidate (the first maxCandidates), each as module c<i> of the template's
// module src with its imports, against an empty local state, and sets each one's Fit from its part of the plan.
func (r *Runner) fit(ctx context.Context, tf *tfProc, dir, src string, t templates.Template, lists []string,
	cands []runnerapi.Candidate, vals []map[string]any, w io.Writer) {
	unchecked := func(i int, msg string) { cands[i].Fit, cands[i].Message = "unchecked", msg }
	var mods []runnerapi.Module
	var imports []runnerapi.Import
	for i, c := range cands {
		if c.Fit != "" {
			continue
		}
		if len(mods) == maxCandidates {
			unchecked(i, fmt.Sprintf("not checked: a scan checks at most %d", maxCandidates))
			continue
		}
		name := "c" + strconv.Itoa(i)
		mods = append(mods, runnerapi.Module{Name: name, Source: src, Inputs: vals[i]})
		for j, id := range c.Adopt {
			if id != "" {
				imports = append(imports, runnerapi.Import{To: "module." + name + "." + t.Adopt[j].To, ID: id})
			}
		}
	}
	failAll := func(err error) {
		msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
		fmt.Fprintf(w, "\n==> fit check failed: %v\n", err)
		for i := range cands {
			if cands[i].Fit == "" {
				unchecked(i, "fit check failed: "+msg)
			}
		}
	}
	fdir := filepath.Join(dir, "fit")
	err := os.Mkdir(fdir, 0o700)
	if err == nil {
		err = writeModules(fdir, mods)
	}
	if err == nil {
		err = writeImports(fdir, imports)
	}
	if err == nil {
		err = writeProviders(fdir, lists) // for modules that don't configure their provider themselves
	}
	if err != nil {
		failAll(err)
		return
	}
	tf.dir = fdir
	fmt.Fprintf(w, "\n==> terraform init (fit check: %d candidates)\n", len(mods))
	if _, err := tf.run(ctx, w, w, "init", "-input=false"); err != nil {
		failAll(err)
		return
	}
	fmt.Fprintln(w, "\n==> terraform plan (fit check: adopting all, into a throwaway state)")
	if _, err := tf.run(ctx, w, w, "plan", "-input=false", "-lock=false", "-out=fit.tfplan", "-compact-warnings"); err != nil {
		failAll(err)
		return
	}
	var p tfjson.Plan
	if err := tf.json(ctx, &p, "show", "-json", "-no-color", "fit.tfplan"); err != nil {
		failAll(err)
		return
	}
	fitPlan(cands, p.ResourceChanges)
}

// fitPlan sets each checked candidate's Fit from the plan's changes in its module c<i>: fits (imports only),
// differs (also creates or updates), replaces (replaces or deletes).
func fitPlan(cands []runnerapi.Candidate, rcs []*tfjson.ResourceChange) {
	for i := range cands {
		if cands[i].Fit != "" {
			continue
		}
		prefix := "module.c" + strconv.Itoa(i) + "."
		var mine []*tfjson.ResourceChange
		for _, rc := range rcs {
			if strings.HasPrefix(rc.Address, prefix) {
				mine = append(mine, rc)
			}
		}
		sum := summarize(mine)
		switch {
		case sum.Destroy > 0:
			cands[i].Fit = "replaces"
		case sum.Add > 0 || sum.Change > 0:
			cands[i].Fit = "differs"
		default:
			cands[i].Fit = "fits"
		}
		for _, rc := range mine {
			a := rc.Change.Actions
			if a.NoOp() || a.Read() {
				continue
			}
			ch := runnerapi.FitChange{Address: strings.TrimPrefix(rc.Address, prefix), Action: action(a)}
			if a.Update() || a.Replace() {
				ch.Attributes = changed(rc.Change)
			}
			cands[i].Changes = append(cands[i].Changes, ch)
		}
	}
}

func action(a tfjson.Actions) string {
	switch {
	case a.Replace():
		return "replace"
	case a.Create():
		return "create"
	case a.Delete():
		return "delete"
	}
	return "update"
}

// changed lists the top-level attributes a change sets to another value (known or not): names, never values.
func changed(c *tfjson.Change) []string {
	before, _ := c.Before.(map[string]any)
	after, _ := c.After.(map[string]any)
	unknown, _ := c.AfterUnknown.(map[string]any)
	var out []string
	for _, k := range slices.Sorted(maps.Keys(after)) {
		if !reflect.DeepEqual(before[k], after[k]) || unknown[k] == true && before[k] != nil {
			out = append(out, k)
		}
	}
	return out
}
