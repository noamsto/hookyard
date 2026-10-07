package commit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// recordingSSH writes an ssh stand-in to dir that records its argv, one
// argument per line, in marker and fails.
func recordingSSH(t *testing.T, dir, marker string) string {
	t.Helper()
	path := filepath.Join(dir, "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+marker+"'\nexit 1\n"), 0o755); err != nil {
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

// git resolves a subcommand it does not build in through GIT_EXEC_PATH first,
// so an inherited one would let a shim stand in for git's own helpers.
func TestRunGitEnvDropsExecPath(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	execPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(execPath, "git-frobnicate"), []byte("#!/bin/sh\necho ran > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_EXEC_PATH", execPath)

	repo := t.TempDir()
	if out, err := exec.Command(tools.Git, "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	_, _ = runGitEnv(context.Background(), repo, nil, time.Minute, nil, "frobnicate")

	if _, err := os.Stat(marker); err == nil {
		t.Error("a helper from the inherited GIT_EXEC_PATH ran")
	}
}

func TestPushUsesPinnedSSHConfigAndKnownHosts(t *testing.T) {
	fx, _ := withRemote(t)
	const url = "ssh://example.invalid/x.git"
	git(t, fx.dir(), "remote", "set-url", "origin", url)
	fx.root.Remote = url

	pinnedMarker := filepath.Join(t.TempDir(), "pinned")
	savedSSH := tools.SSH
	t.Cleanup(func() { tools.SSH = savedSSH })
	tools.SSH = recordingSSH(t, t.TempDir(), pinnedMarker)

	others := map[string]string{
		"GIT_SSH_COMMAND": filepath.Join(t.TempDir(), "ssh-command"),
		"GIT_SSH":         filepath.Join(t.TempDir(), "ssh"),
		"core.sshCommand": filepath.Join(t.TempDir(), "core-ssh-command"),
	}
	t.Setenv("GIT_SSH_COMMAND", recordingSSH(t, t.TempDir(), others["GIT_SSH_COMMAND"]))
	t.Setenv("GIT_SSH", recordingSSH(t, t.TempDir(), others["GIT_SSH"]))
	git(t, fx.dir(), "config", "--global", "core.sshCommand", recordingSSH(t, t.TempDir(), others["core.sshCommand"]))

	fx.put(t, "_global/good-fact.md", cleanFact("good-fact"))
	fx.index(t)
	if w := fx.checkout(t); !strings.Contains(w, "git push failed") {
		t.Errorf("warning = %q, want a failed push", w)
	}

	raw, err := os.ReadFile(pinnedMarker)
	if err != nil {
		t.Fatalf("pinned ssh was not run: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, pair := range [][2]string{{"-F", tools.SSHConfig}, {"-o", "UserKnownHostsFile=" + tools.KnownHosts}} {
		if i := slices.Index(argv, pair[1]); i < 1 || argv[i-1] != pair[0] {
			t.Errorf("ssh argv %q lacks %s %s", argv, pair[0], pair[1])
		}
	}
	for name, marker := range others {
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s ran", name)
		}
	}
}
