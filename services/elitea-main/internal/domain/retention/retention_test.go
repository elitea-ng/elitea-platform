package retention

import (
	"errors"
	"testing"
)

func days(n int) *int   { return &n }
func id(n int64) *int64 { return &n }

func platformConversation(minDays int, maxDays *int, allow bool) Policy {
	return Policy{Class: ClassConversation, MinAgeDays: minDays, MaxAgeDays: maxDays, AllowProjectOverride: allow}
}

// The seeded defaults (shared 0195) must validate: an administrator who opens
// the page and saves without changing anything must not be refused.
func TestSeededDefaultsValidate(t *testing.T) {
	seeded := []Policy{
		{Class: ClassConversation, MinAgeDays: 0, AllowProjectOverride: true},
		{Class: ClassAttachment},
		{Class: ClassGatewayRequestLog, MinAgeDays: 1, MaxAgeDays: days(92)},
		{Class: ClassAdminAudit, MinAgeDays: 7, MaxAgeDays: days(365)},
		{Class: ClassSecurityAudit, MinAgeDays: 1},
	}
	for _, p := range seeded {
		if err := ValidatePlatform(p); err != nil {
			t.Errorf("%s: %v", p.Class, err)
		}
	}
}

// Keep forever is the default of conversations, so the effective maximum of a
// project without an override is nil: nothing is deleted.
func TestDefaultConversationPolicyKeepsForever(t *testing.T) {
	if got := Effective(platformConversation(0, nil, true), nil); got != nil {
		t.Fatalf("default effective maximum = %d, want keep forever", *got)
	}
}

func TestProjectOverrideOutsidePlatformBoundsIsRefused(t *testing.T) {
	platform := platformConversation(30, days(365), true)
	cases := map[string]*int{
		"below the minimum":  days(29),
		"above the maximum":  days(366),
		"forever over a max": nil,
	}
	for name, maxDays := range cases {
		err := ValidateOverride(platform, Policy{Class: ClassConversation, ProjectID: id(7), MaxAgeDays: maxDays})
		if !errors.Is(err, ErrOutOfBounds) {
			t.Errorf("%s: got %v, want ErrOutOfBounds", name, err)
		}
	}
	if err := ValidateOverride(platform, Policy{Class: ClassConversation, ProjectID: id(7), MaxAgeDays: days(90)}); err != nil {
		t.Fatalf("an override inside the bounds was refused: %v", err)
	}
}

func TestOverrideRefusedWhenThePlatformDisallowsIt(t *testing.T) {
	err := ValidateOverride(platformConversation(0, nil, false),
		Policy{Class: ClassConversation, ProjectID: id(7), MaxAgeDays: days(90)})
	if !errors.Is(err, ErrNoOverride) {
		t.Fatalf("got %v, want ErrNoOverride", err)
	}
	gateway := Policy{Class: ClassGatewayRequestLog, MinAgeDays: 1, MaxAgeDays: days(92)}
	err = ValidateOverride(gateway, Policy{Class: ClassGatewayRequestLog, ProjectID: id(7), MaxAgeDays: days(30)})
	if !errors.Is(err, ErrNoOverride) {
		t.Fatalf("gateway override: got %v, want ErrNoOverride", err)
	}
}

func TestPlatformRefusals(t *testing.T) {
	cases := map[string]struct {
		p    Policy
		want error
	}{
		"unknown class":       {Policy{Class: "step_record"}, ErrUnknownClass},
		"gateway forever":     {Policy{Class: ClassGatewayRequestLog}, ErrForeverRefused},
		"attachment max":      {Policy{Class: ClassAttachment, MaxAgeDays: days(30)}, ErrNotEnforced},
		"audit below floor":   {Policy{Class: ClassAdminAudit, MaxAgeDays: days(3)}, ErrInvalid},
		"max below min":       {Policy{Class: ClassConversation, MinAgeDays: 30, MaxAgeDays: days(10)}, ErrInvalid},
		"override on gateway": {Policy{Class: ClassGatewayRequestLog, MaxAgeDays: days(92), AllowProjectOverride: true}, ErrNoOverride},
	}
	for name, c := range cases {
		if err := ValidatePlatform(c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

// A platform edit that tightens the maximum wins over an override stored
// under the old bounds.
func TestEffectiveClampsAStaleOverride(t *testing.T) {
	platform := platformConversation(0, days(30), true)
	got := Effective(platform, &Policy{Class: ClassConversation, ProjectID: id(7), MaxAgeDays: days(90)})
	if got == nil || *got != 30 {
		t.Fatalf("effective = %v, want 30", got)
	}
	got = Effective(platform, &Policy{Class: ClassConversation, ProjectID: id(7), MaxAgeDays: days(10)})
	if got == nil || *got != 10 {
		t.Fatalf("effective = %v, want 10", got)
	}
	// Overrides switched off: the platform value applies.
	platform.AllowProjectOverride = false
	got = Effective(platform, &Policy{Class: ClassConversation, ProjectID: id(7), MaxAgeDays: days(10)})
	if got == nil || *got != 30 {
		t.Fatalf("effective with overrides off = %v, want 30", got)
	}
}

func TestValidateHold(t *testing.T) {
	ok := []Hold{
		{Scope: HoldProject, ProjectID: id(1), Reason: "litigation"},
		{Scope: HoldAgent, ProjectID: id(1), AgentID: id(2), Reason: "model review"},
		{Scope: HoldUser, UserID: id(3), Reason: "employment dispute"},
	}
	for _, h := range ok {
		if err := ValidateHold(h); err != nil {
			t.Errorf("%s: %v", h.Scope, err)
		}
	}
	bad := []Hold{
		{Scope: HoldProject, ProjectID: id(1)},
		{Scope: HoldAgent, ProjectID: id(1), Reason: "x"},
		{Scope: HoldUser, UserID: id(3), ProjectID: id(1), Reason: "x"},
		{Scope: "tenant", Reason: "x"},
	}
	for _, h := range bad {
		if err := ValidateHold(h); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: got %v, want ErrInvalid", h, err)
		}
	}
}
