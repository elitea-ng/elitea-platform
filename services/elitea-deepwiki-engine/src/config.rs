//! Settings, strict-parsed from the environment.
//!
//! Strict-parsed means an unparsable value fails the start rather than
//! falling back to a default (the Python `config.py` rule). Names are the
//! ones the Python sidecar reads, so a deployment swaps the image and keeps
//! its environment.

use std::path::PathBuf;
use std::time::Duration;

/// The prefix every setting carries.
pub const ENV_PREFIX: &str = "ELITEA_DEEPWIKI_";

/// The socket the Go host dials when nothing else is set.
pub const DEFAULT_SOCKET: &str = "/run/deepwiki/engine.sock";

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
        Ok(Self {
            runner,
            fixture_step,
            engine_socket,
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
