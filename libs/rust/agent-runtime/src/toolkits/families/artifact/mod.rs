//! The `artifact` toolkit family (#906): claim-scoped bucket list, read, write
//! and delete, and (`extra_tools.rs`) read-many, grep, edit, append, metadata and
//! bucket creation.
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
mod extra_tools;
pub mod tools;

use std::sync::Arc;

use crate::host::PlatformWriter;

use super::office_host::SharedOfficeToolHost;
use super::web::WebToolHost;

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
    office: Option<SharedOfficeToolHost>,
    web: Option<WebToolHost>,
}

impl ArtifactToolAuthority {
    #[must_use]
    pub fn new(writer: Arc<dyn PlatformWriter>) -> Self {
        Self {
            writer,
            office: None,
            web: None,
        }
    }

    /// This authority with the web capability (#1238): the claim's meter and
    /// the deployment's egress policy. Without it a `web` or `host_intel`
    /// toolkit is built but refuses every call (`metering_unavailable`).
    #[must_use]
    pub fn with_web(mut self, web: WebToolHost) -> Self {
        self.web = Some(web);
        self
    }

    /// The web capability, when the host lends one.
    #[must_use]
    pub fn web(&self) -> Option<&WebToolHost> {
        self.web.as_ref()
    }

    /// This authority with the Office capability (ADR-0032): binary objects
    /// in and out and a chat model, for the families that author documents.
    /// Without it a `pptx` toolkit is left out as unsupported.
    #[must_use]
    pub fn with_office(mut self, office: SharedOfficeToolHost) -> Self {
        self.office = Some(office);
        self
    }

    /// The Office capability, when the host lends one.
    #[must_use]
    pub fn office(&self) -> Option<&SharedOfficeToolHost> {
        self.office.as_ref()
    }

    pub fn writer(&self) -> &dyn PlatformWriter {
        self.writer.as_ref()
    }
}
