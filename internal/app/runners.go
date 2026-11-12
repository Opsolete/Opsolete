package app

import (
	"cmp"
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
)

// Runners: each has its own token and a pool; a project's jobs go only to runners of its pool
// (ProjectSettings.RunnerPool). Admins manage them; the runner API authenticates with RunnerByToken.

// MinRunnerToken is the shortest token an operator may register (opsolete runner-token -from-stdin).
const MinRunnerToken = 32

// RunnerByToken is the runner token belongs to. ErrNotFound: none (unknown or revoked).
func (a *App) RunnerByToken(ctx context.Context, token string) (storage.Runner, error) {
	if token == "" {
		return storage.Runner{}, ErrNotFound
	}
	return a.Store.RunnerByToken(ctx, token)
}

// Runners lists the registered runners. Admins only.
func (a *App) Runners(ctx context.Context, actor Actor) ([]storage.Runner, error) {
	if !actor.Admin {
		return nil, errAdminsOnly
	}
	return a.Store.Runners(ctx)
}

// RunnerPools lists the pools in use and the default one, for the project's pool picker. Admins only.
func (a *App) RunnerPools(ctx context.Context, actor Actor) ([]string, error) {
	if !actor.Admin {
		return nil, errAdminsOnly
	}
	return a.Store.RunnerPools(ctx)
}

// checkRunner trims and checks a runner's name and pool ("": storage.DefaultPool).
func checkRunner(name, pool string) (string, string, error) {
	name, pool = strings.TrimSpace(name), cmp.Or(strings.TrimSpace(pool), storage.DefaultPool)
	if !ValidName(name) {
		return "", "", invalid("name", "a runner name is "+NameRule)
	}
	if !ValidName(pool) {
		return "", "", invalid("pool", "a runner pool is "+NameRule)
	}
	return name, pool, nil
}

// CreateRunner registers a runner in pool and returns its new token, shown once. Admins only (or a system actor:
// the CLI); ValidationError: name, pool, or the name is taken.
func (a *App) CreateRunner(ctx context.Context, actor Actor, name, pool string) (string, error) {
	if !actor.Admin {
		return "", errAdminsOnly
	}
	name, pool, err := checkRunner(name, pool)
	if err != nil {
		return "", err
	}
	token, err := a.Store.CreateRunner(ctx, name, pool)
	if errors.Is(err, storage.ErrExists) {
		return "", invalid("name", "a runner named "+name+" exists: delete it first to give it a new token")
	}
	if err == nil {
		a.Log.Info("runner created", "runner", name, "pool", pool, "user", actor.Email)
	}
	return token, err
}

// RegisterRunner registers runner name in pool with a token the operator generated (at least MinRunnerToken
// characters), replacing the token and pool it had: deployments that keep the token in their own secret store.
// Admins only (or a system actor: the CLI).
func (a *App) RegisterRunner(ctx context.Context, actor Actor, name, pool, token string) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	name, pool, err := checkRunner(name, pool)
	if err != nil {
		return err
	}
	if len(token) < MinRunnerToken {
		return invalid("token", "a runner token is at least 32 characters (e.g. openssl rand -hex 32)")
	}
	if err := a.Store.RegisterRunner(ctx, name, pool, token); errors.Is(err, storage.ErrExists) {
		return invalid("token", "another runner has this token")
	} else if err != nil {
		return err
	}
	a.Log.Info("runner registered", "runner", name, "pool", pool, "user", actor.Email)
	return nil
}

// DeleteRunner revokes a runner's token: its next request is refused, and a job it holds fails when its lease
// runs out. Admins only.
func (a *App) DeleteRunner(ctx context.Context, actor Actor, id uuid.UUID) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	name, err := a.Store.DeleteRunner(ctx, id)
	if err == nil {
		a.Log.Info("runner deleted", "runner", name, "user", actor.Email)
	}
	return err
}
