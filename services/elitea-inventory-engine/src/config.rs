//! Sidecar settings, strict-parsed from `ELITEA_INVENTORY_*` — the names the
//! Python sidecar read (`elitea_inventory.config`), so a deployment switches
//! engines without renaming anything.

use crate::ingest::source::DEFAULT_SOURCE_TYPES;
use elitea_repo_ingest::IngestSettings;
use elitea_repo_ingest::artifact::ArtifactCaps;
use elitea_repo_ingest::egress::EgressPolicy;
use elitea_repo_ingest::limits::IngestLimits;
use elitea_repo_ingest::names::SettingNames;
use std::path::PathBuf;
use std::time::Duration;

/// The variable prefix.
pub const ENV_PREFIX: &str = "ELITEA_INVENTORY_";

/// The scratch root when `SCRATCH_PATH` is unset (the Python engine's).
pub const DEFAULT_SCRATCH_PATH: &str = "/var/scratch/inventory";

/// What this engine calls the clone settings, for the messages that ask an
/// operator to change one.
pub static INGEST_NAMES: SettingNames = SettingNames {
    max_clone_bytes: "ELITEA_INVENTORY_MAX_CLONE_BYTES",
    max_file_count: "ELITEA_INVENTORY_MAX_FILE_COUNT",
    max_file_bytes: "ELITEA_INVENTORY_MAX_FILE_BYTES",
    max_parsed_bytes: "ELITEA_INVENTORY_MAX_PARSED_BYTES",
    clone_timeout_seconds: "ELITEA_INVENTORY_CLONE_TIMEOUT_SECONDS",
    artifact_max_files: "ELITEA_INVENTORY_ARTIFACT_MAX_FILES",
    artifact_max_bytes: "ELITEA_INVENTORY_ARTIFACT_MAX_BYTES",
    git_allowlist: "ELITEA_INVENTORY_GIT_ALLOWLIST",
    user_agent: concat!("elitea-inventory-engine/", env!("CARGO_PKG_VERSION")),
};

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
    /// The engine itself: the PostgreSQL graph, ingestion, retrieval.
    Native,
}

/// Everything the engine sidecar reads from the environment.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Settings {
    /// `ENGINE_SOCKET`: the Unix socket the Go host in the same pod dials.
    pub engine_socket: PathBuf,
    /// `RUNNER`: `unavailable` (default), `fixture` or `native`.
    pub runner: RunnerKind,
    /// `FIXTURE_STEP_SECONDS`: the pause between the fixture's progress lines.
    pub fixture_step: Duration,
    /// `FIXTURES`: a directory shaped like
    /// `conformance/provider/fixtures/inventory` (holding `spi/graph.json`);
    /// unset serves the packaged copy.
    pub fixtures: Option<PathBuf>,
    /// `SOURCE_TYPES`: the source toolkit types ingestion reads
    /// (comma-separated; default `github,ado_repos`).
    pub source_types: Vec<String>,
    /// The clone: `GIT_ALLOWLIST` (fail-closed: unset admits no host — the
    /// SAME list elitea-main's facade checks the source host against),
    /// `MAX_CLONE_BYTES`, `MAX_FILE_COUNT`, `MAX_FILE_BYTES`,
    /// `MAX_PARSED_BYTES`, `CLONE_TIMEOUT_SECONDS`, `SCRATCH_PATH`.
    pub ingest: IngestSettings,
    /// `DATABASE_URL`: the graph store (required by `native`).
    pub database_url: Option<elitea_engine_core::secret::Secret>,
    /// `CALLBACK_CA_FILE`: a PEM bundle the model transport trusts besides
    /// the platform roots (the gateway reached over TLS through the edge).
    pub callback_ca_file: Option<PathBuf>,
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
            Some("native") => RunnerKind::Native,
            Some("legacy") => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER=legacy names the Python engine, which this native image does not contain; use the elitea-inventory image for it, or fixture here"
                )));
            }
            Some(other) => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER must be unavailable, fixture or native, got '{other}'"
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
        let source_types: Vec<String> = match raw("SOURCE_TYPES") {
            None => DEFAULT_SOURCE_TYPES
                .iter()
                .map(|t| (*t).to_owned())
                .collect(),
            Some(text) => text
                .split(',')
                .map(|item| item.trim().to_lowercase())
                .filter(|item| !item.is_empty())
                .collect(),
        };
        if source_types.is_empty() {
            return Err(ConfigError(format!(
                "{ENV_PREFIX}SOURCE_TYPES names no source type"
            )));
        }
        if let Some(unknown) = source_types
            .iter()
            .find(|name| !DEFAULT_SOURCE_TYPES.contains(&name.as_str()))
        {
            return Err(ConfigError(format!(
                "{ENV_PREFIX}SOURCE_TYPES names '{unknown}', which this engine cannot read (it reads {})",
                DEFAULT_SOURCE_TYPES.join(", ")
            )));
        }
        let ingest = ingest_settings(&raw)?;
        let database_url = raw("DATABASE_URL").and_then(elitea_engine_core::secret::Secret::new);
        if runner == RunnerKind::Native && database_url.is_none() {
            return Err(ConfigError(format!(
                "{ENV_PREFIX}RUNNER=native needs {ENV_PREFIX}DATABASE_URL: the native engine keeps every graph in PostgreSQL and has no other storage"
            )));
        }
        Ok(Self {
            source_types,
            ingest,
            database_url,
            callback_ca_file: raw("CALLBACK_CA_FILE").map(PathBuf::from),
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

/// A whole number above zero, or the default when unset.
fn positive_count(
    raw: &impl Fn(&str) -> Option<String>,
    name: &str,
    default: u64,
) -> Result<u64, ConfigError> {
    match raw(name) {
        None => Ok(default),
        Some(text) => match text.parse::<u64>() {
            Ok(value) if value > 0 => Ok(value),
            _ => Err(ConfigError(format!(
                "{ENV_PREFIX}{name} must be a whole number of at least 1, got '{text}'"
            ))),
        },
    }
}

fn ingest_settings(raw: &impl Fn(&str) -> Option<String>) -> Result<IngestSettings, ConfigError> {
    let defaults = IngestLimits::default();
    let timeout = match raw("CLONE_TIMEOUT_SECONDS") {
        None => defaults.clone_timeout,
        Some(text) => text
            .parse::<f64>()
            .ok()
            .filter(|seconds| seconds.is_finite() && *seconds > 0.0)
            .and_then(|seconds| Duration::try_from_secs_f64(seconds).ok())
            .ok_or_else(|| {
                ConfigError(format!(
                    "{ENV_PREFIX}CLONE_TIMEOUT_SECONDS must be a number of seconds above zero, got '{text}'"
                ))
            })?,
    };
    Ok(IngestSettings {
        git_allowlist: EgressPolicy::parse(raw("GIT_ALLOWLIST").as_deref()).named(&INGEST_NAMES),
        limits: IngestLimits {
            max_clone_bytes: positive_count(raw, "MAX_CLONE_BYTES", defaults.max_clone_bytes)?,
            max_file_count: positive_count(raw, "MAX_FILE_COUNT", defaults.max_file_count)?,
            max_file_bytes: positive_count(raw, "MAX_FILE_BYTES", defaults.max_file_bytes)?,
            max_parsed_bytes: positive_count(raw, "MAX_PARSED_BYTES", defaults.max_parsed_bytes)?,
            clone_timeout: timeout,
            names: &INGEST_NAMES,
        },
        scratch_path: PathBuf::from(
            raw("SCRATCH_PATH").unwrap_or_else(|| DEFAULT_SCRATCH_PATH.to_owned()),
        ),
        // No artifact-folder sources: the caps are the crate's defaults.
        artifact: ArtifactCaps::default(),
    })
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
        assert_eq!(defaults.source_types, ["github", "ado_repos"]);
        assert_eq!(
            defaults.ingest.scratch_path,
            PathBuf::from("/var/scratch/inventory")
        );
        assert!(
            defaults.ingest.git_allowlist.is_empty(),
            "fail-closed: no host until one is listed"
        );
    }

    #[test]
    fn ingest_settings_parse_and_refuse_by_name() {
        let Ok(parsed) = settings(&[
            ("ELITEA_INVENTORY_SOURCE_TYPES", "GitHub"),
            ("ELITEA_INVENTORY_GIT_ALLOWLIST", "github.com"),
            ("ELITEA_INVENTORY_MAX_FILE_COUNT", "10"),
            ("ELITEA_INVENTORY_CLONE_TIMEOUT_SECONDS", "2.5"),
        ]) else {
            panic!("parses");
        };
        assert_eq!(parsed.source_types, ["github"]);
        assert!(parsed.ingest.git_allowlist.permits("github.com"));
        assert_eq!(parsed.ingest.limits.max_file_count, 10);
        assert_eq!(
            parsed.ingest.limits.clone_timeout,
            Duration::from_millis(2500)
        );
        for (name, value) in [
            ("ELITEA_INVENTORY_SOURCE_TYPES", " , "),
            ("ELITEA_INVENTORY_SOURCE_TYPES", "github,gitlab"),
            ("ELITEA_INVENTORY_MAX_FILE_COUNT", "0"),
            ("ELITEA_INVENTORY_CLONE_TIMEOUT_SECONDS", "-1"),
        ] {
            let refused = settings(&[(name, value)]);
            assert!(
                refused.as_ref().is_err_and(|e| e.0.contains(name)),
                "{name}={value}: {refused:?}"
            );
        }
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
