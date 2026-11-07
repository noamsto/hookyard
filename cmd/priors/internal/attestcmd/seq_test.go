package attestcmd

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

const (
	seqStore = "store-1"
	seqName  = "fact-a"
	seqPath  = "facts/fact-a.md"
)

func seqHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func seqSigned(k attesttest.Key, store, name string, seq uint64, op string) []byte {
	e := attesttest.Entry(store, name, seqPath, seqHex([]byte("fact")), seq, op)
	return attesttest.Signed(k, e, 0x05, attest.Namespace)
}

// seqJunk is well-formed but carries no valid signature, as an agent can push.
func seqJunk(store, name string, seq uint64) []byte {
	return append(attesttest.Entry(store, name, seqPath, seqHex([]byte("fact")), seq, "attest"),
		"-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"...)
}

func TestSummarize(t *testing.T) {
	k := attesttest.NewSKEd25519(t)
	a3 := seqSigned(k, seqStore, seqName, 3, "attest")
	a7 := seqJunk(seqStore, seqName, 7)
	otherStore := seqSigned(k, "other", seqName, 50, "attest")
	otherName := seqSigned(k, seqStore, "fact-b", 60, "attest")
	huge := seqJunk(seqStore, seqName, 1<<63)
	malformed := []byte("not an entry\n")

	t.Run("max over well-formed versions, digests of every version", func(t *testing.T) {
		versions := [][]byte{a3, a7, otherStore, otherName, huge, malformed}
		got := summarize(versions, seqStore, seqName)
		if got.maxSeq != 7 {
			t.Fatalf("maxSeq = %d, want 7", got.maxSeq)
		}
		for _, v := range versions {
			if !got.digests[seqHex(v)] {
				t.Errorf("digest of %q missing", v[:min(12, len(v))])
			}
		}
		if len(got.digests) != len(versions) {
			t.Errorf("%d digests, want %d", len(got.digests), len(versions))
		}
	})
	t.Run("ignored entries give zero", func(t *testing.T) {
		got := summarize([][]byte{otherStore, otherName, huge, malformed}, seqStore, seqName)
		if got.maxSeq != 0 {
			t.Fatalf("maxSeq = %d, want 0", got.maxSeq)
		}
	})
	t.Run("no versions", func(t *testing.T) {
		got := summarize(nil, seqStore, seqName)
		if got.maxSeq != 0 || len(got.digests) != 0 {
			t.Fatalf("got %+v, want zero summary", got)
		}
	})
}

func TestNextSequence(t *testing.T) {
	d1, d2 := "d1", "d2"
	sum := func(max uint64) summary { return summary{maxSeq: max, digests: map[string]bool{d1: true}} }

	tests := []struct {
		name string
		s    summary
		v    *attest.Verdicts
		want uint64
	}{
		{"history only", sum(10), nil, 11},
		{"nil verdicts", sum(0), nil, 1},
		{"matching superseded line wins", sum(3), &attest.Verdicts{Superseded: map[string]uint64{d1: 10}}, 11},
		{"non-matching superseded ignored", sum(3), &attest.Verdicts{Superseded: map[string]uint64{d2: 10}}, 4},
		{"history above superseded", sum(12), &attest.Verdicts{Superseded: map[string]uint64{d1: 10}}, 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextSequence(tt.s, tt.v)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}

	t.Run("exhausted", func(t *testing.T) {
		_, err := nextSequence(sum(1<<63-1), nil)
		if err == nil || !strings.Contains(err.Error(), "sequence space exhausted") {
			t.Fatalf("err = %v, want sequence space exhausted", err)
		}
	})
	t.Run("exhausted through superseded line", func(t *testing.T) {
		_, err := nextSequence(sum(1), &attest.Verdicts{Superseded: map[string]uint64{d1: 1<<63 - 1}})
		if err == nil || !strings.Contains(err.Error(), "sequence space exhausted") {
			t.Fatalf("err = %v, want sequence space exhausted", err)
		}
	})
	t.Run("last usable", func(t *testing.T) {
		got, err := nextSequence(sum(1<<63-2), nil)
		if err != nil || got != 1<<63-1 {
			t.Fatalf("got %d, %v", got, err)
		}
	})
}

func TestRevokeSource(t *testing.T) {
	k := attesttest.NewSKEd25519(t)
	stranger := attesttest.NewSKEd25519(t)
	keys := []ssh.PublicKey{k.PublicKey()}

	t.Run("highest signed entry of either op, skipping higher junk", func(t *testing.T) {
		versions := [][]byte{
			seqSigned(k, seqStore, seqName, 2, "attest"),
			seqSigned(k, seqStore, seqName, 5, "revoke"),
			seqJunk(seqStore, seqName, 9),
			seqSigned(stranger, seqStore, seqName, 8, "attest"),
			seqSigned(k, "other", seqName, 7, "attest"),
		}
		got, err := revokeSource(versions, keys, seqStore, seqName)
		if err != nil {
			t.Fatal(err)
		}
		if got.Sequence != 5 || got.Op != attest.OpRevoke || got.Path != seqPath || got.SHA256 != seqHex([]byte("fact")) {
			t.Fatalf("got %+v, want the sequence 5 revoke", got)
		}
	})
	t.Run("attest is the highest", func(t *testing.T) {
		versions := [][]byte{
			seqSigned(k, seqStore, seqName, 4, "revoke"),
			seqSigned(k, seqStore, seqName, 6, "attest"),
		}
		got, err := revokeSource(versions, keys, seqStore, seqName)
		if err != nil {
			t.Fatal(err)
		}
		if got.Sequence != 6 {
			t.Fatalf("sequence = %d, want 6", got.Sequence)
		}
	})
	t.Run("none signed", func(t *testing.T) {
		versions := [][]byte{seqJunk(seqStore, seqName, 3), seqSigned(stranger, seqStore, seqName, 4, "attest")}
		_, err := revokeSource(versions, keys, seqStore, seqName)
		if err == nil || !strings.Contains(err.Error(), "no signed entry for "+seqName+" to revoke") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no versions", func(t *testing.T) {
		if _, err := revokeSource(nil, keys, seqStore, seqName); err == nil {
			t.Fatal("want an error")
		}
	})
}
