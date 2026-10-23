//! Claim-bound, bounded input materialization over Main's runtime content
//! service (`elitea.runtime.v1.RuntimeContentService`).
//!
//! The content channel is distinct from control gRPC and the command bus. Production
//! construction requires the private CA and worker identity, then creates its
//! own tonic [`Channel`] from the same validated origin used for hostname
//! verification. Every claim-scoped call carries the claim id and the raw
//! fence token as gRPC metadata (`claim_request`), never in the message.

use std::fmt;
use std::time::Duration;

use ring::digest;
use subtle::ConstantTimeEq;
use tokio::time::timeout;
use tonic::body::Body;
use tonic::metadata::{AsciiMetadataValue, BinaryMetadataValue};
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};
use tonic::{Code, Status, Streaming};
use tower::ServiceExt as _;
use zeroize::Zeroizing;

use crate::protocol::control::{
    ClaimBoundInputAuthority, LeaseMonitoredAgentExecution, LiveModelCheckpointInspection,
};
use crate::protocol::elitea::runtime::v1::runtime_content_service_client::RuntimeContentServiceClient;
use crate::protocol::elitea::runtime::v1::{
    GetInputRequestV1, GetInputResponseV1, InputContentHeaderV1, RuntimeContentScopeV1,
    get_input_response_v1::Part,
};

#[path = "code_intent_content.rs"]
mod code_intent_content;
#[path = "code_platform_content.rs"]
pub(crate) mod code_platform_content;
#[path = "code_workspace_content.rs"]
mod code_workspace_content;
#[cfg(test)]
pub(crate) use code_intent_content::actual_original_code_visit_fixture;

#[path = "fanout_interrupts_content.rs"]
mod fanout_interrupts_content;
#[path = "node_recovery_content.rs"]
mod node_recovery_content;
#[path = "toolkit_discovery_artifact.rs"]
mod toolkit_discovery_artifact;
pub(crate) use fanout_interrupts_content::{FanoutAckRequest, FanoutDecisionAuthority};
pub(crate) use node_recovery_content::{
    NodeRecoveryAckReply, receipt_sha256 as node_recovery_receipt_sha256,
};

#[cfg(test)]
#[path = "content_fake.rs"]
pub(crate) mod content_fake;
#[cfg(test)]
#[path = "content_grpc_tests.rs"]
mod content_grpc_tests;

const MAX_SAFE_TEXT_BYTES: usize = 256;
const MAX_ORIGIN_BYTES: usize = 2048;
const MAX_MATERIALIZED_INPUT_BYTES: usize = 8 * 1024 * 1024;
const MAX_INPUT_DEADLINE: Duration = Duration::from_mins(5);
/// The accepted claim's id, one ASCII metadata value.
pub(crate) const CLAIM_METADATA: &str = "elitea-claim-id";
/// The raw 32-byte fence token, one binary metadata value.
pub(crate) const FENCE_METADATA: &str = "elitea-claim-fence-bin";
/// One content message's ceiling in either direction. tonic decodes at most
/// 4 MiB by default and an attachment envelope reaches 6 MiB.
pub(crate) const MAX_CONTENT_MESSAGE_BYTES: usize = 16 << 20;

/// The service under the generated content client: the mTLS channel in
/// production, an in-process server in tests. Either way every call is
/// encoded, framed and answered as real gRPC.
pub(crate) type ContentTransport = tower::util::BoxCloneSyncService<
    http::Request<Body>,
    http::Response<Body>,
    ContentTransportError,
>;

/// The failure of the service under the generated client, as one concrete
/// error type. (A boxed `dyn Error` here would leave the generated client's
/// `Into<StdError>` bound higher-ranked, which no `Send` future satisfies.)
#[derive(Debug)]
pub(crate) struct ContentTransportError(tonic::codegen::StdError);

impl fmt::Display for ContentTransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

impl std::error::Error for ContentTransportError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(self.0.as_ref())
    }
}

/// The generated `RuntimeContentService` client every worker-side content
/// call goes through.
pub(crate) type ContentRpc = RuntimeContentServiceClient<ContentTransport>;

/// The generated client over `transport`, with the content message caps.
pub(crate) fn content_rpc(transport: ContentTransport) -> ContentRpc {
    RuntimeContentServiceClient::new(transport)
        .max_decoding_message_size(MAX_CONTENT_MESSAGE_BYTES)
        .max_encoding_message_size(MAX_CONTENT_MESSAGE_BYTES)
}

/// The production transport: one connected channel to the content origin.
pub(crate) fn channel_transport(channel: Channel) -> ContentTransport {
    ContentTransport::new(channel.map_err(|error| ContentTransportError(Box::new(error))))
}

/// One claim-scoped content message with the claim's metadata attached: the
/// claim id as ASCII and the fence token as raw binary metadata. `None` when
/// the claim id cannot be a metadata value.
pub(crate) fn claim_request<T>(
    message: T,
    claim_id: &str,
    fence: &[u8],
) -> Option<tonic::Request<T>> {
    let claim = AsciiMetadataValue::try_from(claim_id).ok()?;
    let mut request = tonic::Request::new(message);
    request.metadata_mut().insert(CLAIM_METADATA, claim);
    request
        .metadata_mut()
        .insert_bin(FENCE_METADATA, BinaryMetadataValue::from_bytes(fence));
    Some(request)
}

/// The execution and generation a claim-scoped call is made under.
pub(crate) fn content_scope(execution_id: &str, generation: u64) -> RuntimeContentScopeV1 {
    RuntimeContentScopeV1 {
        execution_id: execution_id.to_owned(),
        generation,
    }
}

/// Whether a failed call is the channel's failure rather than a handler's
/// answer: no connection, a reset stream, or an overloaded peer. Every such
/// status is handled as the connection failure it replaced.
pub(crate) fn is_transport_status(status: &Status) -> bool {
    matches!(
        status.code(),
        Code::Unavailable | Code::Unknown | Code::Cancelled
    )
}

/// Sensitive materialized bytes which are erased when parsing releases them.
///
/// The value deliberately has no `Clone` or `Debug` implementation. Callers
/// borrow its bytes for canonical protocol parsing instead of extracting an
/// unprotected `Vec`.
pub struct MaterializedInput(Zeroizing<Vec<u8>>);

impl MaterializedInput {
    #[must_use]
    pub fn as_bytes(&self) -> &[u8] {
        self.0.as_slice()
    }

    #[cfg(test)]
    pub(crate) fn for_test(bytes: Vec<u8>) -> Self {
        Self(Zeroizing::new(bytes))
    }
}

/// Immutable request policy for one dedicated content channel.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InputContentConfig {
    pub origin: String,
    pub deadline: Duration,
    pub max_materialized_bytes: usize,
}

impl InputContentConfig {
    fn validate(&self) -> Result<String, InputContentError> {
        let origin = canonical_https_origin(&self.origin)?;
        if self.deadline.is_zero()
            || self.deadline > MAX_INPUT_DEADLINE
            || self.max_materialized_bytes == 0
            || self.max_materialized_bytes > MAX_MATERIALIZED_INPUT_BYTES
        {
            return Err(InputContentError::InvalidConfiguration(
                "the input content configuration is malformed",
            ));
        }
        Ok(origin)
    }
}

/// Stable failures which never include response bodies, URLs, or authority.
///
/// `Display` and [`Self::code`] are safe at the runtime boundary. A production
/// transport failure retains its internal source chain for redacted operator
/// diagnostics; that source chain must not be serialized to callers.
#[derive(Debug)]
pub enum InputContentError {
    InvalidConfiguration(&'static str),
    InvalidInput(&'static str),
    ResourceExhausted(&'static str),
    AuthorizationFailed(&'static str),
    DependencyUnavailable(&'static str),
    Transport(InputContentTransportError),
    Timeout(&'static str),
}

impl InputContentError {
    /// Stable low-cardinality code suitable for terminal error mapping and
    /// structured operator logs.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InvalidConfiguration(_) => "input_content.invalid_configuration",
            Self::InvalidInput(_) => "input_content.invalid_response",
            Self::ResourceExhausted(_) => "input_content.resource_exhausted",
            Self::AuthorizationFailed(_) => "input_content.authorization_failed",
            Self::DependencyUnavailable(_) | Self::Transport(_) => {
                "input_content.dependency_unavailable"
            }
            Self::Timeout(_) => "input_content.timeout",
        }
    }

    /// Whether a fresh delivery attempt may succeed without changing the
    /// admitted command or input descriptor.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self,
            Self::DependencyUnavailable(_) | Self::Transport(_) | Self::Timeout(_)
        )
    }

    /// The channel's failure of one call (see [`is_transport_status`]).
    pub(crate) fn transport(status: Status) -> Self {
        Self::Transport(InputContentTransportError::Status(Box::new(status)))
    }
}

impl fmt::Display for InputContentError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message)
            | Self::InvalidInput(message)
            | Self::ResourceExhausted(message)
            | Self::AuthorizationFailed(message)
            | Self::DependencyUnavailable(message)
            | Self::Timeout(message) => formatter.write_str(message),
            Self::Transport(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for InputContentError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Transport(error) => Some(error),
            _ => None,
        }
    }
}

/// Stable transport failure below semantic response validation.
#[derive(Debug)]
pub enum InputContentTransportError {
    Unavailable,
    Tonic(tonic::transport::Error),
    /// A call the channel failed; the status keeps its source chain.
    Status(Box<Status>),
}

impl fmt::Display for InputContentTransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Unavailable | Self::Tonic(_) | Self::Status(_) => {
                formatter.write_str("the input content service is unavailable")
            }
        }
    }
}

impl std::error::Error for InputContentTransportError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Unavailable => None,
            Self::Tonic(error) => Some(error),
            Self::Status(status) => Some(status.as_ref()),
        }
    }
}

/// One-attempt materialized input client over a dedicated origin-bound
/// content channel.
pub struct InputContentClient {
    rpc: ContentRpc,
    config: InputContentConfig,
}

impl InputContentClient {
    /// Build and connect the dedicated mTLS channel from the validated
    /// origin. The caller cannot substitute a preconnected mismatched channel.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration, timeout, or transport error when the
    /// bounded HTTPS endpoint cannot be connected with the supplied TLS trust
    /// and client identity.
    pub async fn connect(
        config: InputContentConfig,
        private_ca: Certificate,
        client_identity: Identity,
    ) -> Result<Self, InputContentError> {
        let origin = config.validate()?;
        let tls = ClientTlsConfig::new()
            .ca_certificate(private_ca)
            .identity(client_identity);
        let endpoint = Endpoint::from_shared(origin)
            .map(crate::transport::keepalive::with_keepalive)
            .and_then(|endpoint| endpoint.tls_config(tls))
            .map_err(InputContentTransportError::Tonic)
            .map_err(InputContentError::Transport)?;
        let channel = timeout(config.deadline, endpoint.connect())
            .await
            .map_err(|_| InputContentError::Timeout("the input content connection timed out"))?
            .map_err(InputContentTransportError::Tonic)
            .map_err(InputContentError::Transport)?;
        Ok(Self {
            rpc: content_rpc(channel_transport(channel)),
            config,
        })
    }

    /// A client over an in-process content service.
    #[cfg(test)]
    pub(crate) fn with_rpc(
        fake: impl content_fake::ContentFake,
        config: InputContentConfig,
    ) -> Result<Self, InputContentError> {
        config.validate()?;
        Ok(Self {
            rpc: content_rpc(content_fake::in_process(fake)),
            config,
        })
    }

    /// A fresh handle on the shared channel for one call.
    fn rpc(&self) -> ContentRpc {
        self.rpc.clone()
    }

    /// Fetch exactly one claim-bound, source-bound materialized agent input.
    ///
    /// The opaque execution state proves successful `BeginExecution` and
    /// unique lease-monitor ownership. Raw claim IDs and fence tokens are never
    /// accepted through this public API.
    ///
    /// The method performs one network attempt and validates the stream's
    /// header, source identity, length bounds, and the response SHA-256 before
    /// returning bytes.
    ///
    /// # Errors
    ///
    /// Returns a data-free typed error for transport, timeout, authorization,
    /// response-shape, digest, or resource-limit failure.
    pub async fn fetch_materialized(
        &self,
        execution: &LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError> {
        let reference =
            execution
                .input_content_authority()
                .ok_or(InputContentError::InvalidInput(
                    "the sealed input authority is malformed",
                ))?;
        self.fetch_authority(reference).await
    }

    /// Fetch one entry selected from the validated claim manifest.
    ///
    /// # Errors
    /// Returns a typed error if the entry is unbound or its content cannot be verified.
    pub async fn fetch_materialized_entry(
        &self,
        execution: &LeaseMonitoredAgentExecution,
        entry_id: &str,
    ) -> Result<MaterializedInput, InputContentError> {
        let reference = execution
            .input_content_authority_for_entry(entry_id)
            .ok_or(InputContentError::InvalidInput(
                "the selected input authority is absent",
            ))?;
        self.fetch_authority(reference).await
    }

    /// Materialize the frozen request under a supervised checkpoint inspection.
    /// This grants no model invocation or runtime credential authority.
    #[allow(dead_code)] // Recovery dispatch remains disabled until output replacement is wired.
    pub(crate) async fn fetch_checkpoint_request(
        &self,
        inspection: &LiveModelCheckpointInspection,
    ) -> Result<MaterializedInput, InputContentError> {
        let reference =
            inspection
                .input_content_authority()
                .ok_or(InputContentError::InvalidInput(
                    "the checkpoint input authority is malformed",
                ))?;
        self.fetch_authority(reference).await
    }

    /// Only the isolated suspended inspection path calls this read-only fetch.
    pub(crate) async fn fetch_node_recovery_authority(
        &self,
        reference: ClaimBoundInputAuthority<'_>,
    ) -> Result<MaterializedInput, InputContentError> {
        self.fetch_authority(reference).await
    }

    async fn fetch_authority(
        &self,
        reference: ClaimBoundInputAuthority<'_>,
    ) -> Result<MaterializedInput, InputContentError> {
        validate_reference(&reference)?;
        if reference.expected_source_length > self.config.max_materialized_bytes as u64 {
            return Err(InputContentError::ResourceExhausted(
                "the admitted input source exceeds the configured deployment limit",
            ));
        }
        let request = input_request(&reference)?;
        let mut rpc = self.rpc();
        let max_bytes = self.config.max_materialized_bytes;
        let operation = async {
            let stream = rpc
                .get_input(request)
                .await
                .map_err(input_status)?
                .into_inner();
            collect_input(stream, &reference, max_bytes).await
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| InputContentError::Timeout("the input content request timed out"))?
    }

    #[cfg(test)]
    pub(super) async fn fetch_test_authority(
        &self,
        reference: ClaimBoundInputAuthority<'_>,
    ) -> Result<MaterializedInput, InputContentError> {
        self.fetch_authority(reference).await
    }
}

fn validate_reference(reference: &ClaimBoundInputAuthority<'_>) -> Result<(), InputContentError> {
    if !bounded_text(reference.execution_id)
        || reference.generation == 0
        || reference.generation > i64::MAX as u64
        || !bounded_text(reference.content_id)
        || !bounded_text(reference.immutable_version)
        || !bounded_text(reference.claim_id)
        || reference.fence_token.len() != 32
        || reference.fence_token.iter().all(|byte| *byte == 0)
        || reference.expected_source_length == 0
        || reference.expected_source_length > MAX_MATERIALIZED_INPUT_BYTES as u64
        || reference.expected_source_sha256.len() != 32
        || reference
            .expected_source_sha256
            .iter()
            .all(|byte| *byte == 0)
        || !supported_media_type(reference.media_type)
    {
        return Err(InputContentError::InvalidInput(
            "the sealed input authority is malformed",
        ));
    }
    Ok(())
}

/// The claim-scoped request naming one admitted input.
fn input_request(
    reference: &ClaimBoundInputAuthority<'_>,
) -> Result<tonic::Request<GetInputRequestV1>, InputContentError> {
    claim_request(
        GetInputRequestV1 {
            scope: Some(content_scope(reference.execution_id, reference.generation)),
            content_id: reference.content_id.to_owned(),
            immutable_version: reference.immutable_version.to_owned(),
        },
        reference.claim_id,
        reference.fence_token,
    )
    .ok_or(InputContentError::InvalidInput(
        "the input content request authority is malformed",
    ))
}

/// What a refused or failed `GetInput` means, by status code. The codes are
/// the ones Main's content service maps its outcomes to; anything the
/// channel produced is the connection failure it always was.
fn input_status(status: Status) -> InputContentError {
    match status.code() {
        Code::Unauthenticated | Code::PermissionDenied => {
            InputContentError::AuthorizationFailed("the claim-bound input grant was rejected")
        }
        Code::NotFound | Code::Unimplemented => {
            InputContentError::InvalidInput("the admitted input content was not found")
        }
        Code::ResourceExhausted => InputContentError::ResourceExhausted(
            "the admitted input exceeds the content service limit",
        ),
        Code::InvalidArgument | Code::FailedPrecondition => {
            InputContentError::InvalidInput("the input content service rejected materialization")
        }
        Code::DeadlineExceeded => {
            InputContentError::Timeout("the input content transfer was interrupted")
        }
        _ if is_transport_status(&status) => InputContentError::transport(status),
        _ => InputContentError::DependencyUnavailable(
            "the input content service did not accept the request",
        ),
    }
}

struct ValidatedInputHeader {
    response_digest: [u8; 32],
    declared_length: usize,
}

/// Assemble one `GetInput` stream: exactly one header, then chunks that fill
/// exactly the declared length and hash to the declared digest.
async fn collect_input(
    mut stream: Streaming<GetInputResponseV1>,
    reference: &ClaimBoundInputAuthority<'_>,
    max_bytes: usize,
) -> Result<MaterializedInput, InputContentError> {
    let Some(GetInputResponseV1 {
        part: Some(Part::Header(header)),
    }) = stream.message().await.map_err(input_status)?
    else {
        return Err(InputContentError::InvalidInput(
            "the input content stream did not open with its header",
        ));
    };
    let validated = validate_input_header(&header, reference, max_bytes)?;
    let mut body = Zeroizing::new(Vec::with_capacity(validated.declared_length));
    while let Some(message) = stream.message().await.map_err(input_status)? {
        let Some(Part::Chunk(chunk)) = message.part else {
            return Err(InputContentError::InvalidInput(
                "the input content stream carried more than chunks after its header",
            ));
        };
        let chunk = Zeroizing::new(chunk);
        append_bounded(&mut body, &chunk, validated.declared_length)?;
    }
    if body.len() != validated.declared_length {
        return Err(InputContentError::InvalidInput(
            "the input content length does not match its response",
        ));
    }
    let actual = digest::digest(&digest::SHA256, &body);
    if actual
        .as_ref()
        .ct_eq(&validated.response_digest)
        .unwrap_u8()
        != 1
    {
        return Err(InputContentError::InvalidInput(
            "the input content does not match its immutable response",
        ));
    }
    Ok(MaterializedInput(body))
}

/// The header restates the admitted descriptor; nothing is trusted until it
/// matches.
fn validate_input_header(
    header: &InputContentHeaderV1,
    reference: &ClaimBoundInputAuthority<'_>,
    max_bytes: usize,
) -> Result<ValidatedInputHeader, InputContentError> {
    let response_digest = digest_field(&header.sha256)?;
    let source_digest = digest_field(&header.source_sha256)?;
    if source_digest
        .ct_eq(reference.expected_source_sha256)
        .unwrap_u8()
        != 1
    {
        return Err(InputContentError::AuthorizationFailed(
            "the materialized input source digest does not match the admitted descriptor",
        ));
    }
    if header.source_length == 0 {
        return Err(InputContentError::InvalidInput(
            "the input content response length is malformed",
        ));
    }
    if header.source_length != reference.expected_source_length {
        return Err(InputContentError::AuthorizationFailed(
            "the materialized input source length does not match the admitted descriptor",
        ));
    }
    if header.source_immutable_version.as_bytes() != reference.immutable_version.as_bytes() {
        return Err(InputContentError::AuthorizationFailed(
            "the materialized input source version does not match the admitted descriptor",
        ));
    }
    let media = header.media_type.as_str();
    if !media.eq_ignore_ascii_case(reference.media_type)
        && media
            .split(';')
            .next()
            .map(str::trim)
            .is_none_or(|value| !value.eq_ignore_ascii_case(reference.media_type))
    {
        return Err(InputContentError::InvalidInput(
            "the input content media type is malformed",
        ));
    }
    if header.length == 0 {
        return Err(InputContentError::InvalidInput(
            "the input content response length is malformed",
        ));
    }
    let declared_length = usize::try_from(header.length)
        .ok()
        .filter(|length| *length <= max_bytes)
        .ok_or(InputContentError::ResourceExhausted(
            "the materialized input exceeds its approved limit",
        ))?;
    Ok(ValidatedInputHeader {
        response_digest,
        declared_length,
    })
}

fn digest_field(value: &[u8]) -> Result<[u8; 32], InputContentError> {
    value.try_into().map_err(|_| {
        InputContentError::InvalidInput("the input content response digest is malformed")
    })
}

/// A chunk past the declared length is a protocol violation, not a large
/// input: the header already bounded the size.
fn append_bounded(
    body: &mut Vec<u8>,
    chunk: &[u8],
    declared_length: usize,
) -> Result<(), InputContentError> {
    if body
        .len()
        .checked_add(chunk.len())
        .is_none_or(|length| length > declared_length)
    {
        return Err(InputContentError::InvalidInput(
            "the input content length does not match its response",
        ));
    }
    body.extend_from_slice(chunk);
    Ok(())
}

fn canonical_https_origin(value: &str) -> Result<String, InputContentError> {
    if value.is_empty() || value.len() > MAX_ORIGIN_BYTES {
        return Err(InputContentError::InvalidConfiguration(
            "the input content origin is malformed",
        ));
    }
    let uri = value.parse::<http::Uri>().map_err(|_| {
        InputContentError::InvalidConfiguration("the input content origin is malformed")
    })?;
    if uri.scheme_str() != Some("https")
        || uri.authority().is_none()
        || uri.path() != "/"
        || uri.query().is_some()
        || uri.authority().is_some_and(|authority| {
            authority.as_str().contains('@') || !authority.as_str().is_ascii()
        })
    {
        return Err(InputContentError::InvalidConfiguration(
            "the input content origin is malformed",
        ));
    }
    let authority = uri
        .authority()
        .ok_or(InputContentError::InvalidConfiguration(
            "the input content origin is malformed",
        ))?;
    let host = authority.host();
    if host.is_empty() {
        return Err(InputContentError::InvalidConfiguration(
            "the input content origin is malformed",
        ));
    }
    let host = host.to_ascii_lowercase();
    let host = if host.contains(':') {
        format!("[{host}]")
    } else {
        host
    };
    match authority.port_u16() {
        Some(443) | None => Ok(format!("https://{host}")),
        Some(port) => Ok(format!("https://{host}:{port}")),
    }
}

fn bounded_text(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_SAFE_TEXT_BYTES
        && !value.bytes().any(|byte| matches!(byte, b'\r' | b'\n' | 0))
}

fn supported_media_type(value: &str) -> bool {
    matches!(
        value,
        "application/json"
            | "application/json; charset=utf-8"
            | "application/vnd.elitea.agent-execution-input.v1+protobuf"
            | "application/vnd.elitea.toolkit-execute-read-input.v1+protobuf"
    )
}
