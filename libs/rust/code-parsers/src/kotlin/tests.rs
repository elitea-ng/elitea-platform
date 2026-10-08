//! Kotlin extraction rules on small inline sources (the full corpus is
//! `tests/kotlin_swift_golden.rs`).

use super::parse_source;
use crate::model::{RelationshipType, SymbolType};
use serde_json::json;

fn edges(source: &str, kind: RelationshipType) -> Vec<(String, String)> {
    parse_source("/repo/Main.kt", source)
        .relationships
        .into_iter()
        .filter(|r| r.relationship_type == kind)
        .map(|r| (r.source_symbol, r.target_symbol))
        .collect()
}

fn pairs(list: &[(&str, &str)]) -> Vec<(String, String)> {
    list.iter()
        .map(|(a, b)| ((*a).to_owned(), (*b).to_owned()))
        .collect()
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
        "fun main() {\n    val p = Point(1, 2)\n    draw(p)\n}\n",
    );
    let uses = edges(
        "fun main() {\n    val p = Point(1, 2)\n    draw(p)\n}\n",
        RelationshipType::Aggregation,
    );
    assert_eq!(uses, pairs(&[("Main", "Point")]));
    let calls = edges(
        "fun main() {\n    val p = Point(1, 2)\n    draw(p)\n}\n",
        RelationshipType::Calls,
    );
    assert_eq!(calls, pairs(&[("Main", "draw")]));
    // The local `p` is not a symbol.
    assert_eq!(result.symbols.len(), 1);
    let main = &result.symbols[0];
    assert_eq!(main.range.start.line, 1);
    assert_eq!(main.range.end.line, 4);
}

#[test]
fn comments_and_strings_produce_nothing() {
    let source = concat!(
        "// class Ghost : Base()\n",
        "/* fun phantom() = Spook(1)\n   @Haunted val x = 1 */\n",
        "/**\n * @property id not an annotation\n */\n",
        "val s = \"class InString { fun f() = Make(2) } @Fake\"\n",
        "val t = \"\"\"\n    interface Raw\n    call(3)\n\"\"\"\n",
    );
    let result = parse_source("/repo/Main.kt", source);
    let names: Vec<_> = result.symbols.iter().map(|s| s.name.as_str()).collect();
    assert_eq!(names, ["s", "t"]);
    assert!(
        result.relationships.is_empty(),
        "{:?}",
        result.relationships
    );
}

#[test]
fn multi_line_declarations_parse() {
    let source = concat!(
        "package p\n",
        "@Serializable\n",
        "data class Order(\n",
        "    val id: String,\n",
        "    val note: String? = null,\n",
        ") : Base(\n",
        "    \"x\"\n",
        "),\n",
        "    Comparable<Order> {\n",
        "    override suspend fun total(\n",
        "        a: Int,\n",
        "        b: Int,\n",
        "    ): Long =\n",
        "        a.toLong() + b\n",
        "}\n",
    );
    let result = parse_source("/repo/Order.kt", source);
    let order = &result.symbols[0];
    assert_eq!(order.name, "Order");
    assert_eq!((order.range.start.line, order.range.end.line), (2, 15));
    let total = &result.symbols[1];
    assert_eq!(total.name, "total");
    assert_eq!(total.parent_symbol.as_deref(), Some("p.Order"));
    assert_eq!(total.return_type.as_deref(), Some("Long"));
    assert!(total.is_async);
    assert_eq!(total.metadata["modifiers"], json!(["override", "suspend"]));
    assert_eq!((total.range.start.line, total.range.end.line), (10, 14));
    let supertypes: Vec<_> = result
        .relationships
        .iter()
        .filter(|r| {
            matches!(
                r.relationship_type,
                RelationshipType::Inheritance | RelationshipType::Implementation
            )
        })
        .map(|r| (r.target_symbol.as_str(), r.relationship_type))
        .collect();
    assert_eq!(
        supertypes,
        [
            ("Base", RelationshipType::Inheritance),
            ("Comparable", RelationshipType::Implementation)
        ]
    );
    assert_eq!(
        edges(source, RelationshipType::Decorates),
        pairs(&[("Main", "Serializable")])
    );
}

#[test]
fn imports_delegation_and_extensions() {
    let source = concat!(
        "import a.b.*\nimport c.D as E\n",
        "class Box(private val inner: List<Int>) : List<Int> by inner\n",
        "fun String.slug(): String = lowercase()\n",
        "val Int.twice: Int get() = this * 2\n",
    );
    let result = parse_source("/repo/Box.kt", source);
    let imports: Vec<_> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Imports)
        .map(|r| (r.target_symbol.as_str(), r.annotations.clone()))
        .collect();
    assert_eq!(imports.len(), 2);
    assert_eq!(imports[0].0, "a.b");
    assert_eq!(imports[0].1["wildcard"], json!(true));
    assert_eq!((imports[1].0, &imports[1].1["alias"]), ("c.D", &json!("E")));
    assert_eq!(
        edges(source, RelationshipType::Composition),
        pairs(&[("Main", "inner")])
    );
    let slug = &result.symbols[1];
    assert_eq!(slug.metadata["extension_receiver"], json!("String"));
    let twice = &result.symbols[2];
    assert_eq!(twice.metadata["extension_receiver"], json!("Int"));
    assert_eq!(twice.metadata["type"], json!("Int"));
}
