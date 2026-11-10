package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// TestListInputForm: a list input renders its rows and a row template, the posted rows (<list>.<row>.<field>, in
// row order, empty rows dropped) reach Terraform as a typed list, an invalid row re-renders the form with what was
// typed and says which row and field, and the edit form shows the stored rows.
func TestListInputForm(t *testing.T) {
	store := testStore(t)
	ws := "list-" + strings.ToLower(newID()[:8])
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "disks.json"), []byte(`{"id": "disks", "name": "Disks", "module": {"source": "git::https://example.com/m.git", "ref": "v1"},
  "inputs": [{"name": "data_disks", "label": "Data disks", "type": "list", "default": [{"lun": 0, "size_gb": 32}], "fields": [
    {"name": "lun", "label": "LUN", "type": "number", "required": true},
    {"name": "size_gb", "label": "Size (GB)", "type": "number", "required": true},
    {"name": "type", "type": "select", "options": ["Standard_LRS", "Premium_LRS"]},
    {"name": "existing", "type": "bool"}]}]}`), 0o644)
	h := newServer(t, dir, false, store)
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {ws}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	base := "/workspaces/" + ws + "/templates/"
	page := get(h, base+"new?template=disks").Body.String()
	for _, want := range []string{`name="data_disks.0.lun" value="0"`, `name="data_disks.0.size_gb" value="32"`, // the default's row
		`name="data_disks.__i__.lun" value=""`, `data-list-add`, `<option value="Standard_LRS">Standard_LRS</option>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("add form lacks %s:\n%s", want, page)
		}
	}
	rows := url.Values{"data_disks.0.lun": {"0"}, "data_disks.0.size_gb": {"32"}, "data_disks.0.type": {"Premium_LRS"}, "data_disks.0.existing": {"on"},
		"data_disks.3.lun": {"1"}, "data_disks.3.size_gb": {"big"}, "data_disks.3.type": {"Standard_LRS"},
		"data_disks.2.lun": {""}, "data_disks.2.size_gb": {""}} // removed in the middle and left empty: dropped
	rec := post(h, base+"new?template=disks", rows)
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "Row 2, Size (GB): Enter a number") ||
		!strings.Contains(body, `name="data_disks.1.size_gb" value="big"`) {
		t.Fatalf("invalid row: %d %s", rec.Code, body)
	}
	rows.Set("data_disks.3.size_gb", "8")
	if rec := post(h, base+"new?template=disks", rows); rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
	req.Header.Set("Authorization", "Bearer "+testRunnerToken)
	var j runnerapi.Job
	if rec := do(h, req); json.Unmarshal(rec.Body.Bytes(), &j) != nil || len(j.Modules) != 1 {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	want := []any{map[string]any{"lun": 0.0, "size_gb": 32.0, "type": "Premium_LRS", "existing": true},
		map[string]any{"lun": 1.0, "size_gb": 8.0, "type": "Standard_LRS", "existing": false}}
	if got := j.Modules[0].Inputs["data_disks"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("Terraform gets %#v", got)
	}
	store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'applied', lease_id = NULL, lease_until = NULL WHERE id = $1`, j.RunID)
	ws1 := get(h, "/workspaces/"+ws+"?show=templates").Body.String()
	if !strings.Contains(ws1, "LUN: 0, Size (GB): 32, type: Premium_LRS, existing: true\nLUN: 1, Size (GB): 8") {
		t.Errorf("workspace page: the stored rows, a line each:\n%s", ws1)
	}
	if edit := get(h, base+j.Modules[0].Name).Body.String(); !strings.Contains(edit, `name="data_disks.1.size_gb" value="8"`) ||
		!strings.Contains(edit, `id="data_disks.0.existing" name="data_disks.0.existing" checked`) {
		t.Errorf("edit form: the stored rows:\n%s", edit)
	}
}
