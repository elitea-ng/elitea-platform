package gateway

// llm_proxy_models.go — the admin model catalogue, `/api/v2/admin/gateway/models`.
//
// ## What this table is, and why an admin needs to see it
//
// `gateway.gateway_models` is the cost basis for every billed LLM request. The
// gateway prices a call by looking its (provider, model_name) pair up here and
// multiplying by the quantities the provider reported.
//
// A pair with NO ROW is not refused, and — for a token model — is not free
// either. `internal/cost` falls back to a legacy-parity prefix table and,
// failing that, to a flat 1.0/3.0 USD per 1M tokens (`cost.go` Price,
// `default_prices.go` defaultPrice). So the call IS billed, the budget counter
// IS incremented, and a ceiling CAN stop it — but every one of those numbers is
// an invention. The failure is silent and it is a WRONG BILL, not a missing one,
// which is harder to notice and harder to reconstruct after the fact.
//
// Audio is the exception and it is the stricter one. A per-second or
// per-character rate is never fabricated, deliberately: an invented figure for a
// second of speech would land on the same counter the budget gate reads back.
// A model with no catalogue audio rate is genuinely UNPRICED — billed zero,
// counted on `gateway_audio_unpriced_total`, and stoppable by no budget at all.
//
// Both cases are the same operator action: put a row here. They are reported
// differently because the consequence differs, and an operator who is told
// "billed at zero" about a token model will go looking for missing spend that
// is in fact present and wrong.
//
// The table is populated by the scheduler's price-sync worker from public
// sources. Nothing has ever been able to read it from an admin screen, and
// nothing has ever been able to correct it. Both are this file.
//
// ## Prices are per 1M tokens, everywhere, without exception
//
// Every price column here is denominated per 1M tokens (per 1M seconds or
// characters for the audio columns 0086 added). The gateway's cost calculator
// divides by the same 1M. Accepting a per-1k number into one of these fields is
// a 1000x costing error that no test would catch and that design-bifrost-gateway
// §7.3 exists to warn about, so the field labels state the denomination and
// this file never converts.
//
// ## An override is a real override
//
// Writing a price here would be pointless on its own: price-sync UPSERTs on the
// same unique key and its DO UPDATE reassigns every price column. Shared
// migration 0097 adds `price_overridden`, and the syncer's DO UPDATE now
// carries `WHERE NOT gateway_models.price_overridden`. So a row this surface
// authors is one the sync declines to touch, and a row it has not authored
// continues to track upstream exactly as before.
//
// That is also why there is a `DELETE` and why it is described as "revert":
// clearing the override does not delete the price, it hands the row back to the
// sync, which will refresh it on its next tick.
//
// ## Usage is reported beside the catalogue, from a different table
//
// `gateway.llm_usage_events` holds one row per billed request — provider,
// model, tokens, `api_requests`, `cost_usd`, `occurred_at`. Joining it to the
// catalogue answers the question that makes the catalogue actionable: which
// models are actually being called, and is the one costing the most priced at
// all. It is deliberately a REPORT and never a second source of truth: no
// budget decision reads it, and this surface does not write it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// actorFrom names who authored an override, for the row's provenance columns.
//
// It is recorded, never trusted: the authorization boundary is the
// RequireCentralPermissions wrapper this surface is mounted under, and an empty
// string here changes nothing about whether the write is allowed. Email first
// because that is what an operator reading an audit answer recognises, falling
// back to the principal id when the token carries no email.
func actorFrom(r *http.Request) string {
	principal, _ := auth.UserFromContext(r.Context())
	if principal.Email != "" {
		return principal.Email
	}
	return principal.ID
}

// decodeJSONBody reads a JSON request body, refusing unknown fields.
//
// DisallowUnknownFields is the point of having this helper rather than calling
// the decoder inline. A price field misspelled by the client would otherwise be
// dropped in silence and the row stored with that price null — a model priced
// at zero for one of its dimensions, saved with a success response.
func decodeJSONBody(r *http.Request, target any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

// LLMProxyQuerier is the minimal subset of *pgxpool.Pool this surface needs,
// narrowed to an interface so tests substitute a fake without a live database —
// the same seam governanceQuerier uses.
//
// Exported so the composition root can declare a variable of this type and
// assign the pool only when it is non-nil. A nil *pgxpool.Pool boxed into this
// interface is not nil, which would defeat every `h.db == nil` check below.
type LLMProxyQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// modelWriteTimeout bounds a catalogue write. A write touches one row by unique
// key, so a slow one is a database in trouble rather than a large result.
const modelWriteTimeout = 5 * time.Second

// modelReadTimeout bounds a catalogue read, and is deliberately larger than the
// write budget.
//
// A read aggregates every gateway.llm_usage_events row inside the window: at
// ?window=30d on a deployment that writes one row per billed request, that is
// millions of rows, served by the (occurred_at) index alone. Five seconds is a
// realistic figure for a small deployment and an unrealistic one for a large,
// and the failure it produces is the worst kind for this screen — the pricing
// data becomes unreadable on exactly the deployments where mispricing costs
// most, reported as "the catalogue could not be read".
const modelReadTimeout = 20 * time.Second

// usageWindows are the reporting windows this surface offers, mapped to their
// interval. A closed set rather than a free-form duration: the value reaches a
// SQL interval, and an allowlist keeps that a lookup rather than a parse.
//
// They match what the catalogue is used for — "what is running right now" (24h),
// "what did this week cost" (7d), and the billing-shaped 30d. Both statements
// below filter on `occurred_at` alone with no project predicate, so the index
// that serves them is `idx_llm_usage_events_occurred_at` (migration 0084); the
// (project_id, occurred_at) index cannot, its leading column being unconstrained
// here.
var usageWindows = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// defaultUsageWindow is what an absent or unrecognised ?window= resolves to. An
// unrecognised value is NOT an error: the window is a display choice, and
// refusing the whole catalogue because a query string was mistyped would hide
// the pricing data over a reporting preference.
const defaultUsageWindow = "24h"

// ModelPrices carries the price columns the gateway's cost path actually reads.
//
// These six, and no others. `internal/cost`'s catalogue statement (`modelPriceSQL`)
// selects exactly `input/output_cost_per_1m_tokens`, `..._per_1m_seconds` and
// `..._per_1m_characters`. The table has three more price columns —
// `cache_creation_input_token_cost`, `cache_read_input_token_cost` and
// `input_cost_per_1m_tokens_above_128k` — that the price sync writes and that
// NOTHING in this platform reads.
//
// They are deliberately absent from this type rather than offered and
// annotated. Offering them would put a control on an admin screen whose value
// changes no bill, and — worse — would let an operator satisfy the
// "at least one price" rule with a column the cost path never consults, pinning
// the row off the sync while leaving it billed at the fabricated default. The
// upsert therefore never names those columns either, so a value the sync wrote
// into one of them survives an override untouched.
//
// Every field is a pointer because NULL is meaningful and distinct from zero: a
// NULL per-second rate means the model has no audio rate at all (and audio calls
// to it are UNPRICED), while 0 means audio is free. Collapsing them would
// silently invent a price for one and erase a real one for the other.
type ModelPrices struct {
	InputPer1MTokens      *float64 `json:"input_cost_per_1m_tokens"`
	OutputPer1MTokens     *float64 `json:"output_cost_per_1m_tokens"`
	InputPer1MSeconds     *float64 `json:"input_cost_per_1m_seconds"`
	OutputPer1MSeconds    *float64 `json:"output_cost_per_1m_seconds"`
	InputPer1MCharacters  *float64 `json:"input_cost_per_1m_characters"`
	OutputPer1MCharacters *float64 `json:"output_cost_per_1m_characters"`
}

// ModelRow is one catalogue entry as this surface returns it: the stored price
// row, its provenance, and the usage observed in the requested window.
type ModelRow struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	ModelName string `json:"model_name"`

	ModelPrices

	// Source and SourceSyncedAt name which upstream priced the row and when.
	// They are the answer to "where did this number come from", which is the
	// first question asked of a price that looks wrong.
	Source         string `json:"source,omitempty"`
	SourceSyncedAt string `json:"source_synced_at,omitempty"`
	LastSyncAt     string `json:"last_sync_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`

	// Overridden reports that an operator authored these prices and the sync
	// leaves them alone (migration 0097).
	Overridden   bool   `json:"price_overridden"`
	OverriddenAt string `json:"price_overridden_at,omitempty"`
	OverriddenBy string `json:"price_overridden_by,omitempty"`

	// Usage over the requested window. Zero for a catalogued model nobody
	// called, which is a legitimate and common state rather than missing data.
	Requests    int64   `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// UnpricedModel is a (provider, model) pair that was CALLED in the window but
// has no catalogue row.
//
// This is the finding the catalogue screen exists to surface, and what it costs
// depends on what kind of call it was:
//
//   - **Token calls are billed at an invented rate.** `internal/cost` falls back
//     to a prefix table and then to a flat 1.0/3.0 USD per 1M. The spend is
//     recorded and the budget counter moves, so nothing looks broken — the
//     figures are simply wrong, in an unknown direction, for as long as the row
//     is missing.
//   - **Audio calls are billed at zero.** A per-second or per-character rate is
//     never fabricated (`Price.AudioFromCatalog`), so the call consumes a real
//     provider quota, contributes nothing to any counter, and no ceiling can
//     stop it.
//
// It is reported separately from the catalogue rather than as a row with null
// prices, because it is not a catalogue entry at all — it is a gap in the
// catalogue, and the action it calls for is "add a price", not "edit this one".
type UnpricedModel struct {
	Provider    string  `json:"provider"`
	ModelName   string  `json:"model_name"`
	Requests    int64   `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// ModelWrite is the authored half of a catalogue row. Provider and model name
// identify it; the prices are what the operator is asserting.
type ModelWrite struct {
	Provider  string `json:"provider"`
	ModelName string `json:"model_name"`
	ModelPrices
}

// LLMProxyHandler serves the admin LLM Proxy surface: gateway enforcement
// status, the model price catalogue, and the usage rollup beside it.
//
// The caller is responsible for RBAC gating — the authorization boundary is the
// server-side RequireCentralPermissions wrapper in router.go, never a client
// check. `status` may be nil (a deployment with no gateway address), and every
// route reports that posture rather than failing.
type LLMProxyHandler struct {
	db     LLMProxyQuerier
	status StatusReader
}

// NewLLMProxyHandler wires a handler over the given querier and status reader.
// Both may be nil; the routes report the resulting posture.
func NewLLMProxyHandler(db LLMProxyQuerier, status StatusReader) *LLMProxyHandler {
	return &LLMProxyHandler{db: db, status: status}
}

// Register attaches the LLM Proxy endpoints to an existing router, alongside
// the governance CRUD under the same `/gateway` prefix and RBAC group.
func (h *LLMProxyHandler) Register(r chi.Router) {
	r.Get("/status", h.Status)
	r.Get("/models", h.ListModels)
	r.Get("/usage", h.Usage)
	r.Get("/logs", h.Logs)
	r.Put("/models", h.UpsertModel)
	r.Delete("/models/{id}", h.ClearModelOverride)
	r.Get("/metered-prices", h.ListMeteredPrices)
	r.Put("/metered-prices", h.UpsertMeteredPrice)
}

// Routes returns a standalone router mounting the same endpoints — used in tests.
func (h *LLMProxyHandler) Routes() chi.Router {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

// resolveWindow maps ?window= onto an interval, falling back to the default.
func resolveWindow(raw string) (string, time.Duration) {
	if d, ok := usageWindows[raw]; ok {
		return raw, d
	}
	return defaultUsageWindow, usageWindows[defaultUsageWindow]
}

// maxCatalogueRows bounds one catalogue page.
//
// The price sync ingests LiteLLM's whole price sheet — on the order of 1800
// entries — so an unbounded read serialises hundreds of kilobytes and the client
// mounts a table row for every one of them. The cap is paired with `?q=`
// (below) and with a `truncated` flag, so a bounded response is never a silently
// short one: an operator who cannot see the model they want narrows the search
// rather than scrolling a list that was cut off without saying so.
const maxCatalogueRows = 200

// listModelsSQL reads one page of the catalogue with the window's usage folded
// in, optionally narrowed by a search term.
//
// $2 is the search term, already lowercased and wrapped in %; the empty-string
// case is handled by the `$2 = ”` short-circuit rather than by a second
// statement, so there is one query plan to reason about.
//
// $3 is the row cap. It is applied AFTER the ORDER BY, so the page is the first
// N by (provider, model_name) rather than an arbitrary N.
//
// A LEFT JOIN onto a pre-aggregated usage subquery, not a join onto the raw
// events: aggregating first keeps the row count at one per catalogue entry,
// where joining the events directly would multiply each catalogue row by its
// event count and then need a DISTINCT to undo it.
//
// COALESCE on every usage column so an uncalled model reports 0 rather than
// NULL. The scan targets are not pointers, and a NULL arriving there is a scan
// error that would drop the row — losing exactly the catalogue entries nobody
// has called, which are the ones an operator is most likely to be auditing.
const listModelsSQL = `
	SELECT m.id::text, m.provider, m.model_name,
	       m.input_cost_per_1m_tokens, m.output_cost_per_1m_tokens,
	       m.input_cost_per_1m_seconds, m.output_cost_per_1m_seconds,
	       m.input_cost_per_1m_characters, m.output_cost_per_1m_characters,
	       COALESCE(m.source, ''), m.source_synced_at, m.last_sync_at, m.updated_at,
	       m.price_overridden, m.price_overridden_at, COALESCE(m.price_overridden_by, ''),
	       COALESCE(u.requests, 0), COALESCE(u.total_tokens, 0), COALESCE(u.cost_usd, 0)
	  FROM gateway.gateway_models m
	  LEFT JOIN (
	        SELECT provider, model,
	               SUM(api_requests) AS requests,
	               SUM(total_tokens) AS total_tokens,
	               SUM(cost_usd)     AS cost_usd
	          FROM gateway.llm_usage_events
	         WHERE occurred_at >= now() - $1::interval
	         GROUP BY provider, model
	  ) u ON u.provider = m.provider AND u.model = m.model_name
	 WHERE $2 = '' OR lower(m.model_name) LIKE $2 OR lower(m.provider) LIKE $2
	 ORDER BY m.provider, m.model_name
	 LIMIT $3`

// unpricedModelsSQL finds pairs that were called in the window and have no
// catalogue row. The NOT EXISTS is against the catalogue rather than an anti-join
// so the absence is stated directly, in the terms the finding is reported in.
const unpricedModelsSQL = `
	SELECT e.provider, e.model,
	       SUM(e.api_requests) AS requests,
	       SUM(e.total_tokens) AS total_tokens,
	       SUM(e.cost_usd)     AS cost_usd
	  FROM gateway.llm_usage_events e
	 WHERE e.occurred_at >= now() - $1::interval
	   AND NOT EXISTS (
	         SELECT 1 FROM gateway.gateway_models m
	          WHERE m.provider = e.provider AND m.model_name = e.model
	       )
	 GROUP BY e.provider, e.model
	 ORDER BY requests DESC`

// ListModels serves GET /gateway/models.
//
// A read failure returns an empty catalogue with the reason attached rather
// than a 5xx, matching the governance List beside it: the screen renders an
// explained empty state instead of a generic failure, and an operator can still
// reach the rest of the section.
func (h *LLMProxyHandler) ListModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), modelReadTimeout)
	defer cancel()

	window, interval := resolveWindow(r.URL.Query().Get("window"))
	pgInterval := strconv.FormatInt(int64(interval/time.Second), 10) + " seconds"
	search := searchPattern(r.URL.Query().Get("q"))

	if h == nil || h.db == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []ModelRow{}, "unpriced": []UnpricedModel{}, "window": window,
			"error": "this deployment has no database pool, so the model catalogue cannot be read.",
		})
		return
	}

	items, err := h.queryModels(ctx, pgInterval, search)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []ModelRow{}, "unpriced": []UnpricedModel{}, "window": window,
			"error": err.Error(),
		})
		return
	}

	// The unpriced report is a SEPARATE query, and its failure is reported
	// separately rather than discarded.
	//
	// Swallowing it was a real defect: an empty list renders as no alert at all,
	// so a query that failed looked exactly like a deployment where every called
	// model is priced — the one conclusion this panel exists to prevent an
	// operator from reaching by accident. The catalogue is still worth showing
	// when this fails, so it is a field beside the data rather than a refusal of
	// the whole read.
	unpriced, unpricedErr := h.queryUnpriced(ctx, pgInterval)
	if unpriced == nil {
		unpriced = []UnpricedModel{}
	}

	body := map[string]any{
		"items":    items,
		"unpriced": unpriced,
		"window":   window,
		"total":    len(items),
		// `truncated` says the catalogue was capped, so a short list is never
		// mistaken for a complete one. The client turns it into a prompt to
		// search rather than into an error.
		"truncated": len(items) >= maxCatalogueRows,
	}
	if unpricedErr != nil {
		body["unpriced_error"] = unpricedErr.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// likeEscaper neutralises the LIKE metacharacters. Backslash is PostgreSQL's
// default LIKE escape character, so it must itself be doubled first — declared
// once here rather than built per request.
var likeEscaper = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)

// searchPattern turns a raw ?q= into the LIKE pattern listModelsSQL expects, or
// "" for no filter. Lowercased here so the statement can compare against
// lower(...) on both columns, and the wildcards are added here so the SQL never
// concatenates user input.
func searchPattern(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// The LIKE metacharacters are escaped: a model name legitimately contains
	// neither, but a search for "gpt_4" must not silently match "gpt-4".
	escaped := likeEscaper.Replace(strings.ToLower(trimmed))
	return "%" + escaped + "%"
}

func (h *LLMProxyHandler) queryModels(ctx context.Context, pgInterval, search string) ([]ModelRow, error) {
	rows, err := h.db.Query(ctx, listModelsSQL, pgInterval, search, maxCatalogueRows)
	if err != nil {
		return nil, fmt.Errorf("read model catalogue: %w", err)
	}
	defer rows.Close()

	items := make([]ModelRow, 0)
	for rows.Next() {
		row, scanErr := scanModelRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("read model catalogue: %w", scanErr)
		}
		items = append(items, row)
	}
	return items, rows.Err()
}

func (h *LLMProxyHandler) queryUnpriced(ctx context.Context, pgInterval string) ([]UnpricedModel, error) {
	rows, err := h.db.Query(ctx, unpricedModelsSQL, pgInterval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UnpricedModel, 0)
	for rows.Next() {
		var m UnpricedModel
		if err := rows.Scan(&m.Provider, &m.ModelName, &m.Requests, &m.TotalTokens, &m.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanModelRow reads one catalogue row. Timestamps are scanned through
// *time.Time because every one of them is nullable, and formatted RFC3339 so
// the client renders a single, unambiguous shape.
func scanModelRow(rows pgx.Rows) (ModelRow, error) {
	var (
		m                                               ModelRow
		sourceSynced, lastSync, updatedAt, overriddenAt *time.Time
	)
	if err := rows.Scan(
		&m.ID, &m.Provider, &m.ModelName,
		&m.InputPer1MTokens, &m.OutputPer1MTokens,
		&m.InputPer1MSeconds, &m.OutputPer1MSeconds,
		&m.InputPer1MCharacters, &m.OutputPer1MCharacters,
		&m.Source, &sourceSynced, &lastSync, &updatedAt,
		&m.Overridden, &overriddenAt, &m.OverriddenBy,
		&m.Requests, &m.TotalTokens, &m.CostUSD,
	); err != nil {
		return ModelRow{}, err
	}
	m.SourceSyncedAt = formatTime(sourceSynced)
	m.LastSyncAt = formatTime(lastSync)
	m.UpdatedAt = formatTime(updatedAt)
	m.OverriddenAt = formatTime(overriddenAt)
	return m, nil
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// upsertModelSQL writes an operator-authored price and marks it overridden.
//
// It is a single statement keyed on the same (provider, model_name) unique
// constraint the sync uses, so authoring a price for a model the catalogue
// already holds and one it has never seen are the same operation. Unlike the
// sync's version this one carries NO guard: the operator IS the override, and
// re-authoring a price they already authored must be allowed.
//
// `source` is deliberately left untouched. It records which UPSTREAM priced the
// row, and overwriting it with something like 'admin' would destroy the
// provenance of the number being replaced — which is the context needed to
// judge whether the override is still wanted after the upstream moves.
const upsertModelSQL = `
	INSERT INTO gateway.gateway_models (
		provider, model_name,
		input_cost_per_1m_tokens, output_cost_per_1m_tokens,
		input_cost_per_1m_seconds, output_cost_per_1m_seconds,
		input_cost_per_1m_characters, output_cost_per_1m_characters,
		price_overridden, price_overridden_at, price_overridden_by, updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, now(), $9, now())
	ON CONFLICT (provider, model_name) DO UPDATE SET
		input_cost_per_1m_tokens            = EXCLUDED.input_cost_per_1m_tokens,
		output_cost_per_1m_tokens           = EXCLUDED.output_cost_per_1m_tokens,
		input_cost_per_1m_seconds           = EXCLUDED.input_cost_per_1m_seconds,
		output_cost_per_1m_seconds          = EXCLUDED.output_cost_per_1m_seconds,
		input_cost_per_1m_characters        = EXCLUDED.input_cost_per_1m_characters,
		output_cost_per_1m_characters       = EXCLUDED.output_cost_per_1m_characters,
		price_overridden                    = true,
		price_overridden_at                 = now(),
		price_overridden_by                 = EXCLUDED.price_overridden_by,
		updated_at                          = now()
	RETURNING id::text`

// maxModelIdentifier bounds provider and model name to the column widths
// (VARCHAR(64) and VARCHAR(128)). Checked here so an over-long value is a
// stated refusal rather than a Postgres error surfaced as a 500.
const (
	maxProviderLen  = 64
	maxModelNameLen = 128
)

// invalidTextRepresentation is Postgres 22P02, raised when a value cannot be
// cast to the target type — here, a malformed uuid reaching `$1::uuid`. Spelled
// as the literal code, matching isUniqueViolation in governance.go beside it.
const invalidTextRepresentation = "22P02"

// UpsertModel serves PUT /gateway/models — author a price override.
func (h *LLMProxyHandler) UpsertModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), modelWriteTimeout)
	defer cancel()

	if h == nil || h.db == nil {
		writeError(w, http.StatusServiceUnavailable, "this deployment has no database pool, so the model catalogue cannot be written.")
		return
	}

	body, ok := decodeModelWrite(w, r)
	if !ok {
		return
	}

	var id string
	err := h.db.QueryRow(ctx, upsertModelSQL,
		body.Provider, body.ModelName,
		body.InputPer1MTokens, body.OutputPer1MTokens,
		body.InputPer1MSeconds, body.OutputPer1MSeconds,
		body.InputPer1MCharacters, body.OutputPer1MCharacters,
		actorFrom(r),
	).Scan(&id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save the model price")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id,
		"provider":         body.Provider,
		"model_name":       body.ModelName,
		"price_overridden": true,
	})
}

// clearOverrideSQL hands a row back to the price sync.
//
// It clears the FLAG and leaves the prices standing. Deleting the row instead
// would take effect immediately and bill every call to that model at zero until
// the next sync tick — turning a "revert to upstream" click into an unpriced
// window. Clearing the flag means the currently-stored numbers keep applying
// until the sync replaces them, which is the behaviour the word "revert"
// promises.
const clearOverrideSQL = `
	UPDATE gateway.gateway_models
	   SET price_overridden = false, price_overridden_at = NULL, price_overridden_by = NULL,
	       updated_at = now()
	 WHERE id = $1::uuid
	   AND price_overridden`

// ClearModelOverride serves DELETE /gateway/models/{id}.
//
// The name says what it does. It is mounted on DELETE because that is the verb
// the row editor's remove action sends, but nothing is deleted: see
// clearOverrideSQL.
func (h *LLMProxyHandler) ClearModelOverride(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), modelWriteTimeout)
	defer cancel()

	if h == nil || h.db == nil {
		writeError(w, http.StatusServiceUnavailable, "this deployment has no database pool, so the model catalogue cannot be written.")
		return
	}

	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "a model id is required")
		return
	}

	tag, err := h.db.Exec(ctx, clearOverrideSQL, id)
	if err != nil {
		// A malformed id reaches `$1::uuid` and Postgres raises 22P02. That is
		// the caller's input, not a server fault, and reporting it as a 500
		// would send an operator to look at the database.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation {
			writeError(w, http.StatusBadRequest, "that is not a valid model id")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to clear the price override")
		return
	}
	// A zero row count means the id matched nothing, or matched a row that was
	// never overridden. Both are reported as 404 rather than as a silent
	// success: a success toast for a click that changed nothing is how an
	// operator concludes the sync will now refresh a row it will keep skipping.
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "no overridden model with that id")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "price_overridden": false})
}

// errNoPrices reports a write that named a model but asserted no price at all.
var errNoPrices = errors.New("at least one price must be set")

// decodeModelWrite reads and validates a price-override body.
func decodeModelWrite(w http.ResponseWriter, r *http.Request) (ModelWrite, bool) {
	var body ModelWrite
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return ModelWrite{}, false
	}

	body.Provider = strings.TrimSpace(body.Provider)
	body.ModelName = strings.TrimSpace(body.ModelName)

	if body.Provider == "" || body.ModelName == "" {
		writeError(w, http.StatusBadRequest, "provider and model_name are required")
		return ModelWrite{}, false
	}
	if len(body.Provider) > maxProviderLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("provider must be %d characters or fewer", maxProviderLen))
		return ModelWrite{}, false
	}
	if len(body.ModelName) > maxModelNameLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model_name must be %d characters or fewer", maxModelNameLen))
		return ModelWrite{}, false
	}

	// A negative price is refused rather than stored. It would not error
	// anywhere — the cost calculator would multiply it out and produce a
	// NEGATIVE spend, which subtracts from the project's accumulated cost and
	// makes every budget ceiling further away the more the model is used.
	for _, p := range []*float64{
		body.InputPer1MTokens, body.OutputPer1MTokens,
		body.InputPer1MSeconds, body.OutputPer1MSeconds,
		body.InputPer1MCharacters, body.OutputPer1MCharacters,
	} {
		if p != nil && *p < 0 {
			writeError(w, http.StatusBadRequest, "a price cannot be negative")
			return ModelWrite{}, false
		}
	}

	if !body.hasAnyPrice() {
		writeError(w, http.StatusBadRequest, errNoPrices.Error())
		return ModelWrite{}, false
	}
	return body, true
}

// hasAnyPrice reports whether the write asserts at least one price the cost path
// will actually read.
//
// An override with every field null is refused. It would mark the row
// overridden — permanently excluding it from the price sync — while pricing
// nothing, so the model would keep billing at the fabricated default forever and
// the sync that would have fixed it is precisely what the flag now prevents.
// That is the worst outcome this surface can produce, and it is one empty form
// away.
//
// The check is meaningful only because ModelPrices carries exactly the columns
// `internal/cost` consults. When this type also held `cache_read_input_token_cost`
// and its two unread neighbours, an operator could satisfy this rule with one of
// them and reach that same outcome while the form reported success — which is
// why those columns are not on this surface at all.
func (m ModelWrite) hasAnyPrice() bool {
	for _, p := range []*float64{
		m.InputPer1MTokens, m.OutputPer1MTokens,
		m.InputPer1MSeconds, m.OutputPer1MSeconds,
		m.InputPer1MCharacters, m.OutputPer1MCharacters,
	} {
		if p != nil {
			return true
		}
	}
	return false
}
