-- 0243_gateway_image_pricing.sql — per-image prices in the model price catalog,
-- and the image columns of the gateway request log (issue 1969).
--
-- WHY A SECOND TABLE. gateway.gateway_models holds ONE row per (provider,
-- model) and every price in it is a rate PER 1,000,000 UNITS of ONE basis:
-- tokens, seconds or characters. An image model that is sold by the image has
-- a price that depends on the SIZE and the QUALITY of the request, so it is a
-- set of rows for one model, not a column. DALL-E 3 lists 0.040 USD for a
-- standard 1024x1024 image and 0.120 USD for an HD 1792x1024 one.
--
-- Until this table existed the gateway billed every image whose provider
-- reported no token usage at a flat 0.04 USD, so DALL-E 3 HD 1792x1024 was
-- billed 3 times too low and DALL-E 2 256x256 (0.016 USD) 2.5 times too high.
--
-- ONE ROW = ONE PRICE.
--
--   provider, model_name  the same pair gateway_models uses. The model name is
--                         the bare model: the sheet keys that carry a size and
--                         a quality ("hd/1792-x-1024/dall-e-3") are split by the
--                         price sync into these columns.
--   size                  'WIDTHxHEIGHT' ('1792x1024'), or '' for any size.
--   quality               'standard', 'hd', 'low', ..., or '' for any quality.
--   usd_per_image         the price of ONE image, in USD. NOT per 1,000,000:
--                         the unit is the image, and a per-1M scale would be a
--                         price nobody could read. The gateway converts it to
--                         integer nano-USD with an exact NUMERIC cast.
--   usd_per_1m_pixels     for a model the sheet prices per pixel and gives no
--                         size, USD per 1,000,000 PIXELS. The gateway multiplies
--                         it by the pixels of the requested size. It is the
--                         same per-1M scale as every other column of the
--                         catalog, so the price sync applies the one conversion
--                         it already has.
--
-- A row has usd_per_image, or usd_per_1m_pixels, or both (usd_per_image wins).
-- A sheet that prices per pixel AND names the size is converted by the sync to
-- usd_per_image for that size, because the size is then known and the result
-- is the number a price list prints.
--
-- WHICH ROW PAYS. The gateway picks the row whose size and quality equal the
-- request's; a '' column matches any value and loses to an exact match. When
-- the request names no size or quality, the model default is assumed (see the
-- gateway), and an ambiguous request takes the HIGHEST matching price, so a
-- call is never billed below the list price.
--
-- price_overridden has the meaning it has in gateway_models (0097): a row an
-- operator priced by hand is left as authored by the next price sync.
--
-- gateway.llm_request_logs gets five nullable columns. kind is 'image' for an
-- image request and NULL for every other row. pricing_source says which rate
-- paid: 'token' (the provider reported token usage), 'catalog_per_image' (this
-- table) or 'fallback_estimate' (no price: the flat per-image estimate, which
-- an operator must replace by adding a price). image_count is the number of
-- images the provider RETURNED; image_size and image_quality are the request's.
--
-- Nothing here scans a large table: the new columns are nullable with no
-- default, which is a catalog change, and the new table is empty.

CREATE TABLE IF NOT EXISTS gateway.gateway_model_image_prices (
    provider          VARCHAR(64)  NOT NULL,
    model_name        VARCHAR(128) NOT NULL,
    size              VARCHAR(16)  NOT NULL DEFAULT '',
    quality           VARCHAR(32)  NOT NULL DEFAULT '',
    usd_per_image     NUMERIC(20,10),
    usd_per_1m_pixels NUMERIC(20,8),
    source            VARCHAR(32),
    price_overridden  BOOLEAN      NOT NULL DEFAULT false,
    source_synced_at  TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT gateway_model_image_prices_pk PRIMARY KEY (provider, model_name, size, quality),
    CONSTRAINT gateway_model_image_prices_has_price
        CHECK (usd_per_image IS NOT NULL OR usd_per_1m_pixels IS NOT NULL),
    CONSTRAINT gateway_model_image_prices_non_negative
        CHECK ((usd_per_image IS NULL OR usd_per_image >= 0)
           AND (usd_per_1m_pixels IS NULL OR usd_per_1m_pixels >= 0))
);

ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS kind VARCHAR(16);
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS image_count INTEGER;
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS image_size VARCHAR(16);
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS image_quality VARCHAR(32);
ALTER TABLE gateway.llm_request_logs ADD COLUMN IF NOT EXISTS pricing_source VARCHAR(24);

COMMENT ON TABLE gateway.gateway_model_image_prices IS
    'USD per ONE generated image, by model, size and quality. An empty size or quality matches any value.';
COMMENT ON COLUMN gateway.gateway_model_image_prices.usd_per_1m_pixels IS
    'USD per 1,000,000 pixels, for a model priced per pixel with no named size.';
COMMENT ON COLUMN gateway.llm_request_logs.pricing_source IS
    'Which rate paid an image request: token, catalog_per_image or fallback_estimate (an estimate: add a price).';
