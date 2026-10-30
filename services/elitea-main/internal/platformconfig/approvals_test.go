package platformconfig

import (
	"testing"
	"time"
)

func TestApprovalsSettingsDefaultToFailAndNeverApprove(t *testing.T) {
	if got := DefaultApprovalsSettings(); got.UnattendedDefault != OnApprovalRequiredFail || got.UnattendedWait != 24*time.Hour {
		t.Fatalf("defaults = %+v", got)
	}
	for _, value := range []any{"auto_approve", "approve", "", 3, nil} {
		got := ApprovalsSettingsFromValues(Values{KeyApprovalsUnattendedDefault: value})
		if got.UnattendedDefault != OnApprovalRequiredFail {
			t.Fatalf("%v read as %q, want fail", value, got.UnattendedDefault)
		}
	}
	got := ApprovalsSettingsFromValues(Values{
		KeyApprovalsUnattendedDefault:   " notify_and_wait ",
		KeyApprovalsUnattendedWaitHours: float64(2),
	})
	if got.UnattendedDefault != OnApprovalRequiredNotifyAndWait || got.UnattendedWait != 2*time.Hour {
		t.Fatalf("saved values = %+v", got)
	}
}

func TestApprovalsWaitHoursAreClampedAndValidated(t *testing.T) {
	for raw, want := range map[float64]time.Duration{0: time.Hour, 5000: 720 * time.Hour, 3.7: 3 * time.Hour} {
		if got := ApprovalsSettingsFromValues(Values{KeyApprovalsUnattendedWaitHours: raw}).UnattendedWait; got != want {
			t.Fatalf("%v hours = %v, want %v", raw, got, want)
		}
	}
	for raw, want := range map[any]bool{
		float64(1): true, float64(720): true, float64(0): false, float64(721): false, 1.5: false, "24": false,
	} {
		if got := ValidApprovalsWaitHours(raw); got != want {
			t.Fatalf("ValidApprovalsWaitHours(%v) = %v", raw, got)
		}
	}
}
