//! The Office toolkit families' host for one claimed turn (ADR-0032 P3):
//! binary artifacts in and out, and the run's chat model.
//!
//! **Objects.** The `artifact` family moves text through the claim-bound
//! runtime-context routes; a `.pptx` is bytes. The bytes go through the
//! platform's artifact store, `elitea.runtime.v1.ArtifactObjectService` on
//! elitea-main's platform listener (#1359 U5; it replaced the HTTP object
//! API on that listener), over the channel the model calls ride and under
//! the turn's own model-gateway credential and claim, so elitea-main applies
//! the project and the per-bucket access list of the person the turn runs
//! as. A refusal keeps the HTTP status the route decided, so each cause keeps
//! its kind; the hop failing is `Unreachable`.
//!
//! **Model.** `complete` binds a second model on the agent's own profile
//! (the binding the step-back index tools use), through the same model
//! facade: the gateway authenticates it with the turn's credential, applies
//! the budget and writes the request log row attributed to the execution.
//! There is no model path of its own and no key in this process.

use std::sync::Arc;

use adk_rust::Part;
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    ImageEditRequest, ImageGenerateRequest, OfficeBucketCreated, OfficeFailure, OfficeHostError,
    OfficeModelRequest, OfficeObjectWritten, OfficeToolHost, TextExtractionFailure,
};
use elitea_connectors::artifact::client::{valid_object_key, valid_segment};
use elitea_connectors::transport::Url;
use elitea_llm_wire::refusal::Refusal;
use elitea_platform_rpc::artifacts::{
    ArtifactClient, ArtifactError, Caller, PutObject, RuntimeClaim,
};
use tonic::transport::Channel;

use super::extractor_client::{Extraction, ExtractorClient, SkipReason};
use super::images_facade::{ImageCall, ImagesClient, ImagesError};
use super::model_text::ModelCallBudget;
use super::runtime_context::ClaimScopedEliteaContext;
use crate::indexing::tools::{LazyStepbackModel, generate_text};

/// What every Office host of the worker shares: the platform's artifact
/// store, the public base URL and the extraction sidecar.
pub(crate) struct OfficeRuntime {
    artifacts: ArtifactClient,
    /// The deployment's public base URL, when it has stated one.
    public_base_url: Option<String>,
    /// The document extraction sidecar, when the deployment names it: the
    /// toolkit tools that read a PDF or Office file as text (`SharePoint`
    /// `read_document`) ask it. Absent, those tools say so.
    extractor: Option<Arc<ExtractorClient>>,
}

/// The turn's credential or project is not usable for the artifact store.
#[derive(Debug)]
pub(crate) struct OfficeRuntimeError;

impl OfficeRuntime {
    /// A runtime over `platform_channel`: elitea-main's platform listener,
    /// the channel the model calls ride.
    pub(crate) fn new(platform_channel: Channel) -> Self {
        Self {
            artifacts: ArtifactClient::new(platform_channel),
            public_base_url: None,
            extractor: None,
        }
    }

    /// The document extraction sidecar the text readers ask.
    #[must_use]
    pub(crate) fn with_extractor(mut self, extractor: Option<Arc<ExtractorClient>>) -> Self {
        self.extractor = extractor;
        self
    }

    /// The public base URL links in answers are built against. Absent, links
    /// are root-relative.
    #[must_use]
    pub(crate) fn with_public_base_url(mut self, public_base_url: Option<String>) -> Self {
        self.public_base_url = public_base_url;
        self
    }
}

/// The claim a turn's Office host names on every object request (review
/// round 2): Main verifies it and confines the turn, in a SYSTEM bucket, to
/// its own conversation's files (`internal/api/v2/artifacts/system_buckets.go`).
/// A prompt cannot make the host stop sending it.
#[derive(Clone)]
pub(crate) struct OfficeClaim {
    execution_id: String,
    generation: u64,
    claim_id: String,
    fence: String,
    /// The conversation the turn belongs to; `None` on a path that has none
    /// (a direct toolkit call), which then reaches no attachment bucket.
    conversation: Option<String>,
    /// The project's attachment bucket name, resolved once from Main (its
    /// storage policy, else the default) over the claim's existing
    /// runtime-context channel (review round 3).
    attachment_bucket: Option<Arc<dyn elitea_agent_runtime::toolkits::AttachmentBucketResolver>>,
    resolved: Arc<tokio::sync::OnceCell<Option<String>>>,
}

impl OfficeClaim {
    /// The claim of `authority`, for a turn of `conversation`.
    pub(crate) fn new(
        authority: &crate::protocol::control::ClaimBoundRuntimeContextAuthority,
        conversation: Option<String>,
    ) -> Self {
        use base64::Engine as _;
        let binding = authority.redemption_binding();
        Self {
            execution_id: binding.execution_id.to_owned(),
            generation: binding.generation,
            claim_id: binding.claim_id.to_owned(),
            fence: base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(binding.fence_token),
            conversation: conversation.filter(|value| canonical_uuid(value)),
            attachment_bucket: None,
            resolved: Arc::new(tokio::sync::OnceCell::new()),
        }
    }

    /// The claim as the artifact store reads it.
    fn runtime_claim(&self) -> RuntimeClaim {
        RuntimeClaim {
            execution_id: self.execution_id.clone(),
            generation: self.generation,
            claim_id: self.claim_id.clone(),
            fence: self.fence.clone(),
        }
    }

    /// This claim, resolving the project's attachment bucket name with
    /// `resolver` (the policy may name one other than the default).
    #[must_use]
    pub(crate) fn with_attachment_bucket(
        mut self,
        resolver: Arc<dyn elitea_agent_runtime::toolkits::AttachmentBucketResolver>,
    ) -> Self {
        self.attachment_bucket = Some(resolver);
        self
    }

    /// The project's attachment bucket name, when it could be resolved.
    async fn attachment_bucket(&self) -> Option<String> {
        let resolver = self.attachment_bucket.clone()?;
        self.resolved
            .get_or_init(|| async move { resolver.attachment_bucket().await.ok() })
            .await
            .clone()
    }
}

/// The default chat attachment bucket and the index result bucket. Main holds
/// the authoritative rule (`bucket_type`); this is the worker's own check.
const DEFAULT_ATTACHMENT_BUCKET: &str = "chat-attachments";
const INDEX_ARTIFACT_BUCKET: &str = "index-artifacts";

/// Whether an Office object request may address `bucket`/`key`, given the
/// project's resolved attachment bucket name: the index result bucket never;
/// the attachment bucket (the resolved name, and the default whatever the
/// policy says) only under the turn's own conversation; every other bucket
/// (a person's, or a provisioned `reports`/`tasks` system bucket) as its
/// per-bucket access list allows (review rounds 2 and 3).
fn office_object_allowed(
    bucket: &str,
    key: Option<&str>,
    claim: Option<&OfficeClaim>,
    attachment_bucket: Option<&str>,
) -> bool {
    if bucket == INDEX_ARTIFACT_BUCKET {
        return false;
    }
    if bucket != DEFAULT_ATTACHMENT_BUCKET && Some(bucket) != attachment_bucket {
        return true;
    }
    let Some(conversation) = claim.and_then(|claim| claim.conversation.as_deref()) else {
        return false;
    };
    key.is_some_and(|key| {
        key.strip_prefix(conversation)
            .and_then(|rest| rest.strip_prefix('/'))
            .is_some_and(|rest| !rest.is_empty())
    })
}

#[cfg(test)]
pub(crate) fn office_object_allowed_for_test(
    bucket: &str,
    key: Option<&str>,
    conversation: Option<&str>,
    attachment_bucket: Option<&str>,
) -> bool {
    let claim = OfficeClaim {
        execution_id: String::new(),
        generation: 1,
        claim_id: String::new(),
        fence: String::new(),
        conversation: conversation.map(str::to_owned),
        attachment_bucket: None,
        resolved: Arc::new(tokio::sync::OnceCell::new()),
    };
    office_object_allowed(bucket, key, Some(&claim), attachment_bucket)
}

fn canonical_uuid(value: &str) -> bool {
    value.len() == 36
        && value.char_indices().all(|(index, character)| {
            if matches!(index, 8 | 13 | 18 | 23) {
                character == '-'
            } else {
                character.is_ascii_digit() || ('a'..='f').contains(&character)
            }
        })
}

/// The Office host of one claimed turn.
pub(crate) struct ClaimOfficeHost {
    runtime: Arc<OfficeRuntime>,
    /// The turn's credential and project (and, with a claim, the claim).
    caller: Caller,
    model: LazyStepbackModel,
    project_id: String,
    /// The claim named on every object request.
    claim: Option<OfficeClaim>,
    /// The gateway's images routes under the claim (the `imagegen` toolkit,
    /// #864); `None` leaves that toolkit out as unsupported.
    images: Option<(ImagesClient, Arc<ClaimScopedEliteaContext>)>,
    /// The claim's toolkit completion budget the image calls are charged to
    /// (the chat prompts are charged through the model binding).
    budget: Option<ModelCallBudget>,
}

impl ClaimOfficeHost {
    /// A host acting with `bearer` (the turn's model-gateway credential) in
    /// `project_id`, calling `model` for completions.
    ///
    /// # Errors
    ///
    /// The bearer or project are not usable for the artifact store.
    pub(crate) fn new(
        runtime: Arc<OfficeRuntime>,
        bearer: &str,
        project_id: &str,
        model: LazyStepbackModel,
    ) -> Result<Self, OfficeRuntimeError> {
        let caller = Caller::parse(bearer, project_id).ok_or(OfficeRuntimeError)?;
        Ok(Self {
            runtime,
            caller,
            model,
            project_id: project_id.to_owned(),
            claim: None,
            images: None,
            budget: None,
        })
    }

    /// This host naming `claim` on every object request.
    #[must_use]
    pub(crate) fn with_claim(mut self, claim: OfficeClaim) -> Self {
        self.caller = self.caller.clone().with_claim(claim.runtime_claim());
        self.claim = Some(claim);
        self
    }

    /// The claim's toolkit completion budget, shared with the chat binding:
    /// every image call takes one unit of it.
    #[must_use]
    pub(crate) fn with_budget(mut self, budget: ModelCallBudget) -> Self {
        self.budget = Some(budget);
        self
    }

    /// This host with the gateway's images API, called under `context`'s
    /// credential, project and execution (the same attribution, budget and
    /// request log as the turn's chat calls).
    #[must_use]
    pub(crate) fn with_images(
        mut self,
        client: ImagesClient,
        context: Arc<ClaimScopedEliteaContext>,
    ) -> Self {
        self.images = Some((client, context));
        self
    }

    /// One unit of the claim's toolkit completion budget for an image call
    /// (generation or edit; `n` images are still ONE call): the same budget
    /// the chat prompts and the step-back tools draw from.
    fn charge_image_call(&self) -> Result<(), OfficeHostError> {
        match &self.budget {
            Some(budget) if !budget.try_take() => Err(OfficeHostError::toolkit_budget()),
            _ => Ok(()),
        }
    }

    /// Gives the unit back for an image call that was never sent (the
    /// request was too large, a file name unusable, a header malformed).
    fn refund_unsent_image_call(&self, error: &ImagesError) {
        if let Some(budget) = &self.budget
            && matches!(
                error,
                ImagesError::RequestTooLarge
                    | ImagesError::InvalidFileName
                    | ImagesError::Malformed
            )
        {
            budget.refund();
        }
    }

    /// The worker's system-bucket check for one object (see
    /// [`office_object_allowed`]), before any request is sent.
    async fn check_object(&self, bucket: &str, name: &str) -> Result<(), OfficeHostError> {
        let attachment_bucket = match &self.claim {
            Some(claim) => claim.attachment_bucket().await,
            None => None,
        };
        if office_object_allowed(
            bucket,
            Some(name),
            self.claim.as_ref(),
            attachment_bucket.as_deref(),
        ) {
            Ok(())
        } else {
            Err(OfficeHostError::new(
                OfficeFailure::AccessRefused,
                "office_host.system_bucket_refused",
            ))
        }
    }

    /// The worker's name check, before any call is made.
    fn check_names(bucket: &str, key: Option<&str>) -> Result<(), OfficeHostError> {
        // A control character is refused as the multipart encoder refused
        // it: a model-chosen name never carries a line break into a key.
        if !valid_segment(bucket)
            || key.is_some_and(|k| !valid_object_key(k) || k.chars().any(char::is_control))
        {
            return Err(OfficeHostError::new(
                OfficeFailure::InvalidRequest,
                "office_host.invalid_name",
            ));
        }
        Ok(())
    }
}

/// An artifact store failure as the host classifies it: each real cause its
/// own kind, so the tool names it. A refusal is classified by the HTTP status
/// the route decided.
pub(crate) fn failure(error: &ArtifactError) -> OfficeHostError {
    let (kind, reason) = match error {
        ArtifactError::Refused(refusal) => match refusal.http_status {
            404 => (OfficeFailure::NotFound, "office_host.not_found"),
            401 | 403 => (OfficeFailure::AccessRefused, "office_host.access_refused"),
            409 => (OfficeFailure::AlreadyExists, "office_host.already_exists"),
            415 => (
                OfficeFailure::UnsupportedType,
                "office_host.unsupported_type",
            ),
            400 | 422 => (OfficeFailure::InvalidRequest, "office_host.invalid_request"),
            413 => (OfficeFailure::TooLarge, "office_host.too_large"),
            _ => (
                OfficeFailure::Unreachable,
                "office_host.dependency_unavailable",
            ),
        },
        ArtifactError::TooLarge(_) => (OfficeFailure::TooLarge, "office_host.too_large"),
        // A name the call cannot carry: nothing was sent.
        ArtifactError::Invalid(_) => (OfficeFailure::InvalidRequest, "office_host.invalid_name"),
        ArtifactError::Hop(_) | ArtifactError::Protocol(_) => (
            OfficeFailure::Unreachable,
            "office_host.dependency_unavailable",
        ),
    };
    OfficeHostError::new(kind, reason)
}

/// A model-call failure, classified on the error's stable CODE (never its
/// message): the claim's toolkit cap and the gateway's budgets are budget
/// refusals, each with its own sentence; a reply the model spent on
/// reasoning, or one cut off or oversized, is an unusable reply (not an
/// unreachable platform); the gateway's own refusals keep their kinds.
pub(crate) fn model_failure(error: &adk_rust::AdkError) -> OfficeHostError {
    use crate::indexing::tools::reply_codes;
    use crate::transport::model_text::TOOLKIT_BUDGET_EXHAUSTED_CODE;
    let (kind, reason) = match error.code {
        TOOLKIT_BUDGET_EXHAUSTED_CODE => return OfficeHostError::toolkit_budget(),
        "model_gateway.budget_exhausted"
        | "model_gateway.member_budget_exhausted"
        | "model_gateway.project_budget_exhausted" => {
            (OfficeFailure::BudgetRefused, "office_host.budget_refused")
        }
        // Round 4: each reply cause its own kind and sentence. A cut reply,
        // or one that spent the limit reasoning, is cured by more max_tokens.
        reply_codes::REASONING_ONLY | reply_codes::LENGTH_CUT => (
            OfficeFailure::ReplyCutOff,
            "office_host.model_reply_cut_off",
        ),
        reply_codes::DECLINED => (
            OfficeFailure::ReplyDeclined,
            "office_host.model_reply_declined",
        ),
        // The reply came back over a bound (it was sent, so it was charged).
        reply_codes::TOO_LARGE
        | "model_gateway.stream_too_large"
        | "model_gateway.event_too_large"
        | "model_gateway.output_too_large"
        | "model_gateway.too_many_events"
        | "anthropic_gateway.output_too_large" => (
            OfficeFailure::ReplyTooLarge,
            "office_host.model_reply_too_large",
        ),
        "model_gateway.unauthorized" | "model_gateway.forbidden" => (
            OfficeFailure::AccessRefused,
            "office_host.model_call_refused",
        ),
        "model_gateway.rate_limited" => {
            (OfficeFailure::RateLimited, "office_host.model_rate_limited")
        }
        "model_gateway.upstream_timeout"
        | "model_gateway.response_header_timeout"
        | "model_gateway.stream_idle_timeout"
        | "anthropic_gateway.response_header_timeout" => {
            (OfficeFailure::Timeout, "office_host.model_timeout")
        }
        // The native Anthropic facade refuses a sampling setting the model
        // does not take: the run's model is not usable for the call.
        "anthropic_gateway.sampling_unsupported" => {
            (OfficeFailure::ModelUnusable, "office_host.model_unusable")
        }
        "model_gateway.conflict" => (OfficeFailure::Conflict, "office_host.model_conflict"),
        // Explicit, as on the OpenAI-compatible side (#1292 round 4).
        "model_gateway.transport"
        | "model_gateway.stream_transport"
        | "anthropic_gateway.transport" => {
            (OfficeFailure::Unreachable, "office_host.model_call_failed")
        }
        // Live defect D3: a request the gateway (or the facade's own
        // admission) refused as invalid is not "the platform could not be
        // reached"; nothing was unreachable and retrying cannot help.
        _ if error.category == adk_rust::ErrorCategory::InvalidInput => (
            OfficeFailure::InvalidRequest,
            "office_host.model_invalid_request",
        ),
        _ => (OfficeFailure::Unreachable, "office_host.model_call_failed"),
    };
    OfficeHostError::new(kind, reason)
}

/// Why the run's chat model could not be bound for one call (#1292 round 4):
/// no model at all (a pipeline node) is `NoModel`; the CLAIM's context (its
/// credential, execution id or project) is `ContextUnusable`, which retrying
/// the turn cures; anything about the model or its settings is
/// `ModelUnusable`; the gateway being down is `Unreachable`.
pub(crate) fn bind_failure(
    error: Option<super::model_facade::ModelFacadeError>,
) -> OfficeHostError {
    use super::model_facade::ModelFacadeError as E;
    let (kind, reason) = match error {
        None => (OfficeFailure::NoModel, "office_host.no_model"),
        Some(E::InvalidClaimContext) => (
            OfficeFailure::ContextUnusable,
            "office_host.context_unusable",
        ),
        Some(E::InvalidConfiguration | E::InvalidInvocation | E::ResourceExhausted) => {
            (OfficeFailure::ModelUnusable, "office_host.model_unusable")
        }
        Some(E::DependencyUnavailable) => (OfficeFailure::Unreachable, "office_host.model_unbound"),
    };
    OfficeHostError::new(kind, reason)
}

/// How long the sidecar may take to read one document for a tool call.
const EXTRACT_TIMEOUT: std::time::Duration = std::time::Duration::from_mins(1);

/// A sidecar skip as the tool tells it.
pub(crate) fn text_failure(reason: SkipReason) -> TextExtractionFailure {
    match reason {
        SkipReason::TooLarge | SkipReason::TooManyPages | SkipReason::OutputTooLarge => {
            TextExtractionFailure::TooLarge
        }
        SkipReason::Encrypted | SkipReason::Corrupt | SkipReason::Empty => {
            TextExtractionFailure::Unreadable
        }
        SkipReason::Unsupported => TextExtractionFailure::Unsupported,
        SkipReason::Timeout => TextExtractionFailure::TimedOut,
    }
}

/// Whether a file of this name can be read as text here: a kind the readers
/// handle (by its extension), and, for a PDF or Office file, a sidecar. Answered
/// from the name alone, so a tool refuses before it downloads.
pub(crate) fn readable_kind(has_extractor: bool, name: &str) -> Result<(), TextExtractionFailure> {
    let mime = elitea_content_source::mime_of(name);
    if mime == "application/octet-stream" || mime.starts_with("image/") {
        return Err(TextExtractionFailure::Unsupported);
    }
    if elitea_doc_extract::is_document_type(mime) && !has_extractor {
        return Err(TextExtractionFailure::NoExtractor);
    }
    Ok(())
}

/// The text of a document, by its name's extension: a PDF or Office file by
/// the sidecar, plain text and HTML here. A kind with no extension in the
/// table, or an image, is not text.
async fn document_text(
    extractor: Option<&Arc<ExtractorClient>>,
    name: &str,
    bytes: Vec<u8>,
) -> Result<String, TextExtractionFailure> {
    readable_kind(extractor.is_some(), name)?;
    let mime = elitea_content_source::mime_of(name);
    if elitea_doc_extract::is_document_type(mime) {
        let extractor = extractor.ok_or(TextExtractionFailure::NoExtractor)?;
        return match extractor
            .extract(
                name,
                mime,
                bytes,
                elitea_agent_runtime::toolkits::MAX_OFFICE_OBJECT_BYTES,
                EXTRACT_TIMEOUT,
                None,
            )
            .await
        {
            Ok(Extraction::Text(document)) => Ok(document.text),
            Ok(Extraction::Skipped(reason)) => Err(text_failure(reason)),
            Err(error) => {
                tracing::warn!(
                    event = "toolkit_document_extract_failed",
                    error = %error,
                    "the document extraction sidecar did not answer"
                );
                Err(TextExtractionFailure::Unreachable)
            }
        };
    }
    let options = elitea_doc_extract::ExtractOptions {
        html_to_markdown: true,
    };
    let mime = mime.to_owned();
    let decoded = tokio::task::spawn_blocking(move || {
        elitea_doc_extract::extract_with(&mime, &bytes, &options)
    })
    .await
    .map_err(|_| TextExtractionFailure::Unreachable)?;
    match decoded {
        elitea_doc_extract::Extracted::Text { text, .. } => Ok(text),
        elitea_doc_extract::Extracted::Unsupported => Err(TextExtractionFailure::Unsupported),
        elitea_doc_extract::Extracted::Unreadable(_) => Err(TextExtractionFailure::Unreadable),
    }
}

/// The Office host of a direct toolkit call of `imagegen`: it claims to serve
/// the images API so the toolkit materializes and its tools are listed, and it
/// does nothing else. The tools write artifacts, `toolkit.call_tool.v1` runs
/// read-only tools only, and the runtime refuses them before one runs; so no
/// claim is redeemed and no platform call is made for it.
pub(crate) struct NoRoundTripImagesHost;

fn direct_path_refusal() -> OfficeHostError {
    OfficeHostError::new(
        OfficeFailure::NoModel,
        "office_host.direct_path_no_round_trip",
    )
}

#[async_trait]
impl OfficeToolHost for NoRoundTripImagesHost {
    fn serves_images(&self) -> bool {
        true
    }

    async fn read_object(&self, _: &str, _: &str, _: u64) -> Result<Vec<u8>, OfficeHostError> {
        Err(direct_path_refusal())
    }

    async fn write_object(
        &self,
        _: &str,
        _: &str,
        _: &str,
        _: Vec<u8>,
        _: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        Err(direct_path_refusal())
    }

    async fn complete(&self, _: OfficeModelRequest) -> Result<String, OfficeHostError> {
        Err(direct_path_refusal())
    }
}

/// An images-API failure as the host classifies it: each real cause its own
/// kind, so the tool names it (a rate limit is not a lost connection).
pub(crate) fn images_failure(error: &ImagesError) -> OfficeHostError {
    let (kind, reason) = match error {
        ImagesError::Refused { refusal } => match refusal {
            Refusal::BudgetExhausted(_) => {
                (OfficeFailure::BudgetRefused, "office_host.budget_refused")
            }
            Refusal::Unauthorized | Refusal::Forbidden => {
                (OfficeFailure::AccessRefused, "office_host.images_refused")
            }
            Refusal::Rejected => (OfficeFailure::InvalidRequest, "office_host.images_rejected"),
            Refusal::RateLimited => (
                OfficeFailure::RateLimited,
                "office_host.images_rate_limited",
            ),
            Refusal::UpstreamTimeout => (OfficeFailure::Timeout, "office_host.images_timeout"),
            Refusal::Conflict => (OfficeFailure::Conflict, "office_host.images_conflict"),
            Refusal::Unavailable => (OfficeFailure::Unreachable, "office_host.images_unavailable"),
        },
        ImagesError::RequestTooLarge => (
            OfficeFailure::TooLarge,
            "office_host.images_request_too_large",
        ),
        ImagesError::ReplyTooLarge => (
            OfficeFailure::ReplyTooLarge,
            "office_host.images_reply_too_large",
        ),
        ImagesError::UnusableReply(_) => (
            OfficeFailure::ReplyUnusable,
            "office_host.images_reply_unusable",
        ),
        ImagesError::Malformed => (
            OfficeFailure::InvalidRequest,
            "office_host.images_malformed",
        ),
        ImagesError::InvalidFileName => (
            OfficeFailure::InvalidRequest,
            "office_host.images_file_name",
        ),
        ImagesError::Transport => (OfficeFailure::Unreachable, "office_host.images_unavailable"),
    };
    OfficeHostError::new(kind, reason)
}

#[async_trait]
impl OfficeToolHost for ClaimOfficeHost {
    fn serves_images(&self) -> bool {
        self.images.is_some()
    }

    async fn generate_images(
        &self,
        request: ImageGenerateRequest,
    ) -> Result<Vec<Option<Vec<u8>>>, OfficeHostError> {
        let Some((client, context)) = &self.images else {
            return Err(OfficeHostError::new(
                OfficeFailure::NoModel,
                "office_host.no_images_api",
            ));
        };
        let call = ImageCall {
            model: &request.model,
            prompt: &request.prompt,
            size: request.size.as_deref(),
            count: request.count,
        };
        self.charge_image_call()?;
        client.generate(context, &call).await.map_err(|error| {
            tracing::warn!(event = "imagegen_call_failed", error = %error, "an image generation call failed");
            self.refund_unsent_image_call(&error);
            images_failure(&error)
        })
    }

    async fn edit_images(
        &self,
        request: ImageEditRequest,
    ) -> Result<Vec<Option<Vec<u8>>>, OfficeHostError> {
        let Some((client, context)) = &self.images else {
            return Err(OfficeHostError::new(
                OfficeFailure::NoModel,
                "office_host.no_images_api",
            ));
        };
        let ImageEditRequest {
            model,
            prompt,
            image,
            mask,
        } = request;
        let call = ImageCall {
            model: &model,
            prompt: &prompt,
            size: None,
            count: 1,
        };
        self.charge_image_call()?;
        client
            .edit(context, &call, image, mask)
            .await
            .map_err(|error| {
                tracing::warn!(event = "imagegen_call_failed", error = %error, "an image edit call failed");
                self.refund_unsent_image_call(&error);
                images_failure(&error)
            })
    }

    fn can_read_text(&self, name: &str) -> Result<(), TextExtractionFailure> {
        readable_kind(self.runtime.extractor.is_some(), name)
    }

    async fn extract_text(
        &self,
        name: &str,
        bytes: Vec<u8>,
    ) -> Result<String, TextExtractionFailure> {
        document_text(self.runtime.extractor.as_ref(), name, bytes).await
    }

    async fn object_size(&self, bucket: &str, name: &str) -> Result<Option<u64>, OfficeHostError> {
        // The object's info: nothing is read.
        Self::check_names(bucket, Some(name))?;
        self.check_object(bucket, name).await?;
        let info = self
            .runtime
            .artifacts
            .stat(&self.caller, bucket, name)
            .await
            .map_err(|e| failure(&e))?;
        Ok(u64::try_from(info.size_bytes).ok())
    }

    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        // The object's info arrives before its bytes, so a large object is
        // refused from its declared size without being read.
        Self::check_names(bucket, Some(name))?;
        self.check_object(bucket, name).await?;
        let reader = self
            .runtime
            .artifacts
            .get_object(&self.caller, bucket, name)
            .await
            .map_err(|e| failure(&e))?;
        reader.read_to_end(max_bytes).await.map_err(|e| failure(&e))
    }

    async fn write_object(
        &self,
        bucket: &str,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        Self::check_names(bucket, Some(name))?;
        if media_type.chars().any(char::is_control) {
            return Err(OfficeHostError::new(
                OfficeFailure::InvalidRequest,
                "office_host.invalid_name",
            ));
        }
        self.check_object(bucket, name).await?;
        let byte_length = bytes.len() as u64;
        // The server refuses a taken name (409) unless the write asks to
        // replace it.
        let info = self
            .runtime
            .artifacts
            .put_object(
                &self.caller,
                PutObject {
                    bucket: bucket.to_owned(),
                    key: name.to_owned(),
                    media_type: media_type.to_owned(),
                    overwrite,
                },
                bytes.into(),
            )
            .await
            .map_err(|e| failure(&e))?;
        let echoed = |value: String, fallback: &str| {
            if value.is_empty() {
                fallback.to_owned()
            } else {
                value
            }
        };
        Ok(OfficeObjectWritten {
            bucket: bucket.to_owned(),
            name: echoed(info.key, name),
            media_type: echoed(info.media_type, media_type),
            byte_length: u64::try_from(info.size_bytes)
                .ok()
                .filter(|size| *size > 0 || byte_length == 0)
                .unwrap_or(byte_length),
        })
    }

    /// The link the SDK's chart answer used
    /// (`{base}/api/v2/artifacts/artifact/default/{project}/{bucket}/{file}`).
    /// `base` is the deployment's PUBLIC base URL when it has one; the
    /// platform origin this host reaches is the in-cluster edge, which a
    /// browser cannot open, so it is never used. With no public base URL the
    /// link is root-relative and the chat UI resolves it against its own
    /// origin.
    fn object_link(&self, bucket: &str, name: &str) -> Option<String> {
        if !valid_segment(bucket) || !valid_object_key(name) {
            return None;
        }
        let base = self.runtime.public_base_url.as_deref();
        // Percent-encoding comes from the URL type's path-segment rules.
        let mut url = Url::parse(base.unwrap_or("https://relative.invalid/")).ok()?;
        {
            let mut segments = url.path_segments_mut().ok()?;
            segments.pop_if_empty().extend([
                "api",
                "v2",
                "artifacts",
                "artifact",
                "default",
                self.project_id.as_str(),
                bucket,
            ]);
            segments.extend(name.split('/'));
        }
        Some(if base.is_some() {
            url.to_string()
        } else {
            url.path().to_owned()
        })
    }

    async fn create_bucket(
        &self,
        name: &str,
        retention_days: Option<u32>,
    ) -> Result<OfficeBucketCreated, OfficeHostError> {
        // The platform's own rule (`^[a-z][a-z0-9-]{1,62}$`), checked here too
        // so a name that could never exist is not sent.
        if !valid_segment(name) || name.len() < 2 || name.len() > 63 {
            return Err(OfficeHostError::new(
                OfficeFailure::InvalidRequest,
                "office_host.invalid_name",
            ));
        }
        let created = self
            .runtime
            .artifacts
            .create_bucket(
                &self.caller,
                name,
                retention_days.and_then(|days| i32::try_from(days).ok()),
            )
            .await
            .map_err(|e| failure(&e))?;
        Ok(OfficeBucketCreated {
            name: if created.name.is_empty() {
                name.to_owned()
            } else {
                created.name
            },
            retention_days: created
                .retention_days
                .and_then(|days| u32::try_from(days).ok())
                .or(retention_days),
        })
    }

    async fn complete(&self, request: OfficeModelRequest) -> Result<String, OfficeHostError> {
        let binding = self.model.try_bind().map_err(bind_failure)?;
        let mut parts = vec![Part::Text {
            text: request.prompt,
        }];
        for (index, image) in request.images_png.iter().enumerate() {
            parts.push(Part::InlineData {
                mime_type: request
                    .image_media_types
                    .get(index)
                    .map_or("image/png", String::as_str)
                    .to_owned(),
                data: image.clone(),
                uri: None,
                annotations: None,
            });
        }
        generate_text(&binding, parts).await.map_err(|error| {
            tracing::warn!(
                event = "office_model_call_failed",
                error_code = error.code,
                "a document tool's model call failed"
            );
            model_failure(&error)
        })
    }
}
