//! Workspaces: folders the person opened for local work (ADR-0029 decision
//! 4), kept in the app data directory as `workspaces.json`.
//!
//! A workspace is its canonical path, a display name and the project it is
//! bound to (a local turn runs in that project, decision 8). Nothing about a
//! workspace is ever written inside the folder itself: per-workspace data
//! (remembered approvals, copy checkpoints, the session's temporary
//! directory) lives under `workspaces/<id>/` next to this file.

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

use crate::error::HostError;

const FILE: &str = "workspaces.json";

/// One workspace, as IPC returns it.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Workspace {
    pub id: String,
    pub path: String,
    pub name: String,
    pub project_id: Option<i64>,
    /// Computed when listed for the UI (`list`, `add`, `bind_project`): the
    /// folder is inside a git work tree. `get` leaves it `false`.
    #[serde(default)]
    pub is_git: bool,
}

pub struct WorkspaceStore {
    dir: PathBuf,
    lock: Mutex<()>,
}

fn io(error: &std::io::Error) -> HostError {
    HostError::Storage(error.to_string())
}

#[cfg(test)]
thread_local! {
    /// How many folders `in_git_tree` probed on this thread (tests only).
    static GIT_PROBES: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
}

/// Stats `.git` in the folder and every ancestor: only for what the UI shows.
fn in_git_tree(path: &Path) -> bool {
    #[cfg(test)]
    GIT_PROBES.with(|probes| probes.set(probes.get() + 1));
    path.ancestors().any(|dir| dir.join(".git").exists())
}

impl WorkspaceStore {
    #[must_use]
    pub fn new(dir: PathBuf) -> Self {
        Self {
            dir,
            lock: Mutex::new(()),
        }
    }

    /// The directory a workspace's own host data lives in.
    #[must_use]
    pub fn data_dir(&self, id: &str) -> PathBuf {
        self.dir.join("workspaces").join(id)
    }

    fn read(&self) -> Result<Vec<Workspace>, HostError> {
        match fs::read_to_string(self.dir.join(FILE)) {
            // A file that does not parse is an error, never "no workspaces":
            // the next write would otherwise erase every workspace in it.
            Ok(text) => serde_json::from_str(&text).map_err(|e| {
                HostError::Storage(format!(
                    "{FILE} in the app data folder is damaged ({e}); it was left as it is, \
                     fix or remove it to manage workspaces again"
                ))
            }),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Vec::new()),
            Err(e) => Err(io(&e)),
        }
    }

    fn write(&self, workspaces: &[Workspace]) -> Result<(), HostError> {
        fs::create_dir_all(&self.dir).map_err(|e| io(&e))?;
        let text = serde_json::to_string_pretty(workspaces)
            .map_err(|e| HostError::Internal(e.to_string()))?;
        let staging = self.dir.join(format!("{FILE}.tmp"));
        fs::write(&staging, text).map_err(|e| io(&e))?;
        fs::rename(&staging, self.dir.join(FILE)).map_err(|e| io(&e))
    }

    fn with<T>(&self, work: impl FnOnce() -> Result<T, HostError>) -> Result<T, HostError> {
        let _guard = self
            .lock
            .lock()
            .map_err(|_| HostError::Internal("workspace store poisoned".into()))?;
        work()
    }

    /// Every workspace, with `is_git` computed now (outside the store's lock).
    ///
    /// # Errors
    ///
    /// The file cannot be read.
    pub fn list(&self) -> Result<Vec<Workspace>, HostError> {
        let all = self.with(|| self.read())?;
        Ok(all
            .into_iter()
            .map(|mut workspace| {
                workspace.is_git = in_git_tree(Path::new(&workspace.path));
                workspace
            })
            .collect())
    }

    /// One workspace, read for the host's own use: `is_git` is NOT computed
    /// (it is `false`), so a lookup never stats the file system.
    ///
    /// # Errors
    ///
    /// The file cannot be read.
    pub fn get(&self, id: &str) -> Result<Option<Workspace>, HostError> {
        self.with(|| Ok(self.read()?.into_iter().find(|w| w.id == id)))
    }

    /// `workspaces.json` (the Doctor checks and moves it).
    #[must_use]
    pub fn file_path(&self) -> PathBuf {
        self.dir.join(FILE)
    }

    /// Every workspace as stored, without `is_git` (the Doctor's view).
    ///
    /// # Errors
    ///
    /// The file cannot be read or does not parse.
    pub fn all(&self) -> Result<Vec<Workspace>, HostError> {
        self.with(|| self.read())
    }

    /// Add a folder (or return the workspace it already is).
    ///
    /// # Errors
    ///
    /// The folder does not exist or is not a directory, or the file cannot
    /// be written.
    pub fn add(&self, folder: &Path) -> Result<Workspace, HostError> {
        let canonical = folder
            .canonicalize()
            .map_err(|_| HostError::Storage("the folder does not exist".into()))?;
        if !canonical.is_dir() {
            return Err(HostError::Storage("not a folder".into()));
        }
        let path = canonical.to_string_lossy().into_owned();
        let workspace = self.with(|| {
            let mut all = self.read()?;
            if let Some(existing) = all.iter().find(|w| w.path == path) {
                return Ok(existing.clone());
            }
            let workspace = Workspace {
                id: uuid::Uuid::new_v4().simple().to_string(),
                name: canonical
                    .file_name()
                    .map_or_else(|| path.clone(), |n| n.to_string_lossy().into_owned()),
                path: path.clone(),
                project_id: None,
                is_git: false,
            };
            all.push(workspace.clone());
            self.write(&all)?;
            Ok(workspace)
        })?;
        Ok(Workspace {
            is_git: in_git_tree(&canonical),
            ..workspace
        })
    }

    /// Forget a workspace (the folder itself is untouched) and its host data.
    ///
    /// # Errors
    ///
    /// The file cannot be written.
    pub fn remove(&self, id: &str) -> Result<(), HostError> {
        self.with(|| {
            let mut all = self.read()?;
            all.retain(|w| w.id != id);
            self.write(&all)
        })?;
        // Ids are ours (32 hex characters); anything else names no directory.
        let data = self.data_dir(id);
        if !id.is_empty() && id.bytes().all(|b| b.is_ascii_alphanumeric()) && data.exists() {
            fs::remove_dir_all(data).map_err(|e| io(&e))?;
        }
        Ok(())
    }

    /// Bind a workspace to a project.
    ///
    /// # Errors
    ///
    /// No such workspace, or the file cannot be written.
    pub fn bind_project(&self, id: &str, project_id: i64) -> Result<Workspace, HostError> {
        let workspace = self.with(|| {
            let mut all = self.read()?;
            let workspace = all
                .iter_mut()
                .find(|w| w.id == id)
                .ok_or_else(|| HostError::Storage("no such workspace".into()))?;
            workspace.project_id = Some(project_id);
            let updated = workspace.clone();
            self.write(&all)?;
            Ok(updated)
        })?;
        Ok(Workspace {
            is_git: in_git_tree(Path::new(&workspace.path)),
            ..workspace
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn workspaces_are_added_once_bound_and_removed() {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        std::fs::create_dir(folder.path().join(".git")).unwrap();
        let store = WorkspaceStore::new(app.path().to_owned());
        assert!(store.list().unwrap().is_empty());
        let added = store.add(folder.path()).unwrap();
        assert!(added.is_git);
        assert_eq!(added.project_id, None);
        assert_eq!(
            added.path,
            folder.path().canonicalize().unwrap().to_string_lossy()
        );
        // The same folder again is the same workspace.
        assert_eq!(store.add(folder.path()).unwrap().id, added.id);
        let bound = store.bind_project(&added.id, 42).unwrap();
        assert_eq!(bound.project_id, Some(42));
        assert_eq!(store.get(&added.id).unwrap().unwrap().project_id, Some(42));
        std::fs::create_dir_all(store.data_dir(&added.id)).unwrap();
        store.remove(&added.id).unwrap();
        assert!(store.list().unwrap().is_empty());
        assert!(!store.data_dir(&added.id).exists());
        assert!(folder.path().exists(), "the folder itself is never touched");
        assert!(store.add(&folder.path().join("missing")).is_err());
        assert!(store.bind_project("nope", 1).is_err());
    }

    #[test]
    fn get_does_not_probe_for_git() {
        let app = tempfile::tempdir().unwrap();
        let store = WorkspaceStore::new(app.path().to_owned());
        let folders: Vec<_> = (0..3).map(|_| tempfile::tempdir().unwrap()).collect();
        let ids: Vec<String> = folders
            .iter()
            .map(|f| store.add(f.path()).unwrap().id)
            .collect();
        let probes = || GIT_PROBES.with(std::cell::Cell::get);
        let before = probes();
        let found = store.get(&ids[1]).unwrap().unwrap();
        assert_eq!(found.id, ids[1]);
        assert_eq!(probes(), before, "a lookup stats no .git anywhere");
        assert_eq!(store.list().unwrap().len(), 3);
        assert_eq!(probes(), before + 3, "the list shown to the UI does");
    }

    #[test]
    fn a_damaged_file_is_an_error_and_is_never_overwritten() {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        let store = WorkspaceStore::new(app.path().to_owned());
        let file = app.path().join(FILE);
        std::fs::write(&file, "[{\"id\": \"abc\", truncated").unwrap();
        let before = std::fs::read_to_string(&file).unwrap();

        assert!(matches!(store.list(), Err(HostError::Storage(_))));
        assert!(store.get("abc").is_err());
        assert!(store.add(folder.path()).is_err());
        assert!(store.bind_project("abc", 1).is_err());
        assert!(store.remove("abc").is_err());
        assert_eq!(std::fs::read_to_string(&file).unwrap(), before);
    }
}
