//! `search_workspace` over a real folder: the chunks are committed with the
//! build (replaced when a file changes, dropped when it goes), BM25 finds
//! split identifiers, the graph and its neighbours add their places, and the
//! filters and the tool's answer hold.

use elitea_local_index::search::{Mode, Request};
use elitea_local_index::service::{IndexService, NoEvents};
use elitea_local_index::tools::IndexToolset;
use serde_json::json;
use std::fs;
use std::path::Path;
use std::sync::Arc;

fn write(root: &Path, path: &str, text: &str) {
    let path = root.join(path);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, text).unwrap();
}

fn fixture(root: &Path) {
    write(
        root,
        "src/registry.rs",
        "//! Keeps one index per workspace.\n\npub const REFRESH_DELAY_MS: u64 = 3000;\n\npub struct IndexRegistry {\n    entries: Vec<String>,\n}\n\nimpl IndexRegistry {\n    /// The service a turn is offered.\n    pub fn for_turn(&self, workspace: &str) -> Option<String> {\n        self.entries.iter().find(|e| *e == workspace).cloned()\n    }\n}\n",
    );
    write(
        root,
        "src/tokens.py",
        "class TokenStore:\n    def rotate_refresh_token(self, old):\n        return old[::-1]\n\n\ndef sign_in(store):\n    return store.rotate_refresh_token('x')\n",
    );
    write(
        root,
        "docs/guide.md",
        "# Guide\n\nThe workspace index is refreshed a few seconds after the agent changes files.\n\n## Tokens\n\nRefresh tokens are rotated on every sign in, and the old one stops working.\n",
    );
}

struct Opened {
    folder: tempfile::TempDir,
    _data: tempfile::TempDir,
    service: Arc<IndexService>,
}

async fn built() -> Opened {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    let data = tempfile::tempdir().unwrap();
    let service = IndexService::open(
        "ws",
        folder.path(),
        &[],
        &data.path().join("index"),
        Arc::new(NoEvents),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    Opened {
        folder,
        _data: data,
        service,
    }
}

fn request(query: &str) -> Request {
    Request {
        query: query.to_owned(),
        top_k: 10,
        path_glob: None,
        kind: None,
        mode: Mode::Auto,
        execution_id: None,
    }
}

fn paths(service: &IndexService, request: &Request) -> Vec<String> {
    let (answer, _) = service.search(request, None).unwrap();
    answer.hits.into_iter().map(|hit| hit.path).collect()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn identifiers_are_found_by_their_parts_and_lines_are_cited() {
    let opened = built().await;
    let service = &opened.service;
    let (answer, notice) = service
        .search(&request("index registry for turn"), None)
        .unwrap();
    assert!(notice.is_none());
    let first = &answer.hits[0];
    assert_eq!(first.path, "src/registry.rs", "{answer:#?}");
    assert!(first.matched_by.contains(&"lexical"), "{first:?}");
    assert!(first.line_start >= 1 && first.line_end >= first.line_start);
    // A constant outside any function is a chunk too (the gap chunks).
    let (constant, _) = service.search(&request("REFRESH_DELAY_MS"), None).unwrap();
    let hit = &constant.hits[0];
    assert_eq!(hit.path, "src/registry.rs");
    assert!(hit.snippet.contains("REFRESH_DELAY_MS"), "{hit:?}");
    assert_eq!(hit.line_start, 1, "{hit:?}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_graph_adds_its_entities_and_neighbours() {
    let opened = built().await;
    let (answer, _) = opened.service.search(&request("TokenStore"), None).unwrap();
    let hit = answer
        .hits
        .iter()
        .find(|hit| hit.path == "src/tokens.py" && hit.matched_by.contains(&"graph"))
        .unwrap_or_else(|| panic!("{answer:#?}"));
    assert!(
        hit.entity_ref
            .as_deref()
            .is_some_and(|r| r.starts_with("TokenStore")),
        "{hit:?}"
    );
    assert!(
        answer
            .hits
            .iter()
            .any(|hit| hit.matched_by.contains(&"neighbour")),
        "a method of the class is one hop away: {answer:#?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn kind_and_path_filters_hold() {
    let opened = built().await;
    let service = &opened.service;
    let mut docs = request("refresh tokens rotated");
    docs.kind = Some(elitea_local_index::chunks::ChunkKind::Docs);
    assert_eq!(paths(service, &docs), ["docs/guide.md"]);
    let mut code = request("refresh tokens rotated");
    code.kind = Some(elitea_local_index::chunks::ChunkKind::Code);
    assert!(paths(service, &code).iter().all(|p| p.starts_with("src/")));
    let mut glob = request("refresh");
    glob.path_glob = Some("*.py".to_owned());
    let found = paths(service, &glob);
    assert!(
        !found.is_empty() && found.iter().all(|p| p == "src/tokens.py"),
        "{found:?}"
    );
    let mut semantic = request("refresh");
    semantic.mode = Mode::Semantic;
    let (answer, _) = service.search(&semantic, None).unwrap();
    assert!(answer.note.unwrap().contains("embeddings"));
    assert!(!answer.hits.is_empty(), "falls back to lexical");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn chunks_follow_the_files_through_refreshes() {
    let opened = built().await;
    let service = &opened.service;
    let store = service.store();
    let before = store.chunk_count().unwrap();
    assert!(before >= 4, "{before}");
    assert_eq!(
        store.chunk_generation().unwrap().as_deref(),
        Some(elitea_local_index::chunks::GENERATION)
    );
    // Changed: its chunks are replaced (the old text no longer found).
    write(
        opened.folder.path(),
        "docs/guide.md",
        "# Guide\n\nThe index now watches the folder for changes.\n",
    );
    service.refresh(false).await.unwrap();
    assert!(
        paths(service, &request("rotated sign"))
            .iter()
            .all(|p| p != "docs/guide.md")
    );
    assert_eq!(
        paths(service, &request("watches folder")),
        ["docs/guide.md"]
    );
    // Deleted: its chunks go with it.
    fs::remove_file(opened.folder.path().join("src/tokens.py")).unwrap();
    service.refresh(false).await.unwrap();
    assert_eq!(store.chunks_of("src/tokens.py").unwrap(), []);
    assert_eq!(paths(service, &request("TokenStore")), [] as [String; 0]);
    // A full rebuild gives the same chunks.
    let count = store.chunk_count().unwrap();
    service.refresh(true).await.unwrap();
    assert_eq!(store.chunk_count().unwrap(), count);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_denied_file_leaves_no_chunk() {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    let data = tempfile::tempdir().unwrap();
    let service = IndexService::open(
        "ws",
        folder.path(),
        &["docs/**".to_owned()],
        &data.path().join("index"),
        Arc::new(NoEvents),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    assert_eq!(service.store().chunks_of("docs/guide.md").unwrap(), []);
    assert!(
        paths(&service, &request("rotated sign"))
            .iter()
            .all(|p| p != "docs/guide.md")
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_tool_answers_locations_and_checks_its_arguments() {
    let opened = built().await;
    let tools = IndexToolset::new(opened.service.clone()).current_tools();
    let tool = tools
        .iter()
        .find(|tool| adk_core::Tool::name(tool.as_ref()) == "search_workspace")
        .unwrap();
    let answer = tool
        .call(json!({"query": "IndexRegistry", "top_k": 3}))
        .await;
    assert_eq!(answer["status"], "ok", "{answer}");
    let text = answer["result"].as_str().unwrap();
    let hits: serde_json::Value = serde_json::from_str(text).unwrap();
    let first = &hits[0];
    assert_eq!(first["path"], "src/registry.rs");
    for key in ["line_start", "line_end", "snippet", "score", "matched_by"] {
        assert!(first.get(key).is_some(), "{key}: {first}");
    }
    assert!(hits.as_array().unwrap().len() <= 3);
    let refused = tool.call(json!({"query": "x", "kind": "binary"})).await;
    assert_eq!(refused["code"], "index.invalid_arguments");
    opened.service.mark_changed(1);
    let stale = tool.call(json!({"query": "IndexRegistry"})).await;
    assert!(
        stale["result"]
            .as_str()
            .unwrap()
            .starts_with("Note: the workspace index may be out of date")
    );
    opened.service.close();
    let closed = tool.call(json!({"query": "IndexRegistry"})).await;
    assert_eq!(closed["code"], "index.closed");
}

/// An index built before chunks existed (schema 2) is upgraded when it is
/// opened, and its next refresh cuts every file's chunks without parsing
/// any file again.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_index_from_before_chunks_gains_them_without_a_reparse() {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    let data = tempfile::tempdir().unwrap();
    let dir = data.path().join("index");
    let open = || IndexService::open("ws", folder.path(), &[], &dir, Arc::new(NoEvents)).unwrap();
    let service = open();
    service.refresh(false).await.unwrap();
    service.close();
    let raw = rusqlite::Connection::open(dir.join("index.sqlite")).unwrap();
    raw.execute_batch(
        "DROP TABLE chunk_vectors; DROP TABLE vector_sets;
         DROP TABLE chunks; DROP TABLE chunks_fts;
         DELETE FROM meta WHERE key = 'chunks_generation';
         UPDATE meta SET value = '2' WHERE key = 'schema_version';
         PRAGMA user_version = 2;",
    )
    .unwrap();
    drop(raw);
    let service = open();
    assert_eq!(service.store().chunk_count().unwrap(), 0);
    service.refresh(false).await.unwrap();
    let report = service.last_report().unwrap();
    assert_eq!(report.read, 0, "nothing parsed again: {report:?}");
    assert_eq!(report.hashed, 3, "every file read once: {report:?}");
    assert!(service.store().chunk_count().unwrap() >= 4);
    assert_eq!(
        paths(&service, &request("IndexRegistry"))[0],
        "src/registry.rs"
    );
    // And the refresh after that reads nothing.
    service.refresh(false).await.unwrap();
    assert_eq!(service.last_report().unwrap().hashed, 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn staged_chunks_stay_invisible_until_a_build_commits() {
    use elitea_local_index::chunks::{ChunkKind, ChunkRow};
    use elitea_local_index::sqlite_store::Staged;
    let opened = built().await;
    let store = opened.service.store();
    let before = store.chunk_count().unwrap();
    let staged = std::collections::HashMap::from([(
        "src/ghost.py".to_owned(),
        Staged {
            version: "v-ghost".into(),
            rows: vec![ChunkRow {
                line_start: 1,
                line_end: 1,
                kind: ChunkKind::Code,
                text: "def zebraphantom(): pass".into(),
                idents: "zebraphantom".into(),
                text_hash: "h-ghost".into(),
            }],
        },
    )]);
    // A build in flight stages chunks: searches never see them.
    store.stage_chunks(&staged).unwrap();
    assert_eq!(store.chunk_count().unwrap(), before);
    assert_eq!(
        store.search_fts("zebraphantom", 10).unwrap(),
        [] as [(i64, f64); 0]
    );
    assert_eq!(
        paths(&opened.service, &request("zebraphantom")),
        [] as [String; 0]
    );
    // The next build starts by clearing them, and its commit only moves
    // chunks of files the run itself read.
    opened.service.refresh(false).await.unwrap();
    assert_eq!(store.chunk_count().unwrap(), before);
    assert_eq!(store.chunks_of("src/ghost.py").unwrap(), []);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_graph_neighbour_whose_chunk_no_word_matches_is_still_a_hit() {
    let folder = tempfile::tempdir().unwrap();
    write(
        folder.path(),
        "pkg/caller.py",
        "from pkg.target import quietly_answers\n\n\ndef alpha_helper():\n    return quietly_answers()\n",
    );
    write(
        folder.path(),
        "pkg/target.py",
        "def quietly_answers():\n    return 42\n",
    );
    let data = tempfile::tempdir().unwrap();
    let service = IndexService::open(
        "ws",
        folder.path(),
        &[],
        &data.path().join("index"),
        Arc::new(NoEvents),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    // No word of the query is in target.py: the full-text side never sees
    // its chunk; only the graph (one hop from alpha_helper) does.
    let fts = service
        .store()
        .search_fts(
            &elitea_local_index::search::fts_expression("alpha helper").unwrap(),
            50,
        )
        .unwrap();
    let target: Vec<i64> = service
        .store()
        .chunks_of("pkg/target.py")
        .unwrap()
        .iter()
        .map(|c| c.id)
        .collect();
    assert!(fts.iter().all(|(id, _)| !target.contains(id)), "{fts:?}");
    let (answer, _) = service.search(&request("alpha helper"), None).unwrap();
    let hit = answer
        .hits
        .iter()
        .find(|hit| hit.path == "pkg/target.py")
        .unwrap_or_else(|| panic!("the neighbour is a hit: {answer:#?}"));
    assert_eq!(hit.matched_by, ["neighbour"], "{hit:?}");
    // And a filter admits it (its chunk is loaded, not only listed).
    let mut filtered = request("alpha helper");
    filtered.path_glob = Some("pkg/target.py".to_owned());
    assert_eq!(paths(&service, &filtered), ["pkg/target.py"]);
}
