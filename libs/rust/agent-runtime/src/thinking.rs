//! `auto` thinking as a per-step policy (ADR-0038 decision 4), for an agent
//! run.
//!
//! The policy itself is ONE copy, in `elitea_llm_wire::thinking`
//! ([`classify`] of tool results, [`step`] of a request), pinned by
//! `conformance/llm-caller/contract.json`. This module only reads an adk
//! request for it: the tool results of the last conversation turn (system
//! contents, instructions and context notes, do not change what that turn
//! was), whether it carries a response schema, and whether it declares
//! tools. The desktop host and both worker facades call [`step_for_request`].

use adk_core::{Content, LlmRequest, Part};
use serde_json::{Map, Value};

pub use elitea_llm_wire::thinking::{
    AutoPolicy, DEFAULT_LEVEL, Level, ReasoningControl, StepKind, StepRequest, StepThinking,
    Thinking, continuation_setting, reports_error, resolve,
};

/// The kind of an agent-loop request from its contents. `structured` is
/// whether the request carries a response schema.
#[must_use]
pub fn classify(contents: &[Content], structured: bool) -> StepKind {
    let last = contents
        .iter()
        .rev()
        .find(|content| content.role != "system");
    let results = last
        .into_iter()
        .flat_map(|content| content.parts.iter())
        .filter_map(|part| match part {
            Part::FunctionResponse {
                function_response, ..
            } => Some(&function_response.response),
            _ => None,
        });
    elitea_llm_wire::thinking::classify(structured, results)
}

/// The control kind a request is built for: the catalogue's, else the kind
/// the caller used before the catalogue declared one
/// ([`ReasoningControl::legacy`]).
#[must_use]
pub const fn effective_control(
    declared: Option<ReasoningControl>,
    native_anthropic: bool,
) -> ReasoningControl {
    match declared {
        Some(control) => control,
        None => ReasoningControl::legacy(native_anthropic),
    }
}

/// The thinking of one adk request on a model that has `reasoning` (the
/// catalogue's data, or `None` when the row declares no control form):
/// `elitea_llm_wire::thinking::step` over the request's kind and tools.
#[must_use]
pub fn step_for_request(
    setting: Option<Thinking>,
    reasoning: Option<crate::request::ModelReasoning>,
    request: &LlmRequest,
) -> Option<StepThinking> {
    let structured = request
        .config
        .as_ref()
        .is_some_and(|config| config.response_schema.is_some());
    elitea_llm_wire::thinking::step(
        setting,
        reasoning.map(|reasoning| (reasoning.control, reasoning.auto_policy())),
        StepRequest {
            kind: classify(&request.contents, structured),
            has_tools: !request.tools.is_empty(),
        },
    )
}

/// Writes the thinking form of one OpenAI-compatible chat-completions request
/// into `body`, for a host that builds its own body (the desktop): the
/// translation table, with the catalogue's control form (`reasoning_effort`
/// when undeclared, the form such a host always sent) and
/// [`step_for_request`]. It returns whether the request must carry no
/// temperature.
pub fn apply_openai_step(
    body: &mut Map<String, Value>,
    setting: Option<Thinking>,
    reasoning: Option<crate::request::ModelReasoning>,
    request: &LlmRequest,
) -> bool {
    let Some(step) = step_for_request(setting, reasoning, request) else {
        return false;
    };
    let control = reasoning.map_or(ReasoningControl::Effort, |reasoning| reasoning.control);
    elitea_llm_wire::thinking::apply_openai(
        body,
        elitea_llm_wire::thinking::openai_form(control, step),
    );
    control.refuses_temperature(step)
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_core::FunctionResponseData;
    use serde_json::json;

    fn user(text: &str) -> Content {
        Content {
            role: "user".to_owned(),
            parts: vec![Part::Text {
                text: text.to_owned(),
            }],
        }
    }

    fn call() -> Content {
        Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "read_file".to_owned(),
                args: json!({}),
                id: Some("call_1".to_owned()),
                thought_signature: None,
            }],
        }
    }

    fn result(response: Value) -> Content {
        Content {
            role: "function".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: FunctionResponseData {
                    name: "read_file".to_owned(),
                    response,
                    inline_data: Vec::new(),
                    file_data: Vec::new(),
                },
                id: Some("call_1".to_owned()),
                annotations: None,
            }],
        }
    }

    fn system() -> Content {
        Content {
            role: "system".to_owned(),
            parts: vec![Part::Text {
                text: "note".to_owned(),
            }],
        }
    }

    #[test]
    fn an_undeclared_control_keeps_the_callers_previous_form() {
        assert_eq!(effective_control(None, true), ReasoningControl::Budget);
        assert_eq!(effective_control(None, false), ReasoningControl::Effort);
        assert_eq!(
            effective_control(Some(ReasoningControl::Toggle), false),
            ReasoningControl::Toggle
        );
    }

    fn request(contents: Vec<Content>, tools: bool) -> LlmRequest {
        let mut request = LlmRequest::new("m", contents);
        if tools {
            request
                .tools
                .insert("read_file".to_owned(), json!({"name": "read_file"}));
        }
        request
    }

    #[test]
    fn a_host_body_takes_the_table_and_the_policy() {
        let toggle = crate::request::ModelReasoning::parse("toggle", false);
        let first = request(vec![user("go")], true);
        let after_tool = request(vec![user("go"), call(), result(json!({"value": 1}))], true);

        let mut body = Map::new();
        let refuses = apply_openai_step(&mut body, Some(Thinking::Auto), toggle, &first);
        assert_eq!(body["chat_template_kwargs"]["enable_thinking"], json!(true));
        assert!(!refuses, "a toggle model takes a temperature");

        let mut body = Map::new();
        apply_openai_step(&mut body, Some(Thinking::Auto), toggle, &after_tool);
        assert_eq!(
            body["chat_template_kwargs"]["enable_thinking"],
            json!(false)
        );

        // Undeclared: reasoning_effort, and auto never sends the off form.
        let mut body = Map::new();
        let refuses = apply_openai_step(&mut body, Some(Thinking::Auto), None, &after_tool);
        assert_eq!(body["reasoning_effort"], json!("medium"));
        assert!(refuses);

        let mut body = Map::new();
        apply_openai_step(&mut body, Some(Thinking::Off), None, &after_tool);
        assert_eq!(body["reasoning_effort"], json!("none"));

        let mut body = Map::new();
        assert!(!apply_openai_step(&mut body, None, toggle, &after_tool));
        assert!(body.is_empty(), "no setting sends nothing");
    }

    /// ADR-0038 (#1574 review): a model whose chat-completions tool calls
    /// refuse thinking (GPT-5.6) does not think under auto on a request
    /// with tools, from the planning step on; without tools it thinks.
    #[test]
    fn off_with_tools_turns_auto_off_on_a_tool_request() {
        let gpt = crate::request::ModelReasoning::parse("effort", false)
            .map(|reasoning| reasoning.with_off_with_tools(true));
        let mut body = Map::new();
        let refuses = apply_openai_step(
            &mut body,
            Some(Thinking::Auto),
            gpt,
            &request(vec![user("go")], true),
        );
        assert_eq!(body["reasoning_effort"], json!("none"));
        assert!(
            !refuses,
            "an effort model that does not think takes a temperature"
        );

        let mut body = Map::new();
        apply_openai_step(
            &mut body,
            Some(Thinking::Auto),
            gpt,
            &request(vec![user("go")], false),
        );
        assert_eq!(body["reasoning_effort"], json!("medium"));
    }

    /// ADR-0038 (#1574 review): a budget model never switches thinking from
    /// off to on inside one tool loop under auto.
    #[test]
    fn a_budget_loop_keeps_one_thinking_state() {
        let claude = crate::request::ModelReasoning::parse("budget", false);
        let loop_steps = [
            request(vec![user("go")], true),
            request(vec![user("go"), call(), result(json!({"value": 1}))], true),
            request(
                vec![user("go"), call(), result(json!({"error": "denied"}))],
                true,
            ),
            request(
                vec![
                    user("go"),
                    call(),
                    result(json!({"value": 1})),
                    call(),
                    result(json!({"value": 2})),
                ],
                true,
            ),
        ];
        let states: Vec<_> = loop_steps
            .iter()
            .map(|step| step_for_request(Some(Thinking::Auto), claude, step))
            .collect();
        assert!(
            states
                .iter()
                .all(|state| *state == Some(StepThinking::On(DEFAULT_LEVEL))),
            "{states:?}"
        );
    }

    #[test]
    fn steps_are_classified_from_the_last_conversation_content() {
        assert_eq!(classify(&[], false), StepKind::Planning);
        assert_eq!(classify(&[user("hi")], false), StepKind::Planning);
        assert_eq!(
            classify(
                &[user("hi"), call(), result(json!({"text": "ok"})), system()],
                false
            ),
            StepKind::ToolFollowUp,
            "a trailing context note does not hide the tool result"
        );
        assert_eq!(
            classify(
                &[user("hi"), call(), result(json!({"error": "denied"}))],
                false
            ),
            StepKind::Recovery
        );
        assert_eq!(
            classify(&[user("hi"), call(), result(json!({}))], true),
            StepKind::StructuredAnswer
        );
    }
}
