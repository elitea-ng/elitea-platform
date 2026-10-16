package runtimegrpc

import (
	"time"

	"google.golang.org/grpc/keepalive"
)

// The keepalive enforcement policy of Main's private runtime gRPC servers
// (control and output). It lives here, in one place, and every private runtime
// server gets it through RuntimeKeepaliveEnforcementPolicy. It is deliberately
// not a field of GRPCServerPolicy: a per-deployment value could drift below the
// Worker's ping interval and abort healthy connections.
//
// The Worker (services/elitea-worker-rust/src/transport/keepalive.rs) pings every
// idle channel to Main so that a dead Main Pod is found in bounded time:
//
//	HTTP2_KEEPALIVE_INTERVAL = 15 s   idle time before an HTTP/2 PING
//	HTTP2_KEEPALIVE_TIMEOUT  =  5 s   unanswered PING closes the connection
//	KEEPALIVE_WHILE_IDLE     = true   pings are sent with no call in flight
//
// A dead peer is therefore detected in interval + timeout = 20 s, well under the
// 60 s claim lease. grpc-go counts a ping that arrives sooner than MinTime (or,
// without PermitWithoutStream, any ping on a connection with no stream) as a
// strike and answers the third strike with GOAWAY too_many_pings. That aborted
// idle control and output connections and long silent output streams after
// about 40 s when MinTime was 30 s and PermitWithoutStream was false.
//
// Invariant, pinned by tests on both sides: RuntimeKeepaliveMinTime is below the
// Worker's HTTP2_KEEPALIVE_INTERVAL with a margin (10 s < 15 s).
//
// Both servers verify client certificates before this applies: the listener TLS
// configuration is validated by validateServerTLSConfig (TLS 1.3, ClientAuth
// RequireAndVerifyClientCert, a client CA pool), so only mTLS-authenticated
// internal peers can open a connection and send an idle ping. A flood of pings
// from such a peer is still bounded: pings closer than MinTime are strikes.
const (
	// RuntimeKeepaliveMinTime is the shortest ping spacing a private runtime
	// server accepts from a client.
	RuntimeKeepaliveMinTime = 10 * time.Second
	// RuntimeKeepalivePermitWithoutStream accepts pings on a connection with no
	// stream, which is the Worker's idle-channel probe.
	RuntimeKeepalivePermitWithoutStream = true
)

// RuntimeKeepaliveEnforcementPolicy is the only enforcement policy a private
// runtime gRPC server may use.
func RuntimeKeepaliveEnforcementPolicy() keepalive.EnforcementPolicy {
	return newRuntimeKeepalivePolicy(RuntimeKeepaliveMinTime)
}

// newRuntimeKeepalivePolicy builds the policy from the minimum ping spacing so a
// real-connection test can scale time down. Production passes
// RuntimeKeepaliveMinTime and nothing else.
func newRuntimeKeepalivePolicy(minTime time.Duration) keepalive.EnforcementPolicy {
	return keepalive.EnforcementPolicy{
		MinTime:             minTime,
		PermitWithoutStream: RuntimeKeepalivePermitWithoutStream,
	}
}
