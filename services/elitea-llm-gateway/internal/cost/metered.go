// metered.go — the price of a metered tool operation (#1238): a web search, a
// page fetch, a host query.
//
// WHY A SIBLING TABLE. Every gateway.gateway_models row is a MODEL to the rest
// of the platform: /llm/v1/models lists it, the admin catalogue shows it,
// credential linking and the price sync read it, and the analytics re-price
// tokens by joining it. A `tavily / web_search` row there would be a
// selectable model priced in tokens. gateway.metered_operation_prices (shared
// migration 0174) holds one price per (provider, operation) in one unit:
// request, credit or page.
//
// NO SILENT ZERO. An operation with no row is UNPRICED, and the caller refuses
// it before the provider is called. There is no default table here, unlike the
// token path: a free operation has an explicit 0 row, so "free" and "nobody
// priced it" can never look the same.
package cost

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
)

// The units a metered price can be denominated in. They match the CHECK on
// gateway.metered_operation_prices.unit.
const (
	MeteredUnitRequest = "request"
	MeteredUnitCredit  = "credit"
	MeteredUnitPage    = "page"
)

// meteredPriceSQL reads one operation's price and scales it to nano-USD in
// SQL: NUMERIC(20,8) × 1e9 is an integer, so the ::bigint cast is exact.
//
// $1 provider, $2 operation, $3 NanoUSD scale factor.
const meteredPriceSQL = `SELECT unit, (price_per_unit_usd * $3::numeric)::bigint
	FROM gateway.metered_operation_prices
	WHERE provider = $1 AND operation = $2`

// MeteredPrice is the price of one unit of a metered operation.
type MeteredPrice struct {
	// Unit is MeteredUnitRequest, MeteredUnitCredit or MeteredUnitPage.
	Unit string
	// NanoUSDPerUnit is the price of one unit, in nano-USD. Zero is a real,
	// explicit price.
	NanoUSDPerUnit int64
}

// MeteredUnits are the units a metered operation consumed, as the provider
// reported them where it does, else as computed from the request.
type MeteredUnits struct {
	Requests int64 `json:"requests"`
	Credits  int64 `json:"credits"`
	Pages    int64 `json:"pages"`
}

// Of returns the count in unit; 0 for an unknown unit or a negative count.
func (u MeteredUnits) Of(unit string) int64 {
	var n int64
	switch unit {
	case MeteredUnitRequest:
		n = u.Requests
	case MeteredUnitCredit:
		n = u.Credits
	case MeteredUnitPage:
		n = u.Pages
	}
	return max(n, 0)
}

// MeteredCost is a priced metered operation.
type MeteredCost struct {
	Price MeteredPrice
	// Units is the count billed, in Price.Unit.
	Units int64
	// TotalNanoUSD is Units × Price.NanoUSDPerUnit.
	TotalNanoUSD int64
}

type meteredEntry struct {
	price   MeteredPrice
	priced  bool
	expires int64
}

// MeteredPrice resolves (provider, operation) through the price cache, then
// the table. ok is false when the operation is unpriced: no row, no database,
// or a read that failed. A failed read is not cached, so the next call retries
// it; an absent row is, so an operator's new price is seen within the TTL.
func (c *Calculator) MeteredPrice(ctx context.Context, provider, operation string) (MeteredPrice, bool) {
	key := provider + "\x00" + operation
	now := c.now().UnixNano()
	c.mu.RLock()
	entry, hit := c.metered[key]
	c.mu.RUnlock()
	if hit && now < entry.expires {
		return entry.price, entry.priced
	}
	if c.db == nil {
		return MeteredPrice{}, false
	}
	var price MeteredPrice
	err := c.db.QueryRow(ctx, meteredPriceSQL, provider, operation, NanoUSD).Scan(&price.Unit, &price.NanoUSDPerUnit)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		c.storeMetered(key, meteredEntry{expires: c.now().Add(c.ttl).UnixNano()})
		return MeteredPrice{}, false
	default:
		c.logger.Warn("cost: metered price lookup failed; the operation is refused as unpriced",
			"provider", provider, "operation", operation, "err", err)
		return MeteredPrice{}, false
	}
	if price.NanoUSDPerUnit < 0 {
		return MeteredPrice{}, false
	}
	c.storeMetered(key, meteredEntry{price: price, priced: true, expires: c.now().Add(c.ttl).UnixNano()})
	return price, true
}

func (c *Calculator) storeMetered(key string, entry meteredEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metered == nil {
		c.metered = make(map[string]meteredEntry)
	}
	c.metered[key] = entry
}

// CostMetered prices units of (provider, operation) in integers. ok is false
// when the operation is unpriced or the product overflows int64.
func (c *Calculator) CostMetered(ctx context.Context, provider, operation string, units MeteredUnits) (MeteredCost, bool) {
	price, ok := c.MeteredPrice(ctx, provider, operation)
	if !ok {
		return MeteredCost{}, false
	}
	return PriceMetered(price, units)
}

// PriceMetered multiplies the units in price.Unit by the unit price. ok is
// false when the product overflows int64.
func PriceMetered(price MeteredPrice, units MeteredUnits) (MeteredCost, bool) {
	n := units.Of(price.Unit)
	if price.NanoUSDPerUnit != 0 && n > math.MaxInt64/price.NanoUSDPerUnit {
		return MeteredCost{}, false
	}
	return MeteredCost{Price: price, Units: n, TotalNanoUSD: n * price.NanoUSDPerUnit}, true
}
