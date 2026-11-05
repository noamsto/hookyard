package verify

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// drainWait must be a future deadline: a past one makes Accept time out
// before the accept syscall, draining nothing.
const drainWait = 20 * time.Millisecond

// Listener is a net.Listener whose Accept can be bounded by a deadline;
// *net.UnixListener and *net.TCPListener satisfy it.
type Listener interface {
	net.Listener
	SetDeadline(time.Time) error
}

// Serve runs once for the connections already queued, then once more for
// every batch that arrived during a run. Connections are only a wake-up
// signal: they are closed without being read.
func Serve(l Listener, run func()) {
	drain(l)
	run()
	for drain(l) > 0 {
		run()
	}
}

func drain(l Listener) int {
	n := 0
	for {
		if err := l.SetDeadline(time.Now().Add(drainWait)); err != nil {
			return n
		}
		conn, err := l.Accept()
		if err != nil {
			return n
		}
		_ = conn.Close()
		n++
	}
}

// SystemdListener returns the socket-activated listener on fd 3, or nil, nil
// when this process was not socket-activated.
func SystemdListener() (Listener, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil
	}
	fds, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || fds < 1 {
		return nil, nil
	}
	f := os.NewFile(3, "trigger")
	defer f.Close()
	fl, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("socket activation: %w", err)
	}
	l, ok := fl.(Listener)
	if !ok {
		_ = fl.Close()
		return nil, fmt.Errorf("socket activation: %T has no deadline", fl)
	}
	return l, nil
}
