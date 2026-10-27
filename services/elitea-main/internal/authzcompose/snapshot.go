package authzcompose

import (
	"context"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// decisionSnapshot holds the rows one decision read, so every gate judges
// the same grant and mandate and each is read once per decision. Decide
// runs its gates in order on one goroutine, so it needs no lock.
type decisionSnapshot struct {
	grantRead   bool
	grant       credentialgrant.CredentialUseGrant
	grantErr    error
	mandateRead bool
	mandate     mandate.Mandate
	mandateErr  error
}

type decisionSnapshotKey struct{}

func withDecisionSnapshot(ctx context.Context) context.Context {
	return context.WithValue(ctx, decisionSnapshotKey{}, &decisionSnapshot{})
}

// snapshotGrant reads the request's grant once per decision. Outside
// Decide (a gate evaluated alone) it reads through.
func snapshotGrant(ctx context.Context, grants credentialgrant.GrantReader, key credentialgrant.GrantKey) (credentialgrant.CredentialUseGrant, error) {
	s, ok := ctx.Value(decisionSnapshotKey{}).(*decisionSnapshot)
	if !ok {
		return grants.GrantForKey(ctx, key)
	}
	if !s.grantRead {
		s.grant, s.grantErr = grants.GrantForKey(ctx, key)
		s.grantRead = true
	}
	return s.grant, s.grantErr
}

// snapshotMandate reads the request's mandate once per decision.
func snapshotMandate(ctx context.Context, mandates mandate.Reader, id string) (mandate.Mandate, error) {
	s, ok := ctx.Value(decisionSnapshotKey{}).(*decisionSnapshot)
	if !ok {
		return mandates.Get(ctx, id)
	}
	if !s.mandateRead {
		s.mandate, s.mandateErr = mandates.Get(ctx, id)
		s.mandateRead = true
	}
	return s.mandate, s.mandateErr
}
