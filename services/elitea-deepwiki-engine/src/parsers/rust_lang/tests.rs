//! Extraction rules, against small inline sources and a golden fixture.
//!
//! `testdata/expected.txt` is the Python parser's output for the two
//! `testdata/*.txt` sources (written to disk under their real names and
//! parsed together, so it covers the cross-file pass), one compact JSON row
//! per symbol and relationship, produced with the pinned Python engine:
//!
//! ```text
//! file:         ["imports", imports, errors]
//! symbol:       [type, name, full_name, parent, scope, return_type,
//!                parameter_types, is_static, is_abstract, is_async,
//!                visibility, docstring, signature, range, metadata]
//! relationship: [type, source, target, source_range, source_file,
//!                target_file, confidence, annotations]
//! ```
//!
//! The sources are written to exercise the Python quirks the corpus rarely
//! reaches: leaking attributes, `use` list forms, `extern crate` aliases,
//! `Self` resolution, duplicated field-value calls, unions and nested
//! modules.

use super::RustParser;
use super::visitor::{core_type_name, parse_source};
use crate::parsers::LanguageParser;
use crate::parsers::model::{ParseResult, Range, RelationshipType, SymbolType};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

const FIXTURES: &[(&str, &str)] = &[
    ("model.rs", include_str!("testdata/model.rs.txt")),
    ("use.rs", include_str!("testdata/use.rs.txt")),
];

/// A fresh directory holding `files`; removed by the caller.
fn write_tree(tag: &str, files: &[(&str, &[u8])]) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dwrs-{tag}-{}", std::process::id()));
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
    RustParser.parse_files(&files)
}

fn range(r: &Range) -> Value {
    json!({"start": {"line": r.start.line, "column": r.start.column},
           "end": {"line": r.end.line, "column": r.end.column}})
}

fn render(result: &ParseResult, prefix: &str) -> Vec<String> {
    let relative = |p: &str| p.strip_prefix(prefix).unwrap_or(p).to_owned();
    let mut rows = vec![json!(["imports", result.imports, result.errors]).to_string()];
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
                s.is_static,
                s.is_abstract,
                s.is_async,
                s.visibility,
                s.docstring,
                s.signature,
                range(&s.range),
                Value::Object(s.metadata.clone())
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

fn parse_one(source: &str) -> ParseResult {
    parse_source("/repo/src/lib.rs", source)
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

fn fixture_bytes() -> Vec<(&'static str, &'static [u8])> {
    FIXTURES.iter().map(|(n, t)| (*n, t.as_bytes())).collect()
}

#[test]
fn the_golden_fixture_matches_the_python_parser() {
    let dir = write_tree("golden", &fixture_bytes());
    let results = parse_tree(&dir, &["model.rs", "use.rs"]);
    let prefix = format!("{}/", dir.to_string_lossy());
    let mut actual = Vec::new();
    for (name, _) in FIXTURES {
        actual.push(format!("== {name}"));
        actual.extend(render(&results[&format!("{prefix}{name}")], &prefix));
    }
    let _ = std::fs::remove_dir_all(&dir);
    let expected: Vec<&str> = include_str!("testdata/expected.txt").lines().collect();
    for (index, (a, e)) in actual.iter().zip(&expected).enumerate() {
        // Python's json.dumps(separators=(',', ':'), ensure_ascii=False) is
        // serde_json's compact form.
        assert_eq!(a, e, "row {index}");
    }
    assert_eq!(actual.len(), expected.len());
}

#[test]
fn output_is_deterministic() {
    let dir = write_tree("determinism", &fixture_bytes());
    let first = parse_tree(&dir, &["model.rs", "use.rs"]);
    for _ in 0..5 {
        assert_eq!(parse_tree(&dir, &["model.rs", "use.rs"]), first);
    }
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn text_is_decoded_with_replacement_and_newline_normalisation() {
    // Python: open(..., errors='replace') in text mode; byte columns.
    let source: &[u8] =
        b"/// caf\xe9 \xff\r\nfn a() {\r\n    b();\r\n}\rstruct \xc3\xa9X { f: Y }\n";
    let dir = write_tree("decode", &[("enc.rs", source)]);
    let results = parse_tree(&dir, &["enc.rs", "missing.rs"]);
    let prefix = format!("{}/", dir.to_string_lossy());
    let _ = std::fs::remove_dir_all(&dir);
    let result = &results[&format!("{prefix}enc.rs")];
    let a = &result.symbols[0];
    assert_eq!(a.docstring.as_deref(), Some("caf\u{fffd} \u{fffd}"));
    assert_eq!(a.range, Range::new(2, 0, 4, 1));
    assert_eq!(a.source_text.as_deref(), Some("fn a() {\n    b();\n}"));
    assert_eq!(result.symbols[1].name, "éX");
    assert_eq!(result.symbols[2].range, Range::new(5, 13, 5, 17));
    let missing = format!("{prefix}missing.rs");
    assert_eq!(
        results[&missing].errors,
        [format!("File not found: {missing}")]
    );
}

#[test]
fn attributes_leak_to_the_next_consuming_item() {
    let result = parse_one(
        "#[derive(Clone, MyTrait)]\nuse a::b;\nstruct A;\n#[derive(Debug)]\n// c\nstruct B;\nimpl X { fn f() {} #[derive(Late)] }\nstruct C;\n",
    );
    let derives = |i: usize| result.symbols[i].metadata.get("derives").cloned();
    assert_eq!(derives(0), Some(json!(["Clone", "MyTrait"])));
    assert_eq!(derives(1), None);
    // The trailing attribute in the impl reaches the next top-level item.
    let c = result.symbols.iter().find(|s| s.name == "C");
    assert_eq!(
        c.and_then(|s| s.metadata.get("derives").cloned()),
        Some(json!(["Late"]))
    );
    assert_eq!(
        rels(&result, RelationshipType::Annotates),
        [pair("MyTrait", "A"), pair("Late", "C")]
    );
}

#[test]
fn use_declarations_keep_the_python_paths() {
    let result = parse_one(
        "use std::io::*;\nuse a::{b, c::D, e as f, self, g::{h, *}};\nuse x as y;\npub use p::Q;\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Imports)
            .into_iter()
            .map(|(_, t)| t)
            .collect::<Vec<_>>(),
        ["::*", "a::b", "a::g::h", "a::g::*", "x", "p::Q"]
    );
    assert!(
        result
            .relationships
            .iter()
            .all(|r| r.source_symbol == "lib")
    );
    assert_eq!(
        result.relationships[5].annotations["re_export"],
        json!(true)
    );
    assert_eq!(result.relationships[4].annotations["alias"], json!("y"));
    assert_eq!(
        result.imports,
        ["::*", "a::b", "a", "a::g::h", "a::g::*", "x", "p::Q"]
    );
}

#[test]
fn field_types_split_into_composition_and_aggregation() {
    let result = parse_one(
        "struct S<'a> { a: &'a T, b: Box<U>, c: Vec<Option<V>>, d: HashMap<K, W>, e: Refs, f: *mut Z, g: std::sync::Arc<Q> }\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Aggregation),
        // `*mut Z` is a `pointer_type`, not a listed field type kind, so the
        // field type is empty and gives no edge.
        [pair("S", "'a T"), pair("S", "U"), pair("S", "Refs")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Composition),
        [pair("S", "V"), pair("S", "std::sync::Arc")]
    );
    assert_eq!(core_type_name("*const T"), "const T");
    assert_eq!(core_type_name("&mut dyn Fn() -> X"), "Fn() -> X");
    // `->` holds the last `>`: Python slices `A-` out of `Box<A->B`.
    assert_eq!(core_type_name("Box<A->B"), "A-");
}

#[test]
fn calls_resolve_self_and_record_field_values_twice() {
    let result = parse_one(
        "extern crate foo as bar;\nimpl W {\n  fn n() -> Self {\n    Self { a: make(), ..base() };\n    Self::other();\n    bar::go();\n    x.0();\n    u32::from(1);\n    a.b();\n    println!();\n    log::warn!();\n  }\n}\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Calls),
        [
            pair("W.n", "make"),
            pair("W.n", "base"),
            pair("W.n", "make"),
            pair("W.n", "base"),
            pair("W.n", "W::other"),
            pair("W.n", "bar::go"),
            // `from` is not a builtin; `u32` before `::` is not checked.
            pair("W.n", "u32::from"),
            pair("W.n", "a.b"),
            pair("W.n", "log::warn"),
        ]
    );
    assert_eq!(rels(&result, RelationshipType::Creates), [pair("W.n", "W")]);
    let go = result
        .relationships
        .iter()
        .find(|r| r.target_symbol == "bar::go");
    assert_eq!(
        go.map(|r| r.annotations["resolved_crate"].clone()),
        Some(json!("foo"))
    );
}

#[test]
fn methods_and_traits_carry_their_impl_context() {
    let result = parse_one(
        "trait T: A + B<u8> { fn s(&self); fn d() {} }\nimpl<X> T for Vec<X> { fn s(&self) {} }\n",
    );
    let kinds: Vec<_> = result
        .symbols
        .iter()
        .map(|s| {
            (
                s.full_name.clone().unwrap_or_default(),
                s.symbol_type,
                s.is_abstract,
                s.is_static,
            )
        })
        .collect();
    assert_eq!(
        kinds,
        [
            ("T".to_owned(), SymbolType::Trait, false, false),
            ("T.s".to_owned(), SymbolType::Method, true, false),
            ("T.d".to_owned(), SymbolType::Method, false, true),
            ("Vec.s".to_owned(), SymbolType::Method, false, false),
        ]
    );
    assert_eq!(
        rels(&result, RelationshipType::Inheritance),
        [pair("T", "A")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Implementation),
        [pair("Vec", "T")]
    );
    assert_eq!(result.symbols[3].signature.as_deref(), Some("fn s (&self)"));
}

#[test]
fn methods_of_a_type_in_another_file_get_a_cross_file_edge() {
    let files: &[(&str, &[u8])] = &[
        ("a.rs", b"pub struct Thing;\npub fn helper() {}\n"),
        (
            "b.rs",
            b"impl Thing { fn go(&self) { helper(); x.go(); Thing::go(); } }\n",
        ),
    ];
    let dir = write_tree("cross", files);
    let results = parse_tree(&dir, &["a.rs", "b.rs"]);
    let prefix = format!("{}/", dir.to_string_lossy());
    let _ = std::fs::remove_dir_all(&dir);
    let (a, b) = (format!("{prefix}a.rs"), format!("{prefix}b.rs"));
    let rels = &results[&b].relationships;
    let cross = rels.last().expect("cross-file edge");
    assert_eq!(cross.source_file, a);
    assert_eq!(cross.target_file.as_deref(), Some(b.as_str()));
    assert_eq!(cross.annotations["impl_kind"], json!("inherent"));
    let targets: Vec<_> = rels
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Calls)
        .map(|r| r.target_file.clone().unwrap_or_default())
        .collect();
    assert_eq!(targets, [a, b.clone(), b]);
}
