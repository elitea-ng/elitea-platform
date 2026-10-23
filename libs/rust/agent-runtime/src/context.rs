//! Shrinking older tool results inside a turn (#1215 stage 4).
//!
//! Every model step resends every earlier tool result. Once the request
//! the model is about to get passes a threshold of its window (its
//! contents, its tool declarations, and the instruction a transport adds
//! outside them, [`ToolResultElision::with_fixed_tokens`]; estimated with
//! the shared [`crate::context_budget::estimate_tokens`]),
//! [`ToolResultElision`] replaces the bodies of older tool results (all
//! but the newest [`DEFAULT_KEEP_RECENT`]) with a short note naming the
//! tool and its arguments, and the `output_id` when the tool spilled its
//! output, so the model can re-run it or page it with `read_output` (a
//! spilled output is kept for a while, not forever: `output_expired` tells
//! the model to re-run the tool).
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
use adk_core::{Content, LlmRequest, Part};
use serde_json::{Map, Value, json};

use crate::assembly_error::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::context_budget::estimate_tokens;
use crate::request::ModelContextLimits;

/// Tool results always kept whole: the newest this many.
pub const DEFAULT_KEEP_RECENT: usize = 6;

/// The share of the window past which older results are elided.
pub const DEFAULT_THRESHOLD_PERCENT: u64 = 60;

/// The context-settings key ([`ToolResultElision::admit`]):
/// `{"threshold_tokens": n, "keep_recent": n}`, both optional.
pub const ELIDE_TOOL_RESULTS_KEY: &str = "elide_tool_results";

/// The window assumed when the model's limits are not known
/// ([`ToolResultElision::for_limits`] with `None`).
pub const FALLBACK_WINDOW_TOKENS: u64 = 128_000;

/// The longest argument summary a note carries.
const MAX_ARGS_SUMMARY: usize = 160;

/// When and how much of a request's older tool results to elide.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ToolResultElision {
    /// Elide once the request's estimated tokens pass this.
    pub threshold_tokens: u64,
    /// The newest tool results kept whole.
    pub keep_recent: usize,
    /// Estimated tokens every request carries outside its contents and
    /// tool declarations (the system instruction a transport adds).
    pub fixed_tokens: u64,
}

const fn invalid_input() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "native agent context settings are malformed",
    )
}

/// Bytes of one content the estimate counts: its text, call arguments
/// and response bodies.
fn content_bytes(content: &Content) -> usize {
    content
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
        .sum()
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
            fixed_tokens: 0,
        }
    }

    /// Elide past [`DEFAULT_THRESHOLD_PERCENT`] of a `window_tokens` window.
    #[must_use]
    pub const fn for_window(window_tokens: u64) -> Self {
        Self::new(window_tokens / 100 * DEFAULT_THRESHOLD_PERCENT)
    }

    /// [`Self::for_window`] of the model's input window: its context
    /// window, or its `max_input_tokens` when smaller; without limits,
    /// [`FALLBACK_WINDOW_TOKENS`].
    #[must_use]
    pub fn for_limits(limits: Option<ModelContextLimits>) -> Self {
        let window = limits.map_or(FALLBACK_WINDOW_TOKENS, |limits| {
            let window = limits
                .max_input_tokens
                .map_or(limits.context_window_tokens, |input| {
                    input.min(limits.context_window_tokens)
                });
            u64::from(window)
        });
        Self::for_window(window)
    }

    /// Count `tokens` more in every request: what the transport sends
    /// outside the request's contents (the system instruction).
    #[must_use]
    pub const fn with_fixed_tokens(mut self, tokens: u64) -> Self {
        self.fixed_tokens = tokens;
        self
    }

    /// The request's estimated tokens: its contents, its tool
    /// declarations and generation config, and [`Self::fixed_tokens`].
    #[must_use]
    pub fn request_tokens(&self, request: &LlmRequest) -> u64 {
        let contents: usize = request.contents.iter().map(content_bytes).sum();
        let tools: usize = request
            .tools
            .iter()
            .map(|(name, declaration)| name.len() + declaration.to_string().len())
            .sum();
        let config = request
            .config
            .as_ref()
            .and_then(|config| serde_json::to_string(config).ok())
            .map_or(0, |text| text.len());
        estimate_tokens(contents + tools + config).saturating_add(self.fixed_tokens)
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

    /// Shrink the request's contents in place when the request passes the
    /// threshold ([`Self::request_tokens`]): every tool result but the
    /// newest [`Self::keep_recent`] (and any already as small as its note)
    /// becomes a note. The number elided.
    pub fn apply(&self, request: &mut LlmRequest) -> usize {
        let total = self.request_tokens(request);
        if total <= self.threshold_tokens {
            return 0;
        }
        let contents = request.contents.as_mut_slice();
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
                                "page it with read_output {{\"output_id\": \"{id}\"}} while it is kept, or re-run it (output_expired: it is gone)"
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
                self.apply(&mut request);
                Ok(adk_core::callbacks::BeforeModelResult::Continue(request))
            })
        }))
    }
}

#[cfg(test)]
mod tests {
    use adk_core::{Content, FunctionResponseData, LlmRequest, Part};
    use serde_json::{Map, Value, json};

    use super::{
        DEFAULT_KEEP_RECENT, ELIDE_TOOL_RESULTS_KEY, FALLBACK_WINDOW_TOKENS, ToolResultElision,
    };
    use crate::request::ModelContextLimits;

    fn request(contents: Vec<Content>) -> LlmRequest {
        LlmRequest::new("model", contents)
    }

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
        let mut short = request(turn(3));
        let before = short.contents.clone();
        assert_eq!(elision.apply(&mut short), 0);
        assert_eq!(json_of(&short.contents), json_of(&before));

        let mut long = request(turn(10));
        let original = long.contents.clone();
        assert_eq!(elision.apply(&mut long), 10 - DEFAULT_KEEP_RECENT);
        let results: Vec<&Content> = long
            .contents
            .iter()
            .filter(|c| c.role == "function")
            .collect();
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
        for (shrunk, kept) in long.contents.iter().zip(&original) {
            if shrunk.role != "function" {
                assert_eq!(
                    json_of(std::slice::from_ref(shrunk)),
                    json_of(std::slice::from_ref(kept))
                );
            }
        }
        // Applying again changes nothing more (the notes are small).
        let again = long.contents.clone();
        assert_eq!(elision.apply(&mut long), 0);
        assert_eq!(json_of(&long.contents), json_of(&again));
    }

    /// The instruction a transport adds and the tool declarations count
    /// toward the threshold, as the provider request carries them.
    #[test]
    fn instructions_and_tool_declarations_count() {
        let elision = ToolResultElision::new(5_000);
        // Three results of ~1k tokens each: under the threshold alone.
        let contents = turn(8);
        let mut plain = request(contents.clone());
        let bare = elision.request_tokens(&plain);
        assert!(bare < 10_000, "{bare}");
        let small = ToolResultElision::new(bare + 10);
        assert_eq!(small.apply(&mut plain), 0);
        // The same contents with 2k tokens of tool declarations pass it.
        let mut with_tools = request(contents.clone());
        with_tools.tools.insert(
            "read_file".to_owned(),
            json!({ "description": "d".repeat(8_000) }),
        );
        assert!(small.request_tokens(&with_tools) >= bare + 2_000);
        assert_eq!(small.apply(&mut with_tools), 8 - DEFAULT_KEEP_RECENT);
        // And so does a system instruction outside the contents.
        let mut instructed = request(contents);
        let with_instruction = small.with_fixed_tokens(2_000);
        assert_eq!(with_instruction.request_tokens(&instructed), bare + 2_000);
        assert_eq!(
            with_instruction.apply(&mut instructed),
            8 - DEFAULT_KEEP_RECENT
        );
    }

    #[test]
    fn the_window_comes_from_the_model_limits_when_known() {
        assert_eq!(
            ToolResultElision::for_limits(None).threshold_tokens,
            FALLBACK_WINDOW_TOKENS / 100 * 60
        );
        let limits = |window: u32, input: Option<u32>| ModelContextLimits {
            context_window_tokens: window,
            max_output_tokens: 4_096,
            context_window_fallback: false,
            max_output_fallback: false,
            max_input_tokens: input,
        };
        assert_eq!(
            ToolResultElision::for_limits(Some(limits(32_000, None))).threshold_tokens,
            19_200
        );
        assert_eq!(
            ToolResultElision::for_limits(Some(limits(1_000_000, Some(200_000)))).threshold_tokens,
            120_000
        );
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
                keep_recent: 2,
                fixed_tokens: 0,
            }))
        );
        assert!(with(json!({ "keep_recent": 0 })).is_err());
        assert!(with(json!({ "other": 1 })).is_err());
        assert!(with(json!("yes")).is_err());
    }
}
