package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
profile        = "work"
personal_store = "~/memory/personal"
work_store     = "~/memory/work"
work_orgs      = ["github.com/Factify-Inc"]
personal_orgs  = ["GitHub.com/noamsto"]
work_names     = ["corp-host.internal"]
state_dir      = "~/state"
event_record   = "/abs/record"
rules          = "~/rules.toml"
scanner        = "gitleaks"
ssh_config     = "~/ssh_config"
commit         = false
push           = true
`

func TestLoadFullSample(t *testing.T) {
	home := isolate(t)

	got, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}

	no := false
	want := Config{
		Profile:       "work",
		PersonalStore: filepath.Join(home, "memory/personal"),
		WorkStore:     filepath.Join(home, "memory/work"),
		WorkOrgs:      []string{"github.com/factify-inc"},
		PersonalOrgs:  []string{"github.com/noamsto"},
		WorkNames:     []string{"corp-host.internal"},
		StateDir:      filepath.Join(home, "state"),
		EventRecord:   "/abs/record",
		Rules:         filepath.Join(home, "rules.toml"),
		Scanner:       "gitleaks",
		SSHConfig:     filepath.Join(home, "ssh_config"),
		Commit:        &no,
		Push:          true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %+v\nwant   %+v", got, want)
	}
}

func TestLoadRejects(t *testing.T) {
	isolate(t)
	cases := map[string]string{
		"unknown key":          "profile = \"work\"\nbogus = 1\n",
		"no profile":           "push = true\n",
		"bad profile":          "profile = \"office\"\n",
		"org without slash":    "profile = \"work\"\nwork_orgs = [\"factify-inc\"]\n",
		"org empty owner":      "profile = \"work\"\nwork_orgs = [\"github.com/\"]\n",
		"org empty host":       "profile = \"work\"\npersonal_orgs = [\"/noamsto\"]\n",
		"org with two slashes": "profile = \"work\"\nwork_orgs = [\"github.com/a/b\"]\n",
		"invalid toml":         "profile = \n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Errorf("Load accepted %q", body)
			}
		})
	}
}

func TestLoadValidatesStoresAndOrgs(t *testing.T) {
	home := isolate(t)
	const orgs = "work_orgs = [\"github.com/w\"]\n"
	accepted := map[string]string{
		"minimal":                 "profile = \"personal\"\npersonal_store = \"/m/personal\"\n" + orgs,
		"tilde paths":             "profile = \"work\"\npersonal_store = \"~/p\"\nwork_store = \"~/w\"\nstate_dir = \"~/s\"\n" + orgs,
		"siblings share a prefix": "profile = \"work\"\npersonal_store = \"/m/store\"\nwork_store = \"/m/store-work\"\nstate_dir = \"/m/store-state\"\n" + orgs,
	}
	for name, body := range accepted {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err != nil {
				t.Errorf("Load(%q): %v", body, err)
			}
		})
	}
	rejected := map[string]struct{ body, want string }{
		"no personal store":               {"profile = \"work\"\n" + orgs, `personal_store must be an absolute path, got ""`},
		"relative personal store":         {"profile = \"work\"\npersonal_store = \"memory/p\"\n" + orgs, `personal_store must be an absolute path, got "memory/p"`},
		"relative work store":             {"profile = \"work\"\npersonal_store = \"/m/p\"\nwork_store = \"memory/w\"\n" + orgs, `work_store must be an absolute path, got "memory/w"`},
		"relative state dir":              {"profile = \"work\"\npersonal_store = \"/m/p\"\nstate_dir = \"state\"\n" + orgs, `state_dir must be an absolute path, got "state"`},
		"no work orgs":                    {"profile = \"personal\"\npersonal_store = \"/m/p\"\n", "work_orgs is required"},
		"empty work orgs":                 {"profile = \"personal\"\npersonal_store = \"/m/p\"\nwork_orgs = []\n", "work_orgs is required"},
		"stores equal":                    {"profile = \"work\"\npersonal_store = \"/m/s\"\nwork_store = \"/m/s/\"\n" + orgs, "personal_store (/m/s) and work_store (/m/s) must not be the same or nested"},
		"work inside personal":            {"profile = \"work\"\npersonal_store = \"/m/p\"\nwork_store = \"/m/p/w\"\n" + orgs, "personal_store (/m/p) and work_store (/m/p/w) must not"},
		"personal inside work":            {"profile = \"work\"\npersonal_store = \"/m/w/p\"\nwork_store = \"/m/w\"\n" + orgs, "personal_store (/m/w/p) and work_store (/m/w) must not"},
		"state inside personal":           {"profile = \"work\"\npersonal_store = \"/m/p\"\nstate_dir = \"/m/p/.state\"\n" + orgs, "personal_store (/m/p) and state_dir (/m/p/.state) must not"},
		"personal inside state":           {"profile = \"work\"\npersonal_store = \"/m/s/p\"\nstate_dir = \"/m/s\"\n" + orgs, "personal_store (/m/s/p) and state_dir (/m/s) must not"},
		"work inside state":               {"profile = \"work\"\npersonal_store = \"/m/p\"\nwork_store = \"/m/s/w\"\nstate_dir = \"/m/s\"\n" + orgs, "work_store (/m/s/w) and state_dir (/m/s) must not"},
		"default state inside home store": {"profile = \"work\"\npersonal_store = \"" + home + "\"\n" + orgs, "personal_store (" + home + ") and the default state dir"},
	}
	for name, c := range rejected {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, c.body))
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
	body := fmt.Sprintf("profile = \"work\"\npersonal_store = %q\nwork_store = %q\nstate_dir = %q\nwork_orgs = [\"github.com/w\"]\n",
		symlink(t, personal, filepath.Join(links, "p")),
		filepath.Join(symlink(t, work, filepath.Join(links, "w")), "not-yet"),
		symlink(t, state, filepath.Join(links, "s")))

	c, err := Load(writeConfig(t, body))
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
	body := fmt.Sprintf("profile = \"personal\"\npersonal_store = %q\nwork_orgs = [\"github.com/w\"]\n", t.TempDir())

	c, err := Load(writeConfig(t, body))
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
	body := fmt.Sprintf("profile = \"work\"\npersonal_store = %q\nwork_store = %q\nstate_dir = %q\nwork_orgs = [\"github.com/w\"]\n",
		personal, work, t.TempDir())

	_, err := Load(writeConfig(t, body))

	if err == nil || !strings.Contains(err.Error(), "must not be the same or nested") {
		t.Errorf("err = %v, want a nesting error", err)
	}
}

func TestLoadRejectsADanglingSymlinkStore(t *testing.T) {
	isolate(t)
	dangling := symlink(t, filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "p"))
	body := fmt.Sprintf("profile = \"personal\"\npersonal_store = %q\nstate_dir = %q\nwork_orgs = [\"github.com/w\"]\n", dangling, t.TempDir())

	_, err := Load(writeConfig(t, body))

	if err == nil || !strings.Contains(err.Error(), "personal_store") {
		t.Errorf("err = %v, want an error naming personal_store", err)
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
	_, err := Load(writeConfig(t, "profile = \"work\"\nwork_path = \"x\"\n"))
	if err == nil || !strings.Contains(err.Error(), "work_path") {
		t.Errorf("err = %v, want it to name work_path", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	isolate(t)
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Error("Load of a missing file succeeded")
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
