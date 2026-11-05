package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
)

func TestRunFirstFetchOK(t *testing.T) {
	f := newFixture(t)
	fact := []byte("fact a\n")
	ent := f.entry(f.A, "a", "facts/a.md", fact, 1, "attest")
	f.commit(map[string][]byte{"facts/a.md": fact, ".attest/a": ent})
	f.push()
	tip := gitIn(t, f.remote, "", "rev-parse", "main")
	j := f.job()

	for run := 1; run <= 2; run++ {
		res := f.run(j)
		if res.Status != StatusOK || !res.Wrote {
			t.Fatalf("run %d: %+v, want ok and written", run, res)
		}
		v := f.verdicts()
		if v.Store != "personal" || v.Tip != tip || v.Status != string(StatusOK) || v.Trust != j.TrustDigest {
			t.Fatalf("run %d: header %+v", run, v)
		}
		if len(v.States) != 1 || v.States[digest(ent)] != attest.StateReviewed {
			t.Fatalf("run %d: states %v, want %s reviewed", run, v.States, digest(ent))
		}
	}

	private := filepath.Join(f.state, "private")
	for path, want := range map[string]os.FileMode{private: 0o700, filepath.Join(private, "record.json"): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", path, fi.Mode().Perm(), want)
		}
	}
	if log := f.sshLog(); !strings.Contains(log, "git-upload-pack") {
		t.Errorf("ssh.log = %q, want a git-upload-pack invocation", log)
	}
}
