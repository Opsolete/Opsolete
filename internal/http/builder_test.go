package httpserver

import (
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/templates"
)

func TestEditorIsAdminOnly(t *testing.T) {
	store := testStore(t)
	h := newServerAs(t, t.TempDir(), true, store, devAuth(t, store, "someone@example.com", false))
	if rec := get(h, "/templates/new"); rec.Code != http.StatusForbidden {
		t.Fatalf("builder for non-admin: %d", rec.Code)
	}
	if rec := upload(h, map[string]string{"namespace": "x"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("upload for non-admin: %d", rec.Code)
	}
	if page := get(h, "/").Body.String(); strings.Contains(page, "New template") || !strings.Contains(page, "someone@example.com") {
		t.Fatalf("index for non-admin: %s", page)
	}
}

func TestTemplateBuilder(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	if rec := get(newServer(t, dir, false, store), "/templates/new"); rec.Code != http.StatusNotFound {
		t.Fatalf("builder must be off by default, got %d", rec.Code)
	}

	h := newServer(t, dir, true, store)
	form := url.Values{
		"name": {"Virtual network"}, "module_source": {"git::x"}, "module_ref": {"v1"},
		// five filled cards and the blank one, like the page submits; checkboxes post their card's index
		"in_name":         {"cidr", "region", "zone", "disks", "backup", ""},
		"in_label":        {"Address space", "", "", "", "", ""},
		"in_help":         {"A private range: 10.0.0.0/16", "", "", "", "", ""},
		"in_type":         {"string", "select", "string", "number", "bool", "string"},
		"in_required":     {"0"},
		"in_pattern":      {"", "", "", ""},
		"in_opt_value_0":  {"stale"},
		"in_opt_value_1":  {"westeurope", "northeurope", ""}, // the blank row posts too
		"in_opt_label_1":  {"", "North, Europe", ""},         // a comma in a label stays one option
		"in_default":      {"10.0.0.0/16", "westeurope", "", "7", "", ""},
		"in_bool_default": {"", "", "", "", "true", ""}, // a Yes / no card's default comes from its select
		"in_show_input":   {"", "", "region", ""},
		"in_show_equals":  {"", "", "northeurope", ""},
		"key":             {"zone"},
		"ad_to":           {"azurerm_virtual_network.this", ""}, // an adopt row and the blank one
		"ad_label":        {"VNet resource ID", ""},
		"ad_show_input":   {"region", ""},
		"ad_show_equals":  {"northeurope", ""},
	}
	// a preview renders the deploy form from the unsaved template, and saves nothing
	preview := url.Values{}
	maps.Copy(preview, form)
	preview.Set("preview", "1")
	if rec := post(h, "/templates/new", preview); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Preview:") ||
		!strings.Contains(rec.Body.String(), `<button type="submit" disabled>Create workspace`) || !strings.Contains(rec.Body.String(), `data-show-if=`) {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(files) != 0 {
		t.Fatalf("a preview must not save: %v", files)
	}
	rec := post(h, "/templates/new", form)
	loc := rec.Header().Get("Location")
	id := strings.TrimPrefix(loc, "/templates/")
	if rec.Code != http.StatusSeeOther || len(id) != 36 {
		t.Fatalf("create: %d %q %s", rec.Code, loc, rec.Body.String())
	}
	if rec := get(h, loc); !strings.Contains(rec.Body.String(), `<option value="westeurope" selected>westeurope</option>`) ||
		!strings.Contains(rec.Body.String(), `value="10.0.0.0/16"`) || !strings.Contains(rec.Body.String(), `data-show-if=`) {
		t.Fatalf("generated form wrong: %s", rec.Body.String())
	}
	saved, _ := os.ReadFile(filepath.Join(dir, id+".json"))
	if s := string(saved); !strings.Contains(s, `"show_if": {`) || strings.Contains(s, "stale") || !strings.Contains(s, `"required": true`) ||
		!strings.Contains(s, `"label": "North, Europe"`) || strings.Count(s, `"key": true`) != 1 ||
		!strings.Contains(s, `"help": "A private range: 10.0.0.0/16"`) || !strings.Contains(s, `"default": 7`) || !strings.Contains(s, `"default": true`) ||
		!strings.Contains(s, `"to": "azurerm_virtual_network.this"`) {
		t.Fatalf("saved template wrong:\n%s", s)
	}
	// the deploy form shows the help, described by it
	if body := get(h, loc).Body.String(); !strings.Contains(body, `<p class="hint" id="cidr-help">A private range: 10.0.0.0/16</p>`) ||
		!strings.Contains(body, `aria-describedby="cidr-help"`) {
		t.Fatalf("deploy form lacks the help: %s", body)
	}
	// the builder shows the condition, the option rows, the help and the typed defaults again
	if body := get(h, loc+"/edit").Body.String(); !strings.Contains(body, `<option value="region" selected>when region is</option>`) ||
		!strings.Contains(body, `name="in_help" value="A private range: 10.0.0.0/16"`) ||
		!strings.Contains(body, `value="7" type="number"`) || !strings.Contains(body, `<option value="true" selected>yes (true)</option>`) ||
		!strings.Contains(body, `name="in_opt_label_1" value="North, Europe"`) || !strings.Contains(body, `<option value="zone" selected>zone</option>`) ||
		!strings.Contains(body, `name="ad_to" value="azurerm_virtual_network.this"`) || !strings.Contains(body, `name="ad_show_equals" class="mono" value="northeurope"`) ||
		strings.Count(body, `name="ad_to"`) != 2 { // the row and a blank one
		t.Fatalf("builder lost the condition or options: %s", body)
	}
	// a condition on a dropdown below the field is refused with a readable error
	bad := url.Values{}
	maps.Copy(bad, form)
	bad["in_show_input"] = []string{"region", "", "", ""}
	if rec := post(h, "/templates/new", bad); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not a dropdown or yes / no field above this field") {
		t.Fatalf("bad condition: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/templates/new", form); rec.Header().Get("Location") == loc {
		t.Fatal("each new template must get its own id")
	}
	// saving the new-template page again (its pull request opened) keeps the id it was given
	again := url.Values{}
	maps.Copy(again, form)
	again.Set("id", id)
	if rec := post(h, "/templates/new", again); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("resave with the page's id: %d %s", rec.Code, rec.Body.String())
	}

	form.Set("name", "VNet")
	if rec := post(h, loc+"/edit", form); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != loc {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	// A fresh server reads the file back from disk, proving it was persisted.
	if rec := get(newServer(t, dir, false, store), "/"); !strings.Contains(rec.Body.String(), "VNet") {
		t.Fatalf("not persisted: %s", rec.Body.String())
	}
	if rec := post(h, loc+"/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(newServer(t, dir, false, store), loc); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted template still served: %d", rec.Code)
	}
}

// The builder's form becomes a 1.0 file that loads back as the same template: typed values come from Encode (Parse rejects untyped ones).
func TestBuilderRoundTrip(t *testing.T) {
	tpl := templateFromForm(url.Values{
		"name": {"VM"}, "module_source": {"git::https://x//vm"}, "module_ref": {"v1"}, "key": {"vm_name"},
		"in_name":            {"vm_name", "disks", "backup", "size", "note"},
		"in_label":           {"VM name", "Disks", "Backups", "Size", ""},
		"in_help":            {"1-64 letters or digits", "", "", "", "Free text"},
		"in_type":            {"string", "number", "bool", "select", "string"},
		"in_default":         {"vm1", "2", "", "s", ""},
		"in_bool_default":    {"", "", "true", "", ""},
		"in_required":        {"0"},
		"in_per_environment": {"3"},
		"in_opt_value_3":     {"s", "l"},
		"in_opt_label_3":     {"Small", "l"},
		"in_show_input":      {"", "", "", "", "backup"},
		"in_show_equals":     {"", "", "", "", "false"},
		// adopt rows, the blank one last, like the page submits
		"ad_to":          {"azurerm_windows_virtual_machine.self[0]", " azurerm_network_interface.self ", ""},
		"ad_label":       {"VM resource ID", "", ""},
		"ad_help":        {"From the portal", "", ""},
		"ad_pattern":     {`(?i)/subscriptions/.+/virtualMachines/[^/]+`, "", ""},
		"ad_show_input":  {"backup", "", ""},
		"ad_show_equals": {"true", "", ""},
	})
	want := []templates.Adopt{{To: "azurerm_windows_virtual_machine.self[0]", Label: "VM resource ID", Help: "From the portal",
		Pattern: `(?i)/subscriptions/.+/virtualMachines/[^/]+`, ShowIf: &templates.ShowIf{Input: "backup", Equals: []string{"true"}}},
		{To: "azurerm_network_interface.self"}}
	if !reflect.DeepEqual(tpl.Adopt, want) {
		t.Errorf("adopt rows: %+v", tpl.Adopt)
	}
	tpl.ID = "vm"
	b, err := templates.Encode(tpl)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"default": 2`, `"default": true`, `"help": "Free text"`, `"per_environment": true`, `"to": "azurerm_network_interface.self"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("encoded lacks %s:\n%s", want, b)
		}
	}
	ts, _, err := templates.Parse(map[string][]byte{"vm.json": b})
	if err != nil || !reflect.DeepEqual(ts["vm"], tpl) {
		t.Errorf("round trip: %v\n%+v\n%+v", err, ts["vm"], tpl)
	}
}
