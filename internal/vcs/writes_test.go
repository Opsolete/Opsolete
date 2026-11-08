package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Repository files are named relative to the root (the Git catalog picks its folder from them).
func TestRepoFiles(t *testing.T) {
	files := repoFiles([]file{{"acme-cat-abc/catalog/a.json", []byte("a")}, {"acme-cat-abc/root.json", []byte("r")}}, true)
	if !reflect.DeepEqual(files, []file{{"catalog/a.json", []byte("a")}, {"root.json", []byte("r")}}) {
		t.Fatalf("github: %v", files)
	}
	// Azure DevOps zips have no top directory: a repo holding only catalog/ must not lose it.
	if az := repoFiles([]file{{"/catalog/a.json", []byte("a")}}, false); !reflect.DeepEqual(az, []file{{"catalog/a.json", []byte("a")}}) {
		t.Fatalf("azure devops: %v", az)
	}
}

// recorder answers every request with the status and body of the first matching route, and records it.
type recorder struct {
	mu     sync.Mutex
	got    []string // "METHOD path?query body"
	routes map[string][2]string
}

func (rc *recorder) server(t *testing.T) *httptest.Server {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		key := r.Method + " " + r.URL.Path
		rc.got = append(rc.got, strings.TrimSuffix(key+"?"+r.URL.RawQuery, "?")+" "+string(b))
		res, ok := rc.routes[key]
		rc.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		code := http.StatusOK
		if res[0] != "" {
			code = map[string]int{"201": 201, "401": 401, "409": 409, "422": 422}[res[0]]
		}
		w.WriteHeader(code)
		w.Write([]byte(res[1]))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestGitHubWrites(t *testing.T) {
	rc := &recorder{routes: map[string][2]string{
		"POST /repos/acme/cat/git/refs":                 {"201", `{}`},
		"GET /repos/acme/cat/contents/catalog/a.json":   {"", `{"sha":"blob1"}`},
		"PUT /repos/acme/cat/contents/catalog/a.json":   {"", `{}`},
		"PUT /repos/acme/cat/contents/catalog/new.json": {"422", `{"message":"sha wasn't supplied"}`},
		"POST /repos/acme/cat/pulls":                    {"201", `{"html_url":"https://github.com/acme/cat/pull/7"}`},
	}}
	ts := rc.server(t)
	c := NewClient(ts.Client()).Conn(GitHub, ts.URL, "tok")
	ctx := context.Background()
	if err := c.CreateBranch(ctx, "acme/cat", "opsolete/x", "base1"); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitFile(ctx, "acme/cat", "opsolete/x", "base1", "catalog/a.json", []byte("{}\n"), false, "Update"); err != nil {
		t.Fatal(err)
	}
	url, err := c.OpenPR(ctx, "acme/cat", "opsolete/x", "main", "Update", "body")
	if err != nil || url != "https://github.com/acme/cat/pull/7" {
		t.Fatalf("pr: %q %v", url, err)
	}
	want := []string{
		`POST /repos/acme/cat/git/refs {"ref":"refs/heads/opsolete/x","sha":"base1"}`,
		`GET /repos/acme/cat/contents/catalog/a.json?ref=base1 `, // the blob the edit is based on
		`PUT /repos/acme/cat/contents/catalog/a.json {"branch":"opsolete/x","content":"e30K","message":"Update","sha":"blob1"}`,
		`POST /repos/acme/cat/pulls {"base":"main","body":"body","head":"opsolete/x","title":"Update"}`,
	}
	if !reflect.DeepEqual(rc.got, want) {
		t.Fatalf("requests:\n%s", strings.Join(rc.got, "\n"))
	}
	// A new file that exists by now: the provider's refusal comes back with its status.
	err = c.CommitFile(ctx, "acme/cat", "main", "base1", "catalog/new.json", []byte("{}"), true, "Add")
	if se, ok := errors.AsType[*StatusError](err); !ok || se.Code != 422 || strings.Contains(rc.got[len(rc.got)-1], `"sha"`) {
		t.Fatalf("new file: %v %s", err, rc.got[len(rc.got)-1])
	}
}

func TestAzureDevOpsWrites(t *testing.T) {
	repo := "/org/Platform Team/_apis/git/repositories/cat"
	rc := &recorder{routes: map[string][2]string{
		"POST " + repo + "/refs":         {"", `{"value":[{"success":true}]}`},
		"POST " + repo + "/pushes":       {"409", `{"message":"TF401028: The reference has already been updated by another client"}`},
		"POST " + repo + "/pullrequests": {"201", `{"pullRequestId":12}`},
	}}
	ts := rc.server(t)
	c := NewClient(ts.Client()).Conn(AzureDevOps, ts.URL+"/org", "pat")
	ctx := context.Background()
	if err := c.CreateBranch(ctx, "Platform Team/cat", "opsolete/x", "base1"); err != nil {
		t.Fatal(err)
	}
	err := c.CommitFile(ctx, "Platform Team/cat", "main", "base1", "catalog/a.json", []byte("{}"), false, "Update")
	if se, ok := errors.AsType[*StatusError](err); !ok || se.Code != http.StatusConflict {
		t.Fatalf("a moved branch must be refused: %v", err)
	}
	url, err := c.OpenPR(ctx, "Platform Team/cat", "opsolete/x", "main", "Update", "body")
	if err != nil || url != ts.URL+"/org/Platform%20Team/_git/cat/pullrequest/12" {
		t.Fatalf("pr: %q %v", url, err)
	}
	var push map[string]any
	json.Unmarshal([]byte(strings.SplitN(rc.got[1], " ", 4)[3]), &push)
	ref := push["refUpdates"].([]any)[0].(map[string]any)
	change := push["commits"].([]any)[0].(map[string]any)["changes"].([]any)[0].(map[string]any)
	if ref["name"] != "refs/heads/main" || ref["oldObjectId"] != "base1" || change["changeType"] != "edit" ||
		change["item"].(map[string]any)["path"] != "/catalog/a.json" {
		t.Fatalf("push: %s", rc.got[1])
	}
	if !strings.Contains(rc.got[0], `"oldObjectId":"0000000000000000000000000000000000000000","newObjectId":"base1"`) &&
		!strings.Contains(rc.got[0], `"newObjectId":"base1","oldObjectId":"0000000000000000000000000000000000000000"`) {
		t.Fatalf("create branch: %s", rc.got[0])
	}
	if !strings.Contains(rc.got[2], `"sourceRefName":"refs/heads/opsolete/x"`) || !strings.Contains(rc.got[2], `"targetRefName":"refs/heads/main"`) {
		t.Fatalf("pull request: %s", rc.got[2])
	}
}
