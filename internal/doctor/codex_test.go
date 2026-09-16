package doctor

import (
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/vocab"
)

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
