package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeRoot is the real file system with every owner reported as uid 0, so
// load runs the trust walk over temp files without root.
type fakeRoot struct{ osFS }

func (r fakeRoot) Lstat(name string) (fs.FileInfo, error) {
	fi, err := r.osFS.Lstat(name)
	if err != nil {
		return nil, err
	}
	owned := *fi.Sys().(*syscall.Stat_t)
	owned.Uid = 0
	return fakeRootInfo{fi, &owned}, nil
}

type fakeRootInfo struct {
	fs.FileInfo
	sys *syscall.Stat_t
}

func (i fakeRootInfo) Sys() any { return i.sys }

const sampleTrust = `
profile       = "work"
work_orgs     = ["github.com/Factify-Inc"]
personal_orgs = ["GitHub.com/noamsto"]
trust_root    = "separate"

[stores.personal]
id = "personal-test"

[stores.work]
id = "work-test"
`

func writeTrust(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	// Under umask 002 a temp dir is group-writable, which the trust walk refuses.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trust.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadUser loads the user config at path against a valid trust file.
func loadUser(t *testing.T, path string) (Config, error) {
	t.Helper()
	return load(path, writeTrust(t, sampleTrust), fakeRoot{})
}

func isolate(t *testing.T) string {
	t.Helper()
	home := realPath(t, t.TempDir())
	t.Setenv("HOME", home)
	for _, k := range []string{"PRIORS_CONFIG", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "HOOKYARD_STATE_DIR"} {
		t.Setenv(k, "")
	}
	return home
}

const sample = `
personal_store = "~/memory/personal"
work_store     = "~/memory/work"
work_names     = ["corp-host.internal"]
state_dir      = "~/state"
event_record   = "/abs/record"
rules          = "~/rules.toml"
ssh_config     = "~/ssh_config"
commit         = false
push           = true
`

func TestLoadFullSample(t *testing.T) {
	home := isolate(t)

	got, err := loadUser(t, writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}

	no := false
	want := Config{
		Profile:         "work",
		WorkOrgs:        []string{"github.com/factify-inc"},
		PersonalOrgs:    []string{"github.com/noamsto"},
		PersonalStoreID: "personal-test",
		WorkStoreID:     "work-test",
		TrustRoot:       "separate",
		PersonalStore:   filepath.Join(home, "memory/personal"),
		WorkStore:       filepath.Join(home, "memory/work"),
		workNames:       []string{"corp-host.internal"},
		StateDir:        filepath.Join(home, "state"),
		EventRecord:     "/abs/record",
		Rules:           filepath.Join(home, "rules.toml"),
		SSHConfig:       filepath.Join(home, "ssh_config"),
		Commit:          &no,
		Push:            true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %+v\nwant   %+v", got, want)
	}
}

func TestLoadWorkNamesFloor(t *testing.T) {
	isolate(t)
	const (
		floor = "work_names    = [\"acme\"]\n\n[stores.personal]"
		base  = "personal_store = \"/m/personal\"\n"
	)
	withFloor := strings.Replace(sampleTrust, "[stores.personal]", floor, 1)
	cases := []struct {
		name        string
		trust, user string
		want        []string
	}{
		{"floor, user omitted", withFloor, base, []string{"acme"}},
		{"floor, user empty", withFloor, base + "work_names = []\n", []string{"acme"}},
		{"floor plus user addition", withFloor, base + "work_names = [\"globex\"]\n", []string{"acme", "globex"}},
		{"floor, user repeats and duplicates", withFloor, base + "work_names = [\"acme\", \"globex\", \"globex\"]\n", []string{"acme", "globex"}},
		{"no floor, user addition", sampleTrust, base + "work_names = [\"globex\"]\n", []string{"globex"}},
		{"neither", sampleTrust, base, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := load(writeConfig(t, c.user), writeTrust(t, c.trust), fakeRoot{})
			if err != nil {
				t.Fatal(err)
			}
			if names := got.WorkNames(); len(names) != len(c.want) || (len(c.want) > 0 && !reflect.DeepEqual(names, c.want)) {
				t.Errorf("WorkNames() = %v, want %v", names, c.want)
			}
		})
	}

	t.Run("returns a copy", func(t *testing.T) {
		got, err := load(writeConfig(t, base), writeTrust(t, withFloor), fakeRoot{})
		if err != nil {
			t.Fatal(err)
		}
		got.WorkNames()[0] = "mutated"
		if names := got.WorkNames(); !reflect.DeepEqual(names, []string{"acme"}) {
			t.Errorf("WorkNames() = %v after the caller mutated a previous result, want [acme]", names)
		}
	})
}

func TestLoadTrustRoot(t *testing.T) {
	isolate(t)
	for name, c := range map[string]struct{ line, want string }{
		"separate":   {`trust_root = "separate"`, "separate"},
		"bool":       {"trust_root = true", ""},
		"int":        {"trust_root = 1", ""},
		"other text": {`trust_root = "owner-admin"`, "owner-admin"},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(sampleTrust, `trust_root    = "separate"`, c.line, 1)
			got, err := load(writeConfig(t, sample), writeTrust(t, body), fakeRoot{})
			if err != nil {
				t.Fatal(err)
			}
			if got.TrustRoot != c.want {
				t.Errorf("TrustRoot = %q, want %q", got.TrustRoot, c.want)
			}
		})
	}
}

func TestLoadRejects(t *testing.T) {
	isolate(t)
	cases := map[string]string{
		"unknown key":  "push = true\nbogus = 1\n",
		"invalid toml": "push = \n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadUser(t, writeConfig(t, body)); err == nil {
				t.Errorf("Load accepted %q", body)
			}
		})
	}
}

func TestLoadValidatesStores(t *testing.T) {
	home := isolate(t)
	accepted := map[string]string{
		"minimal":                 "personal_store = \"/m/personal\"\n",
		"tilde paths":             "personal_store = \"~/p\"\nwork_store = \"~/w\"\nstate_dir = \"~/s\"\n",
		"siblings share a prefix": "personal_store = \"/m/store\"\nwork_store = \"/m/store-work\"\nstate_dir = \"/m/store-state\"\n",
	}
	for name, body := range accepted {
		t.Run(name, func(t *testing.T) {
			if _, err := loadUser(t, writeConfig(t, body)); err != nil {
				t.Errorf("Load(%q): %v", body, err)
			}
		})
	}
	rejected := map[string]struct{ body, want string }{
		"no personal store":               {"", `personal_store must be an absolute path, got ""`},
		"relative personal store":         {"personal_store = \"memory/p\"\n", `personal_store must be an absolute path, got "memory/p"`},
		"relative work store":             {"personal_store = \"/m/p\"\nwork_store = \"memory/w\"\n", `work_store must be an absolute path, got "memory/w"`},
		"relative state dir":              {"personal_store = \"/m/p\"\nstate_dir = \"state\"\n", `state_dir must be an absolute path, got "state"`},
		"stores equal":                    {"personal_store = \"/m/s\"\nwork_store = \"/m/s/\"\n", "personal_store (/m/s) and work_store (/m/s) must not be the same or nested"},
		"work inside personal":            {"personal_store = \"/m/p\"\nwork_store = \"/m/p/w\"\n", "personal_store (/m/p) and work_store (/m/p/w) must not"},
		"personal inside work":            {"personal_store = \"/m/w/p\"\nwork_store = \"/m/w\"\n", "personal_store (/m/w/p) and work_store (/m/w) must not"},
		"state inside personal":           {"personal_store = \"/m/p\"\nstate_dir = \"/m/p/.state\"\n", "personal_store (/m/p) and state_dir (/m/p/.state) must not"},
		"personal inside state":           {"personal_store = \"/m/s/p\"\nstate_dir = \"/m/s\"\n", "personal_store (/m/s/p) and state_dir (/m/s) must not"},
		"work inside state":               {"personal_store = \"/m/p\"\nwork_store = \"/m/s/w\"\nstate_dir = \"/m/s\"\n", "work_store (/m/s/w) and state_dir (/m/s) must not"},
		"default state inside home store": {"personal_store = \"" + home + "\"\n", "personal_store (" + home + ") and the default state dir"},
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			_, err := loadUser(t, writeConfig(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Load(%q) = %v, want an error containing %q", c.body, err, c.want)
			}
		})
	}
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLoadResolvesSymlinkedStores(t *testing.T) {
	isolate(t)
	personal, work, state := t.TempDir(), t.TempDir(), t.TempDir()
	links := t.TempDir()
	body := fmt.Sprintf("personal_store = %q\nwork_store = %q\nstate_dir = %q\n",
		symlink(t, personal, filepath.Join(links, "p")),
		filepath.Join(symlink(t, work, filepath.Join(links, "w")), "not-yet"),
		symlink(t, state, filepath.Join(links, "s")))

	c, err := loadUser(t, writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}

	got := []string{c.PersonalStore, c.WorkStore, c.StateDir, c.State()}
	want := []string{realPath(t, personal), filepath.Join(realPath(t, work), "not-yet"), realPath(t, state), realPath(t, state)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}

func TestLoadResolvesTheDefaultStateDir(t *testing.T) {
	isolate(t)
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", symlink(t, xdg, filepath.Join(t.TempDir(), "xdg")))
	body := fmt.Sprintf("personal_store = %q\n", t.TempDir())

	c, err := loadUser(t, writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(realPath(t, xdg), "priors"); c.State() != want {
		t.Errorf("State() = %q, want %q", c.State(), want)
	}
}

func TestLoadRejectsNestingThroughASymlink(t *testing.T) {
	isolate(t)
	personal := t.TempDir()
	if err := os.Mkdir(filepath.Join(personal, "w"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := symlink(t, filepath.Join(personal, "w"), filepath.Join(t.TempDir(), "work"))
	body := fmt.Sprintf("personal_store = %q\nwork_store = %q\nstate_dir = %q\n",
		personal, work, t.TempDir())

	_, err := loadUser(t, writeConfig(t, body))

	if err == nil || !strings.Contains(err.Error(), "must not be the same or nested") {
		t.Errorf("err = %v, want a nesting error", err)
	}
}

func TestLoadRejectsADanglingSymlinkStore(t *testing.T) {
	isolate(t)
	dangling := symlink(t, filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "p"))
	body := fmt.Sprintf("personal_store = %q\nstate_dir = %q\n", dangling, t.TempDir())

	_, err := loadUser(t, writeConfig(t, body))

	if err == nil || !strings.Contains(err.Error(), "personal_store") {
		t.Errorf("err = %v, want an error naming personal_store", err)
	}
}

func TestLoadRejectsAStateDirInAGitWorkTree(t *testing.T) {
	mkdir := func(t *testing.T, p string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	repo := func(t *testing.T) string {
		t.Helper()
		root := realPath(t, t.TempDir())
		mkdir(t, filepath.Join(root, ".git"))
		return root
	}
	cases := []struct {
		name string
		// setup returns state_dir ("" for the default), the work tree root the
		// error must name, and the state path it must name.
		setup func(t *testing.T) (stateDir, root, state string)
		key   string
	}{
		{"repo root", func(t *testing.T) (string, string, string) {
			r := repo(t)
			return r, r, r
		}, "state_dir"},
		{"repo subdir not yet created", func(t *testing.T) (string, string, string) {
			r := repo(t)
			s := filepath.Join(r, "a", "b")
			return s, r, s
		}, "state_dir"},
		{"symlink into a repo", func(t *testing.T) (string, string, string) {
			r := repo(t)
			sub := mkdir(t, filepath.Join(r, "sub"))
			return symlink(t, sub, filepath.Join(t.TempDir(), "link")), r, sub
		}, "state_dir"},
		{"linked worktree gitfile", func(t *testing.T) (string, string, string) {
			r := realPath(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(r, ".git"), []byte("gitdir: /x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			s := filepath.Join(r, "state")
			return s, r, s
		}, "state_dir"},
		{"git entry is a symlink", func(t *testing.T) (string, string, string) {
			r := realPath(t, t.TempDir())
			symlink(t, t.TempDir(), filepath.Join(r, ".git"))
			s := filepath.Join(r, "state")
			return s, r, s
		}, "state_dir"},
		{"default state dir", func(t *testing.T) (string, string, string) {
			r := repo(t)
			t.Setenv("XDG_STATE_HOME", r)
			return "", r, filepath.Join(r, "priors")
		}, "the default state dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stateDir, root, state := tc.setup(t)
			body := fmt.Sprintf("personal_store = %q\n", t.TempDir())
			if stateDir != "" {
				body += fmt.Sprintf("state_dir = %q\n", stateDir)
			}

			_, err := loadUser(t, writeConfig(t, body))

			if err == nil {
				t.Fatal("Load accepted a state dir inside a git work tree")
			}
			for _, want := range []string{tc.key, state, "inside the git work tree at " + root} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadRejectsARelativeDefaultStateDir(t *testing.T) {
	isolate(t)
	t.Setenv("XDG_STATE_HOME", "rel")
	body := fmt.Sprintf("personal_store = %q\n", t.TempDir())

	_, err := loadUser(t, writeConfig(t, body))

	if err == nil || !strings.Contains(err.Error(), "the default state dir must be an absolute path") {
		t.Errorf("err = %v, want an absolute-path error for the default state dir", err)
	}
}

func TestLoadAcceptsAStateDirOutsideAnyRepo(t *testing.T) {
	isolate(t)
	parent := realPath(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(parent, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(parent, "repo-state")
	body := fmt.Sprintf("personal_store = %q\nstate_dir = %q\n", t.TempDir(), state)

	c, err := loadUser(t, writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}

	if c.State() != state {
		t.Errorf("State() = %q, want %q", c.State(), state)
	}
}

func TestResolveDir(t *testing.T) {
	real := t.TempDir()
	links := t.TempDir()
	link := symlink(t, real, filepath.Join(links, "link"))
	dangling := symlink(t, filepath.Join(real, "absent"), filepath.Join(links, "dangling"))
	resolved := realPath(t, real)

	for _, c := range []struct{ in, want string }{
		{real, resolved},
		{link, resolved},
		{link + "/", resolved},
		{filepath.Join(link, "a", "b"), filepath.Join(resolved, "a", "b")},
		{"/no-such-root-dir/x", "/no-such-root-dir/x"},
	} {
		got, err := ResolveDir(c.in)
		if err != nil || got != c.want {
			t.Errorf("ResolveDir(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{dangling, filepath.Join(dangling, "sub")} {
		if got, err := ResolveDir(in); err == nil {
			t.Errorf("ResolveDir(%q) = %q, want an error for a dangling symlink", in, got)
		}
	}
}

func TestLoadNamesTheUnknownKey(t *testing.T) {
	isolate(t)
	_, err := loadUser(t, writeConfig(t, "work_path = \"x\"\n"))
	if err == nil || !strings.Contains(err.Error(), "work_path") {
		t.Errorf("err = %v, want it to name work_path", err)
	}
}

func TestLoadRejectsTheScannerKey(t *testing.T) {
	isolate(t)
	_, err := loadUser(t, writeConfig(t, "scanner = \"x\"\n"))
	if err == nil || !strings.Contains(err.Error(), "pinned at build time") {
		t.Errorf("err = %v, want it to say the scanner is pinned at build time", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	isolate(t)
	if _, err := loadUser(t, filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

func TestLoadRefusesTrustKeysInTheUserConfig(t *testing.T) {
	isolate(t)
	for key, body := range map[string]string{
		"profile":       "profile = \"personal\"\n",
		"work_orgs":     "work_orgs = [\"github.com/noamsto\"]\n",
		"personal_orgs": "personal_orgs = [\"github.com/factify-inc\"]\n",
		"trust_root":    "trust_root = \"separate\"\n",
		"stores":        "[stores.personal]\nid = \"mine\"\n",
	} {
		t.Run(key, func(t *testing.T) {
			trust := writeTrust(t, sampleTrust)
			_, err := load(writeConfig(t, "personal_store = \"/m/p\"\n"+body), trust, fakeRoot{})
			want := key + " is read from the trust file " + trust + ", not the user config"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
		})
	}
}

func TestPriorsConfigCannotOverrideTheTrustFile(t *testing.T) {
	isolate(t)
	t.Setenv("PRIORS_CONFIG", writeConfig(t, "personal_store = \"/m/p\"\nprofile = \"personal\"\nwork_orgs = [\"github.com/noamsto\"]\n"))

	c, err := loadUser(t, DefaultPath())

	if err == nil || !strings.Contains(err.Error(), "is read from the trust file") {
		t.Errorf("Load = %+v, %v; want a trust-key error", c, err)
	}
}

func TestLoadNamesTheTrustFile(t *testing.T) {
	isolate(t)
	user := writeConfig(t, "personal_store = \"/m/p\"\n")
	missing := filepath.Join(t.TempDir(), "trust.toml")

	_, err := load(user, missing, fakeRoot{})

	if err == nil || !strings.HasPrefix(err.Error(), "trust file "+missing+": ") || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want a not-exist error naming %s", err, missing)
	}

	bad := writeTrust(t, "seam_probe = 1\n"+sampleTrust)
	_, err = load(user, bad, fakeRoot{})
	if err == nil || !strings.Contains(err.Error(), "trust file "+bad+": unknown keys: seam_probe") {
		t.Errorf("err = %v, want an unknown-key error naming %s", err, bad)
	}
}

func TestDefaultPath(t *testing.T) {
	home := isolate(t)
	if got, want := DefaultPath(), filepath.Join(home, ".config/priors/config.toml"); got != want {
		t.Errorf("home fallback = %q, want %q", got, want)
	}

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := DefaultPath(), filepath.Join(xdg, "priors/config.toml"); got != want {
		t.Errorf("XDG_CONFIG_HOME = %q, want %q", got, want)
	}

	t.Setenv("PRIORS_CONFIG", "/explicit.toml")
	if got := DefaultPath(); got != "/explicit.toml" {
		t.Errorf("PRIORS_CONFIG = %q", got)
	}
}

func TestState(t *testing.T) {
	home := isolate(t)
	if got, want := (Config{}).State(), filepath.Join(home, ".local/state/priors"); got != want {
		t.Errorf("home fallback = %q, want %q", got, want)
	}

	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	if got, want := (Config{}).State(), filepath.Join(xdg, "priors"); got != want {
		t.Errorf("XDG_STATE_HOME = %q, want %q", got, want)
	}

	if got := (Config{StateDir: "/explicit"}).State(); got != "/explicit" {
		t.Errorf("StateDir = %q", got)
	}
}

func TestProvenanceDir(t *testing.T) {
	isolate(t)
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	if got, want := (Config{}).ProvenanceDir(), filepath.Join(xdg, "priors", "provenance"); got != want {
		t.Errorf("XDG_STATE_HOME = %q, want %q", got, want)
	}

	if got, want := (Config{StateDir: "/explicit"}).ProvenanceDir(), "/explicit/provenance"; got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
}

func TestStatePathsDeriveFromTheResolvedState(t *testing.T) {
	isolate(t)
	real := realPath(t, t.TempDir())
	body := fmt.Sprintf("personal_store = %q\nstate_dir = %q\n",
		t.TempDir(), symlink(t, real, filepath.Join(t.TempDir(), "link")))

	c, err := loadUser(t, writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}

	got := []string{c.QuarantineDir(), c.LocalDir("work"), c.LockDir(), c.ProvenanceDir()}
	want := []string{
		filepath.Join(real, "quarantine"),
		filepath.Join(real, "local", "work"),
		filepath.Join(real, "locks"),
		filepath.Join(real, "provenance"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("state paths = %q, want %q", got, want)
	}
}

func TestRecordDirChain(t *testing.T) {
	home := isolate(t)
	if got, want := (Config{}).RecordDir(), filepath.Join(home, ".local/state/hookyard"); got != want {
		t.Errorf("home fallback = %q, want %q", got, want)
	}

	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	if got, want := (Config{}).RecordDir(), filepath.Join(xdg, "hookyard"); got != want {
		t.Errorf("XDG_STATE_HOME = %q, want %q", got, want)
	}

	t.Setenv("HOOKYARD_STATE_DIR", "/hy")
	if got := (Config{}).RecordDir(); got != "/hy" {
		t.Errorf("HOOKYARD_STATE_DIR = %q", got)
	}

	if got := (Config{EventRecord: "/rec"}).RecordDir(); got != "/rec" {
		t.Errorf("EventRecord = %q", got)
	}
}

func TestWorkPresent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"work with clone", Config{Profile: "work", WorkStore: dir}, true},
		{"personal profile", Config{Profile: "personal", WorkStore: dir}, false},
		{"missing dir", Config{Profile: "work", WorkStore: filepath.Join(dir, "absent")}, false},
		{"unset store", Config{Profile: "work"}, false},
		{"store is a file", Config{Profile: "work", WorkStore: file}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.WorkPresent(); got != c.want {
				t.Errorf("WorkPresent = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCommitEnabled(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		commit *bool
		want   bool
	}{{nil, true}, {&yes, true}, {&no, false}} {
		if got := (Config{Commit: c.commit}).CommitEnabled(); got != c.want {
			t.Errorf("CommitEnabled(%v) = %v, want %v", c.commit, got, c.want)
		}
	}
}
