//! FU-226 at the conversation root: an Agent's approved sensitive call that changes data runs
//! exactly once across Worker process replacement against real `PostgreSQL`.
//!
//! The root Runner persists the replayed call event through the fenced session append before ADK
//! runs the tool; that event is the dispatch marker. Each phase runs in its own OS process and
//! claim. Scenarios:
//!
//! * `approve`: `start` pauses; `approve` runs the tool once; `repeat` runs nothing.
//! * `block`: the Agent continues with the blocked result; the tool never runs.
//! * `crash_after_dispatch`: the approving Worker dies inside the tool; `recover` refuses with the
//!   "may have started" cause and never calls the tool.
//! * `stale`: a decision naming another interrupt is refused; the tool never runs.
//! * `precheck_crash`: one message calls two sensitive tools. A is approved; the Worker dies after
//!   the replayed message is persisted and before ADK's pre-check pause on B. Nothing ran, so the
//!   replacement continues A (B's card again), and after B is approved each call runs once.
//! * `nested_outcome_crash`: a nested Agent's approved call runs and its outcome is journaled;
//!   the Worker dies at the child's next model request. The replacement serves the recorded
//!   outcome (class R) and never calls the tool again.
//!
//! * `dispatch_append_fails`: the approved call's `dispatched` append fails; the run ends with a
//!   typed failure before any model step and never runs the call.
//!
//! A replacement is the same execution re-claimed with a higher claim attempt, as Main does after
//! a lease expires.

use super::*;
use crate::agents::pipeline_tests::restart_pg_tests::{
    CHECKPOINT_MIGRATION, PHASE_DEADLINE, connect, run_phase,
};
use crate::agents::runtime::{AssembledNativeAgentInvocation, AuthorizedNativeAssembly};
use crate::protocol::control::test_session_authority_for_claim;
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use crate::state::{SessionLimits, TestStateWriterLease};
use crate::toolkits::{McpConnector, McpMaterializationError, RemoteMcpConfig};

const CHILD_TEST: &str = "agents::ordinary_tests::effect_pg_tests::postgres_root_effect_child";
const DATABASE_ENV: &str = "ELITEA_ROOT_EFFECT_PG_DATABASE";
const SCENARIO_ENV: &str = "ELITEA_ROOT_EFFECT_PG_SCENARIO";
const PHASE_ENV: &str = "ELITEA_ROOT_EFFECT_PG_PHASE";
const PENDING_ENV: &str = "ELITEA_ROOT_EFFECT_PG_PENDING";
const PENDING_MARKER: &str = "ROOT_EFFECT_PENDING=";
const EXIT_AFTER_DISPATCH: i32 = 86;
const EXIT_AFTER_OUTCOME: i32 = 87;
const EXIT_IN_BLOCKED_RESULT: i32 = 88;
const EXIT_BEFORE_SIBLING_PAUSE: i32 = 89;
/// The execution that approves and dies; its replacement re-claims it.
const APPROVAL_EXECUTION: &str = "execution-approval";
const GENERATION: u64 = 3;
const DONE: &str = "root finished after the approved change";
const LATER: &str = "root answered the later turn";

fn phases(scenario: &str) -> &'static [(&'static str, i32)] {
    match scenario {
        "approve" => &[("start", 0), ("approve", 0), ("repeat", 0)],
        "later_turn" => &[("start", 0), ("approve", 0), ("later", 0)],
        "block" => &[("start", 0), ("block", 0)],
        // The Worker dies between the replayed call event and the local blocked
        // result; the replacement continues the same Deny and serves it again.
        "block_crash" => &[
            ("start", 0),
            ("block_crash", EXIT_IN_BLOCKED_RESULT),
            ("block_recover", 0),
        ],
        "crash_after_dispatch" => &[("start", 0), ("crash", EXIT_AFTER_DISPATCH), ("recover", 0)],
        // Another execution submits the same approval after the dead one dispatched:
        // stale, not "may have started"; the dead execution's replacement refuses.
        "duplicate_after_dispatch" => &[
            ("start", 0),
            ("crash", EXIT_AFTER_DISPATCH),
            ("duplicate", 0),
            ("recover", 0),
        ],
        "stale" => &[("start", 0), ("stale", 0)],
        // The tool runs and its result is persisted; the Worker dies at the next model
        // request, twice; the third Worker continues with the recorded result.
        "crash_after_outcome_twice" => &[
            ("start", 0),
            ("outcome_crash", EXIT_AFTER_OUTCOME),
            ("outcome_crash_again", EXIT_AFTER_OUTCOME),
            ("outcome_recover", 0),
        ],
        "dispatch_append_fails" => &[("start", 0), ("append_fails", 0)],
        "precheck_crash" => &[
            ("start_two", 0),
            ("precheck_crash", EXIT_BEFORE_SIBLING_PAUSE),
            ("recover_first", 0),
            ("approve_second", 0),
        ],
        "nested_outcome_crash" => &[
            ("nested_start", 0),
            ("nested_crash", EXIT_AFTER_OUTCOME),
            ("nested_recover", 0),
        ],
        other => panic!("unknown scenario {other}"),
    }
}

async fn run_scenario(scenario: &str) {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the root approved-effect proof");
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
async fn postgres_root_approved_effectful_call_runs_once_and_a_repeat_runs_nothing() {
    run_scenario("approve").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_later_turn_after_an_approval_sends_one_call_and_result() {
    run_scenario("later_turn").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_block_interrupted_before_its_result_resumes_and_runs_nothing() {
    run_scenario("block_crash").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_blocked_effectful_call_never_runs() {
    run_scenario("block").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_worker_replaced_after_dispatch_refuses_as_may_have_started() {
    run_scenario("crash_after_dispatch").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_duplicate_after_a_dispatch_is_stale_and_runs_nothing() {
    run_scenario("duplicate_after_dispatch").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_recorded_outcome_survives_two_replaced_workers_without_a_rerun() {
    run_scenario("crash_after_outcome_twice").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_failed_dispatch_record_ends_the_run_typed_and_never_runs_the_call() {
    run_scenario("dispatch_append_fails").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_replayed_message_before_a_sibling_pause_is_no_dispatch() {
    run_scenario("precheck_crash").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_nested_recorded_outcome_is_served_after_process_replacement() {
    run_scenario("nested_outcome_crash").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_decision_for_another_interrupt_is_refused() {
    run_scenario("stale").await;
}

struct EffectConnector {
    exit_in_execute: bool,
    calls: Arc<AtomicUsize>,
    releases: Arc<std::sync::Mutex<Vec<String>>>,
}

#[async_trait]
impl McpConnector for EffectConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "root_effect_fixture",
            vec![Arc::new(EffectTool {
                exit_in_execute: self.exit_in_execute,
                calls: Arc::clone(&self.calls),
                releases: Arc::clone(&self.releases),
            })],
        )))
    }
}

struct EffectTool {
    exit_in_execute: bool,
    calls: Arc<AtomicUsize>,
    releases: Arc<std::sync::Mutex<Vec<String>>>,
}

#[async_trait]
impl Tool for EffectTool {
    fn name(&self) -> &'static str {
        "lookup_release"
    }
    fn description(&self) -> &'static str {
        "Record release evidence (changes data)."
    }
    fn parameters_schema(&self) -> Option<serde_json::Value> {
        Some(serde_json::json!({
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
        arguments: serde_json::Value,
    ) -> adk_rust::Result<serde_json::Value> {
        if self.exit_in_execute {
            std::process::exit(EXIT_AFTER_DISPATCH);
        }
        self.calls.fetch_add(1, Ordering::AcqRel);
        self.releases
            .lock()
            .expect("releases")
            .push(arguments["release"].as_str().unwrap_or_default().to_owned());
        Ok(serde_json::json!({"recorded": arguments["release"], "effect": "written"}))
    }
}

fn worker(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    exit_in_execute: bool,
) -> (
    OrdinaryNativeAgentAssembler,
    crate::transport::openai_compatible_facade::CapturedModelRequests,
    Arc<AtomicUsize>,
) {
    let (runtime_context, _) = runtime_context_client_for_redemptions(2);
    let (assembler, captured, calls, _) =
        worker_with(pool, runtime_context, outcomes, exit_in_execute);
    (assembler, captured, calls)
}

/// A Worker over `runtime_context`; also returns the releases the tool wrote, in order.
fn worker_with(
    pool: &sqlx::PgPool,
    runtime_context: RuntimeContextClient,
    outcomes: Vec<TestModelGatewayOutcome>,
    exit_in_execute: bool,
) -> (
    OrdinaryNativeAgentAssembler,
    crate::transport::openai_compatible_facade::CapturedModelRequests,
    Arc<AtomicUsize>,
    Arc<std::sync::Mutex<Vec<String>>>,
) {
    let (model_gateway, captured) =
        test_model_gateway_client(outcomes, test_model_gateway_config()).expect("model gateway");
    let calls = Arc::new(AtomicUsize::new(0));
    let releases = Arc::new(std::sync::Mutex::new(Vec::new()));
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        sensitive_mcp_policy(),
    )
    .with_mcp_connector(Arc::new(EffectConnector {
        exit_in_execute,
        calls: Arc::clone(&calls),
        releases: Arc::clone(&releases),
    }))
    .with_postgres_sessions(pool.clone(), SessionLimits::default());
    (assembler, captured, calls, releases)
}

/// One assistant message calling the sensitive tool twice (releases `a` and `b`).
fn two_sensitive_calls_response() -> Response<Body> {
    let calls: Vec<_> = [("call_a", "a"), ("call_b", "b")]
        .iter()
        .enumerate()
        .map(|(index, (id, release))| {
            serde_json::json!({
                "index": index,
                "id": id,
                "type": "function",
                "function": {
                    "name": "lookup_release",
                    "arguments": format!("{{\"release\":\"{release}\"}}")
                }
            })
        })
        .collect();
    let frame =
        serde_json::json!({"choices":[{"delta":{"tool_calls":calls},"finish_reason":null}]});
    let raw = format!(
        "data: {frame}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

fn nested_request(digest: u8) -> super::super::request::AgentExecutionRequest {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    request.binding.request_content_digest = [digest; 32];
    attach_nested_agent(&mut request);
    request
}

fn turn<'a>(
    request: &'a super::super::request::AgentExecutionRequest,
    execution_id: &str,
    claim_ordinal: u8,
) -> AuthorizedNativeAssembly<'a> {
    AuthorizedNativeAssembly::from_authorized(
        request,
        test_runtime_context_authority(),
        test_session_authority_for_claim(execution_id, GENERATION, claim_ordinal),
        Arc::new(TestStateWriterLease::current()),
        AuthorizedNativeCommandBinding::fixture_for_execution(execution_id),
    )
}

fn request() -> super::super::request::AgentExecutionRequest {
    let mut request = ordinary_request(AgentExecutionKind::Application);
    attach_remote_mcp_tool(&mut request);
    request
}

fn decision(
    card: &serde_json::Value,
    action: &str,
    value: &str,
    digest: u8,
) -> super::super::request::AgentExecutionRequest {
    let mut resume = request();
    resume.binding.request_content_digest = [digest; 32];
    resume.payload.should_continue = true;
    resume.payload.hitl_resume = true;
    resume.payload.hitl_action = Some(action.to_owned());
    resume.payload.hitl_value = Some(value.to_owned());
    resume.payload.hitl_decisions = vec![serde_json::json!({
        "interrupt_id": card["interrupt_id"],
        "tool_call_id": card["tool_call_id"],
        "action": action,
        "value": value,
    })];
    resume
}

/// Run one turn to its end: the browser events and the stream failure, if any.
async fn run_turn(
    mut invocation: AssembledNativeAgentInvocation<
        impl super::super::runtime::NativeAgentCompletionSelector,
    >,
) -> (Vec<serde_json::Value>, Option<String>) {
    invocation
        .project_start(chrono::Utc::now())
        .expect("browser start");
    let (mut run, mut projector, _completion) = invocation.start().expect("native start");
    let mut browser = Vec::new();
    loop {
        match run.next_event().await {
            Ok(Some(event)) => browser.extend(
                projector
                    .project(&event)
                    .expect("projection")
                    .into_iter()
                    .map(|event| {
                        serde_json::from_slice::<serde_json::Value>(
                            &encode_current_node_event_json(&event).expect("browser event"),
                        )
                        .expect("browser JSON")
                    }),
            ),
            Ok(None) => return (browser, None),
            Err(error) => return (browser, error.upstream_code().map(str::to_owned)),
        }
    }
}

fn card_of(browser: &[serde_json::Value]) -> serde_json::Value {
    fn find(value: &serde_json::Value) -> Option<serde_json::Value> {
        match value {
            serde_json::Value::Object(map) => {
                if map.contains_key("interrupt_id") && map.contains_key("tool_call_id") {
                    return Some(value.clone());
                }
                map.values().find_map(find)
            }
            serde_json::Value::Array(items) => items.iter().find_map(find),
            _ => None,
        }
    }
    browser
        .iter()
        .find_map(find)
        .unwrap_or_else(|| panic!("a sensitive card: {browser:?}"))
}

fn said(browser: &[serde_json::Value], text: &str) -> bool {
    browser
        .iter()
        .any(|event| serde_json::to_string(event).is_ok_and(|raw| raw.contains(text)))
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One child entry point owns every phase of the replaced process.
async fn postgres_root_effect_child() {
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
    let card = || serde_json::from_str::<serde_json::Value>(&pending).expect("pending card");
    match phase.as_str() {
        "start" => {
            let (assembler, _, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(mcp_tool_call_response())],
                false,
            );
            let request = request();
            let invocation = assembler
                .assemble(turn(&request, "execution-start", 1))
                .await
                .unwrap_or_else(|error| panic!("start: assemble refused: {error}"));
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "start: {failure:?}");
            assert_eq!(
                calls.load(Ordering::Acquire),
                0,
                "start: the card holds the call"
            );
            println!("{PENDING_MARKER}{}", card_of(&browser));
        }
        "approve" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(DONE))],
                false,
            );
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 41),
                    "execution-approve",
                    2,
                ))
                .await
                .expect("approve: the replacement Worker assembles");
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "approve: {failure:?}");
            assert!(
                said(&browser, DONE),
                "approve: the Agent finishes: {browser:?}"
            );
            assert_eq!(
                calls.load(Ordering::Acquire),
                1,
                "approve: runs exactly once"
            );
            assert_eq!(captured.lock().expect("captured").len(), 1);
            println!("{PENDING_MARKER}{}", card());
        }
        "repeat" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(
                    "unexpected",
                ))],
                false,
            );
            if let Ok(invocation) = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 42),
                    "execution-repeat",
                    3,
                ))
                .await
            {
                let _ =
                    tokio::time::timeout(std::time::Duration::from_secs(30), run_turn(invocation))
                        .await;
            }
            assert_eq!(calls.load(Ordering::Acquire), 0, "repeat: never runs again");
            assert_eq!(
                captured.lock().expect("captured").len(),
                0,
                "repeat: no model call"
            );
        }
        "later" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(LATER))],
                false,
            );
            let mut next = request();
            next.binding.request_content_digest = [47; 32];
            next.payload.user_input = super::super::request::UserInput::Text("run it again".into());
            let invocation = assembler
                .assemble(turn(&next, "execution-later", 3))
                .await
                .expect("later: a new user turn assembles");
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "later: {failure:?}");
            assert!(
                said(&browser, LATER),
                "later: the Agent answers: {browser:?}"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "later: nothing re-runs");
            let body: serde_json::Value =
                serde_json::from_slice(&captured.lock().expect("captured")[0].body)
                    .expect("model request JSON");
            // A later turn has a fresh turn's shape: the decided call, its result,
            // the answer as one string, the new user turn. No runtime bookkeeping.
            let raw = body.to_string();
            assert!(
                !raw.contains("[Elitea direct HITL") && !raw.contains("Tool confirmation required"),
                "later: runtime bookkeeping reaches the provider: {body}"
            );
            let messages = body["messages"].as_array().expect("messages");
            let roles: Vec<&str> = messages
                .iter()
                .map(|message| message["role"].as_str().unwrap_or_default())
                .collect();
            assert_eq!(
                roles,
                ["system", "user", "assistant", "tool", "assistant", "user"],
                "later: {body}"
            );
            assert_eq!(messages[2]["tool_calls"].as_array().map(Vec::len), Some(1));
            assert_eq!(
                messages[4]["content"], DONE,
                "later: the earlier answer is one string: {body}"
            );
        }
        "block" => {
            let (assembler, _, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(DONE))],
                false,
            );
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "block_with_comment", "not this one", 43),
                    "execution-block",
                    2,
                ))
                .await
                .expect("block: assembles");
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "block: {failure:?}");
            assert!(
                said(&browser, DONE),
                "block: the Agent continues: {browser:?}"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "block: never runs");
        }
        "block_crash" => {
            super::super::direct_hitl::EXIT_IN_BLOCKED_REPLAY
                .store(EXIT_IN_BLOCKED_RESULT, Ordering::Release);
            let (assembler, _, _) = worker(&pool, Vec::new(), false);
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "block_with_comment", "not this one", 48),
                    "execution-block-crash",
                    2,
                ))
                .await
                .expect("block_crash: assembles");
            let _ = run_turn(invocation).await;
            panic!("block_crash: the Worker did not die inside the blocked result");
        }
        "block_recover" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(DONE))],
                false,
            );
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "block_with_comment", "not this one", 49),
                    "execution-block-recover",
                    3,
                ))
                .await
                .unwrap_or_else(|error| panic!("block_recover: the same Deny resumes: {error}"));
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "block_recover: {failure:?}");
            assert!(
                said(&browser, DONE),
                "block_recover: the Agent continues: {browser:?}"
            );
            assert_eq!(
                calls.load(Ordering::Acquire),
                0,
                "block_recover: never runs"
            );
            let captured = captured.lock().expect("captured");
            assert_eq!(captured.len(), 1, "block_recover: one model call");
            let body = String::from_utf8_lossy(&captured[0].body);
            assert!(
                body.contains("sensitive_tool_blocked") && body.contains("not this one"),
                "block_recover: the model sees the blocked result: {body}"
            );
        }
        "crash" => {
            let (assembler, _, _) = worker(&pool, Vec::new(), true);
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 44),
                    APPROVAL_EXECUTION,
                    2,
                ))
                .await
                .expect("crash: assembles");
            let _ = run_turn(invocation).await;
            panic!("crash: the Worker did not die inside the tool");
        }
        "recover" => {
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(
                    "unexpected",
                ))],
                false,
            );
            // Claims are monotonic per session: after `duplicate`, the replacement's is 4.
            let attempt = if scenario == "duplicate_after_dispatch" {
                4
            } else {
                3
            };
            let Err(error) = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 44),
                    APPROVAL_EXECUTION,
                    attempt,
                ))
                .await
            else {
                panic!("recover: a dispatched approval was assembled again");
            };
            assert_eq!(
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code),
                Some(crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE),
                "recover: the may-have-started cause: {error}"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "recover: never repeated");
            assert_eq!(captured.lock().expect("captured").len(), 0);
        }
        "duplicate" => {
            let (assembler, captured, calls) = worker(&pool, Vec::new(), true);
            let Err(error) = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 44),
                    "execution-duplicate",
                    3,
                ))
                .await
            else {
                panic!("duplicate: a second execution's approval was assembled");
            };
            assert_eq!(
                error.code(),
                crate::agents::runtime::NativeAgentAssemblyErrorCode::InvalidInput,
                "duplicate: stale, got {error}"
            );
            assert_ne!(
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code),
                Some(crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE),
                "duplicate: not the may-have-started reason"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0);
            assert_eq!(captured.lock().expect("captured").len(), 0);
        }
        "outcome_crash" | "outcome_crash_again" => {
            // The second Worker's tool dies if it is ever called: reaching the model
            // request (exit 87) instead proves the recorded outcome was served.
            let (assembler, _, _) = worker(
                &pool,
                vec![TestModelGatewayOutcome::ExitProcess(EXIT_AFTER_OUTCOME)],
                phase == "outcome_crash_again",
            );
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 46),
                    APPROVAL_EXECUTION,
                    if phase == "outcome_crash" { 2 } else { 3 },
                ))
                .await
                .unwrap_or_else(|error| panic!("{phase}: assemble refused: {error}"));
            let _ = run_turn(invocation).await;
            panic!("{phase}: the Worker did not die at the model request");
        }
        "outcome_recover" => {
            let (assembler, _, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(DONE))],
                false,
            );
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 46),
                    APPROVAL_EXECUTION,
                    4,
                ))
                .await
                .unwrap_or_else(|error| panic!("outcome_recover: assemble refused: {error}"));
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "outcome_recover: {failure:?}");
            assert!(
                said(&browser, DONE),
                "outcome_recover: the Agent finishes: {browser:?}"
            );
            assert_eq!(
                calls.load(Ordering::Acquire),
                0,
                "outcome_recover: never re-run"
            );
        }
        "append_fails" => {
            crate::agents::dispatch_journal::FAIL_DISPATCHED_APPEND.store(true, Ordering::Release);
            let (assembler, captured, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(
                    "unexpected answer past the approved call",
                ))],
                false,
            );
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 52),
                    APPROVAL_EXECUTION,
                    2,
                ))
                .await
                .expect("append_fails: assembles");
            let (browser, failure) = run_turn(invocation).await;
            assert_eq!(
                crate::protocol::output::model_failure(failure.as_deref()),
                crate::protocol::output::RuntimeFailureKind::DependencyUnavailable,
                "append_fails: a typed failure, got {failure:?}"
            );
            assert!(
                !said(&browser, "unexpected answer"),
                "append_fails: nothing answers past the call: {browser:?}"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "append_fails: never runs");
            assert_eq!(captured.lock().expect("captured").len(), 0);
        }
        "start_two" => {
            let (assembler, _, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(
                    two_sensitive_calls_response(),
                )],
                false,
            );
            let invocation = assembler
                .assemble(turn(&request(), "execution-start", 1))
                .await
                .unwrap_or_else(|error| panic!("start_two: assemble refused: {error}"));
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "start_two: {failure:?}");
            let card = card_of(&browser);
            assert_eq!(card["tool_call_id"], "call_a", "start_two: A's card first");
            assert_eq!(calls.load(Ordering::Acquire), 0);
            println!("{PENDING_MARKER}{card}");
        }
        "precheck_crash" => {
            crate::agents::session::EXIT_BEFORE_CONFIRMATION_APPEND
                .store(EXIT_BEFORE_SIBLING_PAUSE, Ordering::Release);
            let (assembler, _, _) = worker(&pool, Vec::new(), true);
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 50),
                    APPROVAL_EXECUTION,
                    2,
                ))
                .await
                .expect("precheck_crash: assembles");
            let _ = run_turn(invocation).await;
            panic!("precheck_crash: the Worker did not die before B's pause");
        }
        "recover_first" => {
            // Its tool exits if it is called: nothing may run before B is decided.
            let (assembler, captured, _) = worker(&pool, Vec::new(), true);
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 50),
                    APPROVAL_EXECUTION,
                    3,
                ))
                .await
                .unwrap_or_else(|error| {
                    panic!("recover_first: the replayed message alone is no dispatch: {error}")
                });
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "recover_first: {failure:?}");
            let second = card_of(&browser);
            assert_eq!(second["tool_call_id"], "call_b", "recover_first: B's card");
            assert_eq!(captured.lock().expect("captured").len(), 0);
            println!("{PENDING_MARKER}{second}");
        }
        "approve_second" => {
            let (runtime_context, _) = runtime_context_client_for_redemptions(2);
            let (assembler, _, calls, releases) = worker_with(
                &pool,
                runtime_context,
                vec![TestModelGatewayOutcome::Response(text_response(DONE))],
                false,
            );
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 51),
                    "execution-approve-second",
                    4,
                ))
                .await
                .unwrap_or_else(|error| panic!("approve_second: assemble refused: {error}"));
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "approve_second: {failure:?}");
            assert!(said(&browser, DONE), "approve_second: {browser:?}");
            assert_eq!(calls.load(Ordering::Acquire), 2);
            let mut releases = releases.lock().expect("releases").clone();
            releases.sort_unstable();
            assert_eq!(releases, ["a", "b"], "approve_second: each call runs once");
        }
        "nested_start" => {
            let (runtime_context, _) = runtime_context_with_sensitive_child();
            let (assembler, _, calls, _) = worker_with(
                &pool,
                runtime_context,
                vec![
                    TestModelGatewayOutcome::Response(nested_agent_call_response()),
                    TestModelGatewayOutcome::Response(mcp_tool_call_response()),
                ],
                false,
            );
            let invocation = assembler
                .assemble(turn(&nested_request(60), "execution-start", 1))
                .await
                .unwrap_or_else(|error| panic!("nested_start: assemble refused: {error}"));
            let (browser, failure) = run_turn(invocation).await;
            assert!(failure.is_none(), "nested_start: {failure:?}");
            assert_eq!(calls.load(Ordering::Acquire), 0);
            println!("{PENDING_MARKER}{}", card_of(&browser));
        }
        "nested_crash" | "nested_recover" => {
            let recover = phase == "nested_recover";
            let (runtime_context, _) = runtime_context_with_sensitive_child();
            // The replacement's tool exits if it is ever called.
            let (assembler, captured, calls, _) = worker_with(
                &pool,
                runtime_context,
                if recover {
                    vec![
                        TestModelGatewayOutcome::Response(text_response(
                            "child recorded the release",
                        )),
                        TestModelGatewayOutcome::Response(text_response(DONE)),
                    ]
                } else {
                    vec![TestModelGatewayOutcome::ExitProcess(EXIT_AFTER_OUTCOME)]
                },
                recover,
            );
            let mut approve = nested_request(61);
            admit_approved_resume(
                &mut approve,
                card()["interrupt_id"].as_str().expect("interrupt id"),
            );
            let invocation = assembler
                .assemble(turn(
                    &approve,
                    APPROVAL_EXECUTION,
                    if recover { 3 } else { 2 },
                ))
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "{phase}: assemble refused: {error} {:?}",
                        error.cause().map(
                            elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
                        )
                    )
                });
            let (browser, failure) = run_turn(invocation).await;
            assert!(
                recover,
                "nested_crash: the Worker did not die at the child's model request"
            );
            assert!(failure.is_none(), "nested_recover: {failure:?}");
            assert!(said(&browser, DONE), "nested_recover: {browser:?}");
            assert_eq!(
                calls.load(Ordering::Acquire),
                0,
                "nested_recover: never re-run"
            );
            let captured = captured.lock().expect("captured");
            let child = String::from_utf8_lossy(&captured[0].body);
            assert!(
                child.contains("written"),
                "nested_recover: the child model sees the recorded outcome: {child}"
            );
            println!("{PENDING_MARKER}{}", card());
        }
        "stale" => {
            let (assembler, _, calls) = worker(
                &pool,
                vec![TestModelGatewayOutcome::Response(text_response(
                    "unexpected",
                ))],
                false,
            );
            let mut foreign = card();
            foreign["interrupt_id"] =
                serde_json::json!("hitl_e1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
            let result = assembler
                .assemble(turn(
                    &decision(&foreign, "approve", "", 45),
                    "execution-stale",
                    2,
                ))
                .await;
            assert!(
                result.is_err(),
                "stale: a decision for another interrupt is refused"
            );
            assert_eq!(calls.load(Ordering::Acquire), 0, "stale: never runs");
        }
        other => panic!("unknown phase {other} in {scenario}"),
    }
    pool.close().await;
}
