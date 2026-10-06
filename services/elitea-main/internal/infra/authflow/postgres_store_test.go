package authflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authstatetest"
)

var _ browserauth.TransactionStore = (*PostgresStore)(nil)

func TestNewPostgresStoreRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	pool := authstatetest.ClosedPool(t)
	if _, err := NewPostgresStore(nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil pool error = %v, want %v", err, ErrInvalidConfiguration)
	}
	if _, err := newPostgresStore(pool, nil, time.Now); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil generator error = %v, want %v", err, ErrInvalidConfiguration)
	}
	if _, err := newPostgresStore(pool, browserflow.NewTransactionID, nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil clock error = %v, want %v", err, ErrInvalidConfiguration)
	}
}

func TestPostgresStoreCreateConsumeAndReplay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	id := testTransactionID(1)
	store, pool, _ := newTestStore(t, now, fixedGenerator(id))
	transaction := testTransaction(now, "oidc", "session-1")

	createdID, err := store.Create(context.Background(), transaction)
	if err != nil {
		t.Fatal(err)
	}
	if createdID != id || browserflow.ValidateTransactionID(createdID) != nil {
		t.Fatalf("created ID = %q", createdID)
	}
	// The originating session ID is the session cookie's bearer value: the
	// row holds only its SHA-256, in the binding column and in the record,
	// and the database computes the same hash.
	var provider, session, record, whole string
	var matchesDatabaseHash bool
	if err := pool.QueryRow(context.Background(), `
		SELECT provider, originating_session_hash, convert_from(record, 'UTF8'), t::text,
		       originating_session_hash = encode(sha256(convert_to($2, 'UTF8')), 'hex')
		FROM elitea_auth.form_login_transactions AS t WHERE id = $1`, id, "session-1",
	).Scan(&provider, &session, &record, &whole, &matchesDatabaseHash); err != nil ||
		provider != "oidc" || session != sessionHash("session-1") || !matchesDatabaseHash {
		t.Fatalf("stored binding = %q %q (database hash match %t), %v", provider, session, matchesDatabaseHash, err)
	}
	for _, text := range []string{record, whole} {
		if strings.Contains(text, "session-1") {
			t.Fatalf("a stored row contains the raw originating session ID: %s", text)
		}
	}
	if ttl := remainingLifetime(t, pool, id); ttl <= 4*time.Minute || ttl > 5*time.Minute {
		t.Fatalf("lifetime = %s, want the derived five minutes", ttl)
	}

	consumed, err := store.Consume(context.Background(), id, "oidc", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if consumed != transaction {
		t.Fatalf("consumed transaction = %+v, want %+v", consumed, transaction)
	}
	if exists(t, pool, id) {
		t.Fatal("consumed transaction remains in the table")
	}
	if _, err := store.Consume(context.Background(), id, "oidc", "session-1"); !errors.Is(err, browserflow.ErrTransactionRejected) {
		t.Fatalf("replay error = %v, want %v", err, browserflow.ErrTransactionRejected)
	}
}

func TestPostgresStoreBindingMismatchDoesNotConsume(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	id := testTransactionID(2)
	store, pool, _ := newTestStore(t, now, fixedGenerator(id))
	if _, err := store.Create(context.Background(), testTransaction(now, "oidc", "session-1")); err != nil {
		t.Fatal(err)
	}

	for _, binding := range []struct {
		provider string
		session  string
	}{
		{provider: "saml", session: "session-1"},
		{provider: "oidc", session: "session-2"},
	} {
		if _, err := store.Consume(context.Background(), id, binding.provider, binding.session); !errors.Is(err, browserflow.ErrTransactionRejected) {
			t.Fatalf("binding %+v error = %v, want %v", binding, err, browserflow.ErrTransactionRejected)
		}
		if !exists(t, pool, id) {
			t.Fatalf("binding mismatch %+v consumed transaction", binding)
		}
	}
	if _, err := store.Consume(context.Background(), id, "oidc", "session-1"); err != nil {
		t.Fatalf("correct binding after mismatches: %v", err)
	}
}

func TestPostgresStoreLifetimeAndApplicationExpirationAreBounded(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	t.Run("row expiry", func(t *testing.T) {
		t.Parallel()
		id := testTransactionID(3)
		store, pool, _ := newTestStore(t, now, fixedGenerator(id))
		if _, err := store.Create(context.Background(), testTransaction(now, "oidc", "session-1")); err != nil {
			t.Fatal(err)
		}
		authstatetest.Exec(t, pool,
			`UPDATE elitea_auth.form_login_transactions SET expires_at = now() - interval '1 millisecond' WHERE id = $1`, id)
		if _, err := store.Consume(context.Background(), id, "oidc", "session-1"); !errors.Is(err, browserflow.ErrTransactionRejected) {
			t.Fatalf("expired consume error = %v, want %v", err, browserflow.ErrTransactionRejected)
		}
	})

	t.Run("application clock expiry", func(t *testing.T) {
		t.Parallel()
		id := testTransactionID(4)
		store, pool, clock := newTestStore(t, now, fixedGenerator(id))
		transaction := testTransaction(now, "oidc", "session-1")
		if _, err := store.Create(context.Background(), transaction); err != nil {
			t.Fatal(err)
		}
		clock.Set(transaction.ExpiresAt)
		if _, err := store.Consume(context.Background(), id, "oidc", "session-1"); !errors.Is(err, browserflow.ErrTransactionRejected) {
			t.Fatalf("expired consume error = %v, want %v", err, browserflow.ErrTransactionRejected)
		}
		if exists(t, pool, id) {
			t.Fatal("application-expired transaction was not consumed")
		}
	})

	t.Run("already expired create", func(t *testing.T) {
		t.Parallel()
		id := testTransactionID(5)
		store, pool, clock := newTestStore(t, now, fixedGenerator(id))
		transaction := testTransaction(now, "oidc", "session-1")
		clock.Set(transaction.ExpiresAt)
		if _, err := store.Create(context.Background(), transaction); !errors.Is(err, browserflow.ErrTransactionRejected) {
			t.Fatalf("expired create error = %v, want %v", err, browserflow.ErrTransactionRejected)
		}
		if count := rowCount(t, pool); count != 0 {
			t.Fatalf("expired create wrote %d rows", count)
		}
	})
}

func TestPostgresStoreRetriesAndBoundsIDCollisions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	pool := authstatetest.Pool(t, 4)
	firstID := testTransactionID(6)
	secondID := testTransactionID(7)
	clock := &testClock{now: now}
	store := storeOn(t, pool, sequenceGenerator(firstID, firstID, secondID), clock)
	if id, err := store.Create(context.Background(), testTransaction(now, "oidc", "session-1")); err != nil || id != firstID {
		t.Fatalf("first create = %q, %v", id, err)
	}
	if id, err := store.Create(context.Background(), testTransaction(now, "saml", "session-2")); err != nil || id != secondID {
		t.Fatalf("collision retry create = %q, %v", id, err)
	}

	collidingID := testTransactionID(8)
	bounded := storeOn(t, pool, fixedGenerator(collidingID), clock)
	if _, err := bounded.Create(context.Background(), testTransaction(now, "oidc", "session-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Create(context.Background(), testTransaction(now, "saml", "session-2")); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("bounded collision error = %v, want %v", err, ErrIDCollision)
	}
	// An expired row is absent, so its ID is free again.
	authstatetest.Exec(t, pool,
		`UPDATE elitea_auth.form_login_transactions SET expires_at = now() - interval '1 millisecond' WHERE id = $1`, collidingID)
	if id, err := bounded.Create(context.Background(), testTransaction(now, "saml", "session-2")); err != nil || id != collidingID {
		t.Fatalf("create over an expired row = %q, %v", id, err)
	}
	if _, err := bounded.Consume(context.Background(), collidingID, "saml", "session-2"); err != nil {
		t.Fatalf("consume the replacement: %v", err)
	}

	before := rowCount(t, pool)
	invalid := storeOn(t, pool, fixedGenerator("transaction-1"), clock)
	if _, err := invalid.Create(context.Background(), testTransaction(now, "oidc", "session-3")); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid generated ID error = %v, want %v", err, ErrInvalidID)
	}
	if rowCount(t, pool) != before {
		t.Fatal("invalid generated ID wrote a row")
	}
}

func TestPostgresStoreCollisionRecomputesRemainingAbsoluteLifetime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: now}
	firstID := testTransactionID(60)
	secondID := testTransactionID(61)
	pool := authstatetest.Pool(t, 4)
	authstatetest.Exec(t, pool, `
		INSERT INTO elitea_auth.form_login_transactions (id, provider, originating_session_hash, record, expires_at)
		VALUES ($1, 'oidc', repeat('0', 64), 'unrelated', now() + interval '1 hour')`, firstID)

	calls := 0
	generate := func() (string, error) {
		calls++
		if calls == 1 {
			clock.Set(now.Add(2 * time.Minute))
			return firstID, nil
		}
		return secondID, nil
	}
	store := storeOn(t, pool, generate, clock)
	id, err := store.Create(context.Background(), testTransaction(now, "oidc", "session-1"))
	if err != nil {
		t.Fatal(err)
	}
	if id != secondID || calls != 2 {
		t.Fatalf("created ID = %q, generator calls = %d", id, calls)
	}
	if ttl := remainingLifetime(t, pool, secondID); ttl <= 2*time.Minute+50*time.Second || ttl > 3*time.Minute {
		t.Fatalf("lifetime after a delayed collision retry = %s, want 3m", ttl)
	}
}

func TestPostgresStoreConcurrentConsumeHasOneWinner(t *testing.T) {
	t.Parallel()

	const attempts = 32
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	id := testTransactionID(9)
	pool := authstatetest.Pool(t, 16)
	store := storeOn(t, pool, fixedGenerator(id), &testClock{now: now})
	if _, err := store.Create(context.Background(), testTransaction(now, "oidc", "session-1")); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, attempts)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.Consume(context.Background(), id, "oidc", "session-1")
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, browserflow.ErrTransactionRejected):
		default:
			t.Fatalf("unexpected consume error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful consumes = %d, want 1", successes)
	}
}

func TestPostgresStoreConsumesMalformedRecordsWithoutReturningClaims(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	validTransaction := testTransaction(now, "oidc", "session-1")
	// What Create writes: the record binds the HASH of the originating session.
	storedTransaction := validTransaction
	storedTransaction.OriginatingSessionID = sessionHash(validTransaction.OriginatingSessionID)
	validRecord, err := json.Marshal(storedTransaction)
	if err != nil {
		t.Fatal(err)
	}
	unknownRecord := append(append([]byte(nil), validRecord[:len(validRecord)-1]...), []byte(`,"unknown":true}`)...)
	duplicateRecord := append(append([]byte(nil), validRecord[:len(validRecord)-1]...), []byte(`,"provider":"oidc"}`)...)
	mismatchedTransaction := storedTransaction
	mismatchedTransaction.Provider = "saml"
	mismatchedRecord, err := json.Marshal(mismatchedTransaction)
	if err != nil {
		t.Fatal(err)
	}
	rawSessionRecord, err := json.Marshal(validTransaction)
	if err != nil {
		t.Fatal(err)
	}

	pool := authstatetest.Pool(t, 4)
	for index, test := range []struct {
		name   string
		record []byte
	}{
		{name: "malformed JSON", record: []byte("{")},
		{name: "unknown field", record: unknownRecord},
		{name: "duplicate field", record: duplicateRecord},
		{name: "non canonical whitespace", record: append([]byte(" "), validRecord...)},
		{name: "record binding differs from the binding columns", record: mismatchedRecord},
		{name: "record holds the raw originating session ID", record: rawSessionRecord},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := testTransactionID(byte(20 + index))
			store := storeOn(t, pool, fixedGenerator(id), &testClock{now: now})
			if _, err := store.Create(context.Background(), validTransaction); err != nil {
				t.Fatal(err)
			}
			authstatetest.Exec(t, pool,
				`UPDATE elitea_auth.form_login_transactions SET record = $2 WHERE id = $1`, id, test.record)
			if _, err := store.Consume(context.Background(), id, "oidc", "session-1"); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidRecord)
			}
			if exists(t, pool, id) {
				t.Fatal("malformed transaction remains in the table")
			}
		})
	}

	// What Redis had to detect at read time, the table refuses at write time:
	// a missing binding, a binding that is not a hash, an empty or oversized
	// record.
	t.Run("the table refuses incomplete rows", func(t *testing.T) {
		for name, statement := range map[string]string{
			"missing provider":            `INSERT INTO elitea_auth.form_login_transactions (id, originating_session_hash, record, expires_at) VALUES ($1, repeat('a', 64), 'r', now())`,
			"missing originating session": `INSERT INTO elitea_auth.form_login_transactions (id, provider, record, expires_at) VALUES ($1, 'oidc', 'r', now())`,
			"raw originating session":     `INSERT INTO elitea_auth.form_login_transactions (id, provider, originating_session_hash, record, expires_at) VALUES ($1, 'oidc', 'session-1', 'r', now())`,
			"missing record":              `INSERT INTO elitea_auth.form_login_transactions (id, provider, originating_session_hash, expires_at) VALUES ($1, 'oidc', repeat('a', 64), now())`,
			"empty record":                `INSERT INTO elitea_auth.form_login_transactions (id, provider, originating_session_hash, record, expires_at) VALUES ($1, 'oidc', repeat('a', 64), ''::bytea, now())`,
			"oversized record":            `INSERT INTO elitea_auth.form_login_transactions (id, provider, originating_session_hash, record, expires_at) VALUES ($1, 'oidc', repeat('a', 64), convert_to(repeat('x', 65537), 'UTF8'), now())`,
			"missing expiry":              `INSERT INTO elitea_auth.form_login_transactions (id, provider, originating_session_hash, record) VALUES ($1, 'oidc', repeat('a', 64), 'r')`,
		} {
			if _, err := pool.Exec(context.Background(), statement, testTransactionID(50)); err == nil {
				t.Fatalf("%s: the table stored the row", name)
			}
		}
	})
}

func TestPostgresStoreCancellationOutageAndInvalidInput(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	id := testTransactionID(40)
	store, pool, _ := newTestStore(t, now, fixedGenerator(id))
	transaction := testTransaction(now, "oidc", "session-1")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Create(canceled, transaction); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create error = %v, want %v", err, context.Canceled)
	}
	if _, err := store.Create(context.Background(), transaction); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consume(canceled, id, "oidc", "session-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled consume error = %v, want %v", err, context.Canceled)
	}
	if !exists(t, pool, id) {
		t.Fatal("pre-canceled consume mutated transaction")
	}
	if _, err := store.Consume(context.Background(), "transaction-1", "oidc", "session-1"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid ID error = %v, want %v", err, ErrInvalidID)
	}
	if _, err := store.Consume(context.Background(), id, "oidc provider", "session-1"); !errors.Is(err, browserflow.ErrTransactionRejected) {
		t.Fatalf("invalid binding error = %v, want %v", err, browserflow.ErrTransactionRejected)
	}
	if !exists(t, pool, id) {
		t.Fatal("invalid binding consumed transaction")
	}

	outage := storeOn(t, authstatetest.ClosedPool(t), fixedGenerator(id), &testClock{now: now})
	if _, err := outage.Consume(context.Background(), id, "oidc", "session-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("outage consume error = %v, want %v", err, ErrUnavailable)
	}
	if _, err := outage.Create(context.Background(), testTransaction(now, "saml", "session-2")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("outage create error = %v, want %v", err, ErrUnavailable)
	}
}

func newTestStore(t *testing.T, now time.Time, generate idGenerator) (*PostgresStore, *pgxpool.Pool, *testClock) {
	t.Helper()
	pool := authstatetest.Pool(t, 4)
	clock := &testClock{now: now}
	return storeOn(t, pool, generate, clock), pool, clock
}

func storeOn(t *testing.T, pool *pgxpool.Pool, generate idGenerator, clock *testClock) *PostgresStore {
	t.Helper()
	store, err := newPostgresStore(pool, generate, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func exists(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM elitea_auth.form_login_transactions WHERE id = $1)`, id).Scan(&present); err != nil {
		t.Fatal(err)
	}
	return present
}

func rowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_auth.form_login_transactions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func remainingLifetime(t *testing.T, pool *pgxpool.Pool, id string) time.Duration {
	t.Helper()
	var seconds float64
	if err := pool.QueryRow(context.Background(),
		`SELECT extract(epoch FROM expires_at - now()) FROM elitea_auth.form_login_transactions WHERE id = $1`, id,
	).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	return time.Duration(seconds * float64(time.Second))
}

func testTransaction(now time.Time, provider string, sessionID string) browserflow.Transaction {
	return browserflow.Transaction{
		SchemaVersion:        browserflow.CurrentTransactionSchemaVersion,
		Provider:             provider,
		OriginatingSessionID: sessionID,
		ReturnTarget:         "/projects/7",
		CreatedAt:            now,
		ExpiresAt:            now.Add(5 * time.Minute),
		Correlation:          browserflow.ProtocolCorrelation{Nonce: "nonce-1"},
	}
}

func testTransactionID(value byte) string {
	random := make([]byte, browserflow.TransactionIDRandomBytes)
	for index := range random {
		random[index] = value
	}
	return base64.RawURLEncoding.EncodeToString(random)
}

func fixedGenerator(id string) idGenerator {
	return func() (string, error) { return id, nil }
}

func sequenceGenerator(ids ...string) idGenerator {
	var mutex sync.Mutex
	index := 0
	return func() (string, error) {
		mutex.Lock()
		defer mutex.Unlock()
		if index >= len(ids) {
			return ids[len(ids)-1], nil
		}
		id := ids[index]
		index++
		return id, nil
	}
}

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}
