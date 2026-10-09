//! Direct tool nodes with external effects run only behind the fenced Started
//! journal. Pauses, blocks and skips are decided before it and never reach it.
use super::*;
use crate::agents::graph::direct_tool::{
    DIRECT_TOOL_RESUME_STATE_KEY, DirectToolExecutionError, DirectToolNode,
    DirectToolNodeDefinition, DirectToolSelection, PipelineDirectToolResolver, ResolvedDirectTool,
};
use crate::toolkits::{
    SensitiveToolPolicy, ToolAdmissionPolicy, delegated_authorization_error_fixture,
};
use adk_rust::graph::END;
use adk_rust::graph::interrupt::Interrupt;
use adk_rust::{Tool, ToolContext};
use serde_json::{Value, json};
use std::sync::atomic::AtomicUsize;
use tokio::sync::Notify;

#[path = "node_recovery_artifact_tests.rs"]
mod artifact_tests;

const NODE: &str = r"
id: lookup
type: toolkit
toolkit_name: Customer Support
tool: search_records
input_mapping:
  query: {type: fixed, value: create}
input: []
output: [report, messages]
structured_output: true
transition: END
";

enum Behavior {
    Succeed,
    Fail,
    Authorization(Arc<AtomicBool>),
    /// The call succeeds, then the writer lease is lost before the result commits.
    SucceedThenLoseLease(Arc<Lease>),
    /// The call is held inside the tool until the gate releases it.
    Gated(Arc<Gate>),
}

/// Holds a call inside the tool so a `PostgreSQL` proof can end or fence the process mid-effect.
pub(crate) struct Gate {
    pub(crate) entered: Notify,
    pub(crate) release: Notify,
}

/// The shared fixture journal ignores the activation; a real journal is one per activation.
pub(in crate::agents::graph) struct ScopedFactory(Factory);

#[async_trait]
impl NodeRecoveryFactory for ScopedFactory {
    async fn open(
        &self,
        activation: &NodeAttemptActivation,
        policy: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, GraphError> {
        let scope = format!("{:?}", activation.input_digest);
        Ok(NodeAttemptJournal::bound(
            self.0.store.clone(),
            self.0.lease.clone(),
            format!("node-journal-{}-{scope}", activation.step),
            activation.input_digest,
            policy.clone(),
        ))
    }
}

pub(in crate::agents::graph) fn scoped() -> Arc<ScopedFactory> {
    Arc::new(ScopedFactory(Factory {
        store: Arc::new(Store::default()),
        lease: Arc::new(Lease(AtomicBool::new(true))),
    }))
}

struct EffectTool {
    calls: Arc<AtomicUsize>,
    behavior: Behavior,
}

#[async_trait]
impl Tool for EffectTool {
    fn name(&self) -> &'static str {
        "search_records"
    }
    fn description(&self) -> &'static str {
        "effectful direct tool fixture"
    }
    fn is_read_only(&self) -> bool {
        false
    }
    async fn execute(&self, _: Arc<dyn ToolContext>, _: Value) -> adk_rust::Result<Value> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        match &self.behavior {
            Behavior::Authorization(authorized) if !authorized.load(Ordering::SeqCst) => {
                Err(delegated_authorization_error_fixture("mcp"))
            }
            Behavior::SucceedThenLoseLease(lease) => {
                lease.0.store(false, Ordering::SeqCst);
                Ok(json!({"report": {"created": true}, "messages": []}))
            }
            Behavior::Gated(gate) => {
                gate.entered.notify_one();
                gate.release.notified().await;
                Ok(created_result())
            }
            Behavior::Succeed | Behavior::Authorization(_) => Ok(created_result()),
            Behavior::Fail => Err(adk_rust::AdkError::new(
                adk_rust::ErrorComponent::Tool,
                adk_rust::ErrorCategory::Internal,
                "tool.fixture_failed",
                "fixture failure",
            )),
        }
    }
}

fn created_result() -> Value {
    json!({
        "report": {"created": true},
        "messages": [{"role": "assistant", "content": "created"}],
    })
}

struct Resolver {
    tool: Arc<dyn Tool>,
    sensitive: Option<SensitiveToolPolicy>,
}

impl PipelineDirectToolResolver for Resolver {
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

fn sensitive_policy() -> SensitiveToolPolicy {
    let config = json!({
        "toolkit_security": {
            "sensitive_tools": {"customer_support": ["search_records"]},
            "sensitive_action_company_name": "Example Corp"
        }
    });
    ToolAdmissionPolicy::from_runtime_config(config.as_object().expect("runtime policy"))
        .expect("sensitive policy")
        .sensitive_tool("customer_support", "Customer Support", "search_records")
        .expect("sensitive action")
}

fn node(
    factory: &Arc<ScopedFactory>,
    behavior: Behavior,
    sensitive: bool,
) -> (DirectToolNode, Arc<AtomicUsize>) {
    build(factory.clone(), behavior, sensitive)
}

/// The effectful node over any journal factory, so real-journal proofs reuse this fixture.
pub(crate) fn journaled_node(
    factory: Arc<dyn NodeRecoveryFactory>,
    gate: Option<Arc<Gate>>,
    sensitive: bool,
) -> (Arc<dyn Node>, Arc<AtomicUsize>) {
    let behavior = gate.map_or(Behavior::Succeed, Behavior::Gated);
    let (node, calls) = build(factory, behavior, sensitive);
    (Arc::new(node), calls)
}

fn build(
    factory: Arc<dyn NodeRecoveryFactory>,
    behavior: Behavior,
    sensitive: bool,
) -> (DirectToolNode, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    let resolver = Arc::new(Resolver {
        tool: Arc::new(EffectTool {
            calls: Arc::clone(&calls),
            behavior,
        }),
        sensitive: sensitive.then(sensitive_policy),
    });
    let types = BTreeMap::from([
        ("report".to_owned(), "dict".to_owned()),
        ("messages".to_owned(), "list".to_owned()),
    ]);
    let definition = DirectToolNodeDefinition::from_yaml(NODE).expect("direct node");
    (
        DirectToolNode::new(definition, types, resolver).with_node_recovery(factory),
        calls,
    )
}

pub(crate) fn state() -> State {
    State::from([("input".to_owned(), json!(7))])
}

fn context(state: State) -> NodeContext {
    NodeContext::new(state, ExecutionConfig::new("root"), 2)
}

/// The journal activation this node derives for `context`, as a real journal sees it.
pub(crate) fn node_activation(context: &NodeContext) -> NodeAttemptActivation {
    NodeAttemptActivation::from_context("lookup", digest(), context).expect("activation")
}

fn digest() -> [u8; 32] {
    DirectToolNodeDefinition::from_yaml(NODE)
        .expect("direct node")
        .config_digest()
}

async fn journal_for(
    factory: &ScopedFactory,
    state: &State,
) -> (NodeAttemptJournal, NodeJournalSnapshot) {
    let activation =
        NodeAttemptActivation::from_context("lookup", digest(), &context(state.clone()))
            .expect("activation");
    let journal = factory
        .open(&activation, &NodeRecoveryPolicy::default())
        .await
        .expect("journal");
    let snapshot = journal.load().await.expect("snapshot");
    (journal, snapshot)
}

pub(crate) fn recovery_card(output: &NodeOutput) -> bool {
    matches!(
        &output.interrupt,
        Some(Interrupt::Dynamic { data: Some(data), .. })
            if data["guardrail_type"] == "pipeline_node_recovery"
    )
}

pub(crate) fn pause_data(output: NodeOutput) -> Value {
    let Some(Interrupt::Dynamic { data, .. }) = output.interrupt else {
        panic!("expected a dynamic interrupt");
    };
    data.expect("interrupt data")
}

pub(crate) fn with_decision(
    mut state: State,
    data: &Value,
    action: &str,
    value: Option<&str>,
) -> State {
    let mut decision = json!({
        "definition_digest": data["definition_digest"],
        "tool_call_id": data["tool_call_id"],
        "argument_digest": data["argument_digest"],
        "action": action,
    });
    if let Some(value) = value {
        decision["value"] = json!(value);
    }
    state.insert(
        DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
        json!({"lookup": decision}),
    );
    state
}

#[tokio::test]
async fn effectful_direct_tool_runs_once_and_its_committed_result_is_replayed() {
    let factory = scoped();
    let (node, calls) = node(&factory, Behavior::Succeed, false);
    let first = node.execute(&context(state())).await.unwrap();
    assert_eq!(first.updates["report"], json!({"created": true}));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let (_, snapshot) = journal_for(&factory, &state()).await;
    assert!(matches!(
        snapshot.ledger.phase(),
        NodeAttemptPhase::Completed { .. }
    ));
    // The step checkpoint was lost after the journal committed: re-entry projects it.
    let replay = node.execute(&context(state())).await.unwrap();
    assert_eq!(replay.updates, first.updates);
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn started_attempt_without_result_never_repeats_an_effectful_tool() {
    let factory = scoped();
    let (journal, initial) = journal_for(&factory, &state()).await;
    journal
        .append(&initial, initial.ledger.start_attempt(90).unwrap(), None)
        .await
        .unwrap();
    let (node, calls) = node(&factory, Behavior::Succeed, false);
    for _ in 0..2 {
        let output = node.execute(&context(state())).await.unwrap();
        assert!(recovery_card(&output));
        assert!(output.updates.is_empty());
    }
    assert_eq!(calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn effectful_tool_failure_stops_with_its_error_and_is_not_called_again() {
    let factory = scoped();
    let (node, calls) = node(&factory, Behavior::Fail, false);
    // The failing visit stops the pipeline with the typed tool error, as a read-only
    // tool does, instead of a recovery card the person cannot act on.
    let Err(error) = node.execute(&context(state())).await else {
        panic!("failed effectful tool call did not stop the node");
    };
    assert!(error.to_string().contains("tool_execution"), "{error}");
    // The journal keeps the uncertain effect: the same attempt is never dispatched again.
    let output = node.execute(&context(state())).await.unwrap();
    assert!(recovery_card(&output));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn effect_whose_result_cannot_be_recorded_is_reported_and_never_repeated() {
    let factory = scoped();
    let behavior = Behavior::SucceedThenLoseLease(Arc::clone(&factory.0.lease));
    let (node, calls) = node(&factory, behavior, false);
    let Err(error) = node.execute(&context(state())).await else {
        panic!("an unrecorded effect was reported as committed");
    };
    assert!(
        error.to_string().contains("result could not be recorded"),
        "{error}"
    );
    factory.0.lease.0.store(true, Ordering::SeqCst);
    let output = node.execute(&context(state())).await.unwrap();
    assert!(recovery_card(&output));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn sensitive_effectful_pause_leaves_no_started_attempt_and_approval_runs_once() {
    let factory = scoped();
    let (node, calls) = node(&factory, Behavior::Succeed, true);
    let paused = node.execute(&context(state())).await.unwrap();
    let data = pause_data(paused);
    assert_eq!(data["guardrail_type"], "sensitive_tool");
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    let (_, snapshot) = journal_for(&factory, &state()).await;
    assert!(snapshot.ledger.history().is_empty());

    let approved = with_decision(state(), &data, "approve", None);
    let output = node.execute(&context(approved.clone())).await.unwrap();
    assert_eq!(output.updates["report"], json!({"created": true}));
    assert_eq!(output.updates[DIRECT_TOOL_RESUME_STATE_KEY], json!({}));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    // The consumed approval cannot dispatch a second time after the same checkpoint.
    node.execute(&context(approved)).await.unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn blocked_sensitive_effectful_tool_stops_the_pipeline_without_a_journal_record() {
    for (action, comment) in [("reject", None), ("block_with_comment", Some("no"))] {
        let factory = scoped();
        let (node, calls) = node(&factory, Behavior::Succeed, true);
        let data = pause_data(node.execute(&context(state())).await.unwrap());
        let blocked = with_decision(state(), &data, action, comment);
        let output = node.execute(&context(blocked.clone())).await.unwrap();
        assert_eq!(output.goto, Some(vec![END.to_owned()]));
        assert!(!output.updates.contains_key("report"));
        assert!(
            output.updates["_pipeline_blocked"]
                .as_str()
                .is_some_and(|message| message.contains("was **blocked** by user"))
        );
        assert_eq!(calls.load(Ordering::SeqCst), 0);
        let (_, snapshot) = journal_for(&factory, &blocked).await;
        assert!(snapshot.ledger.history().is_empty());
        assert!(snapshot.updates.is_none());
    }
}

#[tokio::test]
async fn effectful_authorization_challenge_pauses_then_skip_stops_without_a_journal_record() {
    let factory = scoped();
    let authorized = Arc::new(AtomicBool::new(false));
    let (node, calls) = node(
        &factory,
        Behavior::Authorization(Arc::clone(&authorized)),
        false,
    );
    let challenge = node.execute(&context(state())).await.unwrap();
    let data = pause_data(challenge);
    assert_eq!(data["guardrail_type"], "mcp_auth");
    assert_eq!(calls.load(Ordering::SeqCst), 1);

    let skipped = with_decision(state(), &data, "skip", None);
    let output = node.execute(&context(skipped.clone())).await.unwrap();
    assert_eq!(output.goto, Some(vec![END.to_owned()]));
    assert_eq!(output.updates["report"], Value::Null);
    assert!(
        output.updates["_pipeline_blocked"]
            .as_str()
            .is_some_and(|message| message.contains("was skipped"))
    );
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let (_, snapshot) = journal_for(&factory, &skipped).await;
    assert!(snapshot.ledger.history().is_empty());

    // After authorization the effect runs once, journaled under its own activation.
    authorized.store(true, Ordering::SeqCst);
    let authorize = with_decision(state(), &data, "authorize", None);
    let output = node.execute(&context(authorize.clone())).await.unwrap();
    assert_eq!(output.updates["report"], json!({"created": true}));
    assert_eq!(calls.load(Ordering::SeqCst), 2);
    node.execute(&context(authorize)).await.unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn effectful_tool_requires_the_fenced_writer_before_any_call() {
    let factory = scoped();
    let (journaled, calls) = node(&factory, Behavior::Succeed, false);
    let unfenced = DirectToolNode::new(
        DirectToolNodeDefinition::from_yaml(NODE).expect("direct node"),
        BTreeMap::new(),
        Arc::new(Resolver {
            tool: Arc::new(EffectTool {
                calls: Arc::clone(&calls),
                behavior: Behavior::Succeed,
            }),
            sensitive: None,
        }),
    );
    let error = unfenced.execute(&context(state())).await.err().unwrap();
    assert!(
        error
            .to_string()
            .contains("requires its current fenced node writer")
    );
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    journaled.execute(&context(state())).await.unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1);
}
