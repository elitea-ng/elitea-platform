//! `run_tests` over a real `cargo test` of the fixture crate
//! (`tests/fixtures/projects/cargo`), under the OS sandbox where the host
//! has one (#1224): the failing test at its file and line, a passing run as
//! counts only, a compile error as the compiler's message, and the whole
//! log pageable with `read_output`.
//!
//! Without `cargo` on `PATH` the test skips, unless
//! `ELITEA_REQUIRE_TOOLCHAINS=1`.

use std::path::Path;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use elitea_local_tools::approvals::{MemoryChoices, WorkspaceSettings};
use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde_json::{Value, json};

#[derive(Default)]
struct Approving {
    asked: Mutex<Vec<ApprovalRequest>>,
}

#[async_trait]
impl ApprovalChannel for Approving {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.asked.lock().expect("lock").push(request);
        Ok(ApprovalOutcome::Decided {
            action: "approve".to_owned(),
            value: Value::Null,
        })
    }
}

fn has_cargo() -> bool {
    let found = std::process::Command::new("cargo")
        .arg("--version")
        .output()
        .is_ok_and(|output| output.status.success());
    assert!(
        found || std::env::var_os("ELITEA_REQUIRE_TOOLCHAINS").is_none(),
        "ELITEA_REQUIRE_TOOLCHAINS is set but cargo is not on PATH"
    );
    found
}

fn copy_tree(from: &Path, to: &Path) {
    std::fs::create_dir_all(to).expect("dir");
    for entry in std::fs::read_dir(from).expect("read fixture") {
        let entry = entry.expect("entry");
        let name = entry.file_name();
        if name == "target" || name == "Cargo.lock" {
            continue;
        }
        let path = entry.path();
        if entry.file_type().expect("type").is_dir() {
            copy_tree(&path, &to.join(&name));
        } else {
            std::fs::copy(&path, to.join(&name)).expect("copy");
        }
    }
}

#[tokio::test]
async fn cargo_fixture_failure_reports_file_and_line() {
    if !has_cargo() {
        eprintln!("skipped: cargo is not on PATH");
        return;
    }
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("ws");
    copy_tree(
        &Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/projects/cargo"),
        &root,
    );
    let data_dir = dir.path().join("data");
    let mut shell = ShellConfig::new(data_dir.join("tmp"));
    // Linux CI has no sandbox helper binary; on macOS Seatbelt confines
    // the run either way.
    shell.sandbox.allow_unenforced = true;
    let prompt = Arc::new(Approving::default());
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
        prompt,
        data_dir,
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: elitea_local_tools::budget::Budgets::default(),
    })
    .expect("session");

    // The detected runner, the failing test at its file and line.
    let failing = session.call("run_tests", "t1", json!({})).await;
    assert_eq!(failing["status"], "ok", "{failing}");
    let result = &failing;
    assert_eq!(result["kind"], "cargo", "{result}");
    assert_eq!(result["cwd"], ".");
    assert!(
        result["command"]
            .as_str()
            .is_some_and(|command| command.starts_with("cargo test --message-format=json")),
        "{result}"
    );
    assert_eq!(
        (result["passed"].as_u64(), result["failed"].as_u64()),
        (Some(1), Some(1))
    );
    let failure = &result["failures"][0];
    assert_eq!(failure["name"], "tests::breaks", "{result}");
    assert_eq!(failure["file"], "src/lib.rs");
    assert_eq!(failure["line"], 14);
    assert!(
        failure["message"]
            .as_str()
            .is_some_and(|message| message.contains("two and two")),
        "{failure}"
    );

    // The whole log pages with read_output.
    let output_id = result["output_id"].as_str().expect("output_id").to_owned();
    let page = session
        .call("read_output", "t2", json!({ "output_id": output_id }))
        .await;
    assert_eq!(page["status"], "ok", "{page}");
    assert!(page.to_string().contains("test result: FAILED"), "{page}");
    assert!(page.to_string().contains("tests::breaks"), "{page}");

    // A passing run is counts only, well under 500 tokens.
    let passing = session
        .call("run_tests", "t3", json!({ "filter": "adds" }))
        .await;
    let result = &passing;
    assert_eq!(result["exit_code"], 0, "{passing}");
    assert_eq!(
        (result["passed"].as_u64(), result["failed"].as_u64()),
        (Some(1), Some(0))
    );
    assert!(result.get("failures").is_none(), "{result}");
    assert!(result.get("output").is_none(), "{result}");
    let size = result.to_string().len();
    assert!(
        size < 500 * 3,
        "a passing run is small: {size} bytes: {result}"
    );

    // A compile error: the compiler's message at its file and line.
    std::fs::write(
        root.join("src/lib.rs"),
        "pub fn add(a: i32, b: i32) -> i32 {\n    a + c\n}\n",
    )
    .expect("break the build");
    let broken = session.call("run_tests", "t4", json!({})).await;
    let result = &broken;
    assert_eq!(result["build_failed"], true, "{broken}");
    let error = &result["errors"][0];
    assert_eq!(error["file"], "src/lib.rs", "{result}");
    assert_eq!(error["line"], 2);
    assert!(
        error["message"]
            .as_str()
            .is_some_and(|message| message.contains("cannot find value `c`")),
        "{error}"
    );
}
