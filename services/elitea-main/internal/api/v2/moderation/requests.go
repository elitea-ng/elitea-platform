package moderation

// The admin App Requests surface, plus the product-side path that FILLS it.
//
//	GET  /admin/moderation_statuses/administration            — the request queue
//	PUT  /admin/moderation_status/administration              — approve / reject
//	GET    /admin/moderation_status/{mode}/{projectID}/{entityID} — my own requests
//	POST   /admin/moderation_status/{mode}/{projectID}/{entityID} — raise a request
//	DELETE /admin/moderation_status/{mode}/{projectID}/{entityID} — withdraw mine
//
// ## What these rows ARE
//
// One row of `centry.moderation_state` is a user asking an operator for access
// to something the catalogue offers but their project is not configured for.
// `entity_id` names the catalogue entry, `issue_type` carries the label the
// requesting client displayed for it, `description` is the user's own
// justification, and `status` moves once, from `pending` to `approved` or
// `rejected`.
//
// ## This subsystem is LIVE, and it is live in THIS repository
//
// Worth stating because the obvious guess is the opposite. `moderation_state`
// is not the prompt/application PUBLISH moderation — that is a different
// mechanism entirely (`PublishStatus.on_moderation` on application VERSIONS,
// with its own `prompt_moderation_*` events), and the two never touch. This
// table is generic access requests, and the client that creates them ships in
// this repository: `apps/elitea-web/src/features/apps/` renders a "Request
// Access" button on every catalogue card the project cannot configure, and
// posts here.
//
// ## What was here before unit A14
//
// Three of the four routes above did not exist, and the fourth was worse than
// missing.
//
//   - `GET /admin/moderation_statuses/{mode}` was mounted, UNGATED, on a
//     `_ *http.Request` stub returning a fixed `{"rows":[],"total":0}`. The
//     admin queue was permanently empty and could not be told apart from a
//     platform nobody has ever asked anything of.
//   - `PUT /admin/moderation_status/administration` had no route at all. There
//     was no way to approve or reject anything.
//   - `admin.Handler.ModerationStatusSingle` was a second copy of the same stub,
//     mounted on NO route — dead code with no caller, the shape #126/#129/#134/
//     #136/#138/#149 keep producing.
//   - `GET|POST /admin/moderation_status/{mode}/{projectID}/{entityID}` answered
//     `{"status":"approved"}`. Unconditionally, to every caller, for every
//     entity, whether or not a request had ever been made — and the POST created
//     nothing. That is a gate that always says yes: the product asks "has this
//     been approved for me?" and the server answers yes to everyone, while the
//     button that would create the request writes to nowhere. Both are replaced
//     here, because leaving a fail-open answer beside a real table would be
//     worse than either alone.
//
// No migration in this repository created the table either, so even the query
// these handlers should have been running had nothing to run against.
//
// ## What a decision CAUSES
//
// Approving does not itself grant anything, here or in pylon: nothing in either
// stack reads an approved row and unlocks a capability, and the catalogue card's
// real gate is whether a toolkit schema exists. What a decision does is record
// the operator's answer and TELL the requester — legacy fires a
// `notifications_stream` event, which lands as a `centry.notifications` row and
// an SSE push. That row is written here, in the same transaction as the status
// change, so a decision cannot be recorded without the requester learning of it.
//
// This is stated rather than assumed because it decides what the page may claim.
// The approve and reject controls say "record a decision and notify the user",
// which is what they do; neither is dressed as provisioning.
//
// ## The security boundary: nobody writes the other side's fields
//
// A moderation row has two authors and they are not equally trusted.
//
// The REQUESTER supplies `issue_type` and `description` and nothing else.
// `status` is the one that matters: pylon's `ModerationStateCreate` declares
// `status: ModerationStatus = PENDING`, which is a DEFAULT and not a
// restriction — a body carrying `"status": "approved"` is accepted verbatim, so
// any project member holding `admin.moderation.create` can file a request that
// is already approved and land it in the operator's queue as decided. That is
// self-approval, and `createRequest` below refuses any status but `pending`
// rather than silently downgrading it: a client that believes it set a status
// and got a 201 is the failure this unit exists to stop. `user_id` is refused
// for the same reason in the other direction — pylon accepts the field and then
// ignores it, so a caller cannot tell that authorship (and therefore who the
// decision notification is delivered to) is not theirs to choose. It is taken
// from the authenticated principal, and a token principal is not accepted as an
// author at all.
//
// The MODERATOR supplies `status` and `rejection_comment` and nothing else. The
// record of WHAT was asked must not be editable by the person answering it: if
// the decision endpoint could rewrite `entity_id`, `issue_type` or
// `description`, an approved row would no longer be evidence of what was
// approved. `meta` is refused on both sides — pylon lets either party write raw
// JSONB, and the admin PUT REPLACES rather than merges it, so an operator's
// decision silently destroys whatever the requester stored. Nothing in either
// stack reads a key out of it, so refusing a non-empty `meta` costs no
// legitimate-user outcome; an empty one is tolerated because both existing
// clients send `{}` and refusing that would break them for nothing.
//
// This narrowing is deliberate under AGENTS.md's "compatibility never requires
// preserving a vulnerability".
//
// ## Two pylon behaviours corrected rather than reproduced
//
//   - `rejection_comment` is REQUIRED when rejecting. pylon declares a
//     `@field_validator` for it, but a pydantic v2 field validator does not run
//     when the field is absent from the payload, so `{"id":5,"status":"rejected"}`
//     validates and rejects with a null reason. The rule is enforced here where
//     it cannot be sidestepped by omission.
//   - Searching the queue matches the user's email and name IN SQL. pylon calls
//     `auth.list_users_paginated(limit=1000)` and filters on the ids it gets
//     back, so on a deployment with more than a thousand users the queue's
//     search box silently cannot find most of them — and when nothing matched it
//     applied `filter(False)`, which is the right answer reached by a route that
//     stops being right at user 1001.
//
// ## Authorisation
//
// Gated in internal/api/router.go on the permissions the pylon handlers declare
// (legacy/plugins/admin/api/v2/moderation_status*.py): `admin.moderation` for
// the queue, `admin.moderation.edit` for the decision, `admin.moderation.view`
// and `admin.moderation.create` for the two project-scoped routes. The first
// two resolve CENTRALLY — a platform operator answering requests from every
// tenant is not a member of the projects they arrive from — and the last two
// resolve against the caller's membership of `{projectID}`.
//
// The per-entity read is additionally scoped to the CALLER's own rows, in SQL
// and not in the client, matching pylon: the endpoint answers "what have I
// asked for", never "what has anyone asked for". The per-entity DELETE carries
// the same scope, and takes `admin.moderation.create`: a person who may file a
// request may withdraw it. See RequestDelete.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The three legal values of `status`. pylon's `ModerationStatus` enum, which is
// the only place the vocabulary is enforced there — the column is a plain
// VARCHAR(64) in both schemas.
const (
	statusPending  = "pending"
	statusApproved = "approved"
	statusRejected = "rejected"
)

// requestRow is one `centry.moderation_state` row as both listings return it.
//
// `user_email` is the one field that is not a column: pylon resolves it per page
// through the auth service and attaches it, and the admin queue's "Requesting
// User" column reads it. Here it is a LEFT JOIN, so a request whose author has
// since been deleted still lists — with an empty address rather than vanishing.
type requestRow struct {
	ID               int64     `json:"id"`
	UserID           int64     `json:"user_id"`
	UserEmail        string    `json:"user_email"`
	ProjectID        int64     `json:"project_id"`
	IssueType        string    `json:"issue_type"`
	EntityID         string    `json:"entity_id"`
	Description      string    `json:"description"`
	Status           string    `json:"status"`
	RejectionComment *string   `json:"rejection_comment"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// CreatedProjectID is set ONLY by decideProjectRequest (project_requests.go),
	// on an APPROVED Project Request row — the id `projectprovisioning
	// .Provision` returned, kept in memory rather than round-tripped through
	// `meta` for the response. Every other decision path leaves it nil, which
	// `omitempty` keeps out of the JSON body entirely rather than serialising
	// a null every existing caller has never seen.
	CreatedProjectID *int64 `json:"created_project_id,omitempty"`
}

// sortableRequestColumns is both the sort allow-list and what keeps the ORDER BY
// injection-free, since the value is interpolated. pylon resolves `sort_by` with
// `getattr` on the model and emits NO ordering at all when the attribute is
// missing, so an unknown column there means an unordered page — which under
// LIMIT/OFFSET repeats and drops rows while paging. An unknown column here falls
// back to `created_at`.
var sortableRequestColumns = map[string]string{
	"created_at": "m.created_at",
	"updated_at": "m.updated_at",
	"status":     "m.status",
	"entity_id":  "m.entity_id",
	"issue_type": "m.issue_type",
	"project_id": "m.project_id",
	"id":         "m.id",
}

const requestColumns = `
SELECT m.id, m.user_id, COALESCE(u.email, ''), m.project_id, m.issue_type,
       COALESCE(m.entity_id, ''), m.description, m.status, m.rejection_comment,
       m.created_at, m.updated_at
FROM centry.moderation_state m
LEFT JOIN public.auth_core__user u ON u.id = m.user_id`

/* ── the admin queue ────────────────────────────────────────────────────── */

type queueParams struct {
	limit     int
	offset    int
	search    string
	status    string
	issueType string
	projectID string
	entityID  string
	sortBy    string
	sortOrder string
}

// AdministrationRequests serves `GET /admin/moderation_statuses/administration`.
//
// Registered on a STATIC `administration` segment, so there is no `{mode}` to
// read and this handler states its mode by existing — the trap #207's tests
// caught, where a handler sniffing `chi.URLParam(r, "mode")` gets an empty
// string on precisely the administration requests it was written for.
func (h *Handler) AdministrationRequests(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	query := r.URL.Query()
	limit, offset := paginationParams(query)
	params := queueParams{
		limit:     limit,
		offset:    offset,
		search:    strings.TrimSpace(query.Get("search")),
		status:    strings.TrimSpace(query.Get("status")),
		issueType: strings.TrimSpace(query.Get("issue_type")),
		projectID: strings.TrimSpace(query.Get("project_id")),
		entityID:  strings.TrimSpace(query.Get("entity_id")),
		sortBy:    query.Get("sort_by"),
		sortOrder: query.Get("sort_order"),
	}

	rows, total, err := h.listQueue(r.Context(), params)
	if err != nil {
		// Deliberately NOT an empty page. The stub this replaces answered 200
		// with `{"rows":[],"total":0}` unconditionally, so "no one has asked for
		// anything" and "this deployment cannot see the queue" rendered
		// identically — and an operator reading the former concludes there is
		// nothing to do.
		writeModerationError(w, http.StatusInternalServerError, "failed to read app requests")
		return
	}
	writeModerationJSON(w, http.StatusOK, map[string]any{"total": total, "rows": rows})
}

// paginationParams mirrors the limits pylon's `ModerationStateListQuery`
// declares: limit 1..100 defaulting to 20, offset >= 0.
func paginationParams(query map[string][]string) (limit, offset int) {
	limit, offset = 20, 0
	if values := query["limit"]; len(values) > 0 {
		if parsed, err := strconv.Atoi(values[0]); err == nil && parsed > 0 {
			limit = min(parsed, 100)
		}
	}
	if values := query["offset"]; len(values) > 0 {
		if parsed, err := strconv.Atoi(values[0]); err == nil && parsed > 0 {
			offset = parsed
		}
	}
	return limit, offset
}

// queueFilters renders the five filters the admin client may send.
//
// `search` matches the requester's email or name. pylon matches only users too,
// so widening it to the description would be answering a different question from
// the one the search box's own placeholder asks.
func queueFilters(params queueParams) (where string, args []any) {
	conditions := make([]string, 0, 5)
	add := func(condition string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(condition, "$"+strconv.Itoa(len(args))))
	}

	if params.search != "" {
		add(`(u.email ILIKE %[1]s OR u.name ILIKE %[1]s)`, "%"+params.search+"%")
	}
	if params.status != "" {
		add(`m.status = %[1]s`, params.status)
	}
	if params.issueType != "" {
		add(`m.issue_type = %[1]s`, params.issueType)
	}
	if params.entityID != "" {
		add(`m.entity_id = %[1]s`, params.entityID)
	}
	if params.projectID != "" {
		// A non-numeric project_id filters to NOTHING rather than being dropped:
		// silently ignoring an unparseable filter returns the whole queue to a
		// caller who asked for one project's slice of it.
		id, err := strconv.Atoi(params.projectID)
		if err != nil {
			return " WHERE FALSE", nil
		}
		add(`m.project_id = %[1]s`, id)
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func (h *Handler) listQueue(ctx context.Context, params queueParams) ([]requestRow, int, error) {
	where, args := queueFilters(params)

	var total int
	if err := h.pool.QueryRow(ctx, `
SELECT COUNT(*)
FROM centry.moderation_state m
LEFT JOIN public.auth_core__user u ON u.id = m.user_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count app requests: %w", err)
	}

	sortColumn, ok := sortableRequestColumns[params.sortBy]
	if !ok {
		sortColumn = sortableRequestColumns["created_at"]
	}
	direction := "ASC"
	if strings.EqualFold(params.sortOrder, "desc") {
		direction = "DESC"
	}

	pageArgs := append(append([]any{}, args...), params.limit, params.offset)
	// The `m.id` tiebreaker is not decoration: `created_at` has a default of
	// NOW() and a burst of requests can share a timestamp, so ordering on it
	// alone is not a total order and rows repeat across LIMIT/OFFSET pages.
	statement := requestColumns + where +
		` ORDER BY ` + sortColumn + ` ` + direction + ` NULLS LAST, m.id ` + direction +
		` LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)

	rows, err := scanRequests(ctx, h.queryFunc(), statement, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

/* ── the decision ───────────────────────────────────────────────────────── */

// requestDecisionBody is the accepted write.
//
// Everything below `RejectionComment` is declared ONLY so a body carrying it can
// be refused explicitly. None of them is ever applied. See this file's header on
// why the person answering a request may not edit what was asked.
type requestDecisionBody struct {
	ID               *int64          `json:"id"`
	Status           *string         `json:"status"`
	RejectionComment *string         `json:"rejection_comment"`
	Meta             json.RawMessage `json:"meta"`
	UserID           *int64          `json:"user_id"`
	ProjectID        *int64          `json:"project_id"`
	EntityID         *string         `json:"entity_id"`
	IssueType        *string         `json:"issue_type"`
	Description      *string         `json:"description"`
}

// AdministrationRequestUpdate serves `PUT /admin/moderation_status/administration`.
//
// The id travels in the BODY, not the path — pylon's shape, and what the
// existing admin_ui client sends.
func (h *Handler) AdministrationRequestUpdate(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	var body requestDecisionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeModerationError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if reason, ok := refusedDecisionField(body); !ok {
		writeModerationError(w, http.StatusBadRequest, reason)
		return
	}
	if body.ID == nil || *body.ID <= 0 {
		writeModerationError(w, http.StatusBadRequest, "id is required")
		return
	}

	status, comment, err := decisionFields(body)
	if err != nil {
		writeModerationError(w, http.StatusBadRequest, err.Error())
		return
	}

	// issue_type decides which decision path runs. It is read separately
	// from, and BEFORE, the row lock decideProjectRequest takes: issue_type
	// is immutable once a request is filed (see this file's header on why
	// the requester's own fields cannot be rewritten later), so a plain read
	// here cannot race with anything that would change the answer.
	issueType, err := h.lookupIssueType(r.Context(), *body.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeModerationError(w, http.StatusNotFound, "app request not found")
		return
	case err != nil:
		writeModerationError(w, http.StatusInternalServerError, "failed to read the app request")
		return
	}

	var row *requestRow
	if issueType == ProjectRequestIssueType {
		row, err = h.decideProjectRequest(r.Context(), *body.ID, status, comment)
	} else {
		row, err = h.applyDecision(r.Context(), *body.ID, status, comment)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// An id that matches nothing is a 404, not a 200 that changed nothing.
		writeModerationError(w, http.StatusNotFound, "app request not found")
		return
	case errors.Is(err, errRequestAlreadyDecided):
		// Not a 500: the row exists and the decision even matches the caller's
		// intent half the time (approving twice). A 200 that ran the
		// provisioning pipeline a second time would create a SECOND project
		// for one request.
		writeModerationError(w, http.StatusConflict, "this request has already been decided")
		return
	case errors.Is(err, errProjectProvisioningFailed):
		// The moderation row is untouched (still pending) — see
		// decideProjectRequest: the UPDATE only runs after Provision
		// succeeds. A 502 names the failure without leaving a request an
		// operator cannot retry.
		writeModerationError(w, http.StatusBadGateway,
			"approved, but creating the project failed: "+err.Error())
		return
	case err != nil:
		writeModerationError(w, http.StatusInternalServerError, "failed to update the app request")
		return
	}

	// The in-app row is the delivery of record and is already committed; the
	// e-mail is a second channel that never fails the decision (ADR-0024 WP7).
	h.mailDecision(r.Context(), row)
	writeModerationJSON(w, http.StatusOK, row)
}

// mailDecision sends the same pre-rendered sentence the notification row
// carries to the requester's address, when a mailer is wired.
func (h *Handler) mailDecision(ctx context.Context, row *requestRow) {
	if h.mailer == nil || row == nil || !h.mailer.Configured(ctx) {
		return
	}
	email := row.UserEmail
	if email == "" {
		if err := h.pool.QueryRow(ctx, `SELECT email FROM public.auth_core__user WHERE id = $1`, row.UserID).Scan(&email); err != nil || email == "" {
			slog.Warn("moderation decision e-mail not sent: requester address unknown", "user_id", row.UserID)
			return
		}
	}
	if err := h.mailer.SendModerationDecision(ctx, appmailer.ModerationDecision{
		Email: email, Message: decisionMessage(*row),
	}); err != nil {
		slog.Warn("moderation decision e-mail not sent", "user_id", row.UserID, "reason", err.Error())
	}
}

// decisionMessage is the one sentence both channels carry.
func decisionMessage(row requestRow) string {
	// A Project Request reads oddly through the generic sentence below —
	// "Your Project Request moderation request has been approved" repeats
	// "request" twice and never says what was approved. See
	// project_requests.go for what a Project Request row is and what its
	// approval causes.
	if row.IssueType == ProjectRequestIssueType {
		return projectRequestDecisionMessage(row)
	}

	message := fmt.Sprintf("Your %s moderation request has been %s.", row.IssueType, row.Status)
	if row.RejectionComment != nil && *row.RejectionComment != "" {
		message += " Reason: " + *row.RejectionComment
	}
	return message
}

// refusedDecisionField reports the first field a moderator may not write.
func refusedDecisionField(body requestDecisionBody) (string, bool) {
	const recordIsImmutable = "a decision may set status and rejection_comment only: %s belongs to the " +
		"request and is not editable by the person answering it, or an approved row would stop being " +
		"evidence of what was approved"

	switch {
	case body.UserID != nil:
		return fmt.Sprintf(recordIsImmutable, "user_id"), false
	case body.ProjectID != nil:
		return fmt.Sprintf(recordIsImmutable, "project_id"), false
	case body.EntityID != nil:
		return fmt.Sprintf(recordIsImmutable, "entity_id"), false
	case body.IssueType != nil:
		return fmt.Sprintf(recordIsImmutable, "issue_type"), false
	case body.Description != nil:
		return fmt.Sprintf(recordIsImmutable, "description"), false
	case !isEmptyMeta(body.Meta):
		return "meta cannot be written: nothing reads it, and this endpoint REPLACES rather than " +
			"merges it, so a decision would silently destroy what the requester stored", false
	}
	return "", true
}

// decisionFields validates the status/comment pair.
func decisionFields(body requestDecisionBody) (status string, comment *string, err error) {
	if body.Status == nil {
		return "", nil, errors.New("status is required")
	}
	status = *body.Status

	trimmed := ""
	if body.RejectionComment != nil {
		trimmed = strings.TrimSpace(*body.RejectionComment)
	}

	switch status {
	case statusRejected:
		// Enforced here rather than by a validator that a caller can skip by
		// omitting the key — see this file's header. A rejection whose reason is
		// null is delivered to the requester as a bare refusal.
		if trimmed == "" {
			return "", nil, errors.New("rejection_comment is required when rejecting a request")
		}
		return status, &trimmed, nil
	case statusApproved:
		if trimmed != "" {
			return "", nil, errors.New("rejection_comment cannot be set on an approval")
		}
		return status, nil, nil
	case statusPending:
		// pylon allows a decided request to be moved back to `pending`, silently
		// and with no record that it was ever decided. No client sends it and
		// nothing distinguishes the reopened row from one never answered.
		return "", nil, errors.New("a request cannot be returned to pending: approve or reject it")
	default:
		return "", nil, fmt.Errorf("status must be %q or %q", statusApproved, statusRejected)
	}
}

// applyDecision writes the status and the requester's notification atomically.
//
// One transaction on purpose: a decision the requester is never told about is
// the same to them as no decision, and pylon's version — which fires the event
// after committing and swallows any failure into a log line — can produce
// exactly that.
func (h *Handler) applyDecision(
	ctx context.Context, id int64, status string, comment *string,
) (*requestRow, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin decision: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var row requestRow
	if err := tx.QueryRow(ctx, `
UPDATE centry.moderation_state
SET status = $1, rejection_comment = $2, updated_at = NOW()
WHERE id = $3
RETURNING id, user_id, project_id, issue_type, COALESCE(entity_id, ''), description,
          status, rejection_comment, created_at, updated_at`,
		status, comment, id,
	).Scan(&row.ID, &row.UserID, &row.ProjectID, &row.IssueType, &row.EntityID,
		&row.Description, &row.Status, &row.RejectionComment, &row.CreatedAt, &row.UpdatedAt); err != nil {
		return nil, err
	}

	if err := insertDecisionNotification(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit decision: %w", err)
	}

	// The address is not returned by the UPDATE (it lives on another table) and
	// the client re-reads the list after a write, so it is left empty here
	// rather than costing a second round trip.
	return &row, nil
}

// insertDecisionNotification mirrors the payload pylon's `notifications_stream`
// event carries, including the pre-rendered `message`: the notification
// renderer has no branch for these two event types, in either frontend, so the
// sentence has to travel with the row.
func insertDecisionNotification(ctx context.Context, tx pgx.Tx, row requestRow) error {
	message := decisionMessage(row)

	meta := map[string]any{
		"issue_type":        row.IssueType,
		"entity_id":         row.EntityID,
		"status":            row.Status,
		"rejection_comment": row.RejectionComment,
		"message":           message,
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode notification meta: %w", err)
	}

	// `uuid` and `is_seen` are bound here, and not left to a column default.
	// centry.notifications belongs to the current platform schema, and that
	// schema declares both columns NOT NULL with NO server default: the
	// defaults are Python-side, on the legacy model. Only this repository's own
	// 001_initial.sql adds server defaults. It creates the table with
	// CREATE TABLE IF NOT EXISTS. A database that the legacy platform created
	// therefore never gets them. An INSERT that omits the two columns therefore
	// fails with SQLSTATE 23502 on such a database. The failure rolls back the
	// whole decision transaction, so approve and reject answer 500 and the
	// moderation_state row stays `pending`. Every generated notification query
	// in internal/db/queries binds both columns for the same reason.
	if _, err := tx.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type)
VALUES ($1::text::uuid, FALSE, $2, $3, $4, $5)`,
		uuid.NewString(), row.ProjectID, row.UserID, encoded, "moderation_"+row.Status,
	); err != nil {
		return fmt.Errorf("notify requester: %w", err)
	}
	return nil
}

/* ── the project-scoped pair ────────────────────────────────────────────── */

// Requests serves `GET /admin/moderation_status/{mode}/{projectID}/{entityID}`.
//
// "What have I asked for, for this entity" — scoped to the caller in SQL, which
// is where pylon scopes it too. The mode segment selects nothing: this path has
// only the one handler in pylon, and an `administration` value on it must not
// widen the answer, so it is not read.
func (h *Handler) Requests(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	projectID, ok := pathProjectID(r)
	if !ok {
		writeModerationError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	userID, err := requesterID(r)
	if err != nil {
		writeModerationError(w, http.StatusForbidden, err.Error())
		return
	}

	args := []any{projectID, chi.URLParam(r, "entityID"), userID}
	statement := requestColumns + `
WHERE m.project_id = $1 AND m.entity_id = $2 AND m.user_id = $3`
	if issueType := strings.TrimSpace(r.URL.Query().Get("issue_type")); issueType != "" {
		args = append(args, issueType)
		statement += ` AND m.issue_type = $4`
	}
	// Newest first: the client reads `rows[0]`, and the state it wants is the
	// most recent request, not the first one ever made.
	statement += ` ORDER BY m.created_at DESC, m.id DESC`

	rows, err := scanRequests(r.Context(), h.queryFunc(), statement, args...)
	if err != nil {
		writeModerationError(w, http.StatusInternalServerError, "failed to read app requests")
		return
	}
	writeModerationJSON(w, http.StatusOK, map[string]any{"total": len(rows), "rows": rows})
}

// requestCreateBody is what a REQUESTER may send.
//
// `Status` and `UserID` are declared so they can be checked, not applied — see
// this file's header on self-approval and forged authorship. `ProjectID` and
// `EntityID` are absent entirely: they come from the path, so a body carrying
// them cannot disagree with the URL in the first place.
type requestCreateBody struct {
	IssueType   *string         `json:"issue_type"`
	Description *string         `json:"description"`
	Status      *string         `json:"status"`
	UserID      *int64          `json:"user_id"`
	Meta        json.RawMessage `json:"meta"`
}

// RequestCreate serves `POST /admin/moderation_status/{mode}/{projectID}/{entityID}`.
func (h *Handler) RequestCreate(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	projectID, ok := pathProjectID(r)
	if !ok {
		writeModerationError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	entityID := chi.URLParam(r, "entityID")
	if strings.TrimSpace(entityID) == "" {
		writeModerationError(w, http.StatusBadRequest, "entity id is required")
		return
	}
	userID, err := requesterID(r)
	if err != nil {
		writeModerationError(w, http.StatusForbidden, err.Error())
		return
	}

	var body requestCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeModerationError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if reason, ok := refusedCreateField(body); !ok {
		writeModerationError(w, http.StatusBadRequest, reason)
		return
	}

	issueType, description, err := createFields(body)
	if err != nil {
		writeModerationError(w, http.StatusBadRequest, err.Error())
		return
	}

	var row requestRow
	if err := h.pool.QueryRow(r.Context(), `
INSERT INTO centry.moderation_state (user_id, project_id, issue_type, entity_id, description, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_id, project_id, issue_type, COALESCE(entity_id, ''), description,
          status, rejection_comment, created_at, updated_at`,
		userID, projectID, issueType, entityID, description, statusPending,
	).Scan(&row.ID, &row.UserID, &row.ProjectID, &row.IssueType, &row.EntityID,
		&row.Description, &row.Status, &row.RejectionComment, &row.CreatedAt, &row.UpdatedAt); err != nil {
		writeModerationError(w, http.StatusInternalServerError, "failed to create the app request")
		return
	}

	writeModerationJSON(w, http.StatusCreated, row)
}

func refusedCreateField(body requestCreateBody) (string, bool) {
	switch {
	case body.Status != nil && *body.Status != statusPending:
		// Refused, never downgraded. A caller that believes it filed an approved
		// request and got a 201 has been told something false.
		return "status cannot be chosen by the requester: a new request is always pending, and only " +
			"a moderator decides it", false
	case body.UserID != nil:
		return "user_id cannot be set: a request is authored by the authenticated caller, and the " +
			"decision notification is delivered to that author", false
	case !isEmptyMeta(body.Meta):
		return "meta cannot be written: nothing in this platform reads a key out of it", false
	}
	return "", true
}

func createFields(body requestCreateBody) (issueType, description string, err error) {
	if body.IssueType == nil {
		return "", "", errors.New("issue_type is required")
	}
	issueType = strings.TrimSpace(*body.IssueType)
	if issueType == "" {
		return "", "", errors.New("issue_type is required")
	}
	// The column is VARCHAR(256); a longer value is a 500 from PostgreSQL
	// otherwise, which reads to the user as "the request failed to send".
	if len(issueType) > 256 {
		return "", "", errors.New("issue_type is longer than 256 characters")
	}

	if body.Description == nil {
		return "", "", errors.New("description is required")
	}
	description = strings.TrimSpace(*body.Description)
	if description == "" {
		return "", "", errors.New("description is required")
	}
	return issueType, description, nil
}

// RequestDelete serves `DELETE /admin/moderation_status/{mode}/{projectID}/{entityID}`.
//
// "Withdraw what I asked for." The caller removes their OWN requests for one
// catalogue entry in one project, together with the decision notifications that
// name it.
//
// ## Why this route exists (issue #544)
//
// A moderation row could be created and decided, and never removed. pylon has
// no delete either, so a deployment accumulated one row per request for ever,
// and the queue's first page filled with dead rows. The end-to-end suite made
// the same fault visible fastest: 20 runs on one stack left 111 rows, and the
// journeys that file a probe row could no longer find their own.
//
// ## The scope is the CALLER's own rows, not the operator's queue
//
// The delete uses the same three-column scope as the per-entity GET beside it —
// `project_id`, `entity_id` and the authenticated `user_id` — and the `{mode}`
// segment selects nothing. An `administration` value on the path must not widen
// the answer, exactly as it must not widen the read.
//
// So a moderator cannot delete another person's request. That is deliberate:
// the row is the record of what an operator was asked and what they answered,
// and a moderator who could erase it could erase the evidence of their own
// decision. The requester deletes only their own record, which no gate reads:
// nothing in this platform treats an approved row as an authorisation (see this
// file's header on what a decision causes), so a withdrawn request takes no
// access with it.
//
// ## The notifications go too
//
// A decision writes a `centry.notifications` row addressed to the requester and
// naming the entity. Leaving those behind reproduces the fault this route
// exists for on a second table: the requester's own notification list is read
// with a limit, so a hundred dead decision notices push the newest one off the
// first page. Only the caller's own rows for this entity are removed, and only
// the two `moderation_*` event types this handler writes.
//
// ## Authorisation
//
// Gated in internal/api/router.go on `admin.moderation.create`, in the default
// mode, resolved against the caller's membership of `{projectID}` — the same
// permission as the POST beside it. A person who may file a request may take it
// back. No new permission is granted, so no deployment gains a capability that
// an operator has to review.
func (h *Handler) RequestDelete(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeModerationError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	projectID, ok := pathProjectID(r)
	if !ok {
		writeModerationError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	entityID := chi.URLParam(r, "entityID")
	if strings.TrimSpace(entityID) == "" {
		writeModerationError(w, http.StatusBadRequest, "entity id is required")
		return
	}
	userID, err := requesterID(r)
	if err != nil {
		writeModerationError(w, http.StatusForbidden, err.Error())
		return
	}

	rows, notifications, err := h.deleteOwnRequests(r.Context(), int64(projectID), entityID, userID)
	if err != nil {
		writeModerationError(w, http.StatusInternalServerError, "failed to delete the app request")
		return
	}
	if len(rows) == 0 {
		// A 404, not a 200 that removed nothing. A caller that deleted somebody
		// else's row by mistake, or named an entity that does not exist, must
		// read a different answer from a caller whose row is really gone —
		// otherwise a teardown reports success while the table keeps growing,
		// which is how this issue was measured in the first place.
		writeModerationError(w, http.StatusNotFound, "app request not found")
		return
	}
	writeModerationJSON(w, http.StatusOK, map[string]any{
		"total": len(rows), "rows": rows, "notifications_deleted": notifications,
	})
}

// deleteOwnRequests removes the caller's rows for one entity and the decision
// notifications that name it, in ONE transaction.
//
// One transaction because the two deletes are one act: a commit that removed
// the request and kept the notice would leave the requester a message about a
// request that no longer exists, and the reverse would leave the row this
// endpoint was called to remove.
func (h *Handler) deleteOwnRequests(
	ctx context.Context, projectID int64, entityID string, userID int64,
) ([]requestRow, int64, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The address is not selected: it lives on another table, and the row is
	// gone by the time the caller reads this answer. See applyDecision.
	deleted, err := scanRequests(ctx,
		func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			return tx.Query(ctx, sql, args...)
		}, `
DELETE FROM centry.moderation_state m
WHERE m.project_id = $1 AND m.entity_id = $2 AND m.user_id = $3
RETURNING m.id, m.user_id, ''::text, m.project_id, m.issue_type, COALESCE(m.entity_id, ''),
          m.description, m.status, m.rejection_comment, m.created_at, m.updated_at`,
		projectID, entityID, userID)
	if err != nil {
		return nil, 0, err
	}
	if len(deleted) == 0 {
		return nil, 0, nil
	}

	tag, err := tx.Exec(ctx, `
DELETE FROM centry.notifications
WHERE user_id = $1 AND project_id = $2
  AND event_type IN ('moderation_approved', 'moderation_rejected')
  AND meta->>'entity_id' = $3`, userID, projectID, entityID)
	if err != nil {
		return nil, 0, fmt.Errorf("delete decision notifications: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit delete: %w", err)
	}
	return deleted, tag.RowsAffected(), nil
}

/* ── shared ─────────────────────────────────────────────────────────────── */

// isEmptyMeta accepts an absent, null or `{}` meta and nothing else. Both
// existing clients send `{}`; refusing that would break them for no gain.
func isEmptyMeta(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null" || trimmed == "{}"
}

func pathProjectID(r *http.Request) (int, bool) {
	projectID, err := strconv.Atoi(chi.URLParam(r, "projectID"))
	return projectID, err == nil && projectID > 0
}

// requesterID resolves the authenticated author.
//
// `OwningUserID` refuses a token principal as an author, which is the behaviour
// wanted here rather than an obstacle: a request filed by an API key has no
// person to notify of the decision and no one for the operator to answer.
func requesterID(r *http.Request) (int64, error) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0, errors.New("an app request requires an authenticated user")
	}
	id, ok := user.OwningUserID()
	if !ok {
		return 0, errors.New("an app request must be filed by a user, not by a token principal")
	}
	return id, nil
}

type queryFunc func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)

func (h *Handler) queryFunc() queryFunc {
	return func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
		return h.pool.Query(ctx, sql, args...)
	}
}

func scanRequests(ctx context.Context, query queryFunc, statement string, args ...any) ([]requestRow, error) {
	rows, err := query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list app requests: %w", err)
	}
	defer rows.Close()

	// Non-nil so an empty result marshals as `[]` and not `null`.
	result := make([]requestRow, 0)
	for rows.Next() {
		var row requestRow
		if err := rows.Scan(&row.ID, &row.UserID, &row.UserEmail, &row.ProjectID, &row.IssueType,
			&row.EntityID, &row.Description, &row.Status, &row.RejectionComment,
			&row.CreatedAt, &row.UpdatedAt); err != nil {
			// A scan failure is a schema disagreement, not a bad row: skipping it
			// would report a shorter queue rather than a broken one.
			return nil, fmt.Errorf("scan app request: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func writeModerationJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeModerationError(w http.ResponseWriter, code int, reason string) {
	writeModerationJSON(w, code, map[string]any{"error": reason})
}
