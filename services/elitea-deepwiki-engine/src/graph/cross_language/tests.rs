use super::*;
use crate::graph::NodeData;

fn node(
    graph: &mut CodeGraph,
    id: &str,
    language: &str,
    rel_path: &str,
    name: &str,
    parent: Option<&str>,
) {
    graph.add_node(
        id,
        NodeData {
            language: language.into(),
            rel_path: rel_path.into(),
            symbol_name: name.to_owned(),
            parent_symbol: parent.map(str::to_owned),
            ..NodeData::default()
        },
    );
}

fn contract(graph: &mut CodeGraph, id: &str, surface: &str) {
    graph.add_node(
        id,
        NodeData {
            symbol_type: "contract".into(),
            signature: "rest".to_owned(),
            symbol_name: surface.to_owned(),
            ..NodeData::default()
        },
    );
}

fn rel(graph: &mut CodeGraph, source: &str, target: &str, rel_type: &str) {
    graph.add_edge(
        source,
        target,
        EdgeData {
            rel_type: rel_type.to_owned().into(),
            ..EdgeData::default()
        },
    );
}

// The graph and the expected edges come from running
// `run_cross_language_linker` on the same graph in Python; the weights are
// Python's `repr`, compared bit for bit.
#[test]
fn l1_and_l3_match_the_python_linker() {
    let mut graph = CodeGraph::new();
    node(&mut graph, "py:h", "python", "app/a.py", "H", None);
    node(&mut graph, "ts:c", "TypeScript", "app/b.ts", "C", None);
    node(&mut graph, "go:s", "go", "svc/c.go", "S", None);
    node(&mut graph, "py:h2", "python", "", "H2", None);
    contract(&mut graph, "contract::rest::GET /x", "GET /x");
    contract(&mut graph, "contract::rest::GET /lonely", "GET /lonely");
    for (source, rel_type) in [
        ("py:h", "defines"),
        ("ts:c", "consumes"),
        ("go:s", "defines"),
        ("py:h2", "defines"),
    ] {
        rel(&mut graph, source, "contract::rest::GET /x", rel_type);
    }
    rel(&mut graph, "ts:c", "contract::rest::GET /lonely", "calls");
    rel(&mut graph, "py:h", "contract::rest::GET /lonely", "defines");
    node(&mut graph, "py:h.run", "python", "", "H.run", Some("py:h"));
    node(
        &mut graph,
        "ts:c.run",
        "typescript",
        "",
        "run",
        Some("ts:c"),
    );
    node(
        &mut graph,
        "ts:c.other",
        "typescript",
        "",
        "other",
        Some("ts:c"),
    );

    let edges =
        run_cross_language_linker(&graph, &[], None, &HitsPerNode::new(), &HitsPerNode::new());
    let got: Vec<(&str, &str, &str, f64)> = edges
        .iter()
        .map(|(s, t, d)| (s.as_str(), t.as_str(), &*d.rel_type, d.weight))
        .collect();
    assert_eq!(
        got,
        [
            (
                "py:h",
                "ts:c",
                "cross_language_L1",
                0.434_934_454_191_728_27
            ),
            ("py:h", "go:s", "cross_language_L1", 0.304_454_117_934_209_8),
            ("ts:c", "go:s", "cross_language_L1", 0.304_454_117_934_209_8),
            ("ts:c", "py:h2", "cross_language_L1", 0.3),
            ("go:s", "py:h2", "cross_language_L1", 0.3),
            ("py:h.run", "ts:c.run", "cross_language_L3", 0.3),
        ]
    );
    let first = &edges[0].2;
    assert_eq!(first.edge_class, "cross_language");
    assert_eq!(
        crate::pyjson::dumps(&Value::Object(first.annotations.to_map())),
        r#"{"via": ["contract=contract::rest::GET /x"]}"#
    );
    assert_eq!(
        first.extra["provenance"],
        json!({"source": "cross_language_linker", "level": "L1", "matcher": "api_surface:rest", "surface": "GET /x"})
    );
    assert!(edges[5].2.annotations.is_empty());
    assert_eq!(
        edges[5].2.extra["provenance"]["parent_pair"],
        json!(["py:h", "ts:c"])
    );
}

#[test]
fn without_contracts_l1_pairs_by_surface_key() {
    use crate::graph::api_surface::ApiSurface;
    let mut graph = CodeGraph::new();
    node(&mut graph, "a", "python", "x/a.py", "A", None);
    node(&mut graph, "b", "go", "x/b.go", "B", None);
    let surface = |hint: f64| ApiSurface {
        kind: "grpc".to_owned(),
        surface: "grpc:S/M".to_owned(),
        weight_hint: hint,
        metadata: Map::new(),
    };
    let mut surfaces = SurfacesByNode::new();
    surfaces.insert("a".to_owned(), vec![surface(0.8)]);
    surfaces.insert("b".to_owned(), vec![surface(0.7)]);
    let edges = link_l1_api_surface(&graph, Some(&surfaces));
    assert_eq!(edges.len(), 1);
    // 0.7 · 0.75 · 1.0 / ln 3.
    let expected = 0.7 * f64::midpoint(0.8, 0.7) * 1.0 * (1.0 / 3f64.ln());
    assert!((edges[0].2.weight - expected.clamp(0.3, 0.8)).abs() < 1e-12);
    assert_eq!(
        link_api_surface_any_language(&graph, &surfaces)[0]
            .2
            .rel_type,
        "api_surface_any_language"
    );
}

#[test]
fn l0_and_l2_follow_python() {
    let mut graph = CodeGraph::new();
    node(&mut graph, "a", "python", "x/a.py", "A", None);
    node(&mut graph, "b", "go", "y/b.go", "B", None);
    node(&mut graph, "c", "python", "x/c.py", "C", None);
    let l0 = link_l0_exact(
        &graph,
        &[
            CrossLanguageRelationship {
                source: "a".into(),
                target: "b".into(),
                confidence: 0.95,
                matcher: "parser_exact".into(),
            },
            CrossLanguageRelationship {
                source: "a".into(),
                target: "c".into(),
                confidence: 0.8,
                matcher: "parser_exact".into(),
            },
        ],
    );
    assert_eq!(l0.len(), 1);
    assert!((l0[0].2.weight - 0.8).abs() < f64::EPSILON);
    let mut fts = HitsPerNode::new();
    // c: 1/61 alone is below the 0.02 threshold; b: 1/61 + 1/61 is not.
    fts.insert("a".into(), vec!["b".into(), "c".into()]);
    let mut vec_hits = HitsPerNode::new();
    vec_hits.insert("a".into(), vec!["b".into()]);
    let l2 = link_l2_hybrid(&graph, &fts, &vec_hits);
    assert_eq!(l2.len(), 1);
    assert_eq!((l2[0].0.as_str(), l2[0].1.as_str()), ("a", "b"));
    // 0.5 · 2/61 · 0.7 is below the floor.
    assert!((l2[0].2.weight - 0.3).abs() < f64::EPSILON);
    let fused = rrf_fuse(&[&["x".to_owned(), "y".to_owned()], &["y".to_owned()]], 60);
    assert_eq!(fused[0].0, "y");
}
