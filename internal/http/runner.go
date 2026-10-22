package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/templates"
)

// claimWait is the long-poll window, below typical proxy idle timeouts (a var for tests).
var claimWait = 20 * time.Second

const (
	leaseTTL     = 2 * time.Minute  // runners heartbeat well within this
	maxLogBytes  = 10 << 20         // per deployment
	maxLogChunk  = 256 << 10        // per request
	runnerSrcTTL = 15 * time.Minute // signed archive URL handed to the runner; it downloads right away
)

// Runner API: runners pull work over HTTPS with a runner-scoped token, so they need no
// database access and no inbound connectivity.
func (s *Server) runnerRoutes(mux *http.ServeMux) {
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/claim", s.requireToken(storage.ScopeRunner, s.claimJob))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/logs", s.requireToken(storage.ScopeRunner, s.jobLogs))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/heartbeat", s.requireToken(storage.ScopeRunner, s.jobHeartbeat))
	mux.Handle("POST "+runnerapi.Prefix+"/jobs/{id}/finish", s.requireToken(storage.ScopeRunner, s.jobFinish))
	mux.HandleFunc("GET /deployments/{id}/logs", s.logText)
	mux.HandleFunc("GET /deployments/{id}/logs/stream", s.logStream)
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
			s.Log.Info("job claimed", "id", j.DeploymentID, "action", j.Action, "runner", req.Runner)
			writeJSON(w, runnerapi.Job{
				DeploymentID: j.DeploymentID, LeaseID: j.LeaseID, StateKey: j.StateKey, Action: j.Action, Vars: j.Inputs,
				LeaseSeconds: int(leaseTTL.Seconds()),
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
	s.leaseResult(w, s.Store.AppendLog(r.Context(), r.PathValue("id"), r.Header.Get(runnerapi.LeaseHeader), strings.ToValidUTF8(string(b), "\uFFFD"), leaseTTL, maxLogBytes))
}

func (s *Server) jobHeartbeat(w http.ResponseWriter, r *http.Request) {
	s.leaseResult(w, s.Store.Heartbeat(r.Context(), r.PathValue("id"), r.Header.Get(runnerapi.LeaseHeader), leaseTTL))
}

func (s *Server) jobFinish(w http.ResponseWriter, r *http.Request) {
	var req runnerapi.FinishRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		(req.Status != "succeeded" && req.Status != "failed" && req.Status != "canceled") {
		writeErrors(w, http.StatusBadRequest, "status must be succeeded, failed or canceled")
		return
	}
	id := r.PathValue("id")
	err := s.Store.FinishJob(r.Context(), id, r.Header.Get(runnerapi.LeaseHeader), req.Status, req.Message, req.Outputs)
	if err == nil {
		s.Log.Info("job finished", "id", id, "status", req.Status)
	}
	s.leaseResult(w, err)
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

func (s *Server) logText(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, err := s.Store.GetDeployment(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.Log.Error("get deployment", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var after int64
	for {
		chunks, err := s.Store.Logs(r.Context(), id, after, 500)
		if err != nil {
			s.Log.Error("read logs", "err", err)
			return
		}
		for _, c := range chunks {
			io.WriteString(w, c.Chunk)
			after = c.Seq
		}
		if len(chunks) < 500 {
			return
		}
	}
}

// logStream follows a deployment's log as Server-Sent Events. Event ids are log sequence numbers,
// so a reconnecting EventSource resumes via Last-Event-ID. Ends with a "done" event once the
// deployment is finished and the log is drained.
// shortcut: polls Postgres every second; switch to LISTEN/NOTIFY if many viewers make that costly.
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	if _, _, err := s.Store.GetDeployment(ctx, id); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.Log.Error("get deployment", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cursor, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // long-lived response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // stop nginx-style ingresses from buffering the stream
	w.WriteHeader(http.StatusOK)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	idle := 0
	for {
		chunks, err := s.Store.Logs(ctx, id, cursor, 200)
		if err != nil {
			return
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "id: %d\n", c.Seq)
			for _, line := range strings.Split(c.Chunk, "\n") {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			io.WriteString(w, "\n")
			cursor = c.Seq
		}
		if len(chunks) == 0 {
			d, _, err := s.Store.GetDeployment(ctx, id)
			if err != nil {
				return
			}
			if d.Status != "queued" && d.Status != "running" {
				fmt.Fprintf(w, "event: done\ndata: %s\n\n", d.Status)
				rc.Flush()
				return
			}
			if idle++; idle%15 == 0 {
				io.WriteString(w, ": keepalive\n\n")
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
		if len(chunks) == 200 {
			continue // more backlog, don't wait
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
