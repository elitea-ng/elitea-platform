//! W3-2: a Code node run directly as a Map item or Parallel branch.
//!
//! - [`FanoutCodeChildScopeProvider`] names the child's owner scope on its
//!   Code visit; Main resolves it against the run's frozen root source in the
//!   visit's own transaction (C1). It grants nothing by itself.
//! - [`code_child_visit`] is the child's journal visit: a fan-out child thread
//!   is not the run root, so its node journal is visit-scoped (FU-248), and an
//!   attempt that needs an operator ends typed, never repeated (no operator
//!   pause inside a member).
//! - [`CodeBranchNode`] names the registered Code failure in the member's
//!   failure (N1).

use std::sync::Arc;

use adk_rust::graph::{GraphError, Node, NodeContext, NodeOutput};
use async_trait::async_trait;
use ring::digest;

use crate::agents::graph::http_action::HttpActionError;
use crate::agents::pipeline::saved_child_http::{
    CodeVisitScope, FANOUT_CODE_CHILD_SCOPE_SCHEMA, FanoutCodeChildScope,
};
use crate::agents::pipeline::saved_child_scope_provider::{
    SavedChildPurpose, SavedChildScopeProvider,
};

use super::code_runtime::CodeFailureSlot;
use super::parallel::ParallelBranchExecution;

/// The owner scope of one Code child, bound to its own thread and node.
pub(super) struct FanoutCodeChildScopeProvider {
    scope: FanoutCodeChildScope,
}

impl FanoutCodeChildScopeProvider {
    /// The scope of `execution`'s child running Code node `owned_node_id`.
    pub(super) fn for_branch(
        execution: &ParallelBranchExecution,
        owned_node_id: &str,
    ) -> Result<Self, GraphError> {
        let invalid = || GraphError::InvalidGraph("the Code child scope is unproved".to_owned());
        let ordinal = u32::try_from(execution.branch_ordinal()).map_err(|_| invalid())?;
        let item_count = u32::try_from(execution.family_size()).map_err(|_| invalid())?;
        if item_count == 0 || ordinal >= item_count || execution.owned_node() != owned_node_id {
            return Err(invalid());
        }
        Ok(Self {
            scope: FanoutCodeChildScope {
                schema: FANOUT_CODE_CHILD_SCOPE_SCHEMA,
                kind: if execution.is_map_item() {
                    "map_item"
                } else {
                    "parallel_branch"
                },
                node_id: execution.activation_node_id().to_owned(),
                owned_node_id: owned_node_id.to_owned(),
                ordinal,
                item_count,
                parent_thread_id: execution.activation_root_thread_id().to_owned(),
                child_thread_id: execution.branch_thread_id().to_owned(),
            },
        })
    }
}

#[cfg(test)]
impl FanoutCodeChildScopeProvider {
    pub(super) const fn from_scope_for_tests(scope: FanoutCodeChildScope) -> Self {
        Self { scope }
    }
}

#[async_trait]
impl SavedChildScopeProvider for FanoutCodeChildScopeProvider {
    async fn for_visit(
        &self,
        thread_id: &str,
        node_id: &str,
        purpose: SavedChildPurpose,
    ) -> Result<CodeVisitScope, HttpActionError> {
        // Only this child's own Code visit; anything else is never widened.
        if thread_id != self.scope.child_thread_id
            || node_id != self.scope.owned_node_id
            || purpose != SavedChildPurpose::CodeRecovery
        {
            return Err(HttpActionError::PolicyDenied);
        }
        Ok(CodeVisitScope::FanoutChild(self.scope.clone()))
    }
}

/// The journal visit of a Code child: its branch key, its node and its frozen
/// input, so every item of a Map and every branch of a Parallel has its own.
pub(super) fn code_child_visit(
    branch_id: &str,
    node_id: &str,
    input: &adk_rust::graph::State,
) -> Result<String, GraphError> {
    let bytes = serde_json::to_vec(input).map_err(|_| {
        GraphError::SerializationError("a Code child input is not serializable".to_owned())
    })?;
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(b"elitea.graph.fanout.code-child-visit.v1\0");
    for field in [branch_id.as_bytes(), node_id.as_bytes(), &bytes] {
        hash.update(&u64::try_from(field.len()).unwrap_or(u64::MAX).to_be_bytes());
        hash.update(field);
    }
    Ok(crate::sandbox::code_recovery::hex(hash.finish().as_ref()))
}

/// The child's one node. A failure whose Code node reported the per-attempt
/// deadline refusal is named by that code (N1), so the member failure under
/// On Error says why; any other failure is unchanged.
pub(super) struct CodeBranchNode {
    pub(super) inner: Box<dyn Node>,
    pub(super) failure: CodeFailureSlot,
}

#[async_trait]
impl Node for CodeBranchNode {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn capabilities(&self) -> adk_rust::AgentCapabilities {
        self.inner.capabilities()
    }

    fn validate(&self) -> Result<(), GraphError> {
        self.inner.validate()
    }

    fn validate_against(&self, parent: &adk_rust::graph::StateSchema) -> Result<(), GraphError> {
        self.inner.validate_against(parent)
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let result = self.inner.execute(context).await;
        if result.is_err()
            && self.failure.lock().ok().and_then(|slot| *slot)
                == Some(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE)
        {
            return Err(GraphError::NodeExecutionFailed {
                node: self.inner.name().to_owned(),
                message: format!(
                    "{}: {}",
                    crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE,
                    crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_MESSAGE
                ),
            });
        }
        result
    }
}

/// A shared empty failure slot for one Code child.
pub(super) fn failure_slot() -> CodeFailureSlot {
    Arc::new(std::sync::Mutex::new(None))
}
