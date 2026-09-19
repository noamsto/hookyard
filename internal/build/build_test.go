package build

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/vocab"
)

var update = flag.Bool("update", false, "rewrite golden files")

const manifestBody = `{"handlers":[
  {"id":"deny","exec":"handlers/deny.sh","events":["pre_tool"],"engines":["claude-code","codex"],"match":["Bash"]},
  {"id":"log","exec":"handlers/log.sh","events":["post_tool"],"engines":["claude-code"],"lane":"fire_and_forget"}
]}`

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

// setupPlugin lays out a plugin root with the manifest's two handler scripts,
// a manifest file naming them plugin-root-relative, and a fake binary to
// bundle. The manifest lives outside root, matching how an author invokes
// build against their own tree.
func setupPlugin(t *testing.T) (root, manifestPath, binaryPath string) {
	t.Helper()
	root = t.TempDir()
	writeFile(t, filepath.Join(root, "handlers", "deny.sh"), []byte("#!/bin/sh\n"), 0o755)
	writeFile(t, filepath.Join(root, "handlers", "log.sh"), []byte("#!/bin/sh\n"), 0o755)

	manifestPath = filepath.Join(t.TempDir(), "hookyard.json")
	writeFile(t, manifestPath, []byte(manifestBody), 0o644)

	binaryPath = filepath.Join(t.TempDir(), "fake-hookyard")
	writeFile(t, binaryPath, []byte("fake-binary"), 0o755)
	return root, manifestPath, binaryPath
}

func tablePath(root string) string { return filepath.Join(root, manifest.PluginTablePath) }

func TestBuildGolden(t *testing.T) {
	root, manifestPath, binaryPath := setupPlugin(t)

	if err := Build(vocab.ClaudeCode, Options{
		Manifests: []string{manifestPath},
		Out:       root,
		Name:      "example",
		Binary:    binaryPath,
	}); err != nil {
		t.Fatal(err)
	}

	generated := map[string]string{
		"hooks/hooks.json":           filepath.Join(root, "hooks", "hooks.json"),
		"hookyard/table.json":        filepath.Join(root, "hookyard", "table.json"),
		".claude-plugin/plugin.json": filepath.Join(root, ".claude-plugin", "plugin.json"),
		"bin/hookyard":               filepath.Join(root, "bin", "hookyard"),
	}
	for rel, path := range generated {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		golden := filepath.Join("testdata", "golden", filepath.FromSlash(rel))
		if *update {
			writeFile(t, golden, got, 0o644)
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: reading golden: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s mismatch\ngot:\n%s\nwant:\n%s", rel, got, want)
		}
	}

	modes := map[string]os.FileMode{
		filepath.Join(root, "hooks", "hooks.json"):                                              0o644,
		filepath.Join(root, "hookyard", "table.json"):                                           0o644,
		filepath.Join(root, ".claude-plugin", "plugin.json"):                                    0o644,
		filepath.Join(root, "bin", "hookyard"):                                                  0o755,
		filepath.Join(root, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH)): 0o755,
	}
	for path, want := range modes {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}

	binPath := filepath.Join(root, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH))
	gotBinary, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBinary) != "fake-binary" {
		t.Errorf("bundled binary = %q, want %q", gotBinary, "fake-binary")
	}
}

func TestBuildKeepsAnExistingPluginJSON(t *testing.T) {
	root, manifestPath, binaryPath := setupPlugin(t)
	pluginJSONPath := filepath.Join(root, ".claude-plugin", "plugin.json")
	existing := []byte(`{"name":"mine","description":"x"}`)
	writeFile(t, pluginJSONPath, existing, 0o644)

	if err := Build(vocab.ClaudeCode, Options{
		Manifests: []string{manifestPath},
		Out:       root,
		Binary:    binaryPath,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(pluginJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("plugin.json changed: got %s, want %s", got, existing)
	}
}

func TestBuildMergesHooksJSONAndIsIdempotent(t *testing.T) {
	root, manifestPath, binaryPath := setupPlugin(t)
	hooksPath := filepath.Join(root, "hooks", "hooks.json")
	foreign := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"/usr/bin/true","timeout":5}]}]}}`
	writeFile(t, hooksPath, []byte(foreign), 0o644)

	opts := Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath}
	if err := Build(vocab.ClaudeCode, opts); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "/usr/bin/true") {
		t.Fatalf("foreign hook was stripped: %s", first)
	}

	if err := Build(vocab.ClaudeCode, opts); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("rebuild is not idempotent\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestBuildErrors(t *testing.T) {
	t.Run("unsupported engine", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPlugin(t)
		err := Build(vocab.Codex, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
	})

	t.Run("missing handler file", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPlugin(t)
		if err := os.Remove(filepath.Join(root, "handlers", "deny.sh")); err != nil {
			t.Fatal(err)
		}
		err := Build(vocab.ClaudeCode, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
	})

	t.Run("absolute exec in manifest", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "handlers", "deny.sh"), []byte("#!/bin/sh\n"), 0o755)
		manifestPath := filepath.Join(t.TempDir(), "hookyard.json")
		body := fmt.Sprintf(`{"handlers":[{"id":"deny","exec":%q,"events":["pre_tool"],"engines":["claude-code"],"match":["Bash"]}]}`,
			filepath.Join(root, "handlers", "deny.sh"))
		writeFile(t, manifestPath, []byte(body), 0o644)
		binaryPath := filepath.Join(t.TempDir(), "fake-hookyard")
		writeFile(t, binaryPath, []byte("fake-binary"), 0o755)

		err := Build(vocab.ClaudeCode, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), "must be relative to the plugin root") {
			t.Errorf("got %v, want an error about the plugin-root-relative exec form", err)
		}
	})

	t.Run("plugin.json absent and name empty", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPlugin(t)
		err := Build(vocab.ClaudeCode, Options{Manifests: []string{manifestPath}, Out: root, Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), "--name is required") {
			t.Errorf("got %v, want an error about --name", err)
		}
	})

	t.Run("hooks.json is a symlink", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPlugin(t)
		hooksPath := filepath.Join(root, "hooks", "hooks.json")
		if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		writeFile(t, target, []byte(`{}`), 0o644)
		if err := os.Symlink(target, hooksPath); err != nil {
			t.Fatal(err)
		}
		err := Build(vocab.ClaudeCode, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
	})

	for _, dir := range []string{"bin", "hooks", "hookyard", ".claude-plugin"} {
		t.Run(dir+" is a symlink", func(t *testing.T) {
			root, manifestPath, binaryPath := setupPlugin(t)
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, dir)); err != nil {
				t.Fatal(err)
			}
			err := Build(vocab.ClaudeCode, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
			requireErrorAndNothingWritten(t, root, err)
			if !strings.Contains(err.Error(), outside) || !strings.Contains(err.Error(), "--out") {
				t.Errorf("got %v, want an error naming --out and the link target %s", err, outside)
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Errorf("build wrote %d entries through the %s link into %s", len(entries), dir, outside)
			}
		})
	}

	t.Run("malformed existing hooks.json", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPlugin(t)
		writeFile(t, filepath.Join(root, "hooks", "hooks.json"), []byte("not json"), 0o644)
		err := Build(vocab.ClaudeCode, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
	})
}

const piManifestBody = `{"handlers":[
  {"id":"guard","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["pi"],"match":["Bash"]},
  {"id":"log","exec":"handlers/log.sh","events":["post_tool"],"engines":["pi"],"lane":"fire_and_forget"}
],
"commands":[
  {"name":"aeye","description":"Open the aeye image carousel for this session","exec":"scripts/aeye-toggle"}
]}`

// setupPiPlugin is setupPlugin's pi counterpart: the two handler scripts plus
// the one command exec the manifest names, all plugin-root-relative.
func setupPiPlugin(t *testing.T) (root, manifestPath, binaryPath string) {
	t.Helper()
	root = t.TempDir()
	writeFile(t, filepath.Join(root, "handlers", "guard.sh"), []byte("#!/bin/sh\n"), 0o755)
	writeFile(t, filepath.Join(root, "handlers", "log.sh"), []byte("#!/bin/sh\n"), 0o755)
	writeFile(t, filepath.Join(root, "scripts", "aeye-toggle"), []byte("#!/bin/sh\n"), 0o755)

	manifestPath = filepath.Join(t.TempDir(), "hookyard.json")
	writeFile(t, manifestPath, []byte(piManifestBody), 0o644)

	binaryPath = filepath.Join(t.TempDir(), "fake-hookyard")
	writeFile(t, binaryPath, []byte("fake-binary"), 0o755)
	return root, manifestPath, binaryPath
}

func TestPiBuildGolden(t *testing.T) {
	root, manifestPath, binaryPath := setupPiPlugin(t)

	if err := Build(vocab.Pi, Options{
		Manifests: []string{manifestPath},
		Out:       root,
		Name:      "example",
		Binary:    binaryPath,
	}); err != nil {
		t.Fatal(err)
	}

	generated := map[string]string{
		"package.json":           filepath.Join(root, "package.json"),
		"extensions/hookyard.ts": filepath.Join(root, "extensions", "hookyard.ts"),
		"hookyard/table.json":    filepath.Join(root, "hookyard", "table.json"),
		"bin/hookyard":           filepath.Join(root, "bin", "hookyard"),
	}
	for rel, path := range generated {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		golden := filepath.Join("testdata", "golden-pi", filepath.FromSlash(rel))
		if *update {
			writeFile(t, golden, got, 0o644)
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: reading golden: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s mismatch\ngot:\n%s\nwant:\n%s", rel, got, want)
		}
	}

	modes := map[string]os.FileMode{
		filepath.Join(root, "package.json"):                                                     0o644,
		filepath.Join(root, "extensions", "hookyard.ts"):                                        0o644,
		filepath.Join(root, "hookyard", "table.json"):                                           0o644,
		filepath.Join(root, "bin", "hookyard"):                                                  0o755,
		filepath.Join(root, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH)): 0o755,
	}
	for path, want := range modes {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}

	binPath := filepath.Join(root, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH))
	gotBinary, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBinary) != "fake-binary" {
		t.Errorf("bundled binary = %q, want %q", gotBinary, "fake-binary")
	}
}

func TestPiBuildMergesPackageJSONAndIsIdempotent(t *testing.T) {
	root, manifestPath, binaryPath := setupPiPlugin(t)
	packageJSONPath := filepath.Join(root, "package.json")
	existing := []byte(`{"name":"mine","version":"1.0.0","pi":{"extensions":["./other.ts"],"settings":{"x":1}}}`)
	writeFile(t, packageJSONPath, existing, 0o644)

	opts := Options{Manifests: []string{manifestPath}, Out: root, Binary: binaryPath}
	if err := Build(vocab.Pi, opts); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(packageJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatalf("package.json is not valid JSON: %v\n%s", err, first)
	}
	if doc["name"] != "mine" || doc["version"] != "1.0.0" {
		t.Errorf("existing top-level keys were not kept: %s", first)
	}
	pi, _ := doc["pi"].(map[string]any)
	if pi == nil {
		t.Fatalf(`"pi" key missing or not an object: %s`, first)
	}
	if _, ok := pi["settings"]; !ok {
		t.Errorf(`existing "pi.settings" key was not kept: %s`, first)
	}
	extensions, _ := pi["extensions"].([]any)
	want := []any{"./other.ts", "./extensions/hookyard.ts"}
	if len(extensions) != len(want) || extensions[0] != want[0] || extensions[1] != want[1] {
		t.Errorf("pi.extensions = %v, want %v", extensions, want)
	}

	if err := Build(vocab.Pi, opts); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(packageJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("rebuild is not idempotent\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestPiBuildMergePackageJSONKeepsKeyOrder pins the failure mode a
// map[string]json.RawMessage merge would reintroduce: alphabetically
// re-sorting a hand-maintained package.json's top-level and pi.* keys turns a
// two-line hook change into a whole-file diff for the plugin author.
func TestPiBuildMergePackageJSONKeepsKeyOrder(t *testing.T) {
	existing := []byte(`{"scripts":{},"name":"mine","pi":{"settings":{"x":1},"extensions":["./other.ts"]},"version":"1.0.0"}`)
	got, err := mergePackageJSON("package.json", existing, "")
	if err != nil {
		t.Fatal(err)
	}

	positions := func(s string, keys ...string) []int {
		out := make([]int, len(keys))
		for i, key := range keys {
			out[i] = strings.Index(s, `"`+key+`"`)
			if out[i] < 0 {
				t.Fatalf("key %q missing from output: %s", key, s)
			}
		}
		return out
	}

	top := positions(string(got), "scripts", "name", "pi", "version")
	if !slices.IsSorted(top) {
		t.Errorf("top-level key order not preserved: %s", got)
	}
	nested := positions(string(got), "settings", "extensions")
	if !slices.IsSorted(nested) {
		t.Errorf("pi.* key order not preserved: %s", got)
	}
}

func TestPiBuildErrors(t *testing.T) {
	t.Run("missing handler file", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPiPlugin(t)
		if err := os.Remove(filepath.Join(root, "handlers", "guard.sh")); err != nil {
			t.Fatal(err)
		}
		err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
	})

	t.Run("missing command exec", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPiPlugin(t)
		if err := os.Remove(filepath.Join(root, "scripts", "aeye-toggle")); err != nil {
			t.Fatal(err)
		}
		err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), `command "aeye"`) {
			t.Errorf("got %v, want an error naming the command", err)
		}
	})

	t.Run("absolute exec in manifest", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "handlers", "guard.sh"), []byte("#!/bin/sh\n"), 0o755)
		manifestPath := filepath.Join(t.TempDir(), "hookyard.json")
		body := fmt.Sprintf(`{"handlers":[{"id":"guard","exec":%q,"events":["pre_tool"],"engines":["pi"],"match":["Bash"]}]}`,
			filepath.Join(root, "handlers", "guard.sh"))
		writeFile(t, manifestPath, []byte(body), 0o644)
		binaryPath := filepath.Join(t.TempDir(), "fake-hookyard")
		writeFile(t, binaryPath, []byte("fake-binary"), 0o755)

		err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), "must be relative to the plugin root") {
			t.Errorf("got %v, want an error about the plugin-root-relative exec form", err)
		}
	})

	t.Run("package.json absent and name empty", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPiPlugin(t)
		err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), "--name is required") {
			t.Errorf("got %v, want an error about --name", err)
		}
	})

	t.Run("malformed existing package.json", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPiPlugin(t)
		writeFile(t, filepath.Join(root, "package.json"), []byte("not json"), 0o644)
		err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), "not valid JSON") {
			t.Errorf("got %v, want an error about invalid JSON", err)
		}
	})

	for name, content := range map[string][]byte{
		"empty existing package.json":           {},
		"whitespace-only existing package.json": []byte("  \n\t "),
	} {
		t.Run(name, func(t *testing.T) {
			root, manifestPath, binaryPath := setupPiPlugin(t)
			writeFile(t, filepath.Join(root, "package.json"), content, 0o644)
			err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
			requireErrorAndNothingWritten(t, root, err)
			if !strings.Contains(err.Error(), "empty") {
				t.Errorf("got %v, want an error about the empty file", err)
			}
		})
	}

	t.Run("commands refused for claude-code", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "handlers", "guard.sh"), []byte("#!/bin/sh\n"), 0o755)
		writeFile(t, filepath.Join(root, "scripts", "aeye-toggle"), []byte("#!/bin/sh\n"), 0o755)
		manifestPath := filepath.Join(t.TempDir(), "hookyard.json")
		body := `{"handlers":[{"id":"guard","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"],"match":["Bash"]}],
"commands":[{"name":"aeye","description":"Open the aeye image carousel","exec":"scripts/aeye-toggle"}]}`
		writeFile(t, manifestPath, []byte(body), 0o644)
		binaryPath := filepath.Join(t.TempDir(), "fake-hookyard")
		writeFile(t, binaryPath, []byte("fake-binary"), 0o755)

		err := Build(vocab.ClaudeCode, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), "commands are supported only for pi") {
			t.Errorf("got %v, want an error naming pi as the only engine commands are supported for", err)
		}
	})

	t.Run("no handler claims pi", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "handlers", "guard.sh"), []byte("#!/bin/sh\n"), 0o755)
		manifestPath := filepath.Join(t.TempDir(), "hookyard.json")
		body := `{"handlers":[{"id":"guard","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"],"match":["Bash"]}]}`
		writeFile(t, manifestPath, []byte(body), 0o644)
		binaryPath := filepath.Join(t.TempDir(), "fake-hookyard")
		writeFile(t, binaryPath, []byte("fake-binary"), 0o755)

		err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
		if !strings.Contains(err.Error(), "no handler in the manifests claims pi") {
			t.Errorf("got %v, want an error about no handler claiming pi", err)
		}
	})

	for _, dir := range []string{"bin", "hookyard", "extensions"} {
		t.Run(dir+" is a symlink", func(t *testing.T) {
			root, manifestPath, binaryPath := setupPiPlugin(t)
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, dir)); err != nil {
				t.Fatal(err)
			}
			err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
			requireErrorAndNothingWritten(t, root, err)
			if !strings.Contains(err.Error(), outside) || !strings.Contains(err.Error(), "--out") {
				t.Errorf("got %v, want an error naming --out and the link target %s", err, outside)
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Errorf("build wrote %d entries through the %s link into %s", len(entries), dir, outside)
			}
		})
	}

	t.Run("package.json is a symlink", func(t *testing.T) {
		root, manifestPath, binaryPath := setupPiPlugin(t)
		packageJSONPath := filepath.Join(root, "package.json")
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		writeFile(t, target, []byte(`{}`), 0o644)
		if err := os.Symlink(target, packageJSONPath); err != nil {
			t.Fatal(err)
		}
		err := Build(vocab.Pi, Options{Manifests: []string{manifestPath}, Out: root, Name: "example", Binary: binaryPath})
		requireErrorAndNothingWritten(t, root, err)
	})
}

func requireErrorAndNothingWritten(t *testing.T, root string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if _, statErr := os.Stat(tablePath(root)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("table.json exists after a failed build (stat err: %v)", statErr)
	}
}

// launcherSupported reports whether this host's GOOS/GOARCH is one the
// launcher script itself recognizes, so the "supported host" and "missing
// binary" subtests below exercise the real uname on a host they can pass on.
func launcherSupported() bool {
	switch runtime.GOOS {
	case "linux", "darwin":
	default:
		return false
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		return false
	}
	return true
}

func TestLauncherSelectsTheBundledBinary(t *testing.T) {
	if !launcherSupported() {
		t.Skipf("launcher does not recognize %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "bin", "hookyard")
	writeFile(t, launcherPath, launcher, 0o755)
	binPath := filepath.Join(dir, "bin", fmt.Sprintf("hookyard-%s-%s", runtime.GOOS, runtime.GOARCH))
	writeFile(t, binPath, []byte("#!/bin/sh\necho \"args:$*\"; cat\n"), 0o755)

	cmd := exec.Command("sh", launcherPath, "a", "b c")
	cmd.Stdin = strings.NewReader("payload")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("launcher failed: %v", err)
	}
	if got, want := string(out), "args:a b c\npayload"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLauncherExitsCleanlyOnAnUnsupportedHost(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "bin", "hookyard")
	writeFile(t, launcherPath, launcher, 0o755)

	fakeUnameDir := t.TempDir()
	writeFile(t, filepath.Join(fakeUnameDir, "uname"), []byte("#!/bin/sh\necho Plan9\n"), 0o755)

	cmd := exec.Command("sh", launcherPath)
	cmd.Env = append(os.Environ(), "PATH="+fakeUnameDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("launcher should exit 0 on an unsupported host: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got stdout %q, want none", out)
	}
}

func TestLauncherExitsCleanlyWhenTheBinaryIsMissing(t *testing.T) {
	if !launcherSupported() {
		t.Skipf("launcher does not recognize %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "bin", "hookyard")
	writeFile(t, launcherPath, launcher, 0o755)

	cmd := exec.Command("sh", launcherPath)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("launcher should exit 0 when the binary is missing: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got stdout %q, want none", out)
	}
}
