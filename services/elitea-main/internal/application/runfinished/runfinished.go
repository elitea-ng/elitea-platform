// Package runfinished is the content-free run_finished notification
// (ADR-0045 decision 5, #1437).
//
// A run that a person may have left gets one row in centry.notifications
// when its chain ends: the bell shows it, the desktop shows it as a native
// notice, and the web push dispatcher (package webpush) sends it to the
// person's browsers. The row's meta holds ids and the outcome only. It never
// holds a prompt, an answer or a title.
//
// The writers live in the repositories, in the transaction that ends the
// chain, so the row and the end state commit together:
//
//   - a cloud chain: the after-settle hook and the chain sweep
//     (repos.endRunChain, repos.SweepChains);
//   - a desktop chain: the local turn commit that carries chain_outcome;
//   - a schedule or webhook run, or a cloud turn of a handed-off thread,
//     that has no chain: the settle hook in this package (Hook).
//
// The notification uuid is derived from the chain root, so a replayed settle
// or commit writes nothing new (ON CONFLICT (uuid) DO NOTHING). The write is
// best-effort: it runs in a savepoint, and a failure leaves the end state to
// commit without its row.
package runfinished

import (
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// EventType is centry.notifications.event_type of the row.
const EventType = "run_finished"

// The outcomes of a chain (unattended.End*). A cancelled chain gets no row.
const (
	OutcomeCompleted    = "completed"
	OutcomeFailed       = "failed"
	OutcomeWaiting      = "waiting"
	OutcomeLimitReached = "limit_reached"
	OutcomeCancelled    = "cancelled"
)

// The origins of a chain.
const (
	OriginCloud   = "cloud"
	OriginDesktop = "desktop"
)

// MinDuration is how long a chain runs before a completed chain gets a row
// on its duration alone.
const MinDuration = 2 * time.Minute

// Facts is what the noise rule reads about an ended chain.
type Facts struct {
	// Outcome is the chain's end state.
	Outcome string
	// Continuations is the count of automatic continuations.
	Continuations int
	// Duration is the time from the chain's start to its end.
	Duration time.Duration
	// TriggerOrigin is manual, api, schedule or webhook.
	TriggerOrigin string
	// HandedOff is true for a cloud turn of a handed-off thread (ADR-0034).
	HandedOff bool
}

// Notify is the noise rule (ADR-0045 decision 5, amended 2026-11-03). Almost
// every send carries the default grant, so a row for each chain would mark
// every answer. A chain gets a row only when the person may have left it:
//
//   - a cancelled chain never gets a row (the person stopped it);
//   - failed, waiting and limit_reached always get one;
//   - completed gets one when the chain had at least one continuation, ran
//     for MinDuration or more, is a schedule or webhook run, or is a cloud
//     turn of a handed-off thread.
//
// An unknown outcome gets no row.
func Notify(facts Facts) bool {
	switch facts.Outcome {
	case OutcomeFailed, OutcomeWaiting, OutcomeLimitReached:
		return true
	case OutcomeCompleted:
		return facts.Continuations >= 1 || facts.Duration >= MinDuration ||
			unattended.IsSystemTrigger(facts.TriggerOrigin) || facts.HandedOff
	}
	return false
}

// Meta is the notification's meta. It holds ids and the outcome only.
type Meta struct {
	Outcome string `json:"outcome"`
	// Limit names the limit of a limit_reached chain: continuations, spend
	// or deadline.
	Limit     string `json:"limit,omitempty"`
	ProjectID int64  `json:"project_id"`
	// ConversationUUID is the chain's conversation.
	ConversationUUID string `json:"conversation_uuid,omitempty"`
	// ThreadID is the desktop host's thread id of a desktop chain: the
	// conversation's numeric id in decimal (the id the desktop app keeps its
	// Local work threads under). Empty when Main cannot resolve it.
	ThreadID             string `json:"thread_id,omitempty"`
	ChainRootExecutionID string `json:"chain_root_execution_id"`
	// ExecutionID is the chain's last run.
	ExecutionID   string `json:"execution_id"`
	Origin        string `json:"origin"`
	Continuations int    `json:"continuations"`
}

// NotificationKey is the key the notification uuid is derived from: one per
// chain root.
func NotificationKey(chainRootExecutionID string) string {
	return "elitea.run-finished.v1\x00" + chainRootExecutionID
}
