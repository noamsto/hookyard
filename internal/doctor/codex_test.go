package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
)

// withCodexOnPath points PATH at a fresh directory holding only a "codex"
// stub, so the workspace-trust check resolves codex without reaching the
// host's real install.
func withCodexOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// TestCodexWorkspaceTrustFollowsConfigWhenInstalled: with codex on PATH the
// trust level in config.toml decides Pass vs Fail.
func TestCodexWorkspaceTrustFollowsConfigWhenInstalled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trust string
		want  Status
	}{
		{"trusted", "trusted", Pass},
		{"untrusted", "untrusted", Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCodexOnPath(t)
			root := t.TempDir()
			dir := filepath.Join(root, "project")
			codexHome := filepath.Join(root, "codex")
			writeFile(t, filepath.Join(codexHome, "config.toml"), []byte(
				"[projects.\""+dir+"\"]\ntrust_level = \""+tc.trust+"\"\n"))

			f := findByCheck(t, codexFindings(Paths{CodexHome: codexHome}, dir), "workspace trust")
			if f.Status != tc.want {
				t.Fatalf("status = %v, want %v; detail=%q", f.Status, tc.want, f.Detail)
			}
		})
	}
}

func TestCodexRegistrationCountsCommandsNotBeginComments(t *testing.T) {
	root := t.TempDir()
	cmd := `/nix/store/x/bin/hookyard route --registered-for codex --event pre_tool`
	dup := "[[hooks.PreToolUse.hooks]]\ncommand = \"" + cmd + "\"\n"
	config := writeFile(t, root+"/config.toml", []byte(strings.Repeat(dup, 3)))

	f := findByCheck(t, []Finding{codexRegistration(config)}, "hookyard registered")
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "3") {
		t.Errorf("detail = %q, want the decimal count 3", f.Detail)
	}
	if f.Engine != vocab.Codex {
		t.Errorf("engine = %v, want Codex", f.Engine)
	}
}

func TestCodexRegistrationPassesWhenOne(t *testing.T) {
	root := t.TempDir()
	config := writeFile(t, root+"/config.toml", []byte(
		`[[hooks.PreToolUse.hooks]]
command = "/nix/store/x/bin/hookyard route --registered-for codex --event pre_tool"
`))

	f := findByCheck(t, []Finding{codexRegistration(config)}, "hookyard registered")
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	want := "1 hookyard entry in " + config
	if f.Detail != want {
		t.Errorf("detail = %q, want %q", f.Detail, want)
	}
}

func TestCodexRegistrationPassesWhenDistinctEvents(t *testing.T) {
	root := t.TempDir()
	config := writeFile(t, root+"/config.toml", []byte(
		`[[hooks.PreToolUse.hooks]]
command = "/nix/store/x/bin/hookyard route --registered-for codex --event pre_tool --state-dir /s"

[[hooks.PostToolUse.hooks]]
command = "/nix/store/x/bin/hookyard route --registered-for codex --event post_tool --state-dir /s"
`))

	f := findByCheck(t, []Finding{codexRegistration(config)}, "hookyard registered")
	if f.Status != Pass {
		t.Fatalf("status = %v, want Pass; detail=%q", f.Status, f.Detail)
	}
	if !strings.Contains(f.Detail, "2 hookyard entries") {
		t.Errorf("detail = %q, want the count 2", f.Detail)
	}
}

func TestCodexRegistrationFailsWhenNone(t *testing.T) {
	root := t.TempDir()
	config := writeFile(t, root+"/config.toml", []byte("[hooks.state]\nreviewed = true\n"))

	f := findByCheck(t, []Finding{codexRegistration(config)}, "hookyard registered")
	if f.Status != Fail {
		t.Fatalf("status = %v, want Fail; detail=%q", f.Status, f.Detail)
	}
	want := "no hookyard entry in " + config + "; run hookyard install"
	if f.Detail != want {
		t.Errorf("detail = %q, want %q", f.Detail, want)
	}
}
