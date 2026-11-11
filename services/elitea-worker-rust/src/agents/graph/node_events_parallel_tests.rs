use super::*;
use crate::agents::graph::node_events::{pipeline_node_event_channel, pipeline_node_signal_event};
use adk_rust::graph::GraphError;
use adk_rust::{AdkError, ErrorCategory, ErrorComponent};
use serde_json::json;
use std::sync::Arc;

fn parent() -> PipelineNodeEventScope {
    PipelineNodeEventScope::new("outer-call", "saved graph", "root/graph").unwrap()
}
fn threads() -> BTreeSet<String> {
    [
        "hashed-branch",
        "hashed-branch/owned",
        "hashed-branch/owned/inner",
    ]
    .map(str::to_owned)
    .into_iter()
    .collect()
}
fn binding(parent: Option<PipelineNodeEventScope>) -> ParallelEventScope {
    ParallelEventScope::from_catalog("root/graph", "hashed-branch", "owned", threads(), parent)
        .unwrap()
}
fn sender(
    scope: ParallelEventScope,
) -> (
    PipelineNodeEventSender,
    super::super::PipelineNodeEventReceiver,
) {
    let (mut sender, receiver) = pipeline_node_event_channel();
    sender.parallel_scope = Some(Arc::new(scope));
    (sender, receiver)
}

#[test]
fn parallel_scope_admits_only_factory_catalog_and_exact_parent_root() {
    assert!(
        ParallelEventScope::from_catalog(
            "root/other",
            "hashed-branch",
            "owned",
            threads(),
            Some(parent())
        )
        .is_err()
    );
    assert!(
        ParallelEventScope::from_catalog(
            "root/graph",
            "root/graph",
            "owned",
            threads(),
            Some(parent())
        )
        .is_err()
    );
    assert!(
        ParallelEventScope::from_catalog(
            "root/graph",
            "hashed-branch",
            "own",
            threads(),
            Some(parent())
        )
        .is_err()
    );
    let mut missing = threads();
    missing.remove("hashed-branch/owned");
    assert!(
        ParallelEventScope::from_catalog(
            "root/graph",
            "hashed-branch",
            "owned",
            missing,
            Some(parent())
        )
        .is_err()
    );
    let mut cycle = threads();
    cycle.insert("root/graph".to_owned());
    assert!(
        ParallelEventScope::from_catalog(
            "root/graph",
            "hashed-branch",
            "owned",
            cycle.clone(),
            None
        )
        .is_err()
    );
    assert!(
        ParallelEventScope::from_catalog(
            "root/graph",
            "hashed-branch",
            "owned",
            cycle,
            Some(parent())
        )
        .is_err()
    );
    let mut oversized = threads();
    for n in 0..MAX_BRANCH_THREADS {
        oversized.insert(format!("thread-{n}"));
    }
    assert!(
        ParallelEventScope::from_catalog(
            "root/graph",
            "hashed-branch",
            "owned",
            oversized,
            Some(parent())
        )
        .is_err()
    );
}

#[test]
fn hashed_edge_is_sidecar_only_and_local_scopes_keep_static_bounds() {
    let local = PipelineNodeEventScope::new("local-call", "owned", "hashed-branch/owned").unwrap();
    assert!(local.clone().with_parent(Some(parent())).is_err());
    let scope = binding(Some(parent()));
    assert!(scope.validate_local(Some(&local)).is_ok());
    let neighboring =
        PipelineNodeEventScope::new("neighbor", "neighbor", "hashed-branch/owned_suffix").unwrap();
    assert!(scope.validate_local(Some(&neighboring)).is_err());
    let skipped =
        PipelineNodeEventScope::new("inner", "inner", "hashed-branch/owned/inner").unwrap();
    assert!(scope.validate_local(Some(&skipped)).is_err());
    let nested_parent = parent()
        .with_parent(Some(
            PipelineNodeEventScope::new("root-call", "root", "root").unwrap(),
        ))
        .unwrap();
    let local_inner = PipelineNodeEventScope::new("inner", "inner", "hashed-branch/owned/inner")
        .unwrap()
        .with_parent(Some(local))
        .unwrap();
    assert!(
        binding(Some(nested_parent))
            .validate_local(Some(&local_inner))
            .is_err()
    );
}

#[tokio::test]
async fn parallel_events_preserve_ancestor_attribution_and_original_identity() {
    let (sender, receiver) = sender(binding(Some(parent())));
    assert!(
        sender
            .inherited_event_scope("hashed-branch_suffix", None)
            .is_err()
    );
    assert!(
        sender
            .inherited_event_scope("hashed-branch/owned", None)
            .is_err()
    );
    let inherited = sender
        .inherited_event_scope("hashed-branch", None)
        .unwrap()
        .unwrap();
    assert_eq!(inherited.parent_call_id(), "outer-call");
    let local = PipelineNodeEventScope::new("local-call", "owned", "hashed-branch/owned").unwrap();
    assert!(
        sender
            .inherited_event_scope("hashed-branch", Some(&local))
            .is_err()
    );
    assert_eq!(
        sender
            .inherited_event_scope("hashed-branch/owned", Some(&local))
            .unwrap()
            .unwrap()
            .parent_call_id(),
        "outer-call"
    );
    let mut original = Event::new("saved-original");
    original.invocation_id = "pipeline-child:outer-call".to_owned();
    original.author = "saved graph".to_owned();
    original.branch = "application.application_12".to_owned();
    original
        .provider_metadata
        .insert("receipt".to_owned(), "immutable".to_owned());
    let immutable = serde_json::to_value(&original).unwrap();
    sender
        .send_original_application_event(original.clone(), Some(&local), "root-invocation")
        .await
        .unwrap();
    let mut channel = receiver.inner.lock().await.take().unwrap();
    let projected = pipeline_node_signal_event(
        channel.recv().await.unwrap(),
        "root-invocation",
        "root agent",
        "application",
    )
    .unwrap();
    assert_eq!(
        projected.provider_metadata[DESCENDANT_CONTAINER_INVOCATION_KEY],
        "pipeline-child:outer-call"
    );
    assert_eq!(
        projected.provider_metadata[DESCENDANT_PARENT_CALL_KEY],
        "local-call"
    );
    assert_eq!(
        projected.provider_metadata[DESCENDANT_CHECKPOINT_THREAD_KEY],
        "hashed-branch/owned"
    );
    // FU-264: the member's own child is named against the proved branch
    // thread, as its node events are.
    assert_eq!(
        serde_json::from_str::<serde_json::Value>(
            &projected.provider_metadata[crate::agents::events::FANOUT_MEMBER_THREAD_KEY]
        )
        .unwrap(),
        serde_json::json!({"call": "local-call", "thread": "hashed-branch"})
    );
    let mut restored = projected;
    for key in [
        DESCENDANT_CONTAINER_INVOCATION_KEY,
        DESCENDANT_PARENT_CALL_KEY,
        DESCENDANT_CHECKPOINT_THREAD_KEY,
        crate::agents::events::FANOUT_MEMBER_THREAD_KEY,
    ] {
        restored.provider_metadata.remove(key);
    }
    assert_eq!(serde_json::to_value(restored).unwrap(), immutable);
    assert_eq!(serde_json::to_value(&original).unwrap(), immutable);
    sender
        .send("answer", Some(&local), Event::new("node-progress"))
        .await
        .unwrap();
    let node = pipeline_node_signal_event(
        channel.recv().await.unwrap(),
        "root-invocation",
        "root agent",
        "application",
    )
    .unwrap();
    assert_eq!(node.invocation_id, "pipeline-child:local-call");
    assert_eq!(
        node.provider_metadata[DESCENDANT_CONTAINER_INVOCATION_KEY],
        "pipeline-child:outer-call"
    );
    assert_eq!(node.author, "owned");
}

#[test]
fn routed_leaf_retains_its_nearest_edge_and_refuses_partial_or_unknown_threads() {
    let (sender, _) = sender(binding(Some(parent())));
    let mut event = Event::new("ordinary-leaf");
    event.provider_metadata.insert(
        DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
        "ordinary-child".to_owned(),
    );
    event.provider_metadata.insert(
        DESCENDANT_PARENT_CALL_KEY.to_owned(),
        "ordinary-call".to_owned(),
    );
    event.provider_metadata.insert(
        DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
        "hashed-branch/owned/inner".to_owned(),
    );
    let original = json!(event);
    sender
        .project_parallel_application_event(&mut event, None, "root-invocation", None)
        .unwrap();
    assert_eq!(json!(event), original);
    event.provider_metadata.insert(
        DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
        "hashed-branch/owned_suffix".to_owned(),
    );
    assert!(
        sender
            .project_parallel_application_event(&mut event, None, "root-invocation", None)
            .is_err()
    );
    event.provider_metadata.remove(DESCENDANT_PARENT_CALL_KEY);
    assert!(
        sender
            .project_parallel_application_event(&mut event, None, "root-invocation", None)
            .is_err()
    );
}

/// An Agent child's leaf has no checkpoint thread: its activation receipt
/// (stamped by the scoped runtime) proves the branch thread instead.
#[test]
fn agent_child_leaf_is_proved_by_its_activation_receipt() {
    use crate::agents::pipeline::scoped_applications::PipelineApplicationScope;
    let definition = crate::agents::graph::compiler::PipelineDefinition::from_yaml(
        "state: {answer: str}\nentry_point: child\nnodes:\n  - id: child\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n",
    )
    .unwrap();
    let scope = PipelineApplicationScope::new(String::new(), &definition, None).unwrap();
    let leaf = |thread: &str| {
        let mut event = Event::new("agent-leaf");
        event.provider_metadata.insert(
            DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
            "agent-child".to_owned(),
        );
        event.provider_metadata.insert(
            DESCENDANT_PARENT_CALL_KEY.to_owned(),
            "agent-call".to_owned(),
        );
        scope
            .activate(thread, "child", 0)
            .unwrap()
            .stamp_with_branch(&mut event, "elitea.saved_applications".to_owned())
            .unwrap();
        event
    };
    let (sender, _) = sender(binding(Some(parent())));
    let mut event = leaf("hashed-branch/owned");
    let original = json!(event);
    sender
        .project_parallel_application_event(&mut event, None, "root-invocation", None)
        .unwrap();
    assert_eq!(json!(event), original, "the leaf keeps its own edge");
    // A receipt for a thread this branch does not own.
    let mut foreign = leaf("other-branch");
    assert!(
        sender
            .project_parallel_application_event(&mut foreign, None, "root-invocation", None)
            .is_err()
    );
    // No receipt: nothing proves the leaf.
    let mut bare = leaf("hashed-branch/owned");
    bare.provider_metadata.remove(
        crate::agents::pipeline::scoped_applications::PIPELINE_APPLICATION_SCOPE_METADATA_KEY,
    );
    assert!(
        sender
            .project_parallel_application_event(&mut bare, None, "root-invocation", None)
            .is_err()
    );
}

#[tokio::test]
async fn ordinary_sender_keeps_existing_root_projection() {
    let (sender, receiver) = pipeline_node_event_channel();
    sender
        .send("answer", None, Event::new("ordinary"))
        .await
        .unwrap();
    let mut channel = receiver.inner.lock().await.take().unwrap();
    let event = pipeline_node_signal_event(
        channel.recv().await.unwrap(),
        "root-invocation",
        "root agent",
        "application",
    )
    .unwrap();
    assert_eq!(event.invocation_id, "root-invocation");
    assert_eq!(event.author, "root agent");
    assert_eq!(event.branch, "application");
    assert!(
        !event
            .provider_metadata
            .contains_key(DESCENDANT_CHECKPOINT_THREAD_KEY)
    );
}

/// A fixed Parallel member's failure is its branch outcome: its failure
/// signals never reach the root stream, and an approved call that may have
/// started stays typed on the branch (browser finding, 2026-10-27).
#[tokio::test]
async fn a_member_failure_stays_in_its_branch_and_a_root_failure_ends_the_run() {
    let (mut member, receiver) = sender(binding(Some(parent())));
    member.fanout_member = true;
    member
        .send_execution_failure("pipeline.tool_unavailable")
        .await
        .unwrap();
    member
        .send_model_failure(&AdkError::new(
            ErrorComponent::Model,
            ErrorCategory::Internal,
            "model.failed",
            "failed",
        ))
        .await
        .unwrap();
    assert_eq!(receiver.try_failure_code().await, None);
    let stop = member
        .approved_call_stop(
            crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE,
            "rev",
        )
        .await
        .expect("a typed member stop");
    assert!(
        matches!(&stop, GraphError::NodeExecutionFailed { message, .. }
        if message.starts_with(crate::agents::pipeline::scoped_runtime::CALL_MAY_HAVE_STARTED_CODE))
    );
    assert_eq!(receiver.try_failure_code().await, None);

    let (root, receiver) = pipeline_node_event_channel();
    root.send_execution_failure("pipeline.tool_unavailable")
        .await
        .unwrap();
    assert_eq!(
        receiver.try_failure_code().await,
        Some("pipeline.tool_unavailable")
    );
}

/// A nested pipeline leaf of the member Agent on `agent_thread`, with its
/// receipt and the thread it names.
fn nested_leaf(
    agent_thread: &str,
    call: &crate::agents::application_pipeline::PipelineToolCallLineage,
    named: &str,
) -> Event {
    let mut event = Event::new("nested-leaf");
    event.provider_metadata.insert(
        DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
        "member-agent".to_owned(),
    );
    event
        .provider_metadata
        .insert(DESCENDANT_PARENT_CALL_KEY.to_owned(), "call-1".to_owned());
    crate::agents::graph::tool_pipeline_threads::stamp_durable_tool_thread(
        &mut event,
        agent_thread,
        call,
    )
    .unwrap();
    event.provider_metadata.insert(
        DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
        named.to_owned(),
    );
    event
}

/// FU-291 C1: a member admits a nested pipeline thread only when the event's
/// receipt re-derives it from an Agent thread of this member and its lineage,
/// and that thread is registered in the set the member's checkpointer admits
/// into. The root (no registry) is unchanged.
#[test]
fn a_nested_pipeline_thread_is_proved_by_its_receipt_and_the_member_registry() {
    use crate::agents::graph::tool_pipeline_threads::tests::{admitted, lineage};
    use crate::agents::graph::tool_pipeline_threads::{ToolPipelineThreads, tool_pipeline_thread};
    let agent = "hashed-branch/owned";
    let call = lineage("call-1", "summarize");
    let thread = tool_pipeline_thread(agent, &call).unwrap();
    let registry = Arc::new(ToolPipelineThreads::new(threads().len()));
    let mut scope = binding(Some(parent()));
    scope.tool_threads = Some(Arc::clone(&registry));
    let (member, _) = sender(scope);
    let project = |event: &mut Event| {
        member.project_parallel_application_event(event, None, "root-invocation", None)
    };

    // A correct derivation whose writer is not active (nothing registered).
    assert!(project(&mut nested_leaf(agent, &call, &thread)).is_err());

    registry.register(admitted(agent, &call)).unwrap();
    let mut proved = nested_leaf(agent, &call, &thread);
    let original = json!(proved);
    project(&mut proved).unwrap();
    assert_eq!(json!(proved), original, "the nearest edge is kept as is");

    // The string alone, with no receipt.
    let mut bare = nested_leaf(agent, &call, &thread);
    bare.provider_metadata
        .remove(crate::agents::graph::tool_pipeline_threads::DURABLE_TOOL_THREAD_METADATA_KEY);
    assert!(project(&mut bare).is_err());

    // Another member's receipt: its own derivation names another thread, and
    // naming this member's registered thread under it fails the derivation.
    let other_agent = "other-branch/owned";
    let other = tool_pipeline_thread(other_agent, &call).unwrap();
    assert!(project(&mut nested_leaf(other_agent, &call, &other)).is_err());
    assert!(project(&mut nested_leaf(other_agent, &call, &thread)).is_err());

    // A registered thread under a receipt with a changed lineage.
    let changed = lineage("call-1", "another task");
    assert!(project(&mut nested_leaf(agent, &changed, &thread)).is_err());

    // The member root is never an Agent thread.
    let rooted = tool_pipeline_thread("hashed-branch", &call).unwrap();
    registry.register(admitted("hashed-branch", &call)).unwrap();
    assert!(project(&mut nested_leaf("hashed-branch", &call, &rooted)).is_err());

    // Without a registry (no admission lent) nothing nested is admitted.
    let (unlent, _) = sender(binding(Some(parent())));
    assert!(
        unlent
            .project_parallel_application_event(
                &mut nested_leaf(agent, &call, &thread),
                None,
                "root-invocation",
                None
            )
            .is_err()
    );
}
