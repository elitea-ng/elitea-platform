//! `run_tests` over real runs of the fixture projects
//! (`tests/fixtures/projects/`), under the OS sandbox where the host has
//! one (#1224): `cargo test` (the failing test at its file and line, a
//! passing run as counts only, a compile error as the compiler's message,
//! the whole log pageable with `read_output`), `go test` and `pytest` (its
//! junit report read back from the session's `TMPDIR`).
//!
//! Without the runner on `PATH` a test skips, unless
//! `ELITEA_REQUIRE_TOOLCHAINS=1`.

use std::path::{Path, PathBuf};
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

/// Whether `program` runs; without it a test skips, unless
/// `ELITEA_REQUIRE_TOOLCHAINS=1`.
fn has(program: &str, version: &str) -> bool {
    let found = std::process::Command::new(program)
        .arg(version)
        .output()
        .is_ok_and(|output| output.status.success());
    assert!(
        found || std::env::var_os("ELITEA_REQUIRE_TOOLCHAINS").is_none(),
        "ELITEA_REQUIRE_TOOLCHAINS is set but {program} is not on PATH"
    );
    if !found {
        eprintln!("skipped: {program} is not on PATH");
    }
    found
}

/// The fixture project `name`, copied into a fresh workspace, and a
/// session over it.
fn fixture(name: &str) -> (tempfile::TempDir, PathBuf, Arc<LocalSession>) {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("ws");
    copy_tree(
        &Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("tests/fixtures/projects")
            .join(name),
        &root,
    );
    let data_dir = dir.path().join("data");
    let mut shell = ShellConfig::new(data_dir.join("tmp"));
    // Linux CI has no sandbox helper binary; on macOS Seatbelt confines
    // the run either way.
    shell.sandbox.allow_unenforced = true;
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
        prompt: Arc::new(Approving::default()),
        data_dir,
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: elitea_local_tools::budget::Budgets::default(),
        documents: elitea_local_tools::document::Documents::default(),
    })
    .expect("session");
    (dir, root, session)
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
    if !has("cargo", "--version") {
        return;
    }
    let (_dir, root, session) = fixture("cargo");

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
        (Some(1), Some(1)),
        "{result}"
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

#[tokio::test]
async fn go_fixture_failure_reports_file_and_line() {
    if !has("go", "version") {
        return;
    }
    let (dir, root, session) = fixture("go");
    std::fs::write(
        root.join("add_test.go"),
        "package fixture\n\nimport \"testing\"\n\nfunc TestAdds(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n\nfunc TestBreaks(t *testing.T) {\n\tif got := Add(2, 2); got != 5 {\n\t\tt.Errorf(\"Add(2, 2) = %d, want 5\", got)\n\t}\n}\n",
    )
    .expect("test file");
    let result = session.call("run_tests", "g1", json!({})).await;
    check_go(&result);
    // Go's build cache went to the cache root the sessions over this data
    // directory share (not the session's temporary directory), which the
    // sandbox let it write.
    let cache = dir.path().join("data/build-cache/go");
    assert!(
        std::fs::read_dir(&cache).is_ok_and(|mut entries| entries.next().is_some()),
        "{}",
        cache.display()
    );
}

fn check_go(result: &Value) {
    assert_eq!(result["kind"], "go", "{result}");
    assert_eq!(result["failed"], 1, "{result}");
    let failure = &result["failures"][0];
    assert_eq!(failure["file"], "add_test.go", "{result}");
    assert_eq!(failure["line"], 13);
    assert!(
        failure["message"]
            .as_str()
            .is_some_and(|message| message.contains("want 5")),
        "{failure}"
    );
}

#[tokio::test]
async fn pytest_fixture_failure_reports_file_and_line() {
    if !has("pytest", "--version") {
        return;
    }
    let (_dir, root, session) = fixture("pytest");
    std::fs::write(
        root.join("tests/test_add.py"),
        "def test_adds():\n    assert 1 + 1 == 2\n\n\ndef test_breaks():\n    assert 2 + 2 == 5\n",
    )
    .expect("test file");
    let result = session.call("run_tests", "p1", json!({})).await;
    assert_eq!(result["kind"], "pytest", "{result}");
    assert_eq!(
        (result["passed"].as_u64(), result["failed"].as_u64()),
        (Some(1), Some(1))
    );
    let failure = &result["failures"][0];
    assert_eq!(failure["file"], "tests/test_add.py", "{result}");
    assert_eq!(failure["line"], 6);
    assert!(
        result["report_id"].is_string(),
        "the junit report was read: {result}"
    );
}
