//! Post-authorization, claim-bound Elitea runtime-context redemption.
//!
//! The Rust worker permits execution-actor PAT redemption only after it has
//! crossed `AuthorizeInvocation`. Main currently validates the live claim,
//! fence, desired state and project but does not independently inspect the
//! invocation-state transition for this endpoint. This transport owns one
//! bounded mTLS gRPC attempt and accepts only the opaque authority minted by
//! the worker's durable transition. Response bodies, fence tokens and PATs
//! never enter an error message or tracing field.

#![allow(dead_code)] // Production provider assembly remains capability-disabled.

use std::fmt;
use std::time::Duration;

use serde::{Deserialize, Deserializer};
use serde_json::{Map, Value};
use tokio::time::timeout;
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity};
use tonic::{Code, Status};
use zeroize::Zeroizing;

use super::input_content::{
    ContentRpc, channel_transport, claim_request, content_rpc, content_scope, is_transport_status,
};
use crate::protocol::control::{
    ClaimBoundRuntimeContextAuthority, RuntimeContextRedemptionBinding,
};
use crate::protocol::elitea::runtime::v1::{
    DeleteArtifactRequestV1, GetApplicationVersionRequestV1, GetAttachmentObjectRequestV1,
    GetEliteaClientTokenRequestV1, ListArtifactsRequestV1, ReadArtifactRequestV1,
    RuntimeContentScopeV1, WriteArtifactRequestV1, WriteProjectContextRequestV1,
    WriteSkillRequestV1, get_attachment_object_response_v1::Outcome,
};
use crate::workspace_run::context::{
    RUNTIME_CONTEXT_FIELD as WORKSPACE_FIELD, WorkspaceRuntimeContext,
};

const TOKEN_CONTEXT_SCHEMA: &str = "elitea.runtime.elitea-client-token.v1";
const APPLICATION_VERSION_SCHEMA: &str = "elitea.runtime.application-version.v1";
/// v1 served raw UTF-8 bytes only; v2 serves EXTRACTED text with its unit
/// map (`RuntimeAttachmentObjectSchemaVersion`,
/// `services/elitea-main/internal/infra/storage/runtime_attachment_object.go`).
/// Both are accepted so that this worker can run against a main that has not
/// been upgraded yet.
const ATTACHMENT_OBJECT_SCHEMA_V1: &str = "elitea.runtime.attachment-object.v1";
const ATTACHMENT_OBJECT_SCHEMA_V2: &str = "elitea.runtime.attachment-object.v2";
/// The refusal reasons main may send, as `'static` strings so that a
/// `RuntimeContextError::Rejected` can carry one. Anything else is reported
/// as `unreadable`.
const ATTACHMENT_UNREADABLE_REASONS: [&str; 9] = [
    // Main is still extracting a large file. A read of the same file after
    // the extraction is filed (a regenerate, or the file attached again)
    // gets its text; a message without the file does not read it.
    "processing",
    "empty",
    "too_large",
    "unsupported_format",
    "encrypted",
    "malformed",
    "unsafe_structure",
    "no_text",
    "timeout",
];
/// The unit kinds main's extractor produces (`extract.UnitKind`).
const ATTACHMENT_UNIT_KINDS: [&str; 5] = ["page", "slide", "sheet", "section", "part"];
const MAX_ATTACHMENT_UNIT_LABEL_BYTES: usize = 512;
const SKILL_WRITE_SCHEMA: &str = "elitea.runtime.skill-write.v1";
const PROJECT_CONTEXT_WRITE_SCHEMA: &str = "elitea.runtime.project-context-write.v1";
/// The `artifact` toolkit family's four schemas (#906).
const ARTIFACT_LIST_SCHEMA: &str = "elitea.runtime.artifact-list.v1";
const ARTIFACT_READ_SCHEMA: &str = "elitea.runtime.artifact-read.v1";
const ARTIFACT_WRITE_SCHEMA: &str = "elitea.runtime.artifact-write.v1";
const ARTIFACT_DELETE_SCHEMA: &str = "elitea.runtime.artifact-delete.v1";
/// The artifact REQUEST ceiling, enforced here before a request is sent.
///
/// Main refuses a larger body with 413 and an over-cap document with 422
/// (`maxRuntimeArtifactRequestBytes` / `maxRuntimeArtifactWriteChars`,
/// `services/elitea-main/internal/infra/storage/runtime_artifact_object.go`).
/// Checking here is not redundant for the reason `MAX_BUILDER_REQUEST_BYTES`
/// gives: a request that cannot be accepted should not spend the turn's
/// deadline crossing the network to be told so.
const MAX_ARTIFACT_REQUEST_BYTES: usize = 512 * 1_024;
/// The artifact ENVELOPE's ceiling, which is not the file's.
///
/// Main serves at most `200_000` CHARACTERS of file content and caps the JSON
/// envelope at 2 MiB (`maxRuntimeArtifactReadChars` /
/// `maxRuntimeArtifactResponseBytes`), because the content travels as a JSON
/// string and a control-character-dense file escapes to six characters per
/// byte. This side has to admit the envelope main is willing to send, so it is
/// the larger of the two numbers that appears here.
const MAX_ARTIFACT_OBJECT_BYTES: usize = 2 * 1_024 * 1_024;
/// The two builder WRITE bodies' own ceiling, enforced on this side before a
/// request is sent.
///
/// Main refuses anything larger with 413 and the individual field caps with 422
/// (`maxRuntimeBuilderRequestBytes`,
/// `services/elitea-main/internal/infra/storage/runtime_entity_builder.go`).
/// Checking here too is not redundant: a request that cannot possibly be
/// accepted should not spend the turn's deadline crossing the network to be
/// told so, and the tool can answer the model with something it can act on
/// (write less) instead of a transport failure.
const MAX_BUILDER_REQUEST_BYTES: usize = 64 * 1_024;
const MAX_SAFE_TEXT_BYTES: usize = 256;
const MAX_ORIGIN_BYTES: usize = 2_048;
const MAX_TOKEN_CONTEXT_BYTES: usize = 32 * 1_024;
const MAX_APPLICATION_VERSION_BYTES: usize = 1_024 * 1_024;
/// The attachment ENVELOPE's ceiling, which is not the text's.
///
/// Main serves at most 2 MiB of extracted text per attachment and caps the
/// JSON envelope at 6 MiB (`maxRuntimeAttachmentServedTextBytes` /
/// `maxRuntimeAttachmentObjectResponseBytes` in
/// `services/elitea-main/internal/infra/storage/runtime_attachment_object.go`):
/// the text has no control characters but newline and tab and is encoded
/// without HTML escaping, so it at most doubles, and the unit map adds under
/// 1.5 MiB. This side has to admit the envelope main is willing to send.
pub(crate) const MAX_ATTACHMENT_OBJECT_BYTES: usize = 6 * 1_024 * 1_024;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_RUNTIME_CONTEXT_DEADLINE: Duration = Duration::from_mins(5);
/// The optional runtime-context fields this worker reads (Main's
/// `accept_fields`): only `workspace` (ADR-0034). The `secrets-header-value`
/// field is not read by this worker, so it is not asked for.
const ACCEPT_FIELDS: [&str; 1] = [WORKSPACE_FIELD];

/// Immutable policy for the dedicated runtime-context channel.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RuntimeContextConfig {
    pub(crate) origin: String,
    pub(crate) deadline: Duration,
    pub(crate) max_response_bytes: usize,
    pub(crate) max_application_response_bytes: usize,
    pub(crate) max_attachment_response_bytes: usize,
    /// The artifact family's own envelope ceiling. It is a separate field
    /// rather than a reuse of the attachment one because the two carry
    /// different documents with different caps, and one number that covered
    /// both would be the larger of them applied to the smaller — which is a
    /// cap that no longer bounds what it was written for.
    pub(crate) max_artifact_response_bytes: usize,
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
            || self.max_artifact_response_bytes == 0
            || self.max_artifact_response_bytes > MAX_ARTIFACT_OBJECT_BYTES
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
    /// The claim was accepted and the DOCUMENT cannot be stored as written.
    ///
    /// Only the two builder WRITE routes produce this (main answers 422). It is
    /// terminal and deliberately distinct from `NotFound`: the reference is
    /// fine, the content is not, so the caller's honest move is to tell the
    /// model what it wrote is too large or empty and let it write again —
    /// retrying the identical body would fail identically.
    ///
    /// The attachment READ route produces it too, for a stored file main has no
    /// text for (main answers 422 with a reason). The message is then that
    /// reason code — see `rejection_reason`.
    Rejected(&'static str),
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
            Self::Rejected(_) => "runtime_context.rejected",
            Self::DependencyUnavailable(_) | Self::Transport(_) => {
                "runtime_context.dependency_unavailable"
            }
            Self::Timeout(_) => "runtime_context.timeout",
        }
    }

    /// The reason code of a `Rejected` error, for the attachment read route.
    #[must_use]
    pub(crate) const fn rejection_reason(&self) -> Option<&'static str> {
        match self {
            Self::Rejected(reason) => Some(reason),
            _ => None,
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
            | Self::Rejected(message)
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
    /// A call the channel failed; the status keeps its source chain.
    Status(Box<Status>),
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
            Self::Status(status) => Some(status.as_ref()),
        }
    }
}

/// One ephemeral SDK/model credential scoped to the accepted actor.
///
/// The PAT is erased on drop. The value deliberately exposes no formatting,
/// serialization or cloning surface; the model facade will
/// consume it directly when building bounded request headers.
pub(crate) struct ClaimScopedEliteaContext {
    project_id: u64,
    token: Zeroizing<String>,
    execution_id: String,
    /// ADR-0034: present only for a cloud turn of a handed-off thread.
    workspace: Option<WorkspaceRuntimeContext>,
}

impl ClaimScopedEliteaContext {
    /// Mint one bounded model-request credential from this invocation owner.
    ///
    /// Parent and nested agents share the same accepted actor, but each ADK
    /// model instance owns its own zeroizing header value and turn budget. The
    /// authority itself remains non-cloneable and cannot outlive assembly.
    pub(crate) fn model_facade_token(&self) -> Zeroizing<String> {
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
    pub(crate) fn execution_id(&self) -> &str {
        self.execution_id.as_str()
    }

    /// The handed-off thread this turn runs in (ADR-0034), if any.
    #[must_use]
    pub(crate) const fn workspace(&self) -> Option<&WorkspaceRuntimeContext> {
        self.workspace.as_ref()
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
    pub(crate) fn fixture(project_id: u64, token: &str) -> Self {
        Self::fixture_with_execution_id(project_id, token, "0123456789abcdef0123456789abcdef")
    }

    #[cfg(test)]
    pub(crate) fn with_workspace(mut self, workspace: WorkspaceRuntimeContext) -> Self {
        self.workspace = Some(workspace);
        self
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
            workspace: None,
        }
    }
}

/// One-attempt origin-bound runtime-context client.
pub(crate) struct RuntimeContextClient {
    rpc: ContentRpc,
    config: RuntimeContextConfig,
}

impl RuntimeContextClient {
    /// Connect one dedicated mTLS channel from the validated origin.
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
            .map(crate::transport::keepalive::with_keepalive)
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
            rpc: content_rpc(channel_transport(channel)),
            config,
        })
    }

    /// A client over an in-process content service.
    #[cfg(test)]
    pub(crate) fn with_rpc(
        fake: impl super::input_content::content_fake::ContentFake,
        mut config: RuntimeContextConfig,
    ) -> Result<Self, RuntimeContextError> {
        config.origin = config.validate()?;
        Ok(Self {
            rpc: content_rpc(super::input_content::content_fake::in_process(fake)),
            config,
        })
    }

    /// A fresh handle on the shared channel for one call.
    fn rpc(&self) -> ContentRpc {
        self.rpc.clone()
    }

    /// Redeem one ephemeral actor PAT from the exact authorized claim.
    pub(crate) async fn redeem(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
    ) -> Result<ClaimScopedEliteaContext, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = scoped(
            &binding,
            GetEliteaClientTokenRequestV1 {
                scope: Some(scope(&binding)),
                accept_fields: ACCEPT_FIELDS.map(str::to_owned).to_vec(),
            },
            "the runtime context request authority is malformed",
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .get_elitea_client_token(request)
                .await
                .map_err(|status| context_status(status, None))?
                .into_inner()
                .context_json;
            redeem_response(
                &bounded_document(body, self.config.max_response_bytes)?,
                &binding,
            )
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the runtime context request timed out"))?
    }

    /// Load one exact child definition through the same live claim and fence.
    ///
    /// Main serves this RPC from the private content listener
    /// (`ContentServer.GetApplicationVersion` in
    /// `services/elitea-main/internal/infra/storage/content_server.go`), returning
    /// a definition already frozen by the same freeze the parent's own start
    /// applies. There is deliberately no fallback: the mutable public version
    /// endpoint and the legacy `X-SECRET` expansion path would both hand back an
    /// unfrozen document, so a missing RPC has to fail the turn rather than
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
        let request = scoped(
            &binding,
            GetApplicationVersionRequestV1 {
                scope: Some(scope(&binding)),
                application_id,
                version_id,
            },
            "the nested application request authority is malformed",
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .get_application_version(request)
                .await
                .map_err(|status| context_status(status, None))?
                .into_inner()
                .version_json;
            load_application_response(
                &bounded_document(body, self.config.max_application_response_bytes)?,
                &binding,
                application_id,
                version_id,
            )
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the nested application request timed out"))?
    }

    /// Read one stored chat attachment's TEXT through the same live claim.
    ///
    /// Main serves this from the private content listener
    /// (`ContentServer.GetAttachmentObject`, backed by
    /// `services/elitea-main/internal/infra/storage/runtime_attachment_object.go`),
    /// and it authorizes on the CLAIM's own project and conversation: the
    /// `(bucket, name)` pair below only selects INSIDE them, so a key belonging
    /// to another chat is refused there rather than trusted here.
    ///
    /// `name` is the object key and it contains slashes
    /// (`{conversationUUID}/{filename}`); it travels as one message field,
    /// exactly as stored.
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
        let request = scoped(
            &binding,
            GetAttachmentObjectRequestV1 {
                scope: Some(scope(&binding)),
                bucket: bucket.to_owned(),
                name: name.to_owned(),
            },
            "the attachment request authority is malformed",
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let outcome = rpc
                .get_attachment_object(request)
                .await
                .map_err(attachment_status)?
                .into_inner()
                .outcome;
            match outcome {
                Some(Outcome::DocumentJson(body)) => load_attachment_response(
                    &bounded_document(body, self.config.max_attachment_response_bytes)?,
                    &binding,
                    bucket,
                    name,
                ),
                Some(Outcome::UnreadableReason(reason)) => Err(attachment_rejection(&reason)),
                None => Err(RuntimeContextError::InvalidResponse(
                    "the attachment response is malformed",
                )),
            }
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the attachment request timed out"))?
    }

    /// Create or update one Skill through the same live claim (#940 A8).
    ///
    /// This is a WRITE, and it is the first one on this channel. Everything the
    /// reads above rely on still holds — one bounded attempt, one origin, the
    /// claim and fence as the only authority — and the project is still main's
    /// to decide: nothing in this request names one, exactly as nothing in the
    /// attachment read does.
    ///
    /// The write is NOT retried on a transport failure here, and that is the
    /// point of leaving it to the single attempt the rest of this module makes:
    /// a retried create whose first attempt actually landed would produce a
    /// second skill with the same name, which is precisely the duplicate case
    /// the by-name selection exists to prevent.
    pub(crate) async fn write_skill(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        request: &SkillWriteRequest,
    ) -> Result<SkillWriteOutcome, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = scoped(
            &binding,
            WriteSkillRequestV1 {
                scope: Some(scope(&binding)),
                request_json: encode_builder_body(request)?,
            },
            "the skill write request authority is malformed",
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .write_skill(request)
                .await
                .map_err(|status| context_status(status, Some(builder_rejection())))?
                .into_inner()
                .response_json;
            write_skill_response(
                &bounded_document(body, self.config.max_response_bytes)?,
                &binding,
            )
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the skill write request timed out"))?
    }

    /// List one artifact bucket through the same live claim (#906).
    ///
    /// The four artifact methods below are the `artifact` toolkit family's
    /// whole transport. They keep every property the reads above have — one
    /// bounded attempt, one origin, the claim and fence as the only authority
    /// — and the project is still main's to decide: nothing in these requests
    /// names one. What main adds that the other calls do not need is the
    /// per-bucket ACCESS LIST, applied to the claim's own actor, so a bucket
    /// the person who started the turn may not touch is refused there rather
    /// than trusted here.
    pub(crate) async fn list_artifacts(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        request: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = scoped(
            &binding,
            ListArtifactsRequestV1 {
                scope: Some(scope(&binding)),
                request_json: encode_artifact_body(request)?,
            },
            ARTIFACT_AUTHORITY_MALFORMED,
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .list_artifacts(request)
                .await
                .map_err(artifact_status)?
                .into_inner()
                .response_json;
            artifact_list_response(&self.artifact_document(body)?, &binding)
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the artifact listing timed out"))?
    }

    /// Read one artifact's TEXT through the same live claim.
    ///
    /// An over-cap file is NOT an error on this call: main answers OK with
    /// `over_limit` and the measurement, because the tool's answer to the model
    /// has to name the cap and the actual size. `FailedPrecondition` here would
    /// mean the file is not text at all.
    pub(crate) async fn read_artifact(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        request: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = scoped(
            &binding,
            ReadArtifactRequestV1 {
                scope: Some(scope(&binding)),
                request_json: encode_artifact_body(request)?,
            },
            ARTIFACT_AUTHORITY_MALFORMED,
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .read_artifact(request)
                .await
                .map_err(artifact_status)?
                .into_inner()
                .response_json;
            artifact_read_response(&self.artifact_document(body)?, &binding)
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the artifact read timed out"))?
    }

    /// Create or replace one artifact through the same live claim.
    ///
    /// Not retried on a transport failure, for the reason `write_skill` gives:
    /// a retried write whose first attempt actually landed would overwrite a
    /// file a concurrent turn may already have changed.
    pub(crate) async fn write_artifact(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        request: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = scoped(
            &binding,
            WriteArtifactRequestV1 {
                scope: Some(scope(&binding)),
                request_json: encode_artifact_body(request)?,
            },
            ARTIFACT_AUTHORITY_MALFORMED,
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .write_artifact(request)
                .await
                .map_err(artifact_status)?
                .into_inner()
                .response_json;
            artifact_write_response(&self.artifact_document(body)?, &binding)
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the artifact write timed out"))?
    }

    /// Delete one artifact through the same live claim.
    pub(crate) async fn delete_artifact(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        request: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = scoped(
            &binding,
            DeleteArtifactRequestV1 {
                scope: Some(scope(&binding)),
                request_json: encode_artifact_body(request)?,
            },
            ARTIFACT_AUTHORITY_MALFORMED,
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .delete_artifact(request)
                .await
                .map_err(artifact_status)?
                .into_inner()
                .response_json;
            artifact_delete_response(&self.artifact_document(body)?, &binding)
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| RuntimeContextError::Timeout("the artifact delete timed out"))?
    }

    /// Write the claimed project's Project Context through the same live claim.
    pub(crate) async fn write_project_context(
        &self,
        authority: &ClaimBoundRuntimeContextAuthority,
        request: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, RuntimeContextError> {
        let binding = authority.redemption_binding();
        validate_binding(&binding)?;
        let request = scoped(
            &binding,
            WriteProjectContextRequestV1 {
                scope: Some(scope(&binding)),
                request_json: encode_builder_body(request)?,
            },
            "the project context write request authority is malformed",
        )?;
        let mut rpc = self.rpc();
        let operation = async {
            let body = rpc
                .write_project_context(request)
                .await
                .map_err(|status| context_status(status, Some(builder_rejection())))?
                .into_inner()
                .response_json;
            write_project_context_response(
                &bounded_document(body, self.config.max_response_bytes)?,
                &binding,
            )
        };
        timeout(self.config.deadline, operation)
            .await
            .map_err(|_| {
                RuntimeContextError::Timeout("the project context write request timed out")
            })?
    }

    /// One artifact response document, within the artifact family's own
    /// envelope ceiling.
    fn artifact_document(&self, body: Vec<u8>) -> Result<Zeroizing<Vec<u8>>, RuntimeContextError> {
        bounded_document(body, self.config.max_artifact_response_bytes)
    }
}

pub(crate) use elitea_agent_runtime::platform::{
    ArtifactDeleteOutcome, ArtifactDeleteRequest, ArtifactFile, ArtifactListOutcome,
    ArtifactListRequest, ArtifactReadOutcome, ArtifactReadRequest, ArtifactWriteOutcome,
    ArtifactWriteRequest, ProjectContextWriteOutcome, ProjectContextWriteRequest,
    SkillWriteOutcome, SkillWriteRequest,
};

/// One claim-scoped attachment document, already proven to belong to the
/// claimed execution's own project and conversation by the service that served
/// it.
///
/// `content` is TEXT that main extracted (v2) or the file's own UTF-8 bytes
/// (v1), so this type has no binary shape to represent. It is deliberately not
/// `Debug` or `Clone` — the text is tenant document content and belongs in
/// exactly one place, the prompt this turn is building (and the turn's own
/// attachment tools).
pub(crate) struct RuntimeAttachmentObject {
    bucket: String,
    name: String,
    content: String,
    layout: AttachmentTextLayout,
}

/// One page, slide, sheet, section or text part. `start` and `end` are byte
/// offsets into the served text, on character boundaries.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub(crate) struct AttachmentTextUnit {
    pub(crate) kind: String,
    pub(crate) label: String,
    pub(crate) start: usize,
    pub(crate) end: usize,
    pub(crate) has_text: bool,
}

/// What main says about the text it served: its unit map, how many units the
/// SOURCE has, which units have little or no text, and whether the text is the
/// whole document.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct AttachmentTextLayout {
    pub(crate) format: String,
    pub(crate) units: Vec<AttachmentTextUnit>,
    pub(crate) unit_count: usize,
    pub(crate) low_text_units: Vec<usize>,
    pub(crate) text_bytes: u64,
    pub(crate) complete: bool,
    pub(crate) partial_reason: String,
}

impl AttachmentTextLayout {
    /// The layout of a v1 document: one text part covering everything.
    fn whole_text(content: &str) -> Self {
        Self {
            format: "text".to_owned(),
            units: vec![AttachmentTextUnit {
                kind: "part".to_owned(),
                label: "1".to_owned(),
                start: 0,
                end: content.len(),
                has_text: true,
            }],
            unit_count: 1,
            low_text_units: Vec::new(),
            text_bytes: content.len() as u64,
            complete: true,
            partial_reason: String::new(),
        }
    }
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

    #[must_use]
    pub(crate) fn into_parts(self) -> (String, AttachmentTextLayout) {
        (self.content, self.layout)
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
    saved_agent_fingerprint: Option<SavedAgentFingerprint>,
    version_details: Map<String, Value>,
}

/// Main's pre-redemption definition identity from one claim-bound response.
///
/// The transport owns construction. Editable version fields cannot mint this
/// identity. The digest is opaque and never recomputed from redeemed settings.
#[derive(Clone, Copy, Eq, PartialEq)]
pub(crate) struct SavedAgentFingerprint {
    application_id: u64,
    version_id: u64,
    definition_digest: [u8; 32],
}

impl SavedAgentFingerprint {
    #[must_use]
    pub(crate) const fn application_id(&self) -> u64 {
        self.application_id
    }

    #[must_use]
    pub(crate) const fn version_id(&self) -> u64 {
        self.version_id
    }

    #[must_use]
    pub(crate) const fn definition_digest(&self) -> [u8; 32] {
        self.definition_digest
    }
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

    #[must_use]
    pub(crate) const fn saved_agent_fingerprint(&self) -> Option<SavedAgentFingerprint> {
        self.saved_agent_fingerprint
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

/// The execution and generation every call on this channel is made under.
fn scope(binding: &RuntimeContextRedemptionBinding<'_>) -> RuntimeContentScopeV1 {
    content_scope(binding.execution_id, binding.generation)
}

/// `message` under the binding's claim and fence. `malformed` names the call
/// whose authority could not be attached.
fn scoped<T>(
    binding: &RuntimeContextRedemptionBinding<'_>,
    message: T,
    malformed: &'static str,
) -> Result<tonic::Request<T>, RuntimeContextError> {
    claim_request(message, binding.claim_id, binding.fence_token)
        .ok_or(RuntimeContextError::AuthorizationFailed(malformed))
}

/// What a refused or failed runtime-context call means, by status code.
///
/// `rejected` is the call's own `FailedPrecondition` outcome — the DOCUMENT is
/// refused, not the claim — for the calls that have one. Without it that code
/// is one more answer Main did not serve. `NotFound` and `Unimplemented` are
/// both the terminal "this reference does not resolve here" a missing route
/// always meant on this channel.
fn context_status(status: Status, rejected: Option<RuntimeContextError>) -> RuntimeContextError {
    if status.code() == Code::FailedPrecondition
        && let Some(rejected) = rejected
    {
        return rejected;
    }
    match status.code() {
        Code::Unauthenticated | Code::PermissionDenied => {
            RuntimeContextError::AuthorizationFailed("the claim-bound runtime context was rejected")
        }
        Code::NotFound | Code::Unimplemented => RuntimeContextError::NotFound(
            "the claim-bound runtime context reference no longer exists",
        ),
        Code::DeadlineExceeded => {
            RuntimeContextError::Timeout("the runtime context transfer was interrupted")
        }
        _ if is_transport_status(&status) => {
            RuntimeContextError::Transport(RuntimeContextTransportError::Status(Box::new(status)))
        }
        _ => RuntimeContextError::DependencyUnavailable(
            "the runtime context service did not accept the request",
        ),
    }
}

/// One response document within `max_response_bytes`, erased on drop.
fn bounded_document(
    body: Vec<u8>,
    max_response_bytes: usize,
) -> Result<Zeroizing<Vec<u8>>, RuntimeContextError> {
    let body = Zeroizing::new(body);
    if body.is_empty() || body.len() > max_response_bytes {
        return Err(RuntimeContextError::ResourceExhausted(
            "the runtime context exceeds the approved limit",
        ));
    }
    Ok(body)
}

fn encode_builder_body<T: serde::Serialize>(request: &T) -> Result<Vec<u8>, RuntimeContextError> {
    let body = serde_json::to_vec(request).map_err(|_| {
        RuntimeContextError::InvalidConfiguration("the builder request is not encodable")
    })?;
    if body.len() > MAX_BUILDER_REQUEST_BYTES {
        return Err(RuntimeContextError::ResourceExhausted(
            "the builder request exceeds the approved limit",
        ));
    }
    Ok(body)
}

/// `FailedPrecondition` on a builder write means the DOCUMENT is refused, not
/// the claim — see `RuntimeContextError::Rejected`. Everything else maps the
/// way every call on this channel does, which for this one code would turn
/// "write less" into "try the identical body again".
const fn builder_rejection() -> RuntimeContextError {
    RuntimeContextError::Rejected("the builder document cannot be stored as written")
}

fn write_skill_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<SkillWriteOutcome, RuntimeContextError> {
    let decoded: SkillWriteResponse = serde_json::from_slice(body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the skill write response is malformed")
    })?;
    // The project is re-checked against the ACCEPTED execution, the same way
    // every read on this channel does. Main derives it from the claim, so a
    // response naming a different project means the two ends disagree about
    // what was authorized — and this one wrote a row, so a silent mismatch
    // would be a write into a tenant this turn never had.
    if decoded.schema_version != SKILL_WRITE_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.skill_id.is_empty()
        || decoded.name.is_empty()
    {
        return Err(RuntimeContextError::AuthorizationFailed(
            "the skill write response does not match the accepted execution",
        ));
    }
    Ok(SkillWriteOutcome {
        skill_id: decoded.skill_id,
        name: decoded.name,
        created: decoded.created,
    })
}

fn write_project_context_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<ProjectContextWriteOutcome, RuntimeContextError> {
    let decoded: ProjectContextWriteResponse = serde_json::from_slice(body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the project context write response is malformed")
    })?;
    if decoded.schema_version != PROJECT_CONTEXT_WRITE_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
    {
        return Err(RuntimeContextError::AuthorizationFailed(
            "the project context write response does not match the accepted execution",
        ));
    }
    Ok(ProjectContextWriteOutcome {
        content_bytes: decoded.content_bytes,
        enabled: decoded.enabled,
        created: decoded.created,
    })
}

fn encode_artifact_body<T: serde::Serialize>(request: &T) -> Result<Vec<u8>, RuntimeContextError> {
    let body = serde_json::to_vec(request).map_err(|_| {
        RuntimeContextError::InvalidConfiguration("the artifact request is not encodable")
    })?;
    if body.len() > MAX_ARTIFACT_REQUEST_BYTES {
        return Err(RuntimeContextError::ResourceExhausted(
            "the artifact request exceeds the approved limit",
        ));
    }
    Ok(body)
}

const ARTIFACT_AUTHORITY_MALFORMED: &str = "the artifact request authority is malformed";

/// `FailedPrecondition` on an artifact call means the DOCUMENT is refused, not
/// the claim — a file that is not text, or one the model wrote that main will
/// not store — for the reason `builder_rejection` gives.
fn artifact_status(status: Status) -> RuntimeContextError {
    context_status(
        status,
        Some(RuntimeContextError::Rejected(
            "the artifact cannot be served or stored as written",
        )),
    )
}

/// Every artifact response is re-checked against the ACCEPTED execution.
///
/// The project is re-checked the way every call on this channel re-checks it:
/// main derives it from the claim, so a response naming a different project
/// means the two ends disagree about what was authorized — and two of these
/// four calls CHANGE a bucket, so a silent mismatch would be a write into a
/// tenant this turn never had.
fn artifact_identity_failure() -> RuntimeContextError {
    RuntimeContextError::AuthorizationFailed(
        "the artifact response does not match the accepted execution",
    )
}

fn artifact_list_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<ArtifactListOutcome, RuntimeContextError> {
    let decoded: ArtifactListResponse = serde_json::from_slice(body)
        .map_err(|_| RuntimeContextError::InvalidResponse("the artifact listing is malformed"))?;
    if decoded.schema_version != ARTIFACT_LIST_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.bucket.is_empty()
    {
        return Err(artifact_identity_failure());
    }
    Ok(ArtifactListOutcome {
        bucket: decoded.bucket,
        files: decoded
            .files
            .into_iter()
            .map(|file| ArtifactFile {
                name: file.name,
                byte_length: file.byte_length,
                media_type: file.media_type,
                modified_at: file.modified_at,
            })
            .collect(),
        truncated: decoded.truncated,
    })
}

fn artifact_read_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<ArtifactReadOutcome, RuntimeContextError> {
    let decoded: ArtifactReadResponse = serde_json::from_slice(body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the artifact read response is malformed")
    })?;
    if decoded.schema_version != ARTIFACT_READ_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.bucket.is_empty()
        || decoded.name.is_empty()
        || decoded.max_chars == 0
    {
        return Err(artifact_identity_failure());
    }
    // A served read must carry content and a refused one must not. Either
    // half alone would let a refusal read as an empty file — which a model
    // answers from as though the file were empty.
    if decoded.over_limit != decoded.content.is_empty() {
        return Err(RuntimeContextError::InvalidResponse(
            "the artifact read response contradicts its own size verdict",
        ));
    }
    Ok(ArtifactReadOutcome {
        name: decoded.name,
        media_type: decoded.media_type,
        byte_length: decoded.byte_length,
        char_length: decoded.char_length,
        total_lines: decoded.total_lines,
        max_chars: decoded.max_chars,
        over_limit: decoded.over_limit,
        content: decoded.content,
    })
}

fn artifact_write_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<ArtifactWriteOutcome, RuntimeContextError> {
    let decoded: ArtifactWriteResponse = serde_json::from_slice(body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the artifact write response is malformed")
    })?;
    if decoded.schema_version != ARTIFACT_WRITE_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.bucket.is_empty()
        || decoded.name.is_empty()
    {
        return Err(artifact_identity_failure());
    }
    Ok(ArtifactWriteOutcome {
        bucket: decoded.bucket,
        name: decoded.name,
        media_type: decoded.media_type,
        byte_length: decoded.byte_length,
    })
}

fn artifact_delete_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<ArtifactDeleteOutcome, RuntimeContextError> {
    let decoded: ArtifactDeleteResponse = serde_json::from_slice(body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the artifact delete response is malformed")
    })?;
    if decoded.schema_version != ARTIFACT_DELETE_SCHEMA
        || decoded.project_id == 0
        || decoded.project_id.to_string() != binding.resource_project_id
        || decoded.bucket.is_empty()
        || decoded.name.is_empty()
    {
        return Err(artifact_identity_failure());
    }
    Ok(ArtifactDeleteOutcome {
        name: decoded.name,
        deleted: decoded.deleted,
    })
}

fn redeem_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
) -> Result<ClaimScopedEliteaContext, RuntimeContextError> {
    let decoded: RuntimeContextResponse = serde_json::from_slice(body).map_err(|_| {
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
    if let Some(workspace) = &decoded.workspace {
        workspace
            .validate()
            .map_err(RuntimeContextError::InvalidResponse)?;
    }
    Ok(ClaimScopedEliteaContext {
        project_id: decoded.project_id,
        token: decoded.token.0,
        execution_id: binding.execution_id.to_owned(),
        workspace: decoded.workspace,
    })
}

fn load_application_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
    application_id: u64,
    version_id: u64,
) -> Result<RuntimeApplicationVersion, RuntimeContextError> {
    let decoded: ApplicationVersionResponse = serde_json::from_slice(body).map_err(|_| {
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
        saved_agent_fingerprint: decoded.frozen_definition_sha256.0.map(|definition_digest| {
            SavedAgentFingerprint {
                application_id,
                version_id,
                definition_digest,
            }
        }),
        version_details: decoded.version_details,
    })
}

/// The attachment call's own refusals.
///
/// A missing OBJECT is `NotFound`. A deployment with no Go object store does
/// not serve the RPC at all (`Unimplemented`): there the file exists and this
/// deployment simply cannot read it, so telling the model the file "no longer
/// exists" would be false — it is a terminal rejection with its own reason,
/// never an outage to retry. A `FailedPrecondition` is a file main has no text
/// for, whose reason did not come back.
fn attachment_status(status: Status) -> RuntimeContextError {
    match status.code() {
        Code::NotFound => RuntimeContextError::NotFound("the attachment object no longer exists"),
        Code::Unimplemented => RuntimeContextError::Rejected("unsupported_deployment"),
        _ => context_status(status, Some(RuntimeContextError::Rejected("unreadable"))),
    }
}

fn load_attachment_response(
    body: &[u8],
    binding: &RuntimeContextRedemptionBinding<'_>,
    bucket: &str,
    name: &str,
) -> Result<RuntimeAttachmentObject, RuntimeContextError> {
    let decoded: Value = serde_json::from_slice(body).map_err(|_| {
        RuntimeContextError::InvalidResponse("the attachment response is malformed")
    })?;
    let malformed = || RuntimeContextError::InvalidResponse("the attachment response is malformed");
    let (identity, content, layout) = match decoded.get("schema_version").and_then(Value::as_str) {
        Some(ATTACHMENT_OBJECT_SCHEMA_V2) => {
            let document: AttachmentObjectResponseV2 =
                serde_json::from_value(decoded).map_err(|_| malformed())?;
            let layout = AttachmentTextLayout {
                format: document.format,
                units: document.units,
                unit_count: document.unit_count,
                low_text_units: document.low_text_units,
                text_bytes: document.text_bytes,
                complete: document.complete,
                partial_reason: document.partial_reason,
            };
            if !valid_attachment_layout(&document.content, &layout) {
                return Err(malformed());
            }
            (
                (document.project_id, document.bucket, document.name),
                document.content,
                layout,
            )
        }
        Some(ATTACHMENT_OBJECT_SCHEMA_V1) => {
            let document: AttachmentObjectResponse =
                serde_json::from_value(decoded).map_err(|_| malformed())?;
            if u64::try_from(document.content.len()) != Ok(document.byte_length) {
                return Err(RuntimeContextError::AuthorizationFailed(
                    "the attachment response does not match the accepted execution",
                ));
            }
            let layout = AttachmentTextLayout::whole_text(&document.content);
            (
                (document.project_id, document.bucket, document.name),
                document.content,
                layout,
            )
        }
        _ => {
            return Err(RuntimeContextError::AuthorizationFailed(
                "the attachment response does not match the accepted execution",
            ));
        }
    };
    // The identity is re-checked against what was ASKED for, not merely against
    // itself. Main derives the project and the conversation from the claim, so
    // a document that names a different project or a different object than this
    // request selected means the two ends disagree about what was authorized —
    // which must fail the read, never silently enrich a prompt.
    let (project_id, decoded_bucket, decoded_name) = identity;
    if project_id == 0
        || project_id.to_string() != binding.resource_project_id
        || decoded_bucket != bucket
        || decoded_name != name
        || content.is_empty()
    {
        return Err(RuntimeContextError::AuthorizationFailed(
            "the attachment response does not match the accepted execution",
        ));
    }
    Ok(RuntimeAttachmentObject {
        bucket: decoded_bucket,
        name: decoded_name,
        content,
        layout,
    })
}

/// Main's reason a stored attachment has no text. Any reason this worker does
/// not know still yields a terminal rejection, with the reason `unreadable`:
/// the answer already says "this file has no text", and that must never turn
/// into a retryable outage because a reason was new.
fn attachment_rejection(reason: &str) -> RuntimeContextError {
    ATTACHMENT_UNREADABLE_REASONS
        .iter()
        .find(|known| **known == reason)
        .map_or(RuntimeContextError::Rejected("unreadable"), |reason| {
            RuntimeContextError::Rejected(reason)
        })
}

/// The unit map must describe the served text exactly: ordered, disjoint,
/// inside it, on character boundaries, of a known kind, and no more units than
/// the source has. A map that fails this would make the read tool slice the
/// wrong text, so the whole document is refused instead.
fn valid_attachment_layout(content: &str, layout: &AttachmentTextLayout) -> bool {
    let mut previous_end = 0;
    for unit in &layout.units {
        if unit.start < previous_end
            || unit.start > unit.end
            || unit.end > content.len()
            || !content.is_char_boundary(unit.start)
            || !content.is_char_boundary(unit.end)
            || !ATTACHMENT_UNIT_KINDS.contains(&unit.kind.as_str())
            || unit.label.len() > MAX_ATTACHMENT_UNIT_LABEL_BYTES
        {
            return false;
        }
        previous_end = unit.end;
    }
    !layout.units.is_empty()
        && layout.unit_count >= layout.units.len()
        && layout.text_bytes >= content.len() as u64
        && layout
            .low_text_units
            .iter()
            .all(|number| *number >= 1 && *number <= layout.units.len())
        && layout.format.len() <= MAX_SAFE_TEXT_BYTES
        && layout.partial_reason.len() <= MAX_SAFE_TEXT_BYTES
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
    /// Served only because this worker accepts it, and only for a cloud
    /// turn; absent otherwise. Its own keys are as strict as these.
    #[serde(default)]
    workspace: Option<WorkspaceRuntimeContext>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AttachmentObjectResponse {
    #[allow(dead_code)] // Matched before this struct is decoded.
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
struct AttachmentObjectResponseV2 {
    #[allow(dead_code)] // Matched before this struct is decoded.
    schema_version: String,
    project_id: u64,
    bucket: String,
    name: String,
    #[allow(dead_code)] // Carried for diagnostics; the bytes decided, not the type.
    media_type: String,
    #[allow(dead_code)] // The SOURCE object's size, not the text's.
    byte_length: u64,
    format: String,
    #[allow(dead_code)] // Recorded by main with the sidecar.
    extractor_version: String,
    content: String,
    units: Vec<AttachmentTextUnit>,
    unit_count: usize,
    low_text_units: Vec<usize>,
    text_bytes: u64,
    #[allow(dead_code)] // The worker counts its own budget from the text.
    token_estimate: u64,
    complete: bool,
    partial_reason: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SkillWriteResponse {
    schema_version: String,
    project_id: u64,
    skill_id: String,
    name: String,
    created: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ProjectContextWriteResponse {
    schema_version: String,
    project_id: u64,
    content_bytes: u64,
    enabled: bool,
    created: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ArtifactListResponse {
    schema_version: String,
    project_id: u64,
    bucket: String,
    files: Vec<ArtifactFileResponse>,
    truncated: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ArtifactFileResponse {
    name: String,
    byte_length: u64,
    media_type: String,
    modified_at: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ArtifactReadResponse {
    schema_version: String,
    project_id: u64,
    bucket: String,
    name: String,
    media_type: String,
    byte_length: u64,
    char_length: u64,
    total_lines: u64,
    max_chars: u64,
    over_limit: bool,
    content: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ArtifactWriteResponse {
    schema_version: String,
    project_id: u64,
    bucket: String,
    name: String,
    media_type: String,
    byte_length: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ArtifactDeleteResponse {
    schema_version: String,
    project_id: u64,
    bucket: String,
    name: String,
    deleted: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ApplicationVersionResponse {
    schema_version: String,
    project_id: u64,
    application_id: u64,
    version_id: u64,
    #[serde(default)]
    frozen_definition_sha256: OptionalFrozenDefinitionDigest,
    version_details: Map<String, Value>,
}

/// Omission supports legacy responses. Present null is not an omission.
#[derive(Default)]
struct OptionalFrozenDefinitionDigest(Option<[u8; 32]>);

impl<'de> Deserialize<'de> for OptionalFrozenDefinitionDigest {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        let value = String::deserialize(deserializer)?;
        let bytes = value.as_bytes();
        if bytes.len() != 64
            || !bytes
                .iter()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(byte))
        {
            return Err(serde::de::Error::custom(
                "the frozen application definition digest is malformed",
            ));
        }
        let nibble = |byte: u8| {
            if byte.is_ascii_digit() {
                byte - b'0'
            } else {
                byte - b'a' + 10
            }
        };
        let mut digest = [0_u8; 32];
        for (output, pair) in digest.iter_mut().zip(bytes.chunks_exact(2)) {
            *output = (nibble(pair[0]) << 4) | nibble(pair[1]);
        }
        Ok(Self(Some(digest)))
    }
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

#[path = "code_debug.rs"]
mod code_debug;
