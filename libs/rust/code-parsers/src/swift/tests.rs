//! Swift extraction rules on small inline sources (the full corpus is
//! `tests/kotlin_swift_golden.rs`).

use super::parse_source;
use crate::model::{RelationshipType, SymbolType};
use serde_json::json;

fn edges(source: &str, kind: RelationshipType) -> Vec<(String, String)> {
    parse_source("/repo/Main.swift", source)
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
    let id = &result.symbols[1];
    assert_eq!(id.parent_symbol.as_deref(), Some("Models.User"));
    assert_eq!(id.metadata["type"], json!("Int"));
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

#[test]
fn comments_and_strings_produce_nothing() {
    let source = concat!(
        "// class Ghost: Base {}\n",
        "/* func phantom() -> Int { Spook(1) }\n   @Haunted var x: Int */\n",
        "let s = \"struct InString { func f() { Make(2) } } @Fake x.call()\"\n",
        "let t = \"\"\"\n    protocol Raw {}\n    User(\\(s))\n\"\"\"\n",
    );
    let result = parse_source("/repo/Main.swift", source);
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
        "import Foundation\nimport UIKit\n\n",
        "@MainActor\n",
        "final class Profile:\n",
        "    Base,\n",
        "    Delegate\n",
        "{\n",
        "    private(set) var email: String?\n",
        "    static let shared = Profile()\n",
        "    class func make(\n",
        "        name: String,\n",
        "        age: Int\n",
        "    ) async throws\n",
        "        -> Profile {\n",
        "        return Summary(name: name).build()\n",
        "    }\n",
        "}\n",
    );
    let result = parse_source("/repo/Main.swift", source);
    let profile = &result.symbols[0];
    assert_eq!(profile.name, "Profile");
    assert_eq!((profile.range.start.line, profile.range.end.line), (4, 18));
    assert_eq!(profile.metadata["final"], json!(true));
    let names: Vec<_> = result.symbols.iter().map(|s| s.name.as_str()).collect();
    assert_eq!(names, ["Profile", "email", "shared", "make"]);
    let email = &result.symbols[1];
    assert_eq!(email.visibility.as_deref(), Some("internal"));
    assert_eq!(email.metadata["type"], json!("String?"));
    assert!(result.symbols[2].is_static);
    let make = &result.symbols[3];
    assert!(make.is_async && make.is_static);
    assert_eq!(make.return_type.as_deref(), Some("Profile"));
    assert_eq!((make.range.start.line, make.range.end.line), (11, 17));
    assert_eq!(
        edges(source, RelationshipType::Imports),
        pairs(&[("Main", "Foundation"), ("Main", "UIKit")])
    );
    assert_eq!(
        edges(source, RelationshipType::Inheritance),
        pairs(&[("Profile", "Base")])
    );
    assert_eq!(
        edges(source, RelationshipType::Implementation),
        pairs(&[("Profile", "Delegate")])
    );
    assert_eq!(
        edges(source, RelationshipType::Aggregation),
        pairs(&[("Main", "Summary")])
    );
    assert_eq!(
        edges(source, RelationshipType::Calls),
        pairs(&[("Main", "build")])
    );
    assert_eq!(
        edges(source, RelationshipType::Decorates),
        pairs(&[("Main", "MainActor")])
    );
}
