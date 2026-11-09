package platformconfig

import (
	"slices"
	"testing"
)

func TestBrowserSettingsFromValues(t *testing.T) {
	for _, test := range []struct {
		name   string
		values Values
		want   BrowserSettings
	}{
		// Open egress is an explicit choice (ADR-0049 decision 5).
		{name: "never saved", values: Values{}, want: BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: 1800}},
		{name: "allowlist", values: Values{"browser_egress": "allowlist", "max_run_seconds": float64(60),
			"monthly_quota_seconds": float64(3600)},
			want: BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: 60, MonthlyQuotaSeconds: 3600}},
		{name: "open", values: Values{"browser_egress": "open"}, want: BrowserSettings{Egress: BrowserEgressOpen, MaxRunSeconds: 1800}},
		// A misspelt mode narrows, never widens.
		{name: "unknown mode", values: Values{"browser_egress": "Opn"}, want: BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: 1800}},
		{name: "mode not a string", values: Values{"browser_egress": 1}, want: BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: 1800}},
		{name: "zero is no cap", values: Values{"max_run_seconds": float64(0)}, want: BrowserSettings{Egress: BrowserEgressAllowlist}},
		// A value that does not read is the tightest cap, never no cap.
		{name: "unreadable", values: Values{"max_run_seconds": "x", "monthly_quota_seconds": float64(-5)},
			want: BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: 1, MonthlyQuotaSeconds: 1}},
		{name: "clamped", values: Values{"max_run_seconds": float64(1e9), "monthly_quota_seconds": float64(1e12)},
			want: BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: 86400, MonthlyQuotaSeconds: 100_000_000}},
		{name: "fraction", values: Values{"max_run_seconds": 0.4}, want: BrowserSettings{Egress: BrowserEgressAllowlist, MaxRunSeconds: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := BrowserSettingsFromValues(test.values); got != test.want {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestBrowserEnginesFollowTheAdmission(t *testing.T) {
	for _, test := range []struct {
		admission BrowserAdmission
		want      []string
	}{
		{BrowserAdmission{}, []string{"full"}},
		{BrowserAdmission{Auto: true}, []string{"full", "auto"}},
		{BrowserAdmission{Auto: true, Lite: true}, []string{"full", "lite", "auto"}},
	} {
		if got := BrowserEngines(test.admission); !slices.Equal(got, test.want) {
			t.Fatalf("%+v: got %v, want %v", test.admission, got, test.want)
		}
	}
	// The recorded appendix: auto admitted, lite not.
	if AdmittedBrowserEngines != (BrowserAdmission{Auto: true}) {
		t.Fatalf("admission = %+v, want auto only", AdmittedBrowserEngines)
	}
}
