//! W3-2b: the owner scope of a Code node inside a saved pipeline that an
//! Agent node of the run root runs, directly or as the Map worker or Parallel
//! branch of a root fan-out node.
//!
//! Main resolves every field in the visit's own transaction against its freeze
//! capture and the run root's frozen source (D-b1); the scope grants nothing
//! by itself. The provider serves only the Code nodes of its own saved
//! pipeline on the Agent node's own subgraph thread, so a visit can never
//! borrow another use's scope.

use std::collections::BTreeSet;
use std::sync::Arc;

use async_trait::async_trait;
use serde::Serialize;

use super::saved_child_http::CodeVisitScope;
use super::saved_child_scope_provider::{SavedChildPurpose, SavedChildScopeProvider};
use crate::agents::graph::http_action::HttpActionError;

/// The wire schema of [`SavedCodeChildScope`].
pub(crate) const SAVED_CODE_CHILD_SCOPE_SCHEMA: &str = "elitea.runtime.saved-code-child-scope.v1";

/// The saved pipeline Main froze (and captured) for one root-level Agent
/// node: its application, version and Main's frozen definition digest, and
/// the Code nodes it declares.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct SavedCodeOwner {
    application_id: u64,
    version_id: u64,
    definition_sha256: String,
    code_nodes: BTreeSet<String>,
}

impl SavedCodeOwner {
    pub(crate) fn new(
        application_id: u64,
        version_id: u64,
        definition_sha256: &[u8; 32],
        code_nodes: BTreeSet<String>,
    ) -> Self {
        Self {
            application_id,
            version_id,
            definition_sha256: crate::sandbox::code_recovery::hex(definition_sha256),
            code_nodes,
        }
    }
}

/// The root fan-out member that runs the Agent node (the W3-2a family).
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub(crate) struct SavedCodeFanoutMember {
    /// `map_item` or `parallel_branch`.
    kind: &'static str,
    node_id: String,
    ordinal: u32,
    item_count: u32,
    /// The member's own thread: the Agent node's parent thread.
    #[serde(skip)]
    parent_thread_id: String,
}

impl SavedCodeFanoutMember {
    /// The member of `execution`; `None` when its family is unproved.
    pub(crate) fn of(execution: &crate::agents::graph::ParallelBranchExecution) -> Option<Self> {
        let ordinal = u32::try_from(execution.branch_ordinal()).ok()?;
        let item_count = u32::try_from(execution.family_size()).ok()?;
        (item_count > 0 && ordinal < item_count).then(|| Self {
            kind: if execution.is_map_item() {
                "map_item"
            } else {
                "parallel_branch"
            },
            node_id: execution.activation_node_id().to_owned(),
            ordinal,
            item_count,
            parent_thread_id: execution.branch_thread_id().to_owned(),
        })
    }
}

#[cfg(test)]
impl SavedCodeFanoutMember {
    pub(crate) fn for_tests(
        kind: &'static str,
        node_id: &str,
        ordinal: u32,
        item_count: u32,
        parent_thread_id: &str,
    ) -> Self {
        Self {
            kind,
            node_id: node_id.to_owned(),
            ordinal,
            item_count,
            parent_thread_id: parent_thread_id.to_owned(),
        }
    }
}

/// What a saved pipeline's Code visit names as its owner.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub(crate) struct SavedCodeChildScope {
    schema: &'static str,
    application_id: u64,
    version_id: u64,
    definition_sha256: String,
    agent_node_id: String,
    fanout: Option<SavedCodeFanoutMember>,
    parent_thread_id: String,
    child_thread_id: String,
    code_node_id: String,
}

/// The scope of one use of a saved pipeline: the root-level Agent node that
/// runs it and, under a fan-out, the member running that Agent node.
pub(crate) struct SavedCodeChildScopeProvider {
    owner: Arc<SavedCodeOwner>,
    agent_node_id: String,
    member: Option<Arc<SavedCodeFanoutMember>>,
}

impl SavedCodeChildScopeProvider {
    pub(crate) fn new(
        owner: Arc<SavedCodeOwner>,
        agent_node_id: &str,
        member: Option<Arc<SavedCodeFanoutMember>>,
    ) -> Self {
        Self {
            owner,
            agent_node_id: agent_node_id.to_owned(),
            member,
        }
    }
}

#[async_trait]
impl SavedChildScopeProvider for SavedCodeChildScopeProvider {
    async fn for_visit(
        &self,
        thread_id: &str,
        node_id: &str,
        purpose: SavedChildPurpose,
    ) -> Result<CodeVisitScope, HttpActionError> {
        // Only a Code node of this saved pipeline, on this Agent node's own
        // subgraph thread (its parent, "/", the Agent node), is ever served.
        let parent = thread_id
            .strip_suffix(self.agent_node_id.as_str())
            .and_then(|prefix| prefix.strip_suffix('/'))
            .filter(|parent| !parent.is_empty())
            .ok_or(HttpActionError::PolicyDenied)?;
        if purpose != SavedChildPurpose::CodeRecovery
            || !self.owner.code_nodes.contains(node_id)
            || self
                .member
                .as_ref()
                .is_some_and(|member| member.parent_thread_id != parent)
        {
            return Err(HttpActionError::PolicyDenied);
        }
        Ok(CodeVisitScope::SavedChild(SavedCodeChildScope {
            schema: SAVED_CODE_CHILD_SCOPE_SCHEMA,
            application_id: self.owner.application_id,
            version_id: self.owner.version_id,
            definition_sha256: self.owner.definition_sha256.clone(),
            agent_node_id: self.agent_node_id.clone(),
            fanout: self.member.as_deref().cloned(),
            parent_thread_id: parent.to_owned(),
            child_thread_id: thread_id.to_owned(),
            code_node_id: node_id.to_owned(),
        }))
    }
}

#[cfg(test)]
#[path = "saved_code_child_tests.rs"]
mod tests;
