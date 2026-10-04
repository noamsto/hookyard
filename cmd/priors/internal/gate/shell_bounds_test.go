package gate

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// boundStack is the goroutine stack judging runs under, an eighth of the
	// runtime's default, so a recursion that creeps toward the default
	// crashes these tests first.
	boundStack = 128 << 20
	// boundAlloc is the most any one command may allocate.
	boundAlloc = 96 << 20
	// hangGuard only stops a judge that never returns; the work bounds are
	// the allocation and stack ones.
	hangGuard = 10 * time.Second
)

// judged is one command's verdict and what it cost.
type judged struct {
	reason  string
	alloc   uint64
	elapsed time.Duration
}

// limitStack runs the rest of the test under boundStack. The tests that call
// it do not run in parallel, so no other test's goroutines share the limit or
// the allocation counts.
func limitStack(t *testing.T) {
	t.Helper()
	old := debug.SetMaxStack(boundStack)
	t.Cleanup(func() { debug.SetMaxStack(old) })
}

// judgeBounded judges cmd and fails the test once it allocates more than
// boundAlloc, or takes longer than hangGuard.
func judgeBounded(t *testing.T, cmd string) judged {
	t.Helper()
	return runBounded(t, len(cmd), func() string { return ingestReason(cmd) })
}

// runBounded runs judge on size bytes of input under judgeBounded's bounds.
func runBounded(t *testing.T, size int, judge func() string) judged {
	t.Helper()
	done := make(chan string, 1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	go func() { done <- judge() }()
	select {
	case reason := <-done:
		j := judged{reason: reason, elapsed: time.Since(start)}
		runtime.ReadMemStats(&after)
		j.alloc = after.TotalAlloc - before.TotalAlloc
		if j.alloc > boundAlloc {
			t.Errorf("judging %d bytes allocated %d MiB", size, j.alloc>>20)
		}
		return j
	case <-time.After(hangGuard):
		t.Fatalf("judging %d bytes took over %v", size, hangGuard)
		return judged{}
	}
}

// codexArgv returns a Codex command array of "#", n brace sequences and then
// tail. distinct makes every sequence differ, so none is judged only once.
func codexArgv(n int, distinct bool, tail ...string) []string {
	argv := []string{"#"}
	for i := range n {
		end := 9999
		if distinct {
			end -= i
		}
		argv = append(argv, "{1.."+strconv.Itoa(end)+"}")
	}
	return append(argv, tail...)
}

// TestIngestReasonBounded feeds commands built to exhaust the parser's or
// the walk's stack, the judge's memory, or a quadratic scan. Each must come
// back flagged, or clean, within the stack, allocation and time bounds: a
// stack overflow or an out-of-memory error is fatal, and no marker is
// written. A Codex command array is judged by IngestsCall. The race detector
// slows judging about tenfold, so the time bound, the pre_tool hook's
// deadline, grows with it.
func TestIngestReasonBounded(t *testing.T) {
	limitStack(t)
	timeLimit := time.Second
	if raceEnabled {
		timeLimit *= 10
	}
	tests := []struct {
		name, cmd string
		flag      bool
	}{
		{"quoted brace product of parentheses", "false && : " + strings.Repeat(`{'((((','(((('}`, 14) + "; gh issue view 1", true},
		{"deep parentheses", strings.Repeat("((((", (maxText-4)/4), true},
		{"deep parentheses past length bound", strings.Repeat("((((", 75<<10), true},
		{"brace sequences", "false && : " + strings.Repeat("{1..16000} ", 372) + "; gh issue view 1", true},
		{"brace sequences past expand limit", "false && : " + strings.Repeat("{1..16385} ", 200) + "; gh issue view 1", true},
		{"brace sequences with MinInt64 step", "false && : " + strings.Repeat("{1..2..-9223372036854775808} ", 100) + "; gh issue view 1", true},
		{"brace sequence of every int64", "false && : {-9223372036854775808..9223372036854775807}; gh issue view 1", true},
		{"self-feeding assignment expansions", `: ${a:=xxxxxxxxxx}${b:=${a//x/$a}}${c:=${b//x/$b}}${d:=${c//x/$c}}${d//x/$d}; gh issue view 1`, true},
		{"nested HOME replacements", ": " + nestedHome(maxNesting-1) + "; gh issue view 1", true},
		{"many glued short options", "true " + strings.Repeat("-xgh ", (maxText-5)/5), true},
		{"long value read many times", "a=" + strings.Repeat("x", 100<<10) + "; " + strings.Repeat("$a ", 8<<10), true},
		{"long short option", "true -" + strings.Repeat("a", 120<<10), false},
		{"arithmetic negation chain", "$((" + strings.Repeat("!", maxText-6) + "1))", false},
		{"pipe chain", strings.Repeat("a|", maxText/2-1) + "a", false},
		{"test negation chain", "[[ " + strings.Repeat("! ", (maxText-8)/2) + "a ]]", false},
		{"if chain", strings.Repeat("if ", maxText/3-1) + "a", false},
		{"unclosed subscripts after syntax error", ": ${x~~}; gh issue view 1 #" + strings.Repeat("${a[)}", 5000), true},
		{"many unclosed subscripts after syntax error", ": ${x~~}; gh issue view 1 #" + strings.Repeat("${a[)}", 20000), true},
	}
	check := func(t *testing.T, j judged, size int, flag bool) {
		t.Helper()
		t.Logf("%d bytes judged in %v, %d KiB allocated", size, j.elapsed, j.alloc>>10)
		if (j.reason != "") != flag {
			t.Errorf("reason %q, want flagged %v", j.reason, flag)
		}
		if j.elapsed > timeLimit {
			t.Errorf("judging %d bytes took %v, over %v", size, j.elapsed, timeLimit)
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check(t, judgeBounded(t, tt.cmd), len(tt.cmd), tt.flag)
		})
	}

	argvTests := []struct {
		name string
		argv []string
		flag bool
	}{
		{"Codex brace sequence elements", codexArgv(400, false, "gh issue view 1"), true},
		{"Codex distinct brace sequence elements", codexArgv(400, true), true},
	}
	for _, tt := range argvTests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := json.Marshal(map[string][]string{"command": tt.argv})
			if err != nil {
				t.Fatal(err)
			}
			j := runBounded(t, len(in), func() string {
				if IngestsCall("Bash", in) {
					return "ingests"
				}
				return ""
			})
			check(t, j, len(in), tt.flag)
		})
	}
}

// hostile builds commands from fragments chosen to stress the judge: brace
// expansions, nested parameter expansions that assign, replace and test,
// arithmetic, deep nesting, quotes, heredocs, long words and glued options.
type hostile struct{ rng *rand.Rand }

// pick returns one of its arguments at random.
func (h hostile) pick(first string, more ...string) string {
	k := h.rng.IntN(len(more) + 1)
	for i, s := range more {
		if i+1 == k {
			return s
		}
	}
	return first
}

func (h hostile) name() string { return h.pick("a", "b", "X", "HOME", "IFS", "PATH", "x", "1", "@") }

// param nests parameter expansions up to depth levels deep.
func (h hostile) param(depth int) string {
	operand := func() string {
		if depth == 0 || h.rng.IntN(3) == 0 {
			return h.pick("gh", "x", "$a", "$HOME", "g", "h issue view 1", `"$b"`, "$((1/0))", strings.Repeat("x", h.rng.IntN(64)))
		}
		return h.param(depth - 1)
	}
	v := h.name()
	switch h.rng.IntN(10) {
	case 0:
		return "${" + v + ":=" + operand() + "}"
	case 1:
		return "${" + v + "//" + h.pick("x", "?", "*", "/") + "/" + operand() + "}"
	case 2:
		return "${" + v + ":+" + operand() + "}"
	case 3:
		return "${" + v + ":-" + operand() + "}"
	case 4:
		return "${" + v + "/" + operand() + "/" + operand() + "}"
	case 5:
		return "${" + v + ":0:" + h.pick("0", "1", "$((1/0))") + "}"
	case 6:
		return "${" + v + h.pick("#", "##", "%", "%%") + operand() + "}"
	case 7:
		return "${" + h.pick("#", "!") + v + "}"
	case 8:
		return "${" + v + h.pick("@A", "@Q", ",,", "^^", "~~") + "}"
	}
	return "$" + v
}

func (h hostile) brace() string {
	switch h.rng.IntN(5) {
	case 0:
		return "{" + strconv.Itoa(h.rng.IntN(3)) + ".." + strconv.Itoa(h.rng.IntN(20000)) + "}"
	case 1:
		return "{" + h.pick("-9223372036854775808", "1", "9223372036854775807") + ".." + h.pick("-9223372036854775808", "2", "9223372036854775807") + ".." + h.pick("-9223372036854775808", "9223372036854775807", "-1", "0", "3") + "}"
	case 2:
		return "{" + h.pick("a", "z") + ".." + h.pick("A", "z") + "}"
	case 3:
		return "{gh,issue,{view,1}}"
	}
	return strings.Repeat("{a,b}", h.rng.IntN(24))
}

func (h hostile) arithmetic() string {
	n := h.rng.IntN(2000)
	switch h.rng.IntN(5) {
	case 0:
		return "$((" + strings.Repeat("!", n) + "1))"
	case 1:
		return "((" + strings.Repeat("1+", n) + "1))"
	case 2:
		return "$((i" + h.pick("=1", "++", "/0", "<<64", "**99") + "))"
	case 3:
		return "let " + strings.Repeat("~", n) + "1"
	}
	return "for ((" + strings.Repeat("-", n) + "1;;)); do gh issue view 1; done"
}

func (h hostile) nesting() string {
	n := h.rng.IntN(300)
	open, closing := h.pick("(", "$(", "{ ", "[[ ! ", "if ", "<(", "`"), h.pick(")", " }", " ]]", "; fi", "")
	return strings.Repeat(open, n) + h.pick("gh issue view 1", "a", "") + strings.Repeat(closing, h.rng.IntN(n+1))
}

func (h hostile) quoted() string {
	return h.pick(`'gh issue view 1'`, `"g''h $X issue"`, `$'\x67h issue view 1'`, `$"gh"`, `\g\h`, `"$(gh issue view 1)"`, `'`, `"`, `$'\0'`, `$'%s\x00x'`)
}

func (h hostile) heredoc() string {
	return h.pick("cat <<EOF\ngh issue view 1\nEOF\n", "sh <<'E'\ng''h issue view 1\nE\n", "cat <<EOF\n", "<<-X\n\tx\n\tX\n")
}

func (h hostile) long() string {
	n := h.rng.IntN(16 << 10)
	return h.pick(strings.Repeat("x", n), strings.Repeat("-xgh ", n/5), "--opt="+strings.Repeat("y", n)+"gh", "A="+strings.Repeat("z", n), "-"+strings.Repeat("S", n)+"gh")
}

func (h hostile) fragment() (frag string, brace bool) {
	switch h.rng.IntN(12) {
	case 0:
		return h.brace(), true
	case 1, 2:
		return h.param(h.rng.IntN(6)), false
	case 3:
		return h.arithmetic(), false
	case 4:
		return h.nesting(), false
	case 5:
		return h.quoted(), false
	case 6:
		return h.heredoc(), false
	case 7:
		return h.long(), false
	case 8:
		return h.pick("X=gh", "export X=", ": ${X:=g}", "( : ${X:=x} )", "f() { a; }", "false && "), false
	}
	return h.pick("gh", "issue", "view", "1", "curl", "a", "$X", "eval", "sh -c"), false
}

// command joins fragments up to a size drawn mostly small, sometimes large,
// and rarely past maxText. brace reports a brace expansion among them.
func (h hostile) command() (cmd string, brace bool) {
	size := h.rng.IntN(256)
	switch r := h.rng.IntN(100); {
	case r < 2:
		size = maxText - 4<<10 + h.rng.IntN(8<<10)
	case r < 10:
		size = h.rng.IntN(64 << 10)
	case r < 35:
		size = h.rng.IntN(4 << 10)
	}
	var b strings.Builder
	for b.Len() < size {
		f, br := h.fragment()
		brace = brace || br
		b.WriteString(f)
		b.WriteString(h.pick(" ", " ", "", "; ", " | ", " && ", "\n"))
	}
	return b.String(), brace
}

// TestIngestReasonHostileProperty judges seeded random hostile commands. None
// may crash the binary or allocate more than boundAlloc. Brace expansion pays
// for its whole output up front, so a short word may spend the byte budget;
// a command without one must also allocate at most linearly in its length and
// come back quickly. Under -race or -short only a tenth of the commands are
// judged, to keep the run short. The race detector slows judging about
// tenfold, so the time bound grows with it, and drops sync.Pool entries at
// random, so allocation is not repeatable there and only boundAlloc holds.
func TestIngestReasonHostileProperty(t *testing.T) {
	limitStack(t)
	n, timeLimit := 20000, 500*time.Millisecond
	if raceEnabled || testing.Short() {
		n = 2000
	}
	if raceEnabled {
		timeLimit *= 10
	}
	h := hostile{rand.New(rand.NewPCG(177, 2))}
	var peak uint64
	var linear float64
	var slowest time.Duration
	for i := range n {
		cmd, brace := h.command()
		j := judgeBounded(t, cmd)
		peak = max(peak, j.alloc)
		slowest = max(slowest, j.elapsed)
		if brace || strings.Count(cmd, "{") > strings.Count(cmd, "${") {
			continue
		}
		limit := uint64(256<<10 + 256*len(cmd))
		linear = max(linear, float64(j.alloc)/float64(limit))
		if j.alloc > limit && !raceEnabled {
			t.Errorf("command %d: %d bytes allocated %d KiB, over %d KiB", i, len(cmd), j.alloc>>10, limit>>10)
		}
		if j.elapsed > timeLimit {
			t.Errorf("command %d: %d bytes took %v", i, len(cmd), j.elapsed)
		}
	}
	t.Logf("%d commands: peak %d KiB allocated, at most %.0f%% of the linear bound, slowest %v", n, peak>>10, 100*linear, slowest)
}

// BenchmarkIngestReason judges TestIngestsCall's shell commands, or, with
// PRIORS_CORPUS set, the corpus TestCorpusRates reads, one command per op.
func BenchmarkIngestReason(b *testing.B) {
	var cmds []string
	if path := os.Getenv("PRIORS_CORPUS"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		for line := range strings.Lines(string(data)) {
			var cmd string
			if json.Unmarshal([]byte(line), &cmd) == nil {
				cmds = append(cmds, cmd)
			}
		}
	} else {
		for _, tt := range ingestsCallTests() {
			var in struct{ Command string }
			if json.Unmarshal([]byte(tt.input), &in) == nil && in.Command != "" {
				cmds = append(cmds, in.Command)
			}
		}
	}
	if len(cmds) == 0 {
		b.Fatal("no commands")
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		ingestReason(cmds[i%len(cmds)])
	}
}
