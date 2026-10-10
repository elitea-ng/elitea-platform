//! The SQLite graph store: the shared conformance suite, diff writes that
//! keep the graph's order, owner-only files and the schema guard.

use elitea_content_source::Acl;
use elitea_inventory_core::graph::{Citation, Graph};
use elitea_inventory_core::store::{
    Completion, DocumentState, GraphKey, GraphStore, RunCounts, SourceStatus,
};
use elitea_local_index::sqlite_store::{FILE_NAME, SqliteGraphStore, Stats, StoreError};
use serde_json::json;
use std::collections::BTreeMap;
use std::path::Path;

#[tokio::test]
async fn the_sqlite_store_passes_the_shared_conformance_suite() {
    let dir = tempfile::tempdir().unwrap();
    let store = SqliteGraphStore::open(&dir.path().join("index")).unwrap();
    elitea_inventory_core::store_conformance::run(&store).await;
}

fn citation(path: &str, line: u64) -> Citation {
    Citation::from_value(&json!({
        "file_path": path, "line_start": line, "line_end": line + 3,
        "source_toolkit": "workspace", "doc_id": path, "content_hash": "h"
    }))
}

/// `files` files of `per_file` entities each, every entity calling the
/// next one, so edges cross files.
fn graph_of(files: usize, per_file: usize) -> Graph {
    let mut graph = Graph::new();
    for file in 0..files {
        let path = format!("src/f{file}.py");
        for n in 0..per_file {
            let id = format!("{file}-{n}");
            graph.add_entity(
                &id,
                &format!("E{file}_{n}"),
                "function",
                Some(&citation(&path, n as u64 * 10)),
                None,
            );
        }
    }
    let ids: Vec<String> = graph.nodes().map(|(id, _)| id.to_owned()).collect();
    for pair in ids.windows(2) {
        graph.add_relation(&pair[0], &pair[1], "calls", None);
    }
    graph
}

/// What a refresh does to one changed file: its entities removed, then
/// read again (appended).
fn refresh_one_file(graph: &mut Graph, file: usize, per_file: usize, tag: &str) {
    let path = format!("src/f{file}.py");
    graph.remove_file("workspace", &path);
    for n in 0..per_file {
        let id = format!("{file}-{n}");
        graph.add_entity(
            &id,
            &format!("E{file}_{n}{tag}"),
            "function",
            Some(&citation(&path, n as u64 * 10)),
            None,
        );
    }
    let next = format!("{}-0", file + 1);
    graph.add_relation(&format!("{file}-{}", per_file - 1), &next, "calls", None);
}

fn order(graph: &Graph) -> (Vec<String>, Vec<(String, String)>) {
    (
        graph.nodes().map(|(id, _)| id.to_owned()).collect(),
        graph
            .edges()
            .map(|(s, t, _)| (s.to_owned(), t.to_owned()))
            .collect(),
    )
}

async fn commit(store: &SqliteGraphStore, graph: &Graph, documents: usize) -> i64 {
    let key = GraphKey::LOCAL;
    store
        .start(
            key,
            &SourceStatus {
                toolkit_id: "workspace".into(),
                toolkit_name: "workspace".into(),
                toolkit_type: "local_folder".into(),
                branch: None,
            },
        )
        .await
        .unwrap();
    let documents: BTreeMap<String, DocumentState> = (0..documents)
        .map(|file| {
            (
                format!("src/f{file}.py"),
                DocumentState {
                    version: format!("v{file}"),
                    mime: "text/plain".into(),
                    acl: Acl::Project,
                },
            )
        })
        .collect();
    let stats: Stats = documents
        .keys()
        .map(|key| (key.clone(), (10, 20)))
        .collect();
    store
        .complete_with_stats(
            key,
            graph,
            &Completion {
                toolkit_id: "workspace",
                source_name: "workspace",
                documents: &documents,
                counts: RunCounts::default(),
                commit_sha: None,
            },
            &stats,
        )
        .unwrap()
}

#[tokio::test]
async fn a_one_file_refresh_writes_only_what_changed_and_keeps_the_order() {
    let dir = tempfile::tempdir().unwrap();
    let store = SqliteGraphStore::open(&dir.path().join("index")).unwrap();
    let (files, per_file) = (100, 10);
    let mut graph = graph_of(files, per_file);
    commit(&store, &graph, files).await;
    let first = store.last_commit_rows();
    assert!(first > 2000, "the first build writes the graph ({first})");

    // Unchanged: only the graph row, the source's status and the run.
    commit(&store, &graph, files).await;
    assert!(
        store.last_commit_rows() <= 3,
        "an unchanged graph writes almost nothing ({})",
        store.last_commit_rows()
    );

    // One file of 100 changed: its 10 entities and their edges move to the
    // end, nothing else is written.
    refresh_one_file(&mut graph, 40, per_file, "x");
    commit(&store, &graph, files).await;
    let rows = store.last_commit_rows();
    assert!(
        rows <= 3 * per_file as u64 + 10,
        "a one-file refresh writes tens of rows, not the graph ({rows})"
    );
    let (loaded, _) = store.load(GraphKey::LOCAL).await.unwrap().unwrap();
    assert_eq!(order(&loaded), order(&graph), "node and edge order kept");
    assert_eq!(loaded, graph, "the same graph");

    // Again on another file, and on the first again: still the graph's order.
    refresh_one_file(&mut graph, 3, per_file, "y");
    refresh_one_file(&mut graph, 40, per_file, "z");
    commit(&store, &graph, files).await;
    let (loaded, _) = store.load(GraphKey::LOCAL).await.unwrap().unwrap();
    assert_eq!(order(&loaded), order(&graph));
    assert_eq!(loaded, graph);
}

#[tokio::test]
async fn the_stat_cache_is_committed_with_the_versions() {
    let dir = tempfile::tempdir().unwrap();
    let store = SqliteGraphStore::open(&dir.path().join("index")).unwrap();
    commit(&store, &graph_of(2, 1), 2).await;
    let stats = store.document_stats(GraphKey::LOCAL, "workspace").unwrap();
    assert_eq!(stats.len(), 2);
    assert_eq!(stats["src/f1.py"].version, "v1");
    assert_eq!(
        (stats["src/f1.py"].size, stats["src/f1.py"].mtime_ns),
        (10, 20)
    );
    // The trait's `complete` records no stat: nothing then matches a file.
    let documents = BTreeMap::from([(
        "a.py".to_owned(),
        DocumentState {
            version: "v".into(),
            mime: "text/plain".into(),
            acl: Acl::Project,
        },
    )]);
    store
        .complete(
            GraphKey::LOCAL,
            &Graph::new(),
            &Completion {
                toolkit_id: "workspace",
                source_name: "workspace",
                documents: &documents,
                counts: RunCounts::default(),
                commit_sha: None,
            },
        )
        .await
        .unwrap();
    assert!(
        store
            .document_stats(GraphKey::LOCAL, "workspace")
            .unwrap()
            .is_empty()
    );
}

#[cfg(unix)]
#[tokio::test]
async fn the_directory_database_and_sidecars_are_owner_only() {
    use std::os::unix::fs::PermissionsExt as _;
    let dir = tempfile::tempdir().unwrap();
    let index = dir.path().join("workspaces/abc/index");
    let store = SqliteGraphStore::open(&index).unwrap();
    commit(&store, &graph_of(3, 2), 3).await;
    let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode(&index), 0o700);
    assert_eq!(mode(&dir.path().join("workspaces/abc")), 0o700);
    assert_eq!(mode(store.path()), 0o600);
    for sidecar in ["-wal", "-shm"] {
        let path = index.join(format!("{FILE_NAME}{sidecar}"));
        assert!(path.exists(), "WAL mode keeps {sidecar}");
        assert_eq!(mode(&path), 0o600, "{sidecar}");
    }
    let journal: String = rusqlite::Connection::open(store.path())
        .unwrap()
        .query_row("PRAGMA journal_mode", [], |row| row.get(0))
        .unwrap();
    assert_eq!(journal, "wal");
    // A wider database is narrowed when the index is opened again.
    store.close();
    std::fs::set_permissions(store.path(), std::fs::Permissions::from_mode(0o644)).unwrap();
    let again = SqliteGraphStore::open(&index).unwrap();
    assert_eq!(mode(again.path()), 0o600);
    assert!(matches!(again.revision(GraphKey::LOCAL).await, Ok(Some(_))));
    again.close();
    assert!(matches!(
        again.revision(GraphKey::LOCAL).await,
        Err(StoreError::Closed)
    ));
}

#[test]
fn an_index_from_a_newer_app_is_refused() {
    let dir = tempfile::tempdir().unwrap();
    let index = dir.path().join("index");
    SqliteGraphStore::open(&index).unwrap().close();
    rusqlite::Connection::open(index.join(FILE_NAME))
        .unwrap()
        .execute_batch("PRAGMA user_version = 2;")
        .unwrap();
    match SqliteGraphStore::open(&index) {
        Err(StoreError::NewerSchema(2)) => {}
        other => panic!("a newer schema is refused, got {other:?}"),
    }
}

#[test]
fn an_index_inside_the_workspace_is_refused() {
    let dir = tempfile::tempdir().unwrap();
    let workspace = dir.path().join("project");
    std::fs::create_dir(&workspace).unwrap();
    assert!(matches!(
        SqliteGraphStore::open_for_workspace(&workspace.join("index"), &workspace),
        Err(StoreError::Refused(_))
    ));
    assert!(!workspace.join("index").exists(), "nothing was created");
    assert!(SqliteGraphStore::open_for_workspace(&dir.path().join("index"), &workspace).is_ok());
}

#[cfg(unix)]
#[test]
fn a_symlinked_database_is_refused() {
    let dir = tempfile::tempdir().unwrap();
    let index = dir.path().join("index");
    std::fs::create_dir(&index).unwrap();
    let elsewhere = dir.path().join("elsewhere.sqlite");
    std::fs::write(&elsewhere, b"").unwrap();
    std::os::unix::fs::symlink(&elsewhere, index.join(FILE_NAME)).unwrap();
    assert!(matches!(
        SqliteGraphStore::open(&index),
        Err(StoreError::Refused(_))
    ));
}
