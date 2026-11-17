package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// The attach path of delegation grants (ADR-0044 amendment 1, A1.2 rule 2).
//
// Attaching a sub-agent, an agent tool or a pipeline child X to a caller Y
// creates a PENDING grant for the edge Y to X in the transaction that writes
// the attachment, so a composition either has its grant or does not exist. The
// grant is approved by an editor of Y in the attach dialog (story S3), or
// through its change request under the governed profile. Detaching revokes the
// grant when no version of Y still attaches X.
//
// Nothing here refuses an attach, and nothing here changes what runs: the
// worker consumes approved grants only (story S2c).

// WithDelegationCallees composes the reader of what a callee version holds.
// Without it the attach path creates no grant.
func WithDelegationCallees(callees interface {
	Snapshot(ctx context.Context, projectID, applicationID, versionID int64) (dg.Callee, error)
}) DelegationGrantsOption {
	return func(s *DelegationGrants) { s.callees = callees }
}

// WithDelegationGoverned composes the reader of the governed profile: while it
// answers true, a grant is proposed as governed and its approval goes through
// a change request. Without it no grant is governed.
func WithDelegationGoverned(governed func(context.Context) bool) DelegationGrantsOption {
	return func(s *DelegationGrants) { s.governed = governed }
}

// DelegationAttach is an attachment written in the caller's transaction.
type DelegationAttach struct {
	ProjectID       int64
	CallerID        int64
	CalleeID        int64
	CalleeVersionID int64
	// Pipeline is true when the caller is a pipeline: the edge is then a
	// pipeline child, and an agent tool otherwise.
	Pipeline bool
	// Actor is the person who attached. Without one no grant is proposed.
	Actor int64
}

func (a DelegationAttach) edge() dg.Edge {
	kind := dg.EdgeAgentTool
	if a.Pipeline {
		kind = dg.EdgePipelineChild
	}
	return dg.Edge{CallerEntityID: a.CallerID, CalleeEntityID: a.CalleeID, Kind: kind}
}

// AttachTx proposes the grant of the attached edge in the attach's
// transaction. A callee that holds nothing needs no grant (no toolkit, no
// host, no data class), and an edge that already has an active grant keeps
// it: the drift of a new callee version is the drift check's business.
func (s *DelegationGrants) AttachTx(ctx context.Context, tx pgx.Tx, attach DelegationAttach) error {
	if s == nil || s.callees == nil || attach.Actor <= 0 {
		return nil
	}
	callee, err := s.callees.Snapshot(ctx, attach.ProjectID, attach.CalleeID, attach.CalleeVersionID)
	if errors.Is(err, dg.ErrCalleeNotFound) {
		return nil // the attach itself refuses a version that does not exist
	}
	if err != nil {
		return err
	}
	if len(callee.Set.Toolkits) == 0 && len(callee.Set.EgressHosts) == 0 && len(callee.Set.DataClasses) == 0 {
		return nil
	}
	actor := attach.Actor
	_, err = s.ProposeTx(ctx, tx, ProposeInput{
		ProjectID: attach.ProjectID, Edge: attach.edge(), CalleeVersionID: attach.CalleeVersionID, Requested: callee.Set,
		Origin: dg.OriginAttach, Governed: s.governed != nil && s.governed(ctx), ProposedBy: &actor,
	})
	if errors.Is(err, dg.ErrActiveEdgeExists) {
		return nil
	}
	return err
}

// DetachTx revokes the grant of the detached edge, in the detach's
// transaction, once no version of the caller attaches the callee any more.
// The attachment is already deleted when this runs.
func (s *DelegationGrants) DetachTx(ctx context.Context, tx pgx.Tx, attach DelegationAttach) error {
	if s == nil {
		return nil
	}
	schema, err := tenantSchema(strconv.FormatInt(attach.ProjectID, 10))
	if err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT count(*)
FROM %s.entity_tool_mapping AS mapping
JOIN %s.elitea_tools AS tool ON tool.id = mapping.tool_id
JOIN %s.application_versions AS version ON version.id = mapping.entity_version_id
WHERE mapping.entity_type = 'agent' AND tool.type = 'application'
  AND version.application_id = $1 AND tool.settings ->> 'application_id' = $2`, schema, schema, schema),
		attach.CallerID, strconv.FormatInt(attach.CalleeID, 10)).Scan(&remaining); err != nil {
		return fmt.Errorf("delegation grants: count the remaining attachments: %w", err)
	}
	if remaining > 0 {
		return nil
	}
	_, err = s.RevokeEdgeTx(ctx, tx, attach.ProjectID, attach.edge(), attach.Actor)
	return err
}
