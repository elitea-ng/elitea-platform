package webhook

// Dispatcher turns a domain event into signed HTTP POSTs to every active,
// subscribed webhook in the event's project, and records what happened.
//
// # HOW A PRODUCER REACHES HERE
//
// Dispatcher implements internal/events.Sink (structurally — this package
// does not import internal/events, which would be a cycle: events would then
// need webhook's types to describe its own Sink parameter). The composition
// root (cmd/elitea-main/main.go) passes a *Dispatcher as one of the sinks an
// events.Publisher fans every Emit out to, so every producer that already
// calls Emit for the project SSE stream reaches this dispatcher for free — no
// producer imports this package or knows webhooks exist.
//
// events.Publisher.Emit calls a Sink's HandleDomainEvent on its OWN
// goroutine, so nothing below blocks the request that produced the event.
//
// # WHAT #876's FIRST HALF LEFT UNFINISHED
//
// The registry UI and the five CRUD routes shipped in #876 with this comment
// in dispatcher.go: NewDispatcher had no caller anywhere in the repository.
// A registered webhook could not fire on anything. This file is the fix:
// every method below now has a caller, traced in the doc comment above.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// deliveryOutcome is the exit case one delivery attempt sequence lands on. It
// exists so attempt() has one return value instead of four.
type deliveryOutcome struct {
	Attempts     int
	Status       DeliveryStatus
	ResponseCode *int
	LastError    string
}

// SignatureHeader is the header a receiver reads to verify a delivery came
// from this platform and was not altered in transit. Its value is
// "sha256=<hex HMAC-SHA256 of the exact request body, keyed by the webhook's
// secret>" — see verifySignature in dispatcher_test.go for the reference
// verification a receiver implements.
const SignatureHeader = "X-Webhook-Signature"

// EventTypeHeader carries the event type outside the signed body too, so a
// receiver that fans out to multiple handlers can route without parsing JSON
// first. It is NOT part of what the signature covers — only the body is
// signed, matching every mainstream webhook provider's contract (Stripe,
// GitHub) and keeping verification a single documented byte range.
const EventTypeHeader = "X-Webhook-Event"

// deliveryTimeout bounds ONE HTTP attempt. maxDeliveryAttempts bounds how many
// attempts one event-webhook pair gets before the delivery is logged FAILED.
// deliveryBackoff is the pause between attempts, one entry shorter than
// maxDeliveryAttempts (no pause after the last attempt). Three attempts over
// at most ~6s keeps one goroutine's worst case bounded and small enough that
// Redeliver — which runs this same sequence SYNCHRONOUSLY, on the caller's
// request goroutine, so the settings page can show the outcome immediately —
// does not read as a hung request.
const (
	deliveryTimeout     = 10 * time.Second
	maxDeliveryAttempts = 3
)

var deliveryBackoff = []time.Duration{500 * time.Millisecond, 2 * time.Second}

// DeliveryStatus is the closed vocabulary webhook_deliveries.status holds —
// see migrations/shared/0122_webhooks_and_deliveries.sql's CHECK constraint,
// which this type's three values must stay in sync with.
type DeliveryStatus string

const (
	DeliveryStatusPending DeliveryStatus = "pending"
	DeliveryStatusSuccess DeliveryStatus = "success"
	DeliveryStatusFailed  DeliveryStatus = "failed"
)

// Delivery is one logged attempt sequence against one webhook for one event.
// It is the row webhook_deliveries stores and the shape the "Recent
// deliveries" panel and GET .../deliveries render.
type Delivery struct {
	ID        string
	WebhookID string
	ProjectID string
	Event     string
	Status    DeliveryStatus
	// Attempts is how many HTTP attempts this sequence made — 1 to
	// maxDeliveryAttempts. It is never 0: a Delivery is only logged after at
	// least one attempt has run.
	Attempts     int
	ResponseCode *int
	LastError    string
	// Payload is the EXACT bytes POSTed to the destination (the signed
	// body), kept so Redeliver resends byte-identical content rather than
	// re-deriving a payload that may have drifted since (a project rename
	// between the original event and a redelivery days later, say).
	Payload json.RawMessage
	// RedeliveryOf is the id of the Delivery this one resent, or "" for an
	// original delivery. A redelivery is always a NEW row — see Redeliver's
	// doc comment for why an update-in-place was rejected.
	RedeliveryOf string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// DeliveryRepository is webhook_deliveries' persistence seam.
type DeliveryRepository interface {
	// Create inserts one COMPLETED attempt sequence (Status is never
	// DeliveryStatusPending on the way in — see deliverAndLog).
	Create(ctx context.Context, d Delivery) (Delivery, error)
	// ListRecent returns a webhook's deliveries, newest first, capped at
	// limit.
	ListRecent(ctx context.Context, projectID, webhookID string, limit int) ([]Delivery, error)
	// Get reads one delivery, scoped to the project AND the webhook it
	// belongs to — the same "the path segment only says where to look"
	// discipline handler.go's five routes already apply, so a caller cannot
	// redeliver another project's webhook by guessing a delivery id.
	Get(ctx context.Context, projectID, webhookID, deliveryID string) (Delivery, error)
}

type Dispatcher struct {
	repo       Repository
	deliveries DeliveryRepository
	client     *http.Client
}

// NewDispatcher builds a Dispatcher. deliveries may be nil — a Dispatcher
// with no delivery log still SENDS every webhook (best effort), it just does
// not persist the outcome, which is the same "degraded, not wrong" shape
// WithUserContextDefaults documents elsewhere in this service for an absent
// optional dependency.
func NewDispatcher(repo Repository, deliveries DeliveryRepository) *Dispatcher {
	return &Dispatcher{
		repo:       repo,
		deliveries: deliveries,
		client: &http.Client{
			Timeout: deliveryTimeout,
		},
	}
}

// HandleDomainEvent is internal/events.Sink's method. See the package doc
// comment above for how a producer's Emit call reaches here.
func (d *Dispatcher) HandleDomainEvent(ctx context.Context, projectID, eventType string, payload any) {
	if d == nil || d.repo == nil || projectID == "" || eventType == "" {
		return
	}

	webhooks, err := d.repo.ListByEvent(ctx, projectID, eventType)
	if err != nil {
		slog.Error("webhook: list subscribers", "err", err, "project", projectID, "type", eventType)
		return
	}
	if len(webhooks) == 0 {
		return
	}

	body, err := deliveryBody(eventType, projectID, payload)
	if err != nil {
		slog.Error("webhook: marshal delivery body", "err", err, "type", eventType)
		return
	}

	for _, wh := range webhooks {
		if !wh.Active {
			continue
		}
		wh := wh
		// One goroutine per subscriber: a slow or unreachable destination
		// must not delay delivery to every other webhook on the same event,
		// the same reason the original (unwired) HandleEvent already used
		// `go d.deliver` per webhook.
		go d.deliverAndLog(ctx, wh, eventType, body)
	}
}

// deliveryBody is the exact byte sequence signed and POSTed. `payload` is
// whatever the producer passed to events.Publisher.Emit — a small struct or
// map describing the entity, never a full row (see each producer's own
// comment for what it sends and why).
func deliveryBody(eventType, projectID string, payload any) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":       eventType,
		"project_id": projectID,
		"payload":    payload,
		"timestamp":  time.Now().UTC(),
	})
}

// deliverAndLog runs the attempt sequence and writes exactly one Delivery row
// for it. It is the async path's terminal function (always called via `go`)
// and Redeliver's building block (called directly, on the caller's
// goroutine, for the synchronous UX Redeliver's own comment explains).
func (d *Dispatcher) deliverAndLog(ctx context.Context, wh Webhook, eventType string, body []byte) {
	outcome := d.attempt(ctx, wh, eventType, body)
	if d.deliveries == nil {
		return
	}
	if _, err := d.deliveries.Create(ctx, Delivery{
		WebhookID:    wh.ID,
		ProjectID:    wh.ProjectID,
		Event:        eventType,
		Status:       outcome.Status,
		Attempts:     outcome.Attempts,
		ResponseCode: outcome.ResponseCode,
		LastError:    outcome.LastError,
		Payload:      body,
	}); err != nil {
		slog.Error("webhook: log delivery", "err", err, "webhook", wh.ID)
	}
}

// attempt sends body to wh.URL, retrying up to maxDeliveryAttempts times with
// deliveryBackoff between attempts, and reports the outcome. A 2xx or 3xx
// response is success; any other status, or a transport error (DNS, refused
// connection, timeout), counts as a failed attempt and is retried.
func (d *Dispatcher) attempt(ctx context.Context, wh Webhook, eventType string, body []byte) deliveryOutcome {
	var outcome deliveryOutcome
	for i := 0; i < maxDeliveryAttempts; i++ {
		outcome.Attempts++
		code, err := d.send(ctx, wh, eventType, body)
		if err != nil {
			outcome.LastError = err.Error()
			outcome.ResponseCode = nil
		} else if code >= 200 && code < 400 {
			outcome.Status = DeliveryStatusSuccess
			outcome.ResponseCode = &code
			outcome.LastError = ""
			return outcome
		} else {
			outcome.ResponseCode = &code
			outcome.LastError = fmt.Sprintf("destination responded %d", code)
		}
		if i < maxDeliveryAttempts-1 {
			select {
			case <-ctx.Done():
				outcome.Status = DeliveryStatusFailed
				return outcome
			case <-time.After(deliveryBackoff[i]):
			}
		}
	}
	outcome.Status = DeliveryStatusFailed
	return outcome
}

// send makes exactly one HTTP attempt and returns the response status code
// (0 on a transport-level failure, alongside the error).
func (d *Dispatcher) send(ctx context.Context, wh Webhook, eventType string, body []byte) (int, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, wh.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "EliteA-Webhook/1.0")
	req.Header.Set(EventTypeHeader, eventType)
	if wh.Secret != "" {
		req.Header.Set(SignatureHeader, "sha256="+signBody(wh.Secret, body))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}

// signBody is the ONE place the signature is computed, so Redeliver and the
// original delivery can never disagree with each other or with a test's
// reference implementation.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ErrDeliveryNotFound is Redeliver's answer when this Dispatcher has no
// delivery log or dispatcher wired at all — see Redeliver's nil guard.
// apierr.NotFound rather than a plain error so apierr.Write answers 404
// rather than a bare 500.
var ErrDeliveryNotFound = apierr.NotFound("webhook: delivery not found")

// Redeliver resends a previously logged delivery's EXACT payload to the
// webhook's CURRENT url and secret, and logs a NEW Delivery row rather than
// mutating the original.
//
// A new row, not an update, for the same reason pipeline_triggers keeps a
// revoked row instead of deleting it (migrations/tenant/0133's header): "was
// this webhook still failing last Tuesday" has an answer from a row that is
// still there, and "the operator clicked redeliver and it worked" deserves
// its own row rather than overwriting the evidence of the failure that made
// them click it.
//
// It runs SYNCHRONOUSLY — unlike the async path every producer's Emit call
// reaches — because Redeliver is a person clicking a button in the deliveries
// panel who is waiting to see whether it worked; a 202 that reports nothing
// would make "Redeliver" indistinguishable from "maybe redeliver, check back
// later".
func (d *Dispatcher) Redeliver(ctx context.Context, projectID, webhookID, deliveryID string) (Delivery, error) {
	if d == nil || d.repo == nil || d.deliveries == nil {
		return Delivery{}, ErrDeliveryNotFound
	}
	original, err := d.deliveries.Get(ctx, projectID, webhookID, deliveryID)
	if err != nil {
		return Delivery{}, err
	}
	wh, err := d.repo.Get(ctx, projectID, webhookID)
	if err != nil {
		return Delivery{}, err
	}

	outcome := d.attempt(ctx, wh, original.Event, original.Payload)
	return d.deliveries.Create(ctx, Delivery{
		WebhookID:    wh.ID,
		ProjectID:    wh.ProjectID,
		Event:        original.Event,
		Status:       outcome.Status,
		Attempts:     outcome.Attempts,
		ResponseCode: outcome.ResponseCode,
		LastError:    outcome.LastError,
		Payload:      original.Payload,
		RedeliveryOf: original.ID,
	})
}
