package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// Drift detection of delegation grants (ADR-0044 amendment 1, A1.2 rule 3;
// delegation epic 2156 story S4).
//
// The detector (application/delegationdrift) reads what a callee holds now and
// asks dg.Assess what to do. This file applies the answer in one transaction:
// the state change, the recorded drift, the audit event and, once per edge and
// digest, the notification to the caller's owners and editors. Nothing here
// changes what runs: the worker already narrows a stale grant's callee to the
// grant by set containment.

// maxEditorVersions bounds how many recent versions of the caller are read for
// their authors (the people who edit it).
const maxEditorVersions = 25

// ListTracked lists the approved and stale grants of the project with their
// recorded drift and closure. calleeIDs narrows the list to grants into those
// callees; nil lists every grant of the project. A tenant schema without the
// table answers none.
func (s *DelegationGrants) ListTracked(ctx context.Context, projectID int64, calleeIDs []int64, callerID int64) ([]dg.Tracked, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT %s, closure FROM %s.delegation_grants
WHERE state IN ('approved', 'stale') AND ($1::int[] IS NULL OR callee_entity_id = ANY($1))
  AND ($2::int = 0 OR caller_entity_id = $2)
ORDER BY caller_entity_id, callee_entity_id, edge_kind`, delegationGrantColumns, schema), driftCalleeFilter(calleeIDs), int32(callerID)) //nolint:gosec // application ids are int4 rows
	if err != nil {
		if undefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("delegation grants: list tracked: %w", err)
	}
	defer rows.Close()
	var out []dg.Tracked
	for rows.Next() {
		var closure []byte
		grant, err := scanDelegationGrantExtra(rows, &closure)
		if err != nil {
			return nil, err
		}
		tracked := dg.Tracked{Grant: grant, Drift: grant.Drift}
		if len(closure) > 0 {
			tracked.Closure, err = parseClosure(closure)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, tracked)
	}
	return out, rows.Err()
}

func driftCalleeFilter(ids []int64) []int32 {
	if len(ids) == 0 {
		return nil
	}
	out := make([]int32, 0, len(ids))
	for _, id := range ids {
		out = append(out, int32(id)) //nolint:gosec // application ids are int4 rows
	}
	return out
}

func parseClosure(raw []byte) (dg.Closure, error) {
	decoded := map[string]string{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("delegation grants: closure: %w", err)
	}
	closure := make(dg.Closure, len(decoded))
	for key, digest := range decoded {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("delegation grants: closure: %w", err)
		}
		closure[id] = digest
	}
	return closure, nil
}

func encodeClosure(closure dg.Closure) ([]byte, error) {
	encoded := make(map[string]string, len(closure))
	for id, digest := range closure {
		encoded[strconv.FormatInt(id, 10)] = digest
	}
	return json.Marshal(encoded)
}

// ApplyDrift applies a drift verdict and the closure just read to the grant, in
// one transaction. The grant is read again under its lock: a grant that is no
// longer in the state the verdict was made for (it was revoked, superseded or
// re-approved meanwhile) is left alone.
func (s *DelegationGrants) ApplyDrift(ctx context.Context, projectID, grantID int64, expect dg.State, verdict dg.Verdict, closure dg.Closure) (dg.DriftApplied, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.DriftApplied{}, err
	}
	var out dg.DriftApplied
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		grant, err := lockGrant(ctx, tx, schema, projectID, grantID)
		if err != nil {
			return err
		}
		out.Grant = grant
		if grant.State != expect {
			return nil
		}
		var storedClosure []byte
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT closure FROM %s.delegation_grants WHERE id = $1`, schema), grantID).Scan(&storedClosure); err != nil {
			return fmt.Errorf("delegation grants: read the closure: %w", err)
		}
		var before dg.Closure
		if len(storedClosure) > 0 {
			if before, err = parseClosure(storedClosure); err != nil {
				return err
			}
		}
		if err := s.applyVerdict(ctx, tx, schema, &out, verdict); err != nil {
			return err
		}
		return s.applyClosure(ctx, tx, schema, &out, before, closure)
	})
	return out, err
}

func (s *DelegationGrants) applyVerdict(ctx context.Context, tx pgx.Tx, schema string, out *dg.DriftApplied, verdict dg.Verdict) error {
	grant := out.Grant
	switch verdict.Action {
	case dg.DriftMarkStale, dg.DriftUpdate:
		drift, err := json.Marshal(verdict.Drift)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.delegation_grants SET state = 'stale', drift = $2, updated_at = clock_timestamp() WHERE id = $1`, schema), grant.ID, drift); err != nil {
			return mapDelegationGrantError(err)
		}
		updated, err := readGrant(ctx, tx, schema, grant.ID)
		if err != nil {
			return err
		}
		out.Grant, out.Changed = updated, true
		action := dg.ActionStale
		if verdict.Action == dg.DriftUpdate {
			action = dg.ActionDriftRecorded
		}
		if err := s.append(ctx, tx, grantEvent(updated, action, 0, "callee_drifted")); err != nil {
			return err
		}
		out.NotifiedStale, err = s.notifyDrift(ctx, tx, schema, updated, "stale", verdict.Drift.Digest, driftMessage(agentName(ctx, tx, schema, updated.Edge.CallerEntityID), agentName(ctx, tx, schema, updated.Edge.CalleeEntityID), verdict.Drift))
		return err
	case dg.DriftRestore:
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.delegation_grants SET state = 'approved', drift = NULL, updated_at = clock_timestamp() WHERE id = $1`, schema), grant.ID); err != nil {
			return mapDelegationGrantError(err)
		}
		updated, err := readGrant(ctx, tx, schema, grant.ID)
		if err != nil {
			return err
		}
		out.Grant, out.Changed = updated, true
		// A drift that comes back after a restore is a new event: forget what was
		// announced for this edge.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
DELETE FROM %s.delegation_drift_notices
WHERE caller_entity_id = $1 AND callee_entity_id = $2 AND edge_kind = $3 AND notice_kind = 'stale'`, schema),
			updated.Edge.CallerEntityID, updated.Edge.CalleeEntityID, string(updated.Edge.Kind)); err != nil {
			return fmt.Errorf("delegation grants: forget the notices: %w", err)
		}
		return s.append(ctx, tx, grantEvent(updated, dg.ActionApproved, 0, "callee_restored"))
	}
	return nil
}

func (s *DelegationGrants) applyClosure(ctx context.Context, tx pgx.Tx, schema string, out *dg.DriftApplied, before, closure dg.Closure) error {
	if closure == nil {
		return nil
	}
	if closure.Equal(before) {
		return nil
	}
	encoded, err := encodeClosure(closure)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.delegation_grants SET closure = $2 WHERE id = $1`, schema), out.Grant.ID, encoded); err != nil {
		return mapDelegationGrantError(err)
	}
	out.Changed = true
	if !closure.Grew(before) {
		return nil
	}
	message := fmt.Sprintf("The delegation from %s to %s now reaches more agents. Nothing changed for what %s may use, and each new hop still needs its own approval.",
		agentName(ctx, tx, schema, out.Grant.Edge.CallerEntityID), agentName(ctx, tx, schema, out.Grant.Edge.CalleeEntityID),
		agentName(ctx, tx, schema, out.Grant.Edge.CallerEntityID))
	out.NotifiedClosure, err = s.notifyDrift(ctx, tx, schema, out.Grant, "closure", closure.Key(), message)
	return err
}

// notifyDrift tells the caller's owners and editors once per edge and digest:
// the notice row is the dedup record, so a repeated check, a second writer or
// a notification deleted from the bell never repeats it. It answers how many
// people were told.
func (s *DelegationGrants) notifyDrift(ctx context.Context, tx pgx.Tx, schema string, grant dg.Grant, kind, digest, message string) (int, error) {
	people, err := callerEditors(ctx, tx, schema, grant)
	if err != nil {
		return 0, err
	}
	if len(people) == 0 {
		return 0, nil // nobody to tell: leave the digest unannounced rather than record it as told
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.delegation_drift_notices (caller_entity_id, callee_entity_id, edge_kind, notice_kind, digest)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, schema),
		grant.Edge.CallerEntityID, grant.Edge.CalleeEntityID, string(grant.Edge.Kind), kind, digest)
	if err != nil {
		return 0, fmt.Errorf("delegation grants: record the notice: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, nil
	}
	eventType := dg.NotifyStale
	if kind == "closure" {
		eventType = dg.NotifyClosureGrew
	}
	meta := map[string]any{
		"message": message, "project_id": grant.ProjectID, "grant_id": grant.ID,
		"caller_application_id": grant.Edge.CallerEntityID, "callee_application_id": grant.Edge.CalleeEntityID,
		"edge_kind": string(grant.Edge.Kind), "digest": digest,
	}
	if grant.Drift != nil && kind == "stale" {
		meta["added"] = grant.Drift.Added
	}
	for _, person := range people {
		key := fmt.Sprintf("delegation-%s-%d-%d-%d-%s-%s-%d", kind, grant.ProjectID,
			grant.Edge.CallerEntityID, grant.Edge.CalleeEntityID, grant.Edge.Kind, digest, person)
		if err := marketplace.Notify(ctx, tx, key, grant.ProjectID, person, eventType, meta); err != nil {
			return 0, err
		}
	}
	return len(people), nil
}

// callerEditors lists the people to tell about an edge: the caller's business
// and technical owners (its registry row), the people who proposed and
// approved the edge's grant, and the authors of the caller's recent versions,
// who are the people that edit it. Each once, in a stable order.
func callerEditors(ctx context.Context, tx pgx.Tx, schema string, grant dg.Grant) ([]int64, error) {
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range marketplace.OwnersOf(ctx, tx, grant.ProjectID, grant.Edge.CallerEntityID, 0) {
		add(id)
	}
	for _, id := range []*int64{grant.ProposedBy, grant.ApprovedBy} {
		if id != nil {
			add(*id)
		}
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT author_id FROM (
    SELECT author_id FROM %s.application_versions WHERE application_id = $1 ORDER BY id DESC LIMIT %d
) AS recent ORDER BY author_id`, schema, maxEditorVersions), grant.Edge.CallerEntityID)
	if err != nil {
		return nil, fmt.Errorf("delegation grants: read the editors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		add(id)
	}
	return out, rows.Err()
}

func agentName(ctx context.Context, q grantQuerier, schema string, applicationID int64) string {
	var name string
	if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s.applications WHERE id = $1`, schema), applicationID).Scan(&name); err != nil || name == "" {
		return "agent " + strconv.FormatInt(applicationID, 10)
	}
	return name
}

func driftMessage(caller, callee string, drift dg.Drift) string {
	what := ""
	for index, item := range drift.Added {
		if index == 3 {
			what += fmt.Sprintf(" and %d more", len(drift.Added)-3)
			break
		}
		if index > 0 {
			what += ", "
		}
		what += item.Name
	}
	return fmt.Sprintf("Re-approval needed: %s now holds more than %s approved for it (%s). "+
		"It runs without the new items until an editor of %s re-approves the delegation.", callee, caller, what, caller)
}
