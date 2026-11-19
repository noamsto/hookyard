package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
)

func TestImperativeCommandEdges(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"prose run", "the run failed twice", false},
		{"prose call", "we used to call it X", false},
		{"verb without command", "Set the alarm.", false},
		{"heading", "# Run notes", false},
		{"empty", "", false},
		{"list item", "- run `x`", true},
		{"numbered", "1. Run `x`", true},
		{"paren numbered", "2) use ./bin/x", true},
		{"lead-ins", "Then, please just run `x`", true},
		{"you should", "You should install `x`", true},
		{"sentence after period", "It breaks. Run `x` to fix it", true},
		{"sentence after colon", "One step: append `-rc1`", true},
		{"flag word", "Pass --force to it", true},
		{"dotted word", "use foo.bar", true},
		{"quoted", `Enter "yes" at the prompt`, true},
		{"single quoted", "Type 'yes' at the prompt", true},
		{"trailing punctuation stripped", "Use the thing, then stop.", false},
		{"verb prefix is not a word", "running `x` is slow", false},
		{"case insensitive", "RUN `x`", true},
		{"second line", "intro\nrun `x`", true},
		{"remember to", "Remember to run `nix flake check` before pushing.", true},
		{"make sure to", "Make sure to run `make clean` first.", true},
		{"make sure you", "Make sure you run `make clean` first.", true},
		{"dont forget", "Don't forget to run `direnv allow`.", true},
		{"do not forget", "Do not forget to run `direnv allow`.", true},
		{"be sure to", "Be sure to run `make clean` first.", true},
		{"leading if clause", "If the build breaks, run `make clean`.", true},
		{"leading before clause", "Before pushing, run `nix flake check`.", true},
		{"leading to clause", "To reset it, run `make clean`.", true},
		{"noun source code", "Source code for the daemon lives in `./src/daemon`.", false},
		{"noun type checking", "Type checking runs through `tsc --noEmit` in CI.", false},
		{"noun call sites", "Call sites of `foo()` are all in the handler.", false},
		{"noun use of", "Use of `--impure` is required for this flake.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ImperativeCommand(tt.body); got != tt.want {
				t.Errorf("ImperativeCommand(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestImperativeCommandCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "imported", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var notes, instrs int
	for _, path := range files {
		base := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := fact.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		switch {
		case strings.HasPrefix(base, "note-"):
			notes++
			if ImperativeCommand(f.Description + "\n" + f.Body) {
				t.Errorf("%s: ImperativeCommand = true, want false", base)
			}
			if got := Content(string(raw)); len(got) != 0 {
				t.Errorf("%s: Content = %v, want none", base, got)
			}
			if got := Size(len(raw)); len(got) != 0 {
				t.Errorf("%s: Size = %v, want none", base, got)
			}
		case strings.HasPrefix(base, "instr-"):
			instrs++
			if !ImperativeCommand(f.Description + "\n" + f.Body) {
				t.Errorf("%s: ImperativeCommand = false, want true", base)
			}
		default:
			t.Errorf("%s: fixture name needs a note- or instr- prefix", base)
		}
	}
	if notes < 29 || instrs < 18 {
		t.Errorf("corpus has %d notes and %d instructions, want at least 29 and 18", notes, instrs)
	}
	t.Logf("corpus: %d notes, %d instructions", notes, instrs)
}
