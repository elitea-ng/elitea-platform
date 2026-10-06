//! NATS `JetStream` pull-consumer transport for signed worker commands.
//!
//! `docs/runtime-command-bus.md` is the contract. The worker is a consumer
//! only and binds to what the bootstrap created: it reads the durable
//! (`CONSUMER.INFO`), refuses to start when it is absent or drifted, and then
//! uses exactly the operations its permission row grants: pull (`MSG.NEXT`),
//! `+WPI`, nak with a delay, `+TERM` for a command that can never verify, a
//! double ack after the settlement receipt, and a dead-letter KV put. It never
//! publishes a command, creates a consumer or administers a stream.
//!
//! With a client identity (the chart's posture) the worker is the only user of
//! the WORKER account, which holds its dead-letter bucket and nothing else, and
//! reaches RUNTIME's durables through service imports under
//! [`RUNTIME_API_PREFIX`]: the command-bus `JetStream` context uses that prefix,
//! the dead-letter bucket the default `$JS.API` of WORKER's own `JetStream`.
//! A request's reply subject is not checked against permissions and the server
//! answers `JetStream` API requests onto it, so in RUNTIME a reply naming a
//! command subject stored the answer in a command stream; from WORKER it lands
//! in WORKER. Without an identity (compose, one global account) both contexts
//! use the default prefix.
//!
//! async-nats reconnects internally. The transport therefore never assumes
//! that anything sent across a disconnect arrived: retirement and naks are
//! request/reply (the server answers the ack subject), and a heartbeat round
//! is confirmed by a flush on the same connection epoch. Every connection
//! event is logged.
//!
//! The mTLS client identity and the private CA are read from their files on
//! every TLS handshake (a reconnect included), so a rotated certificate is
//! picked up without a restart.

use std::fmt;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use async_nats::jetstream::consumer::{AckPolicy, DeliverPolicy, PullConsumer};
use async_nats::jetstream::{self, kv};
use async_nats::{Client, ConnectOptions, Event};
use async_trait::async_trait;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::client::{ResolvesClientCert, WebPkiServerVerifier};
use rustls::crypto::CryptoProvider;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName, UnixTime, pem::PemObject as _};
use rustls::sign::CertifiedKey;
use rustls::{ClientConfig, DigitallySignedStruct, RootCertStore, SignatureScheme};
use tokio_stream::StreamExt as _;
use zeroize::Zeroizing;

use super::command_bus::{
    AckReplyInfo, CommandBusLimits, CommandRetirementClient, CommandRetirementClientError,
    CommandRetirementRequest, DEAD_LETTER_BUCKET, DeadLetterRecord, DecodedDelivery,
    MAX_REQUEST_BATCH, MAX_REQUEST_EXPIRES, RUNTIME_API_PREFIX, WORKER_INBOX_PREFIX,
    decode_delivery, filter_subject, valid_route_pair,
};
use crate::security::{MAX_CA_BYTES, MAX_CERTIFICATE_BYTES, MAX_PRIVATE_KEY_BYTES};

const MAX_URL_BYTES: usize = 2_048;
const MAX_SERVERS: usize = 16;
const MAX_CLIENT_NAME_BYTES: usize = 256;
const MIN_FETCH_EXPIRES: Duration = Duration::from_millis(100);
const MIN_OPERATION_TIMEOUT: Duration = Duration::from_millis(1);
const MAX_OPERATION_TIMEOUT: Duration = Duration::from_mins(5);

/// The mTLS material of the `elitea-worker` identity: all three or none.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NatsTlsPaths {
    pub ca_path: PathBuf,
    pub certificate_path: PathBuf,
    pub private_key_path: PathBuf,
}

/// Non-secret policy for one command-stream consumer.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NatsJetStreamConfig {
    /// `tls://host:port` (with `tls`) or `nats://host:port` (without), or a
    /// comma-separated seed list of one scheme. No user information.
    pub url: String,
    pub tls: Option<NatsTlsPaths>,
    pub stream: String,
    pub consumer: String,
    /// The NATS connection name (observability only).
    pub client_name: String,
    pub limits: CommandBusLimits,
    pub fetch_batch: usize,
    pub fetch_expires: Duration,
    pub connection_timeout: Duration,
    pub request_timeout: Duration,
}

struct ValidatedConfig {
    servers: Vec<String>,
    tls: Option<NatsTlsPaths>,
    stream: String,
    consumer: String,
    filter_subject: String,
    client_name: String,
    limits: CommandBusLimits,
    fetch_batch: usize,
    fetch_expires: Duration,
    connection_timeout: Duration,
    request_timeout: Duration,
}

impl NatsJetStreamConfig {
    fn validate(&self) -> Result<ValidatedConfig, NatsJetStreamError> {
        let (servers, tls_scheme) = parse_server_urls(&self.url)?;
        if tls_scheme != self.tls.is_some() {
            return Err(NatsJetStreamError::configuration(
                "the NATS URL scheme does not match the client TLS material",
            ));
        }
        if let Some(tls) = &self.tls
            && [&tls.ca_path, &tls.certificate_path, &tls.private_key_path]
                .iter()
                .any(|path| !path.is_absolute())
        {
            return Err(NatsJetStreamError::configuration(
                "the NATS TLS material paths are malformed",
            ));
        }
        if !valid_route_pair(&self.stream, &self.consumer) {
            return Err(NatsJetStreamError::configuration(
                "the NATS stream and consumer are not one contract route",
            ));
        }
        let filter_subject = filter_subject(&self.stream).ok_or_else(|| {
            NatsJetStreamError::configuration("the NATS stream is not a contract stream")
        })?;
        if self.client_name.is_empty()
            || self.client_name.len() > MAX_CLIENT_NAME_BYTES
            || !self
                .client_name
                .bytes()
                .all(|byte| (0x21..=0x7e).contains(&byte))
        {
            return Err(NatsJetStreamError::configuration(
                "the NATS connection name is malformed",
            ));
        }
        let limits = self.limits.validate().map_err(|_| {
            NatsJetStreamError::configuration("the NATS command limits are malformed")
        })?;
        if limits != CommandBusLimits::runtime_v1()
            || !(1..=MAX_REQUEST_BATCH).contains(&self.fetch_batch)
            || !(MIN_FETCH_EXPIRES..=MAX_REQUEST_EXPIRES).contains(&self.fetch_expires)
            || !bounded_operation_timeout(self.connection_timeout)
            || !bounded_operation_timeout(self.request_timeout)
        {
            return Err(NatsJetStreamError::configuration(
                "the NATS command transport limits are malformed",
            ));
        }
        Ok(ValidatedConfig {
            servers,
            tls: self.tls.clone(),
            stream: self.stream.clone(),
            consumer: self.consumer.clone(),
            filter_subject,
            client_name: self.client_name.clone(),
            limits,
            fetch_batch: self.fetch_batch,
            fetch_expires: self.fetch_expires,
            connection_timeout: self.connection_timeout,
            request_timeout: self.request_timeout,
        })
    }
}

/// Stable low-cardinality NATS transport failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NatsJetStreamErrorKind {
    Configuration,
    /// The stream or durable the deployment bootstrap owns does not exist.
    /// The worker never creates either; only nats-bootstrap can fix this.
    ConsumerMissing,
    Authentication,
    DependencyUnavailable,
    Timeout,
    Protocol,
    ResourceExhausted,
    Closed,
}

/// Data-free NATS transport failure suitable for operator logs and metrics.
/// Library error text (which may carry subjects or server text) is dropped.
#[derive(Clone, Copy)]
pub struct NatsJetStreamError {
    kind: NatsJetStreamErrorKind,
    message: &'static str,
}

impl NatsJetStreamError {
    pub(crate) const fn configuration(message: &'static str) -> Self {
        Self::new(NatsJetStreamErrorKind::Configuration, message)
    }

    pub(crate) const fn consumer_missing(message: &'static str) -> Self {
        Self::new(NatsJetStreamErrorKind::ConsumerMissing, message)
    }

    pub(crate) const fn protocol(message: &'static str) -> Self {
        Self::new(NatsJetStreamErrorKind::Protocol, message)
    }

    pub(crate) const fn authentication(message: &'static str) -> Self {
        Self::new(NatsJetStreamErrorKind::Authentication, message)
    }

    pub(crate) const fn unavailable(message: &'static str) -> Self {
        Self::new(NatsJetStreamErrorKind::DependencyUnavailable, message)
    }

    pub(crate) const fn timeout(message: &'static str) -> Self {
        Self::new(NatsJetStreamErrorKind::Timeout, message)
    }

    pub(crate) const fn resource_exhausted(message: &'static str) -> Self {
        Self::new(NatsJetStreamErrorKind::ResourceExhausted, message)
    }

    pub(crate) const fn closed() -> Self {
        Self::new(
            NatsJetStreamErrorKind::Closed,
            "the NATS command transport is closed",
        )
    }

    const fn new(kind: NatsJetStreamErrorKind, message: &'static str) -> Self {
        Self { kind, message }
    }

    /// Stable failure category for branching and metrics.
    #[must_use]
    pub const fn kind(&self) -> NatsJetStreamErrorKind {
        self.kind
    }

    /// Stable metric/log code that never contains server or credential data.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self.kind {
            NatsJetStreamErrorKind::Configuration => "nats_jetstream.configuration",
            NatsJetStreamErrorKind::ConsumerMissing => "nats_jetstream.consumer_missing",
            NatsJetStreamErrorKind::Authentication => "nats_jetstream.authentication",
            NatsJetStreamErrorKind::DependencyUnavailable => "nats_jetstream.unavailable",
            NatsJetStreamErrorKind::Timeout => "nats_jetstream.timeout",
            NatsJetStreamErrorKind::Protocol => "nats_jetstream.protocol",
            NatsJetStreamErrorKind::ResourceExhausted => "nats_jetstream.resource_exhausted",
            NatsJetStreamErrorKind::Closed => "nats_jetstream.closed",
        }
    }

    /// Whether an outer loop may retry a fresh operation later.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self.kind,
            NatsJetStreamErrorKind::DependencyUnavailable | NatsJetStreamErrorKind::Timeout
        )
    }
}

impl fmt::Debug for NatsJetStreamError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("NatsJetStreamError")
            .field("kind", &self.kind)
            .field("code", &self.code())
            .field("message", &self.message)
            .finish()
    }
}

impl fmt::Display for NatsJetStreamError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for NatsJetStreamError {}

/// The restricted operations the delivery runtime uses. Crate-private so a
/// test double cannot widen the production surface.
#[async_trait]
pub(crate) trait CommandBusConnection:
    CommandRetirementClient + Send + Sync + 'static
{
    /// Messages per pull (`limits.nats_fetch_batch`).
    fn fetch_batch(&self) -> usize;

    /// Pull at most `max` messages, waiting up to `expires` for the FIRST and
    /// returning as soon as one has arrived (with whatever else is available
    /// at that moment), never holding a delivered command until `expires`.
    async fn fetch(
        &self,
        max: usize,
        expires: Duration,
    ) -> Result<Vec<DecodedDelivery>, NatsJetStreamError>;

    /// Send `+WPI` for each owned delivery and confirm the round reached the
    /// server on one connection epoch. Returns how many were confirmed.
    async fn in_progress(&self, replies: &[String]) -> Result<usize, NatsJetStreamError>;

    /// Nak one delivery with a redelivery delay; confirmed by the server.
    async fn nak(&self, reply: &str, delay: Duration) -> Result<(), NatsJetStreamError>;

    /// Terminate one delivery (`+TERM`): it is never redelivered and leaves
    /// the `WorkQueue` stream. Only for a command that can never verify.
    async fn term(&self, reply: &str) -> Result<(), NatsJetStreamError>;

    /// Put one dead-letter record into the bucket.
    async fn record_dead_letter(&self, record: &DeadLetterRecord)
    -> Result<(), NatsJetStreamError>;

    /// The connection name, recorded as the dead letter's `worker`.
    fn worker_name(&self) -> &str;

    /// Drain and close the connection. Owned, unsettled deliveries are left
    /// to `AckWait`.
    async fn close(&self) -> Result<(), NatsJetStreamError>;
}

/// Connection events, observable and counted. Every `Disconnected` starts a
/// new epoch; a heartbeat round that spans an epoch change is unconfirmed.
#[derive(Debug, Default)]
pub(crate) struct ConnectionObserver {
    epoch: AtomicU64,
    disconnected: AtomicBool,
}

impl ConnectionObserver {
    fn observe(&self, event: &Event) {
        match event {
            Event::Connected => {
                let reconnect = self.disconnected.swap(false, Ordering::AcqRel);
                tracing::info!(
                    event = if reconnect {
                        "nats_reconnected"
                    } else {
                        "nats_connected"
                    },
                    connection_epoch = self.epoch.load(Ordering::Acquire),
                );
            }
            Event::Disconnected => {
                self.disconnected.store(true, Ordering::Release);
                let epoch = self.epoch.fetch_add(1, Ordering::AcqRel) + 1;
                tracing::warn!(event = "nats_disconnected", connection_epoch = epoch);
            }
            Event::LameDuckMode => tracing::warn!(event = "nats_lame_duck_mode"),
            Event::Draining => tracing::info!(event = "nats_draining"),
            Event::Closed => tracing::info!(event = "nats_closed"),
            Event::SlowConsumer(_) => tracing::warn!(event = "nats_slow_consumer"),
            Event::ServerError(_) => tracing::warn!(event = "nats_server_error"),
            Event::ClientError(_) => tracing::warn!(event = "nats_client_error"),
        }
    }

    fn epoch(&self) -> u64 {
        self.epoch.load(Ordering::Acquire)
    }

    fn is_disconnected(&self) -> bool {
        self.disconnected.load(Ordering::Acquire)
    }
}

/// Connected, bound command-stream consumer.
pub struct NatsCommandBus {
    client: Client,
    consumer: PullConsumer,
    dead_letters: kv::Store,
    config: ValidatedConfig,
    observer: Arc<ConnectionObserver>,
    closed: AtomicBool,
}

impl NatsCommandBus {
    /// Connect as the configured identity, bind to the bootstrap-created
    /// stream, durable and dead-letter bucket, and verify the durable is the
    /// contract's.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration, authentication, timeout or dependency
    /// error; drift or absence of the stream or durable is a configuration
    /// error (the worker refuses to start).
    pub async fn connect(config: NatsJetStreamConfig) -> Result<Self, NatsJetStreamError> {
        let config = config.validate()?;
        let observer = Arc::new(ConnectionObserver::default());
        let mut options = ConnectOptions::new()
            .name(config.client_name.clone())
            .custom_inbox_prefix(WORKER_INBOX_PREFIX)
            .connection_timeout(config.connection_timeout)
            .request_timeout(Some(config.request_timeout))
            .event_callback({
                let observer = Arc::clone(&observer);
                move |event| {
                    let observer = Arc::clone(&observer);
                    async move { observer.observe(&event) }
                }
            });
        if let Some(tls) = &config.tls {
            preflight_tls_material(tls)?;
            options = options
                .require_tls(true)
                .tls_client_config(reloading_tls_config(tls.clone())?);
        }
        let servers = config
            .servers
            .iter()
            .map(|server| server.parse::<async_nats::ServerAddr>())
            .collect::<Result<Vec<_>, _>>()
            .map_err(|_| NatsJetStreamError::configuration("the NATS URL is malformed"))?;
        let client = tokio::time::timeout(
            config.connection_timeout.saturating_mul(2),
            options.connect(servers),
        )
        .await
        .map_err(|_| NatsJetStreamError::timeout("the NATS connection timed out"))?
        .map_err(|error| map_connect_error(&error))?;
        match Self::bind(client.clone(), config, observer).await {
            Ok(bus) => Ok(bus),
            Err(error) => {
                let _ = client.drain().await;
                Err(error)
            }
        }
    }

    async fn bind(
        client: Client,
        config: ValidatedConfig,
        observer: Arc<ConnectionObserver>,
    ) -> Result<Self, NatsJetStreamError> {
        // The durable is RUNTIME's, reached from WORKER through the imported
        // API prefix when the worker presents its identity; the dead-letter
        // bucket is WORKER's own, on the default prefix.
        let mut command_bus = if config.tls.is_some() {
            jetstream::with_prefix(client.clone(), RUNTIME_API_PREFIX)
        } else {
            jetstream::new(client.clone())
        };
        command_bus.set_timeout(config.request_timeout);
        let mut context = jetstream::new(client.clone());
        context.set_timeout(config.request_timeout);
        let consumer: PullConsumer = command_bus
            .get_consumer_from_stream(&config.consumer, &config.stream)
            .await
            .map_err(|_| {
                NatsJetStreamError::consumer_missing("the NATS durable consumer is absent")
            })?;
        verify_consumer(
            &consumer.cached_info().config,
            &config.consumer,
            &config.filter_subject,
            config.fetch_batch,
            config.fetch_expires,
        )?;
        let dead_letters = context
            .get_key_value(DEAD_LETTER_BUCKET)
            .await
            .map_err(|_| {
                NatsJetStreamError::configuration("the NATS dead-letter bucket is absent")
            })?;
        tracing::info!(
            event = "nats_command_consumer_bound",
            nats_stream = %config.stream,
            nats_consumer = %config.consumer,
            fetch_batch = config.fetch_batch,
        );
        Ok(Self {
            client,
            consumer,
            dead_letters,
            config,
            observer,
            closed: AtomicBool::new(false),
        })
    }

    fn ensure_open(&self) -> Result<(), NatsJetStreamError> {
        if self.closed.load(Ordering::Acquire) {
            Err(NatsJetStreamError::closed())
        } else {
            Ok(())
        }
    }

    /// Only ever act on this durable's own ack subjects.
    fn owned_reply(&self, reply: &str) -> Result<(), NatsJetStreamError> {
        let info = AckReplyInfo::parse(reply).map_err(|_| {
            NatsJetStreamError::protocol("the delivery acknowledgement subject is malformed")
        })?;
        if info.stream != self.config.stream || info.consumer != self.config.consumer {
            return Err(NatsJetStreamError::protocol(
                "the delivery belongs to another stream or consumer",
            ));
        }
        Ok(())
    }

    /// One pull that ends on the server before it ends here: a long poll
    /// (`expires` set) or a no-wait pull (`expires` None), until `deliveries`
    /// holds `up_to`.
    async fn pull_into(
        &self,
        deliveries: &mut Vec<DecodedDelivery>,
        up_to: usize,
        expires: Option<Duration>,
    ) -> Result<(), NatsJetStreamError> {
        let wanted = up_to.saturating_sub(deliveries.len());
        if wanted == 0 {
            return Ok(());
        }
        let started = deliveries.len();
        let request = async {
            match expires {
                Some(expires) => {
                    self.consumer
                        .batch()
                        .max_messages(wanted)
                        .expires(expires)
                        .messages()
                        .await
                }
                None => self.consumer.fetch().max_messages(wanted).messages().await,
            }
        };
        let mut batch = tokio::time::timeout(self.config.request_timeout, request)
            .await
            .map_err(|_| NatsJetStreamError::timeout("the NATS pull request timed out"))?
            .map_err(|_| NatsJetStreamError::unavailable("the NATS pull request failed"))?;
        let deadline = tokio::time::Instant::now()
            + expires
                .unwrap_or_default()
                .saturating_add(self.config.request_timeout);
        let mut received = 0;
        loop {
            let next = match tokio::time::timeout_at(deadline, batch.next()).await {
                Err(_) | Ok(None) => break,
                Ok(Some(next)) => next,
            };
            let message = match next {
                Ok(message) => message,
                Err(_) if deliveries.len() == started => {
                    return Err(NatsJetStreamError::unavailable(
                        "the NATS pull ended with a server status",
                    ));
                }
                // Return what this pull already owns; the next pull reports
                // the status again if it persists.
                Err(_) => break,
            };
            received += 1;
            if received > wanted {
                return Err(NatsJetStreamError::resource_exhausted(
                    "the NATS pull returned more messages than requested",
                ));
            }
            let Some(reply) = message.message.reply.as_ref() else {
                tracing::error!(event = "nats_delivery_without_ack_subject");
                continue;
            };
            match decode_delivery(
                message.message.subject.as_str(),
                reply.as_str(),
                message.message.payload.to_vec(),
                message.message.length,
                self.config.limits,
            ) {
                Ok(decoded) => deliveries.push(decoded),
                // Only this message: an ack subject that is unusable or names
                // another stream or consumer cannot be answered by this
                // identity, so it is left to AckWait; the rest is kept.
                Err(error) => {
                    tracing::error!(event = "nats_delivery_unackable", error_code = error.code(),);
                }
            }
            if received == wanted {
                break;
            }
        }
        Ok(())
    }

    async fn request_ack(
        &self,
        reply: &str,
        payload: Vec<u8>,
        what: &'static str,
    ) -> Result<(), NatsJetStreamError> {
        self.ensure_open()?;
        self.owned_reply(reply)?;
        let response = tokio::time::timeout(
            self.config.request_timeout,
            self.client.request(reply.to_owned(), payload.into()),
        )
        .await
        .map_err(|_| NatsJetStreamError::timeout(what))?;
        match response {
            Ok(_) => Ok(()),
            Err(error) => Err(match error.kind() {
                async_nats::RequestErrorKind::TimedOut => NatsJetStreamError::timeout(what),
                async_nats::RequestErrorKind::InvalidSubject
                | async_nats::RequestErrorKind::MaxPayloadExceeded => {
                    NatsJetStreamError::protocol(what)
                }
                _ => NatsJetStreamError::unavailable(what),
            }),
        }
    }
}

#[async_trait]
impl CommandBusConnection for NatsCommandBus {
    fn fetch_batch(&self) -> usize {
        self.config.fetch_batch
    }

    async fn fetch(
        &self,
        max: usize,
        expires: Duration,
    ) -> Result<Vec<DecodedDelivery>, NatsJetStreamError> {
        self.ensure_open()?;
        if !(1..=self.config.fetch_batch).contains(&max)
            || !(MIN_FETCH_EXPIRES..=self.config.fetch_expires).contains(&expires)
        {
            return Err(NatsJetStreamError::configuration(
                "the NATS pull exceeds its configured bound",
            ));
        }
        // Two pulls, each COMPLETE on the server before it is abandoned here.
        //
        // A batch pull for `max` messages does not end until `max` arrived or
        // the server answers 408 at `expires`, so a single command used to
        // wait up to `expires` (5s deployed) in this function before anything
        // could start it. Dropping that pull early instead would leave the
        // server delivering the rest of the batch to an inbox nobody reads —
        // each such command then waits out AckWait. So: a long poll for ONE
        // message, which the server closes as soon as it delivers it, then a
        // no-wait pull for at most `max - 1` more, which the server closes at
        // once with whatever is already available.
        let mut deliveries = Vec::with_capacity(max);
        self.pull_into(&mut deliveries, 1, Some(expires)).await?;
        if !deliveries.is_empty() && max > 1 {
            // A failure here keeps what the first pull already owns.
            if let Err(error) = self.pull_into(&mut deliveries, max, None).await {
                tracing::warn!(
                    event = "nats_pull_remainder_unavailable",
                    error_code = error.code(),
                );
            }
        }
        Ok(deliveries)
    }

    async fn in_progress(&self, replies: &[String]) -> Result<usize, NatsJetStreamError> {
        self.ensure_open()?;
        if replies.is_empty() {
            return Ok(0);
        }
        if self.observer.is_disconnected() {
            return Err(NatsJetStreamError::unavailable(
                "the NATS connection is down; the heartbeat waits for a reconnect",
            ));
        }
        let epoch = self.observer.epoch();
        for reply in replies {
            self.owned_reply(reply)?;
            self.client
                .publish(reply.clone(), bytes::Bytes::from_static(b"+WPI"))
                .await
                .map_err(|_| NatsJetStreamError::unavailable("the NATS heartbeat failed"))?;
        }
        tokio::time::timeout(self.config.request_timeout, self.client.flush())
            .await
            .map_err(|_| NatsJetStreamError::timeout("the NATS heartbeat was not confirmed"))?
            .map_err(|_| NatsJetStreamError::unavailable("the NATS heartbeat was not confirmed"))?;
        if self.observer.epoch() != epoch {
            return Err(NatsJetStreamError::unavailable(
                "the NATS heartbeat spanned a reconnect and is unconfirmed",
            ));
        }
        Ok(replies.len())
    }

    async fn nak(&self, reply: &str, delay: Duration) -> Result<(), NatsJetStreamError> {
        let payload = format!("-NAK {{\"delay\":{}}}", delay.as_nanos()).into_bytes();
        self.request_ack(reply, payload, "the NATS nak was not confirmed")
            .await
    }

    async fn term(&self, reply: &str) -> Result<(), NatsJetStreamError> {
        self.request_ack(reply, b"+TERM".to_vec(), "the NATS term was not confirmed")
            .await
    }

    async fn record_dead_letter(
        &self,
        record: &DeadLetterRecord,
    ) -> Result<(), NatsJetStreamError> {
        self.ensure_open()?;
        tokio::time::timeout(
            self.config.request_timeout,
            self.dead_letters.put(&record.key, record.to_json().into()),
        )
        .await
        .map_err(|_| NatsJetStreamError::timeout("the NATS dead-letter put timed out"))?
        .map_err(|_| NatsJetStreamError::unavailable("the NATS dead-letter put failed"))?;
        Ok(())
    }

    fn worker_name(&self) -> &str {
        &self.config.client_name
    }

    async fn close(&self) -> Result<(), NatsJetStreamError> {
        if self.closed.swap(true, Ordering::AcqRel) {
            return Ok(());
        }
        tokio::time::timeout(self.config.request_timeout, self.client.drain())
            .await
            .map_err(|_| NatsJetStreamError::timeout("the NATS connection did not drain"))?
            .map_err(|_| NatsJetStreamError::unavailable("the NATS connection did not drain"))
    }
}

#[async_trait]
impl CommandRetirementClient for NatsCommandBus {
    /// Double ack (`AckSync`): the server answers the ack subject once the
    /// ack is applied. An already-acknowledged delivery is answered the same
    /// way, which is the idempotent "already retired" outcome.
    async fn retire_delivery(
        &self,
        request: CommandRetirementRequest,
    ) -> Result<(), CommandRetirementClientError> {
        if request.stream() != self.config.stream || request.consumer() != self.config.consumer {
            return Err(CommandRetirementClientError::Protocol);
        }
        self.request_ack(
            request.reply(),
            b"+ACK".to_vec(),
            "the NATS double ack was not confirmed",
        )
        .await
        .map_err(|error| match error.kind() {
            NatsJetStreamErrorKind::Authentication => CommandRetirementClientError::Authentication,
            NatsJetStreamErrorKind::Timeout => CommandRetirementClientError::Timeout,
            NatsJetStreamErrorKind::DependencyUnavailable | NatsJetStreamErrorKind::Closed => {
                CommandRetirementClientError::DependencyUnavailable
            }
            NatsJetStreamErrorKind::Configuration
            | NatsJetStreamErrorKind::ConsumerMissing
            | NatsJetStreamErrorKind::Protocol
            | NatsJetStreamErrorKind::ResourceExhausted => CommandRetirementClientError::Protocol,
        })
    }
}

/// The durable must be the contract's pull consumer (docs/runtime-command-bus.md
/// "Consumers and workers"); any drift refuses the bind.
pub(crate) fn verify_consumer(
    config: &async_nats::jetstream::consumer::Config,
    durable: &str,
    filter_subject: &str,
    fetch_batch: usize,
    fetch_expires: Duration,
) -> Result<(), NatsJetStreamError> {
    let filter_matches = (config.filter_subject == filter_subject
        && config.filter_subjects.is_empty())
        || (config.filter_subject.is_empty() && config.filter_subjects == [filter_subject]);
    let batch_admitted = config.max_batch == 0
        || i64::try_from(fetch_batch).is_ok_and(|batch| batch <= config.max_batch);
    let expires_admitted = config.max_expires.is_zero() || fetch_expires <= config.max_expires;
    if config.durable_name.as_deref() != Some(durable)
        || config.deliver_subject.is_some()
        || config.ack_policy != AckPolicy::Explicit
        || config.ack_wait != super::command_bus::ACK_WAIT
        || config.max_deliver != -1
        || !matches!(config.deliver_policy, DeliverPolicy::All)
        || !config.backoff.is_empty()
        || !filter_matches
        || !batch_admitted
        || !expires_admitted
    {
        return Err(NatsJetStreamError::configuration(
            "the NATS durable consumer drifted from the contract",
        ));
    }
    Ok(())
}

/// Parse `tls://h:p[,tls://h:p...]` or the `nats://` equivalent. Returns the
/// servers and whether they use TLS. User information, paths, queries and
/// mixed schemes are refused.
pub(crate) fn parse_server_urls(url: &str) -> Result<(Vec<String>, bool), NatsJetStreamError> {
    const MALFORMED: NatsJetStreamError =
        NatsJetStreamError::configuration("the NATS URL is malformed");
    if url.is_empty()
        || url.len() > MAX_URL_BYTES
        || !url
            .bytes()
            .all(|byte| (0x21..=0x7e).contains(&byte) && !matches!(byte, b'%' | b'?' | b'#'))
    {
        return Err(MALFORMED);
    }
    let mut servers = Vec::new();
    let mut tls = None;
    for server in url.split(',') {
        let (is_tls, authority) = if let Some(rest) = server.strip_prefix("tls://") {
            (true, rest)
        } else if let Some(rest) = server.strip_prefix("nats://") {
            (false, rest)
        } else {
            return Err(MALFORMED);
        };
        if *tls.get_or_insert(is_tls) != is_tls
            || authority.contains('@')
            || authority.contains('/')
            || servers.len() >= MAX_SERVERS
        {
            return Err(MALFORMED);
        }
        let (host, port_text) = if let Some(bracketed) = authority.strip_prefix('[') {
            let (host, suffix) = bracketed.split_once(']').ok_or(MALFORMED)?;
            let port = suffix.strip_prefix(':').ok_or(MALFORMED)?;
            if !host.contains(':')
                || !host
                    .bytes()
                    .all(|byte| byte.is_ascii_hexdigit() || matches!(byte, b':' | b'.'))
            {
                return Err(MALFORMED);
            }
            (host, port)
        } else {
            let (host, port) = authority.rsplit_once(':').ok_or(MALFORMED)?;
            if !host
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
            {
                return Err(MALFORMED);
            }
            (host, port)
        };
        if host.is_empty()
            || port_text.is_empty()
            || !port_text.bytes().all(|byte| byte.is_ascii_digit())
        {
            return Err(MALFORMED);
        }
        port_text
            .parse::<u16>()
            .ok()
            .filter(|port| *port != 0 && port_text == port.to_string())
            .ok_or(MALFORMED)?;
        servers.push(server.to_owned());
    }
    Ok((servers, tls.unwrap_or(false)))
}

fn bounded_operation_timeout(value: Duration) -> bool {
    (MIN_OPERATION_TIMEOUT..=MAX_OPERATION_TIMEOUT).contains(&value)
}

fn map_connect_error(error: &async_nats::ConnectError) -> NatsJetStreamError {
    match error.kind() {
        async_nats::ConnectErrorKind::AuthorizationViolation
        | async_nats::ConnectErrorKind::Authentication
        | async_nats::ConnectErrorKind::Tls => {
            NatsJetStreamError::authentication("the NATS connection was not authenticated")
        }
        async_nats::ConnectErrorKind::TimedOut => {
            NatsJetStreamError::timeout("the NATS connection timed out")
        }
        async_nats::ConnectErrorKind::ServerParse => {
            NatsJetStreamError::configuration("the NATS URL is malformed")
        }
        _ => NatsJetStreamError::unavailable("the NATS connection failed"),
    }
}

fn ring_provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// Read and check the material once before connecting, so a malformed
/// identity is a startup configuration error rather than a silent handshake
/// failure loop.
fn preflight_tls_material(paths: &NatsTlsPaths) -> Result<(), NatsJetStreamError> {
    let provider = ring_provider();
    load_roots(&paths.ca_path)?;
    load_certified_key(paths, &provider)?;
    Ok(())
}

pub(crate) fn reloading_tls_config(
    paths: NatsTlsPaths,
) -> Result<ClientConfig, NatsJetStreamError> {
    let provider = ring_provider();
    let verifier = Arc::new(ReloadingServerVerifier {
        ca_path: paths.ca_path.clone(),
        provider: Arc::clone(&provider),
    });
    let identity = Arc::new(ReloadingClientIdentity {
        paths,
        provider: Arc::clone(&provider),
    });
    Ok(ClientConfig::builder_with_provider(provider)
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| NatsJetStreamError::configuration("the NATS TLS profile is unavailable"))?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_cert_resolver(identity))
}

fn load_roots(ca_path: &std::path::Path) -> Result<RootCertStore, NatsJetStreamError> {
    let ca_pem = read_material(ca_path, MAX_CA_BYTES)?;
    let certificates = CertificateDer::pem_slice_iter(&ca_pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| NatsJetStreamError::configuration("the NATS private CA is malformed"))?;
    if certificates.is_empty() {
        return Err(NatsJetStreamError::configuration(
            "the NATS private CA is empty",
        ));
    }
    let mut roots = RootCertStore::empty();
    for certificate in certificates {
        roots
            .add(certificate)
            .map_err(|_| NatsJetStreamError::configuration("the NATS private CA is malformed"))?;
    }
    Ok(roots)
}

fn load_certified_key(
    paths: &NatsTlsPaths,
    provider: &CryptoProvider,
) -> Result<CertifiedKey, NatsJetStreamError> {
    let certificate_pem = read_material(&paths.certificate_path, MAX_CERTIFICATE_BYTES)?;
    let private_key_pem = Zeroizing::new(read_material(
        &paths.private_key_path,
        MAX_PRIVATE_KEY_BYTES,
    )?);
    let chain = CertificateDer::pem_slice_iter(&certificate_pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| {
            NatsJetStreamError::configuration("the NATS client certificate is malformed")
        })?;
    if chain.is_empty() {
        return Err(NatsJetStreamError::configuration(
            "the NATS client certificate is empty",
        ));
    }
    let key = PrivateKeyDer::from_pem_slice(&private_key_pem)
        .map_err(|_| NatsJetStreamError::configuration("the NATS client key is malformed"))?;
    CertifiedKey::from_der(chain, key, provider)
        .map_err(|_| NatsJetStreamError::configuration("the NATS client identity is malformed"))
}

/// Read one piece of NATS client material plainly, bounded in size.
///
/// Unlike the runtime material under the worker's private directory, these
/// files are a cert-manager Secret volume: root-owned, group-readable
/// (fsGroup) and symlinked through `..data`, which cert-manager swaps on
/// renewal. Following the symlink on every read is what makes a renewed
/// identity take effect on the next handshake, so no owner-only or
/// no-symlink policy applies here (the same as Go's `natsconn`).
fn read_material(path: &std::path::Path, max_bytes: usize) -> Result<Vec<u8>, NatsJetStreamError> {
    use std::io::Read as _;

    let file = std::fs::File::open(path)
        .map_err(|_| NatsJetStreamError::unavailable("the NATS TLS material is unavailable"))?;
    let mut bytes = Vec::new();
    file.take(
        u64::try_from(max_bytes)
            .unwrap_or(u64::MAX)
            .saturating_add(1),
    )
    .read_to_end(&mut bytes)
    .map_err(|_| NatsJetStreamError::unavailable("the NATS TLS material is unavailable"))?;
    if bytes.is_empty() || bytes.len() > max_bytes {
        return Err(NatsJetStreamError::configuration(
            "the NATS TLS material is invalid",
        ));
    }
    Ok(bytes)
}

/// Client identity read from its files on every handshake.
#[derive(Debug)]
struct ReloadingClientIdentity {
    paths: NatsTlsPaths,
    provider: Arc<CryptoProvider>,
}

impl ResolvesClientCert for ReloadingClientIdentity {
    fn resolve(
        &self,
        _root_hint_subjects: &[&[u8]],
        _sigschemes: &[SignatureScheme],
    ) -> Option<Arc<CertifiedKey>> {
        match load_certified_key(&self.paths, &self.provider) {
            Ok(key) => Some(Arc::new(key)),
            Err(error) => {
                tracing::error!(
                    event = "nats_client_identity_unavailable",
                    error_code = error.code()
                );
                None
            }
        }
    }

    fn has_certs(&self) -> bool {
        true
    }
}

/// Private-CA verification with the CA bundle read on every handshake.
#[derive(Debug)]
struct ReloadingServerVerifier {
    ca_path: PathBuf,
    provider: Arc<CryptoProvider>,
}

impl ServerCertVerifier for ReloadingServerVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let roots = load_roots(&self.ca_path).map_err(|error| {
            tracing::error!(
                event = "nats_private_ca_unavailable",
                error_code = error.code()
            );
            rustls::Error::General("the NATS private CA is unavailable".to_owned())
        })?;
        WebPkiServerVerifier::builder_with_provider(Arc::new(roots), Arc::clone(&self.provider))
            .build()
            .map_err(|_| rustls::Error::General("the NATS private CA is unusable".to_owned()))?
            .verify_server_cert(end_entity, intermediates, server_name, ocsp_response, now)
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

#[cfg(test)]
mod tests {
    use std::path::PathBuf;
    use std::time::Duration;

    use async_nats::jetstream::consumer::{AckPolicy, Config as ConsumerConfig, DeliverPolicy};

    use super::{
        NatsJetStreamConfig, NatsJetStreamErrorKind, NatsTlsPaths, parse_server_urls,
        verify_consumer,
    };
    use crate::transport::command_bus::CommandBusLimits;

    fn config() -> NatsJetStreamConfig {
        NatsJetStreamConfig {
            url: "tls://nats.internal:4222".to_owned(),
            tls: Some(NatsTlsPaths {
                ca_path: PathBuf::from("/runtime/nats/ca.crt"),
                certificate_path: PathBuf::from("/runtime/nats/tls.crt"),
                private_key_path: PathBuf::from("/runtime/nats/tls.key"),
            }),
            stream: "ELITEA_RT_V1_AGENT".to_owned(),
            consumer: "elitea-agent-worker-v1".to_owned(),
            client_name: "worker-1".to_owned(),
            limits: CommandBusLimits::runtime_v1(),
            fetch_batch: 4,
            fetch_expires: Duration::from_secs(1),
            connection_timeout: Duration::from_secs(5),
            request_timeout: Duration::from_secs(5),
        }
    }

    fn contract_consumer() -> ConsumerConfig {
        ConsumerConfig {
            durable_name: Some("elitea-agent-worker-v1".to_owned()),
            name: Some("elitea-agent-worker-v1".to_owned()),
            deliver_policy: DeliverPolicy::All,
            ack_policy: AckPolicy::Explicit,
            ack_wait: Duration::from_mins(1),
            max_deliver: -1,
            filter_subject: "elitea.rt.v1.agent.d.*".to_owned(),
            max_waiting: 512,
            max_ack_pending: 1024,
            max_batch: 64,
            max_expires: Duration::from_secs(30),
            ..Default::default()
        }
    }

    #[test]
    fn client_material_follows_symlinks_and_is_size_bounded() {
        use std::os::unix::fs::PermissionsExt as _;

        let root = tempfile::tempdir().expect("material directory");
        let data = root.path().join("..2026_10_06");
        std::fs::create_dir(&data).expect("data directory");
        std::fs::write(data.join("tls.crt"), b"certificate").expect("material");
        std::fs::set_permissions(data.join("tls.crt"), std::fs::Permissions::from_mode(0o444))
            .expect("group-readable mode");
        std::os::unix::fs::symlink(&data, root.path().join("..data")).expect("..data link");
        std::os::unix::fs::symlink("..data/tls.crt", root.path().join("tls.crt"))
            .expect("file link");
        assert_eq!(
            super::read_material(&root.path().join("tls.crt"), 64).expect("symlinked read"),
            b"certificate"
        );
        assert_eq!(
            super::read_material(&root.path().join("tls.crt"), 4)
                .err()
                .map(|error| error.kind()),
            Some(NatsJetStreamErrorKind::Configuration)
        );
        assert_eq!(
            super::read_material(&root.path().join("absent.crt"), 64)
                .err()
                .map(|error| error.kind()),
            Some(NatsJetStreamErrorKind::DependencyUnavailable)
        );
    }

    #[test]
    fn urls_refuse_userinfo_paths_and_mixed_schemes() {
        assert_eq!(
            parse_server_urls("tls://a:4222,tls://b.internal:4222").expect("seed list"),
            (
                vec![
                    "tls://a:4222".to_owned(),
                    "tls://b.internal:4222".to_owned()
                ],
                true
            )
        );
        assert!(!parse_server_urls("nats://nats:4222").expect("plain").1);
        assert!(parse_server_urls("tls://[::1]:4222").is_ok());
        for malformed in [
            "",
            "tls://user:pass@nats:4222",
            "tls://token@nats:4222",
            "tls://nats:4222/path",
            "tls://nats",
            "tls://nats:0",
            "tls://nats:04222",
            "tls://nats:4222,nats://nats:4222",
            "rediss://worker@redis:6379/0",
            "tls://nats:4222?x=1",
            "tls://na ts:4222",
        ] {
            assert!(parse_server_urls(malformed).is_err(), "{malformed}");
        }
    }

    #[test]
    fn tls_material_is_all_or_nothing_and_matches_the_scheme() {
        assert!(config().validate().is_ok());
        let mut plain = config();
        plain.url = "nats://nats:4222".to_owned();
        assert!(plain.validate().is_err(), "nats:// with material");
        plain.tls = None;
        assert!(plain.validate().is_ok());
        let mut missing = config();
        missing.tls = None;
        assert!(missing.validate().is_err(), "tls:// requires material");
        let mut relative = config();
        relative.tls.as_mut().expect("tls").ca_path = PathBuf::from("ca.crt");
        assert!(relative.validate().is_err());
    }

    #[test]
    fn only_contract_pairs_and_bounded_pulls_are_admitted() {
        let mut mismatched = config();
        mismatched.consumer = "elitea-index-worker-v1".to_owned();
        assert_eq!(
            mismatched.validate().err().map(|error| error.kind()),
            Some(NatsJetStreamErrorKind::Configuration)
        );
        for (batch, expires) in [
            (0, Duration::from_secs(1)),
            (65, Duration::from_secs(1)),
            (4, Duration::from_millis(99)),
            (4, Duration::from_millis(30_001)),
        ] {
            let mut invalid = config();
            invalid.fetch_batch = batch;
            invalid.fetch_expires = expires;
            assert!(invalid.validate().is_err(), "{batch} {expires:?}");
        }
        let mut name = config();
        name.client_name = "worker 1".to_owned();
        assert!(name.validate().is_err());
    }

    #[test]
    fn the_bind_refuses_an_absent_or_drifted_durable() {
        let filter = "elitea.rt.v1.agent.d.*";
        let accept = |config: &ConsumerConfig| {
            verify_consumer(
                config,
                "elitea-agent-worker-v1",
                filter,
                4,
                Duration::from_secs(1),
            )
            .is_ok()
        };
        assert!(accept(&contract_consumer()));
        let drifts: [fn(&mut ConsumerConfig); 10] = [
            |c| c.deliver_subject = Some("push.inbox".to_owned()),
            |c| c.durable_name = None,
            |c| c.ack_policy = AckPolicy::All,
            |c| c.ack_wait = Duration::from_secs(30),
            |c| c.max_deliver = 5,
            |c| c.deliver_policy = DeliverPolicy::New,
            |c| c.backoff = vec![Duration::from_secs(1)],
            |c| c.filter_subject = "elitea.rt.v1.index.d.*".to_owned(),
            |c| c.max_batch = 2,
            |c| c.max_expires = Duration::from_millis(500),
        ];
        for drift in drifts {
            let mut drifted = contract_consumer();
            drift(&mut drifted);
            assert!(!accept(&drifted));
        }
        let mut subjects = contract_consumer();
        subjects.filter_subject = String::new();
        subjects.filter_subjects = vec![filter.to_owned()];
        assert!(accept(&subjects));
    }
}
