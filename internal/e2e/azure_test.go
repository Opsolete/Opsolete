package e2e

import (
	"bytes"
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

// TestAzureImport runs an import session against real Azure (only with OPS_AZURE_E2E=1 and ARM_CLIENT_ID,
// ARM_CLIENT_SECRET, ARM_TENANT_ID, ARM_SUBSCRIPTION_ID set): a resource group made outside Opsolete is imported
// by a template whose module imports itself (examples/modules/azure-rg). The form's default region differs from
// the group's, so the first round would replace it and can't be adopted; planned again with what it found, it only
// imports, is adopted, confirmed and applied. Then a change made outside Opsolete shows as drift, and removing
// the template destroys the group. Credentials come in a global variable set, sensitive: never in a log.
// Free resources only: a resource group, in westeurope/northeurope.
func TestAzureImport(t *testing.T) {
	if os.Getenv("OPS_AZURE_E2E") != "1" {
		t.Skip("OPS_AZURE_E2E=1 and ARM_* credentials run it against real Azure")
	}
	creds := map[string]string{}
	for _, k := range []string{"ARM_CLIENT_ID", "ARM_CLIENT_SECRET", "ARM_TENANT_ID", "ARM_SUBSCRIPTION_ID"} {
		if creds[k] = os.Getenv(k); creds[k] == "" {
			t.Fatalf("%s is not set", k)
		}
	}
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform is not on PATH")
	}
	ctx := context.Background()
	rg := "ops-e2e-" + strings.ToLower(uuid.NewString()[:8])
	cache := filepath.Join(os.TempDir(), "opsolete-e2e-plugins")
	os.MkdirAll(cache, 0o755)
	t.Setenv("TF_PLUGIN_CACHE_DIR", cache)

	// The group made outside Opsolete, with Terraform of its own (removed again however the test ends). It gets
	// the credentials explicitly: they leave this process's environment below, for the runner's sake.
	outside := t.TempDir()
	outsideEnv := os.Environ()
	for k, v := range creds {
		outsideEnv = append(outsideEnv, k+"="+v)
	}
	outsideTF := func(t *testing.T, tag string, args ...string) {
		t.Helper()
		cfg := `provider "azurerm" {
  features {}
}
resource "azurerm_resource_group" "pre" {
  name     = "` + rg + `"
  location = "northeurope"
  tags     = { owner = "` + tag + `" }
}
`
		if err := os.WriteFile(filepath.Join(outside, "main.tf"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, a := range [][]string{{"init", "-input=false"}, args} {
			cmd := exec.Command(tf, a...)
			cmd.Dir, cmd.Env = outside, outsideEnv
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("terraform %v: %v\n%s", a, err, out)
			}
		}
	}
	outsideTF(t, "e2e", "apply", "-auto-approve", "-input=false")
	t.Cleanup(func() { // whatever Opsolete did: nothing stays behind in the subscription
		cmd := exec.Command(tf, "destroy", "-auto-approve", "-input=false")
		cmd.Dir, cmd.Env = outside, outsideEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("clean up %s: %v\n%s", rg, err, out)
		}
	})

	// Opsolete: server, runner, a registry module and its template.
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
	keys, _ := secrets.New("e2e-secret-key", "")
	dir := t.TempDir()
	tpl := `{"id": "rg", "name": "Resource group", "module": {"source": "opsolete.localhost:8080/e2e/rg/azurerm", "ref": "1.0.0"},
		"inputs": [{"name": "name", "type": "string", "required": true, "key": true},
		{"name": "location", "type": "select", "options": ["westeurope", "northeurope"], "default": "westeurope"},
		{"name": "tags", "type": "map"}, {"name": "existing", "type": "bool"}]}`
	os.WriteFile(filepath.Join(dir, "rg.json"), []byte(tpl), 0o600)
	tpls, err := templates.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("http://opsolete.localhost:8080")
	a := &app.App{Store: store, Blobs: memblob.OpenBucket(nil), Secrets: keys, Templates: tpls, Log: log,
		Registry: &registry.Registry{PublicURL: public, Key: []byte("e2e-registry-key")}, RegistryEnabled: true, VCS: vcs.NewClient(nil)}
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
	serverURL, _ := url.Parse(ts.URL)
	// the runner gets Azure credentials only from the job (the variable set): not from its own environment
	for k := range creds {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	r := runner.New(config.Runner{ServerURL: serverURL, Token: runnerToken, Name: "e2e", Terraform: tf, JobTimeout: 10 * time.Minute, JobGrace: time.Minute}, log)
	drain, stop := context.WithCancel(ctx)
	hard, kill := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(drain, hard) }()
	t.Cleanup(func() { stop(); kill(); <-done })

	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	send := func(t *testing.T, path, contentType string, body io.Reader) string {
		t.Helper()
		res, err := client.Post(ts.URL+path, contentType, body)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: %d %s", path, res.StatusCode, b)
		}
		return res.Header.Get("Location")
	}
	post := func(t *testing.T, path string, form url.Values) string {
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
		return string(b)
	}
	runOf := func(loc string) uuid.UUID { return uuid.MustParse(loc[strings.LastIndex(loc, "/")+1:]) }
	ws := "azure-" + strings.ToLower(uuid.NewString()[:6])
	await := func(t *testing.T, id uuid.UUID, status string) storage.Run {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
			run, _, err := store.GetRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status == status {
				return run
			}
			if storage.Ended(run.Status) {
				t.Fatalf("run %s ended %s, want %s; log:\n%s", id, run.Status, status, page(t, "/workspaces/"+ws+"/runs/"+id.String()+"/logs"))
			}
		}
		t.Fatalf("run %s never reached %s", id, status)
		return storage.Run{}
	}
	var runs []uuid.UUID

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range map[string]string{"namespace": "e2e", "name": "rg", "system": "azurerm", "version": "1.0.0"} {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("archive", "rg.tar.gz")
	fw.Write(tarGz(t, "../../examples/modules/azure-rg/1.0.0"))
	mw.Close()
	send(t, "/registry", mw.FormDataContentType(), &body)
	post(t, "/variable-sets", url.Values{"name": {"azure"}, "global": {"on"}})
	for k, v := range creds {
		post(t, "/variable-sets/azure/variables", url.Values{"category": {"env"}, "key": {k}, "value": {v}, "sensitive": {"on"}})
	}

	// Round 1: the form's default region isn't the group's: the plan would replace it, so it can't be adopted.
	round1 := runOf(post(t, "/templates/rg", url.Values{"ops-workspace": {ws}, "name": {rg}, "existing": {"on"}}))
	runs = append(runs, round1)
	r1 := await(t, round1, storage.Planned)
	if r1.Plan == nil || r1.Plan.Import != 1 || r1.Plan.Destroy != 1 {
		t.Fatalf("round 1 plan: %+v", r1.Plan)
	}
	round1URL := "/workspaces/" + ws + "/runs/" + round1.String()
	if p := page(t, round1URL); !strings.Contains(p, "northeurope") || !strings.Contains(p, round1URL+"/plan-again") || strings.Contains(p, round1URL+"/adopt") {
		t.Fatalf("round 1's import card: the found region, plan again, no adopt:\n%s", p)
	}

	// Round 2, planned again with what round 1 found: it only imports, and is adopted.
	round2 := runOf(post(t, round1URL+"/plan-again", nil))
	runs = append(runs, round2)
	r2 := await(t, round2, storage.Planned)
	if r2.Plan == nil || r2.Plan.Import != 1 || r2.Plan.Destroy != 0 || r2.Plan.Add != 0 {
		t.Fatalf("round 2 plan: %+v", r2.Plan)
	}
	apply := runOf(post(t, "/workspaces/"+ws+"/runs/"+round2.String()+"/adopt", nil))
	runs = append(runs, apply)
	await(t, apply, storage.NeedsConfirmation)
	post(t, "/workspaces/"+ws+"/runs/"+apply.String()+"/confirm", nil)
	await(t, apply, storage.Applied)
	w, _ := store.GetWorkspace(ctx, ws)
	mods, _ := store.WorkspaceTemplates(ctx, w.ID)
	if res, _ := store.Resources(ctx, w.ID); len(mods) != 1 || mods[0].Import || mods[0].Inputs["location"] != "northeurope" || len(res) != 1 {
		t.Fatalf("after the import: templates %+v, resources %+v", mods, res)
	}

	// A change made outside Opsolete is drift.
	outsideTF(t, "changed-outside", "apply", "-auto-approve", "-input=false", "-refresh=false")
	drift := runOf(post(t, "/workspaces/"+ws+"/runs", url.Values{"action": {"assess"}}))
	runs = append(runs, drift)
	await(t, drift, storage.Drifted)

	// Removed with its resources: a destroy limited to the template's module.
	remove := runOf(post(t, "/workspaces/"+ws+"/templates/"+mods[0].Name+"/remove", nil))
	runs = append(runs, remove)
	await(t, remove, storage.NeedsConfirmation)
	post(t, "/workspaces/"+ws+"/runs/"+remove.String()+"/confirm", nil)
	await(t, remove, storage.Applied)
	if res, _ := store.Resources(ctx, w.ID); len(res) != 0 {
		t.Fatalf("after removing the template: %+v", res)
	}

	// The credentials never reached a log.
	for _, id := range runs {
		l := page(t, "/workspaces/"+ws+"/runs/"+id.String()+"/logs")
		if strings.Contains(l, creds["ARM_CLIENT_SECRET"]) {
			t.Errorf("run %s's log holds the client secret", id)
		}
	}
}
