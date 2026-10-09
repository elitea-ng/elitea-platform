//! The `app://command` channel (IPC.md, "App commands") and the native
//! ways a folder becomes a workspace: File ▸ Open Folder…, a drop on the
//! window, a drop on the dock icon.
//!
//! Every one of them goes through [`WorkspaceStore::add`], the same call
//! `workspace_open` makes, then tells the UI `workspace_opened`.

use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::Serialize;
use serde_json::{Value, json};
use tauri::{AppHandle, Emitter as _, Manager as _};

use crate::local_commands::{LocalState, pick_folder};
use crate::workspaces::WorkspaceStore;

/// The event name the main window listens to for menu and OS actions.
pub const APP_COMMAND_EVENT: &str = "app://command";

/// One `app://command` payload.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct AppCommand {
    pub id: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub args: Option<Value>,
}

pub fn emit(app: &AppHandle, id: &'static str, args: Option<Value>) {
    if let Err(error) = app.emit_to("main", APP_COMMAND_EVENT, AppCommand { id, args }) {
        eprintln!("elitea-desktop: could not deliver an app command: {error}");
    }
}

/// Bring the main window forward (it may be hidden or minimised).
pub fn show_main(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.unminimize();
        let _ = window.show();
        let _ = window.set_focus();
    }
}

/// Paths the OS handed over before the host's state existed (a launch by
/// dropping a folder on the dock icon).
#[derive(Default)]
pub struct PendingOpens(Mutex<Vec<PathBuf>>);

/// Open what arrived during start-up; called once the state exists. The
/// UI may not listen yet: the workspace is in `workspace_list` either way.
pub fn drain_pending(app: &AppHandle) {
    let paths = app
        .try_state::<PendingOpens>()
        .map(|pending| {
            std::mem::take(
                &mut *pending
                    .0
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner),
            )
        })
        .unwrap_or_default();
    if !paths.is_empty() {
        open_paths(app, &paths);
    }
}

/// What a drop or an "open with" carried: folders and everything else.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Sorted {
    pub folders: Vec<PathBuf>,
    pub files: Vec<PathBuf>,
}

/// Folders (a symlink to a folder counts: the workspace is its target)
/// and files; paths that no longer exist are dropped.
#[must_use]
pub fn sort_paths(paths: &[PathBuf]) -> Sorted {
    let mut sorted = Sorted::default();
    for path in paths {
        match std::fs::metadata(path) {
            Ok(meta) if meta.is_dir() => sorted.folders.push(path.clone()),
            Ok(_) => sorted.files.push(path.clone()),
            Err(_) => {}
        }
    }
    sorted
}

/// Open each folder as a workspace; the ids opened, and the first failure's
/// message.
fn open_folders(store: &WorkspaceStore, folders: &[PathBuf]) -> (Vec<String>, Option<String>) {
    let mut opened = Vec::new();
    let mut failure = None;
    for folder in folders {
        match store.add(folder) {
            Ok(workspace) => opened.push(workspace.id),
            Err(error) => {
                failure.get_or_insert_with(|| error.to_string());
            }
        }
    }
    (opened, failure)
}

/// A drop on the window or the dock icon: folders become workspaces
/// (`workspace_opened` each), files are handed to the UI (`files_dropped`).
pub fn open_paths(app: &AppHandle, paths: &[PathBuf]) {
    let Some(local) = app.try_state::<LocalState>() else {
        // Opened before the host finished starting: kept for `drain_pending`.
        if let Some(pending) = app.try_state::<PendingOpens>() {
            pending
                .0
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .extend_from_slice(paths);
        }
        return;
    };
    let sorted = sort_paths(paths);
    let (opened, failure) = open_folders(&local.workspaces, &sorted.folders);
    if !opened.is_empty() {
        show_main(app);
    }
    for workspace_id in opened {
        emit(
            app,
            "workspace_opened",
            Some(json!({ "workspace_id": workspace_id })),
        );
    }
    if let Some(message) = failure {
        emit(
            app,
            "workspace_open_failed",
            Some(json!({ "message": message })),
        );
    }
    if !sorted.files.is_empty() {
        let paths: Vec<String> = sorted
            .files
            .iter()
            .map(|p| p.to_string_lossy().into_owned())
            .collect();
        emit(app, "files_dropped", Some(json!({ "paths": paths })));
    }
}

/// File ▸ Open Folder…: the native picker host-side, then `workspace_opened`.
pub fn open_folder_dialog(app: &AppHandle) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let Some(folder) = pick_folder(&app).await else {
            return;
        };
        open_paths(&app, std::slice::from_ref(&folder));
    });
}

/// `file://` URLs (the dock's "open with") as local paths.
#[cfg_attr(not(target_os = "macos"), allow(dead_code))] // RunEvent::Opened is macOS-only
#[must_use]
pub fn file_urls_to_paths(urls: &[url::Url]) -> Vec<PathBuf> {
    urls.iter()
        .filter(|url| url.scheme() == "file")
        .filter_map(|url| url.to_file_path().ok())
        .filter(|path| Path::new(path).is_absolute())
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_command_payload_is_id_and_optional_args() {
        let bare = AppCommand {
            id: "new_thread",
            args: None,
        };
        assert_eq!(
            serde_json::to_value(bare).unwrap(),
            json!({"id": "new_thread"})
        );
        let opened = AppCommand {
            id: "workspace_opened",
            args: Some(json!({"workspace_id": "abc"})),
        };
        assert_eq!(
            serde_json::to_value(opened).unwrap(),
            json!({"id": "workspace_opened", "args": {"workspace_id": "abc"}})
        );
    }

    #[test]
    fn drops_are_sorted_into_folders_and_files() {
        let dir = tempfile::tempdir().unwrap();
        let folder = dir.path().join("repo");
        std::fs::create_dir(&folder).unwrap();
        let file = dir.path().join("notes.md");
        std::fs::write(&file, "x").unwrap();
        let gone = dir.path().join("gone");
        let sorted = sort_paths(&[folder.clone(), file.clone(), gone]);
        assert_eq!(sorted.folders, std::slice::from_ref(&folder));
        assert_eq!(sorted.files, std::slice::from_ref(&file));

        let app = tempfile::tempdir().unwrap();
        let store = WorkspaceStore::new(app.path().to_owned());
        let (opened, failure) = open_folders(&store, &[folder.clone(), folder]);
        assert_eq!(opened.len(), 2);
        assert_eq!(
            opened[0], opened[1],
            "the same folder is the same workspace"
        );
        assert!(failure.is_none());
        let (opened, failure) = open_folders(&store, &[file]);
        assert!(opened.is_empty());
        assert!(failure.is_some());
    }

    #[test]
    fn only_local_file_urls_become_paths() {
        let urls: Vec<url::Url> = [
            "file:///Users/a/repo",
            "https://example.com/x",
            "file:///tmp/x%20y",
        ]
        .iter()
        .map(|u| url::Url::parse(u).unwrap())
        .collect();
        assert_eq!(
            file_urls_to_paths(&urls),
            [PathBuf::from("/Users/a/repo"), PathBuf::from("/tmp/x y")]
        );
    }
}
