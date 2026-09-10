package render

import (
	"os"
	"path/filepath"
	"testing"
)

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// Each writer's mode policy lives at its own call site, not in the shared
// atomic-write helper: these are files hookyard does not own, so a
// pre-existing one keeps its perm bits, while one hookyard creates lands
// 0600. No caller may skip the stat.
func TestWritersMode(t *testing.T) {
	tests := map[string]struct {
		fixture string // pre-existing content the writer accepts
		write   func(path string) error
	}{
		"WriteClaude": {
			fixture: "{}",
			write: func(path string) error {
				return WriteClaude(path, []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route"}})
			},
		},
		"WriteCodex": {
			fixture: "model_reasoning_effort = \"low\"\n",
			write: func(path string) error {
				return WriteCodex(path, []Entry{{Event: "PreToolUse", Command: "/x/bin/hookyard route"}})
			},
		},
		"WriteCursor": {
			fixture: "{}",
			write: func(path string) error {
				return WriteCursor(path, []Entry{{Event: "preToolUse", Command: "/x/bin/hookyard route"}})
			},
		},
	}

	for name, tc := range tests {
		t.Run(name+"/preserves an existing file's mode", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte(tc.fixture), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tc.write(path); err != nil {
				t.Fatal(err)
			}
			if mode := mustMode(t, path); mode != 0o644 {
				t.Errorf("mode = %v, want 0644 (the pre-existing file's mode)", mode)
			}
		})
		t.Run(name+"/lands 0600 on a file it creates", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := tc.write(path); err != nil {
				t.Fatal(err)
			}
			if mode := mustMode(t, path); mode != 0o600 {
				t.Errorf("mode = %v, want 0600", mode)
			}
		})
	}
}
