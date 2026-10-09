//! The model client against a mock OpenAI-compatible gateway on loopback.
//!
//! Each test scripts the gateway's replies, runs the client, and checks
//! both what the client returned and what it sent. One test runs over TLS
//! with a throwaway CA, to prove a private CA bundle (`TransportSettings::ca_file`).
//!
//! `ELITEA_MODEL_CLIENT_LIVE_LLM=1` adds a chat round trip against the LAN
//! vLLM (`ELITEA_MODEL_CLIENT_LIVE_LLM_BASE`, default
//! `http://192.168.29.60:8000/v1`).

// The mock vectors hold small whole numbers, exact in f32.
#![allow(clippy::float_cmp)]

use axum::Router;
use axum::body::{Body, Bytes};
use axum::extract::{Request, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::stream::StopSignal;
use elitea_model_client::embeddings::MIN_SPLIT_TOKENS;
use elitea_model_client::sse::SseLimits;
use elitea_model_client::tokens::embedding_split;
use elitea_model_client::transport::Backoff;
use elitea_model_client::{
    ChatClient, ChatMessage, ChatRequest, EmbeddingClient, EmbeddingOptions, ModelSettings,
    Sampling, Timeouts, ToolChoice, ToolDefinition, Transport, TransportSettings,
};
use serde_json::{Value, json};
use std::convert::Infallible;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::net::TcpListener;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

const KEY: &str = "sk-mock-0123456789-secret";

/// What the gateway saw.
#[derive(Debug, Clone)]
struct Seen {
    path: String,
    headers: HeaderMap,
    body: Value,
}

/// One scripted reply.
enum Reply {
    Json(StatusCode, Value, Vec<(&'static str, &'static str)>),
    /// SSE chunks, written with a pause between them.
    Stream(Vec<Vec<u8>>, Duration),
    /// Headers, then silence until the client gives up.
    Hang,
}

type Script = Arc<dyn Fn(usize, &Seen) -> Reply + Send + Sync>;

#[derive(Clone)]
struct Gateway {
    seen: Arc<Mutex<Vec<Seen>>>,
    script: Script,
}

impl Gateway {
    fn requests(&self) -> Vec<Seen> {
        self.seen.lock().map(|s| s.clone()).unwrap_or_default()
    }
}

async fn handle(State(gateway): State<Gateway>, request: Request) -> Response {
    let path = request.uri().path().to_owned();
    let headers = request.headers().clone();
    let bytes = axum::body::to_bytes(request.into_body(), 64 * 1024 * 1024)
        .await
        .unwrap_or_default();
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    let seen = Seen {
        path,
        headers,
        body,
    };
    let index = {
        let Ok(mut all) = gateway.seen.lock() else {
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        };
        all.push(seen.clone());
        all.len() - 1
    };
    match (gateway.script)(index, &seen) {
        Reply::Json(status, value, headers) => {
            let mut response = (status, axum::Json(value)).into_response();
            for (name, value) in headers {
                if let Ok(value) = value.parse() {
                    response.headers_mut().insert(name, value);
                }
            }
            response
        }
        Reply::Stream(chunks, pause) => {
            let (sender, receiver) = mpsc::channel::<Result<Bytes, Infallible>>(4);
            tokio::spawn(async move {
                for chunk in chunks {
                    if sender.send(Ok(Bytes::from(chunk))).await.is_err() {
                        return;
                    }
                    tokio::time::sleep(pause).await;
                }
            });
            Response::builder()
                .header("content-type", "text/event-stream")
                .body(Body::from_stream(ReceiverStream::new(receiver)))
                .unwrap_or_default()
        }
        Reply::Hang => {
            let (sender, receiver) = mpsc::channel::<Result<Bytes, Infallible>>(1);
            tokio::spawn(async move {
                tokio::time::sleep(Duration::from_mins(1)).await;
                drop(sender);
            });
            Response::builder()
                .header("content-type", "text/event-stream")
                .body(Body::from_stream(ReceiverStream::new(receiver)))
                .unwrap_or_default()
        }
    }
}

/// Start a gateway on loopback; returns it and its `/v1` base URL.
async fn gateway(
    script: impl Fn(usize, &Seen) -> Reply + Send + Sync + 'static,
) -> (Gateway, String) {
    let gateway = Gateway {
        seen: Arc::new(Mutex::new(Vec::new())),
        script: Arc::new(script),
    };
    let Ok(listener) = TcpListener::bind("127.0.0.1:0").await else {
        panic!("cannot bind loopback");
    };
    let Ok(address) = listener.local_addr() else {
        panic!("no local address");
    };
    let app = Router::new().fallback(handle).with_state(gateway.clone());
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    (gateway, format!("http://{address}/llm/v1"))
}

#[allow(clippy::needless_pass_by_value)]
fn settings(base: &str, extra: Value) -> ModelSettings {
    let mut block = json!({
        "api_base": base,
        "api_key": KEY,
        "organization": "42",
        "execution_id": "callback-test-run",
        "model_name": "gpt-4o",
    });
    if let (Some(block), Some(extra)) = (block.as_object_mut(), extra.as_object()) {
        for (key, value) in extra {
            block.insert(key.clone(), value.clone());
        }
    }
    match ModelSettings::from_llm_settings(&block) {
        Ok(settings) => settings,
        Err(error) => panic!("{error}"),
    }
}

fn fast() -> TransportSettings {
    TransportSettings {
        ca_file: None,
        timeouts: Timeouts {
            connect: Duration::from_secs(2),
            request: Duration::from_secs(5),
            stream_idle: Duration::from_millis(500),
            stream_total: Duration::from_secs(5),
        },
        backoff: Backoff {
            initial: Duration::from_millis(5),
            max: Duration::from_millis(20),
            max_retry_after: Duration::from_secs(1),
        },
        ..TransportSettings::default()
    }
}

fn transport(settings: &TransportSettings) -> Transport {
    match Transport::new(settings) {
        Ok(transport) => transport,
        Err(error) => panic!("{error}"),
    }
}

fn embedder(base: &str, options: EmbeddingOptions) -> EmbeddingClient {
    EmbeddingClient::new(
        transport(&fast()),
        settings(base, json!({})),
        "emb-model",
        options,
    )
}

/// A vector that encodes the text it is for: `t17` → `[17, dims-1 zeros]`.
fn vector_for(text: &str, dims: usize) -> Vec<f64> {
    let mut vector = vec![0.0; dims];
    vector[0] = text.trim_start_matches('t').parse().unwrap_or(-1.0);
    vector
}

/// An embeddings reply with `dims` dimensions, data in REVERSE order (the
/// client must sort by `index`).
fn embeddings_reply(seen: &Seen, dims: usize) -> Reply {
    let inputs: Vec<String> = match &seen.body["input"] {
        Value::Array(items) => items
            .iter()
            .map(|i| i.as_str().unwrap_or("").to_owned())
            .collect(),
        Value::String(text) => vec![text.clone()],
        _ => Vec::new(),
    };
    let mut data: Vec<Value> = inputs
        .iter()
        .enumerate()
        .map(|(index, text)| json!({"object": "embedding", "index": index, "embedding": vector_for(text, dims)}))
        .collect();
    data.reverse();
    Reply::Json(
        StatusCode::OK,
        json!({"object": "list", "data": data, "model": "emb-model", "usage": {"prompt_tokens": inputs.len(), "total_tokens": inputs.len()}}),
        Vec::new(),
    )
}

fn texts(n: usize) -> Vec<String> {
    (0..n).map(|i| format!("t{i}")).collect()
}

fn ok<T>(result: Result<T, EngineError>) -> T {
    match result {
        Ok(value) => value,
        Err(error) => panic!("{error}"),
    }
}

fn err<T: std::fmt::Debug>(result: Result<T, EngineError>) -> EngineError {
    match result {
        Ok(value) => panic!("expected an error, got {value:?}"),
        Err(error) => error,
    }
}

#[tokio::test]
async fn embeddings_batch_in_order_with_the_right_headers() {
    let (gw, base) = gateway(|_, seen| embeddings_reply(seen, 8)).await;
    let client = embedder(
        &base,
        EmbeddingOptions {
            batch_size: 64,
            concurrency: 2,
            ..EmbeddingOptions::default()
        },
    );
    let vectors = ok(client
        .embed_documents(&texts(150), &StopSignal::default())
        .await);
    assert_eq!(vectors.len(), 150);
    for (i, vector) in vectors.iter().enumerate() {
        #[allow(clippy::cast_precision_loss)]
        let expected = i as f32;
        assert_eq!(vector[0], expected, "vector {i} out of order");
        assert_eq!(vector.len(), 8);
    }
    let requests = gw.requests();
    let mut sizes: Vec<usize> = requests
        .iter()
        .map(|r| r.body["input"].as_array().map_or(0, Vec::len))
        .collect();
    sizes.sort_unstable();
    assert_eq!(sizes, [22, 64, 64]);
    for request in &requests {
        assert_eq!(request.path, "/llm/v1/embeddings");
        assert_eq!(
            request
                .headers
                .get("authorization")
                .and_then(|v| v.to_str().ok()),
            Some(format!("Bearer {KEY}").as_str())
        );
        // The worker's selector and attribution headers; no
        // OpenAI-Organization (the edge's fallback selector) beside them.
        assert_eq!(
            request
                .headers
                .get("x-project-id")
                .and_then(|v| v.to_str().ok()),
            Some("42")
        );
        assert_eq!(
            request
                .headers
                .get("x-elitea-execution-id")
                .and_then(|v| v.to_str().ok()),
            Some("callback-test-run")
        );
        assert!(request.headers.get("openai-organization").is_none());
        assert_eq!(request.body["model"], "emb-model");
        assert_eq!(request.body["encoding_format"], "float");
    }
    assert_eq!(client.dimension(), Some(8));
    assert_eq!(client.prompt_tokens(), 150);
    let query = ok(client.embed_query("t7", &StopSignal::default()).await);
    assert_eq!(query[0], 7.0);
}

#[tokio::test]
async fn the_dimension_comes_from_the_first_response_not_a_constant() {
    let (_, base) =
        gateway(|index, seen| embeddings_reply(seen, if index == 0 { 3072 } else { 1536 })).await;
    let client = embedder(&base, EmbeddingOptions::default());
    let first = ok(client
        .embed_documents(&texts(3), &StopSignal::default())
        .await);
    assert!(first.iter().all(|v| v.len() == 3072));
    assert_eq!(client.dimension(), Some(3072));
    let error = err(client
        .embed_documents(&texts(2), &StopSignal::default())
        .await);
    assert!(error.message.contains("'emb-model'"), "{error}");
    assert!(
        error.message.contains("1536") && error.message.contains("3072"),
        "{error}"
    );
    assert_eq!(error.category(), "inference_failed");
}

#[tokio::test]
async fn a_mixed_batch_is_refused() {
    let (_, base) = gateway(|_, _| {
        Reply::Json(
            StatusCode::OK,
            json!({"data": [{"index": 0, "embedding": [1.0, 2.0]}, {"index": 1, "embedding": [1.0]}]}),
            Vec::new(),
        )
    })
    .await;
    let client = embedder(&base, EmbeddingOptions::default());
    let error = err(client
        .embed_documents(&texts(2), &StopSignal::default())
        .await);
    assert!(error.message.contains("emb-model"), "{error}");
}

#[tokio::test]
async fn an_empty_text_gets_the_vector_of_the_empty_string_once() {
    let (gw, base) = gateway(|_, seen| embeddings_reply(seen, 4)).await;
    let client = embedder(&base, EmbeddingOptions::default());
    let input = vec![String::new(), "t3".to_owned(), String::new()];
    let vectors = ok(client.embed_documents(&input, &StopSignal::default()).await);
    assert_eq!(vectors.len(), 3);
    assert_eq!(vectors[1][0], 3.0);
    assert_eq!(vectors[0], vectors[2]);
    let requests = gw.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[1].body["input"], json!(""));
}

#[tokio::test]
async fn a_long_text_is_embedded_in_windows_and_averaged() {
    let (gw, base) = gateway(|_, seen| {
        // Every window gets the same unit vector, so the normalised
        // average is that vector again.
        let n = seen.body["input"].as_array().map_or(0, Vec::len);
        let data: Vec<Value> = (0..n)
            .map(|i| json!({"index": i, "embedding": [0.0, 2.0]}))
            .collect();
        Reply::Json(StatusCode::OK, json!({"data": data}), Vec::new())
    })
    .await;
    let client = embedder(
        &base,
        EmbeddingOptions {
            ctx_length: 4,
            ..EmbeddingOptions::default()
        },
    );
    let text = "one two three four five six seven eight nine ten".to_owned();
    let vectors = ok(client
        .embed_documents(&[text], &StopSignal::default())
        .await);
    assert_eq!(vectors, [vec![0.0, 1.0]]);
    let windows = gw.requests()[0].body["input"]
        .as_array()
        .map_or(0, Vec::len);
    assert!(windows >= 3, "{windows}");
}

/// A text of about `words` words, each its own `cl100k_base` token or two.
fn prose(words: usize) -> String {
    use std::fmt::Write as _;
    (0..words).fold(String::new(), |mut text, i| {
        let _ = write!(text, "word{i} ");
        text
    })
}

/// The inputs of one embeddings request.
fn inputs_of(seen: &Seen) -> Vec<String> {
    match &seen.body["input"] {
        Value::Array(items) => items
            .iter()
            .map(|i| i.as_str().unwrap_or("").to_owned())
            .collect(),
        Value::String(text) => vec![text.clone()],
        _ => Vec::new(),
    }
}

/// vLLM's context refusal, as its OpenAI-compatible server sends it.
fn vllm_context_refusal() -> Reply {
    Reply::Json(
        StatusCode::BAD_REQUEST,
        json!({
            "object": "error",
            "message": "This model's maximum context length is 8192 tokens. However, you requested 8193 tokens in the input for embedding generation. Please reduce the length of the input.",
            "type": "BadRequestError",
            "param": null,
            "code": 400
        }),
        Vec::new(),
    )
}

/// A model that refuses, with vLLM's 400, every request holding an input
/// longer than `max_chars`; otherwise each text's vector is
/// `[chars, 1]`.
fn context_limited(seen: &Seen, max_chars: usize) -> Reply {
    let inputs = inputs_of(seen);
    if inputs.iter().any(|text| text.chars().count() > max_chars) {
        return vllm_context_refusal();
    }
    let data: Vec<Value> = inputs
        .iter()
        .enumerate()
        .map(|(index, text)| {
            #[allow(clippy::cast_precision_loss)]
            let chars = text.chars().count() as f64;
            json!({"index": index, "embedding": [chars, 1.0]})
        })
        .collect();
    Reply::Json(StatusCode::OK, json!({"data": data}), Vec::new())
}

#[tokio::test]
async fn a_window_over_the_models_context_is_cut_and_averaged() {
    let long = prose(400);
    let max_chars = long.chars().count() * 3 / 5;
    let (gw, base) = gateway(move |_, seen| context_limited(seen, max_chars)).await;
    let client = embedder(&base, EmbeddingOptions::default());
    let texts = vec!["t1".to_owned(), long.clone(), "t2 t2".to_owned()];
    let vectors = ok(client.embed_documents(&texts, &StopSignal::default()).await);

    // The short texts keep their own vectors, in their places.
    assert_eq!(vectors.len(), 3);
    assert_eq!(vectors[0], [2.0, 1.0]);
    assert_eq!(vectors[2], [5.0, 1.0]);

    // The long text is the token-weighted, normalised average of its
    // halves' vectors.
    let whole = ok(embedding_split(&long, usize::MAX));
    let halves = ok(embedding_split(&long, whole[0].tokens.div_ceil(2)));
    assert_eq!(halves.len(), 2);
    #[allow(clippy::cast_precision_loss)]
    let (mut x, mut y, mut total) = (0.0_f64, 0.0_f64, 0.0_f64);
    for half in &halves {
        #[allow(clippy::cast_precision_loss)]
        let (chars, weight) = (half.text.chars().count() as f64, half.tokens as f64);
        x += chars * weight;
        y += weight;
        total += weight;
    }
    let (x, y) = (x / total, y / total);
    let norm = x.hypot(y);
    let expected = [x / norm, y / norm];
    for (got, want) in vectors[1].iter().zip(expected) {
        assert!(
            (f64::from(*got) - want).abs() < 1e-6,
            "{:?} vs {expected:?}",
            vectors[1]
        );
    }

    // The batch, then each window alone, then the two halves.
    let sizes: Vec<usize> = gw.requests().iter().map(|r| inputs_of(r).len()).collect();
    assert_eq!(sizes, [3, 1, 1, 1, 1, 1]);
    assert_eq!(client.split_windows(), 1);
    assert_eq!(client.fallback_requests(), 5);
}

#[tokio::test]
async fn a_lone_refused_window_is_cut_without_a_repeat() {
    let long = prose(400);
    let max_chars = long.chars().count() * 3 / 5;
    let (gw, base) = gateway(move |_, seen| context_limited(seen, max_chars)).await;
    let client = embedder(&base, EmbeddingOptions::default());
    let vector = ok(client.embed_query(&long, &StopSignal::default()).await);
    assert_eq!(vector.len(), 2);
    // The refused request held only this window: the next two are halves.
    assert_eq!(gw.requests().len(), 3);
    assert_eq!(client.split_windows(), 1);
}

#[tokio::test]
async fn another_bad_request_still_fails_the_call() {
    let (gw, base) = gateway(|_, _| {
        Reply::Json(
            StatusCode::BAD_REQUEST,
            json!({"error": {"message": "The model `emb-model` does not exist.", "type": "NotFoundError", "code": 400}}),
            Vec::new(),
        )
    })
    .await;
    let client = embedder(&base, EmbeddingOptions::default());
    let error = err(client
        .embed_documents(&[prose(400), "t1".to_owned()], &StopSignal::default())
        .await);
    assert!(error.message.contains("HTTP 400"), "{error}");
    assert!(error.message.contains("does not exist"), "{error}");
    assert_eq!(gw.requests().len(), 1);
    assert_eq!(client.split_windows(), 0);
    assert_eq!(client.fallback_requests(), 0);
}

#[tokio::test]
async fn a_context_refusal_below_the_floor_fails_naming_the_setting() {
    // Refuses every request as over its context.
    let (gw, base) = gateway(|_, _| vllm_context_refusal()).await;
    // The consumer names its own setting; the client repeats that name.
    let client = embedder(
        &base,
        EmbeddingOptions {
            ctx_setting: "TEST_EMBED_CTX_TOKENS",
            ..EmbeddingOptions::default()
        },
    );
    let error = err(client
        .embed_documents(&[prose(400)], &StopSignal::default())
        .await);
    assert!(error.message.contains("TEST_EMBED_CTX_TOKENS"), "{error}");
    assert!(
        error.message.contains(&MIN_SPLIT_TOKENS.to_string()),
        "{error}"
    );
    assert_eq!(error.category(), "inference_failed");
    // Cut down to the floor, never past it: a few requests, not hundreds.
    let sent = gw.requests().len();
    assert!((3..=16).contains(&sent), "{sent}");
}

#[tokio::test]
async fn busy_replies_are_retried_with_the_server_delay() {
    let (gw, base) = gateway(|index, seen| {
        if index < 2 {
            Reply::Json(
                StatusCode::TOO_MANY_REQUESTS,
                json!({"error": {"message": "slow down"}}),
                vec![("retry-after-ms", "10")],
            )
        } else {
            embeddings_reply(seen, 2)
        }
    })
    .await;
    let client = embedder(&base, EmbeddingOptions::default());
    let vectors = ok(client
        .embed_documents(&texts(1), &StopSignal::default())
        .await);
    assert_eq!(vectors.len(), 1);
    assert_eq!(gw.requests().len(), 3);
}

#[tokio::test]
async fn retries_stop_at_max_retries() {
    let (gw, base) = gateway(|_, _| {
        Reply::Json(
            StatusCode::BAD_GATEWAY,
            json!({"error": {"message": "upstream down"}}),
            Vec::new(),
        )
    })
    .await;
    let client = embedder(&base, EmbeddingOptions::default());
    let error = err(client
        .embed_documents(&texts(1), &StopSignal::default())
        .await);
    assert_eq!(gw.requests().len(), 3, "max_retries=2 is three attempts");
    assert!(error.message.contains("after 3 attempts"), "{error}");
    assert!(error.message.contains("upstream down"), "{error}");
    assert_eq!(error.category(), "inference_failed");

    let (gw, base) =
        gateway(|_, _| Reply::Json(StatusCode::SERVICE_UNAVAILABLE, json!({}), Vec::new())).await;
    let client = EmbeddingClient::new(
        transport(&fast()),
        settings(&base, json!({"max_retries": 0})),
        "emb-model",
        EmbeddingOptions::default(),
    );
    let error = err(client
        .embed_documents(&texts(1), &StopSignal::default())
        .await);
    assert_eq!(gw.requests().len(), 1);
    assert_eq!(error.category(), "service_busy");
}

#[tokio::test]
async fn refusals_land_in_their_categories_without_retries() {
    for (status, category) in [
        (StatusCode::NOT_FOUND, "resource_not_found"),
        (StatusCode::PAYMENT_REQUIRED, "invalid_input"),
        (StatusCode::UNAUTHORIZED, "inference_failed"),
        (StatusCode::BAD_REQUEST, "inference_failed"),
    ] {
        let (gw, base) = gateway(move |_, _| {
            Reply::Json(status, json!({"error": {"message": "no"}}), Vec::new())
        })
        .await;
        let client = ChatClient::new(
            transport(&fast()),
            settings(&base, json!({"streaming": false})),
        );
        let error = err(client
            .complete(
                &ChatRequest::new(vec![ChatMessage::User("hi".to_owned())]),
                &StopSignal::default(),
            )
            .await);
        assert_eq!(error.category(), category, "{status}: {error}");
        assert_eq!(gw.requests().len(), 1, "{status} is not retried");
    }
}

#[tokio::test]
async fn a_stop_aborts_a_hung_request_and_a_backoff() {
    let (_, base) = gateway(|_, _| Reply::Hang).await;
    let client = ChatClient::new(transport(&fast()), settings(&base, json!({})));
    let stop = StopSignal::default();
    let trigger = stop.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(100)).await;
        trigger.request();
    });
    // The stop (100 ms) lands before the idle timeout (500 ms) would.
    let error = err(client
        .stream(
            &ChatRequest::new(vec![ChatMessage::User("hi".to_owned())]),
            &stop,
            &mut |_| {},
        )
        .await);
    assert_eq!(error, EngineError::cancelled());

    // A long server delay, then a stop during the wait.
    let (gw, base) = gateway(|_, _| {
        Reply::Json(
            StatusCode::TOO_MANY_REQUESTS,
            json!({}),
            vec![("retry-after", "1")],
        )
    })
    .await;
    let client = embedder(&base, EmbeddingOptions::default());
    let stop = StopSignal::default();
    let trigger = stop.clone();
    let watched = gw.clone();
    tokio::spawn(async move {
        // Stop once the first refusal is in, i.e. during the 1 s wait.
        while watched.requests().is_empty() {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        trigger.request();
    });
    let error = err(client.embed_documents(&texts(1), &stop).await);
    assert_eq!(error, EngineError::cancelled());
    assert_eq!(gw.requests().len(), 1);

    // A stop that came first sends nothing.
    let stop = StopSignal::default();
    stop.request();
    assert_eq!(
        err(client.embed_documents(&texts(1), &stop).await),
        EngineError::cancelled()
    );
    assert_eq!(gw.requests().len(), 1);
}

fn sse(events: &[Value]) -> Vec<u8> {
    let mut out = Vec::new();
    for event in events {
        out.extend_from_slice(format!("data: {event}\n\n").as_bytes());
    }
    out.extend_from_slice(b"data: [DONE]\n\n");
    out
}

/// Split bytes into chunks of `size` (to cut events and UTF-8 apart).
fn chunked(bytes: &[u8], size: usize) -> Vec<Vec<u8>> {
    bytes.chunks(size).map(<[u8]>::to_vec).collect()
}

#[allow(clippy::needless_pass_by_value)]
fn delta(content: Value) -> Value {
    json!({"choices": [{"index": 0, "delta": content, "finish_reason": null}]})
}

#[tokio::test]
async fn a_streamed_answer_arrives_in_fragments_with_usage() {
    let stream = sse(&[
        delta(json!({"role": "assistant", "content": ""})),
        delta(json!({"content": "Grüße, "})),
        delta(json!({"content": "世界"})),
        json!({"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}),
        json!({"choices": [], "usage": {"prompt_tokens": 11, "completion_tokens": 3, "total_tokens": 14}}),
    ]);
    let (gw, base) =
        gateway(move |_, _| Reply::Stream(chunked(&stream, 7), Duration::from_millis(1))).await;
    let client = ChatClient::new(transport(&fast()), settings(&base, json!({})));
    let mut fragments = Vec::new();
    let request = ChatRequest::new(vec![
        ChatMessage::System("You write wikis."),
        ChatMessage::User("<repository>…</repository>".to_owned()),
    ]);
    let response = ok(client
        .chat(&request, &StopSignal::default(), &mut |text| {
            fragments.push(text.to_owned());
        })
        .await);
    assert_eq!(fragments, ["Grüße, ", "世界"]);
    assert_eq!(response.content, "Grüße, 世界");
    assert_eq!(response.finish_reason.as_deref(), Some("stop"));
    assert_eq!(response.usage.map(|u| u.total_tokens), Some(14));
    let body = &gw.requests()[0].body;
    assert_eq!(body["stream"], true);
    assert_eq!(body["stream_options"]["include_usage"], true);
    assert_eq!(body["max_completion_tokens"], 64000);
    assert_eq!(body["temperature"], 0.1);
    assert_eq!(body["messages"][0]["role"], "system");
    assert_eq!(
        gw.requests()[0]
            .headers
            .get("accept")
            .and_then(|v| v.to_str().ok()),
        Some("text/event-stream")
    );
}

#[tokio::test]
async fn streamed_tool_call_deltas_assemble() {
    let tool_delta = |calls: Value| delta(json!({"tool_calls": calls}));
    let stream = sse(&[
        tool_delta(
            json!([{"index": 0, "id": "call_a", "type": "function", "function": {"name": "get_code", "arguments": ""}}]),
        ),
        tool_delta(
            json!([{"index": 1, "id": "call_b", "type": "function", "function": {"name": "search_docs", "arguments": "{\"q\""}}]),
        ),
        tool_delta(json!([{"index": 0, "function": {"arguments": "{\"path\": \"src/"}}])),
        tool_delta(json!([{"index": 1, "function": {"arguments": ": \"auth\"}"}}])),
        tool_delta(json!([{"index": 0, "function": {"arguments": "main.rs\"}"}}])),
        json!({"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]}),
    ]);
    let (gw, base) = gateway(move |_, _| Reply::Stream(chunked(&stream, 13), Duration::ZERO)).await;
    let client = ChatClient::new(transport(&fast()), settings(&base, json!({})));
    let mut request = ChatRequest::new(vec![ChatMessage::User("where is main?".to_owned())]);
    request.tools = vec![
        ToolDefinition {
            name: "get_code".to_owned(),
            description: "Read a file".to_owned(),
            parameters: json!({"type": "object", "properties": {"path": {"type": "string"}}}),
        },
        ToolDefinition {
            name: "search_docs".to_owned(),
            description: "Search".to_owned(),
            parameters: json!({"type": "object", "properties": {"q": {"type": "string"}}}),
        },
    ];
    request.tool_choice = Some(ToolChoice::Auto);
    let response = ok(client
        .stream(&request, &StopSignal::default(), &mut |_| {})
        .await);
    assert_eq!(response.finish_reason.as_deref(), Some("tool_calls"));
    assert_eq!(response.tool_calls.len(), 2);
    assert_eq!(response.tool_calls[0].id, "call_a");
    assert_eq!(response.tool_calls[0].name, "get_code");
    assert_eq!(
        ok(response.tool_calls[0].parsed_arguments()).get("path"),
        Some(&json!("src/main.rs"))
    );
    assert_eq!(
        ok(response.tool_calls[1].parsed_arguments()).get("q"),
        Some(&json!("auth"))
    );
    let body = &gw.requests()[0].body;
    assert_eq!(body["tools"][0]["type"], "function");
    assert_eq!(body["tools"][1]["function"]["name"], "search_docs");
    assert_eq!(body["tool_choice"], "auto");
}

#[tokio::test]
async fn a_tool_round_trip_is_sent_in_openai_shape() {
    let (gw, base) = gateway(|_, _| {
        Reply::Json(
            StatusCode::OK,
            json!({"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "done"}}],
                   "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}),
            Vec::new(),
        )
    })
    .await;
    let client = ChatClient::new(
        transport(&fast()),
        settings(&base, json!({"model_name": "o3-mini", "max_tokens": 4096})),
    );
    let call = elitea_model_client::ToolCall {
        id: "call_1".to_owned(),
        name: "think".to_owned(),
        arguments: "{}".to_owned(),
    };
    let mut request = ChatRequest::new(vec![
        ChatMessage::System("plan"),
        ChatMessage::User("q".to_owned()),
        ChatMessage::Assistant {
            content: None,
            tool_calls: vec![call],
        },
        ChatMessage::Tool {
            tool_call_id: "call_1".to_owned(),
            content: "ok".to_owned(),
        },
    ]);
    request.sampling = Sampling::Deterministic;
    request.max_tokens = Some(1000);
    let response = ok(client.complete(&request, &StopSignal::default()).await);
    assert_eq!(response.content, "done");
    let body = &gw.requests()[0].body;
    assert_eq!(body["stream"], false);
    assert!(body.get("stream_options").is_none());
    assert_eq!(
        body["messages"][0]["role"], "developer",
        "o-digit models take developer"
    );
    assert_eq!(
        body["temperature"], 1.0,
        "o-series refuses any other temperature"
    );
    assert_eq!(body["max_completion_tokens"], 1000);
    assert_eq!(
        body["messages"][2]["tool_calls"][0]["function"]["name"],
        "think"
    );
    assert_eq!(
        body["messages"][3],
        json!({"role": "tool", "tool_call_id": "call_1", "content": "ok"})
    );
}

#[tokio::test]
async fn malformed_streams_fail_as_inference_errors() {
    let cases: Vec<(&str, Vec<u8>)> = vec![
        ("not json", b"data: {oops\n\n".to_vec()),
        (
            "ends early",
            b"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n".to_vec(),
        ),
        (
            "error event",
            b"data: {\"error\":{\"message\":\"provider overloaded\"}}\n\n".to_vec(),
        ),
        (
            "nameless tool",
            sse(&[delta(
                json!({"tool_calls": [{"index": 0, "function": {"arguments": "{}"}}]}),
            )]),
        ),
    ];
    for (name, stream) in cases {
        let (_, base) =
            gateway(move |_, _| Reply::Stream(vec![stream.clone()], Duration::ZERO)).await;
        let client = ChatClient::new(transport(&fast()), settings(&base, json!({})));
        let error = err(client
            .stream(
                &ChatRequest::new(vec![ChatMessage::User("q".to_owned())]),
                &StopSignal::default(),
                &mut |_| {},
            )
            .await);
        assert_eq!(error.category(), "inference_failed", "{name}: {error}");
        assert!(!error.message.contains(KEY), "{name}");
    }
}

#[tokio::test]
async fn an_oversized_event_is_refused() {
    let big = "x".repeat(4096);
    let stream = sse(&[delta(json!({"content": big}))]);
    let (_, base) = gateway(move |_, _| Reply::Stream(chunked(&stream, 512), Duration::ZERO)).await;
    let client = ChatClient::new(transport(&fast()), settings(&base, json!({}))).with_sse_limits(
        SseLimits {
            max_event_bytes: 1024,
            ..SseLimits::default()
        },
    );
    let error = err(client
        .stream(
            &ChatRequest::new(vec![ChatMessage::User("q".to_owned())]),
            &StopSignal::default(),
            &mut |_| {},
        )
        .await);
    assert!(error.message.contains("size cap"), "{error}");
    assert_eq!(error.category(), "inference_failed");
}

#[tokio::test]
async fn a_silent_stream_times_out() {
    let (_, base) = gateway(|_, _| Reply::Hang).await;
    let client = ChatClient::new(transport(&fast()), settings(&base, json!({})));
    let error = err(client
        .stream(
            &ChatRequest::new(vec![ChatMessage::User("q".to_owned())]),
            &StopSignal::default(),
            &mut |_| {},
        )
        .await);
    assert_eq!(error.category(), "timeout_error", "{error}");
}

#[tokio::test]
async fn the_key_never_reaches_errors_or_logs() {
    // The gateway echoes the Authorization header back in its refusal.
    let (_, base) = gateway(|_, seen| {
        let echoed = seen
            .headers
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_owned();
        Reply::Json(
            StatusCode::INTERNAL_SERVER_ERROR,
            json!({"error": {"message": format!("bad request with {echoed}")}}),
            Vec::new(),
        )
    })
    .await;
    let logs = Arc::new(Mutex::new(Vec::<u8>::new()));
    let writer_logs = Arc::clone(&logs);
    let subscriber = tracing_subscriber::fmt()
        .with_max_level(tracing::Level::TRACE)
        .with_writer(move || LogWriter(Arc::clone(&writer_logs)))
        .finish();
    let _guard = tracing::subscriber::set_default(subscriber);

    let client = ChatClient::new(
        transport(&fast()),
        settings(&base, json!({"streaming": false})),
    );
    let error = err(client
        .complete(
            &ChatRequest::new(vec![ChatMessage::User("q".to_owned())]),
            &StopSignal::default(),
        )
        .await);
    assert!(!error.message.contains(KEY), "{error}");
    assert!(error.message.contains("<redacted>"), "{error}");
    assert!(!format!("{error:?}").contains(KEY));
    assert!(!format!("{client:?}").contains(KEY));
    let embedder = embedder(&base, EmbeddingOptions::default());
    assert!(!format!("{embedder:?}").contains(KEY));
    let logged = logs
        .lock()
        .map(|l| String::from_utf8_lossy(&l).into_owned())
        .unwrap_or_default();
    assert!(
        logged.contains("retrying"),
        "the retries were logged: {logged}"
    );
    assert!(!logged.contains(KEY), "{logged}");
}

struct LogWriter(Arc<Mutex<Vec<u8>>>);

impl std::io::Write for LogWriter {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        if let Ok(mut logs) = self.0.lock() {
            logs.extend_from_slice(buf);
        }
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Days since 1970-01-01 → (year, month, day): Howard Hinnant's
/// `civil_from_days`, so the test certificate's validity is relative to
/// today without a date crate.
fn civil_from_days(days: i64) -> (i32, u8, u8) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    (
        i32::try_from(year).unwrap_or(2026),
        u8::try_from(month).unwrap_or(1),
        u8::try_from(day).unwrap_or(1),
    )
}

/// A CA and a `localhost` leaf it signed, valid from yesterday for 30 days
/// (macOS refuses leaves valid for more than 825 days).
fn test_pki() -> (String, String, Vec<u8>) {
    use rcgen::{
        BasicConstraints, CertificateParams, CertifiedIssuer, ExtendedKeyUsagePurpose, IsCa,
        KeyPair, KeyUsagePurpose, date_time_ymd,
    };
    let today = i64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_secs() / 86_400),
    )
    .unwrap_or(0);
    let (y0, m0, d0) = civil_from_days(today - 1);
    let (y1, m1, d1) = civil_from_days(today + 30);
    let validity = |params: &mut CertificateParams| {
        params.not_before = date_time_ymd(y0, m0, d0);
        params.not_after = date_time_ymd(y1, m1, d1);
    };
    let Ok(ca_key) = KeyPair::generate() else {
        panic!("ca key")
    };
    let Ok(mut ca_params) = CertificateParams::new(Vec::<String>::new()) else {
        panic!("ca params")
    };
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    ca_params
        .distinguished_name
        .push(rcgen::DnType::CommonName, "elitea model-client test CA");
    validity(&mut ca_params);
    let Ok(ca) = CertifiedIssuer::self_signed(ca_params, ca_key) else {
        panic!("ca cert")
    };
    let Ok(leaf_key) = KeyPair::generate() else {
        panic!("leaf key")
    };
    let Ok(mut leaf_params) = CertificateParams::new(vec!["localhost".to_owned()]) else {
        panic!("leaf params")
    };
    leaf_params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    leaf_params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
    validity(&mut leaf_params);
    let Ok(leaf) = leaf_params.signed_by(&leaf_key, &ca) else {
        panic!("leaf cert")
    };
    (ca.pem(), leaf.pem(), leaf_key.serialize_der())
}

struct TlsListener {
    tcp: TcpListener,
    acceptor: tokio_rustls::TlsAcceptor,
}

impl axum::serve::Listener for TlsListener {
    type Io = tokio_rustls::server::TlsStream<tokio::net::TcpStream>;
    type Addr = std::net::SocketAddr;

    async fn accept(&mut self) -> (Self::Io, Self::Addr) {
        loop {
            let Ok((stream, address)) = self.tcp.accept().await else {
                continue;
            };
            // A client that refuses the certificate fails its handshake;
            // keep serving the next one.
            if let Ok(tls) = self.acceptor.accept(stream).await {
                return (tls, address);
            }
        }
    }

    fn local_addr(&self) -> std::io::Result<Self::Addr> {
        self.tcp.local_addr()
    }
}

#[tokio::test]
async fn the_ca_file_is_trusted_and_its_absence_is_not() {
    use tokio_rustls::rustls;
    use tokio_rustls::rustls::pki_types::{
        CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, pem::PemObject,
    };

    let (ca_pem, leaf_pem, leaf_key) = test_pki();
    let Ok(leaf) = CertificateDer::from_pem_slice(leaf_pem.as_bytes()) else {
        panic!("leaf pem")
    };
    let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(leaf_key));
    let provider = Arc::new(rustls::crypto::aws_lc_rs::default_provider());
    let Ok(builder) =
        rustls::ServerConfig::builder_with_provider(provider).with_safe_default_protocol_versions()
    else {
        panic!("protocol versions")
    };
    let Ok(config) = builder
        .with_no_client_auth()
        .with_single_cert(vec![leaf], key)
    else {
        panic!("server config")
    };
    let Ok(tcp) = TcpListener::bind("127.0.0.1:0").await else {
        panic!("bind")
    };
    let Ok(address) = tcp.local_addr() else {
        panic!("address")
    };
    let gateway_state = Gateway {
        seen: Arc::new(Mutex::new(Vec::new())),
        script: Arc::new(|_, seen| embeddings_reply(seen, 3)),
    };
    let app = Router::new()
        .fallback(handle)
        .with_state(gateway_state.clone());
    let listener = TlsListener {
        tcp,
        acceptor: tokio_rustls::TlsAcceptor::from(Arc::new(config)),
    };
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    let base = format!("https://localhost:{}/llm/v1", address.port());

    let scratch =
        std::env::temp_dir().join(format!("elitea-model-client-ca-{}", std::process::id()));
    let _ = std::fs::create_dir_all(&scratch);
    let ca_file = scratch.join("ca.pem");
    assert!(
        std::fs::write(&ca_file, ca_pem).is_ok(),
        "cannot write the CA file"
    );

    let trusted = TransportSettings {
        ca_file: Some(ca_file.clone()),
        ..fast()
    };
    let client = EmbeddingClient::new(
        transport(&trusted),
        settings(&base, json!({"max_retries": 0})),
        "emb-model",
        EmbeddingOptions::default(),
    );
    let vectors = ok(client
        .embed_documents(&texts(2), &StopSignal::default())
        .await);
    assert_eq!(vectors.len(), 2);

    let untrusted = EmbeddingClient::new(
        transport(&fast()),
        settings(&base, json!({"max_retries": 0})),
        "emb-model",
        EmbeddingOptions::default(),
    );
    let error = err(untrusted
        .embed_documents(&texts(1), &StopSignal::default())
        .await);
    assert!(
        error.message.contains("cannot reach the model gateway"),
        "{error}"
    );
    assert_eq!(
        gateway_state.requests().len(),
        1,
        "the untrusted call sent nothing"
    );

    let missing = TransportSettings {
        ca_file: Some(scratch.join("absent.pem")),
        ca_file_setting: "TEST_TLS_CA_FILE",
        ..fast()
    };
    let error = err(Transport::new(&missing));
    assert!(error.message.contains("TEST_TLS_CA_FILE"), "{error}");
    assert_eq!(error.category(), "runtime_error");
    let _ = std::fs::remove_dir_all(&scratch);
}

/// A real round trip against the LAN vLLM, when asked for.
#[tokio::test]
async fn live_lan_model_chat() {
    if std::env::var("ELITEA_MODEL_CLIENT_LIVE_LLM")
        .ok()
        .as_deref()
        != Some("1")
    {
        return;
    }
    let base = std::env::var("ELITEA_MODEL_CLIENT_LIVE_LLM_BASE")
        .unwrap_or_else(|_| "http://192.168.29.60:8000/v1".to_owned());
    let model = std::env::var("ELITEA_MODEL_CLIENT_LIVE_LLM_MODEL")
        .unwrap_or_else(|_| "RadixArk/Qwen3.8-27B-NVFP4".to_owned());
    let settings = settings(&base, json!({"model_name": model, "max_tokens": 2048}));
    let live = TransportSettings::default();
    let client = ChatClient::new(transport(&live), settings);
    let mut request = ChatRequest::new(vec![
        ChatMessage::System("Answer in one short sentence. /no_think"),
        ChatMessage::User("What is the capital of France?".to_owned()),
    ]);
    let blocking = ok(client.complete(&request, &StopSignal::default()).await);
    eprintln!("live blocking: {blocking:?}");
    assert!(blocking.content.contains("Paris"), "{blocking:?}");

    let mut fragments = 0;
    let streamed = ok(client
        .stream(&request, &StopSignal::default(), &mut |_| fragments += 1)
        .await);
    eprintln!("live streamed ({fragments} fragments): {streamed:?}");
    assert!(streamed.content.contains("Paris"), "{streamed:?}");

    request.messages = vec![ChatMessage::User(
        "Call the get_weather tool for Paris. /no_think".to_owned(),
    )];
    request.tools = vec![ToolDefinition {
        name: "get_weather".to_owned(),
        description: "Current weather for a city".to_owned(),
        parameters: json!({"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}),
    }];
    request.tool_choice = Some(ToolChoice::Required);
    let tools = ok(client
        .stream(&request, &StopSignal::default(), &mut |_| {})
        .await);
    eprintln!("live tool call: {tools:?}");
    assert_eq!(
        tools.tool_calls.first().map(|c| c.name.as_str()),
        Some("get_weather"),
        "{tools:?}"
    );
}
