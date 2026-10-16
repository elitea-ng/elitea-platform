//! Image generation and editing over the Elitea `/llm` gateway, on the model
//! facade's channel (#864).
//!
//! The `imagegen` toolkit calls `POST /llm/v1/images/generations` (JSON) and
//! `POST /llm/v1/images/edits` (multipart) through the same origin-bound mTLS
//! HTTP/2 channel, the same claim-scoped PAT and the same caller contract
//! (`elitea-llm-wire`) as the agent's chat completions and the embeddings
//! route: the project selector, the execution id, the gateway's budget gate
//! and its request log. Nothing here adds a transport, a key or a route.
//!
//! Every request asks for `b64_json`; an answer item that carries only a
//! `url` is skipped, because fetching it would be a connection of the
//! worker's own to a host nobody approved. Every failure is a stable,
//! data-free code.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use bytes::Bytes;
use elitea_agent_runtime::toolkits::{ImageEditInput, MAX_IMAGES_PER_REQUEST};
use elitea_llm_wire::refusal::Refusal;
use elitea_llm_wire::route::{IMAGES_EDITS_ROUTE, IMAGES_GENERATIONS_ROUTE};
use serde::Deserialize;

use super::gateway_call::{CallError, Timing, build_request, send};
use super::multipart::{Multipart, PartError};
use super::openai_compatible_facade::{ModelGatewayClient, ModelGatewayTransport};
use super::runtime_context::ClaimScopedEliteaContext;

/// The most bytes of one request body (an image, a mask and a prompt).
const MAX_REQUEST_BYTES: usize = 64 * 1024 * 1024;
/// A response body cap: ten base64 images.
const MAX_RESPONSE_BYTES: usize = 128 * 1024 * 1024;
/// Image generation is slow; the chat timeouts do not fit. The SDK waits
/// `model_timeout`; this is the same order.
const MIN_IMAGE_TIMEOUT: Duration = Duration::from_mins(5);

/// A stable, data-free images failure.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum ImagesError {
    /// The gateway refused the call.
    Refused { refusal: Refusal },
    /// The channel could not carry the request or the response.
    Transport,
    /// The gateway answered 2xx but the body is not the images object. The
    /// call may have been billed.
    UnusableReply(&'static str),
    /// The answer is larger than its bound. The call may have been billed.
    ReplyTooLarge,
    /// The request is larger than its bound; nothing was sent.
    RequestTooLarge,
    /// A file name of the upload cannot be sent (see
    /// [`elitea_agent_runtime::toolkits::valid_upload_file_name`]): the
    /// request was never made.
    InvalidFileName,
    /// The request could not be built (a header value is unusable); nothing
    /// was sent.
    Malformed,
}

impl fmt::Display for ImagesError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Refused { refusal } => {
                write!(formatter, "the model gateway refused: {}", refusal.code())
            }
            Self::Transport => formatter.write_str("the model gateway transport failed"),
            Self::UnusableReply(reason) => {
                write!(formatter, "the images response is unusable: {reason}")
            }
            Self::ReplyTooLarge => formatter.write_str("the images response exceeds its bound"),
            Self::RequestTooLarge => formatter.write_str("the images request exceeds its bound"),
            Self::InvalidFileName => formatter.write_str("a file name of the upload is unusable"),
            Self::Malformed => formatter.write_str("the images request is malformed"),
        }
    }
}

impl std::error::Error for ImagesError {}

/// One generation or edit, as the caller's contract carries it.
pub(crate) struct ImageCall<'a> {
    pub(crate) model: &'a str,
    pub(crate) prompt: &'a str,
    pub(crate) size: Option<&'a str>,
    pub(crate) count: u32,
}

/// The gateway's images routes, bound to the shared channel.
#[derive(Clone)]
pub(crate) struct ImagesClient {
    transport: Arc<dyn ModelGatewayTransport>,
    response_timeout: Duration,
}

impl fmt::Debug for ImagesClient {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ImagesClient")
            .finish_non_exhaustive()
    }
}

#[derive(Deserialize)]
struct ImagesResponse {
    data: Vec<ImageItem>,
}

#[derive(Deserialize)]
struct ImageItem {
    #[serde(default)]
    b64_json: Option<String>,
}

impl ImagesClient {
    /// The images routes over `gateway`'s channel.
    #[must_use]
    pub(crate) fn from_gateway(gateway: &ModelGatewayClient) -> Self {
        Self {
            transport: gateway.transport.clone(),
            response_timeout: (gateway.config.response_header_timeout
                + gateway.config.stream_idle_timeout)
                .max(MIN_IMAGE_TIMEOUT),
        }
    }

    #[cfg(test)]
    pub(crate) fn with_transport(
        transport: Arc<dyn ModelGatewayTransport>,
        response_timeout: Duration,
    ) -> Self {
        Self {
            transport,
            response_timeout,
        }
    }

    /// Generate `call.count` images; one slot per answer item, in order.
    ///
    /// # Errors
    ///
    /// A gateway refusal, a transport failure, a malformed response, or a
    /// request over the size bound.
    pub(crate) async fn generate(
        &self,
        context: &ClaimScopedEliteaContext,
        call: &ImageCall<'_>,
    ) -> Result<Vec<Option<Vec<u8>>>, ImagesError> {
        let mut body = serde_json::json!({
            "prompt": call.prompt,
            "model": call.model,
            "n": call.count,
            "response_format": "b64_json",
        });
        if let Some(size) = call.size {
            body["size"] = serde_json::Value::String(size.to_owned());
        }
        let body = serde_json::to_vec(&body).map_err(|_| ImagesError::RequestTooLarge)?;
        self.post(
            context,
            IMAGES_GENERATIONS_ROUTE,
            "application/json",
            Bytes::from(body),
        )
        .await
    }

    /// Edit `image` (with an optional `mask`); one slot per answer item.
    ///
    /// # Errors
    ///
    /// As [`ImagesClient::generate`], and [`ImagesError::InvalidFileName`]
    /// before anything is sent.
    pub(crate) async fn edit(
        &self,
        context: &ClaimScopedEliteaContext,
        call: &ImageCall<'_>,
        image: ImageEditInput,
        mask: Option<ImageEditInput>,
    ) -> Result<Vec<Option<Vec<u8>>>, ImagesError> {
        let mut form = Multipart::new();
        form.text("prompt", call.prompt);
        form.text("model", call.model);
        form.text("response_format", "b64_json");
        let part_error = |error: PartError| match error {
            PartError::FileName | PartError::MediaType => ImagesError::InvalidFileName,
        };
        let media = media_type(&image.name);
        form.file("image", &image.name, media, &image.bytes)
            .map_err(part_error)?;
        if let Some(mask) = mask {
            let media = media_type(&mask.name);
            form.file("mask", &mask.name, media, &mask.bytes)
                .map_err(part_error)?;
        }
        let (content_type, body) = form.finish();
        self.post(context, IMAGES_EDITS_ROUTE, &content_type, body)
            .await
    }

    async fn post(
        &self,
        context: &ClaimScopedEliteaContext,
        route: &'static str,
        content_type: &str,
        body: Bytes,
    ) -> Result<Vec<Option<Vec<u8>>>, ImagesError> {
        if body.len() > MAX_REQUEST_BYTES {
            return Err(ImagesError::RequestTooLarge);
        }
        let token = context.model_facade_token();
        let request = build_request(
            route,
            content_type,
            body,
            context.resource_project_id(),
            &token,
            context.execution_id(),
        )
        .map_err(images_error)?;
        let collected = send(
            &self.transport,
            request,
            Timing::Total(self.response_timeout),
            MAX_RESPONSE_BYTES,
        )
        .await
        .map_err(images_error)?;
        parse_response(&collected)
    }
}

fn images_error(error: CallError) -> ImagesError {
    match error {
        CallError::Refused(refusal) => ImagesError::Refused { refusal },
        CallError::Transport => ImagesError::Transport,
        CallError::TooLarge => ImagesError::ReplyTooLarge,
        CallError::Malformed => ImagesError::Malformed,
    }
}

/// The media type a file name implies; the gateway reads the bytes, not this.
fn media_type(name: &str) -> &'static str {
    match name
        .rsplit('.')
        .next()
        .map(str::to_ascii_lowercase)
        .as_deref()
    {
        Some("png") => "image/png",
        Some("jpg" | "jpeg") => "image/jpeg",
        Some("webp") => "image/webp",
        _ => "application/octet-stream",
    }
}

/// The images object: one slot per answer item, in order. An item with no
/// inline data (a bare `url`) is `None`, so the position stays the SDK's.
fn parse_response(raw: &[u8]) -> Result<Vec<Option<Vec<u8>>>, ImagesError> {
    let parsed: ImagesResponse = serde_json::from_slice(raw)
        .map_err(|_| ImagesError::UnusableReply("not the images object"))?;
    if parsed.data.len() > MAX_IMAGES_PER_REQUEST {
        return Err(ImagesError::UnusableReply("too many images"));
    }
    parsed
        .data
        .into_iter()
        .map(|item| {
            item.b64_json
                .filter(|value| !value.is_empty())
                .map(|encoded| {
                    STANDARD
                        .decode(encoded.as_bytes())
                        .map_err(|_| ImagesError::UnusableReply("an image is not valid base64"))
                })
                .transpose()
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn positions_are_kept_and_url_only_items_are_empty_slots() {
        let png = STANDARD.encode([1_u8, 2, 3]);
        let raw = format!(
            r#"{{"created":1,"data":[{{"url":"https://x/y.png"}},{{"b64_json":"{png}"}},{{"b64_json":""}}]}}"#
        );
        let images = parse_response(raw.as_bytes()).expect("parses");
        assert_eq!(images, vec![None, Some(vec![1_u8, 2, 3]), None]);
        assert!(parse_response(b"not json").is_err());
        assert!(parse_response(br#"{"data":[{"b64_json":"%%%"}]}"#).is_err());
        assert_eq!(
            parse_response(br#"{"data":[]}"#).expect("empty is empty"),
            Vec::<Option<Vec<u8>>>::new()
        );
    }

    #[test]
    fn media_types_follow_the_extension() {
        assert_eq!(media_type("A.PNG"), "image/png");
        assert_eq!(media_type("a.jpeg"), "image/jpeg");
        assert_eq!(media_type("noext"), "application/octet-stream");
    }
}
