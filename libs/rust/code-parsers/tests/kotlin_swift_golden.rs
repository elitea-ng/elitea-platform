//! The Kotlin and Swift parsers against their goldens, on the sample files
//! in `tests/fixtures/{kotlin,swift}`.
//!
//! HISTORY: until 2026-10 these parsers were regex ports of the Inventory
//! engine's Python `KotlinParser` / `SwiftParser`, and `expected.json` was
//! the Python parsers' own output (parity). They are now tree-sitter
//! visitors (`tree-sitter-kotlin-ng` 1.1.0, `tree-sitter-swift` 0.7.4)
//! that keep the regex output model (symbol and relationship kinds, names,
//! `global` scope, file-stem sources), and `expected.json` is THIS crate's
//! output, re-baselined deliberately. It is no longer Python parity.
//!
//! What the re-baseline changed (symbols / relationships, regex → tree):
//!
//! * Kotlin `Encoding.kt` 3/3 → 3/1, `Models.kt` 13/22 → 15/12,
//!   `Script.kts` 2/3 → 2/2, `Services.kt` 30/25 → 26/19;
//! * Swift `Encoding.swift` 2/1 → 2/1, `Models.swift` 17/18 → 18/17,
//!   `Services.swift` 25/17 → 25/16.
//!
//! The differences, by cause:
//!
//! * regex false positives gone: Kotlin `implementation Success -> val`
//!   (a constructor with a default argument), `decorates property` (a
//!   `@property` tag inside a `KDoc`), `calls val` / `get` / `name`;
//!   Swift a class named `func` (from `class func make()`), `uses DidLoad`
//!   / `ByName` / `Request` (the tails of `viewDidLoad(`, `sortedByName(`,
//!   `urlRequest(`), `uses User` from the string `"User(\(name))"`, and
//!   `imports import` (the import pattern swallowing the next line, which
//!   hid `Foundation`, `SwiftUI`, `Combine` and `Foundation.URL`);
//! * declarations the regex missed: Kotlin `data class Failure(…= null)`
//!   and `enum class Currency(val symbol: String)` (parentheses in the
//!   header), so `Failure -> CheckoutResult` is found; Swift
//!   `private(set) var email` and `static let guest = …` (no annotation),
//!   and an actor's conformance (`SessionStore -> ObservableObject`);
//! * locals inside bodies are not symbols (Kotlin `val total`,
//!   `catalogue`, `anon` and the anonymous object's `run`);
//! * a constructor call is only `uses` (Kotlin `calls Logger` / `Pair` /
//!   `RemoteCatalogue` / `File` dropped); supertype constructor calls
//!   (`Basis()`), annotation arguments (`@Throws(…)`) and enum entries
//!   (`EUR("€")`) are not calls; calls inside lambdas are found
//!   (`runBlocking`, `launch`);
//! * Kotlin: one `import a.b.*` edge to `a.b` (`wildcard`), not a second
//!   one to `a.b.`; an object's `Base()` supertype is `inheritance`; all
//!   modifiers are kept (the regex kept the last); `type` only when
//!   declared;
//! * Swift: `type` is the annotation alone (was `String {`, `Int { get }`);
//!   `async` after the parameters sets `is_async`;
//! * members carry `parent_symbol` (their type's `full_name`);
//! * ranges are the declaration node's, from an annotation line to the
//!   real end column (the regex started after annotations and ended at
//!   column 0); columns are bytes, as in every tree-sitter visitor here;
//!   relationships sit on the node that makes them;
//! * symbols are in document order (the regex grouped them by pattern).
//!
//! Regenerate after a deliberate change with
//! `UPDATE_GOLDENS=1 cargo test -p elitea-code-parsers --test kotlin_swift_golden`
//! and review the diff. File paths are stored relative to the fixture
//! directory; the parser is handed absolute paths.

use elitea_code_parsers::parser_for;
use serde_json::{Map, Value, json};
use std::path::PathBuf;

fn fixture_dir(language: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(language)
}

/// Every sample in the fixture directory plus one missing file.
fn samples(language: &str, extensions: &[&str], missing: &str) -> Vec<String> {
    let dir = fixture_dir(language);
    let mut names: Vec<String> = std::fs::read_dir(&dir)
        .map(|entries| {
            entries
                .filter_map(Result::ok)
                .map(|e| e.file_name().to_string_lossy().into_owned())
                .filter(|n| extensions.iter().any(|x| n.ends_with(x)))
                .collect()
        })
        .unwrap_or_default();
    names.push(missing.to_owned());
    names.sort();
    names
}

/// The parser's output for every sample, paths relative to the fixture
/// directory, as the golden stores it.
fn actual(language: &str, names: &[String]) -> Value {
    let dir = fixture_dir(language);
    let prefix = format!("{}/", dir.to_string_lossy());
    let files: Vec<String> = names
        .iter()
        .map(|name| dir.join(name).to_string_lossy().into_owned())
        .collect();
    let parser = parser_for(language);
    assert!(parser.is_some(), "{language}: no parser");
    let Some(parser) = parser else {
        return Value::Null;
    };
    assert_eq!(parser.language(), language);
    let results = parser.parse_files(&files);
    assert_eq!(results.len(), names.len());
    let mut out = Map::new();
    for (name, path) in names.iter().zip(&files) {
        let result = results.get(path);
        assert!(result.is_some(), "{name}: not parsed");
        let text = serde_json::to_string(&result).unwrap_or_default();
        let relative = text.replace(&prefix, "");
        out.insert(
            name.clone(),
            serde_json::from_str(&relative).unwrap_or_default(),
        );
    }
    json!({ "language": language, "files": out })
}

/// Compare with `expected.json` (or rewrite it under `UPDATE_GOLDENS`);
/// returns the symbol and `aggregation` counts so the corpus cannot
/// shrink to nothing unnoticed.
fn check(language: &str, extensions: &[&str], missing: &str) -> (usize, usize) {
    let names = samples(language, extensions, missing);
    assert!(names.len() >= 4, "{language}: fixture corpus is missing");
    let got = actual(language, &names);
    let path = fixture_dir(language).join("expected.json");
    if std::env::var_os("UPDATE_GOLDENS").is_some() {
        let text = serde_json::to_string_pretty(&got).unwrap_or_default();
        assert!(std::fs::write(&path, text + "\n").is_ok());
    }
    let text = std::fs::read_to_string(&path).unwrap_or_default();
    let want: Value = serde_json::from_str(&text).unwrap_or_default();
    for name in &names {
        let (g, w) = (&got["files"][name], &want["files"][name]);
        for key in ["symbols", "relationships"] {
            let (ga, wa) = (
                g[key].as_array().cloned().unwrap_or_default(),
                w[key].as_array().cloned().unwrap_or_default(),
            );
            for (index, (a, b)) in ga.iter().zip(&wa).enumerate() {
                assert_eq!(a, b, "{language}/{name} {key} #{index}");
            }
            assert_eq!(ga.len(), wa.len(), "{language}/{name} {key} count");
        }
        assert_eq!(g, w, "{language}/{name}");
    }
    assert_eq!(got, want, "{language}: golden");
    let files = got["files"].as_object().cloned().unwrap_or_default();
    let count = |key: &str, pick: &dyn Fn(&Value) -> bool| -> usize {
        files
            .values()
            .map(|f| {
                f[key]
                    .as_array()
                    .map_or(0, |a| a.iter().filter(|x| pick(x)).count())
            })
            .sum()
    };
    (
        count("symbols", &|_| true),
        count("relationships", &|r| {
            r["relationship_type"] == json!("aggregation")
        }),
    )
}

#[test]
fn kotlin_matches_its_golden() {
    let (symbols, uses) = check("kotlin", &[".kt", ".kts"], "Missing.kt");
    assert!(symbols >= 40, "kotlin: only {symbols} symbols compared");
    assert!(uses > 0, "kotlin: no constructor-call uses edges");
}

#[test]
fn swift_matches_its_golden() {
    let (symbols, uses) = check("swift", &[".swift"], "Missing.swift");
    assert!(symbols >= 40, "swift: only {symbols} symbols compared");
    assert!(uses > 0, "swift: no initializer-call uses edges");
}
