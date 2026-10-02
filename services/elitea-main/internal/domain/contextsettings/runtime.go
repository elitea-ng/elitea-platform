package contextsettings

import "maps"

// BudgetMode selects a combined window. Empty legacy selections resolve to Balanced.
type BudgetMode string

const (
	BudgetBalanced BudgetMode = "balanced"
	BudgetFull     BudgetMode = "full"
)

func validateBudgetMode(mode BudgetMode) *FieldError {
	switch mode {
	case "", BudgetBalanced, BudgetFull:
		return nil
	default:
		return fieldErrorf("budget_mode", "budget_mode must be balanced or full")
	}
}

// RuntimeSettings is a projection for the worker, not a stored settings document.
// Model limits and summary authorization belong to execution admission.
type RuntimeSettings struct {
	Enabled                bool       `json:"enabled"`
	BudgetMode             BudgetMode `json:"budget_mode,omitempty"`
	EnableSummarization    bool       `json:"enable_summarization"`
	EnableContextEditing   bool       `json:"enable_context_editing"`
	PreserveRecentMessages int        `json:"preserve_recent_messages"`
	PreserveSystemMessages bool       `json:"preserve_system_messages"`
	SummaryInstructions    string     `json:"summary_instructions"`
}

// Runtime separates authored selection from runtime authority. Disabled policies
// do not request a summary-model binding. Complete provider requests retain limits.
func (s Strategy) Runtime() (RuntimeSettings, map[string]any, *FieldError) {
	if err := s.Validate(); err != nil {
		return RuntimeSettings{}, nil, err
	}
	settings := RuntimeSettings{
		Enabled: s.Enabled, BudgetMode: s.BudgetMode,
		EnableSummarization: s.EnableSummarization, EnableContextEditing: s.EnableContextEditing,
		PreserveRecentMessages: s.PreserveRecentMessages, PreserveSystemMessages: s.PreserveSystemMessages,
		SummaryInstructions: s.SummaryInstructions,
	}
	if settings.BudgetMode == "" {
		settings.BudgetMode = BudgetBalanced
	}
	if !s.Enabled || !s.EnableSummarization {
		return settings, nil, nil
	}
	return settings, maps.Clone(s.SummaryLLMSettings), nil
}
