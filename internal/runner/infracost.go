package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// estimate prices a plan with `infracost breakdown`. nil when Infracost isn't configured or
// fails: a missing estimate never fails the run.
func (r *Runner) estimate(ctx context.Context, dir string, p *tfjson.Plan, currency string, w io.Writer) *runnerapi.Cost {
	if r.InfracostAPIKey == "" {
		return nil
	}
	skip := func(err error) *runnerapi.Cost {
		fmt.Fprintf(w, "\n==> cost estimation skipped: %v\n", err)
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return skip(err)
	}
	path := filepath.Join(dir, "opsolete.tfplan.json") // holds variable values: stays in the job's dir
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return skip(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Infracost, "breakdown", "--path", path, "--format", "json", "--log-level", "error")
	cmd.Dir = dir
	// Only what Infracost needs: none of the job's variables or credentials. HOME is the job's dir,
	// so its config and cache go away with it.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "INFRACOST_API_KEY=" + r.InfracostAPIKey,
		"INFRACOST_SKIP_UPDATE_CHECK=true", "INFRACOST_NO_COLOR=true"}
	if currency != "" {
		cmd.Env = append(cmd.Env, "INFRACOST_CURRENCY="+currency)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		return skip(fmt.Errorf("infracost: %w: %s", err, msg))
	}
	var res struct {
		Currency                                                     string
		PastTotalMonthlyCost, TotalMonthlyCost, DiffTotalMonthlyCost *string // null without priced resources
		Summary                                                      struct{ TotalSupportedResources, TotalUnsupportedResources int }
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return skip(fmt.Errorf("read infracost output: %w", err))
	}
	c := &runnerapi.Cost{Currency: res.Currency, Priced: res.Summary.TotalSupportedResources, Unsupported: res.Summary.TotalUnsupportedResources}
	for _, f := range []struct {
		dst *float64
		src *string
	}{{&c.Prior, res.PastTotalMonthlyCost}, {&c.Proposed, res.TotalMonthlyCost}, {&c.Delta, res.DiffTotalMonthlyCost}} {
		if f.src == nil {
			continue
		}
		if *f.dst, err = strconv.ParseFloat(*f.src, 64); err != nil {
			return skip(fmt.Errorf("read infracost output: %w", err))
		}
	}
	fmt.Fprintf(w, "\n==> estimated monthly cost: %s %.2f (%+.2f)\n", c.Currency, c.Proposed, c.Delta)
	return c
}
