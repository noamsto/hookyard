package commit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

func recordingSSH(t *testing.T, dir, marker string) string {
	t.Helper()
	path := filepath.Join(dir, "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ran > "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunGitEnvTransportUsesPinnedSSH(t *testing.T) {
	pinnedMarker := filepath.Join(t.TempDir(), "pinned")
	pathMarker := filepath.Join(t.TempDir(), "path")
	envMarker := filepath.Join(t.TempDir(), "env")

	savedSSH := tools.SSH
	t.Cleanup(func() { tools.SSH = savedSSH })
	tools.SSH = recordingSSH(t, t.TempDir(), pinnedMarker)

	pathDir := t.TempDir()
	recordingSSH(t, pathDir, pathMarker)
	t.Setenv("PATH", pathDir+":"+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", recordingSSH(t, t.TempDir(), envMarker))
	t.Setenv("GIT_EXEC_PATH", t.TempDir())

	repo := t.TempDir()
	if out, err := exec.Command(tools.Git, "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	_, _ = runGitEnv(context.Background(), repo, nil, time.Minute, nil, "ls-remote", "ssh://example.invalid/x.git")

	if _, err := os.Stat(pinnedMarker); err != nil {
		t.Errorf("pinned ssh was not run: %v", err)
	}
	for name, marker := range map[string]string{"PATH ssh": pathMarker, "env GIT_SSH_COMMAND": envMarker} {
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s ran", name)
		}
	}
}
