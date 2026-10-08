package attest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
)

// MaxVerdictBytes bounds a verdict file, which every claiming fact's index
// check reads.
const MaxVerdictBytes = 1 << 20

// verdictMaxAge is how long a verdict file holds after its last fetch.
const verdictMaxAge = 24 * time.Hour

// State is an entry digest's state in a verdict file. Only StateReviewed
// makes a fact reviewed.
type State string

const (
	StateReviewed   State = "reviewed"
	StateRevoked    State = "revoked"
	StateLapsed     State = "lapsed"
	StateSuperseded State = "superseded"
)

// Verdicts is a parsed verdict file. States maps each listed entry sha256 to
// its state; Superseded holds, for each superseded digest, the record's
// highest sequence for that name.
type Verdicts struct {
	Store, Trust, Tip, Status string
	Fetched                   time.Time
	States                    map[string]State
	Superseded                map[string]uint64
}

const verdictMagic = "priors-verdicts v1"

var (
	verdictStoreRE = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	tipRE          = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	verdictStatus  = []string{"ok", "behind", "oversize", "stale-remote"}
	plainStates    = []State{StateReviewed, StateRevoked, StateLapsed}
)

// verdictFields is the header after the magic line, in its fixed order.
var verdictFields = [...]struct {
	key string
	set func(v *Verdicts, val string) bool
}{
	{"store", func(v *Verdicts, val string) bool { v.Store = val; return verdictStoreRE.MatchString(val) }},
	{"trust", func(v *Verdicts, val string) bool { v.Trust = val; return sha256RE.MatchString(val) }},
	{"tip", func(v *Verdicts, val string) bool { v.Tip = val; return tipRE.MatchString(val) }},
	{"fetched", func(v *Verdicts, val string) bool {
		secs, ok := decimal(val)
		v.Fetched = time.Unix(int64(secs), 0)
		return ok
	}},
	{"status", func(v *Verdicts, val string) bool { v.Status = val; return slices.Contains(verdictStatus, val) }},
}

// ParseVerdicts parses raw by the strict verdict-file grammar. Any line off
// the grammar fails the whole file rather than being skipped, so no parse
// ambiguity can read reviewed.
func ParseVerdicts(raw []byte) (Verdicts, error) {
	v := Verdicts{States: map[string]State{}, Superseded: map[string]uint64{}}
	for n := 1; len(raw) > 0 || n <= len(verdictFields)+1; n++ {
		line, rest, ok := bytes.Cut(raw, []byte("\n"))
		if !ok || !v.parseLine(n, string(line)) {
			return Verdicts{}, fmt.Errorf("verdict file malformed: line %d", n)
		}
		raw = rest
	}
	return v, nil
}

// parseLine parses line n (1-based) into v.
func (v *Verdicts) parseLine(n int, line string) bool {
	if n == 1 {
		return line == verdictMagic
	}
	if n <= len(verdictFields)+1 {
		h := verdictFields[n-2]
		val, ok := strings.CutPrefix(line, h.key+": ")
		return ok && h.set(v, val)
	}
	fields := strings.Split(line, " ")
	if len(fields) < 2 || !sha256RE.MatchString(fields[0]) {
		return false
	}
	digest, state := fields[0], State(fields[1])
	if _, dup := v.States[digest]; dup {
		return false
	}
	switch {
	case len(fields) == 2 && slices.Contains(plainStates, state):
	case len(fields) == 3 && state == StateSuperseded:
		seq, ok := decimal(fields[2])
		if !ok || seq == 0 {
			return false
		}
		v.Superseded[digest] = seq
	default:
		return false
	}
	v.States[digest] = state
	return true
}

// decimal parses s as a decimal in 0..2^63-1 with no leading zeros.
func decimal(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 63)
	return n, err == nil && (s[0] != '0' || s == "0")
}

// ReadVerdicts is check 1's verdict-file part: the file at path passes the
// ownership walk with owner as its and its store dir's owner, is at most
// MaxVerdictBytes, parses strictly, names storeID and trustDigest, and was
// fetched less than 24 h before now.
func ReadVerdicts(fsys config.StatFS, path, storeID, trustDigest string, owner uint32, now time.Time) (Verdicts, error) {
	resolved, err := config.CheckOwnedPath(fsys, path, &owner)
	if errors.Is(err, fs.ErrNotExist) {
		return Verdicts{}, errors.New("no verdict file")
	}
	if err != nil {
		return Verdicts{}, fmt.Errorf("verdict file: %w", err)
	}
	fi, err := fsys.Lstat(resolved)
	if err != nil {
		return Verdicts{}, fmt.Errorf("verdict file: %w", err)
	}
	if fi.Size() > MaxVerdictBytes {
		return Verdicts{}, errors.New("verdict file over 1 MiB")
	}
	raw, err := fsys.ReadFile(resolved)
	if err != nil {
		return Verdicts{}, fmt.Errorf("verdict file: %w", err)
	}
	// The file can grow between the Lstat and the read.
	if len(raw) > MaxVerdictBytes {
		return Verdicts{}, errors.New("verdict file over 1 MiB")
	}
	v, err := ParseVerdicts(raw)
	if err != nil {
		return Verdicts{}, err
	}
	if v.Store != storeID {
		return Verdicts{}, fmt.Errorf("verdict file names store %s", v.Store)
	}
	if v.Trust != trustDigest {
		return Verdicts{}, errors.New("verdict file is for another trust file")
	}
	switch age := now.Sub(v.Fetched); {
	case age < 0:
		return Verdicts{}, errors.New("verdict file fetched in the future")
	case age >= verdictMaxAge:
		return Verdicts{}, fmt.Errorf("verdicts stale: fetched %s ago", age.Truncate(time.Second))
	}
	return v, nil
}
