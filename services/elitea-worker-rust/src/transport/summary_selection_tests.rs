use super::model_facade::{ModelAdapterKind, ModelFacade};
use super::openai_compatible_facade::{
    TestModelGatewayOutcome, test_model_facade_invocation, test_model_gateway_client,
    test_model_gateway_config, test_model_gateway_response, test_model_request,
};
use super::runtime_context::ClaimScopedEliteaContext;
use crate::agents::assembly::SummaryModelProfile;
use crate::agents::request::{ModelContextLimits, SummaryModelSnapshot};
use crate::agents::session::BoundOrdinaryAgentModel as _;
use adk_rust::{
    BaseEventsSummarizer as _, Content, Event, agent::LlmEventSummarizer, futures::StreamExt as _,
};
use bytes::Bytes;
use http_body_util::Full;
use serde_json::{Map, Value, json};
use tonic::body::Body;

fn selection(native: bool) -> SummaryModelProfile {
    SummaryModelProfile::admit(
        &SummaryModelSnapshot {
            llm_settings:
                json!({"model_name": if native { "claude-sonnet-4-5" } else { "summary-model" },
            "model_project_id":23,"max_tokens":512,"temperature":null,"openai_compatible":!native})
                .as_object()
                .unwrap()
                .clone(),
            model_context_limits: ModelContextLimits {
                context_window_tokens: 32_000,
                max_output_tokens: 4_000,
                context_window_fallback: false,
                max_output_fallback: false,
                max_input_tokens: Some(20_000),
            },
        },
        &Map::new(),
    )
    .unwrap()
}

fn response(native: bool) -> TestModelGatewayOutcome {
    let bytes = if native {
        super::anthropic_facade_tests::native_sse("claude-sonnet-4-5").into_bytes()
    } else {
        super::openai_compatible_facade_tests::ordinary_sse()
    };
    TestModelGatewayOutcome::Response(test_model_gateway_response(Body::new(Full::new(
        Bytes::from(bytes),
    ))))
}

#[tokio::test]
async fn dedicated_summary_uses_its_own_adapter_and_keeps_execution_billing_and_chat_completion() {
    for summary_native in [false, true] {
        let (gateway, captured) = test_model_gateway_client(
            vec![response(summary_native), response(!summary_native)],
            test_model_gateway_config(),
        )
        .unwrap();
        let mut invocation = test_model_facade_invocation();
        invocation.reasoning_effort = None;
        invocation.max_model_turns = 1;
        let adapter = if summary_native {
            ModelAdapterKind::OpenAiCompatible
        } else {
            invocation.model_name = "claude-sonnet-4-5".into();
            ModelAdapterKind::Anthropic
        };
        let name = invocation.model_name.clone();
        let bound = ModelFacade::from_gateway(gateway)
            .bind_with_summary(
                adapter,
                &ClaimScopedEliteaContext::fixture(17, "summary-fixture-token"),
                17,
                invocation,
                Some(&selection(summary_native)),
            )
            .unwrap();
        let summarizer = LlmEventSummarizer::new(bound.summarization_model().unwrap());
        let mut event = Event::new("summary-selection");
        event.set_content(
            Content::new("user")
                .with_text("Completed the first step; continue the remaining work."),
        );
        let summary = summarizer
            .summarize_events(&[event])
            .await
            .unwrap()
            .unwrap();
        assert!(summary.actions.compaction.is_some());
        assert!(
            bound
                .durable_completion()
                .unwrap()
                .snapshot()
                .unwrap()
                .is_none()
        );
        let mut chat = test_model_request("Continue");
        chat.model = name.clone();
        let responses = bound
            .adk_model()
            .generate_content(chat, true)
            .await
            .unwrap()
            .collect::<Vec<_>>()
            .await;
        assert!(responses.iter().all(Result::is_ok));
        assert_eq!(
            bound.take_completed_text().unwrap(),
            if summary_native {
                "Hello 🌍"
            } else {
                "native response"
            }
        );
        let requests = captured.lock().unwrap();
        assert_eq!(requests.len(), 2);
        for request in requests.iter() {
            assert_eq!(request.headers["x-project-id"], "17");
            assert_eq!(
                request.headers["authorization"],
                "Bearer summary-fixture-token"
            );
        }
        assert_eq!(
            requests[0].headers["x-elitea-execution-id"],
            requests[1].headers["x-elitea-execution-id"]
        );
        let summary: Value = serde_json::from_slice(&requests[0].body).unwrap();
        let chat: Value = serde_json::from_slice(&requests[1].body).unwrap();
        assert_eq!(summary["model"], selection(summary_native).model_name);
        assert_eq!(
            summary[if summary_native {
                "max_tokens"
            } else {
                "max_completion_tokens"
            }],
            512
        );
        assert!(summary.get("tools").is_none());
        assert_eq!(chat["model"], name);
        assert_eq!(
            chat[if summary_native {
                "max_completion_tokens"
            } else {
                "max_tokens"
            }],
            4_000
        );
    }
}

#[tokio::test]
async fn dedicated_summary_enforces_its_smaller_input_limit_before_dispatch() {
    let (gateway, captured) =
        test_model_gateway_client(Vec::new(), test_model_gateway_config()).unwrap();
    let mut profile = selection(false);
    profile.context_budget.limits.max_input_tokens = Some(1_000);
    profile.context_budget = profile
        .context_budget
        .for_model(profile.context_budget.limits, Some(profile.max_tokens))
        .unwrap();
    let bound = ModelFacade::from_gateway(gateway)
        .bind_with_summary(
            ModelAdapterKind::OpenAiCompatible,
            &ClaimScopedEliteaContext::fixture(17, "fixture"),
            17,
            test_model_facade_invocation(),
            Some(&profile),
        )
        .unwrap();
    let summarizer = LlmEventSummarizer::new(bound.summarization_model().unwrap());
    let mut event = Event::new("oversized-summary");
    event.set_content(Content::new("user").with_text("private summary input ".repeat(1_000)));
    let error = summarizer.summarize_events(&[event]).await.unwrap_err();
    assert_eq!(error.code, "context_budget_exceeded");
    assert!(!error.to_string().contains("private summary input"));
    assert!(captured.lock().unwrap().is_empty());
    assert!(
        bound
            .durable_completion()
            .unwrap()
            .snapshot()
            .unwrap()
            .is_none()
    );
}
