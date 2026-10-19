//! The shadow git directory of a folder that is not a git work tree
//! (ADR-0034 decision 5): kept in the desktop's app data, never inside the
//! folder, with a default ignore set (`info/exclude`). From there the send
//! and take-back are the same as for a repository.
//!
//! The app data directory is on the session's deny list (no local tool,
//! host git included, reads it). Host git working on the shadow needs its
//! own directory, and nothing else of the app's data: [`carve`] replaces a
//! deny entry above the shadow with the entry's other children, so the
//! rest of the app's data stays denied.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use sha2::{Digest as _, Sha256};

use crate::deny::DenyList;
use crate::error::{ToolError, ToolResult};

/// What a shadow repository ignores besides the folder's own
/// `.gitignore` files.
pub const DEFAULT_IGNORES: &[&str] = &[
    "node_modules/",
    "target/",
    ".venv/",
    "venv/",
    "__pycache__/",
    "dist/",
    "build/",
    ".gradle/",
    ".next/",
    ".nuxt/",
    ".cache/",
    ".tox/",
    ".mypy_cache/",
    ".pytest_cache/",
    ".DS_Store",
    "*.pyc",
];

/// The shadow directory of `root` under `data_dir`.
#[must_use]
pub fn dir_for(data_dir: &Path, root: &Path) -> PathBuf {
    let key = Sha256::digest(root.as_os_str().as_encoded_bytes());
    let key = key[..8].iter().fold(String::new(), |mut out, byte| {
        use std::fmt::Write as _;
        let _ = write!(out, "{byte:02x}");
        out
    });
    data_dir.join("handoff").join("shadow").join(key)
}

/// Create the shadow git directory (idempotent): `HEAD`, `objects/`,
/// `refs/`, a minimal `config` and the default ignores.
///
/// # Errors
///
/// It cannot be created.
pub fn ensure(git_dir: &Path) -> ToolResult<()> {
    let io = |error: std::io::Error| ToolError::io("cannot prepare the shadow repository", &error);
    for sub in [
        "objects/info",
        "objects/pack",
        "refs/heads",
        "refs/tags",
        "info",
    ] {
        std::fs::create_dir_all(git_dir.join(sub)).map_err(io)?;
    }
    let write_new = |name: &str, text: &str| -> ToolResult<()> {
        let path = git_dir.join(name);
        if path.exists() {
            return Ok(());
        }
        std::fs::write(path, text).map_err(io)
    };
    write_new("HEAD", "ref: refs/heads/main\n")?;
    write_new(
        "config",
        "[core]\n\trepositoryformatversion = 0\n\tbare = false\n",
    )?;
    let mut exclude =
        String::from("# Elitea handoff: the default ignore set of a shadow repository\n");
    for pattern in DEFAULT_IGNORES {
        exclude.push_str(pattern);
        exclude.push('\n');
    }
    std::fs::write(git_dir.join("info/exclude"), exclude).map_err(io)
}

/// Whether `ancestor` is `path` or above it.
fn is_ancestor(ancestor: &Path, path: &Path) -> bool {
    path.starts_with(ancestor)
}

/// The entries a deny entry stands for once `target` is opened inside it.
fn carve_entry(entry: &Path, target: &Path, out: &mut Vec<PathBuf>, depth: usize) {
    if depth > 16 || !is_ancestor(entry, target) {
        out.push(entry.to_path_buf());
        return;
    }
    if entry == target {
        return;
    }
    let Ok(children) = std::fs::read_dir(entry) else {
        return;
    };
    for child in children.flatten() {
        carve_entry(&child.path(), target, out, depth + 1);
    }
}

/// `deny` with `target` (and everything under it) opened, every other path
/// it covers still denied. A name-prefix entry (`…/name*`) stands for the
/// entries of its directory whose names start with it.
#[must_use]
pub fn carve(deny: &DenyList, target: &Path) -> Arc<DenyList> {
    let mut out = Vec::new();
    for literal in deny.literals() {
        let text = literal.to_string_lossy();
        if let Some(stem) = text.strip_suffix('*') {
            let stem = Path::new(stem);
            let (Some(parent), Some(prefix)) = (stem.parent(), stem.file_name()) else {
                out.push(literal.clone());
                continue;
            };
            let prefix = prefix.to_string_lossy();
            let mut on_path = false;
            if let Ok(entries) = std::fs::read_dir(parent) {
                for entry in entries.flatten() {
                    let path = entry.path();
                    if entry
                        .file_name()
                        .to_string_lossy()
                        .starts_with(prefix.as_ref())
                        && is_ancestor(&path, target)
                    {
                        on_path = true;
                        carve_entry(&path, target, &mut out, 0);
                    }
                }
            }
            if on_path {
                // The prefix itself would cover the target: the carved
                // entries above stand for it, plus later siblings.
                if let Ok(entries) = std::fs::read_dir(parent) {
                    for entry in entries.flatten() {
                        let path = entry.path();
                        if entry
                            .file_name()
                            .to_string_lossy()
                            .starts_with(prefix.as_ref())
                            && !is_ancestor(&path, target)
                        {
                            out.push(path);
                        }
                    }
                }
            } else {
                out.push(literal.clone());
            }
            continue;
        }
        carve_entry(literal, target, &mut out, 0);
    }
    Arc::new(DenyList::new(out))
}

#[cfg(test)]
mod tests {
    use super::{carve, dir_for, ensure};
    use crate::deny::DenyList;

    #[test]
    fn a_carved_list_opens_the_shadow_only() {
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let data = base.join("data");
        let shadow = dir_for(&data, &base.join("folder"));
        ensure(&shadow).expect("ensure");
        std::fs::write(data.join("threads.sqlite"), "x").expect("write");
        std::fs::create_dir_all(data.join("workspaces/abc")).expect("mkdir");
        let other = base.join("elsewhere");
        let deny = DenyList::new([data.clone(), other.clone(), base.join("data-other*")]);
        let carved = carve(&deny, &shadow);
        assert!(!carved.covers(&shadow.join("objects")));
        assert!(carved.covers(&data.join("threads.sqlite")));
        assert!(carved.covers(&data.join("workspaces/abc")));
        assert!(carved.covers(&other));
        assert!(
            !carved.covers(&data),
            "the directory itself is no longer an entry"
        );
        assert!(
            std::fs::read_to_string(shadow.join("info/exclude"))
                .expect("read")
                .contains("node_modules/")
        );
    }
}
