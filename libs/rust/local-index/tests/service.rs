//! The index service and its tools over a real folder: a build, an
//! incremental refresh that re-reads only what changed, cancellation, the
//! stale notice, and the tools through the adk `Tool` interface.

use elitea_inventory_core::store::{GraphKey, GraphStore as _};
use elitea_local_index::service::{IndexEvent, IndexEvents, IndexService, IndexState, NoEvents};
use elitea_local_index::tools::{IndexToolset, NAMES, TOOLS, TOOLSET_NAME};
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
        .collect();
    assert_eq!(phases.first(), Some(&"listing"));
    assert_eq!(phases.last(), Some(&"ready"));
    assert!(phases.contains(&"saving"), "{phases:?}");

    // Nothing changed: nothing read, nothing hashed.
    service.refresh(false).await.unwrap();
    let report = service.last_report().unwrap();
    assert_eq!((report.read, report.unchanged, report.hashed), (0, 4, 0));

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
    assert_eq!(report.read, 2, "the edited and the added file");
    assert_eq!(report.unchanged, 2);
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
    let report = service
        .store()
        .status_document(GraphKey::LOCAL)
        .await
        .unwrap();
    assert_eq!(report["sources"]["workspace"]["error_message"], "cancelled");
    assert!(!service.cancel(), "nothing runs now");
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
    assert_eq!(TOOLS.len(), NAMES.len());
    for tool in &tools {
        assert!(adk_core::Tool::is_read_only(tool.as_ref()));
        assert!(adk_core::Tool::is_concurrency_safe(tool.as_ref()));
        let schema = adk_core::Tool::parameters_schema(tool.as_ref()).unwrap();
        assert_eq!(schema["type"], "object");
    }
}
