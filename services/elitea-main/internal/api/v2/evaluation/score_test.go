package evaluation

import (
	"math"
	"testing"
)

// The reference UI carries its own copy of this function and AVERAGES its
// output together with the server's stored `normalized_score` values. So a
// disagreement between the two is not a visible error — it is a headline that
// silently blends two scales. Every case below is a value where a plausible
// alternative implementation would answer differently.
func TestNormalizeScoreMatchesTheReferenceImplementation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		native    float64
		scaleType string
		min, max  float64
		polarity  string
		want      float64
		wantOK    bool
	}{
		{
			name: "continuous midpoint", native: 50, scaleType: ScaleContinuous,
			min: 0, max: 100, polarity: PolarityHigherBetter, want: 50, wantOK: true,
		},
		{
			name: "ordinal 4 of 1..5 is 75", native: 4, scaleType: ScaleOrdinal,
			min: 1, max: 5, polarity: PolarityHigherBetter, want: 75, wantOK: true,
		},
		{
			// The 2dp rounding. 2 of 1..4 is 33.333...; a implementation that
			// rounded to a whole number would answer 33 and disagree with the
			// client's fallback on every ordinal scale that is not a factor of
			// 100.
			name: "two decimal places", native: 2, scaleType: ScaleOrdinal,
			min: 1, max: 4, polarity: PolarityHigherBetter, want: 33.33, wantOK: true,
		},
		{
			name: "lower_better flips", native: 20, scaleType: ScaleContinuous,
			min: 0, max: 100, polarity: PolarityLowerBetter, want: 80, wantOK: true,
		},
		{
			// THE ORDER CASE. A native -50 on a 0..100 lower_better scale:
			// clamp-then-flip gives 0 -> 100. Flip-then-clamp would compute
			// 100 - (-50) = 150 and clamp to 100 as well... so the discriminator
			// has to be the OTHER side. See the next case.
			name: "below the floor, lower_better", native: -50, scaleType: ScaleContinuous,
			min: 0, max: 100, polarity: PolarityLowerBetter, want: 100, wantOK: true,
		},
		{
			// The real discriminator: native 150 on 0..100 lower_better.
			// clamp-then-flip = 100 -> 0. flip-then-clamp = 100-150 = -50 ->
			// clamped to 0. Both 0. So use higher_better with an over-range
			// value, where a MISSING clamp shows up directly.
			name: "above the ceiling clamps", native: 150, scaleType: ScaleContinuous,
			min: 0, max: 100, polarity: PolarityHigherBetter, want: 100, wantOK: true,
		},
		{
			// The case that ONLY a clamp before the flip answers this way. An
			// implementation with no clamp at all would give 100 - 150 = -50,
			// and a UI rendering -50 on a 0..100 bar draws nothing.
			name: "above the ceiling, lower_better", native: 150, scaleType: ScaleContinuous,
			min: 0, max: 100, polarity: PolarityLowerBetter, want: 0, wantOK: true,
		},
		{
			name: "binary non-zero is 100", native: 1, scaleType: ScaleBinary,
			min: 0, max: 1, polarity: PolarityHigherBetter, want: 100, wantOK: true,
		},
		{
			name: "binary zero is 0", native: 0, scaleType: ScaleBinary,
			min: 0, max: 1, polarity: PolarityHigherBetter, want: 0, wantOK: true,
		},
		{
			// A judge that answered 0.5 on a pass/fail dimension. The linear
			// branch would say 50; the reference's binary branch says 100,
			// because "not zero" is a pass.
			name: "binary half is a pass", native: 0.5, scaleType: ScaleBinary,
			min: 0, max: 1, polarity: PolarityHigherBetter, want: 100, wantOK: true,
		},
		{
			name: "binary lower_better inverts the pass", native: 1, scaleType: ScaleBinary,
			min: 0, max: 1, polarity: PolarityLowerBetter, want: 0, wantOK: true,
		},
		{
			// A degenerate snapshot scale. `snapshot` is free-form jsonb, so
			// this is reachable even though tenant/0130's CHECK forbids it on a
			// library row. The answer must be "no score" and NOT 0.
			name: "degenerate scale is unscorable", native: 5, scaleType: ScaleContinuous,
			min: 3, max: 3, polarity: PolarityHigherBetter, wantOK: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NormalizeScore(testCase.native, testCase.scaleType,
				testCase.min, testCase.max, testCase.polarity)
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v (score %v)", ok, testCase.wantOK, got)
			}
			if ok && got != testCase.want {
				t.Errorf("NormalizeScore = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A NaN score must be unscorable and not zero. An AI judge that answered
// `{"score": null}` reaches here as an absent score, and a run that recorded
// zero would report a working agent as the worst possible one.
func TestNormalizeScoreRefusesANonFiniteNative(t *testing.T) {
	t.Parallel()

	for _, native := range []float64{nan(), inf(1), inf(-1)} {
		if score, ok := NormalizeScore(native, ScaleContinuous, 0, 100, PolarityHigherBetter); ok {
			t.Errorf("NormalizeScore(%v) = %v, ok — a non-finite score is not a score", native, score)
		}
	}
}

// The target is evaluated on the NATIVE scale. This is what stops a target of
// "4" on a 1..5 scale from being compared against a normalised 75.
func TestEvaluateTargetMetComparesOnTheNativeScale(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		native    float64
		operator  string
		target    float64
		want      bool
		decidable bool
	}{
		{name: "4 >= 4", native: 4, operator: ">=", target: 4, want: true, decidable: true},
		{name: "4 > 4", native: 4, operator: ">", target: 4, want: false, decidable: true},
		{name: "3 <= 4", native: 3, operator: "<=", target: 4, want: true, decidable: true},
		{name: "5 < 4", native: 5, operator: "<", target: 4, want: false, decidable: true},
		{name: "4 == 4", native: 4, operator: "==", target: 4, want: true, decidable: true},
		{
			// `=` is NOT an operator here. The reference offers `==` and the
			// stored comparison switches on that exact string, so accepting `=`
			// would make a target that the client can never author silently
			// pass.
			name: "a single equals is not an operator", native: 4, operator: "=", target: 4,
			decidable: false,
		},
		{name: "an empty operator decides nothing", native: 4, operator: "", target: 4, decidable: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, decidable := EvaluateTargetMet(testCase.native, testCase.operator, testCase.target)
			if decidable != testCase.decidable {
				t.Fatalf("decidable = %v, want %v", decidable, testCase.decidable)
			}
			if decidable && got != testCase.want {
				t.Errorf("EvaluateTargetMet = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A dimension with no target has NOT failed one. The third return value is the
// difference between "not met" and "nothing to meet", and a caller that read
// only the boolean would paint every unbounded dimension as failing.
func TestEvaluateTargetMetSeparatesUnsetFromUnmet(t *testing.T) {
	t.Parallel()

	if _, decidable := EvaluateTargetMet(nan(), ">=", 4); decidable {
		t.Error("a non-finite score decided a target")
	}
	if _, decidable := EvaluateTargetMet(4, ">=", nan()); decidable {
		t.Error("a non-finite target was decided")
	}
}

func TestIsRunTerminalCoversTheReferenceVocabulary(t *testing.T) {
	t.Parallel()

	terminal := []string{RunStatusFinished, RunStatusErrored, RunStatusCancelled}
	active := []string{RunStatusCreated, RunStatusRunning}
	for _, status := range terminal {
		if !IsRunTerminal(status) {
			t.Errorf("%q is not terminal, want terminal", status)
		}
	}
	for _, status := range active {
		if IsRunTerminal(status) {
			t.Errorf("%q is terminal, want active", status)
		}
	}
	// An unknown status is NOT terminal. A poller that treated it as terminal
	// would stop following a run whose status it simply had not been taught.
	if IsRunTerminal("something_new") {
		t.Error("an unknown status reported as terminal")
	}
}

func nan() float64         { return math.NaN() }
func inf(sign int) float64 { return math.Inf(sign) }
