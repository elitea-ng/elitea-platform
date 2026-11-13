package cost

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestImageUnitNanoReducesCallerTextToABoundedSet(t *testing.T) {
	db := &imageDB{perImage: ptr(1)}
	c := New(Config{DB: db})
	// Free-text size and quality must not reach the key or the statement.
	for _, size := range []string{"auto", "1024x1024; DROP", strings.Repeat("9", 40), "x", "10x", "axb"} {
		c.ImageUnitNano(context.Background(), "openai", "m", size, "ultra-"+size)
		if db.args[3] != "" || db.args[4] != "" {
			t.Fatalf("size %q reached the statement as %q / %q", size, db.args[3], db.args[4])
		}
	}
	c.ImageUnitNano(context.Background(), "openai", "m", "1024X1792", "AUTO")
	if db.args[3] != "1024x1792" || db.args[4] != "" {
		t.Fatalf("args = %v; auto quality must be unspecified", db.args)
	}
}

func TestImageUnitNanoCacheIsBounded(t *testing.T) {
	c := New(Config{DB: &imageDB{perImage: ptr(1)}})
	for i := range 3 * maxImageCacheEntries {
		c.ImageUnitNano(context.Background(), "openai", "model-"+strconv.Itoa(i), "1024x1024", "")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.images) > maxImageCacheEntries {
		t.Fatalf("cache holds %d entries, bound is %d", len(c.images), maxImageCacheEntries)
	}
}

func TestImageUnitNanoCachesAFailureNegatively(t *testing.T) {
	db := &imageDB{err: errors.New("bigint out of range")}
	c := New(Config{DB: db})
	for range 5 {
		if _, ok := c.ImageUnitNano(context.Background(), "openai", "dall-e-3", "1024x1024", ""); ok {
			t.Fatal("a failed lookup must not price")
		}
	}
	if db.queries != 1 {
		t.Fatalf("%d queries, want 1: the failure must be cached", db.queries)
	}
}

func TestTokenPriced(t *testing.T) {
	var nilCalc *Calculator
	if nilCalc.TokenPriced(context.Background(), "openai", "gpt-image-1") {
		t.Fatal("a nil calculator prices nothing")
	}
	if New(Config{}).TokenPriced(context.Background(), "openai", "gpt-image-1") {
		t.Fatal("the default table is not a catalog price")
	}
}
