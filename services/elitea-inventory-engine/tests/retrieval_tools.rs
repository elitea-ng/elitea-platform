//! The graph-reading tools against the Python handlers: `graph.json` was
//! built by the real `KnowledgeGraph`, and `goldens.json` holds what the
//! real `tool_operations` handlers answered over it
//! (frozen; its generator, which documents the order and deviation
//! patches it applied, is recorded in `fixtures/PROVENANCE.md`).

use elitea_engine_core::pyjson::dumps;
use elitea_inventory_engine::graph::Graph;
use elitea_inventory_engine::retrieval::view::GraphView;
use elitea_inventory_engine::retrieval::{Call, dispatch};
use serde_json::{Map, Value, json};

const GRAPH: &str = include_str!("fixtures/retrieval/graph.json");
const GOLDENS: &str = include_str!("fixtures/retrieval/goldens.json");

fn view() -> GraphView {
    let document: Value = serde_json::from_str(GRAPH).unwrap();
    GraphView::new(Graph::from_node_link(&document).unwrap(), 7)
}

fn goldens() -> Value {
    serde_json::from_str(GOLDENS).unwrap()
}

fn run(view: &GraphView, family: &str, tool: &str, params: &Value) -> String {
    let params = params.as_object().unwrap();
    let call = Call {
        tool,
        family,
        params,
        view,
    };
    let answered = dispatch(&call)
        .unwrap_or_else(|| panic!("{family}/{tool} is not served"))
        .unwrap_or_else(|e| panic!("{family}/{tool} {params:?} failed: {e:?}"));
    assert_eq!(answered["success"], json!(true));
    answered["result"].as_str().unwrap().to_owned()
}

/// D5: the port sends no raw vectors, so they leave the expectation too.
fn without_embeddings(value: &mut Value) -> bool {
    let mut removed = false;
    match value {
        Value::Object(fields) => {
            removed |= fields.shift_remove("embedding").is_some();
            for child in fields.values_mut() {
                removed |= without_embeddings(child);
            }
        }
        Value::Array(items) => {
            for child in items {
                removed |= without_embeddings(child);
            }
        }
        _ => {}
    }
    removed
}

/// D5 in text: the detail page's `- embedding: N items` property line
/// (Python listed the vector) leaves the expectation, and with it a
/// `**Properties:**` header it was the only line of.
fn without_embedding_property(text: &str) -> (String, bool) {
    let mut dropped = false;
    let kept: Vec<&str> = text
        .split_inclusive('\n')
        .filter(|line| {
            let vector = line.starts_with("- embedding: ") && line.trim_end().ends_with(" items");
            dropped |= vector;
            !vector
        })
        .collect();
    let mut text = kept.concat();
    if dropped {
        // A vector that was the only property leaves no empty section.
        text = text.replace("\n**Properties:**\n\n", "\n");
        if let Some(stripped) = text.strip_suffix("\n**Properties:**\n") {
            text = stripped.to_owned();
        }
    }
    (text, dropped)
}

/// Arrays sorted by their serialisation, recursively (an `unordered` case).
fn canonical(value: &Value) -> Value {
    match value {
        Value::Object(fields) => {
            let mut keys: Vec<&String> = fields.keys().collect();
            keys.sort();
            Value::Object(
                keys.into_iter()
                    .map(|k| (k.clone(), canonical(&fields[k])))
                    .collect::<Map<_, _>>(),
            )
        }
        Value::Array(items) => {
            let mut items: Vec<Value> = items.iter().map(canonical).collect();
            items.sort_by_key(ToString::to_string);
            Value::Array(items)
        }
        other => other.clone(),
    }
}

fn sorted_lines(text: &str) -> Vec<&str> {
    let mut lines: Vec<&str> = text.lines().collect();
    lines.sort_unstable();
    lines
}

fn is_node_id(goldens: &Value, text: &str) -> bool {
    goldens["ids"]
        .as_object()
        .unwrap()
        .values()
        .any(|id| id == text)
}

/// The cases whose golden IS the port's answer (no deviation case).
fn compared(case: &Value, goldens: &Value) -> bool {
    let tool = case["tool"].as_str().unwrap();
    let json_form = case["params"]["output_format"] == json!("json");
    let unpatched_d3 = matches!(
        tool,
        "get_entity_content" | "impact_analysis" | "list_entities_by_type"
    ) && !json_form
        && !case["patches"]
            .as_array()
            .unwrap()
            .iter()
            .any(|p| p == "citation");
    let by_id = case["params"]["entity_name"]
        .as_str()
        .is_some_and(|name| is_node_id(goldens, name));
    case["expected"].get("result").is_some() && !unpatched_d3 && !by_id
}

#[test]
fn every_tool_answers_what_the_python_handler_answered() {
    let view = view();
    let goldens = goldens();
    let mut checked = 0;
    let mut embeddings_dropped = 0;
    for case in goldens["cases"].as_array().unwrap() {
        if !compared(case, &goldens) {
            continue;
        }
        let (family, tool) = (
            case["family"].as_str().unwrap(),
            case["tool"].as_str().unwrap(),
        );
        let expected = case["expected"]["result"].as_str().unwrap();
        let actual = run(&view, family, tool, &case["params"]);
        let unordered = case["unordered"] == json!(true);
        let label = format!("{family}/{tool} {}", case["params"]);
        if expected.starts_with('{') {
            let mut want: Value = serde_json::from_str(expected).unwrap();
            if without_embeddings(&mut want) {
                embeddings_dropped += 1;
            }
            if unordered {
                let got: Value = serde_json::from_str(&actual).unwrap();
                assert_eq!(canonical(&got), canonical(&want), "{label}");
            } else {
                assert_eq!(actual, dumps(&want), "{label}");
            }
        } else {
            let (expected, dropped) = without_embedding_property(expected);
            if dropped {
                embeddings_dropped += 1;
            }
            if unordered {
                assert_eq!(sorted_lines(&actual), sorted_lines(&expected), "{label}");
            } else {
                assert_eq!(actual, expected, "{label}");
            }
        }
        checked += 1;
    }
    assert!(checked >= 160, "only {checked} goldens compared");
    assert!(embeddings_dropped > 0, "the fixture exercises D5");
}

#[test]
fn the_json_forms_python_could_not_serialise_answer_edges() {
    // D2: Python raised TypeError on these; the goldens are the patched
    // handler's answer, which the main test compares.
    let goldens = goldens();
    let mut raised: Vec<String> = goldens["cases"]
        .as_array()
        .unwrap()
        .iter()
        .filter_map(|case| {
            case.get("python_raised").map(|error| {
                format!(
                    "{} {}",
                    case["tool"].as_str().unwrap(),
                    error.as_str().unwrap()
                )
            })
        })
        .collect();
    raised.sort();
    raised.dedup();
    assert_eq!(
        raised,
        [
            "impact_analysis TypeError: Object of type set is not JSON serializable",
            "list_entities_by_layer TypeError: Object of type set is not JSON serializable",
            "list_entities_by_source TypeError: Object of type set is not JSON serializable",
            "list_entities_by_type TypeError: Object of type set is not JSON serializable",
            "list_graphs NameError: name 'bucket' is not defined",
        ]
    );
    let view = view();
    let answer = run(
        &view,
        "inventory",
        "list_entities_by_layer",
        &json!({"layer": "service", "output_format": "json"}),
    );
    let document: Value = serde_json::from_str(&answer).unwrap();
    assert!(document["edges"].is_array());
    // D1: every row is addressable by id.
    for row in document["entities"].as_array().unwrap() {
        assert!(row["id"].is_string(), "{row}");
    }
}

#[test]
fn locations_come_from_citations_where_python_read_the_legacy_key() {
    // D3: Python answered `unknown` / "has no file citation" for every v1
    // entity; the port reads `citations[0]` (the golden with the citation
    // patch is compared by the main test).
    let view = view();
    let goldens = goldens();
    for case in goldens["cases"].as_array().unwrap() {
        let tool = case["tool"].as_str().unwrap();
        let patched = case["patches"]
            .as_array()
            .unwrap()
            .iter()
            .any(|p| p == "citation");
        if patched
            || case["params"]["output_format"] == json!("json")
            || !matches!(
                tool,
                "get_entity_content" | "impact_analysis" | "list_entities_by_type"
            )
        {
            continue;
        }
        let expected = case["expected"]["result"].as_str().unwrap();
        let actual = run(&view, "inventory", tool, &case["params"]);
        assert_ne!(actual, expected);
        assert!(
            expected.contains("has no file citation") || expected.contains("`unknown`"),
            "{expected}"
        );
    }
    let content = run(
        &view,
        "inventory",
        "get_entity_content",
        &json!({"entity_name": "UserService"}),
    );
    assert_eq!(
        content,
        "Could not retrieve content for 'UserService'\nLocation: src/services/user_service.py:10-80\nSource: repo\n\nThe file may not be accessible locally. Ensure base_directory is set or the source toolkit is available."
    );
}

#[test]
fn an_entity_id_resolves_where_a_name_is_expected() {
    // D4: Python answered "not found" for an id.
    let view = view();
    let goldens = goldens();
    let ids = goldens["ids"].as_object().unwrap();
    let mut seen = 0;
    for case in goldens["cases"].as_array().unwrap() {
        let Some(name) = case["params"]["entity_name"]
            .as_str()
            .filter(|n| is_node_id(&goldens, n))
        else {
            continue;
        };
        let expected = case["expected"]["result"].as_str().unwrap();
        assert!(expected.contains("not found"), "{expected}");
        let real_name = view.node(name).unwrap()["name"]
            .as_str()
            .unwrap()
            .to_owned();
        let mut by_name = case["params"].clone();
        by_name["entity_name"] = json!(real_name);
        let family = case["family"].as_str().unwrap();
        let tool = case["tool"].as_str().unwrap();
        assert_eq!(
            run(&view, family, tool, &case["params"]),
            run(&view, family, tool, &by_name)
        );
        seen += 1;
    }
    assert!(seen >= 2);
    for tool in [
        "impact_analysis",
        "get_related_entities",
        "get_entity_content",
    ] {
        let answer = run(
            &view,
            "inventory",
            tool,
            &json!({"entity_name": ids["hash_password"]}),
        );
        assert!(!answer.contains("not found"), "{tool}: {answer}");
    }
    let query = format!("related:{}", ids["create_user"].as_str().unwrap());
    let answer = run(
        &view,
        "inventory_search",
        "query_graph",
        &json!({"query": query}),
    );
    assert!(
        answer.starts_with("# Entities related to create_user (method)"),
        "{answer}"
    );
}

#[test]
fn json_answers_carry_no_embedding_vectors() {
    // D5.
    let view = view();
    for (family, tool, params) in [
        (
            "inventory",
            "search_graph",
            json!({"query": "user", "output_format": "json", "top_k": 50}),
        ),
        (
            "inventory",
            "get_entity",
            json!({"entity_name": "UserService", "output_format": "json"}),
        ),
        (
            "inventory",
            "get_entity_neighbors",
            json!({"entity_id": view.ids_named("UserService")[0]}),
        ),
        (
            "inventory_search",
            "query_graph",
            json!({"query": "type:class", "output_format": "json"}),
        ),
    ] {
        let answer = run(&view, family, tool, &params);
        assert!(!answer.contains("\"embedding\""), "{tool}");
    }
}

#[test]
fn list_graphs_answers_the_toolkit_graph() {
    // D6: Python raised NameError on every call.
    let view = view();
    let text = run(&view, "inventory", "list_graphs", &json!({}));
    assert!(
        text.starts_with("# Available Graphs in 'inventory'\n\n- **inventory** ("),
        "{text}"
    );
    assert!(text.ends_with(" KB)\n"));
    let document: Value = serde_json::from_str(&run(
        &view,
        "inventory",
        "list_graphs",
        &json!({"output_format": "json", "bucket": "b", "graph_name": "g"}),
    ))
    .unwrap();
    assert_eq!(document["bucket"], json!("b"));
    assert_eq!(document["graphs"][0]["name"], json!("g"));
    assert!(document["graphs"][0]["size"].as_u64().unwrap() > 1000);
    let empty = GraphView::default();
    assert_eq!(
        run(&empty, "inventory", "list_graphs", &json!({})),
        "No graphs found in bucket 'inventory'"
    );
}

#[test]
fn a_missing_graph_reports_no_data_and_reads_empty() {
    let empty = GraphView::default();
    assert_eq!(
        run(&empty, "inventory", "get_stats", &json!({})),
        "No graph data yet. Run ingestion to populate the knowledge graph."
    );
    let json_stats: Value = serde_json::from_str(&run(
        &empty,
        "inventory",
        "get_stats",
        &json!({"output_format": "json"}),
    ))
    .unwrap();
    assert_eq!(json_stats["node_count"], json!(0));
    assert_eq!(
        run(&empty, "inventory", "search_graph", &json!({"query": "x"})),
        "No entities found matching 'x'"
    );
    assert_eq!(
        run(
            &empty,
            "inventory_search",
            "get_entity_details",
            &json!({"entity_name": "x"})
        ),
        "Entity 'x' not found. Try search_knowledge_graph to find the correct name."
    );
}

#[test]
fn tools_route_by_family() {
    let view = view();
    let params = Map::new();
    let call = |family: &'static str, tool: &'static str| Call {
        tool,
        family,
        params: &params,
        view: &view,
    };
    assert!(dispatch(&call("inventory", "query_graph")).is_none());
    assert!(dispatch(&call("inventory_search", "get_stats")).is_none());
    assert!(dispatch(&call("inventory", "get_related_entities")).is_some());
    assert!(dispatch(&call("inventory_search", "get_related_entities")).is_some());
    let failed = dispatch(&Call {
        tool: "search_graph",
        family: "inventory",
        params: json!({"query": "x", "top_k": "many"}).as_object().unwrap(),
        view: &view,
    });
    assert!(matches!(failed, Some(Err(_))));
}

#[test]
fn the_entity_detail_page_does_not_list_the_vector_as_a_property() {
    // D5: Python printed `- embedding: 3 items` among UserService's
    // properties; the other properties stay.
    let view = view();
    for (family, tool) in [
        ("inventory", "get_entity"),
        ("inventory_search", "get_entity_details"),
    ] {
        let page = run(&view, family, tool, &json!({"entity_name": "UserService"}));
        assert!(
            page.contains("**Properties:**\n- bases: 1 items\n"),
            "{page}"
        );
        assert!(!page.contains("embedding"), "{family}/{tool}: {page}");
    }
}
