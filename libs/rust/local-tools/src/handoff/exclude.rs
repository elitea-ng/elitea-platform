//! What a handoff never sends (ADR-0034 decision 5): the mandatory set,
//! the effective `local_work_path_deny`, the folder's own `.eliteaignore`,
//! and what the person chose to leave on the laptop. The set applies to
//! tracked files too: an excluded tracked file is removed from the snapshot
//! tree and listed in the manifest, so take-back never deletes it.

use std::path::Path;

use globset::{GlobBuilder, GlobSet, GlobSetBuilder};
use ignore::gitignore::{Gitignore, GitignoreBuilder};
use serde::{Deserialize, Serialize};

use crate::error::{ToolError, ToolResult};
use crate::workspace::{Workspace, WsPath, deny_globset};

/// The mandatory exclusions, matched at any depth; a pattern ending in
/// `/**` excludes a directory of that name and everything in it.
pub const MANDATORY: &[&str] = &[
    ".env",
    ".env.*",
    "*.pem",
    "*.key",
    "id_rsa*",
    "id_ed25519*",
    ".npmrc",
    ".pypirc",
    ".netrc",
    ".aws/**",
    ".ssh/**",
    "*.p12",
    "*.pfx",
    "*.kdbx",
    "credentials*.json",
];

/// The folder's own exclusion file (gitignore syntax, at its root).
pub const ELITEAIGNORE: &str = ".eliteaignore";

/// The largest `.eliteaignore` read.
const MAX_IGNORE_FILE: u64 = 256 * 1024;

/// Why a path stays on the laptop.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Reason {
    /// The mandatory set.
    Mandatory,
    /// The policy's `local_work_path_deny`.
    PathDeny,
    /// The folder's `.eliteaignore`.
    Eliteaignore,
    /// The person chose it in the send dialog (a secret finding, say).
    LeaveOnLaptop,
    /// A nested git repository, a socket, a FIFO or a device: not
    /// something a bundle carries.
    NotAFile,
}

/// One excluded path.
#[derive(Clone, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
pub struct Excluded {
    pub path: String,
    pub reason: Reason,
    /// It is in `HEAD`: take-back leaves the local copy alone.
    pub tracked: bool,
}

/// The exclusion rules of one send.
pub struct Exclusions {
    mandatory: GlobSet,
    deny: GlobSet,
    eliteaignore: Option<Gitignore>,
    leave: Vec<String>,
}

impl std::fmt::Debug for Exclusions {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Exclusions")
            .field("leave", &self.leave)
            .finish_non_exhaustive()
    }
}

fn mandatory_set() -> ToolResult<GlobSet> {
    let mut builder = GlobSetBuilder::new();
    for pattern in MANDATORY {
        let glob = format!("**/{pattern}");
        builder.add(
            GlobBuilder::new(&glob)
                .case_insensitive(crate::workspace::CASE_INSENSITIVE_FS)
                .literal_separator(true)
                .build()
                .map_err(|_| ToolError::invalid(format!("bad mandatory pattern {pattern}")))?,
        );
    }
    builder
        .build()
        .map_err(|_| ToolError::invalid("the mandatory exclusions do not compile"))
}

impl Exclusions {
    /// The rules for `workspace` under `path_deny`, with `leave_on_laptop`
    /// (workspace-relative paths).
    ///
    /// # Errors
    ///
    /// A `path_deny` pattern that is not a glob, or a `leave_on_laptop`
    /// path that escapes the folder.
    pub fn new(
        workspace: &Workspace,
        path_deny: &[String],
        leave_on_laptop: &[String],
    ) -> ToolResult<Self> {
        let eliteaignore = WsPath::from_relative(Path::new(ELITEAIGNORE))
            .ok()
            .and_then(|path| workspace.read(&path, MAX_IGNORE_FILE).ok())
            .and_then(|file| {
                let mut builder = GitignoreBuilder::new(workspace.root());
                for line in String::from_utf8_lossy(&file.bytes).lines() {
                    let _ = builder.add_line(None, line);
                }
                builder.build().ok()
            });
        let mut leave = Vec::new();
        for path in leave_on_laptop {
            leave.push(WsPath::from_relative(Path::new(path))?.display_string());
        }
        Ok(Self {
            mandatory: mandatory_set()?,
            deny: deny_globset(path_deny)?,
            eliteaignore,
            leave,
        })
    }

    /// Why `path` (a `/`-joined, top-relative file path) is excluded, if it
    /// is. A directory rule covers everything under it, so every prefix is
    /// tested.
    #[must_use]
    pub fn reason(&self, path: &str) -> Option<Reason> {
        if self
            .leave
            .iter()
            .any(|leave| path == leave || path.starts_with(&format!("{leave}/")))
        {
            return Some(Reason::LeaveOnLaptop);
        }
        let normalised = crate::workspace::nfc(path);
        let components: Vec<&str> = normalised.split('/').collect();
        for end in 1..=components.len() {
            let prefix = components[..end].join("/");
            if self.mandatory.is_match(&prefix) {
                return Some(Reason::Mandatory);
            }
            if self.deny.is_match(&prefix) {
                return Some(Reason::PathDeny);
            }
        }
        if let Some(ignore) = &self.eliteaignore
            && ignore
                .matched_path_or_any_parents(Path::new(path), false)
                .is_ignore()
        {
            return Some(Reason::Eliteaignore);
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::{Exclusions, Reason};
    use crate::workspace::Workspace;

    fn rules(
        deny: &[&str],
        leave: &[&str],
        eliteaignore: Option<&str>,
    ) -> (tempfile::TempDir, Exclusions) {
        let dir = tempfile::tempdir().expect("dir");
        if let Some(text) = eliteaignore {
            std::fs::write(dir.path().join(".eliteaignore"), text).expect("write");
        }
        let workspace = Workspace::open(dir.path(), &[]).expect("open");
        let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
        let leave: Vec<String> = leave.iter().map(|s| (*s).to_owned()).collect();
        let rules = Exclusions::new(&workspace, &deny, &leave).expect("rules");
        (dir, rules)
    }

    #[test]
    fn the_exclusion_matrix() {
        let (_dir, rules) = rules(
            &["secrets/**", "*.sqlite"],
            &["notes/todo.txt"],
            Some("build-cache/\n*.tmp\n!keep.tmp\n"),
        );
        let cases: &[(&str, Option<Reason>)] = &[
            (".env", Some(Reason::Mandatory)),
            ("app/.env", Some(Reason::Mandatory)),
            (".env.local", Some(Reason::Mandatory)),
            ("config/.env.production", Some(Reason::Mandatory)),
            ("server.pem", Some(Reason::Mandatory)),
            ("deep/a/b/tls.key", Some(Reason::Mandatory)),
            ("id_rsa", Some(Reason::Mandatory)),
            ("keys/id_rsa.pub", Some(Reason::Mandatory)),
            ("id_ed25519", Some(Reason::Mandatory)),
            (".npmrc", Some(Reason::Mandatory)),
            ("pkg/.pypirc", Some(Reason::Mandatory)),
            (".netrc", Some(Reason::Mandatory)),
            (".aws/credentials", Some(Reason::Mandatory)),
            ("home/.aws/config", Some(Reason::Mandatory)),
            (".ssh/known_hosts", Some(Reason::Mandatory)),
            ("cert.p12", Some(Reason::Mandatory)),
            (
                "cert.PFX",
                if cfg!(target_os = "macos") {
                    Some(Reason::Mandatory)
                } else {
                    None
                },
            ),
            ("vault.kdbx", Some(Reason::Mandatory)),
            ("credentials.json", Some(Reason::Mandatory)),
            ("gcp/credentials-prod.json", Some(Reason::Mandatory)),
            ("secrets/db.txt", Some(Reason::PathDeny)),
            ("data/app.sqlite", Some(Reason::PathDeny)),
            ("notes/todo.txt", Some(Reason::LeaveOnLaptop)),
            ("build-cache/x.o", Some(Reason::Eliteaignore)),
            ("a/b.tmp", Some(Reason::Eliteaignore)),
            // Not excluded.
            ("keep.tmp", None),
            ("src/env.rs", None),
            ("environment.md", None),
            (".envrc.md", None),
            ("keyboard.rs", None),
            ("monkey.keys", None),
            ("notes/todo.txt.bak", None),
            ("credentials.yaml", None),
            ("aws/config", None),
            ("src/main.rs", None),
        ];
        for (path, expected) in cases {
            assert_eq!(rules.reason(path), *expected, "{path}");
        }
    }

    #[test]
    fn a_leave_on_laptop_path_cannot_escape() {
        let dir = tempfile::tempdir().expect("dir");
        let workspace = Workspace::open(dir.path(), &[]).expect("open");
        assert!(Exclusions::new(&workspace, &[], &["../x".to_owned()]).is_err());
    }
}
