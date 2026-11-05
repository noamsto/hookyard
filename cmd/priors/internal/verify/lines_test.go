package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
)

const lnFactSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type lnEntry struct {
	raw    []byte
	digest string
}

func lnSigned(k attesttest.Key, store, name string, seq uint64, op attest.Op) lnEntry {
	raw := attesttest.Signed(k, attesttest.Entry(store, name, "facts/"+name+".md", lnFactSHA, seq, string(op)), 0x05, attest.Namespace)
	sum := sha256.Sum256(raw)
	return lnEntry{raw: raw, digest: hex.EncodeToString(sum[:])}
}

func lnRecord(names map[string]*NameState, sup map[string]Superseded, lapsed ...string) *Record {
	r := newRecord()
	maps.Copy(r.Names, names)
	maps.Copy(r.Superseded, sup)
	for _, d := range lapsed {
		r.Lapsed[d] = true
	}
	return r
}

func TestLines(t *testing.T) {
	keyA, keyB := attesttest.NewSKEd25519(t), attesttest.NewSKECDSA(t)
	sigA, sigB := keyA.PublicKey().Marshal(), keyB.PublicKey().Marshal()
	both := []ssh.PublicKey{keyA.PublicKey(), keyB.PublicKey()}
	onlyA := []ssh.PublicKey{keyA.PublicKey()}
	onlyB := []ssh.PublicKey{keyB.PublicKey()}

	a1 := lnSigned(keyA, "s", "n", 1, attest.OpAttest)
	a2 := lnSigned(keyA, "s", "n", 2, attest.OpAttest)
	b1 := lnSigned(keyB, "s", "n", 1, attest.OpAttest)
	b2 := lnSigned(keyB, "s", "n", 2, attest.OpAttest)
	r2 := lnSigned(keyA, "s", "n", 2, attest.OpRevoke)
	r3 := lnSigned(keyA, "s", "n", 3, attest.OpRevoke)
	otherStore := lnSigned(keyA, "other", "n", 2, attest.OpAttest)
	otherName := lnSigned(keyA, "s", "m", 2, attest.OpAttest)
	junk := lnEntry{raw: []byte("not an entry\n")}
	junkSum := sha256.Sum256(junk.raw)
	junk.digest = hex.EncodeToString(junkSum[:])

	high := func(seq uint64, e lnEntry, op attest.Op, signer []byte) map[string]*NameState {
		return map[string]*NameState{"n": {High: seq, Digest: e.digest, Op: op, Signer: signer}}
	}
	at := func(name string, e lnEntry) tipEntry { return tipEntry{Name: name, Raw: e.raw} }

	tests := []struct {
		name   string
		tip    []tipEntry
		rec    *Record
		keys   []ssh.PublicKey
		states map[string]attest.State
		seqs   map[string]uint64
	}{
		{
			name:   "tip attest at high, untied, not lapsed reads reviewed",
			tip:    []tipEntry{at("n", a2)},
			rec:    lnRecord(high(2, a2, attest.OpAttest, sigA), nil),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateReviewed},
		},
		{
			name:   "lapsed digest reads lapsed",
			tip:    []tipEntry{at("n", a2)},
			rec:    lnRecord(high(2, a2, attest.OpAttest, sigA), nil, a2.digest),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateLapsed},
		},
		{
			name:   "revoke at high reads revoked",
			tip:    []tipEntry{at("n", r2)},
			rec:    lnRecord(high(2, r2, attest.OpRevoke, sigA), nil),
			keys:   both,
			states: map[string]attest.State{r2.digest: attest.StateRevoked},
		},
		{
			name:   "revoke beats lapsed",
			tip:    []tipEntry{at("n", r2)},
			rec:    lnRecord(high(2, r2, attest.OpRevoke, sigA), nil, r2.digest),
			keys:   both,
			states: map[string]attest.State{r2.digest: attest.StateRevoked},
		},
		{
			name: "tip below high reads superseded with the high",
			tip:  []tipEntry{at("n", a1)},
			rec: lnRecord(high(5, a2, attest.OpAttest, sigA),
				map[string]Superseded{a1.digest: {Name: "n", Signer: sigA}}),
			keys:   both,
			states: map[string]attest.State{a1.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{a1.digest: 5},
		},
		{
			name: "revoke below high reads superseded, not revoked",
			tip:  []tipEntry{at("n", r2)},
			rec: lnRecord(high(3, a1, attest.OpAttest, nil),
				map[string]Superseded{r2.digest: {Name: "n", Signer: sigA}}),
			keys:   both,
			states: map[string]attest.State{r2.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{r2.digest: 3},
		},
		{
			name: "tie at high reads superseded",
			tip:  []tipEntry{at("n", a2)},
			rec: lnRecord(map[string]*NameState{"n": {High: 2, Digest: a2.digest, Op: attest.OpAttest, Tied: true, Signer: sigA}},
				map[string]Superseded{a2.digest: {Name: "n", Signer: sigA}, b2.digest: {Name: "n", Signer: sigB}}),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateSuperseded, b2.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{a2.digest: 2, b2.digest: 2},
		},
		{
			name: "tip digest held superseded reads superseded",
			tip:  []tipEntry{at("n", a2)},
			rec: lnRecord(high(2, a2, attest.OpAttest, sigA),
				map[string]Superseded{a2.digest: {Name: "n", Signer: sigA}}),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{a2.digest: 2},
		},
		{
			name:   "digest other than the record's at high reads superseded",
			tip:    []tipEntry{at("n", b2)},
			rec:    lnRecord(high(2, a2, attest.OpAttest, sigA), nil),
			keys:   both,
			states: map[string]attest.State{b2.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{b2.digest: 2},
		},
		{
			name: "tip with no record state gets no line",
			tip:  []tipEntry{at("n", a2)},
			rec:  lnRecord(nil, nil),
			keys: both,
		},
		{
			name: "tip signed by a key not on the allowlist gets no line",
			tip:  []tipEntry{at("n", b2)},
			rec:  lnRecord(high(2, b2, attest.OpAttest, sigB), nil),
			keys: onlyA,
		},
		{
			name: "tip entry for another store gets no line",
			tip:  []tipEntry{at("n", otherStore)},
			rec:  lnRecord(high(2, otherStore, attest.OpAttest, sigA), nil),
			keys: both,
		},
		{
			name: "tip entry naming another name gets no line",
			tip:  []tipEntry{at("n", otherName)},
			rec:  lnRecord(high(2, otherName, attest.OpAttest, sigA), nil),
			keys: both,
		},
		{
			name: "copy of a valid entry under a junk name gets no line",
			tip:  []tipEntry{at("junk", a2)},
			rec: lnRecord(map[string]*NameState{
				"n":    {High: 2, Digest: a2.digest, Op: attest.OpAttest, Signer: sigA},
				"junk": {High: 2, Digest: a2.digest, Op: attest.OpAttest},
			}, nil),
			keys: both,
		},
		{
			name: "copy under a junk name beside the original adds nothing",
			tip:  []tipEntry{at("n", a2), at("junk", a2)},
			rec: lnRecord(map[string]*NameState{
				"n":    {High: 2, Digest: a2.digest, Op: attest.OpAttest, Signer: sigA},
				"junk": {High: 2, Digest: a2.digest, Op: attest.OpAttest},
			}, nil),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateReviewed},
		},
		{
			name: "record-highest revoke under a junk tip keeps its revoked line",
			tip:  []tipEntry{at("n", junk)},
			rec: lnRecord(high(3, r3, attest.OpRevoke, sigA),
				map[string]Superseded{a1.digest: {Name: "n", Signer: sigA}}),
			keys:   both,
			states: map[string]attest.State{r3.digest: attest.StateRevoked, a1.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{a1.digest: 3},
		},
		{
			name: "record-highest revoke whose signer left the allowlist gets no line",
			tip:  []tipEntry{at("n", junk)},
			rec: lnRecord(high(3, r3, attest.OpRevoke, sigA),
				map[string]Superseded{a1.digest: {Name: "n", Signer: sigA}}),
			keys: onlyB,
		},
		{
			name: "record-highest revoke that is tied gets no revoked line",
			tip:  []tipEntry{at("n", junk)},
			rec: lnRecord(map[string]*NameState{"n": {High: 3, Digest: r3.digest, Op: attest.OpRevoke, Tied: true, Signer: sigA}},
				map[string]Superseded{r3.digest: {Name: "n", Signer: sigA}}),
			keys:   both,
			states: map[string]attest.State{r3.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{r3.digest: 3},
		},
		{
			name: "record-highest revoke whose tip copy fails the allowlist gets no non-tip line",
			tip:  []tipEntry{at("n", r3)},
			rec:  lnRecord(high(3, r3, attest.OpRevoke, sigA), nil),
			keys: onlyB,
		},
		{
			name: "tip revoked drops the name's superseded lines",
			tip:  []tipEntry{at("n", r3)},
			rec: lnRecord(high(3, r3, attest.OpRevoke, sigA),
				map[string]Superseded{a1.digest: {Name: "n", Signer: sigA}}),
			keys:   both,
			states: map[string]attest.State{r3.digest: attest.StateRevoked},
		},
		{
			name: "tip lapsed drops the name's superseded lines",
			tip:  []tipEntry{at("n", a2)},
			rec: lnRecord(high(2, a2, attest.OpAttest, sigA),
				map[string]Superseded{a1.digest: {Name: "n", Signer: sigA}}, a2.digest),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateLapsed},
		},
		{
			name: "absent tip lists every allowlisted superseded digest of the name",
			rec: lnRecord(high(4, a2, attest.OpAttest, sigA), map[string]Superseded{
				a1.digest: {Name: "n", Signer: sigA},
				b1.digest: {Name: "n", Signer: sigB},
				r2.digest: {Name: "other", Signer: sigA},
			}),
			keys:   both,
			states: map[string]attest.State{a1.digest: attest.StateSuperseded, b1.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{a1.digest: 4, b1.digest: 4},
		},
		{
			name: "superseded tip lists every allowlisted superseded digest of the name",
			tip:  []tipEntry{at("n", a1)},
			rec: lnRecord(high(4, a2, attest.OpAttest, sigA), map[string]Superseded{
				a1.digest: {Name: "n", Signer: sigA},
				b1.digest: {Name: "n", Signer: sigB},
			}),
			keys:   both,
			states: map[string]attest.State{a1.digest: attest.StateSuperseded, b1.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{a1.digest: 4, b1.digest: 4},
		},
		{
			name: "reviewed tip drops the name's superseded lines",
			tip:  []tipEntry{at("n", a2)},
			rec: lnRecord(high(2, a2, attest.OpAttest, sigA), map[string]Superseded{
				a1.digest: {Name: "n", Signer: sigA},
				b1.digest: {Name: "n", Signer: sigB},
			}),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateReviewed},
		},
		{
			name: "superseded digest signed by a removed key gets no line",
			rec: lnRecord(high(4, a2, attest.OpAttest, sigA), map[string]Superseded{
				a1.digest: {Name: "n", Signer: sigA},
				b1.digest: {Name: "n", Signer: sigB},
			}),
			keys:   onlyA,
			states: map[string]attest.State{a1.digest: attest.StateSuperseded},
			seqs:   map[string]uint64{a1.digest: 4},
		},
		{
			name: "ordinary re-attest history yields only the tip line",
			tip:  []tipEntry{at("n", a2)},
			rec: lnRecord(high(2, a2, attest.OpAttest, sigA),
				map[string]Superseded{a1.digest: {Name: "n", Signer: sigA}}),
			keys:   both,
			states: map[string]attest.State{a2.digest: attest.StateReviewed},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			states, seqs := Lines("s", tc.tip, tc.rec, tc.keys)
			if !maps.Equal(states, tc.states) {
				t.Errorf("states = %v, want %v", states, tc.states)
			}
			if !maps.Equal(seqs, tc.seqs) {
				t.Errorf("superseded = %v, want %v", seqs, tc.seqs)
			}
			for d, s := range states {
				if _, ok := seqs[d]; ok != (s == attest.StateSuperseded) {
					t.Errorf("digest %s: state %s, sequence present %v", d, s, ok)
				}
			}
		})
	}
}
