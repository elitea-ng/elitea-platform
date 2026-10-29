//! Invocation-owned event lineage for one factory-minted fixed Parallel branch.

use std::collections::BTreeSet;

use super::{
    DESCENDANT_CHECKPOINT_THREAD_KEY, DESCENDANT_CONTAINER_INVOCATION_KEY,
    DESCENDANT_PARENT_CALL_KEY, Event, PipelineNodeEventScope, PipelineNodeEventSender,
    pipeline_node_event_channel_error, valid_event_identity, valid_graph_id,
};
use crate::agents::graph::ParallelBranchExecution;
use crate::agents::pipeline::scoped_applications::PipelineApplicationActivation;

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
    /// A Parallel member's tier (`{"call": fo1_…, "ordinal": n}`), stamped on
    /// its own call so the projector keys the call and every descendant by the
    /// member tier the fan-out frames and cards use (contract §3.3). `None`
    /// for a Map item.
    member_tier: Option<String>,
    /// PM-5: a root-level inline LLM branch, whose own events carry its
    /// member tier (only [`PipelineNodeEventSender::for_inline_llm_branch`]).
    inline_llm: bool,
}

impl PipelineNodeEventSender {
    /// [`Self::for_parallel_branch`] for a root-level inline LLM branch: its own
    /// model and tool events stream under its member tier (PM-5).
    pub(crate) fn for_inline_llm_branch(
        &self,
        execution: &ParallelBranchExecution,
    ) -> adk_rust::Result<Self> {
        let mut branch = self.for_parallel_branch(execution, None)?;
        let scope = branch
            .parallel_scope
            .take()
            .ok_or_else(pipeline_node_event_channel_error)?;
        let mut scope =
            std::sync::Arc::try_unwrap(scope).map_err(|_| pipeline_node_event_channel_error())?;
        scope.inline_llm = true;
        branch.parallel_scope = Some(std::sync::Arc::new(scope));
        Ok(branch)
    }

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
        let mut scope = ParallelEventScope::from_catalog(
            execution.activation_root_thread_id(),
            execution.branch_thread_id(),
            execution.owned_node(),
            execution.admitted_threads().clone(),
            parent,
        )?;
        scope.member_tier = Some(
            serde_json::json!({
                "call": crate::agents::graph::fanout_frames::member_call_id(
                    execution.branch_thread_id()
                ),
                "ordinal": execution.branch_ordinal() + 1,
            })
            .to_string(),
        );
        Ok(Self {
            inner: self.inner.clone(),
            parallel_scope: Some(std::sync::Arc::new(scope)),
            fanout_member: true,
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

    /// `original_root` is the invocation the scoped call's receipt names: a
    /// root-level member's already-routed leaf is projected onto the current
    /// root invocation from it, as a scoped Agent node's leaves are.
    pub(crate) fn project_parallel_application_event(
        &self,
        event: &mut Event,
        local: Option<&PipelineNodeEventScope>,
        root_container: &str,
        original_root: Option<&str>,
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
            let edge = |key: &str| {
                event
                    .provider_metadata
                    .get(key)
                    .is_some_and(|value| valid_event_identity(value))
            };
            let proved = if existing == keys.len() {
                keys.iter().all(|key| edge(key))
                    && scope
                        .contains_thread(&event.provider_metadata[DESCENDANT_CHECKPOINT_THREAD_KEY])
            } else {
                // An Agent child's leaf carries no checkpoint thread (the
                // projector refuses one on a non-pipeline child); the
                // activation receipt stamped on it proves its branch thread.
                !event
                    .provider_metadata
                    .contains_key(DESCENDANT_CHECKPOINT_THREAD_KEY)
                    && edge(DESCENDANT_CONTAINER_INVOCATION_KEY)
                    && edge(DESCENDANT_PARENT_CALL_KEY)
                    && PipelineApplicationActivation::from_event(event)
                        .ok()
                        .flatten()
                        .is_some_and(|activation| scope.contains_thread(activation.thread_id()))
            };
            if !proved {
                return Err(pipeline_node_event_channel_error());
            }
            if current.is_none()
                && let Some(original) = original_root
            {
                crate::agents::pipeline::scoped_applications::stamp_root_scope_projection(
                    event,
                    original,
                    root_container,
                )
                .map_err(|_| pipeline_node_event_channel_error())?;
            }
            return Ok(());
        }
        // The member's own call start or result (the scoped runtime sends
        // nothing else without an edge): it stays open beside its siblings.
        if !event.tool_calls().is_empty() || !event.tool_results().is_empty() {
            event.provider_metadata.insert(
                crate::agents::events::FANOUT_MEMBER_CALL_KEY.to_owned(),
                scope.member_tier.clone().unwrap_or_else(|| "1".to_owned()),
            );
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
            member_tier: None,
            inline_llm: false,
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
            // The member's own child pipeline: its thread is relative to the
            // proved branch thread, not to the root graph's naming.
            if local.is_some() && current.checkpoint_thread_id() == self.owned_thread {
                event.provider_metadata.insert(
                    crate::agents::events::FANOUT_MEMBER_THREAD_KEY.to_owned(),
                    serde_json::json!({
                        "call": current.parent_call_id(),
                        "thread": self.branch_root_thread,
                    })
                    .to_string(),
                );
            }
            event.invocation_id = format!("pipeline-child:{}", current.parent_call_id());
            current.agent_name().clone_into(&mut event.author);
            event.branch.clear();
        } else {
            root_invocation.clone_into(&mut event.invocation_id);
            root_author.clone_into(&mut event.author);
            root_branch.clone_into(&mut event.branch);
            // PM-5: an inline LLM branch's own events stream under its member
            // tier, named by its node, never as the run's own text.
            if let Some(tier) = self.member_tier.as_deref().filter(|_| self.inline_llm) {
                let mut tier: serde_json::Value =
                    serde_json::from_str(tier).map_err(|_| pipeline_node_event_channel_error())?;
                let node = self
                    .owned_thread
                    .strip_prefix(&format!("{}/", self.branch_root_thread))
                    .filter(|node| valid_graph_id(node))
                    .ok_or_else(pipeline_node_event_channel_error)?;
                tier["name"] = serde_json::Value::String(node.to_owned());
                if event
                    .provider_metadata
                    .insert(
                        crate::agents::events::FANOUT_INLINE_MEMBER_KEY.to_owned(),
                        tier.to_string(),
                    )
                    .is_some()
                {
                    return Err(pipeline_node_event_channel_error());
                }
            }
        }
        Ok(())
    }
}

#[cfg(test)]
#[path = "node_events_parallel_tests.rs"]
mod tests;
