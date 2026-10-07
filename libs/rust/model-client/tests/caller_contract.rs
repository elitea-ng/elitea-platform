//! The `/llm` caller contract (`conformance/llm-caller/contract.json`): the
//! same fixture the agent worker's tests read. Each case runs through the
//! real client against a loopback gateway that answers with the fixture's
//! status and body, so what is checked is what a call does, not a helper.

use axum::Router;
use axum::body::{Body, Bytes};
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use elitea_engine_core::errors::ErrorType;
use elitea_engine_core::stream::StopSignal;
use elitea_model_client::{
    ChatClient, ChatMessage, ChatRequest, ModelSettings, Transport, TransportSettings,
    refusal_code, retryable, valid_execution_id,
};
use serde_json::{Value, json};
use std::sync::{Arc, Mutex};

const CONTRACT: &str = include_str!("../../../../conformance/llm-caller/contract.json");
const TOKEN: &str = "callback-bearer";
const PROJECT: &str = "42";
const EXECUTION: &str = "callback-6f1c2d4e-8a7b-4c3d-9e2f-1a2b3c4d5e6f";

fn contract() -> Value {
    serde_json::from_str(CONTRACT).unwrap_or_else(|e| panic!("contract.json: {e}"))
}

/// One scripted answer, and the headers of every request that reached it.
#[derive(Clone)]
struct Gateway {
    status: u16,
    body: String,
    content_type: &'static str,
    seen: Arc<Mutex<Vec<HeaderMap>>>,
}

async fn answer(State(gateway): State<Gateway>, headers: HeaderMap) -> Response {
    gateway
        .seen
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .push(headers);
    let status = StatusCode::from_u16(gateway.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    (
        status,
        [("content-type", gateway.content_type)],
        Body::from(Bytes::from(gateway.body)),
    )
        .into_response()
}

async fn serve(gateway: Gateway) -> String {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap_or_else(|e| panic!("{e}"));
    let address = listener.local_addr().unwrap_or_else(|e| panic!("{e}"));
    let app = Router::new()
        .route("/llm/v1/chat/completions", axum::routing::post(answer))
        .with_state(gateway);
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    format!("http://{address}/llm/v1")
}

fn client(base: &str) -> ChatClient {
    let settings = ModelSettings::from_llm_settings(&json!({
        "api_base": base,
        "api_key": TOKEN,
        "organization": PROJECT,
        "execution_id": EXECUTION,
        "model_name": "m",
        "max_retries": 0,
    }))
    .unwrap_or_else(|e| panic!("{e}"));
    let transport = Transport::new(&TransportSettings::default()).unwrap_or_else(|e| panic!("{e}"));
    ChatClient::new(transport, settings)
}

fn request() -> ChatRequest {
    ChatRequest::new(vec![ChatMessage::User("q".to_owned())])
}

fn sse(events: &[Value]) -> String {
    let mut text = String::new();
    for event in events {
        text.push_str("data: ");
        text.push_str(&event.to_string());
        text.push_str("\n\n");
    }
    text.push_str("data: [DONE]\n\n");
    text
}

#[tokio::test]
async fn a_call_sends_the_contract_headers_and_reads_reasoning_apart_from_the_answer() {
    let contract = contract();
    let events = contract["reasoning"]["stream"]
        .as_array()
        .cloned()
        .unwrap_or_default();
    let seen = Arc::new(Mutex::new(Vec::new()));
    let base = serve(Gateway {
        status: 200,
        body: sse(&events),
        content_type: "text/event-stream",
        seen: Arc::clone(&seen),
    })
    .await;
    let mut streamed = String::new();
    let answer = client(&base)
        .stream(&request(), &StopSignal::default(), &mut |text| {
            streamed.push_str(text);
        })
        .await
        .unwrap_or_else(|e| panic!("{e}"));

    let expected = &contract["reasoning"]["expected"];
    assert_eq!(
        answer.content,
        expected["content"].as_str().unwrap_or_default()
    );
    assert_eq!(
        answer.reasoning,
        expected["reasoning"].as_str().unwrap_or_default()
    );
    assert_eq!(
        streamed, answer.content,
        "reasoning is never streamed as answer text"
    );
    let usage = answer.usage.unwrap_or_default();
    assert_eq!(
        Some(usage.reasoning_tokens),
        expected["reasoning_tokens"].as_u64()
    );
    assert_eq!(
        Some(usage.cached_tokens),
        expected["cached_tokens"].as_u64()
    );

    let headers = seen
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    let headers = headers
        .first()
        .unwrap_or_else(|| panic!("no request reached the gateway"));
    for (name, template) in contract["request"]["headers"]
        .as_object()
        .into_iter()
        .flatten()
    {
        let want = template
            .as_str()
            .unwrap_or_default()
            .replace("{token}", TOKEN)
            .replace("{project_id}", PROJECT)
            .replace("{execution_id}", EXECUTION);
        assert_eq!(
            headers.get(name.as_str()).and_then(|v| v.to_str().ok()),
            Some(want.as_str()),
            "{name}"
        );
    }
    for name in contract["request"]["headers_absent"]
        .as_array()
        .into_iter()
        .flatten()
    {
        let name = name.as_str().unwrap_or_default();
        assert!(headers.get(name).is_none(), "{name} must not be sent");
    }
}

#[tokio::test]
async fn every_refusal_reads_as_the_worker_reads_it() {
    let contract = contract();
    for case in contract["refusals"].as_array().into_iter().flatten() {
        let name = case["name"].as_str().unwrap_or_default();
        let status = u16::try_from(case["status"].as_u64().unwrap_or(0)).unwrap_or(0);
        let body = case["body"].to_string();
        assert_eq!(
            refusal_code(status, body.as_bytes()),
            case["code"],
            "{name}"
        );
        assert_eq!(
            Some(retryable(status)),
            case["retryable"].as_bool(),
            "{name}"
        );

        let base = serve(Gateway {
            status,
            body: body.clone(),
            content_type: "application/json",
            seen: Arc::default(),
        })
        .await;
        let error = client(&base)
            .complete(&request(), &StopSignal::default())
            .await
            .err()
            .unwrap_or_else(|| panic!("{name}: a refusal must fail"));
        if status == 402 {
            assert_eq!(error.error_type, ErrorType::Value, "{name}");
            assert_eq!(error.category(), "invalid_input", "{name}");
            let scope = match case["code"].as_str().unwrap_or_default() {
                "model_gateway.member_budget_exhausted" => "member model budget",
                "model_gateway.project_budget_exhausted" => "project model budget",
                _ => "The model budget",
            };
            assert!(error.message.contains(scope), "{name}: {}", error.message);
        }
    }
}

#[test]
fn execution_ids_follow_the_edges_shape_rule() {
    let contract = contract();
    let rule = &contract["request"]["execution_id"];
    for id in rule["valid"].as_array().into_iter().flatten() {
        assert!(valid_execution_id(id.as_str().unwrap_or_default()), "{id}");
    }
    for id in rule["invalid"].as_array().into_iter().flatten() {
        assert!(!valid_execution_id(id.as_str().unwrap_or_default()), "{id}");
    }
    let max = usize::try_from(rule["max_bytes"].as_u64().unwrap_or(0)).unwrap_or(0);
    assert!(valid_execution_id(&"a".repeat(max)));
    assert!(!valid_execution_id(&"a".repeat(max + 1)));
}
