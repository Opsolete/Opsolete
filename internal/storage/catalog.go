package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// TemplateCatalog is the Git repository the templates come from, and its last loaded commit.
type TemplateCatalog struct {
	ConnectionID            uuid.UUID
	Repo, Branch, Dir, Mode string // mode: off | commit | pr
	Commit                  string
	Files                   map[string]string // path -> content, at Commit
	SyncedAt                *time.Time        // the last successful sync
	SyncError               string
}

// GetTemplateCatalog returns ErrNotFound when the templates come from TEMPLATES_DIR.
func (s *Store) GetTemplateCatalog(ctx context.Context) (TemplateCatalog, error) {
	r, err := s.q.GetTemplateCatalog(ctx)
	if err != nil {
		return TemplateCatalog{}, notFound(err)
	}
	c := TemplateCatalog{ConnectionID: r.ConnectionID, Repo: r.Repo, Branch: r.Branch, Dir: r.Dir, Mode: r.Mode, Commit: r.CommitSha,
		SyncedAt: r.SyncedAt, SyncError: r.SyncError}
	return c, json.Unmarshal(r.Files, &c.Files)
}

// TemplateCatalogVersion returns the editor mode and commit of the stored catalog; ErrNotFound like GetTemplateCatalog.
func (s *Store) TemplateCatalogVersion(ctx context.Context) (mode, commit string, err error) {
	r, err := s.q.TemplateCatalogVersion(ctx)
	return r.Mode, r.CommitSha, notFound(err)
}

// SetTemplateCatalog points the catalog at a repository. The next sync loads it afresh (another folder
// may have the same commit); until then the last files stay, as the fallback if it fails.
func (s *Store) SetTemplateCatalog(ctx context.Context, c TemplateCatalog) error {
	return s.q.SetTemplateCatalog(ctx, db.SetTemplateCatalogParams{ConnectionID: c.ConnectionID, Repo: c.Repo, Branch: c.Branch, Dir: c.Dir, Mode: c.Mode})
}

// SyncedTemplateCatalog records a sync started from prev: the files of commit, or (syncErr set) why it
// failed, keeping the last good files and synced_at (the last success). It reports false, writing nothing,
// if the row is no longer prev (another instance synced, or the catalog was reconfigured, meanwhile): that
// result is newer than this one.
func (s *Store) SyncedTemplateCatalog(ctx context.Context, prev TemplateCatalog, commit string, files map[string]string, syncErr string) (bool, error) {
	if syncErr != "" {
		n, err := s.q.CatalogSyncFailed(ctx, db.CatalogSyncFailedParams{SyncError: syncErr, PrevCommit: prev.Commit,
			ConnectionID: prev.ConnectionID, Repo: prev.Repo, Branch: prev.Branch, Dir: prev.Dir})
		return n == 1, err
	}
	b, err := json.Marshal(files)
	if err != nil {
		return false, err
	}
	n, err := s.q.CatalogSynced(ctx, db.CatalogSyncedParams{Commit: commit, Files: b, PrevCommit: prev.Commit,
		ConnectionID: prev.ConnectionID, Repo: prev.Repo, Branch: prev.Branch, Dir: prev.Dir})
	return n == 1, err
}

// CheckedTemplateCatalog records a sync from prev that found the branch still at its commit: clears any
// earlier error. It reports false, writing nothing, if the catalog was synced or reconfigured meanwhile.
func (s *Store) CheckedTemplateCatalog(ctx context.Context, prev TemplateCatalog) (bool, error) {
	n, err := s.q.CatalogChecked(ctx, db.CatalogCheckedParams{PrevCommit: prev.Commit, ConnectionID: prev.ConnectionID,
		Repo: prev.Repo, Branch: prev.Branch, Dir: prev.Dir})
	return n == 1, err
}

// DeleteTemplateCatalog disconnects the repository: the templates come from TEMPLATES_DIR again.
func (s *Store) DeleteTemplateCatalog(ctx context.Context) error {
	return s.q.DeleteTemplateCatalog(ctx)
}

// ErrInUse: the row is still referenced (a VCS connection by the template catalog).
var ErrInUse = errors.New("in use")
