package verify

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type countingListener struct {
	*net.UnixListener
	accepted atomic.Int64
}

func (c *countingListener) Accept() (net.Conn, error) {
	conn, err := c.UnixListener.Accept()
	if err == nil {
		c.accepted.Add(1)
	}
	return conn, err
}

func newCountingListener(t *testing.T) (*countingListener, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	ul, ok := l.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.UnixListener", l)
	}
	return &countingListener{UnixListener: ul}, sock
}

func dialN(t *testing.T, sock string, n int) []net.Conn {
	t.Helper()
	conns := make([]net.Conn, 0, n)
	for range n {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		conns = append(conns, c)
	}
	return conns
}

func TestServeQueuesOneRun(t *testing.T) {
	l, sock := newCountingListener(t)
	conns := dialN(t, sock, 3)

	var runs atomic.Int64
	Serve(l, func() {
		if runs.Add(1) == 1 {
			conns = append(conns, dialN(t, sock, 20)...)
		}
	})

	if got := runs.Load(); got != 2 {
		t.Errorf("runs = %d, want 2", got)
	}
	if got := l.accepted.Load(); got != 23 {
		t.Errorf("accepted = %d, want 23", got)
	}
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := c.Read(make([]byte, 1))
		if n != 0 || (!errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET)) {
			t.Fatalf("client read = %d, %v; want 0 and EOF or ECONNRESET", n, err)
		}
	}
}

func TestServeIdleRunsOnce(t *testing.T) {
	l, _ := newCountingListener(t)

	var runs atomic.Int64
	Serve(l, func() { runs.Add(1) })

	if got := runs.Load(); got != 1 {
		t.Errorf("runs = %d, want 1", got)
	}
}

func TestSystemdListener(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv("LISTEN_PID", "")
		t.Setenv("LISTEN_FDS", "")
		l, err := SystemdListener()
		if l != nil || err != nil {
			t.Errorf("got %v, %v; want nil, nil", l, err)
		}
	})
	t.Run("other pid", func(t *testing.T) {
		t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()+1))
		t.Setenv("LISTEN_FDS", "1")
		l, err := SystemdListener()
		if l != nil || err != nil {
			t.Errorf("got %v, %v; want nil, nil", l, err)
		}
	})
}
