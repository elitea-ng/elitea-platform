//! The `artifact` toolkit family (#906): claim-scoped bucket list, read, write
//! and delete.
//!
//! IT IS NOT LIKE ITS NEIGHBOURS, and the difference is the whole reason it did
//! not exist until now. Every other family in this directory talks to a third
//! party over the egress allowlist with a credential main redeemed into the
//! frozen snapshot, so `materialize.rs` can build it from settings alone. An
//! artifact bucket is THIS platform's own storage: there is no third-party
//! endpoint, no credential to redeem, and the worker's egress allowlist reaches
//! the model gateway alone. `materialize.rs`'s own header says so — "artifact-
//! owning families have separate authority owners and therefore never fall
//! through this map".
//!
//! The authority it owns instead is the live execution CLAIM, and main serves
//! the four operations from the private mTLS content listener
//! (`ContentServer.PostArtifact*`,
//! `services/elitea-main/internal/infra/storage/runtime_artifact_object.go`),
//! where the project comes from the claimed execution row and the per-bucket
//! access list is applied to the claim's own actor. So this family is built
//! where the claim is in scope — `agents::ordinary::materialize_runtime`, the
//! same place the two builder tools are bound — and carries
//! `ArtifactToolAuthority` for the same reason `BuilderToolAuthority` exists:
//! a tool the model calls mid-run has no other way to hold the claim it must
//! act under, and minting a second one would be a second authorization this
//! worker is not permitted to perform.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub mod config;
pub mod tools;

use std::sync::Arc;

use crate::host::PlatformWriter;

/// The platform-write authority the artifact family acts under, shared for
/// one run.
///
/// In the cloud worker it is the live claim (`ClaimPlatformWriter` holds the
/// `PlatformClient` and the claim-bound runtime-context authority, which is
/// neither cloneable nor formattable and is shared behind an `Arc`, never
/// duplicated); on the desktop it is the native session. The family sees
/// only the [`PlatformWriter`] capability.
#[derive(Clone)]
pub struct ArtifactToolAuthority {
    writer: Arc<dyn PlatformWriter>,
}

impl ArtifactToolAuthority {
    #[must_use]
    pub fn new(writer: Arc<dyn PlatformWriter>) -> Self {
        Self { writer }
    }

    pub fn writer(&self) -> &dyn PlatformWriter {
        self.writer.as_ref()
    }
}
