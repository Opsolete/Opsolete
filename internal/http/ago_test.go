package httpserver

import (
	"testing"
	"time"
)

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", time.Minute: "1 minute ago", 5 * time.Minute: "5 minutes ago",
		3 * time.Hour: "3 hours ago", 49 * time.Hour: "2 days ago", 400 * 24 * time.Hour: "1 year ago",
	} {
		if got := ago(time.Now().Add(-d)); got != want {
			t.Errorf("ago(-%v) = %q, want %q", d, got, want)
		}
	}
}
