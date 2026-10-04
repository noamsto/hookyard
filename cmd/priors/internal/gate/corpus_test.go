package gate

import (
	"bufio"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
)

// tally counts one population of commands against gate 2 and rule (b) alone.
type tally struct{ n, gate, over, both int }

func (c *tally) add(gate, over bool) {
	c.n++
	if gate {
		c.gate++
	}
	if over {
		c.over++
	}
	if gate && over {
		c.both++
	}
}

func (c tally) log(t *testing.T, label string) {
	t.Helper()
	pct := func(k int) float64 {
		if c.n == 0 {
			return 0
		}
		return 100 * float64(k) / float64(c.n)
	}
	row := func(name string, k int) { t.Logf("%-9s %-14s %7d  %5.1f%%", label, name, k, pct(k)) }
	row("gate-2", c.gate)
	row("(b)-alone", c.over)
	row("both", c.both)
	row("gate-2-only", c.gate-c.both)
	row("(b)-only", c.over-c.both)
}

// TestCorpusRates reports how often gate 2 and rule (b) alone flag a corpus of
// commands, one JSON string per line. Skipped unless PRIORS_CORPUS names the
// file. Owner's one-liner:
//
//	find ~/.claude/projects -name '*.jsonl' -mtime -30 -exec jq -cR 'fromjson? | select(.type=="assistant") | .message.content[]? | select(.type=="tool_use" and .name=="Bash") | .input.command' {} + > /tmp/priors-cmds.jsonl && PRIORS_CORPUS=/tmp/priors-cmds.jsonl go test ./cmd/priors/internal/gate -run TestCorpusRates -v
func TestCorpusRates(t *testing.T) {
	path := os.Getenv("PRIORS_CORPUS")
	if path == "" {
		t.Skip("PRIORS_CORPUS not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	var lines, distinct tally
	var unreadable, total int
	seen := map[string]bool{}
	reasons := map[string]int{}
	for sc.Scan() {
		total++
		var cmd string
		if json.Unmarshal(sc.Bytes(), &cmd) != nil {
			unreadable++
			continue
		}
		reason := ingestReason(cmd)
		gate, over := reason != "", overflagReason(cmd) != ""
		lines.add(gate, over)
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		distinct.add(gate, over)
		if gate {
			reasons[reason]++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	// Aggregates only: never log a command or any part of one.
	t.Logf("lines %d, distinct %d, unreadable %d", total, distinct.n, unreadable)
	lines.log(t, "lines")
	distinct.log(t, "distinct")
	for _, r := range slices.Sorted(maps.Keys(reasons)) {
		t.Logf("gate-2 reason %-12s %7d", r, reasons[r])
	}
}
