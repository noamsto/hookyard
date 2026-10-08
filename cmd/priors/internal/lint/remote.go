package lint

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/fact"
	"github.com/noamsto/hookyard/cmd/priors/internal/route"
	"github.com/noamsto/hookyard/cmd/priors/internal/store"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

const (
	attestDir = ".attest"
	// maxHistoryFactBytes bounds a fact blob read from history, which no
	// client gate has vetted.
	maxHistoryFactBytes = 1 << 20
	remoteGitTimeout    = 2 * time.Minute
	shortRev            = 12
)

// Remote is the structural half of a store's required check, which has no
// trust file and no verdicts: every attest entry parses as v1 under its own
// name, and every fact claiming confidence: reviewed has an entry. With since
// it also checks the pushed range: no entry file deleted, and no commit
// leaves a fact claiming reviewed without an entry. It never verifies a
// signature, so it never needs a key.
func Remote(ctx context.Context, root store.Root, since string) []Finding {
	out := remoteTree(root)
	if since == "" {
		return out
	}
	return append(out, remoteRange(ctx, root.Path, since)...)
}

func remoteTree(root store.Root) []Finding {
	var out []Finding
	dir := filepath.Join(root.Path, attestDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		out = append(out, Finding{File: attestDir, Rule: "attest-entry", Msg: "cannot be read as a directory of entries"})
	}
	for _, d := range entries {
		rel := attestDir + "/" + d.Name()
		if !d.Type().IsRegular() {
			out = append(out, Finding{File: rel, Rule: "attest-entry", Msg: "must be a regular file directly in " + attestDir})
			continue
		}
		raw, err := readEntryFile(filepath.Join(dir, d.Name()))
		if err != nil {
			out = append(out, Finding{File: rel, Rule: "attest-entry", Msg: "cannot be read: " + err.Error()})
			continue
		}
		e, err := attest.ParseEntry(raw)
		if err != nil {
			out = append(out, Finding{File: rel, Rule: "attest-entry", Msg: "is not a v1 attest entry: " + err.Error()})
			continue
		}
		if e.Name != d.Name() {
			out = append(out, Finding{File: rel, Rule: "attest-entry", Msg: "file name must equal the entry's name"})
		}
	}

	facts, _ := root.Walk()
	for _, e := range facts {
		if e.Fact.Metadata.Confidence != "reviewed" {
			continue
		}
		if !fact.NameRE.MatchString(e.Fact.Name) {
			out = append(out, Finding{File: e.Rel, Rule: "unattested-review", Msg: "claims confidence: reviewed but its name cannot have an attest entry"})
			continue
		}
		if fi, err := os.Lstat(filepath.Join(dir, e.Fact.Name)); err != nil || !fi.Mode().IsRegular() {
			out = append(out, Finding{File: e.Rel, Rule: "unattested-review", Msg: "claims confidence: reviewed without " + attestDir + "/" + e.Fact.Name})
		}
	}
	return out
}

// readEntryFile reads at most one byte past the entry cap, so ParseEntry
// reports an oversized entry without the file being buffered whole.
func readEntryFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is a regular file listed under the store's .attest
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, attest.MaxEntryBytes+1))
}

func remoteRange(ctx context.Context, dir, since string) []Finding {
	badRange := Finding{File: ".", Rule: "remote-range", Msg: "--since " + since + " is not a commit here; fetch full history"}
	if strings.HasPrefix(since, "-") {
		return []Finding{badRange}
	}
	base, err := remoteGit(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", since+"^{commit}")
	if err != nil {
		return []Finding{badRange}
	}
	base = strings.TrimSpace(base)

	out, err := deletedEntries(ctx, dir, base)
	if err != nil {
		return []Finding{gitFailure("log", err)}
	}

	commits, err := remoteGit(ctx, dir, nil, "rev-list", "--reverse", base+"..HEAD")
	if err != nil {
		return append(out, gitFailure("rev-list", err))
	}
	seen := map[string]claim{}
	for c := range strings.FieldsSeq(commits) {
		found, err := checkCommit(ctx, dir, c, seen)
		out = append(out, found...)
		if err != nil {
			return append(out, gitFailure("commit check", err))
		}
	}
	return out
}

// deletedEntries finds an entry deleted or replaced by a symlink in any
// commit in base..HEAD, so one added and dropped inside the range still
// counts. -m diffs a merge against each parent, so a merge that takes the
// side without an entry counts too; --no-renames makes a rename a delete;
// T counts an entry turned into a symlink, which no reader honours.
func deletedEntries(ctx context.Context, dir, base string) ([]Finding, error) {
	log, err := remoteGit(ctx, dir, nil, "log", "--reverse", "--format=%x01%H", "-z", "--name-only", "-m", "--no-renames", "--relative", "--diff-filter=DT", base+"..HEAD", "--", attestDir+"/")
	if err != nil {
		return nil, err
	}
	var out []Finding
	seen := map[string]bool{}
	var commit string
	for tok := range strings.SplitSeq(log, "\x00") {
		tok = strings.TrimLeft(tok, "\n")
		if c, ok := strings.CutPrefix(tok, "\x01"); ok {
			commit = c
			continue
		}
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, Finding{File: tok, Rule: "attest-deleted", Msg: "entry was deleted or replaced by commit " + short(commit)})
	}
	return out, nil
}

// claim is what a fact blob says about its confidence, kept by blob id so a
// blob unchanged across commits is read once.
type claim struct {
	reviewed bool
	name     string
	oversize bool
}

func checkCommit(ctx context.Context, dir, commit string, seen map[string]claim) ([]Finding, error) {
	listing, err := remoteGit(ctx, dir, nil, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	entries := map[string]bool{}
	var facts []struct{ path, oid string }
	var fresh []string
	for rec := range strings.SplitSeq(listing, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || fields[0] == "120000" {
			continue
		}
		oid := fields[2]
		if name, ok := strings.CutPrefix(p, attestDir+"/"); ok && !strings.Contains(name, "/") {
			entries[name] = true
			continue
		}
		if !isFactPath(p) {
			continue
		}
		facts = append(facts, struct{ path, oid string }{p, oid})
		if _, ok := seen[oid]; !ok && !slices.Contains(fresh, oid) {
			fresh = append(fresh, oid)
		}
	}

	if len(fresh) > 0 {
		err := catBlobs(ctx, dir, fresh, func(oid string, data []byte, oversize bool) {
			c := claim{oversize: oversize}
			if f, err := fact.Parse(data); !oversize && err == nil && f.Metadata.Confidence == "reviewed" {
				c.reviewed, c.name = true, f.Name
			}
			seen[oid] = c
		})
		if err != nil {
			return nil, err
		}
	}

	var out []Finding
	for _, f := range facts {
		c := seen[f.oid]
		switch {
		case c.oversize:
			out = append(out, Finding{File: f.path, Rule: "remote-blob", Msg: fmt.Sprintf("in commit %s is over %d bytes and was not checked", short(commit), maxHistoryFactBytes)})
		case !c.reviewed:
		case !fact.NameRE.MatchString(c.name) || !entries[c.name]:
			out = append(out, Finding{File: f.path, Rule: "unattested-review", Msg: fmt.Sprintf("commit %s leaves it claiming confidence: reviewed without %s/%s", short(commit), attestDir, c.name)})
		}
	}
	return out, nil
}

// isFactPath mirrors store.Root.Walk: a .md file below the root, outside any
// dot directory.
func isFactPath(p string) bool {
	dirs, ok := strings.CutSuffix(p, ".md")
	if !ok {
		return false
	}
	parts := strings.Split(dirs, "/")
	return len(parts) > 1 && !slices.ContainsFunc(parts[:len(parts)-1], func(s string) bool { return strings.HasPrefix(s, ".") })
}

// catBlobs reads every oid through one `git cat-file --batch`.
func catBlobs(ctx context.Context, dir string, oids []string, visit func(oid string, data []byte, oversize bool)) error {
	ctx, cancel := context.WithTimeout(ctx, remoteGitTimeout)
	defer cancel()
	cmd := remoteCommand(ctx, dir, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	readErr := readBatch(bufio.NewReader(stdout), oids, visit)
	if readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	return waitErr
}

func readBatch(r *bufio.Reader, oids []string, visit func(oid string, data []byte, oversize bool)) error {
	for _, oid := range oids {
		header, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("cat-file header for %s: %w", oid, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != oid || fields[1] != "blob" {
			return fmt.Errorf("cat-file could not read blob %s", oid)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return fmt.Errorf("cat-file size for %s: %w", oid, err)
		}
		if size > maxHistoryFactBytes {
			visit(oid, nil, true)
			// +1 for the newline cat-file prints after every blob
			if _, err := io.CopyN(io.Discard, r, size+1); err != nil {
				return err
			}
			continue
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}
		visit(oid, data[:size], false)
	}
	return nil
}

func remoteGit(ctx context.Context, dir string, stdin io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteGitTimeout)
	defer cancel()
	cmd := remoteCommand(ctx, dir, args...)
	cmd.Stdin = stdin
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.String(), err
}

// remoteCommand strips the repo-locating variables a hook exports, so `-C
// dir` is the repository that is read.
func remoteCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := tools.Command(ctx, tools.Git, append([]string{"-C", dir}, args...)...)
	cmd.Env = route.RepoEnv("GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return cmd
}

func gitFailure(what string, err error) Finding {
	return Finding{File: ".", Rule: "remote-git", Msg: fmt.Sprintf("git %s failed: %v", what, err)}
}

func short(rev string) string {
	if len(rev) > shortRev {
		return rev[:shortRev]
	}
	return rev
}
