package tier1

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
)

// native is what Claude's own memory dir already tells a session: the names of
// its topic files and the source paths an import records for them.
type native struct {
	names, sources map[string]bool
}

// has reports whether f is one of the native facts: it carries a native topic
// file's name, or was imported from one (a rename survives in the source path).
func (n native) has(f fact.Fact) bool {
	if n.names[f.Name] {
		return true
	}
	src := f.Metadata.Source
	return src != nil && src.Engine == "claude" && n.sources[src.Path]
}

// nativeIndex reads dir's topic files. MEMORY.md, other extensions and files
// that are not readable facts are ignored.
func nativeIndex(dir string) native {
	n := native{names: map[string]bool{}, sources: map[string]bool{}}
	if dir == "" {
		return n
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return n
	}
	for _, e := range entries {
		name := e.Name()
		if name == "MEMORY.md" || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := importer.ReadRegular(path)
		if err != nil {
			continue
		}
		f, err := fact.Parse(raw)
		if err != nil {
			continue
		}
		n.names[f.Name] = true
		n.sources[importer.SourcePath(path)] = true
	}
	return n
}
