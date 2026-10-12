//! The session's deny list reaches the in-process file tools: a workspace
//! bound at the home directory holds `~/.ssh` and the desktop app's own
//! data, and neither is read, listed, searched or written, with the same
//! error class as `path_deny`. The host's resolved directories and the
//! session's data directory count the same.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde_json::{Value, json};

const APP_ID: &str = "ai.elitea.desktop";

fn plant(home: &Path, path: &str, content: &str) {
    let path = home.join(path);
    std::fs::create_dir_all(path.parent().expect("parent")).expect("parents");
    std::fs::write(path, content).expect("plant");
}

struct Fixture {
    _dir: tempfile::TempDir,
    session: Arc<LocalSession>,
}

/// A session whose workspace is the fixture home itself.
fn fixture() -> (Fixture, PathBuf) {
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let home = base.join("home");
    plant(&home, ".ssh/id_ed25519", "SECRET-SSH-KEY\n");
    plant(
        &home,
        "Library/Application Support/ai.elitea.desktop/credentials.json",
        "SECRET-REFRESH-TOKEN\n",
    );
    plant(
        &home,
        "xdg-config/elitea/settings.json",
        "SECRET-HOST-DIR\n",
    );
    plant(&home, "notes.txt", "SECRET-free notes\n");
    let mut shell = ShellConfig::new(base.join("tmp"));
    shell.home = Some(home.clone());
    shell.sandbox.allow_unenforced = true;
    let session = LocalSession::open(SessionConfig {
        root: home.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(DeferringPrompt),
        data_dir: base.join("data"),
        shell: Some(shell),
        // The host's resolved config directory (an XDG override inside the
        // home) and its identifier.
        deny_read: vec![home.join("xdg-config/elitea")],
        app_id: Some(APP_ID.to_owned()),
    })
    .expect("session");
    (Fixture { _dir: dir, session }, home)
}

async fn call(fixture: &Fixture, tool: &str, args: Value) -> Value {
    fixture.session.call(tool, "c", args).await
}

#[tokio::test]
async fn reads_are_refused_like_path_deny() {
    let (fixture, home) = fixture();
    for path in [
        ".ssh/id_ed25519".to_owned(),
        ".ssh".to_owned(),
        "Library/Application Support/ai.elitea.desktop/credentials.json".to_owned(),
        "xdg-config/elitea/settings.json".to_owned(),
        // Absolute, as the model may write it.
        home.join(".ssh/id_ed25519").display().to_string(),
    ] {
        for tool in ["read_file", "read_document"] {
            let result = call(&fixture, tool, json!({ "path": path })).await;
            assert_eq!(
                result["code"], "local_tools.denied",
                "{tool} {path}: {result}"
            );
            assert!(!result.to_string().contains("SECRET-"), "{tool} {path}");
        }
    }
    let write = call(
        &fixture,
        "write_file",
        json!({ "path": ".ssh/authorized_keys", "content": "x" }),
    )
    .await;
    assert_eq!(write["code"], "local_tools.denied", "{write}");
    assert!(!home.join(".ssh/authorized_keys").exists());
    let notes = call(&fixture, "read_file", json!({ "path": "notes.txt" })).await;
    assert_eq!(notes["status"], "ok", "{notes}");
}

#[tokio::test]
async fn listings_and_searches_leave_them_out() {
    let (fixture, _home) = fixture();
    let tree = call(&fixture, "list_tree", json!({ "depth": 8 })).await;
    assert_eq!(tree["status"], "ok", "{tree}");
    let text = tree.to_string();
    assert!(text.contains("notes.txt"), "{text}");
    for hidden in ["id_ed25519", "credentials.json", "settings.json"] {
        assert!(!text.contains(hidden), "{hidden} listed: {text}");
    }
    let search = call(
        &fixture,
        "search_files",
        json!({ "pattern": "SECRET-", "fixed_strings": true }),
    )
    .await;
    assert_eq!(search["status"], "ok", "{search}");
    let text = search.to_string();
    assert!(text.contains("notes.txt"), "the search ran: {text}");
    for secret in ["SECRET-SSH-KEY", "SECRET-REFRESH-TOKEN", "SECRET-HOST-DIR"] {
        assert!(!text.contains(secret), "{secret} found: {text}");
    }
}

/// The list is built once per session and shared: the commands' sandbox
/// and the file tools read the same one, and asking again does not build
/// (or resolve) it again.
#[tokio::test]
async fn one_list_per_session_shared_by_every_tool() {
    let (fixture, home) = fixture();
    let list = fixture.session.deny_list();
    assert!(
        Arc::ptr_eq(&list, &fixture.session.deny_list()),
        "built once"
    );
    assert!(
        Arc::ptr_eq(
            &list,
            fixture
                .session
                .workspace()
                .deny_list()
                .expect("workspace list")
        ),
        "the file tools read the session's list"
    );
    let request = fixture
        .session
        .sandbox_request(
            elitea_local_tools::policy::SandboxMode::WorkspaceWrite,
            false,
        )
        .expect("request");
    for path in list.paths() {
        assert!(request.deny_paths.contains(path), "{}", path.display());
    }
    assert!(list.literals().contains(&home.join(".ssh")));
}
