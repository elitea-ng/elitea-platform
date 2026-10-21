//! The client of `elitea.llm.v1.ModelService` (#1359 U2): elitea-main's
//! model plane on its platform gRPC listener, which replaced the HTTP
//! `/llm/v1` calls the agent worker and the engines made.
//!
//! The service keeps the provider-native wire format as bytes, so this crate
//! hands its callers the answer in the shape they already parse: a
//! [`ModelResponse`] with the HTTP status, the headers that matter
//! (`content-type`, `retry-after`) and the body bytes as a stream, SSE
//! framing included. A refusal (budget 402, rate limit 429, a bad bearer
//! 401, ...) arrives as a gRPC status carrying an `llm.v1.ModelRefusal` in
//! the [`REFUSAL_TRAILER`]; it is turned back into the response the HTTP edge
//! answered, so each caller's refusal classification
//! (`elitea_llm_wire::refusal`) is unchanged. A status without that trailer
//! is the hop failing: [`CallError::Hop`].
//!
//! The crate opens no connection. The caller builds the mutual-TLS
//! [`tonic::transport::Channel`] to the platform listener with its own
//! crypto provider and client certificate, and passes it in.
//!
//! Cancellation is dropping the call or its body stream: the RPC is reset
//! and elitea-main cancels the request it hands the gateway.

use std::pin::Pin;

use bytes::Bytes;
use futures_util::{Stream, StreamExt as _};
use http::header::{CONTENT_TYPE, RETRY_AFTER};
use http::{HeaderMap, HeaderValue, StatusCode};
use prost::Message as _;
use tonic::metadata::{AsciiMetadataValue, MetadataValue};
use tonic::transport::Channel;

/// The generated `elitea.llm.v1` bindings.
#[allow(clippy::all, clippy::pedantic, missing_docs)]
pub mod pb {
    tonic::include_proto!("elitea.llm.v1");
}

pub use pb::model_service_client::ModelServiceClient;

#[cfg(feature = "testing")]
pub mod testing;

/// The trailer that carries a refusal (`elitea.llm.v1.ModelRefusal`).
pub const REFUSAL_TRAILER: &str = "elitea-model-refusal-bin";

/// The size of one body chunk of a streamed request (image edits).
const REQUEST_CHUNK_BYTES: usize = 64 * 1024;

/// The RPC a `/llm/v1` route maps to.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Route {
    ChatCompletions,
    Messages,
    Embeddings,
    ImageGenerate,
    ImageEdit,
}

impl Route {
    /// The route's path under `/llm/v1` (`"/chat/completions"`, ...).
    #[must_use]
    pub fn path(self) -> &'static str {
        match self {
            Self::ChatCompletions => "/chat/completions",
            Self::Messages => "/messages",
            Self::Embeddings => "/embeddings",
            Self::ImageGenerate => "/images/generations",
            Self::ImageEdit => "/images/edits",
        }
    }

    /// The route a request path names: the whole route
    /// (`/llm/v1/chat/completions`) or any base followed by the route's path
    /// (`{api_base}/chat/completions`).
    #[must_use]
    pub fn from_path(path: &str) -> Option<Self> {
        let path = path.split(['?', '#']).next().unwrap_or_default();
        [
            Self::ChatCompletions,
            Self::Messages,
            Self::Embeddings,
            Self::ImageGenerate,
            Self::ImageEdit,
        ]
        .into_iter()
        .find(|route| path.ends_with(route.path()))
    }
}

/// One model call.
#[derive(Clone, Default)]
pub struct ModelRequest {
    /// The bearer (`authorization: Bearer <token>`), never logged.
    pub bearer: String,
    /// The billing project selector (`X-Project-Id` over HTTP).
    pub project_id: Option<String>,
    /// The run the spend is attributed to (`X-Elitea-Execution-Id`).
    pub execution_id: Option<String>,
    /// The body's Content-Type: required for an image edit (the multipart
    /// boundary), ignored otherwise.
    pub content_type: Option<String>,
    /// `anthropic-version` and `anthropic-beta`, for [`Route::Messages`].
    pub anthropic_version: Option<String>,
    pub anthropic_beta: Vec<String>,
    /// The provider-native body.
    pub body: Bytes,
}

impl std::fmt::Debug for ModelRequest {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ModelRequest")
            .field("bearer", &"<redacted>")
            .field("project_id", &self.project_id)
            .field("execution_id", &self.execution_id)
            .field("content_type", &self.content_type)
            .field("body_bytes", &self.body.len())
            .finish_non_exhaustive()
    }
}

impl ModelRequest {
    /// Read a request as the HTTP callers built it: the bearer from
    /// `authorization`, the selector and execution id from the `/llm`
    /// caller-contract headers, the content type and the Anthropic headers.
    ///
    /// # Errors
    ///
    /// [`CallError::Malformed`] when a header the call needs is not text.
    pub fn from_http_parts(headers: &HeaderMap, body: Bytes) -> Result<Self, CallError> {
        let text = |name: &str| -> Result<Option<String>, CallError> {
            headers
                .get(name)
                .map(|value| {
                    value
                        .to_str()
                        .map(str::to_owned)
                        .map_err(|_| CallError::Malformed("a header is not text"))
                })
                .transpose()
        };
        let bearer = text("authorization")?
            .and_then(|value| {
                value
                    .strip_prefix("Bearer ")
                    .or_else(|| value.strip_prefix("bearer "))
                    .map(str::to_owned)
            })
            .unwrap_or_default();
        let anthropic_beta = headers
            .get_all("anthropic-beta")
            .iter()
            .map(|value| {
                value
                    .to_str()
                    .map(str::to_owned)
                    .map_err(|_| CallError::Malformed("anthropic-beta is not text"))
            })
            .collect::<Result<_, _>>()?;
        Ok(Self {
            bearer,
            project_id: text("x-project-id")?,
            execution_id: text("x-elitea-execution-id")?,
            content_type: text("content-type")?,
            anthropic_version: text("anthropic-version")?,
            anthropic_beta,
            body,
        })
    }

    fn model_call(&self) -> pb::ModelCall {
        pb::ModelCall {
            project_id: self.project_id.clone().unwrap_or_default(),
            execution_id: self.execution_id.clone().unwrap_or_default(),
        }
    }

    fn bearer_metadata(&self) -> Result<Option<AsciiMetadataValue>, CallError> {
        if self.bearer.is_empty() {
            return Ok(None);
        }
        let mut value =
            MetadataValue::try_from(format!("Bearer {}", self.bearer)).map_err(|_| {
                CallError::Malformed("the bearer holds characters metadata cannot carry")
            })?;
        value.set_sensitive(true);
        Ok(Some(value))
    }

    fn tonic_request<T>(&self, message: T) -> Result<tonic::Request<T>, CallError> {
        let mut request = tonic::Request::new(message);
        if let Some(bearer) = self.bearer_metadata()? {
            request.metadata_mut().insert("authorization", bearer);
        }
        Ok(request)
    }
}

/// A body stream: the answer's bytes in order. An `Err` after bytes is a
/// broken stream, never an end.
pub type BodyStream = Pin<Box<dyn Stream<Item = Result<Bytes, tonic::Status>> + Send>>;

/// The answer, in the HTTP edge's terms.
pub struct ModelResponse {
    pub status: StatusCode,
    /// `content-type`, and `retry-after` on a refusal that had one.
    pub headers: HeaderMap,
    pub body: BodyStream,
}

impl std::fmt::Debug for ModelResponse {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ModelResponse")
            .field("status", &self.status)
            .field("headers", &self.headers)
            .finish_non_exhaustive()
    }
}

/// Why a call produced no [`ModelResponse`].
#[derive(Debug)]
pub enum CallError {
    /// The request cannot be expressed (a header that is not text).
    Malformed(&'static str),
    /// The hop failed: the peer was not admitted, the platform listener is
    /// unreachable, or the call was malformed in transit. The status never
    /// carries a refusal (that is a [`ModelResponse`]).
    Hop(tonic::Status),
}

impl std::fmt::Display for CallError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Malformed(reason) => write!(f, "the model request is malformed: {reason}"),
            Self::Hop(status) => write!(
                f,
                "the model service call failed: {:?}: {}",
                status.code(),
                status.message()
            ),
        }
    }
}

impl std::error::Error for CallError {}

/// The gRPC code a refusal's HTTP status maps to (elitea-main's
/// `modelgrpc.CodeForHTTPStatus`; `ModelRefusal` documents it).
#[must_use]
pub fn code_for_http_status(status: u16) -> tonic::Code {
    match status {
        400 | 413 | 422 => tonic::Code::InvalidArgument,
        401 => tonic::Code::Unauthenticated,
        402 | 429 => tonic::Code::ResourceExhausted,
        403 => tonic::Code::PermissionDenied,
        404 => tonic::Code::NotFound,
        408 | 504 => tonic::Code::DeadlineExceeded,
        409 => tonic::Code::Aborted,
        501 => tonic::Code::Unimplemented,
        502 | 503 => tonic::Code::Unavailable,
        500..=599 => tonic::Code::Internal,
        _ => tonic::Code::FailedPrecondition,
    }
}

/// The refusal a status carries, as the response the HTTP edge answered.
#[must_use]
pub fn refusal_response(status: &tonic::Status) -> Option<ModelResponse> {
    let raw = status
        .metadata()
        .get_bin(REFUSAL_TRAILER)?
        .to_bytes()
        .ok()?;
    let refusal = pb::ModelRefusal::decode(raw).ok()?;
    let code = u16::try_from(refusal.http_status)
        .ok()
        .and_then(|code| StatusCode::from_u16(code).ok())
        .filter(|code| !code.is_success())?;
    let mut headers = HeaderMap::new();
    if let Ok(value) = HeaderValue::from_str(&refusal.content_type)
        && !refusal.content_type.is_empty()
    {
        headers.insert(CONTENT_TYPE, value);
    }
    if let Ok(value) = HeaderValue::from_str(&refusal.retry_after)
        && !refusal.retry_after.is_empty()
    {
        headers.insert(RETRY_AFTER, value);
    }
    Some(ModelResponse {
        status: code,
        headers,
        body: once(refusal.body),
    })
}

fn once(body: Bytes) -> BodyStream {
    Box::pin(futures_util::stream::iter(
        (!body.is_empty()).then_some(Ok(body)),
    ))
}

fn refused_or_hop(status: tonic::Status) -> Result<ModelResponse, CallError> {
    refusal_response(&status).ok_or(CallError::Hop(status))
}

fn success(head: Option<&pb::ResponseHead>, body: BodyStream) -> ModelResponse {
    let mut headers = HeaderMap::new();
    if let Some(value) = head
        .map(|head| head.content_type.as_str())
        .filter(|value| !value.is_empty())
        .and_then(|value| HeaderValue::from_str(value).ok())
    {
        headers.insert(CONTENT_TYPE, value);
    }
    ModelResponse {
        status: StatusCode::OK,
        headers,
        body,
    }
}

/// One frame of a streamed answer.
enum Frame {
    Head(pb::ResponseHead),
    Data(Bytes),
    Empty,
}

trait Framed {
    fn frame(self) -> Frame;
}

impl Framed for pb::ChatCompletionsResponse {
    fn frame(self) -> Frame {
        use pb::chat_completions_response::Frame as F;
        match self.frame {
            Some(F::Head(head)) => Frame::Head(head),
            Some(F::Data(data)) => Frame::Data(data),
            None => Frame::Empty,
        }
    }
}

impl Framed for pb::MessagesResponse {
    fn frame(self) -> Frame {
        use pb::messages_response::Frame as F;
        match self.frame {
            Some(F::Head(head)) => Frame::Head(head),
            Some(F::Data(data)) => Frame::Data(data),
            None => Frame::Empty,
        }
    }
}

/// Wait for the head, then hand the rest on as the body.
async fn streamed<M: Framed + Send + 'static>(
    answer: Result<tonic::Response<tonic::Streaming<M>>, tonic::Status>,
) -> Result<ModelResponse, CallError> {
    let mut frames = match answer {
        Ok(response) => response.into_inner(),
        Err(status) => return refused_or_hop(status),
    };
    let head = match frames.message().await {
        Ok(Some(message)) => match message.frame() {
            Frame::Head(head) => head,
            _ => {
                return Err(CallError::Hop(tonic::Status::internal(
                    "the model stream did not start with its head",
                )));
            }
        },
        Ok(None) => {
            return Err(CallError::Hop(tonic::Status::internal(
                "the model stream ended before its head",
            )));
        }
        Err(status) => return refused_or_hop(status),
    };
    let body = frames.filter_map(|frame| async move {
        match frame {
            Ok(message) => match message.frame() {
                Frame::Data(data) if !data.is_empty() => Some(Ok(data)),
                Frame::Data(_) | Frame::Empty => None,
                Frame::Head(_) => Some(Err(tonic::Status::internal(
                    "the model stream sent a second head",
                ))),
            },
            // A refusal after the head cannot happen; a status here is the
            // stream breaking.
            Err(status) => Some(Err(status)),
        }
    });
    Ok(success(Some(&head), Box::pin(body)))
}

/// Make one call on `client`.
///
/// # Errors
///
/// [`CallError::Malformed`] for a request that cannot be expressed, and
/// [`CallError::Hop`] when the hop fails. A refusal is an `Ok` response
/// with its HTTP status.
pub async fn call(
    client: &mut ModelServiceClient<Channel>,
    route: Route,
    request: ModelRequest,
) -> Result<ModelResponse, CallError> {
    let call = request.model_call();
    match route {
        Route::ChatCompletions => {
            let message = request.tonic_request(pb::ChatCompletionsRequest {
                call: Some(call),
                body: request.body.clone(),
            })?;
            streamed(client.chat_completions(message).await).await
        }
        Route::Messages => {
            let message = request.tonic_request(pb::MessagesRequest {
                call: Some(call),
                body: request.body.clone(),
                anthropic_version: request.anthropic_version.clone().unwrap_or_default(),
                anthropic_beta: request.anthropic_beta.clone(),
            })?;
            streamed(client.messages(message).await).await
        }
        Route::Embeddings => {
            let message = request.tonic_request(pb::EmbeddingsRequest {
                call: Some(call),
                body: request.body.clone(),
            })?;
            match client.embeddings(message).await {
                Ok(answer) => {
                    let answer = answer.into_inner();
                    Ok(success(answer.head.as_ref(), once(answer.body)))
                }
                Err(status) => refused_or_hop(status),
            }
        }
        Route::ImageGenerate => {
            let message = request.tonic_request(pb::ImageGenerateRequest {
                call: Some(call),
                body: request.body.clone(),
            })?;
            match client.image_generate(message).await {
                Ok(answer) => {
                    let answer = answer.into_inner();
                    Ok(success(answer.head.as_ref(), once(answer.body)))
                }
                Err(status) => refused_or_hop(status),
            }
        }
        Route::ImageEdit => {
            let content_type = request.content_type.clone().ok_or(CallError::Malformed(
                "an image edit needs its multipart content type",
            ))?;
            let head = pb::ImageEditRequest {
                frame: Some(pb::image_edit_request::Frame::Head(pb::ImageEditHead {
                    call: Some(call),
                    content_type,
                })),
            };
            let body = request.body.clone();
            let chunks = (0..body.len())
                .step_by(REQUEST_CHUNK_BYTES)
                .map(move |start| {
                    let end = (start + REQUEST_CHUNK_BYTES).min(body.len());
                    pb::ImageEditRequest {
                        frame: Some(pb::image_edit_request::Frame::Data(body.slice(start..end))),
                    }
                })
                .collect::<Vec<_>>();
            let frames = futures_util::stream::iter(std::iter::once(head).chain(chunks));
            let message = request.tonic_request(frames)?;
            match client.image_edit(message).await {
                Ok(answer) => {
                    let answer = answer.into_inner();
                    Ok(success(answer.head.as_ref(), once(answer.body)))
                }
                Err(status) => refused_or_hop(status),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn routes_are_read_from_whole_routes_and_api_bases() {
        assert_eq!(
            Route::from_path("/llm/v1/chat/completions"),
            Some(Route::ChatCompletions)
        );
        assert_eq!(
            Route::from_path("https://main:9446/llm/v1/embeddings"),
            Some(Route::Embeddings)
        );
        assert_eq!(Route::from_path("/v1/messages"), Some(Route::Messages));
        assert_eq!(
            Route::from_path("/llm/v1/images/edits"),
            Some(Route::ImageEdit)
        );
        assert_eq!(Route::from_path("/llm/v1/models"), None);
    }

    #[test]
    fn a_refusal_trailer_becomes_the_http_answer() {
        let refusal = pb::ModelRefusal {
            http_status: 402,
            content_type: "application/json".to_owned(),
            body: Bytes::from_static(br#"{"error":{"type":"budget_exceeded"}}"#),
            retry_after: "3".to_owned(),
        };
        let mut metadata = tonic::metadata::MetadataMap::new();
        metadata.insert_bin(
            REFUSAL_TRAILER,
            MetadataValue::from_bytes(&refusal.encode_to_vec()),
        );
        let status =
            tonic::Status::with_metadata(tonic::Code::ResourceExhausted, "refused", metadata);
        let Some(response) = refusal_response(&status) else {
            panic!("no refusal read");
        };
        assert_eq!(response.status, StatusCode::PAYMENT_REQUIRED);
        assert_eq!(response.headers[CONTENT_TYPE], "application/json");
        assert_eq!(response.headers[RETRY_AFTER], "3");
        assert!(refusal_response(&tonic::Status::unavailable("down")).is_none());
    }

    #[test]
    fn the_bearer_is_read_and_never_shown() {
        let mut headers = HeaderMap::new();
        headers.insert("authorization", HeaderValue::from_static("Bearer tok-123"));
        headers.insert("x-project-id", HeaderValue::from_static("7"));
        headers.insert("x-elitea-execution-id", HeaderValue::from_static("exec"));
        headers.append("anthropic-beta", HeaderValue::from_static("a"));
        headers.append("anthropic-beta", HeaderValue::from_static("b"));
        let Ok(request) = ModelRequest::from_http_parts(&headers, Bytes::new()) else {
            panic!("refused");
        };
        assert_eq!(request.bearer, "tok-123");
        assert_eq!(request.project_id.as_deref(), Some("7"));
        assert_eq!(request.execution_id.as_deref(), Some("exec"));
        assert_eq!(request.anthropic_beta, ["a", "b"]);
        assert!(!format!("{request:?}").contains("tok-123"));
    }

    #[test]
    fn codes_follow_the_http_status() {
        assert_eq!(code_for_http_status(402), tonic::Code::ResourceExhausted);
        assert_eq!(code_for_http_status(401), tonic::Code::Unauthenticated);
        assert_eq!(code_for_http_status(503), tonic::Code::Unavailable);
        assert_eq!(code_for_http_status(500), tonic::Code::Internal);
        assert_eq!(code_for_http_status(418), tonic::Code::FailedPrecondition);
    }
}
