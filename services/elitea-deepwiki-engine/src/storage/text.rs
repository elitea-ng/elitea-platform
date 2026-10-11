//! The arithmetic the lexical branch depends on.
//!
//! Each item is a port of one definition in `storage/postgres.py`, kept
//! verbatim because the P0 retrieval fixtures record the legacy numbers:
//! "improving" any of them ends exact parity. (The standalone `'bm25'`
//! branch's tokenizer was removed with the branch, ADR-0031 phase D0.)

/// SQLite FTS5's `bm25()` `k1`, used for the lexical branch's statistics.
pub const FTS_K1: f64 = 1.2;
/// SQLite FTS5's `bm25()` `b`.
pub const FTS_B: f64 = 0.75;

/// The `branch` value of the lexical branch's statistics.
pub const BRANCH_FTS: &str = "fts";

/// The text-search configuration migration 0001 creates. Every SQL
/// statement that names it spells it out (a configuration name is an
/// identifier, not a value); a test checks the migration still creates it.
pub const TS_CONFIG: &str = "deepwiki_porter";

/// `ln(1 + (N - df + 0.5) / (df + 0.5))`, the legacy idf.
///
/// The read path computes it in SQL (the statement `postgres.py` runs); this
/// is the same formula for tests and for callers that score in memory.
#[must_use]
pub fn bm25_idf(doc_count: f64, df: f64) -> f64 {
    (1.0 + (doc_count - df + 0.5) / (df + 0.5)).ln()
}

/// `1 / (1 + exp(rank))`, `unified_db._attach_score_norm`. A non-finite
/// rank maps to 0; an overflowing `exp` saturates to 0, which is where
/// Python's `OverflowError` branch lands for a positive rank.
#[must_use]
pub fn score_norm(fts_rank: f64) -> f64 {
    if !fts_rank.is_finite() {
        return 0.0;
    }
    1.0 / (1.0 + fts_rank.exp())
}

/// Python text-mode reading (`Path.read_text`): `\r\n` and a lone `\r`
/// become `\n`. The migration checksum is taken over the text Python read,
/// not over the bytes on disk.
#[must_use]
pub fn universal_newlines(text: &str) -> std::borrow::Cow<'_, str> {
    if !text.contains('\r') {
        return std::borrow::Cow::Borrowed(text);
    }
    std::borrow::Cow::Owned(text.replace("\r\n", "\n").replace('\r', "\n"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn score_norm_is_the_legacy_logistic() {
        assert!((score_norm(0.0) - 0.5).abs() < 1e-15);
        assert!(score_norm(-2.0) > 0.88);
        assert!(score_norm(f64::NAN).abs() < f64::EPSILON);
        assert!(score_norm(1e6).abs() < f64::EPSILON);
        assert!((score_norm(-1e6) - 1.0).abs() < f64::EPSILON);
    }

    #[test]
    fn idf_matches_the_formula() {
        let expected = (1.0_f64 + (20.0 - 2.0 + 0.5) / (2.0 + 0.5)).ln();
        assert!((bm25_idf(20.0, 2.0) - expected).abs() < f64::EPSILON);
    }

    #[test]
    fn newlines_translate_as_python_text_mode() {
        assert_eq!(universal_newlines("a\r\nb\rc\n"), "a\nb\nc\n");
        assert!(matches!(
            universal_newlines("a\nb"),
            std::borrow::Cow::Borrowed(_)
        ));
    }
}
