//! `run_tests`: run the project's test command (or one the model gave),
//! read the runner's structured output ([`crate::test_report`]) and answer
//! with the failing tests, inside the session's command budget.
//!
//! The command runs exactly as `run_command` runs a command (the
//! [`crate::shell`] path: the OS sandbox in `workspace-write`, the
//! network off unless asked, the session's deny list, the same approval
//! rules); this module plans it and shapes its answer:
//!
//! ```json
//! {"kind": "cargo", "command": "cargo test --message-format=json", "cwd": ".",
//!  "exit_code": 101, "passed": 41, "failed": 1, "skipped": 0,
//!  "failures": [{"name": "tests::breaks", "file": "src/lib.rs", "line": 14,
//!                "message": "assertion `left == right` failed …"}],
//!  "output_id": "…", "truncated": false, "enforcement": "full", …}
//! ```
//!
//! * a build that fails answers `errors` (the compiler's messages at
//!   their file and line) and `build_failed: true`, not a test list;
//! * failures come first, at most [`MAX_INLINE`], each message at most
//!   2 KiB, all within the budget; the rest is counted in `more_failures`;
//! * the whole log (stdout, then stderr) is spilled to the session's
//!   spill files: `output_id` pages it with `read_output`; a junit report
//!   read for the run is spilled too (`report_id`);
//! * output no parser recognised comes back as its head and tail
//!   (`parsed: false`);
//! * an offline run that failed the way a dependency fetch fails answers
//!   `needs_network: true` and a hint: retry with `network: true` (the
//!   person is asked; "always allow" is kept for the runner kind).
//!
//! pytest writes its junit report into the session's temporary directory
//! (the commands' `TMPDIR`, writable in `workspace-write`); it is read
//! back without following a symlink and only when it is a regular file
//! with one link, owned by this user, then removed. Maven's and Gradle's
//! reports are read through the confined [`Workspace`], and only those
//! written by this run.

use std::io::Read as _;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::Deserialize;
use serde_json::{Map, Value, json};

use crate::budget::{self, SpillStore};
use crate::command::{CommandShape, analyse};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::project::{self, ProjectKind, ProjectProfile};
use crate::shell::CommandOutput;
use crate::test_report::{self, Failure, TestReport};
use crate::workspace::{EntryKind, Intent, Workspace, WsPath};

/// The most failures (and build errors) an answer shows inline.
pub const MAX_INLINE: usize = 20;

/// The largest junit report read back (one file).
pub const MAX_REPORT_BYTES: u64 = 8 * 1024 * 1024;

/// The most report files read for one Maven or Gradle run.
const MAX_REPORT_FILES: usize = 500;

/// The most bytes of report files read for one Maven or Gradle run.
const MAX_REPORT_TOTAL_BYTES: usize = 32 * 1024 * 1024;

/// How deep below the run's folder module folders are looked for (a
/// multi-module build's `app/core/target/surefire-reports`).
const MAX_MODULE_DEPTH: usize = 3;

/// The most folders listed looking for modules.
const MAX_MODULE_DIRS: usize = 256;

/// Folders never looked into for modules: build output, dependencies.
const NOT_MODULES: &[&str] = &["target", "build", "node_modules", "src", "out", "bin"];

/// The longest `filter` taken.
const MAX_FILTER: usize = 1024;

/// What part of the budget the answer's fixed fields and markers keep.
const RESERVE: usize = 1024;

/// How much of an unrecognised output's head and tail an answer shows
/// at most (within the budget).
const MAX_TEXT: usize = 8 * 1024;

/// `run_tests`' arguments.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct TestArgs {
    #[serde(default)]
    pub command: Option<String>,
    #[serde(default)]
    pub filter: Option<String>,
    #[serde(default)]
    pub cwd: Option<String>,
    #[serde(default)]
    pub timeout_seconds: Option<u64>,
    #[serde(default)]
    pub network: bool,
}

/// One planned run.
#[derive(Clone, Debug)]
pub(crate) struct Planned {
    pub kind: ProjectKind,
    /// As it runs, and as the rules and the person see it.
    pub command: String,
    pub cwd: WsPath,
}

/// The run `args` asks for: the detected runner for its folder (or the
/// given runner command), with the structured output on.
///
/// # Errors
///
/// No runner detected and none given, a command that is not a runner's,
/// a bad filter or folder.
pub(crate) fn plan(
    workspace: &Workspace,
    profile: &ProjectProfile,
    args: &TestArgs,
    junit: &Path,
) -> ToolResult<Planned> {
    let filter = args.filter.as_deref().map(str::trim);
    if let Some(filter) = filter
        && (filter.is_empty() || filter.len() > MAX_FILTER || filter.chars().any(char::is_control))
    {
        return Err(ToolError::invalid(
            "`filter` is 1 to 1024 characters, without control characters",
        ));
    }
    let typed = match &args.cwd {
        Some(cwd) => Some(workspace.resolve(cwd, Intent::Read)?),
        None => None,
    };
    let (kind, words, cwd) = if let Some(command) = &args.command {
        let given = project::given_command(command, filter, Some(junit))?;
        // Without `cwd`, the detected project's folder when exactly one
        // runner of the command's kind was found (a package script: one
        // JavaScript folder), else the workspace root.
        let cwd = typed.unwrap_or_else(|| {
            let javascript = |kind: ProjectKind| {
                matches!(
                    kind,
                    ProjectKind::Vitest | ProjectKind::Jest | ProjectKind::Script
                )
            };
            let mut matching = profile.runners.iter().filter(|runner| {
                runner.kind == given.kind
                    || (given.kind == ProjectKind::Script && javascript(runner.kind))
            });
            match (matching.next(), matching.next()) {
                (Some(runner), None) => runner.dir.clone(),
                _ => WsPath::root(),
            }
        });
        if let Some(script) = &given.script {
            project::check_package_script(workspace, &cwd, script)?;
        }
        (given.kind, given.argv, cwd)
    } else {
        let at = typed.clone().unwrap_or_default();
        let runner = profile.runner_for(&at).ok_or_else(|| {
            ToolError::new(
                ErrorCode::NotFound,
                format!(
                    "no test runner was detected for `{at}` (cargo, go, vitest, jest, pytest, \
                     maven or gradle); pass `command`"
                ),
            )
        })?;
        // A folder asked for below the runner's own (a workspace's member)
        // is not widened to the whole workspace: the run is narrowed to it.
        let (words, cwd) = match &typed {
            Some(member) if *member != runner.dir => {
                runner.member_test(workspace, member, filter, Some(junit))
            }
            _ => (runner.test_argv(filter, Some(junit)), runner.dir.clone()),
        };
        (runner.kind, words, cwd)
    };
    if workspace.stat(&cwd)? != Some(EntryKind::Dir) {
        return Err(ToolError::invalid(format!(
            "`{cwd}` is not a directory in the workspace"
        )));
    }
    let command = join(&words)?;
    // Quoted, it is still one plain command (no shell): what the rules
    // judge is what runs.
    if !matches!(analyse(&command), CommandShape::Simple(_)) {
        return Err(ToolError::invalid(
            "the test command (with its filter) would need a shell; pass a simpler filter",
        ));
    }
    Ok(Planned { kind, command, cwd })
}

/// The words as one command line: plain words as they are, the rest
/// quoted for a POSIX shell (what the person reads and the rules judge).
fn join(words: &[String]) -> ToolResult<String> {
    let plain = |word: &str| {
        !word.is_empty()
            && word
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || "-_./=:,+@%".contains(c))
    };
    let mut quoted = Vec::with_capacity(words.len());
    for word in words {
        if plain(word) {
            quoted.push(word.clone());
        } else {
            let word = shlex::try_quote(word)
                .map_err(|_| ToolError::invalid("the test command cannot be quoted"))?;
            quoted.push(word.into_owned());
        }
    }
    Ok(quoted.join(" "))
}

/// A fresh name for pytest's junit report in the session's temporary
/// directory (created, resolved).
///
/// # Errors
///
/// The directory cannot be created or resolved.
pub(crate) fn junit_path(temp_dir: &Path) -> ToolResult<PathBuf> {
    std::fs::create_dir_all(temp_dir).map_err(|error| {
        ToolError::io("cannot create the session's temporary directory", &error)
    })?;
    let temp = std::fs::canonicalize(temp_dir).map_err(|error| {
        ToolError::io("cannot resolve the session's temporary directory", &error)
    })?;
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since| since.as_nanos());
    Ok(temp.join(format!("elitea-junit-{}-{nanos:x}.xml", std::process::id())))
}

/// The junit report at `path`, read without following a symlink, only
/// when it is a regular file with one link that this user owns and of at
/// most [`MAX_REPORT_BYTES`]; removed after. `None` otherwise (not
/// written, or not trusted).
pub(crate) fn take_private_report(path: &Path) -> Option<String> {
    use rustix::fs::{FileType, Mode, OFlags};
    let fd = rustix::fs::open(
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
        Mode::empty(),
    )
    .ok();
    // Its links are counted before it is removed (removed whatever it is:
    // a later run must not read it).
    let stat = fd.as_ref().and_then(|fd| rustix::fs::fstat(fd).ok());
    let _ = rustix::fs::unlink(path);
    let (fd, stat) = (fd?, stat?);
    let trusted = FileType::from_raw_mode(stat.st_mode) == FileType::RegularFile
        && stat.st_nlink == 1
        && stat.st_uid == rustix::process::getuid().as_raw()
        && u64::try_from(stat.st_size).is_ok_and(|size| size <= MAX_REPORT_BYTES);
    if !trusted {
        return None;
    }
    let mut text = String::new();
    std::fs::File::from(fd)
        .take(MAX_REPORT_BYTES)
        .read_to_string(&mut text)
        .ok()?;
    Some(text)
}

/// Whether `dir` may be listed: not denied, nothing on the way a symlink,
/// a directory.
fn listable(workspace: &Workspace, dir: &WsPath) -> bool {
    workspace.denied_by(dir, Intent::Read).is_none()
        && workspace.final_target(dir).ok().as_ref() == Some(dir)
        && workspace.stat(dir).ok().flatten() == Some(EntryKind::Dir)
}

/// `name` in `dir`.
fn child_of(dir: &WsPath, name: &str) -> Option<WsPath> {
    WsPath::from_relative(&Path::new(&dir.display_string()).join(name)).ok()
}

/// `cwd` and the module folders below it, at most [`MAX_MODULE_DEPTH`]
/// down and [`MAX_MODULE_DIRS`] in all (no symlink followed, no denied or
/// hidden folder, no build output).
fn module_dirs(workspace: &Workspace, cwd: &WsPath) -> Vec<WsPath> {
    let mut found = vec![cwd.clone()];
    let mut next = 0;
    let mut depth_of = vec![0];
    while next < found.len() {
        let (dir, depth) = (found[next].clone(), depth_of[next]);
        next += 1;
        if depth >= MAX_MODULE_DEPTH || !listable(workspace, &dir) {
            continue;
        }
        let Ok(entries) = std::fs::read_dir(workspace.absolute(&dir)) else {
            continue;
        };
        let mut names: Vec<String> = entries
            .flatten()
            .filter_map(|entry| entry.file_name().into_string().ok())
            .filter(|name| !name.starts_with('.') && !NOT_MODULES.contains(&name.as_str()))
            .collect();
        names.sort();
        for name in names {
            if found.len() >= MAX_MODULE_DIRS {
                return found;
            }
            if let Some(path) = child_of(&dir, &name)
                && workspace.stat(&path).ok().flatten() == Some(EntryKind::Dir)
            {
                found.push(path);
                depth_of.push(depth + 1);
            }
        }
    }
    found
}

/// The junit files under the report directories for `kind` of `cwd` and
/// of its modules below it ([`module_dirs`]) that this run wrote (modified
/// since `started`), read through the workspace: at most
/// [`MAX_REPORT_FILES`] files of [`MAX_REPORT_TOTAL_BYTES`] in all.
fn workspace_reports(
    workspace: &Workspace,
    cwd: &WsPath,
    kind: ProjectKind,
    started: SystemTime,
) -> Vec<String> {
    let since = started
        .checked_sub(Duration::from_secs(2))
        .and_then(|at| at.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |since| {
            i128::try_from(since.as_nanos()).unwrap_or(i128::MAX)
        });
    let mut out = Vec::new();
    if kind.report_dirs().is_empty() {
        return out;
    }
    let mut pending: Vec<(WsPath, usize)> = module_dirs(workspace, cwd)
        .iter()
        .rev()
        .flat_map(|module| {
            kind.report_dirs()
                .iter()
                .filter_map(move |dir| child_of(module, dir))
        })
        .map(|dir| (dir, 0))
        .collect();
    let (mut seen, mut bytes) = (0, 0);
    while let Some((dir, depth)) = pending.pop() {
        // Listed only when nothing on the way is a symlink and it is not
        // denied; each file is then read through the workspace.
        if !listable(workspace, &dir) {
            continue;
        }
        let Ok(entries) = std::fs::read_dir(workspace.absolute(&dir)) else {
            continue;
        };
        let mut names: Vec<String> = entries
            .flatten()
            .filter_map(|entry| entry.file_name().into_string().ok())
            .collect();
        names.sort();
        for name in names {
            let Ok(path) = WsPath::from_relative(&Path::new(&dir.display_string()).join(&name))
            else {
                continue;
            };
            match workspace.stat(&path).ok().flatten() {
                Some(EntryKind::Dir) if depth < 2 => pending.push((path, depth + 1)),
                Some(EntryKind::File)
                    if Path::new(&name)
                        .extension()
                        .is_some_and(|ext| ext.eq_ignore_ascii_case("xml")) =>
                {
                    seen += 1;
                    if seen > MAX_REPORT_FILES {
                        return out;
                    }
                    if let Ok(read) = workspace.read(&path, MAX_REPORT_BYTES)
                        && read.stamp.mtime_ns >= since
                    {
                        bytes += read.bytes.len();
                        if bytes > MAX_REPORT_TOTAL_BYTES {
                            return out;
                        }
                        out.push(String::from_utf8_lossy(&read.bytes).into_owned());
                    }
                }
                _ => {}
            }
        }
    }
    out
}

/// What the run did, by its runner's output; the junit text read for it.
pub(crate) fn report(
    workspace: &Workspace,
    planned: &Planned,
    output: &CommandOutput,
    junit: &Path,
    started: SystemTime,
) -> (TestReport, Option<String>) {
    let root = workspace.absolute(&planned.cwd);
    let root = Some(root.as_path());
    let (stdout, stderr) = (output.stdout.as_str(), output.stderr.as_str());
    let junit_text = take_private_report(junit);
    let mut report = match planned.kind {
        ProjectKind::Cargo => test_report::cargo(stdout, stderr, root),
        ProjectKind::Go => test_report::go_test_json(stdout, stderr, root),
        ProjectKind::Vitest | ProjectKind::Jest | ProjectKind::Script => {
            test_report::js_json(stdout, root)
        }
        ProjectKind::Pytest => match &junit_text {
            Some(xml) => test_report::junit_xml(xml, root),
            None => test_report::pytest_text(stdout, root),
        },
        ProjectKind::Maven | ProjectKind::Gradle => {
            let mut all = TestReport::default();
            for xml in workspace_reports(workspace, &planned.cwd, planned.kind, started) {
                all.merge(test_report::junit_xml(&xml, root));
            }
            all
        }
    };
    // Files are named from the run's folder; the model reads them from
    // the workspace root.
    if !planned.cwd.is_root() {
        for failure in report.failures.iter_mut().chain(report.errors.iter_mut()) {
            if let Some(file) = failure.file.take() {
                failure.file = Some(anchored(&planned.cwd, file));
            }
        }
    }
    (report, junit_text)
}

/// `file` (relative to `cwd`) as a workspace path, when it is one.
fn anchored(cwd: &WsPath, file: String) -> String {
    if Path::new(&file).is_absolute() {
        return file;
    }
    WsPath::from_relative(&Path::new(&cwd.display_string()).join(&file))
        .map_or(file, |path| path.display_string())
}

/// The whole log: stdout, then stderr.
fn whole_log(output: &CommandOutput) -> String {
    match (output.stdout.is_empty(), output.stderr.is_empty()) {
        (_, true) => output.stdout.clone(),
        (true, false) => output.stderr.clone(),
        (false, false) => format!("[stdout]\n{}\n[stderr]\n{}", output.stdout, output.stderr),
    }
}

/// `items` that fit in `room` bytes (as JSON), at most [`MAX_INLINE`]:
/// the shown ones, and how many were left out.
fn fitting(items: &[Failure], room: &mut usize) -> (Vec<Value>, usize) {
    let mut shown = Vec::new();
    for item in items.iter().take(MAX_INLINE) {
        let value = serde_json::to_value(item).unwrap_or(Value::Null);
        let size = value.to_string().len() + 1;
        if size > *room {
            break;
        }
        *room -= size;
        shown.push(value);
    }
    let left = items.len() - shown.len();
    (shown, left)
}

/// The run's answer, within `max_bytes`.
pub(crate) fn answer(
    planned: &Planned,
    output: &CommandOutput,
    report: &TestReport,
    junit_text: Option<&str>,
    spill: &SpillStore,
    max_bytes: usize,
    network: bool,
) -> Value {
    let log = whole_log(output);
    let output_id = (!log.is_empty()).then(|| spill.spill(&log).ok()).flatten();
    let report_id = junit_text.and_then(|text| spill.spill(text).ok());
    let mut answer = Map::new();
    let mut put = |key: &str, value: Value| {
        answer.insert(key.to_owned(), value);
    };
    put("kind", json!(planned.kind.as_str()));
    put("command", json!(planned.command));
    put("cwd", json!(planned.cwd.display_string()));
    put("exit_code", json!(output.exit_code));
    put("timed_out", json!(output.timed_out));
    put("passed", json!(report.passed));
    put("failed", json!(report.failed));
    put("skipped", json!(report.skipped));
    put("duration_ms", json!(output.duration_ms));
    put("sandbox", json!(output.sandbox.as_str()));
    put("enforcement", json!(output.enforcement.as_str()));
    if let Some(note) = output.enforcement_note {
        put("enforcement_note", json!(note));
    }
    if let Some(id) = &output_id {
        put("output_id", json!(id));
    }
    if let Some(id) = &report_id {
        put("report_id", json!(id));
    }
    let succeeded = output.exit_code == Some(0) && !output.timed_out;
    let fixed = Value::Object(answer.clone()).to_string().len();
    let mut room = max_bytes.saturating_sub(fixed + RESERVE);
    let mut cut = output.stdout_truncated || output.stderr_truncated;
    if !report.errors.is_empty() {
        let (shown, left) = fitting(&report.errors, &mut room);
        answer.insert("build_failed".to_owned(), json!(true));
        answer.insert("errors".to_owned(), Value::Array(shown));
        if left > 0 {
            cut = true;
            answer.insert("more_errors".to_owned(), json!(left));
        }
    }
    if !report.failures.is_empty() {
        let (shown, kept_left) = fitting(&report.failures, &mut room);
        answer.insert("failures".to_owned(), Value::Array(shown));
        // Failures past the parser's own cap are counted too.
        let counted = usize::try_from(report.failed).unwrap_or(usize::MAX);
        let left = kept_left + counted.saturating_sub(report.failures.len());
        if left > 0 {
            cut = true;
            answer.insert("more_failures".to_owned(), json!(left));
            let paging = output_id.as_ref().map_or_else(String::new, |id| {
                format!("; read_output {{\"output_id\": \"{id}\"}} pages the whole log")
            });
            answer.insert(
                "note".to_owned(),
                json!(format!("{left} more failures are not shown{paging}")),
            );
        }
    }
    // Nothing recognised, or a failed run that named no failure: the
    // output's head and tail.
    let unexplained = !succeeded && report.failures.is_empty() && report.errors.is_empty();
    if !report.parsed || unexplained {
        if !report.parsed {
            answer.insert("parsed".to_owned(), json!(false));
        }
        let shown = room.min(MAX_TEXT);
        if !log.is_empty() && !succeeded {
            let text = budget::head_and_tail(&log, shown, output_id.as_deref());
            cut |= text.len() < log.len();
            answer.insert("output".to_owned(), json!(text));
        }
    }
    // An offline run that stopped where a dependency fetch stops: the
    // retry with the network on is the person's call.
    if !network && !succeeded && report.failures.is_empty() && test_report::needs_network(&log) {
        answer.insert("needs_network".to_owned(), json!(true));
        answer.insert(
            "hint".to_owned(),
            json!(
                "the runner could not fetch its dependencies with the network off; retry with \
                 network: true (the person is asked)"
            ),
        );
    }
    answer.insert("truncated".to_owned(), json!(cut));
    Value::Object(answer)
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::time::{Duration, SystemTime};

    use super::{
        MAX_INLINE, Planned, TestArgs, answer, plan, take_private_report, workspace_reports,
    };
    use crate::budget::{self, Budgets, SpillStore};
    use crate::policy::SandboxMode;
    use crate::project::ProjectKind;
    use crate::project::detect;
    use crate::sandbox::Enforcement;
    use crate::shell::CommandOutput;
    use crate::test_report::{Failure, TestReport};
    use crate::workspace::Workspace;
    use crate::workspace::WsPath;

    fn output(exit_code: i32, stderr: &str) -> CommandOutput {
        CommandOutput {
            exit_code: Some(exit_code),
            timed_out: false,
            stdout: String::new(),
            stderr: stderr.to_owned(),
            stdout_truncated: false,
            stderr_truncated: false,
            stdout_bytes: 0,
            stderr_bytes: stderr.len() as u64,
            output_id: None,
            duration_ms: 1,
            sandbox: SandboxMode::WorkspaceWrite,
            enforcement: Enforcement::Full,
            enforcement_note: None,
            shell: false,
        }
    }

    fn planned() -> Planned {
        Planned {
            kind: ProjectKind::Cargo,
            command: "cargo test --message-format=json".to_owned(),
            cwd: WsPath::default(),
        }
    }

    #[test]
    fn many_failures_stay_within_the_budget_and_are_counted() {
        let dir = tempfile::tempdir().expect("dir");
        let spill = SpillStore::new(&dir.path().join("data"), "s");
        let failures: Vec<Failure> = (0..60)
            .map(|n| Failure {
                name: format!("tests::t{n}"),
                file: Some("src/lib.rs".to_owned()),
                line: Some(n),
                message: "x".repeat(2000),
            })
            .collect();
        let report = TestReport {
            passed: 3,
            failed: 60,
            failures,
            parsed: true,
            ..TestReport::default()
        };
        let max = budget::bytes(Budgets::default().run_command);
        let value = answer(
            &planned(),
            &output(101, "log"),
            &report,
            None,
            &spill,
            max,
            false,
        );
        let shown = value["failures"].as_array().expect("failures").len();
        assert!((1..=MAX_INLINE).contains(&shown), "{shown}");
        assert_eq!(value["more_failures"], 60 - shown);
        assert_eq!(value["truncated"], true);
        assert!(
            value.to_string().len() <= max,
            "{}",
            value.to_string().len()
        );
        assert!(value["output_id"].is_string());
    }

    #[test]
    fn an_offline_fetch_failure_asks_for_the_network() {
        let dir = tempfile::tempdir().expect("dir");
        let spill = SpillStore::new(&dir.path().join("data"), "s");
        let log = "error: failed to download from `https://static.crates.io/x`";
        let max = budget::bytes(Budgets::default().run_command);
        let report = TestReport::default();
        let offline = answer(
            &planned(),
            &output(101, log),
            &report,
            None,
            &spill,
            max,
            false,
        );
        assert_eq!(offline["needs_network"], true, "{offline}");
        assert!(
            offline["hint"]
                .as_str()
                .is_some_and(|h| h.contains("network: true"))
        );
        let online = answer(
            &planned(),
            &output(101, log),
            &report,
            None,
            &spill,
            max,
            true,
        );
        assert!(online.get("needs_network").is_none(), "{online}");
        let passing = answer(
            &planned(),
            &output(0, ""),
            &report,
            None,
            &spill,
            max,
            false,
        );
        assert!(passing.get("needs_network").is_none());
        assert!(passing.get("output").is_none(), "{passing}");
    }

    #[test]
    fn a_report_that_is_a_link_is_never_read() {
        let dir = tempfile::tempdir().expect("dir");
        let secret = dir.path().join("secret");
        std::fs::write(&secret, "<testsuite/>").expect("secret");
        let link = dir.path().join("report.xml");
        std::os::unix::fs::symlink(&secret, &link).expect("link");
        assert_eq!(take_private_report(&link), None);
        assert!(secret.exists(), "the target is left alone");
        let plain = dir.path().join("plain.xml");
        std::fs::write(&plain, "<testsuite/>").expect("plain");
        assert_eq!(take_private_report(&plain).as_deref(), Some("<testsuite/>"));
        assert!(!plain.exists(), "removed once read");
    }

    fn tree(files: &[(&str, &str)]) -> (tempfile::TempDir, Workspace) {
        let dir = tempfile::tempdir().expect("dir");
        for (path, text) in files {
            let path = dir.path().join(path);
            std::fs::create_dir_all(path.parent().expect("parent")).expect("dirs");
            std::fs::write(path, text).expect("write");
        }
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        (dir, workspace)
    }

    fn planned_for(workspace: &Workspace, args: &TestArgs) -> Result<(String, String), String> {
        let profile = detect(workspace, &[]);
        plan(workspace, &profile, args, Path::new("/t/j.xml"))
            .map(|planned| (planned.command, planned.cwd.display_string()))
            .map_err(|error| error.message().to_owned())
    }

    fn pair(command: &str, cwd: &str) -> (String, String) {
        (command.to_owned(), cwd.to_owned())
    }

    /// A given command without `cwd` runs in the detected project's
    /// folder when exactly one runner of its kind was found, as the schema
    /// says; else at the root (#1224).
    #[test]
    fn a_given_command_runs_in_its_projects_folder() {
        let (_dir, workspace) = tree(&[
            (
                "web/package.json",
                r#"{"devDependencies":{"jest":"1"},"scripts":{"test":"jest","lint":"eslint ."}}"#,
            ),
            ("api/go.mod", "module x\n"),
            ("svc/go.mod", "module y\n"),
        ]);
        let args = |command: &str| TestArgs {
            command: Some(command.to_owned()),
            ..TestArgs::default()
        };
        assert_eq!(
            planned_for(&workspace, &args("npx jest")),
            Ok(pair("npx jest --json", "web"))
        );
        assert_eq!(
            planned_for(&workspace, &args("npm run lint")),
            Ok(pair("npm run lint", "web"))
        );
        // Two go modules: the root (the model names `cwd` to choose).
        assert_eq!(
            planned_for(&workspace, &args("go vet ./...")),
            Ok(pair("go vet ./...", "."))
        );
        // A script the folder does not declare is refused.
        assert!(planned_for(&workspace, &args("npm run typecheck")).is_err());
        let mut given = args("go test ./...");
        given.cwd = Some("svc".to_owned());
        assert_eq!(
            planned_for(&workspace, &given),
            Ok(pair("go test -json ./...", "svc"))
        );
    }

    /// Without a command, a `cwd` inside a member of the root's workspace
    /// narrows the run to that member instead of widening it to the whole
    /// workspace (#1224).
    #[test]
    fn a_member_cwd_is_not_widened_to_the_workspace() {
        let (_dir, workspace) = tree(&[
            ("Cargo.toml", "[workspace]\nmembers = [\"crates/*\"]\n"),
            ("crates/foo/Cargo.toml", "[package]\nname = \"foo\"\n"),
            ("crates/foo/src/lib.rs", ""),
        ]);
        let at = |cwd: Option<&str>| TestArgs {
            cwd: cwd.map(str::to_owned),
            filter: Some("adds".to_owned()),
            ..TestArgs::default()
        };
        assert_eq!(
            planned_for(&workspace, &at(Some("crates/foo"))),
            Ok(pair("cargo test -p foo --message-format=json adds", "."))
        );
        assert_eq!(
            planned_for(&workspace, &at(None)),
            Ok(pair("cargo test --message-format=json adds", "."))
        );
        assert_eq!(
            planned_for(&workspace, &at(Some("."))),
            Ok(pair("cargo test --message-format=json adds", "."))
        );
    }

    /// A multi-module build's reports are read from each module's report
    /// folder, a few levels down; a symlinked module is not followed
    /// (#1224).
    #[test]
    fn module_reports_are_read_below_the_run_folder() {
        let suite = |name: &str| {
            format!(
                r#"<testsuite><testcase classname="a.{name}" name="ok"/><testcase classname="a.{name}" name="bad"><failure message="no"/></testcase></testsuite>"#
            )
        };
        let (core, api, root, real) = (suite("Core"), suite("Api"), suite("Root"), suite("Real"));
        let (dir, workspace) = tree(&[
            ("pom.xml", "<project/>"),
            ("target/surefire-reports/TEST-root.xml", &root),
            ("core/target/surefire-reports/TEST-core.xml", &core),
            ("services/api/target/surefire-reports/TEST-api.xml", &api),
            // Hidden: never a module; reached only through the link below.
            (".real/target/surefire-reports/TEST-real.xml", &real),
        ]);
        std::os::unix::fs::symlink(dir.path().join(".real"), dir.path().join("module"))
            .expect("link");
        let started = SystemTime::now() - Duration::from_secs(1);
        let reports = workspace_reports(&workspace, &WsPath::root(), ProjectKind::Maven, started);
        assert_eq!(reports.len(), 3, "{reports:?}");
        for name in ["Core", "Api", "Root"] {
            assert!(
                reports
                    .iter()
                    .any(|report| report.contains(&format!("a.{name}"))),
                "{name}"
            );
        }
        assert!(
            !reports.iter().any(|report| report.contains("a.Real")),
            "{reports:?}"
        );
    }
}
