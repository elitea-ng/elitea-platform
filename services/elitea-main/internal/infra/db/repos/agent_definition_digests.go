package repos

// The canonical definition digest of an agent version (#1462), read from
// the version's stored definition: the shared
// application_version_details_json projection the start resolver reads the
// root version with (ResolveCurrentApplicationVersionDetails, a verbatim
// copy of ResolveCurrentApplicationTurn's). Admission compares it with the
// active binding's definition_digest; the binding admin API shows it so an
// operator can pin, or re-pin, a binding to the version actually run.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ReasonDefinitionDigestMismatch is the audit reason of an admission that
// bound no agent identity because the active binding's digest is not the
// computed digest of the version the run executes.
const ReasonDefinitionDigestMismatch = "definition_digest_mismatch"

// applicationVersionReader reads one stored application version
// (*CurrentNestedApplicationVersionRepository).
type applicationVersionReader interface {
	ReadCurrentApplicationVersion(ctx context.Context, projectID, applicationID, versionID int64) (storage.CurrentApplicationVersionRecord, error)
}

// AgentDefinitionDigests computes canonical version definition digests and
// records a binding whose digest does not match.
type AgentDefinitionDigests struct {
	versions applicationVersionReader
	pool     *pgxpool.Pool
	audit    securityaudit.Appender
}

// NewAgentDefinitionDigests builds the reader. audit may be nil for a
// read-only use (the admin API); RecordDefinitionMismatch then fails.
func NewAgentDefinitionDigests(pool *pgxpool.Pool, audit securityaudit.Appender) (*AgentDefinitionDigests, error) {
	versions, err := NewCurrentNestedApplicationVersionRepository(pool)
	if err != nil {
		return nil, err
	}
	return &AgentDefinitionDigests{versions: versions, pool: pool, audit: audit}, nil
}

// DefinitionDigest is agentidentity.VersionDefinitionDigest of the stored
// version. found is false when the tenant project holds no such version of
// the application (a version of another project, the published catalogue's
// included, is never read from this one).
func (d *AgentDefinitionDigests) DefinitionDigest(ctx context.Context, tenantProjectID, applicationID, applicationVersionID int64) ([32]byte, bool, error) {
	if d == nil || d.versions == nil {
		return [32]byte{}, false, errors.New("agent definition digests: no version reader")
	}
	if tenantProjectID <= 0 || applicationID <= 0 || applicationVersionID <= 0 {
		return [32]byte{}, false, nil
	}
	record, err := d.versions.ReadCurrentApplicationVersion(ctx, tenantProjectID, applicationID, applicationVersionID)
	if errors.Is(err, storage.ErrContentNotFound) {
		return [32]byte{}, false, nil
	}
	if err != nil {
		return [32]byte{}, false, fmt.Errorf("read the agent version definition: %w", err)
	}
	digest, err := agentidentity.VersionDefinitionDigest(tenantProjectID, applicationID, applicationVersionID, record.VersionDetails)
	if err != nil {
		return [32]byte{}, false, nil
	}
	return digest, true, nil
}

// RecordDefinitionMismatch appends the security audit decision of an
// admission that bound no agent identity because the binding's digest is
// not the version's (#1462). It names the binding and the version, never a
// definition.
func (d *AgentDefinitionDigests) RecordDefinitionMismatch(ctx context.Context, tenantProjectID int64, bindingID string, applicationID, applicationVersionID int64) error {
	if d == nil || d.pool == nil || d.audit == nil {
		return errors.New("agent definition digests: no audit appender")
	}
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		return d.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type:            securityaudit.TypeDecision,
			TenantProjectID: tenantProjectID,
			Action:          "agent_identity_binding.bind",
			Effect:          securityaudit.EffectDeny,
			Reasons:         []string{ReasonDefinitionDigestMismatch, "application." + strconv.FormatInt(applicationID, 10)},
			AgentBindingID:  bindingID,
			ResourceType:    "application_version",
			ResourceID:      strconv.FormatInt(applicationVersionID, 10),
		})
	})
}
