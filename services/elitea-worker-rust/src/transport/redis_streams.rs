//! Restricted Redis Streams transport for signed worker commands.
//!
//! This module deliberately exposes only the runtime-v1 operations required by
//! command delivery: new-entry intake, pending-entry reclaim and heartbeat, and
//! exact post-settlement retirement. It has no `XADD`, arbitrary `EVAL`, ACK,
//! delete, key/value, or pub/sub surface.
//!
//! redis-rs decodes a complete RESP frame before these post-decode bounds run. The
//! deployment must therefore keep this client on the dedicated mTLS/ACL command
//! plane and retain the producer-side stream/entry capacity gate. A streaming
//! pre-allocation RESP limit remains a production hardening gate.

use std::{
    collections::BTreeSet,
    fmt,
    future::Future,
    io,
    sync::{
        Arc, Mutex as StdMutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use async_trait::async_trait;
use redis::{
    AsyncConnectionConfig, ErrorKind as RedisErrorKind, ProtocolVersion, RedisConnectionInfo,
    RedisError, Value, aio::MultiplexedConnection, cmd,
};
use rustls::{
    ClientConfig, RootCertStore,
    pki_types::{CertificateDer, PrivateKeyDer, ServerName, pem::PemObject},
};
use tokio::{net::TcpStream, sync::Semaphore, task::JoinHandle};
use tokio_rustls::TlsConnector;
use zeroize::Zeroizing;

use super::redis_commands::{
    RedisCommandDelivery, RedisCommandError, RedisCommandLimits, RedisRetirementClient,
    RedisRetirementClientError, RedisRetirementRequest, RedisRetirementResponse, decode_entry_id,
};

const MAX_URL_BYTES: usize = 2_048;
const MAX_STREAM_BYTES: usize = 512;
const MAX_IDENTITY_BYTES: usize = 256;
const MAX_PASSWORD_BYTES: usize = 512;
const MAX_PEM_BYTES: usize = 1024 * 1024;
const RUNTIME_V1_ENTRY_BYTES: usize = 64 * 1024;
const RUNTIME_V1_FIELD_BYTES: usize = 48 * 1024;
const MAX_READ_COUNT: u64 = 64;
const MIN_BLOCK_MILLIS: u64 = 100;
const MAX_BLOCK_MILLIS: u64 = 30_000;
const MIN_OPERATION_TIMEOUT: Duration = Duration::from_millis(1);
const MAX_OPERATION_TIMEOUT: Duration = Duration::from_mins(5);
// Twice Main's 30-second claim lease. Reclaiming earlier can race a live
// business owner even when Redis PEL idle has elapsed.
const MIN_RECLAIM_IDLE_MILLIS: u64 = 60_000;
const MAX_RECLAIM_IDLE_MILLIS: u64 = 86_400_000;
// Python's largest deployed pool is delivery_max_concurrency (128) plus four
// intake/liveness/control slots. Rust uses two sockets, but preserves the same
// maximum bounded pipeline admission profile.
const MAX_CONTROL_CONCURRENCY: usize = 132;

const HEARTBEAT_OWNED_PENDING_SCRIPT: &str = r"
if #ARGV < 3 or #ARGV > 66 then
    return redis.error_reply('invalid bounded PEL heartbeat')
end

local group = ARGV[1]
local consumer = ARGV[2]
local refreshed = {}
for argument_index = 3, #ARGV do
    local entry_id = ARGV[argument_index]
    local pending = redis.call(
        'XPENDING', KEYS[1], group, entry_id, entry_id, 1
    )
    if #pending == 1
        and pending[1][1] == entry_id
        and pending[1][2] == consumer then
        local claimed = redis.call(
            'XCLAIM', KEYS[1], group, consumer, 0, entry_id, 'JUSTID'
        )
        if #claimed == 1 and claimed[1] == entry_id then
            refreshed[#refreshed + 1] = entry_id
        end
    end
end
return refreshed
";

const RETIRE_DELIVERY_SCRIPT: &str = r"
local mapped_entry_id = redis.call('HGET', KEYS[2], ARGV[1])
local entries = redis.call('XRANGE', KEYS[1], ARGV[2], ARGV[2], 'COUNT', 1)
local pending = redis.call('XPENDING', KEYS[1], ARGV[4], ARGV[2], ARGV[2], 1)

if not mapped_entry_id and #entries == 0 and #pending == 0 then
    return {2, 0, 0}
end

if mapped_entry_id ~= ARGV[2] then
    return {0, 0, 0}
end

if #entries ~= 1 or entries[1][1] ~= ARGV[2] then
    return {0, 0, 0}
end
local fields = entries[1][2]
if #fields ~= 2 or fields[1] ~= 'signed_envelope' or fields[2] ~= ARGV[3] then
    return {0, 0, 0}
end

if #pending ~= 1 or pending[1][1] ~= ARGV[2] or pending[1][2] ~= ARGV[5] then
    return {0, 0, 0}
end

local acknowledged = redis.call('XACK', KEYS[1], ARGV[4], ARGV[2])
local deleted = redis.call('XDEL', KEYS[1], ARGV[2])
local unmapped = redis.call('HDEL', KEYS[2], ARGV[1])
return {acknowledged, deleted, unmapped}
";

/// Non-secret policy for one Redis command-stream consumer.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RedisStreamsConfig {
    pub url: String,
    pub stream: String,
    pub group: String,
    pub consumer: String,
    pub command_limits: RedisCommandLimits,
    pub read_count: u64,
    pub block_millis: u64,
    pub reclaim_idle_millis: u64,
    pub connection_timeout: Duration,
    pub control_timeout: Duration,
    pub control_concurrency: usize,
}

impl RedisStreamsConfig {
    fn validate(&self) -> Result<ValidatedRedisConfig, RedisStreamsError> {
        let endpoint = validate_endpoint(&self.url)?;
        if !bounded_ascii_identity(&self.stream, MAX_STREAM_BYTES)
            || !bounded_ascii_identity(&self.group, MAX_IDENTITY_BYTES)
            || !bounded_ascii_identity(&self.consumer, MAX_IDENTITY_BYTES)
        {
            return Err(RedisStreamsError::configuration(
                "the Redis stream identity is malformed",
            ));
        }
        self.command_limits.validate().map_err(|_| {
            RedisStreamsError::configuration("the Redis command limits are malformed")
        })?;
        if self.command_limits.max_entry_bytes != RUNTIME_V1_ENTRY_BYTES
            || self.command_limits.max_field_bytes != RUNTIME_V1_FIELD_BYTES
            || !(1..=MAX_READ_COUNT).contains(&self.read_count)
            || !(MIN_BLOCK_MILLIS..=MAX_BLOCK_MILLIS).contains(&self.block_millis)
            || !(MIN_RECLAIM_IDLE_MILLIS..=MAX_RECLAIM_IDLE_MILLIS)
                .contains(&self.reclaim_idle_millis)
            || !bounded_operation_timeout(self.connection_timeout)
            || !bounded_operation_timeout(self.control_timeout)
            || !(1..=MAX_CONTROL_CONCURRENCY).contains(&self.control_concurrency)
        {
            return Err(RedisStreamsError::configuration(
                "the Redis command transport limits are malformed",
            ));
        }
        let intake_timeout =
            Duration::from_millis(self.block_millis.checked_add(1_000).ok_or_else(|| {
                RedisStreamsError::configuration("the Redis block limit is malformed")
            })?);
        Ok(ValidatedRedisConfig {
            endpoint,
            stream: self.stream.clone(),
            group: self.group.clone(),
            consumer: self.consumer.clone(),
            command_limits: self.command_limits,
            read_count: self.read_count,
            block_millis: self.block_millis,
            reclaim_idle_millis: self.reclaim_idle_millis,
            connection_timeout: self.connection_timeout,
            control_timeout: self.control_timeout,
            intake_timeout,
            control_concurrency: self.control_concurrency,
        })
    }
}

/// Secret material loaded from the worker's private-plane workload identity.
///
/// This value is intentionally non-`Clone` and non-`Debug`. The source buffers
/// are zeroized after client construction. redis-rs and Rustls retain the parsed
/// credentials for the lifetime of the connected transport process.
pub struct RedisTlsMaterial {
    password: Zeroizing<String>,
    ca_pem: Zeroizing<Vec<u8>>,
    client_certificate_pem: Zeroizing<Vec<u8>>,
    client_private_key_pem: Zeroizing<Vec<u8>>,
}

impl RedisTlsMaterial {
    /// Construct bounded Redis credentials without parsing or connecting.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration error for empty, oversized, or
    /// control-bearing password/PEM material.
    pub fn new(
        password: String,
        ca_pem: Vec<u8>,
        client_certificate_pem: Vec<u8>,
        client_private_key_pem: Vec<u8>,
    ) -> Result<Self, RedisStreamsError> {
        if password.is_empty()
            || password.len() > MAX_PASSWORD_BYTES
            || password
                .bytes()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
            || !valid_pem_input(&ca_pem)
            || !valid_pem_input(&client_certificate_pem)
            || !valid_pem_input(&client_private_key_pem)
        {
            return Err(RedisStreamsError::configuration(
                "the Redis TLS credential material is malformed",
            ));
        }
        Ok(Self {
            password: Zeroizing::new(password),
            ca_pem: Zeroizing::new(ca_pem),
            client_certificate_pem: Zeroizing::new(client_certificate_pem),
            client_private_key_pem: Zeroizing::new(client_private_key_pem),
        })
    }
}

/// One bounded page returned by Redis 7 `XAUTOCLAIM`.
pub struct RedisReclaimPage {
    pub next_start_id: String,
    pub deliveries: Vec<RedisCommandDelivery>,
}

/// One page of entries already pending for the configured consumer.
/// A zero cursor starts another scan. This page grants no execution authority.
pub(crate) struct RedisOwnedPendingPage {
    pub next_start_id: String,
    pub deliveries: Vec<RedisCommandDelivery>,
}

/// Stable low-cardinality Redis transport failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RedisStreamsErrorKind {
    Configuration,
    Authentication,
    DependencyUnavailable,
    Timeout,
    ConsumerGroupMissing,
    Protocol,
    ResourceExhausted,
    Closed,
}

/// Data-free Redis transport failure suitable for operator logs and metrics.
pub struct RedisStreamsError {
    kind: RedisStreamsErrorKind,
    message: &'static str,
    upstream_kind: Option<RedisErrorKind>,
}

impl RedisStreamsError {
    pub(crate) const fn configuration(message: &'static str) -> Self {
        Self {
            kind: RedisStreamsErrorKind::Configuration,
            message,
            upstream_kind: None,
        }
    }

    pub(crate) const fn consumer_group_missing(message: &'static str) -> Self {
        Self {
            kind: RedisStreamsErrorKind::ConsumerGroupMissing,
            message,
            upstream_kind: None,
        }
    }

    pub(crate) const fn protocol(message: &'static str) -> Self {
        Self {
            kind: RedisStreamsErrorKind::Protocol,
            message,
            upstream_kind: None,
        }
    }

    pub(crate) const fn authentication(message: &'static str) -> Self {
        Self {
            kind: RedisStreamsErrorKind::Authentication,
            message,
            upstream_kind: None,
        }
    }

    pub(crate) const fn unavailable(message: &'static str) -> Self {
        Self {
            kind: RedisStreamsErrorKind::DependencyUnavailable,
            message,
            upstream_kind: None,
        }
    }

    pub(crate) const fn timeout(message: &'static str) -> Self {
        Self {
            kind: RedisStreamsErrorKind::Timeout,
            message,
            upstream_kind: None,
        }
    }

    pub(crate) const fn resource_exhausted(message: &'static str) -> Self {
        Self {
            kind: RedisStreamsErrorKind::ResourceExhausted,
            message,
            upstream_kind: None,
        }
    }

    pub(crate) const fn closed() -> Self {
        Self {
            kind: RedisStreamsErrorKind::Closed,
            message: "the Redis command transport is closed",
            upstream_kind: None,
        }
    }

    fn from_redis(error: RedisError, operation: &'static str) -> Self {
        let upstream_kind = error.kind();
        let timeout = error.is_timeout();
        let missing_group = error.code() == Some("NOGROUP");
        drop(error);
        if missing_group && !timeout {
            return Self {
                upstream_kind: Some(upstream_kind),
                ..Self::consumer_group_missing(operation)
            };
        }
        let kind = if timeout {
            RedisStreamsErrorKind::Timeout
        } else {
            match upstream_kind {
                RedisErrorKind::AuthenticationFailed
                | RedisErrorKind::Server(redis::ServerErrorKind::NoPerm) => {
                    RedisStreamsErrorKind::Authentication
                }
                RedisErrorKind::Server(
                    redis::ServerErrorKind::BusyLoading
                    | redis::ServerErrorKind::TryAgain
                    | redis::ServerErrorKind::ClusterDown
                    | redis::ServerErrorKind::MasterDown
                    | redis::ServerErrorKind::ReadOnly,
                ) => RedisStreamsErrorKind::DependencyUnavailable,
                RedisErrorKind::Parse
                | RedisErrorKind::UnexpectedReturnType
                | RedisErrorKind::Client
                | RedisErrorKind::Extension
                | RedisErrorKind::Server(_)
                | RedisErrorKind::RESP3NotSupported => RedisStreamsErrorKind::Protocol,
                RedisErrorKind::InvalidClientConfig => RedisStreamsErrorKind::Configuration,
                _ => RedisStreamsErrorKind::DependencyUnavailable,
            }
        };
        Self {
            kind,
            message: operation,
            upstream_kind: Some(upstream_kind),
        }
    }

    /// Stable failure category for branching and metrics.
    #[must_use]
    pub const fn kind(&self) -> RedisStreamsErrorKind {
        self.kind
    }

    /// Stable metric/log code that never contains server or credential data.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self.kind {
            RedisStreamsErrorKind::Configuration => "redis_streams.configuration",
            RedisStreamsErrorKind::Authentication => "redis_streams.authentication",
            RedisStreamsErrorKind::DependencyUnavailable => "redis_streams.unavailable",
            RedisStreamsErrorKind::Timeout => "redis_streams.timeout",
            RedisStreamsErrorKind::ConsumerGroupMissing => "redis_streams.consumer_group_missing",
            RedisStreamsErrorKind::Protocol => "redis_streams.protocol",
            RedisStreamsErrorKind::ResourceExhausted => "redis_streams.resource_exhausted",
            RedisStreamsErrorKind::Closed => "redis_streams.closed",
        }
    }

    /// Whether an outer coordinator may reconnect and retry a fresh operation.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self.kind,
            RedisStreamsErrorKind::DependencyUnavailable | RedisStreamsErrorKind::Timeout
        )
    }

    /// Safe upstream category retained without redis-rs' potentially sensitive
    /// server error text.
    #[must_use]
    pub fn upstream_kind(&self) -> Option<&RedisErrorKind> {
        self.upstream_kind.as_ref()
    }
}

impl fmt::Debug for RedisStreamsError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RedisStreamsError")
            .field("kind", &self.kind)
            .field("code", &self.code())
            .field("message", &self.message)
            .field("upstream_kind", &self.upstream_kind)
            .finish()
    }
}

impl fmt::Display for RedisStreamsError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for RedisStreamsError {}

/// Connected, restricted Redis command-plane client.
///
/// Intake uses one dedicated connection because `XREADGROUP BLOCK` occupies a
/// Redis connection. Control operations use a second connection and a bounded
/// semaphore. Neither client has a reconnect policy and each command has one
/// send attempt; the worker intake/coordinator owns reconnect and ambiguity.
pub struct RedisStreamsClient {
    intake: MultiplexedConnection,
    control: MultiplexedConnection,
    config: ValidatedRedisConfig,
    intake_gate: Arc<Semaphore>,
    control_gate: Arc<Semaphore>,
    tasks: StdMutex<Option<ConnectionTasks>>,
    failed: Arc<AtomicBool>,
    closed: AtomicBool,
}

impl RedisStreamsClient {
    #[must_use]
    pub(crate) const fn delivery_batch_size(&self) -> u64 {
        self.config.read_count
    }

    /// Build two mTLS Redis connections and wait for both to authenticate.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration, authentication, timeout, or dependency
    /// error. Cancellation aborts both connection drivers; it never leaves a
    /// detached initialized Redis client.
    pub async fn connect(
        config: RedisStreamsConfig,
        tls: RedisTlsMaterial,
    ) -> Result<Self, RedisStreamsError> {
        let config = config.validate()?;
        let tls_config = Arc::new(build_tls_config(&tls)?);
        let (intake, control) = tokio::join!(
            connect_redis(
                &config,
                &tls,
                tls_config.clone(),
                config.intake_timeout,
                1,
                "the Redis intake connection failed",
            ),
            connect_redis(
                &config,
                &tls,
                tls_config,
                config.control_timeout,
                config.control_concurrency,
                "the Redis control connection failed",
            ),
        );
        let (intake, intake_task) = intake?;
        let (control, control_task) = control?;
        let tasks = ConnectionTasks::new(intake_task, control_task);

        Ok(Self {
            intake,
            control,
            control_gate: Arc::new(Semaphore::new(config.control_concurrency)),
            config,
            intake_gate: Arc::new(Semaphore::new(1)),
            tasks: StdMutex::new(Some(tasks)),
            failed: Arc::new(AtomicBool::new(false)),
            closed: AtomicBool::new(false),
        })
    }

    /// Validate both authenticated Redis connections.
    ///
    /// # Errors
    ///
    /// Returns a stable transport error if either `PING` fails or returns a
    /// malformed response.
    pub async fn ping(&self) -> Result<(), RedisStreamsError> {
        self.ensure_open()?;
        let intake_permit = self.intake_permit().await?;
        let mut intake = self.intake.clone();
        let intake = run_owned_operation(intake_permit, self.failed.clone(), async move {
            cmd("PING")
                .query_async::<Value>(&mut intake)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "the Redis intake ping failed")
                })
        })
        .await?;
        let control_permit = self.control_permit().await?;
        self.ensure_open()?;
        let mut control = self.control.clone();
        let control = run_owned_operation(control_permit, self.failed.clone(), async move {
            cmd("PING")
                .query_async::<Value>(&mut control)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "the Redis control ping failed")
                })
        })
        .await?;
        if !is_pong(&intake) || !is_pong(&control) {
            self.fail();
            return Err(RedisStreamsError::protocol(
                "Redis returned a malformed ping response",
            ));
        }
        Ok(())
    }

    /// Check the configured group with one read-only pending-entry row.
    ///
    /// Group creation remains owned by the deployment bootstrap. This check
    /// neither admits a delivery nor changes its pending owner.
    ///
    /// # Errors
    ///
    /// Returns a missing-group, transport, protocol, or response-bound error.
    pub(crate) async fn verify_consumer_group(&self) -> Result<(), RedisStreamsError> {
        self.ensure_open()?;
        let permit = self.control_permit().await?;
        self.ensure_open()?;
        let mut control = self.control.clone();
        let stream = self.config.stream.clone();
        let group = self.config.group.clone();
        let response = run_owned_operation(permit, self.failed.clone(), async move {
            cmd("XPENDING")
                .arg(stream)
                .arg(group)
                .arg("-")
                .arg("+")
                .arg(1)
                .query_async::<Value>(&mut control)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "Redis consumer group check failed")
                })
        })
        .await?;
        let result = decode_group_check_response(response);
        if result.is_err() {
            self.fail();
        }
        result
    }

    /// Block for new entries in the configured consumer group.
    ///
    /// No group is created and `NOACK` is never used. The dedicated intake
    /// owned permit admits only one outstanding blocking read and remains held
    /// until Redis replies or the configured response timeout expires.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration, transport, protocol, or post-decode
    /// resource-bound error.
    pub async fn read_new(
        &self,
        count: u64,
        block_millis: u64,
    ) -> Result<Vec<RedisCommandDelivery>, RedisStreamsError> {
        self.ensure_open()?;
        if !valid_read_request(&self.config, count, block_millis) {
            return Err(RedisStreamsError::configuration(
                "the Redis intake request exceeds its configured bound",
            ));
        }
        let permit = self.intake_permit().await?;
        self.ensure_open()?;
        let mut intake = self.intake.clone();
        let group = self.config.group.clone();
        let consumer = self.config.consumer.clone();
        let stream = self.config.stream.clone();
        let response = run_owned_operation(permit, self.failed.clone(), async move {
            cmd("XREADGROUP")
                .arg("GROUP")
                .arg(group)
                .arg(consumer)
                .arg("COUNT")
                .arg(count)
                .arg("BLOCK")
                .arg(block_millis)
                .arg("STREAMS")
                .arg(stream)
                .arg(">")
                .query_async::<Value>(&mut intake)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "Redis command intake failed")
                })
        })
        .await?;
        let result = decode_read_response(
            response,
            &self.config.stream,
            count,
            self.config.command_limits,
        );
        if result.is_err() {
            self.fail();
        }
        result
    }

    /// Read only this consumer's pending entries without claiming another owner.
    /// Main must still authorize each exact redelivery before execution.
    /// # Errors
    /// Returns bounded configuration, transport, cursor, or payload errors.
    pub(crate) async fn read_owned_pending(
        &self,
        count: u64,
        start_id: &str,
    ) -> Result<RedisOwnedPendingPage, RedisStreamsError> {
        self.ensure_open()?;
        if count == 0 || count > self.config.read_count {
            return Err(RedisStreamsError::configuration(
                "the Redis pending read exceeds its configured bound",
            ));
        }
        decode_entry_id(start_id.as_bytes()).map_err(|error| map_command_decode_error(&error))?;
        let permit = self.intake_permit().await?;
        self.ensure_open()?;
        let mut intake = self.intake.clone();
        let command = owned_pending_read_command(
            &self.config.stream,
            &self.config.group,
            &self.config.consumer,
            count,
            start_id,
        );
        let response = run_owned_operation(permit, self.failed.clone(), async move {
            command
                .query_async::<Value>(&mut intake)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "Redis owned pending read failed")
                })
        })
        .await?;
        let result = decode_owned_pending_response(
            response,
            &self.config.stream,
            count,
            start_id,
            self.config.command_limits,
        );
        if result.is_err() {
            self.fail();
        }
        result
    }

    /// Reclaim one bounded page of abandoned pending entries with Redis 7
    /// `XAUTOCLAIM`.
    ///
    /// # Errors
    ///
    /// Returns a stable validation, transport, or response-shape error.
    pub async fn reclaim_page(
        &self,
        min_idle_millis: u64,
        start_id: &str,
        count: u64,
    ) -> Result<RedisReclaimPage, RedisStreamsError> {
        self.ensure_open()?;
        if !(MIN_RECLAIM_IDLE_MILLIS..=self.config.reclaim_idle_millis).contains(&min_idle_millis)
            || count == 0
            || count > self.config.read_count
        {
            return Err(RedisStreamsError::configuration(
                "the Redis reclaim request exceeds its configured bound",
            ));
        }
        decode_entry_id(start_id.as_bytes()).map_err(|error| map_command_decode_error(&error))?;
        let permit = self.control_permit().await?;
        self.ensure_open()?;
        let mut control = self.control.clone();
        let stream = self.config.stream.clone();
        let group = self.config.group.clone();
        let consumer = self.config.consumer.clone();
        let start_id = start_id.to_owned();
        let response = run_owned_operation(permit, self.failed.clone(), async move {
            cmd("XAUTOCLAIM")
                .arg(stream)
                .arg(group)
                .arg(consumer)
                .arg(min_idle_millis)
                .arg(start_id)
                .arg("COUNT")
                .arg(count)
                .query_async::<Value>(&mut control)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "Redis command reclaim failed")
                })
        })
        .await?;
        let result = decode_reclaim_response(
            response,
            &self.config.stream,
            count,
            self.config.command_limits,
        );
        if result.is_err() {
            self.fail();
        }
        result
    }

    /// Refresh PEL idle time only for entries still owned by this consumer.
    ///
    /// This is transport liveness, not a Go business lease or authority.
    ///
    /// # Errors
    ///
    /// Returns a stable bound, ownership-response, or transport error.
    pub async fn heartbeat_owned_pending(
        &self,
        entry_ids: &[String],
    ) -> Result<Vec<String>, RedisStreamsError> {
        self.ensure_open()?;
        if entry_ids.is_empty() {
            return Ok(Vec::new());
        }
        if entry_ids.len() > usize::try_from(self.config.read_count).unwrap_or(usize::MAX) {
            return Err(RedisStreamsError::resource_exhausted(
                "the Redis heartbeat exceeds its configured batch bound",
            ));
        }
        let mut unique = BTreeSet::new();
        for entry_id in entry_ids {
            decode_entry_id(entry_id.as_bytes())
                .map_err(|error| map_command_decode_error(&error))?;
            if !unique.insert(entry_id.as_str()) {
                return Err(RedisStreamsError::protocol(
                    "the Redis heartbeat contains duplicate entry identities",
                ));
            }
        }
        let permit = self.control_permit().await?;
        self.ensure_open()?;
        let mut control = self.control.clone();
        let stream = self.config.stream.clone();
        let group = self.config.group.clone();
        let consumer = self.config.consumer.clone();
        let requested_entry_ids = entry_ids.to_vec();
        let command_entry_ids = requested_entry_ids.clone();
        let response = run_owned_operation(permit, self.failed.clone(), async move {
            let mut command = cmd("EVAL");
            command
                .arg(HEARTBEAT_OWNED_PENDING_SCRIPT)
                .arg(1)
                .arg(stream)
                .arg(group)
                .arg(consumer);
            for entry_id in &command_entry_ids {
                command.arg(entry_id);
            }
            command
                .query_async::<Value>(&mut control)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "Redis command heartbeat failed")
                })
        })
        .await?;
        let result = decode_heartbeat_response(response, &requested_entry_ids);
        if result.is_err() {
            self.fail();
        }
        result
    }

    /// Gracefully close both Redis connection drivers.
    ///
    /// The close transition is one-way and cancellation-safe: dropping this
    /// future aborts the now-unusable connection tasks rather than detaching
    /// them. The caller still owns the outer shutdown deadline.
    ///
    /// # Errors
    ///
    /// Returns the first stable Redis or connection-task failure. Repeated
    /// close calls are successful no-ops.
    pub async fn close(&self) -> Result<(), RedisStreamsError> {
        if self.closed.swap(true, Ordering::AcqRel) {
            return Ok(());
        }
        let mut tasks = self
            .tasks
            .lock()
            .map_err(|_| RedisStreamsError::protocol("the Redis task owner is unavailable"))?
            .take();
        let _intake_idle = self
            .intake_gate
            .clone()
            .acquire_owned()
            .await
            .map_err(|_| RedisStreamsError::closed())?;
        let control_permits = u32::try_from(self.config.control_concurrency).map_err(|_| {
            RedisStreamsError::configuration("the Redis control concurrency is malformed")
        })?;
        let _control_idle = self
            .control_gate
            .clone()
            .acquire_many_owned(control_permits)
            .await
            .map_err(|_| RedisStreamsError::closed())?;
        self.intake_gate.close();
        self.control_gate.close();
        let mut intake = self.intake.clone();
        let mut control = self.control.clone();
        let intake_quit_command = cmd("QUIT");
        let control_quit_command = cmd("QUIT");
        let (intake_quit, control_quit) = tokio::join!(
            intake_quit_command.query_async::<Value>(&mut intake),
            control_quit_command.query_async::<Value>(&mut control),
        );
        let intake_quit = intake_quit
            .map_err(|error| RedisStreamsError::from_redis(error, "the Redis intake close failed"));
        let control_quit = control_quit.map_err(|error| {
            RedisStreamsError::from_redis(error, "the Redis control close failed")
        });
        let task_result = if let Some(tasks) = tasks.as_mut() {
            tasks.finish().await
        } else {
            Ok(())
        };
        intake_quit?;
        control_quit?;
        task_result
    }

    fn ensure_open(&self) -> Result<(), RedisStreamsError> {
        if self.closed.load(Ordering::Acquire) {
            Err(RedisStreamsError::closed())
        } else if self.failed.load(Ordering::Acquire) {
            Err(RedisStreamsError::unavailable(
                "the Redis command transport requires reconnection",
            ))
        } else {
            Ok(())
        }
    }

    fn fail(&self) {
        self.failed.store(true, Ordering::Release);
    }

    async fn intake_permit(&self) -> Result<tokio::sync::OwnedSemaphorePermit, RedisStreamsError> {
        self.intake_gate
            .clone()
            .acquire_owned()
            .await
            .map_err(|_| RedisStreamsError::closed())
    }

    async fn control_permit(&self) -> Result<tokio::sync::OwnedSemaphorePermit, RedisStreamsError> {
        self.control_gate
            .clone()
            .acquire_owned()
            .await
            .map_err(|_| RedisStreamsError::closed())
    }

    async fn retire_exact(
        &self,
        request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
        self.ensure_open()
            .map_err(|error| map_retirement_error(&error))?;
        if request.stream() != self.config.stream
            || request.group() != self.config.group
            || request.consumer() != self.config.consumer
        {
            return Err(RedisRetirementClientError::Protocol);
        }
        let permit = self
            .control_permit()
            .await
            .map_err(|error| map_retirement_error(&error))?;
        self.ensure_open()
            .map_err(|error| map_retirement_error(&error))?;
        let index_key = format!("{}:delivery-index.v1", request.stream());
        let mut control = self.control.clone();
        let stream = request.stream().to_owned();
        let stable_delivery_id = request.stable_delivery_id().to_owned();
        let entry_id = request.entry_id().to_owned();
        let signed_envelope = request.signed_envelope().to_vec();
        let group = request.group().to_owned();
        let consumer = request.consumer().to_owned();
        let response = run_owned_operation(permit, self.failed.clone(), async move {
            cmd("EVAL")
                .arg(RETIRE_DELIVERY_SCRIPT)
                .arg(2)
                .arg(stream)
                .arg(index_key)
                .arg(stable_delivery_id)
                .arg(entry_id)
                .arg(signed_envelope)
                .arg(group)
                .arg(consumer)
                .query_async::<Value>(&mut control)
                .await
                .map_err(|error| {
                    RedisStreamsError::from_redis(error, "Redis command retirement failed")
                })
        })
        .await
        .map_err(|error| map_retirement_error(&error))?;
        let result = decode_retirement_response(response);
        if result.is_err() {
            self.fail();
        }
        result
    }
}

impl Drop for RedisStreamsClient {
    fn drop(&mut self) {
        self.closed.store(true, Ordering::Release);
        self.intake_gate.close();
        self.control_gate.close();
        if let Ok(tasks) = self.tasks.get_mut()
            && let Some(tasks) = tasks.as_mut()
        {
            tasks.abort();
        }
    }
}

#[async_trait]
impl RedisRetirementClient for RedisStreamsClient {
    async fn retire_delivery(
        &self,
        request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
        self.retire_exact(request).await
    }
}

#[async_trait]
impl RedisRetirementClient for std::sync::Arc<RedisStreamsClient> {
    async fn retire_delivery(
        &self,
        request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
        self.retire_exact(request).await
    }
}

struct ValidatedRedisConfig {
    endpoint: RedisEndpoint,
    stream: String,
    group: String,
    consumer: String,
    command_limits: RedisCommandLimits,
    read_count: u64,
    block_millis: u64,
    reclaim_idle_millis: u64,
    connection_timeout: Duration,
    control_timeout: Duration,
    intake_timeout: Duration,
    control_concurrency: usize,
}

struct RedisEndpoint {
    host: String,
    port: u16,
    username: String,
}

struct ConnectionTasks {
    intake: Option<DriverTask>,
    control: Option<DriverTask>,
}

impl ConnectionTasks {
    const fn new(intake: DriverTask, control: DriverTask) -> Self {
        Self {
            intake: Some(intake),
            control: Some(control),
        }
    }

    fn abort(&mut self) {
        if let Some(task) = self.intake.as_mut() {
            task.abort();
        }
        if let Some(task) = self.control.as_mut() {
            task.abort();
        }
    }

    async fn finish(&mut self) -> Result<(), RedisStreamsError> {
        if let Some(task) = self.intake.as_mut() {
            task.finish("the Redis intake task was lost").await?;
            self.intake = None;
        }
        if let Some(task) = self.control.as_mut() {
            task.finish("the Redis control task was lost").await?;
            self.control = None;
        }
        Ok(())
    }
}

struct DriverTask {
    handle: Option<JoinHandle<()>>,
}

impl DriverTask {
    fn new(handle: JoinHandle<()>) -> Self {
        Self {
            handle: Some(handle),
        }
    }

    fn abort(&mut self) {
        if let Some(handle) = self.handle.as_ref() {
            handle.abort();
        }
    }

    async fn finish(&mut self, lost_message: &'static str) -> Result<(), RedisStreamsError> {
        let Some(handle) = self.handle.take() else {
            return Ok(());
        };
        handle
            .await
            .map_err(|_| RedisStreamsError::protocol(lost_message))
    }
}

impl Drop for DriverTask {
    fn drop(&mut self) {
        self.abort();
    }
}

impl Drop for ConnectionTasks {
    fn drop(&mut self) {
        self.abort();
    }
}

fn validate_endpoint(url: &str) -> Result<RedisEndpoint, RedisStreamsError> {
    if url.is_empty()
        || url.len() > MAX_URL_BYTES
        || !url
            .bytes()
            .all(|byte| (0x21..=0x7e).contains(&byte) && !matches!(byte, b'%' | b'?' | b'#'))
    {
        return Err(RedisStreamsError::configuration(
            "the Redis TLS URL is malformed",
        ));
    }
    let authority = url
        .strip_prefix("rediss://")
        .and_then(|remainder| remainder.strip_suffix("/0"))
        .ok_or_else(|| RedisStreamsError::configuration("the Redis TLS URL is malformed"))?;
    let (username, host_and_port) = authority
        .split_once('@')
        .filter(|(username, host)| !username.is_empty() && !host.is_empty() && !host.contains('@'))
        .ok_or_else(|| RedisStreamsError::configuration("the Redis TLS URL is malformed"))?;
    if username.len() > MAX_IDENTITY_BYTES
        || !username
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
    {
        return Err(RedisStreamsError::configuration(
            "the Redis ACL username is malformed",
        ));
    }
    let (host, port_text) = if let Some(bracketed) = host_and_port.strip_prefix('[') {
        let (host, suffix) = bracketed
            .split_once(']')
            .ok_or_else(|| RedisStreamsError::configuration("the Redis TLS URL is malformed"))?;
        let port = suffix
            .strip_prefix(':')
            .ok_or_else(|| RedisStreamsError::configuration("the Redis TLS URL is malformed"))?;
        if !host.contains(':')
            || !host
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit() || matches!(byte, b':' | b'.'))
        {
            return Err(RedisStreamsError::configuration(
                "the Redis TLS URL is malformed",
            ));
        }
        (host, port)
    } else {
        let (host, port) = host_and_port
            .rsplit_once(':')
            .ok_or_else(|| RedisStreamsError::configuration("the Redis TLS URL is malformed"))?;
        if !host
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
        {
            return Err(RedisStreamsError::configuration(
                "the Redis TLS URL is malformed",
            ));
        }
        (host, port)
    };
    if host.is_empty()
        || host
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte.is_ascii_whitespace())
        || port_text.is_empty()
        || !port_text.bytes().all(|byte| byte.is_ascii_digit())
    {
        return Err(RedisStreamsError::configuration(
            "the Redis TLS URL is malformed",
        ));
    }
    let port = port_text
        .parse::<u16>()
        .ok()
        .filter(|port| *port != 0 && port_text == port.to_string())
        .ok_or_else(|| RedisStreamsError::configuration("the Redis TLS URL is malformed"))?;
    Ok(RedisEndpoint {
        host: host.to_owned(),
        port,
        username: username.to_owned(),
    })
}

fn build_tls_config(tls: &RedisTlsMaterial) -> Result<ClientConfig, RedisStreamsError> {
    let mut roots = RootCertStore::empty();
    let ca_certificates = CertificateDer::pem_slice_iter(&tls.ca_pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| RedisStreamsError::configuration("the Redis private CA is malformed"))?;
    if ca_certificates.is_empty() {
        return Err(RedisStreamsError::configuration(
            "the Redis private CA is empty",
        ));
    }
    for certificate in ca_certificates {
        roots
            .add(certificate)
            .map_err(|_| RedisStreamsError::configuration("the Redis private CA is malformed"))?;
    }
    let client_certificates = CertificateDer::pem_slice_iter(&tls.client_certificate_pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| {
            RedisStreamsError::configuration("the Redis client certificate is malformed")
        })?;
    if client_certificates.is_empty() {
        return Err(RedisStreamsError::configuration(
            "the Redis client certificate is empty",
        ));
    }
    let private_key = PrivateKeyDer::from_pem_slice(&tls.client_private_key_pem)
        .map_err(|_| RedisStreamsError::configuration("the Redis client key is malformed"))?;
    ClientConfig::builder_with_provider(rustls::crypto::ring::default_provider().into())
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| RedisStreamsError::configuration("the Redis TLS profile is unavailable"))?
        .with_root_certificates(roots)
        .with_client_auth_cert(client_certificates, private_key)
        .map_err(|_| RedisStreamsError::configuration("the Redis client identity is malformed"))
}

async fn connect_redis(
    config: &ValidatedRedisConfig,
    tls: &RedisTlsMaterial,
    tls_config: Arc<ClientConfig>,
    operation_timeout: Duration,
    command_buffer_len: usize,
    failure_message: &'static str,
) -> Result<(MultiplexedConnection, DriverTask), RedisStreamsError> {
    let host = config.endpoint.host.clone();
    let port = config.endpoint.port;
    let username = config.endpoint.username.clone();
    let password = tls.password.to_string();
    let connection_timeout = config.connection_timeout;
    let connection = async move {
        let tcp = TcpStream::connect((host.as_str(), port))
            .await
            .map_err(|_| RedisStreamsError::unavailable(failure_message))?;
        tcp.set_nodelay(true)
            .map_err(|_| RedisStreamsError::unavailable(failure_message))?;
        let server_name = ServerName::try_from(host)
            .map_err(|_| RedisStreamsError::configuration("the Redis TLS URL is malformed"))?;
        let stream = TlsConnector::from(tls_config)
            .connect(server_name, tcp)
            .await
            .map_err(|error| map_tls_handshake_error(error, failure_message))?;
        let redis = RedisConnectionInfo::default()
            .set_db(0)
            .set_username(username)
            .set_password(password)
            .set_protocol(ProtocolVersion::RESP2)
            .set_skip_set_lib_name();
        let connection_config = AsyncConnectionConfig::new()
            .set_connection_timeout(None)
            .set_response_timeout(Some(operation_timeout))
            .set_pipeline_buffer_size(command_buffer_len)
            .set_concurrency_limit(command_buffer_len);
        let (connection, driver) =
            MultiplexedConnection::new_with_config(&redis, stream, connection_config)
                .await
                .map_err(|error| RedisStreamsError::from_redis(error, failure_message))?;
        Ok((connection, DriverTask::new(tokio::spawn(driver))))
    };
    tokio::time::timeout(connection_timeout, connection)
        .await
        .map_err(|_| RedisStreamsError::timeout(failure_message))?
}

async fn run_owned_operation<T, F>(
    permit: tokio::sync::OwnedSemaphorePermit,
    failed: Arc<AtomicBool>,
    operation: F,
) -> Result<T, RedisStreamsError>
where
    T: Send + 'static,
    F: Future<Output = Result<T, RedisStreamsError>> + Send + 'static,
{
    let (sender, receiver) = tokio::sync::oneshot::channel();
    tokio::spawn(async move {
        let result = operation.await;
        if result.is_err() {
            failed.store(true, Ordering::Release);
        }
        let _ = sender.send(result);
        drop(permit);
    });
    receiver
        .await
        .map_err(|_| RedisStreamsError::protocol("the Redis command task was lost"))?
}

fn map_tls_handshake_error(error: io::Error, message: &'static str) -> RedisStreamsError {
    let kind = error.kind();
    drop(error);
    match kind {
        io::ErrorKind::TimedOut | io::ErrorKind::WouldBlock => RedisStreamsError::timeout(message),
        io::ErrorKind::InvalidData | io::ErrorKind::PermissionDenied => {
            RedisStreamsError::authentication(message)
        }
        _ => RedisStreamsError::unavailable(message),
    }
}

fn decode_group_check_response(response: Value) -> Result<(), RedisStreamsError> {
    let rows = value_array(response, "the Redis group check response is malformed")?;
    if rows.len() > 1 {
        return Err(RedisStreamsError::resource_exhausted(
            "the Redis group check exceeds its row bound",
        ));
    }
    for row in rows {
        let mut fields = value_array(row, "the Redis group check row is malformed")?;
        if fields.len() != 4 {
            return Err(RedisStreamsError::protocol(
                "the Redis group check row is malformed",
            ));
        }
        let deliveries = fields.pop();
        let idle_millis = fields.pop();
        if !matches!(deliveries, Some(Value::Int(value)) if value >= 0)
            || !matches!(idle_millis, Some(Value::Int(value)) if value >= 0)
        {
            return Err(RedisStreamsError::protocol(
                "the Redis group check counters are malformed",
            ));
        }
        let consumer = value_text(
            fields.pop().ok_or_else(|| {
                RedisStreamsError::protocol("the Redis group check consumer is malformed")
            })?,
            "the Redis group check consumer is malformed",
        )?;
        if !std::str::from_utf8(&consumer)
            .is_ok_and(|value| bounded_ascii_identity(value, MAX_IDENTITY_BYTES))
        {
            return Err(RedisStreamsError::protocol(
                "the Redis group check consumer is malformed",
            ));
        }
        let entry_id = value_text(
            fields.pop().ok_or_else(|| {
                RedisStreamsError::protocol("the Redis group check entry is malformed")
            })?,
            "the Redis group check entry is malformed",
        )?;
        decode_entry_id(&entry_id).map_err(|error| map_command_decode_error(&error))?;
    }
    Ok(())
}

fn decode_read_response(
    response: Value,
    expected_stream: &str,
    max_count: u64,
    limits: RedisCommandLimits,
) -> Result<Vec<RedisCommandDelivery>, RedisStreamsError> {
    if matches!(response, Value::Nil) {
        return Ok(Vec::new());
    }
    let streams = value_array(response, "the Redis intake response is malformed")?;
    if streams.len() > 1 {
        return Err(RedisStreamsError::protocol(
            "Redis returned more than one command stream",
        ));
    }
    let mut deliveries = Vec::new();
    for stream in streams {
        let mut pair = value_array(stream, "the Redis intake stream response is malformed")?;
        if pair.len() != 2 {
            return Err(RedisStreamsError::protocol(
                "the Redis intake stream response is malformed",
            ));
        }
        let entries = value_array(
            pair.pop().ok_or_else(|| {
                RedisStreamsError::protocol("the Redis intake response is malformed")
            })?,
            "the Redis intake entries are malformed",
        )?;
        let stream = value_text(
            pair.pop().ok_or_else(|| {
                RedisStreamsError::protocol("the Redis intake response is malformed")
            })?,
            "the Redis intake stream identity is malformed",
        )?;
        if stream.as_slice() != expected_stream.as_bytes() {
            return Err(RedisStreamsError::protocol(
                "Redis returned a command from another stream",
            ));
        }
        for entry in entries {
            deliveries.push(decode_delivery(entry, expected_stream, limits)?);
            if u64::try_from(deliveries.len()).unwrap_or(u64::MAX) > max_count {
                return Err(RedisStreamsError::resource_exhausted(
                    "the Redis intake response exceeds its batch bound",
                ));
            }
        }
    }
    Ok(deliveries)
}

fn owned_pending_read_command(
    stream: &str,
    group: &str,
    consumer: &str,
    count: u64,
    start_id: &str,
) -> redis::Cmd {
    let mut command = cmd("XREADGROUP");
    command
        .arg("GROUP")
        .arg(group)
        .arg(consumer)
        .arg("COUNT")
        .arg(count)
        .arg("STREAMS")
        .arg(stream)
        .arg(start_id);
    command
}

fn pending_cursor_numbers(value: &str) -> Result<(u64, u64), RedisStreamsError> {
    decode_entry_id(value.as_bytes()).map_err(|error| map_command_decode_error(&error))?;
    let (millis, sequence) = value
        .split_once('-')
        .ok_or_else(|| RedisStreamsError::protocol("the Redis pending cursor is malformed"))?;
    Ok((
        millis
            .parse()
            .map_err(|_| RedisStreamsError::protocol("the Redis pending cursor is malformed"))?,
        sequence
            .parse()
            .map_err(|_| RedisStreamsError::protocol("the Redis pending cursor is malformed"))?,
    ))
}

fn decode_owned_pending_response(
    response: Value,
    expected_stream: &str,
    max_count: u64,
    start_id: &str,
    limits: RedisCommandLimits,
) -> Result<RedisOwnedPendingPage, RedisStreamsError> {
    let mut previous = pending_cursor_numbers(start_id)?;
    let mut page = RedisOwnedPendingPage {
        next_start_id: "0-0".to_owned(),
        deliveries: Vec::new(),
    };
    if matches!(response, Value::Nil) {
        return Ok(page);
    }
    let streams = value_array(response, "the Redis pending response is malformed")?;
    if streams.len() > 1 {
        return Err(RedisStreamsError::protocol(
            "Redis returned more than one pending stream",
        ));
    }
    for stream in streams {
        let mut pair = value_array(stream, "the Redis pending stream is malformed")?;
        if pair.len() != 2 {
            return Err(RedisStreamsError::protocol(
                "the Redis pending stream is malformed",
            ));
        }
        let entries = value_array(pair.remove(1), "the Redis pending entries are malformed")?;
        if value_text(pair.remove(0), "the Redis pending stream is malformed")?
            != expected_stream.as_bytes()
        {
            return Err(RedisStreamsError::protocol(
                "Redis returned another pending stream",
            ));
        }
        if u64::try_from(entries.len()).unwrap_or(u64::MAX) > max_count {
            return Err(RedisStreamsError::resource_exhausted(
                "the Redis pending response exceeds its batch bound",
            ));
        }
        let full_page = u64::try_from(entries.len()).unwrap_or(u64::MAX) == max_count;
        for entry in entries {
            let mut fields = value_array(entry, "the Redis pending entry is malformed")?;
            if fields.len() != 2 {
                return Err(RedisStreamsError::protocol(
                    "the Redis pending entry is malformed",
                ));
            }
            let payload = fields.remove(1);
            let id_bytes = value_text(
                fields.remove(0),
                "the Redis pending entry identity is malformed",
            )?;
            let id =
                decode_entry_id(&id_bytes).map_err(|error| map_command_decode_error(&error))?;
            let numbers = pending_cursor_numbers(&id)?;
            if numbers <= previous {
                return Err(RedisStreamsError::protocol(
                    "the Redis pending cursor did not advance",
                ));
            }
            previous = numbers;
            if full_page {
                page.next_start_id.clone_from(&id);
            }
            // Deleted stream payloads remain PEL tombstones. Advance the scan,
            // but never fabricate a command or acknowledge the missing payload.
            if !matches!(payload, Value::Nil) {
                page.deliveries.push(decode_delivery(
                    Value::Array(vec![Value::BulkString(id_bytes), payload]),
                    expected_stream,
                    limits,
                )?);
            }
        }
    }
    Ok(page)
}

fn decode_reclaim_response(
    response: Value,
    expected_stream: &str,
    max_count: u64,
    limits: RedisCommandLimits,
) -> Result<RedisReclaimPage, RedisStreamsError> {
    let mut parts = value_array(response, "the Redis reclaim response is malformed")?;
    if !(2..=3).contains(&parts.len()) {
        return Err(RedisStreamsError::protocol(
            "the Redis reclaim response is malformed",
        ));
    }
    let deleted = if parts.len() == 3 {
        Some(parts.pop().ok_or_else(|| {
            RedisStreamsError::protocol("the Redis reclaim response is malformed")
        })?)
    } else {
        None
    };
    let entries = value_array(
        parts.pop().ok_or_else(|| {
            RedisStreamsError::protocol("the Redis reclaim response is malformed")
        })?,
        "the Redis reclaimed entries are malformed",
    )?;
    let cursor = value_text(
        parts.pop().ok_or_else(|| {
            RedisStreamsError::protocol("the Redis reclaim response is malformed")
        })?,
        "the Redis reclaim cursor is malformed",
    )?;
    let next_start_id =
        decode_entry_id(&cursor).map_err(|error| map_command_decode_error(&error))?;
    if u64::try_from(entries.len()).unwrap_or(u64::MAX) > max_count {
        return Err(RedisStreamsError::resource_exhausted(
            "the Redis reclaim response exceeds its batch bound",
        ));
    }
    if let Some(deleted) = deleted {
        let deleted = value_array(deleted, "the Redis deleted-entry response is malformed")?;
        if u64::try_from(deleted.len()).unwrap_or(u64::MAX) > max_count {
            return Err(RedisStreamsError::resource_exhausted(
                "the Redis deleted-entry response exceeds its batch bound",
            ));
        }
        for entry_id in deleted {
            let entry_id = value_text(entry_id, "a Redis deleted-entry identity is malformed")?;
            decode_entry_id(&entry_id).map_err(|error| map_command_decode_error(&error))?;
        }
    }
    let deliveries = entries
        .into_iter()
        .map(|entry| decode_delivery(entry, expected_stream, limits))
        .collect::<Result<Vec<_>, _>>()?;
    Ok(RedisReclaimPage {
        next_start_id,
        deliveries,
    })
}

fn decode_delivery(
    entry: Value,
    stream: &str,
    limits: RedisCommandLimits,
) -> Result<RedisCommandDelivery, RedisStreamsError> {
    let mut pair = value_array(entry, "the Redis command entry is malformed")?;
    if pair.len() != 2 {
        return Err(RedisStreamsError::protocol(
            "the Redis command entry is malformed",
        ));
    }
    let fields = value_array(
        pair.pop()
            .ok_or_else(|| RedisStreamsError::protocol("the Redis command entry is malformed"))?,
        "the Redis command field list is malformed",
    )?;
    let entry_id = value_text(
        pair.pop()
            .ok_or_else(|| RedisStreamsError::protocol("the Redis command entry is malformed"))?,
        "the Redis command entry identity is malformed",
    )?;
    if fields.len() % 2 != 0 {
        return Err(RedisStreamsError::protocol(
            "the Redis command field list is malformed",
        ));
    }
    let mut fields = fields.into_iter();
    let mut decoded = Vec::new();
    while let Some(name) = fields.next() {
        let value = fields.next().ok_or_else(|| {
            RedisStreamsError::protocol("the Redis command field list is malformed")
        })?;
        decoded.push((
            value_text(name, "a Redis command field name is malformed")?,
            value_binary(value)?,
        ));
    }
    RedisCommandDelivery::decode(stream.as_bytes(), &entry_id, decoded, limits)
        .map_err(|error| map_command_decode_error(&error))
}

fn decode_heartbeat_response(
    response: Value,
    requested: &[String],
) -> Result<Vec<String>, RedisStreamsError> {
    let values = value_array(response, "the Redis heartbeat response is malformed")?;
    if values.len() > requested.len() {
        return Err(RedisStreamsError::resource_exhausted(
            "the Redis heartbeat response exceeds its batch bound",
        ));
    }
    let requested = requested
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    let mut seen = BTreeSet::new();
    let mut refreshed = Vec::with_capacity(values.len());
    for value in values {
        let value = value_text(value, "a Redis heartbeat entry identity is malformed")?;
        let value = decode_entry_id(&value).map_err(|error| map_command_decode_error(&error))?;
        if !requested.contains(value.as_str()) || !seen.insert(value.clone()) {
            return Err(RedisStreamsError::protocol(
                "the Redis heartbeat response is not an owned subset",
            ));
        }
        refreshed.push(value);
    }
    Ok(refreshed)
}

fn decode_retirement_response(
    response: Value,
) -> Result<RedisRetirementResponse, RedisRetirementClientError> {
    let Value::Array(values) = response else {
        return Err(RedisRetirementClientError::Protocol);
    };
    let [
        Value::Int(acknowledged),
        Value::Int(deleted),
        Value::Int(unmapped),
    ] = values.as_slice()
    else {
        return Err(RedisRetirementClientError::Protocol);
    };
    Ok(RedisRetirementResponse {
        acknowledged: *acknowledged,
        deleted: *deleted,
        unmapped: *unmapped,
    })
}

fn value_array(value: Value, message: &'static str) -> Result<Vec<Value>, RedisStreamsError> {
    match value {
        Value::Array(values) => Ok(values),
        _ => Err(RedisStreamsError::protocol(message)),
    }
}

fn value_text(value: Value, message: &'static str) -> Result<Vec<u8>, RedisStreamsError> {
    match value {
        Value::BulkString(value) => Ok(value),
        Value::SimpleString(value) => Ok(value.into_bytes()),
        _ => Err(RedisStreamsError::protocol(message)),
    }
}

fn value_binary(value: Value) -> Result<Vec<u8>, RedisStreamsError> {
    match value {
        Value::BulkString(value) => Ok(value),
        _ => Err(RedisStreamsError::protocol(
            "a Redis command field is not binary-safe",
        )),
    }
}

fn is_pong(value: &Value) -> bool {
    match value {
        Value::BulkString(value) => value.as_slice() == b"PONG",
        Value::SimpleString(value) => value.as_bytes() == b"PONG",
        Value::Okay => true,
        _ => false,
    }
}

fn valid_pem_input(value: &[u8]) -> bool {
    !value.is_empty()
        && value.len() <= MAX_PEM_BYTES
        && !value.contains(&0)
        && std::str::from_utf8(value).is_ok()
}

fn bounded_operation_timeout(value: Duration) -> bool {
    (MIN_OPERATION_TIMEOUT..=MAX_OPERATION_TIMEOUT).contains(&value)
        && value.subsec_nanos().is_multiple_of(1_000_000)
}

fn valid_read_request(config: &ValidatedRedisConfig, count: u64, block_millis: u64) -> bool {
    (1..=config.read_count).contains(&count) && (1..=config.block_millis).contains(&block_millis)
}

fn bounded_ascii_identity(value: &str, max_bytes: usize) -> bool {
    !value.is_empty()
        && value.len() <= max_bytes
        && value.is_ascii()
        && !value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
}

fn map_command_decode_error(error: &RedisCommandError) -> RedisStreamsError {
    match error {
        RedisCommandError::ResourceExhausted(_) => RedisStreamsError::resource_exhausted(
            "the Redis command response exceeds its configured byte bound",
        ),
        _ => RedisStreamsError::protocol("the Redis command response is malformed"),
    }
}

fn map_retirement_error(error: &RedisStreamsError) -> RedisRetirementClientError {
    match error.kind() {
        RedisStreamsErrorKind::Authentication => RedisRetirementClientError::Authentication,
        RedisStreamsErrorKind::Timeout => RedisRetirementClientError::Timeout,
        RedisStreamsErrorKind::ConsumerGroupMissing
        | RedisStreamsErrorKind::Protocol
        | RedisStreamsErrorKind::Configuration => RedisRetirementClientError::Protocol,
        RedisStreamsErrorKind::DependencyUnavailable
        | RedisStreamsErrorKind::ResourceExhausted
        | RedisStreamsErrorKind::Closed => RedisRetirementClientError::DependencyUnavailable,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct DropSignal(Arc<AtomicBool>);

    impl Drop for DropSignal {
        fn drop(&mut self) {
            self.0.store(true, Ordering::Release);
        }
    }

    fn config(url: &str) -> RedisStreamsConfig {
        RedisStreamsConfig {
            url: url.to_owned(),
            stream: "elitea:runtime:commands:v1".to_owned(),
            group: "elitea-workers-v1".to_owned(),
            consumer: "worker-a".to_owned(),
            command_limits: RedisCommandLimits {
                max_entry_bytes: 64 * 1024,
                max_field_bytes: 48 * 1024,
            },
            read_count: 8,
            block_millis: 1_000,
            reclaim_idle_millis: 60_000,
            connection_timeout: Duration::from_secs(5),
            control_timeout: Duration::from_secs(5),
            control_concurrency: 8,
        }
    }

    fn bytes(value: &'static [u8]) -> Value {
        Value::BulkString(value.to_vec())
    }

    fn command_entry(entry_id: &'static [u8], envelope: &'static [u8]) -> Value {
        Value::Array(vec![
            bytes(entry_id),
            Value::Array(vec![bytes(b"signed_envelope"), bytes(envelope)]),
        ])
    }

    #[test]
    fn canonical_endpoint_requires_rediss_acl_explicit_port_and_database_zero() {
        let accepted = [
            "rediss://worker_a@redis.internal:6380/0",
            "rediss://worker-a@[2001:db8::1]:6380/0",
        ];
        for url in accepted {
            config(url).validate().expect("canonical URL should pass");
        }
        let rejected = [
            "redis://worker@redis.internal:6380/0",
            "rediss://redis.internal:6380/0",
            "rediss://worker:password@redis.internal:6380/0",
            "rediss://worker@redis.internal/0",
            "rediss://worker@redis.internal:06380/0",
            "rediss://worker@redis.internal:6380/1",
            "rediss://worker@redis.internal:6380/0?x=1",
            "rediss://worker%20a@redis.internal:6380/0",
            "rediss://worker@redis.internal/path:6380/0",
            "rediss://worker@2001:db8::1:6380/0",
            "rediss://worker@[redis.internal]:6380/0",
        ];
        for url in rejected {
            assert!(config(url).validate().is_err(), "accepted {url}");
        }
    }

    #[test]
    fn configuration_bounds_match_the_runtime_v1_profile() {
        let mut value = config("rediss://worker@redis.internal:6380/0");
        value.read_count = 64;
        value.block_millis = 30_000;
        value.reclaim_idle_millis = 86_400_000;
        value.control_concurrency = 132;
        value.validate().expect("upper bounds should pass");

        value.read_count = 65;
        assert!(value.validate().is_err());
        value.read_count = 1;
        value.block_millis = 99;
        assert!(value.validate().is_err());
        value.block_millis = 100;
        value.reclaim_idle_millis = 59_999;
        assert!(value.validate().is_err());
        value.reclaim_idle_millis = 60_000;
        value.command_limits.max_entry_bytes -= 1;
        assert!(value.validate().is_err());

        let mut value = config("rediss://worker@redis.internal:6380/0");
        value.connection_timeout = Duration::from_millis(1);
        value.control_timeout = Duration::from_mins(5);
        value.validate().expect("bounded timeout edges should pass");
        value.connection_timeout = Duration::from_nanos(999_999);
        assert!(value.validate().is_err());
        value.connection_timeout = Duration::from_millis(1);
        value.control_timeout = Duration::from_mins(5) + Duration::from_millis(1);
        assert!(value.validate().is_err());
        value.control_timeout = Duration::from_micros(1_500);
        assert!(value.validate().is_err());
    }

    #[test]
    fn exact_missing_group_code_is_safe_and_nonretryable() {
        let upstream = redis::make_extension_error(
            "NOGROUP".to_owned(),
            Some("sensitive stream identity and server detail".to_owned()),
        );
        let mapped = RedisStreamsError::from_redis(upstream, "Redis consumer group check failed");
        assert_eq!(mapped.kind(), RedisStreamsErrorKind::ConsumerGroupMissing);
        assert_eq!(mapped.code(), "redis_streams.consumer_group_missing");
        assert_eq!(mapped.upstream_kind(), Some(&RedisErrorKind::Extension));
        assert!(!mapped.retryable());
        assert!(!format!("{mapped:?}").contains("sensitive"));
        assert!(!mapped.to_string().contains("sensitive"));
        assert_eq!(
            map_retirement_error(&mapped),
            RedisRetirementClientError::Protocol
        );
    }

    #[test]
    fn other_extensions_and_detail_substrings_remain_protocol_failures() {
        for code in ["nogroup", "NOGROUPX", "WRONGTYPE", "BUSYGROUP"] {
            let upstream = redis::make_extension_error(
                code.to_owned(),
                Some("NOGROUP sensitive detail".to_owned()),
            );
            let mapped = RedisStreamsError::from_redis(upstream, "Redis group check failed");
            assert_eq!(mapped.kind(), RedisStreamsErrorKind::Protocol);
            assert!(!mapped.retryable());
        }
        let upstream = RedisError::from((RedisErrorKind::Extension, "NOGROUP sensitive detail"));
        let mapped = RedisStreamsError::from_redis(upstream, "Redis group check failed");
        assert_eq!(mapped.kind(), RedisStreamsErrorKind::Protocol);
        assert!(!mapped.retryable());
    }

    fn pending_group_row() -> Value {
        Value::Array(vec![
            Value::BulkString(b"12-3".to_vec()),
            Value::BulkString(b"other-worker".to_vec()),
            Value::Int(60000),
            Value::Int(2),
        ])
    }

    #[test]
    fn group_check_accepts_existing_empty_or_pending_group_without_deliveries() {
        decode_group_check_response(Value::Array(Vec::new())).expect("existing empty group");
        decode_group_check_response(Value::Array(vec![pending_group_row()]))
            .expect("existing pending group");
    }

    #[test]
    fn group_check_refuses_extra_rows_and_malformed_pending_metadata() {
        let extra = decode_group_check_response(Value::Array(vec![
            pending_group_row(),
            pending_group_row(),
        ]))
        .expect_err("at most one pending row");
        assert_eq!(extra.kind(), RedisStreamsErrorKind::ResourceExhausted);
        assert!(!extra.retryable());

        let mut malformed = vec![
            Value::Nil,
            Value::BulkString(b"not an array".to_vec()),
            Value::Array(vec![Value::Array(Vec::new())]),
        ];
        for (index, value) in [
            (0, Value::BulkString(b"invalid-id".to_vec())),
            (1, Value::BulkString(b"invalid\nconsumer".to_vec())),
            (1, Value::BulkString(vec![b'x'; MAX_IDENTITY_BYTES + 1])),
            (2, Value::Int(-1)),
            (3, Value::Int(-1)),
            (3, Value::BulkString(b"2".to_vec())),
        ] {
            let Value::Array(mut row) = pending_group_row() else {
                panic!("pending fixture row");
            };
            row[index] = value;
            malformed.push(Value::Array(vec![Value::Array(row)]));
        }
        for response in malformed {
            let error =
                decode_group_check_response(response).expect_err("malformed pending response");
            assert_eq!(error.kind(), RedisStreamsErrorKind::Protocol);
            assert!(!error.retryable());
        }
    }

    #[test]
    fn transient_server_states_are_retryable_without_retaining_server_text() {
        let transient = [
            redis::ServerErrorKind::BusyLoading,
            redis::ServerErrorKind::TryAgain,
            redis::ServerErrorKind::ClusterDown,
            redis::ServerErrorKind::MasterDown,
            redis::ServerErrorKind::ReadOnly,
        ];
        for kind in transient {
            let error = RedisError::from((RedisErrorKind::Server(kind), "sensitive server text"));
            let mapped = RedisStreamsError::from_redis(error, "Redis command failed");
            assert_eq!(mapped.kind(), RedisStreamsErrorKind::DependencyUnavailable);
            assert!(mapped.retryable());
            assert!(!format!("{mapped:?}").contains("sensitive"));
        }

        let error = RedisError::from((
            RedisErrorKind::Server(redis::ServerErrorKind::ResponseError),
            "sensitive server text",
        ));
        let mapped = RedisStreamsError::from_redis(error, "Redis command failed");
        assert_eq!(mapped.kind(), RedisStreamsErrorKind::Protocol);
        assert!(!mapped.retryable());
    }

    #[test]
    fn tls_handshake_transport_loss_is_distinct_from_identity_rejection() {
        let unavailable = map_tls_handshake_error(
            io::Error::new(io::ErrorKind::ConnectionReset, "sensitive peer detail"),
            "the Redis TLS handshake failed",
        );
        assert_eq!(
            unavailable.kind(),
            RedisStreamsErrorKind::DependencyUnavailable
        );
        assert!(unavailable.retryable());
        assert!(!format!("{unavailable:?}").contains("sensitive"));

        let timeout = map_tls_handshake_error(
            io::Error::new(io::ErrorKind::TimedOut, "sensitive peer detail"),
            "the Redis TLS handshake failed",
        );
        assert_eq!(timeout.kind(), RedisStreamsErrorKind::Timeout);
        assert!(timeout.retryable());

        let authentication = map_tls_handshake_error(
            io::Error::new(io::ErrorKind::InvalidData, "sensitive certificate detail"),
            "the Redis TLS handshake failed",
        );
        assert_eq!(authentication.kind(), RedisStreamsErrorKind::Authentication);
        assert!(!authentication.retryable());
        assert!(!format!("{authentication:?}").contains("sensitive"));
    }

    #[test]
    fn one_millisecond_fair_read_is_allowed_below_the_deployment_default() {
        let config = config("rediss://worker@redis.internal:6380/0")
            .validate()
            .expect("valid deployment configuration");
        assert!(valid_read_request(&config, 1, 1));
        assert!(valid_read_request(&config, 8, 1_000));
        assert!(!valid_read_request(&config, 0, 1));
        assert!(!valid_read_request(&config, 1, 0));
        assert!(!valid_read_request(&config, 9, 1));
        assert!(!valid_read_request(&config, 1, 1_001));
    }

    #[tokio::test]
    async fn caller_cancellation_keeps_capacity_and_latches_a_later_timeout() {
        let capacity = Arc::new(Semaphore::new(1));
        let failed = Arc::new(AtomicBool::new(false));
        let permit = capacity
            .clone()
            .acquire_owned()
            .await
            .expect("capacity should be open");
        let (started_sender, started_receiver) = tokio::sync::oneshot::channel();
        let (release_sender, release_receiver) = tokio::sync::oneshot::channel();
        let waiter = tokio::spawn(run_owned_operation(permit, failed.clone(), async move {
            let _ = started_sender.send(());
            let _ = release_receiver.await;
            Err::<(), _>(RedisStreamsError::timeout("test Redis operation timed out"))
        }));
        started_receiver.await.expect("operation should start");

        waiter.abort();
        let _ = waiter.await;
        assert_eq!(capacity.available_permits(), 0);
        assert!(!failed.load(Ordering::Acquire));

        let _ = release_sender.send(());
        for _ in 0..100 {
            if capacity.available_permits() == 1 && failed.load(Ordering::Acquire) {
                break;
            }
            tokio::task::yield_now().await;
        }
        assert_eq!(capacity.available_permits(), 1);
        assert!(failed.load(Ordering::Acquire));
    }

    #[tokio::test]
    async fn dropping_a_driver_owner_aborts_the_custom_redis_connection_task() {
        let dropped = Arc::new(AtomicBool::new(false));
        let (started_sender, started_receiver) = tokio::sync::oneshot::channel();
        let task_dropped = dropped.clone();
        let task = tokio::spawn(async move {
            let _drop_signal = DropSignal(task_dropped);
            let _ = started_sender.send(());
            std::future::pending::<()>().await;
        });
        started_receiver.await.expect("driver should start");
        drop(DriverTask::new(task));

        for _ in 0..100 {
            if dropped.load(Ordering::Acquire) {
                break;
            }
            tokio::task::yield_now().await;
        }
        assert!(dropped.load(Ordering::Acquire));
    }

    fn pending_response(stream: &'static [u8], entries: Vec<Value>) -> Value {
        Value::Array(vec![Value::Array(vec![
            bytes(stream),
            Value::Array(entries),
        ])])
    }

    fn pending_limits() -> RedisCommandLimits {
        RedisCommandLimits {
            max_entry_bytes: 1024,
            max_field_bytes: 512,
        }
    }

    #[test]
    fn owned_pending_wire_selects_only_the_configured_consumer_without_claim_or_ack() {
        let packed = owned_pending_read_command("commands", "group", "worker-a", 2, "9-0")
            .get_packed_command();
        assert_eq!(packed, b"*9\r\n$10\r\nXREADGROUP\r\n$5\r\nGROUP\r\n$5\r\ngroup\r\n$8\r\nworker-a\r\n$5\r\nCOUNT\r\n$1\r\n2\r\n$7\r\nSTREAMS\r\n$8\r\ncommands\r\n$3\r\n9-0\r\n");
    }

    #[test]
    fn owned_pending_pages_use_numeric_cursor_order_and_preserve_signed_bytes() {
        let page = decode_owned_pending_response(
            pending_response(
                b"commands",
                vec![
                    command_entry(b"10-0", b"\x00\xffsigned"),
                    command_entry(b"11-0", b"second"),
                ],
            ),
            "commands",
            2,
            "9-9",
            pending_limits(),
        )
        .expect("bounded pending page");
        assert_eq!(page.next_start_id, "11-0");
        assert_eq!(page.deliveries.len(), 2);
        assert_eq!(page.deliveries[0].signed_envelope(), b"\x00\xffsigned");
    }

    #[test]
    fn owned_pending_short_and_empty_pages_restart_the_scan() {
        for response in [
            Value::Nil,
            pending_response(b"commands", vec![]),
            pending_response(b"commands", vec![command_entry(b"10-0", b"signed")]),
        ] {
            let page =
                decode_owned_pending_response(response, "commands", 2, "9-0", pending_limits())
                    .expect("end of scan");
            assert_eq!(page.next_start_id, "0-0");
        }
    }

    #[test]
    fn owned_pending_deleted_payload_advances_without_creating_a_delivery() {
        let page = decode_owned_pending_response(
            pending_response(
                b"commands",
                vec![Value::Array(vec![bytes(b"10-0"), Value::Nil])],
            ),
            "commands",
            1,
            "9-0",
            pending_limits(),
        )
        .expect("PEL tombstone");
        assert_eq!(page.next_start_id, "10-0");
        assert!(page.deliveries.is_empty());
    }

    #[test]
    fn owned_pending_rejects_wrong_stream_cursor_order_bounds_and_malformed_payloads() {
        let cases = [
            pending_response(b"other", vec![command_entry(b"10-0", b"signed")]),
            pending_response(b"commands", vec![command_entry(b"9-0", b"signed")]),
            pending_response(
                b"commands",
                vec![
                    command_entry(b"10-0", b"signed"),
                    command_entry(b"10-0", b"duplicate"),
                ],
            ),
            pending_response(
                b"commands",
                vec![
                    command_entry(b"11-0", b"signed"),
                    command_entry(b"10-0", b"backwards"),
                ],
            ),
            pending_response(b"commands", vec![command_entry(b"bad", b"signed")]),
            pending_response(b"commands", vec![Value::Array(vec![bytes(b"10-0")])]),
            pending_response(
                b"commands",
                vec![Value::Array(vec![bytes(b"10-0"), Value::Array(vec![])])],
            ),
            pending_response(
                b"commands",
                vec![Value::Array(vec![
                    bytes(b"10-0"),
                    Value::Array(vec![bytes(b"signed_envelope")]),
                ])],
            ),
        ];
        for response in cases {
            assert!(
                decode_owned_pending_response(response, "commands", 2, "9-0", pending_limits())
                    .is_err()
            );
        }
        assert!(
            decode_owned_pending_response(
                pending_response(
                    b"commands",
                    vec![command_entry(b"10-0", b"a"), command_entry(b"11-0", b"b")]
                ),
                "commands",
                1,
                "9-0",
                pending_limits()
            )
            .is_err()
        );
        assert!(
            decode_owned_pending_response(Value::Nil, "commands", 1, "bad", pending_limits())
                .is_err()
        );
    }

    #[test]
    fn read_response_preserves_binary_envelope_and_rejects_duplicate_fields() {
        let response = Value::Array(vec![Value::Array(vec![
            bytes(b"elitea:runtime:commands:v1"),
            Value::Array(vec![command_entry(b"1-0", b"\x00\xffsigned")]),
        ])]);
        let deliveries = decode_read_response(
            response,
            "elitea:runtime:commands:v1",
            1,
            RedisCommandLimits {
                max_entry_bytes: 1024,
                max_field_bytes: 512,
            },
        )
        .expect("valid read response");
        assert_eq!(deliveries[0].signed_envelope(), b"\x00\xffsigned");

        let duplicate = Value::Array(vec![Value::Array(vec![
            bytes(b"elitea:runtime:commands:v1"),
            Value::Array(vec![Value::Array(vec![
                bytes(b"1-0"),
                Value::Array(vec![
                    bytes(b"signed_envelope"),
                    bytes(b"a"),
                    bytes(b"signed_envelope"),
                    bytes(b"b"),
                ]),
            ])]),
        ])]);
        assert!(
            decode_read_response(
                duplicate,
                "elitea:runtime:commands:v1",
                1,
                RedisCommandLimits {
                    max_entry_bytes: 1024,
                    max_field_bytes: 512,
                },
            )
            .is_err()
        );
    }

    #[test]
    fn read_and_reclaim_responses_are_batch_bounded() {
        let response = Value::Array(vec![Value::Array(vec![
            bytes(b"elitea:runtime:commands:v1"),
            Value::Array(vec![
                command_entry(b"1-0", b"a"),
                command_entry(b"2-0", b"b"),
            ]),
        ])]);
        let Err(error) = decode_read_response(
            response,
            "elitea:runtime:commands:v1",
            1,
            RedisCommandLimits {
                max_entry_bytes: 1024,
                max_field_bytes: 512,
            },
        ) else {
            panic!("response must be bounded");
        };
        assert_eq!(error.kind(), RedisStreamsErrorKind::ResourceExhausted);
        assert!(!error.retryable());

        let reclaim = Value::Array(vec![
            bytes(b"3-0"),
            Value::Array(vec![command_entry(b"1-0", b"a")]),
            Value::Array(vec![bytes(b"9-0"), bytes(b"10-0")]),
        ]);
        assert!(
            decode_reclaim_response(
                reclaim,
                "elitea:runtime:commands:v1",
                1,
                RedisCommandLimits {
                    max_entry_bytes: 1024,
                    max_field_bytes: 512,
                },
            )
            .is_err()
        );
    }

    #[test]
    fn reclaim_accepts_redis_seven_deleted_id_shape() {
        let response = Value::Array(vec![
            bytes(b"3-0"),
            Value::Array(vec![command_entry(b"2-0", b"signed")]),
            Value::Array(vec![bytes(b"1-0")]),
        ]);
        let page = decode_reclaim_response(
            response,
            "elitea:runtime:commands:v1",
            2,
            RedisCommandLimits {
                max_entry_bytes: 1024,
                max_field_bytes: 512,
            },
        )
        .expect("valid Redis 7 reclaim response");
        assert_eq!(page.next_start_id, "3-0");
        assert_eq!(page.deliveries.len(), 1);
        assert_eq!(page.deliveries[0].entry_id(), "2-0");
    }

    #[test]
    fn heartbeat_response_must_be_a_unique_requested_subset() {
        let requested = vec!["1-0".to_owned(), "2-0".to_owned()];
        assert_eq!(
            decode_heartbeat_response(Value::Array(vec![bytes(b"2-0")]), &requested)
                .expect("owned subset"),
            vec!["2-0"]
        );
        assert!(decode_heartbeat_response(
            Value::Array(vec![bytes(b"1-0"), bytes(b"1-0")]),
            &requested,
        )
        .is_err());
        assert!(decode_heartbeat_response(Value::Array(vec![bytes(b"3-0")]), &requested).is_err());
    }

    #[test]
    fn retirement_response_requires_exact_integer_triple() {
        let first = decode_retirement_response(Value::Array(vec![
            Value::Int(1),
            Value::Int(1),
            Value::Int(1),
        ]))
        .expect("first retirement");
        assert_eq!(
            (first.acknowledged, first.deleted, first.unmapped),
            (1, 1, 1)
        );
        assert!(
            decode_retirement_response(Value::Array(vec![
                Value::Int(1),
                bytes(b"1"),
                Value::Int(1),
            ]))
            .is_err()
        );
    }

    #[test]
    fn secret_inputs_are_bounded_and_redacted() {
        let Err(error) = RedisTlsMaterial::new(
            "secret\nline".to_owned(),
            b"ca".to_vec(),
            b"cert".to_vec(),
            b"key".to_vec(),
        ) else {
            panic!("control-bearing password must fail");
        };
        assert_eq!(error.code(), "redis_streams.configuration");
        assert!(!format!("{error:?}").contains("secret"));

        let material = RedisTlsMaterial::new(
            "secret".to_owned(),
            b"not a certificate".to_vec(),
            b"not a certificate".to_vec(),
            b"not a private key".to_vec(),
        )
        .expect("bounded source material");
        let error = build_tls_config(&material).expect_err("invalid PEM must fail closed");
        assert_eq!(error.code(), "redis_streams.configuration");
        assert!(!format!("{error:?}").contains("certificate"));
    }
}
