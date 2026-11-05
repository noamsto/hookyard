package attestcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/atomicfile"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/pin"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/verify"
)

// Options are the command's inputs and seams. The hooks run at the named
// points of an attest, for tests; production leaves them nil.
type Options struct {
	Config    config.Config
	Rules     gate.Rules
	Scanner   gate.Scanner
	Groups    attest.GroupSource
	Env       []string
	StatFS    config.StatFS
	VerifyUID string
	Signer    Signer
	Terminal  func() error
	Home      string
	Now       func() time.Time
	Wait      time.Duration
	Poll      time.Duration
	Trigger   func(path string) error
	Out, Err  io.Writer

	afterRead, afterSign, afterEntryWrite, beforeCommit, beforePush func()
}

// Request is one attest or revoke of the fact called Name, signed with the
// key handle at Handle. Store, when set, picks the store.
type Request struct {
	Name, Store, Handle string
	Op                  attest.Op
}

// DefaultWait is how long attest waits for the verdict job's answer.
var DefaultWait = 60 * time.Second

var skipTTY bool

// RequireTTY reports whether signing must find a controlling terminal; only
// a priorstest build can turn it off.
func RequireTTY() bool { return !skipTTY }

// LoaderVars names the environment's LD_* and DYLD_* variables, in order.
func LoaderVars(env []string) []string { return loaderVars(env) }

const (
	signTimeout  = 5 * time.Minute
	maxFactBytes = 1 << 20
)

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitHex = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var outcomeLabel = map[outcome]string{
	inEffect:    "in effect",
	notInEffect: "observed but not in effect",
	pending:     "not yet observed",
	notPushed:   "not pushed",
}

func (r report) String() string {
	label := outcomeLabel[r.outcome]
	if r.text == "" {
		return label
	}
	if strings.HasPrefix(r.text, label) {
		return r.text
	}
	return label + ": " + r.text
}

func exitCode(r report) int {
	if r.outcome == inEffect {
		return 0
	}
	return 1
}

func refuse(w io.Writer, err error) int {
	writef(w, "priors attest: %v\n", err)
	return 1
}

func writef(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func writeln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }

func hook(f func()) {
	if f != nil {
		f()
	}
}

// run is one attest or revoke in progress.
type run struct {
	o      Options
	req    Request
	host   attest.Host
	t      target
	repo   repo
	head   string
	branch string
	// raw is the fact as read and newRaw as it is committed; attest only.
	raw, newRaw []byte
	factMode    fs.FileMode
	seq         uint64
	payload     []byte
	entry       []byte
	digest      string
}

// Attest signs, commits and pushes an attest or revoke entry for r.Name, then
// waits for the verdict job's answer. It returns 0 only for an entry in
// effect; every refusal writes nothing.
func Attest(ctx context.Context, o Options, r Request) int {
	a := &run{o: o, req: r}
	if err := a.prepare(ctx); err != nil {
		return refuse(o.Err, err)
	}
	if err := a.sign(ctx); err != nil {
		return refuse(o.Err, err)
	}
	commitOid, failed, err := a.publish(ctx)
	if err != nil {
		return refuse(o.Err, err)
	}
	if failed != "" {
		writeln(o.Out, failed)
		return 1
	}
	if err := o.Trigger(attest.TriggerPath(a.t.storeID)); err != nil {
		writef(o.Err, "priors attest: trigger socket unreachable: %v; the job's timer will run\n", err)
	}
	writef(o.Out, "%s %s %s\n", r.Name, a.digest, commitOid)
	rep := wait(ctx, o.Wait, o.Poll, o.observer(ctx, a.repo, a.t, r.Op, a.digest, commitOid))
	writeln(o.Out, rep)
	if rep.outcome == pending {
		writef(o.Out, "priors attest --check %s %s %s\n", r.Name, a.digest, commitOid)
	}
	return exitCode(rep)
}

// Check re-reports the entry with digest sha at commit without signing,
// writing, committing or pushing.
func Check(ctx context.Context, o Options, name, storeName, sha, commitOid string) int {
	host, err := preflight(o)
	if err != nil {
		return refuse(o.Err, err)
	}
	if !sha256Hex.MatchString(sha) {
		return refuse(o.Err, fmt.Errorf("entry digest %q is not 64 lowercase hex digits", sha))
	}
	if !commitHex.MatchString(commitOid) {
		return refuse(o.Err, fmt.Errorf("commit %q is not 40 lowercase hex digits", commitOid))
	}
	t, err := findTarget(o.Config, name, storeName, false)
	if err != nil {
		return refuse(o.Err, err)
	}
	rp, err := openRepo(ctx, t)
	if err != nil {
		return refuse(o.Err, err)
	}
	e, err := entryAt(ctx, rp, commitOid, name, sha)
	if err != nil {
		return refuse(o.Err, err)
	}
	if err := attest.VerifyEntry(e, host.Keys, t.storeID, name, attest.OpAttest, attest.OpRevoke); err != nil {
		return refuse(o.Err, err)
	}
	writef(o.Out, "%s %s op: %s sequence: %d\n", name, sha, e.Op, e.Sequence)
	rep := o.observer(ctx, rp, t, e.Op, sha, commitOid)()
	writeln(o.Out, rep)
	return exitCode(rep)
}

// preflight runs the checks every mode makes before touching a store: no
// loader variable, attestation on for this host, and the git and push pins.
func preflight(o Options) (attest.Host, error) {
	if vars := loaderVars(o.Env); len(vars) > 0 {
		return attest.Host{}, fmt.Errorf("loader variable(s) %s set: unset them and run priors again", strings.Join(vars, ", "))
	}
	host := attest.Classify(o.Config, o.Groups)
	if !host.On() {
		return attest.Host{}, errors.New(host.Off)
	}
	if err := tools.Check(tools.Git); err != nil {
		return attest.Host{}, err
	}
	if _, err := pin.PushEnv(); err != nil {
		return attest.Host{}, err
	}
	return host, nil
}

// prepare runs every check and read before the signature, ending with the
// rendered fact and payload on o.Out.
func (a *run) prepare(ctx context.Context) error {
	var err error
	if a.host, err = preflight(a.o); err != nil {
		return err
	}
	if err := a.o.Terminal(); err != nil {
		return err
	}
	if _, err := handleKey(a.req.Handle, a.host.Keys); err != nil {
		return err
	}
	if a.t, err = findTarget(a.o.Config, a.req.Name, a.req.Store, a.req.Op == attest.OpAttest); err != nil {
		return err
	}
	if a.req.Op == attest.OpAttest {
		raw, mode, err := readFact(a.t.factPath())
		if err != nil {
			return err
		}
		a.raw, a.factMode = raw, mode
		hook(a.o.afterRead)
	}
	if a.repo, err = openRepo(ctx, a.t); err != nil {
		return err
	}
	if a.head, err = checkoutHead(ctx, a.t.root.Path); err != nil {
		return err
	}
	versions, err := a.cleanHistory(ctx)
	if err != nil {
		return err
	}
	if a.seq, err = nextSequence(summarize(versions, a.t.storeID, a.req.Name), a.supersededLines()); err != nil {
		return err
	}
	if err := a.buildPayload(versions); err != nil {
		return err
	}
	if a.req.Op == attest.OpAttest {
		files := map[string][]byte{a.t.entry.Rel: a.newRaw}
		if why := commit.Gate(ctx, a.o.Config, a.t.root, a.host, a.o.Rules, a.o.Scanner, files, true); why != "" {
			return errors.New("not gated for the store: " + why)
		}
		render(a.o.Out, a.newRaw)
	}
	render(a.o.Out, a.payload)
	return nil
}

// cleanHistory fetches the remote's default branch, requires HEAD to descend
// from it, and returns every version of the name's entry reachable from HEAD.
func (a *run) cleanHistory(ctx context.Context) ([][]byte, error) {
	branch, tip, err := a.repo.fetchDefault(ctx, a.t.root.Remote)
	if err != nil {
		return nil, err
	}
	a.branch = branch
	ok, err := a.repo.isAncestor(ctx, tip, a.head)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("HEAD does not descend from origin's %s; pull first", branch)
	}
	return a.repo.history(ctx, a.head, a.req.Name)
}

// supersededLines is the verdict file without its age or trust checks, nil
// when it cannot be read: a stale file still holds the record's highest
// superseded number, and a missing one only risks one retry numbered low.
func (a *run) supersededLines() *attest.Verdicts {
	uid, err := verifyUID(a.o.VerifyUID)
	if err != nil {
		return nil
	}
	v, err := attest.ReadVerdictLines(a.o.StatFS, attest.VerdictPath(a.t.storeID), a.t.storeID, uid)
	if err != nil {
		return nil
	}
	return &v
}

func (a *run) buildPayload(versions [][]byte) error {
	var path, sum string
	switch a.req.Op {
	case attest.OpAttest:
		newRaw, err := fact.WithConfidence(a.raw, "reviewed")
		if err != nil {
			return err
		}
		f, err := fact.Parse(newRaw)
		if err != nil {
			return err
		}
		if f.Metadata.Confidence != "reviewed" {
			return fmt.Errorf("fact %s has no confidence field to set", a.req.Name)
		}
		a.newRaw, path, sum = newRaw, a.t.entry.Rel, entryDigest(newRaw)
	case attest.OpRevoke:
		src, err := revokeSource(versions, a.host.Keys, a.t.storeID, a.req.Name)
		if err != nil {
			return err
		}
		path, sum = src.Path, src.SHA256
	default:
		return fmt.Errorf("unknown op %q", a.req.Op)
	}
	a.payload = fmt.Appendf(nil, "priors-attest v1\nstore: %s\nname: %s\npath: %s\nsha256: %s\nsequence: %d\nop: %s\n",
		a.t.storeID, a.req.Name, path, sum, a.seq, a.req.Op)
	return nil
}

func render(w io.Writer, b []byte) {
	s := Escape(b)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	_, _ = io.WriteString(w, s)
}

// sign has the payload signed and checks that the signature covers exactly
// the payload, by an allowlisted key, for this store and name: a signer that
// signed other bytes aborts with nothing written.
func (a *run) sign(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, signTimeout)
	defer cancel()
	sig, err := a.o.Signer.Sign(ctx, a.req.Handle, a.payload)
	if err != nil {
		return err
	}
	hook(a.o.afterSign)
	entry := append(bytes.Clone(a.payload), sig...)
	e, err := attest.ParseEntry(entry)
	if err != nil {
		return fmt.Errorf("the signed entry does not parse: %w; nothing written", err)
	}
	if !bytes.Equal(e.Signed, a.payload) {
		return errors.New("the signature covers other bytes than the payload; nothing written")
	}
	if err := attest.VerifyEntry(e, a.host.Keys, a.t.storeID, a.req.Name, a.req.Op); err != nil {
		return fmt.Errorf("the signed entry does not verify: %w; nothing written", err)
	}
	a.entry, a.digest = entry, entryDigest(entry)
	return nil
}

// publish writes, commits and pushes under the store's lock. A refusal
// before the write is err; a failure after it is failed, the line to report.
func (a *run) publish(ctx context.Context) (commitOid, failed string, err error) {
	unlock, err := store.Lock(a.o.Config, string(a.t.id))
	if err != nil {
		return "", "", err
	}
	defer unlock()

	files, failed, err := a.writeFiles()
	if err != nil || failed != "" {
		return "", failed, err
	}
	hook(a.o.afterEntryWrite)
	if why := commit.Gate(ctx, a.o.Config, a.t.root, a.host, a.o.Rules, a.o.Scanner, files, false); why != "" {
		return "", a.notCommitted("not gated for the store: " + why), nil
	}
	hook(a.o.beforeCommit)
	msg := fmt.Sprintf("%s: %s #%d", a.req.Op, a.req.Name, a.seq)
	commitOid, warning := commit.Files(ctx, a.t.root.Path, a.head, files, msg)
	if commitOid == "" {
		return "", a.notCommitted(warning), nil
	}
	if warning != "" {
		writef(a.o.Err, "priors attest: %s\n", warning)
	}
	if err := a.checkBlobs(ctx, commitOid); err != nil {
		return "", a.notPushed(err), nil
	}
	hook(a.o.beforePush)
	if err := commit.PushBranch(ctx, a.t.root.Path, a.t.root.Remote, commitOid, a.branch); err != nil {
		return "", a.notPushed(err), nil
	}
	return commitOid, "", nil
}

func (a *run) notCommitted(why string) string {
	left := "the signed entry and the edited fact are"
	if a.req.Op == attest.OpRevoke {
		left = "the signed entry is"
	}
	return fmt.Sprintf("not committed: %s; %s left in the working tree", why, left)
}

func (a *run) notPushed(err error) string {
	where := "the attest reads proposed everywhere"
	if a.req.Op == attest.OpRevoke {
		where = "the revoke is in effect nowhere"
	}
	return report{notPushed, fmt.Sprintf("%v; %s", err, where)}.String()
}

// writeFiles writes the reviewed fact, unless it changed since it was read,
// and the entry, and returns them as the commit carries them. A refusal
// before any write is err; a failure after the fact's write is failed, the
// line to report.
func (a *run) writeFiles() (files map[string][]byte, failed string, err error) {
	root := a.t.root
	dir := filepath.Join(root.Path, ".attest")
	files = map[string][]byte{".attest/" + a.req.Name: a.entry}
	path, writeFact := a.t.factPath(), false
	if a.req.Op == attest.OpAttest {
		cur, _, err := readFact(path)
		if err != nil || !bytes.Equal(cur, a.raw) {
			return nil, "", errors.New("fact changed since it was read; nothing written")
		}
		if writeFact = !bytes.Equal(a.newRaw, a.raw); writeFact {
			if err := root.CheckWrite(filepath.Dir(path)); err != nil {
				return nil, "", err
			}
		}
		files[a.t.entry.Rel] = a.newRaw
	}
	if err := root.CheckWrite(dir); err != nil {
		return nil, "", err
	}
	if writeFact {
		if err := atomicfile.Write(path, a.newRaw, a.factMode); err != nil {
			return nil, "", err
		}
	}
	if err := atomicfile.Write(filepath.Join(dir, a.req.Name), a.entry, 0o644); err != nil {
		if writeFact {
			return nil, fmt.Sprintf("not committed: writing the entry: %v; the edited fact is left in the working tree", err), nil
		}
		return nil, "", err
	}
	return files, "", nil
}

// checkBlobs reads back what the commit holds: git does not re-hash an object
// it already has, so a loose object planted at the entry's oid would carry
// other bytes under the signed entry's name. It also requires the commit to
// sit on HEAD and change only the entry and the fact.
func (a *run) checkBlobs(ctx context.Context, commitOid string) error {
	parents, err := a.repo.run(ctx, "rev-list", "--parents", "-n", "1", commitOid)
	if err != nil {
		return fmt.Errorf("reading the commit: %w", err)
	}
	if got := strings.Fields(parents); !slices.Equal(got, []string{commitOid, a.head}) {
		return errors.New("the commit's parent is not the checkout's HEAD")
	}
	changed, err := a.repo.run(ctx, "diff-tree", "--no-commit-id", "-r", "-z", "--name-only", a.head, commitOid)
	if err != nil {
		return fmt.Errorf("reading the commit: %w", err)
	}
	for rel := range strings.SplitSeq(strings.TrimSuffix(changed, "\x00"), "\x00") {
		if rel != ".attest/"+a.req.Name && (a.req.Op != attest.OpAttest || rel != a.t.entry.Rel) {
			return errors.New("the commit changes paths other than the fact and its entry")
		}
	}
	got, ok, err := a.repo.blobAt(ctx, commitOid, ".attest/"+a.req.Name, attest.MaxEntryBytes)
	if err != nil {
		return fmt.Errorf("reading the committed entry: %w", err)
	}
	if !ok || entryDigest(got) != a.digest {
		return errors.New("the committed entry does not match the signed bytes")
	}
	if a.req.Op != attest.OpAttest {
		return nil
	}
	got, ok, err = a.repo.blobAt(ctx, commitOid, a.t.entry.Rel, maxFactBytes)
	if err != nil {
		return fmt.Errorf("reading the committed fact: %w", err)
	}
	if !ok || !bytes.Equal(got, a.newRaw) {
		return errors.New("the committed fact does not match the signed bytes")
	}
	return nil
}

// observer returns a read of the verdict file classified for the entry with
// digest at commit.
func (o Options) observer(ctx context.Context, rp repo, t target, op attest.Op, digest, commitOid string) func() report {
	uid, uidErr := verifyUID(o.VerifyUID)
	fetched := map[string]bool{}
	descends := func(tip string) bool {
		if !rp.has(ctx, tip) && !fetched[tip] {
			fetched[tip] = true
			_, _, _ = rp.fetchDefault(ctx, t.root.Remote)
		}
		if !rp.has(ctx, tip) {
			return false
		}
		ok, err := rp.isAncestor(ctx, commitOid, tip)
		return err == nil && ok
	}
	return func() report {
		if uidErr != nil {
			return classify(op, digest, attest.Verdicts{}, fmt.Errorf("verdict job uid: %w", uidErr), descends)
		}
		v, err := attest.ReadVerdicts(o.StatFS, attest.VerdictPath(t.storeID), t.storeID, o.Config.TrustDigest, uid, o.Now())
		return classify(op, digest, v, err, descends)
	}
}

func verifyUID(s string) (uint32, error) {
	uid, err := strconv.ParseUint(s, 10, 32)
	return uint32(uid), err
}

// entryAt is the entry at .attest/name in commit, which must hash to sha.
func entryAt(ctx context.Context, rp repo, commitOid, name, sha string) (attest.Entry, error) {
	raw, ok, err := rp.blobAt(ctx, commitOid, ".attest/"+name, attest.MaxEntryBytes)
	if err != nil {
		return attest.Entry{}, err
	}
	if !ok {
		return attest.Entry{}, fmt.Errorf("no .attest/%s at %s", name, commitOid)
	}
	if entryDigest(raw) != sha {
		return attest.Entry{}, fmt.Errorf("entry at %s does not hash to %s", commitOid, sha)
	}
	return attest.ParseEntry(raw)
}

// target is the store an attest acts on, and the fact found there.
type target struct {
	id      route.StoreID
	storeID string
	root    store.Root
	entry   store.Entry
	found   bool
}

func (t target) factPath() string {
	return filepath.Join(t.root.Path, filepath.FromSlash(t.entry.Rel))
}

// findTarget picks the store whose checkout holds the fact called name, or
// want's store. needLive requires a live fact there; without it an archived
// fact, or none with want set, will do. The store must pin an SSH remote, the
// only kind the verdict job fetches.
func findTarget(cfg config.Config, name, want string, needLive bool) (target, error) {
	if !fact.NameRE.MatchString(name) {
		return target{}, fmt.Errorf("%q is not a fact name", name)
	}
	ids := []route.StoreID{route.StorePersonal}
	if cfg.WorkPresent() {
		ids = append(ids, route.StoreWork)
	}
	if want != "" {
		if !slices.Contains(ids, route.StoreID(want)) {
			return target{}, fmt.Errorf("--store must name a store configured on this host (personal or work), got %q", want)
		}
		ids = []route.StoreID{route.StoreID(want)}
	}
	candidates := make([]target, 0, len(ids))
	var hits []target
	for _, id := range ids {
		t := target{id: id, storeID: attest.TrustStoreID(cfg, id), root: checkoutRoot(cfg, id)}
		t.entry, t.found = lookup(t.root, name, needLive)
		candidates = append(candidates, t)
		if t.found {
			hits = append(hits, t)
		}
	}
	var t target
	switch {
	case len(hits) > 1:
		return target{}, fmt.Errorf("fact %s is in both the %s and %s stores: pass --store", name, hits[0].id, hits[1].id)
	case len(hits) == 1:
		t = hits[0]
	case needLive:
		return target{}, fmt.Errorf("no live fact named %s in the store checkouts", name)
	case want == "":
		return target{}, fmt.Errorf("no fact named %s in the store checkouts: pass --store", name)
	default:
		t = candidates[0]
	}
	if t.root.Remote == "" {
		return target{}, fmt.Errorf("the %s store pins no remote: no verdict job can observe an attest", t.id)
	}
	if !verify.SSHURL(t.root.Remote) {
		return target{}, fmt.Errorf("the %s store's pinned remote %q is not an SSH URL: no verdict job can observe an attest", t.id, t.root.Remote)
	}
	return t, nil
}

// checkoutRoot is the store's checkout root without store.CheckoutRoot's pin
// check, which runs git: the graft and shallow refusal comes first.
func checkoutRoot(cfg config.Config, id route.StoreID) store.Root {
	path, remote := cfg.PersonalStore, cfg.PersonalRemote
	if id == route.StoreWork {
		path, remote = cfg.WorkStore, cfg.WorkRemote
	}
	return store.Root{Store: id, Kind: store.KindCheckout, Path: path, Remote: remote}
}

func lookup(root store.Root, name string, needLive bool) (store.Entry, bool) {
	if !needLive {
		return root.FindByName(name)
	}
	entries, _ := root.Walk()
	for _, e := range entries {
		if e.Fact.Name == name && !e.Archived {
			return e, true
		}
	}
	return store.Entry{}, false
}

// readFact reads the fact file once, refusing a symlink, a non-regular file
// and one over 1 MiB, and returns its bytes and permission bits.
func readFact(path string) ([]byte, fs.FileMode, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // path is a fact the store walk found
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFactBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > maxFactBytes {
		return nil, 0, fmt.Errorf("%s is over 1 MiB", path)
	}
	if len(raw) == 0 {
		return nil, 0, fmt.Errorf("%s is empty", path)
	}
	return raw, info.Mode().Perm(), nil
}

// openRepo refuses grafts and shallow history from files alone, then the
// checkout's pin, and returns the clean object runner over it.
func openRepo(ctx context.Context, t target) (repo, error) {
	gitDir, common, err := gitDirs(t.root.Path)
	if err != nil {
		return repo{}, err
	}
	if err := refuseGrafts(gitDir, common); err != nil {
		return repo{}, err
	}
	if err := pin.Check(ctx, t.root.Path, t.root.Remote); err != nil {
		return repo{}, err
	}
	return newRepo(t.root.Path, common)
}

// checkoutHead is the checkout's HEAD commit, read under the user's config
// like pin.Check but with replace refs off; the push repo is sha1 only.
func checkoutHead(ctx context.Context, dir string) (string, error) {
	head, err := checkoutGit(ctx, dir, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil || !commitHex.MatchString(head) {
		return "", fmt.Errorf("the store checkout %s has no HEAD commit", dir)
	}
	format, err := checkoutGit(ctx, dir, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	if format != "sha1" {
		return "", fmt.Errorf("the store checkout is %s; priors attest needs sha1", format)
	}
	return head, nil
}

func checkoutGit(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	if len(args) == 0 {
		return "", errors.New("git: no subcommand")
	}
	cmd := tools.Command(ctx, tools.Git, append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = route.RepoEnv("LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return "", gitErr(args[0], err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
