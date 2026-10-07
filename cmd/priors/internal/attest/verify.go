package attest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
)

// Options are a Verifier's sources of host state.
type Options struct {
	Groups GroupSource
	Now    func() time.Time
	// FS is what verdict files are stat'ed and read through.
	FS config.StatFS
}

// Verifier runs the index-time checks 1–3 over one run's facts. It reads a
// store's verdict file at most once, on that store's first claiming fact,
// and runs no process and no git, so each claiming fact costs one entry read
// and one in-process signature check.
type Verifier struct {
	cfg     config.Config
	o       Options
	host    *Host
	stores  map[route.StoreID]*storeCheck
	reports []string
	seen    map[string]bool
}

// storeCheck is a store's check-1 result; verdicts is set only when ok.
type storeCheck struct {
	id       string
	ok       bool
	verdicts Verdicts
}

// errLapsed is an entry the verdict file lists lapsed, reported as stale
// rather than as a failed check.
var errLapsed = errors.New("lapsed")

// NewVerifier does no I/O; a store is first checked at its first claiming fact.
func NewVerifier(cfg config.Config, o Options) *Verifier {
	return &Verifier{cfg: cfg, o: o, stores: map[route.StoreID]*storeCheck{}, seen: map[string]bool{}}
}

// ForHost is the Verifier for this process on this host.
func ForHost(cfg config.Config) *Verifier {
	return NewVerifier(cfg, Options{Groups: OSGroups, Now: time.Now, FS: cfg.FS()})
}

// Reviewed reports whether e reads reviewed: it claims review and passes
// checks 1–3. A claiming fact that fails is recorded in Reports.
func (v *Verifier) Reviewed(e store.Entry) bool {
	if !Claims(e.Fact) {
		return false
	}
	if e.Root.Kind != store.KindCheckout {
		v.reject(e, &CheckError{Check: 2, Reason: "not in a checkout"})
		return false
	}
	s := v.check1(e.Root.Store)
	if !s.ok {
		return false
	}
	err := v.check3(e, s)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errLapsed):
		v.report(fmt.Sprintf("%s/%s: stale attestation: re-attest or revoke", e.Root.Store, e.Rel))
	default:
		v.reject(e, err)
	}
	return false
}

// Reports are the run's attestation reports in first-seen order, each once.
// Check 1's are per store; the rest per fact. None quotes fact text.
func (v *Verifier) Reports() []string { return v.reports }

func (v *Verifier) report(line string) {
	if v.seen[line] {
		return
	}
	v.seen[line] = true
	v.reports = append(v.reports, line)
}

func (v *Verifier) reject(e store.Entry, err error) {
	var ce *CheckError
	if !errors.As(err, &ce) {
		ce = &CheckError{Check: 3, Reason: err.Error()}
	}
	v.report(fmt.Sprintf("%s/%s: proposed: %v", e.Root.Store, e.Rel, ce))
}

func (v *Verifier) classify() Host {
	if v.host == nil {
		h := Classify(v.cfg, v.o.Groups)
		v.host = &h
	}
	return *v.host
}

// check1 classifies the host before any verdict-file access, so no verdict
// file is opened off a separate trust root.
func (v *Verifier) check1(id route.StoreID) *storeCheck {
	if s, ok := v.stores[id]; ok {
		return s
	}
	s := &storeCheck{id: TrustStoreID(v.cfg, id)}
	v.stores[id] = s
	if h := v.classify(); !h.On() {
		v.report(fmt.Sprintf("%s store: %s", id, h.Off))
		return s
	}
	uid, err := strconv.ParseUint(VerifyUID, 10, 32)
	if err != nil {
		v.report(fmt.Sprintf("%s store: check 1: verdict owner not pinned", id))
		return s
	}
	s.verdicts, err = ReadVerdicts(v.o.FS, VerdictPath(s.id), s.id, v.cfg.TrustDigest, uint32(uid), v.o.Now())
	if err != nil {
		v.report(fmt.Sprintf("%s store: check 1: %v", id, err))
		return s
	}
	s.ok = true
	return s
}

// check3 judges the entry bytes read once, then checks 4–6 on those bytes
// and binds them to the fact.
func (v *Verifier) check3(e store.Entry, s *storeCheck) error {
	name := e.Fact.Name
	if !fact.NameRE.MatchString(name) {
		return &CheckError{Check: 3, Reason: "bad name"}
	}
	raw, err := readEntry(e.Root.Path, name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	switch state := s.verdicts.States[hex.EncodeToString(sum[:])]; state {
	case StateReviewed:
	case StateLapsed:
		return errLapsed
	case "":
		return &CheckError{Check: 3, Reason: "attest entry not in the verdict file"}
	default:
		return &CheckError{Check: 3, Reason: "attest entry is " + string(state)}
	}
	entry, err := ParseEntry(raw)
	if err != nil {
		return &CheckError{Check: 3, Reason: "attest entry: " + err.Error()}
	}
	if err := VerifyEntry(entry, v.classify().Keys, s.id, name, OpAttest); err != nil {
		return err
	}
	return Bind(entry, e.Rel, e.Raw)
}

// readEntry reads root/.attest/<name> through one descriptor that follows no
// symlink and cannot block on a FIFO, so the bytes hashed are the bytes
// checked.
func readEntry(root, name string) ([]byte, error) {
	dir := filepath.Join(root, ".attest")
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, &CheckError{Check: 3, Reason: "no attest entry"}
	case err != nil:
		return nil, &CheckError{Check: 3, Reason: ".attest: " + cause(err)}
	case !fi.IsDir():
		return nil, &CheckError{Check: 3, Reason: ".attest is not a directory"}
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // name passed fact.NameRE, and nothing but a regular file under 4 KiB is read
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, &CheckError{Check: 3, Reason: "no attest entry"}
	case errors.Is(err, syscall.ELOOP):
		return nil, &CheckError{Check: 3, Reason: "attest entry is a symlink"}
	case err != nil:
		return nil, &CheckError{Check: 3, Reason: "attest entry: " + cause(err)}
	}
	defer func() { _ = f.Close() }()
	if fi, err = f.Stat(); err != nil {
		return nil, &CheckError{Check: 3, Reason: "attest entry: " + cause(err)}
	}
	if !fi.Mode().IsRegular() {
		return nil, &CheckError{Check: 3, Reason: "attest entry is not a regular file"}
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxEntryBytes+1))
	if err != nil {
		return nil, &CheckError{Check: 3, Reason: "attest entry: " + cause(err)}
	}
	if len(raw) > MaxEntryBytes {
		return nil, &CheckError{Check: 3, Reason: "attest entry over 4 KiB"}
	}
	return raw, nil
}

// cause is err's errno without the path a PathError adds, which would name
// the user's home.
func cause(err error) string {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno.Error()
	}
	return err.Error()
}
