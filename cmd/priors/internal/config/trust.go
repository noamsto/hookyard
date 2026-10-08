package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
	"golang.org/x/crypto/ssh"
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
	return CheckOwnedPath(fsys, path, nil)
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
// that user.
//
// With leafUID set, the final regular file and the directory holding it must
// instead be owned by *leafUID and writable by it only, with no sticky
// exemption; every other component keeps the root rule. Each directory is
// checked as the parent of the next component, so a directory the walk also
// passes through inside (a symlink in it, a "..") must meet both rules.
func CheckOwnedPath(fsys StatFS, path string, leafUID *uint32) (string, error) {
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
		// last is the component the walk ends on, so cur holds the final file.
		// A failed Lstat counts, so a missing file in a sound leaf directory
		// reads as missing.
		last := len(queue) == 0
		if err == nil {
			last = last && fi.Mode()&fs.ModeSymlink == 0
		}
		owner := uint32(0)
		if last && leafUID != nil {
			owner = *leafUID
			if err := checkLeafDir(cur.path, cur.fi, owner); err != nil {
				return "", err
			}
		} else if err := checkDir(cur.path, cur.fi); err != nil {
			return "", err
		}
		if err != nil {
			return "", err
		}
		mode := fi.Mode()
		// A directory's own check waits for its role: parent of the final file
		// or one more component on the way.
		if !mode.IsDir() {
			if err := ownedBy(next, fi, owner); err != nil {
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

// checkLeafDir requires a directory only uid can add entries to. Sticky does
// not help: the file it holds would be another user's to replace.
func checkLeafDir(path string, fi fs.FileInfo, uid uint32) error {
	if err := ownedBy(path, fi, uid); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 != 0 {
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
	// TrustRoot is any so that any non-string value is off, not a parse error.
	TrustRoot  any              `toml:"trust_root"`
	AttestKeys []trustAttestKey `toml:"attest_keys"`
	Stores     struct {
		Personal *trustStore `toml:"personal"`
		Work     *trustStore `toml:"work"`
	} `toml:"stores"`

	// Set by readTrust from the file as read.
	keys      []AttestKey
	digest    string
	rootLabel string
}

type trustAttestKey struct {
	Key         string `toml:"key"`
	Attestation string `toml:"attestation"`
	Challenge   string `toml:"challenge"`
}

// AttestKey is an allowlisted attestation key with the hardware evidence it
// was enrolled with. The evidence is kept, not verified, here.
type AttestKey struct {
	Key         ssh.PublicKey
	Attestation []byte
	Challenge   []byte
}

type trustStore struct {
	ID string `toml:"id"`
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
			// trust_root decodes into any, so a table value's sub-keys stay undecoded.
			if k[0] != "trust_root" {
				keys = append(keys, k.String())
			}
		}
		if len(keys) > 0 {
			return trust{}, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
		}
	}
	sum := sha256.Sum256(data)
	t.digest = hex.EncodeToString(sum[:])
	if t.rootLabel, err = rootLabel(md, t.TrustRoot); err != nil {
		return trust{}, err
	}
	for i, k := range t.AttestKeys {
		key, err := parseAttestKey(fmt.Sprintf("attest_keys[%d]", i), k)
		if err != nil {
			return trust{}, err
		}
		t.keys = append(t.keys, key)
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

// rootLabel names trust_root for the attestation-off message: absent, a
// string verbatim, or any other value as TOML writes it.
func rootLabel(md toml.MetaData, v any) (string, error) {
	if !md.IsDefined("trust_root") {
		return "absent", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	switch v.(type) {
	case map[string]any:
		return "a table", nil
	case []map[string]any:
		return "an array of tables", nil
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(map[string]any{"v": v}); err != nil {
		return "", fmt.Errorf("trust_root: %w", err)
	}
	return strings.TrimSuffix(strings.TrimPrefix(b.String(), "v = "), "\n"), nil
}

// parseAttestKey parses the allowlist entry named at.
func parseAttestKey(at string, k trustAttestKey) (AttestKey, error) {
	for _, f := range []struct{ name, v string }{{"key", k.Key}, {"attestation", k.Attestation}, {"challenge", k.Challenge}} {
		if f.v == "" {
			return AttestKey{}, fmt.Errorf("%s.%s is required", at, f.name)
		}
	}
	// ParseAuthorizedKey skips lines it cannot parse and cuts a line at \r,
	// so anything past one line could carry a different key.
	if strings.ContainsAny(k.Key, "\r\n") {
		return AttestKey{}, fmt.Errorf("%s.key must be one line", at)
	}
	key, _, options, _, err := ssh.ParseAuthorizedKey([]byte(k.Key))
	if err != nil {
		return AttestKey{}, fmt.Errorf("%s.key: %w", at, err)
	}
	if len(options) > 0 {
		return AttestKey{}, fmt.Errorf("%s.key has options %q", at, options)
	}
	attestation, err := base64.StdEncoding.Strict().DecodeString(k.Attestation)
	if err != nil {
		return AttestKey{}, fmt.Errorf("%s.attestation: %w", at, err)
	}
	challenge, err := base64.StdEncoding.Strict().DecodeString(k.Challenge)
	if err != nil {
		return AttestKey{}, fmt.Errorf("%s.challenge: %w", at, err)
	}
	return AttestKey{Key: key, Attestation: attestation, Challenge: challenge}, nil
}
