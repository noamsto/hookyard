package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/commit/committest"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
)

var binPath string

func TestMain(m *testing.M) {
	os.Exit(buildAndRun(m))
}

func buildAndRun(m *testing.M) int {
	committest.UnsetRepoEnv()
	dir, err := os.MkdirTemp("", "priors-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	binPath = filepath.Join(dir, "priors")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

const (
	personalRemote = "git@github.com:noamsto/demo.git"
	workRemote     = "git@github.com:factify-inc/app.git"
	cleanScanner   = "#!/bin/sh\ncat >/dev/null\necho '[]'\n"
	dirtyScanner   = "#!/bin/sh\ncat >/dev/null\necho '[{\"RuleID\":\"fake-rule\",\"File\":\"x\"}]'\n"
)

type result struct {
	stdout, stderr string
	code           int
}

// sandbox is an isolated HOME, state, config and pair of stores that the
// priors binary runs in. Nothing is inherited from the test process but PATH.
type sandbox struct {
	t                              *testing.T
	dir                            string
	personal, work, state          string
	hookyard, gitConfig, sshConfig string
	configPath, scanner, path      string
	profile                        string
}

func newSandbox(t *testing.T, profile string) *sandbox {
	t.Helper()
	// Resolved because priors reports store paths resolved, and the temp dir
	// may sit behind a symlink (macOS /var).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sb := &sandbox{
		t:          t,
		dir:        dir,
		personal:   filepath.Join(dir, "personal"),
		work:       filepath.Join(dir, "work"),
		state:      filepath.Join(dir, "state", "priors"),
		hookyard:   filepath.Join(dir, "hookyard"),
		gitConfig:  filepath.Join(dir, "gitconfig"),
		sshConfig:  filepath.Join(dir, "ssh_config"),
		configPath: filepath.Join(dir, "config.toml"),
		scanner:    filepath.Join(dir, "scanner"),
		path:       os.Getenv("PATH"),
		profile:    profile,
	}
	sb.writeFile(sb.gitConfig, "[user]\n\tname = Test\n\temail = test@example.com\n[commit]\n\tgpgsign = false\n")
	sb.writeFile(sb.sshConfig, "")
	sb.setScanner(cleanScanner)
	for _, d := range []string{sb.personal, sb.work} {
		sb.mkdir(d)
		sb.git(d, "init", "-q")
	}
	sb.writeConfig()
	return sb
}

func (sb *sandbox) mkdir(dir string) {
	sb.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		sb.t.Fatal(err)
	}
}

func (sb *sandbox) writeFile(path, content string) {
	sb.t.Helper()
	sb.mkdir(filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		sb.t.Fatal(err)
	}
}

func (sb *sandbox) setScanner(script string) {
	sb.t.Helper()
	sb.writeFile(sb.scanner, script)
	if err := os.Chmod(sb.scanner, 0o755); err != nil {
		sb.t.Fatal(err)
	}
}

// writeConfig rewrites the config; extra lines are appended to it.
func (sb *sandbox) writeConfig(extra ...string) {
	sb.t.Helper()
	lines := []string{
		fmt.Sprintf("profile = %q", sb.profile),
		fmt.Sprintf("personal_store = %q", sb.personal),
		fmt.Sprintf("work_store = %q", sb.work),
		fmt.Sprintf("state_dir = %q", sb.state),
		`work_orgs = ["github.com/factify-inc"]`,
		`personal_orgs = ["github.com/noamsto"]`,
		fmt.Sprintf("ssh_config = %q", sb.sshConfig),
		fmt.Sprintf("scanner = %q", sb.scanner),
	}
	sb.writeFile(sb.configPath, strings.Join(append(lines, extra...), "\n")+"\n")
}

func (sb *sandbox) childEnv() []string {
	return []string{
		"HOME=" + sb.dir,
		"XDG_STATE_HOME=" + filepath.Join(sb.dir, "xdg-state"),
		"XDG_CONFIG_HOME=" + filepath.Join(sb.dir, "xdg-config"),
		"HOOKYARD_STATE_DIR=" + sb.hookyard,
		"GIT_CONFIG_GLOBAL=" + sb.gitConfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"PATH=" + sb.path,
		"PRIORS_CONFIG=" + sb.configPath,
	}
}

func (sb *sandbox) git(dir string, args ...string) {
	sb.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = sb.childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		sb.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repo makes a git repo whose origin is remote.
func (sb *sandbox) repo(remote string) string {
	sb.t.Helper()
	dir := sb.t.TempDir()
	sb.git(dir, "init", "-q")
	sb.git(dir, "remote", "add", "origin", remote)
	return dir
}

func (sb *sandbox) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Env = sb.childEnv()
	cmd.Dir = sb.dir
	return cmd
}

func (sb *sandbox) run(stdin string, args ...string) result {
	sb.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := sb.command(ctx, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			sb.t.Fatalf("running priors %v: %v", args, err)
		}
		res.code = exit.ExitCode()
	}
	return res
}

// record writes a hookyard event record for session with one Bash tool call.
func (sb *sandbox) record(session string) {
	sb.t.Helper()
	line := fmt.Sprintf(`{"session_id":%q,"canonical_event":"post_tool","tool_name":"Bash"}`+"\n", session)
	path := filepath.Join(sb.hookyard, "stream", time.Now().UTC().Format(time.DateOnly)+".jsonl")
	sb.mkdir(filepath.Dir(path))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		sb.t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		sb.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		sb.t.Fatal(err)
	}
	sb.toolCall("post_tool", session, "Bash", `{"command":"go test ./..."}`)
}

// toolCall feeds the bare handler a tool event's envelope, as hookyard does
// around every tool call.
func (sb *sandbox) toolCall(event, session, tool, input string) {
	sb.t.Helper()
	sb.send(sb.toolEnvelope(event, session, tool, input, `"ok"`))
}

func (sb *sandbox) toolEnvelope(event, session, tool, input, response string) string {
	return fmt.Sprintf(`{"engine":"claude-code","canonical_event":%q,"session_id":%q,"cwd":%q,"tool_name":%q,"tool_input":%s,"native":{"tool_response":%s}}`,
		event, session, sb.dir, tool, input, response)
}

// send runs the bare handler on envelope, which must stay silent and succeed.
func (sb *sandbox) send(envelope string) {
	sb.t.Helper()
	res := sb.run(envelope)
	wantExit(sb.t, res, 0)
	if res.stdout != "" {
		sb.t.Fatalf("tool event handler printed %q", res.stdout)
	}
}

func today() string { return time.Now().UTC().Format(time.DateOnly) }

func newFact(name, repo, typ string) fact.Fact {
	return fact.Fact{
		Name:        name,
		Description: "description of " + name,
		Metadata: fact.Metadata{
			NodeType:   "memory",
			Type:       typ,
			Scope:      "repo",
			Repos:      []string{repo},
			ValidFrom:  today(),
			Verified:   today(),
			Confidence: "proposed",
			Provenance: &fact.Provenance{Engine: "claude", Session: "s", Host: "h"},
			Modified:   time.Now().UTC().Format(time.RFC3339Nano),
		},
		Body: "body of " + name + "\n",
	}
}

// putFact writes f at root/rel and returns the path.
func (sb *sandbox) putFact(root, rel string, f fact.Fact) string {
	sb.t.Helper()
	data, err := f.Marshal()
	if err != nil {
		sb.t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	sb.writeFile(path, string(data))
	return path
}

func (sb *sandbox) indexWrite() {
	sb.t.Helper()
	if res := sb.run("", "index", "--write"); res.code != 0 {
		sb.t.Fatalf("index --write: exit %d: %s", res.code, res.stderr)
	}
}

func sessionStartEnvelope(cwd string) string {
	return fmt.Sprintf(`{"engine":"claude-code","canonical_event":"session_start","session_id":"s1","cwd":%q}`, cwd)
}

// additionalContext is the injected text of a hook reply, which must have
// exactly the documented shape.
func additionalContext(t *testing.T, stdout string) string {
	t.Helper()
	var reply map[string]map[string]string
	if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
		t.Fatalf("stdout is not a hook reply: %v\n%s", err, stdout)
	}
	inner, ok := reply["hookSpecificOutput"]
	if !ok || len(reply) != 1 || len(inner) != 1 {
		t.Fatalf("unexpected reply shape: %s", stdout)
	}
	text, ok := inner["additionalContext"]
	if !ok || text == "" {
		t.Fatalf("no additionalContext: %s", stdout)
	}
	return text
}

func wantContains(t *testing.T, what, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Errorf("%s lacks %q:\n%s", what, s, got)
		}
	}
}

func wantExit(t *testing.T, res result, code int) {
	t.Helper()
	if res.code != code {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, code, res.stdout, res.stderr)
	}
}

func TestAddPublishesWithFlagsAndStdin(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")

	res := sb.run("the body\n", "add", "--stdin", "--name", "flag-fact", "--description", "from flags",
		"--type", "project", "--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)
	path := filepath.Join(sb.personal, "demo", "flag-fact.md")
	if want := "published personal " + path + "\n"; res.stdout != want {
		t.Fatalf("stdout %q, want %q", res.stdout, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "fact file", string(raw), "from flags", "the body", "repos: [demo]")
}

func TestAddParsesFrontmatterOnStdinAndFlagsOverride(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")

	f := newFact("whole-fact", "demo", "reference")
	f.Description = "from frontmatter"
	f.Metadata.Verified = ""
	data, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	res := sb.run(string(data), "add", "--stdin", "--description", "from the flag", "--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)
	raw, err := os.ReadFile(filepath.Join(sb.personal, "demo", "whole-fact.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	wantContains(t, "fact file", got, "from the flag", "type: reference", "body of whole-fact")
	if strings.Contains(got, "from frontmatter") {
		t.Errorf("flag did not override the frontmatter description:\n%s", got)
	}
}

// TestAddEngineIdentityWins: inside Claude Code the engine's own session id
// is gate 2's subject, so a caller cannot borrow another session's clean
// record by asserting an identity; one that tries is flagged.
func TestAddEngineIdentityWins(t *testing.T) {
	for name, c := range map[string]struct {
		args, env  []string
		conflicted bool
	}{
		"--engine codex --session":  {args: []string{"--engine", "codex", "--session", "sess-1"}, conflicted: true},
		"PRIORS_ENGINE codex":       {env: []string{"PRIORS_ENGINE=codex", "PRIORS_SESSION=sess-1"}, conflicted: true},
		"--engine claude --session": {args: []string{"--engine", "claude", "--session", "sess-1"}, conflicted: true},
		"bare":                      {},
	} {
		t.Run(name, func(t *testing.T) {
			sb := newSandbox(t, "personal")
			repo := sb.repo(personalRemote)
			sb.record("sess-1")

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			args := append([]string{"add", "--name", "claude-fact", "--description", "d", "--type", "project", "--cwd", repo}, c.args...)
			cmd := sb.command(ctx, args...)
			cmd.Env = append(cmd.Env, append([]string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=engine-sess"}, c.env...)...)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("priors add: %v", err)
			}
			path := filepath.Join(sb.state, "local", "personal", "demo", "claude-fact.md")
			stdout := string(out)
			if !strings.HasPrefix(stdout, "flagged personal "+path+": ") {
				t.Fatalf("stdout %q, want the fact flagged at %s", stdout, path)
			}
			wantContains(t, "stdout", stdout, "provenance:no-tool-record")
			if got := strings.Contains(stdout, "provenance:asserted-identity"); got != c.conflicted {
				t.Errorf("asserted-identity flag = %v, want %v: %q", got, c.conflicted, stdout)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wantContains(t, "fact file", string(raw), "session: engine-sess", "engine: claude")
		})
	}
}

func TestResolveIdentity(t *testing.T) {
	type want struct {
		engine, session string
		conflict        bool
	}
	cases := []struct {
		name                    string
		env                     map[string]string
		flagEngine, flagSession string
		want                    want
	}{
		{name: "engine env, nothing asserted",
			env:  map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "engine-sess"},
			want: want{"claude", "engine-sess", false}},
		{name: "engine env, agreeing flags",
			env:        map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "engine-sess"},
			flagEngine: "claude", flagSession: "engine-sess",
			want: want{"claude", "engine-sess", false}},
		{name: "engine env, other engine flag",
			env:        map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "engine-sess"},
			flagEngine: "codex",
			want:       want{"claude", "engine-sess", true}},
		{name: "engine env, other session flag",
			env:         map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "engine-sess"},
			flagSession: "sess-1",
			want:        want{"claude", "engine-sess", true}},
		{name: "engine env, other PRIORS_ENGINE",
			env:  map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "engine-sess", "PRIORS_ENGINE": "codex"},
			want: want{"claude", "engine-sess", true}},
		{name: "engine env, other PRIORS_SESSION",
			env:  map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "engine-sess", "PRIORS_SESSION": "sess-1"},
			want: want{"claude", "engine-sess", true}},
		{name: "marker without id",
			env:         map[string]string{"CLAUDECODE": "1"},
			flagSession: "sess-1",
			want:        want{"claude", "sess-1", false}},
		{name: "id without marker",
			env:        map[string]string{"CLAUDE_CODE_SESSION_ID": "engine-sess"},
			flagEngine: "codex", flagSession: "sess-1",
			want: want{"codex", "sess-1", false}},
		{name: "no engine env, PRIORS values",
			env:  map[string]string{"PRIORS_ENGINE": "codex", "PRIORS_SESSION": "sess-1"},
			want: want{"codex", "sess-1", false}},
		{name: "no engine env, flags beat PRIORS values",
			env:        map[string]string{"PRIORS_ENGINE": "codex", "PRIORS_SESSION": "sess-1"},
			flagEngine: "cursor", flagSession: "sess-2",
			want: want{"cursor", "sess-2", false}},
		{name: "nothing",
			want: want{"unknown", "", false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "PRIORS_ENGINE", "PRIORS_SESSION"} {
				t.Setenv(k, c.env[k])
			}

			id, err := resolveIdentity(t.TempDir(), c.flagEngine, c.flagSession)
			if err != nil {
				t.Fatal(err)
			}

			if got := (want{id.engine, id.session, id.conflict}); got != c.want {
				t.Errorf("identity = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestSymlinkedStoreBehavesLikeItsTarget: a store configured through a
// symlink is indexed, listed and linted as the directory it names.
func TestSymlinkedStoreBehavesLikeItsTarget(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")
	sb.putFact(sb.personal, "demo/old-fact.md", newFact("old-fact", "demo", "project"))
	sb.putFact(sb.personal, "demo/bad-fact.md", newFact("bad-fact", "demo", "bogus"))
	sb.indexWrite()
	direct := sb.run("", "lint")

	target := sb.personal
	sb.personal = filepath.Join(sb.dir, "personal-link")
	if err := os.Symlink(target, sb.personal); err != nil {
		t.Fatal(err)
	}
	sb.writeConfig()

	if linked := sb.run("", "lint"); direct.stdout == "" || linked != direct {
		t.Errorf("lint through the symlink = %+v, want the direct result %+v", linked, direct)
	}

	res := sb.run("", "add", "--name", "new-fact", "--description", "d", "--type", "project", "--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)
	wantContains(t, "add", res.stdout, "published personal "+filepath.Join(target, "demo", "new-fact.md"))
	index, err := os.ReadFile(filepath.Join(target, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "MEMORY.md", string(index), "old-fact", "new-fact")

	listed := sb.run("", "list", "--cwd", repo)
	wantExit(t, listed, 0)
	wantContains(t, "list", listed.stdout, "demo/old-fact.md", "demo/new-fact.md")
}

func TestAddFlagsUnknownSessionAndListFlaggedShowsReasons(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)

	res := sb.run("", "add", "--name", "unsure-fact", "--description", "no session", "--type", "project", "--cwd", repo)
	wantExit(t, res, 0)
	path := filepath.Join(sb.state, "local", "personal", "demo", "unsure-fact.md")
	want := "flagged personal " + path + ": provenance:unknown-session\n"
	if res.stdout != want {
		t.Fatalf("stdout %q, want %q", res.stdout, want)
	}

	listed := sb.run("", "list", "--flagged", "--cwd", repo)
	wantExit(t, listed, 0)
	wantContains(t, "list --flagged", listed.stdout, "personal local demo/unsure-fact.md", "flags: provenance:unknown-session")
}

func TestAddQuarantinesWorkRepoOnPersonalHost(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(workRemote)
	sb.record("sess-1")

	res := sb.run("", "add", "--name", "work-fact", "--description", "from work", "--type", "project",
		"--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)
	path := filepath.Join(sb.state, "quarantine", "app", "work-fact.md")
	if !strings.HasPrefix(res.stdout, "quarantined "+path+": work repo on a personal host") {
		t.Fatalf("stdout %q", res.stdout)
	}
}

// TestSymlinkedStateDirIsOneDir: the marker handler and the write path both
// land under the symlink's target.
func TestSymlinkedStateDirIsOneDir(t *testing.T) {
	sb := newSandbox(t, "personal")
	real := filepath.Join(sb.dir, "real-state")
	sb.mkdir(real)
	sb.state = filepath.Join(sb.dir, "state-link")
	if err := os.Symlink(real, sb.state); err != nil {
		t.Fatal(err)
	}
	sb.writeConfig()
	repo := sb.repo(workRemote)
	sb.record("sess-1")

	res := sb.run("", "add", "--name", "work-fact", "--description", "from work", "--type", "project",
		"--cwd", repo, "--session", "sess-1")
	sb.toolCall("pre_tool", "sess-1", "Bash", `{"command":"gh issue view 12"}`)

	wantExit(t, res, 0)
	path := filepath.Join(real, "quarantine", "app", "work-fact.md")
	if !strings.HasPrefix(res.stdout, "quarantined "+path+": work repo on a personal host") {
		t.Fatalf("stdout %q", res.stdout)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}
	wantMarkerExts(t, sb.markerFiles(filepath.Join(real, "provenance")), ".seen", ".ingest")
}

// TestStateDirInARepoFailsClosed: a state dir that resolves into a git work
// tree stops every command, and the marker handler writes nothing, neither
// there nor at the default state path.
func TestStateDirInARepoFailsClosed(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	inside := filepath.Join(repo, "sub")
	sb.mkdir(inside)
	sb.state = filepath.Join(sb.dir, "state-link")
	if err := os.Symlink(inside, sb.state); err != nil {
		t.Fatal(err)
	}
	sb.writeConfig()

	res := sb.run("", "add", "--name", "a-fact", "--description", "d", "--type", "project",
		"--cwd", repo, "--session", "sess-1")
	sb.toolCall("pre_tool", "sess-1", "Bash", `{"command":"gh issue view 12"}`)

	wantExit(t, res, 2)
	wantContains(t, "stderr", res.stderr, "inside the git work tree")
	for _, dir := range []string{inside, filepath.Join(sb.dir, "xdg-state")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("%s holds %v, want it untouched", dir, entries)
		}
	}
	top, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 {
		t.Errorf("repo holds %v, want only .git and sub", top)
	}
}

// TestLayerDirInARepoRefusesTheWrite: a quarantine or local dir that is a
// symlink into a checkout, or holds its own .git, never receives a fact.
func TestLayerDirInARepoRefusesTheWrite(t *testing.T) {
	linkInto := func(t *testing.T, layer, repo string) {
		if err := os.Symlink(repo, layer); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		layer  string
		remote string
		args   []string
		plant  func(t *testing.T, sb *sandbox, layer, repo string)
	}{
		{"quarantine symlinked into a repo", "quarantine", workRemote, []string{"--session", "sess-1"},
			func(t *testing.T, _ *sandbox, layer, repo string) { linkInto(t, layer, repo) }},
		{"local symlinked into a repo", "local", personalRemote, nil,
			func(t *testing.T, _ *sandbox, layer, repo string) { linkInto(t, layer, repo) }},
		{"quarantine holding a .git", "quarantine", workRemote, []string{"--session", "sess-1"},
			func(_ *testing.T, sb *sandbox, layer, _ string) { sb.mkdir(filepath.Join(layer, ".git")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t, "personal")
			sb.record("sess-1")
			repo := sb.repo(tc.remote)
			other := sb.repo(personalRemote)
			sb.mkdir(sb.state)
			tc.plant(t, sb, filepath.Join(sb.state, tc.layer), other)

			args := append([]string{"add", "--name", "a-fact", "--description", "d", "--type", "project", "--cwd", repo}, tc.args...)
			res := sb.run("", args...)
			if res.code == 0 {
				t.Fatalf("exit 0, want a refusal; stdout %q", res.stdout)
			}
			top, err := os.ReadDir(other)
			if err != nil {
				t.Fatal(err)
			}
			if len(top) != 1 {
				t.Errorf("repo holds %v, want only .git", top)
			}
		})
	}
}

// TestLayerDirInARepoRefusesLintAndIndex: lint --move-flagged and index --write
// re-check the local layer dir like add does, and refuse a planted one.
func TestLayerDirInARepoRefusesLintAndIndex(t *testing.T) {
	plants := []struct {
		name  string
		plant func(t *testing.T, sb *sandbox) (target string, wantTop []string)
	}{
		{"local symlinked into a checkout", func(t *testing.T, sb *sandbox) (string, []string) {
			other := sb.repo(personalRemote)
			if err := os.Symlink(other, filepath.Join(sb.state, "local")); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(other, "personal")
			sb.mkdir(target)
			return other, []string{".git", "personal"}
		}},
		{"local layer holding a .git", func(_ *testing.T, sb *sandbox) (string, []string) {
			layer := filepath.Join(sb.state, "local", "personal")
			sb.mkdir(layer)
			sb.git(layer, "init", "-q")
			return layer, []string{".git"}
		}},
	}
	commands := []struct {
		name string
		seed func(sb *sandbox)
		args []string
		held func(t *testing.T, sb *sandbox)
	}{
		{"lint --move-flagged", func(sb *sandbox) {
			flagged := newFact("linky-fact", "demo", "project")
			flagged.Body = "see https://example.com/docs for details\n"
			sb.putFact(sb.personal, "demo/linky-fact.md", flagged)
			sb.indexWrite()
		}, []string{"lint", "--move-flagged"}, func(t *testing.T, sb *sandbox) {
			if _, err := os.Stat(filepath.Join(sb.personal, "demo", "linky-fact.md")); err != nil {
				t.Errorf("flagged fact left the checkout: %v", err)
			}
		}},
		// The checkout's own index is written before the local root is
		// reached, so only the planted target is asserted untouched.
		{"index --write", func(sb *sandbox) {
			sb.putFact(sb.personal, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))
		}, []string{"index", "--write"}, func(*testing.T, *sandbox) {}},
	}
	for _, p := range plants {
		for _, c := range commands {
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				sb := newSandbox(t, "personal")
				c.seed(sb)
				sb.mkdir(sb.state)
				target, wantTop := p.plant(t, sb)

				res := sb.run("", c.args...)
				if res.code == 0 {
					t.Errorf("exit 0, want a refusal; stdout %q", res.stdout)
				}
				if !strings.Contains(res.stderr, "symlink") && !strings.Contains(res.stderr, "inside the git work tree") {
					t.Errorf("stderr lacks a refusal reason:\n%s", res.stderr)
				}
				c.held(t, sb)
				top, err := os.ReadDir(target)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, e := range top {
					got = append(got, e.Name())
				}
				if !slices.Equal(got, wantTop) {
					t.Errorf("%s holds %v, want %v", target, got, wantTop)
				}
				if len(wantTop) == 2 {
					inner, err := os.ReadDir(filepath.Join(target, "personal"))
					if err != nil {
						t.Fatal(err)
					}
					if len(inner) != 0 {
						t.Errorf("%s/personal holds %v, want empty", target, inner)
					}
				}
			})
		}
	}
}

func TestAddRefusals(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")
	base := []string{"add", "--name", "a-fact", "--description", "d", "--cwd", repo, "--session", "sess-1"}

	t.Run("lint failure", func(t *testing.T) {
		res := sb.run("", append(base, "--type", "bogus")...)
		wantExit(t, res, 1)
		wantContains(t, "stderr", res.stderr, "refused:")
		if res.stdout != "" {
			t.Errorf("stdout %q", res.stdout)
		}
	})
	t.Run("secret scanner hit", func(t *testing.T) {
		sb.setScanner(dirtyScanner)
		defer sb.setScanner(cleanScanner)
		res := sb.run("", append(base, "--type", "project")...)
		wantExit(t, res, 1)
		wantContains(t, "stderr", res.stderr, "refused:", "fake-rule")
	})
	t.Run("scanner missing", func(t *testing.T) {
		good := sb.scanner
		sb.scanner = "/nonexistent/scanner"
		sb.writeConfig()
		defer func() {
			sb.scanner = good
			sb.writeConfig()
		}()
		res := sb.run("", append(base, "--type", "project")...)
		wantExit(t, res, 1)
		wantContains(t, "stderr", res.stderr, "refused: secret scanner unavailable:")
	})
	t.Run("rule set missing", func(t *testing.T) {
		sb.writeConfig(`rules = "/nonexistent/rules.toml"`)
		defer sb.writeConfig()
		res := sb.run("", append(base, "--type", "project")...)
		wantExit(t, res, 1)
		wantContains(t, "stderr", res.stderr, "refused: redaction rule set unavailable:")
	})
	t.Run("no repo", func(t *testing.T) {
		res := sb.run("", "add", "--name", "a-fact", "--description", "d", "--type", "project",
			"--cwd", t.TempDir(), "--session", "sess-1")
		wantExit(t, res, 1)
		wantContains(t, "stderr", res.stderr, "no repo: pass --repo or --scope global")
	})
	t.Run("config error", func(t *testing.T) {
		sb.writeFile(sb.configPath, "profile = \"bogus\"\n")
		defer sb.writeConfig()
		res := sb.run("", append(base, "--type", "project")...)
		wantExit(t, res, 2)
	})
	t.Run("unknown flag", func(t *testing.T) {
		wantExit(t, sb.run("", "add", "--nope"), 1)
	})
}

func TestUnknownSubcommandPrintsUsage(t *testing.T) {
	sb := newSandbox(t, "personal")
	res := sb.run("", "frobnicate")
	wantExit(t, res, 1)
	wantContains(t, "stderr", res.stderr, "usage: priors")
}

func TestBareWithoutPipedStdinIsUsageError(t *testing.T) {
	sb := newSandbox(t, "personal")
	res := sb.run("")
	wantExit(t, res, 1)
	wantContains(t, "stderr", res.stderr, "usage: priors")
}

func TestWriteRoutingAndReadRule(t *testing.T) {
	sb := newSandbox(t, "work")
	personalRepo := sb.repo(personalRemote)
	workRepo := sb.repo(workRemote)
	sb.record("sess-p")
	sb.record("sess-w")

	res := sb.run("", "add", "--name", "personal-fact", "--description", "a personal fact", "--type", "user",
		"--scope", "global", "--cwd", personalRepo, "--session", "sess-p")
	wantExit(t, res, 0)
	wantContains(t, "add from personal repo", res.stdout, "published personal "+filepath.Join(sb.personal, "_global", "personal-fact.md"))

	res = sb.run("", "add", "--name", "work-fact", "--description", "a work fact", "--type", "project",
		"--cwd", workRepo, "--session", "sess-w")
	wantExit(t, res, 0)
	wantContains(t, "add from work repo", res.stdout, "published work "+filepath.Join(sb.work, "app", "work-fact.md"))

	personal := sb.run("", "index", "--text", "--cwd", personalRepo)
	wantExit(t, personal, 0)
	wantContains(t, "personal session index", personal.stdout, "personal-fact")
	if strings.Contains(personal.stdout, "work-fact") {
		t.Errorf("personal-repo session reads the work fact:\n%s", personal.stdout)
	}

	work := sb.run("", "index", "--text", "--cwd", workRepo)
	wantExit(t, work, 0)
	wantContains(t, "work session index", work.stdout, "personal-fact", "work-fact")
}

func TestListFilters(t *testing.T) {
	sb := newSandbox(t, "work")
	repo := sb.repo(workRemote)

	live := newFact("live-fact", "app", "project")
	sb.putFact(sb.work, "app/live-fact.md", live)
	refFact := newFact("reference-fact", "app", "reference")
	sb.putFact(sb.work, "app/reference-fact.md", refFact)
	sb.putFact(sb.personal, "_global/personal-global.md", func() fact.Fact {
		f := newFact("personal-global", "", "user")
		f.Metadata.Scope = "global"
		f.Metadata.Repos = nil
		return f
	}())
	other := newFact("other-repo-fact", "elsewhere", "project")
	sb.putFact(sb.work, "elsewhere/other-repo-fact.md", other)
	old := newFact("old-fact", "app", "project")
	old.Metadata.Verified = "2000-01-01"
	sb.putFact(sb.work, "app/old-fact.md", old)
	archived := newFact("archived-fact", "app", "project")
	sb.putFact(sb.work, "_archive/archived-fact.md", archived)
	superseded := newFact("superseded-fact", "app", "project")
	superseded.Metadata.SupersededBy = "live-fact"
	sb.putFact(sb.work, "app/superseded-fact.md", superseded)
	nullVerified := newFact("unverified-fact", "app", "project")
	path := sb.putFact(sb.work, "app/unverified-fact.md", nullVerified)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "verified: "+today(), "verified: null", 1)
	if edited == string(raw) {
		t.Fatalf("verified line not found in:\n%s", raw)
	}
	sb.writeFile(path, edited)

	list := func(args ...string) string {
		t.Helper()
		res := sb.run("", append([]string{"list", "--cwd", repo}, args...)...)
		wantExit(t, res, 0)
		return res.stdout
	}

	t.Run("default", func(t *testing.T) {
		out := list()
		wantContains(t, "list", out, "[priors memory · list]", "===== BEGIN priors-",
			"work app/live-fact.md — project — verified "+today()+" — description of live-fact",
			"work app/reference-fact.md", "personal _global/personal-global.md")
		for _, name := range []string{"other-repo-fact", "archived-fact", "superseded-fact"} {
			if strings.Contains(out, name) {
				t.Errorf("list shows %s:\n%s", name, out)
			}
		}
	})
	t.Run("stale", func(t *testing.T) {
		out := list("--stale")
		wantContains(t, "list --stale", out, "old-fact", "unverified-fact", "verified never")
		for _, name := range []string{"live-fact", "reference-fact"} {
			if strings.Contains(out, name) {
				t.Errorf("list --stale shows the fresh %s:\n%s", name, out)
			}
		}
	})
	t.Run("repo", func(t *testing.T) {
		out := list("--repo", "elsewhere")
		wantContains(t, "list --repo", out, "other-repo-fact")
		if strings.Contains(out, "live-fact") {
			t.Errorf("list --repo elsewhere shows the app's facts:\n%s", out)
		}
	})
	t.Run("type", func(t *testing.T) {
		out := list("--type", "reference")
		wantContains(t, "list --type", out, "reference-fact")
		if strings.Contains(out, "live-fact") || strings.Contains(out, "personal-global") {
			t.Errorf("list --type reference shows other types:\n%s", out)
		}
	})
	t.Run("store work", func(t *testing.T) {
		out := list("--store", "work")
		wantContains(t, "list --store work", out, "work app/live-fact.md")
		if strings.Contains(out, "personal") {
			t.Errorf("list --store work shows personal facts:\n%s", out)
		}
	})
	t.Run("all", func(t *testing.T) {
		wantContains(t, "list --all", list("--all"), "archived-fact", "superseded-fact")
	})
	t.Run("nothing to list prints nothing", func(t *testing.T) {
		if out := list("--type", "feedback"); out != "" {
			t.Errorf("stdout %q", out)
		}
	})
}

func TestShow(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.putFact(sb.personal, "demo/shown-fact.md", newFact("shown-fact", "demo", "project"))

	res := sb.run("", "show", "shown-fact", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "show", res.stdout, "[priors memory · personal store · demo/shown-fact.md]", "===== BEGIN priors-",
		"name: shown-fact", "body of shown-fact", "===== END priors-")

	missing := sb.run("", "show", "no-such-fact", "--cwd", repo)
	wantExit(t, missing, 1)
	if missing.stdout != "" {
		t.Errorf("stdout %q", missing.stdout)
	}
}

func TestSearch(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	hit := newFact("needle-fact", "demo", "project")
	hit.Body = "the zebra crossing rule\n"
	sb.putFact(sb.personal, "demo/needle-fact.md", hit)
	sb.putFact(sb.personal, "demo/haystack-fact.md", newFact("haystack-fact", "demo", "project"))

	res := sb.run("", "search", "zebra", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "search", res.stdout, "[priors memory · search: zebra]", "===== BEGIN priors-",
		"personal/demo/needle-fact.md — needle-fact — description of needle-fact")
	if strings.Contains(res.stdout, "haystack-fact") {
		t.Errorf("search shows a non-match:\n%s", res.stdout)
	}

	none := sb.run("", "search", "no-such-term", "--cwd", repo)
	wantExit(t, none, 0)
	if none.stdout != "" {
		t.Errorf("stdout %q", none.stdout)
	}
}

func TestSearchWithoutRgFailsOpen(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.putFact(sb.personal, "demo/needle-fact.md", newFact("needle-fact", "demo", "project"))

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(gitBin, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	sb.path = bin

	res := sb.run("", "search", "needle", "--cwd", repo)
	wantExit(t, res, 0)
	if res.stdout != "" {
		t.Errorf("stdout %q", res.stdout)
	}
	wantContains(t, "stderr", res.stderr, "search unavailable:")
}

func TestLintConfiguredStores(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.putFact(sb.personal, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))
	sb.indexWrite()

	clean := sb.run("", "lint")
	wantExit(t, clean, 0)
	if clean.stdout != "" {
		t.Errorf("stdout %q", clean.stdout)
	}

	bad := newFact("bad-fact", "demo", "bogus")
	sb.putFact(sb.personal, "demo/bad-fact.md", bad)
	sb.indexWrite()
	res := sb.run("", "lint")
	wantExit(t, res, 1)
	wantContains(t, "lint", res.stdout, "demo/bad-fact.md: type:")
}

func TestLintDirWithKind(t *testing.T) {
	sb := newSandbox(t, "personal")
	// The store-CI form needs no config, only a scanner on PATH.
	bin := t.TempDir()
	sb.writeFile(filepath.Join(bin, "betterleaks"), cleanScanner)
	if err := os.Chmod(filepath.Join(bin, "betterleaks"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb.path = bin + string(os.PathListSeparator) + sb.path
	sb.writeFile(sb.configPath, "not toml at all = = =\n")

	store := t.TempDir()
	sb.putFact(store, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))
	writeStoreIndex(t, sb, store)

	args := []string{"lint", "--dir", store, "--kind", "personal", "--work-org", "github.com/factify-inc"}
	wantExit(t, sb.run("", args...), 0)

	leaky := newFact("leaky-fact", "demo", "project")
	leaky.Body = "deploys to factify-inc infrastructure\n"
	sb.putFact(store, "demo/leaky-fact.md", leaky)
	writeStoreIndex(t, sb, store)
	res := sb.run("", args...)
	wantExit(t, res, 1)
	wantContains(t, "lint", res.stdout, "demo/leaky-fact.md: work-name:")

	if res := sb.run("", "lint", "--dir", store); res.code != 1 {
		t.Errorf("--dir without --kind: exit %d, want 1", res.code)
	}
}

// writeStoreIndex regenerates dir's MEMORY.md through a throwaway config
// that points personal_store at it.
func writeStoreIndex(t *testing.T, sb *sandbox, dir string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	sb.writeFile(cfg, fmt.Sprintf("profile = \"personal\"\npersonal_store = %q\nstate_dir = %q\nwork_orgs = [\"github.com/factify-inc\"]\n", dir, filepath.Join(t.TempDir(), "state")))
	if res := sb.run("", "index", "--write", "--config", cfg); res.code != 0 {
		t.Fatalf("index --write: %s", res.stderr)
	}
}

func TestLintMoveFlagged(t *testing.T) {
	sb := newSandbox(t, "personal")
	flagged := newFact("linky-fact", "demo", "project")
	flagged.Body = "see https://example.com/docs for details\n"
	sb.putFact(sb.personal, "demo/linky-fact.md", flagged)
	sb.putFact(sb.personal, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))
	sb.indexWrite()

	res := sb.run("", "lint")
	wantExit(t, res, 1)
	wantContains(t, "lint", res.stdout, "demo/linky-fact.md: gate:content:url:")

	moved := sb.run("", "lint", "--move-flagged")
	wantExit(t, moved, 0)
	wantContains(t, "lint --move-flagged", moved.stdout, "moved demo/linky-fact.md")
	if _, err := os.Stat(filepath.Join(sb.state, "local", "personal", "demo", "linky-fact.md")); err != nil {
		t.Errorf("fact not in the local layer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb.personal, "demo", "linky-fact.md")); !os.IsNotExist(err) {
		t.Errorf("fact still in the checkout: %v", err)
	}
}

func TestIndexWriteThenHookOutput(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.putFact(sb.personal, "demo/indexed-fact.md", newFact("indexed-fact", "demo", "project"))

	written := sb.run("", "index", "--write")
	wantExit(t, written, 0)
	wantContains(t, "index --write", written.stdout, "wrote "+filepath.Join(sb.personal, "MEMORY.md"))
	again := sb.run("", "index", "--write")
	wantExit(t, again, 0)
	if again.stdout != "" {
		t.Errorf("a second index --write rewrote something: %q", again.stdout)
	}

	envelope := sessionStartEnvelope(repo)
	viaIndex := sb.run(envelope, "index")
	wantExit(t, viaIndex, 0)
	wantContains(t, "index", additionalContext(t, viaIndex.stdout), "[indexed-fact](demo/indexed-fact.md)")

	viaBare := sb.run(envelope)
	wantExit(t, viaBare, 0)
	wantContains(t, "bare", additionalContext(t, viaBare.stdout), "[indexed-fact](demo/indexed-fact.md)")

	other := sb.run(`{"engine":"claude-code","canonical_event":"pre_tool","session_id":"s1","tool_name":"Bash"}`)
	wantExit(t, other, 0)
	if other.stdout != "" {
		t.Errorf("a pre_tool envelope printed %q", other.stdout)
	}
}

func TestIndexTextPrintsFencedBlock(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.putFact(sb.personal, "demo/indexed-fact.md", newFact("indexed-fact", "demo", "project"))
	sb.indexWrite()

	res := sb.run("", "index", "--text", "--cwd", repo)
	wantExit(t, res, 0)
	if !strings.HasPrefix(res.stdout, "[priors memory · personal store]") {
		t.Errorf("not a fenced block:\n%s", res.stdout)
	}
	if !regexp.MustCompile(`===== BEGIN priors-[0-9a-f]{16} =====`).MatchString(res.stdout) {
		t.Errorf("no fence:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "hookSpecificOutput") {
		t.Errorf("--text printed JSON:\n%s", res.stdout)
	}
}

// failOpenFixture is a personal store with two indexed facts and a session
// repo, so that a case can break one thing and expect silence.
type failOpenFixture struct {
	sb       *sandbox
	repo     string
	envelope string
}

func newFailOpenFixture(t *testing.T) *failOpenFixture {
	t.Helper()
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.putFact(sb.personal, "demo/alpha-fact.md", newFact("alpha-fact", "demo", "project"))
	sb.putFact(sb.personal, "demo/beta-fact.md", newFact("beta-fact", "demo", "project"))
	sb.indexWrite()
	return &failOpenFixture{sb: sb, repo: repo, envelope: sessionStartEnvelope(repo)}
}

// modes runs fn once per way the index handler can be invoked.
func (fx *failOpenFixture) modes() map[string][]string {
	return map[string][]string{"index": {"index"}, "bare": nil}
}

func TestFailOpenMatrix(t *testing.T) {
	isRoot := os.Geteuid() == 0

	silent := map[string]func(t *testing.T, fx *failOpenFixture) (stdin string){
		"no config file": func(t *testing.T, fx *failOpenFixture) string {
			if err := os.Remove(fx.sb.configPath); err != nil {
				t.Fatal(err)
			}
			return fx.envelope
		},
		"invalid config": func(t *testing.T, fx *failOpenFixture) string {
			fx.sb.writeFile(fx.sb.configPath, "profile = \"bogus\"\n")
			return fx.envelope
		},
		"missing store dir": func(t *testing.T, fx *failOpenFixture) string {
			if err := os.RemoveAll(fx.sb.personal); err != nil {
				t.Fatal(err)
			}
			return fx.envelope
		},
		"missing MEMORY.md": func(t *testing.T, fx *failOpenFixture) string {
			if err := os.Remove(filepath.Join(fx.sb.personal, "MEMORY.md")); err != nil {
				t.Fatal(err)
			}
			return fx.envelope
		},
		"MEMORY.md mode 000": func(t *testing.T, fx *failOpenFixture) string {
			if isRoot {
				t.Skip("root ignores file modes")
			}
			if err := os.Chmod(filepath.Join(fx.sb.personal, "MEMORY.md"), 0); err != nil {
				t.Fatal(err)
			}
			return fx.envelope
		},
		"garbage MEMORY.md": func(t *testing.T, fx *failOpenFixture) string {
			fx.sb.writeFile(filepath.Join(fx.sb.personal, "MEMORY.md"), "\x00\x01 garbage \xff\n- [x](y) —\n")
			return fx.envelope
		},
		"missing rules file": func(t *testing.T, fx *failOpenFixture) string {
			fx.sb.writeConfig(`rules = "/nonexistent/rules.toml"`)
			return fx.envelope
		},
		"garbage envelope": func(t *testing.T, fx *failOpenFixture) string {
			return "{{{ this is not json"
		},
		"envelope that is not an object": func(t *testing.T, fx *failOpenFixture) string {
			return `["session_start"]`
		},
	}

	for name, arrange := range silent {
		t.Run(name, func(t *testing.T) {
			fx := newFailOpenFixture(t)
			for mode, args := range fx.modes() {
				if res := fx.sb.run(fx.envelope, args...); res.code != 0 || res.stdout == "" {
					t.Fatalf("%s baseline: exit %d, stdout %q, stderr %q", mode, res.code, res.stdout, res.stderr)
				}
			}
			stdin := arrange(t, fx)
			for mode, args := range fx.modes() {
				res := fx.sb.run(stdin, args...)
				if res.code != 0 || res.stdout != "" {
					t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 0 and no output", mode, res.code, res.stdout, res.stderr)
				}
			}
		})
	}

	t.Run("fact file mode 000 drops only that line", func(t *testing.T) {
		if isRoot {
			t.Skip("root ignores file modes")
		}
		fx := newFailOpenFixture(t)
		if err := os.Chmod(filepath.Join(fx.sb.personal, "demo", "alpha-fact.md"), 0); err != nil {
			t.Fatal(err)
		}
		for mode, args := range fx.modes() {
			res := fx.sb.run(fx.envelope, args...)
			wantExit(t, res, 0)
			text := additionalContext(t, res.stdout)
			wantContains(t, mode+" output", text, "[beta-fact]")
			if strings.Contains(text, "alpha-fact") {
				t.Errorf("%s: the unreadable fact was injected:\n%s", mode, text)
			}
		}
	})

	t.Run("MEMORY.md replaced by a FIFO", func(t *testing.T) {
		fx := newFailOpenFixture(t)
		index := filepath.Join(fx.sb.personal, "MEMORY.md")
		if err := os.Remove(index); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(index, 0o644); err != nil {
			t.Fatal(err)
		}
		for mode, args := range fx.modes() {
			start := time.Now()
			res := fx.sb.run(fx.envelope, args...)
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("%s took %v on a FIFO index", mode, elapsed)
			}
			if res.code != 0 || res.stdout != "" {
				t.Errorf("%s: exit %d, stdout %q; want exit 0 and no output", mode, res.code, res.stdout)
			}
		}
	})
}

func globalFact(name string) fact.Fact {
	f := newFact(name, "", "project")
	f.Metadata.Scope = "global"
	f.Metadata.Repos = nil
	return f
}

func TestSymlinkedFactNeverRead(t *testing.T) {
	sb := newSandbox(t, "work")
	repo := sb.repo(personalRemote)
	target := sb.putFact(sb.work, "app/fact.md", globalFact("work-only-fact"))
	sb.mkdir(filepath.Join(sb.personal, "_global"))
	if err := os.Symlink(target, filepath.Join(sb.personal, "_global", "x.md")); err != nil {
		t.Fatal(err)
	}

	list := sb.run("", "list", "--cwd", repo)
	wantExit(t, list, 1)
	if strings.Contains(list.stdout, "work-only-fact") {
		t.Errorf("list shows a work fact through a personal symlink:\n%s", list.stdout)
	}
	wantContains(t, "list stderr", list.stderr, "skipped personal/_global/x.md: ")

	show := sb.run("", "show", "work-only-fact", "--cwd", repo)
	wantExit(t, show, 1)
	if strings.Contains(show.stdout, "work-only-fact") {
		t.Errorf("show printed a work fact through a personal symlink:\n%s", show.stdout)
	}

	written := sb.run("", "index", "--write")
	wantExit(t, written, 1)
	wantContains(t, "index --write stderr", written.stderr, "skipped _global/x.md: ")
	index, err := os.ReadFile(filepath.Join(sb.personal, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(index), "work-only-fact") {
		t.Errorf("personal MEMORY.md lists a work fact:\n%s", index)
	}
}

func TestShowAppliesInsideStoreFilter(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.putFact(sb.personal, "elsewhere/other-repo-fact.md", newFact("other-repo-fact", "elsewhere", "project"))

	res := sb.run("", "show", "other-repo-fact", "--cwd", repo)
	wantExit(t, res, 1)
	if res.stdout != "" {
		t.Errorf("show printed another repo's fact:\n%s", res.stdout)
	}
}

func TestReadCommandsMatchRulesOnRawBytes(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	token := "gh" + "p_" + strings.Repeat("a1", 18)
	leaky := newFact("leaky-fact", "demo", "project")
	leaky.Metadata.OriginSessionID = token
	sb.putFact(sb.personal, "demo/leaky-fact.md", leaky)
	sb.putFact(sb.personal, "demo/clean-fact.md", newFact("clean-fact", "demo", "project"))

	show := sb.run("", "show", "leaky-fact", "--cwd", repo)
	wantExit(t, show, 1)
	if show.stdout != "" {
		t.Errorf("show printed a fact holding a token:\n%s", show.stdout)
	}
	wantContains(t, "show stderr", show.stderr, "excluded personal/demo/leaky-fact.md: rule github-token")

	list := sb.run("", "list", "--cwd", repo)
	wantExit(t, list, 0)
	wantContains(t, "list", list.stdout, "clean-fact")
	if strings.Contains(list.stdout, "leaky-fact") {
		t.Errorf("list shows a fact holding a token:\n%s", list.stdout)
	}
	wantContains(t, "list stderr", list.stderr, "excluded personal/demo/leaky-fact.md: rule github-token")

	written := sb.run("", "index", "--write")
	wantExit(t, written, 0)
	wantContains(t, "index --write stderr", written.stderr, "excluded demo/leaky-fact.md: rule github-token")

	for _, res := range []result{show, list, written} {
		if strings.Contains(res.stdout+res.stderr, token) {
			t.Errorf("output leaks the token:\n%s\n%s", res.stdout, res.stderr)
		}
	}
}

func TestListReportsProblems(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.writeFile(filepath.Join(sb.personal, "demo", "broken.md"), "no frontmatter\n")

	res := sb.run("", "list", "--cwd", repo)
	wantExit(t, res, 1)
	wantContains(t, "list stderr", res.stderr, "skipped personal/demo/broken.md: ")

	sb.personal = filepath.Join(sb.dir, "absent")
	sb.writeConfig()
	res = sb.run("", "list", "--cwd", repo)
	wantExit(t, res, 0)
	wantContains(t, "list stderr", res.stderr, sb.personal+" does not exist")

	sb.writeConfig(`rules = "/nonexistent/rules.toml"`)
	wantExit(t, sb.run("", "list", "--cwd", repo), 1)
}

// TestReadSkipsExitNonZero pins the read commands' skip→exit-code contract: a
// skipped fact makes list non-zero (partial or total) while an empty store
// stays zero, show stays non-zero when the one fact it needs is skipped, a
// redaction exclusion is report-only, and search stays fail-open.
func TestReadSkipsExitNonZero(t *testing.T) {
	broken := func(t *testing.T, sb *sandbox) {
		t.Helper()
		sb.writeFile(filepath.Join(sb.personal, "demo", "broken.md"), "no frontmatter\n")
	}
	good := func(t *testing.T, sb *sandbox) {
		t.Helper()
		sb.putFact(sb.personal, "demo/good-fact.md", newFact("good-fact", "demo", "project"))
	}
	leaky := func(t *testing.T, sb *sandbox) {
		t.Helper()
		f := newFact("leaky-fact", "demo", "project")
		f.Metadata.OriginSessionID = "gh" + "p_" + strings.Repeat("a1", 18)
		sb.putFact(sb.personal, "demo/leaky-fact.md", f)
	}
	linked := func(t *testing.T, sb *sandbox) {
		t.Helper()
		sb.mkdir(filepath.Join(sb.personal, "_global"))
		if err := os.Symlink(filepath.Join(sb.dir, "nowhere"), filepath.Join(sb.personal, "_global", "linked.md")); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name     string
		setup    func(*testing.T, *sandbox)
		args     func(repo string) []string
		want     int
		stdout   []string
		emptyOut bool
		stderr   []string
	}{
		{
			name: "list all skipped", setup: broken,
			args: func(repo string) []string { return []string{"list", "--cwd", repo} },
			want: 1, emptyOut: true, stderr: []string{"skipped personal/demo/broken.md: "},
		},
		{
			name: "list partial skip", setup: func(t *testing.T, sb *sandbox) { good(t, sb); broken(t, sb) },
			args: func(repo string) []string { return []string{"list", "--cwd", repo} },
			want: 1, stdout: []string{"good-fact"}, stderr: []string{"skipped personal/demo/broken.md: "},
		},
		{
			name: "list empty store", setup: nil,
			args: func(repo string) []string { return []string{"list", "--cwd", repo} },
			want: 0, emptyOut: true,
		},
		{
			name: "show all skipped", setup: linked,
			args: func(repo string) []string { return []string{"show", "linked-fact", "--cwd", repo} },
			want: 1, emptyOut: true, stderr: []string{"skipped personal/_global/linked.md: ", "no fact named"},
		},
		{
			name: "list all excluded is report-only", setup: leaky,
			args: func(repo string) []string { return []string{"list", "--cwd", repo} },
			want: 0, emptyOut: true, stderr: []string{"excluded personal/demo/leaky-fact.md: rule "},
		},
		{
			name: "search stays fail-open", setup: func(t *testing.T, sb *sandbox) {
				f := newFact("needle-fact", "demo", "project")
				f.Body = "the zebra crossing rule\n"
				sb.putFact(sb.personal, "demo/needle-fact.md", f)
				sb.writeFile(filepath.Join(sb.personal, "demo", "broken.md"), "zebra but no frontmatter\n")
			},
			args: func(repo string) []string { return []string{"search", "zebra", "--cwd", repo} },
			want: 0, stdout: []string{"needle-fact"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t, "personal")
			repo := sb.repo(personalRemote)
			if tc.setup != nil {
				tc.setup(t, sb)
			}
			res := sb.run("", tc.args(repo)...)
			wantExit(t, res, tc.want)
			if tc.emptyOut && res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
			wantContains(t, "stdout", res.stdout, tc.stdout...)
			wantContains(t, "stderr", res.stderr, tc.stderr...)
		})
	}
}

func TestIndexWriteFailsOnBrokenFact(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.putFact(sb.personal, "demo/good-fact.md", newFact("good-fact", "demo", "project"))
	sb.writeFile(filepath.Join(sb.personal, "demo", "broken.md"), "no frontmatter\n")

	res := sb.run("", "index", "--write")
	wantExit(t, res, 1)
	wantContains(t, "index --write stderr", res.stderr, "skipped demo/broken.md: ")
	index, err := os.ReadFile(filepath.Join(sb.personal, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "MEMORY.md", string(index), "[good-fact](demo/good-fact.md)")
}

// holdLock takes the flock a priors writer takes and returns its release.
func holdLock(t *testing.T, path string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { _ = f.Close() }
}

// wantBlockedByLock runs priors while the named lock is held and checks it
// only finishes once the lock is released.
func wantBlockedByLock(t *testing.T, sb *sandbox, lock string, args ...string) result {
	t.Helper()
	release := holdLock(t, filepath.Join(sb.state, "locks", lock+".lock"))
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := sb.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("priors %v finished while the %s lock was held: %v\n%s", args, lock, err, stderr.String())
	case <-time.After(500 * time.Millisecond):
	}
	release()
	res := result{}
	if err := <-done; err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		res.code = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func TestIndexWriteTakesStoreLock(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.putFact(sb.personal, "demo/a-fact.md", newFact("a-fact", "demo", "project"))

	res := wantBlockedByLock(t, sb, "personal", "index", "--write")
	wantExit(t, res, 0)
	wantContains(t, "index --write", res.stdout, "wrote "+filepath.Join(sb.personal, "MEMORY.md"))
}

func TestIndexWriteTakesQuarantineLock(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.putFact(filepath.Join(sb.state, "quarantine"), "app/q-fact.md", newFact("q-fact", "app", "project"))
	sb.indexWrite()
	sb.putFact(filepath.Join(sb.state, "quarantine"), "app/q2-fact.md", newFact("q2-fact", "app", "project"))

	res := wantBlockedByLock(t, sb, "quarantine", "index", "--write")
	wantExit(t, res, 0)
	wantContains(t, "index --write", res.stdout, "wrote "+filepath.Join(sb.state, "quarantine", "MEMORY.md"))
}

func TestLintMoveFlaggedTakesStoreLock(t *testing.T) {
	sb := newSandbox(t, "personal")
	flagged := newFact("linky-fact", "demo", "project")
	flagged.Body = "see https://example.com/docs for details\n"
	sb.putFact(sb.personal, "demo/linky-fact.md", flagged)
	sb.indexWrite()

	res := wantBlockedByLock(t, sb, "personal", "lint", "--move-flagged")
	wantExit(t, res, 0)
	wantContains(t, "lint --move-flagged", res.stdout, "moved demo/linky-fact.md")
}

func TestIndexWithCwdIgnoresOpenStdin(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.putFact(sb.personal, "demo/indexed-fact.md", newFact("indexed-fact", "demo", "project"))
	sb.indexWrite()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := sb.command(ctx, "index", "--cwd", repo)
	cmd.Stdin = r
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "index", additionalContext(t, string(out)), "[indexed-fact](demo/indexed-fact.md)")
}

func TestAddFlagsShellIngestion(t *testing.T) {
	for name, c := range map[string]struct{ command, prefix, reason string }{
		"gh issue view 12": {"gh issue view 12", "flagged personal ", "provenance:shell"},
		"go test":          {"go test ./...", "published personal ", ""},
	} {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			sb := newSandbox(t, "personal")
			repo := sb.repo(personalRemote)
			sb.record("sess-1")
			sb.toolCall("post_tool", "sess-1", "Bash", fmt.Sprintf(`{"command":%q}`, c.command))

			res := sb.run("", "add", "--name", "shell-fact", "--description", "from a shell session",
				"--type", "project", "--cwd", repo, "--session", "sess-1")
			wantExit(t, res, 0)
			if !strings.HasPrefix(res.stdout, c.prefix) {
				t.Fatalf("stdout %q, want prefix %q", res.stdout, c.prefix)
			}
			if c.reason != "" {
				wantContains(t, "stdout", res.stdout, c.reason)
			}
		})
	}
}

// markerFiles lists the names in the sandbox's marker dir, failing if it is
// not a private directory holding only private files.
func (sb *sandbox) markerFiles(dir string) []string {
	sb.t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		sb.t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		sb.t.Fatalf("%s: mode %v, want a private directory", dir, info.Mode())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		sb.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			sb.t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			sb.t.Errorf("%s: mode %v has group or other bits", e.Name(), fi.Mode())
		}
		names = append(names, e.Name())
	}
	return names
}

func wantMarkerExts(t *testing.T, names []string, exts ...string) {
	t.Helper()
	if len(names) != len(exts) {
		t.Fatalf("marker files %v, want extensions %v", names, exts)
	}
	for _, ext := range exts {
		found := false
		for _, n := range names {
			found = found || filepath.Ext(n) == ext
		}
		if !found {
			t.Errorf("marker files %v lack %s", names, ext)
		}
	}
}

func TestPostToolMarkerFiles(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.toolCall("post_tool", "sess-1", "Bash", `{"command":"gh issue view 12"}`)

	wantMarkerExts(t, sb.markerFiles(filepath.Join(sb.state, "provenance")), ".seen", ".ingest")
}

// TestPreToolMarksIngestion: pre_tool marks the session before the command
// runs, so a call that fails, and never reaches post_tool, still flags it.
func TestPreToolMarksIngestion(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.toolCall("pre_tool", "sess-1", "Bash", `{"command":"gh issue view 12"}`)

	wantMarkerExts(t, sb.markerFiles(filepath.Join(sb.state, "provenance")), ".seen", ".ingest")
	sb.record("sess-1")
	res := sb.run("", "add", "--name", "pre-tool-fact", "--description", "from a failed shell call",
		"--type", "project", "--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)
	if !strings.HasPrefix(res.stdout, "flagged personal ") {
		t.Fatalf("stdout %q", res.stdout)
	}
	wantContains(t, "stdout", res.stdout, "provenance:shell")
}

func TestPostToolMarkerHonoursXDGStateHome(t *testing.T) {
	sb := newSandbox(t, "personal")
	sb.configPath = filepath.Join(sb.dir, "config-no-state.toml")
	sb.writeFile(sb.configPath, strings.Join([]string{
		fmt.Sprintf("profile = %q", sb.profile),
		fmt.Sprintf("personal_store = %q", sb.personal),
		fmt.Sprintf("work_store = %q", sb.work),
		`work_orgs = ["github.com/factify-inc"]`,
		`personal_orgs = ["github.com/noamsto"]`,
		fmt.Sprintf("ssh_config = %q", sb.sshConfig),
		fmt.Sprintf("scanner = %q", sb.scanner),
	}, "\n")+"\n")
	sb.toolCall("post_tool", "sess-1", "Bash", `{"command":"go test ./..."}`)

	wantMarkerExts(t, sb.markerFiles(filepath.Join(sb.dir, "xdg-state", "priors", "provenance")), ".seen")
}

// TestPostToolLargeResponse: the size cap cuts native, after tool_input, so
// the call is still judged by its command.
func TestPostToolLargeResponse(t *testing.T) {
	sb := newSandbox(t, "personal")
	big := strconv.Quote(strings.Repeat("x", 2<<20))
	sb.send(sb.toolEnvelope("post_tool", "sess-1", "Bash", `{"command":"go test ./..."}`, big))

	wantMarkerExts(t, sb.markerFiles(filepath.Join(sb.state, "provenance")), ".seen")
}

// TestPostToolTruncatedToolInput: when the size cap cuts inside tool_input the
// command is unreadable, and an unreadable shell command counts as ingestion.
func TestPostToolTruncatedToolInput(t *testing.T) {
	sb := newSandbox(t, "personal")
	big := strconv.Quote(strings.Repeat("x", 2<<20))
	sb.send(sb.toolEnvelope("post_tool", "sess-1", "Bash", `{"command":`+big+`}`, `"ok"`))

	wantMarkerExts(t, sb.markerFiles(filepath.Join(sb.state, "provenance")), ".seen", ".ingest")
}

// TestPostToolTruncatedNonShellInput: the same cut on a non-shell tool loses
// input that was never a command, so it is not ingestion.
func TestPostToolTruncatedNonShellInput(t *testing.T) {
	sb := newSandbox(t, "personal")
	big := strconv.Quote(strings.Repeat("x", 2<<20))
	sb.send(sb.toolEnvelope("post_tool", "sess-1", "Write", `{"content":`+big+`}`, `"ok"`))

	wantMarkerExts(t, sb.markerFiles(filepath.Join(sb.state, "provenance")), ".seen")
}

func TestAddUnreadableMarkerDir(t *testing.T) {
	sb := newSandbox(t, "personal")
	repo := sb.repo(personalRemote)
	sb.record("sess-1")
	markers := filepath.Join(sb.state, "provenance")
	if err := os.RemoveAll(markers); err != nil {
		t.Fatal(err)
	}
	sb.writeFile(markers, "not a directory")

	res := sb.run("", "add", "--name", "unwatched-fact", "--description", "markers unreadable",
		"--type", "project", "--cwd", repo, "--session", "sess-1")
	wantExit(t, res, 0)
	if !strings.HasPrefix(res.stdout, "flagged personal ") {
		t.Fatalf("stdout %q", res.stdout)
	}
	wantContains(t, "stdout", res.stdout, "provenance:no-ingest-record")
}
