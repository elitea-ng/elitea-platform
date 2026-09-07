use std::collections::BTreeMap;
use std::io::{self, Write};
use std::sync::{Arc, Mutex};

use serde_json::{Map, Value, json};
use tracing_subscriber::fmt::MakeWriter;

use super::assembly::{
    DEFAULT_AGENT_STEP_LIMIT, OrdinaryModelProvider, OrdinaryNoToolProfile, ReasoningEffort,
};
use super::context_management::ContextManagementPlan;
use super::request::{
    AgentExecutionKind, AgentExecutionPayload, AgentExecutionRequest, AgentInputBinding,
    NextInputSuggestionPolicy, UserInput,
};
use super::runtime::{AuthorizedNativeAssembly, NativeAgentAssemblyErrorCode};
use super::session::AuthorizedNativeCommandBinding;
use crate::protocol::control::test_runtime_context_authority;
use crate::toolkits::ToolAdmissionPolicy;

fn empty_tool_policy() -> ToolAdmissionPolicy {
    ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("empty toolkit policy")
}

fn object(value: Value) -> Map<String, Value> {
    let Value::Object(value) = value else {
        panic!("fixture object")
    };
    value
}

pub(super) fn ordinary_request(kind: AgentExecutionKind) -> AgentExecutionRequest {
    let (llm, application) = match kind {
        AgentExecutionKind::Application => (
            object(json!({"kwargs": {"openai_compatible": true}})),
            object(json!({
                "id": 11,
                "version_id": 22,
                "variables": [],
                "version_details": {
                    "agent_type": "agent",
                    "instructions": "review carefully",
                    "meta": {},
                    "tools": [],
                    "llm_settings": {
                        "model_name": "fixture-model",
                        "model_project_id": 17,
                        "max_tokens": 4096,
                        "reasoning_effort": "medium",
                        "temperature": null,
                        "openai_compatible": true
                    }
                }
            })),
        ),
        AgentExecutionKind::Adhoc => (
            object(json!({
                "kwargs": {
                    "model": "fixture-model",
                    "model_project_id": 17,
                    "max_tokens": 2048,
                    "reasoning_effort": null,
                    "temperature": 0.7,
                    "stream": true,
                    "openai_compatible": true
                }
            })),
            object(json!({"instructions": "be concise"})),
        ),
    };
    AgentExecutionRequest {
        kind,
        binding: AgentInputBinding {
            input_bundle_id: "bundle-1".to_owned(),
            input_bundle_digest: [1; 32],
            request_entry_id: "request-1".to_owned(),
            request_immutable_version: "v1".to_owned(),
            request_content_digest: [2; 32],
        },
        payload: AgentExecutionPayload {
            llm,
            chat_history: Vec::new(),
            user_input: UserInput::Text("current".to_owned()),
            thread_id: Some("thread-1".to_owned()),
            checkpoint_id: None,
            debug: false,
            tools: Vec::new(),
            application,
            internal_tools: Vec::new(),
            steps_limit: None,
            mcp_tokens: Map::new(),
            ignored_mcp_servers: Vec::new(),
            user_declined_mcp_servers: Vec::new(),
            should_continue: false,
            hitl_resume: false,
            hitl_action: None,
            hitl_value: None,
            hitl_decisions: Vec::new(),
            execution_generation: Some("question-1".to_owned()),
            is_regenerate: false,
            meta: Map::new(),
            conversation_id: Some("conversation-1".to_owned()),
            persona: "generic".to_owned(),
            context_settings: Map::new(),
            supports_vision: false,
            return_chat_history: false,
            invoked_skills: Vec::new(),
            applied_skills: Vec::new(),
            auto_approve_sensitive_actions: false,
            attached_skills: Vec::new(),
            input_attachments: Vec::new(),
            parallel_reconcile: None,
            parallel_terminal_errors: Vec::new(),
            exception_handling_enabled: None,
            debug_mode: None,
            next_input_suggestion: NextInputSuggestionPolicy::default(),
            toolkit_guardrails: None,
            truncated_content: None,
        },
    }
}

pub(super) fn current_text_history() -> Vec<Value> {
    vec![
        json!({
            "role": "user",
            "content": [{"type": "text", "text": "earlier question"}],
            "additional_kwargs": {}
        }),
        json!({
            "role": "assistant",
            "content": [
                {"type": "text", "text": "earlier "},
                {"type": "text", "text": "answer"}
            ],
            "additional_kwargs": {}
        }),
    ]
}

#[test]
fn application_and_adhoc_ordinary_profiles_normalize_current_main_model_contracts() {
    let application =
        OrdinaryNoToolProfile::validate(&ordinary_request(AgentExecutionKind::Application))
            .expect("ordinary application profile");
    assert_eq!(application.kind(), AgentExecutionKind::Application);
    assert_eq!(application.model_name(), "fixture-model");
    assert_eq!(
        application.model_provider(),
        OrdinaryModelProvider::OpenAiChat
    );
    assert_eq!(application.model_project_id(), 17);
    assert_eq!(application.max_tokens(), Some(4096));
    assert_eq!(
        application.reasoning_effort(),
        Some(ReasoningEffort::Medium)
    );
    assert_eq!(application.temperature(), None);

    let adhoc = OrdinaryNoToolProfile::validate(&ordinary_request(AgentExecutionKind::Adhoc))
        .expect("ordinary ad-hoc profile");
    assert_eq!(adhoc.kind(), AgentExecutionKind::Adhoc);
    assert_eq!(adhoc.model_name(), "fixture-model");
    assert_eq!(adhoc.model_provider(), OrdinaryModelProvider::OpenAiChat);
    assert_eq!(adhoc.model_project_id(), 17);
    assert_eq!(adhoc.max_tokens(), Some(2048));
    assert_eq!(adhoc.reasoning_effort(), None);
    assert_eq!(adhoc.temperature(), Some(0.7));

    let mut no_system_instruction = ordinary_request(AgentExecutionKind::Adhoc);
    no_system_instruction
        .payload
        .application
        .insert("instructions".to_owned(), json!(""));
    assert!(OrdinaryNoToolProfile::validate(&no_system_instruction).is_ok());

    let mut multiline = ordinary_request(AgentExecutionKind::Application);
    multiline
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version")
        .insert(
            "instructions".to_owned(),
            json!("review carefully\nreturn a concise answer"),
        );
    assert!(OrdinaryNoToolProfile::validate(&multiline).is_ok());

    let mut disabled_reasoning = ordinary_request(AgentExecutionKind::Adhoc);
    let kwargs = disabled_reasoning
        .payload
        .llm
        .get_mut("kwargs")
        .and_then(Value::as_object_mut)
        .expect("ad-hoc model settings");
    kwargs.insert("reasoning_effort".to_owned(), json!("none"));
    let disabled_reasoning = OrdinaryNoToolProfile::validate(&disabled_reasoning)
        .expect("disabled reasoning retains temperature");
    assert_eq!(
        disabled_reasoning.reasoning_effort(),
        Some(ReasoningEffort::None)
    );
    assert_eq!(disabled_reasoning.temperature(), Some(0.7));
}

#[test]
fn context_management_admits_the_frozen_contract_and_refuses_the_rest() {
    let mut disabled = ordinary_request(AgentExecutionKind::Application);
    disabled
        .payload
        .context_settings
        .insert("enabled".to_owned(), json!(false));
    let profile = OrdinaryNoToolProfile::validate(&disabled)
        .expect("the current SDK master switch disables context management");
    assert_eq!(
        profile.context_management(),
        ContextManagementPlan::Disabled
    );

    let mut requested = ordinary_request(AgentExecutionKind::Application);
    requested
        .payload
        .context_settings
        .insert("enabled".to_owned(), json!(true));
    let profile = OrdinaryNoToolProfile::validate(&requested)
        .expect("the frozen context management contract is admitted");
    assert!(
        matches!(
            profile.context_management(),
            ContextManagementPlan::Summarize(_)
        ),
        "an active contract reaches the Runner composition seam"
    );

    let mut editing = ordinary_request(AgentExecutionKind::Application);
    editing
        .payload
        .context_settings
        .insert("enable_context_editing".to_owned(), json!(true));
    let error = OrdinaryNoToolProfile::validate(&editing)
        .expect_err("tool-output editing remains capability-gated");
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );

    let mut malformed = ordinary_request(AgentExecutionKind::Application);
    malformed
        .payload
        .context_settings
        .insert("enabled".to_owned(), json!("yes"));
    let error = OrdinaryNoToolProfile::validate(&malformed)
        .expect_err("a malformed master switch is not silently ignored");
    assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
}

#[test]
fn output_continuation_profile_requires_one_clean_explicit_partial() {
    for partial in ["partial visible answer", ""] {
        let mut request = ordinary_request(AgentExecutionKind::Adhoc);
        request.payload.should_continue = true;
        request.payload.truncated_content = Some(partial.to_owned());
        OrdinaryNoToolProfile::validate_output_continuation(&request)
            .expect("authorized output continuation");
        let error = OrdinaryNoToolProfile::validate(&request)
            .expect_err("a fresh turn must reject continuation state");
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        );
    }

    for mutation in 0..5 {
        let mut request = ordinary_request(AgentExecutionKind::Adhoc);
        request.payload.should_continue = true;
        request.payload.truncated_content = Some("partial visible answer".to_owned());
        match mutation {
            0 => request.payload.truncated_content = None,
            1 => request.payload.truncated_content = Some("invalid\0partial".to_owned()),
            2 => request.payload.truncated_content = Some("x".repeat(64 * 1_024 + 1)),
            3 => request.payload.hitl_resume = true,
            4 => request.payload.hitl_action = Some("approve".to_owned()),
            _ => unreachable!("bounded mutation corpus"),
        }
        let error = OrdinaryNoToolProfile::validate_output_continuation(&request)
            .expect_err("mixed or malformed output continuation must fail closed");
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        );
    }
}

#[test]
fn every_unimplemented_effect_surface_is_rejected_before_redemption() {
    // THREE surfaces LEFT this corpus when the runtime grew variable
    // substitution: a populated request-level `application.variables`, an
    // instruction carrying `{{ }}`, and a `meta.variables` dict. All three are
    // now SERVED (`super::variables`), and each has its own positive test
    // below — `a_populated_variable_list_is_substituted_into_the_instructions`
    // and `an_undefined_variable_survives_as_its_own_placeholder`.
    for mutation in 0..21 {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        match mutation {
            0 => request.payload.ignored_mcp_servers.push(json!("server")),
            1 => {
                request
                    .payload
                    .user_declined_mcp_servers
                    .push(json!({"server_url": "https://issuer.example"}));
            }
            2 => request.payload.hitl_resume = true,
            3 => request.payload.invoked_skills.push(json!("review")),
            4 => request
                .payload
                .attached_skills
                .push(json!({"name": "review"})),
            // Attachments are no longer an unsupported surface (#606): their
            // chunks render into the human message, and their shape is admitted
            // by `attachments::validate_input_attachments` (a MALFORMED chunk is
            // `InvalidInput`, not `UnsupportedCapability`, so it belongs in
            // `attachments`' own tests, not this UnsupportedCapability corpus).
            // `supports_vision` takes the freed slot — it is a genuine
            // unimplemented effect surface and is not otherwise covered here.
            5 => request.payload.supports_vision = true,
            6 => request.payload.user_input = UserInput::ContentBlocks(vec![json!({"text": "x"})]),
            7 => request.payload.checkpoint_id = Some("checkpoint-1".to_owned()),
            8 => request.payload.parallel_reconcile = Some(Map::new()),
            9 => request.payload.auto_approve_sensitive_actions = true,
            // An UNRECOGNIZED context setting is `InvalidInput` (see
            // `context_management_tests`); the surface left to pin here is the
            // one frozen strategy Rust has no ADK primitive for.
            10 => {
                request
                    .payload
                    .context_settings
                    .insert("enable_context_editing".to_owned(), json!(true));
            }
            11 => request.payload.return_chat_history = true,
            12 => request.payload.next_input_suggestion.enabled = true,
            13 => request.payload.debug_mode = Some(false),
            14 => request.payload.should_continue = true,
            // 15-19: names OUTSIDE the platform catalogue, in every
            // slot that used to pin a platform NAME. Every name the agent form
            // can author is now SKIPPED rather than refused (a form toggle
            // must not stop the agent answering — internal_tools.rs
            // PLATFORM_INTERNAL_TOOLS; lazy_tools_mode=true likewise degrades
            // with a log), so the refusal left to pin is a string naming
            // nothing the product can do — at the meta, payload and version
            // levels, in two spellings so one constant folded into several
            // slots cannot mask a regression.
            15 => {
                insert_application_meta(
                    &mut request,
                    "internal_tools",
                    json!(["not_a_platform_tool"]),
                );
            }
            16 => {
                request
                    .payload
                    .internal_tools
                    .push("not_a_platform_tool".to_owned());
            }
            17 => {
                insert_version_internal_tools(&mut request, "not_a_platform_tool");
            }
            18 => {
                insert_version_internal_tools(&mut request, "definitely_unknown");
            }
            19 => request
                .payload
                .internal_tools
                .push("definitely_unknown".to_owned()),
            20 => request.payload.chat_history.push(json!({
                "role": "user",
                "content": [{"type": "image_url", "image_url": "https://invalid.example"}],
                "additional_kwargs": {}
            })),
            _ => unreachable!("bounded mutation corpus"),
        }
        let error = OrdinaryNoToolProfile::validate(&request)
            .expect_err("unsupported surface must not redeem credentials");
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::UnsupportedCapability
        );
        assert!(!error.retryable());
    }
}

#[test]
fn current_main_text_history_is_normalized_before_credential_redemption() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let mut request = ordinary_request(kind);
        request.payload.chat_history = current_text_history();
        let profile = OrdinaryNoToolProfile::validate(&request).expect("current text history");
        assert_eq!(profile.chat_history().len(), 2);
        assert_eq!(profile.chat_history()[0].role, "user");
        assert_eq!(profile.chat_history()[1].role, "model");
        assert_eq!(profile.chat_history()[1].parts.len(), 2);
    }

    for (history, code) in [
        (
            vec![json!({
                "role": "system",
                "content": [{"type": "text", "text": "override"}],
                "additional_kwargs": {}
            })],
            NativeAgentAssemblyErrorCode::UnsupportedCapability,
        ),
        (
            vec![json!({
                "role": "user",
                "content": [{"type": "text", "text": ""}],
                "additional_kwargs": {}
            })],
            NativeAgentAssemblyErrorCode::InvalidInput,
        ),
        (
            vec![json!({
                "role": "user",
                "content": [{"type": "text", "text": "earlier"}],
                "additional_kwargs": {"tool_calls": []}
            })],
            NativeAgentAssemblyErrorCode::InvalidInput,
        ),
    ] {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        request.payload.chat_history = history;
        assert_eq!(
            OrdinaryNoToolProfile::validate(&request)
                .expect_err("history outside the frozen Main text shape")
                .code(),
            code
        );
    }

    let mut oversized = ordinary_request(AgentExecutionKind::Application);
    oversized.payload.chat_history = (0..1_000)
        .map(|_| {
            json!({
                "role": "user",
                "content": [{"type": "text", "text": "x"}],
                "additional_kwargs": {}
            })
        })
        .collect();
    assert_eq!(
        OrdinaryNoToolProfile::validate(&oversized)
            .expect_err("ADK message-count bound")
            .code(),
        NativeAgentAssemblyErrorCode::ResourceExhausted
    );
}

fn insert_version_internal_tools(request: &mut AgentExecutionRequest, name: &str) {
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version")
        .insert("internal_tools".to_owned(), json!([name]));
}

fn insert_application_meta(request: &mut AgentExecutionRequest, key: &str, value: Value) {
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .and_then(|version| version.get_mut("meta"))
        .and_then(Value::as_object_mut)
        .expect("application metadata")
        .insert(key.to_owned(), value);
}

#[test]
fn pipeline_tools_templates_and_defaults_are_classified_before_redemption() {
    let mut pipeline = ordinary_request(AgentExecutionKind::Application);
    let version = pipeline
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version");
    version.insert("agent_type".to_owned(), json!("pipeline"));
    assert_eq!(
        OrdinaryNoToolProfile::validate(&pipeline)
            .expect_err("pipeline profile")
            .code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );

    let mut configured_tool = ordinary_request(AgentExecutionKind::Application);
    configured_tool
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version")
        .insert("tools".to_owned(), json!([{"type": "github"}]));
    assert!(OrdinaryNoToolProfile::validate(&configured_tool).is_ok());
    assert!(
        AuthorizedNativeAssembly::new(
            &configured_tool,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        )
        .admit_llm_agent(&empty_tool_policy())
        .is_err()
    );

    let mut application_override = ordinary_request(AgentExecutionKind::Application);
    application_override
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version")
        .get_mut("llm_settings")
        .and_then(Value::as_object_mut)
        .expect("application model settings")
        .insert("openai_compatible".to_owned(), Value::Bool(false));
    assert!(OrdinaryNoToolProfile::validate(&application_override).is_ok());

    let mut defaulted_application = ordinary_request(AgentExecutionKind::Application);
    let version = defaulted_application
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version");
    version.remove("agent_type");
    version
        .get_mut("llm_settings")
        .and_then(Value::as_object_mut)
        .expect("application model settings")
        .insert("max_tokens".to_owned(), json!(-1));
    let profile = OrdinaryNoToolProfile::validate(&defaulted_application)
        .expect("SDK-compatible application defaults");
    assert_eq!(profile.max_tokens(), None);

    // An ad-hoc turn carries no variables — `predict_agent` builds its
    // assistant data with `variables: []` — but it still goes through the
    // react path's `_resolve_jinja2_variables`, so the template IS rendered
    // and an undefined name survives as Jinja2's `DebugUndefined` prints it.
    // The source spelling here is DELIBERATELY unspaced: the canonical
    // `{{ audience }}` coming back out is what distinguishes a real render
    // from a string that was merely passed through untouched.
    let mut templated_adhoc = ordinary_request(AgentExecutionKind::Adhoc);
    templated_adhoc
        .payload
        .application
        .insert("instructions".to_owned(), json!("review {{audience}}"));
    assert_eq!(
        OrdinaryNoToolProfile::validate(&templated_adhoc)
            .expect("an ad-hoc template is rendered, not refused")
            .instructions(),
        "review {{ audience }}"
    );
}

#[test]
fn authoritative_compatibility_selects_the_sdk_provider_dialect() {
    let mut native_adhoc = ordinary_request(AgentExecutionKind::Adhoc);
    let native_kwargs = native_adhoc
        .payload
        .llm
        .get_mut("kwargs")
        .and_then(Value::as_object_mut)
        .expect("model kwargs");
    native_kwargs.insert("model".to_owned(), json!("claude-sonnet-4-5"));
    native_kwargs.insert("openai_compatible".to_owned(), Value::Bool(false));
    assert_eq!(
        OrdinaryNoToolProfile::validate(&native_adhoc)
            .expect("native Anthropic ad-hoc profile")
            .model_provider(),
        OrdinaryModelProvider::NativeAnthropic
    );

    let mut native_application = ordinary_request(AgentExecutionKind::Application);
    native_application
        .payload
        .llm
        .get_mut("kwargs")
        .and_then(Value::as_object_mut)
        .expect("application runtime model")
        .insert("openai_compatible".to_owned(), Value::Bool(false));
    native_application
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .and_then(|version| version.get_mut("llm_settings"))
        .and_then(Value::as_object_mut)
        .expect("application model settings")
        .insert(
            "model_name".to_owned(),
            json!("anthropic/claude-sonnet-4-5"),
        );
    assert_eq!(
        OrdinaryNoToolProfile::validate(&native_application)
            .expect("native Anthropic application profile")
            .model_provider(),
        OrdinaryModelProvider::NativeAnthropic
    );

    native_adhoc
        .payload
        .llm
        .get_mut("kwargs")
        .and_then(Value::as_object_mut)
        .expect("model kwargs")
        .insert("openai_compatible".to_owned(), Value::Bool(true));
    assert_eq!(
        OrdinaryNoToolProfile::validate(&native_adhoc)
            .expect("Claude through OpenAI-compatible profile")
            .model_provider(),
        OrdinaryModelProvider::OpenAiChat
    );

    let mut disabled_adaptive_reasoning = ordinary_request(AgentExecutionKind::Adhoc);
    let settings = disabled_adaptive_reasoning
        .payload
        .llm
        .get_mut("kwargs")
        .and_then(Value::as_object_mut)
        .expect("adaptive model settings");
    settings.insert("model".to_owned(), json!("claude-sonnet-4-6"));
    settings.insert("openai_compatible".to_owned(), json!(false));
    settings.insert("reasoning_effort".to_owned(), json!("none"));
    let disabled_adaptive_reasoning = OrdinaryNoToolProfile::validate(&disabled_adaptive_reasoning)
        .expect("native Anthropic may disable adaptive reasoning");
    assert_eq!(
        disabled_adaptive_reasoning.reasoning_effort(),
        Some(ReasoningEffort::None)
    );
    assert_eq!(
        disabled_adaptive_reasoning.model_provider(),
        OrdinaryModelProvider::NativeAnthropic
    );
}

#[test]
fn malformed_model_and_session_bindings_are_not_treated_as_dependency_failures() {
    for mutation in 0..4 {
        let mut request = ordinary_request(AgentExecutionKind::Adhoc);
        match mutation {
            0 => request.payload.thread_id = None,
            1 => request.payload.conversation_id = Some("bad\nidentity".to_owned()),
            2 => {
                request
                    .payload
                    .llm
                    .get_mut("kwargs")
                    .and_then(Value::as_object_mut)
                    .expect("model kwargs")
                    .insert("model_project_id".to_owned(), json!(0));
            }
            3 => {
                let kwargs = request
                    .payload
                    .llm
                    .get_mut("kwargs")
                    .and_then(Value::as_object_mut)
                    .expect("model kwargs");
                kwargs.insert("reasoning_effort".to_owned(), json!("high"));
                kwargs.insert("temperature".to_owned(), json!(0.7));
            }
            _ => unreachable!("bounded mutation corpus"),
        }
        let error =
            OrdinaryNoToolProfile::validate(&request).expect_err("malformed ordinary profile");
        assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
        assert!(!error.retryable());
    }
}

#[test]
fn credential_redemption_is_unreachable_until_the_profile_is_admitted() {
    let request = ordinary_request(AgentExecutionKind::Application);
    let admitted = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
    .admit_llm_agent(&empty_tool_policy())
    .expect("admitted assembly");
    assert_eq!(admitted.request().kind, AgentExecutionKind::Application);
    assert_eq!(admitted.profile().model_project_id(), 17);

    let mut unsupported = ordinary_request(AgentExecutionKind::Application);
    unsupported.payload.tools.push(json!({"type": "github"}));
    assert!(
        AuthorizedNativeAssembly::new(
            &unsupported,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        )
        .admit_llm_agent(&empty_tool_policy())
        .is_err()
    );
}

#[test]
fn regeneration_is_admitted_as_a_durable_session_rebuild() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.is_regenerate = true;
    OrdinaryNoToolProfile::validate_regeneration(&request).expect("regeneration profile");
    assert_eq!(
        OrdinaryNoToolProfile::validate(&request)
            .expect_err("fresh admission must not absorb regeneration")
            .code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );

    let admitted = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
    .admit_llm_agent(&empty_tool_policy())
    .expect("admitted regeneration");
    assert!(admitted.is_resume());

    let mut mixed = request;
    mixed.payload.should_continue = true;
    let Err(error) = AuthorizedNativeAssembly::new(
        &mixed,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
    .admit_llm_agent(&empty_tool_policy()) else {
        panic!("regeneration cannot also resume an interrupt");
    };
    assert_eq!(
        error.code(),
        NativeAgentAssemblyErrorCode::UnsupportedCapability
    );
}

#[test]
fn session_tokens_do_not_turn_fresh_or_regenerated_agents_into_guard_resumes() {
    for kind in [AgentExecutionKind::Adhoc, AgentExecutionKind::Application] {
        for regenerate in [false, true] {
            let mut request = ordinary_request(kind);
            request.payload.is_regenerate = regenerate;
            request.payload.mcp_tokens.insert(
                "credential:https://issuer.example".to_owned(),
                json!({"access_token": "test-session-token"}),
            );
            let admitted = AuthorizedNativeAssembly::new(
                &request,
                test_runtime_context_authority(),
                AuthorizedNativeCommandBinding::fixture(),
            )
            .admit_llm_agent(&empty_tool_policy())
            .expect("session credentials do not require an interrupt");
            assert_eq!(admitted.is_resume(), regenerate);
        }
    }
}

#[test]
fn output_continuation_cannot_add_session_authority() {
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.should_continue = true;
    request.payload.truncated_content = Some("partial".to_owned());
    request.payload.mcp_tokens.insert(
        "credential:https://issuer.example".to_owned(),
        json!({"access_token":"test-token"}),
    );
    assert!(OrdinaryNoToolProfile::validate_output_continuation(&request).is_err());
}

/// The authored step limit lives on the version AND on the input, and this
/// pins both halves of that arrangement.
///
/// Main writes `meta.step_limit` into every saved version
/// (`services/elitea-main/internal/api/v2/applications/handler.go`) and derives
/// `steps_limit` on the execution input from the same number
/// (`internal/application/agentexecution/start.go::currentApplicationStepsLimit`).
/// The Python worker still reads the version key for its `LangGraph` recursion
/// limit, so neither side can drop it. Refusing the key here refused every
/// stored agent — measured in a browser against a live stack, where the turn
/// was admitted, streamed nothing, and stopped.
///
/// The effective limit is still `payload.steps_limit` alone: the version value
/// is admitted, not consulted.
#[test]
fn an_authored_step_limit_is_admitted_and_does_not_select_the_effective_one() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    insert_application_meta(&mut request, "step_limit", json!(17));
    request.payload.steps_limit = Some(64);

    let admitted = AuthorizedNativeAssembly::new(
        &request,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
    .admit_llm_agent(&empty_tool_policy())
    .expect("an authored step limit must not refuse the profile");
    assert_eq!(admitted.profile().step_limit(), 64);

    let mut without_input_limit = ordinary_request(AgentExecutionKind::Application);
    insert_application_meta(&mut without_input_limit, "step_limit", json!(17));
    let defaulted = AuthorizedNativeAssembly::new(
        &without_input_limit,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
    .admit_llm_agent(&empty_tool_policy())
    .expect("an authored step limit must not refuse the profile");
    assert_eq!(defaulted.profile().step_limit(), DEFAULT_AGENT_STEP_LIMIT);
}

/// A version step limit outside the bounds the input enforces is malformed
/// input, not an unimplemented capability — the distinction matters because
/// only the second is a "this runtime cannot do that yet" answer.
#[test]
fn a_malformed_version_step_limit_is_refused_as_invalid_input() {
    for value in [json!(0), json!(1_025), json!(-1), json!("many"), json!(1.5)] {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        insert_application_meta(&mut request, "step_limit", value.clone());
        let Err(error) = AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        )
        .admit_llm_agent(&empty_tool_policy()) else {
            panic!("an out-of-range step limit must not be admitted: {value}");
        };
        assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    }
}

/// An agent saved with no instructions must run.
///
/// The create form does not require the field and nothing fills it in, so this
/// is an ordinary thing to have in a project. Refusing it produced "The
/// execution input is invalid." on every turn — measured in a browser against a
/// live stack, on an agent created through the product's own form.
///
/// A PIPELINE is the opposite case: its instructions carry the graph YAML, and
/// an empty one has nothing to compile.
#[test]
fn a_direct_agent_may_have_no_instructions_but_a_pipeline_may_not() {
    let mut agent = ordinary_request(AgentExecutionKind::Application);
    set_application_instructions(&mut agent, "");
    AuthorizedNativeAssembly::new(
        &agent,
        test_runtime_context_authority(),
        AuthorizedNativeCommandBinding::fixture(),
    )
    .admit_llm_agent(&empty_tool_policy())
    .expect("an agent with no system prompt is still an agent");

    let mut pipeline = ordinary_request(AgentExecutionKind::Application);
    set_application_agent_type(&mut pipeline, "pipeline");
    set_application_instructions(&mut pipeline, "");
    assert_eq!(
        OrdinaryNoToolProfile::validate_pipeline_shell(&pipeline, false)
            .expect_err("an empty pipeline graph has nothing to run")
            .code(),
        NativeAgentAssemblyErrorCode::InvalidInput
    );
}

fn application_version_mut(request: &mut AgentExecutionRequest) -> &mut Map<String, Value> {
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version")
}

fn set_application_instructions(request: &mut AgentExecutionRequest, instructions: &str) {
    application_version_mut(request).insert("instructions".to_owned(), json!(instructions));
}

fn set_application_agent_type(request: &mut AgentExecutionRequest, agent_type: &str) {
    application_version_mut(request).insert("agent_type".to_owned(), json!(agent_type));
}

/// `meta.variables` arrives as an ARRAY, because that is what Main stores.
///
/// The create path folds variables into meta only when there are some, but the
/// update path writes the key on presence so that deleting the last variable is
/// distinguishable from never having had one. Every agent saved a second time
/// therefore carries `"variables": []` — measured on a live stack, where such an
/// agent answered "The execution input is invalid." on every turn.
#[test]
fn an_empty_variable_list_is_admitted_in_either_shape() {
    for empty in [json!([]), json!({}), Value::Null] {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        insert_application_meta(&mut request, "variables", empty.clone());
        AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        )
        .admit_llm_agent(&empty_tool_policy())
        .unwrap_or_else(|error| {
            panic!("an empty variable list must be admitted: {empty} ({error:?})")
        });
    }
}

/// A list that actually names variables is SUBSTITUTED, in both stored shapes.
///
/// Main writes `meta.variables` as an array of `{name, value}`; the dict
/// spelling is the one `assistant.py:574` reads. Both reach the same context,
/// and the assertion is on the rendered `instructions` rather than on mere
/// admission — admitting a variable and then ignoring it would pass a
/// "does it refuse?" test while leaving the author's placeholder on screen.
#[test]
fn a_populated_variable_list_is_substituted_into_the_instructions() {
    for populated in [
        json!([{"name": "audience", "value": "ops"}]),
        json!({"audience": "ops"}),
    ] {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        insert_application_meta(&mut request, "variables", populated.clone());
        set_application_instructions(&mut request, "review for {{audience}} only");
        let profile = OrdinaryNoToolProfile::validate(&request)
            .unwrap_or_else(|error| panic!("a populated variable list: {populated} ({error:?})"));
        assert_eq!(profile.instructions(), "review for ops only");
        AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        )
        .admit_llm_agent(&empty_tool_policy())
        .unwrap_or_else(|error| panic!("a populated variable list: {populated} ({error:?})"));
    }
}

/// The REQUEST's own `application.variables` re-VALUE a declared variable and
/// cannot declare one — `client.py:822-825` updates only names already in the
/// version's list, and `meta.variables` is applied afterwards and still wins.
#[test]
fn participant_variables_revalue_declared_names_only() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version")
        .insert(
            "variables".to_owned(),
            json!([{"name": "tone", "value": "terse"}]),
        );
    request.payload.application.insert(
        "variables".to_owned(),
        json!([
            {"name": "tone", "value": "formal"},
            {"name": "undeclared", "value": "ignored"},
        ]),
    );
    set_application_instructions(&mut request, "{{tone}} / {{undeclared}}");
    assert_eq!(
        OrdinaryNoToolProfile::validate(&request)
            .expect("participant variables")
            .instructions(),
        "formal / {{ undeclared }}"
    );

    let mut meta_wins = ordinary_request(AgentExecutionKind::Application);
    meta_wins
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version")
        .insert(
            "variables".to_owned(),
            json!([{"name": "tone", "value": "terse"}]),
        );
    insert_application_meta(
        &mut meta_wins,
        "variables",
        json!([{"name": "tone", "value": "stored"}]),
    );
    meta_wins.payload.application.insert(
        "variables".to_owned(),
        json!([{"name": "tone", "value": "formal"}]),
    );
    set_application_instructions(&mut meta_wins, "{{tone}}");
    assert_eq!(
        OrdinaryNoToolProfile::validate(&meta_wins)
            .expect("meta variables")
            .instructions(),
        "stored"
    );
}

/// An undefined name is neither blanked nor fatal — Jinja2's `DebugUndefined`
/// prints it back, and `assistant.py` never overrides that.
#[test]
fn an_undefined_variable_survives_as_its_own_placeholder() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    insert_application_meta(
        &mut request,
        "variables",
        json!([{"name": "known", "value": "here"}]),
    );
    set_application_instructions(&mut request, "{{known}} then {{missing}}");
    assert_eq!(
        OrdinaryNoToolProfile::validate(&request)
            .expect("an undefined variable is not a refusal")
            .instructions(),
        "here then {{ missing }}"
    );
}

/// A malformed collection is still MALFORMED INPUT, not a served shape.
///
/// The refusal that moved is the "populated" one; the shape gate did not. A
/// scalar where a collection belongs, and an array element that is not a row,
/// are both things no Main path writes.
#[test]
fn a_malformed_variable_collection_is_still_invalid_input() {
    for malformed in [
        json!("audience"),
        json!(7),
        json!(["audience"]),
        json!([null]),
    ] {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        insert_application_meta(&mut request, "variables", malformed.clone());
        assert_eq!(
            OrdinaryNoToolProfile::validate(&request)
                .expect_err("a malformed variable collection")
                .code(),
            NativeAgentAssemblyErrorCode::InvalidInput,
            "malformed variables: {malformed}"
        );
    }
}

/// A row Fork can genuinely produce — `"name": null` — is SKIPPED, not fatal.
///
/// `api/openapi/v2.yaml`'s `VersionVariable` documents both keys as nullable
/// because `eliteacore/handler.go:2421-2428` rebuilds entries from unvalidated
/// client maps. `assistant.py:565` skips such a row; refusing it would end
/// every turn of a forked agent instead.
#[test]
fn a_nameless_or_valueless_variable_row_is_skipped() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    insert_application_meta(
        &mut request,
        "variables",
        json!([
            {"name": null, "value": "orphan"},
            {"name": "blank", "value": ""},
            {"name": "absent"},
            {"name": "served", "value": "yes"},
        ]),
    );
    set_application_instructions(&mut request, "{{served}}|{{blank}}|{{absent}}");
    assert_eq!(
        OrdinaryNoToolProfile::validate(&request)
            .expect("nullable variable rows are skipped, not refused")
            .instructions(),
        "yes|{{ blank }}|{{ absent }}"
    );
}

/// A NESTED agent renders its own stored variables too.
///
/// The SDK reaches a child through `client.application()` as well
/// (`runtime/tools/application.py:396`), which builds a second
/// `LangChainAssistant` over the child's version — so the child's prompt goes
/// through the same `_resolve_jinja2_variables`. A nested PIPELINE keeps the
/// parent rule: its instructions are graph YAML and stay untouched.
#[test]
fn a_nested_agent_renders_its_own_variables() {
    let fallback =
        OrdinaryNoToolProfile::validate(&ordinary_request(AgentExecutionKind::Application))
            .expect("a parent profile to fall back to");

    let mut child = Map::new();
    child.insert("tools".to_owned(), json!([]));
    child.insert(
        "meta".to_owned(),
        json!({"variables": [{"name": "region", "value": "eu"}]}),
    );
    child.insert(
        "instructions".to_owned(),
        json!("serve {{region}} and {{gap}}"),
    );
    assert_eq!(
        OrdinaryNoToolProfile::from_nested_version(&child, &fallback)
            .expect("a nested agent")
            .instructions(),
        "serve eu and {{ gap }}"
    );

    child.insert("agent_type".to_owned(), json!("pipeline"));
    assert_eq!(
        OrdinaryNoToolProfile::from_nested_pipeline_version(&child, &fallback)
            .expect("a nested pipeline")
            .instructions(),
        "serve {{region}} and {{gap}}"
    );
}

/// A PIPELINE's instructions are graph YAML, and the SDK does not render them.
///
/// `assistant.py`'s `pipeline()` hands `self.prompt` straight to
/// `create_graph`; only the react path calls `_resolve_jinja2_variables`. A
/// port that rendered here would rewrite a YAML document behind the compiler's
/// back.
#[test]
fn a_pipeline_graph_is_never_rendered() {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    set_application_agent_type(&mut request, "pipeline");
    insert_application_meta(
        &mut request,
        "variables",
        json!([{"name": "audience", "value": "ops"}]),
    );
    set_application_instructions(&mut request, "state: {{audience}}");
    assert_eq!(
        OrdinaryNoToolProfile::validate_pipeline_shell(&request, false)
            .expect("a pipeline shell")
            .instructions(),
        "state: {{audience}}"
    );
}

/// Smart Tools Selection is a form toggle, and a toggle must not end the turn.
///
/// `meta.lazy_tools_mode = true` used to refuse the whole profile. It now
/// degrades: this runtime exposes every attached tool, which is the mode's SAFE
/// side (lazy mode narrows exposure), and it says so once in the log. The old
/// refusal test was repointed to a name outside the platform catalogue, so
/// nothing asserted the degrade — a return to refusing would have been silent
/// here and visible only as an agent that stops answering.
#[test]
fn smart_tools_selection_degrades_with_one_warning_instead_of_refusing() {
    let capture = CapturedOutput::default();
    let subscriber = tracing_subscriber::fmt()
        .without_time()
        .with_ansi(false)
        .with_target(false)
        .with_writer(capture.clone())
        .finish();
    let mut request = ordinary_request(AgentExecutionKind::Application);
    insert_application_meta(&mut request, "lazy_tools_mode", json!(true));

    tracing::subscriber::with_default(subscriber, || {
        AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        )
        .admit_llm_agent(&empty_tool_policy())
        .expect("smart tools selection must not stop the agent answering");
    });

    let logged = capture.text();
    assert!(logged.contains("WARN"), "degrade must be logged: {logged}");
    assert!(logged.contains("agent_internal_tool_skipped"));
    assert!(logged.contains("lazy_tools_mode"));
    assert!(!logged.contains("ERROR"));
}

/// The off and absent forms stay silent, and a non-boolean is still malformed.
///
/// Without this, "degrades with a warning" could be satisfied by warning on
/// every agent, and the catch-all that still refuses a non-boolean would be
/// unpinned.
#[test]
fn only_the_enabled_smart_tools_toggle_is_reported_and_non_booleans_are_refused() {
    for quiet in [Some(json!(false)), None] {
        let capture = CapturedOutput::default();
        let subscriber = tracing_subscriber::fmt()
            .without_time()
            .with_ansi(false)
            .with_target(false)
            .with_writer(capture.clone())
            .finish();
        let mut request = ordinary_request(AgentExecutionKind::Application);
        if let Some(value) = quiet.clone() {
            insert_application_meta(&mut request, "lazy_tools_mode", value);
        }
        tracing::subscriber::with_default(subscriber, || {
            AuthorizedNativeAssembly::new(
                &request,
                test_runtime_context_authority(),
                AuthorizedNativeCommandBinding::fixture(),
            )
            .admit_llm_agent(&empty_tool_policy())
            .expect("an unset toggle is an ordinary profile");
        });
        assert!(
            !capture.text().contains("lazy_tools_mode"),
            "an unset toggle must not report a skipped capability: {quiet:?}"
        );
    }

    for malformed in [json!("true"), json!(1), json!({})] {
        let mut request = ordinary_request(AgentExecutionKind::Application);
        insert_application_meta(&mut request, "lazy_tools_mode", malformed.clone());
        let Err(error) = AuthorizedNativeAssembly::new(
            &request,
            test_runtime_context_authority(),
            AuthorizedNativeCommandBinding::fixture(),
        )
        .admit_llm_agent(&empty_tool_policy()) else {
            panic!("a non-boolean toggle is malformed input: {malformed}")
        };
        assert_eq!(error.code(), NativeAgentAssemblyErrorCode::InvalidInput);
    }
}

#[derive(Clone, Default)]
struct CapturedOutput {
    bytes: Arc<Mutex<Vec<u8>>>,
}

impl CapturedOutput {
    fn text(&self) -> String {
        String::from_utf8(self.bytes.lock().expect("captured tracing lock").clone())
            .expect("captured tracing UTF-8")
    }
}

struct CapturedWriter {
    bytes: Arc<Mutex<Vec<u8>>>,
}

impl Write for CapturedWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        self.bytes
            .lock()
            .map_err(|_| io::Error::other("captured tracing lock failed"))?
            .extend_from_slice(buffer);
        Ok(buffer.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl<'a> MakeWriter<'a> for CapturedOutput {
    type Writer = CapturedWriter;

    fn make_writer(&'a self) -> Self::Writer {
        CapturedWriter {
            bytes: Arc::clone(&self.bytes),
        }
    }
}
