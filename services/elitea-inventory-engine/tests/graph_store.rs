//! The graph against the Python engine's own, and through PostgreSQL.
//!
//! `fixtures/graph_store/ops.json` is replayed here, as its deleted
//! generator replayed it against the Python `KnowledgeGraph`;
//! `graph.golden.json` is what Python wrote (`fixtures/PROVENANCE.md`). The PostgreSQL tests need `INVENTORY_TEST_DSN` (skipped without
//! it, FAILED with `INVENTORY_REQUIRE_POSTGRES=1`, as in CI):
//!
//! ```text
//! podman run -d --name invpg -e POSTGRES_USER=inventory -e POSTGRES_PASSWORD=inventory \
//!     -e POSTGRES_DB=inventory -p 15521:5432 docker.io/library/postgres:16
//! export INVENTORY_TEST_DSN=postgresql://inventory:inventory@127.0.0.1:15521/inventory
//! ```
//!
//! Each test works in its own database (`invt_<name>`), created afresh.

use elitea_inventory_engine::graph::{Citation, Graph};
use elitea_inventory_engine::store::{self, GraphKey, StoreError};
use serde_json::{Map, Value, json};
use sqlx::postgres::PgPool;
use sqlx::{Connection, PgConnection};
use std::collections::BTreeSet;

const OPS: &str = include_str!("fixtures/graph_store/ops.json");
const GOLDEN: &str = include_str!("fixtures/graph_store/graph.golden.json");

fn replay() -> Graph {
    let ops: Value = serde_json::from_str(OPS).expect("ops.json");
    let mut graph = Graph::new();
    for op in ops["operations"].as_array().expect("operations") {
        let text = |key: &str| op[key].as_str().expect(key);
        match text("op") {
            "entity" => {
                let citation = op.get("citation").map(Citation::from_value);
                let mut properties = op.get("properties").and_then(Value::as_object).cloned();
                if let Some(properties) = properties.as_mut()
                    && properties.get("long") == Some(&json!("LONG_STRING"))
                {
                    properties.insert("long".to_owned(), json!("x".repeat(1000)));
                }
                graph.add_entity(
                    text("id"),
                    text("name"),
                    text("type"),
                    citation.as_ref(),
                    properties.as_ref(),
                );
            }
            "relation" => {
                graph.add_relation(
                    text("source"),
                    text("target"),
                    text("type"),
                    op.get("properties").and_then(Value::as_object),
                );
            }
            "embedding" => {
                let vector: Vec<f64> = op["vector"]
                    .as_array()
                    .expect("vector")
                    .iter()
                    .map(|v| v.as_f64().expect("a number"))
                    .collect();
                assert!(graph.set_embedding(text("id"), &vector));
            }
            "metadata" => {
                graph
                    .metadata
                    .insert(text("key").to_owned(), op["value"].clone());
            }
            "communities" => graph.set_community_data(op["value"].clone()),
            other => panic!("unknown op {other}"),
        }
    }
    graph
}

/// The document, comparable: `last_saved` blanked, and every `_indices`
/// list a set (Python's indices are sets, listed in hash order).
fn comparable(document: &Value) -> Value {
    let mut document = document.clone();
    document["_metadata"]["last_saved"] = json!("<stamp>");
    if let Some(indices) = document.get_mut("_indices").and_then(Value::as_object_mut) {
        for section in indices.values_mut() {
            for ids in section.as_object_mut().expect("an index").values_mut() {
                let set: BTreeSet<String> = ids
                    .as_array()
                    .expect("ids")
                    .iter()
                    .map(|id| id.as_str().expect("an id").to_owned())
                    .collect();
                *ids = json!(set);
            }
        }
    }
    document
}

fn golden() -> Value {
    serde_json::from_str(GOLDEN).expect("graph.golden.json")
}

/// Equal as JSON, and in the same node and link order.
fn assert_same_document(actual: &Value, expected: &Value) {
    let ids = |document: &Value, list: &str| -> Vec<String> {
        document[list]
            .as_array()
            .expect(list)
            .iter()
            .map(|item| {
                if list == "nodes" {
                    item["id"].to_string()
                } else {
                    format!("{}->{}", item["source"], item["target"])
                }
            })
            .collect()
    };
    assert_eq!(ids(actual, "nodes"), ids(expected, "nodes"), "node order");
    assert_eq!(ids(actual, "links"), ids(expected, "links"), "link order");
    assert_eq!(comparable(actual), comparable(expected));
}

#[test]
fn the_replayed_operations_write_what_the_python_graph_wrote() {
    assert_same_document(&replay().to_node_link("now"), &golden());
}

#[test]
fn the_python_document_loads_and_exports_unchanged() {
    let Ok(loaded) = Graph::from_node_link(&golden()) else {
        panic!("the golden loads");
    };
    assert_same_document(&loaded.to_node_link("now"), &golden());
}

#[test]
fn the_text_is_python_json_dump() {
    let text = replay().to_json_text("2026-10-07T00:00:00");
    // ensure_ascii: the Greek name is escaped, as json.dump writes it.
    assert!(
        text.contains("\\u03a9\\u03bc\\u03ad\\u03b3\\u03b1"),
        "{text}"
    );
    assert!(text.contains("\"embedding\": [\n        0.1,\n        -0.25,\n        1e-07,"));
    let reparsed: Value = serde_json::from_str(&text).expect("valid JSON");
    assert_same_document(&reparsed, &golden());
}

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

const DSN_ENV: &str = "INVENTORY_TEST_DSN";
const REQUIRE_ENV: &str = "INVENTORY_REQUIRE_POSTGRES";

fn dsn() -> Option<String> {
    if let Some(value) = std::env::var(DSN_ENV).ok().filter(|v| !v.trim().is_empty()) {
        return Some(value);
    }
    let required = std::env::var(REQUIRE_ENV)
        .is_ok_and(|v| matches!(v.trim().to_lowercase().as_str(), "1" | "true" | "yes"));
    assert!(
        !required,
        "{DSN_ENV} is not set and {REQUIRE_ENV} is: the PostgreSQL graph-store tests must run"
    );
    eprintln!("{DSN_ENV} is not set: the PostgreSQL graph-store tests did NOT run");
    None
}

/// A fresh, migrated database for one test.
async fn database(name: &str) -> Option<PgPool> {
    let dsn = dsn()?;
    let database = format!("invt_{name}");
    let mut admin = PgConnection::connect(&dsn).await.expect("connect");
    sqlx::query(&format!("DROP DATABASE IF EXISTS {database} WITH (FORCE)"))
        .execute(&mut admin)
        .await
        .expect("drop");
    sqlx::query(&format!("CREATE DATABASE {database}"))
        .execute(&mut admin)
        .await
        .expect("create");
    let options = store::connect_options(&dsn)
        .expect("dsn")
        .database(&database);
    let pool = PgPool::connect_with(options).await.expect("pool");
    let applied = store::migrate(&pool).await.expect("migrate");
    assert_eq!(
        applied,
        ["0001", "0002", "0003", "0004", "0005"]
            .map(str::to_owned)
            .to_vec()
    );
    Some(pool)
}

fn key(project: i64, toolkit: i64) -> GraphKey {
    GraphKey::new(project, toolkit).expect("a key")
}

#[tokio::test]
async fn a_saved_graph_loads_back_as_the_same_document() {
    let Some(pool) = database("roundtrip").await else {
        return;
    };
    let graph = replay();
    assert_eq!(store::save(&pool, key(1, 10), &graph).await.ok(), Some(1));
    let Ok(Some((loaded, revision))) = store::load(&pool, key(1, 10)).await else {
        panic!("loads");
    };
    assert_eq!(revision, 1);
    assert_same_document(&loaded.to_node_link("now"), &golden());
    // The embedding comes back as the exact numbers that were written.
    assert_eq!(
        loaded.node("a1").and_then(|n| n.get("embedding")),
        Some(&json!([0.1, -0.25, 1e-7, 0.333_333_333_333_333_3]))
    );
    // The edge provenance graph.json loses is kept.
    let (_, _, edge) = loaded
        .edges()
        .find(|(s, t, _)| *s == "e5" && *t == "b2")
        .expect("the e5 edge");
    assert_eq!(edge.get("source"), Some(&json!("llm")));
}

#[tokio::test]
async fn a_save_replaces_the_graph_and_bumps_its_revision() {
    let Some(pool) = database("replace").await else {
        return;
    };
    assert_eq!(
        store::save(&pool, key(1, 10), &replay()).await.ok(),
        Some(1)
    );
    let mut smaller = Graph::new();
    smaller.add_entity("z", "Z", "module", None, None);
    assert_eq!(store::save(&pool, key(1, 10), &smaller).await.ok(), Some(2));
    let Ok(Some((loaded, 2))) = store::load(&pool, key(1, 10)).await else {
        panic!("loads at revision 2");
    };
    assert_eq!(loaded.node_count(), 1);
    assert_eq!(loaded.edge_count(), 0);
    let rows: i64 = sqlx::query_scalar("SELECT count(*) FROM inventory_graph.relations")
        .fetch_one(&pool)
        .await
        .expect("count");
    assert_eq!(rows, 0, "the old relations went with their entities");
}

#[tokio::test]
async fn graphs_are_separate_per_project_and_toolkit() {
    let Some(pool) = database("tenancy").await else {
        return;
    };
    let graph = replay();
    store::save(&pool, key(1, 10), &graph).await.expect("save");
    // The same toolkit id in another project, another toolkit in this one.
    assert!(matches!(store::load(&pool, key(2, 10)).await, Ok(None)));
    assert!(matches!(store::load(&pool, key(1, 11)).await, Ok(None)));
    let mut other = Graph::new();
    other.add_entity("a1", "Elsewhere", "class", None, None);
    store::save(&pool, key(2, 10), &other).await.expect("save");
    assert_eq!(store::delete(&pool, key(2, 10)).await.ok(), Some(true));
    assert_eq!(store::delete(&pool, key(2, 10)).await.ok(), Some(false));
    let Ok(Some((kept, _))) = store::load(&pool, key(1, 10)).await else {
        panic!("project 1 keeps its graph");
    };
    assert_eq!(kept.node_count(), graph.node_count());
}

#[tokio::test]
async fn generated_columns_and_the_citation_index_answer_lookups() {
    let Some(pool) = database("lookups").await else {
        return;
    };
    store::save(&pool, key(1, 10), &replay())
        .await
        .expect("save");
    let citing: Vec<String> = sqlx::query_scalar(
        "SELECT entity_id FROM inventory_graph.entities
          WHERE project_id = 1 AND application_id = 10
            AND citations @> '[{\"file_path\": \"src/api.py\"}]'
          ORDER BY ordinal",
    )
    .fetch_all(&pool)
    .await
    .expect("query");
    assert_eq!(citing, ["a1"], "a merged citation is findable");
    let (name, layer, community): (Option<String>, Option<String>, Option<String>) =
        sqlx::query_as(
            "SELECT name, layer, community_id FROM inventory_graph.entities
              WHERE project_id = 1 AND application_id = 10 AND entity_id = 'e5'",
        )
        .fetch_one(&pool)
        .await
        .expect("e5");
    assert_eq!(name.as_deref(), Some("overridden by a property"));
    assert_eq!(layer.as_deref(), Some("custom"));
    assert_eq!(community, None);
    let kinds: Vec<String> = sqlx::query_scalar(
        "SELECT relation_type FROM inventory_graph.relations
          WHERE project_id = 1 AND application_id = 10 AND source_id = 'a1'",
    )
    .fetch_all(&pool)
    .await
    .expect("relations");
    assert_eq!(kinds, ["uses"], "one edge per pair, the later type");
}

#[tokio::test]
async fn an_unstorable_graph_writes_nothing() {
    let Some(pool) = database("unstorable").await else {
        return;
    };
    store::save(&pool, key(1, 10), &replay())
        .await
        .expect("save");
    let mut nul = Graph::new();
    let properties: Map<String, Value> = [("description".to_owned(), json!("a\u{0}b"))]
        .into_iter()
        .collect();
    nul.add_entity("n", "N", "class", None, Some(&properties));
    let refused = store::save(&pool, key(1, 10), &nul).await;
    assert!(
        matches!(&refused, Err(StoreError::Unstorable(m)) if m.contains("NUL")),
        "{refused:?}"
    );
    let Ok(Some((kept, 1))) = store::load(&pool, key(1, 10)).await else {
        panic!("the previous graph and revision stay");
    };
    assert_eq!(kept.node_count(), replay().node_count());
}

#[tokio::test]
async fn migrating_twice_applies_nothing_the_second_time() {
    let Some(pool) = database("remigrate").await else {
        return;
    };
    assert_eq!(store::migrate(&pool).await.ok(), Some(Vec::new()));
    let ledger: Vec<String> = sqlx::query_scalar(
        "SELECT version FROM inventory_graph.schema_migrations ORDER BY version",
    )
    .fetch_all(&pool)
    .await
    .expect("ledger");
    assert_eq!(ledger, ["0001", "0002", "0003", "0004", "0005"]);
}

/// The shared `GraphStore` contract (libs/rust/inventory-core
/// `store_conformance`), which the desktop's SQLite store runs too: two
/// stores that pass it answer the retrieval tools alike.
#[tokio::test]
async fn the_postgresql_store_passes_the_graph_store_conformance_suite() {
    let Some(pool) = database("conformance").await else {
        return;
    };
    elitea_inventory_core::store_conformance::run(&store::PgGraphStore::new(pool)).await;
}

/// The view cache over the PostgreSQL store: no graph, no view; a view is
/// reused while the revision stands and reloaded once it moves.
#[tokio::test]
async fn the_view_cache_reloads_when_the_revision_moves() {
    let Some(pool) = database("view_cache").await else {
        return;
    };
    let key = key(1, 1);
    let views =
        elitea_inventory_engine::retrieval::ViewCache::new(store::PgGraphStore::new(pool.clone()));
    assert!(views.view(key).await.expect("view").is_none());
    let graph = replay();
    let first = store::save(&pool, key, &graph).await.expect("save");
    let view = views.view(key).await.expect("view").expect("a graph");
    assert_eq!(view.revision, first);
    let again = views.view(key).await.expect("view").expect("a graph");
    assert!(
        std::sync::Arc::ptr_eq(&view, &again),
        "reused while current"
    );
    let second = store::save(&pool, key, &graph).await.expect("save");
    let fresh = views.view(key).await.expect("view").expect("a graph");
    assert_eq!(fresh.revision, second);
    assert!(store::delete(&pool, key).await.expect("delete"));
    assert!(views.view(key).await.expect("view").is_none());
}

/// The cached view holds no vectors (they are most of a graph's bytes),
/// and nothing a reader sees changes: `get_stats` answers what the view of
/// the full graph answers, and `rank` still ranks over the stored vectors.
#[tokio::test]
async fn the_cached_view_holds_no_vectors_and_the_answers_do_not_change() {
    use elitea_inventory_core::store::GraphStore as _;
    use elitea_inventory_engine::retrieval::{Call, ViewCache, dispatch, view::GraphView};
    let Some(pool) = database("view_vectors").await else {
        return;
    };
    let key = key(1, 1);
    let mut graph = replay();
    let ids: Vec<String> = graph.nodes().map(|(id, _)| id.to_owned()).collect();
    assert!(ids.len() > 3, "the replayed graph has entities");
    for (index, id) in ids.iter().take(3).enumerate() {
        let hot = f64::from(u8::try_from(index).expect("small"));
        assert!(graph.set_embedding(id, &[1.0, hot, 0.5, 0.25]));
    }
    // An empty vector is not an embedding, as `get_stats` always counted it.
    assert!(graph.set_embedding(&ids[3], &[]));
    graph
        .metadata
        .insert("embeddings_model".to_owned(), json!("text-embed-x"));
    graph
        .metadata
        .insert("embeddings_dimension".to_owned(), json!(4));
    store::save(&pool, key, &graph).await.expect("save");

    let views = ViewCache::new(store::PgGraphStore::new(pool.clone()));
    let cached = views.view(key).await.expect("view").expect("a graph");
    assert!(
        cached
            .graph
            .nodes()
            .all(|(_, node)| !node.contains_key("embedding")),
        "no vector in the view"
    );
    assert_eq!(cached.embedded_count(), 3);
    assert!(cached.has_embeddings());
    assert!(cached.is_embedded(&ids[0]) && !cached.is_embedded(&ids[3]));

    // The stats are those of the view that holds every vector.
    let (stored, revision) = store::load(&pool, key).await.expect("load").expect("graph");
    let full = GraphView::new(stored, revision);
    let mut params = Map::new();
    params.insert("output_format".to_owned(), json!("json"));
    let stats = |view: &GraphView| {
        dispatch(&Call {
            tool: "get_stats",
            family: "inventory",
            params: &params,
            view,
        })
        .expect("served")
        .expect("answers")
    };
    assert_eq!(stats(&cached), stats(&full));
    assert_eq!(
        stats(&cached)["result"]
            .as_str()
            .map(|text| text.contains("\"embeddings_count\": 3")
                && text.contains("\"has_embeddings\": true")),
        Some(true),
        "{}",
        stats(&cached)
    );

    // The vectors are stored: rank compares them there.
    let ranked = store::PgGraphStore::new(pool.clone())
        .rank(key, &[1.0, 0.0, 0.5, 0.25], 0.99)
        .await
        .expect("rank")
        .expect("same width");
    assert_eq!(
        ranked.first().map(|(id, _)| id.as_str()),
        Some(ids[0].as_str())
    );
}
