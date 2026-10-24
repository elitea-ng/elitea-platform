package gateway

// metered_prices.go — the admin price list of metered tool operations,
// `/api/v2/admin/gateway/metered-prices` (#1238).
//
// `gateway.metered_operation_prices` (shared migration 0174) prices one
// (provider, operation) pair — `tavily / web_search` — in one unit: a request,
// a credit or a page. It is a sibling of gateway.gateway_models rather than
// rows in it, because every gateway_models row is a selectable model priced
// in tokens.
//
// The gateway refuses an operation with no row (`metered_operation_unpriced`)
// before the provider is called, and a free operation has an explicit 0 row.
// So this surface has no "revert" verb: nothing price-syncs the table, a
// deleted row would refuse every call to that operation, and an operator who
// wants the seeded figure back writes it again. A write still marks the row
// overridden and records its author's user id, as llm_proxy_models.go does, so a reader
// can tell a seeded price from one an operator chose.
//
// Prices are USD per unit, at the column's precision (NUMERIC(20,8)).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The providers, operations and units the meter knows (contract §2). A row
// outside them would never be read, so a typo is refused rather than stored
// as a price nothing consults while the real operation stays unpriced.
var (
	meteredProviders  = map[string]bool{"tavily": true, "firecrawl": true, "brave": true, "ddg_instant": true, "direct": true, "shodan": true}
	meteredOperations = map[string]bool{"web_search": true, "web_search_advanced": true, "web_fetch": true, "host_search": true, "host_lookup": true}
	meteredUnits      = map[string]bool{"request": true, "credit": true, "page": true}
)

// maxMeteredPricePerUnit bounds one unit's price. The dearest seeded unit is
// under a cent; a figure above this is a slipped decimal point, and the
// meter would admit or refuse whole budgets on it.
const maxMeteredPricePerUnit = 1000.0

// maxMeteredPriceDecimals is the column's scale. A longer fraction would be
// rounded by Postgres while the response echoed the operator's figure.
const maxMeteredPriceDecimals = 8

// MeteredPriceRow is one priced operation as this surface returns it.
type MeteredPriceRow struct {
	Provider        string  `json:"provider"`
	Operation       string  `json:"operation"`
	Unit            string  `json:"unit"`
	PricePerUnitUSD float64 `json:"price_per_unit_usd"`
	Source          string  `json:"source,omitempty"`
	Overridden      bool    `json:"price_overridden"`
	OverriddenAt    string  `json:"price_overridden_at,omitempty"`
	OverriddenBy    string  `json:"price_overridden_by,omitempty"`
	UpdatedAt       string  `json:"updated_at,omitempty"`
}

// MeteredPriceWrite is the authored half of a row. The price is the JSON
// number as written, so it reaches NUMERIC without a float64 round trip; it
// is raw because json.Number would also take a quoted string.
type MeteredPriceWrite struct {
	Provider        string          `json:"provider"`
	Operation       string          `json:"operation"`
	Unit            string          `json:"unit"`
	PricePerUnitUSD json.RawMessage `json:"price_per_unit_usd"`
}

const listMeteredPricesSQL = `
	SELECT provider, operation, unit, price_per_unit_usd::float8, COALESCE(source, ''),
	       price_overridden, price_overridden_at, COALESCE(price_overridden_by::text, ''), updated_at
	  FROM gateway.metered_operation_prices
	 ORDER BY provider, operation`

// upsertMeteredPriceSQL writes an operator-authored price and marks it
// overridden. `source` keeps the seed's provenance on an update, as the model
// upsert keeps the upstream's; a row the operator adds is sourced 'admin'.
const upsertMeteredPriceSQL = `
	INSERT INTO gateway.metered_operation_prices (
		provider, operation, unit, price_per_unit_usd, source,
		price_overridden, price_overridden_at, price_overridden_by, updated_at
	) VALUES ($1, $2, $3, $4::numeric, 'admin', true, now(), $5, now())
	ON CONFLICT (provider, operation) DO UPDATE SET
		unit                = EXCLUDED.unit,
		price_per_unit_usd  = EXCLUDED.price_per_unit_usd,
		price_overridden    = true,
		price_overridden_at = now(),
		price_overridden_by = EXCLUDED.price_overridden_by,
		updated_at          = now()
	RETURNING price_per_unit_usd::float8`

// ListMeteredPrices serves GET /gateway/metered-prices.
func (h *LLMProxyHandler) ListMeteredPrices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), modelWriteTimeout)
	defer cancel()

	if h == nil || h.db == nil {
		writeError(w, http.StatusServiceUnavailable, "this deployment has no database pool, so the metered prices cannot be read.")
		return
	}
	rows, err := h.db.Query(ctx, listMeteredPricesSQL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the metered prices could not be read")
		return
	}
	defer rows.Close()
	prices := []MeteredPriceRow{}
	for rows.Next() {
		row, scanErr := scanMeteredPriceRow(rows)
		if scanErr != nil {
			writeError(w, http.StatusInternalServerError, "the metered prices could not be read")
			return
		}
		prices = append(prices, row)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "the metered prices could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": prices, "currency": "USD"})
}

func scanMeteredPriceRow(rows pgx.Rows) (MeteredPriceRow, error) {
	var (
		row          MeteredPriceRow
		overriddenAt *time.Time
		updatedAt    *time.Time
	)
	if err := rows.Scan(
		&row.Provider, &row.Operation, &row.Unit, &row.PricePerUnitUSD, &row.Source,
		&row.Overridden, &overriddenAt, &row.OverriddenBy, &updatedAt,
	); err != nil {
		return MeteredPriceRow{}, err
	}
	row.OverriddenAt = formatTime(overriddenAt)
	row.UpdatedAt = formatTime(updatedAt)
	return row, nil
}

// UpsertMeteredPrice serves PUT /gateway/metered-prices.
func (h *LLMProxyHandler) UpsertMeteredPrice(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), modelWriteTimeout)
	defer cancel()

	if h == nil || h.db == nil {
		writeError(w, http.StatusServiceUnavailable, "this deployment has no database pool, so the metered prices cannot be written.")
		return
	}
	body, ok := decodeMeteredPriceWrite(w, r)
	if !ok {
		return
	}
	// price_overridden_by is the author's user id (an INTEGER column, unlike
	// the model catalogue's free-text one); a principal with no numeric id
	// writes NULL rather than failing the save.
	var actor *int64
	actorLabel := ""
	if principal, ok := auth.UserFromContext(r.Context()); ok {
		if id, err := strconv.ParseInt(principal.ID, 10, 32); err == nil && id > 0 {
			actor, actorLabel = &id, principal.ID
		}
	}
	var stored float64
	if err := h.db.QueryRow(ctx, upsertMeteredPriceSQL,
		body.Provider, body.Operation, body.Unit, string(body.PricePerUnitUSD), actor,
	).Scan(&stored); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save the metered price")
		return
	}
	writeJSON(w, http.StatusOK, MeteredPriceRow{
		Provider:        body.Provider,
		Operation:       body.Operation,
		Unit:            body.Unit,
		PricePerUnitUSD: stored,
		Overridden:      true,
		OverriddenBy:    actorLabel,
	})
}

func decodeMeteredPriceWrite(w http.ResponseWriter, r *http.Request) (MeteredPriceWrite, bool) {
	var body MeteredPriceWrite
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return MeteredPriceWrite{}, false
	}
	body.Provider = strings.TrimSpace(body.Provider)
	body.Operation = strings.TrimSpace(body.Operation)
	body.Unit = strings.TrimSpace(body.Unit)
	if !meteredProviders[body.Provider] {
		writeError(w, http.StatusBadRequest, "provider is not a metered provider")
		return MeteredPriceWrite{}, false
	}
	if !meteredOperations[body.Operation] {
		writeError(w, http.StatusBadRequest, "operation is not a metered operation")
		return MeteredPriceWrite{}, false
	}
	if !meteredUnits[body.Unit] {
		writeError(w, http.StatusBadRequest, "unit must be request, credit or page")
		return MeteredPriceWrite{}, false
	}
	// The price is required, and zero is a real price: a free operation
	// bills an explicit $0 and still lands in the ledger.
	raw := string(body.PricePerUnitUSD)
	price, err := strconv.ParseFloat(raw, 64)
	if raw == "" || raw[0] == '"' || err != nil {
		writeError(w, http.StatusBadRequest, "price_per_unit_usd is required")
		return MeteredPriceWrite{}, false
	}
	if price < 0 {
		writeError(w, http.StatusBadRequest, "a price cannot be negative")
		return MeteredPriceWrite{}, false
	}
	if price > maxMeteredPricePerUnit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("price_per_unit_usd must be %g or less", maxMeteredPricePerUnit))
		return MeteredPriceWrite{}, false
	}
	if strings.ContainsAny(raw, "eE") || fractionDigits(raw) > maxMeteredPriceDecimals {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("price_per_unit_usd takes at most %d decimal places", maxMeteredPriceDecimals))
		return MeteredPriceWrite{}, false
	}
	return body, true
}

func fractionDigits(raw string) int {
	if dot := strings.IndexByte(raw, '.'); dot >= 0 {
		return len(raw) - dot - 1
	}
	return 0
}
