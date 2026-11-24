#![allow(
    clippy::disallowed_methods,
    clippy::items_after_statements,
    clippy::too_many_lines,
    clippy::unnecessary_literal_bound,
    reason = "inline test fixtures for 5b-2 LLM settings, 5b-3 direct-tool settings, and FU-067"
)]

//! End-to-end unit tests for 5b-2 (LLM per-node settings, pre/post-output
//! classification, Retry-After, timeout, terminal-only model-failure emission,
//! `execute_once`, journal resume) and 5b-3 + FU-067 (direct toolkit/MCP
//! settings, platform read-only classification, effectful Reconcile with 0
//! repeats, sensitive-tool / auth Skip 0-retry, and operator recovery
//! projection).

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::graph::interrupt::Interrupt;
use adk_rust::graph::{ExecutionConfig, GraphError, MemoryCheckpointer, Node, NodeContext, State};
use adk_rust::{
    AdkError, Agent, Content, ErrorCategory, ErrorComponent, Event, EventStream, InvocationContext,
    Tool, ToolContext,
};
use async_trait::async_trait;
use elitea_agent_runtime::graph::node_recovery::{NodeFailureClass, ReplaySafety};
use elitea_agent_runtime::graph::node_settings::{MAX_WAIT_BETWEEN_TRIES_MS, NodeSettings};
use serde_json::{Value, json};

use super::compiler::{
    AdmittedNodeFamily, AdmittedNodeSettings, PipelineDefinition, PipelineNodeRuntimes,
};
use super::direct_tool::{
    DIRECT_TOOL_RECOVERY_RECEIPT_SCHEMA, DIRECT_TOOL_RESUME_STATE_KEY, DirectToolExecutionError,
    DirectToolNode, DirectToolNodeDefinition, DirectToolOwnerReceipt, DirectToolReceiptBinding,
    DirectToolReceiptVisit, DirectToolSelection, PipelineDirectToolResolver, ResolvedDirectTool,
};
use super::llm::{
    LlmExecutionError, LlmExecutionInput, LlmNode, LlmNodeDefinition, PipelineLlmAgentBinding,
    PipelineLlmAgentFactory, PipelineLlmReplayEnvelope, PipelineModelScope,
};
use super::node_events::{PipelineNodeEventSender, pipeline_node_event_channel};
use super::node_recovery_owner::{
    DIRECT_TOOL_RECEIPT_AUDIENCE, NodeRecoveryOwnerProof, NodeRecoveryOwnerProofKind,
    NodeRecoveryResultReference,
};
use super::node_recovery_runtime::tests::direct_tool_tests::scoped;
use super::node_recovery_runtime::{NodeAttemptActivation, NodeRecoveryClock, NodeRecoveryFactory};
use crate::toolkits::{
    SensitiveToolPolicy, ToolAdmissionPolicy, delegated_authorization_error_fixture,
};

struct DeterministicClock {
    now_ms: AtomicU64,
    waits: Mutex<Vec<(u64, u64)>>,
}

impl DeterministicClock {
    fn new(start_ms: u64) -> Arc<Self> {
        Arc::new(Self {
            now_ms: AtomicU64::new(start_ms),
            waits: Mutex::new(Vec::new()),
        })
    }

    fn waits(&self) -> Vec<(u64, u64)> {
        self.waits.lock().expect("waits").clone()
    }
}

#[async_trait]
impl NodeRecoveryClock for DeterministicClock {
    fn now_ms(&self) -> Result<u64, GraphError> {
        Ok(self.now_ms.load(Ordering::SeqCst))
    }

    async fn wait_until(&self, target_ms: u64) -> Result<(), GraphError> {
        let before = self.now_ms.load(Ordering::SeqCst);
        if target_ms > before {
            self.waits.lock().expect("waits").push((before, target_ms));
            self.now_ms.store(target_ms, Ordering::SeqCst);
        }
        Ok(())
    }
}

enum ScriptedLlmAttempt {
    FailBeforeOutput(AdkError),
    EmptyChunkThenFail(AdkError),
    PartialTokenThenFail {
        token: &'static str,
        error: AdkError,
    },
    SlowThenSucceed {
        delay: Duration,
        text: &'static str,
    },
    Succeed(&'static str),
}

struct ScriptedLlmAgent {
    attempts: Arc<Mutex<VecDeque<ScriptedLlmAttempt>>>,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl Agent for ScriptedLlmAgent {
    fn name(&self) -> &str {
        "scripted-llm"
    }
    fn description(&self) -> &str {
        "scripted llm fixture"
    }
    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }
    async fn run(&self, _: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let step = self
            .attempts
            .lock()
            .expect("attempts")
            .pop_front()
            .expect("scripted attempt");
        Ok(Box::pin(async_stream::stream! {
            match step {
                ScriptedLlmAttempt::FailBeforeOutput(error) => {
                    yield Err(error);
                }
                ScriptedLlmAttempt::EmptyChunkThenFail(error) => {
                    let mut empty = Event::new("turn-1");
                    empty.llm_response.partial = true;
                    empty.set_content(Content::new("model").with_text(""));
                    yield Ok(empty);
                    yield Err(error);
                }
                ScriptedLlmAttempt::PartialTokenThenFail { token, error } => {
                    let mut partial = Event::new("turn-1");
                    partial.llm_response.partial = true;
                    partial.set_content(Content::new("model").with_text(token));
                    yield Ok(partial);
                    yield Err(error);
                }
                ScriptedLlmAttempt::SlowThenSucceed { delay, text } => {
                    tokio::time::sleep(delay).await;
                    let mut done = Event::new("turn-1");
                    done.set_content(Content::new("model").with_text(text));
                    yield Ok(done);
                }
                ScriptedLlmAttempt::Succeed(text) => {
                    let mut done = Event::new("turn-1");
                    done.set_content(Content::new("model").with_text(text));
                    yield Ok(done);
                }
            }
        }))
    }
}

struct ScriptedLlmFactory {
    attempts: Arc<Mutex<VecDeque<ScriptedLlmAttempt>>>,
    calls: Arc<AtomicUsize>,
    events: Option<PipelineNodeEventSender>,
}

impl PipelineLlmAgentFactory for ScriptedLlmFactory {
    fn build(
        &self,
        _: &LlmNodeDefinition,
        _: &LlmExecutionInput,
        _: Option<Value>,
        _: Option<&PipelineLlmReplayEnvelope>,
        _: &PipelineModelScope,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        Ok(PipelineLlmAgentBinding::new(
            Arc::new(ScriptedLlmAgent {
                attempts: Arc::clone(&self.attempts),
                calls: Arc::clone(&self.calls),
            }),
            BTreeMap::new(),
        ))
    }

    fn event_sender(&self) -> Option<PipelineNodeEventSender> {
        self.events.clone()
    }
}

fn llm_definition() -> LlmNodeDefinition {
    let pipeline = PipelineDefinition::from_yaml(
        "state:\n  answer: str\nentry_point: ask\nnodes:\n  - id: ask\n    type: llm\n    input_mapping:\n      task: {type: fixed, value: hi}\n    output: [answer]\n    transition: END\n",
    )
    .expect("llm pipeline");
    pipeline.llm_definition_fixture("ask").expect("llm node")
}

fn admitted_settings(
    yaml: &str,
    family: AdmittedNodeFamily,
    outputs: &[(&str, &str)],
) -> AdmittedNodeSettings {
    let settings = NodeSettings::parse(serde_yaml_ng::from_str(yaml).expect("settings yaml"))
        .expect("valid settings");
    AdmittedNodeSettings {
        settings,
        family,
        outputs: outputs
            .iter()
            .map(|(k, t)| ((*k).to_owned(), (*t).to_owned()))
            .collect(),
        router_default: None,
    }
}

fn rate_limited_error(retry_after_ms: Option<u64>) -> AdkError {
    let mut error = AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::RateLimited,
        "model_gateway.rate_limited",
        "the model gateway rate limit was reached",
    );
    error.retry.retry_after_ms = retry_after_ms;
    error
}

fn unavailable_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::Unavailable,
        "model_gateway.unavailable",
        "the model gateway is unavailable",
    )
}

#[tokio::test]
async fn llm_pre_output_retries_honor_retry_after_and_emit_zero_premature_failures() {
    let attempts = Arc::new(Mutex::new(VecDeque::from([
        ScriptedLlmAttempt::FailBeforeOutput(rate_limited_error(Some(7_000))),
        ScriptedLlmAttempt::EmptyChunkThenFail(unavailable_error()),
        ScriptedLlmAttempt::Succeed("FINAL_ANSWER"),
    ])));
    let calls = Arc::new(AtomicUsize::new(0));
    let (sender, receiver) = pipeline_node_event_channel();
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts,
        calls: Arc::clone(&calls),
        events: Some(sender),
    });
    let clock = DeterministicClock::new(10_000);
    let journal = scoped();
    let node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_node_recovery(journal)
    .with_clock(Arc::clone(&clock) as Arc<dyn NodeRecoveryClock>)
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 1000}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-1".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("llm-retry-thread"), 1);
    let output = node.execute(&ctx).await.expect("third try succeeds");
    assert_eq!(output.updates.get("answer"), Some(&json!("FINAL_ANSWER")));
    assert_eq!(calls.load(Ordering::SeqCst), 3);
    // First wait honored Retry-After: 7s over configured 1s; second wait used configured 1s.
    assert_eq!(clock.waits(), vec![(10_000, 17_000), (17_000, 18_000)]);
    // Zero premature model failure frames emitted on the event bridge.
    assert_eq!(receiver.try_model_failure_code().await, None);
}

#[tokio::test]
async fn llm_post_output_failure_never_retries_and_emits_one_terminal_model_failure() {
    let attempts = Arc::new(Mutex::new(VecDeque::from([
        ScriptedLlmAttempt::PartialTokenThenFail {
            token: "partial visible text",
            error: unavailable_error(),
        },
        ScriptedLlmAttempt::Succeed("SHOULD_NEVER_RUN"),
    ])));
    let calls = Arc::new(AtomicUsize::new(0));
    let (sender, receiver) = pipeline_node_event_channel();
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts,
        calls: Arc::clone(&calls),
        events: Some(sender),
    });
    let clock = DeterministicClock::new(1_000);
    let node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_node_recovery(scoped())
    .with_clock(clock)
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-2".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("llm-post-thread"), 1);
    let err = node
        .execute(&ctx)
        .await
        .err()
        .expect("post-output failure must stop");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "post-output must not retry"
    );
    assert_eq!(
        super::agent::model_gateway_code_in_message(&err.to_string()),
        Some("model_gateway.unavailable")
    );
    // Drain streamed partial event, then assert exactly one model-failure signal.
    let mut drain = receiver.drain("inv", "agent", "b").await.expect("drain");
    let mut model_failures = 0usize;
    while let Some(item) = drain.try_recv() {
        if let Err(adk_err) = item {
            assert_eq!(adk_err.code, "model_gateway.unavailable");
            model_failures += 1;
        }
    }
    assert_eq!(model_failures, 1);
}

#[tokio::test]
async fn llm_retry_after_above_cap_stops_typed_without_waiting() {
    let attempts = Arc::new(Mutex::new(VecDeque::from([
        ScriptedLlmAttempt::FailBeforeOutput(rate_limited_error(Some(
            MAX_WAIT_BETWEEN_TRIES_MS + 1,
        ))),
        ScriptedLlmAttempt::Succeed("SHOULD_NEVER_RUN"),
    ])));
    let calls = Arc::new(AtomicUsize::new(0));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts,
        calls: Arc::clone(&calls),
        events: None,
    });
    let clock = DeterministicClock::new(1_000);
    let node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_node_recovery(scoped())
    .with_clock(Arc::clone(&clock) as Arc<dyn NodeRecoveryClock>)
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500, on_error: continue}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-cap".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("llm-cap-thread"), 1);
    let err = node
        .execute(&ctx)
        .await
        .err()
        .expect("Retry-After above cap stops typed and is not continued");
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert!(clock.waits().is_empty(), "must never wait past cap");
    assert_eq!(
        super::agent::model_gateway_code_in_message(&err.to_string()),
        Some("model_gateway.rate_limited")
    );
}

#[tokio::test]
async fn llm_retry_past_execution_deadline_stops_with_no_wait_and_no_continue() {
    let attempts = Arc::new(Mutex::new(VecDeque::from([
        ScriptedLlmAttempt::FailBeforeOutput(unavailable_error()),
        ScriptedLlmAttempt::Succeed("NEVER"),
    ])));
    let calls = Arc::new(AtomicUsize::new(0));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts,
        calls: Arc::clone(&calls),
        events: None,
    });
    let clock = DeterministicClock::new(1_000);
    // Execution deadline expires in 500 ms while wait_between_tries_ms is 2000 ms.
    let deadline = tokio::time::Instant::now() + Duration::from_millis(500);
    let node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_execution_deadline(Some(deadline))
    .with_node_recovery(scoped())
    .with_clock(Arc::clone(&clock) as Arc<dyn NodeRecoveryClock>)
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 2000, on_error: continue}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-deadline".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("llm-deadline-thread"), 1);
    let _ = node
        .execute(&ctx)
        .await
        .err()
        .expect("retry past run deadline stops typed");
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert!(
        clock.waits().is_empty(),
        "must not wait past execution deadline"
    );
}

#[tokio::test]
async fn llm_timeout_retries_then_succeeds_and_continue_writes_node_errors_and_neutral_output() {
    let attempts = Arc::new(Mutex::new(VecDeque::from([
        ScriptedLlmAttempt::SlowThenSucceed {
            delay: Duration::from_secs(2),
            text: "TOO_SLOW",
        },
        ScriptedLlmAttempt::Succeed("FAST_SECOND_TRY"),
    ])));
    let calls = Arc::new(AtomicUsize::new(0));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts,
        calls: Arc::clone(&calls),
        events: None,
    });
    let clock = DeterministicClock::new(5_000);
    let mut admitted = admitted_settings(
        "{retry_on_fail: true, max_tries: 2, wait_between_tries_ms: 100, timeout_ms: 1000}",
        AdmittedNodeFamily::Llm,
        &[("answer", "str")],
    );
    // Keep unit test fast: override timeout_ms to 50 ms on the compiled struct.
    admitted.settings.timeout_ms = Some(50);
    let node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_clock(clock)
    .with_settings(admitted, Some(Arc::new("exec-llm-timeout".to_owned())));
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("llm-timeout-thread"), 1);
    let output = node.execute(&ctx).await.expect("second try succeeds");
    assert_eq!(
        output.updates.get("answer"),
        Some(&json!("FAST_SECOND_TRY"))
    );
    assert_eq!(calls.load(Ordering::SeqCst), 2);

    // Non-retryable timeout with on_error: continue + always_output_data writes
    // pipeline.llm.timeout into node_errors and neutral "" into answer.
    let slow_attempts = Arc::new(Mutex::new(VecDeque::from([
        ScriptedLlmAttempt::SlowThenSucceed {
            delay: Duration::from_secs(2),
            text: "TOO_SLOW",
        },
    ])));
    let (sender, receiver) = pipeline_node_event_channel();
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: slow_attempts,
        calls: Arc::new(AtomicUsize::new(0)),
        events: Some(sender),
    });
    let mut cont = admitted_settings(
        "{on_error: continue, always_output_data: true, timeout_ms: 1000}",
        AdmittedNodeFamily::Llm,
        &[("answer", "str")],
    );
    cont.settings.timeout_ms = Some(50);
    let node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_settings(cont, Some(Arc::new("exec-llm-cont".to_owned())));
    let output = node.execute(&ctx).await.expect("timeout continued");
    assert_eq!(output.updates.get("answer"), Some(&json!("")));
    assert_eq!(
        output.updates["node_errors"][0]["error"]["code"],
        json!("pipeline.llm.timeout")
    );
    assert_eq!(
        output.updates["node_errors"][0]["handled"],
        json!("continue")
    );
    assert_eq!(receiver.try_model_failure_code().await, None);
}

#[tokio::test]
async fn llm_execute_once_runs_exactly_once_across_three_loop_visits() {
    let yaml = r#"
state:
  visits: {type: int, value: 0}
  answer: str
entry_point: bump
nodes:
  - id: bump
    type: state_modifier
    template: "{{ visits + 1 }}"
    input: [visits]
    output: [visits]
    transition: ask
  - id: ask
    type: llm
    input_mapping:
      task: {type: fixed, value: hi}
    output: [answer]
    settings: {execute_once: true}
    transition: choose
  - id: choose
    type: router
    condition: "{{ 'bump' if visits < 3 else 'END' }}"
    input: [visits]
    routes: [bump, END]
    default_output: END
"#;
    let calls = Arc::new(AtomicUsize::new(0));
    let attempts = Arc::new(Mutex::new(VecDeque::from([ScriptedLlmAttempt::Succeed(
        "ONCE_ONLY",
    )])));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts,
        calls: Arc::clone(&calls),
        events: None,
    });
    let definition = PipelineDefinition::from_yaml(yaml).expect("compiles");
    let runtimes =
        PipelineNodeRuntimes::new(Some(factory), None, None).with_execution_id("exec-llm-once");
    let result = definition
        .compile_with_runtime(
            "llm-once-root",
            Arc::new(MemoryCheckpointer::new()),
            None,
            &runtimes,
        )
        .expect("graph")
        .invoke(HashMap::new(), ExecutionConfig::new("llm-once-thread"))
        .await
        .expect("completes");
    assert_eq!(result["visits"], json!(3));
    assert_eq!(result["answer"], json!("ONCE_ONLY"));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}

struct ScriptedDirectTool {
    name: &'static str,
    read_only: bool,
    calls: Arc<AtomicUsize>,
    outcomes: Arc<Mutex<VecDeque<Result<Value, AdkError>>>>,
    delay: Option<Duration>,
}

#[async_trait]
impl Tool for ScriptedDirectTool {
    fn name(&self) -> &str {
        self.name
    }
    fn description(&self) -> &str {
        "scripted direct tool"
    }
    fn is_read_only(&self) -> bool {
        self.read_only
    }
    async fn execute(&self, _: Arc<dyn ToolContext>, _: Value) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        if let Some(delay) = self.delay {
            tokio::time::sleep(delay).await;
        }
        self.outcomes
            .lock()
            .expect("outcomes")
            .pop_front()
            .expect("scripted tool outcome")
    }
}

struct DirectResolver {
    tool: Arc<dyn Tool>,
    sensitive: Option<SensitiveToolPolicy>,
}

impl PipelineDirectToolResolver for DirectResolver {
    fn resolve(
        &self,
        _: &DirectToolSelection,
    ) -> Result<ResolvedDirectTool, DirectToolExecutionError> {
        Ok(ResolvedDirectTool::new(
            Arc::clone(&self.tool),
            self.sensitive.clone(),
        ))
    }
}

fn direct_tool_def(node_type: &str) -> DirectToolNodeDefinition {
    let yaml = format!(
        "id: lookup\ntype: {node_type}\ntoolkit_name: Support\ntool: search_records\ninput_mapping:\n  query: {{type: fixed, value: SENTINEL_ARG}}\ninput: []\noutput: [report]\nstructured_output: true\ntransition: END\n"
    );
    DirectToolNodeDefinition::from_yaml(&yaml).expect("direct tool def")
}

#[tokio::test]
async fn platform_read_only_native_toolkit_retries_transient_errors_and_succeeds() {
    let calls = Arc::new(AtomicUsize::new(0));
    let outcomes = Arc::new(Mutex::new(VecDeque::from([
        Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Unavailable,
            "tool.unavailable",
            "upstream unavailable",
        )),
        Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::RateLimited,
            "tool.rate_limited",
            "rate limited",
        )),
        Ok(json!({"report": {"ok": true}})),
    ])));
    let tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&calls),
        outcomes,
        delay: None,
    });
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool,
        sensitive: None,
    });
    let clock = DeterministicClock::new(2_000);
    let node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        resolver,
    )
    .with_clock(Arc::clone(&clock) as Arc<dyn NodeRecoveryClock>)
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 250}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-tk-ro".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("tk-ro-thread"), 1);
    let output = node.execute(&ctx).await.expect("third try succeeds");
    assert_eq!(output.updates.get("report"), Some(&json!({"ok": true})));
    assert_eq!(calls.load(Ordering::SeqCst), 3);
    assert_eq!(clock.waits(), vec![(2_000, 2_250), (2_250, 2_500)]);
}

#[tokio::test]
async fn mcp_read_only_hint_is_not_trusted_for_retry_and_reconciles_with_zero_repeats() {
    let calls = Arc::new(AtomicUsize::new(0));
    let outcomes = Arc::new(Mutex::new(VecDeque::from([
        Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Unavailable,
            "mcp.unavailable",
            "transient mcp error",
        )),
        Ok(json!({"report": {"should_never_run": true}})),
    ])));
    // Remote MCP server claims read_only = true, which the platform MUST NOT trust.
    let tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&calls),
        outcomes,
        delay: None,
    });
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool,
        sensitive: None,
    });
    let node = DirectToolNode::new(
        direct_tool_def("mcp"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        resolver,
    )
    .with_node_recovery(scoped())
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100, on_error: continue}",
            AdmittedNodeFamily::Mcp,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-mcp-reconcile".to_owned())),
    );
    let ctx = NodeContext::new(
        State::new(),
        ExecutionConfig::new("mcp-reconcile-thread"),
        1,
    );
    let output = node
        .execute(&ctx)
        .await
        .expect("reconcile card returned as dynamic interrupt");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "MCP tool must never auto-retry"
    );
    let Some(Interrupt::Dynamic {
        data: Some(data), ..
    }) = output.interrupt
    else {
        panic!(
            "expected pipeline_node_recovery interrupt, got {:?}",
            output.interrupt
        );
    };
    assert_eq!(data["guardrail_type"], "pipeline_node_recovery");
    assert_eq!(
        data["receipt"]["stop_reason"],
        "effect_reconciliation_required"
    );
    assert_eq!(data["receipt"]["allowed_actions"], json!(["reconcile"]));
}

#[tokio::test]
async fn effectful_toolkit_timeout_reconciles_with_zero_repeats_even_with_retry_and_continue() {
    let calls = Arc::new(AtomicUsize::new(0));
    let outcomes = Arc::new(Mutex::new(VecDeque::from([
        Ok(json!({"report": {"created": true}})),
        Ok(json!({"report": {"duplicate": true}})),
    ])));
    let tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: false,
        calls: Arc::clone(&calls),
        outcomes,
        delay: Some(Duration::from_secs(2)),
    });
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool,
        sensitive: None,
    });
    let mut admitted = admitted_settings(
        "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100, on_error: continue, timeout_ms: 1000}",
        AdmittedNodeFamily::Toolkit,
        &[("report", "dict")],
    );
    admitted.settings.timeout_ms = Some(40);
    let node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        resolver,
    )
    .with_node_recovery(scoped())
    .with_settings(admitted, Some(Arc::new("exec-tk-timeout".to_owned())));
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("tk-timeout-thread"), 1);
    let output = node
        .execute(&ctx)
        .await
        .expect("effectful timeout escalates to Reconcile");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "0 repeats on effectful timeout"
    );
    let Some(Interrupt::Dynamic {
        data: Some(data), ..
    }) = output.interrupt
    else {
        panic!("expected pipeline_node_recovery interrupt");
    };
    assert_eq!(data["guardrail_type"], "pipeline_node_recovery");
    assert_eq!(data["receipt"]["failure_class"], "attempt_timeout");
    assert_eq!(
        data["receipt"]["stop_reason"],
        "effect_reconciliation_required"
    );
}

#[tokio::test]
async fn sensitive_tool_rejection_and_mcp_auth_skip_never_retry() {
    // 1) Sensitive-tool reject/block: 0 retries, 0 tool calls.
    let calls = Arc::new(AtomicUsize::new(0));
    let tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&calls),
        outcomes: Arc::new(Mutex::new(VecDeque::new())),
        delay: None,
    });
    let config = json!({
        "toolkit_security": {
            "sensitive_tools": {"support": ["search_records"]},
            "sensitive_action_company_name": "Example Corp"
        }
    });
    let policy = ToolAdmissionPolicy::from_runtime_config(config.as_object().expect("cfg"))
        .expect("policy")
        .sensitive_tool("support", "Support", "search_records")
        .expect("sensitive");
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool,
        sensitive: Some(policy),
    });
    let node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        resolver,
    )
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 5, wait_between_tries_ms: 100, on_error: continue}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-sensitive-0".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("sensitive-thread"), 1);
    let pause = node.execute(&ctx).await.expect("sensitive pause");
    let Some(Interrupt::Dynamic {
        data: Some(pause_data),
        ..
    }) = pause.interrupt
    else {
        panic!("expected sensitive pause");
    };
    let mut resume_state = State::new();
    resume_state.insert(
        DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
        json!({
            "lookup": {
                "definition_digest": pause_data["definition_digest"],
                "tool_call_id": pause_data["tool_call_id"],
                "argument_digest": pause_data["argument_digest"],
                "action": "reject"
            }
        }),
    );
    let blocked = node
        .execute(&NodeContext::new(
            resume_state,
            ExecutionConfig::new("sensitive-thread"),
            1,
        ))
        .await
        .expect("blocked cleanly");
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    assert!(blocked.updates.contains_key("_pipeline_blocked"));
    assert!(!blocked.updates.contains_key("node_errors"));

    // 2) MCP authorization Skip: 0 retries after challenge, 0 continued node_errors.
    let mcp_calls = Arc::new(AtomicUsize::new(0));
    let mcp_tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&mcp_calls),
        outcomes: Arc::new(Mutex::new(VecDeque::from([Err(
            delegated_authorization_error_fixture("mcp"),
        )]))),
        delay: None,
    });
    let mcp_resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool: mcp_tool,
        sensitive: None,
    });
    let mcp_node = DirectToolNode::new(
        direct_tool_def("mcp"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        mcp_resolver,
    )
    .with_node_recovery(scoped())
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 5, wait_between_tries_ms: 100, on_error: continue}",
            AdmittedNodeFamily::Mcp,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-skip-0".to_owned())),
    );
    let auth_ctx = NodeContext::new(State::new(), ExecutionConfig::new("mcp-skip-thread"), 1);
    let auth_pause = mcp_node.execute(&auth_ctx).await.expect("mcp auth pause");
    assert_eq!(
        mcp_calls.load(Ordering::SeqCst),
        1,
        "auth challenge does not retry"
    );
    let Some(Interrupt::Dynamic {
        data: Some(auth_data),
        ..
    }) = auth_pause.interrupt
    else {
        panic!("expected mcp auth pause");
    };
    let mut skip_state = State::new();
    skip_state.insert(
        DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
        json!({
            "lookup": {
                "definition_digest": auth_data["definition_digest"],
                "tool_call_id": auth_data["tool_call_id"],
                "argument_digest": auth_data["argument_digest"],
                "action": "skip"
            }
        }),
    );
    let skipped = mcp_node
        .execute(&NodeContext::new(
            skip_state,
            ExecutionConfig::new("mcp-skip-thread"),
            1,
        ))
        .await
        .expect("skip stops cleanly");
    assert_eq!(
        mcp_calls.load(Ordering::SeqCst),
        1,
        "skip must not invoke tool"
    );
    assert!(skipped.updates.contains_key("_pipeline_blocked"));
    assert!(!skipped.updates.contains_key("node_errors"));
}

#[tokio::test]
async fn read_only_toolkit_continue_records_safe_node_error_without_leaking_arguments() {
    let calls = Arc::new(AtomicUsize::new(0));
    let tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&calls),
        outcomes: Arc::new(Mutex::new(VecDeque::from([Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "tool.invalid",
            "SENTINEL_SECRET_DETAIL",
        ))]))),
        delay: None,
    });
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool,
        sensitive: None,
    });
    let node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        resolver,
    )
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100, on_error: continue, always_output_data: true}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-tk-cont".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("tk-cont-thread"), 1);
    let output = node.execute(&ctx).await.expect("continued cleanly");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "InvalidInput is not retried"
    );
    assert_eq!(output.updates.get("report"), Some(&json!({})));
    let entry = &output.updates["node_errors"][0];
    assert_eq!(entry["error"]["code"], json!("pipeline.toolkit.failed"));
    assert_eq!(entry["handled"], json!("continue"));
    assert!(!entry.to_string().contains("SENTINEL"));
}

#[test]
fn fu067_direct_tool_recovery_spec_and_committed_result_projector_fail_closed() {
    let yaml = "
state:
  report: dict
entry_point: lookup
nodes:
  - id: lookup
    type: toolkit
    toolkit_name: Support
    tool: search_records
    input_mapping:
      query: {type: fixed, value: ticket-42}
    output: [report]
    structured_output: true
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500}
    transition: END
";
    let definition = PipelineDefinition::from_yaml(yaml).expect("compiles");
    let (digest, policy) = definition
        .node_recovery_spec("lookup")
        .expect("FU-067: direct tool node_recovery_spec exists");
    let expected_policy = NodeSettings::parse(
        serde_yaml_ng::from_str("{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500}")
            .expect("yaml"),
    )
    .expect("settings")
    .to_recovery_policy()
    .expect("policy");
    assert_eq!(policy, expected_policy);

    let execution_id = "1".repeat(32);
    let projector = definition
        .node_result_projector("lookup", &execution_id, 2)
        .expect("projector builds")
        .expect("FU-067: direct tool projector exists");
    assert_eq!(projector.name(), "lookup");

    // Verify digest stability: pipelines without settings or with empty/default settings keep identical digests.
    let base_yaml = yaml.replace(
        "    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500}\n",
        "",
    );
    let empty_settings_yaml = yaml.replace(
        "    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500}\n",
        "    settings: {}\n",
    );
    let default_settings_yaml = yaml.replace(
        "    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500}\n",
        "    settings: {on_error: stop}\n",
    );
    let base_digest = PipelineDefinition::from_yaml(&base_yaml)
        .expect("base")
        .definition_digest();
    assert_eq!(
        PipelineDefinition::from_yaml(&empty_settings_yaml)
            .expect("empty")
            .definition_digest(),
        base_digest
    );
    assert_eq!(
        PipelineDefinition::from_yaml(&default_settings_yaml)
            .expect("default")
            .definition_digest(),
        base_digest
    );
    assert_ne!(definition.definition_digest(), base_digest);

    // Exercise projector via real NodeAttemptJournal owner-result resume (FU-067).
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("direct-proj-thread"), 1);
    let activation = NodeAttemptActivation::from_context("lookup", digest, &ctx).expect("act");
    let factory = scoped();
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("rt");
    rt.block_on(async {
        let journal = factory.open(&activation, &policy).await.expect("open");
        let snap = journal.load_for_tests().await.expect("load");
        let started = snap.ledger_for_tests().start_attempt(1_000).expect("start");
        let snap = journal
            .append_for_tests(&snap, started, None)
            .await
            .expect("append start");
        let dispatch_activation = {
            let mut h = ring::digest::Context::new(&ring::digest::SHA256);
            h.update(b"elitea.pipeline.node-attempt-dispatch.v1\0");
            h.update(&journal.activation_id_for_tests());
            h.update(&1u16.to_be_bytes());
            let mut out = [0u8; 32];
            out.copy_from_slice(h.finish().as_ref());
            out
        };
        let failed = snap
            .ledger_for_tests()
            .record_failure(
                elitea_agent_runtime::graph::node_recovery::NodeFailure::new(
                    NodeFailureClass::WorkerInterrupted,
                    ReplaySafety::UnknownExternalEffect {
                        effect_id: dispatch_activation,
                    },
                ),
                1_100,
            )
            .expect("fail");
        let snap = journal
            .append_for_tests(&snap, failed, None)
            .await
            .expect("append fail");
        let receipt = super::node_recovery_receipt::NodeRecoveryRequiredReceipt::from_ledger(
            &activation,
            snap.ledger_for_tests(),
        )
        .expect("receipt");
        let receipt_sha =
            crate::transport::input_content::node_recovery_receipt_sha256(&receipt).expect("sha");
        let call_id = "pipeline:lookup:1";
        let args = json!({"query": "ticket-42"});
        let arg_digest = {
            let encoded = serde_json::to_vec(&args).expect("args");
            let mut h = ring::digest::Context::new(&ring::digest::SHA256);
            h.update(b"elitea.graph.direct_tool.arguments.v1\0");
            for field in [
                call_id.as_bytes(),
                b"search_records".as_slice(),
                encoded.as_slice(),
            ] {
                h.update(&(field.len() as u64).to_be_bytes());
                h.update(field);
            }
            format!(
                "sha256:{}",
                crate::sandbox::code_recovery::hex(h.finish().as_ref())
            )
        };
        let owner_receipt = DirectToolOwnerReceipt::CommittedResult {
            schema: DIRECT_TOOL_RECOVERY_RECEIPT_SCHEMA.to_owned(),
            binding: DirectToolReceiptBinding {
                execution_id: execution_id.clone(),
                original_generation: 2,
                dispatch_activation: crate::sandbox::code_recovery::hex(&dispatch_activation),
                node_digest: crate::sandbox::code_recovery::hex(&digest),
                toolkit_alias: "Support".to_owned(),
                tool_name: "search_records".to_owned(),
                argument_digest: arg_digest.clone(),
            },
            visit: DirectToolReceiptVisit {
                activation_id: receipt.activation_id.clone(),
                attempt: 1,
                expected_revision: receipt.journal_revision,
                node_id: "lookup".to_owned(),
                graph_thread: "direct-proj-thread".to_owned(),
                step: 1,
                receipt_sha256: receipt_sha.clone(),
            },
            result: json!({"report": {"projected": true}}),
        };
        let wire = serde_json::to_vec(&owner_receipt).expect("wire");
        let dispatch_hex = crate::sandbox::code_recovery::hex(&dispatch_activation);
        let wire_sha = crate::sandbox::code_recovery::sha256(&wire);
        let proof = NodeRecoveryOwnerProof {
            schema: "elitea.pipeline.node-recovery-owner-proof.v1".to_owned(),
            kind: NodeRecoveryOwnerProofKind::CommittedResult,
            execution_id: execution_id.clone(),
            generation: 2,
            activation_id: receipt.activation_id.clone(),
            expected_revision: receipt.journal_revision,
            attempt: 1,
            effect_id: dispatch_hex.clone(),
            owner_receipt_sha256: wire_sha.clone(),
            result_ref: Some(NodeRecoveryResultReference {
                content_id: dispatch_hex.clone(),
                immutable_version: wire_sha.clone(),
                digest_sha256: wire_sha,
                byte_length: wire.len() as u64,
                media_type: "application/json".to_owned(),
                required_grant_audience: DIRECT_TOOL_RECEIPT_AUDIENCE.to_owned(),
            }),
            owner_receipt_ref: None,
        };
        let req = elitea_agent_runtime::graph::node_recovery::OperatorRetryRequest {
            request_id: [9; 32],
            activation_id: journal.activation_id_for_tests(),
            expected_revision: receipt.journal_revision,
        };
        struct AllowOwner;
        #[async_trait]
        impl super::node_recovery_runtime::NodeRecoveryOwnerAuthorizer for AllowOwner {
            async fn authorize_committed_result(
                &self,
                _: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
                _: &elitea_agent_runtime::graph::node_recovery::OperatorRetryRequest,
                _: &NodeRecoveryOwnerProof,
            ) -> Result<(), GraphError> {
                Ok(())
            }
        }
        let applied = journal
            .resume_owner_result(
                &activation,
                &ctx,
                req,
                &proof,
                &wire,
                projector.as_ref(),
                &AllowOwner,
                1_200,
            )
            .await
            .expect("FU-067 committed result projection succeeds");
        assert!(applied.matches(req));
        let final_snap = journal.load_for_tests().await.expect("final");
        assert_eq!(
            final_snap
                .updates_for_tests()
                .expect("updates")
                .get("report"),
            Some(&json!({"projected": true}))
        );

        let auth = super::node_recovery_runtime::NodeAttemptAuthority::for_test_owner_projection(
            &activation,
            journal.activation_id_for_tests(),
            1,
            dispatch_activation,
            crate::sandbox::code_recovery::decode_hex_32(&receipt_sha).expect("sha"),
        );

        // Forged argument_digest must fail closed.
        let mut forged = owner_receipt;
        if let DirectToolOwnerReceipt::CommittedResult {
            ref mut binding, ..
        } = forged
        {
            binding.argument_digest = "sha256:forged".to_owned();
        }
        let forged_wire = serde_json::to_vec(&forged).expect("forged wire");
        let forged_sha = crate::sandbox::code_recovery::sha256(&forged_wire);
        let mut forged_proof = proof.clone();
        forged_proof.owner_receipt_sha256 = forged_sha.clone();
        if let Some(ref mut r) = forged_proof.result_ref {
            r.immutable_version = forged_sha.clone();
            r.digest_sha256 = forged_sha;
            r.byte_length = forged_wire.len() as u64;
        }
        assert!(
            projector
                .project_committed_result(&ctx, &auth, &forged_proof, &forged_wire)
                .is_err()
        );

        // Cross-family audience (Code receipt audience) must fail closed on DirectTool projector.
        let mut wrong_audience_proof = proof;
        if let Some(ref mut r) = wrong_audience_proof.result_ref {
            r.required_grant_audience =
                crate::sandbox::code_recovery::CODE_RESULT_AUDIENCE.to_owned();
        }
        assert!(
            projector
                .project_committed_result(&ctx, &auth, &wrong_audience_proof, &wire)
                .is_err()
        );

        // VerifiedNoEffect receipt succeeds with valid binding and fails closed on tampered binding.
        let no_effect_receipt = DirectToolOwnerReceipt::VerifiedNoEffect {
            schema: DIRECT_TOOL_RECOVERY_RECEIPT_SCHEMA.to_owned(),
            binding: DirectToolReceiptBinding {
                execution_id: execution_id.clone(),
                original_generation: 2,
                dispatch_activation: dispatch_hex.clone(),
                node_digest: crate::sandbox::code_recovery::hex(&digest),
                toolkit_alias: "Support".to_owned(),
                tool_name: "search_records".to_owned(),
                argument_digest: arg_digest,
            },
            visit: DirectToolReceiptVisit {
                activation_id: receipt.activation_id.clone(),
                attempt: 1,
                expected_revision: receipt.journal_revision,
                node_id: "lookup".to_owned(),
                graph_thread: "direct-proj-thread".to_owned(),
                step: 1,
                receipt_sha256: receipt_sha.clone(),
            },
        };
        let no_effect_wire = serde_json::to_vec(&no_effect_receipt).expect("no-effect wire");
        let no_effect_sha = crate::sandbox::code_recovery::sha256(&no_effect_wire);
        let no_effect_proof = NodeRecoveryOwnerProof {
            schema: "elitea.pipeline.node-recovery-owner-proof.v1".to_owned(),
            kind: NodeRecoveryOwnerProofKind::VerifiedNoEffect,
            execution_id,
            generation: 2,
            activation_id: receipt.activation_id.clone(),
            expected_revision: receipt.journal_revision,
            attempt: 1,
            effect_id: dispatch_hex.clone(),
            owner_receipt_sha256: no_effect_sha.clone(),
            result_ref: None,
            owner_receipt_ref: Some(NodeRecoveryResultReference {
                content_id: dispatch_hex,
                immutable_version: no_effect_sha.clone(),
                digest_sha256: no_effect_sha,
                byte_length: no_effect_wire.len() as u64,
                media_type: "application/json".to_owned(),
                required_grant_audience: DIRECT_TOOL_RECEIPT_AUDIENCE.to_owned(),
            }),
        };
        projector
            .verify_no_effect_receipt(&ctx, &auth, &no_effect_proof, &no_effect_wire)
            .expect("valid verified-no-effect receipt must succeed");

        let mut bad_no_effect = no_effect_receipt;
        if let DirectToolOwnerReceipt::VerifiedNoEffect { ref mut visit, .. } = bad_no_effect {
            visit.attempt = 2;
        }
        let bad_wire = serde_json::to_vec(&bad_no_effect).expect("bad wire");
        let bad_sha = crate::sandbox::code_recovery::sha256(&bad_wire);
        let mut bad_proof = no_effect_proof;
        bad_proof.owner_receipt_sha256 = bad_sha.clone();
        if let Some(ref mut r) = bad_proof.owner_receipt_ref {
            r.immutable_version = bad_sha.clone();
            r.digest_sha256 = bad_sha;
            r.byte_length = bad_wire.len() as u64;
        }
        assert!(
            projector
                .verify_no_effect_receipt(&ctx, &auth, &bad_proof, &bad_wire)
                .is_err(),
            "mismatched attempt in VerifiedNoEffect receipt must fail closed"
        );
    });
}

#[tokio::test]
async fn effectful_direct_tool_pre_dispatch_deadline_stops_with_zero_calls_and_in_flight_deadline_reconciles_with_zero_repeats()
 {
    // Part 1 (SEC-5B2-01): pre-dispatch expired deadline -> 0 tool calls, Stop (NoExternalEffect).
    let pre_calls = Arc::new(AtomicUsize::new(0));
    let pre_tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: false,
        calls: Arc::clone(&pre_calls),
        outcomes: Arc::new(Mutex::new(VecDeque::from([Ok(
            json!({"report": {"ok": 1}}),
        )]))),
        delay: None,
    });
    let pre_resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool: pre_tool,
        sensitive: None,
    });
    let pre_node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        pre_resolver,
    )
    .with_node_recovery(scoped())
    .with_execution_deadline(Some(tokio::time::Instant::now() - Duration::from_secs(1)))
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500, on_error: continue}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-dt-pre-deadline".to_owned())),
    );
    let pre_ctx = NodeContext::new(
        State::new(),
        ExecutionConfig::new("dt-pre-deadline-thread"),
        1,
    );
    let pre_err = pre_node
        .execute(&pre_ctx)
        .await
        .err()
        .expect("pre-dispatch expired deadline stops typed and does not continue");
    assert_eq!(
        pre_calls.load(Ordering::SeqCst),
        0,
        "must not dispatch after deadline"
    );
    assert!(
        pre_err
            .to_string()
            .contains("deadline expired before the tool call started"),
        "unexpected error: {pre_err}"
    );

    // Part 2 (SEC-5B2-01): in-flight execution deadline expiry on effectful tool -> 1 call, Reconcile, 0 repeats.
    let inflight_calls = Arc::new(AtomicUsize::new(0));
    let inflight_tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: false,
        calls: Arc::clone(&inflight_calls),
        outcomes: Arc::new(Mutex::new(VecDeque::from([
            Ok(json!({"report": {"too_late": true}})),
            Ok(json!({"report": {"should_never_repeat": true}})),
        ]))),
        delay: Some(Duration::from_secs(2)),
    });
    let inflight_resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool: inflight_tool,
        sensitive: None,
    });
    let recovery: Arc<dyn NodeRecoveryFactory> = scoped();
    let inflight_node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        inflight_resolver,
    )
    .with_node_recovery(Arc::clone(&recovery))
    .with_execution_deadline(Some(tokio::time::Instant::now() + Duration::from_millis(80)))
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 500, timeout_ms: 5000, on_error: continue}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-dt-inflight-deadline".to_owned())),
    );
    let inflight_ctx = NodeContext::new(
        State::new(),
        ExecutionConfig::new("dt-inflight-deadline-thread"),
        1,
    );
    let first = inflight_node
        .execute(&inflight_ctx)
        .await
        .expect("in-flight deadline expiry on effectful tool must escalate to Reconcile");
    assert_eq!(inflight_calls.load(Ordering::SeqCst), 1);
    assert!(matches!(
        first.interrupt,
        Some(Interrupt::Dynamic { data: Some(ref d), .. })
            if d["guardrail_type"] == "pipeline_node_recovery"
    ));

    // Re-entering the same step on the same journal still Reconciles with 0 additional calls.
    let second = inflight_node
        .execute(&inflight_ctx)
        .await
        .expect("re-entry stays in Reconcile");
    assert_eq!(
        inflight_calls.load(Ordering::SeqCst),
        1,
        "must never repeat effectful tool"
    );
    assert!(matches!(
        second.interrupt,
        Some(Interrupt::Dynamic { data: Some(ref d), .. })
            if d["guardrail_type"] == "pipeline_node_recovery"
    ));
}

#[test]
fn code_committed_projector_verify_no_effect_rejects_direct_tool_audience() {
    use crate::sandbox::code_recovery::{
        CODE_RESULT_AUDIENCE, CodeRecoveryVisit, WholeCodeBinding, WholeCodeRecoveryReceipt,
        original_job_key,
    };

    let yaml = "
state:
  answer: str
entry_point: calc
nodes:
  - id: calc
    type: code
    language: python
    code:
      type: fixed
      value: \"return 'ok'\"
    input: []
    output: [answer]
    transition: END
";
    let definition = PipelineDefinition::from_yaml(yaml).expect("code pipeline");
    let code_def = definition
        .original_code_definition_fixture("calc")
        .expect("code node");
    let execution_id = "2".repeat(32);
    let projector = definition
        .node_result_projector("calc", &execution_id, 1)
        .expect("projector builds")
        .expect("code projector exists");

    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("code-aud-thread"), 1);
    let digest = code_def.validated_digest();
    let activation = NodeAttemptActivation::from_context("calc", digest, &ctx).expect("act");
    let scoped_activation = [7u8; 32];
    let dispatch_activation = [8u8; 32];
    let receipt_sha = [9u8; 32];
    let dispatch_hex = crate::sandbox::code_recovery::hex(&dispatch_activation);
    let activation_hex = crate::sandbox::code_recovery::hex(&scoped_activation);

    let binding = WholeCodeBinding {
        schema: "elitea.sandbox.whole-code-binding.v1".to_owned(),
        purpose: "whole_code_execute".to_owned(),
        execution_id: execution_id.clone(),
        original_generation: 1,
        dispatch_activation: dispatch_hex.clone(),
        job_key: original_job_key(&execution_id, &dispatch_hex),
        request_digest: "4".repeat(64),
        supervisor_audience: "dns:original.supervisor".to_owned(),
        node_digest: crate::sandbox::code_recovery::hex(&digest),
        activation_id: activation_hex.clone(),
        node_id: "calc".to_owned(),
        graph_thread: "code-aud-thread".to_owned(),
        step: 1,
        attempt: 2,
        language: "python".to_owned(),
        prepared_job_sha256: "3".repeat(64),
        source_sha256: crate::sandbox::code_recovery::sha256(b"return 'ok'"),
        input_sha256: crate::sandbox::code_recovery::sha256(b"{}"),
    };
    let visit = CodeRecoveryVisit {
        activation_id: activation_hex.clone(),
        node_id: "calc".to_owned(),
        graph_thread: "code-aud-thread".to_owned(),
        step: 1,
        attempt: 2,
        expected_revision: 2,
        receipt_sha256: crate::sandbox::code_recovery::hex(&receipt_sha),
    };
    let owner = WholeCodeRecoveryReceipt::sealed_no_effect(binding, visit).expect("sealed");
    let wire = owner.canonical_bytes().expect("wire");
    let wire_sha = crate::sandbox::code_recovery::sha256(&wire);
    let valid_proof = NodeRecoveryOwnerProof {
        schema: "elitea.pipeline.node-recovery-owner-proof.v1".to_owned(),
        kind: NodeRecoveryOwnerProofKind::VerifiedNoEffect,
        execution_id,
        generation: 1,
        activation_id: activation_hex,
        expected_revision: 2,
        attempt: 2,
        effect_id: dispatch_hex.clone(),
        owner_receipt_sha256: wire_sha.clone(),
        result_ref: None,
        owner_receipt_ref: Some(NodeRecoveryResultReference {
            content_id: dispatch_hex,
            immutable_version: wire_sha.clone(),
            digest_sha256: wire_sha,
            byte_length: wire.len() as u64,
            media_type: "application/json".to_owned(),
            required_grant_audience: CODE_RESULT_AUDIENCE.to_owned(),
        }),
    };
    let auth = super::node_recovery_runtime::NodeAttemptAuthority::for_test_owner_projection(
        &activation,
        scoped_activation,
        2,
        dispatch_activation,
        receipt_sha,
    );
    // Positive control: exact CODE_RESULT_AUDIENCE succeeds.
    projector
        .verify_no_effect_receipt(&ctx, &auth, &valid_proof, &wire)
        .expect("CODE_RESULT_AUDIENCE must pass");

    // Negative control (SEC-5B2-02): flipping ONLY audience to DIRECT_TOOL_RECEIPT_AUDIENCE fails closed.
    let mut wrong_audience_proof = valid_proof;
    if let Some(ref mut r) = wrong_audience_proof.owner_receipt_ref {
        r.required_grant_audience = DIRECT_TOOL_RECEIPT_AUDIENCE.to_owned();
    }
    assert!(
        projector
            .verify_no_effect_receipt(&ctx, &auth, &wrong_audience_proof, &wire)
            .is_err(),
        "SEC-5B2-02: CodeCommittedProjector::verify_no_effect_receipt must reject DIRECT_TOOL_RECEIPT_AUDIENCE"
    );
}

#[tokio::test]
async fn saved_child_pipeline_runtimes_preserve_parent_execution_deadline() {
    // SEC-5B2-03: verify PipelineNodeRuntimes carries execution_deadline into compiled
    // subgraph nodes (LlmNode and DirectToolNode) and clamps retries to the deadline.
    let deadline = tokio::time::Instant::now() + Duration::from_millis(250);
    let attempts = Arc::new(Mutex::new(VecDeque::from([
        ScriptedLlmAttempt::FailBeforeOutput(unavailable_error()),
        ScriptedLlmAttempt::Succeed("SHOULD_NOT_RUN"),
    ])));
    let calls = Arc::new(AtomicUsize::new(0));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts,
        calls: Arc::clone(&calls),
        events: None,
    });
    let runtimes = PipelineNodeRuntimes::new(Some(factory), None, None)
        .with_execution_id("exec-child-deadline")
        .with_execution_deadline(deadline);
    assert_eq!(runtimes.execution_deadline_for_tests(), Some(deadline));

    let child_yaml = "
state:
  answer: str
entry_point: ask
nodes:
  - id: ask
    type: llm
    input_mapping:
      task: {type: fixed, value: hi}
    output: [answer]
    settings:
      retry_on_fail: true
      max_tries: 3
      wait_between_tries_ms: 5000
      on_error: continue
    transition: END
";
    let child_def = PipelineDefinition::from_yaml(child_yaml).expect("child pipeline");
    let subgraph = child_def
        .compile_subgraph_with_runtime(Arc::new(MemoryCheckpointer::new()), &runtimes)
        .expect("compiles child subgraph");
    let err = subgraph
        .invoke(
            HashMap::new(),
            ExecutionConfig::new("child-deadline-thread"),
        )
        .await
        .expect_err("child retry exceeding parent execution deadline stops typed");
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert!(
        !err.to_string().is_empty(),
        "typed stop error must be returned"
    );
}

#[tokio::test]
async fn journaled_direct_tool_settled_no_external_effect_matches_live_and_restore() {
    // SEC-5B2-04(a): on a journaled effectful DirectToolNode with `on_error: continue`,
    // a NoExternalEffect stop (`argument_digest`) settles Ok(output) both live and on
    // journal restore without being overwritten by stale `control = Some(Err(_))`.
    let calls = Arc::new(AtomicUsize::new(0));
    let tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: false,
        calls: Arc::clone(&calls),
        outcomes: Arc::new(Mutex::new(VecDeque::new())),
        delay: None,
    });
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool,
        sensitive: None,
    });
    let def = DirectToolNodeDefinition::from_yaml(
        "id: lookup\ntype: toolkit\ntoolkit_name: Support\ntool: search_records\ninput_mapping:\n  query: {type: variable, value: deep_arg}\ninput: [deep_arg]\noutput: [report]\nstructured_output: true\ntransition: END\n",
    )
    .expect("direct tool def");
    let recovery: Arc<dyn NodeRecoveryFactory> = scoped();
    let node = DirectToolNode::new(
        def,
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        resolver,
    )
    .with_node_recovery(Arc::clone(&recovery))
    .with_settings(
        admitted_settings(
            "{on_error: continue, always_output_data: true}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-dt-settled-parity".to_owned())),
    );
    // Nest 70 levels deep (> 64) so `argument_digest` rejects before tool.execute().
    let mut deep = json!("leaf");
    for _ in 0..70 {
        deep = json!({"nested": deep});
    }
    let mut state = State::new();
    state.insert("deep_arg".to_owned(), deep);
    let ctx = NodeContext::new(state, ExecutionConfig::new("dt-settled-parity-thread"), 1);
    let live = node
        .execute(&ctx)
        .await
        .expect("live NoExternalEffect failure settles Ok under on_error: continue");
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    assert!(live.interrupt.is_none());
    let live_errors = live
        .updates
        .get("node_errors")
        .and_then(Value::as_array)
        .expect("live node_errors");
    assert_eq!(live_errors.len(), 1);
    assert_eq!(live_errors[0]["error"]["code"], "pipeline.toolkit.failed");

    // Re-entering on the same journal restores the identical settled failure.
    let restored = node
        .execute(&ctx)
        .await
        .expect("restored NoExternalEffect failure settles identically");
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    assert_eq!(
        restored.updates.get("node_errors"),
        live.updates.get("node_errors")
    );
}

#[tokio::test]
async fn read_only_direct_tool_preserves_64_entry_node_errors_cap_error_output_and_execute_once() {
    // Part 1 (SEC-5B2-04(b)): 65th node_errors entry preserves GraphError("pipeline.node_errors.exhausted").
    let calls = Arc::new(AtomicUsize::new(0));
    let tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&calls),
        outcomes: Arc::new(Mutex::new(VecDeque::from([Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "tool.bad_arg",
            "bad arg",
        ))]))),
        delay: None,
    });
    let resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool,
        sensitive: None,
    });
    let node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        resolver,
    )
    .with_settings(
        admitted_settings(
            "{on_error: continue}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-dt-cap64".to_owned())),
    );
    let mut state = State::new();
    state.insert(
        "node_errors".to_owned(),
        Value::Array(vec![json!({"code": "existing"}); 64]),
    );
    let ctx = NodeContext::new(state, ExecutionConfig::new("dt-cap64-thread"), 1);
    let err = node
        .execute(&ctx)
        .await
        .err()
        .expect("65th node_errors entry must fail closed");
    assert!(
        matches!(err, GraphError::Other(ref code) if code == "pipeline.node_errors.exhausted"),
        "expected GraphError::Other(pipeline.node_errors.exhausted), got: {err:?}"
    );

    // Part 2 (SEC-5B2-04(b) & SEC-5B2-07): on_error: error_output routes to error_transition.
    let err_calls = Arc::new(AtomicUsize::new(0));
    let err_tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&err_calls),
        outcomes: Arc::new(Mutex::new(VecDeque::from([Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "tool.bad_arg",
            "bad arg",
        ))]))),
        delay: None,
    });
    let err_resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool: err_tool,
        sensitive: None,
    });
    let err_node = DirectToolNode::new(
        direct_tool_def("toolkit"),
        BTreeMap::from([("report".to_owned(), "dict".to_owned())]),
        err_resolver,
    )
    .with_settings(
        admitted_settings(
            "{on_error: error_output, error_transition: fallback}",
            AdmittedNodeFamily::Toolkit,
            &[("report", "dict")],
        ),
        Some(Arc::new("exec-dt-err-route".to_owned())),
    );
    let routed = err_node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("dt-err-route-thread"),
            1,
        ))
        .await
        .expect("error_output routes cleanly");
    assert_eq!(routed.goto, Some(vec!["fallback".to_owned()]));
    assert_eq!(
        routed.updates["node_errors"]
            .as_array()
            .expect("errors")
            .len(),
        1
    );

    // Part 3 (SEC-5B2-04(b) & SEC-5B2-07): execute_once runs read-only direct tool once across 3 visits.
    let once_calls = Arc::new(AtomicUsize::new(0));
    let once_tool: Arc<dyn Tool> = Arc::new(ScriptedDirectTool {
        name: "search_records",
        read_only: true,
        calls: Arc::clone(&once_calls),
        outcomes: Arc::new(Mutex::new(VecDeque::from([
            Ok(json!({"report": {"visit": 1}})),
            Ok(json!({"report": {"visit": 2}})),
        ]))),
        delay: None,
    });
    let once_resolver: Arc<dyn PipelineDirectToolResolver> = Arc::new(DirectResolver {
        tool: once_tool,
        sensitive: None,
    });
    let once_yaml = "
state:
  report: dict
  visits: int
entry_point: lookup
nodes:
  - id: lookup
    type: toolkit
    toolkit_name: Support
    tool: search_records
    input_mapping:
      query: {type: fixed, value: q}
    output: [report]
    structured_output: true
    settings: {execute_once: true}
    transition: bump
  - id: bump
    type: state_modifier
    template: '{{ (visits | default(0)) + 1 }}'
    input: [visits]
    output: [visits]
    transition: check
  - id: check
    type: router
    condition: \"{{ 'lookup' if visits < 3 else 'END' }}\"
    input: [visits]
    routes: [lookup, END]
    default_output: END
";
    let once_def = PipelineDefinition::from_yaml(once_yaml).expect("execute_once pipeline");
    let runtimes = PipelineNodeRuntimes::new(None, Some(once_resolver), None)
        .with_execution_id("exec-dt-once");
    let result = once_def
        .compile_with_runtime(
            "dt-once-root",
            Arc::new(MemoryCheckpointer::new()),
            None,
            &runtimes,
        )
        .expect("graph")
        .invoke(HashMap::new(), ExecutionConfig::new("dt-once-thread"))
        .await
        .expect("completes");
    assert_eq!(result["visits"], json!(3));
    assert_eq!(result["report"], json!({"visit": 1}));
    assert_eq!(once_calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn tool_free_llm_journal_restore_preserves_typed_model_gateway_class_and_local_errors_never_emit_model_failure()
 {
    // Part 1 (SEC-5B2-05): restored terminal RateLimited stop preserves typed
    // `model_gateway.rate_limited` GraphError AND emits `model_gateway.rate_limited`.
    let def = llm_definition();
    let recovery: Arc<dyn NodeRecoveryFactory> = scoped();
    let (sender, receiver) = pipeline_node_event_channel();
    let calls = Arc::new(AtomicUsize::new(0));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: Arc::new(Mutex::new(VecDeque::from([
            ScriptedLlmAttempt::FailBeforeOutput(rate_limited_error(None)),
            ScriptedLlmAttempt::Succeed("NEVER"),
        ]))),
        calls: Arc::clone(&calls),
        events: Some(sender),
    });
    let node = LlmNode::new(
        def,
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_node_recovery(Arc::clone(&recovery))
    .with_settings(
        admitted_settings(
            "{on_error: stop}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-restore".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("llm-restore-thread"), 1);
    let live_err = node
        .execute(&ctx)
        .await
        .err()
        .expect("live rate_limited stop");
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        super::agent::model_gateway_code_in_message(&live_err.to_string()),
        Some("model_gateway.rate_limited")
    );
    // Drain the live terminal model failure before restoring.
    assert_eq!(
        receiver.try_model_failure_code().await,
        Some("model_gateway.rate_limited")
    );

    // Re-enter the same step on the same journal -> restored stop must preserve
    // `model_gateway.rate_limited` and invoke 0 new model calls.
    let restored_err = node
        .execute(&ctx)
        .await
        .err()
        .expect("restored rate_limited stop");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "restore must not re-run model"
    );
    assert_eq!(
        super::agent::model_gateway_code_in_message(&restored_err.to_string()),
        Some("model_gateway.rate_limited")
    );
    assert_eq!(
        receiver.try_model_failure_code().await,
        Some("model_gateway.rate_limited")
    );

    // Part 2 (SEC-5B2-05): local non-model error (InvalidStructuredOutput) exits
    // cleanly without looping and emits ZERO spurious `model_gateway.incomplete_stream`.
    let structured_yaml = "
state:
  answer: dict
entry_point: ask
nodes:
  - id: ask
    type: llm
    input_mapping:
      task: {type: fixed, value: hi}
    output: [answer]
    structured_output: true
    transition: END
";
    let structured_def = PipelineDefinition::from_yaml(structured_yaml)
        .expect("structured pipeline")
        .llm_definition_fixture("ask")
        .expect("ask node");
    let (local_sender, local_receiver) = pipeline_node_event_channel();
    let local_calls = Arc::new(AtomicUsize::new(0));
    let local_factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: Arc::new(Mutex::new(VecDeque::from([ScriptedLlmAttempt::Succeed(
            "not valid json",
        )]))),
        calls: Arc::clone(&local_calls),
        events: Some(local_sender),
    });
    let local_node = LlmNode::new(
        structured_def,
        BTreeMap::from([("answer".to_owned(), "dict".to_owned())]),
        local_factory,
    )
    .with_node_recovery(scoped())
    .with_settings(
        admitted_settings(
            "{on_error: stop}",
            AdmittedNodeFamily::Llm,
            &[("answer", "dict")],
        ),
        Some(Arc::new("exec-llm-local-err".to_owned())),
    );
    let local_err = local_node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("llm-local-err-thread"),
            1,
        ))
        .await
        .err()
        .expect("invalid structured output stops");
    assert_eq!(local_calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        super::agent::model_gateway_code_in_message(&local_err.to_string()),
        None,
        "local error must not masquerade as model_gateway.*"
    );
    assert_eq!(
        local_receiver.try_model_failure_code().await,
        None,
        "local non-model error must emit 0 send_model_failure signals"
    );
}

#[tokio::test]
async fn tool_bearing_llm_with_settings_supports_hitl_resume_and_never_retries_may_have_started() {
    // SEC-5B2-07: tool-bearing LlmNode with `settings:` uses the in-memory ledger
    // (never poisoning a step journal across HITL resume) and never retries or
    // swallows `MayHaveStarted`.
    let yaml = "
state:
  answer: str
entry_point: ask
nodes:
  - id: ask
    type: llm
    input_mapping:
      task: {type: fixed, value: hi}
    tool_names:
      Support: [search_records]
    output: [answer]
    transition: END
";
    let def = PipelineDefinition::from_yaml(yaml)
        .expect("tool-bearing llm pipeline")
        .llm_definition_fixture("ask")
        .expect("ask node");
    let recovery: Arc<dyn NodeRecoveryFactory> = scoped();
    let calls = Arc::new(AtomicUsize::new(0));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: Arc::new(Mutex::new(VecDeque::from([
            ScriptedLlmAttempt::FailBeforeOutput(unavailable_error()),
            ScriptedLlmAttempt::Succeed("RESUMED_OK"),
        ]))),
        calls: Arc::clone(&calls),
        events: None,
    });
    let clock = DeterministicClock::new(1_000);
    let node = LlmNode::new(
        def.clone(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_node_recovery(Arc::clone(&recovery))
    .with_clock(Arc::clone(&clock) as Arc<dyn NodeRecoveryClock>)
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 200, on_error: continue}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-tools".to_owned())),
    );
    let ctx = NodeContext::new(State::new(), ExecutionConfig::new("llm-tools-thread"), 1);
    let out = node
        .execute(&ctx)
        .await
        .expect("pre-output retry succeeds on tool-bearing LLM");
    assert_eq!(calls.load(Ordering::SeqCst), 2);
    assert_eq!(out.updates.get("answer"), Some(&json!("RESUMED_OK")));

    // Confirm step journal was NOT touched (empty -> HITL resume at same step stays clean).
    let activation =
        NodeAttemptActivation::from_context("ask", def.config_digest(), &ctx).expect("act");
    let journal = recovery
        .open(
            &activation,
            &elitea_agent_runtime::graph::node_recovery::NodeRecoveryPolicy::default(),
        )
        .await
        .expect("open");
    assert!(
        journal
            .load_for_tests()
            .await
            .expect("load")
            .ledger_for_tests()
            .history()
            .is_empty(),
        "tool-bearing LLM must not write to RecoverableNode step journal"
    );

    // MayHaveStarted on a tool-bearing LLM never retries and never settles under on_error: continue.
    struct MayHaveStartedFactory(Arc<AtomicUsize>);
    impl PipelineLlmAgentFactory for MayHaveStartedFactory {
        fn build(
            &self,
            _: &LlmNodeDefinition,
            _: &LlmExecutionInput,
            _: Option<Value>,
            _: Option<&PipelineLlmReplayEnvelope>,
            _: &PipelineModelScope,
        ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
            self.0.fetch_add(1, Ordering::SeqCst);
            Err(LlmExecutionError::MayHaveStarted)
        }
    }
    let mhs_calls = Arc::new(AtomicUsize::new(0));
    let mhs_node = LlmNode::new(
        def,
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        Arc::new(MayHaveStartedFactory(Arc::clone(&mhs_calls))),
    )
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 200, on_error: continue}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-mhs".to_owned())),
    );
    let mhs_err = mhs_node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("llm-mhs-thread"),
            1,
        ))
        .await
        .err()
        .expect("MayHaveStarted must stop");
    assert_eq!(
        mhs_calls.load(Ordering::SeqCst),
        1,
        "MayHaveStarted must never retry"
    );
    assert!(
        mhs_err
            .to_string()
            .contains(crate::agents::pipeline::scoped_runtime::CALL_MAY_HAVE_STARTED_CODE),
        "expected may_have_started code, got: {mhs_err}"
    );
}

#[tokio::test]
async fn llm_on_error_error_output_routes_to_error_transition() {
    // SEC-5B2-07: on_error: error_output routes LlmNode failure to error_transition.
    let calls = Arc::new(AtomicUsize::new(0));
    let factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: Arc::new(Mutex::new(VecDeque::from([
            ScriptedLlmAttempt::FailBeforeOutput(unavailable_error()),
        ]))),
        calls: Arc::clone(&calls),
        events: None,
    });
    let node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        factory,
    )
    .with_node_recovery(scoped())
    .with_settings(
        admitted_settings(
            "{on_error: error_output, error_transition: recover}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-err-route".to_owned())),
    );
    let out = node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("llm-err-route-thread"),
            1,
        ))
        .await
        .expect("error_output settles and routes");
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(out.goto, Some(vec!["recover".to_owned()]));
    let errors = out.updates["node_errors"].as_array().expect("node_errors");
    assert_eq!(errors.len(), 1);
    assert_eq!(errors[0]["error"]["code"], "pipeline.llm.unavailable");
}

#[tokio::test]
async fn llm_node_emits_recovery_refused_model_failure_on_may_have_started() {
    use crate::protocol::output::{RECOVERY_REFUSED_DETAIL, RECOVERY_REFUSED_UPSTREAM_CODE};

    let refused = || {
        AdkError::new(
            ErrorComponent::Agent,
            ErrorCategory::Unsupported,
            RECOVERY_REFUSED_UPSTREAM_CODE,
            RECOVERY_REFUSED_DETAIL,
        )
    };

    // 1) LlmNode without `settings:`: returns `may_have_started` and emits `RECOVERY_REFUSED_UPSTREAM_CODE`.
    let plain_calls = Arc::new(AtomicUsize::new(0));
    let (plain_sender, plain_receiver) = pipeline_node_event_channel();
    let plain_factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: Arc::new(Mutex::new(VecDeque::from([
            ScriptedLlmAttempt::FailBeforeOutput(refused()),
        ]))),
        calls: Arc::clone(&plain_calls),
        events: Some(plain_sender),
    });
    let plain_node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        plain_factory,
    );
    let plain_err = plain_node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("llm-plain-refused-thread"),
            1,
        ))
        .await
        .err()
        .expect("MayHaveStarted stops");
    assert_eq!(plain_calls.load(Ordering::SeqCst), 1);
    assert!(
        plain_err
            .to_string()
            .contains(crate::agents::pipeline::scoped_runtime::CALL_MAY_HAVE_STARTED_CODE),
        "expected may_have_started code, got: {plain_err}"
    );
    assert_eq!(
        plain_receiver.try_model_failure_code().await,
        Some(RECOVERY_REFUSED_UPSTREAM_CODE)
    );

    // 2) Tool-bearing LlmNode with retry settings (`execute_in_memory_ledger`): 0 retries, emits once.
    let tool_yaml = "
state:
  answer: str
entry_point: ask
nodes:
  - id: ask
    type: llm
    input_mapping:
      task: {type: fixed, value: hi}
    tool_names:
      Support: [search_records]
    output: [answer]
    transition: END
";
    let tool_def = PipelineDefinition::from_yaml(tool_yaml)
        .expect("tool-bearing llm pipeline")
        .llm_definition_fixture("ask")
        .expect("ask node");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let (tool_sender, tool_receiver) = pipeline_node_event_channel();
    let tool_factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: Arc::new(Mutex::new(VecDeque::from([
            ScriptedLlmAttempt::FailBeforeOutput(refused()),
            ScriptedLlmAttempt::Succeed("NEVER_RETRY"),
        ]))),
        calls: Arc::clone(&tool_calls),
        events: Some(tool_sender),
    });
    let tool_node = LlmNode::new(
        tool_def,
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        tool_factory,
    )
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 200, on_error: continue}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-tool-refused".to_owned())),
    );
    let tool_err = tool_node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("llm-tool-refused-thread"),
            1,
        ))
        .await
        .err()
        .expect("MayHaveStarted stops tool-bearing node");
    assert_eq!(tool_calls.load(Ordering::SeqCst), 1);
    assert!(
        tool_err
            .to_string()
            .contains(crate::agents::pipeline::scoped_runtime::CALL_MAY_HAVE_STARTED_CODE),
        "expected may_have_started code, got: {tool_err}"
    );
    assert_eq!(
        tool_receiver.try_model_failure_code().await,
        Some(RECOVERY_REFUSED_UPSTREAM_CODE)
    );

    // 3) Tool-free journaled LlmNode with retry settings (`execute_journaled`): 0 retries, emits once.
    let journal_calls = Arc::new(AtomicUsize::new(0));
    let (journal_sender, journal_receiver) = pipeline_node_event_channel();
    let journal_factory: Arc<dyn PipelineLlmAgentFactory> = Arc::new(ScriptedLlmFactory {
        attempts: Arc::new(Mutex::new(VecDeque::from([
            ScriptedLlmAttempt::FailBeforeOutput(refused()),
            ScriptedLlmAttempt::Succeed("NEVER_RETRY"),
        ]))),
        calls: Arc::clone(&journal_calls),
        events: Some(journal_sender),
    });
    let journal_node = LlmNode::new(
        llm_definition(),
        BTreeMap::from([("answer".to_owned(), "str".to_owned())]),
        journal_factory,
    )
    .with_node_recovery(scoped())
    .with_settings(
        admitted_settings(
            "{retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 200, on_error: continue}",
            AdmittedNodeFamily::Llm,
            &[("answer", "str")],
        ),
        Some(Arc::new("exec-llm-journal-refused".to_owned())),
    );
    let journal_err = journal_node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("llm-journal-refused-thread"),
            1,
        ))
        .await
        .err()
        .expect("MayHaveStarted stops journaled node");
    assert_eq!(journal_calls.load(Ordering::SeqCst), 1);
    assert!(
        journal_err
            .to_string()
            .contains(crate::agents::pipeline::scoped_runtime::CALL_MAY_HAVE_STARTED_CODE),
        "expected may_have_started code, got: {journal_err}"
    );
    assert_eq!(
        journal_receiver.try_model_failure_code().await,
        Some(RECOVERY_REFUSED_UPSTREAM_CODE)
    );
}
