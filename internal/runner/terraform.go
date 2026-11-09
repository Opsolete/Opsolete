package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sync/atomic"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// ansiCodes matches Terraform's color codes (SGR sequences).
var ansiCodes = regexp.MustCompile("\x1b\\[[0-9;]*m")

// tfProc runs every terraform command of one phase under one rule for any interrupt (job timeout,
// user cancel, lost lease, runner shutdown): when the command's context ends, Terraform gets SIGINT
// (it starts nothing new, finishes in-flight operations and saves state), then at most grace to
// exit, and never past end, before its process group is killed.
type tfProc struct {
	bin, dir    string
	env         []string
	grace       time.Duration
	end         time.Time   // the phase's hard end: its deadline plus grace
	interrupted atomic.Bool // a running command got SIGINT
	killed      atomic.Bool // a command didn't stop in time and was killed
}

// run runs a terraform command, with its colors on unless args has -no-color (the run page renders
// them). Stderr goes in the error too. With -detailed-exitcode, exit code 2 means "changes present",
// reported as true.
func (p *tfProc) run(ctx context.Context, stdout, stderr io.Writer, args ...string) (bool, error) {
	errs := headBuffer{max: 64 << 10} // the job's message keeps only the start of it
	cmd := exec.CommandContext(ctx, p.bin, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = p.dir, p.env, stdout, io.MultiWriter(stderr, &errs)
	var kill *time.Timer // set by Cancel, which returns before Wait does
	cmd.Cancel = func() error {
		kill = time.AfterFunc(min(p.grace, time.Until(p.end)), func() {
			p.killed.Store(true)
			killCommand(cmd)
		})
		err := cmd.Process.Signal(os.Interrupt)
		if err == nil {
			p.interrupted.Store(true)
		}
		return err
	}
	// Also bounds the wait for output pipes a process outside the group holds open. A second after
	// kill's timer (both start at Cancel), so a kill is always ours and recorded.
	// shortcut: such a process can hold the phase a second past its hard end; fine unless that matters.
	cmd.WaitDelay = p.grace + time.Second
	isolateCommand(cmd)
	err := cmd.Run()
	if kill != nil {
		kill.Stop()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 && slices.Contains(args, "-detailed-exitcode") {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w\n%s", err, ansiCodes.ReplaceAllString(errs.String(), ""))
	}
	return false, nil
}

// json decodes a terraform command's JSON output (show, output) into v. That output holds values,
// secrets included: it never reaches the log.
func (p *tfProc) json(ctx context.Context, v any, args ...string) error {
	var out bytes.Buffer
	if _, err := p.run(ctx, &out, io.Discard, args...); err != nil {
		return err
	}
	return json.Unmarshal(out.Bytes(), v)
}

// headBuffer keeps the first max bytes written to it and drops the rest.
type headBuffer struct {
	bytes.Buffer
	max int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - h.Len(); room > 0 {
		h.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// summarize counts a plan's resource changes (or drift), like Terraform's "Plan: x to add" line,
// and lists their addresses. Values are never included.
func summarize(rcs []*tfjson.ResourceChange) runnerapi.Plan {
	sum := runnerapi.Plan{Changes: []runnerapi.Change{}}
	for _, rc := range rcs {
		if rc.Change == nil {
			continue
		}
		a := rc.Change.Actions
		var action string
		switch {
		case a.Replace():
			sum.Add++
			sum.Destroy++
			action = "replace"
		case a.Create():
			sum.Add++
			action = "create"
		case a.Update():
			sum.Change++
			action = "update"
		case a.Delete():
			sum.Destroy++
			action = "delete"
		default: // no-op, read, forget
			continue
		}
		sum.Changes = append(sum.Changes, runnerapi.Change{Address: rc.Address, Action: action})
	}
	return sum
}

// inventory lists the managed resources in the workspace's state: names only, never attribute
// values. nil means the state couldn't be read, so the current state version stays unindexed.
func inventory(ctx context.Context, tf *tfProc) []runnerapi.Resource {
	// Own context: after a cancel the job's context is done, but while the run holds its lease
	// the state is still readable, and a half-applied state is the one most worth indexing.
	// Still within the phase's hard end.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(30*time.Second, time.Until(tf.end)))
	defer cancel()
	var st tfjson.State
	if err := tf.json(ctx, &st, "show", "-json", "-no-color"); err != nil {
		return nil
	}
	return runnerapi.Inventory(&st)
}
