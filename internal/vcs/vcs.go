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

// Client talks to the providers and keeps what polling learns between rounds: cached responses,
// rate-limit pauses and tags without a module's folder. App owns one; tests build their own.
type Client struct {
	http     *http.Client
	mu       sync.Mutex
	cache    map[string]cachedResponse
	pauses   map[string]time.Time        // token hash + API root -> when the provider wants polling to resume
	noFolder map[string]map[string]error // module (connection, repo, folder) -> tag commit -> the ErrNoFolder Archive gave there
}

// NewClient returns a client sending through hc; nil uses a client with a 2 minute timeout.
func NewClient(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Client{http: hc, cache: map[string]cachedResponse{}, pauses: map[string]time.Time{}, noFolder: map[string]map[string]error{}}
}

// Conn is a usable connection: provider, API root and plain token, sending through its client.
type Conn struct {
	Provider, BaseURL, Token string
	client                   *Client
}

// Conn returns a connection sending through cl.
func (cl *Client) Conn(provider, baseURL, token string) Conn {
	return Conn{Provider: provider, BaseURL: baseURL, Token: token, client: cl}
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
// shortcut: at most maxCached responses up to maxCachedSize each, then a random one is evicted; LRU if
// more repos than that are polled.
const (
	maxCached     = 500
	maxCachedSize = 1 << 20
)

type cachedResponse struct {
	etag string
	body []byte
}

// pausedUntil is when the provider asked this connection's token to wait until; zero if it didn't.
// Keyed by token, not host: GitHub's rate limits are per token.
func (c Conn) pausedUntil() time.Time {
	if c.client == nil {
		return time.Time{} // get and send report the missing client
	}
	c.client.mu.Lock()
	defer c.client.mu.Unlock()
	if t := c.client.pauses[c.pauseKey()]; time.Now().Before(t) {
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
		cl := c.client
		cl.mu.Lock()
		for k, t := range cl.pauses { // over: forget it, so tokens replaced long ago don't pile up
			if time.Now().After(t) {
				delete(cl.pauses, k)
			}
		}
		cl.pauses[c.pauseKey()] = later(cl.pauses[c.pauseKey()], until)
		cl.mu.Unlock()
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// get fetches u; conditional (GitHub only, see cache) makes it a conditional request.
// errNoClient: a Conn built as a literal, not by Client.Conn or Client.ConnFor (a bug).
var errNoClient = errors.New("vcs: connection without a client (build it with Client.Conn)")

func (c Conn) get(ctx context.Context, u string, limit int64, conditional bool) ([]byte, error) {
	if c.client == nil {
		return nil, errNoClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	conditional = conditional && c.Provider == GitHub
	key := fmt.Sprintf("%x %s", sha256.Sum256([]byte(c.Token)), u) // another token may see other data
	var cached cachedResponse
	c.authorize(req)
	cl := c.client
	if conditional {
		cl.mu.Lock()
		cached = cl.cache[key]
		cl.mu.Unlock()
		if cached.etag != "" {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	resp, err := cl.http.Do(req) // Go drops Authorization on redirects to other hosts (GitHub's codeload is pre-signed)
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
		return nil, &StatusError{resp.StatusCode, fmt.Sprintf("%s %s: %s %s", c.Provider, req.URL.Path, resp.Status, strings.TrimSpace(string(msg)))}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s %s: response larger than %d MB", c.Provider, req.URL.Path, limit>>20)
	}
	if etag := resp.Header.Get("ETag"); err == nil && conditional && etag != "" && len(b) <= maxCachedSize {
		cl.mu.Lock()
		for k := range cl.cache { // full: drop one entry (whichever map order gives), not every 304 at once
			if len(cl.cache) < maxCached {
				break
			}
			delete(cl.cache, k)
		}
		cl.cache[key] = cachedResponse{etag, b}
		cl.mu.Unlock()
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

// StatusError is a provider's error answer (to a write: other than 2xx; to a read: other than 200).
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

// send makes a JSON write request and decodes the answer into out (if not nil).
func (c Conn) send(ctx context.Context, method, u string, body, out any) error {
	if c.client == nil {
		return errNoClient
	}
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
	resp, err := c.client.http.Do(req)
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
	Branch string // the source branch
	Body   string // its description
	URL    string // its web page
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
			Number  int    `json:"number"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				SHA  string `json:"sha"`
				Ref  string `json:"ref"`
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
				out = append(out, PullRequest{Number: p.Number, Commit: p.Head.SHA, Branch: p.Head.Ref, Body: p.Body, URL: p.HTMLURL})
			}
		}
	case AzureDevOps:
		var rs struct {
			Value []struct {
				ID          int    `json:"pullRequestId"`
				SourceRef   string `json:"sourceRefName"`
				Description string `json:"description"`
				Source      struct {
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
				out = append(out, PullRequest{Number: p.ID, Commit: p.Source.CommitID, Branch: strings.TrimPrefix(p.SourceRef, "refs/heads/"),
					Body: p.Description, URL: azurePRURL(c.BaseURL, a, b, p.ID)})
			}
		}
	default:
		return nil, fmt.Errorf("unknown VCS provider %q", c.Provider)
	}
	return out, nil
}

// ErrNoFolder is a module's folder missing at a ref, e.g. a monorepo tag from before the module existed.
var ErrNoFolder = errors.New("folder missing at this ref")

// Archive downloads the repository at a tag or commit (refType "tag" | "commit") as a module
// archive: a .tar.gz with folder dir of the repo ("" = the root) at the archive root.
func (c Conn) Archive(ctx context.Context, repo, ref, refType, dir string) ([]byte, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	files, err := c.download(ctx, a, b, ref, refType)
	if err != nil {
		return nil, err
	}
	if dir != "" {
		files = subdir(files, dir)
		if len(files) == 0 {
			return nil, fmt.Errorf("no folder %q in the repository: %w", dir, ErrNoFolder)
		}
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
		// a zip of a folder path is always recursive; Azure rejects recursionLevel alongside path
		q := url.Values{"path": {"/"}, "$format": {"zip"}, "download": {"true"},
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

// subdir keeps the files under dir (a clean relative path), named relative to it.
func subdir(files []file, dir string) []file {
	var out []file
	for _, f := range files {
		if name, ok := strings.CutPrefix(f.name, dir+"/"); ok {
			out = append(out, file{name, f.body})
		}
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

// OpenToken opens a stored connection's sealed token (app.App.OpenToken).
type OpenToken func(storage.VCSConnection) (string, error)

// ConnFor opens a stored connection's token.
func (cl *Client) ConnFor(open OpenToken, c storage.VCSConnection) (Conn, error) {
	token, err := open(c)
	return cl.Conn(c.Provider, c.BaseURL, token), err
}

// Sync publishes every semver tag of a linked repo that isn't a module version yet, and records
// the outcome. A bad tag doesn't stop the others. It returns the versions it added.
// shortcut: a tag that fails to publish is retried on every sync; fine for the odd broken tag.
func (cl *Client) Sync(ctx context.Context, st *storage.Store, open OpenToken, m storage.VCSModule) ([]string, error) {
	added, err := cl.syncModule(ctx, st, open, m)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if serr := st.SetVCSSync(ctx, m.Namespace, m.Name, m.System, msg); serr != nil {
		err = errors.Join(err, serr)
	}
	return added, err
}

func (cl *Client) syncModule(ctx context.Context, st *storage.Store, open OpenToken, m storage.VCSModule) ([]string, error) {
	sc, err := st.GetVCSConnection(ctx, m.ConnectionID)
	if err != nil {
		return nil, err
	}
	c, err := cl.ConnFor(open, sc)
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
	// a commit's tree never changes: once without the folder, always without it, so Sync skips the download.
	// Only unpublished tags still listed are kept: the cache is pruned on every sync.
	mod := noFolderKey(m)
	cl.mu.Lock()
	seen := cl.noFolder[mod]
	cl.mu.Unlock()
	kept := map[string]error{}
	var added []string
	var errs, noFolder []error
	for _, t := range tags {
		tag := t.Name
		ver, err := registry.CanonicalVersion(tag)
		if err != nil || have[ver] {
			continue // not a release tag, or already published (v1.0.0 and 1.0.0 are one version)
		}
		have[ver] = true
		if known := seen[t.Commit]; known != nil && t.Commit != "" {
			kept[t.Commit] = known
			noFolder = append(noFolder, fmt.Errorf("tag %s: %w", tag, known))
			continue
		}
		archive, err := c.Archive(ctx, m.Repo, tag, "tag", m.Dir)
		if errors.Is(err, ErrNoFolder) {
			if t.Commit != "" {
				kept[t.Commit] = err
			}
			noFolder = append(noFolder, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
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
	cl.mu.Lock()
	if len(kept) > 0 {
		cl.noFolder[mod] = kept
	} else {
		delete(cl.noFolder, mod)
	}
	cl.mu.Unlock()
	// in a monorepo, tags from before the module's folder existed aren't its versions; only a folder no tag has is an error
	if len(existing) == 0 && len(added) == 0 {
		errs = append(errs, noFolder...)
	}
	return added, errors.Join(errs...)
}

// Poll syncs every linked module once (a scheduler job). It needs no inbound webhooks, so Opsolete
// can stay private. A module's error is logged and doesn't stop the others.
func (cl *Client) Poll(ctx context.Context, st *storage.Store, open OpenToken, log *slog.Logger) error {
	mods, err := st.VCSModules(ctx)
	if err != nil {
		return fmt.Errorf("list vcs modules: %w", err)
	}
	linked := map[string]bool{}
	for _, m := range mods {
		linked[noFolderKey(m)] = true
	}
	cl.mu.Lock()
	for k := range cl.noFolder { // unlinked (or relinked to another folder) since
		if !linked[k] {
			delete(cl.noFolder, k)
		}
	}
	cl.mu.Unlock()
	for _, m := range mods {
		added, err := cl.Sync(ctx, st, open, m)
		if err != nil && ctx.Err() == nil {
			log.Warn("vcs sync", "module", m.Namespace+"/"+m.Name+"/"+m.System, "repo", m.Repo, "err", err)
		}
		if len(added) > 0 {
			log.Info("module versions published from vcs", "module", m.Namespace+"/"+m.Name+"/"+m.System, "versions", added)
		}
	}
	return nil
}

func noFolderKey(m storage.VCSModule) string {
	return m.ConnectionID.String() + "\x00" + m.Repo + "\x00" + m.Dir
}

// PollRuns queues the runs VCS events call for, once (a scheduler job): an apply run for a new
// commit on a workspace's branch, a speculative plan for each new commit of a pull request into it.
// Polling, like Poll, needs no inbound webhooks, so Opsolete can stay internal. Each repository
// branch is polled once per round however many workspaces link it, and on GitHub with conditional
// requests, so an unchanged branch costs no rate limit.
func (cl *Client) PollRuns(ctx context.Context, st *storage.Store, open OpenToken, log *slog.Logger) error {
	wss, err := st.VCSTriggered(ctx)
	if err != nil {
		return fmt.Errorf("list vcs-triggered workspaces: %w", err)
	}
	groups := map[string][]storage.Workspace{}
	for _, ws := range wss {
		k := ws.VCSConnectionID.String() + "\n" + strings.ToLower(ws.VCSRepo) + "\n" + ws.VCSBranch
		groups[k] = append(groups[k], ws)
	}
	for _, g := range groups {
		if err := cl.TriggerRuns(ctx, st, open, g); err != nil && ctx.Err() == nil {
			log.Warn("vcs trigger", "repo", g[0].VCSRepo, "branch", g[0].VCSBranch, "workspaces", len(g), "err", err)
		}
	}
	return nil
}

// TriggerRuns queues the runs a branch and its pull requests call for (see PollRuns) on the
// workspaces of group, which all link the same connection, repository and branch. A workspace with
// an active run gets them at a later round, one at a time. One workspace's error (or a failed list
// of tags, head or pull requests) doesn't keep the others from their runs; all are returned.
func (cl *Client) TriggerRuns(ctx context.Context, st *storage.Store, open OpenToken, group []storage.Workspace) error {
	ws := group[0]
	sc, err := st.GetVCSConnection(ctx, ws.VCSConnectionID)
	if err != nil {
		return err
	}
	c, err := cl.ConnFor(open, sc)
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
		if !sc.Allows(ws.Name, ws.ProjectName) { // deselected (or moved out of a selected project) since the link; manual runs fail with the reason
			continue
		}
		if ws.VCSTagsRegex != "" { // tag-based: no branch commits, no pull requests
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

var tagVersionRe = regexp.MustCompile(`\d+(\.\d+)+(-[0-9A-Za-z.-]+)?`) // keeps -rc: a prerelease ranks below its release

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
