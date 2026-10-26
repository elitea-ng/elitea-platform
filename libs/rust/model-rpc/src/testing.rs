//! A test `ModelService` over an axum Router (`testing` feature).
//!
//! The callers' tests describe the gateway as HTTP handlers for
//! `{base}/chat/completions`, `{base}/embeddings`, ... — what the edge
//! answers is still an HTTP status, headers and bytes. [`with_model_service`]
//! serves those handlers as `elitea.llm.v1.ModelService` on the same Router,
//! translating exactly as elitea-main does (`internal/transport/modelgrpc`):
//! a 2xx is a head and the body bytes, anything else a gRPC status carrying
//! the refusal trailer. A test then points the client at the Router's address
//! and exercises the real gRPC path end to end.
//!
//! Serve the result with `axum::serve` (HTTP/2 prior knowledge is accepted,
//! the crate enables axum's `http2`).

use std::pin::Pin;

use axum::Router;
use axum::body::Body;
use bytes::Bytes;
use futures_util::{Stream, StreamExt as _};
use http::{HeaderValue, Request, StatusCode};
use prost::Message as _;
use tonic::metadata::{MetadataMap, MetadataValue};
use tonic::{Status, Streaming};

use crate::pb::model_service_server::{ModelService, ModelServiceServer};
use crate::{REFUSAL_TRAILER, Route, code_for_http_status, pb};

/// The answers the test `ModelService` needs from the Router.
#[derive(Clone)]
struct Bridge {
    router: Router,
    base: String,
}

/// Mount `elitea.llm.v1.ModelService` on `router`, served by the router's own
/// `{base}{route}` handlers (`base` is what a test's `api_base` path was,
/// `/llm/v1` or `/v1`).
pub fn with_model_service(router: Router, base: &str) -> Router {
    let bridge = Bridge {
        router: router.clone(),
        base: base.trim_end_matches('/').to_owned(),
    };
    router.route_service(
        "/elitea.llm.v1.ModelService/{*method}",
        ModelServiceServer::new(bridge),
    )
}

type FrameStream<T> = Pin<Box<dyn Stream<Item = Result<T, Status>> + Send>>;

/// The HTTP answer of the Router.
struct Answer {
    status: StatusCode,
    content_type: String,
    retry_after: String,
    body: Body,
}

impl Bridge {
    async fn http(
        &self,
        route: Route,
        metadata: &MetadataMap,
        call: Option<&pb::ModelCall>,
        content_type: &str,
        extra: &[(&'static str, String)],
        body: Body,
    ) -> Result<Answer, Status> {
        let mut request = Request::post(format!("{}{}", self.base, route.path()))
            .header("content-type", content_type);
        if let Some(value) = metadata.get("authorization") {
            request = request.header(
                "authorization",
                value
                    .to_str()
                    .map_err(|_| Status::invalid_argument("authorization"))?,
            );
        }
        if let Some(call) = call {
            if !call.project_id.is_empty() {
                request = request.header("x-project-id", &call.project_id);
            }
            if !call.execution_id.is_empty() {
                request = request.header("x-elitea-execution-id", &call.execution_id);
            }
        }
        for (name, value) in extra {
            request = request.header(*name, value);
        }
        let request = request
            .body(body)
            .map_err(|_| Status::invalid_argument("the call is not an HTTP request"))?;
        let Ok(response) = tower::ServiceExt::oneshot(self.router.clone(), request).await;
        let header = |name| {
            response
                .headers()
                .get(name)
                .and_then(|value: &HeaderValue| value.to_str().ok())
                .unwrap_or_default()
                .to_owned()
        };
        let content_type = header(http::header::CONTENT_TYPE);
        let retry_after = header(http::header::RETRY_AFTER);
        Ok(Answer {
            status: response.status(),
            content_type,
            retry_after,
            body: response.into_body(),
        })
    }
}

/// The refusal status elitea-main answers for a non-2xx HTTP answer.
async fn refusal(answer: Answer) -> Status {
    let body = axum::body::to_bytes(answer.body, 64 * 1024)
        .await
        .unwrap_or_default();
    let refusal = pb::ModelRefusal {
        http_status: i32::from(answer.status.as_u16()),
        content_type: answer.content_type,
        body,
        retry_after: answer.retry_after,
    };
    let mut metadata = MetadataMap::new();
    metadata.insert_bin(
        REFUSAL_TRAILER,
        MetadataValue::from_bytes(&refusal.encode_to_vec()),
    );
    Status::with_metadata(
        code_for_http_status(answer.status.as_u16()),
        format!(
            "the model call was refused (HTTP {})",
            answer.status.as_u16()
        ),
        metadata,
    )
}

async fn whole(answer: Answer) -> Result<(pb::ResponseHead, Bytes), Status> {
    if !answer.status.is_success() {
        return Err(refusal(answer).await);
    }
    // elitea-main's unary body cap: the gateway's 64 MiB
    // (internal/transport/modelgrpc).
    let body = axum::body::to_bytes(answer.body, 64 * 1024 * 1024)
        .await
        .map_err(|_| Status::unavailable("the answer broke"))?;
    Ok((
        pb::ResponseHead {
            content_type: answer.content_type,
        },
        body,
    ))
}

async fn streamed<T: Send + 'static>(
    answer: Answer,
    head: fn(pb::ResponseHead) -> T,
    data: fn(Bytes) -> T,
) -> Result<FrameStream<T>, Status> {
    if !answer.status.is_success() {
        return Err(refusal(answer).await);
    }
    let first = futures_util::stream::once(async move {
        Ok(head(pb::ResponseHead {
            content_type: answer.content_type,
        }))
    });
    let rest = answer.body.into_data_stream().map(move |chunk| {
        chunk
            .map(data)
            .map_err(|_| Status::unavailable("the model stream broke"))
    });
    Ok(Box::pin(first.chain(rest)))
}

#[tonic::async_trait]
impl ModelService for Bridge {
    type ChatCompletionsStream = FrameStream<pb::ChatCompletionsResponse>;
    type MessagesStream = FrameStream<pb::MessagesResponse>;

    async fn chat_completions(
        &self,
        request: tonic::Request<pb::ChatCompletionsRequest>,
    ) -> Result<tonic::Response<Self::ChatCompletionsStream>, Status> {
        let (metadata, _, message) = request.into_parts();
        let answer = self
            .http(
                Route::ChatCompletions,
                &metadata,
                message.call.as_ref(),
                "application/json",
                &[("accept", "text/event-stream".to_owned())],
                Body::from(message.body),
            )
            .await?;
        let frames = streamed(
            answer,
            |head| pb::ChatCompletionsResponse {
                frame: Some(pb::chat_completions_response::Frame::Head(head)),
            },
            |data| pb::ChatCompletionsResponse {
                frame: Some(pb::chat_completions_response::Frame::Data(data)),
            },
        )
        .await?;
        Ok(tonic::Response::new(frames))
    }

    async fn messages(
        &self,
        request: tonic::Request<pb::MessagesRequest>,
    ) -> Result<tonic::Response<Self::MessagesStream>, Status> {
        let (metadata, _, message) = request.into_parts();
        let mut extra = vec![("accept", "text/event-stream".to_owned())];
        if !message.anthropic_version.is_empty() {
            extra.push(("anthropic-version", message.anthropic_version.clone()));
        }
        for beta in &message.anthropic_beta {
            extra.push(("anthropic-beta", beta.clone()));
        }
        let answer = self
            .http(
                Route::Messages,
                &metadata,
                message.call.as_ref(),
                "application/json",
                &extra,
                Body::from(message.body),
            )
            .await?;
        let frames = streamed(
            answer,
            |head| pb::MessagesResponse {
                frame: Some(pb::messages_response::Frame::Head(head)),
            },
            |data| pb::MessagesResponse {
                frame: Some(pb::messages_response::Frame::Data(data)),
            },
        )
        .await?;
        Ok(tonic::Response::new(frames))
    }

    async fn embeddings(
        &self,
        request: tonic::Request<pb::EmbeddingsRequest>,
    ) -> Result<tonic::Response<pb::EmbeddingsResponse>, Status> {
        let (metadata, _, message) = request.into_parts();
        let answer = self
            .http(
                Route::Embeddings,
                &metadata,
                message.call.as_ref(),
                "application/json",
                &[],
                Body::from(message.body),
            )
            .await?;
        let (head, body) = whole(answer).await?;
        Ok(tonic::Response::new(pb::EmbeddingsResponse {
            head: Some(head),
            body,
        }))
    }

    async fn metered_admit(
        &self,
        request: tonic::Request<pb::MeteredAdmitRequest>,
    ) -> Result<tonic::Response<pb::MeteredAdmitResponse>, Status> {
        let (metadata, _, message) = request.into_parts();
        let answer = self
            .http(
                Route::MeteredAdmit,
                &metadata,
                message.call.as_ref(),
                "application/json",
                &[],
                Body::from(message.body),
            )
            .await?;
        let (head, body) = whole(answer).await?;
        Ok(tonic::Response::new(pb::MeteredAdmitResponse {
            head: Some(head),
            body,
        }))
    }

    async fn metered_settle(
        &self,
        request: tonic::Request<pb::MeteredSettleRequest>,
    ) -> Result<tonic::Response<pb::MeteredSettleResponse>, Status> {
        let (metadata, _, message) = request.into_parts();
        let answer = self
            .http(
                Route::MeteredSettle,
                &metadata,
                message.call.as_ref(),
                "application/json",
                &[],
                Body::from(message.body),
            )
            .await?;
        let (head, body) = whole(answer).await?;
        Ok(tonic::Response::new(pb::MeteredSettleResponse {
            head: Some(head),
            body,
        }))
    }

    async fn image_generate(
        &self,
        request: tonic::Request<pb::ImageGenerateRequest>,
    ) -> Result<tonic::Response<pb::ImageGenerateResponse>, Status> {
        let (metadata, _, message) = request.into_parts();
        let answer = self
            .http(
                Route::ImageGenerate,
                &metadata,
                message.call.as_ref(),
                "application/json",
                &[],
                Body::from(message.body),
            )
            .await?;
        let (head, body) = whole(answer).await?;
        Ok(tonic::Response::new(pb::ImageGenerateResponse {
            head: Some(head),
            body,
        }))
    }

    async fn image_edit(
        &self,
        request: tonic::Request<Streaming<pb::ImageEditRequest>>,
    ) -> Result<tonic::Response<pb::ImageEditResponse>, Status> {
        let (metadata, _, mut frames) = request.into_parts();
        let Some(pb::ImageEditRequest {
            frame: Some(pb::image_edit_request::Frame::Head(head)),
        }) = frames.message().await?
        else {
            return Err(Status::invalid_argument(
                "an ImageEdit call starts with its head",
            ));
        };
        let mut body = Vec::new();
        while let Some(frame) = frames.message().await? {
            match frame.frame {
                Some(pb::image_edit_request::Frame::Data(data)) => body.extend_from_slice(&data),
                _ => return Err(Status::invalid_argument("an ImageEdit call has one head")),
            }
        }
        let answer = self
            .http(
                Route::ImageEdit,
                &metadata,
                head.call.as_ref(),
                &head.content_type,
                &[],
                Body::from(body),
            )
            .await?;
        let (head, body) = whole(answer).await?;
        Ok(tonic::Response::new(pb::ImageEditResponse {
            head: Some(head),
            body,
        }))
    }
}
