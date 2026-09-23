package contextsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Measurement is the content-free estimate emitted by the worker after it
// prepares a provider request. It is not cumulative billing or a tokenizer count.
type Measurement struct {
	Version                 uint8  `json:"version"`
	Phase                   string `json:"phase"`
	BudgetMode              string `json:"budget_mode"`
	TotalTokens             uint32 `json:"total_tokens"`
	UsableInputTokens       uint32 `json:"usable_input_tokens"`
	ReservedOutputTokens    uint32 `json:"reserved_output_tokens"`
	SafetyMarginTokens      uint32 `json:"safety_margin_tokens"`
	EstimatedInputTokens    uint64 `json:"estimated_input_tokens"`
	CompactionTriggerTokens uint64 `json:"compaction_trigger_tokens"`
	CompactionTargetTokens  uint64 `json:"compaction_target_tokens"`
}

// DecodeMeasurement enforces the same versioned arithmetic as the Rust worker.
// Only the explicit fields can enter the public read model; model content cannot.
func DecodeMeasurement(raw []byte) (Measurement, error) {
	var value Measurement
	if len(raw) == 0 || len(raw) > 1024 {
		return value, errors.New("invalid context measurement size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Measurement{}, errors.New("invalid context measurement document")
	}
	if decoder.Decode(new(any)) != io.EOF || !value.Valid() {
		return Measurement{}, errors.New("invalid context measurement budget")
	}
	return value, nil
}

func (m Measurement) Valid() bool {
	input := uint64(m.UsableInputTokens)
	return m.Version == 1 &&
		(m.Phase == "measured" || m.Phase == "compacting" || m.Phase == "compacted") &&
		(m.BudgetMode == "balanced" || m.BudgetMode == "full" || m.BudgetMode == "legacy") &&
		input > 0 && m.ReservedOutputTokens > 0 && m.SafetyMarginTokens > 0 &&
		input+uint64(m.ReservedOutputTokens)+uint64(m.SafetyMarginTokens) <= uint64(m.TotalTokens) &&
		m.CompactionTriggerTokens == (input*90+99)/100 &&
		(m.CompactionTargetTokens == input*15/100 || m.CompactionTargetTokens == input*70/100) &&
		m.EstimatedInputTokens <= 9_007_199_254_740_991 &&
		(m.Phase == "compacting" || m.EstimatedInputTokens <= input)
}

// RuntimeContext is a presentation record, never a resumable checkpoint. Active
// comes from the response's lifecycle, not from a worker assertion.
type RuntimeContext struct {
	Measurement         Measurement `json:"measurement"`
	ExecutionID         string      `json:"execution_id"`
	Generation          uint64      `json:"generation"`
	ExecutionGeneration string      `json:"execution_generation"`
	ResponseMessageID   string      `json:"response_message_id"`
	RecordedAt          time.Time   `json:"recorded_at"`
	Active              bool        `json:"active"`
}

// WithRuntimeContext overlays the latest response's measurement. A new response
// without a measurement must not inherit an older response's occupancy.
func WithRuntimeContext(status Status, raw []byte) Status {
	var runtime RuntimeContext
	if len(raw) == 0 || len(raw) > 4096 || json.Unmarshal(raw, &runtime) != nil ||
		!runtime.Measurement.Valid() || runtime.ExecutionID == "" || runtime.Generation == 0 ||
		runtime.ExecutionGeneration == "" || runtime.ResponseMessageID == "" || runtime.RecordedAt.IsZero() {
		return status
	}
	status.RuntimeContext = &runtime
	status.CurrentTokens = int(runtime.Measurement.EstimatedInputTokens)
	status.MaxTokens = int(runtime.Measurement.UsableInputTokens)
	status.Utilization = float64(status.CurrentTokens) / float64(status.MaxTokens)
	status.ContextAnalyticsAvailable = true
	status.UnavailableReason = "Message and summary counts are not reported by this runtime."
	status.Unavailable = []string{"message_groups_in_context", "summary_count"}
	// The admitted policy wins over settings edited while this run was active.
	if runtime.Measurement.BudgetMode != "legacy" {
		status.BudgetMode = BudgetMode(runtime.Measurement.BudgetMode)
	} else {
		status.BudgetMode = ""
	}
	return status
}
