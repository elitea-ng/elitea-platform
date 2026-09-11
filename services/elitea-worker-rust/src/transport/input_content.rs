//! Claim-bound, bounded HTTP/2 input materialization.
//!
//! The content channel is distinct from control gRPC and Redis. Production
//! construction requires the private CA and worker identity, then creates its
//! own tonic [`Channel`] from the same validated origin used for hostname
//! verification.

use std::fmt;
use std::time::Duration;

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use bytes::Bytes;
use http::header::{CACHE_CONTROL, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderMap, HeaderName, HeaderValue, Method, Request, Response, StatusCode, Version};
use http_body_util::BodyExt as _;
use percent_encoding::{AsciiSet, CONTROLS, utf8_percent_encode};
use ring::digest;
use subtle::ConstantTimeEq;
use tokio::time::timeout;
use tonic::body::Body;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};
use tower::ServiceExt as _;
use zeroize::Zeroizing;

use crate::protocol::control::{ClaimBoundInputAuthority, LeaseMonitoredAgentExecution};

#[path = "toolkit_discovery_artifact.rs"]
mod toolkit_discovery_artifact;

const MAX_SAFE_TEXT_BYTES: usize = 256;
const MAX_ORIGIN_BYTES: usize = 2048;
const MAX_MATERIALIZED_INPUT_BYTES: usize = 1024 * 1024;
const MAX_INPUT_DEADLINE: Duration = Duration::from_mins(5);
const CLAIM_HEADER: HeaderName = HeaderName::from_static("x-elitea-claim-id");
const FENCE_HEADER: HeaderName = HeaderName::from_static("x-elitea-fence");
const CONTENT_DIGEST_HEADER: HeaderName = HeaderName::from_static("content-digest");
const SOURCE_DIGEST_HEADER: HeaderName = HeaderName::from_static("x-elitea-source-content-digest");
const SOURCE_LENGTH_HEADER: HeaderName = HeaderName::from_static("x-elitea-source-content-length");
const SOURCE_VERSION_HEADER: HeaderName =
    HeaderName::from_static("x-elitea-source-immutable-version");
const PATH_SEGMENT: &AsciiSet = &CONTROLS
    .add(b' ')
    .add(b'!')
    .add(b'"')
    .add(b'#')
    .add(b'$')
    .add(b'%')
    .add(b'&')
    .add(b'\'')
    .add(b'(')
    .add(b')')
    .add(b'*')
    .add(b'+')
    .add(b',')
    .add(b'/')
    .add(b':')
    .add(b';')
    .add(b'<')
    .add(b'=')
    .add(b'>')
    .add(b'?')
    .add(b'@')
    .add(b'[')
    .add(b'\\')
    .add(b']')
    .add(b'^')
    .add(b'`')
    .add(b'{')
    .add(b'|')
    .add(b'}');

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
}

impl fmt::Display for InputContentTransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Unavailable | Self::Tonic(_) => {
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
        }
    }
}

/// One-attempt HTTP request edge used for component fault injection.
///
/// Implementors are trusted worker composition: the request contains the
/// bearer fence header. Production uses [`TonicInputContentRpc`]; test doubles
/// must not log or retain request authority beyond the bounded assertion.
#[async_trait]
pub(crate) trait InputContentRpc: Send + Sync {
    async fn get(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, InputContentTransportError>;
}

/// Raw HTTP/2 request adapter over a caller-authenticated tonic channel.
#[derive(Clone)]
struct TonicInputContentRpc {
    channel: Channel,
}

impl TonicInputContentRpc {
    #[must_use]
    fn new(channel: Channel) -> Self {
        Self { channel }
    }
}

#[async_trait]
impl InputContentRpc for TonicInputContentRpc {
    async fn get(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, InputContentTransportError> {
        self.channel
            .clone()
            .oneshot(request)
            .await
            .map_err(InputContentTransportError::Tonic)
    }
}

/// One-attempt materialized input client over a dedicated origin-bound HTTP/2
/// request edge.
pub struct InputContentClient {
    rpc: Box<dyn InputContentRpc>,
    config: InputContentConfig,
}

impl InputContentClient {
    /// Build and connect the dedicated mTLS HTTP/2 channel from the validated
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
            .and_then(|endpoint| endpoint.tls_config(tls))
            .map_err(InputContentTransportError::Tonic)
            .map_err(InputContentError::Transport)?;
        let channel = timeout(config.deadline, endpoint.connect())
            .await
            .map_err(|_| InputContentError::Timeout("the input content connection timed out"))?
            .map_err(InputContentTransportError::Tonic)
            .map_err(InputContentError::Transport)?;
        Ok(Self {
            rpc: Box::new(TonicInputContentRpc::new(channel)),
            config,
        })
    }

    #[cfg(test)]
    pub(super) fn with_rpc(
        rpc: impl InputContentRpc + 'static,
        config: InputContentConfig,
    ) -> Result<Self, InputContentError> {
        config.validate()?;
        Ok(Self {
            rpc: Box::new(rpc),
            config,
        })
    }

    /// Fetch exactly one claim-bound, source-bound materialized agent input.
    ///
    /// The opaque execution state proves successful `BeginExecution` and
    /// unique lease-monitor ownership. Raw claim IDs and fence tokens are never
    /// accepted through this public API.
    ///
    /// The method performs one network attempt, does not follow redirects, and
    /// validates HTTP/2, response metadata, source identity, body bounds, and
    /// the response SHA-256 before returning bytes.
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
        let request = build_request(&reference)?;
        let operation = fetch_response(self.rpc.as_ref(), request, &reference, &self.config);
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

fn build_request(
    reference: &ClaimBoundInputAuthority<'_>,
) -> Result<Request<Body>, InputContentError> {
    let execution = encode_path_segment(reference.execution_id);
    let content = encode_path_segment(reference.content_id);
    let version = encode_path_segment(reference.immutable_version);
    let path = format!(
        "/executions/{execution}/generations/{}/inputs/{content}/versions/{version}",
        reference.generation
    );
    let mut request = Request::builder()
        .method(Method::GET)
        .uri(path)
        .body(Body::empty())
        .map_err(|_| InputContentError::InvalidInput("the input content request is malformed"))?;
    request.headers_mut().insert(
        CLAIM_HEADER,
        HeaderValue::from_str(reference.claim_id).map_err(|_| {
            InputContentError::InvalidInput("the input content request authority is malformed")
        })?,
    );
    request.headers_mut().insert(
        FENCE_HEADER,
        HeaderValue::from_str(&URL_SAFE_NO_PAD.encode(reference.fence_token)).map_err(|_| {
            InputContentError::InvalidInput("the input content request authority is malformed")
        })?,
    );
    Ok(request)
}

async fn fetch_response(
    rpc: &dyn InputContentRpc,
    request: Request<Body>,
    reference: &ClaimBoundInputAuthority<'_>,
    config: &InputContentConfig,
) -> Result<MaterializedInput, InputContentError> {
    let response = rpc
        .get(request)
        .await
        .map_err(InputContentError::Transport)?;
    let validated = validate_response_head(&response, reference, config)?;
    collect_validated_body(response, validated, config.max_materialized_bytes).await
}

struct ValidatedResponseHead {
    response_digest: [u8; 32],
    declared_length: usize,
}

fn validate_response_head(
    response: &Response<Body>,
    reference: &ClaimBoundInputAuthority<'_>,
    config: &InputContentConfig,
) -> Result<ValidatedResponseHead, InputContentError> {
    if response.version() != Version::HTTP_2 {
        return Err(InputContentError::DependencyUnavailable(
            "the input content service did not negotiate HTTP/2",
        ));
    }
    match response.status() {
        StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => {
            return Err(InputContentError::AuthorizationFailed(
                "the claim-bound input grant was rejected",
            ));
        }
        StatusCode::NOT_FOUND => {
            return Err(InputContentError::InvalidInput(
                "the admitted input content was not found",
            ));
        }
        StatusCode::PAYLOAD_TOO_LARGE => {
            return Err(InputContentError::ResourceExhausted(
                "the admitted input exceeds the content service limit",
            ));
        }
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => {
            return Err(InputContentError::InvalidInput(
                "the input content service rejected materialization",
            ));
        }
        status if status.is_redirection() => {
            return Err(InputContentError::DependencyUnavailable(
                "the input content service attempted an unsupported redirect",
            ));
        }
        status if !status.is_success() => {
            return Err(InputContentError::DependencyUnavailable(
                "the input content service did not accept the request",
            ));
        }
        _ => {}
    }
    let headers = response.headers();
    let response_digest = sha256_header(headers, &CONTENT_DIGEST_HEADER)?;
    let source_digest = sha256_header(headers, &SOURCE_DIGEST_HEADER)?;
    if source_digest
        .ct_eq(reference.expected_source_sha256)
        .unwrap_u8()
        != 1
    {
        return Err(InputContentError::AuthorizationFailed(
            "the materialized input source digest does not match the admitted descriptor",
        ));
    }
    let source_length = positive_decimal_header(headers, &SOURCE_LENGTH_HEADER)?;
    if source_length != reference.expected_source_length {
        return Err(InputContentError::AuthorizationFailed(
            "the materialized input source length does not match the admitted descriptor",
        ));
    }
    let source_version = single_header(headers, &SOURCE_VERSION_HEADER)?;
    if source_version.as_bytes() != reference.immutable_version.as_bytes() {
        return Err(InputContentError::AuthorizationFailed(
            "the materialized input source version does not match the admitted descriptor",
        ));
    }
    let cache = single_header(headers, &CACHE_CONTROL)?;
    let normalized_cache = cache
        .bytes()
        .filter(|byte| *byte != b' ')
        .map(|byte| byte.to_ascii_lowercase());
    if !normalized_cache.eq(b"private,no-store".iter().copied()) {
        return Err(InputContentError::InvalidInput(
            "the input content cache policy is malformed",
        ));
    }
    let media = single_header(headers, &CONTENT_TYPE)?;
    if media
        .split(';')
        .next()
        .map(str::trim)
        .is_none_or(|value| !value.eq_ignore_ascii_case(reference.media_type))
    {
        return Err(InputContentError::InvalidInput(
            "the input content media type is malformed",
        ));
    }
    let declared_length = positive_decimal_header(headers, &CONTENT_LENGTH)?;
    let declared_length = usize::try_from(declared_length).map_err(|_| {
        InputContentError::ResourceExhausted("the materialized input exceeds its approved limit")
    })?;
    if declared_length > config.max_materialized_bytes {
        return Err(InputContentError::ResourceExhausted(
            "the materialized input exceeds its approved limit",
        ));
    }
    Ok(ValidatedResponseHead {
        response_digest,
        declared_length,
    })
}

async fn collect_validated_body(
    mut response: Response<Body>,
    validated: ValidatedResponseHead,
    max_bytes: usize,
) -> Result<MaterializedInput, InputContentError> {
    let mut body = Zeroizing::new(Vec::with_capacity(validated.declared_length.min(max_bytes)));
    while let Some(frame) = response.body_mut().frame().await {
        let Ok(frame) = frame else {
            return Err(InputContentError::DependencyUnavailable(
                "the input content response was interrupted",
            ));
        };
        if frame.is_trailers() {
            return Err(InputContentError::InvalidInput(
                "the input content response contains unexpected trailers",
            ));
        }
        if let Ok(chunk) = frame.into_data() {
            append_bounded(&mut body, &chunk, max_bytes)?;
        }
    }
    if body.is_empty() || body.len() != validated.declared_length {
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

fn append_bounded(
    body: &mut Vec<u8>,
    chunk: &Bytes,
    max_bytes: usize,
) -> Result<(), InputContentError> {
    if body
        .len()
        .checked_add(chunk.len())
        .is_none_or(|length| length > max_bytes)
    {
        return Err(InputContentError::ResourceExhausted(
            "the materialized input exceeds its approved limit",
        ));
    }
    body.extend_from_slice(chunk);
    Ok(())
}

fn sha256_header(headers: &HeaderMap, name: &HeaderName) -> Result<[u8; 32], InputContentError> {
    let value = single_header(headers, name)?;
    let encoded = value
        .strip_prefix("sha-256=:")
        .and_then(|value| value.strip_suffix(':'))
        .filter(|value| value.len() == 44 && !value.bytes().any(|byte| matches!(byte, b',' | b' ')))
        .ok_or(InputContentError::InvalidInput(
            "the input content response digest is malformed",
        ))?;
    let decoded = STANDARD.decode(encoded).map_err(|_| {
        InputContentError::InvalidInput("the input content response digest is malformed")
    })?;
    if STANDARD.encode(&decoded) != encoded {
        return Err(InputContentError::InvalidInput(
            "the input content response digest is malformed",
        ));
    }
    decoded.try_into().map_err(|_| {
        InputContentError::InvalidInput("the input content response digest is malformed")
    })
}

fn positive_decimal_header(
    headers: &HeaderMap,
    name: &HeaderName,
) -> Result<u64, InputContentError> {
    let value = single_header(headers, name)?;
    if value.is_empty() || !value.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(InputContentError::InvalidInput(
            "the input content response length is malformed",
        ));
    }
    let parsed = value.parse::<u64>().map_err(|_| {
        InputContentError::InvalidInput("the input content response length is malformed")
    })?;
    if parsed == 0 {
        return Err(InputContentError::InvalidInput(
            "the input content response length is malformed",
        ));
    }
    Ok(parsed)
}

fn single_header<'a>(
    headers: &'a HeaderMap,
    name: &HeaderName,
) -> Result<&'a str, InputContentError> {
    let mut values = headers.get_all(name).iter();
    let value = values.next().ok_or(InputContentError::InvalidInput(
        "the input content response metadata is missing",
    ))?;
    if values.next().is_some() {
        return Err(InputContentError::InvalidInput(
            "the input content response metadata is ambiguous",
        ));
    }
    value.to_str().map_err(|_| {
        InputContentError::InvalidInput("the input content response metadata is malformed")
    })
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

fn encode_path_segment(value: &str) -> String {
    utf8_percent_encode(value, PATH_SEGMENT).to_string()
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
