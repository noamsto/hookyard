package fact

import (
	"bytes"
	"strings"
	"testing"
)

func factWith(meta string) string {
	return "---\nname: n\ndescription: d\nmetadata:\n" + meta + "---\n\nbody: confidence: reviewed\n"
}

func TestWithConfidenceSplicesOnlyTheScalar(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain",
			in:   factWith("  type: user\n  confidence: reviewed\n  scope: repo\n"),
			want: factWith("  type: user\n  confidence: proposed\n  scope: repo\n"),
		},
		{
			name: "double quoted",
			in:   factWith("  confidence: \"reviewed\"\n"),
			want: factWith("  confidence: proposed\n"),
		},
		{
			name: "single quoted",
			in:   factWith("  confidence: 'reviewed'\n"),
			want: factWith("  confidence: proposed\n"),
		},
		{
			name: "trailing comment",
			in:   factWith("  confidence: reviewed # note\n"),
			want: factWith("  confidence: proposed # note\n"),
		},
		{
			name: "flow mapping",
			in:   "---\nname: n\nmetadata: {type: user, confidence: reviewed}\n---\nb\n",
			want: "---\nname: n\nmetadata: {type: user, confidence: proposed}\n---\nb\n",
		},
		{
			name: "non-ASCII before the scalar on its line",
			in:   "---\nname: n\nmetadata: {type: \"é→\", confidence: reviewed}\n---\n",
			want: "---\nname: n\nmetadata: {type: \"é→\", confidence: proposed}\n---\n",
		},
		{
			name: "CRLF",
			in:   "---\r\nname: n\r\nmetadata:\r\n  type: user\r\n  confidence: reviewed\r\n  scope: repo\r\n---\r\nbody\r\n",
			want: "---\r\nname: n\r\nmetadata:\r\n  type: user\r\n  confidence: proposed\r\n  scope: repo\r\n---\r\nbody\r\n",
		},
		{
			name: "CRLF quoted with comment",
			in:   "---\r\nmetadata:\r\n  confidence: 'reviewed' # note\r\n---\r\n",
			want: "---\r\nmetadata:\r\n  confidence: proposed # note\r\n---\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := WithConfidence([]byte(tt.in), "proposed")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tt.want {
				t.Errorf("out = %q\nwant  %q", out, tt.want)
			}
			// Parse only reads LF files.
			f, err := Parse([]byte(strings.ReplaceAll(string(out), "\r\n", "\n")))
			if err != nil {
				t.Fatal(err)
			}
			if f.Metadata.Confidence != "proposed" {
				t.Errorf("Confidence = %q, want proposed", f.Metadata.Confidence)
			}
		})
	}
}

func TestWithConfidenceFallsBackToMarshalWhenSpanIsAmbiguous(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"escape in quoted scalar", factWith("  confidence: \"review\\u0065d\"\n")},
		{"explicit tag", factWith("  confidence: !!str reviewed\n")},
		{"alias", factWith("  base: &c reviewed\n  confidence: *c\n")},
		{"merged from alias", "---\nname: n\nbase: &b {confidence: reviewed}\nmetadata:\n  <<: *b\n---\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := WithConfidence([]byte(tt.in), "proposed")
			if err != nil {
				t.Fatal(err)
			}
			f, err := Parse(out)
			if err != nil {
				t.Fatal(err)
			}
			if f.Metadata.Confidence != "proposed" {
				t.Errorf("Confidence = %q, want proposed\n%s", f.Metadata.Confidence, out)
			}
		})
	}
}

func TestWithConfidenceReturnsRawWhenThereIsNoConfidence(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no key", factWith("  type: user\n")},
		{"null value", factWith("  confidence:\n")},
		{"no metadata", "---\nname: n\nconfidence: reviewed\n---\nbody\n"},
		{"empty frontmatter", "---\n---\nbody\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := WithConfidence([]byte(tt.in), "proposed")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, []byte(tt.in)) {
				t.Errorf("out = %q, want raw %q", out, tt.in)
			}
		})
	}
}

func TestWithConfidenceRejectsMalformedFrontmatter(t *testing.T) {
	for _, in := range []string{"no fence\n", "---\nname: n\n", "---\nname: [\n---\n"} {
		if _, err := WithConfidence([]byte(in), "proposed"); err == nil {
			t.Errorf("WithConfidence(%q) = nil error", in)
		}
	}
}
