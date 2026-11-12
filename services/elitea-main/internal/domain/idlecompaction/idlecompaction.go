// Package idlecompaction is the cloud rule of idle compaction per
// (conversation, agent) (ADR-0050 decision 9b2, issue 1610): when an
// agent's settled turn arms a check, when the check is due, and whether it
// still compacts when it fires. It mirrors the desktop rule
// (apps/elitea-desktop/src-tauri/src/d0/idle.rs `decide`): the same
// minimum, the same cache delay, the same skips.
//
// A check is keyed by (project, conversation, participant). Only a new
// turn of the same participant cancels it: another agent's answer or a
// person's message does not, because the agent's next turn reconciles its
// session after the compaction marker anyway.
package idlecompaction

import "time"

// MinMessageTokens is the conversation size below which an idle summary
// costs more than the next turn's cache miss would (desktop
// `MIN_MESSAGE_TOKENS`).
const MinMessageTokens = 60_000

// CacheTTL is the provider prompt-cache lifetime the cloud breakpoints ask
// for: Anthropic's default five minutes.
const CacheTTL = 5 * time.Minute

// Delay is how long after the last model request START the check fires:
// one minute inside the cache, so the summary still reads it.
const Delay = 4 * time.Minute

// Turn is what a settled turn of one agent leaves for its check.
type Turn struct {
	// Succeeded: the turn settled with an answer (not failed, stopped or
	// paused for a person).
	Succeeded bool
	// PromptCache: the agent's frozen model is a paid provider with a
	// prompt cache.
	PromptCache bool
	// MessageTokens is the last request's conversation tokens (the context
	// meter's); 0 when not measured.
	MessageTokens uint64
	// RequestStarted is when the turn's last model request started.
	RequestStarted time.Time
}

// SkipCode is a stable reason a check is not armed or does not compact.
type SkipCode string

const (
	SkipNone          SkipCode = ""
	SkipNotSucceeded  SkipCode = "turn_not_answered"
	SkipNoPromptCache SkipCode = "no_paid_prompt_cache"
	SkipTooSmall      SkipCode = "context_too_small"
	SkipCacheExpired  SkipCode = "cache_expired"
	SkipSuperseded    SkipCode = "superseded_by_agent_turn"
	SkipAgentBusy     SkipCode = "agent_turn_running"
	SkipNotMeasured   SkipCode = "request_not_measured"
)

// Arm decides whether a settled turn schedules a check and when it is due.
func Arm(turn Turn) (time.Time, SkipCode) {
	switch {
	case !turn.Succeeded:
		return time.Time{}, SkipNotSucceeded
	case !turn.PromptCache:
		return time.Time{}, SkipNoPromptCache
	case turn.RequestStarted.IsZero():
		return time.Time{}, SkipNotMeasured
	case turn.MessageTokens < MinMessageTokens:
		return time.Time{}, SkipTooSmall
	}
	return turn.RequestStarted.Add(Delay), SkipNone
}

// Check is a scheduled check as it fires.
type Check struct {
	RequestStarted time.Time
	// SourceExecution is the execution the check was armed from;
	// LatestExecution is the participant's newest execution in the
	// conversation now.
	SourceExecution string
	LatestExecution string
	// AgentBusy: a run that can hold the agent's worker session runs or
	// waits now; the check waits, it is not cancelled. With a per-participant
	// session thread (issue 1869) that is the agent's own run only; with the
	// conversation's thread it is any agent run of the conversation.
	AgentBusy bool
}

// Decide is whether a due check compacts now (`now` the wall clock). A peer
// agent's or a person's activity is not an input: it never cancels.
func Decide(check Check, now time.Time) SkipCode {
	switch {
	case check.LatestExecution != check.SourceExecution:
		return SkipSuperseded
	case check.AgentBusy:
		return SkipAgentBusy
	case now.Before(check.RequestStarted) || !now.Before(check.RequestStarted.Add(CacheTTL)):
		// A clock set back reads as "before the request": its cache is
		// unknown.
		return SkipCacheExpired
	}
	return SkipNone
}
