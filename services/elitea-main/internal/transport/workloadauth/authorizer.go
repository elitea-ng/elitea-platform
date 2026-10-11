// Package workloadauth binds a verified mTLS peer identity to a durable
// workload session. Certificate identity alone never authorizes a session or
// producer selected by the caller.
package workloadauth

import (
	"context"
	"errors"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var ErrWorkloadUnauthorized = errors.New("workload peer is not authorized")

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
		return "", ErrWorkloadUnauthorized
	}
	return identity, nil
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
