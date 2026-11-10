package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// Extras (ScanEntry.Of: a VM's data disks, its shutdown schedule, a NIC's security group) can't be listed on their
// own (azurerm has no list resource for most, and a list wouldn't say whose they are): they're read from the
// resource they belong to, with the provider only, in a throwaway plan:
//   - a VM's data disks and their attachments: the VM imported as the legacy azurerm_virtual_machine with generated
//     config, whose storage_data_disk blocks say each one's LUN, caching, size and type (that plan fails: the legacy
//     type's generated config is invalid, but it's written)
//   - a VM's shutdown schedule: Azure names it shutdown-computevm-<vm>; when the inventory has it, imported with
//     generated config
//   - a NIC's security group: the azurerm_network_interface data source; its rules from the scan's NSG list
//
// shortcut: Azure's relations, by the extra's type; other types or clouds need their own.

// related is an extra found for the resource it belongs to.
type related struct {
	key string         // the repeat key (a data disk's LUN); "" none
	obj map[string]any // its attributes, as its own type has them
}

// relations are the extras by the lower-cased ID of what they belong to, then by type.
type relations map[string]map[string][]related

const (
	diskList       = "azurerm_managed_disk"
	attachmentList = "azurerm_virtual_machine_data_disk_attachment"
	scheduleList   = "azurerm_dev_test_global_vm_shutdown_schedule"
	nsgList        = "azurerm_network_security_group"
	nsgAssocList   = "azurerm_network_interface_security_group_association"
)

// relations reads the extras the candidates' resources have (owners: ID → the extra types wanted).
func (r *Runner) relations(ctx context.Context, tf *tfProc, dir string, owners map[string][]string, byType map[string][]found,
	inventory []runnerapi.Found, w io.Writer) relations {
	var vms, nics, scheds []string // owner IDs
	var schedIDs []string
	for _, id := range slices.Sorted(maps.Keys(owners)) {
		ls := owners[id]
		if slices.Contains(ls, diskList) || slices.Contains(ls, attachmentList) {
			vms = append(vms, id)
		}
		if slices.Contains(ls, nsgList) || slices.Contains(ls, nsgAssocList) {
			nics = append(nics, id)
		}
		if s := scheduleID(id); slices.Contains(ls, scheduleList) && s != "" && slices.ContainsFunc(inventory, func(f runnerapi.Found) bool {
			return strings.EqualFold(f.ID, s)
		}) {
			scheds, schedIDs = append(scheds, id), append(schedIDs, s)
		}
	}
	if len(vms)+len(nics)+len(scheds) == 0 {
		return nil
	}
	rel := relations{}
	add := func(owner, list string, x related) {
		k := strings.ToLower(owner)
		if rel[k] == nil {
			rel[k] = map[string][]related{}
		}
		rel[k][list] = append(rel[k][list], x)
	}
	skip := func(what string, err error) {
		msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
		fmt.Fprintf(w, "==> reading %s failed, found without them: %s\n", what, msg)
	}
	rdir := filepath.Join(dir, "relations")
	if err := os.Mkdir(rdir, 0o700); err != nil {
		skip("data disks, schedules and security groups", err)
		return nil
	}
	if err := writeProviders(rdir, []string{nsgList}); err != nil {
		skip("data disks, schedules and security groups", err)
		return nil
	}
	tf.dir = rdir
	fmt.Fprintf(w, "\n==> read what the %d resources found have (data disks, schedules, security groups)\n", len(owners))
	if _, err := tf.run(ctx, w, w, "init", "-input=false"); err != nil {
		skip("data disks, schedules and security groups", err)
		return nil
	}
	if len(nics) > 0 {
		nsgs, err := r.nicGroups(ctx, tf, rdir, nics, w)
		if err != nil {
			skip("security groups", err)
		}
		for i, nic := range nics {
			nsg := nsgs["n"+strconv.Itoa(i)]
			if nsg == "" {
				continue
			}
			obj := map[string]any{"id": nsg, "name": nsg[strings.LastIndex(nsg, "/")+1:]}
			for _, f := range byType[nsgList] { // with its rules, when the scan listed it
				if id, _ := f.obj["id"].(string); strings.EqualFold(id, nsg) {
					obj = f.obj
				}
			}
			add(nic, nsgList, related{obj: obj})
			add(nic, nsgAssocList, related{obj: map[string]any{"id": nic + "|" + nsg, "network_interface_id": nic, "network_security_group_id": nsg}})
		}
	}
	if len(vms)+len(scheds) == 0 {
		return rel
	}
	var imports []runnerapi.Import
	for i, id := range vms {
		imports = append(imports, runnerapi.Import{To: "azurerm_virtual_machine.v" + strconv.Itoa(i), ID: id})
	}
	for i, id := range schedIDs {
		imports = append(imports, runnerapi.Import{To: scheduleList + ".s" + strconv.Itoa(i), ID: id})
	}
	if err := writeImports(rdir, imports); err != nil {
		skip("data disks and schedules", err)
		return rel
	}
	// fails for the legacy VM type (its generated config is invalid), after writing gen.tf: only that's read
	_, perr := tf.run(ctx, io.Discard, io.Discard, "plan", "-input=false", "-lock=false", "-generate-config-out=gen.tf", "-no-color")
	gen, err := readGenerated(filepath.Join(rdir, "gen.tf"))
	if err != nil {
		if perr != nil {
			err = perr
		}
		skip("data disks and schedules", err)
		return rel
	}
	for i, vm := range vms {
		disks, _ := gen["azurerm_virtual_machine.v"+strconv.Itoa(i)]["storage_data_disk"].([]any)
		for _, d := range disks {
			d, _ := d.(map[string]any)
			lun, _ := d["lun"].(float64)
			name, _ := d["name"].(string)
			if name == "" {
				continue
			}
			key := strconv.Itoa(int(lun))
			add(vm, diskList, related{key: key, obj: map[string]any{"id": d["managed_disk_id"], "name": name,
				"disk_size_gb": d["disk_size_gb"], "storage_account_type": d["managed_disk_type"]}})
			add(vm, attachmentList, related{key: key, obj: map[string]any{"id": vm + "/dataDisks/" + name, "lun": d["lun"],
				"caching": d["caching"], "managed_disk_id": d["managed_disk_id"], "virtual_machine_id": vm}})
		}
	}
	for i, owner := range scheds {
		if obj := gen[scheduleList+".s"+strconv.Itoa(i)]; obj != nil {
			obj["id"] = schedIDs[i]
			add(owner, scheduleList, related{obj: obj})
		}
	}
	return rel
}

// nicGroups reads the security group ID of each NIC (data source n<i> for nics[i]) in a plan: by n<i>, "" none.
func (r *Runner) nicGroups(ctx context.Context, tf *tfProc, rdir string, nics []string, w io.Writer) (map[string]string, error) {
	data, outs := map[string]any{}, []string{}
	for i, id := range nics {
		segs := strings.Split(id, "/") // /subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/name
		if len(segs) != 9 {
			continue
		}
		n := "n" + strconv.Itoa(i)
		data[n] = map[string]any{"name": templateEscape.Replace(segs[8]), "resource_group_name": templateEscape.Replace(segs[4])}
		outs = append(outs, n+" = data.azurerm_network_interface."+n+".network_security_group_id")
	}
	conf := map[string]any{"data": map[string]any{"azurerm_network_interface": data},
		"output": map[string]any{"nsg": map[string]any{"value": "${{" + strings.Join(outs, ", ") + "}}"}}}
	b, err := json.Marshal(conf)
	if err == nil {
		err = os.WriteFile(filepath.Join(rdir, "nics.tf.json"), b, 0o600)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tf.run(ctx, io.Discard, w, "plan", "-input=false", "-lock=false", "-out=nics.tfplan"); err != nil {
		return nil, err
	}
	var p struct {
		PlannedValues struct {
			Outputs map[string]struct {
				Value map[string]string `json:"value"`
			} `json:"outputs"`
		} `json:"planned_values"`
	}
	err = tf.json(ctx, &p, "show", "-json", "-no-color", "nics.tfplan")
	return p.PlannedValues.Outputs["nsg"].Value, err
}

// scheduleID is the ID of a VM's auto-shutdown schedule, which Azure names after it; "" if id isn't a VM's.
func scheduleID(id string) string {
	segs := strings.Split(id, "/")
	if len(segs) != 9 || !strings.EqualFold(segs[7], "virtualMachines") {
		return ""
	}
	return strings.Join(segs[:5], "/") + "/providers/Microsoft.DevTestLab/schedules/shutdown-computevm-" + segs[8]
}

// readGenerated reads Terraform's generated config: each resource's literal attributes (nested blocks as lists),
// by address.
func readGenerated(file string) (map[string]map[string]any, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	f, diags := hclsyntax.ParseConfig(src, filepath.Base(file), hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	out := map[string]map[string]any{}
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if b.Type == "resource" && len(b.Labels) == 2 {
			out[b.Labels[0]+"."+b.Labels[1]] = hclObject(b.Body)
		}
	}
	return out, nil
}

// hclObject is a body's literal attributes as JSON values (others left out), its blocks as lists of objects.
func hclObject(b *hclsyntax.Body) map[string]any {
	out := map[string]any{}
	for k, a := range b.Attributes {
		v, diags := a.Expr.Value(nil)
		if diags.HasErrors() || !v.IsWhollyKnown() {
			continue
		}
		var x any
		if j, err := ctyjson.Marshal(v, v.Type()); err == nil && json.Unmarshal(j, &x) == nil {
			out[k] = x
		}
	}
	for _, blk := range b.Blocks {
		l, _ := out[blk.Type].([]any)
		out[blk.Type] = append(l, hclObject(blk.Body))
	}
	return out
}
