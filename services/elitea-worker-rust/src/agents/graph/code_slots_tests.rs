//! W3-1: Code slots per Supervisor target and deadline admission on the raw
//! path (`execute`, `execute_tool`, the graph Code node without recovery).
//! No control, sandbox or database socket answers: an admitted attempt fails
//! later, at the never-connecting dispatch journal, which proves it passed
//! admission; a refused one never gets that far.
use super::super::{code::CodeNodeDefinition, code_runtime::CodeNode};
use super::*;
use adk_rust::graph::{ExecutionConfig, Node, NodeContext, State};
use elitea_agent_runtime::graph::fanout::admission::CODE_OBSERVATION_MARGIN;
use serde_json::json;

const TIMEOUT_SECONDS: u32 = 30;

fn required() -> Duration {
    Duration::from_secs(u64::from(TIMEOUT_SECONDS)) + CODE_OBSERVATION_MARGIN
}

fn lazy_client(audience: &str) -> SandboxClient {
    SandboxClient::from_channel(
        tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
        audience.into(),
        Duration::from_secs(30),
    )
    .unwrap()
}

fn never_connecting_pool() -> sqlx::PgPool {
    crate::test_pg::pool_options()
        .max_connections(1)
        .acquire_timeout(Duration::from_millis(50))
        .connect_lazy("postgres://fixture:fixture@127.0.0.1:1/never_connect")
        .unwrap()
}

fn control() -> Arc<AgentControlClient<TonicControlRpc>> {
    Arc::new(
        AgentControlClient::from_channel(
            tonic::transport::Endpoint::from_static("https://127.0.0.1:1").connect_lazy(),
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap(),
    )
}

fn raw_runtime(execution_deadline: tokio::time::Instant, slots: &CodeSlots) -> RemoteCodeRuntime {
    RemoteCodeRuntime {
        saved_child_scope: None,
        intent_content: None,
        control: control(),
        authority: Arc::new(ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture()),
        profiles: vec![CodeRuntimeProfile {
            platform_client: None,
            language: CodeLanguage::Python,
            image_digest: format!("sha256:{}", "a".repeat(64)),
            policy_revision: "isolated-v1".into(),
            timeout_seconds: TIMEOUT_SECONDS,
            client: lazy_client("sandbox-test"),
            preparation: None,
            preparation_client: None,
            compiled_profile: None,
            slots: slots.clone(),
            slot_target: "sandbox.internal:9446".into(),
        }]
        .into(),
        journal: Arc::new(crate::sandbox::dispatch::DispatchJournal::new(
            never_connecting_pool(),
        )),
        compiled_profile: None,
        debug_sink: None,
        execution_deadline,
    }
}

fn invocation() -> CodeInvocation<'static> {
    CodeInvocation {
        activation: [9; 32],
        language: CodeLanguage::Python,
        platform_client: false,
        source: "secret_source_marker = 41",
        dependencies_toml: None,
        provenance: CodeProvenance::SavedLiteral,
        input_json: br#"{"count":"secret_input_marker"}"#.to_vec(),
        trace: None,
        original: None,
        debug: None,
        workspace: None,
    }
}

/// D4 on the raw path: exactly `timeout + margin` left is admitted (the
/// attempt then fails at the dispatch journal, past admission); one
/// millisecond less is refused typed, before preparation, the journal or any
/// submission, and holds no slot.
#[tokio::test(start_paused = true)]
async fn raw_path_admits_exactly_required_and_refuses_one_ms_less() {
    let slots = CodeSlots::new(1);
    let now = tokio::time::Instant::now();
    let admitted = raw_runtime(now + required(), &slots)
        .execute_tool(invocation())
        .await
        .unwrap_err();
    assert_ne!(
        admitted,
        super::super::code_runtime::ToolCodeError::DeadlineInsufficient,
        "exactly the required window must be admitted"
    );
    assert_eq!(
        slots.available(),
        1,
        "the finished attempt released its slot"
    );
    let refused = raw_runtime(now + required() - Duration::from_millis(1), &slots)
        .execute_tool(invocation())
        .await
        .unwrap_err();
    assert_eq!(
        refused,
        super::super::code_runtime::ToolCodeError::DeadlineInsufficient
    );
    assert_eq!(slots.available(), 1, "a refusal holds nothing");
    let graph = raw_runtime(now + required() - Duration::from_millis(1), &slots)
        .execute(invocation())
        .await
        .unwrap_err();
    assert!(
        graph
            .to_string()
            .contains(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_MESSAGE),
        "{graph}"
    );
}

/// The graph Code node on the raw path reports the registered upstream code,
/// which the run maps to the readable `CodeDeadlineInsufficient` terminal.
#[tokio::test(start_paused = true)]
async fn the_raw_graph_node_reports_the_registered_deadline_code() {
    let slots = CodeSlots::new(1);
    let runtime = Arc::new(raw_runtime(
        tokio::time::Instant::now() + Duration::from_secs(10),
        &slots,
    ));
    let (sender, receiver) = super::super::node_events::pipeline_node_event_channel();
    let node = CodeNode::new(
        CodeNodeDefinition::from_yaml(
            "id: run\ntype: code\ncode: '7'\ninput: [count]\noutput: [count]\ntransition: END\n",
        )
        .unwrap(),
        BTreeMap::from([("count".into(), "int".into())]),
        runtime,
    )
    .unwrap()
    .with_events(Some(sender));
    let context = NodeContext::new(
        State::from([("count".into(), json!(2))]),
        ExecutionConfig::new("root"),
        4,
    );
    assert!(node.execute(&context).await.is_err());
    let code = receiver.try_failure_code().await;
    assert_eq!(
        code,
        Some(crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE)
    );
    assert_eq!(
        crate::protocol::output::model_failure(code),
        crate::protocol::output::RuntimeFailureKind::CodeDeadlineInsufficient
    );
}

fn config(
    language: &str,
    target: &str,
    code_slots: Option<u32>,
) -> crate::config::SandboxRuntimeConfig {
    let mut value = json!({
        "language": language, "target": target, "audience": format!("sandbox-{language}"),
        "image_digest": format!("sha256:{}", "a".repeat(64)),
        "policy_revision": "isolated-v1", "timeout_seconds": TIMEOUT_SECONDS
    });
    if let Some(slots) = code_slots {
        value["code_slots"] = json!(slots);
    }
    serde_json::from_value(value).unwrap()
}

/// C4: profiles on one normalised Supervisor target share one set of slots
/// (javascript and typescript on one Supervisor, spelt two ways); another
/// target is independent. The factory logs the effective value per target.
#[tokio::test]
async fn slots_bound_concurrent_attempts_per_target_and_not_across_targets() {
    let profiles = [
        config("javascript", "Sandbox.Internal.:9450", Some(2)),
        config("typescript", "sandbox.internal:9450", Some(2)),
        config("rust", "sandbox.internal:9447", None),
    ]
    .into_iter()
    .map(|config| {
        let client = lazy_client(&config.audience);
        (config, client, None)
    })
    .collect();
    let factory = CodeRuntimeFactory::new(control(), profiles, never_connecting_pool());
    assert_eq!(
        factory
            .code_slots
            .iter()
            .map(|(target, slots)| (target.as_str(), slots.capacity()))
            .collect::<Vec<_>>(),
        [("sandbox.internal:9447", 4), ("sandbox.internal:9450", 2)]
    );
    let (javascript, typescript, rust) = (
        &factory.profiles[0].slots,
        &factory.profiles[1].slots,
        &factory.profiles[2].slots,
    );
    let far = tokio::time::Instant::now() + Duration::from_secs(60);
    let first = javascript.acquire_until(far).await.unwrap();
    let second = typescript.acquire_until(far).await.unwrap();
    assert_eq!(javascript.available(), 0);
    assert_eq!(
        typescript.available(),
        0,
        "one Supervisor, one set of slots"
    );
    assert!(
        tokio::time::timeout(Duration::from_millis(20), typescript.acquire_until(far))
            .await
            .is_err(),
        "the third attempt on a two-slot Supervisor ran"
    );
    let other = tokio::time::timeout(Duration::from_secs(5), rust.acquire_until(far))
        .await
        .expect("another Supervisor is not blocked")
        .unwrap();
    drop((first, second, other));
    assert_eq!(javascript.available(), 2);
    assert_eq!(rust.available(), 4);
}

const FAULT_LINE_CHILD_ENV: &str = "ELITEA_CODE_DEADLINE_FAULT_LINE_CHILD";

/// The refusal is one WARN fault line with the registered code, the ids and
/// the numbers; when the slot wait used up the margin it says so (N1). The
/// Code source and inputs never appear.
#[test]
fn a_refusal_is_one_fault_line_with_its_cause_and_no_user_data() {
    let output = elitea_faults::logtest::run_child(
        "agents::graph::code_remote::slots_tests::fault_line_child",
        FAULT_LINE_CHILD_ENV,
    );
    let lines = output.operator_lines();
    let code = elitea_faults::codes::PIPELINE_CODE_DEADLINE_INSUFFICIENT;
    let refusals: Vec<_> = lines
        .iter()
        .filter(|line| line["code"] == code.as_str())
        .collect();
    assert_eq!(refusals.len(), 2, "{}", output.stdout);
    for line in &refusals {
        assert_eq!(line["level"], "WARN");
        assert_eq!(line["msg"], code.sentence());
        assert_eq!(line["action"], code.action());
        assert_eq!(line["event"], "code_deadline_insufficient");
        assert_eq!(line["required_ms"], 120_000);
        assert_eq!(line["slots"], 1);
        assert!(
            line["support_ref"]
                .as_str()
                .is_some_and(|value| !value.is_empty())
        );
        assert!(line.contains_key("node_id"));
    }
    let clock = refusals[0];
    assert_eq!(clock["remaining_ms"], 119_999);
    assert!(!clock.contains_key("cause") && !clock.contains_key("slot_wait_ms"));
    let waited = refusals[1];
    assert_eq!(waited["cause"], "slot_wait");
    assert_eq!(waited["slot_wait_ms"], 2_000);
    assert_eq!(waited["remaining_ms"], 120_000);
    assert!(
        !output.stdout.contains("secret_source_marker")
            && !output.stdout.contains("secret_input_marker")
            && !output.stderr.contains("secret_source_marker")
            && !output.stderr.contains("secret_input_marker"),
        "Code source or input reached the log"
    );
}

#[test]
fn fault_line_child() {
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;
    if !elitea_faults::logtest::is_child(FAULT_LINE_CHILD_ENV) {
        return;
    }
    tracing_subscriber::registry()
        .with(
            crate::diagnostics::log_layer(Some("debug"), std::io::stdout).expect("operator stream"),
        )
        .init();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .start_paused(true)
        .build()
        .unwrap();
    runtime.block_on(async {
        let slots = CodeSlots::new(1);
        let now = tokio::time::Instant::now();
        // The clock alone: one millisecond short on arrival.
        let _ = raw_runtime(now + required() - Duration::from_millis(1), &slots)
            .execute_tool(invocation())
            .await;
        // The window was there on arrival; a full Worker used it up.
        let held = slots.acquire_until(now + required()).await.unwrap();
        let _ = raw_runtime(now + required() + Duration::from_secs(2), &slots)
            .execute_tool(invocation())
            .await;
        drop(held);
    });
    elitea_faults::logtest::child_ran();
}

const SLOT_NOTICE_CHILD_ENV: &str = "ELITEA_CODE_SLOT_NOTICE_CHILD";

/// Plan §2.1 observability: an uncontended acquire writes no line; a wait
/// over one second writes exactly one INFO line with the code, the wait, the
/// capacity, the normalised target and the execution id; Supervisor Busy is
/// one INFO line per attempt, however many retries. No user data.
#[test]
fn slot_waits_and_supervisor_busy_are_one_info_line_each() {
    let output = elitea_faults::logtest::run_child(
        "agents::graph::code_remote::slots_tests::slot_notice_child",
        SLOT_NOTICE_CHILD_ENV,
    );
    let lines = output.operator_lines();
    let waited = elitea_faults::codes::PIPELINE_CODE_SLOT_WAITED;
    let waits: Vec<_> = lines
        .iter()
        .filter(|line| line["code"] == waited.as_str())
        .collect();
    assert_eq!(waits.len(), 1, "{}", output.stdout);
    let wait = waits[0];
    assert_eq!(wait["level"], "INFO");
    assert_eq!(wait["msg"], waited.sentence());
    assert_eq!(wait["event"], "code_slot_waited");
    assert_eq!(wait["slot_wait_ms"], 2_000);
    assert_eq!(wait["slots_capacity"], 1);
    assert_eq!(wait["supervisor_target"], "sandbox.internal:9446");
    assert!(
        wait["execution_id"]
            .as_str()
            .is_some_and(|id| !id.is_empty())
    );
    let busy = elitea_faults::codes::PIPELINE_CODE_SUPERVISOR_BUSY;
    let busy_lines: Vec<_> = lines
        .iter()
        .filter(|line| line["code"] == busy.as_str())
        .collect();
    assert_eq!(busy_lines.len(), 2, "one per attempt: {}", output.stdout);
    for line in busy_lines {
        assert_eq!(line["level"], "INFO");
        assert_eq!(line["event"], "code_supervisor_busy");
        assert_eq!(line["slots_capacity"], 1);
        assert_eq!(line["supervisor_target"], "sandbox.internal:9446");
        assert!(
            line["execution_id"]
                .as_str()
                .is_some_and(|id| !id.is_empty())
        );
    }
    for marker in ["secret_source_marker", "secret_input_marker"] {
        assert!(
            !output.stdout.contains(marker) && !output.stderr.contains(marker),
            "Code source or input reached the log"
        );
    }
}

#[test]
fn slot_notice_child() {
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;
    if !elitea_faults::logtest::is_child(SLOT_NOTICE_CHILD_ENV) {
        return;
    }
    tracing_subscriber::registry()
        .with(
            crate::diagnostics::log_layer(Some("info"), std::io::stdout).expect("operator stream"),
        )
        .init();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .start_paused(true)
        .build()
        .unwrap();
    runtime.block_on(async {
        let slots = CodeSlots::new(1);
        let far = tokio::time::Instant::now() + Duration::from_secs(3600);
        // Uncontended: admitted at once, no line.
        let _ = raw_runtime(far, &slots).execute_tool(invocation()).await;
        // Contended: the slot frees after 2 s; one INFO line.
        let held = slots.acquire_until(far).await.unwrap();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_secs(2)).await;
            drop(held);
        });
        let _ = raw_runtime(far, &slots).execute_tool(invocation()).await;
        // Busy on two attempts, three retries each: one line per attempt.
        let runtime = raw_runtime(far, &slots);
        for _ in 0..2 {
            let mut busy = SupervisorBusyNotice::default();
            for code in [
                tonic::Code::ResourceExhausted,
                tonic::Code::Unavailable,
                tonic::Code::ResourceExhausted,
                tonic::Code::ResourceExhausted,
            ] {
                busy.note(code, &runtime, &runtime.profiles[0]);
            }
        }
    });
    elitea_faults::logtest::child_ran();
}
