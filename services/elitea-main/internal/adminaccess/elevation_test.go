package adminaccess_test

// The input rules of the elevation ledger (ADR-0059 decision 5; story R2.1a,
// issue 2515). Every refusal happens before the database is touched, so
// these run with no transaction at all.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

var elevationClock = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func validElevationStart() adminaccess.ElevationStart {
	return adminaccess.ElevationStart{
		UserID:            7,
		BrowserSessionRef: 41,
		Level:             adminaccess.ElevationLevelAdmin,
		Path:              adminaccess.ElevationPathApproved,
		ReasonCode:        adminaccess.ElevationReasonIncident,
		Reason:            "the provider is down",
		ApproverUserID:    8,
		At:                elevationClock,
		Duration:          time.Hour,
	}
}

func TestStartElevationRefusesBadInputBeforeTheDatabase(t *testing.T) {
	if err := validElevationStart().Validate(); err != nil {
		t.Fatalf("premise: the valid start is refused: %v", err)
	}
	for name, test := range map[string]struct {
		change func(*adminaccess.ElevationStart)
		want   error
	}{
		// A session without a server-side row has no reference and can hold
		// no elevation.
		"session reference zero":   {func(s *adminaccess.ElevationStart) { s.BrowserSessionRef = 0 }, adminaccess.ErrElevationNoSession},
		"session reference below":  {func(s *adminaccess.ElevationStart) { s.BrowserSessionRef = -1 }, adminaccess.ErrElevationNoSession},
		"unknown level":            {func(s *adminaccess.ElevationStart) { s.Level = "owner" }, adminaccess.ErrElevationLevel},
		"empty level":              {func(s *adminaccess.ElevationStart) { s.Level = "" }, adminaccess.ErrElevationLevel},
		"a role that is no level":  {func(s *adminaccess.ElevationStart) { s.Level = adminaccess.RoleEmergencyAccess }, adminaccess.ErrElevationLevel},
		"unknown path":             {func(s *adminaccess.ElevationStart) { s.Path = "manual" }, adminaccess.ErrElevationPath},
		"empty path":               {func(s *adminaccess.ElevationStart) { s.Path = "" }, adminaccess.ErrElevationPath},
		"unknown reason code":      {func(s *adminaccess.ElevationStart) { s.ReasonCode = "curiosity" }, adminaccess.ErrElevationReasonCode},
		"blank reason":             {func(s *adminaccess.ElevationStart) { s.Reason = "  " }, adminaccess.ErrElevationInvalid},
		"no person":                {func(s *adminaccess.ElevationStart) { s.UserID = 0 }, adminaccess.ErrElevationInvalid},
		"no clock":                 {func(s *adminaccess.ElevationStart) { s.At = time.Time{} }, adminaccess.ErrElevationNoTimestamp},
		"no duration":              {func(s *adminaccess.ElevationStart) { s.Duration = 0 }, adminaccess.ErrElevationInvalid},
		"above the online ceiling": {func(s *adminaccess.ElevationStart) { s.Duration = adminaccess.MaxElevationDurationOnline + time.Second }, adminaccess.ErrElevationInvalid},
		"above the sealed ceiling": {func(s *adminaccess.ElevationStart) {
			s.Path, s.ApproverUserID, s.Duration = adminaccess.ElevationPathSealed, 0, adminaccess.MaxElevationDurationSealed+time.Second
		}, adminaccess.ErrElevationInvalid},
		"approved with no approver": {func(s *adminaccess.ElevationStart) { s.ApproverUserID = 0 }, adminaccess.ErrElevationInvalid},
		"approved by the person":    {func(s *adminaccess.ElevationStart) { s.ApproverUserID = s.UserID }, adminaccess.ErrElevationInvalid},
		"approved by the requester": {func(s *adminaccess.ElevationStart) { s.RequesterUserID, s.ApproverUserID = 9, 9 }, adminaccess.ErrElevationInvalid},
		"self with an approver":     {func(s *adminaccess.ElevationStart) { s.Path = adminaccess.ElevationPathSelf }, adminaccess.ErrElevationInvalid},
		"self at super_admin": {func(s *adminaccess.ElevationStart) {
			s.Path, s.ApproverUserID, s.Level = adminaccess.ElevationPathSelf, 0, adminaccess.ElevationLevelSuperAdmin
		}, adminaccess.ErrElevationInvalid},
		"started before the request": {func(s *adminaccess.ElevationStart) { s.RequestedAt = s.At.Add(time.Minute) }, adminaccess.ErrElevationInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			start := validElevationStart()
			test.change(&start)
			if err := start.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("Validate: %v, want %v", err, test.want)
			}
			// StartElevation refuses it before it needs the transaction or
			// the appender: both are nil here.
			if _, err := adminaccess.StartElevation(context.Background(), nil, nil, start); !errors.Is(err, test.want) {
				t.Fatalf("StartElevation: %v, want %v", err, test.want)
			}
		})
	}
}

func TestTheLedgerNeedsAnAppenderAndATransaction(t *testing.T) {
	ctx := context.Background()
	end := adminaccess.ElevationEnd{Reason: adminaccess.ElevationEndHolder, At: elevationClock}
	// No appender: the write would have no security event (R2.3).
	if _, err := adminaccess.StartElevation(ctx, nil, nil, validElevationStart()); !errors.Is(err, adminaccess.ErrElevationNoAppender) {
		t.Fatalf("StartElevation with no appender: %v", err)
	}
	if _, _, err := adminaccess.EndElevation(ctx, nil, nil, end); !errors.Is(err, adminaccess.ErrElevationNoAppender) {
		t.Fatalf("EndElevation with no appender: %v", err)
	}
	if _, err := adminaccess.StartElevation(ctx, nil, securityaudit.FailingAppender{}, validElevationStart()); err == nil {
		t.Fatal("StartElevation with no transaction succeeded")
	}
	if _, _, err := adminaccess.EndElevation(ctx, nil, securityaudit.FailingAppender{}, end); err == nil {
		t.Fatal("EndElevation with no transaction succeeded")
	}
}

func TestEndElevationRefusesAnUnknownReasonAndNoClock(t *testing.T) {
	for _, end := range []adminaccess.ElevationEnd{
		{Reason: "bored", At: elevationClock},
		{Reason: "", At: elevationClock},
		{Reason: adminaccess.ElevationEndHolder},
	} {
		if _, _, err := adminaccess.EndElevation(context.Background(), nil, securityaudit.FailingAppender{}, end); !errors.Is(err, adminaccess.ErrElevationInvalid) {
			t.Fatalf("EndElevation(%+v): %v, want a refusal", end, err)
		}
	}
}

// The state is derived from the columns and the clock. Expiry is a predicate:
// a started row with no end is active before its expiry and expired from it,
// with no column changed.
func TestElevationStateIsDerivedFromTheClock(t *testing.T) {
	started := adminaccess.ElevationRecord{
		Decision:  adminaccess.ElevationDecisionApproved,
		Level:     adminaccess.ElevationLevelAdmin,
		StartedAt: elevationClock,
		ExpiresAt: elevationClock.Add(time.Hour),
	}
	ended := started
	ended.EndedAt = elevationClock.Add(time.Minute)
	ended.EndReason = adminaccess.ElevationEndHolder
	pending := ended
	pending.ReviewState = adminaccess.ElevationReviewPending
	reviewed := ended
	reviewed.ReviewState = adminaccess.ElevationReviewed
	unreviewed := ended
	unreviewed.ReviewState = adminaccess.ElevationUnreviewedNoReviewer

	for name, test := range map[string]struct {
		row    adminaccess.ElevationRecord
		at     time.Time
		want   string
		active bool
	}{
		"requested":             {adminaccess.ElevationRecord{}, elevationClock, adminaccess.ElevationStateRequested, false},
		"approved":              {adminaccess.ElevationRecord{Decision: adminaccess.ElevationDecisionApproved}, elevationClock, adminaccess.ElevationStateApproved, false},
		"denied":                {adminaccess.ElevationRecord{Decision: adminaccess.ElevationDecisionDenied}, elevationClock, adminaccess.ElevationStateDenied, false},
		"request expired":       {adminaccess.ElevationRecord{Decision: adminaccess.ElevationDecisionRequestExpired}, elevationClock, adminaccess.ElevationStateRequestExpired, false},
		"active":                {started, elevationClock.Add(59 * time.Minute), adminaccess.ElevationStateActive, true},
		"expired at the expiry": {started, elevationClock.Add(time.Hour), adminaccess.ElevationStateExpired, false},
		"expired after":         {started, elevationClock.Add(2 * time.Hour), adminaccess.ElevationStateExpired, false},
		"ended":                 {ended, elevationClock.Add(2 * time.Minute), adminaccess.ElevationStateEnded, false},
		"review pending":        {pending, elevationClock.Add(2 * time.Minute), adminaccess.ElevationStateReviewPending, false},
		"reviewed":              {reviewed, elevationClock.Add(2 * time.Minute), adminaccess.ElevationStateReviewed, false},
		"no reviewer":           {unreviewed, elevationClock.Add(2 * time.Minute), adminaccess.ElevationStateUnreviewedNoReviewer, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := test.row.State(test.at); got != test.want {
				t.Fatalf("State = %q, want %q", got, test.want)
			}
			if got := test.row.ActiveAt(test.at); got != test.active {
				t.Fatalf("ActiveAt = %v, want %v", got, test.active)
			}
		})
	}
}
