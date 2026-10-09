//! Approvals see what a write really changes: a path through an
//! in-workspace symlink is matched (and shown) as its final target, so a
//! rule that allows `docs/**` does not allow writing `src/main.rs` through
//! `docs/link.md`.

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
            rules: vec![WorkspaceRule {
                tools: vec![
                    ToolKind::WriteFile,
                    ToolKind::EditFile,
                    ToolKind::ApplyPatch,
                ],
                command: None,
                path: Some("docs/**".to_owned()),
                verdict: Verdict::Allow,
            }],
        },
        choices: Arc::new(MemoryChoices::default()),
        prompt: prompt.clone(),
        data_dir: dir.path().join("data"),
        shell: None,
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
        "each write was asked, not allowed by docs/**"
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
