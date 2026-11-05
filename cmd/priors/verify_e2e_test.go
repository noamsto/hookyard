package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// verdictTree lists every path under root with its mode and content.
func verdictTree(t *testing.T, root string) []string {
	t.Helper()
	var tree []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := path + " " + info.Mode().String()
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += " " + string(data)
		}
		tree = append(tree, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestVerifyOffSeparateHostWritesNothing(t *testing.T) {
	for name, opts := range map[string]trustOpts{
		"absent":      {},
		"owner-admin": {root: "owner-admin"},
		"bogus":       {root: "bogus"},
		"non-string":  {extra: []string{"trust_root = true"}},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newSandbox(t, "personal")
			sb.writeTrustWith(opts)
			if err := os.MkdirAll(filepath.Join(sb.verdicts, "personal-test"), 0o755); err != nil {
				t.Fatal(err)
			}
			before := verdictTree(t, sb.verdicts)

			res := sb.run("", "verify", "--store", "personal-test", "--credential", "/nonexistent/key")
			if res.code == 0 {
				t.Errorf("verify exit 0, stdout %q", res.stdout)
			}
			wantContains(t, "verify stderr", res.stderr, "attestation off: trust_root")
			if after := verdictTree(t, sb.verdicts); !slices.Equal(before, after) {
				t.Errorf("verdict tree changed:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}
