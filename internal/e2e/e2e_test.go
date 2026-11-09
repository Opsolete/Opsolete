// Package e2e runs the real runner, with the real Terraform, against the real server on a test
// database: the one place both halves of the runner protocol meet in `go test`. Its own package
// because the runner hardens its process (on Linux: not dumpable, child subreaper, every child
// killed after a job), which other packages' tests must not inherit.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/config"
	httpserver "github.com/opsolete/opsolete/internal/http"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runner"
	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/testdb"
	"github.com/opsolete/opsolete/internal/vcs"
)

const runnerToken = "e2e-runner-token-0123456789abcdef"

// TestRealRunner: an admin uploads the greeting module to the registry and creates a workspace from
// the catalog's greeting template; the runner plans it with Terraform, the admin confirms, the runner
// applies. A second template with a sensitive input is added (masked in the log and its outputs) and removed again
// (a targeted destroy, then its outputs dropped); then the workspace is destroyed. Terraform fetches the module from the server's own
// registry and keeps state in its http backend: no network beyond the test server.
func TestRealRunner(t *testing.T) {
	tf := os.Getenv("TERRAFORM")
	if tf == "" {
		var err error
		if tf, err = exec.LookPath("terraform"); err != nil {
			t.Skip("terraform is not on PATH (CI installs it): the real runner needs it")
		}
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	store.AuditKey = []byte("e2e-audit-key")
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	keys, err := secrets.New("e2e-secret-key", "")
	if err != nil {
		t.Fatal(err)
	}
	// the catalog's greeting template, and a copy whose "name" is a sensitive input: the module's
	// message output (not sensitive) prints it, so only masking keeps it out of the log and the stored outputs
	dir := t.TempDir()
	greeting, err := os.ReadFile("../../catalog/greeting.json")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.NewReplacer(`"id": "greeting"`, `"id": "greeting-secret"`, `"name": "Greeting (example)"`, `"name": "Greeting (secret)"`,
		`"required": true, "pattern": "^[A-Za-z0-9 ]{1,40}$", "default": "world", "key": true`, `"required": true, "sensitive": true`).Replace(string(greeting))
	if !strings.Contains(secret, `"sensitive": true`) || !strings.Contains(secret, "greeting-secret") {
		t.Fatalf("sensitive template: %s", secret)
	}
	for name, body := range map[string]string{"greeting.json": string(greeting), "greeting-secret.json": secret} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tpls, err := templates.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// the catalog's greeting template names this registry host
	public, _ := url.Parse("http://opsolete.localhost:8080")
	notify := &app.Notifier{DB: store.Pool.Config().ConnConfig, Log: log}
	nctx, stopNotify := context.WithCancel(ctx)
	waitNotify := notify.Start(nctx)
	t.Cleanup(func() { stopNotify(); waitNotify() })
	a := &app.App{Store: store, Blobs: memblob.OpenBucket(nil), Secrets: keys, Templates: tpls, Notify: notify, Log: log,
		Registry: &registry.Registry{PublicURL: public, Key: []byte("e2e-registry-key")}, VCS: vcs.NewClient(nil)}
	au, err := auth.New(ctx, auth.Config{DevUser: "admin@example.com", Admins: []string{"admin@example.com"}, PublicURL: public}, []byte("k"), store)
	if err != nil {
		t.Fatal(err)
	}
	srv := httpserver.New(a, au, httpserver.Features{Deployments: true, Registry: true, Editor: true})
	srv.RunnerToken, srv.ClaimWait = runnerToken, 500*time.Millisecond
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// the runner, until the test ends
	serverURL, _ := url.Parse(ts.URL)
	r := runner.New(config.Runner{ServerURL: serverURL, Token: runnerToken, Name: "e2e", Terraform: tf, JobTimeout: time.Minute, JobGrace: 10 * time.Second}, log)
	drain, stop := context.WithCancel(ctx)
	hard, kill := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(drain, hard) }()
	t.Cleanup(func() {
		stop()
		kill()
		if err := <-done; err != nil {
			t.Errorf("runner: %v", err)
		}
	})

	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	send := func(t *testing.T, path, contentType string, body io.Reader) *http.Response {
		t.Helper()
		res, err := client.Post(ts.URL+path, contentType, body)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: %d", path, res.StatusCode)
		}
		return res
	}
	postForm := func(t *testing.T, path string, form url.Values) *http.Response {
		return send(t, path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	}
	page := func(t *testing.T, path string) string {
		t.Helper()
		res, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", path, res.StatusCode)
		}
		return string(b)
	}
	const ws = "e2e-greeting"
	var runID uuid.UUID
	// await waits for the run to reach status, failing on any other ended status.
	await := func(t *testing.T, status string) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			run, _, err := store.GetRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status == status {
				return
			}
			if storage.Ended(run.Status) {
				t.Fatalf("run ended %s, want %s; log:\n%s", run.Status, status, page(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/logs"))
			}
		}
		t.Fatalf("run never reached %s; log:\n%s", status, page(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/logs"))
	}
	runOf := func(res *http.Response) {
		loc := res.Header.Get("Location")
		runID = uuid.MustParse(loc[strings.LastIndex(loc, "/")+1:])
	}
	confirm := func(t *testing.T) {
		t.Helper()
		postForm(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/confirm", nil)
	}
	start := time.Now()

	t.Run("upload the greeting module to the registry", func(t *testing.T) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range map[string]string{"namespace": "examples", "name": "greeting", "system": "terraform", "version": "1.1.0"} {
			mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("archive", "greeting.tar.gz")
		fw.Write(tarGz(t, "../../examples/modules/greeting/1.1.0"))
		mw.Close()
		send(t, "/registry", mw.FormDataContentType(), &body)
	})
	t.Run("create a workspace from the template: the runner plans it", func(t *testing.T) {
		runOf(postForm(t, "/templates/greeting", url.Values{"ops-workspace": {ws}, "greeting": {"hi"}, "name": {"Opsolete"}}))
		await(t, storage.NeedsConfirmation)
		if p := page(t, "/workspaces/"+ws+"/runs/"+runID.String()); !strings.Contains(p, `<span class="change create">+1</span>`) {
			t.Fatalf("plan page: %s", p)
		}
	})
	t.Run("confirmed, the runner applies it", func(t *testing.T) {
		confirm(t)
		await(t, storage.Applied)
		if out := page(t, "/workspaces/"+ws+"?show=outputs"); !strings.Contains(out, "hi, Opsolete!") {
			t.Fatalf("outputs: %s", out)
		}
		w, _ := store.GetWorkspace(ctx, ws)
		vs, _ := store.StateVersions(ctx, w.ID, 10)
		if len(vs) != 1 || !vs[0].Indexed || vs[0].Resources != 1 {
			t.Fatalf("state versions: %+v", vs)
		}
		if res, _ := store.Resources(ctx, w.ID); len(res) != 1 || !strings.HasSuffix(res[0].Address, ".terraform_data.greeting") {
			t.Fatalf("resources: %+v", res)
		}
		if log := page(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/logs"); !strings.Contains(log, "Plan: 1 to add") ||
			!strings.Contains(log, "Apply complete! Resources: 1 added") {
			t.Fatalf("log: %s", log)
		}
	})
	const password = "Hunter2secret"
	runLog := func(t *testing.T) string { return page(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/logs") }
	var secretModule string
	t.Run("a sensitive input reaches Terraform, masked in the log and on no page", func(t *testing.T) {
		runOf(postForm(t, "/workspaces/"+ws+"/templates/new?template=greeting-secret", url.Values{"greeting": {"hey"}, "secret.name": {password}}))
		await(t, storage.NeedsConfirmation)
		if log := runLog(t); !strings.Contains(log, `"hey, ***!"`) || strings.Contains(log, password) {
			t.Fatalf("plan log must show the value masked: %s", log)
		}
		confirm(t)
		await(t, storage.Applied)
		if log := runLog(t); !strings.Contains(log, `"hey, ***!"`) || strings.Contains(log, password) {
			t.Fatalf("apply log must show the value masked: %s", log)
		}
		w, _ := store.GetWorkspace(ctx, ws)
		added, _ := store.WorkspaceTemplates(ctx, w.ID)
		for _, a := range added {
			if a.TemplateID == "greeting-secret" {
				secretModule = a.Name
			}
		}
		// The module's message output echoes the value and isn't marked sensitive: the runner (the
		// run's outputs) and the server (the state version's) store it masked, so no page shows it.
		vs, _ := store.StateVersions(ctx, w.ID, 1)
		for _, p := range []string{"/workspaces/" + ws + "?show=templates", "/workspaces/" + ws + "/templates/" + secretModule,
			"/workspaces/" + ws + "/runs", "/workspaces/" + ws + "/runs/" + runID.String() + "/logs/stream",
			"/workspaces/" + ws + "/runs/" + runID.String(), "/workspaces/" + ws + "?show=outputs",
			"/workspaces/" + ws + "/states/" + strconv.FormatInt(vs[0].ID, 10)} {
			body := page(t, p)
			if strings.Contains(body, password) {
				i := strings.Index(body, password)
				t.Errorf("%s shows the sensitive value: …%s…", p, body[max(0, i-400):i+100])
			}
			if strings.Contains(p, "/states/") || strings.HasSuffix(p, "outputs") || strings.HasSuffix(p, runID.String()) {
				if !strings.Contains(body, "(sensitive)") {
					t.Errorf("%s doesn't show the masked output: %s", p, body)
				}
			}
		}
		var admin uuid.UUID
		if err := store.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email = 'admin@example.com'`).Scan(&admin); err != nil {
			t.Fatal(err)
		}
		tok, err := store.CreateUserToken(ctx, admin, "e2e", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("GET", ts.URL+"/api/v2/workspaces/"+w.ID.String()+"/current-state-version-outputs", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.Contains(string(b), secretModule+"__message") || strings.Contains(string(b), password) {
			t.Errorf("cloud API outputs: %d %s", res.StatusCode, b)
		}
		var stored string
		if err := store.Pool.QueryRow(ctx, `SELECT coalesce((SELECT string_agg(outputs::text, ' ') FROM runs WHERE workspace_id = $1), '') ||
			coalesce((SELECT string_agg(outputs::text, ' ') FROM state_versions WHERE workspace_id = $1), '')`, w.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stored, password) || !strings.Contains(stored, `"(sensitive)"`) {
			t.Errorf("stored outputs: %s", stored)
		}
	})
	t.Run("removing a deployed template destroys its resources, then drops its outputs", func(t *testing.T) {
		runOf(postForm(t, "/workspaces/"+ws+"/templates/"+secretModule+"/remove", nil))
		await(t, storage.NeedsConfirmation)
		w, _ := store.GetWorkspace(ctx, ws)
		added, _ := store.WorkspaceTemplates(ctx, w.ID)
		if len(added) != 2 {
			t.Fatalf("the template goes only once its destroy is applied: %+v", added)
		}
		other := added[0].Name
		if other == secretModule {
			other = added[1].Name
		}
		if r, err := client.Post(ts.URL+"/workspaces/"+ws+"/templates/"+other+"/remove", "application/x-www-form-urlencoded", nil); err != nil || r.StatusCode != http.StatusConflict {
			t.Fatalf("removing another template during the run: %v %v", r, err)
		} else {
			r.Body.Close()
		}
		confirm(t)
		await(t, storage.Applied)
		log := runLog(t)
		for _, want := range []string{"Resources: 0 added, 0 changed, 1 destroyed", "terraform apply (outputs only)"} {
			if !strings.Contains(log, want) {
				t.Fatalf("log lacks %q: %s", want, log)
			}
		}
		if added, _ := store.WorkspaceTemplates(ctx, w.ID); len(added) != 1 || added[0].Name != other {
			t.Fatalf("templates after the removal: %+v", added)
		}
		if res, _ := store.Resources(ctx, w.ID); len(res) != 1 || strings.Contains(res[0].Address, secretModule) {
			t.Fatalf("resources after the removal: %+v", res)
		}
		if out := page(t, "/workspaces/"+ws+"?show=outputs"); strings.Contains(out, secretModule) || !strings.Contains(out, "hi, Opsolete!") {
			t.Fatalf("outputs after the removal: %s", out)
		}
	})
	t.Run("destroy: planned, confirmed, applied", func(t *testing.T) {
		runOf(postForm(t, "/workspaces/"+ws+"/runs", url.Values{"action": {"destroy"}}))
		await(t, storage.NeedsConfirmation)
		confirm(t)
		await(t, storage.Applied)
		w, _ := store.GetWorkspace(ctx, ws)
		if res, _ := store.Resources(ctx, w.ID); len(res) != 0 {
			t.Fatalf("resources after destroy: %+v", res)
		}
		if p := page(t, "/workspaces/"+ws); !strings.Contains(p, "destroyed") {
			t.Fatalf("workspace page after destroy: %s", p)
		}
		if log := page(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/logs"); !strings.Contains(log, "Resources: 0 added, 0 changed, 1 destroyed") {
			t.Fatalf("destroy log: %s", log)
		}
	})
	t.Logf("the runs took %s", time.Since(start).Round(time.Millisecond))
}

// tarGz packs the files of dir, as `tar czf x.tar.gz -C dir .` does.
func tarGz(t *testing.T, dir string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.AddFS(os.DirFS(filepath.Clean(dir))); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	return buf.Bytes()
}
