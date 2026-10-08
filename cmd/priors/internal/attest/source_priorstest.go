//go:build priorstest

package attest

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// The subprocess tests run as a user who may be in docker or wheel, as may a
// CI runner, which would switch attestation off before any check runs. This
// reads the verdict root from $PRIORS_TEST_VERDICTS and the groups from
// $PRIORS_TEST_GROUPS (name:gid,name:gid) instead. Only a build with -tags
// priorstest compiles this file; the nix build of priors sets no tags, so no
// flag, env var or file can reach it in a production binary.
func init() {
	if root := os.Getenv("PRIORS_TEST_VERDICTS"); root != "" {
		VerdictRoot = root
	}
	if spec := os.Getenv("PRIORS_TEST_GROUPS"); spec != "" {
		OSGroups = parseEnvGroups(spec)
	}
}

// envGroups is a GroupSource over name:gid pairs; a malformed spec fails
// every call.
type envGroups struct {
	names map[int]string
	gids  []int
	err   error
}

func parseEnvGroups(spec string) envGroups {
	g := envGroups{names: map[int]string{}}
	for pair := range strings.SplitSeq(spec, ",") {
		name, id, ok := strings.Cut(pair, ":")
		gid, err := strconv.Atoi(id)
		if !ok || err != nil {
			return envGroups{err: fmt.Errorf("PRIORS_TEST_GROUPS: bad pair %q", pair)}
		}
		g.names[gid] = name
		g.gids = append(g.gids, gid)
	}
	return g
}

func (g envGroups) Gids() ([]int, error) { return slices.Clone(g.gids), g.err }

func (g envGroups) Name(gid int) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	name, ok := g.names[gid]
	if !ok {
		return "", fmt.Errorf("unknown gid %d", gid)
	}
	return name, nil
}
