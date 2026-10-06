//! Symbol heuristics of the graph builder that do NOT shape the graph.
//!
//! The Python builder uses these only when it turns parse results into
//! vector-store documents (`_iter_symbol_chunks`): which nested symbols are
//! embedded, and the C++ metadata of a chunk. They are ported here with the
//! builder so the document pass can use them; the graph gate does not
//! exercise them, so the unit tests carry the parity.
//!
//! Python's `str.islower()` / `isupper()` / `isalpha()` / `isdigit()` are
//! approximated with Rust's Unicode properties; they agree on identifiers
//! in practice.

use crate::parsers::model::{ParseResult, Symbol};
use serde_json::{Map, Value};

/// `_get_type_priority`: architectural significance, highest first. Used
/// to pick one node when several share a name.
#[must_use]
pub fn type_priority(symbol_type: &str) -> u8 {
    match symbol_type {
        "class" | "interface" | "trait" | "protocol" => 10,
        "enum" | "struct" | "record" | "data_class" | "object" => 9,
        "module" | "namespace" => 8,
        "function" => 7,
        "constant" | "type_alias" | "annotation" | "decorator" | "macro" => 6,
        "method" => 3,
        "constructor" | "field" | "property" => 2,
        "parameter" | "variable" | "local_variable" | "argument" => 1,
        _ => 0,
    }
}

fn is_cased(c: char) -> bool {
    c.is_lowercase() || c.is_uppercase()
}

/// `str.islower()`: at least one cased character, none upper-case.
fn py_islower(text: &str) -> bool {
    text.chars().any(is_cased) && !text.chars().any(char::is_uppercase)
}

fn py_isdigit(text: &str) -> bool {
    !text.is_empty() && text.chars().all(char::is_numeric)
}

/// Package / namespace prefixes of `_is_package_or_namespace_parent`.
const PACKAGE_PREFIXES: &[&str] = &[
    "com",
    "org",
    "net",
    "io",
    "java",
    "javax",
    "android",
    "kotlin",
    "scala",
    "system",
    "microsoft",
    "windows",
    "web",
    "data",
    "collections",
    "app",
    "api",
    "core",
    "common",
    "shared",
    "lib",
    "framework",
    "utils",
];

/// Single-word namespaces of `_is_package_or_namespace_parent`.
const SINGLE_WORD_NAMESPACES: &[&str] = &[
    "system",
    "microsoft",
    "windows",
    "collections",
    "generic",
    "linq",
    "main",
    "fmt",
    "http",
    "json",
    "time",
    "context",
    "sync",
    "os",
    "io",
    "std",
    "core",
    "alloc",
    "tokio",
    "serde",
    "clap",
    "regex",
    "react",
    "express",
    "lodash",
    "moment",
    "axios",
];

/// `_is_package_or_namespace_parent`: whether `parent_symbol` names a
/// package / namespace (the symbol is top level) rather than a class or
/// function (the symbol is nested).
#[must_use]
pub fn is_package_or_namespace_parent<'a>(
    parent_symbol: Option<&str>,
    parse_results: impl IntoIterator<Item = &'a ParseResult>,
) -> bool {
    let Some(parent) = parent_symbol.filter(|p| !p.is_empty()) else {
        return true;
    };
    // Strategy 1: a namespace / module / package / crate symbol of that name.
    let declared = parse_results.into_iter().any(|result| {
        result.symbols.iter().any(|symbol| {
            symbol.name == parent
                && matches!(
                    symbol.symbol_type.as_str(),
                    "namespace" | "module" | "package" | "crate"
                )
        })
    });
    if declared {
        return true;
    }
    // Strategy 2: a dotted, lower-case name under a well-known prefix.
    if parent.contains('.') {
        let parts: Vec<&str> = parent.split('.').collect();
        let looks_like_package = parts.iter().all(|part| {
            py_islower(part)
                || py_isdigit(part)
                || part.contains('_')
                || py_islower(&part.replace('_', ""))
        });

        if looks_like_package && PACKAGE_PREFIXES.contains(&parts[0].to_lowercase().as_str()) {
            return true;
        }
    }
    // Strategy 3: well-known single-word namespaces.

    if SINGLE_WORD_NAMESPACES.contains(&parent.to_lowercase().as_str()) {
        return true;
    }
    // Strategy 4: two or more class-name indicators mean a nested symbol.
    let first_upper = parent.chars().next().is_some_and(char::is_uppercase);
    let lowered = parent.to_lowercase();
    let indicators = [
        first_upper,
        ["class", "interface", "struct", "enum", "trait", "impl"]
            .iter()
            .any(|s| lowered.contains(s)),
        first_upper
            && parent.chars().skip(1).any(char::is_uppercase)
            && !parent.contains('.')
            && !parent.contains('_'),
        parent.contains('<') && parent.contains('>'),
    ];
    if indicators.iter().filter(|&&i| i).count() >= 2 {
        return false;
    }
    // Strategies 5 and the default all answer "package": a Go-style or
    // Rust-style lower-case name, and anything else, are kept.
    true
}

/// `_is_java_standard_library_reference`.
#[must_use]
pub fn is_java_standard_library_reference(target_symbol: &str) -> bool {
    const PACKAGES: &[&str] = &[
        "java.lang",
        "java.util",
        "java.io",
        "java.nio",
        "java.time",
        "java.math",
        "java.net",
        "java.text",
        "java.util.regex",
        "java.util.concurrent",
        "java.util.function",
        "java.util.stream",
        "java.security",
        "java.awt",
        "javax.swing",
        "javax.annotation",
    ];
    const BUILTIN_METHODS: &[&str] = &[
        "isEmpty",
        "trim",
        "length",
        "contains",
        "add",
        "remove",
        "get",
        "set",
        "toString",
        "equals",
        "hashCode",
        "getClass",
        "format",
        "valueOf",
        "randomUUID",
        "now",
        "hash",
        "compile",
        "matches",
        "matcher",
        "orElseThrow",
        "isPresent",
        "of",
        "empty",
    ];
    if !target_symbol.contains('.') {
        return false;
    }
    PACKAGES
        .iter()
        .any(|p| target_symbol.starts_with(&format!("{p}.")))
        || target_symbol
            .rsplit('.')
            .next()
            .is_some_and(|m| BUILTIN_METHODS.contains(&m))
}

/// `_extract_cpp_metadata`: the C++ parser's `key=value` comments.
#[must_use]
pub fn extract_cpp_metadata(symbol: &Symbol) -> Map<String, Value> {
    let mut metadata = Map::new();
    for comment in &symbol.comments {
        let value_of = |prefix: &str| comment.strip_prefix(prefix).map(str::to_owned);
        if let Some(value) = value_of("namespaces=") {
            metadata.insert("namespaces".into(), Value::String(value));
        } else if let Some(value) = value_of("template_params=") {
            metadata.insert("template_params".into(), Value::String(value));
            metadata.insert("is_template".into(), Value::Bool(true));
        } else if let Some(value) = value_of("template_template_params=") {
            metadata.insert("template_template_params".into(), Value::String(value));
        } else if comment == "final_class" {
            metadata.insert("is_final".into(), Value::Bool(true));
        } else if comment == "scoped_enum" {
            metadata.insert("is_scoped_enum".into(), Value::Bool(true));
        } else if let Some(value) = value_of("const_kind=") {
            metadata.insert("const_kind".into(), Value::String(value));
        } else if comment == "lambda" {
            metadata.insert("is_lambda".into(), Value::Bool(true));
        }
    }
    metadata
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::parsers::model::{Range, Scope, SymbolType};

    #[test]
    fn package_parents_follow_the_python_strategies() {
        let mut result = ParseResult::new("/r/a.cs", "csharp");
        result.symbols.push(Symbol::new(
            "Acme.Billing",
            SymbolType::Namespace,
            Scope::Global,
            Range::default(),
            "/r/a.cs",
        ));
        let results = [result];
        assert!(is_package_or_namespace_parent(None, &results));
        assert!(is_package_or_namespace_parent(
            Some("Acme.Billing"),
            &results
        ));
        assert!(is_package_or_namespace_parent(
            Some("com.example.app"),
            &results
        ));
        assert!(is_package_or_namespace_parent(Some("fmt"), &results));
        assert!(!is_package_or_namespace_parent(
            Some("UserService"),
            &results
        ));
        assert!(!is_package_or_namespace_parent(Some("List<T>"), &results));
        assert!(is_package_or_namespace_parent(Some("handlers"), &results));
    }

    #[test]
    fn java_standard_library_references() {
        assert!(is_java_standard_library_reference("java.util.List"));
        assert!(is_java_standard_library_reference("name.trim"));
        assert!(!is_java_standard_library_reference("trim"));
        assert!(!is_java_standard_library_reference("svc.Billing.charge"));
    }

    #[test]
    fn cpp_metadata_comes_from_comments() {
        let mut symbol = Symbol::new(
            "V",
            SymbolType::Class,
            Scope::Global,
            Range::default(),
            "/r/v.h",
        );
        symbol.comments = vec![
            "namespaces=a::b".into(),
            "template_params=T".into(),
            "final_class".into(),
        ];
        let metadata = extract_cpp_metadata(&symbol);
        assert_eq!(metadata["namespaces"], "a::b");
        assert_eq!(metadata["is_template"], true);
        assert_eq!(metadata["is_final"], true);
        assert_eq!(type_priority("class"), 10);
        assert_eq!(type_priority("weird"), 0);
    }
}
