//go:build unix

// Package keepalivetest proves, at the socket, that a PostgreSQL dialer applies
// the shared FU-221 TCP keepalive. Tests only.
package keepalivetest

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// RequireTCPKeepAlive dials a loopback listener with dial and fails t unless the
// kernel socket has keepalive on with a 5 s probe interval and 3 probes (the
// idle option's name differs per OS; the policy value is tested beside it).
func RequireTCPKeepAlive(t testing.TB, dial pgconn.DialFunc) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if accepted, err := listener.Accept(); err == nil {
			_ = accepted.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("dialer returned %T, want *net.TCPConn", conn)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var enabled, interval, count int
	var errs [3]error
	if err := raw.Control(func(fd uintptr) {
		enabled, errs[0] = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
		interval, errs[1] = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL)
		count, errs[2] = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT)
	}); err != nil {
		t.Fatal(err)
	}
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if enabled == 0 || interval != 5 || count != 3 {
		t.Fatalf("socket keepalive: enabled=%d interval=%d count=%d, want on/5/3", enabled, interval, count)
	}
}
