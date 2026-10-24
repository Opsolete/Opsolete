package httpserver

import (
	"testing"

	"github.com/opsolete/opsolete/internal/storage"
)

func TestLatestVersions(t *testing.T) {
	m := func(name, ver string) storage.Module {
		return storage.Module{Namespace: "p", Name: name, System: "azurerm", Version: ver}
	}
	// Ordered by module, newest upload first: the 1.0.1 backport came after 2.0.0.
	got := latestVersions([]storage.Module{m("a", "1.0.1"), m("a", "2.0.0"), m("a", "1.0.0"), m("b", "0.1.0")})
	if len(got) != 2 || got[0].Version != "2.0.0" || got[0].Older != 2 || got[1].Version != "0.1.0" || got[1].Older != 0 {
		t.Fatalf("%+v", got)
	}
}
