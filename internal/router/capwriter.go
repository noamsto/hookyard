package router

import (
	"bytes"
	"errors"
	"sync"
)

// MaxHandlerOutput is §5's stdout cap. A handler producing more than this is a
// defect, and a defect's output is not a verdict to be trusted.
const MaxHandlerOutput = 64 << 10

// errCapExceeded stops the copier os/exec runs between the handler's stdout
// pipe and this writer. Returning an error from Write is what makes the cap
// bite as bytes arrive rather than after the process exits; a handler looping
// on print would otherwise be bounded only by its sub-budget, and by then it
// has already had a pipe buffer's worth of the machine.
var errCapExceeded = errors.New("router: handler output exceeded cap")

// capWriter collects at most limit bytes and kills the handler on the first
// byte past it. The mutex is not decoration: cmd.WaitDelay lets Wait return
// while the copier goroutine is still live, so the read side locks too.
type capWriter struct {
	limit int
	kill  func()

	mu         sync.Mutex
	buf        bytes.Buffer
	overflowed bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.overflowed {
		return 0, errCapExceeded
	}
	room := w.limit - w.buf.Len()
	if len(p) <= room {
		return w.buf.Write(p)
	}
	n, _ := w.buf.Write(p[:room])
	w.overflowed = true
	w.kill()
	return n, errCapExceeded
}

// collected returns a copy rather than the buffer's own bytes, since a copier
// Wait gave up on may still be appending to it.
func (w *capWriter) collected() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buf.Bytes()), w.overflowed
}
