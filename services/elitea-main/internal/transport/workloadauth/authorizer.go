// Package workloadauth binds a verified mTLS peer identity to a durable
// workload session. Certificate identity alone never authorizes a session or
// producer selected by the caller.
package workloadauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/depfailure"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var ErrWorkloadUnauthorized = errors.New("workload peer is not authorized")

// ErrSessionStoreUnavailable marks a refusal whose only cause is that the
// session registry could not be read because of a dependency failure that
// heals on its own (depfailure.Transient: a database restart, recovery or
// PgBouncer login retry). The request is still refused, so authorization
// stays fail closed; the marker lets a caller answer "retry" instead of
// "not authenticated" (FU-275). It never accompanies a session that is
// unknown, expired, revoked or bound to another workload.
var ErrSessionStoreUnavailable = errors.New("workload session store is temporarily unavailable")

// SessionStoreUnavailable reports whether err is a refusal caused only by an
// unreadable session registry. Such an err always also matches
// ErrWorkloadUnauthorized.
func SessionStoreUnavailable(err error) bool {
	return errors.Is(err, ErrSessionStoreUnavailable)
}

type SessionBinding struct {
	WorkloadIdentity  string
	WorkloadSessionID string
	ProducerID        string
}

// SessionBindingVerifier checks one exact binding against the authoritative
// persisted session registry. The implementation owns expiry and revocation
// checks and must use the state owner's clock; it must not fall back to a
// process-local allowlist.
type SessionBindingVerifier interface {
	VerifyActiveSession(ctx context.Context, binding SessionBinding) error
}

type PeerAuthorizer struct {
	sessions SessionBindingVerifier

	logger *slog.Logger
	server string
	mu     sync.Mutex
	since  time.Time // set while the session registry is unreadable
}

// WithLogger names this authorizer's server ("runtime control", "runtime
// output") and logs, once per outage, when its session registry becomes
// unreadable and when it can be read again.
func (a *PeerAuthorizer) WithLogger(logger *slog.Logger, server string) *PeerAuthorizer {
	a.logger = logger
	a.server = server
	return a
}

func NewPeerAuthorizer(sessions SessionBindingVerifier) (*PeerAuthorizer, error) {
	if sessions == nil {
		return nil, errors.New("persisted workload session verifier is required")
	}
	return &PeerAuthorizer{sessions: sessions}, nil
}

func (a *PeerAuthorizer) AuthorizeWorkload(ctx context.Context, workloadSessionID, producerID string) (string, error) {
	return a.authorize(ctx, workloadSessionID, producerID)
}

func (a *PeerAuthorizer) AuthorizeOutput(ctx context.Context, workloadSessionID, producerID string) (string, error) {
	return a.authorize(ctx, workloadSessionID, producerID)
}

func (a *PeerAuthorizer) authorize(ctx context.Context, workloadSessionID, producerID string) (string, error) {
	if !boundedIdentityPart(workloadSessionID) || !boundedIdentityPart(producerID) {
		return "", ErrWorkloadUnauthorized
	}
	identity, err := VerifiedPeerIdentity(ctx)
	if err != nil {
		return "", ErrWorkloadUnauthorized
	}
	binding := SessionBinding{
		WorkloadIdentity:  identity,
		WorkloadSessionID: workloadSessionID,
		ProducerID:        producerID,
	}
	if err := a.sessions.VerifyActiveSession(ctx, binding); err != nil {
		if !errors.Is(err, ErrWorkloadUnauthorized) && depfailure.Transient(err, ctx) {
			a.observe(err)
			// Refused all the same. The cause is described, never carried:
			// no database error text, session id or token leaves here.
			return "", fmt.Errorf("%w: %w", ErrWorkloadUnauthorized, ErrSessionStoreUnavailable)
		}
		return "", ErrWorkloadUnauthorized
	}
	a.observe(nil)
	return identity, nil
}

// observe logs the registry's transitions once per outage.
func (a *PeerAuthorizer) observe(err error) {
	if a.logger == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err != nil && a.since.IsZero():
		a.since = time.Now()
		a.logger.Warn(
			fmt.Sprintf("The %s server cannot read workload sessions; Workers are answered \"retry\" until the database is back", a.server),
			"code", faults.WorkloadSessionStoreUnavailable.String(),
			"server", a.server,
			"cause", depfailure.Describe(err),
		)
	case err == nil && !a.since.IsZero():
		outage := time.Since(a.since).Round(time.Second)
		a.since = time.Time{}
		a.logger.Info(
			fmt.Sprintf("The %s server reads workload sessions again after %s", a.server, outage),
			"server", a.server,
			"outage", outage.String(),
		)
	}
}

// VerifiedPeerIdentity is the canonical identity (one DNS name or SPIFFE ID,
// by the workloadidentity rule) of the verified mTLS peer of ctx. A context
// with no peer, a non-TLS peer or no verified chain is
// ErrWorkloadUnauthorized.
func VerifiedPeerIdentity(ctx context.Context) (string, error) {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo == nil || peerInfo.AuthInfo == nil {
		return "", ErrWorkloadUnauthorized
	}
	var state credentials.TLSInfo
	switch tlsInfo := peerInfo.AuthInfo.(type) {
	case credentials.TLSInfo:
		state = tlsInfo
	case *credentials.TLSInfo:
		if tlsInfo == nil {
			return "", ErrWorkloadUnauthorized
		}
		state = *tlsInfo
	default:
		return "", ErrWorkloadUnauthorized
	}
	if len(state.State.VerifiedChains) == 0 || len(state.State.VerifiedChains[0]) == 0 {
		return "", ErrWorkloadUnauthorized
	}
	return workloadidentity.Certificate(state.State.VerifiedChains[0][0])
}

func boundedIdentityPart(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}
