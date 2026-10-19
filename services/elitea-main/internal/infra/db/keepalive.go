package db

import (
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// tcpKeepAlive is the client TCP keepalive of every PostgreSQL pool Main owns
// (FU-221): a peer that vanished without closing the socket (a killed
// PostgreSQL or pgbouncer, a dropped NAT entry) is detected in about
// 20s + 3*5s instead of the operating-system default of hours. It is TCP only:
// no startup parameter is sent, so it is safe through pgbouncer.
var tcpKeepAlive = net.KeepAliveConfig{
	Enable:   true,
	Idle:     20 * time.Second,
	Interval: 5 * time.Second,
	Count:    3,
}

// WithTCPKeepAlive makes config dial with [tcpKeepAlive], keeping its connect
// timeout. Call it after parsing the DSN and before building the pool.
func WithTCPKeepAlive(config *pgconn.Config) {
	config.DialFunc = keepAliveDialer(config.ConnectTimeout).DialContext
}

func keepAliveDialer(connectTimeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: connectTimeout, KeepAliveConfig: tcpKeepAlive}
}
