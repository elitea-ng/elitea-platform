//! The float arithmetic of Python 3.12 and numpy that reaches Phase 2's
//! output, reproduced to the last bit.
//!
//! * [`py_sum`]: the builtin `sum()` over floats. Since 3.12 it is NOT a
//!   left-to-right loop but Neumaier's compensated sum, so a plain loop
//!   would differ in the last bits of the weight mean.
//! * [`numpy_pairwise_sum`]: `np.add.reduce` over a contiguous `float64`
//!   array, which hub detection's `arr.std()` uses. numpy sums pairwise in
//!   blocks of 128 with eight accumulators; the sum of squared deviations
//!   depends on that order.
//! * [`py_round`]: `round(x, ndigits)`, the correctly rounded decimal
//!   (ties to even on the EXACT binary value), as the stats dict holds it.

/// `sum(values)` for a list of Python floats (`CPython` 3.12
/// `builtin_sum_impl`, float path).
#[must_use]
pub fn py_sum(values: &[f64]) -> f64 {
    let Some((&first, rest)) = values.split_first() else {
        return 0.0;
    };
    // The int start value 0 meets the first float: `0 + x` is `x`.
    let mut sum = first;
    let mut compensation = 0.0_f64;
    for &x in rest {
        let t = sum + x;
        if sum.abs() >= x.abs() {
            compensation += (sum - t) + x;
        } else {
            compensation += (x - t) + sum;
        }
        sum = t;
    }
    // "Avoid losing the sign on a negative result, and don't let adding
    // the compensation convert an infinite or overflowed sum to a NaN."
    if compensation != 0.0 && compensation.is_finite() {
        sum += compensation;
    }
    sum
}

/// numpy's `DOUBLE_pairwise_sum` over `values` (`PW_BLOCKSIZE` 128).
#[must_use]
pub fn numpy_pairwise_sum(values: &[f64]) -> f64 {
    let n = values.len();
    if n < 8 {
        let mut sum = 0.0;
        for &x in values {
            sum += x;
        }
        sum
    } else if n <= 128 {
        let mut r = [0.0_f64; 8];
        r.copy_from_slice(&values[..8]);
        let unrolled = n - n % 8;
        let mut i = 8;
        while i < unrolled {
            for (j, acc) in r.iter_mut().enumerate() {
                *acc += values[i + j];
            }
            i += 8;
        }
        let mut sum = ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]));
        for &x in &values[i..] {
            sum += x;
        }
        sum
    } else {
        let mut half = n / 2;
        half -= half % 8;
        numpy_pairwise_sum(&values[..half]) + numpy_pairwise_sum(&values[half..])
    }
}

/// `round(x, ndigits)` for a finite float and `ndigits >= 0`: the decimal
/// nearest to the exact binary value of `x`, an exact tie to the even last
/// digit. The exact expansion of an `f64` has at most 1074 fraction digits,
/// so formatting with 1100 is exact; the rounding is then done on the
/// digits.
#[must_use]
pub fn py_round(x: f64, ndigits: usize) -> f64 {
    if !x.is_finite() {
        return x;
    }
    let exact = format!("{:.1100}", x.abs());
    let Some((int_part, frac)) = exact.split_once('.') else {
        return x;
    };
    let mut digits: Vec<u8> = int_part.bytes().chain(frac.bytes().take(ndigits)).collect();
    let rest = &frac.as_bytes()[ndigits.min(frac.len())..];
    let first = rest.first().copied().unwrap_or(b'0');
    let tail_nonzero = rest.iter().skip(1).any(|&d| d != b'0');
    let last_odd = digits.last().is_some_and(|d| (d - b'0') % 2 == 1);
    let round_up = first > b'5' || (first == b'5' && (tail_nonzero || last_odd));
    if round_up {
        let mut carry = true;
        for d in digits.iter_mut().rev() {
            if !carry {
                break;
            }
            if *d == b'9' {
                *d = b'0';
            } else {
                *d += 1;
                carry = false;
            }
        }
        if carry {
            digits.insert(0, b'1');
        }
    }
    let split = digits.len() - ndigits;
    let text = format!(
        "{}{}.{}",
        if x.is_sign_negative() { "-" } else { "" },
        String::from_utf8_lossy(&digits[..split]),
        String::from_utf8_lossy(&digits[split..]),
    );
    text.parse::<f64>().unwrap_or(x)
}

/// A count as a float, as Python's `int` → `float` conversion makes it:
/// exact, since every count here is far below 2^53.
#[must_use]
#[allow(clippy::cast_precision_loss)] // counts below 2^53
pub fn count_f64(count: usize) -> f64 {
    count as f64
}

/// `np.array(values, dtype=float)` → `(arr.mean(), arr.std())` (population
/// standard deviation), for non-negative integer values (in-degrees).
///
/// The mean is the exact integer sum over `n` (numpy's pairwise sum of
/// integers below 2^53 is exact, whatever its order); the deviations are
/// squared and summed pairwise as numpy does.
#[must_use]
pub fn numpy_mean_std(values: &[usize]) -> (f64, f64) {
    if values.is_empty() {
        return (f64::NAN, f64::NAN);
    }
    let n = count_f64(values.len());
    let total: f64 = values.iter().map(|&v| count_f64(v)).sum();
    let mean = total / n;
    let squares: Vec<f64> = values
        .iter()
        .map(|&v| {
            let d = count_f64(v) - mean;
            d * d
        })
        .collect();
    let variance = numpy_pairwise_sum(&squares) / n;
    (mean, variance.sqrt())
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;

    #[test]
    fn py_sum_compensates_like_cpython_312() {
        // `CPython` 3.12: sum([0.1] * 10) == 1.0 (a plain loop gives
        // 0.9999999999999999).
        assert_eq!(py_sum(&[0.1; 10]), 1.0);
        let mut plain = 0.0;
        for _ in 0..10 {
            plain += 0.1;
        }
        assert_ne!(plain, 1.0);
        // sum([1e100, 1.0, -1e100, 1.0]) == 2.0 in 3.12.
        assert_eq!(py_sum(&[1e100, 1.0, -1e100, 1.0]), 2.0);
        assert_eq!(py_sum(&[]), 0.0);
    }

    #[test]
    fn pairwise_sum_follows_numpy_blocks() {
        // Below 8: a plain loop. 8..=128: eight lanes. Above: halves.
        let small = [0.1, 0.2, 0.3];
        assert_eq!(numpy_pairwise_sum(&small), (0.1 + 0.2) + 0.3);
        let values: Vec<f64> = (0..300).map(|i| f64::from(i) * 0.1).collect();
        // np.sum(np.arange(300) * 0.1) (numpy 2.5.2)
        assert_eq!(numpy_pairwise_sum(&values), 4485.0);
        let (mean, std) = numpy_mean_std(&[0, 0, 1, 5, 40]);
        assert_eq!(mean, 9.2);
        // np.array([0,0,1,5,40], dtype=float).std()
        assert_eq!(std, 15.509_996_776_273_038);
    }

    #[test]
    fn py_round_is_correctly_rounded_with_ties_to_even() {
        assert_eq!(py_round(0.932_049_999_999, 4), 0.932);
        assert_eq!(py_round(std::f64::consts::LOG2_E, 4), 14427.0 / 10000.0);
        // 0.03125 is exact in binary: a true tie, to even.
        assert_eq!(py_round(0.031_25, 4), 0.0312);
        assert_eq!(py_round(0.031_35, 4), 0.0314); // 0.03135 is above the tie in binary
        // 2.675 is below 2.675 in binary: round(2.675, 2) == 2.67.
        assert_eq!(py_round(2.675, 2), 2.67);
        assert_eq!(py_round(9.999_96, 4), 10.0);
        assert_eq!(py_round(0.0, 4), 0.0);
    }
}
