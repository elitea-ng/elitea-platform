package pgvector

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"strconv"
	"testing"
	"time"

	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/jackc/pgx/v5"
)

func TestCurrentIndexMetaRemoverRealPgvectorSafetyAndBehavior(t *testing.T) {
	databaseURL := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the current index metadata delete test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	normalized, ok := normalizeCurrentPgvectorDSN(databaseURL)
	if !ok {
		t.Fatal("ELITEA_TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	config, err := pgx.ParseConfig(normalized)
	if err != nil {
		t.Fatal("ELITEA_TEST_DATABASE_URL is invalid")
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect to ELITEA_TEST_DATABASE_URL")
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })

	schemaID := int32(1_000_000_000 + time.Now().UnixNano()%500_000_000)
	schema := pgx.Identifier{
		strconv.FormatInt(int64(schemaID), 10),
	}.Sanitize()
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cleanupCancel()
		_, _ = connection.Exec(
			cleanupContext,
			"DROP SCHEMA "+schema+" CASCADE",
		)
	})
	if _, err := connection.Exec(ctx, `
CREATE SCHEMA `+schema+`;
CREATE TABLE `+schema+`.langchain_pg_embedding (
    id TEXT PRIMARY KEY,
    document TEXT,
    cmetadata JSONB
);
INSERT INTO `+schema+`.langchain_pg_embedding (id, document, cmetadata)
VALUES
    ('malicious-chunk', 'chunk',
     '{"type":"document","collection":"Docs"}'::jsonb),
    ('docs-meta', 'index_meta_Docs',
     '{"type":"index_meta","collection":"Docs"}'::jsonb),
    ('docs-chunk', 'chunk',
     '{"type":"document","collection":"Docs"}'::jsonb),
    ('other-meta', 'index_meta_Other',
     '{"type":"index_meta","collection":"Other"}'::jsonb),
    ('other-chunk', 'chunk',
     '{"type":"document","collection":"Other"}'::jsonb),
    ('numeric-seven-meta', 'invalid numeric collection',
     '{"type":"index_meta","collection":7}'::jsonb),
    ('numeric-seven-peer', 'numeric collision peer',
     '{"type":"document","collection":7}'::jsonb),
    ('string-seven-meta', 'string collection',
     '{"type":"index_meta","collection":"7"}'::jsonb),
    ('string-seven-peer', 'string collection peer',
     '{"type":"document","collection":"7"}'::jsonb),
    ('empty-meta', 'invalid empty collection',
     '{"type":"index_meta","collection":""}'::jsonb),
    ('object-meta', 'invalid object collection',
     '{"type":"index_meta","collection":{"name":"Docs"}}'::jsonb),
    ('array-meta', 'invalid array collection',
     '{"type":"index_meta","collection":["Docs"]}'::jsonb),
    ('null-meta', 'invalid null collection',
     '{"type":"index_meta","collection":null}'::jsonb),
    ('missing-meta', 'missing collection',
     '{"type":"index_meta"}'::jsonb),
    ('oversized-meta', 'oversized collection',
     jsonb_build_object(
         'type', 'index_meta',
         'collection', repeat('x', 4097)
     ))`); err != nil {
		t.Fatal(err)
	}

	target := indexmetaapp.ResolvedTarget{
		ConnectionString: databaseURL,
		SchemaID:         schemaID,
	}
	remover := NewCurrentIndexMetaRemover()

	// Approved security correction: an arbitrary document row ID cannot select
	// and delete its whole collection.
	if _, err := remover.Delete(
		ctx,
		target,
		"malicious-chunk",
	); !errors.Is(err, indexmetaapp.ErrCurrentIndexMetaNotFound) {
		t.Fatalf("malicious row error=%v", err)
	}
	assertCurrentIndexCollectionRows(
		t,
		ctx,
		connection,
		schema,
		"Docs",
		3,
	)

	for _, indexMetaID := range []string{
		"numeric-seven-meta",
		"empty-meta",
		"object-meta",
		"array-meta",
		"null-meta",
		"missing-meta",
		"oversized-meta",
	} {
		t.Run("invalid_collection_"+indexMetaID, func(t *testing.T) {
			before := currentIndexTotalRows(t, ctx, connection, schema)
			indexName, err := remover.Delete(ctx, target, indexMetaID)
			if indexName != "" || !errors.Is(err, ErrCurrentIndexMetaDelete) {
				t.Fatalf("index=%q error=%v", indexName, err)
			}
			after := currentIndexTotalRows(t, ctx, connection, schema)
			if after != before {
				t.Fatalf(
					"invalid collection mutated rows: before=%d after=%d",
					before,
					after,
				)
			}
		})
	}

	indexName, err := remover.Delete(ctx, target, "string-seven-meta")
	if err != nil || indexName != "7" {
		t.Fatalf("delete string collection index=%q err=%v", indexName, err)
	}
	assertCurrentIndexJSONCollectionRows(
		t,
		ctx,
		connection,
		schema,
		`"7"`,
		0,
	)
	assertCurrentIndexJSONCollectionRows(
		t,
		ctx,
		connection,
		schema,
		`7`,
		2,
	)

	indexName, err = remover.Delete(ctx, target, "docs-meta")
	if err != nil || indexName != "Docs" {
		t.Fatalf("delete index=%q err=%v", indexName, err)
	}
	assertCurrentIndexCollectionRows(
		t,
		ctx,
		connection,
		schema,
		"Docs",
		0,
	)
	assertCurrentIndexCollectionRows(
		t,
		ctx,
		connection,
		schema,
		"Other",
		2,
	)

	if _, err := remover.Delete(
		ctx,
		target,
		"docs-meta",
	); !errors.Is(err, indexmetaapp.ErrCurrentIndexMetaNotFound) {
		t.Fatalf("second delete error=%v", err)
	}

	missingTarget := target
	missingTarget.SchemaID--
	if _, err := remover.Delete(
		ctx,
		missingTarget,
		"docs-meta",
	); !errors.Is(err, ErrCurrentIndexMetaDelete) {
		t.Fatalf("missing table error=%v", err)
	}
}

func currentIndexTotalRows(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema string,
) int {
	t.Helper()
	var count int
	if err := connection.QueryRow(
		ctx,
		`SELECT count(*) FROM `+schema+`.langchain_pg_embedding`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertCurrentIndexJSONCollectionRows(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema, collectionJSON string,
	want int,
) {
	t.Helper()
	var count int
	if err := connection.QueryRow(ctx, `
SELECT count(*)
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata->'collection' = $1::jsonb`,
		collectionJSON,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf(
			"JSON collection %q rows=%d want=%d",
			collectionJSON,
			count,
			want,
		)
	}
}

func assertCurrentIndexCollectionRows(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema, collection string,
	want int,
) {
	t.Helper()
	var count int
	if err := connection.QueryRow(ctx, `
SELECT count(*)
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata->'collection' = to_jsonb($1::text)`,
		collection,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("collection %q rows=%d want=%d", collection, count, want)
	}
}
