//! Item descendant scope tests use real ADK checkpoints and bounded event senders.
#![allow(clippy::unwrap_used)]

use super::*;
use crate::agents::graph::node_events::{PipelineNodeEventScope, pipeline_node_event_channel};
use adk_rust::graph::MemoryCheckpointer;
use serde_json::json;
use std::collections::BTreeSet;

fn execution(parent: &str, scope: Option<serde_json::Value>) -> MapItemExecution {
    let threads = BTreeSet::from([
        "item-root".to_owned(),
        "item-root/worker".to_owned(),
        "item-root/worker/inner".to_owned(),
    ]);
    MapItemExecution::new(
        Arc::new(MapReceiptCheckpointer::new(
            Arc::new(MemoryCheckpointer::new()),
            "item-root".to_owned(),
            threads.clone(),
        )),
        "worker".to_owned(),
        parent.to_owned(),
        threads,
        scope,
    )
}

#[test]
fn map_event_scope_requires_exact_original_parent_not_captured_label() {
    let (events, _) = pipeline_node_event_channel();
    let scope =
        PipelineNodeEventScope::new("original-call", "original-graph", "root/original").unwrap();
    let wrong = execution("root/different", Some(scope.to_state_value().unwrap()));
    assert!(events.for_map_item(&wrong).is_err());
    let owned = execution("root/original", Some(scope.to_state_value().unwrap()));
    let rebound = events.for_map_item(&owned).unwrap();
    assert_eq!(
        rebound
            .inherited_event_scope("item-root", None)
            .unwrap()
            .unwrap()
            .parent_call_id(),
        "original-call"
    );
    assert!(
        rebound
            .inherited_event_scope("item-root_suffix", None)
            .is_err()
    );
    assert!(rebound.for_map_item(&owned).is_err());
}

#[test]
fn map_event_scope_preserves_exact_descendant_call_lineage() {
    let (events, _) = pipeline_node_event_channel();
    let parent = PipelineNodeEventScope::new("parent-call", "parent-graph", "root").unwrap();
    let owned = execution("root", Some(parent.to_state_value().unwrap()));
    let events = events.for_map_item(&owned).unwrap();
    let local = PipelineNodeEventScope::new("child-call", "worker", "item-root/worker").unwrap();
    assert_eq!(
        events
            .inherited_event_scope("item-root/worker", Some(&local))
            .unwrap()
            .unwrap()
            .parent_call_id(),
        "parent-call"
    );
    let foreign =
        PipelineNodeEventScope::new("foreign-call", "worker", "item-root/worker_suffix").unwrap();
    assert!(
        events
            .inherited_event_scope("item-root/worker_suffix", Some(&foreign))
            .is_err()
    );
    let skipped =
        PipelineNodeEventScope::new("skipped", "inner", "item-root/worker/inner").unwrap();
    assert!(
        events
            .inherited_event_scope("item-root/worker/inner", Some(&skipped))
            .is_err()
    );
}

#[tokio::test]
async fn map_item_completion_receipt_belongs_only_to_root_checkpoint() {
    let execution = execution("root", None);
    execution
        .checkpoint
        .capture(MapItemReceipt::Completed)
        .unwrap();
    let mut descendant = Checkpoint::new("item-root/worker", State::new(), 1, vec![]);
    descendant
        .state
        .insert("result".to_owned(), json!({"opaque":[null, true]}));
    execution.checkpointer().save(&descendant).await.unwrap();
    let saved = execution
        .checkpointer()
        .load("item-root/worker")
        .await
        .unwrap()
        .unwrap();
    assert!(MapReceiptCheckpointer::receipt(&saved).unwrap().is_none());
    let root = Checkpoint::new("item-root", saved.state.clone(), 1, vec![]);
    execution.checkpointer().save(&root).await.unwrap();
    assert!(matches!(
        MapReceiptCheckpointer::receipt(&execution.checkpoint.load_item().await.unwrap().unwrap())
            .unwrap(),
        Some(MapItemReceipt::Completed)
    ));
    let foreign = Checkpoint::new("item-root/worker_suffix", State::new(), 1, vec![]);
    assert!(execution.checkpointer().save(&foreign).await.is_err());
    assert!(execution.checkpointer().load("root").await.is_err());
    assert!(
        execution
            .checkpointer()
            .load("item-root/worker_suffix")
            .await
            .is_err()
    );
}
