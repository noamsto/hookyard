package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBuildFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// setupBuildPlugin lays out a plugin root with one handler script and a
// manifest naming it plugin-root-relative, matching how an author invokes
// build against their own tree. The manifest lives outside root.
func setupBuildPlugin(t *testing.T) (root, manifestPath string) {
	t.Helper()
	root = t.TempDir()
	writeBuildFile(t, filepath.Join(root, "handlers", "guard.sh"), "#!/bin/sh\n", 0o755)
	manifestPath = filepath.Join(t.TempDir(), "hookyard.json")
	writeBuildFile(t, manifestPath, `{"handlers":[`+
		`{"id":"guard","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["claude-code"],"match":["Bash"]}]}`, 0o644)
	return root, manifestPath
}

func TestValidatePluginRootAcceptsAGoodTree(t *testing.T) {
	root, manifestPath := setupBuildPlugin(t)
	if err := validate([]string{"--manifest", manifestPath, "--plugin-root", root}); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
}

func TestValidatePluginRootRejectsAMissingExec(t *testing.T) {
	root, manifestPath := setupBuildPlugin(t)
	if err := os.Remove(filepath.Join(root, "handlers", "guard.sh")); err != nil {
		t.Fatal(err)
	}

	err := validate([]string{"--manifest", manifestPath, "--plugin-root", root})

	if err == nil {
		t.Fatal("want an error for the missing exec, got nil")
	}
}

func TestRunBuildRequiresEngine(t *testing.T) {
	root, manifestPath := setupBuildPlugin(t)

	err := runBuild([]string{"--manifest", manifestPath, "--out", root})

	if err == nil || !strings.Contains(err.Error(), "--engine") {
		t.Fatalf("want an error naming --engine, got %v", err)
	}
}

func TestRunBuildRequiresOut(t *testing.T) {
	_, manifestPath := setupBuildPlugin(t)

	err := runBuild([]string{"--engine", "claude-code", "--manifest", manifestPath})

	if err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("want an error naming --out, got %v", err)
	}
}

func TestRunBuildRequiresManifest(t *testing.T) {
	root, _ := setupBuildPlugin(t)

	err := runBuild([]string{"--engine", "claude-code", "--out", root})

	if err == nil || !strings.Contains(err.Error(), "--manifest") {
		t.Fatalf("want an error naming --manifest, got %v", err)
	}
}

// A nonexistent --out is refused by internal/build before anything else is
// touched (checked ahead of manifest loading, so a typo'd path fails fast).
func TestRunBuildNonexistentOutErrorsAndWritesNothing(t *testing.T) {
	_, manifestPath := setupBuildPlugin(t)
	out := filepath.Join(t.TempDir(), "does-not-exist")

	err := runBuild([]string{"--engine", "claude-code", "--manifest", manifestPath, "--out", out})

	if err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("want an error naming --out, got %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("want nothing written at --out, got stat err %v", statErr)
	}
}

func TestRunBuildHappyPath(t *testing.T) {
	root, manifestPath := setupBuildPlugin(t)

	err := runBuild([]string{
		"--engine", "claude-code",
		"--manifest", manifestPath,
		"--out", root,
		"--name", "example",
	})

	if err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	for _, rel := range []string{filepath.Join("hooks", "hooks.json"), filepath.Join("hookyard", "table.json")} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("want %s to exist: %v", rel, err)
		}
	}
}

func TestRunBuildHappyPathPi(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, filepath.Join(root, "handlers", "guard.sh"), "#!/bin/sh\n", 0o755)
	manifestPath := filepath.Join(t.TempDir(), "hookyard.json")
	writeBuildFile(t, manifestPath, `{"handlers":[`+
		`{"id":"guard","exec":"handlers/guard.sh","events":["pre_tool"],"engines":["pi"],"match":["Bash"]}]}`, 0o644)

	err := runBuild([]string{
		"--engine", "pi",
		"--manifest", manifestPath,
		"--out", root,
		"--name", "example",
	})

	if err != nil {
		t.Fatalf("runBuild: %v", err)
	}
	for _, rel := range []string{"package.json", filepath.Join("extensions", "hookyard.ts"), filepath.Join("hookyard", "table.json")} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("want %s to exist: %v", rel, err)
		}
	}
}

// The unsupported-engine error must name the engines build does support,
// rather than repeating the days when Claude Code was the only one.
func TestRunBuildUnsupportedEngineNamesSupportedEngines(t *testing.T) {
	root, manifestPath := setupBuildPlugin(t)

	err := runBuild([]string{"--engine", "codex", "--manifest", manifestPath, "--out", root, "--name", "example"})

	if err == nil || !strings.Contains(err.Error(), "claude-code") || !strings.Contains(err.Error(), "pi") {
		t.Fatalf("got %v, want an error naming claude-code and pi as supported", err)
	}
}
