package vcs

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// The exact requests Azure DevOps gets (URLs, api-version, page sizes, push bodies) and what is made
// of its answers; no conditional requests (ETags are GitHub only).
func TestAzureDevOpsRequests(t *testing.T) {
	var got []string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if _, pw, _ := r.BasicAuth(); pw != "pat" || r.Header.Get("If-None-Match") != "" {
			t.Errorf("auth %q, If-None-Match %q", pw, r.Header.Get("If-None-Match"))
		}
		got = append(got, strings.TrimSpace(r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b)))
		w.Header().Set("ETag", `"x"`)
		switch q := r.URL.Query(); {
		case strings.HasSuffix(r.URL.Path, "/_apis/git/repositories"):
			w.Write([]byte(`{"value":[{"name":"cat","project":{"name":"Platform Team"}}]}`))
		case q.Get("filter") == "tags/":
			w.Write([]byte(`{"value":[{"name":"refs/tags/v1.0.0","objectId":"tagobj","peeledObjectId":"c1"},{"name":"refs/tags/v0.9.0","objectId":"c09"}]}`))
		case q.Get("filter") == "heads/main":
			w.Write([]byte(`{"value":[{"name":"refs/heads/main","objectId":"h1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/pullrequests") && r.Method == http.MethodGet:
			w.Write([]byte(`{"value":[{"pullRequestId":7,"sourceRefName":"refs/heads/feature","description":"d","lastMergeSourceCommit":{"commitId":"p7"}},
				{"pullRequestId":8,"lastMergeSourceCommit":{"commitId":"p8"},"forkSource":{"name":"refs/heads/x"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/items"):
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			f, _ := zw.Create("/catalog/a.json")
			f.Write([]byte("{}"))
			zw.Close()
			w.Write(buf.Bytes())
		case strings.HasSuffix(r.URL.Path, "/refs"):
			w.Write([]byte(`{"value":[{"success":true}]}`))
		default:
			w.Write([]byte(`{"pullRequestId":9}`))
		}
	}))
	defer ts.Close()
	c := NewClient(ts.Client()).Conn(AzureDevOps, ts.URL+"/org", "pat")
	ctx, repo := t.Context(), "Platform Team/cat"
	if repos, err := c.Repos(ctx); err != nil || !slices.Equal(repos, []string{repo}) {
		t.Fatalf("repos: %v %v", repos, err)
	}
	for range 2 { // the second time too: no cached answer
		if tags, err := c.Tags(ctx, repo); err != nil || !slices.Equal(tags, []Tag{{"v1.0.0", "c1"}, {"v0.9.0", "c09"}}) {
			t.Fatalf("tags (annotated: the peeled commit): %v %v", tags, err)
		}
	}
	if h, err := c.Head(ctx, repo, "main"); err != nil || h != "h1" {
		t.Fatalf("head: %q %v", h, err)
	}
	if _, err := c.Head(ctx, repo, "nope"); err == nil || err.Error() != "branch nope not found in Platform Team/cat" {
		t.Fatalf("missing branch: %v", err)
	}
	prs, err := c.PullRequests(ctx, repo, "main")
	if want := []PullRequest{{7, "p7", "feature", "d", ts.URL + "/org/Platform%20Team/_git/cat/pullrequest/7"}}; err != nil || !slices.Equal(prs, want) {
		t.Fatalf("pull requests (fork left out): %v %v", prs, err)
	}
	archive, err := c.Archive(ctx, repo, "c1", "commit", "catalog")
	if files, _ := readTarGz(archive); err != nil || len(files) != 1 || files[0].name != "a.json" {
		t.Fatalf("archive: %v %v", files, err)
	}
	if err := c.createBranch(ctx, repo, "opsolete/x", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := c.commitFile(ctx, repo, "opsolete/x", "c1", "catalog/new.json", []byte("{}"), true, "Add"); err != nil {
		t.Fatal(err)
	}
	if err := c.commitFile(ctx, repo, "main", "h1", "catalog/a.json", nil, false, "Remove"); err != nil {
		t.Fatal(err)
	}
	if u, err := c.openPR(ctx, repo, "opsolete/x", "main", "Add", "body"); err != nil || u != ts.URL+"/org/Platform%20Team/_git/cat/pullrequest/9" {
		t.Fatalf("pr: %q %v", u, err)
	}
	p := "/org/Platform Team/_apis/git/repositories/cat"
	want := []string{
		"GET /org/_apis/git/repositories?api-version=7.1",
		"GET " + p + "/refs?filter=tags/&peelTags=true&api-version=7.1",
		"GET " + p + "/refs?filter=tags/&peelTags=true&api-version=7.1",
		"GET " + p + "/refs?api-version=7.1&filter=heads%2Fmain",
		"GET " + p + "/refs?api-version=7.1&filter=heads%2Fnope",
		"GET " + p + "/pullrequests?%24top=100&api-version=7.1&searchCriteria.status=active&searchCriteria.targetRefName=refs%2Fheads%2Fmain",
		"GET " + p + "/items?%24format=zip&api-version=7.1&download=true&path=%2F&versionDescriptor.version=c1&versionDescriptor.versionType=commit",
		"POST " + p + `/refs?api-version=7.1 [{"name":"refs/heads/opsolete/x","newObjectId":"c1","oldObjectId":"0000000000000000000000000000000000000000"}]`,
		"POST " + p + `/pushes?api-version=7.1 {"commits":[{"changes":[{"changeType":"add","item":{"path":"/catalog/new.json"},"newContent":{"content":"{}","contentType":"rawtext"}}],"comment":"Add"}],"refUpdates":[{"name":"refs/heads/opsolete/x","oldObjectId":"c1"}]}`,
		"POST " + p + `/pushes?api-version=7.1 {"commits":[{"changes":[{"changeType":"delete","item":{"path":"/catalog/a.json"}}],"comment":"Remove"}],"refUpdates":[{"name":"refs/heads/main","oldObjectId":"h1"}]}`,
		"POST " + p + `/pullrequests?api-version=7.1 {"description":"body","sourceRefName":"refs/heads/opsolete/x","targetRefName":"refs/heads/main","title":"Add"}`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("requests:\n%s", strings.Join(got, "\n"))
	}
}
