package auditexport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Webhook headers.
const (
	// HeaderSignature is "t=<unix seconds>,v1=<hex HMAC-SHA256>". The HMAC is
	// over the ASCII timestamp, a dot and the exact request body bytes, under
	// the key named by HeaderKeyID. Verify it before decoding the body, and
	// reject a timestamp more than five minutes from your clock.
	HeaderSignature = "X-Elitea-Signature"
	// HeaderKeyID names the signing key, so a receiver that holds two keys
	// during a rotation picks the right one.
	HeaderKeyID = "X-Elitea-Key-Id"
	// HeaderDelivery is the batch id: stable across retries of one batch.
	HeaderDelivery = "X-Elitea-Delivery"
	// HeaderSchema is the envelope version of the batch.
	HeaderSchema = "X-Elitea-Schema"
)

// SigningKey is one webhook HMAC key.
type SigningKey struct {
	ID      string
	Secret  []byte
	Created time.Time
}

// KeySource returns the current keys. The webhook calls it for every
// delivery, so a rotated key file is picked up without a restart.
type KeySource func() ([]SigningKey, error)

// Sign computes the v1 signature for a body at time t.
func Sign(secret []byte, t time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(strconv.FormatInt(t.Unix(), 10)))
	_, _ = mac.Write([]byte{'.'})
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader renders the HeaderSignature value.
func SignatureHeader(secret []byte, t time.Time, body []byte) string {
	return "t=" + strconv.FormatInt(t.Unix(), 10) + ",v1=" + Sign(secret, t, body)
}

// WebhookConfig configures a WebhookSink.
type WebhookConfig struct {
	// URL is the receiver. The caller validates it against the egress guard
	// at boot; Client dials through the guard on every request.
	URL    string
	Keys   KeySource
	Client *http.Client
	// BatchSize is the most events in one POST. Default 100.
	BatchSize int
	// MaxAttempts is the tries per POST within one Send for a transient
	// failure, before Send returns the error and the relay offers the batch
	// again. Default 3.
	MaxAttempts int
	// BaseBackoff and MaxBackoff bound the wait between tries (full jitter,
	// doubling). Defaults 250ms and 5s.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// UserAgent defaults to "elitea-audit-export/1".
	UserAgent string

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// WebhookSink POSTs signed JSON batches.
type WebhookSink struct {
	cfg WebhookConfig
}

// NewWebhookSink validates the configuration.
func NewWebhookSink(cfg WebhookConfig) (*WebhookSink, error) {
	if cfg.URL == "" {
		return nil, errors.New("webhook sink needs a URL")
	}
	if cfg.Keys == nil {
		return nil, errors.New("webhook sink needs a key source")
	}
	if cfg.Client == nil {
		return nil, errors.New("webhook sink needs an HTTP client (built on the egress guard)")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 250 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "elitea-audit-export/1"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	return &WebhookSink{cfg: cfg}, nil
}

// Name is the sink's configuration name.
func (*WebhookSink) Name() string { return "webhook" }

// webhookBody is the POSTed JSON.
type webhookBody struct {
	Schema  string  `json:"schema"`
	BatchID string  `json:"batch_id"`
	Events  []Event `json:"events"`
}

// Send POSTs the events in order, in batches of BatchSize. A batch the
// receiver refuses for its content (400, 413 or 422) is split in halves and
// each half sent again, down to single events, so one malformed or oversized
// event does not take its neighbours with it. Send stops at the first event
// that is refused on its own, or at a transient failure, and reports the
// events delivered before it as a *PartialError; a refused event is named in
// a *PermanentError.
func (w *WebhookSink) Send(ctx context.Context, events []Event) error {
	settled := 0
	for start := 0; start < len(events); start += w.cfg.BatchSize {
		end := min(start+w.cfg.BatchSize, len(events))
		n, err := w.deliver(ctx, events[start:end])
		settled += n
		if err != nil {
			return withSettled(settled, err)
		}
	}
	return nil
}

// deliver sends one batch, bisecting on a content rejection. It returns how
// many leading events were delivered before the first failure.
func (w *WebhookSink) deliver(ctx context.Context, events []Event) (int, error) {
	err := w.sendBatch(ctx, events)
	if err == nil {
		return len(events), nil
	}
	var permanent *PermanentError
	if !errors.As(err, &permanent) {
		return 0, err
	}
	if len(events) == 1 {
		return 0, &PermanentError{Err: permanent.Err, Events: events[:1]}
	}
	mid := len(events) / 2
	n, err := w.deliver(ctx, events[:mid])
	if err != nil {
		return n, err
	}
	m, err := w.deliver(ctx, events[mid:])
	return mid + m, err
}

func (w *WebhookSink) sendBatch(ctx context.Context, events []Event) error {
	batchID := batchKey(events)
	body, err := json.Marshal(webhookBody{Schema: SchemaVersion, BatchID: batchID, Events: events})
	if err != nil {
		return fmt.Errorf("encode webhook batch: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= w.cfg.MaxAttempts; attempt++ {
		lastErr = w.post(ctx, batchID, body)
		if lastErr == nil {
			return nil
		}
		var permanent *PermanentError
		if errors.As(lastErr, &permanent) || ctx.Err() != nil {
			return lastErr
		}
		if attempt == w.cfg.MaxAttempts {
			break
		}
		if err := w.cfg.Sleep(ctx, w.backoff(attempt)); err != nil {
			return errors.Join(lastErr, err)
		}
	}
	return lastErr
}

// backoff is full-jitter exponential: a random wait up to base*2^(attempt-1),
// capped.
func (w *WebhookSink) backoff(attempt int) time.Duration {
	ceiling := w.cfg.BaseBackoff << (attempt - 1)
	if ceiling <= 0 || ceiling > w.cfg.MaxBackoff {
		ceiling = w.cfg.MaxBackoff
	}
	return time.Duration(rand.Int64N(int64(ceiling)) + 1)
}

func (w *WebhookSink) post(ctx context.Context, batchID string, body []byte) error {
	keys, err := w.cfg.Keys()
	if err != nil {
		return fmt.Errorf("load webhook signing keys: %w", err)
	}
	key, err := NewestKey(keys)
	if err != nil {
		return err
	}
	now := w.cfg.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", w.cfg.UserAgent)
	req.Header.Set(HeaderSignature, SignatureHeader(key.Secret, now, body))
	req.Header.Set(HeaderKeyID, key.ID)
	req.Header.Set(HeaderDelivery, batchID)
	req.Header.Set(HeaderSchema, SchemaVersion)

	resp, err := w.cfg.Client.Do(req)
	if err != nil {
		// Network failure, DNS, TLS, egress guard refusal: all transient. An
		// egress refusal is an operator error and must block and alert, not
		// dead-letter the evidence.
		return fmt.Errorf("post webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusRequestEntityTooLarge,
		resp.StatusCode == http.StatusUnprocessableEntity:
		return &PermanentError{Err: fmt.Errorf("receiver answered %d", resp.StatusCode)}
	default:
		// 5xx, 429, 408, 401/403/404 (a wrong key or URL), 3xx (redirects are
		// not followed): the same bytes may succeed once the receiver or the
		// configuration is fixed, so they wait.
		return fmt.Errorf("receiver answered %d", resp.StatusCode)
	}
}

// NewestKey picks the signing key: the one with the latest Created.
func NewestKey(keys []SigningKey) (SigningKey, error) {
	if len(keys) == 0 {
		return SigningKey{}, errors.New("no webhook signing key is configured")
	}
	newest := keys[0]
	for _, k := range keys[1:] {
		if k.Created.After(newest.Created) {
			newest = k
		}
	}
	return newest, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
