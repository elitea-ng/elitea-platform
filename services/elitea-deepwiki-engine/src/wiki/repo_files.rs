//! Reading a file of the checked-out repository for the page context.
//!
//! DELIBERATE DIFFERENCE (ADR-0026, security): Python's
//! `_fetch_explicit_target_docs` read `os.path.join(repo_root, target)`
//! for a path the structure PLANNER chose — model output — with no
//! containment: an absolute target or one with `..` read any file the
//! worker could see (`/etc/passwd`, the service account token), and the
//! text went to the model and into the published wiki. `open()` also
//! followed symlinks, and the bare-name `os.walk` search descended into
//! `.git`.
//!
//! Here a target must be a relative path of plain components; every
//! component is checked with `symlink_metadata` and a symlink anywhere is
//! refused; the leaf must be a regular file; the walk never enters `.git`
//! or a symlinked directory and visits entries in sorted order (Python's
//! was the directory's own order).

use std::fs;
use std::io::Read as _;
use std::path::{Component, Path, PathBuf};

/// The checked path of `rel` under `root`, or `None` when it would escape
/// the root, pass through a symlink, or is not a regular file.
#[must_use]
pub fn contained_path(root: &Path, rel: &Path) -> Option<PathBuf> {
    let mut path = root.to_path_buf();
    let mut any = false;
    for component in rel.components() {
        match component {
            Component::Normal(part) => {
                if part == ".git" {
                    return None;
                }
                path.push(part);
                any = true;
                let meta = fs::symlink_metadata(&path).ok()?;
                if meta.file_type().is_symlink() {
                    return None;
                }
            }
            Component::CurDir => {}
            Component::ParentDir | Component::RootDir | Component::Prefix(_) => return None,
        }
    }
    if !any {
        return None;
    }
    let meta = fs::symlink_metadata(&path).ok()?;
    meta.file_type().is_file().then_some(path)
}

/// The bytes of `rel` under `root` (see [`contained_path`]).
#[must_use]
pub fn read_contained(root: &Path, rel: &Path) -> Option<Vec<u8>> {
    fs::read(contained_path(root, rel)?).ok()
}

/// At most the first `max_bytes` bytes of `rel` under `root` (see
/// [`contained_path`]). A caller that keeps `n` characters reads
/// `n * 4 + 8` bytes: no character is longer than 4 bytes, and the
/// sequence a cut splits decodes past the `n` kept.
#[must_use]
pub fn read_contained_prefix(root: &Path, rel: &Path, max_bytes: usize) -> Option<Vec<u8>> {
    let file = fs::File::open(contained_path(root, rel)?).ok()?;
    let mut bytes = Vec::new();
    file.take(u64::try_from(max_bytes).unwrap_or(u64::MAX))
        .read_to_end(&mut bytes)
        .ok()?;
    Some(bytes)
}

/// The first file named `name` in a top-down walk of `root` (sorted, no
/// `.git`, no symlinks), as a path relative to `root`.
#[must_use]
pub fn find_by_name(root: &Path, name: &str) -> Option<PathBuf> {
    let mut stack: Vec<PathBuf> = vec![PathBuf::new()];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = fs::read_dir(root.join(&dir)) else {
            continue;
        };
        let mut files: Vec<String> = Vec::new();
        let mut dirs: Vec<String> = Vec::new();
        for entry in entries.flatten() {
            let Ok(file_type) = entry.file_type() else {
                continue;
            };
            let Ok(entry_name) = entry.file_name().into_string() else {
                continue;
            };
            if file_type.is_dir() && entry_name != ".git" {
                dirs.push(entry_name);
            } else if file_type.is_file() {
                files.push(entry_name);
            }
        }
        if files.iter().any(|f| f == name) {
            return Some(dir.join(name));
        }
        dirs.sort();
        // Pre-order, first sorted directory first.
        for sub in dirs.into_iter().rev() {
            stack.push(dir.join(sub));
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn escapes_and_symlinks_are_refused() {
        let base = std::env::temp_dir().join(format!("dw-repo-files-{}", std::process::id()));
        let root = base.join("repo");
        fs::create_dir_all(root.join("docs/deep")).unwrap();
        fs::write(root.join("docs/a.md"), "A").unwrap();
        fs::write(root.join("docs/deep/b.md"), "B").unwrap();
        fs::write(base.join("secret.txt"), "S").unwrap();
        #[cfg(unix)]
        std::os::unix::fs::symlink(base.join("secret.txt"), root.join("docs/link.md")).unwrap();
        assert_eq!(
            read_contained(&root, Path::new("docs/a.md")),
            Some(b"A".to_vec())
        );
        assert!(read_contained(&root, Path::new("../secret.txt")).is_none());
        assert!(read_contained(&root, &base.join("secret.txt")).is_none());
        assert!(read_contained(&root, Path::new("docs/link.md")).is_none());
        assert!(read_contained(&root, Path::new("docs")).is_none());
        assert_eq!(
            find_by_name(&root, "b.md"),
            Some(PathBuf::from("docs/deep/b.md"))
        );
        assert!(find_by_name(&root, "link.md").is_none());
        assert_eq!(
            read_contained_prefix(&root, Path::new("docs/a.md"), 10),
            Some(b"A".to_vec())
        );
        fs::write(root.join("docs/long.md"), "x".repeat(100)).unwrap();
        assert_eq!(
            read_contained_prefix(&root, Path::new("docs/long.md"), 10).map(|b| b.len()),
            Some(10)
        );
        assert!(read_contained_prefix(&root, Path::new("docs/link.md"), 10).is_none());
        fs::remove_dir_all(&base).unwrap();
    }
}
