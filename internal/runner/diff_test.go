package runner

import (
	"encoding/json"
	"reflect"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

const diffPlan = `{"format_version": "1.2", "resource_changes": [
 {"address": "azurerm_linux_virtual_machine.vm", "mode": "managed", "type": "azurerm_linux_virtual_machine", "name": "vm",
  "action_reason": "replace_because_cannot_update",
  "change": {"actions": ["delete", "create"],
   "before": {"name": "vm1", "size": "B2s", "location": "westeurope", "admin_password": "old-pw", "tags": {"env": "dev", "team": "a"},
     "os_disk": [{"caching": "ReadWrite", "size": 30}], "custom_data": "token hunter2 inside", "id": "/vm1"},
   "after": {"name": "vm1", "size": "B4ms", "location": "northeurope", "admin_password": "new-pw", "tags": {"env": "prd", "team": "a"},
     "os_disk": [{"caching": "ReadWrite", "size": 64}], "custom_data": "token hunter2 inside", "id": null},
   "after_unknown": {"id": true, "os_disk": [{}]},
   "before_sensitive": {"admin_password": true}, "after_sensitive": {"admin_password": true},
   "replace_paths": [["location"]]}},
 {"address": "random_id.x", "mode": "managed", "type": "random_id", "name": "x",
  "change": {"actions": ["create"], "before": null, "after": {"byte_length": 4}, "after_unknown": {"hex": true}}},
 {"address": "null_resource.same", "mode": "managed", "type": "null_resource", "name": "same",
  "change": {"actions": ["no-op"], "before": {"id": "1"}, "after": {"id": "1"}}}
]}`

func TestPlanDiff(t *testing.T) {
	var p tfjson.Plan
	if err := json.Unmarshal([]byte(diffPlan), &p); err != nil {
		t.Fatal(err)
	}
	got := planDiff(p.ResourceChanges, mask.New([]string{"hunter2"}))
	want := []runnerapi.ResourceDiff{
		{Address: "azurerm_linux_virtual_machine.vm", Action: "replace", Reason: "replace_because_cannot_update", Attrs: []runnerapi.Attr{
			{Path: "admin_password", Sensitive: true, HasBefore: true, HasAfter: true},
			{Path: "id", Before: `"/vm1"`, HasBefore: true, Unknown: true},
			{Path: "location", Before: `"westeurope"`, After: `"northeurope"`, HasBefore: true, HasAfter: true, Forces: true},
			{Path: "os_disk[0].size", Before: "30", After: "64", HasBefore: true, HasAfter: true},
			{Path: "size", Before: `"B2s"`, After: `"B4ms"`, HasBefore: true, HasAfter: true},
			{Path: "tags.env", Before: `"dev"`, After: `"prd"`, HasBefore: true, HasAfter: true},
			// custom_data: unchanged, so not listed at all (also not its secret)
		}},
		{Address: "random_id.x", Action: "create", Attrs: []runnerapi.Attr{
			{Path: "byte_length", After: "4", HasAfter: true},
			{Path: "hex", Unknown: true},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("diff:\n%s", g)
	}

	// a changed value holding a secret is hidden; more than MaxDiffAttrs changes are cut
	before, after := map[string]any{}, map[string]any{}
	for i := range runnerapi.MaxDiffAttrs + 5 {
		before[string(rune('a'+i%26))+string(rune('a'+i/26))] = i
	}
	rc := &tfjson.ResourceChange{Address: "x.y", Change: &tfjson.Change{Actions: tfjson.Actions{tfjson.ActionUpdate}, Before: before, After: after}}
	d := planDiff([]*tfjson.ResourceChange{rc}, mask.New([]string{"hunter2"}))[0]
	if !d.Truncated || len(d.Attrs) != runnerapi.MaxDiffAttrs {
		t.Fatalf("truncation: %v %d", d.Truncated, len(d.Attrs))
	}
	rc = &tfjson.ResourceChange{Address: "x.z", Change: &tfjson.Change{Actions: tfjson.Actions{tfjson.ActionUpdate},
		Before: map[string]any{"cmd": "echo hunter2"}, After: map[string]any{"cmd": "echo hunter2 again"}}}
	if a := planDiff([]*tfjson.ResourceChange{rc}, mask.New([]string{"hunter2"}))[0].Attrs; len(a) != 1 || !a[0].Sensitive || a[0].Before != "" || a[0].After != "" {
		t.Fatalf("a secret in a value: %+v", a)
	}
}
