package attestcmd

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // the commit-graph file's trailer is sha1
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/verify"
)

const fixtureStoreID = "personal-t"

// fixture is a store scenario: a bare remote reached through a fake ssh, the
// store checkout the command runs in, a work clone that pushes to the remote,
// and the verdict job over that remote.
type fixture struct {
	t           *testing.T
	cfg         config.Config
	A, B        attesttest.Key
	root        string // store checkout
	remote      string // remote.git
	url         string
	work        string
	verdictRoot string
	job         *verify.Job
	statFS      config.StatFS
	// jobOnTrigger makes trigger run the job, as the verdict daemon would.
	jobOnTrigger bool
	triggers     int
	dir          string
	n            int
	out, errb    bytes.Buffer
	calls        int // signatures goSigner has made
	handle       string
}

// fakeSSH logs each invocation to ssh.log, then acts by the mode in ssh.mode:
// ok logs authentication as ssh -o LogLevel=VERBOSE does and runs git's remote
// command locally; hostkey fails as ssh does before authentication. The job's
// environment has no PATH, so every program is named by its absolute path.
const fakeSSH = `#!%[1]s
printf '%%s\n' "$*" >> %[2]s
for a; do last=$a; done
read -r mode < %[3]s
authed='Authenticated to fake ([127.0.0.1]:22) using "publickey".'
case $mode in
ok) echo "$authed" >&2; unset GIT_DIR; PATH=%[4]s; export PATH; exec %[5]s -c "$last" ;;
hostkey) echo 'Host key verification failed.' >&2; exit 255 ;;
esac
exit 1
`

type usersOnly struct{}

func (usersOnly) Gids() ([]int, error)         { return []int{100}, nil }
func (usersOnly) Name(gid int) (string, error) { return "users", nil }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if tools.Git == "" {
		t.Skip("git not installed")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	dir := t.TempDir()
	// The commit's identity comes from the user's git config, which a
	// sandboxed build lacks.
	gitconfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Priors Test\n\temail = priors@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := &fixture{
		t:           t,
		A:           attesttest.NewSKEd25519(t),
		B:           attesttest.NewSKEd25519(t),
		dir:         dir,
		root:        filepath.Join(dir, "store"),
		remote:      filepath.Join(dir, "remote.git"),
		work:        filepath.Join(dir, "work"),
		verdictRoot: filepath.Join(dir, "var"),
	}
	f.url = "ssh://fake" + f.remote
	if err := os.MkdirAll(filepath.Join(f.verdictRoot, fixtureStoreID), 0o755); err != nil {
		t.Fatal(err)
	}

	committest.PinPush(t)
	path := gitOut(t, dir, "", "--exec-path") + ":" + filepath.Dir(sh)
	script := filepath.Join(dir, "ssh")
	f.writeFile(script, fmt.Sprintf(fakeSSH, sh, shQuote(filepath.Join(dir, "ssh.log")),
		shQuote(filepath.Join(dir, "ssh.mode")), shQuote(path), shQuote(sh)), 0o755)
	f.sshMode("ok")
	f.writeFile(filepath.Join(dir, "id_key"), "", 0o600)
	oldSSH, oldRoot := tools.SSH, attest.VerdictRoot
	tools.SSH, attest.VerdictRoot = script, f.verdictRoot
	t.Cleanup(func() { tools.SSH, attest.VerdictRoot = oldSSH, oldRoot })

	gitOut(t, dir, "", "init", "-q", "--bare", "-b", "main", f.remote)
	f.seedStore()
	gitOut(t, dir, "", "clone", "-q", f.remote, f.work)

	f.cfg = config.Config{
		TrustRoot:       "separate",
		TrustRootLabel:  "separate",
		PersonalStoreID: fixtureStoreID,
		PersonalStore:   f.root,
		PersonalRemote:  f.url,
		Profile:         "personal",
		TrustDigest:     hexSum([]byte("trust")),
		StateDir:        filepath.Join(dir, "state"),
	}
	for _, k := range []attesttest.Key{f.A, f.B} {
		f.cfg.AttestKeys = append(f.cfg.AttestKeys, config.AttestKey{Key: k.PublicKey(), Attestation: []byte("a"), Challenge: []byte("c")})
	}
	f.job, err = verify.Prepare(f.cfg, fixtureStoreID, filepath.Join(dir, "id_key"), verify.Env{
		Groups:    usersOnly{},
		UID:       os.Getuid(),
		VerifyUID: strconv.Itoa(os.Getuid()),
		Root:      f.verdictRoot,
	})
	if err != nil || f.job == nil {
		t.Fatal(err)
	}
	f.statFS = ownerFS{leaf: filepath.Join(f.verdictRoot, fixtureStoreID), uid: uint32(os.Getuid())}
	return f
}

// goSigner signs in Go with an sk key. Its signatures are deterministic.
type goSigner struct {
	key   attesttest.Key
	calls *int
}

func (g goSigner) Sign(_ context.Context, _ string, payload []byte) ([]byte, error) {
	*g.calls++
	return g.key.SignSSHSIG(payload, attest.Namespace, 0x05), nil
}

// opts is a working Options for the fixture: key A's handle, a clean scanner,
// the in-process trigger and short waits. Output lands in f.out and f.errb.
func (f *fixture) opts() Options {
	f.t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		f.t.Fatal(err)
	}
	scanner := filepath.Join(f.dir, "scanner")
	f.writeFile(scanner, "#!"+sh+"\nprintf '[]'\n", 0o755)
	f.handle = filepath.Join(f.dir, "handle")
	f.writeFile(f.handle, string(f.A.Handle(5)), 0o600)
	rules, err := gate.LoadRules("")
	if err != nil {
		f.t.Fatal(err)
	}
	return Options{
		Config:    f.cfg,
		Rules:     rules,
		Scanner:   gate.Scanner{Bin: scanner},
		Groups:    usersOnly{},
		Env:       []string{"PATH=/usr/bin"},
		StatFS:    f.statFS,
		VerifyUID: strconv.Itoa(os.Getuid()),
		Signer:    goSigner{f.A, &f.calls},
		Terminal:  func() error { return nil },
		Now:       time.Now,
		Wait:      3 * time.Second,
		Poll:      50 * time.Millisecond,
		Trigger:   f.trigger,
		Out:       &f.out,
		Err:       &f.errb,
	}
}

// seedStore makes the checkout with two commits (the fact, then the index as
// priors index --write produces it), pushes them, and points origin at url.
func (f *fixture) seedStore() {
	f.t.Helper()
	gitOut(f.t, f.dir, "", "init", "-q", "-b", "main", f.root)
	fc := fact.Fact{
		Name:        "h",
		Description: "description of h",
		Metadata: fact.Metadata{
			NodeType:   "memory",
			Type:       "project",
			Scope:      "repo",
			Repos:      []string{"facts"},
			ValidFrom:  "2026-09-01",
			Verified:   "2026-09-01",
			Confidence: "proposed",
			Provenance: &fact.Provenance{Engine: "claude", Session: "s", Host: "h"},
			Modified:   "2026-09-01T00:00:00.000Z",
		},
		Body: "body of h\n",
	}
	b, err := fc.Marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := fact.Parse(b); err != nil {
		f.t.Fatal(err)
	}
	f.writeFile(filepath.Join(f.root, "facts", "h.md"), string(b), 0o644)
	gitOut(f.t, f.root, "", "add", "-A")
	gitOut(f.t, f.root, "", "commit", "-q", "-m", "fact")

	rules, err := gate.LoadRules("")
	if err != nil {
		f.t.Fatal(err)
	}
	r := store.Root{Store: route.StorePersonal, Kind: store.KindCheckout, Path: f.root}
	if _, _, err := r.WriteIndex(rules); err != nil {
		f.t.Fatal(err)
	}
	gitOut(f.t, f.root, "", "add", "-A")
	gitOut(f.t, f.root, "", "commit", "-q", "-m", "index")
	gitOut(f.t, f.root, "", "push", "-q", f.remote, "HEAD")
	gitOut(f.t, f.root, "", "remote", "add", "origin", f.url)
}

func (f *fixture) writeFile(path, content string, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
}

// sshMode sets how the fake ssh answers the next fetch.
func (f *fixture) sshMode(mode string) {
	f.t.Helper()
	f.writeFile(filepath.Join(f.dir, "ssh.mode"), mode+"\n", 0o644)
}

// sshLog returns what the fake ssh has logged so far.
func (f *fixture) sshLog() string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "ssh.log"))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	return string(b)
}

// commitPush writes files in the work clone, commits and pushes to the
// remote, and returns the new commit.
func (f *fixture) commitPush(files map[string][]byte) string {
	f.t.Helper()
	for rel, data := range files {
		f.writeFile(filepath.Join(f.work, rel), string(data), 0o644)
	}
	f.n++
	gitOut(f.t, f.work, "", "add", "-A")
	gitOut(f.t, f.work, "", "commit", "-q", "--allow-empty", "-m", "w"+strconv.Itoa(f.n))
	gitOut(f.t, f.work, "", "push", "-q", f.remote, "HEAD")
	return f.head(f.work)
}

// forcePush resets the work clone to oid and force-pushes it.
func (f *fixture) forcePush(oid string) {
	f.t.Helper()
	gitOut(f.t, f.work, "", "reset", "-q", "--hard", oid)
	gitOut(f.t, f.work, "", "push", "-q", "-f", f.remote, "HEAD")
}

// syncStore fast-forwards the store checkout to the remote's main.
func (f *fixture) syncStore() {
	f.t.Helper()
	gitOut(f.t, f.root, "", "fetch", "-q", f.remote, "main")
	gitOut(f.t, f.root, "", "merge", "-q", "--ff-only", "FETCH_HEAD")
}

func (f *fixture) head(dir string) string {
	f.t.Helper()
	return gitOut(f.t, dir, "", "rev-parse", "HEAD")
}

// trigger is the verdict daemon's poke: it counts, and runs the job when
// jobOnTrigger is set.
func (f *fixture) trigger(string) error {
	f.triggers++
	if f.jobOnTrigger {
		f.runJob()
	}
	return nil
}

func (f *fixture) runJob() verify.Result {
	f.t.Helper()
	if f.job == nil {
		f.t.Fatal("fixture has no job")
	}
	res, err := f.job.Run(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// verdicts reads the verdict file the job wrote through the same ownership
// walk check 1 uses.
func (f *fixture) verdicts() attest.Verdicts {
	f.t.Helper()
	v, err := attest.ReadVerdictLines(f.statFS, attest.VerdictPath(fixtureStoreID), fixtureStoreID, uint32(os.Getuid()))
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// signedEntry is an attest entry for the fact at factRel, as key signs it.
func (f *fixture) signedEntry(key attesttest.Key, seq uint64, op, factRel string, factBytes []byte) []byte {
	name := strings.TrimSuffix(filepath.Base(factRel), ".md")
	return attesttest.Signed(key, attesttest.Entry(fixtureStoreID, name, factRel, hexSum(factBytes), seq, op), 0x05, attest.Namespace)
}

// junkEntry is a well-formed entry for h with no valid signature, as an agent
// can push.
func (f *fixture) junkEntry(seq uint64) []byte {
	return append(attesttest.Entry(fixtureStoreID, "h", "facts/h.md", hexSum([]byte("x")), seq, "attest"),
		"-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"...)
}

// ownerFS is the real file system with owners faked: the verdict store dir
// and its contents belong to uid, everything else to root.
type ownerFS struct {
	leaf string
	uid  uint32
}

type ownedInfo struct {
	fs.FileInfo
	uid uint32
}

func (i ownedInfo) Sys() any { return &syscall.Stat_t{Uid: i.uid} }

func (o ownerFS) Lstat(name string) (fs.FileInfo, error) {
	fi, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	uid := uint32(0)
	if name == o.leaf || strings.HasPrefix(name, o.leaf+"/") {
		uid = o.uid
	}
	return ownedInfo{fi, uid}, nil
}

func (ownerFS) Readlink(name string) (string, error) { return os.Readlink(name) }
func (ownerFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// shQuote single-quotes s for the POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitOut runs git in dir with a throwaway identity and returns trimmed stdout.
func gitOut(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := tools.Command(context.Background(), tools.Git, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// plantGraph writes a commit-graph in repo that says commit has no parent.
// git writes only true parents, and refuses to write at all while a replace
// ref or graft exists, so it writes the true graph and patches the binary:
// the parent-1 field of commit's CDAT entry becomes "none", and the trailing
// sha1 is recomputed. Call it before planting a replace ref.
func plantGraph(t *testing.T, repo, commit string) {
	t.Helper()
	gitOut(t, repo, "", "commit-graph", "write", "--reachable")
	path := gitOut(t, repo, "", "rev-parse", "--path-format=absolute", "--git-path", "objects/info/commit-graph")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 8 || string(b[:4]) != "CGPH" || b[4] != 1 || b[5] != 1 {
		t.Fatalf("unexpected commit-graph header % x", b[:min(8, len(b))])
	}
	chunks := map[string]uint64{}
	for i := range int(b[6]) {
		e := b[8+12*i:]
		chunks[string(e[:4])] = binary.BigEndian.Uint64(e[4:12])
	}
	oidl, cdat := int(chunks["OIDL"]), int(chunks["CDAT"])
	if oidl == 0 || cdat == 0 {
		t.Fatalf("commit-graph lacks OIDL or CDAT: %v", chunks)
	}
	want, err := hex.DecodeString(commit)
	if err != nil || len(want) != sha1.Size {
		t.Fatalf("commit %q is not a sha1 oid", commit)
	}
	lo, hi := 0, (cdat-oidl)/sha1.Size
	for lo < hi {
		mid := (lo + hi) / 2
		switch got := b[oidl+mid*sha1.Size : oidl+(mid+1)*sha1.Size]; bytes.Compare(got, want) {
		case 0:
			binary.BigEndian.PutUint32(b[cdat+mid*36+20:], 0x70000000)
			sum := sha1.Sum(b[:len(b)-sha1.Size]) //nolint:gosec // the file's own trailer
			copy(b[len(b)-sha1.Size:], sum[:])
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		case 1:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	t.Fatalf("commit %s is not in the commit-graph", commit)
}

func TestFixture(t *testing.T) {
	f := newFixture(t)
	if _, err := os.Stat(filepath.Join(f.root, "facts", "h.md")); err != nil {
		t.Fatal(err)
	}
	if res := f.runJob(); res.Status != verify.StatusOK {
		t.Fatalf("job = %+v, want ok", res)
	}
	if v := f.verdicts(); v.Store != fixtureStoreID {
		t.Fatalf("verdicts = %+v", v)
	}

	plantGraph(t, f.root, f.head(f.root))
	if got := gitOut(t, f.root, "", "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("rev-list --count with the planted graph = %s, want 1", got)
	}
	if got := gitOut(t, f.root, "", "-c", "core.commitGraph=false", "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("rev-list --count without the graph = %s, want 2", got)
	}
}
