package fact

import (
	"bytes"
	"fmt"
	"slices"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// WithConfidence returns raw with metadata.confidence set to value, changing
// only that scalar's bytes. raw is returned as is when it has no confidence.
// A quoted scalar loses its quotes: value is written plain, so it must be a
// plain YAML scalar.
func WithConfidence(raw []byte, value string) ([]byte, error) {
	start, end, err := frontmatterSpan(raw)
	if err != nil {
		return nil, err
	}
	front := raw[start:end]
	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if n := confidenceNode(&doc); n != nil {
		if lo, hi, ok := scalarSpan(front, n); ok {
			return slices.Concat(raw[:start+lo], []byte(value), raw[start+hi:]), nil
		}
	}
	return rewriteConfidence(raw, value)
}

// frontmatterSpan locates the frontmatter between the fences, tolerating CRLF
// line endings so a CRLF file cannot slip an unverified value past the caller.
func frontmatterSpan(raw []byte) (start, end int, err error) {
	for _, open := range []string{"---\n", "---\r\n"} {
		if bytes.HasPrefix(raw, []byte(open)) {
			start = len(open)
		}
	}
	if start == 0 {
		return 0, 0, fmt.Errorf("missing opening --- fence")
	}
	for pos := start; pos < len(raw); {
		line := raw[pos:]
		next := len(raw)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], pos+i+1
		}
		if string(bytes.TrimSuffix(line, []byte("\r"))) == "---" {
			return start, pos, nil
		}
		pos = next
	}
	return 0, 0, fmt.Errorf("missing closing --- fence")
}

func confidenceNode(doc *yaml.Node) *yaml.Node {
	if len(doc.Content) == 0 {
		return nil
	}
	meta := mappingValue(doc.Content[0], "metadata")
	if meta == nil {
		return nil
	}
	return mappingValue(meta, "confidence")
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// scalarSpan is the byte range of n in front, quotes included. ok is false
// unless the source at n's position is exactly n's value, so a tag, an alias,
// an escape or a multi-line scalar sends the caller to the rewrite path.
func scalarSpan(front []byte, n *yaml.Node) (lo, hi int, ok bool) {
	if n.Kind != yaml.ScalarNode || n.Value == "" {
		return 0, 0, false
	}
	if n.Style&(yaml.TaggedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return 0, 0, false
	}
	want := n.Value
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		want = `"` + want + `"`
	case n.Style&yaml.SingleQuotedStyle != 0:
		want = "'" + want + "'"
	}
	lo, ok = byteOffset(front, n.Line, n.Column)
	if !ok || !bytes.HasPrefix(front[lo:], []byte(want)) {
		return 0, 0, false
	}
	return lo, lo + len(want), true
}

// byteOffset converts yaml's 1-based line and rune column into a byte offset.
func byteOffset(b []byte, line, col int) (int, bool) {
	pos := 0
	for ; line > 1; line-- {
		i := bytes.IndexByte(b[pos:], '\n')
		if i < 0 {
			return 0, false
		}
		pos += i + 1
	}
	for ; col > 1; col-- {
		_, size := utf8.DecodeRune(b[pos:])
		pos += size
	}
	return pos, true
}

// rewriteConfidence re-marshals the fact, for the sources whose scalar span
// cannot be located byte-exactly.
func rewriteConfidence(raw []byte, value string) ([]byte, error) {
	f, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if f.Metadata.Confidence == "" {
		return raw, nil
	}
	f.Metadata.Confidence = value
	return f.Marshal()
}
