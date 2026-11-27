//! A folder too large to checkpoint is never changed silently (ADR-0029
//! amendment of 2026-11-27). The turn's first change asks the person
//! whether to go on without undo, once per turn. A "no" refuses every
//! change of the turn. An unattended or Full auto turn is never asked: its
//! changes are refused, with the reason. Bypass mode is attended, so it
//! asks too.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use serde_json::json;

use super::{LocalSession, SessionConfig};
use crate::approvals::{
    MemoryChoices, NO_UNDO_PAYLOAD_KEY, PermissionProfile, Unattended, WorkspaceSettings,
};
use crate::checkpoint::CheckpointLimits;
use crate::policy::LocalWorkPolicy;

/// Answers the no-undo question with `no_undo` (every other one with
/// "approve") and records the no-undo questions.
struct Prompt {
    no_undo: &'static str,
    asked: Mutex<Vec<ApprovalRequest>>,
}

#[async_trait]
impl ApprovalChannel for Prompt {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        let action = if request.payload.get(NO_UNDO_PAYLOAD_KEY).is_some() {
            self.asked.lock().expect("lock").push(request);
            self.no_undo
        } else {
            "approve"
        };
        Ok(ApprovalOutcome::Decided {
            action: action.to_owned(),
            value: json!({}),
        })
    }
}

struct Fixture {
    _dir: tempfile::TempDir,
    root: PathBuf,
    session: Arc<LocalSession>,
    prompt: Arc<Prompt>,
}

impl Fixture {
    fn asked(&self) -> usize {
        self.prompt.asked.lock().expect("lock").len()
    }

    async fn write(&self, call: &str, name: &str) -> serde_json::Value {
        self.session
            .call(
                "write_file",
                call,
                json!({ "path": name, "content": "new\n" }),
            )
            .await
    }
}

/// A plain folder of three files whose copy checkpoint allows `max_files`.
fn fixture(no_undo: &'static str, max_files: usize) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("folder");
    std::fs::create_dir_all(&root).expect("folder");
    for name in ["a.md", "b.md", "c.md"] {
        std::fs::write(root.join(name), name).expect("write");
    }
    let prompt = Arc::new(Prompt {
        no_undo,
        asked: Mutex::new(Vec::new()),
    });
    let mut session = LocalSession::open(SessionConfig {
        spill_dir: None,
        root: root.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: prompt.clone(),
        data_dir: dir.path().join("data"),
        shell: Some(crate::shell::ShellConfig::new(dir.path().join("tmp"))),
        deny_read: Vec::new(),
        app_id: None,
        budgets: crate::budget::Budgets::default(),
        documents: crate::document::Documents::default(),
    })
    .expect("session");
    Arc::get_mut(&mut session)
        .expect("one owner")
        .checkpoints
        .set_limits(CheckpointLimits {
            max_copy_files: max_files,
            ..CheckpointLimits::default()
        });
    session.begin_turn("turn");
    Fixture {
        _dir: dir,
        root,
        session,
        prompt,
    }
}

#[tokio::test]
async fn a_folder_within_the_limits_is_checkpointed_without_a_question() {
    let fx = fixture("reject", 100);
    let written = fx.write("w", "new.md").await;
    assert_eq!(written["status"], "ok", "{written}");
    assert_eq!(fx.asked(), 0);
    assert!(fx.session.turn_checkpoint().is_some());
}

#[tokio::test]
async fn going_on_without_undo_is_asked_once_before_the_first_change() {
    let fx = fixture("approve", 2);
    let written = fx.write("w1", "new.md").await;
    assert_eq!(written["status"], "ok", "{written}");
    assert_eq!(fx.asked(), 1);
    let asked = fx.prompt.asked.lock().expect("lock")[0].clone();
    assert!(
        asked.message.contains("cannot be undone"),
        "{}",
        asked.message
    );
    assert!(
        asked.payload[NO_UNDO_PAYLOAD_KEY]["reason"]
            .as_str()
            .is_some_and(|reason| reason.contains("more than 2 files"))
    );
    // Asked once per turn.
    let again = fx.write("w2", "other.md").await;
    assert_eq!(again["status"], "ok", "{again}");
    assert_eq!(fx.asked(), 1);
    assert!(fx.session.turn_checkpoint().is_none());
    assert!(fx.session.turn_checkpoint_skipped().is_some());
    // The next turn asks again.
    fx.session.begin_turn("next");
    fx.write("w3", "third.md").await;
    assert_eq!(fx.asked(), 2);
}

#[tokio::test]
async fn a_no_refuses_every_change_of_the_turn_and_changes_nothing() {
    let fx = fixture("reject", 2);
    let refused = fx.write("w1", "new.md").await;
    assert_eq!(refused["code"], "local_tools.rejected", "{refused}");
    assert!(
        refused["message"]
            .as_str()
            .is_some_and(|text| text.contains("too large to save for undo"))
    );
    assert!(!fx.root.join("new.md").exists());
    let again = fx.write("w2", "other.md").await;
    assert_eq!(again["code"], "local_tools.rejected", "{again}");
    assert!(!fx.root.join("other.md").exists());
    assert_eq!(fx.asked(), 1, "a no is not asked again in the turn");
}

#[tokio::test]
async fn an_unattended_turn_is_refused_without_a_question() {
    let fx = fixture("approve", 2);
    fx.session
        .rules()
        .set_unattended(Some(Unattended::default()));
    let refused = fx.write("w", "new.md").await;
    assert_eq!(refused["code"], "local_tools.rejected", "{refused}");
    assert!(
        refused["message"]
            .as_str()
            .is_some_and(|text| text.contains("unattended"))
    );
    assert_eq!(fx.asked(), 0);
    assert!(!fx.root.join("new.md").exists());
}

#[tokio::test]
async fn a_full_auto_turn_is_refused_without_a_question_and_bypass_still_asks() {
    let fx = fixture("approve", 2);
    fx.session.set_profile(PermissionProfile::FullAuto);
    let refused = fx.write("w", "new.md").await;
    assert_eq!(refused["code"], "local_tools.rejected", "{refused}");
    assert!(
        refused["message"]
            .as_str()
            .is_some_and(|text| text.contains("Full auto"))
    );
    assert_eq!(fx.asked(), 0);

    let bypass = fixture("approve", 2);
    bypass.session.set_profile(PermissionProfile::Bypass);
    let written = bypass.write("w", "new.md").await;
    assert_eq!(written["status"], "ok", "{written}");
    assert_eq!(bypass.asked(), 1, "bypass mode has a person to ask");
}

#[tokio::test]
async fn a_command_that_writes_asks_too() {
    let fx = fixture("reject", 2);
    let ran = fx
        .session
        .call(
            "run_command",
            "c",
            json!({ "command": "touch made.md", "sandbox": "workspace-write" }),
        )
        .await;
    assert_eq!(ran["code"], "local_tools.rejected", "{ran}");
    assert_eq!(fx.asked(), 1);
    assert!(!fx.root.join("made.md").exists());
}
