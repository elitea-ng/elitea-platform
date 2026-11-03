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
