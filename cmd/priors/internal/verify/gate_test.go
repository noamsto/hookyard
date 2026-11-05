package verify

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

type fakeGroups struct {
	names map[int]string
}

func (f fakeGroups) Gids() ([]int, error) {
	gids := make([]int, 0, len(f.names))
	for g := range f.names {
		gids = append(gids, g)
	}
	return gids, nil
}

func (f fakeGroups) Name(gid int) (string, error) { return f.names[gid], nil }

const (
	personalID = "personal-test"
	workID     = "work-test"
	uid        = 1234
)

func pinSSH(t *testing.T) {
	t.Helper()
	oldCfg, oldKH := tools.SSHConfig, tools.KnownHosts
	tools.SSHConfig, tools.KnownHosts = "/pin/ssh_config", "/pin/known_hosts"
	t.Cleanup(func() { tools.SSHConfig, tools.KnownHosts = oldCfg, oldKH })
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	k := attesttest.NewSKEd25519(t)
	return config.Config{
		TrustRoot:       "separate",
		TrustRootLabel:  "separate",
		AttestKeys:      []config.AttestKey{{Key: k.PublicKey(), Attestation: []byte("a"), Challenge: []byte("c")}},
		PersonalStoreID: personalID,
		PersonalRemote:  "git@host:personal.git",
		WorkStoreID:     workID,
		WorkRemote:      "ssh://git@host/work.git",
		Profile:         "work",
		TrustDigest:     "abc123",
	}
}

func testEnv(t *testing.T) Env {
	t.Helper()
	root := t.TempDir()
	for _, id := range []string{personalID, workID} {
		if err := os.Mkdir(filepath.Join(root, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return Env{Groups: fakeGroups{names: map[int]string{100: "users"}}, UID: uid, VerifyUID: "1234", Root: root}
}

func TestPrepare(t *testing.T) {
	pinSSH(t)
	const cred = "/run/cred/key"
	tests := []struct {
		name    string
		mutate  func(*config.Config, *Env, *string, *string)
		wantErr string
	}{
		{"trust_root absent", func(c *config.Config, _ *Env, _, _ *string) { c.TrustRoot, c.TrustRootLabel = "", "absent" }, "attestation off: trust_root"},
		{"trust_root owner-admin", func(c *config.Config, _ *Env, _, _ *string) {
			c.TrustRoot, c.TrustRootLabel = "owner-admin", "owner-admin"
		}, "attestation off: trust_root"},
		{"trust_root bogus", func(c *config.Config, _ *Env, _, _ *string) { c.TrustRoot, c.TrustRootLabel = "bogus", "bogus" }, "attestation off: trust_root"},
		{"trust_root non-string", func(c *config.Config, _ *Env, _, _ *string) { c.TrustRoot, c.TrustRootLabel = "", "non-string" }, "attestation off: trust_root"},
		{"group wheel", func(_ *config.Config, e *Env, _, _ *string) {
			e.Groups = fakeGroups{names: map[int]string{10: "wheel"}}
		}, "group wheel"},
		{"no keys", func(c *config.Config, _ *Env, _, _ *string) { c.AttestKeys = nil }, "no attestation key"},
		{"verify uid empty", func(_ *config.Config, e *Env, _, _ *string) { e.VerifyUID = "" }, "verify uid"},
		{"verify uid not a number", func(_ *config.Config, e *Env, _, _ *string) { e.VerifyUID = "x" }, "verify uid"},
		{"verify uid mismatch", func(_ *config.Config, e *Env, _, _ *string) { e.VerifyUID = "99" }, "verify uid 99"},
		{"unknown store", func(_ *config.Config, _ *Env, id, _ *string) { *id = "nope" }, "unknown store"},
		{"work id on personal profile", func(c *config.Config, _ *Env, id, _ *string) {
			c.Profile, c.WorkStoreID, c.WorkRemote = "personal", "", ""
			*id = workID
		}, "unknown store"},
		{"no remote", func(c *config.Config, _ *Env, _, _ *string) { c.PersonalRemote = "" }, "no remote"},
		{"https remote", func(c *config.Config, _ *Env, _, _ *string) { c.PersonalRemote = "https://host/p.git" }, "not an ssh url"},
		{"local path remote", func(c *config.Config, _ *Env, _, _ *string) { c.PersonalRemote = "/local/path" }, "not an ssh url"},
		{"ext transport remote", func(c *config.Config, _ *Env, _, _ *string) { c.PersonalRemote = "ext::sh -c x" }, "not an ssh url"},
		{"fd transport remote", func(c *config.Config, _ *Env, _, _ *string) { c.PersonalRemote = "fd::0" }, "not an ssh url"},
		{"https transport remote", func(c *config.Config, _ *Env, _, _ *string) { c.PersonalRemote = "https::https://h/r" }, "not an ssh url"},
		{"ssh url with transport", func(c *config.Config, _ *Env, _, _ *string) { c.PersonalRemote = "ssh://h/ext::r" }, "not an ssh url"},
		{"relative credential", func(_ *config.Config, _ *Env, _, cr *string) { *cr = "key" }, "credential"},
		{"missing store dir", func(_ *config.Config, e *Env, _, _ *string) { e.Root = filepath.Join(e.Root, "gone") }, "missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, env, id, credential := testConfig(t), testEnv(t), personalID, cred
			tc.mutate(&cfg, &env, &id, &credential)
			before := listDir(t, env.Root, id)
			_, err := Prepare(cfg, id, credential, env)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if after := listDir(t, env.Root, id); !slices.Equal(before, after) {
				t.Errorf("store dir changed: %v -> %v", before, after)
			}
		})
	}

	t.Run("unpinned ssh", func(t *testing.T) {
		tools.SSHConfig = ""
		env := testEnv(t)
		_, err := Prepare(testConfig(t), personalID, cred, env)
		if !errors.Is(err, tools.ErrUnpinned) {
			t.Fatalf("err = %v, want ErrUnpinned", err)
		}
		if got := listDir(t, env.Root, personalID); len(got) != 0 {
			t.Errorf("store dir changed: %v", got)
		}
	})
}

func listDir(t *testing.T, root, id string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(root, id))
	if err != nil {
		return nil
	}
	names := make([]string, len(ents))
	for i, e := range ents {
		names[i] = e.Name()
	}
	return names
}

func TestPrepareHappy(t *testing.T) {
	pinSSH(t)
	cfg, env := testConfig(t), testEnv(t)
	for _, id := range []string{personalID, workID} {
		j, err := Prepare(cfg, id, "/run/cred/key", env)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if j.Dir != filepath.Join(env.Root, id) || j.Store.ID != id || len(j.Keys) != 1 ||
			j.Budget != DefaultBudget || j.Credential != "/run/cred/key" || j.TrustDigest != "abc123" || j.Now == nil {
			t.Errorf("%s: job = %+v", id, j)
		}
		fi, err := os.Lstat(filepath.Join(j.Dir, "private"))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: private = %v, %v", id, fi, err)
		}
	}
	if got := mustPrepare(t, cfg, env).Store.Remote; got != cfg.PersonalRemote {
		t.Errorf("remote = %q", got)
	}
}

func mustPrepare(t *testing.T, cfg config.Config, env Env) *Job {
	t.Helper()
	j, err := Prepare(cfg, personalID, "/run/cred/key", env)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestPreparePrivateMode(t *testing.T) {
	pinSSH(t)
	env := testEnv(t)
	private := filepath.Join(env.Root, personalID, "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(testConfig(t), personalID, "/run/cred/key", env); err == nil {
		t.Fatal("0755 private/ accepted")
	}
}

func TestCheckEvidenceFiltersKeys(t *testing.T) {
	pinSSH(t)
	old := CheckEvidence
	t.Cleanup(func() { CheckEvidence = old })

	cfg := testConfig(t)
	second := attesttest.NewSKEd25519(t)
	cfg.AttestKeys = append(cfg.AttestKeys, config.AttestKey{Key: second.PublicKey(), Attestation: []byte("a"), Challenge: []byte("c")})
	bad := cfg.AttestKeys[0].Key

	CheckEvidence = func(k config.AttestKey) error {
		if string(k.Key.Marshal()) == string(bad.Marshal()) {
			return errors.New("rejected")
		}
		return nil
	}
	env := testEnv(t)
	j, err := Prepare(cfg, personalID, "/run/cred/key", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Keys) != 1 || string(j.Keys[0].Marshal()) != string(second.PublicKey().Marshal()) {
		t.Fatalf("Keys = %v, want only the second key", j.Keys)
	}

	CheckEvidence = func(config.AttestKey) error { return errors.New("rejected") }
	env = testEnv(t)
	_, err = Prepare(cfg, personalID, "/run/cred/key", env)
	if err == nil || !strings.Contains(err.Error(), "no attestation key passed the evidence check") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(env.Root, personalID, "private")); !os.IsNotExist(err) {
		t.Errorf("private/ created before keys were filtered: %v", err)
	}
}

func TestLock(t *testing.T) {
	pinSSH(t)
	cfg, env := testConfig(t), testEnv(t)
	a := mustPrepare(t, cfg, env)
	rel, err := Lock(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(a.Dir); err == nil || !strings.Contains(err.Error(), "another run holds the lock") {
		t.Fatalf("second Lock err = %v", err)
	}

	b, err := Prepare(cfg, workID, "/run/cred/key", env)
	if err != nil {
		t.Fatal(err)
	}
	relB, err := Lock(b.Dir)
	if err != nil {
		t.Fatalf("other store's Lock: %v", err)
	}
	relB()

	rel()
	rel2, err := Lock(a.Dir)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	rel2()
}
