package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

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

// optionWrites is five writer calls of eight option values each, all distinct.
func optionWrites() string {
	var calls []string
	for i := range 5 {
		call := "w"
		for j := range 8 {
			call += " --o=u" + strconv.Itoa(i*8+j+1)
		}
		calls = append(calls, call)
	}
	return strings.Join(calls, "; ")
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
		{name: "redirect into a leftover attest dir", want: "/notes/.attest/e2, inside the personal store", input: bash("echo x > ~/notes/.attest/e2")},
		{name: "append to index", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x >> $HOME/notes/MEMORY.md")},
		{name: "fact by shell", want: "/notes/facts/a.md, inside the personal store", input: bash(`printf x > "@H/notes/facts/a.md"`)},
		{name: "rm attest", want: "/notes/.attest, inside the personal store", input: bash("rm -rf ~/notes/.attest")},
		{name: "state dir", want: "/provenance/s1, inside priors' state dir", input: bash("echo x > ~/.local/state/priors/provenance/s1")},
		{name: "config file", want: "/config.toml, inside priors' config dir", input: bash("rm ~/.config/priors/config.toml")},
		{name: "ancestor of config", want: "which holds priors' config dir", input: bash("rm -rf ~/.config")},
		{name: "ancestor of store", want: "/*, inside the personal store", input: bash("rm -rf ~/*")},
		{name: "root glob", want: "/*, which holds the personal store", input: bash("rm -rf /*")},
		{name: "rm dot from home", want: "which holds the personal store", cwd: ".", input: bash("rm -rf .")},
		{name: "var from env", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > $NOTES/MEMORY.md")},
		{name: "tilde user", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x > ~alice/notes/MEMORY.md")},

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
		{name: "relative symlink", want: "/notes/.attest/e2, inside the personal store", input: bash("echo x > rel/e2")},
		{name: "symlink chain", want: "/notes/.git/config, inside the personal store", input: bash("echo x > chain/.git/config")},
		{name: "dangling symlink", want: "/notes/.attest/new, inside the personal store", input: bash("echo x > dang")},
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
		{name: "tar -C ancestor", want: "which holds the personal store", input: bash("tar -xf a.tar -C ~")},
		{name: "env -C", want: "runs in", input: bash("env -C ~/notes touch x")},

		// Quoting and escaping.
		{name: "double quotes", want: "/notes/MEMORY.md, inside the personal store", input: bash(`echo x > ~/"notes"/MEMORY.md`)},
		{name: "split quotes", want: "/notes/.attest/e, inside the personal store", input: bash(`echo x > ~/'no'"tes"/.att''est/e`)},
		{name: "backslashes", want: "/notes/MEMORY.md, inside the personal store", input: bash(`echo x > ~/no\tes/MEMORY\.md`)},
		{name: "braces", want: "/notes/facts/n.md, inside the personal store", input: bash("echo x | tee ~/{work,notes}/facts/n.md")},
		{name: "extglob", want: "/+(no)tes/f, inside the personal store", input: bash("shopt -s extglob; echo x > ~/+(no)tes/f")},
		{name: "negated class rm", want: "/[!q]otes, inside the personal store", input: bash("rm -rf ~/[!q]otes")},
		{name: "negated class redirect", want: "/[!q]otes/MEMORY.md, inside the personal store", input: bash("echo x > ~/[!q]otes/MEMORY.md")},

		// Wrappers.
		{name: "sudo tee", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo x | sudo tee ~/notes/MEMORY.md")},
		{name: "env cp", want: "/notes/.attest, inside the personal store", input: bash("env X=1 cp a ~/notes/.attest/")},
		{name: "nice timeout mv", want: "/notes/.attest/y, inside the personal store", input: bash("nice -n 5 timeout 5 mv x ~/notes/.attest/y")},
		{name: "bash -c", want: "/notes/MEMORY.md, inside the personal store", input: bash(`bash -c 'echo x > ~/notes/MEMORY.md'`)},
		{name: "eval", want: "/notes/MEMORY.md, inside the personal store", input: bash(`eval "rm ~/notes/MEMORY.md"`)},
		{name: "find delete", want: "/notes, the personal store", input: bash("find ~/notes -name '*.md' -delete")},
		{name: "sed -i", want: "/notes/MEMORY.md, inside the personal store", input: bash("sed -i s/a/b/ ~/notes/MEMORY.md")},
		{name: "sed w", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -n 'w @H/notes/facts/a.md' /tmp/x")},
		{name: "sed -i extra flag", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i -s 's/a/b/' @H/notes/facts/a.md")},
		{name: "sed -i backup suffix", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i.bak 's/a/b/' @H/notes/facts/a.md")},
		{name: "sed -i expression flag", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i -e 's/a/b/' @H/notes/facts/a.md")},
		{name: "sed -i two commands", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i 's/a/b/;s/c/d/' @H/notes/facts/a.md")},
		{name: "sed -i e flag", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i 's/a/b/e' @H/notes/facts/a.md")},
		{name: "sed -i w flag", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i 's/a/b/w /tmp/x' @H/notes/facts/a.md")},
		{name: "sed -i w command", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i 'w @H/notes/facts/x' @H/notes/facts/a.md")},
		{name: "sed env prefix", want: "/notes/facts/a.md, inside the personal store", input: bash("POSIXLY_CORRECT=1 sed -i 's/a/b/' @H/notes/facts/a.md")},
		{name: "env sed", want: "/notes/facts/a.md, inside the personal store", input: bash("env sed -i 's/a/b/' @H/notes/facts/a.md")},
		{name: "xargs sed", want: "/notes/facts/a.md, inside the personal store", input: bash("xargs sed -i 's/a/b/' @H/notes/facts/a.md")},
		{name: "sed long in place", want: "/notes/facts/a.md, inside the personal store", input: bash("sed --in-place 's/a/b/' @H/notes/facts/a.md")},
		{name: "sed -i -n", want: "/notes/facts/a.md, inside the personal store", input: bash("sed -i -n 's/a/b/' @H/notes/facts/a.md")},
		{name: "ln", want: "/notes/MEMORY.md, inside the personal store", input: bash("ln -sf /tmp/x ~/notes/MEMORY.md")},
		{name: "xargs involved", want: "/notes/MEMORY.md, inside the personal store", input: bash("echo ~/notes/MEMORY.md | xargs rm")},
		{name: "heredoc", want: "/notes/x, inside the personal store", input: bash("cat > ~/notes/x <<EOF\nhi\nEOF")},
		{name: "python", want: "/notes/MEMORY.md, inside the personal store", input: bash(`python3 -c "open('@H/notes/MEMORY.md','w')"`)},
		{name: "codex argv", want: "/notes/MEMORY.md, inside the personal store", input: `{"command":["bash","-lc","echo x > ~/notes/MEMORY.md"]}`},
		{name: "git plumbing", want: "and runs in", input: bash("git --git-dir=@H/notes/.git hash-object -w x")},
		{name: "dd", want: "/notes/MEMORY.md, inside the personal store", input: bash("dd if=/dev/zero of=@H/notes/MEMORY.md")},
		{name: "bare redirect", want: "/notes/MEMORY.md, inside the personal store", input: bash("> ~/notes/MEMORY.md")},
		{name: "group redirect", want: "/notes/MEMORY.md, inside the personal store", input: bash("{ echo x; } > ~/notes/MEMORY.md")},
		{name: "function body", want: "/notes/MEMORY.md, inside the personal store", input: bash("f() { echo x > ~/notes/MEMORY.md; }; f")},
		{name: "symlink listing to xargs", want: "/notes, the personal store", input: bash("ls ~/link/ | xargs rm")},

		// Options and wrappers that read as something else.
		{name: "env unset option", want: "/notes, the personal store", input: bash("env -u cat rm -rf ~/notes")},
		{name: "exec name option", want: "inside the personal store", input: bash(`exec -a cat bash -c 'echo x > ~/notes/facts/a.md'`)},
		{name: "xargs replace option", want: "/notes, the personal store", input: bash("echo ~/notes | xargs -I cat rm -rf ~/notes")},
		{name: "xargs replace option alone", want: "/notes, the personal store", input: bash("xargs -I cat rm -rf ~/notes")},
		{name: "git -C abbreviated option", want: "and runs in", input: bash("git -C ~/notes --attr-source log reset --hard")},
		{name: "sed abbreviated in-place", want: "inside the personal store", input: bash("sed --in-pl s/a/b/ ~/notes/facts/a.md")},
		{name: "sed expression option", want: "inside the personal store", input: bash("sed --expr='w @H/notes/facts/x' ~/work/a.md")},
		{name: "sed clustered expression", want: "inside the personal store", input: bash("sed -ne'w @H/notes/facts/x' ~/work/a.md")},
		{name: "git grep abbreviated open-files", want: "inside the personal store", input: bash("git grep --open='touch ~/notes/MEMORY.md;:' x")},
		{name: "git fetch abbreviated upload-pack", want: "and runs in", input: bash("git -C ~/notes fetch --upload=x origin")},
		{name: "git grep clustered open-files", want: "and runs in", input: bash("git -C ~/notes grep -iOx y")},
		{name: "fd clustered exec", want: "/notes, the personal store", input: bash("fd . ~/notes -Hx touch")},
		{name: "exported PATH", want: "/notes/MEMORY.md, inside the personal store", input: bash("export PATH=/tmp/evil:$PATH; cat ~/notes/MEMORY.md")},
		{name: "exported BASH_ENV", want: "/notes/MEMORY.md, inside the personal store", input: bash("export BASH_ENV=/tmp/x; cat ~/notes/MEMORY.md")},
		{name: "printf -v PATH", want: "/notes/MEMORY.md, inside the personal store", input: bash("printf -v PATH /tmp/evil; cat ~/notes/MEMORY.md")},
		{name: "path in a commit message substitution", want: "/notes/MEMORY.md, inside the personal store", input: bash("git commit -m \"$(rm $(echo ~/notes/MEMORY.md))\"")},

		// Paths built at run time.
		{name: "opaque under store", want: "notes/…\", which reaches into the personal store", input: bash("echo x > ~/notes/$(echo MEMORY.md)")},
		{name: "opaque involved", want: "which holds the personal store", input: bash(`echo x > "$(printf '%s/notes/MEMORY.md' ~)"`)},

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
		{name: "known writer on root", want: "which holds", input: bash(`rm -rf /`)},
		{name: "function renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`z() { rm -rf "$1"; }; z .`)},
		{name: "function defined after its use", want: "which holds the personal store", cwd: ".", input: bash(`trap 'z .' EXIT; z() { rm -rf "$1"; }`)},
		{name: "alias renaming a writer", want: "which holds the personal store", cwd: ".", input: bash("shopt -s expand_aliases\nalias z=rm\nz -rf .")},
		{name: "hash renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`hash -p /bin/rm z; z -rf .`)},
		{name: "opaque writer name", want: "which holds the personal store", cwd: ".", input: bash(`busybox $(printf rm) -rf .`)},
		{name: "upper-case writer", want: "which holds the personal store", cwd: ".", input: bash(`RM -rf .`)},
		{name: "versioned interpreter", want: "which holds the personal store", cwd: ".", input: bash(`python3.12 x.py .`)},
		{name: "unknown program on dot from home", want: "which holds the personal store", cwd: ".", input: bash(`foo .`)},
		{name: "BASH_CMDS renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`BASH_CMDS[z]=/bin/rm; z -rf .`)},
		{name: "PATH renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`PATH=~/evil:$PATH; z -rf .`)},
		{name: "symlink renaming a writer", want: "which holds the personal store", cwd: ".", input: bash(`ln -s /bin/rm ~/bin/z; z -rf .`)},
		{name: "mixed-case writer", want: "which holds the personal store", cwd: ".", input: bash(`Rm -rf .`)},
		{name: "wrapped upper-case writer", want: "which holds the personal store", cwd: ".", input: bash(`caffeinate RM -rf .`)},
		{name: "env unset before a renamed writer", want: "which holds the personal store", cwd: ".", input: bash(`env -u X z -rf .`)},
		{name: "env unset before claude on dot", want: "which holds the personal store", cwd: ".", input: bash(`env -u X claude .`)},
		{name: "claude mcp command on dot", want: "which holds the personal store", cwd: ".", input: bash(`claude mcp add x -- rm -rf .`)},
		{name: "claude argument substitution", want: "which holds the personal store", cwd: ".", input: bash(`claude -p "x" "$(rm -rf .)"`)},

		// Data-position allowlist probes: a known data-taker renamed, or its
		// resolution reached through a flag a wrapper value could impersonate,
		// keeps its writer semantics.
		{name: "env chdir and unset before a renamed writer", want: "which holds the personal store", cwd: ".", input: bash(`env -C d -u X z -rf .`)},
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
		{name: "gh alias subcommand q", want: "which holds the personal store", cwd: ".", input: bash(`gh alias set x -q /`)},
		{name: "crew body substitution", want: "which holds the personal store", cwd: ".", input: bash(`crew reply x "$(rm -rf .)"`)},
		{name: "alias renaming claude", want: "which holds the personal store", cwd: ".", input: bash(`alias claude=rm; claude -p .`)},
		{name: "function renaming claude", want: "which holds the personal store", cwd: ".", input: bash(`claude() { rm -rf "$1"; }; claude -p /`)},
		{name: "BASH_CMDS renaming gh", want: "which holds the personal store", cwd: ".", input: bash(`BASH_CMDS[gh]=/bin/rm; gh api x -q /`)},
		{name: "printf -v BASH_CMDS renaming crew", want: "/notes/MEMORY.md, inside the personal store", input: bash(`printf -v 'BASH_CMDS[crew]' %s /bin/rm; crew reply x ~/notes/MEMORY.md`)},
		{name: "quote-split declare BASH_CMDS", want: "which holds the personal store", cwd: ".", input: bash(`declare "BASH_CMD""S[zzq]=/bin/rm"; zzq -rf .`)},
		{name: "quote-split printf -v BASH_CMDS", want: "which holds the personal store", cwd: ".", input: bash(`printf -v "BASH_CMD"S"[zzr]" %s /bin/rm; zzr -rf .`)},
		{name: "hash renaming crew", want: "which holds the personal store", cwd: ".", input: bash(`hash -p /bin/rm crew; crew reply x .`)},
		{name: "symlink renaming crew", want: "which holds the personal store", cwd: ".", input: bash(`ln -sf /bin/rm ~/bin/crew; crew reply x .`)},
		{name: "copy renaming gh", want: "which holds the personal store", cwd: ".", input: bash(`cp /bin/rm ~/bin/gh; gh api x -q /`)},
		{name: "redirect renaming crew", want: "/notes/MEMORY.md, inside the personal store", input: bash(`cat /bin/rm > ~/bin/crew; chmod +x ~/bin/crew; crew reply x ~/notes/MEMORY.md`)},
		{name: "opaque rename target for crew", want: "which holds the personal store", cwd: ".", input: bash(`cp /bin/rm ~/bin/"$(printf crew)"; crew reply x .`)},
		{name: "trap body runs a renamed crew", want: "which holds the personal store", cwd: ".", input: bash(`trap 'crew reply x .' EXIT; alias crew=rm`)},

		// Finite for-loop variables.
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
		{name: "sed label hides continued w on bsd", want: "inside the personal store", input: bash(`sed ':x;/\` + "\n" + `w @H/notes/.attest/e\` + "\n" + `/p' f`)},
		{name: "sed append text", want: "/notes/MEMORY.md, inside the personal store", input: bash(`sed '1a eww' ~/notes/MEMORY.md`)},

		// Input the guard cannot read.
		{name: "malformed command", want: "neither a string nor a list of strings", input: `{"command":5}`},

		// Base-denied commands stay denied.
		{name: "sed e command relative store name", want: "cannot resolve, and names ", cwd: ".", input: bash(`sed -n '1e rm -rf notes' f`)},
		{name: "python shutil relative store name", want: "cannot resolve, and names ", cwd: ".", input: bash(`python3 -c 'import shutil; shutil.rmtree("notes")'`)},
		{name: "node rmSync relative store name", want: "cannot resolve, and names ", cwd: ".", input: bash(`node -e 'require("fs").rmSync("notes",{recursive:true})'`)},
		{name: "sed w hardlink operand", want: "a hard link to", cwd: "work", input: bash(`sed -n 'w hard' f`)},
		{name: "sed w symlink attest operand", want: "inside the personal store", cwd: "work", input: bash(`sed -n 'w rel' f`)},
		{name: "python glued out root", want: "which holds", cwd: ".", input: bash(`python3 tool.py --out=/`)},
		{name: "python short glued out root", want: "which holds", cwd: ".", input: bash(`python3 tool.py -o/`)},
		{name: "tar glued chdir root", want: "which holds", cwd: ".", input: bash(`tar -C/ -xf a.tar`)},
		{name: "tar long directory root", want: "which holds", cwd: ".", input: bash(`tar --directory=/ -xf a.tar`)},
		{name: "cp glued target root", want: "which holds", cwd: ".", input: bash(`cp -rt/ x`)},
		{name: "cp long target-directory root", want: "which holds", cwd: ".", input: bash(`cp --target-directory=/ x`)},
		{name: "posix order sed file operand", want: "inside the personal store", cwd: "work", input: bash(`POSIXLY_CORRECT=1 sed -i --follow-symlinks s/a/b/ -e P`)},
		{name: "env posix order sed file operand", want: "inside the personal store", cwd: "work", input: bash(`env POSIXLY_CORRECT=1 sed -i --follow-symlinks s/a/b/ -e P`)},

		// Protected variables set under spelled names.
		{name: "quote-split exported PATH", want: "/notes/MEMORY.md, inside the personal store", input: bash(`export "PA"TH=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "brace-built exported PATH", want: "/notes/MEMORY.md, inside the personal store", input: bash(`export {PA,}TH=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "ansi-c declared PATH", want: "/notes/MEMORY.md, inside the personal store", input: bash(`declare $'\x50ATH'=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "quote-split exported LD_PRELOAD", want: "/notes/MEMORY.md, inside the personal store", input: bash(`export "LD_"PRELOAD=/tmp/x.so; cat ~/notes/MEMORY.md`)},
		{name: "quote-split exported GIT_DIR", want: "/notes/MEMORY.md, inside the personal store", input: bash(`export "GIT_"DIR=/tmp/x; cat ~/notes/MEMORY.md`)},
		{name: "quote-split exported CDPATH", want: "which holds the personal store", input: bash(`export "CD"PATH=~; cd notes && touch MEMORY.md`)},
		{name: "quote-split declared CDPATH", want: "which holds the personal store", input: bash(`declare 'CD'PATH=~; cd notes && touch MEMORY.md`)},
		{name: "BASH_CMDS renaming a reader", want: "/notes/MEMORY.md, inside the personal store", input: bash(`BASH_CMDS[ls]=/bin/rm; ls ~/notes/MEMORY.md`)},
		{name: "read before a data position", want: "which holds the personal store", cwd: ".", input: bash(`n=1; { read n; cp /tmp/evil "$n"; } < f; gh pr view 1 --jq .`)},

		// Setters read after the words they change, and arithmetic.
		{name: "function body before a quote-split CDPATH", want: "which holds the personal store", input: bash(`f(){ cd notes && touch MEMORY.md; }; export "CD"PATH=~; f`)},
		{name: "function body before a default CDPATH", want: "which holds the personal store", input: bash(`f(){ cd notes && touch MEMORY.md; }; : ${CDPATH:=~}; f`)},
		{name: "arithmetic substitution", want: "which holds the personal store", cwd: ".", input: bash(`(( $(rm -rf .) ))`)},
		{name: "quoted paren beside an arithmetic substitution", want: "/no*s, inside the personal store", input: bash(`(( $(echo "(") + $(cd ..; rm -rf no*s) ))`)},

		// POSIX classes in a glob.
		{name: "posix class rm", want: "inside the personal store", input: bash("rm -rf ~/note[[:alpha:]]")},

		// A writer's option values, glued or in the next word.
		{name: "env cluster chdir", want: "runs in", cwd: ".", input: bash(`env -vCnotes touch MEMORY.md`)},
		{name: "env abbreviated glued chdir", want: "runs in", cwd: ".", input: bash(`env --ch=notes touch MEMORY.md`)},
		{name: "relative write through an abbreviated option dir", want: "/notes/MEMORY.md, inside the personal store", cwd: ".", input: bash(`env --ch work rm ../notes/MEMORY.md`)},
		{name: "relative write through a cluster option dir", want: "/notes/MEMORY.md, inside the personal store", cwd: ".", input: bash(`env -vC work rm ../notes/MEMORY.md`)},
		{name: "env split string cluster chdir", want: "and runs in", cwd: ".", input: bash(`env -S '-Cnotes touch MEMORY.md'`)},
		{name: "setsid env glued chdir", want: `the personal store (from the argument "-Cnotes")`, cwd: ".", input: bash(`setsid env -Cnotes touch MEMORY.md`)},
		{name: "sudo cluster glued chdir", want: `the personal store (from the argument "-iDnotes")`, cwd: ".", input: bash(`sudo -iDnotes touch MEMORY.md`)},
		{name: "sudo abbreviated chdir", want: "runs in", cwd: ".", input: bash(`sudo --chd=notes touch MEMORY.md`)},
		{name: "unshare wd", want: `the personal store (from the argument "--wd=notes")`, cwd: ".", input: bash(`unshare --wd=notes touch MEMORY.md`)},
		{name: "uv directory", want: `the personal store (from the argument "--directory=notes")`, cwd: ".", input: bash(`uv --directory=notes run touch MEMORY.md`)},
		{name: "npm prefix", want: `the personal store (from the argument "--prefix=notes")`, cwd: ".", input: bash(`npm --prefix=notes exec touch MEMORY.md`)},
		{name: "relative write through an option dir", want: "/notes/MEMORY.md, inside the personal store", cwd: ".", input: bash(`unshare --wd=work rm ../notes/MEMORY.md`)},
		{name: "option dir resolves against cwd", want: "/notes/MEMORY.md, inside the personal store", cwd: ".", input: bash(`env -vCwork -C work2 rm ../notes/MEMORY.md`)},
		{name: "option dir resolves against a cd dir", want: "/notes/MEMORY.md, inside the personal store", cwd: ".", input: bash(`cd work && env --ch=sub rm ../../notes/MEMORY.md`)},
		{name: "cd after many option dirs", want: "and runs in", input: bash(optionWrites() + "; cd .. && cd link && make")},
		{name: "writer option before a store path names it", want: "the command writes", input: bash(`rm -rf ~/notes/x`)},
		{name: "writer option before a relative store path names it", want: `(from the argument "notes/x")`, cwd: ".", input: bash(`rm -rf notes/x`)},
		{name: "env glued split string", want: "which holds the personal store", cwd: ".", input: bash(`env -u X --split-string='rm -rf .'`)},
		{name: "cluster past the suffix bound", want: "the personal store", cwd: ".", input: bash(`env -vvvvvvvvvvvvvvvvvvvvCnotes rm -rf facts`)},
		{name: "cluster past the suffix bound after cd", want: "the personal store", input: bash(`cd .. && env -vvvvvvvvvvvvvvvvvvvvCnotes rm -rf facts`)},
		// A dir or script in a word of its own is read already.
		{name: "env abbreviated separate chdir", want: "the personal store", cwd: ".", input: bash(`env --ch notes touch MEMORY.md`)},
		{name: "env cluster separate chdir", want: "the personal store", cwd: ".", input: bash(`env -vC notes touch MEMORY.md`)},
		{name: "nice env abbreviated chdir", want: "the personal store", cwd: ".", input: bash(`nice env --ch notes touch MEMORY.md`)},
		{name: "sudo cluster separate chdir", want: "the personal store", cwd: ".", input: bash(`sudo -iD notes touch MEMORY.md`)},
		{name: "sudo separate chdir", want: "the personal store", cwd: ".", input: bash(`sudo -D notes touch MEMORY.md`)},
		{name: "env separate split string", want: "which holds the personal store", cwd: ".", input: bash(`env -u X --split-string 'rm -rf .'`)},

		// Heredoc bodies, glob names and redirect targets read at run time.
		{name: "glob-named data taker", want: "which holds the personal store", cwd: ".", input: bash(`cp /bin/rm ~/bin/g?; gh pr view 1 --jq .`)},
		{name: "bracket-named data taker", want: "which holds the personal store", cwd: ".", input: bash(`cp /bin/rm ~/bin/[g]h; gh pr view 1 --jq .`)},
		{name: "extglob-named data taker", want: "which holds the personal store", cwd: ".", input: bash(`cp /bin/rm ~/bin/!(x); gh pr view 1 --jq .`)},
		{name: "redirect to an opaque target", want: "which holds the personal store", cwd: ".", input: bash(`cat /bin/rm > ~/bin/g$(echo h); gh pr view 1 --jq .`)},

		// An unresolved target from a dir holding a store.
		{name: "loop redirect before a gh filter from home", want: "which holds the personal store", cwd: ".", input: bash(`for n in a b; do echo x > ~/work/$n.log; done; gh pr view 1 --jq .`)},

		// A prefix PATH assignment before a reader of a store.
		{name: "prefix PATH before a reader of a store", want: "/notes/MEMORY.md, inside the personal store", input: bash(`PATH=/x cat ~/notes/MEMORY.md`)},
		{name: "prefix PATH before a builtin", want: "/notes/MEMORY.md, inside the personal store", input: bash(`PATH=/x true; cat ~/notes/MEMORY.md`)},

		// An unknown program's option value naming a dir it may run in.
		{name: "glued option dir", want: "/notes/MEMORY.md, inside the personal store", cwd: ".", input: bash(`foo -Cwork rm ../notes/MEMORY.md`)},
		{name: "option dir as the next word", want: "/notes/x, inside the personal store", cwd: ".", input: bash(`foo -C work rm ../notes/x`)},
		{name: "option dir holding a hard link", want: "a hard link to", cwd: "elsewhere", input: bash(`foo --dir=../work touch hard`)},
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
		{name: "past the parse budget and not writing", input: bash(strings.Repeat("$(", 300) + "true" + strings.Repeat(")", 300))},
		{name: "sibling store name", input: bash("echo x > ~/notes-old/MEMORY.md")},
		{name: "store name prefix", input: bash("mkdir -p ~/notesx && echo x > ~/notesx/f")},
		{name: "posix class glob in project", input: bash("rm -f ~/work/[[:alpha:]]*.o")},
		{name: "attest prefix name", input: bash("touch ~/work/sub/.attestation/y")},
		{name: "attest dir elsewhere", input: bash("touch /srv/other/.attest/e")},
		{name: "attest dir by glob elsewhere", input: bash("rm -rf ~/work/.a*")},
		{name: "text naming an attest dir", input: bash("echo see .attest/ > ~/work/a.md")},
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
		{name: "jq filter after an assignment", input: bash(`CREW_ID=x crew roster | jq -c '.[] | select(.state=="working" or .state=="blocked") | {name,branch,state,age_s}'`)},

		// Data positions of a cleanly resolved known program are text, not a path.
		{name: "gh jq with a slash alternative", input: bash(`gh pr view 1 --json state,mergeCommit --jq '"\(.state) \(.mergeCommit.oid // "-")"'`)},
		{name: "crew body with a spaced slash", cwd: ".", input: bash(`crew reply worker:x "a / b"`)},
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

		// Arithmetic that sets no protected name.
		{name: "c-style loop then ls of a store", input: bash(`for ((i=0; i<3; i++)); do echo $i; done; ls ~/notes`)},
		{name: "arithmetic expansion then cat of a store", input: bash(`n=3; echo $(( n + 1 )); cat ~/notes/MEMORY.md`)},
		{name: "integer test then rg of a store", input: bash(`[[ 1 -lt 2 ]] && rg x ~/notes`)},

		// Writers with option values that name no store.
		{name: "go test with options", input: bash(`go test ./... -run TestX -count=1 -v`)},
		{name: "go test of a notes package", input: bash(`go test ./internal/notes/ -run TestX -count=1 -v`)},
		{name: "go test vet and build", input: bash(`go test ./... -run TestX -count=1 -timeout 30s -v && go vet ./... && go build -o bin/x ./cmd/x`)},
		{name: "go test of a notes package with a timeout", input: bash(`go test ./internal/notes/ -run TestX -count=1 -timeout 30s -v`)},
		{name: "go test race of notes packages", input: bash(`go test -race -timeout 60s ./internal/notes/...`)},
		{name: "gh pr create with many options and a store path in the body", input: bash(`gh pr create --title t --body "see ~/notes/facts/a.md" --base main --head b --assignee me --label a --label b --label c --reviewer r1 --reviewer r2 --milestone m`)},
		{name: "crew await with a timeout", input: bash(`crew await X --timeout 300`)},
		{name: "crew status with a flag", input: bash(`crew status X working --restamp`)},
		{name: "gh pr create with a body file", input: bash(`gh pr create --title t --body-file /tmp/b`)},
		{name: "gh pr view with json and jq", input: bash(`gh pr view 1 --json url --jq .url`)},
		{name: "rm of build dirs", input: bash(`rm -rf build dist`)},
		{name: "mkdir then copy", input: bash(`mkdir -p a/b && cp -r src a/b`)},
		{name: "git commit message", input: bash(`git commit -m "x"`)},
		{name: "nix develop go vet", input: bash(`nix develop -c go vet ./...`)},
		{name: "writers with many option values", input: bash(optionWrites())},

		// Heredoc bodies, globs and redirect targets that name no data taker.
		{name: "object glob before a gh filter", input: bash(`rm -f *.o; gh pr view 1 --jq .`)},
		{name: "object glob before a gh filter from home", cwd: ".", input: bash(`rm -f *.o; gh pr view 1 --jq .`)},
		{name: "source glob listing before a gh filter from home", cwd: ".", input: bash(`ls *.go; gh pr view 1 --jq .`)},
		{name: "heredoc with a variable to a file from home", cwd: ".", input: bash("cat <<EOF > /tmp/x\nhello $USER\nEOF")},
		{name: "loop commit with a here-string", input: bash(`for f in a b; do git commit -m "$f" <<< x; done`)},
		{name: "commit message heredoc with a substitution", input: bash("git commit -F - <<EOF\n$(git log -1 --format=%s)\nEOF")},
		{name: "python heredoc with a substitution", input: bash("python3 - <<EOF\nprint(\"$(date)\")\nEOF")},

		// Readers, data and prose the default-deny rule does not read as
		// targets, and a ~ bash leaves literal after --opt=.
		{name: "option value", input: bash("cp --target-directory=~/notes/facts x")},
		{name: "env split string runs a crew body", input: bash(`env -S 'crew reply x /'`)},
		{name: "gh path after double dash", input: bash(`gh api x -q / -- cat ~/notes/MEMORY.md`)},
		{name: "awk address program", input: bash(`awk '/^foo$/{print $1}' f`)},
		{name: "awk division program", input: bash(`awk '{print $1 / $2}' f`)},
		{name: "awk glued field separator", input: bash(`awk -F/ '{print $NF}' f`)},
		{name: "deferred sed script redefines", input: bash(`git commit -m '~/notes/MEMORY.md' && sed -i '#/crew' f`)},
		{name: "sed in-place script trailing slash regex", input: bash(`sed -i 's/^        monthlyTarget = 100;$/        monthlyTarget = 30;/' home/ai/claude-code/default.nix`)},
		{name: "sed -i pipe delimiter", input: bash("sed -i 's|^        monthlyTarget = 100;$|        monthlyTarget = 30;|' home/ai/claude-code/default.nix")},
		{name: "sed -i hash delimiter", input: bash("sed -i 's#^        monthlyTarget = 100;$#        monthlyTarget = 30;#' home/ai/claude-code/default.nix")},
		{name: "sed -i g flag", input: bash("sed -i 's/a/b/g' @H/elsewhere/a")},
		{name: "sed -i numeric flag", input: bash("sed -i 's/a/b/2' @H/elsewhere/a")},
		{name: "sed -i I flag", input: bash("sed -i 's/a/b/I' @H/elsewhere/a")},
		{name: "export -n then cat of a store", input: bash(`export -n FOO; cat ~/notes/facts/a.md`)},
		{name: "jq alternative filter after an assignment", input: bash(`CREW_ID=x crew roster | jq -c '.[]? // .'`)},
		{name: "inline code naming no path from home", cwd: ".", input: bash(`python3 -c 'print(1)'`)},
		{name: "option value naming a project dir from home", cwd: ".", input: bash(`rsync -a --exclude=work src/ dst/`)},
		{name: "option value naming no dir", input: bash(`foo --exclude=notes rm x`)},
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
	for _, c := range []call{
		{name: "write index", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "content": "x"})},
		{name: "write git", want: "/hooks/pre-commit, inside the personal store's git dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/.git/hooks/pre-commit", "content": "x"})},
		{name: "write state", want: "/provenance/s, inside priors' state dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/.local/state/priors/provenance/s", "content": "x"})},
		{name: "write config dir", want: "/rules.toml, inside priors' config dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/.config/priors/rules.toml", "content": "x"})},
		{name: "write ancestor", want: "which holds the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes", "content": "x"})},
		{name: "symlinked dir", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/link/MEMORY.md", "content": "x"})},
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
		{name: "edit index", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "old_string": "a", "new_string": "b"})},
		{name: "notebook in state", want: "/n.ipynb, inside priors' state dir", tool: "NotebookEdit", input: sb.input(map[string]any{"notebook_path": "@H/.local/state/priors/n.ipynb", "new_source": "x"})},
		{name: "unknown tool command", want: "the command writes", tool: "mcp__exec", input: bash("echo x > ~/notes/MEMORY.md")},

		// Tools and keys beyond Claude's own.
		{name: "unknown path key", want: "/notes/MEMORY.md, the personal store's generated index", cwd: "notes", tool: "mcp__serena__create_text_file", input: sb.input(map[string]any{"relative_path": "MEMORY.md", "content": "x"})},
		{name: "unknown path key camel", want: "/notes/MEMORY.md, the personal store's generated index", cwd: "notes", tool: "mcp__jetbrains__create_new_file", input: sb.input(map[string]any{"pathInProject": "MEMORY.md", "text": "x"})},
		{name: "unknown path key in array", want: "/notes/MEMORY.md, the personal store's generated index", cwd: "notes", tool: "mcp__fs__create_many", input: sb.input(map[string]any{"names": []any{"MEMORY.md"}, "content": "x"})},
		{name: "unknown tool by name", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__fs__search_and_replace", input: sb.input(map[string]any{"path": "@H/notes/MEMORY.md"})},
		{name: "cursor search replace", want: "/notes/MEMORY.md, the personal store's generated index", tool: "search_replace", input: sb.input(map[string]any{"file_path": "@H/notes/MEMORY.md", "old_string": "a", "new_string": "b"})},
		{name: "edit text named twice", want: "names an edit text more than once", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/work/a.md", "old_string": "a", "oldString": "b", "new_string": "c"})},
		{name: "near-miss subagent name", want: "/notes/MEMORY.md, the personal store's generated index", tool: "subagentx", input: sb.input(map[string]any{"task": "touch @H/notes/MEMORY.md"})},
		{name: "near-miss subagents name", want: "/notes/MEMORY.md, the personal store's generated index", tool: "subagents", input: sb.input(map[string]any{"task": "touch @H/notes/MEMORY.md"})},

		// Strings the guard must read wherever they sit.
		{name: "nested path array", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__batch", input: sb.input(map[string]any{"files": []any{[]any{"~/notes/MEMORY.md", "x"}}})},
		{name: "newline in state path", want: "inside priors' state dir", tool: "mcp__fs__create_doc", input: sb.input(map[string]any{"uri": "@H/.local/state/priors/local/personal/z\n.md"})},
		{name: "padded path", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__fs__create_doc", input: sb.input(map[string]any{"uri": " @H/notes/MEMORY.md\t"})},
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
		{name: "oversize json in a string", want: "past what the guard reads", tool: "mcp__x__do", input: sb.input(map[string]any{"args": "[" + strings.Repeat("1,", 17000) + `"@H/notes/MEMORY.md"]`})},
		{name: "deep json in a string", want: "past what the guard reads", tool: "mcp__x__do", input: sb.input(map[string]any{"args": strings.Repeat("[", 20) + `"@H/notes/MEMORY.md"` + strings.Repeat("]", 20)})},
		{name: "file url with one slash", want: "/notes/MEMORY.md, the personal store's generated index", tool: "Write", input: sb.input(map[string]any{"file_path": "file:@H/notes/MEMORY.md", "content": "x"})},
		{name: "file url with one slash in a uri", want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__x__do", input: sb.input(map[string]any{"uri": "file:@H/notes/MEMORY.md"})},
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
	c := call{want: "/notes/MEMORY.md, the personal store's generated index", tool: "mcp__filesystem__write_file", input: sb.input(map[string]any{"path": "@H/notes/MEMORY.md", "Path": "/tmp/safe"})}
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

		// #289: attestation is gone, so `reviewed` is a plain label and
		// .attest names no guarded root.
		{name: "reviewed fact", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\nname: n\nmetadata:\n  confidence: reviewed\n---\nbody\n"})},
		{name: "reviewed yaml escape", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\nname: n\nmetadata:\n  confidence: \"re\\x76iewed\"\n---\n"})},
		{name: "reviewed through a hard link", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hardfact", "content": "---\nname: b\nmetadata:\n  confidence: reviewed\n---\nbody\n"})},
		{name: "edit to reviewed", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "old_string": "unverified", "new_string": "reviewed"})},
		{name: "pi edit to reviewed", tool: "edit", input: sb.input(map[string]any{"path": "@H/notes/facts/b.md", "edits": []any{
			map[string]any{"oldText": "unverified", "newText": "reviewed"},
		}})},
		{name: "patch to reviewed", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/b.md\n@@\n-  confidence: unverified\n+  confidence: reviewed\n*** End Patch"})},
		{name: "write into a leftover attest dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/.attest/e2", "content": "x"})},
		{name: "write an attest dir elsewhere", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/.attest/e", "content": "x"})},
		{name: "content key naming an attest dir", tool: "mcp__srv__do", input: sb.input(map[string]any{"text": "~/work/.attest/e"})},
		{name: "free text naming an attest dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/b.md", "content": "entries live in .attest/ dirs"})},
		{name: "unreviewed fact", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/notes/facts/n.md", "content": "---\nname: n\nmetadata:\n  confidence: unverified\n---\nThe PR was reviewed.\n"})},
		{name: "unreviewed fact through a hard link", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/hardfact", "content": "---\nname: b\nmetadata:\n  confidence: unverified\n---\nmore\n"})},
		{name: "benign edit", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/notes/facts/b.md", "old_string": "body", "new_string": "more body"})},
		{name: "read attest", tool: "Read", input: sb.input(map[string]any{"file_path": "@H/notes/.attest/e1"})},
		{name: "grep store", tool: "Grep", input: sb.input(map[string]any{"pattern": "x", "path": "@H/notes"})},
		{name: "patch benign", tool: "Write", input: sb.input(map[string]any{"input": "*** Begin Patch\n*** Update File: @H/notes/facts/b.md\n@@\n-body\n+more body\n*** End Patch"})},
		{name: "read tool by name", tool: "mcp__github__get_file_contents", input: sb.input(map[string]any{"path": "/"})},
		{name: "subagent prompt with a bare slash", tool: "subagent", input: sb.input(map[string]any{"task": "Run `priors import` / `priors index`, then continue."})},
		{name: "subagent prompt naming home", tool: "subagent", input: sb.input(map[string]any{"task": "Read @H for context, then check the / separator."})},
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

		// JSON files written whole: their content is not read as input.
		{name: "json file with a root url", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/cfg.json", "content": `{"start_url": "/"}`})},
		{name: "json file with a home root", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/cfg.json", "content": `{"root": "~"}`})},
		{name: "json file extending a parent", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/cfg.json", "content": `{"extends": ".."}`})},
		{name: "json file with a relative dir", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/cfg.json", "content": `{"dir":"../notes"}`})},
		{name: "json fixture naming an attest entry", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/fixture.json", "content": `{"entries":["notes/.attest/e1"]}`})},
		{name: "large json file", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/big.json", "content": "[" + strings.Repeat("1,", 17000) + "1]"})},
		{name: "deep json file", tool: "Write", input: sb.input(map[string]any{"file_path": "@H/work/deep.json", "content": strings.Repeat("[", 20) + strings.Repeat("]", 20)})},
		{name: "edit inserting json", tool: "Edit", input: sb.input(map[string]any{"file_path": "@H/work/a.md", "old_string": "x", "new_string": `{"a": "/"}`})},
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
		{"Bash", bash("git commit -m x"), true},
		{"Bash", bash(strings.Repeat("$(", 300) + "true" + strings.Repeat(")", 300)), true},
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
	c := sb.guard.newCheck(sb.work, nil, false)
	c.budget = 1
	if to, unknown := c.linked(filepath.Join(sb.work, "hardfact")); len(to) != 0 || !unknown {
		t.Errorf("linked = %v, %v; want unknown", to, unknown)
	}
	c = sb.guard.newCheck(sb.work, nil, false)
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

// goBody is a Go-like heredoc body of exactly n bytes with no line equal to EOF.
func goBody(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString("func f" + strconv.Itoa(i) + "() string { return \"a/b/c\" }\n")
	}
	return b.String()[:n-1] + "\n"
}

func TestRedesignAllows(t *testing.T) {
	sb := newSandbox(t)
	for _, c := range []call{
		{name: "sed regex over nix store paths", input: bash(`nix path-info -r . | sed -E 's|.*/nix/store/[a-z0-9]{32}-||' | sort`)},
		{name: "grep store path into nix-store roots", input: bash(`grep -E '/nix/store/[a-z0-9]+' f | nix-store --query --roots`)},
		{name: "awk store path into nix-store deriver", input: bash(`awk '/\/nix\/store\// {print $1}' f | nix-store -q --deriver`)},
		{name: "gh repo create description with slashes", input: bash(`gh repo create owner/repo --private --description "A / B / C"`)},
		{name: "git -C elsewhere remote set-url and fetch", input: bash(`git -C @H/elsewhere remote set-url origin git@github.com:owner/repo.git && git -C @H/elsewhere fetch`)},
		{name: "gh pr list in a loop with jq", input: bash(`for r in a b; do gh pr list --repo owner/$r --json number | jq '.[] | .number'; done`)},
		{name: "xargs worktree repair", input: bash(`ls -d @H/work/* | tr '\n' '\0' | xargs -0 git worktree repair`)},
		{name: "mkdir cp sed -i elsewhere", input: bash(`mkdir -p @H/elsewhere/s && cp a b c @H/elsewhere/s/ && sed -i 's/x/y/' @H/elsewhere/s/a`)},
		{name: "sed -i with pipe delimiter on a project file", input: bash(`sed -i 's|^- \[x\].*|- [ ] y|' @H/work/a.md`)},
		{name: "heredoc 1837 bytes", input: bash("cat > f <<'EOF'\n" + goBody(1837) + "EOF")},
		{name: "heredoc 8192 bytes", input: bash("cat > f <<'EOF'\n" + goBody(8192) + "EOF")},
		{name: "priors list in the store", cwd: "notes", input: bash("priors list")},
		{name: "rm -rf build from work", input: bash("rm -rf build/")},
		{name: "rm -rf build from home", cwd: ".", input: bash("rm -rf build/")},
		{name: "cp -r from work", input: bash("cp -r src/ out")},
		{name: "cp -r from home", cwd: ".", input: bash("cp -r src/ out")},
		{name: "awk division", input: bash(`awk '{print $1 / $2}' f`)},
		{name: "awk field separator slash", input: bash(`awk -F/ '{print $NF}' f`)},
		{name: "arithmetic loop", input: bash(`for i in 1 2 3; do echo $((i*2)); done`)},
		{name: "cat store into tmp", input: bash(`cat ~/notes/MEMORY.md > /tmp/x`)},

		// An unresolved name that can start no allowlisted program keeps the
		// allowlist.
		{name: "jq division into a numbered tmp file", input: bash(`n=1; jq '.a / .b' f > "/tmp/out-$n"`)},

		// Environment values that name no store.
		{name: "pager prefix", input: bash(`GIT_PAGER=cat git log`)},
		{name: "locale prefix", input: bash(`LANG=C sort f`)},
		{name: "PATH prefix with a home bin", input: bash(`PATH="$HOME/bin:$PATH" cmd`)},
		{name: "prefix to one program, store read by another", input: bash(`PAGER=cat make build && grep x ~/notes/MEMORY.md`)},
		{name: "token from a substitution in home", cwd: ".", input: bash(`GH_TOKEN=$(gh auth token) gh pr list`)},
		{name: "version from a substitution in home", cwd: ".", input: bash(`VERSION=$(date +%s) make build`)},
		{name: "cdable_vars to a name nothing sets", input: bash(`shopt -s cdable_vars; cd build && make`)},
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

func TestRedesignDenies(t *testing.T) {
	sb := newSandbox(t)
	const (
		store    = "inside the personal store"
		pastRead = "past what the guard reads"
	)
	nested := `{"path":"@H\/notes\/MEMORY.md","pad":` + strings.Repeat("[", 18) + strings.Repeat("]", 18) + `}`
	decoys := strings.TrimSpace(strings.Repeat("/x ", 300))
	long := strings.Repeat("v", 40)
	for _, c := range []call{
		{name: "redirect into store", want: store, input: bash("> @H/notes/x")},
		{name: "cp into store", want: "the personal store", input: bash("cp x @H/notes/")},
		{name: "sed -i fact", want: store, input: bash("sed -i s/a/b/ @H/notes/facts/a.md")},
		{name: "tee into store", want: store, input: bash("echo x | tee @H/notes/x")},

		{name: "tool arguments nested past the read depth", want: pastRead, tool: "mcp__x__do", input: sb.input(map[string]any{"arguments": nested})},
		{name: "tool note behind 300 decoy tokens", want: "the personal store", tool: "mcp__x__do", input: sb.input(map[string]any{"note": decoys + " @H/notes/MEMORY.md"})},

		{name: "env option cluster run in the store via link", want: "runs in", cwd: ".", input: bash("env -" + long + "Clink rm -rf facts")},
		{name: "unknown command option cluster with link", want: "the personal store", cwd: ".", input: bash("foo -" + long + "Clink rm -rf facts")},

		{name: "positional in arithmetic in a function", want: "runs f on inline code, which the guard cannot resolve, and names", input: bash(`f() { (( $1 )); }; f 'a[$(rm -rf ../notes)]'`)},
		{name: "PS4 substitution via env", want: "/notes, the personal store", input: bash(`env SHELLOPTS=xtrace PS4='$(rm -rf ../notes)' bash -c true`)},

		{name: "packed key=path operand", want: "the personal store", input: bash("foo key=@H/notes/x")},
		{name: "packed out:path operand", want: "the personal store", input: bash("foo out:@H/notes/x")},
		{name: "packed comma path operand", want: "the personal store", input: bash("foo a,@H/notes/x")},
		{name: "gawk -o packed path", want: "the personal store", input: bash("gawk -o@H/notes/x '{print}' f")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.tool == "" {
				c.tool = "Bash"
			}
			wantDenied(t, c, sb.run(t, c))
		})
	}
}

// TestStoreWritesStayDenied holds store writes a gap in rendering a word
// would let through: a ~ bash expands after name= or :, an environment value
// a child reads as a path, and a cd cdable_vars resolves.
func TestStoreWritesStayDenied(t *testing.T) {
	sb := newSandbox(t)
	const store = "the personal store"
	for _, c := range []call{
		{name: "dd of= tilde", want: store, input: bash("dd if=/dev/zero of=~/notes/MEMORY.md")},
		{name: "unknown program name= tilde", want: store, input: bash("foo out=~/notes/MEMORY.md")},
		{name: "unknown program tilde after a colon", want: store, input: bash("foo a=b:~/notes/MEMORY.md")},
		{name: "make install PREFIX tilde", want: store, input: bash("make install PREFIX=~/notes")},

		{name: "prefix work tree", want: store, input: bash("GIT_WORK_TREE=~/notes git checkout -- .")},
		{name: "env work tree and git dir", want: store, input: bash("env GIT_WORK_TREE=~/notes GIT_DIR=~/notes/.git git checkout -- .")},
		{name: "prefix git dir from HOME", want: store, input: bash("GIT_DIR=$HOME/notes/.git git reset --hard")},
		{name: "exported work tree", want: store, input: bash("export GIT_WORK_TREE=~/notes; git checkout -- .")},
		{name: "prefix DESTDIR", want: store, input: bash("DESTDIR=~/notes make install")},

		{name: "cdable_vars to an assigned name", want: store, input: bash("shopt -s cdable_vars; d=$HOME/notes; cd d && touch new.md")},
		{name: "cdable_vars to a host name", want: store, cwd: "elsewhere", input: bash("shopt -s cdable_vars; cd NOTES; rm facts/a.md")},
		{name: "tilde in an option cluster", want: store, input: bash(`foo -o~/notes/MEMORY.md`)},
		{name: "tilde in an ssh option cluster", want: store, input: bash(`ssh -M -S~/notes/x host`)},
		{name: "quoted tilde in a -C cluster", want: store, input: bash(`foo -C'~/notes' build`)},
		{name: "quoted ~user word", want: store, input: bash(`foo '~alice/notes/MEMORY.md'`)},
		{name: "ssh option value with ~user", want: store, input: bash(`ssh -o 'UserKnownHostsFile=~alice/notes/MEMORY.md' host`)},
		{name: "tilde value naming a program", want: store, input: bash(`X=~/bin/cat make; cat ~/notes/MEMORY.md`)},

		{name: "loop variable exported before", want: store, input: bash("export GIT_WORK_TREE; for GIT_WORK_TREE in ~/notes; do git checkout -- .; done")},
		{name: "loop variable exported inside", want: store, input: bash("for GIT_WORK_TREE in ~/notes; do export GIT_WORK_TREE; git checkout -- .; done")},
		{name: "loop git dir", want: store, input: bash("export GIT_DIR; for GIT_DIR in ~/notes/.git; do git reset --hard; done")},
		{name: "nested shell -a", want: store, input: bash("bash -a -c 'GIT_WORK_TREE=~/notes; git checkout -- .'")},
		{name: "nested shell -ac", want: store, input: bash("bash -ac 'GIT_WORK_TREE=~/notes; git checkout -- .'")},
		{name: "nested shell -o allexport", want: store, input: bash("bash -o allexport -c 'GIT_WORK_TREE=~/notes; git checkout -- .'")},
		{name: "nested shell -O cdable_vars to HOME", want: store, input: bash("bash -O cdable_vars -c 'cd HOME; echo x > notes/MEMORY.md'")},
		{name: "nested sh -O cdable_vars to HOME", want: store, input: bash("sh -O cdable_vars -c 'cd HOME; touch notes/facts/n.md'")},
		{name: "shopt glob operand", want: store, input: bash("touch cdable_vars; shopt -s cdable_var?; cd HOME && echo x > notes/MEMORY.md")},
		{name: "nested shell -O cdable_vars to an assigned name", want: store, input: bash("bash -O cdable_vars -c 'd=$HOME/notes; cd d && touch new.md'")},
		{name: "prefix value named by a nested cd", want: store, input: bash("d=~/notes bash -O cdable_vars -c 'cd d && touch new.md'")},
		{name: "env value named by a nested cd", want: store, input: bash("env d=~/notes bash -O cdable_vars -c 'cd d && touch new.md'")},
		{name: "prefix value to cd naming it", want: store, input: bash("shopt -s cdable_vars; d=~/notes cd d && touch new.md")},
		{name: "unknown program double quoted subscript tilde", want: store, input: bash(`foo a["x"]=~/notes/MEMORY.md`)},
		{name: "unknown program single quoted subscript tilde", want: store, input: bash(`foo a['x']=~/notes/MEMORY.md`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.tool = "Bash"
			wantDenied(t, c, sb.run(t, c))
		})
	}
}

// These are the commands the owner saw refused on 2026-11-09/10 (#288), none of which writes near a store.
func TestOwnerRefusalsAllowed(t *testing.T) {
	sb := newSandbox(t)
	for _, c := range []call{
		{name: "git -C a dir set by an assignment", input: bash(`W=@H/elsewhere; git -C $W status; git ls-remote origin refs/heads/x`)},
		{name: "polling loop with arithmetic", input: bash(`for i in $(seq 1 60); do echo "$((i*15))s"; sleep 15; done`)},
		{name: "inline awk and tr pipeline", input: bash(`awk '$3 ~ /19:21:(1[1-9]|[2-5][0-9])/' f | tr -s ' '`)},
		{name: "jq alternative operator beside a url built in a loop", input: bash(`H=abc; for c in a.example b.example; do curl -sS "https://$c/$H.narinfo" | jq -r '.x // "-"'; done`)},
		{name: "https url in a curl write-out format", input: bash(`for c in https://a.example https://b.example; do for h in s7 5i; do curl -sS -m 20 -o /dev/null -w "$c $h %{http_code}\n" "$c/$h.narinfo"; done; done`)},
		{name: "drv assignment echoed and used", input: bash(`cd @H/work; nix eval --json .#x.outputs 2>/dev/null; echo; D=$(nix eval --raw .#x.drvPath 2>/dev/null); echo "drv=$D"; for c in https://a.example https://b.example; do for h in s7 5i; do curl -sS -m 20 "$c/$h.narinfo" -o /dev/null -w "$c $h %{http_code}\n"; done; done; echo '--- restricted dry-run'; nix build --dry-run --option substituters https://a.example "$D^*" 2>&1 | cut -c1-160 | head -12`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.tool = "Bash"
			if r := sb.run(t, c); r != "" {
				t.Errorf("denied %s: %s", c.input, r)
			}
		})
	}
}

// TestProvenStoreWritesDenied holds the writes whose store target the guard can prove.
func TestProvenStoreWritesDenied(t *testing.T) {
	sb := newSandbox(t)
	const store = "inside the personal store"
	for _, c := range []call{
		{name: "redirect into a fact", want: store, input: bash(`echo x > ~/notes/facts/n.md`)},
		{name: "append to the index", want: store, input: bash(`echo x >> ~/notes/MEMORY.md`)},
		{name: "cp into a fact", want: store, input: bash(`cp /tmp/x ~/notes/facts/n.md`)},
		{name: "mv into a fact", want: store, input: bash(`mv /tmp/x ~/notes/facts/n.md`)},
		{name: "rm a fact", want: store, input: bash(`rm ~/notes/facts/a.md`)},
		{name: "sed -i a fact", want: store, input: bash(`sed -i s/a/b/ ~/notes/facts/a.md`)},
		{name: "tee into a fact", want: store, input: bash(`echo x | tee ~/notes/facts/n.md`)},
		{name: "git -C store commit", want: "runs in", input: bash(`git -C ~/notes commit -m x`)},
		{name: "git commit after cd into the store", want: "runs in", input: bash(`cd ~/notes && git commit -am x`)},
		{name: "sed w into a fact", want: "cannot resolve, and names", input: bash(`sed -n 'w @H/notes/facts/a.md' f`)},
		{name: "write into a leftover attest dir", want: "inside the personal store", input: bash(`touch ~/notes/.attest/e9`)},
		{name: "opaque name inside the store", want: "reaches into the personal store", input: bash(`echo x > ~/notes/facts/$(date +%s).md`)},
		{name: "literal cd into the store after many relative cds", want: "runs in", input: bash(`cd x1; cd x2; cd x3; cd x4; cd x5; cd x6; cd ~/notes; echo x > MEMORY.md`)},
		{name: "literal cd into the store after many relative cds then rm", want: "runs in", input: bash(`cd x1; cd x2; cd x3; cd x4; cd x5; cd x6; cd ~/notes; rm facts/a.md`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.tool = "Bash"
			wantDenied(t, c, sb.run(t, c))
		})
	}
}

func TestOptionValuesFast(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector slows judging")
	}
	sb := newSandbox(t)
	cmd := "rsync -a"
	for i := range 250 {
		cmd += " --exclude=u" + strconv.Itoa(i)
	}
	cmd += " src/ dst/"
	c := call{tool: "Bash", input: bash(cmd)}
	start := time.Now()
	r := sb.run(t, c)
	if d := time.Since(start); d > 750*time.Millisecond {
		t.Errorf("Check took %s; want under 750ms", d)
	}
	if r != "" {
		t.Errorf("denied: %s", r)
	}
}
