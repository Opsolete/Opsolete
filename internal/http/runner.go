package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

const maxLogChunk = 256 << 10 // per request

// Runner API: runners pull work over HTTPS with the shared RUNNER_TOKEN, so they need no
// database access and no inbound connectivity. The protocol's rules are app's (jobs.go).
func (s *Server) runnerRoutes(mux routes) {
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/claim", s.requireRunner(s.claimJob))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/ack", s.requireRunner(s.jobAck))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/logs", s.requireRunner(s.jobLogs))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/heartbeat", s.requireRunner(s.jobHeartbeat))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/finish", s.requireRunner(s.jobFinish))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/artifacts/{name}", s.requireRunner(s.putArtifact))
	mux.Handle("GET "+runnerapi.Prefix+"/jobs/{id}/artifacts/{name}", s.requireRunner(s.getArtifact))
}

// requireRunner admits requests bearing RUNNER_TOKEN, which the operator generates and gives to
// both the server and the runners, so a fresh deployment needs no token-creation step.
func (s *Server) requireRunner(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.RunnerToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.RunnerToken)) != 1 {
			writeErrors(w, http.StatusUnauthorized, "missing or invalid runner token")
			return
		}
		next(w, r)
	})
}

func (s *Server) claimJob(w http.ResponseWriter, r *http.Request) {
	var req runnerapi.ClaimRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.Runner) == "" {
		writeErrors(w, http.StatusBadRequest, `body must be {"runner": "<name>"}`)
		return
	}
	wait, cancel := context.WithTimeout(r.Context(), s.ClaimWait)
	defer cancel()
	jobs := s.App.Notify.Jobs() // before the first try: a job queued during it still wakes us
	defer jobs.Close()
	for {
		body, ok, err := s.App.ClaimJob(r.Context(), req.Runner)
		if err != nil {
			s.runnerError(w, r, err)
			return
		}
		if ok {
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
			return
		}
		if !jobs.Wait(wait) {
			if r.Context().Err() == nil { // ClaimWait is over, the runner is still there
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
	}
}

func (s *Server) jobAck(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: no such lease, as for an unknown run
	if err := s.App.AckJob(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader)); err != nil {
		s.runnerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobLogs(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogChunk))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeErrors(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("log chunks are limited to %d KB", maxLogChunk>>10))
		return
	} else if err != nil {
		writeErrors(w, http.StatusBadRequest, "reading the log chunk failed")
		return
	}
	id, _ := pathID(r, "id") // not an id: no such lease, as for an unknown run
	cancel, err := s.App.AppendLog(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader), b)
	s.liveResult(w, r, cancel, err)
}

func (s *Server) jobHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: no such lease, as for an unknown run
	cancel, err := s.App.Heartbeat(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader))
	s.liveResult(w, r, cancel, err)
}

// liveResult answers a log post or heartbeat: 202 asks the runner to cancel gracefully (it keeps
// its lease, so Terraform can still save state), 409 means the lease is gone.
func (s *Server) liveResult(w http.ResponseWriter, r *http.Request, cancel bool, err error) {
	switch {
	case err != nil:
		s.runnerError(w, r, err)
	case cancel:
		w.WriteHeader(runnerapi.StatusCancel)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) jobFinish(w http.ResponseWriter, r *http.Request) {
	var req runnerapi.FinishRequest
	// outputs and the resource index of a large state can be big: the same limit as a job
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, runnerapi.MaxJobSize)).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, fmt.Sprintf("invalid finish request (at most %d MB): %v", runnerapi.MaxJobSize>>20, err))
		return
	}
	id, _ := pathID(r, "id") // not an id: no such lease, as for an unknown run
	if _, err := s.App.FinishJob(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader), req); err != nil {
		s.runnerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// putArtifact stores a plan phase's output; only the planning run may write its own artifacts.
func (s *Server) putArtifact(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, app.MaxArtifactSize))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeErrors(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("artifacts are limited to %d MB", app.MaxArtifactSize>>20))
		return
	} else if err != nil {
		writeErrors(w, http.StatusBadRequest, "reading the artifact failed")
		return
	}
	id, _ := pathID(r, "id") // not an id: no such lease, as for an unknown run
	if err := s.App.PutArtifact(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader), r.PathValue("name"), body); err != nil {
		s.runnerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getArtifact hands the saved plan to the run's apply phase.
func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: no such lease, as for an unknown run
	b, err := s.App.Artifact(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader), r.PathValue("name"))
	if err != nil {
		s.runnerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}
