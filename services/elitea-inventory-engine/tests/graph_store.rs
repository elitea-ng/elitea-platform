//! The graph against the Python engine's own, and through PostgreSQL.
//!
//! `fixtures/graph_store/ops.json` is replayed here, as its deleted
//! generator replayed it against the Python `KnowledgeGraph`;
//! `graph.golden.json` is what Python wrote (`fixtures/PROVENANCE.md`). The PostgreSQL tests need `INVENTORY_TEST_DSN` (skipped without
//! it, FAILED with `INVENTORY_REQUIRE_POSTGRES=1`, as in CI):
//!
//! ```text
//! podman run -d --name invpg -e POSTGRES_USER=inventory -e POSTGRES_PASSWORD=inventory \
//!     -e POSTGRES_DB=inventory -p 15521:5432 docker.io/library/postgres:18
//! export INVENTORY_TEST_DSN=postgresql://inventory:inventory@127.0.0.1:15521/inventory
//! ```
//!
//! Each test works in its own database (`invt_<name>`), created afresh.
//!
//! Plain PostgreSQL: the store needs no extension (#1359; the entity vectors
//! are in elitea-vector, faked in process here). The upgrade tests also run
//! the released pgvector chain for real when the server offers the `vector`
//! extension (a `pgvector/pgvector` image), and stand in for it otherwise.

mod common;

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
    let pool = empty_database(name).await?;
    let applied = store::migrate(&pool).await.expect("migrate");
    assert_eq!(
        applied,
        [
            "0001", "0002", "0003", "0004", "0005", "0006", "0007", "0008"
        ]
        .map(str::to_owned)
        .to_vec()
    );
    Some(pool)
}

/// A fresh, EMPTY database for one test (nothing migrated).
async fn empty_database(name: &str) -> Option<PgPool> {
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
    Some(PgPool::connect_with(options).await.expect("pool"))
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
    assert_eq!(
        ledger,
        [
            "0001", "0002", "0003", "0004", "0005", "0006", "0007", "0008"
        ]
    );
}

/// The shared `GraphStore` contract (libs/rust/inventory-core
/// `store_conformance`), which the desktop's SQLite store runs too: two
/// stores that pass it answer the retrieval tools alike.
#[tokio::test]
async fn the_postgresql_store_passes_the_graph_store_conformance_suite() {
    let Some(pool) = database("conformance").await else {
        return;
    };
    // The conformance suite writes and ranks project 1's graphs; the
    // invocation's bearer reaches the fake elitea-vector for that project.
    let (fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let graphs =
        store::PgGraphStore::new(pool).with_vectors(Some(common::entity_vectors(&origin, BEARER)));
    elitea_inventory_core::store_conformance::run(&graphs).await;
    // Every graph the suite wrote it deleted, and each delete emptied the
    // graph's namespace: nothing is left behind in the vector store.
    assert_eq!(fake.point_count(), 0, "the suite's graphs left vectors");
    assert!(fake.calls().contains(&"upsert") && fake.calls().contains(&"search"));
}

/// The bearer the fake elitea-vector admits (for project 1 unless a test
/// says otherwise).
const BEARER: &str = "inventory-callback";

/// The upsert runs before the write's transaction (#1359), so two writers
/// of one graph can stage at once. The one that commits second supersedes
/// the first, and neither's sweep deletes the other's staged points: the
/// sweep deletes only the generations the row lists as stale. A write that
/// never commits leaves no points behind.
#[tokio::test]
async fn interleaved_writers_keep_the_committed_generation_whole() {
    use elitea_inventory_core::store::GraphStore as _;
    let Some(pool) = database("interleaved").await else {
        return;
    };
    let key = key(1, 7);
    let (fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let vectors = common::entity_vectors(&origin, BEARER);
    let graph = |hot: f64| {
        let mut graph = replay();
        let ids: Vec<String> = graph.nodes().map(|(id, _)| id.to_owned()).collect();
        for id in ids.iter().take(2) {
            assert!(graph.set_embedding(id, &[1.0, hot, 0.0, 0.0]));
        }
        graph
            .metadata
            .insert("embeddings_model".to_owned(), json!("text-embed-x"));
        graph
    };
    let (first, second) = (graph(0.0), graph(1.0));
    // Both stage before either commits.
    let a = store::prepare(&pool, key, &first, Some(&vectors))
        .await
        .expect("stage a");
    let b = store::prepare(&pool, key, &second, Some(&vectors))
        .await
        .expect("stage b");
    assert_eq!(fake.point_count(), 4, "two staged generations");
    for (prepared, graph) in [(b, &second), (a, &first)] {
        let mut transaction = pool.begin().await.expect("begin");
        store::write_graph(&mut transaction, key, graph, &prepared)
            .await
            .expect("write");
        transaction.commit().await.expect("commit");
        prepared.settle(&pool, key, Some(&vectors), true).await;
    }
    // `a` committed last: its points are whole, `b`'s were swept.
    assert_eq!(fake.point_count(), 2, "only the committed generation");
    let stale: Vec<String> = sqlx::query_scalar(
        "SELECT vector_stale_generations FROM inventory_graph.graphs
          WHERE project_id = 1 AND application_id = 7",
    )
    .fetch_one(&pool)
    .await
    .expect("row");
    assert!(stale.is_empty(), "{stale:?}");
    let ranked = store::PgGraphStore::new(pool.clone())
        .with_vectors(Some(vectors.clone()))
        .rank(key, &[1.0, 0.0, 0.0, 0.0], 0.99)
        .await
        .expect("rank")
        .expect("same width");
    assert_eq!(ranked.len(), 2, "the committed generation answers");

    // A write staged and then abandoned (its transaction rolled back).
    let c = store::prepare(&pool, key, &second, Some(&vectors))
        .await
        .expect("stage c");
    assert_eq!(fake.point_count(), 4);
    c.settle(&pool, key, Some(&vectors), false).await;
    assert_eq!(fake.point_count(), 2, "the uncommitted generation is gone");
    assert_eq!(staged_rows(&pool, key).await, 0, "nothing is left staged");
}

async fn staged_rows(pool: &PgPool, key: GraphKey) -> i64 {
    sqlx::query_scalar(
        "SELECT count(*) FROM inventory_graph.vector_staged_generations
          WHERE project_id = $1 AND application_id = $2",
    )
    .bind(key.project_id)
    .bind(key.application_id)
    .fetch_one(pool)
    .await
    .expect("count")
}

/// A write that dies between its upsert and its clean-up leaves its staged
/// generation listed; the graph's next write deletes its points once it is
/// older than `STAGED_MAX_AGE` (the janitor), and never a younger one,
/// which may be a live write's.
#[tokio::test]
async fn the_janitor_deletes_what_a_dead_write_staged() {
    use elitea_inventory_core::store::GraphStore as _;
    let Some(pool) = database("janitor").await else {
        return;
    };
    let key = key(1, 8);
    let (fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let vectors = common::entity_vectors(&origin, BEARER);
    let mut graph = replay();
    let ids: Vec<String> = graph.nodes().map(|(id, _)| id.to_owned()).collect();
    for id in ids.iter().take(2) {
        assert!(graph.set_embedding(id, &[1.0, 0.5, 0.0, 0.0]));
    }
    graph
        .metadata
        .insert("embeddings_model".to_owned(), json!("text-embed-x"));
    store::save_with(&pool, key, &graph, Some(&vectors))
        .await
        .expect("first write");
    assert_eq!(fake.point_count(), 2);
    // Two writes die after their upsert: one long ago, one just now.
    let _dead = store::prepare(&pool, key, &graph, Some(&vectors))
        .await
        .expect("stage");
    sqlx::query(
        "UPDATE inventory_graph.vector_staged_generations SET staged_at = now() - interval '7 hours'
          WHERE project_id = 1 AND application_id = 8",
    )
    .execute(&pool)
    .await
    .expect("age it");
    let _young = store::prepare(&pool, key, &graph, Some(&vectors))
        .await
        .expect("stage");
    assert_eq!(fake.point_count(), 6);
    assert_eq!(staged_rows(&pool, key).await, 2);
    store::save_with(&pool, key, &graph, Some(&vectors))
        .await
        .expect("next write");
    // The committed generation and the young staged one remain.
    assert_eq!(fake.point_count(), 4, "the old staged generation is gone");
    assert_eq!(staged_rows(&pool, key).await, 1, "the young one is kept");
    // The graph's deletion takes the rest with the namespace.
    assert!(
        store::PgGraphStore::new(pool.clone())
            .with_vectors(Some(vectors))
            .delete(key)
            .await
            .expect("delete")
    );
    assert_eq!(fake.point_count(), 0);
    assert_eq!(staged_rows(&pool, key).await, 0);
}

/// A delete whose elitea-vector call fails deletes nothing: the graph, its
/// recorded generation and its staged ones stay, so no point is left that
/// nothing lists, and the same call made again deletes both. A delete
/// without a bearer refuses a graph that holds vectors.
#[tokio::test]
async fn a_failed_vector_delete_keeps_the_graph_and_its_records() {
    use elitea_inventory_core::store::GraphStore as _;
    let Some(pool) = database("delete_vectors_fail").await else {
        return;
    };
    let key = key(1, 11);
    let (fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let vectors = common::entity_vectors(&origin, BEARER);
    let mut graph = replay();
    let ids: Vec<String> = graph.nodes().map(|(id, _)| id.to_owned()).collect();
    for id in ids.iter().take(2) {
        assert!(graph.set_embedding(id, &[1.0, 0.5, 0.0, 0.0]));
    }
    graph
        .metadata
        .insert("embeddings_model".to_owned(), json!("text-embed-x"));
    store::save_with(&pool, key, &graph, Some(&vectors))
        .await
        .expect("write");
    // A write staged and never settled: listed, its points in the store.
    let _staged = store::prepare(&pool, key, &graph, Some(&vectors))
        .await
        .expect("stage");
    assert_eq!(fake.point_count(), 4);
    let generation = |pool: PgPool| async move {
        sqlx::query_scalar::<_, Option<String>>(
            "SELECT vector_generation FROM inventory_graph.graphs
              WHERE project_id = 1 AND application_id = 11",
        )
        .fetch_optional(&pool)
        .await
        .expect("row")
        .flatten()
    };
    let recorded = generation(pool.clone()).await.expect("a generation");

    // Without a bearer: refused, nothing changed.
    let refused = store::PgGraphStore::new(pool.clone())
        .delete(key)
        .await
        .expect_err("a graph holding vectors needs vector access");
    assert!(
        matches!(&refused, store::StoreError::Vectors(reason) if reason.contains("elitea-vector")),
        "{refused}"
    );
    assert!(store::load(&pool, key).await.expect("load").is_some());

    // elitea-vector fails the delete: rolled back, nothing changed.
    let graphs = store::PgGraphStore::new(pool.clone()).with_vectors(Some(vectors));
    fake.fail_call("delete", 1, tonic::Status::unavailable("down"));
    let failed = graphs
        .delete(key)
        .await
        .expect_err("the vector delete failed");
    assert!(matches!(failed, store::StoreError::Vectors(_)), "{failed}");
    assert!(store::load(&pool, key).await.expect("load").is_some());
    assert_eq!(generation(pool.clone()).await, Some(recorded));
    assert_eq!(staged_rows(&pool, key).await, 1, "the staged record stays");
    assert_eq!(fake.point_count(), 4, "no point was deleted");

    // Repeated: the graph, its records and every point go.
    assert!(graphs.delete(key).await.expect("delete"));
    assert!(store::load(&pool, key).await.expect("load").is_none());
    assert_eq!(staged_rows(&pool, key).await, 0);
    assert_eq!(fake.point_count(), 0);
}

/// A graph with more entities at or above `min_score` than elitea-vector
/// returns is ranked from its stored vectors, exactly, in the same order.
#[tokio::test]
async fn the_stored_ranking_matches_the_vector_ranking() {
    use elitea_inventory_core::store::GraphStore as _;
    let Some(pool) = database("rank_stored").await else {
        return;
    };
    let key = key(1, 9);
    let (fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let vectors = common::entity_vectors(&origin, BEARER);
    let mut graph = replay();
    let ids: Vec<String> = graph.nodes().map(|(id, _)| id.to_owned()).collect();
    for (index, id) in ids.iter().enumerate() {
        let x = f64::from(u32::try_from(index % 7).expect("small"));
        assert!(graph.set_embedding(id, &[1.0, x, 0.25, -0.5]));
    }
    graph
        .metadata
        .insert("embeddings_model".to_owned(), json!("text-embed-x"));
    store::save_with(&pool, key, &graph, Some(&vectors))
        .await
        .expect("write");
    let query = [0.5, 1.0, 0.0, 0.0];
    let searches = |fake: &elitea_vector_index::testing::FakeVector| {
        fake.calls()
            .iter()
            .filter(|call| **call == "search")
            .count()
    };
    let before = searches(&fake);
    let remote = store::PgGraphStore::new(pool.clone())
        .with_vectors(Some(vectors))
        .rank(key, &query, 0.1)
        .await
        .expect("rank")
        .expect("same width");
    // The whole ranking is one search (one exact scan), not pages.
    assert_eq!(searches(&fake) - before, 1, "one search per ranking");
    let local = store::vectors::rank_stored(&pool, key, &query, 0.1)
        .await
        .expect("rank stored");
    assert_ne!(local, [] as [(std::string::String, f64); 0]);
    let order =
        |ranking: &[(String, f64)]| ranking.iter().map(|(id, _)| id.clone()).collect::<Vec<_>>();
    assert_eq!(order(&local), order(&remote));
    for ((_, l), (_, r)) in local.iter().zip(&remote) {
        assert!((l - r).abs() < 1e-6, "{l} vs {r}");
    }
}

/// A ranking whose search comes back full (more entities reach `min_score`
/// than elitea-vector returns) is answered from the stored vectors, whole:
/// every entity, not the cut page. The depth is lowered so the test needs
/// no 10000 points.
#[tokio::test]
async fn a_full_search_falls_back_to_the_stored_ranking() {
    use elitea_inventory_core::store::GraphStore as _;
    let Some(pool) = database("rank_fallback").await else {
        return;
    };
    let key = key(1, 12);
    let (fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let vectors = common::entity_vectors(&origin, BEARER);
    let mut graph = replay();
    let ids: Vec<String> = graph.nodes().map(|(id, _)| id.to_owned()).collect();
    for (index, id) in ids.iter().enumerate() {
        let x = f64::from(u32::try_from(index % 7).expect("small"));
        assert!(graph.set_embedding(id, &[1.0, x, 0.25, -0.5]));
    }
    graph
        .metadata
        .insert("embeddings_model".to_owned(), json!("text-embed-x"));
    store::save_with(&pool, key, &graph, Some(&vectors))
        .await
        .expect("write");
    let query = [0.5, 1.0, 0.0, 0.0];
    let local = store::vectors::rank_stored(&pool, key, &query, 0.1)
        .await
        .expect("rank stored");
    assert!(local.len() > 3, "the graph ranks more than the depth");
    let searches = || {
        fake.calls()
            .iter()
            .filter(|call| **call == "search")
            .count()
    };
    let before = searches();
    let ranked = store::PgGraphStore::new(pool.clone())
        .with_vectors(Some(vectors.with_ranking_depth(3)))
        .rank(key, &query, 0.1)
        .await
        .expect("rank")
        .expect("same width");
    assert_eq!(searches() - before, 1, "one search, then the fallback");
    assert_eq!(ranked, local, "the whole stored ranking, not 3");
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

/// A graph deleted and created again starts at revision 1 again. A cache
/// that holds the DELETED graph's revision 1 (another replica's, which the
/// deleting replica's `forget` never reached) must not serve it for the new
/// graph: the stamp's incarnation (migration 0006) tells the two apart.
#[tokio::test]
async fn the_view_cache_tells_a_recreated_graph_from_the_deleted_one() {
    use elitea_inventory_core::store::GraphStore as _;
    let Some(pool) = database("view_incarnation").await else {
        return;
    };
    let key = key(1, 1);
    let graphs = store::PgGraphStore::new(pool.clone());
    let views = elitea_inventory_engine::retrieval::ViewCache::new(graphs.clone());
    let mut graph = replay();
    assert_eq!(store::save(&pool, key, &graph).await.expect("save"), 1);
    let stale = views.view(key).await.expect("view").expect("a graph");
    assert_eq!(stale.revision, 1);
    let first = graphs.stamp(key).await.expect("stamp").expect("a graph");
    assert_eq!(stale.incarnation, first.incarnation);
    assert!(!first.incarnation.is_empty(), "the PostgreSQL store stamps");

    // Deleted and created again, with other content, behind the cache's
    // back: revision 1 again, another incarnation.
    assert!(store::delete(&pool, key).await.expect("delete"));
    graph.add_entity("recreated", "Recreated", "class", None, None);
    assert_eq!(store::save(&pool, key, &graph).await.expect("save"), 1);
    let second = graphs.stamp(key).await.expect("stamp").expect("a graph");
    assert_eq!(second.revision, first.revision, "the revision repeats");
    assert_ne!(second.incarnation, first.incarnation);

    let fresh = views.view(key).await.expect("view").expect("a graph");
    assert!(
        !std::sync::Arc::ptr_eq(&stale, &fresh),
        "the deleted graph's view is not served for the new graph"
    );
    assert_eq!(fresh.incarnation, second.incarnation);
    assert!(fresh.graph.node("recreated").is_some());
    assert!(stale.graph.node("recreated").is_none());
    // And a write within the new incarnation moves the revision as before.
    assert_eq!(store::save(&pool, key, &graph).await.expect("save"), 2);
    let moved = views.view(key).await.expect("view").expect("a graph");
    assert_eq!(
        (moved.revision, &moved.incarnation),
        (2, &second.incarnation)
    );
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

    // The vectors are stored: rank compares them in elitea-vector (the
    // graph was saved without vector access, so the ranking backfills it).
    let (_fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let ranked = store::PgGraphStore::new(pool.clone())
        .with_vectors(Some(common::entity_vectors(&origin, BEARER)))
        .rank(key, &[1.0, 0.0, 0.5, 0.25], 0.99)
        .await
        .expect("rank")
        .expect("same width");
    assert_eq!(
        ranked.first().map(|(id, _)| id.as_str()),
        Some(ids[0].as_str())
    );
}

// ---------------------------------------------------------------------------
// The pgvector removal (#1359): 0004 rewritten, 0008 drops what it made
// ---------------------------------------------------------------------------

/// The migrations exactly as `origin/main` released them before the
/// pgvector removal (`fixtures/PROVENANCE.md`).
const RELEASED: &[(&str, &str)] = &[
    (
        "0001_graph_store.sql",
        include_str!("fixtures/released_migrations/0001_graph_store.sql"),
    ),
    (
        "0002_sources.sql",
        include_str!("fixtures/released_migrations/0002_sources.sql"),
    ),
    (
        "0003_documents.sql",
        include_str!("fixtures/released_migrations/0003_documents.sql"),
    ),
    (
        "0004_entity_vectors.sql",
        include_str!("fixtures/released_migrations/0004_entity_vectors.sql"),
    ),
    (
        "0005_entity_embedding_hash.sql",
        include_str!("fixtures/released_migrations/0005_entity_embedding_hash.sql"),
    ),
    (
        "0006_graph_incarnation.sql",
        include_str!("fixtures/released_migrations/0006_graph_incarnation.sql"),
    ),
    (
        "0007_deleted_toolkits.sql",
        include_str!("fixtures/released_migrations/0007_deleted_toolkits.sql"),
    ),
];

/// The released 0004's checksum: the one `store::REWRITTEN` accepts.
const RELEASED_0004: &str = "a28e5e3845a21781ec21d3d376aadb8f66648b8cd731274caa6210cbc370b44f";

async fn scalar_bool(pool: &PgPool, sql: &str) -> bool {
    sqlx::query_scalar(sql).fetch_one(pool).await.expect(sql)
}

async fn has_vector_extension(pool: &PgPool) -> bool {
    scalar_bool(
        pool,
        "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')",
    )
    .await
}

async fn has_column(pool: &PgPool, table: &str, column: &str) -> bool {
    sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM information_schema.columns
                         WHERE table_schema = 'inventory_graph' AND table_name = $1
                           AND column_name = $2)",
    )
    .bind(table)
    .bind(column)
    .fetch_one(pool)
    .await
    .expect("columns")
}

/// Migrate `pool` along the RELEASED chain, as a database deployed before
/// the removal was. With the `vector` extension available the released 0004
/// runs for real; without it (plain PostgreSQL) its ledger row is recorded
/// with the released checksum and a stand-in `embedding_vector` column (a
/// generated `real[]`) takes the pgvector one's place. Returns whether the
/// real text ran.
async fn migrate_released(pool: &PgPool) -> bool {
    let released =
        elitea_pg_migrate::discover_from(RELEASED.iter().copied()).expect("released set");
    assert_eq!(
        released[3].checksum(),
        RELEASED_0004,
        "the fixture is the released text"
    );
    let available = scalar_bool(
        pool,
        "SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')",
    )
    .await;
    if available {
        elitea_pg_migrate::apply(pool, &store::LEDGER, &released)
            .await
            .expect("the released chain");
    } else {
        elitea_pg_migrate::apply(pool, &store::LEDGER, &released[..3])
            .await
            .expect("0001-0003");
        sqlx::raw_sql(
            "ALTER TABLE inventory_graph.entities
                 ADD COLUMN embedding_vector real[] GENERATED ALWAYS AS (embedding::real[]) STORED;
             INSERT INTO inventory_graph.schema_migrations (version, name, checksum)
             VALUES ('0004', 'entity_vectors', 'a28e5e3845a21781ec21d3d376aadb8f66648b8cd731274caa6210cbc370b44f');",
        )
        .execute(pool)
        .await
        .expect("the stand-in 0004");
        elitea_pg_migrate::apply(pool, &store::LEDGER, &released)
            .await
            .expect("0005-0007");
    }
    available
}

/// A fresh install creates nothing of pgvector (plain PostgreSQL suffices)
/// and records the rewritten 0004.
#[tokio::test]
async fn a_fresh_install_needs_no_pgvector() {
    let Some(pool) = database("fresh_install").await else {
        return;
    };
    assert!(!has_vector_extension(&pool).await, "no vector extension");
    assert!(!has_column(&pool, "entities", "embedding_vector").await);
    assert!(
        has_column(&pool, "entities", "embedding").await,
        "the float8[] stays"
    );
    for column in [
        "vector_generation",
        "vector_model_slug",
        "vector_dimension",
        "vector_other_dimension",
        "vector_stale_generations",
    ] {
        assert!(has_column(&pool, "graphs", column).await, "{column}");
    }
    let recorded: String = sqlx::query_scalar(
        "SELECT checksum FROM inventory_graph.schema_migrations WHERE version = '0004'",
    )
    .fetch_one(&pool)
    .await
    .expect("0004");
    let current = store::embedded().expect("embedded");
    assert_eq!(recorded, current[3].checksum(), "the rewritten text");
    assert_ne!(recorded, RELEASED_0004);
}

/// A database on the released chain (pgvector's 0004 applied) accepts the
/// rewritten 0004, applies 0008 (which drops the column and the extension),
/// and its stored vectors reach elitea-vector by the first ranking.
#[tokio::test]
async fn a_database_on_the_released_chain_upgrades_and_backfills() {
    use elitea_inventory_core::store::GraphStore as _;
    let Some(pool) = empty_database("released_upgrade").await else {
        return;
    };
    let real = migrate_released(&pool).await;
    eprintln!(
        "released 0004: {}",
        if real {
            "the real pgvector text"
        } else {
            "stand-in (no vector extension on this server)"
        }
    );
    assert_eq!(has_vector_extension(&pool).await, real);
    assert!(has_column(&pool, "entities", "embedding_vector").await);
    // A graph written by the released engine: rows only, no generation.
    sqlx::raw_sql(
        r#"INSERT INTO inventory_graph.graphs (project_id, application_id, metadata)
           VALUES (1, 5, '{"embeddings_model": "text-embed-x"}');
           INSERT INTO inventory_graph.entities
               (project_id, application_id, entity_id, ordinal, attributes, embedding)
           VALUES (1, 5, 'b', 0, '{"name": "b"}', '{2, 0, 0, 0}'),
                  (1, 5, 'a', 1, '{"name": "a"}', '{1, 0, 0, 0}'),
                  (1, 5, 'c', 2, '{"name": "c"}', '{0, 1, 0, 0}'),
                  (1, 5, 'zero', 3, '{"name": "zero"}', '{0, 0, 0, 0}'),
                  (1, 5, 'none', 4, '{"name": "none"}', NULL);"#,
    )
    .execute(&pool)
    .await
    .expect("a released graph");

    assert_eq!(
        store::migrate(&pool).await.expect("the new chain"),
        ["0008".to_owned()]
    );
    assert!(!has_column(&pool, "entities", "embedding_vector").await);
    assert!(!has_vector_extension(&pool).await, "nothing else used it");
    assert_eq!(
        store::migrate(&pool).await.expect("again"),
        Vec::<String>::new()
    );

    let key = key(1, 5);
    let (fake, origin) = common::fake_vector(&[(BEARER, 1)]).await;
    let graphs = store::PgGraphStore::new(pool.clone())
        .with_vectors(Some(common::entity_vectors(&origin, BEARER)));
    let ranked = graphs
        .rank(key, &[1.0, 0.0, 0.0, 0.0], -1.0)
        .await
        .expect("rank")
        .expect("same width");
    let order: Vec<&str> = ranked.iter().map(|(id, _)| id.as_str()).collect();
    assert_eq!(
        order,
        ["a", "b", "c"],
        "ties by entity id; the zero vector is not ranked"
    );
    assert_eq!(
        fake.point_count(),
        3,
        "the backfill published the non-zero vectors"
    );
    let (generation, slug, dimension): (Option<String>, Option<String>, Option<i32>) =
        sqlx::query_as(
            "SELECT vector_generation, vector_model_slug, vector_dimension
               FROM inventory_graph.graphs WHERE project_id = 1 AND application_id = 5",
        )
        .fetch_one(&pool)
        .await
        .expect("row");
    assert!(generation.is_some());
    assert_eq!(slug, Some(store::vectors::model_slug("text-embed-x")));
    assert_eq!(dimension, Some(4));
    // A second ranking searches what the first published: no second upsert.
    let upserts = |calls: Vec<&str>| calls.iter().filter(|call| **call == "upsert").count();
    let before = upserts(fake.calls());
    graphs
        .rank(key, &[0.0, 1.0, 0.0, 0.0], 0.5)
        .await
        .expect("rank")
        .expect("same width");
    assert_eq!(upserts(fake.calls()), before);
    // The width is still numpy's refusal, decided before elitea-vector.
    assert_eq!(
        graphs.rank(key, &[1.0, 0.0, 0.0], 0.0).await.expect("rank"),
        Err("shapes (3,) and (4,) not aligned: 3 (dim 0) != 4 (dim 0)".to_owned())
    );
}

/// The released 0004's checksum is accepted, but no other: an edited row is
/// still refused.
#[tokio::test]
async fn a_tampered_checksum_is_still_refused() {
    let Some(pool) = database("tampered").await else {
        return;
    };
    for version in ["0004", "0005"] {
        sqlx::query(
            "UPDATE inventory_graph.schema_migrations SET checksum = 'deadbeef' WHERE version = $1",
        )
        .bind(version)
        .execute(&pool)
        .await
        .expect("tamper");
        assert!(
            matches!(store::migrate(&pool).await, Err(StoreError::Migration(_))),
            "{version} with a checksum never released"
        );
        let released = if version == "0004" {
            RELEASED_0004.to_owned()
        } else {
            store::embedded().expect("embedded")[4].checksum()
        };
        sqlx::query(
            "UPDATE inventory_graph.schema_migrations SET checksum = $2 WHERE version = $1",
        )
        .bind(version)
        .bind(released)
        .execute(&pool)
        .await
        .expect("restore");
        assert_eq!(
            store::migrate(&pool).await.expect("accepted"),
            Vec::<String>::new()
        );
    }
}

/// On a server with pgvector, an extension something else still uses (say
/// another engine's table in the same database) is kept, and 0008 succeeds.
#[tokio::test]
async fn the_extension_stays_while_something_else_uses_it() {
    let Some(pool) = empty_database("shared_extension").await else {
        return;
    };
    if !migrate_released(&pool).await {
        eprintln!("this server offers no vector extension: the shared-extension case did NOT run");
        return;
    }
    sqlx::raw_sql("CREATE TABLE public.other_engine (v vector(3))")
        .execute(&pool)
        .await
        .expect("a dependent table");
    assert_eq!(
        store::migrate(&pool).await.expect("0008"),
        ["0008".to_owned()]
    );
    assert!(!has_column(&pool, "entities", "embedding_vector").await);
    assert!(has_vector_extension(&pool).await, "kept for its other user");
}
