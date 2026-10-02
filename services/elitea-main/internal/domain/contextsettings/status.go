package contextsettings

import (
	"encoding/json"
	"math"
)

// AnalyticsUnavailableReason is the named refusal this service returns instead
// of a token count it cannot produce.
//
// WHAT IS AND IS NOT COMPUTED HERE. `current_tokens`,
// `message_groups_in_context` and `summary_count` are not derivable from the
// transcript by this service: they are a RUNTIME record. In pylon they are
// written by the chat runtime as it assembles a turn
// (legacy/plugins/elitea_core/utils/chat_history.py counts tokens with the
// model's own tokenizer, marks each message group `context.included`, and
// increments `summaries_generated` when it summarizes), and every reader —
// `build_context_response` included — only reads them back out of
// `meta.context_analytics`. elitea-main does not run that loop and has no
// tokenizer, so when the record is absent there is no honest number to serve.
//
// The previous implementation invented one: `currentTokens := msgCount * 500`
// (internal/infra/db/repos/conversations.go, GetContextAnalytics). Every
// conversation therefore reported a fabricated budget usage that moved with
// the message count and matched nothing. This flag reports the absence
// instead, and `message_groups_total` — a real COUNT(*) over the transcript —
// is offered alongside so a caller that only wants "how big is this
// conversation" is not left with nothing.
const AnalyticsUnavailableReason = "no context analytics recorded for this conversation: token counts, " +
	"in-context group counts and summary counts are written by the chat runtime that applies the strategy, " +
	"which is not part of this service"

// ConversationState is everything a reader needs from one conversation row to
// answer both "what strategy applies" and "what does the status say": the two
// raw `meta` documents plus the transcript size.
//
// It is one struct, and the repository fills it in one query, because the two
// documents and the count are read together on every one of these routes and a
// second round trip per field buys nothing.
type ConversationState struct {
	// Strategy is the raw `meta.context_strategy`. Nil when the conversation
	// has never had one written — the resolution rule's cue to fall through to
	// the user's defaults.
	Strategy []byte
	// RuntimeContext is the latest response measurement, bound to its current execution.
	RuntimeContext []byte
	// Analytics is the raw `meta.context_analytics`. Nil when the runtime has
	// recorded nothing; see AnalyticsUnavailableReason.
	Analytics []byte
	// MessageGroupsTotal is COUNT(*) over the conversation's message groups.
	MessageGroupsTotal int
}

// Status is pylon's ContextStatus plus two fields this service owes the
// caller: `strategy_name` (which pylon's build_context_response also returns)
// and the availability pair that keeps an absent runtime record from reading
// as a real zero.
type Status struct {
	RuntimeContext         *RuntimeContext `json:"runtime_context,omitempty"`
	BudgetMode             BudgetMode      `json:"budget_mode,omitempty"`
	CurrentTokens          int             `json:"current_tokens"`
	MaxTokens              int             `json:"max_tokens"`
	Utilization            float64         `json:"utilization"`
	MessageGroupsInContext int             `json:"message_groups_in_context"`
	SummaryCount           int             `json:"summary_count"`
	StrategyName           string          `json:"strategy_name"`
	ContextAnalytics       map[string]any  `json:"context_analytics"`

	// MessageGroupsTotal is a real COUNT(*) of the conversation's message
	// groups. It is NOT `message_groups_in_context`: a group can be present in
	// the transcript and excluded from the assembled context.
	MessageGroupsTotal int `json:"message_groups_total"`

	// ContextAnalyticsAvailable is false when the runtime has recorded
	// nothing. The three counters above are then zero because nothing is
	// known, not because they were measured as zero.
	ContextAnalyticsAvailable bool `json:"context_analytics_available"`

	// UnavailableReason names what is missing and why. Empty when available.
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	// Unavailable lists the fields the reason applies to, so a caller can
	// decide field by field without parsing prose.
	Unavailable []string `json:"unavailable,omitempty"`
}

// BuildStatus assembles the status document from the resolved strategy and
// whatever the runtime recorded. `analytics` is the raw
// `meta.context_analytics` value; absent or unparseable means "not recorded".
//
// Utilization is a 0..1 RATIO rounded to four places, which is pylon's
// `build_context_response` (`current_tokens / max_tokens`), not the percentage
// the previous Go implementation returned. apps/elitea-web does not read the
// field — widgets/context-budget/lib/contextStatus.ts derives its percentage
// from the two token counts precisely because the two backends disagreed about
// the scale — so this restores the documented contract without moving the UI.
func BuildStatus(strategy Strategy, analytics []byte, messageGroupsTotal int) Status {
	status := Status{
		BudgetMode:         strategy.BudgetMode,
		MaxTokens:          strategy.MaxContextTokens,
		StrategyName:       strategy.Name,
		MessageGroupsTotal: messageGroupsTotal,
	}

	var decoded map[string]any
	if !isAbsentJSON(analytics) {
		if err := json.Unmarshal(analytics, &decoded); err != nil {
			decoded = nil
		}
	}

	if len(decoded) == 0 {
		status.ContextAnalyticsAvailable = false
		status.UnavailableReason = AnalyticsUnavailableReason
		status.Unavailable = []string{"current_tokens", "utilization", "message_groups_in_context", "summary_count"}
		if strategy.BudgetMode != "" {
			status.MaxTokens = 0
			status.Unavailable = append(status.Unavailable, "max_tokens")
		}
		status.ContextAnalytics = map[string]any{
			"summaries_generated":       0,
			"total_messages_summarized": 0,
			"current_context_tokens":    0,
			"messages_in_context":       0,
			"last_summarization":        nil,
		}
		return status
	}

	status.ContextAnalyticsAvailable = true
	status.ContextAnalytics = decoded
	// Legacy analytics have no admitted provider budget. A preset cannot use
	// a nominal ceiling as its measured input capacity.
	if strategy.BudgetMode != "" {
		status.MaxTokens = 0
		status.Unavailable = []string{"max_tokens", "utilization"}
	}
	if tokens, ok := numeric(decoded["current_context_tokens"]); ok {
		status.CurrentTokens = tokens
	}
	if groups, ok := numeric(decoded["messages_in_context"]); ok {
		status.MessageGroupsInContext = groups
	}
	if summaries, ok := numeric(decoded["summaries_generated"]); ok {
		status.SummaryCount = summaries
	}
	if status.MaxTokens > 0 {
		ratio := float64(status.CurrentTokens) / float64(status.MaxTokens)
		status.Utilization = math.Round(ratio*10000) / 10000
	}
	return status
}
