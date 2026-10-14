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

/// The most bytes an Office document read or written through the host may
/// hold. It is `elitea-office-core`'s own input cap, restated so a host can
/// refuse before it buffers.
pub const MAX_OFFICE_OBJECT_BYTES: u64 = 50 * 1024 * 1024;

/// Why an Office host call failed, in the terms the tool tells the model:
/// each cause has its own sentence, so a refused credential is not reported as
/// an unreachable platform, nor a taken name as a bad one.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum OfficeFailure {
    /// No such file or bucket.
    NotFound,
    /// The file store refused access: the credential is refused or unusable,
    /// or the egress guard does not admit the platform host.
    AccessRefused,
    /// The name is taken and the write did not ask to replace it.
    AlreadyExists,
    /// The file store does not accept this type of file.
    UnsupportedType,
    /// The file name, the bucket or the request was refused as invalid.
    InvalidRequest,
    /// The file is over a size limit.
    TooLarge,
    /// The model call was refused for budget.
    BudgetRefused,
    /// The run binds no chat model (a pipeline node).
    NoModel,
    /// The model gateway is rate-limiting the call; trying again shortly may work.
    RateLimited,
    /// The model call timed out; trying again may work.
    Timeout,
    /// The model gateway reported a conflict; trying again may work.
    Conflict,
    /// The platform or model could not be reached.
    Unreachable,
}

/// A host failure: the cause and a data-free reason code for the log.
#[derive(Clone, Copy, Debug)]
pub struct OfficeHostError {
    kind: OfficeFailure,
    reason: &'static str,
}

impl OfficeHostError {
    #[must_use]
    pub const fn new(kind: OfficeFailure, reason: &'static str) -> Self {
        Self { kind, reason }
    }

    #[must_use]
    pub const fn kind(&self) -> OfficeFailure {
        self.kind
    }

    /// The stable reason code for logs.
    #[must_use]
    pub const fn reason_code(&self) -> &'static str {
        self.reason
    }
}

impl std::fmt::Display for OfficeHostError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.reason)
    }
}

impl std::error::Error for OfficeHostError {}

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
    /// PNG images sent with the prompt (low detail), in order. Shared, so a
    /// tool that sends them with one batch and not the next copies nothing.
    pub images_png: std::sync::Arc<[Vec<u8>]>,
}

/// One image-generation call: the gateway's `POST /llm/v1/images/generations`
/// body, as the `imagegen` toolkit builds it.
#[derive(Clone, Debug)]
pub struct ImageGenerateRequest {
    /// The toolkit's configured image-generation model (a registry name).
    pub model: String,
    /// The text prompt.
    pub prompt: String,
    /// `WIDTHxHEIGHT`, or `None` for the model's default.
    pub size: Option<String>,
    /// How many images to ask for.
    pub count: u32,
}

/// One input image of an edit: its file name and bytes.
#[derive(Clone, Debug)]
pub struct ImageEditInput {
    /// The name the upload carries.
    pub name: String,
    /// The image bytes.
    pub bytes: Vec<u8>,
}

/// One image-edit call: the gateway's multipart
/// `POST /llm/v1/images/edits`, as the `imagegen` toolkit builds it.
#[derive(Clone, Debug)]
pub struct ImageEditRequest {
    /// The toolkit's configured image-generation model (a registry name).
    pub model: String,
    /// The edit prompt.
    pub prompt: String,
    /// The source image.
    pub image: ImageEditInput,
    /// The optional mask (transparent areas mark where the edit applies).
    pub mask: Option<ImageEditInput>,
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
    ) -> Result<Vec<u8>, OfficeHostError>;

    /// Create one object of the turn's project. With `overwrite` false a
    /// name that is taken is [`OfficeFailure::AlreadyExists`] and nothing is
    /// written; true replaces it.
    async fn write_object(
        &self,
        bucket: &str,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError>;

    /// One non-streaming chat completion on the run's model, accounted and
    /// budgeted by the model gateway like every other call the run makes.
    /// Returns the reply text.
    async fn complete(&self, request: OfficeModelRequest) -> Result<String, OfficeHostError>;

    /// Generate images on the gateway's images API under the turn's own
    /// credential (the same budget, request log and execution attribution as
    /// a chat call). Returns one slot per item of the answer's `data` array,
    /// in order: the decoded bytes of an image carried inline, or `None` for
    /// an item with none (a bare URL is not fetched: the runtime opens no
    /// connection of its own beyond the platform). The position matters: the
    /// SDK names files by it.
    ///
    /// The default is a host with no images API: the `imagegen` toolkit is
    /// then left out as unsupported before this is reached.
    async fn generate_images(
        &self,
        _request: ImageGenerateRequest,
    ) -> Result<Vec<Option<Vec<u8>>>, OfficeHostError> {
        Err(OfficeHostError::new(
            OfficeFailure::NoModel,
            "office_host.no_images_api",
        ))
    }

    /// Edit an image on the gateway's images API; see
    /// [`OfficeToolHost::generate_images`].
    async fn edit_images(
        &self,
        _request: ImageEditRequest,
    ) -> Result<Vec<Option<Vec<u8>>>, OfficeHostError> {
        Err(OfficeHostError::new(
            OfficeFailure::NoModel,
            "office_host.no_images_api",
        ))
    }

    /// Whether this host reaches the images API. The `imagegen` toolkit is
    /// served only where it does.
    fn serves_images(&self) -> bool {
        false
    }
}

/// The Office host shared for one run.
pub type SharedOfficeToolHost = Arc<dyn OfficeToolHost>;
