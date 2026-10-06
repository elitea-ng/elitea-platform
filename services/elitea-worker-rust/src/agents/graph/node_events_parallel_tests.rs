use super::*;
use crate::agents::graph::node_events::{pipeline_node_event_channel, pipeline_node_signal_event};
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
    let mut restored = projected;
    for key in [
        DESCENDANT_CONTAINER_INVOCATION_KEY,
        DESCENDANT_PARENT_CALL_KEY,
        DESCENDANT_CHECKPOINT_THREAD_KEY,
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
        .project_parallel_application_event(&mut event, None, "root-invocation")
        .unwrap();
    assert_eq!(json!(event), original);
    event.provider_metadata.insert(
        DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
        "hashed-branch/owned_suffix".to_owned(),
    );
    assert!(
        sender
            .project_parallel_application_event(&mut event, None, "root-invocation")
            .is_err()
    );
    event.provider_metadata.remove(DESCENDANT_PARENT_CALL_KEY);
    assert!(
        sender
            .project_parallel_application_event(&mut event, None, "root-invocation")
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
