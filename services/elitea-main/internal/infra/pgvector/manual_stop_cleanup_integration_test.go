package pgvector

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"strconv"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/jackc/pgx/v5"
)

// TestPostgresServiceBackedCurrentManualStopCleanupPgvector crosses the real
// external transaction, JSONB predicates, generation fence, and idempotent
// retry. It deliberately proves the current baseline's narrow NULL behavior:
// missing/null cmetadata.type rows survive.
func TestPostgresServiceBackedCurrentManualStopCleanupPgvector(t *testing.T) {
	databaseURL := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the manual Stop PgVector test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

	var vectorAvailable bool
	if err := connection.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'vector'
)`).Scan(&vectorAvailable); err != nil {
		t.Fatal(err)
	}
	if !vectorAvailable {
		t.Skip("the ELITEA_TEST_DATABASE_URL server does not provide vector")
	}
	if _, err := connection.Exec(
		ctx,
		`CREATE EXTENSION IF NOT EXISTS vector`,
	); err != nil {
		t.Skipf("ELITEA_TEST_DATABASE_URL cannot install vector: %v", err)
	}

	schemaID := int32(1_400_000_000 + time.Now().UnixNano()%500_000_000)
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
	target := indexingapp.CurrentIndexMetaTarget{
		ConnectionString: databaseURL,
		SchemaID:         schemaID,
	}
	initial := currentIndexMetaIntegrationRecord(
		t,
		schemaID,
		"meta-1",
		"execution-1",
		"message-1",
	)
	writer := NewCurrentIndexMetaWriter()
	if err := writer.MaterializeInitial(ctx, target, initial); err != nil {
		t.Fatal(err)
	}
	terminal := indexingapp.CurrentTerminalIndexMeta{
		MetaID:          initial.MetaID,
		ExecutionID:     initial.ExecutionID,
		Generation:      initial.Generation,
		IndexGeneration: initial.IndexGeneration,
		IndexName:       initial.IndexName,
		ToolkitID:       initial.ToolkitID,
		State:           indexingapp.CurrentIndexMetaCancelled,
		OccurredAt: time.Date(
			2026,
			time.July,
			27,
			12,
			13,
			14,
			0,
			time.UTC,
		),
	}
	if err := writer.MaterializeTerminal(ctx, target, terminal); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
INSERT INTO `+schema+`.langchain_pg_embedding (id, document, cmetadata)
VALUES
    ('explicit-page', 'page', '{"collection":"Docs","type":"page"}'),
    ('missing-type', 'missing', '{"collection":"Docs"}'),
    ('null-type', 'null', '{"collection":"Docs","type":null}'),
    ('other-index', 'other', '{"collection":"Other","type":"page"}')`); err != nil {
		t.Fatal(err)
	}
	var metadataBefore string
	if err := connection.QueryRow(ctx, `
SELECT cmetadata::text
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata @> '{"type":"index_meta"}'::jsonb
  AND cmetadata->>'collection' = 'Docs'`).Scan(&metadataBefore); err != nil {
		t.Fatal(err)
	}

	cleanup := indexingapp.CurrentManualStopCleanup{
		MetaID:          initial.MetaID,
		ExecutionID:     initial.ExecutionID,
		Generation:      initial.Generation,
		IndexGeneration: initial.IndexGeneration,
		IndexName:       initial.IndexName,
		ToolkitID:       initial.ToolkitID,
	}
	for attempt := range 2 {
		if err := writer.CleanupManualStop(ctx, target, cleanup); err != nil {
			t.Fatalf("cleanup attempt %d: %v", attempt+1, err)
		}
	}
	assertCurrentManualStopRowPresence(
		t,
		ctx,
		connection,
		schema,
		"explicit-page",
		false,
	)
	for _, id := range []string{"missing-type", "null-type", "other-index"} {
		assertCurrentManualStopRowPresence(
			t,
			ctx,
			connection,
			schema,
			id,
			true,
		)
	}
	var metadataAfter string
	if err := connection.QueryRow(ctx, `
SELECT cmetadata::text
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata @> '{"type":"index_meta"}'::jsonb
  AND cmetadata->>'collection' = 'Docs'`).Scan(&metadataAfter); err != nil {
		t.Fatal(err)
	}
	if metadataAfter != metadataBefore {
		t.Fatalf(
			"cleanup changed index_meta/history:\nbefore=%s\nafter=%s",
			metadataBefore,
			metadataAfter,
		)
	}

	next := currentIndexMetaRecordWithGeneration(
		t,
		currentIndexMetaIntegrationRecord(
			t,
			schemaID,
			"meta-2",
			"execution-2",
			"message-2",
		),
		2,
	)
	if err := writer.MaterializeInitial(ctx, target, next); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
INSERT INTO `+schema+`.langchain_pg_embedding (id, document, cmetadata)
VALUES ('new-generation-page', 'new', '{"collection":"Docs","type":"page"}')`); err != nil {
		t.Fatal(err)
	}
	if err := writer.CleanupManualStop(
		ctx,
		target,
		cleanup,
	); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("stale cleanup error=%v", err)
	}
	assertCurrentManualStopRowPresence(
		t,
		ctx,
		connection,
		schema,
		"new-generation-page",
		true,
	)

	missingMetaSchemaID := schemaID + 1
	missingMetaSchema := pgx.Identifier{
		strconv.FormatInt(int64(missingMetaSchemaID), 10),
	}.Sanitize()
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cleanupCancel()
		_, _ = connection.Exec(
			cleanupContext,
			"DROP SCHEMA "+missingMetaSchema+" CASCADE",
		)
	})
	missingMetaTarget := indexingapp.CurrentIndexMetaTarget{
		ConnectionString: databaseURL,
		SchemaID:         missingMetaSchemaID,
	}
	missingMetaInitial := currentIndexMetaIntegrationRecord(
		t,
		missingMetaSchemaID,
		"meta-missing",
		"execution-missing",
		"message-missing",
	)
	if err := writer.MaterializeInitial(
		ctx,
		missingMetaTarget,
		missingMetaInitial,
	); err != nil {
		t.Fatal(err)
	}
	if err := writer.MaterializeTerminal(
		ctx,
		missingMetaTarget,
		indexingapp.CurrentTerminalIndexMeta{
			MetaID:          missingMetaInitial.MetaID,
			ExecutionID:     missingMetaInitial.ExecutionID,
			Generation:      missingMetaInitial.Generation,
			IndexGeneration: missingMetaInitial.IndexGeneration,
			IndexName:       missingMetaInitial.IndexName,
			ToolkitID:       missingMetaInitial.ToolkitID,
			State:           indexingapp.CurrentIndexMetaCancelled,
			OccurredAt:      terminal.OccurredAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
INSERT INTO `+missingMetaSchema+`.langchain_pg_embedding (
    id, document, cmetadata
) VALUES (
    'orphan-explicit-page', 'orphan', '{"collection":"Docs","type":"page"}'
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
DELETE FROM `+missingMetaSchema+`.langchain_pg_embedding
WHERE cmetadata @> '{"type":"index_meta"}'::jsonb
  AND cmetadata->>'collection' = 'Docs'`); err != nil {
		t.Fatal(err)
	}
	if err := writer.CleanupManualStop(
		ctx,
		missingMetaTarget,
		indexingapp.CurrentManualStopCleanup{
			MetaID:          missingMetaInitial.MetaID,
			ExecutionID:     missingMetaInitial.ExecutionID,
			Generation:      missingMetaInitial.Generation,
			IndexGeneration: missingMetaInitial.IndexGeneration,
			IndexName:       missingMetaInitial.IndexName,
			ToolkitID:       missingMetaInitial.ToolkitID,
		},
	); err != nil {
		t.Fatalf("missing metadata no-op: %v", err)
	}
	assertCurrentManualStopRowPresence(
		t,
		ctx,
		connection,
		missingMetaSchema,
		"orphan-explicit-page",
		true,
	)
}

func assertCurrentManualStopRowPresence(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema string,
	id string,
	want bool,
) {
	t.Helper()
	var present bool
	if err := connection.QueryRow(
		ctx,
		`SELECT EXISTS (
SELECT 1
FROM `+schema+`.langchain_pg_embedding
WHERE id = $1
)`,
		id,
	).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != want {
		t.Fatalf("row %q present=%v want=%v", id, present, want)
	}
}
