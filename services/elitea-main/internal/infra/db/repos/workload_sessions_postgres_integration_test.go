package repos

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/workloadauth"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// TestPostgresServiceBackedWorkloadSessionRotationAndRevocation crosses the
// real PostgreSQL 16 protocol and the in-process mTLS-peer authorization
// interface. It is service-integration evidence, not a network TLS or process
// end-to-end test.
func TestPostgresServiceBackedWorkloadSessionRotationAndRevocation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const (
		workloadIdentity = "spiffe://elitea.test/runtime/python-worker"
		producerID       = "python-worker-1"
	)
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.workload_sessions (
    workload_session_id, workload_identity, producer_id,
    issued_at, expires_at, revoked_at
) VALUES
    ('session-old', $1, $2, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', NULL),
    ('session-new', $1, $2, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '2 hours', NULL),
    ('session-expired', $1, $2, clock_timestamp() - interval '2 hours', clock_timestamp() - interval '1 hour', NULL),
    ('session-future', $1, $2, clock_timestamp() + interval '1 hour', clock_timestamp() + interval '2 hours', NULL),
    ('session-revoked', $1, $2, clock_timestamp() - interval '1 hour', clock_timestamp() + interval '1 hour', clock_timestamp()),
    ('session-other-producer', $1, 'other-producer', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', NULL)`,
		workloadIdentity,
		producerID,
	); err != nil {
		t.Fatalf("seed workload-session rotation fixtures: %v", err)
	}

	repository, err := NewWorkloadSessionsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := workloadauth.NewPeerAuthorizer(repository)
	if err != nil {
		t.Fatal(err)
	}
	peerCtx := postgresVerifiedWorkloadPeer(t, workloadIdentity)

	for _, sessionID := range []string{"session-old", "session-new"} {
		identity, err := authorizer.AuthorizeWorkload(peerCtx, sessionID, producerID)
		if err != nil {
			t.Fatalf("active overlap session %q: %v", sessionID, err)
		}
		if identity != workloadIdentity {
			t.Fatalf("session %q identity = %q", sessionID, identity)
		}
	}

	for _, test := range []struct {
		name       string
		sessionID  string
		producerID string
	}{
		{name: "expired", sessionID: "session-expired", producerID: producerID},
		{name: "not yet issued", sessionID: "session-future", producerID: producerID},
		{name: "revoked", sessionID: "session-revoked", producerID: producerID},
		{name: "missing", sessionID: "session-missing", producerID: producerID},
		{name: "producer mismatch", sessionID: "session-other-producer", producerID: producerID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := authorizer.AuthorizeWorkload(peerCtx, test.sessionID, test.producerID); !errors.Is(err, workloadauth.ErrWorkloadUnauthorized) {
				t.Fatalf("error = %v, want workload unauthorized", err)
			}
		})
	}

	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.workload_sessions
SET revoked_at = clock_timestamp()
WHERE workload_session_id = 'session-old'`); err != nil {
		t.Fatalf("revoke old workload session: %v", err)
	}
	if _, err := authorizer.AuthorizeWorkload(peerCtx, "session-old", producerID); !errors.Is(err, workloadauth.ErrWorkloadUnauthorized) {
		t.Fatalf("revoked old session returned %v", err)
	}
	if _, err := authorizer.AuthorizeWorkload(peerCtx, "session-new", producerID); err != nil {
		t.Fatalf("new session failed after old-session revocation: %v", err)
	}
}

func postgresVerifiedWorkloadPeer(t *testing.T, rawIdentity string) context.Context {
	t.Helper()
	identity, err := url.Parse(rawIdentity)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{URIs: []*url.URL{identity}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}},
	}})
}
