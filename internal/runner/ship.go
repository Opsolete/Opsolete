package runner

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// shipper buffers job output and posts it to the server about once a second, cut at line
// boundaries. Posts double as heartbeats; a 409 means the lease is gone and the job is cancelled.
type shipper struct {
	r        *Runner
	job      runnerapi.Job
	cancel   context.CancelFunc
	redact   *strings.Replacer
	mu       sync.Mutex
	buf      bytes.Buffer
	lost     atomic.Bool
	canceled atomic.Bool // a user canceled the run: Terraform is interrupted, the lease kept
	end      time.Time   // the phase's hard end: no heartbeats or log posts after it
	stop     chan struct{}
	stopped  chan struct{}
}

const maxBuffered = 8 << 20 // if the server is unreachable for long, drop output rather than memory

func newShipper(r *Runner, job runnerapi.Job, cancel context.CancelFunc, signed []string, end time.Time) *shipper {
	var pairs []string
	for _, u := range signed {
		if i := strings.Index(u, "?"); i >= 0 {
			pairs = append(pairs, u[i:], "?<signed>")
		}
	}
	pairs = append(pairs, mask.New(job.Secrets).Pairs("***")...)
	s := &shipper{r: r, job: job, cancel: cancel, redact: strings.NewReplacer(pairs...), end: end,
		stop: make(chan struct{}), stopped: make(chan struct{})}
	go s.loop()
	return s
}

func (s *shipper) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() < maxBuffered {
		s.buf.Write(p) // redacted in flush: a signed URL may arrive split over two writes
	}
	return len(p), nil
}

func (s *shipper) loop() {
	defer close(s.stopped)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	beat := time.Duration(max(s.job.LeaseSeconds, 8)) * time.Second / 4
	last := time.Now()
	for {
		select {
		case <-s.stop:
			// drain the tail (where errors are) in 256 KB posts; stop at the first failure
			for s.flush(true) {
			}
			return
		case <-tick.C:
			if time.Now().After(s.end) {
				// Terraform is killed by now: if the runner can't finish the job, the lease runs out
				// and the server's reaper does. Close still ships the tail once.
				continue
			}
			if s.flush(false) {
				last = time.Now()
			} else if time.Since(last) > beat {
				s.post("/heartbeat", nil)
				last = time.Now()
			}
		}
	}
}

// flush sends buffered output; unless final, only up to the last newline. Reports whether it sent
// something successfully.
func (s *shipper) flush(final bool) bool {
	s.mu.Lock()
	data := s.buf.Bytes()
	n := len(data)
	if !final {
		n = bytes.LastIndexByte(data, '\n') + 1
	}
	if n > 256<<10 { // cut at a line end if there is one, so redaction sees whole lines
		n = 256 << 10
		if i := bytes.LastIndexByte(data[:n], '\n'); i >= 0 {
			n = i + 1
		}
	}
	chunk := bytes.Clone(data[:n])
	s.buf.Next(n)
	s.mu.Unlock()
	if len(chunk) == 0 {
		return false
	}
	chunk = []byte(s.redact.Replace(string(chunk)))
	if !s.post("/logs", chunk) {
		if s.lost.Load() {
			return false
		}
		s.mu.Lock() // keep it for the next attempt
		rest := bytes.Clone(s.buf.Bytes())
		s.buf.Reset()
		s.buf.Write(chunk)
		s.buf.Write(rest)
		s.mu.Unlock()
		return false
	}
	return true
}

func (s *shipper) post(path string, body []byte) bool {
	if s.lost.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, _, err := s.r.call(ctx, "/jobs/"+s.job.RunID+path, s.job.LeaseID, "text/plain; charset=utf-8", body)
	if err == nil && code == http.StatusConflict {
		s.lost.Store(true)
		s.cancel() // someone else owns this run now; stop terraform
		return false
	}
	if err == nil && code == runnerapi.StatusCancel {
		if !s.canceled.Swap(true) {
			s.cancel() // SIGINT: Terraform finishes in-flight operations and saves state
		}
		return true
	}
	return err == nil && code == http.StatusNoContent
}

// Close stops the loop after a final flush.
func (s *shipper) Close() {
	close(s.stop)
	<-s.stopped
}
