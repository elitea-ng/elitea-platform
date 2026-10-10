//! Approvals see what a write really changes: a path through an
//! in-workspace symlink is matched (and shown) as its final target, so a
//! rule that asks before writing `src/**` is not bypassed through
//! `docs/link.md`, and one that allows `docs/**` does not vouch for it.

use std::os::unix::fs::symlink;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use elitea_local_tools::approvals::{
    MemoryChoices, ToolKind, Verdict, WorkspaceRule, WorkspaceSettings,
};
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::session::{LocalSession, SessionConfig};
use serde_json::{Value, json};

#[derive(Default)]
struct RejectingPrompt {
    seen: Mutex<Vec<ApprovalRequest>>,
}

#[async_trait]
impl ApprovalChannel for RejectingPrompt {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.seen.lock().expect("lock").push(request);
        Ok(ApprovalOutcome::Decided {
            action: "reject".to_owned(),
            value: json!({ "reason": "test" }),
        })
    }
}

const WRITES: [ToolKind; 3] = [
    ToolKind::WriteFile,
    ToolKind::EditFile,
    ToolKind::ApplyPatch,
];

#[tokio::test]
async fn writes_through_symlinks_are_approved_as_their_targets() {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("ws");
    std::fs::create_dir_all(root.join("docs")).expect("docs");
    std::fs::create_dir_all(root.join("src")).expect("src");
    std::fs::write(root.join("src/main.rs"), "fn main() {}\n").expect("main");
    std::fs::write(root.join("docs/readme.md"), "# docs\n").expect("readme");
    symlink("../src/main.rs", root.join("docs/link.md")).expect("file link");
    symlink("../src", root.join("docs/code")).expect("dir link");
    let prompt = Arc::new(RejectingPrompt::default());
    let session = LocalSession::open(SessionConfig {
        root: root.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings {
            rules: vec![
                WorkspaceRule {
                    tools: WRITES.to_vec(),
                    command: None,
                    path: Some("docs/**".to_owned()),
                    verdict: Verdict::Allow,
                },
                WorkspaceRule {
                    tools: WRITES.to_vec(),
                    command: None,
                    path: Some("src/**".to_owned()),
                    verdict: Verdict::Ask,
                },
            ],
        },
        choices: Arc::new(MemoryChoices::default()),
        prompt: prompt.clone(),
        data_dir: dir.path().join("data"),
        shell: None,
        deny_read: Vec::new(),
    })
    .expect("session");

    let read = session
        .call("read_file", "r", json!({ "path": "docs/link.md" }))
        .await;
    assert_eq!(read["status"], "ok", "{read}");
    let allowed = session
        .call(
            "edit_file",
            "e0",
            json!({ "path": "docs/readme.md", "old_string": "# docs", "new_string": "# Docs" }),
        )
        .await;
    assert_eq!(allowed["code"], "local_tools.stale_read", "{allowed}");

    let calls: [(&str, Value); 3] = [
        (
            "edit_file",
            json!({ "path": "docs/link.md", "old_string": "fn main", "new_string": "fn pwned" }),
        ),
        (
            "write_file",
            json!({ "path": "docs/code/new.rs", "content": "planted\n" }),
        ),
        (
            "apply_patch",
            json!({ "patch": "--- /dev/null\n+++ b/docs/code/patched.rs\n@@ -0,0 +1 @@\n+planted\n" }),
        ),
    ];
    for (index, (tool, args)) in calls.into_iter().enumerate() {
        let result = session.call(tool, &format!("c{index}"), args).await;
        assert_eq!(result["code"], "local_tools.rejected", "{tool}: {result}");
    }
    let seen = prompt.seen.lock().expect("lock").clone();
    assert_eq!(
        seen.len(),
        3,
        "each write was asked (src/**), not allowed by docs/**"
    );
    let shown: Vec<String> = seen
        .iter()
        .map(|request| request.payload.to_string() + &request.message)
        .collect();
    assert!(shown[0].contains("src/main.rs"), "{}", shown[0]);
    assert!(shown[1].contains("src/new.rs"), "{}", shown[1]);
    assert!(shown[2].contains("src/patched.rs"), "{}", shown[2]);
    assert_eq!(
        std::fs::read_to_string(root.join("src/main.rs")).expect("main"),
        "fn main() {}\n"
    );
    assert!(!root.join("src/new.rs").exists());
    assert!(!root.join("src/patched.rs").exists());
}

/// Approves, but first swaps `a.txt` for a link to `other.txt` and `docs`
/// for a link to `src`: what the person approved is no longer where the
/// write would land.
struct SwappingPrompt {
    root: std::path::PathBuf,
}

#[async_trait]
impl ApprovalChannel for SwappingPrompt {
    async fn request(&self, _request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        let _ = std::fs::remove_file(self.root.join("a.txt"));
        let _ = symlink("other.txt", self.root.join("a.txt"));
        let _ = std::fs::remove_dir_all(self.root.join("docs"));
        let _ = symlink("src", self.root.join("docs"));
        Ok(ApprovalOutcome::Decided {
            action: "approve".to_owned(),
            value: Value::Null,
        })
    }
}

/// `apply_patch` re-checks its targets after the approval, as `write_file` and
/// `edit_file` do: a symlink swapped in while the person decided does not
/// redirect the patch to a file they were never asked about.
#[tokio::test]
async fn a_patch_target_swapped_during_approval_is_refused() {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("ws");
    std::fs::create_dir_all(root.join("docs")).expect("docs");
    std::fs::create_dir_all(root.join("src")).expect("src");
    std::fs::write(root.join("a.txt"), "1\n").expect("a");
    std::fs::write(root.join("other.txt"), "1\n").expect("other");
    let session = LocalSession::open(SessionConfig {
        root: root.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            ..LocalWorkPolicy::default()
        },
        // Patches are asked here, so the person decides while the swap
        // happens.
        settings: WorkspaceSettings {
            rules: vec![WorkspaceRule {
                tools: vec![ToolKind::ApplyPatch],
                command: None,
                path: None,
                verdict: Verdict::Ask,
            }],
        },
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(SwappingPrompt { root: root.clone() }),
        data_dir: dir.path().join("data"),
        shell: None,
        deny_read: Vec::new(),
    })
    .expect("session");
    for path in ["a.txt", "other.txt"] {
        let read = session
            .call("read_file", "r", json!({ "path": path }))
            .await;
        assert_eq!(read["status"], "ok", "{read}");
    }
    let edit = session
        .call(
            "apply_patch",
            "p1",
            json!({ "patch": "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-1\n+2\n" }),
        )
        .await;
    assert_eq!(edit["code"], "local_tools.conflict", "{edit}");
    assert_eq!(
        std::fs::read_to_string(root.join("other.txt")).expect("other"),
        "1\n",
        "the link's target was not patched"
    );

    std::fs::remove_file(root.join("docs")).expect("unlink");
    std::fs::create_dir(root.join("docs")).expect("docs");
    let create = session
        .call(
            "apply_patch",
            "p2",
            json!({ "patch": "--- /dev/null\n+++ b/docs/new.txt\n@@ -0,0 +1 @@\n+x\n" }),
        )
        .await;
    assert_eq!(create["code"], "local_tools.conflict", "{create}");
    assert!(!root.join("src/new.txt").exists(), "nothing landed in src");
}
