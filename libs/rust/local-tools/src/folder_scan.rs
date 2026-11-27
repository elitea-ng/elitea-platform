//! A stat-only walk of a folder: every regular file's size, modification
//! time, inode and mode, without opening one. Copy checkpoints use it to
//! tell what changed since the last checkpoint (an unchanged file is not
//! read or hashed again), and the host diffs two scans to see what a
//! command created, changed or deleted.
//!
//! Two sets of rules:
//!
//! - [`ScanRules::Folder`], for a folder that is not a git work tree: its
//!   `.gitignore` and `.ignore` files are NOT honoured. A plain folder's
//!   ignore file says nothing about which of the person's documents need
//!   undo. The usual heavy, regenerable directories ([`HEAVY_DIRS`]) are
//!   left out; one with an ordinary name (`target`, `venv`, `Pods`) only
//!   beside the project file of the tool that makes it
//!   ([`HEAVY_DIR_MARKERS`]), so a person's own `target` folder of
//!   spreadsheets keeps its undo.
//! - [`ScanRules::Git`], for a git work tree: `.gitignore` is honoured, as
//!   in what git itself shows.
//!
//! Cloud placeholders (a file whose content lives only in iCloud Drive,
//! `OneDrive`, Dropbox and the like, see [`is_placeholder`]) are listed apart
//! and never opened. Reading one would start a download.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::sys::os::MetadataExt as _;
use crate::workspace::{WsPath, is_protected_name};

/// Directories a [`ScanRules::Folder`] scan leaves out, at any depth:
/// dependency and build caches a tool regenerates, never the person's own
/// work. Those named in [`HEAVY_DIR_MARKERS`] only beside one of their
/// markers.
pub const HEAVY_DIRS: &[&str] = &[
    "node_modules",
    "target",
    ".venv",
    "venv",
    "__pycache__",
    ".tox",
    ".mypy_cache",
    ".pytest_cache",
    ".ruff_cache",
    ".gradle",
    ".next",
    ".nuxt",
    ".turbo",
    ".parcel-cache",
    ".terraform",
    "Pods",
    "DerivedData",
];

/// macOS `SF_DATALESS`: the file's content is not on this disk (a File
/// Provider placeholder: iCloud Drive, `OneDrive`, Dropbox, …). Reading it
/// downloads it.
pub const SF_DATALESS: u32 = 0x4000_0000;
/// Windows `FILE_ATTRIBUTE_OFFLINE`.
pub const FILE_ATTRIBUTE_OFFLINE: u32 = 0x0000_1000;
/// Windows `FILE_ATTRIBUTE_RECALL_ON_OPEN`.
pub const FILE_ATTRIBUTE_RECALL_ON_OPEN: u32 = 0x0004_0000;
/// Windows `FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS` (`OneDrive`'s "online-only").
pub const FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS: u32 = 0x0040_0000;

/// Which ignore rules a scan follows.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ScanRules {
    /// Not a git work tree: ignore files are not honoured, and
    /// [`HEAVY_DIRS`] are left out.
    #[default]
    Folder,
    /// A git work tree: `.gitignore` (and `.ignore`) honoured.
    Git,
}

/// A file's stat, what tells that it changed without reading it.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct FileStat {
    pub len: u64,
    /// Nanoseconds since the epoch.
    pub mtime_ns: i128,
    /// `0` where the system has none (Windows).
    pub ino: u64,
    /// Permission bits.
    pub mode: u32,
}

impl FileStat {
    #[must_use]
    pub fn of(metadata: &std::fs::Metadata) -> Self {
        Self {
            len: metadata.len(),
            mtime_ns: i128::from(metadata.mtime()) * 1_000_000_000
                + i128::from(metadata.mtime_nsec()),
            ino: metadata.ino(),
            mode: metadata.mode() & 0o7777,
        }
    }
}

/// A finished scan.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct FolderScan {
    /// Every regular file (workspace-relative, `/`-joined) and its stat.
    pub files: BTreeMap<String, FileStat>,
    /// Cloud placeholders: listed, never opened.
    pub placeholders: BTreeSet<String>,
    /// The walk stopped at the file limit: `files` is incomplete.
    pub truncated: bool,
}

impl FolderScan {
    /// The bytes of every listed file.
    #[must_use]
    pub fn bytes(&self) -> u64 {
        self.files.values().map(|stat| stat.len).sum()
    }
}

/// What changed between two scans of one folder.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ScanDiff {
    pub created: Vec<String>,
    pub modified: Vec<String>,
    pub deleted: Vec<String>,
}

impl ScanDiff {
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.created.is_empty() && self.modified.is_empty() && self.deleted.is_empty()
    }

    /// Every changed path, sorted.
    #[must_use]
    pub fn paths(&self) -> Vec<String> {
        let mut all: Vec<String> = self
            .created
            .iter()
            .chain(&self.modified)
            .chain(&self.deleted)
            .cloned()
            .collect();
        all.sort();
        all
    }
}

/// The paths created, modified (any stat field differs, or it became or
/// stopped being a placeholder) and deleted from `before` to `after`.
/// Neither scan may be truncated: a path missing from a truncated scan is
/// not known to be gone.
#[must_use]
pub fn diff(before: &FolderScan, after: &FolderScan) -> ScanDiff {
    let mut out = ScanDiff::default();
    let present = |scan: &FolderScan, path: &str| {
        scan.files.contains_key(path) || scan.placeholders.contains(path)
    };
    for (path, stat) in &after.files {
        match before.files.get(path) {
            Some(old) if old == stat => {}
            Some(_) => out.modified.push(path.clone()),
            None if before.placeholders.contains(path) => out.modified.push(path.clone()),
            None => out.created.push(path.clone()),
        }
    }
    for path in &after.placeholders {
        if before.files.contains_key(path) {
            out.modified.push(path.clone());
        } else if !before.placeholders.contains(path) {
            out.created.push(path.clone());
        }
    }
    for path in before.files.keys().chain(&before.placeholders) {
        if !present(after, path) {
            out.deleted.push(path.clone());
        }
    }
    out.modified.sort();
    out.created.sort();
    out.deleted.sort();
    out
}

/// Whether `metadata`'s platform flags mark a cloud placeholder (macOS
/// `st_flags`, Windows file attributes).
#[must_use]
pub fn placeholder_flags(flags: u32) -> bool {
    if cfg!(windows) {
        flags
            & (FILE_ATTRIBUTE_OFFLINE
                | FILE_ATTRIBUTE_RECALL_ON_OPEN
                | FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS)
            != 0
    } else {
        flags & SF_DATALESS != 0
    }
}

/// The platform flags [`placeholder_flags`] reads; `0` where there are
/// none (Linux).
#[must_use]
pub fn platform_flags(metadata: &std::fs::Metadata) -> u32 {
    #[cfg(target_os = "macos")]
    {
        use std::os::macos::fs::MetadataExt as _;
        metadata.st_flags()
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt as _;
        metadata.file_attributes()
    }
    #[cfg(not(any(target_os = "macos", windows)))]
    {
        let _ = metadata;
        0
    }
}

/// Whether the file at `path` is a cloud placeholder: its content is not
/// on this disk, so reading it would download it. Only the stat is
/// looked at. iCloud's older stand-in, a `.<name>.icloud` file, counts
/// too. It is not the document itself.
#[must_use]
pub fn is_placeholder(path: &Path, metadata: &std::fs::Metadata) -> bool {
    let legacy_icloud = path
        .file_name()
        .and_then(|name| name.to_str())
        .is_some_and(|name| name.starts_with('.') && name.ends_with(".icloud"));
    legacy_icloud || placeholder_flags(platform_flags(metadata))
}

/// Tells a placeholder by its path and stat ([`is_placeholder`]; tests
/// pass their own).
pub type PlaceholderTest = fn(&Path, &std::fs::Metadata) -> bool;

/// Heavy directories with an ordinary name, and the files beside them that
/// say a tool made them: without one, the folder is the person's and is
/// scanned like any other.
pub const HEAVY_DIR_MARKERS: &[(&str, &[&str])] = &[
    ("target", &["Cargo.toml", "pom.xml"]),
    (
        "venv",
        &[
            "pyproject.toml",
            "requirements.txt",
            "setup.py",
            "setup.cfg",
        ],
    ),
    ("Pods", &["Podfile"]),
];

/// Whether the directory `name` at `path` is a heavy one to leave out.
fn heavy(name: &str, path: &Path) -> bool {
    if !HEAVY_DIRS.contains(&name) {
        return false;
    }
    match HEAVY_DIR_MARKERS.iter().find(|(dir, _)| *dir == name) {
        None => true,
        Some((_, markers)) => path
            .parent()
            .is_some_and(|parent| markers.iter().any(|marker| parent.join(marker).is_file())),
    }
}

/// Scan `root` under `rules`, stopping after `max_files` files
/// ([`FolderScan::truncated`]). Symlinks are not followed, and `.git` is
/// never entered.
#[must_use]
pub fn scan(root: &Path, rules: ScanRules, max_files: usize) -> FolderScan {
    scan_with(root, rules, max_files, is_placeholder)
}

/// [`scan`] with another placeholder test.
#[must_use]
pub fn scan_with(
    root: &Path,
    rules: ScanRules,
    max_files: usize,
    placeholder: PlaceholderTest,
) -> FolderScan {
    let mut builder = ignore::WalkBuilder::new(root);
    builder.hidden(false).follow_links(false);
    match rules {
        ScanRules::Git => {
            builder.require_git(false);
        }
        ScanRules::Folder => {
            builder
                .ignore(false)
                .git_ignore(false)
                .git_exclude(false)
                .git_global(false)
                .parents(false);
        }
    }
    builder.filter_entry(move |entry| {
        let Some(name) = entry.file_name().to_str() else {
            return true;
        };
        if is_protected_name(name) {
            return false;
        }
        !(rules == ScanRules::Folder
            && entry.depth() > 0
            && entry.file_type().is_some_and(|kind| kind.is_dir())
            && heavy(name, entry.path()))
    });
    let mut out = FolderScan::default();
    for entry in builder.build().flatten() {
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            continue;
        }
        let Ok(relative) = entry.path().strip_prefix(root) else {
            continue;
        };
        let Ok(path) = WsPath::from_relative(relative) else {
            continue;
        };
        let Ok(metadata) = entry.path().symlink_metadata() else {
            continue;
        };
        if out.files.len() + out.placeholders.len() >= max_files {
            out.truncated = true;
            break;
        }
        let name = path.display_string();
        if placeholder(entry.path(), &metadata) {
            out.placeholders.insert(name);
        } else {
            out.files.insert(name, FileStat::of(&metadata));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_plain_folder_keeps_ignored_documents_and_drops_heavy_directories() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        std::fs::write(root.join(".gitignore"), "*.docx\nnotes/\n").unwrap();
        std::fs::write(root.join("report.docx"), "doc").unwrap();
        std::fs::create_dir_all(root.join("notes")).unwrap();
        std::fs::write(root.join("notes/a.md"), "a").unwrap();
        std::fs::create_dir_all(root.join("node_modules/x")).unwrap();
        std::fs::write(root.join("node_modules/x/i.js"), "x").unwrap();
        std::fs::create_dir_all(root.join("sub/target")).unwrap();
        std::fs::write(root.join("sub/target/o.bin"), "o").unwrap();
        std::fs::write(root.join("sub/Cargo.toml"), "[package]").unwrap();
        let folder = scan(root, ScanRules::Folder, 100);
        let names: Vec<&str> = folder.files.keys().map(String::as_str).collect();
        assert_eq!(
            names,
            [".gitignore", "notes/a.md", "report.docx", "sub/Cargo.toml"]
        );
        // The git rules follow the ignore file.
        let git = scan(root, ScanRules::Git, 100);
        assert!(!git.files.contains_key("report.docx"));
        assert!(!git.files.contains_key("notes/a.md"));
    }

    #[test]
    fn an_ordinary_named_folder_is_left_out_only_beside_its_tools_project_file() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        // A person's own `target` folder of spreadsheets: kept.
        std::fs::create_dir_all(root.join("sales/target")).unwrap();
        std::fs::write(root.join("sales/target/q4.xlsx"), "q4").unwrap();
        // Cargo's `target`: left out.
        std::fs::create_dir_all(root.join("tool/target/debug")).unwrap();
        std::fs::write(root.join("tool/Cargo.toml"), "[package]").unwrap();
        std::fs::write(root.join("tool/target/debug/app"), "bin").unwrap();
        let names: Vec<String> = scan(root, ScanRules::Folder, 100)
            .files
            .into_keys()
            .collect();
        assert_eq!(names, ["sales/target/q4.xlsx", "tool/Cargo.toml"]);
    }

    #[test]
    fn placeholders_are_listed_apart_and_never_counted_as_files() {
        fn simulated(path: &Path, metadata: &std::fs::Metadata) -> bool {
            path.ends_with("cloud.pdf") || is_placeholder(path, metadata)
        }
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("here.txt"), "x").unwrap();
        std::fs::write(dir.path().join("cloud.pdf"), "").unwrap();
        std::fs::write(dir.path().join(".old.pages.icloud"), "stub").unwrap();
        let found = scan_with(dir.path(), ScanRules::Folder, 100, simulated);
        assert_eq!(
            found.placeholders.iter().collect::<Vec<_>>(),
            [".old.pages.icloud", "cloud.pdf"]
        );
        assert_eq!(found.files.keys().collect::<Vec<_>>(), ["here.txt"]);
    }

    #[test]
    fn the_platform_placeholder_flags_are_recognised() {
        if cfg!(windows) {
            assert!(placeholder_flags(FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS));
            assert!(placeholder_flags(FILE_ATTRIBUTE_OFFLINE));
        } else {
            assert!(placeholder_flags(SF_DATALESS | 0x1));
        }
        assert!(!placeholder_flags(0));
        assert!(!placeholder_flags(0x20));
    }

    #[test]
    fn a_diff_names_created_modified_and_deleted_files() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("keep.txt"), "k").unwrap();
        std::fs::write(dir.path().join("change.txt"), "c").unwrap();
        std::fs::write(dir.path().join("gone.txt"), "g").unwrap();
        let before = scan(dir.path(), ScanRules::Folder, 100);
        std::fs::write(dir.path().join("change.txt"), "changed").unwrap();
        std::fs::remove_file(dir.path().join("gone.txt")).unwrap();
        std::fs::write(dir.path().join("new.txt"), "n").unwrap();
        let after = scan(dir.path(), ScanRules::Folder, 100);
        let changes = diff(&before, &after);
        assert_eq!(changes.created, ["new.txt"]);
        assert_eq!(changes.modified, ["change.txt"]);
        assert_eq!(changes.deleted, ["gone.txt"]);
    }

    #[test]
    fn a_scan_stops_at_its_limit() {
        let dir = tempfile::tempdir().unwrap();
        for index in 0..5 {
            std::fs::write(dir.path().join(format!("{index}.txt")), "x").unwrap();
        }
        let found = scan(dir.path(), ScanRules::Folder, 3);
        assert!(found.truncated);
        assert_eq!(found.files.len(), 3);
    }
}
