package attest

import (
	"path/filepath"
	"testing"
)

func TestTriggerPath(t *testing.T) {
	want := filepath.Join(TriggerRoot, "personal-1.sock")
	if got := TriggerPath("personal-1"); got != want {
		t.Errorf("TriggerPath = %q, want %q", got, want)
	}
}
