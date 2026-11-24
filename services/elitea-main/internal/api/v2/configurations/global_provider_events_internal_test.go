package configurations

import (
	"strings"
	"testing"
)

// A data key the event cannot carry by name is recorded as one other field,
// so the audit never refuses a provider write the provider rules admit.
func TestGlobalProviderFieldsAreBounded(t *testing.T) {
	data := map[string]any{"api_base": "x", strings.Repeat("k", 300): 1, "with space": 1, "eyJa.eyJb.c": 1}
	for i := 0; i < 200; i++ {
		data["key_"+strings.Repeat("x", i%60)+string(rune('a'+i%26))+strings.Repeat("y", i/26)] = i
	}
	fields := globalProviderFields(map[string]any{"data": data, "type": "open_ai"}, "")
	named := 0
	for _, field := range fields {
		if field == globalProviderOtherField || field == "type" {
			continue
		}
		named++
		if !strings.HasPrefix(field, "data.") || !globalProviderFieldName.MatchString(strings.TrimPrefix(field, "data.")) {
			t.Errorf("field %q is not a plain data key", field)
		}
	}
	if named > maxGlobalProviderFieldNames {
		t.Errorf("%d named fields, want at most %d", named, maxGlobalProviderFieldNames)
	}
	if !strings.Contains(strings.Join(fields, ","), globalProviderOtherField) {
		t.Errorf("fields %v do not record the keys it could not name", fields)
	}
}
