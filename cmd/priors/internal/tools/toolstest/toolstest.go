// Package toolstest is a test helper: the one place priors code looks a tool
// up on PATH, used only from tests' TestMain.
package toolstest

import (
	"os/exec"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// Pin fills each tool path that the build left empty from PATH. A tool that is
// not installed stays empty.
func Pin() {
	fill(&tools.Git, "git")
	fill(&tools.SSH, "ssh")
	fill(&tools.Rg, "rg")
	fill(&tools.Scanner, "betterleaks", "gitleaks")
}

func fill(dst *string, names ...string) {
	if *dst != "" {
		return
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			*dst = p
			return
		}
	}
}
