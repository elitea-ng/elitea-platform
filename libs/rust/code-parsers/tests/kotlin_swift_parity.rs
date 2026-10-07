//! The Kotlin and Swift parsers against the Inventory engine's Python
//! regex parsers, on the sample files in `tests/fixtures/{kotlin,swift}`.
//!
//! `expected.json` in each directory is what the Python parser's
//! `parse_file` returned for every sample (plus a missing file), written by
//! that directory's `generate.py`. Every field is compared, in order. The
//! Python model is the Inventory one, not this crate's, so a few fields
//! map instead of matching by name — each mapping is asserted here, never
//! filtered out:
//!
//! * `Symbol.is_exported` is `metadata["is_exported"]`, so the Rust
//!   metadata is the Python metadata plus that one key;
//! * `Relationship.metadata` is `annotations`;
//! * `Relationship.is_cross_file` (always `false`) has no field; the Rust
//!   `target_file` is `None` as the Python one is;
//! * `rel_type` `uses` (no such wire value here) is `aggregation`, which
//!   the Inventory ingestion maps to the same `uses` relation;
//! * fields only the crate's model has keep their defaults (`is_abstract`,
//!   `comments`, `weight`, `is_direct`).
//!
//! File paths are compared relative to the fixture directory (the generator
//! parses from there; the Rust parser is handed absolute paths).

use elitea_code_parsers::model::{ParseResult, Relationship, Symbol};
use elitea_code_parsers::parser_for;
use serde_json::{Map, Value, json};
use std::collections::BTreeMap;
use std::path::PathBuf;

fn fixture_dir(language: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(language)
}

fn expected(language: &str) -> BTreeMap<String, Value> {
    let path = fixture_dir(language).join("expected.json");
    let text = std::fs::read_to_string(&path).unwrap_or_default();
    let golden: Value = serde_json::from_str(&text).unwrap_or_default();
    assert_eq!(golden["language"], json!(language), "{}", path.display());
    golden["files"]
        .as_object()
        .map(|files| files.clone().into_iter().collect())
        .unwrap_or_default()
}

fn relative(text: &str, prefix: &str) -> String {
    text.replace(prefix, "")
}

fn compare_symbol(file: &str, index: usize, rust: &Symbol, python: &Value, prefix: &str) {
    let at = format!("{file} symbol #{index} ({})", rust.name);
    assert_eq!(json!(rust.name), python["name"], "{at}: name");
    assert_eq!(
        json!(rust.symbol_type.as_str()),
        python["symbol_type"],
        "{at}: symbol_type"
    );
    assert_eq!(json!(rust.scope.as_str()), python["scope"], "{at}: scope");
    assert_eq!(
        serde_json::to_value(rust.range).unwrap_or_default(),
        python["range"],
        "{at}: range"
    );
    assert_eq!(
        json!(relative(&rust.file_path, prefix)),
        python["file_path"],
        "{at}: file_path"
    );
    assert_eq!(
        json!(rust.parent_symbol),
        python["parent_symbol"],
        "{at}: parent"
    );
    assert_eq!(
        json!(rust.full_name),
        python["full_name"],
        "{at}: full_name"
    );
    assert_eq!(
        json!(rust.visibility),
        python["visibility"],
        "{at}: visibility"
    );
    assert_eq!(
        json!(rust.is_static),
        python["is_static"],
        "{at}: is_static"
    );
    assert_eq!(json!(rust.is_async), python["is_async"], "{at}: is_async");
    assert_eq!(
        json!(rust.docstring),
        python["docstring"],
        "{at}: docstring"
    );
    assert_eq!(
        json!(rust.return_type),
        python["return_type"],
        "{at}: return_type"
    );
    assert_eq!(
        json!(rust.parameter_types),
        python["parameter_types"],
        "{at}: parameter_types"
    );
    assert_eq!(
        json!(rust.source_text),
        python["source_text"],
        "{at}: source_text"
    );
    assert_eq!(
        json!(rust.signature),
        python["signature"],
        "{at}: signature"
    );
    // Mapped: `is_exported` lives in the metadata, as its last key.
    let mut metadata: Map<String, Value> =
        python["metadata"].as_object().cloned().unwrap_or_default();
    metadata.insert("is_exported".to_owned(), python["is_exported"].clone());
    let rust_keys: Vec<&String> = rust.metadata.keys().collect();
    let python_keys: Vec<&String> = metadata.keys().collect();
    assert_eq!(rust_keys, python_keys, "{at}: metadata key order");
    assert_eq!(rust.metadata, metadata, "{at}: metadata");
    // Rust-only fields keep their defaults.
    assert!(!rust.is_abstract, "{at}: is_abstract");
    assert!(rust.comments.is_empty(), "{at}: comments");
}

fn compare_relationship(
    file: &str,
    index: usize,
    rust: &Relationship,
    python: &Value,
    prefix: &str,
) {
    let at = format!(
        "{file} relationship #{index} ({} -> {})",
        rust.source_symbol, rust.target_symbol
    );
    assert_eq!(
        json!(rust.source_symbol),
        python["source_symbol"],
        "{at}: source"
    );
    assert_eq!(
        json!(rust.target_symbol),
        python["target_symbol"],
        "{at}: target"
    );
    // Mapped: Python `uses` is `aggregation`.
    let python_type = python["relationship_type"].as_str().unwrap_or_default();
    let expected_type = if python_type == "uses" {
        "aggregation"
    } else {
        python_type
    };
    assert_eq!(
        rust.relationship_type.as_str(),
        expected_type,
        "{at}: relationship_type"
    );
    assert_eq!(
        json!(relative(&rust.source_file, prefix)),
        python["source_file"],
        "{at}: source_file"
    );
    assert_eq!(
        json!(rust.target_file),
        python["target_file"],
        "{at}: target_file"
    );
    // Mapped: no `is_cross_file`; the Python one is always false here.
    assert_eq!(python["is_cross_file"], json!(false), "{at}: is_cross_file");
    assert_eq!(
        serde_json::to_value(rust.source_range).unwrap_or_default(),
        python["source_range"],
        "{at}: source_range"
    );
    assert_eq!(
        json!(rust.confidence),
        python["confidence"],
        "{at}: confidence"
    );
    assert_eq!(json!(rust.context), python["context"], "{at}: context");
    // Mapped: Python `metadata` is `annotations`.
    assert_eq!(
        Value::Object(rust.annotations.clone()),
        python["metadata"],
        "{at}: annotations"
    );
    // Rust-only fields keep their defaults.
    assert!(
        (rust.weight - 1.0).abs() < f64::EPSILON && rust.is_direct,
        "{at}: weight/is_direct"
    );
}

fn compare_result(file: &str, rust: &ParseResult, python: &Value, prefix: &str) {
    assert_eq!(
        json!(relative(&rust.file_path, prefix)),
        python["file_path"],
        "{file}: file_path"
    );
    assert_eq!(json!(rust.language), python["language"], "{file}: language");
    assert_eq!(json!(rust.imports), python["imports"], "{file}: imports");
    assert_eq!(json!(rust.exports), python["exports"], "{file}: exports");
    assert_eq!(
        json!(rust.dependencies),
        python["dependencies"],
        "{file}: dependencies"
    );
    assert_eq!(
        json!(rust.module_docstring),
        python["module_docstring"],
        "{file}: module_docstring"
    );
    let errors: Vec<String> = rust.errors.iter().map(|e| relative(e, prefix)).collect();
    assert_eq!(json!(errors), python["errors"], "{file}: errors");
    assert_eq!(json!(rust.warnings), python["warnings"], "{file}: warnings");

    let symbols = python["symbols"].as_array().cloned().unwrap_or_default();
    for (index, (r, p)) in rust.symbols.iter().zip(&symbols).enumerate() {
        compare_symbol(file, index, r, p, prefix);
    }
    assert_eq!(rust.symbols.len(), symbols.len(), "{file}: symbol count");

    let relationships = python["relationships"]
        .as_array()
        .cloned()
        .unwrap_or_default();
    for (index, (r, p)) in rust.relationships.iter().zip(&relationships).enumerate() {
        compare_relationship(file, index, r, p, prefix);
    }
    assert_eq!(
        rust.relationships.len(),
        relationships.len(),
        "{file}: relationship count"
    );
}

/// Parse every golden file with the Rust parser and compare; returns how
/// many symbols and `uses` edges were compared, so the corpus cannot shrink
/// to nothing unnoticed.
fn check(language: &str) -> (usize, usize) {
    let golden = expected(language);
    assert!(golden.len() >= 4, "{language}: golden corpus is missing");
    let dir = fixture_dir(language);
    let prefix = format!("{}/", dir.to_string_lossy());
    let files: Vec<String> = golden
        .keys()
        .map(|name| dir.join(name).to_string_lossy().into_owned())
        .collect();
    let parser = parser_for(language);
    assert!(parser.is_some(), "{language}: no parser");
    let Some(parser) = parser else {
        return (0, 0);
    };
    assert_eq!(parser.language(), language);
    let results = parser.parse_files(&files);
    assert_eq!(results.len(), golden.len());
    let (mut symbols, mut uses) = (0, 0);
    for (name, python) in &golden {
        let path = dir.join(name).to_string_lossy().into_owned();
        let rust = results.get(&path);
        assert!(rust.is_some(), "{name}: not parsed");
        let Some(rust) = rust else { continue };
        compare_result(name, rust, python, &prefix);
        symbols += rust.symbols.len();
        uses += python["relationships"].as_array().map_or(0, |rels| {
            rels.iter()
                .filter(|r| r["relationship_type"] == json!("uses"))
                .count()
        });
    }
    (symbols, uses)
}

#[test]
fn kotlin_matches_the_python_parser() {
    let (symbols, uses) = check("kotlin");
    assert!(symbols >= 40, "kotlin: only {symbols} symbols compared");
    assert!(
        uses > 0,
        "kotlin: the uses -> aggregation mapping is unexercised"
    );
}

#[test]
fn swift_matches_the_python_parser() {
    let (symbols, uses) = check("swift");
    assert!(symbols >= 40, "swift: only {symbols} symbols compared");
    assert!(
        uses > 0,
        "swift: the uses -> aggregation mapping is unexercised"
    );
}
