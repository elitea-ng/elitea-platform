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
use elitea_llm_wire::headers::{BEARER_PREFIX, EXECUTION_HEADER, PROJECT_HEADER};
use elitea_llm_wire::refusal::Refusal;
use elitea_llm_wire::route::{IMAGES_EDITS_ROUTE, IMAGES_GENERATIONS_ROUTE};
use http::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderName, HeaderValue, Method, Request, Version};
use http_body_util::{BodyExt as _, Full};
use serde::Deserialize;
use tokio::time::timeout;
use tonic::body::Body;
use zeroize::Zeroizing;

use super::openai_compatible_facade::{ModelGatewayClient, ModelGatewayTransport};
use super::runtime_context::ClaimScopedEliteaContext;

type BoxError = Box<dyn std::error::Error + Send + Sync + 'static>;

const PROJECT_SELECTOR: HeaderName = HeaderName::from_static(PROJECT_HEADER);
const EXECUTION_ID_HEADER: HeaderName = HeaderName::from_static(EXECUTION_HEADER);
/// The most bytes of one request body (an image, a mask and a prompt).
const MAX_REQUEST_BYTES: usize = 64 * 1024 * 1024;
/// A response body cap: ten base64 images.
const MAX_RESPONSE_BYTES: usize = 128 * 1024 * 1024;
/// The most images one answer may carry.
const MAX_IMAGES: usize = 10;
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
    /// The response is not the images object.
    InvalidResponse(&'static str),
    /// A request or response would exceed its bound.
    TooLarge,
}

impl fmt::Display for ImagesError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Refused { refusal } => {
                write!(formatter, "the model gateway refused: {}", refusal.code())
            }
            Self::Transport => formatter.write_str("the model gateway transport failed"),
            Self::InvalidResponse(reason) => {
                write!(formatter, "the images response is malformed: {reason}")
            }
            Self::TooLarge => formatter.write_str("the images request exceeds its bound"),
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

/// One file of an edit.
pub(crate) struct EditFile<'a> {
    pub(crate) name: &'a str,
    pub(crate) bytes: &'a [u8],
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

    /// Generate `call.count` images; the decoded bytes of every inline image.
    ///
    /// # Errors
    ///
    /// A gateway refusal, a transport failure, a malformed response, or a
    /// request over the size bound.
    pub(crate) async fn generate(
        &self,
        context: &ClaimScopedEliteaContext,
        call: &ImageCall<'_>,
    ) -> Result<Vec<Vec<u8>>, ImagesError> {
        let mut body = serde_json::json!({
            "prompt": call.prompt,
            "model": call.model,
            "n": call.count,
            "response_format": "b64_json",
        });
        if let Some(size) = call.size {
            body["size"] = serde_json::Value::String(size.to_owned());
        }
        let body = serde_json::to_vec(&body).map_err(|_| ImagesError::TooLarge)?;
        self.post(
            context,
            IMAGES_GENERATIONS_ROUTE,
            "application/json",
            Bytes::from(body),
        )
        .await
    }

    /// Edit `image` (with an optional `mask`); the decoded bytes of every
    /// inline image.
    ///
    /// # Errors
    ///
    /// As [`ImagesClient::generate`].
    pub(crate) async fn edit(
        &self,
        context: &ClaimScopedEliteaContext,
        call: &ImageCall<'_>,
        image: &EditFile<'_>,
        mask: Option<&EditFile<'_>>,
    ) -> Result<Vec<Vec<u8>>, ImagesError> {
        let boundary = format!("elitea-images-{}", uuid::Uuid::new_v4().simple());
        let mut body = Vec::with_capacity(image.bytes.len() + 1024);
        text_part(&mut body, &boundary, "prompt", call.prompt);
        text_part(&mut body, &boundary, "model", call.model);
        text_part(&mut body, &boundary, "response_format", "b64_json");
        file_part(&mut body, &boundary, "image", image)?;
        if let Some(mask) = mask {
            file_part(&mut body, &boundary, "mask", mask)?;
        }
        body.extend_from_slice(format!("--{boundary}--\r\n").as_bytes());
        if body.len() > MAX_REQUEST_BYTES {
            return Err(ImagesError::TooLarge);
        }
        self.post(
            context,
            IMAGES_EDITS_ROUTE,
            &format!("multipart/form-data; boundary={boundary}"),
            Bytes::from(body),
        )
        .await
    }

    async fn post(
        &self,
        context: &ClaimScopedEliteaContext,
        route: &'static str,
        content_type: &str,
        body: Bytes,
    ) -> Result<Vec<Vec<u8>>, ImagesError> {
        if body.len() > MAX_REQUEST_BYTES {
            return Err(ImagesError::TooLarge);
        }
        let token = context.model_facade_token();
        let request = build_request(
            route,
            content_type,
            body,
            context.resource_project_id(),
            &token,
            context.execution_id(),
        )?;
        let response = timeout(self.response_timeout, self.transport.post(request))
            .await
            .map_err(|_| ImagesError::Transport)?
            .map_err(|_| ImagesError::Transport)?;
        let status = response.status().as_u16();
        let limited = http_body_util::Limited::new(response.into_body(), MAX_RESPONSE_BYTES);
        let collected = match timeout(self.response_timeout, limited.collect()).await {
            Ok(Ok(collected)) => collected.to_bytes(),
            Ok(Err(error)) => return Err(classify_body_error(&error)),
            Err(_) => return Err(ImagesError::Transport),
        };
        // The body is read before the refusal is classified: a 402 names its
        // budget scope in it, and the scope is never shown to the model.
        if let Some(refusal) = Refusal::classify(status, &collected) {
            return Err(ImagesError::Refused { refusal });
        }
        parse_response(&collected)
    }
}

fn classify_body_error(error: &BoxError) -> ImagesError {
    if error.is::<http_body_util::LengthLimitError>() {
        ImagesError::TooLarge
    } else {
        ImagesError::Transport
    }
}

fn text_part(body: &mut Vec<u8>, boundary: &str, name: &str, value: &str) {
    body.extend_from_slice(
        format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n"
        )
        .as_bytes(),
    );
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

fn file_part(
    body: &mut Vec<u8>,
    boundary: &str,
    field: &str,
    file: &EditFile<'_>,
) -> Result<(), ImagesError> {
    // A name that could end the header line is never sent.
    if file.name.is_empty() || file.name.contains(['"', '\\', '\r', '\n']) {
        return Err(ImagesError::InvalidResponse("the file name is unusable"));
    }
    body.extend_from_slice(
        format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"{field}\"; filename=\"{}\"\r\nContent-Type: {}\r\n\r\n",
            file.name,
            media_type(file.name)
        )
        .as_bytes(),
    );
    body.extend_from_slice(file.bytes);
    body.extend_from_slice(b"\r\n");
    Ok(())
}

fn build_request(
    route: &'static str,
    content_type: &str,
    body: Bytes,
    billing_project_id: u64,
    token: &Zeroizing<String>,
    execution_id: &str,
) -> Result<Request<Body>, ImagesError> {
    let malformed = || ImagesError::InvalidResponse("the request is malformed");
    let body_length = body.len();
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(route)
        .version(Version::HTTP_2)
        .body(Body::new(Full::new(body)))
        .map_err(|_| malformed())?;
    let headers = request.headers_mut();
    headers.insert(
        CONTENT_TYPE,
        HeaderValue::from_str(content_type).map_err(|_| malformed())?,
    );
    headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
    headers.insert(
        CONTENT_LENGTH,
        HeaderValue::from_str(&body_length.to_string()).map_err(|_| malformed())?,
    );
    headers.insert(
        PROJECT_SELECTOR,
        HeaderValue::from_str(&billing_project_id.to_string()).map_err(|_| malformed())?,
    );
    headers.insert(
        EXECUTION_ID_HEADER,
        HeaderValue::from_str(execution_id).map_err(|_| malformed())?,
    );
    let mut bearer = Zeroizing::new(String::with_capacity(BEARER_PREFIX.len() + token.len()));
    bearer.push_str(BEARER_PREFIX);
    bearer.push_str(token);
    let mut authorization = HeaderValue::from_str(bearer.as_str()).map_err(|_| malformed())?;
    authorization.set_sensitive(true);
    headers.insert(AUTHORIZATION, authorization);
    Ok(request)
}

/// The images object: the inline images, decoded. An item with no inline data
/// (a bare `url`) is skipped.
fn parse_response(raw: &[u8]) -> Result<Vec<Vec<u8>>, ImagesError> {
    let parsed: ImagesResponse = serde_json::from_slice(raw)
        .map_err(|_| ImagesError::InvalidResponse("not the images object"))?;
    if parsed.data.len() > MAX_IMAGES {
        return Err(ImagesError::InvalidResponse("too many images"));
    }
    let mut images = Vec::new();
    for item in parsed.data {
        let Some(encoded) = item.b64_json.filter(|value| !value.is_empty()) else {
            continue;
        };
        let bytes = STANDARD
            .decode(encoded.as_bytes())
            .map_err(|_| ImagesError::InvalidResponse("an image is not valid base64"))?;
        images.push(bytes);
    }
    Ok(images)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn inline_images_are_decoded_and_url_only_items_are_skipped() {
        let png = STANDARD.encode([1_u8, 2, 3]);
        let raw = format!(
            r#"{{"created":1,"data":[{{"b64_json":"{png}"}},{{"url":"https://x/y.png"}},{{"b64_json":""}}]}}"#
        );
        let images = parse_response(raw.as_bytes()).expect("parses");
        assert_eq!(images, vec![vec![1_u8, 2, 3]]);
        assert!(parse_response(b"not json").is_err());
        assert!(parse_response(br#"{"data":[{"b64_json":"%%%"}]}"#).is_err());
        assert_eq!(
            parse_response(br#"{"data":[]}"#).expect("empty is empty"),
            Vec::<Vec<u8>>::new()
        );
    }

    #[test]
    fn the_request_carries_the_caller_contract_headers() {
        let token = Zeroizing::new("pat".to_owned());
        let request = build_request(
            IMAGES_GENERATIONS_ROUTE,
            "application/json",
            Bytes::from_static(b"{}"),
            7,
            &token,
            "exec-1",
        )
        .expect("ok");
        assert_eq!(request.uri().path(), "/llm/v1/images/generations");
        assert_eq!(request.version(), Version::HTTP_2);
        let headers = request.headers();
        assert_eq!(headers.get(PROJECT_SELECTOR).unwrap(), "7");
        assert_eq!(headers.get(EXECUTION_ID_HEADER).unwrap(), "exec-1");
        assert_eq!(headers.get(AUTHORIZATION).unwrap(), "Bearer pat");
        assert!(headers.get(AUTHORIZATION).unwrap().is_sensitive());
    }

    #[test]
    fn a_file_name_that_could_end_the_header_line_is_refused() {
        let mut body = Vec::new();
        let file = EditFile {
            name: "a\".png\r\nX: y",
            bytes: b"x",
        };
        assert!(file_part(&mut body, "b", "image", &file).is_err());
        assert!(body.is_empty());
        assert_eq!(media_type("A.PNG"), "image/png");
        assert_eq!(media_type("noext"), "application/octet-stream");
    }
}
