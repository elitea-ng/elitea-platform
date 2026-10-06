package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/redisdispatch"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	visibilityRepairServiceOptIn  = "ELITEA_RUNTIME_VISIBILITY_REPAIR_TEST"
	visibilityRepairRedisAddress  = "ELITEA_TEST_REDIS_ADDR"
	visibilityDeliveryIndexSuffix = ":delivery-index.v1"
)

// TestPostgresRedisServiceBackedVisibilityRepair is an opt-in, real-service
// integration test. It proves PostgreSQL periodically re-offers the exact
// prepared envelope, Redis deduplicates an ambiguous successful append retry,
// partial stream/index loss fails without mutation, coordinated two-key loss
// is repairable, and independent publishers converge on one entry/mapping. It
// is not a Redis primary-failover, Python-worker, or cross-process system test.
func TestPostgresRedisServiceBackedVisibilityRepair(t *testing.T) {
	if os.Getenv(visibilityRepairServiceOptIn) != "1" {
		t.Skipf("set %s=1 with %s and %s to run the visibility-repair integration test", visibilityRepairServiceOptIn, postgresIntegrationDatabaseURL, visibilityRepairRedisAddress)
	}
	redisAddress := os.Getenv(visibilityRepairRedisAddress)
	if redisAddress == "" {
		t.Skipf("set %s with %s", visibilityRepairRedisAddress, visibilityRepairServiceOptIn)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := newMigratedPostgresIntegrationPool(t)

	stream := fmt.Sprintf("elitea:test:visibility-repair:%d", time.Now().UnixNano())
	indexKey := stream + visibilityDeliveryIndexSuffix
	primaryRedis := newVisibilityRepairRedisClient(t, redisAddress)
	t.Cleanup(func() { closeVisibilityRepairRedisClient(t, primaryRedis) })
	if err := primaryRedis.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis visibility-repair service: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		if err := primaryRedis.Del(cleanupCtx, stream, indexKey).Err(); err != nil {
			t.Errorf("delete visibility-repair Redis keys: %v", err)
		}
	})

	policy := testDispatchPolicy()
	policy.StreamName = stream
	policy.DeadlineTTL = 10 * time.Minute
	policy.MaxOutstanding = 4
	jobs, err := NewExecutionJobsRepository(pool, policy)
	if err != nil {
		t.Fatal(err)
	}
	admission := postgresCapacityAdmission(9_001)
	now := time.Now().UTC()
	admission.Record.Job.CreatedAt = now
	admission.Record.Outbox.CreatedAt = now
	if outcome, err := jobs.AdmitValidation(ctx, admission); err != nil || !outcome.Created {
		t.Fatalf("admit visibility-repair execution: outcome=%+v err=%v", outcome, err)
	}
	outboxID := admission.Record.Outbox.ID

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := redisdispatch.NewEd25519CommandSigner("visibility-repair-ed25519-v1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	firstDispatcher := newVisibilityRepairDispatcher(t, pool, primaryRedis, policy, signer)
	publisher, err := executionapp.NewOutboxPublisher(
		mustVisibilityRepairOutbox(t, pool, stream),
		firstDispatcher,
		executionapp.OutboxPublisherConfig{
			PollInterval:      time.Second,
			VisibilityTimeout: executionapp.MinOutboxVisibilityTimeout,
			BatchSize:         4,
			MaxConcurrent:     2,
			ReportFailure:     func(error) {},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.RunOnce(ctx); err != nil {
		t.Fatalf("initial visibility publication: %v", err)
	}
	initialEntryID, initialEnvelope := assertVisibilityRepairRedisState(t, ctx, primaryRedis, stream, indexKey, outboxID)
	storedEnvelope, attempts := visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(storedEnvelope, initialEnvelope) || attempts != 1 {
		t.Fatalf("initial durable publication bytes_equal=%t attempts=%d, want true/1", bytes.Equal(storedEnvelope, initialEnvelope), attempts)
	}

	// Model an ambiguous append/MarkValidationPublished response loss by
	// retrying the exact durable envelope. Redis must return the existing
	// mapping rather than consume a second capacity slot.
	if err := firstDispatcher.Dispatch(ctx, outboxID); err != nil {
		t.Fatalf("deduplicate ambiguous publication retry: %v", err)
	}
	retriedEntryID, retriedEnvelope := assertVisibilityRepairRedisState(t, ctx, primaryRedis, stream, indexKey, outboxID)
	storedEnvelope, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if retriedEntryID != initialEntryID || !bytes.Equal(retriedEnvelope, initialEnvelope) || !bytes.Equal(storedEnvelope, initialEnvelope) || attempts != 2 {
		t.Fatalf("ambiguous retry entry_equal=%t redis_equal=%t postgres_equal=%t attempts=%d, want true/true/true/2", retriedEntryID == initialEntryID, bytes.Equal(retriedEnvelope, initialEnvelope), bytes.Equal(storedEnvelope, initialEnvelope), attempts)
	}

	// Losing only the stream entry is an ambiguous partial state. Publication
	// must fail closed and leave the surviving mapping untouched.
	if deleted, err := primaryRedis.XDel(ctx, stream, initialEntryID).Result(); err != nil || deleted != 1 {
		t.Fatalf("inject hash-only Redis state: deleted=%d err=%v", deleted, err)
	}
	ageVisibilityRepairRow(t, ctx, pool, outboxID)
	if err := publisher.RunOnce(ctx); !errors.Is(err, redisdispatch.ErrControlDeliveryIndexInconsistent) {
		t.Fatalf("hash-only Redis state did not fail closed: %v", err)
	}
	storedEnvelope, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if length, err := primaryRedis.XLen(ctx, stream).Result(); err != nil || length != 0 {
		t.Fatalf("hash-only rejection mutated stream: length=%d err=%v", length, err)
	}
	if mapped, err := primaryRedis.HGet(ctx, indexKey, outboxID).Result(); err != nil || mapped != initialEntryID {
		t.Fatalf("hash-only rejection mutated mapping=%q err=%v, want %q", mapped, err, initialEntryID)
	}
	if !bytes.Equal(storedEnvelope, initialEnvelope) || attempts != 2 {
		t.Fatalf("hash-only rejection changed PostgreSQL bytes=%t attempts=%d, want false/2", !bytes.Equal(storedEnvelope, initialEnvelope), attempts)
	}

	// A coordinated loss/restore of both Redis keys is repaired from the
	// PostgreSQL winner without re-signing or re-encoding.
	if err := primaryRedis.Del(ctx, stream, indexKey).Err(); err != nil {
		t.Fatalf("inject coordinated Redis key loss: %v", err)
	}
	ageVisibilityRepairRow(t, ctx, pool, outboxID)
	if err := publisher.RunOnce(ctx); err != nil {
		t.Fatalf("repair coordinated Redis key loss: %v", err)
	}
	_, restoredEnvelope := assertVisibilityRepairRedisState(t, ctx, primaryRedis, stream, indexKey, outboxID)
	_, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(restoredEnvelope, initialEnvelope) || attempts != 3 {
		t.Fatalf("coordinated-key repair changed bytes=%t attempts=%d, want true/3", !bytes.Equal(restoredEnvelope, initialEnvelope), attempts)
	}

	// Losing only the mapping is the inverse partial state. It must also fail
	// without replacing or deleting the surviving stream entry.
	currentEntries, err := primaryRedis.XRangeN(ctx, stream, "-", "+", 2).Result()
	if err != nil || len(currentEntries) != 1 {
		t.Fatalf("read stream before stream-only loss: entries=%v err=%v", currentEntries, err)
	}
	if deleted, err := primaryRedis.HDel(ctx, indexKey, outboxID).Result(); err != nil || deleted != 1 {
		t.Fatalf("inject stream-only Redis state: deleted=%d err=%v", deleted, err)
	}
	ageVisibilityRepairRow(t, ctx, pool, outboxID)
	if err := publisher.RunOnce(ctx); !errors.Is(err, redisdispatch.ErrControlDeliveryIndexInconsistent) {
		t.Fatalf("stream-only Redis state did not fail closed: %v", err)
	}
	unchangedEntries, err := primaryRedis.XRangeN(ctx, stream, "-", "+", 2).Result()
	if err != nil || len(unchangedEntries) != 1 || unchangedEntries[0].ID != currentEntries[0].ID {
		t.Fatalf("stream-only rejection mutated entries=%v err=%v", unchangedEntries, err)
	}
	unchangedEnvelope, ok := unchangedEntries[0].Values["signed_envelope"].(string)
	if !ok || !bytes.Equal([]byte(unchangedEnvelope), initialEnvelope) {
		t.Fatalf("stream-only rejection mutated envelope type=%T bytes=%d", unchangedEntries[0].Values["signed_envelope"], len(unchangedEnvelope))
	}
	if mappings, err := primaryRedis.HLen(ctx, indexKey).Result(); err != nil || mappings != 0 {
		t.Fatalf("stream-only rejection mutated mappings=%d err=%v", mappings, err)
	}
	_, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if attempts != 3 {
		t.Fatalf("stream-only rejection incremented publication attempts=%d, want 3", attempts)
	}

	if err := primaryRedis.Del(ctx, stream, indexKey).Err(); err != nil {
		t.Fatalf("clear both Redis keys after partial-loss proof: %v", err)
	}
	if err := publisher.RunOnce(ctx); err != nil {
		t.Fatalf("repair second coordinated Redis key loss: %v", err)
	}
	_, restoredEnvelope = assertVisibilityRepairRedisState(t, ctx, primaryRedis, stream, indexKey, outboxID)
	_, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(restoredEnvelope, initialEnvelope) || attempts != 4 {
		t.Fatalf("second coordinated-key repair changed bytes=%t attempts=%d, want true/4", !bytes.Equal(restoredEnvelope, initialEnvelope), attempts)
	}

	// Independent process-local clients/dispatchers may race on the same stale
	// durable row. Redis must admit exactly one stable-ID mapping and one entry;
	// both callers must observe success for the same exact prepared bytes.
	if err := primaryRedis.Del(ctx, stream, indexKey).Err(); err != nil {
		t.Fatalf("reset Redis before scale-out race: %v", err)
	}
	secondaryRedis := newVisibilityRepairRedisClient(t, redisAddress)
	t.Cleanup(func() { closeVisibilityRepairRedisClient(t, secondaryRedis) })
	secondDispatcher := newVisibilityRepairDispatcher(t, pool, secondaryRedis, policy, signer)
	start := make(chan struct{})
	results := make(chan error, 2)
	var racers sync.WaitGroup
	racers.Add(2)
	for _, dispatcher := range []*executionapp.ValidationDispatcher{firstDispatcher, secondDispatcher} {
		dispatcher := dispatcher
		go func() {
			defer racers.Done()
			<-start
			results <- dispatcher.Dispatch(ctx, outboxID)
		}()
	}
	close(start)
	racers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("scale-out visibility publisher failed: %v", err)
		}
	}
	_, racedEnvelope := assertVisibilityRepairRedisState(t, ctx, primaryRedis, stream, indexKey, outboxID)
	_, attempts = visibilityRepairPostgresState(t, ctx, pool, outboxID)
	if !bytes.Equal(racedEnvelope, initialEnvelope) || attempts != 6 {
		t.Fatalf("scale-out dedupe changed bytes=%t attempts=%d, want false/6", !bytes.Equal(racedEnvelope, initialEnvelope), attempts)
	}
}

func newVisibilityRepairRedisClient(t *testing.T, address string) *redis.Client {
	t.Helper()
	return redis.NewClient(&redis.Options{
		Addr:         address,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     4,
		MaxRetries:   -1,
	})
}

func closeVisibilityRepairRedisClient(t *testing.T, client *redis.Client) {
	t.Helper()
	if err := client.Close(); err != nil {
		t.Errorf("close visibility-repair Redis client: %v", err)
	}
}

func newVisibilityRepairDispatcher(
	t *testing.T,
	pool *pgxpool.Pool,
	client *redis.Client,
	policy ValidationDispatchPolicy,
	signer redisdispatch.CommandSigner,
) *executionapp.ValidationDispatcher {
	t.Helper()
	appender, err := redisdispatch.NewRedisStreamAppender(client, redisdispatch.RedisStreamAppenderConfig{
		MaxEntries:    policy.MaxOutstanding,
		MaxEntryBytes: 64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer, err := redisdispatch.NewProducer(redisdispatch.ProducerConfig{
		Stream:                 policy.StreamName,
		ProtocolRevision:       "elitea.runtime.v1",
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		Limits: redisdispatch.Limits{
			Revision:                 policy.LimitsRevision,
			MaxWorkerCommandBytes:    32 * 1024,
			MaxSignedEnvelopeBytes:   48 * 1024,
			MaxTransportPayloadBytes: 48 * 1024,
			MaxTransportMessageBytes: 64 * 1024,
			MaxSignatureBytes:        256,
			MaxStringBytes:           256,
		},
	}, signer, appender)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := executionapp.NewValidationDispatcher(mustVisibilityRepairOutbox(t, pool, policy.StreamName), producer)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

func mustVisibilityRepairOutbox(t *testing.T, pool *pgxpool.Pool, stream string) *CommandOutboxRepository {
	t.Helper()
	outbox, err := NewCommandOutboxRepository(pool, stream)
	if err != nil {
		t.Fatal(err)
	}
	return outbox
}

func ageVisibilityRepairRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, outboxID string) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
WITH aged_visibility AS MATERIALIZED (
    SELECT clock_timestamp() - interval '2 seconds' AS observed_at
)
UPDATE elitea_runtime.command_outbox
SET published_at = aged_visibility.observed_at,
    last_visibility_at = aged_visibility.observed_at
FROM aged_visibility
WHERE outbox_id = $1 AND published_at IS NOT NULL`, outboxID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age PostgreSQL visibility observation: affected=%d err=%v", tag.RowsAffected(), err)
	}
}

func visibilityRepairPostgresState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, outboxID string) ([]byte, int64) {
	t.Helper()
	var envelope []byte
	var publishedAt, lastVisibilityAt time.Time
	var attempts int64
	if err := pool.QueryRow(ctx, `
SELECT prepared_signed_envelope_bytes, published_at, last_visibility_at, publish_attempts
FROM elitea_runtime.command_outbox
WHERE outbox_id = $1`, outboxID).Scan(&envelope, &publishedAt, &lastVisibilityAt, &attempts); err != nil {
		t.Fatalf("read PostgreSQL visibility state: %v", err)
	}
	if len(envelope) == 0 || publishedAt.IsZero() || lastVisibilityAt.IsZero() || lastVisibilityAt.Before(publishedAt) {
		t.Fatalf("invalid PostgreSQL visibility state: bytes=%d published=%s last=%s", len(envelope), publishedAt, lastVisibilityAt)
	}
	return append([]byte(nil), envelope...), attempts
}

func assertVisibilityRepairRedisState(t *testing.T, ctx context.Context, client *redis.Client, stream, indexKey, outboxID string) (string, []byte) {
	t.Helper()
	entries, err := client.XRangeN(ctx, stream, "-", "+", 2).Result()
	if err != nil || len(entries) != 1 {
		t.Fatalf("Redis visibility entries=%d err=%v, want 1", len(entries), err)
	}
	if len(entries[0].Values) != 1 {
		t.Fatalf("Redis visibility entry fields=%d, want 1", len(entries[0].Values))
	}
	raw, ok := entries[0].Values["signed_envelope"].(string)
	if !ok || raw == "" {
		t.Fatalf("Redis visibility envelope type=%T bytes=%d", entries[0].Values["signed_envelope"], len(raw))
	}
	mappedEntryID, err := client.HGet(ctx, indexKey, outboxID).Result()
	if err != nil || mappedEntryID != entries[0].ID {
		t.Fatalf("Redis visibility mapping=%q entry=%q err=%v", mappedEntryID, entries[0].ID, err)
	}
	if mappings, err := client.HLen(ctx, indexKey).Result(); err != nil || mappings != 1 {
		t.Fatalf("Redis visibility mapping count=%d err=%v, want 1", mappings, err)
	}
	return entries[0].ID, []byte(raw)
}
