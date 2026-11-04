package httpserver

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	tfe "github.com/hashicorp/go-tfe"
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
	client := func(email string) *tfe.Client {
		t.Helper()
		id, err := store.UpsertUser(ctx, "", email, "user", nil)
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
	alice, other := client("tfe-alice-"+suffix+"@example.com"), client(admin)

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
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM workspaces WHERE id::text = $1`, ws.ID) })
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

	// A run's lock (here a rollback's) is reported as a run's.
	if _, ok, err := store.LockState(ctx, ws.ID, "run-lock", "", []byte(`{"ID":"run-lock"}`)); !ok || err != nil {
		t.Fatalf("take a non-CLI lock: %v %v", ok, err)
	}
	if _, err := alice.Workspaces.Unlock(ctx, ws.ID); !errors.Is(err, tfe.ErrWorkspaceLockedByRun) {
		t.Fatalf("unlock of a run's lock: %v", err)
	}
}
