//! Extraction rules, against small inline sources and a golden fixture.
//!
//! `testdata/expected.txt` is the Python parser's output for every
//! `testdata/<a>__<b>__<name>.txt` source (written to disk as
//! `<a>/<b>/<name>` and the `.js`, `.jsx` and `.mjs` files parsed together
//! with one `parse_multiple_files`, so it covers import path resolution and
//! the cross-file pass), one compact JSON row per file, symbol and
//! relationship, produced with the pinned Python engine:
//!
//! ```text
//! file:         ["file", errors, exports, sorted(imports)]
//! symbol:       [type, name, full_name, parent, scope, visibility,
//!                is_static, range, metadata, source_text]
//! relationship: [type, source, target, source_range, source_file,
//!                target_file, confidence, annotations]
//! ```
//!
//! The sources exercise what the express corpus rarely reaches: ES default,
//! named, namespace and side-effect imports, re-exports and `export *`,
//! destructured and aliased `require`, `module.exports` objects, dynamic
//! `import()`, classes with static, private and computed fields, getters,
//! setters, generators, async and `#private` methods, `super` calls, JSX
//! components (member tags, fragments, intrinsic and dashed tags), IIFEs,
//! prototype assignment, `JSDoc` `@param` / `@returns` / `@type`, a class
//! defined twice, `this` calls in a file named after its class, a BOM and
//! CRLF file with multi-byte characters, a lone-`\r` file, an invalid UTF-8
//! file and a file with syntax errors.

use super::JavaScriptParser;
use super::resolve::resolve_import_path;
use super::visitor::parse_source;
use crate::parsers::LanguageParser;
use crate::parsers::java::source::Source;
use crate::parsers::model::{ParseResult, Range, RelationshipType, SymbolType};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

const FIXTURES: &[(&str, &[u8])] = &[
    (
        "lib/Circle.js",
        include_bytes!("testdata/lib__Circle.js.txt"),
    ),
    (
        "lib/shapes.js",
        include_bytes!("testdata/lib__shapes.js.txt"),
    ),
    ("lib/util.js", include_bytes!("testdata/lib__util.js.txt")),
    (
        "lib/widgets/Button.jsx",
        include_bytes!("testdata/lib__widgets__Button.jsx.txt"),
    ),
    (
        "lib/widgets/Theme.js",
        include_bytes!("testdata/lib__widgets__Theme.js.txt"),
    ),
    (
        "lib/widgets/index.js",
        include_bytes!("testdata/lib__widgets__index.js.txt"),
    ),
    ("src/User.js", include_bytes!("testdata/src__User.js.txt")),
    ("src/app.mjs", include_bytes!("testdata/src__app.mjs.txt")),
    (
        "src/broken.js",
        include_bytes!("testdata/src__broken.js.txt"),
    ),
    (
        "src/data.json",
        include_bytes!("testdata/src__data.json.txt"),
    ),
    ("src/dog.js", include_bytes!("testdata/src__dog.js.txt")),
    ("src/iife.js", include_bytes!("testdata/src__iife.js.txt")),
    (
        "src/invalid.js",
        include_bytes!("testdata/src__invalid.js.txt"),
    ),
    (
        "src/oldmac.js",
        include_bytes!("testdata/src__oldmac.js.txt"),
    ),
    (
        "src/unicode.js",
        include_bytes!("testdata/src__unicode.js.txt"),
    ),
];

/// A fresh directory holding `files`, with symlinks resolved (as the
/// golden generator's); removed by the caller.
fn write_tree(tag: &str, files: &[(&str, &[u8])]) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dwjs-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("temp dir");
    let dir = std::fs::canonicalize(&dir).expect("canonical temp dir");
    for (name, bytes) in files {
        let path = dir.join(name);
        std::fs::create_dir_all(path.parent().expect("parent")).expect("fixture dir");
        std::fs::write(path, bytes).expect("fixture");
    }
    dir
}

/// The parsed files: every fixture of a JavaScript extension, sorted.
fn sources() -> Vec<&'static str> {
    FIXTURES
        .iter()
        .map(|(name, _)| *name)
        .filter(|n| {
            Path::new(n)
                .extension()
                .is_some_and(|e| e == "js" || e == "jsx" || e == "mjs")
        })
        .collect()
}

fn parse_tree(dir: &Path, names: &[&str]) -> BTreeMap<String, ParseResult> {
    let files: Vec<String> = names
        .iter()
        .map(|n| dir.join(n).to_string_lossy().into_owned())
        .collect();
    JavaScriptParser.parse_files(&files)
}

fn range(r: &Range) -> Value {
    json!({"start": {"line": r.start.line, "column": r.start.column},
           "end": {"line": r.end.line, "column": r.end.column}})
}

fn render(result: &ParseResult, prefix: &str) -> Vec<String> {
    let strip = |p: &str| p.strip_prefix(prefix).unwrap_or(p).to_owned();
    let mut rows = vec![json!(["file", result.errors, result.exports, result.imports]).to_string()];
    for s in &result.symbols {
        rows.push(
            json!([
                s.symbol_type,
                s.name,
                s.full_name,
                s.parent_symbol,
                s.scope,
                s.visibility,
                s.is_static,
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
                strip(&r.source_file),
                r.target_file.as_deref().map(strip),
                r.confidence,
                Value::Object(r.annotations.clone())
            ])
            .to_string(),
        );
    }
    rows
}

fn parse_one(path: &str, text: &str) -> ParseResult {
    parse_source(path, &Source::from_text(text)).result
}

fn targets(result: &ParseResult, kind: RelationshipType) -> Vec<(String, String)> {
    result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == kind)
        .map(|r| (r.source_symbol.clone(), r.target_symbol.clone()))
        .collect()
}

fn pair(a: &str, b: &str) -> (String, String) {
    (a.to_owned(), b.to_owned())
}

#[test]
fn the_golden_fixture_matches_the_python_parser() {
    let dir = write_tree("golden", FIXTURES);
    let names = sources();
    let results = parse_tree(&dir, &names);
    let prefix = format!("{}/", dir.to_string_lossy());
    let mut actual = Vec::new();
    for name in &names {
        actual.push(format!("== {name}"));
        actual.extend(render(&results[&format!("{prefix}{name}")], &prefix));
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
    let names = sources();
    let first = parse_tree(&dir, &names);
    for _ in 0..5 {
        assert_eq!(parse_tree(&dir, &names), first);
    }
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn an_invalid_or_missing_file_is_an_error_result() {
    let dir = write_tree("errors", &[("bad.js", b"const a = '\xff';\n")]);
    let bad = dir.join("bad.js").to_string_lossy().into_owned();
    let missing = dir.join("missing.js").to_string_lossy().into_owned();
    let results = JavaScriptParser.parse_files(&[bad.clone(), missing.clone()]);
    assert_eq!(
        results[&bad].errors,
        ["'utf-8' codec can't decode byte 0xff in position 11: invalid start byte"]
    );
    assert!(results[&bad].symbols.is_empty());
    assert_eq!(
        results[&missing].errors,
        [format!("[Errno 2] No such file or directory: '{missing}'")]
    );
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn es_imports_bind_names_and_side_effect_imports_name_the_module() {
    let result = parse_one(
        "/r/m.js",
        "import D, { a, b as c } from './x';\nimport * as NS from \"y\";\nimport './side';\n",
    );
    assert_eq!(
        targets(&result, RelationshipType::Imports),
        [
            pair("m", "D"),
            pair("m", "a"),
            pair("m", "c"),
            pair("m", "NS"),
            pair("m", "./side")
        ]
    );
    // A side-effect import binds nothing, so it is not in `imports`.
    assert_eq!(result.imports, ["./x", "y"]);
}

#[test]
fn only_a_destructured_require_binds_names() {
    let output = parse_source(
        "/r/m.js",
        &Source::from_text("const { A, B: C } = require('./x');\nconst whole = require('./y');\n"),
    );
    let bound: Vec<(&str, Option<&str>)> = output
        .imports
        .values()
        .map(|b| (b.local.as_str(), b.imported.as_deref()))
        .collect();
    assert_eq!(bound, [("A", Some("A")), ("C", Some("B"))]);
    // A require binds no `imports` relationship; the plain one is a constant.
    assert!(targets(&output.result, RelationshipType::Imports).is_empty());
    let names: Vec<&str> = output
        .result
        .symbols
        .iter()
        .map(|s| s.name.as_str())
        .collect();
    assert_eq!(names, ["whole"]);
}

#[test]
fn a_destructuring_declaration_is_named_after_its_value() {
    let result = parse_one("/r/m.js", "const { a } = source;\nconst [b] = f();\n");
    let names: Vec<&str> = result.symbols.iter().map(|s| s.name.as_str()).collect();
    assert_eq!(names, ["source"]);
}

#[test]
fn classes_define_methods_fields_and_constructors() {
    let result = parse_one(
        "/r/m.js",
        "class A extends B {\n  static x = 1;\n  #y;\n  constructor() { super(); this.p = new P(); }\n  get g() {}\n  #h() {}\n  static s() {}\n}\n",
    );
    let symbols: Vec<(SymbolType, &str, &str, bool)> = result
        .symbols
        .iter()
        .map(|s| {
            (
                s.symbol_type,
                s.name.as_str(),
                s.full_name.as_deref().unwrap_or_default(),
                s.is_static,
            )
        })
        .collect();
    assert_eq!(
        symbols,
        [
            (SymbolType::Class, "A", "m.A", false),
            (SymbolType::Field, "x", "m.A.x", true),
            (SymbolType::Field, "#y", "m.A.#y", false),
            (
                SymbolType::Constructor,
                "constructor",
                "m.A.constructor",
                false
            ),
            (SymbolType::Method, "g", "m.A.g", false),
            (SymbolType::Method, "s", "m.A.s", true),
        ]
    );
    assert_eq!(
        targets(&result, RelationshipType::Inheritance),
        [pair("A", "B")]
    );
    assert_eq!(
        targets(&result, RelationshipType::Composition),
        [pair("m.A.constructor", "P")]
    );
    let defines = targets(&result, RelationshipType::Defines);
    assert_eq!(defines.len(), 5);
    assert!(defines.iter().all(|(source, _)| source == "A"));
}

#[test]
fn callee_chains_stop_at_calls_and_private_names() {
    let result = parse_one(
        "/r/m.js",
        "a.b.c();\nx().y();\nthis.#p();\nq[0].r();\nsuper.s();\n(f)();\n",
    );
    let calls: Vec<String> = targets(&result, RelationshipType::Calls)
        .into_iter()
        .map(|(_, t)| t)
        .collect();
    assert_eq!(calls, ["a.b.c", "y", "x", "this", "r", "super.s"]);
}

#[test]
fn jsx_references_capitalised_tags_once_per_scope() {
    let result = parse_one(
        "/r/m.jsx",
        "function V() { return <A.B x={f()}><A.B /><div /><C /><my-el /></A.B>; }\n",
    );
    assert_eq!(
        targets(&result, RelationshipType::References),
        [pair("m.V", "A.B"), pair("m.V", "C")]
    );
    // Attributes of an element are not visited: `f()` is no call.
    assert!(targets(&result, RelationshipType::Calls).is_empty());
}

#[test]
fn jsdoc_feeds_metadata_and_type_references() {
    let result = parse_one(
        "/r/m.js",
        "/**\n * @param {A|string} a\n * @param {Array<B>} b\n * @returns {Promise<C>}\n * @type {D}\n */\nfunction f(a, b) {}\n",
    );
    assert_eq!(
        result.symbols[0].metadata,
        *json!({"jsdoc_params": {"a": "A|string", "b": "Array<B>"}, "jsdoc_returns": "Promise<C>"})
            .as_object()
            .expect("object")
    );
    let refs: Vec<String> = targets(&result, RelationshipType::References)
        .into_iter()
        .map(|(_, t)| t)
        .collect();
    assert_eq!(refs, ["A", "B", "C", "D"]);
}

#[test]
fn body_references_follow_the_stack_order() {
    let result = parse_one(
        "/r/m.js",
        "const K = 1;\nfunction g() {}\nfunction f(K2) { const local = K; g(); return K + local; }\n",
    );
    let refs: Vec<(String, String, Option<u32>)> = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::References)
        .map(|r| {
            (
                r.source_symbol.clone(),
                r.target_symbol.clone(),
                r.source_range.map(|r| r.start.column),
            )
        })
        .collect();
    // The last statement is walked first, so `K` is the one in `K + local`.
    assert_eq!(
        refs,
        [
            ("m.f".to_owned(), "K".to_owned(), Some(46)),
            ("m.f".to_owned(), "g".to_owned(), Some(34)),
        ]
    );
}

#[test]
fn relative_specifiers_resolve_through_the_file_system() {
    let dir = write_tree(
        "paths",
        &[
            ("a/m.js", b""),
            ("a/x.js", b""),
            ("a/y.jsx", b""),
            ("a/pkg/index.jsx", b""),
            ("b.cjs", b""),
        ],
    );
    let from = dir.join("a/m.js").to_string_lossy().into_owned();
    let root = dir.to_string_lossy().into_owned();
    let resolve = |s: &str| resolve_import_path(&from, s);
    assert_eq!(resolve("./x"), Some(format!("{root}/a/x.js")));
    assert_eq!(resolve("./x.js"), Some(format!("{root}/a/x.js")));
    // The suffix is replaced, not appended.
    assert_eq!(resolve("./y.min"), Some(format!("{root}/a/y.jsx")));
    assert_eq!(resolve("./pkg"), Some(format!("{root}/a/pkg/index.jsx")));
    // `..` stays in the path.
    assert_eq!(resolve("../b"), Some(format!("{root}/a/../b.cjs")));
    assert_eq!(resolve("react"), None);
    assert_eq!(resolve("./none"), None);
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_this_call_resolves_only_in_a_file_named_after_its_class() {
    let dir = write_tree(
        "this",
        &[
            ("Dog.js", b"class Dog { bark() { this.run(); } run() {} }\n"),
            ("cat.js", b"class Cat { meow() { this.run(); } run() {} }\n"),
        ],
    );
    let results = parse_tree(&dir, &["Dog.js", "cat.js"]);
    let call = |file: &str| {
        let result = &results[&dir.join(file).to_string_lossy().into_owned()];
        result
            .relationships
            .iter()
            .find(|r| r.relationship_type == RelationshipType::Calls)
            .map(|r| r.annotations.get("call_kind").cloned())
    };
    assert_eq!(call("Dog.js"), Some(Some(json!("instance_method"))));
    assert_eq!(call("cat.js"), Some(None));
    let _ = std::fs::remove_dir_all(&dir);
}
