//! The one log-capture helper for tests (feature `logtest`).
//!
//! A capture through `tracing::subscriber::with_default` races parallel
//! tests: a callsite another thread hits first can keep an interest that
//! leaves the scoped capture empty, and an empty capture makes every "never
//! logs X" assertion pass silently. So the test re-runs its own binary for
//! one child test, the child installs a GLOBAL subscriber writing to stdout
//! (and stderr), prints [`CHILD_RAN`], and the parent reads the output.
//!
//! ```ignore
//! #[test]
//! fn parent() {
//!     let output = logtest::run_child("my_module::tests::child", "MY_CHILD_ENV");
//!     let lines = output.operator_lines();
//! }
//!
//! #[test]
//! fn child() {
//!     if !logtest::is_child("MY_CHILD_ENV") { return; }
//!     /* install the global subscriber, log, then */ logtest::child_ran();
//! }
//! ```

use serde_json::{Map, Value};

/// What a child prints last, so an empty capture cannot pass.
pub const CHILD_RAN: &str = "LOGTEST-CHILD-RAN";

/// The output of one child test.
pub struct ChildOutput {
    pub stdout: String,
    pub stderr: String,
}

impl ChildOutput {
    /// Every stdout line that starts with `{`, parsed (libtest's own
    /// `test name ... ` prefix, printed without a newline before the child's
    /// first line, is dropped). Panics on such a line that is not one JSON
    /// object: the operator stream must be valid JSON per line.
    ///
    /// # Panics
    ///
    /// On a line that starts like JSON and is not one JSON object.
    #[must_use]
    pub fn operator_lines(&self) -> Vec<Map<String, Value>> {
        self.stdout
            .lines()
            .filter_map(|line| {
                if line.starts_with('{') {
                    Some(line)
                } else if line.starts_with("test ") {
                    line.find(" ... {").map(|at| &line[at + 5..])
                } else {
                    None
                }
            })
            .map(|line| match serde_json::from_str::<Value>(line) {
                Ok(Value::Object(object)) => object,
                _ => panic!("operator stream line is not one JSON object: {line}"),
            })
            .collect()
    }
}

/// Whether this process is the child `run_child` started.
#[must_use]
pub fn is_child(environment: &str) -> bool {
    std::env::var_os(environment).is_some()
}

/// Marks the child's run complete; call it last in the child.
pub fn child_ran() {
    println!("{CHILD_RAN}");
}

/// Runs the test named `test_path` (as libtest prints it) in a child process
/// with `environment=1` and returns its output, asserting it succeeded and
/// reached [`child_ran`].
///
/// # Panics
///
/// When the child cannot start, fails, or never ran.
#[must_use]
pub fn run_child(test_path: &str, environment: &str) -> ChildOutput {
    run_child_with(test_path, environment, &[])
}

/// [`run_child`] with extra environment variables for the child.
///
/// # Panics
///
/// When the child cannot start, fails, or never ran.
#[must_use]
pub fn run_child_with(test_path: &str, environment: &str, extra: &[(&str, &str)]) -> ChildOutput {
    let executable = std::env::current_exe().expect("the test binary");
    let mut command = std::process::Command::new(executable);
    command
        .args(["--exact", test_path, "--nocapture", "--test-threads=1"])
        .env(environment, "1");
    for (key, value) in extra {
        command.env(key, value);
    }
    let output = command.output().expect("the log capture child process");
    let captured = ChildOutput {
        stdout: String::from_utf8_lossy(&output.stdout).into_owned(),
        stderr: String::from_utf8_lossy(&output.stderr).into_owned(),
    };
    assert!(
        output.status.success(),
        "child {test_path} failed\n{}\n{}",
        captured.stdout,
        captured.stderr
    );
    assert!(
        captured.stdout.contains(CHILD_RAN),
        "child {test_path} never ran\n{}",
        captured.stdout
    );
    captured
}
