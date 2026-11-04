//! The closed set of fan-out sources the shared adapter runs (T-2A3).
//!
//! Every per-family difference lives here, in one place: the node id, the
//! trace and join kind, the children, the concurrency, On Error, Join and the
//! output key. The adapter in `parallel.rs` reads only this, never a family's
//! own definition. A Map's children are known only at activation (one per
//! source item), so `children` takes the node context.

use std::borrow::Cow;

use adk_rust::graph::{GraphError, NodeContext};
use elitea_agent_runtime::graph::fanout::plan::{FanoutKind as JoinKind, JoinMode, OnError};

use super::fanout_trace::FanoutKind;
use super::parallel::parallel_error;
use super::yaml::{ParallelBranchDefinition, ParallelNodeDefinition};

#[derive(Clone)]
pub(crate) enum FanoutSource {
    Parallel(ParallelNodeDefinition),
}

impl FanoutSource {
    pub(crate) fn id(&self) -> &str {
        match self {
            Self::Parallel(definition) => definition.id(),
        }
    }

    /// The `kind` every fan-out span carries.
    pub(crate) const fn kind(&self) -> FanoutKind {
        match self {
            Self::Parallel(_) => FanoutKind::Parallel,
        }
    }

    /// The join entry's family (its key field and limits).
    pub(crate) const fn join_kind(&self) -> JoinKind {
        match self {
            Self::Parallel(_) => JoinKind::Parallel,
        }
    }

    /// The children of this activation, in declared order.
    #[expect(
        clippy::unnecessary_wraps,
        reason = "a Map source reads its children from state and can refuse them"
    )]
    pub(crate) fn children(
        &self,
        _context: &NodeContext,
    ) -> Result<Cow<'_, [ParallelBranchDefinition]>, GraphError> {
        match self {
            Self::Parallel(definition) => Ok(Cow::Borrowed(definition.branches())),
        }
    }

    /// The children the node declares before activation, if it declares
    /// them (a Parallel's branches); `None` when they come from state.
    #[expect(
        clippy::unnecessary_wraps,
        reason = "a Map source declares no children before activation"
    )]
    pub(crate) fn declared_children(&self) -> Option<&[ParallelBranchDefinition]> {
        match self {
            Self::Parallel(definition) => Some(definition.branches()),
        }
    }

    pub(crate) fn max_concurrency(&self) -> u32 {
        match self {
            Self::Parallel(definition) => definition.max_concurrency(),
        }
    }

    pub(crate) const fn on_error(&self) -> OnError {
        match self {
            Self::Parallel(definition) => definition.on_error(),
        }
    }

    pub(crate) const fn join(&self) -> JoinMode {
        match self {
            Self::Parallel(definition) => definition.join(),
        }
    }

    pub(crate) fn output_key(&self) -> &str {
        match self {
            Self::Parallel(definition) => definition.output_key(),
        }
    }

    /// Stable digest bound into every child identity and card key.
    pub(crate) fn config_digest(&self) -> [u8; 32] {
        match self {
            Self::Parallel(definition) => definition.config_digest(),
        }
    }

    /// The node's own configuration contract, before any child is admitted.
    pub(crate) fn validate(&self) -> Result<(), GraphError> {
        match self {
            Self::Parallel(definition) => {
                definition.validate().map_err(|error| {
                    parallel_error(error.code(), "the parallel node configuration is invalid")
                })?;
                if !(2..=16).contains(&definition.branches().len())
                    || !(1..=8).contains(&definition.max_concurrency())
                {
                    return Err(parallel_error(
                        "graph.parallel.invalid_configuration",
                        "the parallel contract requires 2-16 branches and concurrency 1-8",
                    ));
                }
                Ok(())
            }
        }
    }
}
