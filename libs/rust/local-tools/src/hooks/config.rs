//! The hooks file (`hooks.json`, #1237), read from the host's data only:
//! `<app config dir>/hooks.json` (the person's, every workspace) and
//! `<app data>/workspaces/<id>/hooks.json` (one workspace's, where accepted
//! repository proposals go). Never from the workspace folder: a
//! repository's `.elitea/hooks.json` is a proposal ([`super::proposal`]),
//! copied here only when the person trusts it.
//!
//! ```json
//! {"version": 1, "hooks": [
//!   {"id": "fmt-after-edit", "event": "PostToolUse",
//!    "match": {"tools": ["edit_file", "multi_edit", "apply_patch", "write_file"], "paths": ["**/*.rs"]},
//!    "command": ["cargo", "fmt", "--", "{path}"], "timeout_ms": 10000,
//!    "sandbox": "workspace-write", "network": false}
//! ]}
//! ```
//!
//! The schema is enforced on load and unknown keys are refused: `id`
//! `[A-Za-z0-9_-]{1,64}` and unique; `command` an argv (no shell unless the
//! person writes `["sh", "-c", …]`), at most 64 items of 4 KiB; `timeout_ms`
//! 1 to 30 000; `sandbox` `read-only` or `workspace-write` (a hook is never
//! asked about, so it never runs unconfined); `match.tools` tool names (or
//! `*`) and `match.paths` globs. Both files are `0600` in a `0700` folder,
//! opened without following a symlink, and refused when another user owns
//! them or others can read them (the `threads.sqlite` rules).

use std::fs;
use std::io::{Read as _, Write as _};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::error::{ErrorCode, ToolError, ToolResult};
pub use crate::mcp::config::Scope;
use crate::policy::SandboxMode;

/// The file's name in both folders.
pub const FILE_NAME: &str = "hooks.json";

/// The schema version this crate reads and writes.
pub const VERSION: u32 = 1;

/// Hooks in one file.
pub const MAX_HOOKS: usize = 32;

/// Items in a hook's `command`.
pub const MAX_ARGV: usize = 64;

/// Bytes in one `command` item, glob or tool name.
pub const MAX_ARG: usize = 4 * 1024;

/// The longest a hook may run, and what one call's hooks may take together.
pub const MAX_TIMEOUT_MS: u64 = 30_000;

/// A hook's timeout when the file names none.
pub const DEFAULT_TIMEOUT_MS: u64 = 10_000;

/// The largest file read.
pub const MAX_FILE_BYTES: u64 = 256 * 1024;

/// The longest hook id.
pub const MAX_ID: usize = 64;

/// The answered proposals one file keeps (the oldest are dropped).
pub const MAX_ANSWERED: usize = 64;

/// When a hook runs.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize)]
pub enum HookEvent {
    /// Before a local tool call, after the policy ceiling and plan mode:
    /// may deny it, force the approval prompt, or modify its input.
    PreToolUse,
    /// After a local tool call that succeeded: may annotate its result.
    PostToolUse,
    /// When the turn ends: what it says is shown to the person.
    Stop,
    /// Before the person's message goes to the model: may add context or
    /// refuse the message.
    UserPromptSubmit,
}

impl HookEvent {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::PreToolUse => "PreToolUse",
            Self::PostToolUse => "PostToolUse",
            Self::Stop => "Stop",
            Self::UserPromptSubmit => "UserPromptSubmit",
        }
    }

    /// Whether the event is about one tool call (its `match` applies).
    #[must_use]
    pub const fn is_tool_event(self) -> bool {
        matches!(self, Self::PreToolUse | Self::PostToolUse)
    }
}

/// Which calls a tool hook runs for: every listed tool (none or `*`: every
/// tool) and, with `paths`, only calls touching a path one of the globs
/// matches.
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HookMatch {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub paths: Vec<String>,
}

impl HookMatch {
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.tools.is_empty() && self.paths.is_empty()
    }

    /// Whether `tool` is one the hook names.
    #[must_use]
    pub fn names_tool(&self, tool: &str) -> bool {
        self.tools.is_empty() || self.tools.iter().any(|name| name == "*" || name == tool)
    }
}

const fn yes() -> bool {
    true
}

#[allow(clippy::trivially_copy_pass_by_ref)] // serde's skip_serializing_if signature
const fn is_true(value: &bool) -> bool {
    *value
}

#[allow(clippy::trivially_copy_pass_by_ref)] // serde's skip_serializing_if signature
const fn is_false(value: &bool) -> bool {
    !*value
}

const fn default_timeout() -> u64 {
    DEFAULT_TIMEOUT_MS
}

fn user_source() -> String {
    "user".to_owned()
}

fn is_user(source: &str) -> bool {
    source == "user"
}

/// One hook.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)] // independent switches of one entry
pub struct Hook {
    pub id: String,
    pub event: HookEvent,
    #[serde(rename = "match", default, skip_serializing_if = "HookMatch::is_empty")]
    pub matcher: HookMatch,
    /// The program and its arguments. An item that is exactly `{path}`
    /// becomes the call's matching paths (one item each, workspace-relative)
    /// and `{tool}` the tool's name: never spliced into a string.
    pub command: Vec<String>,
    #[serde(default = "default_timeout")]
    pub timeout_ms: u64,
    /// Capped by the policy's `max_sandbox_mode` when it runs.
    #[serde(default)]
    pub sandbox: SandboxMode,
    /// On only when the policy allows the network too.
    #[serde(default)]
    pub network: bool,
    /// A failure or timeout refuses the call (or the message) instead of
    /// letting it proceed.
    #[serde(default, skip_serializing_if = "is_false")]
    pub fail_closed: bool,
    #[serde(default = "yes", skip_serializing_if = "is_true")]
    pub enabled: bool,
    /// `user` (the person wrote it), or `proposal:<sha256>` (accepted from
    /// a repository file with that digest).
    #[serde(default = "user_source", skip_serializing_if = "is_user")]
    pub source: String,
}

impl Hook {
    /// A user hook with the defaults.
    #[must_use]
    pub fn new(id: &str, event: HookEvent, command: Vec<String>) -> Self {
        Self {
            id: id.to_owned(),
            event,
            matcher: HookMatch::default(),
            command,
            timeout_ms: DEFAULT_TIMEOUT_MS,
            sandbox: SandboxMode::ReadOnly,
            network: false,
            fail_closed: false,
            enabled: true,
            source: user_source(),
        }
    }

    /// Check every bound.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`], naming what is wrong.
    pub fn validate(&self) -> ToolResult<()> {
        if !id_ok(&self.id) {
            return Err(ToolError::invalid(format!(
                "hook id `{}` must match [A-Za-z0-9_-]{{1,{MAX_ID}}}",
                self.id
            )));
        }
        let bad = |what: &str| ToolError::invalid(format!("hook `{}`: {what}", self.id));
        if self.command.is_empty() || self.command[0].trim().is_empty() {
            return Err(bad("`command` names no program"));
        }
        if self.command.len() > MAX_ARGV {
            return Err(bad(&format!("`command` holds at most {MAX_ARGV} items")));
        }
        if self
            .command
            .iter()
            .any(|item| item.len() > MAX_ARG || item.contains('\0'))
        {
            return Err(bad(&format!(
                "a `command` item is longer than {MAX_ARG} bytes or holds a NUL"
            )));
        }
        if matches!(self.command[0].as_str(), "{path}" | "{tool}") {
            return Err(bad("the program cannot be a placeholder"));
        }
        if !self.command[0].contains('/') && self.command[0].contains(char::is_whitespace) {
            return Err(bad(
                "the program is one argv item: write [\"sh\", \"-c\", \"…\"] for a shell line",
            ));
        }
        if self.command[0].contains('/') && !Path::new(&self.command[0]).is_absolute() {
            return Err(bad("the program is a bare name or an absolute path"));
        }
        if !(1..=MAX_TIMEOUT_MS).contains(&self.timeout_ms) {
            return Err(bad(&format!("`timeout_ms` is 1 to {MAX_TIMEOUT_MS}")));
        }
        if self.sandbox == SandboxMode::FullAccess {
            return Err(bad(
                "`sandbox` is read-only or workspace-write: hooks are never asked about, so they never run unconfined",
            ));
        }
        if !self.matcher.is_empty() && !self.event.is_tool_event() {
            return Err(bad("`match` applies to PreToolUse and PostToolUse only"));
        }
        if self.matcher.tools.len() > MAX_ARGV || self.matcher.paths.len() > MAX_ARGV {
            return Err(bad(&format!(
                "`match` lists at most {MAX_ARGV} tools and {MAX_ARGV} paths"
            )));
        }
        if self.matcher.tools.iter().any(|tool| {
            tool != "*"
                && (!(1..=MAX_ID).contains(&tool.len())
                    || !tool
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_'))
        }) {
            return Err(bad("a `match.tools` entry is not a tool name"));
        }
        for glob in &self.matcher.paths {
            if glob.len() > MAX_ARG || globset::Glob::new(glob).is_err() {
                return Err(bad(&format!("`{glob}` is not a glob")));
            }
        }
        if !(is_user(&self.source) || proposal_source_ok(&self.source)) {
            return Err(bad("`source` is `user` or `proposal:<sha256>`"));
        }
        Ok(())
    }
}

fn proposal_source_ok(source: &str) -> bool {
    source
        .strip_prefix("proposal:")
        .is_some_and(|digest| digest.len() == 64 && digest.bytes().all(|b| b.is_ascii_hexdigit()))
}

/// Whether `id` is a valid hook id.
#[must_use]
pub fn id_ok(id: &str) -> bool {
    (1..=MAX_ID).contains(&id.len())
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
}

/// One `hooks.json`.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HooksConfig {
    pub version: u32,
    #[serde(default)]
    pub hooks: Vec<Hook>,
    /// The repository proposals the person answered in this workspace (the
    /// SHA-256 of each file, oldest first): a file with one of these
    /// digests is not proposed again, and a changed file is a new proposal.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub answered_proposals: Vec<String>,
}

impl Default for HooksConfig {
    fn default() -> Self {
        Self {
            version: VERSION,
            hooks: Vec::new(),
            answered_proposals: Vec::new(),
        }
    }
}

impl HooksConfig {
    /// Note `digest` as answered, keeping at most [`MAX_ANSWERED`].
    pub fn answered(&mut self, digest: &str) {
        self.answered_proposals.retain(|known| known != digest);
        self.answered_proposals.push(digest.to_owned());
        let over = self.answered_proposals.len().saturating_sub(MAX_ANSWERED);
        self.answered_proposals.drain(..over);
    }

    /// Parse and validate one file's bytes.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`]: it does not parse, holds an unknown
    /// key, or breaks a bound.
    pub fn parse(bytes: &[u8]) -> ToolResult<Self> {
        let config: Self = serde_json::from_slice(bytes)
            .map_err(|error| ToolError::invalid(format!("hooks.json is not valid: {error}")))?;
        config.validate()?;
        Ok(config)
    }

    /// Check every bound.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`], naming the hook.
    pub fn validate(&self) -> ToolResult<()> {
        if self.version != VERSION {
            return Err(ToolError::invalid(format!(
                "hooks.json version {} is not supported (expected {VERSION})",
                self.version
            )));
        }
        if self.hooks.len() > MAX_HOOKS {
            return Err(ToolError::invalid(format!(
                "hooks.json holds at most {MAX_HOOKS} hooks"
            )));
        }
        if self.answered_proposals.len() > MAX_ANSWERED
            || self
                .answered_proposals
                .iter()
                .any(|digest| digest.len() != 64 || !digest.bytes().all(|b| b.is_ascii_hexdigit()))
        {
            return Err(ToolError::invalid(format!(
                "hooks.json answered_proposals holds at most {MAX_ANSWERED} SHA-256 digests"
            )));
        }
        for (index, hook) in self.hooks.iter().enumerate() {
            hook.validate()?;
            if self.hooks[..index].iter().any(|other| other.id == hook.id) {
                return Err(ToolError::invalid(format!(
                    "hook id `{}` is used twice",
                    hook.id
                )));
            }
        }
        Ok(())
    }
}

fn refused(path: &Path, why: &str) -> ToolError {
    ToolError::new(ErrorCode::Denied, format!("{} {why}", path.display()))
}

/// Read `path` (absent: an empty configuration).
///
/// # Errors
///
/// [`ErrorCode::Denied`] for a symlink, a file that is not regular,
/// another user's, or one group or others can read or write;
/// [`ErrorCode::InvalidArgument`] when it does not parse; otherwise I/O.
pub fn load(path: &Path) -> ToolResult<HooksConfig> {
    use rustix::fs::{Mode, OFlags};
    let fd = match rustix::fs::open(
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
        Mode::empty(),
    ) {
        Ok(fd) => fd,
        Err(rustix::io::Errno::NOENT) => return Ok(HooksConfig::default()),
        // O_NOFOLLOW on a symlink: ELOOP (Linux, macOS).
        Err(rustix::io::Errno::LOOP) => return Err(refused(path, "is a symbolic link")),
        Err(errno) => return Err(ToolError::io("cannot open hooks.json", &errno.into())),
    };
    let mut file = fs::File::from(fd);
    let meta = file
        .metadata()
        .map_err(|error| ToolError::io("cannot inspect hooks.json", &error))?;
    if !meta.is_file() {
        return Err(refused(path, "is not a regular file"));
    }
    {
        use std::os::unix::fs::MetadataExt as _;
        if meta.uid() != rustix::process::geteuid().as_raw() {
            return Err(refused(path, "belongs to another user"));
        }
        if meta.mode() & 0o077 != 0 {
            return Err(refused(path, "is readable or writable by other users"));
        }
    }
    if meta.len() > MAX_FILE_BYTES {
        return Err(refused(path, "is too large"));
    }
    let mut bytes = Vec::new();
    std::io::Read::take(&mut file, MAX_FILE_BYTES + 1)
        .read_to_end(&mut bytes)
        .map_err(|error| ToolError::io("cannot read hooks.json", &error))?;
    HooksConfig::parse(&bytes)
}

/// Create `dir` (and missing parents) `0700`; refuse a symlink, a
/// non-directory, another user's or one others can reach.
fn private_dir(dir: &Path) -> ToolResult<()> {
    use std::os::unix::fs::{DirBuilderExt as _, MetadataExt as _};
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .map_err(|error| ToolError::io("cannot create the hooks settings folder", &error))?;
    let meta = fs::symlink_metadata(dir)
        .map_err(|error| ToolError::io("cannot inspect the hooks settings folder", &error))?;
    if meta.file_type().is_symlink() || !meta.is_dir() {
        return Err(refused(dir, "is not a directory"));
    }
    if meta.uid() != rustix::process::geteuid().as_raw() {
        return Err(refused(dir, "belongs to another user"));
    }
    if meta.mode() & 0o077 != 0 {
        return Err(refused(dir, "is readable or writable by other users"));
    }
    Ok(())
}

/// Write `config` to `path` atomically: a new `0600` file beside it (never
/// following a symlink), then renamed over it.
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when it breaks a bound; the folder or an
/// existing file is refused; otherwise I/O.
pub fn save(path: &Path, config: &HooksConfig) -> ToolResult<()> {
    use std::os::unix::fs::OpenOptionsExt as _;
    config.validate()?;
    let dir = path
        .parent()
        .ok_or_else(|| ToolError::invalid("hooks.json has no folder"))?;
    private_dir(dir)?;
    // An existing file must be one we would read: never replace another
    // user's file or write through a symlink's target.
    if fs::symlink_metadata(path).is_ok() {
        load(path).map(|_| ()).or_else(|error| {
            if error.code() == ErrorCode::InvalidArgument {
                // A broken file of ours is replaced by a valid one.
                Ok(())
            } else {
                Err(error)
            }
        })?;
    }
    let mut bytes = serde_json::to_vec_pretty(config)
        .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode hooks.json"))?;
    bytes.push(b'\n');
    let temp = temp_path(path);
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(rustix::fs::OFlags::NOFOLLOW.bits().cast_signed())
        .open(&temp)
        .map_err(|error| ToolError::io("cannot write hooks.json", &error))?;
    let written = file
        .write_all(&bytes)
        .and_then(|()| file.sync_all())
        .and_then(|()| fs::rename(&temp, path));
    if let Err(error) = written {
        let _ = fs::remove_file(&temp);
        return Err(ToolError::io("cannot write hooks.json", &error));
    }
    Ok(())
}

fn temp_path(path: &Path) -> PathBuf {
    let mut bytes = [0u8; 6];
    let random = fs::File::open("/dev/urandom")
        .and_then(|mut file| file.read_exact(&mut bytes))
        .map_or_else(
            |_| format!("{}", std::process::id()),
            |()| crate::mcp::config::hex(&bytes),
        );
    path.with_file_name(format!(".{FILE_NAME}.{random}.tmp"))
}

/// The hooks of both files in the order they run: the person's (every
/// workspace) first, then the workspace's; a workspace hook with a user
/// hook's id replaces it in that workspace. Disabled hooks are left out.
#[must_use]
pub fn merged(user: &HooksConfig, workspace: &HooksConfig) -> Vec<(Scope, Hook)> {
    user.hooks
        .iter()
        .filter(|hook| !workspace.hooks.iter().any(|other| other.id == hook.id))
        .map(|hook| (Scope::User, hook.clone()))
        .chain(
            workspace
                .hooks
                .iter()
                .map(|hook| (Scope::Workspace, hook.clone())),
        )
        .filter(|(_, hook)| hook.enabled)
        .collect()
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt as _;

    use super::*;

    const SAMPLE: &str = r#"{"version":1,"hooks":[
      {"id":"fmt-after-edit","event":"PostToolUse","match":{"tools":["edit_file","multi_edit","apply_patch","write_file"],"paths":["**/*.rs"]},
       "command":["cargo","fmt","--","{path}"],"timeout_ms":10000,"sandbox":"workspace-write","network":false},
      {"id":"no-migrations","event":"PreToolUse","match":{"tools":["run_command","write_file","edit_file"],"paths":["migrations/**"]},
       "command":["/Users/me/bin/elitea-guard"],"timeout_ms":3000,"sandbox":"read-only","network":false}
    ]}"#;

    #[test]
    fn the_documented_sample_parses() {
        let config = HooksConfig::parse(SAMPLE.as_bytes()).expect("parse");
        assert_eq!(config.hooks.len(), 2);
        assert_eq!(config.hooks[0].event, HookEvent::PostToolUse);
        assert_eq!(config.hooks[1].sandbox, SandboxMode::ReadOnly);
        assert!(config.hooks[0].matcher.names_tool("edit_file"));
        assert!(!config.hooks[0].matcher.names_tool("run_command"));
    }

    #[test]
    fn bounds_are_enforced() {
        let bad = [
            r#"{"version":1,"hooks":[{"id":"a b","event":"Stop","command":["/bin/true"]}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":[]}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["./x"]}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["cargo fmt"]}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["{path}"]}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["/bin/true"],"timeout_ms":0}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["/bin/true"],"timeout_ms":30001}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["/bin/true"],"sandbox":"full-access"}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["/bin/true"],"match":{"tools":["x"]}}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["/bin/true"],"extra":1}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Nope","command":["/bin/true"]}]}"#,
            r#"{"version":1,"hooks":[{"id":"a","event":"Stop","command":["/bin/true"]},{"id":"a","event":"Stop","command":["/bin/true"]}]}"#,
            r#"{"version":2,"hooks":[]}"#,
        ];
        for text in bad {
            assert!(HooksConfig::parse(text.as_bytes()).is_err(), "{text}");
        }
    }

    #[test]
    fn files_are_owner_only_and_merge_with_workspace_ids_winning() {
        let dir = tempfile::tempdir().expect("dir");
        let user_path = dir.path().join("config").join(FILE_NAME);
        assert_eq!(load(&user_path).expect("absent"), HooksConfig::default());
        let mut user = HooksConfig::default();
        user.hooks.push(Hook::new(
            "a",
            HookEvent::Stop,
            vec!["/bin/echo".to_owned()],
        ));
        user.hooks.push(Hook::new(
            "b",
            HookEvent::Stop,
            vec!["/bin/echo".to_owned()],
        ));
        save(&user_path, &user).expect("save");
        let mode = fs::metadata(&user_path).expect("meta").permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
        assert_eq!(load(&user_path).expect("load"), user);
        fs::set_permissions(&user_path, fs::Permissions::from_mode(0o644)).expect("chmod");
        assert_eq!(
            load(&user_path).expect_err("refused").code(),
            ErrorCode::Denied
        );
        let link = dir.path().join("link.json");
        std::os::unix::fs::symlink(&user_path, &link).expect("symlink");
        assert_eq!(load(&link).expect_err("link").code(), ErrorCode::Denied);

        let mut workspace = HooksConfig::default();
        let mut replaced = Hook::new("a", HookEvent::Stop, vec!["/bin/cat".to_owned()]);
        replaced.source = format!("proposal:{}", "0".repeat(64));
        workspace.hooks.push(replaced);
        let mut off = Hook::new("c", HookEvent::Stop, vec!["/bin/cat".to_owned()]);
        off.enabled = false;
        workspace.hooks.push(off);
        let merged = merged(&user, &workspace);
        let ids: Vec<_> = merged
            .iter()
            .map(|(scope, hook)| (scope.as_str(), hook.id.as_str()))
            .collect();
        assert_eq!(ids, [("user", "b"), ("workspace", "a")]);
    }
}
