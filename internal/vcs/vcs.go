// Package vcs publishes registry modules from GitHub and Azure DevOps repositories (every
// semver tag of a linked repo becomes an immutable module version), and starts the runs of
// workspaces linked to a branch on pushes and pull requests (see PollRuns). It talks
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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
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
type Conn struct {
	Provider, BaseURL, Token string
}

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

// GitHub answers a conditional request with 304 when nothing changed, and a 304 doesn't count
// against the rate limit, so polling an unchanged branch or pull request list is free. Responses
// are cached per token and URL, in memory: each replica's first poll after a start costs one call.
// shortcut: at most maxCached responses up to maxCachedSize each, then the cache starts over.
const (
	maxCached     = 500
	maxCachedSize = 1 << 20
)

type cachedResponse struct {
	etag string
	body []byte
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cachedResponse{}
	pauses  = map[string]time.Time{} // token hash + API root -> when the provider wants polling to resume
)

// pausedUntil is when the provider asked this connection's token to wait until; zero if it didn't.
// Keyed by token, not host: GitHub's rate limits are per token.
func (c Conn) pausedUntil() time.Time {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if t := pauses[c.pauseKey()]; time.Now().Before(t) {
		return t
	}
	return time.Time{}
}

// paused is an error while the provider asked to wait: pollers skip a round instead of hammering it.
func (c Conn) paused() error {
	if until := c.pausedUntil(); !until.IsZero() {
		return fmt.Errorf("%s asked to wait: polling resumes at %s", c.Provider, until.Format(time.TimeOnly))
	}
	return nil
}

func (c Conn) pauseKey() string {
	return fmt.Sprintf("%x %s", sha256.Sum256([]byte(c.Token)), c.BaseURL)
}

// notePause records how long the provider wants clients to wait: Retry-After (GitHub and Azure
// DevOps, which also sends it with a 200 when a client uses too much), and an exhausted GitHub
// rate limit (x-ratelimit-reset).
func (c Conn) notePause(resp *http.Response) {
	var until time.Time
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
		until = time.Now().Add(time.Duration(n) * time.Second)
	}
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			until = later(until, time.Unix(n, 0))
		}
	}
	if !until.IsZero() {
		cacheMu.Lock()
		pauses[c.pauseKey()] = later(pauses[c.pauseKey()], until)
		cacheMu.Unlock()
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// get fetches u; conditional (GitHub only, see cache) makes it a conditional request.
func (c Conn) get(ctx context.Context, u string, limit int64, conditional bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	conditional = conditional && c.Provider == GitHub
	key := fmt.Sprintf("%x %s", sha256.Sum256([]byte(c.Token)), u) // another token may see other data
	var cached cachedResponse
	c.authorize(req)
	if conditional {
		cacheMu.Lock()
		cached = cache[key]
		cacheMu.Unlock()
		if cached.etag != "" {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	resp, err := HTTPClient.Do(req) // Go drops Authorization on redirects to other hosts (GitHub's codeload is pre-signed)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.notePause(resp)
	if resp.StatusCode == http.StatusNotModified && cached.etag != "" {
		return cached.body, nil
	}
	// Azure DevOps answers a bad token with 203 and a sign-in page, so anything but 200 is an error.
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("%s %s: %s %s", c.Provider, req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s %s: response larger than %d MB", c.Provider, req.URL.Path, limit>>20)
	}
	if etag := resp.Header.Get("ETag"); err == nil && conditional && etag != "" && len(b) <= maxCachedSize {
		cacheMu.Lock()
		if len(cache) >= maxCached {
			clear(cache)
		}
		cache[key] = cachedResponse{etag, b}
		cacheMu.Unlock()
	}
	return b, err
}

func (c Conn) authorize(req *http.Request) {
	switch c.Provider {
	case GitHub:
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	case AzureDevOps:
		req.SetBasicAuth("", c.Token) // PAT as the password
	}
}

// StatusError is a provider's answer other than 2xx to a write.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

// send makes a JSON write request and decodes the answer into out (if not nil).
func (c Conn) send(ctx context.Context, method, u string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.notePause(resp)
	// Azure DevOps answers a bad token with 203 and a sign-in page.
	if resp.StatusCode < 200 || resp.StatusCode > 299 || resp.StatusCode == http.StatusNonAuthoritativeInfo {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return &StatusError{resp.StatusCode, fmt.Sprintf("%s %s %s: %s %s", c.Provider, method, req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(out); err != nil {
		return fmt.Errorf("%s: unexpected response: %w", c.Provider, err)
	}
	return nil
}

// getJSON decodes u; polled endpoints (branches, pull requests, tags) set conditional (see cache).
func (c Conn) getJSON(ctx context.Context, u string, v any, conditional bool) error {
	b, err := c.get(ctx, u, maxJSON, conditional)
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
			if err := c.getJSON(ctx, c.BaseURL+"/user/repos?per_page=100&sort=full_name&page="+strconv.Itoa(page), &rs, false); err != nil {
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
		if err := c.getJSON(ctx, c.BaseURL+"/_apis/git/repositories?api-version=7.1", &rs, false); err != nil {
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

// Tag is a repository tag and the commit it points at.
type Tag struct{ Name, Commit string }

// Tags lists a repository's tags.
func (c Conn) Tags(ctx context.Context, repo string) ([]Tag, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []Tag
	switch c.Provider {
	case GitHub:
		for page := 1; page <= 10; page++ { // shortcut: the first 1000 tags GitHub lists (not by date); page through git refs if repos get more
			var ts []struct {
				Name   string `json:"name"`
				Commit struct {
					SHA string `json:"sha"`
				} `json:"commit"`
			}
			if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/tags?per_page=100&page=%d", c.BaseURL, a, b, page), &ts, true); err != nil {
				return nil, err
			}
			for _, t := range ts {
				out = append(out, Tag{t.Name, t.Commit.SHA})
			}
			if len(ts) < 100 {
				break
			}
		}
	case AzureDevOps:
		var rs struct {
			Value []struct {
				Name           string `json:"name"`
				ObjectID       string `json:"objectId"`
				PeeledObjectID string `json:"peeledObjectId"` // the commit of an annotated tag
			} `json:"value"`
		}
		if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?filter=tags/&peelTags=true&api-version=7.1", c.BaseURL, a, b), &rs, true); err != nil {
			return nil, err
		}
		for _, r := range rs.Value {
			commit := r.PeeledObjectID
			if commit == "" {
				commit = r.ObjectID
			}
			out = append(out, Tag{strings.TrimPrefix(r.Name, "refs/tags/"), commit})
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
		err = c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/branches/%s", c.BaseURL, a, b, url.PathEscape(branch)), &br, true)
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
		if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?%s", c.BaseURL, a, b, q.Encode()), &rs, true); err != nil {
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

// PullRequest is an open pull request into a branch, at its latest commit.
type PullRequest struct {
	Number int
	Commit string
}

// PullRequests lists the open pull requests into branch that come from the repository itself.
// Pull requests from forks are left out: anyone may open one, and its commit isn't in the repository.
// shortcut: the first 100 pull requests; plenty for one branch.
func (c Conn) PullRequests(ctx context.Context, repo, branch string) ([]PullRequest, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []PullRequest
	switch c.Provider {
	case GitHub:
		var prs []struct {
			Number int `json:"number"`
			Head   struct {
				SHA  string `json:"sha"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"` // null once a fork is deleted
			} `json:"head"`
		}
		q := url.Values{"state": {"open"}, "base": {branch}, "per_page": {"100"}}
		if err := c.getJSON(ctx, fmt.Sprintf("%s/repos/%s/%s/pulls?%s", c.BaseURL, a, b, q.Encode()), &prs, true); err != nil {
			return nil, err
		}
		for _, p := range prs {
			if p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, repo) && p.Head.SHA != "" {
				out = append(out, PullRequest{p.Number, p.Head.SHA})
			}
		}
	case AzureDevOps:
		var rs struct {
			Value []struct {
				ID     int `json:"pullRequestId"`
				Source struct {
					CommitID string `json:"commitId"`
				} `json:"lastMergeSourceCommit"`
				ForkSource *struct{} `json:"forkSource"`
			} `json:"value"`
		}
		q := url.Values{"searchCriteria.status": {"active"}, "searchCriteria.targetRefName": {"refs/heads/" + branch},
			"$top": {"100"}, "api-version": {"7.1"}}
		if err := c.getJSON(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pullrequests?%s", c.BaseURL, a, b, q.Encode()), &rs, true); err != nil {
			return nil, err
		}
		for _, p := range rs.Value {
			if p.ForkSource == nil && p.Source.CommitID != "" {
				out = append(out, PullRequest{p.ID, p.Source.CommitID})
			}
		}
	default:
		return nil, fmt.Errorf("unknown VCS provider %q", c.Provider)
	}
	return out, nil
}

// Archive downloads the repository at a tag or commit (refType "tag" | "commit") as a module
// archive: a .tar.gz with the repo root at the archive root.
func (c Conn) Archive(ctx context.Context, repo, ref, refType string) ([]byte, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	files, err := c.download(ctx, a, b, ref, refType)
	if err != nil {
		return nil, err
	}
	out, err := writeTarGz(files)
	if err == nil && len(out) > registry.MaxArchiveSize {
		err = fmt.Errorf("module archive larger than %d MB", registry.MaxArchiveSize>>20)
	}
	return out, err
}

// download returns the files of repository a/b at a tag or commit, named relative to the repo root.
func (c Conn) download(ctx context.Context, a, b, tag, refType string) ([]file, error) {
	var files []file
	switch c.Provider {
	case GitHub:
		raw, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/%s/tarball/%s", c.BaseURL, a, b, url.PathEscape(tag)), maxDownload, false)
		if err != nil {
			return nil, err
		}
		if files, err = readTarGz(raw); err != nil {
			return nil, err
		}
	case AzureDevOps:
		q := url.Values{"path": {"/"}, "recursionLevel": {"full"}, "$format": {"zip"}, "download": {"true"},
			"versionDescriptor.version": {tag}, "versionDescriptor.versionType": {refType}, "api-version": {"7.1"}}
		raw, err := c.get(ctx, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/items?%s", c.BaseURL, a, b, q.Encode()), maxDownload, false)
		if err != nil {
			return nil, err
		}
		if files, err = readZip(raw); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown VCS provider %q", c.Provider)
	}
	return repoFiles(files, c.Provider == GitHub), nil
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

// repoFiles names files relative to the repo root: it drops GitHub's "<owner>-<repo>-<sha>/" (top),
// cleans names and drops entries that would escape the root. Azure DevOps zips have no top directory.
func repoFiles(files []file, top bool) []file {
	out := files[:0]
	for _, f := range files {
		name := strings.TrimPrefix(f.name, "/")
		if top {
			_, name, _ = strings.Cut(name, "/")
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
	k := sha256.Sum256([]byte("opsolete vcs token\n" + secret))
	return k[:]
}

// sealedV2 starts values sealed with associated data. Older values (a bare nonce and ciphertext)
// open only through OpenLegacy.
var sealedV2 = []byte("opsolete-v2:")

// Seal encrypts plain with AES-256-GCM (random nonce). ad binds the result to where it is stored
// (e.g. VariableAD): copied to another row or file, it no longer opens.
func Seal(key []byte, plain, ad string) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(append(bytes.Clone(sealedV2), nonce...), nonce, []byte(plain), []byte(ad)), nil
}

// Open decrypts a value sealed with the same ad; it fails if SECRET_KEY changed since, or the
// value was moved.
func Open(key, sealed []byte, ad string) (string, error) {
	if !bytes.HasPrefix(sealed, sealedV2) {
		return "", errors.New("can't decrypt: not sealed for this place, or from before Opsolete re-sealed it")
	}
	return open(key, sealed[len(sealedV2):], []byte(ad))
}

// OpenLegacy decrypts a value sealed without associated data, for re-sealing and old state files.
func OpenLegacy(key, sealed []byte) (string, error) { return open(key, sealed, nil) }

func open(key, sealed, ad []byte) (string, error) {
	g, err := gcm(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < g.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	b, err := g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], ad)
	if err != nil {
		return "", errors.New("can't decrypt: SECRET_KEY changed, or the value was moved? Set it again")
	}
	return string(b), nil
}

// TokenAD binds a VCS token to its connection's endpoint: pointing base_url elsewhere in the
// database must not send the token there.
func TokenAD(c storage.VCSConnection) string {
	return "vcs token\x00" + c.Provider + "\x00" + c.BaseURL + "\x00" + c.Name
}

// VariableAD binds a variable's value to its owner, name and sensitivity: a sensitive value copied
// into a non-sensitive row (which the UI shows) or into another workspace doesn't open.
func VariableAD(v storage.Variable) string {
	return fmt.Sprintf("variable\x00%s\x00%s\x00%s\x00%s\x00%t", v.Owner.WorkspaceID, v.Owner.SetID, v.Category, v.Key, v.Sensitive)
}

// Reseal moves variables and VCS tokens sealed without associated data to Seal's format, once
// at startup; values that don't open (SECRET_KEY changed) are left as they are.
func Reseal(ctx context.Context, st *storage.Store, key []byte) (int, error) {
	n := 0
	vars, err := st.AllVariables(ctx)
	if err != nil {
		return n, err
	}
	for _, v := range vars {
		if bytes.HasPrefix(v.Value, sealedV2) {
			continue
		}
		plain, err := OpenLegacy(key, v.Value)
		if err != nil {
			continue
		}
		sealed, err := Seal(key, plain, VariableAD(v))
		if err != nil {
			return n, err
		}
		if err := st.ResealVariable(ctx, v.ID, v.Value, sealed); err != nil {
			return n, err
		}
		n++
	}
	conns, err := st.VCSConnections(ctx)
	if err != nil {
		return n, err
	}
	for _, c := range conns {
		if bytes.HasPrefix(c.Token, sealedV2) {
			continue
		}
		plain, err := OpenLegacy(key, c.Token)
		if err != nil {
			continue
		}
		sealed, err := Seal(key, plain, TokenAD(c))
		if err != nil {
			return n, err
		}
		if err := st.ResealVCSToken(ctx, c.ID, c.Token, sealed); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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
	token, err := Open(key, c.Token, TokenAD(c))
	return Conn{Provider: c.Provider, BaseURL: c.BaseURL, Token: token}, err
}

// Sync publishes every semver tag of a linked repo that isn't a module version yet, and records
// the outcome. A bad tag doesn't stop the others. It returns the versions it added.
// shortcut: a tag that fails to publish is retried on every sync; fine for the odd broken tag.
func Sync(ctx context.Context, st *storage.Store, key []byte, m storage.VCSModule) ([]string, error) {
	added, err := syncModule(ctx, st, key, m)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if serr := st.SetVCSSync(ctx, m.Namespace, m.Name, m.System, msg); serr != nil {
		err = errors.Join(err, serr)
	}
	return added, err
}

func syncModule(ctx context.Context, st *storage.Store, key []byte, m storage.VCSModule) ([]string, error) {
	sc, err := st.GetVCSConnection(ctx, m.ConnectionID)
	if err != nil {
		return nil, err
	}
	c, err := ConnFor(key, sc)
	if err != nil {
		return nil, err
	}
	if err := c.paused(); err != nil {
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
	for _, t := range tags {
		tag := t.Name
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
// webhooks, so Opsolete can stay private.
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

// PollRuns queues the runs VCS events call for, now and then every interval, until ctx ends: an
// apply run for a new commit on a workspace's branch, a speculative plan for each new commit of a
// pull request into it. Polling, like Poll, needs no inbound webhooks, so Opsolete can stay
// internal. Each repository branch is polled once per round however many workspaces link it, and
// on GitHub with conditional requests, so an unchanged branch costs no rate limit.
func PollRuns(ctx context.Context, st *storage.Store, key []byte, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		wss, err := st.VCSTriggered(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("list vcs-triggered workspaces", "err", err)
		}
		groups := map[string][]storage.Workspace{}
		for _, ws := range wss {
			k := ws.VCSConnectionID + "\n" + strings.ToLower(ws.VCSRepo) + "\n" + ws.VCSBranch
			groups[k] = append(groups[k], ws)
		}
		for _, g := range groups {
			if err := TriggerRuns(ctx, st, key, g); err != nil && ctx.Err() == nil {
				log.Warn("vcs trigger", "repo", g[0].VCSRepo, "branch", g[0].VCSBranch, "workspaces", len(g), "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// TriggerRuns queues the runs a branch and its pull requests call for (see PollRuns) on the
// workspaces of group, which all link the same connection, repository and branch. A workspace with
// an active run gets them at a later round, one at a time. One workspace's error (or a failed list
// of tags, head or pull requests) doesn't keep the others from their runs; all are returned.
func TriggerRuns(ctx context.Context, st *storage.Store, key []byte, group []storage.Workspace) error {
	ws := group[0]
	sc, err := st.GetVCSConnection(ctx, ws.VCSConnectionID)
	if err != nil {
		return err
	}
	c, err := ConnFor(key, sc)
	if err != nil {
		return err
	}
	if err := c.paused(); err != nil {
		return err
	}
	// each list is fetched once per round, at first need; a failed fetch fails just its users
	tags := sync.OnceValues(func() ([]Tag, error) { return c.Tags(ctx, ws.VCSRepo) })
	head := sync.OnceValues(func() (string, error) { return c.Head(ctx, ws.VCSRepo, ws.VCSBranch) })
	prs := sync.OnceValues(func() ([]PullRequest, error) { return c.PullRequests(ctx, ws.VCSRepo, ws.VCSBranch) })
	var errs []error
	queue := func(ws storage.Workspace, commit string, pr int, msg string) {
		if _, err := st.QueueVCSRun(ctx, ws.ID, commit, pr, msg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ws.Name, err))
		}
	}
	for _, ws := range group {
		if !sc.AllowsWorkspace(ws.Name) { // deselected since the link; manual runs fail with the reason
			continue
		}
		if ws.VCSTagsRegex != "" { // tag-based: no branch commits, no pull requests, as in HCP Terraform
			list, err := tags()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: tags: %w", ws.Name, err))
			} else if tag, ok := LatestTag(list, ws.VCSTagsRegex); ok && tag.Commit != ws.VCSTagBaseline {
				queue(ws, tag.Commit, 0, fmt.Sprintf("tag %s at %s: apply plan queued, waiting for a runner", tag.Name, short(tag.Commit)))
			}
			continue
		}
		if ws.VCSTrigger {
			if h, err := head(); err != nil {
				errs = append(errs, fmt.Errorf("%s: branch head: %w", ws.Name, err))
			} else {
				queue(ws, h, 0, fmt.Sprintf("commit %s on %s: apply plan queued, waiting for a runner", short(h), ws.VCSBranch))
			}
		}
		if !ws.VCSPullRequests {
			continue
		}
		list, err := prs()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: pull requests: %w", ws.Name, err))
			continue
		}
		for _, pr := range list {
			queue(ws, pr.Commit, pr.Number, fmt.Sprintf("pull request #%d at %s: speculative plan queued, waiting for a runner", pr.Number, short(pr.Commit)))
		}
	}
	return errors.Join(errs...)
}

func short(commit string) string { return commit[:min(len(commit), 8)] }

var tagVersionRe = regexp.MustCompile(`\d+(\.\d+)+`)

// LatestTag returns the matching tag with the highest version in its name; tag-based workspaces
// run it. A bad pattern matches nothing.
// shortcut: tags without a version number (1.2 or 1.2.3) are never newest; order by date if custom patterns need it.
func LatestTag(tags []Tag, pattern string) (Tag, bool) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Tag{}, false
	}
	var best Tag
	var bestV *version.Version
	for _, t := range tags {
		if !re.MatchString(t.Name) || t.Commit == "" {
			continue
		}
		v, err := version.NewVersion(tagVersionRe.FindString(t.Name))
		if err == nil && (bestV == nil || v.GreaterThan(bestV)) {
			best, bestV = t, v
		}
	}
	return best, bestV != nil
}
