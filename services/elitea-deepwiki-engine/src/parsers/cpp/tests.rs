//! Extraction rules, against small inline sources and a golden fixture.
//!
//! `testdata/expected.txt` is the Python parser's output for every
//! `testdata/*.txt` source (written to disk under its real name and parsed
//! together with one `parse_multiple_files`, so it covers the cross-file
//! pass), one compact JSON row per symbol and relationship, produced with
//! the pinned Python engine (a `None` metadata or annotation reads as `{}`):
//!
//! ```text
//! file:         ["file", errors]
//! symbol:       [type, name, full_name, parent, scope, return_type,
//!                parameter_types, range, metadata, source_text, comments]
//! relationship: [type, source, target, source_range, source_file,
//!                target_file, confidence, annotations]
//! ```
//!
//! The sources exercise what leveldb rarely reaches: templates with full
//! and partial specialisation, CRTP, explicit instantiation, inline,
//! anonymous and nested `a::b::c` namespaces, out-of-class definitions,
//! operators, virtual / override / final, multiple inheritance, `using` and
//! `typedef` (a function pointer, an anonymous struct), `enum class` and an
//! anonymous enum, lambdas with captures, `new` (array, placement),
//! `make_unique` / `make_shared`, macros, `constexpr`, a concept, a union,
//! `extern "C"`, every header and source extension, a BOM + CRLF file with
//! multi-byte characters (the text-slicing quirk), an invalid byte and a
//! file with syntax errors.

use super::CppParser;
use super::source::Source;
use crate::parsers::LanguageParser;
use crate::parsers::model::{ParseResult, Range, RelationshipType, SymbolType};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

const FIXTURES: &[(&str, &[u8])] = &[
    ("base.hpp", include_bytes!("testdata/base.hpp.txt")),
    ("broken.cpp", include_bytes!("testdata/broken.cpp.txt")),
    ("latin.cxx", include_bytes!("testdata/latin.cxx.txt")),
    ("ops.c++", include_bytes!("testdata/ops.c++.txt")),
    ("ops.hh", include_bytes!("testdata/ops.hh.txt")),
    ("shapes.cc", include_bytes!("testdata/shapes.cc.txt")),
    ("shapes.h", include_bytes!("testdata/shapes.h.txt")),
    ("unicode.cpp", include_bytes!("testdata/unicode.cpp.txt")),
];

/// A fresh directory holding `files`; removed by the caller.
fn write_tree(tag: &str, files: &[(&str, &[u8])]) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dwcpp-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("temp dir");
    for (name, bytes) in files {
        std::fs::write(dir.join(name), bytes).expect("fixture");
    }
    dir
}

fn parse_tree(dir: &Path, names: &[&str]) -> BTreeMap<String, ParseResult> {
    let files: Vec<String> = names
        .iter()
        .map(|n| dir.join(n).to_string_lossy().into_owned())
        .collect();
    CppParser.parse_files(&files)
}

fn range(r: &Range) -> Value {
    json!({"start": {"line": r.start.line, "column": r.start.column},
           "end": {"line": r.end.line, "column": r.end.column}})
}

fn render(result: &ParseResult, prefix: &str) -> Vec<String> {
    let relative = |p: &str| p.strip_prefix(prefix).unwrap_or(p).to_owned();
    let mut rows = vec![json!(["file", result.errors]).to_string()];
    for s in &result.symbols {
        rows.push(
            json!([
                s.symbol_type,
                s.name,
                s.full_name,
                s.parent_symbol,
                s.scope,
                s.return_type,
                s.parameter_types,
                range(&s.range),
                Value::Object(s.metadata.clone()),
                s.source_text,
                s.comments
            ])
            .to_string(),
        );
    }
    for r in &result.relationships {
        rows.push(
            json!([
                r.relationship_type,
                r.source_symbol,
                r.target_symbol,
                r.source_range.as_ref().map(range),
                relative(&r.source_file),
                r.target_file.as_deref().map(relative),
                r.confidence,
                Value::Object(r.annotations.clone())
            ])
            .to_string(),
        );
    }
    rows
}

fn names() -> Vec<&'static str> {
    FIXTURES.iter().map(|(n, _)| *n).collect()
}

fn parse_one(text: &str) -> ParseResult {
    super::parse_source("/repo/src/app.cpp", &Source::from_text(text))
}

fn rels(result: &ParseResult, rel_type: RelationshipType) -> Vec<(String, String)> {
    result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == rel_type)
        .map(|r| (r.source_symbol.clone(), r.target_symbol.clone()))
        .collect()
}

fn pair(a: &str, b: &str) -> (String, String) {
    (a.to_owned(), b.to_owned())
}

fn full_names(result: &ParseResult, symbol_type: SymbolType) -> Vec<String> {
    result
        .symbols
        .iter()
        .filter(|s| s.symbol_type == symbol_type)
        .filter_map(|s| s.full_name.clone())
        .collect()
}

#[test]
fn the_grammar_is_the_language_packs() {
    let language: tree_sitter::Language = tree_sitter_cpp::LANGUAGE.into();
    assert_eq!(language.abi_version(), 15);
    assert_eq!(language.parse_state_count(), 11734);
    assert_eq!(language.field_count(), 53);
    assert_eq!(language.node_kind_count(), 575);
}

#[test]
fn the_golden_fixture_matches_the_python_parser() {
    let dir = write_tree("golden", FIXTURES);
    let results = parse_tree(&dir, &names());
    let prefix = format!("{}/", dir.to_string_lossy());
    let mut rows = Vec::new();
    for (name, _) in FIXTURES {
        let path = dir.join(name).to_string_lossy().into_owned();
        rows.push(format!("== {name}"));
        rows.extend(render(&results[&path], &prefix));
    }
    let _ = std::fs::remove_dir_all(&dir);
    let expected: Vec<&str> = include_str!("testdata/expected.txt").lines().collect();
    assert_eq!(rows.len(), expected.len(), "row count");
    for (index, (got, want)) in rows.iter().zip(&expected).enumerate() {
        let got: Value = serde_json::from_str(got).unwrap_or_else(|_| Value::String(got.clone()));
        let want: Value =
            serde_json::from_str(want).unwrap_or_else(|_| Value::String((*want).to_owned()));
        assert_eq!(got, want, "row {index}");
    }
}

#[test]
fn output_is_deterministic() {
    let dir = write_tree("determinism", FIXTURES);
    let first = parse_tree(&dir, &names());
    for _ in 0..3 {
        assert_eq!(parse_tree(&dir, &names()), first);
    }
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_missing_file_is_a_parse_error() {
    let dir = write_tree("missing", &[]);
    let path = dir.join("gone.cpp").to_string_lossy().into_owned();
    let results = parse_tree(&dir, &["gone.cpp"]);
    assert_eq!(
        results[&path].errors,
        [format!(
            "Parse error: [Errno 2] No such file or directory: '{path}'"
        )]
    );
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn names_are_scoped_under_the_file_stem_and_named_namespaces_only() {
    let result = parse_one(
        "namespace n { struct A { int f; }; }\nnamespace { struct Hidden {}; }\nnamespace a::b { struct Deep {}; }\n",
    );
    assert_eq!(full_names(&result, SymbolType::Struct), ["app.n.A"]);
    assert_eq!(full_names(&result, SymbolType::Field), ["app.n.A.f"]);
    assert_eq!(
        rels(&result, RelationshipType::Defines),
        [pair("app.n.A", "app.n.A.f")]
    );
}

#[test]
fn includes_outside_a_function_are_dropped_as_orphans() {
    let result = parse_one("#include <vector>\n#include \"a.h\"\n");
    let includes: Vec<&str> = result.symbols.iter().map(|s| s.name.as_str()).collect();
    assert_eq!(includes, ["include::vector", "include::a.h"]);
    assert!(rels(&result, RelationshipType::Imports).is_empty());
    assert_eq!(result.warnings.len(), 2);
}

#[test]
fn out_of_class_definitions_are_typed_by_their_qualifier() {
    let result = parse_one(
        "namespace n {\nA::A() {}\nint A::get() const { return 1; }\nint util::helper() { return 2; }\n}\n",
    );
    let kinds: Vec<(String, SymbolType)> = result
        .symbols
        .iter()
        .filter(|s| s.symbol_type != SymbolType::Parameter)
        .map(|s| (s.full_name.clone().unwrap_or_default(), s.symbol_type))
        .collect();
    assert_eq!(
        kinds,
        [
            ("app.n.A.A".to_owned(), SymbolType::Constructor),
            ("app.n.A.get".to_owned(), SymbolType::Method),
            ("app.n.util.helper".to_owned(), SymbolType::Function),
        ]
    );
    // The relationship pass looks `A::get` up WITHOUT the namespaces and
    // finds it by bare name.
    let bodies = rels(&result, RelationshipType::DefinesBody);
    assert!(bodies.contains(&pair("app.n.A.get", "app.n.A.get")));
}

#[test]
fn override_targets_the_first_base() {
    let result = parse_one(
        "struct B { virtual void f(); };\nstruct C { };\nstruct D : B, C { void f() override; void g() override {} };\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Overrides),
        [pair("app.D.f", "B.f"), pair("app.D.g", "B.g")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Inheritance),
        [pair("app.D", "B"), pair("app.D", "C")]
    );
}

#[test]
fn value_declarations_create_and_pointers_do_not() {
    let result = parse_one(
        "void f() { Point a; Point b(1); Point* c; Point* d = new Point(); Box<int> e; }\n",
    );
    let creates: Vec<(String, String)> = rels(&result, RelationshipType::Creates);
    assert_eq!(
        creates,
        [
            pair("app.f", "Point"),
            pair("app.f", "Point"),
            pair("app.f", "Point"),
            pair("app.f", "Box")
        ]
    );
    assert_eq!(
        rels(&result, RelationshipType::Instantiates),
        [pair("app.f", "Box")]
    );
}

#[test]
fn an_anonymous_enum_is_named_after_its_first_enumerator() {
    let result = parse_one("enum { kA = 1, kB };\nenum class E : int { X };\n");
    assert_eq!(
        full_names(&result, SymbolType::Enum),
        ["app.<anonymous_enum:kA>", "app.E"]
    );
    assert_eq!(
        full_names(&result, SymbolType::Constant),
        [
            "app.<anonymous_enum:kA>.kA",
            "app.<anonymous_enum:kA>.kB",
            "app.E.X"
        ]
    );
}

#[test]
fn cross_file_targets_prefer_headers() {
    let files: &[(&str, &[u8])] = &[
        ("a.cc", b"struct Shape {};\nvoid use() { Shape s; }\n"),
        ("b.cc", b"struct Shape {};\n"),
        ("shape.h", b"struct Shape { void draw(); };\n"),
        ("shape.cc", b"void Shape::draw() {}\n"),
    ];
    let dir = write_tree("cross", files);
    let results = parse_tree(&dir, &files.iter().map(|(n, _)| *n).collect::<Vec<_>>());
    let path = |n: &str| dir.join(n).to_string_lossy().into_owned();
    let a = &results[&path("a.cc")];
    let create = a
        .relationships
        .iter()
        .find(|r| r.relationship_type == RelationshipType::Creates)
        .expect("creates");
    assert_eq!(
        create.target_file.as_deref(),
        Some(path("shape.h").as_str())
    );
    let draw = &results[&path("shape.cc")];
    let body = draw
        .relationships
        .iter()
        .find(|r| r.relationship_type == RelationshipType::DefinesBody)
        .expect("defines_body");
    assert_eq!(body.target_symbol, "shape.Shape.draw");
    // Same stem, same full name: the header's declaration is the target.
    assert_eq!(body.target_file.as_deref(), Some(path("shape.h").as_str()));
    let _ = std::fs::remove_dir_all(&dir);
}
