package gate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// maxGenFailures stops a broken generator run from flooding the log.
const maxGenFailures = 20

type spelling struct {
	name  string
	spell func(words []string) string
}

type quoting struct {
	name  string
	quote func(script string) string
}

// position puts a script where it runs. An unquoted position takes the script
// itself, so it has no quoting axis; every other one takes it as one shell word.
type position struct {
	name   string
	quoted bool
	// runs marks the positions the bash oracle can execute with stubs alone.
	runs bool
	// inner leads the script inside its quoting.
	inner string
	place func(w string) string
}

// variant is a position with one quoting, turning a script into command text.
type variant struct {
	name string
	wrap func(script string) string
}

// firstWord rewrites only the first word and keeps the rest.
func firstWord(f func(n string) string) func([]string) string {
	return func(words []string) string {
		return strings.Join(append([]string{f(words[0])}, words[1:]...), " ")
	}
}

var spellings = []spelling{
	{"plain", firstWord(func(n string) string { return n })},
	{"empty-single", firstWord(func(n string) string { return n[:1] + "''" + n[1:] })},
	{"empty-double", firstWord(func(n string) string { return n[:1] + `""` + n[1:] })},
	{"quoted-first-char", firstWord(func(n string) string { return `"` + n[:1] + `"` + n[1:] })},
	{"backslash-every-char", firstWord(func(n string) string {
		var b strings.Builder
		for _, r := range n {
			b.WriteString(`\` + string(r))
		}
		return b.String()
	})},
	{"ansi-c-first-char", firstWord(func(n string) string { return fmt.Sprintf(`$'\x%02x'%s`, n[0], n[1:]) })},
	{"locale-empty", firstWord(func(n string) string { return n[:1] + `$""` + n[1:] })},
	{"empty-single-every-word", func(words []string) string {
		spelled := make([]string, len(words))
		for i, w := range words {
			spelled[i] = w
			if len(w) > 1 {
				spelled[i] = w[:1] + "''" + w[1:]
			}
		}
		return strings.Join(spelled, " ")
	}},
}

var (
	doubleEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	ansiEscaper   = strings.NewReplacer(`\`, `\\`, `'`, `\'`)
)

const shellMeta = " \t\n;&|()<>*?[]#~{}!`'\"\\$"

var quotings = []quoting{
	{"single", func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }},
	{"double", func(s string) string { return `"` + doubleEscaper.Replace(s) + `"` }},
	{"ansi-c", func(s string) string { return "$'" + ansiEscaper.Replace(s) + "'" }},
	{"backslash", func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if strings.ContainsRune(shellMeta, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		return b.String()
	}},
}

func prefixed(prefix string) func(string) string {
	return func(w string) string { return prefix + w }
}

var positions = []position{
	{"top-level", false, true, "", func(s string) string { return s }},
	{"bash -c", true, true, "", func(w string) string { return "bash -c " + w }},
	{"sh -c", true, true, "", func(w string) string { return "sh -c " + w }},
	{"eval", true, true, "", func(w string) string { return "eval " + w }},
	{"printf | sh", true, true, "", func(w string) string { return "printf '%s\\n' " + w + " | sh" }},
	{"ssh -o ProxyCommand=", true, false, "", func(w string) string { return "ssh -o ProxyCommand=" + w + " host" }},
	{"ssh -oProxyCommand=", true, false, "", func(w string) string { return "ssh -oProxyCommand=" + w + " host" }},
	{"git -c core.pager=", true, false, "", func(w string) string { return "git -c core.pager=" + w + " log" }},
	{"flock --command=", true, false, "", func(w string) string { return "flock --command=" + w + " /tmp/l" }},
	{"rsync --rsh=", true, false, "", func(w string) string { return "rsync --rsh=" + w + " a b" }},
	{"GIT_PAGER=", true, false, "", func(w string) string { return "GIT_PAGER=" + w + " git log" }},
	{"env -S", true, false, "", prefixed("env -S ")},
	{"env --split-string=", true, false, "", prefixed("env --split-string=")},
	{"rg --pre", true, false, "", func(w string) string { return "rg --pre " + w + " ." }},
	{"git -c alias.v=!", true, false, "!", func(w string) string { return "git -c alias.v=" + w + " v" }},
	{"eval words", false, true, "", prefixed("eval ")},
	{"echo words | sh", false, true, "", func(s string) string { return "echo " + s + " | sh" }},
	{"ssh host words", false, false, "", prefixed("ssh host ")},
}

// variants lists every way to place a script, restricted to the positions the
// oracle can run when runsOnly is set.
func variants(runsOnly bool) []variant {
	var out []variant
	for _, p := range positions {
		if runsOnly && !p.runs {
			continue
		}
		if !p.quoted {
			out = append(out, variant{p.name, p.place})
			continue
		}
		for _, q := range quotings {
			out = append(out, variant{p.name + " / " + q.name, func(s string) string { return p.place(q.quote(p.inner + s)) }})
		}
	}
	return out
}

var (
	envPrefixes = []string{"", "FOO=1 "}
	wrapperList = []string{"", "sudo ", "env A=1 ", "timeout 5 ", "nice -n 5 ", "setsid ", "nohup "}
)

// failures counts findings and stops the test once there are too many to read.
type failures struct{ n int }

func (f *failures) report(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Errorf(format, args...)
	if f.n++; f.n >= maxGenFailures {
		t.Fatalf("stopping after %d failures", f.n)
	}
}

func jsonCommand(t *testing.T, cmd string) json.RawMessage {
	t.Helper()
	in, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// generated calls fn with every composition of the axes, naming them in desc.
func generated(cmds []string, fn func(cmd, desc string)) {
	for _, c := range cmds {
		words := strings.Fields(c)
		for _, s := range spellings {
			spelled := s.spell(words)
			for _, v := range variants(false) {
				for _, env := range envPrefixes {
					for _, w := range wrapperList {
						desc := fmt.Sprintf("cmd=%q spelling=%s position=%s env=%q wrapper=%q placement=inside", c, s.name, v.name, env, w)
						fn(v.wrap(env+w+spelled), desc)
						// Outside, the prefix runs the position's own command; at the
						// top level that is the inside text, and with no prefix there
						// is nothing to move.
						if v.name == positions[0].name || env+w == "" {
							continue
						}
						fn(env+w+v.wrap(spelled), strings.TrimSuffix(desc, "inside")+"outside")
					}
				}
			}
		}
	}
}

func TestIngestsCallGenerated(t *testing.T) {
	t.Parallel()
	cmds := []string{"gh issue view 1", "gh api repos/o/r/issues", "glab mr view 1", "curl example.org"}
	var f failures
	n := 0
	generated(cmds, func(cmd, desc string) {
		n++
		// A parser regression must not hide behind rule (b), which flags the
		// quote spellings on its own.
		switch reason := ingestReason(cmd); reason {
		case "":
			f.report(t, "not flagged: %s\n  text: %s", desc, cmd)
		case "fallback", "budget":
			f.report(t, "flagged by %q, not the parser path: %s\n  text: %s", reason, desc, cmd)
		}
		// The JSON path runs the same judge, so it is checked for the inside
		// placement only, to keep the run short under -race.
		if strings.HasSuffix(desc, "inside") && !IngestsCall("Bash", jsonCommand(t, cmd)) {
			f.report(t, "IngestsCall false: %s\n  text: %s", desc, cmd)
		}
	})
	t.Logf("%d compositions", n)
}

func TestIngestsCallGeneratedClean(t *testing.T) {
	t.Parallel()
	cmds := []string{"gh auth status", "gh pr create --title t"}
	var f failures
	n := 0
	generated(cmds, func(cmd, desc string) {
		n++
		if reason := ingestReason(cmd); reason != "" {
			f.report(t, "flagged by %q: %s\n  text: %s", reason, desc, cmd)
		}
	})
	t.Logf("%d compositions", n)

	for _, cmd := range []string{"rg gh", "grep curl", "which gh"} {
		if reason := ingestReason(cmd); reason != "" {
			f.report(t, "inert %q flagged by %q", cmd, reason)
		}
	}
}

// TestGeneratedSpellingsRunInBash checks the generator against bash, not the
// matcher: every spelling and quoting it builds must still run the command, so
// the other tests judge text a shell would really execute. Only stubs are on
// PATH, and no real gh, glab or curl runs.
func TestGeneratedSpellingsRunInBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	dir := t.TempDir()
	for _, name := range []string{"bash", "sh"} {
		if err := os.Symlink(bash, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!" + bash + "\nprintf 'RAN %s' \"${0##*/}\"; printf ' %s' \"$@\"; echo\n"
	for _, name := range []string{"gh", "glab", "curl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmds := []string{"gh issue view 1", "gh api repos/o/r/issues", "glab mr view 1", "curl example.org"}
	var f failures
	n := 0
	for _, c := range cmds {
		words := strings.Fields(c)
		want := "RAN " + c + "\n"
		for _, s := range spellings {
			spelled := s.spell(words)
			for _, v := range variants(true) {
				text := v.wrap(spelled)
				n++
				run := exec.Command(bash, "-c", text)
				run.Dir = dir
				run.Env = []string{"PATH=" + dir}
				var stderr bytes.Buffer
				run.Stderr = &stderr
				out, err := run.Output()
				if err != nil || string(out) != want {
					f.report(t, "cmd=%q spelling=%s position=%s\n  text: %s\n  got %q, err %v, stderr %q", c, s.name, v.name, text, out, err, stderr.String())
				}
			}
		}
	}
	t.Logf("%d scripts run", n)
}
