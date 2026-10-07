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

	"github.com/BurntSushi/toml"
)

// statFS is what the trust walk reads the file system through.
type statFS interface {
	Lstat(name string) (fs.FileInfo, error)
	Readlink(name string) (string, error)
	ReadFile(name string) ([]byte, error)
}

type osFS struct{}

func (osFS) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }
func (osFS) Readlink(name string) (string, error)   { return os.Readlink(name) }
func (osFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name) //nolint:gosec // name is the trust path after checkTrustPath passed every component
}

// maxSymlinkHops matches Linux's MAXSYMLINKS, past which open fails ELOOP.
const maxSymlinkHops = 40

// checkTrustPath walks path as the kernel would, following every symlink,
// and returns the real path once every component on the way is owned by root
// and writable by no one else. With no component writable by a non-root user,
// reading the returned path after the check cannot be raced without root.
func checkTrustPath(fsys statFS, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("trust path %q is not absolute", path)
	}
	root, err := fsys.Lstat("/")
	if err != nil {
		return "", err
	}
	if err := checkDir("/", root); err != nil {
		return "", err
	}
	queue := strings.Split(path, "/")
	cur := "/"
	hops := 0
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := fsys.Lstat(next)
		if err != nil {
			return "", err
		}
		if err := ownedByRoot(next, fi); err != nil {
			return "", err
		}
		mode := fi.Mode()
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
				cur = "/"
			}
			queue = append(strings.Split(target, "/"), queue...)
		case mode.IsDir():
			if err := checkDir(next, fi); err != nil {
				return "", err
			}
			cur = next
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
	return "", fmt.Errorf("%s is not a regular file", cur)
}

// checkDir requires a root-owned directory only root can add entries to; a
// sticky directory such as /tmp or /nix/store lets others add entries but not
// replace root's.
func checkDir(path string, fi fs.FileInfo) error {
	if err := ownedByRoot(path, fi); err != nil {
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

func ownedByRoot(path string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner to check", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("%s is owned by uid %d, not root", path, st.Uid)
	}
	return nil
}

type trust struct {
	Profile      string   `toml:"profile"`
	WorkOrgs     []string `toml:"work_orgs"`
	PersonalOrgs []string `toml:"personal_orgs"`
	// TrustRoot is any so that a non-string value is off, not a parse error.
	TrustRoot any `toml:"trust_root"`
	Stores    struct {
		Personal *trustStore `toml:"personal"`
		Work     *trustStore `toml:"work"`
	} `toml:"stores"`
}

type trustStore struct {
	ID string `toml:"id"`
}

// storeID keeps an id usable as a path component.
var storeID = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// readTrust reads and validates the trust file at path once checkTrustPath
// has passed it.
func readTrust(fsys statFS, path string) (trust, error) {
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
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return trust{}, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
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
	if err := checkStoreID("stores.personal.id", t.Stores.Personal.ID); err != nil {
		return trust{}, err
	}
	switch {
	case t.Profile == "personal" && t.Stores.Work != nil:
		return trust{}, errors.New("stores.work is listed on a personal profile")
	case t.Profile == "work" && t.Stores.Work == nil:
		return trust{}, errors.New("stores.work is required on a work profile")
	case t.Stores.Work != nil:
		if err := checkStoreID("stores.work.id", t.Stores.Work.ID); err != nil {
			return trust{}, err
		}
		if t.Stores.Work.ID == t.Stores.Personal.ID {
			return trust{}, fmt.Errorf("stores.personal.id and stores.work.id must differ, both are %q", t.Stores.Work.ID)
		}
	}
	return t, nil
}

func checkStoreID(key, id string) error {
	if !storeID.MatchString(id) {
		return fmt.Errorf("%s %q must match [a-z0-9-]{1,64}", key, id)
	}
	return nil
}
