package gate

import (
	"slices"
	"strings"
	"testing"
)

func TestPathTokens(t *testing.T) {
	for _, c := range []struct {
		in     string
		tokens []string
		fields []string
	}{
		{in: "a,/x/y;~/z", tokens: []string{"/x/y", "~/z"}, fields: []string{"a", "/x/y", "~/z"}},
		{in: "--out=/x/y", tokens: []string{"/x/y"}, fields: []string{"--out", "/x/y"}},
		{in: "path=/x", tokens: []string{"/x"}, fields: []string{"path", "/x"}},
		{in: "-o/x/y", tokens: []string{"/x/y", "-o/x/y"}, fields: []string{"-o/x/y"}},
		{in: "build/ a/b", tokens: []string{"build/", "a/b"}, fields: []string{"build/", "a/b"}},
		{in: "A / B", tokens: []string{"/"}, fields: []string{"A", "/", "B"}},
		{in: "a b\nnotes", tokens: nil, fields: []string{"a", "b", "notes"}},
		{in: "x .attest", tokens: []string{".attest"}, fields: []string{"x", ".attest"}},
		{in: "", tokens: nil, fields: nil},
	} {
		if got := PathTokens(c.in); !slices.Equal(got, c.tokens) {
			t.Errorf("PathTokens(%q) = %q, want %q", c.in, got, c.tokens)
		}
		if got := PathFields(c.in); !slices.Equal(got, c.fields) {
			t.Errorf("PathFields(%q) = %q, want %q", c.in, got, c.fields)
		}
	}
	decoys := strings.Repeat("/x ", 300) + "/notes/MEMORY.md"
	got := PathTokens(decoys)
	if len(got) != 301 {
		t.Fatalf("PathTokens of 300 decoys gave %d tokens, want 301", len(got))
	}
	if got[300] != "/notes/MEMORY.md" {
		t.Errorf("PathTokens of 300 decoys ends %q", got[300])
	}
}
