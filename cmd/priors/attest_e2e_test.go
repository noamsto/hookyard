package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

const attestStoreID = "personal-test"

// attestRig is a separate-host sandbox whose personal store is a clone of a
// bare remote reached through the pinned fake ssh, holding a fact "h" that
// descends from the remote.
type attestRig struct {
	*sandbox
	key    attesttest.Key
	bare   string
	handle string
}

func newAttestRig(t *testing.T, o trustOpts) *attestRig {
	t.Helper()
	sb := newSandbox(t, "personal")
	r := &attestRig{sandbox: sb, key: attesttest.NewSKEd25519(t), bare: filepath.Join(sb.dir, "remote.git")}
	sb.git(sb.dir, "init", "-q", "--bare", "-b", "main", r.bare)
	sb.personalRemote = "ssh://fake" + r.bare
	o.keys = []attesttest.Key{r.key}
	sb.writeTrustWith(o)

	sb.git(sb.personal, "remote", "add", "origin", sb.personalRemote)
	sb.putFact(sb.personal, "demo/h.md", newFact("h", "demo", "project"))
	sb.indexWrite()
	sb.git(sb.personal, "push", "-q", r.bare, "HEAD:main")

	r.handle = filepath.Join(sb.dir, "handle")
	if err := os.WriteFile(r.handle, r.key.Handle(5), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *attestRig) keygenLog() string { return r.handle + ".log" }

func (r *attestRig) wantNoSignature() {
	r.t.Helper()
	if _, err := os.Stat(r.keygenLog()); !errors.Is(err, os.ErrNotExist) {
		r.t.Errorf("keygen log exists (err %v): a signature was attempted", err)
	}
}

// state is what an attest must leave alone on a refusal: the store tree and
// the refs of the checkout and of the remote.
func (r *attestRig) state() []string {
	r.t.Helper()
	refs := func(dir string) string { return r.gitOut(dir, "for-each-ref") }
	return append(verdictTree(r.t, r.personal), refs(r.personal), refs(r.bare))
}

// runAttest runs priors with env added to the child's environment and, when
// detached, without a controlling terminal.
func (r *attestRig) runAttest(stdin string, env []string, detached bool, args ...string) result {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := r.command(ctx, args...)
	cmd.Env = append(cmd.Env, env...)
	if detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := result{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			r.t.Fatalf("running priors %v: %v", args, err)
		}
		res.code = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

var noTTY = []string{"PRIORS_TEST_NO_TTY=1"}

func TestAttestOffSeparateRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		opts trustOpts
		env  []string
		want string
	}{
		"absent":      {trustOpts{}, nil, "attestation off: trust_root"},
		"owner-admin": {trustOpts{root: "owner-admin"}, nil, "attestation off: trust_root"},
		"bogus":       {trustOpts{root: "bogus"}, nil, "attestation off: trust_root"},
		"non-string":  {trustOpts{extra: []string{"trust_root = true"}}, nil, "attestation off: trust_root"},
		"wheel":       {trustOpts{root: "separate"}, []string{"PRIORS_TEST_GROUPS=wheel:10"}, "attestation off: group"},
	} {
		for _, verb := range [][]string{{"attest", "h"}, {"attest", "--revoke", "h"}} {
			t.Run(name+"/"+strings.Join(verb, " "), func(t *testing.T) {
				r := newAttestRig(t, tc.opts)
				before := r.state()

				args := append([]string{verb[0], "-f", r.handle}, verb[1:]...)
				res := r.runAttest("", append(noTTY, tc.env...), false, args...)
				if res.code == 0 {
					t.Errorf("exit 0, stdout %q", res.stdout)
				}
				wantContains(t, "stderr", res.stderr, tc.want)
				r.wantNoSignature()
				if after := r.state(); !slices.Equal(before, after) {
					t.Errorf("state changed:\nbefore %q\nafter  %q", before, after)
				}
			})
		}
	}
}

func TestAttestLoaderVarsRefused(t *testing.T) {
	r := newAttestRig(t, trustOpts{root: "separate"})
	checkArgs := []string{"attest", "--check", "h", strings.Repeat("a", 64), strings.Repeat("b", 40)}
	for _, v := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "DYLD_INSERT_LIBRARIES"} {
		for _, args := range [][]string{
			{"attest", "-f", r.handle, "h"},
			{"attest", "--revoke", "-f", r.handle, "h"},
			checkArgs,
		} {
			res := r.runAttest("", append([]string{v + "=/nonexistent/lib.so"}, noTTY...), false, args...)
			wantExit(t, res, 1)
			wantContains(t, v+" "+strings.Join(args, " "), res.stderr, v)
		}
	}
	r.wantNoSignature()
}

func TestAttestShallowAndGraftsRefused(t *testing.T) {
	r := newAttestRig(t, trustOpts{root: "separate"})
	for _, rel := range []string{"shallow", filepath.Join("info", "grafts")} {
		path := filepath.Join(r.personal, ".git", rel)
		r.writeFile(path, "")
		for _, args := range [][]string{
			{"attest", "-f", r.handle, "h"},
			{"attest", "--check", "h", strings.Repeat("a", 64), strings.Repeat("b", 40)},
		} {
			res := r.runAttest("", noTTY, false, args...)
			wantExit(t, res, 1)
			wantContains(t, rel+" "+strings.Join(args, " "), res.stderr, path)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	r.wantNoSignature()
}

func TestAttestHandleNotAllowlisted(t *testing.T) {
	r := newAttestRig(t, trustOpts{root: "separate"})
	other := filepath.Join(r.dir, "other-handle")
	if err := os.WriteFile(other, attesttest.NewSKEd25519(t).Handle(5), 0o600); err != nil {
		t.Fatal(err)
	}
	before := r.state()

	res := r.runAttest("1234\n", noTTY, false, "attest", "-f", other, "h")
	wantExit(t, res, 1)
	wantContains(t, "stderr", res.stderr, "not on the allowlist")
	r.wantNoSignature()
	if _, err := os.Stat(other + ".log"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("keygen log for the unlisted handle exists (err %v)", err)
	}
	if after := r.state(); !slices.Equal(before, after) {
		t.Errorf("state changed:\nbefore %q\nafter  %q", before, after)
	}
}

func TestAttestNeedsTerminal(t *testing.T) {
	r := newAttestRig(t, trustOpts{root: "separate"})
	before := r.state()

	res := r.runAttest("1234\n", nil, true, "attest", "-f", r.handle, "h")
	wantExit(t, res, 1)
	wantContains(t, "stderr", res.stderr, "interactive terminal")
	r.wantNoSignature()
	if after := r.state(); !slices.Equal(before, after) {
		t.Errorf("state changed:\nbefore %q\nafter  %q", before, after)
	}
}

// serveTrigger listens on the store's trigger socket under dir and, on each
// connection, writes a verdict file marking the remote's .attest/h reviewed.
func (r *attestRig) serveTrigger(dir string) {
	r.t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, attestStoreID+".sock"))
	if err != nil {
		r.t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			cmd := exec.Command("git", "-C", r.bare, "show", "main:.attest/h")
			cmd.Env = r.childEnv()
			blob, err := cmd.Output()
			if err != nil {
				r.t.Errorf("reading the pushed entry: %v", err)
				continue
			}
			sum := sha256.Sum256(blob)
			r.writeVerdicts(attestStoreID, hex.EncodeToString(sum[:])+" reviewed")
		}
	}()
	r.t.Cleanup(func() {
		_ = l.Close()
		wg.Wait()
	})
}

type keygenRecord struct {
	Env   []string `json:"env"`
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func TestAttestEndToEnd(t *testing.T) {
	r := newAttestRig(t, trustOpts{root: "separate"})
	trigger, err := os.MkdirTemp("", "pt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(trigger) })
	r.serveTrigger(trigger)
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	res := r.runAttest("1234\n", []string{
		"PRIORS_TEST_NO_TTY=1",
		"PRIORS_TEST_WAIT=5s",
		"PRIORS_TEST_TRIGGER=" + trigger,
		"SSH_SK_PROVIDER=/evil/provider.so",
		"OPENSSL_CONF=/evil/openssl.cnf",
		"LIBPCSCLITE_DELEGATE=/evil/pcsc",
		"SSH_AUTH_SOCK=/evil/agent.sock",
		"SSH_SK_HELPER=/evil/helper",
	}, false, "attest", "-f", r.handle, "h")
	wantExit(t, res, 0)
	wantContains(t, "stdout", res.stdout, "in effect")

	blob := r.gitOut(r.bare, "show", "main:.attest/h")
	sum := sha256.Sum256([]byte(blob))
	tip := strings.TrimSpace(r.gitOut(r.bare, "rev-parse", "main"))
	wantContains(t, "result line", res.stdout, "h "+hex.EncodeToString(sum[:])+" "+tip)
	if subject := strings.TrimSpace(r.gitOut(r.bare, "log", "-1", "--format=%s", "main")); subject != "attest: h #1" {
		t.Errorf("remote tip subject = %q", subject)
	}

	logged, err := os.ReadFile(r.keygenLog())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(lines) != 1 {
		t.Fatalf("keygen ran %d times: %s", len(lines), logged)
	}
	var rec keygenRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	wantEnv := []string{"PATH=" + toolsDir, "HOME=" + u.HomeDir, "LANG=C", "SSH_SK_HELPER=" + pinnedTool("ssh-sk-helper")}
	gotEnv := slices.Sorted(slices.Values(rec.Env))
	if !slices.Equal(gotEnv, slices.Sorted(slices.Values(wantEnv))) {
		t.Errorf("keygen env = %q, want %q", rec.Env, wantEnv)
	}
	if !strings.HasPrefix(rec.Stdin, "priors-attest v1\nstore: "+attestStoreID+"\nname: h\n") || strings.Contains(rec.Stdin, "1234") {
		t.Errorf("keygen stdin = %q, want the payload", rec.Stdin)
	}
}

func TestPreloadConstructorDoesNotRun(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("LD_PRELOAD is the Linux loader's")
	}
	dir := t.TempDir()
	static := filepath.Join(dir, "priors-static")
	build := exec.Command("go", "build", "-tags", "priorstest", "-ldflags", strings.Join(buildLdflags, " "), "-o", static, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building static priors: %v\n%s", err, out)
	}

	src := filepath.Join(dir, "lib")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	const lib = `package main

import "C"
import "os"

func init() {
	if p := os.Getenv("MARK"); p != "" {
		_ = os.WriteFile(p, []byte("x"), 0o600)
	}
}

func main() {}
`
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(lib), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module preload\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	so := filepath.Join(dir, "preload.so")
	cbuild := exec.Command("go", "build", "-buildmode=c-shared", "-o", so, ".")
	cbuild.Dir = src
	cbuild.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := cbuild.CombinedOutput(); err != nil {
		t.Skipf("no c-shared build: %v\n%s", err, out)
	}

	sb := newSandbox(t, "personal")
	preload := func(mark string, bin string, args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(sb.childEnv(), "LD_PRELOAD="+so, "MARK="+mark)
		cmd.Dir = sb.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
	}
	exists := func(mark string) bool {
		_, err := os.Stat(mark)
		return err == nil
	}

	mark := filepath.Join(dir, "mark-static")
	preload(mark, static, "list")
	if exists(mark) {
		t.Error("the preloaded constructor ran inside the static priors")
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for i, control := range []struct {
		bin  string
		args []string
	}{{binPath, []string{"list"}}, {sh, []string{"-c", "true"}}} {
		mark := filepath.Join(dir, "mark-control-"+strconv.Itoa(i))
		preload(mark, control.bin, control.args...)
		if exists(mark) {
			t.Logf("control: %s", control.bin)
			return
		}
	}
	t.Skip("the preload does not run in a dynamic control binary, so the static result proves nothing")
}
