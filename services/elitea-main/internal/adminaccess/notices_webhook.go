package adminaccess

// The out-of-band webhook of the break-glass notices (ADR-0059 decision 5,
// "Loud"; story R2.5b, issue 2598).
//
// E-mail can be down at the moment break-glass is used. A deployment that
// sets the notice webhook gets one more row in the outbox for each of five
// events (an activation, a critical change under an elevation, an overdue
// review, the move to unreviewed_no_reviewer, an `unauthorized` review),
// written by WriteElevationNotices in the transaction of the event, and
// delivered by the same NoticeSender with its own retries, on its own loop:
// a failing webhook never holds the e-mail rows, and a failing mailer never
// holds the webhook.
//
// The webhook URL of Slack, Teams or PagerDuty carries the receiver's token,
// so it is a credential: it is read from the environment of Main at start
// (the chart fills it from an existing Secret), never stored, never logged,
// and no API, Configuration key or elevation can change or silence it. Every
// error text the sender keeps or logs has the URL scrubbed from it.
//
// The call is client-facing egress, not an internal hop: an HTTPS POST
// through the egress guard (private, loopback, link-local and metadata
// destinations are refused unless the operator allowlists them), TLS
// verification on, no redirect followed, a short timeout. The body is one
// generic JSON document with ids, a type and a time only: it names no
// person. It is signed as the tenant webhooks are: X-Webhook-Signature
// carries sha256=<hex HMAC-SHA256 of the body>, keyed by a secret of its own.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/libs/go/secretstrength"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

// NoticeChannelWebhook is the outbox channel of the webhook rows. A webhook
// row has no recipient person (shared 0280).
const NoticeChannelWebhook = "webhook"

// WebhookNoticeKinds are the kinds the webhook reports. A request goes to
// the approvers only, and the `inherit` token notice to its owner.
var WebhookNoticeKinds = []NoticeKind{
	NoticeActivation, NoticeCriticalChange, NoticeReviewOverdue,
	NoticeUnreviewedNoReviewer, NoticeReviewUnauthorized,
}

// The environment of the webhook. The chart sets the first two from an
// existing Secret (main.breakGlass.noticeWebhook) and the third from a plain
// value.
const (
	NoticeWebhookURLEnv       = "ELITEA_BREAK_GLASS_NOTICE_WEBHOOK_URL"
	NoticeWebhookSecretEnv    = "ELITEA_BREAK_GLASS_NOTICE_WEBHOOK_SECRET"
	NoticeWebhookAllowlistEnv = "ELITEA_BREAK_GLASS_NOTICE_WEBHOOK_EGRESS_ALLOWLIST"
)

// The headers of a delivery, the same names as the tenant webhooks'
// (api/webhook SignatureHeader and EventTypeHeader).
const (
	NoticeWebhookSignatureHeader = "X-Webhook-Signature"
	NoticeWebhookEventHeader     = "X-Webhook-Event"
)

// noticeWebhookTimeout bounds one delivery; noticeWebhookMaxBody bounds the
// response body read.
const (
	noticeWebhookTimeout = 10 * time.Second
	noticeWebhookMaxBody = 4096
)

// noticeWebhookEnabled is set once at start when the deployment configures
// the webhook, before any route or routine runs: WriteElevationNotices then
// writes a webhook row for each event the webhook reports. A writer on a
// deployment with no webhook writes none, so no row waits for a channel that
// does not exist.
var noticeWebhookEnabled atomic.Bool

// SetNoticeWebhookEnabled records whether the webhook is configured. Main
// calls it at start; a test that turns it on turns it off again.
func SetNoticeWebhookEnabled(enabled bool) { noticeWebhookEnabled.Store(enabled) }

// NoticeWebhookEnabled reports SetNoticeWebhookEnabled's value.
func NoticeWebhookEnabled() bool { return noticeWebhookEnabled.Load() }

// writeWebhookNotice writes the one webhook row of an event, when the
// webhook is configured and reports the kind.
func writeWebhookNotice(ctx context.Context, tx pgx.Tx, notice ElevationNotice, detail []byte) (int, error) {
	if !noticeWebhookEnabled.Load() || !slices.Contains(WebhookNoticeKinds, notice.Kind) {
		return 0, nil
	}
	at := notice.At.UTC()
	tag, err := tx.Exec(ctx, `
INSERT INTO elitea_auth.admin_access_notices
    (kind, channel, elevation_id, recipient_user_id, recipient_email, actor_user_id, detail,
     created_at, next_attempt_at)
VALUES ($1, '`+NoticeChannelWebhook+`', $2, NULL, '', NULLIF($3::bigint, 0), $4::jsonb, $5, $5)`,
		string(notice.Kind), notice.Elevation.ID, notice.ActorUserID, detail, at)
	if err != nil {
		return 0, fmt.Errorf("write the webhook notice: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// WebhookNotice is the body of one delivery: ids, a type and a time. No
// person, no reason, no address.
type WebhookNotice struct {
	// Type is the notice kind.
	Type        string `json:"type"`
	ElevationID string `json:"elevation_id"`
	NoticeID    string `json:"notice_id"`
	// Time is the event's time, RFC 3339 in UTC.
	Time  string `json:"time"`
	Level string `json:"level"`
	Path  string `json:"path"`
}

// NoticeWebhookSender delivers one webhook notice. NoticeWebhook is the
// production one.
type NoticeWebhookSender interface {
	DeliverNotice(ctx context.Context, notice WebhookNotice) error
}

// NoticeWebhookConfig is the webhook as Main reads it at start.
type NoticeWebhookConfig struct {
	// URL is the receiver's https URL. It is a credential.
	URL string
	// Secret keys the signature; secretstrength.Check holds it.
	Secret string
	// Allowlist is the egress allowlist, comma-separated (the grammar of
	// the other egress allowlists): the private destinations the webhook
	// may reach. Empty permits public destinations only.
	Allowlist string
	// RootCAs, when set, replaces the system roots (a receiver behind a
	// private authority, a test server). Verification is never off.
	RootCAs *x509.CertPool
}

// NoticeWebhookConfigFromEnv reads the webhook from the environment. ok is
// false when no URL is set (the webhook is off); a secret or an allowlist
// without a URL is a refusal, since the operator meant to set one.
func NoticeWebhookConfigFromEnv(getenv func(string) string) (NoticeWebhookConfig, bool, error) {
	config := NoticeWebhookConfig{
		URL:       strings.TrimSpace(getenv(NoticeWebhookURLEnv)),
		Secret:    getenv(NoticeWebhookSecretEnv),
		Allowlist: getenv(NoticeWebhookAllowlistEnv),
	}
	if config.URL == "" {
		if strings.TrimSpace(config.Secret) != "" || strings.TrimSpace(config.Allowlist) != "" {
			return NoticeWebhookConfig{}, false, fmt.Errorf("%s or %s is set without %s: set the webhook URL too, or unset both",
				NoticeWebhookSecretEnv, NoticeWebhookAllowlistEnv, NoticeWebhookURLEnv)
		}
		return NoticeWebhookConfig{}, false, nil
	}
	return config, true, nil
}

// NoticeWebhook is the production NoticeWebhookSender.
type NoticeWebhook struct {
	client *http.Client
	url    string
	secret []byte
	logger *slog.Logger
	// redact are the parts of the URL that never leave this value: the
	// URL itself, its host, its path and its query.
	redact []string
}

// NewNoticeWebhook checks the configuration and builds the sender. Every
// error names the environment variable, never the URL.
func NewNoticeWebhook(config NoticeWebhookConfig, logger *slog.Logger) (*NoticeWebhook, error) {
	if logger == nil {
		logger = slog.Default()
	}
	parsed, err := url.Parse(config.URL)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s is not a URL", NoticeWebhookURLEnv)
	case parsed.Scheme != "https":
		return nil, fmt.Errorf("%s must be an https URL", NoticeWebhookURLEnv)
	case parsed.Hostname() == "":
		return nil, fmt.Errorf("%s has no host", NoticeWebhookURLEnv)
	}
	if err := secretstrength.Check(NoticeWebhookSecretEnv, config.Secret); err != nil {
		return nil, err
	}
	allowlist, err := egress.ParseAllowlist(splitList(config.Allowlist))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", NoticeWebhookAllowlistEnv, err)
	}
	transport := egress.New(allowlist, egress.RequireHTTPS()).Transport()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: config.RootCAs}
	redact := []string{config.URL, parsed.Host, parsed.Hostname()}
	if len(parsed.EscapedPath()) > 1 {
		redact = append(redact, parsed.EscapedPath(), parsed.Path)
	}
	if parsed.RawQuery != "" {
		redact = append(redact, parsed.RawQuery)
	}
	// The longest first, so a part never leaves a piece of a longer one.
	slices.SortFunc(redact, func(a, b string) int { return len(b) - len(a) })
	return &NoticeWebhook{
		client: &http.Client{
			Transport: transport,
			Timeout:   noticeWebhookTimeout,
			// A redirect is answered as a failure: its Location is never
			// dialled.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		url:    config.URL,
		secret: []byte(strings.TrimSpace(config.Secret)),
		logger: logger,
		redact: slices.Compact(redact),
	}, nil
}

func splitList(raw string) []string {
	var out []string
	for field := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// SignNoticeWebhookBody is the value of NoticeWebhookSignatureHeader for a
// body: what a receiver recomputes to verify a delivery.
func SignNoticeWebhookBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// DeliverNotice posts one notice. A transport error, a timeout, a redirect
// and any status outside 2xx are failures; the error text has the URL
// scrubbed from it. It logs the outcome with the notice id, never the URL.
func (w *NoticeWebhook) DeliverNotice(ctx context.Context, notice WebhookNotice) error {
	err := w.deliver(ctx, notice)
	if err != nil {
		w.logger.Warn("break-glass notice webhook delivery failed",
			"notice_id", notice.NoticeID, "type", notice.Type, "err", err.Error())
		return err
	}
	w.logger.Info("break-glass notice webhook delivered", "notice_id", notice.NoticeID, "type", notice.Type)
	return nil
}

func (w *NoticeWebhook) deliver(ctx context.Context, notice WebhookNotice) error {
	body, err := json.Marshal(notice)
	if err != nil {
		return fmt.Errorf("notice webhook: encode the body: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, noticeWebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return w.scrubbed("build the request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "elitea-break-glass-notice/1")
	req.Header.Set(NoticeWebhookEventHeader, notice.Type)
	req.Header.Set(NoticeWebhookSignatureHeader, SignNoticeWebhookBody(w.secret, body))
	resp, err := w.client.Do(req)
	if err != nil {
		// *url.Error repeats the URL; keep what went wrong only.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return w.scrubbed("post", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, noticeWebhookMaxBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notice webhook: the receiver answered %d", resp.StatusCode)
	}
	return nil
}

// scrubbed is err with every part of the URL replaced, under a fixed prefix.
func (w *NoticeWebhook) scrubbed(step string, err error) error {
	text := err.Error()
	for _, part := range w.redact {
		if part != "" {
			text = strings.ReplaceAll(text, part, "[webhook]")
		}
	}
	return fmt.Errorf("notice webhook: %s: %s", step, text)
}

/* ── the sender's webhook loop ─────────────────────────────────────────── */

// WithWebhook gives the sender the webhook channel. Nil leaves it e-mail
// only.
func (s *NoticeSender) WithWebhook(webhook NoticeWebhookSender) *NoticeSender {
	s.webhook = webhook
	return s
}

// sendWebhooks delivers the due webhook rows, as the e-mail loop does: one
// claim per row, the same lease and retry delay, and an end to the pass
// after maxNoticeFailuresInARow failures in a row.
func (s *NoticeSender) sendWebhooks(ctx context.Context, result *NoticeSendResult) []error {
	var errs []error
	failedInARow := 0
	for range s.batch {
		if failedInARow >= maxNoticeFailuresInARow {
			break
		}
		notice, payload, claimed, err := s.claimWebhook(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if !claimed {
			break
		}
		if sendErr := s.webhook.DeliverNotice(ctx, payload); sendErr != nil {
			result.WebhookFailed++
			failedInARow++
			if err := s.markFailed(ctx, notice, sendErr); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		failedInARow = 0
		if err := s.markSent(ctx, notice); err != nil {
			errs = append(errs, err)
			continue
		}
		result.WebhookSent++
	}
	return errs
}

// claimWebhook takes one due webhook row under the lease.
func (s *NoticeSender) claimWebhook(ctx context.Context) (claimedNotice, WebhookNotice, bool, error) {
	now := s.now().UTC()
	var notice claimedNotice
	var kind, elevationID string
	var created time.Time
	var payload WebhookNotice
	err := s.db.QueryRow(ctx, `
WITH due AS (
    SELECT id FROM elitea_auth.admin_access_notices
    WHERE sent_at IS NULL AND channel = '`+NoticeChannelWebhook+`' AND next_attempt_at <= $1
    ORDER BY next_attempt_at, created_at, id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE elitea_auth.admin_access_notices notice
    SET attempts = notice.attempts + 1,
        next_attempt_at = $1::timestamptz + make_interval(secs => $2)
    FROM due WHERE notice.id = due.id
    RETURNING notice.*
)
SELECT claimed.id, claimed.kind, claimed.attempts, claimed.created_at,
       COALESCE(claimed.elevation_id::text, ''),
       COALESCE(elevation.level, ''), COALESCE(elevation.path, '')
FROM claimed
LEFT JOIN elitea_auth.admin_elevations elevation ON elevation.id = claimed.elevation_id`,
		now, noticeLease.Seconds()).Scan(&notice.ID, &kind, &notice.attempts, &created, &elevationID,
		&payload.Level, &payload.Path)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimedNotice{}, WebhookNotice{}, false, nil
	}
	if err != nil {
		return claimedNotice{}, WebhookNotice{}, false, fmt.Errorf("access notice sender: claim a webhook row: %w", err)
	}
	notice.Kind = NoticeKind(kind)
	notice.at = now
	if elevationID != "" {
		if notice.ElevationID, err = uuid.Parse(elevationID); err != nil {
			return claimedNotice{}, WebhookNotice{}, false, fmt.Errorf("access notice sender: row %s: %w", notice.ID, err)
		}
	}
	payload.Type = kind
	payload.ElevationID = elevationID
	payload.NoticeID = notice.ID.String()
	payload.Time = created.UTC().Format(time.RFC3339)
	return notice, payload, true, nil
}

// webhookPending counts the unsent webhook rows.
func (s *NoticeSender) webhookPending(ctx context.Context) (int, error) {
	var pending int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM elitea_auth.admin_access_notices
WHERE sent_at IS NULL AND channel = '`+NoticeChannelWebhook+`'`).Scan(&pending); err != nil {
		return 0, fmt.Errorf("access notice sender: count the pending webhook rows: %w", err)
	}
	return pending, nil
}
