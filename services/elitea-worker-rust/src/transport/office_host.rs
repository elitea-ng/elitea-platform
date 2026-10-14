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
    OfficeFailure, OfficeHostError, OfficeModelRequest, OfficeObjectWritten, OfficeToolHost,
};
use elitea_connectors::artifact::client::{ArtifactPlatform, valid_object_key, valid_segment};
use elitea_connectors::egress::HostAllowlist;
use elitea_connectors::source::{Failure, SourceHttp, SourceLimits};
use elitea_connectors::transport::{Body, Method, Url, header};
use serde_json::Value;

use crate::indexing::tools::{LazyStepbackModel, generate_text};

/// What every Office host of the worker shares: the platform origin and the
/// guarded HTTP client to it.
pub(crate) struct OfficeRuntime {
    origin: String,
    /// The deployment's public base URL, when it has stated one.
    public_base_url: Option<String>,
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
            public_base_url: None,
            http: SourceHttp::new(HostAllowlist::parse(Some(&host)), Arc::new(transport))
                .with_limits(limits),
        })
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
        }
    }
}

/// The Office host of one claimed turn.
pub(crate) struct ClaimOfficeHost {
    runtime: Arc<OfficeRuntime>,
    platform: ArtifactPlatform,
    model: LazyStepbackModel,
    project_id: String,
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
        })
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

#[async_trait]
impl OfficeToolHost for ClaimOfficeHost {
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
        // A name that could end the multipart header line is never sent.
        if !valid_object_key(name)
            || name.contains(['"', '\\', '\r', '\n'])
            || media_type.contains(['\r', '\n', '"'])
        {
            return Err(OfficeHostError::new(
                OfficeFailure::InvalidRequest,
                "office_host.invalid_name",
            ));
        }
        let mut request = self.object_request(Method::POST, bucket, None, overwrite)?;
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
                .unwrap_or(bytes.len() as u64),
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
