//! The desktop side of the desktop ⇄ cloud handoff (ADR-0034).
//!
//! * [`snapshot`]: the transfer set of a send, built in a temporary index
//!   as checkpoints are (the person's index, `HEAD`, stash and working tree
//!   are never touched); history mode `none`: an orphan base `B`
//!   (`Elitea-Base`), an orphan upstream `U` (`Elitea-Upstream`) when `HEAD`
//!   has one, the snapshot `H_1` on `B`, and its local twin `H'_1` on the
//!   real `HEAD`, kept on a private ref for take-back; a shadow git
//!   directory ([`shadow`]) for a folder that is not a repository.
//! * [`exclude`]: what never leaves the laptop; [`scan`]: the secret scan;
//!   [`caps`]: the size caps.
//! * [`apply`]: take-back: preflight with no side effects (optionally up
//!   to turn `k`), then the fast path or a clean merge, conflict markers, a
//!   worktree, or discard, through the shared apply engine
//!   ([`crate::apply`]).
//!
//! Every git process goes through [`crate::git`]'s hardening (unsafe
//! repositories refused, overrides, sandboxed, timeouts). Objects are read
//! and bundles written through `elitea-workspace-store`, which reads object
//! files only and runs nothing.

pub mod apply;
pub mod caps;
pub mod exclude;
pub mod scan;
pub mod shadow;
pub mod snapshot;

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use serde::{Deserialize, Serialize};

use crate::error::{ErrorCode, ToolError, ToolResult};

/// The private ref namespace of a thread's handoff in the person's own
/// repository (or shadow): `<prefix>/<conversation>/{sent,twin}`.
pub const LOCAL_REF_PREFIX: &str = "refs/elitea/handoff-local";

/// What the desktop keeps about a send, for take-back: written by the host
/// in its app data (never in the folder).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct SendRecord {
    pub conversation_uuid: String,
    /// The folder is not a repository: its shadow is used.
    pub shadow: bool,
    /// `HEAD` when the snapshot was taken (`None`: unborn or shadow).
    pub head: Option<String>,
    pub base: String,
    pub upstream: Option<String>,
    /// `H_1`, as sent.
    pub handoff: String,
    /// `H'_1`: the same tree on the real `HEAD`.
    pub twin: String,
    /// The snapshot tree.
    pub tree: String,
    pub excluded: Vec<exclude::Excluded>,
    /// Paths sent although the exclusions match them (explicit includes).
    #[serde(default)]
    pub included: Vec<String>,
    /// The exclusion patterns of the send (`None`: a record from before
    /// they were configurable; the defaults apply).
    #[serde(default)]
    pub patterns: Option<exclude::Patterns>,
}

static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// A private temporary directory, removed on drop (a temporary index, a
/// quarantine).
#[derive(Debug)]
pub struct TempDir {
    path: PathBuf,
}

impl TempDir {
    /// # Errors
    ///
    /// It cannot be created.
    pub fn new(label: &str) -> ToolResult<Self> {
        let path = std::env::temp_dir().join(format!(
            "elitea-handoff-{label}-{}-{}",
            std::process::id(),
            TEMP_COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir(&path)
            .map_err(|error| ToolError::io("cannot create a temporary directory", &error))?;
        {
            use std::os::unix::fs::PermissionsExt as _;
            let _ = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700));
        }
        let path = std::fs::canonicalize(&path)
            .map_err(|error| ToolError::io("cannot create a temporary directory", &error))?;
        Ok(Self { path })
    }

    #[must_use]
    pub fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

/// A conversation UUID, checked before it goes into a ref name or a path.
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] for anything but a canonical UUID.
pub fn check_conversation(uuid: &str) -> ToolResult<()> {
    let ok = uuid.len() == 36
        && uuid.char_indices().all(|(i, c)| match i {
            8 | 13 | 18 | 23 => c == '-',
            _ => c.is_ascii_hexdigit() && !c.is_ascii_uppercase(),
        });
    if ok {
        Ok(())
    } else {
        Err(ToolError::invalid("a conversation id is a lowercase UUID"))
    }
}

/// The first eight characters of the conversation: the cloud branch name
/// `elitea/cloud/<short>`.
#[must_use]
pub fn short_thread(uuid: &str) -> &str {
    &uuid[..uuid.len().min(8)]
}

pub(crate) fn store_error(error: &elitea_workspace_store::StoreError) -> ToolError {
    let code = match error {
        elitea_workspace_store::StoreError::Integrity(_) => ErrorCode::Conflict,
        elitea_workspace_store::StoreError::UnsupportedObjectFormat(_)
        | elitea_workspace_store::StoreError::Unsupported(_) => ErrorCode::Unsupported,
        elitea_workspace_store::StoreError::Invalid(_) => ErrorCode::TooLarge,
        _ => ErrorCode::Io,
    };
    ToolError::new(code, error.to_string())
}

/// A remote URL as it may leave the laptop: credentials in it removed.
#[must_use]
pub fn redact_url(url: &str) -> String {
    let url = url.trim();
    if let Some((scheme, rest)) = url.split_once("://") {
        let (authority, path) = rest.split_once('/').map_or((rest, ""), |(a, p)| (a, p));
        let host = authority
            .rsplit_once('@')
            .map_or(authority, |(_, host)| host);
        let mut out = format!("{scheme}://{host}");
        if !path.is_empty() || rest.contains('/') {
            out.push('/');
            out.push_str(path.split(['?', '#']).next().unwrap_or_default());
        }
        return out;
    }
    // scp-like `user@host:path` keeps its user (not a secret), but never a
    // `user:password@`.
    match url.split_once('@') {
        Some((user, rest)) if user.contains(':') => rest.to_owned(),
        _ => url.to_owned(),
    }
    .chars()
    .filter(|c| !c.is_control() && !c.is_whitespace())
    .collect()
}

#[cfg(test)]
mod tests {
    use super::{check_conversation, redact_url};

    #[test]
    fn remote_urls_lose_their_credentials() {
        assert_eq!(
            redact_url("https://user:ghp_x@github.com/o/r.git"),
            "https://github.com/o/r.git"
        );
        assert_eq!(
            redact_url("https://github.com/o/r.git?token=x"),
            "https://github.com/o/r.git"
        );
        assert_eq!(
            redact_url("git@github.com:o/r.git"),
            "git@github.com:o/r.git"
        );
        assert_eq!(redact_url("u:p@host:o/r"), "host:o/r");
        assert_eq!(redact_url("ssh://git@host:22/o/r"), "ssh://host:22/o/r");
    }

    #[test]
    fn conversation_ids_are_canonical_uuids() {
        assert!(check_conversation("c0ffee00-0000-4000-8000-000000000001").is_ok());
        for bad in [
            "C0FFEE00-0000-4000-8000-000000000001",
            "../x",
            "c0ffee00",
            "c0ffee00-0000-4000-8000-00000000000g",
        ] {
            assert!(check_conversation(bad).is_err(), "{bad}");
        }
    }
}
