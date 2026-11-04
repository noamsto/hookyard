package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
)

// sandbox is a home with a store at ~/notes, priors' state dir, a config file
// that links to ~/dotfiles/priors.toml, a project at ~/work and links into the
// store: symlinks, and hard links to the index, a fact, the git config and a
// git object and the config target.
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
		"notes/.git/objects/ab/cdef": "object",
		"notes/facts/a.md":           "---\nname: a\nmetadata:\n  confidence: rexxviewed\n---\nbody\n",
		"notes/facts/b.md":           "---\nname: b\nmetadata:\n  confidence: unverified\n---\nbody\n",
		"notes/facts/c.md":           "---\nname: c\nmetadata:\n  confidence: unverified\n---\nThe PR was reviewed.\n",
		"notes/facts/f.md":           "---\nname: f\nmetadata:\n  confidence: re\u201cx\u201dviewed\n---\nbody\n",
		"notes/facts/s.md":           "---\nname: s\nmetadata:\n  confidence: X viewed\n---\nfo re\n",
		"notes-old/MEMORY.md":        "",
		".local/state/priors/x":      "",
		"dotfiles/priors.toml":       "",
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
		"work/lk":    home,
		"work/P":     filepath.Join(sb.store, "MEMORY.md"),

		".config/priors/config.toml": filepath.Join(home, "dotfiles/priors.toml"),
	} {
		p := filepath.Join(home, link)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"hard": "MEMORY.md", "hardfact": "facts/b.md", "hardgit": ".git/config", "hobj": ".git/objects/ab/cdef", "hcfg": "../dotfiles/priors.toml"} {
		if err := os.Link(filepath.Join(sb.store, target), filepath.Join(sb.work, link)); err != nil {
			t.Fatal(err)
		}
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
	// linuxOnly skips a case that needs a path only Linux resolves, such as
	// /proc/self/root.
	linuxOnly bool
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
		{name: "proc root", linuxOnly: true, want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > /proc/self/root@H/notes/MEMORY.md")},
		{name: "dotdot cleaned before symlink", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > ~/work/lk/../lk/notes/MEMORY.md")},

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
		{name: "env -C", want: "runs in", input: bash("env -C ~/notes touch x")},
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
		{name: "negated class rm", want: "/[!q]otes, inside the personal store", input: bash("rm -rf ~/[!q]otes")},
		{name: "negated class redirect", want: "/[!q]otes/MEMORY.md, inside the personal store", input: bash("echo x > ~/[!q]otes/MEMORY.md")},

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

		// Options and wrappers that read as something else.
		{name: "env unset option", want: "/notes, the personal store", input: bash("env -u cat rm -rf ~/notes")},
		{name: "exec name option", want: "inside the personal store", input: bash(`exec -a cat bash -c 'echo x > ~/notes/facts/a.md'`)},
		{name: "xargs replace option", want: "/notes, the personal store, and may write", input: bash("echo ~/notes | xargs -I cat rm -rf ~/notes")},
		{name: "xargs replace option alone", want: "builds at run time", input: bash("xargs -I cat rm -rf ~/notes")},
		{name: "git -C abbreviated option", want: "and runs in", input: bash("git -C ~/notes --attr-source log reset --hard")},
		{name: "sed abbreviated in-place", want: "inside the personal store", input: bash("sed --in-pl s/a/b/ ~/notes/facts/a.md")},
		{name: "sed expression option", want: "inside the personal store", input: bash("sed --expr='w @H/notes/facts/x' ~/work/a.md")},
		{name: "sed clustered expression", want: "inside the personal store", input: bash("sed -ne'w @H/notes/facts/x' ~/work/a.md")},
		{name: "git grep abbreviated open-files", want: "inside the personal store", input: bash("git grep --open='touch ~/notes/MEMORY.md;:' x")},
		{name: "git fetch abbreviated upload-pack", want: "and runs in", input: bash("git -C ~/notes fetch --upload=x origin")},
		{name: "git grep clustered open-files", want: "and runs in", input: bash("git -C ~/notes grep -iOx y")},
		{name: "fd clustered exec", want: "/notes, the personal store", input: bash("fd . ~/notes -Hx touch")},
		{name: "cdpath cd", want: "from a dir it changes into at run time", input: bash("CDPATH=~ cd notes && touch x")},
		{name: "exported PATH", want: "inside the personal store, and may write", input: bash("export PATH=/tmp/evil:$PATH; cat ~/notes/MEMORY.md")},
		{name: "exported BASH_ENV", want: "inside the personal store, and may write", input: bash("export BASH_ENV=/tmp/x; cat ~/notes/MEMORY.md")},
		{name: "printf -v PATH", want: "inside the personal store, and may write", input: bash("printf -v PATH /tmp/evil; cat ~/notes/MEMORY.md")},
		{name: "path in a commit message substitution", want: "builds at run time", input: bash("git commit -m \"$(rm $(echo ~/notes/MEMORY.md))\"")},

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
		{name: "hard link to a fact", want: "a hard link to", input: bash("echo x > ~/work/hardfact")},
		{name: "hard link to git config", want: "a hard link to", input: bash("echo x > ~/work/hardgit")},
		{name: "hard link to a git object", want: "a hard link to", input: bash("echo y > ~/work/hobj")},
		{name: "hard link to the config target", want: "a hard link to", input: bash("echo y > ~/work/hcfg")},

		// Config behind a symlink.
		{name: "config symlink target", want: "/dotfiles/priors.toml, priors' config file", input: bash("rm ~/dotfiles/priors.toml")},

		// Writes of a dir holding a store, which stay denied.
		{name: "known writer on dot from home", want: "which holds the personal store", cwd: ".", input: bash(`cp x .`)},
		{name: "unknown wrapper naming a writer", want: "which holds", cwd: ".", input: bash(`watch /bin/rm -rf .`)},
		{name: "g-prefixed rm on dot from home", want: "which holds", cwd: ".", input: bash(`grm -rf .`)},
		{name: "unknown program on home", want: "which holds", input: bash(`foo ~`)},
		{name: "unknown program on parent", want: "which holds", cwd: "notes-old", input: bash(`foo ..`)},
		{name: "interpreter on dot from home", want: "which holds", cwd: ".", input: bash(`python3 x.py .`)},
		{name: "awk code on dot from home", want: "which holds", cwd: ".", input: bash(`awk 'BEGIN{system("rm -rf ./")}'`)},
		{name: "php code on dot from home", want: "which holds", cwd: ".", input: bash(`php -r 'system("rm -rf ./");'`)},
		{name: "known writer on root", want: "which holds", input: bash(`rm -rf /`)},
		{name: "bracket with a leading close attest", want: "an .attest dir", input: bash(`rm -rf ~/work/.att[]e]st`)},
		{name: "negated bracket with a leading close attest", want: "an .attest dir", input: bash(`rm -rf ~/work/.atte[!]x]t`)},
		{name: "quoted glob piped to shell", want: "an .attest dir", input: bash(`echo 'rm -rf ~/work/.a*' | bash`)},
		{name: "function renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`z() { rm -rf "$1"; }; z .`)},
		{name: "function defined after its use", want: "which holds the personal store", cwd: ".", input: bash(`trap 'z .' EXIT; z() { rm -rf "$1"; }`)},
		{name: "alias renaming a writer", want: "which holds the personal store", cwd: ".", input: bash("shopt -s expand_aliases\nalias z=rm\nz -rf .")},
		{name: "hash renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`hash -p /bin/rm z; z -rf .`)},
		{name: "opaque writer name", want: "which holds the personal store", cwd: ".", input: bash(`busybox $(printf rm) -rf .`)},
		{name: "opaque command word", want: "which holds the personal store", cwd: ".", input: bash(`$(printf rm) -rf .`)},
		{name: "upper-case writer", want: "which holds the personal store", cwd: ".", input: bash(`RM -rf .`)},
		{name: "versioned interpreter", want: "which holds the personal store", cwd: ".", input: bash(`python3.12 x.py .`)},
		{name: "unparsable string naming a writer", want: "which holds the personal store", cwd: ".", input: bash(`tmux new-window 'find ./ -delete (true)'`)},
		{name: "unparsable remote string naming a writer", want: "which holds the personal store", cwd: ".", input: bash(`ssh localhost 'chmod -R 000 ./ (true)'`)},
		{name: "unparsable watch string", want: "which holds the personal store", cwd: ".", input: bash(`watch 'rm -rf ./ ('`)},
		{name: "unknown program on dot from home", want: "which holds the personal store", cwd: ".", input: bash(`foo .`)},
		{name: "BASH_CMDS renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`BASH_CMDS[z]=/bin/rm; z -rf .`)},
		{name: "PATH renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`PATH=~/evil:$PATH; z -rf .`)},
		{name: "symlink renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`ln -s /bin/rm ~/bin/z; z -rf .`)},
		{name: "mixed-case writer", want: "which holds the personal store", cwd: ".", input: bash(`Rm -rf .`)},
		{name: "wrapped upper-case writer", want: "which holds the personal store", cwd: ".", input: bash(`caffeinate RM -rf .`)},
		{name: "gdb shell string with a split name", want: "which holds the personal store", cwd: ".", input: bash(`gdb -batch -ex 'shell r""m -rf ./ (true)'`)},
		{name: "env unset before a renamed writer", want: "which holds the personal store", cwd: ".", input: bash(`env -u X z -rf .`)},
		{name: "env unset before claude on dot", want: "which holds the personal store", cwd: ".", input: bash(`env -u X claude .`)},
		{name: "claude mcp command on dot", want: "which holds the personal store", cwd: ".", input: bash(`claude mcp add x -- rm -rf .`)},
		{name: "claude mcp quoted command on dot", want: "which holds the personal store", cwd: ".", input: bash(`claude mcp add x -- sh -c 'rm -rf . && true'`)},
		{name: "claude double dash quoted command on dot", want: "which holds the personal store", cwd: ".", input: bash(`claude -p x -- sh -c 'rm -rf . && true'`)},
		{name: "unknown program on a spaced slash", want: "which holds", input: bash(`foo "a / b"`)},
		{name: "claude argument substitution", want: "which holds the personal store", cwd: ".", input: bash(`claude -p "x" "$(rm -rf .)"`)},

		// Data-position allowlist probes: a known data-taker renamed, or its
		// resolution reached through a flag a wrapper value could impersonate,
		// keeps its writer semantics.
		{name: "env chdir and unset before a renamed writer", want: "which holds the personal store", cwd: ".", input: bash(`env -C d -u X z -rf .`)},
		{name: "env split string runs a crew body", want: "which holds the personal store", input: bash(`env -S 'crew reply x /'`)},
		{name: "path env unset before claude", want: "which holds the personal store", cwd: ".", input: bash(`/x/env -u X claude -p .`)},
		{name: "exec name before claude", want: "which holds the personal store", cwd: ".", input: bash(`exec -a claude rm -rf .`)},
		{name: "sudo user before claude", want: "which holds the personal store", cwd: ".", input: bash(`sudo -u bob claude -p .`)},
		{name: "path env before claude on store", want: "the personal store", input: bash(`/x/env claude -p ~/notes`)},
		{name: "installed wrapper before claude", want: "the personal store", input: bash(`cp /bin/cp ./nice; ./nice claude -p ~/notes`)},
		{name: "renamed tool after env number", want: "/notes/MEMORY.md, inside the personal store", cwd: ".", input: bash(`cp /usr/bin/tee ~/.local/bin/5; env 5 claude -p ~/notes/MEMORY.md <<< x`)},
		{name: "quoted assignment is a program name for claude", want: "which holds the personal store", cwd: ".", input: bash(`"FOO=bar" claude -p .`)},
		{name: "quoted assignment is a program name for gh", want: "which holds the personal store", cwd: ".", input: bash(`"FOO=bar" gh api x -q /`)},
		{name: "claude settings is executed", want: "/notes/MEMORY.md, inside the personal store", input: bash(`claude --settings @H/notes/MEMORY.md`)},
		{name: "claude mcp config is executed", want: "/notes/MEMORY.md, inside the personal store", input: bash(`claude --mcp-config @H/notes/MEMORY.md`)},
		{name: "claude print not first", want: "/notes/MEMORY.md, inside the personal store", input: bash(`claude --mcp-config -p @H/notes/MEMORY.md`)},
		{name: "claude debug file is a path", want: "/notes/x, inside the personal store", input: bash(`claude --debug-file @H/notes/x`)},
		{name: "claude add dir is a path", want: "the personal store", input: bash(`claude --add-dir ~/notes -p x`)},
		{name: "claude prompt substitution", want: "which holds the personal store", cwd: ".", input: bash(`claude -p "$(rm -rf .)"`)},
		{name: "gh unknown subcommand q", want: "which holds the personal store", cwd: ".", input: bash(`gh my-ext -q /`)},
		{name: "gh path after double dash", want: "/notes/MEMORY.md, inside the personal store", input: bash(`gh api x -q / -- cat ~/notes/MEMORY.md`)},
		{name: "gh alias subcommand q", want: "which holds the personal store", cwd: ".", input: bash(`gh alias set x -q /`)},
		{name: "crew body substitution", want: "which holds the personal store", cwd: ".", input: bash(`crew reply x "$(rm -rf .)"`)},
		{name: "alias renaming claude", want: "which holds the personal store", cwd: ".", input: bash(`alias claude=rm; claude -p .`)},
		{name: "function renaming claude", want: "which holds the personal store", cwd: ".", input: bash(`claude() { rm -rf "$1"; }; claude -p /`)},
		{name: "BASH_CMDS renaming gh", want: "which holds the personal store", cwd: ".", input: bash(`BASH_CMDS[gh]=/bin/rm; gh api x -q /`)},
		{name: "printf -v BASH_CMDS renaming crew", want: "/notes/MEMORY.md, inside the personal store", input: bash(`printf -v 'BASH_CMDS[crew]' %s /bin/rm; crew reply x ~/notes/MEMORY.md`)},
		{name: "quote-split declare BASH_CMDS", want: "which holds the personal store", cwd: ".", input: bash(`declare "BASH_CMD""S[zzq]=/bin/rm"; zzq -rf .`)},
		{name: "quote-split printf -v BASH_CMDS", want: "which holds the personal store", cwd: ".", input: bash(`printf -v "BASH_CMD"S"[zzr]" %s /bin/rm; zzr -rf .`)},
		{name: "hash renaming crew", want: "which holds the personal store", cwd: ".", input: bash(`hash -p /bin/rm crew; crew reply x .`)},
		{name: "PATH before crew body", want: "which holds the personal store", input: bash(`PATH=~/evil:$PATH; crew reply x "a / b"`)},
		{name: "symlink renaming crew", want: "which holds the personal store", cwd: ".", input: bash(`ln -sf /bin/rm ~/bin/crew; crew reply x .`)},
		{name: "copy renaming gh", want: "which holds the personal store", cwd: ".", input: bash(`cp /bin/rm ~/bin/gh; gh api x -q /`)},
		{name: "redirect renaming crew", want: "/notes/MEMORY.md, inside the personal store", input: bash(`cat /bin/rm > ~/bin/crew; chmod +x ~/bin/crew; crew reply x ~/notes/MEMORY.md`)},
		{name: "opaque rename target for crew", want: "which holds the personal store", cwd: ".", input: bash(`cp /bin/rm ~/bin/"$(printf crew)"; crew reply x .`)},
		{name: "trap body runs a renamed crew", want: "which holds the personal store", cwd: ".", input: bash(`trap 'crew reply x .' EXIT; alias crew=rm`)},

		// Finite for-loop variables.
		{name: "loop value as a write target", want: "an .attest dir", cwd: ".", input: bash(`for f in a .attest/x; do touch "$f"; done`)},
		{name: "loop value as the command word", want: "inside the personal store", input: bash(`for c in rm cat; do $c ~/notes/MEMORY.md; done`)},
		{name: "printf -v before a subshell loop", want: "which holds the personal store", cwd: ".", input: bash(`printf -v n %s%s cr ew; (for n in 1 2; do :; done); cp /bin/rm ~/bin/$n; crew reply x .`)},
		{name: "indirect arithmetic default after a skipped loop", want: "which holds the personal store", cwd: ".", input: bash(`read -r x < f; false && for n in a b; do :; done; (( ${!x:=rew} )); cp /bin/rm ~/bin/c$n; crew reply x .`)},
		{name: "assignment index after a skipped loop", want: "which holds the personal store", cwd: ".", input: bash(`read -r x < f; false && for n in a b; do :; done; a[x]=1; cp /bin/rm ~/bin/c$n; crew reply x .`)},
		{name: "read of a glob-named variable after a skipped loop", want: "which holds the personal store", cwd: ".", input: bash(`false && for n in a b; do :; done; read ? < f; cp /bin/rm ~/bin/c$n; crew reply x .`)},
		{name: "xtrace prompt default after a skipped loop", want: "which holds the personal store", cwd: ".", input: bash(`read -r x < f; false && for n in a b; do :; done; PS4='${!x:=rew}'; set -x; :; set +x; cp /bin/rm ~/bin/c$n; crew reply x .`)},

		// sed scripts read as programs.
		{name: "sed s w flag with e", want: "inside the personal store", input: bash(`sed 's/e/x/w @H/notes/facts/a.md' f`)},
		{name: "sed e command", want: "runs in", cwd: "notes", input: bash(`sed '1e touch x' f`)},
		{name: "sed bracket hides delimiter then w", want: "inside the personal store", input: bash(`sed 's/[/]/x/w @H/notes/x' f`)},
		{name: "sed bracket backslash then w", want: "inside the personal store", input: bash(`sed 's/[\]/x/w @H/notes/x` + "\n" + `#]/y/' f`)},
		{name: "sed bracket close first then w", want: "inside the personal store", input: bash(`sed 's/[]/]/x/w @H/notes/x' f`)},
		{name: "sed label then w", want: "inside the personal store", input: bash(`sed ':a;w @H/notes/x' f`)},
		{name: "sed label space then w", want: "inside the personal store", input: bash(`sed ':x w @H/notes/x' f`)},
		{name: "sed branch space then w", want: "inside the personal store", input: bash(`sed 'b x w @H/notes/x` + "\n" + `:x' f`)},
		{name: "sed label hides continued w on bsd", want: "an .attest dir", input: bash(`sed ':x;/\` + "\n" + `w @H/notes/.attest/e\` + "\n" + `/p' f`)},
		{name: "sed append text", want: "/notes/MEMORY.md, inside the personal store", input: bash(`sed '1a eww' ~/notes/MEMORY.md`)},

		// Input the guard cannot read.
		{name: "malformed command", want: "neither a string nor a list of strings", input: `{"command":5}`},
		{name: "oversized command", want: "past what the guard reads", input: bash("echo x > ~/work/" + strings.Repeat("a", 1<<18))},

		// Base-denied commands stay denied.
		{name: "sed e command root", want: "which holds", cwd: ".", input: bash(`sed -n '1e rm -rf /' f`)},
		{name: "lua execute root", want: "which holds", cwd: ".", input: bash(`lua -e 'os.execute("rm -rf /")'`)},
		{name: "node execSync root", want: "which holds", cwd: ".", input: bash(`node -e 'require("child_process").execSync("rm -rf /")'`)},
		{name: "python rmtree root", want: "which holds", cwd: ".", input: bash(`python3 -c 'import shutil; shutil.rmtree("/", ignore_errors=True)'`)},
		{name: "sed e command relative store name", want: "personal store", cwd: ".", input: bash(`sed -n '1e rm -rf notes' f`)},
		{name: "sed e cd relative store name", want: "which holds", cwd: "work", input: bash(`sed -n '1e cd .. && rm -rf notes' f`)},
		{name: "python shutil relative store name", want: "builds at run time", cwd: ".", input: bash(`python3 -c 'import shutil; shutil.rmtree("notes")'`)},
		{name: "node rmSync relative store name", want: "builds at run time", cwd: ".", input: bash(`node -e 'require("fs").rmSync("notes",{recursive:true})'`)},
		{name: "sed w hardlink operand", want: "a hard link to", cwd: "work", input: bash(`sed -n 'w hard' f`)},
		{name: "sed w symlink attest operand", want: "an .attest dir", cwd: "work", input: bash(`sed -n 'w rel' f`)},
		{name: "python glued out root", want: "which holds", cwd: ".", input: bash(`python3 tool.py --out=/`)},
		{name: "python short glued out root", want: "which holds", cwd: ".", input: bash(`python3 tool.py -o/`)},
		{name: "subtoken renamed interpreter heredoc", want: "which holds", cwd: ".", input: bash("python3 -c 'import shutil,os;shutil.copy(\"/bin/sh\",os.getenv(\"HOME\")+\"/bin/python3\")'; env python3 <<EOF\nrm -rf .\nEOF")},
		{name: "env-built store path in awk", want: "builds at run time", cwd: ".", input: bash(`awk 'BEGIN{system("rm -rf " ENVIRON["HOME"] "/notes")}'`)},
		{name: "env-built store path in python", want: "builds at run time", cwd: ".", input: bash(`python3 -c 'import os; os.system("rm -rf " + os.environ["HOME"] + "/notes")'`)},
		{name: "awk program system root wipe", want: "which holds", cwd: ".", input: bash(`awk 'BEGIN{system("rm -rf /")}'`)},
		{name: "ssh remote script root wipe", want: "which holds", cwd: ".", input: bash(`ssh localhost 'rm -rf / (true)'`)},
		{name: "nested interpreter heredoc on dot", want: "which holds", cwd: ".", input: bash("cp /bin/sh ~/bin/python3; python3 <<EOF\npython3 <<EOF2\nrm -rf .\nEOF2\nEOF")},
		{name: "tar glued chdir root", want: "which holds", cwd: ".", input: bash(`tar -C/ -xf a.tar`)},
		{name: "tar long directory root", want: "which holds", cwd: ".", input: bash(`tar --directory=/ -xf a.tar`)},
		{name: "cp glued target root", want: "which holds", cwd: ".", input: bash(`cp -rt/ x`)},
		{name: "cp long target-directory root", want: "which holds", cwd: ".", input: bash(`cp --target-directory=/ x`)},
		{name: "python cleans to root", want: "which holds", cwd: ".", input: bash(`python3 -c 'open("/home/..","w")'`)},
		{name: "awk address program", want: "which holds", input: bash(`awk '/^foo$/{print $1}' f`)},
		{name: "awk division program", want: "which holds", input: bash(`awk '{print $1 / $2}' f`)},
		{name: "awk glued field separator", want: "which holds", input: bash(`awk -F/ '{print $NF}' f`)},
		{name: "python inline spaced slash", want: "which holds", input: bash(`python3 -c 'x = "a / b"'`)},
		{name: "python heredoc slash string", want: "which holds", input: bash("python3 - <<EOF\nsep = \"/\"\nEOF")},
		{name: "deferred sed script redefines", want: "inside the personal store", input: bash(`git commit -m '~/notes/MEMORY.md' && sed -i '#/crew' f`)},
		{name: "sed in-place script trailing slash regex", want: "which holds", input: bash(`sed -i 's/^        monthlyTarget = 100;$/        monthlyTarget = 30;/' home/ai/claude-code/default.nix`)},
		{name: "posix order sed file operand", want: "inside the personal store", cwd: "work", input: bash(`POSIXLY_CORRECT=1 sed -i --follow-symlinks s/a/b/ -e P`)},
		{name: "env posix order sed file operand", want: "inside the personal store", cwd: "work", input: bash(`env POSIXLY_CORRECT=1 sed -i --follow-symlinks s/a/b/ -e P`)},

		// Protected variables set under spelled names.
		{name: "quote-split exported PATH", want: "inside the personal store, and may write", input: bash(`export "PA"TH=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "brace-built exported PATH", want: "inside the personal store, and may write", input: bash(`export {PA,}TH=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "ansi-c declared PATH", want: "inside the personal store, and may write", input: bash(`declare $'\x50ATH'=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "quote-split exported LD_PRELOAD", want: "inside the personal store, and may write", input: bash(`export "LD_"PRELOAD=/tmp/x.so; cat ~/notes/MEMORY.md`)},
		{name: "quote-split exported GIT_DIR", want: "inside the personal store, and may write", input: bash(`export "GIT_"DIR=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "quote-split declared IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; declare 'I'FS=Q; rm -rf $d`)},
		{name: "printf -v built IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; v=IF; printf -v ${v}S Q; rm -rf $d`)},
		{name: "redirect variable IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; exec {IFS}>/tmp/f; rm -rf $d`)},
		{name: "eval escaped IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; eval I\FS=Q; rm -rf $d`)},
		{name: "opaque declare nameref to IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; n=declare; $n -n r=I""FS; r=Q; rm -rf $d`)},
		{name: "brace-built exported IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; export {I,}FS=Q; rm -rf $d`)},
		{name: "quote-split local IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; local "I"FS=Q; rm -rf $d`)},
		{name: "getopts escaped IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; getopts a I\FS; rm -rf $d`)},
		{name: "wait -p escaped IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; wait -p I\FS; rm -rf $d`)},
		{name: "source may set IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; source /tmp/x; rm -rf $d`)},
		{name: "quote-split readonly IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; readonly I""FS=Q; rm -rf $d`)},
		{name: "read escaped IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; read I\FS < /tmp/f; rm -rf $d`)},
		{name: "eval IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; eval IFS=Q; rm -rf $d`)},
		{name: "quoted typeset IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; typeset "IFS"=Q; rm -rf $d`)},
		{name: "quote-split exported CDPATH", want: "from a dir it changes into at run time", input: bash(`export "CD"PATH=~; cd notes && touch MEMORY.md`)},
		{name: "quote-split declared CDPATH", want: "from a dir it changes into at run time", input: bash(`declare 'CD'PATH=~; cd notes && touch MEMORY.md`)},
		{name: "BASH_CMDS renaming a reader", want: "inside the personal store, and may write", input: bash(`BASH_CMDS[ls]=/bin/rm; ls ~/notes/MEMORY.md`)},
		{name: "read before a data position", want: "which holds the personal store", cwd: ".", input: bash(`n=1; { read n; cp /tmp/evil "$n"; } < f; gh pr view 1 --jq .`)},

		// Setters read after the words they change, and arithmetic.
		{name: "function body before a quote-split IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; f(){ rm -rf $d; }; declare 'I'FS=Q; f`)},
		{name: "trap body before a quote-split IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; trap 'rm -rf $d' EXIT; declare 'I'FS=Q`)},
		{name: "function body before a quote-split CDPATH", want: "from a dir it changes into at run time", input: bash(`f(){ cd notes && touch MEMORY.md; }; export "CD"PATH=~; f`)},
		{name: "function body before a default CDPATH", want: "from a dir it changes into at run time", input: bash(`f(){ cd notes && touch MEMORY.md; }; : ${CDPATH:=~}; f`)},
		{name: "function body before an eval IFS", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; f(){ rm -rf $d; }; eval I\FS=Q; f`)},
		{name: "function body before a source", want: "builds at run time", input: bash(`d=/tmpQ$HOME/notes; f(){ rm -rf $d; }; source /tmp/x; f`)},
		{name: "arithmetic target built IFS", want: "builds at run time", input: bash(`v=IF; (( ${v}S = 5 )); d=/tmp5$HOME/notes; rm -rf $d`)},
		{name: "let quote-split IFS", want: "builds at run time", input: bash(`let "I""FS=5"; d=/tmp5$HOME/notes; rm -rf $d`)},
		{name: "arithmetic value sets IFS", want: "builds at run time", input: bash(`x='I'"FS=5"; (( x )); d=/tmp5$HOME/notes; rm -rf $d`)},
		{name: "integer value sets IFS", want: "builds at run time", input: bash(`declare -i n; n='I'FS=5; d=/tmp5$HOME/notes; rm -rf $d`)},
		{name: "opaque arithmetic target", want: "builds at run time", input: bash(`n=$(echo I""FS); (( $n = 5 )); d=/tmp5$HOME/notes; rm -rf $d`)},
		{name: "arithmetic value substitution", want: "/notes, the personal store", input: bash(`x='a[$(rm -rf ~/notes)]'; (( x ))`)},
		{name: "read subscript substitution", want: "which holds the personal store", cwd: ".", input: bash(`read 'a[$(rm -rf .)]'`)},
		{name: "arithmetic substitution", want: "which holds the personal store", cwd: ".", input: bash(`(( $(rm -rf .) ))`)},
		{name: "arithmetic expansion value substitution", want: "which holds the personal store", cwd: ".", input: bash(`x='a[$(rm -rf .)]'; echo $(( x ))`)},

		// POSIX classes in a glob.
		{name: "posix class rm", want: "inside the personal store", input: bash("rm -rf ~/note[[:alpha:]]")},
		{name: "posix class attest", want: "an .attest dir", input: bash("touch /srv/other/.[[:alpha:]]ttest/e")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.linuxOnly && runtime.GOOS != "linux" {
				t.Skipf("%s has no such path", runtime.GOOS)
			}
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
		{name: "posix class glob in project", input: bash("rm -f ~/work/[[:alpha:]]*.o")},
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
		{name: "commit message heredoc", input: bash("git commit -m \"$(cat <<'EOF'\nfix MEMORY.md handling\nEOF\n)\"")},
		{name: "commit message with store name", input: bash(`git commit -m "notes $(date)"`)},
		{name: "pr body heredoc", input: bash("gh pr create --title t --body \"$(cat <<'EOF'\nfix MEMORY.md handling in notes\nEOF\n)\"")},
		{name: "sed script with e letters", input: bash(`cd /tmp/x && grep -oE '^(published|flagged|would [a-z]+ [a-z]+|would [a-z]+|unchanged|duplicate|propose|skipped)' import-dry-run.md | sort | uniq -c; grep -c 'work-name' import-dry-run.md; grep -c 'wikilink-dangling' import-dry-run.md; grep -oE ': (refused: [^:]+: [a-z-]+|routes to the quarantine|not a fact file)' import-dry-run.md | sed -E 's/refused: [^:]+: /refused: /' | sort | uniq -c; sed -n 95,131p import-dry-run.md | cut -c1-220`)},
		{name: "unparsed string naming dot is not a path", cwd: ".", input: bash(`watch 'rm -rf . ('`)},
		{name: "jq filter after an assignment", input: bash(`CREW_ID=x crew roster | jq -c '.[] | select(.state=="working" or .state=="blocked") | {name,branch,state,age_s}'`)},
		{name: "jq alternative filter after an assignment", input: bash(`CREW_ID=x crew roster | jq -c '.[]? // .'`)},

		// Data positions of a cleanly resolved known program are text, not a path.
		{name: "claude prompt with a dot", cwd: ".", input: bash(`claude -p "check the build ." --tools=Bash`)},
		{name: "gh jq with a slash alternative", input: bash(`gh pr view 1 --json state,mergeCommit --jq '"\(.state) \(.mergeCommit.oid // "-")"'`)},
		{name: "crew body with a spaced slash", cwd: ".", input: bash(`crew reply worker:x "a / b"`)},
		{name: "env unset before a claude prompt", cwd: ".", input: bash(`env -u A -u B claude -p "check . now"`)},
		{name: "timeout before a claude prompt", cwd: ".", input: bash(`timeout 5 claude -p "check ."`)},
		{name: "glab jq alternative", input: bash(`glab mr view 3 --jq '.a // .b'`)},
		{name: "gh api q alternative", input: bash(`gh api x -q '.a // .b'`)},
		{name: "crew body after a crew id", cwd: ".", input: bash(`crew --crew c reply x "a / b"`)},
		{name: "crew body before a trailing crew id", cwd: ".", input: bash(`crew reply x "a / b" --crew c`)},
		{name: "gh jq then a crew body", cwd: ".", input: bash(`gh pr view 1 --jq . && crew reply x "a / b"`)},
		// A crew message body is data: a bare . or / body is a message, not a path.
		{name: "crew reply bare dot", cwd: ".", input: bash(`crew reply x .`)},
		{name: "crew reply bare slash", input: bash(`crew reply x /`)},

		// A known data position (a search pattern, a commit or forge message) is text, not a path.
		{name: "rg regex with slashes", input: bash(`rg '"github.com/charmbracelet/[a-z/]*"'`)},
		{name: "grep pattern with a slash", input: bash(`grep -r 'notes/x' .`)},
		{name: "git commit message with store name", input: bash(`git commit -m "fix notes handling"`)},
		{name: "gh pr body with store name", input: bash(`gh pr create --title t --body "fix notes handling"`)},

		// Finite for-loop variables.
		{name: "loop over issue numbers with a jq alternative", input: bash(`for n in 254 221; do gh issue view $n --json title --jq '.title, (.comments[-1].body // "" | .[0:600])'; done`)},
		{name: "loop over crew replies", cwd: ".", input: bash(`for w in a b; do crew reply $w "a / b"; done`)},
		{name: "loop over issue numbers from home", cwd: ".", input: bash(`for n in 1 2; do gh issue view $n --jq '.a // .b'; done`)},

		// Reads in and around a store, and setters of other names.
		{name: "jq index filter on a store file", input: bash(`jq '.[] | select(.a[0])' ~/notes/x.json`)},
		{name: "git reads of a store", input: bash("git -C ~/notes log --oneline; git -C ~/notes diff; git -C ~/notes status")},
		{name: "sed ls rg of a store", input: bash("sed -n 1,5p ~/notes/MEMORY.md; ls ~/notes; rg -n foo ~/notes")},
		{name: "cd store then git log and rg", input: bash("cd ~/notes && git log -3 && rg -l x")},
		{name: "read a line then echo it", input: bash(`read -r line < /tmp/f; echo "$line"`)},
		{name: "printf -v then echo it", input: bash(`printf -v x '%s' a; echo "$x"`)},
		{name: "exported and local values written from home", cwd: ".", input: bash(`export d=/tmp/x; local f=/tmp/y; rm -rf "$d" "$f"`)},

		// Arithmetic that sets no protected name.
		{name: "c-style loop then ls of a store", input: bash(`for ((i=0; i<3; i++)); do echo $i; done; ls ~/notes`)},
		{name: "arithmetic expansion then cat of a store", input: bash(`n=3; echo $(( n + 1 )); cat ~/notes/MEMORY.md`)},
		{name: "integer test then rg of a store", input: bash(`[[ 1 -lt 2 ]] && rg x ~/notes`)},
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
		{name: "dotdot cleaned before symlink", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/lk/../lk/notes/MEMORY.md", "content": "x"})},
		{name: "dotdot cleaned before symlink git", want: "/hooks/pre-commit, inside the personal store's git dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/lk/../lk/notes/.git/hooks/pre-commit", "content": "x"})},
		{name: "hard link", want: "a hard link to", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hard", "content": "x"})},
		{name: "hard link to git config", want: "a hard link to", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hardgit", "content": "x"})},
		{name: "hard link to a git object", want: "a hard link to", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hobj", "content": "x"})},
		{name: "hard link to the config target", want: "a hard link to", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hcfg", "content": "x"})},
		{name: "config symlink target", want: "/dotfiles/priors.toml, priors' config file", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/dotfiles/priors.toml", "content": "x"})},
		{name: "hard link to a fact reviewed", want: "/work/hardfact claiming confidence: reviewed", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hardfact", "content": reviewed})},
		{name: "hard link to a fact edit to reviewed", want: "/work/hardfact claiming confidence: reviewed", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/work/hardfact", "old_string": "unverified", "new_string": "reviewed"})},
		{name: "hard link to a fact patch", want: "/work/hardfact claiming confidence: reviewed", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/work/hardfact\n@@\n-  confidence: unverified\n+  confidence: reviewed\n*** End Patch"})},
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

		// Tools and keys beyond Claude's own.
		{name: "unknown path key", want: "/notes/MEMORY.md, the personal store's generated index", cwd: "notes", tool: "mcp__serena__create_text_file", input: sb.input(map[string]any{"relative_path": "MEMORY.md", "content": "x"})},
		{name: "unknown path key camel", want: "/notes/MEMORY.md, the personal store's generated index", cwd: "notes", tool: "mcp__jetbrains__create_new_file", input: sb.input(map[string]any{"pathInProject": "MEMORY.md", "text": "x"})},
		{name: "unknown path key in array", want: "/notes/MEMORY.md, the personal store's generated index", cwd: "notes", tool: "mcp__fs__create_many", input: sb.input(map[string]any{"names": []any{"MEMORY.md"}, "content": "x"})},
		{name: "unknown content key", want: "/facts/n.md claiming confidence: reviewed", tool: "mcp__fs__write_file", input: sb.input(map[string]any{"path": "@H/notes/facts/n.md", "new_content": reviewed})},
		{name: "unknown tool by name", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__fs__search_and_replace", input: sb.input(map[string]any{"path": "@H/notes/MEMORY.md"})},
		{name: "cursor search replace", want: "/notes/MEMORY.md, the personal store's generated index", tool: "search_replace", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "old_string": "a", "new_string": "b"})},
		{name: "edit text named twice", want: "names an edit text more than once", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/work/a.md", "old_string": "a", "oldString": "b", "new_string": "c"})},

		// Strings the guard must read wherever they sit.
		{name: "nested path array", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__batch", input: sb.input(map[string]any{"files": []any{[]any{"~/notes/MEMORY.md", "x"}}})},
		{name: "nested array under unknown key", want: "/notes/.attest/e, inside an .attest dir", tool: "mcp__x__batch", input: sb.input(map[string]any{"x": []any{[]any{"~/notes/.attest/e"}}})},
		{name: "nested content array", want: "/facts/n.md claiming confidence: reviewed", tool: "mcp__fs__write_many", input: sb.input(map[string]any{"path": "@H/notes/facts/n.md", "contents": []any{[]any{reviewed}}})},
		{name: "newline in unknown key", want: "/facts/z\n.md claiming confidence: reviewed", cwd: "notes", tool: "mcp__serena__create_text_file", input: sb.input(map[string]any{"relative_path": "facts/z\n.md", "content": reviewed})},
		{name: "newline in state path", want: "inside priors' state dir", tool: "mcp__fs__create_doc", input: sb.input(map[string]any{"uri": "@H/.local/state/priors/local/personal/z\n.md"})},
		{name: "padded path", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__fs__create_doc", input: sb.input(map[string]any{"uri": " @H/notes/MEMORY.md\t"})},
		{name: "content key without a path", want: "inside an .attest dir", tool: "mcp__srv__do", input: sb.input(map[string]any{"text": "~/notes/.attest/e"})},
		{name: "edits of strings", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"edits": []any{"~/notes/MEMORY.md"}})},
		{name: "old string without a pair", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"old_string": "~/notes/MEMORY.md"})},

		// MCP tools named for a read that write.
		{name: "search in a write name", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__save_search_results", input: sb.input(map[string]any{"path": "@H/notes/MEMORY.md"})},
		{name: "list in a write name", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__playlist_add", input: sb.input(map[string]any{"path": "@H/notes/MEMORY.md"})},
		{name: "list with a write verb", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__list_and_export", input: sb.input(map[string]any{"path": "@H/notes/MEMORY.md"})},

		// Pi's path forms.
		{name: "pi tilde", want: "/notes/MEMORY.md, the personal store's generated index", tool: "write", input: sb.input(map[string]any{"path": "~/notes/MEMORY.md", "content": "x"})},
		{name: "pi at", want: "/notes/MEMORY.md, the personal store's generated index", tool: "write", input: sb.input(map[string]any{"path": "@@H/notes/MEMORY.md", "content": "x"})},
		{name: "pi file url", want: "/notes/MEMORY.md, the personal store's generated index", tool: "write", input: sb.input(map[string]any{"path": "file://@H/notes/MEMORY.md", "content": "x"})},
		{name: "pi file url localhost", want: "/notes/MEMORY.md, the personal store's generated index", tool: "write", input: sb.input(map[string]any{"path": "file://localhost@H/notes/MEMORY.md", "content": "x"})},
		{name: "pi file url escaped", want: "/notes/MEMORY.md, the personal store's generated index", tool: "write", input: sb.input(map[string]any{"path": "file://@H/notes/%4DEMORY.md", "content": "x"})},
		{name: "pi file url attest", want: "/notes/.attest/e1, inside an .attest dir", tool: "write", input: sb.input(map[string]any{"path": "file://@H/notes/.attest/e1", "content": "x"})},
		{name: "pi tilde reviewed fact", want: "/facts/n.md claiming confidence: reviewed", tool: "write", input: sb.input(map[string]any{"path": "~/notes/facts/n.md", "content": reviewed})},
		{name: "pi at reviewed fact", want: "/facts/n.md claiming confidence: reviewed", tool: "write", input: sb.input(map[string]any{"path": "@@H/notes/facts/n.md", "content": reviewed})},

		// Edits the tool applies against the original, or on a normalised copy.
		{name: "pi edit fuzzy", want: "/facts/b.md claiming confidence: reviewed", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"oldText": "  confidence: unverified  ", "newText": "  confidence: reviewed"},
		}})},
		{name: "pi edit top level fuzzy", want: "/facts/b.md claiming confidence: reviewed", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "oldText": "  confidence: unverified  ", "newText": "  confidence: reviewed"})},
		{name: "pi multi edit", want: "new text may leave", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/c.md", "edits": []any{
			map[string]any{"oldText": "unverified", "newText": "reviewed"},
			map[string]any{"oldText": "reviewed", "newText": "checked"},
		}})},
		{name: "pi edit risky new text", want: "new text may leave", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"oldText": "body", "newText": "&a"},
			map[string]any{"oldText": "name: b", "newText": "name: *a"},
		}})},
		{name: "multi edit approximated at original positions", want: "/facts/c.md claiming confidence: reviewed", tool: "MultiEdit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/c.md", "edits": []any{
			map[string]any{"old_string": "unverified", "new_string": "reviewed"},
			map[string]any{"old_string": "reviewed", "new_string": "checked"},
		}})},
		{name: "pi edits at original positions", want: "/facts/s.md claiming confidence: reviewed", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/s.md", "edits": []any{
			map[string]any{"oldText": "X ", "newText": "re"},
			map[string]any{"oldText": "re", "newText": ""},
		}})},
		{name: "pi edit fuzzy join", want: "/facts/f.md claiming confidence: reviewed", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/f.md", "edits": []any{
			map[string]any{"oldText": "\"x\"", "newText": ""},
		}})},

		// Paths packed in one string, JSON in a string, other file forms.
		{name: "packed paths comma", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"paths": "@H/work/a,@H/notes/MEMORY.md"})},
		{name: "packed paths newline", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"paths": "@H/work/a\n@H/notes/MEMORY.md"})},
		{name: "packed paths semicolon", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"paths": "@H/work/a;@H/notes/MEMORY.md"})},
		{name: "packed paths colon", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"paths": "@H/work/a:@H/notes/MEMORY.md"})},
		{name: "packed paths space", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"paths": "@H/work/a @H/notes/MEMORY.md"})},
		{name: "packed relative name", want: "which holds the personal store", cwd: ".", tool: "mcp__x__do", input: sb.input(map[string]any{"paths": "a,notes"})},
		{name: "assignment in args", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"args": "path=@H/notes/MEMORY.md"})},
		{name: "long option in args", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"args": "--out=@H/notes/MEMORY.md"})},
		{name: "json in a string", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"args": `{"path":"@H/notes/MEMORY.md"}`})},
		{name: "file url with one slash", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "file:@H/notes/MEMORY.md", "content": "x"})},
		{name: "file url with one slash in a uri", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"uri": "file:@H/notes/MEMORY.md"})},
		{name: "claim split across lines", want: "/facts/b.md claiming confidence: reviewed", tool: "mcp__x__write", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "lines": []any{"---", "name: b", "metadata:", "  confidence: \"re\\", "viewed\"", "---", "body"}})},
		{name: "posix class write", want: "/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/note[[:alpha:]]/MEMORY.md", "content": "x"})},

		// Input the guard cannot read.
		{name: "invalid json", want: "input cannot be read", tool: "Write", input: `{"file_path": `},
		{name: "deeply nested input", want: "input is past what the guard reads", tool: "Write", input: strings.Repeat("[", 20) + strings.Repeat("]", 20)},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantDenied(t, c, sb.run(t, c))
		})
	}
}

// TestToolDuplicateKeys runs a call whose two path keys normalise alike many
// times, since a map read picks one at random.
func TestToolDuplicateKeys(t *testing.T) {
	sb := newSandbox(t)
	c := call{want: "/notes/.attest/e1, inside an .attest dir", tool: "mcp__filesystem__write_file", input: sb.input(map[string]any{"path": "@H/notes/.attest/e1", "Path": "/tmp/safe"})}
	for range 20 {
		wantDenied(t, c, sb.run(t, c))
	}
}

func TestToolAllows(t *testing.T) {
	sb := newSandbox(t)
	for _, c := range []call{
		{name: "project write", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/b.md", "content": "confidence: reviewed"})},
		{name: "sibling store", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes-old/MEMORY.md", "content": "x"})},
		{name: "attestation dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/sub/.attestation/y", "content": "x"})},
		{name: "unreviewed fact", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\nname: n\nmetadata:\n  confidence: unverified\n---\nThe PR was reviewed.\n"})},
		{name: "unreviewed fact through a hard link", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hardfact", "content": "---\nname: b\nmetadata:\n  confidence: unverified\n---\nmore\n"})},
		{name: "benign edit", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "old_string": "body", "new_string": "more body"})},
		{name: "read attest", tool: "Read", input: sb.input(map[string]any{"file_path": "@H/notes/.attest/e1"})},
		{name: "grep store", tool: "Grep", input: sb.input(map[string]any{"pattern": "x", "path": "@H/notes"})},
		{name: "patch benign", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/b.md\n@@\n-body\n+more body\n*** End Patch"})},
		{name: "read tool by name", tool: "mcp__github__get_file_contents", input: sb.input(map[string]any{"path": "/"})},
		{name: "lsp in store git", tool: "LSP", input: sb.input(map[string]any{"filePath": "@H/notes/.git/config"})},
		{name: "mcp read in store git", tool: "mcp__filesystem__read_text_file", input: sb.input(map[string]any{"path": "@H/notes/.git/config"})},
		{name: "mcp file contents in store", tool: "mcp__github__get_file_contents", input: sb.input(map[string]any{"path": "@H/notes/MEMORY.md"})},
		{name: "mcp pattern search in store", tool: "mcp__serena__search_for_pattern", input: sb.input(map[string]any{"relative_path": "@H/notes/MEMORY.md"})},
		{name: "mcp list", tool: "mcp__github__list_issues", input: sb.input(map[string]any{"owner": "o", "repo": "r"})},
		{name: "mcp search", tool: "mcp__fs__search_files", input: sb.input(map[string]any{"path": "@H/notes/.attest", "pattern": "e"})},
		{name: "unknown path key outside store", tool: "mcp__serena__create_text_file", input: sb.input(map[string]any{"relative_path": "a.md", "content": "x"})},
		{name: "mcp strings outside store", tool: "mcp__github__create_issue", input: sb.input(map[string]any{"owner": "o", "repo": "r", "title": "t", "body": "a\nb"})},
		{name: "pi tilde project", tool: "write", input: sb.input(map[string]any{"path": "~/work/a.md", "content": "x"})},
		{name: "pi file url project", tool: "write", input: sb.input(map[string]any{"path": "file://@H/work/a.md", "content": "x"})},
		{name: "pi benign edit", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"oldText": "body", "newText": "more body"},
			map[string]any{"oldText": "name: b", "newText": "name: bb"},
		}})},
		{name: "multi edit benign", tool: "MultiEdit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/c.md", "edits": []any{
			map[string]any{"old_string": "The PR", "new_string": "A PR"},
			map[string]any{"old_string": "A PR was", "new_string": "A PR was not"},
		}})},
		{name: "content naming a store path", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/b.md", "content": "@H/notes/.attest/e"})},
		{name: "edit texts naming a store path", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/work/a.md", "old_string": "@H/notes/MEMORY.md", "new_string": "@H/notes/.attest/e"})},
		{name: "mcp prose naming no store", tool: "mcp__x__write", input: sb.input(map[string]any{"body": "see a, b and c/d"})},
		{name: "packed project paths", tool: "mcp__x__do", input: sb.input(map[string]any{"paths": "@H/work/a,@H/work/b"})},
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

// TestLinkedBudget checks that a file with other links the walk ran out of
// budget before matching is unknown, and one it matched is not.
func TestLinkedBudget(t *testing.T) {
	sb := newSandbox(t)
	c := sb.guard.newCheck(sb.work, nil)
	c.budget = 1
	if to, unknown := c.linked(filepath.Join(sb.work, "hardfact")); len(to) != 0 || !unknown {
		t.Errorf("linked = %v, %v; want unknown", to, unknown)
	}
	c = sb.guard.newCheck(sb.work, nil)
	if to, unknown := c.linked(filepath.Join(sb.work, "hardfact")); len(to) == 0 || unknown {
		t.Errorf("linked = %v, %v; want a match", to, unknown)
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
