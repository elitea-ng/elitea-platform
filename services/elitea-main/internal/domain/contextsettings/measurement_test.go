package contextsettings

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const validMeasurement = `{"version":1,"phase":"compacting","budget_mode":"balanced","total_tokens":272000,"usable_input_tokens":205280,"reserved_output_tokens":64000,"safety_margin_tokens":2720,"estimated_input_tokens":190000,"compaction_trigger_tokens":184752,"compaction_target_tokens":143696}`

func TestProviderMeasurementUsesCombinedWindowWithoutDoubleReservation(t *testing.T) {
	for _, usage := range []string{`{"input_tokens":100000,"output_tokens":20000}`, `{"input_tokens":0,"output_tokens":0}`} {
		base := strings.Replace(validMeasurement, `"compacting"`, `"measured"`, 1)
		raw := strings.TrimSuffix(base, "}") + `,"provider_usage":` + usage + `}`
		measurement, err := DecodeMeasurement([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		record, err := json.Marshal(RuntimeContext{Measurement: measurement, ExecutionID: "e", Generation: 1, ExecutionGeneration: "g", ResponseMessageID: "r", RecordedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		status := WithRuntimeContext(BuildStatus(DefaultStrategy(), nil, 0), record)
		want := int(*measurement.ProviderUsage.InputTokens + *measurement.ProviderUsage.OutputTokens)
		if status.CurrentTokens != want || status.MaxTokens != 269280 {
			t.Fatalf("provider occupancy: %+v", status)
		}
		if measurement.EstimatedInputTokens != 190000 {
			t.Fatal("provider usage replaced the admission estimate")
		}
	}
}

func TestProviderMeasurementRejectsIncompleteOrUnscopedCounters(t *testing.T) {
	base := strings.Replace(validMeasurement, `"compacting"`, `"measured"`, 1)
	for _, usage := range []string{`{}`, `{"input_tokens":1}`, `{"input_tokens":null,"output_tokens":2}`, `{"input_tokens":-1,"output_tokens":2}`, `{"input_tokens":2147483647,"output_tokens":2}`, `{"input_tokens":1,"output_tokens":2,"prompt":"private"}`} {
		if _, err := DecodeMeasurement([]byte(strings.TrimSuffix(base, "}") + `,"provider_usage":` + usage + `}`)); err == nil {
			t.Fatalf("accepted invalid counters: %s", usage)
		}
	}
	if _, err := DecodeMeasurement([]byte(strings.TrimSuffix(validMeasurement, "}") + `,"provider_usage":{"input_tokens":1,"output_tokens":2}}`)); err == nil {
		t.Fatal("compacting estimate accepted provider counts")
	}
}

func TestMeasurementPreservesOptionalAutoOutput(t *testing.T) {
	for _, suffix := range []string{"", `,"auto_output":false`, `,"auto_output":true`} {
		raw := strings.TrimSuffix(validMeasurement, "}") + suffix + "}"
		measurement, err := DecodeMeasurement([]byte(raw))
		if err != nil || measurement.AutoOutput != strings.Contains(suffix, "true") {
			t.Fatalf("Auto output mode: %v, %v", measurement.AutoOutput, err)
		}
	}
	if _, err := DecodeMeasurement([]byte(strings.TrimSuffix(validMeasurement, "}") + `,"auto_output":"true"}`)); err == nil {
		t.Fatal("accepted an invalid Auto marker")
	}
}

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

func TestMeasurementAcceptsCompactTargetAndLegacyReplay(t *testing.T) {
	for _, target := range []string{"30792", "143696"} {
		raw := strings.Replace(validMeasurement, `"compaction_target_tokens":143696`, `"compaction_target_tokens":`+target, 1)
		if _, err := DecodeMeasurement([]byte(raw)); err != nil {
			t.Fatalf("target %s: %v", target, err)
		}
	}
	raw := strings.Replace(validMeasurement, `"compaction_target_tokens":143696`, `"compaction_target_tokens":30793`, 1)
	if _, err := DecodeMeasurement([]byte(raw)); err == nil {
		t.Fatal("accepted an unrecognized target")
	}
}
