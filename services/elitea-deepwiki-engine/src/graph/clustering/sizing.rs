//! The size formulas of Phase 3: the section-pass resolution and the
//! consolidation targets.
//!
//! The same IEEE operations as Python (`math.log10`, `math.log2`,
//! `math.sqrt`, `math.ceil` on `f64`), so the targets are equal at every
//! size, including the boundaries where a ceiling flips.

/// `auto_resolution`: `γ = max(0.3, 1 − 0.2·log10(n))`, `1.0` below 2.
///
/// WHY a size-dependent γ: at γ = 1 a small repository splits into one
/// section per file. The input is the FILE count (A.10.1), because the
/// section pass runs on the file graph.
#[must_use]
pub fn auto_resolution(count: usize) -> f64 {
    if count < 2 {
        return 1.0;
    }
    #[allow(clippy::cast_precision_loss)] // counts are far below 2^52
    let n = count as f64;
    f64::max(0.3, 1.0 - 0.2 * n.log10())
}

/// `_target_section_count`: `clamp(5, 20, ceil(1.2·log2(files)))`, 5 below
/// 10 files.
///
/// WHY logarithmic in files: a repository has a bounded number of major
/// subsystems, whatever its size.
#[must_use]
pub fn target_section_count(files: usize) -> usize {
    if files < 10 {
        return 5;
    }
    #[allow(clippy::cast_precision_loss)]
    let n = files.max(10) as f64;
    clamp_ceil(1.2 * n.log2(), 5, 20)
}

/// `_target_total_pages`: `clamp(8, 200, ceil(sqrt(nodes / 7)))`.
///
/// WHY square-root in nodes: pages follow code volume, and a different
/// input and growth rate than sections keeps the two counts independent.
#[must_use]
pub fn target_total_pages(nodes: usize) -> usize {
    #[allow(clippy::cast_precision_loss)]
    let n = nodes as f64;
    clamp_ceil((n / 7.0).sqrt(), 8, 200)
}

/// `max(low, min(high, ceil(x)))` for a finite, non-negative `x`.
fn clamp_ceil(x: f64, low: usize, high: usize) -> usize {
    #[allow(clippy::cast_precision_loss)]
    let capped = x.ceil().min(high as f64);
    #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
    let value = capped as usize; // in [0, high]
    value.max(low)
}

/// `_dir_of_node` and the isolated-file rule: the part of `path` before its
/// last `/`, or `<root>`.
#[must_use]
pub fn dir_of(path: &str) -> &str {
    path.rsplit_once('/').map_or("<root>", |(dir, _)| dir)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn auto_resolution_matches_python() {
        // Values from Python's auto_resolution.
        assert!((auto_resolution(0) - 1.0).abs() < f64::EPSILON);
        assert!((auto_resolution(1) - 1.0).abs() < f64::EPSILON);
        assert_eq!(
            auto_resolution(101).to_bits(),
            0.599_135_725_243_471_5_f64.to_bits()
        );
        assert_eq!(
            auto_resolution(127).to_bits(),
            0.579_239_255_808_808_6_f64.to_bits()
        );
        assert!((auto_resolution(1_000_000) - 0.3).abs() < f64::EPSILON);
    }

    #[test]
    fn section_targets_follow_the_docstring_table() {
        for (files, target) in [
            (0, 5),
            (9, 5),
            (10, 5),
            (50, 7),
            (116, 9),
            (127, 9),
            (500, 11),
            (1000, 12),
            (5000, 15),
            (7165, 16),
            (50_000, 19),
            (10_000_000, 20),
        ] {
            assert_eq!(target_section_count(files), target, "{files} files");
        }
    }

    #[test]
    fn page_targets_follow_the_docstring_table() {
        for (nodes, target) in [
            (0, 8),
            (521, 9),
            (729, 11),
            (5000, 27),
            (11_417, 41),
            (50_000, 85),
            (146_091, 145),
            (280_000, 200),
            (10_000_000, 200),
        ] {
            assert_eq!(target_total_pages(nodes), target, "{nodes} nodes");
        }
    }

    #[test]
    fn dir_of_splits_at_the_last_slash() {
        assert_eq!(dir_of("a/b/c.py"), "a/b");
        assert_eq!(dir_of("c.py"), "<root>");
        assert_eq!(dir_of("/c.py"), "");
        assert_eq!(dir_of(""), "<root>");
    }
}
