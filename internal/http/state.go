package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gocloud.dev/blob"

	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
)

const maxStateSize = 64 << 20

// Terraform http state backend, one state per workspace (deployment):
// https://developer.hashicorp.com/terraform/language/backend/http
// Runners authenticate with per-run credentials (username = workspace id, password = stateToken),
// so only the run a runner is working on can read or write the workspace's state, and only while it
// is planning or applying.
func (s *Server) stateRoutes(mux *http.ServeMux) {
	p := runnerapi.StatePrefix + "/{id}"
	mux.HandleFunc("GET "+p, s.stateAuth(s.getState))
	mux.HandleFunc("POST "+p, s.stateAuth(s.putState))
	mux.HandleFunc("LOCK "+p, s.stateAuth(s.lockState))
	mux.HandleFunc("UNLOCK "+p, s.stateAuth(s.unlockState))
}

// stateToken is a run's state password. It is per run, not per phase: Terraform embeds the backend
// config in the saved plan, so the apply phase authenticates with what the plan phase used.
func (s *Server) stateToken(workspaceID, runID string) string {
	m := hmac.New(sha256.New, s.Registry.Key)
	m.Write([]byte("form2stack state\x00" + workspaceID + "\x00" + runID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) stateAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		user, pass, ok := r.BasicAuth()
		if !ok || user != id {
			http.Error(w, "state credentials required", http.StatusUnauthorized)
			return
		}
		run, err := s.Store.WorkingRun(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no run is working on this workspace", http.StatusForbidden)
			return
		}
		if err != nil {
			s.Log.Error("state auth", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !hmac.Equal([]byte(pass), []byte(s.stateToken(id, run))) {
			http.Error(w, "invalid state credentials", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Store.StateVersions(r.Context(), r.PathValue("id"), 1)
	if err != nil {
		s.stateError(w, "list state versions", err)
		return
	}
	if len(vs) == 0 {
		w.WriteHeader(http.StatusNoContent) // new workspace: no state yet
		return
	}
	b, err := s.StateStore.ReadAll(r.Context(), vs[0].BlobKey)
	if err != nil {
		s.stateError(w, "read state", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) putState(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	lockID, err := s.Store.StateLockID(ctx, id)
	if err != nil {
		s.stateError(w, "state lock", err)
		return
	}
	if lockID == "" || r.URL.Query().Get("ID") != lockID {
		http.Error(w, "the state must be locked by this run", http.StatusConflict)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStateSize))
	if err != nil {
		http.Error(w, fmt.Sprintf("state larger than %d MB", maxStateSize>>20), http.StatusRequestEntityTooLarge)
		return
	}
	sum := md5.Sum(body)
	if h := r.Header.Get("Content-MD5"); h != "" && h != base64.StdEncoding.EncodeToString(sum[:]) {
		http.Error(w, "Content-MD5 mismatch", http.StatusBadRequest)
		return
	}
	if code, msg := s.saveState(ctx, id, body); code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// saveState stores body as the workspace's new current state version, unless it's the same as the
// current one. The caller holds the state lock. It returns 200, or an error status and message.
func (s *Server) saveState(ctx context.Context, id string, body []byte) (int, string) {
	sum := md5.Sum(body)
	// Only the fields needed to version the file and show its outputs; the rest is Terraform's business.
	var head struct {
		Serial  *int64 `json:"serial"`
		Lineage string `json:"lineage"`
		Outputs map[string]struct {
			Value     any  `json:"value"`
			Sensitive bool `json:"sensitive"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(body, &head); err != nil || head.Serial == nil || head.Lineage == "" {
		return http.StatusBadRequest, "not a Terraform state file"
	}
	v := storage.StateVersion{Serial: *head.Serial, Lineage: head.Lineage, MD5: hex.EncodeToString(sum[:]), Size: int64(len(body)),
		Outputs: map[string]any{}}
	for k, o := range head.Outputs {
		v.Outputs[k] = o.Value
		if o.Sensitive { // sensitive values never leave the state file
			v.Outputs[k] = "(sensitive)"
		}
	}
	cur, err := s.Store.StateVersions(ctx, id, 1)
	if err != nil {
		return s.stateFailure("list state versions", err)
	}
	if len(cur) > 0 {
		switch {
		case cur[0].Lineage != v.Lineage:
			return http.StatusConflict, "state lineage differs from the workspace's state"
		case v.Serial < cur[0].Serial:
			return http.StatusConflict, "state serial is older than the workspace's state"
		case v.MD5 == cur[0].MD5:
			return http.StatusOK, "" // unchanged: no new version
		case v.Serial == cur[0].Serial: // Terraform bumps the serial on every change: same serial, other content is a fork
			return http.StatusConflict, "state serial equals the workspace's state, but its content differs"
		}
	}
	ws, err := s.Store.GetWorkspaceByID(ctx, id)
	if err != nil {
		return s.stateFailure("get workspace", err)
	}
	v.BlobKey = fmt.Sprintf("%s/%d-%s.tfstate", strings.TrimSuffix(ws.StateKey, ".tfstate"), v.Serial, rand.Text())
	if err := s.StateStore.WriteAll(ctx, v.BlobKey, body, &blob.WriterOptions{ContentType: "application/json"}); err != nil {
		return s.stateFailure("write state", err)
	}
	if err := s.Store.AddStateVersion(ctx, id, v); err != nil {
		return s.stateFailure("record state version", err) // the blob is orphaned, which is harmless
	}
	s.Log.Info("state saved", "workspace", id, "serial", v.Serial, "bytes", v.Size)
	return http.StatusOK, ""
}

func (s *Server) stateFailure(what string, err error) (int, string) {
	s.Log.Error(what, "err", err)
	return http.StatusInternalServerError, "internal error"
}

func (s *Server) lockState(w http.ResponseWriter, r *http.Request) {
	info, lockID, ok := readLockInfo(w, r)
	if !ok {
		return
	}
	held, ok, err := s.Store.LockState(r.Context(), r.PathValue("id"), lockID, info)
	if err != nil {
		s.stateError(w, "lock state", err)
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

func (s *Server) unlockState(w http.ResponseWriter, r *http.Request) {
	_, lockID, ok := readLockInfo(w, r)
	if !ok {
		return
	}
	if err := s.Store.UnlockState(r.Context(), r.PathValue("id"), lockID); err != nil {
		s.stateError(w, "unlock state", err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// readLockInfo reads Terraform's LockInfo JSON and returns it with its ID.
func readLockInfo(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	var info struct{ ID string }
	if err != nil || json.Unmarshal(b, &info) != nil || info.ID == "" {
		http.Error(w, "body must be Terraform lock info with an ID", http.StatusBadRequest)
		return nil, "", false
	}
	return bytes.TrimSpace(b), info.ID, true
}

func (s *Server) stateError(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
