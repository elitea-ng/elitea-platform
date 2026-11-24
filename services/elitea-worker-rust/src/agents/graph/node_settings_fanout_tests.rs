#![allow(
    clippy::disallowed_methods,
    clippy::items_after_statements,
    clippy::too_many_lines,
    clippy::unnecessary_literal_bound,
    reason = "inline test fixtures for 5b-5 fan-out child node settings"
)]

//! 5b-5 proof tests: per-node automatic `settings:` inside `Parallel` branches,
//! `Map` workers, and `Map` child pipelines (including `silver_batch.yaml`'s
//! `validate` `on_error: error_output` -> `batch_failed` pattern), cross-sibling
//! thread isolation (`node_errors`, `execute_once`, retry journals), limit &
//! limit+1 bounds, escaping `error_transition` refusal, and digest stability.

use std::collections::{BTreeMap, BTreeSet, HashMap, VecDeque};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex as StdMutex};
use std::time::Duration;

use adk_rust::graph::checkpoint::{Checkpointer, MemoryCheckpointer};
use adk_rust::graph::{Checkpoint, ExecutionConfig, GraphError};
use adk_rust::{
    AdkError, Agent, Content, ErrorCategory, ErrorComponent, Event, EventStream, InvocationContext,
};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use elitea_agent_runtime::graph::node_recovery::{
    NodeFailureClass, NodeRecoveryPolicy, ReplaySafety,
};
use elitea_agent_runtime::graph::node_settings::NodeSettingsRefusal;
use ring::digest;
use serde_json::{Value, json};
use tokio::sync::Mutex;

use super::ParallelBranchDefinition;
use super::application::{
    ApplicationExecutionError, PipelineApplicationResolver, PipelineApplicationSelection,
    ResolvedApplicationParticipant,
};
use super::code_runtime::{
    CodeAttemptFailure, CodeAttemptPhase, CodeInvocation, CodeSandboxRuntime,
};
use super::compiler::{
    ParallelCompilerBinding, PipelineConfigurationError, PipelineDefinition, PipelineNodeRuntimes,
};
use super::llm::{
    LlmExecutionError, LlmExecutionInput, LlmNodeDefinition, PipelineLlmAgentBinding,
    PipelineLlmAgentFactory, PipelineLlmReplayEnvelope, PipelineModelScope,
};
use super::node_recovery_runtime::{
    NodeAttemptActivation, NodeAttemptAuthority, NodeAttemptJournal, NodeRecoveryFactory,
};
use super::parallel::{
    ParallelActivation, ParallelCheckpointAppender, ParallelCheckpointAuthority,
    ParallelChildCheckpoint, ParallelChildCheckpointerFactory, ParallelChildOrigin,
};
use crate::agents::pipeline::saved_child_scope_provider::SavedChildScopeProvider;
use crate::state::TestStateWriterLease;

#[derive(Default)]
struct InMemoryAppender(Mutex<Vec<Checkpoint>>);

fn same_checkpoint(left: &Checkpoint, right: &Checkpoint) -> bool {
    serde_json::to_value(left).ok() == serde_json::to_value(right).ok()
}

#[async_trait]
impl Checkpointer for InMemoryAppender {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let mut rows = self.0.lock().await;
        if let Some(existing) = rows
            .iter()
            .find(|row| row.checkpoint_id == checkpoint.checkpoint_id)
        {
            return if same_checkpoint(existing, checkpoint) {
                Ok(existing.checkpoint_id.clone())
            } else {
                Err(GraphError::CheckpointError("immutable conflict".to_owned()))
            };
        }
        rows.push(checkpoint.clone());
        Ok(checkpoint.checkpoint_id.clone())
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .0
            .lock()
            .await
            .iter()
            .rev()
            .find(|row| row.thread_id == thread)
            .cloned())
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .0
            .lock()
            .await
            .iter()
            .find(|row| row.checkpoint_id == id)
            .cloned())
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        Ok(self
            .0
            .lock()
            .await
            .iter()
            .filter(|row| row.thread_id == thread)
            .cloned()
            .collect())
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.0.lock().await.retain(|row| row.thread_id != thread);
        Ok(())
    }
}

#[async_trait]
impl ParallelCheckpointAppender for InMemoryAppender {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let latest = self.load(&candidate.thread_id).await?;
        let matches = match (expected, latest.as_ref()) {
            (Some(expected), Some(latest)) => same_checkpoint(expected, latest),
            (None, None) => true,
            _ => false,
        };
        if !matches
            && latest.as_ref().map(|row| &row.checkpoint_id) != Some(&candidate.checkpoint_id)
        {
            return Err(GraphError::CheckpointError(
                "atomic expected-parent conflict".to_owned(),
            ));
        }
        self.save(candidate).await
    }
}

type OpenedJournalRecord = (String, String, Option<[u8; 32]>, [u8; 32]);

/// Per-thread in-memory `NodeRecoveryFactory` enforcing that each child thread
/// opens journals strictly under its own derived thread identity.
#[derive(Default)]
struct PerThreadJournalFactory {
    allowed_prefix: String,
    stores: Mutex<HashMap<String, Arc<InMemoryAppender>>>,
    opened: StdMutex<Vec<OpenedJournalRecord>>,
}

impl PerThreadJournalFactory {
    fn for_thread(thread_id: String) -> Arc<Self> {
        Arc::new(Self {
            allowed_prefix: thread_id,
            stores: Mutex::default(),
            opened: StdMutex::default(),
        })
    }

    fn opened_records(&self) -> Vec<OpenedJournalRecord> {
        self.opened.lock().expect("opened").clone()
    }
}

#[async_trait]
impl NodeRecoveryFactory for PerThreadJournalFactory {
    async fn open(
        &self,
        activation: &NodeAttemptActivation,
        policy: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, GraphError> {
        let matches_thread = activation.root_thread_id == self.allowed_prefix
            || activation
                .root_thread_id
                .strip_prefix(&self.allowed_prefix)
                .is_some_and(|rest| rest.starts_with('/'));
        if !self.allowed_prefix.is_empty() && !matches_thread {
            return Err(GraphError::Other(
                "pipeline.node_recovery.invalid_scope".to_owned(),
            ));
        }
        if !self.allowed_prefix.is_empty() && activation.visit.is_none() {
            return Err(GraphError::Other(
                "pipeline.node_recovery.unscoped_child".to_owned(),
            ));
        }
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"test.fanout.node-journal.v1\0");
        hash.update(activation.root_thread_id.as_bytes());
        hash.update(b"\0");
        hash.update(activation.node_id.as_bytes());
        hash.update(&activation.step.to_be_bytes());
        hash.update(&activation.node_digest);
        hash.update(&activation.input_digest);
        if let Some(visit) = &activation.visit {
            hash.update(visit);
        }
        let mut activation_id = [0_u8; 32];
        activation_id.copy_from_slice(hash.finish().as_ref());
        let journal_thread = format!("n1:{}", URL_SAFE_NO_PAD.encode(activation_id));
        let store = Arc::clone(
            self.stores
                .lock()
                .await
                .entry(journal_thread.clone())
                .or_insert_with(|| Arc::new(InMemoryAppender::default())),
        );
        self.opened.lock().expect("opened").push((
            activation.root_thread_id.clone(),
            activation.node_id.clone(),
            activation.visit,
            activation_id,
        ));
        Ok(NodeAttemptJournal::bound(
            store,
            Arc::new(TestStateWriterLease::current()),
            journal_thread,
            activation_id,
            policy.clone(),
        ))
    }
}

/// In-memory `ParallelCheckpointAuthority` generating real `p1:` / `m1:` thread
/// ids and lending each branch its own isolated `PerThreadJournalFactory`.
#[derive(Default)]
struct FanoutMemoryAuthority {
    parent: InMemoryAppender,
    child_stores: Mutex<HashMap<String, Arc<MemoryCheckpointer>>>,
    child_journals: StdMutex<HashMap<String, Arc<PerThreadJournalFactory>>>,
}

impl FanoutMemoryAuthority {
    fn child_journals_snapshot(&self) -> HashMap<String, Arc<PerThreadJournalFactory>> {
        self.child_journals.lock().expect("journals").clone()
    }
}

#[async_trait]
impl Checkpointer for FanoutMemoryAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.parent.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.parent.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.parent.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.parent.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.parent.delete(thread).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for FanoutMemoryAuthority {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        self.parent.append_after(expected, candidate).await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for FanoutMemoryAuthority {
    fn child_origin(&self, _: &ParallelActivation) -> Result<ParallelChildOrigin, GraphError> {
        Ok(ParallelChildOrigin {
            execution_id: "fanout-exec-1".to_owned(),
            generation: 1,
        })
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        let prefix = if activation.map.is_some() {
            "m1:"
        } else {
            "p1:"
        };
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(prefix.as_bytes());
        hash.update(activation.root_thread_id.as_bytes());
        hash.update(activation.node_id.as_bytes());
        hash.update(&activation.step.to_be_bytes());
        hash.update(branch.id().as_bytes());
        hash.update(branch.node().as_bytes());
        hash.update(&ordinal.to_be_bytes());
        hash.update(input_digest);
        hash.update(origin.execution_id.as_bytes());
        Ok(format!("{prefix}{}", URL_SAFE_NO_PAD.encode(hash.finish())))
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        let thread_id = self.branch_thread_id(activation, branch, ordinal, input_digest, origin)?;
        let store = Arc::clone(
            self.child_stores
                .lock()
                .await
                .entry(thread_id.clone())
                .or_insert_with(|| Arc::new(MemoryCheckpointer::new())),
        );
        let journals = Arc::clone(
            self.child_journals
                .lock()
                .expect("journals")
                .entry(thread_id.clone())
                .or_insert_with(|| PerThreadJournalFactory::for_thread(thread_id.clone())),
        );
        Ok(ParallelChildCheckpoint {
            admitted_threads: BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer: store,
            node_recovery: Some(journals),
            tool_pipelines: None,
        })
    }
}

impl ParallelCheckpointAuthority for FanoutMemoryAuthority {}

fn refused(yaml: &str) -> (String, NodeSettingsRefusal) {
    match PipelineDefinition::from_yaml(yaml) {
        Err(PipelineConfigurationError::NodeSettings { node, refusal }) => (node, refusal),
        Err(other) => panic!("expected a settings refusal, got {other}"),
        Ok(_) => panic!("expected a settings refusal, the document compiled"),
    }
}

#[test]
fn fanout_children_admit_automatic_settings_refuse_escaping_routes_and_preserve_operator_recovery_and_digests()
 {
    let base_parallel = r#"
entry_point: gather
state:
  topic: str
  a_out: str
  b_out: str
  gathered: list
  tail_out: str
nodes:
  - id: gather
    type: parallel
    branches:
      - {id: a, node: ask_a}
      - {id: b, node: code_b}
    max_concurrency: 2
    wait: all
    output: [gathered]
    transition: tail
  - id: ask_a
    type: llm
    input: [topic]
    input_mapping: {task: {type: fixed, value: Hi}}
    output: [a_out]
  - id: code_b
    type: code
    code: 'return "ok"'
    input: [topic]
    output: [b_out]
  - id: tail
    type: state_modifier
    template: done
    output: [tail_out]
    transition: END
"#;
    let plain_digest = PipelineDefinition::from_yaml(base_parallel)
        .expect("plain parallel")
        .definition_digest();

    // Empty / default settings keep byte-identical definition digest.
    let defaulted = base_parallel.replace(
        "    output: [a_out]\n",
        "    output: [a_out]\n    settings: {on_error: stop, execute_once: false}\n",
    );
    assert_eq!(
        PipelineDefinition::from_yaml(&defaulted)
            .expect("defaulted")
            .definition_digest(),
        plain_digest
    );

    // Automatic settings at limit bounds (max_tries=2/5, wait=100/300000, timeout=1000/900000,
    // error_transition: END) are admitted and fold into the digest.
    let admitted = base_parallel
        .replace(
            "    output: [a_out]\n",
            "    output: [a_out]\n    settings: {retry_on_fail: true, max_tries: 5, wait_between_tries_ms: 300000, on_error: continue, always_output_data: true, execute_once: true, timeout_ms: 900000}\n",
        )
        .replace(
            "    output: [b_out]\n",
            "    output: [b_out]\n    settings: {retry_on_fail: true, max_tries: 2, wait_between_tries_ms: 100, on_error: error_output, error_transition: END, timeout_ms: 1000}\n",
        );
    let admitted_def = PipelineDefinition::from_yaml(&admitted).expect("admitted parallel");
    assert_ne!(admitted_def.definition_digest(), plain_digest);

    // Limit+1 and escaping error_transition checks.
    assert_eq!(
        refused(&admitted.replace("max_tries: 5", "max_tries: 6")),
        ("ask_a".to_owned(), NodeSettingsRefusal::MaxTriesRange)
    );
    assert_eq!(
        refused(&admitted.replace("max_tries: 2", "max_tries: 1")),
        ("code_b".to_owned(), NodeSettingsRefusal::MaxTriesRange)
    );
    assert_eq!(
        refused(&admitted.replace(
            "wait_between_tries_ms: 300000",
            "wait_between_tries_ms: 300001"
        )),
        ("ask_a".to_owned(), NodeSettingsRefusal::WaitRange)
    );
    assert_eq!(
        refused(&admitted.replace("timeout_ms: 900000", "timeout_ms: 900001")),
        ("ask_a".to_owned(), NodeSettingsRefusal::TimeoutRange)
    );
    // Escaping to root node is refused.
    assert_eq!(
        refused(&admitted.replace("error_transition: END", "error_transition: tail")),
        (
            "code_b".to_owned(),
            NodeSettingsRefusal::ErrorTransitionTarget
        )
    );
    // Targeting fan-out sibling is refused.
    assert_eq!(
        refused(&admitted.replace("error_transition: END", "error_transition: ask_a")),
        (
            "code_b".to_owned(),
            NodeSettingsRefusal::ErrorTransitionTarget
        )
    );

    // Operator-mode `recovery:` on a fan-out-owned Code node stays strictly refused.
    let operator_recovery = base_parallel.replace(
        "    output: [b_out]\n",
        "    output: [b_out]\n    recovery:\n      max_attempts: 2\n      retry_mode: operator\n      retry_on: [dependency_unavailable]\n      backoff: {initial_ms: 100, maximum_ms: 100, multiplier: 1}\n      max_elapsed_ms: 1000\n",
    );
    assert!(matches!(
        PipelineDefinition::from_yaml(&operator_recovery),
        Err(PipelineConfigurationError::Unsupported(_))
    ));

    // Agent inside Parallel still refuses settings as FamilyNotYet (5b-4 not added).
    let agent_parallel = base_parallel.replace(
        "  - id: ask_a\n    type: llm\n    input: [topic]\n    input_mapping: {task: {type: fixed, value: Hi}}\n    output: [a_out]\n",
        "  - id: ask_a\n    type: agent\n    tool: child\n    input: [topic]\n    input_mapping: {task: {type: fixed, value: Hi}}\n    output: [a_out]\n    settings: {on_error: continue}\n",
    );
    assert_eq!(
        refused(&agent_parallel),
        ("ask_a".to_owned(), NodeSettingsRefusal::FamilyNotYet)
    );
}

#[tokio::test]
async fn map_state_modifier_worker_continue_error_output_and_sibling_isolation() {
    for settings_yaml in [
        "{on_error: continue, always_output_data: true}",
        "{on_error: error_output, error_transition: END, always_output_data: true}",
    ] {
        let yaml = format!(
            r#"
entry_point: map_items
state:
  items: {{type: list, value: []}}
  rendered: {{type: str, value: ''}}
  mapped: {{type: list, value: []}}
nodes:
  - id: map_items
    type: map
    worker: render
    source: items
    item: entity
    index: item_index
    outputs: [rendered]
    destination: mapped
    max_items: 8
    max_concurrency: 3
    reduction: ordered_collection
    transition: END
  - id: render
    type: state_modifier
    input: [entity, item_index]
    output: [rendered]
    template: "{{% if entity == 'bad' %}}{{{{ entity | nosuchfilter }}}}{{% else %}}{{{{ entity }}}}:{{{{ item_index }}}}{{% endif %}}"
    settings: {settings_yaml}
"#
        );
        let definition = PipelineDefinition::from_yaml(&yaml).expect("map compiles");
        let authority = Arc::new(FanoutMemoryAuthority::default());
        let checkpointer: Arc<dyn Checkpointer> = authority.clone();
        let runtimes = PipelineNodeRuntimes::default()
            .with_execution_id("exec-map-sm")
            .with_parallel(ParallelCompilerBinding::for_tests(
                authority.clone(),
                tokio::time::Instant::now() + Duration::from_secs(60),
                None,
            ));
        let graph = definition
            .compile_with_runtime("map-root", checkpointer, None, &runtimes)
            .expect("graph compiles");
        let input = HashMap::from([("items".to_owned(), json!(["ok", "bad", "ok2"]))]);
        let result = graph
            .invoke(input, ExecutionConfig::new("root-map-sm"))
            .await
            .expect("map succeeds with settled item failure");
        let mapped = result["mapped"].as_array().expect("mapped list");
        assert_eq!(mapped.len(), 3, "{mapped:?}");
        assert_eq!(mapped[0]["outputs"]["rendered"], json!("ok:0"));
        assert_eq!(mapped[1]["outputs"]["rendered"], json!(""));
        assert_eq!(mapped[2]["outputs"]["rendered"], json!("ok2:2"));
        // Parent node_errors stays empty: child failures stay isolated per child thread.
        assert_eq!(result["node_errors"], json!([]));
    }
}

struct BranchScriptedLlmAgent {
    outcomes: Arc<StdMutex<VecDeque<Result<&'static str, AdkError>>>>,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl Agent for BranchScriptedLlmAgent {
    fn name(&self) -> &str {
        "branch-scripted-llm"
    }
    fn description(&self) -> &str {
        "branch scripted llm"
    }
    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }
    async fn run(&self, _: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let next = self
            .outcomes
            .lock()
            .expect("outcomes")
            .pop_front()
            .unwrap_or(Ok("default"));
        let stream = async_stream::stream! {
            match next {
                Ok(text) => {
                    let mut event = Event::new("inv-1");
                    event.author = "branch-scripted-llm".to_owned();
                    event.llm_response.content = Some(Content::new("model").with_text(text));
                    yield Ok(event);
                }
                Err(error) => {
                    yield Err(error);
                }
            }
        };
        Ok(Box::pin(stream))
    }
}

type ScriptedLlmOutcome = (
    Arc<StdMutex<VecDeque<Result<&'static str, AdkError>>>>,
    Arc<AtomicUsize>,
);

struct PerNodeScriptedLlmFactory {
    scripts: BTreeMap<String, ScriptedLlmOutcome>,
}

impl PipelineLlmAgentFactory for PerNodeScriptedLlmFactory {
    fn build(
        &self,
        definition: &LlmNodeDefinition,
        _: &LlmExecutionInput,
        _: Option<Value>,
        _: Option<&PipelineLlmReplayEnvelope>,
        _: &PipelineModelScope,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        let (outcomes, calls) = self
            .scripts
            .get(definition.id())
            .ok_or(LlmExecutionError::Unavailable)?;
        Ok(PipelineLlmAgentBinding::new(
            Arc::new(BranchScriptedLlmAgent {
                outcomes: Arc::clone(outcomes),
                calls: Arc::clone(calls),
            }),
            BTreeMap::new(),
        ))
    }

    fn for_fanout_branch(&self) -> Option<Arc<dyn PipelineLlmAgentFactory>> {
        Some(Arc::new(Self {
            scripts: self.scripts.clone(),
        }))
    }
}

#[tokio::test]
async fn parallel_llm_branches_retry_and_continue_isolated_per_child_thread() {
    let yaml = r#"
entry_point: gather
state:
  topic: str
  a_out: str
  b_out: str
  gathered: list
nodes:
  - id: gather
    type: parallel
    branches:
      - {id: a, node: ask_a}
      - {id: b, node: ask_b}
    max_concurrency: 2
    wait: all
    output: [gathered]
    transition: END
  - id: ask_a
    type: llm
    input: [topic]
    input_mapping: {task: {type: fstring, value: "A: {topic}"}}
    output: [a_out]
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100, execute_once: true}
  - id: ask_b
    type: llm
    input: [topic]
    input_mapping: {task: {type: fstring, value: "B: {topic}"}}
    output: [b_out]
    settings: {on_error: continue, always_output_data: true}
"#;
    let calls_a = Arc::new(AtomicUsize::new(0));
    let calls_b = Arc::new(AtomicUsize::new(0));
    let factory = Arc::new(PerNodeScriptedLlmFactory {
        scripts: BTreeMap::from([
            (
                "ask_a".to_owned(),
                (
                    Arc::new(StdMutex::new(VecDeque::from([
                        Err(AdkError::new(
                            ErrorComponent::Model,
                            ErrorCategory::RateLimited,
                            "model_gateway.rate_limited",
                            "429 rate limited",
                        )),
                        Ok("answer-from-a"),
                    ]))),
                    Arc::clone(&calls_a),
                ),
            ),
            (
                "ask_b".to_owned(),
                (
                    Arc::new(StdMutex::new(VecDeque::from([Err(AdkError::new(
                        ErrorComponent::Model,
                        ErrorCategory::InvalidInput,
                        "model_gateway.invalid_request",
                        "non-retryable",
                    ))]))),
                    Arc::clone(&calls_b),
                ),
            ),
        ]),
    });
    let definition = PipelineDefinition::from_yaml(yaml).expect("parallel llm compiles");
    let authority = Arc::new(FanoutMemoryAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes = PipelineNodeRuntimes::new(Some(factory), None, None)
        .with_execution_id("exec-par-llm")
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority.clone(),
            tokio::time::Instant::now() + Duration::from_secs(60),
            None,
        ));
    let graph = definition
        .compile_with_runtime("par-root", checkpointer, None, &runtimes)
        .expect("graph compiles");
    let result = graph
        .invoke(
            HashMap::from([("topic".to_owned(), json!("hello"))]),
            ExecutionConfig::new("root-par-llm"),
        )
        .await
        .expect("parallel completes with ask_a retried and ask_b continued");
    assert_eq!(calls_a.load(Ordering::SeqCst), 2, "ask_a retried once");
    assert_eq!(
        calls_b.load(Ordering::SeqCst),
        1,
        "ask_b did not retry non-retryable failure"
    );
    let gathered = result["gathered"].as_array().expect("gathered list");
    assert_eq!(gathered.len(), 2, "{gathered:?}");
    let by_id: HashMap<&str, &Value> = gathered
        .iter()
        .map(|entry| (entry["branch_id"].as_str().unwrap(), entry))
        .collect();
    assert_eq!(by_id["a"]["outputs"]["a_out"], json!("answer-from-a"));
    assert_eq!(by_id["b"]["outputs"]["b_out"], json!(""));
    assert_eq!(result["node_errors"], json!([]));

    // Both branches opened journals under distinct p1: child threads.
    let journals = authority.child_journals_snapshot();
    assert_eq!(journals.len(), 2);
    let mut threads = BTreeSet::new();
    let mut activations = BTreeSet::new();
    for (thread, factory) in journals {
        assert!(thread.starts_with("p1:"), "{thread}");
        let opened = factory.opened_records();
        assert_eq!(opened.len(), 1, "one journal per branch");
        assert_eq!(opened[0].0, thread);
        assert!(
            opened[0].2.is_some(),
            "inline LLM branch journal is visit-scoped (SEC-5B5-01)"
        );
        threads.insert(opened[0].0.clone());
        activations.insert(opened[0].3);
    }
    assert_eq!(threads.len(), 2);
    assert_eq!(activations.len(), 2);
}

struct ScriptedCodeRuntime {
    attempts_by_item: Arc<StdMutex<HashMap<i64, usize>>>,
}

#[async_trait]
impl CodeSandboxRuntime for ScriptedCodeRuntime {
    fn bind_saved_child_scope(
        &self,
        _: Arc<dyn SavedChildScopeProvider>,
    ) -> Result<Arc<dyn CodeSandboxRuntime>, GraphError> {
        Ok(Arc::new(Self {
            attempts_by_item: Arc::clone(&self.attempts_by_item),
        }))
    }

    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        Err(GraphError::Other(
            "expected execute_attempt under node recovery".to_owned(),
        ))
    }

    async fn execute_attempt(
        &self,
        invocation: CodeInvocation<'_>,
        _authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        let input: Value = serde_json::from_slice(&invocation.input_json).unwrap();
        let idx = input["item_index"].as_i64().unwrap_or(-1);
        let attempt = {
            let mut map = self.attempts_by_item.lock().expect("attempts");
            let entry = map.entry(idx).or_insert(0);
            *entry += 1;
            *entry
        };
        match idx {
            // Item 0: transient pre-effect failure on try 1 -> retry succeeds on try 2.
            0 if attempt == 1 => Err(CodeAttemptFailure::new(
                CodeAttemptPhase::Dispatch,
                NodeFailureClass::DependencyUnavailable,
                ReplaySafety::NoExternalEffect,
            )),
            // Item 2: non-retryable terminal failure -> settles via error_output -> END.
            2 => Err(CodeAttemptFailure::new(
                CodeAttemptPhase::Observation,
                NodeFailureClass::InvalidResult,
                ReplaySafety::NoExternalEffect,
            )),
            _ => {
                let entity = input["entity"].as_str().unwrap_or_default();
                let result = json!(format!("{entity}:try{attempt}"));
                let wire = serde_json::to_vec(&json!({
                    "revision": 1,
                    "status": "completed",
                    "exit_code": 0,
                    "stdout": json!({"revision": 1, "result": result}).to_string(),
                    "stderr": ""
                }))
                .unwrap();
                Ok(wire)
            }
        }
    }
}

#[tokio::test]
async fn map_code_worker_retries_and_error_output_end_isolated_per_item_thread() {
    let yaml = r#"
entry_point: map_code
state:
  items: {type: list, value: []}
  out: {type: str, value: ''}
  mapped: {type: list, value: []}
nodes:
  - id: map_code
    type: map
    worker: run_item
    source: items
    item: entity
    index: item_index
    outputs: [out]
    destination: mapped
    max_items: 8
    max_concurrency: 3
    reduction: ordered_collection
    transition: END
  - id: run_item
    type: code
    language: python
    code: 'return "ok"'
    input: [entity, item_index]
    output: [out]
    settings:
      retry_on_fail: true
      max_tries: 3
      wait_between_tries_ms: 100
      on_error: error_output
      error_transition: END
      always_output_data: true
      execute_once: true
      timeout_ms: 5000
"#;
    let definition = PipelineDefinition::from_yaml(yaml).expect("map code compiles");
    let authority = Arc::new(FanoutMemoryAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let attempts = Arc::new(StdMutex::new(HashMap::new()));
    let code_runtime = Arc::new(ScriptedCodeRuntime {
        attempts_by_item: Arc::clone(&attempts),
    });
    let root_journals = PerThreadJournalFactory::for_thread(String::new());
    let runtimes = PipelineNodeRuntimes::default()
        .with_code(code_runtime)
        .with_node_recovery_authority(root_journals)
        .with_execution_id("exec-map-code")
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority.clone(),
            tokio::time::Instant::now() + Duration::from_secs(60),
            None,
        ));
    let graph = definition
        .compile_with_runtime("map-code-root", checkpointer, None, &runtimes)
        .expect("graph compiles");
    let result = graph
        .invoke(
            HashMap::from([("items".to_owned(), json!(["alpha", "beta", "gamma"]))]),
            ExecutionConfig::new("root-map-code"),
        )
        .await
        .expect("map code settles cleanly");
    let mapped = result["mapped"].as_array().expect("mapped");
    assert_eq!(mapped.len(), 3, "{mapped:?}");
    assert_eq!(mapped[0]["outputs"]["out"], json!("alpha:try2"));
    assert_eq!(mapped[1]["outputs"]["out"], json!("beta:try1"));
    assert_eq!(mapped[2]["outputs"]["out"], json!(""));
    let counts = attempts.lock().expect("attempts").clone();
    assert_eq!(counts.get(&0), Some(&2));
    assert_eq!(counts.get(&1), Some(&1));
    assert_eq!(counts.get(&2), Some(&1));
    assert_eq!(result["node_errors"], json!([]));

    // Every Map item opened a distinct m1: visit-scoped journal.
    let journals = authority.child_journals_snapshot();
    assert_eq!(journals.len(), 3);
    let mut activations = BTreeSet::new();
    for (thread, factory) in journals {
        assert!(thread.starts_with("m1:"), "{thread}");
        let opened = factory.opened_records();
        assert_eq!(opened.len(), 1);
        assert_eq!(opened[0].0, thread);
        assert!(opened[0].2.is_some(), "Code child journal is visit-scoped");
        activations.insert(opened[0].3);
    }
    assert_eq!(activations.len(), 3);
}

/// Code runtime for the `silver_batch`-shaped Map child pipeline test:
/// fails `validate` on `"BAD_HEADER"`, routes to `batch_failed`, and succeeds
/// on `"VALID_HEADER"`.
struct SilverBatchCodeRuntime;

#[async_trait]
impl CodeSandboxRuntime for SilverBatchCodeRuntime {
    fn bind_saved_child_scope(
        &self,
        _: Arc<dyn SavedChildScopeProvider>,
    ) -> Result<Arc<dyn CodeSandboxRuntime>, GraphError> {
        Ok(Arc::new(Self))
    }

    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        Err(GraphError::Other(
            "expected execute_attempt under node recovery".to_owned(),
        ))
    }

    async fn execute_attempt(
        &self,
        invocation: CodeInvocation<'_>,
        _authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        let input: Value = serde_json::from_slice(&invocation.input_json).unwrap();
        let ok_wire = |val: Value| {
            serde_json::to_vec(&json!({
                "revision": 1,
                "status": "completed",
                "exit_code": 0,
                "stdout": json!({"revision": 1, "result": val}).to_string(),
                "stderr": ""
            }))
            .unwrap()
        };
        match invocation.source {
            "return {\"check\": \"ok\"}" => {
                if input["input"].as_str() == Some("BAD_HEADER") {
                    Err(CodeAttemptFailure::new(
                        CodeAttemptPhase::Observation,
                        NodeFailureClass::InvalidInput,
                        ReplaySafety::NoExternalEffect,
                    ))
                } else {
                    Ok(ok_wire(json!("ok")))
                }
            }
            "return \"quarantined\"" => {
                let err_count = input["node_errors"].as_array().map_or(0, Vec::len);
                let handled = input["node_errors"]
                    .as_array()
                    .and_then(|arr| arr.first())
                    .and_then(|entry| entry["handled"].as_str())
                    .unwrap_or("none");
                Ok(ok_wire(json!(format!(
                    "route=error_output;errors={err_count};handled={handled}"
                ))))
            }
            "return \"ok\"" => {
                let err_count = input["node_errors"].as_array().map_or(0, Vec::len);
                let once = input["once_count"].as_i64().unwrap_or(-1);
                let visits = input["visits"].as_i64().unwrap_or(-1);
                Ok(ok_wire(json!(format!(
                    "route=continue;errors={err_count};once={once};visits={visits}"
                ))))
            }
            _ => Err(CodeAttemptFailure::new(
                CodeAttemptPhase::Admission,
                NodeFailureClass::InvalidConfiguration,
                ReplaySafety::NoExternalEffect,
            )),
        }
    }
}

struct ChildPipelineResolver {
    child: Arc<PipelineDefinition>,
    journals: Option<Arc<dyn NodeRecoveryFactory>>,
    events: Option<super::node_events::PipelineNodeEventSender>,
}

impl PipelineApplicationResolver for ChildPipelineResolver {
    fn resolve(
        &self,
        _: &PipelineApplicationSelection,
        checkpointer: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        let mut runtimes = PipelineNodeRuntimes::default()
            .with_code(Arc::new(SilverBatchCodeRuntime))
            .with_execution_id("exec-silver-batch");
        if let Some(journals) = &self.journals {
            runtimes = runtimes.with_node_recovery_authority(Arc::clone(journals));
        }
        if let Some(events) = &self.events {
            runtimes = runtimes.with_events(events.clone());
        }
        let graph = self
            .child
            .compile_subgraph_with_runtime(checkpointer, &runtimes)
            .map_err(|_| ApplicationExecutionError::Unavailable)?;
        Ok(ResolvedApplicationParticipant::Pipeline {
            graph: Arc::new(graph),
            variable_types: self.child.declared_variable_types(),
            static_pauses: self.child.static_pause_catalog(),
            events: self.events.clone(),
            display_name: "silver_batch".to_owned(),
        })
    }

    fn test_activation_checkpoints(&self) -> bool {
        true
    }

    fn fixed_parallel_definition_digest(
        &self,
        _: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        Ok(self.child.definition_digest())
    }

    fn for_parallel_events(
        &self,
        events: super::node_events::PipelineNodeEventSender,
    ) -> Result<Arc<dyn PipelineApplicationResolver>, ApplicationExecutionError> {
        Ok(Arc::new(Self {
            child: Arc::clone(&self.child),
            journals: self.journals.clone(),
            events: Some(events),
        }))
    }

    fn for_branch_journals(
        &self,
        journals: Arc<dyn NodeRecoveryFactory>,
    ) -> Option<Arc<dyn PipelineApplicationResolver>> {
        Some(Arc::new(Self {
            child: Arc::clone(&self.child),
            journals: Some(journals),
            events: self.events.clone(),
        }))
    }
}

#[tokio::test]
async fn silver_batch_validate_error_output_and_execute_once_in_map_child_pipeline() {
    // Verify the acceptance fixture `scripts/acceptance/etl/pipelines/silver_batch.yaml`
    // declares `validate` with `on_error: error_output, error_transition: batch_failed`.
    let silver_path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../scripts/acceptance/etl/pipelines/silver_batch.yaml");
    let silver_raw = std::fs::read_to_string(&silver_path).expect("silver_batch.yaml exists");
    let silver_doc: serde_yaml_ng::Value =
        serde_yaml_ng::from_str(&silver_raw).expect("silver_batch.yaml parses");
    let validate = silver_doc["nodes"]
        .as_sequence()
        .expect("nodes")
        .iter()
        .find(|n| n["id"].as_str() == Some("validate"))
        .expect("validate node");
    assert_eq!(
        validate["settings"]["on_error"].as_str(),
        Some("error_output")
    );
    assert_eq!(
        validate["settings"]["error_transition"].as_str(),
        Some("batch_failed")
    );

    // Compile & execute a Map child pipeline matching silver_batch's `validate`
    // (`on_error: error_output`, `error_transition: batch_failed`) + `execute_once` loop.
    let child_yaml = r#"
state:
  input: {type: str}
  check: {type: str, value: ''}
  visits: {type: int, value: 0}
  once_count: {type: int, value: 0}
  batch_summary: {type: str, value: ''}
entry_point: validate
nodes:
  - id: validate
    type: code
    language: python
    code: {type: fixed, value: 'return {"check": "ok"}'}
    input: [input]
    output: [check]
    settings:
      on_error: error_output
      error_transition: batch_failed
    transition: bump
  - id: bump
    type: state_modifier
    template: "{{ visits + 1 }}"
    input: [visits]
    output: [visits]
    transition: tick_once
  - id: tick_once
    type: state_modifier
    template: "{{ once_count + 1 }}"
    input: [once_count]
    output: [once_count]
    settings: {execute_once: true}
    transition: loop_check
  - id: loop_check
    type: router
    condition: "{{ 'bump' if visits < 2 else 'finish' }}"
    input: [visits]
    routes: [bump, finish]
    default_output: finish
  - id: finish
    type: code
    language: python
    code: {type: fixed, value: 'return "ok"'}
    input: [node_errors, once_count, visits]
    output: [batch_summary]
    transition: END
  - id: batch_failed
    type: code
    language: python
    code: {type: fixed, value: 'return "quarantined"'}
    input: [node_errors]
    output: [batch_summary]
    transition: END
"#;
    let parent_yaml = r"
entry_point: clean_batches
state:
  batches: {type: list, value: []}
  batch_out: {type: str, value: ''}
  cleaned: {type: list, value: []}
nodes:
  - id: clean_batches
    type: map
    worker: run_batch
    source: batches
    item: batch
    index: batch_idx
    outputs: [batch_out]
    destination: cleaned
    max_items: 8
    max_concurrency: 2
    reduction: ordered_collection
    transition: END
  - id: run_batch
    type: agent
    tool: silver_batch
    input: [batch]
    input_mapping:
      task: {type: variable, value: batch}
    output: [batch_out]
";
    let child_def = Arc::new(PipelineDefinition::from_yaml(child_yaml).expect("child compiles"));
    let parent_def = PipelineDefinition::from_yaml(parent_yaml).expect("parent compiles");
    let authority = Arc::new(FanoutMemoryAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let resolver = Arc::new(ChildPipelineResolver {
        child: child_def,
        journals: None,
        events: None,
    });
    let runtimes = PipelineNodeRuntimes::new(None, None, Some(resolver))
        .with_execution_id("exec-silver-parent")
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + Duration::from_secs(60),
            None,
        ));
    let graph = parent_def
        .compile_with_runtime("silver-parent", checkpointer, None, &runtimes)
        .expect("parent graph compiles");
    let result = graph
        .invoke(
            HashMap::from([("batches".to_owned(), json!(["BAD_HEADER", "VALID_HEADER"]))]),
            ExecutionConfig::new("root-silver-parent"),
        )
        .await
        .expect("silver_batch Map completes");
    let cleaned = result["cleaned"].as_array().expect("cleaned results");
    assert_eq!(cleaned.len(), 2, "{cleaned:?}");
    assert_eq!(
        cleaned[0]["outputs"]["batch_out"],
        json!("route=error_output;errors=1;handled=error_output")
    );
    assert_eq!(
        cleaned[1]["outputs"]["batch_out"],
        json!("route=continue;errors=0;once=1;visits=2")
    );
}

struct MayHaveStartedOnceLlmFactory {
    calls: Arc<AtomicUsize>,
}

impl PipelineLlmAgentFactory for MayHaveStartedOnceLlmFactory {
    fn build(
        &self,
        definition: &LlmNodeDefinition,
        _: &LlmExecutionInput,
        _: Option<Value>,
        _: Option<&PipelineLlmReplayEnvelope>,
        _: &PipelineModelScope,
    ) -> Result<PipelineLlmAgentBinding, LlmExecutionError> {
        if definition.id() == "ask_b" {
            return Ok(PipelineLlmAgentBinding::new(
                Arc::new(BranchScriptedLlmAgent {
                    outcomes: Arc::new(StdMutex::new(VecDeque::from([Ok("ok-b")]))),
                    calls: Arc::new(AtomicUsize::new(0)),
                }),
                BTreeMap::new(),
            ));
        }
        let count = self.calls.fetch_add(1, Ordering::SeqCst);
        if count == 0 {
            Err(LlmExecutionError::MayHaveStarted)
        } else {
            panic!("process 2 must never re-dispatch a MayHaveStarted journaled LLM branch");
        }
    }

    fn for_fanout_branch(&self) -> Option<Arc<dyn PipelineLlmAgentFactory>> {
        Some(Arc::new(Self {
            calls: Arc::clone(&self.calls),
        }))
    }
}

#[tokio::test]
async fn parallel_llm_branch_crash_restore_after_may_have_started_never_emits_recovery_interrupt_or_reruns()
 {
    // SEC-5B5-01: Process 1 hits MayHaveStarted on a tool-free inline Parallel LLM
    // branch with settings: (committing Reconcile in its visit-scoped n1: journal).
    // Process 2 replays against the same journal store: assert 0 re-dispatches,
    // no pipeline_node_recovery interrupt / pause_unproved, and fail-closed error.
    let yaml = r#"
entry_point: gather
state:
  topic: str
  a_out: str
  b_out: str
  gathered: list
nodes:
  - id: gather
    type: parallel
    branches:
      - {id: a, node: ask_a}
      - {id: b, node: ask_b}
    max_concurrency: 1
    wait: all
    output: [gathered]
    transition: END
  - id: ask_a
    type: llm
    input: [topic]
    input_mapping: {task: {type: fstring, value: "A: {topic}"}}
    output: [a_out]
    settings: {retry_on_fail: true, max_tries: 3, wait_between_tries_ms: 100, on_error: continue, always_output_data: true}
  - id: ask_b
    type: llm
    input: [topic]
    input_mapping: {task: {type: fstring, value: "B: {topic}"}}
    output: [b_out]
"#;
    let definition = PipelineDefinition::from_yaml(yaml).expect("parallel llm compiles");
    let authority = Arc::new(FanoutMemoryAuthority::default());
    let calls = Arc::new(AtomicUsize::new(0));

    // Process 1: MayHaveStarted commits Reconcile to the branch's visit-scoped n1: journal.
    let runtimes1 = PipelineNodeRuntimes::new(
        Some(Arc::new(MayHaveStartedOnceLlmFactory {
            calls: Arc::clone(&calls),
        })),
        None,
        None,
    )
    .with_execution_id("exec-llm-crash-restore")
    .with_parallel(ParallelCompilerBinding::for_tests(
        authority.clone(),
        tokio::time::Instant::now() + Duration::from_secs(60),
        None,
    ));
    let graph1 = definition
        .compile_with_runtime("par-llm-p1", authority.clone(), None, &runtimes1)
        .expect("graph1 compiles");
    let res1 = graph1
        .invoke(
            HashMap::from([("topic".to_owned(), json!("restore-me"))]),
            ExecutionConfig::new("root-par-llm-restore"),
        )
        .await
        .expect("parallel settles with ask_a failed as may_have_started");
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let gathered1 = res1["gathered"].as_array().expect("gathered1");
    let first_run_a = gathered1
        .iter()
        .find(|entry| entry["branch_id"] == json!("a"))
        .expect("branch a");
    assert_eq!(first_run_a["status"], json!("failed"));
    assert_eq!(
        first_run_a["error"]["code"],
        json!("graph.fanout.may_have_started")
    );

    // Verify the child journal for ask_a was opened with an explicit visit, and
    // verify an unscoped child open (visit: None) is rejected fail-closed.
    let journals = authority.child_journals_snapshot();
    let (child_thread, child_factory) = journals
        .iter()
        .find(|(_, factory)| factory.opened_records().iter().any(|r| r.1 == "ask_a"))
        .expect("ask_a child journal");
    let opened = child_factory.opened_records();
    assert_eq!(opened.len(), 1);
    assert!(opened[0].2.is_some(), "LLM branch journal must carry visit");
    let unscoped_err = child_factory
        .open(
            &NodeAttemptActivation {
                root_thread_id: child_thread.clone(),
                node_id: "ask_a".to_owned(),
                step: 0,
                node_digest: [0x11; 32],
                input_digest: [0x22; 32],
                visit: None,
            },
            &NodeRecoveryPolicy::default(),
        )
        .await
        .err()
        .expect("unscoped child must be refused");
    assert!(
        unscoped_err
            .to_string()
            .contains("pipeline.node_recovery.unscoped_child"),
        "{unscoped_err}"
    );

    // Process 2: fresh graph/runtime re-invokes the same child journal (simulating
    // crash before parent persisted branch failure) — reuse child_journals on a
    // fresh parent/child checkpoint store with the persisted n1: journal stores!
    let authority2 = Arc::new(FanoutMemoryAuthority::default());
    {
        let mut dst = authority2.child_journals.lock().expect("dst");
        for (thread, factory) in &journals {
            dst.insert(thread.clone(), Arc::clone(factory));
        }
    }
    let runtimes2 = PipelineNodeRuntimes::new(
        Some(Arc::new(MayHaveStartedOnceLlmFactory {
            calls: Arc::clone(&calls),
        })),
        None,
        None,
    )
    .with_execution_id("exec-llm-crash-restore")
    .with_parallel(ParallelCompilerBinding::for_tests(
        authority2.clone(),
        tokio::time::Instant::now() + Duration::from_secs(60),
        None,
    ));
    let graph2 = definition
        .compile_with_runtime("par-llm-p2", authority2, None, &runtimes2)
        .expect("graph2 compiles");
    let res2 = graph2
        .invoke(
            HashMap::from([("topic".to_owned(), json!("restore-me"))]),
            ExecutionConfig::new("root-par-llm-restore"),
        )
        .await
        .expect("process 2 settles fail-closed on restored Reconcile");
    assert_eq!(
        calls.load(Ordering::SeqCst),
        1,
        "process 2 must perform 0 re-dispatches"
    );
    let gathered2 = res2["gathered"].as_array().expect("gathered2");
    let replay_a = gathered2
        .iter()
        .find(|entry| entry["branch_id"] == json!("a"))
        .expect("branch a");
    assert_eq!(replay_a["status"], json!("failed"));
    assert_eq!(
        replay_a["error"]["code"],
        json!("graph.fanout.may_have_started"),
        "restored child LLM must fail closed as may_have_started (never pause_unproved / pipeline_node_recovery): {replay_a:?}"
    );
    let replay_sibling = gathered2
        .iter()
        .find(|entry| entry["branch_id"] == json!("b"))
        .expect("branch b");
    assert_eq!(replay_sibling["status"], json!("succeeded"));
    assert_eq!(replay_sibling["outputs"]["b_out"], json!("ok-b"));
}

struct UnknownEffectCodeRuntime {
    attempts: Arc<AtomicUsize>,
}

#[async_trait]
impl CodeSandboxRuntime for UnknownEffectCodeRuntime {
    fn bind_saved_child_scope(
        &self,
        _: Arc<dyn SavedChildScopeProvider>,
    ) -> Result<Arc<dyn CodeSandboxRuntime>, GraphError> {
        Ok(Arc::new(Self {
            attempts: Arc::clone(&self.attempts),
        }))
    }

    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        Err(GraphError::Other("unexpected direct execute".to_owned()))
    }

    async fn execute_attempt(
        &self,
        _: CodeInvocation<'_>,
        authority: &NodeAttemptAuthority,
    ) -> Result<Vec<u8>, CodeAttemptFailure> {
        self.attempts.fetch_add(1, Ordering::SeqCst);
        Err(CodeAttemptFailure::new(
            CodeAttemptPhase::Observation,
            NodeFailureClass::Unknown,
            ReplaySafety::UnknownExternalEffect {
                effect_id: authority.dispatch_activation(),
            },
        ))
    }
}

#[tokio::test]
async fn fanout_code_child_unknown_external_effect_stops_with_zero_retries_and_no_swallow() {
    // SEC-5B5-02: even with retry_on_fail + on_error: continue, a fan-out Code
    // child whose attempt returns UnknownExternalEffect must stop fail-closed
    // with child_effect_unknown, 0 retries, and never swallow the failure.
    let yaml = r#"
entry_point: map_code
state:
  items: {type: list, value: []}
  out: {type: str, value: ''}
  mapped: {type: list, value: []}
nodes:
  - id: map_code
    type: map
    worker: run_item
    source: items
    item: entity
    index: item_index
    outputs: [out]
    destination: mapped
    max_items: 4
    max_concurrency: 1
    reduction: ordered_collection
    transition: END
  - id: run_item
    type: code
    language: python
    code: 'return "ok"'
    input: [entity, item_index]
    output: [out]
    settings:
      retry_on_fail: true
      max_tries: 3
      wait_between_tries_ms: 100
      on_error: continue
      always_output_data: true
"#;
    let definition = PipelineDefinition::from_yaml(yaml).expect("map code compiles");
    let authority = Arc::new(FanoutMemoryAuthority::default());
    let attempts = Arc::new(AtomicUsize::new(0));
    let runtimes = PipelineNodeRuntimes::default()
        .with_code(Arc::new(UnknownEffectCodeRuntime {
            attempts: Arc::clone(&attempts),
        }))
        .with_node_recovery_authority(PerThreadJournalFactory::for_thread(String::new()))
        .with_execution_id("exec-map-code-unknown")
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority.clone(),
            tokio::time::Instant::now() + Duration::from_secs(60),
            None,
        ));
    let graph = definition
        .compile_with_runtime("map-code-unknown", authority, None, &runtimes)
        .expect("graph compiles");
    let err = graph
        .invoke(
            HashMap::from([("items".to_owned(), json!(["alpha"]))]),
            ExecutionConfig::new("root-map-code-unknown"),
        )
        .await
        .expect_err("UnknownExternalEffect must not be retried or swallowed");
    assert_eq!(attempts.load(Ordering::SeqCst), 1, "zero retries allowed");
    assert!(
        err.to_string()
            .contains("pipeline.node_recovery.child_effect_unknown"),
        "expected child_effect_unknown: {err}"
    );
}

#[tokio::test]
async fn parent_and_fanout_children_isolate_node_errors_across_parallel_and_map() {
    // SEC-5B5-02: parent records a root node_errors entry before Parallel and Map;
    // children fail with on_error: continue; neither direction leaks node_errors,
    // and explicit YAML attempts to pass node_errors across fan-out are refused.
    let yaml = r#"
entry_point: root_fail
state:
  topic: {type: str, value: "ok"}
  items: {type: list, value: []}
  root_out: {type: str, value: ''}
  a_out: {type: str, value: ''}
  b_out: {type: str, value: ''}
  item_out: {type: str, value: ''}
  gathered: {type: list, value: []}
  mapped: {type: list, value: []}
nodes:
  - id: root_fail
    type: state_modifier
    template: "{{ 1 / 0 }}"
    output: [root_out]
    settings: {on_error: continue, always_output_data: true}
    transition: gather
  - id: gather
    type: parallel
    branches:
      - {id: a, node: ask_a}
      - {id: b, node: ask_b}
    max_concurrency: 2
    wait: all
    output: [gathered]
    transition: map_step
  - id: ask_a
    type: llm
    input: [topic]
    input_mapping: {task: {type: fstring, value: "A: {topic}"}}
    output: [a_out]
    settings: {on_error: continue, always_output_data: true}
  - id: ask_b
    type: llm
    input: [topic]
    input_mapping: {task: {type: fstring, value: "B: {topic}"}}
    output: [b_out]
    settings: {on_error: continue, always_output_data: true}
  - id: map_step
    type: map
    worker: item_fail
    source: items
    item: entity
    index: item_index
    outputs: [item_out]
    destination: mapped
    max_items: 4
    max_concurrency: 2
    reduction: ordered_collection
    transition: END
  - id: item_fail
    type: state_modifier
    template: "{{ 1 / 0 }}"
    input: [entity, item_index]
    output: [item_out]
    settings: {on_error: continue, always_output_data: true}
"#;
    let calls_a = Arc::new(AtomicUsize::new(0));
    let calls_b = Arc::new(AtomicUsize::new(0));
    let factory = Arc::new(PerNodeScriptedLlmFactory {
        scripts: BTreeMap::from([
            (
                "ask_a".to_owned(),
                (
                    Arc::new(StdMutex::new(VecDeque::from([Err(AdkError::new(
                        ErrorComponent::Model,
                        ErrorCategory::InvalidInput,
                        "model_gateway.invalid_request",
                        "fail-a",
                    ))]))),
                    Arc::clone(&calls_a),
                ),
            ),
            (
                "ask_b".to_owned(),
                (
                    Arc::new(StdMutex::new(VecDeque::from([Err(AdkError::new(
                        ErrorComponent::Model,
                        ErrorCategory::InvalidInput,
                        "model_gateway.invalid_request",
                        "fail-b",
                    ))]))),
                    Arc::clone(&calls_b),
                ),
            ),
        ]),
    });
    let definition = PipelineDefinition::from_yaml(yaml).expect("pipeline compiles");
    let authority = Arc::new(FanoutMemoryAuthority::default());
    let runtimes = PipelineNodeRuntimes::new(Some(factory), None, None)
        .with_execution_id("exec-isolation")
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority.clone(),
            tokio::time::Instant::now() + Duration::from_secs(60),
            None,
        ));
    let graph = definition
        .compile_with_runtime("iso-root", authority.clone(), None, &runtimes)
        .expect("graph compiles");
    let result = graph
        .invoke(
            HashMap::from([("items".to_owned(), json!(["x", "y"]))]),
            ExecutionConfig::new("root-iso"),
        )
        .await
        .expect("pipeline completes");
    // Parent node_errors has ONLY root_fail's 1 entry (0 leaked from ask_a or item_fail).
    let root_errors = result["node_errors"].as_array().expect("root node_errors");
    assert_eq!(root_errors.len(), 1, "{root_errors:?}");
    assert_eq!(root_errors[0]["node"], json!("root_fail"));

    // Verify each child checkpoint store has only its own 1 child entry (not root_fail).
    let stores = authority.child_stores.lock().await;
    assert_eq!(stores.len(), 4, "2 parallel branches + 2 map items");
    for (thread, store) in stores.iter() {
        let cp = store
            .load(thread)
            .await
            .expect("load")
            .expect("child checkpoint");
        let child_errors = cp.state["node_errors"]
            .as_array()
            .expect("child node_errors");
        assert_eq!(
            child_errors.len(),
            1,
            "child {thread} must not inherit parent node_errors: {child_errors:?}"
        );
        assert_ne!(child_errors[0]["node"], json!("root_fail"));
    }

    // Explicit YAML boundary crossings are refused with NodeErrorsNestedMapping.
    for (bad_yaml, expected_node) in [
        (
            "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, broadcast: [node_errors], outputs: [item_result], destination: mapped, max_items: 4, max_concurrency: 2, reduction: ordered_collection, transition: END}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}', settings: {on_error: continue}}\n",
            "map_entities",
        ),
        (
            "entry_point: gather\nstate:\n  topic: str\n  a_out: {type: str, value: ''}\n  b_out: {type: str, value: ''}\n  gathered: {type: list, value: []}\nnodes:\n  - id: gather\n    type: parallel\n    branches:\n      - {id: a, node: run_a}\n      - {id: b, node: run_b}\n    max_concurrency: 2\n    wait: all\n    output: [gathered]\n    transition: END\n  - {id: run_a, type: code, language: python, code: 'return \"ok\"', input: [topic, node_errors], output: [a_out], settings: {on_error: continue}}\n  - {id: run_b, type: code, language: python, code: 'return \"ok\"', input: [topic], output: [b_out]}\n",
            "run_a",
        ),
    ] {
        assert_eq!(
            refused(bad_yaml),
            (
                expected_node.to_owned(),
                NodeSettingsRefusal::NodeErrorsNestedMapping
            )
        );
    }
}

#[tokio::test]
async fn fanout_child_node_errors_enforces_64_65_cap_truncation_and_secret_redaction() {
    // SEC-5B5-02: prove MAX_NODE_ERRORS (64 ok / 65 -> pipeline.node_errors.exhausted),
    // <= 512 B message bound, and sentinel secret redaction inside a Map child pipeline.
    use elitea_agent_runtime::graph::node_settings::{
        MAX_NODE_ERROR_ENTRY_BYTES, MAX_NODE_ERROR_OBJECT_BYTES, MAX_NODE_ERRORS,
    };

    let sentinel_api_key = "sk-live-secret-99887766554433221100";
    let sentinel_bearer = "Bearer topsecret-token-xyz";
    let child_yaml_for = |seed_count: usize| {
        let entries: Vec<Value> = (0..seed_count).map(|i| json!({"seed": i})).collect();
        format!(
            r#"
state:
  input: {{type: str}}
  node_errors: {{type: list, value: {}, reducer: append}}
  step_out: {{type: str, value: ''}}
  summary: {{type: str, value: ''}}
entry_point: fail_step
nodes:
  - id: fail_step
    type: state_modifier
    template: "{{{{ ('sk-live-secret-99887766554433221100 Bearer topsecret-token-xyz ' ~ (1 / 0)) }}}}"
    input: [input]
    output: [step_out]
    settings: {{on_error: continue, always_output_data: true}}
    transition: report
  - id: report
    type: state_modifier
    template: "count={{{{ node_errors | length }}}}"
    input: [node_errors]
    output: [summary]
    transition: END
"#,
            Value::Array(entries)
        )
    };
    let parent_yaml = r"
entry_point: map_child
state:
  batches: {type: list, value: []}
  out: {type: str, value: ''}
  results: {type: list, value: []}
nodes:
  - id: map_child
    type: map
    worker: run_child
    source: batches
    item: batch
    index: idx
    outputs: [out]
    destination: results
    max_items: 4
    max_concurrency: 1
    reduction: ordered_collection
    transition: END
  - id: run_child
    type: agent
    tool: bounded_child
    input: [batch]
    input_mapping:
      task: {type: variable, value: batch}
    output: [out]
";
    let parent_def = PipelineDefinition::from_yaml(parent_yaml).expect("parent compiles");

    // 63 seeded + 1 runtime failure = 64 -> succeeds, message <= 512 B, no sentinels leaked.
    let child_at_64 = Arc::new(
        PipelineDefinition::from_yaml(&child_yaml_for(MAX_NODE_ERRORS - 1))
            .expect("child 64 compiles"),
    );
    let authority64 = Arc::new(FanoutMemoryAuthority::default());
    let (event_tx, event_rx) = super::node_events::pipeline_node_event_channel();
    let runtimes64 = PipelineNodeRuntimes::new(
        None,
        None,
        Some(Arc::new(ChildPipelineResolver {
            child: child_at_64,
            journals: None,
            events: Some(event_tx),
        })),
    )
    .with_execution_id("exec-cap-64")
    .with_parallel(ParallelCompilerBinding::for_tests(
        authority64.clone(),
        tokio::time::Instant::now() + Duration::from_secs(60),
        None,
    ));
    let graph64 = parent_def
        .compile_with_runtime("cap64", authority64.clone(), None, &runtimes64)
        .expect("graph64 compiles");
    let ok64 = graph64
        .invoke(
            HashMap::from([("batches".to_owned(), json!(["batch-64"]))]),
            ExecutionConfig::new("root-cap64"),
        )
        .await
        .expect("64th node_errors entry inside fan-out child succeeds");
    assert_eq!(ok64["results"][0]["outputs"]["out"], json!("count=64"));

    // Inspect saved child checkpoint's recorded 64th entry: <= 512 B and zero sentinel leakage.
    let child_stores = authority64.child_stores.lock().await;
    let mut verified_entry = false;
    for (thread, store) in child_stores.iter() {
        let subgraph_thread = format!("{thread}/run_child");
        if let Some(cp) = store.load(&subgraph_thread).await.expect("load")
            && let Some(arr) = cp.state.get("node_errors").and_then(Value::as_array)
            && arr.len() == MAX_NODE_ERRORS
        {
            let last = &arr[MAX_NODE_ERRORS - 1];
            let msg = last["error"]["message_safe"]
                .as_str()
                .expect("message_safe");
            assert!(msg.len() <= 512, "len={}", msg.len());
            assert!(
                last["error"].to_string().len() <= MAX_NODE_ERROR_OBJECT_BYTES,
                "error object len={}",
                last["error"].to_string().len()
            );
            let raw_entry = last.to_string();
            assert!(
                raw_entry.len() <= MAX_NODE_ERROR_ENTRY_BYTES,
                "entry len={}",
                raw_entry.len()
            );
            assert!(!raw_entry.contains(sentinel_api_key), "{raw_entry}");
            assert!(!raw_entry.contains(sentinel_bearer), "{raw_entry}");
            verified_entry = true;
        }
    }
    assert!(
        verified_entry,
        "expected 64-entry child node_errors checkpoint"
    );
    let mut drain = event_rx
        .drain("inv-cap64", "cap64", "")
        .await
        .expect("drain");
    while let Some(ev) = drain.try_recv() {
        let serialized = format!("{ev:?}");
        assert!(!serialized.contains(sentinel_api_key), "{serialized}");
        assert!(!serialized.contains(sentinel_bearer), "{serialized}");
    }

    // 64 seeded + 1 runtime failure = 65 -> fails closed with pipeline.node_errors.exhausted.
    let child_at_65 = Arc::new(
        PipelineDefinition::from_yaml(&child_yaml_for(MAX_NODE_ERRORS)).expect("child 65 compiles"),
    );
    let authority65 = Arc::new(FanoutMemoryAuthority::default());
    let runtimes65 = PipelineNodeRuntimes::new(
        None,
        None,
        Some(Arc::new(ChildPipelineResolver {
            child: child_at_65,
            journals: None,
            events: None,
        })),
    )
    .with_execution_id("exec-cap-65")
    .with_parallel(ParallelCompilerBinding::for_tests(
        authority65.clone(),
        tokio::time::Instant::now() + Duration::from_secs(60),
        None,
    ));
    let graph65 = parent_def
        .compile_with_runtime("cap65", authority65, None, &runtimes65)
        .expect("graph65 compiles");
    let err65 = graph65
        .invoke(
            HashMap::from([("batches".to_owned(), json!(["batch-65"]))]),
            ExecutionConfig::new("root-cap65"),
        )
        .await
        .expect_err("65th node_errors entry inside fan-out child must fail closed");
    let err65_text = err65.to_string();
    assert!(!err65_text.contains(sentinel_api_key), "{err65_text}");
    assert!(!err65_text.contains(sentinel_bearer), "{err65_text}");
    assert!(
        err65_text.contains("item_failed") || err65_text.contains("node_errors"),
        "{err65_text}"
    );
}

#[test]
fn result_policy_excludes_fanout_owned_error_transition_end_outputs() {
    // CR-5B5-02: a fan-out child with `settings: {on_error: error_output,
    // error_transition: END}` must never inject its child-local output keys into
    // the root pipeline's `terminal_data_keys` and shadow the root's final answer.
    let yaml = r"
entry_point: map_step
state:
  items: {type: list, value: []}
  branch_code: {type: int, value: 0}
  mapped: {type: list, value: []}
  summary: {type: str, value: ''}
nodes:
  - id: map_step
    type: map
    worker: item_worker
    source: items
    item: entity
    index: item_index
    outputs: [branch_code]
    destination: mapped
    max_items: 4
    max_concurrency: 2
    reduction: ordered_collection
    transition: finalize
  - id: finalize
    type: state_modifier
    template: 'final-root-answer'
    output: [summary]
    transition: END
  - id: item_worker
    type: state_modifier
    template: '{{ item_index }}'
    input: [entity, item_index]
    output: [branch_code]
    settings: {on_error: error_output, error_transition: END}
";
    let definition = PipelineDefinition::from_yaml(yaml).expect("compiles");
    let policy = definition.result_policy();
    assert_eq!(policy.terminal_data_keys, vec!["summary".to_owned()]);
    assert!(
        !policy.terminal_data_keys.iter().any(|k| k == "branch_code"),
        "fan-out child output leaked into terminal_data_keys: {:?}",
        policy.terminal_data_keys
    );
    let final_state = HashMap::from([
        ("branch_code".to_owned(), json!(0)),
        ("summary".to_owned(), json!("final-root-answer")),
    ]);
    assert_eq!(definition.completed_text(&final_state), "final-root-answer");
}
