package verify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

const (
	maxGitOut = 200
	// hashLen is the raw object id length in a tree: the mirror is sha1.
	hashLen = 20
)

var ErrTooBig = errors.New("object over the size cap")

// GitConfigArgs switch off every git feature that reads or writes outside the
// objects the job fetched.
var GitConfigArgs = []string{
	"-c", "core.commitGraph=false",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "fetch.writeCommitGraph=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
}

// mirror is the job's private bare repository. Every git it runs gets an
// environment built from nothing, so no user config, agent or HOME applies.
type mirror struct {
	dir        string // the bare repository, <private>/mirror.git
	private    string // the git working directory
	credential string // ssh private key path
}

func (m mirror) env() []string {
	return []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_DIR=" + m.dir,
		"GIT_ALLOW_PROTOCOL=ssh",
		"GIT_SSH_VARIANT=ssh",
		// VERBOSE logs "Authenticated to" on success, which fetch needs to
		// tell a pre-authentication failure from text the remote pushed.
		"GIT_SSH_COMMAND=" + quote(tools.SSH) + " -F " + quote(tools.SSHConfig) +
			" -o " + quote("UserKnownHostsFile="+tools.KnownHosts) +
			" -o " + quote("IdentitiesOnly=yes") +
			" -o " + quote("IdentityAgent=none") +
			" -o " + quote("BatchMode=yes") +
			" -o " + quote("ConnectTimeout=30") +
			" -o " + quote("LogLevel=VERBOSE") +
			" -i " + quote(m.credential),
	}
}

// quote single-quotes s for the POSIX shell git runs GIT_SSH_COMMAND through.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (m mirror) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := tools.Command(ctx, tools.Git, append(append([]string{}, GitConfigArgs...), args...)...)
	cmd.Env = m.env()
	cmd.Dir = m.private
	cmd.WaitDelay = time.Second
	return cmd
}

// run returns stdout; a failure carries the trimmed stderr.
func (m mirror) run(ctx context.Context, sub string, args ...string) (string, error) {
	cmd := m.cmd(ctx, append([]string{sub}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := short(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", sub, err, msg)
		}
		return "", fmt.Errorf("git %s: %w", sub, err)
	}
	return stdout.String(), nil
}

func short(s string) string {
	msg := strings.Join(strings.Fields(s), " ")
	if len(msg) > maxGitOut {
		msg = strings.ToValidUTF8(msg[:maxGitOut], "") + "…"
	}
	return msg
}

// init creates the bare mirror when it does not exist yet.
func (m mirror) init(ctx context.Context) error {
	if _, err := os.Stat(m.dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := m.run(ctx, "init", "-q", "--bare", "--template=", "--object-format=sha1", m.dir)
	return err
}

type treeEntry struct {
	mode string
	name string
	oid  string // hex
}

// catFile reads objects through one `git cat-file --batch-command` process
// started from the command given to newCatFile.
type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	// trees caches parsed trees until resetTrees.
	trees map[string][]treeEntry
	// attest caches attestEntries by the .attest tree oid.
	attest map[string]map[string]string
}

func newCatFile(cmd *exec.Cmd) (*catFile, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{
		cmd:    cmd,
		in:     in,
		out:    bufio.NewReader(out),
		trees:  map[string][]treeEntry{},
		attest: map[string]map[string]string{},
	}, nil
}

func (c *catFile) close() {
	_ = c.in.Close()
	_ = c.cmd.Wait()
}

// resetTrees drops the parsed trees, so the walk holds only one commit's
// trees at a time however much history it reads.
func (c *catFile) resetTrees() {
	clear(c.trees)
}

// header sends command and parses the "<oid> <type> <size>" reply.
func (c *catFile) header(command, oid string) (typ string, size int64, err error) {
	if _, err := io.WriteString(c.in, command+" "+oid+"\n"); err != nil {
		return "", 0, err
	}
	line, err := c.out.ReadString('\n')
	if err != nil {
		return "", 0, fmt.Errorf("cat-file: %w", err)
	}
	f := strings.Fields(line)
	if len(f) == 2 && f[1] == "missing" {
		return "", 0, fmt.Errorf("cat-file: object %s missing", oid)
	}
	if len(f) != 3 {
		return "", 0, fmt.Errorf("cat-file: unexpected reply %q", short(line))
	}
	size, err = strconv.ParseInt(f[2], 10, 64)
	if err != nil || size < 0 {
		return "", 0, fmt.Errorf("cat-file: bad size in %q", short(line))
	}
	return f[1], size, nil
}

func (c *catFile) info(oid string) (typ string, size int64, err error) {
	return c.header("info", oid)
}

// contents returns the object's bytes, or ErrTooBig without reading them when
// it is larger than max.
func (c *catFile) contents(oid, wantType string, max int64) ([]byte, error) {
	typ, size, err := c.info(oid)
	if err != nil {
		return nil, err
	}
	if typ != wantType {
		return nil, fmt.Errorf("object %s is a %s, not a %s", oid, typ, wantType)
	}
	if size > max {
		return nil, ErrTooBig
	}
	if _, _, err := c.header("contents", oid); err != nil {
		return nil, err
	}
	buf := make([]byte, size+1) // the trailing newline
	if _, err := io.ReadFull(c.out, buf); err != nil {
		return nil, fmt.Errorf("cat-file: %w", err)
	}
	return buf[:size], nil
}

func (c *catFile) blob(oid string, max int64) ([]byte, error) {
	return c.contents(oid, "blob", max)
}

// maxCommit and maxTree bound the objects the parser reads.
const (
	maxCommit = 1 << 20
	maxTree   = 16 << 20
)

// commit returns a commit's tree and parent ids as git reads them: the tree
// from the first header line, the parents from the lines right after it. A
// later tree or parent header passes fsck but means nothing to git.
func (c *catFile) commit(oid string) (tree string, parents []string, err error) {
	data, err := c.contents(oid, "commit", maxCommit)
	if err != nil {
		return "", nil, err
	}
	head, _, _ := bytes.Cut(data, []byte("\n\n"))
	lines := strings.Split(string(head), "\n")
	tree, ok := strings.CutPrefix(lines[0], "tree ")
	if !ok || !hexOID(tree) {
		return "", nil, fmt.Errorf("commit %s: first header is not a tree", oid)
	}
	for _, line := range lines[1:] {
		p, ok := strings.CutPrefix(line, "parent ")
		if !ok {
			break
		}
		if !hexOID(p) {
			return "", nil, fmt.Errorf("commit %s: malformed parent", oid)
		}
		parents = append(parents, p)
	}
	return tree, parents, nil
}

// hexOID reports whether s is a sha1 object id in lowercase hex.
func hexOID(s string) bool {
	if len(s) != 2*hashLen {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// tree parses `<mode> <name>\0<20 raw bytes>` entries; the result is cached.
func (c *catFile) tree(oid string) ([]treeEntry, error) {
	if es, ok := c.trees[oid]; ok {
		return es, nil
	}
	data, err := c.contents(oid, "tree", maxTree)
	if err != nil {
		return nil, err
	}
	var es []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("tree %s: malformed entry", oid)
		}
		nul := bytes.IndexByte(data[sp:], 0)
		if nul < 0 || len(data) < sp+nul+1+hashLen {
			return nil, fmt.Errorf("tree %s: malformed entry", oid)
		}
		nul += sp
		es = append(es, treeEntry{
			mode: string(data[:sp]),
			name: string(data[sp+1 : nul]),
			oid:  fmt.Sprintf("%x", data[nul+1:nul+1+hashLen]),
		})
		data = data[nul+1+hashLen:]
	}
	c.trees[oid] = es
	return es, nil
}

func regular(mode string) bool { return mode == "100644" || mode == "100755" }

// attestEntries maps each regular file directly under .attest to its blob id.
func (c *catFile) attestEntries(rootTree string) (map[string]string, error) {
	root, err := c.tree(rootTree)
	if err != nil {
		return nil, err
	}
	var attest string
	for _, e := range root {
		if e.name == ".attest" && e.mode == "40000" {
			attest = e.oid
			break
		}
	}
	if attest == "" {
		return nil, nil
	}
	if m, ok := c.attest[attest]; ok {
		return m, nil
	}
	es, err := c.tree(attest)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, e := range es {
		if regular(e.mode) {
			m[e.name] = e.oid
		}
	}
	c.attest[attest] = m
	return m, nil
}

// lookupPath returns the blob id at rel, which must be a regular file.
func (c *catFile) lookupPath(rootTree, rel string) (oid string, ok bool, err error) {
	cur := rootTree
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		es, err := c.tree(cur)
		if err != nil {
			return "", false, err
		}
		var found *treeEntry
		for j := range es {
			if es[j].name == part {
				found = &es[j]
				break
			}
		}
		if found == nil {
			return "", false, nil
		}
		if i == len(parts)-1 {
			if !regular(found.mode) {
				return "", false, nil
			}
			return found.oid, true, nil
		}
		if found.mode != "40000" {
			return "", false, nil
		}
		cur = found.oid
	}
	return "", false, nil
}

// Objects reads a repository's objects through one cat-file --batch-command
// process, with replace objects as the command's environment sets them.
type Objects struct{ c *catFile }

// OpenObjects starts cmd, a cat-file --batch-command, and returns the reader
// over it.
func OpenObjects(cmd *exec.Cmd) (*Objects, error) {
	c, err := newCatFile(cmd)
	if err != nil {
		return nil, err
	}
	return &Objects{c: c}, nil
}

// Commit returns the commit's root tree and parent ids.
func (o *Objects) Commit(oid string) (tree string, parents []string, err error) {
	o.c.resetTrees()
	return o.c.commit(oid)
}

// AttestBlob returns the blob id of the regular file .attest/name in tree.
func (o *Objects) AttestBlob(tree, name string) (string, bool, error) {
	m, err := o.c.attestEntries(tree)
	if err != nil {
		return "", false, err
	}
	oid, ok := m[name]
	return oid, ok, nil
}

// PathBlob returns the blob id of the regular file at rel in tree.
func (o *Objects) PathBlob(tree, rel string) (string, bool, error) {
	return o.c.lookupPath(tree, rel)
}

// Blob returns the blob's bytes, or ErrTooBig when it exceeds max.
func (o *Objects) Blob(oid string, max int64) ([]byte, error) {
	return o.c.blob(oid, max)
}

// Close ends the cat-file process.
func (o *Objects) Close() { o.c.close() }
