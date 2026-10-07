// Package attest parses and verifies attest entries: a human's FIDO2 SSHSIG
// over a fact file's path and digest, kept at `.attest/<name>` in a store.
package attest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Namespace is the SSHSIG namespace of every attest signature, one no git or
// file signature uses.
const Namespace = "priors-attest@hookyard"

// MaxEntryBytes bounds an entry so a hostile checkout cannot make the verdict
// job buffer or hash an arbitrarily large file.
const MaxEntryBytes = 4 << 10

type Op string

const (
	OpAttest Op = "attest"
	OpRevoke Op = "revoke"
)

// Entry is one parsed attest entry. Signed is the seven header lines exactly,
// each ending in "\n"; Signature is the SSHSIG blob inside the armour.
type Entry struct {
	Store, Name, Path, SHA256 string
	Sequence                  uint64
	Op                        Op
	Signed                    []byte
	Signature                 []byte
}

// CheckError is a failed verdict check, numbered as in the design.
type CheckError struct {
	Check  int
	Reason string
}

func (e *CheckError) Error() string { return fmt.Sprintf("check %d: %s", e.Check, e.Reason) }

const (
	magicLine   = "priors-attest v1"
	beginArmour = "-----BEGIN SSH SIGNATURE-----"
	endArmour   = "-----END SSH SIGNATURE-----"
	headerLines = 7
)

var (
	headerKeys = [headerLines - 1]string{"store", "name", "path", "sha256", "sequence", "op"}
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseEntry checks raw's shape only; VerifyEntry and Bind check its meaning.
func ParseEntry(raw []byte) (Entry, error) {
	if len(raw) > MaxEntryBytes {
		return Entry{}, fmt.Errorf("entry is %d bytes, over %d", len(raw), MaxEntryBytes)
	}
	if bytes.IndexByte(raw, '\r') >= 0 {
		return Entry{}, errors.New("entry contains a carriage return")
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		return Entry{}, errors.New("entry does not end in a newline")
	}
	lines := strings.Split(string(raw[:len(raw)-1]), "\n")
	// header, BEGIN, at least one body line, END
	if len(lines) < headerLines+3 {
		return Entry{}, fmt.Errorf("entry has %d lines, too few", len(lines))
	}
	if lines[0] != magicLine {
		return Entry{}, fmt.Errorf("first line is %q, want %q", lines[0], magicLine)
	}

	var vals [headerLines - 1]string
	for i, key := range headerKeys {
		v, ok := strings.CutPrefix(lines[i+1], key+": ")
		if !ok {
			return Entry{}, fmt.Errorf("line %d is not %q", i+2, key+": ...")
		}
		vals[i] = v
	}
	e := Entry{Store: vals[0], Name: vals[1], Path: vals[2], SHA256: vals[3], Op: Op(vals[5])}
	if e.Store == "" {
		return Entry{}, errors.New("store is empty")
	}
	if e.Name == "" {
		return Entry{}, errors.New("name is empty")
	}
	if !filepath.IsLocal(e.Path) || path.Clean(e.Path) != e.Path {
		return Entry{}, fmt.Errorf("path %q is not a clean relative slash path", e.Path)
	}
	if !sha256RE.MatchString(e.SHA256) {
		return Entry{}, fmt.Errorf("sha256 %q is not 64 lowercase hex digits", e.SHA256)
	}
	seq, err := strconv.ParseUint(vals[4], 10, 63)
	if err != nil || seq == 0 || vals[4][0] == '0' {
		return Entry{}, fmt.Errorf("sequence %q is not a decimal in 1..2^63-1 without leading zeros", vals[4])
	}
	e.Sequence = seq
	if e.Op != OpAttest && e.Op != OpRevoke {
		return Entry{}, fmt.Errorf("op %q is not attest or revoke", e.Op)
	}

	if lines[headerLines] != beginArmour {
		return Entry{}, fmt.Errorf("line %d is %q, want %q", headerLines+1, lines[headerLines], beginArmour)
	}
	if last := lines[len(lines)-1]; last != endArmour {
		return Entry{}, fmt.Errorf("last line is %q, want %q", last, endArmour)
	}
	body := lines[headerLines+1 : len(lines)-1]
	if slices.Contains(body, "") {
		return Entry{}, errors.New("armour has an empty line")
	}
	e.Signature, err = base64.StdEncoding.Strict().DecodeString(strings.Join(body, ""))
	if err != nil {
		return Entry{}, fmt.Errorf("armour: %w", err)
	}

	e.Signed = raw[:len(strings.Join(lines[:headerLines], "\n"))+1]
	return e, nil
}

// Bind is check 3's binding of an attest entry to the tree: the entry names
// the fact's path and the sha256 of its bytes.
func Bind(e Entry, rel string, factRaw []byte) error {
	if e.Path != rel {
		return &CheckError{Check: 3, Reason: fmt.Sprintf("entry path %q is not %q", e.Path, rel)}
	}
	sum := sha256.Sum256(factRaw)
	if got := hex.EncodeToString(sum[:]); e.SHA256 != got {
		return &CheckError{Check: 3, Reason: fmt.Sprintf("entry sha256 %s is not the fact's %s", e.SHA256, got)}
	}
	return nil
}
