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
		name string
		slot int
		flag string
	}{
		{"claude settings.json", 0, "--claude-settings"},
		{"codex config.toml", 1, "--codex-config"},
		{"cursor hooks.json", 2, "--cursor-hooks"},
		{"pi settings.json", 3, "--pi-settings"},
		{"pi bridge", 4, "--pi-settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "managed-elsewhere.json")
			if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link.json")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			piSettings := filepath.Join(dir, "pi-settings.json")
			paths := []string{
				filepath.Join(dir, "claude.json"),
				filepath.Join(dir, "config.toml"),
				filepath.Join(dir, "cursor.json"),
				piSettings,
				PiBridgePath(piSettings),
			}
			paths[tc.slot] = link

			err := CheckDestinations(
				Destination{"--claude-settings", paths[0]},
				Destination{"--codex-config", paths[1]},
				Destination{"--cursor-hooks", paths[2]},
				Destination{"--pi-settings", paths[3]},
				Destination{"--pi-settings", paths[4]},
			)
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

	err := CheckDestinations(
		Destination{"--claude-settings", filepath.Join(dir, "absent.json")},
		Destination{"--codex-config", existing},
		Destination{"--cursor-hooks", filepath.Join(dir, "absent-too.json")},
		Destination{"--pi-settings", filepath.Join(dir, "pi-settings.json")},
		Destination{"--pi-settings", PiBridgePath(filepath.Join(dir, "pi-settings.json"))},
	)
	if err != nil {
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

// MkdirAll walks a symlinked directory without a word, so the final-path check
// cannot see a link one segment up — and bin/ is the segment hookyard creates
// itself, for the one artifact that is executable code pi loads in-process.
func TestCheckPiBridgeDirRefusesASymlinkedBinDirectory(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "attacker-owned")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	piSettings := filepath.Join(dir, "settings.json")
	bridge := PiBridgePath(piSettings)
	if err := os.Symlink(elsewhere, filepath.Dir(bridge)); err != nil {
		t.Fatal(err)
	}

	// The file-level check is blind to it, which is why this one exists.
	if err := CheckDestinations(Destination{"--pi-settings", bridge}); err != nil {
		t.Fatalf("the final-path check was expected to see nothing here, got %v", err)
	}

	err := CheckPiBridgeDir("--pi-settings", bridge)
	if err == nil {
		t.Fatal("want a refusal for the symlinked bin/, got nil")
	}
	for _, want := range []string{filepath.Dir(bridge), elsewhere, "--pi-settings"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

func TestCheckPiBridgeDirAcceptsAMissingOrRealBinDirectory(t *testing.T) {
	dir := t.TempDir()
	absent := PiBridgePath(filepath.Join(dir, "settings.json"))
	if err := CheckPiBridgeDir("--pi-settings", absent); err != nil {
		t.Errorf("want a missing bin/ accepted, got %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(absent), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckPiBridgeDir("--pi-settings", absent); err != nil {
		t.Errorf("want a real bin/ accepted, got %v", err)
	}
}

// The rule is deliberately Pi's bin/ and nothing else: an engine's own config
// directory is exactly what a dotfiles repo links, and refusing those would
// break installs that work today.
func TestCheckDestinationsAcceptsASymlinkedEngineDirectory(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles-claude")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, ".claude")
	if err := os.Symlink(real, linked); err != nil {
		t.Fatal(err)
	}

	if err := CheckDestinations(Destination{"--claude-settings", filepath.Join(linked, "settings.json")}); err != nil {
		t.Errorf("want a settings.json inside a linked config directory accepted, got %v", err)
	}
}
