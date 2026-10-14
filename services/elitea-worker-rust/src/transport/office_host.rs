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
    ImageEditRequest, ImageGenerateRequest, OfficeFailure, OfficeHostError, OfficeModelRequest,
    OfficeObjectWritten, OfficeToolHost,
};
use elitea_connectors::artifact::client::{ArtifactPlatform, valid_object_key, valid_segment};
use elitea_connectors::egress::HostAllowlist;
use elitea_connectors::source::{Failure, SourceHttp, SourceLimits};
use elitea_connectors::transport::{Body, Method, Url, header};
use elitea_llm_wire::refusal::Refusal;
use serde_json::Value;

use super::images_facade::{EditFile, ImageCall, ImagesClient, ImagesError};
use super::multipart::Multipart;
use super::runtime_context::ClaimScopedEliteaContext;
use crate::indexing::tools::{LazyStepbackModel, generate_text};

/// What every Office host of the worker shares: the platform origin and the
/// guarded HTTP client to it.
pub(crate) struct OfficeRuntime {
    origin: String,
    http: SourceHttp,
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
        let transport = elitea_connectors::reqwest012::ReqwestTransport::hardened(
            &elitea_connectors::github::client::CLIENT_POLICY,
        )
        .map_err(|_| OfficeRuntimeError)?;
        let limits = SourceLimits {
            max_document_bytes: elitea_agent_runtime::toolkits::MAX_OFFICE_OBJECT_BYTES,
            ..SourceLimits::default()
        };
        Ok(Self {
            origin: platform_origin.to_owned(),
            http: SourceHttp::new(HostAllowlist::parse(Some(&host)), Arc::new(transport))
                .with_limits(limits),
        })
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
            http: SourceHttp::new(HostAllowlist::parse(Some(&host)), transport).with_limits(
                SourceLimits {
                    max_document_bytes: elitea_agent_runtime::toolkits::MAX_OFFICE_OBJECT_BYTES,
                    ..SourceLimits::default()
                },
            ),
        }
    }
}

/// The Office host of one claimed turn.
pub(crate) struct ClaimOfficeHost {
    runtime: Arc<OfficeRuntime>,
    platform: ArtifactPlatform,
    model: LazyStepbackModel,
    /// The gateway's images routes under the claim (the `imagegen` toolkit,
    /// #864); `None` leaves that toolkit out as unsupported.
    images: Option<(ImagesClient, Arc<ClaimScopedEliteaContext>)>,
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
            images: None,
        })
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

/// A model-call failure: the gateway's budget refusal is its own cause.
pub(crate) fn model_failure(message: &str) -> OfficeHostError {
    if message.contains("budget is exhausted") {
        OfficeHostError::new(OfficeFailure::BudgetRefused, "office_host.budget_refused")
    } else {
        OfficeHostError::new(OfficeFailure::Unreachable, "office_host.model_call_failed")
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
        ImagesError::TooLarge => (OfficeFailure::TooLarge, "office_host.images_too_large"),
        ImagesError::InvalidFileName => (
            OfficeFailure::InvalidRequest,
            "office_host.images_file_name",
        ),
        ImagesError::Transport | ImagesError::InvalidResponse(_) => {
            (OfficeFailure::Unreachable, "office_host.images_unavailable")
        }
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
        client.generate(context, &call).await.map_err(|error| {
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
        // The image and mask bytes are moved down to the encoder, which copies
        // them once, into the body.
        let image = EditFile {
            name: image.name,
            bytes: image.bytes,
        };
        let mask = mask.map(|mask| EditFile {
            name: mask.name,
            bytes: mask.bytes,
        });
        client
            .edit(context, &call, image, mask)
            .await
            .map_err(|error| {
                tracing::warn!(event = "imagegen_call_failed", error = %error, "an image edit call failed");
                images_failure(&error)
            })
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
        for image in request.images_png.iter() {
            parts.push(Part::InlineData {
                mime_type: "image/png".to_owned(),
                data: image.clone(),
                uri: None,
                annotations: None,
            });
        }
        generate_text(binding, parts).await.map_err(|message| {
            tracing::warn!(
                event = "office_model_call_failed",
                error = %message,
                "a document tool's model call failed"
            );
            model_failure(&message)
        })
    }
}
