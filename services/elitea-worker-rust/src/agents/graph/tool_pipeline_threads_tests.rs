use std::sync::Arc;

use adk_rust::Event;
use adk_rust::graph::{Checkpointer, GraphError, MemoryCheckpointer};
use async_trait::async_trait;
use serde_json::json;

use super::*;
use crate::agents::graph::node_recovery::NodeRecoveryPolicy;
use crate::agents::graph::node_recovery_runtime::{NodeAttemptActivation, NodeAttemptJournal};

pub(crate) fn lineage(call: &str, task: &str) -> PipelineToolCallLineage {
    PipelineToolCallLineage::for_test("batch-1", 1, call, "summarize", &json!({"task": task}))
}

/// A writer stand-in: the registry stores, never dereferences, it here.
pub(crate) struct StubWriter(MemoryCheckpointer);

#[async_trait]
impl Checkpointer for StubWriter {
    async fn save(&self, checkpoint: &adk_rust::graph::Checkpoint) -> Result<String, GraphError> {
        self.0.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<adk_rust::graph::Checkpoint>, GraphError> {
        self.0.load(thread).await
    }
    async fn load_by_id(
        &self,
        id: &str,
    ) -> Result<Option<adk_rust::graph::Checkpoint>, GraphError> {
        self.0.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<adk_rust::graph::Checkpoint>, GraphError> {
        self.0.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.0.delete(thread).await
    }
}

#[async_trait]
impl NodeRecoveryFactory for StubWriter {
    async fn open(
        &self,
        _: &NodeAttemptActivation,
        _: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, GraphError> {
        Err(GraphError::CheckpointError("stub".to_owned()))
    }
}

pub(crate) fn admitted(
    agent_thread: &str,
    lineage: &PipelineToolCallLineage,
) -> AdmittedToolPipeline {
    AdmittedToolPipeline {
        thread_id: tool_pipeline_thread(agent_thread, lineage).unwrap(),
        agent_thread: agent_thread.to_owned(),
        writer: Arc::new(StubWriter(MemoryCheckpointer::new())),
    }
}

#[test]
fn the_nested_thread_is_pure_and_distinct_per_member_call_and_arguments() {
    let call = lineage("call-1", "summarize topic A");
    let first = tool_pipeline_thread("m1:member-a/agent", &call).unwrap();
    assert_eq!(
        first,
        tool_pipeline_thread("m1:member-a/agent", &call).unwrap()
    );
    assert!(first.starts_with("m1:member-a/agent/pt1:"));
    for other in [
        tool_pipeline_thread("m1:member-b/agent", &call).unwrap(),
        tool_pipeline_thread("m1:member-a/agent", &lineage("call-2", "summarize topic A")).unwrap(),
        tool_pipeline_thread("m1:member-a/agent", &lineage("call-1", "summarize topic B")).unwrap(),
        tool_pipeline_thread(
            "m1:member-a/agent",
            &PipelineToolCallLineage::for_test(
                "batch-2",
                1,
                "call-1",
                "summarize",
                &json!({"task": "summarize topic A"}),
            ),
        )
        .unwrap(),
    ] {
        assert_ne!(first, other);
    }
}

/// C7: the nested thread is at most 480 bytes: 480 passes, 481 is refused.
#[test]
fn the_nested_thread_bound_admits_480_bytes_and_refuses_481() {
    let call = lineage("call-1", "task");
    // `A + "/pt1:" + 43` bytes.
    let at_limit = format!("m1:{}", "a".repeat(480 - 48 - 3));
    assert_eq!(
        tool_pipeline_thread(&at_limit, &call).unwrap().len(),
        MAX_TOOL_PIPELINE_THREAD_BYTES
    );
    let over = format!("{at_limit}a");
    assert!(tool_pipeline_thread(&over, &call).is_err());
}

/// An Agent thread can never be a nested thread, so nothing nests below one.
#[test]
fn a_nested_thread_is_never_an_agent_thread() {
    let call = lineage("call-1", "task");
    let nested = tool_pipeline_thread("m1:member/agent", &call).unwrap();
    assert!(tool_pipeline_thread(&nested, &call).is_err());
    assert!(tool_pipeline_thread("", &call).is_err());
    assert!(tool_pipeline_thread("m1:member/\nagent", &call).is_err());
}

/// D3 (a): 8 nested calls per member are admitted; the 9th is refused typed.
#[test]
fn a_member_admits_8_nested_threads_and_refuses_the_9th() {
    let threads = ToolPipelineThreads::new(2);
    for call in 0..MAX_TOOL_PIPELINE_THREADS_PER_MEMBER {
        threads
            .register(admitted(
                "m1:member/agent",
                &lineage(&format!("call-{call}"), "t"),
            ))
            .unwrap();
    }
    assert_eq!(threads.len(), 8);
    let refused = threads
        .register(admitted("m1:member/agent", &lineage("call-9", "t")))
        .unwrap_err();
    assert!(is_tool_pipeline_limit(&refused));
    assert!(threads.check_room().is_err());
    assert_eq!(threads.len(), 8, "a refused thread is never registered");
}

/// The 129 child threads of a member hold its nested threads too: 129 total
/// are admitted, the 130th is refused typed.
#[test]
fn the_member_total_admits_129_threads_and_refuses_the_130th() {
    let threads = ToolPipelineThreads::new(125);
    for call in 0..4 {
        threads
            .register(admitted(
                "m1:member/agent",
                &lineage(&format!("call-{call}"), "t"),
            ))
            .unwrap();
    }
    assert_eq!(
        125 + threads.len(),
        super::super::fanout_budget::MAX_CHILD_THREADS
    );
    assert!(is_tool_pipeline_limit(
        &threads
            .register(admitted("m1:member/agent", &lineage("call-4", "t")))
            .unwrap_err()
    ));
}

/// Review 5: each bound refuses with its own code: the 9th call of a member
/// with room in its total is the per-member bound; the 130th thread of a
/// member with calls left is the shared total.
#[test]
fn each_bound_refuses_with_its_own_code() {
    let per_member = ToolPipelineThreads::new(2);
    for call in 0..MAX_TOOL_PIPELINE_THREADS_PER_MEMBER {
        per_member
            .register(admitted(
                "m1:member/agent",
                &lineage(&format!("call-{call}"), "t"),
            ))
            .unwrap();
    }
    let GraphError::CheckpointError(message) = per_member
        .register(admitted("m1:member/agent", &lineage("call-9", "t")))
        .unwrap_err()
    else {
        panic!("a checkpoint error");
    };
    assert!(message.starts_with(TOOL_PIPELINE_LIMIT_CODE), "{message}");
    let total = ToolPipelineThreads::new(125);
    for call in 0..4 {
        total
            .register(admitted(
                "m1:member/agent",
                &lineage(&format!("call-{call}"), "t"),
            ))
            .unwrap();
    }
    let GraphError::CheckpointError(message) = total
        .register(admitted("m1:member/agent", &lineage("call-4", "t")))
        .unwrap_err()
    else {
        panic!("a checkpoint error");
    };
    assert!(
        message.starts_with(TOOL_PIPELINE_TOTAL_LIMIT_CODE),
        "{message}"
    );
}

/// The set is append-only: a registered thread is never replaced, and it is
/// found only for the Agent it was admitted for.
#[test]
fn a_registered_thread_is_never_replaced_and_names_only_its_agent() {
    let threads = ToolPipelineThreads::new(2);
    let call = lineage("call-1", "t");
    let entry = admitted("m1:member/agent", &call);
    threads.register(entry.clone()).unwrap();
    assert!(threads.register(entry.clone()).is_err());
    assert!(
        threads
            .admitted(&entry.thread_id, "m1:member/agent")
            .is_some()
    );
    assert!(
        threads
            .admitted(&entry.thread_id, "m1:other/agent")
            .is_none()
    );
}

fn stamped(agent_thread: &str, call: &PipelineToolCallLineage, named: Option<&str>) -> Event {
    let mut event = Event::new("pipeline-child:call-1");
    stamp_durable_tool_thread(&mut event, agent_thread, call).unwrap();
    let thread = named.map_or_else(
        || tool_pipeline_thread(agent_thread, call).unwrap(),
        str::to_owned,
    );
    event
        .provider_metadata
        .insert(DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(), thread);
    event
}

#[test]
fn a_receipt_re_derives_its_thread_and_refuses_another_name() {
    let call = lineage("call-1", "t");
    let event = stamped("m1:member/agent", &call, None);
    let (thread, agent) = proved_durable_tool_thread(&event).unwrap().unwrap();
    assert_eq!(
        thread,
        tool_pipeline_thread("m1:member/agent", &call).unwrap()
    );
    assert_eq!(agent, "m1:member/agent");
    let forged = stamped("m1:member/agent", &call, Some("m1:member/agent/pt1:forged"));
    assert!(proved_durable_tool_thread(&forged).is_err());
    assert!(
        proved_durable_tool_thread(&Event::new("x"))
            .unwrap()
            .is_none()
    );
    let mut malformed = Event::new("x");
    malformed
        .provider_metadata
        .insert(DURABLE_TOOL_THREAD_METADATA_KEY.to_owned(), "{}".to_owned());
    assert!(proved_durable_tool_thread(&malformed).is_err());
}
