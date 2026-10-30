package breaker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Override is an agent's own breaker rules, stored on its registry record
// (elitea_lifecycle.agent_registry.circuit_breaker, shared 0213). A nil
// field inherits the tier default. A set field may only tighten it: a lower
// limit than the tier's, or a limit where the tier has the rule off. 0
// turns a rule off, so it is accepted only where the tier has it off too.
type Override struct {
	ErrorRatePercent    *int     `json:"error_rate_percent,omitempty"`
	ErrorRateWindowRuns *int     `json:"error_rate_window_runs,omitempty"`
	SpendPerHourUSD     *float64 `json:"spend_per_hour_usd,omitempty"`
	ToolFailureStreak   *int     `json:"tool_failure_streak,omitempty"`
	RunsPerHour         *int     `json:"runs_per_hour,omitempty"`
}

// Empty reports whether the override sets nothing.
func (o Override) Empty() bool { return o == Override{} }

// ErrInvalidOverride is a malformed override (400).
var ErrInvalidOverride = errors.New("invalid circuit breaker override")

// ParseOverride decodes a stored or posted override. Unknown keys are
// refused, so a misspelt limit is never silently ignored.
func ParseOverride(raw []byte) (Override, error) {
	var out Override
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return out, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return Override{}, fmt.Errorf("%w: %v", ErrInvalidOverride, err)
	}
	if err := out.validate(); err != nil {
		return Override{}, err
	}
	return out, nil
}

func (o Override) validate() error {
	check := func(name string, value *int, lowest, highest int) error {
		if value == nil || *value == 0 {
			return nil
		}
		if *value < lowest || *value > highest {
			return fmt.Errorf("%w: %s must be 0 (off) or from %d to %d", ErrInvalidOverride, name, lowest, highest)
		}
		return nil
	}
	if err := check("error_rate_percent", o.ErrorRatePercent, MinErrorRatePercent, MaxErrorRatePercent); err != nil {
		return err
	}
	if o.ErrorRateWindowRuns != nil && (*o.ErrorRateWindowRuns < MinWindowRuns || *o.ErrorRateWindowRuns > MaxWindowRuns) {
		return fmt.Errorf("%w: error_rate_window_runs must be from %d to %d", ErrInvalidOverride, MinWindowRuns, MaxWindowRuns)
	}
	if err := check("tool_failure_streak", o.ToolFailureStreak, MinToolStreak, MaxToolStreak); err != nil {
		return err
	}
	if err := check("runs_per_hour", o.RunsPerHour, 1, MaxRunsPerHour); err != nil {
		return err
	}
	if o.SpendPerHourUSD != nil {
		usd := *o.SpendPerHourUSD
		if usd < 0 || usd > MaxSpendPerHourUSD || math.IsNaN(usd) {
			return fmt.Errorf("%w: spend_per_hour_usd must be 0 (off) or a USD amount up to %d", ErrInvalidOverride, MaxSpendPerHourUSD)
		}
	}
	return nil
}

// Encode is the stored form: a JSON object, {} for none.
func (o Override) Encode() []byte {
	encoded, err := json.Marshal(o)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// LooseningError refuses an override that is looser than the tier default
// (422 circuit_breaker_override_loosens). Fields names each limit.
type LooseningError struct {
	Fields []string
}

func (e *LooseningError) Error() string {
	return "the circuit breaker override loosens the tier default: " + strings.Join(e.Fields, ", ")
}

// LooseningCode is the typed refusal code of a LooseningError.
const LooseningCode = "circuit_breaker_override_loosens"

// CheckOverride refuses an override that loosens the tier's rules. Only the
// fields the override sets are judged.
func CheckOverride(tier Rules, o Override) error {
	var loose []string
	looserInt := func(name string, value *int, tierValue int) {
		if value == nil {
			return
		}
		if tierValue != 0 && (*value == 0 || *value > tierValue) {
			loose = append(loose, name)
		}
	}
	looserInt("error_rate_percent", o.ErrorRatePercent, tier.ErrorRatePercent)
	if o.ErrorRateWindowRuns != nil && *o.ErrorRateWindowRuns > tier.ErrorRateWindowRuns {
		loose = append(loose, "error_rate_window_runs")
	}
	looserInt("tool_failure_streak", o.ToolFailureStreak, tier.ToolFailureStreak)
	looserInt("runs_per_hour", o.RunsPerHour, tier.RunsPerHour)
	if o.SpendPerHourUSD != nil && tier.SpendPerHourUSD != 0 &&
		(*o.SpendPerHourUSD == 0 || *o.SpendPerHourUSD > tier.SpendPerHourUSD) {
		loose = append(loose, "spend_per_hour_usd")
	}
	if len(loose) > 0 {
		return &LooseningError{Fields: loose}
	}
	return nil
}
