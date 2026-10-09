//! git, through the git CLI: the read-only tools (status, diff, log,
//! branches) and the plumbing the checkpoints use.
//!
//! Why the CLI and not gix (which `repo-ingest` locks): status and diff need
//! gix's `status`/`blob-diff` features, which the workspace does not build,
//! and checkpoints need `add -A` semantics (`.gitignore`, `.git/info/exclude`,
//! `core.excludesFile`) exactly as the person's git applies them. The
//! desktop runs where the person already has git; the cost is a process per
//! call.
//!
//! # The host runs git in a folder someone else may control
//!
//! A repository can make git run its code: filter drivers (`clean` on
//! `status` and `add`, `smudge` on `checkout-index`), `gpg.program`,
//! `core.fsmonitor`, hooks, `core.sshCommand`, textconv and diff drivers,
//! aliases, credential helpers, `include.path` pulling any of those in, a
//! `.git` file or `commondir` pointing at another git directory,
//! `core.worktree` pointing the writes elsewhere, submodules with their own
//! config. The person's global config is trusted; the repository is not.
//! Four layers, all on every call:
//!
//! 1. **Refuse unsafe repositories** ([`Repo::check`], before any git
//!    process): `.git` must be a plain directory (not a symlink, not a
//!    `gitdir:` file) at the work tree's top, with no `commondir`, no
//!    `modules/`, no alternates, and a local config (and `config.worktree`)
//!    that this module parses itself (reading config runs no code) and that
//!    holds none of the dangerous key families ([`dangerous_key`]).
//!    Attributes ([`Repo::check_attributes`], before every call that reads
//!    the work tree): when the person's global config defines filter, diff
//!    or merge drivers with commands, no path may select one. A refusal is
//!    [`ErrorCode::UnsafeRepository`], shown to the agent and the person;
//!    checkpoints then fall back to copies.
//! 2. **Override on the command line** ([`HARDENING`]): hooks to
//!    `/dev/null`, no fsmonitor, no pager, no signing or signature checks,
//!    no external diff, no credential helper, `protocol.allow=never`, no
//!    system config or system attributes, no attributes file, no submodule
//!    recursion; per command `--no-ext-diff --no-textconv`,
//!    `--ignore-submodules=all`, `--no-show-signature`, `--no-gpg-sign`;
//!    `GIT_DIR` and `GIT_WORK_TREE` pinned, the parent's `GIT_*` dropped.
//! 3. **Sandbox every git process** (Seatbelt on macOS, bubblewrap or the
//!    Landlock helper on Linux): no network, and writes only where the call
//!    needs them (the temporary index, `objects/`, `refs/elitea/`; the
//!    workspace for a restore). A path this module did not foresee still
//!    runs confined.
//! 4. **Timeouts** on every call, killing the process group.

use std::collections::BTreeSet;
use std::ffi::OsStr;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::OnceLock;
use std::sync::mpsc;
use std::time::{Duration, Instant};

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;
use crate::sandbox::{SandboxConfig, SandboxRequest, credential_paths, prepare};

/// Bytes of git output a tool returns.
pub const OUTPUT_CAP: usize = 256 * 1024;

/// The most output one git process may produce before it is cut off.
const PROCESS_OUTPUT_CAP: usize = 64 * 1024 * 1024;

/// How long a read-only git call may take.
pub const READ_TIMEOUT: Duration = Duration::from_secs(30);

/// How long a git call that writes (a checkpoint, a restore) may take.
pub const WRITE_TIMEOUT: Duration = Duration::from_mins(2);

/// The largest repository config this module reads.
const MAX_CONFIG_BYTES: u64 = 1024 * 1024;

/// Overrides on every call; later `-c` beat every config file.
pub const HARDENING: &[&str] = &[
    "-c",
    "core.fsmonitor=false",
    "-c",
    "core.hooksPath=/dev/null",
    "-c",
    "diff.external=",
    "-c",
    "core.pager=cat",
    "-c",
    "color.ui=false",
    "-c",
    "core.quotePath=false",
    "-c",
    "log.showSignature=false",
    "-c",
    "commit.gpgSign=false",
    "-c",
    "tag.gpgSign=false",
    "-c",
    "gpg.program=/usr/bin/false",
    "-c",
    "core.sshCommand=/usr/bin/false",
    "-c",
    "core.askPass=",
    "-c",
    "credential.helper=",
    "-c",
    "protocol.allow=never",
    "-c",
    "core.attributesFile=/dev/null",
    "-c",
    "submodule.recurse=false",
    "-c",
    "diff.ignoreSubmodules=all",
    "-c",
    "status.submoduleSummary=false",
    "-c",
    "core.untrackedCache=false",
    "-c",
    "core.logAllRefUpdates=false",
    "-c",
    "gc.auto=0",
    "-c",
    "maintenance.auto=false",
    "-c",
    "core.alternateRefsCommand=",
    "--no-pager",
    "--no-optional-locks",
];

/// Environment variables removed besides every `GIT_*`.
const REMOVED_ENV: &[&str] = &["SSH_ASKPASS", "SSH_AUTH_SOCK", "EDITOR", "VISUAL", "PAGER"];

fn unsafe_repo(message: impl Into<String>) -> ToolError {
    ToolError::new(ErrorCode::UnsafeRepository, message)
}

/// One `key = value` of a git config file; keys lowercased except the
/// subsection, as git compares them.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConfigEntry {
    pub key: String,
    pub value: Option<String>,
}

/// Parse a git config file (the syntax of `git-config(1)`), without
/// following includes. Anything this parser does not understand is an
/// error: an unsafe repository, not a guess.
///
/// # Errors
///
/// A line that is not a section header, a variable or a comment.
pub fn parse_config(text: &str) -> Result<Vec<ConfigEntry>, String> {
    let mut entries = Vec::new();
    let mut section: Option<String> = None;
    let mut lines = text.lines().enumerate();
    while let Some((number, raw)) = lines.next() {
        let mut line = raw.to_owned();
        // A trailing backslash continues a value on the next line.
        while line.ends_with('\\') && !line.ends_with("\\\\") {
            line.pop();
            match lines.next() {
                Some((_, next)) => line.push_str(next),
                None => break,
            }
        }
        let mut rest = line.trim_start();
        if rest.starts_with('[') {
            let close =
                header_end(rest).ok_or_else(|| format!("line {}: bad section", number + 1))?;
            section = Some(
                parse_header(&rest[1..close])
                    .ok_or_else(|| format!("line {}: bad section", number + 1))?,
            );
            rest = rest[close + 1..].trim_start();
        }
        if rest.is_empty() || rest.starts_with('#') || rest.starts_with(';') {
            continue;
        }
        let Some(current) = &section else {
            return Err(format!("line {}: a variable outside a section", number + 1));
        };
        let name_end = rest
            .find(|c: char| !(c.is_ascii_alphanumeric() || c == '-'))
            .unwrap_or(rest.len());
        let name = &rest[..name_end];
        if name.is_empty() || !name.starts_with(|c: char| c.is_ascii_alphabetic()) {
            return Err(format!("line {}: not a variable", number + 1));
        }
        let after = rest[name_end..].trim_start();
        let value = if after.is_empty() || after.starts_with('#') || after.starts_with(';') {
            None
        } else if let Some(value) = after.strip_prefix('=') {
            Some(parse_value(value))
        } else {
            return Err(format!("line {}: not a variable", number + 1));
        };
        entries.push(ConfigEntry {
            key: format!("{current}.{}", name.to_ascii_lowercase()),
            value,
        });
    }
    Ok(entries)
}

/// The index of the `]` closing a section header (quotes respected).
fn header_end(text: &str) -> Option<usize> {
    let mut quoted = false;
    let mut escaped = false;
    for (index, c) in text.char_indices() {
        match c {
            _ if escaped => escaped = false,
            '\\' if quoted => escaped = true,
            '"' => quoted = !quoted,
            ']' if !quoted => return Some(index),
            _ => {}
        }
    }
    None
}

/// `section "sub"`, `section.sub` or `section`, as `section.sub`.
fn parse_header(inner: &str) -> Option<String> {
    let inner = inner.trim();
    if let Some((name, sub)) = inner.split_once(|c: char| c.is_whitespace()) {
        let sub = sub.trim().strip_prefix('"')?.strip_suffix('"')?;
        let mut unescaped = String::new();
        let mut chars = sub.chars();
        while let Some(c) = chars.next() {
            unescaped.push(if c == '\\' { chars.next()? } else { c });
        }
        let name = valid_section(name)?;
        return Some(format!("{name}.{unescaped}"));
    }
    match inner.split_once('.') {
        Some((name, sub)) => Some(format!(
            "{}.{}",
            valid_section(name)?,
            sub.to_ascii_lowercase()
        )),
        None => valid_section(inner),
    }
}

fn valid_section(name: &str) -> Option<String> {
    (!name.is_empty()
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '.'))
    .then(|| name.to_ascii_lowercase())
}

/// A value: quotes removed, escapes resolved, an unquoted comment cut.
fn parse_value(text: &str) -> String {
    let mut out = String::new();
    let mut quoted = false;
    let mut chars = text.trim().chars();
    while let Some(c) = chars.next() {
        match c {
            '"' => quoted = !quoted,
            '\\' => match chars.next() {
                Some('n') => out.push('\n'),
                Some('t') => out.push('\t'),
                Some(other) => out.push(other),
                None => {}
            },
            '#' | ';' if !quoted => break,
            _ => out.push(c),
        }
    }
    out.trim_end().to_owned()
}

fn is_false(value: Option<&str>) -> bool {
    value.is_some_and(|value| {
        matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "false" | "no" | "off" | "0" | ""
        )
    })
}

/// Why a repository-local config key is refused, if it is.
///
/// Whole sections that run code or redirect git (`filter`, `include`,
/// `includeIf`, `alias`, `credential`, `protocol`, `url`, `submodule`,
/// `gpg`, `pager`), any `*.textconv`, `*.command`, `*.cmd`, `*.driver`,
/// and the single keys that run programs or move the work tree. Signing
/// and signature switches and `core.fsmonitor` pass when they are false.
#[must_use]
pub fn dangerous_key(key: &str, value: Option<&str>) -> Option<&'static str> {
    let section = key.split('.').next().unwrap_or_default();
    let name = key.rsplit('.').next().unwrap_or_default();
    let has_subsection = key.matches('.').count() >= 2;
    match section {
        "filter" => return Some("a filter driver"),
        "include" | "includeif" => return Some("an include"),
        "alias" => return Some("an alias"),
        "credential" => return Some("a credential helper"),
        "protocol" | "url" => return Some("a transport setting"),
        "submodule" => return Some("a submodule"),
        "gpg" => return Some("a gpg program"),
        "pager" => return Some("a pager"),
        _ => {}
    }
    if has_subsection && matches!(name, "textconv" | "command" | "cmd" | "driver") {
        return Some("a diff, merge or tool command");
    }
    match key {
        "core.fsmonitor" | "commit.gpgsign" | "tag.gpgsign" | "log.showsignature"
            if is_false(value) =>
        {
            None
        }
        "core.fsmonitor" => Some("an fsmonitor command"),
        "commit.gpgsign" | "tag.gpgsign" | "log.showsignature" => Some("signing"),
        "core.worktree" => Some("core.worktree"),
        "core.hookspath" => Some("a hooks path"),
        "core.sshcommand" | "core.gitproxy" => Some("an ssh or proxy command"),
        "core.askpass" => Some("an askpass program"),
        "core.editor" | "sequence.editor" => Some("an editor"),
        "core.pager" => Some("a pager"),
        "core.alternaterefscommand" | "diff.external" => Some("an external command"),
        "extensions.refstorage"
            if value.is_some_and(|value| value.eq_ignore_ascii_case("files")) =>
        {
            None
        }
        "extensions.refstorage" => Some("a ref storage other than files"),
        _ => None,
    }
}

/// The git executable: the first absolute `PATH` entry holding one
/// (relative entries are ignored). On macOS the `/usr/bin/git` shim is
/// resolved to the developer tools' git, which runs without `xcrun`'s
/// cache files.
fn git_binary() -> ToolResult<PathBuf> {
    static GIT: OnceLock<Option<PathBuf>> = OnceLock::new();
    GIT.get_or_init(|| {
        let path = std::env::var_os("PATH")?;
        let found = std::env::split_paths(&path)
            .filter(|dir| dir.is_absolute())
            .map(|dir| dir.join("git"))
            .find(|candidate| candidate.is_file())?;
        if cfg!(target_os = "macos") && found == Path::new("/usr/bin/git") {
            let resolved = Command::new("/usr/bin/xcrun")
                .args(["--find", "git"])
                .stdin(Stdio::null())
                .stderr(Stdio::null())
                .output()
                .ok()
                .filter(|output| output.status.success())
                .map(|output| PathBuf::from(String::from_utf8_lossy(&output.stdout).trim()))
                .filter(|path| path.is_absolute() && path.is_file());
            return Some(resolved.unwrap_or(found));
        }
        Some(found)
    })
    .clone()
    .ok_or_else(|| ToolError::new(ErrorCode::Unsupported, "git is not available"))
}

/// A git work tree the host has found and checks before every call.
#[derive(Clone, Debug)]
pub struct Repo {
    top: PathBuf,
    git_dir: PathBuf,
    sandbox: SandboxConfig,
    /// Tests: a global config file instead of the person's.
    global_config: Option<PathBuf>,
}

impl Repo {
    /// The repository whose work tree holds `dir` (canonical), found the
    /// way git looks (the nearest `.git` upwards), and checked.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] when one is found but must not be
    /// used; `Ok(None)` when there is none.
    pub fn discover(dir: &Path, sandbox: &SandboxConfig) -> ToolResult<Option<Self>> {
        let Some(top) = dir
            .ancestors()
            .find(|candidate| std::fs::symlink_metadata(candidate.join(".git")).is_ok())
        else {
            return Ok(None);
        };
        let repo = Self {
            top: top.to_path_buf(),
            git_dir: top.join(".git"),
            sandbox: sandbox.clone(),
            global_config: None,
        };
        repo.check()?;
        Ok(Some(repo))
    }

    /// Tests: read this file as the global config.
    #[doc(hidden)]
    #[must_use]
    pub fn with_global_config(mut self, path: PathBuf) -> Self {
        self.global_config = Some(path);
        self
    }

    /// The work tree's top level.
    #[must_use]
    pub fn top(&self) -> &Path {
        &self.top
    }

    #[must_use]
    pub fn git_dir(&self) -> &Path {
        &self.git_dir
    }

    /// Layer 1: the repository's shape and config (no process runs).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] with the reason.
    pub fn check(&self) -> ToolResult<()> {
        let meta = std::fs::symlink_metadata(&self.git_dir)
            .map_err(|_| unsafe_repo("the repository's .git is gone"))?;
        if meta.file_type().is_symlink() {
            return Err(unsafe_repo("the repository's .git is a symlink"));
        }
        if meta.is_file() {
            return Err(unsafe_repo(
                "the repository's .git is a file pointing at a git directory elsewhere (a worktree or submodule)",
            ));
        }
        if !meta.is_dir() {
            return Err(unsafe_repo("the repository's .git is not a directory"));
        }
        for (name, reason) in [
            ("commondir", "shares a git directory elsewhere (commondir)"),
            (
                "objects/info/alternates",
                "borrows objects from elsewhere (alternates)",
            ),
        ] {
            if std::fs::symlink_metadata(self.git_dir.join(name)).is_ok() {
                return Err(unsafe_repo(format!("the repository {reason}")));
            }
        }
        if std::fs::read_dir(self.git_dir.join("modules"))
            .is_ok_and(|mut entries| entries.next().is_some())
        {
            return Err(unsafe_repo(
                "the repository has submodules with their own git directories",
            ));
        }
        for name in ["config", "config.worktree"] {
            for entry in self.read_config(name)? {
                if let Some(reason) = dangerous_key(&entry.key, entry.value.as_deref()) {
                    return Err(unsafe_repo(format!(
                        "the repository's .git/{name} sets `{}` ({reason}), which would run code in the host's git",
                        entry.key
                    )));
                }
            }
        }
        Ok(())
    }

    fn read_config(&self, name: &str) -> ToolResult<Vec<ConfigEntry>> {
        let path = self.git_dir.join(name);
        let meta = match std::fs::symlink_metadata(&path) {
            Ok(meta) => meta,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(_) => return Err(unsafe_repo(format!("cannot read .git/{name}"))),
        };
        if !meta.is_file() || meta.len() > MAX_CONFIG_BYTES {
            return Err(unsafe_repo(format!(
                ".git/{name} is not a plain config file"
            )));
        }
        let text =
            std::fs::read(&path).map_err(|_| unsafe_repo(format!("cannot read .git/{name}")))?;
        let text = String::from_utf8(text)
            .map_err(|_| unsafe_repo(format!(".git/{name} is not UTF-8")))?;
        parse_config(&text)
            .map_err(|reason| unsafe_repo(format!(".git/{name} cannot be parsed safely: {reason}")))
    }

    /// Drivers with commands in the person's global config:
    /// `(kind, name)` for `filter`, `diff` and `merge`.
    fn configured_drivers(&self) -> BTreeSet<(String, String)> {
        let listing = self
            .git(&self.top)
            .run(&["config", "--global", "--includes", "--list", "-z"])
            .unwrap_or_default();
        let mut out = BTreeSet::new();
        for item in listing.split(|byte| *byte == 0) {
            let item = String::from_utf8_lossy(item);
            let key = item.split('\n').next().unwrap_or_default().to_owned();
            if let Some(driver) = driver_of(&key) {
                out.insert(driver);
            }
        }
        out
    }

    /// Layer 1 for attributes: no path in the work tree (nor in `extra`,
    /// top-relative) selects a filter, diff or merge driver that the
    /// person's config gives a command.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] naming the path and the driver.
    pub fn check_attributes(&self, extra: &[String]) -> ToolResult<()> {
        let drivers = self.configured_drivers();
        if drivers.is_empty() {
            return Ok(());
        }
        let mut paths = self.git(&self.top).literal(false).run(&[
            "ls-files",
            "-z",
            "--cached",
            "--others",
            "--exclude-standard",
        ])?;
        for path in extra {
            paths.extend_from_slice(path.as_bytes());
            paths.push(0);
        }
        let attributes = self.git(&self.top).stdin(&paths).run(&[
            "check-attr",
            "-z",
            "--stdin",
            "filter",
            "diff",
            "merge",
        ])?;
        let fields: Vec<String> = attributes
            .split(|byte| *byte == 0)
            .map(|field| String::from_utf8_lossy(field).into_owned())
            .collect();
        for triple in fields.chunks_exact(3) {
            let (path, kind, value) = (&triple[0], &triple[1], &triple[2]);
            if drivers.contains(&(kind.clone(), value.clone())) {
                return Err(unsafe_repo(format!(
                    "`{path}` selects the {kind} driver `{value}`, whose command would run in the host's git"
                )));
            }
        }
        Ok(())
    }

    /// The person's identity from their global git config (never the
    /// repository's): `(name, email)`.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::Git`] when `user.name` or `user.email` is not set
    /// globally.
    pub fn global_identity(&self) -> ToolResult<(String, String)> {
        let get = |key: &str| {
            self.git(&self.top)
                .text(&["config", "--global", "--includes", "--get", key])
                .ok()
                .filter(|value| !value.is_empty())
        };
        match (get("user.name"), get("user.email")) {
            (Some(name), Some(email)) => Ok((name, email)),
            _ => Err(ToolError::new(
                ErrorCode::Git,
                "set user.name and user.email in your global git config to commit",
            )),
        }
    }

    /// What a commit may write: the git directory, except what runs code
    /// or redirects git (hooks, config, info, modules, commondir).
    #[must_use]
    pub(crate) fn commit_access(&self) -> (Vec<PathBuf>, Vec<PathBuf>) {
        let protected = [
            "hooks",
            "config",
            "config.worktree",
            "info",
            "modules",
            "commondir",
        ]
        .iter()
        .map(|name| self.git_dir.join(name))
        .collect();
        (vec![self.git_dir.clone()], protected)
    }

    /// A git call in this repository, from `cwd` (inside the work tree).
    pub(crate) fn git<'a>(&'a self, cwd: &'a Path) -> Git<'a> {
        Git {
            repo: self,
            cwd,
            env: Vec::new(),
            stdin: None,
            writable: Vec::new(),
            protected: Vec::new(),
            timeout: READ_TIMEOUT,
            literal: true,
            worktree: false,
        }
    }
}

/// `(kind, name)` when `key` gives a filter, diff or merge driver a
/// command.
fn driver_of(key: &str) -> Option<(String, String)> {
    let lower = key.to_ascii_lowercase();
    let (section, rest) = lower.split_once('.')?;
    let (name, variable) = rest.rsplit_once('.')?;
    let commands = match section {
        "filter" => matches!(variable, "clean" | "smudge" | "process"),
        "diff" => matches!(variable, "command" | "textconv"),
        "merge" => variable == "driver",
        _ => false,
    };
    // The driver name keeps its case (attributes compare it exactly).
    let original = &key[section.len() + 1..key.len() - variable.len() - 1];
    debug_assert_eq!(original.to_ascii_lowercase(), name);
    commands.then(|| (section.to_owned(), original.to_owned()))
}

/// One git invocation.
pub(crate) struct Git<'a> {
    repo: &'a Repo,
    cwd: &'a Path,
    env: Vec<(&'a str, &'a OsStr)>,
    stdin: Option<&'a [u8]>,
    writable: Vec<PathBuf>,
    protected: Vec<PathBuf>,
    timeout: Duration,
    literal: bool,
    worktree: bool,
}

impl<'a> Git<'a> {
    /// Keep `paths` read-only inside the writable ones.
    pub(crate) fn protect(mut self, paths: &[PathBuf]) -> Self {
        self.protected.extend(paths.iter().cloned());
        self
    }

    pub(crate) fn env(mut self, name: &'a str, value: &'a OsStr) -> Self {
        self.env.push((name, value));
        self
    }

    pub(crate) fn stdin(mut self, bytes: &'a [u8]) -> Self {
        self.stdin = Some(bytes);
        self
    }

    /// Let this call write under `paths` (inside its sandbox), with the
    /// longer timeout.
    pub(crate) fn writes(mut self, paths: &[PathBuf]) -> Self {
        self.writable.extend(paths.iter().cloned());
        self.timeout = WRITE_TIMEOUT;
        self
    }

    /// Pathspecs are literal unless this is off (then the caller writes
    /// `:(literal)` and `:(exclude)` magic itself).
    pub(crate) fn literal(mut self, on: bool) -> Self {
        self.literal = on;
        self
    }

    /// This call reads work tree files (filters could run): check the
    /// attributes first.
    pub(crate) fn worktree(mut self) -> Self {
        self.worktree = true;
        self
    }

    #[cfg(test)]
    fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    /// Run `git <args>`; the raw output on success.
    pub(crate) fn run(&self, args: &[&str]) -> ToolResult<Vec<u8>> {
        self.repo.check()?;
        if self.worktree {
            self.repo.check_attributes(&[])?;
        }
        self.spawn(args)
    }

    fn spawn(&self, args: &[&str]) -> ToolResult<Vec<u8>> {
        let what = args.first().copied().unwrap_or_default();
        let mut words = vec![git_binary()?.display().to_string()];
        words.extend(HARDENING.iter().map(|arg| (*arg).to_owned()));
        if self.literal {
            words.push("--literal-pathspecs".to_owned());
        }
        words.extend(args.iter().map(|arg| (*arg).to_owned()));
        let request = SandboxRequest {
            writable_roots: self.writable.clone(),
            protected: self.protected.clone(),
            deny_paths: std::env::var_os("HOME")
                .filter(|home| !home.is_empty())
                .map(|home| credential_paths(Path::new(&home)))
                .unwrap_or_default(),
            ..SandboxRequest::new(
                if self.writable.is_empty() {
                    SandboxMode::ReadOnly
                } else {
                    SandboxMode::WorkspaceWrite
                },
                false,
            )
        };
        // The host's own git: run it under whatever confinement exists
        // (layers 1 and 2 already hold without one).
        let config = SandboxConfig {
            allow_unenforced: true,
            allow_partial: true,
            ..self.repo.sandbox.clone()
        };
        let prepared = prepare(&request, &words, &config)?;
        let mut command = Command::new(&prepared.program);
        command.current_dir(self.cwd).args(&prepared.args);
        for (name, _) in std::env::vars_os() {
            if name.to_string_lossy().starts_with("GIT_") {
                command.env_remove(name);
            }
        }
        for name in REMOVED_ENV {
            command.env_remove(name);
        }
        command
            .env("GIT_DIR", &self.repo.git_dir)
            .env("GIT_WORK_TREE", &self.repo.top)
            .env("LC_ALL", "C")
            .env("GIT_TERMINAL_PROMPT", "0")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_ATTR_NOSYSTEM", "1")
            .env("GIT_OPTIONAL_LOCKS", "0")
            .env("GIT_NO_REPLACE_OBJECTS", "1")
            .env("GIT_PROTOCOL_FROM_USER", "0");
        if let Some(global) = &self.repo.global_config {
            command.env("GIT_CONFIG_GLOBAL", global);
        }
        for (name, value) in &self.env {
            command.env(name, value);
        }
        run_bounded(command, self.stdin, self.timeout, what)
    }

    /// Run and return trimmed UTF-8 text.
    pub(crate) fn text(&self, args: &[&str]) -> ToolResult<String> {
        Ok(String::from_utf8_lossy(&self.run(args)?)
            .trim_end()
            .to_owned())
    }
}

/// Read a pipe to the end, keeping at most `cap` bytes.
fn drain<R: std::io::Read + Send + 'static>(mut reader: R, cap: usize) -> mpsc::Receiver<Vec<u8>> {
    let (sender, receiver) = mpsc::channel();
    std::thread::spawn(move || {
        let mut out = Vec::new();
        let mut buffer = [0u8; 16 * 1024];
        while let Ok(read) = reader.read(&mut buffer) {
            if read == 0 {
                break;
            }
            let room = cap.saturating_sub(out.len());
            out.extend_from_slice(&buffer[..read.min(room)]);
        }
        let _ = sender.send(out);
    });
    receiver
}

/// Spawn `command` in its own process group, feed `stdin`, and kill the
/// group at `timeout`.
fn run_bounded(
    mut command: Command,
    stdin: Option<&[u8]>,
    timeout: Duration,
    what: &str,
) -> ToolResult<Vec<u8>> {
    use std::os::unix::process::CommandExt as _;
    command
        .stdin(if stdin.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0);
    let mut child = command.spawn().map_err(|error| {
        ToolError::new(
            ErrorCode::Unsupported,
            format!("git is not available: {}", error.kind()),
        )
    })?;
    let group = i32::try_from(child.id())
        .ok()
        .and_then(rustix::process::Pid::from_raw);
    if let (Some(bytes), Some(mut pipe)) = (stdin, child.stdin.take()) {
        let bytes = bytes.to_vec();
        std::thread::spawn(move || {
            let _ = pipe.write_all(&bytes);
        });
    }
    let stdout = child
        .stdout
        .take()
        .map(|pipe| drain(pipe, PROCESS_OUTPUT_CAP));
    let stderr = child.stderr.take().map(|pipe| drain(pipe, 64 * 1024));
    let started = Instant::now();
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break Some(status),
            Ok(None) if started.elapsed() < timeout => {
                std::thread::sleep(Duration::from_millis(5));
            }
            _ => break None,
        }
    };
    if let Some(group) = group {
        let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
    }
    let Some(status) = status else {
        let _ = child.kill();
        let _ = child.wait();
        return Err(ToolError::new(
            ErrorCode::Timeout,
            format!("git {what} did not finish in {}s", timeout.as_secs()),
        ));
    };
    let collect = |receiver: Option<mpsc::Receiver<Vec<u8>>>| {
        receiver
            .and_then(|receiver| receiver.recv_timeout(Duration::from_secs(2)).ok())
            .unwrap_or_default()
    };
    let out = collect(stdout);
    let err = collect(stderr);
    if status.success() {
        Ok(out)
    } else {
        let stderr = String::from_utf8_lossy(&err);
        let first = stderr
            .lines()
            .map(str::trim)
            .find(|line| line.starts_with("fatal") || line.starts_with("error"))
            .or_else(|| stderr.lines().next())
            .unwrap_or("git failed")
            .trim();
        Err(ToolError::new(
            ErrorCode::Git,
            format!("git {what}: {first}"),
        ))
    }
}

/// A revision argument the model may pass: never an option, never a range
/// expression with shell or pathspec meaning.
///
/// # Errors
///
/// When it is empty, starts with `-`, or holds other characters.
pub fn check_revision(revision: &str) -> ToolResult<()> {
    let valid = !revision.is_empty()
        && !revision.starts_with('-')
        && revision.len() <= 200
        && revision
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "._/~^@{}-:".contains(c));
    if valid {
        Ok(())
    } else {
        Err(ToolError::invalid(format!(
            "`{revision}` is not a revision"
        )))
    }
}

/// Cap git output for a tool result.
#[must_use]
pub fn capped(bytes: &[u8]) -> (String, bool) {
    if bytes.len() <= OUTPUT_CAP {
        return (String::from_utf8_lossy(bytes).into_owned(), false);
    }
    let mut end = OUTPUT_CAP;
    while end > 0 && !bytes.is_char_boundary_at(end) {
        end -= 1;
    }
    (String::from_utf8_lossy(&bytes[..end]).into_owned(), true)
}

trait CharBoundary {
    fn is_char_boundary_at(&self, index: usize) -> bool;
}

impl CharBoundary for [u8] {
    fn is_char_boundary_at(&self, index: usize) -> bool {
        // A UTF-8 continuation byte is 0b10xx_xxxx.
        self.get(index)
            .is_none_or(|byte| (*byte & 0b1100_0000) != 0b1000_0000)
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::process::Command;
    use std::time::{Duration, Instant};

    use super::{Repo, capped, check_revision, dangerous_key, driver_of, parse_config};
    use crate::error::ErrorCode;
    use crate::sandbox::SandboxConfig;

    fn init(dir: &Path) {
        let status = Command::new("git")
            .current_dir(dir)
            .args(["-c", "init.defaultBranch=main", "init", "-q"])
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .status()
            .expect("git init");
        assert!(status.success());
    }

    fn repo() -> (tempfile::TempDir, std::path::PathBuf, Repo) {
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let top = base.join("repo");
        std::fs::create_dir(&top).expect("repo");
        init(&top);
        let repo = Repo::discover(&top, &SandboxConfig::default())
            .expect("safe")
            .expect("a repository");
        (dir, base, repo)
    }

    #[test]
    fn config_is_parsed_without_git_and_unknown_syntax_is_refused() {
        let entries = parse_config(
            "# c\n[core]\n\tbare = false ; comment\n\tfsmonitor\n[filter \"lfs\"] clean = git-lfs clean -- %f\n[Remote.Origin]\n\turl = \"a;b\"\n",
        )
        .expect("parse");
        let keys: Vec<&str> = entries.iter().map(|entry| entry.key.as_str()).collect();
        assert_eq!(
            keys,
            [
                "core.bare",
                "core.fsmonitor",
                "filter.lfs.clean",
                "remote.origin.url"
            ]
        );
        assert_eq!(entries[0].value.as_deref(), Some("false"));
        assert_eq!(entries[1].value, None);
        assert_eq!(entries[3].value.as_deref(), Some("a;b"));
        for bad in [
            "x = 1\n",
            "[core\n",
            "[core]\n\t1x = 2\n",
            "[core]\n\tname value\n",
        ] {
            assert!(parse_config(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn dangerous_key_families_are_recognised() {
        for key in [
            "filter.x.clean",
            "include.path",
            "includeif.gitdir:/.path",
            "alias.st",
            "credential.helper",
            "protocol.allow",
            "url.x.insteadof",
            "submodule.m.url",
            "gpg.program",
            "gpg.ssh.program",
            "diff.x.textconv",
            "diff.x.command",
            "merge.x.driver",
            "mergetool.x.cmd",
            "core.worktree",
            "core.sshcommand",
            "core.askpass",
            "core.editor",
            "core.pager",
            "core.hookspath",
            "pager.log",
        ] {
            assert!(dangerous_key(key, Some("x")).is_some(), "{key}");
        }
        assert!(dangerous_key("commit.gpgsign", Some("true")).is_some());
        assert!(dangerous_key("commit.gpgsign", Some("false")).is_none());
        assert!(dangerous_key("core.fsmonitor", Some("false")).is_none());
        assert!(
            dangerous_key("core.fsmonitor", None).is_some(),
            "a bare key is true"
        );
        for key in [
            "core.bare",
            "remote.origin.url",
            "branch.main.merge",
            "user.email",
        ] {
            assert!(dangerous_key(key, Some("x")).is_none(), "{key}");
        }
        assert_eq!(
            driver_of("filter.LFS.clean"),
            Some(("filter".to_owned(), "LFS".to_owned()))
        );
        assert_eq!(driver_of("filter.lfs.required"), None);
    }

    /// Layer 3: even a path through git this module did not foresee (here
    /// an alias passed on the command line) runs inside the sandbox.
    #[test]
    fn host_git_runs_sandboxed_without_writes_or_network() {
        if !cfg!(target_os = "macos") || !Path::new("/usr/bin/sandbox-exec").is_file() {
            return;
        }
        let (_dir, base, repo) = repo();
        let marker = base.join("outside-marker");
        let alias = format!("alias.pwn=!touch {}", marker.display());
        assert!(repo.git(repo.top()).run(&["-c", &alias, "pwn"]).is_err());
        assert!(!marker.exists(), "host git wrote outside its sandbox");
        let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("listen");
        let port = listener.local_addr().expect("addr").port();
        let alias = format!("alias.net=!/usr/bin/nc -z -G 2 127.0.0.1 {port}");
        assert!(
            repo.git(repo.top()).run(&["-c", &alias, "net"]).is_err(),
            "host git reached the network"
        );
        let writable = repo
            .git(repo.top())
            .writes(std::slice::from_ref(&base))
            .run(&["-c", &format!("alias.ok=!touch {}", marker.display()), "ok"]);
        assert!(writable.is_ok(), "{writable:?}");
        assert!(marker.exists(), "an allowed write works");
    }

    /// Layer 4: a git call that hangs is killed at its timeout.
    #[test]
    fn host_git_calls_time_out() {
        let (_dir, _base, repo) = repo();
        let started = Instant::now();
        let error = repo
            .git(repo.top())
            .timeout(Duration::from_millis(300))
            .run(&["-c", "alias.slow=!sleep 30", "slow"])
            .expect_err("timeout");
        assert_eq!(error.code(), ErrorCode::Timeout);
        assert!(started.elapsed() < Duration::from_secs(10));
    }

    /// Layer 1 for attributes: a driver the person's own config defines
    /// (git-lfs is the common case) is not run on the agent's files.
    #[test]
    fn attributes_selecting_a_configured_driver_are_refused() {
        let (_dir, base, repo) = repo();
        let marker = base.join("lfs-ran");
        let global = base.join("gitconfig");
        std::fs::write(
            &global,
            format!(
                "[filter \"lfs\"]\n\tclean = touch {} && cat\n\trequired = true\n",
                marker.display()
            ),
        )
        .expect("global");
        let repo = repo.with_global_config(global);
        std::fs::write(repo.top().join("a.bin"), "x").expect("file");
        assert!(
            repo.check_attributes(&[]).is_ok(),
            "no attribute selects it"
        );
        std::fs::write(repo.top().join(".gitattributes"), "*.bin filter=lfs\n").expect("attrs");
        let error = repo.check_attributes(&[]).expect_err("refused");
        assert_eq!(error.code(), ErrorCode::UnsafeRepository);
        assert!(error.message().contains("a.bin"), "{}", error.message());
        assert!(
            repo.git(repo.top()).worktree().run(&["status"]).is_err(),
            "status would have run the clean filter"
        );
        assert!(!marker.exists());
    }

    #[test]
    fn revisions_cannot_be_options() {
        for good in [
            "HEAD",
            "HEAD~2",
            "main",
            "origin/main",
            "v1.2.3",
            "abc123",
            "HEAD@{1}",
            "a..b",
        ] {
            assert!(check_revision(good).is_ok(), "{good}");
        }
        for bad in ["", "--output=/tmp/x", "-p", "a b", "$(x)", "a;b"] {
            assert!(check_revision(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn capping_respects_utf8() {
        let text = "é".repeat(super::OUTPUT_CAP);
        let (out, truncated) = capped(text.as_bytes());
        assert!(truncated);
        assert!(out.len() <= super::OUTPUT_CAP);
        assert!(!out.contains('\u{fffd}'));
    }
}
