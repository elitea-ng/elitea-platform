//! Mint Map items from the original fenced `PostgreSQL` writer authority.

use std::collections::BTreeSet;
use std::sync::Arc;

use adk_rust::graph::GraphError;
use async_trait::async_trait;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ring::digest;

use super::PostgresCheckpointer;
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
        Ok(MapExecutionIdentity {
            execution_id: self.scope.authority.execution_id.clone(),
            generation: u64::try_from(self.scope.authority.generation).map_err(|_| {
                GraphError::CheckpointError(
                    "checkpoint.invalid_scope: the Map writer generation is invalid".to_owned(),
                )
            })?,
        })
    }
    async fn for_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        kind: MapWorkerKind,
    ) -> Result<MapChildCheckpoint, GraphError> {
        if kind != MapWorkerKind::StateModifier {
            return Err(GraphError::CheckpointError(
                "checkpoint.invalid_scope: the original application family is not bound".to_owned(),
            ));
        }
        let child = self.activate_map_item(activation, item, worker).await?;
        let thread_id = child.scope.authority.thread_id.clone();
        Ok(MapChildCheckpoint {
            admitted_threads: BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer: Arc::new(child),
        })
    }
}

impl PostgresCheckpointer {
    pub(super) async fn activate_map_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
    ) -> Result<Self, GraphError> {
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
        let thread_id = map_child_thread_id(self, activation, item, worker)?;
        let authority = self.scope.authority.for_thread(thread_id)?;
        Self::activate(
            self.pool.clone(),
            authority,
            self.limits,
            Arc::clone(&self.state_writer_lease),
        )
        .await
        .map_err(Into::into)
    }
}

fn map_child_thread_id(
    parent: &PostgresCheckpointer,
    activation: &MapActivation,
    item: &FrozenMapItem,
    worker: &str,
) -> Result<String, GraphError> {
    let ordinal = u64::try_from(item.index).map_err(|_| {
        GraphError::CheckpointError(
            "checkpoint.invalid_scope: the Map item ordinal is invalid".to_owned(),
        )
    })?;
    let authority = &parent.scope.authority;
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(b"elitea.graph.map.child-thread.v1\0");
    for field in [
        authority.tenant_id.as_bytes(),
        &authority.resource_project_id.to_be_bytes(),
        &authority.projection_project_id.to_be_bytes(),
        authority.capability_id.as_bytes(),
        authority.definition_digest.as_slice(),
        authority.execution_id.as_bytes(),
        &authority.generation.to_be_bytes(),
        activation.root_thread_id.as_bytes(),
        activation.node_id.as_bytes(),
        &activation.step.to_be_bytes(),
        activation.config_digest.as_slice(),
        activation.worker_digest.as_slice(),
        activation.source_digest.as_slice(),
        worker.as_bytes(),
        &ordinal.to_be_bytes(),
        item.input_digest.as_slice(),
    ] {
        let length = u64::try_from(field.len()).map_err(|_| {
            GraphError::CheckpointError(
                "checkpoint.invalid_scope: the Map identity field is invalid".to_owned(),
            )
        })?;
        hash.update(&length.to_be_bytes());
        hash.update(field);
    }
    Ok(format!(
        "m1:{}",
        URL_SAFE_NO_PAD.encode(hash.finish().as_ref())
    ))
}

impl crate::agents::graph::map_authority::sealed::Sealed for PostgresCheckpointer {}
impl MapCheckpointAuthority for PostgresCheckpointer {}
