// Self-service "request a project" (issue #871).
//
//	POST /admin/moderation_status/project_request        — file a request
//	GET  /admin/moderation_status/project_requests/mine   — my own requests
//	PUT  /admin/moderation_status/administration           — approve/reject
//	                                                          (requests.go,
//	                                                          branches here
//	                                                          on issue_type)
//
// ## Why this is centry.moderation_state with a new issue_type, not a new table
//
// Every project today is admin-created: `POST /projects/project/{mode}`
// (internal/api/v2/projects/create.go) gates on CreateProjectPermission,
// resolved in `administration` mode. This adds the missing member-facing
// half — a request/approval flow — reusing the SAME table the "Request
// Access" app-catalogue button and the "Request a model connection" dialog
// already write to (requests.go's file header explains that table's shape
// and security boundary; both apply here unchanged). One column earns its
// keep twice over: `issue_type = "Project Request"` is both what the admin
// queue's existing filter already discriminates on and the ONLY thing that
// makes this decision path provision anything — see decideProjectRequest.
//
// `entity_id` carries the requested project's NAME (no path-segment
// encoding needed: unlike the catalogue's per-entity routes, this request's
// name travels in the JSON body, decoded once, stored once).
// `description` carries the requester's justification, matching the shape
// every other moderation request already uses (name/target + one free-text
// field) rather than inventing a "description vs. justification" split the
// table has no second column for.
//
// ## Where `project_id` comes from with no project to scope it to
//
// The four existing routes are project-scoped: a member asks their own
// project's operator for something that project lacks. A project REQUEST
// has no home project yet. `project_id` is NOT NULL
// (migrations/001_initial.sql:239) and carries no FK, so any positive
// integer satisfies the column — this uses the requester's own PERSONAL
// project, resolved (and provisioned if it does not exist yet — #609) via
// `PersonalProjectEnsurer.Ensure`, the same seam `internal/api/router.go`
// wires OIDC/SAML login through. It is a bookkeeping value here, read by
// nothing except the admin queue's own project filter; it is never read
// back to decide what the request means.
//
// ## Approval is NOT clerical here, unlike every other moderation request
//
// `RequestModelConnection.tsx`'s doc comment and
// `TestApprovingAModelConnectionProvisionsNothing` both pin that an
// ordinary decision changes only `status` and sends a notification. A
// Project Request breaks that pattern on purpose: the issue asks for
// approval to actually create the project, with the requester as its admin,
// through the SAME pipeline `POST /projects/project/administration` uses
// (`projectprovisioning.Provisioner.Provision`) — vault, tenant schema,
// membership, the lot. decideProjectRequest is where that happens, gated on
// the row still being `pending` under a row lock so two concurrent
// approvals cannot provision the project twice.
package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
)

// ProjectRequestIssueType is the `issue_type` that marks a
// `centry.moderation_state` row as a project request rather than an
// ordinary app-access or model-connection request. It is what
// AdministrationRequestUpdate branches on to decide whether approving a row
// provisions a project — see this file's header.
const ProjectRequestIssueType = "Project Request"

// ProjectProvisioner is the narrow seam onto
// internal/application/projectprovisioning's real pipeline — the same one
// `internal/api/v2/projects.Handler` uses for admin-initiated creation.
// Declared here, at the consumer, so this package does not import
// internal/api/v2/projects and does not need a database to unit-test the
// gate around it.
type ProjectProvisioner interface {
	Provision(ctx context.Context, request projectprovisioning.Request) (projectprovisioning.Result, error)
}

// PersonalProjectEnsurer resolves (and provisions, if it does not exist yet)
// the given user's personal project id. Satisfied by
// internal/application/personalproject.Ensurer.
type PersonalProjectEnsurer interface {
	Ensure(ctx context.Context, userID int64) (int64, error)
}

// WithProjectProvisioner supplies the pipeline an APPROVED Project Request
// runs through. Without it, approving one fails closed with a 502 rather
// than silently recording an approval that created nothing — see
// decideProjectRequest.
func WithProjectProvisioner(provisioner ProjectProvisioner) Option {
	return func(h *Handler) {
		if provisioner != nil {
			h.provisioner = provisioner
		}
	}
}

// WithPersonalProjectEnsurer supplies the resolver CreateProjectRequest uses
// for the new row's `project_id`. Without it, filing a project request
// answers 503 rather than guessing a project id.
func WithPersonalProjectEnsurer(ensurer PersonalProjectEnsurer) Option {
	return func(h *Handler) {
		if ensurer != nil {
			h.personalProjects = ensurer
		}
	}
}

// errRequestAlreadyDecided reports that a decision arrived for a row that is
// no longer `pending` — see decideProjectRequest's row lock.
var errRequestAlreadyDecided = errors.New("moderation: request already decided")

// errProjectProvisioningFailed wraps whatever projectprovisioning.Provision
// returned, so AdministrationRequestUpdate can answer 502 rather than 500
// without this package depending on the provisioner's own error values.
var errProjectProvisioningFailed = errors.New("moderation: project provisioning failed")

/* ── file / list (the requester's half) ──────────────────────────────── */

// projectRequestCreateBody is what a requester may send. There is no
// `status`/`user_id` field to refuse here (unlike requestCreateBody):
// this route has no `{mode}/{projectID}/{entityID}` path for a body to
// disagree with, so a client attempting either simply has nowhere to put it.
type projectRequestCreateBody struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func projectRequestFields(body projectRequestCreateBody) (name, description string, err error) {
	if body.Name == nil {
		return "", "", errors.New("name is required")
	}
	name = strings.TrimSpace(*body.Name)
	if name == "" {
		return "", "", errors.New("name is required")
	}
	if body.Description == nil {
		return "", "", errors.New("description is required")
	}
	description = strings.TrimSpace(*body.Description)
	if description == "" {
		return "", "", errors.New("description is required")
	}
	return name, description, nil
}

// CreateProjectRequest serves `POST /admin/moderation_status/project_request`.
//
// Gated on nothing beyond the platform's own authentication middleware
// (internal/api/router.go mounts this route with no permission wrapper): the
// issue asks that ANY authenticated user may request a project, the same way
// any authenticated user may ask an operator for app access today.
func (h *Handler) CreateProjectRequest(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if h.personalProjects == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}

	userID, err := requesterID(r)
	if err != nil {
		writeModerationError(w, http.StatusForbidden, err.Error())
		return
	}

	var body projectRequestCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeModerationError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, description, err := projectRequestFields(body)
	if err != nil {
		writeModerationError(w, http.StatusBadRequest, err.Error())
		return
	}

	// `Ensure` answers (0, nil) — not an error — for "not eligible" and for
	// "another process is provisioning this account's personal project right
	// now, poll again" (its own doc comment). Both are refused here rather
	// than let a `0` become this row's `project_id`: the column is NOT NULL,
	// but nothing stops a plain 0 from satisfying that, and a row scoped to
	// project 0 would be invisible to the admin queue's project filter and
	// meaningless everywhere else that reads it.
	projectID, err := h.personalProjects.Ensure(r.Context(), userID)
	if err != nil || projectID <= 0 {
		writeModerationError(w, http.StatusInternalServerError, "failed to resolve your personal project")
		return
	}

	var row requestRow
	if err := h.pool.QueryRow(r.Context(), `
INSERT INTO centry.moderation_state (user_id, project_id, issue_type, entity_id, description, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_id, project_id, issue_type, COALESCE(entity_id, ''), description,
          status, rejection_comment, created_at, updated_at`,
		userID, projectID, ProjectRequestIssueType, name, description, statusPending,
	).Scan(&row.ID, &row.UserID, &row.ProjectID, &row.IssueType, &row.EntityID,
		&row.Description, &row.Status, &row.RejectionComment, &row.CreatedAt, &row.UpdatedAt); err != nil {
		writeModerationError(w, http.StatusInternalServerError, "failed to create the project request")
		return
	}

	writeModerationJSON(w, http.StatusCreated, row)
}

// MyProjectRequests serves `GET /admin/moderation_status/project_requests/mine`.
//
// "What have I asked for" — every Project Request row the caller has ever
// filed, across every project_id it happens to carry (the four existing
// routes scope to ONE project + entity pair; a project request has no
// natural project to scope the read to, so this scopes to the caller alone,
// which is also the more useful answer: "did my earlier request go
// anywhere" should not depend on remembering which project it recorded
// itself under).
func (h *Handler) MyProjectRequests(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	userID, err := requesterID(r)
	if err != nil {
		writeModerationError(w, http.StatusForbidden, err.Error())
		return
	}

	rows, err := scanRequests(r.Context(), h.queryFunc(), requestColumns+`
WHERE m.user_id = $1 AND m.issue_type = $2
ORDER BY m.created_at DESC, m.id DESC`, userID, ProjectRequestIssueType)
	if err != nil {
		writeModerationError(w, http.StatusInternalServerError, "failed to read your project requests")
		return
	}
	writeModerationJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

/* ── decide (the operator's half) ────────────────────────────────────── */

// lookupIssueType reads the one immutable field AdministrationRequestUpdate
// needs to route a decision, without taking the row lock
// decideProjectRequest needs for the write itself.
func (h *Handler) lookupIssueType(ctx context.Context, id int64) (string, error) {
	var issueType string
	err := h.pool.QueryRow(ctx, `SELECT issue_type FROM centry.moderation_state WHERE id = $1`, id).
		Scan(&issueType)
	return issueType, err
}

// decideProjectRequest is applyDecision's counterpart for a Project Request
// row: same status/rejection_comment/notification contract, plus — only on
// approval — the actual project.
//
// ONE transaction, and the row is locked (`FOR UPDATE`) for its entire
// length, held open across the `Provision` call: a second concurrent
// decision on the same id blocks at the lock, not at a stale `pending` read,
// so it is impossible for two operators (or one operator's doubled click)
// to provision the project twice. The lock is released only at COMMIT or
// ROLLBACK, both of which happen after Provision returns.
//
// Provisioning happens BEFORE the status UPDATE, and an error there rolls
// the transaction back — a failed Provision leaves the row exactly
// `pending`, so an operator sees an ordinary undecided request to retry
// (or reject), never an "approved" row with no project behind it.
func (h *Handler) decideProjectRequest(
	ctx context.Context, id int64, status string, comment *string,
) (*requestRow, error) {
	if status == statusApproved && h.provisioner == nil {
		return nil, fmt.Errorf("%w: no project provisioner is configured", errProjectProvisioningFailed)
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin project request decision: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var row requestRow
	if err := tx.QueryRow(ctx, `
SELECT m.id, m.user_id, COALESCE(u.email, ''), m.project_id, m.issue_type,
       COALESCE(m.entity_id, ''), m.description, m.status, m.rejection_comment,
       m.created_at, m.updated_at
FROM centry.moderation_state m
LEFT JOIN public.auth_core__user u ON u.id = m.user_id
WHERE m.id = $1
FOR UPDATE OF m`, id,
	).Scan(&row.ID, &row.UserID, &row.UserEmail, &row.ProjectID, &row.IssueType, &row.EntityID,
		&row.Description, &row.Status, &row.RejectionComment, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return nil, err // pgx.ErrNoRows included, unwrapped for the caller's errors.Is
	}
	if row.Status != statusPending {
		return nil, errRequestAlreadyDecided
	}

	var metaJSON []byte
	if status == statusApproved {
		result, err := h.provisioner.Provision(ctx, projectprovisioning.Request{
			Name:       row.EntityID,
			OwnerID:    row.UserID,
			AdminRoles: []string{"admin"},
			Limits:     projectprovisioning.DefaultLimits(),
		})
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errProjectProvisioningFailed, err) //nolint:errorlint // deliberate: %w wraps the sentinel, the raw message stays readable
		}
		createdProjectID := result.ProjectID
		row.CreatedProjectID = &createdProjectID
		encoded, err := json.Marshal(map[string]any{"created_project_id": createdProjectID})
		if err != nil {
			return nil, fmt.Errorf("encode project request meta: %w", err)
		}
		metaJSON = encoded
	}

	if err := tx.QueryRow(ctx, `
UPDATE centry.moderation_state
SET status = $1, rejection_comment = $2, meta = COALESCE($3::jsonb, meta), updated_at = NOW()
WHERE id = $4
RETURNING id, user_id, project_id, issue_type, COALESCE(entity_id, ''), description,
          status, rejection_comment, created_at, updated_at`,
		status, comment, nullableJSON(metaJSON), id,
	).Scan(&row.ID, &row.UserID, &row.ProjectID, &row.IssueType, &row.EntityID,
		&row.Description, &row.Status, &row.RejectionComment, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return nil, err
	}

	if err := insertDecisionNotification(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit project request decision: %w", err)
	}
	return &row, nil
}

// nullableJSON turns an empty/nil encode result into a real SQL NULL rather
// than an empty byte slice — pgx sends a non-nil zero-length []byte as an
// empty bytea/string, which `” ::jsonb` fails to parse, where `NULL::jsonb`
// is simply NULL and COALESCE($3::jsonb, meta) falls through to the existing
// value exactly as a rejection (which never sets metaJSON) needs it to.
func nullableJSON(encoded []byte) any {
	if len(encoded) == 0 {
		return nil
	}
	return encoded
}

// projectRequestDecisionMessage is decisionMessage's Project-Request branch.
func projectRequestDecisionMessage(row requestRow) string {
	if row.Status == statusApproved {
		return fmt.Sprintf(
			"Your request to create the project %q has been approved. It is ready to use.",
			row.EntityID,
		)
	}
	message := fmt.Sprintf("Your request to create the project %q has been rejected.", row.EntityID)
	if row.RejectionComment != nil && *row.RejectionComment != "" {
		message += " Reason: " + *row.RejectionComment
	}
	return message
}
