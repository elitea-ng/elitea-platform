//! FU-226: an approved sensitive call that changes data, made by a saved Agent inside a pipeline
//! `agent` node, runs exactly once across Worker process replacement against real `PostgreSQL`.
//!
//! Every phase runs in its own OS process with its own claim (the harness of
//! `pipeline_restart_pg_tests.rs`). The tool is a fixture MCP `lookup_release` the policy marks
//! sensitive and that declares no read-only hint, so it is effectful. Scenarios:
//!
//! * `approve`: `start` pauses on the card; `approve` runs the tool once and finishes; `repeat`
//!   sends the same approval again and runs nothing.
//! * `block`: `start`, then `block` resumes the Agent with the blocked result; the tool never runs.
//! * `crash_before_dispatch`: the approving Worker dies while it binds the tool (before any
//!   dispatch record); `recover` runs the approved call once.
//! * `crash_after_dispatch`: the approving Worker dies inside the tool, after its durable dispatch
//!   record; `recover` refuses with the "may have started" reason and never calls the tool.
//! * `crash_after_outcome`: the tool runs and its outcome is recorded, then the Worker dies at the
//!   child's next model request; `recover` serves the recorded outcome and finishes without
//!   calling the tool again.
//! * `late_duplicate`: as `crash_after_outcome`, but first a different execution submits the
//!   same approval (a duplicate or late submission). Nothing moved the checkpoint, yet it is
//!   refused as stale and runs nothing; the dead execution's own replacement then finishes.
//!
//! * `crash_after_forward`: the replayed child call reaches the root session, then the Worker dies
//!   before the approved call's `dispatched` record. Nothing ran; `recover` runs it once (R).
//! * `dispatch_append_fails`: the `dispatched` append fails; the run ends with a typed failure
//!   (FAILED, so Main does not re-claim it) and never runs the call or answers past it.
//!
//! A replacement is the same execution re-claimed with a higher claim attempt, as Main does after
//! a lease expires; every other submission is another execution.

use super::restart_pg_tests::{
    CHECKPOINT_MIGRATION, PHASE_DEADLINE, authorized_turn, connect, model_requests, run_phase,
};
use super::*;
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use crate::state::{CheckpointLimits, SessionLimits};

const CHILD_TEST: &str = "agents::pipeline_tests::effect_pg_tests::postgres_effect_child";
const DATABASE_ENV: &str = "ELITEA_EFFECT_PG_DATABASE";
const SCENARIO_ENV: &str = "ELITEA_EFFECT_PG_SCENARIO";
const PHASE_ENV: &str = "ELITEA_EFFECT_PG_PHASE";
const PENDING_ENV: &str = "ELITEA_EFFECT_PG_PENDING";
const PENDING_MARKER: &str = "EFFECT_PENDING=";
const EXIT_BEFORE_DISPATCH: i32 = 85;
const EXIT_AFTER_DISPATCH: i32 = 86;
const EXIT_AFTER_OUTCOME: i32 = 87;
const EXIT_AFTER_FORWARD: i32 = 88;
const DONE: &str = "child finished after the approved change";
/// The execution that approves and dies; its replacement re-claims it.
const APPROVAL_EXECUTION: &str = "execution-approval";

/// The phases a scenario runs, each with the exit code its process must end with.
fn phases(scenario: &str) -> &'static [(&'static str, i32)] {
    match scenario {
        "approve" => &[("start", 0), ("approve", 0), ("repeat", 0)],
        "block" => &[("start", 0), ("block", 0)],
        "crash_before_dispatch" => &[
            ("start", 0),
            ("crash", EXIT_BEFORE_DISPATCH),
            ("recover", 0),
        ],
        "crash_after_dispatch" => &[("start", 0), ("crash", EXIT_AFTER_DISPATCH), ("recover", 0)],
        "crash_after_outcome" => &[("start", 0), ("crash", EXIT_AFTER_OUTCOME), ("recover", 0)],
        "crash_after_forward" => &[("start", 0), ("crash", EXIT_AFTER_FORWARD), ("recover", 0)],
        "dispatch_append_fails" => &[("start", 0), ("append_fails", 0)],
        "late_duplicate" => &[
            ("start", 0),
            ("crash", EXIT_AFTER_OUTCOME),
            ("late", 0),
            ("recover", 0),
        ],
        "stale" => &[("start", 0), ("stale", 0)],
        "block_after_dispatch" => &[
            ("start", 0),
            ("crash", EXIT_AFTER_DISPATCH),
            ("block_late", 0),
        ],
        other => panic!("unknown scenario {other}"),
    }
}

async fn run_scenario(scenario: &str) {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the approved-effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let mut pending = String::new();
    for (phase, exit) in phases(scenario) {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(SCENARIO_ENV, scenario)
            .env(PHASE_ENV, phase)
            .env(PENDING_ENV, &pending);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout);
        let stderr = String::from_utf8_lossy(&output.stderr);
        if *exit != 0 {
            assert_eq!(
                output.status.code(),
                Some(*exit),
                "{scenario}/{phase} did not die where expected (deadline {PHASE_DEADLINE:?}): {stdout} {stderr}"
            );
            continue;
        }
        assert!(
            output.status.success() && stdout.contains("1 passed"),
            "{scenario}/{phase} process failed: {stdout} {stderr}"
        );
        if let Some(card) = stdout
            .lines()
            .find_map(|line| line.split_once(PENDING_MARKER).map(|(_, value)| value))
        {
            pending = card.to_owned();
        }
    }
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_approved_effectful_call_in_an_agent_node_runs_once_and_a_repeat_runs_nothing() {
    run_scenario("approve").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_blocked_effectful_call_in_an_agent_node_never_runs() {
    run_scenario("block").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_block_after_a_dispatched_approval_is_refused_not_reported_as_blocked() {
    run_scenario("block_after_dispatch").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_decision_for_another_interrupt_never_runs_the_call() {
    run_scenario("stale").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_worker_replaced_before_dispatch_runs_the_approved_call_once() {
    run_scenario("crash_before_dispatch").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_worker_replaced_after_dispatch_refuses_as_may_have_started() {
    run_scenario("crash_after_dispatch").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_worker_replaced_after_forwarding_before_the_dispatch_record_runs_the_call_once() {
    run_scenario("crash_after_forward").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_failed_dispatch_record_ends_the_run_typed_and_never_runs_the_call() {
    run_scenario("dispatch_append_fails").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_late_duplicate_approval_after_a_dispatching_continuation_is_stale() {
    run_scenario("late_duplicate").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_worker_replaced_after_the_recorded_outcome_resumes_with_it() {
    run_scenario("crash_after_outcome").await;
}

/// How the fixture MCP tool behaves in this process.
#[derive(Clone, Copy, PartialEq)]
enum ToolMode {
    Normal,
    ExitOnConnect,
    ExitInExecute,
}

struct EffectConnector {
    mode: ToolMode,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl McpConnector for EffectConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "effect_fixture",
            vec![Arc::new(EffectTool {
                mode: self.mode,
                calls: Arc::clone(&self.calls),
            })],
        )))
    }
}

/// The effectful twin of the pipeline fixture's `lookup_release`.
struct EffectTool {
    mode: ToolMode,
    calls: Arc<AtomicUsize>,
}

#[async_trait]
impl Tool for EffectTool {
    fn name(&self) -> &'static str {
        "lookup_release"
    }
    fn description(&self) -> &'static str {
        "Record release evidence (changes data)."
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {"release": {"type": "string"}},
            "required": ["release"],
            "additionalProperties": false
        }))
    }
    fn is_read_only(&self) -> bool {
        false
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        match self.mode {
            ToolMode::ExitInExecute => std::process::exit(EXIT_AFTER_DISPATCH),
            ToolMode::ExitOnConnect | ToolMode::Normal => {}
        }
        self.calls.fetch_add(1, Ordering::AcqRel);
        Ok(json!({"recorded": arguments["release"], "effect": "written"}))
    }
}

/// The tool binding of the approving Worker: dies before any dispatch record.
struct ExitOnConnect;

#[async_trait]
impl McpConnector for ExitOnConnect {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        std::process::exit(EXIT_BEFORE_DISPATCH)
    }
}

fn worker(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    mode: ToolMode,
) -> (
    PipelineNativeAgentAssembler,
    CapturedModelRequests,
    Arc<AtomicUsize>,
) {
    let ((platform, model_facade, _, _), captured) =
        sensitive_direct_agent_pipeline_runtime_with(outcomes);
    let calls = Arc::new(AtomicUsize::new(0));
    let connector: Arc<dyn McpConnector> = if mode == ToolMode::ExitOnConnect {
        Arc::new(ExitOnConnect)
    } else {
        Arc::new(EffectConnector {
            mode,
            calls: Arc::clone(&calls),
        })
    };
    let assembler = PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        sensitive_pipeline_mcp_policy(),
        platform,
        model_facade,
    )
    .with_mcp_connector(connector);
    (assembler, captured, calls)
}

fn decision(
    card: &Value,
    action: &str,
    value: &str,
    digest: u8,
) -> super::super::request::AgentExecutionRequest {
    pipeline_application_resume_request(
        vec![card["interrupt_id"].as_str().expect("card id").to_owned()],
        action,
        value,
        [digest; 32],
    )
}

/// Run a resumed pipeline to its end and return the upstream code of its failure.
async fn failure_code(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Option<String> {
    invocation
        .project_start(timestamp(1))
        .expect("resume browser start");
    let (mut run, _projector, _completion) = invocation.start().expect("resume start");
    loop {
        match run.next_event().await {
            Err(error) => break error.upstream_code().map(str::to_owned),
            Ok(Some(_)) => {}
            Ok(None) => panic!("a dispatched approval must not complete again"),
        }
    }
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One child entry point owns every phase of the replaced process.
async fn postgres_effect_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let scenario = std::env::var(SCENARIO_ENV).expect("scenario");
    let phase = std::env::var(PHASE_ENV).expect("phase");
    let pending = std::env::var(PENDING_ENV).unwrap_or_default();
    let pool = connect(&database).await;
    let card = || serde_json::from_str::<Value>(&pending).expect("pending card");
    match phase.as_str() {
        "start" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(
                    pipeline_mcp_tool_call_response(),
                )],
                ToolMode::Normal,
            );
            let request = agent_pipeline_request("release-agent", "agent");
            let invocation = assembler
                .assemble(authorized_turn(&request, "execution-start", 1))
                .await
                .expect("start: assemble");
            let (interrupts, _) = collect_pipeline_pause(invocation).await;
            assert_eq!(interrupts.len(), 1, "one sensitive card: {interrupts:?}");
            let card = interrupts[0]["response_metadata"]["hitl_interrupts"][0].clone();
            assert_eq!(model_requests(&captured), 1);
            assert_eq!(
                calls.load(Ordering::Acquire),
                0,
                "start: the card holds the call"
            );
            println!("{PENDING_MARKER}{card}");
        }
        "approve" | "recover"
            if matches!(
                scenario.as_str(),
                "approve" | "crash_before_dispatch" | "crash_after_forward"
            ) =>
        {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    DONE,
                ))],
                ToolMode::Normal,
            );
            let (execution, attempt) = if phase == "recover" {
                (APPROVAL_EXECUTION, 3)
            } else {
                ("execution-approve", 2)
            };
            let invocation = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "approve", "", 41),
                    execution,
                    attempt,
                ))
                .await
                .expect("approve: the replacement Worker assembles");
            let browser = collect_pipeline_completion(invocation).await;
            assert!(
                browser.iter().any(|event| event["content"] == DONE),
                "{phase}: the approved call runs and the Agent finishes: {browser:?}"
            );
            // FU-236: the continued child's model request has a fresh turn's shape:
            // the call and its result, and no ADK confirmation bookkeeping text.
            let body: Value = serde_json::from_slice(&captured.lock().expect("captured")[0].body)
                .expect("model request JSON");
            let messages = body["messages"].as_array().expect("messages");
            assert!(
                !body.to_string().contains("Tool confirmation required"),
                "{phase}: the confirmation notice reaches the provider: {body}"
            );
            let roles: Vec<&str> = messages
                .iter()
                .map(|message| message["role"].as_str().unwrap_or_default())
                .collect();
            assert_eq!(
                roles,
                ["system", "user", "assistant", "tool"],
                "{phase}: {body}"
            );
            assert_eq!(
                messages[2]["tool_calls"][0]["function"]["name"], "lookup_release",
                "{phase}: the approved call is intact"
            );
            assert_eq!(
                calls.load(Ordering::Acquire),
                1,
                "{phase}: runs exactly once"
            );
            assert_eq!(
                model_requests(&captured),
                1,
                "{phase}: only the resumed Agent runs"
            );
            println!("{PENDING_MARKER}{}", card());
        }
        "repeat" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "unexpected second run",
                ))],
                ToolMode::Normal,
            );
            if let Ok(invocation) = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "approve", "", 42),
                    "execution-repeat",
                    3,
                ))
                .await
            {
                let _ = tokio::time::timeout(
                    std::time::Duration::from_secs(30),
                    collect_pipeline_completion(invocation),
                )
                .await;
            }
            assert_eq!(calls.load(Ordering::Acquire), 0, "repeat: never runs again");
            assert_eq!(model_requests(&captured), 0, "repeat: resumes nothing");
        }
        "block" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "child summarized the block",
                ))],
                ToolMode::Normal,
            );
            let invocation = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "block_with_comment", "not this one", 43),
                    "execution-block",
                    2,
                ))
                .await
                .expect("block: assembles");
            let browser = collect_pipeline_completion(invocation).await;
            assert!(
                browser
                    .iter()
                    .any(|event| event["content"] == "child summarized the block"),
                "block: the Agent resumes with the blocked result: {browser:?}"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "block: never runs");
            assert_eq!(model_requests(&captured), 1);
        }
        "crash" => {
            let (outcomes, mode) = match scenario.as_str() {
                "crash_before_dispatch" => (Vec::new(), ToolMode::ExitOnConnect),
                "crash_after_dispatch" | "block_after_dispatch" => {
                    (Vec::new(), ToolMode::ExitInExecute)
                }
                // The tool runs and its outcome is recorded; the Worker dies at the
                // child's next model request, before the parent continues.
                "crash_after_forward" => {
                    crate::agents::dispatch_journal::EXIT_BEFORE_DISPATCHED_RECORD
                        .store(EXIT_AFTER_FORWARD, Ordering::Release);
                    (Vec::new(), ToolMode::ExitInExecute)
                }
                "crash_after_outcome" | "late_duplicate" => (
                    vec![TestModelGatewayOutcome::ExitProcess(EXIT_AFTER_OUTCOME)],
                    ToolMode::Normal,
                ),
                other => panic!("no crash phase in {other}"),
            };
            let (assembler, _, _) = worker(&pool, outcomes, mode);
            let invocation = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "approve", "", 44),
                    APPROVAL_EXECUTION,
                    2,
                ))
                .await
                .expect("crash: the Worker assembles the approval");
            let _ = failure_code(invocation).await;
            panic!("crash: the Worker did not die where the scenario expects");
        }
        "recover" if scenario == "crash_after_dispatch" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "unexpected second effect",
                ))],
                ToolMode::Normal,
            );
            // The pause stays answerable for the same decision; its dispatch record
            // without an outcome refuses the call before it binds a second time.
            let invocation = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "approve", "", 44),
                    APPROVAL_EXECUTION,
                    3,
                ))
                .await
                .expect("recover: assembles");
            let upstream = failure_code(invocation).await;
            assert_eq!(
                crate::protocol::output::model_failure(upstream.as_deref()),
                crate::protocol::output::RuntimeFailureKind::RecoveryRefused,
                "recover: the may-have-started reason, got {upstream:?}"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "recover: never repeated");
            assert_eq!(model_requests(&captured), 0, "recover: no model call");
        }
        "recover" if matches!(scenario.as_str(), "crash_after_outcome" | "late_duplicate") => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    DONE,
                ))],
                ToolMode::Normal,
            );
            // Claims are monotonic per session: after `late`, the replacement's is 4.
            let attempt = if scenario == "late_duplicate" { 4 } else { 3 };
            let invocation = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "approve", "", 44),
                    APPROVAL_EXECUTION,
                    attempt,
                ))
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "recover: assemble refused: {error} {:?}",
                        error.cause().map(
                            elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
                        )
                    )
                });
            let browser = collect_pipeline_completion(invocation).await;
            assert!(
                browser.iter().any(|event| event["content"] == DONE),
                "recover: the run continues with the recorded outcome: {browser:?}"
            );
            assert_eq!(
                calls.load(Ordering::Acquire),
                0,
                "recover: the effect is not repeated"
            );
            assert_eq!(
                model_requests(&captured),
                1,
                "recover: only the next model step runs"
            );
            let request =
                String::from_utf8_lossy(&captured.lock().expect("captured")[0].body).into_owned();
            assert!(
                request.contains("written"),
                "recover: the model sees the recorded outcome: {request}"
            );
        }
        "append_fails" => {
            crate::agents::dispatch_journal::FAIL_DISPATCHED_APPEND.store(true, Ordering::Release);
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "unexpected answer past the approved call",
                ))],
                ToolMode::Normal,
            );
            let invocation = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "approve", "", 44),
                    APPROVAL_EXECUTION,
                    2,
                ))
                .await
                .expect("append_fails: assembles");
            let upstream = failure_code(invocation).await;
            assert_eq!(
                crate::protocol::output::model_failure(upstream.as_deref()),
                crate::protocol::output::RuntimeFailureKind::DependencyUnavailable,
                "append_fails: a typed failure, got {upstream:?}"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "append_fails: never runs");
            assert_eq!(
                model_requests(&captured),
                0,
                "append_fails: nothing answers past it"
            );
        }
        "late" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "unexpected duplicate continuation",
                ))],
                ToolMode::Normal,
            );
            // Another execution submits the same approval. Nothing moved the
            // checkpoint, but the records behind the pause are the dead
            // execution's: this one is a duplicate and is stale.
            match assembler
                .assemble(authorized_turn(
                    &decision(&card(), "approve", "", 44),
                    "execution-late",
                    3,
                ))
                .await
            {
                Err(error) => assert_eq!(
                    error.code(),
                    crate::agents::runtime::NativeAgentAssemblyErrorCode::InvalidInput,
                    "late: refused as stale, got {error}"
                ),
                Ok(invocation) => panic!(
                    "late: a duplicate approval was assembled (failure {:?})",
                    failure_code(invocation).await
                ),
            }
            assert_eq!(calls.load(Ordering::Acquire), 0, "late: never runs");
            assert_eq!(model_requests(&captured), 0, "late: no model call");
        }
        "block_late" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "unexpected blocked summary",
                ))],
                ToolMode::Normal,
            );
            if let Ok(invocation) = assembler
                .assemble(authorized_turn(
                    &decision(&card(), "block_with_comment", "too late", 47),
                    "execution-block-late",
                    3,
                ))
                .await
            {
                assert!(
                    failure_code(invocation).await.is_some(),
                    "block_late: a block of a dispatched call must not complete"
                );
            }
            assert_eq!(calls.load(Ordering::Acquire), 0, "block_late: never runs");
            assert_eq!(model_requests(&captured), 0, "block_late: no model call");
        }
        "stale" => {
            let (assembler, _, calls) = worker(&pool, Vec::new(), ToolMode::Normal);
            let mut foreign = card();
            foreign["interrupt_id"] = json!("hitl_e1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
            if let Ok(invocation) = assembler
                .assemble(authorized_turn(
                    &decision(&foreign, "approve", "", 45),
                    "execution-stale",
                    2,
                ))
                .await
            {
                assert!(
                    failure_code(invocation).await.is_some(),
                    "stale: a decision for another card fails"
                );
            }
            assert_eq!(calls.load(Ordering::Acquire), 0, "stale: never runs");
        }
        other => panic!("unknown phase {other} in {scenario}"),
    }
    pool.close().await;
}
