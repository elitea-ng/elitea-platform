//! Contract tests for the OpenAI-compatible facade and shared Elitea gateway transport.

use std::convert::Infallible;
use std::time::Duration;

use adk_rust::{
    Content, ErrorCategory, FunctionResponseData, GenerateContentConfig, LlmRequest, Part,
};
use bytes::Bytes;
use http::header::{AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderValue, Response, StatusCode, Version};
use http_body::Frame;
use http_body_util::{Full, StreamBody};
use tokio_stream::StreamExt as _;
use tonic::body::Body;

use super::openai_compatible_facade::{
    CapturedModelRequest, ModelFacadeError, ModelReasoningEffort, TestModelGatewayOutcome,
    test_model_facade_invocation, test_model_gateway_client, test_model_gateway_config,
    test_model_gateway_response, test_model_request,
};
use super::runtime_context::ClaimScopedEliteaContext;

const TOKEN: &str = "ephemeral-model-fixture-token";

fn stream_body(chunks: Vec<Bytes>) -> Body {
    let frames = chunks
        .into_iter()
        .map(|chunk| Ok::<_, Infallible>(Frame::data(chunk)));
    Body::new(StreamBody::new(tokio_stream::iter(frames)))
}

fn ordinary_sse() -> Vec<u8> {
    concat!(
        "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"check\"},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"content\":\"lo 🌍\"},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    )
    .as_bytes()
    .to_vec()
}

fn tool_request(contents: Vec<Content>) -> LlmRequest {
    let mut tools = std::collections::HashMap::new();
    tools.insert(
        "double".to_owned(),
        serde_json::json!({
            "description": "Double one integer.",
            "parameters": {
                "type": "object",
                "properties": {"value": {"type": "integer"}},
                "required": ["value"]
            }
        }),
    );
    LlmRequest {
        model: "fixture-model".to_owned(),
        contents,
        config: Some(GenerateContentConfig {
            max_output_tokens: Some(4_000),
            ..GenerateContentConfig::default()
        }),
        tools,
        previous_response_id: None,
    }
}

async fn drain(
    mut stream: adk_rust::LlmResponseStream,
) -> Result<Vec<adk_rust::LlmResponse>, adk_rust::AdkError> {
    let mut responses = Vec::new();
    while let Some(response) = stream.next().await {
        responses.push(response?);
    }
    Ok(responses)
}

fn assert_exact_captured_request(request: &CapturedModelRequest) {
    assert_eq!(request.method, http::Method::POST);
    assert_eq!(request.uri.path(), "/llm/v1/chat/completions");
    assert_eq!(request.version, Version::HTTP_2);
    assert_eq!(
        request.headers.get(CONTENT_TYPE),
        Some(&HeaderValue::from_static("application/json"))
    );
    let authorization = request
        .headers
        .get(AUTHORIZATION)
        .expect("authorization header");
    assert!(authorization.is_sensitive());
    assert_eq!(
        authorization.to_str().expect("authorization text"),
        format!("Bearer {TOKEN}")
    );
    assert_eq!(
        request
            .headers
            .get("x-project-id")
            .expect("billing project header"),
        "17"
    );
    assert_eq!(
        request
            .headers
            .get(CONTENT_LENGTH)
            .expect("request content length")
            .to_str()
            .expect("request content length text"),
        request.body.len().to_string()
    );
    let body: serde_json::Value =
        serde_json::from_slice(&request.body).expect("model request JSON");
    assert_eq!(body["model"], "fixture-model");
    assert_eq!(body["messages"][0]["role"], "system");
    assert_eq!(
        body["messages"][0]["content"],
        "review carefully\nbe concise"
    );
    assert_eq!(body["messages"][1]["role"], "user");
    assert_eq!(body["messages"][1]["content"], "explain this");
    assert_eq!(body["max_completion_tokens"], 4_000);
    assert_eq!(body["reasoning_effort"], "medium");
    assert_eq!(body["stream"], true);
    assert_eq!(body["stream_options"]["include_usage"], true);
    assert!(body.get("model_project_id").is_none());
    assert!(body.get("temperature").is_none());
}

#[tokio::test(flavor = "current_thread")]
async fn exact_sdk_request_and_fragmented_sse_are_preserved() {
    let raw = ordinary_sse();
    let emoji = raw
        .windows("🌍".len())
        .position(|window| window == "🌍".as_bytes())
        .expect("emoji in fixture");
    let chunks = vec![
        Bytes::copy_from_slice(&raw[..emoji + 2]),
        Bytes::copy_from_slice(&raw[emoji + 2..]),
    ];
    let mut config = test_model_gateway_config();
    config.max_sse_event_bytes = 512;
    config.max_stream_bytes = 4_096;
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(stream_body(chunks)),
        )],
        config,
    )
    .expect("model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            23,
            test_model_facade_invocation(),
        )
        .expect("bound model");

    let responses = drain(
        bound
            .generate_for_test(test_model_request("explain this"))
            .await
            .expect("model response stream"),
    )
    .await
    .expect("valid SSE");

    assert_eq!(responses.len(), 4);
    assert!(!responses[0].turn_complete);
    assert!(!responses[1].turn_complete);
    assert!(!responses[2].turn_complete);
    assert!(responses[3].turn_complete);
    assert_eq!(
        responses[3].finish_reason,
        Some(adk_rust::FinishReason::Stop)
    );
    assert!(responses[3].content.is_none());
    assert!(matches!(
        responses[1]
            .content
            .as_ref()
            .and_then(|content| content.parts.first()),
        Some(Part::Thinking { thinking, .. }) if thinking == "check"
    ));
    assert_eq!(
        bound.take_completion_for_test().expect("exact completion"),
        "Hello 🌍"
    );

    let captured = captured.lock().expect("captured model request");
    let [request] = captured.as_slice() else {
        panic!("exactly one model request expected")
    };
    assert_exact_captured_request(request);
}

#[tokio::test(flavor = "current_thread")]
async fn automatic_max_tokens_omits_the_openai_wire_limit() {
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(Body::new(Full::new(Bytes::from(ordinary_sse())))),
        )],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let mut invocation = test_model_facade_invocation();
    invocation.max_tokens = None;
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .expect("bound automatic-limit model");
    let mut request = test_model_request("explain this");
    request
        .config
        .as_mut()
        .expect("generation config")
        .max_output_tokens = None;

    drain(
        bound
            .generate_for_test(request)
            .await
            .expect("automatic-limit model stream"),
    )
    .await
    .expect("valid automatic-limit response");

    let captured = captured.lock().expect("captured requests");
    let body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("model request JSON");
    assert!(body.get("max_completion_tokens").is_none());
}

#[tokio::test(flavor = "current_thread")]
async fn empty_system_instruction_is_omitted_from_openai_messages() {
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(Body::new(Full::new(Bytes::from(ordinary_sse())))),
        )],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let mut invocation = test_model_facade_invocation();
    invocation.system_instruction.clear();
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .expect("bound model without a system instruction");

    drain(
        bound
            .generate_for_test(test_model_request("explain this"))
            .await
            .expect("model response stream"),
    )
    .await
    .expect("valid SSE");

    let captured = captured.lock().expect("captured requests");
    let body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("model request JSON");
    assert_eq!(body["messages"].as_array().map(Vec::len), Some(1));
    assert_eq!(body["messages"][0]["role"], "user");
    assert_eq!(body["messages"][0]["content"], "explain this");
}

#[tokio::test(flavor = "current_thread")]
async fn tool_declarations_calls_and_results_round_trip_across_model_turns() {
    tool_history_round_trip(false).await;
}

#[tokio::test(flavor = "current_thread")]
async fn retired_tool_history_does_not_admit_new_calls() {
    tool_history_round_trip(true).await;
}

#[allow(clippy::too_many_lines)] // Keep the history transition and rejected new call in one proof.
async fn tool_history_round_trip(retire_tool: bool) {
    let tool_sse = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"double\",\"arguments\":\"{\\\"value\\\":\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"21}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n",
        "data: [DONE]\n\n",
    );
    let (client, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
                Bytes::from(tool_sse),
            )))),
            TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
                Bytes::from(ordinary_sse()),
            )))),
            TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
                Bytes::from(tool_sse),
            )))),
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");

    let user = Content::new("user").with_text("double 21");
    let first = drain(
        bound
            .generate_for_test(tool_request(vec![user.clone()]))
            .await
            .expect("tool-call stream"),
    )
    .await
    .expect("tool-call response");
    let call = first
        .last()
        .and_then(|response| response.content.as_ref())
        .and_then(|content| content.parts.first())
        .cloned()
        .expect("function call");
    assert!(!first.last().expect("tool terminal response").turn_complete);
    assert!(matches!(
        &call,
        Part::FunctionCall { name, args, id, .. }
            if name == "double" && args["value"] == 21 && id.as_deref() == Some("call_1")
    ));

    let result = Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: FunctionResponseData::new(
                "double",
                serde_json::json!({"value": 42}),
            ),
            id: Some("call_1".to_owned()),
            annotations: None,
        }],
    };
    let mut resumed = tool_request(vec![
        user,
        Content {
            role: "model".to_owned(),
            parts: vec![call],
        },
        result,
    ]);
    if retire_tool {
        resumed.tools.clear();
    }
    drain(
        bound
            .generate_for_test(resumed)
            .await
            .expect("final stream"),
    )
    .await
    .expect("final response");
    assert_eq!(
        bound.take_completion_for_test().expect("final completion"),
        "Hello 🌍"
    );

    // Historical authority never permits a new call to a removed operation.
    if retire_tool {
        let bound = client
            .bind_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                test_model_facade_invocation(),
            )
            .unwrap();
        let mut unbound = tool_request(vec![Content::new("user").with_text("try again")]);
        unbound.tools.clear();
        let error = drain(bound.generate_for_test(unbound).await.unwrap())
            .await
            .expect_err("a retired tool is never emitted");
        assert_eq!(error.code, super::model_facade::TOOL_NOT_ADMITTED_CODE);
    }

    let captured = captured.lock().expect("captured requests");
    assert_eq!(captured.len(), if retire_tool { 3 } else { 2 });
    let first_body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("first body");
    assert_eq!(first_body["tools"][0]["function"]["name"], "double");
    assert_eq!(first_body["tool_choice"], "auto");
    let second_body: serde_json::Value =
        serde_json::from_slice(&captured[1].body).expect("second body");
    assert_eq!(second_body.get("tools").is_none(), retire_tool);
    assert_eq!(second_body["messages"][2]["role"], "assistant");
    assert_eq!(second_body["messages"][2]["tool_calls"][0]["id"], "call_1");
    assert_eq!(second_body["messages"][3]["role"], "tool");
    assert_eq!(second_body["messages"][3]["tool_call_id"], "call_1");
    assert_eq!(second_body["messages"][3]["content"], "{\"value\":42}");
}

#[tokio::test(flavor = "current_thread")]
async fn gpt_56_tools_default_to_no_reasoning_when_main_omits_an_effort() {
    let tool_sse = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"double\",\"arguments\":\"{\\\"value\\\":21}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: [DONE]\n\n",
    );
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(Body::new(Full::new(Bytes::from(tool_sse)))),
        )],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let mut invocation = test_model_facade_invocation();
    invocation.model_name = "gpt-5.6-terra".to_owned();
    invocation.reasoning_effort = None;
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .expect("bound GPT-5.6 model");
    let mut request = tool_request(vec![Content::new("user").with_text("double 21")]);
    request.model = "gpt-5.6-terra".to_owned();

    drain(
        bound
            .generate_for_test(request)
            .await
            .expect("tool-call stream"),
    )
    .await
    .expect("tool-call response");

    let captured = captured.lock().expect("captured requests");
    let body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("model request JSON");
    assert_eq!(body["reasoning_effort"], "none");
}

#[tokio::test(flavor = "current_thread")]
async fn model_credential_and_completion_are_single_use() {
    let response = test_model_gateway_response(Body::new(Full::new(Bytes::from(ordinary_sse()))));
    let (client, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(response),
            TestModelGatewayOutcome::Unavailable,
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let mut invocation = test_model_facade_invocation();
    invocation.max_model_turns = 1;
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .expect("bound model");
    drain(
        bound
            .generate_for_test(test_model_request("first"))
            .await
            .expect("first stream"),
    )
    .await
    .expect("first completion");

    let Err(error) = bound.generate_for_test(test_model_request("second")).await else {
        panic!("one claim credential cannot make a second model call")
    };
    assert_eq!(error.code, "model_gateway.turn_limit");
    assert_eq!(error.category, ErrorCategory::InvalidInput);
    assert_eq!(captured.lock().expect("captured requests").len(), 1);
    assert_eq!(
        bound.take_completion_for_test().expect("completion once"),
        "Hello 🌍"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn request_profile_and_local_bounds_fail_before_network() {
    for mutation in 0..5 {
        let (client, captured) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Unavailable],
            test_model_gateway_config(),
        )
        .expect("model gateway client");
        let bound = client
            .bind_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                test_model_facade_invocation(),
            )
            .expect("bound model");
        let mut request = test_model_request("valid");
        match mutation {
            0 => request.model = "other-model".to_owned(),
            1 => {
                request.tools.insert(
                    "forbidden".to_owned(),
                    serde_json::json!({"description": 7}),
                );
            }
            2 => request
                .contents
                .push(adk_rust::Content::new("system").with_text("forbidden")),
            3 => request.config.as_mut().expect("generation config").top_p = Some(0.9),
            4 => request.contents[0] = adk_rust::Content::new("user").with_text(""),
            _ => unreachable!("bounded mutation corpus"),
        }
        let Err(error) = bound.generate_for_test(request).await else {
            panic!("outside-profile request must fail")
        };
        assert_eq!(error.code, "model_gateway.invalid_request");
        assert!(captured.lock().expect("captured requests").is_empty());
    }

    let mut config = test_model_gateway_config();
    config.max_request_bytes = 128;
    let (client, captured) =
        test_model_gateway_client(vec![TestModelGatewayOutcome::Unavailable], config)
            .expect("bounded model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let Err(error) = bound
        .generate_for_test(test_model_request(&"x".repeat(256)))
        .await
    else {
        panic!("oversized request must fail")
    };
    assert_eq!(error.code, "model_gateway.request_too_large");
    assert!(captured.lock().expect("captured requests").is_empty());
}

#[tokio::test(flavor = "current_thread")]
async fn status_metadata_and_transport_errors_are_stable_and_secret_safe() {
    for (status, expected_category, expected_code) in [
        (
            StatusCode::UNAUTHORIZED,
            ErrorCategory::Unauthorized,
            "model_gateway.unauthorized",
        ),
        (
            StatusCode::FORBIDDEN,
            ErrorCategory::Forbidden,
            "model_gateway.forbidden",
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            ErrorCategory::RateLimited,
            "model_gateway.rate_limited",
        ),
        (
            StatusCode::PAYMENT_REQUIRED,
            ErrorCategory::InvalidInput,
            "model_gateway.budget_exhausted",
        ),
        (
            StatusCode::CONFLICT,
            ErrorCategory::Unavailable,
            "model_gateway.conflict",
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            ErrorCategory::Unavailable,
            "model_gateway.unavailable",
        ),
        (
            StatusCode::BAD_REQUEST,
            ErrorCategory::InvalidInput,
            "model_gateway.rejected",
        ),
    ] {
        let response = Response::builder()
            .status(status)
            .version(Version::HTTP_2)
            .header(CONTENT_TYPE, "application/json")
            .body(Body::new(Full::new(Bytes::from_static(
                b"secret upstream body",
            ))))
            .expect("status response");
        let (client, _) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(response)],
            test_model_gateway_config(),
        )
        .expect("model gateway client");
        let bound = client
            .bind_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                test_model_facade_invocation(),
            )
            .expect("bound model");
        let Err(error) = bound.generate_for_test(test_model_request("request")).await else {
            panic!("status must fail")
        };
        assert_eq!(error.category, expected_category);
        assert_eq!(error.code, expected_code);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("secret upstream body"));
        assert!(!diagnostic.contains(TOKEN));
    }

    let (client, _) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Unavailable],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let Err(error) = bound.generate_for_test(test_model_request("request")).await else {
        panic!("transport failure expected")
    };
    assert_eq!(error.code, "model_gateway.transport");
    assert!(error.retry.should_retry);
}

#[tokio::test(flavor = "current_thread")]
async fn response_metadata_and_header_timeout_are_dependency_failures() {
    let raw = ordinary_sse();
    for mutation in 0..4 {
        let mut response =
            test_model_gateway_response(Body::new(Full::new(Bytes::from(raw.clone()))));
        match mutation {
            0 => *response.version_mut() = Version::HTTP_11,
            1 => *response.status_mut() = StatusCode::FOUND,
            2 => {
                response.headers_mut().remove(CONTENT_TYPE);
            }
            3 => {
                response
                    .headers_mut()
                    .append(CONTENT_TYPE, HeaderValue::from_static("text/event-stream"));
            }
            _ => unreachable!("bounded response metadata corpus"),
        }
        let (client, _) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(response)],
            test_model_gateway_config(),
        )
        .expect("model gateway client");
        let bound = client
            .bind_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                test_model_facade_invocation(),
            )
            .expect("bound model");
        let Err(error) = bound.generate_for_test(test_model_request("request")).await else {
            panic!("invalid response metadata must fail")
        };
        assert!(matches!(error.category, ErrorCategory::Unavailable));
        assert!(error.retry.should_retry);
    }

    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_millis(1);
    let (client, _) = test_model_gateway_client(vec![TestModelGatewayOutcome::Pending], config)
        .expect("model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let Err(error) = bound.generate_for_test(test_model_request("request")).await else {
        panic!("response header timeout expected")
    };
    assert_eq!(error.code, "model_gateway.response_header_timeout");
    assert_eq!(error.category, ErrorCategory::Timeout);
}

#[tokio::test(flavor = "current_thread")]
async fn sse_protocol_resource_and_tool_shapes_fail_closed() {
    for (raw, expected_code) in [
        (
            "data: [DONE]\n\n".to_owned(),
            "model_gateway.done_before_completion",
        ),
        (
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{}]},\"finish_reason\":null}]}\n\n"
                .to_owned(),
            "model_gateway.invalid_sse",
        ),
        (
            "data: {not-json}\n\n".to_owned(),
            "model_gateway.invalid_sse",
        ),
    ] {
        let (client, _) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(
                test_model_gateway_response(Body::new(Full::new(Bytes::from(raw)))),
            )],
            test_model_gateway_config(),
        )
        .expect("model gateway client");
        let bound = client
            .bind_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                test_model_facade_invocation(),
            )
            .expect("bound model");
        let stream = bound
            .generate_for_test(test_model_request("request"))
            .await
            .expect("response head");
        let error = drain(stream).await.expect_err("invalid SSE");
        assert_eq!(error.code, expected_code);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn sse_completion_event_count_and_stream_size_are_bounded() {
    let large = "x".repeat(60 * 1_024 + 1);
    let raw = format!(
        "data: {{\"choices\":[{{\"delta\":{{\"content\":{}}},\"finish_reason\":\"stop\"}}]}}\n\ndata: [DONE]\n\n",
        serde_json::to_string(&large).expect("large text JSON")
    );
    let mut config = test_model_gateway_config();
    config.max_sse_event_bytes = 128 * 1_024;
    let (client, _) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(Body::new(Full::new(Bytes::from(raw)))),
        )],
        config,
    )
    .expect("model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let stream = bound
        .generate_for_test(test_model_request("request"))
        .await
        .expect("response head");
    assert_eq!(
        drain(stream).await.expect_err("completion bound").code,
        "model_gateway.output_too_large"
    );

    let mut config = test_model_gateway_config();
    config.max_sse_events = 1;
    let (client, _) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(Body::new(Full::new(Bytes::from(ordinary_sse())))),
        )],
        config,
    )
    .expect("event-count client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let stream = bound
        .generate_for_test(test_model_request("request"))
        .await
        .expect("response head");
    assert_eq!(
        drain(stream).await.expect_err("event count bound").code,
        "model_gateway.too_many_events"
    );

    let mut config = test_model_gateway_config();
    config.max_sse_event_bytes = 64;
    config.max_stream_bytes = 128;
    let (client, _) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(Body::new(Full::new(Bytes::from(ordinary_sse())))),
        )],
        config,
    )
    .expect("stream-size client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let stream = bound
        .generate_for_test(test_model_request("request"))
        .await
        .expect("response head");
    assert_eq!(
        drain(stream).await.expect_err("stream size bound").code,
        "model_gateway.stream_too_large"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn idle_stream_and_trailers_fail_without_exposing_payloads() {
    let pending = tokio_stream::pending::<Result<Frame<Bytes>, Infallible>>();
    let response = test_model_gateway_response(Body::new(StreamBody::new(pending)));
    let mut config = test_model_gateway_config();
    config.stream_idle_timeout = Duration::from_millis(1);
    let (client, _) =
        test_model_gateway_client(vec![TestModelGatewayOutcome::Response(response)], config)
            .expect("model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let stream = bound
        .generate_for_test(test_model_request("request"))
        .await
        .expect("response head");
    let error = drain(stream).await.expect_err("idle stream timeout");
    assert_eq!(error.code, "model_gateway.stream_idle_timeout");

    let final_event = Bytes::from_static(
        b"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
    );
    let frames = tokio_stream::iter(vec![
        Ok::<_, Infallible>(Frame::data(final_event)),
        Ok(Frame::trailers(http::HeaderMap::new())),
    ]);
    let response = test_model_gateway_response(Body::new(StreamBody::new(frames)));
    let (client, _) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(response)],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            test_model_facade_invocation(),
        )
        .expect("bound model");
    let stream = bound
        .generate_for_test(test_model_request("request"))
        .await
        .expect("response head");
    let error = drain(stream).await.expect_err("trailers are forbidden");
    assert_eq!(error.code, "model_gateway.stream_trailers");
    assert!(!format!("{error:?} {error}").contains(TOKEN));
}

#[tokio::test(flavor = "current_thread")]
async fn o_series_role_and_configuration_boundaries_are_explicit() {
    let response = test_model_gateway_response(Body::new(Full::new(Bytes::from(ordinary_sse()))));
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(response)],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let mut invocation = test_model_facade_invocation();
    invocation.model_name = "o3-mini".to_owned();
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .expect("o-series model");
    let mut request = test_model_request("request");
    request.model = "o3-mini".to_owned();
    drain(
        bound
            .generate_for_test(request)
            .await
            .expect("model stream"),
    )
    .await
    .expect("model response");
    let body: serde_json::Value = {
        let captured = captured.lock().expect("captured requests");
        serde_json::from_slice(&captured[0].body).expect("model request JSON")
    };
    assert_eq!(body["messages"][0]["role"], "developer");

    let response = test_model_gateway_response(Body::new(Full::new(Bytes::from(ordinary_sse()))));
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(response)],
        test_model_gateway_config(),
    )
    .expect("disabled-reasoning model gateway client");
    let mut disabled_reasoning = test_model_facade_invocation();
    disabled_reasoning.reasoning_effort = Some(ModelReasoningEffort::None);
    disabled_reasoning.temperature = Some(0.2);
    let bound = client
        .bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            disabled_reasoning,
        )
        .expect("disabled reasoning with temperature");
    let mut request = test_model_request("request");
    request
        .config
        .as_mut()
        .expect("generation config")
        .temperature = Some(0.2);
    drain(
        bound
            .generate_for_test(request)
            .await
            .expect("disabled-reasoning model stream"),
    )
    .await
    .expect("disabled-reasoning model response");
    let captured = captured.lock().expect("captured requests");
    let body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("model request JSON");
    assert_eq!(body["reasoning_effort"], "none");
    assert_eq!(body["temperature"], 0.2);

    let mut invalid_invocation = test_model_facade_invocation();
    invalid_invocation.temperature = Some(0.2);
    assert!(matches!(
        client.bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invalid_invocation,
        ),
        Err(ModelFacadeError::InvalidInvocation)
    ));
    assert!(matches!(
        client.bind_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            0,
            test_model_facade_invocation(),
        ),
        Err(ModelFacadeError::InvalidInvocation)
    ));

    for mutation in 0..4 {
        let mut config = test_model_gateway_config();
        match mutation {
            0 => config.origin = "http://platform.internal".to_owned(),
            1 => config.response_header_timeout = Duration::from_micros(500),
            2 => config.stream_idle_timeout = Duration::from_secs(301),
            3 => config.max_sse_events = 0,
            _ => unreachable!("bounded configuration corpus"),
        }
        assert!(matches!(
            test_model_gateway_client(Vec::new(), config),
            Err(ModelFacadeError::InvalidConfiguration)
        ));
    }
}
