package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
)

// sandbox is a home with a store at ~/notes, priors' state and config dirs, a
// project at ~/work and links into the store.
type sandbox struct {
	home, store, work string
	guard             *Guard
}

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sb := sandbox{home: home, store: filepath.Join(home, "notes"), work: filepath.Join(home, "work")}
	files := map[string]string{
		"notes/MEMORY.md":            "# index\n",
		"notes/.attest/e1":           "entry\n",
		"notes/.git/config":          "[core]\n",
		"notes/facts/a.md":           "---\nname: a\nmetadata:\n  confidence: rexxviewed\n---\nbody\n",
		"notes/facts/b.md":           "---\nname: b\nmetadata:\n  confidence: unverified\n---\nbody\n",
		"notes-old/MEMORY.md":        "",
		".local/state/priors/x":      "",
		".config/priors/config.toml": "",
		"work/a.md":                  "",
		"work/sub/.attestation/x":    "",
		"elsewhere/.keep":            "",
	}
	for name, content := range files {
		p := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"link":       sb.store,
		"work/rel":   "../notes/.attest",
		"work/chain": filepath.Join(home, "link"),
		"work/dang":  filepath.Join(sb.store, ".attest", "new"),
	} {
		if err := os.Symlink(target, filepath.Join(home, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(sb.store, "MEMORY.md"), filepath.Join(sb.work, "hard")); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "NOTES": sb.store}
	host := &gate.Host{
		Home: home,
		Env:  func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Users: func(name string) (string, bool) {
			return home, name == "alice"
		},
	}
	sb.guard = New(Paths{
		Stores: []Store{{"personal", sb.store}},
		State:  filepath.Join(home, ".local/state/priors"),
		Config: filepath.Join(home, ".config/priors/config.toml"),
	}, host)
	return sb
}

func bash(cmd string) string {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return string(b)
}

// input marshals v with @H replaced by the sandbox home in every string.
func (sb sandbox) input(v map[string]any) string {
	b, _ := json.Marshal(v)
	return strings.ReplaceAll(string(b), "@H", sb.home)
}

type call struct {
	name string
	// cwd is relative to the home; "" is ~/work.
	cwd   string
	tool  string
	input string
}

func (sb sandbox) run(t *testing.T, c call) string {
	t.Helper()
	cwd := sb.work
	if c.cwd != "" {
		cwd = filepath.Join(sb.home, c.cwd)
	}
	return sb.guard.Check(cwd, c.tool, json.RawMessage(strings.ReplaceAll(c.input, "@H", sb.home)))
}

func TestShellDenies(t *testing.T) {
	sb := newSandbox(t)
	for _, c := range []call{
		{name: "redirect into attest", input: bash("echo x > ~/notes/.attest/e2")},
		{name: "append to index", input: bash("echo x >> $HOME/notes/MEMORY.md")},
		{name: "fact by shell", input: bash(`printf x > "@H/notes/facts/a.md"`)},
		{name: "rm attest", input: bash("rm -rf ~/notes/.attest")},
		{name: "state dir", input: bash("echo x > ~/.local/state/priors/provenance/s1")},
		{name: "config file", input: bash("rm ~/.config/priors/config.toml")},
		{name: "ancestor of config", input: bash("rm -rf ~/.config")},
		{name: "ancestor of store", input: bash("rm -rf ~/*")},
		{name: "root glob", input: bash("rm -rf /*")},
		{name: "rm dot from home", cwd: ".", input: bash("rm -rf .")},
		{name: "var from env", input: bash("echo x > $NOTES/MEMORY.md")},
		{name: "tilde user", input: bash("echo x > ~alice/notes/MEMORY.md")},
		{name: "attest by name elsewhere", input: bash("touch /srv/other/.attest/e")},

		// Path traversal.
		{name: "dotdot", input: bash("echo x > ~/work/../notes/MEMORY.md")},
		{name: "dotdot and dot", input: bash("touch ~/work/./../notes/./facts/../.attest/e2")},
		{name: "dotdot relative", input: bash("echo x > ../notes/.attest/e2")},
		{name: "dotdot past symlink", input: bash("echo x > ~/work/rel/../MEMORY.md")},
		{name: "relative dotdot past symlink", input: bash("echo x > rel/../MEMORY.md")},
		{name: "proc root", input: bash("echo x > /proc/self/root@H/notes/MEMORY.md")},

		// Symlinked dirs.
		{name: "symlinked store", input: bash("echo x > ~/link/MEMORY.md")},
		{name: "relative symlink", input: bash("echo x > rel/e2")},
		{name: "symlink chain", input: bash("echo x > chain/.git/config")},
		{name: "dangling symlink", input: bash("echo x > dang")},
		{name: "symlinked store dir", input: bash("cp x ~/link/")},

		// Relative paths from a cwd in the store.
		{name: "cwd store index", cwd: "notes", input: bash("echo x > MEMORY.md")},
		{name: "cwd facts up", cwd: "notes/facts", input: bash("echo x > ../.attest/e2")},
		{name: "cwd store any write", cwd: "notes", input: bash("touch new.md")},
		{name: "cwd via symlink", cwd: "link", input: bash("touch new.md")},
		{name: "cd then write", input: bash("cd ~/notes && echo x > MEMORY.md")},
		{name: "cd relative", input: bash("cd ../notes; rm -rf .attest")},
		{name: "pushd", input: bash("pushd ~/notes >/dev/null; touch f")},
		{name: "git -C", input: bash("git -C ~/notes checkout -- .")},
		{name: "git work tree", input: bash("git --work-tree=../notes reset --hard")},
		{name: "make -C", input: bash("make -C ~/notes")},
		{name: "cd unknown then involved write", input: bash("cd $(mktemp -d); echo x > .attest/e")},
		{name: "tar -C ancestor", input: bash("tar -xf a.tar -C ~")},
		{name: "env -C", input: bash("env -C ~/notes touch x")},
		{name: "cd through symlink lookup", input: bash(`cd "$(ls -d ~/link)" && touch x`)},

		// Quoting and escaping.
		{name: "double quotes", input: bash(`echo x > ~/"notes"/MEMORY.md`)},
		{name: "split quotes", input: bash(`echo x > ~/'no'"tes"/.att''est/e`)},
		{name: "backslashes", input: bash(`echo x > ~/no\tes/MEMORY\.md`)},
		{name: "ansi c", input: bash(`echo x > ~/notes/$'\x2eattest'/e`)},
		{name: "braces", input: bash(`echo x | tee ~/notes/{a,.attest/b}`)},
		{name: "case folded token", input: bash(`echo x > "$(echo ~/NOTES/memory.md | tr A-Z a-z)"`)},
		{name: "assigned variable", input: bash("N=notes; echo x > ~/$N/MEMORY.md")},
		{name: "default expansion", input: bash("echo x > ${HOME:-/x}/notes/MEMORY.md")},
		{name: "option value", input: bash("cp --target-directory=~/notes/.attest x")},
		{name: "extglob", input: bash("shopt -s extglob; echo x > ~/notes/+(M)EMORY.md")},
		{name: "dot glob attest", input: bash("rm -rf ~/work/.a*")},

		// Wrappers.
		{name: "sudo tee", input: bash("echo x | sudo tee ~/notes/MEMORY.md")},
		{name: "env cp", input: bash("env X=1 cp a ~/notes/.attest/")},
		{name: "nice timeout mv", input: bash("nice -n 5 timeout 5 mv x ~/notes/.attest/y")},
		{name: "bash -c", input: bash(`bash -c 'echo x > ~/notes/MEMORY.md'`)},
		{name: "sh -c double", input: bash(`sh -c "rm -rf $HOME/notes/.attest"`)},
		{name: "eval", input: bash(`eval "rm ~/notes/MEMORY.md"`)},
		{name: "find delete", input: bash("find ~/notes -name '*.md' -delete")},
		{name: "sed -i", input: bash("sed -i s/a/b/ ~/notes/MEMORY.md")},
		{name: "sed w", input: bash("sed -n 'w /tmp/x' ~/notes/facts/a.md")},
		{name: "ln", input: bash("ln -sf /tmp/x ~/notes/MEMORY.md")},
		{name: "xargs involved", input: bash("echo ~/notes/MEMORY.md | xargs rm")},
		{name: "heredoc", input: bash("cat > ~/notes/x <<EOF\nhi\nEOF")},
		{name: "python", input: bash(`python3 -c "open('@H/notes/MEMORY.md','w')"`)},
		{name: "codex argv", input: `{"command":["bash","-lc","echo x > ~/notes/MEMORY.md"]}`},
		{name: "git plumbing", input: bash("git --git-dir=@H/notes/.git hash-object -w x")},
		{name: "dd", input: bash("dd if=/dev/zero of=@H/notes/MEMORY.md")},
		{name: "bare redirect", input: bash("> ~/notes/MEMORY.md")},
		{name: "group redirect", input: bash("{ echo x; } > ~/notes/MEMORY.md")},
		{name: "function body", input: bash("f() { echo x > ~/notes/MEMORY.md; }; f")},
		{name: "script piped to shell", input: bash(`echo "rm ~/notes/MEMORY.md" | bash`)},
		{name: "format piped to shell", input: bash(`printf 'rm %s/notes/MEMORY.md' ~ | sh`)},
		{name: "path piped to interpreter", input: bash(`echo ~/notes/MEMORY.md | python3 -c "import sys; open(sys.stdin.read().strip(),'w')"`)},
		{name: "symlink listing to xargs", input: bash("ls ~/link/ | xargs rm")},

		// Paths built at run time.
		{name: "opaque under store", input: bash("echo x > ~/notes/$(echo MEMORY.md)")},
		{name: "opaque partial element", input: bash("echo x > ~/no$(echo tes)/f")},
		{name: "opaque involved", input: bash(`echo x > "$(printf '%s/notes/MEMORY.md' ~)"`)},
		{name: "variable from substitution", input: bash("d=$(echo ~/notes); echo x > $d/MEMORY.md")},
		{name: "store name element", input: bash(`cp x "$(git rev-parse --show-toplevel)/../notes/f"`)},
		{name: "printf escaped token", input: bash(`echo x > "$(printf '\x2eattest/e')"`)},
		{name: "unparsable", input: bash("echo x > ~/notes/MEMORY.md; (")},

		// Hard links.
		{name: "hard link", input: bash("echo x > ~/work/hard")},

		{name: "malformed command", input: `{"command":5}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.tool == "" {
				c.tool = "Bash"
			}
			if sb.run(t, c) == "" {
				t.Errorf("allowed %s", c.input)
			}
		})
	}
}

func TestShellAllows(t *testing.T) {
	sb := newSandbox(t)
	for _, c := range []call{
		{name: "project write", input: bash("echo x > ~/work/notes.md")},
		{name: "sibling store name", input: bash("echo x > ~/notes-old/MEMORY.md")},
		{name: "store name prefix", input: bash("mkdir -p ~/notesx && echo x > ~/notesx/f")},
		{name: "attest prefix name", input: bash("touch ~/work/sub/.attestation/y")},
		{name: "index name elsewhere", input: bash("echo x > MEMORY.md")},
		{name: "cat store", input: bash("cat ~/notes/MEMORY.md")},
		{name: "grep store", input: bash("grep -r foo ~/notes | head")},
		{name: "git log store", input: bash("git -C ~/notes log --oneline")},
		{name: "ls attest", input: bash("ls -la ~/notes/.attest")},
		{name: "cd store and read", input: bash("cd ~/notes && git status && rg x")},
		{name: "cwd store read", cwd: "notes", input: bash("cat MEMORY.md > /dev/null 2>&1")},
		{name: "priors", cwd: "notes", input: bash("priors list")},
		{name: "cwd home", cwd: ".", input: bash("gh issue view 12")},
		{name: "cd home and write", input: bash("cd ~ && touch scratch")},
		{name: "opaque elsewhere", input: bash("echo x > ~/work/$(date +%s).log")},
		{name: "store name inside an element", input: bash(`cp x "$(git rev-parse --show-toplevel)/notesx/f"`)},
		{name: "globs in project", input: bash("rm -f *.o * && chmod +x *")},
		{name: "pipe to tee", input: bash("go test ./... 2>&1 | tee /tmp/out.txt")},
		{name: "read loop", input: bash(`while read -r f; do rm "$f"; done < /tmp/list`)},
		{name: "no command", input: `{}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.tool == "" {
				c.tool = "Bash"
			}
			if r := sb.run(t, c); r != "" {
				t.Errorf("denied %s: %s", c.input, r)
			}
		})
	}
}

func TestToolDenies(t *testing.T) {
	sb := newSandbox(t)
	reviewed := "---\nname: n\nmetadata:\n  confidence: reviewed\n---\nbody\n"
	for _, c := range []call{
		{name: "write attest", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/.attest/e2", "content": "x"})},
		{name: "write index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "content": "x"})},
		{name: "write git", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/.git/hooks/pre-commit", "content": "x"})},
		{name: "write state", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/.local/state/priors/provenance/s", "content": "x"})},
		{name: "write config dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/.config/priors/rules.toml", "content": "x"})},
		{name: "write ancestor", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes", "content": "x"})},
		{name: "symlinked dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/link/MEMORY.md", "content": "x"})},
		{name: "dangling symlink", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/dang", "content": "x"})},
		{name: "traversal", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/../notes/.attest/e3", "content": "x"})},
		{name: "relative in store", cwd: "notes", tool: "Write", input: sb.input(map[string]any{"file_path": "MEMORY.md", "content": "x"})},
		{name: "relative past symlink", tool: "Write", input: sb.input(map[string]any{"file_path": "rel/../MEMORY.md", "content": "x"})},
		{name: "hard link", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hard", "content": "x"})},
		{name: "edit index", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "old_string": "a", "new_string": "b"})},
		{name: "reviewed fact", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": reviewed})},
		{name: "reviewed yaml escape", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\nname: n\nmetadata:\n  confidence: \"re\\x76iewed\"\n---\n"})},
		{name: "reviewed unparsable", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\r\nconfidence: Reviewed\r\n"})},
		{name: "edit to reviewed", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "old_string": "unverified", "new_string": "reviewed"})},
		{name: "edit joins reviewed", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/a.md", "old_string": "xx", "new_string": ""})},
		{name: "multi edit", tool: "MultiEdit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"old_string": "unverified", "new_string": "re"},
			map[string]any{"old_string": "re\n", "new_string": "reviewed\n"},
		}})},
		{name: "pi edit", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"oldText": "unverified", "newText": "reviewed"},
		}})},
		{name: "symlinked edit", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/link/facts/b.md", "old_string": "unverified", "new_string": "reviewed"})},
		{name: "notebook in state", tool: "NotebookEdit", input: sb.input(map[string]any{"notebook_path": "@H/.local/state/priors/n.ipynb", "new_source": "x"})},
		{name: "unknown move tool", tool: "mcp__fs__move", input: sb.input(map[string]any{"source": "@H/notes/.attest/e1", "destination": "@H/work/e1"})},
		{name: "unknown tool command", tool: "mcp__exec", input: bash("echo x > ~/notes/MEMORY.md")},
		{name: "patch add attest", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Add File: @H/notes/.attest/e9\n+x\n*** End Patch"})},
		{name: "patch reviewed", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/b.md\n@@\n-  confidence: unverified\n+  confidence: reviewed\n*** End Patch"})},
		{name: "patch joins", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/a.md\n@@\n-  confidence: rexxviewed\n+  confidence: re\\\n*** End Patch"})},
		{name: "patch move into attest", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/work/a.md\n*** Move to: @H/notes/.attest/a\n*** End Patch"})},
	} {
		t.Run(c.name, func(t *testing.T) {
			if sb.run(t, c) == "" {
				t.Errorf("allowed %s %s", c.tool, c.input)
			}
		})
	}
}

func TestToolAllows(t *testing.T) {
	sb := newSandbox(t)
	for _, c := range []call{
		{name: "project write", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/b.md", "content": "confidence: reviewed"})},
		{name: "sibling store", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes-old/MEMORY.md", "content": "x"})},
		{name: "attestation dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/sub/.attestation/y", "content": "x"})},
		{name: "unreviewed fact", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\nname: n\nmetadata:\n  confidence: unverified\n---\nThe PR was reviewed.\n"})},
		{name: "benign edit", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "old_string": "body", "new_string": "more body"})},
		{name: "read attest", tool: "Read", input: sb.input(map[string]any{"file_path": "@H/notes/.attest/e1"})},
		{name: "grep store", tool: "Grep", input: sb.input(map[string]any{"pattern": "x", "path": "@H/notes"})},
		{name: "patch benign", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/b.md\n@@\n-body\n+more body\n*** End Patch"})},
		{name: "no path", tool: "WebFetch", input: sb.input(map[string]any{"url": "https://example.com"})},
	} {
		t.Run(c.name, func(t *testing.T) {
			if r := sb.run(t, c); r != "" {
				t.Errorf("denied %s %s: %s", c.tool, c.input, r)
			}
		})
	}
}

func TestBrokenDeniesWrites(t *testing.T) {
	sb := newSandbox(t)
	g := Broken(errors.New("trust file missing"), sb.guard.host)
	for _, tt := range []struct {
		tool, input string
		deny        bool
	}{
		{"Bash", bash("echo x > ~/work/f"), true},
		{"Bash", bash("ls ~/work"), false},
		{"Write", sb.input(map[string]any{"file_path": "@H/work/f", "content": "x"}), true},
		{"Read", sb.input(map[string]any{"file_path": "@H/work/a.md"}), false},
	} {
		r := g.Check(sb.work, tt.tool, json.RawMessage(tt.input))
		if (r != "") != tt.deny {
			t.Errorf("%s %s: got %q, want deny %v", tt.tool, tt.input, r, tt.deny)
		}
		if tt.deny && !strings.Contains(r, "trust file missing") {
			t.Errorf("%s %s: reason %q does not name the cause", tt.tool, tt.input, r)
		}
	}
}

func TestPhysical(t *testing.T) {
	sb := newSandbox(t)
	for in, want := range map[string]string{
		filepath.Join(sb.home, "link/MEMORY.md"): filepath.Join(sb.store, "MEMORY.md"),
		sb.work + "/rel/../MEMORY.md":            filepath.Join(sb.store, "MEMORY.md"),
		filepath.Join(sb.work, "chain/.attest"):  filepath.Join(sb.store, ".attest"),
		filepath.Join(sb.work, "dang"):           filepath.Join(sb.store, ".attest/new"),
		sb.work + "/missing/../a.md":             filepath.Join(sb.work, "a.md"),
	} {
		if got, ok := physical(in); !ok || got != want {
			t.Errorf("physical(%s) = %s, %v; want %s", in, got, ok, want)
		}
	}
	loop := filepath.Join(sb.work, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, ok := physical(loop); ok {
		t.Error("a symlink loop resolved")
	}
}
