package attestcmd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

// summary is what a name's entry history says about its next sequence.
type summary struct {
	maxSeq  uint64
	digests map[string]bool
}

func entryDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// summarize takes the highest sequence over the well-formed versions for this
// store and name, signed or not: an unsigned high entry still burns its number.
func summarize(versions [][]byte, storeID, name string) summary {
	s := summary{digests: make(map[string]bool, len(versions))}
	for _, raw := range versions {
		s.digests[entryDigest(raw)] = true
		e, err := attest.ParseEntry(raw)
		if err != nil || e.Store != storeID || e.Name != name {
			continue
		}
		s.maxSeq = max(s.maxSeq, e.Sequence)
	}
	return s
}

// nextSequence is one above both the history and any verdict-recorded
// supersession of one of its entries.
func nextSequence(s summary, v *attest.Verdicts) (uint64, error) {
	top := s.maxSeq
	if v != nil {
		for digest, seq := range v.Superseded {
			if s.digests[digest] {
				top = max(top, seq)
			}
		}
	}
	if top >= math.MaxInt64 {
		return 0, errors.New("sequence space exhausted for this name: re-attest the fact under a new name")
	}
	return top + 1, nil
}

// revokeSource is the highest-sequence signed entry for the name, attest or
// revoke: the one a revoke covers.
func revokeSource(versions [][]byte, keys []ssh.PublicKey, storeID, name string) (attest.Entry, error) {
	var best attest.Entry
	found := false
	for _, raw := range versions {
		e, err := attest.ParseEntry(raw)
		if err != nil {
			continue
		}
		if attest.VerifyEntry(e, keys, storeID, name, attest.OpAttest, attest.OpRevoke) != nil {
			continue
		}
		if !found || e.Sequence > best.Sequence {
			best, found = e, true
		}
	}
	if !found {
		return attest.Entry{}, fmt.Errorf("no signed entry for %s to revoke", name)
	}
	return best, nil
}
