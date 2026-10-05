//! Symbol classes and path rules, transcribed from the Python engine's
//! `constants.py` (the single source of truth there, and here).

use regex::{Regex, RegexBuilder};
use std::sync::LazyLock;

/// `ARCHITECTURAL_SYMBOLS` without the documentation types (those are added
/// by [`is_doc_symbol`], as `| DOC_SYMBOL_TYPES` does in Python).
pub const ARCHITECTURAL_SYMBOLS: &[&str] = &[
    "class",
    "interface",
    "struct",
    "enum",
    "trait",
    "function",
    "constant",
    "type_alias",
    "macro",
    "contract",
    "sql_schema",
    "sql_table",
    "sql_view",
    "sql_function",
    "sql_column",
    "sql_index",
    "sql_trigger",
];

/// `DOC_SYMBOL_TYPES`.
pub const DOC_SYMBOL_TYPES: &[&str] = &[
    "markdown_document",
    "markdown_section",
    "restructuredtext_document",
    "asciidoc_document",
    "plaintext_document",
    "document_document",
    "pdf_document",
    "html_document",
    "xml_document",
    "json_document",
    "yaml_document",
    "toml_document",
    "config_document",
    "build_config_document",
    "schema_document",
    "infrastructure_document",
    "script_document",
    "text_chunk",
    "text_document",
];

/// The legacy documentation types the index also treats as documents.
const LEGACY_DOC_TYPES: &[&str] = &["module_doc", "file_doc"];

/// `unified_db._is_doc_symbol`.
#[must_use]
pub fn is_doc_symbol(symbol_type: &str) -> bool {
    DOC_SYMBOL_TYPES.contains(&symbol_type)
        || LEGACY_DOC_TYPES.contains(&symbol_type)
        || symbol_type.ends_with("_document")
        || symbol_type.ends_with("_section")
        || symbol_type.ends_with("_chunk")
}

/// `_TEST_PATH_PATTERNS`, case-insensitive, in order.
static TEST_PATH_PATTERNS: LazyLock<Vec<Regex>> = LazyLock::new(|| {
    [
        r"(^|/)tests?/",
        r"(^|/)__tests__/",
        r"(^|/)specs?/",
        r"(^|/)testing/",
        r"(^|/)test_?helpers?/",
        r"(^|/)fixtures/",
        r"(^|/)mocks?/",
        r"(^|/)testdata/",
        r"(^|/)testutils?/",
        r"(^|/)test_[^/]+\.\w+$",
        r"(^|/)[^/]+_test\.\w+$",
        r"(^|/)[^/]+\.test\.\w+$",
        r"(^|/)[^/]+\.spec\.\w+$",
        r"(^|/)[^/]+Tests?\.\w+$",
        r"(^|/)conftest\.py$",
        r"(^|/)setup_tests?\.\w+$",
    ]
    .iter()
    .map(|pattern| {
        RegexBuilder::new(pattern)
            .case_insensitive(true)
            .build()
            .unwrap_or_else(|_| unreachable!("a constant pattern compiles"))
    })
    .collect()
});

/// `is_test_path`.
#[must_use]
pub fn is_test_path(rel_path: &str) -> bool {
    TEST_PATH_PATTERNS.iter().any(|p| p.is_match(rel_path))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_paths_follow_the_python_patterns() {
        for path in [
            "tests/a.py",
            "x/__tests__/b.js",
            "pkg/foo_test.go",
            "web/a.spec.ts",
            "src/FooTest.java",
            "conftest.py",
            "a/Mocks/b.go",
        ] {
            assert!(is_test_path(path), "{path}");
        }
        for path in ["src/main.rs", "attestation/x.go", "latest/x.py"] {
            assert!(!is_test_path(path), "{path}");
        }
        // The Python rule `[^/]+Tests?\.\w+$` is case-insensitive, so it
        // marks "conTEST.py" too. Kept: is_test feeds the index as it is.
        assert!(is_test_path("contest.py"));
    }

    #[test]
    fn documentation_types_include_the_suffix_rules() {
        assert!(is_doc_symbol("markdown_section"));
        assert!(is_doc_symbol("anything_chunk"));
        assert!(is_doc_symbol("module_doc"));
        assert!(!is_doc_symbol("class"));
    }
}
