//! One bounded assembly path across saved graphs and ordinary Agents.

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use super::{
    MAX_AGENT_TIERS, MAX_APPLICATION_HOPS, NativeAgentAssemblyError, NativeAgentAssemblyErrorCode,
    invalid_configuration, resource_exhausted,
};
use crate::agents::pipeline::composition::MAX_PIPELINE_COMPOSITION_DEPTH;

pub(in crate::agents) const MAX_MATERIALIZATION_VERSION_BYTES: usize = 4 * 1024 * 1024;

/// How the saved applications of one graph conversation continue.
///
/// Every conversation this worker starts continues `Scoped`: each graph Agent call owns an
/// original-call receipt, nested ordinary Agents and pipeline attachments are materialized, and
/// static pauses continue through the typed coordinator. `LegacyUnscoped` is data compatibility,
/// not a switch: it is chosen only for a conversation that an older worker left paused inside an
/// application call, whose checkpoint holds no graph-call receipt (see
/// `pipeline::continuation_for_checkpoint`).
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(in crate::agents) enum ApplicationContinuation {
    #[default]
    Scoped,
    LegacyUnscoped,
}

/// Immutable branch ancestry. Siblings share only bounded assembly counters.
#[derive(Clone)]
pub(in crate::agents) struct ApplicationMaterializationPath {
    ancestors: Vec<(u64, u64)>,
    agent_tier: usize,
    pipeline_depth: usize,
    hops: Arc<AtomicUsize>,
    version_bytes: Arc<AtomicUsize>,
    continuation: ApplicationContinuation,
    graph_agent_scopes: usize,
    native_graph_root: bool,
}

impl Default for ApplicationMaterializationPath {
    fn default() -> Self {
        Self::new(ApplicationContinuation::Scoped)
    }
}

impl ApplicationMaterializationPath {
    pub(in crate::agents) fn new(continuation: ApplicationContinuation) -> Self {
        Self {
            ancestors: Vec::new(),
            agent_tier: 1,
            pipeline_depth: 0,
            hops: Arc::default(),
            version_bytes: Arc::default(),
            continuation,
            graph_agent_scopes: 0,
            native_graph_root: false,
        }
    }

    pub(in crate::agents) fn native_graph(continuation: ApplicationContinuation) -> Self {
        let mut path = Self::new(continuation);
        // A native graph occupies the root graph depth without a saved tool hop.
        path.pipeline_depth = 1;
        path.native_graph_root = true;
        path
    }

    /// The conversation's root is a pipeline graph, not an ordinary Agent.
    pub(in crate::agents) const fn has_native_graph_root(&self) -> bool {
        self.native_graph_root
    }

    pub(in crate::agents) const fn is_scoped(&self) -> bool {
        matches!(self.continuation, ApplicationContinuation::Scoped)
    }

    pub(in crate::agents) fn for_graph_agent(&self) -> Result<Self, NativeAgentAssemblyError> {
        // Each ordinary graph scope needs one exact activation frame.
        if self.graph_agent_scopes
            >= crate::agents::pipeline::scoped_applications::MAX_ORDINARY_GRAPH_SCOPES
        {
            return Err(super::unsupported_capability());
        }
        let mut path = self.clone();
        path.graph_agent_scopes += 1;
        Ok(path)
    }

    pub(in crate::agents) fn enter_agent(
        &self,
        identity: (u64, u64),
    ) -> Result<Self, NativeAgentAssemblyError> {
        if self.agent_tier >= MAX_AGENT_TIERS {
            return Err(resource_exhausted());
        }
        let mut next = self.enter(identity)?;
        next.agent_tier += 1;
        Ok(next)
    }

    pub(in crate::agents) fn enter_pipeline(
        &self,
        identity: (u64, u64),
    ) -> Result<Self, NativeAgentAssemblyError> {
        // Tool-root depth is zero in the existing saved composition contract.
        if self.pipeline_depth > MAX_PIPELINE_COMPOSITION_DEPTH {
            return Err(resource_exhausted());
        }
        let mut next = self.enter(identity)?;
        next.pipeline_depth += 1;
        Ok(next)
    }

    fn enter(&self, identity: (u64, u64)) -> Result<Self, NativeAgentAssemblyError> {
        if identity.0 == 0 || identity.1 == 0 {
            return Err(invalid_configuration());
        }
        if self.ancestors.contains(&identity) {
            return Err(NativeAgentAssemblyError::new(
                NativeAgentAssemblyErrorCode::InvalidConfiguration,
                "the saved application composition contains a cycle",
            ));
        }
        self.hops
            .try_update(Ordering::Relaxed, Ordering::Relaxed, |value| {
                (value < MAX_APPLICATION_HOPS).then_some(value + 1)
            })
            .map_err(|_| resource_exhausted())?;
        let mut next = self.clone();
        next.ancestors.push(identity);
        Ok(next)
    }

    pub(in crate::agents) fn admit_version(
        &self,
        version: &serde_json::Map<String, serde_json::Value>,
    ) -> Result<(), NativeAgentAssemblyError> {
        let bytes = serde_json::to_vec(version)
            .map_err(|_| invalid_configuration())?
            .len();
        self.version_bytes
            .try_update(Ordering::Relaxed, Ordering::Relaxed, |value| {
                value
                    .checked_add(bytes)
                    .filter(|total| *total <= MAX_MATERIALIZATION_VERSION_BYTES)
            })
            .map_err(|_| resource_exhausted())?;
        Ok(())
    }
}
