package render

import (
	"fmt"
	"os"
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
// The caller runs this over all three paths before the first write, so a
// refusal on one engine cannot leave the other two already rewritten.
func CheckDestinations(claude, codex, cursor string) error {
	for _, d := range []struct{ flag, path string }{
		{"--claude-settings", claude},
		{"--codex-config", codex},
		{"--cursor-hooks", cursor},
	} {
		if err := checkNotSymlink(d.flag, d.path); err != nil {
			return err
		}
	}
	return nil
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
