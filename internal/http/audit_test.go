package httpserver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAuditLogRecordsChanges(t *testing.T) {
	store := testStore(t)
	h := newServer(t, "../../catalog", false, store)
	name := "audit-" + strings.ToLower(newID()[:8])
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE name = $1`, name) })
	if rec := post(h, "/workspaces/new", url.Values{"name": {name}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/workspaces/"+name+"/variables", url.Values{"category": {"env"}, "key": {"ARM_CLIENT_SECRET"},
		"value": {"hunter2"}, "sensitive": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set variable: %d %s", rec.Code, rec.Body.String())
	}
	last := func(action string) (actor, detail string) {
		t.Helper()
		if err := store.DB.QueryRow(`SELECT actor, detail::text FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`,
			action).Scan(&actor, &detail); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return actor, detail
	}
	actor, detail := last("POST /workspaces/" + name + "/variables")
	for _, want := range []string{"ARM_CLIENT_SECRET", `"status":303`, `"type":"user"`, `"role":"admin"`, `"ip":"192.0.2.1"`} {
		if actor != "admin@example.com" || !strings.Contains(detail, want) || strings.Contains(detail, "hunter2") {
			t.Fatalf("audit entry lacks %s: %s %s", want, actor, detail)
		}
	}
	if body := get(h, "/audit").Body.String(); !strings.Contains(body, "ARM_CLIENT_SECRET") || !strings.Contains(body, "intact") {
		t.Fatalf("audit page: %s", body)
	}

	// Refused requests are recorded too, reads included, with the error sent.
	user := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "someone@example.com", false, "grp-x"))
	if rec := get(user, "/audit"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin audit page: %d", rec.Code)
	}
	actor, detail = last("GET /audit")
	if actor != "someone@example.com" || !strings.Contains(detail, `"error":"admins only"`) || !strings.Contains(detail, `"groups":["grp-x"]`) {
		t.Fatalf("denied entry: %s %s", actor, detail)
	}
}
