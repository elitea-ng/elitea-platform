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
//! | `package.json` with another `test` script (mocha, ava, `node --test`) | `script` | `npm test` (its output read as text) | `npm run typecheck` when the script exists |
//! | `go.mod` | `go` | `go test -json ./...` | `go vet ./...` |
//! | `pyproject.toml`, `pytest.ini`, `setup.cfg` with `[tool:pytest]` | `pytest` | `pytest -q --junitxml=<temp>/…xml` | — |
//! | `pom.xml` | `maven` | `mvn -q test` (or `./mvnw`), then `target/surefire-reports/*.xml` (the modules' too) | — |
//! | `build.gradle(.kts)` | `gradle` | `./gradlew test` (or `gradle`), then `build/test-results/**/*.xml` (the modules' too) | — |
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
        ProjectKind::Gradle => {
            // A task option: after the (last) test task it selects for.
            let at = argv
                .iter()
                .rposition(|word| is_gradle_test_task(word))
                .map_or(argv.len(), |index| index + 1);
            argv.splice(at..at, ["--tests".to_owned(), filter.to_owned()]);
        }
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
    /// in its `test` script, else its own `test` script (not npm's
    /// placeholder); the package manager from the lock file.
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
        } else if !test_script.trim().is_empty() && !test_script.contains("no test specified") {
            // Another runner (mocha, ava, `node --test`, …): the package's
            // own `test` script, its output read as text.
            ProjectKind::Script
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

/// A test run `run_tests` will start: the runner, the argv, and the
/// package manager's script it runs.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TestCommand {
    pub kind: ProjectKind,
    pub argv: Vec<String>,
    /// The `package.json` script a package manager runs (`test`,
    /// `test:unit`): the caller checks that the run's folder declares it
    /// ([`check_package_script`]). `None` for every other command.
    pub script: Option<String>,
}

fn has_flag(argv: &[String], flag: &str) -> bool {
    argv.iter()
        .any(|word| word == flag || word.starts_with(&format!("{flag}=")))
}

/// Maven phases and goals a given command may run: building and testing,
/// never installing, deploying, releasing or publishing a site.
const MAVEN_GOALS: &[&str] = &[
    "clean",
    "validate",
    "compile",
    "test-compile",
    "test",
    "verify",
    "surefire:test",
];

/// The Maven goals that run tests (a filter applies to them).
const MAVEN_TEST_GOALS: &[&str] = &["test", "verify", "surefire:test"];

/// Maven flags taken alone.
const MAVEN_FLAGS: &[&str] = &[
    "-q",
    "--quiet",
    "-e",
    "--errors",
    "-X",
    "--debug",
    "-B",
    "--batch-mode",
    "-o",
    "--offline",
    "-U",
    "--update-snapshots",
    "-fae",
    "--fail-at-end",
    "-ff",
    "--fail-fast",
    "-am",
    "--also-make",
    "-amd",
    "--also-make-dependents",
    "-ntp",
    "--no-transfer-progress",
    "-N",
    "--non-recursive",
    "-V",
    "--show-version",
];

/// Maven flags that take a value (the next word, or `--flag=value`).
const MAVEN_VALUED: &[&str] = &[
    "-pl",
    "--projects",
    "-P",
    "--activate-profiles",
    "-T",
    "--threads",
    "-rf",
    "--resume-from",
    "-D",
    "--define",
];

/// Gradle tasks a given command may run (alone or as `:module:task`):
/// building and testing, never publishing, uploading or releasing.
const GRADLE_TASKS: &[&str] = &[
    "clean",
    "test",
    "check",
    "build",
    "assemble",
    "classes",
    "testClasses",
    "compileJava",
    "compileTestJava",
    "compileKotlin",
    "compileTestKotlin",
    "compileGroovy",
    "compileTestGroovy",
    "compileScala",
    "compileTestScala",
];

/// Gradle flags taken alone.
const GRADLE_FLAGS: &[&str] = &[
    "-q",
    "--quiet",
    "-i",
    "--info",
    "-w",
    "--warn",
    "-d",
    "--debug",
    "-s",
    "--stacktrace",
    "-S",
    "--full-stacktrace",
    "--offline",
    "--continue",
    "--rerun-tasks",
    "--rerun",
    "--no-daemon",
    "--daemon",
    "--parallel",
    "--no-parallel",
    "--build-cache",
    "--no-build-cache",
    "--configuration-cache",
    "--no-configuration-cache",
];

/// Gradle flags that take a value (the next word, or `--flag=value`).
const GRADLE_VALUED: &[&str] = &[
    "-x",
    "--exclude-task",
    "-p",
    "--project-dir",
    "--tests",
    "--console",
    "--warning-mode",
    "--max-workers",
];

/// Whether `word` is a Gradle task path that runs tests (`test`,
/// `:app:test`).
fn is_gradle_test_task(word: &str) -> bool {
    word == "test" || word.ends_with(":test")
}

/// The goals of a Maven command's arguments when every goal is one
/// [`MAVEN_GOALS`] names and every flag is known; `None` otherwise.
fn maven_goals(args: &[String]) -> Option<Vec<&str>> {
    let mut goals = Vec::new();
    let mut words = args.iter();
    while let Some(word) = words.next() {
        let word = word.as_str();
        if !word.starts_with('-') {
            if !MAVEN_GOALS.contains(&word) {
                return None;
            }
            goals.push(word);
        } else if MAVEN_FLAGS.contains(&word) {
        } else if MAVEN_VALUED.contains(&word) {
            words.next()?;
        } else if word.len() > 2 && (word.starts_with("-D") || word.starts_with("-P")) {
            // `-Dkey=value`, `-Pprofile`.
        } else if !word
            .split_once('=')
            .is_some_and(|(flag, _)| flag.starts_with("--") && MAVEN_VALUED.contains(&flag))
        {
            return None;
        }
    }
    (!goals.is_empty()).then_some(goals)
}

/// The tasks of a Gradle command's arguments when every task is one
/// [`GRADLE_TASKS`] names (alone or under a module path) and every flag is
/// known; `None` otherwise.
fn gradle_tasks(args: &[String]) -> Option<Vec<&str>> {
    let task_ok = |word: &str| {
        let (path, name) = word.rsplit_once(':').unwrap_or(("", word));
        GRADLE_TASKS.contains(&name)
            && path
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b":._-".contains(&byte))
    };
    let project_dir_ok = |dir: &str| {
        !dir.is_empty() && !Path::new(dir).is_absolute() && !dir.split('/').any(|part| part == "..")
    };
    let mut tasks = Vec::new();
    let mut words = args.iter();
    while let Some(word) = words.next() {
        let word = word.as_str();
        if !word.starts_with('-') {
            if !task_ok(word) {
                return None;
            }
            tasks.push(word);
            continue;
        }
        let (flag, attached) = match word.split_once('=') {
            Some((flag, value)) if flag.starts_with("--") => (flag, Some(value)),
            _ => (word, None),
        };
        if attached.is_none() && GRADLE_FLAGS.contains(&flag) {
            continue;
        }
        if GRADLE_VALUED.contains(&flag) {
            let value = match attached {
                Some(value) => value,
                None => words.next()?.as_str(),
            };
            if matches!(flag, "-p" | "--project-dir") && !project_dir_ok(value) {
                return None;
            }
            continue;
        }
        if word.len() > 2 && (word.starts_with("-D") || word.starts_with("-P")) {
            // `-Dkey=value`, `-Pkey=value`.
            continue;
        }
        return None;
    }
    (!tasks.is_empty()).then_some(tasks)
}

/// Read a command the model gave as a test runner invocation: the runner
/// it is, with its structured output switched on when the command did not
/// ask for another (`--message-format=json` after `cargo test`, `-json`
/// after `go test`, `--reporter=json` for vitest, `--json` for jest,
/// `--junitxml=<junit>` for pytest), and `filter` added the runner's way.
///
/// Maven and Gradle commands run only the goals and tasks
/// `MAVEN_GOALS` and `GRADLE_TASKS` name (every one of them: `mvn
/// test deploy` is refused); a package manager runs only the scripts
/// `test`, `test:<name>`, `check`, `typecheck` and `lint` (which the
/// caller checks the folder declares, [`TestCommand::script`]).
///
/// # Errors
///
/// [`crate::error::ErrorCode::InvalidArgument`] when it is not a plain
/// (simple) command of a runner `run_tests` knows: anything else belongs in
/// `run_command`; and when `filter` is given for a command that runs no
/// tests (a build check, `go vet`, `mvn compile`, `npm run lint`).
pub fn given_command(
    command: &str,
    filter: Option<&str>,
    junit: Option<&Path>,
) -> ToolResult<TestCommand> {
    let refuse = || {
        ToolError::invalid(
            "`command` must be one plain test runner invocation (cargo test/check/build/clippy, \
             go test/vet/build, vitest, jest, pytest, mvn or gradle building and testing only, \
             or a package manager's test, check, typecheck or lint script); use run_command for \
             anything else",
        )
    };
    let CommandShape::Simple(mut argv) = analyse(command) else {
        return Err(refuse());
    };
    let program = basename(argv.first().ok_or_else(refuse)?).to_owned();
    let at = |argv: &[String], index: usize| argv.get(index).cloned().unwrap_or_default();
    // Whether it runs tests, so that a test filter applies.
    let mut runs_tests = true;
    let mut script = None;
    let kind = match program.as_str() {
        "cargo" => {
            let mut sub = 1;
            if at(&argv, sub).starts_with('+') {
                sub += 1;
            }
            match at(&argv, sub).as_str() {
                word @ ("test" | "t" | "check" | "c" | "build" | "b" | "clippy") => {
                    runs_tests = matches!(word, "test" | "t");
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
            "vet" | "build" => {
                runs_tests = false;
                ProjectKind::Go
            }
            _ => return Err(refuse()),
        },
        "pytest" | "py.test" => ProjectKind::Pytest,
        "python" | "python3" if at(&argv, 1) == "-m" && at(&argv, 2) == "pytest" => {
            ProjectKind::Pytest
        }
        "mvn" | "mvnw" => {
            let goals = maven_goals(&argv[1..]).ok_or_else(refuse)?;
            runs_tests = goals.iter().any(|goal| MAVEN_TEST_GOALS.contains(goal));
            ProjectKind::Maven
        }
        "gradle" | "gradlew" => {
            let tasks = gradle_tasks(&argv[1..]).ok_or_else(refuse)?;
            runs_tests = tasks.iter().any(|task| is_gradle_test_task(task));
            ProjectKind::Gradle
        }
        _ => {
            let (kind, named) = javascript_kind(&mut argv, &program).ok_or_else(refuse)?;
            if kind == ProjectKind::Script {
                // `bun test` (no script) is bun's own test runner.
                runs_tests = named
                    .as_deref()
                    .is_none_or(|name| name == "test" || name.starts_with("test:"));
            }
            script = named;
            kind
        }
    };
    if kind == ProjectKind::Pytest
        && !has_flag(&argv, "--junitxml")
        && let Some(junit) = junit
    {
        argv.push(format!("--junitxml={}", junit.display()));
    }
    if let Some(filter) = filter {
        if !runs_tests {
            return Err(ToolError::invalid(
                "`filter` selects tests, and this command runs none (a build check, `go vet`, \
                 a compile or lint): leave `filter` out, or run the test command",
            ));
        }
        add_filter(kind, &mut argv, filter);
    }
    Ok(TestCommand { kind, argv, script })
}

/// Whether a package manager's script `name` is one a given command may
/// run: `test`, `test:<name>`, `check`, `typecheck`, `lint`.
fn routine_script(name: &str) -> bool {
    matches!(name, "test" | "check" | "typecheck" | "lint")
        || name.strip_prefix("test:").is_some_and(|rest| {
            !rest.is_empty()
                && rest
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || b":._-".contains(&byte))
        })
}

/// A JavaScript runner invocation (`npx vitest`, `pnpm exec jest`, `yarn
/// vitest`, `vitest` itself, `npm test`, `pnpm run test:unit`), its JSON
/// reporter switched on, and the script it runs; `None` for anything else.
fn javascript_kind(argv: &mut Vec<String>, program: &str) -> Option<(ProjectKind, Option<String>)> {
    let at = |argv: &[String], index: usize| argv.get(index).cloned().unwrap_or_default();
    let tool_index = match program {
        "vitest" | "jest" => 0,
        "npx" | "bunx" => 1,
        "npm" if matches!(at(argv, 1).as_str(), "exec" | "x") => 2,
        "pnpm" | "yarn" | "bun" if matches!(at(argv, 1).as_str(), "exec" | "dlx" | "x") => 2,
        "pnpm" | "yarn" | "bun" if matches!(at(argv, 1).as_str(), "vitest" | "jest") => 1,
        "npm" | "pnpm" | "yarn" | "bun" => {
            let script = match at(argv, 1).as_str() {
                // bun's own test runner, not a script.
                "test" if program == "bun" => return Some((ProjectKind::Script, None)),
                "test" => "test".to_owned(),
                "t" if matches!(program, "npm" | "pnpm") => "test".to_owned(),
                "run" | "run-script" => at(argv, 2),
                // pnpm, yarn and bun run a script by its bare name; npm
                // does not.
                other if program != "npm" => other.to_owned(),
                _ => return None,
            };
            return routine_script(&script).then_some((ProjectKind::Script, Some(script)));
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
            Some((ProjectKind::Vitest, None))
        }
        "jest" => {
            if !has_flag(argv, "--json") {
                argv.push("--json".to_owned());
            }
            Some((ProjectKind::Jest, None))
        }
        _ => None,
    }
}

/// Words in a script's body that publish or deploy: such a script is not
/// run as a test.
const PUBLISHING_WORDS: &[&str] = &["publish", "deploy", "release"];

/// Check that `dir`'s `package.json` (read through the confined
/// workspace) declares the script `name`, and that neither it nor the
/// `pre`/`post` scripts a package manager runs around it publish or
/// deploy (a word such as `publish` or `deploy:prod` in its body).
///
/// # Errors
///
/// [`crate::error::ErrorCode::InvalidArgument`] when the file cannot be
/// read, does not declare the script, or one of them publishes.
pub fn check_package_script(workspace: &Workspace, dir: &WsPath, name: &str) -> ToolResult<()> {
    let missing = || {
        ToolError::invalid(format!(
            "`{dir}` has no package.json declaring the script `{name}`; pass a script the \
             package declares"
        ))
    };
    let path = child(dir, "package.json").ok_or_else(missing)?;
    if workspace.denied_by(&path, Intent::Read).is_some() {
        return Err(missing());
    }
    let read = workspace
        .read(&path, MAX_MARKER_BYTES)
        .map_err(|_| missing())?;
    let manifest: Value = serde_json::from_slice(&read.bytes).map_err(|_| missing())?;
    let scripts = manifest.get("scripts");
    let body = |name: &str| {
        scripts
            .and_then(|scripts| scripts.get(name))
            .and_then(Value::as_str)
    };
    if body(name).is_none() {
        return Err(missing());
    }
    let publishes = |text: &str| {
        text.split(|c: char| c.is_whitespace() || "&|;()'\"`".contains(c))
            .any(|word| {
                PUBLISHING_WORDS.iter().any(|publishing| {
                    word == *publishing
                        || word
                            .strip_prefix(publishing)
                            .is_some_and(|rest| rest.starts_with(':'))
                })
            })
    };
    for script in [format!("pre{name}"), name.to_owned(), format!("post{name}")] {
        if body(&script).is_some_and(publishes) {
            return Err(ToolError::invalid(format!(
                "the script `{script}` publishes or deploys; run_tests does not run it (use \
                 run_command, which the person approves)"
            )));
        }
    }
    Ok(())
}

/// The `[package]` name in a `Cargo.toml`'s text, when it is plainly a
/// crate name.
fn cargo_package_name(manifest: &str) -> Option<String> {
    let mut in_package = false;
    for line in manifest.lines() {
        let line = line.trim();
        if line.starts_with('[') {
            in_package = line == "[package]";
            continue;
        }
        if !in_package {
            continue;
        }
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        if key.trim() != "name" {
            continue;
        }
        let value = value.trim();
        let name = value
            .strip_prefix('"')
            .and_then(|rest| rest.split_once('"'))
            .map(|(name, _)| name)?;
        let plain = !name.is_empty()
            && name
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"_-".contains(&byte));
        return plain.then(|| name.to_owned());
    }
    None
}

impl Runner {
    /// The test command narrowed to `member`, a folder below the runner's
    /// own (a cargo workspace's crate, a monorepo's package, a module),
    /// and the folder it runs in: cargo `-p <crate>` (the nearest
    /// `Cargo.toml` with a `[package]` at or above `member`), go in
    /// `member` itself, vitest, jest and pytest with `member` as a path
    /// argument, Maven `-pl <module>` and Gradle `-p <project>` (the
    /// nearest folder with a `pom.xml`, a `build.gradle(.kts)`), a package
    /// script in `member` when its `package.json` declares `test`. A
    /// member none of those narrow to runs the whole command.
    #[must_use]
    pub fn member_test(
        &self,
        workspace: &Workspace,
        member: &WsPath,
        filter: Option<&str>,
        junit: Option<&Path>,
    ) -> (Vec<String>, WsPath) {
        let own = self.dir.components();
        let below = member.components();
        if below.len() <= own.len() || below[..own.len()] != *own {
            return (self.test_argv(filter, junit), self.dir.clone());
        }
        let relative = below[own.len()..].join("/");
        let mut argv = self.test_argv(None, junit);
        let mut cwd = self.dir.clone();
        // The nearest folder at or above `member` (below the runner's) whose
        // marker `name` passes `accept`: that folder (relative to the
        // runner's) and what `accept` made of the marker.
        let nearest = |names: &[&str], accept: &dyn Fn(&str) -> Option<String>| {
            (own.len() + 1..=below.len()).rev().find_map(|end| {
                let dir = WsPath::from_relative(Path::new(&below[..end].join("/"))).ok()?;
                names.iter().find_map(|name| {
                    let path = child(&dir, name)?;
                    if workspace.denied_by(&path, Intent::Read).is_some() {
                        return None;
                    }
                    let read = workspace.read(&path, MAX_MARKER_BYTES).ok()?;
                    let made = accept(&String::from_utf8_lossy(&read.bytes))?;
                    Some((below[own.len()..end].join("/"), made))
                })
            })
        };
        let any = |_: &str| Some(String::new());
        match self.kind {
            ProjectKind::Cargo => {
                if let Some((_, name)) = nearest(&["Cargo.toml"], &cargo_package_name) {
                    argv.splice(2..2, ["-p".to_owned(), name]);
                }
            }
            ProjectKind::Go => cwd = member.clone(),
            ProjectKind::Vitest | ProjectKind::Jest | ProjectKind::Pytest => argv.push(relative),
            ProjectKind::Maven => {
                if let Some((module, _)) = nearest(&["pom.xml"], &any) {
                    argv.splice(1..1, ["-pl".to_owned(), module]);
                }
            }
            ProjectKind::Gradle => {
                if let Some((project, _)) = nearest(&["build.gradle", "build.gradle.kts"], &any) {
                    argv.splice(1..1, ["-p".to_owned(), project]);
                }
            }
            ProjectKind::Script => {
                if check_package_script(workspace, member, "test").is_ok() {
                    cwd = member.clone();
                }
            }
        }
        if let Some(filter) = filter {
            add_filter(self.kind, &mut argv, filter);
        }
        (argv, cwd)
    }
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};

    use super::{ProjectKind, check_package_script, detect, given_command, plain_dir};
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
        assert_eq!(empty.runners, []);
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

    /// Every Maven goal and Gradle task must build or test: one that
    /// installs, deploys or publishes beside a test goal is refused, so a
    /// remembered "always allow" for the runner never covers it (#1224).
    #[test]
    fn maven_and_gradle_run_only_build_and_test_goals() {
        let kind = |command: &str| given_command(command, None, None).map(|plan| plan.kind);
        for accepted in [
            "mvn test",
            "mvn -q -B -pl core -am -Dskip.it=true clean verify",
            "./mvnw -P ci --offline test-compile surefire:test",
            "mvn --projects=core test",
        ] {
            assert_eq!(kind(accepted), Ok(ProjectKind::Maven), "{accepted}");
        }
        for accepted in [
            "gradle test",
            "./gradlew --offline -x lint clean :app:test check",
            "./gradlew -p services/api --console=plain build",
            "gradle -Pci=true compileTestJava",
        ] {
            assert_eq!(kind(accepted), Ok(ProjectKind::Gradle), "{accepted}");
        }
        for refused in [
            "mvn test deploy",
            "mvn install",
            "mvn verify site-deploy",
            "mvn test release:perform",
            "mvn -f other/pom.xml test",
            "mvn -s settings.xml test",
            "mvn test -Dx=1 deploy:deploy",
            "mvn -q",
            "gradle test publish",
            "gradle test :lib:publish",
            "gradle test publishToMavenLocal",
            "gradle uploadArchives test",
            "./gradlew test --scan",
            "./gradlew -I init.gradle test",
            "./gradlew -p /elsewhere test",
            "./gradlew -p ../other test",
            "gradle pub",
        ] {
            assert!(kind(refused).is_err(), "{refused}");
        }
    }

    /// A package manager runs only `test`, `test:<name>`, `check`,
    /// `typecheck` or `lint`, by exact name (#1224).
    #[test]
    fn package_scripts_are_taken_by_exact_name() {
        let script = |command: &str| {
            given_command(command, None, None)
                .map(|plan| (plan.kind, plan.script))
                .map_err(|_| ())
        };
        let ok = |name: &str| Ok((ProjectKind::Script, Some(name.to_owned())));
        assert_eq!(script("npm test"), ok("test"));
        assert_eq!(script("npm t"), ok("test"));
        assert_eq!(script("npm run test:unit"), ok("test:unit"));
        assert_eq!(script("pnpm lint"), ok("lint"));
        assert_eq!(script("yarn typecheck"), ok("typecheck"));
        assert_eq!(script("bun run check"), ok("check"));
        assert_eq!(script("bun test"), Ok((ProjectKind::Script, None)));
        for refused in [
            "npm run testpublish",
            "npm run test-and-deploy",
            "pnpm checkout",
            "yarn lint-fix-and-release",
            "pnpm typecheck:publish",
            "npm run test:",
            "yarn t",
            "pnpm -r publish",
            "npm run deploy",
            "npm check",
        ] {
            assert!(script(refused).is_err(), "{refused}");
        }
    }

    /// The script must be declared, and neither it nor the scripts run
    /// around it may publish.
    #[test]
    fn a_package_script_must_exist_and_not_publish() {
        let dir = tempfile::tempdir().expect("dir");
        std::fs::write(
            dir.path().join("package.json"),
            r#"{"scripts":{"test":"mocha","test:unit":"mocha unit","lint":"eslint . && npm publish",
                "check":"tsc","postcheck":"changeset publish","test:e2e":"playwright test deploy/"}}"#,
        )
        .expect("package.json");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let root = WsPath::root();
        assert!(check_package_script(&workspace, &root, "test").is_ok());
        assert!(check_package_script(&workspace, &root, "test:unit").is_ok());
        // A folder named `deploy/` is not deploying.
        assert!(check_package_script(&workspace, &root, "test:e2e").is_ok());
        for refused in ["typecheck", "test:other", "lint", "check"] {
            assert!(
                check_package_script(&workspace, &root, refused).is_err(),
                "{refused}"
            );
        }
        let denied = Workspace::open(dir.path(), &["package.json".to_owned()]).expect("workspace");
        assert!(check_package_script(&denied, &root, "test").is_err());
    }

    /// A filter selects tests: a command that runs none refuses it instead
    /// of passing it on as an argument (#1224).
    #[test]
    fn a_filter_is_refused_for_commands_that_run_no_tests() {
        for command in [
            "cargo check",
            "cargo clippy",
            "cargo build",
            "go vet ./...",
            "go build ./...",
            "mvn compile",
            "gradle build",
            "gradle check -x test",
            "npm run lint",
            "pnpm typecheck",
        ] {
            assert!(
                given_command(command, Some("adds"), None).is_err(),
                "{command}"
            );
        }
        let argv = |command: &str| {
            given_command(command, Some("FooTest"), None)
                .expect("plan")
                .argv
                .join(" ")
        };
        assert_eq!(argv("mvn clean test"), "mvn clean test -Dtest=FooTest");
        assert_eq!(
            argv("./gradlew :app:test check"),
            "./gradlew :app:test --tests FooTest check"
        );
        assert_eq!(argv("npm run test:unit"), "npm run test:unit -- FooTest");
    }

    /// A `test` script that is neither vitest nor jest (mocha, ava, `node
    /// --test`) is the package's own runner; npm's placeholder is not.
    #[test]
    fn a_plain_test_script_is_detected() {
        let dir = tempfile::tempdir().expect("dir");
        std::fs::write(
            dir.path().join("package.json"),
            r#"{"scripts":{"test":"mocha"}}"#,
        )
        .expect("package.json");
        std::fs::create_dir(dir.path().join("placeholder")).expect("dir");
        std::fs::write(
            dir.path().join("placeholder/package.json"),
            r#"{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}"#,
        )
        .expect("package.json");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let profile = detect(&workspace, &[]);
        assert_eq!(profile.runners.len(), 1, "{:?}", profile.runners);
        let runner = &profile.runners[0];
        assert_eq!(runner.kind, ProjectKind::Script);
        assert_eq!(runner.test_argv(None, None), ["npm", "test"]);
        assert_eq!(
            profile.preamble_line().as_deref(),
            Some("Project: script (npm); tests: npm test")
        );
    }

    /// A run asked for in a member of a workspace runs that member, not
    /// the whole workspace (#1224).
    #[test]
    fn a_member_folder_narrows_the_root_runner() {
        let dir = tempfile::tempdir().expect("dir");
        let write = |path: &str, text: &str| {
            let path = dir.path().join(path);
            std::fs::create_dir_all(path.parent().expect("parent")).expect("dirs");
            std::fs::write(path, text).expect("write");
        };
        write("Cargo.toml", "[workspace]\nmembers = [\"crates/*\"]\n");
        write(
            "crates/foo/Cargo.toml",
            "[package]\nversion = \"0.1.0\"\nname = \"foo-core\"\n\n[dependencies]\nname = \"x\"\n",
        );
        write("crates/foo/src/lib.rs", "");
        write("go.mod", "module x\n");
        write("pom.xml", "<project/>");
        write("core/pom.xml", "<project/>");
        write("core/src/main/java/A.java", "");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let profile = detect(&workspace, &[]);
        let path = |p: &str| WsPath::from_relative(Path::new(p)).expect("p");
        let run = |kind: ProjectKind, member: &str, filter: Option<&str>| {
            let runner = profile
                .runners
                .iter()
                .find(|runner| runner.kind == kind)
                .expect("runner");
            let (argv, cwd) = runner.member_test(&workspace, &path(member), filter, None);
            (argv.join(" "), cwd.display_string())
        };
        let pair = |argv: &str, cwd: &str| (argv.to_owned(), cwd.to_owned());
        assert_eq!(
            run(ProjectKind::Cargo, "crates/foo/src", Some("adds")),
            pair("cargo test -p foo-core --message-format=json adds", ".")
        );
        assert_eq!(
            run(ProjectKind::Cargo, "docs", None),
            pair("cargo test --message-format=json", "."),
            "no crate there: the whole workspace"
        );
        assert_eq!(
            run(ProjectKind::Go, "crates/foo", None),
            pair("go test -json ./...", "crates/foo")
        );
        assert_eq!(
            run(ProjectKind::Maven, "core/src", None),
            pair("mvn -pl core -q test", ".")
        );
    }
}
