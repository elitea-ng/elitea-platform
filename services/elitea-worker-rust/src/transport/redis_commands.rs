use std::fmt;

use async_trait::async_trait;

use crate::protocol::command::{
    VerifiedAgentCommand, VerifiedExecutionCommand, VerifiedToolkitExecuteReadCommand,
};
use crate::protocol::control::AgentCommandRetirementAuthority;

const SIGNED_ENVELOPE_FIELD: &[u8] = b"signed_envelope";
const MAX_STREAM_BYTES: usize = 512;
const MAX_IDENTITY_BYTES: usize = 256;
const MAX_ENTRY_ID_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RedisCommandLimits {
    pub max_entry_bytes: usize,
    pub max_field_bytes: usize,
}

impl RedisCommandLimits {
    /// Validate the post-RESP command bounds used by the dedicated Redis plane.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration error for zero or inverted limits.
    pub fn validate(self) -> Result<Self, RedisCommandError> {
        if self.max_entry_bytes == 0
            || self.max_field_bytes == 0
            || self.max_field_bytes > self.max_entry_bytes
        {
            return Err(RedisCommandError::InvalidInput(
                "the Redis command limits are malformed",
            ));
        }
        Ok(self)
    }
}

/// Exact binary Redis Stream entry admitted for signed-command verification.
///
/// The value is intentionally non-`Clone` and non-`Debug`: retirement must
/// retain the same bounded bytes that were verified and must not log them.
pub struct RedisCommandDelivery {
    stream: String,
    entry_id: String,
    signed_envelope: Vec<u8>,
}

impl RedisCommandDelivery {
    /// Strictly decode one Redis entry while preserving duplicate field names.
    ///
    /// This is a post-RESP bound. Production composition must additionally use
    /// a dedicated TLS/ACL Redis plane and producer-side entry admission.
    ///
    /// # Errors
    ///
    /// Returns a typed malformed or resource-limit error without retaining
    /// unknown fields.
    pub fn decode<I>(
        stream: &[u8],
        entry_id: &[u8],
        fields: I,
        limits: RedisCommandLimits,
    ) -> Result<Self, RedisCommandError>
    where
        I: IntoIterator<Item = (Vec<u8>, Vec<u8>)>,
    {
        let limits = limits.validate()?;
        let stream = decode_bounded_text(stream, MAX_STREAM_BYTES).ok_or(
            RedisCommandError::InvalidInput("the Redis command stream is malformed"),
        )?;
        let entry_id = decode_entry_id(entry_id)?;
        let mut signed_envelope = None;
        let mut encoded_bytes = entry_id.len();
        for (name, value) in fields {
            if name.as_slice() != SIGNED_ENVELOPE_FIELD || signed_envelope.is_some() {
                return Err(RedisCommandError::InvalidInput(
                    "the Redis command contains an unregistered or duplicate field",
                ));
            }
            if name.len() > limits.max_field_bytes || value.len() > limits.max_field_bytes {
                return Err(RedisCommandError::ResourceExhausted(
                    "a Redis command field exceeds the approved limit",
                ));
            }
            encoded_bytes = encoded_bytes
                .checked_add(name.len())
                .and_then(|total| total.checked_add(value.len()))
                .ok_or(RedisCommandError::ResourceExhausted(
                    "the Redis command exceeds the approved limit",
                ))?;
            if encoded_bytes > limits.max_entry_bytes {
                return Err(RedisCommandError::ResourceExhausted(
                    "the Redis command exceeds the approved limit",
                ));
            }
            signed_envelope = Some(value);
        }
        let signed_envelope = signed_envelope.ok_or(RedisCommandError::InvalidInput(
            "the Redis command is missing its signed envelope",
        ))?;
        Ok(Self {
            stream,
            entry_id,
            signed_envelope,
        })
    }

    #[must_use]
    pub fn signed_envelope(&self) -> &[u8] {
        &self.signed_envelope
    }

    #[must_use]
    pub fn stream(&self) -> &str {
        &self.stream
    }

    #[must_use]
    pub fn entry_id(&self) -> &str {
        &self.entry_id
    }
}

#[derive(Clone, Eq, PartialEq)]
pub struct RedisRetirementConfig {
    pub stream: String,
    pub group: String,
    pub consumer: String,
}

impl RedisRetirementConfig {
    fn validate(&self) -> bool {
        bounded_text(&self.stream, MAX_STREAM_BYTES)
            && self.stream.is_ascii()
            && bounded_text(&self.group, MAX_IDENTITY_BYTES)
            && bounded_text(&self.consumer, MAX_IDENTITY_BYTES)
    }
}

/// Restricted atomic-retirement request for a transport implementation.
pub struct RedisRetirementRequest {
    stream: String,
    group: String,
    consumer: String,
    entry_id: String,
    stable_delivery_id: String,
    signed_envelope: Vec<u8>,
}

impl RedisRetirementRequest {
    #[must_use]
    pub fn stream(&self) -> &str {
        &self.stream
    }

    #[must_use]
    pub fn group(&self) -> &str {
        &self.group
    }

    #[must_use]
    pub fn consumer(&self) -> &str {
        &self.consumer
    }

    #[must_use]
    pub fn entry_id(&self) -> &str {
        &self.entry_id
    }

    #[must_use]
    pub fn stable_delivery_id(&self) -> &str {
        &self.stable_delivery_id
    }

    #[must_use]
    pub fn signed_envelope(&self) -> &[u8] {
        &self.signed_envelope
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RedisRetirementResponse {
    pub acknowledged: i64,
    pub deleted: i64,
    pub unmapped: i64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RedisRetirementClientError {
    Authentication,
    DependencyUnavailable,
    Timeout,
    Protocol,
}

impl fmt::Display for RedisRetirementClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        let message = match self {
            Self::Authentication => "Redis command retirement authentication failed",
            Self::DependencyUnavailable => "Redis command retirement is unavailable",
            Self::Timeout => "Redis command retirement timed out",
            Self::Protocol => "Redis command retirement returned a malformed response",
        };
        formatter.write_str(message)
    }
}

impl std::error::Error for RedisRetirementClientError {}

#[async_trait]
pub trait RedisRetirementClient: Send + Sync {
    async fn retire_delivery(
        &self,
        request: RedisRetirementRequest,
    ) -> Result<RedisRetirementResponse, RedisRetirementClientError>;
}

#[derive(Debug)]
pub enum RedisCommandError {
    InvalidInput(&'static str),
    ResourceExhausted(&'static str),
    AuthorizationFailed(&'static str),
    DependencyUnavailable(&'static str),
    Client(RedisRetirementClientError),
}

impl RedisCommandError {
    /// Stable low-cardinality category for operator logs and metrics.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InvalidInput(_) => "redis_command.invalid_input",
            Self::ResourceExhausted(_) => "redis_command.resource_exhausted",
            Self::AuthorizationFailed(_) => "redis_command.authorization_failed",
            Self::DependencyUnavailable(_)
            | Self::Client(
                RedisRetirementClientError::DependencyUnavailable
                | RedisRetirementClientError::Timeout,
            ) => "redis_command.dependency_unavailable",
            Self::Client(RedisRetirementClientError::Authentication) => {
                "redis_command.authentication_failed"
            }
            Self::Client(RedisRetirementClientError::Protocol) => "redis_command.protocol_failure",
        }
    }

    /// Whether the same immutable retirement operation may succeed later.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self,
            Self::DependencyUnavailable(_)
                | Self::Client(
                    RedisRetirementClientError::DependencyUnavailable
                        | RedisRetirementClientError::Timeout
                )
        )
    }
}

impl fmt::Display for RedisCommandError {
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

impl std::error::Error for RedisCommandError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Client(error) => Some(error),
            _ => None,
        }
    }
}

impl From<RedisRetirementClientError> for RedisCommandError {
    fn from(value: RedisRetirementClientError) -> Self {
        Self::Client(value)
    }
}

/// The only path that converts a durable agent terminal proof into Redis
/// command retirement. No arbitrary ACK/delete/publish operation is exposed.
pub struct RedisCommandRetirer<C> {
    client: C,
    config: RedisRetirementConfig,
}

impl<C> RedisCommandRetirer<C> {
    /// Bind the restricted client to one stream/group/consumer identity.
    ///
    /// # Errors
    ///
    /// Returns a stable error for empty, oversized, or control-bearing names.
    pub fn new(client: C, config: RedisRetirementConfig) -> Result<Self, RedisCommandError> {
        if !config.validate() {
            return Err(RedisCommandError::InvalidInput(
                "the Redis retirement identity is malformed",
            ));
        }
        Ok(Self { client, config })
    }
}

impl<C: RedisRetirementClient> RedisCommandRetirer<C> {
    /// Atomically ACK, delete, and unmap one exact verified settled command.
    ///
    /// The consuming authority guarantees that output/settlement or a
    /// state-owner terminal redelivery decision happened first.
    ///
    /// # Errors
    ///
    /// Returns a typed binding, client, or unconfirmed-retirement error.
    pub async fn retire_agent_command(
        &self,
        delivery: RedisCommandDelivery,
        verified: &VerifiedAgentCommand,
        authority: AgentCommandRetirementAuthority,
    ) -> Result<(), RedisCommandError> {
        self.retire_command(delivery, verified, authority).await
    }

    pub(crate) async fn retire_toolkit_execute_read_command(
        &self,
        delivery: RedisCommandDelivery,
        verified: &VerifiedToolkitExecuteReadCommand,
        authority: AgentCommandRetirementAuthority,
    ) -> Result<(), RedisCommandError> {
        self.retire_command(delivery, verified, authority).await
    }

    async fn retire_command(
        &self,
        delivery: RedisCommandDelivery,
        verified: &impl VerifiedExecutionCommand,
        authority: AgentCommandRetirementAuthority,
    ) -> Result<(), RedisCommandError> {
        let (authority_identity, authority_delivery_id, authority_signed_envelope) =
            authority.into_binding();
        if delivery.stream != self.config.stream {
            return Err(RedisCommandError::InvalidInput(
                "the delivered command belongs to another Redis stream",
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
            return Err(RedisCommandError::AuthorizationFailed(
                "the terminal authority does not match the verified command",
            ));
        }
        if delivery.signed_envelope.as_slice() != authority_signed_envelope.as_ref()
            || delivery.signed_envelope.as_slice() != verified.exact_signed_envelope()
        {
            return Err(RedisCommandError::AuthorizationFailed(
                "the Redis delivery does not match the verified command",
            ));
        }
        let response = self
            .client
            .retire_delivery(RedisRetirementRequest {
                stream: self.config.stream.clone(),
                group: self.config.group.clone(),
                consumer: self.config.consumer.clone(),
                entry_id: delivery.entry_id,
                stable_delivery_id: command.idempotency_key.clone(),
                signed_envelope: delivery.signed_envelope,
            })
            .await?;
        if !matches!(
            (response.acknowledged, response.deleted, response.unmapped),
            (1, 1, 1) | (2, 0, 0)
        ) {
            return Err(RedisCommandError::DependencyUnavailable(
                "Redis did not confirm atomic command retirement",
            ));
        }
        Ok(())
    }
}

pub(super) fn decode_entry_id(value: &[u8]) -> Result<String, RedisCommandError> {
    let value = std::str::from_utf8(value).map_err(|_| {
        RedisCommandError::InvalidInput("the Redis command entry identity is malformed")
    })?;
    if value.is_empty() || value.len() > MAX_ENTRY_ID_BYTES {
        return Err(RedisCommandError::InvalidInput(
            "the Redis command entry identity is malformed",
        ));
    }
    let Some((timestamp, sequence)) = value.split_once('-') else {
        return Err(RedisCommandError::InvalidInput(
            "the Redis command entry identity is malformed",
        ));
    };
    if timestamp.is_empty()
        || sequence.is_empty()
        || !timestamp.bytes().all(|byte| byte.is_ascii_digit())
        || !sequence.bytes().all(|byte| byte.is_ascii_digit())
    {
        return Err(RedisCommandError::InvalidInput(
            "the Redis command entry identity is malformed",
        ));
    }
    Ok(value.to_owned())
}

fn decode_bounded_text(value: &[u8], max_bytes: usize) -> Option<String> {
    let value = std::str::from_utf8(value).ok()?;
    (value.is_ascii() && bounded_text(value, max_bytes)).then(|| value.to_owned())
}

fn bounded_text(value: &str, max_bytes: usize) -> bool {
    !value.is_empty()
        && value.len() <= max_bytes
        && !value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
}
