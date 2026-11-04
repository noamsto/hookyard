package tier1

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
)

// maxNativeTopics is Claude's own cap on the entries its MEMORY.md index loads.
const maxNativeTopics = 200

// native is what Claude's own memory dir already tells a session: the names of
// the topic files its MEMORY.md links and the source paths an import records
// for them.
type native struct {
	names, sources map[string]bool
}

// has reports whether f is one of the native facts. A fact imported from a
// Claude memory file is native only if that exact file is; any other fact is
// native if it carries a native topic file's name.
func (n native) has(f fact.Fact) bool {
	if src := f.Metadata.Source; src != nil && src.Engine == "claude" {
		return n.sources[src.Path]
	}
	return n.names[f.Name]
}

// nativeIndex reads the topic files that the loaded part of dir's MEMORY.md
// links to, since Claude never loads an unlinked file. A missing or unreadable
// MEMORY.md, and links to unreadable or non-fact files, count for nothing.
func nativeIndex(ctx context.Context, dir string) native {
	n := native{names: map[string]bool{}, sources: map[string]bool{}}
	if dir == "" {
		return n
	}
	index, err := importer.ReadRegular(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		return n
	}
	read := 0
	seen := map[string]bool{}
	for _, name := range importer.IndexedTopics(index) {
		if ctx.Err() != nil || read >= maxNativeTopics {
			break
		}
		if name == "MEMORY.md" || !strings.HasSuffix(name, ".md") || seen[name] {
			continue
		}
		seen[name] = true
		read++
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
