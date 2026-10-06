//! Transport-neutral runtime command bus contract.
//!
//! `docs/runtime-command-bus.md` is the normative contract this module
//! restates: the stream, consumer and subject names, the per-delivery subject
//! `elitea.rt.v1.<route>.d.<sha256(delivery_id)>`, the dead-letter record and
//! the constants that are not configuration. The NATS client lives in
//! [`super::nats_jetstream`]; everything here is pure so the execution layer
//! and its tests never need a broker.
//!
//! The only path that turns a durable terminal proof into removal of a
//! command is [`CommandRetirer`]: it binds the verified command, the terminal
//! authority and the exact delivered bytes, then asks the transport for a
//! double ack. There is no generic ack, delete or publish surface.

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use async_trait::async_trait;

use crate::protocol::command::{
    VerifiedAgentCommand, VerifiedExecutionCommand, VerifiedToolkitExecuteReadCommand,
};
use crate::protocol::control::AgentCommandRetirementAuthority;

/// The subject namespace of the command bus.
pub const SUBJECT_ROOT: &str = "elitea.rt.v1";
/// Every command stream is `ELITEA_RT_V1_<ROUTE>`.
pub const STREAM_PREFIX: &str = "ELITEA_RT_V1_";
pub const STREAM_VALIDATE: &str = "ELITEA_RT_V1_VALIDATE";
pub const STREAM_AGENT: &str = "ELITEA_RT_V1_AGENT";
pub const STREAM_INDEX: &str = "ELITEA_RT_V1_INDEX";
pub const CONSUMER_VALIDATE: &str = "elitea-configuration-worker-v1";
pub const CONSUMER_AGENT: &str = "elitea-agent-worker-v1";
pub const CONSUMER_INDEX: &str = "elitea-index-worker-v1";
/// `JetStream` de-duplication header; its value is the subject's hash token.
pub const HEADER_MSG_ID: &str = "Nats-Msg-Id";
/// Diagnostic copy of the raw delivery ID. Never authority.
pub const HEADER_DELIVERY_ID: &str = "Elitea-Delivery-Id";
/// The KV bucket a worker records a poison command in.
pub const DEAD_LETTER_BUCKET: &str = "ELITEA_RT_V1_DEADLETTER";
/// Schema of one dead-letter record.
pub const DEAD_LETTER_SCHEMA: &str = "elitea.runtime.dead-letter.v1";
/// The only inbox prefix the `elitea-worker` permission row admits.
pub const WORKER_INBOX_PREFIX: &str = "_INBOX_elitea-worker";
/// The `JetStream` API prefix under which the WORKER account imports RUNTIME's
/// command-bus consumer API (`CONSUMER.INFO` and `CONSUMER.MSG.NEXT` of the
/// three worker durables). A worker that presents its identity opens the
/// command-bus context with it; its own account's `JetStream` (the dead-letter
/// bucket only) keeps the default `$JS.API`. Go's
/// `natsconn.WorkerRuntimeJSAPIPrefix` and the chart's import mappings name the
/// same prefix (`deploy/helm/tests/render-nats-security.sh` pins all three).
pub const RUNTIME_API_PREFIX: &str = "JS.RUNTIME.API";
/// The consumer's redelivery timer (twice Main's 30s claim lease).
pub const ACK_WAIT: Duration = Duration::from_mins(1);
/// The nak delay of a poison command that might still be served later (a
/// decode or version mismatch, an unsupported command). A command that can
/// never verify — a bad signature, or a subject that does not name it — is
/// terminated instead ([`PoisonReason::terminates`]).
pub const POISON_DELAY: Duration = Duration::from_hours(24);
/// The consumer's `MaxRequestBatch`.
pub const MAX_REQUEST_BATCH: usize = 64;
/// The consumer's `MaxRequestExpires`.
pub const MAX_REQUEST_EXPIRES: Duration = Duration::from_secs(30);
/// `max_transport_message_bytes`: payload plus headers (the stream's `MaxMsgSize`).
pub const MAX_TRANSPORT_MESSAGE_BYTES: usize = 64 * 1024;
/// `max_transport_payload_bytes`: the signed envelope body.
pub const MAX_TRANSPORT_PAYLOAD_BYTES: usize = 48 * 1024;

const DELIVERY_TOKEN_HEX_BYTES: usize = 64;
const MAX_IDENTITY_BYTES: usize = 256;
const MAX_REPLY_BYTES: usize = 512;
const MAX_SUBJECT_BYTES: usize = 256;

/// The three (stream, durable, route) rows of the contract.
const ROUTES: [(&str, &str, &str); 3] = [
    (STREAM_VALIDATE, CONSUMER_VALIDATE, "validate"),
    (STREAM_AGENT, CONSUMER_AGENT, "agent"),
    (STREAM_INDEX, CONSUMER_INDEX, "index"),
];

/// The lower-case route token of a contract stream.
#[must_use]
pub fn route_token(stream: &str) -> Option<&'static str> {
    ROUTES
        .iter()
        .find(|(name, _, _)| *name == stream)
        .map(|(_, _, route)| *route)
}

/// The bootstrap-created durable of a contract stream.
#[must_use]
pub fn consumer_for_stream(stream: &str) -> Option<&'static str> {
    ROUTES
        .iter()
        .find(|(name, _, _)| *name == stream)
        .map(|(_, consumer, _)| *consumer)
}

/// Whether `(stream, consumer)` is one contract row. Any other pairing is
/// refused before a connection is opened.
#[must_use]
pub fn valid_route_pair(stream: &str, consumer: &str) -> bool {
    consumer_for_stream(stream) == Some(consumer)
}

/// `elitea.rt.v1.<route>.d.*`, the stream subject and consumer filter.
#[must_use]
pub fn filter_subject(stream: &str) -> Option<String> {
    route_token(stream).map(|route| format!("{SUBJECT_ROOT}.{route}.d.*"))
}

/// `hex(sha256(delivery_id))`, 64 lower-case hex characters.
#[must_use]
pub fn delivery_token(delivery_id: &str) -> String {
    let digest = ring::digest::digest(&ring::digest::SHA256, delivery_id.as_bytes());
    lower_hex(digest.as_ref())
}

/// The one subject a delivery is published on.
#[must_use]
pub fn delivery_subject(stream: &str, delivery_id: &str) -> Option<String> {
    route_token(stream)
        .map(|route| format!("{SUBJECT_ROOT}.{route}.d.{}", delivery_token(delivery_id)))
}

/// A delivery's key in [`DEAD_LETTER_BUCKET`].
#[must_use]
pub fn dead_letter_key(route: &str, token: &str) -> String {
    format!("{route}.{token}")
}

fn lower_hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut text = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        text.push(char::from(DIGITS[usize::from(byte >> 4)]));
        text.push(char::from(DIGITS[usize::from(byte & 0x0f)]));
    }
    text
}

fn is_delivery_token(value: &str) -> bool {
    value.len() == DELIVERY_TOKEN_HEX_BYTES
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

/// Post-decode bounds for one delivered command message.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CommandBusLimits {
    pub max_message_bytes: usize,
    pub max_payload_bytes: usize,
}

impl CommandBusLimits {
    /// The runtime-v1 profile (limits conformance v3).
    #[must_use]
    pub const fn runtime_v1() -> Self {
        Self {
            max_message_bytes: MAX_TRANSPORT_MESSAGE_BYTES,
            max_payload_bytes: MAX_TRANSPORT_PAYLOAD_BYTES,
        }
    }

    /// Validate the bounds.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration error for zero or inverted limits.
    pub fn validate(self) -> Result<Self, CommandBusError> {
        if self.max_message_bytes == 0
            || self.max_payload_bytes == 0
            || self.max_payload_bytes > self.max_message_bytes
        {
            return Err(CommandBusError::InvalidInput(
                "the command bus limits are malformed",
            ));
        }
        Ok(self)
    }
}

/// The `JetStream` coordinates carried by a delivery's ack reply subject
/// (`$JS.ACK.<stream>.<consumer>.<delivered>.<stream seq>.<consumer seq>.<ts>.<pending>`,
/// or the domain/account-hash form).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AckReplyInfo {
    pub stream: String,
    pub consumer: String,
    pub delivered: u64,
    pub stream_sequence: u64,
}

impl AckReplyInfo {
    /// Parse a `JetStream` ack reply subject.
    ///
    /// # Errors
    ///
    /// Returns a stable protocol error when the subject is not a bounded
    /// `JetStream` ack subject.
    pub fn parse(reply: &str) -> Result<Self, CommandBusError> {
        const MALFORMED: CommandBusError =
            CommandBusError::InvalidInput("the delivery acknowledgement subject is malformed");
        if reply.is_empty() || reply.len() > MAX_REPLY_BYTES || !reply.is_ascii() {
            return Err(MALFORMED);
        }
        let tokens = reply
            .strip_prefix("$JS.ACK.")
            .ok_or(MALFORMED)?
            .split('.')
            .collect::<Vec<_>>();
        let offset = match tokens.len() {
            7 => 0,
            9 | 10 => 2,
            _ => return Err(MALFORMED),
        };
        let number = |index: usize| -> Result<u64, CommandBusError> {
            let token = tokens[offset + index];
            if token.is_empty() || !token.bytes().all(|byte| byte.is_ascii_digit()) {
                return Err(MALFORMED);
            }
            token.parse::<u64>().map_err(|_| MALFORMED)
        };
        let stream = tokens[offset];
        let consumer = tokens[offset + 1];
        if !bounded_text(stream, MAX_IDENTITY_BYTES) || !bounded_text(consumer, MAX_IDENTITY_BYTES)
        {
            return Err(MALFORMED);
        }
        let delivered = number(2)?;
        let stream_sequence = number(3)?;
        number(4)?;
        if delivered == 0 || stream_sequence == 0 {
            return Err(MALFORMED);
        }
        Ok(Self {
            stream: stream.to_owned(),
            consumer: consumer.to_owned(),
            delivered,
            stream_sequence,
        })
    }
}

/// Process-local record that the delivery reached a confirmed double ack.
///
/// The delivery runtime keeps a clone while the processor owns the delivery,
/// and uses it to choose between "settled, nothing to do" and a retry nak.
#[derive(Debug, Default)]
pub struct DeliverySettlement {
    retired: AtomicBool,
}

impl DeliverySettlement {
    #[must_use]
    pub fn retired(&self) -> bool {
        self.retired.load(Ordering::Acquire)
    }

    fn mark_retired(&self) {
        self.retired.store(true, Ordering::Release);
    }

    /// Stand in for a confirmed double ack. Test-only.
    #[cfg(test)]
    pub(crate) fn test_mark_retired(&self) {
        self.mark_retired();
    }
}

/// Where a delivery lives: everything but the signed bytes. Cloneable and
/// data-free (subjects carry only hash tokens).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeliveryCoordinates {
    pub stream: String,
    pub consumer: String,
    pub subject: String,
    /// The subject's 64-hex hash token, when the subject carries one.
    pub token: Option<String>,
    pub reply: String,
    pub stream_sequence: u64,
    pub delivered: u64,
}

impl DeliveryCoordinates {
    /// The route token of the delivering stream.
    #[must_use]
    pub fn route(&self) -> Option<&'static str> {
        route_token(&self.stream)
    }
}

/// Exact command message admitted for signed-command verification.
///
/// The value is intentionally non-`Clone` and non-`Debug`: retirement must
/// retain the same bounded bytes that were verified and must not log them.
pub struct CommandDelivery {
    coordinates: DeliveryCoordinates,
    token: String,
    signed_envelope: Vec<u8>,
    settlement: Arc<DeliverySettlement>,
}

/// Why a delivery is poison (a stable, low-cardinality dead-letter reason).
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum PoisonReason {
    /// The subject is not `elitea.rt.v1.<route>.d.<64 hex>` for this stream.
    MalformedSubject,
    /// The body exceeds the transport bounds or is empty.
    MalformedMessage,
    /// The signed envelope or command does not decode or is incompatible.
    EnvelopeInvalid,
    /// The Ed25519 signature or its key ID does not verify.
    SignatureInvalid,
    /// The subject's hash token is not `sha256(command.idempotency_key)`.
    SubjectMismatch,
    /// A verified command this worker cannot serve.
    UnsupportedCommand,
}

impl PoisonReason {
    /// Whether the delivery is terminated (`+TERM`) rather than parked for
    /// [`POISON_DELAY`]. A signature that does not verify, or a subject that
    /// does not name the signed command, cannot become valid by waiting, and
    /// a terminated message frees its slot in the stream (`MaxMsgs`); parked
    /// it would hold the slot for a day.
    #[must_use]
    pub const fn terminates(self) -> bool {
        matches!(self, Self::SignatureInvalid | Self::SubjectMismatch)
    }

    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::MalformedSubject => "malformed_subject",
            Self::MalformedMessage => "malformed_message",
            Self::EnvelopeInvalid => "envelope_invalid",
            Self::SignatureInvalid => "signature_invalid",
            Self::SubjectMismatch => "subject_mismatch",
            Self::UnsupportedCommand => "unsupported_command",
        }
    }
}

/// A delivered message that can never be processed. It is nak'd with
/// [`POISON_DELAY`] and recorded in the dead-letter bucket.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PoisonDelivery {
    pub coordinates: DeliveryCoordinates,
    pub reason: PoisonReason,
}

/// One message as the transport decoded it.
pub enum DecodedDelivery {
    Command(CommandDelivery),
    Poison(PoisonDelivery),
}

/// What processing concluded about a delivery the runtime handed over.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum DeliveryVerdict {
    /// Processing ended. If the command was retired (double ack confirmed) the
    /// runtime does nothing more; otherwise it naks with the retry delay.
    Processed,
    /// The command is poison. `delivery_token` is `sha256(idempotency_key)`
    /// when the envelope decoded far enough to name it.
    Poison {
        reason: PoisonReason,
        delivery_token: Option<String>,
    },
}

impl DeliveryVerdict {
    #[must_use]
    pub const fn poison(reason: PoisonReason) -> Self {
        Self::Poison {
            reason,
            delivery_token: None,
        }
    }
}

impl CommandDelivery {
    /// Strictly decode one delivered message for verification.
    ///
    /// # Errors
    ///
    /// Returns a typed malformed or resource-limit error for an unusable ack
    /// subject, a foreign stream/consumer, a malformed subject or an oversized
    /// body. The transport uses [`decode_delivery`] instead, which keeps a
    /// poison message ackable.
    pub fn decode(
        subject: &str,
        reply: &str,
        payload: Vec<u8>,
        limits: CommandBusLimits,
    ) -> Result<Self, CommandBusError> {
        let message_bytes = subject
            .len()
            .checked_add(reply.len())
            .and_then(|total| total.checked_add(payload.len()))
            .ok_or(CommandBusError::ResourceExhausted(
                "the delivered command exceeds the approved limit",
            ))?;
        match decode_delivery(subject, reply, payload, message_bytes, limits)? {
            DecodedDelivery::Command(delivery) => Ok(delivery),
            DecodedDelivery::Poison(poison) => Err(match poison.reason {
                PoisonReason::MalformedMessage => CommandBusError::ResourceExhausted(
                    "the delivered command exceeds the approved limit",
                ),
                _ => CommandBusError::InvalidInput("the delivered command subject is malformed"),
            }),
        }
    }

    #[must_use]
    pub fn signed_envelope(&self) -> &[u8] {
        &self.signed_envelope
    }

    #[must_use]
    pub fn stream(&self) -> &str {
        &self.coordinates.stream
    }

    #[must_use]
    pub fn subject(&self) -> &str {
        &self.coordinates.subject
    }

    /// The subject's `sha256(delivery_id)` token.
    #[must_use]
    pub fn delivery_token(&self) -> &str {
        &self.token
    }

    #[must_use]
    pub fn stream_sequence(&self) -> u64 {
        self.coordinates.stream_sequence
    }

    #[must_use]
    pub fn delivered(&self) -> u64 {
        self.coordinates.delivered
    }

    #[must_use]
    pub fn coordinates(&self) -> &DeliveryCoordinates {
        &self.coordinates
    }

    #[must_use]
    pub fn settlement(&self) -> Arc<DeliverySettlement> {
        Arc::clone(&self.settlement)
    }

    /// The double-ack request for this delivery, bypassing the authority
    /// binding. Test-only: production retirement goes through
    /// [`CommandRetirer`].
    #[cfg(test)]
    #[must_use]
    pub(crate) fn test_retirement_request(&self) -> CommandRetirementRequest {
        CommandRetirementRequest {
            stream: self.coordinates.stream.clone(),
            consumer: self.coordinates.consumer.clone(),
            subject: self.coordinates.subject.clone(),
            reply: self.coordinates.reply.clone(),
            stream_sequence: self.coordinates.stream_sequence,
        }
    }

    /// The contract's pre-claim check: the subject names this command.
    ///
    /// # Errors
    ///
    /// Returns the poison verdict when the subject's hash token is not
    /// `sha256(idempotency_key)`.
    pub fn bind_idempotency_key(&self, idempotency_key: &str) -> Result<(), DeliveryVerdict> {
        let expected = delivery_token(idempotency_key);
        if expected == self.token {
            Ok(())
        } else {
            Err(DeliveryVerdict::Poison {
                reason: PoisonReason::SubjectMismatch,
                delivery_token: Some(expected),
            })
        }
    }
}

/// Decode one fetched message. `message_bytes` is the size the server
/// reported for the whole message (headers included).
///
/// # Errors
///
/// Returns an error only when the ack subject is unusable or names another
/// stream or consumer: such a message cannot even be nak'd and is left to
/// `AckWait`. Every other defect is a [`DecodedDelivery::Poison`].
pub fn decode_delivery(
    subject: &str,
    reply: &str,
    payload: Vec<u8>,
    message_bytes: usize,
    limits: CommandBusLimits,
) -> Result<DecodedDelivery, CommandBusError> {
    let limits = limits.validate()?;
    let info = AckReplyInfo::parse(reply)?;
    let route = consumer_for_stream(&info.stream)
        .filter(|consumer| *consumer == info.consumer)
        .and_then(|_| route_token(&info.stream))
        .ok_or(CommandBusError::InvalidInput(
            "the delivery belongs to another stream or consumer",
        ))?;
    let subject_token = subject
        .strip_prefix(SUBJECT_ROOT)
        .and_then(|rest| rest.strip_prefix('.'))
        .and_then(|rest| rest.strip_prefix(route))
        .and_then(|rest| rest.strip_prefix(".d."))
        .filter(|token| is_delivery_token(token));
    let bounded_subject = subject.len() <= MAX_SUBJECT_BYTES && subject.is_ascii();
    let coordinates = DeliveryCoordinates {
        stream: info.stream,
        consumer: info.consumer,
        subject: if bounded_subject {
            subject.to_owned()
        } else {
            String::from("<unbounded>")
        },
        token: subject_token.map(str::to_owned),
        reply: reply.to_owned(),
        stream_sequence: info.stream_sequence,
        delivered: info.delivered,
    };
    let Some(token) = subject_token.filter(|_| bounded_subject) else {
        return Ok(DecodedDelivery::Poison(PoisonDelivery {
            coordinates,
            reason: PoisonReason::MalformedSubject,
        }));
    };
    if payload.is_empty()
        || payload.len() > limits.max_payload_bytes
        || message_bytes > limits.max_message_bytes
    {
        return Ok(DecodedDelivery::Poison(PoisonDelivery {
            coordinates,
            reason: PoisonReason::MalformedMessage,
        }));
    }
    let token = token.to_owned();
    Ok(DecodedDelivery::Command(CommandDelivery {
        coordinates,
        token,
        signed_envelope: payload,
        settlement: Arc::new(DeliverySettlement::default()),
    }))
}

/// One dead-letter record (schema `elitea.runtime.dead-letter.v1`). It says
/// where to look, never what the command said: no envelope bytes, no delivery
/// ID and no command field.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeadLetterRecord {
    pub key: String,
    pub stream: String,
    pub consumer: String,
    pub subject: String,
    pub stream_sequence: u64,
    pub num_delivered: u64,
    pub reason: PoisonReason,
    pub worker: String,
    pub recorded_at_unix_millis: u64,
}

impl DeadLetterRecord {
    /// Build the record. The key is `<route>.<sha256(delivery_id)>`: the
    /// idempotency key's token when the envelope decoded far enough, else the
    /// subject's token, else `sha256(subject)` for a subject without one.
    #[must_use]
    pub fn new(
        coordinates: &DeliveryCoordinates,
        reason: PoisonReason,
        delivery_token: Option<&str>,
        worker: &str,
        recorded_at_unix_millis: u64,
    ) -> Self {
        let route = coordinates.route().unwrap_or("unknown");
        let token = delivery_token
            .filter(|token| is_delivery_token(token))
            .map(str::to_owned)
            .or_else(|| coordinates.token.clone())
            .unwrap_or_else(|| self::delivery_token(&coordinates.subject));
        Self {
            key: dead_letter_key(route, &token),
            stream: coordinates.stream.clone(),
            consumer: coordinates.consumer.clone(),
            subject: coordinates.subject.clone(),
            stream_sequence: coordinates.stream_sequence,
            num_delivered: coordinates.delivered,
            reason,
            worker: worker.to_owned(),
            recorded_at_unix_millis,
        }
    }

    /// The JSON value stored in the bucket.
    #[must_use]
    pub fn to_json(&self) -> Vec<u8> {
        let value = serde_json::json!({
            "schema": DEAD_LETTER_SCHEMA,
            "stream": self.stream,
            "consumer": self.consumer,
            "subject": self.subject,
            "stream_sequence": self.stream_sequence,
            "num_delivered": self.num_delivered,
            "reason": self.reason.code(),
            "worker": self.worker,
            "recorded_at_unix_millis": self.recorded_at_unix_millis,
        });
        serde_json::to_vec(&value).unwrap_or_default()
    }
}

#[derive(Clone, Eq, PartialEq)]
pub struct CommandRetirementConfig {
    pub stream: String,
    pub consumer: String,
}

impl CommandRetirementConfig {
    fn validate(&self) -> bool {
        valid_route_pair(&self.stream, &self.consumer)
    }
}

/// Restricted retirement request for a transport implementation: the double
/// ack of one exact delivery.
pub struct CommandRetirementRequest {
    stream: String,
    consumer: String,
    subject: String,
    reply: String,
    stream_sequence: u64,
}

impl CommandRetirementRequest {
    #[must_use]
    pub fn stream(&self) -> &str {
        &self.stream
    }

    #[must_use]
    pub fn consumer(&self) -> &str {
        &self.consumer
    }

    #[must_use]
    pub fn subject(&self) -> &str {
        &self.subject
    }

    /// The `$JS.ACK...` subject the double ack is sent to.
    #[must_use]
    pub fn reply(&self) -> &str {
        &self.reply
    }

    #[must_use]
    pub fn stream_sequence(&self) -> u64 {
        self.stream_sequence
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CommandRetirementClientError {
    Authentication,
    DependencyUnavailable,
    Timeout,
    Protocol,
}

impl fmt::Display for CommandRetirementClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        let message = match self {
            Self::Authentication => "command retirement authentication failed",
            Self::DependencyUnavailable => "command retirement is unavailable",
            Self::Timeout => "command retirement was not confirmed in time",
            Self::Protocol => "command retirement returned a malformed response",
        };
        formatter.write_str(message)
    }
}

impl std::error::Error for CommandRetirementClientError {}

/// The transport's double ack. `Ok` means the server confirmed it; an
/// "already acknowledged" delivery is confirmed like a first ack.
#[async_trait]
pub trait CommandRetirementClient: Send + Sync {
    async fn retire_delivery(
        &self,
        request: CommandRetirementRequest,
    ) -> Result<(), CommandRetirementClientError>;
}

#[async_trait]
impl<T> CommandRetirementClient for Arc<T>
where
    T: CommandRetirementClient + ?Sized,
{
    async fn retire_delivery(
        &self,
        request: CommandRetirementRequest,
    ) -> Result<(), CommandRetirementClientError> {
        self.as_ref().retire_delivery(request).await
    }
}

#[derive(Debug)]
pub enum CommandBusError {
    InvalidInput(&'static str),
    ResourceExhausted(&'static str),
    AuthorizationFailed(&'static str),
    DependencyUnavailable(&'static str),
    Client(CommandRetirementClientError),
}

impl CommandBusError {
    /// Stable low-cardinality category for operator logs and metrics.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InvalidInput(_) => "command_bus.invalid_input",
            Self::ResourceExhausted(_) => "command_bus.resource_exhausted",
            Self::AuthorizationFailed(_) => "command_bus.authorization_failed",
            Self::DependencyUnavailable(_)
            | Self::Client(
                CommandRetirementClientError::DependencyUnavailable
                | CommandRetirementClientError::Timeout,
            ) => "command_bus.dependency_unavailable",
            Self::Client(CommandRetirementClientError::Authentication) => {
                "command_bus.authentication_failed"
            }
            Self::Client(CommandRetirementClientError::Protocol) => "command_bus.protocol_failure",
        }
    }

    /// Whether the same immutable retirement operation may succeed later.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self,
            Self::DependencyUnavailable(_)
                | Self::Client(
                    CommandRetirementClientError::DependencyUnavailable
                        | CommandRetirementClientError::Timeout
                )
        )
    }
}

impl fmt::Display for CommandBusError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidInput(message)
            | Self::ResourceExhausted(message)
            | Self::AuthorizationFailed(message)
            | Self::DependencyUnavailable(message) => formatter.write_str(message),
            Self::Client(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for CommandBusError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Client(error) => Some(error),
            _ => None,
        }
    }
}

impl From<CommandRetirementClientError> for CommandBusError {
    fn from(value: CommandRetirementClientError) -> Self {
        Self::Client(value)
    }
}

/// The only path that converts a durable terminal proof into command
/// retirement. No arbitrary ack/delete/publish operation is exposed.
pub struct CommandRetirer<C> {
    client: C,
    config: CommandRetirementConfig,
}

impl<C> CommandRetirer<C> {
    /// Bind the restricted client to one stream/durable pair of the contract.
    ///
    /// # Errors
    ///
    /// Returns a stable error for a pairing outside the contract.
    pub fn new(client: C, config: CommandRetirementConfig) -> Result<Self, CommandBusError> {
        if !config.validate() {
            return Err(CommandBusError::InvalidInput(
                "the command retirement identity is malformed",
            ));
        }
        Ok(Self { client, config })
    }
}

impl<C: CommandRetirementClient> CommandRetirer<C> {
    /// Double-ack one exact verified settled command.
    ///
    /// The consuming authority guarantees that output/settlement or a
    /// state-owner terminal redelivery decision happened first.
    ///
    /// # Errors
    ///
    /// Returns a typed binding, client, or unconfirmed-retirement error.
    pub async fn retire_agent_command(
        &self,
        delivery: CommandDelivery,
        verified: &VerifiedAgentCommand,
        authority: AgentCommandRetirementAuthority,
    ) -> Result<(), CommandBusError> {
        self.retire_command(delivery, verified, authority).await
    }

    pub(crate) async fn retire_toolkit_execute_read_command(
        &self,
        delivery: CommandDelivery,
        verified: &VerifiedToolkitExecuteReadCommand,
        authority: AgentCommandRetirementAuthority,
    ) -> Result<(), CommandBusError> {
        self.retire_command(delivery, verified, authority).await
    }

    async fn retire_command(
        &self,
        delivery: CommandDelivery,
        verified: &impl VerifiedExecutionCommand,
        authority: AgentCommandRetirementAuthority,
    ) -> Result<(), CommandBusError> {
        let (authority_identity, authority_delivery_id, authority_signed_envelope) =
            authority.into_binding();
        if delivery.coordinates.stream != self.config.stream
            || delivery.coordinates.consumer != self.config.consumer
        {
            return Err(CommandBusError::InvalidInput(
                "the delivered command belongs to another stream or consumer",
            ));
        }
        let command = verified.command();
        if !bounded_text(&authority_delivery_id, MAX_IDENTITY_BYTES)
            || authority_identity.tenant_id != command.tenant_id
            || authority_identity.resource_project_id != command.resource_project_id
            || authority_identity.projection_project_id != command.projection_project_id
            || authority_identity.command_id != command.command_id
            || authority_identity.execution_id != command.execution_id
            || authority_identity.generation != command.generation
            || authority_delivery_id != command.idempotency_key
        {
            return Err(CommandBusError::AuthorizationFailed(
                "the terminal authority does not match the verified command",
            ));
        }
        if delivery.signed_envelope.as_slice() != authority_signed_envelope.as_ref()
            || delivery.signed_envelope.as_slice() != verified.exact_signed_envelope()
            || delivery.token != delivery_token(&command.idempotency_key)
        {
            return Err(CommandBusError::AuthorizationFailed(
                "the delivery does not match the verified command",
            ));
        }
        let settlement = Arc::clone(&delivery.settlement);
        let CommandDelivery { coordinates, .. } = delivery;
        self.client
            .retire_delivery(CommandRetirementRequest {
                stream: coordinates.stream,
                consumer: coordinates.consumer,
                subject: coordinates.subject,
                reply: coordinates.reply,
                stream_sequence: coordinates.stream_sequence,
            })
            .await?;
        settlement.mark_retired();
        Ok(())
    }
}

fn bounded_text(value: &str, max_bytes: usize) -> bool {
    !value.is_empty()
        && value.len() <= max_bytes
        && !value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
}

/// A delivery of `envelope` on the agent stream, on the subject its own
/// idempotency key names (or `fallback-delivery` when the bytes are not an
/// envelope). Test-only.
#[cfg(test)]
#[must_use]
pub(crate) fn test_agent_delivery(envelope: Vec<u8>) -> CommandDelivery {
    use prost::Message as _;

    use crate::protocol::elitea::runtime::v1::{SignedWorkerCommandEnvelopeV1, WorkerCommandV1};

    let idempotency_key = SignedWorkerCommandEnvelopeV1::decode(envelope.as_slice())
        .ok()
        .and_then(|signed| WorkerCommandV1::decode(signed.worker_command_bytes.as_slice()).ok())
        .map_or_else(
            || "fallback-delivery".to_owned(),
            |command| command.idempotency_key,
        );
    let subject = delivery_subject(STREAM_AGENT, &idempotency_key).unwrap_or_default();
    CommandDelivery::decode(
        &subject,
        TEST_AGENT_REPLY,
        envelope,
        CommandBusLimits::runtime_v1(),
    )
    .unwrap_or_else(|error| panic!("test delivery: {error}"))
}

/// An ack subject of the agent durable. Test-only.
#[cfg(test)]
pub(crate) const TEST_AGENT_REPLY: &str =
    "$JS.ACK.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1.1.7.7.1700000000000000000.0";

#[cfg(test)]
mod tests {
    use super::{
        AckReplyInfo, CommandBusLimits, CommandDelivery, DeadLetterRecord, DecodedDelivery,
        DeliveryVerdict, PoisonReason, decode_delivery, delivery_subject, delivery_token,
        filter_subject, valid_route_pair,
    };

    const REPLY: &str =
        "$JS.ACK.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1.3.17.9.1700000000000000000.0";

    #[test]
    fn delivery_token_matches_the_contract_vector() {
        assert_eq!(
            delivery_token("outbox-1"),
            "ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"
        );
        assert_eq!(
            delivery_subject("ELITEA_RT_V1_AGENT", "outbox-1").as_deref(),
            Some(
                "elitea.rt.v1.agent.d.ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"
            )
        );
        assert_eq!(
            filter_subject("ELITEA_RT_V1_VALIDATE").as_deref(),
            Some("elitea.rt.v1.validate.d.*")
        );
        assert!(filter_subject("ELITEA_RT_V1_OTHER").is_none());
    }

    #[test]
    fn only_contract_stream_durable_pairs_are_valid() {
        assert!(valid_route_pair(
            "ELITEA_RT_V1_VALIDATE",
            "elitea-configuration-worker-v1"
        ));
        assert!(valid_route_pair(
            "ELITEA_RT_V1_AGENT",
            "elitea-agent-worker-v1"
        ));
        assert!(valid_route_pair(
            "ELITEA_RT_V1_INDEX",
            "elitea-index-worker-v1"
        ));
        assert!(!valid_route_pair(
            "ELITEA_RT_V1_AGENT",
            "elitea-index-worker-v1"
        ));
        assert!(!valid_route_pair(
            "ELITEA_RT_V1_AGENT",
            "elitea-rust-workers"
        ));
        assert!(!valid_route_pair(
            "commands.v1.agent",
            "elitea-agent-worker-v1"
        ));
    }

    #[test]
    fn ack_reply_subjects_parse_in_both_server_forms() {
        let info = AckReplyInfo::parse(REPLY).expect("v1 ack subject");
        assert_eq!(info.stream, "ELITEA_RT_V1_AGENT");
        assert_eq!(info.consumer, "elitea-agent-worker-v1");
        assert_eq!(info.delivered, 3);
        assert_eq!(info.stream_sequence, 17);
        let v2 = "$JS.ACK._.ACCHASH.ELITEA_RT_V1_AGENT.elitea-agent-worker-v1.1.5.5.1700000000000000000.0.rand";
        let info = AckReplyInfo::parse(v2).expect("v2 ack subject");
        assert_eq!(info.stream_sequence, 5);
        for malformed in [
            "",
            "$JS.ACK.ELITEA_RT_V1_AGENT",
            "_INBOX.x.y",
            "$JS.ACK.S.C.x.1.1.1.0",
            "$JS.ACK.S.C.0.1.1.1.0",
        ] {
            assert!(AckReplyInfo::parse(malformed).is_err(), "{malformed}");
        }
    }

    #[test]
    fn decode_admits_only_contract_subjects_and_bounded_bodies() {
        let subject = delivery_subject("ELITEA_RT_V1_AGENT", "outbox-1").expect("subject");
        let delivery = CommandDelivery::decode(
            &subject,
            REPLY,
            b"signed".to_vec(),
            CommandBusLimits::runtime_v1(),
        )
        .expect("contract delivery");
        assert_eq!(delivery.stream(), "ELITEA_RT_V1_AGENT");
        assert_eq!(delivery.stream_sequence(), 17);
        assert_eq!(delivery.delivered(), 3);
        assert!(delivery.bind_idempotency_key("outbox-1").is_ok());
        assert_eq!(
            delivery.bind_idempotency_key("outbox-2"),
            Err(DeliveryVerdict::Poison {
                reason: PoisonReason::SubjectMismatch,
                delivery_token: Some(delivery_token("outbox-2")),
            })
        );

        let index_subject = delivery_subject("ELITEA_RT_V1_INDEX", "outbox-1").expect("subject");
        for (subject, reason) in [
            (index_subject.as_str(), PoisonReason::MalformedSubject),
            ("elitea.rt.v1.agent.d.UPPER", PoisonReason::MalformedSubject),
            (subject.as_str(), PoisonReason::MalformedMessage),
        ] {
            let payload = if reason == PoisonReason::MalformedMessage {
                vec![0_u8; 48 * 1024 + 1]
            } else {
                b"signed".to_vec()
            };
            match decode_delivery(
                subject,
                REPLY,
                payload,
                subject.len() + REPLY.len(),
                CommandBusLimits::runtime_v1(),
            )
            .expect("ackable message")
            {
                DecodedDelivery::Poison(poison) => assert_eq!(poison.reason, reason),
                DecodedDelivery::Command(_) => panic!("accepted a poison delivery"),
            }
        }
        let foreign =
            "$JS.ACK.ELITEA_RT_V1_AGENT.elitea-index-worker-v1.1.2.2.1700000000000000000.0";
        assert!(
            decode_delivery(
                &subject,
                foreign,
                b"x".to_vec(),
                1,
                CommandBusLimits::runtime_v1()
            )
            .is_err()
        );
        assert!(
            CommandBusLimits {
                max_message_bytes: 8,
                max_payload_bytes: 9
            }
            .validate()
            .is_err()
        );
    }

    #[test]
    fn dead_letter_records_name_where_to_look_and_never_the_command() {
        let subject = delivery_subject("ELITEA_RT_V1_AGENT", "outbox-1").expect("subject");
        let delivery = CommandDelivery::decode(
            &subject,
            REPLY,
            b"secret-envelope".to_vec(),
            CommandBusLimits::runtime_v1(),
        )
        .expect("delivery");
        let record = DeadLetterRecord::new(
            delivery.coordinates(),
            PoisonReason::SignatureInvalid,
            None,
            "worker-1",
            1_700_000_000_000,
        );
        assert_eq!(
            record.key,
            "agent.ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164"
        );
        let value: serde_json::Value =
            serde_json::from_slice(&record.to_json()).expect("record JSON");
        let object = value.as_object().expect("record object");
        let mut keys = object.keys().map(String::as_str).collect::<Vec<_>>();
        keys.sort_unstable();
        assert_eq!(
            keys,
            [
                "consumer",
                "num_delivered",
                "reason",
                "recorded_at_unix_millis",
                "schema",
                "stream",
                "stream_sequence",
                "subject",
                "worker"
            ]
        );
        assert_eq!(value["schema"], "elitea.runtime.dead-letter.v1");
        assert_eq!(value["reason"], "signature_invalid");
        assert_eq!(value["stream_sequence"], 17);
        assert_eq!(value["num_delivered"], 3);
        let text = String::from_utf8(record.to_json()).expect("utf-8");
        assert!(!text.contains("secret-envelope"));
        assert!(!text.contains("outbox-1"));

        let mismatch = DeadLetterRecord::new(
            delivery.coordinates(),
            PoisonReason::SubjectMismatch,
            Some(&delivery_token("outbox-2")),
            "worker-1",
            1,
        );
        assert_eq!(
            mismatch.key,
            format!("agent.{}", delivery_token("outbox-2"))
        );
    }
}
