//! The symbol classes the query tools filter on, from `constants.py`, and
//! `classify_symbol_layer` (SPEC-4).

pub use crate::graph::constants::DOC_SYMBOL_TYPES;

/// `CODE_SYMBOL_TYPES`.
pub const CODE_SYMBOL_TYPES: &[&str] = &[
    "class",
    "interface",
    "struct",
    "enum",
    "trait",
    "function",
    "constant",
    "type_alias",
    "macro",
    "sql_schema",
    "sql_table",
    "sql_view",
    "sql_function",
    "sql_column",
    "sql_index",
    "sql_trigger",
];

/// `PROGRESSIVE_SYMBOL_TYPES = CODE_SYMBOL_TYPES | {'method'}`.
#[must_use]
pub fn is_progressive(symbol_type: &str) -> bool {
    symbol_type == "method" || CODE_SYMBOL_TYPES.contains(&symbol_type)
}

/// `sorted(PROGRESSIVE_SYMBOL_TYPES)`, for the refusal message.
#[must_use]
pub fn progressive_sorted() -> Vec<&'static str> {
    let mut types: Vec<&'static str> = CODE_SYMBOL_TYPES.to_vec();
    types.push("method");
    types.sort_unstable();
    types
}

/// `DOC_SYMBOL_TYPES` membership.
#[must_use]
pub fn is_doc_type(symbol_type: &str) -> bool {
    DOC_SYMBOL_TYPES.contains(&symbol_type)
}

const ENTRY_POINT_NAMES: &[&str] = &[
    "main",
    "run",
    "execute",
    "start",
    "cli",
    "app",
    "serve",
    "setup",
    "bootstrap",
    "entrypoint",
    "entry_point",
];

const INFRA_PATH_SEGMENTS: &[&str] = &[
    "config",
    "configs",
    "configuration",
    "util",
    "utils",
    "utility",
    "utilities",
    "helper",
    "helpers",
    "middleware",
    "logging",
    "logger",
    "common",
    "shared",
    "base",
];

const INTERNAL_PATH_SEGMENTS: &[&str] = &[
    "internal",
    "_internal",
    "private",
    "_private",
    "impl",
    "_impl",
    "detail",
    "_detail",
];

/// `classify_symbol_layer`. The docstring and the public-API suffixes of
/// the Python function decide nothing: every class branch returns
/// `public_api`.
#[must_use]
pub fn classify_symbol_layer(
    symbol_type: &str,
    symbol_name: &str,
    parent_symbol: &str,
    file_path: &str,
) -> &'static str {
    let symbol_type = symbol_type.to_lowercase();
    let name_lower = symbol_name.to_lowercase();
    if matches!(symbol_type.as_str(), "constant" | "macro") {
        return "constant";
    }
    if matches!(symbol_type.as_str(), "struct" | "enum" | "type_alias") {
        return "core_type";
    }
    if symbol_type == "function" {
        if ENTRY_POINT_NAMES.contains(&name_lower.as_str()) {
            return "entry_point";
        }
        for entry in ENTRY_POINT_NAMES {
            if name_lower.starts_with(&format!("{entry}_"))
                || name_lower.ends_with(&format!("_{entry}"))
            {
                return "entry_point";
            }
        }
    }
    let lowered = file_path.to_lowercase().replace('\\', "/");
    let parts: Vec<&str> = lowered.split('/').collect();
    if parts.iter().any(|part| INFRA_PATH_SEGMENTS.contains(part)) {
        return "infrastructure";
    }
    if !parent_symbol.is_empty() {
        return "internal";
    }
    if parts
        .iter()
        .any(|part| INTERNAL_PATH_SEGMENTS.contains(part))
    {
        return "internal";
    }
    "public_api"
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn layers_follow_the_python_priority() {
        assert_eq!(classify_symbol_layer("constant", "X", "m", ""), "constant");
        assert_eq!(classify_symbol_layer("enum", "E", "", ""), "core_type");
        assert_eq!(
            classify_symbol_layer("function", "main", "m", ""),
            "entry_point"
        );
        assert_eq!(
            classify_symbol_layer("function", "run_server", "", ""),
            "entry_point"
        );
        assert_eq!(
            classify_symbol_layer("class", "A", "", "src/utils/a.py"),
            "infrastructure"
        );
        assert_eq!(
            classify_symbol_layer("class", "A", "mod", "src/a.py"),
            "internal"
        );
        assert_eq!(
            classify_symbol_layer("class", "A", "", "x/impl/a.py"),
            "internal"
        );
        assert_eq!(
            classify_symbol_layer("class", "A", "", "src/a.py"),
            "public_api"
        );
        assert_eq!(
            classify_symbol_layer("method", "m", "", "src/a.py"),
            "public_api"
        );
    }

    #[test]
    fn the_supported_types_are_sorted() {
        let sorted = progressive_sorted();
        assert_eq!(sorted.first(), Some(&"class"));
        assert!(sorted.contains(&"method"));
        assert!(is_progressive("method") && !is_progressive("markdown_section"));
    }
}
