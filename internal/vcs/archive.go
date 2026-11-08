package vcs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// ErrNoFolder is a module's folder missing at a ref, e.g. a monorepo tag from before the module existed.
var ErrNoFolder = errors.New("folder missing at this ref")

// Archive downloads the repository at a tag or commit (refType "tag" | "commit") as a module
// archive: a .tar.gz with folder dir of the repo ("" = the root) at the archive root, at most max
// bytes (a required limit: archives are kept in memory and stored).
func (c Conn) Archive(ctx context.Context, repo, ref, refType, dir string, max int) ([]byte, error) {
	files, err := c.download(ctx, repo, ref, refType)
	if err != nil {
		return nil, err
	}
	if dir != "" {
		files = subdir(files, dir)
		if len(files) == 0 {
			return nil, fmt.Errorf("no folder %q in the repository: %w", dir, ErrNoFolder)
		}
	}
	b, err := writeTarGz(files)
	if err == nil && len(b) > max {
		err = fmt.Errorf("module archive larger than %d MB", max>>20)
	}
	return b, err
}

// Files returns the files of repo at a tag or commit (refType "tag" | "commit"), named relative to the repo root.
func (c Conn) Files(ctx context.Context, repo, ref, refType string) (map[string][]byte, error) {
	files, err := c.download(ctx, repo, ref, refType)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		out[f.name] = f.body
	}
	return out, nil
}

// download returns the files of repo at a tag or commit, named relative to the repo root.
func (c Conn) download(ctx context.Context, repo, ref, refType string) ([]file, error) {
	p, a, b, err := c.repo(repo)
	if err != nil {
		return nil, err
	}
	return p.download(ctx, c, a, b, ref, refType)
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
