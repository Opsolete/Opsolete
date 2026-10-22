package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gocloud.dev/gcerrors"

	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/templates"
)

// claimWait is the long-poll window, below typical proxy idle timeouts (a var for tests).
var claimWait = 20 * time.Second

const (
	leaseTTL        = 2 * time.Minute  // runners heartbeat well within this
	maxLogBytes     = 10 << 20         // per run
	maxLogChunk     = 256 << 10        // per request
	maxArtifactSize = 256 << 20        // saved plans embed the configuration; generous
	runnerSrcTTL    = 15 * time.Minute // signed archive URL handed to the runner; it downloads right away
)

// Runner API: runners pull work over HTTPS with a runner-scoped token, so they need no
// database access and no inbound connectivity.
func (s *Server) runnerRoutes(mux *http.ServeMux) {
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/claim", s.requireToken(storage.ScopeRunner, s.claimJob))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/logs", s.requireToken(storage.ScopeRunner, s.jobLogs))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/heartbeat", s.requireToken(storage.ScopeRunner, s.jobHeartbeat))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/finish", s.requireToken(storage.ScopeRunner, s.jobFinish))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/artifacts/{name}", s.requireToken(storage.ScopeRunner, s.putArtifact))
	mux.Handle("GET "+runnerapi.Prefix+"/jobs/{id}/artifacts/{name}", s.requireToken(storage.ScopeRunner, s.getArtifact))
}

func (s *Server) claimJob(w http.ResponseWriter, r *http.Request) {
	var req runnerapi.ClaimRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.Runner) == "" {
		writeErrors(w, http.StatusBadRequest, `body must be {"runner": "<name>"}`)
		return
	}
	deadline := time.After(claimWait)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		j, ok, err := s.Store.ClaimJob(r.Context(), req.Runner, leaseTTL)
		if err != nil {
			s.Log.Error("claim job", "err", err)
			writeErrors(w, http.StatusInternalServerError, "internal error")
			return
		}
		if ok {
			env, vars, err := s.runVariables(r.Context(), j.WorkspaceID, j.Inputs)
			if err != nil {
				// The job is claimed: fail it rather than leave it to the lease reaper.
				s.Log.Error("job variables", "run", j.RunID, "err", err)
				if _, ferr := s.Store.FinishJob(r.Context(), j.RunID, j.LeaseID, "failed", err.Error(), nil, nil, nil); ferr != nil {
					s.Log.Error("fail job", "run", j.RunID, "err", ferr)
				}
				continue
			}
			s.Log.Info("job claimed", "run", j.RunID, "phase", j.Phase, "action", j.Action, "runner", req.Runner)
			writeJSON(w, runnerapi.Job{
				RunID: j.RunID, WorkspaceID: j.WorkspaceID, Phase: j.Phase, Action: j.Action, LeaseID: j.LeaseID,
				LeaseSeconds: int(leaseTTL.Seconds()), StateToken: s.stateToken(j.WorkspaceID, j.RunID), Vars: vars, Env: env,
				ModuleSource: s.Registry.RunnerSource(templates.Module{Source: j.ModuleSource, Ref: j.ModuleRef}, runnerSrcTTL),
			})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-tick.C:
		}
	}
}

func (s *Server) jobLogs(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogChunk))
	if err != nil {
		writeErrors(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("log chunks are limited to %d KB", maxLogChunk>>10))
		return
	}
	cancel, err := s.Store.AppendLog(r.Context(), r.PathValue("id"), r.Header.Get(runnerapi.LeaseHeader), strings.ToValidUTF8(string(b), "\uFFFD"), leaseTTL, maxLogBytes)
	s.liveResult(w, cancel, err)
}

func (s *Server) jobHeartbeat(w http.ResponseWriter, r *http.Request) {
	cancel, err := s.Store.Heartbeat(r.Context(), r.PathValue("id"), r.Header.Get(runnerapi.LeaseHeader), leaseTTL)
	s.liveResult(w, cancel, err)
}

// liveResult answers a log post or heartbeat: 202 asks the runner to cancel gracefully (it keeps
// its lease, so Terraform can still save state), 409 means the lease is gone.
func (s *Server) liveResult(w http.ResponseWriter, cancel bool, err error) {
	if err == nil && cancel {
		w.WriteHeader(runnerapi.StatusCancel)
		return
	}
	s.leaseResult(w, err)
}

func (s *Server) jobFinish(w http.ResponseWriter, r *http.Request) {
	var req runnerapi.FinishRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil ||
		(req.Status != "succeeded" && req.Status != "failed" && req.Status != "canceled") {
		writeErrors(w, http.StatusBadRequest, "status must be succeeded, failed or canceled")
		return
	}
	id := r.PathValue("id")
	status, err := s.Store.FinishJob(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader), req.Status, req.Message,
		req.Outputs, req.Plan, req.Resources)
	if err == nil {
		s.Log.Info("job finished", "run", id, "result", req.Status, "status", status)
		if status != storage.NeedsConfirmation && status != storage.ApplyQueued {
			s.deleteArtifacts(r, id) // the saved plan has been applied, or will never be
		}
	}
	s.leaseResult(w, err)
}

// Saved plans and lock files live in the state store: a plan holds variable values and is as
// sensitive as state. They exist from the plan phase until the run ends.
func artifactKey(runID, name string) string { return "runs/" + runID + "/" + name }

// putArtifact stores a plan phase's output; only the planning run may write its own artifacts.
func (s *Server) putArtifact(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if !s.artifactAccess(w, r, storage.Planning) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxArtifactSize))
	if err != nil {
		writeErrors(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("artifacts are limited to %d MB", maxArtifactSize>>20))
		return
	}
	if err := s.StateStore.WriteAll(r.Context(), artifactKey(id, name), body, nil); err != nil {
		s.Log.Error("write artifact", "err", err)
		writeErrors(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getArtifact hands the saved plan to the run's apply phase.
func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.artifactAccess(w, r, storage.Applying) {
		return
	}
	b, err := s.StateStore.ReadAll(r.Context(), artifactKey(r.PathValue("id"), r.PathValue("name")))
	if gcerrors.Code(err) == gcerrors.NotFound {
		writeErrors(w, http.StatusNotFound, "no such artifact")
		return
	}
	if err != nil {
		s.Log.Error("read artifact", "err", err)
		writeErrors(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

func (s *Server) artifactAccess(w http.ResponseWriter, r *http.Request, phase string) bool {
	if name := r.PathValue("name"); name != runnerapi.ArtifactPlan && name != runnerapi.ArtifactLockfile {
		writeErrors(w, http.StatusNotFound, "no such artifact")
		return false
	}
	ok, err := s.Store.HoldsLease(r.Context(), r.PathValue("id"), r.Header.Get(runnerapi.LeaseHeader), phase)
	if err != nil {
		s.Log.Error("check lease", "err", err)
		writeErrors(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if !ok {
		writeErrors(w, http.StatusConflict, "job lease lost")
	}
	return ok
}

// deleteArtifacts removes a run's saved plan; failures only leave a stale file behind.
func (s *Server) deleteArtifacts(r *http.Request, runID string) {
	for _, name := range []string{runnerapi.ArtifactPlan, runnerapi.ArtifactLockfile} {
		if err := s.StateStore.Delete(r.Context(), artifactKey(runID, name)); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			s.Log.Warn("delete artifact", "run", runID, "name", name, "err", err)
		}
	}
}

// leaseResult answers 409 when the runner lost its lease, so it stops working on the job.
func (s *Server) leaseResult(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrLeaseLost):
		writeErrors(w, http.StatusConflict, "job lease lost")
	case err != nil:
		s.Log.Error("runner job update", "err", err)
		writeErrors(w, http.StatusInternalServerError, "internal error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
