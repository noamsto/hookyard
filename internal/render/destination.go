package render

import (
	"fmt"
	"os"
	"path/filepath"
)

// CheckDestinations refuses to render into a destination that is a symlink.
//
// Every writer here lands its result through a rename, which replaces a
// symlink rather than writing through it: one install would quietly turn a
// managed link into a regular file and detach whatever placed it. That is not
// hypothetical — home-manager puts ~/.claude/settings.json there as a link
// into the store, and the next switch would abort on the file in its way.
//
// Following the link instead is worse, not better: a store target is
// read-only, so write-through cannot work where it is most needed, and a rule
// whose behaviour depends on where a link happens to point is harder to
// predict than one that refuses every link and names it. Refusing a file this
// package cannot handle rather than clobbering it is the discipline §8
// already sets for the writers themselves.
//
// The caller runs this over every destination before the first write, so a
// refusal on one engine cannot leave the others already rewritten. Pi alone
// contributes two of them: its settings.json and the bridge beside it.
func CheckDestinations(destinations ...Destination) error {
	for _, d := range destinations {
		if err := checkNotSymlink(d.Flag, d.Path); err != nil {
			return err
		}
	}
	return nil
}

// Destination is one path the pre-flight guards, carrying the flag that moves
// it so a refusal can name what to change.
type Destination struct {
	Flag string
	Path string
}

// CheckPiBridgeDir refuses to reach the bridge through a symlinked bin/.
//
// checkNotSymlink guards the final path, but the write reaches it through
// os.MkdirAll, which walks an existing symlinked directory transparently and
// reports success. So a link at bin/ silently redirects the one artifact
// hookyard lands that is executable code — pi loads it in-process — and the
// final-path check never sees it, because the file it Lstats is the one inside
// the link's target.
//
// bin/ only, and Pi's only. ~/.claude, ~/.codex and ~/.cursor are directories a
// user legitimately links into a dotfiles repo, so a blanket parent check would
// refuse installs that work today. This segment is different in kind: hookyard
// invents it and creates it, so nothing a user manages lives there, and a link
// found at it was put there for this write to follow.
func CheckPiBridgeDir(flagName, bridgePath string) error {
	dir := filepath.Dir(bridgePath)
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := os.Readlink(dir)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s is a symlink to %s, refusing to write through it: hookyard creates that directory "+
		"itself and puts executable code in it, so a link there redirects the file pi loads; either remove "+
		"the link or pass %s to point hookyard at a directory it can own", dir, target, flagName)
}

func checkNotSymlink(flagName, path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s is a symlink to %s, refusing to replace it: hookyard would replace the link rather "+
		"than write through it, silently detaching whatever manages it; either stop managing that path "+
		"elsewhere or pass %s to point hookyard at a file it can own", path, target, flagName)
}
