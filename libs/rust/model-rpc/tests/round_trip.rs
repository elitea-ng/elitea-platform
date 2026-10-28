//! The client against the test `ModelService` over a real HTTP/2 connection:
//! what an HTTP gateway answered arrives as the same status, headers and
//! bytes.

use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::extract::Request;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use bytes::Bytes;
use elitea_model_rpc::testing::with_model_service;
use elitea_model_rpc::{CallError, ModelRequest, ModelServiceClient, Route, call};
use futures_util::StreamExt as _;
use tokio::net::TcpListener;
use tokio::sync::mpsc;
use tonic::transport::Endpoint;

const FRAMES: [&str; 3] = [
    "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
    "data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n",
    "data: [DONE]\n\n",
];

async fn gateway(request: Request) -> Response {
    let path = request.uri().path().to_owned();
    let headers: HeaderMap = request.headers().clone();
    let body = axum::body::to_bytes(request.into_body(), 1 << 20)
        .await
        .unwrap_or_default();
    if headers.get("authorization").and_then(|v| v.to_str().ok()) != Some("Bearer good") {
        return (StatusCode::UNAUTHORIZED, "{\"error\":\"bad token\"}").into_response();
    }
    if body.starts_with(b"budget") {
        return (
            StatusCode::PAYMENT_REQUIRED,
            [("content-type", "application/json"), ("retry-after", "7")],
            r#"{"error":{"type":"budget_exceeded","scope":"project"}}"#,
        )
            .into_response();
    }
    #[allow(clippy::single_match_else)]
    match path.as_str() {
        "/llm/v1/chat/completions" => {
            let frames = futures_util::stream::iter(FRAMES).then(|frame| async move {
                tokio::time::sleep(Duration::from_millis(5)).await;
                Ok::<_, std::io::Error>(Bytes::from_static(frame.as_bytes()))
            });
            (
                [("content-type", "text/event-stream")],
                Body::from_stream(frames),
            )
                .into_response()
        }
        _ => {
            let echo = format!(
                "{path}|{}|{}|{}|{}",
                headers
                    .get("x-project-id")
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or_default(),
                headers
                    .get("x-elitea-execution-id")
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or_default(),
                headers
                    .get("content-type")
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or_default(),
                body.len()
            );
            ([("content-type", "application/json")], echo).into_response()
        }
    }
}

async fn serve(router: Router) -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let address = listener.local_addr().expect("address");
    tokio::spawn(async move { axum::serve(listener, router).await.expect("serve") });
    format!("http://{address}")
}

async fn client(origin: String) -> ModelServiceClient<tonic::transport::Channel> {
    let channel = Endpoint::from_shared(origin)
        .expect("endpoint")
        .connect()
        .await
        .expect("connect");
    ModelServiceClient::new(channel)
}

fn request(bearer: &str, body: &'static [u8]) -> ModelRequest {
    ModelRequest {
        bearer: bearer.to_owned(),
        project_id: Some("7".to_owned()),
        execution_id: Some("exec-1".to_owned()),
        body: Bytes::from_static(body),
        ..ModelRequest::default()
    }
}

async fn read(response: elitea_model_rpc::ModelResponse) -> Vec<u8> {
    let mut body = response.body;
    let mut bytes = Vec::new();
    while let Some(chunk) = body.next().await {
        bytes.extend_from_slice(&chunk.expect("chunk"));
    }
    bytes
}

#[tokio::test]
async fn a_stream_arrives_byte_identical() {
    let app = Router::new().fallback(gateway);
    let origin = serve(with_model_service(app, "/llm/v1")).await;
    let mut models = client(origin).await;
    let response = call(&mut models, Route::ChatCompletions, request("good", b"{}"))
        .await
        .expect("call");
    assert_eq!(response.status, StatusCode::OK);
    assert_eq!(response.headers["content-type"], "text/event-stream");
    assert_eq!(read(response).await, FRAMES.concat().into_bytes());
}

#[tokio::test]
async fn the_call_carries_its_selector_and_execution() {
    let app = Router::new().fallback(gateway);
    let origin = serve(with_model_service(app, "/llm/v1")).await;
    let mut models = client(origin).await;
    let response = call(
        &mut models,
        Route::Embeddings,
        request("good", b"{\"input\":1}"),
    )
    .await
    .expect("call");
    assert_eq!(
        read(response).await,
        b"/llm/v1/embeddings|7|exec-1|application/json|11"
    );
    let mut edit = request("good", b"--b\r\n--b--\r\n");
    edit.content_type = Some("multipart/form-data; boundary=b".to_owned());
    let response = call(&mut models, Route::ImageEdit, edit)
        .await
        .expect("call");
    assert_eq!(
        read(response).await,
        b"/llm/v1/images/edits|7|exec-1|multipart/form-data; boundary=b|12"
    );
}

#[tokio::test]
async fn refusals_read_as_the_http_answer() {
    let app = Router::new().fallback(gateway);
    let origin = serve(with_model_service(app, "/llm/v1")).await;
    let mut models = client(origin).await;
    let budget = call(
        &mut models,
        Route::ChatCompletions,
        request("good", b"budget"),
    )
    .await
    .expect("a refusal is a response");
    assert_eq!(budget.status, StatusCode::PAYMENT_REQUIRED);
    assert_eq!(budget.headers["retry-after"], "7");
    assert_eq!(
        read(budget).await,
        br#"{"error":{"type":"budget_exceeded","scope":"project"}}"#
    );
    let unauthorized = call(&mut models, Route::Embeddings, request("bad", b"{}"))
        .await
        .expect("a refusal is a response");
    assert_eq!(unauthorized.status, StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn an_unreachable_service_is_a_hop_failure() {
    // A privileged port nothing listens on: the connection is refused at once.
    // Not a freed ephemeral port: dropping a listener handed its port back, and a
    // parallel test's server could take it and answer 200.
    let origin = "http://127.0.0.1:1".to_owned();
    let channel = Endpoint::from_shared(origin)
        .expect("endpoint")
        .connect_lazy();
    let mut models = ModelServiceClient::new(channel);
    let outcome = call(&mut models, Route::Embeddings, request("good", b"{}")).await;
    assert!(matches!(outcome, Err(CallError::Hop(_))), "{outcome:?}");
}

#[tokio::test]
async fn dropping_the_stream_cancels_the_handler() {
    let (dropped, mut seen) = mpsc::channel::<()>(1);
    let app = Router::new().fallback(move |_: Request| {
        let dropped = dropped.clone();
        async move {
            struct Guard(mpsc::Sender<()>);
            impl Drop for Guard {
                fn drop(&mut self) {
                    let _ = self.0.try_send(());
                }
            }
            let guard = Guard(dropped);
            let frames = futures_util::stream::unfold(0_u8, move |n| {
                let _keep = &guard;
                async move {
                    if n > 0 {
                        tokio::time::sleep(Duration::from_mins(1)).await;
                    }
                    Some((
                        Ok::<_, std::io::Error>(Bytes::from_static(b"data: x\n\n")),
                        n + 1,
                    ))
                }
            });
            (
                [("content-type", "text/event-stream")],
                Body::from_stream(frames),
            )
                .into_response()
        }
    });
    let origin = serve(with_model_service(app, "/llm/v1")).await;
    let mut models = client(origin).await;
    let response = call(&mut models, Route::ChatCompletions, request("good", b"{}"))
        .await
        .expect("call");
    let mut body = response.body;
    assert_eq!(
        body.next().await.expect("one").expect("chunk"),
        "data: x\n\n"
    );
    drop(body);
    tokio::time::timeout(Duration::from_secs(5), seen.recv())
        .await
        .expect("the handler's stream was not dropped after the client cancelled");
}
