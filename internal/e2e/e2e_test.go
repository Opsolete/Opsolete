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
// registry and keeps state in its http backend: no network beyond the test server. Last, a dev
// workspace is promoted to a target that runs one of its templates at an older version plus a third
// whose module configures its own provider: a destroy run removes the third, then the apply run
// updates and adds; discarded it leaves the target as the destroy did, promoted again it's one run.
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
	// a template whose module calls another module of the registry, as a VM module composes its building blocks
	nested := `{"id": "nested", "name": "Nested", "module": {"source": "opsolete.localhost:8080/examples/nested/terraform", "ref": "1.0.0"}, "inputs": []}`
	for name, body := range map[string]string{"greeting.json": string(greeting), "greeting-secret.json": secret, "nested.json": nested} {
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
	srv.ClaimWait = 500 * time.Millisecond
	if err := store.RegisterRunner(ctx, "e2e", storage.DefaultPool, runnerToken); err != nil {
		t.Fatal(err)
	}
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
	ws := "e2e-greeting" // the workspace await, confirm and runLog look at
	var runID uuid.UUID
	runLog := func(t *testing.T) string { return page(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/logs") }
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
		// the plan's attribute changes: what Terraform plans for the resource, an id it knows only after apply
		if p := page(t, "/workspaces/"+ws+"/runs/"+runID.String()); !strings.Contains(p, `<span class="change create">+1</span>`) ||
			!strings.Contains(p, `<details class="attrs">`) || !strings.Contains(p, "hi, Opsolete!") || !strings.Contains(p, "(known after apply)") {
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
	t.Run("a template's module calls another registry module: the run fetches it with its registry token", func(t *testing.T) {
		nested := t.TempDir()
		os.WriteFile(filepath.Join(nested, "main.tf"), []byte(`
module "inner" {
  source   = "opsolete.localhost:8080/examples/greeting/terraform"
  version  = "~> 1.1"
  greeting = "nested"
  name     = "Opsolete"
}
output "message" { value = module.inner.message }
`), 0o600)
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range map[string]string{"namespace": "examples", "name": "nested", "system": "terraform", "version": "1.0.0"} {
			mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("archive", "nested.tar.gz")
		fw.Write(tarGz(t, nested))
		mw.Close()
		send(t, "/registry", mw.FormDataContentType(), &body)
		runOf(postForm(t, "/workspaces/"+ws+"/templates/new?template=nested", url.Values{}))
		await(t, storage.NeedsConfirmation)
		if log := runLog(t); !strings.Contains(log, "Downloading opsolete.localhost:8080/examples/greeting/terraform 1.1.0 for") ||
			!strings.Contains(log, "nested, Opsolete!") {
			t.Fatalf("plan log: %s", log)
		}
		postForm(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/discard", nil) // the template goes again
	})
	const password = "Hunter2secret"
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

	// 1.0.0: the older version; 1.2.0: 1.1.0 configuring its own provider (the builtin terraform
	// provider: offline, and Terraform treats its block like any provider's), as catalog modules do
	ownProvider := t.TempDir()
	mainTF, err := os.ReadFile("../../examples/modules/greeting/1.1.0/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownProvider, "main.tf"), append(mainTF, "\nprovider \"terraform\" {}\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	for version, dir := range map[string]string{"1.0.0": "../../examples/modules/greeting/1.0.0", "1.2.0": ownProvider} {
		t.Run("upload the greeting module "+version, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			for k, v := range map[string]string{"namespace": "examples", "name": "greeting", "system": "terraform", "version": version} {
				mw.WriteField(k, v)
			}
			fw, _ := mw.CreateFormFile("archive", "greeting.tar.gz")
			fw.Write(tarGz(t, dir))
			mw.Close()
			send(t, "/registry", mw.FormDataContentType(), &body)
		})
	}
	var dev []storage.WorkspaceTemplate
	t.Run("promote: a dev workspace with two templates, applied", func(t *testing.T) {
		ws = "e2e-dev"
		runOf(postForm(t, "/templates/greeting", url.Values{"ops-workspace": {ws}, "greeting": {"hi"}, "name": {"Dev"}}))
		await(t, storage.NeedsConfirmation)
		confirm(t)
		await(t, storage.Applied)
		runOf(postForm(t, "/workspaces/"+ws+"/templates/new?template=greeting", url.Values{"greeting": {"hey"}, "name": {"Two"}}))
		await(t, storage.NeedsConfirmation)
		confirm(t)
		await(t, storage.Applied)
		w, _ := store.GetWorkspace(ctx, ws)
		if dev, _ = store.WorkspaceTemplates(ctx, w.ID); len(dev) != 2 {
			t.Fatalf("dev: %+v", dev)
		}
	})
	var prd uuid.UUID
	t.Run("promote: the target runs dev's first template at 1.0.0 and a third with its own provider, applied (auto-apply)", func(t *testing.T) {
		ws = "e2e-prd"
		w, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: ws, AutoApply: true})
		if err != nil {
			t.Fatal(err)
		}
		prd = w.ID
		for _, m := range [][4]string{{dev[0].Name, "1.0.0", `{"greeting": "hello"}`}, {"third", "1.2.0", `{"greeting": "hi", "name": "Third"}`}} {
			if _, err := store.Pool.Exec(ctx, putTemplate, w.ID, m[0], "greeting", dev[0].ModuleSource, m[1], m[2]); err != nil {
				t.Fatal(err)
			}
		}
		runOf(postForm(t, "/workspaces/"+ws+"/runs", url.Values{"action": {"apply"}}))
		await(t, storage.Applied)
		if res, _ := store.Resources(ctx, w.ID); len(res) != 2 {
			t.Fatalf("target resources: %+v", res)
		}
	})
	promote := func(t *testing.T, want ...string) {
		t.Helper()
		p := page(t, "/workspaces/e2e-dev/promote?target="+ws)
		for _, w := range want {
			if !strings.Contains(p, w) {
				t.Fatalf("preview lacks %q:\n%s", w, p)
			}
		}
		digest := p[strings.Index(p, `name="digest" value="`)+len(`name="digest" value="`):]
		runOf(postForm(t, "/workspaces/e2e-dev/promote", url.Values{"target": {ws}, "digest": {digest[:strings.Index(digest, `"`)]}}))
	}
	changes := func(t *testing.T) map[string]string {
		t.Helper()
		run, _, _ := store.GetRun(ctx, runID)
		got := map[string]string{}
		if run.Plan != nil {
			for _, c := range run.Plan.Changes {
				got[c.Address] = c.Action
			}
		}
		return got
	}
	addr := func(module string) string { return "module." + module + ".terraform_data.greeting" }
	templates := func(t *testing.T) string {
		t.Helper()
		list, _ := store.WorkspaceTemplates(ctx, prd)
		var out []string
		for _, x := range list {
			out = append(out, x.Name+"@"+x.ModuleRef)
		}
		return strings.Join(out, " ")
	}
	t.Run("promote: the preview says it runs twice; the first run destroys the third template only, with its provider block", func(t *testing.T) {
		promote(t, "<code>module."+dev[0].Name+"</code>", `<span class="tag">1.0.0</span> → <span class="tag">1.1.0</span>`,
			`<span class="change pill create">added</span>`, `<span class="change pill delete">removed</span>`, "Destroys the 1 resource it manages.",
			"This promotion runs twice: first a destroy of <code>module.third</code> (1 resource)")
		await(t, storage.NeedsConfirmation) // auto-apply, but a promotion waits
		if got := changes(t); len(got) != 1 || got[addr("third")] != "delete" {
			t.Fatalf("destroy plan: %v", got)
		}
		if p := page(t, "/workspaces/"+ws+"/runs/"+runID.String()); !strings.Contains(p, "Promotion from e2e-dev: destroys third first") {
			t.Fatalf("run page: %s", p)
		}
		confirm(t)
		await(t, storage.Applied)
	})
	t.Run("promote: the apply run follows; discarded, the target is back after the destroy", func(t *testing.T) {
		if err := store.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE workspace_id = $1 AND run_active(status)`, prd).Scan(&runID); err != nil {
			t.Fatalf("no apply run queued: %v", err)
		}
		await(t, storage.NeedsConfirmation)
		if got := changes(t); got[addr(dev[1].Name)] != "create" || got[addr(dev[0].Name)] == "" || got[addr("third")] != "" {
			t.Fatalf("apply plan: %v", got)
		}
		postForm(t, "/workspaces/"+ws+"/runs/"+runID.String()+"/discard", nil)
		if got := templates(t); got != dev[0].Name+"@1.0.0" {
			t.Fatalf("after discard: %s", got)
		}
		res, _ := store.Resources(ctx, prd)
		if len(res) != 1 || strings.HasPrefix(res[0].Address, "module.third.") {
			t.Fatalf("resources: %+v", res)
		}
		if out := page(t, "/workspaces/"+ws+"?show=outputs"); strings.Contains(out, "third__") {
			t.Fatalf("outputs: %s", out)
		}
	})
	t.Run("promote: nothing left to remove, one run; applied, the target runs exactly dev's templates", func(t *testing.T) {
		promote(t)
		await(t, storage.NeedsConfirmation)
		if run, _, _ := store.GetRun(ctx, runID); run.Action != "apply" {
			t.Fatalf("one apply run: %s", run.Action)
		}
		confirm(t)
		await(t, storage.Applied)
		tgt, _ := store.WorkspaceTemplates(ctx, prd)
		if len(tgt) != len(dev) {
			t.Fatalf("target templates %+v, want %+v", tgt, dev)
		}
		for i := range dev {
			if tgt[i].Name != dev[i].Name || tgt[i].TemplateID != dev[i].TemplateID || tgt[i].ModuleRef != dev[i].ModuleRef ||
				tgt[i].Inputs["greeting"] != dev[i].Inputs["greeting"] || tgt[i].Inputs["name"] != dev[i].Inputs["name"] {
				t.Fatalf("target templates %+v, want %+v", tgt, dev)
			}
		}
		if res, _ := store.Resources(ctx, prd); len(res) != 2 {
			t.Fatalf("resources: %+v", res)
		}
		if out := page(t, "/workspaces/"+ws+"?show=outputs"); strings.Contains(out, "third__") || !strings.Contains(out, "hi, Dev!") || !strings.Contains(out, "hey, Two!") {
			t.Fatalf("outputs: %s", out)
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

// putTemplate ($1 workspace, $2 name, $3 template, $4 source, $5 ref, $6 inputs) makes a template part of the
// workspace's current configuration version, as an applied run would.
const putTemplate = `WITH v AS (
	INSERT INTO configuration_versions (workspace_id, parent_id, source) SELECT id, current_cv_id, 'form' FROM workspaces WHERE id = $1 RETURNING id, parent_id
), keep AS (
	INSERT INTO configuration_modules (cv_id, name, template_id, module_source, module_ref, inputs)
	SELECT v.id, m.name, m.template_id, m.module_source, m.module_ref, m.inputs FROM v JOIN configuration_modules m ON m.cv_id = v.parent_id WHERE m.name <> $2
), put AS (
	INSERT INTO configuration_modules (cv_id, name, template_id, module_source, module_ref, inputs) SELECT v.id, $2, $3, $4, $5, $6 FROM v
)
UPDATE workspaces w SET current_cv_id = v.id FROM v WHERE w.id = $1`
