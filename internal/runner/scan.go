package runner

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// A scan takes stock of the resources in the subscription (or one resource group), finds those the catalog's
// templates can take over (the entries the server read from their modules' code), maps their attributes to each
// template's inputs, and checks the fit with one speculative plan that imports them all, each as its own module,
// into a throwaway local state. It writes nothing: no backend, no lock, no apply.

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
	if job.Scan == nil {
		return fail("read scan", errors.New("a scan job without what to scan"))
	}
	sources := map[string]string{} // module name → source
	for _, m := range job.Modules {
		sources[m.Name] = m.Source
	}
	for _, n := range job.Scan.Notes {
		fmt.Fprintf(w, "==> %s\n", n)
	}
	tpls := make([]templates.Template, len(job.Scan.Templates))
	var lists []string
	for i, st := range job.Scan.Templates {
		if err := json.Unmarshal(st.Template, &tpls[i]); err != nil || sources[st.Module] == "" {
			return fail("read scan", fmt.Errorf("invalid template in the scan job: %v", err))
		}
		for _, n := range st.Notes {
			fmt.Fprintf(w, "==> %s: %s\n", tpls[i].Name, n)
		}
		for _, e := range st.Entries {
			if !slices.Contains(lists, e.List) && listRe.MatchString(e.List) {
				lists = append(lists, e.List)
			}
		}
	}
	rg := job.Scan.ResourceGroup
	if rg != "" && !rgRe.MatchString(rg) {
		return fail("read scan", fmt.Errorf("invalid resource group name %q", rg))
	}
	qdir := filepath.Join(dir, "query")
	if err := os.Mkdir(qdir, 0o700); err != nil {
		return fail("prepare scan", err)
	}
	// shortcut: the inventory is Azure's (azurerm_resources, per resource group); other clouds need their own.
	queried := slices.Clone(lists)
	if rg == "" && !slices.Contains(queried, rgList) { // the resource groups to take stock of
		queried = append(queried, rgList)
	}
	if err := writeProviders(qdir, append(slices.Clone(lists), rgList)); err != nil {
		return fail("prepare scan", err)
	}
	tf.dir = qdir
	fmt.Fprintf(w, "==> terraform init (providers for %s)\n", strings.Join(append(slices.Clone(lists), rgList), ", "))
	if _, err := tf.run(ctx, w, w, "init", "-input=false"); err != nil {
		return fail("init", err)
	}
	byType := map[string][]found{}
	for len(queried) > 0 {
		if err := os.WriteFile(filepath.Join(qdir, "scan.tfquery.hcl"), []byte(queryFile(queried, rg)), 0o600); err != nil {
			return fail("prepare scan", err)
		}
		fmt.Fprintf(w, "\n==> terraform query (find %s)\n", strings.Join(queried, ", "))
		var out bytes.Buffer // resource attributes: never in the log
		_, qerr := tf.run(ctx, &out, w, "query", "-json", "-no-color")
		var unsupported []string
		var err error
		byType, unsupported, err = parseQuery(&out, w) // with -json, Terraform's errors are on stdout too
		if left := slices.DeleteFunc(slices.Clone(queried), func(l string) bool { return slices.Contains(unsupported, l) }); len(unsupported) > 0 && len(left) > 0 {
			// e.g. managed disks: such an entry is still imported by ID (id_from), its inputs just aren't filled
			fmt.Fprintf(w, "==> no list support for %s: found without it\n", strings.Join(unsupported, ", "))
			queried = left
			continue
		}
		if len(unsupported) > 0 && err == nil {
			err = fmt.Errorf("the provider can list none of %s", strings.Join(unsupported, ", "))
		}
		if err = cmp.Or(err, qerr); err != nil {
			return fail("query", err)
		}
		break
	}
	rgs := []string{rg}
	if rg == "" {
		rgs = nil
		for _, g := range byType[rgList] {
			if name, _ := g.obj["name"].(string); name != "" {
				rgs = append(rgs, name)
			}
		}
	}
	inventory := r.inventory(ctx, tf, dir, rgs, w)
	var cands []runnerapi.Candidate
	var vals []map[string]any
	srcs := map[string]string{} // template id → its module's source
	for i, st := range job.Scan.Templates {
		c, v := candidates(tpls[i], st.Entries, byType, func() string { return "Ops-" + rand.Text() + "-1a" })
		cands, vals = append(cands, c...), append(vals, v...)
		srcs[tpls[i].ID] = sources[st.Module]
	}
	if slices.ContainsFunc(job.Scan.Templates, func(st runnerapi.ScanTemplate) bool { return len(st.Probes) > 0 }) {
		r.probe(ctx, tf, dir, tpls, job.Scan.Templates, cands, vals, inventory, lists, w)
	}
	uses(cands, inventory)
	fmt.Fprintf(w, "\n==> %d resources, %d a template can import\n", len(inventory), len(cands))
	if checkable(cands) > 0 {
		r.fit(ctx, tf, dir, srcs, lists, cands, vals, w)
	}
	secrets := mask.New(job.Secrets)
	for i := range cands {
		for k, v := range cands[i].Inputs {
			if secrets.Hides(v) { // a workspace secret, e.g. a credential a module echoes into a tag
				delete(cands[i].Inputs, k)
			}
		}
	}
	return runnerapi.FinishRequest{Status: "succeeded", Candidates: cands, Inventory: inventory}
}

// rgList is the list resource the resource groups to take stock of come from.
const rgList = "azurerm_resource_group"

// inventory lists every resource in the resource groups rgs (azurerm_resources, read in a plan against a throwaway
// state): IDs, names and types only. nil if it can't: the candidates are still worth listing, so the log says why.
func (r *Runner) inventory(ctx context.Context, tf *tfProc, dir string, rgs []string, w io.Writer) []runnerapi.Found {
	if len(rgs) == 0 {
		return []runnerapi.Found{}
	}
	fail := func(err error) []runnerapi.Found {
		msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
		fmt.Fprintf(w, "\n==> taking stock failed, listing only what templates can import: %s\n", msg)
		return nil
	}
	idir := filepath.Join(dir, "inventory")
	if err := os.Mkdir(idir, 0o700); err != nil {
		return fail(err)
	}
	// the names as a variable: data, never HCL
	conf := map[string]any{
		"variable": map[string]any{"rgs": map[string]any{"type": "list(string)"}},
		"data": map[string]any{"azurerm_resources": map[string]any{"rg": map[string]any{
			"for_each": "${toset(var.rgs)}", "resource_group_name": "${each.key}"}}},
		"output": map[string]any{"inventory": map[string]any{"value": "${flatten([for g, d in data.azurerm_resources.rg : " +
			"[for r in d.resources : {id = r.id, name = r.name, type = r.type, location = r.location, resource_group = g}]])}"}},
	}
	for name, v := range map[string]any{"opsolete_inventory.tf.json": conf, "terraform.tfvars.json": map[string]any{"rgs": rgs}} {
		b, err := json.Marshal(v)
		if err == nil {
			err = os.WriteFile(filepath.Join(idir, name), b, 0o600)
		}
		if err != nil {
			return fail(err)
		}
	}
	if err := writeProviders(idir, []string{rgList}); err != nil {
		return fail(err)
	}
	tf.dir = idir
	fmt.Fprintf(w, "\n==> take stock of %d resource groups (terraform plan, data sources only)\n", len(rgs))
	if _, err := tf.run(ctx, w, w, "init", "-input=false"); err != nil {
		return fail(err)
	}
	if _, err := tf.run(ctx, io.Discard, w, "plan", "-input=false", "-lock=false", "-out=inventory.tfplan"); err != nil {
		return fail(err)
	}
	var p struct {
		PlannedValues struct {
			Outputs map[string]struct {
				Value []runnerapi.Found `json:"value"`
			} `json:"outputs"`
		} `json:"planned_values"`
	}
	if err := tf.json(ctx, &p, "show", "-json", "-no-color", "inventory.tfplan"); err != nil {
		return fail(err)
	}
	inv := p.PlannedValues.Outputs["inventory"].Value
	if inv == nil {
		inv = []runnerapi.Found{}
	}
	return inv
}

// uses sets each candidate's Uses: the inventory's resources its resources refer to (collected in Uses by
// candidates) that it doesn't import, such as its OS disk or a public IP.
func uses(cands []runnerapi.Candidate, inventory []runnerapi.Found) {
	known := map[string]string{}
	for _, f := range inventory {
		known[strings.ToLower(f.ID)] = f.ID
	}
	for i := range cands {
		imported := map[string]bool{}
		for _, im := range append(slices.Clone(cands[i].Imports), cands[i].Found...) {
			imported[strings.ToLower(im.ID)] = true
		}
		var out []string
		for _, id := range cands[i].Uses {
			if k := strings.ToLower(id); known[k] != "" && !imported[k] && !slices.Contains(out, known[k]) {
				out = append(out, known[k])
			}
		}
		slices.Sort(out)
		cands[i].Uses = out
	}
}

// refs collects the IDs (strings that start with /subscriptions/) anywhere in a resource's attributes v.
func refs(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(strings.ToLower(x), "/subscriptions/") {
			*out = append(*out, x)
		}
	case []any:
		for _, e := range x {
			refs(e, out)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			refs(x[k], out)
		}
	}
}

// queryFile is the .tfquery.hcl that lists the types (listRe-checked), in resource group rg (rgRe-checked; ""
// all): both safe in HCL as they are.
func queryFile(lists []string, rg string) string {
	var q strings.Builder
	for i, l := range lists {
		prov, _, _ := strings.Cut(l, "_")
		fmt.Fprintf(&q, "list %q \"l%d\" {\n  provider         = %s\n  include_resource = true\n  limit            = 1000\n", l, i, prov)
		// shortcut: azurerm's list resources take resource_group_name, except the resource group list; add a
		// per-provider filter setting for other providers.
		if rg != "" && prov == "azurerm" && l != "azurerm_resource_group" {
			fmt.Fprintf(&q, "  config {\n    resource_group_name = %q\n  }\n", rg)
		}
		q.WriteString("}\n")
	}
	return q.String()
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

// parseQuery reads terraform query -json: the resources found by type, and the list types the provider doesn't
// support. Other error diagnostics fail it; their summaries go to the log (they hold no attribute values).
func parseQuery(out io.Reader, w io.Writer) (map[string][]found, []string, error) {
	byType := map[string][]found{}
	var unsupported []string
	sc := bufio.NewScanner(out)
	sc.Buffer(nil, 64<<20) // one line per resource, with all its attributes
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Level   string `json:"@level"`
			Message string `json:"@message"`
			Diag    struct {
				Detail string `json:"detail"`
			} `json:"diagnostic"`
			Found *struct {
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
			if m := unsupportedList.FindStringSubmatch(line.Diag.Detail); m != nil {
				unsupported = append(unsupported, m[1])
				continue
			}
			return nil, nil, fmt.Errorf("%s", line.Message)
		case line.Type == "list_complete" || line.Type == "diagnostic":
			fmt.Fprintln(w, line.Message)
		}
	}
	return byType, unsupported, sc.Err()
}

// unsupportedList matches Terraform's "The provider hashicorp/azurerm does not support list resource "x"."
var unsupportedList = regexp.MustCompile(`does not support list resource "([a-z0-9_]+)"`)

// candidates maps what the query found to template t's entries: one candidate per resource of a primary entry
// (that has Require set), its inputs and related resources (IDFrom) filled in, checked like the form checks them.
// vals are the candidates' Terraform values (placeholders included), by index, for the fit check.
func candidates(t templates.Template, entries []runnerapi.ScanEntry, byType map[string][]found, placeholder func() string) ([]runnerapi.Candidate, []map[string]any) {
	var out []runnerapi.Candidate
	var vals []map[string]any
	seen := map[string]bool{}
	types := map[string]string{}
	for _, in := range t.Inputs {
		types[in.Name] = in.Type
	}
	for pi, p := range entries {
		if !p.Primary() {
			continue
		}
		for _, f := range byType[p.List] {
			id, _ := f.obj["id"].(string)
			if id == "" || seen[strings.ToLower(id)] || p.Require != "" && path(f.obj, p.Require) == nil {
				continue
			}
			seen[strings.ToLower(id)] = true
			raw, ids, secret := map[string]string{}, make([]string, len(entries)), map[string]bool{}
			if c := p.ShowIf; c != nil && len(c.Equals) == 1 {
				raw[c.Input] = c.Equals[0]
			}
			var used []string
			fill := func(a runnerapi.ScanEntry, obj map[string]any) {
				refs(obj, &used)
				for _, in := range slices.Sorted(maps.Keys(a.Inputs)) {
					v := path(obj, a.Inputs[in])
					if types[in] == "bool" { // whether it's set, e.g. a block such as boot_diagnostics
						raw[in] = strconv.FormatBool(isSet(v))
					} else if s, ok := rawValue(v); ok {
						raw[in] = s
					}
				}
			}
			ids[pi] = id
			fill(p, f.obj)
			for j, a := range entries {
				if a.IDFrom == "" || !t.Match(a.ShowIf, raw) {
					continue
				}
				rid, _ := path(f.obj, a.IDFrom).(string)
				ids[j] = rid
				for _, rf := range byType[a.List] {
					if fid, _ := rf.obj["id"].(string); rid != "" && strings.EqualFold(fid, rid) {
						fill(a, rf.obj)
					}
				}
			}
			c := runnerapi.Candidate{Template: t.ID, Name: f.name, Inputs: map[string]string{}, Uses: used}
			for j, a := range entries {
				if ids[j] == "" {
					continue
				}
				for _, in := range a.Placeholder {
					if raw[in] == "" {
						raw[in], secret[in] = placeholder(), true
						c.Placeholder = append(c.Placeholder, in)
					}
				}
				c.Imports = append(c.Imports, runnerapi.Import{To: a.To, ID: sameCase(ids[j], raw)})
			}
			c.ID = sameCase(id, raw)
			v, errs := t.Validate(raw)
			if len(errs) > 0 {
				c.Fit = "invalid"
				var msgs []string
				for _, in := range t.Inputs { // in the form's order and words
					if e, ok := errs[in.Name]; ok {
						msgs = append(msgs, in.Title()+": "+e)
					}
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

// path reads the attribute at p (an attribute path such as os_disk[0].storage_account_type) from v, a
// resource's JSON attributes; nil when absent.
func path(v any, p string) any {
	for _, step := range pathSteps.FindAllString(p, -1) {
		switch x := v.(type) {
		case map[string]any:
			v = x[strings.TrimPrefix(step, ".")]
		case []any:
			i, err := strconv.Atoi(strings.Trim(step, "[]"))
			if err != nil || i >= len(x) || !strings.HasPrefix(step, "[") {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

var (
	pathSteps = regexp.MustCompile(`^[^.\[]+|\.[^.\[]+|\[\d+\]`)
	listRe    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	rgRe      = regexp.MustCompile(`^[-\w.()]{1,90}$`) // Azure's resource group names; also what's safe in HCL
)

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

// fit plans adopting every valid candidate (the first maxCandidates), each as module c<i> of its template's
// module (srcs by template id) with its imports, against an empty local state, and sets each one's Fit from its part of the plan.
func (r *Runner) fit(ctx context.Context, tf *tfProc, dir string, srcs map[string]string, lists []string,
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
		mods = append(mods, runnerapi.Module{Name: name, Source: srcs[c.Template], Inputs: vals[i]})
		for _, im := range c.Imports {
			imports = append(imports, runnerapi.Import{To: "module." + name + "." + im.To, ID: im.ID})
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
