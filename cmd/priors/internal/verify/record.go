package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/noamsto/hookyard/cmd/priors/internal/atomicfile"
	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

const recordVersion = 1

// Record is the per-name record (spec R6): the job's monotonic memory of
// every well-formed entry it has walked. It only grows, so a mirror reset or
// a remote rewrite cannot make a superseded, tied or lapsed entry pass again.
type Record struct {
	Version    int                   `json:"version"`
	Tip        string                `json:"tip,omitempty"`
	Fetched    int64                 `json:"fetched,omitempty"`
	Names      map[string]*NameState `json:"names"`
	Superseded map[string]Superseded `json:"superseded"`
	Lapsed     map[string]bool       `json:"lapsed"`
	Entries    map[string]*Tracked   `json:"entries"`
}

type NameState struct {
	High   uint64    `json:"high"`
	Digest string    `json:"digest"`
	Op     attest.Op `json:"op"`
	Tied   bool      `json:"tied"`
	Signer []byte    `json:"signer,omitempty"`
}

type Superseded struct {
	Name   string `json:"name"`
	Signer []byte `json:"signer"`
}

type Tracked struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	SHA256 string   `json:"sha256"`
	Intros []string `json:"intros"`
}

func newRecord() *Record {
	return &Record{
		Version:    recordVersion,
		Names:      map[string]*NameState{},
		Superseded: map[string]Superseded{},
		Lapsed:     map[string]bool{},
		Entries:    map[string]*Tracked{},
	}
}

// Observe applies check 7's update for one well-formed entry e of name, with
// digest digest. signer is the marshalled key when e is signed by its own
// embedded key with check 6's binding, else nil; only signed digests are
// recorded superseded, since only they could ever pass a verdict line.
// Re-observing an entry changes nothing, so a re-walk is idempotent.
func (r *Record) Observe(name, digest string, e attest.Entry, signer []byte) {
	ns, ok := r.Names[name]
	switch {
	case !ok || e.Sequence > ns.High:
		if ok {
			r.supersede(ns.Digest, name, ns.Signer)
		}
		r.Names[name] = &NameState{High: e.Sequence, Digest: digest, Op: e.Op, Signer: signer}
	case e.Sequence == ns.High && digest != ns.Digest:
		ns.Tied = true
		r.supersede(ns.Digest, name, ns.Signer)
		r.supersede(digest, name, signer)
	case e.Sequence < ns.High:
		r.supersede(digest, name, signer)
	}
}

func (r *Record) supersede(digest, name string, signer []byte) {
	if signer == nil {
		return
	}
	r.Superseded[digest] = Superseded{Name: name, Signer: signer}
}

// loadRecord reads the record at path. A missing file is a fresh record and
// false; anything that is not exactly a version-1 record is an error, since
// silently starting over would forget every revoke and lapse.
func loadRecord(path string) (*Record, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is the job's own record in its private dir
	if errors.Is(err, fs.ErrNotExist) {
		return newRecord(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return nil, false, fmt.Errorf("record: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false, errors.New("record: trailing data")
	}
	if r.Version != recordVersion {
		return nil, false, fmt.Errorf("record: version %d, want %d", r.Version, recordVersion)
	}
	if r.Names == nil {
		r.Names = map[string]*NameState{}
	}
	if r.Superseded == nil {
		r.Superseded = map[string]Superseded{}
	}
	if r.Lapsed == nil {
		r.Lapsed = map[string]bool{}
	}
	if r.Entries == nil {
		r.Entries = map[string]*Tracked{}
	}
	return &r, true, nil
}

func saveRecord(path string, r *Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b, 0o600)
}
