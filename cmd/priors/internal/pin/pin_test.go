package pin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

func TestMain(m *testing.M) {
	toolstest.Pin()
	for _, k := range route.RepoLocatingEnv {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

const (
	pinned = "git@github.com:o/r.git"
	other  = "git@github.com:evil/r.git"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command(tools.Git, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// isolate points git's global config at an empty file and turns off the
// system one, and returns the global file.
func isolate(t *testing.T) string {
	t.Helper()
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return global
}

// repo makes a git checkout in base, with origin at url unless url is empty.
func repo(t *testing.T, base, url string) string {
	t.Helper()
	git(t, base, "init", "-q")
	if url != "" {
		git(t, base, "remote", "add", "origin", url)
	}
	return base
}

// subdir makes and returns a directory below dir.
func subdir(t *testing.T, dir string) string {
	t.Helper()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		setup  func(t *testing.T, base, global string) string
		// want is a substring of the error; empty means no error.
		want string
	}{
		{
			name: "missing dir", remote: pinned,
			setup: func(t *testing.T, base, _ string) string { return filepath.Join(base, "absent") },
		},
		{
			name:  "no pin, plain dir",
			setup: func(t *testing.T, base, _ string) string { return base },
		},
		{
			name:  "no pin, checkout without remotes",
			setup: func(t *testing.T, base, _ string) string { return repo(t, base, "") },
		},
		{
			name:  "no pin, checkout with origin",
			setup: func(t *testing.T, base, _ string) string { return repo(t, base, pinned) },
			want:  "has remote(s) origin but the trust file pins none",
		},
		{
			name:  "no pin, dir nested in a checkout with origin",
			setup: func(t *testing.T, base, _ string) string { return subdir(t, repo(t, base, pinned)) },
			want:  "has remote(s) origin but the trust file pins none",
		},
		{
			name:  "no pin, dir nested in a checkout without remotes",
			setup: func(t *testing.T, base, _ string) string { return subdir(t, repo(t, base, "")) },
		},
		{
			name: "pin, plain dir", remote: pinned,
			setup: func(t *testing.T, base, _ string) string { return base },
			want:  "not the top level of a git checkout",
		},
		{
			name: "pin, subdir of a checkout", remote: pinned,
			setup: func(t *testing.T, base, _ string) string { return subdir(t, repo(t, base, pinned)) },
			want:  "not the top level of a git checkout",
		},
		{
			name: "pin, matching origin", remote: pinned,
			setup: func(t *testing.T, base, _ string) string { return repo(t, base, pinned) },
		},
		{
			name: "pin, origin set elsewhere", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "remote", "set-url", "origin", other)
				return base
			},
			want: "origin url is " + other + ", not the pinned " + pinned,
		},
		{
			name: "pin, second origin url", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "--add", "remote.origin.url", other)
				return base
			},
			want: "not the pinned",
		},
		{
			name: "pin, insteadOf in the checkout", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "url."+other+".insteadOf", pinned)
				return base
			},
			want: "origin fetch url is " + other,
		},
		{
			name: "pin, pushInsteadOf in the checkout", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "url."+other+".pushInsteadOf", pinned)
				return base
			},
			want: "origin push url is " + other,
		},
		{
			name: "pin, pushurl", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "remote.origin.pushurl", other)
				return base
			},
			want: "origin push url is " + other,
		},
		{
			name: "pin, insteadOf in the global config", remote: pinned,
			setup: func(t *testing.T, base, global string) string {
				repo(t, base, pinned)
				git(t, base, "config", "--file", global, "url."+other+".insteadOf", pinned)
				return base
			},
			want: "origin fetch url is " + other,
		},
		{
			name: "pin, extra remote", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "remote", "add", "other", other)
				return base
			},
			want: "has remote(s) origin, other; the trust file pins origin alone",
		},
		{
			name: "pin, branch upstream on origin", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				repo(t, base, pinned)
				git(t, base, "config", "branch.main.remote", "origin")
				git(t, base, "config", "branch.main.pushRemote", "origin")
				git(t, base, "config", "remote.pushDefault", "origin")
				return base
			},
		},
		{
			name: "pin, branch remote is a url", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "branch.main.remote", other)
				return base
			},
			want: "branch.main.remote is " + other + ", not origin",
		},
		{
			name: "pin, branch remote is another name", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "branch.main.remote", "upstream")
				return base
			},
			want: "branch.main.remote is upstream, not origin",
		},
		{
			name: "pin, branch pushRemote", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "branch.main.pushRemote", other)
				return base
			},
			want: "branch.main.pushremote is " + other + ", not origin",
		},
		{
			name: "pin, pushDefault", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				git(t, repo(t, base, pinned), "config", "remote.pushDefault", "upstream")
				return base
			},
			want: "remote.pushdefault is upstream, not origin",
		},
		{
			name: "pin, no origin", remote: pinned,
			setup: func(t *testing.T, base, _ string) string { return repo(t, base, "") },
			want:  "origin url",
		},
		{
			name: "unpinned git", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				repo(t, base, pinned)
				saved := tools.Git
				tools.Git = ""
				t.Cleanup(func() { tools.Git = saved })
				return base
			},
			want: tools.ErrUnpinned.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global := isolate(t)
			dir := tt.setup(t, t.TempDir(), global)
			err := Check(context.Background(), dir, tt.remote)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Check = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("Check = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestCheckUnpinnedGitIsErrUnpinned(t *testing.T) {
	isolate(t)
	dir := repo(t, t.TempDir(), "")
	saved := tools.Git
	tools.Git = ""
	t.Cleanup(func() { tools.Git = saved })
	if err := Check(context.Background(), dir, ""); !errors.Is(err, tools.ErrUnpinned) {
		t.Fatalf("Check = %v, want ErrUnpinned", err)
	}
}

// pinPush sets the push pins for the length of t, restoring them after.
func pinPush(t *testing.T, sshConfig, knownHosts, gitDir string) {
	t.Helper()
	saved := [3]string{tools.SSHConfig, tools.KnownHosts, tools.PushGitDir}
	t.Cleanup(func() { tools.SSHConfig, tools.KnownHosts, tools.PushGitDir = saved[0], saved[1], saved[2] })
	tools.SSHConfig, tools.KnownHosts, tools.PushGitDir = sshConfig, knownHosts, gitDir
}

func TestPushEnvUnpinned(t *testing.T) {
	for _, pins := range [][3]string{
		{"", "", ""},
		{"/abs/ssh_config", "", "/abs/push.git"},
		{"", "/abs/known_hosts", "/abs/push.git"},
		{"ssh_config", "/abs/known_hosts", "/abs/push.git"},
		{"/abs/ssh_config", "/abs/known_hosts", ""},
		{"/abs/ssh_config", "/abs/known_hosts", "push.git"},
	} {
		pinPush(t, pins[0], pins[1], pins[2])
		if env, err := PushEnv(); err == nil {
			t.Errorf("PushEnv with %q = %v, want an error", pins, env)
		}
	}
}

func TestPushEnv(t *testing.T) {
	dir := t.TempDir()
	pinPush(t, filepath.Join(dir, "ssh config"), filepath.Join(dir, "known'hosts"), filepath.Join(dir, "push.git"))
	for k, v := range map[string]string{
		"PRIORS_TEST_SENTINEL": "1",
		"BASH_ENV":             "/evil/bash_env",
		"LD_PRELOAD":           "/evil/lib.so",
		"GIT_SSL_NO_VERIFY":    "1",
		"http_proxy":           "http://evil.invalid:3128",
		"GIT_SSH_COMMAND":      "evil-ssh",
		"GIT_SSH":              "/evil/ssh",
		"GIT_SSH_VARIANT":      "simple",
		"GIT_EXEC_PATH":        "/evil/libexec",
		"GIT_DIR":              "/evil/.git",
		"HOME":                 "/evil/home",
		"PATH":                 "/evil/bin",
	} {
		t.Setenv(k, v)
	}
	want := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_SSH_COMMAND='" + tools.SSH + "' -F '" + tools.SSHConfig + "' -o 'UserKnownHostsFile=" + dir + "/known'\\''hosts'",
		"GIT_SSH_VARIANT=ssh",
	}

	t.Run("without an agent", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "")
		if err := os.Unsetenv("SSH_AUTH_SOCK"); err != nil {
			t.Fatal(err)
		}
		assertEnv(t, want)
	})
	t.Run("with an agent", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")
		assertEnv(t, append(slices.Clone(want), "SSH_AUTH_SOCK=/run/agent.sock"))
	})
}

// assertEnv fails t unless PushEnv is exactly want. It names only the
// variables that differ, as a leak would carry the test's own environment.
func assertEnv(t *testing.T, want []string) {
	t.Helper()
	env, err := PushEnv()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(env, want) {
		return
	}
	var extra, missing []string
	for _, kv := range env {
		if !slices.Contains(want, kv) {
			k, _, _ := strings.Cut(kv, "=")
			extra = append(extra, k)
		}
	}
	for _, kv := range want {
		if !slices.Contains(env, kv) {
			missing = append(missing, kv)
		}
	}
	t.Errorf("PushEnv differs from the allowlist: unexpected %q, missing %q", extra, missing)
}
