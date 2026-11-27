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

use elitea_local_tools::lsp::Language;

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
    /// What the folder mostly holds ([`folder_kind`]): computed for the UI
    /// (`list_with_kinds`, `add`, `bind_project`); `list` and `get` leave it
    /// `mixed`.
    #[serde(default)]
    pub kind: FolderKind,
    /// A task worktree's workspace (#1235, src/worktrees.rs): the workspace
    /// whose repository it is a linked worktree of. Its folder is in that
    /// workspace's host data; it is listed with it, never on its own.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent_id: Option<String>,
    /// The thread the task worktree was made for.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_uuid: Option<String>,
}

/// The step limits a workspace may set for its turns (inclusive), beside
/// [`StepLimit::Unlimited`].
pub const STEP_LIMIT_RANGE: std::ops::RangeInclusive<u32> = 1..=1_000_000;

/// A workspace's step limit: so many model steps per run, or none
/// (`"unlimited"` in JSON; a number otherwise).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StepLimit {
    Steps(u32),
    Unlimited,
}

impl StepLimit {
    /// The JSON spelling of [`StepLimit::Unlimited`].
    pub const UNLIMITED: &'static str = "unlimited";

    /// Whether a workspace may set it: a number in [`STEP_LIMIT_RANGE`],
    /// or unlimited.
    #[must_use]
    pub fn valid(self) -> bool {
        match self {
            Self::Steps(steps) => STEP_LIMIT_RANGE.contains(&steps),
            Self::Unlimited => true,
        }
    }
}

impl Serialize for StepLimit {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        match self {
            Self::Steps(steps) => serializer.serialize_u32(*steps),
            Self::Unlimited => serializer.serialize_str(Self::UNLIMITED),
        }
    }
}

impl<'de> Deserialize<'de> for StepLimit {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        match serde_json::Value::deserialize(deserializer)? {
            serde_json::Value::String(text) if text == Self::UNLIMITED => Ok(Self::Unlimited),
            serde_json::Value::Number(number) => number
                .as_u64()
                .and_then(|steps| u32::try_from(steps).ok())
                .map(Self::Steps)
                .ok_or_else(|| serde::de::Error::custom("a step limit is a whole number")),
            _ => Err(serde::de::Error::custom(
                "a step limit is a number or \"unlimited\"",
            )),
        }
    }
}

/// The diagnostics waits a person may set per language, in milliseconds
/// (#1236).
pub const DIAGNOSTICS_WAIT_RANGE: std::ops::RangeInclusive<u32> = 100..=30_000;

/// The file in a workspace's host data that holds its [`AgentSettings`].
const AGENT_SETTINGS_FILE: &str = "agent_settings.json";

/// The person's agent settings for one workspace (IPC
/// `workspace_agent_settings_*`), kept in its host data next to its
/// remembered choices, never in the folder.
#[derive(Clone, Copy, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct AgentSettings {
    /// Model steps per run of this workspace's turns: a number within
    /// [`STEP_LIMIT_RANGE`], or unlimited. `None`: the default (1024).
    #[serde(default)]
    pub step_limit: Option<StepLimit>,
    /// Whether the person may choose another model than the agent's for a
    /// turn of this workspace (#1228). `None`: allowed (the default).
    #[serde(default)]
    pub allow_model_override: Option<bool>,
    /// Whether edits report the language servers' diagnostics (#1236).
    /// `None`: on (servers start only when found on `PATH`).
    #[serde(default)]
    pub diagnostics: Option<bool>,
    /// How long an edit waits for each language's diagnostics, within
    /// [`DIAGNOSTICS_WAIT_RANGE`]; `None`: the language's default.
    #[serde(default)]
    pub diagnostics_wait_ms: DiagnosticsWaits,
}

/// Per-language diagnostics waits in milliseconds (`None`: the default,
/// 1.5 s, longer on a server's first answer).
#[derive(Clone, Copy, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct DiagnosticsWaits {
    #[serde(default)]
    pub rust: Option<u32>,
    #[serde(default)]
    pub typescript: Option<u32>,
    #[serde(default)]
    pub python: Option<u32>,
    #[serde(default)]
    pub go: Option<u32>,
}

impl DiagnosticsWaits {
    /// Each set wait with its language.
    #[must_use]
    pub fn entries(&self) -> [(Language, Option<u32>); 4] {
        [
            (Language::Rust, self.rust),
            (Language::Typescript, self.typescript),
            (Language::Python, self.python),
            (Language::Go, self.go),
        ]
    }

    /// Whether every set wait is within [`DIAGNOSTICS_WAIT_RANGE`].
    #[must_use]
    pub fn valid(&self) -> bool {
        self.entries()
            .iter()
            .all(|(_, wait)| wait.is_none_or(|ms| DIAGNOSTICS_WAIT_RANGE.contains(&ms)))
    }

    fn within_range(self) -> Self {
        let keep = |wait: Option<u32>| wait.filter(|ms| DIAGNOSTICS_WAIT_RANGE.contains(ms));
        Self {
            rust: keep(self.rust),
            typescript: keep(self.typescript),
            python: keep(self.python),
            go: keep(self.go),
        }
    }
}

impl AgentSettings {
    /// Edits report diagnostics (the default).
    #[must_use]
    pub fn diagnostics_enabled(&self) -> bool {
        self.diagnostics != Some(false)
    }

    /// A model other than the agent's may be chosen for a turn.
    #[must_use]
    pub fn model_override_allowed(&self) -> bool {
        self.allow_model_override != Some(false)
    }
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

/// What a folder mostly holds, for the UI's chip, the agent it offers
/// first and the turn's environment line.
#[derive(Clone, Copy, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum FolderKind {
    /// Inside a git work tree, whatever it holds.
    Git,
    /// A project marker ([`elitea_local_tools::project::MARKER_FILES`]) or
    /// mostly source files, and few documents.
    Code,
    /// Mostly documents (Office files, PDFs, Markdown, text) and no marker.
    Documents,
    /// Both, or nothing either way (an empty folder).
    #[default]
    Mixed,
}

/// The most entries [`folder_kind`] looks at: a sample, never a walk of a
/// large tree.
pub const KIND_SAMPLE_ENTRIES: usize = 400;

/// How deep below the folder [`folder_kind`] looks.
const KIND_SAMPLE_DEPTH: usize = 3;

/// Folders the sample never enters: dependencies and build output say
/// nothing about what the person keeps here.
const KIND_SKIPPED_DIRS: &[&str] = &[
    "node_modules",
    "target",
    "dist",
    "build",
    "vendor",
    "__pycache__",
    "venv",
];

const DOCUMENT_EXTENSIONS: &[&str] = &[
    "docx", "doc", "pdf", "xlsx", "xls", "pptx", "ppt", "md", "txt", "rtf", "odt", "ods", "odp",
    "pages", "numbers", "key", "csv",
];

const CODE_EXTENSIONS: &[&str] = &[
    "rs", "ts", "tsx", "js", "jsx", "mjs", "py", "go", "java", "kt", "c", "h", "cc", "cpp", "hpp",
    "cs", "rb", "php", "swift", "scala", "sh", "lua", "dart", "sql", "vue", "svelte",
];

#[cfg(test)]
thread_local! {
    /// How many entries `folder_kind` looked at on this thread (tests only).
    static KIND_ENTRIES: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
}

/// The folder's kind: `git` when it is in a git work tree (nothing more is
/// read), else from a breadth-first sample of at most
/// [`KIND_SAMPLE_ENTRIES`] names at most three levels down. Only names
/// and entry types are read: no file is opened, so a cloud placeholder is
/// never downloaded. Hidden entries and [`KIND_SKIPPED_DIRS`] are left out.
#[must_use]
pub fn folder_kind(path: &Path, is_git: bool) -> FolderKind {
    if is_git {
        return FolderKind::Git;
    }
    let (mut code, mut documents, mut marker) = (0_usize, 0_usize, false);
    let mut seen = 0_usize;
    let mut queue = std::collections::VecDeque::from([(path.to_path_buf(), 0_usize)]);
    'walk: while let Some((dir, depth)) = queue.pop_front() {
        let Ok(entries) = fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            if seen >= KIND_SAMPLE_ENTRIES {
                break 'walk;
            }
            seen += 1;
            #[cfg(test)]
            KIND_ENTRIES.with(|entries| entries.set(entries.get() + 1));
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if name.starts_with('.') {
                continue;
            }
            let Ok(kind) = entry.file_type() else {
                continue;
            };
            if kind.is_dir() {
                if depth + 1 < KIND_SAMPLE_DEPTH && !KIND_SKIPPED_DIRS.contains(&name.as_ref()) {
                    queue.push_back((entry.path(), depth + 1));
                }
                continue;
            }
            if depth <= 1 && elitea_local_tools::project::MARKER_FILES.contains(&name.as_ref()) {
                marker = true;
            }
            let extension = name
                .rsplit_once('.')
                .map_or_else(String::new, |(_, ext)| ext.to_ascii_lowercase());
            if DOCUMENT_EXTENSIONS.contains(&extension.as_str()) {
                documents += 1;
            } else if CODE_EXTENSIONS.contains(&extension.as_str()) {
                code += 1;
            }
        }
    }
    classify(code, documents, marker)
}

/// Mostly (four in five or more) one or the other; a marker makes it code
/// unless documents are the large majority.
fn classify(code: usize, documents: usize, marker: bool) -> FolderKind {
    let code = code + usize::from(marker);
    let total = code + documents;
    if total == 0 {
        return FolderKind::Mixed;
    }
    if documents * 5 >= total * 4 && !marker {
        FolderKind::Documents
    } else if code * 5 >= total * 4 || (marker && documents * 5 < total * 4) {
        FolderKind::Code
    } else {
        FolderKind::Mixed
    }
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

    /// [`Self::list`] with each folder's `kind` sampled too
    /// ([`folder_kind`]): only for what the UI shows (`workspace_list`, off
    /// the main thread), never for the host's own lookups, since sampling a
    /// folder on a slow or network drive can block.
    ///
    /// # Errors
    ///
    /// The file cannot be read.
    pub fn list_with_kinds(&self) -> Result<Vec<Workspace>, HostError> {
        Ok(self
            .list()?
            .into_iter()
            .map(|mut workspace| {
                workspace.kind = folder_kind(Path::new(&workspace.path), workspace.is_git);
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
                kind: FolderKind::default(),
                parent_id: None,
                thread_uuid: None,
            };
            all.push(workspace.clone());
            self.write(&all)?;
            Ok(workspace)
        })?;
        let is_git = in_git_tree(&canonical);
        Ok(Workspace {
            is_git,
            kind: folder_kind(&canonical, is_git),
            ..workspace
        })
    }

    /// Add a task worktree's workspace (#1235): `folder` (canonical, in
    /// `parent`'s host data), bound to `parent`'s project, named after
    /// `branch`.
    ///
    /// # Errors
    ///
    /// No such parent, or the file cannot be written.
    pub fn add_worktree(
        &self,
        parent_id: &str,
        folder: &Path,
        thread_uuid: &str,
        branch: &str,
    ) -> Result<Workspace, HostError> {
        self.with(|| {
            let mut all = self.read()?;
            let parent = all
                .iter()
                .find(|w| w.id == parent_id && w.parent_id.is_none())
                .cloned()
                .ok_or_else(|| HostError::Storage("no such workspace".into()))?;
            let path = folder.to_string_lossy().into_owned();
            if let Some(existing) = all.iter().find(|w| w.path == path) {
                return Ok(existing.clone());
            }
            let workspace = Workspace {
                id: uuid::Uuid::new_v4().simple().to_string(),
                name: format!("{} ({branch})", parent.name),
                path,
                project_id: parent.project_id,
                is_git: false,
                kind: FolderKind::default(),
                parent_id: Some(parent.id),
                thread_uuid: Some(thread_uuid.to_owned()),
            };
            all.push(workspace.clone());
            self.write(&all)?;
            Ok(workspace)
        })
    }

    /// The task worktrees' workspaces of `parent_id`.
    ///
    /// # Errors
    ///
    /// The file cannot be read.
    pub fn worktrees_of(&self, parent_id: &str) -> Result<Vec<Workspace>, HostError> {
        Ok(self
            .all()?
            .into_iter()
            .filter(|w| w.parent_id.as_deref() == Some(parent_id))
            .collect())
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

    /// A workspace's agent settings (the defaults when none were saved).
    ///
    /// # Errors
    ///
    /// No such workspace, or the file cannot be read: a symlink, another
    /// user's file, or one that does not parse.
    pub fn agent_settings(&self, id: &str) -> Result<AgentSettings, HostError> {
        self.with(|| {
            self.known(id)?;
            let path = self.data_dir(id).join(AGENT_SETTINGS_FILE);
            let file = match crate::credentials_file::open_no_follow(&path) {
                Ok(file) => file,
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                    return Ok(AgentSettings::default());
                }
                Err(e) => return Err(io(&e)),
            };
            check_private(&file)?;
            let settings: AgentSettings = serde_json::from_reader(file).map_err(|_| {
                HostError::Storage("the workspace's agent settings are unreadable".into())
            })?;
            Ok(AgentSettings {
                step_limit: settings.step_limit.filter(|limit| limit.valid()),
                allow_model_override: settings.allow_model_override,
                diagnostics: settings.diagnostics,
                diagnostics_wait_ms: settings.diagnostics_wait_ms.within_range(),
            })
        })
    }

    /// Store a workspace's agent settings; a step limit outside
    /// [`STEP_LIMIT_RANGE`] (and not unlimited) is refused.
    ///
    /// # Errors
    ///
    /// No such workspace, an invalid limit, or the file cannot be written.
    pub fn set_agent_settings(
        &self,
        id: &str,
        settings: AgentSettings,
    ) -> Result<AgentSettings, HostError> {
        if settings.step_limit.is_some_and(|limit| !limit.valid()) {
            return Err(HostError::Unsupported(format!(
                "the step limit must be {} to {}, or unlimited",
                STEP_LIMIT_RANGE.start(),
                STEP_LIMIT_RANGE.end()
            )));
        }
        if !settings.diagnostics_wait_ms.valid() {
            return Err(HostError::Unsupported(format!(
                "a diagnostics wait must be {} to {} ms",
                DIAGNOSTICS_WAIT_RANGE.start(),
                DIAGNOSTICS_WAIT_RANGE.end()
            )));
        }
        self.with(|| {
            self.known(id)?;
            let dir = self.data_dir(id);
            crate::credentials_file::create_private_dir(&dir)?;
            let bytes =
                serde_json::to_vec(&settings).map_err(|e| HostError::Internal(e.to_string()))?;
            let temp = dir.join(format!(".{AGENT_SETTINGS_FILE}.{}.tmp", std::process::id()));
            let _ = fs::remove_file(&temp);
            {
                use std::io::Write as _;
                let mut file =
                    crate::credentials_file::create_private_file(&temp).map_err(|e| io(&e))?;
                file.write_all(&bytes)
                    .and_then(|()| file.sync_all())
                    .map_err(|e| io(&e))?;
            }
            fs::rename(&temp, dir.join(AGENT_SETTINGS_FILE)).map_err(|e| io(&e))?;
            Ok(settings)
        })
    }

    /// `id` is a workspace of the store (and so names its own data folder).
    fn known(&self, id: &str) -> Result<(), HostError> {
        if self.read()?.iter().any(|w| w.id == id) {
            Ok(())
        } else {
            Err(HostError::Storage("no such workspace".into()))
        }
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
        let is_git = in_git_tree(Path::new(&workspace.path));
        Ok(Workspace {
            is_git,
            kind: folder_kind(Path::new(&workspace.path), is_git),
            ..workspace
        })
    }
}

/// A settings file this user owns, readable by nobody else.
fn check_private(file: &fs::File) -> Result<(), HostError> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt as _;
        let meta = file.metadata().map_err(|e| io(&e))?;
        if !meta.is_file()
            || meta.uid() != rustix::process::geteuid().as_raw()
            || meta.mode() & 0o077 != 0
        {
            return Err(HostError::Storage(
                "the workspace's agent settings file is not private to you; it was not used".into(),
            ));
        }
    }
    #[cfg(not(unix))]
    let _ = file;
    Ok(())
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;

    #[test]
    fn a_step_limit_is_a_number_or_unlimited_in_json() {
        let read = |text: &str| serde_json::from_str::<AgentSettings>(text);
        assert_eq!(
            read(r#"{"step_limit": 1024}"#).unwrap().step_limit,
            Some(StepLimit::Steps(1024))
        );
        assert_eq!(
            read(r#"{"step_limit": "unlimited"}"#).unwrap().step_limit,
            Some(StepLimit::Unlimited)
        );
        assert_eq!(read(r#"{"step_limit": null}"#).unwrap().step_limit, None);
        assert!(read(r#"{"step_limit": "many"}"#).is_err());
        assert!(read(r#"{"step_limit": -1}"#).is_err());
        let settings = AgentSettings {
            step_limit: Some(StepLimit::Unlimited),
            ..AgentSettings::default()
        };
        assert_eq!(
            serde_json::to_value(settings).unwrap()["step_limit"],
            "unlimited"
        );
    }

    #[test]
    fn agent_settings_are_kept_private_in_the_workspaces_data() {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        let store = WorkspaceStore::new(app.path().to_owned());
        let id = store.add(folder.path()).unwrap().id;
        assert_eq!(store.agent_settings(&id).unwrap(), AgentSettings::default());
        let saved = store
            .set_agent_settings(
                &id,
                AgentSettings {
                    step_limit: Some(StepLimit::Steps(150)),
                    allow_model_override: Some(false),
                    diagnostics: Some(false),
                    diagnostics_wait_ms: DiagnosticsWaits {
                        rust: Some(8_000),
                        ..DiagnosticsWaits::default()
                    },
                },
            )
            .unwrap();
        assert_eq!(saved.step_limit, Some(StepLimit::Steps(150)));
        assert_eq!(
            store.agent_settings(&id).unwrap().step_limit,
            Some(StepLimit::Steps(150))
        );
        assert!(!store.agent_settings(&id).unwrap().model_override_allowed());
        assert!(!store.agent_settings(&id).unwrap().diagnostics_enabled());
        assert_eq!(
            store.agent_settings(&id).unwrap().diagnostics_wait_ms.rust,
            Some(8_000)
        );
        let file = store.data_dir(&id).join(AGENT_SETTINGS_FILE);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = |p: &Path| fs::metadata(p).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode(&file), 0o600);
            assert_eq!(mode(&store.data_dir(&id)), 0o700);
            // A file others can read is not used.
            fs::set_permissions(&file, fs::Permissions::from_mode(0o644)).unwrap();
            assert!(store.agent_settings(&id).is_err());
            fs::set_permissions(&file, fs::Permissions::from_mode(0o600)).unwrap();
        }
        // Unlimited is kept as it is.
        let unlimited = AgentSettings {
            step_limit: Some(StepLimit::Unlimited),
            ..AgentSettings::default()
        };
        store.set_agent_settings(&id, unlimited).unwrap();
        assert_eq!(
            store.agent_settings(&id).unwrap().step_limit,
            Some(StepLimit::Unlimited)
        );
        // Outside 1..=1000000 is refused; `None` goes back to the default.
        for wrong in [0, 1_000_001] {
            assert!(
                store
                    .set_agent_settings(
                        &id,
                        AgentSettings {
                            step_limit: Some(StepLimit::Steps(wrong)),
                            ..AgentSettings::default()
                        }
                    )
                    .is_err()
            );
        }
        // A diagnostics wait outside 100..=30000 ms is refused too.
        assert!(
            store
                .set_agent_settings(
                    &id,
                    AgentSettings {
                        diagnostics_wait_ms: DiagnosticsWaits {
                            go: Some(50),
                            ..DiagnosticsWaits::default()
                        },
                        ..AgentSettings::default()
                    }
                )
                .is_err()
        );
        store
            .set_agent_settings(&id, AgentSettings::default())
            .unwrap();
        assert_eq!(store.agent_settings(&id).unwrap().step_limit, None);
        assert!(store.agent_settings(&id).unwrap().diagnostics_enabled());
        assert!(!folder.path().join(AGENT_SETTINGS_FILE).exists());
        assert!(store.agent_settings("nope").is_err());
        assert!(
            store
                .set_agent_settings("../x", AgentSettings::default())
                .is_err()
        );
    }

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

    fn touch(root: &Path, names: &[&str]) {
        for name in names {
            let path = root.join(name);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(path, "x").unwrap();
        }
    }

    #[test]
    fn a_folder_kind_is_read_from_its_names() {
        let docs = tempfile::tempdir().unwrap();
        touch(
            docs.path(),
            &[
                "Plan.docx",
                "Budget.xlsx",
                "notes/minutes.md",
                "scan.pdf",
                "deck.pptx",
            ],
        );
        assert_eq!(folder_kind(docs.path(), false), FolderKind::Documents);

        let code = tempfile::tempdir().unwrap();
        touch(code.path(), &["Cargo.toml", "README.md", "src/main.rs"]);
        assert_eq!(folder_kind(code.path(), false), FolderKind::Code);
        // A marker one level down counts; its content is never read.
        let nested = tempfile::tempdir().unwrap();
        touch(
            nested.path(),
            &["app/package.json", "README.md", "guide.md"],
        );
        assert_eq!(folder_kind(nested.path(), false), FolderKind::Code);

        let mixed = tempfile::tempdir().unwrap();
        touch(
            mixed.path(),
            &["report.docx", "data.pdf", "run.py", "clean.py"],
        );
        assert_eq!(folder_kind(mixed.path(), false), FolderKind::Mixed);
        assert_eq!(
            folder_kind(tempfile::tempdir().unwrap().path(), false),
            FolderKind::Mixed
        );
        // Dependencies, build output and hidden folders say nothing.
        let deps = tempfile::tempdir().unwrap();
        touch(
            deps.path(),
            &[
                "a.docx",
                "b.pdf",
                "node_modules/x/index.js",
                "node_modules/y/index.js",
                ".cache/z.js",
            ],
        );
        assert_eq!(folder_kind(deps.path(), false), FolderKind::Documents);
        // In a git work tree nothing is read.
        assert_eq!(folder_kind(code.path(), true), FolderKind::Git);
    }

    #[test]
    fn the_folder_kind_samples_a_bounded_number_of_entries() {
        let big = tempfile::tempdir().unwrap();
        for dir in 0..20 {
            for file in 0..50 {
                touch(big.path(), &[&format!("d{dir}/f{file}.docx")]);
            }
        }
        let looked = || KIND_ENTRIES.with(std::cell::Cell::get);
        let before = looked();
        assert_eq!(folder_kind(big.path(), false), FolderKind::Documents);
        assert_eq!(
            looked() - before,
            KIND_SAMPLE_ENTRIES,
            "1020 entries, only the sample read"
        );
        let before = looked();
        assert_eq!(folder_kind(big.path(), true), FolderKind::Git);
        assert_eq!(looked(), before, "a git folder is not sampled");
    }

    #[test]
    fn the_listed_kind_is_recomputed_each_time() {
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        touch(folder.path(), &["a.docx"]);
        let store = WorkspaceStore::new(app.path().to_owned());
        let added = store.add(folder.path()).unwrap();
        assert_eq!(added.kind, FolderKind::Documents);
        touch(
            folder.path(),
            &["go.mod", "main.go", "x.go", "y.go", "z.go"],
        );
        assert_eq!(store.list_with_kinds().unwrap()[0].kind, FolderKind::Code);
        // The host's own lookups never sample the folder.
        let looked = || KIND_ENTRIES.with(std::cell::Cell::get);
        let before = looked();
        assert_eq!(
            store.get(&added.id).unwrap().unwrap().kind,
            FolderKind::Mixed
        );
        assert_eq!(store.list().unwrap()[0].kind, FolderKind::Mixed);
        assert_eq!(looked(), before, "list() and get() read no folder names");
        let json = serde_json::to_value(&store.list_with_kinds().unwrap()[0]).unwrap();
        assert_eq!(json["kind"], "code");
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
