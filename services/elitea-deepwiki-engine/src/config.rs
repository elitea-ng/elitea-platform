//! Settings, strict-parsed from the environment.
//!
//! Strict-parsed means an unparsable value fails the start rather than
//! falling back to a default (the Python `config.py` rule). Names are the
//! ones the Python sidecar reads, so a deployment swaps the image and keeps
//! its environment.

use crate::ingest::IngestSettings;
use crate::ingest::egress::EgressPolicy;
use crate::ingest::limits::IngestLimits;
use std::path::PathBuf;
use std::time::Duration;

/// The prefix every setting carries.
pub const ENV_PREFIX: &str = "ELITEA_DEEPWIKI_";

/// The socket the Go host dials when nothing else is set.
pub const DEFAULT_SOCKET: &str = "/run/deepwiki/engine.sock";

/// Python's `scratch_path` default.
pub const DEFAULT_SCRATCH_PATH: &str = "/tmp/deepwiki";

/// A setting that cannot be used.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
#[error("{0}")]
pub struct ConfigError(pub String);

/// Which runner the sidecar serves.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RunnerKind {
    Unavailable,
    Fixture,
}

/// Everything the sidecar reads from the environment.
#[derive(Debug, Clone, PartialEq)]
pub struct Settings {
    pub runner: RunnerKind,
    pub fixture_step: Duration,
    pub engine_socket: PathBuf,
    /// Repository ingest: the git-host allowlist, the per-job limits and
    /// the scratch root (see `ingest::limits` for the defaults).
    pub ingest: IngestSettings,
}

/// A whole number of at least 1, or the default when unset.
fn positive_count(
    raw: &impl Fn(&str) -> Option<String>,
    name: &str,
    default: u64,
) -> Result<u64, ConfigError> {
    match raw(name) {
        None => Ok(default),
        Some(text) => match text.trim().parse::<u64>() {
            Ok(value) if value > 0 => Ok(value),
            _ => Err(ConfigError(format!(
                "{ENV_PREFIX}{name} must be a whole number of at least 1, got '{text}'"
            ))),
        },
    }
}

/// A number of seconds above zero, or the default when unset.
fn positive_seconds(
    raw: &impl Fn(&str) -> Option<String>,
    name: &str,
    default: Duration,
) -> Result<Duration, ConfigError> {
    let Some(text) = raw(name) else {
        return Ok(default);
    };
    let seconds: f64 = text.trim().parse().map_err(|_| {
        ConfigError(format!(
            "{ENV_PREFIX}{name} must be a number of seconds, got '{text}'"
        ))
    })?;
    if !seconds.is_finite() || seconds <= 0.0 {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}{name} must be above zero, got '{text}'"
        )));
    }
    Duration::try_from_secs_f64(seconds)
        .map_err(|_| ConfigError(format!("{ENV_PREFIX}{name} is out of range, got '{text}'")))
}

fn ingest_settings(raw: &impl Fn(&str) -> Option<String>) -> Result<IngestSettings, ConfigError> {
    let defaults = IngestLimits::default();
    Ok(IngestSettings {
        // Fail-closed when unset: the policy is empty and refuses every
        // clone (security/egress.py, spi.ParseEgressPolicy).
        git_allowlist: EgressPolicy::parse(raw("GIT_ALLOWLIST").as_deref()),
        limits: IngestLimits {
            max_clone_bytes: positive_count(raw, "MAX_CLONE_BYTES", defaults.max_clone_bytes)?,
            max_file_count: positive_count(raw, "MAX_FILE_COUNT", defaults.max_file_count)?,
            max_file_bytes: positive_count(raw, "MAX_FILE_BYTES", defaults.max_file_bytes)?,
            max_parsed_bytes: positive_count(raw, "MAX_PARSED_BYTES", defaults.max_parsed_bytes)?,
            clone_timeout: positive_seconds(raw, "CLONE_TIMEOUT_SECONDS", defaults.clone_timeout)?,
        },
        scratch_path: PathBuf::from(
            raw("SCRATCH_PATH").unwrap_or_else(|| DEFAULT_SCRATCH_PATH.to_owned()),
        ),
    })
}

impl Settings {
    /// Read the settings through `lookup` (the process environment in
    /// production, a map in tests).
    ///
    /// # Errors
    ///
    /// A [`ConfigError`] naming the variable and the value it refused.
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, ConfigError> {
        let raw = |name: &str| lookup(&format!("{ENV_PREFIX}{name}")).filter(|v| !v.is_empty());
        let runner = match raw("RUNNER").as_deref().map(str::trim) {
            None | Some("unavailable") => RunnerKind::Unavailable,
            Some("fixture") => RunnerKind::Fixture,
            Some("native") => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER=native: the native analysis engine is not part of this build yet (ADR-0026 phases 2-6); use 'fixture' or 'unavailable'"
                )));
            }
            Some("legacy") => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER=legacy names the Python engine, which this binary is not; run the elitea-deepwiki -engine image for it"
                )));
            }
            Some(other) => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER must be one of ['unavailable', 'fixture'], got '{other}'"
                )));
            }
        };
        let fixture_step = match raw("FIXTURE_STEP_SECONDS") {
            None => Duration::from_secs(1),
            Some(text) => {
                let seconds: f64 = text.trim().parse().map_err(|_| {
                    ConfigError(format!(
                        "{ENV_PREFIX}FIXTURE_STEP_SECONDS must be a number of seconds, got '{text}'"
                    ))
                })?;
                if !seconds.is_finite() || seconds < 0.0 {
                    return Err(ConfigError(format!(
                        "{ENV_PREFIX}FIXTURE_STEP_SECONDS must not be negative, got '{text}'"
                    )));
                }
                Duration::try_from_secs_f64(seconds).map_err(|_| {
                    ConfigError(format!(
                        "{ENV_PREFIX}FIXTURE_STEP_SECONDS is out of range, got '{text}'"
                    ))
                })?
            }
        };
        let engine_socket =
            PathBuf::from(raw("ENGINE_SOCKET").unwrap_or_else(|| DEFAULT_SOCKET.to_owned()));
        let ingest = ingest_settings(&raw)?;
        Ok(Self {
            runner,
            fixture_step,
            engine_socket,
            ingest,
        })
    }

    /// Read the settings from the process environment.
    ///
    /// # Errors
    ///
    /// See [`Settings::from_lookup`].
    pub fn from_env() -> Result<Self, ConfigError> {
        Self::from_lookup(|name| std::env::var(name).ok())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn settings(pairs: &[(&str, &str)]) -> Result<Settings, ConfigError> {
        let map: HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        Settings::from_lookup(|name| map.get(name).cloned())
    }

    #[test]
    fn the_default_runner_refuses() {
        let parsed = settings(&[]);
        assert_eq!(
            parsed.as_ref().map(|s| s.runner),
            Ok(RunnerKind::Unavailable)
        );
        assert_eq!(
            parsed.map(|s| s.engine_socket),
            Ok(PathBuf::from(DEFAULT_SOCKET))
        );
    }

    #[test]
    fn unparsable_values_fail_the_start() {
        assert!(settings(&[("ELITEA_DEEPWIKI_RUNNER", "bogus")]).is_err());
        assert!(settings(&[("ELITEA_DEEPWIKI_RUNNER", "native")]).is_err());
        assert!(settings(&[("ELITEA_DEEPWIKI_RUNNER", "legacy")]).is_err());
        assert!(settings(&[("ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS", "x")]).is_err());
        assert!(settings(&[("ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS", "-1")]).is_err());
        assert!(settings(&[("ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS", "inf")]).is_err());
        // Finite but too large for a Duration: a config error, not a panic.
        assert!(settings(&[("ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS", "1e20")]).is_err());
    }

    #[test]
    fn ingest_settings_default_and_fail_closed() {
        let parsed = settings(&[]).map(|s| s.ingest);
        assert_eq!(
            parsed,
            Ok(IngestSettings {
                git_allowlist: EgressPolicy::parse(None),
                limits: IngestLimits::default(),
                scratch_path: PathBuf::from(DEFAULT_SCRATCH_PATH),
            })
        );
        let parsed = settings(&[
            ("ELITEA_DEEPWIKI_GIT_ALLOWLIST", "github.com,*.github.com"),
            ("ELITEA_DEEPWIKI_MAX_CLONE_BYTES", "1048576"),
            ("ELITEA_DEEPWIKI_MAX_FILE_COUNT", "10"),
            ("ELITEA_DEEPWIKI_MAX_FILE_BYTES", "2048"),
            ("ELITEA_DEEPWIKI_MAX_PARSED_BYTES", "4096"),
            ("ELITEA_DEEPWIKI_CLONE_TIMEOUT_SECONDS", "2.5"),
            ("ELITEA_DEEPWIKI_SCRATCH_PATH", "/scratch"),
        ])
        .map(|s| s.ingest);
        assert_eq!(
            parsed,
            Ok(IngestSettings {
                git_allowlist: EgressPolicy::parse(Some("github.com *.github.com")),
                limits: IngestLimits {
                    max_clone_bytes: 1_048_576,
                    max_file_count: 10,
                    max_file_bytes: 2048,
                    max_parsed_bytes: 4096,
                    clone_timeout: Duration::from_millis(2500),
                },
                scratch_path: PathBuf::from("/scratch"),
            })
        );
    }

    #[test]
    fn unparsable_limits_fail_the_start() {
        for name in [
            "MAX_CLONE_BYTES",
            "MAX_FILE_COUNT",
            "MAX_FILE_BYTES",
            "MAX_PARSED_BYTES",
            "CLONE_TIMEOUT_SECONDS",
        ] {
            for bad in ["0", "-1", "x", "1e400"] {
                let key = format!("ELITEA_DEEPWIKI_{name}");
                assert!(settings(&[(key.as_str(), bad)]).is_err(), "{name}={bad}");
            }
        }
        assert!(settings(&[("ELITEA_DEEPWIKI_MAX_FILE_COUNT", "1.5")]).is_err());
    }

    #[test]
    fn the_fixture_step_is_seconds() {
        let parsed = settings(&[
            ("ELITEA_DEEPWIKI_RUNNER", "fixture"),
            ("ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS", "0.25"),
        ]);
        assert_eq!(
            parsed.map(|s| (s.runner, s.fixture_step)),
            Ok((RunnerKind::Fixture, Duration::from_millis(250)))
        );
    }
}
