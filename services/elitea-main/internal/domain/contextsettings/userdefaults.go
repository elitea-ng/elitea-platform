package contextsettings

import (
	"encoding/json"
)

// ContextManagement is the user-level `default_context_management` block —
// pylon's social/models/pd/users.py ContextManagementModel, plus
// `enable_context_editing`, which that model omits but
// `set_context_strategy` reads out of the same dict
// (legacy/plugins/elitea_core/utils/context_analytics.py) and which
// apps/elitea-web's Memory page has always sent.
//
// Every field is a pointer: this is a DEFAULTS document, and "the user has not
// said" is a different answer from "the user said false / said zero". A
// non-pointer bool here would silently turn an untouched account into an
// explicit `enabled: false`.
type ContextManagement struct {
	Enabled                *bool       `json:"enabled,omitempty"`
	BudgetMode             *BudgetMode `json:"budget_mode,omitempty"`
	MaxContextTokens       *int        `json:"max_context_tokens,omitempty"`
	PreserveRecentMessages *int        `json:"preserve_recent_messages,omitempty"`
	EnableContextEditing   *bool       `json:"enable_context_editing,omitempty"`
}

// Summarization is the user-level `default_summarization` block — pylon's
// SummarizationModel, field for field.
type Summarization struct {
	EnableSummarization   *bool    `json:"enable_summarization,omitempty"`
	SummaryInstructions   *string  `json:"summary_instructions,omitempty"`
	SummaryModelName      *string  `json:"summary_model_name,omitempty"`
	SummaryModelProjectID *int     `json:"summary_model_project_id,omitempty"`
	SummaryTriggerRatio   *float64 `json:"summary_trigger_ratio,omitempty"`
	MinMessagesForSummary *int     `json:"min_messages_for_summary,omitempty"`
	TargetSummaryTokens   *int     `json:"target_summary_tokens,omitempty"`
}

// UserDefaults is the pair as it hangs off the author record. Either half may
// be absent; absent means "no opinion", not "off".
type UserDefaults struct {
	ContextManagement *ContextManagement `json:"default_context_management,omitempty"`
	Summarization     *Summarization     `json:"default_summarization,omitempty"`
}

// DecodeContextManagement parses a stored or submitted
// `default_context_management` blob. A nil/empty/`null` input decodes to nil,
// which is the "no opinion" answer, not an error.
func DecodeContextManagement(raw []byte) (*ContextManagement, *FieldError) {
	raw = withoutClearedFields(raw)
	if isAbsentJSON(raw) {
		return nil, nil
	}
	var decoded ContextManagement
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, prefixField("default_context_management", decodeFieldError(err))
	}
	if fieldErr := decoded.Validate(); fieldErr != nil {
		return nil, fieldErr
	}
	return &decoded, nil
}

// DecodeSummarization parses a stored or submitted `default_summarization`
// blob under the same absence rule as DecodeContextManagement.
func DecodeSummarization(raw []byte) (*Summarization, *FieldError) {
	raw = withoutClearedFields(raw)
	if isAbsentJSON(raw) {
		return nil, nil
	}
	var decoded Summarization
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, prefixField("default_summarization", decodeFieldError(err))
	}
	if fieldErr := decoded.Validate(); fieldErr != nil {
		return nil, fieldErr
	}
	return &decoded, nil
}

// Validate applies the contract's ranges to whichever fields are present. A
// field the user never set cannot be out of range.
func (c *ContextManagement) Validate() *FieldError {
	if c == nil {
		return nil
	}
	if c.BudgetMode != nil {
		if err := validateBudgetMode(*c.BudgetMode); err != nil {
			return prefixField("default_context_management", err)
		}
	}
	if (c.BudgetMode == nil || *c.BudgetMode == "") && c.MaxContextTokens != nil && *c.MaxContextTokens < MinMaxContextTokens {
		return fieldErrorf("default_context_management.max_context_tokens",
			"max_context_tokens must be at least %d", MinMaxContextTokens)
	}
	if c.PreserveRecentMessages != nil &&
		(*c.PreserveRecentMessages < MinPreserveRecentMessages || *c.PreserveRecentMessages > MaxPreserveRecentMessages) {
		return fieldErrorf("default_context_management.preserve_recent_messages",
			"preserve_recent_messages must be between %d and %d",
			MinPreserveRecentMessages, MaxPreserveRecentMessages)
	}
	return nil
}

// Validate range-checks the summarization defaults.
//
// `target_summary_tokens` becomes the strategy's
// `summary_llm_settings.max_tokens` (set_context_strategy), so it takes that
// field's floor. Model capacity and output limits are validated at admission,
// after the authorized summary model has been selected.
func (s *Summarization) Validate() *FieldError {
	if s == nil {
		return nil
	}
	if s.TargetSummaryTokens != nil && *s.TargetSummaryTokens < MinSummaryMaxTokens {
		return fieldErrorf("default_summarization.target_summary_tokens",
			"target_summary_tokens must be at least %d", MinSummaryMaxTokens)
	}
	if s.SummaryTriggerRatio != nil && (*s.SummaryTriggerRatio <= 0 || *s.SummaryTriggerRatio > 1) {
		return fieldErrorf("default_summarization.summary_trigger_ratio",
			"summary_trigger_ratio must be greater than 0 and at most 1")
	}
	if s.MinMessagesForSummary != nil && *s.MinMessagesForSummary < 1 {
		return fieldErrorf("default_summarization.min_messages_for_summary",
			"min_messages_for_summary must be at least 1")
	}
	return nil
}

// Validate checks both halves.
func (u UserDefaults) Validate() *FieldError {
	if fieldErr := u.ContextManagement.Validate(); fieldErr != nil {
		return fieldErr
	}
	return u.Summarization.Validate()
}

// Resolve implements the contract's resolution rule, outermost first:
//
//	per-conversation strategy  >  the user's defaults  >  the constants
//
// `stored` is the raw `meta.context_strategy` document, which may be nil (no
// strategy yet) — it is unmarshalled OVER the defaults-resolved value, so a
// key the stored document does not carry keeps the user's answer rather than
// snapping back to a constant. That is the whole reason this is a raw
// unmarshal and not a struct merge: strategies written by pylon, and by
// earlier versions of this route, do not all carry the same key set.
//
// The user-defaults half is pylon's `set_context_strategy` mapping, including
// its flattening of three summarization fields into `summary_llm_settings`.
func Resolve(stored []byte, defaults UserDefaults) Strategy {
	strategy := DefaultStrategy()

	if cm := defaults.ContextManagement; cm != nil {
		if cm.Enabled != nil {
			strategy.Enabled = *cm.Enabled
		}
		if cm.MaxContextTokens != nil {
			strategy.MaxContextTokens = *cm.MaxContextTokens
		}
		if cm.BudgetMode != nil {
			strategy.BudgetMode = *cm.BudgetMode
			if strategy.BudgetMode != "" {
				strategy.MaxContextTokens = 0
			}
		}
		if cm.PreserveRecentMessages != nil {
			strategy.PreserveRecentMessages = *cm.PreserveRecentMessages
		}
		if cm.EnableContextEditing != nil {
			strategy.EnableContextEditing = *cm.EnableContextEditing
		}
	}

	if sm := defaults.Summarization; sm != nil {
		if sm.EnableSummarization != nil {
			strategy.EnableSummarization = *sm.EnableSummarization
		}
		if sm.SummaryInstructions != nil {
			strategy.SummaryInstructions = *sm.SummaryInstructions
		}
		llm := map[string]any{}
		if sm.SummaryModelName != nil && *sm.SummaryModelName != "" {
			llm["model_name"] = *sm.SummaryModelName
			if sm.SummaryModelProjectID != nil {
				llm["model_project_id"] = *sm.SummaryModelProjectID
			}
		}
		if sm.TargetSummaryTokens != nil {
			llm["max_tokens"] = *sm.TargetSummaryTokens
		}
		if len(llm) > 0 {
			strategy.SummaryLLMSettings = llm
		}
	}

	if !isAbsentJSON(stored) {
		var fields map[string]json.RawMessage
		resolved := strategy
		// Read malformed legacy documents without applying a partial decode.
		if json.Unmarshal(stored, &fields) == nil && json.Unmarshal(stored, &resolved) == nil {
			strategy = resolved
		}
	}
	if strategy.BudgetMode == "" {
		strategy.BudgetMode = BudgetBalanced
	}
	strategy.normalizeSummaryLLMSettings()

	return strategy
}

// withoutClearedFields drops keys whose value is JSON null or the empty
// string, so a CLEARED form field reads as "no opinion" rather than as a
// type error.
//
// WHY THIS IS NOT LAXITY. These blocks are a defaults document written by one
// form, and that form holds a numeric input the user is halfway through
// editing as `”` (apps/elitea-web .../context-budget/validation.ts,
// `handleConvertToNumberChange`). "The user emptied the box" and "the user
// never set this" are the same state for a DEFAULT, and both are already
// spelled by the field's absence. It applies only here: the per-conversation
// StrategyUpdate is an API contract rather than a form, and stays strict.
func withoutClearedFields(raw []byte) []byte {
	if isAbsentJSON(raw) {
		return raw
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		// Not an object — leave it alone and let the typed decode report it.
		return raw
	}
	cleared := false
	for key, value := range fields {
		if value == nil || value == "" {
			delete(fields, key)
			cleared = true
		}
	}
	if !cleared {
		return raw
	}
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return rewritten
}

func isAbsentJSON(raw []byte) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func prefixField(prefix string, fieldErr *FieldError) *FieldError {
	if fieldErr == nil {
		return nil
	}
	if fieldErr.Field == "" {
		return &FieldError{Field: prefix, Message: prefix + " is not a valid object"}
	}
	return &FieldError{Field: prefix + "." + fieldErr.Field, Message: fieldErr.Message}
}
