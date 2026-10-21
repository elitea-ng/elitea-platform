//! The person's approval rule files (#1216; ADR-0029 addendum §3): command
//! prefix rules in the JSON form of `elitea_local_tools::execpolicy`, loaded
//! into layer 3 of the approvals engine.
//!
//! * **User rules**: `<app config>/rules.json`, for every workspace.
//! * **Workspace rules**: `<app data>/workspaces/<id>/rules.json`.
//!
//! Layer 3 reads the workspace's rules, then the person's; a deny beats an
//! ask beats an allow across both. Both files live in the host's own data,
//! never in the folder, and follow the `threads.sqlite` rules
//! (`history.rs`): a `0700` directory, a `0600` file created before it is
//! written, opened without following a symlink, and refused when it is
//! another user's or readable or writable by group or others. A file that
//! is refused or fails validation loads **no** rules (the error says why;
//! the defaults still apply), and is never overwritten by an append.
//!
//! A repository may **propose** rules in `<folder>/.elitea/rules.json`. The
//! host never loads that file into a session; the settings panel shows the
//! proposal rule by rule and the person may import it (copied into the
//! workspace rules file), so a repository still cannot grant itself
//! permissions. An import grants less than a hand-written file may
//! ([`sanitize_proposal`]): no `host_executables` pins (a pin widens what
//! an allow matches, and a repository could pin a program to a file it
//! holds), no allow wider than `workspace-write` or with the network, and
//! every imported rule is marked `origin: "repository"` so its
//! justification (the repository's text) reaches the person but never the
//! model. What was left out is listed as warnings beside the rules.
//!
//! Writes from the panel carry the `version` (the file's digest) they were
//! made from; a file changed since is not overwritten (a conflict: the
//! panel reloads). An import carries the proposal's digest the same way.
//!
//! "Always allow" for a command appends a prefix rule to the workspace
//! rules file ([`WorkspaceRuleSink`]). Command choices an older host kept in
//! `choices.json` are migrated into it once ([`RuleFiles::migrate_choices`]);
//! the file choices stay in `choices.json`, which is still read.

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::SystemTime;

use elitea_local_tools::approvals::{CommandRules, RememberedChoice, RuleSink, choice_as_rule};
use elitea_local_tools::error::{ErrorCode, ToolError, ToolResult};
use elitea_local_tools::execpolicy::{
    LoadError, PrefixRule, RuleDecision, RuleFile, RuleOrigin, RuleScope, RuleSet,
};
use elitea_local_tools::policy::SandboxMode;
use elitea_local_tools::workspace::{Intent, Workspace};
use elitea_workspace_store::digest::sha256_hex;
use serde::Serialize;

use crate::error::HostError;
use crate::workspaces::WorkspaceStore;

/// The rules file's name, in the app config and in a workspace's data.
pub const RULES_FILE: &str = "rules.json";
/// Where a repository proposes rules, relative to its folder.
pub const PROPOSED_RULES: &str = ".elitea/rules.json";
/// The remembered choices an older host wrote (a workspace's data).
const CHOICES_FILE: &str = "choices.json";
/// The largest rules file read (1 MiB).
const MAX_RULES_BYTES: u64 = 1 << 20;

/// Why a write was refused.
#[derive(Debug)]
pub enum PutError {
    /// The file does not validate: nothing was written.
    Invalid(Box<LoadError>),
    /// The file (or the proposal) changed since the version the write was
    /// made from: nothing was written.
    Conflict,
    Host(HostError),
}

impl From<HostError> for PutError {
    fn from(error: HostError) -> Self {
        Self::Host(error)
    }
}

/// One scope's file as the panel lists it.
#[derive(Clone, Debug, Serialize)]
pub struct ScopeState {
    pub scope: RuleScope,
    /// Where it lives (for the person).
    pub path: String,
    /// The rules in force; `None` when the file is refused or broken.
    pub file: Option<RuleFile>,
    /// Why the file loads no rules.
    pub error: Option<LoadError>,
    /// The file's digest (`absent` when there is none): a write from this
    /// state passes it back, and is refused when the file changed since.
    pub version: String,
}

/// What a repository proposes, as an import would copy it.
#[derive(Clone, Debug, Serialize)]
pub struct Proposal {
    pub path: String,
    /// How many rules an import copies (0 when it does not validate).
    pub count: usize,
    /// The rules an import copies (after [`sanitize_proposal`]).
    pub file: Option<RuleFile>,
    pub error: Option<LoadError>,
    /// What the import leaves out or narrows, for the person.
    pub warnings: Vec<String>,
    /// The proposal's digest: an import passes it back, and is refused
    /// when the proposal changed since it was shown.
    pub digest: String,
}

/// One rule file's change marks: modified and changed time (a `chmod` or
/// `chown` moves only the latter), size, inode, mode and owner.
type FileStamp = (SystemTime, i64, i64, u64, u64, u32, u32);

/// When the rule files last changed: a session reloads its rules when this
/// differs at a turn's start.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Stamp(Vec<Option<FileStamp>>);

/// The rules a session loads.
#[derive(Debug, Default)]
pub struct Loaded {
    pub rules: CommandRules,
    pub errors: Vec<LoadError>,
    pub stamp: Stamp,
}

/// The rule files of every workspace and the person's own.
pub struct RuleFiles {
    /// The app config directory (`None`: no user rules).
    user_dir: Option<PathBuf>,
    workspaces: Arc<WorkspaceStore>,
    lock: Mutex<()>,
}

fn io(error: &std::io::Error) -> HostError {
    HostError::Storage(error.to_string())
}

fn stamp_of(path: &Path) -> Option<FileStamp> {
    use std::os::unix::fs::MetadataExt as _;
    let meta = fs::symlink_metadata(path).ok()?;
    Some((
        meta.modified().ok()?,
        meta.ctime(),
        meta.ctime_nsec(),
        meta.len(),
        meta.ino(),
        meta.mode(),
        meta.uid(),
    ))
}

/// The version a panel write must name: the digest of the file's bytes as
/// they are now (`absent` when there is no file).
fn version_of(path: Option<&Path>) -> String {
    match path.map(fs::read) {
        None => "absent".to_owned(),
        Some(Ok(bytes)) => sha256_hex(&bytes),
        Some(Err(error)) if error.kind() == std::io::ErrorKind::NotFound => "absent".to_owned(),
        Some(Err(_)) => "unreadable".to_owned(),
    }
}

/// What an import of `file` (a repository's proposal) may copy: no
/// `host_executables` pins, no allow wider than `workspace-write` or with
/// the network, and every rule marked as the repository's. A rule whose
/// examples relied on the dropped pins is left out. Returns the rules to
/// import and, for the person, what was left out or narrowed.
#[must_use]
pub fn sanitize_proposal(file: RuleFile) -> (RuleFile, Vec<String>) {
    let mut warnings = Vec::new();
    for pin in &file.host_executables {
        warnings.push(format!(
            "The program pin for `{}` ({}) is not imported: pins are set only by you.",
            pin.name,
            pin.paths.join(", ")
        ));
    }
    let mut rules = Vec::new();
    for (position, mut rule) in file.rules.into_iter().enumerate() {
        let index = position + 1;
        let shown = rule.display_pattern();
        if rule.decision == RuleDecision::Allow {
            if rule
                .sandbox
                .is_some_and(|mode| mode > SandboxMode::WorkspaceWrite)
            {
                warnings.push(format!(
                    "Rule {index} (`{shown}`) asks to allow the full-access sandbox; it is imported for the workspace-write sandbox only."
                ));
                rule.sandbox = None;
            }
            if rule.network {
                warnings.push(format!(
                    "Rule {index} (`{shown}`) asks to allow network access; it is imported without it (such calls are asked)."
                ));
                rule.network = false;
            }
        }
        rule.origin = Some(RuleOrigin::Repository);
        let alone = RuleFile {
            rules: vec![rule.clone()],
            ..RuleFile::default()
        };
        if let Err(error) = RuleSet::compile("proposal", alone) {
            warnings.push(format!(
                "Rule {index} (`{shown}`) is not imported: without the repository's program pins {}",
                error.message
            ));
            continue;
        }
        rules.push(rule);
    }
    (
        RuleFile {
            rules,
            ..RuleFile::default()
        },
        warnings,
    )
}

/// Read a private file: `None` when absent; refused (with why) when it is a
/// symlink, not a regular file, another user's, or open to group/others.
fn read_private(path: &Path) -> Result<Option<Vec<u8>>, String> {
    use std::io::Read as _;
    use std::os::unix::fs::MetadataExt as _;
    let file = match crate::credentials_file::open_no_follow(path) {
        Ok(file) => file,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) if error.raw_os_error() == Some(rustix::io::Errno::LOOP.raw_os_error()) => {
            return Err("the rules file is a symlink; it was not used".to_owned());
        }
        Err(error) => return Err(format!("the rules file cannot be read: {error}")),
    };
    let meta = file.metadata().map_err(|e| e.to_string())?;
    if !meta.is_file() {
        return Err("the rules file is not a regular file; it was not used".to_owned());
    }
    if meta.uid() != rustix::process::geteuid().as_raw() {
        return Err("the rules file belongs to another user; it was not used".to_owned());
    }
    if meta.mode() & 0o077 != 0 {
        return Err(format!(
            "the rules file is readable or writable by others (mode {:o}); it was not used — make it 0600",
            meta.mode() & 0o777
        ));
    }
    if meta.len() > MAX_RULES_BYTES {
        return Err("the rules file is larger than 1 MiB; it was not used".to_owned());
    }
    let mut bytes = Vec::new();
    file.take(MAX_RULES_BYTES + 1)
        .read_to_end(&mut bytes)
        .map_err(|e| e.to_string())?;
    Ok(Some(bytes))
}

/// Write `bytes` to `path` atomically: a `0600` temporary file created
/// beside it (in a `0700` directory), then renamed over it.
fn write_private(path: &Path, bytes: &[u8]) -> Result<(), HostError> {
    use std::io::Write as _;
    let dir = path
        .parent()
        .ok_or_else(|| HostError::Internal("a rules file needs a directory".into()))?;
    crate::credentials_file::create_private_dir(dir)?;
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or(RULES_FILE);
    let temp = dir.join(format!(".{name}.{}.tmp", std::process::id()));
    let _ = fs::remove_file(&temp);
    {
        let mut file = crate::credentials_file::create_private_file(&temp).map_err(|e| io(&e))?;
        file.write_all(bytes)
            .and_then(|()| file.sync_all())
            .map_err(|e| io(&e))?;
    }
    fs::rename(&temp, path).map_err(|e| io(&e))
}

fn refused(path: &Path, message: String) -> LoadError {
    LoadError {
        file: path.display().to_string(),
        message,
        ..LoadError::default()
    }
}

impl RuleFiles {
    #[must_use]
    pub fn new(user_dir: Option<PathBuf>, workspaces: Arc<WorkspaceStore>) -> Self {
        Self {
            user_dir,
            workspaces,
            lock: Mutex::new(()),
        }
    }

    fn path(&self, scope: RuleScope, workspace_id: &str) -> Option<PathBuf> {
        match scope {
            RuleScope::User => self.user_dir.as_ref().map(|dir| dir.join(RULES_FILE)),
            RuleScope::Workspace => Some(self.workspaces.data_dir(workspace_id).join(RULES_FILE)),
            // Migrated workspace settings: no file of their own.
            RuleScope::Legacy => None,
        }
    }

    fn guard(&self) -> Result<std::sync::MutexGuard<'_, ()>, HostError> {
        self.lock
            .lock()
            .map_err(|_| HostError::Internal("the rules store is poisoned".into()))
    }

    /// One scope's rules: empty when there is no file, an error (and no
    /// rules) when it is refused or does not validate.
    fn read(&self, scope: RuleScope, workspace_id: &str) -> Result<RuleSet, Box<LoadError>> {
        let Some(path) = self.path(scope, workspace_id) else {
            return Ok(RuleSet::default());
        };
        match read_private(&path) {
            Ok(None) => Ok(RuleSet::default()),
            Ok(Some(bytes)) => RuleSet::parse(&path.display().to_string(), &bytes),
            Err(message) => Err(Box::new(refused(&path, message))),
        }
    }

    /// When `workspace_id`'s rule files last changed.
    #[must_use]
    pub fn stamp(&self, workspace_id: &str) -> Stamp {
        Stamp(
            [RuleScope::Workspace, RuleScope::User]
                .into_iter()
                .map(|scope| {
                    self.path(scope, workspace_id)
                        .and_then(|path| stamp_of(&path))
                })
                .collect(),
        )
    }

    /// The rules a session of `workspace_id` runs with.
    #[must_use]
    pub fn load(&self, workspace_id: &str) -> Loaded {
        let stamp = self.stamp(workspace_id);
        let mut errors = Vec::new();
        let mut take = |scope| {
            self.read(scope, workspace_id).unwrap_or_else(|error| {
                errors.push(*error);
                RuleSet::default()
            })
        };
        let workspace = take(RuleScope::Workspace);
        let user = take(RuleScope::User);
        Loaded {
            rules: CommandRules { workspace, user },
            errors,
            stamp,
        }
    }

    /// Both scopes, as the panel lists them.
    #[must_use]
    pub fn states(&self, workspace_id: &str) -> Vec<ScopeState> {
        [RuleScope::Workspace, RuleScope::User]
            .into_iter()
            .map(|scope| {
                let file_path = self.path(scope, workspace_id);
                let path = file_path
                    .as_ref()
                    .map(|path| path.display().to_string())
                    .unwrap_or_default();
                let version = version_of(file_path.as_deref());
                match self.read(scope, workspace_id) {
                    Ok(set) => ScopeState {
                        scope,
                        path,
                        file: Some(set.file().clone()),
                        error: None,
                        version,
                    },
                    Err(error) => ScopeState {
                        scope,
                        path,
                        file: None,
                        error: Some(*error),
                        version,
                    },
                }
            })
            .collect()
    }

    /// Validate `file` and store it as `scope`'s rules, when the file is
    /// still at `version` (what the panel read); nothing is written when it
    /// does not validate or changed since.
    ///
    /// # Errors
    ///
    /// [`PutError::Invalid`] with the first failing rule;
    /// [`PutError::Conflict`]; storage errors.
    pub fn put(
        &self,
        scope: RuleScope,
        workspace_id: &str,
        file: &serde_json::Value,
        version: &str,
    ) -> Result<RuleSet, PutError> {
        let path = self.path(scope, workspace_id).ok_or_else(|| {
            HostError::Unsupported(format!(
                "there is no {} rules file to write",
                scope.as_str()
            ))
        })?;
        let bytes = serde_json::to_vec(file).map_err(|e| HostError::Internal(e.to_string()))?;
        let set = RuleSet::parse(&path.display().to_string(), &bytes).map_err(PutError::Invalid)?;
        let _guard = self.guard()?;
        if version_of(Some(&path)) != version {
            return Err(PutError::Conflict);
        }
        self.write(&path, &set)?;
        Ok(set)
    }

    fn write(&self, path: &Path, set: &RuleSet) -> Result<(), HostError> {
        let bytes = serde_json::to_vec_pretty(set.file())
            .map_err(|e| HostError::Internal(e.to_string()))?;
        write_private(path, &bytes)
    }

    /// Append `rules` (and the proposal's pins) to the workspace's rules
    /// file, validated as a whole. A refused or broken file is left as it is.
    fn append(
        &self,
        workspace_id: &str,
        rules: Vec<PrefixRule>,
        pins: Vec<elitea_local_tools::execpolicy::HostExecutable>,
    ) -> Result<RuleSet, PutError> {
        let path = self
            .path(RuleScope::Workspace, workspace_id)
            .ok_or_else(|| HostError::Internal("no workspace rules path".into()))?;
        let _guard = self.guard()?;
        let current = self
            .read(RuleScope::Workspace, workspace_id)
            .map_err(PutError::Invalid)?;
        let mut file = current.file().clone();
        for pin in pins {
            if !file.host_executables.contains(&pin) {
                file.host_executables.push(pin);
            }
        }
        for rule in rules {
            // The same prefix again merges its examples into its rule.
            file.add_rule(rule);
        }
        if &file == current.file() {
            return Ok(current);
        }
        let set = RuleSet::compile(&path.display().to_string(), file).map_err(PutError::Invalid)?;
        self.write(&path, &set)?;
        Ok(set)
    }

    /// Move the command choices an older host remembered in `choices.json`
    /// into the workspace's rules file, once: the file choices are written
    /// back, the command choices leave it. Nothing changes while the rules
    /// file is refused or broken (the choices keep applying from
    /// `choices.json`). Returns how many rules were migrated.
    ///
    /// # Errors
    ///
    /// The choices file cannot be read or rewritten.
    pub fn migrate_choices(&self, workspace_id: &str) -> Result<usize, HostError> {
        use std::io::Read as _;
        let path = self.workspaces.data_dir(workspace_id).join(CHOICES_FILE);
        let mut bytes = Vec::new();
        match crate::credentials_file::open_no_follow(&path) {
            Ok(file) => {
                file.take(MAX_RULES_BYTES)
                    .read_to_end(&mut bytes)
                    .map_err(|e| io(&e))?;
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(0),
            Err(error) => return Err(io(&error)),
        }
        let Ok(choices) = serde_json::from_slice::<Vec<RememberedChoice>>(&bytes) else {
            // JsonFileChoices reports it when the session opens.
            return Ok(0);
        };
        let (commands, files): (Vec<_>, Vec<_>) = choices
            .into_iter()
            .partition(|choice| choice_as_rule(choice).is_some());
        if commands.is_empty() {
            return Ok(0);
        }
        let rules: Vec<PrefixRule> = commands.iter().filter_map(choice_as_rule).collect();
        let count = rules.len();
        match self.append(workspace_id, rules, Vec::new()) {
            Ok(_) => {}
            Err(PutError::Invalid(error)) => {
                log::warn!("remembered commands were not migrated into rules: {error}");
                return Ok(0);
            }
            Err(PutError::Conflict) => return Ok(0),
            Err(PutError::Host(error)) => return Err(error),
        }
        let rest =
            serde_json::to_vec_pretty(&files).map_err(|e| HostError::Internal(e.to_string()))?;
        write_private(&path, &rest)?;
        log::info!("migrated {count} remembered command(s) into the workspace's rules");
        Ok(count)
    }

    /// What the folder at `root` proposes in `.elitea/rules.json`, read
    /// confined to the folder (no symlink out of it, `path_deny` honoured),
    /// as an import would copy it ([`sanitize_proposal`]). `None` when it
    /// proposes nothing.
    #[must_use]
    pub fn proposed(root: &Path, path_deny: &[String]) -> Option<Proposal> {
        let display = root.join(PROPOSED_RULES).display().to_string();
        let read = Workspace::open(root, path_deny).and_then(|workspace| {
            let path = workspace.resolve(PROPOSED_RULES, Intent::Read)?;
            workspace.read(&path, MAX_RULES_BYTES)
        });
        let bytes = match read {
            Ok(read) => read.bytes,
            Err(error) if error.code() == ErrorCode::NotFound => return None,
            Err(error) => {
                return Some(Proposal {
                    path: display.clone(),
                    count: 0,
                    file: None,
                    error: Some(refused(Path::new(&display), error.message().to_owned())),
                    warnings: Vec::new(),
                    digest: String::new(),
                });
            }
        };
        let digest = sha256_hex(&bytes);
        Some(match RuleSet::parse(&display, &bytes) {
            Ok(set) => {
                let (file, warnings) = sanitize_proposal(set.file().clone());
                Proposal {
                    path: display,
                    count: file.rules.len(),
                    file: Some(file),
                    error: None,
                    warnings,
                    digest,
                }
            }
            Err(error) => Proposal {
                path: display,
                count: 0,
                file: None,
                error: Some(*error),
                warnings: Vec::new(),
                digest,
            },
        })
    }

    /// Copy the folder's proposed rules, as [`Self::proposed`] shows them
    /// (no pins, bounded, marked as the repository's), into the workspace
    /// rules file: the person's explicit import of the proposal whose
    /// digest is `digest`.
    ///
    /// # Errors
    ///
    /// Nothing is proposed, the proposal does not validate,
    /// [`PutError::Conflict`] when it changed since it was shown, or the
    /// workspace's rules file is broken or cannot be written.
    pub fn import_proposed(
        &self,
        workspace_id: &str,
        root: &Path,
        path_deny: &[String],
        digest: &str,
    ) -> Result<RuleSet, PutError> {
        let proposal = Self::proposed(root, path_deny)
            .ok_or_else(|| HostError::Unsupported("this folder proposes no rules".into()))?;
        if let Some(error) = proposal.error {
            return Err(PutError::Invalid(Box::new(error)));
        }
        if proposal.digest != digest {
            return Err(PutError::Conflict);
        }
        let file = proposal.file.unwrap_or_default();
        self.append(workspace_id, file.rules, Vec::new())
    }
}

/// What a write through IPC did: saved, or refused with the validation
/// error, or refused because the file changed since it was read
/// (`conflict`; nothing written either way).
#[derive(Debug, Serialize)]
pub struct PutOutcome {
    pub saved: bool,
    pub file: Option<RuleFile>,
    pub error: Option<LoadError>,
    pub conflict: bool,
}

impl PutOutcome {
    /// # Errors
    ///
    /// Storage failures (a validation failure is an outcome, not an error).
    pub fn of(result: Result<RuleSet, PutError>) -> Result<Self, HostError> {
        match result {
            Ok(set) => Ok(Self {
                saved: true,
                file: Some(set.file().clone()),
                error: None,
                conflict: false,
            }),
            Err(PutError::Invalid(error)) => Ok(Self {
                saved: false,
                file: None,
                error: Some(*error),
                conflict: false,
            }),
            Err(PutError::Conflict) => Ok(Self {
                saved: false,
                file: None,
                error: None,
                conflict: true,
            }),
            Err(PutError::Host(error)) => Err(error),
        }
    }
}

/// `rules_get`: both files and what the folder proposes.
#[derive(Debug, Serialize)]
pub struct RulesView {
    pub scopes: Vec<ScopeState>,
    pub proposed: Option<Proposal>,
}

/// "Always allow" answers of one workspace, written as prefix rules.
pub struct WorkspaceRuleSink {
    files: Arc<RuleFiles>,
    workspace_id: String,
}

impl WorkspaceRuleSink {
    #[must_use]
    pub fn new(files: Arc<RuleFiles>, workspace_id: &str) -> Self {
        Self {
            files,
            workspace_id: workspace_id.to_owned(),
        }
    }
}

impl RuleSink for WorkspaceRuleSink {
    fn append(&self, rule: PrefixRule) -> ToolResult<RuleSet> {
        self.files
            .append(&self.workspace_id, vec![rule], Vec::new())
            .map_err(|error| match error {
                PutError::Invalid(error) => {
                    ToolError::new(ErrorCode::InvalidArgument, error.to_string())
                }
                PutError::Conflict => ToolError::new(ErrorCode::Io, "the rules file changed"),
                PutError::Host(error) => ToolError::new(ErrorCode::Io, error.to_string()),
            })
    }
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt as _;

    use super::*;

    fn store() -> (tempfile::TempDir, Arc<WorkspaceStore>, String, RuleFiles) {
        let app = tempfile::tempdir().unwrap();
        let folder = app.path().join("folder");
        fs::create_dir_all(&folder).unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().join("data")));
        let id = workspaces.add(&folder).unwrap().id;
        let files = RuleFiles::new(Some(app.path().join("config")), workspaces.clone());
        (app, workspaces, id, files)
    }

    fn write(path: &Path, text: &str, mode: u32) {
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, text).unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(mode)).unwrap();
    }

    const CARGO: &str = r#"{"version":1,"rules":[{"pattern":["cargo",["test","check"]]}]}"#;

    #[test]
    fn both_files_load_and_are_written_owner_only() {
        let (app, workspaces, id, files) = store();
        assert!(files.load(&id).rules.workspace.is_empty());
        write(&app.path().join("config/rules.json"), CARGO, 0o600);
        let set = files
            .put(
                RuleScope::Workspace,
                &id,
                &serde_json::json!({"version":1,"rules":[{"pattern":["npm","publish"],"decision":"forbidden"}]}),
                "absent",
            )
            .unwrap();
        assert_eq!(set.rules().len(), 1);
        let path = workspaces.data_dir(&id).join(RULES_FILE);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(
            fs::metadata(workspaces.data_dir(&id))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o700
        );
        let loaded = files.load(&id);
        assert!(loaded.errors.is_empty(), "{:?}", loaded.errors);
        assert_eq!(loaded.rules.workspace.rules().len(), 1);
        assert_eq!(loaded.rules.user.rules().len(), 1);
    }

    #[test]
    fn loose_symlinked_or_invalid_files_load_nothing() {
        let (app, _workspaces, id, files) = store();
        let user = app.path().join("config/rules.json");
        write(&user, CARGO, 0o644);
        let loaded = files.load(&id);
        assert!(loaded.rules.user.is_empty());
        assert!(
            loaded.errors[0]
                .message
                .contains("readable or writable by others"),
            "{:?}",
            loaded.errors
        );

        fs::remove_file(&user).unwrap();
        let target = app.path().join("elsewhere.json");
        write(&target, CARGO, 0o600);
        std::os::unix::fs::symlink(&target, &user).unwrap();
        let loaded = files.load(&id);
        assert!(loaded.rules.user.is_empty());
        assert!(
            loaded.errors[0].message.contains("symlink"),
            "{:?}",
            loaded.errors
        );

        fs::remove_file(&user).unwrap();
        write(
            &user,
            r#"{"version":1,"rules":[{"pattern":["cargo"]},{"pattern":["git","push"],"match":["git status"]}]}"#,
            0o600,
        );
        let loaded = files.load(&id);
        assert!(
            loaded.rules.user.is_empty(),
            "a broken file never half-loads"
        );
        assert_eq!(loaded.errors[0].rule_index, Some(2));
        assert_eq!(loaded.errors[0].example.as_deref(), Some("git status"));
        // A put that does not validate writes nothing.
        let before = fs::read(&user).unwrap();
        let bad =
            serde_json::json!({"version":1,"rules":[{"pattern":["ls"],"not_match":["ls -la"]}]});
        let version = files.states(&id)[1].version.clone();
        assert!(matches!(
            files.put(RuleScope::User, &id, &bad, &version),
            Err(PutError::Invalid(_))
        ));
        let unknown =
            serde_json::json!({"version":1,"rules":[{"pattern":["ls"],"verdict":"allow"}]});
        let Err(PutError::Invalid(error)) = files.put(RuleScope::User, &id, &unknown, &version)
        else {
            panic!("an unknown field is refused");
        };
        assert!(error.message.contains("verdict"));
        assert_eq!(fs::read(&user).unwrap(), before);
    }

    #[test]
    fn remembered_command_choices_migrate_once() {
        let (_app, workspaces, id, files) = store();
        let choices = workspaces.data_dir(&id).join(CHOICES_FILE);
        write(
            &choices,
            r#"[{"tool":"run_command","command":["/usr/bin/make","test"],"sandbox":"read-only","network":false},
                {"tool":"write_file","paths":["docs/a.md"]}]"#,
            0o644,
        );
        assert_eq!(files.migrate_choices(&id).unwrap(), 1);
        let rules = files.load(&id).rules.workspace;
        assert_eq!(rules.rules()[0].display_pattern(), "/usr/bin/make test");
        let left: Vec<RememberedChoice> =
            serde_json::from_slice(&fs::read(&choices).unwrap()).unwrap();
        assert_eq!(left.len(), 1, "file choices stay in choices.json");
        assert_eq!(
            left[0].tool,
            elitea_local_tools::approvals::ToolKind::WriteFile
        );
        assert_eq!(files.migrate_choices(&id).unwrap(), 0, "once");
        assert_eq!(files.load(&id).rules.workspace.rules().len(), 1);
    }

    #[test]
    fn proposals_are_read_confined_and_imported_only_on_request() {
        let (app, _workspaces, id, files) = store();
        let folder = app.path().join("folder");
        assert!(RuleFiles::proposed(&folder, &[]).is_none());
        write(
            &folder.join(PROPOSED_RULES),
            r#"{"version":1,"host_executables":[{"name":"git","paths":["/usr/bin/git"]}],
               "rules":[{"pattern":["git","status"]},{"pattern":["rm"],"decision":"forbidden"}]}"#,
            0o644,
        );
        let proposal = RuleFiles::proposed(&folder, &[]).unwrap();
        assert_eq!(proposal.count, 2);
        assert!(
            files.load(&id).rules.workspace.is_empty(),
            "never loaded by itself"
        );
        assert!(
            matches!(
                files.import_proposed(&id, &folder, &[], "stale"),
                Err(PutError::Conflict)
            ),
            "an import names the proposal it was shown"
        );
        let set = files
            .import_proposed(&id, &folder, &[], &proposal.digest)
            .unwrap();
        assert_eq!(set.rules().len(), 2);
        assert!(
            set.file().host_executables.is_empty(),
            "a repository's pins are never imported"
        );
        assert!(
            set.rules()
                .iter()
                .all(|rule| rule.origin == Some(RuleOrigin::Repository))
        );
        assert_eq!(files.load(&id).rules.workspace.rules().len(), 2);
        // A proposal reached through a symlink out of the folder is refused.
        fs::remove_file(folder.join(PROPOSED_RULES)).unwrap();
        let outside = app.path().join("outside.json");
        write(&outside, CARGO, 0o644);
        std::os::unix::fs::symlink(&outside, folder.join(PROPOSED_RULES)).unwrap();
        let proposal = RuleFiles::proposed(&folder, &[]).unwrap();
        assert!(proposal.error.is_some() && proposal.count == 0);
    }

    /// `chmod` changes only the ctime: fixing a refused file's mode reloads it.
    #[test]
    fn a_permission_fix_changes_the_stamp() {
        let (app, _workspaces, id, files) = store();
        let user = app.path().join("config/rules.json");
        write(&user, CARGO, 0o644);
        let before = files.stamp(&id);
        assert!(files.load(&id).rules.user.is_empty(), "refused at 0644");
        fs::set_permissions(&user, fs::Permissions::from_mode(0o600)).unwrap();
        assert_ne!(files.stamp(&id), before, "a chmod is a change");
        assert_eq!(files.load(&id).rules.user.rules().len(), 1);
    }

    /// A save made from a stale read is refused, and nothing is written.
    #[test]
    fn a_stale_write_is_a_conflict() {
        let (_app, _workspaces, id, files) = store();
        let first = files.states(&id)[0].version.clone();
        assert_eq!(first, "absent");
        let one = serde_json::json!({"version":1,"rules":[{"pattern":["ls"]}]});
        files.put(RuleScope::Workspace, &id, &one, &first).unwrap();
        let two = serde_json::json!({"version":1,"rules":[{"pattern":["pwd"]}]});
        assert!(matches!(
            files.put(RuleScope::Workspace, &id, &two, &first),
            Err(PutError::Conflict)
        ));
        assert_eq!(
            files.load(&id).rules.workspace.rules()[0].display_pattern(),
            "ls"
        );
        let now = files.states(&id)[0].version.clone();
        assert_ne!(now, first);
        files.put(RuleScope::Workspace, &id, &two, &now).unwrap();
        let outcome = PutOutcome::of(files.put(RuleScope::Workspace, &id, &one, &now)).unwrap();
        assert!(!outcome.saved && outcome.conflict);
    }

    /// An import never grants more than a workspace-write, offline allow,
    /// never pins programs, and says what it left out.
    #[test]
    fn an_import_is_bounded_and_says_what_it_left_out() {
        let (app, _workspaces, id, files) = store();
        let folder = app.path().join("folder");
        let inside = folder.join("bin/git").display().to_string();
        write(
            &folder.join(PROPOSED_RULES),
            &format!(
                r#"{{"version":1,"host_executables":[{{"name":"git","paths":["{inside}"]}}],
                   "rules":[
                     {{"pattern":["curl"],"sandbox":"full-access","network":true,"justification":"Fetch."}},
                     {{"pattern":["git","status"],"match":["{inside} status"],"not_match":["/tmp/x/git status"]}},
                     {{"pattern":["rm"],"decision":"forbidden","network":true}}]}}"#
            ),
            0o644,
        );
        let proposal = RuleFiles::proposed(&folder, &[]).unwrap();
        let file = proposal.file.clone().unwrap();
        assert!(file.host_executables.is_empty());
        assert_eq!(
            proposal.count, 2,
            "the rule that needed the pin is left out"
        );
        assert_eq!(file.rules[0].sandbox, None);
        assert!(!file.rules[0].network);
        assert_eq!(file.rules[0].justification.as_deref(), Some("Fetch."));
        assert_eq!(file.rules[1].decision, RuleDecision::Forbidden);
        let warnings = proposal.warnings.join("\n");
        for expected in ["pin for `git`", "full-access", "network access", "Rule 2"] {
            assert!(warnings.contains(expected), "{expected}: {warnings}");
        }
        let set = files
            .import_proposed(&id, &folder, &[], &proposal.digest)
            .unwrap();
        assert_eq!(set.rules().len(), 2);
        assert!(set.file().host_executables.is_empty());
        assert!(!set.rules()[0].network && set.rules()[0].sandbox.is_none());
    }
}
