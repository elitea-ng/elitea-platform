//! The person's environment for commands in a trusted folder (ADR-0061
//! decision 5c).
//!
//! A sandboxed command gets a scrubbed environment (the passthrough list in
//! [`crate::shell`]). A host command runs as the person, so it gets their
//! login-shell environment: the real `PATH`, `KUBECONFIG`, `SSH_AUTH_SOCK`,
//! `AWS_PROFILE` and the rest. The host captures it once per app session
//! ([`capture`]):
//!
//! | OS | Capture |
//! |---|---|
//! | macOS, Linux | `$SHELL -l -c 'env -0'` (a login shell, never interactive: no `-i`), stdin closed, no terminal, a scrubbed parent environment, killed after [`CAPTURE_TIMEOUT`] |
//! | Windows | the environment the app was started with, which Windows builds from the user's environment block (no login shell exists) |
//!
//! When the capture fails (no shell, a timeout, unreadable output), commands
//! get today's passthrough list plus the real `PATH`, and the environment
//! is marked a fallback ([`HostEnvironment::fallback`]) so the badge can say
//! so.
//!
//! Whatever was captured, a command never gets ([`HostEnvironment::for_command`]):
//! the app's own variables (`ELITEA_*`, `TAURI_*`), the webview's
//! (`WEBKIT_*`, `WEBVIEW2_*`), the variables a rule's grant hands a command
//! (#3006: a credential profile's login and configuration variables), a
//! proxy variable that points at the app's egress proxy, and any variable
//! whose value equals a secret the app holds. Only the names of dropped
//! variables are ever logged.

#[cfg(unix)]
use std::process::Stdio;
use std::time::Duration;

#[cfg(unix)]
use tokio::io::AsyncReadExt;

use crate::credentials::Secret;

/// How long the login shell may take to print its environment.
pub const CAPTURE_TIMEOUT: Duration = Duration::from_secs(5);

/// The most bytes of environment read from the shell.
#[cfg(unix)]
const MAX_CAPTURE_BYTES: usize = 4 * 1024 * 1024;

/// What the shell prints before the environment, so a profile that echoes
/// something cannot shift it.
#[cfg(any(unix, test))]
const MARKER: &[u8] = b"\0ELITEA_HOST_ENV\0";

/// Prefixes of names never passed to a host command (compared ignoring
/// ASCII case).
const DROPPED_PREFIXES: &[&str] = &["ELITEA_", "TAURI_", "WEBKIT_", "WEBVIEW2_", "__TAURI"];

/// The person's environment, as captured for host commands.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct HostEnvironment {
    vars: Vec<(String, String)>,
    fallback: bool,
}

impl HostEnvironment {
    /// A captured environment (tests and hosts that capture it themselves).
    #[must_use]
    pub fn captured(vars: Vec<(String, String)>) -> Self {
        Self {
            vars,
            fallback: false,
        }
    }

    /// The fallback: no capture; commands get the passthrough list and the
    /// real `PATH` ([`crate::shell::host_fallback_environment`]).
    #[must_use]
    pub const fn failed() -> Self {
        Self {
            vars: Vec::new(),
            fallback: true,
        }
    }

    /// Whether the capture failed and commands get the reduced environment.
    #[must_use]
    pub const fn fallback(&self) -> bool {
        self.fallback
    }

    /// The captured variables (empty for a fallback).
    #[must_use]
    pub fn vars(&self) -> &[(String, String)] {
        &self.vars
    }

    /// `vars` without what a host command must never get (see the module
    /// documentation): the app's and the webview's variables, a grant's
    /// variables, a proxy variable naming the app's egress proxy, and any
    /// variable whose value is one of `held` (the app's tokens).
    #[must_use]
    pub fn for_command(vars: &[(String, String)], held: &[Secret]) -> Vec<(String, String)> {
        let mut dropped = Vec::new();
        let kept = vars
            .iter()
            .filter(|(name, value)| {
                let keep = !dropped_name(name)
                    && !crate::egress_proxy::is_app_proxy_url(value)
                    && !held
                        .iter()
                        .any(|secret| !secret.expose().is_empty() && secret.expose() == value);
                if !keep {
                    dropped.push(name.clone());
                }
                keep
            })
            .cloned()
            .collect();
        if !dropped.is_empty() {
            tracing::debug!(dropped = ?dropped, "variables left out of a host command's environment");
        }
        kept
    }
}

/// Whether `name` is never passed to a host command.
fn dropped_name(name: &str) -> bool {
    let upper = name.to_ascii_uppercase();
    DROPPED_PREFIXES
        .iter()
        .any(|prefix| upper.starts_with(prefix))
        || crate::credentials::PROFILES.iter().any(|profile| {
            profile.env.eq_ignore_ascii_case(name)
                || profile
                    .config_dir_env
                    .is_some_and(|env| env.eq_ignore_ascii_case(name))
                || profile
                    .fixed_env
                    .iter()
                    .any(|(env, _)| env.eq_ignore_ascii_case(name))
        })
}

/// Capture the person's environment for this app session (see the module
/// documentation). Never fails: a capture that does not work is
/// [`HostEnvironment::failed`].
#[cfg_attr(
    not(unix),
    allow(clippy::unused_async, reason = "awaits the login shell on unix only")
)]
pub async fn capture() -> HostEnvironment {
    #[cfg(unix)]
    {
        capture_login_shell(&crate::shell::login_shell(), CAPTURE_TIMEOUT).await
    }
    #[cfg(not(unix))]
    {
        // Windows builds a process's environment from the user's
        // environment block when the app starts; there is no login shell.
        HostEnvironment::captured(std::env::vars().collect())
    }
}

/// The parent environment the login shell starts from: enough to find the
/// person's profile, nothing the app holds.
#[cfg(unix)]
fn scrubbed_parent(shell: &std::path::Path) -> Vec<(String, String)> {
    let mut env: Vec<(String, String)> = ["HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE"]
        .iter()
        .filter_map(|name| {
            std::env::var(name)
                .ok()
                .map(|value| ((*name).to_owned(), value))
        })
        .collect();
    env.push(("SHELL".to_owned(), shell.display().to_string()));
    env.push((
        "PATH".to_owned(),
        "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin".to_owned(),
    ));
    env.push(("TERM".to_owned(), "dumb".to_owned()));
    env
}

/// `shell -l -c '<print the marker>; env -0'`, killed after `timeout`.
#[cfg(unix)]
pub async fn capture_login_shell(shell: &std::path::Path, timeout: Duration) -> HostEnvironment {
    let script = r"printf '\000ELITEA_HOST_ENV\000'; exec env -0";
    let spawned = tokio::process::Command::new(shell)
        .args(["-l", "-c", script])
        .env_clear()
        .envs(scrubbed_parent(shell))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .process_group(0)
        .kill_on_drop(true)
        .spawn();
    let Ok(mut child) = spawned else {
        tracing::warn!(
            "the login shell could not be started; host commands get the fallback environment"
        );
        return HostEnvironment::failed();
    };
    let group = crate::sys::process::Group::of_tokio(&child);
    let Some(mut stdout) = child.stdout.take() else {
        return HostEnvironment::failed();
    };
    let read = async {
        let mut bytes = Vec::new();
        let mut chunk = [0u8; 8192];
        loop {
            let count = stdout.read(&mut chunk).await?;
            if count == 0 {
                break;
            }
            bytes.extend_from_slice(&chunk[..count]);
            if bytes.len() > MAX_CAPTURE_BYTES {
                break;
            }
        }
        let status = child.wait().await?;
        Ok::<_, std::io::Error>((status, bytes))
    };
    let outcome = tokio::time::timeout(timeout, read).await;
    // Whatever the profile started goes with the shell.
    if let Some(group) = &group {
        group.signal(crate::sys::process::Signal::KILL);
    }
    match outcome {
        Ok(Ok((status, bytes))) if status.success() => {
            if let Some(vars) = parse(&bytes) {
                HostEnvironment::captured(vars)
            } else {
                tracing::warn!(
                    "the login shell's environment could not be read; host commands get the fallback environment"
                );
                HostEnvironment::failed()
            }
        }
        Ok(_) => {
            tracing::warn!("the login shell failed; host commands get the fallback environment");
            HostEnvironment::failed()
        }
        Err(_) => {
            tracing::warn!(
                "the login shell did not print its environment in time; host commands get the fallback environment"
            );
            HostEnvironment::failed()
        }
    }
}

/// The `NAME=value` entries after [`MARKER`], NUL-separated.
#[cfg(any(unix, test))]
fn parse(bytes: &[u8]) -> Option<Vec<(String, String)>> {
    let start = bytes
        .windows(MARKER.len())
        .position(|window| window == MARKER)?
        + MARKER.len();
    let vars: Vec<(String, String)> = bytes[start..]
        .split(|byte| *byte == 0)
        .filter_map(|entry| {
            let text = std::str::from_utf8(entry).ok()?;
            let (name, value) = text.split_once('=')?;
            (!name.is_empty() && !name.contains(['\n', ' ']))
                .then(|| (name.to_owned(), value.to_owned()))
        })
        .collect();
    vars.iter().any(|(name, _)| name == "PATH").then_some(vars)
}

#[cfg(test)]
mod tests {
    use super::{HostEnvironment, parse};
    use crate::credentials::Secret;

    #[test]
    fn the_environment_follows_the_marker() {
        let mut bytes = b"Welcome!\nlast login\n".to_vec();
        bytes.extend_from_slice(super::MARKER);
        bytes.extend_from_slice(b"PATH=/opt/bin:/usr/bin\0KUBECONFIG=/k/c\0MULTI=a\nb\0");
        let vars = parse(&bytes).unwrap();
        assert_eq!(
            vars,
            vec![
                ("PATH".to_owned(), "/opt/bin:/usr/bin".to_owned()),
                ("KUBECONFIG".to_owned(), "/k/c".to_owned()),
                ("MULTI".to_owned(), "a\nb".to_owned()),
            ]
        );
        assert!(parse(b"PATH=/bin\0").is_none(), "no marker");
    }

    #[test]
    fn app_variables_grants_and_held_tokens_are_left_out() {
        let vars: Vec<(String, String)> = [
            ("PATH", "/usr/bin"),
            ("ELITEA_TEST_TOKEN", "x"),
            ("tauri_env", "y"),
            ("WEBKIT_DISABLE_COMPOSITING_MODE", "1"),
            ("GH_TOKEN", "from-the-grant"),
            ("HTTPS_PROXY", "http://elitea:abc@127.0.0.1:5555"),
            ("CORP_PROXY", "http://proxy.corp:3128"),
            ("SOME_VAR", "the-app-token"),
            ("AWS_PROFILE", "dev"),
        ]
        .iter()
        .map(|(n, v)| ((*n).to_owned(), (*v).to_owned()))
        .collect();
        let held = [Secret::held("the-app-token")];
        let kept: Vec<String> = HostEnvironment::for_command(&vars, &held)
            .into_iter()
            .map(|(name, _)| name)
            .collect();
        assert_eq!(kept, ["PATH", "CORP_PROXY", "AWS_PROFILE"]);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_login_shell_is_captured_and_a_slow_one_falls_back() {
        use std::os::unix::fs::PermissionsExt as _;
        let dir = tempfile::tempdir().unwrap();
        let shell = dir.path().join("fake-shell");
        // A "login shell" that runs the script it is given, with a profile
        // that exports a variable.
        std::fs::write(
            &shell,
            "#!/bin/sh\nexport KUBECONFIG=/home/me/.kube/config\nshift; shift; exec /bin/sh -c \"$1\"\n",
        )
        .unwrap();
        std::fs::set_permissions(&shell, std::fs::Permissions::from_mode(0o755)).unwrap();
        let env = super::capture_login_shell(&shell, super::CAPTURE_TIMEOUT).await;
        assert!(!env.fallback());
        assert!(
            env.vars()
                .iter()
                .any(|(name, value)| name == "KUBECONFIG" && value == "/home/me/.kube/config"),
            "{:?}",
            env.vars()
        );
        let slow = dir.path().join("slow-shell");
        std::fs::write(&slow, "#!/bin/sh\nsleep 30\n").unwrap();
        std::fs::set_permissions(&slow, std::fs::Permissions::from_mode(0o755)).unwrap();
        let started = std::time::Instant::now();
        let env = super::capture_login_shell(&slow, std::time::Duration::from_millis(300)).await;
        assert!(env.fallback() && env.vars().is_empty());
        assert!(started.elapsed() < std::time::Duration::from_secs(5));
        assert_eq!(super::CAPTURE_TIMEOUT, std::time::Duration::from_secs(5));
    }
}
