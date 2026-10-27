package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestOrganizationCurrency(t *testing.T) {
	store := testStore(t)
	h := newServer(t, "../../catalog", false, store)
	t.Cleanup(func() { store.SetCurrency(context.Background(), "USD") })
	if rec := post(h, "/organization", url.Values{"currency": {"GBP"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad currency: %d", rec.Code)
	}
	if rec := post(h, "/organization", url.Values{"currency": {"EUR"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if body := get(h, "/organization").Body.String(); !strings.Contains(body, `value="EUR" selected`) {
		t.Fatalf("page: %s", body)
	}
	if rec := post(newServerAs(t, "../../catalog", false, store, devAuth(t, store, "dev@example.com", false)), "/organization",
		url.Values{"currency": {"USD"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d", rec.Code)
	}
}

func TestSettingsNav(t *testing.T) {
	store := testStore(t)
	h := newServer(t, "../../catalog", false, store)
	if body := get(h, "/workspaces").Body.String(); !strings.Contains(body, `href="/organization">Settings</a>`) || strings.Contains(body, ">Audit log<") {
		t.Fatalf("main nav: %s", body)
	}
	if body := get(h, "/audit").Body.String(); !strings.Contains(body, `aria-label="Settings"`) || !strings.Contains(body, `href="/audit" aria-current="page">Audit log`) {
		t.Fatalf("settings nav: %s", body)
	}
}
