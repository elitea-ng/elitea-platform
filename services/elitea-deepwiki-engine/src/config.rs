//! Settings, strict-parsed from the environment.
//!
//! Strict-parsed means an unparsable value fails the start rather than
//! falling back to a default (the Python `config.py` rule). Names are the
//! ones the Python sidecar reads, so a deployment swaps the image and keeps
//! its environment.

use crate::ingest::IngestSettings;
use crate::ingest::artifact::ArtifactCaps;
use crate::ingest::egress::EgressPolicy;
use crate::ingest::limits::IngestLimits;
use crate::llm::embeddings::{DEFAULT_BATCH_SIZE, DEFAULT_CONCURRENCY, MIN_SPLIT_TOKENS};
use crate::llm::tokens::EMBEDDING_CTX_LENGTH;
use crate::storage::build::{MIN_STALE_AFTER, PublishSettings};
use std::path::PathBuf;
use std::time::Duration;

/// The prefix every setting carries.
pub const ENV_PREFIX: &str = "ELITEA_DEEPWIKI_";

/// The socket the Go host dials when nothing else is set.
pub const DEFAULT_SOCKET: &str = "/run/deepwiki/engine.sock";

/// Python's `scratch_path` default.
pub const DEFAULT_SCRATCH_PATH: &str = "/tmp/deepwiki";

/// The build-space owner when neither `ELITEA_DEEPWIKI_BUILD_OWNER` nor
/// `HOSTNAME` is set. Every such engine would share it, so it is refused
/// when a database is configured (the reconciliation is keyed by owner).
pub const DEFAULT_BUILD_OWNER: &str = "elitea-deepwiki-engine";

/// `ELITEA_DEEPWIKI_QUERY_POOL_SIZE`'s default: the database connections
/// the in-process query tools share.
pub const DEFAULT_QUERY_POOL_SIZE: u32 = 8;
/// The largest `ELITEA_DEEPWIKI_QUERY_POOL_SIZE`.
pub const MAX_QUERY_POOL_SIZE: u32 = 256;

/// A database URL. It can carry a password, so its `Debug` form is
/// redacted and nothing formats it.
#[derive(Clone, PartialEq, Eq)]
pub struct DatabaseUrl(String);

impl DatabaseUrl {
    /// The URL itself, for the connection only.
    #[must_use]
    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl std::fmt::Debug for DatabaseUrl {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("DatabaseUrl(<redacted>)")
    }
}

/// A setting that cannot be used.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
#[error("{0}")]
pub struct ConfigError(pub String);

/// Which runner the sidecar serves.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RunnerKind {
    Unavailable,
    Fixture,
    /// The Rust engine itself (ADR-0026): `generate_wiki` in a worker
    /// child process. Needs `ELITEA_DEEPWIKI_DATABASE_URL`.
    Native,
}

/// The address-space cap of a generation worker when neither the setting
/// nor a cgroup memory limit gives one (16 GiB).
///
/// `RLIMIT_AS` counts reserved address space, not resident memory: thread
/// stacks (the Python parser pool reserves 256 MiB per thread) and malloc
/// arenas count in full. The cap is therefore well above the resident
/// peak of a large repository (1.3–3.2 GB measured on elitea-platform);
/// it stops a runaway, it does not size the pod.
pub const DEFAULT_WORKER_MEMORY_BYTES: u64 = 16 << 30;

/// The share of the container's memory limit (cgroup v2 `memory.max`) the
/// default address-space cap takes: 85 %, the rest for the parent and the
/// page cache.
pub const CGROUP_MEMORY_PERCENT: u64 = 85;

/// The default address-space cap: 85 % of the cgroup memory limit when one
/// is set, at least [`MIN_WORKER_MEMORY_BYTES`]; else
/// [`DEFAULT_WORKER_MEMORY_BYTES`].
#[must_use]
pub fn default_worker_memory(cgroup_memory_max: Option<u64>) -> u64 {
    cgroup_memory_max.map_or(DEFAULT_WORKER_MEMORY_BYTES, |limit| {
        (limit / 100)
            .saturating_mul(CGROUP_MEMORY_PERCENT)
            .max(MIN_WORKER_MEMORY_BYTES)
    })
}

/// The smallest address-space cap the settings accept (1 GiB): below it a
/// worker cannot even start its thread pools.
pub const MIN_WORKER_MEMORY_BYTES: u64 = 1 << 30;

/// The default CPU-time cap of a generation worker (4 h of CPU).
pub const DEFAULT_WORKER_CPU_SECONDS: u64 = 4 * 3600;

/// The smallest CPU-time cap the settings accept (1 min).
pub const MIN_WORKER_CPU_SECONDS: u64 = 60;

/// The most worker threads a generation may use.
pub const MAX_WORKER_THREADS: u64 = 256;

/// The address space each worker thread is sized at: the largest parser
/// stack (the Python parser's 256 MiB), which every thread of a parser
/// pool reserves.
pub const WORKER_THREAD_RESERVE_BYTES: u64 = crate::parsers::LARGEST_PARSER_STACK as u64;

/// The address space a worker needs besides its parser stacks (1 GiB):
/// the heap, the malloc arenas, the runtime's own threads.
pub const WORKER_MEMORY_HEADROOM_BYTES: u64 = 1 << 30;

/// The address space `threads` parser threads need:
/// `threads × 256 MiB + 1 GiB`.
#[must_use]
pub fn worker_memory_needed(threads: u64) -> u64 {
    threads
        .saturating_mul(WORKER_THREAD_RESERVE_BYTES)
        .saturating_add(WORKER_MEMORY_HEADROOM_BYTES)
}

/// The limits of one `generate_wiki` worker child process (ADR-0026
/// decision 10). The child applies them to itself before it reads its
/// request.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct WorkerSettings {
    /// `ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES`: `RLIMIT_AS`, default 85 % of
    /// the cgroup memory limit, else 16 GiB ([`default_worker_memory`]).
    pub memory_bytes: u64,
    /// `ELITEA_DEEPWIKI_WORKER_CPU_SECONDS`: `RLIMIT_CPU` (soft; the hard
    /// limit is 10 s above it), default 4 h.
    pub cpu_seconds: u64,
    /// `ELITEA_DEEPWIKI_WORKER_THREADS`: the child's parser and runtime
    /// threads (`RAYON_NUM_THREADS`, the tokio workers), default the
    /// available parallelism, at most 8. Each parser thread reserves its
    /// stack in the address space the cap counts.
    pub threads: usize,
}

impl Default for WorkerSettings {
    fn default() -> Self {
        Self {
            memory_bytes: DEFAULT_WORKER_MEMORY_BYTES,
            cpu_seconds: DEFAULT_WORKER_CPU_SECONDS,
            threads: default_worker_threads(),
        }
    }
}

/// The available parallelism, at most 8.
#[must_use]
pub fn default_worker_threads() -> usize {
    std::thread::available_parallelism().map_or(4, |n| n.get().min(8))
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
    /// `ELITEA_DEEPWIKI_DATABASE_URL`: the `deepwiki` database (the Python
    /// service's variable). Unset: no index storage, no reconciliation.
    pub database_url: Option<DatabaseUrl>,
    /// `ELITEA_DEEPWIKI_BUILD_OWNER`, else `HOSTNAME` (the pod name): the
    /// identity a build is recorded under. It must survive a restart of
    /// this process and differ between replicas, because the startup
    /// reconciliation deletes this owner's builds from earlier runs. With a
    /// database configured, one of the two must be set
    /// ([`DEFAULT_BUILD_OWNER`] is refused).
    pub build_owner: String,
    /// `ELITEA_DEEPWIKI_BUILD_STALE_SECONDS` (default 2 h, at least 300):
    /// the sweep deletes a build whose heartbeat is older.
    pub build_stale_after: Duration,
    /// `ELITEA_DEEPWIKI_PUBLISH_*`: the publish transaction's timeouts,
    /// `work_mem` and concurrency.
    pub publish: PublishSettings,
    /// The model client's process-wide settings.
    pub model: ModelEnvSettings,
    /// The `generate_wiki` worker child's limits.
    pub worker: WorkerSettings,
    /// `ELITEA_DEEPWIKI_QUERY_POOL_SIZE` (default 8, at most 256): the
    /// connections of the pool that `ask`, `deep_research` and
    /// `resolve_wiki` share in the native runner. A query waits for a free
    /// connection (30 s), so this also caps their concurrent reads. The
    /// clean-up of a killed worker's build has its own pool.
    pub query_pool_size: u32,
}

/// The callback hop's own trust setting (see [`ModelEnvSettings::tls_ca_file`]).
pub const CALLBACK_CA_SETTING: &str = "ELITEA_DEEPWIKI_CALLBACK_CA_FILE";
/// The listener's CA, which the callback hop trusted before the split.
pub const TLS_CA_SETTING: &str = "ELITEA_DEEPWIKI_TLS_CA_FILE";

/// What the environment decides about model calls; the invocation's
/// `llm_settings` decide the rest (`llm::settings`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ModelEnvSettings {
    /// A PEM bundle trusted in addition to the platform roots on the
    /// callback hop (the model gateway and the artifact API behind
    /// `llm_settings.api_base`): `ELITEA_DEEPWIKI_CALLBACK_CA_FILE` — the
    /// runtime CA when that hop is TLS through platform-edge (ADR-0027) —
    /// else `ELITEA_DEEPWIKI_TLS_CA_FILE`, what it read before the two were
    /// separated. The Go host reads the same pair in the same order
    /// (`spi.Settings.CallbackCA`).
    pub tls_ca_file: Option<PathBuf>,
    /// Which of the two variables `tls_ca_file` came from, so an unreadable
    /// bundle names the setting to fix.
    pub tls_ca_setting: &'static str,
    /// `WIKI_EMBED_BATCH_SIZE` (unprefixed: the Python indexer's name):
    /// inputs per embedding request, default 64.
    pub embed_batch_size: usize,
    /// `ELITEA_DEEPWIKI_EMBED_CONCURRENCY`: embedding requests in flight,
    /// default 4. The Python engine sent them one at a time.
    pub embed_concurrency: usize,
    /// `ELITEA_DEEPWIKI_EMBED_CTX_TOKENS`: the embedding window, in
    /// `cl100k_base` tokens, default 8191 (`LangChain`'s). Set it below the
    /// embedding model's own context when that model's tokenizer counts
    /// more tokens than `cl100k_base` for the same text. At least
    /// [`MIN_SPLIT_TOKENS`].
    pub embed_ctx_tokens: usize,
    /// `ELITEA_DEEPWIKI_MODEL_STREAM_TOTAL_SECONDS`: the longest one model
    /// stream may run, default 7200 (Python had no limit).
    pub stream_total: Duration,
}

fn model_settings(
    raw: &impl Fn(&str) -> Option<String>,
    lookup: &impl Fn(&str) -> Option<String>,
) -> Result<ModelEnvSettings, ConfigError> {
    let batch = match lookup("WIKI_EMBED_BATCH_SIZE").filter(|v| !v.is_empty()) {
        None => DEFAULT_BATCH_SIZE,
        Some(text) => match text.trim().parse::<usize>() {
            Ok(value) if value > 0 => value,
            _ => {
                return Err(ConfigError(format!(
                    "WIKI_EMBED_BATCH_SIZE must be a whole number of at least 1, got '{text}'"
                )));
            }
        },
    };
    let concurrency = positive_count(raw, "EMBED_CONCURRENCY", DEFAULT_CONCURRENCY as u64)?;
    let ctx_tokens = positive_count(raw, "EMBED_CTX_TOKENS", EMBEDDING_CTX_LENGTH as u64)?;
    let embed_ctx_tokens = usize::try_from(ctx_tokens)
        .ok()
        .filter(|tokens| *tokens >= MIN_SPLIT_TOKENS)
        .ok_or_else(|| {
            ConfigError(format!(
                "{ENV_PREFIX}EMBED_CTX_TOKENS must be a whole number of at least {MIN_SPLIT_TOKENS}, got '{ctx_tokens}'"
            ))
        })?;
    Ok(ModelEnvSettings {
        tls_ca_file: raw("CALLBACK_CA_FILE")
            .or_else(|| raw("TLS_CA_FILE"))
            .map(PathBuf::from),
        tls_ca_setting: if raw("CALLBACK_CA_FILE").is_some() {
            CALLBACK_CA_SETTING
        } else {
            TLS_CA_SETTING
        },
        embed_batch_size: batch,
        embed_concurrency: usize::try_from(concurrency)
            .map_err(|_| ConfigError(format!("{ENV_PREFIX}EMBED_CONCURRENCY is out of range")))?,
        embed_ctx_tokens,
        stream_total: positive_seconds(
            raw,
            "MODEL_STREAM_TOTAL_SECONDS",
            crate::llm::Timeouts::default().stream_total,
        )?,
    })
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

/// [`positive_seconds`], at most `max`.
fn bounded_seconds(
    raw: &impl Fn(&str) -> Option<String>,
    name: &str,
    default: Duration,
    max: Duration,
) -> Result<Duration, ConfigError> {
    let value = positive_seconds(raw, name, default)?;
    if value > max {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}{name} must be at most {} seconds, got {}",
            max.as_secs(),
            value.as_secs_f64()
        )));
    }
    Ok(value)
}

/// [`positive_count`], at most `max`.
fn bounded_count(
    raw: &impl Fn(&str) -> Option<String>,
    name: &str,
    default: u32,
    max: u32,
) -> Result<u32, ConfigError> {
    let value = positive_count(raw, name, u64::from(default))?;
    u32::try_from(value)
        .ok()
        .filter(|v| *v <= max)
        .ok_or_else(|| {
            ConfigError(format!(
                "{ENV_PREFIX}{name} must be at most {max}, got {value}"
            ))
        })
}

fn publish_settings(raw: &impl Fn(&str) -> Option<String>) -> Result<PublishSettings, ConfigError> {
    let defaults = PublishSettings::default();
    // PostgreSQL holds a timeout in milliseconds in a 32-bit integer
    // (about 24.8 days); a day is far beyond any publish.
    let day = Duration::from_hours(24);
    Ok(PublishSettings {
        statement_timeout: bounded_seconds(
            raw,
            "PUBLISH_STATEMENT_TIMEOUT_SECONDS",
            defaults.statement_timeout,
            day,
        )?,
        lock_timeout: bounded_seconds(
            raw,
            "PUBLISH_LOCK_TIMEOUT_SECONDS",
            defaults.lock_timeout,
            day,
        )?,
        work_mem_mb: bounded_count(raw, "PUBLISH_WORK_MEM_MB", defaults.work_mem_mb, 4096)?,
        slots: bounded_count(raw, "PUBLISH_SLOTS", defaults.slots, 64)?,
        analyze_lock_timeout: bounded_seconds(
            raw,
            "PUBLISH_ANALYZE_LOCK_TIMEOUT_SECONDS",
            defaults.analyze_lock_timeout,
            day,
        )?,
        delete_lock_wait: bounded_seconds(
            raw,
            "PUBLISH_DELETE_LOCK_WAIT_SECONDS",
            defaults.delete_lock_wait,
            day,
        )?,
    })
}

/// The settings the index-maintenance command line (`orphans`) shares with
/// the server: the publish transaction's timeouts (a deletion moves a whole
/// index, as a publish does) and the build staleness limit. Nothing else is
/// read, so the command runs with only the database URL and these.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct MaintenanceSettings {
    /// `ELITEA_DEEPWIKI_PUBLISH_*`, as [`Settings::publish`].
    pub publish: PublishSettings,
    /// `ELITEA_DEEPWIKI_BUILD_STALE_SECONDS`, as [`Settings::build_stale_after`].
    pub build_stale_after: Duration,
}

impl MaintenanceSettings {
    /// Read the settings through `lookup` (the process environment, or a
    /// test's map), strictly: an unparsable value is an error, never a
    /// fallback to the default.
    ///
    /// # Errors
    ///
    /// [`ConfigError`] naming the variable.
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, ConfigError> {
        let raw = |name: &str| lookup(&format!("{ENV_PREFIX}{name}")).filter(|v| !v.is_empty());
        Ok(Self {
            publish: publish_settings(&raw)?,
            build_stale_after: build_stale_after(&raw)?,
        })
    }

    /// [`MaintenanceSettings::from_lookup`] over the process environment.
    ///
    /// # Errors
    ///
    /// [`ConfigError`] naming the variable.
    pub fn from_env() -> Result<Self, ConfigError> {
        Self::from_lookup(|name| std::env::var(name).ok())
    }
}

/// `ELITEA_DEEPWIKI_BUILD_STALE_SECONDS`, at least [`MIN_STALE_AFTER`].
fn build_stale_after(raw: &impl Fn(&str) -> Option<String>) -> Result<Duration, ConfigError> {
    let stale_after = positive_seconds(
        raw,
        "BUILD_STALE_SECONDS",
        crate::storage::build::DEFAULT_STALE_AFTER,
    )?;
    if stale_after < MIN_STALE_AFTER {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}BUILD_STALE_SECONDS must be at least {} (a live build beats its heartbeat every tenth of it and must survive a few missed beats), got {}",
            MIN_STALE_AFTER.as_secs(),
            stale_after.as_secs_f64()
        )));
    }
    Ok(stale_after)
}

/// The build owner: `ELITEA_DEEPWIKI_BUILD_OWNER`, else `HOSTNAME`. With a
/// database, the shared default is refused.
fn build_owner(
    raw: &impl Fn(&str) -> Option<String>,
    lookup: &impl Fn(&str) -> Option<String>,
    has_database: bool,
) -> Result<String, ConfigError> {
    let owner = raw("BUILD_OWNER")
        .map(|v| v.trim().to_owned())
        .filter(|v| !v.is_empty())
        .or_else(|| {
            lookup("HOSTNAME")
                .map(|v| v.trim().to_owned())
                .filter(|v| !v.is_empty())
        })
        .unwrap_or_else(|| DEFAULT_BUILD_OWNER.to_owned());
    if has_database && owner == DEFAULT_BUILD_OWNER {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}DATABASE_URL is set, but the build owner is the shared default '{DEFAULT_BUILD_OWNER}' (neither {ENV_PREFIX}BUILD_OWNER nor HOSTNAME is set). The build reconciliation is keyed by owner, so engines that share it can delete each other's builds. Set {ENV_PREFIX}BUILD_OWNER (or HOSTNAME) to a name that is unique to this engine and stays the same across its restarts (in Kubernetes, the pod name)."
        )));
    }
    Ok(owner)
}

fn worker_settings(
    raw: &impl Fn(&str) -> Option<String>,
    cgroup_memory_max: Option<u64>,
) -> Result<WorkerSettings, ConfigError> {
    let defaults = WorkerSettings::default();
    let memory_bytes = positive_count(
        raw,
        "WORKER_MEMORY_BYTES",
        default_worker_memory(cgroup_memory_max),
    )?;
    if memory_bytes < MIN_WORKER_MEMORY_BYTES {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}WORKER_MEMORY_BYTES must be at least {MIN_WORKER_MEMORY_BYTES} (1 GiB of address space), got {memory_bytes}"
        )));
    }
    let cpu_seconds = positive_count(raw, "WORKER_CPU_SECONDS", defaults.cpu_seconds)?;
    if cpu_seconds < MIN_WORKER_CPU_SECONDS {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}WORKER_CPU_SECONDS must be at least {MIN_WORKER_CPU_SECONDS}, got {cpu_seconds}"
        )));
    }
    // Unset: the default, lowered until its stacks fit the memory cap.
    let fitting =
        memory_bytes.saturating_sub(WORKER_MEMORY_HEADROOM_BYTES) / WORKER_THREAD_RESERVE_BYTES;
    let default_threads = (defaults.threads as u64).min(fitting).max(1);
    let threads = positive_count(raw, "WORKER_THREADS", default_threads)?;
    if threads > MAX_WORKER_THREADS {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}WORKER_THREADS must be at most {MAX_WORKER_THREADS}, got {threads}"
        )));
    }
    Ok(WorkerSettings {
        memory_bytes,
        cpu_seconds,
        threads: usize::try_from(threads)
            .map_err(|_| ConfigError(format!("{ENV_PREFIX}WORKER_THREADS is out of range")))?,
    })
}

/// Refuse a thread count whose parser stacks do not fit the worker's
/// address-space cap: every parser pool would fail to start (or the run
/// would die of an allocation failure) on every repository.
fn check_worker_fits(worker: &WorkerSettings) -> Result<(), ConfigError> {
    let threads = worker.threads as u64;
    let needed = worker_memory_needed(threads);
    if needed > worker.memory_bytes {
        return Err(ConfigError(format!(
            "{ENV_PREFIX}WORKER_THREADS={threads} does not fit {ENV_PREFIX}WORKER_MEMORY_BYTES={}: each worker thread reserves {WORKER_THREAD_RESERVE_BYTES} bytes of stack and the worker needs {WORKER_MEMORY_HEADROOM_BYTES} bytes more, so {threads} threads need {needed} bytes of address space. Lower the threads or raise the memory cap.",
            worker.memory_bytes
        )));
    }
    Ok(())
}

/// The ingest settings under this engine's names: every limit, timeout and
/// allowlist refusal names the `ELITEA_DEEPWIKI_*` variable to change, and
/// the clone identifies itself as this engine.
pub static INGEST_NAMES: crate::ingest::names::SettingNames = crate::ingest::names::SettingNames {
    max_clone_bytes: "ELITEA_DEEPWIKI_MAX_CLONE_BYTES",
    max_file_count: "ELITEA_DEEPWIKI_MAX_FILE_COUNT",
    max_file_bytes: "ELITEA_DEEPWIKI_MAX_FILE_BYTES",
    max_parsed_bytes: "ELITEA_DEEPWIKI_MAX_PARSED_BYTES",
    clone_timeout_seconds: "ELITEA_DEEPWIKI_CLONE_TIMEOUT_SECONDS",
    artifact_max_files: "ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES",
    artifact_max_bytes: "ELITEA_DEEPWIKI_ARTIFACT_MAX_BYTES",
    git_allowlist: "ELITEA_DEEPWIKI_GIT_ALLOWLIST",
    user_agent: concat!("elitea-deepwiki-engine/", env!("CARGO_PKG_VERSION")),
};

fn ingest_settings(raw: &impl Fn(&str) -> Option<String>) -> Result<IngestSettings, ConfigError> {
    let defaults = IngestLimits::default();
    Ok(IngestSettings {
        // Fail-closed when unset: the policy is empty and refuses every
        // clone (security/egress.py, spi.ParseEgressPolicy).
        git_allowlist: EgressPolicy::parse(raw("GIT_ALLOWLIST").as_deref()).named(&INGEST_NAMES),
        limits: IngestLimits {
            max_clone_bytes: positive_count(raw, "MAX_CLONE_BYTES", defaults.max_clone_bytes)?,
            max_file_count: positive_count(raw, "MAX_FILE_COUNT", defaults.max_file_count)?,
            max_file_bytes: positive_count(raw, "MAX_FILE_BYTES", defaults.max_file_bytes)?,
            max_parsed_bytes: positive_count(raw, "MAX_PARSED_BYTES", defaults.max_parsed_bytes)?,
            clone_timeout: positive_seconds(raw, "CLONE_TIMEOUT_SECONDS", defaults.clone_timeout)?,
            names: &INGEST_NAMES,
        },
        scratch_path: PathBuf::from(
            raw("SCRATCH_PATH").unwrap_or_else(|| DEFAULT_SCRATCH_PATH.to_owned()),
        ),
        // Python's artifact_source caps, under Python's names.
        artifact: ArtifactCaps {
            max_files: positive_count(
                raw,
                "ARTIFACT_MAX_FILES",
                ArtifactCaps::default().max_files,
            )?,
            max_bytes: positive_count(
                raw,
                "ARTIFACT_MAX_BYTES",
                ArtifactCaps::default().max_bytes,
            )?,
        },
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
        Self::from_lookup_and_memory_limit(
            lookup,
            crate::cgroup::Cgroup::discover().and_then(|c| c.memory_max()),
        )
    }

    /// [`Settings::from_lookup`] with the container's memory limit given
    /// (cgroup v2 `memory.max`, `None` for none), which sets the default of
    /// `ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES`.
    ///
    /// # Errors
    ///
    /// See [`Settings::from_lookup`].
    pub fn from_lookup_and_memory_limit(
        lookup: impl Fn(&str) -> Option<String>,
        cgroup_memory_max: Option<u64>,
    ) -> Result<Self, ConfigError> {
        let raw = |name: &str| lookup(&format!("{ENV_PREFIX}{name}")).filter(|v| !v.is_empty());
        let runner = match raw("RUNNER").as_deref().map(str::trim) {
            None | Some("unavailable") => RunnerKind::Unavailable,
            Some("fixture") => RunnerKind::Fixture,
            Some("native") => RunnerKind::Native,
            Some("legacy") => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER=legacy named the Python engine, which is retired; use native (see docs/UPGRADING.md)"
                )));
            }
            Some(other) => {
                return Err(ConfigError(format!(
                    "{ENV_PREFIX}RUNNER must be one of ['unavailable', 'fixture', 'native'], got '{other}'"
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
        let model = model_settings(&raw, &lookup)?;
        let database_url = raw("DATABASE_URL")
            .map(|url| url.trim().to_owned())
            .filter(|url| !url.is_empty())
            .map(DatabaseUrl);
        let build_owner = build_owner(&raw, &lookup, database_url.is_some())?;
        let build_stale_after = build_stale_after(&raw)?;
        let publish = publish_settings(&raw)?;
        let worker = worker_settings(&raw, cgroup_memory_max)?;
        let query_pool_size = bounded_count(
            &raw,
            "QUERY_POOL_SIZE",
            DEFAULT_QUERY_POOL_SIZE,
            MAX_QUERY_POOL_SIZE,
        )?;
        if runner == RunnerKind::Native {
            check_worker_fits(&worker)?;
        }
        if runner == RunnerKind::Native && database_url.is_none() {
            return Err(ConfigError(format!(
                "{ENV_PREFIX}RUNNER=native needs {ENV_PREFIX}DATABASE_URL: the native engine stages and publishes every index in the deepwiki PostgreSQL database (ADR-0026 decision 5) and has no other index storage. Set it to the database the migrations ran on, or use 'fixture' or 'unavailable'."
            )));
        }
        Ok(Self {
            runner,
            fixture_step,
            engine_socket,
            ingest,
            database_url,
            build_owner,
            build_stale_after,
            publish,
            model,
            worker,
            query_pool_size,
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
        Settings::from_lookup_and_memory_limit(|name| map.get(name).cloned(), None)
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
        // Native without a database: refused at start, naming the setting.
        let native = settings(&[("ELITEA_DEEPWIKI_RUNNER", "native")]);
        assert!(
            matches!(&native, Err(ConfigError(m)) if m.contains("ELITEA_DEEPWIKI_DATABASE_URL")),
            "{native:?}"
        );
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
                git_allowlist: EgressPolicy::parse(None).named(&INGEST_NAMES),
                limits: IngestLimits {
                    names: &INGEST_NAMES,
                    ..IngestLimits::default()
                },
                scratch_path: PathBuf::from(DEFAULT_SCRATCH_PATH),
                artifact: ArtifactCaps {
                    max_files: 5000,
                    max_bytes: 512 * 1024 * 1024,
                },
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
            ("ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES", "12"),
            ("ELITEA_DEEPWIKI_ARTIFACT_MAX_BYTES", "4096"),
        ])
        .map(|s| s.ingest);
        assert_eq!(
            parsed,
            Ok(IngestSettings {
                git_allowlist: EgressPolicy::parse(Some("github.com *.github.com"))
                    .named(&INGEST_NAMES),
                limits: IngestLimits {
                    max_clone_bytes: 1_048_576,
                    max_file_count: 10,
                    max_file_bytes: 2048,
                    max_parsed_bytes: 4096,
                    clone_timeout: Duration::from_millis(2500),
                    names: &INGEST_NAMES,
                },
                scratch_path: PathBuf::from("/scratch"),
                artifact: ArtifactCaps {
                    max_files: 12,
                    max_bytes: 4096,
                },
            })
        );
    }

    /// The shared ingest crate names whatever setting it is handed; this
    /// engine's refusals must name ITS variables, as the Python engine's did.
    #[test]
    fn ingest_refusals_name_this_engines_settings() {
        let Ok(parsed) = settings(&[]) else {
            panic!("the defaults load");
        };
        let ingest = parsed.ingest;
        let refused = ingest
            .git_allowlist
            .check("github.com", "clone destination");
        assert!(
            refused.is_err_and(|e| e.message.contains("Set ELITEA_DEEPWIKI_GIT_ALLOWLIST to")),
            "the allowlist refusal names the variable"
        );
        let too_big = ingest.limits.clone_bytes_error("o/r", u64::MAX);
        assert!(
            too_big
                .message
                .contains("over ELITEA_DEEPWIKI_MAX_CLONE_BYTES="),
            "{too_big}"
        );
        assert!(
            ingest
                .limits
                .names
                .user_agent
                .starts_with("elitea-deepwiki-engine/")
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
            "ARTIFACT_MAX_FILES",
            "ARTIFACT_MAX_BYTES",
        ] {
            for bad in ["0", "-1", "x", "1e400"] {
                let key = format!("ELITEA_DEEPWIKI_{name}");
                assert!(settings(&[(key.as_str(), bad)]).is_err(), "{name}={bad}");
            }
        }
        assert!(settings(&[("ELITEA_DEEPWIKI_MAX_FILE_COUNT", "1.5")]).is_err());
    }

    #[test]
    fn storage_settings_default_and_parse() {
        let parsed = settings(&[]);
        assert_eq!(parsed.as_ref().map(|s| s.database_url.clone()), Ok(None));
        assert_eq!(
            parsed.as_ref().map(|s| s.build_owner.clone()),
            Ok(DEFAULT_BUILD_OWNER.to_owned())
        );
        assert_eq!(
            parsed.map(|s| s.build_stale_after),
            Ok(Duration::from_hours(2))
        );
        let parsed = settings(&[
            (
                "ELITEA_DEEPWIKI_DATABASE_URL",
                "postgresql://u:secret@db/deepwiki",
            ),
            ("HOSTNAME", "deepwiki-7f9c"),
            ("ELITEA_DEEPWIKI_BUILD_STALE_SECONDS", "600"),
        ]);
        let Ok(parsed) = parsed else {
            panic!("settings refused");
        };
        assert_eq!(parsed.build_owner, "deepwiki-7f9c");
        assert_eq!(parsed.build_stale_after, Duration::from_mins(10));
        assert_eq!(parsed.query_pool_size, DEFAULT_QUERY_POOL_SIZE);
        // The password never reaches a Debug form.
        assert!(!format!("{parsed:?}").contains("secret"));
        assert_eq!(
            parsed.database_url.as_ref().map(DatabaseUrl::expose),
            Some("postgresql://u:secret@db/deepwiki")
        );
        let owner = settings(&[
            ("HOSTNAME", "pod"),
            ("ELITEA_DEEPWIKI_BUILD_OWNER", "replica-a"),
        ]);
        assert_eq!(owner.map(|s| s.build_owner), Ok("replica-a".to_owned()));
        assert!(settings(&[("ELITEA_DEEPWIKI_BUILD_STALE_SECONDS", "0")]).is_err());
    }

    #[test]
    fn the_query_pool_size_parses_strictly() {
        let size = |value: &str| {
            settings(&[("ELITEA_DEEPWIKI_QUERY_POOL_SIZE", value)]).map(|s| s.query_pool_size)
        };
        assert_eq!(size("16"), Ok(16));
        assert_eq!(size(" 256 "), Ok(MAX_QUERY_POOL_SIZE));
        for refused in ["0", "257", "-1", "eight"] {
            assert!(
                matches!(size(refused), Err(ConfigError(m)) if m.contains("QUERY_POOL_SIZE")),
                "{refused}"
            );
        }
    }

    #[test]
    fn a_database_refuses_the_shared_default_owner() {
        let dsn = (
            "ELITEA_DEEPWIKI_DATABASE_URL",
            "postgresql://u:p@db/deepwiki",
        );
        let refused = settings(&[dsn]);
        assert!(
            matches!(&refused, Err(ConfigError(m)) if m.contains("ELITEA_DEEPWIKI_BUILD_OWNER") && m.contains("HOSTNAME")),
            "{refused:?}"
        );
        // Blank values do not count as set.
        assert!(
            settings(&[
                dsn,
                ("HOSTNAME", "  "),
                ("ELITEA_DEEPWIKI_BUILD_OWNER", " ")
            ])
            .is_err()
        );
        // Naming the default explicitly is the same sharing.
        assert!(settings(&[dsn, ("ELITEA_DEEPWIKI_BUILD_OWNER", DEFAULT_BUILD_OWNER)]).is_err());
        assert_eq!(
            settings(&[dsn, ("ELITEA_DEEPWIKI_BUILD_OWNER", "replica-a")]).map(|s| s.build_owner),
            Ok("replica-a".to_owned())
        );
        assert_eq!(
            settings(&[dsn, ("HOSTNAME", "pod-1")]).map(|s| s.build_owner),
            Ok("pod-1".to_owned())
        );
    }

    #[test]
    fn the_stale_limit_has_a_floor() {
        assert!(settings(&[("ELITEA_DEEPWIKI_BUILD_STALE_SECONDS", "299")]).is_err());
        assert!(settings(&[("ELITEA_DEEPWIKI_BUILD_STALE_SECONDS", "60")]).is_err());
        assert_eq!(
            settings(&[("ELITEA_DEEPWIKI_BUILD_STALE_SECONDS", "300")])
                .map(|s| s.build_stale_after),
            Ok(Duration::from_mins(5))
        );
    }

    #[test]
    fn the_maintenance_settings_are_the_servers_publish_and_staleness_settings() {
        let lookup = |pairs: &'static [(&'static str, &'static str)]| {
            move |name: &str| {
                pairs
                    .iter()
                    .find(|(key, _)| *key == name)
                    .map(|(_, value)| (*value).to_owned())
            }
        };
        assert_eq!(
            MaintenanceSettings::from_lookup(lookup(&[])),
            Ok(MaintenanceSettings {
                publish: PublishSettings::default(),
                build_stale_after: crate::storage::build::DEFAULT_STALE_AFTER,
            })
        );
        let configured = MaintenanceSettings::from_lookup(lookup(&[
            ("ELITEA_DEEPWIKI_PUBLISH_STATEMENT_TIMEOUT_SECONDS", "3600"),
            ("ELITEA_DEEPWIKI_PUBLISH_LOCK_TIMEOUT_SECONDS", "2.5"),
            ("ELITEA_DEEPWIKI_BUILD_STALE_SECONDS", "900"),
        ]))
        .expect("parse");
        assert_eq!(
            configured.publish.statement_timeout,
            Duration::from_hours(1)
        );
        assert_eq!(configured.publish.lock_timeout, Duration::from_millis(2500));
        assert_eq!(configured.build_stale_after, Duration::from_mins(15));
        // The same strictness as the server: a bad value is an error.
        for bad in [
            ("ELITEA_DEEPWIKI_PUBLISH_SLOTS", "0"),
            ("ELITEA_DEEPWIKI_BUILD_STALE_SECONDS", "60"),
        ] {
            let pairs: &'static [(&str, &str)] = Box::leak(Box::new([bad]));
            assert!(
                MaintenanceSettings::from_lookup(lookup(pairs)).is_err(),
                "{bad:?}"
            );
        }
    }

    #[test]
    fn the_publish_settings_default_and_parse() {
        assert_eq!(
            settings(&[]).map(|s| s.publish),
            Ok(PublishSettings {
                statement_timeout: Duration::from_mins(30),
                lock_timeout: Duration::from_secs(30),
                work_mem_mb: 64,
                slots: 2,
                analyze_lock_timeout: Duration::from_secs(5),
                delete_lock_wait: Duration::from_secs(30),
            })
        );
        assert_eq!(
            settings(&[
                ("ELITEA_DEEPWIKI_PUBLISH_STATEMENT_TIMEOUT_SECONDS", "3600"),
                ("ELITEA_DEEPWIKI_PUBLISH_LOCK_TIMEOUT_SECONDS", "2.5"),
                ("ELITEA_DEEPWIKI_PUBLISH_WORK_MEM_MB", "256"),
                ("ELITEA_DEEPWIKI_PUBLISH_SLOTS", "1"),
                ("ELITEA_DEEPWIKI_PUBLISH_ANALYZE_LOCK_TIMEOUT_SECONDS", "1"),
                ("ELITEA_DEEPWIKI_PUBLISH_DELETE_LOCK_WAIT_SECONDS", "7"),
            ])
            .map(|s| s.publish),
            Ok(PublishSettings {
                statement_timeout: Duration::from_hours(1),
                lock_timeout: Duration::from_millis(2500),
                work_mem_mb: 256,
                slots: 1,
                analyze_lock_timeout: Duration::from_secs(1),
                delete_lock_wait: Duration::from_secs(7),
            })
        );
        for (name, bad) in [
            ("PUBLISH_STATEMENT_TIMEOUT_SECONDS", "0"),
            ("PUBLISH_STATEMENT_TIMEOUT_SECONDS", "x"),
            ("PUBLISH_STATEMENT_TIMEOUT_SECONDS", "86401"),
            ("PUBLISH_LOCK_TIMEOUT_SECONDS", "-1"),
            ("PUBLISH_DELETE_LOCK_WAIT_SECONDS", "0"),
            ("PUBLISH_WORK_MEM_MB", "0"),
            ("PUBLISH_WORK_MEM_MB", "64MB"),
            ("PUBLISH_WORK_MEM_MB", "4097"),
            ("PUBLISH_SLOTS", "0"),
            ("PUBLISH_SLOTS", "65"),
            ("PUBLISH_SLOTS", "1.5"),
            ("PUBLISH_ANALYZE_LOCK_TIMEOUT_SECONDS", "inf"),
        ] {
            let key = format!("ELITEA_DEEPWIKI_{name}");
            assert!(settings(&[(key.as_str(), bad)]).is_err(), "{name}={bad}");
        }
    }

    #[test]
    fn native_runs_with_a_database() {
        let parsed = settings(&[
            ("ELITEA_DEEPWIKI_RUNNER", "native"),
            (
                "ELITEA_DEEPWIKI_DATABASE_URL",
                "postgresql://u:p@db/deepwiki",
            ),
            ("ELITEA_DEEPWIKI_BUILD_OWNER", "replica-a"),
        ]);
        assert_eq!(parsed.map(|s| s.runner), Ok(RunnerKind::Native));
    }

    #[test]
    fn the_worker_limits_default_and_parse_strictly() {
        let defaults = settings(&[]).map(|s| s.worker);
        assert_eq!(defaults, Ok(WorkerSettings::default()));
        assert_eq!(
            WorkerSettings::default().memory_bytes,
            DEFAULT_WORKER_MEMORY_BYTES
        );
        let set = settings(&[
            ("ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES", "2147483648"),
            ("ELITEA_DEEPWIKI_WORKER_CPU_SECONDS", "600"),
            ("ELITEA_DEEPWIKI_WORKER_THREADS", "2"),
        ])
        .map(|s| s.worker);
        assert_eq!(
            set,
            Ok(WorkerSettings {
                memory_bytes: 2 << 30,
                cpu_seconds: 600,
                threads: 2,
            })
        );
        for (name, bad) in [
            ("WORKER_MEMORY_BYTES", "0"),
            ("WORKER_MEMORY_BYTES", "16GiB"),
            ("WORKER_MEMORY_BYTES", "1048576"),
            ("WORKER_CPU_SECONDS", "-1"),
            ("WORKER_CPU_SECONDS", "59"),
            ("WORKER_CPU_SECONDS", "1.5"),
            ("WORKER_THREADS", "0"),
            ("WORKER_THREADS", "257"),
        ] {
            let key = format!("ELITEA_DEEPWIKI_{name}");
            assert!(settings(&[(key.as_str(), bad)]).is_err(), "{name}={bad}");
        }
    }

    #[test]
    fn the_memory_cap_defaults_to_the_cgroup_limit() {
        let read = |limit: Option<u64>, pairs: &[(&str, &str)]| {
            let map: HashMap<String, String> = pairs
                .iter()
                .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
                .collect();
            Settings::from_lookup_and_memory_limit(|name| map.get(name).cloned(), limit)
                .map(|s| s.worker.memory_bytes)
        };
        assert_eq!(read(None, &[]), Ok(DEFAULT_WORKER_MEMORY_BYTES));
        // 85 % of an 8 GiB container.
        assert_eq!(read(Some(8 << 30), &[]), Ok((8 << 30) / 100 * 85));
        // Never below the floor.
        assert_eq!(read(Some(512 << 20), &[]), Ok(MIN_WORKER_MEMORY_BYTES));
        // The setting beats the cgroup.
        assert_eq!(
            read(
                Some(8 << 30),
                &[("ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES", "2147483648")]
            ),
            Ok(2 << 30)
        );
    }

    #[test]
    fn native_threads_must_fit_the_memory_cap() {
        let native = [
            ("ELITEA_DEEPWIKI_RUNNER", "native"),
            (
                "ELITEA_DEEPWIKI_DATABASE_URL",
                "postgresql://u:p@db/deepwiki",
            ),
            ("ELITEA_DEEPWIKI_BUILD_OWNER", "replica-a"),
        ];
        let with = |extra: &[(&'static str, &'static str)]| {
            let mut pairs = native.to_vec();
            pairs.extend_from_slice(extra);
            settings(&pairs)
        };
        // 8 threads × 256 MiB + 1 GiB = 3 GiB > 2 GiB.
        let refused = with(&[
            ("ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES", "2147483648"),
            ("ELITEA_DEEPWIKI_WORKER_THREADS", "8"),
        ]);
        assert!(
            matches!(&refused, Err(ConfigError(m)) if m.contains("WORKER_THREADS=8") && m.contains("3221225472")),
            "{refused:?}"
        );
        // 4 threads need exactly 2 GiB.
        let fits = with(&[
            ("ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES", "2147483648"),
            ("ELITEA_DEEPWIKI_WORKER_THREADS", "4"),
        ]);
        assert_eq!(fits.map(|s| s.worker.threads), Ok(4));
        // Unset threads: the default is lowered to fit (at least 1).
        let lowered = with(&[("ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES", "1610612736")]);
        assert_eq!(
            lowered.map(|s| s.worker.threads),
            Ok(default_worker_threads().min(2))
        );
        // 1 GiB holds no parser thread at all.
        assert!(with(&[("ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES", "1073741824")]).is_err());
        // The fixture runner never starts a worker: not checked.
        assert!(
            settings(&[
                ("ELITEA_DEEPWIKI_RUNNER", "fixture"),
                ("ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES", "1073741824"),
                ("ELITEA_DEEPWIKI_WORKER_THREADS", "8"),
            ])
            .is_ok()
        );
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

    #[test]
    fn the_model_settings_read_their_python_names() {
        let defaults = settings(&[]).map(|s| s.model);
        assert_eq!(
            defaults,
            Ok(ModelEnvSettings {
                tls_ca_file: None,
                tls_ca_setting: TLS_CA_SETTING,
                embed_batch_size: DEFAULT_BATCH_SIZE,
                embed_concurrency: DEFAULT_CONCURRENCY,
                embed_ctx_tokens: EMBEDDING_CTX_LENGTH,
                stream_total: Duration::from_hours(2),
            })
        );
        let set = settings(&[
            ("ELITEA_DEEPWIKI_TLS_CA_FILE", "/etc/ca.pem"),
            ("WIKI_EMBED_BATCH_SIZE", "16"),
            ("ELITEA_DEEPWIKI_EMBED_CONCURRENCY", "2"),
            ("ELITEA_DEEPWIKI_EMBED_CTX_TOKENS", "4096"),
            ("ELITEA_DEEPWIKI_MODEL_STREAM_TOTAL_SECONDS", "10800"),
        ])
        .map(|s| s.model);
        assert_eq!(
            set,
            Ok(ModelEnvSettings {
                tls_ca_file: Some(PathBuf::from("/etc/ca.pem")),
                tls_ca_setting: TLS_CA_SETTING,
                embed_batch_size: 16,
                embed_concurrency: 2,
                embed_ctx_tokens: 4096,
                stream_total: Duration::from_hours(3),
            })
        );
        // The callback hop's own CA wins over the listener's, and a CA-file
        // error then names the variable that was actually set.
        let split = settings(&[
            ("ELITEA_DEEPWIKI_TLS_CA_FILE", "/provider-ca.pem"),
            ("ELITEA_DEEPWIKI_CALLBACK_CA_FILE", "/runtime-ca.pem"),
        ])
        .map(|s| (s.model.tls_ca_file, s.model.tls_ca_setting));
        assert_eq!(
            split,
            Ok((Some(PathBuf::from("/runtime-ca.pem")), CALLBACK_CA_SETTING))
        );
        assert!(settings(&[("WIKI_EMBED_BATCH_SIZE", "0")]).is_err());
        for bad in ["0", "255", "-1", "8k"] {
            let refused = settings(&[("ELITEA_DEEPWIKI_EMBED_CTX_TOKENS", bad)]);
            assert!(
                refused
                    .as_ref()
                    .is_err_and(|e| e.to_string().contains("EMBED_CTX_TOKENS")),
                "'{bad}': {refused:?}"
            );
        }
        assert_eq!(
            settings(&[("ELITEA_DEEPWIKI_EMBED_CTX_TOKENS", "256")])
                .map(|s| s.model.embed_ctx_tokens),
            Ok(256)
        );
        assert!(settings(&[("ELITEA_DEEPWIKI_MODEL_STREAM_TOTAL_SECONDS", "0")]).is_err());
    }
}

// The model client is a shared crate (libs/rust/model-client, ADR-0027) and
// knows nothing of this engine's environment; these map it onto the client.
impl From<&ModelEnvSettings> for crate::llm::TransportSettings {
    fn from(settings: &ModelEnvSettings) -> Self {
        Self {
            ca_file: settings.tls_ca_file.clone(),
            ca_file_setting: settings.tls_ca_setting,
            user_agent: concat!("elitea-deepwiki-engine/", env!("CARGO_PKG_VERSION")),
            timeouts: crate::llm::Timeouts {
                stream_total: settings.stream_total,
                ..crate::llm::Timeouts::default()
            },
            ..Self::default()
        }
    }
}

impl From<&ModelEnvSettings> for crate::llm::EmbeddingOptions {
    fn from(settings: &ModelEnvSettings) -> Self {
        Self {
            batch_size: settings.embed_batch_size,
            concurrency: settings.embed_concurrency,
            ctx_length: settings.embed_ctx_tokens,
            ctx_setting: "ELITEA_DEEPWIKI_EMBED_CTX_TOKENS",
        }
    }
}

#[cfg(test)]
mod model_env_tests {
    use super::*;
    use crate::llm::EmbeddingOptions;

    #[test]
    fn the_embedding_window_comes_from_the_environment() {
        let settings = ModelEnvSettings {
            tls_ca_file: None,
            tls_ca_setting: TLS_CA_SETTING,
            embed_batch_size: 16,
            embed_concurrency: 3,
            embed_ctx_tokens: 4096,
            stream_total: std::time::Duration::from_mins(1),
        };
        assert_eq!(
            EmbeddingOptions::from(&settings),
            EmbeddingOptions {
                batch_size: 16,
                concurrency: 3,
                ctx_length: 4096,
                ctx_setting: "ELITEA_DEEPWIKI_EMBED_CTX_TOKENS",
            }
        );
    }
}
