//! The [`GraphStore`] conformance suite (feature `test-support`): what
//! every store must do, run by each implementation against itself — the
//! Inventory engine's PostgreSQL store in its `tests/graph_store.rs`, the
//! desktop's SQLite store in its own tests. Two stores that pass it answer
//! the retrieval tools alike over the same graph.
//!
//! [`run`] takes a FRESH, EMPTY store and panics on the first broken
//! promise, naming it. It uses the graphs `(1, 1)` to `(1, 10)` and
//! `(1, 105)`, and deletes each graph it wrote.

#![allow(clippy::missing_panics_doc, clippy::too_many_lines)]

use crate::graph::{Citation, Graph};
use crate::store::{
    Completion, DocumentState, GraphKey, GraphStore, Imported, RunCounts, SourceStatus,
};
use elitea_content_source::Acl;
use elitea_content_source::acl::{Principal, PrincipalKind};
use serde_json::{Map, json};
use std::collections::BTreeMap;
use std::fmt::Debug;

/// Run every check against `store`.
pub async fn run<S: GraphStore>(store: &S) {
    empty_store_reads_nothing(store, key(1)).await;
    round_trip_keeps_node_and_edge_order(store, key(2)).await;
    revisions_documents_and_status(store, key(3)).await;
    a_failed_completion_leaves_the_previous_run(store, key(4)).await;
    one_lease_holder_at_a_time(store, key(5)).await;
    ranking(store, key(6)).await;
    graphs_are_apart(store).await;
    the_administrative_writers(store, key(9)).await;
    the_view_holds_no_vectors(store, key(10)).await;
}

fn key(application_id: i64) -> GraphKey {
    GraphKey {
        project_id: 1,
        application_id,
    }
}

fn ok<T, E: Debug>(what: &str, result: Result<T, E>) -> T {
    match result {
        Ok(value) => value,
        Err(error) => panic!("{what}: the store failed: {error:?}"),
    }
}

fn status(toolkit_id: &str) -> SourceStatus {
    SourceStatus {
        toolkit_id: toolkit_id.to_owned(),
        toolkit_name: format!("source {toolkit_id}"),
        toolkit_type: "github".to_owned(),
        branch: Some("main".to_owned()),
    }
}

fn document(version: &str) -> DocumentState {
    DocumentState {
        version: version.to_owned(),
        mime: "text/x-python".to_owned(),
        acl: Acl::Project,
    }
}

fn restricted(version: &str) -> DocumentState {
    DocumentState {
        version: version.to_owned(),
        mime: "text/markdown".to_owned(),
        acl: Acl::Restricted {
            principals: vec![Principal {
                kind: PrincipalKind::User,
                id: "7".to_owned(),
            }],
        },
    }
}

/// Commit `graph` for `source_name` with `documents` as one completed run
/// (after `start`, as a run does).
async fn commit<S: GraphStore>(
    store: &S,
    key: GraphKey,
    graph: &Graph,
    source_name: &str,
    documents: &BTreeMap<String, DocumentState>,
) -> Result<i64, S::Error> {
    ok("start", store.start(key, &status(source_name)).await);
    let completion = Completion {
        toolkit_id: source_name,
        source_name,
        documents,
        counts: RunCounts {
            entities: i64::try_from(graph.node_count()).unwrap_or(i64::MAX),
            relations: i64::try_from(graph.edge_count()).unwrap_or(i64::MAX),
            documents: i64::try_from(documents.len()).unwrap_or(i64::MAX),
        },
        commit_sha: Some("abc123"),
    };
    store.complete(key, graph, &completion).await
}

/// A graph whose node, edge and attribute order is NOT sorted order, with
/// citations, properties, an embedding and graph attributes and metadata.
fn sample() -> Graph {
    let mut graph = Graph::new();
    let cite = |path: &str, line: u64| {
        Citation::from_value(&json!({
            "file_path": path, "line_start": line, "line_end": line + 9,
            "source_toolkit": "repo", "doc_id": path, "content_hash": "h"
        }))
    };
    for (id, name, kind, path) in [
        ("zeta", "Zeta", "class", "src/z.py"),
        ("alpha", "Alpha", "function", "src/a.py"),
        ("mid", "Mid", "module", "src/m.py"),
    ] {
        let mut properties = Map::new();
        properties.insert("signature".to_owned(), json!(format!("def {name}()")));
        properties.insert("bases".to_owned(), json!(["object"]));
        graph.add_entity(id, name, kind, Some(&cite(path, 1)), Some(&properties));
    }
    graph.add_relation("zeta", "mid", "imports", None);
    graph.add_relation("zeta", "alpha", "calls", None);
    graph.add_relation("alpha", "zeta", "calls", None);
    graph.set_embedding("alpha", &[0.25, -0.5, 1.0, 0.125]);
    graph
        .attributes
        .insert("name".to_owned(), json!("conformance"));
    graph
        .metadata
        .insert("embeddings_model".to_owned(), json!("text-embed-x"));
    graph
}

fn ids(graph: &Graph) -> Vec<String> {
    graph.nodes().map(|(id, _)| id.to_owned()).collect()
}

fn edges(graph: &Graph) -> Vec<(String, String)> {
    graph
        .edges()
        .map(|(source, target, _)| (source.to_owned(), target.to_owned()))
        .collect()
}

/// The graph's content, every map compared by value: a store may keep a
/// map's keys in its own order (PostgreSQL's `jsonb` does).
fn assert_same_graph(loaded: &Graph, written: &Graph, what: &str) {
    assert_eq!(ids(loaded), ids(written), "{what}: node order");
    assert_eq!(edges(loaded), edges(written), "{what}: edge order");
    for (id, node) in written.nodes() {
        assert_eq!(loaded.node(id), Some(node), "{what}: node {id}");
    }
    for (source, target, edge) in written.edges() {
        assert_eq!(
            loaded.edge(source, target),
            Some(edge),
            "{what}: edge {source}->{target}"
        );
    }
    assert_eq!(loaded.attributes, written.attributes, "{what}: attributes");
    assert_eq!(loaded.metadata, written.metadata, "{what}: metadata");
    assert_eq!(loaded.schema, written.schema, "{what}: schema");
}

async fn empty_store_reads_nothing<S: GraphStore>(store: &S, key: GraphKey) {
    assert!(ok("load", store.load(key).await).is_none(), "no graph yet");
    assert_eq!(ok("revision", store.revision(key).await), None);
    assert!(ok("versions", store.document_versions(key, "repo").await).is_empty());
    assert!(ok("restricted", store.restricted_documents(key).await).is_empty());
    let document = ok("status", store.status_document(key).await);
    assert_eq!(document["sources"], json!({}), "no source status yet");
    assert_eq!(
        ok("rank", store.rank(key, &[1.0, 0.0], 0.0).await),
        Ok(Vec::new())
    );
    assert!(!ok("delete", store.delete(key).await), "nothing to delete");
}

async fn round_trip_keeps_node_and_edge_order<S: GraphStore>(store: &S, key: GraphKey) {
    let graph = sample();
    let revision = ok(
        "complete",
        commit(store, key, &graph, "repo", &BTreeMap::new()).await,
    );
    let Some((loaded, loaded_revision)) = ok("load", store.load(key).await) else {
        panic!("the committed graph loads");
    };
    assert_eq!(
        loaded_revision, revision,
        "load reports the committed revision"
    );
    assert_same_graph(&loaded, &graph, "round trip");
    // A graph built from the loaded one (a refresh's starting point)
    // round-trips as well: removing a node and adding one keeps the order
    // the in-memory graph has (the search's tie order).
    let mut changed = loaded;
    changed.remove_file("repo", "src/a.py");
    changed.add_entity("new", "New", "class", None, None);
    changed.add_relation("new", "zeta", "uses", None);
    ok(
        "complete",
        commit(store, key, &changed, "repo", &BTreeMap::new()).await,
    );
    let Some((again, _)) = ok("load", store.load(key).await) else {
        panic!("the changed graph loads");
    };
    assert_same_graph(&again, &changed, "changed round trip");
    assert!(ok("delete", store.delete(key).await), "the graph existed");
    assert!(ok("load", store.load(key).await).is_none(), "deleted");
    assert_eq!(ok("revision", store.revision(key).await), None, "deleted");
}

async fn revisions_documents_and_status<S: GraphStore>(store: &S, key: GraphKey) {
    let graph = sample();
    let first_documents = BTreeMap::from([
        ("src/a.py".to_owned(), document("v1")),
        ("docs/secret.md".to_owned(), restricted("v1")),
    ]);
    let first = ok(
        "complete",
        commit(store, key, &graph, "repo", &first_documents).await,
    );
    assert_eq!(ok("revision", store.revision(key).await), Some(first));
    assert_eq!(
        ok("versions", store.document_versions(key, "repo").await),
        BTreeMap::from([
            ("docs/secret.md".to_owned(), "v1".to_owned()),
            ("src/a.py".to_owned(), "v1".to_owned()),
        ])
    );
    assert!(
        ok("versions", store.document_versions(key, "other").await).is_empty(),
        "versions are per source"
    );
    let restricted_rows = ok("restricted", store.restricted_documents(key).await);
    assert_eq!(restricted_rows.len(), 1, "only the restricted document");
    assert_eq!(restricted_rows[0].0, "repo");
    assert_eq!(restricted_rows[0].1, "docs/secret.md");
    assert!(restricted_rows[0].2.is_restricted());

    let report = ok("status", store.status_document(key).await);
    let repo = &report["sources"]["repo"];
    assert_eq!(repo["status"], json!("completed"), "{report}");
    assert_eq!(repo["toolkit_name"], json!("source repo"));
    assert_eq!(repo["entities_count"], json!(3));
    assert_eq!(repo["relations_count"], json!(3));
    assert_eq!(repo["documents_processed"], json!(2));
    assert!(report["last_modified"].is_string(), "{report}");

    // A second run replaces the source's versions, and the revision moves.
    let second_documents = BTreeMap::from([("src/b.py".to_owned(), document("v2"))]);
    let second = ok(
        "complete",
        commit(store, key, &graph, "repo", &second_documents).await,
    );
    assert!(
        second > first,
        "a commit bumps the revision ({first} -> {second})"
    );
    assert_eq!(
        ok("versions", store.document_versions(key, "repo").await),
        BTreeMap::from([("src/b.py".to_owned(), "v2".to_owned())])
    );
    assert!(ok("restricted", store.restricted_documents(key).await).is_empty());

    // A failed run says why, and keeps the graph.
    ok("start", store.start(key, &status("repo")).await);
    let running = ok("status", store.status_document(key).await);
    assert_eq!(running["sources"]["repo"]["status"], json!("in_progress"));
    ok(
        "fail",
        store.fail(key, "repo", "the clone was refused").await,
    );
    let failed = ok("status", store.status_document(key).await);
    assert_eq!(failed["sources"]["repo"]["status"], json!("error"));
    assert_eq!(
        failed["sources"]["repo"]["error_message"],
        json!("the clone was refused")
    );
    assert_eq!(ok("revision", store.revision(key).await), Some(second));

    assert!(ok("delete", store.delete(key).await));
    assert!(ok("versions", store.document_versions(key, "repo").await).is_empty());
    let gone = ok("status", store.status_document(key).await);
    assert_eq!(
        gone["sources"],
        json!({}),
        "a delete takes the sources' state"
    );
}

async fn a_failed_completion_leaves_the_previous_run<S: GraphStore>(store: &S, key: GraphKey) {
    let graph = sample();
    let documents = BTreeMap::from([("src/a.py".to_owned(), document("v1"))]);
    let revision = ok(
        "complete",
        commit(store, key, &graph, "repo", &documents).await,
    );
    // A graph no store can keep: an embedding that is not a list of
    // numbers, on a node after valid ones.
    let mut unstorable = sample();
    unstorable.add_entity("late", "Late", "class", None, None);
    for (id, node) in unstorable.nodes_mut() {
        if id == "late" {
            node.insert("embedding".to_owned(), json!(["not", "numbers"]));
        }
    }
    let changed = BTreeMap::from([("src/a.py".to_owned(), document("v2"))]);
    let refused = commit(store, key, &unstorable, "repo", &changed).await;
    assert!(refused.is_err(), "an unstorable graph is refused");
    let Some((loaded, loaded_revision)) = ok("load", store.load(key).await) else {
        panic!("the previous graph is still there");
    };
    assert_eq!(loaded_revision, revision, "the revision did not move");
    assert_same_graph(&loaded, &graph, "after a refused commit");
    assert_eq!(
        ok("versions", store.document_versions(key, "repo").await),
        BTreeMap::from([("src/a.py".to_owned(), "v1".to_owned())]),
        "the previous run's versions"
    );
    ok("delete", store.delete(key).await);
}

async fn one_lease_holder_at_a_time<S: GraphStore>(store: &S, key: GraphKey) {
    let first = ok("lease", store.lease(key).await);
    assert!(first.is_some(), "a free graph's lease is granted");
    assert!(
        ok("lease", store.lease(key).await).is_none(),
        "a held lease is not granted twice"
    );
    let other = ok(
        "lease",
        store
            .lease(GraphKey {
                application_id: key.application_id + 100,
                ..key
            })
            .await,
    );
    assert!(other.is_some(), "another graph's lease is independent");
    drop(first);
    assert!(
        ok("lease", store.lease(key).await).is_some(),
        "a dropped lease frees the graph"
    );
}

async fn ranking<S: GraphStore>(store: &S, key: GraphKey) {
    let mut graph = Graph::new();
    for id in ["e", "d", "c", "b", "a", "z"] {
        graph.add_entity(id, id, "class", None, None);
    }
    // a and b tie (same vector), c is orthogonal, d is opposite, z has none.
    for (id, vector) in [
        ("a", [1.0, 0.0, 0.0, 0.0]),
        ("b", [2.0, 0.0, 0.0, 0.0]),
        ("c", [0.0, 1.0, 0.0, 0.0]),
        ("d", [-1.0, 0.0, 0.0, 0.0]),
        ("e", [1.0, 1.0, 0.0, 0.0]),
    ] {
        graph.set_embedding(id, &vector);
    }
    ok(
        "complete",
        commit(store, key, &graph, "repo", &BTreeMap::new()).await,
    );
    let ranked = ok("rank", store.rank(key, &[1.0, 0.0, 0.0, 0.0], -1.0).await)
        .unwrap_or_else(|error| panic!("same width ranks: {error}"));
    let order: Vec<&str> = ranked.iter().map(|(id, _)| id.as_str()).collect();
    assert_eq!(order, ["a", "b", "e", "c", "d"], "best first, ties by id");
    let close = |got: f64, want: f64| (got - want).abs() < 1e-6;
    assert!(
        close(ranked[0].1, 1.0) && close(ranked[1].1, 1.0),
        "{ranked:?}"
    );
    assert!(
        close(ranked[2].1, std::f64::consts::FRAC_1_SQRT_2),
        "{ranked:?}"
    );
    assert!(
        close(ranked[3].1, 0.0) && close(ranked[4].1, -1.0),
        "{ranked:?}"
    );
    let above = ok("rank", store.rank(key, &[1.0, 0.0, 0.0, 0.0], 0.5).await)
        .unwrap_or_else(|error| panic!("same width ranks: {error}"));
    let order: Vec<&str> = above.iter().map(|(id, _)| id.as_str()).collect();
    assert_eq!(order, ["a", "b", "e"], "min_score filters");
    assert_eq!(
        ok("rank", store.rank(key, &[0.0; 4], 0.0).await),
        Ok(Vec::new()),
        "a zero query ranks nothing"
    );
    assert_eq!(
        ok(
            "rank",
            store.rank(key, &[1.0, 0.0, 0.0, 0.0, 0.0], 0.0).await
        ),
        Err("shapes (5,) and (4,) not aligned: 5 (dim 0) != 4 (dim 0)".to_owned()),
        "another width is numpy's message"
    );
    ok("delete", store.delete(key).await);
}

async fn graphs_are_apart<S: GraphStore>(store: &S) {
    let (one, two) = (key(7), key(8));
    let graph = sample();
    let documents = BTreeMap::from([("src/a.py".to_owned(), document("v1"))]);
    ok(
        "complete",
        commit(store, one, &graph, "repo", &documents).await,
    );
    assert!(
        ok("load", store.load(two).await).is_none(),
        "keys do not leak"
    );
    assert!(ok("versions", store.document_versions(two, "repo").await).is_empty());
    let mut other = Graph::new();
    other.add_entity("only", "Only", "class", None, None);
    ok(
        "complete",
        commit(store, two, &other, "repo", &BTreeMap::new()).await,
    );
    assert!(ok("delete", store.delete(two).await));
    let Some((kept, _)) = ok("load", store.load(one).await) else {
        panic!("deleting one graph keeps the other");
    };
    assert_same_graph(&kept, &graph, "the other graph");
    assert_eq!(
        ok("versions", store.document_versions(one, "repo").await).len(),
        1
    );
    ok("delete", store.delete(one).await);
}

/// `save`, `remove_source` and `import`: the writes that are not a run's
/// completion. Each is one transaction that moves the revision, and each
/// leaves exactly the state it is documented to leave.
async fn the_administrative_writers<S: GraphStore>(store: &S, key: GraphKey) {
    let graph = sample();
    let documents = BTreeMap::from([("src/a.py".to_owned(), document("v1"))]);
    let first = ok(
        "complete",
        commit(store, key, &graph, "repo", &documents).await,
    );

    // save: the graph is replaced, the sources' state is not touched.
    let mut changed = graph.clone();
    changed.add_entity("saved", "Saved", "class", None, None);
    let saved = ok("save", store.save(key, &changed).await);
    assert!(
        saved > first,
        "a save bumps the revision ({first} -> {saved})"
    );
    assert_eq!(ok("revision", store.revision(key).await), Some(saved));
    let Some((loaded, _)) = ok("load", store.load(key).await) else {
        panic!("the saved graph loads");
    };
    assert_same_graph(&loaded, &changed, "saved graph");
    assert_eq!(
        ok("versions", store.document_versions(key, "repo").await),
        BTreeMap::from([("src/a.py".to_owned(), "v1".to_owned())]),
        "a save keeps the document versions"
    );
    let status = ok("status", store.status_document(key).await);
    assert_eq!(status["sources"]["repo"]["status"], json!("completed"));

    // import: refused while native state exists, and it writes nothing.
    let mut imported = Graph::new();
    imported.add_entity("imp", "Imp", "class", None, None);
    assert!(
        matches!(
            ok("import", store.import(key, &imported, false).await),
            Imported::HasIngestionState {
                sources: 1,
                documents: 1
            }
        ),
        "an import over ingestion state is refused"
    );
    assert_eq!(
        ok("revision", store.revision(key).await),
        Some(saved),
        "a refused import writes nothing"
    );
    // ... and with replace_state it takes the state with the old graph.
    let Imported::Saved { revision } = ok("import", store.import(key, &imported, true).await)
    else {
        panic!("an import that replaces the state is stored");
    };
    assert!(revision > saved, "an import bumps the revision");
    let Some((loaded, _)) = ok("load", store.load(key).await) else {
        panic!("the imported graph loads");
    };
    assert_same_graph(&loaded, &imported, "imported graph");
    assert!(ok("versions", store.document_versions(key, "repo").await).is_empty());
    assert_eq!(
        ok("status", store.status_document(key).await)["sources"],
        json!({}),
        "replacing the state removed the sources"
    );
    // An import over a graph with no state needs no flag.
    assert!(matches!(
        ok("import", store.import(key, &graph, false).await),
        Imported::Saved { .. }
    ));

    // remove_source: the graph without the source, and its state gone,
    // while another source's state stays.
    ok(
        "complete",
        commit(store, key, &graph, "repo", &documents).await,
    );
    let mirror = BTreeMap::from([("m.py".to_owned(), document("m1"))]);
    ok(
        "complete",
        commit(store, key, &graph, "mirror", &mirror).await,
    );
    let removed_at = ok(
        "remove_source",
        store.remove_source(key, &graph, "repo", "repo").await,
    );
    assert_eq!(ok("revision", store.revision(key).await), Some(removed_at));
    assert!(ok("versions", store.document_versions(key, "repo").await).is_empty());
    assert_eq!(
        ok("versions", store.document_versions(key, "mirror").await).len(),
        1,
        "another source's versions stay"
    );
    let status = ok("status", store.status_document(key).await);
    assert!(status["sources"]["repo"].is_null(), "{status}");
    assert!(status["sources"]["mirror"].is_object(), "{status}");
    ok("delete", store.delete(key).await);
}

/// `load_view` is `load` without the vectors, plus the ids that have one:
/// a non-empty vector counts, an empty one and none do not. The revision is
/// the stored one, and the vectors stay stored (`load` and `rank` see them).
async fn the_view_holds_no_vectors<S: GraphStore>(store: &S, key: GraphKey) {
    assert!(
        ok("load_view", store.load_view(key).await).is_none(),
        "no graph, no view"
    );
    let mut graph = sample();
    graph.add_entity("blank", "Blank", "class", None, None);
    assert!(graph.set_embedding("blank", &[]));
    let revision = ok(
        "complete",
        commit(store, key, &graph, "repo", &BTreeMap::new()).await,
    );
    let Some(read) = ok("load_view", store.load_view(key).await) else {
        panic!("the committed graph has a view");
    };
    assert_eq!(read.revision, revision, "the view reports the revision");
    assert_eq!(
        read.embedded,
        vec!["alpha".to_owned()],
        "only the non-empty vector counts"
    );
    assert_eq!(ids(&read.graph), ids(&graph), "node order");
    assert_eq!(edges(&read.graph), edges(&graph), "edge order");
    for (id, node) in graph.nodes() {
        let mut expected = node.clone();
        expected.remove("embedding");
        assert_eq!(read.graph.node(id), Some(&expected), "view node {id}");
    }
    assert_eq!(read.graph.metadata, graph.metadata, "the stamp stays");
    // The vectors are still stored.
    let Some((full, _)) = ok("load", store.load(key).await) else {
        panic!("the graph loads");
    };
    assert_eq!(
        full.node("alpha").and_then(|node| node.get("embedding")),
        Some(&json!([0.25, -0.5, 1.0, 0.125]))
    );
    assert!(
        !ok(
            "rank",
            store.rank(key, &[0.25, -0.5, 1.0, 0.125], 0.9).await
        )
        .unwrap_or_default()
        .is_empty(),
        "rank still sees the vector"
    );
    ok("delete", store.delete(key).await);
}
