package app

import (
	"regexp"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// moduleNameRe is a Terraform module call name; "new" is the add route.
var moduleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func TestModuleName(t *testing.T) {
	for name, want := range map[string]string{"Greeting (example)": "greeting_example", "Virtual machine – Windows": "virtual_machine_windows",
		"2 VMs": "t_2_vms", "": "t", "New": "template", strings.Repeat("a ", 40): strings.Repeat("a_", 31) + "a"} {
		if got := moduleName(name); got != want || !moduleNameRe.MatchString(got) {
			t.Errorf("moduleName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDefaultModule(t *testing.T) {
	keyed := templates.Template{Name: "Virtual machine", Inputs: []templates.Input{{Name: "vm", Type: "string", Key: true}}}
	long := strings.Repeat("a", 63)
	taken := []storage.WorkspaceTemplate{{Name: "web_prod_01"}, {Name: "virtual_machine"}, {Name: long}}
	for _, c := range []struct {
		t    templates.Template
		raw  map[string]string
		want string // a regexp
	}{
		{keyed, map[string]string{"vm": "Web-Prod 01"}, `web_prod_01_[0-9a-f]{4}`},
		{keyed, map[string]string{"vm": "db"}, `db_[0-9a-f]{4}`},             // always a suffix
		{keyed, map[string]string{"vm": " "}, `virtual_machine_[0-9a-f]{4}`}, // empty key: the template's name
		{templates.Template{Name: "Virtual machine"}, nil, `virtual_machine_[0-9a-f]{4}`},
		{keyed, map[string]string{"vm": long}, `a{58}_[0-9a-f]{4}`},
	} {
		if got := defaultModule(c.t, c.raw, taken); !regexp.MustCompile("^"+c.want+"$").MatchString(got) || !moduleNameRe.MatchString(got) {
			t.Errorf("defaultModule(%v) = %q, want %s", c.raw, got, c.want)
		}
	}
}

func TestWords(t *testing.T) {
	for in, want := range map[string]string{"networkSecurityGroups": "network security groups", "publicIPAddresses": "public IP addresses",
		"virtualMachines": "virtual machines", "disks": "disks", "schedules": "schedules", "networkWatchers": "network watchers"} {
		if got := words(in); got != want {
			t.Errorf("words(%s) = %q, want %q", in, got, want)
		}
	}
	if rgOf("/subscriptions/s/resourceGroups/rg_test/providers/x/y/z") != "rg_test" || rgOf("/x") != "" {
		t.Error("rgOf")
	}
}
