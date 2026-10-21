//! A `forbidden` prefix rule refuses `run_command` with a result the model
//! can act on (#1216): `{"error": "forbidden_by_rule", "justification",
//! "rule"}` beside the usual error fields, and the command never runs.

use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use elitea_local_tools::approvals::{CommandRules, MemoryChoices, WorkspaceSettings};
use elitea_local_tools::execpolicy::RuleSet;
use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};
use elitea_local_tools::session::{LocalSession, SessionConfig};
use serde_json::json;

#[derive(Default)]
struct RecordingPrompt {
    seen: Mutex<Vec<ApprovalRequest>>,
}

#[async_trait]
impl ApprovalChannel for RecordingPrompt {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.seen.lock().expect("lock").push(request);
        Ok(ApprovalOutcome::Decided {
            action: "reject".to_owned(),
            value: json!({ "reason": "the person rejected it" }),
        })
    }
}

#[tokio::test]
async fn a_forbidden_rule_refuses_with_its_justification_for_the_model() {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("ws");
    std::fs::create_dir_all(&root).expect("ws");
    let prompt = Arc::new(RecordingPrompt::default());
    let session = LocalSession::open(SessionConfig {
        root: root.clone(),
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: true,
            max_sandbox_mode: SandboxMode::WorkspaceWrite,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: prompt.clone(),
        data_dir: dir.path().join("data"),
        shell: None,
        deny_read: Vec::new(),
        app_id: None,
    })
    .expect("session");
    session.rules().set_command_rules(CommandRules {
        workspace: RuleSet::parse(
            "workspace",
            br#"{"version":1,"rules":[{"pattern":["touch"],"decision":"forbidden",
              "justification":"Create files with write_file instead.","match":["touch x"]}]}"#,
        )
        .expect("rules"),
        user: RuleSet::default(),
    });

    let result = session
        .call(
            "run_command",
            "c1",
            json!({ "command": "sh -c 'touch made-by-shell'" }),
        )
        .await;
    assert_eq!(result["status"], "error", "{result}");
    assert_eq!(result["code"], "local_tools.denied");
    assert_eq!(result["error"], "forbidden_by_rule");
    assert_eq!(result["rule"], "workspace#1");
    assert_eq!(
        result["justification"],
        "Create files with write_file instead."
    );
    assert!(
        result["message"]
            .as_str()
            .is_some_and(|message| message.contains("workspace#1")),
        "{result}"
    );
    assert!(!root.join("made-by-shell").exists(), "it never ran");
    assert!(
        prompt.seen.lock().expect("lock").is_empty(),
        "refused inline, never asked"
    );
}
