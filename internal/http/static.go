package httpserver

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var staticFS embed.FS

// assetURLs maps a static file to its URL, which carries a hash of its content (app.css →
// /static/app.<hash>.css): browsers cache it for good, a changed file gets a new URL.
// assetFiles is the reverse, by the URL's file name. Both are fixed at startup.
var assetURLs, assetFiles = hashAssets()

func hashAssets() (urls, files map[string]string) {
	urls, files = map[string]string{}, map[string]string{}
	entries, err := fs.ReadDir(staticFS, "static")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		b, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(b)
		ext := path.Ext(e.Name())
		v := strings.TrimSuffix(e.Name(), ext) + "." + hex.EncodeToString(sum[:6]) + ext
		urls[e.Name()], files[v] = "/static/"+v, e.Name()
	}
	return urls, files
}

// asset is the template func for a static file's URL; an unknown file fails the page (a test catches it).
func asset(name string) (string, error) {
	if u, ok := assetURLs[name]; ok {
		return u, nil
	}
	return "", fmt.Errorf("no static file %q", name)
}

// static serves a hashed static file; any other name (e.g. an older hash) is not found.
func static(w http.ResponseWriter, r *http.Request) {
	name, ok := assetFiles[r.PathValue("file")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, _ := staticFS.ReadFile("static/" + name)
	w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(b)
}
