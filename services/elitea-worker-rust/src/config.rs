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
const MAX_REDIS_NAME_BYTES: usize = 512;
const MAX_TARGET_BYTES: usize = 512;
const MAX_ORIGIN_BYTES: usize = 2_048;
const RUNTIME_REDIS_ENTRY_BYTES: usize = 64 * 1024;
const RUNTIME_REDIS_FIELD_BYTES: usize = 48 * 1024;
// Match Main's admitted agent bundle and the claim-bound content contract.
// A smaller fetch limit rejects saved history before compaction can run.
const RUNTIME_INPUT_CONTENT_BYTES: usize = 1024 * 1024;
const RUNTIME_OUTPUT_FRAME_BYTES: usize = 64 * 1024;
const RUNTIME_GRPC_REQUEST_BYTES: usize = 64 * 1024;
const RUNTIME_GRPC_RESPONSE_BYTES: usize = 80 * 1024;
const MAX_LEASE_POLL_INTERVAL_MILLIS: u64 = 10_000;
const MIN_REDIS_RECLAIM_IDLE_MILLIS: u64 = 60_000;

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
    pub redis_url: String,
    pub redis_password_path: PathBuf,
    pub redis_stream: String,
    pub redis_group: String,
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
    pub limits: RuntimeLimits,
}

impl RuntimeDeployConfig {
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
        require_bounded_text(&self.redis_stream, MAX_REDIS_NAME_BYTES)?;
        require_bounded_text(&self.redis_group, MAX_IDENTITY_BYTES)?;
        validate_redis_url(&self.redis_url)?;
        validate_grpc_target(&self.control_target)?;
        validate_grpc_target(&self.output_target)?;
        self.content_origin = canonical_https_origin(&self.content_origin)?;
        self.platform_origin = canonical_https_origin(&self.platform_origin)?;
        for path in [
            &self.redis_password_path,
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
        if self.agent_model_checkpoint_recovery && self.agent_checkpoint_connection_path.is_none() {
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
    pub redis_read_batch: usize,
    pub redis_block_millis: u64,
    pub redis_reclaim_idle_millis: u64,
    pub redis_reclaim_interval_millis: u64,
    pub dependency_retry_millis: u64,
    pub delivery_max_concurrency: usize,
    pub delivery_queue_capacity: usize,
    pub sync_max_workers: usize,
    pub sync_max_in_flight: usize,
    pub admission_timeout_millis: u64,
    pub grpc_deadline_millis: u64,
    pub content_timeout_millis: u64,
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

impl RuntimeLimits {
    fn validate(self) -> Result<(), RuntimeConfigError> {
        let valid = (1..=64).contains(&self.redis_read_batch)
            && (100..=30_000).contains(&self.redis_block_millis)
            && (MIN_REDIS_RECLAIM_IDLE_MILLIS..=86_400_000)
                .contains(&self.redis_reclaim_idle_millis)
            && (100..=MAX_LEASE_POLL_INTERVAL_MILLIS).contains(&self.redis_reclaim_interval_millis)
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
    pub const fn redis_max_entry_bytes(self) -> usize {
        RUNTIME_REDIS_ENTRY_BYTES
    }

    #[must_use]
    pub const fn redis_max_field_bytes(self) -> usize {
        RUNTIME_REDIS_FIELD_BYTES
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

fn validate_redis_url(value: &str) -> Result<(), RuntimeConfigError> {
    if value.len() > MAX_ORIGIN_BYTES
        || !value
            .bytes()
            .all(|byte| (0x21..=0x7e).contains(&byte) && !matches!(byte, b'%' | b'?' | b'#'))
    {
        return Err(invalid_config());
    }
    let remainder = value.strip_prefix("rediss://").ok_or_else(invalid_config)?;
    let (authority, database) = remainder.split_once('/').ok_or_else(invalid_config)?;
    if database != "0" || authority.contains('/') {
        return Err(invalid_config());
    }
    let (username, host_port) = authority.split_once('@').ok_or_else(invalid_config)?;
    if authority.matches('@').count() != 1
        || username.is_empty()
        || username.len() > MAX_IDENTITY_BYTES
        || !username
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
    {
        return Err(invalid_config());
    }
    let (host, port_text) = if let Some(ipv6) = host_port.strip_prefix('[') {
        let (host, suffix) = ipv6.split_once(']').ok_or_else(invalid_config)?;
        let port = suffix.strip_prefix(':').ok_or_else(invalid_config)?;
        (host, port)
    } else {
        host_port.rsplit_once(':').ok_or_else(invalid_config)?
    };
    let port = port_text.parse::<u16>().map_err(|_| invalid_config())?;
    if host.is_empty()
        || !host.is_ascii()
        || port == 0
        || port_text != port.to_string()
        || (!host_port.starts_with('[') && host.contains(':'))
    {
        return Err(invalid_config());
    }
    Ok(())
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

    fn config(root: &Path) -> Value {
        let limits = json!({
            "redis_read_batch": 8,
            "redis_block_millis": 1000,
            "redis_reclaim_idle_millis": 60000,
            "redis_reclaim_interval_millis": 5000,
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
            "redis_url": "rediss://worker@redis.internal:6379/0",
            "redis_password_path": root.join("redis-password"),
            "redis_stream": "commands.v1.agent.shared.1.0",
            "redis_group": "elitea-rust-workers",
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
        assert_eq!(loaded.limits.redis_max_entry_bytes(), 64 * 1024);
        assert_eq!(loaded.limits.redis_max_field_bytes(), 48 * 1024);
        assert_eq!(loaded.limits.content_max_body_bytes(), 1024 * 1024);
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
            ("redis_password", json!("must-not-be-inline")),
            ("redis_url", json!("redis://worker@redis.internal:6379/0")),
            (
                "redis_url",
                json!("rediss://worker:secret@redis.internal:6379/0"),
            ),
            ("control_target", json!("https://control.internal:9443")),
            ("content_origin", json!("https://content.internal/path")),
        ];
        for (field, value) in cases {
            let mut document = config(&root_path);
            document[field] = value;
            write_config(&path, &document);
            assert!(matches!(
                load_deploy_config(&path),
                Err(RuntimeConfigError::InvalidConfiguration(_))
            ));
        }
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
            ("redis_reclaim_idle_millis", 59_999),
            ("redis_reclaim_interval_millis", 10_001),
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
