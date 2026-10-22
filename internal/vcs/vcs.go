// Package vcs publishes registry modules from GitHub and Azure DevOps repositories, the way HCP
// Terraform does: every semver tag of a linked repo becomes an immutable module version. It talks
// to the providers' REST APIs with an access token, so the server needs no git binary.
package vcs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rudesome/form2stack/internal/registry"
	"github.com/rudesome/form2stack/internal/storage"
)

const (
	GitHub      = "github"
	AzureDevOps = "azure-devops"

	maxJSON      = 10 << 20
	maxDownload  = 50 << 20  // a repo archive may be larger than the module archive we keep
	maxExtracted = 200 << 20 // guards against zip/gzip bombs
)

// Providers lists the supported providers with their default API root ("" = must be given).
var Providers = []struct{ ID, Label, DefaultURL, Placeholder string }{
	{GitHub, "GitHub", "https://api.github.com", "https://api.github.com, or https://github.example.com/api/v3"},
	{AzureDevOps, "Azure DevOps", "", "https://dev.azure.com/<organization>"},
}

// HTTPClient talks to the providers; tests swap in a TLS test server's client.
var HTTPClient = &http.Client{Timeout: 2 * time.Minute}

// Conn is a usable connection: provider, API root and plain token.
type Conn struct{ Provider, BaseURL, Token string }

// NormalizeURL validates an API root (https, no query) and fills in the provider default.
func NormalizeURL(provider, raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, p := range Providers {
		if p.ID != provider {
			continue
		}
		if raw == "" {
			raw = p.DefaultURL
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return "", fmt.Errorf("API URL must be like %s", p.Placeholder)
		}
		return raw, nil
	}
	return "", fmt.Errorf("unknown VCS provider %q", provider)
}

// splitRepo checks "owner/repo" (GitHub) or "project/repo" (Azure DevOps) and path-escapes both parts.
func splitRepo(repo string) (string, string, error) {
	a, b, ok := strings.Cut(repo, "/")
	if !ok || a == "" || b == "" || strings.Contains(b, "/") || a == "." || a == ".." || b == "." || b == ".." {
		return "", "", fmt.Errorf("repository must be owner/repo (GitHub) or project/repo (Azure DevOps), got %q", repo)
	}
	return url.PathEscape(a), url.PathEscape(b), nil
}

func (c Conn) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	switch c.Provider {
	case GitHub:
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	case AzureDevOps:
		req.SetBasicAuth("", c.Token) // PAT as the password
	}
	resp, err := HTTPClient.Do(req) // Go drops Authorization on redirects to other hosts (GitHub's codeload is pre-signed)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Azure DevOps answers a bad token with 203 and a sign-in page, so anything but 200 is an error.
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("%s %s: %s %s", c.Provider, req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s %s: response larger than %d MB", c.Provider, req.URL.Path, limit>>20)
	}
	return b, err
}

func (c Conn) getJSON(ctx context.Context, u string, v any) error {
	b, err := c.get(ctx, u, maxJSON)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: unexpected response: %w", c.Provider, err)
	}
	return nil
}

// Repos lists the repositories the token can read; it doubles as the connection check.
// shortcut: GitHub stops at 1000 repos; type the name if yours isn't listed.
func (c Conn) Repos(ctx context.Context) ([]string, error) {
	var out []string
	switch c.Provider {
	case GitHub:
		for page := 1; page <= 10; page++ {
			var rs []struct {
				FullName string `json:"full_name"`
			}
			if err := c.getJSON(ctx, c.BaseURL+"/user/repos?per_page=100&sort=full_name&page="+strconv.Itoa(page), &rs); err != nil {
				return nil, err
			}
			for _, r := range rs {
				out = append(out, r.FullName)
			}
			if len(rs) < 100 {
				break
			}
		}
	case AzureDevOps:
		var rs struct {
			Value []struct {
				Name    string `json:"name"`
				Project struct {
					Name string `json:"name"`
				} `json:"project"`
			} `json:"value"`
		}
		if err := c.getJSON(ctx, c.BaseURL+"/_apis/git/repositories?api-version=7.1", &rs); err != nil {
			return nil, err
		}
		for _, r := range rs.Value {
			out = append(out, r.Project.Name+"/"+r.Name)
		}
	default:
		return nil, fmt.Errorf("unknown VCS provider %q", c.Provider)
	}
	return out, nil
}

// Tags lists a repository's tag names.
func (c Conn) Tags(ctx context.Context, repo string) ([]string, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []string
	switch c.Provider {
	case GitHub:
		for page := 1; page <= 10; page++ { // shortcut: the newest 1000 tags
			var ts []struct {
				Name string `json:"name"`
			}
			if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/tags?per_page=100&page=%d", c.BaseURL, a, b, page), &ts); err != nil {
				return nil, err
			}
			for _, t := range ts {
				out = append(out, t.Name)
			}
			if len(ts) < 100 {
				break
			}
		}
	case AzureDevOps:
		var rs struct {
			Value []struct {
				Name string `json:"name"`
			} `json:"value"`
		}
		if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?filter=tags/&api-version=7.1", c.BaseURL, a, b), &rs); err != nil {
			return nil, err
		}
		for _, r := range rs.Value {
			out = append(out, strings.TrimPrefix(r.Name, "refs/tags/"))
		}
	default:
		return nil, fmt.Errorf("unknown VCS provider %q", c.Provider)
	}
	return out, nil
}

// Head returns the commit a branch points at.
func (c Conn) Head(ctx context.Context, repo, branch string) (string, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	switch c.Provider {
	case GitHub:
		var br struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		err = c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/branches/%s", c.BaseURL, a, b, url.PathEscape(branch)), &br)
		if err == nil && br.Commit.SHA == "" {
			err = fmt.Errorf("branch %s not found in %s", branch, repo)
		}
		return br.Commit.SHA, err
	case AzureDevOps:
		var rs struct {
			Value []struct {
				Name     string `json:"name"`
				ObjectID string `json:"objectId"`
			} `json:"value"`
		}
		q := url.Values{"filter": {"heads/" + branch}, "api-version": {"7.1"}}
		if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?%s", c.BaseURL, a, b, q.Encode()), &rs); err != nil {
			return "", err
		}
		for _, r := range rs.Value { // the filter is a prefix match
			if r.Name == "refs/heads/"+branch {
				return r.ObjectID, nil
			}
		}
		return "", fmt.Errorf("branch %s not found in %s", branch, repo)
	}
	return "", fmt.Errorf("unknown VCS provider %q", c.Provider)
}

// Archive downloads the repository at a tag or commit (refType "tag" | "commit") as a module
// archive: a .tar.gz with the repo root at the archive root.
func (c Conn) Archive(ctx context.Context, repo, ref, refType string) ([]byte, error) {
	tag := ref
	a, b, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var files []file
	switch c.Provider {
	case GitHub:
		raw, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/%s/tarball/%s", c.BaseURL, a, b, url.PathEscape(tag)), maxDownload)
		if err != nil {
			return nil, err
		}
		if files, err = readTarGz(raw); err != nil {
			return nil, err
		}
	case AzureDevOps:
		q := url.Values{"path": {"/"}, "recursionLevel": {"full"}, "$format": {"zip"}, "download": {"true"},
			"versionDescriptor.version": {tag}, "versionDescriptor.versionType": {refType}, "api-version": {"7.1"}}
		raw, err := c.get(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/items?%s", c.BaseURL, a, b, q.Encode()), maxDownload)
		if err != nil {
			return nil, err
		}
		if files, err = readZip(raw); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown VCS provider %q", c.Provider)
	}
	out, err := writeTarGz(stripTopDir(files))
	if err == nil && len(out) > registry.MaxArchiveSize {
		err = fmt.Errorf("module archive larger than %d MB", registry.MaxArchiveSize>>20)
	}
	return out, err
}

type file struct {
	name string
	body []byte
}

func readTarGz(raw []byte) ([]file, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("repo archive: %w", err)
	}
	tr := tar.NewReader(gz)
	var files []file
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("repo archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue // dirs are implied; symlinks and GitHub's pax comment header are dropped
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxExtracted-total+1))
		if total += int64(len(b)); err != nil || total > maxExtracted {
			return nil, errors.Join(err, errors.New("repo archive is too large when unpacked"))
		}
		files = append(files, file{h.Name, b})
	}
}

func readZip(raw []byte) ([]file, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("repo archive: %w", err)
	}
	var files []file
	var total int64
	for _, f := range zr.File {
		if !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxExtracted-total+1))
		rc.Close()
		if total += int64(len(b)); err != nil || total > maxExtracted {
			return nil, errors.Join(err, errors.New("repo archive is too large when unpacked"))
		}
		files = append(files, file{f.Name, b})
	}
	return files, nil
}

// stripTopDir drops a directory that wraps everything (GitHub's "owner-repo-sha/"), cleans names
// and drops entries that would escape the module root.
func stripTopDir(files []file) []file {
	top := ""
	for i, f := range files {
		dir, _, ok := strings.Cut(strings.TrimPrefix(f.name, "/"), "/")
		if !ok || (i > 0 && dir != top) {
			top = ""
			break
		}
		top = dir
	}
	out := files[:0]
	for _, f := range files {
		name := strings.TrimPrefix(f.name, "/")
		if top != "" {
			name = strings.TrimPrefix(name, top+"/")
		}
		if name = path.Clean(name); name == "." || name == ".." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			continue
		}
		out = append(out, file{name, f.body})
	}
	return out
}

func writeTarGz(files []file) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Key derives the token encryption key from SECRET_KEY.
func Key(secret string) []byte {
	k := sha256.Sum256([]byte("form2stack vcs token\n" + secret))
	return k[:]
}

// Seal encrypts a token with AES-256-GCM (random nonce prepended).
func Seal(key []byte, token string) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(nonce, nonce, []byte(token), nil), nil
}

// Open decrypts a sealed token; it fails if SECRET_KEY changed since the token was stored.
func Open(key, sealed []byte) (string, error) {
	g, err := gcm(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < g.NonceSize() {
		return "", errors.New("sealed token too short")
	}
	b, err := g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], nil)
	if err != nil {
		return "", errors.New("can't decrypt the token: SECRET_KEY changed? Delete and re-add the connection")
	}
	return string(b), nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// ConnFor opens a stored connection's token.
func ConnFor(key []byte, c storage.VCSConnection) (Conn, error) {
	token, err := Open(key, c.Token)
	return Conn{Provider: c.Provider, BaseURL: c.BaseURL, Token: token}, err
}

// Sync publishes every semver tag of a linked repo that isn't a module version yet, and records
// the outcome. A bad tag doesn't stop the others. It returns the versions it added.
// shortcut: a tag that fails to publish is retried on every sync; fine for the odd broken tag.
func Sync(ctx context.Context, st *storage.Store, key []byte, m storage.VCSModule) ([]string, error) {
	added, err := sync(ctx, st, key, m)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if serr := st.SetVCSSync(ctx, m.Namespace, m.Name, m.System, msg); serr != nil {
		err = errors.Join(err, serr)
	}
	return added, err
}

func sync(ctx context.Context, st *storage.Store, key []byte, m storage.VCSModule) ([]string, error) {
	sc, err := st.GetVCSConnection(ctx, m.ConnectionID)
	if err != nil {
		return nil, err
	}
	c, err := ConnFor(key, sc)
	if err != nil {
		return nil, err
	}
	tags, err := c.Tags(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	existing, err := st.ModuleVersions(ctx, m.Namespace, m.Name, m.System)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, v := range existing {
		have[v] = true
	}
	var added []string
	var errs []error
	for _, tag := range tags {
		ver, err := registry.CanonicalVersion(tag)
		if err != nil || have[ver] {
			continue // not a release tag, or already published (v1.0.0 and 1.0.0 are one version)
		}
		have[ver] = true
		archive, err := c.Archive(ctx, m.Repo, tag, "tag")
		if err != nil {
			errs = append(errs, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
		ins, err := registry.Inspect(archive)
		if err != nil {
			errs = append(errs, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
		err = st.CreateModule(ctx, storage.Module{Namespace: m.Namespace, Name: m.Name, System: m.System, Version: ver, Variables: ins.Variables},
			storage.ModuleDocs{Module: ins.Module, Readme: ins.Readme}, archive)
		if errors.Is(err, storage.ErrExists) {
			continue // another replica was faster
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
		added = append(added, ver)
	}
	return added, errors.Join(errs...)
}

// Poll syncs every linked module now and then every interval, until ctx ends. It needs no inbound
// webhooks, so Form2Stack can stay private.
func Poll(ctx context.Context, st *storage.Store, key []byte, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		mods, err := st.VCSModules(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("list vcs modules", "err", err)
		}
		for _, m := range mods {
			added, err := Sync(ctx, st, key, m)
			if err != nil && ctx.Err() == nil {
				log.Warn("vcs sync", "module", m.Namespace+"/"+m.Name+"/"+m.System, "repo", m.Repo, "err", err)
			}
			if len(added) > 0 {
				log.Info("module versions published from vcs", "module", m.Namespace+"/"+m.Name+"/"+m.System, "versions", added)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
