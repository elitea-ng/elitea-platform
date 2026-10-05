//! Noise-node contraction (`graph_contraction.contract_graph_inplace`).
//!
//! Variables, parameters, fields and properties carry no architecture of
//! their own. Each is folded into the node that contains it (found through
//! `parent_symbol`), its edges are rewired onto that node with a
//! `via=src=<name>@L<line>` / `via=tgt=…` annotation, an edge that becomes a
//! self-loop is dropped, and an edge that duplicates an existing
//! `(source, target, rel_type)` merges its `via` list into that edge.
//!
//! It runs by default (`FeatureFlags.contract_noise_nodes`, env
//! `DEEPWIKI_CONTRACT_NOISE`). Which parallel edge receives a merge depends
//! on networkx's edge order, which [`CodeGraph::edges`] reproduces.

use super::pystr;
use super::{Attributes, CodeGraph, EdgeData, EdgeKey};
use indexmap::IndexMap;
use serde_json::Value;
use std::collections::{BTreeMap, HashMap, HashSet};

/// `NOISE_TYPES`.
pub const NOISE_TYPES: &[&str] = &[
    "parameter",
    "variable",
    "local_variable",
    "argument",
    "field",
    "property",
];

/// Recursion cap of `_walk_to_arch`.
const MAX_DEPTH: usize = 5;

/// What contraction did.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ContractionReport {
    pub nodes_removed: usize,
    pub edges_rewritten: usize,
    pub self_loops_dropped: usize,
    pub unresolved: usize,
    pub by_language: BTreeMap<String, usize>,
}

fn is_noise(symbol_type: &str) -> bool {
    NOISE_TYPES.contains(&symbol_type)
}

/// The node attributes the walk reads, borrowed from the graph.
struct Snapshot<'g> {
    symbol_type: &'g str,
    parent_symbol: &'g str,
}

/// `(module, rest)` of every node id → the id, borrowed from the graph.
type ByPkgFull<'g> = HashMap<(&'g str, &'g str), &'g str>;

/// `(module, rest)` of a three-part id `language::module::rest`.
fn module_and_rest(id: &str) -> Option<(&str, &str)> {
    let mut parts = id.splitn(3, "::");
    let _language = parts.next()?;
    let module = parts.next()?;
    let rest = parts.next()?;
    Some((module, rest))
}

/// `_resolve_arch_parent`: the id of the node that contains `noise_id`, or
/// `None`.
fn resolve_arch_parent<'g>(
    noise_id: &str,
    parent_symbol: &str,
    by_pkg_full: &ByPkgFull<'g>,
) -> Option<&'g str> {
    let own_module = module_and_rest(noise_id).map_or("", |(module, _)| module);
    let lookup = |module: &str, rest: &str| by_pkg_full.get(&(module, rest)).copied();
    if parent_symbol.is_empty() {
        // A package-level variable belongs to the module node.
        return lookup(own_module, own_module);
    }
    if let Some((module_stem, rest)) = parent_symbol.split_once('.') {
        return lookup(module_stem, rest).or_else(|| lookup(own_module, parent_symbol));
    }
    lookup(own_module, parent_symbol)
}

/// `_walk_to_arch`: follow `parent_symbol` until a non-noise node.
fn walk_to_arch(
    id: &str,
    nodes: &HashMap<&str, Snapshot<'_>>,
    by_pkg_full: &ByPkgFull<'_>,
    depth: usize,
) -> Option<String> {
    if depth > MAX_DEPTH {
        return None;
    }
    let data = nodes.get(id)?;
    if !is_noise(data.symbol_type) {
        return Some(id.to_owned());
    }
    let target = resolve_arch_parent(id, data.parent_symbol, by_pkg_full)?;
    let target_is_noise = nodes.get(target).is_some_and(|t| is_noise(t.symbol_type));
    if target_is_noise {
        walk_to_arch(target, nodes, by_pkg_full, depth + 1)
    } else {
        Some(target.to_owned())
    }
}

/// Python's `str()` of a non-list `via` value.
fn python_str(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::Null => "None".to_owned(),
        other => other.to_string(),
    }
}

/// `anns.get("via")` as a list: missing → `[]`, a list → itself, anything
/// else → `[str(value)]`. With `or_empty`, a falsy value is `[]` too
/// (`anns.get("via") or []`).
fn via_list(annotations: &Attributes, or_empty: bool) -> Vec<Value> {
    match annotations.get("via") {
        None => Vec::new(),
        Some(Value::Array(items)) => items.clone(),
        Some(value) if or_empty && is_falsy(value) => Vec::new(),
        Some(value) => vec![Value::String(python_str(value))],
    }
}

fn is_falsy(value: &Value) -> bool {
    match value {
        Value::Null | Value::Bool(false) => true,
        Value::String(text) => text.is_empty(),
        Value::Number(number) => number.as_f64() == Some(0.0),
        Value::Array(items) => items.is_empty(),
        Value::Object(map) => map.is_empty(),
        Value::Bool(true) => false,
    }
}

/// Which noise node folds into which node, and the `via` label of each.
struct Plan {
    contract_map: IndexMap<String, String>,
    labels: HashMap<String, (String, i64)>,
}

/// Snapshot the nodes, index them by `(module, rest)`, and walk every
/// noise node to its container, in node order.
fn plan(graph: &CodeGraph, report: &mut ContractionReport) -> Plan {
    let mut by_pkg_full: ByPkgFull<'_> = HashMap::new();
    let mut nodes: HashMap<&str, Snapshot<'_>> = HashMap::new();
    for (id, data) in graph.nodes() {
        nodes.insert(
            id,
            Snapshot {
                symbol_type: &data.symbol_type,
                parent_symbol: pystr::strip(data.parent_symbol.as_deref().unwrap_or("")),
            },
        );
        if let Some((module, rest)) = module_and_rest(id) {
            by_pkg_full.insert((module, rest), id);
        }
    }
    let mut plan = Plan {
        contract_map: IndexMap::new(),
        labels: HashMap::new(),
    };
    for (id, data) in graph.nodes() {
        if !is_noise(&data.symbol_type) {
            continue;
        }
        let language = if data.language.is_empty() {
            "?".to_owned()
        } else {
            data.language.to_string()
        };
        match walk_to_arch(id, &nodes, &by_pkg_full, 0) {
            Some(target) if target != id => {
                plan.contract_map.insert(id.to_owned(), target);
                *report.by_language.entry(language).or_default() += 1;
                let label = if data.symbol_name.is_empty() {
                    id.rsplit("::").next().unwrap_or(id).to_owned()
                } else {
                    data.symbol_name.clone()
                };
                plan.labels.insert(id.to_owned(), (label, data.start_line));
            }
            _ => report.unresolved += 1,
        }
    }
    plan
}

/// Remove every edge with a noise endpoint; return the rewired copies that
/// are not self-loops, with their `via` entries added.
///
/// The edges to move are listed first (in edge order), then removed in that
/// order; a removed edge's data is reused for its rewired copy rather than
/// cloned, so the graph never holds two copies of the moved edges.
fn rewire(
    graph: &mut CodeGraph,
    plan: &Plan,
    report: &mut ContractionReport,
) -> Vec<(String, String, EdgeData)> {
    // The edges with a contracted endpoint, in edge order.
    let mut moves: Vec<(String, String, EdgeKey)> = Vec::new();
    for edge in graph.edges() {
        if plan.contract_map.contains_key(edge.source)
            || plan.contract_map.contains_key(edge.target)
        {
            moves.push((
                edge.source.to_owned(),
                edge.target.to_owned(),
                edge.key.clone(),
            ));
        }
    }
    let mut to_add = Vec::new();
    for (u, v, key) in moves {
        report.edges_rewritten += 1;
        let removed = graph.remove_edge(&u, &v, &key);
        let target_u = plan.contract_map.get(&u);
        let target_v = plan.contract_map.get(&v);
        let new_u = target_u.map_or(u.as_str(), String::as_str);
        let new_v = target_v.map_or(v.as_str(), String::as_str);
        if new_u == new_v {
            report.self_loops_dropped += 1;
            continue;
        }
        let Some(mut data) = removed else {
            continue;
        };
        let mut via = via_list(&data.annotations, false);
        for (old, new, role) in [(u.as_str(), new_u, "src"), (v.as_str(), new_v, "tgt")] {
            if old != new
                && let Some((label, line)) = plan.labels.get(old)
            {
                via.push(Value::String(format!("{role}={label}@L{line}")));
            }
        }
        data.annotations.insert("via".to_owned(), Value::Array(via));
        let new_u = target_u.map_or(u, Clone::clone);
        let new_v = target_v.map_or(v, Clone::clone);
        to_add.push((new_u, new_v, data));
    }
    to_add
}

/// A set of `via` items with `Value` equality: strings (nearly every item)
/// by hash, anything else by a linear search.
#[derive(Default)]
struct ViaSet {
    strings: HashSet<String>,
    others: Vec<Value>,
}

impl ViaSet {
    /// `true` when `item` was not in the set yet.
    fn insert(&mut self, item: &Value) -> bool {
        match item {
            Value::String(text) => self.strings.insert(text.clone()),
            other if self.others.contains(other) => false,
            other => {
                self.others.push(other.clone());
                true
            }
        }
    }
}

/// Add the rewired edges; one that duplicates a `(u, v, rel_type)` merges
/// its `via` list into the existing edge instead.
fn add_rewired(graph: &mut CodeGraph, to_add: Vec<(String, String, EdgeData)>) {
    // The LAST edge in networkx order for each (u, v, rel_type). Only the
    // node pairs a rewired edge lands on are ever looked up, so only those
    // are indexed.
    let pairs: HashSet<(&str, &str)> = to_add
        .iter()
        .map(|(u, v, _)| (u.as_str(), v.as_str()))
        .collect();
    let mut existing: HashMap<(String, String, String), EdgeKey> = HashMap::new();
    for edge in graph.edges() {
        if !pairs.contains(&(edge.source, edge.target)) {
            continue;
        }
        existing.insert(
            (
                edge.source.to_owned(),
                edge.target.to_owned(),
                edge.data.rel_type.to_string(),
            ),
            edge.key.clone(),
        );
    }
    drop(pairs);
    // The items of each merged edge's `via` (all distinct after its first
    // merge), so a later merge appends in place instead of rebuilding.
    let mut merged: HashMap<(String, String, String), ViaSet> = HashMap::new();
    for (u, v, data) in to_add {
        let triple = (u, v, data.rel_type.to_string());
        if let Some(key) = existing.get(&triple) {
            if let Some(target) = graph.edge_mut(&triple.0, &triple.1, key) {
                // `list(dict.fromkeys(existing_via + new_via))`.
                let seen = merged.entry(triple.clone()).or_insert_with(|| {
                    let mut seen = ViaSet::default();
                    let deduplicated: Vec<Value> = via_list(&target.annotations, true)
                        .into_iter()
                        .filter(|item| seen.insert(item))
                        .collect();
                    target
                        .annotations
                        .insert("via".to_owned(), Value::Array(deduplicated));
                    seen
                });
                if let Some(Value::Array(via)) = target.annotations.get_mut("via") {
                    via.extend(
                        via_list(&data.annotations, true)
                            .into_iter()
                            .filter(|item| seen.insert(item)),
                    );
                }
            }
            continue;
        }
        graph.add_edge(&triple.0, &triple.1, data);
        // Re-read: the FIRST parallel edge with this rel_type.
        let first = graph
            .edges_between(&triple.0, &triple.1)
            .find(|(_, d)| d.rel_type == triple.2)
            .map(|(k, _)| k.clone());
        if let Some(key) = first {
            existing.insert(triple, key);
        }
    }
}

/// `contract_graph_inplace`.
pub fn contract_graph_inplace(graph: &mut CodeGraph) -> ContractionReport {
    let mut report = ContractionReport::default();
    let plan = plan(graph, &mut report);
    if plan.contract_map.is_empty() {
        return report;
    }
    let to_add = rewire(graph, &plan, &mut report);
    add_rewired(graph, to_add);
    for id in plan.contract_map.keys() {
        if graph.remove_node(id).is_some() {
            report.nodes_removed += 1;
        }
    }
    report
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::NodeData;
    use serde_json::{Map, json};

    fn node(graph: &mut CodeGraph, id: &str, kind: &str, parent: Option<&str>, line: i64) {
        graph.add_node(
            id,
            NodeData {
                symbol_type: kind.to_owned().into(),
                symbol_name: id.rsplit("::").next().unwrap_or(id).to_owned(),
                parent_symbol: parent.map(str::to_owned),
                language: "go".into(),
                start_line: line,
                ..NodeData::default()
            },
        );
    }

    fn edge(graph: &mut CodeGraph, u: &str, v: &str, rel: &str, via: &[&str]) {
        let mut annotations = Map::new();
        if !via.is_empty() {
            annotations.insert("via".to_owned(), json!(via));
        }
        graph.add_edge(
            u,
            v,
            EdgeData {
                rel_type: rel.to_owned().into(),
                annotations: annotations.into(),
                ..EdgeData::default()
            },
        );
    }

    #[test]
    fn noise_folds_into_its_container_and_edges_carry_via() {
        let mut graph = CodeGraph::new();
        node(&mut graph, "go::svc::Server", "struct", Some("svc"), 1);
        node(
            &mut graph,
            "go::svc::Server.port",
            "field",
            Some("Server"),
            2,
        );
        node(&mut graph, "go::svc::Run", "function", None, 10);
        node(&mut graph, "go::svc::cfg", "variable", Some(""), 20);
        node(&mut graph, "go::svc::svc", "module", None, 0);
        node(&mut graph, "go::svc::lost", "variable", Some("Nowhere"), 30);
        // Run → Server.port becomes Run → Server.
        edge(
            &mut graph,
            "go::svc::Run",
            "go::svc::Server.port",
            "reads",
            &["reads@L11"],
        );
        // Server → Server.port is a self-loop after contraction.
        edge(
            &mut graph,
            "go::svc::Server",
            "go::svc::Server.port",
            "defines",
            &[],
        );
        // cfg is package-level: folds into the module node.
        edge(&mut graph, "go::svc::cfg", "go::svc::Run", "calls", &[]);
        let report = contract_graph_inplace(&mut graph);
        assert_eq!(report.nodes_removed, 2);
        assert_eq!(report.self_loops_dropped, 1);
        assert_eq!(report.unresolved, 1);
        assert!(graph.has_node("go::svc::lost"));
        let edges: Vec<(&str, &str, Value)> = graph
            .edges()
            .map(|e| (e.source, e.target, e.data.annotations["via"].clone()))
            .collect();
        assert_eq!(
            edges,
            [
                (
                    "go::svc::Run",
                    "go::svc::Server",
                    json!(["reads@L11", "tgt=Server.port@L2"])
                ),
                ("go::svc::svc", "go::svc::Run", json!(["src=cfg@L20"])),
            ]
        );
    }

    #[test]
    fn a_rewired_duplicate_merges_into_the_last_existing_edge() {
        let mut graph = CodeGraph::new();
        node(&mut graph, "py::m::A", "class", Some("m"), 1);
        node(&mut graph, "py::m::A.x", "field", Some("m.A"), 2);
        node(&mut graph, "py::m::f", "function", None, 5);
        edge(&mut graph, "py::m::f", "py::m::A", "uses_type", &["a"]);
        edge(&mut graph, "py::m::f", "py::m::A", "uses_type", &["b"]);
        edge(
            &mut graph,
            "py::m::f",
            "py::m::A.x",
            "uses_type",
            &["b", "c"],
        );
        edge(&mut graph, "py::m::f", "py::m::A.x", "uses_type", &["d"]);
        contract_graph_inplace(&mut graph);
        let vias: Vec<Value> = graph
            .edges()
            .map(|e| e.data.annotations["via"].clone())
            .collect();
        assert_eq!(vias, [json!(["a"]), json!(["b", "c", "tgt=A.x@L2", "d"]),]);
    }

    #[test]
    fn chains_of_noise_walk_to_the_first_arch_node() {
        let mut graph = CodeGraph::new();
        node(&mut graph, "ts::a::C", "class", None, 1);
        node(&mut graph, "ts::a::C.p", "property", Some("a.C"), 2);
        node(&mut graph, "ts::a::C.p.q", "parameter", Some("a.C.p"), 3);
        node(&mut graph, "ts::a::g", "function", None, 9);
        edge(&mut graph, "ts::a::g", "ts::a::C.p.q", "references", &[]);
        contract_graph_inplace(&mut graph);
        let edges: Vec<(&str, &str)> = graph.edges().map(|e| (e.source, e.target)).collect();
        assert_eq!(edges, [("ts::a::g", "ts::a::C")]);
        assert_eq!(graph.node_count(), 2);
    }

    /// 3000 fields of one class, each used by the same function: every
    /// rewired edge merges into one, whose `via` list grows by one each
    /// time. Rebuilding that list with a linear `contains` per item was
    /// cubic.
    #[test]
    fn many_merges_into_one_edge_are_fast() {
        let mut graph = CodeGraph::new();
        node(&mut graph, "py::m::A", "class", Some("m"), 1);
        node(&mut graph, "py::m::f", "function", None, 5);
        let fields = 3000;
        for i in 0..fields {
            let id = format!("py::m::A.x{i}");
            node(
                &mut graph,
                &id,
                "field",
                Some("m.A"),
                i64::try_from(i).unwrap_or(0) + 10,
            );
            edge(&mut graph, "py::m::f", &id, "uses_type", &["shared"]);
        }
        let start = std::time::Instant::now();
        contract_graph_inplace(&mut graph);
        let elapsed = start.elapsed();
        assert!(elapsed.as_secs() < 5, "{elapsed:?}");
        let vias: Vec<Value> = graph
            .edges()
            .map(|e| e.data.annotations["via"].clone())
            .collect();
        assert_eq!(vias.len(), 1);
        let via = vias[0].as_array().cloned().unwrap_or_default();
        // "shared" once, then each field's label in edge order.
        assert_eq!(via.len(), fields + 1);
        assert_eq!(via[0], json!("shared"));
        assert_eq!(via[1], json!("tgt=A.x0@L10"));
        assert_eq!(
            via[fields],
            json!(format!("tgt=A.x{}@L{}", fields - 1, fields + 9))
        );
    }
}
