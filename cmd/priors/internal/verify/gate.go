package verify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// Env is what Prepare reads from the process.
type Env struct {
	Groups    attest.GroupSource
	UID       int
	VerifyUID string
	Root      string
}

// CheckEvidence is a stub until `priors trust check` (#240); the module writes
// an empty allowlist until it can run that check, so every listed key today
// was placed by a human; #240 replaces the body.
var CheckEvidence = func(config.AttestKey) error { return nil }

// Prepare gates a run and returns its Job. Nothing is created until every
// check has passed, so a refusal leaves the store directory untouched.
func Prepare(cfg config.Config, storeID, credential string, env Env) (*Job, error) {
	if h := attest.Classify(cfg, env.Groups); !h.On() {
		return nil, errors.New(h.Off)
	}
	uid, err := strconv.ParseUint(env.VerifyUID, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("verify uid %q: %w", env.VerifyUID, err)
	}
	if int(uid) != env.UID {
		return nil, fmt.Errorf("running as uid %d, not the verify uid %d", env.UID, uid)
	}
	remote, err := storeRemote(cfg, storeID)
	if err != nil {
		return nil, err
	}
	if !sshURL(remote) {
		return nil, errors.New("store remote is not an ssh url")
	}
	if !filepath.IsAbs(credential) {
		return nil, errors.New("credential path is not absolute")
	}
	for _, p := range []string{tools.SSHConfig, tools.KnownHosts} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("ssh pins: %w", tools.ErrUnpinned)
		}
	}
	var keys []ssh.PublicKey
	for _, k := range cfg.AttestKeys {
		if CheckEvidence(k) == nil {
			keys = append(keys, k.Key)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no attestation key passed the evidence check")
	}
	dir := filepath.Join(env.Root, storeID)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("verdict dir for store %s is missing", storeID)
	}
	if err := ensurePrivate(filepath.Join(dir, "private")); err != nil {
		return nil, err
	}
	return &Job{
		Store:       Store{ID: storeID, Remote: remote},
		Credential:  credential,
		Keys:        keys,
		TrustDigest: cfg.TrustDigest,
		Dir:         dir,
		Budget:      DefaultBudget,
		Now:         time.Now,
	}, nil
}

func storeRemote(cfg config.Config, id string) (string, error) {
	if id == "" {
		return "", errors.New("store id is empty")
	}
	var remote string
	switch id {
	case cfg.PersonalStoreID:
		remote = cfg.PersonalRemote
	case cfg.WorkStoreID:
		remote = cfg.WorkRemote
	default:
		return "", fmt.Errorf("unknown store %s", id)
	}
	if remote == "" {
		return "", fmt.Errorf("store %s has no remote", id)
	}
	return remote, nil
}

// sshURL reports whether remote is ssh://... or scp-like (host:path, with the
// colon before any slash).
func sshURL(remote string) bool {
	if strings.HasPrefix(remote, "ssh://") {
		return true
	}
	colon := strings.IndexByte(remote, ':')
	return colon > 0 && !strings.Contains(remote[:colon], "/") && !strings.HasPrefix(remote[colon:], "://")
}

func ensurePrivate(private string) error {
	if err := os.Mkdir(private, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(private)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		return errors.New("private dir is not a 0700 directory")
	}
	return nil
}

// Lock takes the store's run lock; the release func closes the descriptor.
func Lock(dir string) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, "private", "lock"), os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // dir is the job's own store dir, named by the trust file's validated id
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another run holds the lock")
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
