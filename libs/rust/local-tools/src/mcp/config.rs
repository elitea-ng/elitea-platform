//! The local MCP configuration file (`mcp.json`, #1230), read from the
//! host's data only: `<app config dir>/mcp.json` (the person's, every
//! workspace) and `<app data>/workspaces/<id>/mcp.json` (one workspace's,
//! where accepted repository proposals go). Never from the workspace
//! folder: a repository's `.mcp.json` is a proposal
//! ([`super::proposal`]), copied here only when the person accepts it.
//!
//! ```json
//! {
//!   "version": 1,
//!   "servers": {
//!     "github-local": {
//!       "command": "/opt/homebrew/bin/github-mcp-server",
//!       "args": ["stdio"],
//!       "env": { "GITHUB_TOKEN_FILE": "/Users/me/.config/gh/token" },
//!       "cwd": "workspace",
//!       "network": false,
//!       "enabled": true,
//!       "allowed_tools": null,
//!       "read_only_tools": ["search_issues"],
//!       "trust_read_only_hints": false,
//!       "source": "user"
//!     }
//!   }
//! }
//! ```
//!
//! The schema is enforced on load and unknown keys are refused: `command`
//! an absolute path (resolved once when the server starts; it must then be
//! a regular executable file outside the workspace); `args` at most
//! [`MAX_ARGS`] strings; `env` at most [`MAX_ENV`] entries, keys
//! `[A-Z_][A-Z0-9_]*`, values at most [`MAX_ENV_VALUE`] bytes; `cwd`
//! `workspace` (the root) or `none` (a private temporary directory);
//! `network` (off by default, and capped by the policy); `allowed_tools`
//! `null` (all) or the names offered; `read_only_tools` the names the
//! person marks read-only; `source` `user` or `proposal:<sha256 of the
//! proposed file>`.
//!
//! The file is owner-only: created `0600` in a `0700` directory, opened
//! without following a symlink, and refused when it is not a regular file,
//! belongs to another user, or is readable or writable by group or others
//! (as `threads.sqlite`). A refused or broken file starts no server.

use std::collections::BTreeMap;
use std::fmt;
use std::fs;
use std::io::{Read as _, Write as _};
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::error::{ErrorCode, ToolError, ToolResult};

/// The file's name in either directory.
pub const FILE_NAME: &str = "mcp.json";

/// The only `version` there is.
pub const VERSION: u32 = 1;

/// The most servers one file holds.
pub const MAX_SERVERS: usize = 32;

/// The most arguments a server takes.
pub const MAX_ARGS: usize = 64;

/// The most environment variables a server is given.
pub const MAX_ENV: usize = 64;

/// The longest environment value, in bytes.
pub const MAX_ENV_VALUE: usize = 4 * 1024;

/// The longest argument, in bytes.
pub const MAX_ARG: usize = 4 * 1024;

/// The longest tool name a list holds.
const MAX_TOOL_NAME: usize = 128;

/// The largest file read.
pub const MAX_FILE_BYTES: u64 = 256 * 1024;

/// The longest server name.
pub const MAX_SERVER_NAME: usize = 32;

/// Where a server runs.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ServerCwd {
    /// The workspace root.
    #[default]
    Workspace,
    /// A private temporary directory of the server's own.
    None,
}

/// Which file a server comes from.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Scope {
    /// `<app config dir>/mcp.json`: every workspace.
    User,
    /// `<app data>/workspaces/<id>/mcp.json`: one workspace.
    Workspace,
}

impl Scope {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::User => "user",
            Self::Workspace => "workspace",
        }
    }
}

const fn yes() -> bool {
    true
}

fn user_source() -> String {
    "user".to_owned()
}

/// One server, as the file names it.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)] // the person's independent switches
pub struct ServerConfig {
    pub command: String,
    #[serde(default)]
    pub args: Vec<String>,
    /// The server's whole environment beside the passthrough
    /// (`PATH`, `HOME`, `USER`, `LANG`, …): nothing else is inherited.
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    #[serde(default)]
    pub cwd: ServerCwd,
    #[serde(default)]
    pub network: bool,
    #[serde(default = "yes")]
    pub enabled: bool,
    /// `None`: every tool the server lists.
    #[serde(default)]
    pub allowed_tools: Option<Vec<String>>,
    #[serde(default)]
    pub read_only_tools: Vec<String>,
    /// Treat a tool's `readOnlyHint` as the person marking it read-only.
    #[serde(default)]
    pub trust_read_only_hints: bool,
    #[serde(default = "user_source")]
    pub source: String,
}

// `env` values may be secrets (tokens): only their names are printed.
impl fmt::Debug for ServerConfig {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ServerConfig")
            .field("command", &self.command)
            .field("args", &self.args.len())
            .field("env", &self.env.keys().collect::<Vec<_>>())
            .field("cwd", &self.cwd)
            .field("network", &self.network)
            .field("enabled", &self.enabled)
            .field("allowed_tools", &self.allowed_tools)
            .field("read_only_tools", &self.read_only_tools)
            .field("trust_read_only_hints", &self.trust_read_only_hints)
            .field("source", &self.source)
            .finish()
    }
}

impl ServerConfig {
    /// A server that runs `command` with nothing else set.
    #[must_use]
    pub fn new(command: impl Into<String>) -> Self {
        Self {
            command: command.into(),
            args: Vec::new(),
            env: BTreeMap::new(),
            cwd: ServerCwd::Workspace,
            network: false,
            enabled: true,
            allowed_tools: None,
            read_only_tools: Vec::new(),
            trust_read_only_hints: false,
            source: user_source(),
        }
    }

    /// Whether `tool` is offered at all.
    #[must_use]
    pub fn allows(&self, tool: &str) -> bool {
        self.allowed_tools
            .as_ref()
            .is_none_or(|allowed| allowed.iter().any(|name| name == tool))
    }

    /// Whether the person marked `tool` read-only (`hint`: the server's
    /// `readOnlyHint`, honoured only when they trust this server's hints).
    #[must_use]
    pub fn read_only(&self, tool: &str, hint: bool) -> bool {
        self.read_only_tools.iter().any(|name| name == tool) || (hint && self.trust_read_only_hints)
    }

    /// Whether every tool it may offer is marked read-only: only then does
    /// it start read-only (the tools it lists are not known before it starts).
    #[must_use]
    pub fn all_read_only(&self) -> bool {
        self.allowed_tools.as_ref().is_some_and(|allowed| {
            !allowed.is_empty()
                && allowed
                    .iter()
                    .all(|tool| self.read_only_tools.contains(tool))
        })
    }

    /// Check the schema's bounds (`serde` already refused unknown keys).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`], naming the field.
    pub fn validate(&self) -> ToolResult<()> {
        let invalid = |what: String| Err(ToolError::new(ErrorCode::InvalidArgument, what));
        if !Path::new(&self.command).is_absolute() || self.command.contains('\0') {
            return invalid("`command` must be an absolute path".to_owned());
        }
        if self.args.len() > MAX_ARGS {
            return invalid(format!("`args` holds at most {MAX_ARGS} entries"));
        }
        if self
            .args
            .iter()
            .any(|arg| arg.len() > MAX_ARG || arg.contains('\0'))
        {
            return invalid(format!(
                "an argument is longer than {MAX_ARG} bytes or holds a NUL"
            ));
        }
        if self.env.len() > MAX_ENV {
            return invalid(format!("`env` holds at most {MAX_ENV} entries"));
        }
        for (name, value) in &self.env {
            if !env_name_ok(name) {
                return invalid(format!("`env` name `{name}` must match [A-Z_][A-Z0-9_]*"));
            }
            if value.len() > MAX_ENV_VALUE || value.contains('\0') {
                return invalid(format!(
                    "`env` value of `{name}` is longer than {MAX_ENV_VALUE} bytes or holds a NUL"
                ));
            }
        }
        let names = self
            .allowed_tools
            .iter()
            .flatten()
            .chain(&self.read_only_tools);
        for name in names {
            if name.is_empty() || name.len() > MAX_TOOL_NAME || name.chars().any(char::is_control) {
                return invalid(
                    "a tool name in `allowed_tools` or `read_only_tools` is not valid".to_owned(),
                );
            }
        }
        if self.source != "user" && !is_proposal_source(&self.source) {
            return invalid("`source` must be `user` or `proposal:<sha256>`".to_owned());
        }
        Ok(())
    }
}

fn is_proposal_source(source: &str) -> bool {
    source.strip_prefix("proposal:").is_some_and(|digest| {
        digest.len() == 64
            && digest
                .bytes()
                .all(|byte| matches!(byte, b'0'..=b'9' | b'a'..=b'f'))
    })
}

fn env_name_ok(name: &str) -> bool {
    let mut bytes = name.bytes();
    bytes
        .next()
        .is_some_and(|first| first.is_ascii_uppercase() || first == b'_')
        && bytes.all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit() || byte == b'_')
}

/// Whether `name` may name a server: `[A-Za-z0-9_-]{1,32}`.
#[must_use]
pub fn server_name_ok(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= MAX_SERVER_NAME
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
}

/// One `mcp.json`.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct McpConfig {
    pub version: u32,
    #[serde(default)]
    pub servers: BTreeMap<String, ServerConfig>,
    /// The repository proposals the person answered in this workspace (the
    /// SHA-256 of each file, oldest first): a file with one of these digests
    /// is not proposed again, and a changed file is a new proposal.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub answered_proposals: Vec<String>,
}

/// The answered proposals one `mcp.json` keeps (the oldest are dropped).
pub const MAX_ANSWERED: usize = 64;

impl Default for McpConfig {
    fn default() -> Self {
        Self {
            version: VERSION,
            servers: BTreeMap::new(),
            answered_proposals: Vec::new(),
        }
    }
}

impl McpConfig {
    /// Note `digest` (a proposal's file SHA-256) as answered, keeping at
    /// most [`MAX_ANSWERED`] (the oldest go first).
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
        let config: Self = serde_json::from_slice(bytes).map_err(|error| {
            ToolError::new(
                ErrorCode::InvalidArgument,
                format!("mcp.json is not valid: {error}"),
            )
        })?;
        config.validate()?;
        Ok(config)
    }

    /// Check every bound.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`], naming the server.
    pub fn validate(&self) -> ToolResult<()> {
        if self.version != VERSION {
            return Err(ToolError::invalid(format!(
                "mcp.json version {} is not supported (expected {VERSION})",
                self.version
            )));
        }
        if self.servers.len() > MAX_SERVERS {
            return Err(ToolError::invalid(format!(
                "mcp.json holds at most {MAX_SERVERS} servers"
            )));
        }
        if self.answered_proposals.len() > MAX_ANSWERED
            || self
                .answered_proposals
                .iter()
                .any(|digest| digest.len() != 64 || !digest.bytes().all(|b| b.is_ascii_hexdigit()))
        {
            return Err(ToolError::invalid(format!(
                "mcp.json answered_proposals holds at most {MAX_ANSWERED} SHA-256 digests"
            )));
        }
        for (name, server) in &self.servers {
            if !server_name_ok(name) {
                return Err(ToolError::invalid(format!(
                    "server name `{name}` must match [A-Za-z0-9_-]{{1,{MAX_SERVER_NAME}}}"
                )));
            }
            server.validate().map_err(|error| {
                ToolError::invalid(format!("server `{name}`: {}", error.message()))
            })?;
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
pub fn load(path: &Path) -> ToolResult<McpConfig> {
    use rustix::fs::{Mode, OFlags};
    let fd = match rustix::fs::open(
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
        Mode::empty(),
    ) {
        Ok(fd) => fd,
        Err(rustix::io::Errno::NOENT) => return Ok(McpConfig::default()),
        // O_NOFOLLOW on a symlink: ELOOP (Linux, macOS).
        Err(rustix::io::Errno::LOOP) => return Err(refused(path, "is a symbolic link")),
        Err(errno) => return Err(ToolError::io("cannot open mcp.json", &errno.into())),
    };
    let mut file = fs::File::from(fd);
    let meta = file
        .metadata()
        .map_err(|error| ToolError::io("cannot inspect mcp.json", &error))?;
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
        .map_err(|error| ToolError::io("cannot read mcp.json", &error))?;
    McpConfig::parse(&bytes)
}

/// Create `dir` (and missing parents) `0700`; refuse a symlink, a
/// non-directory or another user's.
fn private_dir(dir: &Path) -> ToolResult<()> {
    use std::os::unix::fs::{DirBuilderExt as _, MetadataExt as _};
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .map_err(|error| ToolError::io("cannot create the MCP settings folder", &error))?;
    let meta = fs::symlink_metadata(dir)
        .map_err(|error| ToolError::io("cannot inspect the MCP settings folder", &error))?;
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
/// [`ErrorCode::InvalidArgument`] when it breaks a bound; the folder is
/// refused; otherwise I/O.
pub fn save(path: &Path, config: &McpConfig) -> ToolResult<()> {
    use std::os::unix::fs::OpenOptionsExt as _;
    config.validate()?;
    let dir = path
        .parent()
        .ok_or_else(|| ToolError::invalid("mcp.json has no folder"))?;
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
        .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode mcp.json"))?;
    bytes.push(b'\n');
    let temp = temp_path(path);
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(rustix::fs::OFlags::NOFOLLOW.bits().cast_signed())
        .open(&temp)
        .map_err(|error| ToolError::io("cannot write mcp.json", &error))?;
    let written = file
        .write_all(&bytes)
        .and_then(|()| file.sync_all())
        .and_then(|()| fs::rename(&temp, path));
    if let Err(error) = written {
        let _ = fs::remove_file(&temp);
        return Err(ToolError::io("cannot write mcp.json", &error));
    }
    Ok(())
}

/// `bytes` as lowercase hex.
pub(crate) fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut out, byte| {
        let _ = write!(out, "{byte:02x}");
        out
    })
}

fn temp_path(path: &Path) -> PathBuf {
    let mut bytes = [0u8; 6];
    let random = fs::File::open("/dev/urandom")
        .and_then(|mut file| file.read_exact(&mut bytes))
        .map_or_else(|_| format!("{}", std::process::id()), |()| hex(&bytes));
    path.with_file_name(format!(".{FILE_NAME}.{random}.tmp"))
}

/// The servers of both files, by name: a workspace entry with the name of a
/// user one replaces it in that workspace.
#[must_use]
pub fn merged(user: &McpConfig, workspace: &McpConfig) -> BTreeMap<String, (Scope, ServerConfig)> {
    let mut out: BTreeMap<String, (Scope, ServerConfig)> = user
        .servers
        .iter()
        .map(|(name, server)| (name.clone(), (Scope::User, server.clone())))
        .collect();
    for (name, server) in &workspace.servers {
        out.insert(name.clone(), (Scope::Workspace, server.clone()));
    }
    out
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt as _;

    use super::*;

    const SAMPLE: &str = r#"{
      "version": 1,
      "servers": {
        "github-local": {
          "command": "/opt/homebrew/bin/github-mcp-server",
          "args": ["stdio"],
          "env": { "GITHUB_TOKEN_FILE": "/Users/me/.config/gh/token" },
          "cwd": "workspace",
          "network": false,
          "enabled": true,
          "allowed_tools": null,
          "read_only_tools": ["search_issues"],
          "source": "user"
        }
      }
    }"#;

    #[test]
    fn the_documented_file_parses() {
        let config = McpConfig::parse(SAMPLE.as_bytes()).expect("parse");
        let server = &config.servers["github-local"];
        assert_eq!(server.args, ["stdio"]);
        assert!(server.allows("anything"));
        assert!(server.read_only("search_issues", false));
        assert!(
            !server.read_only("create_issue", true),
            "hints are not trusted"
        );
        assert!(!server.all_read_only());
        assert!(
            !format!("{server:?}").contains("/Users/me/.config/gh/token"),
            "env values are not printed"
        );
    }

    #[test]
    fn config_refuses_unknown_keys() {
        let top = SAMPLE.replacen("\"version\": 1,", "\"version\": 1, \"extra\": true,", 1);
        assert!(McpConfig::parse(top.as_bytes()).is_err());
        let inner = SAMPLE.replacen("\"cwd\"", "\"shell\": true, \"cwd\"", 1);
        let error = McpConfig::parse(inner.as_bytes()).expect_err("unknown key");
        assert!(error.message().contains("shell"), "{}", error.message());
    }

    #[test]
    fn config_bounds_are_enforced() {
        let mut server = ServerConfig::new("bin/relative");
        assert!(server.validate().is_err(), "relative command");
        server.command = "/usr/bin/true".to_owned();
        server.validate().expect("valid");
        server.env.insert("lower".to_owned(), "x".to_owned());
        assert!(server.validate().is_err(), "env key");
        server.env.clear();
        server
            .env
            .insert("BIG".to_owned(), "x".repeat(MAX_ENV_VALUE + 1));
        assert!(server.validate().is_err(), "env value");
        server.env.clear();
        server.args = vec!["a".to_owned(); MAX_ARGS + 1];
        assert!(server.validate().is_err(), "args");
        server.args.clear();
        server.source = "proposal:abc".to_owned();
        assert!(server.validate().is_err(), "source");
        server.source = format!("proposal:{}", "a".repeat(64));
        server.validate().expect("proposal source");
        let mut config = McpConfig::default();
        config.servers.insert("bad name".to_owned(), server);
        assert!(config.validate().is_err(), "server name");
    }

    #[test]
    fn config_refuses_symlink_and_group_readable_file() {
        let dir = tempfile::tempdir().expect("dir");
        let path = dir.path().join(FILE_NAME);
        assert_eq!(load(&path).expect("absent"), McpConfig::default());
        fs::write(&path, SAMPLE).expect("write");
        fs::set_permissions(&path, fs::Permissions::from_mode(0o640)).expect("chmod");
        let error = load(&path).expect_err("group-readable");
        assert_eq!(error.code(), ErrorCode::Denied);
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).expect("chmod");
        load(&path).expect("owner-only");
        let link = dir.path().join("link.json");
        std::os::unix::fs::symlink(&path, &link).expect("symlink");
        let error = load(&link).expect_err("symlink");
        assert_eq!(error.code(), ErrorCode::Denied);
        assert!(
            error.message().contains("symbolic link"),
            "{}",
            error.message()
        );
    }

    #[test]
    fn saved_files_are_owner_only_and_load_back() {
        use std::os::unix::fs::MetadataExt as _;
        let dir = tempfile::tempdir().expect("dir");
        let folder = dir.path().join("config");
        let path = folder.join(FILE_NAME);
        let mut config = McpConfig::default();
        let mut server = ServerConfig::new("/usr/bin/true");
        server
            .env
            .insert("TOKEN_FILE".to_owned(), "/tmp/t".to_owned());
        config.servers.insert("one".to_owned(), server);
        save(&path, &config).expect("save");
        assert_eq!(fs::metadata(&path).expect("meta").mode() & 0o777, 0o600);
        assert_eq!(fs::metadata(&folder).expect("meta").mode() & 0o777, 0o700);
        assert_eq!(load(&path).expect("load"), config);
        // Another save replaces it in place.
        config.servers.clear();
        save(&path, &config).expect("save again");
        assert!(load(&path).expect("load").servers.is_empty());
    }

    #[test]
    fn workspace_entries_replace_user_ones_of_the_same_name() {
        let mut user = McpConfig::default();
        user.servers
            .insert("a".to_owned(), ServerConfig::new("/usr/bin/true"));
        user.servers
            .insert("b".to_owned(), ServerConfig::new("/usr/bin/true"));
        let mut workspace = McpConfig::default();
        workspace
            .servers
            .insert("b".to_owned(), ServerConfig::new("/usr/bin/false"));
        let all = merged(&user, &workspace);
        assert_eq!(all["a"].0, Scope::User);
        assert_eq!(all["b"].0, Scope::Workspace);
        assert_eq!(all["b"].1.command, "/usr/bin/false");
    }
}
