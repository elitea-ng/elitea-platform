package cost

// image_price.go — the per-image price of an image model (issue 1969,
// shared migration 0243, gateway.gateway_model_image_prices).
//
// An image model is billed in one of three ways, and this file owns the second:
//
//  1. The provider reports token usage (gpt-image-*, gemini-*-image). The token
//     rates of gateway_models pay it. This file is not involved.
//  2. The provider reports no usage and the catalog holds a price per image for
//     the request's size and quality. ImageUnitNano answers it.
//  3. Neither. The caller bills a flat estimate and MUST say it is one.
//
// UNITS. usd_per_image is USD per ONE image. The statement scales it by
// NanoUSD (1e9) into integer nano-USD in SQL, with an exact NUMERIC cast, so no
// float reaches the money path. usd_per_1m_pixels is USD per 1,000,000 pixels,
// the catalog's usual per-1M scale; PixelsNano applies it to a pixel count with
// the same big.Int rule as every other basis (costNano, per1M = 1e6). A pixel
// price multiplied without the 1e6 divisor would bill 1,000,000 times too much.

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PixelsPer1M is the divisor of a per-1M-pixels price.
const PixelsPer1M int64 = 1_000_000

// imagePriceSQL picks the ONE row that prices a request.
//
// $1 provider, $2 model_name, $3 NanoUSD, $4 the requested size ("WxH" or ''),
// $5 the requested quality ('' when absent).
//
// A '' column of a row matches any request value. A request value that is ''
// matches every row (the caller did not choose, and the model default was
// already applied). Among the candidates the exact size wins, then the exact
// quality, then the HIGHEST price, so an ambiguous request is never billed
// below the list price.
const imagePriceSQL = `SELECT
		(usd_per_image * $3::numeric)::bigint,
		(usd_per_1m_pixels * $3::numeric)::bigint
	FROM gateway.gateway_model_image_prices
	WHERE provider = $1 AND model_name = $2
	  AND ($4::text = '' OR size IN ($4::text, ''))
	  AND ($5::text = '' OR quality IN ($5::text, ''))
	ORDER BY (size = $4::text AND $4::text <> '') DESC,
	         (quality = $5::text AND $5::text <> '') DESC,
	         usd_per_image DESC NULLS LAST,
	         usd_per_1m_pixels DESC NULLS LAST
	LIMIT 1`

// imageEntry is a cached answer. priced is false for a cached "no row".
type imageEntry struct {
	nano    int64
	priced  bool
	expires int64
}

// ImageUnitNano returns the price of ONE image for (provider, model, size,
// quality) in nano-USD. ok is false when the catalog holds no usable price: no
// row, no table (a database older than this binary), a read error, or a price
// of zero or less. A zero price is NOT a price here: it would bill the image
// at nothing, which is the defect this table removes. The caller then bills
// its flagged estimate.
func (c *Calculator) ImageUnitNano(ctx context.Context, provider, model, size, quality string) (int64, bool) {
	size = normalizeImageSize(size)
	quality = strings.ToLower(strings.TrimSpace(quality))
	key := provider + "\x00" + model + "\x00" + size + "\x00" + quality
	now := c.now().UnixNano()
	c.mu.RLock()
	entry, hit := c.images[key]
	c.mu.RUnlock()
	if hit && now < entry.expires {
		return entry.nano, entry.priced
	}
	if c.db == nil {
		return 0, false
	}
	var perImage, per1MPixels *int64
	err := c.db.QueryRow(ctx, imagePriceSQL, provider, model, NanoUSD, size, quality).Scan(&perImage, &per1MPixels)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		c.storeImage(key, imageEntry{expires: c.now().Add(c.ttl).UnixNano()})
		return 0, false
	default:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == undefinedColumnCode) {
			// A database older than this binary: no image price table yet.
			// Cache the miss so every image request does not pay a refused
			// query, and say so once per TTL and key.
			c.logger.Warn("cost: the image price table is missing; apply shared migration 0243. Images bill the flagged estimate",
				"provider", provider, "model", model)
			c.storeImage(key, imageEntry{expires: c.now().Add(c.ttl).UnixNano()})
			return 0, false
		}
		c.logger.Warn("cost: image price lookup failed; billing the flagged estimate",
			"provider", provider, "model", model, "err", err)
		return 0, false
	}
	nano := int64(0)
	switch {
	case perImage != nil && *perImage > 0:
		nano = *perImage
	case per1MPixels != nil && *per1MPixels > 0:
		pixels, ok := ImagePixels(size)
		if !ok {
			// A per-pixel price needs a size, and the request named none the
			// gateway can read. Not priced: bill the estimate and say so.
			c.storeImage(key, imageEntry{expires: c.now().Add(c.ttl).UnixNano()})
			return 0, false
		}
		var fits bool
		nano, fits = costNano(pixels, *per1MPixels, PixelsPer1M)
		if !fits {
			return 0, false
		}
	}
	if nano <= 0 {
		c.storeImage(key, imageEntry{expires: c.now().Add(c.ttl).UnixNano()})
		return 0, false
	}
	c.storeImage(key, imageEntry{nano: nano, priced: true, expires: c.now().Add(c.ttl).UnixNano()})
	return nano, true
}

func (c *Calculator) storeImage(key string, entry imageEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.images == nil {
		c.images = make(map[string]imageEntry)
	}
	c.images[key] = entry
}

// normalizeImageSize reads "1024x1024", "1024X1024" and "1024-x-1024" as
// "1024x1024". Anything else (such as "auto") is returned lower-cased, and
// matches only a row that names it or a '' row.
func normalizeImageSize(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	return strings.ReplaceAll(size, "-x-", "x")
}

// ImagePixels is the pixel count of a "WxH" size. ok is false for any other
// text, and for a count that is not positive.
func ImagePixels(size string) (int64, bool) {
	w, h, found := strings.Cut(normalizeImageSize(size), "x")
	if !found {
		return 0, false
	}
	wi, err1 := strconv.ParseInt(w, 10, 64)
	hi, err2 := strconv.ParseInt(h, 10, 64)
	if err1 != nil || err2 != nil || wi <= 0 || hi <= 0 || wi > 1<<20 || hi > 1<<20 {
		return 0, false
	}
	return wi * hi, true
}
