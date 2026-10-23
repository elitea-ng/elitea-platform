//! Step compaction (#1217): the cut, the iterative summary, the template
//! retry, the overflow retry and the fresh window, on a fake 8k model.

use std::sync::{Arc, Mutex};

use adk_core::{
    Content, FinishReason, FunctionResponseData, Llm, LlmRequest, LlmResponse, LlmResponseStream,
    Part, UsageMetadata,
};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::*;
use crate::context_budget::RequestContextBudget;
use crate::request::ModelContextLimits;

/// Measures the contents' JSON, as a provider body would carry them.
struct JsonBudget(RequestContextBudget);

impl ModelRequestBudget for JsonBudget {
    fn measure(&self, request: &LlmRequest) -> adk_core::Result<RequestContextUsage> {
        let bytes = serde_json::to_vec(&request.contents).unwrap();
        self.0.measure_provider_request(&bytes, 1 << 24)
    }
}

fn budget(window: u32) -> Arc<JsonBudget> {
    let limits = ModelContextLimits {
        context_window_tokens: window,
        max_output_tokens: 1_000,
        context_window_fallback: false,
        max_output_fallback: false,
        max_input_tokens: None,
    };
    Arc::new(JsonBudget(
        RequestContextBudget::resolve(Some(limits), &Map::new(), Some(256))
            .unwrap()
            .unwrap(),
    ))
}

/// A summary model that answers from a script and records what it was asked.
#[derive(Default)]
struct Summarizer {
    answers: Mutex<Vec<String>>,
    asked: Mutex<Vec<LlmRequest>>,
}

impl Summarizer {
    fn answering(answers: &[&str]) -> Arc<Self> {
        Arc::new(Self {
            answers: Mutex::new(answers.iter().rev().map(|a| (*a).to_owned()).collect()),
            asked: Mutex::default(),
        })
    }

    fn asked(&self) -> Vec<LlmRequest> {
        self.asked.lock().unwrap().clone()
    }
}

fn text_response(text: &str) -> LlmResponse {
    LlmResponse {
        content: Some(Content::new("model").with_text(text)),
        finish_reason: Some(FinishReason::Stop),
        turn_complete: true,
        ..LlmResponse::default()
    }
}

#[async_trait]
impl Llm for Summarizer {
    fn name(&self) -> &'static str {
        "summary-model"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        self.asked.lock().unwrap().push(request);
        let answer = self.answers.lock().unwrap().pop().unwrap_or_default();
        Ok(Box::pin(futures::stream::iter([Ok(text_response(
            &answer,
        ))])))
    }
}

#[derive(Default)]
struct Host {
    files: Vec<FileTouch>,
    compactions: Mutex<Vec<Compaction>>,
    phases: Mutex<Vec<ContextPhase>>,
}

#[async_trait]
impl CompactionHost for Host {
    fn files_touched(&self, _folded: &[Content]) -> Vec<FileTouch> {
        self.files.clone()
    }

    fn status(&self, status: &ModelContextStatus) {
        self.phases.lock().unwrap().push(status.phase);
    }

    async fn compacted(&self, compaction: &Compaction) {
        self.compactions.lock().unwrap().push(compaction.clone());
    }
}

fn policy() -> CompactionPolicy {
    CompactionPolicy {
        reserve_tokens: 1_000,
        keep_recent_tokens: 1_000,
        fresh_window: true,
        guidance: String::new(),
    }
}

fn summary(objective: &str) -> String {
    json!({"version":1,"objective":objective,"constraints":[],"decisions":[],
        "key_facts":["The build is red."],"completed_work":[],"open_work":["Fix it."],
        "next_steps":[],"unresolved_issues":[],"references":[]})
    .to_string()
}

fn call(id: &str, path: &str) -> Content {
    Content {
        role: "model".to_owned(),
        parts: vec![Part::FunctionCall {
            name: "read_file".to_owned(),
            args: json!({ "path": path }),
            id: Some(id.to_owned()),
            thought_signature: None,
        }],
    }
}

fn result(id: &str, bytes: usize) -> Content {
    Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: FunctionResponseData::new(
                "read_file",
                json!({ "content": "x".repeat(bytes) }),
            ),
            id: Some(id.to_owned()),
            annotations: None,
        }],
    }
}

/// `turns` turns of `steps` read calls of `bytes` bytes each, then an answer.
fn session(turns: usize, steps: usize, bytes: usize) -> Vec<Content> {
    let mut contents = Vec::new();
    for turn in 0..turns {
        contents.push(Content::new("user").with_text(format!("turn {turn}: read the files")));
        for step in 0..steps {
            let id = format!("call-{turn}-{step}");
            contents.push(call(&id, &format!("src/f{turn}_{step}.rs")));
            contents.push(result(&id, bytes));
        }
        contents.push(Content::new("model").with_text(format!("answer {turn}")));
    }
    contents
}

fn compactor(summarizer: Arc<Summarizer>, host: Arc<Host>, window: u32) -> Arc<StepCompaction> {
    Arc::new(StepCompaction::new(
        policy(),
        budget(window),
        summarizer,
        host,
    ))
}

fn same_all(a: &[Content], b: &[Content]) -> bool {
    a.len() == b.len() && a.iter().zip(b).all(|(a, b)| same_content(a, b))
}

fn tokens(contents: &[Content]) -> u64 {
    contents.iter().map(content_tokens).sum()
}

/// Every result has its call before it, and every call its result.
fn paired(contents: &[Content]) -> bool {
    let mut open = Vec::new();
    for content in contents {
        for part in &content.parts {
            match part {
                Part::FunctionCall { id, .. } => open.push(id.clone()),
                Part::FunctionResponse { id, .. } => {
                    let Some(at) = open.iter().position(|o| o == id) else {
                        return false;
                    };
                    open.remove(at);
                }
                _ => {}
            }
        }
    }
    open.is_empty()
}

#[tokio::test]
async fn keeps_recent_tokens_at_turn_boundary() {
    let summarizer = Summarizer::answering(&[&summary("Fix the build.")]);
    let host = Arc::new(Host::default());
    let compaction = compactor(summarizer.clone(), host.clone(), 8_000);
    // Six turns of three 600-byte reads: about 9k tokens, past the 6k trigger.
    let contents = session(6, 3, 2_000);
    let request = LlmRequest::new("m", contents.clone());
    let prepared = compaction.prepare(request.clone(), false).await.unwrap();
    assert_eq!(summarizer.asked().len(), 1);
    let sent = &prepared.contents;
    assert!(summary_of(&sent[0]).is_some(), "{:?}", sent[0]);
    let kept = &sent[1..];
    // The tail is verbatim, at least the recent budget, and starts a turn.
    let budget = budget(8_000).0;
    assert!(
        tokens(kept) >= policy().keep_recent_for(&budget),
        "{}",
        tokens(kept)
    );
    assert!(is_turn_start(&kept[0]), "{:?}", kept[0]);
    assert!(same_all(kept, &contents[contents.len() - kept.len()..]));
    assert!(paired(kept));
    // The newest turn boundary that keeps the budget: one turn later keeps less.
    let later = contents
        .iter()
        .enumerate()
        .skip(contents.len() - kept.len() + 1)
        .find(|(_, c)| is_turn_start(c))
        .map(|(at, _)| at);
    if let Some(later) = later {
        assert!(tokens(&contents[later..]) < policy().keep_recent_for(&budget));
    }
    // The host learnt what to persist: the covered events and their digest.
    let compactions = host.compactions.lock().unwrap().clone();
    assert_eq!(compactions.len(), 1);
    assert_eq!(compactions[0].covered_events, contents.len() - kept.len());
    assert_eq!(
        compactions[0].events_digest,
        history_digest(&contents[..compactions[0].covered_events])
    );
    assert!(compactions[0].based_on.is_none());
    assert_eq!(compaction.compactions(), 1);
    assert!(prepared.contents.len() < contents.len());
    // Under the trigger, nothing is summarized and nothing changes.
    let short = session(1, 1, 100);
    let fresh = compaction_with(Summarizer::answering(&[]));
    let same = fresh
        .prepare(LlmRequest::new("m", short.clone()), false)
        .await
        .unwrap();
    assert!(same_all(&same.contents, &short));
    assert_eq!(fresh.summary_calls(), 0);
    assert_eq!(fresh.compactions(), 0);
}

#[tokio::test]
async fn the_cut_inside_a_long_turn_never_orphans_a_result() {
    // One turn of many reads: the cut falls between call pairs, and the
    // turn's request is repeated verbatim in the summary content.
    let summarizer = Summarizer::answering(&[&summary("Fix the build.")]);
    let compaction = compactor(summarizer, Arc::new(Host::default()), 8_000);
    let mut contents = session(1, 20, 1_200);
    contents.pop();
    let prepared = compaction
        .prepare(LlmRequest::new("m", contents.clone()), false)
        .await
        .unwrap();
    let kept = &prepared.contents[1..];
    assert!(is_call_step(&kept[0]), "{:?}", kept[0]);
    assert!(paired(kept));
    let head = text_of(&prepared.contents[0]);
    assert!(head.contains("turn 0: read the files"), "{head}");
}

#[tokio::test]
async fn iterative_summary_carries_previous() {
    let first = summary("Make the build green.");
    let renamed = summary("Something else entirely.");
    let summarizer = Summarizer::answering(&[&first, &renamed]);
    let host = Arc::new(Host {
        files: vec![
            FileTouch {
                path: "src/lib.rs".into(),
                op: FileOp::Edit,
            },
            FileTouch {
                path: "Cargo.toml".into(),
                op: FileOp::Read,
            },
        ],
        ..Host::default()
    });
    let compaction = compactor(summarizer.clone(), host.clone(), 8_000);
    let contents = session(6, 3, 2_000);
    let prepared = compaction
        .prepare(LlmRequest::new("m", contents.clone()), false)
        .await
        .unwrap();
    let one = summary_of(&prepared.contents[0]).unwrap();
    assert_eq!(
        context_summary::objective(&one).unwrap(),
        "Make the build green."
    );
    let files = context_summary::files_touched_entry(&one).unwrap();
    assert!(
        files.contains("src/lib.rs") && files.contains("\"edit\""),
        "{files}"
    );
    // The next turn starts from the stored summary and grows past the
    // trigger again: the summarizer is handed the previous summary, and the
    // new one keeps its objective and the files, cumulatively.
    let mut next = prepared.contents.clone();
    next.extend(session(6, 3, 2_000));
    let again = StepCompaction::new(
        policy(),
        budget(8_000),
        summarizer.clone(),
        Arc::new(Host {
            files: vec![FileTouch {
                path: "src/main.rs".into(),
                op: FileOp::Write,
            }],
            ..Host::default()
        }),
    );
    let prepared = again
        .prepare(LlmRequest::new("m", next.clone()), false)
        .await
        .unwrap();
    let asked = summarizer.asked();
    assert_eq!(asked.len(), 2);
    let prompt = text_of(&asked[1].contents[0]);
    assert!(prompt.contains("Previous summary"), "{prompt}");
    assert!(prompt.contains("Make the build green."), "{prompt}");
    let two = summary_of(&prepared.contents[0]).unwrap();
    assert!(two.len() <= context_summary::MAX_SUMMARY_BYTES);
    assert_eq!(
        context_summary::objective(&two).unwrap(),
        "Make the build green."
    );
    let files = context_summary::files_touched_entry(&two).unwrap();
    for path in ["src/lib.rs", "Cargo.toml", "src/main.rs"] {
        assert!(files.contains(path), "{files}");
    }
    // It validates as the shared contract.
    context_summary::validate(&two, &json!(null)).unwrap();
    assert_eq!(again.latest_summary().as_deref(), Some(two.as_str()));
    // The host was told the summary the history started from.
    let based_on = host.compactions.lock().unwrap()[0].summary_content.clone();
    assert!(same_content(&next[0], &based_on));
}

#[tokio::test]
async fn template_retry_without_tools() {
    // The model ignores the template once: it is asked once more, with no
    // tools, and its second answer is the summary.
    let summarizer = Summarizer::answering(&["Here is some prose.", &summary("Fix it.")]);
    let compaction = compactor(summarizer.clone(), Arc::new(Host::default()), 8_000);
    let prepared = compaction
        .prepare(LlmRequest::new("m", session(6, 3, 2_000)), false)
        .await
        .unwrap();
    let asked = summarizer.asked();
    assert_eq!(asked.len(), 2);
    for request in &asked {
        assert!(request.tools.is_empty());
        assert!(request.config.is_none());
    }
    let retry = &asked[1].contents;
    assert_eq!(retry.len(), 3);
    assert_eq!(text_of(&retry[1]), "Here is some prose.");
    assert!(text_of(&retry[2]).contains("did not follow the required template"));
    assert_eq!(
        context_summary::objective(&summary_of(&prepared.contents[0]).unwrap()).unwrap(),
        "Fix it."
    );

    // Twice ignored: the text itself becomes a v1 summary, at most 32 KiB.
    let prose = "The agent read files. ".repeat(5_000);
    let stubborn = Summarizer::answering(&["not json", &prose]);
    let compaction = compaction_with(stubborn.clone());
    let prepared = compaction
        .prepare(LlmRequest::new("m", session(6, 3, 2_000)), false)
        .await
        .unwrap();
    assert_eq!(stubborn.asked().len(), 2);
    let kept = summary_of(&prepared.contents[0]).unwrap();
    assert!(kept.len() <= context_summary::MAX_SUMMARY_BYTES);
    context_summary::validate(&kept, &json!(null)).unwrap();
    assert!(kept.contains("The agent read files."));
    assert_eq!(
        context_summary::objective(&kept).unwrap(),
        "turn 0: read the files"
    );
}

fn compaction_with(summarizer: Arc<Summarizer>) -> Arc<StepCompaction> {
    compactor(summarizer, Arc::new(Host::default()), 8_000)
}

#[tokio::test]
async fn a_summary_is_reused_for_the_rest_of_the_invocation() {
    let summarizer = Summarizer::answering(&[&summary("Fix the build.")]);
    let compaction = compaction_with(summarizer.clone());
    let mut contents = session(6, 3, 2_000);
    let first = compaction
        .prepare(LlmRequest::new("m", contents.clone()), false)
        .await
        .unwrap();
    // The next step's history is the original one plus the new results
    // (adk rebuilds it each step), with an elided body somewhere: the same
    // summary stands for the same prefix, without another call.
    contents.push(Content::new("user").with_text("one more"));
    let mut elided = contents.clone();
    if let Part::FunctionResponse {
        function_response, ..
    } = &mut elided[2].parts[0]
    {
        function_response.response = json!({"elided": "note"});
    }
    let second = compaction
        .prepare(LlmRequest::new("m", elided), false)
        .await
        .unwrap();
    assert_eq!(summarizer.asked().len(), 1);
    assert!(same_content(&second.contents[0], &first.contents[0]));
    assert!(same_content(
        second.contents.last().unwrap(),
        contents.last().unwrap()
    ));
}

#[tokio::test]
async fn system_contents_stay_pinned_and_are_never_folded() {
    let summarizer = Summarizer::answering(&[&summary("Fix the build.")]);
    let compaction = compaction_with(summarizer.clone());
    let mut contents = vec![Content::new("system").with_text("Skill catalogue")];
    contents.extend(session(6, 3, 2_000));
    let prepared = compaction
        .prepare(LlmRequest::new("m", contents), false)
        .await
        .unwrap();
    assert_eq!(text_of(&prepared.contents[0]), "Skill catalogue");
    assert!(summary_of(&prepared.contents[1]).is_some());
    let prompt = text_of(&summarizer.asked()[0].contents[0]);
    assert!(!prompt.contains("Skill catalogue"));
}

/// How [`Overflowing`] runs out of window.
#[derive(Clone, Copy)]
enum Overflow {
    /// The provider refuses the request as too long.
    Refused,
    /// A `length` finish after `tokens` output tokens: reasoning only, or
    /// answer text too.
    Length { text: bool, tokens: i32 },
    /// Some text streams, then the provider fails as too long.
    AfterText,
}

/// A model that runs out of window on the first `overflows` calls, then answers.
struct Overflowing {
    overflows: Mutex<usize>,
    how: Overflow,
    seen: Mutex<Vec<usize>>,
}

impl Overflowing {
    fn new(overflows: usize, how: Overflow) -> Arc<Self> {
        Arc::new(Self {
            overflows: Mutex::new(overflows),
            how,
            seen: Mutex::default(),
        })
    }

    fn calls(&self) -> usize {
        self.seen.lock().unwrap().len()
    }
}

fn length_response(text: bool, tokens: i32) -> LlmResponse {
    let content = if text {
        Content::new("model").with_text("I")
    } else {
        Content {
            role: "model".to_owned(),
            parts: vec![Part::Thinking {
                thinking: "Let me think".to_owned(),
                signature: None,
            }],
        }
    };
    LlmResponse {
        content: Some(content),
        finish_reason: Some(FinishReason::MaxTokens),
        usage_metadata: Some(UsageMetadata {
            prompt_token_count: 7_000,
            candidates_token_count: tokens,
            total_token_count: 7_000 + tokens,
            ..UsageMetadata::default()
        }),
        ..LlmResponse::default()
    }
}

fn too_long() -> adk_core::AdkError {
    adk_core::AdkError::model("model_gateway.context_length_exceeded: the request is too long")
}

#[async_trait]
impl Llm for Overflowing {
    fn name(&self) -> &'static str {
        "agent-model"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        self.seen.lock().unwrap().push(request.contents.len());
        let mut left = self.overflows.lock().unwrap();
        if *left > 0 {
            *left -= 1;
            return match self.how {
                Overflow::Refused => Err(too_long()),
                Overflow::Length { text, tokens } => Ok(Box::pin(futures::stream::iter([Ok(
                    length_response(text, tokens),
                )]))),
                Overflow::AfterText => {
                    let mut partial = text_response("Partial");
                    partial.partial = true;
                    Ok(Box::pin(futures::stream::iter([
                        Ok(partial),
                        Err(too_long()),
                    ])))
                }
            };
        }
        Ok(Box::pin(futures::stream::iter([Ok(text_response("done"))])))
    }
}

async fn answer(model: &Arc<dyn Llm>, contents: Vec<Content>) -> adk_core::Result<String> {
    let mut stream = model
        .generate_content(LlmRequest::new("m", contents), false)
        .await?;
    let mut text = String::new();
    while let Some(response) = stream.next().await {
        text.push_str(&text_of(&response?.content.unwrap()));
    }
    Ok(text)
}

#[tokio::test]
async fn an_overflow_compacts_and_retries_the_step_once() {
    let thinking_only = Overflow::Length {
        text: false,
        tokens: 1,
    };
    for how in [Overflow::Refused, thinking_only] {
        let early_length = matches!(how, Overflow::Length { .. });
        let summarizer = Summarizer::answering(&[&summary("Fix it."), &summary("Fix it.")]);
        let compaction = compaction_with(summarizer.clone());
        let inner = Overflowing::new(1, how);
        let model = compaction.wrap(inner.clone());
        // Under the trigger: the first call goes out whole, overflows, and
        // the retry is compacted.
        let contents = session(3, 2, 600);
        assert_eq!(answer(&model, contents.clone()).await.unwrap(), "done");
        let seen = inner.seen.lock().unwrap().clone();
        assert_eq!(seen.len(), 2, "{early_length}");
        assert_eq!(seen[0], contents.len());
        assert!(seen[1] < contents.len());
        assert_eq!(summarizer.asked().len(), 1);

        // Twice: the turn fails with context_overflow.
        let compaction = compaction_with(Summarizer::answering(&[&summary("Fix it.")]));
        let inner = Overflowing::new(2, how);
        let model = compaction.wrap(inner.clone());
        let error = answer(&model, session(3, 2, 600)).await.unwrap_err();
        assert_eq!(error.code, "context_overflow");
        assert!(error.message.contains("Start a new thread"));
        assert_eq!(inner.seen.lock().unwrap().len(), 2, "exactly one retry");
    }
}

#[tokio::test]
async fn an_attempt_that_carried_text_is_never_retried() {
    // A host may have shown that text as it streamed: a retry would show a
    // second answer after the dropped one.
    let summarizer = Summarizer::answering(&[&summary("Fix it.")]);
    let compaction = compaction_with(summarizer.clone());
    let inner = Overflowing::new(
        1,
        Overflow::Length {
            text: true,
            tokens: 1,
        },
    );
    let model = compaction.wrap(inner.clone());
    assert_eq!(answer(&model, session(3, 2, 600)).await.unwrap(), "I");
    assert_eq!(inner.calls(), 1);
    // An overflow error after some text is the error, not a retry.
    let inner = Overflowing::new(1, Overflow::AfterText);
    let model = compaction.wrap(inner.clone());
    let error = answer(&model, session(3, 2, 600)).await.unwrap_err();
    assert!(is_overflow(&error), "{error:?}");
    assert_eq!(inner.calls(), 1);
    assert_eq!(summarizer.asked().len(), 0);
}

#[tokio::test]
async fn an_answer_that_used_its_own_output_limit_is_not_an_overflow() {
    // The budget's output is 256 tokens: a reasoning-only `length` finish
    // after 200 of them ran out of output, not of window.
    let compaction = compaction_with(Summarizer::answering(&[&summary("Fix it.")]));
    let used = Overflowing::new(
        1,
        Overflow::Length {
            text: false,
            tokens: 200,
        },
    );
    let model = compaction.wrap(used.clone());
    assert_eq!(answer(&model, session(3, 2, 600)).await.unwrap(), "");
    assert_eq!(used.calls(), 1);
    assert_eq!(compaction.summary_calls(), 0);
    // A request's own limit under 512 counts too: 60 of 100 is most of it,
    // 10 of 100 is not.
    for (tokens, calls) in [(60, 1), (10, 2)] {
        let compaction = compaction_with(Summarizer::answering(&[&summary("Fix it.")]));
        let inner = Overflowing::new(
            1,
            Overflow::Length {
                text: false,
                tokens,
            },
        );
        let model = compaction.wrap(inner.clone());
        let mut request = LlmRequest::new("m", session(3, 2, 600));
        request.config = Some(adk_core::GenerateContentConfig {
            max_output_tokens: Some(100),
            ..adk_core::GenerateContentConfig::default()
        });
        let mut stream = model.generate_content(request, false).await.unwrap();
        while stream.next().await.is_some() {}
        assert_eq!(inner.calls(), calls, "{tokens} of 100");
    }
}

#[tokio::test]
async fn a_second_compaction_of_one_turn_keeps_its_request() {
    // One long turn compacts twice: the second summary content still
    // repeats the turn's request, which the first one folded.
    let summarizer = Summarizer::answering(&[&summary("Fix it."), &summary("Fix it.")]);
    let compaction = compaction_with(summarizer.clone());
    let mut contents = session(1, 20, 1_200);
    contents.pop();
    let first = compaction
        .prepare(LlmRequest::new("m", contents.clone()), false)
        .await
        .unwrap();
    assert!(text_of(&first.contents[0]).contains("turn 0: read the files"));
    for step in 20..40 {
        let id = format!("call-0-{step}");
        contents.push(call(&id, &format!("src/g{step}.rs")));
        contents.push(result(&id, 1_200));
    }
    let second = compaction
        .prepare(LlmRequest::new("m", contents), false)
        .await
        .unwrap();
    assert_eq!(summarizer.asked().len(), 2);
    let head = text_of(&second.contents[0]);
    assert!(head.contains("turn 0: read the files"), "{head}");
    assert_eq!(head.matches(CURRENT_REQUEST_HEADER).count(), 1, "{head}");
}

/// A summary model that always fails.
struct FailingSummarizer;

#[async_trait]
impl Llm for FailingSummarizer {
    fn name(&self) -> &'static str {
        "summary-model"
    }

    async fn generate_content(
        &self,
        _request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        Err(adk_core::AdkError::model(
            "model_gateway.unavailable: could not reach the model gateway",
        ))
    }
}

#[tokio::test]
async fn a_failed_summary_sends_what_fits_and_ends_a_retry_as_overflow() {
    // `/compact` of a short history: the request goes out as it is, and
    // the host is told it was measured, not compacted.
    let host = Arc::new(Host::default());
    let compaction = Arc::new(StepCompaction::new(
        policy(),
        budget(8_000),
        Arc::new(FailingSummarizer),
        host.clone(),
    ));
    compaction.force_next();
    let contents = session(3, 1, 200);
    let prepared = compaction
        .prepare(LlmRequest::new("m", contents.clone()), false)
        .await
        .unwrap();
    assert!(same_all(&prepared.contents, &contents));
    assert!(host.compactions.lock().unwrap().is_empty());
    assert_eq!(
        host.phases.lock().unwrap().as_slice(),
        [ContextPhase::Compacting, ContextPhase::Measured]
    );
    // A request that does not fit keeps the summary model's error.
    let error = compaction
        .prepare(LlmRequest::new("m", session(6, 3, 2_000)), false)
        .await
        .unwrap_err();
    assert!(
        error.message.contains("model_gateway.unavailable"),
        "{error:?}"
    );
    // On the overflow retry it ends as context_overflow.
    let inner = Overflowing::new(1, Overflow::Refused);
    let model = compaction.wrap(inner.clone());
    let error = answer(&model, session(3, 2, 600)).await.unwrap_err();
    assert_eq!(error.code, "context_overflow");
    assert_eq!(inner.calls(), 1);
}

#[tokio::test]
async fn a_reseeded_history_does_not_reuse_the_summary() {
    let summarizer = Summarizer::answering(&[&summary("Fix it."), &summary("Fix it.")]);
    let compaction = compaction_with(summarizer.clone());
    let contents = session(6, 3, 2_000);
    compaction
        .prepare(LlmRequest::new("m", contents.clone()), false)
        .await
        .unwrap();
    let mut reseeded = contents;
    reseeded[0] = Content::new("user").with_text("another thread");
    let prepared = compaction
        .prepare(LlmRequest::new("m", reseeded), false)
        .await
        .unwrap();
    assert_eq!(summarizer.asked().len(), 2);
    assert!(summary_of(&prepared.contents[0]).is_some());
}

#[tokio::test]
async fn a_tail_that_does_not_fit_starts_a_fresh_window() {
    // The last call's result alone is past the window: the step starts from
    // the summary, the turn's request and that call with its result cut.
    let summarizer = Summarizer::answering(&[&summary("Fix it.")]);
    let compaction = compaction_with(summarizer);
    let mut contents = session(2, 2, 400);
    contents.push(Content::new("user").with_text("read the big one"));
    contents.push(call("big", "big.log"));
    contents.push(result("big", 40_000));
    let prepared = compaction
        .prepare(LlmRequest::new("m", contents), false)
        .await
        .unwrap();
    let sent = &prepared.contents;
    assert!(summary_of(&sent[0]).is_some());
    assert!(paired(&sent[1..]));
    assert!(budget(8_000).measure(&prepared).unwrap().fits());
    // The turn's request was folded: the summary content repeats it.
    assert!(text_of(&sent[0]).contains("read the big one"));
}

#[tokio::test]
async fn a_forced_compaction_folds_under_the_trigger() {
    let summarizer = Summarizer::answering(&[&summary("Fix it.")]);
    let host = Arc::new(Host::default());
    let compaction = compactor(summarizer.clone(), host.clone(), 8_000);
    compaction.force_next();
    let contents = session(3, 1, 200);
    let prepared = compaction
        .prepare(LlmRequest::new("m", contents.clone()), false)
        .await
        .unwrap();
    assert_eq!(summarizer.asked().len(), 1);
    assert!(summary_of(&prepared.contents[0]).is_some());
    assert!(host.compactions.lock().unwrap()[0].forced);
    assert_eq!(
        host.phases.lock().unwrap().as_slice(),
        [ContextPhase::Compacting, ContextPhase::Compacted]
    );
}

#[test]
fn the_digest_ignores_result_bodies_but_not_the_conversation() {
    let contents = session(1, 2, 100);
    let mut elided = contents.clone();
    if let Part::FunctionResponse {
        function_response, ..
    } = &mut elided[2].parts[0]
    {
        function_response.response = json!({"elided": "x"});
    }
    assert_eq!(history_digest(&contents), history_digest(&elided));
    let mut edited = contents.clone();
    edited[0] = Content::new("user").with_text("another request");
    assert_ne!(history_digest(&contents), history_digest(&edited));
}

#[test]
fn visible_events_start_after_the_marker_the_history_used() {
    use adk_core::{Event, EventCompaction};
    let event = |author: &str, content: Content| {
        let mut event = Event::new("inv");
        author.clone_into(&mut event.author);
        event.llm_response.content = Some(content);
        event
    };
    let first = event("user", Content::new("user").with_text("a"));
    let second = event("agent", Content::new("model").with_text("b"));
    let stored = summary_content(&summary("x"), None);
    let mut marker = Event::new("inv");
    marker.actions.compaction = Some(EventCompaction {
        start_timestamp: first.timestamp,
        end_timestamp: second.timestamp,
        compacted_content: stored.clone(),
    });
    std::thread::sleep(std::time::Duration::from_millis(2));
    let third = event("user", Content::new("user").with_text("c"));
    let events = vec![first, second, marker, third];
    let all = visible_events(&events, "agent", None);
    assert_eq!(all.len(), 3);
    assert_eq!(all[1].1.role, "model");
    let after = visible_events(&events, "agent", Some(&stored));
    assert_eq!(after.len(), 1);
    assert_eq!(text_of(&after[0].1), "c");
    let unknown = summary_content(&summary("y"), None);
    assert!(visible_events(&events, "agent", Some(&unknown)).is_empty());
}

#[test]
fn the_policy_is_opt_in_and_checked() {
    assert_eq!(CompactionPolicy::admit(&Map::new()).ok(), Some(None));
    let with = |value: Value| {
        let mut settings = Map::new();
        settings.insert(SESSION_COMPACTION_KEY.to_owned(), value);
        CompactionPolicy::admit(&settings)
    };
    assert_eq!(
        with(json!(true)).ok(),
        Some(Some(CompactionPolicy::default()))
    );
    let custom =
        with(json!({"reserve_tokens": 2_000, "keep_recent_tokens": 3_000, "fresh_window": false}))
            .unwrap()
            .unwrap();
    assert_eq!(custom.reserve_tokens, 2_000);
    assert_eq!(custom.keep_recent_tokens, 3_000);
    assert!(!custom.fresh_window);
    assert!(with(json!({"other": 1})).is_err());
    assert!(with(json!({"reserve_tokens": 0})).is_err());
    assert!(with(json!("yes")).is_err());
    // The worker admits it and composes it at the root (#1368).
    let mut settings = Map::new();
    settings.insert(SESSION_COMPACTION_KEY.to_owned(), json!(true));
    let plan = ContextManagementPlan::admit_current(&settings, Some("c")).unwrap();
    assert_eq!(
        plan.step_policy(&settings).unwrap(),
        Some(CompactionPolicy::default())
    );
}

#[test]
fn the_worker_step_policy_follows_the_plan() {
    let settings = |pairs: Value| pairs.as_object().unwrap().clone();
    let step = |value: Value| {
        let settings = settings(value);
        ContextManagementPlan::admit_current(&settings, Some("c"))
            .and_then(|plan| plan.step_policy(&settings))
    };
    // Absent, false, or a plan that is off: no step compaction.
    assert_eq!(step(json!({"enabled": true})).unwrap(), None);
    assert_eq!(step(json!({"session_compaction": false})).unwrap(), None);
    assert_eq!(
        step(json!({"enabled": false, "session_compaction": true})).unwrap(),
        None
    );
    assert_eq!(
        step(json!({"enable_summarization": false, "session_compaction": true})).unwrap(),
        None
    );
    // Explicit instructions guide it; the plan's default does not.
    let guided = step(json!({
        "session_compaction": {"keep_recent_tokens": 5_000},
        "summary_instructions": "Keep the file names",
    }))
    .unwrap()
    .unwrap();
    assert_eq!(guided.keep_recent_tokens, 5_000);
    assert_eq!(guided.guidance, "Keep the file names");
    assert_eq!(
        step(json!({"session_compaction": true}))
            .unwrap()
            .unwrap()
            .guidance,
        ""
    );
    // Malformed is refused even when the plan is off.
    assert!(step(json!({"enabled": false, "session_compaction": "yes"})).is_err());
    assert!(step(json!({"session_compaction": {"reserve_tokens": 0}})).is_err());
}

#[test]
fn pi_numbers_and_small_windows() {
    let budget = |window: u32, output: u32| {
        let limits = ModelContextLimits {
            context_window_tokens: window,
            max_output_tokens: output.max(1_000),
            context_window_fallback: false,
            max_output_fallback: false,
            max_input_tokens: None,
        };
        RequestContextBudget::resolve(Some(limits), &Map::new(), Some(output))
            .unwrap()
            .unwrap()
    };
    let pi = CompactionPolicy::default();
    // 128k with a 4k output: window − 16k.
    assert_eq!(pi.trigger_tokens(&budget(128_000, 4_096)), 128_000 - 16_384);
    assert_eq!(pi.keep_recent_for(&budget(128_000, 4_096)), 20_000);
    // A large output reservation is the reserve, within the usable input.
    let large = budget(200_000, 32_000);
    assert_eq!(pi.trigger_tokens(&large), u64::from(large.input_limit));
    assert!(pi.trigger_tokens(&large) <= 200_000 - 32_000);
    // An 8k window keeps three quarters of it, within the usable input.
    let small = budget(8_000, 256);
    assert_eq!(pi.trigger_tokens(&small), 6_000);
    assert_eq!(pi.keep_recent_for(&small), 3_000);
}

#[test]
fn an_image_costs_what_a_provider_bills_not_its_bytes() {
    let big = "z".repeat(2_000_000);
    let tool_image = Content {
        role: "function".into(),
        parts: vec![Part::FunctionResponse {
            function_response: FunctionResponseData::new(
                "read_file",
                json!({"kind": "image", "mime": "image/png", "image_base64": big}),
            ),
            id: Some("c".into()),
            annotations: None,
        }],
    };
    let inline = Content::new("user")
        .with_text("what is this?")
        .with_inline_data("image/png", vec![7; 1_000_000]);
    for content in [&tool_image, &inline] {
        let cost = content_tokens(content);
        assert!(
            (crate::image_result::TOKEN_ESTIMATE..crate::image_result::TOKEN_ESTIMATE + 100)
                .contains(&cost),
            "{cost}"
        );
    }
    // Text is counted as before.
    let text = Content::new("user").with_text("hello");
    assert_eq!(
        content_tokens(&text),
        estimate_tokens(serde_json::to_string(&text).unwrap().len())
    );
}
