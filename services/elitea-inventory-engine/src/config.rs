//! Sidecar settings, strict-parsed from `ELITEA_INVENTORY_*` — the names the
//! Python sidecar read (`elitea_inventory.config`), so a deployment switches
//! engines without renaming anything.

use std::path::PathBuf;
use std::time::Duration;

/// The variable prefix.
pub const ENV_PREFIX: &str = "ELITEA_INVENTORY_";

/// A setting that cannot be parsed. Startup stops on it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConfigError(pub String);

impl std::fmt::Display for ConfigError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl std::error::Error for ConfigError {}

/// Which tool runner the socket serves.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RunnerKind {
    /// Refuses every tool: an engine that is not wired must look broken.
    Unavailable,
    /// The canned graph (`crate::fixture`).
    Fixture,
}

/// Everything the engine sidecar reads from the environment.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Settings {
    /// `ENGINE_SOCKET`: the Unix socket the Go host in the same pod dials.
    pub engine_socket: PathBuf,
    /// `RUNNER`: `unavailable` (default) or `fixture`.
    pub runner: RunnerKind,
    /// `FIXTURE_STEP_SECONDS`: the pause between the fixture's progress lines.
    pub fixture_step: Duration,
    /// `FIXTURES`: a directory shaped like
    /// `conformance/provider/fixtures/inventory` (holding `spi/graph.json`);
    /// unset serves the packaged copy.
    pub fixtures: Option<PathBuf>,
}

impl Settings {
    /// Read the process environment.
    ///
    /// # Errors
    ///
    /// A value that cannot be parsed, named with its variable.
    pub fn from_env() -> Result<Self, ConfigError> {
        Self::from_lookup(|name| std::env::var(name).ok())
    }

    /// Read settings through `lookup` (the tests pass a map).
    ///
    /// # Errors
    ///
    /// A value that cannot be parsed, named with its variable.
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, ConfigError> {
        let raw = |name: &str| {
            lookup(&format!("{ENV_PREFIX}{name}"))
                .map(|value| value.trim().to_owned())
                .filter(|value| !value.is_empty())
        };
        let runner = match raw("RUNNER").as_deref() {
            None | Some("unavailable") => RunnerKind::Unavailable,
            Some("fixture") => RunnerKind::Fixture,
            Some("legacy") => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER=legacy names the Python engine, which this native image does not contain; use the elitea-inventory image for it, or fixture here"
                )));
            }
            Some(other) => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER must be unavailable or fixture, got '{other}'"
                )));
            }
        };
        let fixture_step = match raw("FIXTURE_STEP_SECONDS") {
            None => Duration::ZERO,
            Some(text) => text
                .parse::<f64>()
                .ok()
                .filter(|seconds| seconds.is_finite() && *seconds >= 0.0 && *seconds <= 60.0)
                .map(Duration::from_secs_f64)
                .ok_or_else(|| {
                    ConfigError(format!(
                        "{ENV_PREFIX}FIXTURE_STEP_SECONDS must be a number of seconds from 0 to 60, got '{text}'"
                    ))
                })?,
        };
        Ok(Self {
            engine_socket: raw("ENGINE_SOCKET").map_or_else(
                || PathBuf::from("/run/inventory/engine.sock"),
                PathBuf::from,
            ),
            runner,
            fixture_step,
            fixtures: raw("FIXTURES").map(PathBuf::from),
        })
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
    fn the_defaults_refuse_and_listen_where_the_python_sidecar_did() {
        let Ok(defaults) = settings(&[]) else {
            panic!("the defaults parse");
        };
        assert_eq!(defaults.runner, RunnerKind::Unavailable);
        assert_eq!(
            defaults.engine_socket,
            PathBuf::from("/run/inventory/engine.sock")
        );
        assert_eq!(defaults.fixture_step, Duration::ZERO);
        assert_eq!(defaults.fixtures, None);
    }

    #[test]
    fn bad_values_stop_the_start_with_their_name() {
        for (name, value) in [
            ("ELITEA_INVENTORY_RUNNER", "legacy"),
            ("ELITEA_INVENTORY_RUNNER", "native"),
            ("ELITEA_INVENTORY_FIXTURE_STEP_SECONDS", "-1"),
            ("ELITEA_INVENTORY_FIXTURE_STEP_SECONDS", "soon"),
        ] {
            let refused = settings(&[(name, value)]);
            assert!(
                refused.as_ref().is_err_and(|e| e.0.contains(name)),
                "{name}={value}: {refused:?}"
            );
        }
        assert_eq!(
            settings(&[
                ("ELITEA_INVENTORY_RUNNER", "fixture"),
                ("ELITEA_INVENTORY_FIXTURE_STEP_SECONDS", "0.5")
            ])
            .map(|s| (s.runner, s.fixture_step)),
            Ok((RunnerKind::Fixture, Duration::from_millis(500)))
        );
    }
}
