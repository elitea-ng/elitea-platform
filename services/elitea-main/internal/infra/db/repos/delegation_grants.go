package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// DelegationGrants stores the delegation grants of a project (tenant 0252,
// ADR-0044 amendment 1). Every state change is one transaction with its
// security audit event (event type securityaudit.TypeAgentLifecycle, actions
// dg.Action*): a change without its evidence does not happen.
//
// The store applies the domain rules (maker-checker, the approver's access,
// the state machine). The unique active edge and the maker-checker CHECK in
// the table are the second line: a code path that forgets a rule is refused
// by the database.
type DelegationGrants struct {
	pool    *pgxpool.Pool
	audit   securityaudit.Appender
	now     func() time.Time
	changes DelegationChangeRecorder
	access  DelegationAccessResolver
	// callees and governed serve the attach path (delegation_attach.go).
	callees interface {
		Snapshot(ctx context.Context, projectID, applicationID, versionID int64) (dg.Callee, error)
	}
	governed func(context.Context) bool
}

// DelegationChangeRecorder records the ADR-0040 change request of a governed
// grant in the proposing transaction (AgentLifecycle satisfies it).
type DelegationChangeRecorder interface {
	RecordDelegationChangeTx(ctx context.Context, tx pgx.Tx, change agentlifecycle.NewDelegationChange) (agentlifecycle.ChangeRequest, error)
	// WithdrawDelegationChangeTx closes the open change request of a grant that
	// was revoked outside the request (a detach, a direct revoke).
	WithdrawDelegationChangeTx(ctx context.Context, tx pgx.Tx, projectID, grantID, actorID int64) error
}

// DelegationAccessResolver answers what a person holds access to in a project:
// the toolkits they may use and the credential references they hold. A grant
// approved by a change request lists only what every approver holds.
type DelegationAccessResolver interface {
	AccessFor(ctx context.Context, projectID, userID int64) (dg.Access, error)
}

// DelegationGrantsOption configures the store.
type DelegationGrantsOption func(*DelegationGrants)

// WithDelegationChanges composes the change request recorder. Without it a
// governed grant cannot be proposed.
func WithDelegationChanges(recorder DelegationChangeRecorder) DelegationGrantsOption {
	return func(s *DelegationGrants) { s.changes = recorder }
}

// WithDelegationAccess composes the approver access resolver. Without it a
// grant approved by a change request cannot be applied.
func WithDelegationAccess(resolver DelegationAccessResolver) DelegationGrantsOption {
	return func(s *DelegationGrants) { s.access = resolver }
}

// NewDelegationGrants builds the store. audit is required.
func NewDelegationGrants(pool *pgxpool.Pool, audit securityaudit.Appender, options ...DelegationGrantsOption) (*DelegationGrants, error) {
	if pool == nil {
		return nil, errors.New("delegation grants: a pool is required")
	}
	if audit == nil {
		return nil, errors.New("delegation grants: the security audit appender is required")
	}
	s := &DelegationGrants{pool: pool, audit: audit, now: time.Now}
	for _, option := range options {
		option(s)
	}
	return s, nil
}

// ProposeInput is a new grant for an edge: what the callee holds now.
type ProposeInput struct {
	ProjectID       int64
	Edge            dg.Edge
	CalleeVersionID int64
	Requested       dg.GrantSet
	Origin          dg.Origin
	// Legacy marks a grant the S6 migration writes for an existing composition.
	Legacy bool
	// Governed sets the maker-checker flag: the approver must differ from
	// ProposedBy, in code and in the table's CHECK.
	Governed bool
	// ProposedBy is the proposer; nil only for a migration.
	ProposedBy *int64
}

func (in ProposeInput) validate() error {
	switch {
	case in.ProjectID <= 0, in.Edge.CallerEntityID <= 0, in.Edge.CalleeEntityID <= 0, in.CalleeVersionID <= 0:
		return fmt.Errorf("%w: ids must be positive", dg.ErrInvalid)
	case in.Edge.CallerEntityID == in.Edge.CalleeEntityID:
		return fmt.Errorf("%w: an agent cannot delegate to itself", dg.ErrInvalid)
	case !in.Edge.Kind.Valid():
		return fmt.Errorf("%w: unknown edge kind %q", dg.ErrInvalid, in.Edge.Kind)
	case !in.Origin.Valid():
		return fmt.Errorf("%w: unknown origin %q", dg.ErrInvalid, in.Origin)
	case in.ProposedBy == nil && in.Origin != dg.OriginMigration:
		return fmt.Errorf("%w: a proposer is required", dg.ErrInvalid)
	case in.Governed && in.ProposedBy == nil:
		return fmt.Errorf("%w: a governed grant needs a proposer", dg.ErrInvalid)
	}
	if err := in.Requested.Validate(); err != nil {
		return fmt.Errorf("%w: %v", dg.ErrInvalid, err)
	}
	return nil
}

// lockEdge serialises the writers of one edge for the transaction. FOR UPDATE
// on the edge's row locks nothing when the edge has no row yet, so two
// concurrent proposals would both miss and one would fail on the unique
// index; the advisory lock makes the second wait and then find the first.
func lockEdge(ctx context.Context, tx pgx.Tx, schema string, edge dg.Edge) error {
	key := fmt.Sprintf("%s/%d/%d/%s", schema, edge.CallerEntityID, edge.CalleeEntityID, edge.Kind)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("delegation grants: lock the edge: %w", err)
	}
	return nil
}

// Propose records a pending grant for the edge. An edge that already has an
// active grant refuses with dg.ErrActiveEdgeExists, except when that grant
// records the same requested digest and is still pending: the attach path
// retries, and the existing grant is returned unchanged.
func (s *DelegationGrants) Propose(ctx context.Context, in ProposeInput) (dg.Grant, error) {
	var out dg.Grant
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		grant, err := s.ProposeTx(ctx, tx, in)
		out = grant
		return err
	})
	return out, err
}

// ProposeGrant is Propose with the proposal spelled out, for the application
// service (which holds no repos types).
func (s *DelegationGrants) ProposeGrant(ctx context.Context, projectID int64, edge dg.Edge, calleeVersionID int64, requested dg.GrantSet, origin dg.Origin, governed bool, proposedBy int64) (dg.Grant, error) {
	return s.Propose(ctx, ProposeInput{
		ProjectID: projectID, Edge: edge, CalleeVersionID: calleeVersionID, Requested: requested,
		Origin: origin, Governed: governed, ProposedBy: &proposedBy,
	})
}

// ProposeTx is Propose in the caller's transaction: the attach path proposes
// the grant in the transaction that writes the attachment, so a composition
// either has its grant or does not exist. A governed grant also records its
// change request (kind delegation) in the same transaction.
func (s *DelegationGrants) ProposeTx(ctx context.Context, tx pgx.Tx, in ProposeInput) (dg.Grant, error) {
	if err := in.validate(); err != nil {
		return dg.Grant{}, err
	}
	schema, err := tenantSchema(strconv.FormatInt(in.ProjectID, 10))
	if err != nil {
		return dg.Grant{}, err
	}
	if err := lockEdge(ctx, tx, schema, in.Edge); err != nil {
		return dg.Grant{}, err
	}
	existing, found, err := activeForEdge(ctx, tx, schema, in.Edge, true)
	if err != nil {
		return dg.Grant{}, err
	}
	if found {
		if existing.State == dg.StatePending && existing.RequestedDigest == in.Requested.Digest() &&
			existing.Governed == in.Governed && sameUser(existing.ProposedBy, in.ProposedBy) {
			return existing, nil
		}
		return dg.Grant{}, dg.ErrActiveEdgeExists
	}
	grant, err := s.insert(ctx, tx, schema, in, dg.StatePending, in.Requested, nil, nil)
	if err != nil {
		return dg.Grant{}, err
	}
	if err := s.append(ctx, tx, grantEvent(grant, dg.ActionProposed, proposerOf(grant))); err != nil {
		return dg.Grant{}, err
	}
	if grant.Governed {
		if s.changes == nil {
			return dg.Grant{}, errors.New("delegation grants: no change request recorder is composed for a governed grant")
		}
		if _, err := s.changes.RecordDelegationChangeTx(ctx, tx, agentlifecycle.NewDelegationChange{
			ProjectID: grant.ProjectID, ApplicationID: grant.Edge.CallerEntityID, GrantID: grant.ID,
			ProposedBy: proposerOf(grant),
			Summary:    fmt.Sprintf("Delegate from agent %d to %d (%s)", grant.Edge.CallerEntityID, grant.Edge.CalleeEntityID, grant.Edge.Kind),
		}); err != nil {
			return dg.Grant{}, err
		}
	}
	return grant, nil
}

// Approve approves a pending grant. choose lists the items the approver
// keeps (nil keeps all), and access is what the approver holds: the grant
// lists only the chosen items the approver holds access to, the rest is
// recorded as omitted. A governed grant refuses its own proposer.
func (s *DelegationGrants) Approve(ctx context.Context, projectID, id, approver int64, choose []dg.Item, access dg.Access) (dg.Grant, error) {
	if approver <= 0 || access == nil {
		return dg.Grant{}, fmt.Errorf("%w: an approver and their access are required", dg.ErrInvalid)
	}
	var out dg.Grant
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		grant, err := s.approveTx(ctx, tx, projectID, id, approver, choose, access)
		out = grant
		return err
	})
	return out, err
}

func (s *DelegationGrants) approveTx(ctx context.Context, tx pgx.Tx, projectID, id, approver int64, choose []dg.Item, access dg.Access) (dg.Grant, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.Grant{}, err
	}
	grant, err := lockGrant(ctx, tx, schema, projectID, id)
	if err != nil {
		return dg.Grant{}, err
	}
	if !dg.CanTransition(grant.State, dg.StateApproved) || grant.State != dg.StatePending {
		return dg.Grant{}, dg.ErrWrongState
	}
	if err := dg.CheckApprover(grant.Governed, grant.ProposedBy, approver); err != nil {
		return dg.Grant{}, err
	}
	approved, omitted, err := dg.Approvable(grant.RequestedSet, choose, access)
	if err != nil {
		return dg.Grant{}, err
	}
	omittedJSON, err := json.Marshal(nonNilItems(omitted))
	if err != nil {
		return dg.Grant{}, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.delegation_grants
SET state = 'approved', grant_set = $2, digest = $3, omitted = $4, approved_by = $5, approved_at = $6, updated_at = clock_timestamp()
WHERE id = $1`, schema), id, []byte(approved.Canonical()), approved.Digest(), omittedJSON, approver, s.now().UTC()); err != nil {
		return dg.Grant{}, mapDelegationGrantError(err)
	}
	grant, err = readGrant(ctx, tx, schema, id)
	if err != nil {
		return dg.Grant{}, err
	}
	return grant, s.append(ctx, tx, grantEvent(grant, dg.ActionApproved, approver, partialReason(omitted)...))
}

// ReapproveInput replaces an approved or stale grant with a new one for the
// callee's current set (A1.2 rule 3: re-approval keeps the old grant as
// superseded).
type ReapproveInput struct {
	ProjectID       int64
	GrantID         int64
	CalleeVersionID int64
	// Requested is what the callee holds now.
	Requested dg.GrantSet
	Choose    []dg.Item
	Access    dg.Access
	Origin    dg.Origin
	Governed  bool
	// ProposedBy asked for the re-approval, ApprovedBy gave it.
	ProposedBy *int64
	ApprovedBy int64
}

// Reapprove supersedes the grant and records a new approved grant for the same
// edge in one transaction.
func (s *DelegationGrants) Reapprove(ctx context.Context, in ReapproveInput) (dg.Grant, error) {
	switch {
	case in.ApprovedBy <= 0 || in.Access == nil:
		return dg.Grant{}, fmt.Errorf("%w: an approver and their access are required", dg.ErrInvalid)
	case in.CalleeVersionID <= 0 || !in.Origin.Valid():
		return dg.Grant{}, fmt.Errorf("%w: a callee version and an origin are required", dg.ErrInvalid)
	}
	if err := in.Requested.Validate(); err != nil {
		return dg.Grant{}, fmt.Errorf("%w: %v", dg.ErrInvalid, err)
	}
	schema, err := tenantSchema(strconv.FormatInt(in.ProjectID, 10))
	if err != nil {
		return dg.Grant{}, err
	}
	var out dg.Grant
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		old, err := lockGrant(ctx, tx, schema, in.ProjectID, in.GrantID)
		if err != nil {
			return err
		}
		if old.State != dg.StateApproved && old.State != dg.StateStale {
			return dg.ErrWrongState
		}
		// A governed edge stays governed: the caller cannot drop the flag
		// to approve its own re-grant.
		in.Governed = in.Governed || old.Governed
		if in.Governed && in.ProposedBy == nil {
			return fmt.Errorf("%w: a governed re-approval needs a proposer", dg.ErrInvalid)
		}
		if err := dg.CheckApprover(in.Governed, in.ProposedBy, in.ApprovedBy); err != nil {
			return err
		}
		approved, omitted, err := dg.Approvable(in.Requested, in.Choose, in.Access)
		if err != nil {
			return err
		}
		// Free the edge's active slot first, naming the row itself as its
		// successor until the new row exists (superseded_by must be set for
		// a superseded row).
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.delegation_grants SET state = 'superseded', superseded_by = id, updated_at = clock_timestamp() WHERE id = $1`, schema), old.ID); err != nil {
			return mapDelegationGrantError(err)
		}
		at := s.now().UTC()
		grant, err := s.insert(ctx, tx, schema, ProposeInput{
			ProjectID: in.ProjectID, Edge: old.Edge, CalleeVersionID: in.CalleeVersionID, Requested: in.Requested,
			Origin: in.Origin, Legacy: false, Governed: in.Governed, ProposedBy: in.ProposedBy,
		}, dg.StateApproved, approved, &in.ApprovedBy, &at)
		if err != nil {
			return err
		}
		omittedJSON, err := json.Marshal(nonNilItems(omitted))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.delegation_grants SET omitted = $2 WHERE id = $1`, schema), grant.ID, omittedJSON); err != nil {
			return mapDelegationGrantError(err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.delegation_grants SET superseded_by = $2 WHERE id = $1`, schema), old.ID, grant.ID); err != nil {
			return mapDelegationGrantError(err)
		}
		if grant, err = readGrant(ctx, tx, schema, grant.ID); err != nil {
			return err
		}
		out = grant
		old.SupersededBy = &grant.ID
		if err := s.append(ctx, tx, grantEvent(old, dg.ActionSuperseded, in.ApprovedBy)); err != nil {
			return err
		}
		return s.append(ctx, tx, grantEvent(grant, dg.ActionApproved, in.ApprovedBy, partialReason(omitted)...))
	})
	return out, err
}

// Revoke ends an active grant: a detach, a rejection or a withdrawal. A grant
// already revoked or superseded refuses with dg.ErrWrongState.
func (s *DelegationGrants) Revoke(ctx context.Context, projectID, id, actor int64) (dg.Grant, error) {
	var out dg.Grant
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		grant, err := s.RevokeTx(ctx, tx, projectID, id, actor)
		out = grant
		return err
	})
	return out, err
}

// RevokeTx is Revoke in the caller's transaction (a detach revokes the grant
// in the transaction that removes the attachment).
func (s *DelegationGrants) RevokeTx(ctx context.Context, tx pgx.Tx, projectID, id, actor int64) (dg.Grant, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.Grant{}, err
	}
	grant, err := lockGrant(ctx, tx, schema, projectID, id)
	if err != nil {
		return dg.Grant{}, err
	}
	if !grant.State.Active() {
		return dg.Grant{}, dg.ErrWrongState
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.delegation_grants SET state = 'revoked', revoked_by = $2, revoked_at = $3, updated_at = clock_timestamp() WHERE id = $1`, schema), id, actor, s.now().UTC()); err != nil {
		return dg.Grant{}, mapDelegationGrantError(err)
	}
	if grant, err = readGrant(ctx, tx, schema, id); err != nil {
		return dg.Grant{}, err
	}
	if grant.Governed && s.changes != nil {
		if err := s.changes.WithdrawDelegationChangeTx(ctx, tx, projectID, id, actor); err != nil {
			return dg.Grant{}, err
		}
	}
	return grant, s.append(ctx, tx, grantEvent(grant, dg.ActionRevoked, actor))
}

// RevokeEdgeTx revokes the active grant of an edge in the caller's
// transaction, if there is one, and reports whether it did.
func (s *DelegationGrants) RevokeEdgeTx(ctx context.Context, tx pgx.Tx, projectID int64, edge dg.Edge, actor int64) (bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return false, err
	}
	if err := lockEdge(ctx, tx, schema, edge); err != nil {
		return false, err
	}
	grant, found, err := activeForEdge(ctx, tx, schema, edge, false)
	if err != nil || !found {
		return false, err
	}
	if _, err := s.RevokeTx(ctx, tx, projectID, grant.ID, actor); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyDelegationTx approves, in the final approval's transaction, the grant
// of a delegation change request. The grant lists only what EVERY approver
// holds access to: an approver who lacks a toolkit or credential leaves it out
// (A1.2 rule 2), and the callee runs without it.
func (s *DelegationGrants) ApplyDelegationTx(ctx context.Context, tx pgx.Tx, apply agentlifecycle.DelegationApply) error {
	if s.access == nil {
		return errors.New("delegation grants: no approver access resolver is composed")
	}
	holders := make([]dg.Access, 0, len(apply.ApproverIDs))
	for _, approver := range apply.ApproverIDs {
		access, err := s.access.AccessFor(ctx, apply.ProjectID, approver)
		if err != nil {
			return fmt.Errorf("delegation grants: resolve the access of approver %d: %w", approver, err)
		}
		holders = append(holders, access)
	}
	_, err := s.approveTx(ctx, tx, apply.ProjectID, apply.GrantID, apply.FinalApproverID, nil, allHold(holders))
	return err
}

// RevokeDelegationTx revokes the grant of a rejected or withdrawn change
// request.
func (s *DelegationGrants) RevokeDelegationTx(ctx context.Context, tx pgx.Tx, projectID, grantID, actorID int64) error {
	_, err := s.RevokeTx(ctx, tx, projectID, grantID, actorID)
	return err
}

// allHold is the access every one of holders has: an item is held only when
// each of them holds it.
type allHold []dg.Access

func (a allHold) HoldsToolkit(key string) bool {
	for _, access := range a {
		if !access.HoldsToolkit(key) {
			return false
		}
	}
	return len(a) > 0
}

func (a allHold) HoldsCredential(reference string) bool {
	for _, access := range a {
		if !access.HoldsCredential(reference) {
			return false
		}
	}
	return len(a) > 0
}

// RevokeEdge revokes the active grant of an edge, if any (a detach). It
// reports whether one was revoked.
func (s *DelegationGrants) RevokeEdge(ctx context.Context, projectID int64, edge dg.Edge, actor int64) (bool, error) {
	// A re-approval can supersede the grant between the read and the revoke
	// (the edge then has a NEW active grant): read the edge again and revoke
	// that one, so a detach never leaves an approved grant behind.
	for range 3 {
		grant, found, err := s.ActiveForEdge(ctx, projectID, edge)
		if err != nil || !found {
			return false, err
		}
		_, err = s.Revoke(ctx, projectID, grant.ID, actor)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, dg.ErrWrongState) {
			return false, err
		}
	}
	return false, dg.ErrWrongState
}

// MarkCurrent moves a stale grant back to approved when the callee's current
// digest is the one the grant recorded again (the drift was undone). It
// reports whether the state changed.
func (s *DelegationGrants) MarkCurrent(ctx context.Context, projectID, id int64, currentDigest string) (dg.Grant, bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.Grant{}, false, err
	}
	var out dg.Grant
	changed := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		grant, err := lockGrant(ctx, tx, schema, projectID, id)
		if err != nil {
			return err
		}
		out = grant
		if grant.State != dg.StateStale || grant.Stale(currentDigest) || !dg.CanTransition(grant.State, dg.StateApproved) {
			return nil
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.delegation_grants SET state = 'approved', updated_at = clock_timestamp() WHERE id = $1`, schema), id); err != nil {
			return mapDelegationGrantError(err)
		}
		if out, err = readGrant(ctx, tx, schema, id); err != nil {
			return err
		}
		changed = true
		return s.append(ctx, tx, grantEvent(out, dg.ActionApproved, 0, "callee_restored"))
	})
	return out, changed, err
}

func sameUser(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// MarkStale moves an approved grant to stale when the callee's current digest
// differs from the one the grant recorded. It reports whether the state
// changed. A grant whose callee still matches, or that is not approved, is
// left alone.
func (s *DelegationGrants) MarkStale(ctx context.Context, projectID, id int64, currentDigest string) (dg.Grant, bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.Grant{}, false, err
	}
	var out dg.Grant
	changed := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		grant, err := lockGrant(ctx, tx, schema, projectID, id)
		if err != nil {
			return err
		}
		out = grant
		if !dg.CanTransition(grant.State, dg.StateStale) || grant.State != dg.StateApproved || !grant.Stale(currentDigest) {
			return nil
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.delegation_grants SET state = 'stale', updated_at = clock_timestamp() WHERE id = $1`, schema), id); err != nil {
			return mapDelegationGrantError(err)
		}
		if out, err = readGrant(ctx, tx, schema, id); err != nil {
			return err
		}
		changed = true
		return s.append(ctx, tx, grantEvent(out, dg.ActionStale, 0, "callee_drifted"))
	})
	return out, changed, err
}

// Get reads one grant of the project.
func (s *DelegationGrants) Get(ctx context.Context, projectID, id int64) (dg.Grant, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.Grant{}, err
	}
	return readGrant(ctx, s.pool, schema, id)
}

// ActiveForEdge reads the edge's active grant (pending, approved or stale).
func (s *DelegationGrants) ActiveForEdge(ctx context.Context, projectID int64, edge dg.Edge) (dg.Grant, bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.Grant{}, false, err
	}
	return activeForEdge(ctx, s.pool, schema, edge, false)
}

// ListForCaller lists every grant of the caller's edges, newest first, with
// the history (superseded and revoked) when includeHistory is set.
func (s *DelegationGrants) ListForCaller(ctx context.Context, projectID, callerEntityID int64, includeHistory bool) ([]dg.Grant, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT %s FROM %s.delegation_grants
WHERE caller_entity_id = $1 AND ($2 OR state IN ('pending', 'approved', 'stale'))
ORDER BY id DESC`, delegationGrantColumns, schema), callerEntityID, includeHistory)
	if err != nil {
		return nil, fmt.Errorf("delegation grants: list: %w", err)
	}
	defer rows.Close()
	grants := []dg.Grant{}
	for rows.Next() {
		grant, err := scanDelegationGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// ConsumableForCallee lists the approved and stale grants whose callee is one
// of calleeEntityIDs: the grants a run reads as G(Y to X) (story S2c).
func (s *DelegationGrants) ConsumableForCallee(ctx context.Context, projectID int64, calleeEntityIDs []int64) ([]dg.Grant, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT %s FROM %s.delegation_grants
WHERE callee_entity_id = ANY($1) AND state IN ('approved', 'stale')
ORDER BY caller_entity_id, callee_entity_id, edge_kind`, delegationGrantColumns, schema), calleeEntityIDs)
	if err != nil {
		return nil, fmt.Errorf("delegation grants: consumable: %w", err)
	}
	defer rows.Close()
	grants := []dg.Grant{}
	for rows.Next() {
		grant, err := scanDelegationGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

const delegationGrantColumns = `id, project_id, caller_entity_id, callee_entity_id, edge_kind, callee_version_id,
requested_set, requested_digest, grant_set, digest, omitted, state, legacy, governed, origin,
proposed_by, approved_by, approved_at, revoked_by, revoked_at, superseded_by, created_at, updated_at`

type grantQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *DelegationGrants) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delegation grants: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delegation grants: commit: %w", err)
	}
	return nil
}

func (s *DelegationGrants) insert(ctx context.Context, tx pgx.Tx, schema string, in ProposeInput, state dg.State, approved dg.GrantSet, approver *int64, approvedAt *time.Time) (dg.Grant, error) {
	var id int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.delegation_grants
 (project_id, caller_entity_id, callee_entity_id, edge_kind, callee_version_id, requested_set, requested_digest,
  grant_set, digest, state, legacy, governed, origin, proposed_by, approved_by, approved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING id`, schema),
		in.ProjectID, in.Edge.CallerEntityID, in.Edge.CalleeEntityID, string(in.Edge.Kind), in.CalleeVersionID,
		[]byte(in.Requested.Canonical()), in.Requested.Digest(), []byte(approved.Canonical()), approved.Digest(),
		string(state), in.Legacy, in.Governed, string(in.Origin), in.ProposedBy, approver, approvedAt).Scan(&id)
	if err != nil {
		return dg.Grant{}, mapDelegationGrantError(err)
	}
	return readGrant(ctx, tx, schema, id)
}

func (s *DelegationGrants) append(ctx context.Context, tx pgx.Tx, event securityaudit.SecurityEvent) error {
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("delegation grants: append the audit event %s: %w", event.Action, err)
	}
	return nil
}

func lockGrant(ctx context.Context, tx pgx.Tx, schema string, projectID, id int64) (dg.Grant, error) {
	grant, err := scanDelegationGrant(tx.QueryRow(ctx, fmt.Sprintf(`
SELECT %s FROM %s.delegation_grants WHERE id = $1 AND project_id = $2 FOR UPDATE`, delegationGrantColumns, schema), id, projectID))
	return grant, notFoundAsDomain(err)
}

func readGrant(ctx context.Context, q grantQuerier, schema string, id int64) (dg.Grant, error) {
	grant, err := scanDelegationGrant(q.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM %s.delegation_grants WHERE id = $1`, delegationGrantColumns, schema), id))
	return grant, notFoundAsDomain(err)
}

func activeForEdge(ctx context.Context, q grantQuerier, schema string, edge dg.Edge, lock bool) (dg.Grant, bool, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	grant, err := scanDelegationGrant(q.QueryRow(ctx, fmt.Sprintf(`
SELECT %s FROM %s.delegation_grants
WHERE caller_entity_id = $1 AND callee_entity_id = $2 AND edge_kind = $3 AND state IN ('pending', 'approved', 'stale')%s`,
		delegationGrantColumns, schema, suffix), edge.CallerEntityID, edge.CalleeEntityID, string(edge.Kind)))
	if errors.Is(err, pgx.ErrNoRows) {
		return dg.Grant{}, false, nil
	}
	if err != nil {
		return dg.Grant{}, false, err
	}
	return grant, true, nil
}

func scanDelegationGrant(row pgx.Row) (dg.Grant, error) {
	var (
		g                                               dg.Grant
		kind, state, origin                             string
		requested, approved, omitted                    []byte
		proposedBy, approvedBy, revokedBy, supersededBy *int64
		approvedAt, revokedAt                           *time.Time
		calleeVersion                                   int32
		caller, callee                                  int32
	)
	if err := row.Scan(&g.ID, &g.ProjectID, &caller, &callee, &kind, &calleeVersion,
		&requested, &g.RequestedDigest, &approved, &g.Digest, &omitted, &state, &g.Legacy, &g.Governed, &origin,
		&proposedBy, &approvedBy, &approvedAt, &revokedBy, &revokedAt, &supersededBy, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return dg.Grant{}, err
	}
	var err error
	if g.RequestedSet, err = dg.ParseGrantSet(requested); err != nil {
		return dg.Grant{}, err
	}
	if g.GrantSet, err = dg.ParseGrantSet(approved); err != nil {
		return dg.Grant{}, err
	}
	// The digest columns are the digests of the stored sets: a row edited by
	// hand, or written by a path that forgot one, must not reach the worker.
	if g.RequestedSet.Digest() != g.RequestedDigest || g.GrantSet.Digest() != g.Digest {
		return dg.Grant{}, fmt.Errorf("delegation grants: grant %d: a digest column does not match its set", g.ID)
	}
	g.Omitted = []dg.Item{}
	if err := json.Unmarshal(omitted, &g.Omitted); err != nil {
		return dg.Grant{}, fmt.Errorf("delegation grants: omitted: %w", err)
	}
	g.Edge = dg.Edge{CallerEntityID: int64(caller), CalleeEntityID: int64(callee), Kind: dg.EdgeKind(kind)}
	g.CalleeVersionID = int64(calleeVersion)
	g.State, g.Origin = dg.State(state), dg.Origin(origin)
	g.ProposedBy, g.ApprovedBy, g.RevokedBy, g.SupersededBy = proposedBy, approvedBy, revokedBy, supersededBy
	g.ApprovedAt, g.RevokedAt = approvedAt, revokedAt
	return g, nil
}

func notFoundAsDomain(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return dg.ErrNotFound
	}
	return err
}

// mapDelegationGrantError turns the table's constraint violations into the
// domain's refusals, so the CHECK is a refusal and not a 500 when it fires.
func mapDelegationGrantError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.ConstraintName {
	case "delegation_grants_active_edge_key":
		return dg.ErrActiveEdgeExists
	case "delegation_grants_approver_not_proposer":
		return dg.ErrSelfApproval
	}
	return err
}

func nonNilItems(items []dg.Item) []dg.Item {
	if items == nil {
		return []dg.Item{}
	}
	return items
}

func partialReason(omitted []dg.Item) []string {
	if len(omitted) == 0 {
		return nil
	}
	return []string{"partial_approval"}
}

func proposerOf(grant dg.Grant) int64 {
	if grant.ProposedBy == nil {
		return 0
	}
	return *grant.ProposedBy
}

// grantEvent builds the audit event of one grant change. The resource is the
// edge (caller:callee:kind) and the grant id.
func grantEvent(grant dg.Grant, action string, actorID int64, reasons ...string) securityaudit.SecurityEvent {
	principal := "system:delegation_grants"
	if actorID > 0 {
		principal = "user:" + strconv.FormatInt(actorID, 10)
	}
	reasons = append([]string{"edge_" + string(grant.Edge.Kind)}, reasons...)
	if grant.Legacy {
		reasons = append(reasons, "legacy")
	}
	return securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentLifecycle, TenantProjectID: grant.ProjectID, Action: action,
		Reasons: reasons, PrincipalRef: principal, ResourceType: "delegation_grant",
		ResourceID: strconv.FormatInt(grant.Edge.CallerEntityID, 10) + ":" + strconv.FormatInt(grant.Edge.CalleeEntityID, 10) +
			":" + string(grant.Edge.Kind) + ":" + strconv.FormatInt(grant.ID, 10),
		ArgumentsDigest: "sha256:" + grant.Digest,
	}
}
