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

	"golang.org/x/crypto/ssh"
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

// trustBody is a trust file whose stores sit at personal and work: a work
// profile when work is set, else a personal one. An empty personal leaves the
// path out.
func trustBody(personal, work string) string {
	profile := "personal"
	if work != "" {
		profile = "work"
	}
	body := fmt.Sprintf(`
profile       = %q
work_orgs     = ["github.com/Factify-Inc"]
personal_orgs = ["GitHub.com/noamsto"]
trust_root    = "separate"

[stores.personal]
id = "personal-test"
`, profile)
	if personal != "" {
		body += fmt.Sprintf("path = %q\n", personal)
	}
	if work != "" {
		body += fmt.Sprintf("\n[stores.work]\nid = \"work-test\"\npath = %q\n", work)
	}
	return body
}

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

// trustWith writes a trust file whose stores sit at personal and work.
func trustWith(t *testing.T, personal, work string) string {
	t.Helper()
	return writeTrust(t, trustBody(personal, work))
}

// loadStores loads the user config at path against a trust file whose stores
// sit at personal and work.
func loadStores(t *testing.T, path, personal, work string) (Config, error) {
	t.Helper()
	return load(path, trustWith(t, personal, work), fakeRoot{})
}

// loadUser loads the user config at path against a valid trust file.
func loadUser(t *testing.T, path string) (Config, error) {
	t.Helper()
	return loadStores(t, path, "/m/personal", "/m/work")
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

	remotes := strings.NewReplacer(
		`id = "personal-test"`, "id = \"personal-test\"\nremote = \"git@example.com:p.git\"",
		`id = "work-test"`, "id = \"work-test\"\nremote = \"git@example.com:w.git\"",
	)
	body := remotes.Replace(trustBody(filepath.Join(home, "memory/personal"), filepath.Join(home, "memory/work")))
	trust := writeTrust(t, body)

	got, err := load(writeConfig(t, sample), trust, fakeRoot{})
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
		PersonalRemote:  "git@example.com:p.git",
		WorkRemote:      "git@example.com:w.git",
		workNames:       []string{"corp-host.internal"},
		StateDir:        filepath.Join(home, "state"),
		EventRecord:     "/abs/record",
		Rules:           filepath.Join(home, "rules.toml"),
		SSHConfig:       filepath.Join(home, "ssh_config"),
		Commit:          &no,
		Push:            true,
		TrustDigest:     sha256Hex(body),
		TrustRootLabel:  "separate",
		fsys:            fakeRoot{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %+v\nwant   %+v", got, want)
	}
}

func TestLoadWorkNamesFloor(t *testing.T) {
	isolate(t)
	const (
		floor = "work_names    = [\"acme\"]\n\n[stores.personal]"
		base  = ""
	)
	noFloor := trustBody("/m/personal", "/m/work")
	withFloor := strings.Replace(noFloor, "[stores.personal]", floor, 1)
	cases := []struct {
		name        string
		trust, user string
		want        []string
	}{
		{"floor, user omitted", withFloor, base, []string{"acme"}},
		{"floor, user empty", withFloor, base + "work_names = []\n", []string{"acme"}},
		{"floor plus user addition", withFloor, base + "work_names = [\"globex\"]\n", []string{"acme", "globex"}},
		{"floor, user repeats and duplicates", withFloor, base + "work_names = [\"acme\", \"globex\", \"globex\"]\n", []string{"acme", "globex"}},
		{"no floor, user addition", noFloor, base + "work_names = [\"globex\"]\n", []string{"globex"}},
		{"neither", noFloor, base, nil},
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

func TestFSIsTheTrustFileSource(t *testing.T) {
	isolate(t)
	c, err := loadUser(t, writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.FS().(fakeRoot); !ok {
		t.Errorf("FS() = %T, want the fakeRoot load read through", c.FS())
	}
	if _, ok := (Config{}).FS().(osFS); !ok {
		t.Errorf("zero Config FS() = %T, want osFS", (Config{}).FS())
	}
}

func TestLoadCopiesAttestKeys(t *testing.T) {
	isolate(t)
	sk, key := skKeyLine(t)

	got, err := load(writeConfig(t, sample), writeTrust(t, trustBody("/m/personal", "/m/work")+attestEntry(sk, "YQ==", "Yg==")), fakeRoot{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.AttestKeys) != 1 {
		t.Fatalf("AttestKeys = %+v, want one", got.AttestKeys)
	}
	k := got.AttestKeys[0]
	if !reflect.DeepEqual(k.Key.Marshal(), key.Marshal()) || string(k.Attestation) != "a" || string(k.Challenge) != "b" {
		t.Errorf("AttestKeys[0] = %s %q %q, want %s \"a\" \"b\"", ssh.MarshalAuthorizedKey(k.Key), k.Attestation, k.Challenge, sk)
	}
}

func TestLoadTrustRoot(t *testing.T) {
	isolate(t)
	for name, c := range map[string]struct{ line, want, label string }{
		"separate":   {`trust_root = "separate"`, "separate", "separate"},
		"absent":     {"", "", "absent"},
		"bool":       {"trust_root = true", "", "true"},
		"int":        {"trust_root = 1", "", "1"},
		"array":      {`trust_root = ["a"]`, "", `["a"]`},
		"table":      {`trust_root = {a = 1}`, "", "a table"},
		"tables":     {"[[trust_root]]\na = 1", "", "an array of tables"},
		"other text": {`trust_root = "owner-admin"`, "owner-admin", "owner-admin"},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(trustBody("/m/personal", "/m/work"), `trust_root    = "separate"`, c.line, 1)
			got, err := load(writeConfig(t, sample), writeTrust(t, body), fakeRoot{})
			if err != nil {
				t.Fatal(err)
			}
			if got.TrustRoot != c.want || got.TrustRootLabel != c.label {
				t.Errorf("TrustRoot, TrustRootLabel = %q, %q; want %q, %q", got.TrustRoot, got.TrustRootLabel, c.want, c.label)
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
	accepted := map[string]struct{ user, personal, work string }{
		"minimal":                 {"", "/m/personal", ""},
		"tilde state dir":         {"state_dir = \"~/s\"\n", "/m/p", "/m/w"},
		"siblings share a prefix": {"state_dir = \"/m/store-state\"\n", "/m/store", "/m/store-work"},
	}
	for name, c := range accepted {
		t.Run(name, func(t *testing.T) {
			if _, err := loadStores(t, writeConfig(t, c.user), c.personal, c.work); err != nil {
				t.Errorf("Load(%q, %q, %q): %v", c.user, c.personal, c.work, err)
			}
		})
	}
	rejected := map[string]struct{ user, personal, work, want string }{
		"no personal store":               {"", "", "", "stores.personal.path is required"},
		"tilde personal store":            {"", "~/p", "", `stores.personal.path "~/p" must be absolute`},
		"relative personal store":         {"", "memory/p", "", `stores.personal.path "memory/p" must be absolute`},
		"relative work store":             {"", "/m/p", "memory/w", `stores.work.path "memory/w" must be absolute`},
		"relative state dir":              {"state_dir = \"state\"\n", "/m/p", "", `state_dir must be an absolute path, got "state"`},
		"stores equal":                    {"", "/m/s", "/m/s/", "stores.personal.path (/m/s) and stores.work.path (/m/s) must not be the same or nested"},
		"work inside personal":            {"", "/m/p", "/m/p/w", "stores.personal.path (/m/p) and stores.work.path (/m/p/w) must not"},
		"personal inside work":            {"", "/m/w/p", "/m/w", "stores.personal.path (/m/w/p) and stores.work.path (/m/w) must not"},
		"state inside personal":           {"state_dir = \"/m/p/.state\"\n", "/m/p", "", "stores.personal.path (/m/p) and state_dir (/m/p/.state) must not"},
		"personal inside state":           {"state_dir = \"/m/s\"\n", "/m/s/p", "", "stores.personal.path (/m/s/p) and state_dir (/m/s) must not"},
		"work inside state":               {"state_dir = \"/m/s\"\n", "/m/p", "/m/s/w", "stores.work.path (/m/s/w) and state_dir (/m/s) must not"},
		"default state inside home store": {"", home, "", "stores.personal.path (" + home + ") and the default state dir"},
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			_, err := loadStores(t, writeConfig(t, c.user), c.personal, c.work)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Load(%q, %q, %q) = %v, want an error containing %q", c.user, c.personal, c.work, err, c.want)
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
	body := fmt.Sprintf("state_dir = %q\n", symlink(t, state, filepath.Join(links, "s")))

	c, err := loadStores(t, writeConfig(t, body),
		symlink(t, personal, filepath.Join(links, "p")),
		filepath.Join(symlink(t, work, filepath.Join(links, "w")), "not-yet"))
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

	c, err := loadStores(t, writeConfig(t, ""), t.TempDir(), "")
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
	body := fmt.Sprintf("state_dir = %q\n", t.TempDir())

	_, err := loadStores(t, writeConfig(t, body), personal, work)

	if err == nil || !strings.Contains(err.Error(), "must not be the same or nested") {
		t.Errorf("err = %v, want a nesting error", err)
	}
}

func TestLoadRejectsADanglingSymlinkStore(t *testing.T) {
	isolate(t)
	dangling := symlink(t, filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "p"))
	trust := trustWith(t, dangling, "")

	_, err := load(writeConfig(t, ""), trust, fakeRoot{})

	if err == nil || !strings.HasPrefix(err.Error(), trust+": stores.personal.path: ") {
		t.Errorf("err = %v, want a stores.personal.path error prefixed with %s", err, trust)
	}
}

func TestLoadPrefixesAStateDirErrorWithTheUserConfig(t *testing.T) {
	isolate(t)
	dangling := symlink(t, filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "s"))
	user := writeConfig(t, fmt.Sprintf("state_dir = %q\n", dangling))

	_, err := loadUser(t, user)

	if err == nil || !strings.HasPrefix(err.Error(), user+": state_dir: ") {
		t.Errorf("err = %v, want a state_dir error prefixed with %s", err, user)
	}
}

func TestLoadPrefixesAStoreNestingErrorWithTheTrustFile(t *testing.T) {
	isolate(t)
	trust := trustWith(t, "/m/p", "/m/p/w")

	_, err := load(writeConfig(t, ""), trust, fakeRoot{})

	if err == nil || !strings.HasPrefix(err.Error(), trust+": stores.personal.path (/m/p)") {
		t.Errorf("err = %v, want a nesting error prefixed with %s", err, trust)
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
			var body string
			if stateDir != "" {
				body = fmt.Sprintf("state_dir = %q\n", stateDir)
			}

			_, err := loadStores(t, writeConfig(t, body), t.TempDir(), "")

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

	_, err := loadStores(t, writeConfig(t, ""), t.TempDir(), "")

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
	body := fmt.Sprintf("state_dir = %q\n", state)

	c, err := loadStores(t, writeConfig(t, body), t.TempDir(), "")
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
		"profile":        "profile = \"personal\"\n",
		"work_orgs":      "work_orgs = [\"github.com/noamsto\"]\n",
		"personal_orgs":  "personal_orgs = [\"github.com/factify-inc\"]\n",
		"trust_root":     "trust_root = \"separate\"\n",
		"stores":         "[stores.personal]\nid = \"mine\"\n",
		"personal_store": "personal_store = \"/m/p\"\n",
		"work_store":     "work_store = \"/m/w\"\n",
		"attest_keys":    "[[attest_keys]]\nkey = \"x\"\n",
	} {
		t.Run(key, func(t *testing.T) {
			trust := trustWith(t, "/m/personal", "/m/work")
			_, err := load(writeConfig(t, body), trust, fakeRoot{})
			want := key + " is read from the trust file " + trust + ", not the user config"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
		})
	}
}

func TestPriorsConfigCannotOverrideTheTrustFile(t *testing.T) {
	isolate(t)
	t.Setenv("PRIORS_CONFIG", writeConfig(t, "profile = \"personal\"\nwork_orgs = [\"github.com/noamsto\"]\n"))

	c, err := loadUser(t, DefaultPath())

	if err == nil || !strings.Contains(err.Error(), "is read from the trust file") {
		t.Errorf("Load = %+v, %v; want a trust-key error", c, err)
	}
}

func TestLoadNamesTheTrustFile(t *testing.T) {
	isolate(t)
	user := writeConfig(t, "")
	missing := filepath.Join(t.TempDir(), "trust.toml")

	_, err := load(user, missing, fakeRoot{})

	if err == nil || !strings.HasPrefix(err.Error(), "trust file "+missing+": ") || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want a not-exist error naming %s", err, missing)
	}

	bad := writeTrust(t, "seam_probe = 1\n"+trustBody("/m/personal", "/m/work"))
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
	body := fmt.Sprintf("state_dir = %q\n", symlink(t, real, filepath.Join(t.TempDir(), "link")))

	c, err := loadStores(t, writeConfig(t, body), t.TempDir(), "")
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
