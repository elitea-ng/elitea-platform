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
const GENERATION: u64 = 3;
const DONE: &str = "root finished after the approved change";
const LATER: &str = "root answered the later turn";

fn phases(scenario: &str) -> &'static [(&'static str, i32)] {
    match scenario {
        "approve" => &[("start", 0), ("approve", 0), ("repeat", 0)],
        "later_turn" => &[("start", 0), ("approve", 0), ("later", 0)],
        "block" => &[("start", 0), ("block", 0)],
        "crash_after_dispatch" => &[("start", 0), ("crash", EXIT_AFTER_DISPATCH), ("recover", 0)],
        "stale" => &[("start", 0), ("stale", 0)],
        // The tool runs and its result is persisted; the Worker dies at the next model
        // request, twice; the third Worker continues with the recorded result.
        "crash_after_outcome_twice" => &[
            ("start", 0),
            ("outcome_crash", EXIT_AFTER_OUTCOME),
            ("outcome_crash_again", EXIT_AFTER_OUTCOME),
            ("outcome_recover", 0),
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
async fn postgres_root_blocked_effectful_call_never_runs() {
    run_scenario("block").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_worker_replaced_after_dispatch_refuses_as_may_have_started() {
    run_scenario("crash_after_dispatch").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_recorded_outcome_survives_two_replaced_workers_without_a_rerun() {
    run_scenario("crash_after_outcome_twice").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_decision_for_another_interrupt_is_refused() {
    run_scenario("stale").await;
}

struct EffectConnector {
    exit_in_execute: bool,
    calls: Arc<AtomicUsize>,
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
            })],
        )))
    }
}

struct EffectTool {
    exit_in_execute: bool,
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
    let (model_gateway, captured) =
        test_model_gateway_client(outcomes, test_model_gateway_config()).expect("model gateway");
    let calls = Arc::new(AtomicUsize::new(0));
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform_client(runtime_context),
        Arc::new(ModelFacade::from_gateway(model_gateway)),
        sensitive_mcp_policy(),
    )
    .with_mcp_connector(Arc::new(EffectConnector {
        exit_in_execute,
        calls: Arc::clone(&calls),
    }))
    .with_postgres_sessions(pool.clone(), SessionLimits::default());
    (assembler, captured, calls)
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
        "crash" => {
            let (assembler, _, _) = worker(&pool, Vec::new(), true);
            let invocation = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 44),
                    "execution-crash",
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
            let Err(error) = assembler
                .assemble(turn(
                    &decision(&card(), "approve", "", 44),
                    "execution-recover",
                    3,
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
                    &format!("execution-{phase}"),
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
                    "execution-outcome-recover",
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
