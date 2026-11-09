package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/config"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

func TestEstimate(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "infracost")
	// Fake infracost: checks it gets the plan JSON and only its own key, then answers like breakdown.
	script := `#!/bin/sh
[ "$INFRACOST_API_KEY" = key ] && [ -z "$ARM_CLIENT_SECRET" ] && grep -q format_version "$3" || exit 1
echo '{"currency":"'"${INFRACOST_CURRENCY:-USD}"'","pastTotalMonthlyCost":"10","totalMonthlyCost":"25.5","diffTotalMonthlyCost":"15.5","summary":{"totalSupportedResources":2,"totalUnsupportedResources":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_CLIENT_SECRET", "s3cret")
	r := &Runner{Runner: config.Runner{Infracost: bin, InfracostAPIKey: "key"}}
	var log strings.Builder
	c := r.estimate(context.Background(), dir, &tfjson.Plan{FormatVersion: "1.2"}, "EUR", &log)
	want := &runnerapi.Cost{Currency: "EUR", Prior: 10, Proposed: 25.5, Delta: 15.5, Priced: 2, Unsupported: 1}
	if !reflect.DeepEqual(c, want) || !strings.Contains(log.String(), "EUR 25.50 (+15.50)") {
		t.Fatalf("estimate: %+v\n%s", c, log.String())
	}

	// A failing infracost leaves the plan without an estimate; off without a key.
	r.InfracostAPIKey = "wrong"
	if c := r.estimate(context.Background(), dir, &tfjson.Plan{}, "", &log); c != nil || !strings.Contains(log.String(), "cost estimation skipped") {
		t.Fatalf("failed estimate: %+v\n%s", c, log.String())
	}
	r.InfracostAPIKey = ""
	if c := r.estimate(context.Background(), dir, &tfjson.Plan{}, "", io.Discard); c != nil {
		t.Fatalf("estimate without a key: %+v", c)
	}
}
