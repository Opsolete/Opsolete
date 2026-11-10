package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// The module registry: every signed-in user reads it, Terraform reads it with a registry or user
// token (ValidToken) and downloads archives through signed URLs; admins upload (when uploads are on),
// publish from VCS (vcsmodules.go) and delete modules.

var (
	errUploadsOff      = reason{ErrForbidden, "module uploads are off (Settings → Organization)"}
	errBadArchiveLink  = reason{ErrForbidden, "invalid or expired download link"}
	errNoModule        = reason{ErrNotFound, "module not found"}
	errNoModuleVersion = reason{ErrNotFound, "module version not found"}
)

// ValidToken reports whether token is a token of scope (storage.ValidToken).
func (a *App) ValidToken(ctx context.Context, token, scope string) (bool, error) {
	return a.Store.ValidToken(ctx, token, scope)
}

// Modules lists every module version, ordered by module.
func (a *App) Modules(ctx context.Context) ([]storage.Module, error) { return a.Store.ListModules(ctx) }

// ModuleVersions lists a module's versions, newest first; ErrNotFound if it has none.
func (a *App) ModuleVersions(ctx context.Context, ns, name, sys string) ([]string, error) {
	vs, err := a.Store.ModuleVersions(ctx, ns, name, sys)
	if err == nil && len(vs) == 0 {
		err = errNoModule
	}
	registry.SortVersions(vs)
	return vs, err
}

// ModuleDownload returns the signed archive URL of a module version, valid for 10 minutes, relative:
// Terraform resolves it against the download endpoint's URL. ErrNotFound for an unknown version.
func (a *App) ModuleDownload(ctx context.Context, ns, name, sys, version string) (string, error) {
	if _, err := a.Store.GetModule(ctx, ns, name, sys, version); errors.Is(err, storage.ErrNotFound) {
		return "", errNoModuleVersion
	} else if err != nil {
		return "", err
	}
	return a.Registry.SignedArchiveURL(registry.ArchivePath(ns, name, sys, version), 10*time.Minute), nil
}

// ModuleArchive reads a module version's archive through its signed URL (path, expires, sig; the
// version's address is read from path):
// ErrForbidden for an invalid or expired link, ErrNotFound for an unknown version.
func (a *App) ModuleArchive(ctx context.Context, path, expires, sig, ns, name, sys, version string) ([]byte, error) {
	if !a.Registry.VerifyArchiveURL(path, expires, sig) {
		return nil, errBadArchiveLink
	}
	return a.Store.ModuleArchive(ctx, ns, name, sys, version)
}

// ModuleDocs is a module version's page: its documentation, every version, its source address, its
// VCS link (nil if none or VCS is off) and the catalog templates that deploy it (any version).
type ModuleDocs struct {
	Docs      storage.ModuleDocs
	Versions  []string // newest first
	Source    string
	Linked    *storage.VCSModule
	Templates []templates.Template
}

// ModuleDocs loads a module version's page; ErrNotFound for an unknown version.
func (a *App) ModuleDocs(ctx context.Context, ns, name, sys, version string) (ModuleDocs, error) {
	var d ModuleDocs
	var err error
	if d.Docs, err = a.Store.GetModuleDocs(ctx, ns, name, sys, version); err != nil {
		return d, err
	}
	if d.Versions, err = a.ModuleVersions(ctx, ns, name, sys); err != nil {
		return d, err
	}
	if _, _, _, d.Source, err = a.Registry.Address(ns, name, sys); err != nil {
		return d, err
	}
	if a.VCSEnabled {
		if d.Linked, err = a.VCSModule(ctx, ns, name, sys); err != nil {
			return d, err
		}
	}
	for _, t := range a.Templates.List() {
		if t.Module.Source == d.Source {
			d.Templates = append(d.Templates, t)
		}
	}
	return d, nil
}

// ModuleResources lists the managed resources (type.name, sorted) of the registry module version m points
// at, as the builder's suggestions for adopt addresses. nil for other sources or on any error: only hints.
func (a *App) ModuleResources(ctx context.Context, m templates.Module) []string {
	ns, name, sys, ok := a.Registry.ParseSource(m.Source)
	if !ok {
		return nil
	}
	d, err := a.Store.GetModuleDocs(ctx, ns, name, sys, m.Ref)
	if err != nil || d.Module == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(d.Module.ManagedResources))
}

// CanUpload: actor is an admin and module uploads are on (the organization setting, or TemplateEditor).
func (a *App) CanUpload(ctx context.Context, actor Actor) (bool, error) {
	if !actor.Admin {
		return false, nil
	}
	if a.TemplateEditor {
		return true, nil
	}
	return a.Store.ModuleUploads(ctx)
}

// ModuleUpload is an uploaded module version: its address as typed and its .tar.gz archive.
type ModuleUpload struct {
	Namespace, Name, System, Version string
	Archive                          []byte // nil: none chosen
}

// UploadModule publishes a module version from its archive, with the variables and documentation
// read from it. It returns the stored version (canonical address and version). Needs CanUpload
// (admins, uploads on); ValidationError: address, version (also when it exists: versions are
// immutable), archive.
func (a *App) UploadModule(ctx context.Context, actor Actor, in ModuleUpload) (storage.Module, error) {
	var m storage.Module
	if !actor.Admin {
		return m, errAdminsOnly
	}
	ok, err := a.CanUpload(ctx, actor)
	if err != nil {
		return m, err
	}
	if !ok {
		return m, errUploadsOff
	}
	if m.Namespace, m.Name, m.System, _, err = a.Registry.Address(in.Namespace, in.Name, in.System); err != nil {
		return m, invalid("name", err.Error())
	}
	if m.Version, err = registry.CanonicalVersion(in.Version); err != nil {
		return m, invalid("version", err.Error())
	}
	switch {
	case in.Archive == nil:
		return m, invalid("archive", "choose a .tar.gz module archive")
	case len(in.Archive) > registry.MaxArchiveSize:
		return m, invalid("archive", fmt.Sprintf("archive larger than %d MB", registry.MaxArchiveSize>>20))
	}
	ins, err := registry.Inspect(in.Archive)
	if err != nil {
		return m, invalid("archive", err.Error())
	}
	m.Variables = ins.Variables
	err = a.Store.CreateModule(ctx, m, storage.ModuleDocs{Module: ins.Module, Readme: ins.Readme}, in.Archive)
	if errors.Is(err, storage.ErrExists) {
		return m, invalid("version", fmt.Sprintf("%s/%s/%s %s already exists; versions are immutable, upload a new version", m.Namespace, m.Name, m.System, m.Version))
	}
	if err != nil {
		return m, err
	}
	a.Log.Info("module uploaded", "module", m.Namespace+"/"+m.Name+"/"+m.System, "version", m.Version, "variables", len(ins.Variables), "user", actor.Email)
	return m, nil
}

// DeleteModule removes every version of a module, unless a catalog template still deploys it (a
// conflict). Workspaces already using it keep their state but can't download it again. Admins only;
// ValidationError: the address; ErrNotFound.
func (a *App) DeleteModule(ctx context.Context, actor Actor, ns, name, sys string) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	ns, name, sys, _, err := a.Registry.Address(ns, name, sys)
	if err != nil {
		return invalid("name", err.Error())
	}
	for _, t := range a.Templates.List() {
		if tns, tname, tsys, ok := a.Registry.ParseSource(t.Module.Source); ok && tns == ns && tname == name && tsys == sys {
			return reason{storage.ErrInUse, fmt.Sprintf("template %q (%s) still uses this module; delete or repoint it first", t.Name, t.ID)}
		}
	}
	if err := a.Store.DeleteModule(ctx, ns, name, sys); err != nil {
		return err
	}
	a.Log.Info("module deleted", "module", ns+"/"+name+"/"+sys, "user", actor.Email)
	return nil
}
