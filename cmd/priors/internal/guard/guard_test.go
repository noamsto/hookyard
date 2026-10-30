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
	// want is a piece of the deny reason that names the rule the case
	// exercises; the allow tables leave it empty.
	want string
	// cwd is relative to the home; "" is ~/work.
	cwd   string
	tool  string
	input string
}

// wantDenied fails t unless reason is a denial holding c.want.
func wantDenied(t *testing.T, c call, reason string) {
	t.Helper()
	switch {
	case reason == "":
		t.Errorf("allowed %s %s", c.tool, c.input)
	case !strings.Contains(reason, c.want):
		t.Errorf("denied %s %s by another rule\n got: %s\nwant: ...%s...", c.tool, c.input, reason, c.want)
	}
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
		{name: "redirect into attest", want: "/notes/.attest/e2, inside an .attest dir", input: bash("echo x > ~/notes/.attest/e2")},
		{name: "append to index", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x >> $HOME/notes/MEMORY.md")},
		{name: "fact by shell", want: "/notes/facts/a.md, inside the personal store", input: bash(`printf x > "@H/notes/facts/a.md"`)},
		{name: "rm attest", want: "/notes/.attest, an .attest dir", input: bash("rm -rf ~/notes/.attest")},
		{name: "state dir", want: "/provenance/s1, inside priors' state dir", input: bash("echo x > ~/.local/state/priors/provenance/s1")},
		{name: "config file", want: "/config.toml, inside priors' config dir", input: bash("rm ~/.config/priors/config.toml")},
		{name: "ancestor of config", want: "which holds priors' config dir", input: bash("rm -rf ~/.config")},
		{name: "ancestor of store", want: "/*, inside the personal store", input: bash("rm -rf ~/*")},
		{name: "root glob", want: "/*, which holds the personal store", input: bash("rm -rf /*")},
		{name: "rm dot from home", want: "which holds the personal store", cwd: ".", input: bash("rm -rf .")},
		{name: "var from env", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > $NOTES/MEMORY.md")},
		{name: "tilde user", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > ~alice/notes/MEMORY.md")},
		{name: "attest by name elsewhere", want: "/srv/other/.attest/e, inside an .attest dir", input: bash("touch /srv/other/.attest/e")},

		// Path traversal.
		{name: "dotdot", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > ~/work/../notes/MEMORY.md")},
		{name: "dotdot and dot", want: "/notes/facts/n.md, inside the personal store", input: bash("touch ~/work/./../notes/./facts/../facts/n.md")},
		{name: "dotdot relative", want: "/notes/facts/n.md, inside the personal store", input: bash("echo x > ../notes/facts/n.md")},
		{name: "dotdot past symlink", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > ~/work/rel/../MEMORY.md")},
		{name: "relative dotdot past symlink", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > rel/../MEMORY.md")},
		{name: "proc root", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > /proc/self/root@H/notes/MEMORY.md")},

		// Symlinked dirs.
		{name: "symlinked store", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > ~/link/MEMORY.md")},
		{name: "relative symlink", want: "/notes/.attest/e2, inside an .attest dir", input: bash("echo x > rel/e2")},
		{name: "symlink chain", want: "/notes/.git/config, inside the personal store", input: bash("echo x > chain/.git/config")},
		{name: "dangling symlink", want: "/notes/.attest/new, inside an .attest dir", input: bash("echo x > dang")},
		{name: "symlinked store dir", want: "/notes, the personal store", input: bash("cp x ~/link/")},

		// Relative paths from a cwd in the store.
		{name: "cwd store index", want: "and runs in", cwd: "notes", input: bash("echo x > MEMORY.md")},
		{name: "cwd store subdir", want: "and runs in", cwd: "notes/facts", input: bash("touch new.md")},
		{name: "cwd store any write", want: "and runs in", cwd: "notes", input: bash("touch new.md")},
		{name: "cwd via symlink", want: "and runs in", cwd: "link", input: bash("touch new.md")},
		{name: "cd then write", want: "and runs in", input: bash("cd ~/notes && echo x > MEMORY.md")},
		{name: "cd relative", want: "and runs in", input: bash("cd ../notes; rm -rf .attest")},
		{name: "pushd", want: "and runs in", input: bash("pushd ~/notes >/dev/null; touch f")},
		{name: "git -C", want: "and runs in", input: bash("git -C ~/notes checkout -- .")},
		{name: "git work tree", want: "and runs in", input: bash("git --work-tree=../notes reset --hard")},
		{name: "make -C", want: "and runs in", input: bash("make -C ~/notes")},
		{name: "cd unknown then involved write", want: "from a dir it changes into at run time", input: bash(`cd "$(git rev-parse --show-toplevel)"; echo x > .attest/e`)},
		{name: "tar -C ancestor", want: "which holds the personal store", input: bash("tar -xf a.tar -C ~")},
		{name: "env -C", want: "/notes, the personal store, and may write", input: bash("env -C ~/notes touch x")},
		{name: "cd through symlink lookup", want: "/notes, the personal store, and may write", input: bash(`cd "$(ls -d ~/link)" && touch x`)},

		// Quoting and escaping.
		{name: "double quotes", want: "/notes/MEMORY.md, inside the personal store", input: bash(`echo x > ~/"notes"/MEMORY.md`)},
		{name: "split quotes", want: "/notes/.attest/e, inside an .attest dir", input: bash(`echo x > ~/'no'"tes"/.att''est/e`)},
		{name: "backslashes", want: "/notes/MEMORY.md, inside the personal store", input: bash(`echo x > ~/no\tes/MEMORY\.md`)},
		{name: "ansi c", want: "/work/.attest/e, inside an .attest dir", input: bash(`echo x > ~/work/$'\x2eattest'/e`)},
		{name: "braces", want: "/notes/facts/n.md, inside the personal store", input: bash("echo x | tee ~/{work,notes}/facts/n.md")},
		{name: "case folded token", want: "builds at run time", input: bash(`echo x > "$(echo ~/NOTES/memory.md | tr A-Z a-z)"`)},
		{name: "assigned variable", want: "/notes/MEMORY.md, inside the personal store", input: bash("N=notes; echo x > ~/$N/MEMORY.md")},
		{name: "default expansion", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > ${HOME:-/x}/notes/MEMORY.md")},
		{name: "option value", want: "/notes/facts, inside the personal store", input: bash("cp --target-directory=~/notes/facts x")},
		{name: "extglob", want: "/+(no)tes/f, inside the personal store", input: bash("shopt -s extglob; echo x > ~/+(no)tes/f")},
		{name: "dot glob attest", want: "/work/.a*, an .attest dir", input: bash("rm -rf ~/work/.a*")},

		// Wrappers.
		{name: "sudo tee", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x | sudo tee ~/notes/MEMORY.md")},
		{name: "env cp", want: "/notes/.attest, an .attest dir", input: bash("env X=1 cp a ~/notes/.attest/")},
		{name: "nice timeout mv", want: "/notes/.attest/y, inside an .attest dir", input: bash("nice -n 5 timeout 5 mv x ~/notes/.attest/y")},
		{name: "bash -c", want: "/notes/MEMORY.md, inside the personal store", input: bash(`bash -c 'echo x > ~/notes/MEMORY.md'`)},
		{name: "sh -c double", want: "/notes/facts, inside the personal store", input: bash(`sh -c "rm -rf $HOME/notes/facts"`)},
		{name: "eval", want: "/notes/MEMORY.md, inside the personal store", input: bash(`eval "rm ~/notes/MEMORY.md"`)},
		{name: "find delete", want: "/notes, the personal store", input: bash("find ~/notes -name '*.md' -delete")},
		{name: "sed -i", want: "/notes/MEMORY.md, inside the personal store", input: bash("sed -i s/a/b/ ~/notes/MEMORY.md")},
		{name: "sed w", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -n 'w @H/notes/facts/a.md' /tmp/x")},
		{name: "ln", want: "/notes/MEMORY.md, inside the personal store", input: bash("ln -sf /tmp/x ~/notes/MEMORY.md")},
		{name: "xargs involved", want: "/notes/MEMORY.md, inside the personal store, and may write", input: bash("echo ~/notes/MEMORY.md | xargs rm")},
		{name: "heredoc", want: "/notes/x, inside the personal store", input: bash("cat > ~/notes/x <<EOF\nhi\nEOF")},
		{name: "python", want: "/notes/MEMORY.md, inside the personal store", input: bash(`python3 -c "open('@H/notes/MEMORY.md','w')"`)},
		{name: "codex argv", want: "/notes/MEMORY.md, inside the personal store", input: `{"command":["bash","-lc","echo x > ~/notes/MEMORY.md"]}`},
		{name: "git plumbing", want: "and runs in", input: bash("git --git-dir=@H/notes/.git hash-object -w x")},
		{name: "dd", want: "/notes/MEMORY.md, inside the personal store", input: bash("dd if=/dev/zero of=@H/notes/MEMORY.md")},
		{name: "bare redirect", want: "/notes/MEMORY.md, inside the personal store", input: bash("> ~/notes/MEMORY.md")},
		{name: "group redirect", want: "/notes/MEMORY.md, inside the personal store", input: bash("{ echo x; } > ~/notes/MEMORY.md")},
		{name: "function body", want: "/notes/MEMORY.md, inside the personal store", input: bash("f() { echo x > ~/notes/MEMORY.md; }; f")},
		{name: "script piped to shell", want: "inside the personal store, and may write", input: bash(`echo "rm ~/notes/MEMORY.md" | bash`)},
		{name: "format piped to shell", want: "builds at run time", input: bash(`printf 'rm %s/notes/MEMORY.md' ~ | sh`)},
		{name: "path piped to interpreter", want: "inside the personal store, and may write", input: bash(`echo ~/notes/MEMORY.md | python3 -c "import sys; open(sys.stdin.read().strip(),'w')"`)},
		{name: "symlink listing to xargs", want: "/notes, the personal store, and may write", input: bash("ls ~/link/ | xargs rm")},

		// Paths built at run time.
		{name: "opaque under store", want: "notes/…\", which reaches into the personal store", input: bash("echo x > ~/notes/$(echo MEMORY.md)")},
		{name: "opaque partial element", want: "/no…\", which reaches into the personal store", input: bash("echo x > ~/no$(echo tes)/f")},
		{name: "opaque involved", want: "builds at run time", input: bash(`echo x > "$(printf '%s/notes/MEMORY.md' ~)"`)},
		{name: "variable from substitution", want: "builds at run time", input: bash("d=$(git rev-parse --show-toplevel); echo x > $d/MEMORY.md")},
		{name: "store name element", want: "builds at run time", input: bash(`cp x "$(git rev-parse --show-toplevel)/../notes/f"`)},
		{name: "printf escaped token", want: "builds at run time", input: bash(`echo x > "$(printf '\x2eattest/e')"`)},
		{name: "unparsable", want: "builds at run time", input: bash("( ; echo x > ~/notes/facts/a.md")},

		// Hard links.
		{name: "hard link", want: "a hard link to", input: bash("echo x > ~/work/hard")},

		// Input the guard cannot read.
		{name: "malformed command", want: "neither a string nor a list of strings", input: `{"command":5}`},
		{name: "oversized command", want: "past what the guard reads", input: bash("echo x > ~/work/" + strings.Repeat("a", 1<<18))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.tool == "" {
				c.tool = "Bash"
			}
			wantDenied(t, c, sb.run(t, c))
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
		{name: "write attest", want: "/notes/.attest/e2, inside an .attest dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/.attest/e2", "content": "x"})},
		{name: "write index", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "content": "x"})},
		{name: "write git", want: "/hooks/pre-commit, inside the personal store's git dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/.git/hooks/pre-commit", "content": "x"})},
		{name: "write state", want: "/provenance/s, inside priors' state dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/.local/state/priors/provenance/s", "content": "x"})},
		{name: "write config dir", want: "/rules.toml, inside priors' config dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/.config/priors/rules.toml", "content": "x"})},
		{name: "write ancestor", want: "which holds the personal store's attest entries", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes", "content": "x"})},
		{name: "symlinked dir", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/link/MEMORY.md", "content": "x"})},
		{name: "dangling symlink", want: "/notes/.attest/new, inside an .attest dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/dang", "content": "x"})},
		{name: "traversal", want: "/notes/.git/config, inside the personal store's git dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/../notes/.git/config", "content": "x"})},
		{name: "relative in store", want: "/notes/MEMORY.md, the personal store's generated index", cwd: "notes", tool: "Write", input: sb.input(map[string]any{"file_path": "MEMORY.md", "content": "x"})},
		{name: "relative past symlink", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "rel/../MEMORY.md", "content": "x"})},
		{name: "hard link", want: "a hard link to", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hard", "content": "x"})},
		{name: "edit index", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "old_string": "a", "new_string": "b"})},
		{name: "reviewed fact", want: "/facts/n.md claiming confidence: reviewed", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": reviewed})},
		{name: "reviewed yaml escape", want: "/facts/n.md claiming confidence: reviewed", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\nname: n\nmetadata:\n  confidence: \"re\\x76iewed\"\n---\n"})},
		{name: "reviewed unparsable", want: "/facts/n.md claiming confidence: reviewed", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\r\nconfidence: Reviewed\r\n"})},
		{name: "edit to reviewed", want: "/facts/b.md claiming confidence: reviewed", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "old_string": "unverified", "new_string": "reviewed"})},
		{name: "edit joins reviewed", want: "/facts/a.md claiming confidence: reviewed", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/a.md", "old_string": "xx", "new_string": ""})},
		{name: "multi edit", want: "/facts/b.md claiming confidence: reviewed", tool: "MultiEdit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"old_string": "unverified", "new_string": "re"},
			map[string]any{"old_string": "re\n", "new_string": "reviewed\n"},
		}})},
		{name: "pi edit", want: "/facts/b.md claiming confidence: reviewed", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"oldText": "unverified", "newText": "reviewed"},
		}})},
		{name: "symlinked edit", want: "/facts/b.md claiming confidence: reviewed", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/link/facts/b.md", "old_string": "unverified", "new_string": "reviewed"})},
		{name: "notebook in state", want: "/n.ipynb, inside priors' state dir", tool: "NotebookEdit", input: sb.input(map[string]any{"notebook_path": "@H/.local/state/priors/n.ipynb", "new_source": "x"})},
		{name: "unknown move tool", want: "/notes/.attest/e1, inside an .attest dir", tool: "mcp__fs__move", input: sb.input(map[string]any{"source": "@H/notes/.attest/e1", "destination": "@H/work/e1"})},
		{name: "unknown tool command", want: "the command writes", tool: "mcp__exec", input: bash("echo x > ~/notes/MEMORY.md")},
		{name: "patch add attest", want: "/notes/.attest/e9, inside an .attest dir", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Add File: @H/notes/.attest/e9\n+x\n*** End Patch"})},
		{name: "patch reviewed", want: "/facts/b.md claiming confidence: reviewed", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/b.md\n@@\n-  confidence: unverified\n+  confidence: reviewed\n*** End Patch"})},
		{name: "patch joins", want: "/facts/a.md claiming confidence: reviewed", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/a.md\n@@\n-  confidence: rexxviewed\n+  confidence: re\\\n*** End Patch"})},
		{name: "patch move into attest", want: "/notes/.attest/a, inside an .attest dir", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/work/a.md\n*** Move to: @H/notes/.attest/a\n*** End Patch"})},

		// Input the guard cannot read.
		{name: "invalid json", want: "input cannot be read", tool: "Write", input: `{"file_path": `},
		{name: "deeply nested input", want: "input is past what the guard reads", tool: "Write", input: strings.Repeat("[", 20) + strings.Repeat("]", 20)},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantDenied(t, c, sb.run(t, c))
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
