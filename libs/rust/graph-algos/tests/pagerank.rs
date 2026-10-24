//! `personalized_pagerank` against networkx: the expected vectors in
//! `fixtures/pagerank_networkx.json` were computed once with networkx 3.2.1
//! (`nx.pagerank` on a `MultiDiGraph` of the same nodes and edges, its
//! default damping, tolerance and iteration cap, `personalization` and
//! `dangling` both the case's masses, as Aider calls it) and committed.

use elitea_graph_algos::pagerank::{PageRankParams, personalized_pagerank};
use serde_json::Value;

#[test]
fn matches_networkx_fixture() {
    let text = include_str!("fixtures/pagerank_networkx.json");
    let fixture: Value = serde_json::from_str(text).expect("fixture JSON");
    let cases = fixture["cases"].as_array().expect("cases");
    assert!(cases.len() >= 4);
    for case in cases {
        let name = case["name"].as_str().expect("name");
        let n = usize::try_from(case["n"].as_u64().expect("n")).expect("n fits");
        let edges: Vec<(u32, u32, f64)> = case["edges"]
            .as_array()
            .expect("edges")
            .iter()
            .map(|edge| {
                let node = |i: usize| u32::try_from(edge[i].as_u64().expect("node")).expect("u32");
                (node(0), node(1), edge[2].as_f64().expect("weight"))
            })
            .collect();
        let personalization: Vec<f64> = case["personalization"]
            .as_array()
            .map(|masses| masses.iter().map(|m| m.as_f64().expect("mass")).collect())
            .unwrap_or_default();
        let expected: Vec<f64> = case["expected"]
            .as_array()
            .expect("expected")
            .iter()
            .map(|r| r.as_f64().expect("rank"))
            .collect();
        let ranks = personalized_pagerank(n, &edges, &personalization, &PageRankParams::default());
        assert_eq!(ranks.len(), expected.len(), "{name}");
        for (node, (got, want)) in ranks.iter().zip(&expected).enumerate() {
            assert!(
                (got - want).abs() < 1e-6,
                "{name}: node {node}: {got} vs networkx {want}"
            );
        }
    }
}
