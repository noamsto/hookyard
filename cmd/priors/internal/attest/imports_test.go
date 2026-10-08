package attest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

// TestNoProcessImports holds the index-time cost: attest itself never starts
// a process. Only direct imports are checked, since store and route reach
// tools transitively.
func TestNoProcessImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, "github.com/noamsto/hookyard/cmd/priors/internal/attest").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	imports := strings.Fields(string(out))
	if len(imports) == 0 {
		t.Fatal("go list printed no imports")
	}
	for _, banned := range []string{"os/exec", "github.com/noamsto/hookyard/cmd/priors/internal/tools"} {
		if slices.Contains(imports, banned) {
			t.Errorf("attest imports %s", banned)
		}
	}
}

// TestReviewedRunsNoGit verifies a fact whose entry has a long history with
// git swapped for a logging wrapper, and expects the log to stay empty.
func TestReviewedRunsNoGit(t *testing.T) {
	toolstest.Pin()
	if tools.Git == "" {
		t.Skip("git not installed")
	}
	f := newFixture(t)
	root := f.roots[route.StorePersonal].Path
	git := func(args ...string) {
		t.Helper()
		cmd := tools.Command(context.Background(), tools.Git, append([]string{
			"-C", root, "-c", "user.name=T", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false",
			// A detached auto gc would still be writing objects when TempDir is removed.
			"-c", "gc.auto=0", "-c", "maintenance.auto=false",
		}, args...)...)
		cmd.Env = route.RepoEnv("LC_ALL=C")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	for i := range 50 {
		f.put(route.StorePersonal, factName, fmt.Appendf(nil, "draft %d\n", i))
		git("add", "-A")
		git("commit", "-q", "-m", fmt.Sprintf("draft %d", i))
	}
	e, _ := f.attested(factRel, factName)
	git("add", "-A")
	git("commit", "-q", "-m", "attest")

	log := filepath.Join(t.TempDir(), "git.log")
	wrapper := filepath.Join(t.TempDir(), "git")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\nexec '%s' \"$@\"\n", log, tools.Git)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	realGit := tools.Git
	tools.Git = wrapper
	t.Cleanup(func() { tools.Git = realGit })

	v := f.verifier()
	if !v.Reviewed(e) {
		t.Fatalf("attested fact reads proposed: %q", v.Reports())
	}
	if b, err := os.ReadFile(log); err == nil || len(b) != 0 {
		t.Fatalf("Reviewed ran git: %q", b)
	}

	if out, err := tools.Command(context.Background(), tools.Git, "--version").CombinedOutput(); err != nil {
		t.Fatalf("wrapper: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "--version") {
		t.Fatalf("wrapper did not log: %q, %v", b, err)
	}
}
