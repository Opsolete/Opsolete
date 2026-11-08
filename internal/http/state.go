package httpserver

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// Terraform http state backend, one state per workspace (deployment):
// https://developer.hashicorp.com/terraform/language/backend/http
// Runners authenticate with per-run credentials (username = workspace id, password = app.StateToken),
// so only the run a runner is working on can read or write the workspace's state, and only while it
// is planning or applying. A run of another workspace may read it (terraform_remote_state, its own
// credentials) if the workspace shares its state with that one (app.StateRun). Errors are plain
// text, as on the UI (uiError); 423 with the holder's LockInfo for a held lock.
func (s *Server) stateRoutes(mux routes) {
	p := runnerapi.StatePrefix + "/{id}"
	mux.HandleFunc("GET "+p, s.stateAuth(s.getState))
	mux.HandleFunc("POST "+p, s.stateAuth(s.putState))
	mux.HandleFunc("LOCK "+p, s.stateAuth(s.lockState))
	mux.HandleFunc("UNLOCK "+p, s.stateAuth(s.unlockState))
}

func (s *Server) stateAuth(next func(http.ResponseWriter, *http.Request, app.StateRun)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, pass, ok := r.BasicAuth()
		if !ok {
			http.Error(w, "state credentials required", http.StatusUnauthorized)
			return
		}
		id, _ := pathID(r, "id") // not an id: uuid.Nil, no run's own state and shared by none
		run, err := s.App.StateRun(r.Context(), id, username, pass)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		next(w, r, run)
	}
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request, run app.StateRun) {
	b, err := run.Read(r.Context())
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	if b == nil {
		w.WriteHeader(http.StatusNoContent) // new workspace: no state yet
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) putState(w http.ResponseWriter, r *http.Request, run app.StateRun) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, app.MaxStateSize))
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		http.Error(w, fmt.Sprintf("state larger than %d MB", app.MaxStateSize>>20), http.StatusRequestEntityTooLarge)
		return
	} else if err != nil {
		http.Error(w, "reading the state failed", http.StatusBadRequest)
		return
	}
	sum := md5.Sum(body)
	if h := r.Header.Get("Content-MD5"); h != "" && h != base64.StdEncoding.EncodeToString(sum[:]) {
		http.Error(w, "Content-MD5 mismatch", http.StatusBadRequest)
		return
	}
	if err := run.Write(r.Context(), r.URL.Query().Get("ID"), body); err != nil {
		s.uiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) lockState(w http.ResponseWriter, r *http.Request, run app.StateRun) {
	info, ok := readLockInfo(w, r)
	if !ok {
		return
	}
	held, ok, err := run.Lock(r.Context(), info)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	if !ok { // Terraform shows who holds the lock from this body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		w.Write(held)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) unlockState(w http.ResponseWriter, r *http.Request, run app.StateRun) {
	info, ok := readLockInfo(w, r)
	if !ok {
		return
	}
	if err := run.Unlock(r.Context(), info); err != nil {
		s.uiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// readLockInfo reads the body, Terraform's LockInfo JSON (app checks it).
func readLockInfo(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "body must be Terraform lock info with an ID", http.StatusBadRequest)
		return nil, false
	}
	return bytes.TrimSpace(b), true
}
