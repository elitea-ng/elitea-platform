package agentexecution

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

// CurrentContinuationFanoutWake is the continuation Main admits by itself
// when a decision lands on a parked fan-out response
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md §8, §8.1). A browser
// never requests it: CurrentContinuationResolveRequest.Validate refuses the
// kind, and the wake carries no decision and no interrupt kind.
const CurrentContinuationFanoutWake CurrentContinuationKind = "fanout_wake"

// FanoutWakeSchema names the payload.meta.fanout_wake_v1 marker.
const FanoutWakeSchema = "elitea.pipeline.fanout-wake.v1"

var errInvalidFanoutWake = errors.New("fanout wake marker is invalid")

// FanoutWakeIdempotencyKey is continue-interrupt/{responseMessageID}/{decision_revision}.
// It is the admission idempotency key of the wake job and the derived key of
// its durable wake row, so a retried admission collapses onto one job.
func FanoutWakeIdempotencyKey(responseMessageID string, decisionRevision int64) string {
	return "continue-interrupt/" + responseMessageID + "/" + strconv.FormatInt(decisionRevision, 10)
}

// FanoutWakeMeta writes the wake input's meta member
// {"fanout_wake_v1": {decision_revision, response_message_id, schema}} in the
// contract canonical form (keys sorted, compact). Every other meta member of
// a wake input is the parked execution's frozen meta, without any resume
// marker; the Worker re-enters the parked node and fetches.
func FanoutWakeMeta(responseMessageID string, decisionRevision int64) (json.RawMessage, error) {
	if !noderecovery.ValidResponseMessageID(responseMessageID) || decisionRevision < 1 {
		return nil, errInvalidFanoutWake
	}
	// Only ASCII ids and an integer: encoding/json output is already canonical.
	return json.RawMessage(`{"decision_revision":` + strconv.FormatInt(decisionRevision, 10) +
		`,"response_message_id":"` + responseMessageID + `","schema":"` + FanoutWakeSchema + `"}`), nil
}
