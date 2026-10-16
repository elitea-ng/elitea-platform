//! What a handoff never sends (ADR-0034 decision 5, amended 2026-10-15):
//! the platform's exclusion list (the server's `exclusions`, or the ADR's
//! default list when the server has none), the project's additions, the
//! person's own patterns for the folder, the folder's `.eliteaignore`, the
//! effective `local_work_path_deny` (a hard deny), and what the person chose
//! to leave on the laptop. The set applies to tracked files too: an excluded
//! tracked file is removed from the snapshot tree and listed in the
//! manifest, so take-back never deletes it.
//!
//! **Explicit include.** The person may include a file the exclusions match
//! (an exact path, or a pattern anchored at the folder's top), when the
//! policy allows it; `path_deny`, what is left on the laptop and what is not
//! a regular file can never be included. An included file is still scanned
//! (its findings warn, they do not block) and is listed as
//! `included_excluded`.

use std::path::Path;

use globset::{GlobBuilder, GlobSet, GlobSetBuilder};
use ignore::gitignore::{Gitignore, GitignoreBuilder};
use serde::{Deserialize, Serialize};

use crate::error::{ToolError, ToolResult};
use crate::workspace::{Workspace, WsPath, deny_globset};

/// The platform's default exclusions (the ADR's list), used when the server
/// sends none. Matched at any depth; a pattern ending in `/**` excludes a
/// directory of that name and everything in it; a leading `/` anchors a
/// pattern at the folder's top.
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
    "*.jks",
    "*.keystore",
    "credentials*.json",
];

/// The most patterns of one list, and the longest pattern.
pub const MAX_PATTERNS: usize = 500;
pub const MAX_PATTERN_LEN: usize = 512;

/// The exclusion patterns by source (contract §6.1: the server's
/// `exclusions` are the platform's then the project's; the person's stay on
/// the desktop).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Patterns {
    pub platform: Vec<String>,
    #[serde(default)]
    pub project: Vec<String>,
    #[serde(default)]
    pub user: Vec<String>,
}

impl Default for Patterns {
    fn default() -> Self {
        Self {
            platform: MANDATORY.iter().map(|p| (*p).to_owned()).collect(),
            project: Vec::new(),
            user: Vec::new(),
        }
    }
}

/// Everything that decides what one send leaves out.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Rules {
    pub patterns: Patterns,
    /// The effective `local_work_path_deny`: never sent, never included.
    pub path_deny: Vec<String>,
    /// This send only: workspace-relative paths.
    pub leave_on_laptop: Vec<String>,
    /// Explicit includes (exact paths, or patterns anchored at the top);
    /// empty when the policy does not allow including.
    pub include: Vec<String>,
}

/// The folder's own exclusion file (gitignore syntax, at its root).
pub const ELITEAIGNORE: &str = ".eliteaignore";

/// The largest `.eliteaignore` read.
const MAX_IGNORE_FILE: u64 = 256 * 1024;

/// Why a path stays on the laptop.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Reason {
    /// The platform's list (the ADR's defaults when the server sends none).
    #[serde(alias = "mandatory")]
    Platform,
    /// The project's additions.
    Project,
    /// The person's own patterns for this folder (desktop settings).
    User,
    /// The policy's `local_work_path_deny`: a hard deny.
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
    platform: GlobSet,
    project: GlobSet,
    user: GlobSet,
    deny: GlobSet,
    eliteaignore: Option<Gitignore>,
    leave: Vec<String>,
    include_exact: Vec<String>,
    include_globs: GlobSet,
}

impl std::fmt::Debug for Exclusions {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Exclusions")
            .field("leave", &self.leave)
            .finish_non_exhaustive()
    }
}

/// Whether `text` holds glob syntax (an include is a pattern then).
fn is_glob(text: &str) -> bool {
    text.contains(['*', '?', '[', '{'])
}

/// Check one exclusion or include pattern.
///
/// # Errors
///
/// Empty, too long, a control character, `..`, or not a glob.
pub fn check_pattern(pattern: &str) -> ToolResult<()> {
    let trimmed = pattern.trim();
    if trimmed.is_empty()
        || trimmed.len() > MAX_PATTERN_LEN
        || trimmed.chars().any(char::is_control)
        || trimmed.split('/').any(|part| part == "..")
    {
        return Err(ToolError::invalid(format!(
            "`{pattern}` is not a pattern a folder may use"
        )));
    }
    GlobBuilder::new(trimmed)
        .build()
        .map(|_| ())
        .map_err(|_| ToolError::invalid(format!("`{pattern}` is not a glob")))
}

/// Patterns matched at any depth (`/x` anchored at the top).
fn any_depth_set(patterns: &[String], what: &str) -> ToolResult<GlobSet> {
    let mut builder = GlobSetBuilder::new();
    for pattern in patterns.iter().take(MAX_PATTERNS) {
        let normalised = crate::workspace::nfc(pattern.trim());
        let normalised = normalised.trim_start_matches("./");
        if normalised.is_empty() {
            continue;
        }
        check_pattern(normalised)?;
        let glob = match normalised.strip_prefix('/') {
            Some(anchored) => anchored.to_owned(),
            None => format!("**/{normalised}"),
        };
        builder.add(
            GlobBuilder::new(&glob)
                .case_insensitive(crate::workspace::CASE_INSENSITIVE_FS)
                .literal_separator(true)
                .build()
                .map_err(|_| ToolError::invalid(format!("bad {what} pattern `{pattern}`")))?,
        );
    }
    builder
        .build()
        .map_err(|_| ToolError::invalid(format!("the {what} exclusions do not compile")))
}

impl Exclusions {
    /// The rules for `workspace`.
    ///
    /// # Errors
    ///
    /// A pattern that is not a glob, or a `leave_on_laptop` path or an
    /// exact include that escapes the folder.
    pub fn new(workspace: &Workspace, rules: &Rules) -> ToolResult<Self> {
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
        for path in &rules.leave_on_laptop {
            leave.push(WsPath::from_relative(Path::new(path))?.display_string());
        }
        let mut include_exact = Vec::new();
        let mut include_globs = GlobSetBuilder::new();
        for entry in rules.include.iter().take(MAX_PATTERNS) {
            let entry = entry.trim().trim_start_matches("./");
            if entry.is_empty() {
                continue;
            }
            check_pattern(entry)?;
            if is_glob(entry) {
                include_globs.add(
                    GlobBuilder::new(entry.trim_start_matches('/'))
                        .case_insensitive(crate::workspace::CASE_INSENSITIVE_FS)
                        .literal_separator(true)
                        .build()
                        .map_err(|_| ToolError::invalid(format!("`{entry}` is not a glob")))?,
                );
            } else {
                include_exact.push(crate::workspace::nfc(
                    &WsPath::from_relative(Path::new(entry.trim_start_matches('/')))?
                        .display_string(),
                ));
            }
        }
        Ok(Self {
            platform: any_depth_set(&rules.patterns.platform, "platform")?,
            project: any_depth_set(&rules.patterns.project, "project")?,
            user: any_depth_set(&rules.patterns.user, "folder")?,
            deny: deny_globset(&rules.path_deny)?,
            eliteaignore,
            leave,
            include_exact,
            include_globs: include_globs
                .build()
                .map_err(|_| ToolError::invalid("the includes do not compile"))?,
        })
    }

    /// Why `path` (a `/`-joined, top-relative file path) is excluded, if it
    /// is. A directory rule covers everything under it, so every prefix is
    /// tested.
    #[must_use]
    pub fn reason(&self, path: &str) -> Option<Reason> {
        let reason = self.matched(path)?;
        if self.included(path, reason) {
            None
        } else {
            Some(reason)
        }
    }

    /// Whether `path`, which the exclusions match for `reason`, is
    /// explicitly included. Never for `path_deny`, what is left on the
    /// laptop, or a path with a control character (it could not be named
    /// in the manifest's trailer).
    fn included(&self, path: &str, reason: Reason) -> bool {
        if matches!(
            reason,
            Reason::PathDeny | Reason::LeaveOnLaptop | Reason::NotAFile
        ) || path.chars().any(char::is_control)
        {
            return false;
        }
        let normalised = crate::workspace::nfc(path);
        let fold = |text: &str| {
            if crate::workspace::CASE_INSENSITIVE_FS {
                text.to_lowercase()
            } else {
                text.to_owned()
            }
        };
        self.include_exact
            .iter()
            .any(|exact| fold(exact) == fold(&normalised))
            || self.include_globs.is_match(&normalised)
    }

    /// The exclusion matched by `path` that an include overrode, if any:
    /// `path` is sent as an `included_excluded` file.
    #[must_use]
    pub fn included_reason(&self, path: &str) -> Option<Reason> {
        let reason = self.matched(path)?;
        self.included(path, reason).then_some(reason)
    }

    /// Why the exclusions match `path`, ignoring the includes.
    #[must_use]
    pub fn matched(&self, path: &str) -> Option<Reason> {
        let normalised = crate::workspace::nfc(path);
        let components: Vec<&str> = normalised.split('/').collect();
        for end in 1..=components.len() {
            let prefix = components[..end].join("/");
            if self.deny.is_match(&prefix) {
                return Some(Reason::PathDeny);
            }
        }
        // After `path_deny`: a path in both lists is a hard deny.
        if self
            .leave
            .iter()
            .any(|leave| path == leave || path.starts_with(&format!("{leave}/")))
        {
            return Some(Reason::LeaveOnLaptop);
        }
        for end in 1..=components.len() {
            let prefix = components[..end].join("/");
            if self.platform.is_match(&prefix) {
                return Some(Reason::Platform);
            }
            if self.project.is_match(&prefix) {
                return Some(Reason::Project);
            }
            if self.user.is_match(&prefix) {
                return Some(Reason::User);
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
    use super::{Exclusions, Patterns, Reason, Rules};
    use crate::workspace::Workspace;

    fn strings(items: &[&str]) -> Vec<String> {
        items.iter().map(|s| (*s).to_owned()).collect()
    }

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
        let rules = Exclusions::new(
            &workspace,
            &Rules {
                path_deny: strings(deny),
                leave_on_laptop: strings(leave),
                ..Rules::default()
            },
        )
        .expect("rules");
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
            (".env", Some(Reason::Platform)),
            ("app/.env", Some(Reason::Platform)),
            (".env.local", Some(Reason::Platform)),
            ("config/.env.production", Some(Reason::Platform)),
            ("server.pem", Some(Reason::Platform)),
            ("deep/a/b/tls.key", Some(Reason::Platform)),
            ("id_rsa", Some(Reason::Platform)),
            ("keys/id_rsa.pub", Some(Reason::Platform)),
            ("id_ed25519", Some(Reason::Platform)),
            (".npmrc", Some(Reason::Platform)),
            ("pkg/.pypirc", Some(Reason::Platform)),
            (".netrc", Some(Reason::Platform)),
            (".aws/credentials", Some(Reason::Platform)),
            ("home/.aws/config", Some(Reason::Platform)),
            (".ssh/known_hosts", Some(Reason::Platform)),
            ("cert.p12", Some(Reason::Platform)),
            (
                "cert.PFX",
                if cfg!(target_os = "macos") {
                    Some(Reason::Platform)
                } else {
                    None
                },
            ),
            ("vault.kdbx", Some(Reason::Platform)),
            ("credentials.json", Some(Reason::Platform)),
            ("gcp/credentials-prod.json", Some(Reason::Platform)),
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
        let leave = |path: &str| Rules {
            leave_on_laptop: vec![path.to_owned()],
            ..Rules::default()
        };
        assert!(Exclusions::new(&workspace, &leave("../x")).is_err());
    }

    #[test]
    fn path_deny_wins_over_leave_on_laptop() {
        let (_dir, rules) = rules(&["deploy/**"], &["deploy", "notes"], None);
        assert_eq!(rules.reason("deploy/x.tf"), Some(Reason::PathDeny));
        assert_eq!(rules.reason("notes/a.md"), Some(Reason::LeaveOnLaptop));
    }

    #[test]
    fn keystores_are_in_the_default_list() {
        let (_dir, rules) = rules(&[], &[], None);
        for path in ["app.jks", "android/release.keystore"] {
            assert_eq!(rules.reason(path), Some(Reason::Platform), "{path}");
        }
    }

    #[test]
    fn sources_includes_and_the_hard_deny() {
        let dir = tempfile::tempdir().expect("dir");
        let workspace = Workspace::open(dir.path(), &[]).expect("open");
        let rules = Exclusions::new(
            &workspace,
            &Rules {
                patterns: Patterns {
                    platform: strings(&[".env", "*.pem"]),
                    project: strings(&["*.sqlite", "/fixtures/**"]),
                    user: strings(&["notes/**"]),
                },
                path_deny: strings(&["secrets/**"]),
                leave_on_laptop: strings(&["left.txt"]),
                include: strings(&[".env", "certs/*.pem", "secrets/a.txt", "left.txt"]),
            },
        )
        .expect("rules");
        // Sources.
        assert_eq!(rules.reason("app/.env"), Some(Reason::Platform));
        assert_eq!(rules.reason("db/x.sqlite"), Some(Reason::Project));
        assert_eq!(rules.reason("fixtures/a.json"), Some(Reason::Project));
        assert_eq!(rules.reason("src/fixtures/a.json"), None, "anchored");
        assert_eq!(rules.reason("notes/todo.md"), Some(Reason::User));
        assert_eq!(rules.reason("secrets/b.txt"), Some(Reason::PathDeny));
        // Includes: exact paths and anchored patterns, never path_deny or
        // what is left on the laptop.
        assert_eq!(rules.reason(".env"), None);
        assert_eq!(rules.included_reason(".env"), Some(Reason::Platform));
        assert_eq!(rules.reason("app/.env"), Some(Reason::Platform), "exact");
        assert_eq!(rules.reason("certs/tls.pem"), None);
        assert_eq!(rules.reason("other/tls.pem"), Some(Reason::Platform));
        assert_eq!(rules.reason("secrets/a.txt"), Some(Reason::PathDeny));
        assert_eq!(rules.reason("left.txt"), Some(Reason::LeaveOnLaptop));
        assert_eq!(rules.included_reason("src/main.rs"), None);
        // A bad pattern is refused.
        assert!(
            Exclusions::new(
                &workspace,
                &Rules {
                    patterns: Patterns {
                        user: strings(&["a/../b"]),
                        ..Patterns::default()
                    },
                    ..Rules::default()
                }
            )
            .is_err()
        );
    }
}
