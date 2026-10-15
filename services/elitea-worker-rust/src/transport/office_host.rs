//! The Office toolkit families' host for one claimed turn (ADR-0032 P3):
//! binary artifacts in and out, and the run's chat model.
//!
//! **Objects.** The `artifact` family moves text through the claim-bound
//! runtime-context routes; a `.pptx` is bytes. The bytes go through the
//! platform's own object API (`/api/v2/artifacts/objects/{project}/{bucket}`),
//! the route the indexing artifact source already reads, under the turn's own
//! model-gateway credential, so elitea-main applies the project and the
//! per-bucket access list of the person the turn runs as. No new route and no
//! new internal REST: the request is an ordinary authenticated client call to
//! the platform origin, and the only host it may reach is that origin (the
//! egress guard is built from it, fail-closed).
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
use elitea_connectors::artifact::client::{ArtifactPlatform, valid_object_key, valid_segment};
use elitea_connectors::egress::HostAllowlist;
use elitea_connectors::source::{Failure, SourceHttp, SourceLimits};
use elitea_connectors::transport::{Body, Method, Url, header};
use elitea_llm_wire::refusal::Refusal;
use serde_json::Value;

use super::extractor_client::{Extraction, ExtractorClient, SkipReason};
use super::images_facade::{ImageCall, ImagesClient, ImagesError};
use super::multipart::Multipart;
use super::runtime_context::ClaimScopedEliteaContext;
use super::toolkit_completion::ModelCallBudget;
use crate::indexing::tools::{LazyStepbackModel, generate_text};

/// The host's HTTP client: the GitHub client's timeouts and limits, with
/// protocol-level retries OFF. Object uploads and bucket creation are effects:
/// a retried POST is a second write and hides a lost answer.
pub(crate) const OFFICE_POLICY: elitea_connectors::ClientPolicy = elitea_connectors::ClientPolicy {
    disable_retries: true,
    ..elitea_connectors::github::client::CLIENT_POLICY
};

/// What every Office host of the worker shares: the platform origin and the
/// guarded HTTP client to it.
pub(crate) struct OfficeRuntime {
    origin: String,
    /// The deployment's public base URL, when it has stated one.
    public_base_url: Option<String>,
    http: SourceHttp,
    /// The document extraction sidecar, when the deployment names it: the
    /// toolkit tools that read a PDF or Office file as text (`SharePoint`
    /// `read_document`) ask it. Absent, those tools say so.
    extractor: Option<Arc<ExtractorClient>>,
}

/// The platform origin is not a usable `https://host[:port]`, or the client
/// cannot be built.
#[derive(Debug)]
pub(crate) struct OfficeRuntimeError;

impl OfficeRuntime {
    /// A runtime that reaches `platform_origin` and nothing else.
    pub(crate) fn new(platform_origin: &str) -> Result<Self, OfficeRuntimeError> {
        let host = Url::parse(platform_origin)
            .ok()
            .and_then(|url| url.host_str().map(str::to_owned))
            .ok_or(OfficeRuntimeError)?;
        let transport = elitea_connectors::reqwest012::ReqwestTransport::hardened(&OFFICE_POLICY)
            .map_err(|_| OfficeRuntimeError)?;
        let limits = SourceLimits {
            max_document_bytes: elitea_agent_runtime::toolkits::MAX_OFFICE_OBJECT_BYTES,
            ..SourceLimits::default()
        };
        Ok(Self {
            origin: platform_origin.to_owned(),
            public_base_url: None,
            http: SourceHttp::new(HostAllowlist::parse(Some(&host)), Arc::new(transport))
                .with_limits(limits),
            extractor: None,
        })
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

    /// Whether the guard admits `url`'s host.
    #[cfg(test)]
    pub(crate) fn permits(&self, url: &str) -> bool {
        Url::parse(url).is_ok_and(|url| self.http.permits(&url))
    }

    #[cfg(test)]
    pub(crate) fn with_transport(
        platform_origin: &str,
        transport: Arc<dyn elitea_connectors::transport::Transport>,
    ) -> Self {
        let host = Url::parse(platform_origin)
            .ok()
            .and_then(|url| url.host_str().map(str::to_owned))
            .unwrap_or_default();
        Self {
            origin: platform_origin.to_owned(),
            public_base_url: None,
            http: SourceHttp::new(HostAllowlist::parse(Some(&host)), transport).with_limits(
                SourceLimits {
                    max_document_bytes: elitea_agent_runtime::toolkits::MAX_OFFICE_OBJECT_BYTES,
                    ..SourceLimits::default()
                },
            ),
            extractor: None,
        }
    }
}

/// The Office host of one claimed turn.
pub(crate) struct ClaimOfficeHost {
    runtime: Arc<OfficeRuntime>,
    platform: ArtifactPlatform,
    model: LazyStepbackModel,
    project_id: String,
    /// The gateway's images routes under the claim (the `imagegen` toolkit,
    /// #864); `None` leaves that toolkit out as unsupported.
    images: Option<(ImagesClient, Arc<ClaimScopedEliteaContext>)>,
    /// The run's ONE toolkit model-call budget (shared with the step-back
    /// tools and this host's prompts): one unit per image call sent. `None`
    /// on a path that sets none (a direct tool run, a pipeline node).
    budget: Option<ModelCallBudget>,
}

impl ClaimOfficeHost {
    /// A host acting with `bearer` (the turn's model-gateway credential) in
    /// `project_id`, calling `model` for completions.
    ///
    /// # Errors
    ///
    /// The origin, bearer or project are not usable for the object API.
    pub(crate) fn new(
        runtime: Arc<OfficeRuntime>,
        bearer: &str,
        project_id: &str,
        model: LazyStepbackModel,
    ) -> Result<Self, OfficeRuntimeError> {
        let platform = ArtifactPlatform::new(&runtime.origin, bearer, project_id)
            .map_err(|_| OfficeRuntimeError)?;
        Ok(Self {
            runtime,
            platform,
            model,
            project_id: project_id.to_owned(),
            images: None,
            budget: None,
        })
    }

    /// This host charging its image calls to the run's toolkit budget.
    #[must_use]
    pub(crate) fn with_budget(mut self, budget: ModelCallBudget) -> Self {
        self.budget = Some(budget);
        self
    }

    /// Takes one unit of the run's budget for an image call.
    fn charge_image_call(&self) -> Result<(), OfficeHostError> {
        match &self.budget {
            Some(budget) if !budget.try_take() => Err(OfficeHostError::new(
                OfficeFailure::RunCallCapReached,
                "office_host.run_call_cap",
            )),
            _ => Ok(()),
        }
    }

    /// Gives the unit back when the image call failed before it was sent.
    fn settle_image_call<T>(&self, result: &Result<T, ImagesError>) {
        if let (Some(budget), Err(error)) = (&self.budget, result)
            && images_failed_before_sending(error)
        {
            budget.refund();
        }
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

    fn object_request(
        &self,
        method: Method,
        bucket: &str,
        key: Option<&str>,
        overwrite: bool,
    ) -> Result<elitea_connectors::transport::Request, OfficeHostError> {
        if !valid_segment(bucket) || key.is_some_and(|k| !valid_object_key(k)) {
            return Err(OfficeHostError::new(
                OfficeFailure::InvalidRequest,
                "office_host.invalid_name",
            ));
        }
        let mut url = self.platform.objects_url(bucket, key);
        // The server refuses a taken name (409) unless the write asks to replace.
        if overwrite {
            url.query_pairs_mut().append_pair("overwrite", "true");
        }
        let mut request = self.platform.get(url).map_err(|_| {
            OfficeHostError::new(
                OfficeFailure::AccessRefused,
                "office_host.credential_unusable",
            )
        })?;
        *request.method_mut() = method;
        Ok(request)
    }
}

/// A platform object failure as the host classifies it: each real cause its
/// own kind, so the tool names it.
pub(crate) fn failure(error: &Failure) -> OfficeHostError {
    let (kind, reason) = match error {
        Failure::Status(status) => match status.as_u16() {
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
        Failure::TooLarge(_) => (OfficeFailure::TooLarge, "office_host.too_large"),
        // The egress guard does not admit the platform host, or the credential
        // does not fit a header: the file store was refused, not unreachable.
        Failure::Refused | Failure::Client(_) => {
            (OfficeFailure::AccessRefused, "office_host.egress_refused")
        }
        _ => (
            OfficeFailure::Unreachable,
            "office_host.dependency_unavailable",
        ),
    };
    OfficeHostError::new(kind, reason)
}

/// A model-call failure, classified on the error's CODE and CATEGORY (review
/// round 2: never on its message). The run's per-run cap and the gateway's
/// billing refusal are two causes with two sentences; a model the run cannot
/// bind is unusable, not an invalid request; a request the model gateway
/// refused as invalid is not "the platform could not be reached" (D3).
pub(crate) fn model_failure(error: &adk_rust::AdkError) -> OfficeHostError {
    use super::toolkit_completion::TOOLKIT_BUDGET_EXHAUSTED_CODE;
    use adk_rust::ErrorCategory;
    let kind = |kind, reason| OfficeHostError::new(kind, reason);
    match error.code {
        // The per-run cap: its own cause, not a billing refusal.
        TOOLKIT_BUDGET_EXHAUSTED_CODE => {
            return kind(OfficeFailure::RunCallCapReached, "office_host.run_call_cap");
        }
        "model_gateway.budget_exhausted"
        | "model_gateway.project_budget_exhausted"
        | "model_gateway.member_budget_exhausted" => {
            return kind(OfficeFailure::BudgetRefused, "office_host.budget_refused");
        }
        // The run's model could not be bound for this call: the model is
        // unusable here, not a request the gateway refused.
        "model_gateway.invalid_configuration" | "model_gateway.invalid_invocation" => {
            return kind(OfficeFailure::ModelUnusable, "office_host.model_unusable");
        }
        // The reply was over a bound on the way back (charged: it was sent).
        "model_gateway.stream_too_large"
        | "model_gateway.event_too_large"
        | "model_gateway.output_too_large"
        | "model_gateway.too_many_events"
        | "anthropic_gateway.output_too_large"
        | "toolkit_completion.reply_size" => {
            return kind(OfficeFailure::ReplyTooLarge, "office_host.reply_too_large");
        }
        "toolkit_completion.cut" | "toolkit_completion.cut_while_reasoning" => {
            return kind(OfficeFailure::ReplyCutOff, "office_host.reply_cut_off");
        }
        "toolkit_completion.declined" => {
            return kind(OfficeFailure::ReplyDeclined, "office_host.reply_declined");
        }
        "toolkit_completion.empty" => {
            return kind(OfficeFailure::ReplyEmpty, "office_host.reply_empty");
        }
        code if code.starts_with("toolkit_completion.") && code != "toolkit_completion.request" => {
            return kind(OfficeFailure::ReplyUnusable, "office_host.reply_unusable");
        }
        _ => {}
    }
    match error.category {
        ErrorCategory::InvalidInput => kind(
            OfficeFailure::InvalidRequest,
            "office_host.model_invalid_request",
        ),
        ErrorCategory::Unauthorized | ErrorCategory::Forbidden => {
            kind(OfficeFailure::AccessRefused, "office_host.model_refused")
        }
        ErrorCategory::RateLimited => {
            kind(OfficeFailure::RateLimited, "office_host.model_rate_limited")
        }
        ErrorCategory::Timeout => kind(OfficeFailure::Timeout, "office_host.model_timeout"),
        _ => kind(OfficeFailure::Unreachable, "office_host.model_call_failed"),
    }
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

/// Whether an images call failed locally, before the request was sent.
const fn images_failed_before_sending(error: &ImagesError) -> bool {
    matches!(
        error,
        ImagesError::RequestTooLarge | ImagesError::InvalidFileName | ImagesError::Malformed
    )
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
        let result = client.generate(context, &call).await;
        self.settle_image_call(&result);
        result.map_err(|error| {
            tracing::warn!(event = "imagegen_call_failed", error = %error, "an image generation call failed");
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
        let result = client.edit(context, &call, image, mask).await;
        self.settle_image_call(&result);
        result.map_err(|error| {
                tracing::warn!(event = "imagegen_call_failed", error = %error, "an image edit call failed");
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
        // A HEAD: the size is the declared Content-Length, and nothing is
        // buffered.
        let head = self.object_request(Method::HEAD, bucket, Some(name), false)?;
        let response = self
            .runtime
            .http
            .send(head)
            .await
            .map_err(|e| failure(&e))?;
        if !response.status().is_success() {
            return Err(failure(&Failure::Status(response.status())));
        }
        Ok(response
            .headers()
            .get(header::CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.trim().parse::<u64>().ok()))
    }

    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        // The size first, from a HEAD, so a large object is refused without
        // being buffered (and an existence check does not download a deck).
        let head = self.object_request(Method::HEAD, bucket, Some(name), false)?;
        let response = self
            .runtime
            .http
            .send(head)
            .await
            .map_err(|e| failure(&e))?;
        if !response.status().is_success() {
            return Err(failure(&Failure::Status(response.status())));
        }
        if response.declares_more_than(usize::try_from(max_bytes).unwrap_or(usize::MAX)) {
            return Err(failure(&Failure::TooLarge(max_bytes)));
        }
        drop(response);
        let get = self.object_request(Method::GET, bucket, Some(name), false)?;
        let bytes = self.runtime.http.raw(get).await.map_err(|e| failure(&e))?;
        if bytes.len() as u64 > max_bytes {
            return Err(failure(&Failure::TooLarge(max_bytes)));
        }
        Ok(bytes)
    }

    async fn write_object(
        &self,
        bucket: &str,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        if !valid_object_key(name) {
            return Err(OfficeHostError::new(
                OfficeFailure::InvalidRequest,
                "office_host.invalid_name",
            ));
        }
        let mut request = self.object_request(Method::POST, bucket, None, overwrite)?;
        let byte_length = bytes.len() as u64;
        // The one multipart encoder refuses a name or media type that could
        // end the header line.
        let mut form = Multipart::new();
        form.file("file", name, media_type, &bytes).map_err(|_| {
            OfficeHostError::new(OfficeFailure::InvalidRequest, "office_host.invalid_name")
        })?;
        let (content_type, body) = form.finish();
        request.headers_mut().insert(
            header::CONTENT_TYPE,
            content_type.parse().map_err(|_| {
                OfficeHostError::new(OfficeFailure::InvalidRequest, "office_host.upload_header")
            })?,
        );
        *request.body_mut() = Some(Body::from(body));
        let mut response = self
            .runtime
            .http
            .send(request)
            .await
            .map_err(|e| failure(&e))?;
        if !response.status().is_success() {
            return Err(failure(&Failure::Status(response.status())));
        }
        let reply = SourceHttp::body(&mut response, 64 * 1024)
            .await
            .map_err(|e| failure(&e))?;
        let reply: Value = serde_json::from_slice(&reply).map_err(|_| {
            OfficeHostError::new(OfficeFailure::Unreachable, "office_host.reply_not_json")
        })?;
        Ok(OfficeObjectWritten {
            bucket: bucket.to_owned(),
            name: reply
                .get("key")
                .and_then(Value::as_str)
                .unwrap_or(name)
                .to_owned(),
            media_type: reply
                .get("media_type")
                .and_then(Value::as_str)
                .unwrap_or(media_type)
                .to_owned(),
            byte_length: reply
                .get("size_bytes")
                .and_then(Value::as_u64)
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
        let mut request = self
            .platform
            .get(self.platform.buckets_url())
            .map_err(|_| {
                OfficeHostError::new(
                    OfficeFailure::AccessRefused,
                    "office_host.credential_unusable",
                )
            })?;
        *request.method_mut() = Method::POST;
        request.headers_mut().insert(
            header::CONTENT_TYPE,
            header::HeaderValue::from_static("application/json"),
        );
        let mut body = serde_json::json!({"name": name});
        if let (Some(days), Some(object)) = (retention_days, body.as_object_mut()) {
            object.insert("retention_days".to_owned(), serde_json::json!(days));
        }
        *request.body_mut() = Some(Body::from(body.to_string()));
        let mut response = self
            .runtime
            .http
            .send(request)
            .await
            .map_err(|e| failure(&e))?;
        if !response.status().is_success() {
            return Err(failure(&Failure::Status(response.status())));
        }
        // The bucket exists once the platform says 2xx; a reply that cannot be
        // read only costs the echo of what it stored.
        let reply = SourceHttp::body(&mut response, 64 * 1024)
            .await
            .ok()
            .and_then(|bytes| serde_json::from_slice::<Value>(&bytes).ok());
        Ok(OfficeBucketCreated {
            name: reply
                .as_ref()
                .and_then(|reply| reply.get("name"))
                .and_then(Value::as_str)
                .unwrap_or(name)
                .to_owned(),
            retention_days: reply
                .as_ref()
                .and_then(|reply| reply.get("retention_days"))
                .and_then(Value::as_u64)
                .and_then(|days| u32::try_from(days).ok())
                .or(retention_days),
        })
    }

    async fn complete(&self, request: OfficeModelRequest) -> Result<String, OfficeHostError> {
        let Some(binding) = self.model.get() else {
            return Err(OfficeHostError::new(
                OfficeFailure::NoModel,
                "office_host.no_model",
            ));
        };
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
        generate_text(binding, parts).await.map_err(|error| {
            tracing::warn!(
                event = "office_model_call_failed",
                error_code = error.code,
                error = %error,
                "a document tool's model call failed"
            );
            model_failure(&error)
        })
    }
}
