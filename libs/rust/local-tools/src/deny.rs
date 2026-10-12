//! One session's deny list: the files and directories no local tool reads
//! or writes, whatever the workspace's `path_deny` says.
//!
//! It holds, as the host gave them ([`DenyList::for_session`]):
//!
//! * the person's credentials under the home directory
//!   ([`crate::sandbox::credential_paths`]);
//! * the host app's own directories under its identifier, in the default
//!   layouts ([`app_paths`]);
//! * the directories the host resolved for itself (its config, data, log
//!   and cache directories: the authority for those) and the session's
//!   data directory.
//!
//! Every consumer reads this one list: the OS sandbox of each command
//! ([`DenyList::paths`]), the host's git (the same), and the in-process
//! file tools and walkers ([`DenyList::covers`], through
//! [`crate::workspace::Workspace::with_deny_list`]).
//!
//! Each entry is kept under two spellings: as given (so a directory created
//! after the session opened is still covered) and resolved (the sandboxes
//! and the walkers see resolved paths: a symlinked `~/.config` must not
//! slip through). The resolved spelling canonicalises the longest existing
//! ancestor and keeps the rest as written; it is computed once, on first
//! use. An entry whose last component ends in `*` is a name prefix.

use std::path::{Path, PathBuf};
use std::sync::OnceLock;

use crate::sandbox::credential_paths;
use crate::workspace::CASE_INSENSITIVE_FS;

/// The host app's directories under its identifier `app_id` in the default
/// layouts: macOS (`~/Library`: Application Support, Caches, Logs, the
/// webview's `WebKit` and `HTTPStorages`, saved window state, preferences) and
/// Linux XDG defaults (`~/.config`, `~/.local/share`, `~/.cache`). Both
/// layouts, whatever the OS: they are fallbacks for directories the host did
/// not resolve, never the authority. Empty for an identifier that is not a
/// plain name (empty, `.`, `..`, or holding `/`, `\` or NUL).
#[must_use]
pub fn app_paths(home: &Path, app_id: &str) -> Vec<PathBuf> {
    if app_id.is_empty() || app_id == "." || app_id == ".." || app_id.contains(['/', '\\', '\0']) {
        return Vec::new();
    }
    [
        format!(".config/{app_id}"),
        format!(".local/share/{app_id}"),
        format!(".cache/{app_id}"),
        format!("Library/Application Support/{app_id}"),
        format!("Library/Caches/{app_id}"),
        format!("Library/Logs/{app_id}"),
        format!("Library/WebKit/{app_id}"),
        format!("Library/HTTPStorages/{app_id}"),
        format!("Library/HTTPStorages/{app_id}.binarycookies"),
        format!("Library/Saved Application State/{app_id}.savedState"),
        format!("Library/Preferences/{app_id}.plist"),
    ]
    .iter()
    .map(|relative| home.join(relative))
    .collect()
}

/// The resolved spelling of `path`: its longest existing ancestor
/// canonicalised, the rest kept as written; for a `prefix*` entry, that of
/// its parent with the prefix kept. `None` when nothing resolves or it is
/// spelled the same.
#[must_use]
pub fn resolved_spelling(path: &Path) -> Option<PathBuf> {
    let resolved = if path.to_string_lossy().ends_with('*') {
        let name = path.file_name()?;
        resolve_existing(path.parent()?)?.join(name)
    } else {
        resolve_existing(path)?
    };
    (resolved != path).then_some(resolved)
}

fn resolve_existing(path: &Path) -> Option<PathBuf> {
    let mut rest = Vec::new();
    let mut current = path;
    loop {
        if let Ok(canonical) = std::fs::canonicalize(current) {
            let mut out = canonical;
            for name in rest.iter().rev() {
                out.push(name);
            }
            return Some(out);
        }
        rest.push(current.file_name()?.to_owned());
        current = current.parent()?;
    }
}

/// One spelling, ready to match.
#[derive(Clone, Debug)]
struct Spelling {
    /// The text compared (case-folded where the file system folds case).
    text: String,
    /// A name prefix (`…/elitea*`, the `*` dropped).
    prefix: bool,
}

/// One session's deny list (see the module documentation).
#[derive(Debug, Default)]
pub struct DenyList {
    literals: Vec<PathBuf>,
    resolved: OnceLock<(Vec<PathBuf>, Vec<Spelling>)>,
}

impl DenyList {
    /// A list of exactly `entries` (duplicates dropped).
    #[must_use]
    pub fn new(entries: impl IntoIterator<Item = PathBuf>) -> Self {
        let mut literals: Vec<PathBuf> = Vec::new();
        for entry in entries {
            if !entry.as_os_str().is_empty() && !literals.contains(&entry) {
                literals.push(entry);
            }
        }
        Self {
            literals,
            resolved: OnceLock::new(),
        }
    }

    /// A session's list: `extra` (the host's own directories, the session's
    /// data directory) and, with `credentials`, the credentials and the
    /// app's identifier directories under `home`.
    #[must_use]
    pub fn for_session(
        home: Option<&Path>,
        app_id: Option<&str>,
        credentials: bool,
        extra: impl IntoIterator<Item = PathBuf>,
    ) -> Self {
        let mut entries: Vec<PathBuf> = extra.into_iter().collect();
        if credentials && let Some(home) = home {
            entries.extend(credential_paths(home));
            if let Some(app_id) = app_id {
                entries.extend(app_paths(home, app_id));
            }
        }
        Self::new(entries)
    }

    /// The person's credentials under a non-empty `HOME`: what a host
    /// that gave no list gets.
    #[must_use]
    pub fn env_home_credentials() -> Self {
        let home = std::env::var_os("HOME")
            .filter(|home| !home.is_empty())
            .map(PathBuf::from);
        Self::for_session(home.as_deref(), None, true, Vec::new())
    }

    /// The entries as given.
    #[must_use]
    pub fn literals(&self) -> &[PathBuf] {
        &self.literals
    }

    fn resolved(&self) -> &(Vec<PathBuf>, Vec<Spelling>) {
        self.resolved.get_or_init(|| {
            let mut paths = Vec::with_capacity(self.literals.len() * 2);
            for literal in &self.literals {
                if !paths.contains(literal) {
                    paths.push(literal.clone());
                }
                if let Some(resolved) = resolved_spelling(literal)
                    && !paths.contains(&resolved)
                {
                    paths.push(resolved);
                }
            }
            let spellings = paths
                .iter()
                .map(|path| {
                    let text = path.to_string_lossy();
                    let (text, prefix) = match text.strip_suffix('*') {
                        Some(stem) => (stem.to_owned(), true),
                        None => (text.trim_end_matches('/').to_owned(), false),
                    };
                    Spelling {
                        text: fold(&text),
                        prefix,
                    }
                })
                .collect();
            (paths, spellings)
        })
    }

    /// Every spelling of every entry: what a sandbox denies.
    #[must_use]
    pub fn paths(&self) -> &[PathBuf] {
        &self.resolved().0
    }

    /// Whether the absolute `path` is an entry or under one (a name prefix
    /// entry: whose name starts with it), under either spelling, without
    /// regard to case where the file system folds case. No file system
    /// call once the spellings are resolved.
    #[must_use]
    pub fn covers(&self, path: &Path) -> bool {
        let text = fold(&path.to_string_lossy());
        self.resolved().1.iter().any(|spelling| {
            if spelling.prefix {
                text.starts_with(&spelling.text)
            } else {
                text.strip_prefix(&spelling.text)
                    .is_some_and(|rest| rest.is_empty() || rest.starts_with('/'))
            }
        })
    }
}

fn fold(text: &str) -> String {
    if CASE_INSENSITIVE_FS {
        text.to_lowercase()
    } else {
        text.to_owned()
    }
}

#[cfg(test)]
mod tests {
    use std::path::{Path, PathBuf};

    use super::{DenyList, app_paths};

    #[test]
    fn app_paths_cover_both_layouts_and_refuse_odd_identifiers() {
        let home = Path::new("/h");
        let paths = app_paths(home, "com.example.app");
        for relative in [
            ".config/com.example.app",
            ".local/share/com.example.app",
            ".cache/com.example.app",
            "Library/Application Support/com.example.app",
            "Library/Caches/com.example.app",
            "Library/Logs/com.example.app",
            "Library/WebKit/com.example.app",
            "Library/HTTPStorages/com.example.app",
            "Library/HTTPStorages/com.example.app.binarycookies",
            "Library/Saved Application State/com.example.app.savedState",
            "Library/Preferences/com.example.app.plist",
        ] {
            assert!(paths.contains(&home.join(relative)), "{relative}");
        }
        for odd in ["", ".", "..", "a/b", "a\\b"] {
            assert!(app_paths(home, odd).is_empty(), "{odd:?}");
        }
    }

    /// Both spellings through a symlinked intermediate directory, for
    /// existing entries, missing ones and prefixes alike; a missing entry
    /// under a symlink is covered once created.
    #[test]
    fn both_spellings_through_a_symlinked_config() {
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let home = base.join("home");
        let real = base.join("dotfiles/config");
        std::fs::create_dir_all(real.join("gh")).expect("gh");
        std::fs::create_dir_all(&home).expect("home");
        std::os::unix::fs::symlink(&real, home.join(".config")).expect("symlink");
        let list = DenyList::for_session(Some(&home), Some("com.example.app"), true, Vec::new());
        for path in [
            home.join(".config/gh"),
            real.join("gh"),
            home.join(".config/com.example.app"),
            real.join("com.example.app"),
            home.join(".config/elitea*"),
            real.join("elitea*"),
        ] {
            assert!(list.paths().contains(&path), "{} missing", path.display());
        }
        assert!(list.covers(&real.join("gh/hosts.yml")));
        assert!(list.covers(&real.join("com.example.app/credentials.json")));
        assert!(list.covers(&real.join("elitea-cli/token")));
        assert!(!list.covers(&real.join("ghost")), "a sibling, not a child");
        assert!(!list.covers(&home.join("project/src")));
        assert_eq!(
            list.paths().iter().filter(|p| p.ends_with(".ssh")).count(),
            1,
            "one spelling when it resolves the same"
        );
    }

    /// Where the file system folds case, so does the list.
    #[test]
    fn covers_folds_case_where_the_file_system_does() {
        let list = DenyList::new([PathBuf::from("/nonexistent-h/.ssh")]);
        assert!(list.covers(Path::new("/nonexistent-h/.ssh/id")));
        assert_eq!(
            list.covers(Path::new("/nonexistent-h/.SSH/id")),
            crate::workspace::CASE_INSENSITIVE_FS
        );
    }
}
