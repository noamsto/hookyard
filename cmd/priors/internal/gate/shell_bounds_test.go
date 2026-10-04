package gate

import (
	"strings"
	"testing"
	"time"
)

// boundLimit is how long a hostile command may take to judge.
const boundLimit = time.Second

// judgeWithin returns ingestReason(cmd), failing the test once judging takes
// longer than boundLimit.
func judgeWithin(t *testing.T, cmd string) string {
	t.Helper()
	done := make(chan string, 1)
	start := time.Now()
	go func() { done <- ingestReason(cmd) }()
	select {
	case reason := <-done:
		t.Logf("%d bytes judged in %v", len(cmd), time.Since(start))
		return reason
	case <-time.After(boundLimit):
		t.Fatalf("judging %d bytes took over %v", len(cmd), boundLimit)
		return ""
	}
}

// TestIngestReasonBounded feeds commands built to exhaust the parser's stack,
// the expander's memory or a quadratic scan. Each must flag, or come back
// clean, quickly, and never crash the binary: a stack overflow is fatal.
func TestIngestReasonBounded(t *testing.T) {
	flags := []struct{ name, cmd string }{
		{"quoted brace product of parentheses", "false && : " + strings.Repeat(`{'((((','(((('}`, 14) + "; gh issue view 1"},
		{"deep parentheses", strings.Repeat("((((", 60<<10)},
		{"deep parentheses past length bound", strings.Repeat("((((", 75<<10)},
		{"brace sequences", "false && : " + strings.Repeat("{1..16000} ", 372) + "; gh issue view 1"},
	}
	for _, tt := range flags {
		t.Run(tt.name, func(t *testing.T) {
			if reason := judgeWithin(t, tt.cmd); reason == "" {
				t.Errorf("not flagged")
			}
		})
	}
	t.Run("long short option", func(t *testing.T) {
		if reason := judgeWithin(t, "true -"+strings.Repeat("a", 120<<10)); reason != "" {
			t.Errorf("flagged by %q", reason)
		}
	})
}
