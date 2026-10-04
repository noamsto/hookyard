// Package config loads priors' TOML config and resolves the directories it
// leaves to defaults.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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

	// defaultState is the resolved default state dir, set by Load when
	// state_dir is unset.
	defaultState string
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
	if len(c.WorkOrgs) == 0 {
		return Config{}, fmt.Errorf("%s: work_orgs is required on every host", path)
	}
	if c.PersonalOrgs, err = normalizeOrgs("personal_orgs", c.PersonalOrgs); err != nil {
		return Config{}, err
	}
	for _, p := range []*string{&c.PersonalStore, &c.WorkStore, &c.StateDir, &c.EventRecord, &c.Rules, &c.Scanner, &c.SSHConfig} {
		if *p, err = expandHome(*p); err != nil {
			return Config{}, err
		}
	}
	if err := c.resolveDirs(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.checkDirs(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// resolveDirs replaces each absolute store and state path with its real one,
// so that a symlinked store reads like the directory it points at and the
// nesting check sees through symlinks. A relative path is left for checkDirs
// to reject.
func (c *Config) resolveDirs() error {
	if c.StateDir == "" {
		c.defaultState = stateHome("priors")
	}
	for _, d := range []struct {
		key string
		p   *string
	}{
		{"personal_store", &c.PersonalStore},
		{"work_store", &c.WorkStore},
		{"state_dir", &c.StateDir},
		{"the default state dir", &c.defaultState},
	} {
		if !filepath.IsAbs(*d.p) {
			continue
		}
		resolved, err := ResolveDir(*d.p)
		if err != nil {
			return fmt.Errorf("%s: %w", d.key, err)
		}
		*d.p = resolved
	}
	return nil
}

// ResolveDir is p with every symlink resolved. When p does not exist yet, its
// longest existing ancestor is resolved and the rest appended. A dangling
// symlink on the way is an error: the directory it names cannot be told.
func ResolveDir(p string) (string, error) {
	var rest []string
	for cur := filepath.Clean(p); ; cur = filepath.Dir(cur) {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			return "", fmt.Errorf("%s is a dangling symlink", cur)
		}
		if filepath.Dir(cur) == cur {
			return "", err
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
	}
}

// checkDirs requires absolute store and state paths and keeps them apart: a
// store nested in another, or in the state dir, would publish one layer's
// files through another's checkout.
func (c Config) checkDirs() error {
	type dir struct{ key, path string }
	dirs := []dir{{"personal_store", c.PersonalStore}}
	if c.WorkStore != "" {
		dirs = append(dirs, dir{"work_store", c.WorkStore})
	}
	if c.StateDir != "" {
		dirs = append(dirs, dir{"state_dir", c.StateDir})
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d.path) {
			return fmt.Errorf("%s must be an absolute path, got %q", d.key, d.path)
		}
	}
	stateKey := "state_dir"
	if c.StateDir == "" {
		stateKey = "the default state dir"
		if !filepath.IsAbs(c.State()) {
			return fmt.Errorf("%s must be an absolute path, got %q", stateKey, c.State())
		}
		dirs = append(dirs, dir{stateKey, c.State()})
	}
	for i, a := range dirs {
		for _, b := range dirs[i+1:] {
			if within(a.path, b.path) || within(b.path, a.path) {
				return fmt.Errorf("%s (%s) and %s (%s) must not be the same or nested", a.key, a.path, b.key, b.path)
			}
		}
	}
	root, err := gitWorkTree(c.State())
	if err != nil {
		return fmt.Errorf("%s (%s): %w", stateKey, c.State(), err)
	}
	if root != "" {
		return fmt.Errorf("%s (%s) is inside the git work tree at %s: the quarantine and local layers must stay off any checkout", stateKey, c.State(), root)
	}
	return nil
}

// gitWorkTree returns the nearest dir at or above p holding a .git entry (a
// repo's dir, a linked worktree's gitfile, or a symlink), or "" if none. It
// walks rather than running git, whose env and config an agent can set. An
// Lstat error other than absence is returned so the caller fails closed.
func gitWorkTree(p string) (string, error) {
	for dir := p; ; dir = filepath.Dir(dir) {
		_, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			return dir, nil
		}
		// ENOTDIR: a regular file at an ancestor cannot hold a .git.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", err
		}
		if filepath.Dir(dir) == dir {
			return "", nil
		}
	}
}

// within reports whether p is dir or lies under it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel)
}

// State is where priors keeps its local layer, quarantine, locks and
// provenance markers. Every path under it, and any guard over it, derives from
// the functions below so all of them see the same resolved directory.
func (c Config) State() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	if c.defaultState != "" {
		return c.defaultState
	}
	return stateHome("priors")
}

// QuarantineDir holds quarantined facts.
func (c Config) QuarantineDir() string { return filepath.Join(c.State(), "quarantine") }

// LocalDir holds the local layer of store id.
func (c Config) LocalDir(id string) string { return filepath.Join(c.State(), "local", id) }

// LockDir holds the writers' flock files.
func (c Config) LockDir() string { return filepath.Join(c.State(), "locks") }

// ProvenanceDir holds gate 2's per-session shell markers.
func (c Config) ProvenanceDir() string { return filepath.Join(c.State(), "provenance") }

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
