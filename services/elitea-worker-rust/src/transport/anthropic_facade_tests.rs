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
use crate::agents::session::BoundOrdinaryAgentModel as _;

const TOKEN: &str = "ephemeral-anthropic-fixture-token";
const MODEL: &str = "claude-sonnet-4-5";

fn invocation(model: &str, effort: Option<ModelReasoningEffort>) -> ModelFacadeInvocation {
    ModelFacadeInvocation {
        context_budget: None,
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
async fn distinct_execution_ids_produce_distinct_execution_id_headers() {
    let response =
        test_model_gateway_response(Body::new(Full::new(Bytes::from(native_sse(MODEL)))));
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(response)],
        test_model_gateway_config(),
    )
    .expect("model gateway client");
    let bound = client
        .bind_anthropic_ordinary(
            &ClaimScopedEliteaContext::fixture_with_execution_id(
                17,
                TOKEN,
                "execution/distinct-42",
            ),
            23,
            invocation(MODEL, Some(ModelReasoningEffort::Medium)),
        )
        .expect("native Anthropic model with a distinct execution id");

    drain(
        bound
            .generate_for_test(request(MODEL, None))
            .await
            .expect("native response stream"),
    )
    .await
    .expect("valid native stream");

    let captured = captured.lock().expect("captured native request");
    let [request] = captured.as_slice() else {
        panic!("exactly one native request expected")
    };
    assert_eq!(
        request.headers["x-elitea-execution-id"],
        "execution/distinct-42"
    );
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
    assert_eq!(
        request.headers["x-elitea-execution-id"],
        "execution/fixture-one"
    );
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
        let error = drain(bound.generate_for_test(unbound).await.unwrap())
            .await
            .expect_err("a retired tool is never emitted");
        assert_eq!(error.code, super::model_facade::TOOL_NOT_ADMITTED_CODE);
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
            super::model_facade::TOOL_NOT_ADMITTED_CODE,
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

#[tokio::test]
async fn authoritative_instruction_content_joins_anthropic_system_blocks() {
    let response =
        test_model_gateway_response(Body::new(Full::new(Bytes::from(native_sse(MODEL)))));
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(response)],
        test_model_gateway_config(),
    )
    .unwrap();
    let bound = client
        .bind_anthropic_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation(MODEL, None),
        )
        .unwrap();
    let mut input = request(MODEL, Some(0.7));
    input.contents.insert(
        0,
        Content::new("system").with_text("Exact restored project instruction."),
    );
    drain(bound.generate_for_test(input).await.unwrap())
        .await
        .unwrap();
    let captured = captured.lock().unwrap();
    let body: serde_json::Value = serde_json::from_slice(&captured[0].body).unwrap();
    assert!(
        body["system"]
            .as_array()
            .unwrap()
            .iter()
            .any(|block| block["text"] == "Exact restored project instruction.")
    );
    assert!(
        body["messages"]
            .as_array()
            .unwrap()
            .iter()
            .all(|message| message["role"] != "system")
    );
}

#[tokio::test(flavor = "current_thread")]
async fn full_request_context_budget_refuses_each_input_component_before_network() {
    use crate::agents::{context_budget::RequestContextBudget, request::ModelContextLimits};
    for component in 0..4 {
        let (client, captured) =
            test_model_gateway_client(Vec::new(), test_model_gateway_config()).unwrap();
        let mut invocation = invocation(MODEL, None);
        invocation.context_budget = RequestContextBudget::resolve(
            Some(ModelContextLimits {
                context_window_tokens: 8_000,
                max_output_tokens: 4_000,
                context_window_fallback: false,
                max_output_fallback: false,
                max_input_tokens: None,
            }),
            &serde_json::Map::new(),
            Some(4_000),
        )
        .unwrap();
        let mut request = request(MODEL, Some(0.7));
        let large = "private-context-fixture ".repeat(700);
        match component {
            0 => invocation.system_instruction = large,
            1 => request
                .contents
                .insert(0, Content::new("system").with_text(large)),
            2 => {
                request.tools.insert("inspect".to_owned(), serde_json::json!({
                "description": "Read an item.",
                "parameters": {"type":"object", "properties": {"item": {"type":"string", "description": large}}}
            }));
            }
            3 => request.contents.push(Content::new("user").with_text(large)),
            _ => unreachable!(),
        }
        let bound = client
            .bind_anthropic_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                invocation,
            )
            .unwrap();
        let usage = bound.request_budget().unwrap().measure(&request).unwrap();
        assert!(usage.needs_compaction(), "component {component}");
        assert!(!usage.fits(), "component {component}");
        assert!(captured.lock().unwrap().is_empty());
        let Err(error) = bound.generate_for_test(request).await else {
            panic!("oversized request was submitted")
        };
        assert_eq!(
            error.code, "context_budget_exceeded",
            "component {component}"
        );
        assert!(!error.to_string().contains("private-context-fixture"));
        assert!(captured.lock().unwrap().is_empty());
    }
}

#[tokio::test(flavor = "current_thread")]
async fn context_measurement_matches_dispatched_body_without_spending_turns() {
    use crate::agents::{context_budget::RequestContextBudget, request::ModelContextLimits};
    let (client, captured) = test_model_gateway_client(
        vec![TestModelGatewayOutcome::Response(
            test_model_gateway_response(Body::new(Full::new(Bytes::from(native_sse(MODEL))))),
        )],
        test_model_gateway_config(),
    )
    .unwrap();
    let mut invocation = invocation(MODEL, None);
    invocation.max_model_turns = 1;
    invocation.context_budget = RequestContextBudget::resolve(
        Some(ModelContextLimits {
            context_window_tokens: 128_000,
            max_output_tokens: 4_000,
            context_window_fallback: false,
            max_output_fallback: false,
            max_input_tokens: None,
        }),
        &serde_json::Map::new(),
        Some(4_000),
    )
    .unwrap();
    let bound = client
        .bind_anthropic_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .unwrap();
    let mut request = request(MODEL, Some(0.7));
    request
        .contents
        .push(Content::new("user").with_text("Measure 🦀 with native framing."));
    let measurement = bound.request_budget().unwrap();
    let usage = measurement.measure(&request).unwrap();
    assert_eq!(measurement.measure(&request).unwrap(), usage);
    assert!(usage.fits());
    assert!(!usage.needs_compaction());
    assert!(captured.lock().unwrap().is_empty());
    assert!(
        bound
            .durable_completion()
            .unwrap()
            .snapshot()
            .unwrap()
            .is_none()
    );
    drain(bound.generate_for_test(request).await.unwrap())
        .await
        .unwrap();
    let requests = captured.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert_eq!(usage.request_bytes, requests[0].body.len());
    assert_eq!(
        usage.estimated_input,
        u64::try_from(requests[0].body.len().div_ceil(4)).unwrap()
    );
    assert_eq!(bound.take_completion_for_test().unwrap(), "native response");
}

#[tokio::test(flavor = "current_thread")]
async fn adk_summaries_are_complete_and_isolated_from_the_chat_binding() {
    use adk_rust::{BaseEventsSummarizer as _, Event, agent::LlmEventSummarizer};
    let (client, captured) = test_model_gateway_client(
        (0..3)
            .map(|_| {
                TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(
                    Full::new(Bytes::from(native_sse(MODEL))),
                )))
            })
            .collect(),
        test_model_gateway_config(),
    )
    .unwrap();
    let mut invocation = invocation(MODEL, None);
    invocation.max_model_turns = 2;
    let bound = client
        .bind_anthropic_ordinary(
            &ClaimScopedEliteaContext::fixture(17, TOKEN),
            17,
            invocation,
        )
        .unwrap();
    let summarizer = LlmEventSummarizer::new(bound.summarization_model().unwrap());
    let original = format!("{}🦀 end of history", "x".repeat(60 * 1024 - 40));
    let mut event = Event::new("summary-fixture");
    event.author = "user".into();
    event.set_content(Content::new("user").with_text(original.clone()));
    for _ in 0..2 {
        let summary = summarizer
            .summarize_events(&[event.clone()])
            .await
            .unwrap()
            .unwrap();
        assert_eq!(
            summary.actions.compaction.unwrap().compacted_content.parts,
            vec![Part::Text {
                text: "native response".into()
            }]
        );
        assert!(
            bound
                .durable_completion()
                .unwrap()
                .snapshot()
                .unwrap()
                .is_none()
        );
    }
    assert!(summarizer.summarize_events(&[event]).await.is_err());
    drain(
        bound
            .generate_for_test(request(MODEL, Some(0.7)))
            .await
            .unwrap(),
    )
    .await
    .unwrap();
    assert_eq!(bound.take_completion_for_test().unwrap(), "native response");
    let requests = captured.lock().unwrap();
    assert_eq!(requests.len(), 3);
    let body: serde_json::Value = serde_json::from_slice(&requests[0].body).unwrap();
    assert_eq!(body["stream"], true);
    assert_eq!(body["max_tokens"], 4_000);
    assert!(body.get("tools").is_none());
    assert_eq!(body["system"][0]["text"], super::summary_model::INSTRUCTION);
    let parts = body["messages"][0]["content"].as_array().unwrap();
    assert!(parts.len() > 1);
    let prompt: String = parts.iter().map(|p| p["text"].as_str().unwrap()).collect();
    assert!(prompt.ends_with(&format!("user: {original}")));
}

#[tokio::test(flavor = "current_thread")]
async fn adk_summary_rejects_truncation_and_stream_failure_after_text() {
    use adk_rust::{BaseEventsSummarizer as _, Event, agent::LlmEventSummarizer};
    for body in [
        native_sse(MODEL).replace(
            "\"stop_reason\":\"end_turn\"",
            "\"stop_reason\":\"max_tokens\"",
        ),
        native_sse(MODEL).replace(
            "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
            "",
        ),
    ] {
        let (client, captured) = test_model_gateway_client(
            vec![TestModelGatewayOutcome::Response(
                test_model_gateway_response(Body::new(Full::new(Bytes::from(body)))),
            )],
            test_model_gateway_config(),
        )
        .unwrap();
        let bound = client
            .bind_anthropic_ordinary(
                &ClaimScopedEliteaContext::fixture(17, TOKEN),
                17,
                invocation(MODEL, None),
            )
            .unwrap();
        let summarizer = LlmEventSummarizer::new(bound.summarization_model().unwrap());
        let mut event = Event::new("summary-fixture");
        event.set_content(Content::new("user").with_text("private summary fixture"));
        let error = summarizer.summarize_events(&[event]).await.unwrap_err();
        assert!(!error.to_string().contains("private summary fixture"));
        assert!(
            bound
                .durable_completion()
                .unwrap()
                .snapshot()
                .unwrap()
                .is_none()
        );
        assert_eq!(captured.lock().unwrap().len(), 1);
    }
}
