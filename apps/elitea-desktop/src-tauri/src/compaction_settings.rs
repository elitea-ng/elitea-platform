//! The app-wide context compaction settings (`compaction_settings_get`,
//! `compaction_settings_set`; IPC.md, "Context compaction settings").
//!
//! One small JSON file in the app data directory
//! (`compaction-settings.json`), validated on every write and read back
//! leniently: a file that no longer parses or validates is the defaults
//! (logged), never a failure. Nothing secret is in it.
//!
//! * **`auto`.** Off: no compaction happens on its own, neither the step
//!   compaction's threshold (`d0/turn.rs`, [`CompactionPolicy::automatic`])
//!   nor the idle one (`d0/idle.rs`); `/compact` still works, and a model
//!   refusal for a too-long request still compacts and retries once.
//! * **`threshold_percent`.** The step compaction starts at this share of
//!   the budget's usable input when that is earlier than its default
//!   (`window − reserve`); `None` keeps the default.
//! * **`idle` / `idle_minutes`.** How long the provider's prompt cache is
//!   asked to live ([`CacheTtl`]: Anthropic breakpoints say `ttl: "1h"` for
//!   the one-hour cache) and when an idle thread is compacted while that
//!   cache is still warm ([`CompactionSettings::idle_delay`]).
//!
//! [`CompactionPolicy::automatic`]: elitea_agent_runtime::context_management::CompactionPolicy::automatic

use std::path::PathBuf;
use std::sync::RwLock;
use std::time::Duration;

use serde::{Deserialize, Serialize};

/// The file in the app data directory.
pub const FILE_NAME: &str = "compaction-settings.json";

/// The event `compaction_settings_set` emits with the saved settings.
pub const EVENT_NAME: &str = "compaction-settings-changed";

/// The accepted `threshold_percent`.
pub const THRESHOLD_PERCENT: std::ops::RangeInclusive<u8> = 40..=99;

/// The accepted `idle_minutes` (`idle: "custom"` only).
pub const IDLE_MINUTES: std::ops::RangeInclusive<u32> = 1..=600;

/// The idle delay of the five-minute cache: a minute before it expires
/// (counted from the last model request's start).
pub const CACHE_5M_DELAY: Duration = Duration::from_secs(4 * 60);

/// The idle delay of the one-hour cache.
pub const CACHE_1H_DELAY: Duration = Duration::from_secs(55 * 60);

/// A custom delay of this many minutes or more lands too close to (or past)
/// the end of the one-hour cache: no idle compaction is scheduled.
const CUSTOM_PAST_CACHE_MINUTES: u32 = 56;

/// A custom delay under this many minutes rides the five-minute cache.
const CUSTOM_5M_UNDER_MINUTES: u32 = 5;

/// When an idle thread is compacted.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum IdleMode {
    /// Four minutes after the last model request started: the default
    /// five-minute cache is still warm.
    #[default]
    #[serde(rename = "cache_5m")]
    Cache5m,
    /// The one-hour cache (breakpoints carry `ttl: "1h"`), compacted after
    /// 55 minutes.
    #[serde(rename = "cache_1h")]
    Cache1h,
    /// After `idle_minutes` (1 to 4: the five-minute cache; 5 to 55: the
    /// one-hour cache; 56 or more: never, past the cache).
    Custom,
    /// No idle compaction (the five-minute cache).
    Off,
}

/// The settings (all fields default; serde `snake_case`).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default, deny_unknown_fields)]
pub struct CompactionSettings {
    pub auto: bool,
    pub threshold_percent: Option<u8>,
    pub idle: IdleMode,
    pub idle_minutes: Option<u32>,
}

impl Default for CompactionSettings {
    fn default() -> Self {
        Self {
            auto: true,
            threshold_percent: None,
            idle: IdleMode::Cache5m,
            idle_minutes: None,
        }
    }
}

/// How long a provider prompt cache entry is asked to live.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum CacheTtl {
    /// Anthropic's default `{"type": "ephemeral"}`; OpenAI's automatic
    /// cache lives about as long.
    #[default]
    FiveMinutes,
    /// `{"type": "ephemeral", "ttl": "1h"}` (a cache write costs 2× the
    /// input price instead of 1.25×; a read 0.1× either way).
    OneHour,
}

impl CacheTtl {
    #[must_use]
    pub const fn duration(self) -> Duration {
        match self {
            Self::FiveMinutes => Duration::from_secs(5 * 60),
            Self::OneHour => Duration::from_secs(60 * 60),
        }
    }
}

impl CompactionSettings {
    /// The settings' own rules: `threshold_percent` 40 to 99; `idle_minutes`
    /// 1 to 600 for `custom` (required) and absent otherwise.
    ///
    /// # Errors
    ///
    /// A message for a person naming the rule.
    pub fn validate(&self) -> Result<(), String> {
        if let Some(percent) = self.threshold_percent
            && !THRESHOLD_PERCENT.contains(&percent)
        {
            return Err(format!(
                "The compaction threshold must be {} to {} percent.",
                THRESHOLD_PERCENT.start(),
                THRESHOLD_PERCENT.end()
            ));
        }
        match (self.idle, self.idle_minutes) {
            (IdleMode::Custom, Some(minutes)) if IDLE_MINUTES.contains(&minutes) => Ok(()),
            (IdleMode::Custom, _) => Err(format!(
                "A custom idle time must be {} to {} minutes.",
                IDLE_MINUTES.start(),
                IDLE_MINUTES.end()
            )),
            (_, Some(_)) => Err("idle_minutes is only for the custom idle time.".to_owned()),
            (_, None) => Ok(()),
        }
    }

    /// The prompt cache the turn's breakpoints ask for: one hour for
    /// `cache_1h` and a custom time of 5 to 55 minutes, else five. A custom
    /// time of 56 minutes or more never compacts while idle, so paying the
    /// 1-hour cache's 2× writes would buy nothing.
    #[must_use]
    pub fn cache_ttl(&self) -> CacheTtl {
        match (self.idle, self.idle_minutes) {
            (IdleMode::Cache1h, _) => CacheTtl::OneHour,
            (IdleMode::Custom, Some(minutes))
                if (CUSTOM_5M_UNDER_MINUTES..CUSTOM_PAST_CACHE_MINUTES).contains(&minutes) =>
            {
                CacheTtl::OneHour
            }
            _ => CacheTtl::FiveMinutes,
        }
    }

    /// How long after the last model request's start an idle thread is
    /// compacted; `None`: never (`auto` off, `idle: "off"`, or a custom
    /// time of 56 minutes or more, which the cache does not outlive).
    #[must_use]
    pub fn idle_delay(&self) -> Option<Duration> {
        if !self.auto {
            return None;
        }
        match (self.idle, self.idle_minutes) {
            (IdleMode::Cache5m, _) => Some(CACHE_5M_DELAY),
            (IdleMode::Cache1h, _) => Some(CACHE_1H_DELAY),
            (IdleMode::Custom, Some(minutes))
                if IDLE_MINUTES.contains(&minutes) && minutes < CUSTOM_PAST_CACHE_MINUTES =>
            {
                Some(Duration::from_secs(u64::from(minutes) * 60))
            }
            (IdleMode::Custom | IdleMode::Off, _) => None,
        }
    }
}

/// The settings, kept in memory and in the file.
pub struct CompactionSettingsStore {
    /// `None`: in memory only (tests, or no data directory).
    path: Option<PathBuf>,
    current: RwLock<CompactionSettings>,
}

impl Default for CompactionSettingsStore {
    fn default() -> Self {
        Self::in_memory()
    }
}

impl CompactionSettingsStore {
    /// Settings that are never written.
    #[must_use]
    pub fn in_memory() -> Self {
        Self {
            path: None,
            current: RwLock::new(CompactionSettings::default()),
        }
    }

    /// The settings in `dir` ([`FILE_NAME`]); the defaults when the file is
    /// absent, unreadable or invalid.
    #[must_use]
    pub fn open(dir: PathBuf) -> Self {
        let path = dir.join(FILE_NAME);
        let current = match std::fs::read_to_string(&path) {
            Ok(text) => match serde_json::from_str::<CompactionSettings>(&text) {
                Ok(settings) if settings.validate().is_ok() => settings,
                _ => {
                    log::warn!("the compaction settings file is invalid; using the defaults");
                    CompactionSettings::default()
                }
            },
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                CompactionSettings::default()
            }
            Err(error) => {
                log::warn!(
                    "the compaction settings could not be read ({error}); using the defaults"
                );
                CompactionSettings::default()
            }
        };
        Self {
            path: Some(path),
            current: RwLock::new(current),
        }
    }

    /// The settings now.
    #[must_use]
    pub fn get(&self) -> CompactionSettings {
        self.current
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone()
    }

    /// Validate and keep `settings` (written then renamed, so a crash never
    /// leaves half a file); the saved settings.
    ///
    /// # Errors
    ///
    /// The rule `settings` breaks, or the file could not be written.
    pub fn set(&self, settings: CompactionSettings) -> Result<CompactionSettings, String> {
        settings.validate()?;
        if let Some(path) = &self.path {
            let text = serde_json::to_string_pretty(&settings).map_err(|e| e.to_string())?;
            if let Some(dir) = path.parent() {
                std::fs::create_dir_all(dir)
                    .map_err(|e| format!("The settings could not be saved: {e}"))?;
            }
            let staging = path.with_extension("json.tmp");
            std::fs::write(&staging, text)
                .and_then(|()| std::fs::rename(&staging, path))
                .map_err(|e| format!("The settings could not be saved: {e}"))?;
        }
        *self
            .current
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = settings.clone();
        Ok(settings)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn parse(value: serde_json::Value) -> Result<CompactionSettings, String> {
        let settings: CompactionSettings =
            serde_json::from_value(value).map_err(|e| e.to_string())?;
        settings.validate()?;
        Ok(settings)
    }

    #[test]
    fn every_field_defaults_and_the_rules_hold() {
        assert_eq!(parse(json!({})).unwrap(), CompactionSettings::default());
        let defaults = CompactionSettings::default();
        assert!(defaults.auto && defaults.threshold_percent.is_none());
        assert_eq!(defaults.idle, IdleMode::Cache5m);
        assert_eq!(
            serde_json::to_value(&defaults).unwrap(),
            json!({"auto": true, "threshold_percent": null, "idle": "cache_5m", "idle_minutes": null})
        );
        for percent in [40, 75, 99] {
            assert!(parse(json!({"threshold_percent": percent})).is_ok());
        }
        for percent in [0, 39, 100, 255] {
            assert!(parse(json!({"threshold_percent": percent})).is_err());
        }
        assert!(parse(json!({"idle": "custom", "idle_minutes": 1})).is_ok());
        assert!(parse(json!({"idle": "custom", "idle_minutes": 600})).is_ok());
        assert!(parse(json!({"idle": "custom"})).is_err());
        assert!(parse(json!({"idle": "custom", "idle_minutes": 0})).is_err());
        assert!(parse(json!({"idle": "custom", "idle_minutes": 601})).is_err());
        // Minutes belong to the custom time only.
        assert!(parse(json!({"idle": "cache_1h", "idle_minutes": 30})).is_err());
        assert!(parse(json!({"idle": "off", "idle_minutes": 5})).is_err());
        // Unknown modes and keys are refused.
        assert!(parse(json!({"idle": "cache_2h"})).is_err());
        assert!(parse(json!({"autos": false})).is_err());
    }

    #[test]
    fn the_idle_mode_picks_the_cache_and_the_delay() {
        let with = |value: serde_json::Value| parse(value).unwrap();
        let five = with(json!({}));
        assert_eq!(five.cache_ttl(), CacheTtl::FiveMinutes);
        assert_eq!(five.idle_delay(), Some(Duration::from_secs(240)));
        let hour = with(json!({"idle": "cache_1h"}));
        assert_eq!(hour.cache_ttl(), CacheTtl::OneHour);
        assert_eq!(hour.idle_delay(), Some(Duration::from_secs(55 * 60)));
        let off = with(json!({"idle": "off"}));
        assert_eq!(off.cache_ttl(), CacheTtl::FiveMinutes);
        assert_eq!(off.idle_delay(), None);
        // Custom: 1-4 minutes ride the five-minute cache, 5-55 the hour,
        // 56 and more are past the cache and never run.
        let custom = |minutes: u32| with(json!({"idle": "custom", "idle_minutes": minutes}));
        assert_eq!(custom(3).cache_ttl(), CacheTtl::FiveMinutes);
        assert_eq!(custom(3).idle_delay(), Some(Duration::from_secs(180)));
        assert_eq!(custom(5).cache_ttl(), CacheTtl::OneHour);
        assert_eq!(custom(55).idle_delay(), Some(Duration::from_secs(55 * 60)));
        assert_eq!(custom(55).cache_ttl(), CacheTtl::OneHour);
        assert_eq!(custom(56).cache_ttl(), CacheTtl::FiveMinutes);
        assert_eq!(custom(56).idle_delay(), None);
        assert_eq!(custom(600).idle_delay(), None);
        // Auto off: no idle compaction, whatever the mode; the cache stays.
        let manual = with(json!({"auto": false, "idle": "cache_1h"}));
        assert_eq!(manual.idle_delay(), None);
        assert_eq!(manual.cache_ttl(), CacheTtl::OneHour);
    }

    #[test]
    fn the_file_keeps_valid_settings_and_reads_back_leniently() {
        let dir = tempfile::tempdir().unwrap();
        let store = CompactionSettingsStore::open(dir.path().to_path_buf());
        assert_eq!(store.get(), CompactionSettings::default());
        let saved = store
            .set(CompactionSettings {
                threshold_percent: Some(70),
                idle: IdleMode::Cache1h,
                ..CompactionSettings::default()
            })
            .unwrap();
        assert_eq!(
            CompactionSettingsStore::open(dir.path().to_path_buf()).get(),
            saved
        );
        // An invalid write changes nothing.
        assert!(
            store
                .set(CompactionSettings {
                    threshold_percent: Some(10),
                    ..CompactionSettings::default()
                })
                .is_err()
        );
        assert_eq!(store.get(), saved);
        // A broken file is the defaults.
        std::fs::write(dir.path().join(FILE_NAME), "{\"threshold_percent\": 5}").unwrap();
        assert_eq!(
            CompactionSettingsStore::open(dir.path().to_path_buf()).get(),
            CompactionSettings::default()
        );
    }
}
