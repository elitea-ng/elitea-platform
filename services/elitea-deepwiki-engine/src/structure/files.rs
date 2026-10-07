//! The repository's file list as the analysis node sees it
//! (`_get_repository_file_paths` over `filter_manager.FilterManager`).
//!
//! The live indexer (`FilesystemRepositoryIndexer`) has neither
//! `get_repository_files` nor a traverser, so the agent walks the clone
//! with `os.walk` and the indexer's `FilterManager`. No `repo.json` ships
//! with the engine, so the filter is its defaults: the excluded directory
//! names and the allowed extensions below, no excluded files, no size
//! limit. This list is NOT graph discovery (`graph::discover`): it decides
//! the tree, the file statistics and the code samples of the analysis
//! prompt, and the `auto` planner's file count.
//!
//! Quirks kept, because the prompt shows them:
//!
//! * a directory path is `strip('./')`-ed before it is matched, so
//!   `.github` at the root becomes `github` and is walked, and a root
//!   `.git` becomes `git` — also walked (none of its files has an allowed
//!   extension); `src/.git` keeps its dot and is excluded;
//! * a name that starts with a dot has no suffix (`PurePath.suffix`), so
//!   `.gitignore` is not listed although `.gitignore` is an "allowed
//!   extension".
//!
//! Deliberate differences:
//!
//! * `os.walk` lists a directory in file-system order; the walk here sorts
//!   each listing by name. The order reaches only the ties of the file
//!   statistics' count sort;
//! * a symbolic link is listed (as `os.walk` lists it: a link to a
//!   directory is a directory that is not entered, anything else a file)
//!   but never read: [`read_prefix`] refuses it, where Python opened its
//!   target;
//! * a name that is not UTF-8 is skipped (Python kept it, escaped).

use crate::graph::pystr;
use std::io::Read;
use std::path::Path;

/// `FilterManager._build_excluded_dirs` defaults.
const EXCLUDED_DIRS: &[&str] = &[
    ".git",
    ".svn",
    ".hg",
    ".bzr",
    ".idea",
    ".vscode",
    "node_modules",
    "bower_components",
    "jspm_packages",
    "__pycache__",
    ".pytest_cache",
    ".tox",
    "venv",
    ".venv",
    "env",
    "virtualenv",
    "build",
    "dist",
    "target",
    "out",
    "bin",
    ".output",
    "coverage",
    "htmlcov",
    ".nyc_output",
];

/// `FilterManager._build_allowed_extensions` defaults.
const ALLOWED_EXTENSIONS: &[&str] = &[
    ".py",
    ".js",
    ".ts",
    ".jsx",
    ".tsx",
    ".java",
    ".go",
    ".cs",
    ".cpp",
    ".cc",
    ".cxx",
    ".c++",
    ".c",
    ".h",
    ".hpp",
    ".hh",
    ".hxx",
    ".rb",
    ".php",
    ".rs",
    ".kt",
    ".scala",
    ".swift",
    ".dart",
    ".r",
    ".m",
    ".sh",
    ".bash",
    ".zsh",
    ".ps1",
    ".bat",
    ".cmd",
    ".json",
    ".yaml",
    ".yml",
    ".toml",
    ".xml",
    ".ini",
    ".cfg",
    ".conf",
    ".gradle",
    ".kts",
    ".wsdl",
    ".xsd",
    ".proto",
    ".tf",
    ".tfvars",
    ".hcl",
    ".mod",
    ".md",
    ".rst",
    ".txt",
    ".adoc",
    ".org",
    ".html",
    ".htm",
    ".css",
    ".scss",
    ".sass",
    ".less",
    ".sql",
    ".csv",
    ".tsv",
    ".dockerfile",
    ".gitignore",
    ".gitattributes",
];

/// `FilterManager.should_process_directory` for a repository-relative
/// directory path.
#[must_use]
pub fn should_process_directory(dir_path: &str) -> bool {
    if dir_path.is_empty() {
        return true;
    }
    let normalized = pystr::strip_chars(dir_path, "./");
    for pattern in EXCLUDED_DIRS {
        if normalized == *pattern
            || normalized
                .strip_prefix(pattern)
                .is_some_and(|rest| rest.starts_with('/'))
            || pystr::fnmatch(normalized, pattern)
        {
            return false;
        }
    }
    !normalized
        .split('/')
        .any(|part| EXCLUDED_DIRS.contains(&part))
}

/// `FilterManager.should_process_file` (no size given, no excluded files):
/// the lower-cased suffix must be allowed.
#[must_use]
pub fn should_process_file(file_path: &str) -> bool {
    let suffix = pystr::suffix(file_path).to_lowercase();
    ALLOWED_EXTENSIONS.contains(&suffix.as_str())
}

/// The agent's file list: repository-relative paths in `os.walk` order
/// (top-down, a directory's files before its subdirectories), each listing
/// sorted by name.
#[must_use]
pub fn repository_files(repo_root: &Path) -> Vec<String> {
    let mut files = Vec::new();
    walk(repo_root, "", &mut files);
    files
}

fn walk(dir: &Path, rel_dir: &str, out: &mut Vec<String>) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    let mut names: Vec<(String, bool)> = Vec::new();
    for entry in entries.flatten() {
        let Ok(name) = entry.file_name().into_string() else {
            continue;
        };
        let Ok(kind) = entry.file_type() else {
            continue;
        };
        // `DirEntry.is_dir()` follows a link: a link to a directory is
        // listed as a directory (and, `followlinks=False`, not entered).
        let is_dir = if kind.is_symlink() {
            std::fs::metadata(entry.path()).is_ok_and(|meta| meta.is_dir())
        } else {
            kind.is_dir()
        };
        names.push((name, is_dir));
    }
    names.sort();
    let rel = |name: &str| {
        if rel_dir.is_empty() {
            name.to_owned()
        } else {
            format!("{rel_dir}/{name}")
        }
    };
    let mut dirs = Vec::new();
    for (name, is_dir) in &names {
        let path = rel(name);
        if *is_dir {
            if should_process_directory(&path) {
                dirs.push((name.clone(), path));
            }
        } else if should_process_file(&path) {
            out.push(path);
        }
    }
    for (name, path) in dirs {
        let child = dir.join(&name);
        if std::fs::symlink_metadata(&child).is_ok_and(|meta| meta.file_type().is_symlink()) {
            continue;
        }
        walk(&child, &path, out);
    }
}

/// `open(path, encoding="utf-8", errors="replace").read(max_chars)` of a
/// regular file under `repo_root`: the first `max_chars` characters after
/// decoding and universal newlines. An unreadable path, a link, or a
/// directory reads as `""`, as Python's `except: return ""` did.
#[must_use]
pub fn read_prefix(repo_root: &Path, rel_path: &str, max_chars: usize) -> String {
    let path = repo_root.join(rel_path);
    let Ok(meta) = std::fs::symlink_metadata(&path) else {
        return String::new();
    };
    if !meta.file_type().is_file() {
        return String::new();
    }
    let Ok(file) = std::fs::File::open(&path) else {
        return String::new();
    };
    // `max_chars` characters need at most 4 bytes each; a sequence cut at
    // the end decodes to one U+FFFD past them.
    let limit = u64::try_from(max_chars.saturating_mul(4).saturating_add(8)).unwrap_or(u64::MAX);
    let mut bytes = Vec::new();
    if file.take(limit).read_to_end(&mut bytes).is_err() {
        return String::new();
    }
    let text = pystr::decode_text(&bytes, pystr::Errors::Replace);
    pystr::prefix_chars(&text, max_chars).to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn directories_are_matched_after_strip() {
        assert!(should_process_directory(".github"));
        assert!(
            should_process_directory(".git"),
            "strip('./') turns it into 'git'"
        );
        assert!(!should_process_directory("src/.git"));
        assert!(!should_process_directory("node_modules"));
        assert!(!should_process_directory("a/build/x"));
        assert!(!should_process_directory("./target"));
        assert!(should_process_directory("builder"));
        assert!(should_process_directory(""));
    }

    #[test]
    fn files_need_an_allowed_suffix() {
        assert!(should_process_file("a/b.PY"));
        assert!(should_process_file("x.c++"));
        assert!(
            !should_process_file(".gitignore"),
            "a dot name has no suffix"
        );
        assert!(!should_process_file("Makefile"));
        assert!(!should_process_file("a.b/c"));
    }

    #[test]
    fn walk_lists_files_before_subdirectories() {
        let dir = std::env::temp_dir().join(format!("dw-files-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        for path in [
            "b.py",
            "a/z.md",
            "a/c/d.txt",
            "node_modules/x.js",
            "build/y.py",
            ".github/w.yml",
            "README",
        ] {
            let full = dir.join(path);
            std::fs::create_dir_all(full.parent().expect("parent")).expect("mkdir");
            std::fs::write(&full, "x\r\ny").expect("write");
        }
        let files = repository_files(&dir);
        assert_eq!(files, ["b.py", ".github/w.yml", "a/z.md", "a/c/d.txt"]);
        assert_eq!(read_prefix(&dir, "b.py", 2), "x\n");
        assert_eq!(read_prefix(&dir, "missing.py", 2), "");
        let _ = std::fs::remove_dir_all(&dir);
    }
}
