//! Non-secret app state kept in the app config directory: which deployment is
//! connected, and the server-driven client policy last received.
//!
//! Nothing secret is written here (see `store.rs`). The files are plain JSON so
//! a person can read what the app remembers.

use std::fs;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::error::HostError;

const SETTINGS_FILE: &str = "settings.json";
const POLICY_FILE: &str = "client-policy.json";

/// The default OAuth client id the deployment must register for this app
/// (ADR-0025 decision 3). Override with `ELITEA_DESKTOP_CLIENT_ID` at build
/// time or at run time, or `client_id` in `settings.json`.
pub const DEFAULT_CLIENT_ID: &str = "desktop";

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct Settings {
    pub origin: Option<String>,
    pub display_name: Option<String>,
    /// Overrides the default client id; the deployment registers whichever it expects.
    pub client_id: Option<String>,
}

pub struct SettingsFiles {
    dir: PathBuf,
}

fn io(error: std::io::Error) -> HostError {
    HostError::Storage(error.to_string())
}

impl SettingsFiles {
    pub fn new(dir: PathBuf) -> Self {
        Self { dir }
    }

    fn read<T: serde::de::DeserializeOwned>(&self, name: &str) -> Result<Option<T>, HostError> {
        match fs::read_to_string(self.dir.join(name)) {
            // A file that no longer parses is treated as absent: settings are
            // re-entered from the connect screen, never worth a hard failure.
            Ok(text) => Ok(serde_json::from_str(&text).ok()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(io(e)),
        }
    }

    fn write<T: Serialize>(&self, name: &str, value: &T) -> Result<(), HostError> {
        fs::create_dir_all(&self.dir).map_err(io)?;
        let text =
            serde_json::to_string_pretty(value).map_err(|e| HostError::Internal(e.to_string()))?;
        let target = self.dir.join(name);
        // Write then rename, so a crash never leaves a half-written file.
        let staging = self.dir.join(format!("{name}.tmp"));
        fs::write(&staging, text).map_err(io)?;
        fs::rename(&staging, target).map_err(io)
    }

    fn remove(&self, name: &str) -> Result<(), HostError> {
        match fs::remove_file(self.dir.join(name)) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(io(e)),
        }
    }

    pub fn settings(&self) -> Result<Settings, HostError> {
        Ok(self.read(SETTINGS_FILE)?.unwrap_or_default())
    }

    pub fn save_settings(&self, settings: &Settings) -> Result<(), HostError> {
        self.write(SETTINGS_FILE, settings)
    }

    /// The last `native_client_policy` the server sent (ADR-0025 decision 5),
    /// stored verbatim. Enforcing `local_work` comes with the local runtime.
    pub fn policy(&self) -> Result<Option<Value>, HostError> {
        self.read(POLICY_FILE)
    }

    pub fn save_policy(&self, policy: &Value) -> Result<(), HostError> {
        self.write(POLICY_FILE, policy)
    }

    pub fn clear_policy(&self) -> Result<(), HostError> {
        self.remove(POLICY_FILE)
    }
}

/// Which client id to present: a run-time environment variable wins, then the
/// settings file, then the value baked in at build time, then the default.
pub fn resolve_client_id(
    runtime_env: Option<&str>,
    file: Option<&str>,
    build_time: Option<&str>,
) -> String {
    [runtime_env, file, build_time]
        .into_iter()
        .flatten()
        .map(str::trim)
        .find(|v| !v.is_empty())
        .unwrap_or(DEFAULT_CLIENT_ID)
        .to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "elitea-desktop-test-{tag}-{}",
            crate::pkce::create_state()
        ));
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn settings_round_trip_and_default_when_absent() {
        let dir = temp_dir("settings");
        let files = SettingsFiles::new(dir.clone());
        assert_eq!(files.settings().unwrap(), Settings::default());
        let settings = Settings {
            origin: Some("https://elitea.example.com".into()),
            display_name: Some("Acme".into()),
            client_id: None,
        };
        files.save_settings(&settings).unwrap();
        assert_eq!(files.settings().unwrap(), settings);
        assert!(!dir.join("settings.json.tmp").exists());
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn a_corrupt_settings_file_reads_as_default() {
        let dir = temp_dir("corrupt");
        fs::write(dir.join("settings.json"), "{ not json").unwrap();
        assert_eq!(
            SettingsFiles::new(dir.clone()).settings().unwrap(),
            Settings::default()
        );
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn policy_is_stored_verbatim_and_clearable() {
        let dir = temp_dir("policy");
        let files = SettingsFiles::new(dir.clone());
        assert_eq!(files.policy().unwrap(), None);
        let policy =
            serde_json::json!({"idle_lock_seconds": 300, "local_work": {"allowed": false}});
        files.save_policy(&policy).unwrap();
        assert_eq!(files.policy().unwrap(), Some(policy));
        files.clear_policy().unwrap();
        files.clear_policy().unwrap();
        assert_eq!(files.policy().unwrap(), None);
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn client_id_precedence_is_runtime_then_file_then_build_then_default() {
        assert_eq!(
            resolve_client_id(Some("rt"), Some("file"), Some("build")),
            "rt"
        );
        assert_eq!(resolve_client_id(None, Some("file"), Some("build")), "file");
        assert_eq!(resolve_client_id(None, None, Some("build")), "build");
        assert_eq!(resolve_client_id(None, None, None), "desktop");
        assert_eq!(resolve_client_id(Some("  "), Some(""), None), "desktop");
    }
}
