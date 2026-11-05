package verify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/gate"
)

const walkedRef = "refs/priors/walked/"

type walkResult struct {
	N        int
	Complete bool     // rev-list stream ended: nothing reachable from a tip is unwalked
	Frontier []string // walked commits with no walked child: new walked refs
	Covered  []string // existing walked-ref targets that are parents of walked commits: refs to delete
}

// afterCommit is a test hook called after each walked commit (n = commits
// walked this run); nil in production.
var afterCommit func(soft context.Context, n int)

// walkEntry is a blob that parses as an entry for this store; its name
// decides where it is well-formed.
type walkEntry struct {
	e        attest.Entry
	digest   string
	observed bool
}

type walker struct {
	m       mirror
	soft    context.Context
	cf      *catFile
	r       *Record
	storeID string
	walked  map[string]bool // walked-ref targets at the start of the run

	entries  map[string]*walkEntry        // blob → parsed entry, nil when ignored
	sums     map[string]string            // blob → sha256 hex
	attests  map[string]map[string]string // commit → .attest name → blob
	live     map[string]map[string]bool   // commit walked this run → live set
	boundary map[string]map[string]bool   // commit walked earlier → live set
	frontier map[string]bool
	covered  map[string]bool
}

// walk walks the unwalked commits reachable from the tip refs, parents first,
// applying checks 7 and 8 to r (memory-layer §4.4). It stops after limit commits or
// once soft is done; the commits walked by then are fully applied, so the
// result can be checkpointed.
func (m mirror) walk(soft context.Context, cf *catFile, r *Record, storeID string, limit int) (walkResult, error) {
	walked, err := m.walkedRefs(soft)
	if err != nil {
		return walkResult{}, err
	}
	w := &walker{
		m: m, soft: soft, cf: cf, r: r, storeID: storeID, walked: walked,
		entries:  map[string]*walkEntry{},
		sums:     map[string]string{},
		attests:  map[string]map[string]string{},
		live:     map[string]map[string]bool{},
		boundary: map[string]map[string]bool{},
		frontier: map[string]bool{},
		covered:  map[string]bool{},
	}

	ctx, cancel := context.WithCancel(soft)
	defer cancel()
	cmd := m.cmd(ctx, "rev-list", "--topo-order", "--reverse", "--parents",
		"--glob="+tipsRef+"*", "--not", "--glob="+walkedRef+"*")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return walkResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return walkResult{}, err
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}

	sc := bufio.NewScanner(stdout)
	// One line holds a commit and its parents, so a commit within maxCommit fits.
	sc.Buffer(nil, maxCommit)
	n := 0
	for {
		if soft.Err() != nil {
			stop()
			return w.result(n, false), nil
		}
		if !sc.Scan() {
			break
		}
		if n >= limit {
			stop()
			return w.result(n, false), nil
		}
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			stop()
			return walkResult{}, errors.New("git rev-list: empty line")
		}
		if err := w.commit(f[0], f[1:]); err != nil {
			stop()
			// A read cut off by the deadline loses only the commit in hand.
			if soft.Err() != nil {
				return w.result(n, false), nil
			}
			return walkResult{}, err
		}
		n++
		if afterCommit != nil {
			afterCommit(soft, n)
		}
	}
	if err := sc.Err(); err != nil {
		stop()
		if soft.Err() != nil {
			return w.result(n, false), nil
		}
		return walkResult{}, fmt.Errorf("git rev-list: %w", err)
	}
	werr := cmd.Wait()
	if soft.Err() != nil {
		return w.result(n, false), nil
	}
	if werr != nil {
		return walkResult{}, fmt.Errorf("git rev-list: %w: %s", werr, short(stderr.String()))
	}
	return w.result(n, true), nil
}

func (w *walker) result(n int, complete bool) walkResult {
	return walkResult{
		N:        n,
		Complete: complete,
		Frontier: slices.Sorted(maps.Keys(w.frontier)),
		Covered:  slices.Sorted(maps.Keys(w.covered)),
	}
}

// commit applies one commit whose parents are all walked, this run or earlier.
func (w *walker) commit(oid string, parents []string) error {
	w.cf.resetTrees()
	tree, _, err := w.cf.commit(oid)
	if err != nil {
		return err
	}
	blobs, err := w.cf.attestEntries(tree)
	if err != nil {
		return err
	}
	w.attests[oid] = blobs

	live := map[string]bool{}
	for name, blob := range blobs {
		we, err := w.entry(name, blob)
		if err != nil {
			return err
		}
		if we == nil {
			continue
		}
		t, ok := w.r.Entries[we.digest]
		if !ok {
			continue
		}
		live[we.digest] = true
		intro, err := w.intro(parents, name, blob)
		if err != nil {
			return err
		}
		if intro && !slices.Contains(t.Intros, oid) {
			t.Intros = append(t.Intros, oid)
		}
	}
	for _, p := range parents {
		pl, err := w.liveOf(p)
		if err != nil {
			return err
		}
		maps.Copy(live, pl)
	}
	for d := range live {
		if w.r.Lapsed[d] {
			delete(live, d)
			continue
		}
		t, found := w.r.Entries[d]
		if !found || t == nil {
			continue
		}
		ok, err := w.holds(tree, t)
		if err != nil {
			return err
		}
		if !ok {
			w.r.Lapsed[d] = true
			delete(live, d)
		}
	}
	// Live sets are never mutated once stored, so a commit that changes
	// nothing shares its parent's set instead of holding a copy.
	if len(parents) > 0 {
		if pl, _ := w.liveOf(parents[0]); maps.Equal(live, pl) {
			live = pl
		}
	}
	w.live[oid] = live

	w.frontier[oid] = true
	for _, p := range parents {
		delete(w.frontier, p)
		if _, ok := w.live[p]; !ok && w.walked[p] {
			w.covered[p] = true
		}
	}
	return nil
}

// entry returns the entry at .attest/name when it is well-formed there (it
// parses, names this store and name), else nil. Its first well-formed
// occurrence this run applies check 7 and tracks a signed attest entry.
func (w *walker) entry(name, blob string) (*walkEntry, error) {
	we, seen := w.entries[blob]
	if !seen {
		var err error
		if we, err = w.parse(blob); err != nil {
			return nil, err
		}
		w.entries[blob] = we
	}
	if we == nil || we.e.Name != name {
		return nil, nil
	}
	if we.observed {
		return we, nil
	}
	var signer []byte
	if pub, err := attest.Signer(we.e, w.storeID, name); err == nil {
		signer = pub.Marshal()
	}
	w.r.Observe(name, we.digest, we.e, signer)
	if we.e.Op == attest.OpAttest && signer != nil {
		if _, ok := w.r.Entries[we.digest]; !ok {
			w.r.Entries[we.digest] = &Tracked{Name: name, Path: we.e.Path, SHA256: we.e.SHA256}
		}
	}
	we.observed = true
	return we, nil
}

// parse reads blob as an entry; one over the size cap, malformed or for
// another store is nil.
func (w *walker) parse(blob string) (*walkEntry, error) {
	raw, err := w.cf.blob(blob, attest.MaxEntryBytes)
	switch {
	case errors.Is(err, errTooBig):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if e, perr := attest.ParseEntry(raw); perr == nil && e.Store == w.storeID {
		sum := sha256.Sum256(raw)
		return &walkEntry{e: e, digest: hex.EncodeToString(sum[:])}, nil
	}
	return nil, nil
}

// intro reports whether no parent holds blob at .attest/name.
func (w *walker) intro(parents []string, name, blob string) (bool, error) {
	for _, p := range parents {
		blobs, err := w.attestOf(p)
		if err != nil {
			return false, err
		}
		if blobs[name] == blob {
			return false, nil
		}
	}
	return true, nil
}

func (w *walker) attestOf(oid string) (map[string]string, error) {
	if blobs, ok := w.attests[oid]; ok {
		return blobs, nil
	}
	tree, _, err := w.cf.commit(oid)
	if err != nil {
		return nil, err
	}
	blobs, err := w.cf.attestEntries(tree)
	if err != nil {
		return nil, err
	}
	w.attests[oid] = blobs
	return blobs, nil
}

func (w *walker) liveOf(p string) (map[string]bool, error) {
	if l, ok := w.live[p]; ok {
		return l, nil
	}
	return w.boundaryLive(p)
}

// boundaryLive rebuilds the live set of p, walked in an earlier run: every
// tracked entry not lapsed with an introducing commit that is an ancestor of
// p or p itself.
func (w *walker) boundaryLive(p string) (map[string]bool, error) {
	if l, ok := w.boundary[p]; ok {
		return l, nil
	}
	var cands []string
	for d, t := range w.r.Entries {
		if !w.r.Lapsed[d] && len(t.Intros) > 0 {
			cands = append(cands, d)
		}
	}
	live := map[string]bool{}
	if len(cands) > 0 {
		out, err := w.m.run(w.soft, "rev-list", p)
		if err != nil {
			return nil, err
		}
		ancestors := map[string]bool{}
		for c := range strings.FieldsSeq(out) {
			ancestors[c] = true
		}
		for _, d := range cands {
			t, found := w.r.Entries[d]
			if !found || t == nil {
				continue
			}
			if slices.ContainsFunc(t.Intros, func(c string) bool { return ancestors[c] }) {
				live[d] = true
			}
		}
	}
	w.boundary[p] = live
	return live, nil
}

// holds is check 8 at one commit: the file at t.Path is a blob with t.SHA256.
// A blob over gate 4's cap could never have been attested as a fact, so it
// does not hold and is not read.
func (w *walker) holds(tree string, t *Tracked) (bool, error) {
	oid, ok, err := w.cf.lookupPath(tree, t.Path)
	if err != nil || !ok {
		return false, err
	}
	sum, ok := w.sums[oid]
	if !ok {
		b, err := w.cf.blob(oid, gate.MaxFactBytes)
		switch {
		case errors.Is(err, errTooBig):
			return false, nil
		case err != nil:
			return false, err
		}
		s := sha256.Sum256(b)
		sum = hex.EncodeToString(s[:])
		w.sums[oid] = sum
	}
	return sum == t.SHA256, nil
}

// walkedRefs returns the <oid> of every refs/priors/walked/<oid>; the job
// only ever points such a ref at its own name.
func (m mirror) walkedRefs(ctx context.Context) (map[string]bool, error) {
	out, err := m.run(ctx, "for-each-ref", "--format=%(refname)", walkedRef)
	if err != nil {
		return nil, err
	}
	walked := map[string]bool{}
	for ref := range strings.FieldsSeq(out) {
		walked[strings.TrimPrefix(ref, walkedRef)] = true
	}
	return walked, nil
}

// checkpoint marks the walked prefix in one transaction: a walked ref per
// frontier commit, and the walked refs it covers dropped. The record must be
// saved first, so a crash in between only re-walks commits.
func (m mirror) checkpoint(hard context.Context, w walkResult) error {
	if len(w.Frontier) == 0 && len(w.Covered) == 0 {
		return nil
	}
	walked, err := m.walkedRefs(hard)
	if err != nil {
		return err
	}
	var tx []string
	for _, oid := range w.Frontier {
		if !walked[oid] {
			tx = append(tx, fmt.Sprintf("create %s%s %s", walkedRef, oid, oid))
		}
	}
	for _, oid := range w.Covered {
		tx = append(tx, "delete "+walkedRef+oid)
	}
	return m.updateRefs(hard, tx)
}

// dropWalked deletes every walked ref, so the next walk starts from scratch.
func (m mirror) dropWalked(ctx context.Context) error {
	walked, err := m.walkedRefs(ctx)
	if err != nil {
		return err
	}
	var tx []string
	for _, oid := range slices.Sorted(maps.Keys(walked)) {
		tx = append(tx, "delete "+walkedRef+oid)
	}
	return m.updateRefs(ctx, tx)
}

// updateRefs applies the update-ref --stdin commands as one transaction.
func (m mirror) updateRefs(ctx context.Context, commands []string) error {
	if len(commands) == 0 {
		return nil
	}
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, c := range commands {
		tx.WriteString(c + "\n")
	}
	tx.WriteString("commit\n")
	cmd := m.cmd(ctx, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(tx.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, short(stderr.String()))
	}
	return nil
}
