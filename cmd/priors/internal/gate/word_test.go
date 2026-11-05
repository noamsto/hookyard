package gate

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestBashAssignedNames keeps bashAssigns and bashDefaults complete for the
// bash on PATH: every variable a bash started with an empty environment holds
// must be one of them. A skip, as inside a nix sandbox, is no coverage.
func TestBashAssignedNames(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	// Lists names by prefix expansion, with no variable of its own: compgen is
	// absent from some builds.
	const list = `eval "printf '%s\n' $(printf '${!%s@} ' {A..Z} {a..z} _)"`
	modes := map[string][]string{
		"non-interactive": {"--norc", "--noprofile", "-c", list},
		"interactive":     {"--norc", "--noprofile", "-i", "-c", list},
		"posix history":   {"--norc", "--noprofile", "-c", "set -o posix -o history;" + list},
	}
	for mode, args := range modes {
		cmd := exec.Command(bash, args...)
		cmd.Env = []string{}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		for name := range strings.FieldsSeq(string(out)) {
			if !bashAssigns(name) && !slices.Contains(bashDefaults, name) && name != "HOME" {
				t.Errorf("%s: bash assigns %s, which bashAssigns and bashDefaults miss", mode, name)
			}
		}
	}
}
