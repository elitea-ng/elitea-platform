//! Swift extraction rules on small inline sources, mirroring the Python
//! parser (the full corpus is `tests/kotlin_swift_parity.rs`).

use super::{PATTERNS, parse_source};
use crate::model::{RelationshipType, SymbolType};
use serde_json::json;

#[test]
fn every_pattern_compiles() {
    assert!(PATTERNS.is_some());
}

#[test]
fn types_are_stem_qualified_and_conformance_is_implementation() {
    let result = parse_source(
        "/repo/Models.swift",
        "/// A user.\npublic struct User: Codable, Equatable {\n    let id: Int\n}\n",
    );
    let user = &result.symbols[0];
    assert_eq!(user.symbol_type, SymbolType::Class);
    assert_eq!(user.full_name.as_deref(), Some("Models.User"));
    assert_eq!(user.docstring.as_deref(), Some("A user."));
    assert_eq!(user.visibility.as_deref(), Some("public"));
    assert_eq!(user.metadata["is_struct"], json!(true));
    assert_eq!(user.range.end.line, 4);
    let conforms: Vec<_> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Implementation)
        .map(|r| r.target_symbol.as_str())
        .collect();
    assert_eq!(conforms, ["Codable", "Equatable"]);
}

#[test]
fn an_extension_is_a_class_that_inherits_its_type() {
    let result = parse_source("/repo/E.swift", "extension User: Hashable {\n}\n");
    assert_eq!(result.symbols[0].name, "User_extension");
    let first = &result.relationships[0];
    assert_eq!(
        (first.source_symbol.as_str(), first.target_symbol.as_str()),
        ("User_extension", "User")
    );
    assert_eq!(first.annotations["extension"], json!(true));
    assert_eq!(
        result.relationships[1].annotations["via_extension"],
        json!(true)
    );
}
