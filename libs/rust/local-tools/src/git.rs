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
//!    `gitdir:` file, except a linked worktree exactly as `git worktree
//!    add` registers it, when the host trusts its repository or the
//!    worktree itself (a workspace the person opened), see
//!    [`trust_worktree_parent`], or a task worktree the host created and
//!    recorded itself, see [`register_worktree`]; its repository's `.git`
//!    is then checked as below) at the work tree's top, with no `commondir`,
//!    no alternates, no submodules once `modules/` or a `submodule.*`
//!    section suggests some (a stale `modules/` or section of submodules
//!    since removed is accepted), and a local config (and `config.worktree`)
//!    that this module parses itself and git's own reader lists (reading
//!    config runs no code; includes are not followed): both must agree on
//!    the sections and neither may see a dangerous key family
//!    ([`dangerous_key`]).
//!    Attributes ([`Repo::check_attributes`], before every call that reads
//!    the work tree): when the person's global config defines filter, diff
//!    or merge drivers with commands (the standard git-lfs filter aside),
//!    no path may select one; a global config that cannot be listed
//!    refuses everything, and a passing scan is reused until the paths,
//!    an attributes file, the index or `HEAD` change. A refusal is
//!    [`ErrorCode::UnsafeRepository`], shown to the agent and the person;
//!    checkpoints then fall back to copies.
//! 2. **Override on the command line** ([`HARDENING`]): hooks to
//!    `/dev/null`, no fsmonitor, no pager, no signing or signature checks,
//!    no external diff, no credential helper, `protocol.allow=never`, no
//!    system config or system attributes, no attributes file, no submodule
//!    recursion; per command `--no-ext-diff --no-textconv`,
//!    `--ignore-submodules=all`, `--no-show-signature`, `--no-gpg-sign`;
//!    `GIT_DIR` and `GIT_WORK_TREE` pinned (a linked worktree's
//!    `GIT_COMMON_DIR` too, so git never reads the `commondir` file), and
//!    nothing else of the parent's environment but `PATH`, `HOME`,
//!    `XDG_CONFIG_HOME` (the trusted global config) and `LANG=C`.
//! 3. **Sandbox every git process** (Seatbelt on macOS, bubblewrap or the
//!    Landlock helper on Linux): no network, and writes only where the call
//!    needs them (the temporary index, `objects/`, `refs/elitea/`; the
//!    workspace for a restore). A path this module did not foresee still
//!    runs confined.
//! 4. **Timeouts** on every call, killing the process group.

use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsStr;
use std::io::Write as _;
use std::os::unix::ffi::OsStrExt as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::mpsc;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use sha2::{Digest as _, Sha256};

use crate::deny::DenyList;
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;
use crate::sandbox::{SandboxConfig, SandboxRequest, prepare};

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

fn unsafe_repo(message: impl Into<String>) -> ToolError {
    ToolError::new(ErrorCode::UnsafeRepository, message)
}

/// A refusal with two texts. The model's (`agent`) names paths relative
/// to the repository (`.git/config`), no home folder and no uid, and says
/// to ask the person: its commands cannot change git's own files. The
/// person's (`person`, shown through the host's UI) names the full path,
/// the owner found, and what to change.
fn refused(agent: &str, person: impl Into<String>) -> ToolError {
    unsafe_repo(format!(
        "{agent}. Git's own files are protected from your tools: tell the person, who can change it"
    ))
    .with_person_message(person)
}

/// Refuse the config file at `path` (`label`: its path relative to the
/// repository, `.git/config`) when one of its entries is dangerous, naming
/// the file and the key. `module`: the config of a submodule git directory
/// under `.git/modules`, where `core.worktree` (which git writes into
/// every one, and which runs nothing) passes.
fn refuse_dangerous(
    label: &str,
    path: &Path,
    entries: &[ConfigEntry],
    module: bool,
) -> ToolResult<()> {
    for entry in entries {
        if module && entry.key == "core.worktree" {
            continue;
        }
        if let Some(reason) = dangerous_key(&entry.key, entry.value.as_deref()) {
            let what = format!(
                "sets `{}` ({reason}), which would run code in the host's git",
                entry.key
            );
            let fix = if module {
                "it is a submodule's git directory: if that submodule is gone, delete the folder"
            } else {
                "remove it from that file to use this repository"
            };
            return Err(refused(
                &format!("{label} {what}"),
                format!("{} {what}; {fix}", path.display()),
            ));
        }
    }
    Ok(())
}

/// The sections (with subsections) that `entries` set variables in.
fn config_sections(entries: &[ConfigEntry]) -> BTreeSet<&str> {
    entries
        .iter()
        .filter_map(|entry| entry.key.rsplit_once('.').map(|(section, _)| section))
        .collect()
}

/// One `key = value` of a git config file; keys lowercased except the
/// subsection, as git compares them.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConfigEntry {
    pub key: String,
    pub value: Option<String>,
}

/// Characters of a config file as git's reader sees them: `\r\n` is a
/// newline, and the end of the file reads as a final newline.
struct ConfigReader<'a> {
    chars: std::iter::Peekable<std::str::Chars<'a>>,
    line: usize,
    eof: bool,
}

impl ConfigReader<'_> {
    fn next(&mut self) -> char {
        match self.chars.next() {
            None => {
                self.eof = true;
                '\n'
            }
            Some('\r') if self.chars.peek() == Some(&'\n') => {
                self.chars.next();
                self.line += 1;
                '\n'
            }
            Some('\n') => {
                self.line += 1;
                '\n'
            }
            Some(c) => c,
        }
    }

    fn error(&self, what: &str) -> String {
        format!("line {}: {what}", self.line + 1)
    }
}

fn is_key_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || c == '-'
}

/// Parse a git config file exactly as git's own reader does
/// (`config.c`'s `git_parse_source`), without following includes:
///
/// * a comment (`#` or `;` where a line or a value may hold one, outside
///   quotes) runs to the end of the line, and a backslash in it continues
///   nothing;
/// * a backslash-newline continues a line only inside a value;
/// * values: quotes toggle, `\t` `\n` `\b` `\"` `\\` are the only escapes,
///   leading and trailing unquoted whitespace is dropped;
/// * headers: `[section]`, `[section.sub]` (lowercased) or
///   `[section "sub"]` (case kept, `\x` is `x`), and a variable may follow
///   on the same line.
///
/// Anything git would refuse is an error here too: an unsafe repository,
/// not a guess.
///
/// # Errors
///
/// What git reports as a bad config line.
pub fn parse_config(text: &str) -> Result<Vec<ConfigEntry>, String> {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let mut reader = ConfigReader {
        chars: text.chars().peekable(),
        line: 0,
        eof: false,
    };
    let mut entries = Vec::new();
    let mut section: Option<String> = None;
    let mut comment = false;
    loop {
        let c = reader.next();
        if c == '\n' {
            if reader.eof {
                return Ok(entries);
            }
            comment = false;
            continue;
        }
        if comment || c.is_ascii_whitespace() {
            continue;
        }
        if c == '#' || c == ';' {
            comment = true;
            continue;
        }
        if c == '[' {
            section = Some(parse_section(&mut reader)?);
            continue;
        }
        if !c.is_ascii_alphabetic() {
            return Err(reader.error("not a section, a variable or a comment"));
        }
        let Some(current) = &section else {
            return Err(reader.error("a variable outside a section"));
        };
        let mut name = String::from(c.to_ascii_lowercase());
        let mut c = reader.next();
        while !reader.eof && is_key_char(c) {
            name.push(c.to_ascii_lowercase());
            c = reader.next();
        }
        while c == ' ' || c == '\t' {
            c = reader.next();
        }
        let value = if c == '\n' {
            None
        } else if c == '=' {
            Some(parse_config_value(&mut reader)?)
        } else {
            return Err(reader.error("not a variable"));
        };
        entries.push(ConfigEntry {
            key: format!("{current}.{name}"),
            value,
        });
        if reader.eof {
            return Ok(entries);
        }
    }
}

/// A section header after its `[`: `section`, `section.sub` or
/// `section "sub"`, as `section.sub`.
fn parse_section(reader: &mut ConfigReader<'_>) -> Result<String, String> {
    let mut name = String::new();
    loop {
        let c = reader.next();
        if reader.eof {
            return Err(reader.error("an unterminated section"));
        }
        if c == ']' {
            return if name.is_empty() {
                Err(reader.error("an empty section"))
            } else {
                Ok(name)
            };
        }
        if c.is_ascii_whitespace() {
            if c == '\n' || name.is_empty() || name.contains('.') {
                return Err(reader.error("a bad section"));
            }
            let mut c = c;
            while c.is_ascii_whitespace() {
                if c == '\n' {
                    return Err(reader.error("a bad section"));
                }
                c = reader.next();
            }
            if c != '"' {
                return Err(reader.error("a bad section"));
            }
            name.push('.');
            loop {
                let mut c = reader.next();
                if c == '\n' {
                    return Err(reader.error("an unterminated subsection"));
                }
                if c == '"' {
                    break;
                }
                if c == '\\' {
                    c = reader.next();
                    if c == '\n' {
                        return Err(reader.error("an unterminated subsection"));
                    }
                }
                name.push(c);
            }
            return if reader.next() == ']' {
                Ok(name)
            } else {
                Err(reader.error("a bad section"))
            };
        }
        if !is_key_char(c) && c != '.' {
            return Err(reader.error("a bad section"));
        }
        name.push(c.to_ascii_lowercase());
    }
}

/// A value after its `=`, to the end of its (possibly continued) line.
fn parse_config_value(reader: &mut ConfigReader<'_>) -> Result<String, String> {
    let mut out = String::new();
    let mut quoted = false;
    let mut comment = false;
    let mut spaces = 0usize;
    loop {
        let mut c = reader.next();
        if c == '\n' {
            if quoted {
                return Err(reader.error("an unterminated quote"));
            }
            return Ok(out);
        }
        if comment {
            continue;
        }
        if c.is_ascii_whitespace() && !quoted {
            if !out.is_empty() {
                spaces += 1;
            }
            continue;
        }
        if !quoted && (c == '#' || c == ';') {
            comment = true;
            continue;
        }
        for _ in 0..spaces {
            out.push(' ');
        }
        spaces = 0;
        if c == '\\' {
            c = match reader.next() {
                '\n' => continue,
                't' => '\t',
                'b' => '\u{8}',
                'n' => '\n',
                c @ ('"' | '\\') => c,
                _ => return Err(reader.error("a bad escape")),
            };
            out.push(c);
            continue;
        }
        if c == '"' {
            quoted = !quoted;
            continue;
        }
        out.push(c);
    }
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
/// `includeIf`, `alias`, `credential`, `protocol`, `url`, `gpg`, `pager`),
/// any `*.textconv`, `*.command`, `*.cmd`, `*.driver`, and the single keys
/// that run programs or move the work tree. Signing and signature switches
/// and `core.fsmonitor` pass when they are false.
///
/// Two families pass that once were refused:
///
/// * `core.hooksPath` (husky's `.husky`, or `.git/hooks` spelled out): a
///   hook runs only when git looks in the hooks path, and every host git
///   process gets `-c core.hooksPath=/dev/null` from [`HARDENING`] in
///   [`Git::spawn`], which beats every config file. `Git::spawn` is the
///   only way this crate and the desktop run git on a repository (the one
///   other `git` process, [`valid_ref_name`], reads no repository); the
///   workspace store reads objects with gix's object and pack crates,
///   which run no hooks. So the repository's hooks path is never looked
///   in, whatever it says. The agent's own `git` in its shell is a command
///   like any other: sandboxed, and running the default `.git/hooks` with
///   or without this key.
/// * `submodule.<name>.<key>` for the keys `git submodule init` writes
///   (`url`, `active`, `branch`, `shallow`, `ignore`,
///   `fetchRecurseSubmodules`) and `update` naming a strategy
///   (`checkout`, `rebase`, `merge`, `none`): none of them runs anything
///   by itself, only a `git submodule` command reads them, and the host
///   never runs one (no host call names `submodule`; [`HARDENING`] sets
///   `submodule.recurse=false`). [`Repo::check`] still refuses the
///   section unless the repository has no submodules (left over from
///   submodules since removed). `update = !<command>`, any other value of
///   `update`, any other key and the top-level `submodule.*` keys stay
///   refused.
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
        "submodule" => return submodule_key(name, has_subsection, value),
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

/// Why a `submodule.*` key is refused, if it is (see [`dangerous_key`]).
fn submodule_key(name: &str, has_subsection: bool, value: Option<&str>) -> Option<&'static str> {
    if !has_subsection {
        return Some("a submodule setting");
    }
    match name {
        "url" | "active" | "branch" | "shallow" | "ignore" | "fetchrecursesubmodules" => None,
        "update"
            if value.is_some_and(|value| {
                matches!(
                    value.trim().to_ascii_lowercase().as_str(),
                    "checkout" | "rebase" | "merge" | "none"
                )
            }) =>
        {
            None
        }
        "update" => Some("a submodule update command"),
        _ => Some("a submodule setting"),
    }
}

/// The text of a config file (`label`: its path relative to the
/// repository, for refusals); `None` when there is none.
fn read_config_file(path: &Path, label: &str) -> ToolResult<Option<String>> {
    let meta = match std::fs::symlink_metadata(path) {
        Ok(meta) => meta,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err(unsafe_repo(format!("cannot read {label}"))),
    };
    if !meta.is_file() || meta.len() > MAX_CONFIG_BYTES {
        return Err(unsafe_repo(format!("{label} is not a plain config file")));
    }
    let text = std::fs::read(path).map_err(|_| unsafe_repo(format!("cannot read {label}")))?;
    String::from_utf8(text)
        .map(Some)
        .map_err(|_| unsafe_repo(format!("{label} is not UTF-8")))
}

type WorktreeTrust = Arc<dyn Fn(&Path) -> bool + Send + Sync>;

/// The longest record a submodule look reads from git (a path is at most
/// 4 KiB on the platforms here); a longer one fails the look.
const SCAN_RECORD_CAP: usize = 64 * 1024;

/// See [`Repo::present_submodule`].
#[derive(Debug, Default)]
struct SubmodulesChecked {
    /// [`Repo::index_key`] of an index without a gitlink.
    index: Option<Vec<u8>>,
    /// A `HEAD` commit whose tree has no gitlink.
    head: Option<String>,
}

/// The tops whose linked worktrees host git may use (a repository's) or
/// that host git may use as a linked worktree (a worktree's own): the tops
/// the host registered ([`trust_worktree_parent`]) and its check
/// ([`set_worktree_trust`]).
static WORKTREE_PARENTS: Mutex<BTreeSet<PathBuf>> = Mutex::new(BTreeSet::new());
static WORKTREE_TRUST: Mutex<Option<WorktreeTrust>> = Mutex::new(None);

/// Trust the linked worktrees of the repository whose top is `top`, or the
/// linked worktree whose own top is `top` (a workspace the person opened:
/// a worktree they made with `git worktree add` works on its own, without
/// its repository open). Kept for the life of the process.
pub fn trust_worktree_parent(top: &Path) {
    let top = std::fs::canonicalize(top).unwrap_or_else(|_| top.to_path_buf());
    if let Ok(mut parents) = WORKTREE_PARENTS.lock() {
        parents.insert(top);
    }
}

/// Stop trusting the linked worktrees of the repository whose top is `top`
/// (its workspace was removed). Undoes [`trust_worktree_parent`], whether
/// the folder still exists or not.
pub fn forget_worktree_parent(top: &Path) {
    let canonical = std::fs::canonicalize(top).unwrap_or_else(|_| top.to_path_buf());
    if let Ok(mut parents) = WORKTREE_PARENTS.lock() {
        parents.remove(&canonical);
        parents.remove(top);
    }
}

/// The host's own check of a top (the desktop: "a workspace the person
/// opened"), consulted besides [`trust_worktree_parent`] for a linked
/// worktree's repository top and for the worktree's own top.
pub fn set_worktree_trust(check: impl Fn(&Path) -> bool + Send + Sync + 'static) {
    if let Ok(mut trust) = WORKTREE_TRUST.lock() {
        *trust = Some(Arc::new(check));
    }
}

fn trusted_worktree_parent(top: &Path) -> bool {
    if WORKTREE_PARENTS
        .lock()
        .is_ok_and(|parents| parents.contains(top))
    {
        return true;
    }
    let check = WORKTREE_TRUST.lock().ok().and_then(|trust| trust.clone());
    check.is_some_and(|check| check(top))
}

/// A task worktree the host created (`git worktree add` under its own
/// data, ADR-0029 decision 4) and recorded in its own registry: the
/// repository never declares one. Host git accepts a linked worktree of
/// this shape only when the caller names it ([`Repo::open_registered_worktree`])
/// or the host registered it ([`register_worktree`]).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RegisteredWorktree {
    /// The worktree's folder.
    pub worktree: PathBuf,
    /// The top of the repository it was created in.
    pub top: PathBuf,
    /// Its admin directory's name: `<top>/.git/worktrees/<name>`.
    pub name: String,
}

impl RegisteredWorktree {
    /// The same entry with both paths canonical (as given when one does not
    /// resolve: the check then refuses it).
    #[must_use]
    pub fn canonical(&self) -> Self {
        let resolve =
            |path: &Path| std::fs::canonicalize(path).unwrap_or_else(|_| path.to_path_buf());
        Self {
            worktree: resolve(&self.worktree),
            top: resolve(&self.top),
            name: self.name.clone(),
        }
    }
}

/// The task worktrees the host registered, by canonical folder.
static REGISTERED_WORKTREES: Mutex<BTreeMap<PathBuf, RegisteredWorktree>> =
    Mutex::new(BTreeMap::new());

/// Record a task worktree the host created (at creation, and at start-up
/// from the host's registry file). Kept until [`unregister_worktree`].
pub fn register_worktree(entry: &RegisteredWorktree) {
    let entry = entry.canonical();
    if let Ok(mut registered) = REGISTERED_WORKTREES.lock() {
        registered.insert(entry.worktree.clone(), entry);
    }
}

/// Forget a task worktree (it was removed).
pub fn unregister_worktree(worktree: &Path) {
    let canonical = std::fs::canonicalize(worktree).unwrap_or_else(|_| worktree.to_path_buf());
    if let Ok(mut registered) = REGISTERED_WORKTREES.lock() {
        registered.remove(&canonical);
        registered.remove(worktree);
    }
}

/// The registered task worktree whose folder is `worktree`, if any.
#[must_use]
pub fn registered_worktree(worktree: &Path) -> Option<RegisteredWorktree> {
    let canonical = std::fs::canonicalize(worktree).unwrap_or_else(|_| worktree.to_path_buf());
    REGISTERED_WORKTREES
        .lock()
        .ok()
        .and_then(|registered| registered.get(&canonical).cloned())
}

/// A worktree admin directory name the host gives (`[A-Za-z0-9._-]`, not
/// `.` or `..`, at most 64 bytes).
#[must_use]
pub fn valid_worktree_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name != "."
        && name != ".."
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
}

/// The narrow exception to "`.git` is a directory" for a registered task
/// worktree: its `.git` is a regular file (not a symlink) the person owns,
/// whose one line is `gitdir: <absolute path>`; that path is, canonically,
/// exactly `<top>/.git/worktrees/<name>` of the registered top; the admin
/// directory's `commondir` leads to `<top>/.git` and its `gitdir` back to
/// this `.git`. `(admin dir, <top>/.git)`, both canonical; every other
/// check of [`Repo::check`] then runs on `<top>/.git`. A `.git` file the
/// repository wrote, a pointer anywhere else, a `commondir` elsewhere and a
/// submodule's git directory are all refused.
fn registered_linked_worktree(entry: &RegisteredWorktree) -> ToolResult<(PathBuf, PathBuf)> {
    use std::os::unix::fs::MetadataExt as _;
    let refuse = |why: &str| {
        unsafe_repo(format!(
            "the worktree's .git is not the one the host created ({why})"
        ))
    };
    if !valid_worktree_name(&entry.name) {
        return Err(refuse("not a worktree name the host gives"));
    }
    let dot_git = entry.worktree.join(".git");
    let meta = std::fs::symlink_metadata(&dot_git).map_err(|_| refuse("it is missing"))?;
    if !meta.file_type().is_file() {
        return Err(refuse("not a regular file"));
    }
    let me = rustix::process::geteuid().as_raw();
    if meta.uid() != me {
        return Err(refused(
            "the worktree's .git is not the one the host created (another user owns it)",
            another_owner(&dot_git, meta.uid(), me),
        ));
    }
    if meta.len() > 4096 {
        return Err(refuse("too large"));
    }
    let text = std::fs::read_to_string(&dot_git).map_err(|_| refuse("unreadable"))?;
    let line = text.strip_suffix('\n').unwrap_or(&text);
    let target = line
        .strip_prefix("gitdir: ")
        .filter(|target| !target.is_empty() && !target.contains(['\n', '\r']))
        .ok_or_else(|| refuse("not a single gitdir line"))?;
    let target = Path::new(target);
    if !target.is_absolute() {
        return Err(refuse("a relative gitdir"));
    }
    let top = std::fs::canonicalize(&entry.top).map_err(|_| refuse("its repository is gone"))?;
    let common = top.join(".git");
    if !std::fs::symlink_metadata(&common).is_ok_and(|meta| meta.file_type().is_dir()) {
        return Err(refuse("its repository's .git is not a directory"));
    }
    let expected = common.join("worktrees").join(&entry.name);
    // No component of the expected path may be a symlink: the canonical
    // spelling must be the literal one.
    if std::fs::canonicalize(&expected).ok().as_deref() != Some(expected.as_path()) {
        return Err(refuse("its admin directory is not in its repository"));
    }
    if std::fs::canonicalize(target).ok().as_deref() != Some(expected.as_path()) {
        return Err(refuse(
            "its gitdir points outside its repository's worktrees",
        ));
    }
    let commondir =
        small_text(&expected.join("commondir")).ok_or_else(|| refuse("no commondir"))?;
    let commondir = Path::new(&commondir);
    let commondir = if commondir.is_absolute() {
        commondir.to_path_buf()
    } else {
        expected.join(commondir)
    };
    if std::fs::canonicalize(commondir).ok().as_deref() != Some(common.as_path()) {
        return Err(refuse("its commondir leads elsewhere"));
    }
    let back = small_text(&expected.join("gitdir")).ok_or_else(|| refuse("no gitdir"))?;
    let back = Path::new(&back);
    let back = if back.is_absolute() {
        back.to_path_buf()
    } else {
        expected.join(back)
    };
    if std::fs::canonicalize(back).ok() != std::fs::canonicalize(&dot_git).ok() {
        return Err(refuse("its repository registered another worktree there"));
    }
    Ok((expected, common))
}

/// Why a file another uid owns is refused: the path, the owner found and
/// who is asking, so the person can tell a real foreign owner (`chown` it
/// back, or work in a copy) from a mistake.
fn another_owner(path: &Path, owner: u32, me: u32) -> String {
    format!(
        "{} belongs to another user (uid {owner}; this app runs as uid {me})",
        path.display()
    )
}

/// A small file's text, trimmed (`None`: missing, not a plain file, or
/// larger than 4 KiB).
fn small_text(path: &Path) -> Option<String> {
    let meta = std::fs::symlink_metadata(path).ok()?;
    if !meta.file_type().is_file() || meta.len() > 4096 {
        return None;
    }
    std::fs::read_to_string(path)
        .ok()
        .map(|text| text.trim().to_owned())
}

/// A linked worktree at `top` (its `.git` a `gitdir:` file), accepted only
/// when it is exactly what `git worktree add` registers: `.git` points (canonically) at `<P>/.git/worktrees/<name>`,
/// `<P>/.git` is a plain directory, `<P>` or the worktree's own top is a
/// trusted top ([`trust_worktree_parent`], [`set_worktree_trust`]), and the admin
/// directory's `commondir` leads back to `<P>/.git` and its `gitdir` back to
/// this `.git`. `(admin dir, <P>/.git)`; every other check of
/// [`Repo::check`] then runs on `<P>/.git`.
fn linked_worktree(top: &Path) -> ToolResult<(PathBuf, PathBuf)> {
    let refuse = |why: &str| {
        unsafe_repo(format!(
            "the repository's .git is a file pointing at a git directory elsewhere ({why})"
        ))
    };
    let dot_git = top.join(".git");
    let pointer = small_text(&dot_git).ok_or_else(|| refuse("unreadable"))?;
    let target = pointer
        .strip_prefix("gitdir:")
        .map(str::trim)
        .filter(|target| !target.is_empty() && !target.contains('\n'))
        .ok_or_else(|| refuse("not a gitdir pointer"))?;
    let target = Path::new(target);
    let target = if target.is_absolute() {
        target.to_path_buf()
    } else {
        top.join(target)
    };
    let admin = std::fs::canonicalize(&target).map_err(|_| refuse("its target is missing"))?;
    if !std::fs::symlink_metadata(&admin).is_ok_and(|meta| meta.file_type().is_dir()) {
        return Err(refuse("its target is not a directory"));
    }
    let worktrees = admin.parent().ok_or_else(|| refuse("not a worktree"))?;
    let common = worktrees.parent().ok_or_else(|| refuse("not a worktree"))?;
    let parent_top = common.parent().ok_or_else(|| refuse("not a worktree"))?;
    if worktrees.file_name() != Some(OsStr::new("worktrees"))
        || common.file_name() != Some(OsStr::new(".git"))
    {
        return Err(refuse("a submodule or another kind of git directory"));
    }
    if !std::fs::symlink_metadata(common).is_ok_and(|meta| meta.file_type().is_dir()) {
        return Err(refuse("its repository's .git is not a directory"));
    }
    // The person opened the repository, or this worktree itself (one they
    // made with `git worktree add`, on another branch): every structural
    // check here and of `<P>/.git` still applies either way.
    let own_top = std::fs::canonicalize(top).unwrap_or_else(|_| top.to_path_buf());
    if !trusted_worktree_parent(parent_top) && !trusted_worktree_parent(&own_top) {
        return Err(refuse(
            "a worktree that is not an open workspace, of a repository that is not one either",
        ));
    }
    let commondir = small_text(&admin.join("commondir")).ok_or_else(|| refuse("no commondir"))?;
    let commondir = Path::new(&commondir);
    let commondir = if commondir.is_absolute() {
        commondir.to_path_buf()
    } else {
        admin.join(commondir)
    };
    if std::fs::canonicalize(commondir).ok().as_deref() != Some(common) {
        return Err(refuse("its commondir leads elsewhere"));
    }
    let back = small_text(&admin.join("gitdir")).ok_or_else(|| refuse("no gitdir"))?;
    let back = Path::new(&back);
    let back = if back.is_absolute() {
        back.to_path_buf()
    } else {
        admin.join(back)
    };
    if std::fs::canonicalize(back).ok() != std::fs::canonicalize(&dot_git).ok() {
        return Err(refuse("its repository registered another worktree there"));
    }
    let common = common.to_path_buf();
    Ok((admin, common))
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
                .env_clear()
                .envs(host_git_environment())
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
    /// Where objects, refs and the config live: `git_dir` itself, or for a
    /// linked worktree ([`linked_worktree`]) its repository's `.git`.
    common_dir: PathBuf,
    /// A task worktree the host registered: checked by
    /// [`registered_linked_worktree`] on every call instead of the trust of
    /// its repository.
    registered: Option<RegisteredWorktree>,
    sandbox: SandboxConfig,
    /// What host git may not read or write: the session's deny list.
    deny: Arc<DenyList>,
    /// Tests: a global config file instead of the person's.
    global_config: Option<PathBuf>,
    /// Config texts (`(file name, text)`) git's own reader already agreed
    /// on, so an unchanged config is not listed again on every call.
    verified: Arc<Mutex<Vec<(String, String)>>>,
    /// The [`Repo::attribute_key`] of the last attribute scan that passed:
    /// the same key again needs no `check-attr` over every path.
    attributes_checked: Arc<Mutex<Option<[u8; 32]>>>,
    /// How many full attribute scans ran (tests).
    attribute_scans: Arc<AtomicUsize>,
    /// What the last looks for submodules found none in: the same index
    /// checksum or `HEAD` commit again needs no listing.
    submodules_checked: Arc<Mutex<SubmodulesChecked>>,
}

impl Repo {
    /// The repository whose work tree holds `dir` (canonical), found the
    /// way git looks (the nearest `.git` upwards), and checked. Host git
    /// runs under `sandbox`'s settings and cannot read what `deny` covers
    /// (the session's deny list).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] when one is found but must not be
    /// used; `Ok(None)` when there is none.
    pub fn discover(
        dir: &Path,
        sandbox: &SandboxConfig,
        deny: Arc<DenyList>,
    ) -> ToolResult<Option<Self>> {
        let Some(top) = dir
            .ancestors()
            .find(|candidate| std::fs::symlink_metadata(candidate.join(".git")).is_ok())
        else {
            return Ok(None);
        };
        let dot_git = top.join(".git");
        let mut registered = None;
        let (git_dir, common_dir) =
            if std::fs::symlink_metadata(&dot_git).is_ok_and(|meta| meta.file_type().is_file()) {
                match registered_worktree(top) {
                    Some(entry) => {
                        let dirs = registered_linked_worktree(&entry)?;
                        registered = Some(entry);
                        dirs
                    }
                    None => linked_worktree(top)?,
                }
            } else {
                (dot_git.clone(), dot_git)
            };
        let repo = Self {
            top: top.to_path_buf(),
            git_dir,
            common_dir,
            registered,
            sandbox: sandbox.clone(),
            deny,
            global_config: None,
            verified: Arc::default(),
            attributes_checked: Arc::default(),
            attribute_scans: Arc::default(),
            submodules_checked: Arc::default(),
        };
        repo.check()?;
        Ok(Some(repo))
    }

    /// A git directory the host keeps for a folder that is not a git work
    /// tree (`git_dir` outside the folder, `work_tree` the folder): the
    /// handoff's shadow repository (ADR-0034 decision 5). Checked like any
    /// other repository.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] as [`Self::check`].
    #[cfg(feature = "handoff")]
    pub(crate) fn shadow(
        git_dir: &Path,
        work_tree: &Path,
        sandbox: &SandboxConfig,
        deny: Arc<DenyList>,
    ) -> ToolResult<Self> {
        let repo = Self {
            top: work_tree.to_path_buf(),
            git_dir: git_dir.to_path_buf(),
            common_dir: git_dir.to_path_buf(),
            registered: None,
            sandbox: sandbox.clone(),
            deny,
            global_config: None,
            verified: Arc::default(),
            attributes_checked: Arc::default(),
            attribute_scans: Arc::default(),
            submodules_checked: Arc::default(),
        };
        repo.check()?;
        Ok(repo)
    }

    /// The task worktree `entry` names (its folder, its repository's top,
    /// its admin directory's name: all from the host's own registry, never
    /// discovered), checked as a registered worktree must be (a regular
    /// `.git` file the person owns naming exactly
    /// `<top>/.git/worktrees/<name>`, whose `commondir` and `gitdir` lead
    /// back) and then as every repository is.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] when the worktree is not exactly the
    /// one the host created, or its repository is unsafe.
    pub fn open_registered_worktree(
        entry: &RegisteredWorktree,
        sandbox: &SandboxConfig,
        deny: Arc<DenyList>,
    ) -> ToolResult<Self> {
        let entry = entry.canonical();
        let (git_dir, common_dir) = registered_linked_worktree(&entry)?;
        let repo = Self {
            top: entry.worktree.clone(),
            git_dir,
            common_dir,
            registered: Some(entry),
            sandbox: sandbox.clone(),
            deny,
            global_config: None,
            verified: Arc::default(),
            attributes_checked: Arc::default(),
            attribute_scans: Arc::default(),
            submodules_checked: Arc::default(),
        };
        repo.check()?;
        Ok(repo)
    }

    /// A task worktree of this repository (`entry.top` must be its top),
    /// opened as [`Self::open_registered_worktree`] opens one, with this
    /// repository's sandbox settings, deny list and global config.
    ///
    /// # Errors
    ///
    /// As [`Self::open_registered_worktree`].
    pub fn registered_worktree_repo(&self, entry: &RegisteredWorktree) -> ToolResult<Self> {
        let mut repo = Self::open_registered_worktree(entry, &self.sandbox, self.deny.clone())?;
        if repo.registered.as_ref().map(|entry| &entry.top) != Some(&self.top) {
            return Err(unsafe_repo("the worktree belongs to another repository"));
        }
        repo.global_config.clone_from(&self.global_config);
        Ok(repo)
    }

    /// The host's record of this task worktree, when it is one.
    #[must_use]
    pub fn registered(&self) -> Option<&RegisteredWorktree> {
        self.registered.as_ref()
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

    /// Where the objects, the shared refs and the config are: the git
    /// directory, or a linked worktree's repository `.git`.
    #[must_use]
    pub fn common_dir(&self) -> &Path {
        &self.common_dir
    }

    /// A linked worktree (its `.git` is a `gitdir:` file).
    #[must_use]
    pub fn is_linked_worktree(&self) -> bool {
        self.common_dir != self.git_dir
    }

    /// Layer 1: the repository's shape and config (no process runs).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] with the reason.
    pub fn check(&self) -> ToolResult<()> {
        if self.is_linked_worktree() {
            // The pointer is read again on every call: a worktree whose
            // `.git` now points elsewhere is not the one that was trusted.
            let (admin, common) = match &self.registered {
                Some(entry) => registered_linked_worktree(entry)?,
                None => linked_worktree(&self.top)?,
            };
            if admin != self.git_dir || common != self.common_dir {
                return Err(unsafe_repo(
                    "the worktree's .git now points at another git directory",
                ));
            }
        }
        let meta = std::fs::symlink_metadata(&self.common_dir)
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
            let path = self.common_dir.join(name);
            if std::fs::symlink_metadata(&path).is_ok() {
                return Err(refused(
                    &format!("the repository {reason}: .git/{name}"),
                    format!("the repository {reason}: {}", path.display()),
                ));
            }
        }
        let mut configs = vec![
            (".git/config".to_owned(), self.common_dir.join("config")),
            (
                ".git/config.worktree".to_owned(),
                self.common_dir.join("config.worktree"),
            ),
        ];
        if self.is_linked_worktree() {
            let name = self
                .git_dir
                .file_name()
                .unwrap_or_default()
                .to_string_lossy();
            configs.push((
                format!(".git/worktrees/{name}/config.worktree"),
                self.git_dir.join("config.worktree"),
            ));
        }
        // The first local config that sets `submodule.*`, by label.
        let mut submodule_sections = None;
        for (label, path) in &configs {
            if self.vet_config(label, path, false)? && submodule_sections.is_none() {
                submodule_sections = Some(label.clone());
            }
        }
        self.check_submodules(submodule_sections.as_deref())
    }

    /// Layer 1 for one config file (`label`: its path relative to the
    /// repository; `module`: see [`refuse_dangerous`]): parsed here and
    /// listed by git's own reader, neither with a dangerous key. Whether
    /// it sets `submodule.*`.
    fn vet_config(&self, label: &str, path: &Path, module: bool) -> ToolResult<bool> {
        let Some(text) = read_config_file(path, label)? else {
            return Ok(false);
        };
        let ours = parse_config(&text)
            .map_err(|reason| unsafe_repo(format!("{label} cannot be parsed safely: {reason}")))?;
        refuse_dangerous(label, path, &ours, module)?;
        let submodules = ours.iter().any(|entry| entry.key.starts_with("submodule."));
        if self.verified_config(label, &text) {
            return Ok(submodules);
        }
        // A second opinion from git's own reader (reading config runs no
        // code; includes are not followed): what one parser misreads the
        // other is unlikely to misread the same way.
        let theirs = self.git_config_listing(&text).map_err(|error| {
            unsafe_repo(format!(
                "git cannot read {label} safely: {}",
                error.message()
            ))
        })?;
        refuse_dangerous(label, path, &theirs, module)?;
        if config_sections(&ours) != config_sections(&theirs) {
            return Err(unsafe_repo(format!(
                "{label} reads differently to git and to this check"
            )));
        }
        if let Ok(mut verified) = self.verified.lock() {
            verified.retain(|(file, _)| file != label);
            verified.push((label.to_owned(), text));
        }
        Ok(submodules)
    }

    /// The submodule rule of [`Self::check`]; `submodule_sections`: the
    /// local config (by label) that sets `submodule.*`, if one does.
    fn check_submodules(&self, submodule_sections: Option<&str>) -> ToolResult<()> {
        // Submodule git directories (`.git/modules`) and `submodule.*`
        // sections are accepted only while the repository has no
        // submodules: no `.gitmodules` entry and no gitlink in the index
        // or in `HEAD`'s tree. Then nothing leads git into them: a git
        // directory under `modules/` is used only for a gitlink, and the
        // host also passes `submodule.recurse=false`,
        // `diff.ignoreSubmodules=all` and never runs `git submodule`. That
        // is a clone whose submodules were removed (`git rm` leaves both
        // behind). The agent cannot make one live: its commands cannot
        // write `.git` at any depth (the workspace's protected name and
        // the sandbox's masks), so neither an index entry nor a work-tree
        // `.git` file pointing into `modules/`; a `.gitmodules` it writes
        // only turns this into a refusal. Should git still enter one, the
        // command-line overrides of [`HARDENING`] travel to every git it
        // starts (`GIT_CONFIG_PARAMETERS`), and every config under
        // `modules/` is vetted like the repository's own, `core.worktree`
        // aside (git writes it into each; it runs nothing). Asked only
        // after the repository's config passed, since it runs git.
        let modules = self.common_dir.join("modules");
        let module_dirs =
            std::fs::read_dir(&modules).is_ok_and(|mut entries| entries.next().is_some());
        if module_dirs {
            self.vet_module_configs(&modules)?;
        }
        if !module_dirs && submodule_sections.is_none() {
            return Ok(());
        }
        let found = match self.present_submodule() {
            Ok(None) => return Ok(()),
            Ok(Some(found)) => found,
            // Not knowing is not "none": refused, and nothing cached.
            Err(error) => {
                return Err(unsafe_repo(format!(
                    "whether the repository has submodules could not be checked ({})",
                    error.message()
                )));
            }
        };
        let (agent, person) = if module_dirs {
            (
                format!(
                    "the repository has submodules with their own git directories (.git/modules): {found}"
                ),
                format!(
                    "the repository has submodules with their own git directories ({}): {found}",
                    modules.display()
                ),
            )
        } else {
            let label = submodule_sections.unwrap_or(".git/config");
            let text =
                format!("the repository's {label} configures submodules and it has them: {found}");
            (text.clone(), text)
        };
        Err(refused(&agent, person))
    }

    /// Vet every config file under `.git/modules` (submodule git
    /// directories, nested ones too) as [`Self::vet_config`] does. Git's
    /// object, ref, log and hook folders are not walked; a tree too deep
    /// or too large to walk is refused.
    fn vet_module_configs(&self, modules: &Path) -> ToolResult<()> {
        const MAX_DIRS: usize = 4096;
        const MAX_DEPTH: usize = 16;
        const SKIP: &[&str] = &[
            "objects", "refs", "logs", "hooks", "info", "lfs", "rr-cache",
        ];
        let too_big =
            || unsafe_repo("the repository's .git/modules is too deep or too large to check");
        let mut stack = vec![(modules.to_path_buf(), 0usize)];
        let mut seen = 0usize;
        while let Some((dir, depth)) = stack.pop() {
            seen += 1;
            if seen > MAX_DIRS || depth > MAX_DEPTH {
                return Err(too_big());
            }
            let entries = std::fs::read_dir(&dir).map_err(|_| too_big())?;
            for entry in entries {
                let entry = entry.map_err(|_| too_big())?;
                let path = entry.path();
                let Ok(kind) = entry.file_type() else {
                    return Err(too_big());
                };
                let name = entry.file_name();
                if kind.is_dir() {
                    if !SKIP.iter().any(|skip| name == OsStr::new(skip)) {
                        stack.push((path, depth + 1));
                    }
                } else if name == OsStr::new("config") || name == OsStr::new("config.worktree") {
                    let relative = path.strip_prefix(&self.common_dir).unwrap_or(&path);
                    let label = format!(".git/{}", relative.display());
                    self.vet_config(&label, &path, true)?;
                }
            }
        }
        Ok(())
    }

    /// A submodule the repository has, described (`None`: it has none):
    /// an entry of `.gitmodules`, or a gitlink in the index or in `HEAD`'s
    /// tree. A `.gitmodules` that cannot be read counts as one. An error
    /// when git could not answer (a timeout, a failure, an oversized
    /// record): never `None`, and never cached.
    ///
    /// Cheap on repeat: the index is listed again only when its checksum
    /// (its trailing hash, which a `git status` that changes nothing
    /// leaves alone) changes, and `HEAD`'s tree only when `HEAD` names
    /// another commit; each listing streams and stops at the first
    /// gitlink.
    fn present_submodule(&self) -> ToolResult<Option<String>> {
        let gitmodules = self.top.join(".gitmodules");
        if std::fs::symlink_metadata(&gitmodules).is_ok() {
            match read_config_file(&gitmodules, ".gitmodules")
                .ok()
                .flatten()
                .map(|text| parse_config(&text))
            {
                Some(Ok(entries)) => {
                    if entries
                        .iter()
                        .any(|entry| entry.key.starts_with("submodule."))
                    {
                        return Ok(Some(".gitmodules declares one".to_owned()));
                    }
                }
                _ => {
                    return Ok(Some(
                        ".gitmodules cannot be read (a symlink, too large, not UTF-8 or not a config file), so it may declare one".to_owned(),
                    ));
                }
            }
        }
        // `ls-files --stage` and `ls-tree` both print `<mode> …\t<path>`.
        let gitlink: fn(&[u8]) -> bool = |record| record.starts_with(b"160000 ");
        let path_of = |record: &[u8]| {
            let record = String::from_utf8_lossy(record);
            record
                .split_once('\t')
                .map_or_else(|| record.to_string(), |(_, path)| path.to_owned())
        };
        let index_key = self.index_key();
        let index_known = self
            .submodules_checked
            .lock()
            .is_ok_and(|checked| checked.index.as_ref() == Some(&index_key));
        if !index_known {
            if let Some(record) = self.git(&self.top).scan(
                &["ls-files", "--stage", "-z"],
                gitlink,
                SCAN_RECORD_CAP,
            )? {
                return Ok(Some(format!(
                    "{} is a submodule in the index",
                    path_of(&record)
                )));
            }
            if let Ok(mut checked) = self.submodules_checked.lock() {
                checked.index = Some(index_key);
            }
        }
        let head = if let Some(commit) = self.head_commit() {
            commit
        } else {
            let commit = self.git(&self.top).exit_one_ok().spawn(&[
                "rev-parse",
                "--verify",
                "--quiet",
                "HEAD^{commit}",
            ])?;
            let commit = String::from_utf8_lossy(&commit).trim().to_owned();
            if commit.is_empty() {
                return Ok(None); // no commit yet
            }
            commit
        };
        let head_known = self
            .submodules_checked
            .lock()
            .is_ok_and(|checked| checked.head.as_deref() == Some(head.as_str()));
        if !head_known {
            if let Some(record) = self.git(&self.top).scan(
                &["ls-tree", "-r", "-z", "--full-tree", &head],
                gitlink,
                SCAN_RECORD_CAP,
            )? {
                return Ok(Some(format!("{} is a submodule in HEAD", path_of(&record))));
            }
            if let Ok(mut checked) = self.submodules_checked.lock() {
                checked.head = Some(head);
            }
        }
        Ok(None)
    }

    /// The index's checksum (its last 32 bytes, which hold the trailing
    /// hash of either object format), or its metadata when it has none
    /// (`index.skipHash` writes zeros), or nothing when there is no index.
    fn index_key(&self) -> Vec<u8> {
        use std::io::{Read as _, Seek as _, SeekFrom};
        let path = self.git_dir.join("index");
        let mut tail = [0u8; 32];
        let read = std::fs::File::open(&path).and_then(|mut file| {
            file.seek(SeekFrom::End(-32))?;
            file.read_exact(&mut tail)
        });
        if read.is_ok() && tail[12..].iter().any(|byte| *byte != 0) {
            return tail.to_vec();
        }
        let mut hash = Sha256::new();
        hash_stat(&mut hash, &path);
        hash.finalize().to_vec()
    }

    /// The commit `HEAD` names, read from the files (`None` when it cannot
    /// be read so: unborn, or an unusual ref layout; the caller asks git).
    fn head_commit(&self) -> Option<String> {
        let hex = |text: &str| {
            let text = text.trim();
            (matches!(text.len(), 40 | 64) && text.bytes().all(|b| b.is_ascii_hexdigit()))
                .then(|| text.to_owned())
        };
        let head = small_text(&self.git_dir.join("HEAD"))?;
        let Some(reference) = head.strip_prefix("ref: ") else {
            return hex(&head);
        };
        if !reference.starts_with("refs/")
            || reference
                .split('/')
                .any(|part| part == ".." || part.is_empty())
        {
            return None;
        }
        if let Some(loose) = small_text(&self.common_dir.join(reference)) {
            return hex(&loose);
        }
        let packed = std::fs::read_to_string(self.common_dir.join("packed-refs")).ok()?;
        packed.lines().find_map(|line| {
            let (oid, name) = line.split_once(' ')?;
            (name == reference).then(|| hex(oid)).flatten()
        })
    }

    /// Whether git already agreed on exactly this text of `name`.
    fn verified_config(&self, name: &str, text: &str) -> bool {
        self.verified.lock().is_ok_and(|verified| {
            verified
                .iter()
                .any(|(file, seen)| file == name && seen == text)
        })
    }

    /// `git config --list` over `text` (fed on stdin, so git reads the
    /// bytes this module parsed), without includes.
    fn git_config_listing(&self, text: &str) -> ToolResult<Vec<ConfigEntry>> {
        let listing = self
            .git(&self.top)
            .literal(false)
            .stdin(text.as_bytes())
            .spawn(&[
                "config",
                "--file",
                "-",
                "--no-includes",
                "--list",
                "--null",
                "--show-scope",
            ])?;
        let mut fields = listing.split(|byte| *byte == 0);
        let mut out = Vec::new();
        while let (Some(_scope), Some(item)) = (fields.next(), fields.next()) {
            let item = String::from_utf8_lossy(item);
            let (key, value) = match item.split_once('\n') {
                Some((key, value)) => (key.to_owned(), Some(value.to_owned())),
                None => (item.into_owned(), None),
            };
            out.push(ConfigEntry { key, value });
        }
        Ok(out)
    }

    /// How many full attribute scans ran.
    #[doc(hidden)]
    #[must_use]
    pub fn attribute_scans(&self) -> usize {
        self.attribute_scans.load(Ordering::Relaxed)
    }

    /// The files git reads as the global config.
    fn global_config_files(&self) -> Vec<PathBuf> {
        if let Some(path) = &self.global_config {
            return vec![path.clone()];
        }
        let home = std::env::var_os("HOME").filter(|home| !home.is_empty());
        let xdg = std::env::var_os("XDG_CONFIG_HOME")
            .filter(|xdg| !xdg.is_empty())
            .map(PathBuf::from)
            .or_else(|| home.as_ref().map(|home| Path::new(home).join(".config")));
        home.map(|home| Path::new(&home).join(".gitconfig"))
            .into_iter()
            .chain(xdg.map(|xdg| xdg.join("git/config")))
            .collect()
    }

    /// Drivers with commands in the person's global config:
    /// `(kind, name)` for `filter`, `diff` and `merge`.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] when a global config exists but
    /// cannot be listed: the drivers are unknown, so nothing is assumed
    /// safe.
    fn configured_drivers(&self) -> ToolResult<BTreeSet<(String, String)>> {
        let absent = self.global_config_files().iter().all(|path| {
            std::fs::symlink_metadata(path)
                .is_err_and(|error| error.kind() == std::io::ErrorKind::NotFound)
        });
        if absent {
            return Ok(BTreeSet::new());
        }
        let listing = self
            .git(&self.top)
            .run(&["config", "--global", "--includes", "--list", "-z"])
            .map_err(|error| {
                unsafe_repo(format!(
                    "cannot list the filter, diff and merge drivers in your global git config ({}), so no git call that reads the work tree is safe",
                    error.message()
                ))
            })?;
        let mut commands: BTreeMap<(String, String), Vec<(String, String)>> = BTreeMap::new();
        for item in listing.split(|byte| *byte == 0) {
            let item = String::from_utf8_lossy(item);
            let (key, value) = item.split_once('\n').unwrap_or((&item, ""));
            if let Some(driver) = driver_of(key) {
                let variable = key.rsplit('.').next().unwrap_or_default();
                commands
                    .entry(driver)
                    .or_default()
                    .push((variable.to_ascii_lowercase(), value.to_owned()));
            }
        }
        Ok(commands
            .into_iter()
            .filter(|(driver, commands)| !standard_lfs(driver, commands))
            .map(|(driver, _)| driver)
            .collect())
    }

    /// Layer 1 for attributes: no path in the work tree (nor in `extra`,
    /// top-relative) selects a filter, diff or merge driver that the
    /// person's config gives a command.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::UnsafeRepository`] naming the path and the driver.
    pub fn check_attributes(&self, extra: &[String]) -> ToolResult<()> {
        let drivers = self.configured_drivers()?;
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
        let key = self.attribute_key(&drivers, &paths);
        if self
            .attributes_checked
            .lock()
            .is_ok_and(|checked| checked.as_ref() == Some(&key))
        {
            return Ok(());
        }
        self.attribute_scans.fetch_add(1, Ordering::Relaxed);
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
        for triple in fields.as_chunks::<3>().0 {
            let (path, kind, value) = (&triple[0], &triple[1], &triple[2]);
            if drivers.contains(&(kind.clone(), value.clone())) {
                return Err(unsafe_repo(format!(
                    "`{path}` selects the {kind} driver `{value}`, whose command would run in the host's git"
                )));
            }
        }
        if let Ok(mut checked) = self.attributes_checked.lock() {
            *checked = Some(key);
        }
        Ok(())
    }

    /// What the attribute scan's answer depends on: the drivers, the
    /// paths (tracked, untracked and `extra`), the index and `HEAD` (an
    /// index-only `.gitattributes` applies), and every `.gitattributes`
    /// that could apply to one of the paths (one in each of their
    /// directories, ignored ones included) and `info/attributes`, by
    /// size, times and inode.
    fn attribute_key(&self, drivers: &BTreeSet<(String, String)>, paths: &[u8]) -> [u8; 32] {
        let mut hash = Sha256::new();
        let mut stat = |path: &Path| hash_stat(&mut hash, path);
        let mut dirs = BTreeSet::new();
        dirs.insert(PathBuf::new());
        for path in paths.split(|byte| *byte == 0) {
            let path = Path::new(OsStr::from_bytes(path));
            dirs.extend(path.ancestors().skip(1).map(Path::to_path_buf));
        }
        for dir in &dirs {
            stat(&self.top.join(dir).join(".gitattributes"));
        }
        for name in ["index", "HEAD"] {
            stat(&self.git_dir.join(name));
        }
        for name in ["info/attributes", "packed-refs"] {
            stat(&self.common_dir.join(name));
        }
        if let Ok(head) = std::fs::read_to_string(self.git_dir.join("HEAD")) {
            if let Some(reference) = head.trim().strip_prefix("ref: ") {
                stat(&self.common_dir.join(reference));
            }
            hash.update(head.as_bytes());
        }
        for (kind, name) in drivers {
            hash.update(kind.as_bytes());
            hash.update([0]);
            hash.update(name.as_bytes());
            hash.update([0]);
        }
        hash.update(Sha256::digest(paths));
        hash.finalize().into()
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

    /// What a commit may write: the git directory (a linked worktree's
    /// repository `.git`, which holds its own), except what runs code or
    /// redirects git (hooks, config, info, modules, commondir; a linked
    /// worktree's `commondir`, `gitdir` and `config.worktree`).
    #[must_use]
    pub(crate) fn commit_access(&self) -> (Vec<PathBuf>, Vec<PathBuf>) {
        let mut protected: Vec<PathBuf> = [
            "hooks",
            "config",
            "config.worktree",
            "info",
            "modules",
            "commondir",
        ]
        .iter()
        .map(|name| self.common_dir.join(name))
        .collect();
        if self.is_linked_worktree() {
            protected.extend(
                ["commondir", "gitdir", "config.worktree"]
                    .iter()
                    .map(|name| self.git_dir.join(name)),
            );
        }
        (vec![self.common_dir.clone()], protected)
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
            exit_one_ok: false,
        }
    }
}

/// Feed `path` and its metadata (or its absence) to `hash`.
fn hash_stat(hash: &mut Sha256, path: &Path) {
    use std::os::unix::fs::MetadataExt as _;
    hash.update(path.as_os_str().as_encoded_bytes());
    hash.update([0]);
    match std::fs::symlink_metadata(path) {
        Ok(meta) => {
            for number in [
                meta.len(),
                meta.ino(),
                meta.mode().into(),
                meta.mtime().cast_unsigned(),
                meta.mtime_nsec().cast_unsigned(),
                meta.ctime().cast_unsigned(),
                meta.ctime_nsec().cast_unsigned(),
            ] {
                hash.update(number.to_le_bytes());
            }
        }
        Err(_) => hash.update([0xff]),
    }
}

/// git-lfs as `git lfs install` writes it: a filter whose only commands
/// are exactly `git-lfs clean -- %f`, `git-lfs smudge -- %f` and
/// `git-lfs filter-process`. Trusted (it comes from the person's global
/// config, and git runs it sandboxed without the network), so LFS
/// repositories are not refused.
fn standard_lfs(driver: &(String, String), commands: &[(String, String)]) -> bool {
    driver.0 == "filter"
        && driver.1 == "lfs"
        && commands.iter().all(|(variable, value)| {
            matches!(
                (variable.as_str(), value.as_str()),
                ("clean", "git-lfs clean -- %f")
                    | ("smudge", "git-lfs smudge -- %f")
                    | ("process", "git-lfs filter-process")
            )
        })
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
    /// Exit status 1 is an answer, not a failure (`merge-tree`'s
    /// "conflicts").
    exit_one_ok: bool,
}

impl<'a> Git<'a> {
    /// Take exit status 1 as success (`merge-tree --write-tree` reports
    /// conflicts so).
    pub(crate) fn exit_one_ok(mut self) -> Self {
        self.exit_one_ok = true;
        self
    }

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

    /// Another deadline (the handoff's pack of a large folder).
    #[cfg(any(test, feature = "handoff"))]
    pub(crate) fn timeout(mut self, timeout: Duration) -> Self {
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

    /// What this git process may write and may not read: its writable
    /// paths, and the session's deny list.
    pub(crate) fn sandbox_request(&self) -> SandboxRequest {
        SandboxRequest {
            writable_roots: self.writable.clone(),
            protected: self.protected.clone(),
            deny_paths: self.repo.deny.paths().to_vec(),
            // A task worktree lives in the host's data, which the deny list
            // covers: its own folder is readable again (never a sibling's).
            readable_roots: self
                .repo
                .registered
                .as_ref()
                .map(|entry| vec![entry.worktree.clone()])
                .unwrap_or_default(),
            ..SandboxRequest::new(
                if self.writable.is_empty() {
                    SandboxMode::ReadOnly
                } else {
                    SandboxMode::WorkspaceWrite
                },
                false,
            )
        }
    }

    fn spawn(&self, args: &[&str]) -> ToolResult<Vec<u8>> {
        let what = args.first().copied().unwrap_or_default();
        let command = self.command(args)?;
        run_bounded(command, self.stdin, self.timeout, what, self.exit_one_ok)
    }

    /// Run `git <args>` and read its NUL-separated records as they come,
    /// with no output cap: the first record `hit` accepts (git is then
    /// stopped), or `None` once git has exited successfully and every
    /// record was read. A timeout, a failure, an unreadable pipe or a
    /// record longer than `record_cap` is an error, never `None`.
    pub(crate) fn scan(
        &self,
        args: &[&str],
        hit: fn(&[u8]) -> bool,
        record_cap: usize,
    ) -> ToolResult<Option<Vec<u8>>> {
        use std::io::{BufRead as _, Read as _};
        use std::os::unix::process::CommandExt as _;
        let what = args.first().copied().unwrap_or_default();
        let mut command = self.command(args)?;
        command
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
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
        let stop = |child: &mut std::process::Child| {
            if let Some(group) = group {
                let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
            }
            let _ = child.kill();
            let _ = child.wait();
        };
        let Some(stdout) = child.stdout.take() else {
            stop(&mut child);
            return Err(ToolError::new(
                ErrorCode::Git,
                format!("git {what}: no output"),
            ));
        };
        let (sender, receiver) = mpsc::channel();
        std::thread::spawn(move || {
            let mut reader = std::io::BufReader::new(stdout);
            let mut record = Vec::new();
            let answer = loop {
                record.clear();
                let limit = u64::try_from(record_cap).unwrap_or(u64::MAX) + 1;
                match reader.by_ref().take(limit).read_until(0, &mut record) {
                    Ok(0) => break Ok(None),
                    Ok(_) => {
                        if record.last() == Some(&0) {
                            record.pop();
                        } else if record.len() > record_cap {
                            break Err("a record too long");
                        }
                        if hit(&record) {
                            break Ok(Some(record));
                        }
                    }
                    Err(_) => break Err("its output could not be read"),
                }
            };
            let _ = sender.send(answer);
        });
        let deadline = Instant::now() + self.timeout;
        let timed_out = || {
            ToolError::new(
                ErrorCode::Timeout,
                format!("git {what} did not finish in {}s", self.timeout.as_secs()),
            )
        };
        let Ok(answer) = receiver.recv_timeout(deadline.saturating_duration_since(Instant::now()))
        else {
            stop(&mut child);
            return Err(timed_out());
        };
        match answer {
            Ok(Some(record)) => {
                stop(&mut child);
                Ok(Some(record))
            }
            Err(why) => {
                stop(&mut child);
                Err(ToolError::new(ErrorCode::Git, format!("git {what}: {why}")))
            }
            Ok(None) => loop {
                match child.try_wait() {
                    Ok(Some(status)) if status.success() => break Ok(None),
                    Ok(Some(_)) | Err(_) => {
                        stop(&mut child);
                        break Err(ToolError::new(ErrorCode::Git, format!("git {what} failed")));
                    }
                    Ok(None) if Instant::now() < deadline => {
                        std::thread::sleep(Duration::from_millis(5));
                    }
                    Ok(None) => {
                        stop(&mut child);
                        break Err(timed_out());
                    }
                }
            },
        }
    }

    /// The hardened, sandboxed, environment-cleared `git <args>` command.
    fn command(&self, args: &[&str]) -> ToolResult<Command> {
        let mut words = vec![git_binary()?.display().to_string()];
        words.extend(HARDENING.iter().map(|arg| (*arg).to_owned()));
        if self.literal {
            words.push("--literal-pathspecs".to_owned());
        }
        words.extend(args.iter().map(|arg| (*arg).to_owned()));
        let request = self.sandbox_request();
        // The host's own git: run it under whatever confinement exists
        // (layers 1 and 2 already hold without one).
        let config = SandboxConfig {
            allow_unenforced: true,
            allow_partial: true,
            ..self.repo.sandbox.clone()
        };
        let prepared = prepare(&request, &words, &config)?;
        let mut command = Command::new(&prepared.program);
        command
            .current_dir(self.cwd)
            .args(&prepared.args)
            .env_clear()
            .envs(host_git_environment());
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
        if self.repo.is_linked_worktree() {
            // git reads `commondir` only when this is unset: the shared
            // directory is the one the check resolved, never what the
            // file says now.
            command.env("GIT_COMMON_DIR", &self.repo.common_dir);
        }
        if let Some(global) = &self.repo.global_config {
            command.env("GIT_CONFIG_GLOBAL", global);
        }
        for (name, value) in &self.env {
            command.env(name, value);
        }
        Ok(command)
    }

    /// Run and return trimmed UTF-8 text.
    pub(crate) fn text(&self, args: &[&str]) -> ToolResult<String> {
        Ok(String::from_utf8_lossy(&self.run(args)?)
            .trim_end()
            .to_owned())
    }
}

/// What the host's git inherits: nothing of the parent environment but an
/// allowlist. `PATH` with its absolute entries only; `HOME` and
/// `XDG_CONFIG_HOME` because the person's global config (identity,
/// `core.excludesFile`, the drivers [`Repo::check`] vetted) is found
/// through them and is trusted; `LANG=C`. Everything else (`GIT_*`,
/// `SSH_*`, `EDITOR`, `PAGER`, a proxy, a custom variable) stays behind,
/// and [`Git::spawn`] pins the git variables it needs after this.
fn host_git_environment() -> Vec<(&'static str, std::ffi::OsString)> {
    let mut env = vec![("LANG", "C".into())];
    if let Ok(path) = std::env::join_paths(crate::command::search_path()) {
        env.push(("PATH", path));
    }
    for name in ["HOME", "XDG_CONFIG_HOME"] {
        if let Some(value) = std::env::var_os(name).filter(|value| !value.is_empty()) {
            env.push((name, value));
        }
    }
    env
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
    exit_one_ok: bool,
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
    if status.success() || (exit_one_ok && status.code() == Some(1)) {
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

/// A revision argument the model may pass: a commit, never an option and
/// never a tree-ish (`HEAD:secrets` would name a path outside the
/// `path_deny` exclusions), a reflog entry (`@{…}`) or a search.
///
/// Accepted: `A`, `A..B` and `A...B`, where each side is a base with any
/// number of `~n` / `^n` suffixes, and a base is `HEAD`, a hexadecimal
/// object id (4 to 64 digits) or a branch or tag name that
/// `git check-ref-format --allow-onelevel` accepts.
///
/// # Errors
///
/// Anything else.
pub fn check_revision(revision: &str) -> ToolResult<()> {
    let invalid = || ToolError::invalid(format!("`{revision}` is not a revision"));
    if revision.is_empty() || revision.len() > 200 {
        return Err(invalid());
    }
    let sides = match revision.split_once("...") {
        Some((from, to)) => vec![from, to],
        None => match revision.split_once("..") {
            Some((from, to)) => vec![from, to],
            None => vec![revision],
        },
    };
    if sides.iter().all(|side| commitish(side)) {
        Ok(())
    } else {
        Err(invalid())
    }
}

/// One side of a revision range: a base and its `~n` / `^n` suffixes.
fn commitish(text: &str) -> bool {
    let (base, suffixes) = text.split_at(text.find(['~', '^']).unwrap_or(text.len()));
    let suffixes_ok = suffixes
        .split(['~', '^'])
        .skip(1)
        .all(|digits| digits.len() <= 6 && digits.chars().all(|c| c.is_ascii_digit()));
    if !suffixes_ok || base.is_empty() || base.starts_with('-') {
        return false;
    }
    if base == "HEAD"
        || ((4..=64).contains(&base.len()) && base.chars().all(|c| c.is_ascii_hexdigit()))
    {
        return true;
    }
    base.chars()
        .all(|c| c.is_ascii_alphanumeric() || "._/-".contains(c))
        && valid_ref_name(base)
}

/// `git check-ref-format --allow-onelevel <name>` (reads no repository and
/// no config).
fn valid_ref_name(name: &str) -> bool {
    let Ok(git) = git_binary() else {
        return false;
    };
    Command::new(git)
        .args(["check-ref-format", "--allow-onelevel", name])
        .current_dir("/")
        .env_clear()
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("GIT_CEILING_DIRECTORIES", "/")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .is_ok_and(|status| status.success())
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

/// The longest branch name [`parse_status_summary`] keeps.
const MAX_SUMMARY_BRANCH: usize = 100;

/// What `git status --porcelain=v1 --branch` says in two facts: the current
/// branch and how many paths changed. For a host's note to the model, not
/// a tool result: nothing else of the output (no path) is kept.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct StatusSummary {
    /// `None` when detached, or when the name is not one git itself would
    /// take (so the note never carries arbitrary text from the repository).
    pub branch: Option<String>,
    /// Changed, staged and untracked paths (one per status line).
    pub changed: usize,
}

/// Parse `git status --porcelain=v1 --branch` output (no `-z`; a path with
/// a newline is quoted by git, so one entry is one line).
#[must_use]
pub fn parse_status_summary(output: &[u8]) -> StatusSummary {
    let text = String::from_utf8_lossy(output);
    let mut summary = StatusSummary::default();
    for line in text.lines() {
        if let Some(header) = line.strip_prefix("## ") {
            let name = header
                .strip_prefix("No commits yet on ")
                .or_else(|| header.strip_prefix("Initial commit on "))
                .unwrap_or(header);
            let name = name.split("...").next().unwrap_or_default();
            let name = name.split(" [").next().unwrap_or_default().trim();
            summary.branch = plain_branch_name(name).then(|| name.to_owned());
        } else if !line.trim().is_empty() {
            summary.changed += 1;
        }
    }
    summary
}

/// A branch name made only of the characters ordinary names use, at most
/// [`MAX_SUMMARY_BRANCH`] bytes; `HEAD (no branch)` (detached) is not one.
fn plain_branch_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= MAX_SUMMARY_BRANCH
        && !name.starts_with('-')
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"._/-+#@".contains(&byte))
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

    use std::sync::Arc;

    use super::{
        Repo, StatusSummary, capped, check_revision, dangerous_key, driver_of, parse_config,
        parse_status_summary,
    };
    use crate::deny::DenyList;
    use crate::error::ErrorCode;
    use crate::sandbox::SandboxConfig;

    #[test]
    fn a_status_summary_keeps_the_branch_and_the_count_only() {
        let summary = |text: &str| parse_status_summary(text.as_bytes());
        assert_eq!(
            summary("## main...origin/main [ahead 1]\n M src/a.rs\n?? new.txt\nR  old -> new\n"),
            StatusSummary {
                branch: Some("main".into()),
                changed: 3
            }
        );
        assert_eq!(summary("## feat/x-1\n").branch.as_deref(), Some("feat/x-1"));
        assert_eq!(
            summary("## No commits yet on trunk\n").branch.as_deref(),
            Some("trunk")
        );
        // Detached, or a name no ordinary branch has: no branch is reported.
        assert_eq!(summary("## HEAD (no branch)\n M a\n").branch, None);
        assert_eq!(summary("## ignore\tall\n").branch, None);
        assert_eq!(summary(&format!("## {}\n", "b".repeat(101))).branch, None);
        assert_eq!(summary("").changed, 0);
    }

    /// Test git, isolated from the machine's config (no global or system
    /// file, a fixed identity, no hooks).
    fn git(dir: &Path, args: &[&str]) {
        let status = Command::new("git")
            .current_dir(dir)
            .args(["-c", "user.name=t", "-c", "user.email=t@example.com"])
            .args([
                "-c",
                "init.defaultBranch=main",
                "-c",
                "core.hooksPath=/dev/null",
            ])
            .args(args)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .status()
            .expect("git");
        assert!(status.success(), "git {args:?}");
    }

    fn init(dir: &Path) {
        git(dir, &["init", "-q"]);
    }

    fn discover(dir: &Path) -> crate::error::ToolResult<Option<Repo>> {
        Repo::discover(
            dir,
            &SandboxConfig::default(),
            Arc::new(DenyList::default()),
        )
    }

    fn repo() -> (tempfile::TempDir, std::path::PathBuf, Repo) {
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let top = base.join("repo");
        std::fs::create_dir(&top).expect("repo");
        init(&top);
        let repo = discover(&top).expect("safe").expect("a repository");
        (dir, base, repo)
    }

    /// The owner's report: a repository they cloned themselves was refused
    /// as "another user's". Nothing about ownership refuses a repository the
    /// person owns: not a path through a symlinked parent (macOS `/var` →
    /// `/private/var`, `/tmp`), and not a linked worktree of it, reached
    /// either way.
    #[test]
    fn a_repository_the_person_owns_opens_through_a_symlinked_parent_and_from_its_worktree() {
        let (_dir, base, _repo) = repo();
        let top = base.join("repo");
        std::fs::write(top.join("a.txt"), "a").expect("write");
        git(&top, &["add", "a.txt"]);
        git(&top, &["commit", "-q", "-m", "a"]);
        let link = base.join("link");
        std::os::unix::fs::symlink(&base, &link).expect("symlink");

        let through_link = discover(&link.join("repo"))
            .expect("trusted")
            .expect("found");
        assert!(!through_link.is_linked_worktree());

        git(&top, &["worktree", "add", "-q", "-b", "task", "../wt"]);
        super::trust_worktree_parent(&top);
        for worktree in [base.join("wt"), link.join("wt")] {
            let repo = discover(&worktree).expect("trusted").expect("found");
            assert!(repo.is_linked_worktree());
            assert_eq!(repo.common_dir(), top.join(".git"));
        }
    }

    /// When a repository is refused, the person is told the full path and
    /// how to clear it; the model gets the path relative to the repository,
    /// no home folder, and is told to ask the person (it cannot change
    /// `.git`). Never a guess about who owns it.
    #[test]
    fn refusals_name_the_path_for_the_person_and_a_relative_one_for_the_model() {
        let (_dir, base, _repo) = repo();
        let top = base.join("repo");
        git(&top, &["config", "core.sshCommand", "touch x"]);
        let error = discover(&top).expect_err("refused");
        assert_eq!(error.code(), ErrorCode::UnsafeRepository);
        let config = top.join(".git/config");
        let person = error.person_message();
        assert!(
            person.contains(&config.display().to_string())
                && person.contains("`core.sshcommand`")
                && person.contains("remove it from that file"),
            "{person}"
        );
        let agent = error.message();
        assert!(
            agent.starts_with(".git/config sets `core.sshcommand`")
                && agent.contains("tell the person")
                && !agent.contains(&base.display().to_string())
                && !agent.contains("remove it"),
            "{agent}"
        );
        assert!(!person.contains("another user") && !agent.contains("another user"));
        git(&top, &["config", "--unset", "core.sshCommand"]);
        discover(&top).expect("trusted again").expect("found");

        // A `submodule.*` section names the file it came from.
        git(&top, &["config", "extensions.worktreeConfig", "true"]);
        git(
            &top,
            &["config", "--worktree", "submodule.sub.url", "../sub"],
        );
        std::fs::write(
            top.join(".gitmodules"),
            "[submodule \"sub\"]\n\tpath = sub\n",
        )
        .expect("gitmodules");
        let error = discover(&top).expect_err("refused");
        assert!(
            error
                .message()
                .contains(".git/config.worktree configures submodules"),
            "{}",
            error.message()
        );
    }

    /// A gitlink at `path` in the index of `top` (a submodule as `git
    /// submodule add` leaves it, without fetching anything).
    fn add_gitlink(top: &Path, path: &str) {
        git(
            top,
            &[
                "update-index",
                "--add",
                "--cacheinfo",
                &format!("160000,{},{path}", "1".repeat(40)),
            ],
        );
    }

    /// The owner's clone as it is: `.git/modules` and `[submodule "…"]`
    /// sections left behind by submodules since removed, and
    /// `core.hooksPath` naming `.git/hooks`. Nothing there can run in host
    /// git, so it is accepted; the hooks path never runs (the marker its
    /// hooks would write stays absent through the host's own calls).
    #[test]
    fn a_clone_with_stale_submodule_leftovers_and_a_hooks_path_is_accepted() {
        let (_dir, base, _repo) = repo();
        let top = base.join("repo");
        std::fs::write(top.join("a.txt"), "a").expect("write");
        git(&top, &["add", "a.txt"]);
        git(&top, &["commit", "-q", "-m", "a"]);
        let marker = base.join("ran");
        let module = top.join(".git/modules/apps/old-ui");
        std::fs::create_dir_all(&module).expect("modules");
        // What git writes into an absorbed submodule's git directory.
        std::fs::write(
            module.join("config"),
            "[core]\n\tbare = false\n\tworktree = ../../../../apps/old-ui\n[remote \"origin\"]\n\turl = https://example.com/old-ui.git\n",
        )
        .expect("module config");
        for (key, value) in [
            (
                "submodule.apps/old-ui.url",
                "https://example.com/old-ui.git",
            ),
            ("submodule.apps/old-ui.active", "true"),
            ("submodule.apps/old-ui.update", "rebase"),
        ] {
            git(&top, &["config", key, value]);
        }
        let hooks = top.join(".git/hooks");
        git(
            &top,
            &["config", "core.hooksPath", &hooks.display().to_string()],
        );
        for hook in ["reference-transaction", "post-index-change", "pre-commit"] {
            let path = hooks.join(hook);
            std::fs::write(&path, format!("#!/bin/sh\ntouch {}\n", marker.display()))
                .expect("hook");
            std::fs::set_permissions(&path, std::os::unix::fs::PermissionsExt::from_mode(0o755))
                .expect("chmod");
        }
        let repo = discover(&top).expect("trusted").expect("found");
        // The check is cached until the index or HEAD changes: twice is
        // the same answer.
        repo.check().expect("still trusted");
        let dot_git = top.join(".git");
        std::fs::write(top.join("a.txt"), "b").expect("change");
        repo.git(&top)
            .writes(std::slice::from_ref(&dot_git))
            .run(&["add", "a.txt"])
            .expect("index write");
        repo.git(&top)
            .writes(std::slice::from_ref(&dot_git))
            .run(&["update-ref", "refs/elitea/probe", "HEAD"])
            .expect("ref update");
        assert!(!marker.exists(), "a repository hook ran");
    }

    /// A stale submodule git directory whose config would run code is
    /// refused (git could still be led into it by a `.git` file in the work
    /// tree, which the agent cannot write but the person might keep), and
    /// so is a nested one.
    #[test]
    fn a_stale_module_config_that_runs_code_is_refused() {
        for module in ["old", "old/modules/nested"] {
            let (_dir, base, _repo) = repo();
            let top = base.join("repo");
            let dir = top.join(".git/modules").join(module);
            std::fs::create_dir_all(dir.join("objects")).expect("module");
            std::fs::write(dir.join("config"), "[core]\n\tfsmonitor = touch x\n").expect("config");
            let error = discover(&top).expect_err("refused");
            assert!(
                error.message().starts_with(&format!(
                    ".git/modules/{module}/config sets `core.fsmonitor`"
                )),
                "{}",
                error.message()
            );
            assert!(error.person_message().contains("delete the folder"));
        }
    }

    /// Not knowing whether there is a submodule is a refusal, never "none",
    /// and it is not remembered: a broken index refuses, the repaired one
    /// is accepted.
    #[test]
    fn a_submodule_look_that_fails_refuses_and_is_not_cached() {
        let (_dir, base, _repo) = repo();
        let top = base.join("repo");
        std::fs::write(top.join("a.txt"), "a").expect("write");
        git(&top, &["add", "a.txt"]);
        git(&top, &["commit", "-q", "-m", "a"]);
        std::fs::create_dir_all(top.join(".git/modules/old")).expect("modules");
        let index = top.join(".git/index");
        let good = std::fs::read(&index).expect("index");
        let repo = discover(&top).expect("trusted").expect("found");
        std::fs::write(
            &index,
            b"DIRC not an index at all, but long enough to have a tail",
        )
        .expect("corrupt");
        let error = repo.check().expect_err("refused");
        assert_eq!(error.code(), ErrorCode::UnsafeRepository);
        assert!(
            error.message().contains("could not be checked"),
            "{}",
            error.message()
        );
        std::fs::write(&index, &good).expect("repair");
        repo.check().expect("accepted again");
    }

    /// The streaming look fails closed: a record longer than its cap, or
    /// a listing that outlives its timeout, is an error, never "none".
    #[test]
    fn a_scan_fails_closed_on_a_long_record_or_a_timeout() {
        let (_dir, base, _repo) = repo();
        let top = base.join("repo");
        for name in ["a-file-with-a-long-name.txt", "b.txt"] {
            std::fs::write(top.join(name), "x").expect("write");
        }
        git(&top, &["add", "-A"]);
        let repo = discover(&top).expect("trusted").expect("found");
        let never = |_: &[u8]| false;
        assert_eq!(
            repo.git(&top)
                .scan(&["ls-files", "--stage", "-z"], never, 4096)
                .expect("listed"),
            None
        );
        let error = repo
            .git(&top)
            .scan(&["ls-files", "--stage", "-z"], never, 16)
            .expect_err("a record is longer than 16 bytes");
        assert_eq!(error.code(), ErrorCode::Git);
        let error = repo
            .git(&top)
            .timeout(Duration::ZERO)
            .scan(&["ls-files", "--stage", "-z"], never, 4096)
            .expect_err("no time at all");
        assert_eq!(error.code(), ErrorCode::Timeout);
        let first = repo
            .git(&top)
            .scan(&["ls-files", "--stage", "-z"], |_| true, 4096)
            .expect("listed")
            .expect("a record");
        assert!(first.ends_with(b"a-file-with-a-long-name.txt"));
    }

    /// A submodule the repository really has keeps it refused when its git
    /// directory or its config section is there.
    #[test]
    fn a_repository_with_a_real_submodule_is_refused() {
        // In the index, with its git directory.
        let (_dir, base, _repo) = repo();
        let top = base.join("repo");
        let modules = top.join(".git/modules");
        std::fs::create_dir_all(modules.join("sub")).expect("modules");
        add_gitlink(&top, "sub");
        let error = discover(&top).expect_err("refused");
        assert_eq!(error.code(), ErrorCode::UnsafeRepository);
        assert!(
            error
                .person_message()
                .contains(&modules.display().to_string())
                && error.message().contains("(.git/modules)")
                && error.message().contains("sub is a submodule in the index"),
            "{}",
            error.person_message()
        );

        // Committed, then gone from the index: HEAD still has it.
        git(&top, &["commit", "-q", "-m", "sub"]);
        git(&top, &["rm", "-q", "--cached", "sub"]);
        let error = discover(&top).expect_err("refused");
        assert!(
            error.message().contains("sub is a submodule in HEAD"),
            "{}",
            error.message()
        );

        // Declared in .gitmodules, configured, no git directory.
        let (_dir, base, _repo) = repo();
        let top = base.join("repo");
        std::fs::write(
            top.join(".gitmodules"),
            "[submodule \"sub\"]\n\tpath = sub\n\turl = ../sub\n",
        )
        .expect("gitmodules");
        git(&top, &["config", "submodule.sub.url", "../sub"]);
        let error = discover(&top).expect_err("refused");
        assert!(
            error.message().contains(".gitmodules declares one"),
            "{}",
            error.message()
        );
        // Declared but never initialised (no section, no git directory):
        // nothing leads host git into it, as before.
        git(&top, &["config", "--remove-section", "submodule.sub"]);
        discover(&top).expect("trusted").expect("found");
    }

    /// `submodule.<name>.update = !<command>` (and every submodule key
    /// beyond the ones `git submodule init` writes) stays refused, stale
    /// or not.
    #[test]
    fn a_submodule_update_command_is_refused() {
        for (key, value) in [
            ("submodule.old.update", "!touch x"),
            ("submodule.old.update", "anything-else"),
            ("submodule.old.foo", "x"),
            ("submodule.recurse", "true"),
        ] {
            let (_dir, base, _repo) = repo();
            let top = base.join("repo");
            git(&top, &["config", key, value]);
            let error = discover(&top).expect_err("refused");
            assert_eq!(error.code(), ErrorCode::UnsafeRepository, "{key}");
            assert!(
                error.message().contains("submodule"),
                "{key}: {}",
                error.message()
            );
        }
    }

    /// A worktree `.git` another uid owns is still refused (the
    /// CVE-2022-24765 protection), and the refusal names the path, the
    /// owner found and who is asking.
    #[test]
    fn another_owner_names_the_path_and_both_uids() {
        let text = super::another_owner(Path::new("/work/wt/.git"), 0, 501);
        assert_eq!(
            text,
            "/work/wt/.git belongs to another user (uid 0; this app runs as uid 501)"
        );
    }

    /// Host git runs under the session's deny list (the host's own
    /// directories, the app's data, the home the session names), not one
    /// recomputed from the environment's `HOME`.
    #[test]
    fn host_git_runs_under_the_sessions_deny_list() {
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let top = base.join("repo");
        std::fs::create_dir(&top).expect("repo");
        init(&top);
        let host_dir = base.join("app-config");
        let home = base.join("home");
        let deny = Arc::new(DenyList::for_session(
            Some(&home),
            Some("com.example.app"),
            true,
            [host_dir.clone()],
        ));
        let repo = Repo::discover(&top, &SandboxConfig::default(), deny)
            .expect("safe")
            .expect("a repository");
        let request = repo.git(&top).sandbox_request();
        for path in [
            host_dir,
            home.join(".ssh"),
            home.join("Library/Application Support/com.example.app"),
        ] {
            assert!(
                request.deny_paths.contains(&path),
                "{} is not denied to host git: {:?}",
                path.display(),
                request.deny_paths
            );
        }
        if let Some(env_home) = std::env::var_os("HOME").filter(|h| !h.is_empty()) {
            let env_ssh = std::path::Path::new(&env_home).join(".ssh");
            assert!(
                !request.deny_paths.contains(&env_ssh) || env_ssh.starts_with(&home),
                "the list is the session's, not recomputed from HOME"
            );
        }
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

    /// A backslash ends nothing inside a comment: the line after it is a
    /// header git reads, not part of the comment.
    #[test]
    fn config_comments_never_continue_and_values_follow_git() {
        let entries = parse_config(
            "[core]\n\t# x \\\n[filter \"evil\"]\n\tclean = touch m\n\tv = \"a # b\" ; c \\\n\tw = 1\\\n2  x \n",
        )
        .expect("parse");
        let pairs: Vec<(&str, Option<&str>)> = entries
            .iter()
            .map(|entry| (entry.key.as_str(), entry.value.as_deref()))
            .collect();
        assert_eq!(
            pairs,
            [
                ("filter.evil.clean", Some("touch m")),
                ("filter.evil.v", Some("a # b")),
                ("filter.evil.w", Some("12  x")),
            ]
        );
        for bad in [
            "[core]\n\tbare # c\n",
            "[core]\n\tv = \"open\n",
            "[core]\n\tv = \\q\n",
            "[c \"sub\n\"]\n",
        ] {
            assert!(parse_config(bad).is_err(), "{bad:?}");
        }
    }

    /// The comment-continuation exploit: a filter section hidden from a
    /// parser that folds `\` inside comments. Both parsers see it; the
    /// repository is refused and the filter never runs.
    #[test]
    fn a_filter_hidden_behind_a_comment_backslash_is_refused() {
        let (_dir, base, repo) = repo();
        let marker = base.join("payload-ran");
        let config = repo.git_dir().join("config");
        let text = std::fs::read_to_string(&config).expect("config");
        std::fs::write(
            &config,
            format!(
                "{text}[core]\n\t# x \\\n[filter \"evil\"]\n\tclean = touch {} && cat\n",
                marker.display()
            ),
        )
        .expect("config");
        std::fs::write(repo.top().join(".gitattributes"), "* filter=evil\n").expect("attrs");
        std::fs::write(repo.top().join("a.txt"), "x").expect("file");
        let error = repo.check().expect_err("refused");
        assert_eq!(error.code(), ErrorCode::UnsafeRepository);
        assert!(
            error.message().contains("filter.evil.clean"),
            "{}",
            error.message()
        );
        assert!(repo.git(repo.top()).worktree().run(&["add", "-A"]).is_err());
        assert!(
            Repo::discover(
                repo.top(),
                &SandboxConfig::default(),
                Arc::new(DenyList::default())
            )
            .is_err(),
            "discovery refuses it too"
        );
        assert!(!marker.exists(), "the payload ran");
    }

    /// git's own reader is asked as well: a config git cannot read is
    /// refused even when this module's parser has no objection.
    #[test]
    fn config_git_cannot_read_is_refused() {
        let (_dir, _base, repo) = repo();
        assert!(repo.check().is_ok());
        assert!(repo.git_config_listing("[core]\n\tbare = false\n").is_ok());
        assert!(repo.git_config_listing("[core\n").is_err());
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
            "submodule.m.update",
            "submodule.m.foo",
            "submodule.recurse",
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
            "pager.log",
        ] {
            assert!(dangerous_key(key, Some("x")).is_some(), "{key}");
        }
        assert!(dangerous_key("submodule.m.update", Some("!touch x")).is_some());
        assert!(dangerous_key("submodule.m.update", None).is_some());
        for (key, value) in [
            ("core.hookspath", ".husky"),
            ("submodule.m.url", "../m"),
            ("submodule.m.active", "true"),
            ("submodule.m.update", "Rebase"),
            ("submodule.m.fetchrecursesubmodules", "false"),
        ] {
            assert!(dangerous_key(key, Some(value)).is_none(), "{key}");
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

    /// The host's git starts from an empty environment: a variable the
    /// parent holds (an ssh command, a custom one) never reaches it. The
    /// test runs itself again as a child process with the variables set,
    /// rather than setting them in this process under the other tests.
    #[test]
    fn host_git_inherits_nothing_of_the_parent_environment() {
        const PROBE: &str = "ELITEA_GIT_ENV_PROBE";
        if std::env::var_os(PROBE).is_none() {
            let output = Command::new(std::env::current_exe().expect("test binary"))
                .args([
                    "--exact",
                    "git::tests::host_git_inherits_nothing_of_the_parent_environment",
                    "--nocapture",
                ])
                .env(PROBE, "1")
                .env("GIT_SSH_COMMAND", "leaked-ssh")
                .env("FOO", "leaked-foo")
                .output()
                .expect("child test");
            assert!(
                output.status.success(),
                "child: {}{}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
            assert!(String::from_utf8_lossy(&output.stdout).contains("1 passed"));
            return;
        }
        assert_eq!(std::env::var("FOO").as_deref(), Ok("leaked-foo"));
        let (_dir, _base, repo) = repo();
        let env = repo
            .git(repo.top())
            .run(&["-c", "alias.dumpenv=!env", "dumpenv"])
            .expect("env");
        let env = String::from_utf8_lossy(&env);
        let names: Vec<&str> = env
            .lines()
            .filter_map(|line| line.split_once('=').map(|(name, _)| name))
            .collect();
        assert!(names.contains(&"PATH"), "{env}");
        assert!(names.contains(&"GIT_DIR"), "{env}");
        for leaked in ["FOO", "GIT_SSH_COMMAND", PROBE] {
            assert!(!names.contains(&leaked), "{leaked} leaked: {env}");
        }
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

    /// A global config that exists but cannot be listed leaves the
    /// drivers unknown: refused, not assumed empty. A missing one is fine.
    #[test]
    fn unreadable_global_config_fails_closed() {
        let (_dir, base, repo) = repo();
        std::fs::write(repo.top().join("a.txt"), "x").expect("file");
        let missing = repo
            .clone()
            .with_global_config(base.join("no-such-gitconfig"));
        assert!(missing.check_attributes(&[]).is_ok(), "no global config");
        let broken = base.join("gitconfig");
        std::fs::write(&broken, "[filter \"x\"\n\tclean = cat\n").expect("global");
        let repo = repo.with_global_config(broken);
        let error = repo.check_attributes(&[]).expect_err("fails closed");
        assert_eq!(error.code(), ErrorCode::UnsafeRepository);
        assert!(
            error.message().contains("global git config"),
            "{}",
            error.message()
        );
        assert!(repo.git(repo.top()).worktree().run(&["status"]).is_err());
    }

    /// git-lfs exactly as `git lfs install` writes it is trusted: LFS
    /// repositories work. Any other command under `filter.lfs` is not.
    #[test]
    fn the_standard_git_lfs_filter_is_trusted() {
        let (_dir, base, repo) = repo();
        let global = base.join("gitconfig");
        std::fs::write(
            &global,
            "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n",
        )
        .expect("global");
        let repo = repo.with_global_config(global.clone());
        std::fs::write(
            repo.top().join(".gitattributes"),
            "*.bin filter=lfs diff=lfs merge=lfs -text\n",
        )
        .expect("attrs");
        std::fs::write(repo.top().join("a.bin"), "x").expect("file");
        assert!(
            repo.check_attributes(&[]).is_ok(),
            "standard git-lfs is trusted"
        );
        assert_eq!(repo.attribute_scans(), 0, "no driver left to scan for");
        std::fs::write(
            &global,
            "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tprocess = git-lfs filter-process --evil\n",
        )
        .expect("global");
        let error = repo
            .check_attributes(&[])
            .expect_err("not the standard filter");
        assert!(error.message().contains("a.bin"), "{}", error.message());
    }

    /// The scan is cached per repository and redone when anything it
    /// depends on changes: a new untracked path, a `.gitattributes`.
    #[test]
    fn attribute_scans_are_cached_until_the_tree_changes() {
        let (_dir, base, repo) = repo();
        let global = base.join("gitconfig");
        std::fs::write(&global, "[diff \"x\"]\n\ttextconv = cat\n").expect("global");
        let repo = repo.with_global_config(global);
        std::fs::create_dir(repo.top().join("sub")).expect("sub");
        std::fs::write(repo.top().join("sub/.gitattributes"), "*.y diff=x\n").expect("attrs");
        std::fs::write(repo.top().join("a.txt"), "x").expect("file");
        assert!(repo.check_attributes(&[]).is_ok());
        assert!(repo.check_attributes(&[]).is_ok());
        assert!(repo.git(repo.top()).worktree().run(&["status"]).is_ok());
        assert_eq!(repo.attribute_scans(), 1, "unchanged: scanned once");
        std::fs::write(repo.top().join("sub/b.y"), "y").expect("new file");
        assert!(repo.check_attributes(&[]).is_err(), "a new path is scanned");
        std::fs::remove_file(repo.top().join("sub/b.y")).expect("remove");
        assert!(repo.check_attributes(&[]).is_ok());
        let scans = repo.attribute_scans();
        std::fs::write(repo.top().join("sub/.gitattributes"), "*.txt diff=x\n").expect("attrs");
        std::fs::write(repo.top().join("sub/c.txt"), "c").expect("file");
        std::fs::remove_file(repo.top().join("sub/c.txt")).expect("remove");
        assert!(repo.check_attributes(&[]).is_ok());
        assert_eq!(
            repo.attribute_scans(),
            scans + 1,
            "a changed .gitattributes is scanned"
        );
        std::fs::write(repo.top().join(".gitignore"), ".gitattributes\n").expect("ignore");
        std::fs::write(repo.top().join(".gitattributes"), "*.txt diff=x\n").expect("ignored attrs");
        assert!(
            repo.check_attributes(&[]).is_err(),
            "an ignored .gitattributes still applies and is seen"
        );
    }

    #[test]
    fn revisions_cannot_be_options() {
        for good in [
            "HEAD",
            "HEAD~2",
            "HEAD^",
            "HEAD^2~3",
            "main",
            "origin/main",
            "feature/x-1",
            "v1.2.3",
            "abc123",
            "a..b",
            "HEAD~1..HEAD",
            "main...feature",
        ] {
            assert!(check_revision(good).is_ok(), "{good}");
        }
        for bad in [
            "",
            "--output=/tmp/x",
            "-p",
            "a b",
            "$(x)",
            "a;b",
            "HEAD:secrets",
            "HEAD:.env",
            "main:src",
            ":/fix",
            "HEAD@{1}",
            "main@{upstream}",
            "@",
            "a..b..c",
            "a....b",
            "..b",
            "x.lock",
            "a/.b",
            "HEAD~x",
            "main..-p",
        ] {
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
