package requestlog

// image_postgres_integration_test.go runs the image-row INSERT against a REAL
// PostgreSQL with the SHIPPED DDL (shared migrations 0067, 0099 and 0243), and
// the per-image price statement of internal/cost against the shipped table.
// A fake execer cannot report a wrong column name, an overflowing VARCHAR or a
// CHECK a value violates.
//
// It runs only when ELITEA_TEST_DATABASE_URL names a database. With
// ELITEA_REQUIRE_GATEWAY_POSTGRES_TEST set (CI), a missing database fails it.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
)

func TestPostgresImageRowAndImagePriceCatalog(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("ELITEA_REQUIRE_GATEWAY_POSTGRES_TEST") != "" {
			t.Fatal("ELITEA_REQUIRE_GATEWAY_POSTGRES_TEST is set, so this Postgres test may not skip")
		}
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the image billing integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, file := range []string{
		"0067_gateway_budget_schema.sql", "0099_gateway_request_logs.sql",
		"0084_budget_usage_dimensions.sql", "0100_gateway_request_log_execution_id.sql",
		"0139_gateway_request_log_credential_owner_and_cache_tokens.sql",
		"0225_model_reasoning_control.sql", "0243_gateway_image_pricing.sql",
	} {
		path := filepath.Join("..", "..", "..", "elitea-main", "migrations", "shared", file)
		ddl, err := os.ReadFile(path) //nolint:gosec // a fixed in-repo path
		if err != nil {
			t.Fatal(err)
		}
		// Applied twice: every statement must be idempotent.
		for range 2 {
			if _, err := pool.Exec(ctx, string(ddl)); err != nil {
				t.Fatalf("apply %s: %v", file, err)
			}
		}
	}
	model := "dall-e-3-it-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gateway.gateway_model_image_prices WHERE model_name LIKE 'dall-e-3-it-%' OR model_name LIKE 'flux-it-%'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM gateway.llm_request_logs WHERE model = $1`, model)
	})

	// --- the request-log row -------------------------------------------------
	store := NewStore(pool)
	rows := []Record{
		{OccurredAt: time.Now(), Route: "/llm/v1/images/generations", Method: "POST", Status: 200, Provider: "openai", Model: model,
			Image: &ImageFields{Count: 2, Size: "1792x1024", Quality: "hd", PricingSource: PricingSourceCatalogPerImage}},
		{OccurredAt: time.Now(), Route: "/llm/v1/chat/completions", Method: "POST", Status: 200, Provider: "openai", Model: model},
	}
	if err := store.WriteBatch(ctx, rows); err != nil {
		t.Fatalf("write batch: %v", err)
	}
	var kind, size, quality, source *string
	var count *int32
	err = pool.QueryRow(ctx, `SELECT kind, image_count, image_size, image_quality, pricing_source
		FROM gateway.llm_request_logs WHERE model = $1 AND kind IS NOT NULL`, model).Scan(&kind, &count, &size, &quality, &source)
	if err != nil {
		t.Fatalf("read the image row: %v", err)
	}
	if *kind != "image" || *count != 2 || *size != "1792x1024" || *quality != "hd" || *source != "catalog_per_image" {
		t.Fatalf("image row = %s %d %s %s %s", *kind, *count, *size, *quality, *source)
	}
	var plain int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway.llm_request_logs WHERE model = $1 AND kind IS NULL AND pricing_source IS NULL`, model).Scan(&plain); err != nil || plain != 1 {
		t.Fatalf("the model row must keep NULL image columns: %d, %v", plain, err)
	}

	// --- the price catalog, read by the gateway's own statement --------------
	insert := func(m, size, quality string, perImage, per1MPixels any) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO gateway.gateway_model_image_prices
			(provider, model_name, size, quality, usd_per_image, usd_per_1m_pixels) VALUES ('openai', $1, $2, $3, $4, $5)`,
			m, size, quality, perImage, per1MPixels); err != nil {
			t.Fatalf("insert price: %v", err)
		}
	}
	insert(model, "1024x1024", "standard", "0.04", nil)
	insert(model, "1792x1024", "hd", "0.12", nil)
	insert(model, "1024x1792", "hd", "0.12", nil)
	flux := "flux-it-" + model
	insert(flux, "", "", nil, "0.05")

	calc := cost.New(cost.Config{DB: cost.NewPoolQuerier(pool), CacheTTL: time.Millisecond})
	for _, tc := range []struct {
		name, model, size, quality string
		want                       int64
		ok                         bool
	}{
		{"standard 1024", model, "1024x1024", "standard", 40_000_000, true},
		{"hd 1792x1024 is 3x, not the flat estimate", model, "1792x1024", "hd", 120_000_000, true},
		{"sheet spelling of the size", model, "1792-x-1024", "HD", 120_000_000, true},
		{"unnamed size and quality takes the highest price", model, "", "", 120_000_000, true},
		{"a size with no row", model, "256x256", "standard", 0, false},
		{"per-pixel price takes the requested size", flux, "1024x1024", "", 52_428_800, true},
		{"per-pixel price with no readable size", flux, "auto", "", 0, false},
		{"unknown model", "no-such-model-" + model, "1024x1024", "", 0, false},
	} {
		time.Sleep(2 * time.Millisecond) // let the 1 ms cache expire
		got, ok := calc.ImageUnitNano(ctx, "openai", tc.model, tc.size, tc.quality)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: ImageUnitNano = %d, %v; want %d, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}

	// The CHECK constraints refuse a row with no price and a negative price.
	if _, err := pool.Exec(ctx, `INSERT INTO gateway.gateway_model_image_prices (provider, model_name) VALUES ('openai', 'dall-e-3-it-none')`); err == nil {
		t.Error("a row with no price must be refused")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway.gateway_model_image_prices (provider, model_name, usd_per_image) VALUES ('openai', 'dall-e-3-it-neg', -1)`); err == nil {
		t.Error("a negative price must be refused")
	}
}
