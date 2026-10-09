//! Extraction rules, against small inline sources and a golden fixture.
//!
//! `testdata/expected.txt` is the Python parser's output for every
//! `testdata/*.cs.txt` source except `Deep.cs.txt` (written to disk under
//! its real name and parsed together with one `parse_multiple_files`, so it
//! covers the cross-file pass), one compact JSON row per symbol and
//! relationship, produced with the pinned Python engine (a `None` metadata
//! or annotations map written as `{}`):
//!
//! ```text
//! file:         ["file", errors]
//! symbol:       [type, name, full_name, parent, scope, return_type,
//!                parameter_types, range, metadata, source_text]
//! relationship: [type, source, target, source_range, source_file,
//!                target_file, confidence, annotations]
//! ```
//!
//! The sources exercise what the `CleanArchitecture` corpus rarely reaches:
//! block and file-scoped namespaces, nested namespaces and types, partial
//! classes within and across files, records (positional, `record class`,
//! `record struct`), structs, interfaces with default and static members,
//! generic constraints, extension methods, properties with `init`,
//! `required` and expression bodies, events (field, nullable, custom
//! accessors), delegates, attributes with arguments, async/await, LINQ
//! query and method syntax, lambdas, operators (`checked`, conversion),
//! indexers, XML doc comments, preprocessor directives, top-level
//! statements, primary constructors, a CRLF file with multi-byte
//! characters, a BOM file with lone `\r` newlines, an invalid UTF-8 file
//! and a file with syntax errors. `Deep.cs.txt` (a 600-operand string
//! concatenation) is the recursion-limit difference: Python fails it whole.

use super::CSharpParser;
use super::visitor::{is_builtin_type, is_interface_name, parse_source};
use crate::LanguageParser;
use crate::java::source::Source;
use crate::model::{ParseResult, Range, RelationshipType, SymbolType};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

const FIXTURES: &[(&str, &[u8])] = &[
    ("Bom.cs", include_bytes!("testdata/Bom.cs.txt")),
    ("Broken.cs", include_bytes!("testdata/Broken.cs.txt")),
    ("Contracts.cs", include_bytes!("testdata/Contracts.cs.txt")),
    ("Events.cs", include_bytes!("testdata/Events.cs.txt")),
    (
        "Extensions.cs",
        include_bytes!("testdata/Extensions.cs.txt"),
    ),
    ("Invalid.cs", include_bytes!("testdata/Invalid.cs.txt")),
    ("Modern.cs", include_bytes!("testdata/Modern.cs.txt")),
    ("Partial1.cs", include_bytes!("testdata/Partial1.cs.txt")),
    ("Partial2.cs", include_bytes!("testdata/Partial2.cs.txt")),
    (
        "Preprocessor.cs",
        include_bytes!("testdata/Preprocessor.cs.txt"),
    ),
    ("Records.cs", include_bytes!("testdata/Records.cs.txt")),
    ("Shapes.cs", include_bytes!("testdata/Shapes.cs.txt")),
    ("TopLevel.cs", include_bytes!("testdata/TopLevel.cs.txt")),
    ("Unicode.cs", include_bytes!("testdata/Unicode.cs.txt")),
];

/// A fresh directory holding `files`; removed by the caller.
fn write_tree(tag: &str, files: &[(&str, &[u8])]) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dwcs-{tag}-{}", std::process::id()));
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
    CSharpParser.parse_files(&files)
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
                s.source_text
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

fn parse_one(text: &str) -> ParseResult {
    parse_source("/repo/src/App.cs", &Source::from_text(text))
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

fn names() -> Vec<&'static str> {
    FIXTURES.iter().map(|(n, _)| *n).collect()
}

#[test]
fn the_grammar_is_the_language_packs() {
    let language: tree_sitter::Language = tree_sitter_c_sharp::LANGUAGE.into();
    assert_eq!(language.abi_version(), 15);
    assert_eq!(language.parse_state_count(), 8495);
    assert_eq!(language.field_count(), 26);
    assert_eq!(language.node_kind_count(), 545);
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
    let mut reversed = names();
    reversed.reverse();
    let first = parse_tree(&dir, &names());
    for _ in 0..3 {
        assert_eq!(parse_tree(&dir, &names()), first);
    }
    // The registries follow the CALLER's order, so a reversed list may
    // resolve differently — but it too is the same on every run.
    let backwards = parse_tree(&dir, &reversed);
    assert_eq!(parse_tree(&dir, &reversed), backwards);
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_deeply_nested_file_parses_where_python_hits_its_recursion_limit() {
    let dir = write_tree(
        "deep",
        &[("Deep.cs", include_bytes!("testdata/Deep.cs.txt"))],
    );
    let results = parse_tree(&dir, &["Deep.cs"]);
    let deep = &results[&dir.join("Deep.cs").to_string_lossy().into_owned()];
    let _ = std::fs::remove_dir_all(&dir);
    assert!(deep.errors.is_empty());
    let names: Vec<&str> = deep.symbols.iter().map(|s| s.name.as_str()).collect();
    assert_eq!(names, ["Concat", "Make"]);
}

#[test]
fn a_missing_file_is_reported_as_python_does() {
    let dir = write_tree("missing", &[]);
    let results = parse_tree(&dir, &["Missing.cs"]);
    let path = dir.join("Missing.cs").to_string_lossy().into_owned();
    assert_eq!(results[&path].errors, [format!("File not found: {path}")]);
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn namespaces_scope_symbols_block_and_file_scoped() {
    let result = parse_one(
        "using X.Y;\nnamespace A.B { using Z; class C { } namespace D { class E { } } }\n",
    );
    let full: Vec<Option<&str>> = result
        .symbols
        .iter()
        .map(|s| s.full_name.as_deref())
        .collect();
    assert_eq!(full, [Some("A.B.C"), Some("A.B.D.E")]);
    assert_eq!(
        rels(&result, RelationshipType::Imports),
        [pair("App", "X.Y"), pair("A.B", "Z")]
    );

    let result = parse_one("namespace A;\nusing Q;\nclass C { void M() { } }\n");
    assert_eq!(result.symbols[1].full_name.as_deref(), Some("A.C.M"));
    assert_eq!(rels(&result, RelationshipType::Imports), [pair("A", "Q")]);
}

#[test]
fn base_lists_follow_the_i_prefix_convention() {
    let result = parse_one(
        "class A : Base, IFoo, Io, IList<int> { }\nstruct S : Base { }\ninterface I : J, Base { }\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Inheritance),
        [
            pair("A", "Base"),
            pair("A", "Io"),
            pair("I", "J"),
            pair("I", "Base")
        ]
    );
    assert_eq!(
        rels(&result, RelationshipType::Implementation),
        [pair("A", "IFoo"), pair("A", "IList"), pair("S", "Base")]
    );
}

#[test]
fn overrides_and_base_calls_follow_the_first_inheritance_edge() {
    let result = parse_one(
        "class A : B, IC { A() : base() { } public override void Go() { } }\nclass D : IC { D() : base() { } public override void Go() { } }\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Overrides),
        [pair("A.Go", "B.Go")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Calls),
        [pair("A.A", "B.B"), pair("D.D", "base.<init>")]
    );
}

#[test]
fn calls_resolve_fields_and_parameters_to_their_types() {
    let result = parse_one(
        "class A { private Repo _r; void M(Svc s) { _r.Save(); s.Run(); x.Y.Z(); Local(); } void N() { s.Run(); } }\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Calls),
        [
            pair("A.M", "Repo.Save"),
            pair("A.M", "Svc.Run"),
            pair("A.M", "x.Y.Z"),
            pair("A.M", "Local"),
            pair("A.N", "s.Run"),
        ]
    );
}

#[test]
fn defines_edges_name_overloads_and_constructors() {
    let result = parse_one(
        "class A { public A(int a) { } static void F(int a) { } private void F(string s) { } int P { get; } }\n",
    );
    let defines: Vec<(&str, &Value)> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Defines)
        .map(|r| (r.target_symbol.as_str(), &r.annotations["visibility"]))
        .collect();
    assert_eq!(
        defines,
        [
            ("A.<init>(int)", &json!("public")),
            ("A.F(int)", &json!("internal")),
            ("A.F(string)", &json!("private")),
            ("A.P", &json!("internal")),
        ]
    );
}

#[test]
fn records_expose_positional_parameters_as_properties() {
    let result = parse_one("record struct P(int X, Owner Y);\n");
    assert_eq!(result.symbols[0].symbol_type, SymbolType::Struct);
    let props: Vec<(&str, Option<&str>)> = result.symbols[1..]
        .iter()
        .map(|s| (s.name.as_str(), s.return_type.as_deref()))
        .collect();
    assert_eq!(props, [("X", Some("int")), ("Y", Some("Owner"))]);
    assert_eq!(
        rels(&result, RelationshipType::Composition),
        [pair("P.Y", "Owner")]
    );
}

#[test]
fn cross_file_resolution_rewrites_targets_and_drops_annotations() {
    let files: &[(&str, &[u8])] = &[
        (
            "A.cs",
            b"namespace P;\nusing Q.Owner;\nclass A : Base { A() : base() { } void M(Owner o) { Save(); new Owner(); } }\n",
        ),
        (
            "Base.cs",
            b"namespace Q;\npublic class Base { } public class Owner { }\n",
        ),
        ("Saver.cs", b"class Saver { void Save() { } }\n"),
        ("Empty.cs", b"using Q.Owner;\n"),
    ];
    let dir = write_tree("cross", files);
    let results = parse_tree(&dir, &files.iter().map(|(n, _)| *n).collect::<Vec<_>>());
    let path = |n: &str| dir.join(n).to_string_lossy().into_owned();
    let a = &results[&path("A.cs")];
    let find = |t: RelationshipType| {
        a.relationships
            .iter()
            .filter(move |r| r.relationship_type == t)
            .collect::<Vec<_>>()
    };
    let calls = find(RelationshipType::Calls);
    assert_eq!(calls[0].target_symbol, "Base.Base");
    assert_eq!(
        calls[0].target_file.as_deref(),
        Some(path("Base.cs").as_str())
    );
    assert!(calls[0].annotations.is_empty());
    assert_eq!(calls[1].target_symbol, "Saver.Save");
    assert_eq!(
        calls[1].target_file.as_deref(),
        Some(path("Saver.cs").as_str())
    );
    let creates = find(RelationshipType::Creates);
    assert_eq!(
        creates[0].target_file.as_deref(),
        Some(path("Base.cs").as_str())
    );
    assert_eq!(creates[0].annotations["creation_type"], json!("new"));
    let references = find(RelationshipType::References);
    assert!(references[0].annotations.is_empty());
    let imports = find(RelationshipType::Imports);
    assert_eq!(
        imports[0].target_file.as_deref(),
        Some(path("Base.cs").as_str())
    );
    // A file without symbols is not enhanced.
    let empty = &results[&path("Empty.cs")];
    assert_eq!(
        empty.relationships[0].target_file.as_deref(),
        Some(path("Empty.cs").as_str())
    );
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn the_builtin_and_interface_heuristics_match_python() {
    assert!(is_builtin_type(""));
    assert!(is_builtin_type("string"));
    assert!(is_builtin_type("List<Owner>"));
    assert!(is_builtin_type("System.Text.Json"));
    assert!(is_builtin_type("Foo.Task"));
    assert!(!is_builtin_type("Owner"));
    assert!(!is_builtin_type("Systemx.Task2"));
    assert!(is_interface_name("IFoo<T>"));
    assert!(is_interface_name("IÄ"));
    assert!(!is_interface_name("Io"));
    assert!(!is_interface_name("I"));
}
