//! Phase 1c: the test linker (`code_graph/test_linker.py`), OFF by default
//! (`DEEPWIKI_TEST_LINKER`).
//!
//! Two heuristics, returned as edges for the caller to add:
//!
//! * same stem — a test file's module node (`test_foo` / `foo_test`, or
//!   any file under `tests/`, `test/`, `__tests__/`, `spec/`) to the
//!   production module nodes with the stripped stem in the same language;
//!   module-level only, as Python's comment explains (symbol × symbol
//!   pairs exploded to 1.25M edges on one repository);
//! * class proxy — `TestUserService` / `UserServiceTest(s)` in a test file
//!   to the production class `UserService` in the same language.
//!
//! Python reads an `is_test` NODE attribute first; no Phase 1 node carries
//! one (the index derives `is_test` from the path later), so only the path
//! and name rules decide, here as there.

use super::{CodeGraph, EdgeData};
use indexmap::IndexMap;
use serde_json::{Map, json};
use std::collections::HashSet;

/// One proposed edge: test node, production node, attributes.
pub type TestLink = (String, String, EdgeData);

const TEST_DIR_TOKENS: &[&str] = &["tests/", "test/", "__tests__/", "spec/"];
const FILE_LEVEL_TYPES: &[&str] = &["module", "module_doc", "file_doc"];

fn strip_test_prefix(name: &str) -> &str {
    name.strip_prefix("test_")
        .or_else(|| name.strip_prefix("test-"))
        .unwrap_or(name)
}

fn strip_test_suffix(name: &str) -> &str {
    name.strip_suffix("_test")
        .or_else(|| name.strip_suffix("-test"))
        .unwrap_or(name)
}

/// `_is_test_node` (the `is_test` attribute is never set; see the module
/// note).
fn is_test_node(rel_path: &str, file_name: &str) -> bool {
    let rel = rel_path.to_lowercase();
    if TEST_DIR_TOKENS.iter().any(|token| rel.contains(token)) {
        return true;
    }
    let name = file_name.to_lowercase();
    strip_test_prefix(&name).len() != name.len() || strip_test_suffix(&name).len() != name.len()
}

/// `_strip_test_decoration`: `test_foo` / `foo_test` → `foo`.
fn strip_test_decoration(file_name: &str) -> String {
    let name = file_name.to_lowercase();
    strip_test_suffix(strip_test_prefix(&name)).to_owned()
}

/// `_strip_test_class`: `TestX` → `X`, `XTest` / `XTests` → `X`.
fn strip_test_class(name: &str) -> &str {
    if let Some(rest) = name.strip_prefix("Test") {
        return rest;
    }
    if name.ends_with("Test") || name.ends_with("Tests") {
        return name.rsplit_once("Test").map_or(name, |(head, _)| head);
    }
    name
}

fn link(rel_type: &str, matcher: &str, key: &str, value: &str) -> EdgeData {
    let mut extra = Map::new();
    extra.insert(
        "provenance".to_owned(),
        json!({"source": "test_linker", "matcher": matcher, key: value}),
    );
    EdgeData {
        rel_type: rel_type.to_owned().into(),
        edge_class: "test_link".into(),
        weight: 0.5,
        extra: extra.into(),
        ..EdgeData::default()
    }
}

/// `link_same_stem`.
#[must_use]
pub fn link_same_stem(graph: &CodeGraph) -> Vec<TestLink> {
    let mut by_stem: IndexMap<(String, String), Vec<&str>> = IndexMap::new();
    let mut tests: Vec<(&str, String, String)> = Vec::new();
    for (id, data) in graph.nodes() {
        if !FILE_LEVEL_TYPES.contains(&data.symbol_type.to_lowercase().as_str()) {
            continue;
        }
        let language = data.language.to_lowercase();
        let stem = data.file_name.to_lowercase();
        if stem.is_empty() {
            continue;
        }
        if is_test_node(&data.rel_path, &data.file_name) {
            tests.push((id, strip_test_decoration(&stem), language));
        } else {
            by_stem.entry((stem, language)).or_default().push(id);
        }
    }
    let mut out = Vec::new();
    for (test, stem, language) in tests {
        let Some(candidates) = by_stem.get(&(stem.clone(), language)) else {
            continue;
        };
        for &production in candidates {
            if production != test {
                out.push((
                    test.to_owned(),
                    production.to_owned(),
                    link("test_link_same_stem", "same_stem", "stem", &stem),
                ));
            }
        }
    }
    out
}

/// `link_class_proxy`.
#[must_use]
pub fn link_class_proxy(graph: &CodeGraph) -> Vec<TestLink> {
    let mut production: IndexMap<(&str, String), Vec<&str>> = IndexMap::new();
    let mut tests: Vec<(&str, &str, String)> = Vec::new();
    for (id, data) in graph.nodes() {
        if data.symbol_type.to_lowercase() != "class" || data.symbol_name.is_empty() {
            continue;
        }
        let name = data.symbol_name.as_str();
        let language = data.language.to_lowercase();
        let test_named =
            name.starts_with("Test") || name.ends_with("Test") || name.ends_with("Tests");
        if test_named && is_test_node(&data.rel_path, &data.file_name) {
            tests.push((id, strip_test_class(name), language));
        } else {
            production.entry((name, language)).or_default().push(id);
        }
    }
    let mut out = Vec::new();
    for (test, base, language) in tests {
        let Some(candidates) = production.get(&(base, language)) else {
            continue;
        };
        for &target in candidates {
            if target != test {
                out.push((
                    test.to_owned(),
                    target.to_owned(),
                    link("test_link_class_proxy", "class_proxy", "base", base),
                ));
            }
        }
    }
    out
}

/// `run_test_linker`: both heuristics, duplicate `(source, target,
/// rel_type)` triples dropped.
#[must_use]
pub fn run_test_linker(graph: &CodeGraph) -> Vec<TestLink> {
    let mut edges = link_same_stem(graph);
    edges.extend(link_class_proxy(graph));
    let mut seen: HashSet<(String, String, String)> = HashSet::new();
    edges.retain(|(source, target, data)| {
        seen.insert((source.clone(), target.clone(), data.rel_type.to_string()))
    });
    edges
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::NodeData;

    fn node(
        graph: &mut CodeGraph,
        id: &str,
        symbol_type: &str,
        name: &str,
        rel_path: &str,
        file_name: &str,
    ) {
        graph.add_node(
            id,
            NodeData {
                symbol_type: symbol_type.to_owned().into(),
                symbol_name: name.to_owned(),
                rel_path: rel_path.into(),
                file_name: file_name.into(),
                language: "python".into(),
                ..NodeData::default()
            },
        );
    }

    #[test]
    fn names_follow_the_python_rules() {
        assert_eq!(strip_test_decoration("Test_Foo"), "foo");
        assert_eq!(strip_test_decoration("foo-test"), "foo");
        assert_eq!(strip_test_decoration("testing"), "testing");
        assert_eq!(strip_test_class("TestUserService"), "UserService");
        assert_eq!(strip_test_class("UserServiceTests"), "UserService");
        // rsplit("Test", 1): the LAST "Test".
        assert_eq!(strip_test_class("ATestBTest"), "ATestB");
        assert!(is_test_node("pkg/tests/a.py", "a"));
        assert!(is_test_node("pkg/a.py", "a_test"));
        assert!(!is_test_node("pkg/latest.py", "latest"));
    }

    // Cross-checked with test_linker.run_test_linker on the same graph.
    #[test]
    fn links_modules_by_stem_and_classes_by_name() {
        let mut graph = CodeGraph::new();
        node(&mut graph, "m:foo", "module", "foo", "src/foo.py", "foo");
        node(
            &mut graph,
            "m:test_foo",
            "module",
            "test_foo",
            "tests/test_foo.py",
            "test_foo",
        );
        node(&mut graph, "c:Svc", "class", "Svc", "src/svc.py", "svc");
        node(
            &mut graph,
            "c:TestSvc",
            "class",
            "TestSvc",
            "tests/test_svc.py",
            "test_svc",
        );
        node(
            &mut graph,
            "c:SvcTests",
            "class",
            "SvcTests",
            "src/svc.py",
            "svc",
        );
        let edges = run_test_linker(&graph);
        let got: Vec<(&str, &str, &str)> = edges
            .iter()
            .map(|(s, t, d)| (s.as_str(), t.as_str(), &*d.rel_type))
            .collect();
        assert_eq!(
            got,
            [
                ("m:test_foo", "m:foo", "test_link_same_stem"),
                ("c:TestSvc", "c:Svc", "test_link_class_proxy"),
            ]
        );
        assert_eq!(edges[0].2.edge_class, "test_link");
        assert!((edges[0].2.weight - 0.5).abs() < f64::EPSILON);
        assert_eq!(
            edges[1].2.extra["provenance"],
            json!({"source": "test_linker", "matcher": "class_proxy", "base": "Svc"})
        );
    }
}
