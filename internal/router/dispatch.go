package router

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/record"
	"github.com/noamsto/hookyard/internal/verdict"
)

// dispatch starts a fire-and-forget handler and returns the instant Start does
// (§4). Nothing about the child after that point — its exit status, its runtime,
// its output — is knowable to hookyard, so the only two outcomes here are the
// start hookyard witnessed and the failure hookyard witnessed.
func dispatch(h manifest.Handler, payload []byte) HandlerResult {
	started := time.Now()

	stdin, err := stageStdin(payload)
	if err != nil {
		return HandlerResult{
			ID:      h.ID,
			Outcome: record.OutcomeError,
			Elapsed: time.Since(started),
			Verdict: verdict.Abstain,
			Message: fmt.Sprintf("failed to stage stdin: %v", err),
		}
	}
	// The child holds its own descriptor once forked, so this closes the
	// router's copy and nothing else.
	defer func() { _ = stdin.Close() }()

	// exec.Command, never exec.CommandContext: the router's deadline must not be
	// able to kill a handler the router has promised not to wait for. Wait is
	// never called for the same reason.
	cmd := exec.Command(h.Exec)
	cmd.Stdin = stdin
	// The load-bearing line. When an engine gives up on the router at its 5 s
	// emitted timeout it signals the router's process group, and a child in that
	// group dies with it; a child in its own session does not, and has no
	// controlling terminal to write to either. Setsid is Unix-only and goes in
	// inline with no build tag and no stub: hookyard is Unix-only throughout —
	// XDG state paths, flock, a shell-executable exec contract.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Stdout and Stderr stay nil, which os/exec wires to /dev/null. The lane has
	// no output contract, and it keeps the child off the router's stdout pipe,
	// which would otherwise hold the engine open for the handler's whole runtime.

	if err := cmd.Start(); err != nil {
		return HandlerResult{
			ID:      h.ID,
			Outcome: record.OutcomeError,
			Elapsed: time.Since(started),
			Verdict: verdict.Abstain,
			Message: fmt.Sprintf("failed to start: %v", err),
		}
	}
	// Elapsed is dispatch cost — staging plus Start — and says nothing about the
	// work that follows (§6).
	return HandlerResult{
		ID:      h.ID,
		Outcome: record.OutcomeDispatched,
		Elapsed: time.Since(started),
		Verdict: verdict.Abstain,
	}
}

// stageStdin writes payload to an unlinked temp file and hands back a read-only
// handle on it. A pipe cannot be used: bytes.NewReader makes os/exec spawn a
// copier goroutine that only Wait drains, so without a Wait every byte past the
// 64 KiB pipe buffer is lost the moment the router exits.
func stageStdin(payload []byte) (*os.File, error) {
	// CreateTemp's 0600 is what the payload needs: it carries tool input. The
	// directory matters just as much — the verdict lane never puts that input on
	// disk at all, so this lane stages it in the user-owned 0700 tmpfs at
	// $XDG_RUNTIME_DIR, where it never reaches a block device to survive the
	// unlink and no other user can traverse to it. Unset (macOS) falls back to
	// CreateTemp's default, which is the old behaviour and still 0600.
	f, err := os.CreateTemp(os.Getenv("XDG_RUNTIME_DIR"), "hookyard-dispatch-*")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return nil, err
	}
	// Reopening read-only is not ceremony: CreateTemp's handle sits at EOF after
	// the write and fork/exec shares that file offset, so handing it to the child
	// directly would deliver an immediate EOF and zero bytes. It also denies the
	// child a writable handle on its own stdin.
	stdin, err := os.Open(f.Name())
	// Unlinking before Start leaves nothing on disk whether the child finishes,
	// crashes, or is still running an hour later.
	_ = os.Remove(f.Name())
	if err != nil {
		return nil, err
	}
	return stdin, nil
}
