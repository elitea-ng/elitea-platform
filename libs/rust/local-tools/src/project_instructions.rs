//! `AGENTS.md` (the cross-tool convention, agents.md): the instructions a
//! repository keeps for coding agents.
//!
//! [`load`] reads the workspace's root `AGENTS.md` and, for each path the
//! person referenced, the `AGENTS.md` files of the folders between it and
//! the root, nearest first (the closest one applies to files in its
//! subtree). Names match without regard to case (`agents.md`,
//! `Agents.md`); an exact `AGENTS.md` wins over another spelling in the
//! same folder. Every read goes through the confined [`Workspace`]: a
//! symlinked file or folder is not read, a `path_deny` match is refused,
//! and the total text is capped.

use std::io::Read as _;

use crate::workspace::{EntryKind, Intent, Workspace, WsPath};

/// The file name of the convention.
pub const FILE_NAME: &str = "AGENTS.md";
/// The most bytes of `AGENTS.md` text one turn takes, all files together.
pub const MAX_TOTAL_BYTES: usize = 32 * 1024;
/// What a cut file ends with.
pub const TRUNCATED_NOTE: &str = "[… truncated: the AGENTS.md text past this point was not read]";

/// One `AGENTS.md` that was read.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct InstructionFile {
    /// Workspace-relative path.
    pub path: String,
    /// Its text (lossy UTF-8), cut at the cap.
    pub text: String,
    pub truncated: bool,
}

/// What [`load`] found.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ProjectInstructions {
    /// Read, in order: the root's first, then the nested ones.
    pub files: Vec<InstructionFile>,
    /// Found but not read, with why (denied, a symlink, past the cap).
    pub skipped: Vec<(String, String)>,
}

impl ProjectInstructions {
    /// The paths that were read, in order.
    #[must_use]
    pub fn paths(&self) -> Vec<String> {
        self.files.iter().map(|file| file.path.clone()).collect()
    }
}

/// The `AGENTS.md` of `dir`, if it holds one: exact name first, else the
/// first other spelling by name. `None` when `dir` is reached through a
/// symlink (its listing could be another folder's).
fn agents_file_in(workspace: &Workspace, dir: &WsPath) -> Option<String> {
    if workspace.final_target(dir).ok()? != *dir {
        return None;
    }
    let mut names: Vec<String> = std::fs::read_dir(workspace.absolute(dir))
        .ok()?
        .flatten()
        .filter_map(|entry| entry.file_name().into_string().ok())
        .filter(|name| name.eq_ignore_ascii_case(FILE_NAME))
        .collect();
    names.sort();
    if names.iter().any(|name| name == FILE_NAME) {
        return Some(FILE_NAME.to_owned());
    }
    names.into_iter().next()
}

fn join(dir: &WsPath, name: &str) -> Option<WsPath> {
    let mut parts: Vec<&str> = dir.components().iter().map(String::as_str).collect();
    parts.push(name);
    WsPath::from_relative(std::path::Path::new(&parts.join("/"))).ok()
}

/// The folders whose `AGENTS.md` applies to `mention`, nearest first,
/// the root excluded.
fn folders_of(mention: &WsPath, is_dir: bool) -> Vec<WsPath> {
    let components = mention.components();
    let deepest = if is_dir {
        components.len()
    } else {
        components.len().saturating_sub(1)
    };
    (1..=deepest)
        .rev()
        .filter_map(|end| {
            WsPath::from_relative(std::path::Path::new(&components[..end].join("/"))).ok()
        })
        .collect()
}

/// Read the root `AGENTS.md` and the nested ones of `mentions`
/// (workspace-relative paths, a folder may end with `/`; one that does not
/// resolve is ignored here, the caller checks mentions itself).
#[must_use]
pub fn load(workspace: &Workspace, mentions: &[String]) -> ProjectInstructions {
    let mut folders = vec![WsPath::root()];
    for mention in mentions {
        let trimmed = mention.trim_end_matches('/');
        let Ok(path) = workspace.resolve(trimmed, Intent::Read) else {
            continue;
        };
        let is_dir = matches!(workspace.stat(&path), Ok(Some(EntryKind::Dir)));
        for folder in folders_of(&path, is_dir) {
            if !folders.contains(&folder) {
                folders.push(folder);
            }
        }
    }
    let mut out = ProjectInstructions::default();
    let mut budget = MAX_TOTAL_BYTES;
    for folder in folders {
        let Some(name) = agents_file_in(workspace, &folder) else {
            continue;
        };
        let Some(path) = join(&folder, &name) else {
            continue;
        };
        let shown = path.display_string();
        if let Some(rule) = workspace.denied_by(&path, Intent::Read) {
            out.skipped.push((shown, format!("denied by {rule}")));
            continue;
        }
        match workspace.stat(&path) {
            Ok(Some(EntryKind::File)) => {}
            Ok(Some(EntryKind::Symlink)) => {
                out.skipped.push((shown, "a symlink".to_owned()));
                continue;
            }
            _ => continue,
        }
        if budget == 0 {
            out.skipped.push((shown, "past the size cap".to_owned()));
            continue;
        }
        let Ok((file, resolved)) = workspace.open_file(&path) else {
            continue;
        };
        if resolved != path {
            out.skipped.push((shown, "a symlink".to_owned()));
            continue;
        }
        let mut bytes = Vec::new();
        if file
            .take(u64::try_from(budget).unwrap_or(u64::MAX).saturating_add(1))
            .read_to_end(&mut bytes)
            .is_err()
        {
            continue;
        }
        let truncated = bytes.len() > budget;
        bytes.truncate(budget);
        budget -= bytes.len();
        let mut text = String::from_utf8_lossy(&bytes).into_owned();
        if truncated {
            // A cut may split a character: lossy decoding then ends in U+FFFD.
            let kept = text.trim_end_matches('\u{fffd}').len();
            text.truncate(kept);
        }
        out.files.push(InstructionFile {
            path: shown,
            text,
            truncated,
        });
    }
    out
}

#[cfg(test)]
mod tests {
    use std::fs;

    use super::{MAX_TOTAL_BYTES, load};
    use crate::workspace::Workspace;

    fn open(dir: &tempfile::TempDir, deny: &[&str]) -> Workspace {
        let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
        Workspace::open(dir.path(), &deny).unwrap()
    }

    #[test]
    fn the_root_file_is_found_in_any_case_exact_name_first() {
        for name in ["AGENTS.md", "agents.md", "Agents.md"] {
            let dir = tempfile::tempdir().unwrap();
            fs::write(dir.path().join(name), "Run task test.").unwrap();
            let found = load(&open(&dir, &[]), &[]);
            assert_eq!(found.files.len(), 1, "{name}");
            assert!(found.files[0].path.eq_ignore_ascii_case("AGENTS.md"));
            assert_eq!(found.files[0].text, "Run task test.");
        }
        let none = tempfile::tempdir().unwrap();
        fs::write(none.path().join("AGENTS.txt"), "no").unwrap();
        assert!(load(&open(&none, &[]), &[]).files.is_empty());
    }

    #[test]
    #[cfg(target_os = "linux")]
    fn the_exact_spelling_wins_on_a_case_sensitive_file_system() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("agents.md"), "lower").unwrap();
        fs::write(dir.path().join("AGENTS.md"), "exact").unwrap();
        let found = load(&open(&dir, &[]), &[]);
        assert_eq!(found.paths(), ["AGENTS.md"]);
        assert_eq!(found.files[0].text, "exact");
    }

    #[test]
    fn text_past_the_cap_is_cut_and_marked() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(
            dir.path().join("AGENTS.md"),
            "a".repeat(MAX_TOTAL_BYTES + 10),
        )
        .unwrap();
        fs::create_dir_all(dir.path().join("web")).unwrap();
        fs::write(dir.path().join("web/AGENTS.md"), "web rules").unwrap();
        fs::write(dir.path().join("web/app.ts"), "x").unwrap();
        let found = load(&open(&dir, &[]), &["web/app.ts".into()]);
        assert_eq!(found.files.len(), 1);
        assert!(found.files[0].truncated);
        assert_eq!(found.files[0].text.len(), MAX_TOTAL_BYTES);
        assert_eq!(
            found.skipped,
            [("web/AGENTS.md".to_owned(), "past the size cap".to_owned())]
        );
    }

    #[test]
    fn a_symlink_out_of_the_workspace_is_not_read() {
        let outside = tempfile::tempdir().unwrap();
        fs::write(outside.path().join("AGENTS.md"), "OUTSIDE").unwrap();
        let dir = tempfile::tempdir().unwrap();
        std::os::unix::fs::symlink(
            outside.path().join("AGENTS.md"),
            dir.path().join("AGENTS.md"),
        )
        .unwrap();
        fs::create_dir_all(dir.path().join("src")).unwrap();
        fs::write(dir.path().join("src/a.rs"), "x").unwrap();
        std::os::unix::fs::symlink(outside.path(), dir.path().join("src/vendor")).unwrap();
        let found = load(
            &open(&dir, &[]),
            &["src/vendor/AGENTS.md".into(), "src/vendor/".into()],
        );
        assert!(found.files.is_empty(), "{found:?}");
        assert!(!format!("{found:?}").contains("OUTSIDE"));
        assert_eq!(
            found.skipped,
            [("AGENTS.md".to_owned(), "a symlink".to_owned())]
        );
    }

    #[test]
    fn a_denied_file_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("AGENTS.md"), "secret").unwrap();
        let found = load(&open(&dir, &["AGENTS.md"]), &[]);
        assert!(found.files.is_empty());
        assert_eq!(found.skipped.len(), 1);
        assert!(found.skipped[0].1.starts_with("denied by"), "{found:?}");
    }

    #[test]
    fn nested_files_of_mentions_come_after_the_root_nearest_first_once() {
        let dir = tempfile::tempdir().unwrap();
        fs::create_dir_all(dir.path().join("apps/web/src")).unwrap();
        fs::write(dir.path().join("AGENTS.md"), "root").unwrap();
        fs::write(dir.path().join("apps/AGENTS.md"), "apps").unwrap();
        fs::write(dir.path().join("apps/web/AGENTS.md"), "web").unwrap();
        fs::write(dir.path().join("apps/web/src/main.ts"), "x").unwrap();
        fs::write(dir.path().join("apps/readme.txt"), "x").unwrap();
        let found = load(
            &open(&dir, &[]),
            &[
                "apps/web/src/main.ts".into(),
                "apps/readme.txt".into(),
                "apps/web/".into(),
            ],
        );
        assert_eq!(
            found.paths(),
            ["AGENTS.md", "apps/web/AGENTS.md", "apps/AGENTS.md"]
        );
        assert_eq!(found.files[1].text, "web");
    }
}
