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
	// NotificationBatch bounds the outbox rows one fan-out takes.
	NotificationBatch = 256
	// DeliveryBatch bounds the sends of one pass.
	DeliveryBatch = 64
	// sendConcurrency bounds the sends in flight.
	sendConcurrency = 8
	// sendTimeout bounds one send.
	sendTimeout = 15 * time.Second
	// ClaimLease is how long a claimed delivery stays invisible to the other
	// senders: well past a whole pass (DeliveryBatch / sendConcurrency rounds
	// of sendTimeout, and the record). A sender that dies leaves its claim to
	// expire, and the delivery goes out again.
	ClaimLease = 5 * time.Minute
	// recordAttempts and recordTimeout bound the write of a pass's
	// outcomes: a failed write leaves the deliveries to their lease, which
	// sends them again.
	recordAttempts = 3
	recordTimeout  = 30 * time.Second
	// firstRetry and maxRetry bound the backoff.
	firstRetry = 30 * time.Second
	maxRetry   = time.Hour
	// MaxSubscriptionsPerUser is the cap on a person's browsers.
	MaxSubscriptionsPerUser = 10
	// MaxRejectedSends is how many 401 or 403 answers in a row, with no
	// success between, delete a subscription.
	MaxRejectedSends = 3
)

// Notification is one outbox row: a new notification.
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
	// LeaseID is the claim the delivery was handed out under.
	LeaseID   string
	Endpoint  string
	P256dh    string
	Auth      string
	Payload   []byte
	Urgency   string
	Attempts  int
	ExpiresAt time.Time
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
	// Rejected: the push service refused the VAPID token (401 or 403). The
	// delivery is dropped, and the subscription is removed at its
	// MaxRejectedSends-th refusal in a row.
	Rejected
)

// Outcome is the result of one send.
type Outcome struct {
	Delivery Delivery
	Action   Action
	Status   int
	RetryAt  time.Time
}

// Plan builds what one outbox row sends; false sends nothing.
type Plan func(Notification) (payload []byte, urgency string, ok bool)

// Store is the outbox. Each method is one short transaction; no
// transaction is open while the dispatcher sends.
type Store interface {
	// FanOut removes at most limit outbox rows and, for each row plan
	// accepts, adds one delivery per subscription of the row's user that
	// expires at expiresAt. It answers how many outbox rows it removed.
	FanOut(ctx context.Context, limit int, plan Plan, expiresAt time.Time) (int, error)
	// ClaimDue removes the expired deliveries and claims at most limit due
	// ones, oldest first, for lease: no other sender gets them before the
	// lease ends. Replicas never claim the same delivery at once.
	ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error)
	// Record applies the outcomes. An outcome whose claim another sender
	// took over after the lease leaves the delivery alone.
	Record(ctx context.Context, outcomes []Outcome) error
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

// Dispatch runs one pass: it moves the outbox rows to deliveries (or drops
// them while push is off), claims the due deliveries, sends them with no
// transaction open, and records the outcomes. Replicas run passes side by
// side; each claims different deliveries.
func (d *Dispatcher) Dispatch(ctx context.Context) error {
	keys, subject, enabled := d.service.sender(ctx)
	if err := d.fanOut(ctx, enabled); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	due, err := d.store.ClaimDue(ctx, DeliveryBatch, ClaimLease)
	if err != nil || len(due) == 0 {
		return err
	}
	return d.record(ctx, d.sendAll(ctx, keys, subject, due))
}

func (d *Dispatcher) fanOut(ctx context.Context, enabled bool) error {
	plan := func(row Notification) ([]byte, string, bool) {
		if !enabled {
			// Push is off: the row is drained, so turning push on later
			// sends nothing old.
			return nil, "", false
		}
		return BuildPayload(row)
	}
	for ctx.Err() == nil {
		taken, err := d.store.FanOut(ctx, NotificationBatch, plan, d.now().Add(TTL))
		if err != nil || taken < NotificationBatch {
			return err
		}
	}
	return ctx.Err()
}

// record writes the outcomes of sends that already happened, so it outlives
// a cancelled ctx (a shutdown mid-pass) and tries again before it gives up:
// an outcome that is not written leaves its delivery to the lease, which
// sends it a second time.
func (d *Dispatcher) record(ctx context.Context, outcomes []Outcome) error {
	for _, outcome := range outcomes {
		if outcome.Action == Rejected {
			d.logger.WarnContext(ctx, "web push: the push service refused the VAPID token",
				"status", outcome.Status, "subscription_id", outcome.Delivery.SubscriptionID)
		}
	}
	detached := context.WithoutCancel(ctx)
	var err error
	for attempt := range recordAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
		recordCtx, cancel := context.WithTimeout(detached, recordTimeout)
		err = d.store.Record(recordCtx, outcomes)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
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
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		// RFC 8292: 403 is a token whose key is not the one the
		// subscription was made with (the browser must subscribe again),
		// 401 a token the service does not accept. Neither heals by
		// waiting. Some services (Apple) answer 403 for any bad token too,
		// an install-wide fault, so one refusal does not delete the
		// subscription: MaxRejectedSends in a row do.
		outcome.Action = Rejected
	default:
		// 429, 5xx and any other answer.
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
