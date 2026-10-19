package repos

import (
	"context"
	"sync"
	"testing"
)

// The operator's attestation of storage encryption is audited ONCE: replicas
// that start together, and every later restart, find the row.
func TestPostgresCloudExecutorAttestationIsAuditedOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	// centry.audit_events is the legacy-baseline table
	// (internal/infra/db/migrations/001_initial.sql), which the shared
	// migrations do not create. The columns are that file's.
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS centry.audit_events (
    id SERIAL PRIMARY KEY, timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(), user_id INTEGER,
    user_email VARCHAR(256), project_id INTEGER, event_type VARCHAR(32) NOT NULL, action VARCHAR(512) NOT NULL,
    http_method VARCHAR(10), http_route VARCHAR(512), status_code SMALLINT, duration_ms DOUBLE PRECISION,
    is_error BOOLEAN NOT NULL DEFAULT FALSE, entity_type VARCHAR(32), entity_id INTEGER, entity_name VARCHAR(256),
    trace_id VARCHAR(32), span_id VARCHAR(16), parent_span_id VARCHAR(16))`); err != nil {
		t.Fatal(err)
	}
	buckets := []string{"elitea-attest-test"}
	var wg sync.WaitGroup
	results := make([]bool, 6)
	errs := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = RecordCloudExecutorEncryptionAttestation(ctx, pool, buckets)
		}(i)
	}
	wg.Wait()
	written := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("attestation %d: %v", i, errs[i])
		}
		if results[i] {
			written++
		}
	}
	if written != 1 {
		t.Fatalf("%d calls wrote the row, want exactly 1", written)
	}
	if again, err := RecordCloudExecutorEncryptionAttestation(ctx, pool, buckets); err != nil || again {
		t.Fatalf("a restart = %t, %v; want no second row", again, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM centry.audit_events
WHERE entity_type = 'cloud_executor' AND event_type = 'admin_task' AND action = $1`,
		CloudExecutorAttestationAction(buckets)).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("audit rows = %d, %v; want 1", rows, err)
	}
	// Another set of buckets is a new attestation.
	if other, err := RecordCloudExecutorEncryptionAttestation(ctx, pool, []string{"elitea-attest-test", "dedicated"}); err != nil || !other {
		t.Fatalf("a new bucket set = %t, %v; want a row", other, err)
	}
}
