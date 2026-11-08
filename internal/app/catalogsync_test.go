package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/vcs"
)

func TestCatalogFiles(t *testing.T) {
	// as vcs.Conn.Files names them: relative to the repository root
	files := map[string][]byte{"catalog/a.json": []byte("a"), "catalog/sub/b.json": nil, "catalog/README.md": nil, "root.json": []byte("r")}
	if got := catalogFiles(files, "catalog"); !reflect.DeepEqual(got, map[string][]byte{"catalog/a.json": []byte("a")}) {
		t.Fatalf("folder: %v", got)
	}
	if got := catalogFiles(files, ""); !reflect.DeepEqual(got, map[string][]byte{"root.json": []byte("r")}) {
		t.Fatalf("root: %v", got)
	}
}

// A refused token says how to fix it, whether a read (GET contents) or a write fails; other errors don't.
func TestTokenHint(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/cat/contents/catalog/a.json", "POST /repos/acme/cat/git/refs":
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	c := vcs.NewClient(ts.Client()).Conn(vcs.GitHub, ts.URL, "bad")
	ctx := context.Background()
	for _, err := range []error{
		c.CommitFile(ctx, "acme/cat", "main", "base1", "catalog/a.json", nil, false, "Remove"),
		c.CreateBranch(ctx, "acme/cat", "opsolete/x", "base1"),
	} {
		if msg := tokenHint(err).Error(); !strings.Contains(msg, "Change access token") || !strings.Contains(msg, "Bad credentials") {
			t.Fatalf("hint: %s", msg)
		}
	}
	if err := tokenHint(&vcs.StatusError{Code: http.StatusConflict, Msg: "x"}); err.Error() != "x" || tokenHint(nil) != nil {
		t.Fatalf("no hint for other errors: %v", err)
	}
}
