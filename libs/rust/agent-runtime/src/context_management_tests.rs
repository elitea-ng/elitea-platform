//! Step compaction (#1217): the cut, the iterative summary, the template
//! retry, the overflow retry and the fresh window, on a fake 8k model.

use std::sync::{Arc, Mutex};

use adk_core::{
    Content, FinishReason, FunctionResponseData, Llm, LlmRequest, LlmResponse, LlmResponseStream,
    Part, UsageMetadata,
};
use async_trait::async_trait;
use futures::StreamExt as _;
use serde_json::{Map, Value, json};

use super::*;
use crate::context_budget::{RequestContextBudget, RequestContextUsage};
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

/// A model that refuses the first `overflows` calls as too long, then answers.
struct Overflowing {
    overflows: Mutex<usize>,
    early_length: bool,
    seen: Mutex<Vec<usize>>,
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
            if self.early_length {
                let response = LlmResponse {
                    content: Some(Content::new("model").with_text("I")),
                    finish_reason: Some(FinishReason::MaxTokens),
                    usage_metadata: Some(UsageMetadata {
                        prompt_token_count: 7_000,
                        candidates_token_count: 1,
                        total_token_count: 7_001,
                        ..UsageMetadata::default()
                    }),
                    ..LlmResponse::default()
                };
                return Ok(Box::pin(futures::stream::iter([Ok(response)])));
            }
            return Err(adk_core::AdkError::model(
                "model_gateway.context_length_exceeded: the request is too long",
            ));
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
    for early_length in [false, true] {
        let summarizer = Summarizer::answering(&[&summary("Fix it."), &summary("Fix it.")]);
        let compaction = compaction_with(summarizer.clone());
        let inner = Arc::new(Overflowing {
            overflows: Mutex::new(1),
            early_length,
            seen: Mutex::default(),
        });
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
        let inner = Arc::new(Overflowing {
            overflows: Mutex::new(2),
            early_length,
            seen: Mutex::default(),
        });
        let model = compaction.wrap(inner.clone());
        let error = answer(&model, session(3, 2, 600)).await.unwrap_err();
        assert_eq!(error.code, "context_overflow");
        assert!(error.message.contains("Start a new thread"));
        assert_eq!(inner.seen.lock().unwrap().len(), 2, "exactly one retry");
    }
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
    // The worker's admission still refuses it: it does not compose it.
    let mut settings = Map::new();
    settings.insert(SESSION_COMPACTION_KEY.to_owned(), json!(true));
    assert!(ContextManagementPlan::admit_current(&settings, Some("c")).is_err());
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
