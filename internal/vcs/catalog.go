package vcs

import (
	"context"
	"crypto/rand"
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
func catalogFiles(files []file, dir string) map[string][]byte {
	if dir == "" {
		dir = "."
	}
	out := map[string][]byte{}
	for _, f := range files {
		if path.Dir(f.name) == dir && path.Ext(f.name) == ".json" {
			out[f.name] = f.body
		}
	}
	return out
}

// LoadCatalog loads the repository's last synced templates into cat, without asking the provider;
// with no repository configured it leaves cat as it is (TEMPLATES_DIR).
func LoadCatalog(ctx context.Context, st *storage.Store, cat *templates.Catalog) error {
	tc, err := st.GetTemplateCatalog(ctx)
	if errors.Is(err, storage.ErrNotFound) {
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
	if errors.Is(err, storage.ErrNotFound) { // disconnected, maybe by another instance
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
		ok, err := st.CheckedTemplateCatalog(ctx, tc)
		switch {
		case err != nil:
			return err
		case !ok: // synced or reconfigured meanwhile: serve that, not tc
			return LoadCatalog(ctx, st, cat)
		}
		if mode, commit := cat.Source(); mode == tc.Mode && commit == tc.Commit {
			return nil // already loaded: don't re-parse every poll
		}
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
			ok, err := st.SyncedTemplateCatalog(ctx, tc, head, snap, "")
			if err != nil {
				return err
			}
			if !ok { // another instance stored a newer result while this one fetched: serve that
				return LoadCatalog(ctx, st, cat)
			}
			cat.Load(m, paths, tc.Mode, head)
			return nil
		}
		err = fmt.Errorf("commit %s: %w", short(head), err)
	}
	if ctx.Err() != nil { // shutting down: not the repository's fault
		return err
	}
	ok, rerr := st.SyncedTemplateCatalog(ctx, tc, "", nil, err.Error())
	if rerr != nil {
		return rerr
	}
	if !ok { // another instance stored a newer result: serve that, not this one's stale snapshot
		return errors.Join(err, LoadCatalog(ctx, st, cat))
	}
	return errors.Join(err, loadSnapshot(cat, tc)) // the last good commit, in the current mode
}

// fetchCatalog returns the branch head and, unless it's the commit already loaded, its template files.
func fetchCatalog(ctx context.Context, st *storage.Store, key []byte, tc storage.TemplateCatalog) (string, map[string][]byte, error) {
	c, err := catalogConn(ctx, st, key, tc)
	if err != nil {
		return "", nil, err
	}
	if err := c.paused(); err != nil {
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
	return head, catalogFiles(files, tc.Dir), nil
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
	body, err := templates.Encode(t)
	if err != nil {
		return "", err
	}
	return writeTemplate(ctx, st, key, cat, t.ID, t.Name, body, by)
}

// DeleteTemplate removes template id's file from the catalog's repository, like SaveTemplate.
func DeleteTemplate(ctx context.Context, st *storage.Store, key []byte, cat *templates.Catalog, id, by string) (string, error) {
	t, ok := cat.Get(id)
	if !ok {
		return "", fmt.Errorf("template %q does not exist", id)
	}
	return writeTemplate(ctx, st, key, cat, id, t.Name, nil, by)
}

// writeTemplate commits body as template id's file; a nil body deletes it.
func writeTemplate(ctx context.Context, st *storage.Store, key []byte, cat *templates.Catalog, id, name string, body []byte, by string) (string, error) {
	tc, err := st.GetTemplateCatalog(ctx)
	if errors.Is(err, storage.ErrNotFound) {
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
	c, err := catalogConn(ctx, st, key, tc)
	if err != nil {
		return "", err
	}
	file, isNew, verb := cat.Path(id), false, "Update"
	switch {
	case body == nil && file == "":
		return "", fmt.Errorf("template %q does not exist", id)
	case body == nil:
		verb = "Remove"
	case file == "":
		file, isNew, verb = path.Join(tc.Dir, id+".json"), true, "Add"
	}
	title := fmt.Sprintf("%s template %s", verb, name)
	msg := fmt.Sprintf("%s\n\nSaved in Opsolete by %s (template %s).", title, by, id)
	branch := tc.Branch
	if tc.Mode == "pr" && body != nil { // a save while the template's pull request is open adds a commit to it
		pr, ok, err := c.templatePR(ctx, tc, id)
		if err != nil {
			return "", tokenHint(err)
		}
		if ok {
			head, err := c.Head(ctx, tc.Repo, pr.Branch)
			if err == nil { // the file is on the branch since its first commit; a push meanwhile is refused
				err = c.commitFile(ctx, tc.Repo, pr.Branch, head, file, body, false, msg)
			}
			if err != nil {
				return "", tokenHint(fmt.Errorf("update pull request %s: %w", pr.URL, err))
			}
			return pr.URL, nil
		}
	}
	if tc.Mode == "pr" { // shortcut: a branch whose commit then fails is left behind; delete it in the provider
		branch = "opsolete/template-" + id + "-" + strings.ToLower(rand.Text()[:6])
		if err := c.createBranch(ctx, tc.Repo, branch, base); err != nil {
			return "", tokenHint(fmt.Errorf("create branch %s: %w", branch, err))
		}
	}
	if err := c.commitFile(ctx, tc.Repo, branch, base, file, body, isNew, msg); err != nil {
		if se, ok := errors.AsType[*StatusError](err); ok && se.Code == http.StatusConflict || isNew && ok && se.Code == http.StatusUnprocessableEntity {
			SyncCatalog(ctx, st, key, cat)
			return "", fmt.Errorf("the provider refused the commit; if %s changed in Git since it was loaded, the catalog is reloaded now: review and save again. %w", file, err)
		}
		return "", tokenHint(err)
	}
	if tc.Mode == "pr" {
		pr, err := c.openPR(ctx, tc.Repo, branch, tc.Branch, title, msg)
		return pr, tokenHint(err)
	}
	if err := SyncCatalog(ctx, st, key, cat); err != nil {
		return "", fmt.Errorf("committed %s to %s, but reloading the catalog failed (it retries every minute): %w", file, tc.Branch, err)
	}
	return "", nil
}

// tokenHint says what to do when the provider refused the connection's token: 401 (GitHub: bad
// credentials), 403 (no write access) or Azure DevOps' 203 sign-in page.
func tokenHint(err error) error {
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Code != http.StatusUnauthorized && se.Code != http.StatusForbidden && se.Code != http.StatusNonAuthoritativeInfo {
		return err
	}
	return fmt.Errorf("the provider refused the connection's token: it may be expired, revoked or lack write access. Replace it with Change access token in Settings → Templates, then try again. (%w)", err)
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

// commitFile commits one file to branch, whose current commit must be base. A nil body deletes the file.
func (c Conn) commitFile(ctx context.Context, repo, branch, base, file string, body []byte, isNew bool, msg string) error {
	a, b, err := splitRepo(repo)
	if err != nil {
		return err
	}
	switch c.Provider {
	case GitHub:
		u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.BaseURL, a, b, (&url.URL{Path: file}).EscapedPath())
		req, method := map[string]string{"message": msg, "branch": branch}, http.MethodDelete
		if body != nil {
			req["content"], method = base64.StdEncoding.EncodeToString(body), http.MethodPut
		}
		if !isNew { // the file's blob at base: GitHub answers 409 if the branch has another version by now
			var cur struct {
				SHA string `json:"sha"`
			}
			if err := c.getJSON(ctx, u+"?ref="+url.QueryEscape(base), &cur, false); err != nil {
				return err
			}
			req["sha"] = cur.SHA
		}
		return c.send(ctx, method, u, req, nil)
	case AzureDevOps:
		change := map[string]any{"changeType": "edit", "item": map[string]string{"path": "/" + file},
			"newContent": map[string]string{"content": string(body), "contentType": "rawtext"}}
		if isNew {
			change["changeType"] = "add"
		}
		if body == nil {
			change["changeType"] = "delete"
			delete(change, "newContent")
		}
		// oldObjectId: Azure DevOps refuses the push (409) if the branch moved since base.
		return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pushes?api-version=7.1", c.BaseURL, a, b), map[string]any{
			"refUpdates": []map[string]string{{"name": "refs/heads/" + branch, "oldObjectId": base}},
			"commits":    []map[string]any{{"comment": msg, "changes": []map[string]any{change}}},
		}, nil)
	}
	return fmt.Errorf("unknown VCS provider %q", c.Provider)
}

// templatePR finds the open pull request a save of template id opened: its opsolete/template-<id>-
// branch, whose description (the commit message) names the template. Removals are left alone.
// Pull requests opened before branches had the full id aren't found: a save then opens a new one.
func (c Conn) templatePR(ctx context.Context, tc storage.TemplateCatalog, id string) (PullRequest, bool, error) {
	prs, err := c.PullRequests(ctx, tc.Repo, tc.Branch)
	if err != nil {
		return PullRequest{}, false, err
	}
	for _, pr := range prs {
		if strings.HasPrefix(pr.Branch, "opsolete/template-"+id+"-") && strings.Contains(pr.Body, "(template "+id+").") &&
			!strings.HasPrefix(pr.Body, "Remove ") {
			return pr, true, nil
		}
	}
	return PullRequest{}, false, nil
}

func azurePRURL(base, project, repo string, id int) string {
	return fmt.Sprintf("%s/%s/_git/%s/pullrequest/%d", base, project, repo, id) // project, repo: escaped by splitRepo
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
		return azurePRURL(c.BaseURL, a, b, pr.ID), err
	}
	return "", fmt.Errorf("unknown VCS provider %q", c.Provider)
}
