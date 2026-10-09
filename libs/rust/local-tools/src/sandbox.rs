//! The OS sandbox commands run under (ADR-0029 decision 4).
//!
//! | OS | Mechanism | Reported enforcement |
//! |---|---|---|
//! | macOS | Seatbelt: `/usr/bin/sandbox-exec -p <profile>` with a profile generated per command ([`seatbelt`]) | `full`: writes confined, network denied (loopback included) |
//! | Linux | Landlock, applied by a re-executed helper (`sandbox::landlock`, Linux builds only) that the host binary dispatches to | `partial`: file system per the kernel's ABI; network denial covers TCP only (Landlock has no UDP or raw-socket rules); no seccomp yet |
//! | Windows | none (the crate does not build there yet; restricted tokens are phase D3) | `none` |
//!
//! Reads are never confined: a command sees what the person's account sees.
//! `path_deny` is enforced by the file tools, not inside commands.
//!
//! When a mode needs confinement the machine cannot give, [`prepare`]
//! refuses ([`ErrorCode::SandboxUnavailable`]) unless the host opted into
//! running unenforced; the result always states the enforcement level, so
//! the UI can show it.

use std::path::PathBuf;

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
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SandboxRequest {
    pub mode: SandboxMode,
    pub network: bool,
    /// Canonical directories writable in [`SandboxMode::WorkspaceWrite`]:
    /// the workspace root and the session's temporary directory.
    pub writable_roots: Vec<PathBuf>,
    /// Canonical paths under a writable root that stay read-only (the
    /// workspace's `.git/hooks` and `.git/config`: code runs from there).
    pub protected: Vec<PathBuf>,
}

impl SandboxRequest {
    /// Whether this request needs no confinement at all.
    #[must_use]
    pub fn is_unconfined(&self) -> bool {
        self.mode == SandboxMode::FullAccess && self.network
    }
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
        return unwrapped(argv, Enforcement::Full);
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
/// `require-not`, network as a separate switch). Paths travel as `-D`
/// parameters, never spliced into the profile text, so no path can change
/// the profile's meaning.
pub mod seatbelt {
    use std::fmt::Write as _;

    use super::SandboxRequest;
    use crate::policy::SandboxMode;

    /// Services nearly every command-line program looks up (user and group
    /// names, logging, notifications, certificate trust, preferences).
    const MACH_SERVICES: &[&str] = &[
        "com.apple.system.opendirectoryd.libinfo",
        "com.apple.system.opendirectoryd.membership",
        "com.apple.system.notification_center",
        "com.apple.system.logger",
        "com.apple.logd",
        "com.apple.diagnosticd",
        "com.apple.SecurityServer",
        "com.apple.trustd.agent",
        "com.apple.cfprefsd.daemon",
        "com.apple.cfprefsd.agent",
        "com.apple.coreservices.launchservicesd",
        "com.apple.lsd.mapdb",
        "com.apple.CoreServices.coreservicesd",
        "com.apple.PowerManagement.control",
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

; reads are not confined
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

    /// The profile text and its `-D` parameters for `request`.
    #[must_use]
    pub fn profile(request: &SandboxRequest) -> (String, Vec<(String, String)>) {
        let mut text = BASE.to_owned();
        text.push_str(&mach_lookup(MACH_SERVICES));
        let mut params = Vec::new();
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
                    params.push((name, root.display().to_string()));
                }
                text.push_str(")\n");
            }
        }
        if request.network {
            text.push_str("\n; network\n(allow network-outbound)\n(allow network-inbound)\n(allow system-socket)\n");
            text.push_str(&mach_lookup(DNS_SERVICES));
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
    use std::path::PathBuf;

    use super::{Enforcement, SandboxConfig, SandboxRequest, prepare, seatbelt};
    use crate::error::ErrorCode;
    use crate::policy::SandboxMode;

    fn request(mode: SandboxMode, network: bool) -> SandboxRequest {
        SandboxRequest {
            mode,
            network,
            writable_roots: vec![PathBuf::from("/work/space"), PathBuf::from("/tmp/session")],
            protected: vec![PathBuf::from("/work/space/.git/hooks")],
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
            "/work/space/.git/hooks".to_owned()
        )));

        let (read_only, params) = seatbelt::profile(&request(SandboxMode::ReadOnly, true));
        assert!(!read_only.contains("(allow file-write*"));
        assert!(read_only.contains("(allow network-outbound)"));
        assert!(params.is_empty());

        let (full, _) = seatbelt::profile(&request(SandboxMode::FullAccess, false));
        assert!(full.contains("(allow file-write*)\n"));
        assert!(!full.contains("(allow network"));
    }

    #[test]
    fn unconfined_requests_run_as_they_are() {
        let argv = vec!["echo".to_owned(), "hi".to_owned()];
        let prepared = prepare(
            &request(SandboxMode::FullAccess, true),
            &argv,
            &SandboxConfig::default(),
        )
        .expect("prepare");
        assert_eq!(prepared.program, PathBuf::from("echo"));
        assert_eq!(prepared.enforcement, Enforcement::Full);
    }

    #[test]
    fn confinement_that_cannot_be_enforced_is_refused_unless_allowed() {
        let argv = vec!["true".to_owned()];
        let req = request(SandboxMode::WorkspaceWrite, false);
        let refused = prepare(&req, &argv, &SandboxConfig::default());
        if cfg!(target_os = "linux") {
            assert_eq!(
                refused.expect_err("no helper").code(),
                ErrorCode::SandboxUnavailable
            );
            let allowed = prepare(
                &req,
                &argv,
                &SandboxConfig {
                    linux_helper: None,
                    allow_unenforced: true,
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
