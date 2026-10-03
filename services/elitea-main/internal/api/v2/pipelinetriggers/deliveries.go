package pipelinetriggers

// Replay protection for SIGNED inbound deliveries (PR #1027 review).
//
// A signature proves a delivery is authentic. It does not prove the delivery
// is NEW, and before this file nothing recorded that one had been used:
//
//   - a `standard_webhooks_hmac` delivery (a GitLab signing token) was
//     accepted again and again for as long as its timestamp stayed inside
//     StandardWebhooksTolerance, and every copy was a separate run. GitLab's
//     own retry of a delivery that timed out double-ran the same way;
//   - an `hmac_sha256` delivery (GitHub) carries no timestamp at all, so a
//     captured one stayed valid for the life of the secret.
//
// THE KEY IS WHAT THE SENDER SIGNED, NOTHING ELSE. For Standard Webhooks that
// is `webhook-id`: the specification makes it the idempotency key, and it is
// inside the signed content, so a replay cannot change it without breaking
// the signature. For `hmac_sha256` it is the RAW BODY. GitHub's
// `X-GitHub-Delivery` would be the friendlier key — stable across a
// redelivery, unique per event — but it is NOT signed: a replay with a fresh
// GUID in that header would pass a dedupe keyed on it, so it is no defence
// against the attack this exists for. GitHub's payloads are unique per event
// in practice, so keying on the body costs a GitHub trigger nothing. A
// generic HMAC sender that posts the SAME body every time is the one sender
// this changes: inside deliveryRetention a repeat of an admitted body is
// answered with the first run. That sender's static signature was already a
// standing credential anyone who saw one request could reuse; varying the
// body (a timestamp, a nonce) is what makes its requests distinct.
//
// A BEARER trigger gets no dedupe. Its credential is the secret itself, not a
// per-delivery signature, so there is no signed delivery identity to key on —
// and a sender holding the secret can mint as many distinct requests as it
// likes anyway.
//
// WHAT A REPLAY GETS. The first run's ids, 202, and no second run: a
// provider's retry of a delivery whose answer it lost is then a success, which
// is the idempotency the Standard Webhooks specification asks of a receiver.
// A copy that arrives while the first is still being admitted is answered 503
// with Retry-After, and the retry finds the admitted run. A delivery whose
// admission FAILED releases its claim, so the sender's retry is a first try.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// deliveryRetention is how long an admitted delivery is remembered. It is far
// longer than StandardWebhooksTolerance (a Standard Webhooks replay outside
// the window is refused by the timestamp check anyway) and covers the
// automatic retry schedules GitHub and GitLab use.
const deliveryRetention = 72 * time.Hour

// deliveryInFlightTimeout is how long a claim with no run behind it blocks a
// copy. A process that died between the claim and the admission would
// otherwise hold the delivery until deliveryRetention; after this a retry
// claims it again.
const deliveryInFlightTimeout = 2 * time.Minute

// deliveryRetryAfterSeconds is the Retry-After an in-flight copy is answered
// with.
const deliveryRetryAfterSeconds = "5"

// deliveryState is what a claim found.
type deliveryState int

const (
	// deliveryNew: this request holds the claim and must admit the run.
	deliveryNew deliveryState = iota
	// deliveryAdmitted: an earlier copy already admitted a run.
	deliveryAdmitted
	// deliveryInFlight: an earlier copy holds the claim and has not finished.
	deliveryInFlight
)

// admittedDelivery is the run an earlier copy admitted.
type admittedDelivery struct {
	ExecutionID      string
	ConversationUUID string
	VersionID        int64
}

// signedDeliveryKey is the dedupe key for a request to a signing trigger, or
// "" for a trigger whose mode signs nothing per delivery.
//
// It is called only AFTER the signature verified, so the material it hashes is
// material the sender signed. The mode is part of the hash so that the two key
// spaces can never collide, and the digest bounds a caller-chosen
// `webhook-id` to a fixed 64 characters.
func signedDeliveryKey(trigger triggerRow, headers http.Header, raw []byte) string {
	digest := sha256.New()
	switch trigger.AuthMode {
	case AuthModeStandardWebhooks:
		// Trimmed exactly as standardWebhooksSignatureMatches trims it, so
		// the key is the id that was verified.
		id := strings.TrimSpace(headers.Get(StandardWebhooksIDHeader))
		if id == "" {
			return ""
		}
		_, _ = digest.Write([]byte(AuthModeStandardWebhooks + "\x00" + id))
	case AuthModeHMACSHA256:
		_, _ = digest.Write([]byte(AuthModeHMACSHA256 + "\x00"))
		_, _ = digest.Write(raw)
	default:
		return ""
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// claimDelivery records that this request is admitting the delivery, or
// reports the earlier copy that already is.
//
// The INSERT … ON CONFLICT is the whole race: two copies arriving together
// serialize on the primary key, and exactly one of them gets a row back. The
// conflict arm re-claims only a row that is no longer a live claim — older
// than the retention, or a claim abandoned without a run — so a stale row
// never blocks a delivery forever and a live one is never taken over.
func (h *Handler) claimDelivery(
	ctx context.Context, schema, tokenID, key string,
) (deliveryState, admittedDelivery, error) {
	// Pruning is opportunistic and failure-tolerant: a row it misses is
	// pruned on the next call, and an expired row is re-claimable below
	// whether or not it was deleted first.
	if _, err := h.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.pipeline_trigger_deliveries WHERE received_at < now() - make_interval(secs => $1)`,
		schema), deliveryRetention.Seconds()); err != nil {
		h.log().Warn("pipelinetriggers: prune delivery log", "err", err)
	}

	var claimed bool
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.pipeline_trigger_deliveries AS existing (token_id, delivery_key)
VALUES ($1, $2)
ON CONFLICT (token_id, delivery_key) DO UPDATE SET
	received_at = now(),
	execution_id = NULL,
	conversation_uuid = NULL,
	version_id = NULL
WHERE existing.received_at < now() - make_interval(secs => $3)
   OR (existing.execution_id IS NULL AND existing.received_at < now() - make_interval(secs => $4))
RETURNING TRUE`, schema),
		tokenID, key, deliveryRetention.Seconds(), deliveryInFlightTimeout.Seconds(),
	).Scan(&claimed)
	if err == nil && claimed {
		return deliveryNew, admittedDelivery{}, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return deliveryNew, admittedDelivery{}, fmt.Errorf("pipelinetriggers: claim delivery: %w", err)
	}

	var (
		executionID      *string
		conversationUUID *string
		versionID        *int64
	)
	err = h.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT execution_id, conversation_uuid::text, version_id::bigint
  FROM %s.pipeline_trigger_deliveries
 WHERE token_id = $1 AND delivery_key = $2`, schema), tokenID, key,
	).Scan(&executionID, &conversationUUID, &versionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The holder released its claim between the two statements: its
		// admission failed. Telling this copy to retry is right — the retry
		// claims afresh.
		return deliveryInFlight, admittedDelivery{}, nil
	case err != nil:
		return deliveryNew, admittedDelivery{}, fmt.Errorf("pipelinetriggers: read delivery: %w", err)
	}
	if executionID == nil || *executionID == "" {
		return deliveryInFlight, admittedDelivery{}, nil
	}
	admitted := admittedDelivery{ExecutionID: *executionID}
	if conversationUUID != nil {
		admitted.ConversationUUID = *conversationUUID
	}
	if versionID != nil {
		admitted.VersionID = *versionID
	}
	return deliveryAdmitted, admitted, nil
}

// completeDelivery stamps the run a claim admitted, so a replay is answered
// with it. A failure is logged and not returned: the run IS admitted, and the
// worst a missing stamp does is let a copy re-claim the delivery after
// deliveryInFlightTimeout.
func (h *Handler) completeDelivery(ctx context.Context, schema, tokenID, key string, outcome runOutcome) {
	ctx = context.WithoutCancel(ctx)
	var conversation any
	if outcome.ConversationUUID != "" {
		conversation = outcome.ConversationUUID
	}
	if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.pipeline_trigger_deliveries
   SET execution_id = $3, conversation_uuid = $4::uuid, version_id = $5
 WHERE token_id = $1 AND delivery_key = $2`, schema),
		tokenID, key, outcome.ExecutionID, conversation, outcome.VersionID,
	); err != nil {
		h.log().Error("pipelinetriggers: record admitted delivery",
			"execution_id", outcome.ExecutionID, "err", err)
	}
}

// releaseDelivery drops a claim whose admission failed, so the sender's retry
// is treated as a first delivery. It never removes a row that carries a run.
func (h *Handler) releaseDelivery(ctx context.Context, schema, tokenID, key string) {
	ctx = context.WithoutCancel(ctx)
	if _, err := h.pool.Exec(ctx, fmt.Sprintf(`
DELETE FROM %s.pipeline_trigger_deliveries
 WHERE token_id = $1 AND delivery_key = $2 AND execution_id IS NULL`, schema),
		tokenID, key,
	); err != nil {
		h.log().Warn("pipelinetriggers: release delivery claim", "err", err)
	}
}
