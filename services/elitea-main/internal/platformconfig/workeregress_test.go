package platformconfig

import (
	"reflect"
	"testing"
)

func TestWorkerEgressSettingsFromValues(t *testing.T) {
	for _, test := range []struct {
		name   string
		values Values
		want   WorkerEgressSettings
	}{
		{name: "never saved", values: Values{}, want: WorkerEgressSettings{Mode: WorkerEgressOpen, Allowlist: []string{}}},
		{name: "open", values: Values{"mode": "open"}, want: WorkerEgressSettings{Mode: WorkerEgressOpen, Allowlist: []string{}}},
		{name: "allowlist", values: Values{"mode": "allowlist", "allowlist": []any{"api.github.com"}},
			want: WorkerEgressSettings{Mode: WorkerEgressAllowlist, Allowlist: []string{"api.github.com"}}},
		// A misspelt mode narrows, never widens.
		{name: "unknown mode", values: Values{"mode": "Open "}, want: WorkerEgressSettings{Mode: WorkerEgressAllowlist, Allowlist: []string{}}},
		{name: "mode not a string", values: Values{"mode": 1}, want: WorkerEgressSettings{Mode: WorkerEgressAllowlist, Allowlist: []string{}}},
		// A non-string entry is kept in a form the grammar refuses.
		{name: "non-string entry", values: Values{"allowlist": []any{"example.com", 7}},
			want: WorkerEgressSettings{Mode: WorkerEgressOpen, Allowlist: []string{"example.com", "7 (not a string)"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := WorkerEgressSettingsFromValues(test.values); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
	if err := ValidateWorkerEgressEntries([]string{"7 (not a string)"}, 10); err == nil {
		t.Fatal("the kept non-string entry parsed")
	}
}
