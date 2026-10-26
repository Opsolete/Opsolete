// Package registry implements Opsolete's private Terraform module registry: archive
// inspection, address validation, signed download URLs and mapping module variables to form inputs.
// The HTTP protocol endpoints live in internal/http.
package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	tfaddr "github.com/hashicorp/terraform-registry-address"

	"github.com/opsolete/opsolete/internal/templates"
)

const (
	MaxArchiveSize   = 20 << 20  // compressed upload limit
	maxExtractedSize = 200 << 20 // guards against gzip bombs
	maxEntries       = 20000     // and against millions of empty files (inodes, CPU)
)

// Registry knows its public URL (whose host is the registry hostname) and the URL signing key.
type Registry struct {
	PublicURL *url.URL
	Key       []byte
}

// Address validates and normalises a module address with Terraform's own rules,
// returning the lowercase namespace, name and system plus the full source address.
func (r *Registry) Address(namespace, name, system string) (ns, n, sys, source string, err error) {
	raw := strings.ToLower(r.PublicURL.Host + "/" + namespace + "/" + name + "/" + system)
	m, err := tfaddr.ParseModuleSource(raw)
	if err != nil {
		return "", "", "", "", err
	}
	if m.Subdir != "" {
		return "", "", "", "", errors.New("namespace, name and system may not contain slashes")
	}
	p := m.Package
	return p.Namespace, p.Name, p.TargetSystem, p.String(), nil
}

// ParseSource splits a source address of this registry; ok is false for any other source (git, other registries).
func (r *Registry) ParseSource(source string) (ns, name, system string, ok bool) {
	m, err := tfaddr.ParseModuleSource(source)
	if err != nil || m.Subdir != "" || !strings.EqualFold(m.Package.Host.String(), r.PublicURL.Host) {
		return "", "", "", false
	}
	return m.Package.Namespace, m.Package.Name, m.Package.TargetSystem, true
}

// CanonicalVersion parses a semver ("v1.2.0" or "1.2.0") into its canonical form.
func CanonicalVersion(v string) (string, error) {
	sv, err := version.NewSemver(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("version must be semver like 1.2.0: %w", err)
	}
	return sv.String(), nil
}

// SortVersions sorts semver strings newest first; invalid ones go last.
func SortVersions(vs []string) {
	slices.SortFunc(vs, func(a, b string) int {
		va, ea := version.NewSemver(a)
		vb, eb := version.NewSemver(b)
		if ea != nil || eb != nil {
			return strings.Compare(b, a)
		}
		return vb.Compare(va)
	})
}

// ArchivePath is the path of a module archive. It ends in .tar.gz so Terraform's getter unpacks it.
func ArchivePath(ns, name, system, ver string) string {
	return fmt.Sprintf("/v1/archives/%s/%s/%s/%s.tar.gz", ns, name, system, ver)
}

func (r *Registry) sig(path string, exp int64) string {
	m := hmac.New(sha256.New, r.Key)
	fmt.Fprintf(m, "%s\n%d", path, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// SignedArchiveURL returns path?expires=..&sig=.., valid for ttl. Terraform does not send
// registry credentials to download URLs, so the signature is what authorises the fetch.
func (r *Registry) SignedArchiveURL(path string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return fmt.Sprintf("%s?expires=%d&sig=%s", path, exp, r.sig(path, exp))
}

func (r *Registry) VerifyArchiveURL(path, expires, sig string) bool {
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(r.sig(path, exp)))
}

// RunnerSource resolves a template's module for `terraform init -from-module`: source?ref=ref, or for
// this registry's modules a relative signed archive path that runners resolve against the URL
// they reach Opsolete on (in k8s, the internal service).
func (r *Registry) RunnerSource(m templates.Module, ttl time.Duration) string {
	if ns, name, sys, ok := r.ParseSource(m.Source); ok {
		return r.SignedArchiveURL(ArchivePath(ns, name, sys, m.Ref), ttl)
	}
	sep := "?"
	if strings.Contains(m.Source, "?") { // e.g. git::https://host/repo//dir?depth=1
		sep = "&"
	}
	return m.Source + sep + "ref=" + url.QueryEscape(m.Ref)
}

// Inspection is what Inspect learns from a module archive.
type Inspection struct {
	Module    *tfconfig.Module    // everything terraform-config-inspect reads: inputs, outputs, providers, resources...
	Variables []tfconfig.Variable // Module.Variables in source order, for form generation
	Readme    string              // README.md at the archive root, if any
}

const maxReadme = 256 << 10

// Inspect unpacks a .tar.gz module archive into a temp dir and reads it with
// terraform-config-inspect. The module must be at the archive root.
func Inspect(archive []byte) (Inspection, error) {
	dir, err := os.MkdirTemp("", "ops-module-*")
	if err != nil {
		return Inspection{}, err
	}
	defer os.RemoveAll(dir)
	if err := extract(archive, dir); err != nil {
		return Inspection{}, err
	}
	if !tfconfig.IsModuleDir(dir) {
		return Inspection{}, errors.New("no .tf files at the archive root (create it with: tar czf module.tar.gz -C <module-dir> .)")
	}
	mod, diags := tfconfig.LoadModule(dir)
	if diags.HasErrors() {
		return Inspection{}, fmt.Errorf("invalid module: %w", diags.Err())
	}
	// Don't persist temp paths; keep file names relative to the module root.
	mod.Path, mod.Diagnostics = "", nil
	rel := func(p *tfconfig.SourcePos) { p.Filename = filepath.Base(p.Filename) }
	for _, v := range mod.Variables {
		rel(&v.Pos)
	}
	for _, o := range mod.Outputs {
		rel(&o.Pos)
	}
	for _, r := range mod.ManagedResources {
		rel(&r.Pos)
	}
	for _, r := range mod.DataResources {
		rel(&r.Pos)
	}
	for _, c := range mod.ModuleCalls {
		rel(&c.Pos)
	}

	vars := make([]tfconfig.Variable, 0, len(mod.Variables))
	for _, v := range mod.Variables {
		vars = append(vars, *v)
	}
	// File order, so the generated form follows the module author's ordering.
	slices.SortFunc(vars, func(a, b tfconfig.Variable) int {
		if c := strings.Compare(a.Pos.Filename, b.Pos.Filename); c != 0 {
			return c
		}
		return a.Pos.Line - b.Pos.Line
	})

	var readme string
	if f, err := os.Open(filepath.Join(dir, "README.md")); err == nil {
		b, _ := io.ReadAll(io.LimitReader(f, maxReadme))
		f.Close()
		readme = string(b)
	}
	return Inspection{Module: mod, Variables: vars, Readme: readme}, nil
}

// extract unpacks regular files and directories only, rejecting paths that escape dir.
func extract(archive []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("not a .tar.gz archive: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if entries >= maxEntries {
			return fmt.Errorf("archive has more than %d entries", maxEntries)
		}
		name := filepath.Clean(h.Name)
		if !filepath.IsLocal(name) {
			if name == "." {
				continue
			}
			return fmt.Errorf("archive entry %q escapes the module directory", h.Name)
		}
		target := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxExtractedSize-total+1))
			total += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if total > maxExtractedSize {
				return errors.New("archive is too large when unpacked")
			}
		}
		// Symlinks and other entry types are skipped on purpose.
	}
}

// Inputs maps module variables to form inputs. Complex types (list, map, object, ...) can't be a
// simple form field yet; they are returned in skipped so the builder can warn about them.
func Inputs(vars []tfconfig.Variable) (inputs []templates.Input, skipped []string) {
	for _, v := range vars {
		in := templates.Input{Name: v.Name, Label: v.Description, Required: v.Required}
		switch v.Type {
		case "", "string":
			in.Type = "string"
		case "number":
			in.Type = "number"
		case "bool":
			in.Type = "bool"
		default:
			skipped = append(skipped, fmt.Sprintf("%s (%s)", v.Name, v.Type))
			continue
		}
		if in.Label == "" {
			in.Label = v.Name
		}
		if v.Default != nil {
			in.Default = fmt.Sprint(v.Default)
		}
		inputs = append(inputs, in)
	}
	return inputs, skipped
}

// Resolve makes a template's form match the module version it pins: one input per module variable,
// with name, type and required-ness from variables.tf. The template's JSON inputs only add a label,
// pattern, select options or default, and keep their order. JSON inputs the module doesn't declare are
// dropped. Sensitive variables appear only if the JSON lists them, because form inputs are stored unsealed.
func Resolve(overrides []templates.Input, vars []tfconfig.Variable) []templates.Input {
	gen, _ := Inputs(vars)
	byName := map[string]templates.Input{}
	for _, in := range gen {
		byName[in.Name] = in
	}
	var out []templates.Input
	for _, o := range overrides {
		in, ok := byName[o.Name]
		if !ok {
			continue
		}
		delete(byName, o.Name)
		if o.Type == "select" && in.Type == "string" {
			in.Type, in.Options = "select", o.Options
		}
		if o.Label != "" {
			in.Label = o.Label
		}
		if o.Default != "" {
			in.Default = o.Default
		}
		in.Pattern, in.Required = o.Pattern, in.Required || o.Required
		out = append(out, in)
	}
	for _, v := range vars {
		if in, ok := byName[v.Name]; ok && !v.Sensitive {
			out = append(out, in)
		}
	}
	return out
}
