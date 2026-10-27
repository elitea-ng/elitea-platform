package repos

// The credential probe's reads and its audit record (ADR-0039, issue
// #1120). With the governance flag off nothing builds the probe handler, so
// nothing here runs.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// maxProbeGrants bounds the read: the handler needs one grant that covers
// the reference, never a full listing.
const maxProbeGrants = 64

// ActiveGrantsWithCapability lists the active, unexpired grants of a
// credential reference in a tenant project that list capability.
func (s *CredentialGrants) ActiveGrantsWithCapability(
	ctx context.Context, tenantProjectID int64, referenceID string, capability credentialgrant.Capability,
) ([]credentialgrant.CredentialUseGrant, error) {
	if err := s.readReady(); err != nil {
		return nil, err
	}
	if !agentIdentityIsUUID(referenceID) || !capability.Valid() {
		return []credentialgrant.CredentialUseGrant{}, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+credentialGrantColumnList+`
FROM elitea_identity.credential_use_grants
WHERE tenant_project_id = $1 AND credential_reference_id = $2::uuid
  AND state = 'active' AND $3 = ANY(capabilities)
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY created_at DESC, id DESC
LIMIT $4`, tenantProjectID, referenceID, string(capability), maxProbeGrants)
	if err != nil {
		return nil, fmt.Errorf("list probe grants: %w", err)
	}
	defer rows.Close()
	out := []credentialgrant.CredentialUseGrant{}
	for rows.Next() {
		g, err := scanCredentialGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list probe grants: %w", err)
	}
	return out, nil
}

// RecordProbe appends one probe audit event in its own transaction. An
// append error is returned: the probe handler then answers no result.
func (s *CredentialGrants) RecordProbe(ctx context.Context, event securityaudit.SecurityEvent) error {
	if err := s.writeReady(); err != nil {
		return err
	}
	if event.Type != securityaudit.TypeProbe {
		return fmt.Errorf("record probe: event type %q is not a probe", event.Type)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin probe audit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("audit probe: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit probe audit: %w", err)
	}
	return nil
}
