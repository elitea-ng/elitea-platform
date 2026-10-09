//! git, through the git CLI: the read-only tools (status, diff, log,
//! branches) and the plumbing the checkpoints use.
//!
//! Why the CLI and not gix (which `repo-ingest` locks): status and diff need
//! gix's `status`/`blob-diff` features, which the workspace does not build,
//! and checkpoints need `add -A` semantics (`.gitignore`, `.git/info/exclude`,
//! `core.excludesFile`, filters) exactly as the person's git applies them.
//! The desktop runs where the person already has git; the cost is a process
//! per call.
//!
//! Every invocation is hardened against the repository it runs in: no
//! hooks, no fsmonitor, no external diff or textconv drivers, no pager, no
//! optional locks, literal pathspecs, and the parent's `GIT_*` variables
//! removed so the host's environment cannot redirect it.

use std::ffi::OsStr;
use std::io::Write as _;
use std::path::Path;
use std::process::{Command, Stdio};

use crate::error::{ErrorCode, ToolError, ToolResult};

/// Bytes of git output a tool returns.
pub const OUTPUT_CAP: usize = 256 * 1024;

const HARDENING: &[&str] = &[
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
    "--no-optional-locks",
    "--literal-pathspecs",
];

/// One git invocation.
pub(crate) struct Git<'a> {
    cwd: &'a Path,
    env: Vec<(&'a str, &'a OsStr)>,
    stdin: Option<&'a [u8]>,
}

impl<'a> Git<'a> {
    pub(crate) fn new(cwd: &'a Path) -> Self {
        Self {
            cwd,
            env: Vec::new(),
            stdin: None,
        }
    }

    pub(crate) fn env(mut self, name: &'a str, value: &'a OsStr) -> Self {
        self.env.push((name, value));
        self
    }

    pub(crate) fn stdin(mut self, bytes: &'a [u8]) -> Self {
        self.stdin = Some(bytes);
        self
    }

    /// Run `git <args>`; the raw output on success.
    pub(crate) fn run(&self, args: &[&str]) -> ToolResult<Vec<u8>> {
        let mut command = Command::new("git");
        command.current_dir(self.cwd).args(HARDENING).args(args);
        for (name, _) in std::env::vars_os() {
            if name.to_string_lossy().starts_with("GIT_") {
                command.env_remove(name);
            }
        }
        command
            .env("LC_ALL", "C")
            .env("GIT_TERMINAL_PROMPT", "0")
            .env("GIT_CONFIG_NOSYSTEM", "1");
        for (name, value) in &self.env {
            command.env(name, value);
        }
        command
            .stdin(if self.stdin.is_some() {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let mut child = command.spawn().map_err(|error| {
            ToolError::new(
                ErrorCode::Unsupported,
                format!("git is not available: {}", error.kind()),
            )
        })?;
        if let (Some(bytes), Some(mut stdin)) = (self.stdin, child.stdin.take()) {
            stdin
                .write_all(bytes)
                .map_err(|error| ToolError::io("cannot talk to git", &error))?;
        }
        let output = child
            .wait_with_output()
            .map_err(|error| ToolError::io("git failed", &error))?;
        if output.status.success() {
            Ok(output.stdout)
        } else {
            let stderr = String::from_utf8_lossy(&output.stderr);
            let first = stderr.lines().next().unwrap_or("git failed").trim();
            Err(ToolError::new(
                ErrorCode::Git,
                format!("git {}: {first}", args.first().copied().unwrap_or_default()),
            ))
        }
    }

    /// Run and return trimmed UTF-8 text.
    pub(crate) fn text(&self, args: &[&str]) -> ToolResult<String> {
        Ok(String::from_utf8_lossy(&self.run(args)?)
            .trim_end()
            .to_owned())
    }
}

/// The repository top level containing `dir`, if `dir` is in a work tree.
pub(crate) fn toplevel(dir: &Path) -> Option<std::path::PathBuf> {
    let top = Git::new(dir).text(&["rev-parse", "--show-toplevel"]).ok()?;
    std::fs::canonicalize(top).ok()
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
    use super::{capped, check_revision};

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
