//! Contract tests for the native Anthropic adapter on the Elitea channel.

use adk_rust::{
    Content, ErrorCategory, FunctionResponseData, GenerateContentConfig, LlmRequest, Part,
};
use bytes::Bytes;
use http::header::{AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderValue, Version};
use http_body_util::Full;
use tokio_stream::StreamExt as _;
use tonic::body::Body;

use super::openai_compatible_facade::{
    ModelFacadeInvocation, ModelReasoningEffort, TestModelGatewayOutcome,
    test_model_gateway_client, test_model_gateway_config, test_model_gateway_response,
};
use super::runtime_context::ClaimScopedEliteaContext;

const TOKEN: &str = "ephemeral-anthropic-fixture-token";
const MODEL: &str = "claude-sonnet-4-5";

fn invocation(model: &str, effort: Option<ModelReasoningEffort>) -> ModelFacadeInvocation {
    ModelFacadeInvocation {
        model_name: model.to_owned(),
        system_instruction: "review carefully\nbe concise".to_owned(),
        max_tokens: Some(4_000),
        reasoning_effort: effort,
        temperature: effort.is_none().then_some(0.7),
        max_model_turns: 25,
    }
}

#[test]
fn native_anthropic_rejects_an_unresolved_automatic_limit() {
    let (client, _) = test_model_gateway_client(Vec::new(), test_model_gateway_config())
        .expect("model gateway client");
    let mut settings = invocation(MODEL, None);
    settings.max_tokens = None;
    assert!(matches!(
        client
            .bind_anthropic_ordinary(&ClaimScopedEliteaContext::fixture(17, TOKEN), 17, settings,),
        Err(super::openai_compatible_facade::ModelFacadeError::InvalidInvocation)
    ));
}

fn request(model: &str, temperature: Option<f32>) -> LlmRequest {
    LlmRequest {
        model: model.to_owned(),
        contents: vec![adk_rust::Content::new("user").with_text("explain this")],
        config: Some(GenerateContentConfig {
            max_output_tokens: Some(4_000),
            temperature,
            ..GenerateContentConfig::default()
        }),
        tools: std::collections::HashMap::new(),
        previous_response_id: None,
    }
}

fn native_sse(model: &str) -> String {
    format!(
        concat!(
            "event: message_start\n",
            "data: {{\"type\":\"message_start\",\"message\":{{\"id\":\"msg_1\",\"content\":[],\"model\":\"{}\",\"role\":\"assistant\",\"type\":\"message\",\"usage\":{{\"input_tokens\":5,\"output_tokens\":0,\"cache_creation_input_tokens\":3,\"cache_read_input_tokens\":2}}}}}}\n\n",
            "event: content_block_start\n",
            "data: {{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}}}\n\n",
            "event: content_block_delta\n",
            "data: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"thinking_delta\",\"thinking\":\"check\"}}}}\n\n",
            "event: content_block_delta\n",
            "data: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"signature_delta\",\"signature\":\"opaque\"}}}}\n\n",
            "event: content_block_stop\n",
            "data: {{\"type\":\"content_block_stop\",\"index\":0}}\n\n",
            "event: content_block_start\n",
            "data: {{\"type\":\"content_block_start\",\"index\":1,\"content_block\":{{\"type\":\"text\",\"text\":\"\"}}}}\n\n",
            "event: content_block_delta\n",
            "data: {{\"type\":\"content_block_delta\",\"index\":1,\"delta\":{{\"type\":\"text_delta\",\"text\":\"native \"}}}}\n\n",
            "event: content_block_delta\n",
            "data: {{\"type\":\"content_block_delta\",\"index\":1,\"delta\":{{\"type\":\"text_delta\",\"text\":\"response\"}}}}\n\n",
            "event: content_block_stop\n",
            "data: {{\"type\":\"content_block_stop\",\"index\":1}}\n\n",
            "event: message_delta\n",
            "data: {{\"type\":\"message_delta\",\"delta\":{{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}},\"usage\":{{\"output_tokens\":4}}}}\n\n",
            "event: message_stop\n",
            "data: {{\"type\":\"message_stop\"}}\n\n",
        ),
        model
    )
}

fn tool_sse(model: &str) -> String {
    format!(
        concat!(
            "event: message_start\n",
            "data: {{\"type\":\"message_start\",\"message\":{{\"id\":\"msg_tool\",\"content\":[],\"model\":\"{}\",\"role\":\"assistant\",\"type\":\"message\",\"usage\":{{\"input_tokens\":5,\"output_tokens\":0}}}}}}\n\n",
            "event: content_block_start\n",
            "data: {{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{{\"type\":\"tool_use\",\"id\":\"tool_1\",\"name\":\"double\",\"input\":{{}}}}}}\n\n",
            "event: content_block_delta\n",
            "data: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"input_json_delta\",\"partial_json\":\"{{\\\"value\\\":21}}\"}}}}\n\n",
            "event: content_block_stop\n",
            "data: {{\"type\":\"content_block_stop\",\"index\":0}}\n\n",
            "event: message_delta\n",
            "data: {{\"type\":\"message_delta\",\"delta\":{{\"stop_reason\":\"tool_use\",\"stop_sequence\":null}},\"usage\":{{\"output_tokens\":2}}}}\n\n",
            "event: message_stop\n",
            "data: {{\"type\":\"message_stop\"}}\n\n",
        ),
        model
    )
}

fn tool_request(contents: Vec<Content>) -> LlmRequest {
    let mut value = request(MODEL, Some(0.7));
    value.contents = contents;
    value.tools.insert(
        "double".to_owned(),
        serde_json::json!({
            "name": "double",
            "description": "Double one integer.",
            "parameters": {
                "type": "object",
                "properties": {"value": {"type": "integer"}},
                "required": ["value"]
            }
        }),
    );
    value
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

#[tokio::test(flavor = "current_thread")]
async fn native_messages_request_preserves_cache_thinking_identity_and_completion() {
    let response =
        test_model_gateway_response(Body::new(Full::new(Bytes::from(native_sse(MODEL)))));
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(response)],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let bound = client
        .bind_anthropic_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            23,
            invocation(MODEL, Some(ModelReasoningEffort::Medium)),
        )
        .expect("native Anthropic model");
    let responses = drain(
        bound
            .generate_for_test(request(MODEL, None))
            .await
            .expect("native response stream"),
    )
    .await
    .expect("valid native stream");

    assert_eq!(responses.len(), 4);
    assert!(matches!(
        responses[0]
            .content
            .as_ref()
            .and_then(|content| content.parts.first()),
        Some(Part::Thinking { thinking, .. }) if thinking == "check"
    ));
    assert!(responses[3].turn_complete);
    assert_eq!(
        responses[3].finish_reason,
        Some(adk_rust::FinishReason::Stop)
    );
    assert!(responses[3].content.is_none());
    let usage = responses[3]
        .usage_metadata
        .as_ref()
        .expect("native usage metadata");
    assert_eq!(usage.prompt_token_count, 5);
    assert_eq!(usage.candidates_token_count, 4);
    assert_eq!(usage.cache_creation_input_token_count, Some(3));
    assert_eq!(usage.cache_read_input_token_count, Some(2));
    assert_eq!(
        bound.take_completion_for_test().expect("exact completion"),
        "native response"
    );

    let captured = captured.lock().expect("captured native request");
    let [request] = captured.as_slice() else {
        panic!("exactly one native request expected")
    };
    assert_eq!(request.uri.path(), "/llm/v1/messages");
    assert_eq!(request.version, Version::HTTP_2);
    assert_eq!(
        request.headers.get(CONTENT_TYPE),
        Some(&HeaderValue::from_static("application/json"))
    );
    assert_eq!(request.headers["x-project-id"], "17");
    assert_eq!(request.headers["anthropic-version"], "2023-06-01");
    assert_eq!(
        request.headers["anthropic-beta"],
        "prompt-caching-2024-07-31"
    );
    assert!(request.headers[AUTHORIZATION].is_sensitive());
    assert!(request.headers["x-api-key"].is_sensitive());
    assert_eq!(
        request.headers[CONTENT_LENGTH],
        request.body.len().to_string()
    );
    let body: serde_json::Value =
        serde_json::from_slice(&request.body).expect("native request JSON");
    assert_eq!(body["model"], MODEL);
    assert_eq!(body["messages"][0]["role"], "user");
    assert_eq!(body["messages"][0]["content"], "explain this");
    assert_eq!(body["system"][0]["type"], "text");
    assert_eq!(body["system"][0]["text"], "review carefully\nbe concise");
    assert_eq!(body["system"][0]["cache_control"]["type"], "ephemeral");
    assert_eq!(body["thinking"]["type"], "enabled");
    assert_eq!(body["thinking"]["budget_tokens"], 4_096);
    assert_eq!(body["temperature"], 1.0);
    assert_eq!(body["max_tokens"], 8_096);
    assert!(body.get("model_project_id").is_none());
}

#[tokio::test(flavor = "current_thread")]
async fn empty_system_instruction_is_omitted_from_anthropic_request() {
    let response =
        test_model_gateway_response(Body::new(Full::new(Bytes::from(native_sse(MODEL)))));
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(response)],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let mut invocation = invocation(MODEL, None);
    invocation.system_instruction.clear();
    let bound = client
        .bind_anthropic_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .expect("bound model without a system instruction");

    drain(
        bound
            .generate_for_test(request(MODEL, Some(0.7)))
            .await
            .expect("native response stream"),
    )
    .await
    .expect("valid native SSE");

    let captured = captured.lock().expect("captured requests");
    let body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("native request JSON");
    assert!(body.get("system").is_none());
    assert_eq!(body["messages"][0]["role"], "user");
}

#[tokio::test(flavor = "current_thread")]
async fn native_tools_calls_and_results_round_trip_across_model_turns() {
    native_tool_history_round_trip(false).await;
}

#[tokio::test(flavor = "current_thread")]
async fn native_retired_tool_history_does_not_admit_new_calls() {
    native_tool_history_round_trip(true).await;
}

#[allow(clippy::too_many_lines)] // Keep the history transition and rejected new call in one proof.
async fn native_tool_history_round_trip(retire_tool: bool) {
    let (client, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
                Bytes::from(tool_sse(MODEL)),
            )))),
            TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
                Bytes::from(native_sse(MODEL)),
            )))),
            TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
                Bytes::from(tool_sse(MODEL)),
            )))),
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let bound = client
        .bind_anthropic_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation(MODEL, None),
        )
        .expect("native model");
    let user = Content::new("user").with_text("double 21");
    let first = drain(
        bound
            .generate_for_test(tool_request(vec![user.clone()]))
            .await
            .expect("tool stream"),
    )
    .await
    .expect("tool response");
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
            if name == "double" && args["value"] == 21 && id.as_deref() == Some("tool_1")
    ));
    let result = Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: FunctionResponseData::new(
                "double",
                serde_json::json!({"value": 42}),
            ),
            id: Some("tool_1".to_owned()),
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
        bound.take_completion_for_test().expect("completion"),
        "native response"
    );

    if retire_tool {
        let bound = client
            .bind_anthropic_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                invocation(MODEL, None),
            )
            .unwrap();
        let mut unbound = tool_request(vec![Content::new("user").with_text("try again")]);
        unbound.tools.clear();
        assert!(
            drain(bound.generate_for_test(unbound).await.unwrap())
                .await
                .is_err()
        );
    }

    let captured = captured.lock().expect("captured requests");
    assert_eq!(captured.len(), if retire_tool { 3 } else { 2 });
    let first_body: serde_json::Value =
        serde_json::from_slice(&captured[0].body).expect("first body");
    assert_eq!(first_body["tools"][0]["name"], "double");
    assert_eq!(
        first_body["tools"][0]["input_schema"]["required"][0],
        "value"
    );
    let second_body: serde_json::Value =
        serde_json::from_slice(&captured[1].body).expect("second body");
    assert_eq!(second_body.get("tools").is_none(), retire_tool);
    assert_eq!(second_body["messages"][1]["content"][0]["type"], "tool_use");
    assert_eq!(
        second_body["messages"][2]["content"][0]["type"],
        "tool_result"
    );
    assert_eq!(
        second_body["messages"][2]["content"][0]["tool_use_id"],
        "tool_1"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn current_anthropic_models_reject_explicit_sampling_before_network_io() {
    for model in [
        "claude-fable-5",
        "claude-mythos-5",
        "claude-opus-4-8",
        "claude-opus-4-7",
        "claude-opus-5",
        "claude-sonnet-5",
    ] {
        let (client, captured) = test_model_gateway_client(Vec::new(), test_model_gateway_config())
            .expect("model gateway client");
        let bound = client
            .bind_anthropic_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                invocation(model, None),
            )
            .expect("bound native Anthropic model");

        let Err(error) = bound.generate_for_test(request(model, Some(0.7))).await else {
            panic!("sampling must fail before network I/O");
        };

        assert_eq!(error.code, "anthropic_gateway.sampling_unsupported");
        assert!(captured.lock().expect("captured requests").is_empty());
    }
}

#[tokio::test(flavor = "current_thread")]
async fn adaptive_and_disabled_reasoning_follow_provider_contracts() {
    for (model, effort, temperature, expected) in [
        (
            "claude-opus-4-7",
            Some(ModelReasoningEffort::High),
            None,
            serde_json::json!({
                "max_tokens": 4000,
                "thinking": {"type": "adaptive", "display": "summarized"},
                "output_config": {"effort": "high"},
            }),
        ),
        (
            "claude-sonnet-4-6",
            Some(ModelReasoningEffort::Medium),
            None,
            serde_json::json!({
                "max_tokens": 4000,
                "thinking": {"type": "adaptive", "display": "summarized"},
                "output_config": {"effort": "medium"},
            }),
        ),
        (
            "claude-sonnet-4-6",
            Some(ModelReasoningEffort::None),
            None,
            serde_json::json!({
                "max_tokens": 4000,
            }),
        ),
        (
            MODEL,
            Some(ModelReasoningEffort::None),
            Some(0.7),
            serde_json::json!({
                "max_tokens": 4000,
                "temperature": 0.7,
            }),
        ),
    ] {
        let (client, captured) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(
                test_model_gateway_response(Body::new(Full::new(Bytes::from(native_sse(model))))),
            )],
            test_model_gateway_config(),
        )
        .expect("model gateway client");
        let mut settings = invocation(model, effort);
        settings.temperature = temperature;
        let bound = client
            .bind_anthropic_ordinary(&ClaimScopedEliteaContext::fixture(17, TOKEN), 17, settings)
            .expect("native model");
        drain(
            bound
                .generate_for_test(request(model, temperature))
                .await
                .expect("native response stream"),
        )
        .await
        .expect("valid native stream");
        let captured = captured.lock().expect("captured request");
        let body: serde_json::Value =
            serde_json::from_slice(&captured[0].body).expect("native body");
        for (key, value) in expected.as_object().expect("expected fields") {
            assert_eq!(body.get(key), Some(value));
        }
        if expected.get("output_config").is_some() {
            assert!(body.get("temperature").is_none());
        } else {
            assert!(body.get("output_config").is_none());
        }
        if effort == Some(ModelReasoningEffort::None) {
            assert!(body.get("thinking").is_none());
        }
    }
}

#[tokio::test(flavor = "current_thread")]
async fn event_name_order_tool_and_citation_surfaces_fail_closed() {
    for (replacement, expected_code) in [
        (
            "event: wrong\ndata: {\"type\":\"message_start\"}\n\n".to_owned(),
            "anthropic_gateway.invalid_stream",
        ),
        (
            format!(
                concat!(
                    "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"msg\",\"content\":[],\"model\":\"{}\",\"role\":\"assistant\",\"type\":\"message\",\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n",
                    "event: content_block_start\ndata: {{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{{\"type\":\"tool_use\",\"id\":\"tool\",\"name\":\"unsafe\",\"input\":{{}}}}}}\n\n",
                ),
                MODEL
            ),
            "anthropic_gateway.unsupported_output",
        ),
        (
            format!(
                concat!(
                    "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"msg\",\"content\":[],\"model\":\"{}\",\"role\":\"assistant\",\"type\":\"message\",\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n",
                    "event: content_block_start\ndata: {{\"type\":\"content_block_start\",\"index\":0,\"content_block\":{{\"type\":\"text\",\"text\":\"\"}}}}\n\n",
                    "event: content_block_delta\ndata: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"citations_delta\",\"citation\":{{\"type\":\"char_location\",\"cited_text\":\"x\",\"document_index\":0,\"start_char_index\":0,\"end_char_index\":1}}}}}}\n\n",
                ),
                MODEL
            ),
            "anthropic_gateway.citations_unmapped",
        ),
    ] {
        let (client, _) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(
                test_model_gateway_response(Body::new(Full::new(Bytes::from(replacement)))),
            )],
            test_model_gateway_config(),
        )
        .expect("model gateway client");
        let bound = client
            .bind_anthropic_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                invocation(MODEL, None),
            )
            .expect("bound native model");
        let error = drain(
            bound
                .generate_for_test(request(MODEL, Some(0.7)))
                .await
                .expect("native response stream"),
        )
        .await
        .expect_err("unsupported native stream");
        assert_eq!(error.code, expected_code);
        assert!(!error.to_string().contains(TOKEN));
    }
}

#[tokio::test(flavor = "current_thread")]
async fn terminal_is_withheld_until_message_stop_and_clean_body_end() {
    for raw in [
        native_sse(MODEL).replace(
            "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
            "",
        ),
        format!(
            "{}event: ping\ndata: {{\"type\":\"ping\"}}\n\n",
            native_sse(MODEL)
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
            .bind_anthropic_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                invocation(MODEL, None),
            )
            .expect("bound native model");
        let error = drain(
            bound
                .generate_for_test(request(MODEL, Some(0.7)))
                .await
                .expect("native stream"),
        )
        .await
        .expect_err("incomplete or trailing stream");
        assert!(matches!(error.category, ErrorCategory::Unavailable));
    }
}

#[tokio::test(flavor = "current_thread")]
async fn native_credential_and_completion_are_single_use() {
    let (client, captured) = test_model_gateway_client(
        vec![
            TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
                Bytes::from(native_sse(MODEL)),
            )))),
            TestModelGatewayOutcome::Unavailable,
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let mut one_turn = invocation(MODEL, None);
    one_turn.max_model_turns = 1;
    let bound = client
        .bind_anthropic_ordinary(&ClaimScopedEliteaContext::fixture(17, TOKEN), 17, one_turn)
        .expect("bound native model");
    drain(
        bound
            .generate_for_test(request(MODEL, Some(0.7)))
            .await
            .expect("first stream"),
    )
    .await
    .expect("first completion");
    let Err(error) = bound.generate_for_test(request(MODEL, Some(0.7))).await else {
        panic!("one credential cannot make two calls")
    };
    assert_eq!(error.code, "anthropic_gateway.turn_limit");
    assert_eq!(captured.lock().expect("captured requests").len(), 1);
    assert_eq!(
        bound.take_completion_for_test().expect("completion once"),
        "native response"
    );
}
