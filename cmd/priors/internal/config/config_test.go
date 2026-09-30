package config

import (
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
	home := t.TempDir()
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
