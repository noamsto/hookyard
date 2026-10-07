package attest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

const (
	testStore = "store-1"
	testName  = "some-fact"
	testPath  = "facts/some-fact.md"
	testSHA   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func canonical(t *testing.T) []byte {
	t.Helper()
	k := attesttest.NewSKEd25519(t)
	return attesttest.Signed(k, attesttest.Entry(testStore, testName, testPath, testSHA, 7, "attest"), 0x05, Namespace)
}

func TestParseEntryCanonical(t *testing.T) {
	raw := canonical(t)
	e, err := ParseEntry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Store != testStore || e.Name != testName || e.Path != testPath || e.SHA256 != testSHA || e.Sequence != 7 || e.Op != OpAttest {
		t.Fatalf("parsed %+v", e)
	}
	lines := attesttest.Entry(testStore, testName, testPath, testSHA, 7, "attest")
	if !bytes.Equal(e.Signed, lines) {
		t.Fatalf("Signed = %q, want %q", e.Signed, lines)
	}
	if !bytes.HasPrefix(e.Signature, []byte("SSHSIG")) {
		t.Fatalf("Signature not decoded: %q", e.Signature[:min(len(e.Signature), 16)])
	}
}

func TestParseEntryMaxSequence(t *testing.T) {
	k := attesttest.NewSKEd25519(t)
	raw := attesttest.Signed(k, attesttest.Entry(testStore, testName, testPath, testSHA, 1<<63-1, "revoke"), 0x05, Namespace)
	e, err := ParseEntry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Sequence != 1<<63-1 || e.Op != OpRevoke {
		t.Fatalf("parsed %+v", e)
	}
}

func TestParseEntryRejects(t *testing.T) {
	good := string(canonical(t))
	header, armour, _ := strings.Cut(good, "-----BEGIN")
	armour = "-----BEGIN" + armour
	lines := strings.SplitAfter(header, "\n")[:7]
	withLine := func(i int, line string) string {
		l := append([]string(nil), lines...)
		l[i] = line
		return strings.Join(l, "") + armour
	}

	cases := map[string]string{
		"wrong magic":        withLine(0, "priors-attest v2\n"),
		"keys out of order":  strings.Join([]string{lines[0], lines[2], lines[1], lines[3], lines[4], lines[5], lines[6]}, "") + armour,
		"missing line":       strings.Join(append(append([]string(nil), lines[:5]...), lines[6]), "") + armour,
		"extra line":         strings.Join(lines, "") + "extra: x\n" + armour,
		"crlf":               strings.ReplaceAll(good, "\n", "\r\n"),
		"cr in value":        withLine(2, "name: a\rb\n"),
		"no final newline":   strings.TrimSuffix(good, "\n"),
		"sequence zero":      withLine(5, "sequence: 0\n"),
		"leading zero":       withLine(5, "sequence: 07\n"),
		"sequence 2^63":      withLine(5, "sequence: 9223372036854775808\n"),
		"non-decimal":        withLine(5, "sequence: 0x7\n"),
		"signed sequence":    withLine(5, "sequence: +7\n"),
		"empty sequence":     withLine(5, "sequence: \n"),
		"op other":           withLine(6, "op: approve\n"),
		"sha256 uppercase":   withLine(4, "sha256: "+strings.ToUpper(testSHA)+"\n"),
		"sha256 short":       withLine(4, "sha256: "+testSHA[:63]+"\n"),
		"path absolute":      withLine(3, "path: /etc/passwd\n"),
		"path dotdot":        withLine(3, "path: facts/../../x.md\n"),
		"path empty seg":     withLine(3, "path: facts//x.md\n"),
		"path empty":         withLine(3, "path: \n"),
		"empty name":         withLine(2, "name: \n"),
		"empty store":        withLine(1, "store: \n"),
		"no space after key": withLine(2, "name:"+testName+"\n"),
		"too large":          strings.Join(lines, "") + armour[:len("-----BEGIN SSH SIGNATURE-----\n")] + strings.Repeat("AAAA\n", MaxEntryBytes/5) + "-----END SSH SIGNATURE-----\n",
		"trailing bytes":     good + "x\n",
		"trailing newline":   good + "\n",
		"no armour":          strings.Join(lines, ""),
		"bad begin":          strings.Join(lines, "") + strings.Replace(armour, "BEGIN SSH SIGNATURE", "BEGIN PGP SIGNATURE", 1),
		"bad end":            strings.Join(lines, "") + strings.Replace(armour, "END SSH SIGNATURE", "END SSH SIG", 1),
		"bad base64":         strings.Join(lines, "") + strings.Replace(armour, "-----\n", "-----\n!!!!\n", 1),
		"empty armour":       strings.Join(lines, "") + "-----BEGIN SSH SIGNATURE-----\n-----END SSH SIGNATURE-----\n",
		"blank armour line":  strings.Join(lines, "") + strings.Replace(armour, "-----\n", "-----\n\n", 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseEntry([]byte(raw)); err == nil {
				t.Fatalf("ParseEntry accepted %q", raw)
			}
		})
	}
}

func TestBind(t *testing.T) {
	fact := []byte("---\nname: some-fact\n---\nbody\n")
	k := attesttest.NewSKEd25519(t)
	raw := attesttest.Signed(k, attesttest.Entry(testStore, testName, testPath, hexSHA256(fact), 1, "attest"), 0x05, Namespace)
	e, err := ParseEntry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := Bind(e, testPath, fact); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := Bind(e, "facts/other.md", fact); checkOf(err) != 3 {
		t.Fatalf("wrong path: %v", err)
	}
	if err := Bind(e, testPath, append(fact, 'x')); checkOf(err) != 3 {
		t.Fatalf("wrong sha256: %v", err)
	}
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
