package pgvector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/jackc/pgx/v5"
)

// The SAVE half of the index editor's Save / Save & Reindex split, against a
// real PostgreSQL. What the fake cannot answer is exactly what this test is
// for: that the statement replaces `index_configuration` and nothing else, and
// that an index that does not exist is a refusal rather than a silent success.
func TestCurrentIndexConfigurationWriterRealPgvector(t *testing.T) {
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the current index configuration test")
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
	schema := pgx.Identifier{strconv.FormatInt(int64(schemaID), 10)}.Sanitize()
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanupContext, "DROP SCHEMA "+schema+" CASCADE")
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
    ('docs-meta', 'index_meta_Docs',
     '{"type":"index_meta","collection":"Docs","state":"completed","indexed":10,
       "task_id":"run-1","history":[{"state":"completed"}],
       "index_configuration":"{\"progress_step\": 50}"}'::jsonb),
    ('docs-chunk', 'chunk', '{"type":"document","collection":"Docs"}'::jsonb),
    ('other-meta', 'index_meta_Other',
     '{"type":"index_meta","collection":"Other","state":"completed",
       "index_configuration":"{\"progress_step\": 10}"}'::jsonb)`); err != nil {
		t.Fatal(err)
	}

	target := indexmetaapp.ResolvedTarget{ConnectionString: databaseURL, SchemaID: schemaID}
	writer := NewCurrentIndexConfigurationWriter()

	saved := `{"index_name":"Docs","progress_step":75,"clean_index":true}`
	if err := writer.SaveConfiguration(ctx, target, "Docs", json.RawMessage(saved)); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The stored form is a JSON STRING, the form a run writes and the form the
	// list projection decodes.
	stored := currentIndexMetadataForTest(t, ctx, connection, schema, "docs-meta")
	encoded, ok := stored["index_configuration"].(string)
	if !ok {
		t.Fatalf("index_configuration=%#v, want a JSON string", stored["index_configuration"])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("stored configuration is not JSON: %q", encoded)
	}
	if decoded["progress_step"] != float64(75) || decoded["clean_index"] != true {
		t.Fatalf("stored configuration=%v", decoded)
	}

	// The run path owns everything else on this document. A configuration
	// save must not advance or clear any of it — that is the whole difference
	// between "Save" and "Save & Reindex".
	if stored["state"] != "completed" || stored["task_id"] != "run-1" || stored["indexed"] != float64(10) {
		t.Fatalf("the save mutated run-owned fields: %#v", stored)
	}
	if history, ok := stored["history"].([]any); !ok || len(history) != 1 {
		t.Fatalf("history=%#v", stored["history"])
	}

	// A sibling index is untouched.
	other := currentIndexMetadataForTest(t, ctx, connection, schema, "other-meta")
	if other["index_configuration"] != `{"progress_step": 10}` {
		t.Fatalf("a save for Docs changed Other: %#v", other["index_configuration"])
	}

	// A chunk row carrying the same collection is not an index_meta document
	// and must never be written to.
	chunk := currentIndexMetadataForTest(t, ctx, connection, schema, "docs-chunk")
	if _, present := chunk["index_configuration"]; present {
		t.Fatalf("the save wrote a configuration onto a document row: %#v", chunk)
	}

	if err := writer.SaveConfiguration(ctx, target, "NoSuchIndex", json.RawMessage(`{"a":1}`)); !errors.Is(err, indexmetaapp.ErrCurrentIndexMetaNotFound) {
		t.Fatalf("missing index err=%v, want ErrCurrentIndexMetaNotFound", err)
	}
}

func currentIndexMetadataForTest(
	t *testing.T,
	ctx context.Context,
	connection *pgx.Conn,
	schema string,
	id string,
) map[string]any {
	t.Helper()
	var raw []byte
	if err := connection.QueryRow(ctx,
		`SELECT cmetadata FROM `+schema+`.langchain_pg_embedding WHERE id = $1`, id,
	).Scan(&raw); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decode %s: %v", id, err)
	}
	return metadata
}
