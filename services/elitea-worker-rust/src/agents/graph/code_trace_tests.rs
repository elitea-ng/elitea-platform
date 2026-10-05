use super::super::{
    code::CodeNodeDefinition,
    code_runtime::{CodeInvocation, CodeNode, CodeSandboxRuntime},
    node_events::pipeline_node_event_channel,
};
use super::*;
use adk_rust::graph::{ExecutionConfig, Node, State};
use serde_json::json;
use std::{collections::BTreeMap, sync::Arc};

fn context(thread: &str, step: usize) -> NodeContext {
    NodeContext::new(
        State::from([
            ("count".into(), json!(2)),
            ("private_state".into(), json!("never trace")),
        ]),
        ExecutionConfig::new(thread),
        step,
    )
}
struct Runtime;
#[async_trait::async_trait]
impl CodeSandboxRuntime for Runtime {
    async fn execute(&self, invocation: CodeInvocation<'_>) -> Result<Vec<u8>, GraphError> {
        assert_eq!(invocation.source, "source-private");
        let trace = invocation
            .trace
            .as_ref()
            .expect("actual CodeNode trace context")
            .bind(("execution-1", 7))?;
        observe(Some(&trace), CodePhase::Preparation, async { Ok(()) }).await?;
        observe(Some(&trace), CodePhase::Hydration, async { Ok(()) }).await?;
        observe(Some(&trace), CodePhase::Execution, async { Ok(serde_json::to_vec(&json!({"revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":7}","stderr":""})).unwrap()) }).await
    }
}
#[tokio::test]
async fn actual_code_node_emits_distinct_phase_boundaries_without_payloads() {
    let (sender, receiver) = pipeline_node_event_channel();
    let node = CodeNode::new(CodeNodeDefinition::from_yaml("id: Code_1\ntype: code\ncode: source-private\ninput: [count]\noutput: [count]\ntransition: END\n").unwrap(), BTreeMap::from([("count".into(),"int".into())]), Arc::new(Runtime)).unwrap().with_events(Some(sender));
    let result = node
        .execute(&context("root/child/grandchild", 4))
        .await
        .unwrap();
    assert_eq!(result.updates, State::from([("count".into(), json!(7))]));
    let mut drain = receiver
        .drain("original-invocation", "root-agent", "")
        .await
        .unwrap();
    let mut runs = std::collections::BTreeSet::new();
    for phase in [
        CodePhase::Preparation,
        CodePhase::Hydration,
        CodePhase::Execution,
    ] {
        let mut pair = Vec::new();
        for status in [CodePhaseStatus::Started, CodePhaseStatus::Completed] {
            let event = drain.try_recv().unwrap().unwrap();
            let encoded = serde_json::to_string(&event).unwrap();
            assert!(!encoded.contains("source-private"));
            assert!(!encoded.contains("private_state"));
            assert!(!encoded.contains("never trace"));
            assert_eq!(event.invocation_id, "original-invocation");
            let trace = CodeTraceEvent::from_event(&event).unwrap().unwrap();
            assert_eq!(trace.lifecycle.execution_id, "execution-1");
            assert_eq!(trace.lifecycle.generation, "7");
            assert_eq!(trace.lifecycle.graph_thread_id, "root/child/grandchild");
            assert_eq!(trace.lifecycle.graph_step, "4");
            assert_eq!(trace.lifecycle.phase, phase);
            assert_eq!(trace.lifecycle.status, status);
            pair.push(trace.lifecycle.run_id());
        }
        assert_eq!(pair[0], pair[1]);
        runs.insert(pair.remove(0));
    }
    assert_eq!(runs.len(), 3);
    assert!(drain.try_recv().is_none());
}
#[tokio::test]
async fn interrupted_observation_has_no_completion_or_cleanup_claim() {
    let (sender, receiver) = pipeline_node_event_channel();
    let trace = CodeTraceContext::new(
        "Code_1",
        CodeLanguage::Python,
        &[7; 32],
        &context("root", 4),
        sender,
    )
    .unwrap();
    let bound = trace.bind(("execution-1", 7)).unwrap();
    let mut observation = Box::pin(observe(
        Some(&bound),
        CodePhase::Execution,
        std::future::pending::<Result<(), GraphError>>(),
    ));
    assert!(
        tokio::time::timeout(std::time::Duration::from_millis(10), &mut observation)
            .await
            .is_err()
    );
    drop(observation);
    let mut drain = receiver
        .drain("original-invocation", "root-agent", "")
        .await
        .unwrap();
    let event = drain.try_recv().unwrap().unwrap();
    assert_eq!(
        CodeTraceEvent::from_event(&event)
            .unwrap()
            .unwrap()
            .lifecycle
            .status,
        CodePhaseStatus::Started
    );
    assert!(drain.try_recv().is_none());
}
#[tokio::test]
async fn phase_failure_uses_safe_status_and_keeps_original_scope() {
    let (sender, receiver) = pipeline_node_event_channel();
    let mut ctx = context("root/child/grandchild", 4);
    let scope = PipelineNodeEventScope::new(
        "original-child-call",
        "child-agent",
        "root/child/grandchild",
    )
    .unwrap();
    ctx.state.insert(
        PIPELINE_NODE_EVENT_SCOPE_STATE_KEY.into(),
        scope.to_state_value().unwrap(),
    );
    let trace =
        CodeTraceContext::new("Code_1", CodeLanguage::Rust, &[7; 32], &ctx, sender).unwrap();
    let bound = trace.bind(("execution-1", 7)).unwrap();
    let result: Result<(), _> = observe(Some(&bound), CodePhase::Preparation, async {
        Err(GraphError::Other("private-registry-credential".into()))
    })
    .await;
    assert!(result.is_err());
    let mut drain = receiver
        .drain("original-invocation", "root-agent", "")
        .await
        .unwrap();
    let _ = drain.try_recv().unwrap().unwrap();
    let end = drain.try_recv().unwrap().unwrap();
    assert_eq!(end.author, "child-agent");
    assert_eq!(
        end.provider_metadata[crate::agents::events::DESCENDANT_PARENT_CALL_KEY],
        "original-child-call"
    );
    assert!(
        !serde_json::to_string(&end)
            .unwrap()
            .contains("private-registry-credential")
    );
    assert_eq!(
        CodeTraceEvent::from_event(&end)
            .unwrap()
            .unwrap()
            .lifecycle
            .status,
        CodePhaseStatus::Failed
    );
}
#[test]
fn phase_identity_is_stable_across_recovery_and_distinct_across_loop_visits() {
    let (sender, _) = pipeline_node_event_channel();
    let trace = CodeTraceContext::new(
        "Code_1",
        CodeLanguage::Python,
        &[0xaa; 32],
        &context("root/child/grandchild", 4),
        sender,
    )
    .unwrap();
    let bound = trace.bind(("execution-1", 7)).unwrap();
    let start = bound.lifecycle(CodePhase::Execution, CodePhaseStatus::Started);
    let end = bound.lifecycle(CodePhase::Execution, CodePhaseStatus::Completed);
    assert_eq!(start.run_id(), end.run_id());
    assert_eq!(
        start.run_id(),
        "code-8233f911d1d8a5ee2ab8028e7c6ff62a336823bb7ea755524e4a845d3c987cab"
    );
    let mut next = start.clone();
    next.activation_id = "b".repeat(64);
    next.graph_step = "5".into();
    assert_ne!(next.run_id(), start.run_id());
}
