//! The admitted profile of a toolkit-internal completion.
//!
//! A toolkit tool that needs a model (the Office `fill_template` and
//! `translate_presentation`, `describe_image`, the step-back index tools) asks
//! for ONE text reply to ONE user message: no tools, no schema, no streaming
//! of its own. The authorized providers admit only the streaming profile of an
//! agent turn (`validate_llm_request`: `stream == true` and a generation
//! config equal to the frozen invocation's), so asking them directly was
//! refused as "outside the admitted ordinary profile" on every live call
//! (`model_gateway.invalid_request`, `tool_count=0`, `streaming=false`).
//!
//! This adapter is that profile, stated once and checked: it admits exactly
//! the shape above, stamps the invocation's own generation config on it, runs
//! it through the provider's streaming path, and folds the stream into the one
//! complete reply the caller asked for. Admission of every other caller is
//! unchanged.
//!
//! Every completion binds its OWN provider model (fresh completion capture,
//! fresh turn counter, one turn): a bound facade is single-use by design
//! (`completion_reused`) and counts against the agent's `max_model_turns`, so
//! a model shared by two completions would fail the second, and one used by
//! many would eat the agent's turn budget. The gateway's own budget
//! accounting still applies to every call.
//!
//! NOTE: the streaming-with-bound-config fold below has a twin in the
//! indexing completion (PR #1291). When that lands, switch to its shared
//! helper and delete this copy of the fold and of `strip_think`.

use std::sync::Arc;

use adk_rust::futures::{StreamExt as _, stream};
use adk_rust::{
    AdkError, Content, ErrorCategory, ErrorComponent, FinishReason, GenerateContentConfig, Llm,
    LlmRequest, LlmResponse, LlmResponseStream, Part,
};
use async_trait::async_trait;

use super::openai_compatible_facade::ModelFacadeInvocation;

/// The most reply text one toolkit completion folds.
const MAX_REPLY_BYTES: usize = 256 * 1024;

/// The reasons a completion is refused after the provider answered. They are
/// the sentences `office_host::model_failure` recognises, so each reaches the
/// person as its own cause.
pub(crate) const REPLY_CUT_AT_LIMIT: &str =
    "the model's reply was cut off at its length limit; raise max_tokens";
pub(crate) const REPLY_CUT_WHILE_REASONING: &str =
    "the model ran out of tokens while reasoning; raise max_tokens";
pub(crate) const REPLY_DECLINED: &str = "the model declined to reply";
pub(crate) const REPLY_EMPTY: &str = "the model's reply held no usable text";

/// Completions one run may make through toolkit tools (step-back, Office,
/// image description share one budget). Mirrors the default of PR #1291's
/// `toolkit_model_call_cap`; switch to its handle when it lands.
pub(crate) const DEFAULT_TOOLKIT_MODEL_CALL_CAP: u32 = 200;

/// The run's budget of toolkit-internal model calls. Clones share it.
#[derive(Clone)]
pub(crate) struct ToolkitCallBudget(Arc<std::sync::atomic::AtomicU32>);

impl ToolkitCallBudget {
    pub(crate) fn new(cap: u32) -> Self {
        Self(Arc::new(std::sync::atomic::AtomicU32::new(cap)))
    }

    fn take(&self) -> bool {
        use std::sync::atomic::Ordering;
        self.0
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |left| {
                left.checked_sub(1)
            })
            .is_ok()
    }
}

/// Binds a fresh provider model for ONE completion.
pub(crate) type FreshModelBinder = Arc<dyn Fn() -> Result<Arc<dyn Llm>, AdkError> + Send + Sync>;

/// A model a toolkit tool can ask for one reply. `bind` runs once per
/// completion; `invocation` is the profile the bound models are built from.
pub(crate) fn toolkit_completion_model(
    bind: FreshModelBinder,
    invocation: &ModelFacadeInvocation,
    budget: ToolkitCallBudget,
) -> Arc<dyn Llm> {
    Arc::new(ToolkitCompletionModel {
        bind,
        budget,
        model_name: invocation.model_name.clone(),
        temperature: invocation.temperature,
        max_output_tokens: invocation
            .max_tokens
            .and_then(|value| i32::try_from(value).ok()),
    })
}

struct ToolkitCompletionModel {
    bind: FreshModelBinder,
    budget: ToolkitCallBudget,
    model_name: String,
    temperature: Option<f32>,
    max_output_tokens: Option<i32>,
}

#[async_trait]
impl Llm for ToolkitCompletionModel {
    fn name(&self) -> &str {
        &self.model_name
    }

    async fn generate_content(
        &self,
        mut request: LlmRequest,
        streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        if streaming
            || request.model != self.model_name
            || request.config.is_some()
            || !request.tools.is_empty()
            || request.previous_response_id.is_some()
            || request.contents.len() != 1
            || request.contents[0].role != "user"
        {
            return Err(refused(
                "toolkit_completion.request",
                "the toolkit completion request is outside its admitted profile",
            ));
        }
        request.config = Some(GenerateContentConfig {
            temperature: self.temperature,
            max_output_tokens: self.max_output_tokens,
            ..GenerateContentConfig::default()
        });
        if !self.budget.take() {
            return Err(unusable(
                "toolkit_completion.budget",
                "the toolkit model call budget is exhausted for this run",
            ));
        }
        let inner = (self.bind)()?;
        let responses = inner.generate_content(request, true).await?;
        let (text, mut response) = fold_reply(responses).await?;
        let text = strip_think(&text);
        if response.error_code.is_some() {
            return Err(unusable(
                "toolkit_completion.finish",
                "the model's reply could not be used: the model ended it abnormally",
            ));
        }
        match response.finish_reason {
            Some(FinishReason::Stop) => {}
            Some(FinishReason::MaxTokens) if text.is_empty() => {
                return Err(unusable(
                    "toolkit_completion.cut_while_reasoning",
                    REPLY_CUT_WHILE_REASONING,
                ));
            }
            Some(FinishReason::MaxTokens) => {
                return Err(unusable("toolkit_completion.cut", REPLY_CUT_AT_LIMIT));
            }
            Some(FinishReason::Safety) => {
                return Err(unusable("toolkit_completion.declined", REPLY_DECLINED));
            }
            _ => {
                return Err(unusable(
                    "toolkit_completion.finish",
                    "the model's reply could not be used: the model ended it abnormally",
                ));
            }
        }
        if text.is_empty() {
            return Err(unusable("toolkit_completion.empty", REPLY_EMPTY));
        }
        response.content = Some(Content::new("model").with_text(text));
        response.partial = false;
        Ok(Box::pin(stream::once(async move { Ok(response) })))
    }
}

/// The stream's text (reasoning parts skipped) and its terminal frame.
async fn fold_reply(mut responses: LlmResponseStream) -> adk_rust::Result<(String, LlmResponse)> {
    let mut text = String::new();
    let mut terminal: Option<LlmResponse> = None;
    while let Some(response) = responses.next().await {
        let response = response?;
        if terminal.is_some() {
            return Err(unusable(
                "toolkit_completion.after_terminal",
                "the model's reply could not be used: it continued after completing",
            ));
        }
        if let Some(content) = &response.content {
            for part in &content.parts {
                match part {
                    Part::Text { text: delta } => {
                        if text.len().saturating_add(delta.len()) > MAX_REPLY_BYTES {
                            return Err(unusable(
                                "toolkit_completion.reply_size",
                                "the model's reply could not be used: it exceeds the toolkit completion bound",
                            ));
                        }
                        text.push_str(delta);
                    }
                    // Provider reasoning is not the reply.
                    Part::Thinking { .. } => {}
                    _ => {
                        return Err(unusable(
                            "toolkit_completion.reply_part",
                            "the model's reply could not be used: it carries a part a toolkit completion cannot use",
                        ));
                    }
                }
            }
        }
        if !response.partial {
            terminal = Some(response);
        }
    }
    let response = terminal.ok_or_else(|| {
        unusable(
            "toolkit_completion.no_terminal",
            "the model's reply could not be used: it never completed",
        )
    })?;
    Ok((text, response))
}

/// `text` without its reasoning. Only a LEADING `<think>…</think>` (after
/// whitespace; an unclosed one makes the whole reply reasoning) is removed, or,
/// when the reply does not open one, everything up to a first bare
/// `</think>` (a server with no reasoning parser leaves one). A tag anywhere
/// else is the model's own text and stays.
pub(crate) fn strip_think(text: &str) -> String {
    const OPEN: &str = "<think>";
    const CLOSE: &str = "</think>";
    let mut rest = text.trim_start();
    let mut opened = false;
    while let Some(after) = rest.strip_prefix(OPEN) {
        opened = true;
        match after.find(CLOSE) {
            Some(close) => rest = after[close + CLOSE.len()..].trim_start(),
            None => return String::new(),
        }
    }
    if !opened
        && let Some(close) = rest.find(CLOSE)
        && !rest[..close].contains(OPEN)
    {
        rest = &rest[close + CLOSE.len()..];
    }
    rest.trim().to_owned()
}

/// The provider answered, but the reply cannot be used (a cause of its own).
fn unusable(code: &'static str, message: &'static str) -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::Internal,
        code,
        message,
    )
}

/// The request itself is outside the admitted profile.
fn refused(code: &'static str, message: &'static str) -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::InvalidInput,
        code,
        message,
    )
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicUsize, Ordering};

    use adk_rust::{Content, GenerateContentConfig, LlmRequest, LlmResponse};

    use super::*;
    use crate::transport::openai_compatible_facade::{
        test_model_facade_invocation, validate_llm_request,
    };

    /// A SINGLE-USE provider, as a bound facade is: it records what reached
    /// it, streams its deltas then a terminal (which carries no text, as the
    /// real facade's does) and refuses a second call.
    struct Provider {
        seen: Mutex<Vec<(LlmRequest, bool)>>,
        deltas: Vec<&'static str>,
        finish: FinishReason,
    }

    impl Provider {
        fn new(deltas: &[&'static str], finish: FinishReason) -> Arc<Self> {
            Arc::new(Self {
                seen: Mutex::new(Vec::new()),
                deltas: deltas.to_vec(),
                finish,
            })
        }
    }

    #[async_trait]
    impl Llm for Provider {
        fn name(&self) -> &'static str {
            "fixture-model"
        }

        async fn generate_content(
            &self,
            request: LlmRequest,
            streaming: bool,
        ) -> adk_rust::Result<LlmResponseStream> {
            let mut seen = self.seen.lock().expect("seen");
            if !seen.is_empty() {
                return Err(refused(
                    "model_gateway.completion_reused",
                    "the model completion was produced more than once",
                ));
            }
            seen.push((request, streaming));
            let mut frames: Vec<adk_rust::Result<LlmResponse>> = self
                .deltas
                .iter()
                .map(|text| {
                    Ok(LlmResponse {
                        content: Some(Content::new("model").with_text(*text)),
                        partial: true,
                        ..LlmResponse::default()
                    })
                })
                .collect();
            frames.push(Ok(LlmResponse {
                finish_reason: Some(self.finish),
                turn_complete: true,
                ..LlmResponse::default()
            }));
            Ok(Box::pin(stream::iter(frames)))
        }
    }

    /// A binder that hands out a new single-use provider per completion and
    /// counts the binds.
    type Made = Arc<Mutex<Vec<Arc<Provider>>>>;

    fn binder(
        deltas: &'static [&'static str],
        finish: FinishReason,
    ) -> (FreshModelBinder, Arc<AtomicUsize>, Made) {
        let binds = Arc::new(AtomicUsize::new(0));
        let made = Arc::new(Mutex::new(Vec::new()));
        let (counter, kept) = (Arc::clone(&binds), Arc::clone(&made));
        let bind: FreshModelBinder = Arc::new(move || {
            counter.fetch_add(1, Ordering::SeqCst);
            let provider = Provider::new(deltas, finish);
            kept.lock().expect("made").push(Arc::clone(&provider));
            Ok(provider as Arc<dyn Llm>)
        });
        (bind, binds, made)
    }

    fn request() -> LlmRequest {
        LlmRequest {
            model: "fixture-model".to_owned(),
            contents: vec![Content::new("user").with_text("Fill the template.")],
            config: None,
            tools: std::collections::HashMap::new(),
            previous_response_id: None,
        }
    }

    async fn run(model: &Arc<dyn Llm>, request: LlmRequest) -> adk_rust::Result<String> {
        let mut responses = model.generate_content(request, false).await?;
        let mut text = String::new();
        while let Some(response) = responses.next().await {
            let response = response?;
            assert!(!response.partial, "the caller sees one complete reply");
            for part in response.content.iter().flat_map(|content| &content.parts) {
                if let Part::Text { text: piece } = part {
                    text.push_str(piece);
                }
            }
        }
        Ok(text)
    }

    #[test]
    fn the_bare_toolkit_request_is_outside_the_ordinary_profile() {
        // The live defect: no config and no streaming.
        let invocation = test_model_facade_invocation();
        assert!(validate_llm_request(&request(), false, &invocation).is_err());
        assert!(validate_llm_request(&request(), true, &invocation).is_err());
    }

    #[tokio::test]
    async fn a_toolkit_completion_reaches_the_provider_inside_the_admitted_profile() {
        let invocation = test_model_facade_invocation();
        let (bind, _, made) = binder(&["Hello, ", "world"], FinishReason::Stop);
        let model = toolkit_completion_model(bind, &invocation, ToolkitCallBudget::new(100));
        assert_eq!(run(&model, request()).await.expect("reply"), "Hello, world");
        let provider = made.lock().expect("made")[0].clone();
        let seen = provider.seen.lock().expect("seen");
        let (sent, streaming) = &seen[0];
        assert!(*streaming);
        // The very check that refused the bare request now admits it.
        assert!(validate_llm_request(sent, *streaming, &invocation).is_ok());
    }

    /// Review round 1: a shared single-use binding failed the second
    /// completion (`completion_reused`) and spent the agent's turns. Each
    /// completion binds its own, so any number in one turn succeed.
    #[tokio::test]
    async fn every_completion_binds_its_own_single_use_model() {
        let invocation = test_model_facade_invocation();
        let (bind, binds, _) = binder(&["ok"], FinishReason::Stop);
        let model = toolkit_completion_model(bind, &invocation, ToolkitCallBudget::new(100));
        for _ in 0..4 {
            assert_eq!(run(&model, request()).await.expect("reply"), "ok");
        }
        assert_eq!(binds.load(Ordering::SeqCst), 4);
    }

    #[tokio::test]
    async fn the_run_budget_is_shared_and_spent_completions_are_refused() {
        let invocation = test_model_facade_invocation();
        let (bind, binds, _) = binder(&["ok"], FinishReason::Stop);
        let budget = ToolkitCallBudget::new(2);
        let first = toolkit_completion_model(bind.clone(), &invocation, budget.clone());
        let second = toolkit_completion_model(bind, &invocation, budget);
        assert!(run(&first, request()).await.is_ok());
        assert!(run(&second, request()).await.is_ok());
        let spent = run(&first, request()).await.expect_err("budget spent");
        assert!(spent.to_string().contains("budget is exhausted"), "{spent}");
        assert_eq!(binds.load(Ordering::SeqCst), 2, "nothing bound once spent");
    }

    #[tokio::test]
    async fn anything_but_one_plain_user_message_is_refused_before_binding() {
        let invocation = test_model_facade_invocation();
        let (bind, binds, _) = binder(&["x"], FinishReason::Stop);
        let model = toolkit_completion_model(bind, &invocation, ToolkitCallBudget::new(100));
        let mut with_config = request();
        with_config.config = Some(GenerateContentConfig::default());
        let mut with_tool = request();
        with_tool
            .tools
            .insert("t".to_owned(), serde_json::json!({"name": "t"}));
        let mut two = request();
        two.contents.push(Content::new("user").with_text("more"));
        let mut other_model = request();
        other_model.model = "other".to_owned();
        for bad in [with_config, with_tool, two, other_model] {
            assert!(run(&model, bad).await.is_err());
        }
        // A streaming ask is not this profile either.
        assert!(model.generate_content(request(), true).await.is_err());
        assert_eq!(binds.load(Ordering::SeqCst), 0);
    }

    async fn failure(deltas: &'static [&'static str], finish: FinishReason) -> String {
        let (bind, _, _) = binder(deltas, finish);
        let model = toolkit_completion_model(
            bind,
            &test_model_facade_invocation(),
            ToolkitCallBudget::new(100),
        );
        run(&model, request())
            .await
            .expect_err("refused")
            .to_string()
    }

    /// Item 5: each cause is its own sentence, which `model_failure` maps to
    /// its own `OfficeFailure`.
    #[tokio::test]
    async fn every_unusable_reply_names_its_own_cause() {
        let cut = failure(&["partial answer"], FinishReason::MaxTokens).await;
        assert!(cut.contains(REPLY_CUT_AT_LIMIT), "{cut}");
        let reasoning = failure(&["<think>still thinking"], FinishReason::MaxTokens).await;
        assert!(reasoning.contains(REPLY_CUT_WHILE_REASONING), "{reasoning}");
        let nothing = failure(&[], FinishReason::MaxTokens).await;
        assert!(nothing.contains(REPLY_CUT_WHILE_REASONING), "{nothing}");
        let declined = failure(&[], FinishReason::Safety).await;
        assert!(declined.contains(REPLY_DECLINED), "{declined}");
        let empty = failure(&["<think>done</think>"], FinishReason::Stop).await;
        assert!(empty.contains(REPLY_EMPTY), "{empty}");
        let odd = failure(&["x"], FinishReason::Other).await;
        assert!(odd.contains("ended it abnormally"), "{odd}");
    }

    #[tokio::test]
    async fn reasoning_before_the_answer_is_not_the_answer() {
        let (bind, _, _) = binder(&["<think>hm", "</think>\n", "Answer"], FinishReason::Stop);
        let model = toolkit_completion_model(
            bind,
            &test_model_facade_invocation(),
            ToolkitCallBudget::new(100),
        );
        assert_eq!(run(&model, request()).await.expect("reply"), "Answer");
    }

    #[test]
    fn only_leading_reasoning_is_stripped() {
        assert_eq!(strip_think("<think>hm</think>\nAnswer"), "Answer");
        assert_eq!(
            strip_think("  <think>a</think><think>b</think> Answer"),
            "Answer"
        );
        assert_eq!(strip_think("<think>only"), "");
        assert_eq!(strip_think("reasoning</think>\nAnswer"), "Answer");
        // Literal tags elsewhere are the model's own text.
        assert_eq!(
            strip_think("Use <think>x</think> tags like this."),
            "Use <think>x</think> tags like this."
        );
        assert_eq!(
            strip_think("<think>r</think>Answer mentions </think> literally"),
            "Answer mentions </think> literally"
        );
        assert_eq!(strip_think("plain"), "plain");
    }
}
