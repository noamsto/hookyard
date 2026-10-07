//go:build !priorstest

package config

import (
	"path/filepath"
	"testing"
)

func TestTrustSourceIgnoresTheTestOverride(t *testing.T) {
	t.Setenv("PRIORS_TEST_TRUST", filepath.Join(t.TempDir(), "trust.toml"))

	p, fsys := trustSource()

	if p != "/etc/priors/trust.toml" {
		t.Errorf("trust path = %q, want /etc/priors/trust.toml", p)
	}
	if _, ok := fsys.(osFS); !ok {
		t.Errorf("trust file system = %T, want osFS", fsys)
	}
}
