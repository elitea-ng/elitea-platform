//! Path lookup for the person's "@" file picker: which files and folders of
//! the workspace match what they typed.
//!
//! Same view of the folder as `list_tree`: `.gitignore` honoured, `.git`
//! and `path_deny` matches left out. Symlinks are left out too (the picker
//! names paths the agent then reads with its own confined tools, and a link
//! may lead out of the folder). Names only: nothing here opens a file.

use serde::Serialize;

use crate::error::ToolResult;
use crate::files::{relative, walker};
use crate::workspace::{Intent, Workspace, WsPath, nfc};

/// The most entries one lookup walks: a huge folder answers from its first
/// part rather than stalling the picker.
pub const MAX_VISITED: usize = 20_000;
/// The most matches one lookup answers.
pub const MAX_LIMIT: usize = 200;

/// One match.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FoundPath {
    /// Workspace-relative, `/`-separated, without a trailing `/`.
    pub path: String,
    /// `file` or `dir`.
    pub kind: &'static str,
}

/// How well `path` matches `query` (both lowercased NFC); lower is better,
/// `None` is no match. The last component containing the query beats a
/// match elsewhere in the path, which beats the query's characters merely
/// appearing in order.
fn score(path: &str, query: &str) -> Option<u8> {
    if query.is_empty() {
        return Some(0);
    }
    let name = path.rsplit('/').next().unwrap_or(path);
    if name.starts_with(query) {
        return Some(0);
    }
    if name.contains(query) {
        return Some(1);
    }
    if path.contains(query) {
        return Some(2);
    }
    let mut wanted = query.chars().filter(|c| !c.is_whitespace()).peekable();
    for c in path.chars() {
        if wanted.peek() == Some(&c) {
            wanted.next();
        }
    }
    wanted.peek().is_none().then_some(3)
}

/// The workspace's files and folders matching `query`, best first (then
/// shallower, then shorter, then by name), at most `limit` (clamped to
/// 1..=[`MAX_LIMIT`]). An empty query lists the shallowest entries.
///
/// # Errors
///
/// When the walk cannot start (the workspace is gone).
pub fn find_paths(workspace: &Workspace, query: &str, limit: usize) -> ToolResult<Vec<FoundPath>> {
    let limit = limit.clamp(1, MAX_LIMIT);
    let query = nfc(query
        .trim()
        .trim_start_matches('@')
        .trim_start_matches("./"))
    .to_lowercase();
    let mut ranked: Vec<(u8, usize, String, FoundPath)> = Vec::new();
    let root = WsPath::root();
    for entry in walker(workspace, &root, None, None)?
        .take(MAX_VISITED)
        .flatten()
    {
        let Some(kind) = entry.file_type() else {
            continue;
        };
        let kind = if kind.is_dir() {
            "dir"
        } else if kind.is_file() {
            "file"
        } else {
            continue;
        };
        let Some(path) = relative(workspace, entry.path()) else {
            continue;
        };
        if path.is_root() || workspace.denied_by(&path, Intent::Read).is_some() {
            continue;
        }
        let shown = path.display_string();
        let key = nfc(&shown).to_lowercase();
        let Some(rank) = score(&key, &query) else {
            continue;
        };
        ranked.push((
            rank,
            path.components().len(),
            key,
            FoundPath { path: shown, kind },
        ));
    }
    ranked.sort_by(|a, b| (a.0, a.1, a.2.len(), &a.2).cmp(&(b.0, b.1, b.2.len(), &b.2)));
    Ok(ranked
        .into_iter()
        .take(limit)
        .map(|(_, _, _, found)| found)
        .collect())
}

#[cfg(test)]
mod tests {
    use std::fs;

    use super::{FoundPath, MAX_LIMIT, find_paths, score};
    use crate::workspace::Workspace;

    fn tree(deny: &[&str]) -> (tempfile::TempDir, Workspace) {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        fs::create_dir_all(root.join("src/components")).unwrap();
        fs::create_dir_all(root.join("target/debug")).unwrap();
        fs::create_dir_all(root.join("secrets")).unwrap();
        fs::create_dir_all(root.join(".git")).unwrap();
        fs::write(root.join(".gitignore"), "target/\n*.log\n").unwrap();
        fs::write(root.join("README.md"), "hi").unwrap();
        fs::write(root.join("src/main.rs"), "fn main() {}").unwrap();
        fs::write(root.join("src/components/Button.tsx"), "x").unwrap();
        fs::write(root.join("target/debug/main"), "bin").unwrap();
        fs::write(root.join("build.log"), "log").unwrap();
        fs::write(root.join("secrets/key.pem"), "k").unwrap();
        fs::write(root.join(".env"), "A=1").unwrap();
        fs::write(root.join(".git/config"), "[core]").unwrap();
        let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
        let workspace = Workspace::open(root, &deny).unwrap();
        (dir, workspace)
    }

    fn paths(found: &[FoundPath]) -> Vec<&str> {
        found.iter().map(|f| f.path.as_str()).collect()
    }

    #[test]
    fn gitignored_and_git_internals_are_left_out() {
        let (_dir, workspace) = tree(&[]);
        let all = find_paths(&workspace, "", MAX_LIMIT).unwrap();
        let all = paths(&all);
        assert!(all.contains(&"src/main.rs"), "{all:?}");
        assert!(all.contains(&".gitignore"), "{all:?}");
        assert!(!all.iter().any(|p| p.starts_with("target")), "{all:?}");
        assert!(!all.contains(&"build.log"), "{all:?}");
        assert!(
            !all.iter().any(|p| p.starts_with(".git/") || *p == ".git"),
            "{all:?}"
        );
    }

    #[test]
    fn path_deny_matches_are_left_out() {
        let (_dir, workspace) = tree(&["secrets/**", ".env"]);
        let all = find_paths(&workspace, "", MAX_LIMIT).unwrap();
        let all = paths(&all);
        assert!(!all.iter().any(|p| p.contains("key.pem")), "{all:?}");
        assert!(!all.contains(&".env"), "{all:?}");
        assert!(find_paths(&workspace, "pem", 10).unwrap().is_empty());
    }

    #[test]
    fn symlinks_are_neither_listed_nor_followed() {
        let outside = tempfile::tempdir().unwrap();
        fs::write(outside.path().join("passwords.txt"), "x").unwrap();
        let (dir, workspace) = tree(&[]);
        std::os::unix::fs::symlink(outside.path(), dir.path().join("escape")).unwrap();
        std::os::unix::fs::symlink(
            outside.path().join("passwords.txt"),
            dir.path().join("link.txt"),
        )
        .unwrap();
        let all = find_paths(&workspace, "", MAX_LIMIT).unwrap();
        let all = paths(&all);
        assert!(
            !all.iter()
                .any(|p| p.contains("escape") || p.contains("passwords") || *p == "link.txt"),
            "{all:?}"
        );
    }

    #[test]
    fn answers_at_most_the_limit_best_first() {
        let (dir, workspace) = tree(&[]);
        for i in 0..80 {
            fs::write(dir.path().join(format!("file{i:02}.txt")), "x").unwrap();
        }
        assert_eq!(find_paths(&workspace, "", 50).unwrap().len(), 50);
        assert_eq!(find_paths(&workspace, "file", 0).unwrap().len(), 1);
        assert_eq!(
            find_paths(&workspace, "", 10_000).unwrap().len(),
            // 80 files + 9 entries of the fixture (the limit is clamped, not refused).
            89
        );

        let found = find_paths(&workspace, "main", 10).unwrap();
        assert_eq!(found.first().map(|f| f.path.as_str()), Some("src/main.rs"));
        let dirs = find_paths(&workspace, "comp", 10).unwrap();
        assert_eq!(
            dirs.first(),
            Some(&FoundPath {
                path: "src/components".into(),
                kind: "dir"
            })
        );
        // Characters in order, across components.
        assert_eq!(
            paths(&find_paths(&workspace, "scbtn", 10).unwrap()),
            vec!["src/components/Button.tsx"]
        );
        // Case-insensitive; a leading `./` or `@` is ignored.
        assert_eq!(
            paths(&find_paths(&workspace, "@./BUTTON", 10).unwrap()),
            vec!["src/components/Button.tsx"]
        );
    }

    #[test]
    fn ranking_prefers_the_name_over_the_path() {
        assert_eq!(score("src/main.rs", "main"), Some(0));
        assert_eq!(score("src/domain.rs", "main"), Some(1));
        assert_eq!(score("main/lib.rs", "main"), Some(2));
        assert_eq!(score("m/a/i/n.rs", "main"), Some(3));
        assert_eq!(score("src/lib.rs", "main"), None);
    }
}
