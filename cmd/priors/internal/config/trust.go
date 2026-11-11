package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	"github.com/BurntSushi/toml"
)

// StatFS is what an ownership walk reads the file system through.
type StatFS interface {
	Lstat(name string) (fs.FileInfo, error)
	Readlink(name string) (string, error)
	ReadFile(name string) ([]byte, error)
}

type osFS struct{}

func (osFS) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }
func (osFS) Readlink(name string) (string, error)   { return os.Readlink(name) }
func (osFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name) //nolint:gosec // name is a path after CheckOwnedPath passed every component
}

// maxSymlinkHops matches Linux's MAXSYMLINKS, past which open fails ELOOP.
const maxSymlinkHops = 40

// checkTrustPath is CheckOwnedPath with root owning every component.
func checkTrustPath(fsys StatFS, path string) (string, error) {
	return CheckOwnedPath(fsys, path)
}

// walkDir is a directory the walk has entered, kept so that its check can
// wait until the walk knows whether it holds the final file.
type walkDir struct {
	path string
	fi   fs.FileInfo
}

// CheckOwnedPath walks path as the kernel would, following every symlink,
// and returns the real path once every component on the way is owned by root
// and writable by no one else. With no component writable by another user,
// reading the returned path after the check cannot be raced without root or
// that user. Each directory is checked as the parent of the next component,
// so a directory the walk passes through (a symlink in it, a "..") is checked
// every time.
func CheckOwnedPath(fsys StatFS, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	root, err := fsys.Lstat("/")
	if err != nil {
		return "", err
	}
	dirs := []walkDir{{"/", root}}
	queue := strings.Split(path, "/")
	hops := 0
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		cur := dirs[len(dirs)-1]
		switch name {
		case "", ".":
			continue
		case "..":
			if err := checkDir(cur.path, cur.fi); err != nil {
				return "", err
			}
			if len(dirs) > 1 {
				dirs = dirs[:len(dirs)-1]
			}
			continue
		}
		next := filepath.Join(cur.path, name)
		fi, err := fsys.Lstat(next)
		if err := checkDir(cur.path, cur.fi); err != nil {
			return "", err
		}
		if err != nil {
			return "", err
		}
		mode := fi.Mode()
		// A directory's own check waits for its role: parent of the next
		// component.
		if !mode.IsDir() {
			if err := ownedBy(next, fi, 0); err != nil {
				return "", err
			}
		}
		switch {
		case mode&fs.ModeSymlink != 0:
			if hops++; hops > maxSymlinkHops {
				return "", fmt.Errorf("%s: too many symlinks", next)
			}
			target, err := fsys.Readlink(next)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				dirs = dirs[:1]
			}
			queue = append(strings.Split(target, "/"), queue...)
		case mode.IsDir():
			dirs = append(dirs, walkDir{next, fi})
		case len(queue) > 0:
			return "", fmt.Errorf("%s is not a directory", next)
		case !mode.IsRegular():
			return "", fmt.Errorf("%s is not a regular file", next)
		case mode.Perm()&0o022 != 0:
			return "", fmt.Errorf("%s is writable by group or others", next)
		default:
			return next, nil
		}
	}
	return "", fmt.Errorf("%s is not a regular file", dirs[len(dirs)-1].path)
}

// checkDir requires a root-owned directory only root can add entries to; a
// sticky directory such as /tmp or /nix/store lets others add entries but not
// replace root's.
func checkDir(path string, fi fs.FileInfo) error {
	if err := ownedBy(path, fi, 0); err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("%s is writable by group or others", path)
	}
	return nil
}

func ownedBy(path string, fi fs.FileInfo, uid uint32) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner to check", path)
	}
	if st.Uid == uid {
		return nil
	}
	want := "root"
	if uid != 0 {
		want = fmt.Sprintf("uid %d", uid)
	}
	return fmt.Errorf("%s is owned by uid %d, not %s", path, st.Uid, want)
}

type trust struct {
	Profile      string   `toml:"profile"`
	WorkOrgs     []string `toml:"work_orgs"`
	PersonalOrgs []string `toml:"personal_orgs"`
	// WorkNames is the floor the user config's work_names can only add to.
	WorkNames []string `toml:"work_names"`
	// TrustRoot and AttestKeys are deprecated and ignored: priors attestation
	// was removed (#289). They decode into any so a trust file generated for
	// the old schema still loads whatever shape the value has. Only
	// trust_root = "separate" is refused, since it asked for attestation.
	TrustRoot  any `toml:"trust_root"`
	AttestKeys any `toml:"attest_keys"`
	Stores     struct {
		Personal *trustStore `toml:"personal"`
		Work     *trustStore `toml:"work"`
	} `toml:"stores"`
}

type trustStore struct {
	ID string `toml:"id"`
	// Path is where the store is checked out. Remote is the origin the checkout
	// must have; empty means none is pinned.
	Path   string `toml:"path"`
	Remote string `toml:"remote"`
}

// storeID keeps an id usable as a path component.
var storeID = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// readTrust reads and validates the trust file at path once checkTrustPath
// has passed it.
func readTrust(fsys StatFS, path string) (trust, error) {
	resolved, err := checkTrustPath(fsys, path)
	if err != nil {
		return trust{}, err
	}
	data, err := fsys.ReadFile(resolved)
	if err != nil {
		return trust{}, err
	}
	var t trust
	md, err := toml.Decode(string(data), &t)
	if err != nil {
		return trust{}, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		var keys []string
		for _, k := range undecoded {
			// Both decode into any, so a table value's sub-keys stay undecoded.
			if k[0] != "trust_root" && k[0] != "attest_keys" {
				keys = append(keys, k.String())
			}
		}
		if len(keys) > 0 {
			return trust{}, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
		}
	}
	if s, _ := t.TrustRoot.(string); s == "separate" {
		return trust{}, errors.New(`trust_root = "separate" is no longer supported: priors attestation was removed (#289); remove the key`)
	}
	if t.Profile != "work" && t.Profile != "personal" {
		return trust{}, fmt.Errorf(`profile must be "work" or "personal", got %q`, t.Profile)
	}
	if t.WorkOrgs, err = normalizeOrgs("work_orgs", t.WorkOrgs); err != nil {
		return trust{}, err
	}
	if len(t.WorkOrgs) == 0 {
		return trust{}, errors.New("work_orgs is required")
	}
	if t.PersonalOrgs, err = normalizeOrgs("personal_orgs", t.PersonalOrgs); err != nil {
		return trust{}, err
	}
	if t.Stores.Personal == nil {
		return trust{}, errors.New("stores.personal is required")
	}
	if err := checkStore("stores.personal", *t.Stores.Personal); err != nil {
		return trust{}, err
	}
	switch {
	case t.Profile == "personal" && t.Stores.Work != nil:
		return trust{}, errors.New("stores.work is listed on a personal profile")
	case t.Profile == "work" && t.Stores.Work == nil:
		return trust{}, errors.New("stores.work is required on a work profile")
	case t.Stores.Work != nil:
		if err := checkStore("stores.work", *t.Stores.Work); err != nil {
			return trust{}, err
		}
		if t.Stores.Work.ID == t.Stores.Personal.ID {
			return trust{}, fmt.Errorf("stores.personal.id and stores.work.id must differ, both are %q", t.Stores.Work.ID)
		}
	}
	return t, nil
}

func checkStore(key string, s trustStore) error {
	if !storeID.MatchString(s.ID) {
		return fmt.Errorf("%s.id %q must match [a-z0-9-]{1,64}", key, s.ID)
	}
	if s.Path == "" {
		return fmt.Errorf("%s.path is required", key)
	}
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("%s.path %q must be absolute", key, s.Path)
	}
	if strings.HasPrefix(s.Remote, "-") {
		return fmt.Errorf(`%s.remote %q must not start with "-"`, key, s.Remote)
	}
	if strings.ContainsFunc(s.Remote, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("%s.remote %q must not hold whitespace or control characters", key, s.Remote)
	}
	return nil
}
