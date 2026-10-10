//! The pattern, community and admin tools against the Python engine's own
//! answers (the semantic search goldens rank in PostgreSQL, so they stay
//! with the Inventory engine's `tests/retrieval_more.rs`, over these
//! fixtures).
//!
//! A deleted generator (the Inventory engine's `tests/fixtures/PROVENANCE.md`) built `graph.json` (and
//! the bare `bare.json`) with the Python `KnowledgeGraph`, loaded them back
//! as production did, and recorded in `goldens.json` what the real Python
//! code answered (see the generator at that commit for the two things it
//! arranged: start-set order, and the chat closures around the wrapper
//! methods).

mod common;

use common::Goldens;
use elitea_inventory_core::graph::Graph;
use elitea_inventory_core::retrieval::view::GraphView;
use elitea_inventory_core::retrieval::{
    Call, admin_tools, community_tools, dispatch, pattern, semantic,
};
use serde_json::{Map, Value, json};

const GRAPH: &str = include_str!("fixtures/retrieval_more/graph.json");
const BARE: &str = include_str!("fixtures/retrieval_more/bare.json");
const GOLDENS: &str = include_str!("fixtures/retrieval_more/goldens.json");

/// The handlers whose answer text depends on a JSON map's key order (see
/// `ORDER_DEPENDENT` in `tests/retrieval_tools.rs`): no pattern or community
/// tool; three admin tools' `output_format=json` documents, whose keys
/// Python wrote in insertion order. Each test fails while what it finds
/// disagrees.
const ORDER_DEPENDENT: &[&str] = &[
    "cleanup_cache json",
    "normalize_types json",
    "smart_normalize_types json",
];

fn view_of(document: &str) -> GraphView {
    let document: Value = serde_json::from_str(document).expect("graph json");
    GraphView::new(Graph::from_node_link(&document).expect("node-link"), 1)
}

fn goldens() -> Value {
    serde_json::from_str(GOLDENS).expect("goldens")
}

fn text(value: &Value) -> &str {
    value.as_str().expect("a string")
}

/// A dispatcher answer against its golden: the text results through
/// [`Goldens::check`], anything else exactly.
fn check_answer(
    seen: &mut Goldens,
    handler: &str,
    got: Option<&Result<Value, String>>,
    want: &Result<Value, String>,
    label: &str,
) {
    match (got, want) {
        (Some(Ok(got)), Ok(want)) if got["result"].is_string() && want["result"].is_string() => {
            assert_eq!(got["success"], want["success"], "{label}");
            let want = text(&want["result"]);
            let handler = if want.starts_with('{') {
                format!("{handler} json")
            } else {
                handler.to_owned()
            };
            seen.check(&handler, text(&got["result"]), want, label);
        }
        _ => assert_eq!(got, Some(want), "{label}"),
    }
}

#[test]
fn query_pattern_answers_as_the_python_chat_tool() {
    let view = view_of(GRAPH);
    let goldens = goldens();
    let cases = goldens["patterns"].as_array().expect("patterns");
    assert!(cases.len() > 40);
    let mut seen = Goldens::default();
    for case in cases {
        let pattern = text(&case["pattern"]);
        seen.check(
            "query_pattern",
            &pattern::query_pattern(&view, pattern),
            text(&case["text"]),
            &format!("pattern {pattern:?}"),
        );
    }
    seen.finish(&[]);
}

#[test]
fn find_paths_reports_the_value_error() {
    let view = view_of(GRAPH);
    let paths = pattern::find_paths(&view, "(UserService)-[:calls]->(?)", 10).expect("paths");
    assert_eq!(paths.len(), 1);
    assert_eq!(paths[0].path, ["n1", "n3"]);
    assert_eq!(paths[0].edges, ["calls"]);
    let refused = pattern::find_paths(&view, "(A)-[:x*0..1]->(B)", 10);
    assert_eq!(refused, Err("Minimum hops must be >= 1, got 0".to_owned()));
}

#[test]
fn the_pattern_vocabulary_answers_as_the_python_chat_tool() {
    let goldens = goldens();
    let mut seen = Goldens::default();
    seen.check(
        "pattern_vocabulary",
        &pattern::pattern_vocabulary(&view_of(GRAPH)),
        text(&goldens["vocabulary"]),
        "vocabulary",
    );
    seen.check(
        "pattern_vocabulary",
        &pattern::pattern_vocabulary(&view_of(BARE)),
        text(&goldens["vocabulary_bare"]),
        "vocabulary (bare)",
    );
    seen.finish(&[]);
}

#[test]
fn the_community_tools_answer_as_the_python_wrapper() {
    let view = view_of(GRAPH);
    let bare = view_of(BARE);
    assert!(community_tools::has_communities(&view));
    assert!(!community_tools::has_communities(&bare));
    let goldens = goldens();
    let tools = &goldens["communities"];
    let mut seen = Goldens::default();
    let pick = |case: &Value| {
        if case.get("bare").is_some() {
            &bare
        } else {
            &view
        }
    };
    for case in tools["list"].as_array().expect("list") {
        let top_n = case["top_n"]
            .as_u64()
            .map(|n| usize::try_from(n).expect("top_n"));
        seen.check(
            "list_communities",
            &community_tools::list_communities(pick(case), top_n),
            text(&case["text"]),
            &format!("list {case}"),
        );
    }
    for case in tools["detail"].as_array().expect("detail") {
        seen.check(
            "get_community_detail",
            &community_tools::get_community_detail(pick(case), text(&case["community_id"])),
            text(&case["text"]),
            &format!("detail {case}"),
        );
    }
    for case in tools["find"].as_array().expect("find") {
        seen.check(
            "find_entity_community",
            &community_tools::find_entity_community(&view, text(&case["entity_name"])),
            text(&case["text"]),
            &format!("find {case}"),
        );
    }
    for case in tools["search"].as_array().expect("search") {
        seen.check(
            "search_within_community",
            &community_tools::search_within_community(
                &view,
                text(&case["community_id"]),
                text(&case["query"]),
            ),
            text(&case["text"]),
            &format!("search {case}"),
        );
    }
    seen.finish(&[]);
}

/// Run `tool` through the dispatcher with `params`.
fn run(view: &GraphView, tool: &str, params: &Value) -> Option<Result<Value, String>> {
    let empty = Map::new();
    let params = params.as_object().unwrap_or(&empty);
    let call = Call {
        tool,
        family: "inventory",
        params,
        view,
    };
    dispatch(&call)
        .map(|handled| handled.map_err(|e| format!("{}: {}", e.error_type.wire_name(), e.message)))
}

/// The golden `{ok}` / `{error_type, message}` as the dispatcher's answer.
fn expected(golden: &Value) -> Result<Value, String> {
    match golden.get("ok") {
        Some(result) => Ok(json!({"success": true, "result": result})),
        None => Err(format!(
            "{}: {}",
            text(&golden["error_type"]),
            text(&golden["message"])
        )),
    }
}

#[test]
fn the_admin_tools_answer_as_the_python_handlers() {
    let view = view_of(GRAPH);
    let goldens = goldens();
    let admin = &goldens["admin"];
    let mut seen = Goldens::default();
    check_answer(
        &mut seen,
        "list_presets",
        run(&view, "list_presets", &json!({})).as_ref(),
        &expected(&admin["list_presets"]),
        "list_presets",
    );
    for case in admin["get_preset_info"].as_array().expect("cases") {
        // The intended answer: the patterns the Python handler never found.
        let answer = run(&view, "get_preset_info", &case["params"]);
        check_answer(
            &mut seen,
            "get_preset_info",
            answer.as_ref(),
            &expected(&case["intended"]),
            &case.to_string(),
        );
        // ... which only ADDS to Python's.
        if let (Some(Ok(answer)), Some(python)) = (&answer, case["python"].get("ok")) {
            assert!(text(&answer["result"]).starts_with(text(python)));
        }
    }
    for tool in ["cleanup_cache", "normalize_types", "smart_normalize_types"] {
        for case in admin[tool].as_array().expect("cases") {
            check_answer(
                &mut seen,
                tool,
                run(&view, tool, &case["params"]).as_ref(),
                &expected(&case["result"]),
                &format!("{tool} {case}"),
            );
        }
    }
    seen.finish(ORDER_DEPENDENT);
}

#[test]
fn rebuild_indices_reports_the_counts_python_lost() {
    let view = view_of(GRAPH);
    let goldens = goldens();
    let cases = goldens["admin"]["rebuild_indices"]
        .as_array()
        .expect("cases");
    // JSON: Python's answer but for the two counts it read from keys that
    // do not exist.
    let python: Value = serde_json::from_str(text(&cases[0]["result"]["ok"])).expect("python json");
    let answer = run(&view, "rebuild_indices", &cases[0]["params"])
        .expect("served")
        .expect("answered");
    let mut ours: Value = serde_json::from_str(text(&answer["result"])).expect("json");
    assert_eq!(ours["entity_count"], json!(17));
    assert_eq!(ours["relation_count"], json!(22));
    ours["entity_count"] = json!(0);
    ours["relation_count"] = json!(0);
    assert_eq!(ours, python);
    let answer = run(&view, "rebuild_indices", &cases[1]["params"])
        .expect("served")
        .expect("answered");
    assert_eq!(
        text(&answer["result"]),
        text(&cases[1]["result"]["ok"])
            .replace("Entities: 0", "Entities: 17")
            .replace("Relations: 0", "Relations: 22")
    );
}

#[test]
fn the_cache_tools_report_no_file_cache() {
    let view = view_of(GRAPH);
    let answer = run(&view, "get_cache_stats", &json!({}))
        .expect("served")
        .expect("answered");
    assert_eq!(
        text(&answer["result"]),
        format!(
            "# Graph Cache Statistics\n\n**Total Graphs:** 0\n**Total Size:** 0.0 MB\n\n{}\n",
            admin_tools::NO_CACHE_NOTE
        )
    );
    let answer = run(&view, "get_cache_stats", &json!({"output_format": "json"}))
        .expect("served")
        .expect("answered");
    let stats: Value = serde_json::from_str(text(&answer["result"])).expect("json");
    assert_eq!(stats["stats"]["total_graphs"], json!(0));
    assert_eq!(stats["graphs"], json!([]));
}

#[test]
fn maintenance_that_needs_a_model_is_refused_or_reported_skipped() {
    let view = view_of(GRAPH);
    // `class` (5 entities) is canonical; nothing qualifies under 1000 only
    // when every type is canonical — add a non-canonical one.
    let mut graph = view.graph.clone();
    let mut node = Map::new();
    node.insert("name".to_owned(), json!("odd"));
    node.insert("type".to_owned(), json!("gizmo"));
    graph.insert_node("x1".to_owned(), node);
    let odd = GraphView::new(graph, 2);
    let refused = run(&odd, "smart_normalize_types", &json!({}));
    assert!(
        matches!(refused, Some(Err(message)) if message.starts_with("RuntimeError: smart_normalize_types would map 1 entity type(s) through a model (gizmo)"))
    );
    let skipped = run(&odd, "normalize_types", &json!({"smart_threshold": 3}))
        .expect("served")
        .expect("answered");
    let report: Value = serde_json::from_str(text(&skipped["result"])).expect("json");
    assert_eq!(report["smart_normalization_ran"], json!(false));
    assert!(report["smart_normalization_skipped"].is_string());
    let falsy = run(&odd, "normalize_types", &json!({"smart": 0}))
        .expect("served")
        .expect("answered");
    let report: Value = serde_json::from_str(text(&falsy["result"])).expect("json");
    assert_eq!(report["smart_normalization_ran"], json!(0));
    assert!(report.get("smart_normalization_skipped").is_none());
}

#[test]
fn only_the_routed_admin_tools_are_served_here() {
    let view = view_of(GRAPH);
    for tool in [
        "remove_source_entities",
        "query_pattern",
        "semantic_search",
        "list_communities",
    ] {
        let call = Call {
            tool,
            family: "inventory",
            params: &Map::new(),
            view: &view,
        };
        assert!(
            admin_tools::handle(&call).is_none()
                && pattern::handle(&call).is_none()
                && semantic::handle(&call).is_none()
                && community_tools::handle(&call).is_none(),
            "{tool}"
        );
    }
}
