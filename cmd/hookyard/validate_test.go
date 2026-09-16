package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeValidateManifest writes a manifest naming exec verbatim, unlike
// writeTestManifest, which always points EXEC at a real executable it just
// created. --build-time's whole point is behaving differently depending on
// whether exec exists and whether its store root exists, so these tests need
// to control both independently.
func writeValidateManifest(t *testing.T, id, exec string) string {
	t.Helper()
	dir := t.TempDir()
	body := `{"handlers":[{"id":"` + id + `","exec":"` + exec + `","events":["pre_tool"],"engines":["cursor"]}]}`
	path := filepath.Join(dir, "hookyard.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole reason --build-time exists: a manifest that entered the store as
// a bare source path never gets its exec references scanned, so the store
// root below is never created in the sandbox even though the exec is
// perfectly fine on the target machine.
func TestValidateBuildTimeAcceptsExecUnderAnAbsentStoreRoot(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)
	exec := filepath.Join(store, "abc-jq", "bin", "jq") // store root "abc-jq" never created
	path := writeValidateManifest(t, "a", exec)

	if err := validate([]string{"--manifest", path, "--build-time"}); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
}

func TestValidateBuildTimeRejectsMissingExecUnderAPresentStoreRoot(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)
	root := filepath.Join(store, "abc-jq")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exec := filepath.Join(root, "bin", "jq") // never created
	path := writeValidateManifest(t, "missing-exec", exec)

	err := validate([]string{"--manifest", path, "--build-time"})
	if err == nil {
		t.Fatal("want an error for a missing exec under a present store root, got nil")
	}
	if !strings.Contains(err.Error(), "missing-exec") {
		t.Errorf("error should name the handler id, got: %v", err)
	}
}

// Plain validate keeps stat-ing every exec regardless of NIX_STORE or store
// root presence — the contract --build-time carves an exception out of, not
// replaces.
func TestValidatePlainRejectsWhatBuildTimeWouldSkipOrCatch(t *testing.T) {
	store := t.TempDir()
	t.Setenv("NIX_STORE", store)

	absentRootExec := filepath.Join(store, "abc-jq", "bin", "jq")
	sourceManifest := writeValidateManifest(t, "source-manifest", absentRootExec)
	if err := validate([]string{"--manifest", sourceManifest}); err == nil {
		t.Error("want an error for an exec that does not exist, even though --build-time would accept it")
	}

	root := filepath.Join(store, "abc-jq")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	presentRootExec := filepath.Join(root, "bin", "jq") // never created
	missingExecManifest := writeValidateManifest(t, "missing-exec", presentRootExec)
	if err := validate([]string{"--manifest", missingExecManifest}); err == nil {
		t.Error("want an error for a missing exec under a present store root")
	}
}

func TestValidateBuildTimeAndPluginRootAreMutuallyExclusive(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "unused.json") // never read; the flag conflict is caught first
	pluginRoot := t.TempDir()

	err := validate([]string{"--manifest", manifestPath, "--build-time", "--plugin-root", pluginRoot})
	if err == nil {
		t.Fatal("want an error combining --build-time with --plugin-root, got nil")
	}
	if !strings.Contains(err.Error(), "--build-time") || !strings.Contains(err.Error(), "--plugin-root") {
		t.Errorf("error should name both flags, got: %v", err)
	}
}
