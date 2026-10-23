package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// The probe against a real platform listener: SERVING with a runtime-CA
// workload certificate, a failure without one, NOT_FOUND for an unknown
// service, and a usage error for missing flags.
func TestProbeAgainstThePlatformListener(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := runtimegrpc.NewPlatformServer(runtimegrpc.PlatformServerConfig{
		Address: listener.Addr().String(),
		TLS:     pki.ServerTLS(t),
		Policy:  runtimegrpc.DefaultPlatformGRPCPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	ca, cert, key := pki.WriteFiles(t, t.TempDir(), "elitea-worker")
	address := listener.Addr().String()

	for name, testCase := range map[string]struct {
		args   []string
		code   int
		output string
	}{
		"serving": {
			args: []string{"--addr", address, "--ca", ca, "--cert", cert, "--key", key, "--server-name", grpcauthtest.ServerName},
			code: 0, output: "SERVING",
		},
		"no client certificate": {
			args: []string{"--addr", address, "--ca", ca, "--server-name", grpcauthtest.ServerName, "--timeout", "3s"},
			code: 1, output: "Unavailable",
		},
		"wrong server name": {
			args: []string{"--addr", address, "--ca", ca, "--cert", cert, "--key", key, "--server-name", "not-elitea-main", "--timeout", "3s"},
			code: 1, output: "Unavailable",
		},
		"unknown service": {
			args: []string{"--addr", address, "--ca", ca, "--cert", cert, "--key", key, "--server-name", grpcauthtest.ServerName, "--service", "elitea.nothing.v1.Nope"},
			code: 1, output: "NotFound",
		},
		"usage": {args: []string{"--addr", address}, code: 2, output: "required"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(testCase.args, &stdout, &stderr)
		combined := stdout.String() + stderr.String()
		if code != testCase.code || !strings.Contains(combined, testCase.output) {
			t.Errorf("%s: exit %d, output %q; want exit %d containing %q", name, code, combined, testCase.code, testCase.output)
		}
	}
}
