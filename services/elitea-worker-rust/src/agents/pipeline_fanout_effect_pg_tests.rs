//! D2 precondition (T-2A §7): an effectful Application (saved Agent) child of a
//! fixed Parallel writes the "may have started" marker before its tool runs, and
//! a replacement Worker never dispatches that tool again.
//!
//! Real `PostgreSQL`, one OS process per Worker (as `pipeline_restart_pg_tests.rs`),
//! production `PipelineNativeAgentAssembler::postgres` with the test-only
//! fan-out composition (`with_fixed_parallel_for_tests`: scoped Agent
//! participants, `FanoutTurnAuthority`, `ParallelCompilerBinding::for_tests`).
//!
//! `gather` runs three branches of the same saved Agent, one at a time in
//! declared order:
//! * `first`: the Agent answers in text and completes;
//! * `second`: the Agent calls `lookup_release`, an MCP tool without a read-only
//!   hint (effectful). The tool checks, from inside its own execution, that
//!   `PostgreSQL` already holds the child's `tool_may_have_started` boundary
//!   written by this claim, then kills the process;
//! * `third`: never started before the crash.
//!
//! A replacement Worker reclaims the same execution with a later claim. The
//! completed branch is replayed from its receipt, `second` fails typed
//! (`graph.fanout.may_have_started`) with zero tool calls and zero model calls,
//! and `third` runs. On Error `continue` lands a failed join entry; `stop` fails
//! the node.
//!
//! The D2 narrowing (T-2A §8) on the same harness: when `second` dies in its
//! model call it resumes from its own model checkpoint (R), and when it dies
//! in a read-only tool that tool runs again (I); both complete all three.
//!
//! The same three cases run with inline LLM branches (T-2A D1): each branch is
//! an `llm` node offered the same MCP tool, with its own model checkpoint.

use super::restart_pg_tests::{CHECKPOINT_MIGRATION, connect, model_requests, run_phase};
use super::*;
use crate::protocol::control::test_session_authority_for_reclaim;
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use crate::state::{CheckpointLimits, SessionLimits, TestStateWriterLease};

const CHILD_TEST: &str =
    "agents::pipeline_tests::fanout_effect_pg_tests::postgres_fanout_effect_child";
const DATABASE_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_DATABASE";
const ON_ERROR_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_ON_ERROR";
const PHASE_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_PHASE";
/// `effect` (effectful tool in flight), `read_only` (I) or `mid_model` (R).
const MODE_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_MODE";
/// `agent` (saved Agent branches) or `llm` (inline LLM branches).
const KIND_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_KIND";
const EXECUTION: &str = "0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e";
const GENERATION: u64 = 3;
/// The tool saw its own boundary in `PostgreSQL`, then the process died.
const CRASH_EXIT_CODE: i32 = 87;
/// The tool started without a durable boundary: the precondition fails.
const MARKER_MISSING_EXIT_CODE: i32 = 88;
const MAY_HAVE_STARTED: &str = "graph.fanout.may_have_started";

fn pipeline_yaml(on_error: &str) -> String {
    let branch = |id: &str, release: &str| {
        format!(
            "  - id: {id}_call\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {{type: fixed, value: 'Summarize release {release}'}}\n    output: [{id}]\n"
        )
    };
    format!(
        "state:\n  first: str\n  second: str\n  third: str\n  joined: list\n  messages: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{{id: first, node: first_call}}, {{id: second, node: second_call}}, {{id: third, node: third_call}}]\n    max_concurrency: 1\n    wait: all\n    on_error: {on_error}\n    output: [joined]\n    transition: END\n{}{}{}",
        branch("first", "1.0"),
        branch("second", "1.1"),
        branch("third", "1.2"),
    )
}

fn request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    request_for(on_error, "agent")
}

/// The same three branches as inline LLM nodes offered the same MCP tool.
fn inline_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    let yaml = pipeline_yaml(on_error)
        .replace(
            "    type: agent\n    tool: release-agent\n",
            "    type: llm\n    tool_names:\n      release intelligence: [\"lookup_release\"]\n",
        )
        .replace(
            "    input_mapping:\n      task:",
            "    input_mapping:\n      system: {type: fixed, value: 'INLINE-BRANCH-SYSTEM'}\n      task:",
        );
    assert!(!yaml.contains("type: agent"), "{yaml}");
    assert!(yaml.contains("INLINE-BRANCH-SYSTEM"), "{yaml}");
    request.payload.application["version_details"]["instructions"] = json!(yaml);
    request
}

/// Every branch selects one saved participant of `agent_type`.
fn request_for(on_error: &str, agent_type: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = agent_pipeline_request("release-agent", agent_type);
    request.payload.application["version_details"]["instructions"] = json!(pipeline_yaml(on_error));
    request
}

/// One saved Agent with one MCP tool that publishes no read-only hint.
fn effectful_agent() -> Value {
    json!({
        "agent_type": "agent", "instructions": "Read the release evidence and summarize it.",
        "meta": {}, "variables": [],
        "tools": [{"id": 92, "type": "mcp", "toolkit_name": "release intelligence",
            "settings": {"url": "https://mcp.example.invalid/v1/mcp", "headers": null,
                "client_id": null, "client_secret": null, "scopes": null, "timeout": 30,
                "selected_tools": ["lookup_release"], "enable_caching": true, "cache_ttl": 300,
                "ssl_verify": true}}],
        "llm_settings": {"model_name": "child-model", "model_project_id": 23,
            "max_tokens": 2048, "reasoning_effort": null, "temperature": 0.2,
            "openai_compatible": true}
    })
}

/// The saved Agent, which may also ask the user (`ask_user`).
fn asking_agent() -> Value {
    let mut agent = effectful_agent();
    agent["meta"] = json!({"internal_tools": ["ask_user"]});
    agent
}

/// Child model-scope sessions (`elitea-model-*`) holding a durable
/// `tool_may_have_started` boundary written by `claim`.
async fn tool_boundaries(pool: &sqlx::PgPool, claim: &str) -> Vec<String> {
    phase_boundaries(pool, claim, "tool_may_have_started").await
}

/// Child model-scope sessions holding a durable checkpoint `phase` written by `claim`.
async fn phase_boundaries(pool: &sqlx::PgPool, claim: &str, phase: &str) -> Vec<String> {
    sqlx::query_scalar::<_, String>(
        "SELECT DISTINCT session_id FROM elitea_runtime.agent_session_events \
         WHERE session_id LIKE 'elitea-model-%' \
           AND event_payload LIKE '%' || $4 || '%' \
           AND writer_claim_id = $1 AND writer_execution_id = $2 AND writer_generation = $3",
    )
    .bind(claim)
    .bind(EXECUTION)
    .bind(i64::try_from(GENERATION).expect("generation"))
    .bind(phase)
    .fetch_all(pool)
    .await
    .expect("read child checkpoint boundaries")
}

/// The crash process's tool. Inside its own execution it proves the boundary
/// is already durable, then dies with the call in flight: an effectful tool
/// behind `tool_may_have_started`, a read-only one behind a replayable
/// `delegation_pending` batch.
struct CrashingMcpConnector {
    pool: sqlx::PgPool,
    read_only: bool,
}

#[async_trait]
impl McpConnector for CrashingMcpConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(CrashingMcpTool {
                inner: PipelineMcpTool {
                    calls: Arc::new(AtomicUsize::new(0)),
                    read_only: self.read_only,
                },
                pool: self.pool.clone(),
            })],
        )))
    }
}

struct CrashingMcpTool {
    inner: PipelineMcpTool,
    pool: sqlx::PgPool,
}

#[async_trait]
impl Tool for CrashingMcpTool {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn description(&self) -> &str {
        self.inner.description()
    }
    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }
    fn is_read_only(&self) -> bool {
        self.inner.is_read_only()
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        let effectful = tool_boundaries(&self.pool, "claim-1").await.len();
        let replayable = phase_boundaries(&self.pool, "claim-1", "delegation_pending")
            .await
            .len();
        let durable = if self.inner.is_read_only() {
            (effectful, replayable) == (0, 1)
        } else {
            effectful == 1
        };
        std::process::exit(if durable {
            CRASH_EXIT_CODE
        } else {
            MARKER_MISSING_EXIT_CODE
        })
    }
}

/// The platform answers by request path, not call order: each scoped Agent
/// node materializes its own participant (same shape as the 2a-1 fixture).
/// The one saved participant every branch selects.
struct SavedAgentVersions {
    details: Value,
    /// A second saved participant (application 5, version 6), when a test
    /// mixes two kinds of branch.
    second: Option<Value>,
    /// A third saved version (application 7, version 8): PM-2 depth 3.
    third: Option<Value>,
}

#[tonic::async_trait]
impl crate::transport::input_content::content_fake::ContextRoutes for SavedAgentVersions {
    async fn answer(
        &self,
        call: crate::transport::input_content::content_fake::ContextCall,
    ) -> Result<crate::transport::input_content::content_fake::ContextAnswer, tonic::Status> {
        let value = if call.route == "elitea-client-token" {
            json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,
                "token":"ephemeral-fanout-token"})
        } else if let Some(second) = self
            .second
            .as_ref()
            .filter(|_| call.route.ends_with("/5/versions/6"))
        {
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":5,"version_id":6,"version_details":second,
                "frozen_definition_sha256":"cd".repeat(32)})
        } else if let Some(third) = self
            .third
            .as_ref()
            .filter(|_| call.route.ends_with("/7/versions/8"))
        {
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":7,"version_id":8,"version_details":third,
                "frozen_definition_sha256":"ef".repeat(32)})
        } else if call.route.ends_with("/3/versions/4") {
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":3,"version_id":4,"version_details":self.details,
                // Main's authenticated digest of the frozen version: the saved
                // participant fingerprint a Parallel branch is bound to.
                "frozen_definition_sha256":"ab".repeat(32)})
        } else {
            return Err(tonic::Status::unavailable("fixture route"));
        };
        Ok(crate::transport::input_content::content_fake::ContextAnswer::from(&value))
    }
}

fn saved_runtime(
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    saved_runtime_with(outcomes, details, None)
}

fn saved_runtime_with(
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
    second: Option<Value>,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    saved_runtime_with3(outcomes, details, second, None)
}

fn saved_runtime_with3(
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
    second: Option<Value>,
    third: Option<Value>,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    saved_runtime_on(
        outcomes,
        details,
        second,
        third,
        test_model_gateway_config(),
    )
}

/// [`saved_runtime`] for a script holding gated replies (`Gated`,
/// `ResponseForRequestAfter`). A gated reply waits for the test to decide a
/// card from its frame, which on a loaded runner can take longer than the
/// fake's 1 s header deadline; that deadline would turn the test's own
/// reaction time into a failed branch, so the gated script is bounded by the
/// test instead (#1532).
fn gated_saved_runtime(
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    saved_runtime_on(outcomes, details, None, None, config)
}

fn saved_runtime_on(
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
    second: Option<Value>,
    third: Option<Value>,
    gateway_config: crate::transport::openai_compatible_facade::ModelGatewayConfig,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    let runtime = RuntimeContextClient::with_rpc(
        crate::transport::input_content::content_fake::Routes(SavedAgentVersions {
            details,
            second,
            third,
        }),
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 2 * 1_024 * 1_024,
        },
    )
    .expect("saved-version runtime-context fixture");
    let (gateway, captured) =
        test_model_gateway_client(outcomes, gateway_config).expect("scripted model gateway");
    (
        Arc::new(PlatformClient::new(Arc::new(runtime))),
        Arc::new(ModelFacade::from_gateway(gateway)),
        captured,
    )
}

/// One Worker process for `claim` on the same execution: a reclaim after a
/// crash has a later attempt, lease epoch and claim start.
fn assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    connector: Arc<dyn McpConnector>,
    details: Value,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    assembler_on(pool, saved_runtime(outcomes, details), connector)
}

/// [`assembler`] over a [`gated_saved_runtime`] script.
fn gated_assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    connector: Arc<dyn McpConnector>,
    details: Value,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    assembler_on(pool, gated_saved_runtime(outcomes, details), connector)
}

fn assembler_on(
    pool: &sqlx::PgPool,
    (platform, model_facade, captured): (
        Arc<PlatformClient>,
        Arc<ModelFacade>,
        CapturedModelRequests,
    ),
    connector: Arc<dyn McpConnector>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let assembler = PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
        platform,
        model_facade,
    )
    .with_mcp_connector(connector)
    .with_fixed_parallel_for_tests();
    (assembler, captured)
}

fn claim(
    request: &super::super::request::AgentExecutionRequest,
    attempt: u8,
) -> AuthorizedNativeAssembly<'_> {
    AuthorizedNativeAssembly::from_authorized(
        request,
        test_runtime_context_authority(),
        test_session_authority_for_reclaim(EXECUTION, GENERATION, attempt),
        Arc::new(TestStateWriterLease::current()),
        AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
    )
}

/// [`assembler`] serving a second saved version (application 5, version 6).
fn assembler_with(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
    second: Value,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let (platform, model_facade, captured) = saved_runtime_with(outcomes, details, Some(second));
    let assembler = PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
        platform,
        model_facade,
    )
    .with_mcp_connector(pipeline_connector())
    .with_fixed_parallel_for_tests();
    (assembler, captured)
}

/// [`assembler_with`] serving a third saved version (application 7, version 8).
fn assembler_with3(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    versions: [Value; 3],
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let [details, second, third] = versions;
    let (platform, model_facade, captured) =
        saved_runtime_with3(outcomes, details, Some(second), Some(third));
    let assembler = PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
        platform,
        model_facade,
    )
    .with_mcp_connector(pipeline_connector())
    .with_fixed_parallel_for_tests();
    (assembler, captured)
}

/// PM-2 depth 3: the second middle pipeline (application 5, version 6),
/// whose Agent node `deeper` runs application 7, version 8.
fn second_middle_pipeline() -> Value {
    let mut middle = middle_pipeline();
    middle["instructions"] = json!(
        middle["instructions"]
            .as_str()
            .expect("middle instructions")
            .replace("entry_point: inner", "entry_point: deeper")
            .replace("  - id: inner\n", "  - id: deeper\n")
            .replace("tool: inner-flow", "tool: deep-flow")
    );
    middle["tools"][0]["name"] = json!("deep-flow");
    middle["tools"][0]["toolkit_name"] = json!("deep-flow");
    middle["tools"][0]["settings"] = json!({"application_id": 7, "application_version_id": 8});
    middle
}

/// PM-2: the saved pipeline the root's Agent node runs, whose own Agent node
/// `inner` runs a second saved pipeline (application 5, version 6).
fn middle_pipeline() -> Value {
    json!({
        "agent_type": "pipeline",
        "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: inner\nnodes:\n  - id: inner\n    type: agent\n    tool: inner-flow\n    input_mapping:\n      task: {type: fixed, value: 'Pick the colours'}\n    output: [answer, messages]\n    transition: END\n",
        "meta": {}, "variables": [],
        "tools": [{
            "id": 45, "type": "application", "name": "inner-flow",
            "description": "Holds the Parallel.", "author_id": 11,
            "settings": {"application_id": 5, "application_version_id": 6},
            "meta": {}, "created_at": "2026-08-21T10:00:00Z", "toolkit_name": "inner-flow",
            "author": null, "agent_type": "pipeline", "online": null, "icon_meta": null,
            "variables": [], "is_pinned": false, "indexes_count": null
        }],
        "llm_settings": null
    })
}

/// A reclaim as production runs it with model-checkpoint recovery on: inspect
/// the root, authorize the evidence, assemble the checkpoint, and release the
/// Runner only for the authorized frontier.
async fn recovered(
    assembler: &PipelineNativeAgentAssembler,
    request: &super::super::request::AgentExecutionRequest,
    execution: &str,
    attempt: u8,
) -> Result<AssembledNativeAgentInvocation<PipelineAgentCompletion>, String> {
    let evidence = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
        assembler,
        request,
        &AuthorizedNativeCommandBinding::fixture_for_execution(execution),
        test_session_authority_for_reclaim(execution, GENERATION, attempt),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .map_err(|error| {
        format!(
            "inspect: {error} [{}]",
            error.cause().map_or(
                "",
                elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
            )
        )
    })?;
    let (authorizer, control) = crate::protocol::control::test_checkpoint_authorizer(
        execution,
        GENERATION,
        evidence.digest(),
    );
    let authorization = authorizer
        .authorize(&control, evidence)
        .await
        .map_err(|_| "authorize".to_owned())?;
    let pending = assembler
        .assemble_checkpoint(AuthorizedNativeAssembly::from_authorized(
            request,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(execution, GENERATION, attempt),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(execution),
        ))
        .await
        .map_err(|error| {
            format!(
                "assemble the checkpoint: {error} [{}]",
                error.cause().map_or(
                    "",
                    elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
                )
            )
        })?;
    let (_, _, _, _, permit) = authorization.into_lifecycle_parts();
    pending
        .authorize_lifecycle(permit)
        .map_err(|error| format!("authorize the lifecycle: {error}"))
}

/// Run the reclaimed pipeline to its end through the browser projector: the
/// `agent_fanout_member` statuses it emitted on completion, the failure's
/// code otherwise.
async fn run_to_end(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Result<Vec<Value>, String> {
    run_collecting(invocation).await.map(|(members, _)| members)
}

/// As [`run_to_end`], also returning every `interrupt_card_v1` raised.
async fn run_collecting(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Result<(Vec<Value>, Vec<Value>), String> {
    run_deciding(invocation, &|_| {}).await
}

/// As [`run_collecting`], calling `decide` with each card as its frame
/// arrives, while the run goes on (a user deciding live).
async fn run_deciding(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
    decide: &dyn Fn(&Value),
) -> Result<(Vec<Value>, Vec<Value>), String> {
    run_keyed_deciding(invocation, decide)
        .await
        .map(|(members, cards, _)| (members, cards))
}

/// As [`run_collecting`], also returning each frame's `parent_agent_call_id`
/// with its first tier's ordinal, in frame order (frames without one skipped).
async fn run_keyed(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Result<(Vec<Value>, Vec<Value>, Vec<(String, Value)>), String> {
    run_keyed_deciding(invocation, &|_| {}).await
}

/// As [`run_keyed`], calling `decide` with each card as its frame arrives.
async fn run_keyed_deciding(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
    decide: &dyn Fn(&Value),
) -> Result<(Vec<Value>, Vec<Value>, Vec<(String, Value)>), String> {
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut members = Vec::new();
    let mut cards = Vec::new();
    let mut keys = Vec::new();
    loop {
        match run.next_event().await {
            Err(error) => {
                return Err(format!(
                    "{error} {:?} {:?}",
                    error.upstream_code(),
                    error.upstream_message_for_tests()
                ));
            }
            Ok(Some(event)) => {
                for frame in projector
                    .project(&event)
                    .map_err(|error| error.to_string())?
                {
                    // One pause authority: a fan-out never raises a legacy card;
                    // its only pause terminal is the park (fanout_parked_v1).
                    let metadata: Value =
                        serde_json::from_slice(&frame.response_metadata).unwrap_or_default();
                    if let Some(key) = metadata["parent_agent_call_id"].as_str() {
                        keys.push((
                            key.to_owned(),
                            metadata["parent_agent_path"][0]["sibling_ordinal"].clone(),
                        ));
                    }
                    let legacy = frame.r#type == "mcp_authorization_required"
                        || (frame.r#type == "agent_hitl_interrupt"
                            && (metadata.get("fanout_parked_v1").is_none()
                                || metadata.get("hitl_interrupts").is_some()
                                || metadata.get("hitl_interrupt").is_some()));
                    assert!(
                        !legacy,
                        "a legacy pause frame inside a fan-out: {}",
                        frame.r#type
                    );
                    if frame.r#type == "agent_interrupt_pending" {
                        let metadata: Value = serde_json::from_slice(&frame.response_metadata)
                            .expect("card frame metadata");
                        decide(&metadata["interrupt_card_v1"]);
                        cards.push(metadata["interrupt_card_v1"].clone());
                    }
                    if frame.r#type == "agent_fanout_member" {
                        let metadata: Value = serde_json::from_slice(&frame.response_metadata)
                            .expect("member frame metadata");
                        members.push(metadata["fanout_member_status_v1"].clone());
                    }
                }
            }
            Ok(None) => break,
        }
    }
    completion
        .select()
        .await
        .map(|_| (members, cards, keys))
        .map_err(|error| error.to_string())
}

/// Run until the first `agent_interrupt_pending` frame is projected, then
/// drop the run there: the process is lost the moment Main could ingest the
/// card (PM-6). Returns that card.
async fn run_until_first_card(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Value {
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, mut projector, _completion) = invocation.start().expect("pipeline start");
    loop {
        let event = run
            .next_event()
            .await
            .expect("the run streams until its first card")
            .expect("a card before the run ends");
        for frame in projector.project(&event).expect("projected frames") {
            if frame.r#type == "agent_interrupt_pending" {
                let metadata: Value =
                    serde_json::from_slice(&frame.response_metadata).expect("card frame metadata");
                return metadata["interrupt_card_v1"].clone();
            }
        }
    }
}

/// Run until the first model request is in flight, then drop the run there:
/// the process is lost mid model call. Waits for the request itself, never a
/// fixed time, so a loaded machine cannot drop the run before its call.
async fn lose_at_first_model_request(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
    captured: &CapturedModelRequests,
) {
    let run = run_collecting(invocation);
    tokio::pin!(run);
    let deadline = tokio::time::Instant::now() + Duration::from_secs(30);
    loop {
        tokio::select! {
            outcome = &mut run => {
                panic!("the run ended before its model call was in flight: {outcome:?}");
            }
            () = tokio::time::sleep(Duration::from_millis(10)) => {
                if model_requests(captured) >= 1 {
                    return;
                }
                assert!(
                    tokio::time::Instant::now() < deadline,
                    "no model request within 30 s"
                );
            }
        }
    }
}

/// The latest root checkpoint's joined entries.
async fn joined(pool: &sqlx::PgPool) -> Option<Vec<Value>> {
    let state = sqlx::query_scalar::<_, String>(
        "SELECT state FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id NOT LIKE '%/%' AND state LIKE '%\"joined\"%' \
         ORDER BY save_ordinal DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await
    .expect("read the joined root checkpoint")?;
    let state: Value = serde_json::from_str(&state).expect("root state JSON");
    state["joined"].as_array().cloned()
}

async fn scenario(url: &str, mode: &str, on_error: &str) {
    scenario_for(url, "agent", mode, on_error).await;
}

async fn scenario_for(url: &str, kind: &str, mode: &str, on_error: &str) {
    let db = IsolatedPostgres::create(url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    for (phase, expected) in [("crash", Some(CRASH_EXIT_CODE)), ("recover", Some(0))] {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(ON_ERROR_ENV, on_error)
            .env(MODE_ENV, mode)
            .env(KIND_ENV, kind)
            .env(PHASE_ENV, phase);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert_eq!(
            output.status.code(),
            expected,
            "{phase} process exit: {stdout} {}",
            String::from_utf8_lossy(&output.stderr)
        );
        if phase == "crash" {
            // (a) the boundary of exactly the effectful child, fenced to claim 1;
            // a read-only batch or a model call leaves a replayable one instead.
            assert_eq!(
                tool_boundaries(&db.pool, "claim-1").await.len(),
                usize::from(matches!(mode, "effect" | "approved_effect"))
            );
            let sessions = sqlx::query_scalar::<_, String>(
                "SELECT DISTINCT session_id FROM elitea_runtime.agent_session_events \
                 WHERE session_id LIKE 'elitea-model-%' AND writer_claim_id = 'claim-1'",
            )
            .fetch_all(&db.pool)
            .await
            .expect("read child model-scope sessions");
            // (c) two branches of the same saved Agent: distinct model-scope
            // sessions; an approved member's decided step adds its own.
            assert_eq!(
                sessions.len(),
                if mode == "approved_effect" { 3 } else { 2 },
                "first and second branch own distinct model-scope sessions: {sessions:?}"
            );
        } else {
            assert!(stdout.contains("1 passed"), "{phase}: {stdout}");
        }
    }
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_fanout_child_may_have_started_continues_with_a_failed_entry() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "effect", "continue").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_fanout_child_may_have_started_stops_the_node() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "effect", "stop").await;
}

/// PM-1: a saved Agent member's sensitive call is approved on its card and is
/// in flight when the process dies. The replacement Worker never dispatches it
/// again: the branch ends typed `graph.fanout.may_have_started` (C), the tool
/// ran once, and the other branches still run.
#[tokio::test(flavor = "current_thread")]
async fn postgres_an_approved_agent_member_call_in_flight_at_a_crash_ends_typed() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "approved_effect", "continue").await;
}

/// FU-246: two saved Agent members need a sensitive-tool approval; each raises
/// its own card, one is approved and one rejected live, only the approved call
/// runs, once, and the pipeline joins with no legacy pause frame.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_agent_branches_approve_and_reject_their_own_calls() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let tool_calls = Arc::new(AtomicUsize::new(0));
    // `third` answers only after both cards are decided, so the runner cannot
    // park on a final fetch that ran before this test saw the cards.
    let decided = Arc::new(tokio::sync::Notify::new());
    let decisions = AtomicUsize::new(0);
    let (platform, model_facade, captured) = gated_saved_runtime(
        vec![
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Gated {
                gate: Arc::clone(&decided),
                response: pipeline_text_response("done"),
            },
            TestModelGatewayOutcome::Response(pipeline_text_response("done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("done")),
        ],
        effectful_agent(),
    );
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        sensitive_pipeline_mcp_policy(),
        platform,
        model_facade,
    )
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: false,
    }))
    .with_fixed_parallel_for_tests()
    .with_fanout_decisions_for_tests(ledger.client(1));
    let request = request("continue");
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the fan-out pipeline");
    let (_, cards) = run_deciding(invocation, &|card| {
        let key = card["interrupt_key"].as_str().expect("card key");
        match (card["fanout_v1"]["member"].as_str(), card["kind"].as_str()) {
            (Some("first"), Some("tool_guard")) => ledger.decide(key, "approve", None),
            (Some("second"), Some("tool_guard")) => ledger.decide(key, "reject", None),
            other => panic!("unexpected card {other:?}"),
        }
        if decisions.fetch_add(1, Ordering::AcqRel) + 1 == 2 {
            decided.notify_one();
        }
    })
    .await
    .expect("the approved and rejected members resume and the pipeline completes");
    assert_eq!(cards.len(), 2, "{cards:?}");
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        1,
        "only the approved call runs, once"
    );
    assert_eq!(model_requests(&captured), 5);
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
    db.pool.close().await;
}

/// FU-246: a member whose saved Agent asks the user pauses with its own card
/// (one pause authority: no legacy pause frame), the card is answered live,
/// and only that member's Agent resumes with the answer; the siblings run and
/// the pipeline joins.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_agent_branch_asks_and_resumes_with_the_answer() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    // `third` answers only after the card is decided. Otherwise, under load,
    // `first` and `third` can finish before the card frame reaches this test:
    // the runner's final fetch finds no decision and parks, and the answer
    // is never delivered live.
    let decided = Arc::new(tokio::sync::Notify::new());
    let (assembler, captured) = gated_assembler(
        &db.pool,
        vec![
            TestModelGatewayOutcome::Response(pipeline_text_response("first done")),
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Gated {
                gate: Arc::clone(&decided),
                response: pipeline_text_response("third done"),
            },
            TestModelGatewayOutcome::Response(pipeline_text_response("second done")),
        ],
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_agent(),
    );
    let request = request("continue");
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the fan-out pipeline");
    let (members, cards) = run_deciding(invocation, &|card| {
        let key = card["interrupt_key"].as_str().expect("card key");
        match (card["fanout_v1"]["member"].as_str(), card["kind"].as_str()) {
            (Some("second"), Some("ask_user")) => {
                ledger.decide(key, "answer", Some("Staging"));
                decided.notify_one();
            }
            other => panic!("unexpected card {other:?}"),
        }
        decided.notify_one();
    })
    .await
    .expect("the answered member resumes and the pipeline completes");
    assert_eq!(cards.len(), 1, "{cards:?}");
    assert_eq!(model_requests(&captured), 4);
    // The resumed Agent sees the answer to its question.
    let resumed = {
        let last = captured.lock().expect("captured model requests");
        String::from_utf8_lossy(&last[3].body).into_owned()
    };
    assert!(resumed.contains("Staging"), "{resumed}");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "second" && member["status"] == "end"),
        "{members:?}"
    );
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
    db.pool.close().await;
}

/// PM-6: an Agent member's card becomes decidable when Main ingests its frame.
/// The process is lost right at that frame, before anything after it runs. A
/// reclaim of the same execution decides the card: the member's forwarded
/// confirmation was already durable in the root session, so the member resumes
/// with the answer. The order holds because the member's events reach the root
/// runner through one FIFO channel and each is appended to the root session
/// before it is yielded, so the confirmation is durable before the card frame.
#[tokio::test(flavor = "current_thread")]
async fn postgres_an_agent_member_lost_at_its_card_frame_is_decided_from_the_durable_confirmation()
{
    let Some((db, outcome, captured)) = lost_at_the_card_and_reclaimed(false).await else {
        return;
    };
    outcome.expect("the decided member resumes and the pipeline completes");
    let resumed = {
        let requests = captured.lock().expect("captured model requests");
        String::from_utf8_lossy(&requests[0].body).into_owned()
    };
    assert!(resumed.contains("Staging"), "{resumed}");
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
    db.pool.close().await;
}

/// PM-6 negative control: the same loss and decision, with the member's
/// forwarded confirmation removed from the root session before the reclaim.
/// The decision has no proof to bind to and is refused; the member's Agent
/// never resumes.
#[tokio::test(flavor = "current_thread")]
async fn postgres_an_agent_member_decision_without_its_forwarded_confirmation_is_refused() {
    let Some((db, outcome, captured)) = lost_at_the_card_and_reclaimed(true).await else {
        return;
    };
    let refused = match outcome {
        Err(error) => error,
        Ok(()) => format!("{:?}", joined(&db.pool).await),
    };
    assert!(
        refused.contains("graph.parallel.decision_refused"),
        "{refused}"
    );
    assert!(
        captured
            .lock()
            .expect("captured model requests")
            .iter()
            .all(|request| !String::from_utf8_lossy(&request.body).contains("Staging")),
        "the member's Agent never resumed with the answer"
    );
    db.pool.close().await;
}

/// PM-6: park at the second member's card, lose the process there, decide the
/// card, optionally delete the member's forwarded confirmation from the root
/// session, and reclaim. Returns the reclaim's outcome; `None` when no test
/// database is configured.
async fn lost_at_the_card_and_reclaimed(
    remove_confirmation: bool,
) -> Option<(IsolatedPostgres, Result<(), String>, CapturedModelRequests)> {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return None;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (first, _) = assembler(
        &db.pool,
        vec![
            TestModelGatewayOutcome::Response(pipeline_text_response("first done")),
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            // `third` may start before the run is dropped at the card: its
            // call stays in flight instead of failing the branch durably.
            TestModelGatewayOutcome::Pending,
        ],
        pipeline_connector(),
        asking_agent(),
    );
    let request = request("continue");
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the fan-out pipeline");
    let card = run_until_first_card(invocation).await;
    assert_eq!(card["fanout_v1"]["member"], "second", "{card:?}");
    assert_eq!(card["kind"], "ask_user", "{card:?}");
    ledger.decide(
        card["interrupt_key"].as_str().expect("card key"),
        "answer",
        Some("Staging"),
    );
    if remove_confirmation {
        // The member's confirmation as forwarded into the root session: its
        // `tool_confirmation` for the card's call, routed to the member.
        let call = card["display"]["tool_call_id"]
            .as_str()
            .expect("card tool call id");
        let removed = sqlx::query(
            "DELETE FROM elitea_runtime.agent_session_events \
             WHERE session_id NOT LIKE 'elitea-model-%' \
               AND event_payload LIKE '%\"tool_confirmation\"%' \
               AND event_payload LIKE '%\"functionCallId\":\"' || $1 || '\"%' \
               AND event_payload LIKE '%' || $2 || '%'",
        )
        .bind(call)
        .bind(crate::agents::events::DESCENDANT_CONTAINER_INVOCATION_KEY)
        .execute(&db.pool)
        .await
        .expect("remove the forwarded confirmation")
        .rows_affected();
        assert_ne!(
            removed, 0,
            "the root session held the forwarded confirmation"
        );
    }
    let (again, captured) = assembler(
        &db.pool,
        vec![
            TestModelGatewayOutcome::Response(pipeline_text_response("second done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("third done")),
        ],
        pipeline_connector(),
        asking_agent(),
    );
    let again = again.with_fanout_decisions_for_tests(ledger.client(2));
    let outcome = match recovered(&again, &request, EXECUTION, 2).await {
        Ok(invocation) => run_collecting(invocation).await.map(|_| ()),
        Err(error) => Err(error),
    };
    Some((db, outcome, captured))
}

/// Production composition: a pipeline with a fixed Parallel node needs the
/// claim's decision ledger; without it the run is refused typed before any
/// child starts (a card could never be decided).
#[tokio::test(flavor = "current_thread")]
async fn postgres_production_parallel_without_the_decision_ledger_is_refused_typed() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (platform, model_facade, captured) = saved_runtime(Vec::new(), effectful_agent());
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
        platform,
        model_facade,
    )
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::new(AtomicUsize::new(0)),
        read_only: false,
    }));
    let request = request("continue");
    let error = assembler
        .assemble(claim(&request, 1))
        .await
        .err()
        .expect("refused without the ledger");
    assert_eq!(
        error
            .cause()
            .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code),
        Some("graph.fanout.decisions_unavailable")
    );
    assert_eq!(model_requests(&captured), 0);
    db.pool.close().await;
}

/// Inline LLM branches (T-2A D1) over the root's MCP toolkit, which is
/// sensitive for `lookup_release`: `ask` asks the user, `guard` and `deny`
/// each call the tool.
fn inline_llm_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    inline_llm_request_with(on_error, 1)
}

/// [`inline_llm_request`] with `max_concurrency` branches running at once.
fn inline_llm_request_with(
    on_error: &str,
    max_concurrency: usize,
) -> super::super::request::AgentExecutionRequest {
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    request.payload.internal_tools = vec![ASK_USER_TOOL_NAME.to_owned()];
    let branch = |id: &str, tools: &str| {
        format!(
            "  - id: {id}\n    type: llm\n    input_mapping:\n      system: {{type: fixed, value: 'INLINE-BRANCH-SYSTEM {id}'}}\n      task: {{type: fixed, value: 'Work on {id}'}}\n    output: [{id}_out]\n{tools}"
        )
    };
    let yaml = format!(
        "state:\n  ask_out: str\n  guard_out: str\n  deny_out: str\n  joined: list\n  messages: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{{id: first, node: ask}}, {{id: second, node: guard}}, {{id: third, node: deny}}]\n    max_concurrency: {max_concurrency}\n    wait: all\n    on_error: {on_error}\n    output: [joined]\n    transition: END\n{}{}{}",
        branch("ask", "    tool_names:\n      ask_user: [\"ask_user\"]\n"),
        branch(
            "guard",
            "    tool_names:\n      release intelligence: [\"lookup_release\"]\n"
        ),
        branch(
            "deny",
            "    tool_names:\n      release intelligence: [\"lookup_release\"]\n"
        ),
    );
    request.payload.application["version_details"]["instructions"] = json!(yaml);
    request
}

/// PM-5: an inline LLM branch streams like an Agent member. Its model text
/// reaches the browser as model frames under its own member tier (named by
/// its node), never as a run-level frame and never ending the run.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branches_stream_their_text_under_their_member_tier() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (platform, model_facade, _) = saved_runtime(
        [
            "ask streamed answer",
            "guard streamed answer",
            "deny streamed answer",
        ]
        .into_iter()
        .map(|text| TestModelGatewayOutcome::Response(pipeline_text_response(text)))
        .collect(),
        effectful_agent(),
    );
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        sensitive_pipeline_mcp_policy(),
        platform,
        model_facade,
    )
    .with_mcp_connector(pipeline_connector())
    .with_fixed_parallel_for_tests()
    .with_fanout_decisions_for_tests(
        crate::agents::graph::fanout_test_ledger::TestLedger::new().client(1),
    );
    let request = inline_llm_request("continue");
    let mut invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .expect("assemble inline LLM branches");
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut streamed = std::collections::BTreeMap::<String, String>::new();
    while let Some(event) = run.next_event().await.expect("the run streams") {
        for frame in projector.project(&event).expect("projected frames") {
            if frame.r#type != "agent_llm_chunk" {
                continue;
            }
            let metadata: Value =
                serde_json::from_slice(&frame.response_metadata).unwrap_or_default();
            let content: Value = serde_json::from_slice(&frame.content).unwrap_or_default();
            let tier = metadata["parent_agent_path"][0]["name"]
                .as_str()
                .unwrap_or("ROOT")
                .to_owned();
            streamed
                .entry(tier)
                .or_default()
                .push_str(content.as_str().unwrap_or_default());
        }
    }
    completion.select().await.expect("the pipeline completes");
    for branch in ["ask", "guard", "deny"] {
        assert_eq!(
            streamed.get(branch).map(String::as_str),
            Some(format!("{branch} streamed answer").as_str()),
            "{streamed:?}"
        );
    }
    // The run's own text is only its result (the join), never a branch's
    // streamed text ahead of it.
    let root = streamed.get("ROOT").map_or("", String::as_str);
    assert!(
        root.starts_with("```json") && root.contains("\"branch_id\": \"first\""),
        "{streamed:?}"
    );
    db.pool.close().await;
}

/// T-2A D1 (live): inline LLM branches each pause with their own card (an
/// `ask_user` question and two sensitive-tool approvals), are answered,
/// approved and rejected while the run goes on, resume and join in the same
/// run; only the approved tool runs, once, and nothing raises a legacy pause.
/// Deterministic: `deny` asks only once `ask` and `guard` are decided, and the
/// resumed `ask` answers only once `deny` is decided, so a branch is always
/// running until every decision exists and the run never parks.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branches_pause_with_their_own_cards_and_resume_live() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let early_decided = Arc::new(tokio::sync::Notify::new());
    let deny_decided = Arc::new(tokio::sync::Notify::new());
    let early = AtomicUsize::new(0);
    let (platform, model_facade, captured) = gated_saved_runtime(
        vec![
            TestModelGatewayOutcome::ResponseForRequest {
                marker: "Work on ask",
                response: pipeline_ask_user_call_response(),
            },
            TestModelGatewayOutcome::ResponseForRequestAfter {
                marker: "Work on ask",
                gate: Arc::clone(&deny_decided),
                response: pipeline_text_response("resumed done"),
            },
            TestModelGatewayOutcome::ResponseForRequest {
                marker: "Work on guard",
                response: pipeline_mcp_tool_call_response(),
            },
            TestModelGatewayOutcome::ResponseForRequest {
                marker: "Work on guard",
                response: pipeline_text_response("resumed done"),
            },
            TestModelGatewayOutcome::ResponseForRequestAfter {
                marker: "Work on deny",
                gate: Arc::clone(&early_decided),
                response: pipeline_mcp_tool_call_response(),
            },
            TestModelGatewayOutcome::ResponseForRequest {
                marker: "Work on deny",
                response: pipeline_text_response("resumed done"),
            },
        ],
        effectful_agent(),
    );
    let invocation = inline_cards_assembler(&db.pool, platform, model_facade, &tool_calls)
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&inline_llm_request_with("continue", 3), 1))
        .await
        .expect("assemble inline LLM branches");
    let (members, cards) = run_deciding(invocation, &|card| {
        let key = card["interrupt_key"].as_str().expect("card key");
        match (card["fanout_v1"]["member"].as_str(), card["kind"].as_str()) {
            (Some("first"), Some("ask_user")) => ledger.decide(key, "answer", Some("Staging")),
            (Some("second"), Some("tool_guard")) => ledger.decide(key, "approve", None),
            (Some("third"), Some("tool_guard")) => {
                ledger.decide(key, "reject", None);
                deny_decided.notify_one();
                return;
            }
            other => panic!("unexpected card {other:?}"),
        }
        if early.fetch_add(1, Ordering::AcqRel) == 1 {
            early_decided.notify_one();
        }
    })
    .await
    .expect("the branches resume and the pipeline completes in this run");
    assert!(
        joined(&db.pool)
            .await
            .is_some_and(|entries| !entries.is_empty()),
        "the run joined live, without parking"
    );
    assert_inline_cards(&db, &members, &cards, &tool_calls, &captured).await;
    db.pool.close().await;
}

/// T-2A D1 (park, then wake): the same three cards, decided only after the
/// run parked on them. The wake (a new execution) resumes each branch once
/// with its decision and joins.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branches_park_their_cards_and_resume_on_the_wake() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let tool_calls = Arc::new(AtomicUsize::new(0));
    // Each branch gets its own replies, in its own order, whatever order the
    // branches' requests arrive in.
    let (platform, model_facade, captured) = saved_runtime(
        ["ask", "guard", "deny"]
            .into_iter()
            .flat_map(|branch| {
                let marker = match branch {
                    "ask" => "Work on ask",
                    "guard" => "Work on guard",
                    _ => "Work on deny",
                };
                let first = if branch == "ask" {
                    pipeline_ask_user_call_response()
                } else {
                    pipeline_mcp_tool_call_response()
                };
                [
                    TestModelGatewayOutcome::ResponseForRequest {
                        marker,
                        response: first,
                    },
                    TestModelGatewayOutcome::ResponseForRequest {
                        marker,
                        response: pipeline_text_response("resumed done"),
                    },
                ]
            })
            .collect(),
        effectful_agent(),
    );
    let request = inline_llm_request("continue");
    let invocation = inline_cards_assembler(
        &db.pool,
        Arc::clone(&platform),
        Arc::clone(&model_facade),
        &tool_calls,
    )
    .with_fanout_decisions_for_tests(ledger.client(1))
    .assemble(claim(&request, 1))
    .await
    .expect("assemble inline LLM branches");
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the undecided cards park the run");
    assert!(
        joined(&db.pool)
            .await
            .is_none_or(|entries| entries.is_empty()),
        "the run parked"
    );
    for card in &cards {
        let key = card["interrupt_key"].as_str().expect("card key");
        match card["fanout_v1"]["member"].as_str() {
            Some("first") => ledger.decide(key, "answer", Some("Staging")),
            Some("second") => ledger.decide(key, "approve", None),
            Some("third") => ledger.decide(key, "reject", None),
            other => panic!("unexpected card {other:?}"),
        }
    }
    let mut wake = request;
    wake.payload.user_input = super::super::request::UserInput::Text(String::new());
    wake.payload.meta.insert(
        crate::agents::graph::fanout_wake::FANOUT_WAKE_META_KEY.to_owned(),
        json!({"schema": "elitea.pipeline.fanout-wake.v1",
            "response_message_id": "10000000-0000-4000-8000-000000000043",
            "decision_revision": 1,
            "parked_execution_id": EXECUTION,
            "parked_generation": GENERATION}),
    );
    let invocation = inline_cards_assembler(&db.pool, platform, model_facade, &tool_calls)
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .expect("assemble the wake");
    run_collecting(invocation)
        .await
        .expect("the wake resumes every decided branch and joins");
    assert_inline_cards(&db, &members, &cards, &tool_calls, &captured).await;
    db.pool.close().await;
}

/// The inline-card scenario's assembler: the sensitive MCP tool counted in
/// `tool_calls`.
fn inline_cards_assembler(
    pool: &sqlx::PgPool,
    platform: Arc<PlatformClient>,
    model_facade: Arc<ModelFacade>,
    tool_calls: &Arc<AtomicUsize>,
) -> PipelineNativeAgentAssembler {
    PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        sensitive_pipeline_mcp_policy(),
        platform,
        model_facade,
    )
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(tool_calls),
        read_only: false,
    }))
    .with_fixed_parallel_for_tests()
}

/// What both inline-card tests prove once the run has joined: all three
/// cards with their member tiers, the approved tool once, six model requests
/// each carrying its branch's own instruction and tool, and three successes.
async fn assert_inline_cards(
    db: &IsolatedPostgres,
    members: &[Value],
    cards: &[Value],
    tool_calls: &AtomicUsize,
    captured: &CapturedModelRequests,
) {
    let kinds = cards
        .iter()
        .map(|card| {
            format!(
                "{}:{}",
                card["fanout_v1"]["member"].as_str().unwrap_or_default(),
                card["kind"].as_str().unwrap_or_default()
            )
        })
        .collect::<Vec<_>>();
    // All three cards, whatever order the branches raised them in.
    let mut kinds = kinds;
    kinds.sort();
    assert_eq!(
        kinds,
        ["first:ask_user", "second:tool_guard", "third:tool_guard"]
    );
    for card in cards {
        // The member tier of an LLM branch is named by its node (§3.3).
        let tier = &card["parent_agent_path"][0];
        assert!(["ask", "guard", "deny"].contains(&tier["name"].as_str().unwrap_or_default()));
    }
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        1,
        "only the approved tool runs, once"
    );
    assert_eq!(model_requests(captured), 6);
    // Browser finding 2026-10-28: an inline branch's model request carries its
    // own system instruction and offers its own bound tool.
    for request in captured.lock().expect("captured model requests").iter() {
        let body: Value = serde_json::from_slice(&request.body).expect("model request JSON");
        let rendered = body.to_string();
        let branch = ["ask", "guard", "deny"]
            .into_iter()
            .find(|branch| rendered.contains(&format!("Work on {branch}")))
            .unwrap_or_else(|| panic!("an unknown branch request: {rendered}"));
        assert_eq!(
            crate::test_content::without_preamble(
                body["messages"][0]["content"]
                    .as_str()
                    .expect("system content")
            ),
            format!("INLINE-BRANCH-SYSTEM {branch}"),
            "{rendered}"
        );
        let tool = if branch == "ask" {
            "ask_user"
        } else {
            "lookup_release"
        };
        assert!(
            body["tools"]
                .as_array()
                .is_some_and(
                    |tools| tools.iter().any(|offered| offered["function"]["name"]
                        .as_str()
                        .is_some_and(|name| name.contains(tool)))
                ),
            "{branch} is not offered {tool}: {rendered}"
        );
    }
    assert!(members.iter().any(|member| member["status"] == "paused"));
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
}

/// A saved pipeline participant whose LLM node asks the user.
fn asking_pipeline() -> Value {
    json!({
        "agent_type": "pipeline",
        "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    input_mapping:\n      task: {type: variable, value: input}\n    input: [messages]\n    output: [answer, messages]\n    tool_names:\n      ask_user: [\"ask_user\"]\n    transition: END\n",
        "meta": {"internal_tools": ["ask_user"]}, "variables": [], "tools": [],
        "llm_settings": null
    })
}

/// Commit 6: three saved-pipeline branches each ask the user. Every branch
/// raises one `ask_user` card frame under its own member tier, nothing raises a
/// legacy card, and with nothing decided the activation parks.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_pipeline_branches_that_ask_raise_one_card_each_and_park() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (assembler, captured) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_pipeline(),
    );
    let request = request_for("continue", "pipeline");
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| {
            panic!(
                "assemble the fan-out pipeline: {error} {:?}",
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
            )
        });
    let (members, cards, keys) = run_keyed(invocation).await.expect("the activation parks");
    assert_eq!(model_requests(&captured), 3);
    // One member, one key: its call, its descendants, its card and its
    // lifecycle all carry the member tier (`fo1_`, its ordinal), so the Web
    // renders one labelled accordion per member (browser finding 2026-10-27).
    let tiers = cards
        .iter()
        .map(|card| {
            card["parent_agent_path"][0]["call_id"]
                .as_str()
                .unwrap_or_default()
                .to_owned()
        })
        .collect::<Vec<_>>();
    assert_eq!(tiers.len(), 3);
    for (key, ordinal) in &keys {
        let position = tiers
            .iter()
            .position(|tier| tier == key)
            .unwrap_or_else(|| panic!("a frame keyed outside the member tiers: {key}"));
        if !ordinal.is_null() {
            assert_eq!(ordinal, &json!(position + 1), "{key}");
        }
    }
    assert!(keys.len() > 9, "{keys:?}");
    assert_eq!(cards.len(), 3, "{cards:?}");
    for (card, (ordinal, branch)) in cards
        .iter()
        .zip([(1, "first"), (2, "second"), (3, "third")])
    {
        assert_eq!(card["kind"], "ask_user");
        assert_eq!(card["available_actions"], json!(["answer"]));
        assert_eq!(card["display"]["guardrail_type"], "clarifying_question");
        assert_eq!(card["fanout_v1"]["member"], branch);
        assert_eq!(card["fanout_v1"]["ordinal"], ordinal);
        assert_eq!(card["parent_agent_path"][0]["sibling_ordinal"], ordinal);
    }
    let statuses = members
        .iter()
        .map(|member| member["status"].as_str().unwrap_or_default().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        statuses,
        ["start", "paused", "start", "paused", "start", "paused"]
    );
    db.pool.close().await;
}

/// A saved pipeline that answers without a model.
fn answering_pipeline() -> Value {
    json!({
        "agent_type": "pipeline",
        "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: state_modifier\n    template: 'answered'\n    input: [input]\n    output: [answer]\n    transition: END\n",
        "meta": {}, "variables": [], "tools": [], "llm_settings": null
    })
}

/// A saved pipeline whose direct MCP node calls a tool with external effects.
/// A saved child has no fenced node writer, so the node is refused at binding.
fn direct_effect_pipeline() -> Value {
    let mut pipeline = effectful_agent();
    pipeline["agent_type"] = json!("pipeline");
    pipeline["instructions"] = json!(
        "state:\n  input: str\n  messages: list\n  answer: dict\nentry_point: rev\nnodes:\n  - id: rev\n    type: mcp\n    toolkit_name: release intelligence\n    tool: lookup_release\n    input_mapping: {}\n    output: [answer]\n    transition: END\n"
    );
    pipeline
}

/// Branches `first` and `third` answer; `second` runs `direct_effect_pipeline`.
fn mixed_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = request_for(on_error, "pipeline");
    let yaml = pipeline_yaml(on_error).replacen(
        "  - id: second_call\n    type: agent\n    tool: release-agent\n",
        "  - id: second_call\n    type: agent\n    tool: direct-flow\n",
        1,
    );
    assert!(yaml.contains("tool: direct-flow"), "{yaml}");
    let version = &mut request.payload.application["version_details"];
    version["instructions"] = json!(yaml);
    let mut second = version["tools"][0].clone();
    second["id"] = json!(45);
    second["name"] = json!("direct-flow");
    second["toolkit_name"] = json!("direct-flow");
    second["settings"] = json!({"application_id": 5, "application_version_id": 6});
    version["tools"].as_array_mut().expect("tools").push(second);
    request
}

/// FU-248: a member whose saved child pipeline starts at an effectful direct
/// MCP node runs that call once, behind the run's journal scoped to the
/// child's visit, under either On Error (it used to be refused as
/// `graph.fanout.child_tool_unavailable`).
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_member_whose_child_calls_an_effectful_direct_tool_runs_it_once() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    for on_error in ["continue", "stop"] {
        let db = IsolatedPostgres::create(&url).await;
        install_schema(&db.pool).await;
        sqlx::raw_sql(CHECKPOINT_MIGRATION)
            .execute(&db.pool)
            .await
            .expect("apply checkpoint migration");
        let tool_calls = Arc::new(AtomicUsize::new(0));
        let (platform, model_facade, captured) = saved_runtime_with(
            Vec::new(),
            answering_pipeline(),
            Some(direct_effect_pipeline()),
        );
        let assembler = PipelineNativeAgentAssembler::postgres(
            db.pool.clone(),
            SessionLimits::default(),
            CheckpointLimits::default(),
            Arc::new(
                ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                    .expect("empty tool policy"),
            ),
            platform,
            model_facade,
        )
        .with_mcp_connector(Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::clone(&tool_calls),
            read_only: false,
        }))
        .with_fixed_parallel_for_tests();
        let request = mixed_request(on_error);
        let invocation = assembler
            .assemble(claim(&request, 1))
            .await
            .unwrap_or_else(|error| panic!("assemble the mixed fan-out: {error}"));
        let members = run_to_end(invocation)
            .await
            .unwrap_or_else(|error| panic!("{on_error}: the fan-out completes: {error}"));
        assert_eq!(
            tool_calls.load(Ordering::Acquire),
            1,
            "{on_error}: once: {:?} {members:?}",
            joined(&db.pool).await
        );
        assert_eq!(model_requests(&captured), 0, "{on_error}");
        assert!(
            members.iter().any(
                |member| member["fanout_v1"]["member"] == "second" && member["status"] == "end"
            ),
            "{members:?}"
        );
        let entries = joined(&db.pool).await.expect("joined entries");
        let statuses = entries
            .iter()
            .map(|entry| {
                format!(
                    "{}:{}",
                    entry["branch_id"].as_str().unwrap_or_default(),
                    entry["status"].as_str().unwrap_or_default()
                )
            })
            .collect::<Vec<_>>();
        assert_eq!(
            statuses,
            ["first:succeeded", "second:succeeded", "third:succeeded"],
            "{on_error}"
        );
        // One journal, under the member's child thread scope.
        assert_eq!(journal_threads(&db.pool).await.len(), 1, "{on_error}");
        db.pool.close().await;
    }
}

/// The wake of a parked fan-out of saved-pipeline branches (contract §8.1):
/// the parked fresh turn, an empty input and only the wake marker.
fn wake_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    wake_request_for(on_error, "pipeline")
}

/// [`wake_request`] for branches whose saved participant is `agent_type`.
fn wake_request_for(
    on_error: &str,
    agent_type: &str,
) -> super::super::request::AgentExecutionRequest {
    let mut request = request_for(on_error, agent_type);
    request.payload.user_input = super::super::request::UserInput::Text(String::new());
    request.payload.meta.insert(
        crate::agents::graph::fanout_wake::FANOUT_WAKE_META_KEY.to_owned(),
        json!({"schema": "elitea.pipeline.fanout-wake.v1",
            "response_message_id": "10000000-0000-4000-8000-000000000043",
            "decision_revision": 1,
            "parked_execution_id": EXECUTION,
            "parked_generation": GENERATION}),
    );
    request
}

/// FU-246: three saved Agent members ask the user and the activation parks;
/// one card is answered while parked; the wake (a new execution) resumes only
/// that member's Agent with the answer, the others park again with their
/// cards, and no legacy pause frame is raised.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_resumes_the_answered_agent_branch_and_parks_the_rest() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let connector = || {
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        })
    };
    let (first, _) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        connector(),
        asking_agent(),
    );
    let request = request("continue");
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the activation parks");
    assert_eq!(cards.len(), 3, "{cards:?}");
    let third = cards[2]["interrupt_key"]
        .as_str()
        .expect("the third card's key")
        .to_owned();
    ledger.decide(&third, "answer", Some("Staging"));

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "third done",
        ))],
        connector(),
        asking_agent(),
    );
    let wake = wake_request_for("continue", "agent");
    let invocation = woken
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the wake resumes the answered member and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered member calls the model"
    );
    let resumed = String::from_utf8_lossy(&captured.lock().expect("requests")[0].body).into_owned();
    assert!(resumed.contains("Staging"), "{resumed}");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    assert!(
        cards
            .iter()
            .filter(|card| card["interrupt_key"] != third.as_str())
            .count()
            >= 2,
        "{cards:?}"
    );
    db.pool.close().await;
}

/// Browser finding 2026-10-27: saved-pipeline branches that ask the user park,
/// one card is answered, and the wake resumes exactly that child from its own
/// pause, while its unanswered siblings park again with their cards.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_resumes_the_answered_pipeline_branch_and_parks_the_rest() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let connector = || {
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        })
    };
    let (first, _) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        connector(),
        asking_pipeline(),
    );
    let request = request_for("continue", "pipeline");
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the activation parks");
    assert_eq!(cards.len(), 3, "{cards:?}");
    let third = cards[2]["interrupt_key"]
        .as_str()
        .expect("the third card's key")
        .to_owned();
    ledger.decide(&third, "answer", Some("red"));

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "the saddle is red",
        ))],
        connector(),
        asking_pipeline(),
    );
    let wake = wake_request("continue");
    let invocation = woken
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the wake resumes the answered child and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered child calls the model"
    );
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    // The unanswered members are restored paused, unchanged: in the wake only
    // the answered member runs (its lifecycle is the only one emitted), and
    // their cards are re-announced on the restore, never decided.
    assert!(
        members
            .iter()
            .all(|status| status["fanout_v1"]["member"] == "third"),
        "{members:?}"
    );
    assert!(
        cards
            .iter()
            .filter(|card| card["interrupt_key"] != third.as_str())
            .count()
            >= 2,
        "{cards:?}"
    );
    db.pool.close().await;
}

/// FU-249: a saved child pipeline whose root is a fixed Parallel of three
/// inline LLM nodes that may ask the user.
fn parallel_child_pipeline() -> Value {
    let branch = |id: &str| {
        format!(
            "  - id: ask_{id}\n    type: llm\n    input_mapping:\n      task: {{type: fixed, value: 'Ask about {id}'}}\n    input: [messages]\n    output: [{id}]\n    tool_names:\n      ask_user: [\"ask_user\"]\n"
        )
    };
    json!({
        "agent_type": "pipeline",
        "instructions": format!(
            "state:\n  input: str\n  messages: list\n  a: str\n  b: str\n  c: str\n  joined: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{{id: a, node: ask_a}}, {{id: b, node: ask_b}}, {{id: c, node: ask_c}}]\n    max_concurrency: 1\n    wait: all\n    on_error: continue\n    output: [joined]\n    transition: END\n{}{}{}",
            branch("a"),
            branch("b"),
            branch("c"),
        ),
        "meta": {"internal_tools": ["ask_user"]}, "variables": [], "tools": [],
        "llm_settings": null
    })
}

/// FU-249: the root pipeline runs the saved child through its `delegate` Agent node.
fn child_parallel_request() -> super::super::request::AgentExecutionRequest {
    agent_pipeline_request("release-agent", "pipeline")
}

/// [`child_parallel_request`] as the wake of the parked run.
fn child_parallel_wake() -> super::super::request::AgentExecutionRequest {
    let mut request = child_parallel_request();
    request.payload.user_input = super::super::request::UserInput::Text(String::new());
    request.payload.meta.insert(
        crate::agents::graph::fanout_wake::FANOUT_WAKE_META_KEY.to_owned(),
        json!({"schema": "elitea.pipeline.fanout-wake.v1",
            "response_message_id": "10000000-0000-4000-8000-000000000043",
            "decision_revision": 1,
            "parked_execution_id": EXECUTION,
            "parked_generation": GENERATION}),
    );
    request
}

/// The child Parallel's rows on the child thread (`<root>/delegate`), oldest
/// first: whether each carries the Agent node's activation stamp.
async fn child_parallel_rows(pool: &sqlx::PgPool) -> Vec<bool> {
    sqlx::query_scalar::<_, bool>(
        "SELECT metadata::text LIKE '%elitea.pipeline.application-activation.v1%' \
         FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id LIKE '%/delegate' ORDER BY save_ordinal",
    )
    .fetch_all(pool)
    .await
    .expect("read the child thread rows")
}

/// The latest child-thread state's `joined` entries.
async fn child_joined(pool: &sqlx::PgPool) -> Option<Vec<Value>> {
    let state = sqlx::query_scalar::<_, String>(
        "SELECT state FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id LIKE '%/delegate' AND state LIKE '%\"joined\"%' \
         ORDER BY save_ordinal DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await
    .expect("read the joined child checkpoint")?;
    let state: Value = serde_json::from_str(&state).expect("child state JSON");
    state["joined"].as_array().cloned()
}

/// FU-249: a fixed Parallel inside a saved child pipeline (root pipeline →
/// Agent node → saved pipeline). Its three branches ask, raise one card each
/// and the run parks; every row the child's Parallel wrote on the child thread
/// carries the Agent node's activation stamp. All three are answered while
/// parked; the wake (a new execution) re-enters the child and the Parallel
/// resumes each branch with its answer, joins, and the root completes.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_parallel_in_a_saved_child_pipeline_parks_and_a_wake_completes_it() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (first, captured) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| {
            panic!(
                "assemble the child fan-out: {error} {:?}",
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
            )
        });
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the child's Parallel parks the run");
    assert_eq!(model_requests(&captured), 3);
    assert_eq!(cards.len(), 3, "{cards:?}");
    for (card, branch) in cards.iter().zip(["a", "b", "c"]) {
        assert_eq!(card["kind"], "ask_user");
        assert_eq!(card["fanout_v1"]["member"], branch);
    }
    let rows = child_parallel_rows(&db.pool).await;
    assert_ne!(rows.len(), 0);
    assert!(rows.iter().all(|stamped| *stamped), "{rows:?}");
    let parked_keys = cards
        .iter()
        .map(|card| {
            card["interrupt_key"]
                .as_str()
                .expect("a card key")
                .to_owned()
        })
        .collect::<Vec<_>>();
    for (key, answer) in parked_keys.iter().zip(["red", "green", "blue"]) {
        ledger.decide(key, "answer", Some(answer));
    }

    let (woken, captured) = assembler(
        &db.pool,
        [
            "the saddle is red",
            "the bridle is green",
            "the stirrup is blue",
        ]
        .into_iter()
        .map(|text| TestModelGatewayOutcome::Response(pipeline_text_response(text)))
        .collect(),
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let wake = child_parallel_wake();
    let invocation = woken
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the wake completes the child's Parallel and the root");
    // Every card was answered while parked: none is raised again.
    assert!(cards.is_empty(), "{cards:?}");
    assert_eq!(
        model_requests(&captured),
        3,
        "each answered branch calls the model once"
    );
    let joined = child_joined(&db.pool).await.expect("the child's join");
    assert_eq!(joined.len(), 3, "{joined:?}");
    let rows = child_parallel_rows(&db.pool).await;
    assert!(rows.iter().all(|stamped| *stamped), "{rows:?}");
    db.pool.close().await;
}

/// FU-249: the child pipeline's Parallel parked, only branch `a` answered; the
/// wake's process is lost while `a`'s resumed model call is in flight. The
/// reclaim (production checkpoint recovery under the wake's own execution)
/// re-enters the child at its parked frontier and resumes `a` from its model
/// checkpoint with the answer (R); `a` never asks again, `b` and `c` park again.
#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One scenario: park, answer, lose the wake, recover, assert in order.
async fn postgres_a_wake_lost_inside_a_child_pipeline_resumes_the_answered_branch() {
    const WAKE_EXECUTION: &str = "execution-child-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (first, _) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the child fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the child's Parallel parks the run");
    assert_eq!(cards.len(), 3, "{cards:?}");
    let parked_keys = cards
        .iter()
        .map(|card| {
            card["interrupt_key"]
                .as_str()
                .expect("a card key")
                .to_owned()
        })
        .collect::<Vec<_>>();
    let answered = parked_keys[0].clone();
    ledger.decide(&answered, "answer", Some("Staging"));

    // The wake's first claim applies the answer, then its process is lost
    // while branch `a`'s resumed model call is in flight.
    let (lost, lost_requests) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let wake = child_parallel_wake();
    let invocation = lost
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 2),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    // Dropped inside the gateway's 1 s header timeout: the process is gone.
    lose_at_first_model_request(invocation, &lost_requests).await;
    assert_eq!(
        model_requests(&lost_requests),
        1,
        "branch a's step was in flight"
    );

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "staging it is",
        ))],
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(3));
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 3)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake finishes branch a and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only branch a's step: {members:?}"
    );
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "a" && member["status"] == "end"),
        "{members:?}"
    );
    // The reclaim re-runs the wake from the park (class I, as a root wake
    // lost before its first root save): `a`'s decision is applied once from
    // its receipt (one model step, `a` ends), its answered card is never
    // raised again, and only `b` and `c` are re-raised as they park again.
    let keys = cards
        .iter()
        .filter_map(|card| card["interrupt_key"].as_str())
        .collect::<Vec<_>>();
    assert_eq!(
        keys,
        [parked_keys[1].as_str(), parked_keys[2].as_str()],
        "{cards:?}"
    );
    let rows = child_parallel_rows(&db.pool).await;
    assert!(rows.iter().all(|stamped| *stamped), "{rows:?}");
    db.pool.close().await;
}

/// FU-249 follow-up: the child pipeline's Parallel parked, only branch `a`
/// answered; in the wake `a`'s resumed model call fails. Under On Error
/// `continue` `a` ends failed, `b` and `c` are re-raised and park again: the
/// failed branch never ends the run or drops the other branches' cards.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_branch_failing_in_a_child_pipeline_wake_keeps_the_other_cards() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (first, _) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the child fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the child's Parallel parks the run");
    let parked_keys = cards
        .iter()
        .map(|card| {
            card["interrupt_key"]
                .as_str()
                .expect("a card key")
                .to_owned()
        })
        .collect::<Vec<_>>();
    assert_eq!(parked_keys.len(), 3, "{cards:?}");
    ledger.decide(&parked_keys[0], "answer", Some("Staging"));

    let (woken, _) = assembler(
        &db.pool,
        (0..4)
            .map(|_| TestModelGatewayOutcome::Unavailable)
            .collect(),
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let wake = child_parallel_wake();
    let invocation = woken
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("a failed branch parks the others again, never fails the run");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "a" && member["status"] == "failed"),
        "{members:?}"
    );
    let keys = cards
        .iter()
        .filter_map(|card| card["interrupt_key"].as_str())
        .collect::<Vec<_>>();
    assert_eq!(
        keys,
        [parked_keys[1].as_str(), parked_keys[2].as_str()],
        "{cards:?}"
    );
    db.pool.close().await;
}

/// PM-2: a Parallel two Agent-node levels down (root → `delegate` → saved P1
/// → `inner` → saved P2 holding the Parallel). Park, answer `a`, lose the wake
/// while `a`'s resumed model call is in flight: the reclaim walks the frozen
/// levels to the parked occurrence and resumes `a` from its model checkpoint
/// once (R); `b` and `c` are re-raised and park again.
#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One scenario: park, answer, lose the wake, recover, assert in order.
async fn postgres_a_wake_lost_two_agent_levels_down_resumes_the_answered_branch() {
    const WAKE_EXECUTION: &str = "execution-grandchild-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (first, _) = assembler_with(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        middle_pipeline(),
        parallel_child_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| {
            panic!(
                "assemble the grandchild fan-out: {error} {:?}",
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
            )
        });
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the grandchild's Parallel parks the run");
    assert_eq!(cards.len(), 3, "{cards:?}");
    let parked_keys = cards
        .iter()
        .map(|card| {
            card["interrupt_key"]
                .as_str()
                .expect("a card key")
                .to_owned()
        })
        .collect::<Vec<_>>();
    ledger.decide(&parked_keys[0], "answer", Some("Staging"));

    let (lost, lost_requests) = assembler_with(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        middle_pipeline(),
        parallel_child_pipeline(),
    );
    let wake = child_parallel_wake();
    let invocation = lost
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 2),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    lose_at_first_model_request(invocation, &lost_requests).await;
    assert_eq!(
        model_requests(&lost_requests),
        1,
        "branch a's step was in flight"
    );

    let (woken, captured) = assembler_with(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "staging it is",
        ))],
        middle_pipeline(),
        parallel_child_pipeline(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(3));
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 3)
        .await
        .unwrap_or_else(|error| panic!("the lost wake two levels down recovers: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake finishes branch a and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only branch a's step: {members:?}"
    );
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "a" && member["status"] == "end"),
        "{members:?}"
    );
    let keys = cards
        .iter()
        .filter_map(|card| card["interrupt_key"].as_str())
        .collect::<Vec<_>>();
    assert_eq!(
        keys,
        [parked_keys[1].as_str(), parked_keys[2].as_str()],
        "{cards:?}"
    );
    db.pool.close().await;
}

/// PM-2: park the grandchild's Parallel two levels down, answer `a`, and lose
/// the wake mid model call in `a`. Returns the wake request and the model
/// requests the lost wake made.
async fn lost_wake_two_levels(
    pool: &sqlx::PgPool,
    ledger: &Arc<crate::agents::graph::fanout_test_ledger::TestLedger>,
    execution: &str,
) -> super::super::request::AgentExecutionRequest {
    let (first, _) = assembler_with(
        pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        middle_pipeline(),
        parallel_child_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the grandchild fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the grandchild's Parallel parks the run");
    ledger.decide(
        cards[0]["interrupt_key"].as_str().expect("a card key"),
        "answer",
        Some("Staging"),
    );
    let (lost, lost_requests) = assembler_with(
        pool,
        vec![TestModelGatewayOutcome::Pending],
        middle_pipeline(),
        parallel_child_pipeline(),
    );
    let wake = child_parallel_wake();
    let invocation = lost
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(execution, GENERATION, 2),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(execution),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    lose_at_first_model_request(invocation, &lost_requests).await;
    wake
}

/// PM-2: reclaim a lost two-level wake after `tamper`, serving `middle` as the
/// frozen middle pipeline; the assembly-time walk must refuse with `cause`
/// before any model call or tool runs (F, nothing resumed).
async fn refused_two_levels_down(middle: Value, cause: &str, tamper: &'static str) {
    const WAKE_EXECUTION: &str = "execution-grandchild-refused";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let wake = lost_wake_two_levels(&db.pool, &ledger, WAKE_EXECUTION).await;
    if !tamper.is_empty() {
        let changed = sqlx::query(tamper)
            .execute(&db.pool)
            .await
            .expect("tamper the stored frontier")
            .rows_affected();
        assert_ne!(changed, 0, "the tamper changed nothing: {tamper}");
    }
    let (again, captured) = assembler_with(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "never",
        ))],
        middle,
        parallel_child_pipeline(),
    );
    let again = again.with_fanout_decisions_for_tests(ledger.client(3));
    let Err(refusal) = recovered(&again, &wake, WAKE_EXECUTION, 3).await else {
        panic!("the refused frontier must not be recovered ({cause})");
    };
    // Refused before anything resumes: by the pre-assembly inspect where the
    // structure alone shows it, else by the assembly-time walk.
    assert!(
        (refusal.starts_with("inspect:") || refusal.starts_with("assemble the checkpoint:"))
            && refusal.contains(&format!("[{cause}]")),
        "{refusal}"
    );
    assert_eq!(
        model_requests(&captured),
        0,
        "no model call after the refusal"
    );
    db.pool.close().await;
}

/// PM-2 (option A trust argument): the evidence Main authorizes binds the
/// root and the first-level child; the deeper rows are read only at assembly,
/// under the current claim's writer fence. A newer claim taking over between
/// claim 3's inspect and its assembly fences claim 3 out: its assembly is
/// refused as a lost writer lease, and it writes no checkpoint, no session
/// event and no ledger acknowledgement.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_takeover_between_inspect_and_assembly_fences_the_older_claim() {
    const WAKE_EXECUTION: &str = "execution-grandchild-takeover";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let wake = lost_wake_two_levels(&db.pool, &ledger, WAKE_EXECUTION).await;
    let (older, captured) = assembler_with(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "never",
        ))],
        middle_pipeline(),
        parallel_child_pipeline(),
    );
    let older = older.with_fanout_decisions_for_tests(ledger.client(3));
    // Claim 3 inspects the frontier.
    crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
        &older,
        &wake,
        &AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 3),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("claim 3 inspects the frontier");
    let rows = |pool: sqlx::PgPool| async move {
        sqlx::query_as::<_, (i64, i64)>(
            "SELECT (SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints), \
                    (SELECT count(*) FROM elitea_runtime.agent_session_events)",
        )
        .fetch_one(&pool)
        .await
        .expect("count checkpoint rows and session events")
    };
    // Claim 4 takes the execution over before claim 3 assembles.
    let (newer, _) = assembler_with(
        &db.pool,
        Vec::new(),
        middle_pipeline(),
        parallel_child_pipeline(),
    );
    crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
        &newer,
        &wake,
        &AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 4),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("claim 4 takes over and inspects");
    let before = rows(db.pool.clone()).await;
    let acks = ledger.acks().len();
    let assembled = older
        .assemble_checkpoint(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 3),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        ))
        .await;
    let Err(refusal) = assembled else {
        panic!("the fenced claim 3 must not assemble after claim 4 took over");
    };
    assert_eq!(
        refusal.code(),
        elitea_agent_runtime::assembly_error::NativeAgentAssemblyErrorCode::AuthorizationFailed,
        "a lost writer lease, not another assembly error: {refusal}"
    );
    assert_eq!(rows(db.pool.clone()).await, before, "claim 3 wrote nothing");
    assert_eq!(ledger.acks().len(), acks, "claim 3 acknowledged nothing");
    assert_eq!(model_requests(&captured), 0);
    db.pool.close().await;
}

/// PM-2 depth 3: root → `delegate` → P1 → `inner` → P2 → `deeper` → P3 holding
/// a Parallel of inline LLM branches. A branch needs a checkpoint path one
/// level below P3, past the composition depth, so the run is refused typed at
/// assembly as a composition bound (readable `APPLICATION_COMPOSITION_INVALID`,
/// as Main refuses it at save) before any model call; never an opaque failure.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_parallel_three_agent_levels_down_is_refused_typed() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (first, captured) = assembler_with3(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(
            pipeline_ask_user_call_response(),
        )],
        [
            middle_pipeline(),
            second_middle_pipeline(),
            parallel_child_pipeline(),
        ],
    );
    let request = child_parallel_request();
    let Err(error) = first
        .with_fanout_decisions_for_tests(
            crate::agents::graph::fanout_test_ledger::TestLedger::new().client(1),
        )
        .assemble(claim(&request, 1))
        .await
    else {
        panic!("a Parallel three Agent-node levels down must be refused");
    };
    assert!(
        crate::agents::application_tools::is_composition_bound(&error),
        "a typed composition bound, not an opaque failure: {error} {:?}",
        error
            .cause()
            .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
    );
    assert_eq!(
        model_requests(&captured),
        0,
        "refused before any model call"
    );
    db.pool.close().await;
}

/// The latest grandchild row lost its Agent-node activation stamp.
#[tokio::test(flavor = "current_thread")]
async fn postgres_two_levels_down_a_grandchild_row_without_its_activation_stamp_is_refused() {
    refused_two_levels_down(
        middle_pipeline(),
        "pipeline.recovery.child_unstamped",
        "UPDATE elitea_runtime.agent_graph_checkpoints \
         SET metadata = (metadata::jsonb - 'elitea.pipeline.application-activation.v1')::text \
         WHERE thread_id LIKE '%/delegate/inner'",
    )
    .await;
}

/// The middle level's frontier is not a single recovery-supported Agent node.
#[tokio::test(flavor = "current_thread")]
async fn postgres_two_levels_down_a_middle_frontier_that_is_not_one_agent_node_is_refused() {
    refused_two_levels_down(
        middle_pipeline(),
        "pipeline.recovery.child_frontier_unsupported",
        "UPDATE elitea_runtime.agent_graph_checkpoints \
         SET pending_nodes = '[\"inner\", \"inner\"]' \
         WHERE thread_id LIKE '%/delegate' AND thread_id NOT LIKE '%/delegate/%' \
           AND pending_nodes::text LIKE '%inner%'",
    )
    .await;
}

/// The frozen middle pipeline no longer holds the stored frontier node.
#[tokio::test(flavor = "current_thread")]
async fn postgres_two_levels_down_a_middle_pipeline_whose_frozen_definition_differs_is_refused() {
    let mut middle = middle_pipeline();
    middle["instructions"] = json!(
        middle["instructions"]
            .as_str()
            .expect("middle instructions")
            .replace("entry_point: inner", "entry_point: inner_v2")
            .replace("  - id: inner\n", "  - id: inner_v2\n")
    );
    refused_two_levels_down(middle, "pipeline.recovery.child_definition_mismatch", "").await;
}

/// R1: the leaf keeps its occurrence key but is parked on a branch node, not on
/// the frozen leaf definition's own Parallel node: nothing resumes blind.
#[tokio::test(flavor = "current_thread")]
async fn postgres_two_levels_down_a_leaf_parked_on_a_node_other_than_its_parallel_is_refused() {
    refused_two_levels_down(
        middle_pipeline(),
        "pipeline.recovery.child_frontier_unsupported",
        "UPDATE elitea_runtime.agent_graph_checkpoints \
         SET pending_nodes = '[\"ask_a\"]' \
         WHERE thread_id LIKE '%/delegate/inner' \
           AND metadata LIKE '%elitea.graph.parallel.occurrence.v3%'",
    )
    .await;
}

/// R1: the grandchild's activation stamp records an Agent node definition
/// other than the one Main froze: refused typed at assembly, not at run time.
#[tokio::test(flavor = "current_thread")]
async fn postgres_two_levels_down_a_grandchild_stamped_by_another_definition_is_refused() {
    refused_two_levels_down(
        middle_pipeline(),
        "pipeline.recovery.child_definition_mismatch",
        "UPDATE elitea_runtime.agent_graph_checkpoints AS row \
         SET metadata = jsonb_set( \
             row.metadata::jsonb, '{elitea.pipeline.application-activation.v1}', \
             (SELECT jsonb_object_agg(entry.key, jsonb_set(entry.value, '{definition_digest,0}', \
                  to_jsonb(((entry.value->'definition_digest'->>0)::int + 1) % 256))) \
              FROM jsonb_each(row.metadata::jsonb->'elitea.pipeline.application-activation.v1') AS entry) \
           )::text \
         WHERE row.thread_id LIKE '%/delegate/inner'",
    )
    .await;
}

/// PM-2: the walk reaches a frozen saved pipeline two levels down, but that
/// grandchild was lost mid model call in an ordinary node (no Parallel): the
/// assembly-time walk refuses the frontier and nothing is resumed blind.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_grandchild_lost_outside_a_parallel_is_not_resumed() {
    const EXECUTION_LOST: &str = "execution-grandchild-lost";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (lost, lost_requests) = assembler_with(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        middle_pipeline(),
        asking_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = lost
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &request,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(EXECUTION_LOST, GENERATION, 1),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION_LOST),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the grandchild run: {error}"));
    lose_at_first_model_request(invocation, &lost_requests).await;
    assert_eq!(model_requests(&lost_requests), 1);

    let (again, _) = assembler_with(&db.pool, Vec::new(), middle_pipeline(), asking_pipeline());
    let Err(refusal) = recovered(&again, &request, EXECUTION_LOST, 2).await else {
        panic!("a grandchild outside a Parallel must not be recovered blind");
    };
    // The assembly-time walk reaches the grandchild and refuses its frontier.
    assert!(refusal.starts_with("assemble the checkpoint:"), "{refusal}");
    db.pool.close().await;
}

/// FU-249: recovery resumes an Agent node's saved child only where the child
/// is parked inside a fixed Parallel occurrence. A child lost mid model call
/// in an ordinary node (here its LLM node) is not resumed blind: the
/// assembly-time walk refuses the frontier and the run ends typed, as before.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_child_pipeline_lost_outside_a_parallel_is_not_resumed() {
    const EXECUTION_LOST: &str = "execution-child-lost";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (lost, lost_requests) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        pipeline_connector(),
        asking_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = lost
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &request,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(EXECUTION_LOST, GENERATION, 1),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION_LOST),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the child run: {error}"));
    lose_at_first_model_request(invocation, &lost_requests).await;
    assert_eq!(model_requests(&lost_requests), 1);

    let (again, _) = assembler(
        &db.pool,
        Vec::new(),
        pipeline_connector(),
        asking_pipeline(),
    );
    let Err(refusal) = recovered(&again, &request, EXECUTION_LOST, 2).await else {
        panic!("a child outside a Parallel must not be recovered blind");
    };
    // PM-2: the pre-assembly inspect sees one pending child node; the
    // assembly-time walk against the frozen definitions refuses it.
    assert!(refusal.starts_with("assemble the checkpoint:"), "{refusal}");
    db.pool.close().await;
}

/// A second fresh turn in the same conversation through the same Agent node
/// and an ordinary saved child pipeline (one LLM node, no Parallel), the task
/// mapped from the user input as the browser's "FP outer" does.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_second_turn_through_the_same_child_pipeline_runs() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    for (turn, execution) in [(1u8, "execution-turn-one"), (2, "execution-turn-two")] {
        let (assembler, captured) = assembler(
            &db.pool,
            vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                "done",
            ))],
            pipeline_connector(),
            asking_pipeline(),
        );
        let mut request = child_parallel_request();
        let yaml = request.payload.application["version_details"]["instructions"]
            .as_str()
            .expect("root yaml")
            .replace(
                "task: {type: fixed, value: 'Summarize the release'}",
                "task: {type: variable, value: input}",
            )
            .replace("state:\n", "state:\n  input: str\n");
        request.payload.application["version_details"]["instructions"] = json!(yaml);
        request.payload.user_input = super::super::request::UserInput::Text(format!("turn {turn}"));
        let invocation = assembler
            .assemble(AuthorizedNativeAssembly::from_authorized(
                &request,
                test_runtime_context_authority(),
                test_session_authority_for_reclaim(execution, GENERATION, turn),
                Arc::new(TestStateWriterLease::current()),
                AuthorizedNativeCommandBinding::fixture_for_execution(execution),
            ))
            .await
            .unwrap_or_else(|error| panic!("assemble turn {turn}: {error}"));
        run_collecting(invocation)
            .await
            .unwrap_or_else(|error| panic!("turn {turn} completes: {error}"));
        assert_eq!(model_requests(&captured), 1, "turn {turn} runs its child");
    }
    db.pool.close().await;
}

/// FU-249: a second fresh turn in the same conversation through the same
/// Agent node and saved child pipeline holding a Parallel. The child thread
/// holds the first turn's completed visit (its occurrence closed by the join);
/// the second visit runs a new occurrence and joins again.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_second_turn_through_a_child_pipeline_parallel_runs() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    for (turn, execution) in [(1u8, "execution-turn-one"), (2, "execution-turn-two")] {
        let (assembler, captured) = assembler(
            &db.pool,
            (0..3)
                .map(|_| TestModelGatewayOutcome::Response(pipeline_text_response("done")))
                .collect(),
            pipeline_connector(),
            parallel_child_pipeline(),
        );
        let mut request = child_parallel_request();
        request.payload.user_input = super::super::request::UserInput::Text(format!("turn {turn}"));
        let invocation = assembler
            .with_fanout_decisions_for_tests(ledger.client(u64::from(turn)))
            .assemble(AuthorizedNativeAssembly::from_authorized(
                &request,
                test_runtime_context_authority(),
                test_session_authority_for_reclaim(execution, GENERATION, turn),
                Arc::new(TestStateWriterLease::current()),
                AuthorizedNativeCommandBinding::fixture_for_execution(execution),
            ))
            .await
            .unwrap_or_else(|error| panic!("assemble turn {turn}: {error}"));
        run_collecting(invocation)
            .await
            .unwrap_or_else(|error| panic!("turn {turn} completes: {error}"));
        assert_eq!(model_requests(&captured), 3, "turn {turn}");
    }
    db.pool.close().await;
}

fn pipeline_connector() -> Arc<PipelineMcpConnector> {
    Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::new(AtomicUsize::new(0)),
        read_only: false,
    })
}

/// Three saved-pipeline branches ask and park; the third card is answered.
async fn parked_with_the_third_answered(
    pool: &sqlx::PgPool,
    ledger: &Arc<crate::agents::graph::fanout_test_ledger::TestLedger>,
) -> String {
    parked_with_the_third_answered_for(pool, ledger, "pipeline", asking_pipeline()).await
}

/// [`parked_with_the_third_answered`] for saved participants of `agent_type`.
async fn parked_with_the_third_answered_for(
    pool: &sqlx::PgPool,
    ledger: &Arc<crate::agents::graph::fanout_test_ledger::TestLedger>,
    agent_type: &str,
    details: Value,
) -> String {
    let (first, _) = assembler(
        pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        pipeline_connector(),
        details,
    );
    let request = request_for("continue", agent_type);
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the activation parks");
    let third = cards[2]["interrupt_key"]
        .as_str()
        .expect("the third card's key")
        .to_owned();
    ledger.decide(&third, "answer", Some("red"));
    third
}

/// Browser finding 2026-10-27: a wake whose process is lost before its first
/// root save is reclaimed through the production checkpoint recovery (inspect,
/// authorize, assemble the checkpoint) under its own execution. Its root still
/// holds the park an earlier execution wrote; it runs again as the wake (I),
/// never ending as an internal failure with the answer left undelivered.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_lost_before_its_first_root_save_recovers_as_the_wake() {
    const WAKE_EXECUTION: &str = "execution-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let third = parked_with_the_third_answered(&db.pool, &ledger).await;

    // The wake's first claim was lost before it saved its root: its reclaim
    // inspects the root, which still holds the park of the first execution.
    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "the saddle is red",
        ))],
        pipeline_connector(),
        asking_pipeline(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(2));
    let wake = wake_request("continue");
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 2)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers from its park: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake resumes the answered child and parks again");
    assert_eq!(model_requests(&captured), 1, "only the answered child runs");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    assert!(
        cards
            .iter()
            .filter(|card| card["interrupt_key"] != third.as_str())
            .count()
            >= 2,
        "{cards:?}"
    );
    db.pool.close().await;
}

/// FU-246: the wake applied the answer to a saved Agent member's card, and the
/// process was lost while that member's resumed Agent was in its model call.
/// The reclaim (production recovery under the wake's own execution) resumes
/// that Agent with the answer: the answer is never asked again, the answered
/// member never raises a new card, and the unanswered members park again.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_lost_mid_model_resumes_the_answered_agent_branch() {
    const WAKE_EXECUTION: &str = "execution-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let third =
        parked_with_the_third_answered_for(&db.pool, &ledger, "agent", asking_agent()).await;

    let (lost, lost_requests) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        pipeline_connector(),
        asking_agent(),
    );
    let wake = wake_request_for("continue", "agent");
    let invocation = lost
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 2),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    lose_at_first_model_request(invocation, &lost_requests).await;
    assert_eq!(model_requests(&lost_requests), 1);

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "third done",
        ))],
        pipeline_connector(),
        asking_agent(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(3));
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 3)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake finishes the answered member and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered member's step: {members:?}"
    );
    let resumed = String::from_utf8_lossy(&captured.lock().expect("requests")[0].body).into_owned();
    assert!(resumed.contains("red"), "{resumed}");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    let keys = cards
        .iter()
        .filter_map(|card| card["interrupt_key"].as_str())
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(keys.len(), 2, "{cards:?}");
    assert!(!keys.contains(third.as_str()), "{cards:?}");
    db.pool.close().await;
}

/// Browser finding 2026-10-27: the wake applied the answer, its child was in
/// its model call when the process was lost. The reclaim (production recovery
/// under the wake's own execution) resumes that model step with the answer
/// (R): the answer is never asked again and the ledger is never re-read for it.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_lost_mid_model_resumes_the_answered_child() {
    const WAKE_EXECUTION: &str = "execution-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let third = parked_with_the_third_answered(&db.pool, &ledger).await;

    // The wake's first claim applies the answer, then its process is lost
    // while the answered child's model call is in flight.
    let (lost, lost_requests) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        pipeline_connector(),
        asking_pipeline(),
    );
    let wake = wake_request("continue");
    let invocation = lost
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            // Claim ordinals stay unique across both executions, as claim ids are.
            test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 2),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    // Dropped inside the gateway's 1 s header timeout: the process is gone.
    lose_at_first_model_request(invocation, &lost_requests).await;
    assert_eq!(
        model_requests(&lost_requests),
        1,
        "the answered child's step was in flight"
    );

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "the saddle is red",
        ))],
        pipeline_connector(),
        asking_pipeline(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(3));
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 3)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake finishes the answered child and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered child's step: {members:?} {:?}",
        cards
            .iter()
            .map(|card| (
                card["interrupt_key"].clone(),
                card["display"]["message"].clone()
            ))
            .collect::<Vec<_>>()
    );
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    // The answered member never asks again: only the two unanswered members'
    // cards are (re-)announced, never a new card of the answered one.
    let keys = cards
        .iter()
        .filter_map(|card| card["interrupt_key"].as_str())
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(keys.len(), 2, "{cards:?}");
    assert!(!keys.contains(third.as_str()), "{cards:?}");
    db.pool.close().await;
}

/// R: `second` dies in its model call and resumes from its model checkpoint.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_child_caught_mid_model_resumes_from_its_checkpoint() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "mid_model", "stop").await;
}

/// I: `second` dies in a read-only tool, which runs again.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_child_read_only_tool_in_flight_runs_again() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "read_only", "stop").await;
}

/// T-2A D1, the D2 classes for an inline LLM branch: mid-model R.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branch_caught_mid_model_resumes_from_its_checkpoint() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario_for(&url, "llm", "mid_model", "stop").await;
}

/// Inline LLM branch, read-only tool in flight: I.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branch_read_only_tool_in_flight_runs_again() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario_for(&url, "llm", "read_only", "stop").await;
}

/// Inline LLM branch, effectful tool in flight without a receipt: the
/// branch fails `graph.fanout.may_have_started` and the tool never runs
/// again; On Error decides the node.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branch_effect_in_flight_may_have_started() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario_for(&url, "llm", "effect", "continue").await;
    scenario_for(&url, "llm", "effect", "stop").await;
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One child process per phase: crash, then recover with its assertions.
async fn postgres_fanout_effect_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let on_error = std::env::var(ON_ERROR_ENV).expect("on_error");
    let mode = std::env::var(MODE_ENV).expect("mode");
    let pool = connect(&database).await;
    let request = match std::env::var(KIND_ENV).expect("kind").as_str() {
        "llm" => inline_request(&on_error),
        _ => request(&on_error),
    };
    match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "crash" if mode == "approved_effect" => {
            let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
            // `third` runs while `second` waits on its card, and answers only
            // once the card is approved: the approval is taken live, never
            // after the activation parked.
            let decided = Arc::new(tokio::sync::Notify::new());
            let (platform, model_facade, _) = gated_saved_runtime(
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response("first done")),
                    TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
                    TestModelGatewayOutcome::Gated {
                        gate: Arc::clone(&decided),
                        response: pipeline_text_response("third done"),
                    },
                ],
                effectful_agent(),
            );
            let invocation = PipelineNativeAgentAssembler::postgres(
                pool.clone(),
                SessionLimits::default(),
                CheckpointLimits::default(),
                sensitive_pipeline_mcp_policy(),
                platform,
                model_facade,
            )
            .with_mcp_connector(Arc::new(CrashingMcpConnector {
                pool: pool.clone(),
                read_only: false,
            }))
            .with_fixed_parallel_for_tests()
            .with_fanout_decisions_for_tests(ledger.client(1))
            .assemble(claim(&request, 1))
            .await
            .expect("crash: assemble the fan-out pipeline");
            // The approved call exits this process; reaching the end is the failure.
            let outcome = run_deciding(invocation, &|card| {
                let key = card["interrupt_key"].as_str().expect("card key");
                match (card["fanout_v1"]["member"].as_str(), card["kind"].as_str()) {
                    (Some("second"), Some("tool_guard")) => ledger.decide(key, "approve", None),
                    other => panic!("crash: unexpected card {other:?}"),
                }
                decided.notify_one();
            })
            .await;
            panic!("crash: the process outlived the approved call: {outcome:?}");
        }
        "crash" => {
            let second = if mode == "mid_model" {
                TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE)
            } else {
                TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response())
            };
            let (assembler, _) = assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response("first done")),
                    second,
                ],
                Arc::new(CrashingMcpConnector {
                    pool: pool.clone(),
                    read_only: mode == "read_only",
                }),
                effectful_agent(),
            );
            let invocation = assembler
                .assemble(claim(&request, 1))
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "crash: assemble the fan-out pipeline: {error} {:?}",
                        error.cause().map(
                            elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
                        )
                    )
                });
            // The tool or the model call exits this process; reaching the end is the failure.
            let outcome = run_to_end(invocation).await;
            panic!("crash: the process outlived its crash point: {outcome:?}");
        }
        "recover" if !matches!(mode.as_str(), "effect" | "approved_effect") => {
            let tool_calls = Arc::new(AtomicUsize::new(0));
            let (assembler, captured) = assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response("second done")),
                    TestModelGatewayOutcome::Response(pipeline_text_response("third done")),
                ],
                Arc::new(PipelineMcpConnector {
                    connections: Arc::new(AtomicUsize::new(0)),
                    tool_calls: Arc::clone(&tool_calls),
                    // The same tool declarations as the crashed process offered.
                    read_only: mode == "read_only",
                }),
                effectful_agent(),
            );
            // The production reclaim: model-checkpoint recovery (profile ON).
            let invocation = recovered(&assembler, &request, EXECUTION, 2)
                .await
                .expect("recover: the replacement Worker recovers the reclaim");
            let members = run_to_end(invocation)
                .await
                .expect("recover: the resumed child completes the pipeline");
            // `first` is served from its receipt; `second` and `third` raise
            // their lifecycle in order, under their frozen member identity.
            let statuses = members
                .iter()
                .map(|member| {
                    let tier = &member["parent_agent_path"][0];
                    assert_eq!(tier["sibling_ordinal"], member["fanout_v1"]["ordinal"]);
                    assert!(
                        tier["call_id"]
                            .as_str()
                            .is_some_and(|id| id.starts_with("fo1_"))
                    );
                    assert_eq!(member["fanout_v1"]["total"], 3);
                    (
                        member["fanout_v1"]["member"]
                            .as_str()
                            .unwrap_or_default()
                            .to_owned(),
                        member["status"].as_str().unwrap_or_default().to_owned(),
                    )
                })
                .collect::<Vec<_>>();
            assert_eq!(
                statuses,
                [
                    ("second", "start"),
                    ("second", "end"),
                    ("third", "start"),
                    ("third", "end")
                ]
                .map(|(member, status)| (member.to_owned(), status.to_owned()))
            );
            // R resumes the saved model request; I replays the saved call
            // batch without a model call and runs the read-only tool again.
            assert_eq!(
                tool_calls.load(Ordering::Acquire),
                usize::from(mode == "read_only")
            );
            assert_eq!(
                model_requests(&captured),
                2,
                "recover: `second` once (after its tool or its saved request) and `third`"
            );
            let entries = joined(&pool).await.expect("joined entries");
            assert_eq!(entries.len(), 3, "{entries:?}");
            for (entry, id) in entries.iter().zip(["first", "second", "third"]) {
                assert_eq!(entry["branch_id"], id);
                assert_eq!(entry["status"], "succeeded", "{entries:?}");
            }
            assert_eq!(
                tool_boundaries(&pool, "claim-2").await,
                Vec::<String>::new()
            );
        }
        "recover" => {
            let tool_calls = Arc::new(AtomicUsize::new(0));
            let connector = Arc::new(PipelineMcpConnector {
                connections: Arc::new(AtomicUsize::new(0)),
                tool_calls: Arc::clone(&tool_calls),
                read_only: false,
            });
            let outcomes = vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                "third done",
            ))];
            let (assembler, captured) = if mode == "approved_effect" {
                // The approved call's run under the same sensitive-tool policy.
                let (platform, model_facade, captured) = saved_runtime(outcomes, effectful_agent());
                (
                    PipelineNativeAgentAssembler::postgres(
                        pool.clone(),
                        SessionLimits::default(),
                        CheckpointLimits::default(),
                        sensitive_pipeline_mcp_policy(),
                        platform,
                        model_facade,
                    )
                    .with_mcp_connector(connector)
                    .with_fixed_parallel_for_tests()
                    .with_fanout_decisions_for_tests(
                        crate::agents::graph::fanout_test_ledger::TestLedger::new().client(2),
                    ),
                    captured,
                )
            } else {
                assembler(&pool, outcomes, connector, effectful_agent())
            };
            let invocation = recovered(&assembler, &request, EXECUTION, 2)
                .await
                .unwrap_or_else(|error| {
                    panic!("recover: the replacement Worker recovers the reclaim: {error}")
                });
            let outcome = run_to_end(invocation).await;
            assert_eq!(
                tool_calls.load(Ordering::Acquire),
                0,
                "recover: the effectful tool is never dispatched again"
            );
            if on_error == "continue" {
                let members = outcome.expect("recover: partial results complete the pipeline");
                // The re-run `second` ends failed as data; `third` runs.
                let statuses = members
                    .iter()
                    .map(|member| {
                        format!(
                            "{}:{}",
                            member["fanout_v1"]["member"].as_str().unwrap_or_default(),
                            member["status"].as_str().unwrap_or_default()
                        )
                    })
                    .collect::<Vec<_>>();
                // An approved member waited on its card while `third` ran.
                // `third` may have ended before the approved call's crash
                // (served from its receipt here) or not (it runs here, once):
                // both interleavings are correct, the approved call is never
                // repeated either way.
                let approved = mode == "approved_effect";
                let third_ran_here = statuses.iter().any(|status| status.starts_with("third:"));
                let expected: &[&str] = if approved && !third_ran_here {
                    &["second:start", "second:failed"]
                } else {
                    &["second:start", "second:failed", "third:start", "third:end"]
                };
                assert_eq!(statuses, expected);
                assert_eq!(
                    model_requests(&captured),
                    usize::from(!approved || third_ran_here),
                    "recover: only `third` calls the model, unless it already ended"
                );
                let entries = joined(&pool).await.expect("joined entries");
                assert_eq!(entries.len(), 3, "{entries:?}");
                assert_eq!(entries[0]["branch_id"], "first");
                assert_eq!(entries[0]["status"], "succeeded");
                assert_eq!(entries[1]["branch_id"], "second");
                assert_eq!(entries[1]["status"], "failed");
                assert_eq!(entries[1]["error"]["code"], MAY_HAVE_STARTED);
                assert_eq!(entries[2]["branch_id"], "third");
                assert_eq!(entries[2]["status"], "succeeded");
            } else {
                let error = outcome.expect_err("recover: On Error stop fails the node");
                // The run ends with the registered "may have started, not
                // repeated" reason, as an unscoped Agent node's approved call.
                assert!(
                    error.contains(crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE),
                    "recover: a typed fan-out failure: {error}"
                );
                assert_eq!(
                    model_requests(&captured),
                    0,
                    "recover: nothing calls the model"
                );
            }
            assert!(
                tool_boundaries(&pool, "claim-2").await.is_empty(),
                "recover: no child crossed a tool boundary again"
            );
        }
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}

// ---------------------------------------------------------------------------
// FU-248: an effectful direct node inside a saved child pipeline runs behind
// the run's fenced journal, scoped to the child's visit, and a Worker lost
// inside its call recovers by what the journal holds.
// ---------------------------------------------------------------------------

const CHILD_DIRECT_TEST: &str =
    "agents::pipeline_tests::fanout_effect_pg_tests::postgres_child_direct_effect_process";
/// `1`: root → Agent node → saved pipeline with the direct node.
/// `2`: root → Agent node → saved pipeline → Agent node → saved pipeline with it.
const DEPTH_ENV: &str = "ELITEA_CHILD_DIRECT_PG_DEPTH";
/// `1`: the child also declares keys the Agent node above it feeds from mapped
/// variables (REVIEW-1541 R1). A nested pipeline may declare only overwrite
/// channels (`compile_subgraph_with_runtime` refuses typed reducers), so a
/// re-entry that re-sent them would reduce nothing twice; it no longer sends
/// them, and the journal must stay the one the lost call opened.
const VARS_ENV: &str = "ELITEA_CHILD_DIRECT_PG_VARS";
/// The child state keys fed by mapped variables.
const CHILD_MAPPED_STATE: &str = "  items: list\n  count: int\n";
/// The Agent-node variables that feed them.
const CHILD_MAPPED_MAPPING: &str =
    "      items: {type: fixed, value: [seed]}\n      count: {type: fixed, value: 1}\n";

/// `yaml` with the mapped child keys declared (`state`) or mapped (`task`).
fn with_mapped_variables(yaml: &str, vars: bool, after: &str, insert: &str) -> String {
    if !vars {
        return yaml.to_owned();
    }
    let at = yaml.find(after).expect("anchor") + after.len();
    let end = at + yaml[at..].find('\n').expect("line end") + 1;
    format!("{}{insert}{}", &yaml[..end], &yaml[end..])
}

/// The saved child that runs the effectful MCP call, then, for `committed`,
/// an LLM step the crash process dies in (so the call's journal committed).
fn child_direct_pipeline(then_llm: bool, vars: bool) -> Value {
    let mut pipeline = direct_effect_pipeline();
    if then_llm {
        pipeline["instructions"] = json!(
            "state:\n  input: str\n  messages: list\n  answer: dict\n  summary: str\nentry_point: rev\nnodes:\n  - id: rev\n    type: mcp\n    toolkit_name: release intelligence\n    tool: lookup_release\n    input_mapping: {}\n    output: [answer]\n    transition: after\n  - id: after\n    type: llm\n    input_mapping:\n      task: {type: fixed, value: 'Summarize the lookup'}\n    output: [summary]\n    transition: END\n"
        );
    }
    let yaml = pipeline["instructions"].as_str().expect("yaml").to_owned();
    pipeline["instructions"] = json!(with_mapped_variables(
        &yaml,
        vars,
        "  messages: list",
        CHILD_MAPPED_STATE
    ));
    pipeline
}

/// Root → `delegate` → the saved pipeline (app 3, version 4); the child's
/// `answer` is the MCP call's object.
fn child_direct_request(vars: bool) -> super::super::request::AgentExecutionRequest {
    let mut request = child_parallel_request();
    request.payload.application["version_details"]["instructions"] = json!(with_mapped_variables(
        "state:\n  answer: dict\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: \"release-agent\"\n    input_mapping:\n      task: {type: fixed, value: 'Summarize the release'}\n    output: [answer]\n    transition: END\n",
        vars,
        "      task:",
        CHILD_MAPPED_MAPPING,
    ));
    request
}

/// The middle saved pipeline (depth 2): its `inner` Agent node runs the
/// saved pipeline with the direct node (app 5, version 6).
fn child_direct_middle(vars: bool) -> Value {
    let mut middle = middle_pipeline();
    middle["instructions"] = json!(with_mapped_variables(
        "state:\n  input: str\n  messages: list\n  answer: dict\nentry_point: inner\nnodes:\n  - id: inner\n    type: agent\n    tool: inner-flow\n    input_mapping:\n      task: {type: fixed, value: 'Look the release up'}\n    output: [answer]\n    transition: END\n",
        vars,
        "      task:",
        CHILD_MAPPED_MAPPING,
    ));
    middle
}

/// The run's assembler for `depth`, with `connector` serving the child's MCP tool.
fn child_direct_assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    depth: &str,
    then_llm: bool,
    connector: Arc<dyn McpConnector>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    // At depth 2 the mapped keys are fed by the inner Agent node.
    let vars = std::env::var(VARS_ENV).is_ok_and(|vars| vars == "1");
    let (details, second) = if depth == "2" {
        (
            child_direct_middle(vars),
            Some(child_direct_pipeline(then_llm, vars)),
        )
    } else {
        (child_direct_pipeline(then_llm, vars), None)
    };
    assembler_on(
        pool,
        saved_runtime_with(outcomes, details, second),
        connector,
    )
}

/// The child thread that holds the direct node at `depth`.
fn child_direct_thread(depth: &str) -> &'static str {
    if depth == "2" {
        "%/delegate/inner"
    } else {
        "%/delegate"
    }
}

/// Every node journal thread (`n1:…`) in the database, sorted.
async fn journal_threads(pool: &sqlx::PgPool) -> Vec<String> {
    sqlx::query_scalar::<_, String>(
        "SELECT DISTINCT thread_id FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id LIKE 'n1:%' ORDER BY thread_id",
    )
    .fetch_all(pool)
    .await
    .expect("read the node journal threads")
}

/// The child's MCP tool, when the Worker is lost inside its call: it exits
/// only once its journal's Started record is durable under claim 1.
struct JournalCrashingConnector {
    pool: sqlx::PgPool,
}

#[async_trait]
impl McpConnector for JournalCrashingConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(JournalCrashingTool {
                inner: PipelineMcpTool {
                    calls: Arc::new(AtomicUsize::new(0)),
                    read_only: false,
                },
                pool: self.pool.clone(),
            })],
        )))
    }
}

struct JournalCrashingTool {
    inner: PipelineMcpTool,
    pool: sqlx::PgPool,
}

#[async_trait]
impl Tool for JournalCrashingTool {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn description(&self) -> &str {
        self.inner.description()
    }
    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }
    fn is_read_only(&self) -> bool {
        false
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        let started = sqlx::query_scalar::<_, i64>(
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE 'n1:%' AND writer_claim_id = 'claim-1'",
        )
        .fetch_one(&self.pool)
        .await
        .unwrap_or_default();
        std::process::exit(if started > 0 {
            CRASH_EXIT_CODE
        } else {
            MARKER_MISSING_EXIT_CODE
        })
    }
}

/// One phase of a [`child_direct_scenario`], in its own process.
#[tokio::test(flavor = "current_thread")]
async fn postgres_child_direct_effect_process() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let depth = std::env::var(DEPTH_ENV).expect("depth");
    let mode = std::env::var(MODE_ENV).expect("mode");
    let pool = connect(&database).await;
    let vars = std::env::var(VARS_ENV).is_ok_and(|vars| vars == "1");
    let request = child_direct_request(vars && depth == "1");
    let committed = mode == "committed";
    if std::env::var(PHASE_ENV).expect("phase") == "crash" {
        // `committed`: the call returns and its journal commits; the process
        // dies at the next step's model request. Otherwise it dies inside the
        // call, once its Started record is durable.
        let (assembler, _) = if committed {
            child_direct_assembler(
                &pool,
                vec![TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE)],
                &depth,
                true,
                pipeline_connector(),
            )
        } else {
            child_direct_assembler(
                &pool,
                Vec::new(),
                &depth,
                false,
                Arc::new(JournalCrashingConnector { pool: pool.clone() }),
            )
        };
        let invocation = assembler
            .assemble(claim(&request, 1))
            .await
            .unwrap_or_else(|error| panic!("crash: assemble: {error}"));
        let outcome = run_to_end(invocation).await;
        panic!("crash: the process outlived its crash point: {outcome:?}");
    }
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: false,
    });
    let outcomes = if committed {
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "summarized",
        ))]
    } else {
        Vec::new()
    };
    let (assembler, captured) =
        child_direct_assembler(&pool, outcomes, &depth, committed, connector);
    let invocation = recovered(&assembler, &request, EXECUTION, 2)
        .await
        .unwrap_or_else(|error| panic!("recover: the reclaim recovers: {error}"));
    let outcome = run_to_end(invocation).await;
    match mode.as_str() {
        // R: the committed result replays; the call never runs again.
        "committed" => {
            outcome.unwrap_or_else(|error| panic!("recover: the child completes: {error}"));
            assert_eq!(tool_calls.load(Ordering::Acquire), 0, "recover: replayed");
            assert_eq!(model_requests(&captured), 1, "recover: only the next step");
        }
        // C → F: the call may have run; it is not repeated and the run ends typed.
        "started" => {
            let error = outcome.expect_err("recover: a call that may have run ends the run");
            assert!(
                error.contains(crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE),
                "recover: the registered may-have-started reason: {error}"
            );
            assert_eq!(
                tool_calls.load(Ordering::Acquire),
                0,
                "recover: not repeated"
            );
            assert_eq!(model_requests(&captured), 0);
        }
        // I: the journal never recorded a start; the call runs once.
        "undispatched" => {
            outcome.unwrap_or_else(|error| panic!("recover: the child completes: {error}"));
            assert_eq!(tool_calls.load(Ordering::Acquire), 1, "recover: runs once");
        }
        other => panic!("unknown mode {other}"),
    }
}

/// FU-248 two-process proof: a Worker lost inside a saved child's journaled
/// direct call at `depth`, then a reclaim of the same execution. The journal
/// state after the loss is set to `mode`:
/// - `started`: as the crash left it, Started without a result;
/// - `committed`: the crash process died one step later; the child's newest
///   row (pending the next node) is removed, as if the process had died
///   between the journal's commit and the child's checkpoint save;
/// - `undispatched`: the journal rows are removed, as if the process had died
///   before the Started append.
///
/// The reclaim must find the same visit-scoped journal identity, and recover
/// inside the per-node budget.
async fn child_direct_scenario(url: &str, depth: &str, mode: &str, vars: bool) {
    let db = IsolatedPostgres::create(url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let phase = |phase: &'static str| {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args([
                "--exact",
                CHILD_DIRECT_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, &db.database_name)
            .env(DEPTH_ENV, depth)
            .env(MODE_ENV, mode)
            .env(VARS_ENV, if vars { "1" } else { "0" })
            .env(PHASE_ENV, phase);
        command
    };
    let output = run_phase("crash", phase("crash"));
    assert_eq!(
        output.status.code(),
        Some(CRASH_EXIT_CODE),
        "crash: {} {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let journals = journal_threads(&db.pool).await;
    assert_eq!(
        journals.len(),
        1,
        "one journal for the one call: {journals:?}"
    );
    match mode {
        "committed" => {
            let removed = sqlx::query(
                "DELETE FROM elitea_runtime.agent_graph_checkpoints \
                 WHERE thread_id LIKE $1 AND pending_nodes LIKE '%after%'",
            )
            .bind(child_direct_thread(depth))
            .execute(&db.pool)
            .await
            .expect("remove the row saved after the call")
            .rows_affected();
            assert_eq!(removed, 1, "the child saved one row after the call");
        }
        "undispatched" => {
            let removed = sqlx::query(
                "DELETE FROM elitea_runtime.agent_graph_checkpoints WHERE thread_id LIKE 'n1:%'",
            )
            .execute(&db.pool)
            .await
            .expect("remove the journal")
            .rows_affected();
            assert_ne!(removed, 0, "the call's journal existed");
        }
        _ => {}
    }
    let started = std::time::Instant::now();
    let output = run_phase("recover", phase("recover"));
    let elapsed = started.elapsed();
    assert_eq!(
        output.status.code(),
        Some(0),
        "recover: {} {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    // The reclaim derived the same visit-scoped identity: it used (or, when
    // none was left, recreated) exactly the journal the lost call opened.
    assert_eq!(journal_threads(&db.pool).await, journals, "{depth} {mode}");
    // Recovery budget per node (≤ 20 s), process start included.
    eprintln!("FU-248 recovery depth {depth} {mode}: {elapsed:?}");
    assert!(elapsed < std::time::Duration::from_secs(20), "{elapsed:?}");
    db.pool.close().await;
}

macro_rules! child_direct_case {
    ($name:ident, $depth:literal, $mode:literal) => {
        child_direct_case!($name, $depth, $mode, false);
    };
    ($name:ident, $depth:literal, $mode:literal, $vars:literal) => {
        #[tokio::test(flavor = "current_thread")]
        async fn $name() {
            let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
                eprintln!(
                    "SKIP: set ELITEA_TEST_DATABASE_URL for the child direct-effect PostgreSQL proof"
                );
                return;
            };
            child_direct_scenario(&url, $depth, $mode, $vars).await;
        }
    };
}

child_direct_case!(
    postgres_a_child_direct_call_lost_after_its_commit_replays_without_calling_again,
    "1",
    "committed"
);
child_direct_case!(
    postgres_a_child_direct_call_lost_after_it_started_ends_typed_and_is_not_repeated,
    "1",
    "started"
);
child_direct_case!(
    postgres_a_child_direct_call_lost_before_it_started_runs_once,
    "1",
    "undispatched"
);
child_direct_case!(
    postgres_two_levels_down_a_child_direct_call_lost_after_its_commit_replays,
    "2",
    "committed"
);
child_direct_case!(
    postgres_two_levels_down_a_child_direct_call_lost_after_it_started_ends_typed,
    "2",
    "started"
);
// REVIEW-1541 R1: the child's keys are fed by mapped Agent-node variables; a
// re-entry at the child's frontier keeps the input its journal recorded.
child_direct_case!(
    postgres_a_child_with_mapped_variables_lost_after_its_commit_replays_without_calling_again,
    "1",
    "committed",
    true
);
child_direct_case!(
    postgres_a_child_with_mapped_variables_lost_after_it_started_is_not_repeated,
    "1",
    "started",
    true
);
child_direct_case!(
    postgres_two_levels_down_a_child_with_mapped_variables_lost_after_its_commit_replays,
    "2",
    "committed",
    true
);

/// FU-248: one execution visits the same saved child twice with identical
/// inputs; each visit's effectful call runs once, under its own journal.
/// Here the visits are already told apart by the child's node event scope
/// (its call id binds the parent step), so this test does not by itself prove
/// the visit scope; `a_root_node_journal_keeps_its_identity_and_a_child_visit_changes_it`
/// and `node_journals_are_visit_scoped_in_a_child_and_unscoped_on_the_root` do.
#[tokio::test(flavor = "current_thread")]
async fn postgres_two_visits_of_one_child_in_one_execution_each_run_their_call() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the child direct-effect PostgreSQL proof"
        );
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let (assembler, _) = child_direct_assembler(
        &db.pool,
        Vec::new(),
        "1",
        false,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::clone(&tool_calls),
            read_only: false,
        }),
    );
    let mut request = child_parallel_request();
    request.payload.application["version_details"]["instructions"] = json!(
        "state:\n  answer: dict\n  messages: list\n  count: {type: int, value: 0, reducer: sum_int}\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: \"release-agent\"\n    input_mapping:\n      task: {type: fixed, value: 'Summarize the release'}\n    output: [answer]\n    transition: tick\n  - id: tick\n    type: state_modifier\n    template: \"1\"\n    output: [count]\n    transition: choose\n  - id: choose\n    type: router\n    condition: \"{{ 'delegate' if count < 2 else 'END' }}\"\n    input: [count]\n    routes: [delegate, END]\n    default_output: END\n"
    );
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the looping parent: {error}"));
    run_to_end(invocation)
        .await
        .unwrap_or_else(|error| panic!("both visits complete: {error}"));
    assert_eq!(tool_calls.load(Ordering::Acquire), 2, "one call per visit");
    assert_eq!(
        journal_threads(&db.pool).await.len(),
        2,
        "one journal per visit"
    );
    db.pool.close().await;
}

/// FU-248: a member whose saved child holds a sensitive effectful direct MCP
/// node pauses with its own card (it used to be refused before the card).
/// The run parks on it; the wake runs the approved call once under the
/// branch family's journal, and never runs a rejected one.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_member_child_sensitive_direct_call_pauses_then_runs_once_or_never() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the child direct-effect PostgreSQL proof"
        );
        return;
    };
    for (action, calls) in [("approve", 1), ("reject", 0)] {
        let db = IsolatedPostgres::create(&url).await;
        install_schema(&db.pool).await;
        sqlx::raw_sql(CHECKPOINT_MIGRATION)
            .execute(&db.pool)
            .await
            .expect("apply checkpoint migration");
        let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
        let tool_calls = Arc::new(AtomicUsize::new(0));
        let (platform, model_facade, captured) = saved_runtime_with(
            Vec::new(),
            answering_pipeline(),
            Some(direct_effect_pipeline()),
        );
        let assembler = |claim: u64| {
            PipelineNativeAgentAssembler::postgres(
                db.pool.clone(),
                SessionLimits::default(),
                CheckpointLimits::default(),
                sensitive_pipeline_mcp_policy(),
                Arc::clone(&platform),
                Arc::clone(&model_facade),
            )
            .with_mcp_connector(Arc::new(PipelineMcpConnector {
                connections: Arc::new(AtomicUsize::new(0)),
                tool_calls: Arc::clone(&tool_calls),
                read_only: false,
            }))
            .with_fixed_parallel_for_tests()
            .with_fanout_decisions_for_tests(ledger.client(claim))
        };
        let request = mixed_request("continue");
        let invocation = assembler(1)
            .assemble(claim(&request, 1))
            .await
            .unwrap_or_else(|error| panic!("{action}: assemble: {error}"));
        let (_, cards) = run_collecting(invocation)
            .await
            .unwrap_or_else(|error| panic!("{action}: the member's card parks the run: {error}"));
        assert_eq!(cards.len(), 1, "{action}: {cards:?}");
        assert_eq!(cards[0]["fanout_v1"]["member"], "second", "{cards:?}");
        assert_eq!(cards[0]["kind"], "tool_guard", "{cards:?}");
        assert_eq!(
            tool_calls.load(Ordering::Acquire),
            0,
            "{action}: nothing ran yet"
        );
        ledger.decide(
            cards[0]["interrupt_key"].as_str().expect("card key"),
            action,
            None,
        );
        let mut wake = request;
        wake.payload.user_input = super::super::request::UserInput::Text(String::new());
        wake.payload.meta.insert(
            crate::agents::graph::fanout_wake::FANOUT_WAKE_META_KEY.to_owned(),
            json!({"schema": "elitea.pipeline.fanout-wake.v1",
                "response_message_id": "10000000-0000-4000-8000-000000000043",
                "decision_revision": 1,
                "parked_execution_id": EXECUTION,
                "parked_generation": GENERATION}),
        );
        let invocation = assembler(2)
            .assemble(claim(&wake, 2))
            .await
            .unwrap_or_else(|error| panic!("{action}: assemble the wake: {error}"));
        run_to_end(invocation)
            .await
            .unwrap_or_else(|error| panic!("{action}: the wake joins: {error}"));
        assert_eq!(tool_calls.load(Ordering::Acquire), calls, "{action}");
        assert_eq!(model_requests(&captured), 0, "{action}");
        assert_eq!(
            journal_threads(&db.pool).await.len(),
            calls,
            "{action}: a journal only for the approved call"
        );
        db.pool.close().await;
    }
}
