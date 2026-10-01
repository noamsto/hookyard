// Package fact is the on-disk shape of a memory fact: Markdown with YAML
// frontmatter that Claude Code already writes, extended with the metadata
// the memory layer needs. Unknown keys survive a parse/marshal round trip.
package fact

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// NameRE is a fact's name, which is also its filename stem.
var NameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,80}$`)

// Types is Claude's own memory-type vocabulary.
var Types = []string{"project", "reference", "feedback", "user"}

type Provenance struct {
	Engine  string         `yaml:"engine"`
	Session string         `yaml:"session"`
	Host    string         `yaml:"host"`
	Extra   map[string]any `yaml:",inline"`
}

type Source struct {
	Engine   string         `yaml:"engine"`
	Path     string         `yaml:"path"`
	ThreadID string         `yaml:"thread_id"`
	SHA256   string         `yaml:"sha256"`
	Extra    map[string]any `yaml:",inline"`
}

// Metadata is the `metadata:` block. SupersededBy and Verified are "" exactly
// when the file says null.
type Metadata struct {
	NodeType        string         `yaml:"node_type"`
	Type            string         `yaml:"type"`
	Scope           string         `yaml:"scope"`
	Repos           []string       `yaml:"repos"`
	Engines         []string       `yaml:"engines"`
	ValidFrom       string         `yaml:"valid_from"`
	SupersededBy    string         `yaml:"superseded_by"`
	Verified        string         `yaml:"verified"`
	Confidence      string         `yaml:"confidence"`
	Provenance      *Provenance    `yaml:"provenance"`
	Source          *Source        `yaml:"source"`
	OriginSessionID string         `yaml:"originSessionId"`
	Modified        string         `yaml:"modified"`
	Flags           []string       `yaml:"flags"`
	Extra           map[string]any `yaml:",inline"`
}

type Fact struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Metadata    Metadata       `yaml:"metadata"`
	Extra       map[string]any `yaml:",inline"`
	Body        string         `yaml:"-"`
}

// Parse reads a fact: "---\n", the frontmatter, a line that is exactly
// "---", then the body, which is kept verbatim.
func Parse(b []byte) (Fact, error) {
	rest, ok := strings.CutPrefix(string(b), "---\n")
	if !ok {
		return Fact{}, errors.New("missing opening --- fence")
	}
	front, body, ok := cutFence(rest)
	if !ok {
		return Fact{}, errors.New("missing closing --- fence")
	}
	var f Fact
	if err := yaml.Unmarshal([]byte(front), &f); err != nil {
		return Fact{}, fmt.Errorf("frontmatter: %w", err)
	}
	f.Body = body
	return f, nil
}

func cutFence(s string) (front, body string, ok bool) {
	if body, ok := strings.CutPrefix(s, "---\n"); ok {
		return "", body, true
	}
	if s == "---" {
		return "", "", true
	}
	if i := strings.Index(s, "\n---\n"); i >= 0 {
		return s[:i+1], s[i+5:], true
	}
	if front, ok := strings.CutSuffix(s, "\n---"); ok {
		return front + "\n", "", true
	}
	return "", "", false
}

// Marshal writes the fact with known keys in a fixed order, then Extra keys
// sorted, so the same fact always produces the same bytes.
func (f Fact) Marshal() ([]byte, error) {
	meta := mapNode()
	m := f.Metadata
	putString(meta, "node_type", m.NodeType)
	putString(meta, "type", m.Type)
	putString(meta, "scope", m.Scope)
	putList(meta, "repos", m.Repos)
	putList(meta, "engines", m.Engines)
	putDate(meta, "valid_from", m.ValidFrom)
	putNullable(meta, "superseded_by", m.SupersededBy)
	putNullable(meta, "verified", m.Verified)
	putString(meta, "confidence", m.Confidence)
	if p := m.Provenance; p != nil {
		sub := mapNode()
		putString(sub, "engine", p.Engine)
		putString(sub, "session", p.Session)
		putString(sub, "host", p.Host)
		if err := putExtra(sub, p.Extra); err != nil {
			return nil, err
		}
		putMap(meta, "provenance", sub)
	}
	if s := m.Source; s != nil {
		sub := mapNode()
		putString(sub, "engine", s.Engine)
		putString(sub, "path", s.Path)
		putString(sub, "thread_id", s.ThreadID)
		putString(sub, "sha256", s.SHA256)
		if err := putExtra(sub, s.Extra); err != nil {
			return nil, err
		}
		putMap(meta, "source", sub)
	}
	putString(meta, "originSessionId", m.OriginSessionID)
	putDate(meta, "modified", m.Modified)
	putList(meta, "flags", m.Flags)
	if err := putExtra(meta, m.Extra); err != nil {
		return nil, err
	}

	root := mapNode()
	put(root, "name", stringNode(f.Name))
	put(root, "description", stringNode(f.Description))
	put(root, "metadata", meta)
	if err := putExtra(root, f.Extra); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteString("---\n")
	buf.WriteString(f.Body)
	return buf.Bytes(), nil
}

// Text is what a search matches against: the name, the description and the body.
func (f Fact) Text() string {
	return f.Name + "\n" + f.Description + "\n" + f.Body
}

// ModifiedTime is when the fact last changed: Modified, else ValidFrom, else
// the zero time.
func (f Fact) ModifiedTime() time.Time {
	if t, ok := parseTime(f.Metadata.Modified); ok {
		return t
	}
	if t, err := time.Parse(time.DateOnly, f.Metadata.ValidFrom); err == nil {
		return t
	}
	return time.Time{}
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func mapNode() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode} }

func put(m *yaml.Node, key string, v *yaml.Node) {
	m.Content = append(m.Content, stringNode(key), v)
}

// stringNode forces a single-line scalar: yaml would otherwise pick a block
// style for a value with a newline.
func stringNode(s string) *yaml.Node {
	n := &yaml.Node{}
	_ = n.Encode(s)
	if strings.Contains(s, "\n") {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

func putString(m *yaml.Node, key, v string) {
	if v != "" {
		put(m, key, stringNode(v))
	}
}

// putDate writes a date or timestamp unquoted, as Claude does, so the file
// keeps its shape; yaml would quote the string to keep it a string.
func putDate(m *yaml.Node, key, v string) {
	if v == "" {
		return
	}
	n := stringNode(v)
	if _, isTime := parseTime(v); isTime || isDate(v) {
		n.Style = 0
		n.Tag = "!!timestamp"
	}
	put(m, key, n)
}

func isDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

func putNullable(m *yaml.Node, key, v string) {
	if v == "" {
		put(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"})
		return
	}
	putDate(m, key, v)
}

func putList(m *yaml.Node, key string, vs []string) {
	if len(vs) == 0 {
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, v := range vs {
		seq.Content = append(seq.Content, stringNode(v))
	}
	put(m, key, seq)
}

func putMap(m *yaml.Node, key string, sub *yaml.Node) {
	if len(sub.Content) > 0 {
		put(m, key, sub)
	}
}

func putExtra(m *yaml.Node, extra map[string]any) error {
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		n := &yaml.Node{}
		if err := n.Encode(extra[k]); err != nil {
			return fmt.Errorf("key %q: %w", k, err)
		}
		dateOnly(n)
		put(m, k, n)
	}
	return nil
}

// dateOnly restores the date an unknown key was written with: yaml decodes
// `2026-09-16` to a time and re-encodes it as midnight UTC.
func dateOnly(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!timestamp" {
		n.Value = strings.TrimSuffix(n.Value, "T00:00:00Z")
	}
	for _, c := range n.Content {
		dateOnly(c)
	}
}
