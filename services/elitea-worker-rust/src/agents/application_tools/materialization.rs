//! One bounded assembly path across saved graphs and ordinary Agents.

use std::collections::BTreeSet;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use super::{
    MAX_AGENT_TIERS, MAX_APPLICATION_HOPS, MAX_SKIPPED_APPLICATION_CHILDREN,
    NativeAgentAssemblyError, NativeAgentAssemblyErrorCode, SkippedApplicationChild,
    invalid_configuration, resource_exhausted,
};
use crate::agents::pipeline::composition::MAX_PIPELINE_COMPOSITION_DEPTH;

pub(in crate::agents) const MAX_MATERIALIZATION_VERSION_BYTES: usize = 4 * 1024 * 1024;

/// The typed cause every composition bound violation carries: a cycle, nesting past a tier,
/// depth or scope bound, the hop or checkpoint-path budget, or a pausing pipeline tool a graph
/// root cannot continue. Main checks the same bounds when a composition is saved. These are authoring errors of the saved composition, not runtime
/// unavailability, so they are refused (`native_agent_lifecycle::assembly_failure` turns the
/// cause into its registered readable terminal) and never degrade into a skipped attachment.
pub(crate) const COMPOSITION_BOUND_CODE: &str = "application.composition_bound";

/// Which bound a composition violated; data-free, logged with the cause.
pub(crate) mod bound {
    pub(crate) const CYCLE: &str = "cycle";
    pub(crate) const AGENT_TIERS: &str = "agent_tiers";
    pub(crate) const PIPELINE_DEPTH: &str = "pipeline_depth";
    pub(crate) const ORDINARY_GRAPH_SCOPES: &str = "ordinary_graph_scopes";
    pub(crate) const APPLICATION_HOPS: &str = "application_hops";
    pub(crate) const CHECKPOINT_PATHS: &str = "checkpoint_paths";
    pub(crate) const STATIC_PAUSE_UNDER_GRAPH_ROOT: &str = "static_pause_under_graph_root";
}

/// Mark `error` as a composition bound violation (see [`COMPOSITION_BOUND_CODE`]).
pub(crate) const fn composition_bound(
    error: NativeAgentAssemblyError,
    detail: &'static str,
) -> NativeAgentAssemblyError {
    error.with_cause(COMPOSITION_BOUND_CODE, Some(detail))
}

/// Whether `error` is a composition bound violation, which is refused, never skipped.
pub(crate) fn is_composition_bound(error: &NativeAgentAssemblyError) -> bool {
    error
        .cause()
        .is_some_and(|cause| cause.code() == COMPOSITION_BOUND_CODE)
}

/// The typed cause of a composition whose loaded versions exceed the byte budget. Main cannot
/// measure the frozen version documents the Worker loads, so this is not a save-time bound: an
/// attachment that would exceed it is skipped and named (`too_large`) where the caller can
/// degrade, and a root that cannot load its own composition ends with its own registered
/// terminal (`RuntimeFailureKind::ApplicationCompositionTooLarge`).
pub(crate) const COMPOSITION_SIZE_CODE: &str = "application.composition_size";

/// The byte budget of one composition is exhausted (see [`COMPOSITION_SIZE_CODE`]).
pub(crate) fn composition_size() -> NativeAgentAssemblyError {
    resource_exhausted().with_cause(COMPOSITION_SIZE_CODE, None)
}

/// Whether `error` is an exhausted composition byte budget.
pub(crate) fn is_composition_size(error: &NativeAgentAssemblyError) -> bool {
    error
        .cause()
        .is_some_and(|cause| cause.code() == COMPOSITION_SIZE_CODE)
}

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
    /// Every unbuildable attachment skipped anywhere below the root (FU-243). Shared like the
    /// hop counter, so the root names an attachment a pipeline `agent` node's Agent left out
    /// however deep it sits. Bounded by `MAX_SKIPPED_APPLICATION_CHILDREN`.
    skipped: Arc<Mutex<BTreeSet<SkippedApplicationChild>>>,
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
            skipped: Arc::default(),
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
            return Err(composition_bound(
                super::unsupported_capability(),
                bound::ORDINARY_GRAPH_SCOPES,
            ));
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
            return Err(composition_bound(resource_exhausted(), bound::AGENT_TIERS));
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
            return Err(composition_bound(
                resource_exhausted(),
                bound::PIPELINE_DEPTH,
            ));
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
            return Err(composition_bound(
                NativeAgentAssemblyError::new(
                    NativeAgentAssemblyErrorCode::InvalidConfiguration,
                    "the saved application composition contains a cycle",
                ),
                bound::CYCLE,
            ));
        }
        self.hops
            .try_update(Ordering::Relaxed, Ordering::Relaxed, |value| {
                (value < MAX_APPLICATION_HOPS).then_some(value + 1)
            })
            .map_err(|_| composition_bound(resource_exhausted(), bound::APPLICATION_HOPS))?;
        let mut next = self.clone();
        next.ancestors.push(identity);
        Ok(next)
    }

    /// Remember attachments this assembly left out, for the root's skipped notice.
    pub(in crate::agents) fn record_skipped(&self, children: &[SkippedApplicationChild]) {
        if children.is_empty() {
            return;
        }
        // A poisoned set only loses notice entries, never assembly correctness.
        let Ok(mut skipped) = self.skipped.lock() else {
            return;
        };
        for child in children {
            if skipped.len() >= MAX_SKIPPED_APPLICATION_CHILDREN {
                break;
            }
            skipped.insert(child.clone());
        }
    }

    /// Every attachment left out anywhere in this assembly, in a stable order.
    pub(in crate::agents) fn skipped(&self) -> Vec<SkippedApplicationChild> {
        self.skipped
            .lock()
            .map(|skipped| skipped.iter().cloned().collect())
            .unwrap_or_default()
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
            .map_err(|_| composition_size())?;
        Ok(())
    }
}
