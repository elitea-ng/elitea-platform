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

pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

use std::sync::Arc;

use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::transport::platform_client::PlatformClient;

/// The live claim the artifact family acts under, shared for one run.
///
/// `ClaimBoundRuntimeContextAuthority` is deliberately neither cloneable nor
/// formattable (`protocol::control`), and this type does not weaken either
/// property: it shares the single minted authority behind an `Arc` rather than
/// duplicating it, exactly as `BuilderToolAuthority` does.
#[derive(Clone)]
pub(crate) struct ArtifactToolAuthority {
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

impl ArtifactToolAuthority {
    #[must_use]
    pub(crate) const fn new(
        platform: Arc<PlatformClient>,
        authority: Arc<ClaimBoundRuntimeContextAuthority>,
    ) -> Self {
        Self {
            platform,
            authority,
        }
    }

    pub(in crate::toolkits) fn platform(&self) -> &PlatformClient {
        &self.platform
    }

    pub(in crate::toolkits) fn authority(&self) -> &ClaimBoundRuntimeContextAuthority {
        &self.authority
    }
}
