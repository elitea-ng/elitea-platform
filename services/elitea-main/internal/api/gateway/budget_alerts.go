// Package gateway holds the elitea-main edge control surfaces for the
// embedded Bifrost LLM gateway (governance authoring, budget alert toggles).
//
// # The global soft-alert control
//
// Soft alerts are emitted by the gateway when a budget crosses its threshold.
// This surface lets an operator turn that emission off platform-wide and set
// the default crossing threshold for projects that did not author one.
//
// Both halves were inert until issue #322. The config lived in a process-local
// struct, so a PUT returned 200, changed the GET, was lost on the next restart,
// and answered differently on every elitea-main replica; and no gateway read
// it at all. An operator who disabled soft alerts got a success response and
// alerts that kept firing.
//
// It is now one row in gateway.governance_config — the existing global
// authoring table — which the gateway's budget snapshot query joins on every
// /llm call. `enabled` gates the gateway's alert emission
// (llmproxy/budget_gate.go trySoftAlert); `threshold_pct` supplies the default
// where gateway.project_budget.soft_alert_pct is NULL, which migration 0084
// made representable by dropping that column's NOT NULL.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultSoftAlertThresholdPct is the budget-utilisation percentage at which a
// soft alert fires when neither the project nor the global config names one.
// Matches design §4.2, and the same literal in the gateway's snapshot query.
const DefaultSoftAlertThresholdPct = 80

// The coordinates of the single global row. gateway.governance_config's unique
// key is (section, type, name); these three values are also what migration 0084
// seeds and what the gateway's snapshot query filters on. All three must agree
// or the writer and the reader address different rows.
const (
	alertConfigSection = "governance"
	alertConfigType    = "budget_alert"
	alertConfigName    = "global"
)

// BudgetAlertConfig is the platform-wide soft-alert emission setting.
type BudgetAlertConfig struct {
	// Enabled toggles global soft-alert emission. When false the gateway keeps
	// billing and keeps enforcing budgets, and emits no budget.soft_alert event
	// and no soft-alert log line.
	Enabled bool `json:"enabled"`
	// ThresholdPct is the default budget-utilisation percentage (1..100) that
	// triggers a soft alert for projects whose gateway.project_budget row
	// carries no soft_alert_pct of its own.
	ThresholdPct int `json:"threshold_pct"`
}

// ErrNoPool reports that the store was built without a database pool. It is a
// wiring fault, surfaced as an error rather than a silent in-memory fallback:
// an in-memory fallback is exactly the defect #322 records, and it would look
// identical to a working surface from the outside.
var ErrNoPool = errors.New("gateway: budget alert store has no database pool")

// BudgetAlertStore reads and writes the global soft-alert config row.
type BudgetAlertStore struct {
	pool *pgxpool.Pool
}

// NewBudgetAlertStore builds a store over the shared pool.
func NewBudgetAlertStore(pool *pgxpool.Pool) *BudgetAlertStore {
	return &BudgetAlertStore{pool: pool}
}

// selectConfigSQL reads the global row. A deployment whose migration has not
// run, or whose row an operator deleted, returns no row and the caller answers
// with the shipped defaults — the same values the gateway falls back to.
const selectConfigSQL = `SELECT data FROM gateway.governance_config
	WHERE section = $1 AND type = $2 AND name = $3 AND enabled`

// upsertConfigSQL applies a PARTIAL update atomically.
//
// `data || $4::jsonb` merges only the keys the request supplied, in one
// statement, so two operators changing different fields at the same time cannot
// lose each other's write — which a read-modify-write in Go would, and which
// the in-process store did not even have to try to get right because it never
// persisted anything.
//
// The row's own `enabled` column is SET BACK TO TRUE on every write, and that
// is not tidying. It means "this governance_config row is live", not "alerts
// are on" — the alert switch is data->>'enabled'.
//
// The generic governance CRUD can write any row in this table, including this
// one, and can set enabled=false on it. Both readers filter on `AND enabled`.
// Without this line the surface reproduces #322 through the other door: Get
// finds no row and answers the shipped defaults, Update writes into the
// invisible row and returns the operator's new value from RETURNING, so the
// operator sees 200 OK and a changed GET while the gateway reads nothing. A
// write to this surface therefore asserts that the row it needs is live.
const upsertConfigSQL = `INSERT INTO gateway.governance_config
		(type, section, name, data, enabled)
	VALUES ($2, $1, $3, $4::jsonb, true)
	ON CONFLICT (section, type, name) DO UPDATE SET
		data = gateway.governance_config.data || $4::jsonb,
		enabled = true,
		updated_at = now()
	RETURNING data`

// storedConfig is the JSONB body. Both fields are pointers so a row that names
// only one of them is distinguishable from one that names it as zero.
type storedConfig struct {
	Enabled      *bool `json:"enabled,omitempty"`
	ThresholdPct *int  `json:"threshold_pct,omitempty"`
}

// resolve applies the shipped defaults to whatever the row supplied.
func (s storedConfig) resolve() BudgetAlertConfig {
	cfg := BudgetAlertConfig{Enabled: true, ThresholdPct: DefaultSoftAlertThresholdPct}
	if s.Enabled != nil {
		cfg.Enabled = *s.Enabled
	}
	if s.ThresholdPct != nil {
		cfg.ThresholdPct = *s.ThresholdPct
	}
	return cfg
}

// Get returns the persisted config, or the shipped defaults when no row exists.
func (s *BudgetAlertStore) Get(ctx context.Context) (BudgetAlertConfig, error) {
	if s == nil || s.pool == nil {
		return BudgetAlertConfig{}, ErrNoPool
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, selectConfigSQL,
		alertConfigSection, alertConfigType, alertConfigName).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedConfig{}.resolve(), nil
	}
	if err != nil {
		return BudgetAlertConfig{}, err
	}
	var stored storedConfig
	if err := json.Unmarshal(raw, &stored); err != nil {
		return BudgetAlertConfig{}, err
	}
	return stored.resolve(), nil
}

// Update applies a partial update and returns the resulting config.
func (s *BudgetAlertStore) Update(ctx context.Context, req BudgetAlertUpdateRequest) (BudgetAlertConfig, error) {
	if s == nil || s.pool == nil {
		return BudgetAlertConfig{}, ErrNoPool
	}
	// A conversion, not a field-by-field copy: the request body and the stored
	// JSONB are the same partial-config shape, and a conversion stops compiling
	// the day they stop being the same rather than silently dropping whichever
	// field the copy forgot.
	patch, err := json.Marshal(storedConfig(req))
	if err != nil {
		return BudgetAlertConfig{}, err
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, upsertConfigSQL,
		alertConfigSection, alertConfigType, alertConfigName, patch).Scan(&raw); err != nil {
		return BudgetAlertConfig{}, err
	}
	var stored storedConfig
	if err := json.Unmarshal(raw, &stored); err != nil {
		return BudgetAlertConfig{}, err
	}
	return stored.resolve(), nil
}

// BudgetAlertUpdateRequest is a partial update; omitted fields are left as-is.
type BudgetAlertUpdateRequest struct {
	Enabled      *bool `json:"enabled,omitempty"`
	ThresholdPct *int  `json:"threshold_pct,omitempty"`
}

// BudgetAlertHandler serves the global soft-alert control surface.
type BudgetAlertHandler struct {
	store *BudgetAlertStore
}

// NewBudgetAlertHandler wires a handler over the given store.
func NewBudgetAlertHandler(store *BudgetAlertStore) *BudgetAlertHandler {
	return &BudgetAlertHandler{store: store}
}

// Routes mounts the budget-alerts read/write endpoints. The caller is
// responsible for RBAC gating (RequirePermissions) — the authorization
// boundary is the server, not the client.
func (h *BudgetAlertHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/budget-alerts", h.Get)
	r.Put("/budget-alerts", h.Update)
	return r
}

// Get returns the current global soft-alert config.
func (h *BudgetAlertHandler) Get(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.store.Get(r.Context())
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read budget alert config")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// Update applies a partial update to the global soft-alert config.
//
// A failure to persist is a 500, not a 200 over a value only this replica
// holds. That distinction is the whole of issue #322: the surface must not
// report success for a policy it did not store.
func (h *BudgetAlertHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req BudgetAlertUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ThresholdPct != nil && (*req.ThresholdPct < 1 || *req.ThresholdPct > 100) {
		apierr.WriteStatus(w, http.StatusBadRequest, "threshold_pct must be between 1 and 100")
		return
	}
	cfg, err := h.store.Update(r.Context(), req)
	if err != nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to save budget alert config")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
