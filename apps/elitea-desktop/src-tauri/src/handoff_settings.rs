//! The handoff's effective policy before begin (contract §6.1), the
//! per-folder handoff settings (ADR-0034 decision 5, amended 2026-10-15)
//! and the env harvest.
//!
//! * **Policy.** `GET workspace_handoff_policy/prompt_lib/{project}` gives
//!   the caps, the quota left, the exclusions (the platform's then the
//!   project's, merged), `path_deny`, `allow_user_include` and the env
//!   policy. A server without the route (404) gets the ADR's defaults,
//!   marked `source: "defaults"`.
//! * **Folder settings.** The person's exclusion patterns, explicit
//!   includes and selected env variable names, per workspace in the app
//!   data (`handoff/settings/<workspace id>.json`, owner-only), never in
//!   the folder. Env values are never stored: they are read again at send.
//! * **Env harvest.** The person's login-shell environment (where
//!   variables like `DATABASE_URL` live), read once per preview, settings
//!   read or send the way editors read it
//!   ([`elitea_local_tools::shell::login_environment`]) and merged over the
//!   desktop process's own; filtered by `env_deny` and `env_allow`, bounded
//!   by `env_limits`. Values are never written anywhere. What local
//!   `run_command` passes to commands is not changed by any of this.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Mutex;
use std::time::Duration;

use async_trait::async_trait;
use elitea_local_tools::shell::LoginEnvironment;

use elitea_local_tools::handoff::caps::Caps;
use elitea_local_tools::handoff::exclude::{self, MANDATORY, Patterns};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::handoff::HandoffError;

/// The platform's default env deny list (ADR-0034 decision 5).
pub const ENV_DENY_DEFAULT: &[&str] = &[
    "PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "TMPDIR", "TERM*", "LANG", "LC_*",
    "SSH_*", "GPG_*", "XPC_*", "__CF*", "DISPLAY", "ELITEA_*",
];

/// How long the login shell may take to print its environment.
pub const LOGIN_SHELL_TIMEOUT: Duration = Duration::from_secs(10);

/// Where the env harvest reads variables from.
#[async_trait]
pub trait EnvSource: Send + Sync {
    /// The variables now (and a notice when some source could not be read).
    async fn environment(&self) -> LoginEnvironment;
}

/// The person's login shell (`$SHELL -l -i -c`, or `shell` in tests),
/// merged over this process's environment. A shell that fails or times out
/// gives no variables and a notice, never an error.
pub struct LoginShellEnv {
    /// `None`: `$SHELL`, else `/bin/zsh` (macOS) or `/bin/bash`.
    pub shell: Option<PathBuf>,
    pub timeout: Duration,
}

#[async_trait]
impl EnvSource for LoginShellEnv {
    async fn environment(&self) -> LoginEnvironment {
        let shell = self
            .shell
            .clone()
            .unwrap_or_else(elitea_local_tools::shell::login_shell);
        let login = elitea_local_tools::shell::login_environment(&shell, self.timeout).await;
        if login.notice.is_some() {
            return login;
        }
        let mut merged: BTreeMap<String, String> = std::env::vars().collect();
        merged.extend(login.vars);
        LoginEnvironment {
            vars: merged.into_iter().collect(),
            notice: None,
        }
    }
}

/// A fixed environment (tests).
#[cfg(test)]
pub struct FixedEnv(pub Vec<(String, String)>);

#[cfg(test)]
#[async_trait]
impl EnvSource for FixedEnv {
    async fn environment(&self) -> LoginEnvironment {
        LoginEnvironment {
            vars: self.0.clone(),
            notice: None,
        }
    }
}

/// The most entries of one settings list.
const MAX_ENTRIES: usize = 500;

/// `env_limits` (contract §6.1).
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct EnvLimits {
    pub max_vars: usize,
    pub max_bytes: usize,
}

impl Default for EnvLimits {
    fn default() -> Self {
        Self {
            max_vars: 200,
            max_bytes: 64 * 1024,
        }
    }
}

/// The effective handoff policy of one project, as the desktop uses it.
#[derive(Clone, Debug, PartialEq)]
pub struct EffectivePolicy {
    /// `server`, or `defaults` when the server has no policy route.
    pub source: &'static str,
    pub enabled: bool,
    /// Why `enabled` is false, as the server names it (`platform_off`,
    /// `project_off`, `device_off`, `no_executor`,
    /// `storage_encryption_unconfirmed`); `None` when enabled or when the
    /// server predates the field.
    pub disabled_reason: Option<String>,
    /// Platform and project patterns (`user` empty: the folder's own).
    pub patterns: Patterns,
    pub path_deny: Vec<String>,
    pub allow_user_include: bool,
    pub env_harvest: bool,
    pub env_deny: Vec<String>,
    pub env_allow: Option<Vec<String>>,
    pub env_limits: EnvLimits,
    pub limits: Caps,
}

fn strings(value: Option<&Value>) -> Option<Vec<String>> {
    value?.as_array().map(|items| {
        items
            .iter()
            .filter_map(Value::as_str)
            .map(str::to_owned)
            .collect()
    })
}

impl EffectivePolicy {
    /// The ADR's defaults (a server without the policy route).
    #[must_use]
    pub fn defaults() -> Self {
        Self {
            source: "defaults",
            enabled: true,
            disabled_reason: None,
            patterns: Patterns::default(),
            path_deny: Vec::new(),
            allow_user_include: true,
            env_harvest: false,
            env_deny: ENV_DENY_DEFAULT.iter().map(|s| (*s).to_owned()).collect(),
            env_allow: None,
            env_limits: EnvLimits::default(),
            limits: Caps::default(),
        }
    }

    /// The server's answer. The merged `exclusions` are split into the
    /// platform's and the project's by `exclusions_platform` /
    /// `exclusions_project` when the server sends them; otherwise a pattern
    /// of the ADR's default list counts as the platform's and any other as
    /// the project's (the contract's list is merged). Absent switches are
    /// off.
    #[must_use]
    pub fn from_server(body: &Value) -> Self {
        let defaults = Self::defaults();
        let merged = strings(body.get("exclusions")).unwrap_or_default();
        let patterns = match (
            strings(body.get("exclusions_platform")),
            strings(body.get("exclusions_project")),
        ) {
            (Some(platform), Some(project)) => Patterns {
                platform,
                project,
                user: Vec::new(),
            },
            _ => {
                let (platform, project) = merged
                    .into_iter()
                    .partition(|pattern| MANDATORY.contains(&pattern.as_str()));
                Patterns {
                    platform,
                    project,
                    user: Vec::new(),
                }
            }
        };
        let flag = |key: &str| body.get(key).and_then(Value::as_bool).unwrap_or(false);
        let limits: Caps = body
            .get("limits")
            .and_then(|limits| serde_json::from_value(limits.clone()).ok())
            .unwrap_or(defaults.limits);
        Self {
            source: "server",
            enabled: body.get("enabled").and_then(Value::as_bool).unwrap_or(true),
            disabled_reason: body
                .get("disabled_reason")
                .and_then(Value::as_str)
                .filter(|reason| !reason.is_empty())
                .map(str::to_owned),
            patterns,
            path_deny: strings(body.get("path_deny")).unwrap_or_default(),
            allow_user_include: flag("allow_user_include"),
            env_harvest: flag("env_harvest"),
            env_deny: strings(body.get("env_deny")).unwrap_or(defaults.env_deny),
            env_allow: strings(body.get("env_allow")),
            env_limits: body
                .get("env_limits")
                .and_then(|limits| serde_json::from_value(limits.clone()).ok())
                .unwrap_or_default(),
            limits,
        }
    }

    /// `HandoffPolicy` for the UI (IPC.md), with the folder's patterns and
    /// the device's `path_deny` merged in.
    #[must_use]
    pub fn wire(&self, user: &[String], path_deny: &[String]) -> Value {
        json!({
            "source": self.source,
            "exclusions": {
                "platform": self.patterns.platform,
                "project": self.patterns.project,
                "user": user,
                "path_deny": path_deny,
            },
            "allow_user_include": self.allow_user_include,
            "env_harvest": self.env_harvest,
            "env_limits": self.env_limits,
            "limits": self.limits,
        })
    }
}

/// One folder's handoff settings (never values).
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct FolderSettings {
    #[serde(default)]
    pub exclude_patterns: Vec<String>,
    #[serde(default)]
    pub include: Vec<String>,
    #[serde(default)]
    pub env_names: Vec<String>,
}

fn invalid(message: impl Into<String>) -> HandoffError {
    HandoffError::new("invalid_request", message)
}

/// A variable name as a shell knows one.
#[must_use]
pub fn valid_env_name(name: &str) -> bool {
    let mut chars = name.chars();
    chars
        .next()
        .is_some_and(|c| c.is_ascii_alphabetic() || c == '_')
        && chars.all(|c| c.is_ascii_alphanumeric() || c == '_')
        && name.len() <= 256
}

fn dedupe(items: Vec<String>) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    for item in items {
        if !out.contains(&item) {
            out.push(item);
        }
    }
    out
}

impl FolderSettings {
    /// Trimmed, deduplicated and checked.
    ///
    /// # Errors
    ///
    /// `invalid_request` for a pattern that is not one, an include that
    /// leaves the folder, a name that is not a variable name, or too many
    /// entries.
    pub fn validated(self) -> Result<Self, HandoffError> {
        let clean = |items: Vec<String>| -> Vec<String> {
            dedupe(
                items
                    .into_iter()
                    .map(|item| item.trim().to_owned())
                    .filter(|item| !item.is_empty())
                    .collect(),
            )
        };
        let settings = Self {
            exclude_patterns: clean(self.exclude_patterns),
            include: clean(self.include)
                .into_iter()
                .map(|item| item.trim_start_matches("./").to_owned())
                .collect(),
            env_names: clean(self.env_names),
        };
        if settings.exclude_patterns.len() > MAX_ENTRIES
            || settings.include.len() > MAX_ENTRIES
            || settings.env_names.len() > MAX_ENTRIES
        {
            return Err(invalid("A list of the handoff settings is too long."));
        }
        for pattern in settings.exclude_patterns.iter().chain(&settings.include) {
            exclude::check_pattern(pattern).map_err(|e| invalid(e.message()))?;
        }
        for include in &settings.include {
            if include.starts_with('/') && include.len() == 1 {
                return Err(invalid("`/` is not a file of the folder."));
            }
        }
        if let Some(bad) = settings.env_names.iter().find(|n| !valid_env_name(n)) {
            return Err(invalid(format!(
                "`{bad}` is not an environment variable name."
            )));
        }
        Ok(settings)
    }
}

/// The settings files.
pub struct SettingsStore {
    dir: PathBuf,
    lock: Mutex<()>,
}

impl SettingsStore {
    #[must_use]
    pub fn new(dir: PathBuf) -> Self {
        Self {
            dir,
            lock: Mutex::new(()),
        }
    }

    fn path(&self, workspace_id: &str) -> Result<PathBuf, HandoffError> {
        if workspace_id.is_empty()
            || !workspace_id
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
        {
            return Err(invalid("not a workspace id"));
        }
        Ok(self.dir.join(format!("{workspace_id}.json")))
    }

    /// The folder's settings (empty when none were saved).
    ///
    /// # Errors
    ///
    /// The file cannot be read.
    pub fn get(&self, workspace_id: &str) -> Result<FolderSettings, HandoffError> {
        let path = self.path(workspace_id)?;
        let _guard = self
            .lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        match std::fs::read(&path) {
            Ok(bytes) => serde_json::from_slice(&bytes)
                .map_err(|_| HandoffError::new("storage", "the handoff settings are unreadable")),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                Ok(FolderSettings::default())
            }
            Err(error) => Err(HandoffError::new(
                "storage",
                format!("cannot read the handoff settings: {error}"),
            )),
        }
    }

    /// Store `settings` (already validated).
    ///
    /// # Errors
    ///
    /// The file cannot be written.
    pub fn put(&self, workspace_id: &str, settings: &FolderSettings) -> Result<(), HandoffError> {
        let path = self.path(workspace_id)?;
        let _guard = self
            .lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let storage = |e: &dyn std::fmt::Display| {
            HandoffError::new("storage", format!("cannot store the handoff settings: {e}"))
        };
        crate::credentials_file::create_private_dir(&self.dir).map_err(|e| storage(&e))?;
        let bytes = serde_json::to_vec(settings).map_err(|e| storage(&e))?;
        let temp = self
            .dir
            .join(format!(".{workspace_id}.{}.tmp", std::process::id()));
        let _ = std::fs::remove_file(&temp);
        {
            use std::io::Write as _;
            let mut file =
                crate::credentials_file::create_private_file(&temp).map_err(|e| storage(&e))?;
            file.write_all(&bytes).map_err(|e| storage(&e))?;
            file.sync_all().map_err(|e| storage(&e))?;
        }
        std::fs::rename(&temp, path).map_err(|e| storage(&e))
    }
}

/// `name` matches a deny/allow pattern (`*` matches any run of characters).
#[must_use]
pub fn name_matches(pattern: &str, name: &str) -> bool {
    let parts: Vec<&str> = pattern.split('*').collect();
    if parts.len() == 1 {
        return pattern == name;
    }
    let mut rest = name;
    for (index, part) in parts.iter().enumerate() {
        if index == 0 {
            let Some(after) = rest.strip_prefix(part) else {
                return false;
            };
            rest = after;
        } else if index == parts.len() - 1 {
            return rest.ends_with(part);
        } else if let Some(at) = rest.find(part) {
            rest = &rest[at + part.len()..];
        } else {
            return false;
        }
    }
    true
}

/// The variables a send may offer: `environment` filtered by the policy's
/// deny and allow lists, sorted by name. Values stay in this process.
#[must_use]
pub fn offered(
    environment: Vec<(String, String)>,
    policy: &EffectivePolicy,
) -> Vec<(String, String)> {
    let mut out: Vec<(String, String)> = environment
        .into_iter()
        .filter(|(name, _)| valid_env_name(name))
        .filter(|(name, _)| !policy.env_deny.iter().any(|p| name_matches(p, name)))
        .filter(|(name, _)| {
            policy
                .env_allow
                .as_ref()
                .is_none_or(|allow| allow.iter().any(|p| name_matches(p, name)))
        })
        .collect();
    out.sort();
    out.dedup_by(|a, b| a.0 == b.0);
    out
}

/// `EnvVar[]` for the UI: names and value lengths, never a value.
#[must_use]
pub fn offered_wire(offered: &[(String, String)]) -> Value {
    Value::Array(
        offered
            .iter()
            .map(|(name, value)| json!({"name": name, "length": value.chars().count()}))
            .collect(),
    )
}

/// The selection a send would carry. Its `Debug` names the variables, never
/// their values.
#[derive(Default)]
pub struct Harvest {
    pub env: BTreeMap<String, String>,
    /// Selected names not offered now.
    pub missing: Vec<String>,
    pub bytes: usize,
    pub over_limit: bool,
}

impl std::fmt::Debug for Harvest {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Harvest")
            .field("names", &self.env.keys().collect::<Vec<_>>())
            .field("missing", &self.missing)
            .field("bytes", &self.bytes)
            .field("over_limit", &self.over_limit)
            .finish()
    }
}

/// The selected names' values from `offered`, within `limits`.
#[must_use]
pub fn harvest(selected: &[String], offered: &[(String, String)], limits: EnvLimits) -> Harvest {
    let mut out = Harvest::default();
    for name in selected {
        match offered.iter().find(|(offered, _)| offered == name) {
            Some((name, value)) => {
                out.bytes += name.len() + value.len();
                out.env.insert(name.clone(), value.clone());
            }
            None => out.missing.push(name.clone()),
        }
    }
    out.over_limit = out.env.len() > limits.max_vars || out.bytes > limits.max_bytes;
    out
}

#[cfg(test)]
mod tests {
    use super::{EffectivePolicy, EnvLimits, FolderSettings, harvest, name_matches, offered};
    use serde_json::json;

    #[test]
    fn the_servers_policy_and_the_defaults() {
        let policy = EffectivePolicy::from_server(&json!({
            "enabled": true, "exclusions": [".env", "*.pem", "*.sqlite"], "path_deny": ["secrets/**"],
            "allow_user_include": true, "env_harvest": true, "env_deny": ["PATH", "SSH_*"], "env_allow": null,
            "limits": {"max_segment_bytes": 10, "max_file_bytes": 5, "max_files": 3, "max_unpacked_bytes": 20,
                       "project_workspace_bytes_left": 7},
            "env_limits": {"max_vars": 2, "max_bytes": 100}
        }));
        assert_eq!(policy.source, "server");
        assert_eq!(policy.patterns.platform, [".env", "*.pem"]);
        assert_eq!(policy.patterns.project, ["*.sqlite"]);
        assert_eq!(policy.limits.project_workspace_bytes_left, Some(7));
        assert_eq!(policy.env_limits.max_vars, 2);
        let split = EffectivePolicy::from_server(&json!({
            "exclusions": ["a", "b"], "exclusions_platform": ["a"], "exclusions_project": ["b"]}));
        assert_eq!(split.patterns.platform, ["a"]);
        assert!(
            !split.allow_user_include && !split.env_harvest,
            "absent is off"
        );
        let defaults = EffectivePolicy::defaults();
        assert_eq!(defaults.source, "defaults");
        assert!(defaults.allow_user_include && !defaults.env_harvest);
    }

    #[test]
    fn env_names_are_filtered_and_bounded() {
        assert!(name_matches("SSH_*", "SSH_AUTH_SOCK"));
        assert!(name_matches("*_TOKEN", "GITHUB_TOKEN"));
        assert!(name_matches("A*B*C", "AxxBxxC"));
        assert!(!name_matches("PATH", "XPATH"));
        let mut policy = EffectivePolicy::defaults();
        let env = vec![
            ("PATH".to_owned(), "/bin".to_owned()),
            ("DATABASE_URL".to_owned(), "postgres://x".to_owned()),
            ("LC_ALL".to_owned(), "C".to_owned()),
            ("bad-name".to_owned(), "x".to_owned()),
        ];
        let names: Vec<String> = offered(env.clone(), &policy)
            .into_iter()
            .map(|(n, _)| n)
            .collect();
        assert_eq!(names, ["DATABASE_URL"]);
        policy.env_allow = Some(vec!["NOTHING".into()]);
        assert!(offered(env.clone(), &policy).is_empty());
        let offered_now = vec![("A".to_owned(), "12345".to_owned())];
        let got = harvest(
            &["A".into(), "B".into()],
            &offered_now,
            EnvLimits {
                max_vars: 1,
                max_bytes: 6,
            },
        );
        assert_eq!(got.missing, ["B"]);
        assert_eq!(got.bytes, 6);
        assert!(!got.over_limit);
        let got = harvest(
            &["A".into()],
            &offered_now,
            EnvLimits {
                max_vars: 1,
                max_bytes: 5,
            },
        );
        assert!(got.over_limit);
    }

    #[test]
    fn settings_are_checked() {
        let ok = FolderSettings {
            exclude_patterns: vec![" notes/** ".into(), "notes/**".into()],
            include: vec!["./.env".into()],
            env_names: vec!["DATABASE_URL".into()],
        }
        .validated()
        .unwrap();
        assert_eq!(ok.exclude_patterns, ["notes/**"]);
        assert_eq!(ok.include, [".env"]);
        for bad in [
            FolderSettings {
                include: vec!["../x".into()],
                ..FolderSettings::default()
            },
            FolderSettings {
                env_names: vec!["1X".into()],
                ..FolderSettings::default()
            },
            FolderSettings {
                exclude_patterns: vec!["a\nb".into()],
                ..FolderSettings::default()
            },
        ] {
            assert_eq!(bad.validated().unwrap_err().code, "invalid_request");
        }
    }
}
