package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

var errUnauthorized = errors.New("server rejected the runner token")

// errUnreadableJob: the server claimed a job for this runner but its response didn't decode. The
// job isn't acknowledged, so the server queues it again once the ack window passes.
var errUnreadableJob = errors.New("claimed job unreadable, not acknowledged: the server queues it again")

func (r *Runner) claim(ctx context.Context) (runnerapi.Job, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second) // server long-polls ~20s
	defer cancel()
	body, _ := json.Marshal(runnerapi.ClaimRequest{Runner: r.Name})
	code, resp, err := r.call(ctx, "/jobs/claim", "", "application/json", body)
	if err != nil {
		return runnerapi.Job{}, false, err
	}
	switch code {
	case http.StatusOK:
		var j runnerapi.Job
		if err := json.Unmarshal(resp, &j); err != nil {
			return runnerapi.Job{}, false, fmt.Errorf("%w: %v", errUnreadableJob, err)
		}
		return j, true, nil
	case http.StatusNoContent:
		return runnerapi.Job{}, false, nil
	case http.StatusUnauthorized:
		return runnerapi.Job{}, false, errUnauthorized
	default:
		return runnerapi.Job{}, false, fmt.Errorf("claim: HTTP %d: %s", code, resp)
	}
}

// ack acknowledges a claimed job before any work on it, retrying within the server's ack window
// (30 s); false: drop the job (the lease is gone, or the server queues it again unacknowledged).
func (r *Runner) ack(ctx context.Context, job runnerapi.Job) bool {
	log := r.Log.With("run", job.RunID, "phase", job.Phase)
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, 8*time.Second)
		code, resp, err := r.call(actx, "/jobs/"+job.RunID+"/ack", job.LeaseID, "application/json", nil)
		cancel()
		if err == nil && code == http.StatusNoContent {
			return true
		}
		if (err == nil && code < 500) || attempt == 3 || ctx.Err() != nil {
			log.Error("acknowledging the job failed; dropped (the server queues it again)", "err", err, "code", code, "body", string(resp))
			return false
		}
		sleep(ctx, time.Duration(attempt)*time.Second)
	}
}

func (r *Runner) finish(log *slog.Logger, job runnerapi.Job, res runnerapi.FinishRequest) {
	body, _ := json.Marshal(res)
	// Until the server would have failed the run for its lease (lease plus its reaper's grace): a server restart
	// during that time must not turn a successful apply into an errored run. The lease id fences a late report.
	until := time.Now().Add(time.Duration(max(job.LeaseSeconds, 30))*time.Second + time.Minute)
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		code, resp, err := r.call(ctx, "/jobs/"+job.RunID+"/finish", job.LeaseID, "application/json", body)
		cancel()
		if err == nil && code == http.StatusNoContent {
			log.Info("job finished", "status", res.Status)
			return
		}
		if err == nil && code < 500 {
			log.Error("finish rejected", "code", code, "body", string(resp))
			return
		}
		wait := min(time.Duration(attempt)*2*time.Second, 10*time.Second)
		if time.Now().Add(wait).After(until) {
			log.Error("could not report the result; the server fails the run when its lease expires", "err", err, "code", code)
			return
		}
		log.Warn("reporting the result failed, retrying", "err", err, "code", code, "in", wait)
		time.Sleep(wait)
	}
}

// transferTimeout bounds an artifact upload or download, so a stalled connection doesn't hold the
// phase until the job timeout while heartbeats keep its lease.
// shortcut: a limit on the whole transfer, not on stalls; at 256 MB it needs about 0.5 MB/s.
const transferTimeout = 10 * time.Minute

// upload stores a plan phase's artifact on the server.
func (r *Runner) upload(ctx context.Context, job runnerapi.Job, name string, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	code, resp, err := r.call(ctx, "/jobs/"+job.RunID+"/artifacts/"+name, job.LeaseID, "application/octet-stream", b)
	if err == nil && code != http.StatusNoContent {
		err = fmt.Errorf("HTTP %d: %s", code, resp)
	}
	return err
}

// download writes an artifact of the run's plan phase to path; a missing optional one is skipped.
func (r *Runner) download(ctx context.Context, job runnerapi.Job, name, path string, required bool) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.ServerURL.JoinPath(runnerapi.Prefix, "jobs", job.RunID, "artifacts", name).String(), nil)
	if err != nil {
		return err
	}
	r.authorize(req, job.LeaseID)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && !required {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *Runner) authorize(req *http.Request, lease string) {
	req.Header.Set("Authorization", "Bearer "+r.Token)
	if lease != "" {
		req.Header.Set(runnerapi.LeaseHeader, lease)
	}
}

func (r *Runner) call(ctx context.Context, path, lease, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ServerURL.JoinPath(runnerapi.Prefix, path).String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	r.authorize(req, lease)
	req.Header.Set("Content-Type", contentType)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, runnerapi.MaxJobSize+1))
	if err != nil {
		return 0, nil, err
	}
	if len(b) > runnerapi.MaxJobSize {
		return 0, nil, fmt.Errorf("%s: response is over %d bytes", path, runnerapi.MaxJobSize)
	}
	return resp.StatusCode, b, nil
}
