//! What an Office toolkit family needs of its host (ADR-0032): binary
//! artifacts in and out, and a chat model.
//!
//! The `artifact` family moves *text*: the platform's runtime-context read
//! returns a file's text and its write takes a string capped at 64k
//! characters. A `.pptx` is a zip, so the Office families read and write the
//! object bytes instead, through this capability. The cloud worker lends it
//! over the platform's object API under the turn's own credential (the one
//! the indexing artifact source already reads with) and the model facade it
//! already binds for the step-back tools; nothing here opens a connection of
//! its own, and no new internal route exists (AGENTS.md: internal calls are
//! gRPC over mTLS, never new REST).
//!
//! Like [`super::artifact::ArtifactToolAuthority`] it is an authority the
//! frozen settings cannot carry: it is the live claim's. It rides on that
//! authority ([`super::artifact::ArtifactToolAuthority::with_office`]) so
//! exactly the paths that lend artifact access (an ordinary agent, a root
//! pipeline's nodes) serve the Office families, and a host that lends none
//! (the desktop, until P6) leaves them out with the usual "unsupported
//! toolkit" skip.

use std::sync::Arc;

use async_trait::async_trait;

use crate::host::HostError;

/// The most bytes an Office document read or written through the host may
/// hold. It is `elitea-office-core`'s own input cap, restated so a host can
/// refuse before it buffers.
pub const MAX_OFFICE_OBJECT_BYTES: u64 = 50 * 1024 * 1024;

/// One object stored, as the host confirms it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct OfficeObjectWritten {
    /// The bucket written to.
    pub bucket: String,
    /// The object key written.
    pub name: String,
    /// The media type the object was stored with.
    pub media_type: String,
    /// The stored size.
    pub byte_length: u64,
}

/// One chat-model call a tool makes inside its own run: a user prompt, with
/// optional PNG images beside it. No system prompt and no tools: the SDK's
/// `fill_template` and `translate_presentation` send exactly one human
/// message per batch.
#[derive(Clone, Debug, Default)]
pub struct OfficeModelRequest {
    /// The user message text.
    pub prompt: String,
    /// PNG images sent with the prompt (low detail), in order.
    pub images_png: Vec<Vec<u8>>,
}

/// The host side of the Office families.
#[async_trait]
pub trait OfficeToolHost: Send + Sync {
    /// The bytes of one object of the turn's project. `max_bytes` bounds
    /// the read: a larger object is a
    /// [`crate::host::HostErrorCode::ResourceExhausted`] error and is not
    /// buffered.
    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, HostError>;

    /// Create or replace one object of the turn's project.
    async fn write_object(
        &self,
        bucket: &str,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
    ) -> Result<OfficeObjectWritten, HostError>;

    /// One non-streaming chat completion on the run's model, accounted and
    /// budgeted by the model gateway like every other call the run makes.
    /// Returns the reply text.
    async fn complete(&self, request: OfficeModelRequest) -> Result<String, HostError>;
}

/// The Office host shared for one run.
pub type SharedOfficeToolHost = Arc<dyn OfficeToolHost>;
