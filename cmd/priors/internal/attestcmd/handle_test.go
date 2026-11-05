package attestcmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

func writeHandleFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handle")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func openSSHPEM(body []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: body})
}

func TestHandleKey(t *testing.T) {
	a := attesttest.NewSKEd25519(t)
	b := attesttest.NewSKEd25519(t)
	skPath := writeHandleFile(t, a.Handle(5))

	_, plain, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	plainBlock, err := ssh.MarshalPrivateKey(plain, "")
	if err != nil {
		t.Fatal(err)
	}
	plainPath := writeHandleFile(t, pem.EncodeToMemory(plainBlock))

	twoKeys := openSSHPEM(append([]byte("openssh-key-v1\x00"), ssh.Marshal(struct {
		Cipher, KDF, KDFOpts string
		N                    uint32
		Pub                  []byte
		Priv                 []byte
	}{"none", "none", "", 2, a.PublicKey().Marshal(), nil})...))

	cases := []struct {
		name    string
		path    string
		allow   []ssh.PublicKey
		wantErr string
	}{
		{name: "allowlisted", path: skPath, allow: []ssh.PublicKey{b.PublicKey(), a.PublicKey()}},
		{name: "other key", path: skPath, allow: []ssh.PublicKey{b.PublicKey()}, wantErr: "not on the allowlist"},
		{name: "plain ed25519", path: plainPath, allow: []ssh.PublicKey{a.PublicKey()}, wantErr: "not a hardware (sk) key"},
		{name: "not pem", path: writeHandleFile(t, []byte("hello")), allow: []ssh.PublicKey{a.PublicKey()}, wantErr: "PEM"},
		{name: "two keys", path: writeHandleFile(t, twoKeys), allow: []ssh.PublicKey{a.PublicKey()}, wantErr: "1 key"},
		{name: "over 64 KiB", path: writeHandleFile(t, []byte(strings.Repeat("x", 64<<10+1))), allow: []ssh.PublicKey{a.PublicKey()}, wantErr: "64 KiB"},
		{name: "missing", path: filepath.Join(t.TempDir(), "nope"), allow: []ssh.PublicKey{a.PublicKey()}, wantErr: "no such file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := handleKey(tc.path, tc.allow)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Marshal()) != string(a.PublicKey().Marshal()) {
				t.Fatal("returned key is not the handle's key")
			}
		})
	}
}
