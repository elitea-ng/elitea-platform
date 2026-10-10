//! The workspace folder as a content source: what is listed, what is
//! hashed, and a build into the SQLite store through the shared ingestion.

use elitea_content_source::ContentSource as _;
use elitea_inventory_core::ingest::files::Selection;
use elitea_inventory_core::store::GraphKey;
use elitea_local_index::source::LocalFolderSource;
use elitea_local_index::sqlite_store::DocumentStat;
use elitea_local_tools::workspace::Workspace;
use std::collections::HashMap;
use std::fs;
use std::path::Path;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

fn write(root: &Path, path: &str, text: &str) {
    let path = root.join(path);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, text).unwrap();
}

fn workspace(root: &Path, deny: &[&str]) -> Arc<Workspace> {
    let deny: Vec<String> = deny.iter().map(|d| (*d).to_owned()).collect();
    Arc::new(Workspace::open(root, &deny).unwrap())
}

fn keys(source: &LocalFolderSource) -> Vec<String> {
    source
        .list_now()
        .unwrap()
        .into_iter()
        .map(|reference| reference.key)
        .collect()
}

#[test]
fn the_listing_is_what_the_agent_tools_see() {
    let folder = tempfile::tempdir().unwrap();
    let root = folder.path();
    write(root, "src/app.py", "def main():\n    pass\n");
    write(root, "README.md", "# hello\n");
    write(root, ".gitignore", "build/\n*.log\n");
    write(root, "build/out.py", "x = 1\n");
    write(root, "debug.log", "noise\n");
    write(root, ".git/config", "[core]\n");
    write(root, "secrets/key.py", "KEY = 1\n");
    write(root, ".env", "TOKEN=1\n");
    #[cfg(unix)]
    {
        let outside = tempfile::tempdir().unwrap();
        write(outside.path(), "stolen.py", "secret = 1\n");
        std::os::unix::fs::symlink(outside.path().join("stolen.py"), root.join("link.py")).unwrap();
        std::os::unix::fs::symlink(outside.path(), root.join("linked_dir")).unwrap();
    }
    // Past the 16 MiB the file tools read: left out (sparse, no disk used).
    fs::File::create(root.join("huge.py"))
        .unwrap()
        .set_len(17 * 1024 * 1024)
        .unwrap();
    let source = LocalFolderSource::new(workspace(root, &["secrets/**", ".env"]));
    assert_eq!(
        keys(&source),
        [".gitignore", "README.md", "src/app.py"],
        "gitignored, .git, path_deny, symlinks and oversized files are not listed"
    );
}

#[tokio::test]
async fn documents_are_versioned_by_content_and_read_confined() {
    let folder = tempfile::tempdir().unwrap();
    write(folder.path(), "a.py", "x = 1\n");
    let source = LocalFolderSource::new(workspace(folder.path(), &[]));
    let listed = source.list().await.unwrap();
    assert_eq!(listed.len(), 1);
    assert_eq!(
        listed[0].version,
        elitea_content_source::content_version(b"x = 1\n")
    );
    assert_eq!(listed[0].mime, "text/plain");
    let document = source.fetch("a.py").await.unwrap();
    assert_eq!(document.bytes, b"x = 1\n");
    assert_eq!(document.reference.version, listed[0].version);
    assert!(source.fetch("../etc/passwd").await.is_err());
    assert!(source.fetch("missing.py").await.is_err());
}

fn set_mtime(path: &Path, when: SystemTime) {
    fs::File::options()
        .write(true)
        .open(path)
        .unwrap()
        .set_modified(when)
        .unwrap();
}

fn previous_of(source: &LocalFolderSource) -> HashMap<String, DocumentStat> {
    let listed = source.list_now().unwrap();
    let stats = source.stats();
    listed
        .into_iter()
        .map(|reference| {
            let (size, mtime_ns) = stats[&reference.key];
            (
                reference.key,
                DocumentStat {
                    version: reference.version,
                    size,
                    mtime_ns,
                },
            )
        })
        .collect()
}

#[test]
fn only_files_whose_size_or_mtime_changed_are_hashed_again() {
    let folder = tempfile::tempdir().unwrap();
    let root = folder.path();
    write(root, "same.py", "a = 1\n");
    write(root, "edited.py", "b = 1\n");
    write(root, "touched.py", "c = 1\n");
    let past = SystemTime::now() - Duration::from_hours(1);
    for name in ["same.py", "edited.py", "touched.py"] {
        set_mtime(&root.join(name), past);
    }
    let ws = workspace(root, &[]);
    let first = LocalFolderSource::new(ws.clone());
    let previous = previous_of(&first);
    assert_eq!(first.hashed(), 3, "a first listing hashes everything");

    // Same size, new content, new mtime: hashed again, new version.
    fs::write(root.join("edited.py"), "b = 2\n").unwrap();
    // Touched, content unchanged: hashed again, same version.
    set_mtime(&root.join("touched.py"), SystemTime::now());
    let second = LocalFolderSource::new(ws).with_previous(previous.clone());
    let now = previous_of(&second);
    assert_eq!(second.hashed(), 2, "the unchanged file is not read");
    assert_eq!(now["same.py"].version, previous["same.py"].version);
    assert_ne!(now["edited.py"].version, previous["edited.py"].version);
    assert_eq!(now["touched.py"].version, previous["touched.py"].version);
    assert_ne!(now["touched.py"].mtime_ns, previous["touched.py"].mtime_ns);
}

#[test]
fn files_ingestion_would_skip_are_listed_unread() {
    let folder = tempfile::tempdir().unwrap();
    write(folder.path(), "a.py", "x = 1\n");
    write(folder.path(), "image.bin", "\0\0\0");
    let source =
        LocalFolderSource::new(workspace(folder.path(), &[])).selecting(Selection::default());
    let listed = source.list_now().unwrap();
    assert_eq!(listed.len(), 2);
    let binary = listed.iter().find(|r| r.key == "image.bin").unwrap();
    assert!(binary.version.is_empty(), "not hashed");
    assert_eq!(source.hashed(), 1);
    assert!(!source.stats().contains_key("image.bin"));
}

#[tokio::test]
async fn a_folder_builds_into_the_store_and_refreshes_incrementally() {
    use elitea_inventory_core::graph::Graph;
    use elitea_inventory_core::ingest::{SourceSelection, ingest_documents};
    use elitea_inventory_core::store::{Completion, GraphStore as _, RunCounts};
    use elitea_local_index::sqlite_store::SqliteGraphStore;

    let folder = tempfile::tempdir().unwrap();
    let root = folder.path();
    write(
        root,
        "pkg/users.py",
        "class Users:\n    def create(self):\n        return Store().save()\n",
    );
    write(
        root,
        "pkg/store.py",
        "class Store:\n    def save(self):\n        return 1\n",
    );
    write(root, "README.md", "# Demo\n\nUsers are stored by Store.\n");
    let data = tempfile::tempdir().unwrap();
    let store = SqliteGraphStore::open_for_workspace(&data.path().join("index"), root).unwrap();
    let ws = workspace(root, &[]);
    let selection = SourceSelection::all("workspace");
    let key = GraphKey::LOCAL;

    let build = |previous_stats: HashMap<String, DocumentStat>| {
        LocalFolderSource::new(ws.clone())
            .with_previous(previous_stats)
            .selecting(Selection::default())
    };
    let (stop, context) = context();
    let _ = stop;
    let source = build(HashMap::new());
    let mut graph = Graph::new();
    let outcome = ingest_documents(
        &mut graph,
        &selection,
        &source,
        ws.root(),
        &std::collections::BTreeMap::default(),
        &context,
    )
    .await
    .unwrap();
    assert_eq!(outcome.documents_processed, 3, "{outcome:?}");
    let names: Vec<String> = graph
        .nodes()
        .filter_map(|(_, node)| node.get("name")?.as_str().map(str::to_owned))
        .collect();
    assert!(names.iter().any(|n| n == "Users"), "{names:?}");
    store
        .complete_with_stats(
            key,
            &graph,
            &Completion {
                toolkit_id: "workspace",
                source_name: "workspace",
                documents: &outcome.documents,
                counts: RunCounts::default(),
                commit_sha: None,
            },
            &source.stats(),
            None,
        )
        .unwrap();

    // Edit one file; the refresh reads only it.
    fs::write(
        root.join("pkg/store.py"),
        "class Store:\n    def save(self):\n        return 2\n    def load(self):\n        return 3\n",
    )
    .unwrap();
    let (mut graph, _) = store.load(key).await.unwrap().unwrap();
    let previous = store.document_versions(key, "workspace").await.unwrap();
    let source = build(store.document_stats(key, "workspace").unwrap());
    let outcome = ingest_documents(
        &mut graph,
        &selection,
        &source,
        ws.root(),
        &previous,
        &context,
    )
    .await
    .unwrap();
    assert_eq!(outcome.documents_processed, 1, "{outcome:?}");
    assert_eq!(outcome.unchanged, 2);
    assert_eq!(source.hashed(), 1, "only the edited file was read to hash");
    assert!(
        graph
            .nodes()
            .any(|(_, node)| node.get("name").and_then(|n| n.as_str()) == Some("load"))
    );
}

fn context() -> (
    tokio::sync::mpsc::UnboundedReceiver<elitea_engine_core::stream::Line>,
    elitea_engine_core::stream::Context,
) {
    let (lines, receiver) = tokio::sync::mpsc::unbounded_channel();
    (
        receiver,
        elitea_engine_core::stream::Context::new(
            lines,
            elitea_engine_core::stream::StopSignal::default(),
        ),
    )
}

/// The folder source, with something done to the folder just before or
/// just after each fetch: a file changed between the listing and the read,
/// or swapped between the read and the parse.
struct Hooked {
    inner: LocalFolderSource,
    before_fetch: Box<dyn Fn(&str) + Send + Sync>,
    after_fetch: Box<dyn Fn(&str) + Send + Sync>,
}

impl Hooked {
    fn new(root: &Path) -> Self {
        Self {
            inner: LocalFolderSource::new(workspace(root, &[])).selecting(Selection::default()),
            before_fetch: Box::new(|_| {}),
            after_fetch: Box::new(|_| {}),
        }
    }
}

impl elitea_content_source::ContentSource for Hooked {
    async fn list(
        &self,
    ) -> Result<Vec<elitea_content_source::DocumentRef>, elitea_content_source::SourceError> {
        self.inner.list_now()
    }

    async fn fetch(
        &self,
        key: &str,
    ) -> Result<elitea_content_source::Document, elitea_content_source::SourceError> {
        (self.before_fetch)(key);
        let document = self.inner.fetch_now(key);
        (self.after_fetch)(key);
        document
    }
}

async fn ingest_hooked(
    source: &Hooked,
    root: &Path,
) -> (
    elitea_inventory_core::graph::Graph,
    elitea_inventory_core::ingest::Outcome,
    Vec<String>,
) {
    use elitea_inventory_core::ingest::{SourceSelection, ingest_documents};
    let (_lines, context) = context();
    let mut graph = elitea_inventory_core::graph::Graph::new();
    let outcome = ingest_documents(
        &mut graph,
        &SourceSelection::all("workspace"),
        source,
        root,
        &std::collections::BTreeMap::default(),
        &context,
    )
    .await
    .unwrap();
    let names = graph
        .nodes()
        .filter_map(|(_, node)| node.get("name")?.as_str().map(str::to_owned))
        .collect();
    (graph, outcome, names)
}

/// Plan risk 7: the parser parses the bytes the confined read returned. A
/// file replaced by a symlink to a file outside the folder after it was
/// read is not read through the link; before, the parser opened
/// `root/path` again and indexed the outside file's symbols.
#[cfg(unix)]
#[tokio::test]
async fn the_parser_parses_the_bytes_read_never_the_path_again() {
    use elitea_content_source::content_version;

    let folder = tempfile::tempdir().unwrap();
    let outside = tempfile::tempdir().unwrap();
    let root = folder.path().to_owned();
    write(&root, "app.py", "class Mine:\n    pass\n");
    write(outside.path(), "secret.py", "class Secret:\n    pass\n");
    let secret = outside.path().join("secret.py");
    let swapped = root.clone();
    let mut source = Hooked::new(&root);
    source.after_fetch = Box::new(move |key| {
        let path = swapped.join(key);
        fs::remove_file(&path).unwrap();
        std::os::unix::fs::symlink(&secret, &path).unwrap();
    });
    let (_, outcome, names) = ingest_hooked(&source, &root).await;
    assert!(names.iter().any(|n| n == "Mine"), "{names:?}");
    assert!(
        !names.iter().any(|n| n == "Secret"),
        "read through a symlink: {names:?}"
    );
    assert_eq!(
        outcome.hashes["app.py"],
        content_version(b"class Mine:\n    pass\n")
    );
}

/// The version recorded is the hash of the bytes parsed: a file that
/// changed between the listing and the read is recorded with what was
/// read, so the next run compares against what the graph holds.
#[tokio::test]
async fn the_version_recorded_is_the_one_of_the_bytes_parsed() {
    use elitea_content_source::content_version;

    let folder = tempfile::tempdir().unwrap();
    let root = folder.path().to_owned();
    write(&root, "app.py", "class Before:\n    pass\n");
    let after = "class After:\n    pass\n";
    let edited = root.join("app.py");
    let mut source = Hooked::new(&root);
    source.before_fetch = Box::new(move |_| fs::write(&edited, after).unwrap());
    let (_, outcome, names) = ingest_hooked(&source, &root).await;
    assert!(names.iter().any(|n| n == "After"), "{names:?}");
    assert_eq!(outcome.hashes["app.py"], content_version(after.as_bytes()));
    assert_eq!(
        outcome.documents["app.py"].version,
        content_version(after.as_bytes()),
        "not the listing's version of the old bytes"
    );
}
