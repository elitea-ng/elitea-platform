//! Reading repository files by relative path, inside the repository only.
//!
//! The Phase 1c API-surface pass reads files the graph names
//! (`Path(repo_root) / rel_path`, `read_text(encoding="utf-8",
//! errors="ignore")`). Python's `is_file()` follows symbolic links and an
//! absolute or `..` path leaves the root. ADR-0026 forbids both: a cloned
//! repository is untrusted input. So a path is read only when it is
//! relative, has no `..` part, and no part of it (directories included) is
//! a symbolic link. Discovery never follows links either, so every path a
//! parser saw passes; the rule differs from Python only for a path that
//! would have escaped.

use super::pystr;
use std::collections::{HashMap, VecDeque};
use std::path::PathBuf;

/// The text of `<repo_root>/<rel_path>` in Python text mode, or `None`
/// when the path is unsafe, not a regular file, or unreadable.
#[must_use]
pub fn read_repo_text(repo_root: &str, rel_path: &str) -> Option<String> {
    if rel_path.is_empty() || rel_path.starts_with('/') || rel_path.contains('\0') {
        return None;
    }
    let parts: Vec<&str> = rel_path
        .split('/')
        .filter(|part| !part.is_empty() && *part != ".")
        .collect();
    if parts.is_empty() || parts.contains(&"..") {
        return None;
    }
    let mut path = PathBuf::from(repo_root);
    for (index, part) in parts.iter().enumerate() {
        path.push(part);
        let metadata = std::fs::symlink_metadata(&path).ok()?;
        let last = index + 1 == parts.len();
        let usable = if last {
            metadata.file_type().is_file()
        } else {
            metadata.file_type().is_dir()
        };
        if !usable {
            return None;
        }
    }
    let bytes = std::fs::read(&path).ok()?;
    Some(pystr::decode_text(&bytes, pystr::Errors::Ignore))
}

/// A per-pass cache of file texts, keyed by relative path.
///
/// It holds the [`TextCache::CAPACITY`] most recently read files, not every
/// file: a pass walks the graph's nodes, which come file by file, and over a
/// large repository an unbounded cache held the whole source tree. A file
/// read again after its text was dropped reads the same text — the
/// repository does not change during a build.
#[derive(Debug, Default)]
pub struct TextCache {
    root: Option<String>,
    texts: HashMap<String, Option<String>>,
    /// Cached paths, oldest first.
    order: VecDeque<String>,
}

impl TextCache {
    /// How many file texts the cache holds.
    pub const CAPACITY: usize = 16;

    /// A cache over `repo_root`; `None` reads nothing (Python's
    /// `repo_root=None`).
    #[must_use]
    pub fn new(repo_root: Option<&str>) -> Self {
        Self {
            root: repo_root.map(str::to_owned),
            texts: HashMap::new(),
            order: VecDeque::new(),
        }
    }

    /// Whether a root was given.
    #[must_use]
    pub fn has_root(&self) -> bool {
        self.root.is_some()
    }

    /// The text of `rel_path`, read when it is not cached.
    pub fn text(&mut self, rel_path: &str) -> Option<&str> {
        let root = self.root.as_deref()?;
        if !self.texts.contains_key(rel_path) {
            let text = read_repo_text(root, rel_path);
            if self.order.len() == Self::CAPACITY
                && let Some(oldest) = self.order.pop_front()
            {
                self.texts.remove(&oldest);
            }
            self.texts.insert(rel_path.to_owned(), text);
            self.order.push_back(rel_path.to_owned());
        }
        self.texts.get(rel_path).and_then(Option::as_deref)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_inside_the_root_only() {
        let dir = std::env::temp_dir().join(format!("dw-repo-files-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("a")).unwrap();
        std::fs::write(dir.join("a/x.py"), b"one\r\ntwo\xff").unwrap();
        std::fs::write(dir.join("outside.txt"), b"secret").unwrap();
        std::os::unix::fs::symlink(dir.join("a/x.py"), dir.join("link.py")).unwrap();
        std::os::unix::fs::symlink(dir.join("a"), dir.join("dirlink")).unwrap();
        let root = dir.join("a");
        let root = root.to_str().unwrap();
        assert_eq!(read_repo_text(root, "x.py").as_deref(), Some("one\ntwo"));
        assert_eq!(read_repo_text(root, "./x.py").as_deref(), Some("one\ntwo"));
        assert_eq!(read_repo_text(root, "../outside.txt"), None);
        assert_eq!(read_repo_text(root, "/etc/hosts"), None);
        assert_eq!(read_repo_text(root, ""), None);
        let top = dir.to_str().unwrap();
        assert_eq!(read_repo_text(top, "link.py"), None);
        assert_eq!(read_repo_text(top, "dirlink/x.py"), None);
        assert_eq!(read_repo_text(top, "a"), None);
        let mut cache = TextCache::new(Some(top));
        assert_eq!(cache.text("a/x.py"), Some("one\ntwo"));
        assert_eq!(cache.text("missing"), None);
        assert_eq!(TextCache::new(None).text("a/x.py"), None);
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn the_cache_holds_the_latest_files_and_reads_an_evicted_one_again() {
        let dir = std::env::temp_dir().join(format!("dw-text-cache-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let count = TextCache::CAPACITY + 2;
        for i in 0..count {
            std::fs::write(dir.join(format!("f{i}.txt")), format!("text {i}")).unwrap();
        }
        let mut cache = TextCache::new(dir.to_str());
        for i in 0..count {
            assert_eq!(
                cache.text(&format!("f{i}.txt")),
                Some(format!("text {i}").as_str())
            );
        }
        assert_eq!(cache.texts.len(), TextCache::CAPACITY);
        assert!(!cache.texts.contains_key("f0.txt"));
        assert_eq!(cache.text("f0.txt"), Some("text 0"));
        assert_eq!(cache.texts.len(), TextCache::CAPACITY);
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
