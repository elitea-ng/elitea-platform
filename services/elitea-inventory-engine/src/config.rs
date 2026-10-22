//! Sidecar settings, strict-parsed from `ELITEA_INVENTORY_*` — the names the
//! retired Python sidecar read, kept so a deployment switched engines without
//! renaming anything (provenance: `tests/fixtures/PROVENANCE.md`).

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
    /// the platform roots (the gateway reached over TLS: elitea-main's
    /// internal platform listener signs with the runtime CA).
    pub callback_ca_file: Option<PathBuf>,
    /// `CALLBACK_CERT_FILE` and `CALLBACK_KEY_FILE`, both or neither: the
    /// client certificate the model transport presents. elitea-main's
    /// platform listener requires a runtime-CA one.
    pub callback_identity: Option<elitea_model_client::ClientIdentityFiles>,
    /// `MODEL_SERVICE_URL`: the origin of elitea-main's platform listener
    /// (`https://elitea-main:9446`). Every model call
    /// (`elitea.llm.v1.ModelService`) and every source-tool call of
    /// `investigate` (`elitea.runtime.v1.InternalToolService`, #1359 U5)
    /// goes there over the callback CA and client certificate;
    /// `llm_settings.api_base` only names the model route. The native runner
    /// requires it; `http://` is accepted for a loopback host only (tests).
    pub model_service: Option<String>,
    /// `VECTOR_URL`: the origin of elitea-vector (`https://elitea-vector:9470`),
    /// which holds every graph's entity vectors in Qdrant (#1359;
    /// `store::vectors`). Reached over mutual TLS with the callback material
    /// (`CALLBACK_CA_FILE` trusts its server certificate, the callback client
    /// certificate is presented), and every call carries the invocation's
    /// callback bearer, which elitea-vector admits for that project and
    /// source `inventory`. The native runner requires it; `http://` is
    /// accepted for a loopback host only (tests).
    pub vector_service: Option<String>,
    /// `VECTOR_SERVER_NAME`: the name elitea-vector's certificate is verified
    /// against, when it differs from `VECTOR_URL`'s host.
    pub vector_server_name: Option<String>,
    /// `MODEL_CONCURRENCY`: the ingestion model calls (extraction,
    /// relations, community labels) in flight at once across this engine
    /// process (default [`DEFAULT_MODEL_CONCURRENCY`]). Set it to the
    /// model server's concurrent slots: past them a call only queues there.
    pub model_concurrency: usize,
    /// `POOL_SIZE`: the native runner's PostgreSQL connections, shared by
    /// every invocation this engine process runs (default
    /// [`DEFAULT_POOL_SIZE`], at most [`MAX_POOL_SIZE`]). One replica uses
    /// this many, so the fleet uses replicas x this.
    pub pool_size: u32,
}

/// `MODEL_CONCURRENCY` when unset: a small model server's slot count.
pub const DEFAULT_MODEL_CONCURRENCY: usize = 8;

/// `POOL_SIZE` when unset: the size the native runner always had.
pub const DEFAULT_POOL_SIZE: u32 = 8;
/// The largest `POOL_SIZE`, the bound elitea-main puts on its own pools.
pub const MAX_POOL_SIZE: u32 = 64;

/// The callback client certificate's setting, for its errors.
pub const CALLBACK_CERT_SETTING: &str = "ELITEA_INVENTORY_CALLBACK_CERT_FILE";
/// Its private key's setting.
pub const CALLBACK_KEY_SETTING: &str = "ELITEA_INVENTORY_CALLBACK_KEY_FILE";
/// The model service origin's setting.
pub const MODEL_SERVICE_SETTING: &str = "ELITEA_INVENTORY_MODEL_SERVICE_URL";
/// elitea-vector's origin setting.
pub const VECTOR_SERVICE_SETTING: &str = "ELITEA_INVENTORY_VECTOR_URL";

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
                    "{ENV_PREFIX}RUNNER=legacy named the retired Python engine, which no longer exists; use native (the engine over PostgreSQL), or fixture for the canned graph"
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
        let callback_identity =
            callback_identity(raw("CALLBACK_CERT_FILE"), raw("CALLBACK_KEY_FILE"))?;
        let model_service = model_service(raw("MODEL_SERVICE_URL"))?;
        if runner == RunnerKind::Native && model_service.is_none() {
            return Err(ConfigError(format!(
                "{ENV_PREFIX}RUNNER=native needs {MODEL_SERVICE_SETTING}: the native engine reaches the model calls and the source tools only on elitea-main's platform listener (https://elitea-main:9446); llm_settings.api_base never names a host"
            )));
        }
        let vector_service = vector_service(raw("VECTOR_URL"), runner)?;
        Ok(Self {
            vector_service,
            vector_server_name: raw("VECTOR_SERVER_NAME"),
            source_types,
            ingest,
            database_url,
            callback_ca_file: raw("CALLBACK_CA_FILE").map(PathBuf::from),
            callback_identity,
            model_service,
            model_concurrency: usize::try_from(positive_count(
                &raw,
                "MODEL_CONCURRENCY",
                u64::try_from(DEFAULT_MODEL_CONCURRENCY).unwrap_or(u64::MAX),
            )?)
            .unwrap_or(usize::MAX),
            pool_size: pool_size(&raw)?,
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

/// `CALLBACK_CERT_FILE` and `CALLBACK_KEY_FILE`: both or neither.
fn callback_identity(
    cert: Option<String>,
    key: Option<String>,
) -> Result<Option<elitea_model_client::ClientIdentityFiles>, ConfigError> {
    match (cert, key) {
        (None, None) => Ok(None),
        (Some(cert), Some(key)) => Ok(Some(elitea_model_client::ClientIdentityFiles {
            cert_file: PathBuf::from(cert),
            cert_file_setting: CALLBACK_CERT_SETTING,
            key_file: PathBuf::from(key),
            key_file_setting: CALLBACK_KEY_SETTING,
        })),
        _ => Err(ConfigError(format!(
            "{CALLBACK_CERT_SETTING} and {CALLBACK_KEY_SETTING} must be set together"
        ))),
    }
}

/// `http://` to a loopback host: the tests' in-process platform listener.
fn is_loopback_http(origin: &str) -> bool {
    let Some(rest) = origin.strip_prefix("http://") else {
        return false;
    };
    let authority = rest.split(['/', '?', '#']).next().unwrap_or_default();
    let host = match authority.rsplit_once(':') {
        Some((host, port)) if !port.contains(']') => host,
        _ => authority,
    };
    let bare = host.trim_start_matches('[').trim_end_matches(']');
    !authority.contains('@')
        && (bare.eq_ignore_ascii_case("localhost")
            || bare
                .parse::<std::net::IpAddr>()
                .is_ok_and(|address| address.is_loopback()))
}

/// `MODEL_SERVICE_URL`: an https origin (the platform listener), a loopback
/// `http://` one (tests), or unset.
fn model_service(value: Option<String>) -> Result<Option<String>, ConfigError> {
    match value {
        None => Ok(None),
        Some(origin) if origin.starts_with("https://") || is_loopback_http(&origin) => {
            Ok(Some(origin))
        }
        Some(origin) => Err(ConfigError(format!(
            "{MODEL_SERVICE_SETTING} must be the https origin of elitea-main's platform listener, got '{origin}'"
        ))),
    }
}

/// `VECTOR_URL`: an https origin (elitea-vector), a loopback `http://` one
/// (tests), or unset.
/// The native runner requires it.
fn vector_service(
    value: Option<String>,
    runner: RunnerKind,
) -> Result<Option<String>, ConfigError> {
    match value {
        None if runner == RunnerKind::Native => Err(ConfigError(format!(
            "{ENV_PREFIX}RUNNER=native needs {VECTOR_SERVICE_SETTING}: the native engine keeps every graph's entity vectors in elitea-vector (https://elitea-vector:9470) and ranks them there"
        ))),
        None => Ok(None),
        Some(origin) if origin.starts_with("https://") || is_loopback_http(&origin) => {
            Ok(Some(origin.trim_end_matches('/').to_owned()))
        }
        Some(origin) => Err(ConfigError(format!(
            "{VECTOR_SERVICE_SETTING} must be the https origin of elitea-vector, got '{origin}'"
        ))),
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

/// `POOL_SIZE`, 1 through [`MAX_POOL_SIZE`].
fn pool_size(raw: &impl Fn(&str) -> Option<String>) -> Result<u32, ConfigError> {
    let size = positive_count(raw, "POOL_SIZE", u64::from(DEFAULT_POOL_SIZE))?;
    u32::try_from(size)
        .ok()
        .filter(|size| *size <= MAX_POOL_SIZE)
        .ok_or_else(|| {
            ConfigError(format!(
                "{ENV_PREFIX}POOL_SIZE must be at most {MAX_POOL_SIZE}, got {size}"
            ))
        })
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
        assert_eq!(defaults.model_concurrency, DEFAULT_MODEL_CONCURRENCY);
        assert_eq!(defaults.pool_size, DEFAULT_POOL_SIZE);
        assert_eq!(defaults.callback_identity, None);
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
    fn the_callback_client_certificate_is_both_halves_or_neither() {
        let parsed = settings(&[
            ("ELITEA_INVENTORY_CALLBACK_CERT_FILE", "/run/callback.crt"),
            ("ELITEA_INVENTORY_CALLBACK_KEY_FILE", "/run/callback.key"),
        ])
        .map(|s| s.callback_identity);
        assert_eq!(
            parsed,
            Ok(Some(elitea_model_client::ClientIdentityFiles {
                cert_file: PathBuf::from("/run/callback.crt"),
                cert_file_setting: CALLBACK_CERT_SETTING,
                key_file: PathBuf::from("/run/callback.key"),
                key_file_setting: CALLBACK_KEY_SETTING,
            }))
        );
        for half in [CALLBACK_CERT_SETTING, CALLBACK_KEY_SETTING] {
            let refused = settings(&[(half, "/run/half.pem")]);
            assert!(
                refused
                    .as_ref()
                    .is_err_and(|e| e.to_string().contains("must be set together")),
                "{half}"
            );
        }
    }

    #[test]
    fn the_model_service_is_an_https_origin() {
        let parsed = settings(&[(MODEL_SERVICE_SETTING, "https://elitea-main:9446")])
            .map(|s| s.model_service);
        assert_eq!(parsed, Ok(Some("https://elitea-main:9446".to_owned())));
        assert_eq!(settings(&[]).map(|s| s.model_service), Ok(None));
        assert!(
            settings(&[(MODEL_SERVICE_SETTING, "http://elitea-main:8080")])
                .is_err_and(|e| e.to_string().contains(MODEL_SERVICE_SETTING))
        );
        for loopback in [
            "http://127.0.0.1:8080",
            "http://[::1]:1/v1",
            "http://localhost:9",
        ] {
            assert!(
                settings(&[(MODEL_SERVICE_SETTING, loopback)]).is_ok(),
                "{loopback}"
            );
        }
        assert!(settings(&[(MODEL_SERVICE_SETTING, "http://127.0.0.1@evil:1")]).is_err());
        // The native runner reaches the platform only there: required.
        let native = settings(&[
            ("ELITEA_INVENTORY_RUNNER", "native"),
            (
                "ELITEA_INVENTORY_DATABASE_URL",
                "postgresql://u:p@db/inventory",
            ),
        ]);
        assert!(
            native.is_err_and(|e| e.to_string().contains(MODEL_SERVICE_SETTING)),
            "started without the platform listener"
        );
        let native = settings(&[
            ("ELITEA_INVENTORY_RUNNER", "native"),
            (
                "ELITEA_INVENTORY_DATABASE_URL",
                "postgresql://u:p@db/inventory",
            ),
            (MODEL_SERVICE_SETTING, "https://elitea-main:9446"),
            (VECTOR_SERVICE_SETTING, "https://elitea-vector:9470"),
        ]);
        assert!(native.is_ok(), "{native:?}");
    }

    #[test]
    fn the_native_runner_needs_elitea_vector() {
        let base = [
            ("ELITEA_INVENTORY_RUNNER", "native"),
            (
                "ELITEA_INVENTORY_DATABASE_URL",
                "postgresql://u:p@db/inventory",
            ),
            (MODEL_SERVICE_SETTING, "https://elitea-main:9446"),
        ];
        assert!(
            settings(&base).is_err_and(|e| e.to_string().contains(VECTOR_SERVICE_SETTING)),
            "started without elitea-vector"
        );
        let mut with = base.to_vec();
        with.push((VECTOR_SERVICE_SETTING, "https://elitea-vector:9470/"));
        with.push(("ELITEA_INVENTORY_VECTOR_SERVER_NAME", "elitea-vector"));
        let parsed = settings(&with).map(|s| (s.vector_service, s.vector_server_name));
        assert_eq!(
            parsed,
            Ok((
                Some("https://elitea-vector:9470".to_owned()),
                Some("elitea-vector".to_owned())
            ))
        );
        let mut plaintext = base.to_vec();
        plaintext.push((VECTOR_SERVICE_SETTING, "http://elitea-vector:9470"));
        assert!(
            settings(&plaintext).is_err_and(|e| e.to_string().contains(VECTOR_SERVICE_SETTING))
        );
        // The fixture runner stores nothing and needs none.
        assert!(settings(&[("ELITEA_INVENTORY_RUNNER", "fixture")]).is_ok());
    }

    #[test]
    fn ingest_settings_parse_and_refuse_by_name() {
        let Ok(parsed) = settings(&[
            ("ELITEA_INVENTORY_SOURCE_TYPES", "GitHub"),
            ("ELITEA_INVENTORY_GIT_ALLOWLIST", "github.com"),
            ("ELITEA_INVENTORY_MAX_FILE_COUNT", "10"),
            ("ELITEA_INVENTORY_CLONE_TIMEOUT_SECONDS", "2.5"),
            ("ELITEA_INVENTORY_MODEL_CONCURRENCY", "3"),
            ("ELITEA_INVENTORY_POOL_SIZE", "64"),
        ]) else {
            panic!("parses");
        };
        assert_eq!(parsed.source_types, ["github"]);
        assert_eq!(parsed.model_concurrency, 3);
        assert_eq!(parsed.pool_size, 64);
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
            ("ELITEA_INVENTORY_MODEL_CONCURRENCY", "0"),
            ("ELITEA_INVENTORY_POOL_SIZE", "0"),
            ("ELITEA_INVENTORY_POOL_SIZE", "65"),
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
        assert!(
            settings(&[("ELITEA_INVENTORY_RUNNER", "legacy")])
                .is_err_and(|e| e.0.contains("use native")),
            "the retired runner points at the native one"
        );
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
