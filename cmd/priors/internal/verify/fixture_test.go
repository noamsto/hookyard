package verify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools/toolstest"
)

func TestMain(m *testing.M) {
	committest.UnsetRepoEnv()
	toolstest.Pin()
	os.Exit(m.Run())
}

// fixture is a store scenario: a bare remote reached through a fake ssh, a
// work clone that commits and pushes to it, and the job's verdict root.
type fixture struct {
	t      *testing.T
	root   string
	remote string // remote.git
	work   string
	id     string
	state  string // the job's <VerdictRoot>/<id>
	cred   string
	A, B   attesttest.Key
	n      int
}

// fakeSSH logs each invocation to ssh.log, then acts by the mode in ssh.mode:
// ok logs authentication as ssh -o LogLevel=VERBOSE does and runs git's
// remote command locally; hostkey and refused fail as ssh does before
// authentication; inject fails after authentication with stderr quoting a
// pre-authentication phrase, as fsck quotes pushed values; stall hangs until
// git dies and closes its stdin (a sleep would outlive the test and hold the
// descriptors nix develop waits on). The job's environment has no PATH, so
// every program is named by its absolute path.
const fakeSSH = `#!%[1]s
printf '%%s\n' "$*" >> %[2]s
for a; do last=$a; done
read -r mode < %[3]s
authed='Authenticated to fake ([127.0.0.1]:22) using "publickey".'
case $mode in
ok) echo "$authed" >&2; unset GIT_DIR; PATH=%[4]s; export PATH; exec %[5]s -c "$last" ;;
hostkey) echo 'Host key verification failed.' >&2; exit 255 ;;
refused) echo 'Permission denied (publickey).' >&2; exit 255 ;;
inject) echo "$authed" >&2; echo 'Host key verification failed.' >&2; exit 1 ;;
stall) while read -r _; do :; done; exit 255 ;;
esac
exit 1
`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if tools.Git == "" {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	f := &fixture{
		t:      t,
		root:   root,
		remote: filepath.Join(root, "remote.git"),
		work:   filepath.Join(root, "work"),
		id:     "personal",
		state:  filepath.Join(root, "var", "personal"),
		cred:   filepath.Join(root, "id_key"),
		A:      attesttest.NewSKEd25519(t),
		B:      attesttest.NewSKEd25519(t),
	}
	gitIn(t, root, "", "init", "-q", "--bare", "-b", "main", f.remote)
	gitIn(t, root, "", "init", "-q", "-b", "main", f.work)
	if err := os.MkdirAll(f.state, 0o755); err != nil {
		t.Fatal(err)
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	path := gitIn(t, root, "", "--exec-path") + ":" + filepath.Dir(sh)
	script := filepath.Join(root, "ssh")
	f.write(script, fmt.Sprintf(fakeSSH, sh, quote(filepath.Join(root, "ssh.log")),
		quote(filepath.Join(root, "ssh.mode")), quote(path), quote(sh)), 0o755)
	f.sshMode("ok")
	f.write(f.cred, "", 0o600)
	f.write(filepath.Join(root, "ssh_config"), "", 0o644)
	f.write(filepath.Join(root, "known_hosts"), "", 0o644)

	oldSSH, oldCfg, oldKH := tools.SSH, tools.SSHConfig, tools.KnownHosts
	tools.SSH, tools.SSHConfig, tools.KnownHosts = script, filepath.Join(root, "ssh_config"), filepath.Join(root, "known_hosts")
	t.Cleanup(func() { tools.SSH, tools.SSHConfig, tools.KnownHosts = oldSSH, oldCfg, oldKH })
	return f
}

func (f *fixture) write(path, content string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
}

// sshMode sets how the fake ssh answers the next fetch.
func (f *fixture) sshMode(mode string) {
	f.t.Helper()
	f.write(filepath.Join(f.root, "ssh.mode"), mode+"\n", 0o644)
}

// sshLog returns what the fake ssh has logged so far.
func (f *fixture) sshLog() string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "ssh.log"))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	return string(b)
}

// commit writes files and deletes del in the work clone, then commits.
func (f *fixture) commit(files map[string][]byte, del ...string) string {
	f.t.Helper()
	for _, rel := range del {
		if err := os.Remove(filepath.Join(f.work, rel)); err != nil {
			f.t.Fatal(err)
		}
	}
	for rel, data := range files {
		p := filepath.Join(f.work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.n++
	gitIn(f.t, f.work, "", "add", "-A")
	gitIn(f.t, f.work, "", "commit", "-q", "--allow-empty", "-m", "c"+strconv.Itoa(f.n))
	return gitIn(f.t, f.work, "", "rev-parse", "HEAD")
}

// push pushes the work clone's current branch to the same branch on the remote.
func (f *fixture) push() {
	f.t.Helper()
	gitIn(f.t, f.work, "", "push", "-q", f.remote, "HEAD")
}

// forcePush resets the work clone's current branch to oid and force-pushes it.
func (f *fixture) forcePush(oid string) {
	f.t.Helper()
	gitIn(f.t, f.work, "", "reset", "-q", "--hard", oid)
	gitIn(f.t, f.work, "", "push", "-q", "-f", f.remote, "HEAD")
}

// setHead switches the remote's default branch.
func (f *fixture) setHead(branch string) {
	f.t.Helper()
	gitIn(f.t, f.remote, "", "symbolic-ref", "HEAD", "refs/heads/"+branch)
}

// entry is a signed attest entry for fact at path, as the human's key signs it.
func (f *fixture) entry(key attesttest.Key, name, path string, fact []byte, seq uint64, op string) []byte {
	return attesttest.Signed(key, attesttest.Entry(f.id, name, path, hexSum(fact), seq, op), 0x05, attest.Namespace)
}

// junk is a well-formed entry with no valid signature, as an agent can push.
func (f *fixture) junk(name, path string, fact []byte, seq uint64) []byte {
	return append(attesttest.Entry(f.id, name, path, hexSum(fact), seq, "attest"),
		"-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"...)
}

// job gates a run through Prepare; with no keys, both A and B are allowlisted.
func (f *fixture) job(keys ...attesttest.Key) *Job {
	f.t.Helper()
	if len(keys) == 0 {
		keys = []attesttest.Key{f.A, f.B}
	}
	cfg := config.Config{
		TrustRoot:       "separate",
		TrustRootLabel:  "separate",
		PersonalStoreID: f.id,
		PersonalRemote:  "ssh://fake" + f.remote,
		Profile:         "personal",
		TrustDigest:     hexSum([]byte("trust")),
	}
	for _, k := range keys {
		cfg.AttestKeys = append(cfg.AttestKeys, config.AttestKey{Key: k.PublicKey(), Attestation: []byte("a"), Challenge: []byte("c")})
	}
	j, err := Prepare(cfg, f.id, f.cred, Env{
		Groups:    fakeGroups{names: map[int]string{100: "users"}},
		UID:       os.Getuid(),
		VerifyUID: strconv.Itoa(os.Getuid()),
		Root:      filepath.Join(f.root, "var"),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) run(j *Job) Result {
	f.t.Helper()
	res, err := j.Run(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// verdicts parses the verdict file the job wrote, so every scenario also
// checks that the job writes only what check 1 can read.
func (f *fixture) verdicts() attest.Verdicts {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, "verdicts"))
	if err != nil {
		f.t.Fatal(err)
	}
	v, err := attest.ParseVerdicts(b)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// digest is an entry's verdict-file key.
func digest(b []byte) string { return hexSum(b) }
