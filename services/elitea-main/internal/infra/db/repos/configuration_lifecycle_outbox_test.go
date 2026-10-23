package repos

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

func TestCurrentConfigurationLifecycleOutboxClaimPreservesSnapshotAndBindings(t *testing.T) {
	snapshot := []byte(`{ "type": "github", "revision": 1 }`)
	digest := bytes.Repeat([]byte{0x5a}, 32)
	queries := &currentConfigurationLifecycleQueriesStub{
		claimRows: []sqlcgen.ClaimConfigurationLifecycleEventsRow{{
			EventID:           "11111111-1111-4111-8111-111111111111",
			ResourceProjectID: 7,
			ConfigurationUuid: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			Revision:          3,
			Operation:         string(configurationapp.CurrentConfigurationUpdated),
			ActorID:           42,
			SanitizedSnapshot: snapshot,
			SnapshotDigest:    digest,
			AttemptCount:      2,
			LeaseToken:        "lease.batch-1",
		}},
	}
	repository := mustCurrentConfigurationLifecycleOutboxRepository(t, queries)

	events, err := repository.ClaimCurrentConfigurationLifecycle(
		context.Background(), "lease.batch-1", 8, 30*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if queries.claimCalls != 1 || queries.claimParams != (sqlcgen.ClaimConfigurationLifecycleEventsParams{
		ClaimLimit: 8, LeaseToken: "lease.batch-1", LeaseTtlMillis: 30_000,
	}) {
		t.Fatalf("claim calls=%d params=%+v", queries.claimCalls, queries.claimParams)
	}
	if len(events) != 1 {
		t.Fatalf("events=%#v", events)
	}
	event := events[0]
	if event.EventID != queries.claimRows[0].EventID || event.ProjectID != 7 ||
		event.ConfigurationUUID != queries.claimRows[0].ConfigurationUuid || event.Revision != 3 ||
		event.Operation != configurationapp.CurrentConfigurationUpdated || event.ActorID != 42 ||
		event.AttemptCount != 2 || event.LeaseToken != "lease.batch-1" ||
		!bytes.Equal(event.Snapshot, snapshot) || !bytes.Equal(event.SnapshotDigest[:], digest) {
		t.Fatalf("event=%#v", event)
	}

	queries.claimRows[0].SanitizedSnapshot[0] = '!'
	queries.claimRows[0].SnapshotDigest[0] = 0
	if event.Snapshot[0] != '{' || event.SnapshotDigest[0] != 0x5a {
		t.Fatal("claimed bytes alias the SQL row buffers")
	}
}

func TestCurrentConfigurationLifecycleOutboxClaimRejectsInvalidRequestsAndRows(t *testing.T) {
	validRow := currentConfigurationLifecycleClaimRowForTest("11111111-1111-4111-8111-111111111111")
	queries := &currentConfigurationLifecycleQueriesStub{claimRows: []sqlcgen.ClaimConfigurationLifecycleEventsRow{validRow}}
	repository := mustCurrentConfigurationLifecycleOutboxRepository(t, queries)

	invalidRequests := []struct {
		name  string
		token string
		limit int
		ttl   time.Duration
	}{
		{name: "empty token", limit: 1, ttl: time.Second},
		{name: "unsafe token", token: "lease secret", limit: 1, ttl: time.Second},
		{name: "zero limit", token: "lease-1", ttl: time.Second},
		{name: "oversized limit", token: "lease-1", limit: configurationapp.MaxCurrentConfigurationLifecycleBatchSize + 1, ttl: time.Second},
		{name: "sub-millisecond ttl", token: "lease-1", limit: 1, ttl: time.Millisecond + time.Nanosecond},
		{name: "oversized ttl", token: "lease-1", limit: 1, ttl: configurationapp.MaxCurrentConfigurationLifecycleLeaseTTL + time.Millisecond},
	}
	for _, test := range invalidRequests {
		t.Run(test.name, func(t *testing.T) {
			before := queries.claimCalls
			_, err := repository.ClaimCurrentConfigurationLifecycle(
				context.Background(), test.token, test.limit, test.ttl,
			)
			if !errors.Is(err, ErrInvalidCurrentConfigurationLifecycleOutbox) || queries.claimCalls != before {
				t.Fatalf("error=%v calls=%d", err, queries.claimCalls)
			}
		})
	}

	queries.claimRows = []sqlcgen.ClaimConfigurationLifecycleEventsRow{validRow, validRow}
	queries.claimRows[1].EventID = "22222222-2222-4222-8222-222222222222"
	if _, err := repository.ClaimCurrentConfigurationLifecycle(
		context.Background(), validRow.LeaseToken, 2, time.Second,
	); !errors.Is(err, ErrCurrentConfigurationLifecycleUnavailable) {
		t.Fatalf("duplicate configuration error=%v", err)
	}

	queries.claimRows = []sqlcgen.ClaimConfigurationLifecycleEventsRow{validRow}
	queries.claimRows[0].SnapshotDigest = []byte("short")
	if _, err := repository.ClaimCurrentConfigurationLifecycle(
		context.Background(), validRow.LeaseToken, 1, time.Second,
	); !errors.Is(err, ErrCurrentConfigurationLifecycleUnavailable) {
		t.Fatalf("invalid database row error=%v", err)
	}
}

func TestCurrentConfigurationLifecycleOutboxTransitionsAreBoundedAndFenced(t *testing.T) {
	queries := &currentConfigurationLifecycleQueriesStub{
		deliveredRows: 1,
		retryRows:     1,
		deadRows:      1,
	}
	repository := mustCurrentConfigurationLifecycleOutboxRepository(t, queries)
	eventID := "11111111-1111-4111-8111-111111111111"
	leaseToken := "lease.batch-1"

	if err := repository.MarkCurrentConfigurationLifecycleDelivered(
		context.Background(), eventID, leaseToken,
	); err != nil {
		t.Fatal(err)
	}
	if queries.deliveredParams != (sqlcgen.MarkConfigurationLifecycleDeliveredParams{
		EventID: eventID, LeaseToken: leaseToken,
	}) {
		t.Fatalf("delivered params=%+v", queries.deliveredParams)
	}

	if err := repository.MarkCurrentConfigurationLifecycleRetry(
		context.Background(), eventID, leaseToken, "DEPENDENCY_UNAVAILABLE", 2500*time.Millisecond,
	); err != nil {
		t.Fatal(err)
	}
	if queries.retryParams != (sqlcgen.MarkConfigurationLifecycleRetryParams{
		RetryDelayMillis: 2500,
		ErrorCode:        "DEPENDENCY_UNAVAILABLE",
		EventID:          eventID,
		LeaseToken:       leaseToken,
	}) {
		t.Fatalf("retry params=%+v", queries.retryParams)
	}

	if err := repository.MarkCurrentConfigurationLifecycleDead(
		context.Background(), eventID, leaseToken, "SNAPSHOT_INVALID",
	); err != nil {
		t.Fatal(err)
	}
	if queries.deadParams != (sqlcgen.MarkConfigurationLifecycleDeadParams{
		ErrorCode: "SNAPSHOT_INVALID", EventID: eventID, LeaseToken: leaseToken,
	}) {
		t.Fatalf("dead params=%+v", queries.deadParams)
	}

	queries.deliveredRows = 0
	if err := repository.MarkCurrentConfigurationLifecycleDelivered(
		context.Background(), eventID, leaseToken,
	); !errors.Is(err, ErrCurrentConfigurationLifecycleLeaseLost) {
		t.Fatalf("zero-row transition error=%v", err)
	}
	queries.deliveredRows = 2
	if err := repository.MarkCurrentConfigurationLifecycleDelivered(
		context.Background(), eventID, leaseToken,
	); !errors.Is(err, ErrCurrentConfigurationLifecycleUnavailable) {
		t.Fatalf("multi-row transition error=%v", err)
	}

	before := queries.retryCalls
	if err := repository.MarkCurrentConfigurationLifecycleRetry(
		context.Background(), eventID, leaseToken, "lowercase", time.Second,
	); !errors.Is(err, ErrInvalidCurrentConfigurationLifecycleOutbox) || queries.retryCalls != before {
		t.Fatalf("invalid error code error=%v calls=%d", err, queries.retryCalls)
	}
	if err := repository.MarkCurrentConfigurationLifecycleRetry(
		context.Background(), eventID, leaseToken, "RETRY", configurationapp.MaxCurrentConfigurationLifecycleRetryDelay+time.Millisecond,
	); !errors.Is(err, ErrInvalidCurrentConfigurationLifecycleOutbox) || queries.retryCalls != before {
		t.Fatalf("invalid retry delay error=%v calls=%d", err, queries.retryCalls)
	}
}

func TestCurrentConfigurationLifecycleOutboxHidesDatabaseErrorsAndPreservesCancellation(t *testing.T) {
	sensitive := errors.New("database detail password=TEST_ONLY_DO_NOT_EMIT")
	queries := &currentConfigurationLifecycleQueriesStub{claimErr: sensitive, deliveredErr: sensitive}
	repository := mustCurrentConfigurationLifecycleOutboxRepository(t, queries)

	_, err := repository.ClaimCurrentConfigurationLifecycle(
		context.Background(), "lease-1", 1, time.Second,
	)
	if !errors.Is(err, ErrCurrentConfigurationLifecycleUnavailable) || strings.Contains(err.Error(), "password") {
		t.Fatalf("claim error=%v", err)
	}
	eventID := "11111111-1111-4111-8111-111111111111"
	err = repository.MarkCurrentConfigurationLifecycleDelivered(
		context.Background(), eventID, "lease-1",
	)
	if !errors.Is(err, ErrCurrentConfigurationLifecycleUnavailable) || strings.Contains(err.Error(), "password") {
		t.Fatalf("transition error=%v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	before := queries.claimCalls
	_, err = repository.ClaimCurrentConfigurationLifecycle(canceled, "lease-1", 1, time.Second)
	if !errors.Is(err, context.Canceled) || queries.claimCalls != before {
		t.Fatalf("canceled error=%v calls=%d", err, queries.claimCalls)
	}
}

func TestCurrentConfigurationLifecycleSQLContainsOrderingAndLeaseFences(t *testing.T) {
	query, err := os.ReadFile("../../../db/queries/configuration_mutations.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := string(query)
	fragments := []string{
		"FOR UPDATE OF candidate SKIP LOCKED",
		"FOR UPDATE OF exhausted SKIP LOCKED",
		"exhausted.attempt_count = 1000",
		"last_error_code = 'ATTEMPTS_EXHAUSTED'",
		"ORDER BY candidate.available_at, candidate.created_at, candidate.event_id",
		"blocker.revision < candidate.revision",
		"blocker.state IN ('PENDING', 'RETRY', 'PROCESSING', 'DEAD')",
		"candidate.state = 'PROCESSING'",
		"candidate.lease_expires_at <= authority_clock.observed_at",
		"attempt_count = outbox.attempt_count + 1",
		"outbox.state = 'PROCESSING'",
		"outbox.lease_owner = sqlc.arg('lease_token')::text",
		"outbox.lease_expires_at > authority_clock.observed_at",
		"sqlc.arg('retry_delay_millis')::bigint * interval '1 millisecond'",
	}
	for _, fragment := range fragments {
		if !strings.Contains(source, fragment) {
			t.Fatalf("lifecycle SQL is missing %q", fragment)
		}
	}
}

func currentConfigurationLifecycleClaimRowForTest(
	eventID string,
) sqlcgen.ClaimConfigurationLifecycleEventsRow {
	return sqlcgen.ClaimConfigurationLifecycleEventsRow{
		EventID:           eventID,
		ResourceProjectID: 7,
		ConfigurationUuid: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Revision:          1,
		Operation:         string(configurationapp.CurrentConfigurationCreated),
		ActorID:           42,
		SanitizedSnapshot: []byte(`{}`),
		SnapshotDigest:    bytes.Repeat([]byte{1}, 32),
		AttemptCount:      1,
		LeaseToken:        "lease-1",
	}
}

func mustCurrentConfigurationLifecycleOutboxRepository(
	t *testing.T,
	queries currentConfigurationLifecycleQueries,
) *CurrentConfigurationLifecycleOutboxRepository {
	t.Helper()
	repository, err := newCurrentConfigurationLifecycleOutboxRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

type currentConfigurationLifecycleQueriesStub struct {
	claimRows   []sqlcgen.ClaimConfigurationLifecycleEventsRow
	claimErr    error
	claimCalls  int
	claimParams sqlcgen.ClaimConfigurationLifecycleEventsParams

	deliveredRows   int64
	deliveredErr    error
	deliveredCalls  int
	deliveredParams sqlcgen.MarkConfigurationLifecycleDeliveredParams

	retryRows   int64
	retryErr    error
	retryCalls  int
	retryParams sqlcgen.MarkConfigurationLifecycleRetryParams

	deadRows   int64
	deadErr    error
	deadCalls  int
	deadParams sqlcgen.MarkConfigurationLifecycleDeadParams
}

func (s *currentConfigurationLifecycleQueriesStub) ClaimConfigurationLifecycleEvents(
	_ context.Context,
	params sqlcgen.ClaimConfigurationLifecycleEventsParams,
) ([]sqlcgen.ClaimConfigurationLifecycleEventsRow, error) {
	s.claimCalls++
	s.claimParams = params
	return s.claimRows, s.claimErr
}

func (s *currentConfigurationLifecycleQueriesStub) MarkConfigurationLifecycleDelivered(
	_ context.Context,
	params sqlcgen.MarkConfigurationLifecycleDeliveredParams,
) (int64, error) {
	s.deliveredCalls++
	s.deliveredParams = params
	return s.deliveredRows, s.deliveredErr
}

func (s *currentConfigurationLifecycleQueriesStub) MarkConfigurationLifecycleRetry(
	_ context.Context,
	params sqlcgen.MarkConfigurationLifecycleRetryParams,
) (int64, error) {
	s.retryCalls++
	s.retryParams = params
	return s.retryRows, s.retryErr
}

func (s *currentConfigurationLifecycleQueriesStub) MarkConfigurationLifecycleDead(
	_ context.Context,
	params sqlcgen.MarkConfigurationLifecycleDeadParams,
) (int64, error) {
	s.deadCalls++
	s.deadParams = params
	return s.deadRows, s.deadErr
}
