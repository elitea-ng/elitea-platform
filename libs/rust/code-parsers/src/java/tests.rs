//! Extraction rules, against small inline sources and a golden fixture.
//!
//! `testdata/expected.txt` is the Python parser's output for every
//! `testdata/*.java.txt` source (written to disk under its real name and
//! parsed together with one `parse_multiple_files`, so it covers the
//! cross-file pass), one compact JSON row per symbol and relationship,
//! produced with the pinned Python engine:
//!
//! ```text
//! file:         ["file", errors]
//! symbol:       [type, name, full_name, parent, scope, return_type,
//!                parameter_types, range, metadata, source_text]
//! relationship: [type, source, target, source_range, source_file,
//!                target_file, confidence, annotations]
//! ```
//!
//! The sources exercise what the petclinic corpus rarely reaches: generics,
//! nested, local and anonymous classes, enums with bodies, records, sealed
//! and non-sealed types, default interface methods, annotation types,
//! lambdas, method references, static and wildcard imports, overloads,
//! `@Override` across files, a file without a package, `package-info`, a
//! CRLF file with multi-byte characters, a BOM file with lone `\r`
//! newlines, an invalid UTF-8 file and a file with syntax errors.

use super::JavaParser;
use super::source::Source;
use super::visitor::{is_builtin_type, parse_source};
use crate::LanguageParser;
use crate::model::{ParseResult, Range, RelationshipType};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

const FIXTURES: &[(&str, &[u8])] = &[
    ("Base.java", include_bytes!("testdata/Base.java.txt")),
    ("Bom.java", include_bytes!("testdata/Bom.java.txt")),
    ("Broken.java", include_bytes!("testdata/Broken.java.txt")),
    ("Circle.java", include_bytes!("testdata/Circle.java.txt")),
    (
        "Duplicate.java",
        include_bytes!("testdata/Duplicate.java.txt"),
    ),
    ("Errors.java", include_bytes!("testdata/Errors.java.txt")),
    (
        "Measurable.java",
        include_bytes!("testdata/Measurable.java.txt"),
    ),
    ("Named.java", include_bytes!("testdata/Named.java.txt")),
    (
        "NoPackage.java",
        include_bytes!("testdata/NoPackage.java.txt"),
    ),
    ("Owner.java", include_bytes!("testdata/Owner.java.txt")),
    (
        "OwnerService.java",
        include_bytes!("testdata/OwnerService.java.txt"),
    ),
    ("Point.java", include_bytes!("testdata/Point.java.txt")),
    (
        "Registry.java",
        include_bytes!("testdata/Registry.java.txt"),
    ),
    ("Shape.java", include_bytes!("testdata/Shape.java.txt")),
    ("Square.java", include_bytes!("testdata/Square.java.txt")),
    ("Unicode.java", include_bytes!("testdata/Unicode.java.txt")),
    (
        "package-info.java",
        include_bytes!("testdata/package-info.java.txt"),
    ),
];

/// A fresh directory holding `files`; removed by the caller.
fn write_tree(tag: &str, files: &[(&str, &[u8])]) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dwjava-{tag}-{}", std::process::id()));
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
    JavaParser.parse_files(&files)
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
    parse_source("/repo/src/App.java", &Source::from_text(text))
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
fn an_invalid_byte_fails_the_whole_file_with_the_cpython_message() {
    let dir = write_tree("strict", &[("Bad.java", b"class A {}\n// \xff\n")]);
    let results = parse_tree(&dir, &["Bad.java", "Missing.java"]);
    let bad = &results[&dir.join("Bad.java").to_string_lossy().into_owned()];
    assert!(bad.symbols.is_empty());
    assert_eq!(
        bad.errors,
        ["'utf-8' codec can't decode byte 0xff in position 14: invalid start byte"]
    );
    let missing_path = dir.join("Missing.java").to_string_lossy().into_owned();
    assert_eq!(
        results[&missing_path].errors,
        [format!("File not found: {missing_path}")]
    );
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn symbols_are_scoped_under_the_package_or_the_file_stem() {
    let result = parse_one("package a.b;\nimport x.y.Z;\nclass C { int f; }\n");
    assert_eq!(result.symbols[0].full_name.as_deref(), Some("a.b.C"));
    assert_eq!(result.symbols[0].parent_symbol.as_deref(), Some("a.b"));
    assert_eq!(
        rels(&result, RelationshipType::Imports),
        [pair("a.b", "x.y.Z")]
    );

    let result = parse_one("import x.y.Z;\nclass C { void m() {} }\n");
    assert_eq!(result.symbols[0].full_name.as_deref(), Some("App.C"));
    assert_eq!(result.symbols[1].full_name.as_deref(), Some("App.C.m"));
    assert_eq!(
        rels(&result, RelationshipType::Imports),
        [pair("App", "x.y.Z")]
    );
}

#[test]
fn only_bare_supertypes_give_inheritance_edges() {
    let result = parse_one(
        "package p;\nclass A extends B implements I, J<K> {}\nclass G extends B<T> {}\ninterface X extends Y {}\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Inheritance),
        [pair("p.A", "B")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Implementation),
        [pair("p.A", "I")]
    );
}

#[test]
fn a_multi_declarator_field_is_one_field_named_after_the_last() {
    let result = parse_one("package p;\nclass A { Owner a, b; }\n");
    let fields: Vec<&str> = result
        .symbols
        .iter()
        .filter(|s| s.symbol_type == crate::model::SymbolType::Field)
        .map(|s| s.name.as_str())
        .collect();
    assert_eq!(fields, ["b"]);
    assert_eq!(
        rels(&result, RelationshipType::Composition),
        [pair("p.A.b", "Owner")]
    );
}

#[test]
fn local_variable_references_repeat_the_innermost_scope() {
    let result = parse_one("package p;\nclass A { void run() { Owner o = get(); o.save(); } }\n");
    assert_eq!(
        rels(&result, RelationshipType::References),
        [pair("p.A.run.run", "Owner")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Calls),
        [pair("p.A.run", "get"), pair("p.A.run", "Owner.save")]
    );
}

#[test]
fn overrides_follow_the_first_inheritance_edge_of_the_scope() {
    let result = parse_one(
        "package p;\nclass A extends B implements I {\n  @Override public void go() {}\n}\nclass C implements I { @Override public void go() {} }\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Overrides),
        [pair("p.A.go", "B.go")]
    );
}

#[test]
fn defines_edges_name_overloads_and_read_modifiers_from_the_first_line() {
    let result = parse_one(
        "package p;\nclass A {\n  public static void f(int a) {}\n  private void f(String s) {}\n  @Deprecated\n  public int g() { return 0; }\n  String publicKey;\n}\n",
    );
    let defines: Vec<(&str, &Value, &Value)> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Defines)
        .map(|r| {
            (
                r.target_symbol.as_str(),
                &r.annotations["visibility"],
                &r.annotations["is_static"],
            )
        })
        .collect();
    assert_eq!(
        defines,
        [
            ("p.A.f(int)", &json!("public"), &json!(true)),
            ("p.A.f(String)", &json!("private"), &json!(false)),
            ("p.A.g", &json!("package"), &json!(false)),
            ("p.A.publicKey", &json!("public"), &json!(false)),
        ]
    );
}

#[test]
fn cross_file_resolution_rewrites_targets_and_drops_reference_annotations() {
    let files: &[(&str, &[u8])] = &[
        (
            "A.java",
            b"package p;\nimport q.Owner;\nclass A { void m(Owner o) { save(); } }\n",
        ),
        (
            "Owner.java",
            b"package q;\npublic class Owner { void save() {} }\n",
        ),
        (
            "Saver.java",
            b"package r;\nclass Saver { void save() {} }\n",
        ),
        ("package-info.java", b"package p;\nimport q.Owner;\n"),
    ];
    let dir = write_tree("cross", files);
    let results = parse_tree(&dir, &files.iter().map(|(n, _)| *n).collect::<Vec<_>>());
    let path = |n: &str| dir.join(n).to_string_lossy().into_owned();
    let a = &results[&path("A.java")];
    let call = a
        .relationships
        .iter()
        .find(|r| r.relationship_type == RelationshipType::Calls)
        .expect("call");
    // `save` is registered for Owner and Saver; the imported class wins.
    assert_eq!(call.target_symbol, "Owner.save");
    assert_eq!(
        call.target_file.as_deref(),
        Some(path("Owner.java").as_str())
    );
    let reference = a
        .relationships
        .iter()
        .find(|r| r.relationship_type == RelationshipType::References)
        .expect("reference");
    assert_eq!(
        reference.target_file.as_deref(),
        Some(path("Owner.java").as_str())
    );
    assert!(reference.annotations.is_empty());
    // A file without symbols is not enhanced.
    let info = &results[&path("package-info.java")];
    assert_eq!(
        info.relationships[0].target_file.as_deref(),
        Some(path("package-info.java").as_str())
    );
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn the_builtin_list_matches_python() {
    assert!(is_builtin_type(""));
    assert!(is_builtin_type("String"));
    assert!(is_builtin_type("java.util.List"));
    assert!(!is_builtin_type("java.utilx.List"));
    assert!(!is_builtin_type("Owner"));
}
