package app

import (
	"context"

	"github.com/opsolete/opsolete/internal/storage"
)

// Organization settings and the audit log: admins only. Stats and Ready are for monitoring.

// Organization are the organization's settings.
type Organization struct {
	Currency      string // of cost estimates: USD or EUR
	ModuleUploads bool   // admins may upload registry modules (see CanUpload)
}

// Organization loads the settings. Admins only.
func (a *App) Organization(ctx context.Context, actor Actor) (Organization, error) {
	var o Organization
	if !actor.Admin {
		return o, errAdminsOnly
	}
	var err error
	if o.Currency, err = a.Store.Currency(ctx); err != nil {
		return o, err
	}
	o.ModuleUploads, err = a.Store.ModuleUploads(ctx)
	return o, err
}

// SetCurrency sets the currency of cost estimates. Admins only; ValidationError: not USD or EUR.
func (a *App) SetCurrency(ctx context.Context, actor Actor, currency string) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	if currency != "USD" && currency != "EUR" {
		return invalid("currency", "currency must be USD or EUR")
	}
	if err := a.Store.SetCurrency(ctx, currency); err != nil {
		return err
	}
	a.Log.Info("currency set", "currency", currency, "user", actor.Email)
	return nil
}

// SetModuleUploads turns module uploads on or off. Admins only.
func (a *App) SetModuleUploads(ctx context.Context, actor Actor, on bool) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	if err := a.Store.SetModuleUploads(ctx, on); err != nil {
		return err
	}
	a.Log.Info("module uploads set", "on", on, "user", actor.Email)
	return nil
}

// Audit records an action in the audit log, by who (an email, or "anonymous").
func (a *App) Audit(ctx context.Context, who, action string, detail any) error {
	return a.Store.Audit(ctx, who, action, detail)
}

// AuditLog lists the newest 500 entries (access: only the access entries: sign-ins and refused
// requests) and how many entries break the log's hash chain. Admins only.
func (a *App) AuditLog(ctx context.Context, actor Actor, access bool) (entries []storage.AuditEntry, broken int64, err error) {
	if !actor.Admin {
		return nil, 0, errAdminsOnly
	}
	if entries, err = a.Store.AuditLog(ctx, 500, access); err != nil {
		return nil, 0, err
	}
	broken, err = a.Store.VerifyAuditLog(ctx)
	return entries, broken, err
}

// Stats are the run and workspace counts for metrics, the same on every replica.
func (a *App) Stats(ctx context.Context) (storage.Stats, error) { return a.Store.Stats(ctx) }

// Ready reports whether the database answers (the readiness probe).
func (a *App) Ready(ctx context.Context) error { return a.Store.Pool.Ping(ctx) }
