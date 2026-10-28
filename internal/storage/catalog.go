package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// TemplateCatalog is the Git repository the templates come from, and its last loaded commit.
type TemplateCatalog struct {
	ConnectionID, Repo, Branch, Dir string
	Mode                            string // off | commit | pr
	Commit                          string
	Files                           map[string]string // path -> content, at Commit
	SyncedAt                        *time.Time        // the last successful sync
	SyncError                       string
}

// GetTemplateCatalog returns sql.ErrNoRows when the templates come from TEMPLATES_DIR.
func (s *Store) GetTemplateCatalog(ctx context.Context) (TemplateCatalog, error) {
	var c TemplateCatalog
	var files []byte
	err := s.DB.QueryRowContext(ctx, `SELECT connection_id, repo, branch, dir, mode, commit_sha, files, synced_at, sync_error
		FROM template_catalog`).Scan(&c.ConnectionID, &c.Repo, &c.Branch, &c.Dir, &c.Mode, &c.Commit, &files, &c.SyncedAt, &c.SyncError)
	if err == nil {
		err = json.Unmarshal(files, &c.Files)
	}
	return c, err
}

// SetTemplateCatalog points the catalog at a repository. The next sync loads it afresh (another folder
// may have the same commit); until then the last files stay, as the fallback if it fails.
func (s *Store) SetTemplateCatalog(ctx context.Context, c TemplateCatalog) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO template_catalog (connection_id, repo, branch, dir, mode) VALUES (ops_uuid($1), $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET connection_id = EXCLUDED.connection_id, repo = EXCLUDED.repo, branch = EXCLUDED.branch,
			dir = EXCLUDED.dir, mode = EXCLUDED.mode, commit_sha = ''`, c.ConnectionID, c.Repo, c.Branch, c.Dir, c.Mode)
	return err
}

// SyncedTemplateCatalog records a sync started from prev: the files of commit, or (syncErr set) why it
// failed, keeping the last good files and synced_at (the last success). It reports false, writing nothing,
// if the row is no longer prev (another instance synced, or the catalog was reconfigured, meanwhile): that
// result is newer than this one.
func (s *Store) SyncedTemplateCatalog(ctx context.Context, prev TemplateCatalog, commit string, files map[string]string, syncErr string) (bool, error) {
	args := []any{prev.Commit, prev.ConnectionID, prev.Repo, prev.Branch, prev.Dir, syncErr}
	set := `sync_error = $6`
	if syncErr == "" {
		b, err := json.Marshal(files)
		if err != nil {
			return false, err
		}
		args = append(args, commit, b)
		set = `sync_error = $6, commit_sha = $7, files = $8, synced_at = now()`
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE template_catalog SET `+set+` WHERE `+catalogIs, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// catalogIs matches the row a sync started from: $1 commit, $2-$5 connection, repo, branch, dir.
const catalogIs = `commit_sha = $1 AND connection_id = ops_uuid($2) AND repo = $3 AND branch = $4 AND dir = $5`

// CheckedTemplateCatalog records a sync from prev that found the branch still at its commit: clears any
// earlier error. A no-op if the catalog was synced or reconfigured meanwhile.
func (s *Store) CheckedTemplateCatalog(ctx context.Context, prev TemplateCatalog) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE template_catalog SET sync_error = '', synced_at = now() WHERE `+catalogIs,
		prev.Commit, prev.ConnectionID, prev.Repo, prev.Branch, prev.Dir)
	return err
}

// DeleteTemplateCatalog disconnects the repository: the templates come from TEMPLATES_DIR again.
func (s *Store) DeleteTemplateCatalog(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM template_catalog`)
	return err
}

// ModuleUploads reports whether admins may upload registry modules.
func (s *Store) ModuleUploads(ctx context.Context) (bool, error) {
	var on bool
	err := s.DB.QueryRowContext(ctx, `SELECT module_uploads FROM organization`).Scan(&on)
	return on, err
}

func (s *Store) SetModuleUploads(ctx context.Context, on bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE organization SET module_uploads = $1`, on)
	return err
}

// ErrInUse: the row is still referenced (a VCS connection by the template catalog).
var ErrInUse = errors.New("in use")
