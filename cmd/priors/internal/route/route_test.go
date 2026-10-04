package route

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
)

func TestMain(m *testing.M) {
	for _, k := range RepoLocatingEnv {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

func TestParseURL(t *testing.T) {
	tests := []struct {
		raw               string
		host, owner, repo string
		sshLike, wantErr  bool
	}{
		{raw: "git@github.com:Owner/Repo.git", host: "github.com", owner: "owner", repo: "repo", sshLike: true},
		{raw: "ssh://git@github.com:22/o/r", host: "github.com", owner: "o", repo: "r", sshLike: true},
		{raw: "https://GitHub.com/o/r.git", host: "github.com", owner: "o", repo: "r"},
		{raw: "https://user@github.com:443/o/r/", host: "github.com", owner: "o", repo: "r"},
		{raw: "git@gh-work:o/r", host: "gh-work", owner: "o", repo: "r", sshLike: true},
		{raw: "gh-work:o/r.git", host: "gh-work", owner: "o", repo: "r", sshLike: true},
		{raw: "git://example.org/o/r.git", host: "example.org", owner: "o", repo: "r"},
		{raw: "git+ssh://git@github.com/o/r.git", host: "github.com", owner: "o", repo: "r", sshLike: true},
		{raw: "ssh+git://git@gh-work/o/r", host: "gh-work", owner: "o", repo: "r", sshLike: true},
		{raw: "https://gitlab.com/Group/Sub/Repo.git", host: "gitlab.com", owner: "group", repo: "repo"},
		{raw: "file:///x", wantErr: true},
		{raw: "/srv/git/o/r.git", wantErr: true},
		{raw: "../r", wantErr: true},
		{raw: "", wantErr: true},
		{raw: "https://github.com/only", wantErr: true},
		{raw: "git@github.com:only.git", wantErr: true},
		{raw: "git@-oProxyCommand=x:o/r", wantErr: true},
		{raw: "ssh://-oProxyCommand=x/o/r", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			host, owner, repo, sshLike, err := ParseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseURL(%q) = %q %q %q, want error", tt.raw, host, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tt.raw, err)
			}
			if host != tt.host || owner != tt.owner || repo != tt.repo || sshLike != tt.sshLike {
				t.Errorf("ParseURL(%q) = %q %q %q %v, want %q %q %q %v",
					tt.raw, host, owner, repo, sshLike, tt.host, tt.owner, tt.repo, tt.sshLike)
			}
		})
	}
}

// isolateGit keeps the developer's git config out of the test repos.
func isolateGit(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = t\n\temail = t@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepo makes a git repo in a directory named name, with the given remotes
// (name → URL).
func newRepo(t *testing.T, name string, remotes map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	for r, url := range remotes {
		git(t, dir, "remote", "add", r, url)
	}
	return realpath(t, dir)
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var stubResolver = Resolver{SSHHost: func(_ context.Context, alias string) string {
	switch alias {
	case "gh-work":
		return "github.com"
	case "github.com":
		return "ssh.github.com"
	}
	return alias
}}

func workCfg(profile string) config.Config {
	return config.Config{
		Profile:      profile,
		WorkOrgs:     []string{"github.com/factify-inc"},
		PersonalOrgs: []string{"github.com/noamsto"},
	}
}

func TestResolve(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		name    string
		profile string
		remotes map[string]string
		class   Class
		repo    string
	}{
		{"work https case-fold .git", "personal", map[string]string{"origin": "https://GitHub.com/FACTIFY-inc/X.git"}, ClassWork, "x"},
		{"work scp literal host kept when ssh -G rewrites it", "personal", map[string]string{"origin": "git@github.com:factify-inc/x.git"}, ClassWork, "x"},
		{"work via ssh alias", "personal", map[string]string{"origin": "git@gh-work:factify-inc/x"}, ClassWork, "x"},
		{"work via ssh:// alias", "personal", map[string]string{"origin": "ssh://git@gh-work/factify-inc/x"}, ClassWork, "x"},
		{"upstream work with personal origin", "personal", map[string]string{"origin": "git@github.com:noamsto/fork.git", "upstream": "https://github.com/factify-inc/x"}, ClassWork, "fork"},
		{"work remote without origin", "personal", map[string]string{"upstream": "https://github.com/factify-inc/x"}, ClassWork, "clone"},
		{"no origin", "personal", map[string]string{"upstream": "https://github.com/noamsto/x"}, ClassUnresolvable, "clone"},
		{"no remotes", "personal", nil, ClassUnresolvable, "clone"},
		{"origin unparsable", "personal", map[string]string{"origin": "/srv/git/x.git"}, ClassUnresolvable, "clone"},
		{"personal org on personal host", "personal", map[string]string{"origin": "git@github.com:NoamSto/Dots.git"}, ClassPersonal, "dots"},
		{"personal org on work host", "work", map[string]string{"origin": "https://github.com/noamsto/dots"}, ClassPersonal, "dots"},
		{"work owner under another host", "personal", map[string]string{"origin": "git@gitlab.com:factify-inc/x.git"}, ClassUnresolvable, "x"},
		{"work owner under unresolved alias", "personal", map[string]string{"origin": "git@gh-other:factify-inc/x"}, ClassUnresolvable, "x"},
		{"neither list on work host", "work", map[string]string{"origin": "https://github.com/someone/x"}, ClassUnresolvable, "x"},
		{"neither list on personal host", "personal", map[string]string{"origin": "https://github.com/someone/x"}, ClassPersonal, "x"},
		{"upstream work over git+ssh", "personal", map[string]string{"origin": "git@github.com:noamsto/fork.git", "upstream": "git+ssh://git@github.com/factify-inc/z.git"}, ClassWork, "fork"},
		{"upstream work owner under unresolved alias", "personal", map[string]string{"origin": "git@github.com:noamsto/fork.git", "upstream": "gh-other:factify-inc/z.git"}, ClassUnresolvable, "fork"},
		{"upstream work owner under another host", "personal", map[string]string{"origin": "https://github.com/noamsto/fork", "upstream": "https://gitlab.com/factify-inc/z"}, ClassUnresolvable, "fork"},
		{"upstream unparsable", "personal", map[string]string{"origin": "https://github.com/noamsto/fork", "upstream": "weird/relative/path"}, ClassUnresolvable, "fork"},
		{"upstream local absolute path", "personal", map[string]string{"origin": "https://github.com/noamsto/fork", "upstream": "/some/local/path"}, ClassPersonal, "fork"},
		{"upstream local relative and file paths", "personal", map[string]string{"origin": "https://github.com/noamsto/fork", "a": "./a", "b": "../b", "c": "file:///srv/c.git"}, ClassPersonal, "fork"},
		{"origin repo name normalised", "personal", map[string]string{"origin": "https://github.com/noamsto/My_Repo.js.git"}, ClassPersonal, "my-repo-js"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newRepo(t, "clone", tt.remotes)
			got := Resolve(t.Context(), dir, workCfg(tt.profile), stubResolver)
			want := Session{Class: tt.class, Repo: tt.repo, Dir: dir}
			if got != want {
				t.Errorf("Resolve = %+v, want %+v", got, want)
			}
		})
	}
}

func TestResolveRewrites(t *testing.T) {
	const (
		noamsto   = "https://github.com/noamsto/x"
		factify   = "https://github.com/factify-inc/x"
		someone   = "https://github.com/someone/x"
		insteadOf = "insteadOf"
		pushInst  = "pushInsteadOf"
	)
	type rewrite struct{ base, kind, prefix string }
	tests := []struct {
		name     string
		profile  string
		origin   string
		rewrites []rewrite
		setup    [][]string
		class    Class
		repo     string
	}{
		{"work raw hidden by insteadOf", "personal", "git@github.com:factify-inc/x", []rewrite{{"git@github.com:noamsto/", insteadOf, "git@github.com:factify-inc/"}}, nil, ClassWork, "x"},
		{"personal raw rewritten to work", "personal", noamsto, []rewrite{{"https://github.com/factify-inc/", insteadOf, "https://github.com/noamsto/"}}, nil, ClassWork, "x"},
		{"pushInsteadOf to work", "personal", noamsto, []rewrite{{"https://github.com/factify-inc/", pushInst, "https://github.com/noamsto/"}}, nil, ClassWork, "x"},
		{"second url value is work", "personal", noamsto, nil, [][]string{{"config", "--add", "remote.origin.url", factify}}, ClassWork, "x"},
		{"pushurl via ssh alias is work", "personal", noamsto, nil, [][]string{{"config", "remote.origin.pushurl", "git@gh-work:factify-inc/x"}}, ClassWork, "x"},
		{"work raw rewritten to local path", "personal", factify, []rewrite{{"/srv/mirror/", insteadOf, "https://github.com/factify-inc/"}}, nil, ClassWork, "clone"},
		{"plain personal", "personal", noamsto, nil, nil, ClassPersonal, "x"},
		{"personal raw rewritten to unlisted org on work host", "work", noamsto, []rewrite{{"https://github.com/someone/", insteadOf, "https://github.com/noamsto/"}}, nil, ClassUnresolvable, "x"},
		{"personal rewritten to personal on work host", "work", noamsto, []rewrite{{"git@github.com:noamsto/", insteadOf, "https://github.com/noamsto/"}}, nil, ClassPersonal, "x"},
		{"unlisted raw rewritten to personal on work host", "work", someone, []rewrite{{"https://github.com/noamsto/", insteadOf, "https://github.com/someone/"}}, nil, ClassUnresolvable, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateGit(t)
			dir := newRepo(t, "clone", map[string]string{"origin": tt.origin})
			for _, rw := range tt.rewrites {
				git(t, dir, "config", "--global", "url."+rw.base+"."+rw.kind, rw.prefix)
			}
			for _, args := range tt.setup {
				git(t, dir, args...)
			}
			got := Resolve(t.Context(), dir, workCfg(tt.profile), stubResolver)
			want := Session{Class: tt.class, Repo: tt.repo, Dir: dir}
			if got != want {
				t.Errorf("Resolve = %+v, want %+v", got, want)
			}
		})
	}
}

func TestResolveIgnoresInheritedRepoEnv(t *testing.T) {
	isolateGit(t)
	dir := newRepo(t, "clone", map[string]string{"origin": "https://github.com/noamsto/x"})
	outer := newRepo(t, "outer", map[string]string{"origin": "https://github.com/factify-inc/y"})
	t.Setenv("GIT_DIR", filepath.Join(outer, ".git"))
	t.Setenv("GIT_WORK_TREE", outer)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(outer, ".git", "index"))

	got := Resolve(t.Context(), dir, workCfg("personal"), stubResolver)
	if want := (Session{Class: ClassPersonal, Repo: "x", Dir: dir}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolveSubdirUsesToplevel(t *testing.T) {
	isolateGit(t)
	dir := newRepo(t, "clone", map[string]string{"origin": "https://github.com/noamsto/x"})
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	got := Resolve(t.Context(), sub, workCfg("personal"), stubResolver)
	if want := (Session{Class: ClassPersonal, Repo: "x", Dir: dir}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolveNoRepo(t *testing.T) {
	isolateGit(t)
	got := Resolve(t.Context(), t.TempDir(), workCfg("work"), stubResolver)
	if want := (Session{Class: ClassNoRepo}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolveBrokenRepoIsUnresolvable(t *testing.T) {
	isolateGit(t)
	dir := newRepo(t, "clone", map[string]string{"origin": "https://github.com/noamsto/x"})
	cfgPath := filepath.Join(dir, ".git", "config")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[core\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := Resolve(t.Context(), dir, workCfg("personal"), stubResolver)
	if got.Class != ClassUnresolvable {
		t.Errorf("Resolve = %+v, want class %q", got, ClassUnresolvable)
	}
}

func TestResolveWorktreeNamesMainRepo(t *testing.T) {
	isolateGit(t)
	main := newRepo(t, "Main_Repo", nil)
	git(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(t.TempDir(), "feature-wt")
	git(t, main, "worktree", "add", "-q", wt)
	wt = realpath(t, wt)

	got := Resolve(t.Context(), wt, workCfg("personal"), stubResolver)
	if want := (Session{Class: ClassUnresolvable, Repo: "main-repo", Dir: wt}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestRepoName(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"My_Repo.js", "my-repo-js"},
		{"Hookyard", "hookyard"},
		{"MainRepo", "mainrepo"},
		{"--a..b__", "a-b"},
		{"dots.", "dots"},
		{"___", "repo"},
		{"", "repo"},
		{strings.Repeat("a", 80) + "-b", strings.Repeat("a", 80)},
		{strings.Repeat("x", 100), strings.Repeat("x", 81)},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := repoName(tt.raw); got != tt.want {
				t.Errorf("repoName(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestResolveDefaultSSHHost(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}
	isolateGit(t)
	sshConfig := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(sshConfig, []byte("Host gh-alias\n  HostName github.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := workCfg("personal")
	cfg.SSHConfig = sshConfig
	dir := newRepo(t, "clone", map[string]string{"origin": "git@gh-alias:factify-inc/x"})

	got := Resolve(t.Context(), dir, cfg, Resolver{})
	if want := (Session{Class: ClassWork, Repo: "x", Dir: dir}); got != want {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func presentCfg(t *testing.T, profile string, present bool) config.Config {
	t.Helper()
	cfg := workCfg(profile)
	cfg.WorkStore = filepath.Join(t.TempDir(), "missing")
	if present {
		cfg.WorkStore = t.TempDir()
	}
	return cfg
}

func TestReadStores(t *testing.T) {
	both := []StoreID{StoreWork, StorePersonal}
	personal := []StoreID{StorePersonal}
	tests := []struct {
		name    string
		class   Class
		profile string
		present bool
		want    []StoreID
	}{
		{"work org, work present", ClassWork, "work", true, both},
		{"work org, work clone missing", ClassWork, "work", false, personal},
		{"work org, personal host", ClassWork, "personal", false, personal},
		{"personal on work host", ClassPersonal, "work", true, personal},
		{"no repo on work host", ClassNoRepo, "work", true, personal},
		{"unresolvable on work host", ClassUnresolvable, "work", true, personal},
		{"personal on personal host", ClassPersonal, "personal", false, personal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReadStores(Session{Class: tt.class}, presentCfg(t, tt.profile, tt.present))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ReadStores = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteDest(t *testing.T) {
	tests := []struct {
		name       string
		class      Class
		profile    string
		present    bool
		store      StoreID
		quarantine bool
	}{
		{"personal org, personal host", ClassPersonal, "personal", false, StorePersonal, false},
		{"personal org, work host", ClassPersonal, "work", true, StorePersonal, false},
		{"work org, work host", ClassWork, "work", true, StoreWork, false},
		{"work org, work host, clone missing", ClassWork, "work", false, "", true},
		{"work org, personal host", ClassWork, "personal", false, "", true},
		{"no repo, work host", ClassNoRepo, "work", true, StoreWork, false},
		{"no repo, work host, clone missing", ClassNoRepo, "work", false, "", true},
		{"no repo, personal host", ClassNoRepo, "personal", false, StorePersonal, false},
		{"unresolvable, work host", ClassUnresolvable, "work", true, StoreWork, false},
		{"unresolvable, work host, clone missing", ClassUnresolvable, "work", false, "", true},
		{"unresolvable, personal host", ClassUnresolvable, "personal", false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WriteDest(Session{Class: tt.class}, presentCfg(t, tt.profile, tt.present))
			if got.Store != tt.store || got.Quarantine != tt.quarantine {
				t.Errorf("WriteDest = %+v, want store %q quarantine %v", got, tt.store, tt.quarantine)
			}
			if got.Quarantine && got.Why == "" {
				t.Errorf("WriteDest = %+v: quarantine without a reason", got)
			}
		})
	}
}
