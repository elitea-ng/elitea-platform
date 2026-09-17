package contextsettings

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const validMeasurement = `{"version":1,"phase":"compacting","budget_mode":"balanced","total_tokens":272000,"usable_input_tokens":205280,"reserved_output_tokens":64000,"safety_margin_tokens":2720,"estimated_input_tokens":190000,"compaction_trigger_tokens":184752,"compaction_target_tokens":143696}`

func TestMeasurementRejectsContentAndImpossibleBudgets(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(validMeasurement, `"version":1`, `"version":1,"prompt":"secret"`, 1),
		strings.Replace(validMeasurement, `"usable_input_tokens":205280`, `"usable_input_tokens":272000`, 1),
		strings.Replace(validMeasurement, `"compaction_trigger_tokens":184752`, `"compaction_trigger_tokens":184753`, 1),
		strings.Replace(validMeasurement, `"phase":"compacting"`, `"phase":"failed"`, 1),
		validMeasurement + `{}`,
	} {
		if _, err := DecodeMeasurement([]byte(raw)); err == nil {
			t.Fatal("accepted invalid context measurement")
		}
	}
	if _, err := DecodeMeasurement([]byte(validMeasurement)); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeContextUsesAdmittedCapacityWithoutInventingOtherCounters(t *testing.T) {
	measurement, err := DecodeMeasurement([]byte(validMeasurement))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(RuntimeContext{
		Measurement: measurement, ExecutionID: "execution", Generation: 1,
		ExecutionGeneration: "client", ResponseMessageID: "response", RecordedAt: time.Now(), Active: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	strategy := DefaultStrategy()
	strategy.BudgetMode = "full"
	status := WithRuntimeContext(BuildStatus(strategy, nil, 8), raw)
	if !status.ContextAnalyticsAvailable || status.BudgetMode != "balanced" || status.MaxTokens != 205280 ||
		status.CurrentTokens != 190000 || status.RuntimeContext.Active || status.RuntimeContext.Measurement.Phase != "compacting" ||
		len(status.Unavailable) != 2 || status.MessageGroupsTotal != 8 {
		t.Fatalf("unexpected status: %+v", status)
	}
	if WithRuntimeContext(BuildStatus(strategy, nil, 8), nil).RuntimeContext != nil {
		t.Fatal("invented a runtime measurement")
	}
}
