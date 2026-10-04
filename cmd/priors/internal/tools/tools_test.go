package tools

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func script(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommand(t *testing.T) {
	for _, path := range []string{"", "git"} {
		err := Command(context.Background(), path).Run()
		if !errors.Is(err, ErrUnpinned) {
			t.Errorf("Command(%q).Run() = %v, want ErrUnpinned", path, err)
		}
	}
	if err := Command(context.Background(), script(t, 0o700)).Run(); err != nil {
		t.Errorf("absolute path: %v", err)
	}
}

func TestCheck(t *testing.T) {
	exe := script(t, 0o700)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"relative", "git", true},
		{"empty", "", true},
		{"missing", filepath.Join(t.TempDir(), "absent"), true},
		{"not executable", script(t, 0o600), true},
		{"directory", t.TempDir(), true},
		{"ok", exe, false},
		{"symlink", link, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Check(tt.path); (err != nil) != tt.wantErr {
				t.Errorf("Check(%q) = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestUnpinned(t *testing.T) {
	saved := [4]string{Git, SSH, Rg, Scanner}
	t.Cleanup(func() { Git, SSH, Rg, Scanner = saved[0], saved[1], saved[2], saved[3] })

	Git, SSH, Rg, Scanner = "", "ssh", "/abs/rg", ""
	got := strings.Join(Unpinned(), ",")
	if got != "git,ssh,scanner" {
		t.Errorf("Unpinned = %q, want git,ssh,scanner", got)
	}
	Git, SSH, Rg, Scanner = "/a/git", "/a/ssh", "/a/rg", "/a/scan"
	if got := Unpinned(); len(got) != 0 {
		t.Errorf("Unpinned = %v, want none", got)
	}
}

func TestNoPATHExec(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	toolsGo := filepath.Join(root, "internal", "tools", "tools.go")
	toolstestDir := filepath.Join(root, "internal", "tools", "toolstest")

	var sawToolsGo bool
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if path == toolsGo {
			sawToolsGo = true
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		local := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p != "os/exec" {
				continue
			}
			local = "exec"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" || local == "_" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		isExec := func(x ast.Expr) bool {
			id, ok := x.(*ast.Ident)
			return ok && id.Name == local
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if !isExec(n.X) {
					return true
				}
				switch n.Sel.Name {
				case "LookPath":
					if filepath.Dir(path) != toolstestDir {
						t.Errorf("%s: exec.LookPath outside toolstest", rel)
					}
				case "Command", "CommandContext":
					if path != toolsGo {
						t.Errorf("%s: exec.%s outside internal/tools/tools.go", rel, n.Sel.Name)
					}
				}
			case *ast.CompositeLit:
				if sel, ok := n.Type.(*ast.SelectorExpr); ok && isExec(sel.X) && sel.Sel.Name == "Cmd" {
					t.Errorf("%s: exec.Cmd literal", rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawToolsGo {
		t.Fatalf("walk never visited %s", toolsGo)
	}
}
