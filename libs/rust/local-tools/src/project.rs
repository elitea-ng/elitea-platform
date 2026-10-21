//! Project detection: which test runner and which build check a workspace
//! uses, from its marker files, so the agent knows them from the start of
//! a turn (the desktop's environment section) and `run_tests` can run them
//! without being told.
//!
//! [`detect`] looks at the workspace root, its directories one level down,
//! and each folder the person `@`-mentioned. Every read goes through the
//! confined [`Workspace`]: a marker that `path_deny` or the session's deny
//! list covers is never opened or even looked at, so a denied
//! `package.json` hides the JavaScript profile. Detection never runs
//! project code (no `npm run` to list scripts, no `cargo metadata`): it
//! reads at most a few marker files of at most [`MAX_MARKER_BYTES`].
//!
//! | Marker | Kind | Test command (structured output) | Check |
//! |---|---|---|---|
//! | `Cargo.toml` | `cargo` | `cargo test --message-format=json` (libtest's human output parsed beside it) | `cargo check --message-format=json` |
//! | `package.json` with `vitest` | `vitest` | `npx vitest run --reporter=json` | `npm run typecheck` when the script exists |
//! | `package.json` with `jest` | `jest` | `npx jest --json` | `npm run typecheck` when the script exists |
//! | `go.mod` | `go` | `go test -json ./...` | `go vet ./...` |
//! | `pyproject.toml`, `pytest.ini`, `setup.cfg` with `[tool:pytest]` | `pytest` | `pytest -q --junitxml=<temp>/…xml` | — |
//! | `pom.xml` | `maven` | `mvn -q test` (or `./mvnw`), then `target/surefire-reports/*.xml` | — |
//! | `build.gradle(.kts)` | `gradle` | `./gradlew test` (or `gradle`), then `build/test-results/**/*.xml` | — |
//!
//! JavaScript commands use the folder's package manager (`pnpm-lock.yaml`:
//! `pnpm exec`, `yarn.lock`: `yarn`, `bun.lock(b)`: `bunx`, else `npx`).
//!
//! A [`ProjectProfile`] remembers the stamp (kind, size, modification
//! time) of every marker it looked for, present or not, and of the folders
//! it listed: [`ProjectProfile::is_fresh`] says whether it still holds, so
//! a session detects once and again only after a marker changes.

use std::path::Path;

use serde::Serialize;
use serde_json::Value;

use crate::command::{CommandShape, analyse, basename};
use crate::error::{ToolError, ToolResult};
use crate::files;
use crate::workspace::{EntryKind, EntryStamp, Intent, Workspace, WsPath};

/// The largest marker file detection reads.
pub const MAX_MARKER_BYTES: u64 = 1024 * 1024;

/// The most directories one level below the root that detection looks in.
pub const MAX_SUBDIRS: usize = 64;

/// The most runners the preamble line names.
const MAX_LINE_RUNNERS: usize = 4;

/// The longest folder name the preamble line carries.
const MAX_LINE_DIR: usize = 64;

/// A test runner `run_tests` knows how to drive and read.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ProjectKind {
    Cargo,
    Go,
    Vitest,
    Jest,
    Pytest,
    Maven,
    Gradle,
    /// A package manager's own test script (`npm test`, `pnpm run
    /// test:unit`): whatever it runs, its output is read as JavaScript
    /// JSON when it is, else as text.
    Script,
}

impl ProjectKind {
    /// The name results, rules and remembered choices use.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Cargo => "cargo",
            Self::Go => "go",
            Self::Vitest => "vitest",
            Self::Jest => "jest",
            Self::Pytest => "pytest",
            Self::Maven => "maven",
            Self::Gradle => "gradle",
            Self::Script => "script",
        }
    }

    /// The report directories (relative to the run's working directory)
    /// whose junit XML files a run leaves behind.
    #[must_use]
    pub const fn report_dirs(self) -> &'static [&'static str] {
        match self {
            Self::Maven => &["target/surefire-reports"],
            Self::Gradle => &["build/test-results"],
            _ => &[],
        }
    }
}

/// A JavaScript folder's package manager.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum PackageManager {
    Npm,
    Pnpm,
    Yarn,
    Bun,
}

impl PackageManager {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Npm => "npm",
            Self::Pnpm => "pnpm",
            Self::Yarn => "yarn",
            Self::Bun => "bun",
        }
    }

    /// How it runs a package's binary (`npx vitest`).
    fn exec(self) -> &'static [&'static str] {
        match self {
            Self::Npm => &["npx"],
            Self::Pnpm => &["pnpm", "exec"],
            Self::Yarn => &["yarn"],
            Self::Bun => &["bunx"],
        }
    }
}

/// One runner found in one folder.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Runner {
    pub kind: ProjectKind,
    /// The folder its marker is in (the run's working directory).
    pub dir: WsPath,
    /// JavaScript: the folder's package manager.
    pub package_manager: Option<PackageManager>,
    /// Cargo: the manifest declares a `[workspace]`.
    pub cargo_workspace: bool,
    /// JavaScript: `package.json` has a `typecheck` script.
    pub typecheck_script: bool,
    /// Maven and Gradle: the folder has its wrapper script (`./mvnw`,
    /// `./gradlew`).
    pub wrapper: bool,
}

impl Runner {
    fn new(kind: ProjectKind, dir: &WsPath) -> Self {
        Self {
            kind,
            dir: dir.clone(),
            package_manager: None,
            cargo_workspace: false,
            typecheck_script: false,
            wrapper: false,
        }
    }

    fn js(&self, tool: &str) -> Vec<String> {
        let manager = self.package_manager.unwrap_or(PackageManager::Npm);
        let mut argv: Vec<String> = manager.exec().iter().map(|s| (*s).to_owned()).collect();
        argv.push(tool.to_owned());
        argv
    }

    /// The test command, with its structured output on: `junit` is where
    /// pytest writes its report.
    #[must_use]
    pub fn test_argv(&self, filter: Option<&str>, junit: Option<&Path>) -> Vec<String> {
        let words = |words: &[&str]| words.iter().map(|w| (*w).to_owned()).collect::<Vec<_>>();
        let mut argv = match self.kind {
            ProjectKind::Cargo => words(&["cargo", "test", "--message-format=json"]),
            ProjectKind::Go => words(&["go", "test", "-json", "./..."]),
            ProjectKind::Vitest => {
                let mut argv = self.js("vitest");
                argv.extend(words(&["run", "--reporter=json"]));
                argv
            }
            ProjectKind::Jest => {
                let mut argv = self.js("jest");
                argv.push("--json".to_owned());
                argv
            }
            ProjectKind::Pytest => {
                let mut argv = words(&["pytest", "-q"]);
                if let Some(junit) = junit {
                    argv.push(format!("--junitxml={}", junit.display()));
                }
                argv
            }
            ProjectKind::Maven => {
                let program = if self.wrapper { "./mvnw" } else { "mvn" };
                words(&[program, "-q", "test"])
            }
            ProjectKind::Gradle => {
                let program = if self.wrapper { "./gradlew" } else { "gradle" };
                words(&[program, "test"])
            }
            ProjectKind::Script => {
                let manager = self.package_manager.unwrap_or(PackageManager::Npm);
                words(&[manager.as_str(), "test"])
            }
        };
        if let Some(filter) = filter {
            add_filter(self.kind, &mut argv, filter);
        }
        argv
    }

    /// The build check, when the runner has one.
    #[must_use]
    pub fn check_argv(&self) -> Option<Vec<String>> {
        let words = |words: &[&str]| words.iter().map(|w| (*w).to_owned()).collect::<Vec<_>>();
        match self.kind {
            ProjectKind::Cargo => Some(words(&["cargo", "check", "--message-format=json"])),
            ProjectKind::Go => Some(words(&["go", "vet", "./..."])),
            ProjectKind::Vitest | ProjectKind::Jest | ProjectKind::Script
                if self.typecheck_script =>
            {
                let manager = self.package_manager.unwrap_or(PackageManager::Npm);
                Some(words(&[manager.as_str(), "run", "typecheck"]))
            }
            _ => None,
        }
    }

    /// What the preamble calls it (`cargo workspace`, `vitest (pnpm)`).
    fn label(&self) -> String {
        match self.kind {
            ProjectKind::Cargo if self.cargo_workspace => "cargo workspace".to_owned(),
            ProjectKind::Cargo => "cargo crate".to_owned(),
            ProjectKind::Go => "go module".to_owned(),
            ProjectKind::Vitest | ProjectKind::Jest | ProjectKind::Script => format!(
                "{} ({})",
                self.kind.as_str(),
                self.package_manager.unwrap_or(PackageManager::Npm).as_str()
            ),
            kind => kind.as_str().to_owned(),
        }
    }

    /// The test command as the preamble names it: the plain command,
    /// without the structured-output flags `run_tests` adds.
    fn test_name(&self) -> String {
        match self.kind {
            ProjectKind::Cargo => "cargo test".to_owned(),
            ProjectKind::Go => "go test ./...".to_owned(),
            ProjectKind::Vitest => format!("{} run", self.js("vitest").join(" ")),
            ProjectKind::Jest => self.js("jest").join(" "),
            ProjectKind::Pytest => "pytest".to_owned(),
            _ => self.test_argv(None, None).join(" "),
        }
    }

    fn check_name(&self) -> Option<String> {
        match self.kind {
            ProjectKind::Cargo => Some("cargo check".to_owned()),
            _ => self.check_argv().map(|argv| argv.join(" ")),
        }
    }
}

/// `filter` added to `argv` the runner's way.
fn add_filter(kind: ProjectKind, argv: &mut Vec<String>, filter: &str) {
    match kind {
        ProjectKind::Cargo => {
            // Positional, before any `--` (what follows goes to libtest).
            let at = argv.iter().position(|w| w == "--").unwrap_or(argv.len());
            argv.insert(at, filter.to_owned());
        }
        ProjectKind::Go => argv.extend(["-run".to_owned(), filter.to_owned()]),
        ProjectKind::Pytest => argv.extend(["-k".to_owned(), filter.to_owned()]),
        ProjectKind::Vitest | ProjectKind::Jest => {
            argv.extend(["-t".to_owned(), filter.to_owned()]);
        }
        ProjectKind::Maven => argv.push(format!("-Dtest={filter}")),
        ProjectKind::Gradle => argv.extend(["--tests".to_owned(), filter.to_owned()]),
        ProjectKind::Script => argv.extend(["--".to_owned(), filter.to_owned()]),
    }
}

/// What [`detect`] found, and how to tell whether it still holds.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ProjectProfile {
    /// The root's runners first, then the folders' below it, then the
    /// mentioned folders', each folder's in the table's order.
    pub runners: Vec<Runner>,
    /// The stamp of every marker looked for (absent: `None`) and of every
    /// folder listed.
    stamps: Vec<(WsPath, Option<EntryStamp>)>,
}

impl ProjectProfile {
    /// Whether nothing detection looked at has changed since.
    #[must_use]
    pub fn is_fresh(&self, workspace: &Workspace) -> bool {
        self.stamps
            .iter()
            .all(|(path, stamp)| workspace.stamp(path).ok().flatten() == *stamp)
    }

    /// The runner a run in `cwd` uses: the nearest folder at or above
    /// `cwd` with one (its first), else, at the root, the first runner
    /// found anywhere (a repository whose code sits one level down).
    #[must_use]
    pub fn runner_for(&self, cwd: &WsPath) -> Option<&Runner> {
        let components = cwd.components();
        (0..=components.len())
            .rev()
            .find_map(|end| {
                self.runners
                    .iter()
                    .find(|runner| runner.dir.components() == &components[..end])
            })
            .or_else(|| self.runners.first().filter(|_| cwd.is_root()))
    }

    /// The preamble's one line: `Project: cargo workspace; tests: cargo
    /// test; check: cargo check` (more runners after ` | `). Fixed words
    /// and folder names that are plainly names only (see
    /// [`plain_dir`]), never a file's contents. `None` when nothing was
    /// found.
    #[must_use]
    pub fn preamble_line(&self) -> Option<String> {
        if self.runners.is_empty() {
            return None;
        }
        let entries: Vec<String> = self
            .runners
            .iter()
            .take(MAX_LINE_RUNNERS)
            .map(|runner| {
                let mut entry = runner.label();
                if !runner.dir.is_root() {
                    entry.push_str(" in ");
                    entry.push_str(&plain_dir(&runner.dir));
                }
                entry.push_str("; tests: ");
                entry.push_str(&runner.test_name());
                if let Some(check) = runner.check_name() {
                    entry.push_str("; check: ");
                    entry.push_str(&check);
                }
                entry
            })
            .collect();
        let more = self.runners.len().saturating_sub(MAX_LINE_RUNNERS);
        let rest = if more > 0 {
            format!(" | and {more} more")
        } else {
            String::new()
        };
        Some(format!("Project: {}{rest}", entries.join(" | ")))
    }
}

/// A folder's path as the preamble shows it: the path when every
/// component is plainly a name (ASCII letters, digits, `.`, `_`, `-`; not
/// starting with `-` or `.`; at most `MAX_LINE_DIR` bytes in all), else
/// `a subfolder` (withheld, never cleaned into something else).
#[must_use]
pub fn plain_dir(dir: &WsPath) -> String {
    let text = dir.display_string();
    let plain = text.len() <= MAX_LINE_DIR
        && dir.components().iter().all(|name| {
            !name.is_empty()
                && !name.starts_with(['-', '.'])
                && name
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || b"._-".contains(&byte))
        });
    if plain {
        text
    } else {
        "a subfolder".to_owned()
    }
}

fn child(dir: &WsPath, name: &str) -> Option<WsPath> {
    let mut parts: Vec<&str> = dir.components().iter().map(String::as_str).collect();
    parts.push(name);
    WsPath::from_relative(Path::new(&parts.join("/"))).ok()
}

/// One detection pass: the stamps it takes along the way.
struct Scan<'a> {
    workspace: &'a Workspace,
    stamps: Vec<(WsPath, Option<EntryStamp>)>,
}

impl Scan<'_> {
    /// The stamp of `path`, recorded; `None` (and not recorded) when the
    /// deny rules cover it: a denied path is never looked at.
    fn look(&mut self, path: &WsPath) -> Option<EntryStamp> {
        if self.workspace.denied_by(path, Intent::Read).is_some() {
            return None;
        }
        let stamp = self.workspace.stamp(path).ok().flatten();
        self.stamps.push((path.clone(), stamp));
        stamp
    }

    /// The text of `name` in `dir` when it is a regular file (not a
    /// symlink) small enough to read.
    fn text(&mut self, dir: &WsPath, name: &str) -> Option<String> {
        let path = child(dir, name)?;
        let stamp = self.look(&path)?;
        if stamp.kind != EntryKind::File || stamp.len > MAX_MARKER_BYTES {
            return None;
        }
        let read = self.workspace.read(&path, MAX_MARKER_BYTES).ok()?;
        Some(String::from_utf8_lossy(&read.bytes).into_owned())
    }

    /// Whether `name` in `dir` is a regular file.
    fn has(&mut self, dir: &WsPath, name: &str) -> bool {
        child(dir, name)
            .and_then(|path| self.look(&path))
            .is_some_and(|stamp| stamp.kind == EntryKind::File)
    }

    /// The runners of one folder.
    fn folder(&mut self, dir: &WsPath) -> Vec<Runner> {
        let mut found = Vec::new();
        if let Some(manifest) = self.text(dir, "Cargo.toml") {
            let mut runner = Runner::new(ProjectKind::Cargo, dir);
            runner.cargo_workspace = manifest
                .lines()
                .any(|line| line.trim_start().starts_with("[workspace"));
            found.push(runner);
        }
        if self.has(dir, "go.mod") {
            found.push(Runner::new(ProjectKind::Go, dir));
        }
        if let Some(manifest) = self.text(dir, "package.json")
            && let Some(runner) = self.javascript(dir, &manifest)
        {
            found.push(runner);
        }
        let pytest = self.has(dir, "pyproject.toml")
            || self.has(dir, "pytest.ini")
            || self
                .text(dir, "setup.cfg")
                .is_some_and(|text| text.contains("[tool:pytest]"));
        if pytest {
            found.push(Runner::new(ProjectKind::Pytest, dir));
        }
        if self.has(dir, "pom.xml") {
            let mut runner = Runner::new(ProjectKind::Maven, dir);
            runner.wrapper = self.has(dir, "mvnw");
            found.push(runner);
        }
        if self.has(dir, "build.gradle") || self.has(dir, "build.gradle.kts") {
            let mut runner = Runner::new(ProjectKind::Gradle, dir);
            runner.wrapper = self.has(dir, "gradlew");
            found.push(runner);
        }
        found
    }

    /// A `package.json`'s runner: vitest or jest among its dependencies or
    /// in its `test` script; the package manager from the lock file.
    fn javascript(&mut self, dir: &WsPath, manifest: &str) -> Option<Runner> {
        let manifest: Value = serde_json::from_str(manifest).ok()?;
        let has_dependency = |name: &str| {
            ["dependencies", "devDependencies"]
                .iter()
                .any(|key| manifest.get(key).and_then(|deps| deps.get(name)).is_some())
        };
        let scripts = manifest.get("scripts");
        let test_script = scripts
            .and_then(|scripts| scripts.get("test"))
            .and_then(Value::as_str)
            .unwrap_or_default();
        let kind = if has_dependency("vitest") || test_script.contains("vitest") {
            ProjectKind::Vitest
        } else if has_dependency("jest") || test_script.contains("jest") {
            ProjectKind::Jest
        } else {
            return None;
        };
        let mut runner = Runner::new(kind, dir);
        runner.typecheck_script = scripts
            .and_then(|scripts| scripts.get("typecheck"))
            .is_some_and(Value::is_string);
        runner.package_manager = Some(self.package_manager(dir));
        Some(runner)
    }

    fn package_manager(&mut self, dir: &WsPath) -> PackageManager {
        let mut dirs = vec![dir.clone()];
        if !dir.is_root() {
            dirs.push(WsPath::root());
        }
        for dir in dirs {
            if self.has(&dir, "pnpm-lock.yaml") {
                return PackageManager::Pnpm;
            }
            if self.has(&dir, "yarn.lock") {
                return PackageManager::Yarn;
            }
            if self.has(&dir, "bun.lockb") || self.has(&dir, "bun.lock") {
                return PackageManager::Bun;
            }
        }
        PackageManager::Npm
    }

    /// The folders one level below the root, as the listing tools see them
    /// (`.gitignore` honoured, denied ones pruned), hidden ones left out.
    fn subdirs(&mut self) -> Vec<WsPath> {
        self.look(&WsPath::root());
        let Ok(walk) = files::walker(self.workspace, &WsPath::root(), None, Some(1)) else {
            return Vec::new();
        };
        walk.flatten()
            .filter(|entry| entry.depth() == 1)
            .filter(|entry| entry.file_type().is_some_and(|kind| kind.is_dir()))
            .filter_map(|entry| files::relative(self.workspace, entry.path()))
            .filter(|path| !path.file_name().is_some_and(|name| name.starts_with('.')))
            .take(MAX_SUBDIRS)
            .collect()
    }
}

/// Detect the workspace's runners: the root, the folders one level below
/// it, and `mentioned` (a mentioned file stands for its folder).
#[must_use]
pub fn detect(workspace: &Workspace, mentioned: &[WsPath]) -> ProjectProfile {
    let mut scan = Scan {
        workspace,
        stamps: Vec::new(),
    };
    let mut runners = scan.folder(&WsPath::root());
    let mut dirs = scan.subdirs();
    for mention in mentioned {
        if workspace.denied_by(mention, Intent::Read).is_some() {
            continue;
        }
        let dir = match scan.look(mention) {
            Some(stamp) if stamp.kind == EntryKind::Dir => mention.clone(),
            Some(_) => {
                let components = mention.components();
                let parent = &components[..components.len().saturating_sub(1)];
                WsPath::from_relative(Path::new(&parent.join("/"))).unwrap_or_default()
            }
            None => continue,
        };
        if !dir.is_root() {
            dirs.push(dir);
        }
    }
    let mut seen = vec![WsPath::root()];
    for dir in dirs {
        if seen.contains(&dir) {
            continue;
        }
        seen.push(dir.clone());
        for runner in scan.folder(&dir) {
            // The root's runner of a kind covers its members (a cargo
            // workspace's crates, a monorepo's packages).
            let covered = runners
                .iter()
                .any(|found| found.kind == runner.kind && found.dir.is_root());
            if !covered {
                runners.push(runner);
            }
        }
    }
    ProjectProfile {
        runners,
        stamps: scan.stamps,
    }
}

/// A test run `run_tests` will start: the runner, the argv, where the
/// junit report goes (pytest).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TestCommand {
    pub kind: ProjectKind,
    pub argv: Vec<String>,
}

fn has_flag(argv: &[String], flag: &str) -> bool {
    argv.iter()
        .any(|word| word == flag || word.starts_with(&format!("{flag}=")))
}

/// Read a command the model gave as a test runner invocation: the runner
/// it is, with its structured output switched on when the command did not
/// ask for another (`--message-format=json` after `cargo test`, `-json`
/// after `go test`, `--reporter=json` for vitest, `--json` for jest,
/// `--junitxml=<junit>` for pytest), and `filter` added the runner's way.
///
/// # Errors
///
/// [`crate::error::ErrorCode::InvalidArgument`] when it is not a plain
/// (simple) command of a runner `run_tests` knows: anything else belongs in
/// `run_command`.
pub fn given_command(
    command: &str,
    filter: Option<&str>,
    junit: Option<&Path>,
) -> ToolResult<TestCommand> {
    let refuse = || {
        ToolError::invalid(
            "`command` must be one plain test runner invocation (cargo test/check/build/clippy, \
             go test/vet/build, vitest, jest, pytest, mvn, gradle, or a package manager's test \
             script); use run_command for anything else",
        )
    };
    let CommandShape::Simple(mut argv) = analyse(command) else {
        return Err(refuse());
    };
    let program = basename(argv.first().ok_or_else(refuse)?).to_owned();
    let at = |argv: &[String], index: usize| argv.get(index).cloned().unwrap_or_default();
    let kind = match program.as_str() {
        "cargo" => {
            let mut sub = 1;
            if at(&argv, sub).starts_with('+') {
                sub += 1;
            }
            match at(&argv, sub).as_str() {
                "test" | "t" | "check" | "c" | "build" | "b" | "clippy" => {
                    if !has_flag(&argv, "--message-format") {
                        let end = argv.iter().position(|w| w == "--").unwrap_or(argv.len());
                        argv.insert((sub + 1).min(end), "--message-format=json".to_owned());
                    }
                }
                "nextest" => {}
                _ => return Err(refuse()),
            }
            ProjectKind::Cargo
        }
        "go" => match at(&argv, 1).as_str() {
            "test" => {
                if !has_flag(&argv, "-json") && !has_flag(&argv, "--json") {
                    argv.insert(2, "-json".to_owned());
                }
                ProjectKind::Go
            }
            "vet" | "build" => ProjectKind::Go,
            _ => return Err(refuse()),
        },
        "pytest" | "py.test" => ProjectKind::Pytest,
        "python" | "python3" if at(&argv, 1) == "-m" && at(&argv, 2) == "pytest" => {
            ProjectKind::Pytest
        }
        "mvn" | "mvnw" => {
            let goals = ["test", "verify", "compile", "test-compile"];
            if !argv[1..].iter().any(|word| goals.contains(&word.as_str())) {
                return Err(refuse());
            }
            ProjectKind::Maven
        }
        "gradle" | "gradlew" => {
            let task = |word: &String| {
                matches!(word.as_str(), "test" | "check" | "build")
                    || word.ends_with(":test")
                    || word.ends_with(":check")
                    || word.starts_with("compile")
            };
            if !argv[1..].iter().any(task) {
                return Err(refuse());
            }
            ProjectKind::Gradle
        }
        _ => javascript_kind(&mut argv, &program).ok_or_else(refuse)?,
    };
    if kind == ProjectKind::Pytest
        && !has_flag(&argv, "--junitxml")
        && let Some(junit) = junit
    {
        argv.push(format!("--junitxml={}", junit.display()));
    }
    if let Some(filter) = filter {
        add_filter(kind, &mut argv, filter);
    }
    Ok(TestCommand { kind, argv })
}

/// A JavaScript runner invocation (`npx vitest`, `pnpm exec jest`, `yarn
/// vitest`, `vitest` itself, `npm test`, `pnpm run test:unit`), its JSON
/// reporter switched on; `None` for anything else.
fn javascript_kind(argv: &mut Vec<String>, program: &str) -> Option<ProjectKind> {
    let at = |argv: &[String], index: usize| argv.get(index).cloned().unwrap_or_default();
    let tool_index = match program {
        "vitest" | "jest" => 0,
        "npx" | "bunx" => 1,
        "npm" if matches!(at(argv, 1).as_str(), "exec" | "x") => 2,
        "pnpm" | "yarn" | "bun" if matches!(at(argv, 1).as_str(), "exec" | "dlx" | "x") => 2,
        "pnpm" | "yarn" | "bun" if matches!(at(argv, 1).as_str(), "vitest" | "jest") => 1,
        "npm" | "pnpm" | "yarn" | "bun" => {
            let script = match at(argv, 1).as_str() {
                "test" | "t" => return Some(ProjectKind::Script),
                "run" | "run-script" => at(argv, 2),
                // pnpm, yarn and bun run a script by its bare name; npm
                // does not.
                other if program != "npm" => other.to_owned(),
                _ => return None,
            };
            let routine = ["test", "check", "typecheck", "lint"];
            return routine
                .iter()
                .any(|name| script.starts_with(name))
                .then_some(ProjectKind::Script);
        }
        _ => return None,
    };
    match basename(&at(argv, tool_index)) {
        "vitest" => {
            let next = at(argv, tool_index + 1);
            if !matches!(next.as_str(), "run" | "related" | "list") {
                argv.insert(tool_index + 1, "run".to_owned());
            }
            if !has_flag(argv, "--reporter") {
                argv.push("--reporter=json".to_owned());
            }
            Some(ProjectKind::Vitest)
        }
        "jest" => {
            if !has_flag(argv, "--json") {
                argv.push("--json".to_owned());
            }
            Some(ProjectKind::Jest)
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};

    use super::{ProjectKind, detect, given_command, plain_dir};
    use crate::workspace::{Workspace, WsPath};

    fn fixtures() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/projects")
    }

    fn kinds(root: &Path, deny: &[&str]) -> Vec<(ProjectKind, String)> {
        let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
        let workspace = Workspace::open(root, &deny).expect("workspace");
        detect(&workspace, &[])
            .runners
            .iter()
            .map(|runner| (runner.kind, runner.dir.display_string()))
            .collect()
    }

    #[test]
    fn detects_cargo_go_vitest_jest_pytest_maven_gradle() {
        let root = fixtures();
        for (name, kind) in [
            ("cargo", ProjectKind::Cargo),
            ("go", ProjectKind::Go),
            ("vitest", ProjectKind::Vitest),
            ("jest", ProjectKind::Jest),
            ("pytest", ProjectKind::Pytest),
            ("maven", ProjectKind::Maven),
            ("gradle", ProjectKind::Gradle),
        ] {
            assert_eq!(
                kinds(&root.join(name), &[]),
                [(kind, ".".to_owned())],
                "{name}"
            );
        }
        // From the folder above, each is found one level down, and named
        // there in the preamble line.
        let all = kinds(&root, &[]);
        assert_eq!(all.len(), 7, "{all:?}");
        let workspace = Workspace::open(&root, &[]).expect("workspace");
        let profile = detect(&workspace, &[]);
        let line = profile.preamble_line().expect("line");
        assert!(
            line.starts_with("Project: cargo workspace in cargo; tests: cargo test; check: cargo check | go module in go; tests: go test ./...; check: go vet ./..."),
            "{line}"
        );
        assert!(line.ends_with("| and 3 more"), "{line}");

        let vitest = Workspace::open(&root.join("vitest"), &[]).expect("workspace");
        let profile = detect(&vitest, &[]);
        let runner = &profile.runners[0];
        assert_eq!(
            runner.test_argv(Some("adds"), None),
            [
                "pnpm",
                "exec",
                "vitest",
                "run",
                "--reporter=json",
                "-t",
                "adds"
            ]
        );
        assert_eq!(
            profile.preamble_line().as_deref(),
            Some("Project: vitest (pnpm); tests: pnpm exec vitest run; check: pnpm run typecheck")
        );
        let jest = Workspace::open(&root.join("jest"), &[]).expect("workspace");
        assert_eq!(
            detect(&jest, &[]).runners[0].test_argv(None, None),
            ["npx", "jest", "--json"]
        );
        let gradle = Workspace::open(&root.join("gradle"), &[]).expect("workspace");
        assert_eq!(
            detect(&gradle, &[]).runners[0].test_argv(Some("FooTest"), None),
            ["./gradlew", "test", "--tests", "FooTest"]
        );
        let pytest = Workspace::open(&root.join("pytest"), &[]).expect("workspace");
        assert_eq!(
            detect(&pytest, &[]).runners[0].test_argv(None, Some(Path::new("/t/j.xml"))),
            ["pytest", "-q", "--junitxml=/t/j.xml"]
        );
    }

    /// A `path_deny` on `package.json` hides the JavaScript profile: the
    /// marker is never opened (regression for confinement).
    #[test]
    fn detection_never_reads_denied_paths() {
        let root = fixtures();
        assert_eq!(
            kinds(&root.join("vitest"), &["package.json"]),
            [],
            "a denied marker is not read"
        );
        assert_eq!(kinds(&root.join("pytest"), &["*.toml"]), []);
        // A denied folder below the root is not looked into at all.
        let all = kinds(&root, &["jest/**", "go"]);
        assert!(!all.iter().any(|(kind, _)| *kind == ProjectKind::Jest));
        assert!(!all.iter().any(|(kind, _)| *kind == ProjectKind::Go));
        let workspace =
            Workspace::open(&root.join("vitest"), &["package.json".to_owned()]).expect("workspace");
        let profile = detect(&workspace, &[]);
        assert!(
            !profile
                .stamps
                .iter()
                .any(|(path, _)| path.display_string() == "package.json"),
            "not even stat'ed"
        );
    }

    #[test]
    fn a_profile_goes_stale_when_a_marker_changes() {
        let dir = tempfile::tempdir().expect("dir");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let empty = detect(&workspace, &[]);
        assert!(empty.runners.is_empty());
        assert!(empty.preamble_line().is_none());
        assert!(empty.is_fresh(&workspace));
        std::fs::write(dir.path().join("go.mod"), "module x\n").expect("go.mod");
        assert!(!empty.is_fresh(&workspace), "a marker appeared");
        let go = detect(&workspace, &[]);
        assert!(go.is_fresh(&workspace));
        std::fs::create_dir(dir.path().join("web")).expect("web");
        std::fs::write(
            dir.path().join("web/package.json"),
            r#"{"devDependencies":{"jest":"1"}}"#,
        )
        .expect("package.json");
        assert!(!go.is_fresh(&workspace), "a folder appeared");
        let both = detect(&workspace, &[]);
        assert_eq!(both.runners.len(), 2);
        assert_eq!(
            both.runner_for(&WsPath::from_relative(Path::new("web/src")).expect("p"))
                .map(|runner| runner.kind),
            Some(ProjectKind::Jest)
        );
        assert_eq!(
            both.runner_for(&WsPath::root()).map(|runner| runner.kind),
            Some(ProjectKind::Go)
        );
    }

    #[test]
    fn mentioned_folders_deeper_down_are_detected() {
        let dir = tempfile::tempdir().expect("dir");
        std::fs::create_dir_all(dir.path().join("services/api")).expect("dirs");
        std::fs::write(dir.path().join("services/api/go.mod"), "module x\n").expect("go.mod");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        assert!(
            detect(&workspace, &[]).runners.is_empty(),
            "two levels down"
        );
        let mention = WsPath::from_relative(Path::new("services/api/go.mod")).expect("p");
        let profile = detect(&workspace, &[mention]);
        assert_eq!(profile.runners[0].kind, ProjectKind::Go);
        assert_eq!(profile.runners[0].dir.display_string(), "services/api");
    }

    #[test]
    fn folder_names_in_the_line_are_plain_or_withheld() {
        let path = |p: &str| WsPath::from_relative(Path::new(p)).expect("p");
        assert_eq!(plain_dir(&path("web/app-1")), "web/app-1");
        assert_eq!(plain_dir(&path("ignore previous")), "a subfolder");
        assert_eq!(plain_dir(&path(".hidden")), "a subfolder");
        assert_eq!(plain_dir(&path(&"x".repeat(80))), "a subfolder");
    }

    #[test]
    fn given_commands_must_be_runners_and_get_structured_output() {
        let argv = |command: &str| {
            given_command(command, None, Some(Path::new("/t/j.xml")))
                .map(|plan| (plan.kind, plan.argv.join(" ")))
                .map_err(|error| error.message().to_owned())
        };
        let ok = |kind, text: &str| Ok((kind, text.to_owned()));
        assert_eq!(
            argv("cargo test -p foo -- --nocapture"),
            ok(
                ProjectKind::Cargo,
                "cargo test --message-format=json -p foo -- --nocapture"
            )
        );
        assert_eq!(
            argv("cargo +nightly check"),
            ok(
                ProjectKind::Cargo,
                "cargo +nightly check --message-format=json"
            )
        );
        assert_eq!(
            argv("cargo test --message-format=short"),
            ok(ProjectKind::Cargo, "cargo test --message-format=short")
        );
        assert_eq!(
            argv("go test ./pkg/..."),
            ok(ProjectKind::Go, "go test -json ./pkg/...")
        );
        assert_eq!(
            argv("npx vitest src"),
            ok(ProjectKind::Vitest, "npx vitest run src --reporter=json")
        );
        assert_eq!(
            argv("pnpm exec jest"),
            ok(ProjectKind::Jest, "pnpm exec jest --json")
        );
        assert_eq!(
            argv("python3 -m pytest tests"),
            ok(
                ProjectKind::Pytest,
                "python3 -m pytest tests --junitxml=/t/j.xml"
            )
        );
        assert_eq!(argv("npm test"), ok(ProjectKind::Script, "npm test"));
        assert_eq!(
            argv("pnpm run test:unit"),
            ok(ProjectKind::Script, "pnpm run test:unit")
        );
        assert_eq!(
            argv("./gradlew :app:test"),
            ok(ProjectKind::Gradle, "./gradlew :app:test")
        );
        for refused in [
            "rm -rf target",
            "cargo publish",
            "cargo test && rm -rf /",
            "npm install",
            "npm run deploy",
            "go run .",
            "mvn deploy",
            "sh -c 'cargo test'",
            "",
        ] {
            assert!(argv(refused).is_err(), "{refused}");
        }
        let filtered = given_command("cargo test -- --nocapture", Some("adds"), None)
            .expect("plan")
            .argv;
        assert_eq!(
            filtered,
            [
                "cargo",
                "test",
                "--message-format=json",
                "adds",
                "--",
                "--nocapture"
            ]
        );
    }
}
