package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// TestLatestTag: tag-based workspaces run the matching tag with the highest version, not the newest-listed.
func TestLatestTag(t *testing.T) {
	tags := []vcs.Tag{{Name: "1.2.0", Commit: "a"}, {Name: "1.10.0-rc", Commit: "f"}, {Name: "1.10.0", Commit: "b"}, {Name: "v-2.0.0", Commit: "c"}, {Name: "1.9.0-rc", Commit: "d"}, {Name: "release", Commit: "e"}}
	for _, c := range []struct{ pattern, want string }{
		{`^\d+\.\d+\.\d+$`, "1.10.0"}, // semantic versioning: 1.10.0 beats 1.2.0 (not a string compare)
		{`\d+\.\d+\.\d+$`, "v-2.0.0"}, // with a prefix
		{`^\d+\.\d+\.\d+`, "1.10.0"},  // with a suffix: 1.10.0-rc, listed first, is lower
		{`-rc$`, "1.10.0-rc"},
		{`^release$`, ""}, // no version number: never picked
		{`(`, ""},         // bad pattern
	} {
		got, ok := latestTag(tags, c.pattern)
		if got.Name != c.want || ok != (c.want != "") {
			t.Errorf("latestTag(%q) = %q %v, want %q", c.pattern, got.Name, ok, c.want)
		}
	}
}

// TestPollVCSRuns (I6–I8): one round polls each repository branch once however many workspaces
// link it, queues a run of its head on each, and one failing repository doesn't keep the others
// from theirs; a connection the provider asked to wait is skipped until then. Tag-based workspaces
// run only tags newer than their baseline, and a run started by hand plans the newest tag.
func TestPollVCSRuns(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	a.VCSEnabled = true
	a.Registry = &registry.Registry{PublicURL: &url.URL{Scheme: "https", Host: "ops.example.com"}, Key: []byte("poll-test")}
	var mu sync.Mutex
	calls := map[string]int{} // token + path
	broken, wait, tags := false, false, `[{"name":"1.0.0","commit":{"sha":"t100"}}]`
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		calls[token+" "+r.URL.Path]++
		switch {
		case r.URL.Path == "/repos/acme/broken/branches/main" && broken:
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			if token == "slow" && wait {
				w.Header().Set("Retry-After", "3600")
			}
			w.Write([]byte(`{"commit":{"sha":"sha1"}}`))
		case r.URL.Path == "/repos/acme/infra/tags":
			w.Write([]byte(tags))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	a.VCS = vcs.NewClient(gh.Client())
	conn := func(token string) uuid.UUID {
		c := storage.VCSConnection{Name: uniq("gh-"), Provider: vcs.GitHub, BaseURL: gh.URL}
		var err error
		if c.Token, err = a.Secrets.Seal(token, TokenAD(c)); err != nil {
			t.Fatal(err)
		}
		id, err := a.Store.CreateVCSConnection(ctx, c)
		if err == nil {
			err = a.Store.SetVCSScope(ctx, id, "all")
		}
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pat, slow := conn("pat"), conn("slow")
	linked := func(c uuid.UUID, repo string, tags bool) storage.Workspace {
		t.Helper()
		ws, err := a.CreateEmptyWorkspace(ctx, admin, EmptyWorkspace{Name: uniq("poll-"), VCS: VCSLinkInput{ConnectionID: c, Repo: repo, Trigger: true, Tags: tags, TagFormat: "semantic"}})
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	w1, w2, bad, paused, tagged := linked(pat, "acme/infra", false), linked(pat, "acme/infra", false), linked(pat, "acme/broken", false),
		linked(slow, "acme/infra", false), linked(pat, "acme/infra", true)
	// commit is ws's latest run's commit, "" without a run.
	commit := func(ws storage.Workspace) string {
		runs, err := a.Store.Runs(ctx, ws.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 0 {
			return ""
		}
		return runs[0].VCSCommit
	}
	end := func(ws storage.Workspace) {
		if _, err := a.Store.Pool.Exec(ctx, `UPDATE runs SET status = 'applied' WHERE workspace_id = $1`, ws.ID); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	clear(calls)
	broken, wait = true, true // the slow token's provider asks to wait from its next response
	mu.Unlock()

	var logged bytes.Buffer
	a.Log = slog.New(slog.NewTextHandler(&logged, nil))
	if err := a.PollVCSRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), `msg="vcs trigger" repo=acme/broken`) {
		t.Errorf("the failing repository isn't logged:\n%s", logged.String())
	}
	for ws, want := range map[storage.Workspace]string{w1: "sha1", w2: "sha1", bad: "", paused: "sha1", tagged: ""} {
		if got := commit(ws); got != want {
			t.Errorf("%s: run of %q, want %q", ws.Name, got, want)
		}
	}
	mu.Lock()
	if calls["pat /repos/acme/infra/branches/main"] != 1 || calls["pat /repos/acme/infra/tags"] != 1 || calls["pat /repos/acme/broken/branches/main"] != 1 {
		t.Errorf("one request per branch (and tag list) per round: %v", calls)
	}
	clear(calls)
	tags = `[{"name":"1.1.0","commit":{"sha":"t110"}},{"name":"1.0.0","commit":{"sha":"t100"}},{"name":"2.0.0-rc","commit":{"sha":"t2rc"}}]`
	broken = false
	mu.Unlock()
	for _, ws := range []storage.Workspace{w1, w2, paused} {
		end(ws)
	}

	if err := a.PollVCSRuns(ctx); err != nil { // logs the failures, returns none
		t.Fatal(err)
	}
	mu.Lock()
	if calls["slow /repos/acme/infra/branches/main"] != 0 {
		t.Errorf("a connection asked to wait was polled: %v", calls)
	}
	mu.Unlock()
	if got := commit(paused); got != "sha1" {
		t.Errorf("paused: %q", got)
	}
	if runs, _ := a.Store.Runs(ctx, paused.ID, 5); len(runs) != 1 {
		t.Errorf("a paused connection's workspace got another run: %d", len(runs))
	}
	if got := commit(bad); got != "sha1" {
		t.Errorf("the repository works again: run of %q", got)
	}
	if got := commit(tagged); got != "t110" {
		t.Errorf("a tag pushed after the baseline: run of %q, want t110", got)
	}
	for _, ws := range []storage.Workspace{w1, w2} { // the same commit runs once
		if runs, _ := a.Store.Runs(ctx, ws.ID, 5); len(runs) != 1 {
			t.Errorf("%s: %d runs of one commit", ws.Name, len(runs))
		}
	}

	// by hand, a tag-based workspace plans the newest matching tag (not a prerelease)
	end(tagged)
	_, run, err := a.StartRun(ctx, admin, tagged.Name, NewRun{Action: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, ok, err := a.ClaimJob(ctx, "r")
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if got, _, _ := a.Store.GetRun(ctx, run.ID); got.Status == storage.Planning {
			if got.VCSCommit != "t110" {
				t.Fatalf("manual run of a tag-based workspace plans %q, want t110", got.VCSCommit)
			}
			break
		}
	}
}
