//go:build unix

package db

import (
	"net"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/keepalivetest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTCPKeepAliveIsTheSharedFU221Policy(t *testing.T) {
	want := net.KeepAliveConfig{Enable: true, Idle: 20 * time.Second, Interval: 5 * time.Second, Count: 3}
	if tcpKeepAlive != want {
		t.Fatalf("keepalive policy changed: %+v", tcpKeepAlive)
	}
}

func TestWithTCPKeepAliveKeepsTheConnectTimeoutAndEnablesKeepAlive(t *testing.T) {
	config, err := pgxpool.ParseConfig("postgres://user@127.0.0.1:1/db?sslmode=disable&connect_timeout=7")
	if err != nil {
		t.Fatal(err)
	}
	WithTCPKeepAlive(&config.ConnConfig.Config)
	if config.ConnConfig.ConnectTimeout != 7*time.Second {
		t.Fatalf("connect timeout changed: %s", config.ConnConfig.ConnectTimeout)
	}
	dialer := keepAliveDialer(config.ConnConfig.ConnectTimeout)
	if dialer.Timeout != 7*time.Second || dialer.KeepAliveConfig != tcpKeepAlive {
		t.Fatalf("dialer: timeout=%s keepalive=%+v", dialer.Timeout, dialer.KeepAliveConfig)
	}

	keepalivetest.RequireTCPKeepAlive(t, config.ConnConfig.DialFunc)
}

func TestNewPoolDialsWithTheSharedTCPKeepAlive(t *testing.T) {
	config, err := poolConfig(Config{DSN: "postgres://user@127.0.0.1:1/db?sslmode=disable", MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	keepalivetest.RequireTCPKeepAlive(t, config.ConnConfig.DialFunc)
}
