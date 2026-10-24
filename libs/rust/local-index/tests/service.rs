//! The index service and its tools over a real folder: a build, an
//! incremental refresh that re-reads only what changed, cancellation, the
//! stale notice, and the tools through the adk `Tool` interface.

use elitea_inventory_core::store::{GraphKey, GraphStore as _};
use elitea_local_index::service::{IndexEvent, IndexEvents, IndexService, IndexState, NoEvents};
use elitea_local_index::tools::{IndexToolset, LOCAL_TOOLS, NAMES, TOOLS, TOOLSET_NAME};
use serde_json::{Value, json};
use std::fs;
use std::path::Path;
use std::sync::{Arc, Mutex};

fn write(root: &Path, path: &str, text: &str) {
    let path = root.join(path);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, text).unwrap();
}

fn fixture(root: &Path) {
    write(
        root,
        "pkg/users.py",
        "from pkg.store import Store\n\n\nclass Users:\n    def create(self, name):\n        return Store().save(name)\n",
    );
    write(
        root,
        "pkg/store.py",
        "class Store:\n    def save(self, value):\n        return value\n",
    );
    write(root, "README.md", "# Demo\n\nUsers are kept in a Store.\n");
    write(root, "notes.txt", "nothing to parse\n");
}

#[derive(Default)]
struct Recorded(Mutex<Vec<IndexEvent>>);

impl IndexEvents for Recorded {
    fn emit(&self, event: IndexEvent) {
        self.0.lock().unwrap().push(event);
    }
}

struct Opened {
    folder: tempfile::TempDir,
    _data: tempfile::TempDir,
    service: Arc<IndexService>,
    events: Arc<Recorded>,
}

fn open() -> Opened {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    let data = tempfile::tempdir().unwrap();
    let events = Arc::new(Recorded::default());
    let service = IndexService::open(
        "ws1",
        folder.path(),
        &[],
        &data.path().join("index"),
        events.clone(),
    )
    .unwrap();
    Opened {
        folder,
        _data: data,
        service,
        events,
    }
}

/// A file ingestion decoded from `Shift_JIS` reads back through the tool.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn entity_content_reads_a_legacy_encoded_file_ingestion_indexed() {
    let folder = tempfile::tempdir().unwrap();
    let source = "# 返金ポリシー: 返金は三十日以内に受け付けます。\n# 担当は請求チームです。\nclass Refunds:\n    def run(self):\n        return 1\n";
    let (bytes, _, _) = encoding_rs::SHIFT_JIS.encode(source);
    assert!(std::str::from_utf8(&bytes).is_err());
    std::fs::write(folder.path().join("refunds.py"), &bytes).unwrap();
    let data = tempfile::tempdir().unwrap();
    let service = IndexService::open(
        "ws1",
        folder.path(),
        &[],
        &data.path().join("index"),
        Arc::new(Recorded::default()),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    let content = call(
        &service,
        "get_entity_content",
        json!({"entity_name": "Refunds"}),
    )
    .await;
    let content = text(&content);
    assert!(content.contains("class Refunds:"), "{content}");
    assert!(content.contains("def run(self):"), "{content}");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_build_then_an_incremental_refresh_reads_only_what_changed() {
    let opened = open();
    let service = &opened.service;
    assert_eq!(service.status().state, IndexState::Stale, "never built");
    assert!(service.view().is_none(), "no build, nothing to answer from");

    let status = service.refresh(false).await.unwrap();
    assert_eq!(status.state, IndexState::Ready);
    assert_eq!(status.files, 4);
    assert!(status.entities > 4, "{status:?}");
    assert!(status.last_run.is_some());
    let report = service.last_report().unwrap();
    assert_eq!((report.read, report.unchanged, report.hashed), (4, 0, 4));
    let phases: Vec<&str> = opened
        .events
        .0
        .lock()
        .unwrap()
        .iter()
        .map(|e| e.phase)
        // Another test's refresh may hold the turn first.
        .filter(|phase| *phase != "queued")
        .collect();
    assert_eq!(phases.first(), Some(&"listing"));
    assert_eq!(phases.last(), Some(&"ready"));
    assert!(phases.contains(&"saving"), "{phases:?}");

    // Nothing changed: nothing read, nothing hashed, and no run recorded
    // (the build in memory answers as it is).
    let runs = service.store().runs(GraphKey::LOCAL).unwrap().len();
    service.refresh(false).await.unwrap();
    let report = service.last_report().unwrap();
    assert_eq!((report.read, report.unchanged, report.hashed), (0, 4, 0));
    assert_eq!(service.store().runs(GraphKey::LOCAL).unwrap().len(), runs);

    // One edited, one deleted, one added.
    let root = opened.folder.path();
    write(
        root,
        "pkg/store.py",
        "class Store:\n    def save(self, value):\n        return value\n\n    def load(self, key):\n        return key\n",
    );
    fs::remove_file(root.join("notes.txt")).unwrap();
    write(root, "pkg/audit.py", "def audit():\n    return True\n");
    let status = service.refresh(false).await.unwrap();
    let report = service.last_report().unwrap();
    // The edited and the added file, and users.py, whose edges into
    // store.py's nodes are restored by reading it again.
    assert_eq!(report.read, 3, "{report:?}");
    assert_eq!(report.dependents, 1, "{report:?}");
    assert_eq!(report.unchanged, 1);
    assert_eq!(report.removed, 1);
    assert_eq!(report.hashed, 2);
    assert_eq!(status.files, 4);
    let (view, notice) = service.view().unwrap();
    assert!(notice.is_none(), "a fresh build has no notice");
    let names: Vec<&str> = view
        .graph
        .nodes()
        .filter_map(|(_, node)| node.get("name")?.as_str())
        .collect();
    assert!(
        names.contains(&"load") && names.contains(&"audit"),
        "{names:?}"
    );
    assert!(!names.contains(&"notes.txt"), "{names:?}");

    // A full rebuild re-reads everything and answers the same graph.
    let before: Vec<String> = view.graph.nodes().map(|(id, _)| id.to_owned()).collect();
    service.refresh(true).await.unwrap();
    assert_eq!(service.last_report().unwrap().read, 4);
    let (rebuilt, _) = service.view().unwrap();
    let mut after: Vec<String> = rebuilt.graph.nodes().map(|(id, _)| id.to_owned()).collect();
    let mut before = before;
    before.sort();
    after.sort();
    assert_eq!(before, after);
}

#[tokio::test]
async fn a_cancelled_refresh_keeps_the_previous_build() {
    let opened = open();
    let service = &opened.service;
    let built = service.refresh(false).await.unwrap();
    let revision = service.store().revision(GraphKey::LOCAL).await.unwrap();
    write(opened.folder.path(), "pkg/new.py", "def new():\n    pass\n");

    // On this single-threaded runtime the refresh cannot start before the
    // cancel lands: it stops at its first checkpoint.
    service.start_refresh(false).unwrap();
    assert_eq!(service.status().state, IndexState::Building);
    assert_eq!(
        service.start_refresh(false).unwrap_err().code,
        "index_busy",
        "one refresh at a time"
    );
    assert!(service.cancel());
    for _ in 0..500 {
        if !service.is_building() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert!(!service.is_building());
    assert_eq!(
        service.store().revision(GraphKey::LOCAL).await.unwrap(),
        revision,
        "nothing committed"
    );
    let status = service.status();
    assert_eq!(
        status.state,
        IndexState::Ready,
        "back to the previous build"
    );
    assert_eq!(status.entities, built.entities);
    let last = opened.events.0.lock().unwrap().last().unwrap().clone();
    assert_eq!(last.phase, "cancelled");
    // Recorded as cancelled, not as a failure: the source still reads as
    // its last completed build.
    let report = service
        .store()
        .status_document(GraphKey::LOCAL)
        .await
        .unwrap();
    assert_eq!(report["sources"]["workspace"]["status"], "completed");
    assert!(report["sources"]["workspace"]["error_message"].is_null());
    assert!(!service.cancel(), "nothing runs now");
}

/// A cancelled refresh goes back to where the index was (here: stale, not
/// checked since it opened), never to `ready`; it is recorded in the runs
/// as cancelled and never as the source's error, so the last build's time
/// survives a restart.
#[tokio::test]
async fn a_cancelled_refresh_restores_the_state_and_keeps_the_last_run() {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    let data = tempfile::tempdir().unwrap();
    let dir = data.path().join("index");
    let first = IndexService::open("ws", folder.path(), &[], &dir, Arc::new(NoEvents)).unwrap();
    let built = first.refresh(false).await.unwrap();
    let last_run = built.last_run.clone().unwrap();
    first.close();

    let again = IndexService::open("ws", folder.path(), &[], &dir, Arc::new(NoEvents)).unwrap();
    assert_eq!(again.status().state, IndexState::Stale);
    assert_eq!(again.status().last_run.as_deref(), Some(last_run.as_str()));
    again.start_refresh(false).unwrap();
    assert!(again.cancel());
    for _ in 0..500 {
        if !again.is_building() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    let status = again.status();
    assert_eq!(
        status.state,
        IndexState::Stale,
        "not checked yet: still stale"
    );
    assert_eq!(status.last_run.as_deref(), Some(last_run.as_str()));
    let runs = again.store().runs(GraphKey::LOCAL).unwrap();
    let statuses: Vec<&str> = runs.iter().map(|run| run.status.as_str()).collect();
    assert!(
        statuses == ["completed", "cancelled"] || statuses == ["completed"],
        "{statuses:?}"
    );
    assert!(runs.iter().all(|run| run.status != "error"), "{runs:?}");
    again.close();

    let reopened = IndexService::open("ws", folder.path(), &[], &dir, Arc::new(NoEvents)).unwrap();
    assert_eq!(
        reopened.status().last_run.as_deref(),
        Some(last_run.as_str()),
        "the last build's time survives the cancel and a restart"
    );
    let report = reopened
        .store()
        .status_document(GraphKey::LOCAL)
        .await
        .unwrap();
    assert_eq!(report["sources"]["workspace"]["status"], "completed");
}

/// A build is served only under the policy it was made with: reopened with
/// a `path_deny` that now hides a file, the old build is not loaded, the
/// tools answer nothing from it, and a refresh rebuilds without that file.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_build_from_another_policy_is_never_served() {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    let data = tempfile::tempdir().unwrap();
    let dir = data.path().join("index");
    let first = IndexService::open("ws", folder.path(), &[], &dir, Arc::new(NoEvents)).unwrap();
    first.refresh(false).await.unwrap();
    assert_eq!(
        first.store().policy().unwrap().as_deref(),
        Some(first.policy())
    );
    first.close();

    let deny = vec!["pkg/store.py".to_owned()];
    let again = IndexService::open("ws", folder.path(), &deny, &dir, Arc::new(NoEvents)).unwrap();
    assert_eq!(again.status().state, IndexState::StalePolicy);
    assert_eq!(again.status().entities, 0, "the old build is not loaded");
    assert!(again.view().is_none(), "nothing is served from it");
    let refused = call(&again, "search_knowledge_graph", json!({"query": "Store"})).await;
    assert_eq!(refused["code"], "index.not_ready", "{refused}");

    let rebuilt = again.refresh(false).await.unwrap();
    assert_eq!(rebuilt.state, IndexState::Ready);
    assert_eq!(
        again.store().policy().unwrap().as_deref(),
        Some(again.policy())
    );
    let (view, _) = again.view().unwrap();
    let names: Vec<&str> = view
        .graph
        .nodes()
        .filter_map(|(_, node)| node.get("name")?.as_str())
        .collect();
    assert!(
        !names
            .iter()
            .any(|name| *name == "Store" || *name == "store.py"),
        "the denied file is gone: {names:?}"
    );
    assert!(names.contains(&"Users"), "{names:?}");
    // The same policy, in another order: the same fingerprint.
    assert_eq!(
        elitea_local_index::service::policy_fingerprint(&["b".to_owned(), "a".to_owned()]),
        elitea_local_index::service::policy_fingerprint(&[
            "a".to_owned(),
            "b".to_owned(),
            "a".to_owned()
        ])
    );
}

/// Closed while a refresh runs (the index removed or turned off): the
/// refresh's end changes nothing, and the tools a turn still holds answer
/// that the index is gone.
#[tokio::test]
async fn a_closed_service_stays_closed_and_answers_no_tool() {
    let opened = open();
    let service = &opened.service;
    service.refresh(false).await.unwrap();
    service.start_refresh(false).unwrap();
    service.close();
    for _ in 0..500 {
        if !service.is_building() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert!(!service.is_building());
    assert_eq!(service.status().state, IndexState::Off);
    assert!(service.view().is_none());
    let answer = call(service, "search_knowledge_graph", json!({"query": "Users"})).await;
    assert_eq!(answer["code"], "index.closed", "{answer}");
    assert!(
        answer["message"]
            .as_str()
            .unwrap()
            .contains("removed or turned off"),
        "{answer}"
    );
    assert_eq!(
        service.refresh(false).await.unwrap_err().code,
        "index_closed"
    );
    let phases: Vec<&str> = opened
        .events
        .0
        .lock()
        .unwrap()
        .iter()
        .map(|e| e.phase)
        .collect();
    assert_eq!(
        phases.last(),
        Some(&"ready"),
        "nothing reported after the close: {phases:?}"
    );
}

/// Cancelled after a failed refresh, the index goes back to `error` with
/// the failure's message, not to `error` without one.
#[tokio::test]
async fn a_cancel_after_a_failure_keeps_the_failure() {
    let opened = open();
    let service = &opened.service;
    service.refresh(false).await.unwrap();
    // Someone else holds the graph: the next refresh fails.
    let lease = service
        .store()
        .lease(GraphKey::LOCAL)
        .await
        .unwrap()
        .unwrap();
    let failed = service.refresh(false).await.unwrap_err();
    drop(lease);
    let status = service.status();
    assert_eq!(status.state, IndexState::Error);
    assert_eq!(status.error.as_deref(), Some(failed.message.as_str()));

    service.start_refresh(false).unwrap();
    assert!(service.cancel());
    for _ in 0..500 {
        if !service.is_building() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    let status = service.status();
    assert_eq!(status.state, IndexState::Error);
    assert_eq!(
        status.error.as_deref(),
        Some(failed.message.as_str()),
        "the failure's message comes back with its state"
    );
}

/// An incremental refresh starts from the build in memory when it is the
/// stored revision; it loads from SQLite only when the stored graph moved.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn an_incremental_refresh_starts_from_the_build_in_memory() {
    use elitea_inventory_core::store::{Completion, RunCounts};
    let opened = open();
    let service = &opened.service;
    // Nothing in memory yet: the first refresh reads the store.
    service.refresh(false).await.unwrap();
    let opened_loads = service.store().loads();
    service.refresh(false).await.unwrap();
    write(
        opened.folder.path(),
        "pkg/extra.py",
        "def extra():
    pass
",
    );
    service.refresh(false).await.unwrap();
    assert_eq!(
        service.store().loads(),
        opened_loads,
        "no reload while in step"
    );
    assert_eq!(service.last_report().unwrap().read, 1);

    // Another writer moves the stored graph: the next refresh loads it.
    let (view, _) = service.view().unwrap();
    let other = elitea_local_index::sqlite_store::SqliteGraphStore::open(
        service.store().path().parent().unwrap(),
    )
    .unwrap();
    other
        .complete_with_stats(
            GraphKey::LOCAL,
            &view.graph,
            &Completion {
                toolkit_id: "workspace",
                source_name: "workspace",
                documents: &std::collections::BTreeMap::default(),
                counts: RunCounts::default(),
                commit_sha: None,
            },
            &std::collections::HashMap::default(),
            Some(service.policy()),
        )
        .unwrap();
    other.close();
    service.refresh(false).await.unwrap();
    assert_eq!(service.store().loads(), opened_loads + 1);
}

/// At most one refresh runs at a time across workspaces: the second waits
/// (`queued`) until the first ends.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn refreshes_of_different_workspaces_never_overlap() {
    let events = Arc::new(Recorded::default());
    let mut services = Vec::new();
    let mut guards = Vec::new();
    for id in ["a", "b", "c"] {
        let folder = tempfile::tempdir().unwrap();
        for n in 0..60 {
            write(
                folder.path(),
                &format!("m{n}.py"),
                &format!(
                    "class C{n}:
    def m(self):
        return C{}()
",
                    (n + 1) % 60
                ),
            );
        }
        let data = tempfile::tempdir().unwrap();
        let service = IndexService::open(
            id,
            folder.path(),
            &[],
            &data.path().join("index"),
            events.clone(),
        )
        .unwrap();
        services.push(service);
        guards.push((folder, data));
    }
    for service in &services {
        service.start_refresh(false).unwrap();
    }
    for _ in 0..1000 {
        if services.iter().all(|service| !service.is_building()) {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert!(
        services
            .iter()
            .all(|s| s.status().state == IndexState::Ready)
    );
    let mut running: Option<String> = None;
    for event in events.0.lock().unwrap().iter() {
        match event.phase {
            "listing" => {
                assert!(
                    running.is_none(),
                    "{} started while {running:?} ran",
                    event.workspace_id
                );
                running = Some(event.workspace_id.clone());
            }
            "ready" | "cancelled" | "error" => {
                assert_eq!(running.as_deref(), Some(event.workspace_id.as_str()));
                running = None;
            }
            _ => {}
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_index_reopens_stale_and_closing_stops_everything() {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    let data = tempfile::tempdir().unwrap();
    let dir = data.path().join("index");
    let first = IndexService::open("ws", folder.path(), &[], &dir, Arc::new(NoEvents)).unwrap();
    let built = first.refresh(false).await.unwrap();
    first.close();
    assert_eq!(first.status().state, IndexState::Off);
    assert_eq!(first.refresh(false).await.unwrap_err().code, "index_closed");

    let again = IndexService::open("ws", folder.path(), &[], &dir, Arc::new(NoEvents)).unwrap();
    let status = again.status();
    assert_eq!(
        status.state,
        IndexState::Stale,
        "not checked since it opened"
    );
    assert_eq!(status.entities, built.entities, "the last build answers");
    assert_eq!(status.files, built.files);
    let (_, notice) = again.view().unwrap();
    assert!(notice.unwrap().contains("not been checked"));
    // An index directory inside the workspace is refused.
    let inside = IndexService::open(
        "ws",
        folder.path(),
        &[],
        &folder.path().join(".index"),
        Arc::new(NoEvents),
    );
    assert_eq!(inside.unwrap_err().code, "index_refused");
}

async fn call(service: &Arc<IndexService>, name: &str, args: Value) -> Value {
    let tools = IndexToolset::new(service.clone()).current_tools();
    let tool = tools
        .iter()
        .find(|tool| adk_core::Tool::name(tool.as_ref()) == name)
        .unwrap();
    tool.call(args).await
}

fn text(result: &Value) -> &str {
    assert_eq!(result["status"], "ok", "{result}");
    result["result"].as_str().unwrap()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_tools_answer_from_the_build_and_say_when_it_may_be_stale() {
    let opened = open();
    let service = &opened.service;
    let not_ready = call(service, "search_knowledge_graph", json!({"query": "Users"})).await;
    assert_eq!(not_ready["status"], "error");
    assert_eq!(not_ready["code"], "index.not_ready");

    service.refresh(false).await.unwrap();
    let found = call(service, "search_knowledge_graph", json!({"query": "Users"})).await;
    assert!(text(&found).contains("Users"), "{found}");
    assert!(!text(&found).starts_with("Note:"));

    let types = call(service, "list_entity_types", json!({})).await;
    assert!(text(&types).contains("class"), "{types}");

    let details = call(
        service,
        "get_entity_details",
        json!({"entity_name": "Store"}),
    )
    .await;
    assert!(text(&details).contains("Store"), "{details}");

    let impact = call(
        service,
        "impact_analysis",
        json!({"entity_name": "Store", "direction": "upstream"}),
    )
    .await;
    assert_eq!(impact["status"], "ok", "{impact}");

    let query = call(service, "query_graph", json!({"query": "type:class"})).await;
    assert!(text(&query).contains("Users"), "{query}");

    // The cited lines, read from the folder.
    let content = call(
        service,
        "get_entity_content",
        json!({"entity_name": "Store"}),
    )
    .await;
    let content = text(&content);
    assert!(content.contains("Location: pkg/store.py:1"), "{content}");
    assert!(content.contains("def save(self, value):"), "{content}");
    let missing = call(
        service,
        "get_entity_content",
        json!({"entity_name": "Nope"}),
    )
    .await;
    assert!(text(&missing).contains("not found"), "{missing}");

    // A turn changed two files: answers carry the notice until a refresh.
    service.mark_changed(2);
    assert_eq!(service.status().state, IndexState::Stale);
    let stale = call(service, "search_knowledge_graph", json!({"query": "Users"})).await;
    assert!(
        text(&stale).starts_with(
            "Note: the workspace index may be out of date: 2 file(s) changed since the last build."
        ),
        "{stale}"
    );
    service.refresh(false).await.unwrap();
    let fresh = call(service, "search_knowledge_graph", json!({"query": "Users"})).await;
    assert!(!text(&fresh).starts_with("Note:"));
}

#[tokio::test]
async fn the_toolset_is_read_only_and_named_as_the_cloud_tools() {
    let opened = open();
    let toolset = IndexToolset::new(opened.service.clone());
    assert_eq!(adk_core::Toolset::name(&toolset), TOOLSET_NAME);
    let tools = toolset.current_tools();
    let names: Vec<&str> = tools
        .iter()
        .map(|tool| adk_core::Tool::name(tool.as_ref()))
        .collect();
    assert_eq!(names, NAMES);
    assert_eq!(TOOLS.len() + LOCAL_TOOLS.len(), NAMES.len());
    for tool in &tools {
        assert!(adk_core::Tool::is_read_only(tool.as_ref()));
        assert!(adk_core::Tool::is_concurrency_safe(tool.as_ref()));
        let schema = adk_core::Tool::parameters_schema(tool.as_ref()).unwrap();
        assert_eq!(schema["type"], "object");
    }
}

/// The edges into the nodes named `target`, as `(source name, relation)`.
fn edges_into(service: &Arc<IndexService>, target: &str) -> Vec<(String, String)> {
    let (view, _) = service.view().unwrap();
    let name = |id: &str| {
        view.graph
            .node(id)
            .and_then(|node| node.get("name"))
            .and_then(Value::as_str)
            .unwrap_or(id)
            .to_owned()
    };
    view.graph
        .edges()
        .filter(|(_, to, _)| name(to) == target)
        .map(|(from, _, edge)| {
            (
                name(from),
                edge.get("relation_type")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned(),
            )
        })
        .collect()
}

/// #1219 regression: editing the file that defines a function keeps the
/// `calls` edge from an unchanged caller in another file. Re-reading `b.rs`
/// removes `f` (and every edge into it) before parsing it again; without
/// the dependents expansion `a.rs` is skipped (stat cache) and its edge is
/// lost.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn dependents_expansion_keeps_cross_file_calls_edge() {
    let folder = tempfile::tempdir().unwrap();
    write(
        folder.path(),
        "src/a.rs",
        "use crate::b::f;\n\npub fn run() {\n    f();\n}\n",
    );
    write(folder.path(), "src/b.rs", "pub fn f() {}\n");
    let data = tempfile::tempdir().unwrap();
    let service = IndexService::open(
        "ws1",
        folder.path(),
        &[],
        &data.path().join("index"),
        Arc::new(NoEvents),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    let before = edges_into(&service, "f");
    assert!(
        before
            .iter()
            .any(|(from, kind)| from == "run" && kind == "calls"),
        "{before:?}"
    );

    write(
        folder.path(),
        "src/b.rs",
        "/// Edited.\npub fn f() {}\n\npub fn g() {}\n",
    );
    service.refresh(false).await.unwrap();
    let report = service.last_report().unwrap();
    let after = edges_into(&service, "f");
    assert!(
        after
            .iter()
            .any(|(from, kind)| from == "run" && kind == "calls"),
        "{after:?} ({report:?})"
    );
    // The caller was read again for it; only the edited file drifts.
    assert_eq!(
        (report.read, report.unchanged, report.dependents),
        (2, 0, 1),
        "{report:?}"
    );
    assert_eq!(service.store().drift_files().unwrap(), 1);

    // Editing the caller alone re-reads nothing else (`f` is not its).
    write(
        folder.path(),
        "src/a.rs",
        "use crate::b::f;\n\n/// Runs.\npub fn run() {\n    f();\n}\n",
    );
    service.refresh(false).await.unwrap();
    let report = service.last_report().unwrap();
    assert_eq!((report.read, report.dependents), (1, 0), "{report:?}");
    assert!(
        edges_into(&service, "f")
            .iter()
            .any(|(from, kind)| from == "run" && kind == "calls")
    );
}

/// #1219 review: a dependent read again keeps the edges into its own
/// nodes. `c.rs` calls `b::helper`, `b.rs` calls `a::f`; editing `a.rs`
/// reads `b.rs` again, and `c.rs`'s call into `helper` (a node only `b.rs`
/// cites) survives though `c.rs` is not read.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_dependent_read_again_keeps_the_edges_into_its_nodes() {
    let folder = tempfile::tempdir().unwrap();
    write(folder.path(), "src/a.rs", "pub fn f() {}\n");
    write(
        folder.path(),
        "src/b.rs",
        "use crate::a::f;\n\npub fn helper() {\n    f();\n}\n",
    );
    write(
        folder.path(),
        "src/c.rs",
        "use crate::b::helper;\n\npub fn run() {\n    helper();\n}\n",
    );
    let data = tempfile::tempdir().unwrap();
    let service = IndexService::open(
        "ws1",
        folder.path(),
        &[],
        &data.path().join("index"),
        Arc::new(NoEvents),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    let calls = |edges: &[(String, String)], caller: &str| {
        edges
            .iter()
            .any(|(from, kind)| from == caller && kind == "calls")
    };
    let before = edges_into(&service, "helper");
    assert!(calls(&before, "run"), "{before:?}");

    write(folder.path(), "src/a.rs", "/// Edited.\npub fn f() {}\n");
    service.refresh(false).await.unwrap();
    let report = service.last_report().unwrap();
    // `b.rs` is read again for its call into `f`; `c.rs` is not.
    assert_eq!(
        (report.read, report.unchanged, report.dependents),
        (2, 1, 1),
        "{report:?}"
    );
    let into_f = edges_into(&service, "f");
    assert!(calls(&into_f, "helper"), "{into_f:?}");
    let after = edges_into(&service, "helper");
    assert!(calls(&after, "run"), "{after:?} ({report:?})");

    // The same graph a full build makes.
    let graph = || {
        let (view, _) = service.view().unwrap();
        let mut nodes: Vec<String> = view.graph.nodes().map(|(id, _)| id.to_owned()).collect();
        let mut edges: Vec<(String, String)> = view
            .graph
            .edges()
            .map(|(from, to, _)| (from.to_owned(), to.to_owned()))
            .collect();
        nodes.sort();
        edges.sort();
        (nodes, edges)
    };
    let incremental = graph();
    service.refresh(true).await.unwrap();
    assert_eq!(incremental, graph());
}

/// Files changed since the last full build add up; past the threshold the
/// index asks for a full rebuild, and one resets the count.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn drift_past_the_threshold_asks_for_a_full_rebuild() {
    use elitea_local_index::service::DRIFT_REBUILD;
    let opened = open();
    let service = &opened.service;
    service.refresh(false).await.unwrap();
    // The first build is a full one.
    assert_eq!(service.store().drift_files().unwrap(), 0);
    write(opened.folder.path(), "notes.txt", "changed\n");
    fs::remove_file(opened.folder.path().join("README.md")).unwrap();
    service.refresh(false).await.unwrap();
    assert_eq!(service.store().drift_files().unwrap(), 2);
    assert!(!service.wants_full_rebuild());

    service.store().set_drift_files(DRIFT_REBUILD + 1).unwrap();
    assert!(service.wants_full_rebuild());
    service.refresh(true).await.unwrap();
    assert_eq!(service.store().drift_files().unwrap(), 0);
    assert!(!service.wants_full_rebuild());
}

#[test]
fn repo_map_listed_in_names() {
    assert!(NAMES.contains(&"repo_map"));
    let spec = LOCAL_TOOLS
        .iter()
        .find(|spec| spec.name == "repo_map")
        .unwrap();
    let schema = (spec.parameters)();
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(schema["properties"]["max_tokens"]["default"], 2048);
    assert_eq!(schema["properties"]["focus_symbols"]["maxItems"], 20);
}

/// #1240: the map ranks the focus symbol's file first, cites its lines
/// read from the folder, and never names a `path_deny` file, even one the
/// focus names.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn repo_map_omits_denied_paths() {
    let folder = tempfile::tempdir().unwrap();
    fixture(folder.path());
    write(
        folder.path(),
        "secret/keys.py",
        "from pkg.store import Store\n\n\ndef load_keys():\n    return Store().save('k')\n",
    );
    let data = tempfile::tempdir().unwrap();
    let deny = vec!["secret".to_owned()];
    let service = IndexService::open(
        "ws1",
        folder.path(),
        &deny,
        &data.path().join("index"),
        Arc::new(NoEvents),
    )
    .unwrap();
    service.refresh(false).await.unwrap();
    let answer = call(
        &service,
        "repo_map",
        json!({"focus_symbols": ["Store", "load_keys"], "focus_paths": ["secret/keys.py"], "max_tokens": 512}),
    )
    .await;
    assert_eq!(answer["status"], "ok", "{answer}");
    let map = text(&answer);
    assert!(map.starts_with("Repository map:"), "{map}");
    assert!(
        !map.contains("secret") && !map.contains("load_keys"),
        "{map}"
    );
    let first = map.lines().nth(2).unwrap();
    assert_eq!(first, "pkg/store.py", "{map}");
    assert!(map.contains(": class Store:"), "{map}");
    assert!(map.len() <= 512 * 4, "{}", map.len());

    let invalid = call(&service, "repo_map", json!({"focus_paths": "pkg"})).await;
    assert_eq!(invalid["status"], "error", "{invalid}");
}
