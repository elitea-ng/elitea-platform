package repos

// The connector approval queue (ADR-0044 decision 1, #1501): the store of
// centry.connector_requests (shared 0207), the approval that writes the
// toolkit-type grants and the worker egress allowlist entries in ONE
// transaction, and the approved-connector read the catalogue gate uses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ConnectorRequests is the store.
type ConnectorRequests struct {
	pool     *pgxpool.Pool
	audit    securityaudit.Appender
	settings connectorapproval.SettingsReader
	now      func() time.Time
}

var _ connectorapproval.GrantReader = (*ConnectorRequests)(nil)

// NewConnectorRequests builds the store. settings reads the catalogue
// section (the attestation requirement) on every approval.
func NewConnectorRequests(pool *pgxpool.Pool, audit securityaudit.Appender, settings connectorapproval.SettingsReader) (*ConnectorRequests, error) {
	if pool == nil || audit == nil || settings == nil {
		return nil, errors.New("connector requests: the database, the audit appender and the settings reader are required")
	}
	return &ConnectorRequests{pool: pool, audit: audit, settings: settings, now: time.Now}, nil
}

// ComposeConnectorRequests builds the store the way the process uses it: the
// security audit outbox of elitea-main and the catalogue section read on
// every call.
func ComposeConnectorRequests(pool *pgxpool.Pool) (*ConnectorRequests, error) {
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, err
	}
	return NewConnectorRequests(pool, appender, func(ctx context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.LoadCatalogueSettings(ctx, pool)
	})
}

const connectorRequestColumns = `id, project_id, target_kind, target, toolkit_type, platform_wide, project_ids,
declared_tools, egress_hosts, data_classes, justification, state, requested_by, decided_by,
decision_reason, attestation_id, created_at, updated_at, submitted_at, decided_at`

func scanConnectorRequest(row pgx.Row) (connectorapproval.Request, error) {
	var request connectorapproval.Request
	var kind, state string
	var tools []byte
	var decidedBy, attestationID *int64
	if err := row.Scan(&request.ID, &request.ProjectID, &kind, &request.Target, &request.ToolkitType,
		&request.PlatformWide, &request.ProjectIDs, &tools, &request.EgressHosts, &request.DataClasses,
		&request.Justification, &state, &request.RequestedBy, &decidedBy, &request.DecisionReason,
		&attestationID, &request.CreatedAt, &request.UpdatedAt, &request.SubmittedAt, &request.DecidedAt); err != nil {
		return connectorapproval.Request{}, err
	}
	request.TargetKind = connectorapproval.TargetKind(kind)
	request.State = connectorapproval.State(state)
	if decidedBy != nil {
		request.DecidedBy = *decidedBy
	}
	if attestationID != nil {
		request.AttestationID = *attestationID
	}
	if len(tools) > 0 {
		if err := json.Unmarshal(tools, &request.Tools); err != nil {
			return connectorapproval.Request{}, fmt.Errorf("connector requests: decode the declared tools: %w", err)
		}
	}
	if request.Tools == nil {
		request.Tools = []connectorapproval.DeclaredTool{}
	}
	if request.ProjectIDs == nil {
		request.ProjectIDs = []int64{}
	}
	if request.EgressHosts == nil {
		request.EgressHosts = []string{}
	}
	if request.DataClasses == nil {
		request.DataClasses = []string{}
	}
	return request, nil
}

func connectorEvent(request connectorapproval.Request, action string, actorID int64, reasons ...string) securityaudit.SecurityEvent {
	principal := "system:connector_approval"
	if actorID > 0 {
		principal = "user:" + strconv.FormatInt(actorID, 10)
	}
	out := []string{"kind:" + string(request.TargetKind)}
	if request.PlatformWide {
		out = append(out, "scope:platform")
	} else {
		for _, id := range request.ProjectIDs {
			out = append(out, "project:"+strconv.FormatInt(id, 10))
		}
	}
	if typeReason := "toolkit_type:" + strings.ToLower(request.ToolkitType); auditReason(typeReason) {
		out = append(out, typeReason)
	}
	for _, reason := range reasons {
		if auditReason(reason) {
			out = append(out, reason)
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return securityaudit.SecurityEvent{
		Type: securityaudit.TypeConnectorApproval, TenantProjectID: request.ProjectID, Action: action,
		Reasons: out, PrincipalRef: principal, ResourceType: "connector_request",
		ResourceID: strconv.FormatInt(request.ID, 10),
	}
}

// auditReason keeps only what the audit reason grammar admits.
func auditReason(reason string) bool {
	if reason == "" || len(reason) > 128 {
		return false
	}
	for index, r := range reason {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			(index > 0 && (r == '_' || r == '.' || r == ':' || r == '-'))
		if !ok {
			return false
		}
	}
	return true
}

func (s *ConnectorRequests) append(ctx context.Context, tx pgx.Tx, event securityaudit.SecurityEvent) error {
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("connector requests: append the audit event %s: %w", event.Action, err)
	}
	return nil
}

func (s *ConnectorRequests) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("connector requests: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("connector requests: commit: %w", err)
	}
	return nil
}

// Create stores a new request in the requesting project, as a draft or (with
// submit) submitted at once. Every named project must exist.
func (s *ConnectorRequests) Create(ctx context.Context, projectID, requesterID int64, draft connectorapproval.Draft, submit bool) (connectorapproval.Request, error) {
	if projectID <= 0 || requesterID <= 0 {
		return connectorapproval.Request{}, connectorapproval.Refuse(connectorapproval.InvalidCode, "A connector request needs a project and a signed-in requester.")
	}
	if !draft.PlatformWide && len(draft.ProjectIDs) == 0 {
		// A request names its own project unless it says otherwise.
		draft.ProjectIDs = []int64{projectID}
	}
	normalized, toolkitType, err := connectorapproval.NormalizeDraft(draft)
	if err != nil {
		return connectorapproval.Request{}, err
	}
	tools, err := json.Marshal(normalized.Tools)
	if err != nil {
		return connectorapproval.Request{}, err
	}
	var created connectorapproval.Request
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if len(normalized.ProjectIDs) > 0 {
			var missing []int64
			rows, err := tx.Query(ctx, `
SELECT wanted.id FROM unnest($1::bigint[]) AS wanted(id)
WHERE NOT EXISTS (SELECT 1 FROM centry.project AS p WHERE p.id = wanted.id)`, normalized.ProjectIDs)
			if err != nil {
				return fmt.Errorf("connector requests: check the projects: %w", err)
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				missing = append(missing, id)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(missing) > 0 {
				return &connectorapproval.Refusal{Code: connectorapproval.InvalidCode,
					Message: "A named project does not exist.", Detail: map[string]any{"missing_project_ids": missing}}
			}
		}
		state := connectorapproval.StateDraft
		var submittedAt any
		if submit {
			state = connectorapproval.StateSubmitted
			submittedAt = s.now()
		}
		created, err = scanConnectorRequest(tx.QueryRow(ctx, `
INSERT INTO centry.connector_requests
    (project_id, target_kind, target, toolkit_type, platform_wide, project_ids, declared_tools,
     egress_hosts, data_classes, justification, state, requested_by, submitted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING `+connectorRequestColumns,
			projectID, string(normalized.TargetKind), normalized.Target, toolkitType, normalized.PlatformWide,
			normalized.ProjectIDs, tools, normalized.EgressHosts, normalized.DataClasses, normalized.Justification,
			string(state), requesterID, submittedAt))
		if err != nil {
			return fmt.Errorf("connector requests: create: %w", err)
		}
		if err := s.append(ctx, tx, connectorEvent(created, connectorapproval.ActionCreated, requesterID)); err != nil {
			return err
		}
		if submit {
			return s.append(ctx, tx, connectorEvent(created, connectorapproval.ActionSubmitted, requesterID))
		}
		return nil
	})
	return created, err
}

// Get reads one request. projectID > 0 scopes the read to the requesting
// project (the project routes); 0 reads any (the admin routes).
func (s *ConnectorRequests) Get(ctx context.Context, projectID, id int64) (connectorapproval.Request, error) {
	return readConnectorRequest(ctx, s.pool, projectID, id, false)
}

type connectorQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func readConnectorRequest(ctx context.Context, q connectorQuerier, projectID, id int64, lock bool) (connectorapproval.Request, error) {
	query := `SELECT ` + connectorRequestColumns + ` FROM centry.connector_requests WHERE id = $1 AND ($2 = 0 OR project_id = $2)`
	if lock {
		query += ` FOR UPDATE`
	}
	request, err := scanConnectorRequest(q.QueryRow(ctx, query, id, projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return connectorapproval.Request{}, connectorapproval.Refuse(connectorapproval.NotFoundCode, "The connector request does not exist.")
	}
	if err != nil {
		return connectorapproval.Request{}, fmt.Errorf("connector requests: read: %w", err)
	}
	return request, nil
}

// ConnectorRequestFilter narrows a listing.
type ConnectorRequestFilter struct {
	// ProjectID > 0 lists the requests a project made.
	ProjectID int64
	State     connectorapproval.State
	Limit     int
}

// List lists requests, newest first.
func (s *ConnectorRequests) List(ctx context.Context, filter ConnectorRequestFilter) ([]connectorapproval.Request, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT `+connectorRequestColumns+` FROM centry.connector_requests
WHERE ($1 = 0 OR project_id = $1) AND ($2 = '' OR state = $2)
ORDER BY id DESC LIMIT $3`, filter.ProjectID, string(filter.State), limit)
	if err != nil {
		return nil, fmt.Errorf("connector requests: list: %w", err)
	}
	defer rows.Close()
	out := []connectorapproval.Request{}
	for rows.Next() {
		request, err := scanConnectorRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, request)
	}
	return out, rows.Err()
}

// Submit moves the requester's draft to submitted.
func (s *ConnectorRequests) Submit(ctx context.Context, projectID, id, actorID int64) (connectorapproval.Request, error) {
	var updated connectorapproval.Request
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		request, err := readConnectorRequest(ctx, tx, projectID, id, true)
		if err != nil {
			return err
		}
		if request.RequestedBy != actorID {
			return connectorapproval.Refuse(connectorapproval.RequesterOnlyCode, "Only the requester submits a connector request.")
		}
		if request.State != connectorapproval.StateDraft {
			return connectorapproval.Refuse(connectorapproval.StateConflictCode, "Only a draft is submitted.")
		}
		updated, err = scanConnectorRequest(tx.QueryRow(ctx, `
UPDATE centry.connector_requests SET state = 'submitted', submitted_at = $2, updated_at = $2
WHERE id = $1 RETURNING `+connectorRequestColumns, id, s.now()))
		if err != nil {
			return fmt.Errorf("connector requests: submit: %w", err)
		}
		return s.append(ctx, tx, connectorEvent(updated, connectorapproval.ActionSubmitted, actorID))
	})
	return updated, err
}

// Withdraw closes the requester's draft or submitted request.
func (s *ConnectorRequests) Withdraw(ctx context.Context, projectID, id, actorID int64, reason string) (connectorapproval.Request, error) {
	reason, err := connectorapproval.ValidateDecisionReason(reason, false)
	if err != nil {
		return connectorapproval.Request{}, err
	}
	var updated connectorapproval.Request
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		request, err := readConnectorRequest(ctx, tx, projectID, id, true)
		if err != nil {
			return err
		}
		if request.RequestedBy != actorID {
			return connectorapproval.Refuse(connectorapproval.RequesterOnlyCode, "Only the requester withdraws a connector request.")
		}
		if request.State != connectorapproval.StateDraft && request.State != connectorapproval.StateSubmitted {
			return connectorapproval.Refuse(connectorapproval.StateConflictCode, "Only a draft or a submitted request is withdrawn.")
		}
		updated, err = scanConnectorRequest(tx.QueryRow(ctx, `
UPDATE centry.connector_requests SET state = 'withdrawn', decision_reason = $2, updated_at = $3
WHERE id = $1 RETURNING `+connectorRequestColumns, id, reason, s.now()))
		if err != nil {
			return fmt.Errorf("connector requests: withdraw: %w", err)
		}
		return s.append(ctx, tx, connectorEvent(updated, connectorapproval.ActionWithdrawn, actorID))
	})
	return updated, err
}

// Reject closes a submitted request. The approver differs from the
// requester.
func (s *ConnectorRequests) Reject(ctx context.Context, id, approverID int64, reason string) (connectorapproval.Request, error) {
	reason, err := connectorapproval.ValidateDecisionReason(reason, true)
	if err != nil {
		return connectorapproval.Request{}, err
	}
	var updated connectorapproval.Request
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		request, err := readConnectorRequest(ctx, tx, 0, id, true)
		if err != nil {
			return err
		}
		if err := decidable(request, approverID); err != nil {
			return err
		}
		updated, err = scanConnectorRequest(tx.QueryRow(ctx, `
UPDATE centry.connector_requests
SET state = 'rejected', decided_by = $2, decided_at = $3, decision_reason = $4, updated_at = $3
WHERE id = $1 RETURNING `+connectorRequestColumns, id, approverID, s.now(), reason))
		if err != nil {
			return fmt.Errorf("connector requests: reject: %w", err)
		}
		return s.append(ctx, tx, connectorEvent(updated, connectorapproval.ActionRejected, approverID))
	})
	return updated, err
}

func decidable(request connectorapproval.Request, approverID int64) error {
	if approverID <= 0 {
		return connectorapproval.Refuse(connectorapproval.InvalidCode, "A decision needs a signed-in approver.")
	}
	if request.RequestedBy == approverID {
		return connectorapproval.Refuse(connectorapproval.SelfApprovalCode,
			"The requester cannot decide their own connector request: another connector approver must.")
	}
	if request.State != connectorapproval.StateSubmitted {
		return connectorapproval.Refuse(connectorapproval.StateConflictCode, "Only a submitted request is approved or rejected.")
	}
	return nil
}

// ConnectorApprovalOutcome is what an approval wrote.
type ConnectorApprovalOutcome struct {
	Request connectorapproval.Request
	// GrantedProjects are the projects that got a toolkit-type grant row;
	// empty for a platform-wide approval (the policy row is `enabled`).
	GrantedProjects []int64
	// EgressHosts are the hosts added to the allowlists (the platform's for
	// a platform-wide approval, else each project's).
	EgressHosts []string
}

// Approve approves a submitted request in ONE transaction: the state, the
// toolkit-type grants (centry.toolkit_type_policy and
// toolkit_type_project_grant), the worker egress allowlist entries (the
// platform section, or each project's worker_egress setting) and the audit
// event. Any failure writes none of it.
func (s *ConnectorRequests) Approve(ctx context.Context, id, approverID int64, reason string) (ConnectorApprovalOutcome, error) {
	reason, err := connectorapproval.ValidateDecisionReason(reason, false)
	if err != nil {
		return ConnectorApprovalOutcome{}, err
	}
	settings, err := s.settings(ctx)
	if err != nil {
		return ConnectorApprovalOutcome{}, fmt.Errorf("connector requests: read the catalogue settings: %w", err)
	}
	var outcome ConnectorApprovalOutcome
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		request, err := readConnectorRequest(ctx, tx, 0, id, true)
		if err != nil {
			return err
		}
		if err := decidable(request, approverID); err != nil {
			return err
		}
		var attestationID any
		if settings.RequireArchitectureAttestation {
			found, err := validArchitectureAttestation(ctx, tx, request, s.now())
			if err != nil {
				return err
			}
			if found == 0 {
				return &connectorapproval.Refusal{Code: connectorapproval.AttestationMissingCode,
					Message: "The platform requires a valid architecture attestation of this request before approval. " +
						"Record one in the requesting project (subject connector, the request id).",
					Detail: map[string]any{"missing": []string{"architecture"}}}
			}
			attestationID = found
		}
		decision := "Connector request #" + strconv.FormatInt(request.ID, 10) + " approved"
		granted, err := writeToolkitGrants(ctx, tx, request, approverID, decision)
		if err != nil {
			return err
		}
		hosts := append([]string(nil), request.EgressHosts...)
		if host, ok := connectorapproval.HostOfTarget(request.TargetKind, request.Target); ok {
			hosts = append(hosts, host)
		}
		hosts = dedupeSorted(hosts)
		if err := writeEgressEntries(ctx, tx, request, hosts, approverID); err != nil {
			return err
		}
		updated, err := scanConnectorRequest(tx.QueryRow(ctx, `
UPDATE centry.connector_requests
SET state = 'approved', decided_by = $2, decided_at = $3, decision_reason = $4, attestation_id = $5, updated_at = $3
WHERE id = $1 RETURNING `+connectorRequestColumns, id, approverID, s.now(), reason, attestationID))
		if err != nil {
			return fmt.Errorf("connector requests: approve: %w", err)
		}
		reasons := make([]string, 0, len(hosts))
		for _, host := range hosts {
			reasons = append(reasons, "egress:"+host)
		}
		if err := s.append(ctx, tx, connectorEvent(updated, connectorapproval.ActionApproved, approverID, reasons...)); err != nil {
			return err
		}
		outcome = ConnectorApprovalOutcome{Request: updated, GrantedProjects: granted, EgressHosts: hosts}
		return nil
	})
	return outcome, err
}

func validArchitectureAttestation(ctx context.Context, tx pgx.Tx, request connectorapproval.Request, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
SELECT id FROM elitea_lifecycle.attestations
WHERE tenant_project_id = $1 AND kind = 'architecture' AND subject_type = 'connector' AND subject_id = $2
  AND revoked_at IS NULL AND (valid_until IS NULL OR valid_until > $3)
ORDER BY id DESC LIMIT 1`, request.ProjectID, strconv.FormatInt(request.ID, 10), now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("connector requests: read the attestations: %w", err)
	}
	return id, nil
}

// writeToolkitGrants makes the request's toolkit type available where it
// asks, in tx. A platform-wide approval sets the deployment policy to
// `enabled`. A project approval keeps the deployment policy (inserting
// `enabled`, the default, when there is none, because a grant needs a
// policy row) and adds an `enabled` grant per project. A `disabled` policy
// is the operator's refusal: the approval is refused rather than overruling
// it.
func writeToolkitGrants(ctx context.Context, tx pgx.Tx, request connectorapproval.Request, approverID int64, reason string) ([]int64, error) {
	decider := "user:" + strconv.FormatInt(approverID, 10)
	var availability string
	err := tx.QueryRow(ctx, `SELECT availability FROM centry.toolkit_type_policy WHERE toolkit_type = $1 FOR UPDATE`,
		request.ToolkitType).Scan(&availability)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("connector requests: read the toolkit type policy: %w", err)
	}
	if availability == string(toolkitcatalogue.AvailabilityDisabled) {
		return nil, connectorapproval.Refuse(connectorapproval.TypeDisabledCode, fmt.Sprintf(
			"The deployment's toolkit policy disables %q. Change it under Admin, Toolkits before approving.", request.ToolkitType))
	}
	if request.PlatformWide {
		if availability != string(toolkitcatalogue.AvailabilityEnabled) {
			if _, err := tx.Exec(ctx, `
INSERT INTO centry.toolkit_type_policy (toolkit_type, availability, reason, decided_by, decided_at)
VALUES ($1, 'enabled', $2, $3, clock_timestamp())
ON CONFLICT (toolkit_type) DO UPDATE
   SET availability = 'enabled', reason = EXCLUDED.reason, decided_by = EXCLUDED.decided_by, decided_at = EXCLUDED.decided_at`,
				request.ToolkitType, reason, decider); err != nil {
				return nil, fmt.Errorf("connector requests: write the toolkit type policy: %w", err)
			}
		}
		return nil, nil
	}
	if availability == "" {
		if _, err := tx.Exec(ctx, `
INSERT INTO centry.toolkit_type_policy (toolkit_type, availability, reason, decided_by, decided_at)
VALUES ($1, 'enabled', $2, $3, clock_timestamp())`, request.ToolkitType, reason, decider); err != nil {
			return nil, fmt.Errorf("connector requests: write the toolkit type policy: %w", err)
		}
	}
	for _, projectID := range request.ProjectIDs {
		if _, err := tx.Exec(ctx, `
INSERT INTO centry.toolkit_type_project_grant (toolkit_type, project_id, availability, reason, granted_by, granted_at)
VALUES ($1, $2, 'enabled', $3, $4, clock_timestamp())
ON CONFLICT (toolkit_type, project_id) DO UPDATE
   SET availability = 'enabled', reason = EXCLUDED.reason, granted_by = EXCLUDED.granted_by, granted_at = EXCLUDED.granted_at`,
			request.ToolkitType, projectID, reason, decider); err != nil {
			return nil, fmt.Errorf("connector requests: write the toolkit type grant: %w", err)
		}
	}
	return append([]int64(nil), request.ProjectIDs...), nil
}

// writeEgressEntries adds hosts to the platform worker egress allowlist (a
// platform-wide request) or to each named project's worker_egress setting,
// in tx. Entries already present are kept once; a list that would pass its
// bound refuses the approval.
func writeEgressEntries(ctx context.Context, tx pgx.Tx, request connectorapproval.Request, hosts []string, approverID int64) error {
	if len(hosts) == 0 {
		return nil
	}
	if request.PlatformWide {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('centry.platform_config') IS NOT NULL`).Scan(&exists); err != nil {
			return fmt.Errorf("connector requests: find the platform configuration: %w", err)
		}
		if !exists {
			return connectorapproval.Refuse(connectorapproval.StateConflictCode,
				"The platform configuration table is missing, so the egress hosts cannot be recorded.")
		}
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT value FROM centry.platform_config WHERE section = $1 AND key = $2 FOR UPDATE`,
			platformconfig.SectionWorkerEgress, platformconfig.KeyWorkerEgressAllowlist).Scan(&raw)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("connector requests: read the platform allowlist: %w", err)
		}
		current, err := decodeAllowlist(raw)
		if err != nil {
			return connectorapproval.Refuse(connectorapproval.StateConflictCode,
				"The platform worker egress allowlist does not decode; correct it under Configuration, Worker Egress first.")
		}
		merged, changed := mergeAllowlist(current, hosts)
		if !changed {
			return nil
		}
		if len(merged) > platformconfig.MaxWorkerEgressPlatformEntries {
			return connectorapproval.Refuse(connectorapproval.EgressLimitCode, fmt.Sprintf(
				"The platform worker egress allowlist would hold more than %d entries.", platformconfig.MaxWorkerEgressPlatformEntries))
		}
		encoded, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC', $4)
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
			platformconfig.SectionWorkerEgress, platformconfig.KeyWorkerEgressAllowlist, string(encoded),
			"connector-request:"+strconv.FormatInt(request.ID, 10)); err != nil {
			return fmt.Errorf("connector requests: write the platform allowlist: %w", err)
		}
		return nil
	}
	for _, projectID := range request.ProjectIDs {
		if err := writeProjectEgressEntries(ctx, tx, projectID, hosts, approverID); err != nil {
			return err
		}
	}
	return nil
}

func writeProjectEgressEntries(ctx context.Context, tx pgx.Tx, projectID int64, hosts []string, approverID int64) error {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil || projectID > math.MaxInt32 {
		return connectorapproval.Refuse(connectorapproval.InvalidCode, "A named project is not a valid project.")
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".configuration").Scan(&exists); err != nil {
		return fmt.Errorf("connector requests: find the project configuration: %w", err)
	}
	if !exists {
		return &connectorapproval.Refusal{Code: connectorapproval.StateConflictCode,
			Message: "A named project has no configuration store, so its egress hosts cannot be recorded.",
			Detail:  map[string]any{"project_id": projectID}}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "worker_egress_setting:"+schema); err != nil {
		return fmt.Errorf("connector requests: lock the project setting: %w", err)
	}
	var rowID int64
	var raw []byte
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT id, data FROM %s.configuration WHERE type = $1 ORDER BY id LIMIT 1 FOR UPDATE`, schema),
		configurationapp.WorkerEgressConfigurationType).Scan(&rowID, &raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("connector requests: read the project allowlist: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if len(hosts) > platformconfig.MaxWorkerEgressProjectEntries {
			return connectorapproval.Refuse(connectorapproval.EgressLimitCode, fmt.Sprintf(
				"A project's worker egress allowlist would hold more than %d entries.", platformconfig.MaxWorkerEgressProjectEntries))
		}
		data, err := json.Marshal(map[string]any{"allowlist": hosts})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.configuration (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source, author_id, created_at)
VALUES (gen_random_uuid(), $1, 'Worker egress', $2 || '_' || substr(md5(random()::text || clock_timestamp()::text), 1, 12),
        $2, $3, $4::jsonb, '{}'::jsonb, false, true, 'user', $5, now() AT TIME ZONE 'UTC')`, schema),
			projectID, configurationapp.WorkerEgressConfigurationType, configurationapp.WorkerEgressSection,
			string(data), approverID); err != nil {
			return fmt.Errorf("connector requests: create the project allowlist: %w", err)
		}
		return nil
	}
	var setting map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &setting); err != nil {
			return &connectorapproval.Refusal{Code: connectorapproval.StateConflictCode,
				Message: "A named project's worker egress setting does not decode; re-save it first.",
				Detail:  map[string]any{"project_id": projectID}}
		}
	}
	if setting == nil {
		setting = map[string]any{}
	}
	listRaw, _ := json.Marshal(setting["allowlist"])
	current, err := decodeAllowlist(listRaw)
	if err != nil {
		return &connectorapproval.Refusal{Code: connectorapproval.StateConflictCode,
			Message: "A named project's worker egress allowlist does not decode; re-save it first.",
			Detail:  map[string]any{"project_id": projectID}}
	}
	merged, changed := mergeAllowlist(current, hosts)
	if !changed {
		return nil
	}
	if len(merged) > platformconfig.MaxWorkerEgressProjectEntries {
		return &connectorapproval.Refusal{Code: connectorapproval.EgressLimitCode, Message: fmt.Sprintf(
			"A project's worker egress allowlist would hold more than %d entries.", platformconfig.MaxWorkerEgressProjectEntries),
			Detail: map[string]any{"project_id": projectID}}
	}
	setting["allowlist"] = merged
	data, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.configuration SET data = $2::jsonb, updated_at = now() AT TIME ZONE 'UTC' WHERE id = $1`, schema),
		rowID, string(data)); err != nil {
		return fmt.Errorf("connector requests: write the project allowlist: %w", err)
	}
	return nil
}

func decodeAllowlist(raw []byte) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// mergeAllowlist appends the hosts a list lacks, keeping its order.
func mergeAllowlist(current, hosts []string) ([]string, bool) {
	present := map[string]bool{}
	merged := make([]string, 0, len(current)+len(hosts))
	for _, entry := range current {
		present[entry] = true
		merged = append(merged, entry)
	}
	changed := false
	for _, host := range hosts {
		if !present[host] {
			present[host] = true
			merged = append(merged, host)
			changed = true
		}
	}
	return merged, changed
}

func dedupeSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// ApprovedConnectors answers the approved requests naming the project or
// the whole platform (the catalogue gate's GrantReader). A database without
// the table has none.
func (s *ConnectorRequests) ApprovedConnectors(ctx context.Context, projectID int64) ([]connectorapproval.ApprovedConnector, error) {
	rows, err := s.pool.Query(ctx, `
SELECT target_kind, target, declared_tools FROM centry.connector_requests
WHERE state = 'approved' AND (platform_wide OR $1 = ANY (project_ids))`, projectID)
	if undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connector requests: read the approved connectors: %w", err)
	}
	defer rows.Close()
	var out []connectorapproval.ApprovedConnector
	for rows.Next() {
		var kind, target string
		var tools []byte
		if err := rows.Scan(&kind, &target, &tools); err != nil {
			return nil, err
		}
		connector := connectorapproval.ApprovedConnector{TargetKind: connectorapproval.TargetKind(kind), Target: target}
		if len(tools) > 0 {
			if err := json.Unmarshal(tools, &connector.Tools); err != nil {
				return nil, fmt.Errorf("connector requests: decode the approved tools: %w", err)
			}
		}
		out = append(out, connector)
	}
	return out, rows.Err()
}

// ConnectorRequestUser is a requester's display name and email.
type ConnectorRequestUser struct {
	Name  string
	Email string
}

// UserNames answers the names and emails of users, for the review page. A
// database without the user table answers none.
func (s *ConnectorRequests) UserNames(ctx context.Context, ids []int64) (map[int64]ConnectorRequestUser, error) {
	out := map[int64]ConnectorRequestUser{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(name, ''), COALESCE(email, '') FROM public.auth_core__user WHERE id = ANY($1::bigint[])`, ids)
	if undefinedTable(err) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("connector requests: read the requesters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var user ConnectorRequestUser
		if err := rows.Scan(&id, &user.Name, &user.Email); err != nil {
			return out, err
		}
		out[id] = user
	}
	return out, rows.Err()
}

// ComposeConnectorGate builds the store and the catalogue gate over it the
// way the process uses them: the catalogue section read on every decision,
// the approved requests read per project, and governed (the process's
// credential governance profile) forcing approved_only. A nil pool answers
// nil for both: the routes answer 503 and the gate is open, as a
// composition with no database always was.
func ComposeConnectorGate(pool *pgxpool.Pool, governed bool) (*connectorapproval.Gate, *ConnectorRequests, error) {
	if pool == nil {
		return nil, nil, nil
	}
	store, err := ComposeConnectorRequests(pool)
	if err != nil {
		return nil, nil, err
	}
	gate, err := connectorapproval.NewGate(store.settings, store, governed)
	if err != nil {
		return nil, nil, err
	}
	return gate, store, nil
}
