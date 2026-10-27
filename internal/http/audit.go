package httpserver

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// secretFields are form fields whose values never go into the audit log, only that they were sent.
var secretFields = map[string]bool{"value": true, "token": true, "code": true, "code_verifier": true}

const maxAuditError = 300 // bytes of a plain-text error response kept as the entry's error

type statusWriter struct {
	http.ResponseWriter
	status int
	errMsg []byte
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.errMsg) < maxAuditError {
		w.errMsg = append(w.errMsg, b[:min(len(b), maxAuditError-len(w.errMsg))]...)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// audit records in the audit log every state-changing request, every sign-in (successful or not)
// and every request refused as unauthenticated or forbidden: who (principal and groups), from where
// (address, user agent), what (method, path, form) and the outcome (status, error). Runner and state
// backend calls are only recorded when refused: they act for a run whose trigger and confirmation
// are recorded, and their progress is the run's status history.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Store == nil { // only the DB-less tests
			next.ServeHTTP(w, r)
			return
		}
		mutating := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if mutating {
			r.ParseForm() // urlencoded bodies only (same 10 MB cap as the handlers' own ParseForm); a bad body is the handler's to reject
		}
		p := &auth.Principal{Type: "anonymous"}
		if u, ok := s.Auth.User(r); ok {
			*p = auth.Principal{User: u, Type: "user"}
		}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		machine := strings.HasPrefix(r.URL.Path, runnerapi.Prefix+"/") || strings.HasPrefix(r.URL.Path, runnerapi.StatePrefix+"/")
		denied := sw.status == http.StatusUnauthorized || sw.status == http.StatusForbidden
		if !denied && (machine || !mutating) && r.URL.Path != "/auth/callback" {
			return
		}
		ctx := context.WithoutCancel(r.Context())
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		req := map[string]any{"ip": ip, "user_agent": r.UserAgent(), "status": sw.status}
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			req["forwarded_for"] = f // as sent: only trustworthy behind a proxy that sets it
		}
		if len(sw.errMsg) > 0 && strings.HasPrefix(sw.Header().Get("Content-Type"), "text/plain") {
			req["error"] = strings.TrimSpace(string(sw.errMsg))
		}
		principal := map[string]any{"type": p.Type}
		actor := "anonymous"
		if p.Email != "" {
			actor = p.Email
			principal["id"], principal["role"] = p.ID, p.Role
			if groups, err := s.Store.UserGroups(ctx, p.ID); err == nil {
				principal["groups"] = groups
			}
		}
		detail := map[string]any{"request": req, "principal": principal}
		if len(r.PostForm) > 0 {
			form := map[string]any{}
			for k, v := range r.PostForm {
				if secretFields[k] {
					form[k] = "(redacted)"
				} else {
					form[k] = v
				}
			}
			detail["form"] = form
		}
		// shortcut: written after the response, so a failed write loses the entry (logged below); make it
		// part of each handler's transaction if the log must never miss an action.
		if err := s.Store.Audit(ctx, actor, r.Method+" "+r.URL.Path, detail); err != nil {
			s.Log.Error("audit log write failed", "err", err, "actor", actor, "method", r.Method, "path", r.URL.Path)
		}
	})
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	access := r.URL.Query().Get("show") == "access"
	entries, err := s.Store.AuditLog(r.Context(), 500, access)
	if err != nil {
		s.internalError(w, "audit log", err)
		return
	}
	broken, err := s.Store.VerifyAuditLog(r.Context())
	if err != nil {
		s.internalError(w, "verify audit log", err)
		return
	}
	s.render(w, r, http.StatusOK, "audit.html", map[string]any{"Entries": entries, "Broken": broken, "Access": access})
}
