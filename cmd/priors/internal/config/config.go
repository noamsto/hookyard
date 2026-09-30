// Package config loads priors' TOML config and resolves the directories it
// leaves to defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Profile       string   `toml:"profile"`
	PersonalStore string   `toml:"personal_store"`
	WorkStore     string   `toml:"work_store"`
	WorkOrgs      []string `toml:"work_orgs"`
	PersonalOrgs  []string `toml:"personal_orgs"`
	WorkNames     []string `toml:"work_names"`
	StateDir      string   `toml:"state_dir"`
	EventRecord   string   `toml:"event_record"`
	Rules         string   `toml:"rules"`
	Scanner       string   `toml:"scanner"`
	SSHConfig     string   `toml:"ssh_config"`
	// Commit is a pointer so that unset means true.
	Commit *bool `toml:"commit"`
	Push   bool  `toml:"push"`
}

// DefaultPath is where the config lives when --config is not given.
func DefaultPath() string {
	if p := os.Getenv("PRIORS_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "priors", "config.toml")
	}
	return filepath.Join(homeDir(), ".config", "priors", "config.toml")
}

// Load reads and validates the config at path. A typo'd key is an error, not
// a silently ignored setting.
func Load(path string) (Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return Config{}, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if c.Profile != "work" && c.Profile != "personal" {
		return Config{}, fmt.Errorf(`%s: profile must be "work" or "personal", got %q`, path, c.Profile)
	}
	if c.WorkOrgs, err = normalizeOrgs("work_orgs", c.WorkOrgs); err != nil {
		return Config{}, err
	}
	if c.PersonalOrgs, err = normalizeOrgs("personal_orgs", c.PersonalOrgs); err != nil {
		return Config{}, err
	}
	for _, p := range []*string{&c.PersonalStore, &c.WorkStore, &c.StateDir, &c.EventRecord, &c.Rules, &c.Scanner, &c.SSHConfig} {
		if *p, err = expandHome(*p); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

// State is where priors keeps its local layer, quarantine and locks.
func (c Config) State() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	return stateHome("priors")
}

// RecordDir is hookyard's state directory, following hookyard's own chain so
// priors finds the records hookyard wrote; the records sit under stream/.
func (c Config) RecordDir() string {
	if c.EventRecord != "" {
		return c.EventRecord
	}
	if d := os.Getenv("HOOKYARD_STATE_DIR"); d != "" {
		return d
	}
	return stateHome("hookyard")
}

func (c Config) CommitEnabled() bool {
	return c.Commit == nil || *c.Commit
}

// WorkPresent reports whether this host is a work host with a work clone to
// read and write.
func (c Config) WorkPresent() bool {
	if c.Profile != "work" {
		return false
	}
	info, err := os.Stat(c.WorkStore)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func stateHome(name string) string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, name)
	}
	return filepath.Join(homeDir(), ".local", "state", name)
}

func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

func expandHome(p string) (string, error) {
	rest, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, rest), nil
}

func normalizeOrgs(field string, orgs []string) ([]string, error) {
	if len(orgs) == 0 {
		return nil, nil
	}
	out := make([]string, len(orgs))
	for i, o := range orgs {
		host, owner, ok := strings.Cut(o, "/")
		if !ok || host == "" || owner == "" || strings.Contains(owner, "/") {
			return nil, fmt.Errorf("%s: %q must be host/owner", field, o)
		}
		out[i] = strings.ToLower(o)
	}
	return out, nil
}
