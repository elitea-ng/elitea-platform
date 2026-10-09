//! Extraction rules, against small inline sources and a golden fixture.
//!
//! `testdata/expected.txt` is the Python parser's output for the three
//! `testdata/*.txt` sources (written to disk under their real names), one
//! JSON row per symbol and relationship, produced with the pinned Python
//! engine:
//!
//! ```text
//! symbol:       [type, name, full_name, parent, scope, return_type,
//!                parameter_types, is_static, is_abstract, is_async,
//!                visibility, range, metadata]
//! relationship: [type, source, target, source_range, target_file,
//!                annotations]
//! ```

use super::{TypeScriptParser, parse_file, source::Source};
use crate::LanguageParser;
use crate::model::{ParseResult, RelationshipType, SymbolType};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::path::PathBuf;

const FIXTURES: &[(&str, &str)] = &[
    ("svc.ts", include_str!("testdata/svc.ts.txt")),
    ("View.tsx", include_str!("testdata/View.tsx.txt")),
    ("use.ts", include_str!("testdata/use.ts.txt")),
];

/// A fresh directory holding `files`; removed by the caller.
fn write_tree(tag: &str, files: &[(&str, &str)]) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dwts-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("temp dir");
    for (name, text) in files {
        std::fs::write(dir.join(name), text).expect("fixture");
    }
    dir
}

fn parse_tree(dir: &std::path::Path, names: &[&str]) -> BTreeMap<String, ParseResult> {
    let files: Vec<String> = names
        .iter()
        .map(|n| dir.join(n).to_string_lossy().into_owned())
        .collect();
    TypeScriptParser.parse_files(&files)
}

fn render(result: &ParseResult, prefix: &str) -> Vec<String> {
    let range = |r: &crate::model::Range| {
        json!({"start": {"line": r.start.line, "column": r.start.column},
               "end": {"line": r.end.line, "column": r.end.column}})
    };
    let mut rows = Vec::new();
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
                range(&s.range),
                Value::Object(s.metadata.clone())
            ])
            .to_string(),
        );
    }
    for r in &result.relationships {
        let target_file = r
            .target_file
            .as_deref()
            .map(|t| t.strip_prefix(prefix).unwrap_or(t).to_owned());
        rows.push(
            json!([
                r.relationship_type,
                r.source_symbol,
                r.target_symbol,
                r.source_range.as_ref().map(range),
                target_file,
                Value::Object(r.annotations.clone())
            ])
            .to_string(),
        );
    }
    rows
}

fn parse_one(path: &str, text: &str) -> ParseResult {
    parse_file(path, &Source::from_text(text))
}

#[test]
fn the_golden_fixture_matches_the_python_parser() {
    let dir = write_tree("golden", FIXTURES);
    let results = parse_tree(&dir, &["svc.ts", "View.tsx", "use.ts"]);
    let prefix = format!("{}/", dir.to_string_lossy());
    let mut actual = Vec::new();
    for name in ["svc.ts", "View.tsx", "use.ts"] {
        let result = &results[&format!("{prefix}{name}")];
        assert!(result.errors.is_empty(), "{:?}", result.errors);
        actual.push(format!("== {name}"));
        actual.extend(render(result, &prefix));
    }
    let _ = std::fs::remove_dir_all(&dir);
    let expected: Vec<&str> = include_str!("testdata/expected.txt").lines().collect();
    for (index, (a, e)) in actual.iter().zip(&expected).enumerate() {
        // Python's json.dumps(separators=(',', ':')) is serde_json's compact form.
        assert_eq!(a, e, "row {index}");
    }
    assert_eq!(actual.len(), expected.len());
}

#[test]
fn output_is_deterministic() {
    let dir = write_tree("determinism", FIXTURES);
    let first = parse_tree(&dir, &["svc.ts", "View.tsx", "use.ts"]);
    for _ in 0..5 {
        assert_eq!(parse_tree(&dir, &["svc.ts", "View.tsx", "use.ts"]), first);
    }
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_later_file_wins_the_cross_file_registry() {
    let dir = write_tree(
        "registry",
        &[
            ("a.ts", "export class Dup {}\n"),
            ("b.ts", "export class Dup {}\n"),
            ("c.ts", "export function f() { return new Dup(); }\n"),
        ],
    );
    let results = parse_tree(&dir, &["a.ts", "b.ts", "c.ts"]);
    let c = &results[&dir.join("c.ts").to_string_lossy().into_owned()];
    let creates = c
        .relationships
        .iter()
        .find(|r| r.relationship_type == RelationshipType::Creates)
        .expect("creates");
    assert!(
        creates
            .target_file
            .as_deref()
            .is_some_and(|t| t.ends_with("/b.ts"))
    );
    // A target registered to the source file itself gets no target_file.
    let b = &results[&dir.join("b.ts").to_string_lossy().into_owned()];
    assert!(b.relationships.iter().all(|r| r.target_file.is_none()));
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn decorators_go_to_the_next_symbol() {
    // A field's own decorator is its child, met after the field is made, so
    // it lands on the following member (Python's visit order).
    let result = parse_one(
        "/r/m.ts",
        "class A {\n  @Input() x: Foo;\n  y = 1;\n  @Get() run() {}\n}\n",
    );
    let decorators = |name: &str| {
        result
            .symbols
            .iter()
            .find(|s| s.name == name)
            .and_then(|s| s.metadata.get("decorators"))
            .map(|d| d[0]["name"].clone())
    };
    assert_eq!(decorators("x"), None);
    assert_eq!(decorators("y"), Some(json!("Input")));
    assert_eq!(decorators("run"), Some(json!("Get")));
}

#[test]
fn only_bare_enum_members_and_first_declarators_are_symbols() {
    let result = parse_one(
        "/r/m.ts",
        "enum E { A, B = 2 }\nconst x = 1, y = 2;\nvar z = 3;\n",
    );
    let names: Vec<(&str, SymbolType)> = result
        .symbols
        .iter()
        .map(|s| (s.name.as_str(), s.symbol_type))
        .collect();
    assert_eq!(
        names,
        [
            ("E", SymbolType::Enum),
            ("A", SymbolType::Constant),
            ("x", SymbolType::Constant)
        ]
    );
}

#[test]
fn nothing_inside_a_function_body_is_a_symbol_but_arrow_bodies_are_walked() {
    let result = parse_one(
        "/r/m.ts",
        "function f() { const inner = 1; }\nconst g = () => { const deep = 2; };\n",
    );
    let full: Vec<&str> = result
        .symbols
        .iter()
        .filter_map(|s| s.full_name.as_deref())
        .collect();
    assert_eq!(full, ["m.f", "m.g", "m.deep"]);
}

#[test]
fn a_body_is_also_attributed_to_the_enclosing_function() {
    // The generic walk after a handler revisits the body with the OUTER
    // symbol current, so `outer` also "calls" what `inner` calls.
    let result = parse_one(
        "/r/m.ts",
        "export const outer = () => {\n  const inner = () => {\n    work();\n  };\n};\n",
    );
    let callers: Vec<&str> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Calls)
        .map(|r| r.source_symbol.as_str())
        .collect();
    assert_eq!(callers, ["m.inner", "m.outer"]);
}

#[test]
fn non_ascii_text_is_sliced_by_byte_offsets() {
    let result = parse_one("/r/m.ts", "const é = 1; const after = 2;\n");
    let names: Vec<&str> = result.symbols.iter().map(|s| s.name.as_str()).collect();
    // "é" is two bytes; every later node reads one code point to the right.
    assert_eq!(names, ["é ", "fter "]);
}

#[test]
fn a_missing_file_is_an_error_result() {
    let results = TypeScriptParser.parse_files(&["/nonexistent/dwts/x.ts".to_owned()]);
    let result = &results["/nonexistent/dwts/x.ts"];
    assert!(result.symbols.is_empty());
    assert_eq!(result.errors.len(), 1);
    assert!(result.errors[0].starts_with("Parse error: "));
}
