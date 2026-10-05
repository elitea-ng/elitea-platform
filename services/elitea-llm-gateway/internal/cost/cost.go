// Package cost computes per-request LLM cost in int64 nano-USD for the
// governance PostLLMHook path (design §8.7, §5.1, §8.8).
//
// Two denominations meet here and MUST NOT be confused (design §5.1):
//
//   - Model price — USD per 1,000,000 tokens, stored in gateway.gateway_models
//     (input_cost_per_1m_tokens / output_cost_per_1m_tokens). This is the cost
//     BASIS, written by the price-sync service.
//   - Budget counter — int64 nano-USD (×1e9, NanoUSD), incremented via Nats-Incr
//     after each response.
//
// The load-bearing invariant (§8.8, guards the 1000× / 1e6 costing bug): prices
// are per-1M tokens. The pylon reference (gateway_analytics.CostCalculator)
// computes cost as (tokens / 1_000_000) * price_per_1M in USD; this package
// produces the exact int64 nano-USD equivalent so the two agree end-to-end
// (pre-flight gate BFF.2 / spec §7 s8):
//
//	costNano = round( tokens * priceNanoPer1M / TokensPer1M )
//
// where priceNanoPer1M = price_per_1M_usd * NanoUSD. The multiply is done in
// math/big so a large token count cannot overflow int64 before the divide.
//
// The package reads prices from Postgres (source of truth) with a short
// in-process TTL cache on the hot path (§8.8, line 448), and falls back to the
// same default price table pylon uses (identical values, identical ordered
// prefix match) so an un-catalogued model is priced consistently with the
// legacy path rather than dropped.
//
// THREE BASES, ONE DIVISOR RULE (issue #323, the audio routes). A model is not
// always sold by the token. whisper-1 is sold by the second. Some text-to-speech
// models are sold by the character. The catalog therefore carries three PAIRS of
// per-1,000,000-unit prices, and this package prices exactly ONE of them per
// request:
//
//	basis        quantity                       divisor
//	tokens       tokens                         TokensPer1M        = 1e6
//	characters   characters                     CharsPer1M         = 1e6
//	seconds      MILLISECONDS (see below)       MillisPer1MSeconds = 1e9
//
// The seconds divisor is 1e9, not 1e6, and that is the whole reason it has a
// name. A duration arrives from a provider as a fractional second count. A
// float never reaches the money path here, so the caller rounds it to integer
// milliseconds ONCE at the response boundary. A per-1,000,000-SECONDS price
// divided by TokensPer1M against a millisecond quantity bills 1000x — the exact
// denomination bug the per-1M rule exists to stop.
//
// NEVER sum two bases. gpt-4o-mini-tts publishes both a token price and a
// per-second price upstream. A request billed on both is billed twice.
package cost

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// NanoUSD is the nano-USD scale factor: 1 USD = 1e9 nano-USD. Budget counters
// are int64 nano-USD (design §8; matches failmode.NanoUSD — the same domain
// constant, defined locally to avoid coupling the cost package to failmode).
const NanoUSD int64 = 1_000_000_000

// TokensPer1M is the price denominator: prices in gateway_models are per this
// many tokens. Using any other divisor here is the 1000×/1e6 costing bug the
// migration guards (design §8.8).
const TokensPer1M int64 = 1_000_000

// CharsPer1M is the price denominator for a character-billed model:
// input_cost_per_1m_characters is the price of this many characters.
const CharsPer1M int64 = 1_000_000

// MillisPer1MSeconds is the price denominator for a second-billed model when
// the quantity is counted in MILLISECONDS.
//
// input_cost_per_1m_seconds is the price of 1,000,000 SECONDS, and 1,000,000
// seconds are 1,000,000,000 milliseconds. Using TokensPer1M here instead would
// bill every audio second 1000 times over. The quantity is milliseconds and not
// seconds because a provider reports a fractional duration, and a float on the
// money path is forbidden (CLAUDE.md); the caller rounds once, at the response
// boundary, and every step after that is int64.
const MillisPer1MSeconds int64 = 1_000_000_000

// The three names a Cost may carry for the rate that paid. An empty basis means
// UNPRICED: the gateway found no rate for the units the provider reported, and
// billed nothing. An empty basis is NOT "billed zero" — see CostUnits.
const (
	// BasisTokens means a per-1M-tokens rate paid.
	BasisTokens = "tokens"
	// BasisSeconds means a per-1M-seconds rate paid.
	BasisSeconds = "seconds"
	// BasisCharacters means a per-1M-characters rate paid.
	BasisCharacters = "characters"
)

// DefaultCacheTTL is the in-process price-cache lifetime on the cost hot path
// (design §8.8: "a 5-minute in-process cache on the cost hot path"). It matches
// the pylon CostCalculator's 5-minute pricing cache.
const DefaultCacheTTL = 5 * time.Minute

// Price is a resolved set of per-1,000,000-unit prices in nano-USD, plus its
// provenance.
type Price struct {
	// InputNanoPer1M is the input token price in nano-USD per 1,000,000 tokens.
	InputNanoPer1M int64
	// OutputNanoPer1M is the output token price in nano-USD per 1,000,000 tokens.
	OutputNanoPer1M int64

	// InputNanoPer1MSeconds is the input audio price in nano-USD per 1,000,000
	// SECONDS. whisper-1 is sold this way and has no token price at all.
	InputNanoPer1MSeconds int64
	// OutputNanoPer1MSeconds is the generated-audio price in nano-USD per
	// 1,000,000 SECONDS.
	OutputNanoPer1MSeconds int64
	// InputNanoPer1MChars is the input text price in nano-USD per 1,000,000
	// characters. Some text-to-speech models are sold this way.
	InputNanoPer1MChars int64
	// OutputNanoPer1MChars is the output text price in nano-USD per 1,000,000
	// characters.
	OutputNanoPer1MChars int64

	// AudioFromCatalog is true ONLY when the four rates above were read from
	// gateway_models. It is the guard that keeps a fabricated price off the
	// audio path.
	//
	// defaultPrice fabricates a 1.0/3.0 USD-per-1M TOKEN price for any model no
	// prefix matches. Those two numbers are a defensible guess for a text model
	// and a meaningless one for a second of audio. If the audio rates rode the
	// same struct through that path with no flag, whisper-1 would take an
	// INVENTED per-second price onto the authoritative budget counter, and the
	// counter is what the budget gate reads back. defaultPrice therefore leaves
	// this false, and CostUnits treats an unflagged or absent audio rate as
	// UNPRICED rather than as a zero-cost billed request.
	AudioFromCatalog bool

	// Source is where the TOKEN price came from: sourceCatalog (gateway_models),
	// sourceDefault (a matched entry in the pylon default table), or
	// sourceFallback (the ultimate 1.0/3.0 USD default). The audio rates have
	// their own provenance flag, AudioFromCatalog, because they have only one
	// source.
	//
	// A whisper-1 row carries a real per-second rate and NULL token columns, so
	// this field reads "fallback" for it — the token prices it does not sell by
	// really are fabricated. That says nothing about the rate that pays the
	// request, and Cost.Source, not this field, is what the billing log reports.
	Source string

	// OutputDerived is true when the catalog row had NO output price and
	// OutputNanoPer1M is the pylon input x 3 estimate (tokenPricesFromRow).
	//
	// Issue #6719 is why it is a field. An image model publishes its output
	// price as a per-IMAGE-TOKEN rate, and the price sync read only the
	// per-token one, so gpt-image-2 reached this package with a NULL output
	// price. Its output tokens were then billed at 15 USD per 1M instead of
	// 30, and nothing anywhere said that the figure was an estimate. The sync
	// now stores the image rate in the output column. This flag makes the
	// next row that still lacks one visible on the billing log.
	OutputDerived bool
}

// Units is the quantity a request consumed, in every denomination the catalog
// can price. A caller sets exactly ONE pair; see CostUnits for the precedence
// that applies when it sets more.
//
// InputMillis / OutputMillis are MILLISECONDS, not seconds. A provider reports
// a fractional second count, and the conversion to an integer happens once, at
// the response boundary, so that no float ever reaches this package.
type Units struct {
	InputTokens  int64
	OutputTokens int64
	InputChars   int64
	// OutputChars has NO producer on the request path today, and that is a
	// stated gap and not an oversight (issue #323). Neither audio route can
	// fill it: bifrost's SpeechUsage reports InputChars only, and its
	// TranscriptionUsage reports tokens or a duration and no character count at
	// all. Counting the characters of a transcript the gateway received would
	// be a quantity the GATEWAY derived from the body, not one the provider
	// reported and agreed to bill.
	//
	// The consequence is real and must not be discovered later: a model whose
	// ONLY character rate is output_cost_per_1m_characters is UNPRICED. It
	// bills zero and raises gateway_audio_unpriced_total, exactly as a model
	// with no catalog rate does. The field stays because audioCost prices both
	// directions of every basis with one function, and because a route that
	// gains a provider-reported output character count must have somewhere
	// truthful to put it.
	OutputChars  int64
	InputMillis  int64
	OutputMillis int64
}

// Basis reports which rate CostUnits will try to apply to u. The precedence is
// fixed and is the same one the audio routes write down: tokens, then seconds,
// then characters. Empty units read as tokens, which keeps a zero-usage
// response on the path it has always taken.
//
// The precedence exists to stop double-billing. gpt-4o-mini-tts publishes both
// a token price and a per-second price upstream, so a response can carry both
// quantities; summing the two bills the request twice.
func (u Units) Basis() string {
	switch {
	case u.InputTokens > 0 || u.OutputTokens > 0:
		return BasisTokens
	case u.InputMillis > 0 || u.OutputMillis > 0:
		return BasisSeconds
	case u.InputChars > 0 || u.OutputChars > 0:
		return BasisCharacters
	default:
		return BasisTokens
	}
}

// Cost is a computed per-request cost, all fields in int64 nano-USD.
type Cost struct {
	InputNanoUSD  int64
	OutputNanoUSD int64
	TotalNanoUSD  int64
	// Source names where the rate that ACTUALLY PAID came from: sourceCatalog,
	// sourceDefault or sourceFallback. It is "" for an UNPRICED cost, because
	// no rate paid.
	//
	// It is NOT a copy of Price.Source (issue #323 review). whisper-1 has a
	// real catalog per-second rate and NULL token columns, so its Price.Source
	// is "fallback" — the provenance of token prices it never uses. Copying
	// that here made the ONE line an operator reads to confirm a catalog price
	// say source=fallback for a price that came 100% from the catalog.
	Source string
	// Basis names the rate that paid: BasisTokens, BasisSeconds,
	// BasisCharacters, or "" for UNPRICED.
	//
	// "" and a zero TotalNanoUSD are different facts. A token-billed request
	// with a zero price is priced and costs nothing. An UNPRICED request is one
	// the gateway could not price at all, and it is a hole in the money path
	// that an operator must be able to count.
	Basis string
	// OutputDerived is true when OutputNanoUSD was priced at the input x 3
	// estimate because the catalog row carries no output price. See
	// Price.OutputDerived. It is false for every other basis and source.
	OutputDerived bool
}

// rowQuerier is the minimal pgx surface the calculator needs, satisfied by
// *pgxpool.Pool and by test fakes (mirrors the account package's DB seam so the
// package is unit-testable with no live database).
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgxRow
}

// pgxRow mirrors pgx.Row (Scan-only).
type pgxRow interface {
	Scan(dest ...any) error
}

// modelPriceSQL reads the per-1M prices for one (provider, model) and scales
// them to nano-USD exactly in SQL. NUMERIC(20,8) × 1e9 is an integer (≤8
// fractional digits), so the ::bigint cast is exact — no float ever touches the
// money path. NULL costs scan into the *int64 pointers as nil (pylon treats a
// row with a NULL input cost as "not usable" and falls back to defaults).
//
// The four audio columns (migration 0086) are NUMERIC(20,8) and nullable, and
// they are denominated per 1,000,000 units exactly as the token columns are.
// Most rows hold NULL in all four: a text model has no per-second price, and
// NULL is the only honest value for it. A NULL scans into its *int64 as nil and
// leaves AudioFromCatalog false, which is what makes such a request UNPRICED
// instead of billed at zero.
//
// A database that predates 0086 answers this statement with 42703 for EVERY
// model, not only for an audio one. That is why modelTokenPriceSQL below still
// exists and why queryCatalog falls back to it: see schema_probe.go.
//
// $1 provider, $2 model_name, $3 NanoUSD scale factor.
const modelPriceSQL = `SELECT
		(input_cost_per_1m_tokens      * $3::numeric)::bigint AS input_nano,
		(output_cost_per_1m_tokens     * $3::numeric)::bigint AS output_nano,
		(input_cost_per_1m_seconds     * $3::numeric)::bigint AS input_nano_seconds,
		(output_cost_per_1m_seconds    * $3::numeric)::bigint AS output_nano_seconds,
		(input_cost_per_1m_characters  * $3::numeric)::bigint AS input_nano_chars,
		(output_cost_per_1m_characters * $3::numeric)::bigint AS output_nano_chars
	FROM gateway.gateway_models
	WHERE provider = $1 AND model_name = $2`

// modelTokenPriceSQL is modelPriceSQL as it read before migration 0086: the two
// token columns, which have existed since 0067.
//
// It is the statement the calculator falls back to when the widened one is
// refused with 42703, and it is written out rather than generated so that the
// degraded path is a statement somebody can read, run by hand, and compare with
// the one it replaces.
//
// $1 provider, $2 model_name, $3 NanoUSD scale factor.
const modelTokenPriceSQL = `SELECT
		(input_cost_per_1m_tokens  * $3::numeric)::bigint AS input_nano,
		(output_cost_per_1m_tokens * $3::numeric)::bigint AS output_nano
	FROM gateway.gateway_models
	WHERE provider = $1 AND model_name = $2`

// The three provenances a price can have. They are the strings the billing log
// and the usage ledger carry, so they are named once here rather than spelled
// at each site.
const (
	sourceCatalog  = "catalog"
	sourceDefault  = "default"
	sourceFallback = "fallback"
)

// Calculator resolves prices and computes nano-USD costs. It is safe for
// concurrent use.
type Calculator struct {
	db     rowQuerier
	ttl    time.Duration
	now    func() time.Time
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]cacheEntry

	// schemaBehindUntil is the unix-nano time before which the widened price
	// statement must not be attempted again, because the database answered
	// 42703 for its audio columns. Zero means "attempt it". See
	// schema_probe.go; it is atomic because the price path is concurrent and
	// the cache mutex must not be held across a query.
	schemaBehindUntil atomic.Int64
}

type cacheEntry struct {
	price   Price
	expires time.Time
}

// Config configures a Calculator.
type Config struct {
	// DB is the Postgres handle (*pgxpool.Pool in production, via NewPoolQuerier).
	// When nil the calculator serves prices from the default table only — useful
	// for tests and for a gateway booted without a catalog.
	DB rowQuerier
	// CacheTTL overrides the price-cache lifetime. <= 0 uses DefaultCacheTTL.
	CacheTTL time.Duration
	// Now overrides the clock (tests). nil uses time.Now.
	Now func() time.Time
	// Logger is used for price-lookup warnings. nil uses slog.Default().
	Logger *slog.Logger
}

// New builds a Calculator from cfg.
func New(cfg Config) *Calculator {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Calculator{
		db:     cfg.DB,
		ttl:    ttl,
		now:    now,
		logger: logger,
		cache:  make(map[string]cacheEntry),
	}
}

// Cost computes the input/output/total cost in int64 nano-USD for a request of
// inputTokens/outputTokens against (provider, model). Negative token counts are
// clamped to zero. The price is resolved from the catalog (cached), then the
// default table, then the ultimate fallback — never erroring on an unknown
// model, so a pricing gap never blocks the /llm response path.
//
// If a catalog price is so large that the multiplication overflows int64 after
// the per-1M divide (indicating a corrupt DB row), Cost falls back to the
// default price for the model and logs a warning rather than returning a
// silently wrong value.
func (c *Calculator) Cost(ctx context.Context, provider, model string, inputTokens, outputTokens int64) Cost {
	return c.CostUnits(ctx, provider, model, Units{InputTokens: inputTokens, OutputTokens: outputTokens})
}

// CostUnits computes the cost in int64 nano-USD for a request that consumed u
// against (provider, model). It is the ONE implementation; Cost is a token-only
// call into it.
//
// Exactly one rate pays. u.Basis() picks it, with the fixed precedence tokens →
// seconds → characters, so a response that reports both a token count and a
// duration is billed once and not twice.
//
// The token basis never fails: an unknown model falls through the catalog to
// the default table to the ultimate fallback, so a pricing gap never blocks the
// /llm response path.
//
// The audio bases DO fail, on purpose. There is no default per-second or
// per-character price and there must not be one — see Price.AudioFromCatalog. A
// response whose quantity the catalog carries no rate for returns a Cost with
// an empty Basis and a zero total. The caller must count that as UNPRICED and
// must not read it as a request that cost nothing.
// FromCatalog reports whether the rate that ACTUALLY PAID came from
// gateway_models, rather than from the pylon default table or the ultimate
// fallback.
//
// It exists so a caller can tell a real price from a fabricated one without
// matching a string literal of its own. The audio routes need exactly that: the
// seconds and characters bases refuse a non-catalog rate outright, but the TOKEN
// basis falls back like every other route on this gateway, so an audio request
// can bill a made-up figure. updateUsageUnits uses this to say so out loud.
//
// An UNPRICED cost returns false, because no rate paid at all.
func (c Cost) FromCatalog() bool { return c.Source == sourceCatalog }

// The provenance values Cost.Source can carry, exported so a caller — and a
// test double in another package — can name one without repeating a literal.
const (
	// SourceCatalog means the rate came from gateway_models.
	SourceCatalog = sourceCatalog
	// SourceDefault means the rate came from the pylon default price table.
	SourceDefault = sourceDefault
	// SourceFallback means the rate came from the ultimate 1.0/3.0 USD-per-1M
	// default: the gateway knows nothing about this model and priced it anyway.
	SourceFallback = sourceFallback
)

func (c *Calculator) CostUnits(ctx context.Context, provider, model string, u Units) Cost {
	price := c.Price(ctx, provider, model)
	switch u.Basis() {
	case BasisSeconds:
		return audioCost(price, BasisSeconds,
			u.InputMillis, u.OutputMillis,
			price.InputNanoPer1MSeconds, price.OutputNanoPer1MSeconds,
			MillisPer1MSeconds)
	case BasisCharacters:
		return audioCost(price, BasisCharacters,
			u.InputChars, u.OutputChars,
			price.InputNanoPer1MChars, price.OutputNanoPer1MChars,
			CharsPer1M)
	default:
		return c.tokenCost(ctx, provider, model, price, u.InputTokens, u.OutputTokens)
	}
}

// tokenCost prices a token-billed request.
//
// If a catalog price is so large that the multiplication overflows int64 after
// the per-1M divide (indicating a corrupt DB row), it falls back to the default
// price for the model and logs a warning rather than returning a silently wrong
// value.
func (c *Calculator) tokenCost(ctx context.Context, provider, model string, price Price, inputTokens, outputTokens int64) Cost {
	in, inOK := costNano(inputTokens, price.InputNanoPer1M, TokensPer1M)
	out, outOK := costNano(outputTokens, price.OutputNanoPer1M, TokensPer1M)
	if !inOK || !outOK {
		// The catalog price is pathologically large (corrupt DB row): fall back
		// to the default price so the /llm path is never blocked or mis-billed.
		c.logger.WarnContext(ctx, "cost: price overflows int64 after divide; falling back to default price",
			"provider", provider, "model", model,
			"input_nano_per_1m", price.InputNanoPer1M,
			"output_nano_per_1m", price.OutputNanoPer1M,
		)
		price = defaultPrice(model)
		in, _ = costNano(inputTokens, price.InputNanoPer1M, TokensPer1M)
		out, _ = costNano(outputTokens, price.OutputNanoPer1M, TokensPer1M)
	}
	return Cost{
		InputNanoUSD:  in,
		OutputNanoUSD: out,
		TotalNanoUSD:  in + out,
		Source:        price.Source,
		Basis:         BasisTokens,
		// Only when output tokens were actually priced at the estimate. A
		// request with no output (an embedding) did not use the estimate.
		OutputDerived: price.OutputDerived && outputTokens > 0,
	}
}

// audioCost prices a seconds-billed or characters-billed request against the
// pair of catalog rates for that basis, using per1M as the divisor.
//
// It returns an UNPRICED Cost (empty Basis, zero total) when:
//
//   - the rates did not come from the catalog. defaultPrice never sets
//     AudioFromCatalog, so an unmatched model cannot take its fabricated
//     1.0/3.0 USD token price onto the audio path.
//   - a quantity the response reported has no rate paired with it. A rate is
//     not guessed from the other direction: an input-seconds price is the price
//     of audio sent TO the model, and using it for audio the model GENERATED is
//     an invented figure on the authoritative counter.
//   - the multiplication overflows int64 after the divide. That is a corrupt
//     catalog row, and unlike the token path there is no default rate to fall
//     back to, so the request is unpriced rather than billed a truncated value.
//
// The Cost it returns carries the provenance of the rate that PAID, which for
// every priced audio request is the catalog — never price.Source, which
// describes the token prices this request did not use.
func audioCost(price Price, basis string, inQty, outQty, inRate, outRate, per1M int64) Cost {
	// An UNPRICED cost has no source, because no rate paid. Naming one here
	// would put a provenance on a request that was never priced.
	unpriced := Cost{}
	// This check cannot fail against any Price this package builds today:
	// defaultPrice leaves all four audio rates zero, and the zero-rate check
	// below already refuses those. It is a STRUCTURAL guard, and it is the one
	// that states the rule — a rate that did not come from the catalog must
	// never pay, whatever its value — so that a future producer of Price cannot
	// put a fabricated per-second figure on the authoritative budget counter by
	// filling a rate field. TestAudioCost_RefusesAPositiveRateNotFromTheCatalog
	// builds exactly that Price by hand and fails when this line is deleted.
	if !price.AudioFromCatalog {
		return unpriced
	}
	// The reachable half of the rule: a quantity whose own rate is absent is
	// not paid by the rate for the other direction.
	if (inQty > 0 && inRate <= 0) || (outQty > 0 && outRate <= 0) {
		return unpriced
	}
	in, inOK := costNano(inQty, inRate, per1M)
	out, outOK := costNano(outQty, outRate, per1M)
	if !inOK || !outOK {
		return unpriced
	}
	return Cost{
		InputNanoUSD:  in,
		OutputNanoUSD: out,
		TotalNanoUSD:  in + out,
		// AudioFromCatalog is true, so the rate that paid IS the catalog's,
		// whatever the token columns of the same row had to say.
		Source: sourceCatalog,
		Basis:  basis,
	}
}

// Price resolves the per-1M nano-USD price for (provider, model). It checks the
// TTL cache, then the catalog, then the default table. It never returns an
// error: an unknown model resolves to the ultimate fallback price.
func (c *Calculator) Price(ctx context.Context, provider, model string) Price {
	key := provider + ":" + model

	c.mu.RLock()
	if ent, ok := c.cache[key]; ok && c.now().Before(ent.expires) {
		c.mu.RUnlock()
		return ent.price
	}
	c.mu.RUnlock()

	price, ok := c.lookupCatalog(ctx, provider, model)
	if !ok {
		// Not in the catalog (or a NULL/absent input price, or a DB error):
		// fall back to the pylon default table, exactly as the legacy path does.
		price = defaultPrice(model)
	}

	// Fix #5 (cost): Re-check the cache under the write lock before writing
	// (standard check-then-act pattern for concurrent caches). Without this,
	// two goroutines that both miss the read-lock check both call lookupCatalog;
	// whichever finishes last overwrites the entry from the other — potentially
	// overwriting a fresher entry with a stale one (or extending TTL incorrectly).
	c.mu.Lock()
	if ent, ok := c.cache[key]; !ok || !c.now().Before(ent.expires) {
		c.cache[key] = cacheEntry{price: price, expires: c.now().Add(c.ttl)}
	} else {
		// A concurrent goroutine already wrote a fresh entry; use it.
		price = ent.price
	}
	c.mu.Unlock()

	return price
}

// lookupCatalog reads (provider, model) from gateway_models. ok is false when
// there is no DB, the row is absent, the query errors, or the row carries
// NEITHER a usable token price NOR any audio rate — in every such case the
// caller falls back to the default table (this mirrors pylon, which uses the DB
// row only when input_cost_per_1m_tokens is not None and otherwise drops to its
// default table).
//
// A row with NULL token prices and a per-second rate is USABLE. whisper-1 is
// exactly that row: it is sold by the second and has no token price to write.
// Refusing the whole row because its token columns are NULL would send a model
// the catalog DOES price down the unpriced path, which is the opposite of the
// point. The token prices for such a row come from the default table, and the
// audio rates stay the catalog's.
func (c *Calculator) lookupCatalog(ctx context.Context, provider, model string) (Price, bool) {
	if c.db == nil {
		return Price{}, false
	}

	row, ok := c.queryCatalog(ctx, provider, model)
	if !ok {
		return Price{}, false
	}

	p := Price{
		InputNanoPer1MSeconds:  positiveOrZero(row.inputSeconds),
		OutputNanoPer1MSeconds: positiveOrZero(row.outputSeconds),
		InputNanoPer1MChars:    positiveOrZero(row.inputChars),
		OutputNanoPer1MChars:   positiveOrZero(row.outputChars),
	}
	// The flag says the rates were READ, not that any of them is usable for a
	// given direction. audioCost still refuses a quantity whose own rate is
	// absent. Both checks are needed: this one keeps a fabricated default off
	// the audio path, and that one keeps an input rate from paying for output.
	p.AudioFromCatalog = p.InputNanoPer1MSeconds > 0 || p.OutputNanoPer1MSeconds > 0 ||
		p.InputNanoPer1MChars > 0 || p.OutputNanoPer1MChars > 0

	inTok, outTok, outDerived, tokOK := tokenPricesFromRow(row.inputTokens, row.outputTokens)
	if !tokOK {
		if !p.AudioFromCatalog {
			// Neither a usable token price nor an audio rate: the row is not
			// usable, exactly as it was before the audio columns existed.
			return Price{}, false
		}
		// A second-billed or character-billed model. Its token columns are
		// NULL because there is no token price to record. Keep the audio rates
		// and take the token prices from the default table, so a caller that
		// somehow reports tokens for it still gets the legacy answer.
		d := defaultPrice(model)
		inTok, outTok = d.InputNanoPer1M, d.OutputNanoPer1M
		p.InputNanoPer1M, p.OutputNanoPer1M, p.Source = inTok, outTok, d.Source
		return p, true
	}
	p.InputNanoPer1M, p.OutputNanoPer1M, p.Source = inTok, outTok, sourceCatalog
	p.OutputDerived = outDerived
	return p, true
}

// catalogPrices are the nullable price columns of ONE gateway_models row, in
// nano-USD per 1,000,000 units. A nil pointer is the SQL NULL. The audio four
// are nil for every row on a database that predates migration 0086, and that is
// the same value they hold for a text model — which is what makes the degraded
// read safe: it costs the audio bases and nothing else.
type catalogPrices struct {
	inputTokens, outputTokens   *int64
	inputSeconds, outputSeconds *int64
	inputChars, outputChars     *int64
}

// queryCatalog reads one row and answers with whatever price columns this
// database HAS.
//
// It attempts the widened statement first. Postgres 42703 means the audio
// columns are not there — the database is older than this binary — so it
// latches, says so once and loudly (schema_probe.go), and re-reads the same row
// with the pre-0086 token-only statement. The token price therefore survives a
// migration skew that would otherwise send the WHOLE catalog to the default
// price table. Any other error is the pre-existing behaviour: no row, no price,
// caller falls back to defaults.
func (c *Calculator) queryCatalog(ctx context.Context, provider, model string) (catalogPrices, bool) {
	var row catalogPrices
	if c.audioColumnsReadable() {
		err := c.db.QueryRow(ctx, modelPriceSQL, provider, model, NanoUSD).Scan(
			&row.inputTokens, &row.outputTokens,
			&row.inputSeconds, &row.outputSeconds,
			&row.inputChars, &row.outputChars,
		)
		switch {
		case err == nil:
			c.clearSchemaBehind()
			return row, true
		case isUndefinedColumn(err):
			c.latchSchemaBehind(err)
			// A partial scan may have written into the targets before the
			// error. Start the degraded read from a clean row.
			row = catalogPrices{}
		default:
			c.reportLookupFailure(provider, model, err)
			return catalogPrices{}, false
		}
	}

	if err := c.db.QueryRow(ctx, modelTokenPriceSQL, provider, model, NanoUSD).Scan(
		&row.inputTokens, &row.outputTokens,
	); err != nil {
		c.reportLookupFailure(provider, model, err)
		return catalogPrices{}, false
	}
	return row, true
}

// reportLookupFailure logs a failed catalog read. An absent row is not a
// failure and is not logged: most models are not in the catalog, and the caller
// prices them from the default table by design.
func (c *Calculator) reportLookupFailure(provider, model string, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	c.logger.Warn("cost: catalog price lookup failed; using defaults",
		"provider", provider, "model", model, "err", err)
}

// tokenPricesFromRow resolves the token price pair from the two token columns.
// ok is false when the input price is NULL, or when the pylon input*3 output
// default would overflow int64. derived is true when out is that input*3
// estimate rather than a catalog price.
//
// An image model does not reach the estimate any more. The price sync stores
// its per-image-token output rate in output_cost_per_1m_tokens (issue #6719),
// because that is the rate of the tokens the provider reports as output. The
// estimate stays for a row that still has no output price, and derived makes
// such a row visible.
func tokenPricesFromRow(inputNano, outputNano *int64) (in, out int64, derived, ok bool) {
	if inputNano == nil {
		// Row exists but has no usable input price — treat as uncatalogued.
		return 0, 0, false, false
	}
	if outputNano != nil {
		return *inputNano, *outputNano, false, true
	}
	// pylon default when output price is absent: input * 3. Guard against
	// silent int64 overflow (which would produce an output price of 0 instead
	// of the correct fallback) for a corrupt/absurd catalog row.
	if *inputNano > math.MaxInt64/3 {
		return 0, 0, false, false
	}
	return *inputNano, *inputNano * 3, true, true
}

// positiveOrZero reads a nullable catalog rate. NULL and a non-positive value
// both mean "no rate", and a rate of zero must not read as a priced rate: it
// would bill every audio second at nothing while reporting a basis.
func positiveOrZero(v *int64) int64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}

// costNano returns round( quantity * priceNanoPer1M / per1M ) in nano-USD and
// whether the result fits in int64. ok is false only when priceNanoPer1M is so
// extreme that the final quotient does not fit in int64, which indicates a
// corrupt DB row; the caller must fall back to a safe default in that case.
// The multiply uses math/big so a large quantity cannot overflow int64 before
// the divide; rounding is half-up (both operands are non-negative).
//
// per1M is the divisor for the basis being priced, and it is a PARAMETER
// because it is not always 1e6. Pass TokensPer1M for tokens, CharsPer1M for
// characters, and MillisPer1MSeconds for a millisecond quantity against a
// per-1M-seconds price. Passing TokensPer1M for a millisecond quantity bills
// 1000x — the denomination bug this package exists to stop.
func costNano(quantity, priceNanoPer1M, per1M int64) (int64, bool) {
	if quantity <= 0 || priceNanoPer1M <= 0 {
		return 0, true
	}
	if per1M <= 0 {
		// A zero or negative divisor is a programming error, not a data one.
		// Refuse rather than divide: the caller reads ok=false as "do not bill
		// this", and a panic on the money path would take the response with it.
		return 0, false
	}
	prod := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(priceNanoPer1M))
	// Round half-up: (prod + per1M/2) / per1M.
	prod.Add(prod, big.NewInt(per1M/2))
	prod.Quo(prod, big.NewInt(per1M))
	if !prod.IsInt64() {
		return 0, false
	}
	return prod.Int64(), true
}
