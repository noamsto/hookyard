package attestcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/noamsto/hookyard/cmd/priors/internal/attest"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// Signer returns the armoured SSHSIG over payload made with the key behind handle.
type Signer interface {
	Sign(ctx context.Context, handle string, payload []byte) ([]byte, error)
}

// Keygen signs with the pinned ssh-keygen. Stderr receives its touch and PIN
// prompts; nil means os.Stderr.
type Keygen struct {
	Path, Helper, Home string
	RequireTTY         bool
	Stderr             io.Writer
}

var ttyPath = "/dev/tty"

// CheckTerminal requires a controlling terminal when RequireTTY is set:
// ssh-keygen reads the PIN from stdin when /dev/tty cannot be opened, which
// would spend a PIN retry on the payload.
func (k Keygen) CheckTerminal() error {
	if !k.RequireTTY {
		return nil
	}
	f, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
	if err != nil {
		return errors.New("priors attest needs an interactive terminal for the PIN")
	}
	return f.Close()
}

// Sign runs `ssh-keygen -Y sign` with no file argument, so the payload goes
// over a pipe: no same-user process can swap a payload file between the
// human's review and the signature. The environment is built, not scrubbed:
// only these four variables reach the signer, so SSH_SK_PROVIDER,
// SSH_AUTH_SOCK, SSH_ASKPASS, LD_*, OPENSSL_* and the like cannot redirect
// the key, the agent, the PIN prompt or the loaded libraries.
func (k Keygen) Sign(ctx context.Context, handle string, payload []byte) ([]byte, error) {
	if err := k.CheckTerminal(); err != nil {
		return nil, err
	}
	cmd := tools.Command(ctx, k.Path, "-Y", "sign", "-n", attest.Namespace, "-f", handle)
	cmd.Env = []string{"PATH=" + filepath.Dir(k.Path), "HOME=" + k.Home, "LANG=C", "SSH_SK_HELPER=" + k.Helper}
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stderr = k.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh-keygen: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(stdout, attest.MaxEntryBytes+1))
	if err == nil && len(out) > attest.MaxEntryBytes {
		err = fmt.Errorf("signature over %d bytes", attest.MaxEntryBytes)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("ssh-keygen: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ssh-keygen sign failed: %w", err)
	}
	return append(bytes.TrimRight(out, "\n"), '\n'), nil
}
