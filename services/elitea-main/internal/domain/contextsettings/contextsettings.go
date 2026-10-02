// Package contextsettings holds the conversation context-management contract:
// the per-conversation strategy, the user-level defaults it falls back to, and
// the status document a reader serves.
//
// WHY A DOMAIN PACKAGE. The same shape is written in two unrelated places —
// `PUT /social/author` (the user's defaults, centry.social_users) and
// `PUT /elitea_core/context_strategy/...` (one conversation, its
// `meta.context_strategy`) — and read by a third (the context status). pylon
// had the same duplication and kept the two halves agreed only by convention:
// social/models/pd/users.py declared the defaults, elitea_core/models/pd/
// context.py declared the strategy, and elitea_core/utils/context_analytics.py
// `set_context_strategy` mapped one onto the other by hand. Defaults spelled
// twice are defaults that drift, so every constant, range and mapping rule
// lives here once.
//
// Legacy numeric context settings do not define the new combined window.
// New compaction uses Balanced or Full. Stored legacy records stay unchanged.
// Model catalogue limits remain authoritative at execution admission.
// Runtime projection removes UI-only fields and separates summary-model selection.
// Main activation and deployed browser acceptance remain separate gates.
package contextsettings

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Shared strategy defaults. Zero legacy capacity means no numeric override.
const (
	DefaultStrategyName           = "default"
	DefaultEnabled                = true
	DefaultEnableSummarization    = true
	DefaultEnableContextEditing   = false
	DefaultMaxContextTokens       = 0 // Presets resolve against the execution model.
	DefaultPreserveRecentMessages = 5
	DefaultPreserveSystemMessages = true
	DefaultSummaryInstructions    = "Generate a concise summary of the following conversation messages"
)

// The frozen contract's ranges. pylon: the `Field(..., ge=, le=)` constraints
// on ContextStrategy/ContextStrategyUpdate, plus the model validator that
// bounds the summary model's own max_tokens.
const (
	MinMaxContextTokens       = 1000
	MinPreserveRecentMessages = 1
	MaxPreserveRecentMessages = 99
	MinSummaryMaxTokens       = 100
)

// FieldError names the offending field, so a handler can answer with this
// API's `{"error": ..., "field": ...}` validation shape (the same one
// internal/api/v2/configurations/mutation.go writes).
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Message }

func fieldErrorf(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Strategy is one conversation's resolved context-management configuration —
// the document stored at `chat_conversations.meta.context_strategy`.
//
// Every field is non-pointer on purpose: this is the RESOLVED value, the
// answer to "what applies to this conversation", so there is no such thing as
// an absent field here. Absence lives in StrategyUpdate and in the user
// defaults, both of which resolve INTO this.
type Strategy struct {
	Name                   string         `json:"name"`
	Enabled                bool           `json:"enabled"`
	EnableSummarization    bool           `json:"enable_summarization"`
	EnableContextEditing   bool           `json:"enable_context_editing"`
	MaxContextTokens       int            `json:"max_context_tokens"`
	BudgetMode             BudgetMode     `json:"budget_mode,omitempty"`
	PreserveRecentMessages int            `json:"preserve_recent_messages"`
	PreserveSystemMessages bool           `json:"preserve_system_messages"`
	SummaryInstructions    string         `json:"summary_instructions"`
	SummaryLLMSettings     map[string]any `json:"summary_llm_settings"`
}

// DefaultStrategy is the contract's constants — the last fallback of the
// resolution rule, used when neither the conversation nor the user says
// anything.
func DefaultStrategy() Strategy {
	return Strategy{
		Name:                   DefaultStrategyName,
		Enabled:                DefaultEnabled,
		EnableSummarization:    DefaultEnableSummarization,
		EnableContextEditing:   DefaultEnableContextEditing,
		MaxContextTokens:       DefaultMaxContextTokens,
		BudgetMode:             BudgetBalanced,
		PreserveRecentMessages: DefaultPreserveRecentMessages,
		PreserveSystemMessages: DefaultPreserveSystemMessages,
		SummaryInstructions:    DefaultSummaryInstructions,
		SummaryLLMSettings:     nil,
	}
}

// normalizeSummaryLLMSettings gives an absent selection one representation.
// Main resolves a nonempty selection through the authorized catalogue.
func (s *Strategy) normalizeSummaryLLMSettings() {
	if len(s.SummaryLLMSettings) == 0 {
		s.SummaryLLMSettings = nil
	}
}

// Validate range-checks a resolved strategy. It is the same check the update
// path runs, applied to the merged result, so a stored document that predates
// a range can never be served as if it were valid.
func (s Strategy) Validate() *FieldError {
	if err := validateBudgetMode(s.BudgetMode); err != nil {
		return err
	}
	if s.MaxContextTokens != 0 && s.MaxContextTokens < MinMaxContextTokens {
		return fieldErrorf("max_context_tokens", "max_context_tokens must be at least %d", MinMaxContextTokens)
	}
	if s.PreserveRecentMessages < MinPreserveRecentMessages || s.PreserveRecentMessages > MaxPreserveRecentMessages {
		return fieldErrorf("preserve_recent_messages",
			"preserve_recent_messages must be between %d and %d",
			MinPreserveRecentMessages, MaxPreserveRecentMessages)
	}
	return validateSummaryMaxTokens(s.SummaryLLMSettings)
}

// validateSummaryMaxTokens checks the output-cap type and minimum.
// Catalogue admission supplies the actual model limits.
func validateSummaryMaxTokens(settings map[string]any) *FieldError {
	if settings == nil {
		return nil
	}
	raw, present := settings["max_tokens"]
	if !present || raw == nil {
		return nil
	}
	maxTokens, ok := numeric(raw)
	if !ok {
		return fieldErrorf("summary_llm_settings.max_tokens", "summary_llm_settings.max_tokens must be a whole number")
	}
	if maxTokens < MinSummaryMaxTokens {
		return fieldErrorf("summary_llm_settings.max_tokens",
			"summary max tokens (%d) must be at least %d", maxTokens, MinSummaryMaxTokens)
	}

	return nil
}

// numeric reads a JSON number out of a decoded `any`, refusing a fractional
// value where the contract says integer.
func numeric(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if typed != math.Trunc(typed) {
			return 0, false
		}
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	}
	return 0, false
}

// StrategyUpdate is the per-conversation PUT body: pylon's
// ContextStrategyUpdate. Every field is optional; an absent field keeps
// whatever the resolution rule already produced.
type StrategyUpdate struct {
	Name                   *string        `json:"name,omitempty"`
	Enabled                *bool          `json:"enabled,omitempty"`
	EnableSummarization    *bool          `json:"enable_summarization,omitempty"`
	EnableContextEditing   *bool          `json:"enable_context_editing,omitempty"`
	MaxContextTokens       *int           `json:"max_context_tokens,omitempty"`
	BudgetMode             *BudgetMode    `json:"budget_mode,omitempty"`
	PreserveRecentMessages *int           `json:"preserve_recent_messages,omitempty"`
	PreserveSystemMessages *bool          `json:"preserve_system_messages,omitempty"`
	SummaryInstructions    *string        `json:"summary_instructions,omitempty"`
	SummaryLLMSettings     map[string]any `json:"summary_llm_settings,omitempty"`
}

// DecodeStrategyUpdate parses a PUT body, reporting a wrong-typed field by
// name rather than as an opaque "invalid request body".
func DecodeStrategyUpdate(raw []byte) (StrategyUpdate, *FieldError) {
	var update StrategyUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return StrategyUpdate{}, decodeFieldError(err)
	}
	return update, nil
}

func decodeFieldError(err error) *FieldError {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fieldErrorf(typeErr.Field, "%s must be of type %s", typeErr.Field, typeErr.Type.String())
	}
	return &FieldError{Field: "", Message: "invalid request body"}
}

// Apply lays the update over a resolved strategy and validates the result.
// Model-specific limits are resolved at execution admission.
func (s Strategy) Apply(update StrategyUpdate) (Strategy, *FieldError) {
	merged := s
	if update.Name != nil {
		merged.Name = *update.Name
	}
	if update.Enabled != nil {
		merged.Enabled = *update.Enabled
	}
	if update.EnableSummarization != nil {
		merged.EnableSummarization = *update.EnableSummarization
	}
	if update.EnableContextEditing != nil {
		merged.EnableContextEditing = *update.EnableContextEditing
	}
	if update.MaxContextTokens != nil {
		merged.MaxContextTokens = *update.MaxContextTokens
	}
	if update.BudgetMode != nil {
		merged.BudgetMode = *update.BudgetMode
		if merged.BudgetMode != "" {
			merged.MaxContextTokens = 0
		}
	}
	if update.PreserveRecentMessages != nil {
		merged.PreserveRecentMessages = *update.PreserveRecentMessages
	}
	if update.PreserveSystemMessages != nil {
		merged.PreserveSystemMessages = *update.PreserveSystemMessages
	}
	if update.SummaryInstructions != nil {
		merged.SummaryInstructions = *update.SummaryInstructions
	}
	if update.SummaryLLMSettings != nil {
		merged.SummaryLLMSettings = update.SummaryLLMSettings
	}
	if merged.BudgetMode == "" {
		merged.BudgetMode = BudgetBalanced
	}
	merged.normalizeSummaryLLMSettings()
	if fieldErr := merged.Validate(); fieldErr != nil {
		return Strategy{}, fieldErr
	}
	return merged, nil
}
