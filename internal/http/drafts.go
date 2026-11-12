package httpserver

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A refused form keeps the secrets typed into it without showing them: its re-render carries each one sealed
// (SECRET_KEY) in a hidden draft.<field>, bound to the user, the page and the field, for draftTTL; posted again
// with the field left empty, the draft is its value. A password field is never filled in.

const (
	draftPrefix = "draft."
	draftTTL    = time.Hour
)

func draftAD(r *http.Request, field string) string {
	return "opsolete form draft\n" + actor(r).UserID.String() + "\n" + r.URL.Path + "\n" + field
}

// sealDraft is the hidden draft of a secret typed into field; "" if it can't be sealed (the user types it again).
func (s *Server) sealDraft(r *http.Request, field, value string) string {
	sealed, err := s.App.Secrets.Seal(strconv.FormatInt(time.Now().Unix(), 10)+"\n"+value, draftAD(r, field))
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(sealed)
}

// draft is field's value from the draft posted with form f: "" without one, or one that doesn't open here (another
// user, page or field) or is older than draftTTL.
func (s *Server) draft(r *http.Request, f url.Values, field string) string {
	b, err := base64.StdEncoding.DecodeString(f.Get(draftPrefix + field))
	if err != nil || len(b) == 0 {
		return ""
	}
	plain, err := s.App.Secrets.Open(b, draftAD(r, field))
	if err != nil {
		return ""
	}
	at, value, ok := strings.Cut(plain, "\n")
	sec, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || time.Since(time.Unix(sec, 0)) > draftTTL {
		return ""
	}
	return value
}

// drafts seals the secrets typed into a refused form (raw by field, sensitive the fields that hold one).
func (s *Server) drafts(r *http.Request, raw map[string]string, sensitive []string) map[string]string {
	out := map[string]string{}
	for _, field := range sensitive {
		if v := raw[field]; strings.TrimSpace(v) != "" {
			if d := s.sealDraft(r, field, v); d != "" {
				out[field] = d
			}
		}
	}
	return out
}
