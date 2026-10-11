//! Invocation-owned event lineage for one factory-minted fixed Parallel branch.

use std::collections::BTreeSet;

use super::{
    DESCENDANT_CHECKPOINT_THREAD_KEY, DESCENDANT_CONTAINER_INVOCATION_KEY,
    DESCENDANT_PARENT_CALL_KEY, Event, PipelineNodeEventScope, PipelineNodeEventSender,
    pipeline_node_event_channel_error, valid_event_identity, valid_graph_id,
};
use crate::agents::graph::ParallelBranchExecution;

const MAX_BRANCH_THREADS: usize = 129;
const MAX_FIXED_BRANCHES: usize = 16;
const _: () = assert!(
    MAX_BRANCH_THREADS == crate::agents::graph::fanout_budget::MAX_CHILD_THREADS
        && MAX_FIXED_BRANCHES == crate::agents::graph::fanout_budget::MAX_PARALLEL_BRANCHES
);

pub(super) struct ParallelEventScope {
    branch_root_thread: String,
    owned_thread: String,
    threads: BTreeSet<String>,
    parent: Option<PipelineNodeEventScope>,
}

impl PipelineNodeEventSender {
    pub(crate) fn for_parallel_branch(
        &self,
        execution: &ParallelBranchExecution,
        parent: Option<PipelineNodeEventScope>,
    ) -> adk_rust::Result<Self> {
        if self.parallel_scope.is_some() || execution.branch_ordinal() >= MAX_FIXED_BRANCHES {
            return Err(pipeline_node_event_channel_error());
        }
        let captured = PipelineNodeEventScope::from_state(execution.parent_event_scope_value())?;
        if captured
            .as_ref()
            .map(PipelineNodeEventScope::to_state_value)
            .transpose()?
            != parent
                .as_ref()
                .map(PipelineNodeEventScope::to_state_value)
                .transpose()?
        {
            return Err(pipeline_node_event_channel_error());
        }
        let scope = ParallelEventScope::from_catalog(
            execution.activation_root_thread_id(),
            execution.branch_thread_id(),
            execution.owned_node(),
            execution.admitted_threads().clone(),
            parent,
        )?;
        Ok(Self {
            inner: self.inner.clone(),
            parallel_scope: Some(std::sync::Arc::new(scope)),
        })
    }

    /// Return inherited identity only after proving the exact branch context.
    pub(crate) fn inherited_event_scope(
        &self,
        thread: &str,
        local: Option<&PipelineNodeEventScope>,
    ) -> adk_rust::Result<Option<&PipelineNodeEventScope>> {
        let Some(scope) = self.parallel_scope.as_ref() else {
            return Ok(None);
        };
        scope.validate_local(local)?;
        if !scope.threads.contains(thread)
            || (local.is_none() && thread != scope.branch_root_thread)
            || local.is_some_and(|local| local.checkpoint_thread_id() != thread)
        {
            return Err(pipeline_node_event_channel_error());
        }
        Ok(scope.parent.as_ref())
    }

    pub(crate) fn has_parallel_event_scope(&self) -> bool {
        self.parallel_scope.is_some()
    }

    pub(crate) fn project_parallel_application_event(
        &self,
        event: &mut Event,
        local: Option<&PipelineNodeEventScope>,
        root_container: &str,
    ) -> adk_rust::Result<()> {
        let scope = self
            .parallel_scope
            .as_ref()
            .ok_or_else(pipeline_node_event_channel_error)?;
        scope.validate_local(local)?;
        let current = local.or(scope.parent.as_ref());
        // Already-routed ordinary leaves retain their nearest proved receipt edge.
        let keys = [
            DESCENDANT_CONTAINER_INVOCATION_KEY,
            DESCENDANT_PARENT_CALL_KEY,
            DESCENDANT_CHECKPOINT_THREAD_KEY,
        ];
        let existing = keys
            .iter()
            .filter(|key| event.provider_metadata.contains_key(**key))
            .count();
        if existing != 0 {
            if existing != keys.len()
                || keys
                    .iter()
                    .any(|key| !valid_event_identity(&event.provider_metadata[*key]))
                || !scope
                    .contains_thread(&event.provider_metadata[DESCENDANT_CHECKPOINT_THREAD_KEY])
            {
                return Err(pipeline_node_event_channel_error());
            }
            return Ok(());
        }
        if let Some(current) = current {
            event.provider_metadata.insert(
                DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
                scope.container(local, root_container),
            );
            event.provider_metadata.insert(
                DESCENDANT_PARENT_CALL_KEY.to_owned(),
                current.parent_call_id().to_owned(),
            );
            event.provider_metadata.insert(
                DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
                current.checkpoint_thread_id().to_owned(),
            );
        } else {
            let original_root = event.invocation_id.clone();
            crate::agents::pipeline::scoped_applications::stamp_root_scope_projection(
                event,
                &original_root,
                root_container,
            )
            .map_err(|_| pipeline_node_event_channel_error())?;
        }
        Ok(())
    }
}

impl ParallelEventScope {
    pub(super) fn from_catalog(
        parent_thread: &str,
        root_thread: &str,
        owned_node: &str,
        threads: BTreeSet<String>,
        parent: Option<PipelineNodeEventScope>,
    ) -> adk_rust::Result<Self> {
        if !valid_event_identity(parent_thread)
            || !valid_event_identity(root_thread)
            || !valid_graph_id(owned_node)
            || parent_thread == root_thread
            || threads.is_empty()
            || threads.len() > MAX_BRANCH_THREADS
            || threads.iter().any(|thread| !valid_event_identity(thread))
            || threads.contains(parent_thread)
            || !threads.contains(root_thread)
            || !threads.contains(&format!("{root_thread}/{owned_node}"))
        {
            return Err(pipeline_node_event_channel_error());
        }
        if let Some(parent) = parent.as_ref() {
            parent.validate()?;
            if parent.checkpoint_thread_id() != parent_thread {
                return Err(pipeline_node_event_channel_error());
            }
            let mut cursor = Some(parent);
            while let Some(scope) = cursor {
                if threads.contains(scope.checkpoint_thread_id()) {
                    return Err(pipeline_node_event_channel_error());
                }
                cursor = scope.parent();
            }
        }
        Ok(Self {
            branch_root_thread: root_thread.to_owned(),
            owned_thread: format!("{root_thread}/{owned_node}"),
            threads,
            parent,
        })
    }

    fn contains_thread(&self, thread: &str) -> bool {
        if self.threads.contains(thread) {
            return true;
        }
        let mut cursor = self.parent.as_ref();
        while let Some(scope) = cursor {
            if scope.checkpoint_thread_id() == thread {
                return true;
            }
            cursor = scope.parent();
        }
        false
    }

    fn validate_local(&self, local: Option<&PipelineNodeEventScope>) -> adk_rust::Result<()> {
        let mut depth = 0;
        let mut cursor = self.parent.as_ref();
        while let Some(scope) = cursor {
            depth += 1;
            cursor = scope.parent();
        }
        if let Some(local) = local {
            local.validate()?;
            let mut cursor = Some(local);
            while let Some(scope) = cursor {
                depth += 1;
                if !self.threads.contains(scope.checkpoint_thread_id()) {
                    return Err(pipeline_node_event_channel_error());
                }
                cursor = scope.parent();
            }
            let mut root = local;
            while let Some(parent) = root.parent() {
                root = parent;
            }
            if root.checkpoint_thread_id() != self.owned_thread {
                return Err(pipeline_node_event_channel_error());
            }
        }
        if depth > 3 {
            return Err(pipeline_node_event_channel_error());
        }
        Ok(())
    }

    fn container(&self, local: Option<&PipelineNodeEventScope>, root_container: &str) -> String {
        let parent = match local {
            Some(local) => local.parent().or(self.parent.as_ref()),
            None => self
                .parent
                .as_ref()
                .and_then(PipelineNodeEventScope::parent),
        };
        parent.map_or_else(
            || root_container.to_owned(),
            |parent| format!("pipeline-child:{}", parent.parent_call_id()),
        )
    }

    pub(super) fn project_node_event(
        &self,
        event: &mut Event,
        local: Option<&PipelineNodeEventScope>,
        root_invocation: &str,
        root_author: &str,
        root_branch: &str,
    ) -> adk_rust::Result<()> {
        self.validate_local(local)?;
        let current = local.or(self.parent.as_ref());
        if let Some(current) = current {
            if [
                DESCENDANT_CONTAINER_INVOCATION_KEY,
                DESCENDANT_PARENT_CALL_KEY,
                DESCENDANT_CHECKPOINT_THREAD_KEY,
            ]
            .iter()
            .any(|key| event.provider_metadata.contains_key(*key))
            {
                return Err(pipeline_node_event_channel_error());
            }
            event.provider_metadata.insert(
                DESCENDANT_CONTAINER_INVOCATION_KEY.to_owned(),
                self.container(local, root_invocation),
            );
            event.provider_metadata.insert(
                DESCENDANT_PARENT_CALL_KEY.to_owned(),
                current.parent_call_id().to_owned(),
            );
            event.provider_metadata.insert(
                DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
                current.checkpoint_thread_id().to_owned(),
            );
            event.invocation_id = format!("pipeline-child:{}", current.parent_call_id());
            current.agent_name().clone_into(&mut event.author);
            event.branch.clear();
        } else {
            root_invocation.clone_into(&mut event.invocation_id);
            root_author.clone_into(&mut event.author);
            root_branch.clone_into(&mut event.branch);
        }
        Ok(())
    }
}

#[cfg(test)]
#[path = "node_events_parallel_tests.rs"]
mod tests;
