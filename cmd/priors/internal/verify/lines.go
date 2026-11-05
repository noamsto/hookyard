package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

// tipEntry is one `.attest/<name>` blob in the tip commit's tree.
type tipEntry struct {
	Name string
	Raw  []byte
}

// Lines computes the verdict lines (spec R8) from the tip's entries, the
// record and the current allowlist: the state per entry digest, and the
// record's highest sequence for each superseded digest. Only a tip entry that
// passes checks 4–6 against keys can read reviewed; non-tip lines are only
// revoked or superseded, and only while the signer the record holds is on keys.
func Lines(storeID string, tip []tipEntry, r *Record, keys []ssh.PublicKey) (map[string]attest.State, map[string]uint64) {
	states := map[string]attest.State{}
	seqs := map[string]uint64{}
	tipDigest := map[string]string{}
	tipState := map[string]attest.State{}

	for _, te := range tip {
		digest := entryDigest(te.Raw)
		tipDigest[te.Name] = digest
		e, err := attest.ParseEntry(te.Raw)
		if err != nil || attest.VerifyEntry(e, keys, storeID, te.Name, attest.OpAttest, attest.OpRevoke) != nil {
			continue
		}
		ns, ok := r.Names[te.Name]
		// A verified tip entry is always walked, so a missing name state
		// means no high to number above; no line keeps it short of reviewed.
		if !ok {
			continue
		}
		var state attest.State
		_, held := r.Superseded[digest]
		switch {
		case e.Sequence != ns.High || digest != ns.Digest || ns.Tied || held:
			state = attest.StateSuperseded
			seqs[digest] = ns.High
		case e.Op == attest.OpRevoke:
			state = attest.StateRevoked
		case r.Lapsed[digest]:
			state = attest.StateLapsed
		default:
			state = attest.StateReviewed
		}
		states[digest] = state
		tipState[te.Name] = state
	}

	for name, ns := range r.Names {
		if ns.Op != attest.OpRevoke || ns.Tied || !allowed(keys, ns.Signer) {
			continue
		}
		if d, ok := tipDigest[name]; ok && d == ns.Digest {
			continue
		}
		if _, ok := states[ns.Digest]; !ok {
			states[ns.Digest] = attest.StateRevoked
		}
	}

	for digest, sup := range r.Superseded {
		ns, ok := r.Names[sup.Name]
		if !ok || !allowed(keys, sup.Signer) {
			continue
		}
		if s, ok := tipState[sup.Name]; ok && s != attest.StateSuperseded {
			continue
		}
		if _, ok := states[digest]; ok {
			continue
		}
		states[digest] = attest.StateSuperseded
		seqs[digest] = ns.High
	}
	return states, seqs
}

func entryDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func allowed(keys []ssh.PublicKey, signer []byte) bool {
	return signer != nil && slices.ContainsFunc(keys, func(k ssh.PublicKey) bool {
		return bytes.Equal(k.Marshal(), signer)
	})
}
