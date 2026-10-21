//! Shrinking older tool results inside a turn (#1215 stage 4).
//!
//! Every model step resends every earlier tool result. Once the request
//! the model is about to get passes a threshold of its window,
//! [`ToolResultElision`] replaces the bodies of older tool results (all
//! but the newest [`DEFAULT_KEEP_RECENT`]) with a short note naming the
//! tool and its arguments, and the `output_id` when the tool spilled its
//! output, so the model can re-run it or page it with `read_output`.
//!
//! Only the request is shrunk, in an adk `before_model` callback: the
//! session keeps every event as it was, so a resume, the transcript and
//! the server's copy see the full results.
//!
//! Opt-in, per host: nothing changes unless a host composes it
//! ([`ToolResultElision::bind_builder`]). The desktop does; the cloud
//! worker does not yet, so its requests (and digests) are unchanged. The
//! setting's shape in a conversation's context settings is
//! [`ELIDE_TOOL_RESULTS_KEY`], read by [`ToolResultElision::admit`]; it is
//! not one of `context_management`'s admitted keys yet, because a worker
//! that admits it must also compose it (admitting it alone would silently
//! drop what it promises).

use adk_agent::LlmAgentBuilder;
use adk_core::{Content, Part};
use serde_json::{Map, Value, json};

use crate::assembly_error::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};

/// Tool results always kept whole: the newest this many.
pub const DEFAULT_KEEP_RECENT: usize = 6;

/// The share of the window past which older results are elided.
pub const DEFAULT_THRESHOLD_PERCENT: u64 = 60;

/// The context-settings key ([`ToolResultElision::admit`]):
/// `{"threshold_tokens": n, "keep_recent": n}`, both optional.
pub const ELIDE_TOOL_RESULTS_KEY: &str = "elide_tool_results";

/// The longest argument summary a note carries.
const MAX_ARGS_SUMMARY: usize = 160;

/// Bytes per estimated token (as the local tools' budgets count).
const BYTES_PER_TOKEN: usize = 4;

/// When and how much of a request's older tool results to elide.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ToolResultElision {
    /// Elide once the request's estimated tokens pass this.
    pub threshold_tokens: u64,
    /// The newest tool results kept whole.
    pub keep_recent: usize,
}

const fn invalid_input() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "native agent context settings are malformed",
    )
}

/// Estimated tokens of one content: its text, call arguments and
/// response bodies.
fn content_tokens(content: &Content) -> usize {
    let bytes: usize = content
        .parts
        .iter()
        .map(|part| match part {
            Part::Text { text } | Part::Thinking { thinking: text, .. } => text.len(),
            Part::FunctionCall { name, args, .. } => name.len() + args.to_string().len(),
            Part::FunctionResponse {
                function_response, ..
            } => function_response.name.len() + function_response.response.to_string().len(),
            other => serde_json::to_string(other).map_or(0, |text| text.len()),
        })
        .sum();
    bytes.div_ceil(BYTES_PER_TOKEN)
}

/// A short, single-line view of a call's arguments.
fn args_summary(args: &Value) -> String {
    let text = args.to_string();
    if text.len() <= MAX_ARGS_SUMMARY {
        return text;
    }
    let mut end = MAX_ARGS_SUMMARY;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &text[..end])
}

/// The arguments of the call `id` (or, without ids, of the latest call of
/// `name` before `before`), from the model's turns.
fn call_args<'a>(
    contents: &'a [Content],
    before: usize,
    id: Option<&str>,
    name: &str,
) -> Option<&'a Value> {
    contents[..before].iter().rev().find_map(|content| {
        content.parts.iter().rev().find_map(|part| match part {
            Part::FunctionCall {
                name: called,
                args,
                id: call_id,
                ..
            } => {
                let same = match (id, call_id.as_deref()) {
                    (Some(want), Some(got)) => want == got,
                    _ => called == name,
                };
                same.then_some(args)
            }
            _ => None,
        })
    })
}

impl ToolResultElision {
    /// Elide past `threshold_tokens`, keeping the newest
    /// [`DEFAULT_KEEP_RECENT`].
    #[must_use]
    pub const fn new(threshold_tokens: u64) -> Self {
        Self {
            threshold_tokens,
            keep_recent: DEFAULT_KEEP_RECENT,
        }
    }

    /// Elide past [`DEFAULT_THRESHOLD_PERCENT`] of a `window_tokens` window.
    #[must_use]
    pub const fn for_window(window_tokens: u64) -> Self {
        Self::new(window_tokens / 100 * DEFAULT_THRESHOLD_PERCENT)
    }

    /// The setting in a conversation's context settings: `None` when
    /// [`ELIDE_TOOL_RESULTS_KEY`] is absent (nothing changes), else its
    /// `threshold_tokens` (default: 60% of `window_tokens`) and
    /// `keep_recent` (default 6, at least 1).
    ///
    /// # Errors
    ///
    /// [`NativeAgentAssemblyErrorCode::InvalidInput`] for a value of the
    /// wrong shape.
    pub fn admit(
        settings: &Map<String, Value>,
        window_tokens: u64,
    ) -> Result<Option<Self>, NativeAgentAssemblyError> {
        let object = match settings.get(ELIDE_TOOL_RESULTS_KEY) {
            None | Some(Value::Null | Value::Bool(false)) => return Ok(None),
            Some(Value::Bool(true)) => return Ok(Some(Self::for_window(window_tokens))),
            Some(Value::Object(object)) => object,
            Some(_) => return Err(invalid_input()),
        };
        if object
            .keys()
            .any(|key| key != "threshold_tokens" && key != "keep_recent")
        {
            return Err(invalid_input());
        }
        let mut elision = Self::for_window(window_tokens);
        if let Some(value) = object.get("threshold_tokens") {
            elision.threshold_tokens = value
                .as_u64()
                .filter(|tokens| *tokens > 0)
                .ok_or_else(invalid_input)?;
        }
        if let Some(value) = object.get("keep_recent") {
            elision.keep_recent = value
                .as_u64()
                .and_then(|keep| usize::try_from(keep).ok())
                .filter(|keep| (1..=1000).contains(keep))
                .ok_or_else(invalid_input)?;
        }
        Ok(Some(elision))
    }

    /// Shrink `contents` in place when they pass the threshold: every tool
    /// result but the newest [`Self::keep_recent`] (and any already as
    /// small as its note) becomes a note. The number elided.
    pub fn apply(&self, contents: &mut [Content]) -> usize {
        let total: usize = contents.iter().map(content_tokens).sum();
        if (total as u64) <= self.threshold_tokens {
            return 0;
        }
        let results: Vec<(usize, usize)> = contents
            .iter()
            .enumerate()
            .flat_map(|(at, content)| {
                content
                    .parts
                    .iter()
                    .enumerate()
                    .filter(|(_, part)| matches!(part, Part::FunctionResponse { .. }))
                    .map(move |(index, _)| (at, index))
            })
            .collect();
        let older = results.len().saturating_sub(self.keep_recent);
        let mut elided = 0;
        for &(at, index) in &results[..older] {
            let note = {
                let Part::FunctionResponse {
                    function_response,
                    id,
                    ..
                } = &contents[at].parts[index]
                else {
                    continue;
                };
                // Already a note: leave it as it is.
                if function_response.response.get("elided").is_some() {
                    continue;
                }
                let name = function_response.name.as_str();
                let args = call_args(contents, at, id.as_deref(), name)
                    .map_or_else(String::new, |args| format!(" {}", args_summary(args)));
                let spilled = function_response
                    .response
                    .get("output_id")
                    .and_then(Value::as_str)
                    .map_or_else(
                        || "re-run it if you need it again".to_owned(),
                        |id| {
                            format!(
                                "re-run it, or page it with read_output {{\"output_id\": \"{id}\"}}"
                            )
                        },
                    );
                let note = format!("[result elided after use: {name}{args}; {spilled}]");
                if function_response.response.to_string().len() <= note.len() + 16 {
                    continue;
                }
                note
            };
            if let Part::FunctionResponse {
                function_response, ..
            } = &mut contents[at].parts[index]
            {
                function_response.response = json!({ "elided": note });
                function_response.inline_data.clear();
                function_response.file_data.clear();
                elided += 1;
            }
        }
        if elided > 0 {
            tracing::debug!(
                event = "tool_results_elided",
                elided,
                estimated_tokens = total,
                threshold_tokens = self.threshold_tokens,
                "older tool results elided from the model request"
            );
        }
        elided
    }

    /// Compose onto an agent: every model request is shrunk by
    /// [`Self::apply`] before it is sent; the session is not touched.
    #[must_use]
    pub fn bind_builder(self, builder: LlmAgentBuilder) -> LlmAgentBuilder {
        builder.before_model_callback(Box::new(move |_ctx, mut request| {
            Box::pin(async move {
                self.apply(&mut request.contents);
                Ok(adk_core::callbacks::BeforeModelResult::Continue(request))
            })
        }))
    }
}

#[cfg(test)]
mod tests {
    use adk_core::{Content, FunctionResponseData, Part};
    use serde_json::{Map, Value, json};

    use super::{DEFAULT_KEEP_RECENT, ELIDE_TOOL_RESULTS_KEY, ToolResultElision};

    fn call(id: &str, name: &str, args: Value) -> Content {
        Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: name.to_owned(),
                args,
                id: Some(id.to_owned()),
                thought_signature: None,
            }],
        }
    }

    fn result(id: &str, name: &str, response: Value) -> Content {
        Content {
            role: "function".to_owned(),
            parts: vec![Part::FunctionResponse {
                function_response: FunctionResponseData::new(name, response),
                id: Some(id.to_owned()),
                annotations: None,
            }],
        }
    }

    fn turn(steps: usize) -> Vec<Content> {
        let mut contents = vec![Content::new("user").with_text("fix the build")];
        for step in 0..steps {
            let id = format!("call-{step}");
            let args = json!({ "path": format!("src/file{step}.rs") });
            contents.push(call(&id, "read_file", args));
            let mut body = json!({ "content": "x".repeat(4_000), "status": "ok" });
            if step == 1 {
                body["output_id"] = json!("out-0123456789abcdef");
            }
            contents.push(result(&id, "read_file", body));
        }
        contents
    }

    fn json_of(contents: &[Content]) -> Value {
        serde_json::to_value(contents).expect("json")
    }

    fn response(content: &Content) -> &Value {
        match &content.parts[0] {
            Part::FunctionResponse {
                function_response, ..
            } => &function_response.response,
            _ => panic!("not a tool result"),
        }
    }

    #[test]
    fn old_tool_results_elided_newest_kept() {
        let elision = ToolResultElision::new(5_000);
        // Under the threshold: nothing changes.
        let mut short = turn(3);
        let before = short.clone();
        assert_eq!(elision.apply(&mut short), 0);
        assert_eq!(json_of(&short), json_of(&before));

        let mut long = turn(10);
        let original = long.clone();
        assert_eq!(elision.apply(&mut long), 10 - DEFAULT_KEEP_RECENT);
        let results: Vec<&Content> = long.iter().filter(|c| c.role == "function").collect();
        for (step, content) in results.iter().enumerate() {
            let body = response(content);
            if step < 10 - DEFAULT_KEEP_RECENT {
                let note = body["elided"].as_str().expect("note");
                assert!(
                    note.starts_with("[result elided after use: read_file"),
                    "{note}"
                );
                assert!(note.contains(&format!("src/file{step}.rs")), "{note}");
                if step == 1 {
                    assert!(note.contains("read_output"), "{note}");
                    assert!(note.contains("out-0123456789abcdef"), "{note}");
                }
            } else {
                assert_eq!(
                    body["content"].as_str().map(str::len),
                    Some(4_000),
                    "{step}"
                );
            }
        }
        // Calls, the person's message and the result ids stay as they were.
        for (shrunk, kept) in long.iter().zip(&original) {
            if shrunk.role != "function" {
                assert_eq!(
                    json_of(std::slice::from_ref(shrunk)),
                    json_of(std::slice::from_ref(kept))
                );
            }
        }
        // Applying again changes nothing more (the notes are small).
        let again = long.clone();
        assert_eq!(elision.apply(&mut long), 0);
        assert_eq!(json_of(&long), json_of(&again));
    }

    #[test]
    fn the_setting_is_absent_by_default_and_checked() {
        assert_eq!(
            ToolResultElision::admit(&Map::new(), 100_000).ok(),
            Some(None)
        );
        let with = |value: Value| {
            let mut settings = Map::new();
            settings.insert(ELIDE_TOOL_RESULTS_KEY.to_owned(), value);
            ToolResultElision::admit(&settings, 100_000)
        };
        assert_eq!(
            with(json!(true)).ok(),
            Some(Some(ToolResultElision::new(60_000)))
        );
        assert_eq!(
            with(json!({ "threshold_tokens": 9, "keep_recent": 2 })).ok(),
            Some(Some(ToolResultElision {
                threshold_tokens: 9,
                keep_recent: 2
            }))
        );
        assert!(with(json!({ "keep_recent": 0 })).is_err());
        assert!(with(json!({ "other": 1 })).is_err());
        assert!(with(json!("yes")).is_err());
    }
}
