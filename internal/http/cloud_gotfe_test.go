package httpserver

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tfe "github.com/hashicorp/go-tfe"

	"github.com/google/uuid"
)

// TestCloudGoTFE drives the cloud API with go-tfe, the client inside the Terraform CLI, at the
// version Terraform 1.14 ships (go.mod pins it). Older go-tfe reads only JSON:API error objects, and
// Terraform's fallbacks and lock messages depend on the errors it maps them to.
func TestCloudGoTFE(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := newID()[:8]
	admin := "tfe-admin-" + suffix + "@example.com"
	app := newApp(t, "../../catalog", store, devAuth(t, store, admin, true), Features{Deployments: true, Registry: true})
	app.Organization = "acme"
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	client := func(email, role string) *tfe.Client { // role: as recorded at the user's last login
		t.Helper()
		id, err := store.UpsertUser(ctx, "", email, role, nil)
		if err != nil {
			t.Fatal(err)
		}
		token, err := store.CreateUserToken(ctx, id, "go-tfe test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		c, err := tfe.NewClient(&tfe.Config{Address: srv.URL, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	alice, other := client("tfe-alice-"+suffix+"@example.com", "user"), client(admin, "admin")

	if _, err := alice.Organizations.ReadEntitlements(ctx, "acme"); err != nil {
		t.Fatalf("entitlements: %v", err)
	}
	name := "tfe-" + suffix
	if _, err := alice.Workspaces.Read(ctx, "acme", name); !errors.Is(err, tfe.ErrResourceNotFound) {
		t.Fatalf("missing workspace: %v", err)
	}
	ws, err := alice.Workspaces.Create(ctx, "acme", tfe.WorkspaceCreateOptions{Name: tfe.String(name)})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { store.Pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id::text = $1`, ws.ID) })
	if _, err := alice.Workspaces.UpdateByID(ctx, ws.ID, tfe.WorkspaceUpdateOptions{TerraformVersion: tfe.String("1.14.0")}); err != nil {
		t.Fatalf("update workspace: %v", err)
	}

	if _, err := alice.Workspaces.Lock(ctx, ws.ID, tfe.WorkspaceLockOptions{}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := alice.Workspaces.Lock(ctx, ws.ID, tfe.WorkspaceLockOptions{}); !errors.Is(err, tfe.ErrWorkspaceLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if _, err := other.Workspaces.Unlock(ctx, ws.ID); !errors.Is(err, tfe.ErrWorkspaceLockedByUser) {
		t.Fatalf("unlock of another user's lock: %v", err)
	}

	// Terraform's state upload: the upload-URL flow must fail with exactly this error, so it retries inline.
	state := []byte(`{"version":4,"serial":1,"lineage":"l-` + suffix + `","outputs":{"pw":{"value":"x","type":"string","sensitive":true}},"resources":[]}`)
	sum := md5.Sum(state)
	create := tfe.StateVersionCreateOptions{Serial: tfe.Int64(1), MD5: tfe.String(hex.EncodeToString(sum[:])), Lineage: tfe.String("l-" + suffix)}
	if _, err := alice.StateVersions.Upload(ctx, ws.ID, tfe.StateVersionUploadOptions{StateVersionCreateOptions: create, RawState: state}); !errors.Is(err, tfe.ErrStateVersionUploadNotSupported) {
		t.Fatalf("upload must fall back to inline state: %v", err)
	}
	create.State = tfe.String(base64.StdEncoding.EncodeToString(state))
	if _, err := alice.StateVersions.Create(ctx, ws.ID, create); err != nil {
		t.Fatalf("create state version: %v", err)
	}
	cur, err := alice.StateVersions.ReadCurrent(ctx, ws.ID)
	if err != nil {
		t.Fatalf("current state version: %v", err)
	}
	if got, err := alice.StateVersions.Download(ctx, cur.DownloadURL); err != nil || string(got) != string(state) {
		t.Fatalf("download: %q %v", got, err)
	}
	if outs, err := alice.StateVersionOutputs.ReadCurrent(ctx, ws.ID); err != nil || len(outs.Items) != 1 || outs.Items[0].Name != "pw" {
		t.Fatalf("outputs: %+v %v", outs, err)
	}
	if _, err := alice.Workspaces.Unlock(ctx, ws.ID); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	// cloud { workspaces { project = "x" } }: Terraform's calls in internal/cloud/backend.go (StateMgr).
	if ws.Project == nil || ws.Project.ID == "" {
		t.Fatalf("a workspace created without a project is in the default one: %+v", ws.Project)
	}
	if def, err := alice.Projects.List(ctx, "acme", &tfe.ProjectListOptions{Name: "default"}); err != nil || len(def.Items) != 1 || def.Items[0].ID != ws.Project.ID {
		t.Fatalf("default project by name: %+v %v", def, err)
	}
	pname := "tfe-prj-" + suffix
	if none, err := alice.Projects.List(ctx, "acme", &tfe.ProjectListOptions{Name: pname}); err != nil || len(none.Items) != 0 {
		t.Fatalf("missing project: %+v %v", none, err)
	}
	if _, err := alice.Projects.Create(ctx, "acme", tfe.ProjectCreateOptions{Name: pname}); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("a user creates a project: %v", err)
	}
	prj, err := other.Projects.Create(ctx, "acme", tfe.ProjectCreateOptions{Name: pname})
	if err != nil || prj.Name != pname {
		t.Fatalf("an admin creates the project: %+v %v", prj, err)
	}
	t.Cleanup(func() {
		store.Pool.Exec(context.Background(), `DELETE FROM workspaces WHERE project_id::text = $1`, prj.ID)
		store.Pool.Exec(context.Background(), `DELETE FROM projects WHERE id::text = $1`, prj.ID)
	})
	if _, err := alice.Workspaces.Create(ctx, "acme", tfe.WorkspaceCreateOptions{Name: tfe.String(name + "-p"), Project: prj}); err == nil || !strings.Contains(err.Error(), "operate") {
		t.Fatalf("a user without operate on the project creates a workspace in it: %v", err)
	}
	inPrj, err := other.Workspaces.Create(ctx, "acme", tfe.WorkspaceCreateOptions{Name: tfe.String(name + "-p"), Project: prj})
	if err != nil || inPrj.Project == nil || inPrj.Project.ID != prj.ID {
		t.Fatalf("create in the project: %+v %v", inPrj, err)
	}
	if got, err := other.Workspaces.Read(ctx, "acme", name+"-p"); err != nil || got.Project == nil || got.Project.ID != prj.ID {
		t.Fatalf("read returns the project: %+v %v", got, err)
	}

	// A run's lock (here a rollback's) is reported as a run's.
	if _, ok, err := store.LockState(ctx, uuid.MustParse(ws.ID), "run-lock", uuid.Nil, []byte(`{"ID":"run-lock"}`)); !ok || err != nil {
		t.Fatalf("take a non-CLI lock: %v %v", ok, err)
	}
	if _, err := alice.Workspaces.Unlock(ctx, ws.ID); !errors.Is(err, tfe.ErrWorkspaceLockedByRun) {
		t.Fatalf("unlock of a run's lock: %v", err)
	}
}
