//! The person's "@" references: the picker's lookup (`workspace_files`)
//! and the `mentions` of `agent_turn_start`.
//!
//! A mention is a workspace-relative path the person picked. The turn adds
//! the checked paths to the user message under a short heading; it never
//! inlines a file, the agent reads what it needs with its own (confined,
//! approved) tools.

use std::path::{Path, PathBuf};

use elitea_local_tools::find::{FoundPath, find_paths};
use elitea_local_tools::workspace::{EntryKind, Intent, Workspace, WsPath};

use super::turn::TurnError;

/// The most mentions one turn takes.
pub const MAX_MENTIONS: usize = 50;
/// The longest mention accepted.
const MAX_MENTION_LEN: usize = 1024;
/// What the picker answers when the UI asks for no particular count.
pub const DEFAULT_FILES_LIMIT: usize = 50;
/// The heading of the section a turn's mentions add to the user message.
pub const MENTIONS_HEADING: &str = "Files the user referenced:";

fn unavailable(message: &str) -> TurnError {
    TurnError::new("workspace_unavailable", message)
}

/// Open the workspace folder with the policy's `path_deny`, the way the
/// turn's session does.
pub fn open(root: &Path, path_deny: &[String]) -> Result<Workspace, TurnError> {
    Workspace::open(root, path_deny).map_err(|e| unavailable(e.message()))
}

/// `workspace_files`: the picker's matches (see [`find_paths`]).
pub fn files(
    workspace: &Workspace,
    query: &str,
    limit: Option<usize>,
) -> Result<Vec<FoundPath>, TurnError> {
    find_paths(workspace, query, limit.unwrap_or(DEFAULT_FILES_LIMIT))
        .map_err(|e| unavailable(e.message()))
}

fn invalid(mention: &str, why: &str) -> TurnError {
    TurnError::new("invalid_request", format!("`{}` {why}", shown(mention)))
}

/// The checked mentions, as the picker spells them (directories end with
/// `/`), duplicates dropped, in the order given.
///
/// # Errors
///
/// `invalid_request` for too many mentions, or one that is empty, leads
/// out of the workspace, is denied by `path_deny`, is a symlink or does
/// not exist.
pub fn check(workspace: &Workspace, mentions: &[String]) -> Result<Vec<String>, TurnError> {
    if mentions.len() > MAX_MENTIONS {
        return Err(TurnError::new(
            "invalid_request",
            format!("A message can reference at most {MAX_MENTIONS} files."),
        ));
    }
    let mut out: Vec<String> = Vec::new();
    for mention in mentions {
        let trimmed = mention.trim().trim_start_matches('@').trim_end_matches('/');
        if trimmed.is_empty() || mention.len() > MAX_MENTION_LEN {
            return Err(invalid(mention, "is not a path in this workspace."));
        }
        let path = workspace
            .resolve(trimmed, Intent::Read)
            .map_err(|_| invalid(mention, "is not a path in this workspace."))?;
        if path.is_root() {
            return Err(invalid(mention, "is the workspace itself."));
        }
        let shown = match workspace.stat(&path) {
            Ok(Some(EntryKind::File)) => path.display_string(),
            Ok(Some(EntryKind::Dir)) => format!("{}/", path.display_string()),
            Ok(Some(_)) => return Err(invalid(mention, "is not a file or a folder.")),
            Ok(None) | Err(_) => return Err(invalid(mention, "does not exist in this workspace.")),
        };
        if !out.contains(&shown) {
            out.push(shown);
        }
    }
    Ok(out)
}

/// What [`locate`] found: the absolute path to hand to the OS, and whether
/// it is a folder.
#[derive(Debug, Eq, PartialEq)]
pub struct Located {
    pub absolute: PathBuf,
    pub is_dir: bool,
}

/// `reveal_path` / `open_path`: a WORKSPACE-RELATIVE path (empty or `.` is
/// the workspace itself) resolved through the confined workspace.
///
/// # Errors
///
/// `invalid_request` for an absolute path, an escape (`..` or an
/// in-workspace symlink leading out), a `path_deny` match, a symlink or
/// anything but a file or folder; `not_found` when it does not exist.
pub fn locate(workspace: &Workspace, path: &str) -> Result<Located, TurnError> {
    let refused = |why: &str| invalid(path, why);
    if path.len() > MAX_MENTION_LEN {
        return Err(refused("is not a path in this workspace."));
    }
    let trimmed = path.trim().trim_end_matches('/');
    if Path::new(trimmed).is_absolute() || trimmed.starts_with('\\') {
        return Err(refused("must be relative to the workspace."));
    }
    let resolved = if trimmed.is_empty() || trimmed == "." {
        WsPath::root()
    } else {
        workspace
            .resolve(trimmed, Intent::Read)
            .map_err(|_| refused("is not a path in this workspace."))?
    };
    let is_dir = match workspace.stat(&resolved) {
        Ok(Some(EntryKind::File)) => false,
        Ok(Some(EntryKind::Dir)) => true,
        Ok(Some(EntryKind::Symlink)) => return Err(refused("is a symbolic link.")),
        Ok(Some(_)) => return Err(refused("is not a file or a folder.")),
        Ok(None) => {
            return Err(TurnError::new(
                "not_found",
                format!("`{}` does not exist in this workspace.", shown(path)),
            ));
        }
        Err(_) => return Err(refused("is not a path in this workspace.")),
    };
    // Where it really is: in-workspace symlinks on the way followed (one
    // leading out is refused), and the deny rules checked again there.
    let target = workspace
        .final_target(&resolved)
        .map_err(|_| refused("is not a path in this workspace."))?;
    workspace
        .check(&target, Intent::Read)
        .map_err(|_| refused("is not a path in this workspace."))?;
    Ok(Located {
        absolute: workspace.absolute(&target),
        is_dir,
    })
}

fn shown(path: &str) -> String {
    path.chars().take(200).collect()
}

/// `prompt` with the mentions section appended (unchanged without any).
#[must_use]
pub fn with_mentions(prompt: &str, mentions: &[String]) -> String {
    if mentions.is_empty() {
        return prompt.to_owned();
    }
    let mut out = prompt.trim_end().to_owned();
    out.push_str("\n\n");
    out.push_str(MENTIONS_HEADING);
    for mention in mentions {
        out.push_str("\n- ");
        out.push_str(mention);
    }
    out
}

#[cfg(test)]
mod tests {
    use std::fs;

    use super::{MAX_MENTIONS, check, locate, open, with_mentions};

    fn folder() -> (tempfile::TempDir, elitea_local_tools::workspace::Workspace) {
        let dir = tempfile::tempdir().unwrap();
        fs::create_dir_all(dir.path().join("src")).unwrap();
        fs::create_dir_all(dir.path().join("secrets")).unwrap();
        fs::write(dir.path().join("src/main.rs"), "fn main() {}").unwrap();
        fs::write(dir.path().join("secrets/key.pem"), "k").unwrap();
        let workspace = open(dir.path(), &["secrets/**".to_owned()]).unwrap();
        (dir, workspace)
    }

    #[test]
    fn paths_inside_the_workspace_pass_spelled_as_the_picker_does() {
        let (dir, workspace) = folder();
        let absolute = dir
            .path()
            .join("src/main.rs")
            .to_string_lossy()
            .into_owned();
        let checked = check(
            &workspace,
            &[
                "src/main.rs".into(),
                "@src/".into(),
                "./src/main.rs".into(),
                absolute,
            ],
        )
        .unwrap();
        assert_eq!(checked, ["src/main.rs", "src/"]);
    }

    #[test]
    fn escapes_denied_missing_and_symlinked_paths_are_refused() {
        let (dir, workspace) = folder();
        let outside = tempfile::tempdir().unwrap();
        fs::write(outside.path().join("x.txt"), "x").unwrap();
        std::os::unix::fs::symlink(outside.path().join("x.txt"), dir.path().join("link.txt"))
            .unwrap();
        for bad in [
            "../etc/passwd",
            "/etc/passwd",
            "secrets/key.pem",
            "missing.rs",
            "link.txt",
            "",
            ".",
        ] {
            let error = check(&workspace, &[bad.to_owned()]).unwrap_err();
            assert_eq!(error.code, "invalid_request", "{bad}");
        }
        let many = vec!["src/main.rs".to_owned(); MAX_MENTIONS + 1];
        assert_eq!(
            check(&workspace, &many).unwrap_err().code,
            "invalid_request"
        );
    }

    #[test]
    fn the_section_lists_paths_and_never_contents() {
        assert_eq!(with_mentions("fix it", &[]), "fix it");
        assert_eq!(
            with_mentions("fix it\n", &["src/main.rs".into(), "src/".into()]),
            "fix it\n\nFiles the user referenced:\n- src/main.rs\n- src/"
        );
    }

    #[test]
    fn located_paths_are_confined_to_the_workspace() {
        let (dir, workspace) = folder();
        let root = dir.path().canonicalize().unwrap();
        let file = locate(&workspace, "src/main.rs").unwrap();
        assert_eq!(file.absolute, root.join("src/main.rs"));
        assert!(!file.is_dir);
        assert!(locate(&workspace, "src/").unwrap().is_dir);
        for itself in ["", ".", "./"] {
            let located = locate(&workspace, itself).unwrap();
            assert!(located.is_dir, "{itself:?}");
            assert_eq!(located.absolute, root, "{itself:?}");
        }
        assert_eq!(
            locate(&workspace, "missing.rs").unwrap_err().code,
            "not_found"
        );
    }

    #[test]
    fn located_paths_refuse_escapes_denials_symlinks_and_absolute_paths() {
        let (dir, workspace) = folder();
        let outside = tempfile::tempdir().unwrap();
        fs::write(outside.path().join("x.txt"), "x").unwrap();
        std::os::unix::fs::symlink(outside.path().join("x.txt"), dir.path().join("out.txt"))
            .unwrap();
        std::os::unix::fs::symlink(outside.path(), dir.path().join("outdir")).unwrap();
        std::os::unix::fs::symlink(dir.path().join("src"), dir.path().join("inner")).unwrap();
        std::os::unix::fs::symlink(dir.path().join("secrets"), dir.path().join("hidden")).unwrap();
        let absolute = dir
            .path()
            .join("src/main.rs")
            .to_string_lossy()
            .into_owned();
        for bad in [
            "../etc/passwd",
            "src/../../x",
            "/etc/passwd",
            absolute.as_str(),
            "secrets/key.pem",
            "out.txt",
            "outdir/x.txt",
            "inner",
            "hidden/key.pem",
        ] {
            assert_eq!(
                locate(&workspace, bad).unwrap_err().code,
                "invalid_request",
                "{bad}"
            );
        }
    }
}
