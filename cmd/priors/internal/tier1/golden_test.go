package tier1

import (
	goflag "flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
)

var update = goflag.Bool("update", false, "rewrite the golden files")

var delimRE = regexp.MustCompile(`priors-[0-9a-f]{16}`)

// checkGolden compares out to testdata/name with the fence delimiter, which is
// random per call, replaced by a fixed one.
func checkGolden(t *testing.T, name, out string) {
	t.Helper()
	got := delimRE.ReplaceAllString(out, "priors-0000000000000000")
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rerun with -update to accept):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

func typed(name, typ, modified string) fact.Fact {
	f := mk(name, "about "+name, "global")
	f.Metadata.Type = typ
	f.Metadata.Modified = modified
	return f
}

// seedTyped fills the personal store with a fact of every type, one untyped,
// and one local-layer (flagged) fact for personalSession.
func seedTyped(t *testing.T, e env) {
	t.Helper()
	put(t, e.personal, "_global/fb-one.md", typed("fb-one", "feedback", "2026-09-08T00:00:00Z"))
	put(t, e.personal, "_global/proj-one.md", typed("proj-one", "project", "2026-09-07T00:00:00Z"))
	put(t, e.personal, "_global/ref-one.md", typed("ref-one", "reference", "2026-09-06T00:00:00Z"))
	put(t, e.personal, "_global/user-one.md", typed("user-one", "user", "2026-09-05T00:00:00Z"))
	put(t, e.personal, "_global/fb-two.md", typed("fb-two", "feedback", "2026-09-04T00:00:00Z"))
	put(t, e.personal, "_global/misc-one.md", typed("misc-one", "", "2026-09-03T00:00:00Z"))
	index(t, e.personal)
	put(t, e.localPersonal, "repo-a/learned.md", typed("learned", "project", "2026-09-09T00:00:00Z"))
	index(t, e.localPersonal)
}

func TestNonClaudeOutputUnchanged(t *testing.T) {
	e := newEnv(t)
	seedTyped(t, e)
	out, _ := e.assemble(personalSession)
	checkGolden(t, "flat.golden", out)
}
