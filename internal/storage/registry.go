package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage/db"
)

var ErrExists = errors.New("already exists")

type Module struct {
	Namespace, Name, System, Version string
	Variables                        []registry.Variable
	UploadedAt                       time.Time
}

// ModuleDocs is the generated documentation of one module version.
type ModuleDocs struct {
	Module *tfconfig.Module // every upload and VCS publish stores it; nil only if the column is empty
	Readme string
}

// CreateModule returns ErrExists if the version exists.
func (s *Store) CreateModule(ctx context.Context, m Module, docs ModuleDocs, archive []byte) error {
	vars, err := json.Marshal(m.Variables)
	if err != nil {
		return err
	}
	mod, err := json.Marshal(docs.Module)
	if err != nil {
		return err
	}
	err = s.q.CreateModule(ctx, db.CreateModuleParams{Namespace: m.Namespace, Name: m.Name, System: m.System, Version: m.Version,
		Archive: archive, Variables: vars, Docs: mod, Readme: docs.Readme})
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func toModule(namespace, name, system, version string, variables []byte, uploaded time.Time) (Module, error) {
	m := Module{Namespace: namespace, Name: name, System: system, Version: version, UploadedAt: uploaded}
	return m, json.Unmarshal(variables, &m.Variables)
}

// ListModules returns all module versions (without archives) by module, newest upload first.
func (s *Store) ListModules(ctx context.Context) ([]Module, error) {
	rows, err := s.q.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Module, 0, len(rows))
	for _, r := range rows {
		m, err := toModule(r.Namespace, r.Name, r.System, r.Version, r.Variables, r.UploadedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// ModuleVersions returns the versions of one module; empty if it doesn't exist.
func (s *Store) ModuleVersions(ctx context.Context, namespace, name, system string) ([]string, error) {
	return s.q.ModuleVersions(ctx, db.ModuleVersionsParams{Namespace: namespace, Name: name, System: system})
}

// GetModule returns ErrNotFound if that version doesn't exist.
func (s *Store) GetModule(ctx context.Context, namespace, name, system, version string) (Module, error) {
	r, err := s.q.GetModule(ctx, db.GetModuleParams{Namespace: namespace, Name: name, System: system, Version: version})
	if err != nil {
		return Module{}, notFound(err)
	}
	return toModule(r.Namespace, r.Name, r.System, r.Version, r.Variables, r.UploadedAt)
}

// GetModuleDocs returns ErrNotFound if that version doesn't exist.
func (s *Store) GetModuleDocs(ctx context.Context, namespace, name, system, version string) (ModuleDocs, error) {
	r, err := s.q.GetModuleDocs(ctx, db.GetModuleDocsParams{Namespace: namespace, Name: name, System: system, Version: version})
	d := ModuleDocs{Readme: r.Readme}
	if err != nil || r.Docs == nil {
		return d, notFound(err)
	}
	d.Module, err = decodeModule(r.Docs)
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

// ModuleArchive returns ErrNotFound if that version doesn't exist.
func (s *Store) ModuleArchive(ctx context.Context, namespace, name, system, version string) ([]byte, error) {
	b, err := s.q.ModuleArchive(ctx, db.ModuleArchiveParams{Namespace: namespace, Name: name, System: system, Version: version})
	return b, notFound(err)
}

// DeleteModule removes every version of a module and its VCS link (else the next sync republishes
// it). Returns ErrNotFound if the module has no versions.
func (s *Store) DeleteModule(ctx context.Context, namespace, name, system string) error {
	return s.tx(ctx, func(q *db.Queries) error {
		if err := affected(q.DeleteModuleVersions(ctx, db.DeleteModuleVersionsParams{Namespace: namespace, Name: name, System: system})); err != nil {
			return err
		}
		_, err := q.UnlinkVCSModule(ctx, db.UnlinkVCSModuleParams{Namespace: namespace, Name: name, System: system})
		return err
	})
}

// Token scopes: registry tokens are for Terraform clients, user tokens (terraform login) act as a
// user and also work for the registry. Runners use their own tokens (runners table) instead. The values are
// api_tokens.scope's (001_schema.sql).
const (
	ScopeRegistry = "registry"
	ScopeUser     = "user"
)

// newToken returns a new API token and its hash, the only form stored.
func newToken() (string, []byte) {
	token := "ops_" + rand.Text()
	return token, tokenHash(token)
}

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CreateToken stores a new API token and returns it; it can't be retrieved again.
func (s *Store) CreateToken(ctx context.Context, name, scope string) (string, error) {
	token, h := newToken()
	return token, s.q.CreateToken(ctx, db.CreateTokenParams{Name: name, TokenHash: h, Scope: scope})
}

// ValidToken reports whether token exists with the given scope; an unexpired user token also
// passes as a registry token, since `terraform login` stores one token per host.
func (s *Store) ValidToken(ctx context.Context, token, scope string) (bool, error) {
	return s.q.ValidToken(ctx, db.ValidTokenParams{TokenHash: tokenHash(token), Scope: scope})
}

// CreateUserToken stores a new token that acts as the user until it expires, and returns it.
func (s *Store) CreateUserToken(ctx context.Context, userID uuid.UUID, name string, ttl time.Duration) (string, error) {
	token, h := newToken()
	return token, s.q.CreateUserToken(ctx, db.CreateUserTokenParams{Name: name, TokenHash: h, UserID: userID, TtlSecs: float64(int64(ttl.Seconds()))})
}

// TokenUser returns the id and email of an unexpired user token's user; ErrNotFound if
// the token is unknown, expired or not a user token.
// role and groups are what the user's last login recorded.
func (s *Store) TokenUser(ctx context.Context, token string) (id uuid.UUID, email, role string, groups []string, err error) {
	u, err := s.q.TokenUser(ctx, tokenHash(token))
	return u.ID, u.Email, u.Role, u.Groups, notFound(err)
}

// UserToken is one of a user's unexpired `terraform login` tokens; the token itself is only stored hashed.
type UserToken struct {
	ID                   uuid.UUID
	Name                 string
	CreatedAt, ExpiresAt time.Time
}

// UserTokens lists a user's unexpired tokens, newest first.
func (s *Store) UserTokens(ctx context.Context, userID uuid.UUID) ([]UserToken, error) {
	rows, err := s.q.UserTokens(ctx, userID)
	out := make([]UserToken, 0, len(rows))
	for _, t := range rows {
		out = append(out, UserToken{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt})
	}
	return out, err
}

// RevokeUserToken deletes one of the user's tokens; other users' token ids are ignored.
func (s *Store) RevokeUserToken(ctx context.Context, userID, id uuid.UUID) error {
	return s.q.RevokeUserToken(ctx, db.RevokeUserTokenParams{UserID: userID, ID: id})
}

// RevokeUserTokens deletes all of the user's tokens.
func (s *Store) RevokeUserTokens(ctx context.Context, userID uuid.UUID) error {
	return s.q.RevokeUserTokens(ctx, userID)
}
