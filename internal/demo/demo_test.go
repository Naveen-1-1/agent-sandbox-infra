package demo

import (
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestStatusLinesDropChecksums(t *testing.T) {
	fp := "## agent/feature\nA  src/feature.js\n?? PLAN.md\n" +
		strings.Repeat("a", 64) + "  src/feature.js\n" + strings.Repeat("b", 64) + "  PLAN.md\n"
	got := strings.Join(statusLines(fp), "|")
	if want := "## agent/feature|A  src/feature.js|?? PLAN.md"; got != want {
		t.Fatalf("statusLines = %q, want %q", got, want)
	}
}

func TestBarScales(t *testing.T) {
	for _, c := range []struct {
		at, scale time.Duration
		want      int
	}{{0, 10 * time.Second, 1}, {5 * time.Second, 10 * time.Second, 20}, {10 * time.Second, 10 * time.Second, 40}} {
		if got := len([]rune(bar(c.at, c.scale, 40))); got != c.want {
			t.Errorf("bar(%v, %v) has %d cells, want %d", c.at, c.scale, got, c.want)
		}
	}
	if bar(time.Second, 0, 40) != "" {
		t.Error("a zero scale should draw nothing")
	}
}
