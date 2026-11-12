package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

const maxLogChunk = 256 << 10 // per request

// Runner API: runners pull work over HTTPS with their own token (app.RunnerByToken), so they need no
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

// runnerKey is the context key of the runner a request authenticated as (requireRunner).
type runnerKey struct{}

// requireRunner admits requests bearing a registered runner's token (opsolete runner-token, or the
// Runners settings page); the handler finds the runner with runnerOf.
func (s *Server) requireRunner(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		runner, err := s.App.RunnerByToken(r.Context(), token)
		if errors.Is(err, app.ErrNotFound) {
			writeErrors(w, http.StatusUnauthorized, "missing or invalid runner token")
			return
		}
		if err != nil {
			s.runnerError(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), runnerKey{}, runner)))
	})
}

func runnerOf(r *http.Request) storage.Runner {
	runner, _ := r.Context().Value(runnerKey{}).(storage.Runner)
	return runner
}

func (s *Server) claimJob(w http.ResponseWriter, r *http.Request) {
	wait, cancel := context.WithTimeout(r.Context(), s.ClaimWait)
	defer cancel()
	// on shutdown: 204, the runner claims again elsewhere
	defer context.AfterFunc(s.Stopping, cancel)()
	jobs := s.App.Notify.Jobs() // before the first try: a job queued during it still wakes us
	defer jobs.Close()
	for {
		body, ok, err := s.App.ClaimJob(r.Context(), runnerOf(r)) // the name it was registered with, not the one it sends
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
