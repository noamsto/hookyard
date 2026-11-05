package verify

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

func recEntry(seq uint64, op attest.Op) attest.Entry {
	return attest.Entry{Store: "s", Name: "n", Sequence: seq, Op: op}
}

var (
	recKeyA = []byte("key-a")
	recKeyB = []byte("key-b")
)

func TestObserve(t *testing.T) {
	t.Run("higher entry supersedes the signed old digest", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d1", recEntry(1, attest.OpAttest), recKeyA)
		r.Observe("n", "d2", recEntry(2, attest.OpRevoke), recKeyB)
		want := &NameState{High: 2, Digest: "d2", Op: attest.OpRevoke, Signer: recKeyB}
		if got := r.Names["n"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("name state = %+v, want %+v", got, want)
		}
		wantSup := map[string]Superseded{"d1": {Name: "n", Signer: recKeyA}}
		if !reflect.DeepEqual(r.Superseded, wantSup) {
			t.Fatalf("superseded = %+v, want %+v", r.Superseded, wantSup)
		}
		if len(r.Entries) != 0 {
			t.Fatalf("Observe created tracked entries: %+v", r.Entries)
		}
	})

	t.Run("higher entry over an unsigned old digest records nothing", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d1", recEntry(1, attest.OpAttest), nil)
		r.Observe("n", "d2", recEntry(2, attest.OpAttest), recKeyA)
		if len(r.Superseded) != 0 {
			t.Fatalf("superseded = %+v, want empty", r.Superseded)
		}
		if got := r.Names["n"]; got.High != 2 || got.Digest != "d2" || got.Tied {
			t.Fatalf("name state = %+v", got)
		}
	})

	t.Run("same digest at high is not a tie", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d1", recEntry(1, attest.OpAttest), recKeyA)
		before := cloneRecord(t, r)
		r.Observe("n", "d1", recEntry(1, attest.OpAttest), recKeyA)
		if !reflect.DeepEqual(r, before) {
			t.Fatalf("re-observing changed the record: %+v, want %+v", r, before)
		}
		if r.Names["n"].Tied || len(r.Superseded) != 0 {
			t.Fatalf("same digest recorded as a tie: %+v", r)
		}
	})

	t.Run("different digest at high ties and supersedes both signed digests", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d1", recEntry(3, attest.OpAttest), recKeyA)
		r.Observe("n", "d2", recEntry(3, attest.OpAttest), recKeyB)
		got := r.Names["n"]
		if !got.Tied || got.High != 3 {
			t.Fatalf("name state = %+v, want tied at 3", got)
		}
		wantSup := map[string]Superseded{
			"d1": {Name: "n", Signer: recKeyA},
			"d2": {Name: "n", Signer: recKeyB},
		}
		if !reflect.DeepEqual(r.Superseded, wantSup) {
			t.Fatalf("superseded = %+v, want %+v", r.Superseded, wantSup)
		}
	})

	t.Run("tie with an unsigned side supersedes only the signed one", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d1", recEntry(3, attest.OpAttest), recKeyA)
		r.Observe("n", "junk", recEntry(3, attest.OpAttest), nil)
		if !r.Names["n"].Tied {
			t.Fatalf("unsigned different digest at high did not tie")
		}
		wantSup := map[string]Superseded{"d1": {Name: "n", Signer: recKeyA}}
		if !reflect.DeepEqual(r.Superseded, wantSup) {
			t.Fatalf("superseded = %+v, want %+v", r.Superseded, wantSup)
		}
	})

	t.Run("lower entry is superseded if signed", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d5", recEntry(5, attest.OpAttest), recKeyA)
		r.Observe("n", "d2", recEntry(2, attest.OpRevoke), recKeyB)
		r.Observe("n", "junk", recEntry(1, attest.OpAttest), nil)
		want := &NameState{High: 5, Digest: "d5", Op: attest.OpAttest, Signer: recKeyA}
		if got := r.Names["n"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("name state = %+v, want %+v", got, want)
		}
		wantSup := map[string]Superseded{"d2": {Name: "n", Signer: recKeyB}}
		if !reflect.DeepEqual(r.Superseded, wantSup) {
			t.Fatalf("superseded = %+v, want %+v", r.Superseded, wantSup)
		}
	})

	t.Run("higher entry after a tie clears tied", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d1", recEntry(1, attest.OpAttest), recKeyA)
		r.Observe("n", "d2", recEntry(1, attest.OpAttest), recKeyB)
		r.Observe("n", "d3", recEntry(2, attest.OpAttest), recKeyA)
		want := &NameState{High: 2, Digest: "d3", Op: attest.OpAttest, Signer: recKeyA}
		if got := r.Names["n"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("name state = %+v, want %+v", got, want)
		}
		if _, ok := r.Superseded["d3"]; ok {
			t.Fatalf("new high digest recorded superseded")
		}
		if len(r.Superseded) != 2 {
			t.Fatalf("superseded = %+v, want d1 and d2", r.Superseded)
		}
	})

	t.Run("names are independent", func(t *testing.T) {
		r := newRecord()
		r.Observe("n", "d1", recEntry(4, attest.OpAttest), recKeyA)
		r.Observe("m", "e1", attest.Entry{Store: "s", Name: "m", Sequence: 1, Op: attest.OpAttest}, recKeyA)
		if r.Names["n"].High != 4 || r.Names["m"].High != 1 || len(r.Superseded) != 0 {
			t.Fatalf("record = %+v", r)
		}
	})

	t.Run("re-walking the same entries yields an identical record", func(t *testing.T) {
		type obs struct {
			digest string
			seq    uint64
			op     attest.Op
			signer []byte
		}
		walk := []obs{
			{"d1", 1, attest.OpAttest, recKeyA},
			{"d3", 3, attest.OpAttest, recKeyA},
			{"d2", 2, attest.OpRevoke, recKeyB},
			{"j3", 3, attest.OpAttest, nil},
			{"d4", 4, attest.OpRevoke, recKeyA},
			{"x4", 4, attest.OpAttest, recKeyB},
			{"j0", 1, attest.OpAttest, nil},
		}
		r := newRecord()
		for _, o := range walk {
			r.Observe("n", o.digest, recEntry(o.seq, o.op), o.signer)
		}
		once := cloneRecord(t, r)
		for _, o := range walk {
			r.Observe("n", o.digest, recEntry(o.seq, o.op), o.signer)
		}
		if !reflect.DeepEqual(r, once) {
			t.Fatalf("second walk changed the record:\n got %+v\nwant %+v", r, once)
		}
	})
}

func cloneRecord(t *testing.T, r *Record) *Record {
	t.Helper()
	path := filepath.Join(t.TempDir(), "record.json")
	if err := saveRecord(path, r); err != nil {
		t.Fatal(err)
	}
	c, ok, err := loadRecord(path)
	if err != nil || !ok {
		t.Fatalf("loadRecord = %v, %v", ok, err)
	}
	return c
}

func TestRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	r := newRecord()
	r.Tip = "abc123"
	r.Fetched = 1_700_000_000
	r.Observe("n", "d1", recEntry(1, attest.OpAttest), recKeyA)
	r.Observe("n", "d2", recEntry(2, attest.OpRevoke), recKeyB)
	r.Lapsed["d0"] = true
	r.Entries["d1"] = &Tracked{Name: "n", Path: "src/a.go", SHA256: "ff", Intros: []string{"c1", "c2"}}

	if err := saveRecord(path, r); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("record mode = %o, want 600", mode)
	}
	got, ok, err := loadRecord(path)
	if err != nil || !ok {
		t.Fatalf("loadRecord = %v, %v", ok, err)
	}
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, r)
	}

	t.Run("missing file is a fresh record", func(t *testing.T) {
		got, ok, err := loadRecord(filepath.Join(dir, "absent.json"))
		if err != nil || ok {
			t.Fatalf("loadRecord = %v, %v; want false, nil", ok, err)
		}
		if !reflect.DeepEqual(got, newRecord()) {
			t.Fatalf("missing record = %+v, want newRecord()", got)
		}
	})

	t.Run("empty maps load non-nil", func(t *testing.T) {
		p := filepath.Join(dir, "bare.json")
		if err := os.WriteFile(p, []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok, err := loadRecord(p)
		if err != nil || !ok {
			t.Fatalf("loadRecord = %v, %v", ok, err)
		}
		if got.Names == nil || got.Superseded == nil || got.Lapsed == nil || got.Entries == nil {
			t.Fatalf("nil map after load: %+v", got)
		}
		got.Observe("n", "d1", recEntry(1, attest.OpAttest), recKeyA)
	})

	for name, content := range map[string]string{
		"garbage":       "not json",
		"truncated":     `{"version":1,"names":{`,
		"wrong version": `{"version":2}`,
		"no version":    `{}`,
		"unknown field": `{"version":1,"extra":true}`,
		"trailing data": `{"version":1} {"version":1}`,
		"null name":     `{"version":1,"names":{"n":null}}`,
		"null entry":    `{"version":1,"entries":{"d":null}}`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			p := filepath.Join(dir, "bad.json")
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadRecord(p); err == nil {
				t.Fatalf("loadRecord accepted %q", content)
			}
		})
	}
}
