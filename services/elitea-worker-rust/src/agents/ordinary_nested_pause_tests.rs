//! ADR-0041 decision 5: a `hitl` pause inside a saved pipeline that a NESTED
//! saved agent calls as a tool (agent → agent → pipeline → `hitl`), and one
//! level deeper again (agent → agent → pipeline → pipeline node → `hitl`).
//!
//! Each run drives the whole ordinary assembler twice: the first run must end
//! paused with exactly one card whose owner path names both agent hops, and
//! the answer must re-enter the pipeline checkpoint once, at the right level,
//! and then let both agents above it continue.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use super::{
    AgentExecutionKind, AuthorizedNativeAssembly, AuthorizedNativeCommandBinding,
    CapturedModelRequest, ModelFacade, NativeAgentAssembler, NativeAgentCompletionSelector,
    OrdinaryNativeAgentAssembler, RuntimeContextClient, TOKEN, TestModelGatewayOutcome,
    application_version_response, attach_nested_agent, empty_tool_policy,
    encode_current_node_event_json, model_response_for, nested_agent_version, ordinary_request,
    platform_client, runtime_context_client_from, runtime_context_response,
    saved_agent_call_response, stored_pipeline_reference, test_model_gateway_client,
    test_model_gateway_config, test_runtime_context_authority, text_response,
};

/// One `hitl` gate whose two routes end on distinguishable verdicts.
const GATE_PIPELINE_YAML: &str = "state:\n  input:\n    type: str\n  messages:\n    type: list\n  verdict:\n    type: str\nentry_point: review\nnodes:\n  - id: review\n    type: hitl\n    input:\n      - input\n    user_message:\n      type: fixed\n      value: Approve?\n    routes:\n      approve: approved\n      reject: rejected\n  - id: approved\n    type: state_modifier\n    template: 'APPROVED {{ input }}'\n    input: [input]\n    output: [verdict]\n    transition: END\n  - id: rejected\n    type: state_modifier\n    template: 'REJECTED {{ input }}'\n    input: [input]\n    output: [verdict]\n    transition: END\n";

/// A pipeline whose only node runs the saved gate pipeline as a child graph.
const WRAPPER_PIPELINE_YAML: &str = "state:\n  input:\n    type: str\n  messages:\n    type: list\n  verdict:\n    type: str\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: gate-pipeline\n    input_mapping:\n      task: {type: variable, value: input}\n    output: [verdict]\n    transition: END\n";

fn pipeline_version(yaml: &str, tools: &[serde_json::Value]) -> serde_json::Value {
    serde_json::json!({
        "agent_type": "pipeline",
        "instructions": yaml,
        "meta": {},
        "variables": [],
        "tools": tools,
        "llm_settings": {
            "model_name": "child-model",
            "model_project_id": 23,
            "max_tokens": 2048,
            "reasoning_effort": null,
            "temperature": 0.2,
            "openai_compatible": true
        }
    })
}

/// The composition both runs resolve: the root's saved agent (31/41) holds
/// one saved pipeline (32/42). With `wrapped`, that pipeline is a wrapper
/// whose node runs the gate pipeline (33/43) as its own child graph.
fn nested_pipeline_runtime_context(
    wrapped: bool,
    cycles: usize,
) -> (RuntimeContextClient, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = (0..cycles)
        .flat_map(|_| {
            let mut cycle = vec![
                runtime_context_response(&serde_json::json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": TOKEN,
                })),
                application_version_response(
                    31,
                    41,
                    nested_agent_version(
                        "Delegate the review.",
                        "orchestrator-model",
                        23,
                        vec![stored_pipeline_reference(45, 32, 42, "review-pipeline")],
                    ),
                ),
            ];
            if wrapped {
                cycle.push(application_version_response(
                    32,
                    42,
                    pipeline_version(
                        WRAPPER_PIPELINE_YAML,
                        &[stored_pipeline_reference(46, 33, 43, "gate-pipeline")],
                    ),
                ));
                cycle.push(application_version_response(
                    33,
                    43,
                    pipeline_version(GATE_PIPELINE_YAML, &[]),
                ));
            } else {
                cycle.push(application_version_response(
                    32,
                    42,
                    pipeline_version(GATE_PIPELINE_YAML, &[]),
                ));
            }
            cycle
        })
        .collect::<VecDeque<_>>();
    let client = runtime_context_client_from(responses, Arc::clone(&calls), paths);
    (client, calls)
}

fn nested_pipeline_outcomes() -> Vec<TestModelGatewayOutcome> {
    vec![
        model_response_for(
            "fixture-model",
            saved_agent_call_response("call_orchestrator", "elitea_agent_31_v_41", "Review it"),
        ),
        model_response_for(
            "orchestrator-model",
            saved_agent_call_response("call_review", "elitea_agent_32_v_42", "ship it"),
        ),
        model_response_for("orchestrator-model", text_response("orchestrated")),
        model_response_for(
            "fixture-model",
            text_response("root nested pipeline answer"),
        ),
    ]
}

struct NestedPipelineHarness {
    assembler: OrdinaryNativeAgentAssembler,
    context_calls: Arc<AtomicUsize>,
    captured: Arc<Mutex<Vec<CapturedModelRequest>>>,
}

fn nested_pipeline_harness(wrapped: bool) -> NestedPipelineHarness {
    let (runtime_context, context_calls) = nested_pipeline_runtime_context(wrapped, 2);
    let (model_gateway, captured) =
        test_model_gateway_client(nested_pipeline_outcomes(), test_model_gateway_config())
            .expect("nested pipeline model gateway");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    )
    .with_sessions(Arc::new(adk_rust::session::InMemorySessionService::new())
        as Arc<dyn adk_rust::session::SessionService>);
    NestedPipelineHarness {
        assembler,
        context_calls,
        captured,
    }
}

/// Run one assembly to its end; every browser frame, and whether it paused.
async fn drain(
    assembler: &OrdinaryNativeAgentAssembler,
    request: &super::super::request::AgentExecutionRequest,
    authority: crate::protocol::control::ClaimBoundRuntimeContextAuthority,
) -> (Vec<serde_json::Value>, bool, Option<String>) {
    let mut invocation = assembler
        .assemble(AuthorizedNativeAssembly::new(
            request,
            authority,
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await
        .expect("nested pipeline assembly");
    invocation
        .project_start(chrono::Utc::now())
        .expect("nested pipeline start");
    let (mut native, mut projector, completion) = invocation.start().expect("native start");
    let mut frames = Vec::new();
    while let Some(event) = native.next_event().await.expect("nested pipeline event") {
        for projected in projector
            .project(&event)
            .expect("nested pipeline projection")
        {
            frames.push(
                serde_json::from_slice(
                    &encode_current_node_event_json(&projected).expect("browser event"),
                )
                .expect("browser JSON"),
            );
        }
    }
    if projector.is_paused() {
        return (frames, true, None);
    }
    let completed = completion
        .select()
        .await
        .expect("nested pipeline completion");
    let full = projector
        .finish_after_eos(completed, chrono::Utc::now())
        .expect("nested pipeline output")
        .into_iter()
        .find(|event| event.r#type == "full_message")
        .and_then(|event| serde_json::from_slice::<serde_json::Value>(&event.content).ok())
        .and_then(|content| content.as_str().map(str::to_owned));
    (frames, false, full)
}

fn decide(
    request: &mut super::super::request::AgentExecutionRequest,
    interrupt_id: &str,
    action: &str,
) {
    request.binding.request_content_digest = [5; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some(action.to_owned());
    request.payload.hitl_value = Some(String::new());
    request.payload.hitl_decisions = vec![serde_json::json!({
        "interrupt_id": interrupt_id,
        "action": action,
        "value": "",
    })];
}

/// The one card of the first run, with its owner path asserted.
fn only_card(frames: &[serde_json::Value], wrapped: bool) -> String {
    let cards = frames
        .iter()
        .filter(|frame| frame["type"] == "agent_hitl_interrupt")
        .collect::<Vec<_>>();
    assert_eq!(cards.len(), 1, "one pause raises one card: {frames:#?}");
    let card = cards[0];
    let path = card["response_metadata"]["parent_agent_path"]
        .as_array()
        .expect("owner path");
    assert_eq!(
        path[..2],
        [
            serde_json::json!({"name": "release-risk-agent", "call_id": "call_orchestrator", "sibling_ordinal": 1}),
            serde_json::json!({"name": "review-pipeline", "call_id": "call_review", "sibling_ordinal": 1}),
        ],
        "the card names the agent hop and the pipeline call above the gate"
    );
    if wrapped {
        // The wrapper's node that runs the gate pipeline is the third owner.
        assert_eq!(path.len(), 3, "{path:?}");
        assert_eq!(path[2]["name"], "gate-pipeline");
        assert!(
            path[2]["call_id"]
                .as_str()
                .is_some_and(|id| id.starts_with("pipeline-v2:delegate:")),
            "{path:?}"
        );
    } else {
        assert_eq!(path.len(), 2, "{path:?}");
    }
    assert_eq!(
        card["response_metadata"]["hitl_interrupts"][0]["parent_agent_path"],
        card["response_metadata"]["parent_agent_path"],
        "the interrupt and its card share one owner path"
    );
    card["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
        .as_str()
        .expect("nested pipeline interrupt identity")
        .to_owned()
}

async fn nested_pipeline_pause_round_trip(wrapped: bool, action: &str, expected: &str) {
    let harness = nested_pipeline_harness(wrapped);
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (frames, paused, _) = drain(
        &harness.assembler,
        &request,
        test_runtime_context_authority(),
    )
    .await;
    assert!(paused, "the gate pauses the whole run: {frames:#?}");
    let interrupt_id = only_card(&frames, wrapped);
    assert_eq!(harness.captured.lock().expect("captured").len(), 2);

    decide(&mut request, &interrupt_id, action);
    let (frames, paused, full) = drain(
        &harness.assembler,
        &request,
        test_runtime_context_authority(),
    )
    .await;
    assert!(!paused, "the answer continues every level: {frames:#?}");
    assert_eq!(full.as_deref(), Some("root nested pipeline answer"));
    assert!(
        frames
            .iter()
            .all(|frame| frame["type"] != "agent_hitl_interrupt"),
        "an answered gate is never raised again"
    );
    let captured = harness.captured.lock().expect("captured");
    // Two calls before the pause, then only the orchestrator's and the root's
    // answers: the replayed calls never reach the provider, and the gate's
    // pipeline resumes exactly once.
    assert_eq!(captured.len(), 4, "each level continues once");
    let orchestrator: serde_json::Value =
        serde_json::from_slice(&captured[2].body).expect("orchestrator continuation");
    let messages = orchestrator["messages"].as_array().expect("messages");
    let result = messages
        .iter()
        .find(|message| message["role"] == "tool" && message["tool_call_id"] == "call_review")
        .expect("the orchestrator reads the pipeline's own result");
    assert!(
        result["content"]
            .as_str()
            .is_some_and(|content| content.contains(expected)),
        "the parent sees the route the decision took: {result}"
    );
    drop(captured);
    assert_eq!(
        harness.context_calls.load(Ordering::Acquire),
        if wrapped { 8 } else { 6 }
    );
}

#[tokio::test(flavor = "current_thread")]
async fn two_level_nested_pipeline_pause_approves_once_and_continues_both_agents() {
    nested_pipeline_pause_round_trip(false, "approve", "APPROVED ship it").await;
}

#[tokio::test(flavor = "current_thread")]
async fn two_level_nested_pipeline_pause_rejects_and_the_parent_reads_the_rejection() {
    nested_pipeline_pause_round_trip(false, "reject", "REJECTED ship it").await;
}

#[tokio::test(flavor = "current_thread")]
async fn three_level_nested_pipeline_pause_approves_once_and_continues_every_parent() {
    nested_pipeline_pause_round_trip(true, "approve", "APPROVED ship it").await;
}

#[tokio::test(flavor = "current_thread")]
async fn three_level_nested_pipeline_pause_rejects_and_the_parent_reads_the_rejection() {
    nested_pipeline_pause_round_trip(true, "reject", "REJECTED ship it").await;
}

/// The answered card cannot be answered a second time: the decision is stale
/// once the run continued past it.
#[tokio::test(flavor = "current_thread")]
async fn a_nested_pipeline_pause_resumes_exactly_once() {
    let harness = nested_pipeline_harness(false);
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (frames, _, _) = drain(
        &harness.assembler,
        &request,
        test_runtime_context_authority(),
    )
    .await;
    let interrupt_id = only_card(&frames, false);
    decide(&mut request, &interrupt_id, "approve");
    let (_, paused, full) = drain(
        &harness.assembler,
        &request,
        test_runtime_context_authority(),
    )
    .await;
    assert!(!paused);
    assert_eq!(full.as_deref(), Some("root nested pipeline answer"));
    request.binding.request_content_digest = [6; 32];
    let replayed = harness
        .assembler
        .assemble(AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        ))
        .await;
    assert!(
        replayed.is_err(),
        "a second answer to the same card is refused before any model call"
    );
    assert_eq!(harness.captured.lock().expect("captured").len(), 4);
}

/// ADR-0044 decision 3: the delegation depth bounds the nesting a pause can
/// sit at. With a depth of one, the nested agent may not delegate to the
/// pipeline at all: its call answers the typed refusal, and nothing pauses.
#[tokio::test(flavor = "current_thread")]
async fn the_delegation_depth_cap_refuses_the_pipeline_before_it_can_pause() {
    let (runtime_context, _) = nested_pipeline_runtime_context(false, 1);
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            model_response_for(
                "fixture-model",
                saved_agent_call_response("call_orchestrator", "elitea_agent_31_v_41", "Review it"),
            ),
            model_response_for(
                "orchestrator-model",
                saved_agent_call_response("call_review", "elitea_agent_32_v_42", "ship it"),
            ),
            model_response_for("orchestrator-model", text_response("refused")),
            model_response_for("fixture-model", text_response("root after refusal")),
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let policy = elitea_agent_runtime::delegation::DelegationPolicy::new(
        1,
        elitea_agent_runtime::delegation::NarrowingMode::Enforce,
        [],
    )
    .expect("depth-one policy");
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (frames, paused, full) = drain(
        &assembler,
        &request,
        test_runtime_context_authority().with_delegation_policy_for_test(policy),
    )
    .await;
    assert!(!paused, "a refused pipeline never pauses: {frames:#?}");
    assert_eq!(full.as_deref(), Some("root after refusal"));
    let captured = captured.lock().expect("captured");
    let orchestrator: serde_json::Value =
        serde_json::from_slice(&captured[2].body).expect("orchestrator continuation");
    let result = orchestrator["messages"]
        .as_array()
        .expect("messages")
        .iter()
        .find(|message| message["role"] == "tool" && message["tool_call_id"] == "call_review")
        .expect("the refusal is the pipeline call's result")
        .to_string();
    assert!(
        result.contains("DELEGATION_DEPTH_EXCEEDED"),
        "the refusal names the depth: {result}"
    );
}

/// The deepest agent chain the tiers admit: root → agent → agent → pipeline.
/// With `wrapped`, that pipeline runs the gate pipeline as a child graph, so
/// the gate sits at delegation depth four, the default limit.
fn third_tier_pipeline_runtime_context(cycles: usize, wrapped: bool) -> RuntimeContextClient {
    let calls = Arc::new(AtomicUsize::new(0));
    let paths = Arc::new(Mutex::new(Vec::new()));
    let responses = (0..cycles)
        .flat_map(|_| {
            let mut cycle = vec![
                runtime_context_response(&serde_json::json!({
                    "schema_version": "elitea.runtime.elitea-client-token.v1",
                    "project_id": 17,
                    "token": TOKEN,
                })),
                application_version_response(
                    31,
                    41,
                    nested_agent_version(
                        "Delegate the review.",
                        "orchestrator-model",
                        23,
                        vec![super::stored_application_reference(
                            45,
                            32,
                            42,
                            "review-agent",
                        )],
                    ),
                ),
                application_version_response(
                    32,
                    42,
                    nested_agent_version(
                        "Run the review pipeline.",
                        "reviewer-model",
                        24,
                        vec![stored_pipeline_reference(46, 33, 43, "review-pipeline")],
                    ),
                ),
            ];
            if wrapped {
                cycle.push(application_version_response(
                    33,
                    43,
                    pipeline_version(
                        WRAPPER_PIPELINE_YAML,
                        &[stored_pipeline_reference(47, 34, 44, "gate-pipeline")],
                    ),
                ));
                cycle.push(application_version_response(
                    34,
                    44,
                    pipeline_version(GATE_PIPELINE_YAML, &[]),
                ));
            } else {
                cycle.push(application_version_response(
                    33,
                    43,
                    pipeline_version(GATE_PIPELINE_YAML, &[]),
                ));
            }
            cycle
        })
        .collect::<VecDeque<_>>();
    runtime_context_client_from(responses, calls, paths)
}

/// The deepest agent tier holds the pipeline: three owner tiers above the
/// gate, the most a card can name.
async fn deepest_tier_round_trip() {
    for (action, expected) in [
        ("approve", "APPROVED ship it"),
        ("reject", "REJECTED ship it"),
    ] {
        let (model_gateway, captured) = test_model_gateway_client(
            vec![
                model_response_for(
                    "fixture-model",
                    saved_agent_call_response(
                        "call_orchestrator",
                        "elitea_agent_31_v_41",
                        "Review it",
                    ),
                ),
                model_response_for(
                    "orchestrator-model",
                    saved_agent_call_response("call_reviewer", "elitea_agent_32_v_42", "Review"),
                ),
                model_response_for(
                    "reviewer-model",
                    saved_agent_call_response("call_review", "elitea_agent_33_v_43", "ship it"),
                ),
                model_response_for("reviewer-model", text_response("reviewed")),
                model_response_for("orchestrator-model", text_response("orchestrated")),
                model_response_for("fixture-model", text_response("root deepest answer")),
            ],
            test_model_gateway_config(),
        )
        .expect("model gateway");
        let assembler = OrdinaryNativeAgentAssembler::new(
            platform_client(third_tier_pipeline_runtime_context(2, false)),
            Arc::new(ModelFacade::from_gateway(model_gateway)),
            empty_tool_policy(),
        )
        .with_sessions(Arc::new(adk_rust::session::InMemorySessionService::new())
            as Arc<dyn adk_rust::session::SessionService>);
        let mut request = ordinary_request(AgentExecutionKind::Application);
        attach_nested_agent(&mut request);
        let (frames, paused, _) =
            drain(&assembler, &request, test_runtime_context_authority()).await;
        assert!(paused, "{frames:#?}");
        let cards = frames
            .iter()
            .filter(|frame| frame["type"] == "agent_hitl_interrupt")
            .collect::<Vec<_>>();
        assert_eq!(cards.len(), 1);
        let path = cards[0]["response_metadata"]["parent_agent_path"]
            .as_array()
            .cloned()
            .expect("owner path");
        assert_eq!(
            path[..3],
            [
                serde_json::json!({"name": "release-risk-agent", "call_id": "call_orchestrator", "sibling_ordinal": 1}),
                serde_json::json!({"name": "review-agent", "call_id": "call_reviewer", "sibling_ordinal": 1}),
                serde_json::json!({"name": "review-pipeline", "call_id": "call_review", "sibling_ordinal": 1}),
            ]
        );
        assert_eq!(path.len(), 3, "{path:?}");
        let interrupt_id = cards[0]["response_metadata"]["hitl_interrupts"][0]["interrupt_id"]
            .as_str()
            .expect("interrupt id")
            .to_owned();
        decide(&mut request, &interrupt_id, action);
        let (frames, paused, full) =
            drain(&assembler, &request, test_runtime_context_authority()).await;
        assert!(!paused, "{frames:#?}");
        assert_eq!(full.as_deref(), Some("root deepest answer"));
        let captured = captured.lock().expect("captured");
        assert_eq!(captured.len(), 6, "each level continues once");
        let reviewer: serde_json::Value =
            serde_json::from_slice(&captured[3].body).expect("reviewer continuation");
        assert!(
            reviewer.to_string().contains(expected),
            "the reviewer reads the gate's route: {reviewer}"
        );
    }
}

#[tokio::test(flavor = "current_thread")]
async fn a_pipeline_pause_below_the_deepest_agent_tier_round_trips() {
    deepest_tier_round_trip().await;
}

#[tokio::test(flavor = "current_thread")]
async fn a_pause_past_the_owner_path_bound_is_skipped_before_the_run() {
    // root → agent → agent → pipeline → pipeline → `hitl`: inside the default
    // delegation depth of four, but a fourth owner tier, which no card can
    // name. The wrapper is skipped and named before any model call; the
    // reviewer runs without it and nothing pauses.
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            model_response_for(
                "fixture-model",
                saved_agent_call_response("call_orchestrator", "elitea_agent_31_v_41", "Review it"),
            ),
            model_response_for(
                "orchestrator-model",
                saved_agent_call_response("call_reviewer", "elitea_agent_32_v_42", "Review"),
            ),
            model_response_for("reviewer-model", text_response("no gate offered")),
            model_response_for("orchestrator-model", text_response("orchestrated")),
            model_response_for("fixture-model", text_response("root without the gate")),
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(third_tier_pipeline_runtime_context(1, true)),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (frames, paused, full) =
        drain(&assembler, &request, test_runtime_context_authority()).await;
    assert!(!paused, "{frames:#?}");
    assert_eq!(full.as_deref(), Some("root without the gate"));
    let captured = captured.lock().expect("captured");
    let reviewer: serde_json::Value =
        serde_json::from_slice(&captured[2].body).expect("reviewer request");
    assert!(
        reviewer["tools"]
            .as_array()
            .into_iter()
            .flatten()
            .all(|tool| tool["function"]["name"] != "elitea_agent_33_v_43"),
        "the pipeline past the owner path bound is not offered: {reviewer}"
    );
}

/// One level below the delegation limit is refused, not paused: with a
/// depth of three, the gate pipeline (depth four) is refused when its
/// wrapper is offered, and the wrapper's call answers the typed refusal.
#[tokio::test(flavor = "current_thread")]
async fn a_pause_past_the_delegation_depth_is_refused_typed() {
    let (model_gateway, captured) = test_model_gateway_client(
        vec![
            model_response_for(
                "fixture-model",
                saved_agent_call_response("call_orchestrator", "elitea_agent_31_v_41", "Review it"),
            ),
            model_response_for(
                "orchestrator-model",
                saved_agent_call_response("call_reviewer", "elitea_agent_32_v_42", "Review"),
            ),
            model_response_for(
                "reviewer-model",
                saved_agent_call_response("call_review", "elitea_agent_33_v_43", "ship it"),
            ),
            model_response_for("reviewer-model", text_response("refused")),
            model_response_for("orchestrator-model", text_response("orchestrated")),
            model_response_for("fixture-model", text_response("root after refusal")),
        ],
        test_model_gateway_config(),
    )
    .expect("model gateway");
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(third_tier_pipeline_runtime_context(1, true)),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        empty_tool_policy(),
    );
    let policy = elitea_agent_runtime::delegation::DelegationPolicy::new(
        3,
        elitea_agent_runtime::delegation::NarrowingMode::Enforce,
        [],
    )
    .expect("depth-three policy");
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_nested_agent(&mut request);
    let (frames, paused, full) = drain(
        &assembler,
        &request,
        test_runtime_context_authority().with_delegation_policy_for_test(policy),
    )
    .await;
    assert!(!paused, "a refused pipeline never pauses: {frames:#?}");
    assert_eq!(full.as_deref(), Some("root after refusal"));
    let captured = captured.lock().expect("captured");
    let reviewer = String::from_utf8_lossy(&captured[3].body).into_owned();
    assert!(
        reviewer.contains("DELEGATION_DEPTH_EXCEEDED"),
        "the refusal names the depth: {reviewer}"
    );
}
