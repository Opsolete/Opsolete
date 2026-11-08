package httpserver

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/opsolete/opsolete/internal/app"
)

// A run's log: as plain text, and followed live (Server-Sent Events) by the run page.

// runLog opens the {run} log of the {name} workspace (app.RunLog); it writes the error and returns
// ok=false otherwise.
func (s *Server) runLog(w http.ResponseWriter, r *http.Request) (app.RunLog, bool) {
	id, _ := pathID(r, "run") // not an id: not found
	l, err := s.App.RunLog(r.Context(), actor(r), r.PathValue("name"), id)
	if err != nil {
		s.uiError(w, r, err)
		return l, false
	}
	return l, true
}

func (s *Server) logText(w http.ResponseWriter, r *http.Request) {
	l, ok := s.runLog(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if l.Removed != "" {
		io.WriteString(w, l.Removed+"\n")
		return
	}
	var after int64
	for {
		chunks, err := l.Chunks(r.Context(), after, 500)
		if err != nil {
			s.App.Log.Error("read logs", "err", err)
			return
		}
		for _, c := range chunks {
			io.WriteString(w, ansiCodes.ReplaceAllString(c.Chunk, "")) // Terraform's colors are for the run page
			after = c.Seq
		}
		if len(chunks) < 500 {
			return
		}
	}
}

var sseNewlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// ansiCodes matches the color codes (SGR sequences) in Terraform's output.
var ansiCodes = regexp.MustCompile("\x1b\\[[0-9;]*m")

// logStream follows a run's log as Server-Sent Events. Event ids are log sequence numbers, so a
// reconnecting EventSource resumes via Last-Event-ID. Ends with a "done" event (data: the run's
// status) once no runner works on the run any more: finished, or waiting for confirmation.
// shortcut: polls Postgres every second; switch to LISTEN/NOTIFY if many viewers make that costly.
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	l, ok := s.runLog(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cursor, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // long-lived response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // stop nginx-style ingresses from buffering the stream
	w.WriteHeader(http.StatusOK)
	if l.Removed != "" { // an ended run: that line, then done like any finished run
		cur, err := l.Status(ctx)
		if err == nil {
			fmt.Fprintf(w, "data: %s\n\nevent: done\ndata: %s\n\n", l.Removed, cur.Status)
		}
		return
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	idle, ended, sent := 0, false, ""
	for {
		chunks, err := l.Chunks(ctx, cursor, 200)
		if err != nil {
			return
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "id: %d\n", c.Seq)
			// SSE ends a line at \r too: a bare \r in Terraform's output would start a bogus field
			for _, line := range strings.Split(sseNewlines.Replace(c.Chunk), "\n") {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			io.WriteString(w, "\n")
			cursor = c.Seq
		}
		if len(chunks) < 200 {
			cur, err := l.Status(ctx)
			if err != nil {
				return
			}
			if cur.Working() && cur.Status != sent { // lets the page re-render its steps, e.g. queued → applying
				fmt.Fprintf(w, "event: status\ndata: %s\n\n", cur.Status)
				sent = cur.Status
			}
			if len(chunks) == 0 {
				if !cur.Working() {
					if !ended { // its last lines may have landed after the Logs query: read once more
						ended = true
						continue
					}
					fmt.Fprintf(w, "event: done\ndata: %s\n\n", cur.Status)
					rc.Flush()
					return
				}
				if idle++; idle%15 == 0 {
					io.WriteString(w, ": keepalive\n\n")
				}
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
