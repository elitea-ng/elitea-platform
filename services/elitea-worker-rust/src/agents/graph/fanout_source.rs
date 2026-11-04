//! The closed set of fan-out sources the shared adapter runs (T-2A3).
//!
//! Every per-family difference lives here, in one place: the node id, the
//! trace and join kind, the children, the concurrency, On Error, Join and the
//! output key. The adapter in `parallel.rs` reads only this, never a family's
//! own definition. A Map's children are known only at activation (one per
//! source item), so `children` takes the node context.
//!
//! The identity domain is per family too: a Map keeps main's child-thread
//! fields (`map_children.rs`), carried by the activation's `map` identity,
//! its own configuration digest (no owned-definition fold) and its own
//! frozen item input (the broadcast plus `{item, index}`).

use std::borrow::Cow;

use adk_rust::graph::{GraphError, NodeContext, State};
use elitea_agent_runtime::graph::fanout::plan::{FanoutKind as JoinKind, JoinMode, OnError};

use super::fanout_trace::FanoutKind;
use super::map_reduce::{MapDefinition, map_error, source_digest, validate_state_structure};
use super::parallel::{MapIdentity, parallel_error};
use super::yaml::{ParallelBranchDefinition, ParallelNodeDefinition};

#[derive(Clone)]
pub(crate) enum FanoutSource {
    Parallel(ParallelNodeDefinition),
    #[cfg_attr(
        not(test),
        expect(
            dead_code,
            reason = "the Map node runs on the shared adapter in T-2A3 commit B2"
        )
    )]
    Map(MapSource),
}

/// One child's frozen input as its source derives it, in child order.
pub(crate) struct SourceChildInput {
    pub(crate) owned_definition_digest: [u8; 32],
    pub(crate) input_digest: [u8; 32],
    pub(crate) input: State,
}

/// A Map node as a fan-out source: its definition, the digest of its owned
/// worker, and the digests derived once at construction.
#[derive(Clone)]
pub(crate) struct MapSource {
    definition: MapDefinition,
    worker_digest: [u8; 32],
    config_digest: [u8; 32],
    max_concurrency: u32,
}

impl MapSource {
    #[cfg_attr(
        not(test),
        expect(
            dead_code,
            reason = "the Map node runs on the shared adapter in T-2A3 commit B2"
        )
    )]
    pub(crate) fn new(
        definition: MapDefinition,
        worker_digest: [u8; 32],
    ) -> Result<Self, GraphError> {
        definition.validate()?;
        if worker_digest == [0; 32] {
            return Err(map_error("invalid_configuration"));
        }
        let config_digest = definition.config_digest()?;
        let max_concurrency = u32::try_from(definition.max_concurrency)
            .map_err(|_| map_error("invalid_configuration"))?;
        Ok(Self {
            definition,
            worker_digest,
            config_digest,
            max_concurrency,
        })
    }
}

impl FanoutSource {
    pub(crate) fn id(&self) -> &str {
        match self {
            Self::Parallel(definition) => definition.id(),
            Self::Map(map) => &map.definition.id,
        }
    }

    /// The `kind` every fan-out span carries.
    pub(crate) const fn kind(&self) -> FanoutKind {
        match self {
            Self::Parallel(_) => FanoutKind::Parallel,
            Self::Map(_) => FanoutKind::Map,
        }
    }

    /// The join entry's family (its key field and limits).
    pub(crate) const fn join_kind(&self) -> JoinKind {
        match self {
            Self::Parallel(_) => JoinKind::Parallel,
            Self::Map(_) => JoinKind::Map,
        }
    }

    /// The children of this activation, in declared order: a Parallel's
    /// branches, or one child per Map source item, keyed by its index.
    pub(crate) fn children(
        &self,
        context: &NodeContext,
    ) -> Result<Cow<'_, [ParallelBranchDefinition]>, GraphError> {
        match self {
            Self::Parallel(definition) => Ok(Cow::Borrowed(definition.branches())),
            Self::Map(map) => {
                let items = map.definition.source_items(&context.state)?;
                Ok(Cow::Owned(
                    (0..items.len())
                        .map(|index| {
                            ParallelBranchDefinition::map_item(index, &map.definition.worker)
                        })
                        .collect(),
                ))
            }
        }
    }

    /// The children the node declares before activation, if it declares
    /// them (a Parallel's branches); `None` when they come from state.
    pub(crate) fn declared_children(&self) -> Option<&[ParallelBranchDefinition]> {
        match self {
            Self::Parallel(definition) => Some(definition.branches()),
            Self::Map(_) => None,
        }
    }

    pub(crate) fn max_concurrency(&self) -> u32 {
        match self {
            Self::Parallel(definition) => definition.max_concurrency(),
            Self::Map(map) => map.max_concurrency,
        }
    }

    pub(crate) fn on_error(&self) -> OnError {
        match self {
            Self::Parallel(definition) => definition.on_error(),
            Self::Map(map) => map.definition.on_error(),
        }
    }

    pub(crate) fn join(&self) -> JoinMode {
        match self {
            Self::Parallel(definition) => definition.join(),
            Self::Map(map) => map.definition.join(),
        }
    }

    pub(crate) fn output_key(&self) -> &str {
        match self {
            Self::Parallel(definition) => definition.output_key(),
            Self::Map(map) => &map.definition.destination,
        }
    }

    /// Stable digest bound into every child identity and card key. A
    /// Parallel's is folded with its owned branch definitions at activation;
    /// a Map's is final (main's `map.config.v1`).
    pub(crate) fn config_digest(&self) -> [u8; 32] {
        match self {
            Self::Parallel(definition) => definition.config_digest(),
            Self::Map(map) => map.config_digest,
        }
    }

    /// The per-family identity a child id binds beyond the activation's
    /// shared fields. Only a Map has one, and it always has one: its worker
    /// and the digest of this activation's frozen source items.
    pub(crate) fn map_identity(
        &self,
        context: &NodeContext,
    ) -> Result<Option<MapIdentity>, GraphError> {
        match self {
            Self::Parallel(_) => Ok(None),
            Self::Map(map) => Ok(Some(MapIdentity {
                worker_digest: map.worker_digest,
                source_digest: source_digest(map.definition.source_items(&context.state)?)?,
            })),
        }
    }

    /// The frozen child inputs a source derives itself. `None` for a
    /// Parallel: the runtime projects each branch's own mapping and folds
    /// the owned branch definitions into the activation's digest. A Map
    /// freezes the broadcast plus `{item, index}` per item, digested with
    /// main's `map.item-input.v1`, and folds nothing.
    pub(crate) fn frozen_inputs(
        &self,
        children: &[ParallelBranchDefinition],
        parent: &State,
    ) -> Option<Result<Vec<SourceChildInput>, GraphError>> {
        match self {
            Self::Parallel(_) => None,
            Self::Map(map) => Some(map.frozen_inputs(children, parent)),
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
            Self::Map(map) => map.definition.validate(),
        }
    }
}

impl MapSource {
    fn frozen_inputs(
        &self,
        children: &[ParallelBranchDefinition],
        parent: &State,
    ) -> Result<Vec<SourceChildInput>, GraphError> {
        validate_state_structure(parent)?;
        let items = self.definition.source_items(parent)?;
        let frozen = self.definition.item_inputs(parent, items)?;
        if frozen.len() != children.len() {
            return Err(map_error("invalid_activation"));
        }
        frozen
            .into_iter()
            .zip(children)
            .map(|(item, child)| {
                if child.id() != item.index.to_string() || child.node() != self.definition.worker {
                    return Err(map_error("invalid_activation"));
                }
                Ok(SourceChildInput {
                    owned_definition_digest: self.worker_digest,
                    input_digest: item.input_digest,
                    input: item.input,
                })
            })
            .collect()
    }
}
