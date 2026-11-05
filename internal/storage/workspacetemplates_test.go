package storage

import (
	"slices"
	"testing"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

func TestRootAddresses(t *testing.T) {
	res := []runnerapi.Resource{
		{Address: "azurerm_resource_group.rg"},
		{Address: "azurerm_subnet.s[0]"},
		{Address: "module.net.azurerm_vnet.v", Module: "module.net"},                  // the root's own module call: targeted whole
		{Address: `module.net["b"].module.x.a.b`, Module: `module.net["b"].module.x`}, // same call, once
		{Address: "module.web_c8ad.azurerm_vm.vm", Module: "module.web_c8ad"},         // an added template: kept
		{Address: `module.web_c8ad.module.disk["a"].d.e`, Module: `module.web_c8ad.module.disk["a"]`},
	}
	got := rootAddresses(res, []string{"web_c8ad"})
	if want := []string{"azurerm_resource_group.rg", "azurerm_subnet.s[0]", "module.net"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := rootAddresses(res[4:], []string{"web_c8ad"}); got != nil {
		t.Fatalf("only added resources: %q", got)
	}
}
