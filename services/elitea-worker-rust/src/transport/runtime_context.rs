//! Post-authorization, claim-bound Elitea runtime-context redemption.
//!
//! The Rust worker permits execution-actor PAT redemption only after it has
//! crossed `AuthorizeInvocation`. Main currently validates the live claim,
//! fence, desired state and project but does not independently inspect the
//! invocation-state transition for this endpoint. This transport owns one
//! bounded mTLS HTTP/2 attempt and accepts only the opaque authority minted by
//! the worker's durable transition. Response bodies, fence tokens and PATs
//! never enter an error message or tracing field.

#![allow(dead_code)] // Production provider assembly remains capability-disabled.

use std::fmt;
use std::time::Duration;

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use http::header::{CACHE_CONTROL, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderMap, HeaderName, HeaderValue, Method, Request, Response, StatusCode, Version};
use http_body_util::BodyExt as _;
use percent_encoding::{AsciiSet, CONTROLS, utf8_percent_encode};
use serde::{Deserialize, Deserializer};
use serde_json::{Map, Value};
use tokio::time::timeout;
use tonic::body::Body;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};
use tower::ServiceExt as _;
use zeroize::Zeroizing;

use crate::protocol::control::{
    ClaimBoundRuntimeContextAuthority, RuntimeContextRedemptionBinding,
};

const TOKEN_CONTEXT_SCHEMA: &str = "elitea.runtime.elitea-client-token.v1";
const APPLICATION_VERSION_SCHEMA: &str = "elitea.runtime.application-version.v1";
const ATTACHMENT_OBJECT_SCHEMA: &str = "elitea.runtime.attachment-object.v1";
const MAX_SAFE_TEXT_BYTES: usize = 256;
const MAX_ORIGIN_BYTES: usize = 2_048;
const MAX_TOKEN_CONTEXT_BYTES: usize = 32 * 1_024;
const MAX_APPLICATION_VERSION_BYTES: usize = 1_024 * 1_024;
/// The attachment ENVELOPE's ceiling, which is not the object's.
///
/// Main caps one attachment's bytes at 128 KiB and its JSON envelope at 1 MiB
/// (`maxRuntimeAttachmentObjectBytes` / `maxRuntimeAttachmentObjectResponseBytes`
/// in `services/elitea-main/internal/infra/storage/runtime_attachment_object.go`),
/// because the content travels as a JSON string and a control-character-dense
/// file escapes to six characters per byte. This side has to admit the envelope
/// main is willing to send, so it is the larger of the two numbers that appears
/// here.
const MAX_ATTACHMENT_OBJECT_BYTES: usize = 1_024 * 1_024;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_RUNTIME_CONTEXT_DEADLINE: Duration = Duration::from_mins(5);
const CLAIM_HEADER: HeaderName = HeaderName::from_static("x-elitea-claim-id");
const FENCE_HEADER: HeaderName = HeaderName::from_static("x-elitea-fence");
const PRAGMA_HEADER: HeaderName = HeaderName::from_static("pragma");
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

/// Immutable policy for the dedicated runtime-context channel.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RuntimeContextConfig {
    pub(crate) origin: String,
    pub(crate) deadline: Duration,
    pub(crate) max_response_bytes: usize,
    pub(crate) max_application_response_bytes: usize,
    pub(crate) max_attachment_response_bytes: usize,
}

impl RuntimeContextConfig {
    fn validate(&self) -> Result<String, RuntimeContextError> {
        let origin = canonical_https_origin(&self.origin)?;
        if self.deadline.is_zero()
            || self.deadline > MAX_RUNTIME_CONTEXT_DEADLINE
            || self.max_response_bytes == 0
            || self.max_response_bytes > MAX_TOKEN_CONTEXT_BYTES
            || self.max_application_response_bytes == 0
            || self.max_application_response_bytes > MAX_APPLICATION_VERSION_BYTES
            || self.max_attachment_response_bytes == 0
            || self.max_attachment_response_bytes > MAX_ATTACHMENT_OBJECT_BYTES
        {
            return Err(RuntimeContextError::InvalidConfiguration(
                "the runtime context configuration is malformed",
            ));
        }
        Ok(origin)
    }
}

/// Stable, data-free runtime-context failure.
pub(crate) enum RuntimeContextError {
    InvalidConfiguration(&'static str),
    InvalidResponse(&'static str),
    ResourceExhausted(&'static str),
    AuthorizationFailed(&'static str),
    /// The claim was accepted and the referenced resource does not exist.
    ///
    /// Main answers 404 rather than 403 for a stale nested application/version
    /// precisely so a deleted reference stays distinguishable from a rejected
    /// claim (`internal/infra/storage/content_server.go`). It is TERMINAL on
    /// this side for the same reason: a version that is gone does not come
    /// back, so a retry only spends the turn's budget on a failure whose
    /// reason is already known.
    NotFound(&'static str),
    DependencyUnavailable(&'static str),
    Transport(RuntimeContextTransportError),
    Timeout(&'static str),
}

impl RuntimeContextError {
    #[must_use]
    pub(crate) const fn code(&self) -> &'static str {
        match self {
            Self::InvalidConfiguration(_) => "runtime_context.invalid_configuration",
            Self::InvalidResponse(_) => "runtime_context.invalid_response",
            Self::ResourceExhausted(_) => "runtime_context.resource_exhausted",
            Self::AuthorizationFailed(_) => "runtime_context.authorization_failed",
            Self::NotFound(_) => "runtime_context.not_found",
            Self::DependencyUnavailable(_) | Self::Transport(_) => {
                "runtime_context.dependency_unavailable"
            }
            Self::Timeout(_) => "runtime_context.timeout",
        }
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        matches!(
            self,
            Self::DependencyUnavailable(_) | Self::Transport(_) | Self::Timeout(_)
        )
    }
}

impl fmt::Debug for RuntimeContextError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RuntimeContextError")
            .field("code", &self.code())
            .finish_non_exhaustive()
    }
}

impl fmt::Display for RuntimeContextError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message)
            | Self::InvalidResponse(message)
            | Self::ResourceExhausted(message)
            | Self::AuthorizationFailed(message)
            | Self::NotFound(message)
            | Self::DependencyUnavailable(message)
            | Self::Timeout(message) => formatter.write_str(message),
            Self::Transport(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for RuntimeContextError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Transport(error) => Some(error),
            _ => None,
        }
    }
}

/// Redacted transport failure below response validation.
pub(crate) enum RuntimeContextTransportError {
    Unavailable,
    Tonic(tonic::transport::Error),
}

impl fmt::Debug for RuntimeContextTransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("RuntimeContextTransportError(..)")
    }
}

impl fmt::Display for RuntimeContextTransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the runtime context service is unavailable")
    }
}

impl std::error::Error for RuntimeContextTransportError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Unavailable => None,
            Self::Tonic(error) => Some(error),
        }
    }
}

/// One ephemeral SDK/model credential scoped to the accepted actor.
///
/// The PAT is erased on drop. The value deliberately exposes no formatting,
/// serialization or cloning surface; the future model-gateway adapter will
/// consume it directly when building bounded request headers.
pub(crate) struct ClaimScopedEliteaContext {
    project_id: u64,
    token: Zeroizing<String>,
    execution_id: String,
}

impl ClaimScopedEliteaContext {
    /// Mint one bounded model-request credential from this invocation owner.
    ///
    /// Parent and nested agents share the same accepted actor, but each ADK
    /// model instance owns its own zeroizing header value and turn budget. The
    /// authority itself remains non-cloneable and cannot outlive assembly.
    pub(super) fn model_facade_token(&self) -> Zeroizing<String> {
        Zeroizing::new(self.token.to_string())
    }

    #[must_use]
    pub(crate) const fn resource_project_id(&self) -> u64 {
        self.project_id
    }

    /// The execution this claim was redeemed for (issue 875). The model
    /// gateway sends this as `X-Elitea-Execution-Id` so the gateway's request
    /// log can attribute LLM cost per execution.
    #[must_use]
    pub(super) fn execution_id(&self) -> &str {
        self.execution_id.as_str()
    }

    #[cfg(test)]
    pub(super) const fn project_id(&self) -> u64 {
        self.project_id
    }

    #[cfg(test)]
    pub(super) fn token(&self) -> &str {
        self.token.as_str()
    }

    #[cfg(test)]
    pub(super) fn fixture(project_id: u64, token: &str) -> Self {
        Self::fixture_with_execution_id(project_id, token, "execution/fixture-one")
    }

    #[cfg(test)]
    pub(super) fn fixture_with_execution_id(
        project_id: u64,
        token: &str,
        execution_id: &str,
    ) -> Self {
        Self {
            project_id,
            token: Zeroizing::new(token.to_owned()),
            execution_id: execution_id.to_owned(),
        }
    }
}

#[async_trait]
pub(crate) trait RuntimeContextRpc: Send + Sync {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError>;
}

#[derive(Clone)]
struct TonicRuntimeContextRpc {
    channel: Channel,
}

#[async_trait]
impl RuntimeContextRpc for TonicRuntimeContextRpc {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        self.channel
            .clone()
            .oneshot(request)
            .await
            .map_err(RuntimeContextTransportError::Tonic)
    }
}

/// One-attempt origin-bound runtime-context client.
pub(crate) struct RuntimeContextClient {
    rpc: Box<dyn RuntimeContextRpc>,
    config: RuntimeContextConfig,
}

impl RuntimeContextClient {
    /// Connect one dedicated mTLS HTTP/2 channel from the validated origin.
    pub(crate) async fn connect(
        mut config: RuntimeContextConfig,
        private_ca: Certificate,
        client_identity: Identity,
    ) -> Result<Self, RuntimeContextError> {
        let origin = config.validate()?;
        let tls = ClientTlsConfig::new()
            .ca_certificate(private_ca)
            .identity(client_identity);
        let endpoint = Endpoint::from_shared(origin.clone())
            .and_then(|endpoint| endpoint.tls_config(tls))
            .map_err(RuntimeContextTransportError::Tonic)
            .map_err(RuntimeContextError::Transport)?;
        let channel = timeout(config.deadline, endpoint.connect())
            .await
            .map_err(|_| RuntimeContextError::Timeout("the runtime context connection timed out"))?
            .map_err(RuntimeContextTransportError::Tonic)
            .map_err(RuntimeContextError::Transport)?;
        config.origin = origin;
        Ok(Self {
            rpc: Box::new(TonicRuntimeContextRpc { channel }),
            config,
        })
    }

    #[cfg(test)]
    pub(crate) fn with_rpc(
        rpc: impl RuntimeContextRpc + 'static,
        mut config: RuntimeContextConfig,
    ) -> Result<Self, RuntimeContextError> {
        config.origin = config.validate()?;
        Ok(Self {
            rpc: Box::new(rpc),
            config,
        })
    }

    /// Redeem one ephemeral actor PAT from the exact authorized claim.
    pub(crate) async fn redeem(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
    ) -> Result<ClaimScopedEliteaContext, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = build_request(&binding)?;
        let operation = redeem_response(self.rpc.as_ref(), request, &binding, &self.config);
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the runtime context request timed out"))?
    }

    /// Load one exact child definition through the same live claim and fence.
    ///
    /// Main serves this route from the private content listener
    /// (`ContentServer.PostApplicationVersion` in
    /// `services/elitea-main/internal/infra/storage/content_server.go`), returning
    /// a definition already frozen by the same freeze the parent's own start
    /// applies. There is deliberately no fallback: the mutable public version
    /// endpoint and the legacy `X-SECRET` expansion path would both hand back an
    /// unfrozen document, so a missing route has to fail the turn rather than
    /// quietly resolve a different one.
    pub(crate) async fn load_application_version(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        application_id: u64,
        version_id: u64,
    ) -> Result<RuntimeApplicationVersion, RuntimeContextError> {
        if application_id == 0
            || version_id == 0
            || i64::try_from(application_id).is_err()
            || i64::try_from(version_id).is_err()
        {
            return Err(RuntimeContextError::InvalidConfiguration(
                "the nested application identity is malformed",
            ));
        }
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = build_application_request(&binding, application_id, version_id)?;
        let operation = load_application_response(
            self.rpc.as_ref(),
            request,
            &binding,
            application_id,
            version_id,
            &self.config,
        );
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the nested application request timed out"))?
    }

    /// Read one stored chat attachment's TEXT through the same live claim.
    ///
    /// Main serves this from the private content listener
    /// (`ContentServer.PostAttachmentObject` in
    /// `services/elitea-main/internal/infra/storage/runtime_attachment_object.go`),
    /// and it authorizes on the CLAIM's own project and conversation: the
    /// `(bucket, name)` pair below only selects INSIDE them, so a key belonging
    /// to another chat is refused there rather than trusted here.
    ///
    /// `name` is the object key and it contains slashes
    /// (`{conversationUUID}/{filename}`). It is percent-encoded into ONE path
    /// segment — `PATH_SEGMENT` encodes `/` — because main routes on
    /// `r.URL.RawPath` and unescapes the segment itself, the same mechanism the
    /// immutable input route already uses for its content id.
    pub(crate) async fn load_attachment_object(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        bucket: &str,
        name: &str,
    ) -> Result<RuntimeAttachmentObject, RuntimeContextError> {
        if !bounded_reference(bucket) || !bounded_reference(name) {
            return Err(RuntimeContextError::InvalidConfiguration(
                "the attachment reference is malformed",
            ));
        }
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = build_attachment_request(&binding, bucket, name)?;
        let operation = load_attachment_response(
            self.rpc.as_ref(),
            request,
            &binding,
            bucket,
            name,
            &self.config,
        );
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the attachment request timed out"))?
    }
}

/// One claim-scoped attachment document, already proven to belong to the
/// claimed execution's own project and conversation by the service that served
/// it.
///
/// `content` is TEXT: main refuses anything it cannot serve as valid UTF-8, so
/// this type has no binary shape to represent. It is deliberately not `Debug`
/// or `Clone` — the bytes are tenant document content and belong in exactly one
/// place, the prompt this turn is building.
pub(crate) struct RuntimeAttachmentObject {
    bucket: String,
    name: String,
    content: String,
}

impl RuntimeAttachmentObject {
    #[must_use]
    pub(crate) fn bucket(&self) -> &str {
        &self.bucket
    }

    #[must_use]
    pub(crate) fn name(&self) -> &str {
        &self.name
    }

    #[must_use]
    pub(crate) fn into_content(self) -> String {
        self.content
    }
}

/// One claim-bound, already-frozen and claim-materialized child definition.
///
/// The document deliberately has no `Debug` or `Clone`: nested settings may
/// contain redeemed credentials. The loader returns it once to the recursive
/// ADK assembler, which retains it for that invocation and never re-fetches it
/// after a pause or model retry.
pub(crate) struct RuntimeApplicationVersion {
    application_id: u64,
    version_id: u64,
    version_details: Map<String, Value>,
}

impl RuntimeApplicationVersion {
    #[must_use]
    pub(crate) const fn application_id(&self) -> u64 {
        self.application_id
    }

    #[must_use]
    pub(crate) const fn version_id(&self) -> u64 {
        self.version_id
    }

    pub(crate) fn into_version_details(self) -> Map<String, Value> {
        self.version_details
    }
}

fn validate_binding(
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<(), RuntimeContextError> {
    if !bounded_text(binding.execution_id)
        || binding.generation == 0
        || binding.generation > i64::MAX as u64
        || !bounded_text(binding.claim_id)
        || binding.fence_token.len() != 32
        || binding.fence_token.iter().all(|byte| *byte == 0)
        || !canonical_positive_integer(binding.resource_project_id)
    {
        return Err(RuntimeContextError::AuthorizationFailed(
            "the runtime context authority is malformed",
        ));
    }
    Ok(())
}

fn build_request(
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<Request<Body>, RuntimeContextError> {
    let execution = utf8_percent_encode(binding.execution_id, PATH_SEGMENT);
    let path = format!(
        "/executions/{execution}/generations/{}/runtime-context/elitea-client-token",
        binding.generation
    );
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(path)
        .body(Body::empty())
        .map_err(|_| {
            RuntimeContextError::AuthorizationFailed(
                "the runtime context request authority is malformed",
            )
        })?;
    request.headers_mut().insert(
        CLAIM_HEADER,
        HeaderValue::from_str(binding.claim_id).map_err(|_| {
            RuntimeContextError::AuthorizationFailed(
                "the runtime context request authority is malformed",
            )
        })?,
    );
    request.headers_mut().insert(
        FENCE_HEADER,
        HeaderValue::from_str(&URL_SAFE_NO_PAD.encode(binding.fence_token)).map_err(|_| {
            RuntimeContextError::AuthorizationFailed(
                "the runtime context request authority is malformed",
            )
        })?,
    );
    Ok(request)
}

fn build_application_request(
    binding: &RuntimeContextRedemptionBinding<'_>,
    application_id: u64,
    version_id: u64,
) -> Result<Request<Body>, RuntimeContextError> {
    let execution = utf8_percent_encode(binding.execution_id, PATH_SEGMENT);
    let path = format!(
        "/executions/{execution}/generations/{}/runtime-context/applications/{application_id}/versions/{version_id}",
        binding.generation
    );
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(path)
        .body(Body::empty())
        .map_err(|_| {
            RuntimeContextError::AuthorizationFailed(
                "the nested application request authority is malformed",
            )
        })?;
    insert_claim_headers(&mut request, binding)?;
    Ok(request)
}

fn build_attachment_request(
    binding: &RuntimeContextRedemptionBinding<'_>,
    bucket: &str,
    name: &str,
) -> Result<Request<Body>, RuntimeContextError> {
    let execution = utf8_percent_encode(binding.execution_id, PATH_SEGMENT);
    let bucket_segment = utf8_percent_encode(bucket, PATH_SEGMENT);
    let name_segment = utf8_percent_encode(name, PATH_SEGMENT);
    let path = format!(
        "/executions/{execution}/generations/{}/runtime-context/attachments/{bucket_segment}/{name_segment}",
        binding.generation
    );
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(path)
        .body(Body::empty())
        .map_err(|_| {
            RuntimeContextError::AuthorizationFailed(
                "the attachment request authority is malformed",
            )
        })?;
    insert_claim_headers(&mut request, binding)?;
    Ok(request)
}

fn insert_claim_headers(
    request: &mut Request<Body>,
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<(), RuntimeContextError> {
    request.headers_mut().insert(
        CLAIM_HEADER,
        HeaderValue::from_str(binding.claim_id).map_err(|_| {
            RuntimeContextError::AuthorizationFailed(
                "the runtime context request authority is malformed",
            )
        })?,
    );
    request.headers_mut().insert(
        FENCE_HEADER,
        HeaderValue::from_str(&URL_SAFE_NO_PAD.encode(binding.fence_token)).map_err(|_| {
            RuntimeContextError::AuthorizationFailed(
                "the runtime context request authority is malformed",
            )
        })?,
    );
    Ok(())
}

async fn redeem_response(
    rpc: &dyn RuntimeContextRpc,
    request: Request<Body>,
    binding: &RuntimeContextRedemptionBinding<'_>,
    config: &RuntimeContextConfig,
) -> Result<ClaimScopedEliteaContext, RuntimeContextError> {
    let response = rpc
        .post(request)
        .await
        .map_err(RuntimeContextError::Transport)?;
    let declared_length = validate_response_head(&response, config)?;
    let body = collect_body(response, declared_length, config.max_response_bytes).await?;
    let decoded: RuntimeContextResponse = serde_json::from_slice(&body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the runtime context response is malformed")
    })?;
    if decoded.schema_version != TOKEN_CONTEXT_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id > i64::MAX as u64
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.token.0.is_empty()
        || decoded.token.0.len() > MAX_TOKEN_BYTES
        || decoded
            .token
            .0
            .bytes()
            .any(|byte| matches!(byte, b'\r' | b'\n' | 0))
    {
        return Err(RuntimeContextError::AuthorizationFailed(
            "the runtime context does not match the accepted execution",
        ));
    }
    Ok(ClaimScopedEliteaContext {
        project_id: decoded.project_id,
        token: decoded.token.0,
        execution_id: binding.execution_id.to_owned(),
    })
}

async fn load_application_response(
    rpc: &dyn RuntimeContextRpc,
    request: Request<Body>,
    binding: &RuntimeContextRedemptionBinding<'_>,
    application_id: u64,
    version_id: u64,
    config: &RuntimeContextConfig,
) -> Result<RuntimeApplicationVersion, RuntimeContextError> {
    let response = rpc
        .post(request)
        .await
        .map_err(RuntimeContextError::Transport)?;
    let declared_length =
        validate_response_head_with_limit(&response, config.max_application_response_bytes)?;
    let body = collect_body(
        response,
        declared_length,
        config.max_application_response_bytes,
    )
    .await?;
    let decoded: ApplicationVersionResponse = serde_json::from_slice(&body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the nested application response is malformed")
    })?;
    if decoded.schema_version != APPLICATION_VERSION_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.application_id != application_id
        || decoded.version_id != version_id
        || decoded.version_details.is_empty()
    {
        return Err(RuntimeContextError::AuthorizationFailed(
            "the nested application response does not match the accepted execution",
        ));
    }
    Ok(RuntimeApplicationVersion {
        application_id,
        version_id,
        version_details: decoded.version_details,
    })
}

async fn load_attachment_response(
    rpc: &dyn RuntimeContextRpc,
    request: Request<Body>,
    binding: &RuntimeContextRedemptionBinding<'_>,
    bucket: &str,
    name: &str,
    config: &RuntimeContextConfig,
) -> Result<RuntimeAttachmentObject, RuntimeContextError> {
    let response = rpc
        .post(request)
        .await
        .map_err(RuntimeContextError::Transport)?;
    let declared_length =
        validate_response_head_with_limit(&response, config.max_attachment_response_bytes)?;
    let body = collect_body(
        response,
        declared_length,
        config.max_attachment_response_bytes,
    )
    .await?;
    let decoded: AttachmentObjectResponse = serde_json::from_slice(&body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the attachment response is malformed")
    })?;
    // The identity is re-checked against what was ASKED for, not merely against
    // itself. Main derives the project and the conversation from the claim, so
    // a document that names a different project or a different object than this
    // request selected means the two ends disagree about what was authorized —
    // which must fail the read, never silently enrich a prompt.
    if decoded.schema_version != ATTACHMENT_OBJECT_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.bucket != bucket
        || decoded.name != name
        || decoded.content.is_empty()
        || u64::try_from(decoded.content.len()) != Ok(decoded.byte_length)
    {
        return Err(RuntimeContextError::AuthorizationFailed(
            "the attachment response does not match the accepted execution",
        ));
    }
    Ok(RuntimeAttachmentObject {
        bucket: decoded.bucket,
        name: decoded.name,
        content: decoded.content,
    })
}

fn validate_response_head(
    response: &Response<Body>,
    config: &RuntimeContextConfig,
) -> Result<usize, RuntimeContextError> {
    validate_response_head_with_limit(response, config.max_response_bytes)
}

fn validate_response_head_with_limit(
    response: &Response<Body>,
    max_response_bytes: usize,
) -> Result<usize, RuntimeContextError> {
    if response.version() != Version::HTTP_2 {
        return Err(RuntimeContextError::DependencyUnavailable(
            "the runtime context service did not negotiate HTTP/2",
        ));
    }
    match response.status() {
        StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => {
            return Err(RuntimeContextError::AuthorizationFailed(
                "the claim-bound runtime context was rejected",
            ));
        }
        StatusCode::NOT_FOUND => {
            return Err(RuntimeContextError::NotFound(
                "the claim-bound runtime context reference no longer exists",
            ));
        }
        status if status.is_redirection() => {
            return Err(RuntimeContextError::DependencyUnavailable(
                "the runtime context service attempted an unsupported redirect",
            ));
        }
        status if !status.is_success() => {
            return Err(RuntimeContextError::DependencyUnavailable(
                "the runtime context service did not accept the request",
            ));
        }
        _ => {}
    }
    let headers = response.headers();
    let media_type = single_header(headers, &CONTENT_TYPE)?;
    if !media_type
        .split(';')
        .next()
        .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"))
    {
        return Err(RuntimeContextError::InvalidResponse(
            "the runtime context response type is malformed",
        ));
    }
    validate_cache_policy(headers)?;
    let declared = single_header(headers, &CONTENT_LENGTH)?;
    if declared.is_empty() || !declared.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(RuntimeContextError::InvalidResponse(
            "the runtime context length is malformed",
        ));
    }
    let declared = declared.parse::<usize>().map_err(|_| {
        RuntimeContextError::ResourceExhausted("the runtime context exceeds the approved limit")
    })?;
    if declared == 0 || declared > max_response_bytes {
        return Err(RuntimeContextError::ResourceExhausted(
            "the runtime context exceeds the approved limit",
        ));
    }
    Ok(declared)
}

fn validate_cache_policy(headers: &HeaderMap) -> Result<(), RuntimeContextError> {
    let cache = single_header(headers, &CACHE_CONTROL)?;
    let mut no_store = false;
    let mut no_cache = false;
    for directive in cache.split(',').map(str::trim) {
        no_store |= directive.eq_ignore_ascii_case("no-store");
        no_cache |= directive.eq_ignore_ascii_case("no-cache");
    }
    let pragma = single_header(headers, &PRAGMA_HEADER)?;
    if !no_store || !no_cache || !pragma.trim().eq_ignore_ascii_case("no-cache") {
        return Err(RuntimeContextError::InvalidResponse(
            "the runtime context cache policy is malformed",
        ));
    }
    Ok(())
}

async fn collect_body(
    mut response: Response<Body>,
    declared_length: usize,
    max_response_bytes: usize,
) -> Result<Zeroizing<Vec<u8>>, RuntimeContextError> {
    let mut body = Zeroizing::new(Vec::with_capacity(declared_length));
    while let Some(frame) = response.body_mut().frame().await {
        let frame = frame.map_err(|_| {
            RuntimeContextError::DependencyUnavailable(
                "the runtime context response was interrupted",
            )
        })?;
        if frame.is_trailers() {
            return Err(RuntimeContextError::InvalidResponse(
                "the runtime context response contains unexpected trailers",
            ));
        }
        if let Ok(chunk) = frame.into_data() {
            let length = body.len().checked_add(chunk.len()).ok_or(
                RuntimeContextError::ResourceExhausted(
                    "the runtime context exceeds the approved limit",
                ),
            )?;
            if length > declared_length || length > max_response_bytes {
                return Err(RuntimeContextError::InvalidResponse(
                    "the runtime context length is malformed",
                ));
            }
            body.extend_from_slice(&chunk);
        }
    }
    if body.len() != declared_length {
        return Err(RuntimeContextError::InvalidResponse(
            "the runtime context length is malformed",
        ));
    }
    Ok(body)
}

fn single_header<'a>(
    headers: &'a HeaderMap,
    name: &HeaderName,
) -> Result<&'a str, RuntimeContextError> {
    let mut values = headers.get_all(name).iter();
    let value = values.next().ok_or(RuntimeContextError::InvalidResponse(
        "the runtime context response metadata is missing",
    ))?;
    if values.next().is_some() {
        return Err(RuntimeContextError::InvalidResponse(
            "the runtime context response metadata is ambiguous",
        ));
    }
    value.to_str().map_err(|_| {
        RuntimeContextError::InvalidResponse("the runtime context response metadata is malformed")
    })
}

fn canonical_https_origin(value: &str) -> Result<String, RuntimeContextError> {
    if value.is_empty() || value.len() > MAX_ORIGIN_BYTES {
        return Err(RuntimeContextError::InvalidConfiguration(
            "the runtime context origin is malformed",
        ));
    }
    let uri = value.parse::<http::Uri>().map_err(|_| {
        RuntimeContextError::InvalidConfiguration("the runtime context origin is malformed")
    })?;
    if uri.scheme_str() != Some("https")
        || uri.authority().is_none()
        || uri.path() != "/"
        || uri.query().is_some()
        || uri.authority().is_some_and(|authority| {
            authority.as_str().contains('@') || !authority.as_str().is_ascii()
        })
    {
        return Err(RuntimeContextError::InvalidConfiguration(
            "the runtime context origin is malformed",
        ));
    }
    let authority = uri
        .authority()
        .ok_or(RuntimeContextError::InvalidConfiguration(
            "the runtime context origin is malformed",
        ))?;
    let host = authority.host();
    if host.is_empty() {
        return Err(RuntimeContextError::InvalidConfiguration(
            "the runtime context origin is malformed",
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

/// varchar(256) each side in migrations/tenant/0127, and both the Go admission
/// path and `agents::attachments` refuse anything longer. A reference this
/// worker cannot have been handed is not worth a round trip.
fn bounded_reference(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_SAFE_TEXT_BYTES
        && !value.bytes().any(|byte| matches!(byte, b'\r' | b'\n' | 0))
}

fn canonical_positive_integer(value: &str) -> bool {
    value.parse::<u64>().is_ok_and(|parsed| {
        parsed > 0 && i64::try_from(parsed).is_ok() && parsed.to_string() == value
    })
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RuntimeContextResponse {
    schema_version: String,
    project_id: u64,
    token: SecretToken,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AttachmentObjectResponse {
    schema_version: String,
    project_id: u64,
    bucket: String,
    name: String,
    #[allow(dead_code)] // Carried for diagnostics; the bytes decide, not the type.
    media_type: String,
    byte_length: u64,
    content: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ApplicationVersionResponse {
    schema_version: String,
    project_id: u64,
    application_id: u64,
    version_id: u64,
    version_details: Map<String, Value>,
}

struct SecretToken(Zeroizing<String>);

impl<'de> Deserialize<'de> for SecretToken {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        String::deserialize(deserializer)
            .map(Zeroizing::new)
            .map(Self)
    }
}
