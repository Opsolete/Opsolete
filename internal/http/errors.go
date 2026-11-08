package httpserver

import (
	"errors"
	"net/http"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
)

// actor is who sent the request, for app operations.
func actor(r *http.Request) app.Actor {
	u := auth.FromContext(r.Context())
	return app.Actor{UserID: u.ID, Email: u.Email, Admin: u.Admin()}
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.App.Log.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// uiError answers an app error on the UI: not found 404, forbidden 403, conflict 409 and a
// validation error 400, each with its message; anything else 500. Handlers whose form shows a
// ValidationError re-render it before calling this. The Terraform http state backend answers in the
// same plain-text shape.
func (s *Server) uiError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, app.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, app.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case app.IsConflict(err):
		http.Error(w, err.Error(), http.StatusConflict)
	case isValidation(err):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		s.internalError(w, r.Method+" "+r.URL.Path, err)
	}
}

// apiError answers an app error on the cloud API as JSON:API errors: 404, 403, 409, 422 (Terraform
// shows a 422's message), else 500.
func (s *Server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not found")
	case errors.Is(err, app.ErrForbidden):
		writeAPIError(w, http.StatusForbidden, err.Error())
	case app.IsConflict(err):
		writeAPIError(w, http.StatusConflict, err.Error())
	case isValidation(err):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.cloudError(w, r.Method+" "+r.URL.Path, err)
	}
}

// runnerError answers an app error on the runner API as {"errors": [msg]}: not found 404 (no such
// artifact), forbidden 403, conflict 409 (storage.ErrLeaseLost: "job lease lost", the runner stops
// working on the job), validation 400, else 500. The registry protocol has the same error shape.
func (s *Server) runnerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeErrors(w, http.StatusNotFound, err.Error())
	case errors.Is(err, app.ErrForbidden):
		writeErrors(w, http.StatusForbidden, err.Error())
	case app.IsConflict(err):
		writeErrors(w, http.StatusConflict, err.Error())
	case isValidation(err):
		writeErrors(w, http.StatusBadRequest, err.Error())
	default:
		s.App.Log.Error("runner "+r.Method+" "+r.URL.Path, "err", err)
		writeErrors(w, http.StatusInternalServerError, "internal error")
	}
}

func isValidation(err error) bool { _, ok := errors.AsType[*app.ValidationError](err); return ok }
