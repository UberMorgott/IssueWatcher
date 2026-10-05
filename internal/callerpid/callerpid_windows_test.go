package callerpid

import (
	"errors"
	"net"
	"os"
	"testing"
)

// dialSelf listens on listenAddr, connects to dialHost:<its port> from this
// process and returns the accepted (server side) connection.
func dialSelf(t *testing.T, listenNet, listenAddr, dialNet, dialHost string) net.Conn {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), listenNet, listenAddr)
	if err != nil {
		t.Skipf("listen %s %s: %v", listenNet, listenAddr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := (&net.Dialer{}).DialContext(t.Context(), dialNet, net.JoinHostPort(dialHost, port))
	if err != nil {
		t.Skipf("dial %s %s: %v", dialNet, dialHost, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLoopbackPIDFindsThisProcess(t *testing.T) {
	for _, tc := range []struct{ name, listenNet, listenAddr, dialNet, dialHost string }{
		{"ipv4", "tcp4", "127.0.0.1:0", "tcp4", "127.0.0.1"},
		{"ipv6", "tcp6", "[::1]:0", "tcp6", "::1"},
		{"dual-stack", "tcp", "[::]:0", "tcp4", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dialSelf(t, tc.listenNet, tc.listenAddr, tc.dialNet, tc.dialHost)
			pid, err := LoopbackPID(s.RemoteAddr().String(), s.LocalAddr().String())
			if err != nil || pid != uint32(os.Getpid()) { //nolint:gosec // G115: own pid
				t.Fatalf("%s -> %s: pid %d %v, want %d", s.RemoteAddr(), s.LocalAddr(), pid, err, os.Getpid())
			}
		})
	}
}

func TestLoopbackPIDRefusals(t *testing.T) {
	s := dialSelf(t, "tcp4", "127.0.0.1:0", "tcp4", "127.0.0.1")
	local := s.LocalAddr().String()
	if _, err := LoopbackPID("127.0.0.1:1", local); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown client port: %v", err)
	}
	if _, err := LoopbackPID("10.1.2.3:5000", local); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("non-loopback accepted: %v", err)
	}
	if _, err := LoopbackPID("bogus", local); err == nil {
		t.Fatal("bad address accepted")
	}
}
