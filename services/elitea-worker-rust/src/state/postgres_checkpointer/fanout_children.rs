//! One lineage store for every fan-out family (Parallel branches, Map items).
//!
//! A fan-out child thread is a pure function of the parent writer's tenant,
//! project, capability and definition scope, the activation, the child key
//! and the **frozen** origin the occurrence stored when it first froze its
//! children. It never reads the current execution, generation or claim, so a
//! continuation, a reclaim and a node-recovery generation bump all re-derive
//! the same child threads. Activation always goes through the parent's fenced
//! writer: the thread name is identity only and grants no authority.

use std::sync::Arc;

use adk_rust::graph::GraphError;
use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;

use super::{CheckpointWriterAuthority, PostgresCheckpointer};
use crate::agents::graph::FanoutChildOrigin;

/// The fan-out family a child belongs to. Each keeps its own hash domain and
/// thread prefix, so ids minted before the stores were unified stay valid.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum FanoutFamily {
    Parallel,
    Map,
}

impl FanoutFamily {
    const fn domain(self) -> &'static [u8] {
        match self {
            Self::Parallel => b"elitea.graph.parallel.child-thread.v1\0",
            Self::Map => b"elitea.graph.map.child-thread.v1\0",
        }
    }

    const fn prefix(self) -> &'static str {
        match self {
            Self::Parallel => "p1:",
            Self::Map => "m1:",
        }
    }

    const fn label(self) -> &'static str {
        match self {
            Self::Parallel => "parallel",
            Self::Map => "Map",
        }
    }
}

/// Where the family's fields sit relative to the root thread in the digest.
/// Parallel hashes `root, origin`; Map hashes `origin, root`. Both orders are
/// kept byte-for-byte; only the code is shared.
pub(super) struct FanoutChildKey<'a> {
    pub(super) family: FanoutFamily,
    pub(super) root_thread_id: &'a str,
    pub(super) origin: &'a FanoutChildOrigin,
    /// Activation and child fields after the root/origin pair, in order.
    pub(super) fields: &'a [&'a [u8]],
}

/// The origin of a brand-new occurrence: the current writer's execution.
pub(super) fn current_origin(
    authority: &CheckpointWriterAuthority,
    family: FanoutFamily,
) -> Result<FanoutChildOrigin, GraphError> {
    Ok(FanoutChildOrigin {
        execution_id: authority.execution_id.clone(),
        generation: u64::try_from(authority.generation).map_err(|_| {
            GraphError::CheckpointError(format!(
                "checkpoint.invalid_scope: the {} writer generation is invalid",
                family.label()
            ))
        })?,
    })
}

/// Derive one child thread. Pure: activates nothing and reads no clock.
pub(super) fn child_thread_id(
    authority: &CheckpointWriterAuthority,
    key: &FanoutChildKey<'_>,
) -> Result<String, GraphError> {
    let generation = i64::try_from(key.origin.generation).map_err(|_| {
        GraphError::CheckpointError(format!(
            "checkpoint.invalid_scope: the {} origin generation is invalid",
            key.family.label()
        ))
    })?;
    let generation = generation.to_be_bytes();
    let resource = authority.resource_project_id.to_be_bytes();
    let projection = authority.projection_project_id.to_be_bytes();
    let scope: [&[u8]; 5] = [
        authority.tenant_id.as_bytes(),
        &resource,
        &projection,
        authority.capability_id.as_bytes(),
        authority.definition_digest.as_slice(),
    ];
    let root = key.root_thread_id.as_bytes();
    let execution = key.origin.execution_id.as_bytes();
    let anchor: [&[u8]; 3] = match key.family {
        FanoutFamily::Parallel => [root, execution, &generation],
        FanoutFamily::Map => [execution, &generation, root],
    };
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(key.family.domain());
    for field in scope.iter().chain(anchor.iter()).chain(key.fields.iter()) {
        let length = u64::try_from(field.len()).map_err(|_| {
            GraphError::CheckpointError(format!(
                "checkpoint.invalid_scope: the {} identity field is invalid",
                key.family.label()
            ))
        })?;
        hash.update(&length.to_be_bytes());
        hash.update(field);
    }
    Ok(format!(
        "{}{}",
        key.family.prefix(),
        URL_SAFE_NO_PAD.encode(hash.finish().as_ref())
    ))
}

impl PostgresCheckpointer {
    /// Activate one derived child under this writer's exact fenced claim.
    pub(super) async fn activate_fanout_child(
        &self,
        thread_id: String,
    ) -> Result<Self, GraphError> {
        let authority = self.scope.authority.for_thread(thread_id)?;
        Self::activate_under_root(
            self.pool.clone(),
            authority,
            &self.run_root_thread_id,
            self.limits,
            Arc::clone(&self.state_writer_lease),
            &self.io,
        )
        .await
        .map_err(Into::into)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::postgres_checkpointer::APPLICATION_CAPABILITY_ID;

    fn authority(execution: &str, generation: u64, claim: u64) -> CheckpointWriterAuthority {
        CheckpointWriterAuthority::new(
            "tenant-1".to_owned(),
            7,
            8,
            APPLICATION_CAPABILITY_ID,
            [0x11; 32],
            "root-thread".to_owned(),
            execution.to_owned(),
            generation,
            format!("claim-{claim}"),
            claim,
            claim,
            1_700_000_000_000_000,
            "workload-1".to_owned(),
            "producer-1".to_owned(),
            [0x22; 32],
        )
        .expect("valid writer")
    }

    fn origin() -> FanoutChildOrigin {
        FanoutChildOrigin {
            execution_id: "execution-1".to_owned(),
            generation: 1,
        }
    }

    const STEP: u64 = 4;
    const ORDINAL: u64 = 2;

    fn parallel_fields() -> Vec<Vec<u8>> {
        vec![
            b"gather".to_vec(),
            STEP.to_be_bytes().to_vec(),
            vec![0x33; 32],
            b"third".to_vec(),
            b"ask".to_vec(),
            ORDINAL.to_be_bytes().to_vec(),
            vec![0x44; 32],
        ]
    }

    fn map_fields() -> Vec<Vec<u8>> {
        vec![
            b"each".to_vec(),
            STEP.to_be_bytes().to_vec(),
            vec![0x33; 32],
            vec![0x55; 32],
            vec![0x66; 32],
            b"worker".to_vec(),
            ORDINAL.to_be_bytes().to_vec(),
            vec![0x44; 32],
        ]
    }

    fn derive(family: FanoutFamily, writer: &CheckpointWriterAuthority) -> String {
        let fields = match family {
            FanoutFamily::Parallel => parallel_fields(),
            FanoutFamily::Map => map_fields(),
        };
        let refs = fields.iter().map(Vec::as_slice).collect::<Vec<_>>();
        child_thread_id(
            writer,
            &FanoutChildKey {
                family,
                root_thread_id: "root-thread",
                origin: &origin(),
                fields: &refs,
            },
        )
        .expect("derive")
    }

    /// Golden vectors computed independently from the pre-unification hash
    /// layout (length-prefixed SHA-256 fields): ids already stored stay valid.
    #[test]
    fn unified_derivation_keeps_the_pre_unification_ids() {
        let writer = authority("execution-1", 1, 1);
        assert_eq!(
            derive(FanoutFamily::Parallel, &writer),
            "p1:Z8y5h_dA4WoUJhoL5KeWzytJ6KsEHvKFo9OBSMzxW6Q"
        );
        assert_eq!(
            derive(FanoutFamily::Map, &writer),
            "m1:KRe5343ZNGVAvfuZ-MJ-RAfTgyFfIxbwR_9OVB4NU24"
        );
    }

    /// The child thread depends on the frozen origin only: a later execution,
    /// claim or generation of the current writer derives the same thread.
    #[test]
    fn child_threads_ignore_the_current_execution_claim_and_generation() {
        for family in [FanoutFamily::Parallel, FanoutFamily::Map] {
            let first = derive(family, &authority("execution-1", 1, 1));
            for writer in [
                authority("execution-2", 1, 2),
                authority("execution-1", 2, 3),
                authority("execution-9", 5, 9),
            ] {
                assert_eq!(derive(family, &writer), first);
            }
        }
        assert_ne!(
            derive(FanoutFamily::Parallel, &authority("execution-1", 1, 1)),
            derive(FanoutFamily::Map, &authority("execution-1", 1, 1))
        );
    }

    #[test]
    fn current_origin_is_the_writers_execution() {
        let writer = authority("execution-7", 3, 4);
        assert_eq!(
            current_origin(&writer, FanoutFamily::Map).expect("origin"),
            FanoutChildOrigin {
                execution_id: "execution-7".to_owned(),
                generation: 3
            }
        );
    }
}
