package repos

// Web push subscriptions and the outbox against a real PostgreSQL (ADR-0045
// decision 6, #1437). Requires ELITEA_TEST_DATABASE_URL; every test skips
// without it.

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/webpush"
)

// pushSubscriptionFixture is a subscription with real browser keys.
func pushSubscriptionFixture(t *testing.T, user int64, endpoint string) PushSubscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return PushSubscription{
		UserID: user, Endpoint: endpoint, UserAgent: "test",
		P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(secret),
	}
}

func pushSubscriptionCount(t *testing.T, pool *pgxpool.Pool, user int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM centry.push_subscriptions WHERE user_id = $1`, user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// seedPushNotification writes a notification row as a writer does.
func seedPushNotification(t *testing.T, pool *pgxpool.Pool, user int64, eventType, meta string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::uuid, FALSE, 1, $2, $3::jsonb, $4, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
RETURNING id`, uuid.NewString(), user, meta, eventType).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPostgresWebPushSubscriptions: the POST is idempotent on the endpoint,
// an endpoint moves to the last user, the 11th subscription replaces the one
// with the oldest last success, and a delete of an absent row succeeds.
func TestPostgresWebPushSubscriptions(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	repo, err := NewWebPushRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	const user, other int64 = 4801, 4802
	first := pushSubscriptionFixture(t, user, "https://push.example.com/a")
	id, created, err := repo.SaveSubscription(ctx, first)
	if err != nil || !created {
		t.Fatalf("first save = %q %v %v", id, created, err)
	}
	renewed := pushSubscriptionFixture(t, user, first.Endpoint)
	again, created, err := repo.SaveSubscription(ctx, renewed)
	if err != nil || created || again != id {
		t.Fatalf("second save = %q %v %v, want %q not created", again, created, err, id)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT p256dh FROM centry.push_subscriptions WHERE id = $1::uuid`, id).
		Scan(&stored); err != nil || stored != renewed.P256dh {
		t.Fatalf("keys not updated: %v", err)
	}

	// The endpoint moves to the user who opted in last.
	moved, created, err := repo.SaveSubscription(ctx, pushSubscriptionFixture(t, other, first.Endpoint))
	if err != nil || !created || moved != id {
		t.Fatalf("moved save = %q %v %v", moved, created, err)
	}
	if pushSubscriptionCount(t, pool, user) != 0 || pushSubscriptionCount(t, pool, other) != 1 {
		t.Fatalf("the endpoint did not move")
	}

	// The cap: ten rows; the 11th evicts the one with the oldest success.
	ids := make([]string, 0, webpush.MaxSubscriptionsPerUser)
	for index := range webpush.MaxSubscriptionsPerUser {
		saved, _, err := repo.SaveSubscription(ctx,
			pushSubscriptionFixture(t, user, "https://push.example.com/cap/"+strconv.Itoa(index)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, saved)
	}
	// Every row but ids[3] succeeded recently.
	if _, err := pool.Exec(ctx, `
UPDATE centry.push_subscriptions SET last_success_at = now()
WHERE user_id = $1 AND id <> $2::uuid`, user, ids[3]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE centry.push_subscriptions SET created_at = now() - interval '1 day' WHERE id = $1::uuid`, ids[3]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.SaveSubscription(ctx, pushSubscriptionFixture(t, user, "https://push.example.com/cap/new")); err != nil {
		t.Fatal(err)
	}
	if count := pushSubscriptionCount(t, pool, user); count != webpush.MaxSubscriptionsPerUser {
		t.Fatalf("subscriptions = %d, want %d", count, webpush.MaxSubscriptionsPerUser)
	}
	var evicted bool
	if err := pool.QueryRow(ctx, `
SELECT NOT EXISTS (SELECT 1 FROM centry.push_subscriptions WHERE id = $1::uuid)`, ids[3]).Scan(&evicted); err != nil ||
		!evicted {
		t.Fatalf("the oldest-success row stayed: %v", err)
	}

	// Concurrent opt-ins of one user still hold the cap.
	var group sync.WaitGroup
	for index := range 6 {
		group.Go(func() {
			if _, _, err := repo.SaveSubscription(ctx,
				pushSubscriptionFixture(t, user, "https://push.example.com/race/"+strconv.Itoa(index))); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if count := pushSubscriptionCount(t, pool, user); count != webpush.MaxSubscriptionsPerUser {
		t.Fatalf("subscriptions after the race = %d", count)
	}

	// Delete: only the caller's row; an absent row is not an error.
	if err := repo.DeleteSubscription(ctx, user, first.Endpoint); err != nil {
		t.Fatal(err)
	}
	if pushSubscriptionCount(t, pool, other) != 1 {
		t.Fatalf("a delete removed another user's row")
	}
	if err := repo.DeleteSubscription(ctx, other, first.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteSubscription(ctx, other, first.Endpoint); err != nil {
		t.Fatalf("delete of an absent row = %v", err)
	}
	if pushSubscriptionCount(t, pool, other) != 0 {
		t.Fatalf("the row stayed")
	}
}

// TestPostgresWebPushOutboxPass: the pass reads past the cursor, enqueues
// one delivery per subscription, records a retry, and a Gone answer removes
// the subscription with its deliveries.
func TestPostgresWebPushOutboxPass(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	repo, err := NewWebPushRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	const user int64 = 4811
	for _, endpoint := range []string{"https://push.example.com/one", "https://push.example.com/two"} {
		if _, _, err := repo.SaveSubscription(ctx, pushSubscriptionFixture(t, user, endpoint)); err != nil {
			t.Fatal(err)
		}
	}
	other := seedPushNotification(t, pool, user, "chat_mention", `{}`)
	finished := seedPushNotification(t, pool, user, webpush.EventRunFinished,
		`{"outcome":"failed","project_id":1,"chain_root_execution_id":"r","execution_id":"r","origin":"cloud","continuations":0}`)

	var due []webpush.Delivery
	ran, err := repo.WithSenderLock(ctx, func(pass webpush.Pass) error {
		rows, last, err := pass.NewNotifications(ctx, 10)
		if err != nil {
			return err
		}
		if last != finished || len(rows) != 1 || rows[0].ID != finished || rows[0].UserID != user {
			t.Fatalf("new = %+v last %d, want only %d (skipped %d)", rows, last, finished, other)
		}
		payload, urgency, ok := webpush.BuildPayload(rows[0])
		if !ok {
			t.Fatal("no payload")
		}
		for range 2 { // a second enqueue adds nothing
			if err := pass.Enqueue(ctx, rows[0], payload, urgency, time.Now().Add(webpush.TTL)); err != nil {
				return err
			}
		}
		if err := pass.AdvanceCursor(ctx, last); err != nil {
			return err
		}
		due, err = pass.DueDeliveries(ctx, 10)
		return err
	})
	if err != nil || !ran {
		t.Fatalf("pass = %v, %v", ran, err)
	}
	if len(due) != 2 || due[0].Urgency != "normal" {
		t.Fatalf("due = %+v", due)
	}
	if _, err := repo.WithSenderLock(ctx, func(pass webpush.Pass) error {
		rows, _, err := pass.NewNotifications(ctx, 10)
		if err == nil && len(rows) != 0 {
			t.Fatalf("the cursor did not move: %+v", rows)
		}
		if err != nil {
			return err
		}
		return pass.Record(ctx, []webpush.Outcome{
			{Delivery: due[0], Action: webpush.Retry, Status: 503, RetryAt: time.Now().Add(time.Hour)},
			{Delivery: due[1], Action: webpush.Gone, Status: 410},
		})
	}); err != nil {
		t.Fatal(err)
	}
	var attempts, status int
	if err := pool.QueryRow(ctx, `
SELECT attempts, last_status FROM elitea_runtime.web_push_deliveries WHERE subscription_id = $1::uuid`,
		due[0].SubscriptionID).Scan(&attempts, &status); err != nil || attempts != 1 || status != 503 {
		t.Fatalf("retry row = %d %d %v", attempts, status, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM centry.push_subscriptions WHERE id = $1::uuid)
     + (SELECT count(*) FROM elitea_runtime.web_push_deliveries WHERE subscription_id = $1::uuid)`,
		due[1].SubscriptionID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("gone subscription left %d rows: %v", left, err)
	}
	// A retry that is not due is not handed out again.
	if _, err := repo.WithSenderLock(ctx, func(pass webpush.Pass) error {
		again, err := pass.DueDeliveries(ctx, 10)
		if err == nil && len(again) != 0 {
			t.Fatalf("due again = %+v", again)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresWebPushSingleSender: while one pass holds the lock, another
// replica's pass does not run.
func TestPostgresWebPushSingleSender(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	repo, err := NewWebPushRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := repo.WithSenderLock(ctx, func(webpush.Pass) error {
			close(held)
			<-release
			return nil
		})
		done <- err
	}()
	<-held
	ran, err := repo.WithSenderLock(ctx, func(webpush.Pass) error { return nil })
	if err != nil || ran {
		t.Fatalf("second sender ran = %v, %v", ran, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ran, err := repo.WithSenderLock(ctx, func(webpush.Pass) error { return nil }); err != nil || !ran {
		t.Fatalf("pass after release = %v, %v", ran, err)
	}
}

type pushTestVault struct {
	mu    sync.Mutex
	value string
}

func (v *pushTestVault) EnsureAdminHiddenSecret(_ context.Context, _ string, generate func() (string, error)) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.value == "" {
		value, err := generate()
		if err != nil {
			return "", err
		}
		v.value = value
	}
	return v.value, nil
}

// TestPostgresWebPushDispatcherEndToEnd: a run_finished row reaches a push
// service through the dispatcher; a 410 removes the subscription.
func TestPostgresWebPushDispatcherEndToEnd(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	repo, err := NewWebPushRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	const user int64 = 4821
	for _, path := range []string{"/ok", "/gone"} {
		if _, _, err := repo.SaveSubscription(ctx, pushSubscriptionFixture(t, user, server.URL+path)); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.DiscardHandler)
	service := webpush.NewService(webpush.NewKeySource(&pushTestVault{}, logger),
		webpush.SettingsFunc(func(context.Context) bool { return true }), "mailto:ops@example.com")
	dispatcher, err := webpush.NewDispatcher(repo, service, server.Client(), logger)
	if err != nil {
		t.Fatal(err)
	}
	seedPushNotification(t, pool, user, webpush.EventRunFinished,
		`{"outcome":"completed","project_id":1,"chain_root_execution_id":"r","execution_id":"r","origin":"cloud","continuations":2}`)
	if err := dispatcher.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("sends = %d, want 2", hits.Load())
	}
	var subscriptions, deliveries int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM centry.push_subscriptions WHERE user_id = $1),
       (SELECT count(*) FROM elitea_runtime.web_push_deliveries)`, user).Scan(&subscriptions, &deliveries); err != nil {
		t.Fatal(err)
	}
	if subscriptions != 1 || deliveries != 0 {
		t.Fatalf("subscriptions %d deliveries %d, want 1 and 0", subscriptions, deliveries)
	}
	var success bool
	if err := pool.QueryRow(ctx, `
SELECT last_success_at IS NOT NULL FROM centry.push_subscriptions WHERE user_id = $1`, user).Scan(&success); err != nil ||
		!success {
		t.Fatalf("last_success_at not set: %v", err)
	}
}
