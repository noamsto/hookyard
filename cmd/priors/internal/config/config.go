// Package config loads priors' TOML config and resolves the directories it
// leaves to defaults.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Read from the trust file only; a user config naming any of them is an
	// error.
	Profile      string   `toml:"-"`
	WorkOrgs     []string `toml:"-"`
	PersonalOrgs []string `toml:"-"`
	// PersonalStoreID and WorkStoreID are the trust file's store ids, which
	// name a store in attestation; they are not route.StoreID kinds.
	PersonalStoreID string `toml:"-"`
	WorkStoreID     string `toml:"-"`
	// TrustRoot is kept verbatim; only "separate" turns attestation on. A
	// non-string scalar or array in the trust file reads as empty, which is off.
	TrustRoot string `toml:"-"`
	// TrustRootLabel names trust_root for the attestation-off message:
	// "absent", a string verbatim, or any other value as TOML writes it.
	TrustRootLabel string `toml:"-"`
	// AttestKeys is the attestation allowlist; empty means no fact reads
	// reviewed.
	AttestKeys []AttestKey `toml:"-"`
	// TrustDigest is the lowercase hex sha256 of the trust file's bytes as
	// this run read them, which a verdict file must name.
	TrustDigest string `toml:"-"`

	PersonalStore string `toml:"personal_store"`
	WorkStore     string `toml:"work_store"`
	StateDir      string `toml:"state_dir"`
	EventRecord   string `toml:"event_record"`
	Rules         string `toml:"rules"`
	SSHConfig     string `toml:"ssh_config"`
	// Commit is a pointer so that unset means true.
	Commit *bool `toml:"commit"`
	Push   bool  `toml:"push"`

	// workNames is the trust file's floor plus the user config's additions; read
	// through WorkNames.
	workNames []string

	// defaultState is the resolved default state dir, set by Load when
	// state_dir is unset.
	defaultState string
	// fsys is the file system the trust file was read through.
	fsys StatFS
}

// FS is the file system the trust file was read through, for walking other
// root-owned paths the same way; the real one on a zero Config.
func (c Config) FS() StatFS {
	if c.fsys == nil {
		return osFS{}
	}
	return c.fsys
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

// Load reads the trust file, then validates the user config at path. A typo'd
// key is an error, not a silently ignored setting.
func Load(path string) (Config, error) {
	trustPath, fsys := trustSource()
	return load(path, trustPath, fsys)
}

func load(path, trustPath string, fsys StatFS) (Config, error) {
	t, err := readTrust(fsys, trustPath)
	if err != nil {
		return Config{}, fmt.Errorf("trust file %s: %w", trustPath, err)
	}
	var u struct {
		Config
		WorkNames []string `toml:"work_names"`
	}
	md, err := toml.DecodeFile(path, &u)
	if err != nil {
		return Config{}, err
	}
	if md.IsDefined("scanner") {
		return Config{}, fmt.Errorf("%s: scanner is no longer a config key: the secret scanner is pinned at build time", path)
	}
	for _, k := range []string{"profile", "work_orgs", "personal_orgs", "trust_root", "stores", "attest_keys"} {
		if md.IsDefined(k) {
			return Config{}, fmt.Errorf("%s: %s is read from the trust file %s, not the user config", path, k, trustPath)
		}
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	c := u.Config
	c.Profile, c.WorkOrgs, c.PersonalOrgs = t.Profile, t.WorkOrgs, t.PersonalOrgs
	c.TrustRoot, _ = t.TrustRoot.(string)
	c.workNames = unionNames(t.WorkNames, u.WorkNames)
	c.TrustRootLabel, c.AttestKeys, c.TrustDigest = t.rootLabel, t.keys, t.digest
	c.fsys = fsys
	c.PersonalStoreID = t.Stores.Personal.ID
	if t.Stores.Work != nil {
		c.WorkStoreID = t.Stores.Work.ID
	}
	for _, p := range []*string{&c.PersonalStore, &c.WorkStore, &c.StateDir, &c.EventRecord, &c.Rules, &c.SSHConfig} {
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

// WorkNames is the work-name list every scan uses: the trust file's floor plus
// the user config's additions, which cannot remove a floor name.
func (c Config) WorkNames() []string { return slices.Clone(c.workNames) }

func unionNames(floor, extra []string) []string {
	out := slices.Clone(floor)
	for _, n := range extra {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
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
	if err := RefuseWorkTree(c.State()); err != nil {
		return fmt.Errorf("%s: %w", stateKey, err)
	}
	return nil
}

// RefuseWorkTree errors when dir is inside a git work tree. Load checks the
// state dir once; a writer re-checks the layer dir it is about to write,
// since a .git or symlink can appear below the state dir afterwards.
func RefuseWorkTree(dir string) error {
	root, err := gitWorkTree(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if root != "" {
		return fmt.Errorf("%s is inside the git work tree at %s: the quarantine and local layers must stay off any checkout", dir, root)
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
