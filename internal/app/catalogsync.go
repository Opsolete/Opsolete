package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// The template catalog can come from a Git repository instead of TEMPLATES_DIR (Settings → Templates):
// the scheduler's leader polls the branch and stores the *.json files of its folder at each new commit
// (PullCatalog), every instance loads them from Postgres (ReloadCatalog), and the editor commits a
// saved template to the branch or opens a pull request with it. Postgres keeps the last loaded files,
// so the catalog doesn't depend on the provider at startup.

// catalogFiles picks the *.json files directly in dir ("" = the repository root).
func catalogFiles(files map[string][]byte, dir string) map[string][]byte {
	if dir == "" {
		dir = "."
	}
	out := map[string][]byte{}
	for name, body := range files {
		if path.Dir(name) == dir && path.Ext(name) == ".json" {
			out[name] = body
		}
	}
	return out
}

// LoadCatalog loads the repository's last synced templates into a.Templates, without asking the
// provider; with no repository configured it leaves them as they are (TEMPLATES_DIR).
func (a *App) LoadCatalog(ctx context.Context) error {
	tc, err := a.Store.GetTemplateCatalog(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return loadSnapshot(a.Templates, tc)
}

// ReloadCatalog brings a.Templates to the snapshot in Postgres when it changed (another instance
// synced or reconfigured it): every instance runs it, only the leader fetches from Git (PullCatalog).
func (a *App) ReloadCatalog(ctx context.Context) error {
	cat := a.Templates
	mode, commit, err := a.Store.TemplateCatalogVersion(ctx)
	if errors.Is(err, storage.ErrNotFound) { // disconnected
		if m, _ := cat.Source(); m != "" {
			return cat.Reopen()
		}
		return nil
	}
	if err != nil {
		return err
	}
	if m, c := cat.Source(); m == mode && c == commit {
		return nil
	}
	return a.LoadCatalog(ctx)
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

// PullCatalog brings a.Templates to the head of the configured branch. A commit with an invalid
// template is recorded as the sync error and not loaded: the catalog keeps the last good commit.
func (a *App) PullCatalog(ctx context.Context) error {
	st, cat := a.Store, a.Templates
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
	head, files, err := a.fetchCatalog(ctx, tc)
	if err == nil && head == tc.Commit {
		ok, err := st.CheckedTemplateCatalog(ctx, tc)
		switch {
		case err != nil:
			return err
		case !ok: // synced or reconfigured meanwhile: serve that, not tc
			return a.LoadCatalog(ctx)
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
				return a.LoadCatalog(ctx)
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
		return errors.Join(err, a.LoadCatalog(ctx))
	}
	return errors.Join(err, loadSnapshot(cat, tc)) // the last good commit, in the current mode
}

// fetchCatalog returns the branch head and, unless it's the commit already loaded, its template files.
func (a *App) fetchCatalog(ctx context.Context, tc storage.TemplateCatalog) (string, map[string][]byte, error) {
	c, err := a.catalogConn(ctx, tc)
	if err != nil {
		return "", nil, err
	}
	if err := c.Paused(); err != nil {
		return "", nil, err
	}
	head, err := c.Head(ctx, tc.Repo, tc.Branch)
	if err != nil || head == tc.Commit {
		return head, nil, err
	}
	files, err := c.Files(ctx, tc.Repo, head, "commit")
	if err != nil {
		return "", nil, err
	}
	return head, catalogFiles(files, tc.Dir), nil
}

func (a *App) catalogConn(ctx context.Context, tc storage.TemplateCatalog) (vcs.Conn, error) {
	sc, err := a.Store.GetVCSConnection(ctx, tc.ConnectionID)
	if err != nil {
		return vcs.Conn{}, fmt.Errorf("VCS connection: %w", err)
	}
	return a.conn(sc)
}

// saveGitTemplate writes t to the catalog's repository, as the editor mode says: a commit to the
// branch (then reloads the catalog), or a pull request (returned: its URL). The commit is based on
// the loaded one, so the provider refuses to overwrite a change made in Git since it was loaded.
func (a *App) saveGitTemplate(ctx context.Context, t templates.Template, by string) (string, error) {
	body, err := templates.Encode(t)
	if err != nil {
		return "", err
	}
	return a.writeTemplate(ctx, t.ID, t.Name, body, by)
}

// deleteGitTemplate removes template id's file from the catalog's repository, like saveGitTemplate.
func (a *App) deleteGitTemplate(ctx context.Context, id, by string) (string, error) {
	t, ok := a.Templates.Get(id)
	if !ok {
		return "", fmt.Errorf("template %q does not exist", id)
	}
	return a.writeTemplate(ctx, id, t.Name, nil, by)
}

// writeTemplate commits body as template id's file; a nil body deletes it.
func (a *App) writeTemplate(ctx context.Context, id, name string, body []byte, by string) (string, error) {
	cat := a.Templates
	tc, err := a.Store.GetTemplateCatalog(ctx)
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
	c, err := a.catalogConn(ctx, tc)
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
		pr, ok, err := templatePR(ctx, c, tc, id)
		if err != nil {
			return "", tokenHint(err)
		}
		if ok {
			head, err := c.Head(ctx, tc.Repo, pr.Branch)
			if err == nil { // the file is on the branch since its first commit; a push meanwhile is refused
				err = c.CommitFile(ctx, tc.Repo, pr.Branch, head, file, body, false, msg)
			}
			if err != nil {
				return "", tokenHint(fmt.Errorf("update pull request %s: %w", pr.URL, err))
			}
			return pr.URL, nil
		}
	}
	if tc.Mode == "pr" { // shortcut: a branch whose commit then fails is left behind; delete it in the provider
		branch = "opsolete/template-" + id + "-" + strings.ToLower(rand.Text()[:6])
		if err := c.CreateBranch(ctx, tc.Repo, branch, base); err != nil {
			return "", tokenHint(fmt.Errorf("create branch %s: %w", branch, err))
		}
	}
	if err := c.CommitFile(ctx, tc.Repo, branch, base, file, body, isNew, msg); err != nil {
		if se, ok := errors.AsType[*vcs.StatusError](err); ok && se.Code == http.StatusConflict || isNew && ok && se.Code == http.StatusUnprocessableEntity {
			a.PullCatalog(ctx)
			return "", fmt.Errorf("the provider refused the commit; if %s changed in Git since it was loaded, the catalog is reloaded now: review and save again. %w", file, err)
		}
		return "", tokenHint(err)
	}
	if tc.Mode == "pr" {
		pr, err := c.OpenPR(ctx, tc.Repo, branch, tc.Branch, title, msg)
		return pr, tokenHint(err)
	}
	if err := a.PullCatalog(ctx); err != nil {
		return "", fmt.Errorf("committed %s to %s, but reloading the catalog failed (it retries every minute): %w", file, tc.Branch, err)
	}
	return "", nil
}

// tokenHint says what to do when the provider refused the connection's token: 401 (GitHub: bad
// credentials), 403 (no write access) or Azure DevOps' 203 sign-in page.
func tokenHint(err error) error {
	se, ok := errors.AsType[*vcs.StatusError](err)
	if !ok || se.Code != http.StatusUnauthorized && se.Code != http.StatusForbidden && se.Code != http.StatusNonAuthoritativeInfo {
		return err
	}
	return fmt.Errorf("the provider refused the connection's token: it may be expired, revoked or lack write access. Replace it with Change access token in Settings → Templates, then try again. (%w)", err)
}

// templatePR finds the open pull request a save of template id opened: its opsolete/template-<id>-
// branch, whose description (the commit message) names the template. Removals are left alone.
// Pull requests opened before branches had the full id aren't found: a save then opens a new one.
func templatePR(ctx context.Context, c vcs.Conn, tc storage.TemplateCatalog, id string) (vcs.PullRequest, bool, error) {
	prs, err := c.PullRequests(ctx, tc.Repo, tc.Branch)
	if err != nil {
		return vcs.PullRequest{}, false, err
	}
	for _, pr := range prs {
		if strings.HasPrefix(pr.Branch, "opsolete/template-"+id+"-") && strings.Contains(pr.Body, "(template "+id+").") &&
			!strings.HasPrefix(pr.Body, "Remove ") {
			return pr, true, nil
		}
	}
	return vcs.PullRequest{}, false, nil
}
