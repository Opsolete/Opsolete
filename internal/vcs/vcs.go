// Package vcs is the client of GitHub and Azure DevOps: repositories, tags, branch heads, pull
// requests, archives and the commits and pull requests of the template editor. It talks to the
// providers' REST APIs with an access token, so the server needs no git binary. What Opsolete does
// with them (module versions, VCS runs, the Git template catalog) is internal/app's.
package vcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
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

// Client talks to the providers and keeps what polling learns between rounds: cached responses and
// rate-limit pauses. App owns one; tests build their own.
type Client struct {
	http   *http.Client
	mu     sync.Mutex
	cache  map[string]cachedResponse
	pauses map[string]time.Time // token hash + API root -> when the provider wants polling to resume
}

// NewClient returns a client sending through hc; nil uses a client with a 2 minute timeout.
func NewClient(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Client{http: hc, cache: map[string]cachedResponse{}, pauses: map[string]time.Time{}}
}

// Conn is a usable connection: provider, API root and plain token, sending through its client.
type Conn struct {
	Provider, BaseURL, Token string
	client                   *Client
	p                        provider // nil: unknown provider (its operations fail)
}

// Conn returns a connection sending through cl. An unknown provider (connections are validated when
// created, see NormalizeURL) gives a Conn whose operations fail with "unknown VCS provider".
func (cl *Client) Conn(name, baseURL, token string) Conn {
	return Conn{Provider: name, BaseURL: baseURL, Token: token, client: cl, p: providerFor(name)}
}

// provider is what differs between GitHub and Azure DevOps (github.go, azuredevops.go); Conn does the
// rest: requests, the ETag cache, pauses, size limits, error mapping and unpacking archives. a and b
// are the path-escaped repository parts (splitRepo).
type provider interface {
	authorize(req *http.Request, token string)
	etags() bool // polled reads may be conditional requests (see cache)
	repos(ctx context.Context, c Conn) ([]string, error)
	tags(ctx context.Context, c Conn, a, b string) ([]Tag, error)
	head(ctx context.Context, c Conn, a, b, branch string) (string, error)                      // "": no such branch
	pullRequests(ctx context.Context, c Conn, repo, a, b, branch string) ([]PullRequest, error) // forks left out
	download(ctx context.Context, c Conn, a, b, ref, refType string) ([]file, error)            // see Conn.download
	createBranch(ctx context.Context, c Conn, a, b, branch, commit string) error
	commitFile(ctx context.Context, c Conn, a, b, branch, base, file string, body []byte, isNew bool, msg string) error
	openPR(ctx context.Context, c Conn, a, b, branch, target, title, body string) (string, error) // its web URL
}

func providerFor(name string) provider {
	switch name {
	case GitHub:
		return github{}
	case AzureDevOps:
		return azureDevOps{}
	}
	return nil
}

// prov is the connection's provider, or why it has none.
func (c Conn) prov() (provider, error) {
	switch {
	case c.client == nil:
		return nil, errNoClient
	case c.p == nil:
		return nil, fmt.Errorf("unknown VCS provider %q", c.Provider)
	}
	return c.p, nil
}

// repo checks and splits repo (splitRepo), then returns the provider.
func (c Conn) repo(repo string) (provider, string, string, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return nil, "", "", err
	}
	p, err := c.prov()
	return p, a, b, err
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

// Paused is an error while the provider asked to wait: pollers skip a round instead of hammering it.
func (c Conn) Paused() error {
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
// errNoClient: a Conn built as a literal, not by Client.Conn (a bug).
var errNoClient = errors.New("vcs: connection without a client (build it with Client.Conn)")

func (c Conn) get(ctx context.Context, u string, limit int64, conditional bool) ([]byte, error) {
	if c.client == nil {
		return nil, errNoClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	conditional = conditional && c.p.etags()
	key := fmt.Sprintf("%x %s", sha256.Sum256([]byte(c.Token)), u) // another token may see other data
	var cached cachedResponse
	c.p.authorize(req, c.Token)
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
	c.p.authorize(req, c.Token)
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
func (c Conn) Repos(ctx context.Context) ([]string, error) {
	p, err := c.prov()
	if err != nil {
		return nil, err
	}
	return p.repos(ctx, c)
}

// Tag is a repository tag and the commit it points at.
type Tag struct{ Name, Commit string }

// Tags lists a repository's tags; an annotated tag gives the commit it points at.
func (c Conn) Tags(ctx context.Context, repo string) ([]Tag, error) {
	p, a, b, err := c.repo(repo)
	if err != nil {
		return nil, err
	}
	return p.tags(ctx, c, a, b)
}

// Head returns the commit a branch points at.
func (c Conn) Head(ctx context.Context, repo, branch string) (string, error) {
	p, a, b, err := c.repo(repo)
	if err != nil {
		return "", err
	}
	head, err := p.head(ctx, c, a, b, branch)
	if err == nil && head == "" {
		err = fmt.Errorf("branch %s not found in %s", branch, repo)
	}
	return head, err
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
func (c Conn) PullRequests(ctx context.Context, repo, branch string) ([]PullRequest, error) {
	p, a, b, err := c.repo(repo)
	if err != nil {
		return nil, err
	}
	return p.pullRequests(ctx, c, repo, a, b, branch)
}
