package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A link stands for something else managing that path — on this machine
// home-manager's store link at ~/.claude/settings.json. The assertion that
// matters is the last one: the link is still a link, so nothing detached it.
func TestCheckDestinationsRefusesASymlinkAndLeavesItIntact(t *testing.T) {
	for _, tc := range []struct {
		slot int
		flag string
	}{
		{0, "--claude-settings"},
		{1, "--codex-config"},
		{2, "--cursor-hooks"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "managed-elsewhere.json")
			if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link.json")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(dir, "claude.json"), filepath.Join(dir, "config.toml"), filepath.Join(dir, "cursor.json")}
			paths[tc.slot] = link

			err := CheckDestinations(paths[0], paths[1], paths[2])
			if err == nil {
				t.Fatal("want a refusal for the symlinked destination, got nil")
			}
			for _, want := range []string{link, target, tc.flag} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %q: %v", want, err)
				}
			}

			info, statErr := os.Lstat(link)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the destination is no longer a symlink: %v", info.Mode())
			}
		})
	}
}

func TestCheckDestinationsAcceptsRegularAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(existing, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CheckDestinations(filepath.Join(dir, "absent.json"), existing, filepath.Join(dir, "absent-too.json")); err != nil {
		t.Fatalf("want regular and missing destinations accepted, got %v", err)
	}
}

// The check guards the destination, not the writers: once it passes, both a
// pre-existing regular file and a missing one still render as before.
func TestWritersStillWriteRegularAndMissingDestinations(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(existing, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "settings.json")
	entries := []Entry{{Event: "preToolUse", Command: "/nix/store/x/bin/hookyard route --registered-for cursor --event pre_tool"}}

	if err := WriteCursor(existing, entries); err != nil {
		t.Fatal(err)
	}
	if err := WriteClaude(missing, []Entry{{Event: "PreToolUse", Command: entries[0].Command}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{existing, missing} {
		if !strings.Contains(readFile(t, path), Marker) {
			t.Errorf("%s has no hookyard entry after the write", path)
		}
	}
}
