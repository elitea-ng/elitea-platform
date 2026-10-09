//! The OS sandbox commands run under (ADR-0029 decision 4).
//!
//! | OS | Mechanism | Reported enforcement |
//! |---|---|---|
//! | macOS | Seatbelt: `/usr/bin/sandbox-exec -p <profile>` with a profile generated per command ([`seatbelt`]) | `full`: writes confined, `.git` read-only, credentials and `path_deny` unreadable, network denied (loopback included) |
//! | Linux | Landlock, applied by a re-executed helper (`sandbox::landlock`, Linux builds only) that the host binary dispatches to | `partial`: file system per the kernel's ABI; network denial covers TCP only (Landlock has no UDP or raw-socket rules); no seccomp yet |
//! | Windows | none (the crate does not build there yet; restricted tokens are phase D3) | `none` |
//!
//! What every confined command gets, whatever the mode:
//!
//! * **`.git` is read-only** at any depth and in any case under the
//!   [`SandboxRequest::git_roots`]: hooks, config, `commondir`, the object
//!   store and the refs are where code and checkpoints live, and the host
//!   runs git there.
//! * **Credentials are unreadable**: [`credential_paths`] (SSH, cloud, git
//!   and browser credentials, the keychains), the host's own data
//!   directory, and the workspace's `path_deny` files. Metadata stays
//!   visible (`ls`, `git status` work); contents do not. The keychain's mach
//!   services are not reachable unless the host allows it.
//! * **No listening sockets** when the network is allowed, unless the host
//!   allows it.
//!
//! [`SandboxMode::FullAccess`] with the network on is no sandbox at all and
//! reports [`Enforcement::None`].
//!
//! When a mode needs confinement the machine cannot give, [`prepare`]
//! refuses ([`ErrorCode::SandboxUnavailable`]) unless the host opted into
//! running unenforced; the result always states the enforcement level, so
//! the UI can show it.

use std::path::{Path, PathBuf};

use serde::Serialize;

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;

/// How much of the requested confinement the OS enforces.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Enforcement {
    Full,
    Partial,
    None,
}

impl Enforcement {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Full => "full",
            Self::Partial => "partial",
            Self::None => "none",
        }
    }
}

/// What one command may do.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct SandboxRequest {
    pub mode: SandboxMode,
    pub network: bool,
    /// Canonical directories writable in [`SandboxMode::WorkspaceWrite`]:
    /// the workspace root and the session's temporary directory.
    pub writable_roots: Vec<PathBuf>,
    /// Canonical paths under a writable root that stay read-only.
    pub protected: Vec<PathBuf>,
    /// Canonical directories under which every `.git` entry (file or
    /// directory, at any depth, in any case) and its whole subtree stay
    /// read-only, whatever the mode: the workspace.
    pub git_roots: Vec<PathBuf>,
    /// Canonical files or directories whose contents cannot be read or
    /// written (their metadata stays visible). A final component ending in
    /// `*` matches every name with that prefix (`~/.config/elitea*`).
    pub deny_paths: Vec<PathBuf>,
    /// `path_deny` globs (see [`crate::workspace::Workspace::open`]), denied
    /// like [`Self::deny_paths`] under [`Self::deny_root`].
    pub deny_globs: Vec<String>,
    /// The canonical workspace root the globs are relative to.
    pub deny_root: Option<PathBuf>,
    /// With the network on: may the command listen for connections.
    pub allow_listen: bool,
    /// May the command reach the keychain's services.
    pub allow_keychain: bool,
}

impl SandboxRequest {
    /// A request with only a mode and the network switch; nothing denied.
    #[must_use]
    pub fn new(mode: SandboxMode, network: bool) -> Self {
        Self {
            mode,
            network,
            ..Self::default()
        }
    }

    /// Whether this request needs no confinement at all.
    #[must_use]
    pub fn is_unconfined(&self) -> bool {
        self.mode == SandboxMode::FullAccess && self.network
    }
}

/// Credentials a command never needs to read: SSH and GPG keys, cloud and
/// container credentials, git credentials, the keychains, browser profiles
/// (cookies and saved passwords), and Elitea's own configuration. Caches
/// and toolchains (`~/.cargo`, `~/.npm`, `~/.rustup`) stay readable.
#[must_use]
pub fn credential_paths(home: &Path) -> Vec<PathBuf> {
    const RELATIVE: &[&str] = &[
        ".ssh",
        ".gnupg",
        ".aws",
        ".azure",
        ".config/gcloud",
        ".kube",
        ".docker/config.json",
        ".netrc",
        ".git-credentials",
        ".config/git/credentials",
        ".config/gh",
        ".config/hub",
        ".cargo/credentials",
        ".cargo/credentials.toml",
        ".pypirc",
        ".terraform.d/credentials.tfrc.json",
        ".password-store",
        ".local/share/keyrings",
        ".config/elitea*",
        ".mozilla",
        ".config/google-chrome",
        ".config/chromium",
        ".config/BraveSoftware",
        ".config/microsoft-edge",
        "Library/Keychains",
        "Library/Cookies",
        "Library/Safari",
        "Library/Application Support/Google/Chrome",
        "Library/Application Support/Chromium",
        "Library/Application Support/BraveSoftware",
        "Library/Application Support/Microsoft Edge",
        "Library/Application Support/Firefox",
        "Library/Application Support/Arc",
        "Library/Application Support/Vivaldi",
        "Library/Application Support/com.operasoftware.Opera",
        "Library/Application Support/elitea*",
    ];
    let home = std::fs::canonicalize(home).unwrap_or_else(|_| home.to_path_buf());
    RELATIVE
        .iter()
        .map(|relative| home.join(relative))
        .collect()
}

/// The host's sandbox settings.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct SandboxConfig {
    /// Linux: the executable that dispatches `landlock::HELPER_FLAG` to
    /// `landlock::run_if_requested` (normally the host binary itself).
    /// Without it, nothing is enforced on Linux.
    pub linux_helper: Option<PathBuf>,
    /// Run commands even when the requested confinement cannot be enforced
    /// at all. Off: such commands are refused.
    pub allow_unenforced: bool,
}

/// A command ready to spawn: the program, its arguments, and what the
/// sandbox around it enforces.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Prepared {
    pub program: PathBuf,
    pub args: Vec<String>,
    pub enforcement: Enforcement,
}

fn unwrapped(argv: &[String], enforcement: Enforcement) -> ToolResult<Prepared> {
    let (program, rest) = argv
        .split_first()
        .ok_or_else(|| ToolError::invalid("an empty command"))?;
    Ok(Prepared {
        program: PathBuf::from(program),
        args: rest.to_vec(),
        enforcement,
    })
}

/// Wrap `argv` in this OS's sandbox for `request`.
///
/// # Errors
///
/// [`ErrorCode::SandboxUnavailable`] when confinement is needed, none can
/// be enforced, and the host did not allow running unenforced.
pub fn prepare(
    request: &SandboxRequest,
    argv: &[String],
    config: &SandboxConfig,
) -> ToolResult<Prepared> {
    if request.is_unconfined() {
        // Full access with the network on: nothing is confined.
        return unwrapped(argv, Enforcement::None);
    }
    match platform_prepare(request, argv, config)? {
        Some(prepared) => Ok(prepared),
        None if config.allow_unenforced => unwrapped(argv, Enforcement::None),
        None => Err(ToolError::new(
            ErrorCode::SandboxUnavailable,
            format!(
                "the {} sandbox{} cannot be enforced on this machine",
                request.mode.as_str(),
                if request.network {
                    ""
                } else {
                    " without network"
                }
            ),
        )),
    }
}

#[cfg(target_os = "macos")]
#[allow(clippy::unnecessary_wraps)] // one signature on every OS; Linux can fail
fn platform_prepare(
    request: &SandboxRequest,
    argv: &[String],
    _config: &SandboxConfig,
) -> ToolResult<Option<Prepared>> {
    const SANDBOX_EXEC: &str = "/usr/bin/sandbox-exec";
    if !std::path::Path::new(SANDBOX_EXEC).is_file() {
        return Ok(None);
    }
    let (profile, params) = seatbelt::profile(request);
    let mut wrapped = vec!["-p".to_owned(), profile];
    for (name, value) in params {
        wrapped.push("-D".to_owned());
        wrapped.push(format!("{name}={value}"));
    }
    wrapped.push("--".to_owned());
    wrapped.extend(argv.iter().cloned());
    Ok(Some(Prepared {
        program: PathBuf::from(SANDBOX_EXEC),
        args: wrapped,
        enforcement: Enforcement::Full,
    }))
}

#[cfg(target_os = "linux")]
fn platform_prepare(
    request: &SandboxRequest,
    argv: &[String],
    config: &SandboxConfig,
) -> ToolResult<Option<Prepared>> {
    let Some(helper) = &config.linux_helper else {
        return Ok(None);
    };
    Ok(Some(Prepared {
        program: helper.clone(),
        args: landlock::helper_args(request, argv)?,
        enforcement: Enforcement::Partial,
    }))
}

#[cfg(not(any(target_os = "macos", target_os = "linux")))]
#[allow(clippy::unnecessary_wraps)]
fn platform_prepare(
    _request: &SandboxRequest,
    _argv: &[String],
    _config: &SandboxConfig,
) -> ToolResult<Option<Prepared>> {
    Ok(None)
}

/// macOS Seatbelt profiles.
///
/// Written for this crate; the shape follows the public approach of
/// sandboxing CLI agents with `sandbox-exec` (deny by default, allow reads,
/// allow writes under parameterised roots, keep sub-paths read-only with
/// `require-not`, network as a separate switch). Later rules win, so the
/// denials come after the allowances. Paths and the regular expressions
/// built from them travel as `-D` parameters, never spliced into the
/// profile text, so no path can change the profile's meaning.
pub mod seatbelt {
    use std::fmt::Write as _;
    use std::path::Path;

    use unicode_normalization::UnicodeNormalization;

    use super::SandboxRequest;
    use crate::policy::SandboxMode;

    /// Services nearly every command-line program looks up (user and group
    /// names, logging, notifications, certificate trust, preferences). Not
    /// the keychain (`com.apple.SecurityServer`): see
    /// [`KEYCHAIN_SERVICES`].
    const MACH_SERVICES: &[&str] = &[
        "com.apple.system.opendirectoryd.libinfo",
        "com.apple.system.opendirectoryd.membership",
        "com.apple.system.notification_center",
        "com.apple.system.logger",
        "com.apple.logd",
        "com.apple.diagnosticd",
        "com.apple.trustd.agent",
        "com.apple.cfprefsd.daemon",
        "com.apple.cfprefsd.agent",
        "com.apple.coreservices.launchservicesd",
        "com.apple.lsd.mapdb",
        "com.apple.CoreServices.coreservicesd",
        "com.apple.PowerManagement.control",
    ];

    /// The keychain and credential services, only when the host allows it.
    pub const KEYCHAIN_SERVICES: &[&str] = &[
        "com.apple.SecurityServer",
        "com.apple.securityd.xpc",
        "com.apple.security.agent",
        "com.apple.secd",
        "com.apple.security.keychain-circle-notification",
    ];

    /// Name resolution, only when the network is allowed.
    const DNS_SERVICES: &[&str] = &["com.apple.dnssd.service", "com.apple.mDNSResponder"];

    const BASE: &str = r#"(version 1)
(deny default)

; processes: run and signal programs inside this sandbox
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))

; reads, except the denials below
(allow file-read*)
(allow sysctl-read)
(allow user-preference-read)
(allow ipc-posix-sem)
(allow ipc-posix-shm)

; terminals and the usual device files
(allow pseudo-tty)
(allow file-ioctl (literal "/dev/tty") (literal "/dev/ptmx") (regex #"^/dev/ttys[0-9]+$"))
(allow file-write-data
  (literal "/dev/null")
  (literal "/dev/zero")
  (literal "/dev/tty")
  (literal "/dev/dtracehelper")
  (literal "/dev/ptmx")
  (regex #"^/dev/ttys[0-9]+$")
  (regex #"^/dev/fd/[0-9]+$"))
"#;

    fn mach_lookup(services: &[&str]) -> String {
        let mut out = "(allow mach-lookup".to_owned();
        for service in services {
            let _ = write!(out, "\n  (global-name \"{service}\")");
        }
        out.push_str(")\n");
        out
    }

    /// One character as a POSIX extended regular expression literal.
    fn push_literal(out: &mut String, c: char) {
        match c {
            '^' | '\\' | '[' | ']' => {
                out.push('\\');
                out.push(c);
            }
            '.' | '*' | '+' | '?' | '(' | ')' | '|' | '{' | '}' | '$' => {
                out.push('[');
                out.push(c);
                out.push(']');
            }
            _ => out.push(c),
        }
    }

    /// `text` as a regular expression matching exactly it.
    #[must_use]
    pub fn escape(text: &str) -> String {
        let mut out = String::new();
        for c in text.chars() {
            push_literal(&mut out, c);
        }
        out
    }

    /// One glob character as a regular expression: both cases when
    /// `fold_case`, and both Unicode normal forms (APFS keeps names as they
    /// were written, NFC or NFD).
    fn push_glob_char(out: &mut String, c: char, fold_case: bool) {
        let mut forms: Vec<String> = Vec::new();
        let mut add = |form: String| {
            if !form.is_empty() && !forms.contains(&form) {
                forms.push(form);
            }
        };
        let variants: Vec<String> = if fold_case {
            vec![
                c.to_string(),
                c.to_lowercase().collect(),
                c.to_uppercase().collect(),
            ]
        } else {
            vec![c.to_string()]
        };
        for variant in variants {
            add(variant.nfc().collect());
            add(variant.nfd().collect());
        }
        let single = forms.iter().all(|form| form.chars().count() == 1);
        if forms.len() == 1 {
            for c in forms[0].chars() {
                push_literal(out, c);
            }
        } else if single
            && forms
                .iter()
                .all(|form| form.chars().all(char::is_alphanumeric))
        {
            out.push('[');
            for form in &forms {
                out.push_str(form);
            }
            out.push(']');
        } else {
            out.push('(');
            for (index, form) in forms.iter().enumerate() {
                if index > 0 {
                    out.push('|');
                }
                for c in form.chars() {
                    push_literal(out, c);
                }
            }
            out.push(')');
        }
    }

    /// A glob (the `globset` syntax `path_deny` uses) as a regular
    /// expression body over `/`-separated paths; `None` for a malformed one.
    #[must_use]
    pub fn glob_body(glob: &str, fold_case: bool) -> Option<String> {
        let chars: Vec<char> = glob.chars().collect();
        let mut out = String::new();
        let mut braces = 0usize;
        let mut index = 0usize;
        while index < chars.len() {
            let c = chars[index];
            match c {
                '*' if chars.get(index + 1) == Some(&'*') => {
                    index += 1;
                    if chars.get(index + 1) == Some(&'/') {
                        index += 1;
                        out.push_str("(.*/)?");
                    } else {
                        out.push_str(".*");
                    }
                }
                '*' => out.push_str("[^/]*"),
                '?' => out.push_str("[^/]"),
                '[' => {
                    let mut end = index + 1;
                    if matches!(chars.get(end), Some('!' | '^')) {
                        end += 1;
                    }
                    if chars.get(end) == Some(&']') {
                        end += 1;
                    }
                    while chars.get(end).is_some_and(|c| *c != ']') {
                        end += 1;
                    }
                    if end >= chars.len() {
                        return None;
                    }
                    out.push('[');
                    let mut inner = index + 1;
                    if matches!(chars.get(inner), Some('!' | '^')) {
                        out.push('^');
                        inner += 1;
                    }
                    for c in &chars[inner..end] {
                        if fold_case && c.is_alphabetic() {
                            out.extend(c.to_lowercase());
                            out.extend(c.to_uppercase());
                        } else {
                            out.push(*c);
                        }
                    }
                    out.push(']');
                    index = end;
                }
                '{' => {
                    braces += 1;
                    out.push('(');
                }
                ',' if braces > 0 => out.push('|'),
                '}' if braces > 0 => {
                    braces -= 1;
                    out.push(')');
                }
                '\\' => {
                    index += 1;
                    push_glob_char(&mut out, *chars.get(index)?, fold_case);
                }
                _ => push_glob_char(&mut out, c, fold_case),
            }
            index += 1;
        }
        (braces == 0).then_some(out)
    }

    /// The regular expression denying `pattern` (a `path_deny` entry) under
    /// `root`, and everything beneath what it matches.
    #[must_use]
    pub fn deny_glob_regex(root: &Path, pattern: &str, fold_case: bool) -> Option<String> {
        let normalised: String = pattern.nfc().collect();
        let trimmed = normalised.trim().trim_start_matches("./");
        if trimmed.is_empty() {
            return None;
        }
        let anchored = if trimmed.contains('/') {
            trimmed.trim_start_matches('/').to_owned()
        } else {
            format!("**/{trimmed}")
        };
        let body = glob_body(&anchored, fold_case)?;
        Some(format!(
            "^{}/{body}(/.*)?$",
            escape(&root.display().to_string())
        ))
    }

    /// The regular expression for every `.git` entry under `root`.
    #[must_use]
    pub fn git_regex(root: &Path) -> String {
        format!(
            "^{}/(.*/)?[.][gG][iI][tT](/|$)",
            escape(&root.display().to_string())
        )
    }

    /// A denied path as a filter: a subpath, or a name prefix when its last
    /// component ends with `*`.
    fn deny_filter(path: &Path, name: &str, params: &mut Vec<(String, String)>) -> String {
        let text = path.display().to_string();
        if let Some(prefix) = text.strip_suffix('*') {
            params.push((name.to_owned(), format!("^{}[^/]*(/|$)", escape(prefix))));
            format!("(regex (param \"{name}\"))")
        } else {
            params.push((name.to_owned(), text));
            format!("(subpath (param \"{name}\"))")
        }
    }

    /// Credentials, the host's data directory and `path_deny`: contents
    /// neither read nor written; then the writable roots inside a denied
    /// directory are opened again.
    fn push_denials(
        text: &mut String,
        params: &mut Vec<(String, String)>,
        request: &SandboxRequest,
    ) {
        let mut denied = Vec::new();
        for (index, path) in request.deny_paths.iter().enumerate() {
            denied.push(deny_filter(path, &format!("DENY_{index}"), params));
        }
        if let Some(root) = &request.deny_root {
            for (index, pattern) in request.deny_globs.iter().enumerate() {
                if let Some(regex) =
                    deny_glob_regex(root, pattern, crate::workspace::CASE_INSENSITIVE_FS)
                {
                    let name = format!("DENY_GLOB_{index}");
                    denied.push(format!("(regex (param \"{name}\"))"));
                    params.push((name, regex));
                }
            }
        }
        if !denied.is_empty() {
            text.push_str("\n; credentials and path_deny: contents neither read nor written\n(deny file-read-data file-write*");
            for filter in &denied {
                let _ = write!(text, "\n  {filter}");
            }
            text.push_str(")\n");
        }
        // A writable root inside a denied directory (the session's temporary
        // directory lives in the host's data directory) stays usable.
        let reopened: Vec<usize> = request
            .writable_roots
            .iter()
            .enumerate()
            .filter(|(_, root)| {
                request.deny_paths.iter().any(|path| {
                    let text = path.display().to_string();
                    text.strip_suffix('*').map_or_else(
                        || root.starts_with(path),
                        |prefix| root.display().to_string().starts_with(prefix),
                    )
                })
            })
            .map(|(index, _)| index)
            .collect();
        for index in reopened {
            let _ = writeln!(
                text,
                "(allow file-read* file-read-data (subpath (param \"WRITABLE_ROOT_{index}\")))"
            );
            if request.mode == SandboxMode::WorkspaceWrite {
                let _ = writeln!(
                    text,
                    "(allow file-write* (subpath (param \"WRITABLE_ROOT_{index}\")))"
                );
            }
        }
    }

    /// The profile text and its `-D` parameters for `request`.
    #[must_use]
    pub fn profile(request: &SandboxRequest) -> (String, Vec<(String, String)>) {
        let mut text = BASE.to_owned();
        text.push_str(&mach_lookup(MACH_SERVICES));
        if request.allow_keychain {
            text.push_str("\n; the keychain, allowed by the host\n");
            text.push_str(&mach_lookup(KEYCHAIN_SERVICES));
        }
        let mut params = Vec::new();
        for (index, root) in request.writable_roots.iter().enumerate() {
            params.push((format!("WRITABLE_ROOT_{index}"), root.display().to_string()));
        }
        match request.mode {
            SandboxMode::ReadOnly => {
                text.push_str("\n; read-only: no writes beyond the devices above\n");
            }
            SandboxMode::FullAccess => {
                text.push_str("\n; full access to the file system\n(allow file-write*)\n");
            }
            SandboxMode::WorkspaceWrite => {
                text.push_str("\n; writes only under the workspace and the session's temporary directory\n(allow file-write*");
                for (index, root) in request.writable_roots.iter().enumerate() {
                    let name = format!("WRITABLE_ROOT_{index}");
                    let protected: Vec<_> = request
                        .protected
                        .iter()
                        .filter(|path| path.starts_with(root))
                        .collect();
                    if protected.is_empty() {
                        let _ = write!(text, "\n  (subpath (param \"{name}\"))");
                    } else {
                        let _ = write!(text, "\n  (require-all (subpath (param \"{name}\"))");
                        for (inner, path) in protected.iter().enumerate() {
                            let protected_name = format!("{name}_PROTECTED_{inner}");
                            let _ = write!(
                                text,
                                " (require-not (subpath (param \"{protected_name}\")))"
                            );
                            params.push((protected_name, path.display().to_string()));
                        }
                        text.push(')');
                    }
                }
                text.push_str(")\n");
            }
        }

        push_denials(&mut text, &mut params, request);

        if request.mode != SandboxMode::ReadOnly && !request.git_roots.is_empty() {
            text.push_str(
                "\n; every .git entry, any depth, any case: read-only\n(deny file-write*",
            );
            for (index, root) in request.git_roots.iter().enumerate() {
                let name = format!("GIT_ROOT_{index}");
                let _ = write!(text, "\n  (regex (param \"{name}\"))");
                params.push((name, git_regex(root)));
            }
            text.push_str(")\n");
        }

        if request.network {
            text.push_str("\n; network\n(allow network-outbound)\n(allow network-inbound)\n(allow system-socket)\n");
            text.push_str(&mach_lookup(DNS_SERVICES));
            if !request.allow_listen {
                text.push_str("; no listening sockets\n(deny network-bind)\n");
            }
        } else {
            text.push_str(
                "\n; no network: every socket, loopback included, is denied by default\n",
            );
        }
        (text, params)
    }
}

/// Linux Landlock, applied in a helper process before `exec`.
///
/// Landlock restricts the calling thread, so it must run in the child
/// between `fork` and `exec`. Doing that from `pre_exec` needs `unsafe`,
/// which this workspace forbids; instead the host binary is re-executed
/// with [`HELPER_FLAG`], restricts itself (single-threaded, at the top of
/// `main`) and `exec`s the command. The host calls [`run_if_requested`]
/// first thing in `main` and passes its own path as
/// [`super::SandboxConfig::linux_helper`].
#[cfg(target_os = "linux")]
pub mod landlock {
    use std::ffi::OsString;
    use std::os::unix::process::CommandExt;
    use std::path::PathBuf;

    use landlock::{
        ABI, Access, AccessFs, AccessNet, Ruleset, RulesetAttr, RulesetCreatedAttr, RulesetStatus,
        path_beneath_rules,
    };

    use super::SandboxRequest;
    use crate::error::{ToolError, ToolResult};
    use crate::policy::SandboxMode;

    /// The first argument that turns the host binary into the helper.
    pub const HELPER_FLAG: &str = "--elitea-landlock-exec";

    /// The exit status when the sandbox cannot be applied.
    pub const EXIT_UNAVAILABLE: i32 = 125;

    /// The highest ABI this code asks for; older kernels get best effort.
    const TARGET_ABI: ABI = ABI::V5;

    /// The helper's arguments: flag, mode, network, roots, `--`, argv.
    ///
    /// # Errors
    ///
    /// When a root is not UTF-8.
    pub fn helper_args(request: &SandboxRequest, argv: &[String]) -> ToolResult<Vec<String>> {
        let mut helper = vec![
            HELPER_FLAG.to_owned(),
            request.mode.as_str().to_owned(),
            if request.network { "net" } else { "no-net" }.to_owned(),
        ];
        for root in &request.writable_roots {
            helper.push(
                root.to_str()
                    .ok_or_else(|| ToolError::invalid("a writable root is not UTF-8"))?
                    .to_owned(),
            );
        }
        helper.push("--".to_owned());
        helper.extend(argv.iter().cloned());
        Ok(helper)
    }

    fn restrict(
        mode: SandboxMode,
        network: bool,
        roots: &[PathBuf],
    ) -> Result<RulesetStatus, landlock::RulesetError> {
        let mut ruleset = Ruleset::default();
        if mode != SandboxMode::FullAccess {
            ruleset = ruleset.handle_access(AccessFs::from_all(TARGET_ABI))?;
        }
        if !network {
            ruleset = ruleset.handle_access(AccessNet::from_all(TARGET_ABI))?;
        }
        let mut created = ruleset.create()?;
        if mode != SandboxMode::FullAccess {
            created =
                created.add_rules(path_beneath_rules(["/"], AccessFs::from_read(TARGET_ABI)))?;
            created = created.add_rules(path_beneath_rules(
                [
                    "/dev/null",
                    "/dev/zero",
                    "/dev/tty",
                    "/dev/ptmx",
                    "/dev/pts",
                ],
                AccessFs::from_all(TARGET_ABI),
            ))?;
            if mode == SandboxMode::WorkspaceWrite {
                created =
                    created.add_rules(path_beneath_rules(roots, AccessFs::from_all(TARGET_ABI)))?;
            }
        }
        Ok(created.restrict_self()?.ruleset)
    }

    /// If this process was started as the helper, restrict it and `exec`
    /// the command; never returns then. Otherwise returns at once.
    pub fn run_if_requested() {
        let args: Vec<OsString> = std::env::args_os().collect();
        if args.get(1).is_none_or(|flag| flag != HELPER_FLAG) {
            return;
        }
        let text: Vec<String> = args
            .iter()
            .skip(2)
            .filter_map(|arg| arg.to_str().map(str::to_owned))
            .collect();
        let fail = |message: &str| -> ! {
            eprintln!("elitea sandbox: {message}");
            std::process::exit(EXIT_UNAVAILABLE);
        };
        let Some(separator) = text.iter().position(|arg| arg == "--") else {
            fail("malformed helper arguments");
        };
        let (head, command) = (&text[..separator], &text[separator + 1..]);
        let mode = match head.first().map(String::as_str) {
            Some("read-only") => SandboxMode::ReadOnly,
            Some("workspace-write") => SandboxMode::WorkspaceWrite,
            Some("full-access") => SandboxMode::FullAccess,
            _ => fail("unknown sandbox mode"),
        };
        let network = head.get(1).is_some_and(|value| value == "net");
        let roots: Vec<PathBuf> = head.iter().skip(2).map(PathBuf::from).collect();
        match restrict(mode, network, &roots) {
            Ok(RulesetStatus::NotEnforced) => fail("Landlock is not available on this kernel"),
            Ok(_) => {}
            Err(_) => fail("the Landlock ruleset could not be applied"),
        }
        let Some((program, rest)) = command.split_first() else {
            fail("no command");
        };
        let error = std::process::Command::new(program).args(rest).exec();
        eprintln!("elitea sandbox: cannot run the command: {}", error.kind());
        std::process::exit(127);
    }
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};

    use super::{Enforcement, SandboxConfig, SandboxRequest, credential_paths, prepare, seatbelt};
    use crate::error::ErrorCode;
    use crate::policy::SandboxMode;

    fn request(mode: SandboxMode, network: bool) -> SandboxRequest {
        SandboxRequest {
            writable_roots: vec![PathBuf::from("/work/space"), PathBuf::from("/tmp/session")],
            protected: vec![PathBuf::from("/work/space/vendor")],
            ..SandboxRequest::new(mode, network)
        }
    }

    #[test]
    fn profiles_parameterise_paths_and_switch_network() {
        let (text, params) = seatbelt::profile(&request(SandboxMode::WorkspaceWrite, false));
        assert!(text.starts_with("(version 1)\n(deny default)"));
        assert!(
            !text.contains("/work/space"),
            "paths are parameters, not profile text"
        );
        assert!(text.contains("(require-all (subpath (param \"WRITABLE_ROOT_0\")) (require-not (subpath (param \"WRITABLE_ROOT_0_PROTECTED_0\"))))"));
        assert!(text.contains("(subpath (param \"WRITABLE_ROOT_1\"))"));
        assert!(!text.contains("(allow network"));
        assert!(params.contains(&("WRITABLE_ROOT_0".to_owned(), "/work/space".to_owned())));
        assert!(params.contains(&(
            "WRITABLE_ROOT_0_PROTECTED_0".to_owned(),
            "/work/space/vendor".to_owned()
        )));

        let (read_only, _) = seatbelt::profile(&request(SandboxMode::ReadOnly, true));
        assert!(!read_only.contains("(allow file-write*"));
        assert!(read_only.contains("(allow network-outbound)"));

        let (full, _) = seatbelt::profile(&request(SandboxMode::FullAccess, false));
        assert!(full.contains("(allow file-write*)\n"));
        assert!(!full.contains("(allow network"));
    }

    #[test]
    fn git_entries_are_read_only_at_any_depth_and_case() {
        let mut req = request(SandboxMode::WorkspaceWrite, false);
        req.git_roots = vec![PathBuf::from("/work/space")];
        let (text, params) = seatbelt::profile(&req);
        let deny = text.find("(deny file-write*").expect("a .git denial");
        let allow = text
            .find("(allow file-write*")
            .expect("the workspace allowance");
        assert!(deny > allow, "the denial comes last, so it wins");
        assert!(params.contains(&(
            "GIT_ROOT_0".to_owned(),
            "^/work/space/(.*/)?[.][gG][iI][tT](/|$)".to_owned()
        )));
        let full = SandboxRequest {
            mode: SandboxMode::FullAccess,
            ..req.clone()
        };
        assert!(seatbelt::profile(&full).0.contains("GIT_ROOT_0"));
    }

    #[test]
    fn credentials_path_deny_keychain_and_listening_are_denied_by_default() {
        let mut req = request(SandboxMode::WorkspaceWrite, true);
        req.deny_paths = vec![
            PathBuf::from("/home/me/.ssh"),
            PathBuf::from("/home/me/.config/elitea*"),
        ];
        req.deny_globs = vec![".env".to_owned(), "secrets/**".to_owned()];
        req.deny_root = Some(PathBuf::from("/work/space"));
        let (text, params) = seatbelt::profile(&req);
        assert!(text.contains("(deny file-read-data file-write*"));
        assert!(text.contains("(subpath (param \"DENY_0\"))"));
        assert!(params.contains(&(
            "DENY_1".to_owned(),
            "^/home/me/[.]config/elitea[^/]*(/|$)".to_owned()
        )));
        assert!(
            !text.contains("com.apple.SecurityServer"),
            "no keychain by default"
        );
        assert!(text.contains("(deny network-bind)"));
        let env = params
            .iter()
            .find(|(name, _)| name == "DENY_GLOB_0")
            .map(|(_, value)| value.clone())
            .expect("glob");
        assert!(env.starts_with("^/work/space/(.*/)?"), "{env}");
        assert!(env.ends_with("(/.*)?$"), "{env}");

        req.allow_keychain = true;
        req.allow_listen = true;
        let (allowed, _) = seatbelt::profile(&req);
        assert!(allowed.contains("com.apple.SecurityServer"));
        assert!(!allowed.contains("(deny network-bind)"));

        let home = credential_paths(Path::new("/nonexistent-home"));
        for wanted in [
            ".ssh",
            ".aws",
            ".kube",
            ".netrc",
            ".git-credentials",
            "Library/Keychains",
        ] {
            assert!(
                home.contains(&Path::new("/nonexistent-home").join(wanted)),
                "{wanted}"
            );
        }
    }

    #[test]
    fn globs_translate_to_anchored_regexes() {
        let root = Path::new("/w");
        let regex = |glob: &str, fold| seatbelt::deny_glob_regex(root, glob, fold).expect("glob");
        assert_eq!(regex("*.pem", false), "^/w/(.*/)?[^/]*[.]pem(/.*)?$");
        assert_eq!(regex("secrets/**", false), "^/w/secrets/.*(/.*)?$");
        assert_eq!(regex("a?{b,c}", false), "^/w/(.*/)?a[^/](b|c)(/.*)?$");
        assert_eq!(regex("ab", true), "^/w/(.*/)?[aA][bB](/.*)?$");
        assert!(
            regex("caf\u{e9}", false).contains("(\u{e9}|e\u{301})"),
            "both normal forms"
        );
        assert_eq!(seatbelt::escape("/a.b(c)^"), "/a[.]b[(]c[)]\\^");
        assert!(seatbelt::glob_body("[abc", false).is_none());
    }

    #[test]
    fn unconfined_requests_run_as_they_are_and_say_so() {
        let argv = vec!["echo".to_owned(), "hi".to_owned()];
        let prepared = prepare(
            &request(SandboxMode::FullAccess, true),
            &argv,
            &SandboxConfig::default(),
        )
        .expect("prepare");
        assert_eq!(prepared.program, PathBuf::from("echo"));
        assert_eq!(
            prepared.enforcement,
            Enforcement::None,
            "full access with the network is no sandbox"
        );
    }

    #[test]
    fn confinement_that_cannot_be_enforced_is_refused_unless_allowed() {
        let argv = vec!["true".to_owned()];
        let req = request(SandboxMode::WorkspaceWrite, false);
        let refused = prepare(&req, &argv, &SandboxConfig::default());
        if cfg!(target_os = "linux") {
            if let Ok(prepared) = &refused {
                // bubblewrap is installed: it enforces.
                assert_eq!(prepared.enforcement, Enforcement::Full);
                return;
            }
            assert_eq!(
                refused.expect_err("no helper").code(),
                ErrorCode::SandboxUnavailable
            );
            let allowed = prepare(
                &req,
                &argv,
                &SandboxConfig {
                    allow_unenforced: true,
                    ..SandboxConfig::default()
                },
            )
            .expect("allowed");
            assert_eq!(allowed.enforcement, Enforcement::None);
        } else if cfg!(target_os = "macos") {
            let prepared = refused.expect("seatbelt");
            assert_eq!(prepared.program, PathBuf::from("/usr/bin/sandbox-exec"));
            assert_eq!(prepared.enforcement, Enforcement::Full);
            assert_eq!(
                &prepared.args[prepared.args.len() - 2..],
                ["--".to_owned(), "true".to_owned()]
            );
        }
    }
}
