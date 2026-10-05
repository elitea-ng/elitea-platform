use std::sync::Arc;

use adk_rust::graph::{Checkpointer, GraphError, MemoryCheckpointer};
use async_trait::async_trait;

use super::code_runtime::{CodeInvocation, CodeSandboxRuntime};
use super::compiler::{PipelineDefinition, PipelineNodeRuntimes};

const CODE: &str = "state:\n  answer: int\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: '7'\n    output: [answer]\n    transition: END\n";
const POLICY: &str = "    recovery:\n      max_attempts: 3\n      retry_on: [dependency_unavailable]\n      backoff: {initial_ms: 10, maximum_ms: 50, multiplier: 2}\n      max_elapsed_ms: 1000\n";
const ERROR_ROUTE: &str = "state:\n  answer: int\n  node_error: dict\nentry_point: run\nnodes:\n  - id: run\n    type: code\n    code: '7'\n    output: [answer]\n    recovery:\n      on_failure:\n        route: fallback\n        error_input: node_error\n        classes: [invalid_input]\n    transition: END\n  - id: fallback\n    type: code\n    code: '8'\n    input: [node_error]\n    output: [answer]\n    failure_handler: {error_input: node_error}\n    transition: END\n";

#[test]
fn node_recovery_compiler_preserves_absent_policy_digest_and_binds_explicit_policy() {
    let baseline = PipelineDefinition::from_yaml(CODE).unwrap();
    let same =
        PipelineDefinition::from_yaml(&CODE.replace("answer: int", "answer: {type: int}")).unwrap();
    assert_eq!(baseline.definition_digest(), same.definition_digest());
    let configured = CODE.replace(
        "    transition: END",
        &format!("{POLICY}    transition: END"),
    );
    assert_ne!(
        baseline.definition_digest(),
        PipelineDefinition::from_yaml(&configured)
            .unwrap()
            .definition_digest()
    );
    assert!(
        PipelineDefinition::from_yaml(&configured.replace("max_attempts: 3", "max_attempts: 17"))
            .is_err()
    );
    assert!(
        PipelineDefinition::from_yaml(&configured.replace(
            "retry_on: [dependency_unavailable]",
            "retry_on: [authorization_denied]"
        ))
        .is_err()
    );
    assert!(
        PipelineDefinition::from_yaml(&configured.replace(
            "max_elapsed_ms: 1000",
            "max_elapsed_ms: 1000\n      typo: true"
        ))
        .is_err()
    );
    assert!(
        PipelineDefinition::from_yaml(&configured.replace(
            "max_attempts: 3",
            "max_attempts: 3\n      retry_mode: operator"
        ))
        .is_ok()
    );
}

#[test]
fn node_recovery_compiler_rejects_failure_handlers_that_consume_success_or_reenter_business_paths()
{
    PipelineDefinition::from_yaml(ERROR_ROUTE).unwrap();
    for unsafe_yaml in [
        ERROR_ROUTE.replace("input: [node_error]", "input: [node_error, answer]"),
        ERROR_ROUTE.replace("node_error: dict", "node_error: string"),
        ERROR_ROUTE.replace(
            "classes: [invalid_input]",
            "classes: [authorization_denied]",
        ),
        ERROR_ROUTE.replace("classes: [invalid_input]", "classes: [cancelled]"),
        ERROR_ROUTE.replace("classes: [invalid_input]", "classes: [lease_lost]"),
        ERROR_ROUTE.replacen("transition: END", "transition: fallback", 1),
        ERROR_ROUTE.replace("entry_point: run", "entry_point: fallback"),
        ERROR_ROUTE.replace("code: '8'", "code: {type: variable, value: answer}"),
        ERROR_ROUTE.replace(
            "failure_handler: {error_input: node_error}",
            "failure_handler: {error_input: answer}",
        ),
        ERROR_ROUTE.replace("route: fallback", "route: run"),
    ] {
        assert!(PipelineDefinition::from_yaml(&unsafe_yaml).is_err());
    }
}

struct Sandbox;
#[async_trait]
impl CodeSandboxRuntime for Sandbox {
    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        panic!("compile admission must not dispatch")
    }
}

#[test]
fn node_recovery_compiler_requires_current_writer_authority_before_any_dispatch() {
    let configured = CODE.replace(
        "    transition: END",
        &format!("{POLICY}    transition: END"),
    );
    let definition = PipelineDefinition::from_yaml(&configured).unwrap();
    let runtimes = PipelineNodeRuntimes::default().with_code(Arc::new(Sandbox));
    assert!(
        definition
            .compile_subgraph_with_runtime(Arc::new(MemoryCheckpointer::default()), &runtimes)
            .is_err()
    );
    assert!(
        PipelineDefinition::from_yaml(CODE)
            .unwrap()
            .compile_subgraph_with_runtime(Arc::new(MemoryCheckpointer::default()), &runtimes)
            .is_ok()
    );
}

// Exercise the actual compiler, Code body, ADK frontier and immutable journal.
// This store substitutes only persistence; it does not mint operator authority.
#[derive(Default)]
struct FrontierStore {
    memory: MemoryCheckpointer,
    append_lock: tokio::sync::Mutex<()>,
}

#[async_trait]
impl adk_rust::graph::Checkpointer for FrontierStore {
    async fn save(&self, checkpoint: &adk_rust::graph::Checkpoint) -> Result<String, GraphError> {
        self.memory.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<adk_rust::graph::Checkpoint>, GraphError> {
        self.memory.load(thread).await
    }
    async fn load_by_id(
        &self,
        id: &str,
    ) -> Result<Option<adk_rust::graph::Checkpoint>, GraphError> {
        self.memory.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<adk_rust::graph::Checkpoint>, GraphError> {
        self.memory.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.memory.delete(thread).await
    }
}

#[async_trait]
impl super::parallel::ParallelCheckpointAppender for FrontierStore {
    async fn append_after(
        &self,
        expected: Option<&adk_rust::graph::Checkpoint>,
        candidate: &adk_rust::graph::Checkpoint,
    ) -> Result<String, GraphError> {
        let _guard = self.append_lock.lock().await;
        if let Some(stored) = self.memory.load_by_id(&candidate.checkpoint_id).await? {
            assert_eq!(
                serde_json::to_value(stored).unwrap(),
                serde_json::to_value(candidate).unwrap()
            );
            return Ok(candidate.checkpoint_id.clone());
        }
        let latest = self.memory.load(&candidate.thread_id).await?;
        if serde_json::to_value(latest).unwrap() != serde_json::to_value(expected).unwrap() {
            return Err(GraphError::Other("fixture.current_writer_cas".into()));
        }
        self.memory.save(candidate).await
    }
}

struct FrontierFactory {
    store: Arc<FrontierStore>,
    lease: Arc<crate::state::TestStateWriterLease>,
}

#[async_trait]
impl super::node_recovery_runtime::NodeRecoveryFactory for FrontierFactory {
    #[allow(
        clippy::format_collect,
        reason = "Keep independent hexadecimal fixture generation separate from production helpers."
    )]
    async fn open(
        &self,
        activation: &super::node_recovery_runtime::NodeAttemptActivation,
        policy: &super::node_recovery::NodeRecoveryPolicy,
    ) -> Result<super::node_recovery_runtime::NodeAttemptJournal, GraphError> {
        let bytes = serde_json::to_vec(&(
            &activation.root_thread_id,
            &activation.node_id,
            activation.step,
            activation.node_digest,
            activation.input_digest,
        ))
        .unwrap();
        let id: [u8; 32] = ring::digest::digest(&ring::digest::SHA256, &bytes)
            .as_ref()
            .try_into()
            .unwrap();
        let thread = format!(
            "fixture-attempt-{}",
            id.iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>()
        );
        Ok(super::node_recovery_runtime::NodeAttemptJournal::bound(
            self.store.clone(),
            self.lease.clone(),
            thread,
            id,
            policy.clone(),
        ))
    }
}

#[derive(Default)]
struct FrontierSandbox {
    dispatches: std::sync::Mutex<std::collections::BTreeMap<String, usize>>,
}

#[async_trait]
impl CodeSandboxRuntime for FrontierSandbox {
    async fn execute(&self, _: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        panic!("durable typed attempt admission is required")
    }
    async fn execute_attempt(
        &self,
        invocation: CodeInvocation<'_>,
        authority: &super::node_recovery_runtime::NodeAttemptAuthority,
    ) -> Result<Vec<u8>, super::code_runtime::CodeAttemptFailure> {
        let mut calls = self.dispatches.lock().unwrap();
        let count = calls.entry(invocation.source.to_owned()).or_default();
        *count += 1;
        assert_eq!(authority.attempt() as usize, *count);
        if *count == 1 {
            return Err(super::code_runtime::CodeAttemptFailure::new(
                super::code_runtime::CodeAttemptPhase::Preparation,
                super::node_recovery::NodeFailureClass::DependencyUnavailable,
                super::node_recovery::ReplaySafety::NoExternalEffect,
            ));
        }
        let result: i64 = invocation.source.parse().unwrap();
        Ok(serde_json::to_vec(&serde_json::json!({
            "revision":1,"status":"completed","exit_code":0,"stderr":"",
            "stdout":serde_json::json!({"revision":1,"result":result}).to_string()
        }))
        .unwrap())
    }
}

struct FrontierOperator;
#[async_trait]
impl super::node_recovery_runtime::NodeRecoveryOperatorAuthorizer for FrontierOperator {
    async fn authorize_retry(
        &self,
        receipt: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
        request: &super::node_recovery::OperatorRetryRequest,
    ) -> Result<(), GraphError> {
        assert_eq!(receipt.journal_revision, request.expected_revision);
        assert_eq!(
            receipt.allowed_actions,
            [super::node_recovery_receipt::NodeRecoveryAction::Retry]
        );
        Ok(())
    }
}

fn frontier_receipt(
    error: GraphError,
) -> super::node_recovery_receipt::NodeRecoveryRequiredReceipt {
    let GraphError::Interrupted(interrupted) = error else {
        panic!("expected durable interruption");
    };
    let adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(data), ..
    } = interrupted.interrupt
    else {
        panic!("expected typed recovery interruption");
    };
    serde_json::from_value(data["receipt"].clone()).unwrap()
}

async fn approve_frontier(
    definition: &PipelineDefinition,
    factory: &FrontierFactory,
    checkpoint: &adk_rust::graph::Checkpoint,
    config: &adk_rust::graph::ExecutionConfig,
    receipt: &super::node_recovery_receipt::NodeRecoveryRequiredReceipt,
    request_byte: u8,
) -> super::node_recovery_runtime::AppliedNodeRecoveryAction {
    use super::node_recovery_runtime::{NodeAttemptActivation, NodeRecoveryFactory};
    let (digest, policy) = definition.node_recovery_spec(&receipt.node_id).unwrap();
    let context = adk_rust::graph::NodeContext::new(
        checkpoint.state.clone(),
        config.clone(),
        checkpoint.step,
    );
    let activation =
        NodeAttemptActivation::from_context(&receipt.node_id, digest, &context).unwrap();
    let journal = factory.open(&activation, &policy).await.unwrap();
    journal
        .verify_pending_receipt(&activation, receipt)
        .await
        .unwrap();
    let activation_id = std::array::from_fn(|index| {
        u8::from_str_radix(&receipt.activation_id[index * 2..index * 2 + 2], 16).unwrap()
    });
    let request = super::node_recovery::OperatorRetryRequest {
        request_id: [request_byte; 32],
        activation_id,
        expected_revision: receipt.journal_revision,
    };
    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis()
        .try_into()
        .unwrap();
    let applied = journal
        .resume_operator_retry(&activation, request, &FrontierOperator, now_ms)
        .await
        .unwrap();
    journal
        .verify_applied_revision(applied.applied_revision())
        .await
        .unwrap();
    // Lost ACK re-applies neither attempt nor business updates.
    let replay = journal
        .resume_operator_retry(&activation, request, &FrontierOperator, now_ms)
        .await
        .unwrap();
    assert_eq!(replay.applied_revision(), applied.applied_revision());
    applied
}

#[tokio::test]
#[allow(
    clippy::items_after_statements,
    reason = "Keep local fixture types beside their exact validation checks."
)]
#[allow(
    clippy::too_many_lines,
    reason = "Keep the complete identity and failure assertions in one fixture."
)]
async fn node_recovery_actual_frontier_distinguishes_applied_a_from_unpublished_b() {
    use adk_rust::graph::{Checkpointer, ExecutionConfig, State};
    let yaml = "state:\n  a_result: {type: int, value: -1}\n  b_result: {type: int, value: -2}\nentry_point: A\nnodes:\n  - id: A\n    type: code\n    code: '41'\n    output: [a_result]\n    transition: B\n  - id: B\n    type: code\n    code: '43'\n    output: [b_result]\n    transition: END\n";
    let operator = "    recovery:\n      max_attempts: 3\n      retry_mode: operator\n      retry_on: [dependency_unavailable]\n      backoff: {initial_ms: 1, maximum_ms: 1, multiplier: 1}\n      max_elapsed_ms: 60000\n";
    let yaml = yaml.replace("    transition:", &format!("{operator}    transition:"));
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    let store = Arc::new(FrontierStore::default());
    let factory = Arc::new(FrontierFactory {
        store: store.clone(),
        lease: Arc::new(crate::state::TestStateWriterLease::current()),
    });
    let sandbox = Arc::new(FrontierSandbox::default());
    let runtimes = PipelineNodeRuntimes::default()
        .with_code(sandbox.clone())
        .with_node_recovery_authority(factory.clone());
    let graph = definition
        .compile_subgraph_with_runtime(store.clone(), &runtimes)
        .unwrap();
    let config = ExecutionConfig::new("fixture-5b-root");
    let a = frontier_receipt(
        graph
            .invoke(State::new(), config.clone())
            .await
            .unwrap_err(),
    );
    let pending_a = store.load(&config.thread_id).await.unwrap().unwrap();
    assert_eq!(pending_a.pending_nodes, ["A"]);
    assert_eq!(a.node_id, "A");
    assert_eq!(a.step, pending_a.step as u64);
    assert_eq!(
        pending_a.state.get("a_result"),
        Some(&serde_json::json!(-1))
    );
    assert_eq!(
        pending_a.state.get("b_result"),
        Some(&serde_json::json!(-2))
    );
    let action = approve_frontier(&definition, &factory, &pending_a, &config, &a, 1).await;
    assert_eq!(action.applied_revision(), a.journal_revision + 1);
    let applied_journal_a = store
        .load(&format!("fixture-attempt-{}", a.activation_id))
        .await
        .unwrap()
        .unwrap();
    // Before ACK/root dispatch, the exact old A frontier remains durable.
    assert_eq!(
        store
            .load(&config.thread_id)
            .await
            .unwrap()
            .unwrap()
            .checkpoint_id,
        pending_a.checkpoint_id
    );
    assert_eq!(sandbox.dispatches.lock().unwrap().get("41"), Some(&1));

    // An authenticated restore would now run the original frontier, never new input.
    let b = frontier_receipt(
        graph
            .invoke(State::new(), config.clone())
            .await
            .unwrap_err(),
    );
    let pending_b = store.load(&config.thread_id).await.unwrap().unwrap();
    assert_eq!(pending_b.pending_nodes, ["B"]);
    assert_eq!(b.node_id, "B");
    assert_eq!(b.step, pending_b.step as u64);
    assert!(pending_b.step > pending_a.step);
    assert_eq!(
        pending_b.state.get("a_result"),
        Some(&serde_json::json!(41))
    );
    assert_eq!(
        pending_b.state.get("b_result"),
        Some(&serde_json::json!(-2))
    );
    assert_ne!(a.activation_id, b.activation_id);
    let a_receipt_wire = serde_json::to_vec(&serde_json::to_value(&a).unwrap()).unwrap();
    let b_receipt_wire = serde_json::to_vec(&serde_json::to_value(&b).unwrap()).unwrap();
    assert_ne!(
        ring::digest::digest(&ring::digest::SHA256, &a_receipt_wire).as_ref(),
        ring::digest::digest(&ring::digest::SHA256, &b_receipt_wire).as_ref(),
    );
    assert_eq!(
        *sandbox.dispatches.lock().unwrap(),
        std::collections::BTreeMap::from([("41".into(), 2), ("43".into(), 1)])
    );
    let pending_journal_b = store
        .load(&format!("fixture-attempt-{}", b.activation_id))
        .await
        .unwrap()
        .unwrap();
    use super::node_recovery_runtime::{NodeAttemptActivation, NodeRecoveryFactory};
    let (b_digest, b_policy) = definition.node_recovery_spec("B").unwrap();
    let b_context =
        adk_rust::graph::NodeContext::new(pending_b.state.clone(), config.clone(), pending_b.step);
    let b_activation = NodeAttemptActivation::from_context("B", b_digest, &b_context).unwrap();
    let b_journal = factory.open(&b_activation, &b_policy).await.unwrap();
    assert!(
        b_journal
            .verify_pending_receipt(&b_activation, &a)
            .await
            .is_err()
    );
    println!(
        "NODE_FRONTIER_EVIDENCE_V1 {}",
        serde_json::json!({
            "schema":"elitea.pipeline.node-recovery-offline-frontier-evidence.v1",
            "persistence":"memory_checkpointer_current_writer_cas_fixture",
            "definition_digest":definition.definition_digest(),
            "a_receipt":serde_json::to_value(&a).unwrap(),
            "pending_a_before_ack":pending_a,
            "applied_a_journal_before_ack":applied_journal_a,
            "b_receipt_before_main_publication":serde_json::to_value(&b).unwrap(),
            "pending_b_before_main_publication":pending_b,
            "pending_b_journal_before_main_publication":pending_journal_b,
            "dispatch_counts_at_b":{"A":2,"B":1},
            "old_a_receipt_at_b":"rejected"
        })
    );
    // Model checkpoint inspection may rediscover this B receipt after a lost publication.
    // It cannot use Main's old A action to approve or replay B.
    let b_again = frontier_receipt(
        graph
            .invoke(State::new(), config.clone())
            .await
            .unwrap_err(),
    );
    assert_eq!(b, b_again);
    assert_eq!(
        *sandbox.dispatches.lock().unwrap(),
        std::collections::BTreeMap::from([("41".into(), 2), ("43".into(), 1)])
    );
    approve_frontier(&definition, &factory, &pending_b, &config, &b, 2).await;
    let completed = graph.invoke(State::new(), config).await.unwrap();
    assert_eq!(completed.get("a_result"), Some(&serde_json::json!(41)));
    assert_eq!(completed.get("b_result"), Some(&serde_json::json!(43)));
    assert_eq!(
        *sandbox.dispatches.lock().unwrap(),
        std::collections::BTreeMap::from([("41".into(), 2), ("43".into(), 2)])
    );
}
