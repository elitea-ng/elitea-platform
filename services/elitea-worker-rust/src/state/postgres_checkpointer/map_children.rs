//! Mint Map items from the original fenced `PostgreSQL` writer authority.

use std::collections::BTreeSet;
use std::sync::Arc;

use adk_rust::graph::GraphError;
use async_trait::async_trait;

use super::PostgresCheckpointer;
use super::fanout_children::{FanoutChildKey, FanoutFamily, current_origin};
use crate::agents::graph::{
    FrozenMapItem, MapActivation, MapCheckpointAuthority, MapChildCheckpoint,
    MapChildCheckpointerFactory, MapExecutionIdentity, MapWorkerKind,
};

#[async_trait]
impl MapChildCheckpointerFactory for PostgresCheckpointer {
    fn execution_identity(&self, root_thread: &str) -> Result<MapExecutionIdentity, GraphError> {
        if root_thread != self.scope.authority.thread_id {
            return Err(GraphError::CheckpointError(
                "checkpoint.invalid_scope: the Map root is not admitted".to_owned(),
            ));
        }
        current_origin(&self.scope.authority, FanoutFamily::Map)
    }
    fn item_thread_id(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        origin: &MapExecutionIdentity,
    ) -> Result<String, GraphError> {
        self.check_map_item(activation, item, worker)?;
        map_child_thread_id(self, activation, item, worker, origin)
    }
    async fn for_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        kind: MapWorkerKind,
        origin: &MapExecutionIdentity,
    ) -> Result<MapChildCheckpoint, GraphError> {
        if kind != MapWorkerKind::StateModifier {
            return Err(GraphError::CheckpointError(
                "checkpoint.invalid_scope: the original application family is not bound".to_owned(),
            ));
        }
        let child = self
            .activate_map_item(activation, item, worker, origin)
            .await?;
        let thread_id = child.scope.authority.thread_id.clone();
        Ok(MapChildCheckpoint {
            admitted_threads: BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer: Arc::new(child),
        })
    }
}

impl PostgresCheckpointer {
    fn check_map_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
    ) -> Result<(), GraphError> {
        if activation.root_thread_id != self.scope.authority.thread_id
            || item.index >= 64
            || item.input_digest == [0; 32]
            || activation.config_digest == [0; 32]
            || activation.worker_digest == [0; 32]
            || activation.source_digest == [0; 32]
            || worker.is_empty()
            || worker.len() > 128
            || !worker.bytes().all(|byte| {
                byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':')
            })
        {
            return Err(GraphError::CheckpointError("checkpoint.invalid_scope: the Map item does not belong to this original writer family".to_owned()));
        }
        Ok(())
    }

    pub(super) async fn activate_map_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        origin: &MapExecutionIdentity,
    ) -> Result<Self, GraphError> {
        self.check_map_item(activation, item, worker)?;
        let thread_id = map_child_thread_id(self, activation, item, worker, origin)?;
        self.activate_fanout_child(thread_id).await
    }
}

fn map_child_thread_id(
    parent: &PostgresCheckpointer,
    activation: &MapActivation,
    item: &FrozenMapItem,
    worker: &str,
    origin: &MapExecutionIdentity,
) -> Result<String, GraphError> {
    let ordinal = u64::try_from(item.index).map_err(|_| {
        GraphError::CheckpointError(
            "checkpoint.invalid_scope: the Map item ordinal is invalid".to_owned(),
        )
    })?;
    let step = activation.step.to_be_bytes();
    let ordinal = ordinal.to_be_bytes();
    super::fanout_children::child_thread_id(
        &parent.scope.authority,
        &FanoutChildKey {
            family: FanoutFamily::Map,
            root_thread_id: &activation.root_thread_id,
            origin,
            fields: &[
                activation.node_id.as_bytes(),
                &step,
                activation.config_digest.as_slice(),
                activation.worker_digest.as_slice(),
                activation.source_digest.as_slice(),
                worker.as_bytes(),
                &ordinal,
                item.input_digest.as_slice(),
            ],
        },
    )
}

impl crate::agents::graph::map_authority::sealed::Sealed for PostgresCheckpointer {}
impl MapCheckpointAuthority for PostgresCheckpointer {}
