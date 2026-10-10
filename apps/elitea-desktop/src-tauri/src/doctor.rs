//! The Doctor: Help ▸ Run Diagnostics… and Settings ▸ Troubleshoot
//! (IPC.md, "Diagnostics").
//!
//! `doctor_run` checks what the app keeps on this computer and what it needs
//! from the deployment, and names a repair for what it can repair;
//! `doctor_fix` applies one. Nothing is repaired behind the person's back:
//! a file the app will not trust (a symlink, another user's, one others can
//! write, one that does not parse) is never read as data and never silently
//! overwritten — the Doctor offers to MOVE IT ASIDE (into a new directory
//! `<name>.broken-<YYYYmmdd-HHMMSS-mmm>-<random>/` beside it, under its own
//! name; the entry itself, never a symlink's target; never over an earlier
//! copy) so the app can start afresh. The thread history is moved with its
//! `-wal` and `-shm` into one such directory, after its writer stopped and
//! its connections closed, and a fresh history starts at once.
//!
//! A repair that deletes what the app keeps (`workspaces.drop_missing`)
//! names what it deletes in `fix_confirm` and runs only with `confirm`.
//! Repairs that end or change the session go through the app's own paths
//! ([`DoctorHooks`]): a workspace leaves the list the way `workspace_remove`
//! removes it, and moving the stored sign-in aside signs out locally the
//! way `host_sign_out` does.
//!
//! Every repair is logged (paths and modes, never contents).

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use serde::Serialize;
use serde_json::Value;

use crate::auth::AuthService;
use crate::credentials_file::{self, CredentialsFile};
use crate::error::HostError;
use crate::history::{self, HistoryStore};
use elitea_local_index::sqlite_store::{
    FILE_NAME as INDEX_FILE, SCHEMA_VERSION as INDEX_SCHEMA_VERSION,
};

use crate::workspaces::{Workspace, WorkspaceStore};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Status {
    Ok,
    Warn,
    Fail,
}

/// One diagnostic, as `doctor_run` returns it.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Check {
    /// Stable: `credentials`, `dir.config`, `dir.data`, `dir.logs`,
    /// `history`, `workspaces`, `index.<workspace id>` (one per workspace
    /// with a local index), `deployment`, `session`, `local_work`,
    /// `pending_revokes`.
    pub id: String,
    pub title: &'static str,
    pub status: Status,
    /// For a person; never a secret.
    pub message: String,
    /// The repair `doctor_fix` takes, when there is one. A workspace's
    /// index repair names it: `index.<repair>:<workspace id>`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fix_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fix_label: Option<&'static str>,
    /// What the repair deletes, for the person to confirm first; the repair
    /// is refused without `confirm` when this is set.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fix_confirm: Option<String>,
}

/// What the Doctor's repairs need from the rest of the app.
pub trait DoctorHooks: Send + Sync {
    /// Remove one workspace the way `workspace_remove` does (refused while
    /// a turn runs in it); the reason for a person when it is refused.
    ///
    /// # Errors
    ///
    /// The removal was refused or failed.
    fn remove_workspace(&self, workspace_id: &str) -> Result<(), String>;
    /// The session ended on this computer: forget every turn and tell the
    /// webview, as a local sign-out does.
    fn signed_out(&self);
    /// Stop the workspace's local index and close its database (if open),
    /// then run `work` (the move or deletion of its files) while still
    /// holding it: nothing opens the index again until `work` is done.
    fn with_index_closed(&self, workspace_id: &str, work: &mut (dyn FnMut() + Send));
    /// Turn the workspace's index on again and build it from nothing, after
    /// its damaged files were deleted; why not, for a person.
    ///
    /// # Errors
    ///
    /// The index could not be turned on (the policy turns it off, …).
    fn rebuild_index(&self, workspace_id: &str) -> Result<(), String>;
}

/// The repairs that delete what the app keeps: refused without `confirm`.
/// A workspace's index repair is matched without its `:<workspace id>`.
const CONFIRMED_FIXES: &[&str] = &["workspaces.drop_missing", "index.rebuild"];

impl Check {
    fn new(id: impl Into<String>, title: &'static str, status: Status, message: String) -> Self {
        Self {
            id: id.into(),
            title,
            status,
            message,
            fix_id: None,
            fix_label: None,
            fix_confirm: None,
        }
    }

    fn fix(mut self, fix_id: impl Into<String>, label: &'static str) -> Self {
        self.fix_id = Some(fix_id.into());
        self.fix_label = Some(label);
        self
    }

    fn confirming(mut self, what: String) -> Self {
        self.fix_confirm = Some(what);
        self
    }
}

/// Whether a workspace's folder can be used now, and if not, why.
#[derive(Debug, PartialEq, Eq)]
enum Reach {
    Reachable,
    /// Gone: its parent folder is there, the folder is not (and it is not
    /// on a drive that is not connected). The only case the Doctor offers
    /// to remove from the list.
    Missing,
    /// The OS refuses the app access (macOS privacy settings, permissions).
    NoAccess,
    /// Not reachable now, and maybe again later: why.
    Unavailable(&'static str),
}

/// `/Volumes/<name>/…` whose `/Volumes/<name>` is not there: a drive that
/// is not connected (macOS mounts every other drive there).
fn on_unmounted_volume(path: &Path) -> bool {
    let mut components = path.components();
    let (Some(std::path::Component::RootDir), Some(volumes), Some(name)) =
        (components.next(), components.next(), components.next())
    else {
        return false;
    };
    volumes.as_os_str() == "Volumes"
        && fs::symlink_metadata(Path::new("/Volumes").join(name.as_os_str())).is_err()
}

fn reach(path: &Path) -> Reach {
    let error = match fs::read_dir(path) {
        Ok(_) => return Reach::Reachable,
        Err(error) => error,
    };
    // EPERM is what macOS's privacy protection answers; EACCES the mode.
    if error.kind() == std::io::ErrorKind::PermissionDenied || error.raw_os_error() == Some(1) {
        return Reach::NoAccess;
    }
    if on_unmounted_volume(path) {
        return Reach::Unavailable("drive not connected");
    }
    if error.kind() != std::io::ErrorKind::NotFound {
        return Reach::Unavailable("cannot be read now");
    }
    match path.parent().map(fs::metadata) {
        Some(Ok(parent)) if parent.is_dir() => Reach::Missing,
        _ => Reach::Unavailable("the folder above it is not there either"),
    }
}

/// How to let the app read a folder the OS keeps from it.
fn grant_access_hint() -> &'static str {
    if cfg!(target_os = "macos") {
        "Allow Elitea in System Settings › Privacy & Security › Files and Folders (or Full Disk Access), then run the checks again."
    } else {
        "Check that your user may read them, then run the checks again."
    }
}

/// What a file or directory looks like to the app's trust rules.
#[derive(Debug, PartialEq, Eq)]
enum Health {
    Missing,
    Healthy,
    /// Ours, but group or others can read it (they cannot write it).
    Loose(u32),
    /// Not to be read as data: why.
    Untrusted(&'static str),
}

fn health(path: &Path, want_dir: bool) -> Health {
    let meta = match fs::symlink_metadata(path) {
        Ok(meta) => meta,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Health::Missing,
        Err(_) => return Health::Untrusted("cannot be inspected"),
    };
    if meta.file_type().is_symlink() {
        return Health::Untrusted("is a symbolic link");
    }
    if want_dir && !meta.is_dir() {
        return Health::Untrusted("is not a folder");
    }
    if !want_dir && !meta.is_file() {
        return Health::Untrusted("is not a regular file");
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt as _;
        if meta.uid() != rustix::process::geteuid().as_raw() {
            return Health::Untrusted("belongs to another user");
        }
        if meta.mode() & 0o022 != 0 {
            return Health::Untrusted("is writable by other users");
        }
        if meta.mode() & 0o077 != 0 {
            return Health::Loose(meta.mode() & 0o777);
        }
    }
    Health::Healthy
}

/// Narrow a file of ours to `0600`, through a no-follow descriptor.
fn tighten_file(path: &Path) -> Result<(), HostError> {
    let file = credentials_file::open_no_follow(path)
        .map_err(|e| HostError::Storage(format!("could not open {}: {e}", path.display())))?;
    tighten_open(path, &file, false)
}

/// Narrow a folder of ours to `0700`, through a no-follow descriptor.
fn tighten_dir(path: &Path) -> Result<(), HostError> {
    let dir = credentials_file::open_no_follow(path)
        .map_err(|e| HostError::Storage(format!("could not open {}: {e}", path.display())))?;
    tighten_open(path, &dir, true)
}

fn tighten_open(path: &Path, file: &fs::File, is_dir: bool) -> Result<(), HostError> {
    let meta = file
        .metadata()
        .map_err(|e| HostError::Storage(e.to_string()))?;
    if meta.is_dir() != is_dir {
        return Err(HostError::Storage(format!(
            "{} is not what it should be; move it aside instead",
            path.display()
        )));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::{MetadataExt as _, PermissionsExt as _};
        if meta.uid() != rustix::process::geteuid().as_raw() {
            return Err(HostError::Storage(format!(
                "{} belongs to another user; it is not changed",
                path.display()
            )));
        }
        let mode = if is_dir { 0o700 } else { 0o600 };
        file.set_permissions(fs::Permissions::from_mode(mode))
            .map_err(|e| HostError::Storage(e.to_string()))?;
        log::warn!(
            "diagnostics: restricted {} from {:o} to {mode:o}",
            path.display(),
            meta.mode() & 0o777
        );
    }
    Ok(())
}

/// Move `path` (the entry itself: a symlink is moved, never followed) into
/// a new directory `<name>.broken-<time>-<random>/` next to it, under its
/// own name; the directory. `None` when there is nothing to move.
fn move_aside(path: &Path) -> Result<Option<PathBuf>, HostError> {
    if fs::symlink_metadata(path).is_err() {
        return Ok(None);
    }
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    let aside = history::move_files_aside(parent, &name, &[path.to_owned()])?;
    log::warn!(
        "diagnostics: moved {} aside to {}",
        path.display(),
        aside.display()
    );
    Ok(Some(aside))
}

/// The checks and repairs on this computer's files (no network).
pub struct LocalDoctor {
    /// The app's own paths for what a repair ends or removes.
    pub hooks: Arc<dyn DoctorHooks>,
    pub config_dir: PathBuf,
    pub data_dir: PathBuf,
    pub log_dir: Option<PathBuf>,
    pub credentials: Arc<CredentialsFile>,
    pub workspaces: Arc<WorkspaceStore>,
    /// The thread history opened at launch; `None` when it was refused.
    pub history: Option<Arc<HistoryStore>>,
}

const MOVE_ASIDE: &str = "Move it aside";
const RESTRICT: &str = "Restrict to you";

impl LocalDoctor {
    fn credentials_path(&self) -> PathBuf {
        self.credentials.path()
    }

    fn history_path(&self) -> PathBuf {
        self.data_dir.join(history::FILE_NAME)
    }

    fn workspaces_path(&self) -> PathBuf {
        self.workspaces.file_path()
    }

    #[must_use]
    pub fn checks(&self) -> Vec<Check> {
        let mut checks = vec![self.credentials_check()];
        checks.push(dir_check(
            "dir.config",
            "Settings folder",
            &self.config_dir,
            "dir.tighten.config",
        ));
        checks.push(dir_check(
            "dir.data",
            "Data folder",
            &self.data_dir,
            "dir.tighten.data",
        ));
        if let Some(logs) = &self.log_dir {
            checks.push(dir_check(
                "dir.logs",
                "Logs folder",
                logs,
                "dir.tighten.logs",
            ));
        }
        checks.push(self.history_check());
        checks.push(self.workspaces_check());
        checks.extend(self.index_checks());
        checks
    }

    fn credentials_check(&self) -> Check {
        const ID: &str = "credentials";
        const TITLE: &str = "Stored sign-in";
        let path = self.credentials_path();
        match health(&path, false) {
            Health::Missing => Check::new(
                ID,
                TITLE,
                Status::Ok,
                "No sign-in is stored on this computer yet.".into(),
            ),
            Health::Untrusted(why) => Check::new(
                ID,
                TITLE,
                Status::Fail,
                format!(
                    "{} {why}, so the app will not read it or sign in over it. Moving it aside lets you sign in again.",
                    path.display()
                ),
            )
            .fix("credentials.move_aside", MOVE_ASIDE),
            Health::Loose(mode) => Check::new(
                ID,
                TITLE,
                Status::Warn,
                format!(
                    "{} can be read by other users (mode {mode:o}); it should be 0600.",
                    path.display()
                ),
            )
            .fix("credentials.tighten", RESTRICT),
            Health::Healthy => {
                let parses = credentials_file::open_no_follow(&path)
                    .and_then(std::io::read_to_string)
                    .ok()
                    .is_some_and(|text| {
                        serde_json::from_str::<BTreeMap<String, String>>(&text).is_ok()
                    });
                if parses {
                    Check::new(
                        ID,
                        TITLE,
                        Status::Ok,
                        "Owner-only and readable.".into(),
                    )
                } else {
                    Check::new(
                        ID,
                        TITLE,
                        Status::Fail,
                        format!(
                            "{} is damaged (it does not parse). Moving it aside lets you sign in again.",
                            path.display()
                        ),
                    )
                    .fix("credentials.move_aside", MOVE_ASIDE)
                }
            }
        }
    }

    fn history_check(&self) -> Check {
        const ID: &str = "history";
        const TITLE: &str = "Thread history";
        let path = self.history_path();
        let sidecars_ok = ["-wal", "-shm"].iter().all(|suffix| {
            let sidecar = path.with_file_name(format!("{}{suffix}", history::FILE_NAME));
            matches!(health(&sidecar, false), Health::Missing | Health::Healthy)
        });
        match health(&path, false) {
            Health::Missing => {
                return Check::new(
                    ID,
                    TITLE,
                    Status::Ok,
                    "No history yet; it starts with the first local turn.".into(),
                );
            }
            Health::Untrusted(why) => {
                return Check::new(
                    ID,
                    TITLE,
                    Status::Fail,
                    format!(
                        "{} {why}, so the app will not use it. Move it aside to start a new history.",
                        path.display()
                    ),
                )
                .fix("history.move_aside", MOVE_ASIDE);
            }
            Health::Loose(mode) => {
                return Check::new(
                    ID,
                    TITLE,
                    Status::Warn,
                    format!(
                        "{} can be read by other users (mode {mode:o}); it should be 0600.",
                        path.display()
                    ),
                )
                .fix("history.tighten", RESTRICT);
            }
            Health::Healthy if !sidecars_ok => {
                return Check::new(
                    ID,
                    TITLE,
                    Status::Warn,
                    "The history's journal files are readable by other users.".into(),
                )
                .fix("history.tighten", RESTRICT);
            }
            Health::Healthy => {}
        }
        match history::integrity(&path) {
            Ok(version) if version > history::SCHEMA_VERSION => Check::new(
                ID,
                TITLE,
                Status::Fail,
                format!(
                    "Written by a newer version of Elitea (schema {version}); update the app."
                ),
            ),
            Ok(_) if self.history.is_none() => Check::new(
                ID,
                TITLE,
                Status::Warn,
                "The history looks fine now but did not open at launch; restart Elitea.".into(),
            ),
            Ok(_) => Check::new(ID, TITLE, Status::Ok, "Opens and passes its integrity check.".into()),
            Err(error) => Check::new(
                ID,
                TITLE,
                Status::Fail,
                format!(
                    "The history is damaged ({error}). Move it aside to start a new one; the threads on the server are not affected."
                ),
            )
            .fix("history.move_aside", MOVE_ASIDE),
        }
    }

    fn workspaces_check(&self) -> Check {
        const ID: &str = "workspaces";
        const TITLE: &str = "Workspaces";
        let all = match self.workspaces.all() {
            Ok(all) => all,
            Err(error) => {
                return Check::new(ID, TITLE, Status::Fail, error.to_string())
                    .fix("workspaces.move_aside", MOVE_ASIDE);
            }
        };
        let (mut missing, mut no_access, mut unavailable) = (Vec::new(), Vec::new(), Vec::new());
        for workspace in &all {
            match reach(Path::new(&workspace.path)) {
                Reach::Reachable => {}
                Reach::Missing => missing.push(workspace.name.clone()),
                Reach::NoAccess => no_access.push(workspace.name.clone()),
                Reach::Unavailable(why) => unavailable.push(format!("{} ({why})", workspace.name)),
            }
        }
        if missing.is_empty() && no_access.is_empty() && unavailable.is_empty() {
            return Check::new(
                ID,
                TITLE,
                Status::Ok,
                format!("{} folder(s), all reachable.", all.len()),
            );
        }
        let mut parts = Vec::new();
        if !missing.is_empty() {
            parts.push(format!(
                "These folders no longer exist: {}.",
                missing.join(", ")
            ));
        }
        if !no_access.is_empty() {
            parts.push(format!(
                "Elitea is not allowed to read: {}. {}",
                no_access.join(", "),
                grant_access_hint()
            ));
        }
        if !unavailable.is_empty() {
            parts.push(format!(
                "Unavailable now: {}. They stay in the list; connect the drive and run the checks again.",
                unavailable.join(", ")
            ));
        }
        let check = Check::new(ID, TITLE, Status::Warn, parts.join(" "));
        if missing.is_empty() {
            return check;
        }
        check
            .fix("workspaces.drop_missing", "Remove them from the list")
            .confirming(format!(
                "Remove {} from the list? What Elitea keeps for them on this computer is deleted: \
                 remembered approvals, undo checkpoints and their thread history here. \
                 The folders themselves are not touched.",
                missing.join(", ")
            ))
    }

    /// The workspaces whose folder is gone (see [`Reach::Missing`]).
    fn missing_workspaces(&self) -> Result<Vec<Workspace>, HostError> {
        Ok(self
            .workspaces
            .all()?
            .into_iter()
            .filter(|w| reach(Path::new(&w.path)) == Reach::Missing)
            .collect())
    }

    /// Apply one repair; what happened, for a person. `confirm`: the
    /// person confirmed what the repair's `fix_confirm` said it deletes.
    ///
    /// # Errors
    ///
    /// An unknown repair, an unconfirmed one that deletes data, or the
    /// repair failed.
    pub fn fix(&self, fix_id: &str, confirm: bool) -> Result<String, HostError> {
        let (repair, workspace_id) = fix_id
            .split_once(':')
            .map_or((fix_id, None), |(repair, id)| (repair, Some(id)));
        if CONFIRMED_FIXES.contains(&repair) && !confirm {
            return Err(HostError::Unsupported(
                "this repair deletes what Elitea keeps for these folders; confirm it first".into(),
            ));
        }
        if let Some(workspace_id) = workspace_id {
            return self.fix_index(repair, workspace_id);
        }
        match fix_id {
            "credentials.tighten" => {
                tighten_file(&self.credentials_path())?;
                self.credentials.forget_cache();
                Ok("The stored sign-in is now readable by you only.".into())
            }
            "dir.tighten.config" => {
                tighten_dir(&self.config_dir).map(|()| "Restricted to you.".into())
            }
            "dir.tighten.data" => tighten_dir(&self.data_dir).map(|()| "Restricted to you.".into()),
            "dir.tighten.logs" => match &self.log_dir {
                Some(logs) => tighten_dir(logs).map(|()| "Restricted to you.".into()),
                None => Err(HostError::Storage("there is no logs folder".into())),
            },
            "history.tighten" => {
                let path = self.history_path();
                tighten_file(&path)?;
                for suffix in ["-wal", "-shm"] {
                    let sidecar = path.with_file_name(format!("{}{suffix}", history::FILE_NAME));
                    if fs::symlink_metadata(&sidecar).is_ok() {
                        tighten_file(&sidecar)?;
                    }
                }
                Ok("The history is now readable by you only.".into())
            }
            "history.move_aside" => self.move_history_aside(),
            "workspaces.drop_missing" => {
                let (mut removed, mut kept) = (0, Vec::new());
                for workspace in self.missing_workspaces()? {
                    match self.hooks.remove_workspace(&workspace.id) {
                        Ok(()) => removed += 1,
                        Err(why) => kept.push(format!("{} ({why})", workspace.name)),
                    }
                }
                log::info!(
                    "diagnostics: removed {removed} missing workspace(s) from the list, {} refused",
                    kept.len()
                );
                let mut message = format!("Removed {removed} folder(s) from the list.");
                if !kept.is_empty() {
                    message.push_str(&format!(" Not removed: {}.", kept.join(", ")));
                }
                Ok(message)
            }
            "workspaces.move_aside" => {
                let aside = move_aside(&self.workspaces_path())?;
                Ok(match aside {
                    Some(aside) => {
                        format!("Moved to {}. Open your folders again.", aside.display())
                    }
                    None => "There was nothing to move.".into(),
                })
            }
            _ => Err(HostError::Internal(format!("unknown repair `{fix_id}`"))),
        }
    }
}

impl LocalDoctor {
    /// Move the stored sign-in aside and read the file again on next use;
    /// where it went. The session ends with it: [`Doctor::fix`] signs out.
    fn move_credentials_aside(&self) -> Result<Option<PathBuf>, HostError> {
        let aside = move_aside(&self.credentials_path())?;
        self.credentials.forget_cache();
        Ok(aside)
    }

    /// `history.move_aside`: the open store closes, moves its three files
    /// together and starts afresh; without one (refused at launch) the
    /// files are moved the same way and the next launch starts afresh.
    fn move_history_aside(&self) -> Result<String, HostError> {
        if let Some(store) = &self.history {
            let aside = store.move_aside()?;
            return Ok(format!(
                "Moved to {}. A new, empty history has started; the threads on the server are not affected.",
                aside.display()
            ));
        }
        let files = history::history_files(&self.data_dir);
        if files.iter().all(|file| fs::symlink_metadata(file).is_err()) {
            return Ok("There was nothing to move.".into());
        }
        let aside = history::move_files_aside(&self.data_dir, history::FILE_NAME, &files)?;
        log::warn!(
            "diagnostics: moved the thread history aside to {}",
            aside.display()
        );
        Ok(format!(
            "Moved to {}. Restart Elitea to start a new history.",
            aside.display()
        ))
    }
}

/// A workspace's local index files: the database and its journals.
fn index_files(dir: &Path) -> [PathBuf; 3] {
    [
        dir.join(INDEX_FILE),
        dir.join(format!("{INDEX_FILE}-wal")),
        dir.join(format!("{INDEX_FILE}-shm")),
    ]
}

/// `PRAGMA quick_check`, then the schema version, read-only and without
/// following a symlinked file; why it is damaged when it is.
fn index_integrity(path: &Path) -> Result<i64, String> {
    // The folder canonical, so no-follow refuses a symlinked FILE only.
    let path = match (path.parent(), path.file_name()) {
        (Some(dir), Some(name)) => fs::canonicalize(dir).map_err(|e| e.to_string())?.join(name),
        _ => path.to_owned(),
    };
    let conn = rusqlite::Connection::open_with_flags(
        &path,
        rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY | rusqlite::OpenFlags::SQLITE_OPEN_NOFOLLOW,
    )
    .map_err(|e| e.to_string())?;
    let verdict: String = conn
        .query_row("PRAGMA quick_check", [], |row| row.get(0))
        .map_err(|e| e.to_string())?;
    if verdict != "ok" {
        return Err(verdict);
    }
    conn.query_row("PRAGMA user_version", [], |row| row.get(0))
        .map_err(|e| e.to_string())
}

/// The workspaces' local indexes (IPC.md, "Local index"): one check per
/// workspace that has one on this computer, and their repairs.
impl LocalDoctor {
    fn index_dir(&self, workspace_id: &str) -> PathBuf {
        self.workspaces.data_dir(workspace_id).join("index")
    }

    fn index_checks(&self) -> Vec<Check> {
        // An unreadable list is the workspaces check's to report.
        let Ok(all) = self.workspaces.all() else {
            return Vec::new();
        };
        all.iter()
            .filter(|workspace| fs::symlink_metadata(self.index_dir(&workspace.id)).is_ok())
            .map(|workspace| self.index_check(workspace))
            .collect()
    }

    fn index_check(&self, workspace: &Workspace) -> Check {
        const TITLE: &str = "Code index";
        let id = format!("index.{}", workspace.id);
        let name = &workspace.name;
        let fix = |repair: &str| format!("index.{repair}:{}", workspace.id);
        let dir = self.index_dir(&workspace.id);
        let [file, wal, shm] = index_files(&dir);
        let untrusted = |path: &Path, why: &str| {
            Check::new(
                id.clone(),
                TITLE,
                Status::Fail,
                format!(
                    "{name}: {} {why}, so the app will not use it. Move it aside; the index can be turned on again.",
                    path.display()
                ),
            )
            .fix(fix("move_aside"), MOVE_ASIDE)
        };
        let loose = |path: &Path, mode: u32| {
            Check::new(
                id.clone(),
                TITLE,
                Status::Warn,
                format!(
                    "{name}: {} can be read by other users (mode {mode:o}); it should be owner-only.",
                    path.display()
                ),
            )
            .fix(fix("tighten"), RESTRICT)
        };
        match health(&dir, true) {
            Health::Missing => {}
            Health::Untrusted(why) => return untrusted(&dir, why),
            Health::Loose(mode) => return loose(&dir, mode),
            Health::Healthy => {}
        }
        match health(&file, false) {
            Health::Missing => {
                return Check::new(
                    id,
                    TITLE,
                    Status::Ok,
                    format!("{name}: not turned on (nothing is built)."),
                );
            }
            Health::Untrusted(why) => return untrusted(&file, why),
            Health::Loose(mode) => return loose(&file, mode),
            Health::Healthy => {}
        }
        for sidecar in [&wal, &shm] {
            match health(sidecar, false) {
                Health::Missing | Health::Healthy => {}
                Health::Untrusted(why) => return untrusted(sidecar, why),
                Health::Loose(mode) => return loose(sidecar, mode),
            }
        }
        match index_integrity(&file) {
            Ok(version) if version > INDEX_SCHEMA_VERSION => Check::new(
                id,
                TITLE,
                Status::Fail,
                format!(
                    "{name}: written by a newer version of Elitea (schema {version}); update the app."
                ),
            ),
            Ok(_) => Check::new(
                id,
                TITLE,
                Status::Ok,
                format!("{name}: owner-only, opens and passes its integrity check."),
            ),
            Err(error) => Check::new(
                id,
                TITLE,
                Status::Fail,
                format!("{name}: the index is damaged ({error}). Rebuild it from the folder."),
            )
            .fix(fix("rebuild"), "Rebuild")
            .confirming(format!(
                "Delete the code index of {name} and build it again from the folder? \
                 It is parsed again on this computer and costs nothing; nothing is sent \
                 anywhere (it has no embeddings). The folder itself is not touched."
            )),
        }
    }

    /// `index.<repair>:<workspace id>`, for a workspace in the list only
    /// (the id names a folder under the app's data).
    fn fix_index(&self, repair: &str, workspace_id: &str) -> Result<String, HostError> {
        let Some(workspace) = self.workspaces.get(workspace_id)? else {
            return Err(HostError::Internal(format!(
                "unknown repair `{repair}:{workspace_id}`"
            )));
        };
        let dir = self.index_dir(&workspace.id);
        match repair {
            "index.tighten" => {
                tighten_dir(&dir)?;
                for file in index_files(&dir) {
                    if fs::symlink_metadata(&file).is_ok() {
                        tighten_file(&file)?;
                    }
                }
                Ok(format!(
                    "The code index of {} is now readable by you only.",
                    workspace.name
                ))
            }
            "index.move_aside" => {
                let mut moved = None;
                self.hooks
                    .with_index_closed(&workspace.id, &mut || moved = Some(move_aside(&dir)));
                Ok(match moved.unwrap_or(Ok(None))? {
                    Some(aside) => format!(
                        "Moved to {}. Turn the index on again to build a new one.",
                        aside.display()
                    ),
                    None => "There was nothing to move.".into(),
                })
            }
            "index.rebuild" => {
                // Deleted only when the app trusts it; anything else is
                // moved aside instead.
                if matches!(health(&dir, true), Health::Untrusted(_)) {
                    return Err(HostError::Storage(format!(
                        "{} is not the app's own folder; move it aside instead",
                        dir.display()
                    )));
                }
                let mut deleted = None;
                self.hooks.with_index_closed(&workspace.id, &mut || {
                    deleted = Some(match fs::remove_dir_all(&dir) {
                        Ok(()) => Ok(()),
                        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
                        Err(error) => Err(HostError::Storage(format!(
                            "could not delete {}: {error}",
                            dir.display()
                        ))),
                    });
                });
                deleted.unwrap_or(Ok(()))?;
                log::warn!("diagnostics: deleted the damaged index {}", dir.display());
                Ok(match self.hooks.rebuild_index(&workspace.id) {
                    Ok(()) => format!(
                        "Deleted the damaged index of {}; a new one is being built.",
                        workspace.name
                    ),
                    Err(why) => format!(
                        "Deleted the damaged index of {}. It was not built again ({why}).",
                        workspace.name
                    ),
                })
            }
            _ => Err(HostError::Internal(format!(
                "unknown repair `{repair}:{workspace_id}`"
            ))),
        }
    }
}

fn dir_check(id: &'static str, title: &'static str, path: &Path, fix_id: &'static str) -> Check {
    match health(path, true) {
        Health::Missing => Check::new(
            id,
            title,
            Status::Ok,
            format!("{} is created when first needed.", path.display()),
        ),
        Health::Untrusted(why) => Check::new(
            id,
            title,
            Status::Fail,
            format!(
                "{} {why}. The app will not use it; fix or remove it by hand.",
                path.display()
            ),
        ),
        Health::Loose(mode) => Check::new(
            id,
            title,
            Status::Warn,
            format!(
                "{} can be read by other users (mode {mode:o}); it should be 0700.",
                path.display()
            ),
        )
        .fix(fix_id, RESTRICT),
        Health::Healthy if !writable(path) => Check::new(
            id,
            title,
            Status::Fail,
            format!("{} is not writable.", path.display()),
        ),
        Health::Healthy => Check::new(
            id,
            title,
            Status::Ok,
            format!("{} is yours and private.", path.display()),
        ),
    }
}

fn writable(path: &Path) -> bool {
    #[cfg(unix)]
    {
        rustix::fs::access(path, rustix::fs::Access::WRITE_OK).is_ok()
    }
    #[cfg(not(unix))]
    {
        fs::metadata(path).is_ok_and(|m| !m.permissions().readonly())
    }
}

/// The deployment, the session and the policy (network).
pub async fn remote_checks(auth: &AuthService) -> Vec<Check> {
    let mut checks = Vec::new();
    let state = auth.state();
    let origin = state.as_ref().ok().and_then(|s| s.origin.clone());
    checks.push(match &origin {
        None => Check::new(
            "deployment",
            "Deployment",
            Status::Warn,
            "Not connected to a deployment yet.".into(),
        ),
        Some(origin) => match auth.probe_deployment(origin).await {
            Ok(name) => Check::new(
                "deployment",
                "Deployment",
                Status::Ok,
                format!("{name} ({origin}) answers and its discovery document is valid."),
            ),
            Err(error) => Check::new(
                "deployment",
                "Deployment",
                Status::Warn,
                format!("{origin}: {error}"),
            ),
        },
    });
    let signed_in = state.as_ref().is_ok_and(|s| s.signed_in);
    checks.push(match &state {
        Err(error) => Check::new("session", "Session", Status::Fail, error.to_string()),
        Ok(_) if !signed_in => {
            Check::new("session", "Session", Status::Warn, "Not signed in.".into())
        }
        Ok(_) => match auth.access_token().await {
            Ok(Some(_)) => Check::new(
                "session",
                "Session",
                Status::Ok,
                "Signed in; the session refreshes.".into(),
            ),
            Ok(None) => Check::new(
                "session",
                "Session",
                Status::Fail,
                "The deployment ended this session; sign in again.".into(),
            ),
            Err(error) => Check::new("session", "Session", Status::Warn, error.to_string()),
        },
    });
    let policy = state.as_ref().ok().and_then(|s| s.policy.clone());
    checks.push(local_work_check(signed_in, policy.as_ref()));
    let waiting = auth.pending_revoke_count();
    checks.push(if waiting == 0 {
        Check::new(
            "pending_revokes",
            "Sign-outs to deliver",
            Status::Ok,
            "Every sign-out reached the server.".into(),
        )
    } else {
        Check::new(
            "pending_revokes",
            "Sign-outs to deliver",
            Status::Warn,
            format!("{waiting} earlier sign-out(s) have not reached the server yet."),
        )
        .fix("revokes.retry", "Send them now")
    });
    checks
}

fn local_work_check(signed_in: bool, policy: Option<&Value>) -> Check {
    const ID: &str = "local_work";
    const TITLE: &str = "Local work";
    if !signed_in {
        return Check::new(ID, TITLE, Status::Ok, "Known after sign-in.".into());
    }
    let allowed = policy
        .and_then(|p| p.pointer("/local_work/allowed"))
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if allowed {
        Check::new(
            ID,
            TITLE,
            Status::Ok,
            "Your organisation's policy allows local work.".into(),
        )
    } else {
        Check::new(
            ID,
            TITLE,
            Status::Warn,
            "Local work is turned off by your organisation's policy. An administrator turns it on in Admin › Configuration › Native client policy (Local work allowed); this computer picks it up at the next sign-in or session refresh.".into(),
        )
    }
}

/// The whole Doctor, as the app holds it.
pub struct Doctor {
    pub local: LocalDoctor,
    pub auth: Arc<AuthService>,
}

impl Doctor {
    /// `scope`: `local` checks this computer's files only (no network);
    /// anything else adds the deployment, the session and the policy.
    pub async fn run(&self, scope: Option<&str>) -> Vec<Check> {
        let mut checks = self.local.checks();
        if scope != Some("local") {
            checks.extend(remote_checks(&self.auth).await);
        }
        checks
    }

    /// # Errors
    ///
    /// An unknown repair, an unconfirmed one that deletes data, or the
    /// repair failed.
    pub async fn fix(self: &Arc<Self>, fix_id: &str, confirm: bool) -> Result<String, HostError> {
        if fix_id == "credentials.move_aside" {
            return self.move_credentials_aside().await;
        }
        if fix_id == "revokes.retry" {
            let waiting = self.auth.retry_pending_revokes().await;
            log::info!("diagnostics: retried the pending revokes, {waiting} still waiting");
            return Ok(if waiting == 0 {
                "Delivered.".into()
            } else {
                format!(
                    "{waiting} still could not be delivered; they are retried at the next launch."
                )
            });
        }
        // File repairs, and a workspace removal that waits for the history's
        // writer: off the async workers.
        let doctor = self.clone();
        let fix_id = fix_id.to_owned();
        tokio::task::spawn_blocking(move || doctor.local.fix(&fix_id, confirm))
            .await
            .map_err(|e| HostError::Internal(format!("the repair stopped: {e}")))?
    }

    /// `credentials.move_aside`: the file moves aside, then the local
    /// sign-out path runs as `host_sign_out` runs it — the cached token and
    /// any unsaved rotated session are forgotten, the session epoch moves
    /// on, every turn is forgotten and the webview is told. The moved file
    /// is not trusted, so it is never read: its session is not revoked.
    async fn move_credentials_aside(&self) -> Result<String, HostError> {
        let aside = self.local.move_credentials_aside()?;
        let Some(aside) = aside else {
            return Ok("There was nothing to move.".into());
        };
        if let Err(error) = self.auth.wipe().await {
            log::warn!("diagnostics: the local sign-out after moving the sign-in aside: {error}");
        }
        self.local.hooks.signed_out();
        log::warn!("diagnostics: signed out locally after moving the stored sign-in aside");
        Ok(format!(
            "Moved to {}. You are signed out on this computer; sign in again to continue. \
             The session in the moved file was not ended on the server (Elitea does not read a \
             file it does not trust): end it from Settings › Devices on the web if you need to.",
            aside.display()
        ))
    }
}

/// `doctor_run`: every check, `scope: "local"` for this computer's files only.
#[tauri::command]
pub async fn doctor_run(
    doctor: tauri::State<'_, Arc<Doctor>>,
    scope: Option<String>,
) -> Result<Vec<Check>, HostError> {
    Ok(doctor.run(scope.as_deref()).await)
}

/// What `doctor_fix` did.
#[derive(Clone, Debug, Serialize)]
pub struct FixOutcome {
    pub message: String,
}

/// `doctor_fix`: apply the repair a check named; `confirm` once the person
/// confirmed what a repair with `fix_confirm` deletes.
#[tauri::command(rename_all = "snake_case")]
pub async fn doctor_fix(
    doctor: tauri::State<'_, Arc<Doctor>>,
    fix_id: String,
    confirm: Option<bool>,
) -> Result<FixOutcome, HostError> {
    log::info!("diagnostics: applying `{fix_id}`");
    doctor
        .inner()
        .fix(&fix_id, confirm.unwrap_or(false))
        .await
        .map(|message| FixOutcome { message })
}

#[cfg(test)]
mod tests {
    use super::*;

    use std::sync::Mutex;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// The app's side of the repairs: removes from the list as the agent
    /// host would (refused for a "busy" id), counts sign-outs.
    struct FakeHooks {
        workspaces: Arc<WorkspaceStore>,
        busy: Mutex<Vec<String>>,
        removed: Mutex<Vec<String>>,
        signed_out: AtomicUsize,
        /// Indexes closed, and rebuilt, through the app's paths.
        closed_indexes: Mutex<Vec<String>>,
        /// Whether each closed index's directory was gone (moved or
        /// deleted) before the index was released again.
        gone_while_closed: Mutex<Vec<bool>>,
        rebuilt_indexes: Mutex<Vec<String>>,
    }

    impl DoctorHooks for FakeHooks {
        fn remove_workspace(&self, workspace_id: &str) -> Result<(), String> {
            if self
                .busy
                .lock()
                .unwrap()
                .iter()
                .any(|id| id == workspace_id)
            {
                return Err("a turn is running in it".into());
            }
            self.workspaces
                .remove(workspace_id)
                .map_err(|e| e.to_string())?;
            self.removed.lock().unwrap().push(workspace_id.to_owned());
            Ok(())
        }

        fn signed_out(&self) {
            self.signed_out.fetch_add(1, Ordering::SeqCst);
        }

        fn with_index_closed(&self, workspace_id: &str, work: &mut (dyn FnMut() + Send)) {
            self.closed_indexes
                .lock()
                .unwrap()
                .push(workspace_id.to_owned());
            work();
            let dir = self.workspaces.data_dir(workspace_id).join("index");
            self.gone_while_closed
                .lock()
                .unwrap()
                .push(fs::symlink_metadata(dir).is_err());
        }

        fn rebuild_index(&self, workspace_id: &str) -> Result<(), String> {
            self.rebuilt_indexes
                .lock()
                .unwrap()
                .push(workspace_id.to_owned());
            Ok(())
        }
    }

    struct Fixture {
        _root: tempfile::TempDir,
        doctor: LocalDoctor,
        hooks: Arc<FakeHooks>,
    }

    fn fixture() -> Fixture {
        let root = tempfile::tempdir().unwrap();
        let config_dir = root.path().join("config");
        let data_dir = root.path().join("data");
        let log_dir = root.path().join("logs");
        for dir in [&config_dir, &data_dir, &log_dir] {
            credentials_file::create_private_dir(dir).unwrap();
        }
        let workspaces = Arc::new(WorkspaceStore::new(data_dir.clone()));
        let hooks = Arc::new(FakeHooks {
            workspaces: workspaces.clone(),
            busy: Mutex::default(),
            removed: Mutex::default(),
            signed_out: AtomicUsize::new(0),
            closed_indexes: Mutex::default(),
            gone_while_closed: Mutex::default(),
            rebuilt_indexes: Mutex::default(),
        });
        let doctor = LocalDoctor {
            hooks: hooks.clone(),
            credentials: CredentialsFile::new(config_dir.clone()),
            workspaces,
            config_dir,
            data_dir,
            log_dir: Some(log_dir),
            history: None,
        };
        Fixture {
            _root: root,
            doctor,
            hooks,
        }
    }

    struct NoBrowser;

    impl crate::auth::BrowserOpener for NoBrowser {
        fn open(&self, _url: &str) -> Result<(), HostError> {
            Err(HostError::Browser)
        }
    }

    /// The whole Doctor over `local`, with a real auth service on its files.
    fn whole(local: LocalDoctor) -> Arc<Doctor> {
        let auth = AuthService::new(crate::auth::AuthConfig {
            store: Arc::new(local.credentials.slot("device-session")),
            pending_revokes: Arc::new(local.credentials.slot("pending-revoke")),
            files: crate::settings::SettingsFiles::new(local.config_dir.clone()),
            tokens: crate::tokens::TokenEndpoint::new("0.1.0").unwrap(),
            opener: Arc::new(NoBrowser),
            build_client_id: None,
            runtime_client_id: None,
        });
        Arc::new(Doctor {
            local,
            auth: Arc::new(auth),
        })
    }

    fn check<'a>(checks: &'a [Check], id: &str) -> &'a Check {
        checks.iter().find(|c| c.id == id).unwrap()
    }

    #[test]
    fn a_fresh_install_is_all_ok() {
        let f = fixture();
        let checks = f.doctor.checks();
        assert!(checks.iter().all(|c| c.status == Status::Ok), "{checks:#?}");
    }

    #[cfg(unix)]
    #[test]
    fn a_credentials_file_others_can_read_is_tightened_on_request() {
        use crate::store::SecretStore as _;
        use std::os::unix::fs::PermissionsExt as _;
        let f = fixture();
        f.doctor
            .credentials
            .slot("device-session")
            .save("s")
            .unwrap();
        let path = f.doctor.credentials.path();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).unwrap();
        let c = check(&f.doctor.checks(), "credentials").clone();
        assert_eq!(c.status, Status::Warn);
        assert_eq!(c.fix_id.as_deref(), Some("credentials.tighten"));
        f.doctor.fix("credentials.tighten", false).unwrap();
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(check(&f.doctor.checks(), "credentials").status, Status::Ok);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn an_untrusted_credentials_file_is_moved_aside_and_sign_in_works_again() {
        use crate::store::SecretStore as _;
        use std::os::unix::fs::PermissionsExt as _;
        let f = fixture();
        let slot = f.doctor.credentials.slot("device-session");
        slot.save("planted?").unwrap();
        let path = f.doctor.credentials.path();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o666)).unwrap();
        // A new launch refuses it, and sign-in says where to go.
        let launch = CredentialsFile::new(f.doctor.config_dir.clone());
        let error = launch.slot("device-session").save("new").unwrap_err();
        assert!(error.to_string().contains("Run Diagnostics"), "{error}");
        let doctor = whole(LocalDoctor {
            credentials: launch.clone(),
            ..f.doctor
        });
        let c = check(&doctor.local.checks(), "credentials").clone();
        assert_eq!(c.status, Status::Fail);
        assert_eq!(c.fix_id.as_deref(), Some("credentials.move_aside"));
        doctor.fix("credentials.move_aside", false).await.unwrap();
        let doctor = &doctor.local;
        // The original is kept aside, untouched; sign-in works.
        let aside = entries(&doctor.config_dir, "credentials.json.broken-");
        assert_eq!(aside.len(), 1);
        assert!(
            doctor
                .config_dir
                .join(&aside[0])
                .join("credentials.json")
                .is_file()
        );
        launch.slot("device-session").save("new").unwrap();
        assert_eq!(check(&doctor.checks(), "credentials").status, Status::Ok);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_symlinked_credentials_file_is_moved_aside_never_followed() {
        let f = fixture();
        let target = f.doctor.config_dir.parent().unwrap().join("elsewhere");
        fs::write(&target, "{}").unwrap();
        std::os::unix::fs::symlink(&target, f.doctor.credentials.path()).unwrap();
        assert_eq!(
            check(&f.doctor.checks(), "credentials").status,
            Status::Fail
        );
        let path = f.doctor.credentials.path();
        whole(f.doctor)
            .fix("credentials.move_aside", false)
            .await
            .unwrap();
        assert!(fs::symlink_metadata(&path).is_err());
        assert_eq!(
            fs::read_to_string(&target).unwrap(),
            "{}",
            "target untouched"
        );
    }

    #[test]
    fn a_damaged_credentials_file_is_a_failure_with_a_repair() {
        use crate::store::SecretStore as _;
        let f = fixture();
        f.doctor
            .credentials
            .slot("device-session")
            .save("s")
            .unwrap();
        fs::write(f.doctor.credentials.path(), "not json").unwrap();
        let c = check(&f.doctor.checks(), "credentials").clone();
        assert_eq!(c.status, Status::Fail);
        assert_eq!(c.fix_id.as_deref(), Some("credentials.move_aside"));
    }

    #[cfg(unix)]
    #[test]
    fn a_folder_others_can_read_is_tightened() {
        use std::os::unix::fs::PermissionsExt as _;
        let f = fixture();
        let logs = f.doctor.log_dir.clone().unwrap();
        fs::set_permissions(&logs, fs::Permissions::from_mode(0o755)).unwrap();
        let c = check(&f.doctor.checks(), "dir.logs").clone();
        assert_eq!(
            (c.status, c.fix_id.as_deref()),
            (Status::Warn, Some("dir.tighten.logs"))
        );
        f.doctor.fix("dir.tighten.logs", false).unwrap();
        assert_eq!(
            fs::metadata(&logs).unwrap().permissions().mode() & 0o777,
            0o700
        );
    }

    #[test]
    fn a_damaged_history_is_moved_aside() {
        let f = fixture();
        let path = f.doctor.data_dir.join(history::FILE_NAME);
        credentials_file::create_private_file(&path)
            .unwrap()
            .write_all_bytes(b"this is not a database at all, not even close")
            .unwrap();
        let c = check(&f.doctor.checks(), "history").clone();
        assert_eq!(
            (c.status, c.fix_id.as_deref()),
            (Status::Fail, Some("history.move_aside")),
            "{c:?}"
        );
        f.doctor.fix("history.move_aside", false).unwrap();
        assert!(!path.exists());
        assert_eq!(check(&f.doctor.checks(), "history").status, Status::Ok);
    }

    #[test]
    fn a_healthy_history_passes_its_integrity_check() {
        let f = fixture();
        let store = Arc::new(history::HistoryStore::open(&f.doctor.data_dir).unwrap());
        let open = LocalDoctor {
            history: Some(store),
            ..f.doctor
        };
        assert_eq!(check(&open.checks(), "history").status, Status::Ok);
        let closed = LocalDoctor {
            history: None,
            ..open
        };
        assert_eq!(check(&closed.checks(), "history").status, Status::Warn);
    }

    /// The names of the entries in `dir` that start with `prefix`.
    fn entries(dir: &Path, prefix: &str) -> Vec<String> {
        fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|n| n.starts_with(prefix))
            .collect()
    }

    #[test]
    fn an_open_history_is_closed_moved_with_its_wal_and_started_afresh() {
        let f = fixture();
        let store = Arc::new(history::HistoryStore::open(&f.doctor.data_dir).unwrap());
        let doctor = LocalDoctor {
            history: Some(store.clone()),
            ..f.doctor
        };
        let message = doctor.fix("history.move_aside", false).unwrap();
        assert!(message.contains("new, empty history"), "{message}");
        let moved = entries(&doctor.data_dir, "threads.sqlite.broken-");
        assert_eq!(moved.len(), 1, "one directory: {moved:?}");
        let aside = doctor.data_dir.join(&moved[0]);
        assert!(aside.is_dir());
        // The database keeps its own name, so SQLite still finds its WAL.
        assert!(aside.join(history::FILE_NAME).is_file());
        for name in entries(&aside, "") {
            assert!(name.starts_with(history::FILE_NAME), "{name}");
        }
        // The store works on a fresh file, and the check is green.
        assert!(doctor.data_dir.join(history::FILE_NAME).is_file());
        assert_eq!(check(&doctor.checks(), "history").status, Status::Ok);
        // Again: a second directory, the first untouched.
        doctor.fix("history.move_aside", false).unwrap();
        assert_eq!(entries(&doctor.data_dir, "threads.sqlite.broken-").len(), 2);
        assert!(aside.join(history::FILE_NAME).is_file());
    }

    #[test]
    fn moving_aside_twice_never_overwrites_the_first_copy() {
        let f = fixture();
        for content in ["[{", "[{{"] {
            fs::write(f.doctor.workspaces.file_path(), content).unwrap();
            f.doctor.fix("workspaces.move_aside", false).unwrap();
        }
        let moved = entries(&f.doctor.data_dir, "workspaces.json.broken-");
        assert_eq!(moved.len(), 2, "{moved:?}");
        let mut kept: Vec<String> = moved
            .iter()
            .map(|dir| {
                fs::read_to_string(f.doctor.data_dir.join(dir).join("workspaces.json")).unwrap()
            })
            .collect();
        kept.sort();
        assert_eq!(kept, ["[{", "[{{"]);
    }

    #[test]
    fn missing_workspace_folders_are_removed_the_way_the_app_removes_them() {
        let f = fixture();
        let kept = tempfile::tempdir().unwrap();
        let gone = tempfile::tempdir().unwrap();
        let busy_gone = tempfile::tempdir().unwrap();
        f.doctor.workspaces.add(kept.path()).unwrap();
        let gone_id = f.doctor.workspaces.add(gone.path()).unwrap().id;
        let busy_id = f.doctor.workspaces.add(busy_gone.path()).unwrap().id;
        fs::create_dir_all(f.doctor.workspaces.data_dir(&gone_id)).unwrap();
        drop(gone);
        drop(busy_gone);
        f.hooks.busy.lock().unwrap().push(busy_id.clone());
        let c = check(&f.doctor.checks(), "workspaces").clone();
        assert_eq!(
            (c.status, c.fix_id.as_deref()),
            (Status::Warn, Some("workspaces.drop_missing"))
        );
        let confirm = c.fix_confirm.unwrap();
        assert!(confirm.contains("thread history"), "{confirm}");
        // Nothing is deleted before the person confirms.
        assert!(f.doctor.fix("workspaces.drop_missing", false).is_err());
        assert_eq!(f.doctor.workspaces.all().unwrap().len(), 3);
        assert!(f.doctor.workspaces.data_dir(&gone_id).exists());
        // Confirmed: through the app's removal, which refuses a busy one.
        let message = f.doctor.fix("workspaces.drop_missing", true).unwrap();
        assert_eq!(
            *f.hooks.removed.lock().unwrap(),
            std::slice::from_ref(&gone_id)
        );
        assert!(message.contains("Not removed"), "{message}");
        assert_eq!(f.doctor.workspaces.all().unwrap().len(), 2);
        assert!(!f.doctor.workspaces.data_dir(&gone_id).exists());
        f.hooks.busy.lock().unwrap().clear();
        f.doctor.fix("workspaces.drop_missing", true).unwrap();
        assert_eq!(check(&f.doctor.checks(), "workspaces").status, Status::Ok);
    }

    #[test]
    fn a_folder_is_missing_only_when_its_parent_is_there() {
        let root = tempfile::tempdir().unwrap();
        let here = root.path().join("here");
        fs::create_dir(&here).unwrap();
        assert_eq!(reach(&here), Reach::Reachable);
        assert_eq!(reach(&root.path().join("gone")), Reach::Missing);
        assert!(matches!(
            reach(&root.path().join("no/such/tree")),
            Reach::Unavailable(_)
        ));
        let drive = format!(
            "/Volumes/elitea-doctor-{:08x}/project",
            rand::random::<u32>()
        );
        assert_eq!(
            reach(Path::new(&drive)),
            Reach::Unavailable("drive not connected")
        );
    }

    #[cfg(unix)]
    #[test]
    fn a_folder_the_app_may_not_read_or_on_a_drive_not_connected_is_never_removed() {
        use std::os::unix::fs::PermissionsExt as _;
        if rustix::process::geteuid().is_root() {
            return; // root reads a 000 folder anyway
        }
        let f = fixture();
        let locked = tempfile::tempdir().unwrap();
        let gone = tempfile::tempdir().unwrap();
        let locked_id = f.doctor.workspaces.add(locked.path()).unwrap().id;
        f.doctor.workspaces.add(gone.path()).unwrap();
        fs::create_dir_all(f.doctor.workspaces.data_dir(&locked_id)).unwrap();
        // A workspace on a drive that is not connected (written as stored).
        let file = f.doctor.workspaces.file_path();
        let mut stored: Vec<Value> =
            serde_json::from_str(&fs::read_to_string(&file).unwrap()).unwrap();
        stored.push(serde_json::json!({
            "id": "0123456789abcdef0123456789abcdef",
            "path": format!("/Volumes/elitea-doctor-{:08x}/project", rand::random::<u32>()),
            "name": "project", "project_id": null,
        }));
        fs::write(&file, serde_json::to_string(&stored).unwrap()).unwrap();
        fs::set_permissions(locked.path(), fs::Permissions::from_mode(0o000)).unwrap();
        drop(gone);

        let c = check(&f.doctor.checks(), "workspaces").clone();
        assert!(c.message.contains("not allowed to read"), "{}", c.message);
        assert!(c.message.contains("drive not connected"), "{}", c.message);
        if cfg!(target_os = "macos") {
            assert!(c.message.contains("Privacy & Security"), "{}", c.message);
        }
        f.doctor.fix("workspaces.drop_missing", true).unwrap();
        let left: Vec<String> = f
            .doctor
            .workspaces
            .all()
            .unwrap()
            .into_iter()
            .map(|w| w.id)
            .collect();
        fs::set_permissions(locked.path(), fs::Permissions::from_mode(0o700)).unwrap();
        assert_eq!(left.len(), 2, "only the gone one left the list: {left:?}");
        assert!(left.contains(&locked_id));
        assert!(
            f.doctor.workspaces.data_dir(&locked_id).exists(),
            "its data is kept"
        );
        // With only those left, there is nothing to repair.
        let c = check(&f.doctor.checks(), "workspaces").clone();
        assert_eq!(c.fix_id, None, "{c:?}");
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn moving_the_sign_in_aside_signs_out_on_this_computer() {
        use crate::store::SecretStore as _;
        use std::os::unix::fs::PermissionsExt as _;
        let f = fixture();
        f.doctor
            .credentials
            .slot("device-session")
            .save("planted?")
            .unwrap();
        fs::set_permissions(
            f.doctor.credentials.path(),
            fs::Permissions::from_mode(0o666),
        )
        .unwrap();
        let hooks = f.hooks.clone();
        let doctor = whole(f.doctor);
        let epoch = doctor.auth.session_epoch();
        let message = doctor.fix("credentials.move_aside", false).await.unwrap();
        // The local sign-out ran: a new session epoch, turns forgotten and
        // the webview told, once.
        assert!(doctor.auth.session_epoch() > epoch);
        assert_eq!(hooks.signed_out.load(Ordering::SeqCst), 1);
        assert!(!doctor.auth.state().unwrap().signed_in);
        // The moved file is not read, so its session is not revoked: said.
        assert!(message.contains("not ended on the server"), "{message}");
        assert_eq!(doctor.auth.pending_revoke_count(), 0);
        // Nothing to move: no sign-out.
        doctor.fix("credentials.move_aside", false).await.unwrap();
        assert_eq!(hooks.signed_out.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn a_damaged_workspaces_file_is_moved_aside() {
        let f = fixture();
        fs::write(f.doctor.workspaces.file_path(), "[{").unwrap();
        let c = check(&f.doctor.checks(), "workspaces").clone();
        assert_eq!(
            (c.status, c.fix_id.as_deref()),
            (Status::Fail, Some("workspaces.move_aside"))
        );
        f.doctor.fix("workspaces.move_aside", false).unwrap();
        assert!(f.doctor.workspaces.all().unwrap().is_empty());
    }

    #[test]
    fn local_work_off_says_how_to_turn_it_on() {
        let off = serde_json::json!({"local_work": {"allowed": false}});
        let c = local_work_check(true, Some(&off));
        assert_eq!(c.status, Status::Warn);
        assert!(c.message.contains("Native client policy"));
        let on = serde_json::json!({"local_work": {"allowed": true}});
        assert_eq!(local_work_check(true, Some(&on)).status, Status::Ok);
        assert_eq!(local_work_check(false, None).status, Status::Ok);
    }

    /// A workspace over a real folder, with its index built (and closed);
    /// the workspace, the index directory and the folder's guard.
    fn indexed_workspace(f: &Fixture) -> (Workspace, PathBuf, tempfile::TempDir) {
        let folder = tempfile::tempdir().unwrap();
        let workspace = f.doctor.workspaces.add(folder.path()).unwrap();
        fs::create_dir_all(f.doctor.workspaces.data_dir(&workspace.id)).unwrap();
        let dir = f.doctor.index_dir(&workspace.id);
        elitea_local_index::sqlite_store::SqliteGraphStore::open(&dir)
            .unwrap()
            .close();
        (workspace, dir, folder)
    }

    fn index_check_of(f: &Fixture, workspace: &Workspace) -> Option<Check> {
        f.doctor
            .checks()
            .into_iter()
            .find(|c| c.id == format!("index.{}", workspace.id))
    }

    #[test]
    fn a_workspace_without_an_index_has_no_index_check_and_a_built_one_passes() {
        let f = fixture();
        let folder = tempfile::tempdir().unwrap();
        let plain = f.doctor.workspaces.add(folder.path()).unwrap();
        assert!(index_check_of(&f, &plain).is_none());

        let (workspace, dir, _folder) = indexed_workspace(&f);
        let c = index_check_of(&f, &workspace).unwrap();
        assert_eq!((c.status, c.fix_id.as_deref()), (Status::Ok, None), "{c:?}");
        assert!(c.message.contains(&workspace.name), "{}", c.message);

        // Turned on but nothing built yet: not a problem.
        fs::remove_file(dir.join(INDEX_FILE)).unwrap();
        assert_eq!(index_check_of(&f, &workspace).unwrap().status, Status::Ok);
    }

    #[cfg(unix)]
    #[test]
    fn an_index_others_can_read_is_tightened() {
        use std::os::unix::fs::PermissionsExt as _;
        let f = fixture();
        let (workspace, dir, _folder) = indexed_workspace(&f);
        let file = dir.join(INDEX_FILE);
        fs::set_permissions(&file, fs::Permissions::from_mode(0o644)).unwrap();
        let c = index_check_of(&f, &workspace).unwrap();
        let fix = format!("index.tighten:{}", workspace.id);
        assert_eq!(
            (c.status, c.fix_id.as_deref()),
            (Status::Warn, Some(fix.as_str()))
        );
        f.doctor.fix(&fix, false).unwrap();
        assert_eq!(
            fs::metadata(&file).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(index_check_of(&f, &workspace).unwrap().status, Status::Ok);
    }

    #[cfg(unix)]
    #[test]
    fn a_symlinked_index_is_moved_aside_after_it_is_closed() {
        let f = fixture();
        let (workspace, dir, _folder) = indexed_workspace(&f);
        let elsewhere = tempfile::tempdir().unwrap();
        fs::rename(&dir, elsewhere.path().join("index")).unwrap();
        std::os::unix::fs::symlink(elsewhere.path().join("index"), &dir).unwrap();

        let c = index_check_of(&f, &workspace).unwrap();
        let fix = format!("index.move_aside:{}", workspace.id);
        assert_eq!(
            (c.status, c.fix_id.as_deref()),
            (Status::Fail, Some(fix.as_str()))
        );
        assert!(c.message.contains("symbolic link"), "{}", c.message);
        // A rebuild never deletes through it.
        assert!(
            f.doctor
                .fix(&format!("index.rebuild:{}", workspace.id), true)
                .is_err()
        );
        f.doctor.fix(&fix, false).unwrap();
        assert_eq!(
            *f.hooks.closed_indexes.lock().unwrap(),
            vec![workspace.id.clone()]
        );
        assert!(fs::symlink_metadata(&dir).is_err());
        assert_eq!(
            *f.hooks.gone_while_closed.lock().unwrap(),
            vec![true],
            "moved while the index was held closed"
        );
        assert!(
            elsewhere.path().join("index").join(INDEX_FILE).is_file(),
            "the target is untouched"
        );
        assert!(index_check_of(&f, &workspace).is_none());
    }

    #[test]
    fn a_damaged_index_is_rebuilt_only_once_confirmed() {
        let f = fixture();
        let (workspace, dir, _folder) = indexed_workspace(&f);
        fs::write(dir.join(INDEX_FILE), vec![0x5a_u8; 8192]).unwrap();

        let c = index_check_of(&f, &workspace).unwrap();
        let fix = format!("index.rebuild:{}", workspace.id);
        assert_eq!(
            (c.status, c.fix_id.as_deref()),
            (Status::Fail, Some(fix.as_str()))
        );
        assert!(
            c.fix_confirm
                .as_deref()
                .is_some_and(|what| what.contains("costs nothing"))
        );

        assert!(
            f.doctor.fix(&fix, false).is_err(),
            "refused without confirm"
        );
        assert!(dir.join(INDEX_FILE).is_file());

        let message = f.doctor.fix(&fix, true).unwrap();
        assert!(message.contains("being built"), "{message}");
        assert!(!dir.exists());
        assert_eq!(
            *f.hooks.closed_indexes.lock().unwrap(),
            vec![workspace.id.clone()]
        );
        assert_eq!(
            *f.hooks.rebuilt_indexes.lock().unwrap(),
            vec![workspace.id.clone()]
        );
        assert_eq!(
            *f.hooks.gone_while_closed.lock().unwrap(),
            vec![true],
            "deleted while the index was held closed"
        );
    }

    #[test]
    fn an_index_from_a_newer_app_asks_for_an_update() {
        let f = fixture();
        let (workspace, dir, _folder) = indexed_workspace(&f);
        rusqlite::Connection::open(dir.join(INDEX_FILE))
            .unwrap()
            .execute_batch(&format!(
                "PRAGMA user_version = {};",
                INDEX_SCHEMA_VERSION + 1
            ))
            .unwrap();
        let c = index_check_of(&f, &workspace).unwrap();
        assert_eq!((c.status, c.fix_id.as_deref()), (Status::Fail, None));
        assert!(c.message.contains("update the app"), "{}", c.message);
    }

    #[test]
    fn an_index_repair_names_a_workspace_in_the_list() {
        let f = fixture();
        let (_workspace, _dir, _folder) = indexed_workspace(&f);
        for fix in [
            "index.tighten:nope",
            "index.rebuild:../..",
            "index.move_aside:",
        ] {
            assert!(f.doctor.fix(fix, true).is_err(), "{fix}");
        }
        assert!(f.hooks.closed_indexes.lock().unwrap().is_empty());
    }

    #[test]
    fn an_unknown_repair_is_refused() {
        assert!(fixture().doctor.fix("rm -rf", false).is_err());
    }

    trait WriteAll {
        fn write_all_bytes(self, bytes: &[u8]) -> std::io::Result<()>;
    }
    impl WriteAll for fs::File {
        fn write_all_bytes(mut self, bytes: &[u8]) -> std::io::Result<()> {
            std::io::Write::write_all(&mut self, bytes)
        }
    }
}
