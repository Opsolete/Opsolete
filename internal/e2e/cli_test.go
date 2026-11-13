package e2e

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	httpserver "github.com/opsolete/opsolete/internal/http"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/testdb"
	"github.com/opsolete/opsolete/internal/vcs"
)

// TestTerraformCLI drives the real Terraform CLI through a `cloud` block against Opsolete: init creates the
// workspace (local execution), apply keeps its state in Opsolete under the CLI's lock, plan sees changes,
// state list and output read it back. A user who may only read the workspace reads its outputs.
// Offline: terraform_data, no provider download.
func TestTerraformCLI(t *testing.T) {
	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform is not on PATH")
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
	keys, _ := secrets.New("e2e-secret-key", "")
	tpls, err := templates.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("https://opsolete.test")
	log := slog.New(slog.DiscardHandler)
	a := &app.App{Store: store, Blobs: memblob.OpenBucket(nil), Secrets: keys, Templates: tpls, Log: log,
		Registry: &registry.Registry{PublicURL: public, Key: []byte("e2e-registry-key")}, VCS: vcs.NewClient(nil)}
	const admin = "cli-admin@example.com"
	au, err := auth.New(ctx, auth.Config{DevUser: admin, Admins: []string{admin}, PublicURL: public}, []byte("k"), store)
	if err != nil {
		t.Fatal(err)
	}
	srv := httpserver.New(a, au, httpserver.Features{Deployments: true, Registry: true})
	srv.Organization = "acme"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	token := func(email, role string, groups ...string) string { // role: as recorded at the user's last login
		id, err := store.UpsertUser(ctx, "", email, role, groups)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := store.CreateUserToken(ctx, id, "cli", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	// terraform runs in dir as the user of tok; the CLI config points the cloud host at the test server
	// (service discovery needs HTTPS: the host block skips it).
	terraform := func(t *testing.T, dir, tok string, args ...string) string {
		t.Helper()
		rc := filepath.Join(dir, ".terraformrc")
		os.WriteFile(rc, []byte(`host "opsolete.test" {
  services = { "tfe.v2" = "`+ts.URL+`/api/v2/" }
}
credentials "opsolete.test" {
  token = "`+tok+`"
}
`), 0o600)
		cmd := exec.Command(tf, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+rc, "TF_IN_AUTOMATION=1", "HOME="+dir)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError // with -detailed-exitcode, 2 is "changes present"
		if err != nil && !(slices.Contains(args, "-detailed-exitcode") && errors.As(err, &exit) && exit.ExitCode() == 2) {
			t.Fatalf("terraform %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	config := func(input string) string {
		return `terraform {
  cloud {
    hostname     = "opsolete.test"
    organization = "acme"
    workspaces { name = "cli-ws" }
  }
}
resource "terraform_data" "x" { input = "` + input + `" }
output "o" { value = terraform_data.x.output }
`
	}

	adminTok := token(admin, "admin")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(config("a")), 0o600)
	terraform(t, dir, adminTok, "init", "-input=false")
	terraform(t, dir, adminTok, "apply", "-auto-approve", "-input=false")
	if out := terraform(t, dir, adminTok, "output", "-raw", "o"); out != "a" {
		t.Fatalf("output: %q", out)
	}
	if out := terraform(t, dir, adminTok, "state", "list"); strings.TrimSpace(out) != "terraform_data.x" {
		t.Fatalf("state list: %q", out)
	}
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(config("b")), 0o600)
	if out := terraform(t, dir, adminTok, "plan", "-input=false", "-detailed-exitcode"); !strings.Contains(out, "1 to change") && !strings.Contains(out, "1 to add") {
		t.Fatalf("plan after a change:\n%s", out)
	}
	terraform(t, dir, adminTok, "apply", "-auto-approve", "-input=false")

	w, err := store.GetWorkspace(ctx, "cli-ws")
	if err != nil || w.ExecutionMode != "local" {
		t.Fatalf("the workspace init created: %+v %v", w, err)
	}
	if vs, _ := store.StateVersions(ctx, w.ID, 10); len(vs) < 2 {
		t.Fatalf("state versions: %d", len(vs))
	}
	if lock, _ := store.StateLockID(ctx, w.ID); lock != "" {
		t.Fatalf("the CLI's lock outlived its apply: %q", lock)
	}

	// A reader can't download the state (it holds secrets: operators only), so the CLI's init refuses; the outputs
	// it reads as the tfe provider's tfe_outputs does: values and types from Opsolete.
	if err := a.SetGrant(ctx, app.Actor{Email: admin, Admin: true}, "cli-ws", "cli-readers", "read"); err != nil {
		t.Fatal(err)
	}
	readerTok := token("cli-reader@example.com", "user", "cli-readers")
	req := httptest.NewRequest("GET", "/api/v2/workspaces/"+w.ID.String()+"/current-state-version-outputs", nil)
	req.Header.Set("Authorization", "Bearer "+readerTok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"value":"b"`) {
		t.Fatalf("a reader's outputs: %d %s", rec.Code, rec.Body.String())
	}
}
