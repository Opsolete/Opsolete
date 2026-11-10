package app

import "testing"

// TestKind: a resource's kind, for the scan results, from an address in the module, nested and keyed too.
func TestKind(t *testing.T) {
	for addr, want := range map[string]string{"azurerm_windows_virtual_machine.self[0]": "windows virtual machine",
		`module.data_disk["1.0"].azurerm_managed_disk.this`: "managed disk", "module.nic.azurerm_network_interface.this": "network interface"} {
		if got := kind(addr); got != want {
			t.Errorf("%s: %q", addr, got)
		}
	}
}
