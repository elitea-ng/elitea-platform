package webpush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The event types the dispatcher sends.
const (
	EventRunFinished     = "run_finished"
	EventApprovalWaiting = "agent_approval_required"
)

// Dispatch bounds (ADR-0045 decision 6).
const (
	// TTL is how long a push service keeps a message, and how long a
	// delivery is retried.
	TTL = 24 * time.Hour
	// PollInterval is how often the dispatcher looks for new rows.
	PollInterval = 5 * time.Second
	// NotificationBatch bounds the rows one pass reads.
	NotificationBatch = 256
	// DeliveryBatch bounds the sends of one pass.
	DeliveryBatch = 64
	// sendConcurrency bounds the sends in flight.
	sendConcurrency = 8
	// sendTimeout bounds one send.
	sendTimeout = 15 * time.Second
	// firstRetry and maxRetry bound the backoff.
	firstRetry = 30 * time.Second
	maxRetry   = time.Hour
	// MaxSubscriptionsPerUser is the cap on a person's browsers.
	MaxSubscriptionsPerUser = 10
)

// Notification is one new notification row the outbox reads.
type Notification struct {
	ID        int64
	UserID    int64
	ProjectID int64
	EventType string
	Meta      json.RawMessage
}

// Delivery is one send that waits.
type Delivery struct {
	NotificationID int64
	SubscriptionID string
	Endpoint       string
	P256dh         string
	Auth           string
	Payload        []byte
	Urgency        string
	Attempts       int
	ExpiresAt      time.Time
}

// Action is what a send's result does to its delivery.
type Action int

const (
	// Delivered: the push service accepted the message. The delivery is
	// removed and the subscription's last success is set.
	Delivered Action = iota + 1
	// Gone: the push service answered 404 or 410. The subscription and its
	// deliveries are removed.
	Gone
	// Retry: another failure. The delivery waits until RetryAt.
	Retry
	// Drop: the failure cannot heal or the TTL ends before the next try.
	Drop
)

// Outcome is the result of one send.
type Outcome struct {
	Delivery Delivery
	Action   Action
	Status   int
	RetryAt  time.Time
}

// Pass is one dispatcher pass inside the single-sender lock.
type Pass interface {
	// NewNotifications reads at most limit run_finished and
	// agent_approval_required rows above the cursor, oldest first, and the
	// highest id it looked at (rows of other types move the cursor too).
	NewNotifications(ctx context.Context, limit int) ([]Notification, int64, error)
	// Enqueue adds one delivery of the payload per subscription of the user.
	Enqueue(ctx context.Context, notification Notification, payload []byte, urgency string, expiresAt time.Time) error
	// AdvanceCursor moves the cursor to the id.
	AdvanceCursor(ctx context.Context, to int64) error
	// DueDeliveries removes the expired deliveries and reads at most limit
	// due ones, oldest first.
	DueDeliveries(ctx context.Context, limit int) ([]Delivery, error)
	// Record applies the outcomes.
	Record(ctx context.Context, outcomes []Outcome) error
}

// Store runs a pass under the single-sender lock. It answers false, and
// runs nothing, when another replica holds the lock.
type Store interface {
	WithSenderLock(ctx context.Context, pass func(Pass) error) (bool, error)
}

// HTTPDoer sends a request (an *http.Client).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Dispatcher sends the outbox.
type Dispatcher struct {
	store    Store
	service  *Service
	client   HTTPDoer
	logger   *slog.Logger
	now      func() time.Time
	interval time.Duration
}

// NewDispatcher builds the dispatcher.
func NewDispatcher(store Store, service *Service, client HTTPDoer, logger *slog.Logger) (*Dispatcher, error) {
	if store == nil || service == nil || client == nil {
		return nil, errors.New("web push dispatcher dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{store: store, service: service, client: client, logger: logger, now: time.Now,
		interval: PollInterval}, nil
}

// Run dispatches until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) error {
	if d == nil {
		return errors.New("web push dispatcher is not configured")
	}
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		if err := d.Dispatch(ctx); err != nil && ctx.Err() == nil {
			d.logger.Error("web push: dispatch failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Dispatch runs one pass: it moves the outbox cursor (adding deliveries
// while push is on, and skipping the rows while it is off), then sends the
// due deliveries. Another replica's pass makes this one a no-op.
func (d *Dispatcher) Dispatch(ctx context.Context) error {
	keys, subject, enabled := d.service.sender(ctx)
	_, err := d.store.WithSenderLock(ctx, func(pass Pass) error {
		if err := d.enqueue(ctx, pass, enabled); err != nil {
			return err
		}
		if !enabled {
			return nil
		}
		due, err := pass.DueDeliveries(ctx, DeliveryBatch)
		if err != nil || len(due) == 0 {
			return err
		}
		return pass.Record(ctx, d.sendAll(ctx, keys, subject, due))
	})
	return err
}

func (d *Dispatcher) enqueue(ctx context.Context, pass Pass, enabled bool) error {
	for ctx.Err() == nil {
		rows, last, err := pass.NewNotifications(ctx, NotificationBatch)
		if err != nil {
			return err
		}
		if last == 0 {
			return nil
		}
		if enabled {
			expires := d.now().Add(TTL)
			for _, row := range rows {
				payload, urgency, ok := BuildPayload(row)
				if !ok {
					continue
				}
				if err := pass.Enqueue(ctx, row, payload, urgency, expires); err != nil {
					return err
				}
			}
		}
		if err := pass.AdvanceCursor(ctx, last); err != nil {
			return err
		}
		if len(rows) < NotificationBatch {
			return nil
		}
	}
	return ctx.Err()
}

func (d *Dispatcher) sendAll(ctx context.Context, keys Keys, subject string, due []Delivery) []Outcome {
	outcomes := make([]Outcome, len(due))
	limit := make(chan struct{}, sendConcurrency)
	var wg sync.WaitGroup
	for i, delivery := range due {
		wg.Add(1)
		limit <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-limit }()
			outcomes[i] = d.send(ctx, keys, subject, delivery)
		}()
	}
	wg.Wait()
	return outcomes
}

// send sends one delivery and classifies the answer.
func (d *Dispatcher) send(ctx context.Context, keys Keys, subject string, delivery Delivery) Outcome {
	outcome := Outcome{Delivery: delivery}
	body, err := Encrypt(delivery.Payload, delivery.P256dh, delivery.Auth)
	if err != nil {
		// Keys that do not decode never will.
		outcome.Action = Drop
		return outcome
	}
	authorization, err := keys.Authorization(delivery.Endpoint, subject, d.now())
	if err != nil {
		outcome.Action = Drop
		return outcome
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(sendCtx, http.MethodPost, delivery.Endpoint, bytes.NewReader(body))
	if err != nil {
		outcome.Action = Drop
		return outcome
	}
	request.Header.Set("Content-Encoding", "aes128gcm")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("TTL", strconv.Itoa(max(int(delivery.ExpiresAt.Sub(d.now()).Seconds()), 0)))
	request.Header.Set("Urgency", delivery.Urgency)
	request.Header.Set("Authorization", authorization)
	response, err := d.client.Do(request)
	if err != nil {
		return d.retry(outcome)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	outcome.Status = response.StatusCode
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		outcome.Action = Delivered
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		outcome.Action = Gone
	case response.StatusCode == http.StatusRequestEntityTooLarge || response.StatusCode == http.StatusBadRequest:
		// The same request fails the same way again.
		outcome.Action = Drop
	default:
		// 401/403 (a key problem that an operator fixes), 429 and 5xx.
		return d.retry(outcome)
	}
	return outcome
}

// retry schedules the next try: 30 s, then doubling to one hour, never past
// the TTL.
func (d *Dispatcher) retry(outcome Outcome) Outcome {
	at := d.now().Add(Backoff(outcome.Delivery.Attempts + 1))
	if !at.Before(outcome.Delivery.ExpiresAt) {
		outcome.Action = Drop
		return outcome
	}
	outcome.Action, outcome.RetryAt = Retry, at
	return outcome
}

// Backoff is the wait after the attempt-th failed send (1-based).
func Backoff(attempt int) time.Duration {
	wait := firstRetry
	for i := 1; i < attempt && wait < maxRetry; i++ {
		wait *= 2
	}
	return min(wait, maxRetry)
}

// Payload is what a push carries: the type and the ids. Never content.
type Payload struct {
	Type             string `json:"type"`
	NotificationID   int64  `json:"notification_id"`
	ProjectID        int64  `json:"project_id"`
	ConversationUUID string `json:"conversation_uuid,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	ExecutionID      string `json:"execution_id,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
}

// BuildPayload builds the payload and the urgency of a notification row. It
// copies ids only: the outcome of a run_finished row, the execution and
// message ids of an agent_approval_required row. false: not a type the
// dispatcher sends.
func BuildPayload(row Notification) ([]byte, string, bool) {
	var meta struct {
		Outcome          string `json:"outcome"`
		ProjectID        int64  `json:"project_id"`
		ConversationUUID string `json:"conversation_uuid"`
		ExecutionID      string `json:"execution_id"`
		MessageID        string `json:"message_id"`
	}
	_ = json.Unmarshal(row.Meta, &meta)
	payload := Payload{
		Type: row.EventType, NotificationID: row.ID, ProjectID: row.ProjectID,
		ConversationUUID: boundedID(meta.ConversationUUID),
	}
	if payload.ProjectID <= 0 {
		payload.ProjectID = meta.ProjectID
	}
	urgency := "normal"
	switch row.EventType {
	case EventRunFinished:
		switch meta.Outcome {
		case "completed", "failed", "limit_reached":
			payload.Outcome = meta.Outcome
		case "waiting":
			payload.Outcome, urgency = meta.Outcome, "high"
		default:
			return nil, "", false
		}
	case EventApprovalWaiting:
		payload.ExecutionID, payload.MessageID = boundedID(meta.ExecutionID), boundedID(meta.MessageID)
		urgency = "high"
	default:
		return nil, "", false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", false
	}
	return raw, urgency, true
}

// boundedID keeps an id-shaped value (letters, digits, '-' and '_', at most
// 64) and drops anything else, so no text can ride in an id field.
func boundedID(value string) string {
	if len(value) == 0 || len(value) > 64 {
		return ""
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '-', character == '_':
		default:
			return ""
		}
	}
	return value
}
