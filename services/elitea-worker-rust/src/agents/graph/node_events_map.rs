//! Reuse exact descendant event projection for one sealed Map item.

use super::{
    ParallelEventScope, PipelineNodeEventScope, PipelineNodeEventSender,
    pipeline_node_event_channel_error,
};
use crate::agents::graph::map_reduce::MapItemExecution;

impl PipelineNodeEventSender {
    pub(crate) fn for_map_item(&self, execution: &MapItemExecution) -> adk_rust::Result<Self> {
        if self.parallel_scope.is_some() {
            return Err(pipeline_node_event_channel_error());
        }
        let parent = PipelineNodeEventScope::from_state(execution.parent_event_scope_value())?;
        let scope = ParallelEventScope::from_catalog(
            execution.parent_thread_id(),
            execution.thread_id(),
            execution.worker(),
            execution.admitted_threads().clone(),
            parent,
        )?;
        Ok(Self {
            inner: self.inner.clone(),
            parallel_scope: Some(std::sync::Arc::new(scope)),
        })
    }
}
