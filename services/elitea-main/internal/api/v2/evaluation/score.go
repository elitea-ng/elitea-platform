package evaluation

import "math"

// NormalizeScore maps a native score onto 0..100.
//
// THIS IS THE SERVER SIDE OF A PAIR, AND THE PAIR MUST AGREE EXACTLY.
//
// The reference UI carries its own copy in
// `widgets/evaluation/lib/helpers/scorecard.helpers.js`, and that copy's
// comment says it "mirrors the server's `normalize_score` exactly — same
// clamp-then-flip order, same 2dp rounding". It is used as a FALLBACK, for
// rows whose `normalized_score` is absent, and the fallback values are
// AVERAGED TOGETHER with the server's stored ones in a single headline number.
// So a disagreement between the two does not produce a visible error: it
// produces a headline that is a blend of two scales, and nothing anywhere
// reports it. That is why the order below is spelled out step by step.
//
// THE ORDER, and why each step is where it is:
//
//  1. binary is not interpolated. Any non-zero native value is 100 and zero is
//  0. A binary dimension's scale_min/scale_max are 0 and 1 in practice, so
//     the linear branch would give the same answer for 0 and 1 — but not for a
//     judge that answered 0.5, which a "pass or fail" dimension must round to a
//     verdict rather than report as half a pass.
//  2. the linear map is ((value - min) / (max - min)) * 100. tenant/0130's
//     CHECK guarantees min < max on a stored dimension, so the division cannot
//     be by zero for a library row; the guard below is for a SNAPSHOT, which is
//     free-form jsonb and could hold anything a later writer put there.
//  3. CLAMP to [0, 100] BEFORE the polarity flip. This is the step whose order
//     matters. A native 150 on a 0..100 lower_better scale clamps to 100 and
//     then flips to 0 — a very bad answer. Flipping first would give -50 and
//     then clamp to 0 as well, which agrees here, but a native -50 flips to 150
//     and clamps to 100, where clamp-first gives 0 → 100. The two orders
//     disagree on out-of-range input, and out-of-range input is exactly what an
//     AI judge produces when it ignores the scale.
//  4. FLIP for lower_better. Applied last of the value transforms, because it
//     is a statement about the DIMENSION and not about the measurement.
//  5. ROUND to 2 decimal places, last. JavaScript's Math.round is half-up and
//     Go's math.Round is half-away-from-zero; the value here is already clamped
//     to [0, 100], so it is non-negative and the two are the same function.
//
// The second return value is false when no score can be produced — a
// non-finite native value, or a snapshot scale that is degenerate. It is
// deliberately not "0", because 0 is a legitimate and very bad score, and
// reporting an unscorable case as the worst possible one is the failure mode
// this whole slice is built to avoid.
func NormalizeScore(native float64, scaleType string, scaleMin, scaleMax float64, polarity string) (float64, bool) {
	if math.IsNaN(native) || math.IsInf(native, 0) {
		return 0, false
	}

	var norm float64
	if scaleType == ScaleBinary {
		if native != 0 {
			norm = 100
		}
	} else {
		if math.IsNaN(scaleMin) || math.IsInf(scaleMin, 0) ||
			math.IsNaN(scaleMax) || math.IsInf(scaleMax, 0) ||
			scaleMax == scaleMin {
			return 0, false
		}
		norm = ((native - scaleMin) / (scaleMax - scaleMin)) * 100
	}

	norm = math.Min(100, math.Max(0, norm))
	if polarity == PolarityLowerBetter {
		norm = 100 - norm
	}
	return math.Round(norm*100) / 100, true
}

// EvaluateTargetMet answers whether a native score meets a stored target.
//
// ON THE NATIVE SCALE, NOT THE NORMALISED ONE. This is the reference's
// `evaluateTargetMet` and it is not an accident of implementation: an author
// states a target in the units they authored the dimension in ("at least 4 out
// of 5"), and comparing 4 against a normalised 80 would silently pass or fail
// every target the moment a polarity or a scale bound changed. The
// normalisation exists to make dimensions COMPARABLE to each other; the target
// exists to make ONE dimension answerable, and the two are different questions.
//
// The third return value is false when there is nothing to decide — no score,
// no operator or no target. It is not "not met": a dimension with no target
// has not failed one.
func EvaluateTargetMet(native float64, operator string, target float64) (bool, bool) {
	if math.IsNaN(native) || math.IsInf(native, 0) ||
		math.IsNaN(target) || math.IsInf(target, 0) {
		return false, false
	}
	switch operator {
	case ">=":
		return native >= target, true
	case ">":
		return native > target, true
	case "<=":
		return native <= target, true
	case "<":
		return native < target, true
	case "==":
		return native == target, true
	default:
		return false, false
	}
}
