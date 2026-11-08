package httpserver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// secretFields are form fields whose values never go into the audit log, only that they were sent.
var secretFields = map[string]bool{"value": true, "token": true, "code": true, "code_verifier": true}

// secretName is the backstop for secrets in fields not marked sensitive, e.g. a template input
// whose template forgot to set sensitive.
func secretName(k string) bool {
	k = strings.ToLower(k)
	return strings.Contains(k, "password") || strings.Contains(k, "secret") || strings.Contains(k, "token") || strings.Contains(k, "passphrase")
}

const maxAuditError = 300 // bytes of a plain-text error response kept as the entry's error

const (
	maxFormSize   = 1 << 20 // urlencoded body; the largest real field is a 64 KB variable value
	maxAuditValue = 1 << 10 // bytes of each form value kept in the entry
)

// truncate cuts each value to maxAuditValue: the log is append-only, refused requests included.
func truncate(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		if len(s) > maxAuditValue {
			s = s[:maxAuditValue] + "…(truncated)"
		}
		out[i] = s
	}
	return out
}

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
		if s.App.Store == nil { // only the DB-less tests
			next.ServeHTTP(w, r)
			return
		}
		mutating := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		h := next
		if mutating {
			// urlencoded bodies only; capped as this runs before login. Rejected here: the handlers' own
			// ParseForm then returns nil and they'd act on an empty form.
			if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
				r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
			}
			if err := r.ParseForm(); err != nil {
				h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
				})
			}
		}
		p := &auth.Principal{Type: "anonymous"}
		if u, ok := s.Auth.User(r); ok {
			*p = auth.Principal{User: u, Type: "user"}
		}
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r.WithContext(auth.WithPrincipal(r.Context(), p)))
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
			if groups, err := s.App.UserGroups(ctx, app.Actor{UserID: p.ID}); err == nil {
				principal["groups"] = groups
			}
		}
		detail := map[string]any{"request": req, "principal": principal}
		if len(r.PostForm) > 0 {
			form := map[string]any{}
			for k, v := range r.PostForm {
				if secretFields[k] || strings.HasPrefix(k, secretPrefix) || secretName(k) {
					form[k] = "(redacted)"
				} else {
					form[k] = truncate(v)
				}
			}
			detail["form"] = form
		}
		// shortcut: written after the response, so a failed write loses the entry (logged below); make it
		// part of each handler's transaction if the log must never miss an action.
		// escaped: a decoded %00 or %ff would fail the insert, and a probe would leave no entry
		if err := s.App.Audit(ctx, actor, r.Method+" "+r.URL.EscapedPath(), detail); err != nil {
			s.App.Log.Error("audit log write failed", "err", err, "actor", actor, "method", r.Method, "path", r.URL.Path)
		}
	})
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	access := r.URL.Query().Get("show") == "access"
	entries, broken, err := s.App.AuditLog(r.Context(), actor(r), access)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	rows := make([]auditRow, len(entries))
	for i, e := range entries {
		rows[i] = newAuditRow(e)
	}
	s.render(w, r, http.StatusOK, "audit.html", map[string]any{"Entries": rows, "Broken": broken, "Access": access})
}

// auditRow is an entry as the audit page lists it: a one-line summary, the full detail behind a click.
type auditRow struct {
	storage.AuditEntry
	Status      int
	StatusClass string // a .status class
	StatusText  string
	From        string // client address
	Summary     string // key form fields and the error, if any
	Pretty      string // the detail, indented
}

func newAuditRow(e storage.AuditEntry) auditRow {
	row := auditRow{AuditEntry: e}
	var d struct {
		Form    map[string]any `json:"form"`
		Request struct {
			IP           string `json:"ip"`
			ForwardedFor string `json:"forwarded_for"`
			Status       int    `json:"status"`
			Error        string `json:"error"`
		} `json:"request"`
	}
	json.Unmarshal(e.Detail, &d) // a malformed detail still lists, only without summary
	var buf bytes.Buffer
	if json.Indent(&buf, e.Detail, "", "  ") == nil {
		row.Pretty = buf.String()
	} else {
		row.Pretty = string(e.Detail)
	}
	row.From = d.Request.IP
	if f := d.Request.ForwardedFor; f != "" { // client-supplied unless a proxy sets it: keep the peer address next to it
		row.From = f + " (via " + d.Request.IP + ")"
	}
	row.Status = d.Request.Status
	switch st := d.Request.Status; {
	case st == 0:
	case st == http.StatusUnauthorized || st == http.StatusForbidden:
		row.StatusClass, row.StatusText = "failed", "denied"
	case st >= 500:
		row.StatusClass, row.StatusText = "failed", "error"
	case st >= 400:
		row.StatusClass, row.StatusText = "needs_confirmation", "rejected"
	default:
		row.StatusClass, row.StatusText = "succeeded", "ok"
	}
	row.Summary = formSummary(d.Form)
	if d.Request.Error != "" {
		row.Summary = strings.TrimPrefix(row.Summary+" · "+d.Request.Error, " · ")
	}
	return row
}

// formSummary lists up to 3 non-empty form fields, name-like ones first. The template builder's
// per-input arrays (in_*) are left to the full detail.
func formSummary(form map[string]any) string {
	rank := func(k string) int { // name, key, action first, the rest alphabetical
		if i := slices.Index([]string{"name", "key", "action"}, k); i >= 0 {
			return i
		}
		return 3
	}
	var keys []string
	for k, v := range form {
		if !strings.HasPrefix(k, "in_") && auditValue(v) != "" {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(rank(a)-rank(b), strings.Compare(a, b)) })
	var parts []string
	for i, k := range keys {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(keys)-3))
			break
		}
		parts = append(parts, k+"="+auditValue(form[k]))
	}
	return strings.Join(parts, " ")
}

// auditValue is a form value for the summary: a list's non-empty values joined, cut at 40 runes.
func auditValue(v any) string {
	var s string
	switch v := v.(type) {
	case string:
		s = v
	case []any:
		var vals []string
		for _, x := range v {
			if x, ok := x.(string); ok && x != "" {
				vals = append(vals, x)
			}
		}
		s = strings.Join(vals, ",")
	}
	if r := []rune(s); len(r) > 40 {
		s = string(r[:39]) + "…"
	}
	return s
}
