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
    recovered_with(assembler, request, execution, attempt, None).await
}

/// [`recovered`] whose claims carry Main's paused writer (FU-269).
async fn recovered_with(
    assembler: &PipelineNativeAgentAssembler,
    request: &super::super::request::AgentExecutionRequest,
    execution: &str,
    attempt: u8,
    writer: Option<crate::protocol::elitea::runtime::v1::PausedWriterV1>,
) -> Result<AssembledNativeAgentInvocation<PipelineAgentCompletion>, String> {
    let evidence = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
        assembler,
        request,
        &AuthorizedNativeCommandBinding::fixture_for_execution(execution),
        test_session_authority_for_reclaim(execution, GENERATION, attempt)
            .with_paused_writer_for_test(writer.clone()),
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
            test_session_authority_for_reclaim(execution, GENERATION, attempt)
                .with_paused_writer_for_test(writer),
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

/// FU-264: the saved pipeline a member's `second` branch runs at `depth`:
/// `1` is the effectful direct pipeline itself (application 5); `2` is a
/// middle pipeline (application 5) whose Agent node runs it (application 7).
/// `then_llm`: an llm node follows the direct call.
fn member_child_runtime(
    depth: &str,
    outcomes: Vec<TestModelGatewayOutcome>,
    then_llm: bool,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    let direct = child_direct_pipeline(then_llm, false);
    if depth == "2" {
        saved_runtime_with3(
            outcomes,
            answering_pipeline(),
            Some(member_middle_pipeline()),
            Some(direct),
        )
    } else {
        saved_runtime_with(outcomes, answering_pipeline(), Some(direct))
    }
}

/// A middle saved pipeline whose Agent node runs application 7 (FU-264).
fn member_middle_pipeline() -> Value {
    deep_flow(child_direct_middle(false))
}

/// [`member_middle_pipeline`] whose answer is text (its inner pipeline
/// answers text, not the direct call's object).
fn member_middle_text_pipeline() -> Value {
    deep_flow(middle_pipeline())
}

/// `middle` with its Agent node pointed at application 7 (`deep-flow`).
fn deep_flow(mut middle: Value) -> Value {
    let instructions = middle["instructions"]
        .as_str()
        .expect("instructions")
        .replace("tool: inner-flow", "tool: deep-flow");
    middle["instructions"] = json!(instructions);
    let tool = &mut middle["tools"][0];
    tool["name"] = json!("deep-flow");
    tool["toolkit_name"] = json!("deep-flow");
    tool["settings"] = json!({"application_id": 7, "application_version_id": 8});
    middle
}

/// FU-264 (P4): a member whose saved child runs its own Agent node (a saved
/// pipeline one level further down, nothing effectful in it) completes and
/// projects: the inner call is a tier beneath the member's tier, named
/// against the member's branch thread. On main this projection was refused.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_member_whose_child_runs_an_agent_node_completes_and_projects_the_inner_tier() {
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
    let (platform, model_facade, captured) = saved_runtime_with3(
        Vec::new(),
        answering_pipeline(),
        Some(member_middle_text_pipeline()),
        Some(answering_pipeline()),
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
    .with_fixed_parallel_for_tests();
    let request = mixed_request("stop");
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the fan-out: {error}"));
    let (frames, completion) = run_frames(invocation).await;
    completion.unwrap_or_else(|error| panic!("the fan-out completes and projects: {error}"));
    assert_eq!(model_requests(&captured), 0);
    // The inner call opens and closes under the member's tier.
    let inner = |kind: &str| {
        frames
            .iter()
            .filter(|(frame, metadata)| {
                frame == kind && metadata["metadata"]["original_name"] == "deep-flow"
            })
            .map(|(_, metadata)| metadata["metadata"]["parent_agent_path"].clone())
            .collect::<Vec<_>>()
    };
    for kind in ["agent_tool_start", "agent_tool_end"] {
        let paths = inner(kind);
        assert_eq!(paths.len(), 1, "{kind}: {frames:?}");
        let path = paths[0].as_array().expect("parent path");
        assert_eq!(
            path.len(),
            1,
            "{kind}: one tier above the inner call: {path:?}"
        );
        assert_eq!(
            path[0]["name"], "direct-flow",
            "{kind}: the member's tier: {path:?}"
        );
    }
    let entries = joined(&db.pool).await.expect("joined entries");
    assert!(
        entries
            .iter()
            .all(|entry| entry["status"].as_str() == Some("succeeded")),
        "{entries:?}"
    );
    db.pool.close().await;
}

/// FU-248 / FU-264: a member whose saved child pipeline (or the saved
/// pipeline its Agent node runs, one level further down) starts at an
/// effectful direct MCP node runs that call once, behind the member's branch
/// journals scoped to the visit, under either On Error (it used to be refused
/// as `graph.fanout.child_tool_unavailable`; two levels down, FU-264).
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_member_whose_child_calls_an_effectful_direct_tool_runs_it_once() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    for (depth, on_error) in [
        ("1", "continue"),
        ("1", "stop"),
        ("2", "continue"),
        ("2", "stop"),
    ] {
        let db = IsolatedPostgres::create(&url).await;
        install_schema(&db.pool).await;
        sqlx::raw_sql(CHECKPOINT_MIGRATION)
            .execute(&db.pool)
            .await
            .expect("apply checkpoint migration");
        let tool_calls = Arc::new(AtomicUsize::new(0));
        let (platform, model_facade, captured) = member_child_runtime(depth, Vec::new(), false);
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
        let members = run_to_end(invocation).await.unwrap_or_else(|error| {
            panic!("depth {depth} {on_error}: the fan-out completes: {error}")
        });
        assert_eq!(
            tool_calls.load(Ordering::Acquire),
            1,
            "depth {depth} {on_error}: once: {:?} {members:?}",
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
    for (depth, action, calls) in [
        ("1", "approve", 1),
        ("1", "reject", 0),
        ("2", "approve", 1),
        ("2", "reject", 0),
    ] {
        let db = IsolatedPostgres::create(&url).await;
        install_schema(&db.pool).await;
        sqlx::raw_sql(CHECKPOINT_MIGRATION)
            .execute(&db.pool)
            .await
            .expect("apply checkpoint migration");
        let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
        let tool_calls = Arc::new(AtomicUsize::new(0));
        let (platform, model_facade, captured) = member_child_runtime(depth, Vec::new(), false);
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
        let (_, cards) = run_collecting(invocation).await.unwrap_or_else(|error| {
            panic!("depth {depth} {action}: the member's card parks the run: {error}")
        });
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

// ---------------------------------------------------------------------------
// FU-265 / FU-269: a continuation lost before its first root save. Execution
// A pauses on a saved child's sensitive direct call (at `depth`); continuation
// B (Main names A as the paused writer on its claim) approves it and is lost
// around the journaled call; the reclaim of B re-enters A's row, re-runs the
// continuation behind its own suffix, and the call's journal decides.
// ---------------------------------------------------------------------------

const CONTINUATION_TEST: &str =
    "agents::pipeline_tests::fanout_effect_pg_tests::postgres_continuation_effect_process";
/// The execution the approval continues (its generation is [`GENERATION`]).
const CONTINUING_EXECUTION: &str = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b";
/// `1`: Main names the paused writer; `0`: an older Main without it.
const MARKER_ENV: &str = "ELITEA_CONTINUATION_PG_MARKER";
/// The paused card, handed from the pause phase to the later ones.
const PENDING_ENV: &str = "ELITEA_CONTINUATION_PG_PENDING";
const PENDING_LINE: &str = "FU265_PENDING ";

/// The paused writer Main serves on B's claim (A at [`GENERATION`]).
fn paused_writer(marker: bool) -> Option<crate::protocol::elitea::runtime::v1::PausedWriterV1> {
    marker.then(|| crate::protocol::elitea::runtime::v1::PausedWriterV1 {
        execution_id: EXECUTION.to_owned(),
        generation: GENERATION,
    })
}

fn claim_for<'a>(
    request: &'a super::super::request::AgentExecutionRequest,
    execution: &str,
    attempt: u8,
    marker: bool,
) -> AuthorizedNativeAssembly<'a> {
    AuthorizedNativeAssembly::from_authorized(
        request,
        test_runtime_context_authority(),
        test_session_authority_for_reclaim(execution, GENERATION, attempt)
            .with_paused_writer_for_test(paused_writer(marker)),
        Arc::new(TestStateWriterLease::current()),
        AuthorizedNativeCommandBinding::fixture_for_execution(execution),
    )
}

/// The root → Agent node → saved child (a sensitive direct call at `depth`)
/// assembler. `then_llm`: an llm node follows the call in that child.
fn continuation_assembler(
    pool: &sqlx::PgPool,
    connector: Arc<dyn McpConnector>,
    depth: &str,
    then_llm: bool,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let (details, second) = if depth == "2" {
        (
            child_direct_middle(false),
            Some(child_direct_pipeline(then_llm, false)),
        )
    } else {
        (child_direct_pipeline(then_llm, false), None)
    };
    let (platform, model_facade, captured) = saved_runtime_with(outcomes, details, second);
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
        .with_fixed_parallel_for_tests(),
        captured,
    )
}

/// A pipeline whose root HITL node routes an approval to a state modifier.
const CONTINUATION_GATE_YAML: &str = "state:\n  input:\n    type: str\n  messages:\n    type: list\n  verdict:\n    type: str\nentry_point: review\nnodes:\n  - id: review\n    type: hitl\n    input:\n      - input\n    user_message:\n      type: fixed\n      value: Approve?\n    routes:\n      approve: approved\n      reject: rejected\n  - id: approved\n    type: state_modifier\n    template: 'APPROVED {{ input }}'\n    input: [input]\n    output: [verdict]\n    transition: END\n  - id: rejected\n    type: state_modifier\n    template: 'REJECTED {{ input }}'\n    input: [input]\n    output: [verdict]\n    transition: END\n";

/// The paused run at `depth`: `0` a sensitive direct node on the root,
/// `gate` a root HITL node, `1`/`2` a saved child's direct node that deep.
fn continuation_request(depth: &str) -> super::super::request::AgentExecutionRequest {
    match depth {
        "0" => mcp_pipeline_request(
            "release intelligence",
            &["lookup_release"],
            "lookup_release",
        ),
        "gate" => {
            let mut request = pipeline_request();
            request.payload.application["version_details"]["instructions"] =
                json!(CONTINUATION_GATE_YAML);
            request
        }
        _ => child_direct_request(false),
    }
}

/// The approval of `pending`, as Main admits it.
fn approval(pending: &Value, depth: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = continuation_request(depth);
    request.binding.request_content_digest = [29; 32];
    request.payload.should_continue = true;
    request.payload.hitl_resume = true;
    request.payload.hitl_action = Some("approve".to_owned());
    request.payload.hitl_value = Some(String::new());
    let mut decision = json!({
        "interrupt_id": pending["interrupt_id"],
        "action": "approve", "value": ""
    });
    if let Some(call) = pending.get("tool_call_id").filter(|call| !call.is_null()) {
        decision["tool_call_id"] = call.clone();
    }
    request.payload.hitl_decisions = vec![decision];
    request
}

/// One phase of a [`continuation_scenario`], in its own process.
#[tokio::test(flavor = "current_thread")]
async fn postgres_continuation_effect_process() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let pool = connect(&database).await;
    let marker = std::env::var(MARKER_ENV).is_ok_and(|marker| marker == "1");
    let depth = std::env::var(DEPTH_ENV).expect("depth");
    let mode = std::env::var(MODE_ENV).expect("mode");
    let committed = mode == "committed";
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let counting = Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: false,
    });
    match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "pause" => {
            let request = continuation_request(&depth);
            let (assembler, _) =
                continuation_assembler(&pool, counting, &depth, committed, Vec::new());
            let invocation = assembler
                .assemble(claim_for(&request, EXECUTION, 1, false))
                .await
                .unwrap_or_else(|error| panic!("pause: assemble: {error}"));
            let (browser, _) = collect_pipeline_pause_events(invocation).await;
            let card = browser
                .iter()
                .find(|event| event["type"] == "agent_hitl_interrupt")
                .unwrap_or_else(|| panic!("pause: a sensitive card: {browser:?}"));
            let metadata = &card["response_metadata"];
            let pending = metadata["hitl_interrupts"].get(0).unwrap_or(metadata);
            assert_eq!(tool_calls.load(Ordering::Acquire), 0, "pause: nothing ran");
            println!("{PENDING_LINE}{pending}");
        }
        "crash" => {
            let pending: Value =
                serde_json::from_str(&std::env::var(PENDING_ENV).expect("pending")).expect("json");
            let request = approval(&pending, &depth);
            // `before_runner`: B is lost after its start recorded `continuing`
            // and before its Runner ran anything.
            if mode == "before_runner" {
                let (assembler, _) =
                    continuation_assembler(&pool, counting, &depth, false, Vec::new());
                let _invocation = assembler
                    .assemble(claim_for(&request, CONTINUING_EXECUTION, 1, marker))
                    .await
                    .unwrap_or_else(|error| panic!("crash: assemble the continuation: {error}"));
                std::process::exit(CRASH_EXIT_CODE);
            }
            // `committed`: the call returns and its journal commits; the
            // process dies at the next step's model request. Otherwise it
            // dies inside the call, once its Started record is durable.
            let (assembler, _) = if committed {
                continuation_assembler(
                    &pool,
                    counting,
                    &depth,
                    true,
                    vec![TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE)],
                )
            } else {
                continuation_assembler(
                    &pool,
                    Arc::new(JournalCrashingConnector { pool: pool.clone() }),
                    &depth,
                    false,
                    Vec::new(),
                )
            };
            let invocation = assembler
                .assemble(claim_for(&request, CONTINUING_EXECUTION, 1, marker))
                .await
                .unwrap_or_else(|error| panic!("crash: assemble the continuation: {error}"));
            let outcome = run_to_end(invocation).await;
            panic!("crash: the process outlived its crash point: {outcome:?}");
        }
        // B's first attempt runs to its end (no loss).
        "complete" => complete_continuation(&pool, counting, &depth, marker, &tool_calls).await,
        "recover" => {
            let outcomes = if committed {
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "summarized",
                ))]
            } else {
                Vec::new()
            };
            let (assembler, captured) =
                continuation_assembler(&pool, counting, &depth, committed, outcomes);
            recover_continuation(&assembler, marker, &depth, &mode, &tool_calls, &captured).await;
        }
        other => panic!("unknown phase {other}"),
    }
}

/// B's first attempt of a [`continuation_scenario`] run to its end: the
/// call runs once.
async fn complete_continuation(
    pool: &sqlx::PgPool,
    counting: Arc<PipelineMcpConnector>,
    depth: &str,
    marker: bool,
    tool_calls: &AtomicUsize,
) {
    let pending: Value =
        serde_json::from_str(&std::env::var(PENDING_ENV).expect("pending")).expect("json");
    let request = approval(&pending, depth);
    let (assembler, _) = continuation_assembler(pool, counting, depth, false, Vec::new());
    let invocation = assembler
        .assemble(claim_for(&request, CONTINUING_EXECUTION, 1, marker))
        .await
        .unwrap_or_else(|error| panic!("complete: assemble the continuation: {error}"));
    run_to_end(invocation)
        .await
        .unwrap_or_else(|error| panic!("complete: the continuation completes: {error}"));
    assert_eq!(tool_calls.load(Ordering::Acquire), 1, "complete: runs once");
}

/// What the re-run of a lost continuation does, per `depth` and `mode`
/// (R / C / I).
async fn assert_rerun(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
    depth: &str,
    mode: &str,
    tool_calls: &AtomicUsize,
    captured: &CapturedModelRequests,
) {
    if depth == "0" && mode == "started" {
        // C: a root direct node's call that may have run is never
        // repeated; it is escalated to an operator (node recovery's
        // reconcile pause), as on any reclaim of such a node.
        let (frames, completion) = run_frames(invocation).await;
        completion.unwrap_or_else(|error| panic!("recover: the run pauses: {error}"));
        let recovery = frames
            .iter()
            .find(|(kind, _)| kind == "agent_node_recovery_required")
            .map_or_else(
                || panic!("recover: an operator reconcile pause: {frames:?}"),
                |(_, metadata)| &metadata["node_recovery_required_v1"],
            );
        assert_eq!(recovery["stop_reason"], "effect_reconciliation_required");
        assert_eq!(recovery["node_id"], "direct");
        assert_eq!(recovery["allowed_actions"], json!(["reconcile"]));
        assert_eq!(
            tool_calls.load(Ordering::Acquire),
            0,
            "recover: not repeated"
        );
        return;
    }
    let outcome = run_to_end(invocation).await;
    match mode {
        // R: the committed result replays; the call never runs again.
        "committed" => {
            outcome.unwrap_or_else(|error| panic!("recover: the run completes: {error}"));
            assert_eq!(tool_calls.load(Ordering::Acquire), 0, "recover: replayed");
            assert_eq!(model_requests(captured), 1, "recover: only the next step");
        }
        // C → F: the call may have run; it is not repeated and the run
        // ends with the registered may-have-started reason.
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
        }
        // I: the journal never recorded a start; the call runs once.
        "undispatched" => {
            outcome.unwrap_or_else(|error| panic!("recover: the run completes: {error}"));
            assert_eq!(tool_calls.load(Ordering::Acquire), 1, "recover: runs once");
        }
        // Lost before its Runner: the decision is applied once (a direct
        // call runs once, a HITL node routes and completes).
        "before_runner" => {
            outcome.unwrap_or_else(|error| panic!("recover: the run completes: {error}"));
            assert_eq!(
                tool_calls.load(Ordering::Acquire),
                usize::from(depth != "gate"),
                "recover: runs once"
            );
        }
        other => panic!("unknown mode {other}"),
    }
}

/// The reclaim phase of a [`continuation_scenario`]. With the paused writer
/// B re-runs from A's row and the journal decides the call (R / C / I);
/// without it (an older Main) B ends typed. A refusal log carries ids and
/// generations only (C4).
async fn recover_continuation(
    assembler: &PipelineNativeAgentAssembler,
    marker: bool,
    depth: &str,
    mode: &str,
    tool_calls: &AtomicUsize,
    captured: &CapturedModelRequests,
) {
    let pending: Value =
        serde_json::from_str(&std::env::var(PENDING_ENV).expect("pending")).expect("json");
    let request = approval(&pending, depth);
    // This phase runs in its own process, so a global subscriber
    // captures exactly its logs.
    let capture = crate::agents::assembly_tests::CapturedOutput::default();
    tracing::subscriber::set_global_default(
        tracing_subscriber::fmt()
            .without_time()
            .with_ansi(false)
            .with_writer(capture.clone())
            .finish(),
    )
    .expect("the phase process's subscriber");
    let recovered = recovered_with(
        assembler,
        &request,
        CONTINUING_EXECUTION,
        2,
        paused_writer(marker),
    )
    .await;
    if marker {
        let invocation =
            recovered.unwrap_or_else(|error| panic!("recover: the continuation re-runs: {error}"));
        assert_rerun(invocation, depth, mode, tool_calls, captured).await;
        return;
    }
    let cause = "pipeline.recovery.root_writer_mismatch";
    let Err(error) = recovered else {
        panic!("recover: without its paused writer a continuation is not re-run");
    };
    assert!(error.contains(cause), "recover: typed {cause}: {error}");
    // C4: the refusal is logged with ids and generations only.
    let logged = capture.text();
    let refusal = logged
        .lines()
        .find(|line| line.contains("pipeline_recovery_frontier_refused"))
        .unwrap_or_else(|| panic!("recover: the refusal is logged: {logged}"));
    assert!(
        refusal.contains(cause) && refusal.contains(CONTINUING_EXECUTION),
        "{refusal}"
    );
    for private in [
        pending["interrupt_id"].as_str().expect("interrupt id"),
        pending["tool_call_id"].as_str().expect("tool call"),
        "approve",
        "lookup_release",
        "requires approval",
    ] {
        assert!(
            !refusal.contains(private),
            "the refusal logs {private}: {refusal}"
        );
    }
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        0,
        "recover: not repeated"
    );
}

/// Run to its end: every projected frame (type and metadata) and the
/// completion's error, if any.
async fn run_frames(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> (Vec<(String, Value)>, Result<(), String>) {
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut frames = Vec::new();
    loop {
        match run.next_event().await {
            Err(error) => return (frames, Err(error.to_string())),
            Ok(Some(event)) => {
                for frame in projector.project(&event).expect("project") {
                    frames.push((
                        frame.r#type.clone(),
                        serde_json::from_slice(&frame.response_metadata).unwrap_or_default(),
                    ));
                }
            }
            Ok(None) => break,
        }
    }
    let completed = completion
        .select()
        .await
        .map(|_| ())
        .map_err(|error| error.to_string());
    (frames, completed)
}

/// The root session of the paused pipeline: the session holding B's
/// `continuing` record. Its `continuing` records and its pipeline resume
/// markers (user events), counted.
async fn root_continuation_counts(pool: &sqlx::PgPool) -> (i64, i64) {
    sqlx::query_as::<_, (i64, i64)>(
        "WITH records AS (SELECT session_id, event_payload FROM elitea_runtime.agent_session_events \
             WHERE event_payload LIKE '%elitea.direct_hitl.dispatch.v1%continuing%'), \
         root AS (SELECT DISTINCT session_id FROM records) \
         SELECT (SELECT count(*) FROM records), \
                count(*) FILTER (WHERE e.event_payload LIKE '%elitea:pipeline-resume:v1%') \
         FROM elitea_runtime.agent_session_events e JOIN root USING (session_id)",
    )
    .fetch_one(pool)
    .await
    .expect("read the root continuation events")
}

/// Set the journal to the loss point `mode` names (see
/// [`continuation_scenario`]).
async fn set_loss_point(pool: &sqlx::PgPool, depth: &str, mode: &str) {
    match mode {
        "committed" => {
            let removed = sqlx::query(
                "DELETE FROM elitea_runtime.agent_graph_checkpoints \
                 WHERE thread_id LIKE $1 AND pending_nodes LIKE '%after%'",
            )
            .bind(child_direct_thread(depth))
            .execute(pool)
            .await
            .expect("remove the row saved after the call")
            .rows_affected();
            assert_eq!(removed, 1, "the child saved one row after the call");
        }
        "undispatched" => {
            let removed = sqlx::query(
                "DELETE FROM elitea_runtime.agent_graph_checkpoints WHERE thread_id LIKE 'n1:%'",
            )
            .execute(pool)
            .await
            .expect("remove the journal")
            .rows_affected();
            assert_ne!(removed, 0, "the call's journal existed");
        }
        _ => {}
    }
}

/// FU-265 / FU-269 two-process proof: A pauses, B's approval is lost around
/// the call at `depth`, B is reclaimed; `marker` says whether Main named A as
/// the paused writer. The journal after the loss is set to `mode`, as in
/// [`child_direct_scenario`]: `started` (as the crash left it), `committed`
/// (the child's row after the call removed: lost between the journal commit
/// and the child's save), `undispatched` (the journal removed: lost before
/// the Started append).
async fn continuation_scenario(url: &str, marker: bool, depth: &str, mode: &str) {
    let db = IsolatedPostgres::create(url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let phase = |phase: &'static str, pending: &str| {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args([
                "--exact",
                CONTINUATION_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, &db.database_name)
            .env(MARKER_ENV, if marker { "1" } else { "0" })
            .env(DEPTH_ENV, depth)
            .env(MODE_ENV, mode)
            .env(PENDING_ENV, pending)
            .env(PHASE_ENV, phase);
        command
    };
    let output = run_phase("pause", phase("pause", ""));
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert_eq!(
        output.status.code(),
        Some(0),
        "pause: {stdout} {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let pending = stdout
        .lines()
        .find_map(|line| line.split_once(PENDING_LINE).map(|(_, card)| card))
        .unwrap_or_else(|| panic!("the pause phase printed its card: {stdout}"))
        .to_owned();
    let output = run_phase("crash", phase("crash", &pending));
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
        usize::from(mode != "before_runner"),
        "one journal for the one call: {journals:?}"
    );
    set_loss_point(&db.pool, depth, mode).await;
    let before = root_continuation_counts(&db.pool).await;
    // E3: with the paused writer B records `continuing` once, before its
    // Runner starts; from an older Main (no writer) it records none, as before.
    assert_eq!(
        before.0,
        i64::from(marker),
        "B's `continuing` records (E3): {before:?}"
    );
    let output = run_phase("recover", phase("recover", &pending));
    assert_eq!(
        output.status.code(),
        Some(0),
        "recover: {} {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let after = root_continuation_counts(&db.pool).await;
    // E3/E4/E6: the re-run wrote no second `continuing` record and no second
    // resume marker, and B's earlier events are still stored.
    assert_eq!(
        after.0, before.0,
        "no second `continuing` record: {after:?}"
    );
    assert!(
        after.1 <= 1 && after.1 >= before.1,
        "one resume marker at most, none removed: {before:?} {after:?}"
    );
    db.pool.close().await;
}

/// Decision 2 deviation: a response for which Main names no writer (no single
/// settled paused terminal) still resumes on its first attempt, as before
/// FU-269, and records no `continuing` record.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_continuation_without_a_named_writer_resumes_on_its_first_attempt() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-269 PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let phase = |phase: &'static str, pending: &str| {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args([
                "--exact",
                CONTINUATION_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, &db.database_name)
            .env(MARKER_ENV, "0")
            .env(DEPTH_ENV, "1")
            .env(MODE_ENV, "started")
            .env(PENDING_ENV, pending)
            .env(PHASE_ENV, phase);
        command
    };
    let output = run_phase("pause", phase("pause", ""));
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert_eq!(output.status.code(), Some(0), "pause: {stdout}");
    let pending = stdout
        .lines()
        .find_map(|line| line.split_once(PENDING_LINE).map(|(_, card)| card))
        .unwrap_or_else(|| panic!("the pause phase printed its card: {stdout}"))
        .to_owned();
    let output = run_phase("complete", phase("complete", &pending));
    assert_eq!(
        output.status.code(),
        Some(0),
        "complete: {} {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        root_continuation_counts(&db.pool).await.0,
        0,
        "no `continuing` record"
    );
    db.pool.close().await;
}

/// The matrix of a decided continuation lost before its first root save
/// (decision 1), each case with Main's paused writer.
macro_rules! continuation_case {
    ($name:ident, $depth:literal, $mode:literal) => {
        #[tokio::test(flavor = "current_thread")]
        async fn $name() {
            let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
                eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-269 PostgreSQL proof");
                return;
            };
            continuation_scenario(&url, true, $depth, $mode).await;
        }
    };
}

continuation_case!(
    postgres_an_approved_child_call_lost_before_the_continuation_saves_re_runs_and_is_not_repeated,
    "1",
    "started"
);
continuation_case!(
    postgres_a_lost_continuation_replays_a_committed_child_call,
    "1",
    "committed"
);
continuation_case!(
    postgres_a_lost_continuation_runs_an_undispatched_child_call_once,
    "1",
    "undispatched"
);
continuation_case!(
    postgres_a_lost_continuation_stops_typed_on_a_started_call_two_levels_down,
    "2",
    "started"
);
continuation_case!(
    postgres_a_lost_continuation_replays_a_committed_call_two_levels_down,
    "2",
    "committed"
);
continuation_case!(
    postgres_a_lost_continuation_runs_an_undispatched_call_two_levels_down_once,
    "2",
    "undispatched"
);

continuation_case!(
    postgres_a_lost_continuation_escalates_a_started_root_direct_call_to_reconcile,
    "0",
    "started"
);
continuation_case!(
    postgres_a_continuation_lost_before_its_runner_runs_a_root_direct_call_once,
    "0",
    "before_runner"
);
continuation_case!(
    postgres_a_continuation_lost_before_its_runner_applies_a_hitl_node_decision_once,
    "gate",
    "before_runner"
);

#[tokio::test(flavor = "current_thread")]
async fn postgres_a_continuation_without_its_paused_writer_is_refused_typed_on_reclaim() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-265 PostgreSQL proof");
        return;
    };
    continuation_scenario(&url, false, "1", "started").await;
}

// ---------------------------------------------------------------------------
// FU-264 / FU-267: a fan-out member's saved child (depth 1) or the saved
// pipeline its Agent node runs (depth 2) is lost around an effectful direct
// call; the reclaim resumes the Parallel and the member's branch journals
// decide the call.
// ---------------------------------------------------------------------------

const MEMBER_DIRECT_TEST: &str =
    "agents::pipeline_tests::fanout_effect_pg_tests::postgres_member_child_direct_process";

fn member_direct_assembler(
    pool: &sqlx::PgPool,
    depth: &str,
    then_llm: bool,
    outcomes: Vec<TestModelGatewayOutcome>,
    connector: Arc<dyn McpConnector>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let (platform, model_facade, captured) = member_child_runtime(depth, outcomes, then_llm);
    (
        PipelineNativeAgentAssembler::postgres(
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
        .with_fixed_parallel_for_tests(),
        captured,
    )
}

/// One phase of a [`member_direct_scenario`], in its own process.
#[tokio::test(flavor = "current_thread")]
async fn postgres_member_child_direct_process() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let pool = connect(&database).await;
    let depth = std::env::var(DEPTH_ENV).expect("depth");
    let mode = std::env::var(MODE_ENV).expect("mode");
    let committed = mode == "committed";
    let request = mixed_request("continue");
    if std::env::var(PHASE_ENV).expect("phase") == "crash" {
        // `committed`: the call returns and its journal commits; the process
        // dies at the child's next model request. `started`: it dies inside
        // the call, once its Started record is durable.
        let (assembler, _) = if committed {
            member_direct_assembler(
                &pool,
                &depth,
                true,
                vec![TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE)],
                pipeline_connector(),
            )
        } else {
            member_direct_assembler(
                &pool,
                &depth,
                false,
                Vec::new(),
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
        member_direct_assembler(&pool, &depth, committed, outcomes, connector);
    let invocation = recovered(&assembler, &request, EXECUTION, 2)
        .await
        .unwrap_or_else(|error| panic!("recover: the reclaim recovers: {error}"));
    run_to_end(invocation)
        .await
        .unwrap_or_else(|error| panic!("recover: the fan-out joins: {error}"));
    let entries = joined(&pool).await.expect("joined entries");
    let second = entries
        .iter()
        .find(|entry| entry["branch_id"] == "second")
        .unwrap_or_else(|| panic!("the member's entry: {entries:?}"));
    if committed {
        // R: the committed result replays; the call never runs again.
        assert_eq!(second["status"], "succeeded", "{entries:?}");
        assert_eq!(tool_calls.load(Ordering::Acquire), 0, "recover: replayed");
        assert_eq!(model_requests(&captured), 1, "recover: only the next step");
    } else {
        // C -> F: the call may have run; the branch fails typed, never repeated.
        assert_eq!(second["status"], "failed", "{entries:?}");
        assert_eq!(second["error"]["code"], MAY_HAVE_STARTED, "{entries:?}");
        assert_eq!(
            tool_calls.load(Ordering::Acquire),
            0,
            "recover: not repeated"
        );
    }
}

/// Two processes on real PostgreSQL: lose the member's call at `depth` in
/// `mode`, then reclaim. `committed` also removes the child's row saved after
/// the call: the process was lost between the journal's commit and the
/// child's next save.
async fn member_direct_scenario(url: &str, depth: &str, mode: &str) {
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
                MEMBER_DIRECT_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, &db.database_name)
            .env(DEPTH_ENV, depth)
            .env(MODE_ENV, mode)
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
    if mode == "committed" {
        let removed = sqlx::query(
            "DELETE FROM elitea_runtime.agent_graph_checkpoints \
             WHERE writer_claim_id = 'claim-1' AND pending_nodes LIKE '%\"after\"%'",
        )
        .execute(&db.pool)
        .await
        .expect("remove the row saved after the call")
        .rows_affected();
        assert_eq!(removed, 1, "the child saved one row after the call");
    }
    let output = run_phase("recover", phase("recover"));
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert_eq!(
        output.status.code(),
        Some(0),
        "recover: {stdout} {}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(
        stdout.contains("1 passed"),
        "recover ran its assertions: {stdout}"
    );
    assert_eq!(journal_threads(&db.pool).await, journals, "no new journal");
    db.pool.close().await;
}

macro_rules! member_direct_case {
    ($name:ident, $depth:literal, $mode:literal) => {
        #[tokio::test(flavor = "current_thread")]
        async fn $name() {
            let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
                eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the member direct-call proof");
                return;
            };
            member_direct_scenario(&url, $depth, $mode).await;
        }
    };
}

// FU-267: depth 1, a started call ends the member typed, never repeated.
member_direct_case!(
    postgres_a_member_child_call_lost_after_it_started_ends_the_member_typed,
    "1",
    "started"
);
member_direct_case!(
    postgres_two_levels_below_a_member_a_started_call_ends_the_member_typed,
    "2",
    "started"
);
member_direct_case!(
    postgres_two_levels_below_a_member_a_committed_call_replays_without_calling_again,
    "2",
    "committed"
);

// ---------------------------------------------------------------------------
// FU-273: a member's approved sensitive call lost inside the call on its
// wake. The reclaim must never raise a new card for the decided call: the
// call's journal decides it (Started -> may have started, 0 calls).
// ---------------------------------------------------------------------------

/// The member child's MCP tool, which hangs once called (its journal's
/// Started record is durable by then): the wake is lost inside the call.
struct HangingConnector {
    reached: Arc<tokio::sync::Notify>,
}

#[async_trait]
impl McpConnector for HangingConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(HangingTool {
                inner: PipelineMcpTool {
                    calls: Arc::new(AtomicUsize::new(0)),
                    read_only: false,
                },
                reached: Arc::clone(&self.reached),
            })],
        )))
    }
}

struct HangingTool {
    inner: PipelineMcpTool,
    reached: Arc<tokio::sync::Notify>,
}

#[async_trait]
impl Tool for HangingTool {
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
        self.reached.notify_one();
        std::future::pending::<()>().await;
        unreachable!("the wake is lost inside the call")
    }
}

/// One row of the parked member's branch thread: its checkpoint, its receipt
/// status, and whether the row owns that receipt (a receipt carried from an
/// earlier row, or none, is not the row's own).
#[derive(Debug)]
struct MemberBranchRow {
    checkpoint_id: String,
    receipt: Option<String>,
    owned: bool,
}

/// The rows, oldest first, of the branch thread that saved a pause receipt.
async fn member_branch_rows(pool: &sqlx::PgPool) -> Vec<MemberBranchRow> {
    let rows = sqlx::query_as::<_, (String, String)>(
        "SELECT checkpoint_id, metadata FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id = (SELECT thread_id FROM elitea_runtime.agent_graph_checkpoints \
           WHERE metadata LIKE '%branch-receipt.v1%' AND metadata LIKE '%\"status\":\"paused\"%' \
           LIMIT 1) \
         ORDER BY save_ordinal",
    )
    .fetch_all(pool)
    .await
    .expect("read the member branch rows");
    rows.into_iter()
        .map(|(checkpoint_id, metadata)| {
            let metadata: Value = serde_json::from_str(&metadata).expect("metadata JSON");
            let receipt = metadata["elitea.graph.parallel.branch-receipt.v1"]["status"]
                .as_str()
                .map(str::to_owned);
            let owned = metadata["elitea.graph.parallel.branch-receipt-checkpoint.v1"].as_str()
                == Some(checkpoint_id.as_str());
            MemberBranchRow {
                checkpoint_id,
                receipt,
                owned,
            }
        })
        .collect()
}

/// Main admits each fan-out wake as its own execution; its node journals are
/// keyed by it (`node_attempts.rs` journal identity).
const MEMBER_WAKE_EXECUTION: &str = "0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d";

/// What the reclaim of a lost member wake must do with the approved call.
struct MemberWakeOutcome {
    cards: Vec<Value>,
    second: Option<Value>,
    /// Tool calls the reclaim made.
    calls: usize,
    /// Model requests the reclaim made.
    model_requests: usize,
}

/// FU-273 at `depth`: park the member's sensitive call, approve it, lose the
/// wake under its own execution, reclaim it. `mode` is where it was lost:
/// - `started`: inside the approved call, its journal's Started record durable;
/// - `undispatched`: as `started`, with the journal removed (lost before the
///   Started append);
/// - `committed`: at the model step after the call; the member child's row
///   saved after the call is removed (lost between the journal's commit and
///   the child's save).
async fn member_wake_lost_on_the_approved_call(
    url: &str,
    depth: &str,
    mode: &str,
) -> MemberWakeOutcome {
    let db = IsolatedPostgres::create(url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let rig = MemberWakeRig {
        pool: db.pool.clone(),
        ledger: crate::agents::graph::fanout_test_ledger::TestLedger::new(),
        depth,
        committed: mode == "committed",
    };
    let request = mixed_request("continue");
    let (parking, _) = rig.assembler(
        1,
        counting_connector(&Arc::new(AtomicUsize::new(0))),
        Vec::new(),
    );
    let invocation = parking
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .unwrap_or_else(|error| panic!("the member's card parks the run: {error}"));
    assert_eq!(cards.len(), 1, "{cards:?}");
    let key = cards[0]["interrupt_key"]
        .as_str()
        .expect("card key")
        .to_owned();
    rig.ledger.decide(&key, "approve", None);
    let wake = member_wake_request(request);
    rig.lose(&wake).await;
    assert_eq!(
        journal_threads(&db.pool).await.len(),
        1,
        "the call's journal"
    );
    let pause_checkpoint = lost_attempt_pause(&db.pool).await;
    narrow_lost_wake(&db.pool, mode).await;
    // The reclaim of the wake.
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let outcomes = if rig.committed {
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "summarized",
        ))]
    } else {
        Vec::new()
    };
    let (reclaim, captured) = rig.assembler(3, counting_connector(&tool_calls), outcomes);
    let invocation = recovered(&reclaim, &wake, MEMBER_WAKE_EXECUTION, 3)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .unwrap_or_else(|error| panic!("the recovered wake ends: {error}"));
    let second = joined(&db.pool).await.and_then(|entries| {
        entries
            .into_iter()
            .find(|entry| entry["branch_id"] == "second")
    });
    let settled = if mode == "started" {
        "failed"
    } else {
        "completed"
    };
    assert_settled_once(&db.pool, &rig.ledger, key, &pause_checkpoint, settled).await;
    db.pool.close().await;
    MemberWakeOutcome {
        cards,
        second,
        calls: tool_calls.load(Ordering::Acquire),
        model_requests: model_requests(&captured),
    }
}

/// The FU-273 run's fixed parts.
struct MemberWakeRig<'a> {
    pool: sqlx::PgPool,
    ledger: Arc<crate::agents::graph::fanout_test_ledger::TestLedger>,
    depth: &'a str,
    /// The member child runs a model step after the call (`committed`).
    committed: bool,
}

impl MemberWakeRig<'_> {
    fn assembler(
        &self,
        claim: u64,
        connector: Arc<dyn McpConnector>,
        outcomes: Vec<TestModelGatewayOutcome>,
    ) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
        let (platform, model_facade, captured) =
            member_child_runtime(self.depth, outcomes, self.committed);
        let assembler = PipelineNativeAgentAssembler::postgres(
            self.pool.clone(),
            SessionLimits::default(),
            CheckpointLimits::default(),
            sensitive_pipeline_mcp_policy(),
            platform,
            model_facade,
        )
        .with_mcp_connector(connector)
        .with_fixed_parallel_for_tests()
        .with_fanout_decisions_for_tests(self.ledger.client(claim));
        (assembler, captured)
    }

    /// The wake, under its own execution, lost inside the approved call or,
    /// `committed`, at the member child's model step after it.
    async fn lose(&self, wake: &super::super::request::AgentExecutionRequest) {
        if self.committed {
            let lost_calls = Arc::new(AtomicUsize::new(0));
            let (lost, lost_requests) = self.assembler(
                2,
                counting_connector(&lost_calls),
                vec![TestModelGatewayOutcome::Pending],
            );
            let invocation = lost
                .assemble(claim_for(wake, MEMBER_WAKE_EXECUTION, 2, false))
                .await
                .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
            lose_at_first_model_request(invocation, &lost_requests).await;
            assert_eq!(lost_calls.load(Ordering::Acquire), 1, "the call ran once");
            return;
        }
        let reached = Arc::new(tokio::sync::Notify::new());
        let (lost, _) = self.assembler(
            2,
            Arc::new(HangingConnector {
                reached: Arc::clone(&reached),
            }),
            Vec::new(),
        );
        let invocation = lost
            .assemble(claim_for(wake, MEMBER_WAKE_EXECUTION, 2, false))
            .await
            .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
        let run = run_collecting(invocation);
        tokio::pin!(run);
        tokio::select! {
            outcome = &mut run => panic!("the wake ended before its call: {outcome:?}"),
            () = reached.notified() => {}
            () = tokio::time::sleep(Duration::from_secs(30)) => panic!("the call never started"),
        }
    }
}

fn counting_connector(tool_calls: &Arc<AtomicUsize>) -> Arc<dyn McpConnector> {
    Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(tool_calls),
        read_only: false,
    })
}

/// The fan-out wake of the parked `request`.
fn member_wake_request(
    request: super::super::request::AgentExecutionRequest,
) -> super::super::request::AgentExecutionRequest {
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
    wake
}

/// E3: the lost attempt saved rows after the member's pause, none with a
/// receipt of its own. The reclaim keeps the member parked at that pause and
/// re-delivers its decision; those rows are never a frontier it continues
/// from without the decision. Returns the pause row.
async fn lost_attempt_pause(pool: &sqlx::PgPool) -> String {
    let before = member_branch_rows(pool).await;
    let pause = before
        .iter()
        .rposition(|row| row.owned)
        .expect("the member's own pause row");
    assert_eq!(
        before[pause].receipt.as_deref(),
        Some("paused"),
        "{before:?}"
    );
    assert!(
        pause + 1 < before.len(),
        "the lost attempt's rows: {before:?}"
    );
    before[pause].checkpoint_id.clone()
}

/// Narrow the loss to `mode`, as FU-248's crash matrix does.
async fn narrow_lost_wake(pool: &sqlx::PgPool, mode: &str) {
    match mode {
        "committed" => {
            let removed = sqlx::query(
                "DELETE FROM elitea_runtime.agent_graph_checkpoints \
                 WHERE thread_id LIKE 'p1:%/%' AND pending_nodes LIKE '%\"after\"%'",
            )
            .execute(pool)
            .await
            .expect("remove the row saved after the call")
            .rows_affected();
            assert_eq!(removed, 1, "the member child saved one row after the call");
        }
        "undispatched" => {
            let removed = sqlx::query(
                "DELETE FROM elitea_runtime.agent_graph_checkpoints WHERE thread_id LIKE 'n1:%'",
            )
            .execute(pool)
            .await
            .expect("remove the journal")
            .rows_affected();
            assert_ne!(removed, 0, "the call's journal existed");
        }
        _ => {}
    }
}

/// E3: no pause after the decided one; the member ends on its own `settled`
/// receipt. E2: the decision is acknowledged once, only with that proof from the
/// member's own rows.
async fn assert_settled_once(
    pool: &sqlx::PgPool,
    ledger: &crate::agents::graph::fanout_test_ledger::TestLedger,
    key: String,
    pause_checkpoint: &str,
    settled: &str,
) {
    let after = member_branch_rows(pool).await;
    let owned = after.iter().filter(|row| row.owned).collect::<Vec<_>>();
    assert_eq!(
        owned.len(),
        2,
        "the pause and the settlement only: {after:?}"
    );
    assert_eq!(owned[0].checkpoint_id, pause_checkpoint, "{after:?}");
    assert_eq!(owned[1].receipt.as_deref(), Some(settled), "{after:?}");
    assert_eq!(
        ledger.acks(),
        vec![(
            key,
            elitea_agent_runtime::graph::fanout::runner::AckResult::Applied {
                child_checkpoint_id: owned[1].checkpoint_id.clone(),
            }
        )],
        "{after:?}"
    );
    assert_eq!(ledger.open_decisions(), 0);
}

macro_rules! member_wake_case {
    ($name:ident, $depth:literal, $mode:literal) => {
        #[tokio::test(flavor = "current_thread")]
        async fn $name() {
            let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
                eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-273 proof");
                return;
            };
            let outcome = member_wake_lost_on_the_approved_call(&url, $depth, $mode).await;
            assert!(
                outcome.cards.is_empty(),
                "no card raised again: {:?}",
                outcome.cards
            );
            let second = outcome.second.expect("the member's joined entry");
            match $mode {
                // C -> F: the call may have run; never repeated, typed.
                "started" => {
                    assert_eq!(outcome.calls, 0, "the approved call is never repeated");
                    assert_eq!(second["status"], "failed", "{second}");
                    assert_eq!(second["error"]["code"], MAY_HAVE_STARTED, "{second}");
                }
                // I: no Started record; the call runs once.
                "undispatched" => {
                    assert_eq!(outcome.calls, 1, "runs once");
                    assert_eq!(second["status"], "succeeded", "{second}");
                }
                // R: the committed result replays; only the next step runs.
                "committed" => {
                    assert_eq!(outcome.calls, 0, "replayed, never called again");
                    assert_eq!(outcome.model_requests, 1, "only the next step");
                    assert_eq!(second["status"], "succeeded", "{second}");
                }
                other => panic!("unknown mode {other}"),
            }
        }
    };
}

member_wake_case!(
    postgres_a_member_wake_lost_inside_its_approved_call_never_asks_again,
    "1",
    "started"
);
member_wake_case!(
    postgres_two_levels_below_a_member_a_wake_lost_inside_its_approved_call_never_asks_again,
    "2",
    "started"
);
member_wake_case!(
    postgres_a_member_wake_lost_before_its_approved_call_runs_it_once,
    "1",
    "undispatched"
);
member_wake_case!(
    postgres_two_levels_below_a_member_a_wake_lost_before_its_approved_call_runs_it_once,
    "2",
    "undispatched"
);
member_wake_case!(
    postgres_a_member_wake_lost_after_its_approved_call_committed_replays_it,
    "1",
    "committed"
);
member_wake_case!(
    postgres_two_levels_below_a_member_a_wake_lost_after_its_approved_call_committed_replays_it,
    "2",
    "committed"
);

/// FU-273 R1: a pipeline the member's pause names with no row left is a
/// broken state, not progress. The reclaim refuses it typed; it never
/// re-asks and never calls again.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_member_whose_paused_pipeline_has_no_row_is_refused_typed() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-273 proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let rig = MemberWakeRig {
        pool: db.pool.clone(),
        ledger: crate::agents::graph::fanout_test_ledger::TestLedger::new(),
        depth: "1",
        committed: false,
    };
    let request = mixed_request("continue");
    let (parking, _) = rig.assembler(
        1,
        counting_connector(&Arc::new(AtomicUsize::new(0))),
        Vec::new(),
    );
    let invocation = parking
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .unwrap_or_else(|error| panic!("the member's card parks the run: {error}"));
    let key = cards[0]["interrupt_key"].as_str().expect("card key");
    rig.ledger.decide(key, "approve", None);
    let wake = member_wake_request(request);
    rig.lose(&wake).await;
    lost_attempt_pause(&db.pool).await;
    let removed = sqlx::query(
        "DELETE FROM elitea_runtime.agent_graph_checkpoints WHERE thread_id LIKE \
         (SELECT thread_id FROM elitea_runtime.agent_graph_checkpoints \
          WHERE metadata LIKE '%branch-receipt.v1%' AND metadata LIKE '%\"status\":\"paused\"%' \
          LIMIT 1) || '/%'",
    )
    .execute(&db.pool)
    .await
    .expect("remove the member's paused pipeline rows")
    .rows_affected();
    assert_ne!(removed, 0, "the member's pipeline had rows");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let (reclaim, _) = rig.assembler(3, counting_connector(&tool_calls), Vec::new());
    let outcome = match recovered(&reclaim, &wake, MEMBER_WAKE_EXECUTION, 3).await {
        Ok(invocation) => run_collecting(invocation).await.map(|(_, cards)| cards),
        Err(error) => Err(error),
    };
    let error = outcome.expect_err("the broken member state ends the run typed");
    assert!(error.contains("graph.parallel.corrupt_receipt"), "{error}");
    assert_eq!(tool_calls.load(Ordering::Acquire), 0, "never called again");
    assert_eq!(
        rig.ledger.open_decisions(),
        1,
        "the decision is not consumed"
    );
    db.pool.close().await;
}

/// T-2A3: one saved Agent node run as three concurrent Map items. The node has
/// one nested-event channel; each item waits for it instead of failing, a
/// paused item releases it so the next one starts, and all three pause with
/// their own card, and the run parks. Answered in reverse order, the wake
/// resumes every item with its own answer and the Map joins three successes. Production assembler, real
/// PostgreSQL; before the fix two of the three items failed at start.
#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One park-then-wake scenario, its two runs and their checks together.
async fn postgres_map_saved_agent_items_run_concurrently_configured_and_all_pause() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the Map saved-Agent PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (platform, model_facade, captured) = gated_saved_runtime(
        vec![
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response("item done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("item done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("item done")),
        ],
        asking_agent(),
    );
    let connector = || -> Arc<dyn McpConnector> {
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        })
    };
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] = json!(
        "state:\n  topics: {type: list, value: [alpha, beta, gamma]}\n  answer: str\n  joined: list\n  messages: list\nentry_point: ask_each\nnodes:\n  - id: ask_each\n    type: map\n    worker: asker\n    source: topics\n    item: topic\n    index: topic_index\n    outputs: [answer]\n    destination: joined\n    max_items: 3\n    max_concurrency: 3\n    on_error: continue\n    join: append\n    transition: END\n  - id: asker\n    type: agent\n    tool: release-agent\n    input: [topic]\n    input_mapping:\n      task: {type: variable, value: topic}\n    output: [answer]\n"
    );
    let (assembler, _) = assembler_on(
        &db.pool,
        (
            Arc::clone(&platform),
            Arc::clone(&model_facade),
            captured.clone(),
        ),
        connector(),
    );
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the Map pipeline");
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("three undecided cards park the run");
    assert_eq!(cards.len(), 3, "{cards:?}");
    let members = cards
        .iter()
        .map(|card| {
            card["fanout_v1"]["member"]
                .as_str()
                .expect("member")
                .to_owned()
        })
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(members, ["0", "1", "2"].map(str::to_owned).into());
    assert_eq!(model_requests(&captured), 3, "each item asked once");
    // Answered last first.
    for card in cards.iter().rev() {
        assert_eq!(card["kind"], "ask_user", "{card}");
        let member = card["fanout_v1"]["member"].as_str().expect("member");
        ledger.decide(
            card["interrupt_key"].as_str().expect("card key"),
            "answer",
            Some(&format!("answer-{member}")),
        );
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
    let (assembler, _) = assembler_on(
        &db.pool,
        (platform, model_facade, captured.clone()),
        connector(),
    );
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .expect("assemble the wake");
    run_collecting(invocation)
        .await
        .expect("the wake resumes every answered item and the Map joins");
    assert_eq!(model_requests(&captured), 6);
    let resumed = captured
        .lock()
        .expect("captured model requests")
        .iter()
        .skip(3)
        .map(|request| String::from_utf8_lossy(&request.body).into_owned())
        .collect::<Vec<_>>();
    for member in ["0", "1", "2"] {
        assert!(
            resumed
                .iter()
                .any(|body| body.contains(&format!("answer-{member}"))),
            "item {member} resumed with its own answer"
        );
    }
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for (index, entry) in entries.iter().enumerate() {
        assert_eq!(entry["index"], index, "{entries:?}");
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
    db.pool.close().await;
}

/// A Map of one saved Agent over `topics` at `concurrency`, landing `joined`.
fn saved_agent_map_yaml(topics: &[String], concurrency: usize) -> String {
    format!(
        "state:\n  topics: {{type: list, value: [{}]}}\n  answer: str\n  joined: list\n  messages: list\nentry_point: ask_each\nnodes:\n  - id: ask_each\n    type: map\n    worker: asker\n    source: topics\n    item: topic\n    index: topic_index\n    outputs: [answer]\n    destination: joined\n    max_items: {}\n    max_concurrency: {concurrency}\n    on_error: continue\n    join: append\n    transition: END\n  - id: asker\n    type: agent\n    tool: release-agent\n    input: [topic]\n    input_mapping:\n      task: {{type: variable, value: topic}}\n    output: [answer]\n",
        topics.join(", "),
        topics.len(),
    )
}

/// T-2A3: sixteen items of one saved Agent at concurrency 8 each run as their
/// own invocation and complete. The model answers by the item's own topic, so
/// an item whose request carried another item's input, or whose result landed
/// under another index, fails the check: no state crosses items.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_sixteen_item_saved_agent_map_completes_without_cross_item_state() {
    saved_agent_map_without_cross_item_state(16).await;
}

/// The same past Parallel's 16-member bound: a saved-Agent Map of 24 items
/// gives every item its own member event scope (T-2A3; items 16 and up were
/// refused `graph.invalid` before).
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_saved_agent_map_of_more_than_sixteen_items_completes_every_item() {
    saved_agent_map_without_cross_item_state(24).await;
}

async fn saved_agent_map_without_cross_item_state(items: usize) {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the Map saved-Agent PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let topics = (0..items)
        .map(|index| format!("topic-q{index:02}x"))
        .collect::<Vec<_>>();
    let outcomes = topics
        .iter()
        .map(|topic| TestModelGatewayOutcome::ResponseForRequest {
            marker: Box::leak(topic.clone().into_boxed_str()),
            response: pipeline_text_response(&format!("answer for {topic}")),
        })
        .collect();
    let (assembler, captured) = gated_assembler(
        &db.pool,
        outcomes,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_agent(),
    );
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] =
        json!(saved_agent_map_yaml(&topics, 8));
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the Map pipeline");
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("every item completes and the Map joins");
    assert!(cards.is_empty(), "{cards:?}");
    let entries = joined(&db.pool).await.expect("joined entries");
    let failed = entries
        .iter()
        .filter(|entry| entry["status"] != "succeeded")
        .map(|entry| (entry["index"].clone(), entry["error"].clone()))
        .collect::<Vec<_>>();
    assert!(failed.is_empty(), "{failed:?}");
    assert_eq!(model_requests(&captured), items);
    for body in captured
        .lock()
        .expect("captured model requests")
        .iter()
        .map(|request| String::from_utf8_lossy(&request.body).into_owned())
    {
        let carried = topics
            .iter()
            .filter(|topic| body.contains(topic.as_str()))
            .count();
        assert_eq!(
            carried, 1,
            "one item's request carried exactly its own input"
        );
    }
    assert_eq!(entries.len(), items, "{entries:?}");
    for (index, (entry, topic)) in entries.iter().zip(&topics).enumerate() {
        assert_eq!(entry["index"], index, "{entries:?}");
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
        assert_eq!(
            entry["outputs"]["answer"],
            format!("answer for {topic}"),
            "{entries:?}"
        );
    }
    db.pool.close().await;
}

/// T-2A3: a fixed Parallel with the same saved Agent in two branches running
/// at once: both pause with their own card (each member its own invocation).
#[tokio::test(flavor = "current_thread")]
async fn postgres_parallel_branches_of_the_same_saved_agent_both_pause_concurrently() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the Parallel saved-Agent PostgreSQL proof"
        );
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (assembler, captured) = gated_assembler(
        &db.pool,
        vec![
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
        ],
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_agent(),
    );
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] = json!(
        "state:\n  left: str\n  right: str\n  joined: list\n  messages: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{id: left, node: ask_left}, {id: right, node: ask_right}]\n    max_concurrency: 2\n    wait: all\n    on_error: continue\n    output: [joined]\n    transition: END\n  - id: ask_left\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {type: fixed, value: 'Ask about the left side'}\n    output: [left]\n  - id: ask_right\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {type: fixed, value: 'Ask about the right side'}\n    output: [right]\n"
    );
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the Parallel pipeline");
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("both branches pause and the run parks");
    assert_eq!(model_requests(&captured), 2);
    let members = cards
        .iter()
        .map(|card| {
            card["fanout_v1"]["member"]
                .as_str()
                .expect("member")
                .to_owned()
        })
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(
        members,
        ["left", "right"].map(str::to_owned).into(),
        "{cards:?}"
    );
    db.pool.close().await;
}

/// T-2A3 measurement (not a gate): a 64-item saved-Agent Map at `concurrency`
/// through the production assembler. Run one per process under
/// `/usr/bin/time -l` to compare peak resident memory and wall time.
async fn measure_saved_agent_map(concurrency: usize) {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the Map measurement");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let topics = (0..64)
        .map(|index| format!("topic-q{index:02}x"))
        .collect::<Vec<_>>();
    let outcomes = topics
        .iter()
        .map(|topic| TestModelGatewayOutcome::ResponseForRequest {
            marker: Box::leak(topic.clone().into_boxed_str()),
            response: pipeline_text_response(&format!("answer for {topic}")),
        })
        .collect();
    let (assembler, captured) = gated_assembler(
        &db.pool,
        outcomes,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_agent(),
    );
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] =
        json!(saved_agent_map_yaml(&topics, concurrency));
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let started = std::time::Instant::now();
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the Map pipeline");
    run_collecting(invocation).await.expect("the Map joins");
    let elapsed = started.elapsed();
    let entries = joined(&db.pool).await.expect("joined");
    let failed = entries
        .iter()
        .filter(|entry| entry["status"] != "succeeded")
        .map(|entry| entry["error"]["code"].to_string())
        .collect::<Vec<_>>();
    eprintln!("T2A3_MAP_FAILED {failed:?}");
    assert_eq!(model_requests(&captured), 64);
    assert_eq!(entries.len(), 64);
    eprintln!(
        "T2A3_MAP_MEASURE concurrency={concurrency} items=64 elapsed_ms={} per_item_ms={:.2}",
        elapsed.as_millis(),
        elapsed.as_secs_f64() * 1_000.0 / 64.0
    );
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
#[ignore = "measurement: run alone under /usr/bin/time -l"]
async fn measure_a_64_item_saved_agent_map_at_concurrency_8() {
    measure_saved_agent_map(8).await;
}

#[tokio::test(flavor = "current_thread")]
#[ignore = "measurement: run alone under /usr/bin/time -l"]
async fn measure_a_64_item_saved_agent_map_at_concurrency_1() {
    measure_saved_agent_map(1).await;
}

/// A saved Agent that delegates to a nested saved Agent (application 5,
/// version 6, the tool `elitea_agent_5_v_6`).
fn delegating_agent() -> Value {
    let mut agent = effectful_agent();
    agent["instructions"] = json!("Delegate the topic to the helper and report its answer.");
    agent["tools"] = json!([{
        "id": 46, "type": "application", "name": "helper-agent",
        "description": "Answers one topic.", "author_id": 11,
        "settings": {"application_id": 5, "application_version_id": 6},
        "meta": {}, "created_at": "2026-08-21T10:00:00Z", "toolkit_name": "helper-agent",
        "author": null, "agent_type": "agent", "online": null, "icon_meta": null,
        "variables": [], "is_pinned": false, "indexes_count": null
    }]);
    agent
}

/// The nested saved Agent: no tools, answers in text.
fn helper_agent() -> Value {
    let mut agent = effectful_agent();
    agent["instructions"] = json!("Answer the topic you are given.");
    agent["tools"] = json!([]);
    agent
}

/// The outer Agent calls the nested saved Agent once with `task`.
fn nested_agent_call_response(call_id: &str, task: &str) -> Response<Body> {
    let arguments = serde_json::to_string(&json!({"task": task})).expect("arguments");
    let raw = format!(
        "data: {{\"choices\":[{{\"delta\":{{\"tool_calls\":[{{\"index\":0,\"id\":{call_id:?},\"type\":\"function\",\"function\":{{\"name\":\"elitea_agent_5_v_6\",\"arguments\":{arguments:?}}}}}]}},\"finish_reason\":null}}]}}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: {{\"choices\":[],\"usage\":{{\"prompt_tokens\":3,\"completion_tokens\":2}}}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

/// T-2A3: three concurrent Map items of a saved Agent that itself calls a
/// nested saved Agent. The nested Agent was built once with the node's
/// channel; inside each item its events go to that item's own channel. All
/// three items complete with their own answers, and every nested event is
/// projected under the item that ran it.
#[tokio::test(flavor = "current_thread")]
async fn postgres_map_items_of_a_saved_agent_with_a_nested_saved_agent_keep_their_own_events() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the Map nested-Agent PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let topics = (0..3)
        .map(|index| format!("topic-q{index:02}x"))
        .collect::<Vec<_>>();
    // Per topic, in order: the outer Agent calls the helper, the helper
    // answers, the outer Agent reports. Each request carries its own topic.
    let mut outcomes = Vec::new();
    for (index, topic) in topics.iter().enumerate() {
        let marker: &'static str = Box::leak(topic.clone().into_boxed_str());
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker,
            response: nested_agent_call_response(&format!("call_item_{index}"), topic),
        });
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker,
            response: pipeline_text_response(&format!("nested answer for {topic}")),
        });
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker,
            response: pipeline_text_response(&format!("outer answer for {topic}")),
        });
    }
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    let runtime = saved_runtime_on(
        outcomes,
        delegating_agent(),
        Some(helper_agent()),
        None,
        config,
    );
    let (assembler, captured) = assembler_on(
        &db.pool,
        runtime,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
    );
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] =
        json!(saved_agent_map_yaml(&topics, 3));
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let mut invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the Map pipeline");
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    // Every frame that carries a nested answer, with its member tier ordinal.
    let mut nested = Vec::new();
    while let Some(event) = run.next_event().await.expect("the run streams") {
        for frame in projector.project(&event).expect("projected frames") {
            let text = String::from_utf8_lossy(&frame.content).into_owned();
            if text.contains("nested answer for") {
                let metadata: Value =
                    serde_json::from_slice(&frame.response_metadata).unwrap_or_default();
                nested.push((text, metadata["parent_agent_path"].clone()));
            }
        }
    }
    completion.select().await.expect("the Map completes");
    assert_eq!(model_requests(&captured), 9);
    assert!(
        !nested.is_empty(),
        "the nested Agents' answers stream to the run"
    );
    for (text, path) in &nested {
        let topic = topics
            .iter()
            .position(|topic| text.contains(topic.as_str()))
            .expect("the nested answer names its topic");
        assert_eq!(
            path[0]["sibling_ordinal"],
            topic + 1,
            "a nested event lands under the item that ran it: {text} {path}"
        );
    }
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for (index, (entry, topic)) in entries.iter().zip(&topics).enumerate() {
        assert_eq!(entry["index"], index, "{entries:?}");
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
        assert_eq!(
            entry["outputs"]["answer"],
            format!("outer answer for {topic}"),
            "{entries:?}"
        );
    }
    db.pool.close().await;
}

/// A saved Agent that delegates to a nested saved pipeline (application 5,
/// version 6, the tool `elitea_agent_5_v_6`).
fn pipeline_delegating_agent() -> Value {
    let mut agent = delegating_agent();
    agent["instructions"] = json!("Delegate the topic to the helper flow and report its answer.");
    agent["tools"][0]["name"] = json!("helper-flow");
    agent["tools"][0]["toolkit_name"] = json!("helper-flow");
    agent["tools"][0]["agent_type"] = json!("pipeline");
    agent
}

/// The nested saved pipeline: one LLM node answers the topic it is given.
fn helper_pipeline() -> Value {
    let mut pipeline = effectful_agent();
    pipeline["agent_type"] = json!("pipeline");
    pipeline["tools"] = json!([]);
    pipeline["instructions"] = json!(
        "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    input: [input]\n    input_mapping:\n      system: {type: fixed, value: 'Answer the topic you are given.'}\n      task: {type: variable, value: input}\n    output: [answer]\n    transition: END\n"
    );
    pipeline
}

/// A Parallel of two branches whose Agent nodes run the same saved Agent.
const SAVED_AGENT_PARALLEL_YAML: &str = "state:\n  left: str\n  right: str\n  joined: list\n  messages: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{id: left, node: ask_left}, {id: right, node: ask_right}]\n    max_concurrency: 2\n    wait: all\n    on_error: continue\n    output: [joined]\n    transition: END\n  - id: ask_left\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {type: fixed, value: 'topic-left'}\n    output: [left]\n  - id: ask_right\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {type: fixed, value: 'topic-right'}\n    output: [right]\n";

/// The nested saved pipeline with a Code node after its LLM node.
fn helper_pipeline_with_code() -> Value {
    let mut pipeline = helper_pipeline();
    pipeline["instructions"] = json!(
        "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    input: [input]\n    input_mapping:\n      system: {type: fixed, value: 'Answer the topic you are given.'}\n      task: {type: variable, value: input}\n    output: [answer]\n    transition: tidy\n  - id: tidy\n    type: code\n    code: 'answer'\n    input: [answer]\n    output: [answer]\n    transition: END\n"
    );
    pipeline
}

async fn nested_tool_threads(pool: &sqlx::PgPool) -> Vec<String> {
    sqlx::query_scalar(
        "SELECT thread_id FROM elitea_runtime.agent_graph_checkpoint_writers \
         WHERE thread_id LIKE '%/pt1:%' ORDER BY thread_id",
    )
    .fetch_all(pool)
    .await
    .expect("nested pipeline threads")
}

/// FU-291 D-7 (T5): a member whose saved Agent's pipeline tool holds a Code
/// node is refused before the run, by name, with the precise registered
/// code and its cause; no model call, no nested thread.
async fn assert_member_with_a_code_pipeline_tool_is_refused(yaml: String) {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-291 refusal proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let runtime = saved_runtime_on(
        Vec::new(),
        pipeline_delegating_agent(),
        Some(helper_pipeline_with_code()),
        None,
        test_model_gateway_config(),
    );
    let (assembler, captured) = assembler_on(
        &db.pool,
        runtime,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
    );
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] = json!(yaml);
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let Err(error) = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
    else {
        panic!("a member whose pipeline tool holds Code was assembled");
    };
    let cause = error.cause().expect("a typed cause");
    assert_eq!(
        cause.code(),
        elitea_faults::codes::GRAPH_FANOUT_AGENT_TOOL_PIPELINE_UNSUPPORTED.as_str()
    );
    assert_eq!(cause.detail(), Some("fanout_agent_tool_pipeline_code"));
    assert_eq!(model_requests(&captured), 0, "nothing ran");
    assert_eq!(nested_tool_threads(&db.pool).await, Vec::<String>::new());
    db.pool.close().await;
}

/// FU-291 T5: a Map whose item Agent's pipeline tool holds Code is refused.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_map_item_agent_with_a_code_pipeline_tool_is_refused_readably() {
    let topics = (0..3)
        .map(|index| format!("topic-p{index:02}x"))
        .collect::<Vec<_>>();
    assert_member_with_a_code_pipeline_tool_is_refused(saved_agent_map_yaml(&topics, 3)).await;
}

/// FU-291 T5: a Parallel branch whose Agent's pipeline tool holds Code is
/// refused the same way.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_parallel_branch_agent_with_a_code_pipeline_tool_is_refused_readably() {
    assert_member_with_a_code_pipeline_tool_is_refused(SAVED_AGENT_PARALLEL_YAML.to_owned()).await;
}

/// Per topic, in order: the member's Agent calls the pipeline tool, the
/// pipeline's LLM node answers, the Agent reports. Each request carries its
/// own topic.
fn pipeline_member_outcomes(topics: &[String]) -> Vec<TestModelGatewayOutcome> {
    let mut outcomes = Vec::new();
    for (index, topic) in topics.iter().enumerate() {
        let marker: &'static str = Box::leak(topic.clone().into_boxed_str());
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker,
            response: nested_agent_call_response(&format!("call_item_{index}"), topic),
        });
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker,
            response: pipeline_text_response(&format!("nested answer for {topic}")),
        });
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker,
            response: pipeline_text_response(&format!("outer answer for {topic}")),
        });
    }
    outcomes
}

/// Run `yaml` whose members' saved Agent calls the helper pipeline; returns
/// the model request count and the nested texts each projected frame carried.
async fn run_pipeline_members(
    db: &IsolatedPostgres,
    topics: &[String],
    yaml: String,
) -> (usize, Vec<(String, Value)>) {
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    let runtime = saved_runtime_on(
        pipeline_member_outcomes(topics),
        pipeline_delegating_agent(),
        Some(helper_pipeline()),
        None,
        config,
    );
    let (assembler, captured) = assembler_on(
        &db.pool,
        runtime,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
    );
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] = json!(yaml);
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let mut invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the fan-out pipeline");
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut nested = Vec::new();
    while let Some(event) = run.next_event().await.expect("the run streams") {
        for frame in projector.project(&event).expect("projected frames") {
            let text = String::from_utf8_lossy(&frame.content).into_owned();
            if text.contains("nested answer for") {
                let metadata: Value =
                    serde_json::from_slice(&frame.response_metadata).unwrap_or_default();
                nested.push((text, metadata["parent_agent_path"].clone()));
            }
        }
    }
    completion.select().await.expect("the fan-out completes");
    (model_requests(&captured), nested)
}

/// FU-291: three concurrent Map items of a saved Agent that calls a saved
/// pipeline as a tool. Each call runs on its own durable nested thread under
/// its item (one writer per item, distinct), every nested answer streams
/// under the item that ran it, and every item lands with its own answer.
#[tokio::test(flavor = "current_thread")]
async fn postgres_map_items_of_a_saved_agent_calling_a_saved_pipeline_run_on_their_own_nested_threads()
 {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-291 Map proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let topics = (0..3)
        .map(|index| format!("topic-r{index:02}x"))
        .collect::<Vec<_>>();
    let (requests, nested) =
        run_pipeline_members(&db, &topics, saved_agent_map_yaml(&topics, 3)).await;
    assert_eq!(
        requests, 9,
        "each item: its call, the nested answer, its report"
    );
    assert!(!nested.is_empty(), "the nested answers stream to the run");
    for (text, path) in &nested {
        let topic = topics
            .iter()
            .position(|topic| text.contains(topic.as_str()))
            .expect("the nested answer names its topic");
        assert_eq!(
            path[0]["sibling_ordinal"],
            topic + 1,
            "a nested event lands under the item that ran it: {text} {path}"
        );
    }
    let threads = nested_tool_threads(&db.pool).await;
    assert_eq!(threads.len(), 3, "one nested thread per item: {threads:?}");
    let agents = threads
        .iter()
        .map(|thread| thread.split_once("/pt1:").expect("nested").0.to_owned())
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(agents.len(), 3, "each under its own item: {threads:?}");
    assert!(
        agents.iter().all(|agent| agent.starts_with("m1:")),
        "{agents:?}"
    );
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for (index, (entry, topic)) in entries.iter().zip(&topics).enumerate() {
        assert_eq!(entry["index"], index, "{entries:?}");
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
        assert_eq!(
            entry["outputs"]["answer"],
            format!("outer answer for {topic}"),
            "{entries:?}"
        );
    }
    db.pool.close().await;
}

/// FU-291: the same for a Parallel of two branches of one saved Agent.
#[tokio::test(flavor = "current_thread")]
async fn postgres_parallel_branches_of_a_saved_agent_calling_a_saved_pipeline_run_on_their_own_nested_threads()
 {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-291 Parallel proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let topics = ["topic-left".to_owned(), "topic-right".to_owned()];
    let (requests, nested) =
        run_pipeline_members(&db, &topics, SAVED_AGENT_PARALLEL_YAML.to_owned()).await;
    assert_eq!(
        requests, 6,
        "each branch: its call, the nested answer, its report"
    );
    assert!(!nested.is_empty(), "the nested answers stream to the run");
    let threads = nested_tool_threads(&db.pool).await;
    assert_eq!(
        threads.len(),
        2,
        "one nested thread per branch: {threads:?}"
    );
    assert!(
        threads.iter().all(|thread| thread.starts_with("p1:")),
        "{threads:?}"
    );
    db.pool.close().await;
}

/// FU-291 scope: a saved pipeline used directly as the Map worker is not
/// affected. Three items each run the pipeline's LLM node with their own
/// topic and land in source order.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_saved_pipeline_map_worker_still_runs_every_item() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the saved-pipeline worker proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let topics = (0..3)
        .map(|index| format!("topic-w{index:02}x"))
        .collect::<Vec<_>>();
    let outcomes = topics
        .iter()
        .map(|topic| TestModelGatewayOutcome::ResponseForRequest {
            marker: Box::leak(topic.clone().into_boxed_str()),
            response: pipeline_text_response(&format!("flow answer for {topic}")),
        })
        .collect();
    let runtime = saved_runtime_on(
        outcomes,
        helper_pipeline(),
        None,
        None,
        test_model_gateway_config(),
    );
    let (assembler, captured) = assembler_on(
        &db.pool,
        runtime,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
    );
    let mut request = request_for("continue", "pipeline");
    request.payload.application["version_details"]["instructions"] =
        json!(saved_agent_map_yaml(&topics, 3));
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("a saved pipeline worker assembles");
    run_collecting(invocation).await.expect("the Map joins");
    assert_eq!(model_requests(&captured), 3);
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for (index, (entry, topic)) in entries.iter().zip(&topics).enumerate() {
        assert_eq!(entry["index"], index, "{entries:?}");
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
        assert!(
            entry["outputs"]["answer"]
                .to_string()
                .contains(&format!("flow answer for {topic}")),
            "{entries:?}"
        );
    }
    db.pool.close().await;
}

/// T-2A3 measurement (not a gate): 64 items of one saved Agent whose model
/// answers after a fixed 100 ms, on a multi-thread runtime. At concurrency 8
/// the items overlap their model latency (about 8 x faster than one at a
/// time); the wall time is the evidence that a Map runs one Agent in parallel.
async fn measure_saved_agent_map_with_latency(concurrency: usize) {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the Map latency measurement");
        return;
    };
    // A timing run: the cross-check would add a full read to every refresh.
    let _cross_check_off = crate::agents::runner_view::disable_cross_check();
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let topics = (0..64)
        .map(|index| format!("topic-q{index:02}x"))
        .collect::<Vec<_>>();
    let outcomes = topics
        .iter()
        .map(|_| TestModelGatewayOutcome::Delayed {
            delay: Duration::from_millis(100),
            response: pipeline_text_response("item done"),
        })
        .collect();
    let (assembler, captured) = gated_assembler(
        &db.pool,
        outcomes,
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_agent(),
    );
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] =
        json!(saved_agent_map_yaml(&topics, concurrency));
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let started = std::time::Instant::now();
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the Map pipeline");
    run_collecting(invocation).await.expect("the Map joins");
    let elapsed = started.elapsed();
    assert_eq!(model_requests(&captured), 64);
    let entries = joined(&db.pool).await.expect("joined");
    assert_eq!(entries.len(), 64);
    assert!(
        entries.iter().all(|entry| entry["status"] == "succeeded"),
        "{entries:?}"
    );
    eprintln!(
        "T2A3_MAP_LATENCY concurrency={concurrency} items=64 model_latency_ms=100 elapsed_ms={}",
        elapsed.as_millis()
    );
    // FU-287: the root session's refreshes of this run, from the stats of the
    // views built over this run's own database (concurrent tests have theirs).
    let tally = crate::agents::runner_view::stats_for_database(&db.database_name);
    let (refreshes, fallbacks, rows_read) = (tally.refreshes, tally.fallbacks, tally.rows_read);
    eprintln!(
        "FU287_REFRESH concurrency={concurrency} refreshes={refreshes} fallbacks={fallbacks} rows_read={rows_read}"
    );
    assert!(
        refreshes > 0,
        "the root session's refreshes went through the view"
    );
    assert!(
        fallbacks <= 1,
        "the 64-item Map re-reads in full at most once ({fallbacks} times)"
    );
    db.pool.close().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "measurement: model latency overlap"]
async fn measure_64_saved_agent_items_with_model_latency_at_concurrency_8() {
    measure_saved_agent_map_with_latency(8).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "measurement: model latency overlap"]
async fn measure_64_saved_agent_items_with_model_latency_at_concurrency_1() {
    measure_saved_agent_map_with_latency(1).await;
}

/// B1 (W3-2a): a Parallel whose branches are Code nodes, through the real
/// assembly path: the production assembler opens the run's checkpoint
/// family from its composition paths (none for Code) and binds a real
/// `CodeRuntimeFactory` with the claim's sandbox authority. No sandbox
/// answers, so each branch's Code attempt fails typed at its own boundary.
/// The fan-out itself must start both branches: before the fix it failed at
/// once (`checkpoint.invalid_scope`: no application family for a Code
/// branch) and no Code attempt ran.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_parallel_of_code_branches_starts_each_branch_through_assembly() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the Code fan-out assembly proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let control = Arc::new(
        crate::protocol::control::AgentControlClient::from_channel(
            tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap(),
    );
    let profiles = ["python", "javascript"]
        .into_iter()
        .map(|language| {
            let config: crate::config::SandboxRuntimeConfig = serde_json::from_value(json!({
                "language": language, "target": "sandbox.internal:9446",
                "audience": format!("sandbox-{language}"),
                "image_digest": format!("sha256:{}", "a".repeat(64)),
                "policy_revision": "isolated-v1", "timeout_seconds": 30
            }))
            .unwrap();
            let client = crate::sandbox::client::SandboxClient::from_channel(
                tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
                config.audience.clone(),
                Duration::from_secs(1),
            )
            .unwrap();
            (config, client, None)
        })
        .collect();
    let factory = Arc::new(crate::agents::graph::CodeRuntimeFactory::new(
        control,
        profiles,
        db.pool.clone(),
    ));
    let (assembler, _) = assembler_on(
        &db.pool,
        saved_runtime(Vec::new(), json!({})),
        pipeline_connector(),
    );
    let assembler = assembler.with_sandbox(Some(factory));
    let mut request = super::pipeline_request();
    request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture")
        .insert(
            "instructions".to_owned(),
            json!("entry_point: gather\nstate:\n  input: {type: str, value: ''}\n  topic: {type: str, value: slots}\n  detail: {type: str, value: ''}\n  joined: {type: list, value: []}\nnodes:\n  - {id: gather, type: parallel, branches: [{id: left, node: js_a}, {id: right, node: js_b}], max_concurrency: 2, wait: all, on_error: continue, output: [joined], transition: END}\n  - {id: js_a, type: code, language: javascript, code: \"export default async (state) => 'a:' + state.topic;\", input: [topic], output: [detail]}\n  - {id: js_b, type: code, language: javascript, code: \"export default async (state) => 'b:' + state.topic;\", input: [topic], output: [detail]}\n"),
        );
    let invocation = assembler
        .with_fanout_decisions_for_tests(
            crate::agents::graph::fanout_test_ledger::TestLedger::new().client(1),
        )
        .assemble(claim(&request, 1).with_sandbox_for_tests(Arc::new(
            crate::protocol::control::ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture(),
        )))
        .await
        .unwrap_or_else(|error| panic!("the Parallel of Code branches assembles: {error:?}"));
    // The sandbox here is unreachable, so each Code branch fails typed as a
    // member and, every member failed, the fan-out ends with the registered
    // member failure. Before B1 the branch never started: its checkpoint
    // family was refused (`agent.legacy` / `checkpoint.invalid_scope`).
    let error = run_to_end(invocation)
        .await
        .expect_err("every Code branch failed against an unreachable sandbox");
    assert!(
        error.contains("Some(\"graph.fanout.member_failed\")"),
        "each Code branch started and failed as a member: {error}"
    );
    assert!(!error.contains("invalid_scope"), "{error}");
    db.pool.close().await;
}

// ---------------------------------------------------------------------------
// W3-2b: a Code node inside a saved pipeline the root's Agent node runs, lost
// inside its journaled attempt. The production assembler binds a real
// `CodeRuntimeFactory`; Main's content service is a fake that answers the
// attempt's original visit (`register_code_visit`), the boundary at which the
// crash process dies once its journal's Started record is durable. The
// reclaim's assembly-time recovery walk must stop at the saved child's Code
// node (a leaf its visit-scoped journal decides), not refuse the frontier.
// ---------------------------------------------------------------------------

const SAVED_CODE_TEST: &str =
    "agents::pipeline_tests::fanout_effect_pg_tests::postgres_saved_child_code_process";

/// The saved pipeline (app 3, version 4): `validate` (Code), first or after a
/// State Modifier (`leaf`: the child saved a row pending `validate`).
fn saved_code_pipeline(leaf: bool) -> Value {
    let mut pipeline = direct_effect_pipeline();
    pipeline["instructions"] = json!(if leaf {
        "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: prep\nnodes:\n  - id: prep\n    type: state_modifier\n    input: [input]\n    output: [answer]\n    template: 'p'\n    transition: validate\n  - id: validate\n    type: code\n    language: python\n    code: 'return 41'\n    input: [answer]\n    output: [answer]\n    transition: END\n"
    } else {
        "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: validate\nnodes:\n  - id: validate\n    type: code\n    language: python\n    code: 'return 41'\n    input: [input]\n    output: [answer]\n    transition: END\n"
    });
    pipeline
}

/// Root → `delegate` → the saved pipeline with Code.
fn saved_code_request() -> super::super::request::AgentExecutionRequest {
    let mut request = child_parallel_request();
    request.payload.application["version_details"]["instructions"] = json!(
        "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: \"release-agent\"\n    input_mapping:\n      task: {type: fixed, value: 'Validate the partition'}\n    output: [answer]\n    transition: END\n"
    );
    request
}

/// Main's content service for the Code attempt: the crash phase dies at the
/// original visit once the attempt's Started record is durable and the visit
/// names its saved-child owner scope; the recover phase refuses the visit.
struct SavedCodeMain {
    pool: sqlx::PgPool,
    crash: bool,
}

#[tonic::async_trait]
impl crate::transport::input_content::content_fake::ContentFake for SavedCodeMain {
    async fn register_code_visit(
        &self,
        request: tonic::Request<crate::protocol::elitea::runtime::v1::RegisterCodeVisitRequestV1>,
    ) -> Result<
        tonic::Response<crate::protocol::elitea::runtime::v1::RegisterCodeVisitResponseV1>,
        tonic::Status,
    > {
        let body: Value = serde_json::from_slice(&request.into_inner().visit_json)
            .map_err(|_| tonic::Status::invalid_argument("visit"))?;
        let scoped = body["saved_child_scope"]["schema"]
            == crate::agents::pipeline::saved_code_child::SAVED_CODE_CHILD_SCOPE_SCHEMA
            && body["saved_child_scope"]["agent_node_id"] == "delegate"
            && body["saved_child_scope"]["code_node_id"] == "validate";
        if self.crash {
            let started = sqlx::query_scalar::<_, i64>(
                "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
                 WHERE thread_id LIKE 'n1:%' AND writer_claim_id = 'claim-1'",
            )
            .fetch_one(&self.pool)
            .await
            .unwrap_or_default();
            std::process::exit(if started > 0 && scoped {
                CRASH_EXIT_CODE
            } else {
                MARKER_MISSING_EXIT_CODE
            });
        }
        assert!(
            scoped,
            "the reclaimed attempt names its owner scope: {body}"
        );
        Err(tonic::Status::permission_denied("refused"))
    }
}

fn saved_code_assembler(
    pool: &sqlx::PgPool,
    leaf: bool,
    crash: bool,
) -> PipelineNativeAgentAssembler {
    let control = Arc::new(
        crate::protocol::control::AgentControlClient::from_channel(
            tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap(),
    );
    let config: crate::config::SandboxRuntimeConfig = serde_json::from_value(json!({
        "language": "python", "target": "sandbox.internal:9446", "audience": "sandbox-python",
        "image_digest": format!("sha256:{}", "a".repeat(64)),
        "policy_revision": "isolated-v1", "timeout_seconds": 30
    }))
    .unwrap();
    let client = crate::sandbox::client::SandboxClient::from_channel(
        tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
        config.audience.clone(),
        Duration::from_secs(1),
    )
    .unwrap();
    let content = Arc::new(
        crate::transport::InputContentClient::with_original_code_visit_fixture_rpc_deadline(
            SavedCodeMain {
                pool: pool.clone(),
                crash,
            },
            Duration::from_secs(5),
        ),
    );
    let factory = Arc::new(
        crate::agents::graph::CodeRuntimeFactory::new(
            control,
            vec![(config, client, None)],
            pool.clone(),
        )
        .with_code_intents(content),
    );
    let (assembler, _) = assembler_on(
        pool,
        saved_runtime(Vec::new(), saved_code_pipeline(leaf)),
        pipeline_connector(),
    );
    assembler.with_sandbox(Some(factory))
}

/// One phase of [`saved_code_scenario`], in its own process.
#[tokio::test(flavor = "current_thread")]
async fn postgres_saved_child_code_process() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let leaf = std::env::var(MODE_ENV).expect("mode") == "leaf";
    let pool = connect(&database).await;
    let request = saved_code_request();
    let sandbox = || {
        Arc::new(
            crate::protocol::control::ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture(),
        )
    };
    if std::env::var(PHASE_ENV).expect("phase") == "crash" {
        let invocation = saved_code_assembler(&pool, leaf, true)
            .assemble(claim(&request, 1).with_sandbox_for_tests(sandbox()))
            .await
            .unwrap_or_else(|error| panic!("crash: assemble: {error}"));
        let outcome = run_to_end(invocation).await;
        panic!("crash: the process outlived its crash point: {outcome:?}");
    }
    let assembler = saved_code_assembler(&pool, leaf, false);
    let evidence = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
        &assembler,
        &request,
        &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
        test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .unwrap_or_else(|error| panic!("recover: inspect: {error} [{:?}]", error.cause()));
    let (authorizer, control) = crate::protocol::control::test_checkpoint_authorizer(
        EXECUTION,
        GENERATION,
        evidence.digest(),
    );
    let authorization = authorizer
        .authorize(&control, evidence)
        .await
        .unwrap_or_else(|_| panic!("recover: authorize"));
    // Before W3-2b the walk refused this frontier here
    // (`pipeline.recovery.child_not_parked` / `child_missing`).
    let pending = assembler
        .assemble_checkpoint(
            AuthorizedNativeAssembly::from_authorized(
                &request,
                test_runtime_context_authority(),
                test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                Arc::new(TestStateWriterLease::current()),
                AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
            )
            .with_sandbox_for_tests(sandbox()),
        )
        .await
        .unwrap_or_else(|error| {
            panic!(
                "recover: the walk stops at the saved child's Code: {error} [{:?}]",
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
            )
        });
    let (_, _, _, _, permit) = authorization.into_lifecycle_parts();
    let invocation = pending
        .authorize_lifecycle(permit)
        .unwrap_or_else(|error| panic!("recover: lifecycle: {error}"));
    // The reclaimed attempt re-enters the same journaled attempt and asks
    // Main again (here refused): it ends typed, never re-run blind.
    let error = run_to_end(invocation)
        .await
        .expect_err("recover: the refused visit ends the run typed");
    assert!(
        error.contains("pipeline.code_authorization_failed"),
        "recover: {error}"
    );
}

/// W3-2b two-process proof: the crash process dies inside the saved child's
/// Code attempt (its Started record durable, its visit naming its owner
/// scope); the reclaim's walk admits the frontier and the attempt re-enters
/// its own visit-scoped journal (the same journal thread, no second one).
async fn saved_code_scenario(url: &str, mode: &str) {
    let db = IsolatedPostgres::create(url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    for migration in [
        include_str!(
            "../../../elitea-main/migrations/agentstate/0007_sandbox_dispatch_journal.sql"
        ),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0014_sandbox_dispatch_submission_marker.sql"
        ),
    ] {
        sqlx::raw_sql(migration)
            .execute(&db.pool)
            .await
            .expect("apply the dispatch journal");
    }
    let phase = |phase: &'static str| {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args([
                "--exact",
                SAVED_CODE_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, &db.database_name)
            .env(MODE_ENV, mode)
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
        "one journal for the one attempt: {journals:?}"
    );
    let output = run_phase("recover", phase("recover"));
    assert_eq!(
        output.status.code(),
        Some(0),
        "recover: {} {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(journal_threads(&db.pool).await, journals, "{mode}");
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_a_saved_child_lost_inside_its_first_code_attempt_is_recovered_by_its_journal() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the saved-child Code PostgreSQL proof");
        return;
    };
    saved_code_scenario(&url, "entry").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_a_saved_child_lost_inside_a_later_code_attempt_is_recovered_by_its_journal() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the saved-child Code PostgreSQL proof");
        return;
    };
    saved_code_scenario(&url, "leaf").await;
}

const TOOL_PIPELINE_TEST: &str =
    "agents::pipeline_tests::fanout_effect_pg_tests::postgres_member_pipeline_tool_child";
/// `map` (one Map item), `parallel` (two Parallel branches) or `map8` (eight
/// Map items at concurrency 4, T4).
const TOOL_PIPELINE_SHAPE_ENV: &str = "ELITEA_FU291_PG_SHAPE";
/// Where the crash process dies:
/// - `nested_llm`: inside the nested LLM node's model call (T1a/T1c);
/// - `completed`: after the nested pipeline finished, before the Agent
///   answered (T1f);
/// - `llm_tool_effect` / `llm_tool_read`: inside the nested LLM node's own
///   effectful / read-only tool (T1d);
/// - `direct_started`: inside the nested direct toolkit node's call, once its
///   journal's Started record is durable (T1e);
/// - `takeover`: as `nested_llm`; the recovery's claim is superseded between
///   its inspect and its assembly, and a newer claim completes (T2).
const TOOL_PIPELINE_MODE_ENV: &str = "ELITEA_FU291_PG_MODE";
const NESTED_SYSTEM: &str = "Answer the topic you are given.";

fn tool_pipeline_topics(shape: &str) -> Vec<String> {
    match shape {
        "map" => vec!["topic-k00x".to_owned()],
        "map8" => (0..8).map(|index| format!("topic-k{index:02}x")).collect(),
        _ => vec!["topic-left".to_owned(), "topic-right".to_owned()],
    }
}

fn tool_pipeline_yaml(shape: &str) -> String {
    match shape {
        "map" => saved_agent_map_yaml(&tool_pipeline_topics(shape), 1),
        "map8" => saved_agent_map_yaml(&tool_pipeline_topics(shape), 4),
        _ => SAVED_AGENT_PARALLEL_YAML.to_owned(),
    }
}

/// Members inside their nested pipeline when the crash process dies.
fn tool_pipeline_in_flight(shape: &str) -> usize {
    match shape {
        "map8" => 4,
        other => tool_pipeline_topics(other).len(),
    }
}

/// The nested saved pipeline whose LLM node may call the effectful MCP tool.
fn helper_tool_pipeline() -> Value {
    let mut pipeline = effectful_agent();
    pipeline["agent_type"] = json!("pipeline");
    pipeline["instructions"] = json!(
        "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    tool_names:\n      release intelligence: [\"lookup_release\"]\n    input: [input]\n    input_mapping:\n      system: {type: fixed, value: 'Answer the topic you are given.'}\n      task: {type: variable, value: input}\n    output: [answer]\n    transition: END\n"
    );
    pipeline
}

fn helper_for_mode(mode: &str) -> Value {
    match mode {
        "llm_tool_effect" | "llm_tool_read" => helper_tool_pipeline(),
        "direct_started" => direct_effect_pipeline(),
        _ => helper_pipeline(),
    }
}

/// Only a nested LLM node's request carries its system text right before the
/// member's own topic as its task.
fn nested_marker(topic: &str) -> &'static str {
    Box::leak(format!("given.\",\"role\":\"system\"}},{{\"content\":\"{topic}\"").into_boxed_str())
}

/// Only a member Agent's report carries its nested pipeline's answer.
fn report_marker(topic: &str) -> &'static str {
    Box::leak(format!("nested answer for {topic}").into_boxed_str())
}

fn topic_marker(topic: &str) -> &'static str {
    Box::leak(topic.to_owned().into_boxed_str())
}

/// The MCP tool of the crash process: every member's call enters it (its
/// boundary or journal Started record is durable before it runs), and once
/// `members` are in, the process dies.
struct EnteringMcpConnector {
    entered: Arc<AtomicUsize>,
    members: usize,
    read_only: bool,
}

#[async_trait]
impl McpConnector for EnteringMcpConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(EnteringMcpTool {
                inner: PipelineMcpTool {
                    calls: Arc::new(AtomicUsize::new(0)),
                    read_only: self.read_only,
                },
                entered: Arc::clone(&self.entered),
                members: self.members,
            })],
        )))
    }
}

struct EnteringMcpTool {
    inner: PipelineMcpTool,
    entered: Arc<AtomicUsize>,
    members: usize,
}

#[async_trait]
impl Tool for EnteringMcpTool {
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
        self.entered.fetch_add(1, Ordering::SeqCst);
        while self.entered.load(Ordering::SeqCst) < self.members {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        std::process::exit(CRASH_EXIT_CODE)
    }
}

/// The crash process's model outcomes and its death condition.
fn crash_plan(
    mode: &str,
    shape: &str,
    topics: &[String],
    pool: &sqlx::PgPool,
    members: usize,
) -> Vec<TestModelGatewayOutcome> {
    let held = Arc::new(tokio::sync::Notify::new());
    let mut outcomes = Vec::new();
    for topic in topics {
        let nested = match mode {
            "completed" => TestModelGatewayOutcome::ResponseForRequest {
                marker: nested_marker(topic),
                response: pipeline_text_response(&format!("nested answer for {topic}")),
            },
            "llm_tool_effect" | "llm_tool_read" => TestModelGatewayOutcome::ResponseForRequest {
                marker: nested_marker(topic),
                response: pipeline_mcp_tool_call_response(),
            },
            _ => TestModelGatewayOutcome::ResponseForRequestAfter {
                marker: nested_marker(topic),
                gate: Arc::clone(&held),
                response: pipeline_text_response("never served"),
            },
        };
        outcomes.push(nested);
    }
    if mode == "completed" {
        for topic in topics {
            outcomes.push(TestModelGatewayOutcome::ResponseForRequestAfter {
                marker: report_marker(topic),
                gate: Arc::clone(&held),
                response: pipeline_text_response("never served"),
            });
        }
    }
    for (index, topic) in topics.iter().enumerate() {
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker: topic_marker(topic),
            response: nested_agent_call_response(&format!("call_item_{index}"), topic),
        });
    }
    let frontier = match mode {
        // The tool kills the process itself, once every member is inside it.
        "llm_tool_effect" | "llm_tool_read" | "direct_started" => return outcomes,
        // The nested run finished; the Agent's report request is held.
        "completed" => {
            "SELECT count(DISTINCT thread_id) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE '%/pt1:%' AND pending_nodes::text = '[]'"
        }
        // T4: the items' calls of one shared pipeline tool run one at a time
        // (its node-event channel); four in flight are four admitted nested
        // threads, one of them inside its nested LLM node.
        _ if shape == "map8" => {
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoint_writers w \
             WHERE w.thread_id LIKE '%/pt1:%' AND EXISTS (\
               SELECT 1 FROM elitea_runtime.agent_graph_checkpoints c \
               WHERE c.thread_id LIKE '%/pt1:%' AND c.pending_nodes::text LIKE '%answer%')"
        }
        _ => {
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE '%/pt1:%' AND pending_nodes::text LIKE '%answer%'"
        }
    };
    let watcher = pool.clone();
    let members = i64::try_from(members).expect("members");
    tokio::spawn(async move {
        loop {
            let durable = sqlx::query_scalar::<_, i64>(frontier)
                .fetch_one(&watcher)
                .await
                .unwrap_or_default();
            if durable >= members {
                std::process::exit(CRASH_EXIT_CODE);
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    });
    outcomes
}

/// The recovery's model outcomes, routed by what each request carries: a
/// nested node's, a member Agent's report (it holds the nested answer), and
/// last an Agent's first request (only a member that had not chosen its call
/// before the kill makes one). An effectful nested call that may have started
/// leaves the Agent a typed failure to report, routed by its topic.
fn recovery_plan(mode: &str, topics: &[String]) -> Vec<TestModelGatewayOutcome> {
    let mut outcomes = Vec::new();
    if matches!(mode, "llm_tool_effect" | "direct_started") {
        for topic in topics {
            outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
                marker: topic_marker(topic),
                response: pipeline_text_response(&format!("outer answer for {topic}")),
            });
        }
        return outcomes;
    }
    if mode == "llm_tool_read" {
        // The replayed read's result reaches only the nested node's next
        // request (its replayed history shapes the task message otherwise);
        // the Agent's report then carries its topic.
        for _ in topics {
            outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
                marker: "call_mcp",
                response: pipeline_text_response("nested answer"),
            });
        }
        for topic in topics {
            outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
                marker: topic_marker(topic),
                response: pipeline_text_response(&format!("outer answer for {topic}")),
            });
        }
        return outcomes;
    }
    if mode != "completed" {
        for topic in topics {
            outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
                marker: nested_marker(topic),
                response: pipeline_text_response(&format!("nested answer for {topic}")),
            });
        }
    }
    for topic in topics {
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker: report_marker(topic),
            response: pipeline_text_response(&format!("outer answer for {topic}")),
        });
    }
    for (index, topic) in topics.iter().enumerate() {
        outcomes.push(TestModelGatewayOutcome::ResponseForRequest {
            marker: topic_marker(topic),
            response: nested_agent_call_response(&format!("call_item_{index}"), topic),
        });
    }
    outcomes
}

/// T2: the reclaim of claim 2 is authorized at inspect, then claim 3 takes
/// the run before claim 2 assembles. Claim 2 runs nothing (no model request,
/// no nested row); claim 3 completes.
async fn superseded_then_recovered(
    assembler: &PipelineNativeAgentAssembler,
    request: &super::super::request::AgentExecutionRequest,
    pool: &sqlx::PgPool,
    captured: &CapturedModelRequests,
) -> AssembledNativeAgentInvocation<PipelineAgentCompletion> {
    let inspect = |attempt: u8| async move {
        let evidence = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
            assembler,
            request,
            &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
            test_session_authority_for_reclaim(EXECUTION, GENERATION, attempt),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap_or_else(|error| panic!("recover: inspect claim {attempt}: {error}"));
        let (authorizer, control) = crate::protocol::control::test_checkpoint_authorizer(
            EXECUTION,
            GENERATION,
            evidence.digest(),
        );
        authorizer
            .authorize(&control, evidence)
            .await
            .unwrap_or_else(|_| panic!("recover: authorize claim {attempt}"))
    };
    let assemble = |attempt: u8| async move {
        assembler
            .assemble_checkpoint(AuthorizedNativeAssembly::from_authorized(
                request,
                test_runtime_context_authority(),
                test_session_authority_for_reclaim(EXECUTION, GENERATION, attempt),
                Arc::new(TestStateWriterLease::current()),
                AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
            ))
            .await
            .map_err(|error| format!("assemble claim {attempt}: {error}"))
    };
    let nested_rows = || async {
        sqlx::query_scalar::<_, i64>(
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE '%/pt1:%'",
        )
        .fetch_one(pool)
        .await
        .expect("nested rows")
    };
    let stale = inspect(2).await;
    let newer = inspect(3).await;
    let before = nested_rows().await;
    let outcome = match assemble(2).await {
        Ok(pending) => {
            let (_, _, _, _, permit) = stale.into_lifecycle_parts();
            match pending.authorize_lifecycle(permit) {
                Ok(invocation) => run_to_end(invocation).await.map(|_| ()),
                Err(error) => Err(format!("authorize the lifecycle of claim 2: {error}")),
            }
        }
        Err(error) => Err(error),
    };
    let error = outcome.expect_err("recover: the superseded claim runs to an end");
    // The newer claim's inspect took the run's session writer: the older
    // claim's assembly is refused before anything is built.
    assert!(
        error.contains("assemble claim 2")
            && (error.contains("session is unavailable") || error.contains("writer_not_current")),
        "recover: the superseded claim is fenced at its assembly: {error}"
    );
    assert_eq!(
        model_requests(captured),
        0,
        "recover: claim 2 asked no model"
    );
    assert_eq!(
        nested_rows().await,
        before,
        "recover: claim 2 wrote no nested row"
    );
    let pending = assemble(3)
        .await
        .unwrap_or_else(|error| panic!("recover: {error}"));
    let (_, _, _, _, permit) = newer.into_lifecycle_parts();
    pending
        .authorize_lifecycle(permit)
        .unwrap_or_else(|error| panic!("recover: authorize the lifecycle of claim 3: {error}"))
}

/// One process per phase of a FU-291 member kill (`TOOL_PIPELINE_MODE_ENV`):
/// `crash` dies as the mode says; `recover` is the production reclaim with
/// model-checkpoint recovery on.
#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One child process per phase and mode: crash, then recover with its assertions.
async fn postgres_member_pipeline_tool_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let shape = std::env::var(TOOL_PIPELINE_SHAPE_ENV).expect("shape");
    let mode = std::env::var(TOOL_PIPELINE_MODE_ENV).expect("mode");
    let topics = tool_pipeline_topics(&shape);
    // Four members inside their nested pipelines at once (T4) need more than
    // the four connections the other shapes share.
    let pool = if shape == "map8" {
        crate::test_pg::pool_options()
            .max_connections(16)
            .connect_with(
                crate::test_pg::connect_options(
                    &crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL")
                        .expect("database URL"),
                )
                .expect("parse database URL")
                .database(&database),
            )
            .await
            .expect("connect isolated database")
    } else {
        connect(&database).await
    };
    let mut request = request("continue");
    request.payload.application["version_details"]["instructions"] =
        json!(tool_pipeline_yaml(&shape));
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    let read_only = mode == "llm_tool_read";
    if std::env::var(PHASE_ENV).expect("phase") == "crash" {
        let outcomes = crash_plan(
            &mode,
            &shape,
            &topics,
            &pool,
            tool_pipeline_in_flight(&shape),
        );
        let runtime = saved_runtime_on(
            outcomes,
            pipeline_delegating_agent(),
            Some(helper_for_mode(&mode)),
            None,
            config,
        );
        let connector = Arc::new(EnteringMcpConnector {
            entered: Arc::new(AtomicUsize::new(0)),
            members: tool_pipeline_in_flight(&shape),
            read_only,
        });
        let (assembler, _) = assembler_on(&pool, runtime, connector);
        let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
        let invocation = assembler
            .with_fanout_decisions_for_tests(ledger.client(1))
            .assemble(claim(&request, 1))
            .await
            .unwrap_or_else(|error| panic!("crash: assemble: {error}"));
        let outcome = run_to_end(invocation).await;
        panic!("crash: the process outlived its crash point: {outcome:?}");
    }
    let runtime = saved_runtime_on(
        recovery_plan(&mode, &topics),
        pipeline_delegating_agent(),
        Some(helper_for_mode(&mode)),
        None,
        config,
    );
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let connector = Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only,
    });
    let (assembler, captured) = assembler_on(&pool, runtime, connector);
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let assembler = assembler.with_fanout_decisions_for_tests(ledger.client(2));
    let invocation = if mode == "takeover" {
        superseded_then_recovered(&assembler, &request, &pool, &captured).await
    } else {
        recovered(&assembler, &request, EXECUTION, 2)
            .await
            .unwrap_or_else(|error| panic!("recover: the reclaim recovers: {error}"))
    };
    run_to_end(invocation)
        .await
        .unwrap_or_else(|error| panic!("recover: the members complete: {error}"));
    let bodies = captured
        .lock()
        .expect("captured")
        .iter()
        .map(|request| String::from_utf8_lossy(&request.body).into_owned())
        .collect::<Vec<_>>();
    let mine = |topic: &str| {
        bodies
            .iter()
            .filter(|body| body.contains(topic))
            .cloned()
            .collect::<Vec<_>>()
    };
    match mode.as_str() {
        // C3 (T1f): the finished nested thread answers its stored result; its
        // LLM node never runs again and the Agent reads exactly that result.
        "completed" => {
            assert_eq!(bodies.len(), topics.len(), "recover: {bodies:?}");
            for topic in &topics {
                let mine = mine(topic);
                assert_eq!(mine.len(), 1, "recover: {topic}");
                assert!(!mine[0].contains(NESTED_SYSTEM), "recover: no nested run");
                assert!(
                    mine[0].contains(&format!("nested answer for {topic}")),
                    "recover: the Agent reads the stored result"
                );
            }
        }
        // T1d effectful / T1e started: the call may have run; it is never
        // repeated, the nested model is not asked again, and the Agent reads
        // a typed failure instead of a result.
        "llm_tool_effect" | "direct_started" => {
            assert_eq!(
                tool_calls.load(Ordering::SeqCst),
                0,
                "recover: not repeated"
            );
            assert_eq!(bodies.len(), topics.len(), "recover: {bodies:?}");
            for topic in &topics {
                let mine = mine(topic);
                assert_eq!(mine.len(), 1, "recover: {topic}");
                assert!(
                    !mine[0].contains(NESTED_SYSTEM),
                    "recover: no nested model call"
                );
                assert!(
                    mine[0].contains("RECOVERY_REFUSED") || mine[0].contains("may already"),
                    "recover: the Agent reads the typed may-have-started failure: {}",
                    mine[0]
                );
            }
        }
        // T1d read-only: the nested node's stored call batch is replayed (no
        // nested model call chose it again) and its read-only tool runs once
        // more; then the nested node answers and the Agent reports.
        "llm_tool_read" => {
            assert_eq!(
                tool_calls.load(Ordering::SeqCst),
                topics.len(),
                "recover: each read-only call runs once more"
            );
            assert_eq!(bodies.len(), 2 * topics.len(), "recover: {bodies:?}");
            for topic in &topics {
                let mine = mine(topic);
                assert_eq!(mine.len(), 2, "recover: {topic}");
                assert!(mine[0].contains(NESTED_SYSTEM) && mine[0].contains("lookup_release"));
                assert!(!mine[1].contains(NESTED_SYSTEM), "recover: then the report");
            }
        }
        // T4: the four killed members (one inside its nested LLM node, three
        // admitted with their call recorded, waiting for the shared tool)
        // replay their call and run or re-enter the nested node (2 requests:
        // nested, report); the four that had not started run whole (3).
        _ if shape == "map8" => {
            let mut resumed = 0;
            for topic in &topics {
                let mine = mine(topic);
                match mine.len() {
                    2 => {
                        resumed += 1;
                        assert!(mine[0].contains(NESTED_SYSTEM), "recover: {topic}");
                    }
                    3 => assert!(!mine[0].contains(NESTED_SYSTEM), "recover: {topic}"),
                    other => panic!("recover: {topic} made {other} requests"),
                }
                for body in &mine {
                    for other in topics.iter().filter(|other| *other != topic) {
                        assert!(
                            !body.contains(other.as_str()),
                            "recover: {topic} saw {other}"
                        );
                    }
                }
            }
            assert_eq!(resumed, 4, "recover: the four killed members resumed");
        }
        // C2: no member's Agent asked the model again for its call. Each
        // member's first request after the kill is its nested LLM node's.
        _ => {
            assert_eq!(bodies.len(), 2 * topics.len(), "recover: {bodies:?}");
            for topic in &topics {
                let mine = mine(topic);
                assert_eq!(mine.len(), 2, "recover: {topic}");
                assert!(
                    mine[0].contains(NESTED_SYSTEM),
                    "recover: {topic}'s first request is the nested node's"
                );
                assert!(!mine[1].contains(NESTED_SYSTEM), "recover: then the report");
            }
        }
    }
}

/// The nested thread rows at the crash, by mode.
async fn crashed_frontier(pool: &sqlx::PgPool, mode: &str) -> usize {
    let query = match mode {
        "completed" => {
            "SELECT count(DISTINCT thread_id) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE '%/pt1:%' AND pending_nodes::text = '[]'"
        }
        "direct_started" => {
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE '%/pt1:%' AND pending_nodes::text LIKE '%rev%'"
        }
        _ => {
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE '%/pt1:%' AND pending_nodes::text LIKE '%answer%'"
        }
    };
    usize::try_from(
        sqlx::query_scalar::<_, i64>(query)
            .fetch_one(pool)
            .await
            .expect("nested rows"),
    )
    .expect("rows")
}

/// FU-291 two-process member kills: a Worker killed as `mode` says inside a
/// member's pipeline tool call, then the production reclaim. Each killed call's
/// nested thread was admitted and its frontier saved before its first effect;
/// the reclaim re-enters the same nested threads (no new writer); every member
/// lands one answer, its own.
#[allow(clippy::too_many_lines)] // One parent per mode: the crash's durable evidence, then the reclaim's.
async fn member_pipeline_tool_kill(url: &str, shape: &str, mode: &str) {
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
                TOOL_PIPELINE_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, &db.database_name)
            .env(TOOL_PIPELINE_SHAPE_ENV, shape)
            .env(TOOL_PIPELINE_MODE_ENV, mode)
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
    let in_flight = tool_pipeline_in_flight(shape);
    let crashed = nested_tool_threads(&db.pool).await;
    assert_eq!(
        crashed.len(),
        in_flight,
        "each killed call's nested thread was admitted before its first effect"
    );
    assert_eq!(
        crashed_frontier(&db.pool, mode).await,
        if shape == "map8" { 1 } else { in_flight },
        "each nested thread inside its pipeline has a durable frontier"
    );
    match mode {
        "llm_tool_effect" => assert_eq!(
            phase_boundaries(&db.pool, "claim-1", "tool_may_have_started")
                .await
                .len(),
            in_flight,
            "each nested LLM node's effectful call left its boundary"
        ),
        "direct_started" => assert_eq!(
            journal_threads(&db.pool).await.len(),
            in_flight,
            "each nested direct node's journal recorded its start"
        ),
        _ => {}
    }
    let nested_rows = sqlx::query_scalar::<_, i64>(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints WHERE thread_id LIKE '%/pt1:%'",
    )
    .fetch_one(&db.pool)
    .await
    .expect("nested rows");
    let output = run_phase("recover", phase("recover"));
    assert!(
        output.status.success(),
        "recover: {} {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let recovered = nested_tool_threads(&db.pool).await;
    assert_eq!(
        recovered.len(),
        tool_pipeline_topics(shape).len(),
        "one nested thread per member: {recovered:?}"
    );
    assert!(
        crashed.iter().all(|thread| recovered.contains(thread)),
        "the same nested threads before and after the kill: {crashed:?} {recovered:?}"
    );
    if mode == "completed" {
        let after = sqlx::query_scalar::<_, i64>(
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE '%/pt1:%'",
        )
        .fetch_one(&db.pool)
        .await
        .expect("nested rows");
        assert_eq!(
            after, nested_rows,
            "a completed nested thread is never written"
        );
    }
    if mode == "takeover" {
        let owners = sqlx::query_scalar::<_, String>(
            "SELECT DISTINCT writer_claim_id FROM elitea_runtime.agent_graph_checkpoint_writers \
             WHERE thread_id LIKE '%/pt1:%'",
        )
        .fetch_all(&db.pool)
        .await
        .expect("nested writers");
        assert_eq!(
            owners,
            ["claim-3"],
            "the newer claim owns the nested writers"
        );
    }
    let entries = joined(&db.pool).await.expect("joined entries");
    let topics = tool_pipeline_topics(shape);
    assert_eq!(entries.len(), topics.len(), "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
        let answer = entry["outputs"]
            .as_object()
            .and_then(|outputs| outputs.values().next())
            .and_then(Value::as_str)
            .unwrap_or_default();
        assert!(
            topics
                .iter()
                .any(|topic| answer == format!("outer answer for {topic}")),
            "each member lands its own answer: {entries:?}"
        );
    }
    if shape == "map8" {
        for (index, (entry, topic)) in entries.iter().zip(&topics).enumerate() {
            assert_eq!(entry["index"], index, "{entries:?}");
            assert_eq!(
                entry["outputs"]["answer"],
                format!("outer answer for {topic}"),
                "no state crosses items: {entries:?}"
            );
        }
    }
    db.pool.close().await;
}

macro_rules! member_pipeline_tool_kill_test {
    ($(#[$doc:meta])* $name:ident, $shape:literal, $mode:literal) => {
        $(#[$doc])*
        #[tokio::test(flavor = "current_thread")]
        async fn $name() {
            let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
                eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-291 kill proof");
                return;
            };
            member_pipeline_tool_kill(&url, $shape, $mode).await;
        }
    };
}

member_pipeline_tool_kill_test!(
    /// T1a/T1c: killed inside the nested LLM node; resumes on the same thread.
    postgres_a_map_item_killed_inside_its_pipeline_tool_resumes_on_the_same_nested_thread,
    "map",
    "nested_llm"
);
member_pipeline_tool_kill_test!(
    postgres_parallel_branches_killed_inside_their_pipeline_tools_resume_on_the_same_nested_threads,
    "parallel",
    "nested_llm"
);
member_pipeline_tool_kill_test!(
    /// T1f/C3: killed after the nested pipeline finished, before the member's
    /// Agent answered: the reclaim returns the stored result, the nested
    /// pipeline never runs again and its thread is never written.
    postgres_a_map_item_killed_after_its_pipeline_tool_finished_reads_the_stored_result,
    "map",
    "completed"
);
member_pipeline_tool_kill_test!(
    postgres_parallel_branches_killed_after_their_pipeline_tools_finished_read_the_stored_results,
    "parallel",
    "completed"
);
member_pipeline_tool_kill_test!(
    /// T1d: killed inside the nested LLM node's effectful tool: F, the call is
    /// never repeated and the Agent reads a typed failure.
    postgres_a_map_item_killed_inside_its_nested_llm_effectful_tool_never_repeats_it,
    "map",
    "llm_tool_effect"
);
member_pipeline_tool_kill_test!(
    postgres_parallel_branches_killed_inside_their_nested_llm_effectful_tools_never_repeat_them,
    "parallel",
    "llm_tool_effect"
);
member_pipeline_tool_kill_test!(
    /// T1d: killed inside the nested LLM node's read-only tool: I, the stored
    /// call is replayed and the read runs once more.
    postgres_a_map_item_killed_inside_its_nested_llm_read_only_tool_replays_it,
    "map",
    "llm_tool_read"
);
member_pipeline_tool_kill_test!(
    postgres_parallel_branches_killed_inside_their_nested_llm_read_only_tools_replay_them,
    "parallel",
    "llm_tool_read"
);
member_pipeline_tool_kill_test!(
    /// T1e: killed inside the nested direct toolkit node once its lent
    /// journal's Started record is durable: F, never repeated.
    postgres_a_map_item_killed_inside_its_nested_direct_tool_never_repeats_it,
    "map",
    "direct_started"
);
member_pipeline_tool_kill_test!(
    postgres_parallel_branches_killed_inside_their_nested_direct_tools_never_repeat_them,
    "parallel",
    "direct_started"
);
member_pipeline_tool_kill_test!(
    /// T2: killed inside the nested LLM node; the reclaim's claim is superseded
    /// between its inspect and its assembly and runs nothing; the newer claim
    /// re-enters the same nested thread and completes.
    postgres_a_superseded_reclaim_runs_nothing_and_the_newer_claim_resumes_the_nested_thread,
    "map",
    "takeover"
);
member_pipeline_tool_kill_test!(
    /// T4: eight Map items at concurrency 4, killed with four inside their
    /// nested LLM nodes: eight distinct nested threads, the four resume, the
    /// four others run whole, no state crosses items.
    postgres_eight_map_items_killed_with_four_in_flight_keep_their_own_nested_threads,
    "map8",
    "nested_llm"
);
