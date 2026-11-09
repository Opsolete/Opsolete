package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/opsolete/opsolete/internal/storage"
)

// View helpers: template funcs and the values pages show.

// dict makes a map of key, value pairs, to pass several values to a shared template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v isn't a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// toJSON renders a value (e.g. a variable default) compactly for display.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// picker is a searchable multi-select of workspaces: the "picker" template in layout.html.
type picker struct {
	Name, Label string       // the checkboxes' form field, and what they select
	Items       []pickerItem // the selected ones first
	Selected    int
}

type pickerItem struct {
	Value, Name string
	Selected    bool
}

// workspacePicker offers the workspaces by name, the selected ones first: they stay selected
// even if all (a capped list) lacks them.
func workspacePicker(field, label string, selected []string, all []storage.Workspace) picker {
	p := picker{Name: field, Label: label, Selected: len(selected)}
	for _, n := range selected {
		p.Items = append(p.Items, pickerItem{n, n, true})
	}
	for _, ws := range all {
		if !slices.Contains(selected, ws.Name) {
			p.Items = append(p.Items, pickerItem{ws.Name, ws.Name, false})
		}
	}
	return p
}

// runGroup is the runs page filter a status falls under.
func runGroup(status string) string {
	switch status {
	case storage.NeedsConfirmation:
		return "attention"
	case storage.Errored:
		return "errored"
	case storage.PlanQueued, storage.Planning, storage.ApplyQueued, storage.Applying:
		return "running"
	case storage.Applied, storage.NoChanges, storage.Planned:
		return "success"
	}
	return "other" // discarded, canceled: only under All
}

// phase is the plan or apply step of a run as the run page shows it.
type phase struct {
	State      string // queued | running | finished | errored | canceled | pending | discarded; "" never runs
	Start, End time.Time
}

func (p phase) Duration() string {
	if p.Start.IsZero() || p.End.IsZero() {
		return ""
	}
	return humanDuration(p.End.Sub(p.Start))
}

// humanDuration: "" for 0 (nothing measured), else "less than a minute" or e.g. "2m5s".
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return "less than a minute"
	}
	return d.Round(time.Second).String()
}

// ago renders t relative to now: "just now", "5 minutes ago", "3 days ago".
func ago(t time.Time) string {
	d := time.Since(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return strconv.Itoa(n) + " " + unit + "s ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month")
	}
	return plural(int(d.Hours()/24/365), "year")
}

// runPhases reads the plan and apply steps from the run's status history (oldest first).
func runPhases(r storage.Run, hist []storage.StatusEntry) (plan, apply phase) {
	span := func(status string) (p phase, next string) {
		for i, e := range hist { // the last entry wins
			if e.Status == status {
				p, next = phase{Start: e.CreatedAt}, ""
				if i+1 < len(hist) {
					p.End, next = hist[i+1].CreatedAt, hist[i+1].Status
				}
			}
		}
		return
	}
	ended := func(next string) string {
		if next == storage.Errored || next == storage.Canceled {
			return next
		}
		return "finished"
	}
	plan, next := span(storage.Planning)
	switch {
	case plan.Start.IsZero() && r.Status == storage.PlanQueued:
		plan.State = "queued"
	case plan.Start.IsZero(): // ended before a runner picked it up
		plan.State = r.Status
	case next == "":
		plan.State = "running"
	default:
		plan.State = ended(next)
	}
	apply, next = span(storage.Applying)
	switch {
	case !apply.Start.IsZero() && next == "":
		apply.State = "running"
	case !apply.Start.IsZero():
		apply.State = ended(next)
	case r.Status == storage.NeedsConfirmation:
		apply.State = "pending"
	case r.Status == storage.ApplyQueued:
		apply.State = "queued"
	case r.Status == storage.Discarded:
		apply.State = "discarded"
	}
	return
}
