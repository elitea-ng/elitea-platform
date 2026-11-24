package platformconfig

import (
	"reflect"
	"testing"
)

func TestWorkerEgressSettingsFromValues(t *testing.T) {
	settings := func(mode WorkerEgressMode, allow, private []string) WorkerEgressSettings {
		return WorkerEgressSettings{Mode: mode, Allowlist: allow, PrivatePermit: private}
	}
	for _, test := range []struct {
		name   string
		values Values
		want   WorkerEgressSettings
	}{
		{name: "never saved", values: Values{}, want: settings(WorkerEgressOpen, []string{}, []string{})},
		{name: "open", values: Values{"mode": "open"}, want: settings(WorkerEgressOpen, []string{}, []string{})},
		{name: "allowlist", values: Values{"mode": "allowlist", "allowlist": []any{"api.github.com"}},
			want: settings(WorkerEgressAllowlist, []string{"api.github.com"}, []string{})},
		// A misspelt mode narrows, never widens.
		{name: "unknown mode", values: Values{"mode": "Open "}, want: settings(WorkerEgressAllowlist, []string{}, []string{})},
		{name: "mode not a string", values: Values{"mode": 1}, want: settings(WorkerEgressAllowlist, []string{}, []string{})},
		// A non-string entry is kept in a form the grammar refuses.
		{name: "non-string entry", values: Values{"allowlist": []any{"example.com", 7}},
			want: settings(WorkerEgressOpen, []string{"example.com", "7 (not a string)"}, []string{})},
		// The private permit (#2950) is read in either mode, kept as stored.
		{name: "private permit", values: Values{"private_permit": []any{"jira.corp.example", "10.20.0.0/16", false}},
			want: settings(WorkerEgressOpen, []string{}, []string{"jira.corp.example", "10.20.0.0/16", "false (not a string)"})},
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
