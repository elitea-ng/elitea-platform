//! The Doctor: Help ▸ Run Diagnostics… and Settings ▸ Troubleshoot
//! (IPC.md, "Diagnostics").
//!
//! `doctor_run` checks what the app keeps on this computer and what it needs
//! from the deployment, and names a repair for what it can repair;
//! `doctor_fix` applies one. Nothing is repaired behind the person's back:
//! a file the app will not trust (a symlink, another user's, one others can
//! write, one that does not parse) is never read as data and never silently
//! overwritten — the Doctor offers to MOVE IT ASIDE (`<name>.broken-<time>`,
//! the entry itself, never a symlink's target) so the app can start afresh.
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
use crate::history;
use crate::workspaces::WorkspaceStore;

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
    /// `history`, `workspaces`, `deployment`, `session`, `local_work`,
    /// `pending_revokes`.
    pub id: &'static str,
    pub title: &'static str,
    pub status: Status,
    /// For a person; never a secret.
    pub message: String,
    /// The repair `doctor_fix` takes, when there is one.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fix_id: Option<&'static str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fix_label: Option<&'static str>,
}

impl Check {
    fn new(id: &'static str, title: &'static str, status: Status, message: String) -> Self {
        Self {
            id,
            title,
            status,
            message,
            fix_id: None,
            fix_label: None,
        }
    }

    fn fix(mut self, fix_id: &'static str, label: &'static str) -> Self {
        self.fix_id = Some(fix_id);
        self.fix_label = Some(label);
        self
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

/// Rename `path` (the entry itself: a symlink is moved, never followed) to
/// `<name>.broken-<time>` next to it; the new path.
fn move_aside(path: &Path) -> Result<Option<PathBuf>, HostError> {
    if fs::symlink_metadata(path).is_err() {
        return Ok(None);
    }
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let stamp = chrono::Utc::now().format("%Y%m%dT%H%M%SZ");
    let aside = path.with_file_name(format!("{name}.broken-{stamp}"));
    fs::rename(path, &aside)
        .map_err(|e| HostError::Storage(format!("could not move {} aside: {e}", path.display())))?;
    log::warn!(
        "diagnostics: moved {} aside to {}",
        path.display(),
        aside.display()
    );
    Ok(Some(aside))
}

/// The checks and repairs on this computer's files (no network).
pub struct LocalDoctor {
    pub config_dir: PathBuf,
    pub data_dir: PathBuf,
    pub log_dir: Option<PathBuf>,
    pub credentials: Arc<CredentialsFile>,
    pub workspaces: Arc<WorkspaceStore>,
    /// The thread history opened at launch.
    pub history_open: bool,
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
                        "{} {why}, so the app runs without a history. Move it aside, then restart Elitea.",
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
            Ok(_) if !self.history_open => Check::new(
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
                    "The history is damaged ({error}). Move it aside, then restart Elitea; the threads on the server are not affected."
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
        let missing: Vec<String> = all
            .iter()
            .filter(|w| fs::read_dir(&w.path).is_err())
            .map(|w| w.name.clone())
            .collect();
        if missing.is_empty() {
            return Check::new(
                ID,
                TITLE,
                Status::Ok,
                format!("{} folder(s), all reachable.", all.len()),
            );
        }
        Check::new(
            ID,
            TITLE,
            Status::Warn,
            format!(
                "These folders are gone or unreadable: {}.",
                missing.join(", ")
            ),
        )
        .fix("workspaces.drop_missing", "Remove them from the list")
    }

    /// Apply one repair; what happened, for a person.
    ///
    /// # Errors
    ///
    /// An unknown repair, or the repair failed.
    pub fn fix(&self, fix_id: &str) -> Result<String, HostError> {
        match fix_id {
            "credentials.tighten" => {
                tighten_file(&self.credentials_path())?;
                self.credentials.forget_cache();
                Ok("The stored sign-in is now readable by you only.".into())
            }
            "credentials.move_aside" => {
                let aside = move_aside(&self.credentials_path())?;
                self.credentials.forget_cache();
                Ok(match aside {
                    Some(aside) => {
                        format!("Moved to {}. Sign in again to continue.", aside.display())
                    }
                    None => "There was nothing to move.".into(),
                })
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
            "history.move_aside" => {
                let path = self.history_path();
                for suffix in ["-wal", "-shm"] {
                    move_aside(&path.with_file_name(format!("{}{suffix}", history::FILE_NAME)))?;
                }
                let aside = move_aside(&path)?;
                Ok(match aside {
                    Some(aside) => format!(
                        "Moved to {}. Restart Elitea to start a new history.",
                        aside.display()
                    ),
                    None => "There was nothing to move.".into(),
                })
            }
            "workspaces.drop_missing" => {
                let dropped = self.workspaces.drop_unreachable()?;
                log::info!(
                    "diagnostics: removed {} unreachable workspace(s) from the list",
                    dropped.len()
                );
                Ok(format!(
                    "Removed {} folder(s) from the list.",
                    dropped.len()
                ))
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
    /// An unknown repair, or the repair failed.
    pub async fn fix(&self, fix_id: &str) -> Result<String, HostError> {
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
        self.local.fix(fix_id)
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

/// `doctor_fix`: apply the repair a check named.
#[tauri::command(rename_all = "snake_case")]
pub async fn doctor_fix(
    doctor: tauri::State<'_, Arc<Doctor>>,
    fix_id: String,
) -> Result<FixOutcome, HostError> {
    log::info!("diagnostics: applying `{fix_id}`");
    doctor
        .fix(&fix_id)
        .await
        .map(|message| FixOutcome { message })
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Fixture {
        _root: tempfile::TempDir,
        doctor: LocalDoctor,
    }

    fn fixture() -> Fixture {
        let root = tempfile::tempdir().unwrap();
        let config_dir = root.path().join("config");
        let data_dir = root.path().join("data");
        let log_dir = root.path().join("logs");
        for dir in [&config_dir, &data_dir, &log_dir] {
            credentials_file::create_private_dir(dir).unwrap();
        }
        let doctor = LocalDoctor {
            credentials: CredentialsFile::new(config_dir.clone()),
            workspaces: Arc::new(WorkspaceStore::new(data_dir.clone())),
            config_dir,
            data_dir,
            log_dir: Some(log_dir),
            history_open: true,
        };
        Fixture {
            _root: root,
            doctor,
        }
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
        assert_eq!(c.fix_id, Some("credentials.tighten"));
        f.doctor.fix("credentials.tighten").unwrap();
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(check(&f.doctor.checks(), "credentials").status, Status::Ok);
    }

    #[cfg(unix)]
    #[test]
    fn an_untrusted_credentials_file_is_moved_aside_and_sign_in_works_again() {
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
        let doctor = LocalDoctor {
            credentials: launch.clone(),
            ..f.doctor
        };
        let c = check(&doctor.checks(), "credentials").clone();
        assert_eq!(c.status, Status::Fail);
        assert_eq!(c.fix_id, Some("credentials.move_aside"));
        doctor.fix("credentials.move_aside").unwrap();
        // The original is kept aside, untouched; sign-in works.
        let aside: Vec<_> = fs::read_dir(&doctor.config_dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|n| n.starts_with("credentials.json.broken-"))
            .collect();
        assert_eq!(aside.len(), 1);
        launch.slot("device-session").save("new").unwrap();
        assert_eq!(check(&doctor.checks(), "credentials").status, Status::Ok);
    }

    #[cfg(unix)]
    #[test]
    fn a_symlinked_credentials_file_is_moved_aside_never_followed() {
        let f = fixture();
        let target = f.doctor.config_dir.parent().unwrap().join("elsewhere");
        fs::write(&target, "{}").unwrap();
        std::os::unix::fs::symlink(&target, f.doctor.credentials.path()).unwrap();
        assert_eq!(
            check(&f.doctor.checks(), "credentials").status,
            Status::Fail
        );
        f.doctor.fix("credentials.move_aside").unwrap();
        assert!(fs::symlink_metadata(f.doctor.credentials.path()).is_err());
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
        assert_eq!(c.fix_id, Some("credentials.move_aside"));
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
            (c.status, c.fix_id),
            (Status::Warn, Some("dir.tighten.logs"))
        );
        f.doctor.fix("dir.tighten.logs").unwrap();
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
            (c.status, c.fix_id),
            (Status::Fail, Some("history.move_aside")),
            "{c:?}"
        );
        f.doctor.fix("history.move_aside").unwrap();
        assert!(!path.exists());
        assert_eq!(check(&f.doctor.checks(), "history").status, Status::Ok);
    }

    #[test]
    fn a_healthy_history_passes_its_integrity_check() {
        let f = fixture();
        drop(history::HistoryStore::open(&f.doctor.data_dir).unwrap());
        assert_eq!(check(&f.doctor.checks(), "history").status, Status::Ok);
        let closed = LocalDoctor {
            history_open: false,
            ..f.doctor
        };
        assert_eq!(check(&closed.checks(), "history").status, Status::Warn);
    }

    #[test]
    fn missing_workspace_folders_are_dropped_on_request() {
        let f = fixture();
        let kept = tempfile::tempdir().unwrap();
        let gone = tempfile::tempdir().unwrap();
        f.doctor.workspaces.add(kept.path()).unwrap();
        f.doctor.workspaces.add(gone.path()).unwrap();
        drop(gone);
        let c = check(&f.doctor.checks(), "workspaces").clone();
        assert_eq!(
            (c.status, c.fix_id),
            (Status::Warn, Some("workspaces.drop_missing"))
        );
        f.doctor.fix("workspaces.drop_missing").unwrap();
        assert_eq!(f.doctor.workspaces.all().unwrap().len(), 1);
        assert_eq!(check(&f.doctor.checks(), "workspaces").status, Status::Ok);
    }

    #[test]
    fn a_damaged_workspaces_file_is_moved_aside() {
        let f = fixture();
        fs::write(f.doctor.workspaces.file_path(), "[{").unwrap();
        let c = check(&f.doctor.checks(), "workspaces").clone();
        assert_eq!(
            (c.status, c.fix_id),
            (Status::Fail, Some("workspaces.move_aside"))
        );
        f.doctor.fix("workspaces.move_aside").unwrap();
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

    #[test]
    fn an_unknown_repair_is_refused() {
        assert!(fixture().doctor.fix("rm -rf").is_err());
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
