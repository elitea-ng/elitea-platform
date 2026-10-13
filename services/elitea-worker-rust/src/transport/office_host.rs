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
use elitea_agent_runtime::host::{HostError, HostErrorCode};
use elitea_agent_runtime::toolkits::{OfficeModelRequest, OfficeObjectWritten, OfficeToolHost};
use elitea_connectors::artifact::client::{ArtifactPlatform, valid_object_key, valid_segment};
use elitea_connectors::egress::HostAllowlist;
use elitea_connectors::source::{Failure, SourceHttp, SourceLimits};
use elitea_connectors::transport::{Body, Method, StatusCode, Url, header};
use serde_json::Value;

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
        })
    }

    fn object_request(
        &self,
        method: Method,
        bucket: &str,
        key: Option<&str>,
        overwrite: bool,
    ) -> Result<elitea_connectors::transport::Request, HostError> {
        if !valid_segment(bucket) || key.is_some_and(|k| !valid_object_key(k)) {
            return Err(HostError::new(
                HostErrorCode::InvalidInput,
                "the bucket or file name is not valid",
            ));
        }
        let mut url = self.platform.objects_url(bucket, key);
        if overwrite {
            url.query_pairs_mut().append_pair("overwrite", "true");
        }
        let mut request = self.platform.get(url).map_err(|_| {
            HostError::new(
                HostErrorCode::InvalidConfiguration,
                "the platform credential is unusable",
            )
        })?;
        *request.method_mut() = method;
        Ok(request)
    }
}

/// A platform object failure as the host classifies it.
fn failure(error: &Failure) -> HostError {
    let (code, message, reason) = match error {
        Failure::Status(status) => match status.as_u16() {
            404 => (
                HostErrorCode::NotFound,
                "no such object or bucket",
                "office_host.not_found",
            ),
            401 | 403 => (
                HostErrorCode::AuthorizationFailed,
                "the object API refused the turn's credential",
                "office_host.authorization_failed",
            ),
            400 | 409 | 415 | 422 => (
                HostErrorCode::InvalidInput,
                "the object API refused the request",
                "office_host.invalid_input",
            ),
            413 => (
                HostErrorCode::ResourceExhausted,
                "the object is over the project's size limit",
                "office_host.too_large",
            ),
            _ => (
                HostErrorCode::DependencyUnavailable,
                "the object API failed",
                "office_host.dependency_unavailable",
            ),
        },
        Failure::TooLarge(_) => (
            HostErrorCode::ResourceExhausted,
            "the object is larger than the Office limit",
            "office_host.too_large",
        ),
        Failure::Refused => (
            HostErrorCode::InvalidConfiguration,
            "the platform host is not allowed for object access",
            "office_host.egress_refused",
        ),
        _ => (
            HostErrorCode::DependencyUnavailable,
            "the object API could not be reached",
            "office_host.dependency_unavailable",
        ),
    };
    HostError::new(code, message).with_reason(reason)
}

#[async_trait]
impl OfficeToolHost for ClaimOfficeHost {
    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, HostError> {
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
    ) -> Result<OfficeObjectWritten, HostError> {
        // A name that could end the multipart header line is never sent.
        if !valid_object_key(name)
            || name.contains(['"', '\\', '\r', '\n'])
            || media_type.contains(['\r', '\n', '"'])
        {
            return Err(HostError::new(
                HostErrorCode::InvalidInput,
                "the file name is not valid",
            ));
        }
        let mut request = self.object_request(Method::POST, bucket, None, true)?;
        let boundary = format!("elitea-office-{}", uuid::Uuid::new_v4().simple());
        let mut body = Vec::with_capacity(bytes.len() + 256);
        body.extend_from_slice(
            format!(
                "--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{name}\"\r\nContent-Type: {media_type}\r\n\r\n"
            )
            .as_bytes(),
        );
        body.extend_from_slice(&bytes);
        body.extend_from_slice(format!("\r\n--{boundary}--\r\n").as_bytes());
        let content_type = format!("multipart/form-data; boundary={boundary}");
        request.headers_mut().insert(
            header::CONTENT_TYPE,
            content_type.parse().map_err(|_| {
                HostError::new(
                    HostErrorCode::InvalidInput,
                    "the upload header is not valid",
                )
            })?,
        );
        *request.body_mut() = Some(Body::from(body));
        let mut response = self
            .runtime
            .http
            .send(request)
            .await
            .map_err(|e| failure(&e))?;
        if response.status() != StatusCode::CREATED && !response.status().is_success() {
            return Err(failure(&Failure::Status(response.status())));
        }
        let reply = SourceHttp::body(&mut response, 64 * 1024)
            .await
            .map_err(|e| failure(&e))?;
        let reply: Value = serde_json::from_slice(&reply).map_err(|_| {
            HostError::new(
                HostErrorCode::InvalidResponse,
                "the object API reply is not JSON",
            )
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
                .unwrap_or(bytes.len() as u64),
        })
    }

    async fn complete(&self, request: OfficeModelRequest) -> Result<String, HostError> {
        let Some(binding) = self.model.get() else {
            return Err(HostError::new(
                HostErrorCode::InvalidConfiguration,
                "no model is bound for this run",
            )
            .with_reason("office_host.no_model"));
        };
        let mut parts = vec![Part::Text {
            text: request.prompt,
        }];
        for image in request.images_png {
            parts.push(Part::InlineData {
                mime_type: "image/png".to_owned(),
                data: image,
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
            // A budget or rate refusal arrives as the gateway's own text; the
            // reason is kept for the log, the sentence the model reads is fixed.
            HostError::new(
                HostErrorCode::DependencyUnavailable,
                "the model call failed",
            )
            .with_reason("office_host.model_call_failed")
        })
    }
}
