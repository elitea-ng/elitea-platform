package agentworkforce

import "testing"

func rate(v float64) *float64 { return &v }

// passing is a hire proposal that meets every condition of ADR-0051 decision 6.
func passing() ListingHireEvidence {
	return ListingHireEvidence{
		Mode: "install", Tier: TierLow, Reviews: 3, RatingAvg: 4.0, DismissalRate: rate(0.19),
	}
}

func TestCheckListingHireAdmitsOnlyWhenEveryConditionHolds(t *testing.T) {
	policy := DefaultPolicy()
	policy.AutoHireEnabled = true
	if got := CheckListingHire(policy, passing()); got != "" {
		t.Fatalf("a listing that meets every condition waits: %q", got)
	}
	cases := map[string]struct {
		change func(*ListingHireEvidence)
		want   string
	}{
		"fork":                    {func(e *ListingHireEvidence) { e.Mode = "fork" }, ListingNotInstall},
		"medium tier":             {func(e *ListingHireEvidence) { e.Tier = TierMedium }, ListingNotLowTier},
		"a write tool":            {func(e *ListingHireEvidence) { e.HasWriteTools = true }, ListingWriteTools},
		"an unbindable type":      {func(e *ListingHireEvidence) { e.Unbound = []string{"github"} }, ListingUnbound},
		"two reviews":             {func(e *ListingHireEvidence) { e.Reviews = 2 }, ListingFewReviews},
		"rating 3.9":              {func(e *ListingHireEvidence) { e.RatingAvg = 3.9 }, ListingLowRating},
		"dismissal rate 0.2":      {func(e *ListingHireEvidence) { e.DismissalRate = rate(0.2) }, ListingDismissal},
		"dismissal not measured":  {func(e *ListingHireEvidence) { e.DismissalRate = nil }, ListingNoDismissals},
		"the probation cap":       {func(e *ListingHireEvidence) { e.OpenAutoHired = policy.MaxAutoHired }, HireCapReached},
		"undone in the last days": {func(e *ListingHireEvidence) { e.Undone = true }, HireUndoneRecent},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			evidence := passing()
			c.change(&evidence)
			if got := CheckListingHire(policy, evidence); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCheckListingHireIsOffUntilAPersonTurnsAutoHireOn(t *testing.T) {
	if got := CheckListingHire(DefaultPolicy(), passing()); got != HirePolicyDisable {
		t.Fatalf("got %q", got)
	}
}

func TestCheckListingHireReadsTheProjectsNumbers(t *testing.T) {
	policy := DefaultPolicy()
	policy.AutoHireEnabled = true
	policy.ListingMinReviews, policy.ListingMinRating, policy.ListingMaxDismissalRate = 10, 4.5, 0.05
	evidence := passing()
	if got := CheckListingHire(policy, evidence); got != ListingFewReviews {
		t.Fatalf("got %q", got)
	}
	evidence.Reviews, evidence.RatingAvg = 10, 4.5
	if got := CheckListingHire(policy, evidence); got != ListingDismissal {
		t.Fatalf("got %q", got)
	}
	evidence.DismissalRate = rate(0.04)
	if got := CheckListingHire(policy, evidence); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestPolicyListingNumbersDefaultAndAreBounded(t *testing.T) {
	// An older client sends only the six original fields: the three numbers read
	// as the defaults and auto_feedback stays off.
	old := Policy{MinSuccessfulRuns: 3, MaxAutoHired: 5, GraceDays: 7}
	got, err := CheckPolicy(old)
	if err != nil {
		t.Fatal(err)
	}
	if got.ListingMinReviews != 3 || got.ListingMinRating != 4.0 || got.ListingMaxDismissalRate != 0.2 || got.AutoFeedbackEnabled {
		t.Fatalf("defaults: %+v", got)
	}
	for name, change := range map[string]func(*Policy){
		"min_reviews too high": func(p *Policy) { p.ListingMinReviews = 1001 },
		"min_reviews negative": func(p *Policy) { p.ListingMinReviews = -1 },
		"rating above five":    func(p *Policy) { p.ListingMinRating = 5.1 },
		"rating below one":     func(p *Policy) { p.ListingMinRating = 0.5 },
		"rate above one":       func(p *Policy) { p.ListingMaxDismissalRate = 1.1 },
		"rate negative":        func(p *Policy) { p.ListingMaxDismissalRate = -0.1 },
	} {
		t.Run(name, func(t *testing.T) {
			policy := DefaultPolicy()
			change(&policy)
			if _, err := CheckPolicy(policy); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	rounded := DefaultPolicy()
	rounded.ListingMinRating, rounded.ListingMaxDismissalRate = 4.26, 0.153
	got, err = CheckPolicy(rounded)
	if err != nil || got.ListingMinRating != 4.3 || got.ListingMaxDismissalRate != 0.15 {
		t.Fatalf("rounded: %+v %v", got, err)
	}
}

func TestIsMarketplaceKindAndOutcomes(t *testing.T) {
	for kind, want := range map[string]bool{KindHireListing: true, KindFeedbackReport: true, "dismiss": false, "demote": false, "": false} {
		if IsMarketplaceKind(kind) != want {
			t.Errorf("IsMarketplaceKind(%q) != %v", kind, want)
		}
	}
	for _, outcome := range []string{"kept", "demoted", "dismissed"} {
		if !ValidReportOutcome(outcome) {
			t.Errorf("%q refused", outcome)
		}
	}
	for _, outcome := range []string{"", "fired", "approved", "Kept"} {
		if ValidReportOutcome(outcome) {
			t.Errorf("%q accepted", outcome)
		}
	}
}
