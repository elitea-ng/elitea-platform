//! The host interface: everything the runtime needs from the process it runs
//! in (ADR-0029 decision 2).
//!
//! Two hosts implement it. The cloud worker (`services/elitea-worker-rust`)
//! answers each trait from a claimed turn: claim-bound runtime-context
//! routes, the mTLS model facade, materialised toolkit credentials, the gRPC
//! output stream, the agent-state PostgreSQL database and the claim lease.
//! The desktop host answers the same traits over the public HTTPS API with a
//! native token, a local SQLite store and a local cancellation token.
//!
//! [`PlatformWriter`] and [`CodeSandbox`] joined in stages 2–3 of
//! `EXTRACTION.md` (its finding 1: the first trait set had no way to write to
//! the platform or to run code).
//!
//! Each trait is shaped from the worker call sites it replaces, named in its
//! documentation. They are object safe (`Arc<dyn …>`), carry no claim, lease,
//! fence or credential type, and return the data-free [`HostError`]. The
//! runtime modules still in the worker take the cloud types directly; each
//! extraction stage in `EXTRACTION.md` switches the modules it moves to these
//! traits.

use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use crate::platform::{
    ArtifactDeleteOutcome, ArtifactDeleteRequest, ArtifactListOutcome, ArtifactListRequest,
    ArtifactReadOutcome, ArtifactReadRequest, ArtifactWriteOutcome, ArtifactWriteRequest,
    ProjectContextWriteOutcome, ProjectContextWriteRequest, SkillWriteOutcome, SkillWriteRequest,
};
use adk_core::{Event, Llm, Toolset};
use adk_graph::Checkpointer;
use adk_session::SessionService;
use async_trait::async_trait;
use serde_json::{Map, Value};

/// Stable, data-free classification of a host failure.
///
/// Mirrors the codes the worker already distinguishes
/// (`RuntimeContextError`, `ModelFacadeError`, `NativeAgentAssemblyErrorCode`)
/// so a cloud adapter maps one to one and a desktop adapter maps HTTP status
/// classes onto the same set.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HostErrorCode {
    /// The host or the request is misconfigured; not retryable.
    InvalidConfiguration,
    /// The runtime asked for something malformed; not retryable.
    InvalidInput,
    /// The platform answered with something the host cannot accept.
    InvalidResponse,
    /// The referenced resource does not exist (terminal, unlike unavailable).
    NotFound,
    /// The caller's grants do not cover the request.
    AuthorizationFailed,
    /// A bound (size, count, budget) was exceeded.
    ResourceExhausted,
    /// The platform or a dependency is unavailable; retryable.
    DependencyUnavailable,
    /// The execution was stopped or lost its authority.
    ExecutionEnded,
}

impl HostErrorCode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "runtime_host.invalid_configuration",
            Self::InvalidInput => "runtime_host.invalid_input",
            Self::InvalidResponse => "runtime_host.invalid_response",
            Self::NotFound => "runtime_host.not_found",
            Self::AuthorizationFailed => "runtime_host.authorization_failed",
            Self::ResourceExhausted => "runtime_host.resource_exhausted",
            Self::DependencyUnavailable => "runtime_host.dependency_unavailable",
            Self::ExecutionEnded => "runtime_host.execution_ended",
        }
    }

    #[must_use]
    pub const fn retryable(self) -> bool {
        matches!(self, Self::DependencyUnavailable)
    }
}

/// A host failure: a stable code and a static, data-free message. No host
/// puts response bodies, tokens or user content in here.
///
/// Deliberately not `PartialEq`: the log-only reason
/// ([`HostError::with_reason`]) must not decide equality, and nothing compares
/// whole errors. Compare [`HostError::code`] (what the runtime branches on)
/// and, in tests, [`HostError::reason_code`].
#[derive(Clone, Copy)]
pub struct HostError {
    code: HostErrorCode,
    message: &'static str,
    reason: Option<&'static str>,
}

impl HostError {
    #[must_use]
    pub const fn new(code: HostErrorCode, message: &'static str) -> Self {
        Self {
            code,
            message,
            reason: None,
        }
    }

    /// Keep the host's own stable reason code (the cloud adapter's
    /// `runtime_context.*` codes), so log fields stay what they were before
    /// the call went through the trait.
    #[must_use]
    pub const fn with_reason(mut self, reason: &'static str) -> Self {
        self.reason = Some(reason);
        self
    }

    #[must_use]
    pub const fn code(&self) -> HostErrorCode {
        self.code
    }

    /// The host's reason code when it gave one, else the [`HostErrorCode`]'s.
    #[must_use]
    pub const fn reason_code(&self) -> &'static str {
        match self.reason {
            Some(reason) => reason,
            None => self.code.as_str(),
        }
    }
}

impl fmt::Debug for HostError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("HostError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for HostError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for HostError {}

// ---------------------------------------------------------------------------
// DefinitionSource
// ---------------------------------------------------------------------------

/// One nested application version, resolved for execution.
#[derive(Clone, Debug, PartialEq)]
pub struct ResolvedApplicationVersion {
    pub application_id: u64,
    pub version_id: u64,
    /// The platform's opaque definition identity, when it states one. Never
    /// recomputed from the (redeemed) settings below.
    pub definition_digest: Option<[u8; 32]>,
    /// The version document with every credential replaced by an opaque
    /// toolkit reference.
    pub version_details: Map<String, Value>,
}

/// The extracted text of one attachment of the turn.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AttachmentText {
    pub bucket: String,
    pub name: String,
    pub content: String,
}

/// Where the runtime reads definitions it did not receive with the turn.
///
/// Replaces, in the worker, `transport::platform_client::PlatformClient`'s
/// `resolve_application_version` and `read_attachment_object`, each called
/// with a `ClaimBoundRuntimeContextAuthority` over the private runtime-context
/// gRPC routes. The cloud adapter holds the authority; the trait does not.
/// Desktop: the resolved-definition operation of decision 5a over HTTPS.
///
/// The turn's own input bundle is not here: it arrives with the turn
/// (`agents::protocol::parse_agent_execution_input` in the worker, the
/// decision 5c `start` response on the desktop).
#[async_trait]
pub trait DefinitionSource: Send + Sync {
    /// Resolve one exact nested application version. A stale reference is
    /// [`HostErrorCode::NotFound`], terminal for the turn.
    async fn application_version(
        &self,
        application_id: u64,
        version_id: u64,
    ) -> Result<ResolvedApplicationVersion, HostError>;

    /// Read one attachment of this turn. Callers treat every failure as
    /// "unreadable" and continue; it never fails the turn.
    async fn attachment(&self, bucket: &str, name: &str) -> Result<AttachmentText, HostError>;
}

// ---------------------------------------------------------------------------
// ModelTransport
// ---------------------------------------------------------------------------

/// Reasoning control preserved from the frozen agent settings.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReasoningEffort {
    Low,
    Medium,
    High,
    None,
}

/// Frozen generation controls for one bound model (the worker's
/// `ModelFacadeInvocation`, minus the claim credential and the request
/// context budget, which joins when `context_budget` moves).
#[derive(Clone, Debug, PartialEq)]
pub struct ModelRequest {
    /// The project whose model configuration and budget the call uses.
    pub model_project_id: u32,
    pub model_name: String,
    pub system_instruction: String,
    pub max_tokens: Option<u32>,
    pub temperature: Option<f32>,
    pub reasoning_effort: Option<ReasoningEffort>,
    pub response_schema: Option<Value>,
    /// Pipeline output is validated after joining bounded continuations.
    pub allow_text_continuation: bool,
    pub max_model_turns: u32,
}

/// One bound model for one agent run. Mirrors the worker's
/// `agents::session::BoundOrdinaryAgentModel`.
pub trait BoundModel: Send {
    /// The adk model the agent loop calls.
    fn adk_model(&self) -> Arc<dyn Llm>;

    /// A separate summarisation binding for context compaction, if any.
    fn summarization_model(&self) -> Option<Arc<dyn Llm>> {
        None
    }

    /// The exact completed answer text, once the run has finished.
    ///
    /// # Errors
    ///
    /// When no complete answer was produced.
    fn take_completed_text(self: Box<Self>) -> Result<String, HostError>;
}

/// How the runtime reaches models: always the platform's `/llm` gateway.
///
/// Replaces `transport::model_facade::ModelFacade::bind`/`bind_with_summary`
/// with a `ClaimScopedEliteaContext` (the redeemed claim credential) over the
/// mTLS facade. Desktop: HTTPS to `/llm` with the native token and the local
/// turn's `X-Elitea-Execution-Id`.
pub trait ModelTransport: Send + Sync {
    /// Bind one model for one run.
    ///
    /// # Errors
    ///
    /// [`HostErrorCode::InvalidInput`] for a request the gateway contract
    /// refuses; [`HostErrorCode::DependencyUnavailable`] when unreachable.
    fn bind(&self, request: ModelRequest) -> Result<Box<dyn BoundModel>, HostError>;
}

// ---------------------------------------------------------------------------
// ToolProvider
// ---------------------------------------------------------------------------

/// The tools one agent or pipeline node is configured with.
#[derive(Clone, Debug, PartialEq)]
pub struct ToolsetRequest {
    /// The frozen toolkit references from the definition (the SDK `tools`
    /// array), credentials already replaced by opaque references.
    pub toolkits: Vec<Value>,
    /// Bounded label for errors and traces (`agent`, `pipeline node <id>`).
    pub context_label: String,
}

/// Where tools come from.
///
/// Replaces `toolkits::materialize_configured_toolsets_with_*` and
/// `toolkits::materialize_mcp_toolsets_with_tokens_and_authorization` (native
/// families with credentials materialised at claim, Streamable HTTP MCP with
/// claim-fetched tokens). Desktop: local tools (decision 4) plus
/// `RemoteToolkit` tools that call the decision 5b route; no credential ever
/// reaches it. The runtime then binds the flat namespace itself
/// (`toolkits::bind_toolsets`).
#[async_trait]
pub trait ToolProvider: Send + Sync {
    /// Materialise the toolsets for one request.
    async fn toolsets(&self, request: &ToolsetRequest) -> Result<Vec<Arc<dyn Toolset>>, HostError>;
}

// ---------------------------------------------------------------------------
// EventSink
// ---------------------------------------------------------------------------

/// How one run ended, as the sink records it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum RunOutcome {
    Completed {
        answer: String,
    },
    /// Paused for an approval or an authorisation (see [`ApprovalChannel`]).
    Paused,
    Failed {
        code: &'static str,
    },
    Stopped,
}

/// Where the run's events go.
///
/// Replaces the worker's `agents::events::AgentEventProjector` →
/// `ProjectedAgentEventBatch` (`NodeEventV1`) → `transport::output_grpc`
/// path. The projection to the wire is the cloud host's; the runtime hands
/// over adk events in order. Desktop: the UI channel, and the steps the
/// decision 5c `commit` records.
#[async_trait]
pub trait EventSink: Send + Sync {
    /// One adk event, in stream order. A failure stops the run.
    async fn emit(&self, event: &Event) -> Result<(), HostError>;

    /// The run's terminal outcome, exactly once.
    async fn finish(&self, outcome: RunOutcome) -> Result<(), HostError>;
}

// ---------------------------------------------------------------------------
// StateStore
// ---------------------------------------------------------------------------

/// Durable sessions and graph checkpoints.
///
/// Replaces `state::PostgresSessionService` and `state::PostgresCheckpointer`
/// (the agent-state database, every write fenced by a `StateWriterLease`).
/// Desktop: SQLite. The runtime's own wrappers (history projection,
/// receipt and activation checkpointers) layer on top of what this returns.
pub trait StateStore: Send + Sync {
    /// The adk session service for agent turns.
    fn sessions(&self) -> Arc<dyn SessionService>;

    /// The checkpointer for one pipeline thread family.
    fn checkpointer(&self) -> Arc<dyn Checkpointer>;
}

// ---------------------------------------------------------------------------
// MemoryStore
// ---------------------------------------------------------------------------

/// One personal memory of the turn's user in the turn's project.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MemoryEntry {
    pub id: u64,
    pub content: String,
}

/// The recall Main computed for this turn's input.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MemoryRecall {
    /// Text the runtime appends to the instructions, verbatim.
    pub instruction_text: String,
    /// The entries used, reported as `memories_used`.
    pub used: Vec<u64>,
}

/// Personal memory, owned by Main: the only memory store (decision 8).
///
/// No worker code exists yet: today Main recalls at admission
/// (`ResolveCurrentMemoryRecall`) and the agent-facing tools are worker
/// gate 7b. Cloud: a new runtime-context memory route. Desktop: the public
/// `/memories/prompt_lib/{project}` routes, recall from decision 5c `start`.
/// Neither host caches recall between turns.
#[async_trait]
pub trait MemoryStore: Send + Sync {
    /// The recall for this turn, computed by Main for `input`.
    async fn recall(&self, input: &str) -> Result<MemoryRecall, HostError>;

    /// Save one memory; policy (`local_work.memory_write`) may refuse it.
    async fn save(&self, content: &str) -> Result<MemoryEntry, HostError>;

    /// Search the user's memories in the project.
    async fn search(&self, query: &str, limit: u32) -> Result<Vec<MemoryEntry>, HostError>;

    /// Delete one of the user's memories.
    async fn delete(&self, id: u64) -> Result<(), HostError>;
}

// ---------------------------------------------------------------------------
// ApprovalChannel
// ---------------------------------------------------------------------------

/// What the runtime asks a person to decide.
#[derive(Clone, Debug, PartialEq)]
pub struct ApprovalRequest {
    /// The HITL node id or the tool call id.
    pub subject: String,
    pub message: String,
    /// The actions offered (`approve`, `reject`, `edit`, …).
    pub available_actions: Vec<String>,
    /// The interrupt payload (`graph::hitl` interrupt data, a tool call).
    pub payload: Value,
}

/// The answer to an [`ApprovalRequest`].
#[derive(Clone, Debug, PartialEq)]
pub enum ApprovalOutcome {
    /// Decided now (desktop: the person answered the prompt or a rule did).
    Decided { action: String, value: Value },
    /// The run must pause; the decision arrives with a later resume (cloud:
    /// the turn settles with an interrupt frame and a new claim resumes it
    /// with `HITL_RESUME_STATE_KEY`).
    Deferred,
}

/// How the runtime asks for approval.
///
/// Replaces the worker's HITL interrupt frames (`agents::direct_hitl`,
/// `graph::hitl` interrupt data, `graph::static_pause`), which always pause
/// and resume under a new claim. Desktop: the approval UI, which can answer
/// inline, and the policy/workspace/remembered rules of decision 4.
#[async_trait]
pub trait ApprovalChannel: Send + Sync {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError>;
}

// ---------------------------------------------------------------------------
// PlatformWriter
// ---------------------------------------------------------------------------

/// Where the runtime writes to the platform on the turn's behalf: the
/// `artifact` toolkit's bucket operations and the two builder tools.
///
/// Replaces, in the worker, `transport::platform_client::PlatformClient`'s
/// `list/read/write/delete_artifact`, `write_skill` and
/// `write_project_context`, each called with the turn's
/// `ClaimBoundRuntimeContextAuthority` over the private runtime-context
/// routes; the cloud adapter (`transport::platform_writer::ClaimPlatformWriter`)
/// holds both. Desktop: the same operations over the public API with the
/// native token, scoped to the turn's project.
///
/// Every target is inside the turn's own project; no request names a
/// project. A refused document is [`HostErrorCode::InvalidInput`] (main's
/// 422), a refused bucket [`HostErrorCode::AuthorizationFailed`], a missing
/// file [`HostErrorCode::NotFound`]; callers turn each into a model-visible
/// answer rather than failing the turn.
#[async_trait]
pub trait PlatformWriter: Send + Sync {
    async fn list_artifacts(
        &self,
        request: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError>;

    async fn read_artifact(
        &self,
        request: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError>;

    async fn write_artifact(
        &self,
        request: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError>;

    async fn delete_artifact(
        &self,
        request: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError>;

    async fn write_skill(
        &self,
        request: &SkillWriteRequest,
    ) -> Result<SkillWriteOutcome, HostError>;

    async fn write_project_context(
        &self,
        request: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError>;
}

// ---------------------------------------------------------------------------
// CodeSandbox
// ---------------------------------------------------------------------------

/// A language the code node runs. Mirrors the worker's
/// `sandbox::request::Language`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CodeLanguage {
    Python,
    JavaScript,
    TypeScript,
    Rust,
}

/// One admitted code job. The runtime chooses none of the image, policy or
/// limits beyond the timeout: those belong to the host's deployment. Not
/// `Debug`: source and input may hold private user data.
pub struct CodeJob {
    /// Stable identity of this activation; a resubmission with the same id
    /// is the same job (the host reconciles it, never reruns it blindly).
    pub job_id: String,
    pub language: CodeLanguage,
    pub source: String,
    pub input: BTreeMap<String, Value>,
    pub timeout: Duration,
}

/// What became of a [`CodeJob`]. Output is untrusted: the graph applies its
/// typed state projection to it. Mirrors `sandbox::client::SandboxOutcome`.
///
/// `Debug` is written by hand: a completed job's output is the user's code's
/// output (it may hold private data), so it prints only its byte length.
#[derive(Clone, Eq, PartialEq)]
pub enum CodeOutcome {
    Pending,
    Completed(Vec<u8>),
    Failed {
        code: String,
    },
    Cancelled,
    /// Completion cannot be confirmed either way; the caller reconciles the
    /// same `job_id` before anything else runs.
    Uncertain {
        code: String,
    },
}

impl fmt::Debug for CodeOutcome {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Pending => formatter.write_str("Pending"),
            Self::Completed(output) => formatter
                .debug_struct("Completed")
                .field("bytes", &output.len())
                .finish(),
            Self::Failed { code } => formatter
                .debug_struct("Failed")
                .field("code", code)
                .finish(),
            Self::Cancelled => formatter.write_str("Cancelled"),
            Self::Uncertain { code } => formatter
                .debug_struct("Uncertain")
                .field("code", code)
                .finish(),
        }
    }
}

/// How code nodes run code.
///
/// Replaces `sandbox::client::SandboxClient` (`submit`,
/// `submit_with_dependencies`, `cancel`, each authorised by main through
/// `ControlGrpcClient::authorize_sandbox_job` for the claim) as the
/// `graph::code*` nodes use it. No moved module calls it yet: the code nodes
/// and the cloud adapter move together in stage 6 of `EXTRACTION.md`, and
/// only then does [`Host`] carry one (until there is a reader, a host would
/// have to stub it).
/// Desktop: the local sandbox of ADR-0029 decision 4.
#[async_trait]
pub trait CodeSandbox: Send + Sync {
    /// Submit (or reconcile) one job and report its state.
    async fn submit(&self, job: &CodeJob) -> Result<CodeOutcome, HostError>;

    /// Ask the host to stop one job; never runs code.
    async fn cancel(&self, job_id: &str) -> Result<(), HostError>;
}

// ---------------------------------------------------------------------------
// ExecutionGuard
// ---------------------------------------------------------------------------

/// Why an execution may no longer proceed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ExecutionEnd {
    /// A person (or the platform) asked it to stop: cancel cooperatively.
    Stopped,
    /// Its authority is gone (lease lost, monitor closed): fail closed and
    /// write nothing more.
    Lost,
}

/// The execution's right to continue: lease, fence and stop in one.
///
/// Replaces `state::StateWriterLease::ensure_current` (checked before every
/// durable state write) and `execution::agent_lease::ClaimLeaseStateProbe`
/// (`ensure_running`, `wait_for_change`, which the native lifecycle races
/// against the run to cancel it on durable Stop). Desktop: a local
/// cancellation token; `Lost` never happens there.
#[async_trait]
pub trait ExecutionGuard: Send + Sync {
    /// Whether work (above all, a durable write) may proceed now.
    ///
    /// # Errors
    ///
    /// The end that has already happened.
    fn ensure_current(&self) -> Result<(), ExecutionEnd>;

    /// Resolve when the execution ends. Cancellation safe.
    async fn ended(&mut self) -> ExecutionEnd;
}

/// A local [`ExecutionGuard`]: stopped by its [`LocalStop`] handle, never
/// lost. The desktop host's guard and the one tests use.
pub struct LocalExecutionGuard {
    stopped: tokio::sync::watch::Receiver<bool>,
}

/// Stops the [`LocalExecutionGuard`] it was created with. Dropping it does
/// not stop the execution.
#[derive(Clone)]
pub struct LocalStop {
    stop: Arc<tokio::sync::watch::Sender<bool>>,
}

impl LocalStop {
    pub fn stop(&self) {
        self.stop.send_replace(true);
    }
}

impl LocalExecutionGuard {
    #[must_use]
    pub fn new() -> (Self, LocalStop) {
        let (stop, stopped) = tokio::sync::watch::channel(false);
        (
            Self { stopped },
            LocalStop {
                stop: Arc::new(stop),
            },
        )
    }
}

#[async_trait]
impl ExecutionGuard for LocalExecutionGuard {
    fn ensure_current(&self) -> Result<(), ExecutionEnd> {
        if *self.stopped.borrow() {
            Err(ExecutionEnd::Stopped)
        } else {
            Ok(())
        }
    }

    async fn ended(&mut self) -> ExecutionEnd {
        // A closed channel cannot happen while `LocalStop` keeps the sender
        // alive in an `Arc`; if every handle is gone, nothing can stop it.
        if self.stopped.wait_for(|stopped| *stopped).await.is_err() {
            std::future::pending::<()>().await;
        }
        ExecutionEnd::Stopped
    }
}

/// The shared capabilities one runtime invocation is given. The
/// [`ExecutionGuard`] is not here: it is owned by the one invocation it
/// guards (`ended` takes `&mut self`) and is passed alongside.
#[derive(Clone)]
pub struct Host {
    pub definitions: Arc<dyn DefinitionSource>,
    pub models: Arc<dyn ModelTransport>,
    pub tools: Arc<dyn ToolProvider>,
    pub events: Arc<dyn EventSink>,
    pub state: Arc<dyn StateStore>,
    pub memory: Arc<dyn MemoryStore>,
    pub approvals: Arc<dyn ApprovalChannel>,
    pub platform: Arc<dyn PlatformWriter>,
    // `code: Arc<dyn CodeSandbox>` joins in stage 6, with its first reader.
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::{
        CodeOutcome, ExecutionEnd, ExecutionGuard, HostError, HostErrorCode, LocalExecutionGuard,
    };

    #[tokio::test]
    async fn a_local_guard_ends_only_when_stopped() {
        let (mut guard, stop) = LocalExecutionGuard::new();
        assert_eq!(guard.ensure_current(), Ok(()));
        assert!(
            tokio::time::timeout(Duration::from_millis(20), guard.ended())
                .await
                .is_err(),
            "a running execution has not ended"
        );
        stop.clone().stop();
        assert_eq!(guard.ended().await, ExecutionEnd::Stopped);
        assert_eq!(guard.ensure_current(), Err(ExecutionEnd::Stopped));
        // The trait is object safe: hosts pass guards as `Box<dyn …>`.
        let mut boxed: Box<dyn ExecutionGuard> = Box::new(guard);
        assert_eq!(boxed.ended().await, ExecutionEnd::Stopped);
    }

    #[test]
    fn host_errors_are_data_free_and_only_unavailability_retries() {
        let error = HostError::new(HostErrorCode::NotFound, "the version is gone");
        assert_eq!(error.to_string(), "the version is gone");
        assert_eq!(format!("{error:?}"), "HostError { code: NotFound, .. }");
        assert!(!HostErrorCode::NotFound.retryable());
        assert!(HostErrorCode::DependencyUnavailable.retryable());
        assert_eq!(
            HostErrorCode::ExecutionEnded.as_str(),
            "runtime_host.execution_ended"
        );
    }

    /// A completed job's output is the user's code's output: `Debug` shows
    /// its length, never its bytes.
    #[test]
    fn code_outcomes_debug_without_output() {
        let completed = CodeOutcome::Completed(b"secret-token".to_vec());
        assert_eq!(format!("{completed:?}"), "Completed { bytes: 12 }");
        assert_eq!(
            format!(
                "{:?}",
                CodeOutcome::Failed {
                    code: "sandbox.timeout".into()
                }
            ),
            "Failed { code: \"sandbox.timeout\" }"
        );
        assert_eq!(format!("{:?}", CodeOutcome::Pending), "Pending");
    }
}
