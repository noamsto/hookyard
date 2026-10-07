package attesttest_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSKSignaturesVerify(t *testing.T) {
	for name, newKey := range map[string]func(testing.TB) attesttest.Key{
		"sk-ed25519": attesttest.NewSKEd25519,
		"sk-ecdsa":   attesttest.NewSKECDSA,
	} {
		t.Run(name, func(t *testing.T) {
			k := newKey(t)
			raw := attesttest.Signed(k, attesttest.Entry("s", "n", "n.md", sha, 1, "attest"), 0x05, attest.Namespace)
			e, err := attest.ParseEntry(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := attest.VerifyEntry(e, []ssh.PublicKey{k.PublicKey()}, "s", "n"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestSSHKeygenAgrees keeps the helper's SSHSIG framing honest against
// OpenSSH itself.
func TestSSHKeygenAgrees(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not on PATH")
	}
	for name, newKey := range map[string]func(testing.TB) attesttest.Key{
		"ed25519":    attesttest.NewEd25519,
		"sk-ed25519": attesttest.NewSKEd25519,
		"sk-ecdsa":   attesttest.NewSKECDSA,
	} {
		t.Run(name, func(t *testing.T) {
			k := newKey(t)
			msg := attesttest.Entry("s", "n", "n.md", sha, 1, "attest")
			sigFile := filepath.Join(t.TempDir(), "entry.sig")
			if err := os.WriteFile(sigFile, k.SignSSHSIG(msg, attest.Namespace, 0x05), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(keygen, "-Y", "check-novalidate", "-n", attest.Namespace, "-s", sigFile)
			cmd.Stdin = bytes.NewReader(msg)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("ssh-keygen rejected the signature: %v\n%s", err, out)
			}

			cmd = exec.Command(keygen, "-Y", "check-novalidate", "-n", attest.Namespace, "-s", sigFile)
			cmd.Stdin = bytes.NewReader(append(bytes.Clone(msg), 'x'))
			if err := cmd.Run(); err == nil {
				t.Fatal("ssh-keygen accepted the signature over a different message")
			}
		})
	}
}
