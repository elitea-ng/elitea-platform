//! `auto` thinking as a per-step policy (ADR-0038 decision 4).
//!
//! The canonical setting ([`Thinking`]) and the request forms live in
//! `elitea_llm_wire::thinking`. This module decides, for one model request
//! of an agent run, whether `auto` thinks:
//!
//! * **On**, at [`DEFAULT_LEVEL`] (`auto` has no level of its own):
//!   * the first model step of a turn (planning) — the request ends with a
//!     message that is not a tool result;
//!   * a step after a tool error or a refused tool call (recovery) — a tool
//!     result that reports an error (`{"error": …}`, which the agent loop
//!     writes for a failed, denied or unknown call, or `{"status":
//!     "error"}`, which the platform's own tools write);
//!   * a step of an agent with a structured output schema. The runtime
//!     cannot tell which step gives the final answer before the model
//!     answers, so every step of such an agent thinks.
//! * **Off**:
//!   * a routine follow-up after successful tool results, unless the model's
//!     catalogue entry keeps thinking on for tool steps
//!     ([`AutoPolicy::tool_steps`]: families whose interleaved thinking
//!     helps tool choice);
//!   * an auxiliary call: summarisation, compaction, chat naming and
//!     cut-off summaries.
//!
//! An explicit setting (`off`, `low`, `medium`, `high`) always wins and
//! applies to every step. No setting (`None`) sends nothing, as before the
//! policy existed. The verified-loop roles of decisions 6 and 10 are not
//! built yet.

use adk_core::{Content, Part};
use serde_json::{Map, Value};

pub use elitea_llm_wire::thinking::{
    DEFAULT_LEVEL, Level, ReasoningControl, StepThinking, Thinking,
};

/// The catalogue's `auto` defaults for one model.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct AutoPolicy {
    /// `reasoning_tool_steps`: thinking stays on for a routine follow-up
    /// after successful tool results. Default off.
    pub tool_steps: bool,
}

/// What kind of step one model request is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StepKind {
    /// The first model step of a turn, or any step that does not follow
    /// tool results.
    Planning,
    /// A step after a tool error or a refused tool call.
    Recovery,
    /// A step after successful tool results.
    ToolFollowUp,
    /// A step of an agent with a structured output schema.
    StructuredAnswer,
    /// Summarisation, compaction, chat naming or a cut-off summary.
    Auxiliary,
}

/// The kind of an agent-loop request from its contents. `structured` is
/// whether the request carries a response schema.
#[must_use]
pub fn classify(contents: &[Content], structured: bool) -> StepKind {
    if structured {
        return StepKind::StructuredAnswer;
    }
    // Instructions and context notes are system contents; they do not
    // change what the last turn of the conversation was.
    let Some(last) = contents
        .iter()
        .rev()
        .find(|content| content.role != "system")
    else {
        return StepKind::Planning;
    };
    let mut results = last.parts.iter().filter_map(|part| match part {
        Part::FunctionResponse {
            function_response, ..
        } => Some(&function_response.response),
        _ => None,
    });
    let Some(first) = results.next() else {
        return StepKind::Planning;
    };
    if reports_error(first) || results.any(reports_error) {
        StepKind::Recovery
    } else {
        StepKind::ToolFollowUp
    }
}

/// Whether a tool result reports a failure: an `error` member that is not
/// null, or `status: "error"`.
fn reports_error(response: &Value) -> bool {
    response.as_object().is_some_and(|fields| {
        fields.get("error").is_some_and(|error| !error.is_null())
            || fields.get("status").and_then(Value::as_str) == Some("error")
    })
}

/// The thinking of one request: `None` sends nothing.
#[must_use]
pub const fn resolve(
    setting: Option<Thinking>,
    kind: StepKind,
    policy: AutoPolicy,
) -> Option<StepThinking> {
    let Some(setting) = setting else {
        return None;
    };
    if let Some(explicit) = setting.explicit() {
        return Some(explicit);
    }
    Some(match kind {
        StepKind::Planning | StepKind::Recovery | StepKind::StructuredAnswer => {
            StepThinking::On(DEFAULT_LEVEL)
        }
        StepKind::ToolFollowUp if policy.tool_steps => StepThinking::On(DEFAULT_LEVEL),
        StepKind::ToolFollowUp | StepKind::Auxiliary => StepThinking::Off,
    })
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

/// The thinking of one request on a model that has `reasoning` (the
/// catalogue's data, or `None` when the row declares no control form).
///
/// A declared form takes the `auto` per-step policy. An undeclared one never
/// switches thinking off by itself: an `OpenAI` o-series model refuses
/// `reasoning_effort: none`, so `auto` there thinks at the default level on
/// every step (the single-call rule). The worker facades apply the same rule.
#[must_use]
pub fn step(
    setting: Option<Thinking>,
    reasoning: Option<crate::request::ModelReasoning>,
    contents: &[Content],
    structured: bool,
) -> Option<StepThinking> {
    let setting = setting?;
    match reasoning {
        None => Some(setting.single_call()),
        Some(reasoning) => resolve(
            Some(setting),
            classify(contents, structured),
            reasoning.auto_policy(),
        ),
    }
}

/// Writes the thinking form of one OpenAI-compatible chat-completions request
/// into `body`, for a host that builds its own body (the desktop): the
/// translation table, with the catalogue's control form (`reasoning_effort`
/// when undeclared, the form such a host always sent) and [`step`]. It
/// returns whether the request must carry no temperature.
pub fn apply_openai_step(
    body: &mut Map<String, Value>,
    setting: Option<Thinking>,
    reasoning: Option<crate::request::ModelReasoning>,
    contents: &[Content],
    structured: bool,
) -> bool {
    let Some(step) = step(setting, reasoning, contents, structured) else {
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
    fn steps_are_classified_from_the_last_conversation_content() {
        assert_eq!(classify(&[], false), StepKind::Planning);
        assert_eq!(classify(&[user("hi")], false), StepKind::Planning);
        assert_eq!(
            classify(&[user("hi"), call(), result(json!({"text": "ok"}))], false),
            StepKind::ToolFollowUp
        );
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
            classify(
                &[user("hi"), call(), result(json!({"status": "error"}))],
                false
            ),
            StepKind::Recovery
        );
        assert_eq!(
            classify(
                &[
                    user("hi"),
                    call(),
                    result(json!({"error": null, "text": "ok"}))
                ],
                false
            ),
            StepKind::ToolFollowUp
        );
        assert_eq!(
            classify(&[user("hi"), call(), result(json!({}))], true),
            StepKind::StructuredAnswer
        );
    }

    #[test]
    fn auto_thinks_on_planning_and_recovery_and_not_on_routine_tool_steps() {
        let policy = AutoPolicy::default();
        let auto = Some(Thinking::Auto);
        let on = Some(StepThinking::On(Level::Medium));
        assert_eq!(resolve(auto, StepKind::Planning, policy), on);
        assert_eq!(resolve(auto, StepKind::Recovery, policy), on);
        assert_eq!(resolve(auto, StepKind::StructuredAnswer, policy), on);
        assert_eq!(
            resolve(auto, StepKind::ToolFollowUp, policy),
            Some(StepThinking::Off)
        );
        assert_eq!(
            resolve(auto, StepKind::Auxiliary, policy),
            Some(StepThinking::Off)
        );
        let interleaved = AutoPolicy { tool_steps: true };
        assert_eq!(resolve(auto, StepKind::ToolFollowUp, interleaved), on);
        assert_eq!(
            resolve(auto, StepKind::Auxiliary, interleaved),
            Some(StepThinking::Off)
        );
    }

    #[test]
    fn an_explicit_setting_wins_and_no_setting_sends_nothing() {
        let policy = AutoPolicy::default();
        assert_eq!(
            resolve(Some(Thinking::High), StepKind::ToolFollowUp, policy),
            Some(StepThinking::On(Level::High))
        );
        assert_eq!(
            resolve(Some(Thinking::Off), StepKind::Planning, policy),
            Some(StepThinking::Off)
        );
        assert_eq!(resolve(None, StepKind::Planning, policy), None);
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

    #[test]
    fn a_host_body_takes_the_table_and_the_policy() {
        let toggle = crate::request::ModelReasoning::parse("toggle", false);
        let after_tool = vec![user("go"), call(), result(json!({"value": 1}))];

        let mut body = Map::new();
        let refuses = apply_openai_step(
            &mut body,
            Some(Thinking::Auto),
            toggle,
            &[user("go")],
            false,
        );
        assert_eq!(body["chat_template_kwargs"]["enable_thinking"], json!(true));
        assert!(!refuses, "a toggle model takes a temperature");

        let mut body = Map::new();
        apply_openai_step(&mut body, Some(Thinking::Auto), toggle, &after_tool, false);
        assert_eq!(
            body["chat_template_kwargs"]["enable_thinking"],
            json!(false)
        );

        // Undeclared: reasoning_effort, and auto never sends the off form.
        let mut body = Map::new();
        let refuses = apply_openai_step(&mut body, Some(Thinking::Auto), None, &after_tool, false);
        assert_eq!(body["reasoning_effort"], json!("medium"));
        assert!(refuses);

        let mut body = Map::new();
        apply_openai_step(&mut body, Some(Thinking::Off), None, &after_tool, false);
        assert_eq!(body["reasoning_effort"], json!("none"));

        let mut body = Map::new();
        assert!(!apply_openai_step(
            &mut body,
            None,
            toggle,
            &after_tool,
            false
        ));
        assert!(body.is_empty(), "no setting sends nothing");
    }
}
