package unattended

import (
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// ADR-0045: a max_spend of 0 would store "no spend limit". The server
// refuses 0, a negative value and a value that rounds to 0 as invalid; an
// absent max_spend takes the default.
func TestResolveLimitsRefusesANonPositiveSpend(t *testing.T) {
	for _, settings := range []platformconfig.UnattendedSettings{
		platformconfig.DefaultUnattendedSettings(),
		func() platformconfig.UnattendedSettings {
			s := platformconfig.DefaultUnattendedSettings()
			s.MaxSpendUSD = 0
			return s
		}(),
	} {
		for _, value := range []float64{0, -1, 0.00004, -0.00001} {
			_, err := ResolveLimits(settings, ChainRequest{MaxSpendUSD: &value}, 2)
			var refusal *RefusalError
			if !errors.As(err, &refusal) || refusal.Field != "chain.max_spend" || refusal.Reason != RefusedInvalid {
				t.Fatalf("max_spend %v (platform max %v) = %v, want invalid", value, settings.MaxSpendUSD, err)
			}
		}
		smallest := 0.0001
		limits, err := ResolveLimits(settings, ChainRequest{MaxSpendUSD: &smallest}, 2)
		if err != nil || limits.MaxSpendUSD != 0.0001 {
			t.Fatalf("max_spend 0.0001 = %+v, %v", limits, err)
		}
		limits, err = ResolveLimits(settings, ChainRequest{}, 2)
		if err != nil || limits.MaxSpendUSD <= 0 {
			t.Fatalf("absent max_spend = %+v, %v", limits, err)
		}
	}
}

func TestValidateBrowserSubmits(t *testing.T) {
	many := make([]string, MaxBrowserSubmits+1)
	for index := range many {
		many[index] = "example.com"
	}
	for _, test := range []struct {
		name  string
		hosts []string
		want  string
	}{
		{name: "none"},
		{name: "valid", hosts: []string{"example.com", "shop.example.co.uk", "a-b.example.com", "x1.example.io"}},
		{name: "repeated host is accepted", hosts: []string{"example.com", "example.com"}},
		{name: "upper case", hosts: []string{"Example.com"}, want: RefusedInvalid},
		{name: "port", hosts: []string{"example.com:443"}, want: RefusedInvalid},
		{name: "wildcard", hosts: []string{"*.example.com"}, want: RefusedInvalid},
		{name: "scheme", hosts: []string{"https://example.com"}, want: RefusedInvalid},
		{name: "path", hosts: []string{"example.com/login"}, want: RefusedInvalid},
		{name: "one label", hosts: []string{"localhost"}, want: RefusedInvalid},
		{name: "ipv4", hosts: []string{"10.0.0.1"}, want: RefusedInvalid},
		{name: "empty", hosts: []string{""}, want: RefusedInvalid},
		{name: "space", hosts: []string{" example.com"}, want: RefusedInvalid},
		{name: "leading hyphen", hosts: []string{"-a.example.com"}, want: RefusedInvalid},
		{name: "trailing dot", hosts: []string{"example.com."}, want: RefusedInvalid},
		{name: "too many", hosts: many, want: RefusedOverMaximum},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateShape(Request{GrantID: "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60", BrowserSubmits: test.hosts})
			if test.want == "" {
				if err != nil {
					t.Fatalf("err=%v", err)
				}
				return
			}
			refusal, ok := AsRefusal(err)
			if !ok || refusal.Reason != test.want || refusal.Field != "browser_submits" {
				t.Fatalf("err=%v, want %s on browser_submits", err, test.want)
			}
		})
	}
}

func TestGrantOfDeduplicatesBrowserSubmits(t *testing.T) {
	grant := GrantOf(Request{BrowserSubmits: []string{"b.example.com", "a.example.com", "b.example.com"}}, nil)
	if len(grant.BrowserSubmits) != 2 || grant.BrowserSubmits[0] != "b.example.com" || grant.BrowserSubmits[1] != "a.example.com" {
		t.Fatalf("browser_submits=%v", grant.BrowserSubmits)
	}
	if GrantOf(Request{}, nil).BrowserSubmits != nil {
		t.Fatal("a grant without hosts stores some")
	}
	if (Request{BrowserSubmits: []string{"example.com"}}).OnlyContinues() {
		t.Fatal("a grant that names browser hosts only continues")
	}
}
