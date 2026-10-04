package gate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every sample is assembled at runtime so no secret-shaped literal sits in
// the source for push protection or a scanner to trip on.
func ruleSamples() map[string]string {
	return map[string]string{
		"private-key":        "-----BEGIN " + "RSA PRIVATE" + " KEY-----",
		"aws-access-key-id":  "id: " + "AK" + "IA" + "IOSFODNN7EXAMPLE",
		"github-token":       "gh" + "p_" + strings.Repeat("a1B2", 9),
		"slack-token":        "xo" + "xb-" + strings.Repeat("1234567890", 2),
		"anthropic-key":      "sk-" + "ant-" + strings.Repeat("aB3_", 6),
		"openai-key":         "sk-" + "proj-" + strings.Repeat("Ab12", 6),
		"stripe-live-key":    "sk" + "_live_" + strings.Repeat("Ab12", 5),
		"google-api-key":     "AI" + "za" + strings.Repeat("Ab1_z", 7),
		"jwt":                "ey" + "JhbGciOiJIUzI1NiJ9." + "ey" + "JzdWIiOiIxMjM0In0." + strings.Repeat("sig", 4),
		"generic-assignment": "api_key" + " = " + strings.Repeat("Zx9", 6),
	}
}

func TestEmbeddedRulesMatchEachSampleByIDOnly(t *testing.T) {
	rules, err := LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	for id, sample := range ruleSamples() {
		t.Run(id, func(t *testing.T) {
			got := rules.Match("some prose " + sample + " more prose")
			if !slices.Equal(got, []string{id}) {
				t.Fatalf("Match = %v, want [%s]", got, id)
			}
			for _, g := range got {
				if strings.Contains(sample, g) || strings.Contains(g, sample) {
					t.Errorf("Match leaked sample text: %q", g)
				}
			}
		})
	}
}

func TestEmbeddedRulesIgnoreCleanText(t *testing.T) {
	rules, err := LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	clean := "Claude haiku dispatch workers stall on permission prompts; sonnet handles unattended trivial work."
	if got := rules.Match(clean); len(got) != 0 {
		t.Errorf("Match(clean) = %v, want none", got)
	}
}

func TestLoadRulesBuiltinsOnly(t *testing.T) {
	rules, err := LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(rules.rules), len(ruleSamples()); got != want {
		t.Errorf("LoadRules(\"\") has %d rules, want %d", got, want)
	}
}

func TestConfiguredRulesAddToBuiltins(t *testing.T) {
	aws := ruleSamples()["aws-access-key-id"]
	tests := []struct {
		name    string
		body    string
		text    string
		want    []string
		wantErr []string
	}{
		{
			name: "no-match rule leaves built-ins in force",
			body: "[[rule]]\nid = \"never\"\nregex = 'NEVERMATCHES[0-9]{40}'\n",
			text: "id: " + aws,
			want: []string{"aws-access-key-id"},
		},
		{
			name: "configured rule follows built-ins",
			body: "[[rule]]\nid = \"marker\"\nregex = 'MARK[0-9]+'\n",
			text: aws + " MARK42",
			want: []string{"aws-access-key-id", "marker"},
		},
		{
			name:    "built-in id reused",
			body:    "[[rule]]\nid = \"jwt\"\nregex = 'MARK[0-9]+'\n",
			wantErr: []string{"jwt", "duplicate"},
		},
		{
			name:    "id repeated in file",
			body:    "[[rule]]\nid = \"a\"\nregex = 'A'\n[[rule]]\nid = \"a\"\nregex = 'B'\n",
			wantErr: []string{"a", "duplicate"},
		},
		{
			name:    "unknown key",
			body:    "[[rule]]\nid = \"x\"\nregex = 'X'\nallow = [\"aws-access-key-id\"]\n",
			wantErr: []string{"allow"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "rules.toml")
			if err := os.WriteFile(p, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			rules, err := LoadRules(p)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatal("LoadRules succeeded, want error")
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q lacks %q", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := rules.Match(tt.text); !slices.Equal(got, tt.want) {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadRulesFromFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid", write("valid.toml", "[[rule]]\nid = \"marker\"\nregex = 'MARK[0-9]+'\n"), false},
		{"missing", filepath.Join(dir, "absent.toml"), true},
		{"unparsable", write("bad.toml", "[[rule]\nid = \n"), true},
		{"invalid regex", write("re.toml", "[[rule]]\nid = \"x\"\nregex = '(unclosed'\n"), true},
		{"empty rule list", write("empty.toml", "# no rules\n"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules, err := LoadRules(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("LoadRules succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := rules.Match("a MARK42 b"); !slices.Equal(got, []string{"marker"}) {
				t.Errorf("Match = %v, want [marker]", got)
			}
		})
	}
}
