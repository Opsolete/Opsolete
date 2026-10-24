package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrExists = errors.New("already exists")

type Module struct {
	Namespace, Name, System, Version string
	Variables                        []tfconfig.Variable
	UploadedAt                       time.Time
}

// ModuleDocs is the generated documentation of one module version.
type ModuleDocs struct {
	Module *tfconfig.Module // nil for versions uploaded before docs existed
	Readme string
}

func (s *Store) CreateModule(ctx context.Context, m Module, docs ModuleDocs, archive []byte) error {
	vars, err := json.Marshal(m.Variables)
	if err != nil {
		return err
	}
	mod, err := json.Marshal(docs.Module)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO modules (namespace, name, system, version, archive, variables, docs, readme)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, m.Namespace, m.Name, m.System, m.Version, archive, vars, mod, docs.Readme)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrExists
	}
	return err
}

// ListModules returns all module versions (without archives), newest upload first.
func (s *Store) ListModules(ctx context.Context) ([]Module, error) {
	return s.queryModules(ctx, `SELECT namespace, name, system, version, variables, uploaded_at
		FROM modules ORDER BY namespace, name, system, uploaded_at DESC`)
}

// ModuleVersions returns the versions of one module; empty if it doesn't exist.
func (s *Store) ModuleVersions(ctx context.Context, namespace, name, system string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT version FROM modules WHERE namespace=$1 AND name=$2 AND system=$3`,
		namespace, name, system)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetModule returns sql.ErrNoRows if that version doesn't exist.
func (s *Store) GetModule(ctx context.Context, namespace, name, system, version string) (Module, error) {
	ms, err := s.queryModules(ctx, `SELECT namespace, name, system, version, variables, uploaded_at
		FROM modules WHERE namespace=$1 AND name=$2 AND system=$3 AND version=$4`, namespace, name, system, version)
	if err != nil {
		return Module{}, err
	}
	if len(ms) == 0 {
		return Module{}, sql.ErrNoRows
	}
	return ms[0], nil
}

func (s *Store) queryModules(ctx context.Context, q string, args ...any) ([]Module, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Module
	for rows.Next() {
		var m Module
		var raw []byte
		if err := rows.Scan(&m.Namespace, &m.Name, &m.System, &m.Version, &raw, &m.UploadedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &m.Variables); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModuleDocs returns sql.ErrNoRows if that version doesn't exist.
func (s *Store) GetModuleDocs(ctx context.Context, namespace, name, system, version string) (ModuleDocs, error) {
	var d ModuleDocs
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `SELECT docs, readme FROM modules WHERE namespace=$1 AND name=$2 AND system=$3 AND version=$4`,
		namespace, name, system, version).Scan(&raw, &d.Readme)
	if err != nil || raw == nil {
		return d, err
	}
	d.Module, err = decodeModule(raw)
	return d, err
}

// decodeModule reverses json.Marshal(*tfconfig.Module). tfconfig.ResourceMode marshals to a
// string but has no UnmarshalJSON, so resources are decoded via a shadow type.
func decodeModule(raw []byte) (*tfconfig.Module, error) {
	type resource struct {
		Type     string               `json:"type"`
		Name     string               `json:"name"`
		Provider tfconfig.ProviderRef `json:"provider"`
		Pos      tfconfig.SourcePos   `json:"pos"`
	}
	var m struct {
		*tfconfig.Module
		ManagedResources map[string]*resource `json:"managed_resources"`
		DataResources    map[string]*resource `json:"data_resources"`
	}
	m.Module = &tfconfig.Module{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	conv := func(in map[string]*resource, mode tfconfig.ResourceMode) map[string]*tfconfig.Resource {
		out := make(map[string]*tfconfig.Resource, len(in))
		for k, r := range in {
			out[k] = &tfconfig.Resource{Mode: mode, Type: r.Type, Name: r.Name, Provider: r.Provider, Pos: r.Pos}
		}
		return out
	}
	m.Module.ManagedResources = conv(m.ManagedResources, tfconfig.ManagedResourceMode)
	m.Module.DataResources = conv(m.DataResources, tfconfig.DataResourceMode)
	return m.Module, nil
}

// ModuleArchive returns sql.ErrNoRows if that version doesn't exist.
func (s *Store) ModuleArchive(ctx context.Context, namespace, name, system, version string) ([]byte, error) {
	var b []byte
	err := s.DB.QueryRowContext(ctx, `SELECT archive FROM modules WHERE namespace=$1 AND name=$2 AND system=$3 AND version=$4`,
		namespace, name, system, version).Scan(&b)
	return b, err
}

// Token scopes: registry tokens are for Terraform clients, runner tokens for execution runners,
// user tokens (terraform login) act as a user and also work for the registry.
const (
	ScopeRegistry = "registry"
	ScopeRunner   = "runner"
	ScopeUser     = "user"
)

// CreateToken stores a new API token and returns it; it can't be retrieved again.
func (s *Store) CreateToken(ctx context.Context, name, scope string) (string, error) {
	token := "ops_" + rand.Text()
	h := sha256.Sum256([]byte(token))
	_, err := s.DB.ExecContext(ctx, `INSERT INTO api_tokens (name, token_hash, scope) VALUES ($1, $2, $3)`, name, h[:], scope)
	return token, err
}

// ValidToken reports whether token exists with the given scope; an unexpired user token also
// passes as a registry token, since `terraform login` stores one token per host.
func (s *Store) ValidToken(ctx context.Context, token, scope string) (bool, error) {
	h := sha256.Sum256([]byte(token))
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM api_tokens WHERE token_hash=$1
		AND (scope=$2 OR ($2 = 'registry' AND scope = 'user' AND expires_at > now())))`, h[:], scope).Scan(&ok)
	return ok, err
}

// CreateUserToken stores a new token that acts as the user until it expires, and returns it.
func (s *Store) CreateUserToken(ctx context.Context, userID, name string, ttl time.Duration) (string, error) {
	token := "ops_" + rand.Text()
	h := sha256.Sum256([]byte(token))
	_, err := s.DB.ExecContext(ctx, `INSERT INTO api_tokens (name, token_hash, scope, user_id, expires_at)
		VALUES ($1, $2, 'user', $3::uuid, now() + $4 * interval '1 second')`, name, h[:], userID, int64(ttl.Seconds()))
	return token, err
}

// TokenUser returns the id, email and role of an unexpired user token's user; sql.ErrNoRows if
// the token is unknown, expired or not a user token.
func (s *Store) TokenUser(ctx context.Context, token string) (id, email, role string, err error) {
	h := sha256.Sum256([]byte(token))
	err = s.DB.QueryRowContext(ctx, `SELECT u.id, u.email, u.role FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND t.scope = 'user' AND t.expires_at > now()`, h[:]).Scan(&id, &email, &role)
	return id, email, role, err
}

// SigningKey returns the registry's HMAC key, creating it on first use.
func (s *Store) SigningKey(ctx context.Context) ([]byte, error) {
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO registry_signing_key (id, key) VALUES (1, $1) ON CONFLICT DO NOTHING`,
		[]byte(rand.Text()+rand.Text())); err != nil {
		return nil, err
	}
	var k []byte
	err := s.DB.QueryRowContext(ctx, `SELECT key FROM registry_signing_key WHERE id=1`).Scan(&k)
	return k, err
}
