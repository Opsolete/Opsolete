package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/storage"
)

func TestAuditLogRecordsChanges(t *testing.T) {
	store := testStore(t)
	h := newServer(t, "../../catalog", false, store)
	name := "audit-" + strings.ToLower(newID()[:8])
	if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {name}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/workspaces/"+name+"/variables", url.Values{"category": {"env"}, "key": {"ARM_CLIENT_SECRET"},
		"value": {"hunter2"}, "sensitive": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set variable: %d %s", rec.Code, rec.Body.String())
	}
	last := func(action string) (actor, detail string) {
		t.Helper()
		if err := store.Pool.QueryRow(context.Background(), `SELECT actor, detail::text FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`,
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
	// Refused requests are listed apart from the changes.
	if body := get(h, "/audit").Body.String(); strings.Contains(body, "GET /audit") {
		t.Fatalf("changes list shows a refused request: %s", body)
	}
	if body := get(h, "/audit?show=access").Body.String(); !strings.Contains(body, "GET /audit") || strings.Contains(body, "ARM_CLIENT_SECRET") {
		t.Fatalf("access list: %s", body)
	}
}

// Audit runs before login: an anonymous POST can't make it parse or store much.
func TestAuditCapsForms(t *testing.T) {
	store := testStore(t)
	h := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "someone@example.com", false))
	if rec := post(h, "/workspaces/new", url.Values{"x": {strings.Repeat("a", maxFormSize)}}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized form: %d", rec.Code)
	}
	path := "/audit-cap-" + strings.ToLower(newID()[:8])
	post(h, path, url.Values{"x": {strings.Repeat("a", 2*maxAuditValue)}})
	var detail string
	if err := store.Pool.QueryRow(context.Background(), `SELECT detail::text FROM audit_log WHERE action = $1`, "POST "+path).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "(truncated)") || strings.Contains(detail, strings.Repeat("a", maxAuditValue+1)) {
		t.Fatalf("value not truncated: %d bytes", len(detail))
	}
}

func TestAuditRowSummary(t *testing.T) {
	for _, k := range []string{"admin_password", "secret.x", "client_secret", "API_TOKEN"} {
		if !secretName(k) {
			t.Errorf("secretName(%q) = false", k)
		}
	}
	if secretName("key") || secretName("ssh_public_key") {
		t.Error("secretName redacts a plain field")
	}
	row := newAuditRow(storage.AuditEntry{Detail: []byte(`{"form":{"description":[""],"in_label":["x"],"module_ref":["2.0.0"],
		"module_source":["opsolete.lan/terraform-modules-examples/virtual-machine/azurerm"],"name":["vm"],"zz":["1"]},
		"request":{"ip":"127.0.0.1","forwarded_for":"100.1.2.3","status":422,"error":"bad input"}}`)})
	want := "name=vm module_ref=2.0.0 module_source=opsolete.lan/terraform-modules-examples… +1 more · bad input"
	if row.Summary != want || row.From != "100.1.2.3 (via 127.0.0.1)" || row.StatusText != "rejected" || !strings.Contains(row.Pretty, "\n  ") {
		t.Fatalf("row: %q %q %q", row.Summary, row.From, row.StatusText)
	}
}

// TestAuditLogTampered (N2): the table refuses changes, and an entry appended without SECRET_KEY
// breaks the chain: the page names it, even with valid entries after it.
func TestAuditLogTampered(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	h := newServer(t, "../../catalog", false, store)
	change := func() {
		t.Helper()
		if rec := post(h, "/workspaces/new", url.Values{fieldWorkspace: {"at-" + strings.ToLower(newID()[:8])}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("create: %d", rec.Code)
		}
	}
	change()
	if _, err := store.Pool.Exec(ctx, `UPDATE audit_log SET actor = 'mallory'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE of the audit log: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("DELETE from the audit log succeeded")
	}
	// someone with database access but not SECRET_KEY appends an entry, chained like a real one
	key := store.AuditKey
	store.AuditKey = []byte("not-the-secret-key")
	if err := store.Audit(ctx, "mallory", "forged", nil); err != nil {
		t.Fatal(err)
	}
	store.AuditKey = key
	var forged int64
	if err := store.Pool.QueryRow(ctx, `SELECT max(id) FROM audit_log`).Scan(&forged); err != nil {
		t.Fatal(err)
	}
	change() // signed again, after the forged one
	pageHas(t, h, "/audit", fmt.Sprintf("The audit log was tampered with: entry %d was changed", forged))
}
