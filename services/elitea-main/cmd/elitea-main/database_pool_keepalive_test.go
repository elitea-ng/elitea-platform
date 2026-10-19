//go:build unix

package main

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/keepalivetest"
)

// FU-221: every pool Main serves from dials with the shared TCP keepalive.
func TestMainPoolsDialWithTheSharedTCPKeepAlive(t *testing.T) {
	const dsn = "postgres://localhost:5432/elitea?sslmode=disable&connect_timeout=5"
	platform, err := databasePoolConfig(dsn, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	keepalivetest.RequireTCPKeepAlive(t, platform.ConnConfig.DialFunc)
	runtime, err := runtimePostgresPoolConfig(dsn, runtimePoolSpec{role: "control", maxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	keepalivetest.RequireTCPKeepAlive(t, runtime.ConnConfig.DialFunc)
	if runtime.ConnConfig.ConnectTimeout != platform.ConnConfig.ConnectTimeout {
		t.Fatal("the connect timeout must survive the keepalive dialer")
	}
}
