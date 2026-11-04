package repos

// Web push subscriptions and the push outbox (ADR-0045 decision 6, #1437;
// shared 0223). See package webpush.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/webpush"
)

// webPushSenderLock is the advisory lock of the single sender.
const webPushSenderLock = "elitea.web_push.dispatcher"

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
    last_success_at = CASE WHEN user_id = $2 THEN last_success_at END
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

// WithSenderLock runs one dispatcher pass in a transaction that holds the
// sender lock. Another replica's pass makes this call answer false.
func (r *WebPushRepository) WithSenderLock(ctx context.Context, pass func(webpush.Pass) error) (bool, error) {
	ran := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`,
			webPushSenderLock).Scan(&locked); err != nil || !locked {
			return err
		}
		ran = true
		return pass(webPushPass{tx: tx})
	})
	if err != nil {
		if undefinedColumnOrTable(err) {
			// A database before 0223 (or without the legacy notification
			// table) has no outbox.
			return false, nil
		}
		return ran, fmt.Errorf("web push: pass: %w", err)
	}
	return ran, nil
}

// webPushPass is one pass on the locked transaction.
type webPushPass struct {
	tx pgx.Tx
}

func (p webPushPass) NewNotifications(ctx context.Context, limit int) ([]webpush.Notification, int64, error) {
	var cursor int64
	if err := p.tx.QueryRow(ctx, `
SELECT last_notification_id FROM elitea_runtime.web_push_cursor WHERE id = 1 FOR UPDATE`).Scan(&cursor); err != nil {
		return nil, 0, err
	}
	rows, err := p.tx.Query(ctx, `
SELECT id::bigint, COALESCE(user_id, 0)::bigint, COALESCE(project_id, 0)::bigint, COALESCE(event_type, ''),
       COALESCE(meta::text, '{}')
FROM centry.notifications
WHERE id > $1
ORDER BY id
LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out  []webpush.Notification
		last int64
	)
	for rows.Next() {
		var (
			row  webpush.Notification
			meta string
		)
		if err := rows.Scan(&row.ID, &row.UserID, &row.ProjectID, &row.EventType, &meta); err != nil {
			return nil, 0, err
		}
		last = row.ID
		if row.EventType != webpush.EventRunFinished && row.EventType != webpush.EventApprovalWaiting {
			continue
		}
		row.Meta = []byte(meta)
		out = append(out, row)
	}
	return out, last, rows.Err()
}

func (p webPushPass) Enqueue(
	ctx context.Context, notification webpush.Notification, payload []byte, urgency string, expiresAt time.Time,
) error {
	_, err := p.tx.Exec(ctx, `
INSERT INTO elitea_runtime.web_push_deliveries (notification_id, subscription_id, payload, urgency, expires_at)
SELECT $1, s.id, $3::jsonb, $4, $5
FROM centry.push_subscriptions AS s
WHERE s.user_id = $2
ON CONFLICT (notification_id, subscription_id) DO NOTHING`,
		notification.ID, notification.UserID, string(payload), urgency, expiresAt)
	return err
}

func (p webPushPass) AdvanceCursor(ctx context.Context, to int64) error {
	_, err := p.tx.Exec(ctx, `
UPDATE elitea_runtime.web_push_cursor
SET last_notification_id = GREATEST(last_notification_id, $1), updated_at = clock_timestamp()
WHERE id = 1`, to)
	return err
}

func (p webPushPass) DueDeliveries(ctx context.Context, limit int) ([]webpush.Delivery, error) {
	if _, err := p.tx.Exec(ctx, `
DELETE FROM elitea_runtime.web_push_deliveries WHERE expires_at <= clock_timestamp()`); err != nil {
		return nil, err
	}
	rows, err := p.tx.Query(ctx, `
SELECT d.notification_id, d.subscription_id::text, s.endpoint, s.p256dh, s.auth, d.payload::text, d.urgency,
       d.attempts, d.expires_at
FROM elitea_runtime.web_push_deliveries AS d
JOIN centry.push_subscriptions AS s ON s.id = d.subscription_id
WHERE d.next_attempt_at <= clock_timestamp()
ORDER BY d.next_attempt_at, d.notification_id
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []webpush.Delivery
	for rows.Next() {
		var (
			delivery webpush.Delivery
			payload  string
		)
		if err := rows.Scan(&delivery.NotificationID, &delivery.SubscriptionID, &delivery.Endpoint, &delivery.P256dh,
			&delivery.Auth, &payload, &delivery.Urgency, &delivery.Attempts, &delivery.ExpiresAt); err != nil {
			return nil, err
		}
		delivery.Payload = []byte(payload)
		due = append(due, delivery)
	}
	return due, rows.Err()
}

func (p webPushPass) Record(ctx context.Context, outcomes []webpush.Outcome) error {
	for _, outcome := range outcomes {
		delivery := outcome.Delivery
		var err error
		switch outcome.Action {
		case webpush.Delivered:
			if _, err = p.tx.Exec(ctx, `
DELETE FROM elitea_runtime.web_push_deliveries WHERE notification_id = $1 AND subscription_id = $2::uuid`,
				delivery.NotificationID, delivery.SubscriptionID); err == nil {
				_, err = p.tx.Exec(ctx, `
UPDATE centry.push_subscriptions SET last_success_at = clock_timestamp() WHERE id = $1::uuid`,
					delivery.SubscriptionID)
			}
		case webpush.Gone:
			// The deliveries go with it (ON DELETE CASCADE).
			_, err = p.tx.Exec(ctx, `DELETE FROM centry.push_subscriptions WHERE id = $1::uuid`, delivery.SubscriptionID)
		case webpush.Retry:
			_, err = p.tx.Exec(ctx, `
UPDATE elitea_runtime.web_push_deliveries
SET attempts = attempts + 1, last_status = NULLIF($3, 0), next_attempt_at = $4
WHERE notification_id = $1 AND subscription_id = $2::uuid`,
				delivery.NotificationID, delivery.SubscriptionID, outcome.Status, outcome.RetryAt)
		default:
			_, err = p.tx.Exec(ctx, `
DELETE FROM elitea_runtime.web_push_deliveries WHERE notification_id = $1 AND subscription_id = $2::uuid`,
				delivery.NotificationID, delivery.SubscriptionID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var _ webpush.Store = (*WebPushRepository)(nil)
