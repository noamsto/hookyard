package tier1

import (
	"fmt"
	"slices"
	"strings"
)

const (
	pointerLine  = "Search: priors search <query> · Read: priors show <name> · Review: priors list --flagged"
	flaggedGroup = "flagged, unreviewed"
	otherGroup   = "other"
)

// groupOrder is the order the grouped layout lists its groups in.
var groupOrder = []string{"feedback", "project", "reference", "user", otherGroup, flaggedGroup}

func groupOf(e entry) string {
	switch {
	case e.flagged:
		return flaggedGroup
	case slices.Contains(groupOrder[:4], e.typ):
		return e.typ
	}
	return otherGroup
}

func heading(group string) string {
	return "## " + group
}

func nativeCount(n int) string {
	if n == 1 {
		return "1 fact also in MEMORY.md"
	}
	return fmt.Sprintf("%d facts also in MEMORY.md", n)
}

// groupedMarker counts the dropped entries and names their groups in group
// order. It sits outside every group, so no heading claims it.
func groupedMarker(dropped []entry) string {
	var groups []string
	for _, e := range dropped {
		if g := groupOf(e); !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	return fmt.Sprintf("… %d entries omitted (%s) …", len(dropped), strings.Join(groups, ", "))
}

// groupedLines is one store's body in the grouped layout: a pointer line, the
// facts Claude's memory does not already hold under a heading per type,
// flagged ones last, then how many were left out. It returns nil when the
// store has nothing to say, or when even the framing does not fit avail.
func groupedLines(entries []entry, avail int) []string {
	kept := make([]entry, 0, len(entries))
	dups := 0
	for _, e := range entries {
		if e.native {
			dups++
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 && dups == 0 {
		return nil
	}
	slices.SortStableFunc(kept, func(a, b entry) int {
		return slices.Index(groupOrder, groupOf(a)) - slices.Index(groupOrder, groupOf(b))
	})

	// The framing is spent before the entries, so the cut leaves room for it;
	// a group that loses every entry to the cut costs less than reserved.
	overhead := []string{pointerLine}
	if dups > 0 {
		overhead = append(overhead, nativeCount(dups))
	}
	for i, e := range kept {
		if i == 0 || groupOf(e) != groupOf(kept[i-1]) {
			overhead = append(overhead, heading(groupOf(e)))
		}
	}
	reserve := cost(overhead)
	if reserve > avail {
		return nil
	}

	lines := make([]string, len(kept))
	for i, e := range kept {
		lines[i] = e.line
	}
	lo, hi := len(lines), len(lines)
	if reserve+cost(lines) > avail {
		// The cut needs a marker line, which names the groups it dropped from.
		reserve += cost([]string{groupedMarker(kept)})
		if reserve > avail {
			return nil
		}
		lo, hi = fitRange(lines, avail-reserve)
	}
	if hi-lo == len(kept) && dups == 0 {
		return nil
	}

	out := []string{pointerLine}
	if hi > lo {
		out = append(out, groupedMarker(kept[lo:hi]))
	}
	last := ""
	emit := func(from, to int) {
		for _, e := range kept[from:to] {
			if g := groupOf(e); g != last {
				out = append(out, heading(g))
				last = g
			}
			out = append(out, e.line)
		}
	}
	emit(0, lo)
	emit(hi, len(kept))
	if dups > 0 {
		out = append(out, nativeCount(dups))
	}
	return out
}
