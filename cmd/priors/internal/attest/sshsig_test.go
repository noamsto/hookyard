package attest

import (
	"errors"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

func checkOf(err error) int {
	var ce *CheckError
	if !errors.As(err, &ce) {
		return 0
	}
	return ce.Check
}

func entry(t *testing.T, k attesttest.Key, op string, flags byte, namespace string) Entry {
	t.Helper()
	raw := attesttest.Signed(k, attesttest.Entry(testStore, testName, testPath, testSHA, 3, op), flags, namespace)
	e, err := ParseEntry(raw)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func allow(keys ...attesttest.Key) []ssh.PublicKey {
	out := make([]ssh.PublicKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.PublicKey())
	}
	return out
}

// reframe rewrites one field of e's SSHSIG blob without re-signing.
func reframe(t *testing.T, e Entry, edit func(*sigBlob)) Entry {
	t.Helper()
	b, err := parseBlob(e.Signature)
	if err != nil {
		t.Fatal(err)
	}
	edit(&b)
	e.Signature = b.marshal()
	return e
}

func TestVerifyEntryPasses(t *testing.T) {
	for name, newKey := range map[string]func(testing.TB) attesttest.Key{
		"sk-ed25519": attesttest.NewSKEd25519,
		"sk-ecdsa":   attesttest.NewSKECDSA,
	} {
		t.Run(name, func(t *testing.T) {
			k := newKey(t)
			other := attesttest.NewSKEd25519(t)
			e := entry(t, k, "attest", 0x05, Namespace)
			if err := VerifyEntry(e, allow(other, k), testStore, testName); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifyEntrySHA256(t *testing.T) {
	k := attesttest.NewSKEd25519(t)
	lines := attesttest.Entry(testStore, testName, testPath, testSHA, 3, "attest")
	e, err := ParseEntry(append(lines, k.SignSSHSIGHash(lines, Namespace, "sha256", 0x05)...))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEntry(e, allow(k), testStore, testName); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyEntryRevoke(t *testing.T) {
	k := attesttest.NewSKECDSA(t)
	e := entry(t, k, "revoke", 0x05, Namespace)
	if err := VerifyEntry(e, allow(k), testStore, testName); checkOf(err) != 4 {
		t.Fatalf("revoke with default ops: %v", err)
	}
	if err := VerifyEntry(e, allow(k), testStore, testName, OpAttest, OpRevoke); err != nil {
		t.Fatalf("revoke allowed: %v", err)
	}
	a := entry(t, k, "attest", 0x05, Namespace)
	if err := VerifyEntry(a, allow(k), testStore, testName, OpRevoke); checkOf(err) != 4 {
		t.Fatalf("attest when only revoke allowed: %v", err)
	}
}

func TestVerifyEntryFails(t *testing.T) {
	sk := attesttest.NewSKEd25519(t)
	plain := attesttest.NewEd25519(t)
	good := entry(t, sk, "attest", 0x05, Namespace)

	tampered := good
	tampered.Signed = append([]byte(nil), good.Signed...)
	tampered.Signed[len(tampered.Signed)-2] ^= 1

	corrupt := good
	corrupt.Signature = append([]byte(nil), good.Signature...)
	// The last five bytes are the flags and counter; the byte before them
	// ends the ed25519 signature.
	corrupt.Signature[len(corrupt.Signature)-6] ^= 1

	cases := []struct {
		name  string
		e     Entry
		allow []ssh.PublicKey
		store string
		file  string
		check int
	}{
		{"not allowlisted", good, allow(attesttest.NewSKEd25519(t)), testStore, testName, 4},
		{"empty allowlist", good, nil, testStore, testName, 4},
		{"namespace git", entry(t, sk, "attest", 0x05, "git"), allow(sk), testStore, testName, 4},
		{"namespace file", entry(t, sk, "attest", 0x05, "file"), allow(sk), testStore, testName, 4},
		{"tampered payload", tampered, allow(sk), testStore, testName, 4},
		{"corrupted signature", corrupt, allow(sk), testStore, testName, 4},
		{"hash sha384", reframe(t, good, func(b *sigBlob) { b.HashAlgorithm = "sha384" }), allow(sk), testStore, testName, 4},
		{"version 2", reframe(t, good, func(b *sigBlob) { b.Version = 2 }), allow(sk), testStore, testName, 4},
		{"namespace reframed", reframe(t, good, func(b *sigBlob) { b.Namespace = "git" }), allow(sk), testStore, testName, 4},
		{"bad magic", func() Entry { e := good; e.Signature = append([]byte("SSHSIH"), good.Signature[6:]...); return e }(), allow(sk), testStore, testName, 4},
		{"truncated blob", func() Entry { e := good; e.Signature = good.Signature[:len(good.Signature)-1]; return e }(), allow(sk), testStore, testName, 4},
		{"plain ed25519", entry(t, plain, "attest", 0x05, Namespace), allow(plain), testStore, testName, 5},
		{"flags UP only", entry(t, sk, "attest", 0x01, Namespace), allow(sk), testStore, testName, 5},
		{"flags UV only", entry(t, sk, "attest", 0x04, Namespace), allow(sk), testStore, testName, 5},
		{"flags none", entry(t, sk, "attest", 0x00, Namespace), allow(sk), testStore, testName, 5},
		{"wrong store", good, allow(sk), "store-2", testName, 6},
		{"wrong file name", good, allow(sk), testStore, "other-fact", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyEntry(tc.e, tc.allow, tc.store, tc.file)
			if got := checkOf(err); got != tc.check {
				t.Fatalf("VerifyEntry = %v (check %d), want check %d", err, got, tc.check)
			}
		})
	}
}

func TestCheckErrorString(t *testing.T) {
	if got := (&CheckError{Check: 5, Reason: "no user verification"}).Error(); got != "check 5: no user verification" {
		t.Fatalf("Error() = %q", got)
	}
}
