package vcs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// The template catalog can come from a Git repository instead of TEMPLATES_DIR (Settings → Templates):
// every app instance polls the branch and loads the *.json files of its folder at each new commit, and
// the editor commits a saved template to the branch or opens a pull request with it. Postgres keeps the
// last loaded files, so the catalog doesn't depend on the provider at startup.

// catalogFiles picks the *.json files directly in dir ("" = the repository root).
func catalogFiles(provider string, files []file, dir string) map[string][]byte {
	if dir == "" {
		dir = "."
	}
	out := map[string][]byte{}
	for _, f := range files {
		name := strings.TrimPrefix(f.name, "/")
		if provider == GitHub { // its tarball has everything under <owner>-<repo>-<sha>/
			_, name, _ = strings.Cut(name, "/")
		}
		if name = path.Clean(name); path.Dir(name) == dir && path.Ext(name) == ".json" {
			out[name] = f.body
		}
	}
	return out
}

// LoadCatalog loads the repository's last synced templates into cat, without asking the provider;
// with no repository configured it leaves cat as it is (TEMPLATES_DIR).
func LoadCatalog(ctx context.Context, st *storage.Store, cat *templates.Catalog) error {
	tc, err := st.GetTemplateCatalog(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return loadSnapshot(cat, tc)
}

func loadSnapshot(cat *templates.Catalog, tc storage.TemplateCatalog) error {
	files := map[string][]byte{}
	for p, b := range tc.Files {
		files[p] = []byte(b)
	}
	m, paths, err := templates.Parse(files)
	if err != nil {
		return fmt.Errorf("template catalog at %s: %w", short(tc.Commit), err)
	}
	cat.Load(m, paths, tc.Mode, tc.Commit)
	return nil
}

// SyncCatalog brings cat to the head of the configured branch. A commit with an invalid template is
// recorded as the sync error and not loaded: the catalog keeps the last good commit.
func SyncCatalog(ctx context.Context, st *storage.Store, key []byte, cat *templates.Catalog) error {
	tc, err := st.GetTemplateCatalog(ctx)
	if errors.Is(err, sql.ErrNoRows) { // disconnected, maybe by another instance
		if mode, _ := cat.Source(); mode != "" {
			return cat.Reopen()
		}
		return nil
	}
	if err != nil {
		return err
	}
	head, files, err := fetchCatalog(ctx, st, key, tc)
	if err == nil && head == tc.Commit {
		return loadSnapshot(cat, tc) // another instance synced it, or only the mode changed
	}
	if err == nil {
		var m map[string]templates.Template
		var paths map[string]string
		if m, paths, err = templates.Parse(files); err == nil {
			snap := map[string]string{}
			for p, b := range files {
				snap[p] = string(b)
			}
			if err = st.SyncedTemplateCatalog(ctx, head, snap, ""); err == nil {
				cat.Load(m, paths, tc.Mode, head)
			}
			return err
		}
		err = fmt.Errorf("commit %s: %w", short(head), err)
	}
	if ctx.Err() != nil { // shutting down: not the repository's fault
		return err
	}
	if rerr := st.SyncedTemplateCatalog(ctx, "", nil, err.Error()); rerr != nil {
		return rerr
	}
	if lerr := loadSnapshot(cat, tc); lerr != nil { // the last good commit, in the current mode
		return lerr
	}
	return err
}

// fetchCatalog returns the branch head and, unless it's the commit already loaded, its template files.
func fetchCatalog(ctx context.Context, st *storage.Store, key []byte, tc storage.TemplateCatalog) (string, map[string][]byte, error) {
	c, err := catalogConn(ctx, st, key, tc)
	if err != nil {
		return "", nil, err
	}
	head, err := c.Head(ctx, tc.Repo, tc.Branch)
	if err != nil || head == tc.Commit {
		return head, nil, err
	}
	a, b, err := splitRepo(tc.Repo)
	if err != nil {
		return "", nil, err
	}
	files, err := c.download(ctx, a, b, head, "commit")
	if err != nil {
		return "", nil, err
	}
	return head, catalogFiles(c.Provider, files, tc.Dir), nil
}

func catalogConn(ctx context.Context, st *storage.Store, key []byte, tc storage.TemplateCatalog) (Conn, error) {
	sc, err := st.GetVCSConnection(ctx, tc.ConnectionID)
	if err != nil {
		return Conn{}, fmt.Errorf("VCS connection: %w", err)
	}
	return ConnFor(key, sc)
}

// PollCatalog syncs the template catalog now and then every interval, until ctx ends.
func PollCatalog(ctx context.Context, st *storage.Store, key []byte, cat *templates.Catalog, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := SyncCatalog(ctx, st, key, cat); err != nil && ctx.Err() == nil {
			log.Warn("template catalog sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SaveTemplate writes t to the catalog's repository, as the editor mode says: a commit to the branch
// (then reloads the catalog), or a pull request (returned: its URL). The commit is based on the loaded
// one, so the provider refuses to overwrite a change made in Git since it was loaded.
func SaveTemplate(ctx context.Context, st *storage.Store, key []byte, cat *templates.Catalog, t templates.Template, by string) (string, error) {
	tc, err := st.GetTemplateCatalog(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("the template catalog is not a Git repository")
	}
	if err != nil {
		return "", err
	}
	if tc.Mode != "commit" && tc.Mode != "pr" {
		return "", errors.New("the template editor is off for this catalog (Settings → Templates)")
	}
	_, base := cat.Source() // what the admin saw; a stale one makes the provider refuse, not overwrite
	if base == "" {
		return "", errors.New("the catalog isn't loaded from the repository yet: see Settings → Templates")
	}
	body, err := templates.Encode(t)
	if err != nil {
		return "", err
	}
	c, err := catalogConn(ctx, st, key, tc)
	if err != nil {
		return "", err
	}
	file, isNew, verb := cat.Path(t.ID), false, "Update"
	if file == "" {
		file, isNew, verb = path.Join(tc.Dir, t.ID+".json"), true, "Add"
	}
	title := fmt.Sprintf("%s template %s", verb, t.Name)
	msg := fmt.Sprintf("%s\n\nSaved in Opsolete by %s (template %s).", title, by, t.ID)
	branch := tc.Branch
	if tc.Mode == "pr" { // shortcut: a branch whose commit then fails is left behind; delete it in the provider
		branch = "opsolete/template-" + t.ID[:min(len(t.ID), 8)] + "-" + strings.ToLower(rand.Text()[:6])
		if err := c.createBranch(ctx, tc.Repo, branch, base); err != nil {
			return "", fmt.Errorf("create branch %s: %w", branch, err)
		}
	}
	if err := c.commitFile(ctx, tc.Repo, branch, base, file, body, isNew, msg); err != nil {
		if se, ok := errors.AsType[*StatusError](err); ok && se.Code == http.StatusConflict || isNew && ok && se.Code == http.StatusUnprocessableEntity {
			SyncCatalog(ctx, st, key, cat)
			return "", fmt.Errorf("the provider refused the commit; if %s changed in Git since it was loaded, the catalog is reloaded now: review and save again. %w", file, err)
		}
		return "", err
	}
	if tc.Mode == "pr" {
		return c.openPR(ctx, tc.Repo, branch, tc.Branch, title, msg)
	}
	if err := SyncCatalog(ctx, st, key, cat); err != nil {
		return "", fmt.Errorf("committed %s to %s, but reloading the catalog failed (it retries every minute): %w", file, tc.Branch, err)
	}
	return "", nil
}

const zeroCommit = "0000000000000000000000000000000000000000"

func (c Conn) createBranch(ctx context.Context, repo, branch, commit string) error {
	a, b, err := splitRepo(repo)
	if err != nil {
		return err
	}
	switch c.Provider {
	case GitHub:
		return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/git/refs", c.BaseURL, a, b),
			map[string]string{"ref": "refs/heads/" + branch, "sha": commit}, nil)
	case AzureDevOps:
		var res struct {
			Value []struct {
				Success      bool   `json:"success"`
				UpdateStatus string `json:"updateStatus"`
			} `json:"value"`
		}
		err := c.send(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?api-version=7.1", c.BaseURL, a, b),
			[]map[string]string{{"name": "refs/heads/" + branch, "oldObjectId": zeroCommit, "newObjectId": commit}}, &res)
		if err == nil && (len(res.Value) != 1 || !res.Value[0].Success) {
			err = fmt.Errorf("azure-devops: branch not created: %+v", res.Value)
		}
		return err
	}
	return fmt.Errorf("unknown VCS provider %q", c.Provider)
}

// commitFile commits one file to branch, whose current commit must be base.
func (c Conn) commitFile(ctx context.Context, repo, branch, base, file string, body []byte, isNew bool, msg string) error {
	a, b, err := splitRepo(repo)
	if err != nil {
		return err
	}
	switch c.Provider {
	case GitHub:
		u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.BaseURL, a, b, (&url.URL{Path: file}).EscapedPath())
		req := map[string]string{"message": msg, "content": base64.StdEncoding.EncodeToString(body), "branch": branch}
		if !isNew { // the file's blob at base: GitHub answers 409 if the branch has another version by now
			var cur struct {
				SHA string `json:"sha"`
			}
			if err := c.getJSON(ctx, u+"?ref="+url.QueryEscape(base), &cur, false); err != nil {
				return err
			}
			req["sha"] = cur.SHA
		}
		return c.send(ctx, http.MethodPut, u, req, nil)
	case AzureDevOps:
		change := "edit"
		if isNew {
			change = "add"
		}
		// oldObjectId: Azure DevOps refuses the push (409) if the branch moved since base.
		return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pushes?api-version=7.1", c.BaseURL, a, b), map[string]any{
			"refUpdates": []map[string]string{{"name": "refs/heads/" + branch, "oldObjectId": base}},
			"commits": []map[string]any{{"comment": msg, "changes": []map[string]any{{"changeType": change, "item": map[string]string{"path": "/" + file},
				"newContent": map[string]string{"content": string(body), "contentType": "rawtext"}}}}},
		}, nil)
	}
	return fmt.Errorf("unknown VCS provider %q", c.Provider)
}

// openPR opens a pull request of branch into target and returns its web URL.
func (c Conn) openPR(ctx context.Context, repo, branch, target, title, body string) (string, error) {
	a, b, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	switch c.Provider {
	case GitHub:
		var pr struct {
			HTMLURL string `json:"html_url"`
		}
		err := c.send(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/pulls", c.BaseURL, a, b),
			map[string]string{"title": title, "head": branch, "base": target, "body": body}, &pr)
		return pr.HTMLURL, err
	case AzureDevOps:
		var pr struct {
			ID int `json:"pullRequestId"`
		}
		err := c.send(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pullrequests?api-version=7.1", c.BaseURL, a, b),
			map[string]string{"sourceRefName": "refs/heads/" + branch, "targetRefName": "refs/heads/" + target, "title": title, "description": body}, &pr)
		return fmt.Sprintf("%s/%s/_git/%s/pullrequest/%d", c.BaseURL, a, b, pr.ID), err // a, b: escaped
	}
	return "", fmt.Errorf("unknown VCS provider %q", c.Provider)
}
