package repos

// Web push subscriptions and the push outbox (ADR-0045 decision 6, #1437;
// shared 0223). See package webpush.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/webpush"
)

// WebPushRepository stores subscriptions and runs the outbox.
type WebPushRepository struct {
	pool *pgxpool.Pool
}

// NewWebPushRepository builds the repository.
func NewWebPushRepository(pool *pgxpool.Pool) (*WebPushRepository, error) {
	if pool == nil {
		return nil, errors.New("web push repository requires a pool")
	}
	return &WebPushRepository{pool: pool}, nil
}

// PushSubscription is one browser's subscription.
type PushSubscription struct {
	UserID    int64
	Endpoint  string
	P256dh    string
	Auth      string
	UserAgent string
}

// SaveSubscription stores a subscription, idempotent on the endpoint: the
// same endpoint keeps its row and gets the new keys (created false). An
// endpoint that another user subscribed moves to this user: a browser has
// one subscription, and it belongs to whoever opted in last. A user with
// webpush.MaxSubscriptionsPerUser rows loses the one with the oldest last
// success first.
func (r *WebPushRepository) SaveSubscription(ctx context.Context, subscription PushSubscription) (string, bool, error) {
	var (
		id      string
		created bool
	)
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// One writer per user at a time, so two opt-ins cannot both pass
		// the cap.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('web_push_user:' || $1::bigint, 0))`,
			subscription.UserID); err != nil {
			return err
		}
		var owner int64
		err := tx.QueryRow(ctx, `
SELECT id::text, user_id FROM centry.push_subscriptions WHERE endpoint = $1 FOR UPDATE`,
			subscription.Endpoint).Scan(&id, &owner)
		switch {
		case err == nil:
			created = owner != subscription.UserID
			_, err = tx.Exec(ctx, `
UPDATE centry.push_subscriptions
SET user_id = $2, p256dh = $3, auth = $4, user_agent = $5, updated_at = clock_timestamp(),
    last_success_at = CASE WHEN user_id = $2 THEN last_success_at END, rejected_sends = 0
WHERE id = $1::uuid`, id, subscription.UserID, subscription.P256dh, subscription.Auth, subscription.UserAgent)
			if err != nil || !created {
				return err
			}
			// A moved row lost its old owner's queue.
			_, err = tx.Exec(ctx, `DELETE FROM elitea_runtime.web_push_deliveries WHERE subscription_id = $1::uuid`, id)
			if err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			created = true
			if err := tx.QueryRow(ctx, `
INSERT INTO centry.push_subscriptions (user_id, endpoint, p256dh, auth, user_agent)
VALUES ($1, $2, $3, $4, $5)
RETURNING id::text`, subscription.UserID, subscription.Endpoint, subscription.P256dh, subscription.Auth,
				subscription.UserAgent).Scan(&id); err != nil {
				return err
			}
		default:
			return err
		}
		// The cap: keep the newest MaxSubscriptionsPerUser, ranked by last
		// success (a row that never succeeded counts by its creation), and
		// never the row just saved.
		_, err = tx.Exec(ctx, `
DELETE FROM centry.push_subscriptions
WHERE id IN (
    SELECT id FROM centry.push_subscriptions
    WHERE user_id = $1 AND id <> $2::uuid
    ORDER BY COALESCE(last_success_at, created_at) DESC, created_at DESC
    OFFSET $3)`, subscription.UserID, id, webpush.MaxSubscriptionsPerUser-1)
		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("web push: save the subscription: %w", err)
	}
	return id, created, nil
}

// DeleteSubscription removes the user's subscription of an endpoint. An
// absent one is not an error.
func (r *WebPushRepository) DeleteSubscription(ctx context.Context, userID int64, endpoint string) error {
	if _, err := r.pool.Exec(ctx, `
DELETE FROM centry.push_subscriptions WHERE user_id = $1 AND endpoint = $2`, userID, endpoint); err != nil {
		return fmt.Errorf("web push: delete the subscription: %w", err)
	}
	return nil
}

// FanOut moves at most limit outbox rows to deliveries in one short
// transaction. SKIP LOCKED: two replicas take different rows.
func (r *WebPushRepository) FanOut(
	ctx context.Context, limit int, plan webpush.Plan, expiresAt time.Time,
) (int, error) {
	taken := 0
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		taken = 0
		rows, err := tx.Query(ctx, `
WITH taken AS (
    DELETE FROM elitea_runtime.web_push_queue
    WHERE notification_id IN (
        SELECT notification_id FROM elitea_runtime.web_push_queue
        ORDER BY notification_id
        LIMIT $1
        FOR UPDATE SKIP LOCKED)
    RETURNING notification_id)
SELECT taken.notification_id, n.id IS NOT NULL, COALESCE(n.user_id, 0)::bigint,
       COALESCE(n.project_id, 0)::bigint, COALESCE(n.event_type, ''), COALESCE(n.meta::text, '{}')
FROM taken
LEFT JOIN centry.notifications AS n ON n.id = taken.notification_id
ORDER BY taken.notification_id`, limit)
		if err != nil {
			return err
		}
		var found []webpush.Notification
		for rows.Next() {
			var (
				row    webpush.Notification
				exists bool
				meta   string
			)
			if err := rows.Scan(&row.ID, &exists, &row.UserID, &row.ProjectID, &row.EventType, &meta); err != nil {
				rows.Close()
				return err
			}
			taken++
			// A notification deleted before the fan-out sends nothing.
			if exists {
				row.Meta = []byte(meta)
				found = append(found, row)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, row := range found {
			payload, urgency, ok := plan(row)
			if !ok {
				continue
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.web_push_deliveries (notification_id, subscription_id, payload, urgency, expires_at)
SELECT $1, s.id, $3::jsonb, $4, $5
FROM centry.push_subscriptions AS s
WHERE s.user_id = $2
ON CONFLICT (notification_id, subscription_id) DO NOTHING`,
				row.ID, row.UserID, string(payload), urgency, expiresAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if undefinedColumnOrTable(err) {
			// A database before 0223 has no outbox.
			return 0, nil
		}
		return 0, fmt.Errorf("web push: fan out: %w", err)
	}
	return taken, nil
}

// ClaimDue removes the expired deliveries and claims at most limit due ones
// under a new lease id: their next_attempt_at moves past the lease, so no
// other sender takes them before it ends. Two statements, each its own
// short transaction; SKIP LOCKED keeps two replicas' claims apart.
func (r *WebPushRepository) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]webpush.Delivery, error) {
	if _, err := r.pool.Exec(ctx, `
DELETE FROM elitea_runtime.web_push_deliveries WHERE expires_at <= clock_timestamp()`); err != nil {
		if undefinedColumnOrTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("web push: drop the expired deliveries: %w", err)
	}
	leaseID := uuid.NewString()
	rows, err := r.pool.Query(ctx, `
WITH due AS (
    SELECT notification_id, subscription_id FROM elitea_runtime.web_push_deliveries
    WHERE next_attempt_at <= clock_timestamp()
    ORDER BY next_attempt_at, notification_id
    LIMIT $1
    FOR UPDATE SKIP LOCKED)
UPDATE elitea_runtime.web_push_deliveries AS d
SET lease_id = $2::uuid, next_attempt_at = clock_timestamp() + make_interval(secs => $3)
FROM due, centry.push_subscriptions AS s
WHERE d.notification_id = due.notification_id AND d.subscription_id = due.subscription_id
  AND s.id = d.subscription_id
RETURNING d.notification_id, d.subscription_id::text, s.endpoint, s.p256dh, s.auth, d.payload::text, d.urgency,
          d.attempts, d.expires_at`, limit, leaseID, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("web push: claim the due deliveries: %w", err)
	}
	defer rows.Close()
	var due []webpush.Delivery
	for rows.Next() {
		delivery := webpush.Delivery{LeaseID: leaseID}
		var payload string
		if err := rows.Scan(&delivery.NotificationID, &delivery.SubscriptionID, &delivery.Endpoint, &delivery.P256dh,
			&delivery.Auth, &payload, &delivery.Urgency, &delivery.Attempts, &delivery.ExpiresAt); err != nil {
			return nil, fmt.Errorf("web push: claim the due deliveries: %w", err)
		}
		delivery.Payload = []byte(payload)
		due = append(due, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("web push: claim the due deliveries: %w", err)
	}
	return due, nil
}

// Record applies the outcomes in one short transaction. A delivery's row
// changes only under the lease it was claimed with; the subscription's
// facts (a success, a 404 or 410, a refusal) apply whatever the lease.
func (r *WebPushRepository) Record(ctx context.Context, outcomes []webpush.Outcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		for _, outcome := range outcomes {
			if err := recordWebPushOutcome(ctx, tx, outcome); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("web push: record the sends: %w", err)
	}
	return nil
}

// deleteClaimedDeliverySQL removes a delivery still held by its claim.
const deleteClaimedDeliverySQL = `
DELETE FROM elitea_runtime.web_push_deliveries
WHERE notification_id = $1 AND subscription_id = $2::uuid AND lease_id = $3::uuid`

func recordWebPushOutcome(ctx context.Context, tx pgx.Tx, outcome webpush.Outcome) error {
	delivery := outcome.Delivery
	switch outcome.Action {
	case webpush.Delivered:
		if _, err := tx.Exec(ctx, deleteClaimedDeliverySQL,
			delivery.NotificationID, delivery.SubscriptionID, delivery.LeaseID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
UPDATE centry.push_subscriptions SET last_success_at = clock_timestamp(), rejected_sends = 0
WHERE id = $1::uuid`, delivery.SubscriptionID)
		return err
	case webpush.Gone:
		// The deliveries go with it (ON DELETE CASCADE).
		_, err := tx.Exec(ctx, `DELETE FROM centry.push_subscriptions WHERE id = $1::uuid`, delivery.SubscriptionID)
		return err
	case webpush.Rejected:
		if _, err := tx.Exec(ctx, deleteClaimedDeliverySQL,
			delivery.NotificationID, delivery.SubscriptionID, delivery.LeaseID); err != nil {
			return err
		}
		var rejected int
		err := tx.QueryRow(ctx, `
UPDATE centry.push_subscriptions SET rejected_sends = rejected_sends + 1
WHERE id = $1::uuid
RETURNING rejected_sends`, delivery.SubscriptionID).Scan(&rejected)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || rejected < webpush.MaxRejectedSends {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM centry.push_subscriptions WHERE id = $1::uuid`, delivery.SubscriptionID)
		return err
	case webpush.Retry:
		_, err := tx.Exec(ctx, `
UPDATE elitea_runtime.web_push_deliveries
SET attempts = attempts + 1, last_status = NULLIF($4, 0), next_attempt_at = $5, lease_id = NULL
WHERE notification_id = $1 AND subscription_id = $2::uuid AND lease_id = $3::uuid`,
			delivery.NotificationID, delivery.SubscriptionID, delivery.LeaseID, outcome.Status, outcome.RetryAt)
		return err
	default:
		_, err := tx.Exec(ctx, deleteClaimedDeliverySQL,
			delivery.NotificationID, delivery.SubscriptionID, delivery.LeaseID)
		return err
	}
}

var _ webpush.Store = (*WebPushRepository)(nil)
