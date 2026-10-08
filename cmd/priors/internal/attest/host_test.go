package attest

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest/attesttest"
	"github.com/noamsto/hookyard/cmd/priors/internal/config"
)

type fakeGroups struct {
	gids     []int
	gidsErr  error
	names    map[int]string
	calls    int
	nameCall []int
}

func (f *fakeGroups) Gids() ([]int, error) {
	f.calls++
	return f.gids, f.gidsErr
}

func (f *fakeGroups) Name(gid int) (string, error) {
	f.calls++
	f.nameCall = append(f.nameCall, gid)
	name, ok := f.names[gid]
	if !ok {
		return "", errors.New("unknown group")
	}
	return name, nil
}

func separateCfg(keys ...ssh.PublicKey) config.Config {
	cfg := config.Config{TrustRoot: "separate", TrustRootLabel: "separate"}
	for _, k := range keys {
		cfg.AttestKeys = append(cfg.AttestKeys, config.AttestKey{Key: k})
	}
	return cfg
}

func TestClassifyOffWithoutSeparateTrustRoot(t *testing.T) {
	for _, tc := range []struct{ root, label string }{
		{"", "absent"},
		{"owner-admin", "owner-admin"},
		{"weird", "weird"},
		{"", "1"},
	} {
		g := &fakeGroups{gids: []int{100}, names: map[int]string{100: "users"}}
		cfg := config.Config{TrustRoot: tc.root, TrustRootLabel: tc.label}
		h := Classify(cfg, g)
		if want := "attestation off: trust_root " + tc.label; h.Off != want {
			t.Errorf("label %q: Off = %q, want %q", tc.label, h.Off, want)
		}
		if h.On() {
			t.Errorf("label %q: On() = true", tc.label)
		}
		if g.calls != 0 {
			t.Errorf("label %q: GroupSource called %d times", tc.label, g.calls)
		}
	}
}

func TestClassifyOnWithCleanGroups(t *testing.T) {
	key := attesttest.NewSKEd25519(t).PublicKey()
	g := &fakeGroups{gids: []int{100}, names: map[int]string{100: "users"}}
	h := Classify(separateCfg(key), g)
	if !h.On() {
		t.Fatalf("Off = %q", h.Off)
	}
	if len(h.Keys) != 1 || !bytes.Equal(h.Keys[0].Marshal(), key.Marshal()) {
		t.Errorf("Keys = %v, want the enrolled key", h.Keys)
	}
}

func TestClassifyOffForPrivilegedGroups(t *testing.T) {
	key := attesttest.NewSKEd25519(t).PublicKey()
	for _, name := range []string{"wheel", "admin", "docker", "libvirtd", "input", "disk", "lxd", "incus-admin"} {
		g := &fakeGroups{gids: []int{100, 7}, names: map[int]string{100: "users", 7: name}}
		h := Classify(separateCfg(key), g)
		if want := "attestation off: group " + name; h.Off != want {
			t.Errorf("%s: Off = %q, want %q", name, h.Off, want)
		}
	}
}

func TestClassifyOffWhenGroupsUnreadable(t *testing.T) {
	key := attesttest.NewSKEd25519(t).PublicKey()
	g := &fakeGroups{gidsErr: errors.New("boom")}
	h := Classify(separateCfg(key), g)
	if want := "attestation off: groups unreadable: boom"; h.Off != want {
		t.Errorf("Off = %q, want %q", h.Off, want)
	}
}

func TestClassifyOffForUnnamedGroup(t *testing.T) {
	key := attesttest.NewSKEd25519(t).PublicKey()
	g := &fakeGroups{gids: []int{100, 4242}, names: map[int]string{100: "users"}}
	h := Classify(separateCfg(key), g)
	if want := "attestation off: group 4242"; h.Off != want {
		t.Errorf("Off = %q, want %q", h.Off, want)
	}
}

func TestClassifyDeduplicatesAndSortsGids(t *testing.T) {
	key := attesttest.NewSKEd25519(t).PublicKey()
	g := &fakeGroups{gids: []int{100, 30, 100, 30}, names: map[int]string{30: "audio", 100: "users"}}
	if h := Classify(separateCfg(key), g); !h.On() {
		t.Fatalf("Off = %q", h.Off)
	}
	if len(g.nameCall) != 2 || g.nameCall[0] != 30 || g.nameCall[1] != 100 {
		t.Errorf("Name calls = %v, want [30 100]", g.nameCall)
	}
}

func TestClassifyOffWithoutEnrolledKey(t *testing.T) {
	g := &fakeGroups{gids: []int{100}, names: map[int]string{100: "users"}}
	h := Classify(separateCfg(), g)
	if want := "no attestation key enrolled"; h.Off != want {
		t.Errorf("Off = %q, want %q", h.Off, want)
	}
}

func TestOSGroupsIncludePrimaryGid(t *testing.T) {
	gids, err := OSGroups.Gids()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gids, os.Getgid()) {
		t.Errorf("Gids() = %v, want it to hold the primary gid %d", gids, os.Getgid())
	}
}
