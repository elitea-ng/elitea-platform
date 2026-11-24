package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// weightSumEpsilon bounds float error when re-verifying that a routing rule's
// target weights sum to 1.0. Weights are authored as decimals (0.7 + 0.3), so a
// small tolerance absorbs base-2 rounding without admitting a real mis-sum.
const weightSumEpsilon = 1e-6

// maxGovernanceRequestBytes bounds every governance request body before it is
// decoded. A row is one authored definition — a routing rule, a budget, an
// egress allowlist of hostnames (≤ 253 bytes each) — so 256 KiB holds about a
// thousand allowlist entries and is far above any realistic form submission,
// while keeping the decode allocation small on an admin path.
const maxGovernanceRequestBytes = 256 << 10

// governanceQuerier is the minimal subset of *pgxpool.Pool the governance CRUD
// needs. Narrowing it to an interface lets tests substitute a fake without a
// live database — the project's established seam pattern (project_resolver.go).
type governanceQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// GovernanceRow is one authored governance definition in the global
// gateway.governance_config table (see migrations/shared/0067_gateway_budget_schema.sql).
// The JSONB `Data` payload carries the entity-specific fields the schema-driven
// admin form produces and the gateway GovernanceStore consumes.
type GovernanceRow struct {
	ID        string         `json:"id,omitempty"`
	Type      string         `json:"type"`
	Section   string         `json:"section"`
	Name      string         `json:"name"`
	Data      map[string]any `json:"data"`
	Enabled   bool           `json:"enabled"`
	CreatedAt string         `json:"created_at,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
}

// GovernanceHandler serves CRUD for the global governance authoring table.
// Register takes the route gates (gates.go), and every write additionally
// checks the caller against the kind of the row (governanceWritePermissions):
// the route gate admits a policy or a guardrails manager, and only the second
// writes a guardrail. This handler also re-verifies routing-rule CEL and
// target weights on every write regardless of what the client validated
// (§3.1).
type GovernanceHandler struct {
	db    governanceQuerier
	holds func(r *http.Request, names ...string) bool
}

// NewGovernanceHandler wires a handler over the given querier.
func NewGovernanceHandler(db governanceQuerier) *GovernanceHandler {
	return &GovernanceHandler{db: db}
}

// Routes returns a standalone router mounting the governance CRUD +
// CEL-validation endpoints — used in tests.
func (h *GovernanceHandler) Routes(gates GovernanceGates) chi.Router {
	r := chi.NewRouter()
	h.Register(r, gates)
	return r
}

// Register attaches the governance endpoints to an existing router, each
// behind its gate, alongside the other gateway edge controls so they share
// the /gateway prefix without colliding on a Mount pattern. The CEL
// validation is a read: it changes nothing.
func (h *GovernanceHandler) Register(r chi.Router, gates GovernanceGates) {
	view := mustGate("governance", "view", gates.View)
	manage := mustGate("governance", "manage", gates.Manage)
	if gates.Holds == nil {
		panic("gateway: governance mounted without its Holds check")
	}
	h.holds = gates.Holds
	r.With(view).Get("/governance", h.List)
	r.With(manage).Post("/governance", h.Create)
	r.With(manage).Put("/governance/{id}", h.Update)
	r.With(manage).Delete("/governance/{id}", h.Delete)
	r.With(view).Post("/governance/validate-cel", h.ValidateCEL)
}

// mayWriteType answers whether the caller may write a row of rowType, and
// writes the 403 when not. It runs before validation, so a caller who may not
// write the kind learns nothing about the body. The kind of the STORED row is
// checked in the write's own statement (governanceWriter.storedClause).
func (h *GovernanceHandler) mayWriteType(w http.ResponseWriter, r *http.Request, rowType string) bool {
	// A row with no type has no kind to decide by, and validation refuses it
	// for every caller with the same 400; nothing is written.
	if strings.TrimSpace(rowType) == "" || h.writer(r).may(rowType) {
		return true
	}
	writeForbidden(w)
	return false
}

// writer answers what the caller may write, from the gate's resolution. A
// handler that was never registered has no holds and may write nothing.
func (h *GovernanceHandler) writer(r *http.Request) governanceWriter {
	if h.holds == nil {
		return governanceWriter{}
	}
	writer := governanceWriter{
		legacy:     h.holds(r, adminaccess.ConfigurationGovernance),
		guardrails: h.holds(r, adminaccess.LLMGuardrailsManage),
		policy:     h.holds(r, adminaccess.LLMPolicyManage),
	}
	// The token rule (ADR-0059 R1.2b, issue 2993): a stored guardrail is the
	// critical outcome of an update or a delete. Only those two verbs are
	// rows of GovernanceRowPattern; a create is decided on its body.
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		writer.tokenNoGuardrail = adminaccess.TokenRefused(r.Context(), r.Method,
			adminaccess.GovernanceRowPattern, adminaccess.OutcomeGuardrail)
	}
	return writer
}

// refuseTokenForType applies the token rule of the action table to the
// outcome of a row of rowType on (r.Method, pattern): a guardrail is the
// critical outcome (adminaccess.GovernanceRowAction). It answers true after
// writing the 403.
func refuseTokenForType(w http.ResponseWriter, r *http.Request, pattern, rowType string) bool {
	return adminaccess.RefuseToken(w, r, r.Method, pattern, adminaccess.GovernanceRowAction(rowType))
}

// refuseMissedWrite answers a write that matched no row: 404 when the row
// does not exist, 403 when it exists and its stored kind is one the caller
// may not write, and 500 when the read fails, so an outage is not reported
// as a missing row.
//
// The 403 tells a caller that the id exists. That is no disclosure: ids are
// UUIDs, and the kind of every row is in the listing, which the policy view
// permission reads.
//
// A stored guardrail that the write's statement left alone for the token
// rule's sake is answered with the token rule's refusal. A token's miss on a
// row it may write now means the row changed between the statement and this
// read (it was a guardrail then): that is a 409 to retry, not a missing
// permission.
func (h *GovernanceHandler) refuseMissedWrite(ctx context.Context, w http.ResponseWriter, r *http.Request, id string) {
	var stored string
	err := h.db.QueryRow(ctx, `SELECT type FROM gateway.governance_config WHERE id::text = $1`, id).Scan(&stored)
	writer := h.writer(r)
	switch {
	case err == nil && stored == guardrailType && writer.tokenNoGuardrail:
		refuseTokenForType(w, r, adminaccess.GovernanceRowPattern, stored)
	case err == nil && writer.tokenNoGuardrail && writer.may(stored):
		writeError(w, http.StatusConflict, "the governance entry changed while this write was made: read it again and retry")
	case err == nil:
		writeForbidden(w)
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "governance entry not found")
	default:
		writeError(w, http.StatusInternalServerError, "read failed")
	}
}

func writeForbidden(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "insufficient permissions")
}

// List returns governance rows, optionally filtered by ?type=.
func (h *GovernanceHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	typeFilter := r.URL.Query().Get("type")
	var (
		rows pgx.Rows
		err  error
	)
	const base = `SELECT id::text, type, section, name, data, enabled, created_at, updated_at
		FROM gateway.governance_config`
	if typeFilter != "" {
		rows, err = h.db.Query(ctx, base+` WHERE type = $1 ORDER BY section, type, name`, typeFilter)
	} else {
		rows, err = h.db.Query(ctx, base+` ORDER BY section, type, name`)
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []GovernanceRow{}, "total": 0})
		return
	}
	defer rows.Close()

	items := make([]GovernanceRow, 0)
	for rows.Next() {
		row, scanErr := scanGovernanceRow(rows)
		if scanErr != nil {
			continue
		}
		items = append(items, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// Create inserts a new governance row after validating the payload.
func (h *GovernanceHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	body, ok := decodeGovernanceBody(w, r)
	if !ok {
		return
	}
	if !h.mayWriteType(w, r, body.Type) {
		return
	}
	// The token rule on the kind of the new row (ADR-0059 R1.2b).
	if refuseTokenForType(w, r, adminaccess.GovernanceCreatePattern, body.Type) {
		return
	}
	if err := validateGovernanceRow(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.checkThrottleIDsExist(ctx, w, body.Data) {
		return
	}

	dataBytes, _ := json.Marshal(body.Data)
	const q = `INSERT INTO gateway.governance_config (type, section, name, data, enabled)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, type, section, name, data, enabled, created_at, updated_at`
	row, err := scanGovernanceRow(h.db.QueryRow(ctx, q, body.Type, body.Section, body.Name, dataBytes, body.Enabled))
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a governance entry with this section/type/name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "create failed")
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// Update replaces the mutable fields of an existing governance row by id.
func (h *GovernanceHandler) Update(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	id := chi.URLParam(r, "id")
	body, ok := decodeGovernanceBody(w, r)
	if !ok {
		return
	}
	if !h.mayWriteType(w, r, body.Type) {
		return
	}
	// The token rule (ADR-0059 R1.2b): an update is a guardrail write when
	// either side is a guardrail. The body's side is checked here; the stored
	// side is checked in the statement (storedClause), and refuseMissedWrite
	// answers a miss on it.
	if refuseTokenForType(w, r, adminaccess.GovernanceRowPattern, body.Type) {
		return
	}
	if err := validateGovernanceRow(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.checkThrottleIDsExist(ctx, w, body.Data) {
		return
	}

	dataBytes, _ := json.Marshal(body.Data)
	// The stored row's kind is checked in the same statement, so a policy
	// manager cannot turn a guardrail into a routing rule, and no write can
	// slip between a read of the kind and the update.
	clause, clauseArgs := h.writer(r).storedClause(7)
	q := `UPDATE gateway.governance_config
		SET type = $1, section = $2, name = $3, data = $4, enabled = $5, updated_at = now()
		WHERE id = $6 AND ` + clause + `
		RETURNING id::text, type, section, name, data, enabled, created_at, updated_at`
	args := append([]any{body.Type, body.Section, body.Name, dataBytes, body.Enabled, id}, clauseArgs...)
	row, err := scanGovernanceRow(h.db.QueryRow(ctx, q, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.refuseMissedWrite(ctx, w, r, id)
			return
		}
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a governance entry with this section/type/name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// Delete removes a governance row by id.
func (h *GovernanceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	id := chi.URLParam(r, "id")
	clause, clauseArgs := h.writer(r).storedClause(2)
	ct, err := h.db.Exec(ctx, `DELETE FROM gateway.governance_config WHERE id = $1 AND `+clause,
		append([]any{id}, clauseArgs...)...)
	if err != nil {
		writeError(w, http.StatusNotFound, "governance entry not found")
		return
	}
	if ct.RowsAffected() == 0 {
		h.refuseMissedWrite(ctx, w, r, id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ValidateCELRequest is the ad-hoc "Validate CEL" action payload.
type ValidateCELRequest struct {
	CEL string `json:"cel"`
}

// ValidateCEL type-checks a CEL expression against the governance environment
// without persisting anything — backs the type:action "Validate CEL" control.
func (h *GovernanceHandler) ValidateCEL(w http.ResponseWriter, r *http.Request) {
	var req ValidateCELRequest
	if !decodeGovernanceJSON(w, r, &req) {
		return
	}
	if err := CompileRoutingCEL(req.CEL); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

// decodeGovernanceJSON decodes a body of at most maxGovernanceRequestBytes into
// dst. The body must be exactly one JSON value with no unknown top-level field:
// a misspelt key or a second value would otherwise be dropped silently. An
// oversized body is a 413 and any other refusal a 400; either way it writes the
// response and returns false before any database work starts.
func decodeGovernanceJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxGovernanceRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(dst)
	if err == nil {
		var trailing json.RawMessage
		if err = decoder.Decode(&trailing); errors.Is(err, io.EOF) {
			return true
		}
	}
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid request body")
	return false
}

// decodeGovernanceBody decodes a request body into a GovernanceRow, writing a
// 413 or 400 and returning ok=false on oversized or malformed input.
func decodeGovernanceBody(w http.ResponseWriter, r *http.Request) (GovernanceRow, bool) {
	var body GovernanceRow
	if !decodeGovernanceJSON(w, r, &body) {
		return GovernanceRow{}, false
	}
	if body.Section == "" {
		body.Section = "governance"
	}
	if body.Data == nil {
		body.Data = map[string]any{}
	}
	return body, true
}

// validateGovernanceRow enforces the required discriminators and — for routing
// rules — compiles the CEL expression and re-verifies the target weights sum to
// 1.0. This runs on every write and is the source of truth (design §3.1): a rule
// that fails here is rejected no matter what the client showed.
func validateGovernanceRow(row GovernanceRow) error {
	if strings.TrimSpace(row.Type) == "" {
		return errors.New("type is required")
	}
	if strings.TrimSpace(row.Name) == "" {
		return errors.New("name is required")
	}
	// The scope check runs for EVERY type, before the type switch. A scope the
	// gateway cannot evaluate makes the row inert whatever it carries, so
	// refusing it once here beats repeating the check in each branch and
	// missing one.
	if err := validateGovernanceScope(row.Data); err != nil {
		return err
	}
	if err := validateThrottleScope(row.Type, row.Data); err != nil {
		return err
	}
	switch row.Type {
	case "routing_rule":
		return validateRoutingRule(row.Data)
	case alertConfigType:
		return validateBudgetAlertData(row.Data)
	case egressAllowlistType:
		return validateEgressAllowlistData(row.Data)
	case guardrailType:
		return validateGuardrailData(row.Data)
	}
	return nil
}

// egressAllowlistType is the governance row that carries the LLM gateway's
// egress policy: the hosts a tenant-authored credential `api_base` may name
// (gap G6, shared migration 0112).
const egressAllowlistType = "egress_allowlist"

// validateEgressAllowlistData refuses a row the gateway would drop.
//
// It uses libs/go/egresslib — the SAME parser the gateway compiles the row with
// and the same one that reads GATEWAY_EGRESS_ALLOWLIST. That is the whole point
// of the shared module: a host this form accepts and the gateway silently
// ignores would leave an operator looking at a saved rule while their provider
// keeps failing.
//
// Two rules beyond the grammar:
//
//   - The list may not be EMPTY. An empty egress list is not the empty
//     mcp_allowlist, which turns that control off. This one permits nothing and
//     widens nothing, so saving it is always a mistake.
//   - The row may not be SCOPED. Half of what it governs — whether bifrost's
//     SSRF-safe dialer is relaxed for the self-hosted provider classes — is
//     decided with no project in hand, so a scoped row could only be
//     half-honoured. Applying it globally would be wider than it was authored.
func validateEgressAllowlistData(data map[string]any) error {
	if err := requireGlobalScope(data); err != nil {
		return err
	}
	raw, err := egressAllowlistEntries(data)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("egress.allowlist must name at least one host: an empty list permits no destination " +
			"and widens nothing. Delete the entry instead")
	}
	for _, entry := range raw {
		if _, perr := egresslib.ParseEntry(entry); perr != nil {
			return perr
		}
	}
	return nil
}

// egressAllowlistEntries reads the list from either the grouped shape the admin
// form writes (`egress.allowlist`) or the flat one a hand-written row carries.
// A non-string member is refused rather than skipped: a dropped entry is a
// destination the operator believes they permitted.
func egressAllowlistEntries(data map[string]any) ([]string, error) {
	raw, ok := data["allowlist"].([]any)
	if !ok {
		group, _ := data["egress"].(map[string]any)
		raw, _ = group["allowlist"].([]any)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("every egress.allowlist entry must be a string")
		}
		if strings.TrimSpace(s) == "" {
			return nil, errors.New("an egress.allowlist entry must not be empty")
		}
		out = append(out, s)
	}
	return out, nil
}

// governanceScope returns the scope of a row the way the LLM gateway reads it
// (elitea-llm-gateway internal/policy parseScope): the `scope` object, or the
// top level of `data` when `scope` is absent, not an object, or empty. A
// hand-written row can carry `agent_ids` or `project_ids` at the top level,
// and the gateway applies them. Every scope check here reads through this
// function, so a key cannot skip a check by moving out of `scope`.
func governanceScope(data map[string]any) map[string]any {
	scope, _ := data["scope"].(map[string]any)
	if len(scope) == 0 {
		return data
	}
	return scope
}

// requireGlobalScope refuses any scope dimension on a row that is global.
func requireGlobalScope(data map[string]any) error {
	scope := governanceScope(data)
	for _, key := range []string{"project_ids", "providers", "models", "team_ids", "agent_ids", "user_ids"} {
		if list, ok := scope[key].([]any); ok && len(list) > 0 {
			return errors.New("an egress_allowlist entry is global and carries no scope: the private-network " +
				"half of the decision is made without a project, so a scoped entry would apply to every " +
				"project anyway. Remove the scope")
		}
	}
	return nil
}

// validateGovernanceScope refuses a scope this platform cannot evaluate.
//
// `scope.team_ids` names teams. There are none: no migration creates a teams
// table and no /llm request carries a team, so the gateway marks such a row
// INERT — it loads, it appears in the list, and it can never match. Refusing it
// on write is the difference between an operator learning that now and learning
// it after an incident in which the rule they trusted did nothing.
//
// The admin schema no longer offers the field. This check is what covers the
// paths the schema does not: the REST API, a database restore, and any older
// client still sending it.
func validateGovernanceScope(data map[string]any) error {
	teams, ok := governanceScope(data)["team_ids"].([]any)
	if !ok || len(teams) == 0 {
		return nil
	}
	return errors.New("scope.team_ids is not supported: this platform has no teams, so a team-scoped " +
		"governance entry could never match a request. Scope the entry by project instead")
}

// validateBudgetAlertData rejects a global soft-alert row this surface would
// write but the gateway could not read.
//
// The gateway's budget snapshot casts both keys of this row on EVERY /llm call.
// The cast is guarded, so a bad value degrades to the shipped default rather
// than failing the request — but a value that silently does nothing is the
// defect #322 is about. The dedicated PUT /admin/gateway/budget-alerts surface
// validates the range already; this generic path writes the same row and must
// hold the same rule, or the two disagree about what a valid config is.
func validateBudgetAlertData(data map[string]any) error {
	if raw, present := data["enabled"]; present {
		if _, ok := raw.(bool); !ok {
			return errors.New("enabled must be a boolean")
		}
	}
	raw, present := data["threshold_pct"]
	if !present {
		return nil
	}
	// JSON numbers decode as float64 here; an integral value is required
	// because the column is smallint.
	pct, ok := raw.(float64)
	if !ok || pct != math.Trunc(pct) {
		return errors.New("threshold_pct must be a whole number")
	}
	if pct < 1 || pct > 100 {
		return errors.New("threshold_pct must be between 1 and 100")
	}
	return nil
}

// validateRoutingRule checks the CEL predicate compiles to a bool and the
// weighted targets sum to 1.0. Both checks are mandatory server-side.
func validateRoutingRule(data map[string]any) error {
	celExpr, _ := data["cel"].(string)
	if err := CompileRoutingCEL(celExpr); err != nil {
		return err
	}

	rawTargets, ok := data["targets"].([]any)
	if !ok || len(rawTargets) == 0 {
		return errors.New("routing rule requires at least one weighted target")
	}
	var sum float64
	for _, rt := range rawTargets {
		t, ok := rt.(map[string]any)
		if !ok {
			return errors.New("each routing target must be an object")
		}
		if p, _ := t["provider"].(string); strings.TrimSpace(p) == "" {
			return errors.New("each routing target requires a provider")
		}
		if m, _ := t["model"].(string); strings.TrimSpace(m) == "" {
			return errors.New("each routing target requires a model")
		}
		weight, ok := toFloat(t["weight"])
		if !ok {
			return errors.New("each routing target requires a numeric weight")
		}
		if weight < 0 {
			return errors.New("routing target weights must be non-negative")
		}
		sum += weight
	}
	if math.Abs(sum-1.0) > weightSumEpsilon {
		return errors.New("routing target weights must sum to 1.0")
	}
	return nil
}

// toFloat coerces a JSON-decoded numeric value to float64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// scanGovernanceRow scans one row/Row into a GovernanceRow. It accepts anything
// with a Scan method (pgx.Row and pgx.Rows both satisfy it), unmarshalling the
// JSONB data column and formatting timestamps to RFC3339.
func scanGovernanceRow(s interface{ Scan(...any) error }) (GovernanceRow, error) {
	var (
		row                  GovernanceRow
		dataBytes            []byte
		createdAt, updatedAt *time.Time
	)
	if err := s.Scan(&row.ID, &row.Type, &row.Section, &row.Name, &dataBytes, &row.Enabled, &createdAt, &updatedAt); err != nil {
		return GovernanceRow{}, err
	}
	if len(dataBytes) > 0 {
		_ = json.Unmarshal(dataBytes, &row.Data) // DB JSONB column; malformed rows surface as empty data
	}
	if row.Data == nil {
		row.Data = map[string]any{}
	}
	if createdAt != nil {
		row.CreatedAt = createdAt.Format(time.RFC3339)
	}
	if updatedAt != nil {
		row.UpdatedAt = updatedAt.Format(time.RFC3339)
	}
	return row, nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// Fallback for fakes/adapters that surface the constraint as a string.
	return strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint")
}

// writeError writes a JSON error envelope with the given status.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
