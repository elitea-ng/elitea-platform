//! The tokenizer and the arithmetic the BM25 branches depend on.
//!
//! Each item is a port of one definition in `storage/postgres.py` or
//! `storage/publish.py`, kept verbatim because the P0 retrieval fixtures
//! record the legacy numbers: "improving" any of them ends exact BM25
//! parity.

use crate::graph::pystr;
use crate::storage::rows::IndexNode;

/// The standalone BM25 index's `k1` (`bm25_disk.py`).
pub const BM25_K1: f64 = 1.5;
/// The standalone BM25 index's `b` (`bm25_disk.py`).
pub const BM25_B: f64 = 0.75;
/// SQLite FTS5's `bm25()` `k1`, used for the lexical branch's statistics.
pub const FTS_K1: f64 = 1.2;
/// SQLite FTS5's `bm25()` `b`.
pub const FTS_B: f64 = 0.75;

/// The `branch` value of the standalone BM25 statistics.
pub const BRANCH_BM25: &str = "bm25";
/// The `branch` value of the lexical branch's statistics.
pub const BRANCH_FTS: &str = "fts";

/// The text-search configuration migration 0001 creates. Every SQL
/// statement that names it spells it out (a configuration name is an
/// identifier, not a value); a test checks the migration still creates it.
pub const TS_CONFIG: &str = "deepwiki_porter";

/// The longest `'bm25'` term (in UTF-8 bytes) that gets a posting.
///
/// `wiki_bm25_terms` and `wiki_bm25_postings` (migration 0001) have the
/// term in their B-tree keys, and a B-tree entry cannot exceed about 2.7 kB.
/// Source code has whitespace-free runs far longer than that (a minified
/// line, an embedded base64 blob: the elitea-platform corpus has one of
/// 108 kB), and one of them failed the whole publish. Such a token still
/// counts in its document's length, so `avgdl` and every length
/// normalisation stay exact; it only gets no posting, so a query naming a
/// term over 1 kB finds nothing for it. The legacy SQLite index had no
/// limit; this is a deliberate difference, and the Python publisher failed
/// on the same token.
pub const MAX_TERM_BYTES: usize = 1024;

/// The legacy BM25 tokenizer (`whitespace_tokens`): Python's `str.split()`,
/// which also splits on U+001C..U+001F, unlike Rust's `split_whitespace`.
/// No lowercasing and no punctuation stripping.
pub fn whitespace_tokens(text: &str) -> impl Iterator<Item = &str> {
    pystr::split_whitespace(text)
}

/// The BM25 document text of a node (`publish._document_text`): the
/// non-empty parts of name, signature, docstring and source, joined with
/// `"\n"`.
///
/// The legacy index was built over the docstore, whose documents the engine
/// built from these same fields. `publish.py` states the assumption; this
/// keeps it.
#[must_use]
pub fn document_text(node: &IndexNode) -> String {
    let parts = [
        node.symbol_name.as_str(),
        node.signature.as_str(),
        node.docstring.as_str(),
        node.source_text.as_str(),
    ];
    let mut text = String::new();
    for part in parts.into_iter().filter(|part| !part.is_empty()) {
        if !text.is_empty() {
            text.push('\n');
        }
        text.push_str(part);
    }
    text
}

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
    fn tokens_split_as_python_str_split() {
        let tokens: Vec<&str> = whitespace_tokens("  a\tb\u{1c}c\u{a0}d\n\ne.f ").collect();
        assert_eq!(tokens, ["a", "b", "c", "d", "e.f"]);
        assert_eq!(whitespace_tokens(" \n\t ").count(), 0);
    }

    #[test]
    fn document_text_skips_empty_parts() {
        let node = IndexNode {
            node_id: "n".into(),
            symbol_name: "name".into(),
            docstring: "doc".into(),
            source_text: "src".into(),
            ..IndexNode::default()
        };
        assert_eq!(document_text(&node), "name\ndoc\nsrc");
        assert_eq!(document_text(&IndexNode::default()), "");
    }

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
