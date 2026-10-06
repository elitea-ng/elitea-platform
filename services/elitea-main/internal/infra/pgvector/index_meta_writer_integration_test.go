package pgvector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/jackc/pgx/v5"
)

// TestCurrentIndexMetaWriterRealPgvector crosses the production pgx adapter,
// PostgreSQL DDL/JSONB transactions, the vector type, history compatibility,
// idempotent recovery, and the same-index conflict guard. When
// ELITEA_TEST_SDK_PYTHON is set, it also crosses the installed SDK's real
// index_meta_init/index_meta_update lifecycle. It does not exercise the runtime
// admission database, Configurations/vault lookup, the command bus, or a worker.
func TestCurrentIndexMetaWriterRealPgvector(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the current index-meta writer test")
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
		t.Skip("the ELITEA_TEST_DATABASE_URL server does not provide the vector extension")
	}
	if _, err := connection.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Skipf("ELITEA_TEST_DATABASE_URL cannot install the vector extension: %v", err)
	}

	schemaID := int32(1_500_000_000 + time.Now().UnixNano()%500_000_000)
	schema := pgx.Identifier{strconv.FormatInt(int64(schemaID), 10)}.Sanitize()
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanupContext, "DROP SCHEMA "+schema+" CASCADE")
	})
	target := indexingapp.CurrentIndexMetaTarget{
		ConnectionString: databaseURL,
		SchemaID:         schemaID,
	}
	first := currentIndexMetaIntegrationRecord(t, schemaID, "meta-1", "execution-1", "message-1")
	writer := NewCurrentIndexMetaWriter()
	if err := writer.MaterializeInitial(ctx, target, first); err != nil {
		t.Fatalf("create metadata: %v", err)
	}
	assertCurrentIndexMetaIntegrationRow(t, ctx, connection, schema, "meta-1", "meta-1", "execution-1", 1, 1)

	// Simulate a process crash after the external commit but before the runtime
	// database gate transition. The exact retry must not append history.
	if err := writer.MaterializeInitial(ctx, target, first); err != nil {
		t.Fatalf("recover committed metadata: %v", err)
	}
	assertCurrentIndexMetaIntegrationRow(t, ctx, connection, schema, "meta-1", "meta-1", "execution-1", 1, 1)

	// Reproduce the current synchronous SDK reset: the metadata generation is
	// unchanged, but task_id is cleared before the in-progress callback.
	if _, err := connection.Exec(ctx, `
UPDATE `+schema+`.langchain_pg_embedding
SET cmetadata = jsonb_set(cmetadata, '{task_id}', 'null'::jsonb)
WHERE id = 'meta-1'`); err != nil {
		t.Fatal(err)
	}
	restamp := indexingapp.CurrentTaskRestampIndexMeta{
		MetaID:          first.MetaID,
		ExecutionID:     first.ExecutionID,
		Generation:      first.Generation,
		IndexGeneration: first.IndexGeneration,
		IndexName:       first.IndexName,
		ToolkitID:       first.ToolkitID,
		CreatedOn:       1_700_000_000.25,
	}
	if err := writer.MaterializeTaskID(ctx, target, restamp); err != nil {
		t.Fatalf("restamp task id: %v", err)
	}
	// Simulate a process restart after the PgVector commit and before intent
	// resolution. The exact retry is a no-op.
	if err := NewCurrentIndexMetaWriter().MaterializeTaskID(
		ctx,
		target,
		restamp,
	); err != nil {
		t.Fatalf("recover committed task restamp: %v", err)
	}
	var taskID string
	if err := connection.QueryRow(ctx, `
SELECT cmetadata->>'task_id'
FROM `+schema+`.langchain_pg_embedding
WHERE id = 'meta-1'`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if taskID != first.ExecutionID {
		t.Fatalf("restamped task_id=%q", taskID)
	}
	assertCurrentIndexMetaIntegrationRow(t, ctx, connection, schema, "meta-1", "meta-1", "execution-1", 1, 1)
	staleCreatedOn := restamp
	staleCreatedOn.CreatedOn++
	if err := writer.MaterializeTaskID(
		ctx,
		target,
		staleCreatedOn,
	); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("stale created_on restamp error=%v", err)
	}

	conflicting := currentIndexMetaIntegrationRecord(t, schemaID, "meta-2", "execution-2", "message-2")
	conflicting = currentIndexMetaRecordWithGeneration(t, conflicting, 2)
	if err := writer.MaterializeInitial(ctx, target, conflicting); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("active same-index conflict=%v", err)
	}
	assertCurrentIndexMetaIntegrationRow(t, ctx, connection, schema, "meta-1", "meta-1", "execution-1", 1, 1)

	terminal := indexingapp.CurrentTerminalIndexMeta{
		MetaID:          first.MetaID,
		ExecutionID:     first.ExecutionID,
		Generation:      first.Generation,
		IndexGeneration: first.IndexGeneration,
		IndexName:       first.IndexName,
		ToolkitID:       first.ToolkitID,
		State:           indexingapp.CurrentIndexMetaFailed,
		OccurredAt:      time.Date(2026, time.July, 26, 12, 13, 14, 567_000_000, time.UTC),
		SafeError:       "A dependency is unavailable.",
	}
	if err := writer.MaterializeTerminal(ctx, target, terminal); err != nil {
		t.Fatalf("terminalize failed metadata: %v", err)
	}
	if err := writer.MaterializeTerminal(ctx, target, terminal); err != nil {
		t.Fatalf("recover committed terminal metadata: %v", err)
	}
	var terminalState string
	if err := connection.QueryRow(ctx, `
SELECT cmetadata->>'state'
FROM `+schema+`.langchain_pg_embedding
WHERE id = 'meta-1'`).Scan(&terminalState); err != nil {
		t.Fatal(err)
	}
	if terminalState != "failed" {
		t.Fatalf("terminal state=%q", terminalState)
	}
	assertCurrentIndexMetaIntegrationHistory(
		t,
		ctx,
		connection,
		schema,
		2,
		"failed",
		0,
	)
	if err := writer.MaterializeInitial(ctx, target, conflicting); err != nil {
		t.Fatalf("start reindex generation: %v", err)
	}
	assertCurrentIndexMetaIntegrationRow(t, ctx, connection, schema, "meta-1", "meta-2", "execution-2", 2, 2)
	if err := writer.MaterializeTerminal(
		ctx,
		target,
		terminal,
	); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("older terminal generation error=%v", err)
	}
	if err := writer.MaterializeTaskID(
		ctx,
		target,
		restamp,
	); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("older task restamp generation error=%v", err)
	}

	// Simulate the unchanged SDK's normal ownership of the run history: it
	// appends one exact run entry and replaces that entry as progress changes.
	simulateCurrentSDKTerminalIntegration(
		t,
		ctx,
		connection,
		schema,
		"completed",
		61,
	)
	secondTerminal := indexingapp.CurrentTerminalIndexMeta{
		MetaID:          conflicting.MetaID,
		ExecutionID:     conflicting.ExecutionID,
		Generation:      conflicting.Generation,
		IndexGeneration: conflicting.IndexGeneration,
		IndexName:       conflicting.IndexName,
		ToolkitID:       conflicting.ToolkitID,
		State:           indexingapp.CurrentIndexMetaCancelled,
		OccurredAt: time.Date(
			2026,
			time.July,
			26,
			12,
			14,
			0,
			0,
			time.UTC,
		),
	}
	if err := writer.MaterializeTerminal(
		ctx,
		target,
		secondTerminal,
	); err != nil {
		t.Fatalf("replace exact SDK run: %v", err)
	}
	if err := writer.MaterializeTerminal(
		ctx,
		target,
		secondTerminal,
	); err != nil {
		t.Fatalf("recover exact SDK terminal: %v", err)
	}
	assertCurrentIndexMetaIntegrationHistory(
		t,
		ctx,
		connection,
		schema,
		3,
		"cancelled",
		61,
	)

	// A later cancellation before the SDK starts has no exact run entry to
	// replace, so Main appends one generation-fenced terminal entry.
	third := currentIndexMetaRecordWithGeneration(
		t,
		currentIndexMetaIntegrationRecord(
			t,
			schemaID,
			"meta-3",
			"execution-3",
			"message-3",
		),
		3,
	)
	if err := writer.MaterializeInitial(ctx, target, third); err != nil {
		t.Fatalf("start third generation: %v", err)
	}
	assertCurrentIndexMetaIntegrationRow(
		t,
		ctx,
		connection,
		schema,
		"meta-1",
		"meta-3",
		"execution-3",
		3,
		3,
	)
	thirdTerminal := indexingapp.CurrentTerminalIndexMeta{
		MetaID:          third.MetaID,
		ExecutionID:     third.ExecutionID,
		Generation:      third.Generation,
		IndexGeneration: third.IndexGeneration,
		IndexName:       third.IndexName,
		ToolkitID:       third.ToolkitID,
		State:           indexingapp.CurrentIndexMetaCancelled,
		OccurredAt: time.Date(
			2026,
			time.July,
			26,
			12,
			15,
			0,
			0,
			time.UTC,
		),
	}
	if err := writer.MaterializeTerminal(
		ctx,
		target,
		thirdTerminal,
	); err != nil {
		t.Fatalf("append pre-SDK cancellation: %v", err)
	}
	assertCurrentIndexMetaIntegrationHistory(
		t,
		ctx,
		connection,
		schema,
		4,
		"cancelled",
		0,
	)

	t.Run("installed SDK owns one completed run entry", func(t *testing.T) {
		python := os.Getenv("ELITEA_TEST_SDK_PYTHON")
		if python == "" {
			t.Skip("set ELITEA_TEST_SDK_PYTHON to run the installed SDK lifecycle")
		}
		fourth := currentIndexMetaRecordWithGeneration(
			t,
			currentIndexMetaIntegrationRecord(
				t,
				schemaID,
				"meta-4",
				"execution-4",
				"message-4",
			),
			4,
		)
		if err := writer.MaterializeInitial(ctx, target, fourth); err != nil {
			t.Fatalf("start SDK-owned generation: %v", err)
		}
		assertCurrentIndexMetaIntegrationRow(
			t,
			ctx,
			connection,
			schema,
			"meta-1",
			"meta-4",
			"execution-4",
			4,
			4,
		)
		command := osexec.CommandContext(
			ctx,
			python,
			"testdata/sdk_index_meta_lifecycle.py",
		)
		command.Env = append(
			os.Environ(),
			"ELITEA_TEST_DATABASE_URL="+databaseURL,
			"ELITEA_TEST_SCHEMA="+strconv.FormatInt(int64(schemaID), 10),
			"ELITEA_TEST_TOOLKIT_ID="+strconv.FormatInt(int64(schemaID), 10),
			"ELITEA_TEST_INDEX_NAME="+fourth.IndexName,
		)
		if err := command.Run(); err != nil {
			t.Fatalf("installed SDK lifecycle failed: %v", err)
		}
		assertCurrentIndexMetaIntegrationHistory(
			t,
			ctx,
			connection,
			schema,
			5,
			"completed",
			61,
		)
	})

	t.Run("adopts current-baseline top level from typed Go history", func(t *testing.T) {
		sixteenth := currentIndexMetaRecordWithGeneration(
			t,
			currentIndexMetaIntegrationRecord(
				t,
				schemaID,
				"meta-16",
				"execution-16",
				"message-16",
			),
			16,
		)
		sixteenth = currentIndexMetaRecordWithIndexName(
			t,
			sixteenth,
			"new13",
		)
		stored, baselineHistory := currentHybridLiveStoredForTest(
			t,
			sixteenth,
		)
		stored.id = "hybrid-physical-row"
		encoded, err := json.Marshal(stored.metadata)
		if err != nil {
			t.Fatal(err)
		}
		if len(baselineHistory) != 37 ||
			len(encoded) != currentHybridLiveMetadataBytes {
			t.Fatalf(
				"history entries=%d metadata bytes=%d",
				len(baselineHistory),
				len(encoded),
			)
		}
		if _, err := connection.Exec(
			ctx,
			`UPDATE `+schema+`.langchain_pg_embedding
SET id = $1, document = $2, cmetadata = $3::jsonb
WHERE cmetadata @> '{"type":"index_meta","collection":"Docs"}'::jsonb`,
			stored.id,
			*stored.document,
			encoded,
		); err != nil {
			t.Fatal(err)
		}

		if err := writer.MaterializeInitial(
			ctx,
			target,
			sixteenth,
		); err != nil {
			t.Fatalf("adopt hybrid metadata: %v", err)
		}
		if err := NewCurrentIndexMetaWriter().MaterializeInitial(
			ctx,
			target,
			sixteenth,
		); err != nil {
			t.Fatalf("recover committed hybrid adoption: %v", err)
		}

		var storedID, document string
		var raw []byte
		if err := connection.QueryRow(ctx, `
	SELECT id, document, cmetadata
	FROM `+schema+`.langchain_pg_embedding
	WHERE cmetadata @> '{"type":"index_meta","collection":"new13"}'::jsonb`,
		).Scan(&storedID, &document, &raw); err != nil {
			t.Fatal(err)
		}
		metadata := mustDecodeCurrentIndexMeta(t, raw)
		history := currentIndexMetaHistory(metadata["history"])
		if storedID != stored.id || document != sixteenth.Document ||
			metadata["index_meta_id"] != sixteenth.MetaID ||
			metadata["execution_id"] != sixteenth.ExecutionID ||
			metadata["index_generation"] != json.Number("16") ||
			metadata["state"] != "in_progress" ||
			!reflect.DeepEqual(history, baselineHistory) {
			t.Fatalf(
				"id=%q document=%q metadata=%#v history=%#v",
				storedID,
				document,
				metadata,
				history,
			)
		}
	})
}

func currentIndexMetaIntegrationRecord(
	t *testing.T,
	toolkitID int32,
	metaID string,
	executionID string,
	correlationID string,
) indexingapp.CurrentInitialIndexMeta {
	t.Helper()
	metadata, err := json.Marshal(map[string]any{
		"collection":           "Docs",
		"type":                 "index_meta",
		"indexed":              0,
		"updated":              0,
		"state":                "in_progress",
		"index_configuration":  map[string]any{"index_name": "Docs"},
		"created_on":           1_700_000_000.25,
		"updated_on":           1_700_000_000.25,
		"task_id":              executionID,
		"conversation_id":      nil,
		"toolkit_id":           toolkitID,
		"execution_id":         executionID,
		"execution_generation": 1,
		"index_generation":     1,
		"index_meta_id":        metaID,
		"correlation_id":       correlationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return indexingapp.CurrentInitialIndexMeta{
		MetaID:          metaID,
		ExecutionID:     executionID,
		CorrelationID:   correlationID,
		Generation:      1,
		IndexGeneration: 1,
		IndexName:       "Docs",
		ToolkitID:       toolkitID,
		Document:        "index_meta_Docs",
		InitialMetadata: metadata,
	}
}

func assertCurrentIndexMetaIntegrationRow(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema string,
	physicalID string,
	metaID string,
	executionID string,
	historyLength int,
	indexGeneration int64,
) {
	t.Helper()
	var storedID, document string
	var raw []byte
	if err := connection.QueryRow(ctx, `
SELECT id, document, cmetadata
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata @> '{"type":"index_meta","collection":"Docs"}'::jsonb`,
	).Scan(&storedID, &document, &raw); err != nil {
		t.Fatal(err)
	}
	metadata := mustDecodeCurrentIndexMeta(t, raw)
	history, ok := metadata["history"].(string)
	if !ok {
		t.Fatalf("history type=%T", metadata["history"])
	}
	historyItems := currentIndexMetaHistory(history)
	if len(historyItems) == 0 {
		t.Fatal("history has no permanent created marker")
	}
	assertCurrentIndexMetaCreatedMarker(t, historyItems[0], "Docs")
	if storedID != physicalID || document != "index_meta_Docs" ||
		metadata["index_meta_id"] != metaID ||
		metadata["execution_id"] != executionID ||
		metadata["execution_generation"] != json.Number("1") ||
		metadata["index_generation"] != json.Number(strconv.FormatInt(indexGeneration, 10)) ||
		metadata["state"] != "in_progress" ||
		len(historyItems) != historyLength {
		t.Fatalf("id=%q document=%q metadata=%#v", storedID, document, metadata)
	}
}

func simulateCurrentSDKTerminalIntegration(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema string,
	state string,
	indexed int64,
) {
	t.Helper()
	var raw []byte
	if err := connection.QueryRow(ctx, `
SELECT cmetadata
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata @> '{"type":"index_meta","collection":"Docs"}'::jsonb`,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	metadata := mustDecodeCurrentIndexMeta(t, raw)
	history, historyOK := decodeCurrentIndexMetaHistory(metadata["history"])
	if !historyOK {
		t.Fatalf("history=%#v", metadata["history"])
	}
	metadata["state"] = state
	metadata["indexed"] = indexed
	metadata["updated"] = int64(0)
	metadata["task_id"] = nil
	metadata["skipped"] = `{"total_skipped":5}`
	entry := cloneCurrentIndexMetaObject(metadata)
	delete(entry, "history")
	history = append(history, entry)
	encoded, err := encodeCurrentIndexMetaWithHistory(entry, history)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(
		ctx,
		`UPDATE `+schema+`.langchain_pg_embedding
SET cmetadata = $1::jsonb
WHERE id = 'meta-1'`,
		encoded,
	); err != nil {
		t.Fatal(err)
	}
}

func assertCurrentIndexMetaIntegrationHistory(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema string,
	historyLength int,
	lastState string,
	indexed int64,
) {
	t.Helper()
	var raw []byte
	if err := connection.QueryRow(ctx, `
SELECT cmetadata
FROM `+schema+`.langchain_pg_embedding
WHERE cmetadata @> '{"type":"index_meta","collection":"Docs"}'::jsonb`,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	metadata := mustDecodeCurrentIndexMeta(t, raw)
	history, historyOK := decodeCurrentIndexMetaHistory(metadata["history"])
	if !historyOK || len(history) != historyLength {
		t.Fatalf("history=%#v", metadata["history"])
	}
	assertCurrentIndexMetaCreatedMarker(t, history[0], "Docs")
	last, ok := history[len(history)-1].(map[string]any)
	if !ok ||
		metadata["state"] != lastState ||
		last["state"] != lastState ||
		metadata["indexed"] != json.Number(strconv.FormatInt(indexed, 10)) ||
		last["indexed"] != json.Number(strconv.FormatInt(indexed, 10)) {
		t.Fatalf("metadata=%#v history=%#v", metadata, history)
	}
}
