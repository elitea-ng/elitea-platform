//! Kotlin extraction rules on small inline sources, mirroring the Python
//! parser (the full corpus is `tests/kotlin_swift_parity.rs`).

use super::{PATTERNS, parse_source};
use crate::model::{RelationshipType, SymbolType};
use serde_json::json;

#[test]
fn every_pattern_compiles() {
    assert!(PATTERNS.is_some());
}

#[test]
fn classes_are_package_qualified_with_their_kind() {
    let result = parse_source(
        "/repo/Shapes.kt",
        "package a.b\n\n/** A shape. */\nsealed class Shape {}\ndata class Dot(val x: Int) : Shape() {}\nenum class Color { RED }\n",
    );
    let names: Vec<_> = result
        .symbols
        .iter()
        .map(|s| (s.name.as_str(), s.symbol_type))
        .collect();
    assert_eq!(names[0], ("Shape", SymbolType::Class));
    assert_eq!(names[2], ("Color", SymbolType::Enum));
    let shape = &result.symbols[0];
    assert_eq!(shape.full_name.as_deref(), Some("a.b.Shape"));
    assert_eq!(shape.docstring.as_deref(), Some("A shape."));
    assert_eq!(shape.metadata["is_sealed"], json!(true));
    assert_eq!(shape.metadata["is_exported"], json!(false));
    assert_eq!(result.symbols[1].metadata["is_data_class"], json!(true));
    let inherits: Vec<_> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Inheritance)
        .map(|r| (r.source_symbol.as_str(), r.target_symbol.as_str()))
        .collect();
    assert_eq!(inherits, [("Dot", "Shape")]);
}

#[test]
fn constructor_calls_are_aggregation_edges() {
    let result = parse_source(
        "/repo/Main.kt",
        "fun main() {\n    val p = Point(1, 2)\n}\n",
    );
    let uses: Vec<_> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Aggregation)
        .map(|r| (r.source_symbol.as_str(), r.target_symbol.as_str()))
        .collect();
    assert_eq!(uses, [("Main", "Point")]);
    let main = &result.symbols[0];
    assert_eq!(main.range.start.line, 1);
    assert_eq!(main.range.end.line, 3);
}
