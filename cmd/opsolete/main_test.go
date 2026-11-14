package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"gocloud.dev/blob"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/testdb"
)

// testEnv points the commands at a new test database, with the rest of the configuration at its
// defaults (not the dev shell's), and returns a store on it.
func testEnv(t *testing.T, secretKey string) *storage.Store {
	t.Helper()
	u := testdb.URL(t)
	t.Setenv("DATABASE_URL", u)
	t.Setenv("SECRET_KEY", secretKey)
	for _, k := range []string{"SECRET_KEY_PREVIOUS", "STATE_STORE", "TEMPLATE_EDITOR", "TEMPLATES_DIR", "DEPLOYMENTS_ENABLED",
		"REGISTRY_ENABLED", "PUBLIC_URL", "STALE_LOCK_TIMEOUT", "RUN_LOG_RETENTION_DAYS"} {
		t.Setenv(k, "")
	}
	store, err := storage.Open(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

// audited is the detail of the last audit entry of action, which actor "cli" must have written.
func audited(t *testing.T, store *storage.Store, action string) string {
	t.Helper()
	var actor, detail string
	if err := store.Pool.QueryRow(context.Background(), `SELECT actor, detail::text FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`,
		action).Scan(&actor, &detail); err != nil || actor != "cli" {
		t.Fatalf("audit entry %q: %q %v", action, actor, err)
	}
	return detail
}

// TestRotateKeyCLI (P2): `opsolete rotate-key` needs both keys and a usable STATE_STORE, re-seals
// variables and state store files from SECRET_KEY_PREVIOUS to SECRET_KEY, and audits each rotation.
func TestRotateKeyCLI(t *testing.T) {
	oldKey, newKey := strings.Repeat("o", 32), strings.Repeat("n", 32)
	store := testEnv(t, newKey)
	ctx, log := context.Background(), slog.New(slog.DiscardHandler)
	old, _ := secrets.New(oldKey, "")
	if err := store.CreateVariableSet(ctx, "rk", "", false); err != nil {
		t.Fatal(err)
	}
	set, _ := store.GetVariableSet(ctx, "rk")
	o := storage.VarOwner{SetID: set.ID}
	v := storage.Variable{Owner: o, Category: "env", Key: "SECRET", Sensitive: true}
	v.Value, _ = old.Seal("s3cret", app.VariableAD(v))
	if err := store.SetVariable(ctx, o, v); err != nil {
		t.Fatal(err)
	}
	dir := "file://" + t.TempDir() + "?metadata=skip"
	bucket, err := blob.OpenBucket(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer bucket.Close()
	if err := (&app.App{Blobs: bucket, Secrets: old}).WriteBlob(ctx, "opsolete/x/1.tfstate", []byte("state")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ previous, store, want string }{
		{"", "", "set SECRET_KEY to the new key and SECRET_KEY_PREVIOUS to the old one"},
		{newKey, "", "SECRET_KEY_PREVIOUS, when set, must be at least 32 characters and differ from SECRET_KEY"},
		{"short", "", "SECRET_KEY_PREVIOUS, when set, must be at least 32 characters"},
		{oldKey, "nope://x", "STATE_STORE:"},
	} {
		t.Setenv("SECRET_KEY_PREVIOUS", c.previous)
		t.Setenv("STATE_STORE", c.store)
		if err := rotateKey(log); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("rotate-key with SECRET_KEY_PREVIOUS=%q STATE_STORE=%q: %v, want %q", c.previous, c.store, err, c.want)
		}
	}
	var n int
	store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&n)
	if n != 0 {
		t.Fatalf("a refused rotation was audited: %d entries", n)
	}

	t.Setenv("SECRET_KEY_PREVIOUS", oldKey)
	t.Setenv("STATE_STORE", dir)
	if err := rotateKey(log); err != nil {
		t.Fatal(err)
	}
	if d := audited(t, store, "rotate secret key"); !strings.Contains(d, `"resealed":2`) {
		t.Fatalf("audit entry: %s", d)
	}
	now, _ := secrets.New(newKey, "")
	vars, _ := store.Variables(ctx, o)
	if got, err := now.Open(vars[0].Value, app.VariableAD(vars[0])); err != nil || got != "s3cret" {
		t.Fatalf("variable after the rotation: %q %v", got, err)
	}
	if b, err := (&app.App{Blobs: bucket, Secrets: now}).ReadBlob(ctx, "opsolete/x/1.tfstate"); err != nil || string(b) != "state" {
		t.Fatalf("state file after the rotation: %q %v", b, err)
	}
	if err := rotateKey(log); err != nil {
		t.Fatal(err)
	}
	if d := audited(t, store, "rotate secret key"); !strings.Contains(d, `"resealed":0`) {
		t.Fatalf("second rotation: %s", d)
	}
}

// TestCreateToken (P3): `opsolete token <name>` prints a working registry token, and only it, on
// stdout and audits its creation without it.
func TestCreateToken(t *testing.T) {
	store := testEnv(t, strings.Repeat("k", 32))
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"opsolete", "token", "ci-pipeline"}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	err = createToken()
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	token, rest, _ := strings.Cut(string(out), "\n")
	if token == "" || rest != "" {
		t.Fatalf("stdout must be the token alone: %q", out)
	}
	if ok, err := store.ValidToken(context.Background(), token, storage.ScopeRegistry); err != nil || !ok {
		t.Fatalf("printed token: %v %v", ok, err)
	}
	if d := audited(t, store, "create token"); !strings.Contains(d, "ci-pipeline") || strings.Contains(d, token) {
		t.Fatalf("audit entry: %s", d)
	}
}

// TestRunnerTokenCLI: `opsolete runner-token` prints a new runner's token alone; -from-stdin registers the one on
// stdin (refused when short) and replaces that runner's token and pool.
func TestRunnerTokenCLI(t *testing.T) {
	store := testEnv(t, strings.Repeat("k", 32))
	ctx := context.Background()
	run := func(stdin string, args ...string) (string, error) {
		t.Helper()
		in, inW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		inW.WriteString(stdin)
		inW.Close()
		outR, outW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdinWas, stdoutWas := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = in, outW
		err = runnerToken(args)
		os.Stdin, os.Stdout = stdinWas, stdoutWas
		outW.Close()
		out, _ := io.ReadAll(outR)
		return string(out), err
	}

	out, err := run("", "-pool", "prd", "ci-1")
	token, rest, _ := strings.Cut(out, "\n")
	if err != nil || token == "" || rest != "" {
		t.Fatalf("stdout must be the token alone: %q %v", out, err)
	}
	if r, err := store.RunnerByToken(ctx, token); err != nil || r.Name != "ci-1" || r.Pool != "prd" {
		t.Fatalf("printed token: %+v %v", r, err)
	}
	if d := audited(t, store, "create runner"); !strings.Contains(d, "ci-1") || strings.Contains(d, token) {
		t.Fatalf("audit entry: %s", d)
	}
	if _, err := run("", "ci-1"); err == nil {
		t.Fatal("the same name again")
	}

	if _, err := run("short\n", "-from-stdin", "ci-1"); err == nil {
		t.Fatal("a short token registered")
	}
	own := strings.Repeat("t", 40)
	if out, err := run(own+"\n", "-from-stdin", "ci-1"); err != nil || out != "" {
		t.Fatalf("register: %q %v", out, err)
	}
	if r, err := store.RunnerByToken(ctx, own); err != nil || r.Name != "ci-1" || r.Pool != storage.DefaultPool {
		t.Fatalf("registered token: %+v %v", r, err)
	}
	fromEnv := strings.Repeat("e", 40)
	t.Setenv("CHART_RUNNER_TOKEN", fromEnv)
	if out, err := run("", "-from-env", "CHART_RUNNER_TOKEN", "-pool", "k8s", "ci-1"); err != nil || out != "" {
		t.Fatalf("register from env: %q %v", out, err)
	}
	if r, err := store.RunnerByToken(ctx, fromEnv); err != nil || r.Name != "ci-1" || r.Pool != "k8s" {
		t.Fatalf("token from env: %+v %v", r, err)
	}
	if _, err := run("", "-from-env", "NOT_SET_ANYWHERE", "ci-1"); err == nil {
		t.Fatal("an empty variable registered")
	}
	if _, err := run("", "-from-env", "CHART_RUNNER_TOKEN", "-from-stdin", "ci-1"); err == nil {
		t.Fatal("both sources")
	}
	if _, err := store.RunnerByToken(ctx, token); err == nil {
		t.Fatal("the replaced token still works")
	}
}

// TestAPITokenCLI: `opsolete api-token` prints a new service token alone; -from-env registers one generated
// elsewhere (refused when short), replacing that token's old value.
func TestAPITokenCLI(t *testing.T) {
	store := testEnv(t, strings.Repeat("k", 32))
	ctx := context.Background()
	run := func(args ...string) (string, error) {
		t.Helper()
		outR, outW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdoutWas := os.Stdout
		os.Stdout = outW
		err = apiToken(args)
		os.Stdout = stdoutWas
		outW.Close()
		out, _ := io.ReadAll(outR)
		return string(out), err
	}
	name := "tf-" + strings.ToLower(uuid.NewString()[:8])
	out, err := run(name)
	token, rest, _ := strings.Cut(out, "\n")
	if err != nil || token == "" || rest != "" {
		t.Fatalf("stdout must be the token alone: %q %v", out, err)
	}
	if n, err := store.ServiceTokenName(ctx, token); err != nil || n != name {
		t.Fatalf("printed token: %q %v", n, err)
	}
	if d := audited(t, store, "create service token"); !strings.Contains(d, name) || strings.Contains(d, token) {
		t.Fatalf("audit entry: %s", d)
	}
	if _, err := run(name); err == nil {
		t.Fatal("the same name again")
	}
	t.Setenv("OPS_TF_TOKEN", "short")
	if _, err := run("-from-env", "OPS_TF_TOKEN", name); err == nil {
		t.Fatal("a short token registered")
	}
	own := strings.Repeat("s", 40)
	t.Setenv("OPS_TF_TOKEN", own)
	if out, err := run("-from-env", "OPS_TF_TOKEN", name); err != nil || out != "" {
		t.Fatalf("register: %q %v", out, err)
	}
	if n, err := store.ServiceTokenName(ctx, own); err != nil || n != name {
		t.Fatalf("registered token: %q %v", n, err)
	}
	if _, err := store.ServiceTokenName(ctx, token); err == nil {
		t.Fatal("the replaced token still works")
	}
}
