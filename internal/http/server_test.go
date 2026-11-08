package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/auth/authtest"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/testdb"
)

var testReg = func() *registry.Registry {
	u, _ := url.Parse("https://ops.example.com")
	return &registry.Registry{PublicURL: u, Key: []byte("test-key")}
}()

const testRunnerToken = "test-runner-token-0123456789abcdef"

func newTestServer(t *testing.T) http.Handler {
	return newServer(t, "../../catalog", false, testStore(t))
}

// newServer is logged in as an admin dev user.
func newServer(t *testing.T, dir string, editor bool, store *storage.Store) http.Handler {
	return newServerAs(t, dir, editor, store, devAuth(t, store, "admin@example.com", true))
}

func newServerAs(t *testing.T, dir string, editor bool, store *storage.Store, a *auth.Auth) http.Handler {
	return newApp(t, dir, store, a, Features{Deployments: true, Registry: true, Editor: editor}).Handler()
}

func newApp(t *testing.T, dir string, store *storage.Store, a *auth.Auth, f Features) *Server {
	tpls, err := templates.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&app.App{Store: store, Blobs: memblob.OpenBucket(nil), Secrets: testKeys, Templates: tpls, Registry: testReg,
		Notify: startNotifier(t, store), Log: testLog(t)}, a, f)
	srv.RunnerToken, srv.ClaimWait = testRunnerToken, 100*time.Millisecond
	return srv
}

// startNotifier listens until t ends, as main.go does on every replica.
func startNotifier(t *testing.T, store *storage.Store) *app.Notifier {
	n := &app.Notifier{DB: store.Pool.Config().ConnConfig, Log: testLog(t)}
	ctx, cancel := context.WithCancel(context.Background())
	wait := n.Start(ctx)
	t.Cleanup(func() { cancel(); wait() })
	return n
}

// testLog discards the log, but fails the test on errNotShown (a form that doesn't re-render its
// ValidationError) and errRender (a page template that doesn't fit its model).
func testLog(t *testing.T) *slog.Logger {
	return slog.New(notShown{slog.DiscardHandler, t})
}

type notShown struct {
	slog.Handler
	t *testing.T
}

func (h notShown) Enabled(context.Context, slog.Level) bool { return true }

func (h notShown) Handle(_ context.Context, r slog.Record) error {
	if r.Message == errRender {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "err" {
				h.t.Errorf("%s: %s", errRender, a.Value)
			}
			return true
		})
	}
	if r.Message == errNotShown {
		var path string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "path" {
				path = a.Value.String()
			}
			return true
		})
		h.t.Errorf("%s: %s", errNotShown, path)
	}
	return nil
}

func devAuth(t *testing.T, store *storage.Store, email string, admin bool, groups ...string) *auth.Auth {
	save := func(context.Context, string, string, string, []string) (uuid.UUID, error) { return uuid.Nil, nil }
	if store != nil {
		save = store.UpsertUser
	}
	var admins []string
	if admin {
		admins = []string{email}
	}
	a, err := auth.New(context.Background(), auth.Config{DevUser: email, DevGroups: groups, Admins: admins, PublicURL: testReg.PublicURL}, []byte("k"), save)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// operateDefault grants group operate on the default project, where only admins and such groups
// create workspaces.
func operateDefault(t *testing.T, store *storage.Store, group string) {
	t.Helper()
	def, err := store.DefaultProject(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProjectGrant(t.Context(), def.ID, group, "operate"); err != nil {
		t.Fatal(err)
	}
}

// testStore returns a migrated store on a database of t's own (testdb), or skips without TEST_DATABASE_URL.
func testStore(t *testing.T) *storage.Store {
	s, err := storage.Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.AuditKey = []byte("test-audit-key")
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// step runs one step of a scenario test as a subtest. Every later step needs the state this one
// leaves, so a step that failed ends the scenario, and one that -run filtered out skips the rest.
// Checks that change nothing use t.Run instead: the scenario goes on after their failure.
func step(t *testing.T, name string, f func(t *testing.T)) {
	t.Helper()
	ran := false
	if !t.Run(name, func(t *testing.T) { ran = true; f(t) }) {
		t.Fatalf("step %q failed: the steps after it need its state", name)
	}
	if !ran {
		t.Skipf("step %q was filtered out: the steps after it need its state", name)
	}
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(h, req)
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	return do(h, httptest.NewRequest("GET", path, nil))
}

func TestPublicPages(t *testing.T) {
	h := newTestServer(t)
	for path, want := range map[string]string{
		"/healthz":                    "ok",
		"/":                           "Azure Storage Account",
		"/templates/storage-account":  `name="retention_days"`,
		"/.well-known/terraform.json": `"modules.v1":"/v1/modules/"`,
	} {
		rec := get(h, path)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s: %d, missing %q", path, rec.Code, want)
		}
	}
}

func TestLoginRequired(t *testing.T) {
	is := authtest.New(t)
	a, err := auth.New(context.Background(), auth.Config{Issuer: is.URL, ClientID: authtest.ClientID, ClientSecret: authtest.ClientSecret,
		PublicURL: testReg.PublicURL}, []byte("k"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newServerAs(t, "../../catalog", true, testStore(t), a)
	if rec := get(h, "/workspaces?x=1"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=%2Fworkspaces%3Fx%3D1" {
		t.Fatalf("anonymous page view: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/templates/storage-account", "/templates/new", "/registry", "/workspaces/x/runs"} {
		if rec := post(h, path, url.Values{"name": {"abc"}}); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous POST %s: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/healthz", "/.well-known/terraform.json"} {
		if rec := get(h, path); rec.Code != http.StatusOK {
			t.Errorf("public %s: %d", path, rec.Code)
		}
	}
	if rec := get(h, "/auth/login"); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), is.URL) {
		t.Fatalf("login must redirect to the issuer: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	req := httptest.NewRequest("POST", "/templates/storage-account", strings.NewReader("name=abc"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := do(newTestServer(t), req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", rec.Code)
	}
}

func TestFeatureSwitches(t *testing.T) {
	store := testStore(t)
	a := devAuth(t, store, "admin@example.com", true)
	noRegistry := newApp(t, "../../catalog", store, a, Features{Deployments: true, Editor: true}).Handler()
	for _, path := range []string{"/registry", "/v1/modules/a/b/c/versions"} {
		if rec := get(noRegistry, path); rec.Code != http.StatusNotFound {
			t.Errorf("registry off, GET %s: %d", path, rec.Code)
		}
	}
	if d := get(noRegistry, "/.well-known/terraform.json").Body.String(); strings.Contains(d, "modules.v1") || !strings.Contains(d, `"tfe.v2":"/api/v2/"`) {
		t.Errorf("registry off, discovery: %s", d)
	}
	if page := get(noRegistry, "/").Body.String(); strings.Contains(page, `href="/registry"`) || !strings.Contains(page, `href="/workspaces"`) {
		t.Fatalf("nav with registry off: %s", page)
	}
	if page := get(noRegistry, "/templates/new").Body.String(); strings.Contains(page, "Start from a registry module") {
		t.Fatal("builder must not offer registry modules when the registry is off")
	}
	// Registry templates are refused while the registry is off.
	form := url.Values{"name": {"x"}, "module_source": {"ops.example.com/acme/storage/azurerm"}, "module_ref": {"1.0.0"}}
	dir := t.TempDir()
	if rec := post(newApp(t, dir, store, a, Features{Deployments: true, Editor: true}).Handler(), "/templates/new", form); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("registry template saved with registry off: %d", rec.Code)
	}
	if err := newApp(t, dir, store, a, Features{Deployments: true}).App.CheckTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/reg.json", []byte(`{"id":"reg","name":"r","module":{"source":"ops.example.com/acme/storage/azurerm","ref":"1.0.0"},"inputs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newApp(t, dir, store, a, Features{Deployments: true}).App.CheckTemplates(); err == nil {
		t.Fatal("startup must fail on a registry template while the registry is off")
	}

	registryOnly := newApp(t, "../../catalog", store, a, Features{Registry: true, Editor: true}).Handler()
	if rec := get(registryOnly, "/"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/registry" {
		t.Fatalf("deployments off, GET /: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/workspaces", "/templates/storage-account", "/templates/new"} {
		if rec := get(registryOnly, path); rec.Code != http.StatusNotFound {
			t.Errorf("deployments off, GET %s: %d", path, rec.Code)
		}
	}
	if rec := post(registryOnly, runnerapi.Prefix+"/jobs/claim", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("runner API must be off with deployments: %d", rec.Code)
	}
	if rec := get(registryOnly, "/.well-known/terraform.json"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "tfe.v2") {
		t.Fatalf("registry protocol must stay on, the cloud API off: %d %s", rec.Code, rec.Body.String())
	}
}

func newID() string { return uuid.NewString() }

var testKeys = func() *secrets.Keyring {
	k, err := secrets.New("test-secret-key", "")
	if err != nil {
		panic(err)
	}
	return k
}()
