package tier1

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/importer"
)

const (
	// nativeHead bounds the part of a topic file read: matching needs only the
	// frontmatter's name and description, and parsing a huge one is slow.
	nativeHead = 8 << 10
	// nativeBudget keeps a slow memory dir from eating the hook deadline; past
	// it nothing is deduplicated.
	nativeBudget = 150 * time.Millisecond
)

// native is what Claude's own memory dir already tells a session: the topic
// files its MEMORY.md links, as name -> description and as the source path an
// import records for them -> description.
type native struct {
	names, sources map[string]string
}

// has reports whether f is one of the native facts. A fact imported from a
// Claude memory file matches only that exact file; any other fact matches the
// topic file of its name. Either way the descriptions must agree, so an edited
// native file or an unrelated one with the same name hides nothing.
func (n native) has(f fact.Fact) bool {
	var desc string
	var ok bool
	if src := f.Metadata.Source; src != nil && src.Engine == "claude" {
		desc, ok = n.sources[src.Path]
	} else {
		desc, ok = n.names[f.Name]
	}
	return ok && desc == strings.TrimSpace(f.Description)
}

// nativeIndex reads the topic files that the loaded part of dir's MEMORY.md
// links to, since Claude never loads an unlinked file. A missing or unreadable
// MEMORY.md, and links to unreadable or non-fact files, count for nothing. If
// the work does not finish in time the result is empty.
func nativeIndex(ctx context.Context, dir string) native {
	n := native{names: map[string]string{}, sources: map[string]string{}}
	if dir == "" {
		return n
	}
	ctx, cancel := context.WithTimeout(ctx, nativeBudget)
	defer cancel()
	index, err := importer.ReadRegular(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		return n
	}
	seen := map[string]bool{}
	for _, name := range importer.IndexedTopics(index) {
		if ctx.Err() != nil {
			return native{}
		}
		if name == "MEMORY.md" || !strings.HasSuffix(name, ".md") || seen[name] {
			continue
		}
		seen[name] = true
		path := filepath.Join(dir, name)
		head, err := importer.ReadRegularPrefix(path, nativeHead)
		if err != nil {
			continue
		}
		if len(head) == nativeHead {
			// Drop a possibly cut last line so it cannot pass for a fence.
			if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
				head = head[:i+1]
			}
		}
		f, err := fact.Parse(head)
		if err != nil {
			continue
		}
		desc := strings.TrimSpace(f.Description)
		n.names[f.Name] = desc
		n.sources[importer.SourcePath(path)] = desc
	}
	if ctx.Err() != nil {
		return native{}
	}
	return n
}
