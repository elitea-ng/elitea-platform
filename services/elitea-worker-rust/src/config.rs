//! Strict file-backed production worker configuration.
//!
//! The deployment document carries only identities, endpoints, limits and
//! paths. Credentials remain in separately permissioned regular files and are
//! loaded by the owning transport when it constructs one dependency
//! generation.

use std::fmt;
use std::fs::File;
use std::io::Read as _;
use std::path::{Path, PathBuf};

use http::Uri;
use rustix::fs::{FileType, Mode, OFlags, fstat, openat};
use rustix::process::geteuid;
use serde::Deserialize;

use crate::protocol::command::LIMITS_REVISION;

pub const RUNTIME_DEPLOY_SCHEMA_VERSION: &str = "elitea.runtime-deploy.v1";

const MAX_CONFIG_BYTES: usize = 64 * 1024;
const MAX_IDENTITY_BYTES: usize = 256;
const MAX_TARGET_BYTES: usize = 512;
const MAX_ORIGIN_BYTES: usize = 2_048;
const RUNTIME_TRANSPORT_MESSAGE_BYTES: usize =
    crate::transport::command_bus::MAX_TRANSPORT_MESSAGE_BYTES;
const RUNTIME_TRANSPORT_PAYLOAD_BYTES: usize =
    crate::transport::command_bus::MAX_TRANSPORT_PAYLOAD_BYTES;
// Match Main's admitted agent bundle and the claim-bound content contract.
// A smaller fetch limit rejects saved history before compaction can run.
const RUNTIME_INPUT_CONTENT_BYTES: usize = 8 * 1024 * 1024;
const RUNTIME_OUTPUT_FRAME_BYTES: usize = 64 * 1024;
const RUNTIME_GRPC_REQUEST_BYTES: usize = 64 * 1024;
const RUNTIME_GRPC_RESPONSE_BYTES: usize = 80 * 1024;
const MAX_LEASE_POLL_INTERVAL_MILLIS: u64 = 10_000;

/// Stable, data-free deployment configuration failure.
#[derive(Debug)]
pub enum RuntimeConfigError {
    InvalidConfiguration(&'static str),
    ResourceExhausted(&'static str),
    Unavailable {
        message: &'static str,
        source: std::io::Error,
    },
}

impl fmt::Display for RuntimeConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message)
            | Self::ResourceExhausted(message)
            | Self::Unavailable { message, .. } => formatter.write_str(message),
        }
    }
}

impl std::error::Error for RuntimeConfigError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Unavailable { source, .. } => Some(source),
            Self::InvalidConfiguration(_) | Self::ResourceExhausted(_) => None,
        }
    }
}

/// Deployment-selected identities and file locations.
#[derive(Clone, Debug, Eq, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RuntimeDeployConfig {
    pub schema_version: String,
    pub limits_revision: String,
    pub workload_session_id: String,
    pub producer_id: String,
    pub consumer_id: String,
    /// `tls://host:port` with the mTLS trio below, `nats://host:port`
    /// without it (compose only). No user information.
    pub nats_url: String,
    /// The `elitea-worker` identity's mTLS material: all three or none.
    pub nats_ca_path: Option<PathBuf>,
    pub nats_certificate_path: Option<PathBuf>,
    pub nats_private_key_path: Option<PathBuf>,
    /// `ELITEA_RT_V1_VALIDATE`, `ELITEA_RT_V1_AGENT` or `ELITEA_RT_V1_INDEX`.
    pub nats_stream: String,
    /// That stream's bootstrap-created durable; any other pairing is refused.
    pub nats_consumer: String,
    pub control_target: String,
    pub output_target: String,
    pub content_origin: String,
    pub platform_origin: String,
    pub ca_path: PathBuf,
    pub certificate_path: PathBuf,
    pub private_key_path: PathBuf,
    pub ed25519_keyring_path: PathBuf,
    pub spool_root: PathBuf,
    pub spool_key_path: PathBuf,
    pub agent_checkpoint_connection_path: Option<PathBuf>,
    #[serde(default)]
    pub agent_model_checkpoint_recovery: bool,
    #[serde(default)]
    pub agent_node_recovery: bool,
    #[serde(default)]
    pub sandbox_runtimes: Vec<SandboxRuntimeConfig>,
    pub limits: RuntimeLimits,
}

/// Deployment-selected Code backend; never supplied by YAML or model output.
#[derive(Clone, Debug, Eq, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SandboxRuntimeConfig {
    pub language: crate::sandbox::request::Language,
    pub target: String,
    pub audience: String,
    pub image_digest: String,
    pub policy_revision: String,
    pub timeout_seconds: u32,
    #[serde(default)]
    #[serde(deserialize_with = "non_null_code_platform_config")]
    pub platform_client: Option<CodePlatformRuntimeConfig>,
    #[serde(default)]
    #[serde(deserialize_with = "crate::sandbox::compiled_profile_config::non_null_config")]
    pub compiled_snapshot:
        Option<crate::sandbox::compiled_profile_config::RustCompiledSnapshotConfig>,
    #[serde(default)]
    pub preparation: Option<PythonPreparationConfig>,
}

/// Optional operator-selected Code broker route and bounded Main policy.
#[derive(Clone, Debug, Eq, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CodePlatformRuntimeConfig {
    pub target: String,
    pub audience: String,
    pub image_digest: String,
    pub policy_revision: String,
    pub timeout_seconds: u32,
    pub max_calls: u16,
    pub max_total_bytes: u32,
    #[serde(default)]
    #[serde(deserialize_with = "crate::sandbox::compiled_profile_config::non_null_config")]
    pub compiled_snapshot:
        Option<crate::sandbox::compiled_profile_config::RustCompiledSnapshotConfig>,
}

impl CodePlatformRuntimeConfig {
    pub(crate) fn policy(
        &self,
    ) -> Result<crate::sandbox::platform_client_binding::PlatformClientPolicy, RuntimeConfigError>
    {
        crate::sandbox::platform_client_binding::PlatformClientPolicy::new(
            self.max_calls,
            self.max_total_bytes,
        )
        .map_err(|_| invalid_config())
    }

    pub(crate) fn execution_config(&self, base: &SandboxRuntimeConfig) -> SandboxRuntimeConfig {
        let mut config = base.clone();
        config.target.clone_from(&self.target);
        config.audience.clone_from(&self.audience);
        config.image_digest.clone_from(&self.image_digest);
        config.policy_revision.clone_from(&self.policy_revision);
        config.timeout_seconds = self.timeout_seconds;
        config.platform_client = None;
        config.compiled_snapshot.clone_from(&self.compiled_snapshot);
        config
    }

    fn validate(&self, base: &SandboxRuntimeConfig) -> Result<(), RuntimeConfigError> {
        validate_grpc_target(&self.target)?;
        if self.audience.is_empty()
            || self.audience.len() > 256
            || self
                .audience
                .chars()
                .any(|c| c.is_control() || c.is_whitespace())
            || (base.language == crate::sandbox::request::Language::Rust
                && self.policy_revision != "cargo-broker-execute-v1")
        {
            return Err(invalid_config());
        }
        crate::sandbox::request::PreparedJob::new(
            base.language,
            "validate".into(),
            std::collections::BTreeMap::new(),
            self.image_digest.clone(),
            self.policy_revision.clone(),
            self.timeout_seconds,
        )
        .map_err(|_| invalid_config())?;
        self.policy()?;
        if let Some(compiled) = &self.compiled_snapshot {
            if base.language != crate::sandbox::request::Language::Rust {
                return Err(invalid_config());
            }
            compiled.validate().map_err(|_| invalid_config())?;
        }
        Ok(())
    }
}

fn non_null_code_platform_config<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<CodePlatformRuntimeConfig>, D::Error> {
    CodePlatformRuntimeConfig::deserialize(deserializer).map(Some)
}

/// Optional trusted Python preparation backend. Saved source cannot select it.
#[derive(Clone, Debug, Eq, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PythonPreparationConfig {
    #[serde(default)]
    pub native_platform: Option<crate::sandbox::native_bundle::NativePlatform>,
    pub target: String,
    pub audience: String,
    pub image_digest: String,
    pub policy_revision: String,
    pub timeout_seconds: u32,
}

impl PythonPreparationConfig {
    fn validate(&self) -> Result<(), RuntimeConfigError> {
        validate_grpc_target(&self.target)?;
        if self.audience.is_empty()
            || self.audience.len() > 256
            || self
                .audience
                .chars()
                .any(|c| c.is_control() || c.is_whitespace())
        {
            return Err(invalid_config());
        }
        crate::sandbox::preparation::PreparationJob::new(
            "validate".into(),
            self.image_digest.clone(),
            self.policy_revision.clone(),
            self.timeout_seconds,
        )
        .map_err(|_| invalid_config())?;
        Ok(())
    }
}

impl SandboxRuntimeConfig {
    fn validate_preparation(&self) -> Result<(), RuntimeConfigError> {
        let Some(preparation) = &self.preparation else {
            return Ok(());
        };
        if (self.language == crate::sandbox::request::Language::Python)
            != preparation.native_platform.is_none()
        {
            return Err(invalid_config());
        }
        preparation.validate()?;
        if preparation.native_platform.is_some()
            && preparation.timeout_seconds
                > if self.language == crate::sandbox::request::Language::Rust {
                    600
                } else {
                    120
                }
        {
            return Err(invalid_config());
        }
        if let Some(platform) = &preparation.native_platform {
            platform.validate().map_err(|_| invalid_config())?;
        }
        Ok(())
    }
}

impl RuntimeDeployConfig {
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    fn validate(mut self) -> Result<Self, RuntimeConfigError> {
        if self.schema_version != RUNTIME_DEPLOY_SCHEMA_VERSION
            || self.limits_revision != LIMITS_REVISION
        {
            return Err(invalid_config());
        }
        for identity in [
            &self.workload_session_id,
            &self.producer_id,
            &self.consumer_id,
        ] {
            require_bounded_text(identity, MAX_IDENTITY_BYTES)?;
        }
        if !crate::transport::command_bus::valid_route_pair(&self.nats_stream, &self.nats_consumer)
        {
            return Err(invalid_config());
        }
        let tls_material = [
            &self.nats_ca_path,
            &self.nats_certificate_path,
            &self.nats_private_key_path,
        ];
        let present = tls_material.iter().filter(|path| path.is_some()).count();
        let (_, tls_scheme) = crate::transport::nats_jetstream::parse_server_urls(&self.nats_url)
            .map_err(|_| invalid_config())?;
        if !matches!(present, 0 | 3) || tls_scheme != (present == 3) {
            return Err(invalid_config());
        }
        for path in tls_material.into_iter().flatten() {
            require_absolute_path(path)?;
        }
        validate_grpc_target(&self.control_target)?;
        validate_grpc_target(&self.output_target)?;
        if self.sandbox_runtimes.len() > 4 {
            return Err(invalid_config());
        }
        for (index, profile) in self.sandbox_runtimes.iter().enumerate() {
            validate_grpc_target(&profile.target)?;
            if profile.audience.is_empty()
                || profile.audience.len() > 256
                || profile
                    .audience
                    .chars()
                    .any(|c| c.is_control() || c.is_whitespace())
                || self.sandbox_runtimes[..index]
                    .iter()
                    .any(|other| other.language == profile.language)
            {
                return Err(invalid_config());
            }
            crate::sandbox::request::PreparedJob::new(
                profile.language,
                "validate".into(),
                std::collections::BTreeMap::new(),
                profile.image_digest.clone(),
                profile.policy_revision.clone(),
                profile.timeout_seconds,
            )
            .map_err(|_| invalid_config())?;
            profile.validate_preparation()?;
            if let Some(platform) = &profile.platform_client {
                if !self.agent_node_recovery || self.agent_checkpoint_connection_path.is_none() {
                    return Err(invalid_config());
                }
                platform.validate(profile)?;
            }
            if let Some(compiled) = &profile.compiled_snapshot {
                if profile.language != crate::sandbox::request::Language::Rust {
                    return Err(RuntimeConfigError::InvalidConfiguration(
                        "compiled snapshots require the Rust execution runtime",
                    ));
                }
                compiled.validate().map_err(|_| {
                    RuntimeConfigError::InvalidConfiguration(
                        "compiled snapshot release settings are incomplete or invalid",
                    )
                })?;
            }
        }

        // Stop delivery stores audience only. It must select one transport target.
        let mut sandbox_targets = std::collections::BTreeMap::new();
        for profile in &self.sandbox_runtimes {
            let endpoints = std::iter::once((&profile.audience, &profile.target))
                .chain(
                    profile
                        .platform_client
                        .iter()
                        .map(|platform| (&platform.audience, &platform.target)),
                )
                .chain(
                    profile
                        .preparation
                        .iter()
                        .map(|preparation| (&preparation.audience, &preparation.target)),
                );
            for (audience, target) in endpoints {
                if sandbox_targets
                    .insert(audience, target)
                    .is_some_and(|prior| prior != target)
                {
                    return Err(invalid_config());
                }
            }
        }

        self.content_origin = canonical_https_origin(&self.content_origin)?;
        self.platform_origin = canonical_https_origin(&self.platform_origin)?;
        for path in [
            &self.ca_path,
            &self.certificate_path,
            &self.private_key_path,
            &self.ed25519_keyring_path,
            &self.spool_root,
            &self.spool_key_path,
        ] {
            require_absolute_path(path)?;
        }
        if let Some(path) = &self.agent_checkpoint_connection_path {
            require_absolute_path(path)?;
        }
        if (self.agent_model_checkpoint_recovery || self.agent_node_recovery)
            && self.agent_checkpoint_connection_path.is_none()
        {
            return Err(invalid_config());
        }
        self.limits.validate()?;
        Ok(self)
    }
}

/// All bounded queue, transport, lifecycle and shutdown settings.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RuntimeLimits {
    /// Messages per pull, 1..64 (never more than the free delivery permits).
    pub nats_fetch_batch: usize,
    /// How long one pull waits, 100..30000.
    pub nats_fetch_expires_millis: u64,
    /// The `+WPI` period, 1000..15000 (at most a quarter of `AckWait`).
    pub nats_in_progress_interval_millis: u64,
    /// The retry-later nak delay, 1000..300000.
    pub nats_retry_delay_millis: u64,
    pub dependency_retry_millis: u64,
    pub delivery_max_concurrency: usize,
    pub delivery_queue_capacity: usize,
    pub sync_max_workers: usize,
    pub sync_max_in_flight: usize,
    pub admission_timeout_millis: u64,
    pub grpc_deadline_millis: u64,
    pub content_timeout_millis: u64,
    #[serde(default = "default_model_timeout_millis")]
    pub model_response_header_timeout_millis: u64,
    #[serde(default = "default_model_timeout_millis")]
    pub model_stream_idle_timeout_millis: u64,
    pub http_max_connections: usize,
    pub http_max_keepalive_connections: usize,
    pub output_max_queued_frames: usize,
    pub output_max_queued_bytes: usize,
    pub output_max_sessions: usize,
    pub output_ack_timeout_millis: u64,
    pub output_stream_deadline_millis: u64,
    pub lease_poll_interval_millis: u64,
    pub shutdown_timeout_millis: u64,
}

fn default_model_timeout_millis() -> u64 {
    120_000
}

impl RuntimeLimits {
    fn validate(self) -> Result<(), RuntimeConfigError> {
        let valid = (1..=64).contains(&self.nats_fetch_batch)
            && (100..=30_000).contains(&self.nats_fetch_expires_millis)
            && (1_000..=15_000).contains(&self.nats_in_progress_interval_millis)
            && (1_000..=300_000).contains(&self.nats_retry_delay_millis)
            && (100..=60_000).contains(&self.dependency_retry_millis)
            && (1..=128).contains(&self.delivery_max_concurrency)
            && (1..=512).contains(&self.delivery_queue_capacity)
            && self.delivery_queue_capacity >= self.delivery_max_concurrency
            && (1..=128).contains(&self.sync_max_workers)
            && (1..=512).contains(&self.sync_max_in_flight)
            && self.sync_max_in_flight >= self.sync_max_workers
            && (1..=60_000).contains(&self.admission_timeout_millis)
            && (1..=300_000).contains(&self.grpc_deadline_millis)
            && (1..=300_000).contains(&self.content_timeout_millis)
            && (1..=300_000).contains(&self.model_response_header_timeout_millis)
            && (1..=300_000).contains(&self.model_stream_idle_timeout_millis)
            && (1..=512).contains(&self.http_max_connections)
            && self.http_max_keepalive_connections <= 512
            && self.http_max_keepalive_connections <= self.http_max_connections
            && (1..=128).contains(&self.output_max_queued_frames)
            && (RUNTIME_OUTPUT_FRAME_BYTES..=64 * 1024 * 1024)
                .contains(&self.output_max_queued_bytes)
            && (1..=8).contains(&self.output_max_sessions)
            && (1..=300_000).contains(&self.output_ack_timeout_millis)
            && (1..=3_600_000).contains(&self.output_stream_deadline_millis)
            && (1..=MAX_LEASE_POLL_INTERVAL_MILLIS).contains(&self.lease_poll_interval_millis)
            && (1..=300_000).contains(&self.shutdown_timeout_millis);
        if !valid {
            return Err(invalid_limits());
        }
        Ok(())
    }

    #[must_use]
    pub const fn max_transport_message_bytes(self) -> usize {
        RUNTIME_TRANSPORT_MESSAGE_BYTES
    }

    #[must_use]
    pub const fn max_transport_payload_bytes(self) -> usize {
        RUNTIME_TRANSPORT_PAYLOAD_BYTES
    }

    #[must_use]
    pub const fn content_max_body_bytes(self) -> usize {
        RUNTIME_INPUT_CONTENT_BYTES
    }

    #[must_use]
    pub const fn grpc_max_request_bytes(self) -> usize {
        RUNTIME_GRPC_REQUEST_BYTES
    }

    #[must_use]
    pub const fn grpc_max_response_bytes(self) -> usize {
        RUNTIME_GRPC_RESPONSE_BYTES
    }

    #[must_use]
    pub const fn output_max_frame_bytes(self) -> usize {
        RUNTIME_OUTPUT_FRAME_BYTES
    }
}

/// Load one bounded, strict deployment document without following symlinks.
///
/// # Errors
///
/// Returns a stable configuration, resource-limit or filesystem error when
/// the document is unsafe, malformed or outside the runtime-v1 profile.
pub fn load_deploy_config(path: &Path) -> Result<RuntimeDeployConfig, RuntimeConfigError> {
    let raw = read_regular_file(
        path,
        MAX_CONFIG_BYTES,
        false,
        "runtime deployment configuration",
    )?;
    serde_json::from_slice::<RuntimeDeployConfig>(&raw)
        .map_err(|_| invalid_config())?
        .validate()
}

/// Read one regular file under an explicit permission and size policy.
///
/// # Errors
///
/// Returns a stable configuration, resource-limit or filesystem error for an
/// unsafe path, file type, permission profile, size or interrupted read.
pub fn read_regular_file(
    path: &Path,
    max_bytes: usize,
    private: bool,
    description: &'static str,
) -> Result<Vec<u8>, RuntimeConfigError> {
    if max_bytes == 0 || description.is_empty() || !path.is_absolute() {
        return Err(invalid_file(description));
    }
    let canonical = path
        .canonicalize()
        .map_err(|error| unavailable_file(description, error))?;
    if canonical != path {
        return Err(invalid_file(description));
    }
    let descriptor = openat(
        rustix::fs::CWD,
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|error| unavailable_file(description, std::io::Error::from(error)))?;
    let stat = fstat(&descriptor)
        .map_err(|error| unavailable_file(description, std::io::Error::from(error)))?;
    let size = usize::try_from(stat.st_size).map_err(|_| exhausted_file(description))?;
    let unsafe_permissions = if private {
        stat.st_mode & 0o077 != 0
    } else {
        stat.st_mode & 0o022 != 0
    };
    if FileType::from_raw_mode(stat.st_mode) != FileType::RegularFile
        || unsafe_permissions
        || size == 0
    {
        return Err(invalid_file(description));
    }
    if size > max_bytes {
        return Err(exhausted_file(description));
    }
    let mut file = File::from(descriptor);
    let mut bytes = Vec::with_capacity(size);
    file.read_to_end(&mut bytes)
        .map_err(|error| unavailable_file(description, error))?;
    if bytes.len() != size {
        return Err(invalid_file(description));
    }
    Ok(bytes)
}

/// Validate an existing canonical owner-private directory.
///
/// # Errors
///
/// Returns a stable configuration or filesystem error when the path is not an
/// exact canonical directory owned by this process with mode `0700` or tighter.
pub fn validate_private_directory(
    path: &Path,
    description: &'static str,
) -> Result<PathBuf, RuntimeConfigError> {
    if description.is_empty() || !path.is_absolute() {
        return Err(invalid_file(description));
    }
    let canonical = path
        .canonicalize()
        .map_err(|error| unavailable_file(description, error))?;
    if canonical != path {
        return Err(invalid_file(description));
    }
    let descriptor = openat(
        rustix::fs::CWD,
        path,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .map_err(|error| unavailable_file(description, std::io::Error::from(error)))?;
    let stat = fstat(&descriptor)
        .map_err(|error| unavailable_file(description, std::io::Error::from(error)))?;
    if FileType::from_raw_mode(stat.st_mode) != FileType::Directory
        || stat.st_uid != geteuid().as_raw()
        || stat.st_mode & 0o077 != 0
    {
        return Err(invalid_file(description));
    }
    Ok(canonical)
}

fn validate_grpc_target(value: &str) -> Result<(), RuntimeConfigError> {
    require_bounded_text(value, MAX_TARGET_BYTES)?;
    if value.contains("://") || value.contains('/') || value.contains('@') || value.starts_with(':')
    {
        return Err(invalid_config());
    }
    Ok(())
}

fn canonical_https_origin(value: &str) -> Result<String, RuntimeConfigError> {
    require_bounded_text(value, MAX_ORIGIN_BYTES)?;
    let uri = value.parse::<Uri>().map_err(|_| invalid_config())?;
    let authority = uri.authority().ok_or_else(invalid_config)?;
    if uri.scheme_str() != Some("https")
        || authority.as_str().contains('@')
        || uri.query().is_some()
        || !matches!(uri.path(), "" | "/")
    {
        return Err(invalid_config());
    }
    Ok(format!("https://{authority}"))
}

fn require_bounded_text(value: &str, maximum: usize) -> Result<(), RuntimeConfigError> {
    if value.is_empty()
        || value.len() > maximum
        || value
            .bytes()
            .any(|byte| matches!(byte, b'\r' | b'\n' | b'\0'))
    {
        return Err(invalid_config());
    }
    Ok(())
}

fn require_absolute_path(path: &Path) -> Result<(), RuntimeConfigError> {
    if !path.is_absolute() {
        return Err(invalid_config());
    }
    Ok(())
}

const fn invalid_config() -> RuntimeConfigError {
    RuntimeConfigError::InvalidConfiguration("the runtime deployment configuration is invalid")
}

const fn invalid_limits() -> RuntimeConfigError {
    RuntimeConfigError::InvalidConfiguration("the runtime deployment limits are invalid")
}

const fn invalid_file(description: &'static str) -> RuntimeConfigError {
    RuntimeConfigError::InvalidConfiguration(description)
}

const fn exhausted_file(description: &'static str) -> RuntimeConfigError {
    RuntimeConfigError::ResourceExhausted(description)
}

fn unavailable_file(description: &'static str, source: std::io::Error) -> RuntimeConfigError {
    RuntimeConfigError::Unavailable {
        message: description,
        source,
    }
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::PermissionsExt as _;
    use std::path::Path;

    use serde_json::{Value, json};
    use tempfile::tempdir;

    use super::{
        RUNTIME_DEPLOY_SCHEMA_VERSION, RuntimeConfigError, load_deploy_config, read_regular_file,
        validate_private_directory,
    };
    use crate::protocol::command::LIMITS_REVISION;

    #[test]
    fn model_timeouts_default_independently_and_reject_unbounded_values() {
        let mut value = config(Path::new("/runtime"))["limits"].clone();
        let limits: super::RuntimeLimits = serde_json::from_value(value.clone()).unwrap();
        assert_eq!(limits.content_timeout_millis, 15_000);
        assert_eq!(limits.model_response_header_timeout_millis, 120_000);
        assert_eq!(limits.model_stream_idle_timeout_millis, 120_000);
        for field in [
            "model_response_header_timeout_millis",
            "model_stream_idle_timeout_millis",
        ] {
            for invalid in [0, 300_001] {
                value[field] = json!(invalid);
                let limits: super::RuntimeLimits = serde_json::from_value(value.clone()).unwrap();
                assert!(limits.validate().is_err());
            }
            value[field] = json!(240_000);
            let limits: super::RuntimeLimits = serde_json::from_value(value.clone()).unwrap();
            assert!(limits.validate().is_ok());
        }
    }

    fn config(root: &Path) -> Value {
        let limits = json!({
            "nats_fetch_batch": 8,
            "nats_fetch_expires_millis": 1000,
            "nats_in_progress_interval_millis": 5000,
            "nats_retry_delay_millis": 60000,
            "dependency_retry_millis": 250,
            "delivery_max_concurrency": 4,
            "delivery_queue_capacity": 8,
            "sync_max_workers": 2,
            "sync_max_in_flight": 4,
            "admission_timeout_millis": 1000,
            "grpc_deadline_millis": 5000,
            "content_timeout_millis": 15000,
            "http_max_connections": 8,
            "http_max_keepalive_connections": 4,
            "output_max_queued_frames": 2,
            "output_max_queued_bytes": 131_072,
            "output_max_sessions": 2,
            "output_ack_timeout_millis": 15000,
            "output_stream_deadline_millis": 300_000,
            "lease_poll_interval_millis": 10000,
            "shutdown_timeout_millis": 30000
        });
        json!({
            "schema_version": RUNTIME_DEPLOY_SCHEMA_VERSION,
            "limits_revision": LIMITS_REVISION,
            "workload_session_id": "session-1",
            "producer_id": "rust-worker-1",
            "consumer_id": "rust-worker-1-consumer",
            "nats_url": "tls://nats.internal:4222",
            "nats_ca_path": root.join("nats-ca.crt"),
            "nats_certificate_path": root.join("nats-worker.crt"),
            "nats_private_key_path": root.join("nats-worker.key"),
            "nats_stream": "ELITEA_RT_V1_AGENT",
            "nats_consumer": "elitea-agent-worker-v1",
            "control_target": "control.internal:9443",
            "output_target": "output.internal:9444",
            "content_origin": "https://content.internal:9445/",
            "platform_origin": "https://platform.internal",
            "ca_path": root.join("ca.pem"),
            "certificate_path": root.join("worker.pem"),
            "private_key_path": root.join("worker-key.pem"),
            "ed25519_keyring_path": root.join("command-keys.json"),
            "spool_root": root.join("spool"),
            "spool_key_path": root.join("spool.key"),
            "agent_checkpoint_connection_path": root.join("agentstate-connection"),
            "limits": limits
        })
    }

    fn write_config(path: &Path, value: &Value) {
        fs::write(path, serde_json::to_vec(value).expect("configuration JSON"))
            .expect("write configuration");
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))
            .expect("configuration permissions");
    }

    #[test]
    fn checkpoint_recovery_requires_explicit_opt_in_and_durable_storage() {
        let root = tempdir().expect("root");
        let mut value = config(root.path());
        let loaded: super::RuntimeDeployConfig =
            serde_json::from_value(value.clone()).expect("config");
        assert!(!loaded.agent_model_checkpoint_recovery);
        value["agent_model_checkpoint_recovery"] = json!(true);
        let loaded: super::RuntimeDeployConfig =
            serde_json::from_value(value.clone()).expect("opt-in");
        assert!(loaded.validate().is_ok());
        value["agent_checkpoint_connection_path"] = Value::Null;
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(value).expect("no storage");
        assert!(loaded.validate().is_err());
    }

    #[test]
    fn node_recovery_requires_explicit_opt_in_and_durable_storage() {
        let root = tempdir().expect("root");
        let mut value = config(root.path());
        let loaded: super::RuntimeDeployConfig =
            serde_json::from_value(value.clone()).expect("config");
        assert!(!loaded.agent_node_recovery);
        value["agent_node_recovery"] = json!(true);
        let loaded: super::RuntimeDeployConfig =
            serde_json::from_value(value.clone()).expect("opt-in");
        assert!(loaded.validate().is_ok());
        value["agent_checkpoint_connection_path"] = Value::Null;
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(value).expect("no storage");
        assert!(loaded.validate().is_err());
    }

    #[test]
    fn sandbox_profiles_are_optional_and_require_bounded_unique_runtime_identity() {
        let base = config(Path::new("/runtime"));
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(base.clone()).unwrap();
        assert!(loaded.sandbox_runtimes.is_empty());
        assert!(loaded.validate().is_ok());
        let profile = json!({
            "language": "python",
            "target": "sandbox.internal:9446",
            "audience": "sandbox-code",
            "image_digest": format!("sha256:{}", "a".repeat(64)),
            "policy_revision": "code-v1",
            "timeout_seconds": 60
        });
        let mut valid = base.clone();
        valid["sandbox_runtimes"] = json!([profile.clone()]);
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(valid.clone()).unwrap();
        assert!(loaded.sandbox_runtimes[0].preparation.is_none());
        assert!(loaded.validate().is_ok());
        for (field, value) in [
            ("target", json!("http://sandbox.internal:9446")),
            ("audience", json!("")),
            ("audience", json!("sandbox other")),
            ("image_digest", json!("runner:latest")),
            ("policy_revision", json!("")),
            ("timeout_seconds", json!(0)),
            ("timeout_seconds", json!(3601)),
        ] {
            let mut invalid = valid.clone();
            invalid["sandbox_runtimes"][0][field] = value;
            let loaded: super::RuntimeDeployConfig = serde_json::from_value(invalid).unwrap();
            assert!(loaded.validate().is_err(), "accepted invalid {field}");
        }
        valid["sandbox_runtimes"] = json!([profile.clone(), profile]);
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(valid).unwrap();
        assert!(
            loaded.validate().is_err(),
            "duplicate language must not select an arbitrary backend"
        );
    }

    #[test]
    fn compiled_snapshot_settings_are_explicit_and_rust_only() {
        let mut value = config(Path::new("/runtime"));
        value["sandbox_runtimes"] = json!([{
            "language":"rust","target":"sandbox.internal:9447","audience":"sandbox-rust",
            "image_digest":format!("sha256:{}", "a".repeat(64)),"policy_revision":"rust-v1","timeout_seconds":30
        }]);
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(value.clone()).unwrap();
        assert!(loaded.sandbox_runtimes[0].compiled_snapshot.is_none());
        assert!(loaded.validate().is_ok());
        value["sandbox_runtimes"][0]["compiled_snapshot"] = json!({
            "profiles_file":"/runtime/compiled-profiles.json","profiles_sha256":"a".repeat(64),"dependency_bundle_sha256":""
        });
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(value.clone()).unwrap();
        assert!(loaded.validate().is_ok());
        for (field, bad) in [
            ("language", json!("python")),
            (
                "compiled_snapshot",
                json!({"profiles_file":"/runtime/compiled-profiles.json"}),
            ),
            ("compiled_snapshot", serde_json::Value::Null),
        ] {
            let mut invalid = value.clone();
            invalid["sandbox_runtimes"][0][field] = bad;
            assert!(
                serde_json::from_value::<super::RuntimeDeployConfig>(invalid)
                    .map_err(|_| ())
                    .and_then(|config| config.validate().map_err(|_| ()))
                    .is_err()
            );
        }
        value["sandbox_runtimes"][0]["compiled_snapshot"]["profiles_sha256"] =
            json!("A".repeat(64));
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(value).unwrap();
        assert!(loaded.validate().is_err());
    }

    #[test]
    fn python_preparation_is_optional_bounded_and_python_only() {
        let mut enabled = config(Path::new("/runtime"));
        enabled["sandbox_runtimes"] = json!([{
            "language": "python",
            "target": "sandbox.internal:9446",
            "audience": "sandbox-code",
            "image_digest": format!("sha256:{}", "a".repeat(64)),
            "policy_revision": "code-v1",
            "timeout_seconds": 60,
            "preparation": {
                "target": "preparation.internal:9446",
                "audience": "sandbox-preparation",
                "image_digest": format!("sha256:{}", "b".repeat(64)),
                "policy_revision": "python-preparation-v1",
                "timeout_seconds": 120
            }
        }]);
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(enabled.clone()).unwrap();
        assert!(loaded.sandbox_runtimes[0].preparation.is_some());
        assert!(loaded.validate().is_ok());
        for (field, value) in [
            ("target", json!("http://preparation.internal:9446")),
            ("audience", json!("")),
            ("audience", json!("sandbox other")),
            ("image_digest", json!("runner:latest")),
            ("policy_revision", json!("")),
            ("timeout_seconds", json!(0)),
            ("timeout_seconds", json!(3601)),
        ] {
            let mut invalid = enabled.clone();
            invalid["sandbox_runtimes"][0]["preparation"][field] = value;
            let loaded: super::RuntimeDeployConfig = serde_json::from_value(invalid).unwrap();
            assert!(
                loaded.validate().is_err(),
                "accepted invalid preparation {field}"
            );
        }
        for language in ["javascript", "typescript", "rust"] {
            let mut invalid = enabled.clone();
            invalid["sandbox_runtimes"][0]["language"] = json!(language);
            let loaded: super::RuntimeDeployConfig = serde_json::from_value(invalid).unwrap();
            assert!(loaded.validate().is_err());
        }
        let mut ambiguous = enabled.clone();
        ambiguous["sandbox_runtimes"][0]["preparation"]["audience"] = json!("sandbox-code");
        let loaded: super::RuntimeDeployConfig = serde_json::from_value(ambiguous).unwrap();
        assert!(
            loaded.validate().is_err(),
            "one audience cannot select different Stop targets"
        );
        enabled["sandbox_runtimes"][0]["preparation"]["package_list"] = json!(["anything"]);
        assert!(serde_json::from_value::<super::RuntimeDeployConfig>(enabled).is_err());
    }

    #[test]
    fn strict_file_config_normalizes_origins_and_retains_fixed_limits() {
        let root = tempdir().expect("temporary directory");
        let root_path = root
            .path()
            .canonicalize()
            .expect("canonical temporary root");
        let path = root_path.join("runtime.json");
        write_config(&path, &config(&root_path));

        let loaded = load_deploy_config(&path).expect("valid runtime configuration");

        assert_eq!(loaded.content_origin, "https://content.internal:9445");
        assert_eq!(loaded.limits.max_transport_message_bytes(), 64 * 1024);
        assert_eq!(loaded.limits.max_transport_payload_bytes(), 48 * 1024);
        assert_eq!(loaded.nats_stream, "ELITEA_RT_V1_AGENT");
        assert_eq!(loaded.limits.nats_in_progress_interval_millis, 5_000);
        assert_eq!(loaded.limits.content_max_body_bytes(), 8 * 1024 * 1024);
        assert_eq!(loaded.limits.grpc_max_request_bytes(), 64 * 1024);
        assert_eq!(loaded.limits.grpc_max_response_bytes(), 80 * 1024);
        assert_eq!(loaded.limits.output_max_frame_bytes(), 64 * 1024);
    }

    #[test]
    fn unknown_fields_credentials_and_malformed_transport_profiles_fail_closed() {
        let root = tempdir().expect("temporary directory");
        let root_path = root
            .path()
            .canonicalize()
            .expect("canonical temporary root");
        let path = root_path.join("runtime.json");
        let cases = [
            ("nats_password", json!("must-not-be-inline")),
            ("redis_url", json!("rediss://worker@redis.internal:6379/0")),
            ("redis_stream", json!("commands.v1.agent.shared.1.0")),
            ("nats_url", json!("nats://nats.internal:4222")),
            ("nats_url", json!("tls://worker:secret@nats.internal:4222")),
            ("nats_url", json!("tls://token@nats.internal:4222")),
            ("nats_url", json!("rediss://worker@redis.internal:6379/0")),
            ("nats_ca_path", Value::Null),
            ("nats_private_key_path", json!("relative.key")),
            ("nats_stream", json!("ELITEA_RT_V1_INDEX")),
            ("nats_consumer", json!("elitea-rust-workers")),
            ("control_target", json!("https://control.internal:9443")),
            ("content_origin", json!("https://content.internal/path")),
        ];
        for (field, value) in cases {
            let mut document = config(&root_path);
            document[field] = value;
            write_config(&path, &document);
            assert!(
                matches!(
                    load_deploy_config(&path),
                    Err(RuntimeConfigError::InvalidConfiguration(_))
                ),
                "accepted {field}"
            );
        }
    }

    #[test]
    fn plaintext_nats_is_admitted_only_without_any_tls_material() {
        let root = tempdir().expect("temporary directory");
        let root_path = root
            .path()
            .canonicalize()
            .expect("canonical temporary root");
        let path = root_path.join("runtime.json");
        let mut document = config(&root_path);
        document["nats_url"] = json!("nats://nats:4222");
        let object = document.as_object_mut().expect("config object");
        for field in [
            "nats_ca_path",
            "nats_certificate_path",
            "nats_private_key_path",
        ] {
            object.remove(field);
        }
        write_config(&path, &document);
        let loaded = load_deploy_config(&path).expect("compose profile");
        assert!(loaded.nats_ca_path.is_none());
        document["nats_url"] = json!("tls://nats:4222");
        write_config(&path, &document);
        assert!(load_deploy_config(&path).is_err(), "tls:// needs material");
        document["nats_url"] = json!("nats://nats:4222");
        document["nats_ca_path"] = json!(root_path.join("ca.crt"));
        write_config(&path, &document);
        assert!(load_deploy_config(&path).is_err(), "partial material");
    }

    #[test]
    fn related_lifecycle_and_queue_limits_are_validated_together() {
        let root = tempdir().expect("temporary directory");
        let root_path = root
            .path()
            .canonicalize()
            .expect("canonical temporary root");
        let path = root_path.join("runtime.json");
        for (field, value) in [
            ("delivery_queue_capacity", 3),
            ("sync_max_in_flight", 1),
            ("nats_fetch_batch", 0),
            ("nats_fetch_batch", 65),
            ("nats_fetch_expires_millis", 99),
            ("nats_fetch_expires_millis", 30_001),
            ("nats_in_progress_interval_millis", 999),
            ("nats_in_progress_interval_millis", 15_001),
            ("nats_retry_delay_millis", 999),
            ("nats_retry_delay_millis", 300_001),
            ("lease_poll_interval_millis", 10_001),
            ("output_max_queued_bytes", 65_535),
        ] {
            let mut document = config(&root_path);
            document["limits"][field] = json!(value);
            write_config(&path, &document);
            assert!(matches!(
                load_deploy_config(&path),
                Err(RuntimeConfigError::InvalidConfiguration(_))
            ));
        }
    }

    #[test]
    fn file_and_directory_boundaries_reject_symlinks_and_open_permissions() {
        let root = tempdir().expect("temporary directory");
        let root_path = root
            .path()
            .canonicalize()
            .expect("canonical temporary root");
        let private = root_path.join("private");
        fs::write(&private, b"secret").expect("write private fixture");
        fs::set_permissions(&private, fs::Permissions::from_mode(0o640))
            .expect("private permissions");
        assert!(read_regular_file(&private, 64, true, "private fixture").is_err());

        fs::set_permissions(&private, fs::Permissions::from_mode(0o600))
            .expect("private permissions");
        let linked = root_path.join("linked");
        std::os::unix::fs::symlink(&private, &linked).expect("symlink fixture");
        assert!(read_regular_file(&linked, 64, true, "private fixture").is_err());

        let directory = root_path.join("spool");
        fs::create_dir(&directory).expect("spool directory");
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))
            .expect("spool permissions");
        assert_eq!(
            validate_private_directory(&directory, "spool fixture")
                .expect("private spool directory"),
            directory
        );
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o750))
            .expect("unsafe spool permissions");
        assert!(validate_private_directory(&directory, "spool fixture").is_err());
    }
}

#[cfg(test)]
#[path = "config_code_platform_tests.rs"]
mod code_platform_tests;
