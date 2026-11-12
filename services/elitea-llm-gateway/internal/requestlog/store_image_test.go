package requestlog

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// imageExecer refuses a statement that names pricing_source with 42703 while
// missing0242 is set: a database shared migration 0242 has not reached.
type imageExecer struct {
	missing0242 bool
	statements  []string
	args        [][]any
}

func (e *imageExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.statements = append(e.statements, sql)
	e.args = append(e.args, args)
	if e.missing0242 && strings.Contains(sql, "pricing_source") {
		return pgconn.CommandTag{}, &pgconn.PgError{Code: "42703", Message: `column "pricing_source" of relation "llm_request_logs" does not exist`}
	}
	return pgconn.CommandTag{}, nil
}

func imageBatch() []Record {
	return []Record{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "openai", Model: "dall-e-3", Image: &ImageFields{
			Count: 2, Size: "1792x1024", Quality: "hd", PricingSource: PricingSourceCatalogPerImage,
		}},
	}
}

func TestRequestLogWritesTheImageColumns(t *testing.T) {
	db := &imageExecer{}
	if err := NewStore(db).WriteBatch(context.Background(), imageBatch()); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 1 || !strings.Contains(db.statements[0], "kind, image_count, image_size, image_quality, pricing_source") {
		t.Fatalf("statements = %q, want one insert with the 0242 columns", db.statements)
	}
	args := db.args[0]
	perRow := columnsPerRow + imageColumnsPerRow
	if len(args) != 2*perRow {
		t.Fatalf("%d arguments, want %d", len(args), 2*perRow)
	}
	model := args[columnsPerRow:perRow]
	for _, v := range model {
		if v != nil {
			t.Fatalf("a model row's image columns = %v, want all NULL", model)
		}
	}
	image := args[perRow+columnsPerRow:]
	if image[0] != "image" || image[1] != int64(2) || image[2] != "1792x1024" || image[3] != "hd" || image[4] != "catalog_per_image" {
		t.Fatalf("the image row's columns = %v", image)
	}
}

func TestRequestLogImageColumnsAreNullWhenEmpty(t *testing.T) {
	db := &imageExecer{}
	rec := []Record{{Image: &ImageFields{}}}
	if err := NewStore(db).WriteBatch(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	image := db.args[0][columnsPerRow:]
	if image[0] != "image" || image[1] != int64(0) || image[2] != nil || image[3] != nil || image[4] != nil {
		t.Fatalf("a failed image request's columns = %v, want image, 0 and three NULLs", image)
	}
}

func TestRequestLogFallsBackWhen0242IsAbsent(t *testing.T) {
	db := &imageExecer{missing0242: true}
	store := NewStore(db)
	if err := store.WriteBatch(context.Background(), imageBatch()); err != nil {
		t.Fatalf("a missing 0242 must not lose the batch: %v", err)
	}
	if len(db.statements) != 2 || strings.Contains(db.statements[1], "pricing_source") {
		t.Fatalf("statements = %q, want the image insert and then the plain one", db.statements)
	}
	// The latch holds: the next batch skips the refused statement.
	if err := store.WriteBatch(context.Background(), imageBatch()); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 3 || strings.Contains(db.statements[2], "pricing_source") {
		t.Fatalf("a latched store tried the 0242 columns again: %q", db.statements)
	}
}

func TestEnrichmentSetImageLastCallWinsAndSealDropsLateWrites(t *testing.T) {
	e := &Enrichment{}
	e.SetImage(ImageFields{Size: "1024x1024"})
	e.SetImage(ImageFields{Count: 2, Size: "1024x1024", PricingSource: PricingSourceFallbackEstimate})
	got := e.seal()
	if got.image == nil || got.image.Count != 2 || got.image.PricingSource != PricingSourceFallbackEstimate {
		t.Fatalf("sealed image = %+v", got.image)
	}
	e.SetImage(ImageFields{Count: 9})
	var nilE *Enrichment
	nilE.SetImage(ImageFields{}) // must not panic
}
