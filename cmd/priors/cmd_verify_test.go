package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/config"
)

// TestVerifyRunsGateEveryRun: a run queued on the socket reads the trust
// file again, so one turned off since the process started writes nothing.
func TestVerifyRunsGateEveryRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	loads := 0
	runs := &verifyRuns{
		store:      "personal",
		credential: "/key",
		s:          streams{out: &stdout, err: &stderr},
		loadTrust: func() (config.Config, error) {
			loads++
			return config.Config{TrustRootLabel: "absent"}, nil
		},
	}
	runs.once()
	runs.once()
	if loads != 2 {
		t.Fatalf("trust file read %d times over 2 runs, want 2", loads)
	}
	if runs.code != 1 {
		t.Fatalf("code %d, want 1", runs.code)
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stderr %q, want 2 lines", stderr.String())
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "priors verify: attestation off: trust_root") {
			t.Fatalf("stderr line %q, want the gating refusal", line)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q, want empty", stdout.String())
	}
}
