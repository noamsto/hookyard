//go:build priorstest

package attestcmd

import (
	"os"
	"time"
)

// The subprocess tests run without a controlling terminal and cannot wait a
// full minute per verdict. This lets $PRIORS_TEST_NO_TTY skip the /dev/tty
// requirement and reads the wait from $PRIORS_TEST_WAIT (a Go duration)
// instead. Only a build with -tags priorstest compiles this file; the nix
// build of priors sets no tags, so no flag, env var or file can reach it in a
// production binary.
func init() {
	if os.Getenv("PRIORS_TEST_NO_TTY") != "" {
		skipTTY = true
	}
	if d, err := time.ParseDuration(os.Getenv("PRIORS_TEST_WAIT")); err == nil {
		DefaultWait = d
	}
}
