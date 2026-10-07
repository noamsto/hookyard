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
			name: "pin, plain dir", remote: pinned,
			setup: func(t *testing.T, base, _ string) string { return base },
			want:  "not the top level of a git checkout",
		},
		{
			name: "pin, subdir of a checkout", remote: pinned,
			setup: func(t *testing.T, base, _ string) string {
				sub := filepath.Join(repo(t, base, pinned), "sub")
				if err := os.Mkdir(sub, 0o700); err != nil {
					t.Fatal(err)
				}
				return sub
			},
			want: "not the top level of a git checkout",
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

func TestPushEnvUnpinned(t *testing.T) {
	saved := [2]string{tools.SSHConfig, tools.KnownHosts}
	t.Cleanup(func() { tools.SSHConfig, tools.KnownHosts = saved[0], saved[1] })
	for _, pins := range [][2]string{{"", ""}, {"/abs/ssh_config", ""}, {"", "/abs/known_hosts"}, {"ssh_config", "/abs/known_hosts"}} {
		tools.SSHConfig, tools.KnownHosts = pins[0], pins[1]
		if env, err := PushEnv(); err == nil {
			t.Errorf("PushEnv with %q = %v, want an error", pins, env)
		}
	}
}

func TestPushEnv(t *testing.T) {
	saved := [2]string{tools.SSHConfig, tools.KnownHosts}
	t.Cleanup(func() { tools.SSHConfig, tools.KnownHosts = saved[0], saved[1] })
	dir := t.TempDir()
	tools.SSHConfig = filepath.Join(dir, "ssh config")
	tools.KnownHosts = filepath.Join(dir, "known'hosts")
	t.Setenv("GIT_SSH_COMMAND", "evil-ssh")
	t.Setenv("GIT_SSH", "/evil/ssh")
	t.Setenv("GIT_SSH_VARIANT", "simple")
	t.Setenv("GIT_EXEC_PATH", "/evil/libexec")
	t.Setenv("GIT_DIR", "/evil/.git")

	env, err := PushEnv()
	if err != nil {
		t.Fatal(err)
	}
	values := func(key string) []string {
		var vs []string
		for _, kv := range env {
			if k, v, _ := strings.Cut(kv, "="); k == key {
				vs = append(vs, v)
			}
		}
		return vs
	}
	wantSSH := "'" + tools.SSH + "' -F '" + tools.SSHConfig + "' -o 'UserKnownHostsFile=" + dir + "/known'\\''hosts'"
	if got := values("GIT_SSH_COMMAND"); !slices.Equal(got, []string{wantSSH}) {
		t.Errorf("GIT_SSH_COMMAND = %q, want [%q]", got, wantSSH)
	}
	if got := values("GIT_SSH_VARIANT"); !slices.Equal(got, []string{"ssh"}) {
		t.Errorf("GIT_SSH_VARIANT = %q, want [ssh]", got)
	}
	for _, k := range []string{"GIT_SSH", "GIT_EXEC_PATH", "GIT_DIR"} {
		if got := values(k); got != nil {
			t.Errorf("%s = %q, want unset", k, got)
		}
	}
	for _, kv := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"} {
		if !slices.Contains(env, kv) {
			t.Errorf("env lacks %s", kv)
		}
	}
}
