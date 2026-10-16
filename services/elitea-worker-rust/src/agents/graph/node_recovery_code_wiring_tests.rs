//! The production `RemoteCodeRuntime::execute_recovery_attempt`, driven end to end
//! through the real Code node and recovery journal with trusted doubles for the
//! three peers it talks to: Main's claim-bound content routes (an HTTP fixture),
//! Main's control service and the sandbox supervisor (real tonic servers on
//! loopback), plus a disposable `PostgreSQL` for the dispatch journal. No mTLS, no
//! container runtime. These tests exist so that reverting the dispatch proof, the
//! submission marker, or the terminal-failure journal proof fails a test.
use super::*;
use crate::agents::graph::code_remote::CodeRuntimeFactory;
use crate::config::SandboxRuntimeConfig;
use crate::protocol::control::{AgentControlClient, ClaimBoundSandboxAuthority};
use crate::protocol::elitea::runtime::v1 as pb;
use crate::protocol::elitea::runtime::v1::runtime_control_service_server::{
    RuntimeControlService, RuntimeControlServiceServer,
};
use crate::protocol::elitea::runtime::v1::sandbox_supervisor_service_server::{
    SandboxSupervisorService, SandboxSupervisorServiceServer,
};
use crate::sandbox::{
    client::SandboxClient,
    dispatch::{DispatchJournal, DispatchScope},
    platform_client_binding::PlatformClientPolicy,
};
use crate::state::postgres_session_tests::IsolatedPostgres;
use crate::transport::input_content::{InputContentRpc, InputContentTransportError};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use http_body_util::BodyExt as _;
use std::sync::Mutex as StdMutex;
use std::time::Duration;
use tonic::{Request, Response, Status};

const PLATFORM_YAML: &str = "entry_point: code\nstate:\n  count: int\n  value: int\n  private: str\nnodes:\n  - id: code\n    type: code\n    language: python\n    code: 'return 41'\n    input: [value]\n    output: [count]\n    platform_client: true\n    transition: END\n";
const PURE_YAML: &str = "entry_point: code\nstate:\n  count: int\n  value: int\n  private: str\nnodes:\n  - id: code\n    type: code\n    language: python\n    code: 'return 41'\n    input: [value]\n    output: [count]\n    transition: END\n";

// ---------------------------------------------------------------- Main content

#[derive(Clone, Copy)]
enum Effects {
    None,
    Possible,
    Refused,
}
#[derive(Clone, Copy)]
struct MainScript {
    /// Main refuses the original visit: a deterministic failure before dispatch.
    refuse_visit: bool,
    /// The first platform step takes this long (a step in flight).
    first_step_delay: Option<Duration>,
    effects: Effects,
}
#[derive(Default)]
struct MainCalls {
    steps: usize,
    effects: usize,
    effects_body: Option<serde_json::Value>,
    /// `submission_pending` of the dispatch row when the intent route is called.
    pending_at_intent: Vec<Option<bool>>,
}
struct FakeMain {
    script: MainScript,
    effects: Arc<StdMutex<Effects>>,
    calls: Arc<StdMutex<MainCalls>>,
    pool: sqlx::PgPool,
}
fn json_response(
    status: http::StatusCode,
    value: &serde_json::Value,
) -> http::Response<tonic::body::Body> {
    let raw = serde_json::to_vec(value).unwrap();
    http::Response::builder()
        .status(status)
        .version(http::Version::HTTP_2)
        .header(http::header::CONTENT_TYPE, "application/json")
        .header(http::header::CONTENT_LENGTH, raw.len())
        .body(tonic::body::Body::new(http_body_util::Full::new(
            bytes::Bytes::from(raw),
        )))
        .unwrap()
}
#[async_trait]
impl InputContentRpc for FakeMain {
    async fn get(
        &self,
        request: http::Request<tonic::body::Body>,
    ) -> Result<http::Response<tonic::body::Body>, InputContentTransportError> {
        let path = request.uri().path().to_owned();
        let segments: Vec<_> = path.split('/').collect();
        let execution = segments[2].to_owned();
        let generation: u64 = segments[4].parse().unwrap();
        let body = request.into_body().collect().await.unwrap().to_bytes();
        let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
        let ok = http::StatusCode::OK;
        if path.ends_with("/code-sandbox/visits") {
            if self.script.refuse_visit {
                return Ok(json_response(
                    http::StatusCode::FORBIDDEN,
                    &serde_json::json!({}),
                ));
            }
            let pre = URL_SAFE_NO_PAD
                .decode(
                    body["pre_workspace_prepared_job_json_base64url"]
                        .as_str()
                        .unwrap(),
                )
                .unwrap();
            return Ok(json_response(
                ok,
                &serde_json::json!({
                    "schema":"elitea.sandbox.original-code-visit-response.v1",
                    "original_visit":{"visit_id":"1".repeat(64),"revision":1,"digest_sha256":"2".repeat(64)},
                    "execution_id":execution,"original_generation":generation,
                    "activation_id":body["activation_id"],"attempt":body["attempt"],
                    "node_digest":body["node_digest"],
                    "pre_workspace_prepared_sha256":crate::sandbox::code_recovery::sha256(&pre),
                }),
            ));
        }
        if path.ends_with("/code-sandbox/intents") {
            let pending: Option<bool> = sqlx::query_scalar(
                "SELECT submission_pending FROM elitea_runtime.sandbox_dispatches LIMIT 1",
            )
            .fetch_optional(&self.pool)
            .await
            .unwrap();
            self.calls.lock().unwrap().pending_at_intent.push(pending);
            return Ok(json_response(
                ok,
                &serde_json::json!({
                    "schema":"elitea.sandbox.original-code-intent-response.v1",
                    "intent":{
                        "schema":"elitea.sandbox.original-code-intent-signed.v1",
                        "key_id":"intent-key",
                        "claims_base64url":URL_SAFE_NO_PAD.encode(b"{}"),
                        "signature_base64url":URL_SAFE_NO_PAD.encode([0_u8; 64]),
                    },
                }),
            ));
        }
        if path.ends_with("/code-platform/step") {
            let first = {
                let mut calls = self.calls.lock().unwrap();
                calls.steps += 1;
                calls.steps == 1
            };
            if first && let Some(delay) = self.script.first_step_delay {
                tokio::time::sleep(delay).await;
            }
            return Ok(json_response(
                ok,
                &serde_json::json!({"schema":"elitea.runtime.code-platform-step-response.v1","revision":1,"disposition":"idle","effect_id":null}),
            ));
        }
        if path.ends_with("/code-platform/effects") {
            {
                let mut calls = self.calls.lock().unwrap();
                calls.effects += 1;
                calls.effects_body = Some(body);
            }
            let answer = *self.effects.lock().unwrap();
            return Ok(match answer {
                Effects::Refused => {
                    json_response(http::StatusCode::FORBIDDEN, &serde_json::json!({}))
                }
                Effects::None => json_response(
                    ok,
                    &serde_json::json!({"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"none"}),
                ),
                Effects::Possible => json_response(
                    ok,
                    &serde_json::json!({"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"possible"}),
                ),
            });
        }
        panic!("the fixture serves no route {path}");
    }
}

// ------------------------------------------------- Main control + supervisor

struct FakeControl;
#[tonic::async_trait]
impl RuntimeControlService for FakeControl {
    async fn authorize_sandbox_job(
        &self,
        _: Request<pb::AuthorizeSandboxJobRequestV1>,
    ) -> Result<Response<pb::AuthorizeSandboxJobResponseV1>, Status> {
        Ok(Response::new(pb::AuthorizeSandboxJobResponseV1 {
            grant: Some(pb::SignedSandboxJobGrantV1 {
                key_id: "grant-key".into(),
                claims_bytes: b"claims".to_vec(),
                signature: vec![0; 64],
            }),
            rejection: None,
        }))
    }
    async fn authorize_rust_compiled_snapshot(
        &self,
        _: Request<pb::AuthorizeRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<pb::AuthorizeRustCompiledSnapshotResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn claim_command(
        &self,
        _: Request<pb::ClaimCommandRequestV1>,
    ) -> Result<Response<pb::ClaimCommandResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn begin_execution(
        &self,
        _: Request<pb::BeginExecutionRequestV1>,
    ) -> Result<Response<pb::BeginExecutionResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn authorize_invocation(
        &self,
        _: Request<pb::AuthorizeInvocationRequestV1>,
    ) -> Result<Response<pb::AuthorizeInvocationResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn authorize_agent_model_checkpoint(
        &self,
        _: Request<pb::AuthorizeAgentModelCheckpointRequestV1>,
    ) -> Result<Response<pb::AuthorizeAgentModelCheckpointResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn renew_lease(
        &self,
        _: Request<pb::RenewLeaseRequestV1>,
    ) -> Result<Response<pb::RenewLeaseResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn observe_desired_state(
        &self,
        _: Request<pb::ObserveDesiredStateRequestV1>,
    ) -> Result<Response<pb::ObserveDesiredStateResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn prepare_settlement(
        &self,
        _: Request<pb::PrepareSettlementRequestV1>,
    ) -> Result<Response<pb::PrepareSettlementResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn remove_index(
        &self,
        _: Request<pb::RemoveIndexRequestV1>,
    ) -> Result<Response<pb::RemoveIndexResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
}

#[derive(Default)]
struct SupervisorCalls {
    submits: usize,
    cancels: usize,
    /// `submission_pending` of the dispatch row when the submission arrived.
    pending_at_submit: Vec<Option<bool>>,
}
struct FakeSupervisor {
    /// The terminal Failed receipt arrives after this delay.
    fail_after: Duration,
    calls: Arc<StdMutex<SupervisorCalls>>,
    pool: sqlx::PgPool,
}
#[tonic::async_trait]
impl SandboxSupervisorService for FakeSupervisor {
    async fn submit_sandbox_job(
        &self,
        _: Request<pb::SubmitSandboxJobRequestV1>,
    ) -> Result<Response<pb::SubmitSandboxJobResponseV1>, Status> {
        let pending: Option<bool> = sqlx::query_scalar(
            "SELECT submission_pending FROM elitea_runtime.sandbox_dispatches LIMIT 1",
        )
        .fetch_optional(&self.pool)
        .await
        .unwrap();
        {
            let mut calls = self.calls.lock().unwrap();
            calls.submits += 1;
            calls.pending_at_submit.push(pending);
        }
        tokio::time::sleep(self.fail_after).await;
        Ok(Response::new(pb::SubmitSandboxJobResponseV1 {
            status: pb::SandboxJobStatusV1::Failed.into(),
            result_json: Vec::new(),
            failure_code: "runtime_failed".into(),
            cleanup_pending: false,
        }))
    }
    async fn cancel_sandbox_job(
        &self,
        _: Request<pb::CancelSandboxJobRequestV1>,
    ) -> Result<Response<pb::CancelSandboxJobResponseV1>, Status> {
        self.calls.lock().unwrap().cancels += 1;
        Ok(Response::new(pb::CancelSandboxJobResponseV1 {
            status: pb::SandboxJobStatusV1::Cancelled.into(),
            cleanup_pending: false,
        }))
    }
    async fn lookup_sandbox_dependencies(
        &self,
        _: Request<pb::LookupSandboxDependenciesRequestV1>,
    ) -> Result<Response<pb::LookupSandboxDependenciesResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn submit_rust_compiled_snapshot(
        &self,
        _: Request<pb::SubmitRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<pb::SubmitRustCompiledSnapshotResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn publish_rust_compiled_snapshot(
        &self,
        _: Request<pb::PublishRustCompiledSnapshotRequestV1>,
    ) -> Result<Response<pb::PublishRustCompiledSnapshotResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn hydrate_sandbox_dependencies(
        &self,
        _: Request<pb::HydrateSandboxDependenciesRequestV1>,
    ) -> Result<Response<pb::HydrateSandboxDependenciesResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn prepare_sandbox_dependencies(
        &self,
        _: Request<pb::PrepareSandboxDependenciesRequestV1>,
    ) -> Result<Response<pb::PrepareSandboxDependenciesResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn publish_sandbox_dependencies(
        &self,
        _: Request<pb::PublishSandboxDependenciesRequestV1>,
    ) -> Result<Response<pb::PublishSandboxDependenciesResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn hydrate_sandbox_workspace(
        &self,
        _: Request<pb::HydrateSandboxWorkspaceRequestV1>,
    ) -> Result<Response<pb::HydrateSandboxWorkspaceResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
}

// ------------------------------------------------------------------- harness

fn runtime_config(broker: bool) -> SandboxRuntimeConfig {
    serde_json::from_value(serde_json::json!({
        "language":"python","target":"sandbox.internal:9446",
        "audience":if broker {"broker-owner"} else {"pure-owner"},
        "image_digest":format!("sha256:{}", if broker {"b".repeat(64)} else {"a".repeat(64)}),
        "policy_revision":"isolated-v1",
        "timeout_seconds":30
    }))
    .unwrap()
}

struct Harness {
    _database: IsolatedPostgres,
    factory: CodeRuntimeFactory,
    runtime: Arc<dyn CodeSandboxRuntime>,
    authority: Arc<ClaimBoundSandboxAuthority>,
    scope: DispatchScope,
    dispatch: DispatchJournal,
    effects: Arc<StdMutex<Effects>>,
    main: Arc<StdMutex<MainCalls>>,
    supervisor: Arc<StdMutex<SupervisorCalls>>,
}

#[allow(clippy::too_many_lines)] // One harness wires every fake the code-recovery proofs share.
async fn harness(script: MainScript, fail_after: Duration) -> Harness {
    let url = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL")
        .expect("ELITEA_TEST_DATABASE_URL is required");
    let database = IsolatedPostgres::create(&url).await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&database.pool)
        .await
        .unwrap();
    for migration in [
        include_str!(
            "../../../../elitea-main/migrations/agentstate/0007_sandbox_dispatch_journal.sql"
        ),
        include_str!(
            "../../../../elitea-main/migrations/agentstate/0014_sandbox_dispatch_submission_marker.sql"
        ),
    ] {
        sqlx::raw_sql(migration)
            .execute(&database.pool)
            .await
            .unwrap();
    }
    let authority = Arc::new(ClaimBoundSandboxAuthority::original_code_visit_conformance_fixture());
    let scope = authority.dispatch_scope().unwrap();
    let main = Arc::new(StdMutex::new(MainCalls::default()));
    let supervisor = Arc::new(StdMutex::new(SupervisorCalls::default()));
    let effects = Arc::new(StdMutex::new(script.effects));
    let content = Arc::new(
        crate::transport::InputContentClient::with_original_code_visit_fixture_rpc_deadline(
            FakeMain {
                script,
                effects: effects.clone(),
                calls: main.clone(),
                pool: database.pool.clone(),
            },
            Duration::from_secs(30),
        ),
    );
    let control_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let control_address = control_listener.local_addr().unwrap();
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(RuntimeControlServiceServer::new(FakeControl))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(
                control_listener,
            )),
    );
    let supervisor_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let supervisor_address = supervisor_listener.local_addr().unwrap();
    tokio::spawn(
        tonic::transport::Server::builder()
            .add_service(SandboxSupervisorServiceServer::new(FakeSupervisor {
                fail_after,
                calls: supervisor.clone(),
                pool: database.pool.clone(),
            }))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(
                supervisor_listener,
            )),
    );
    let control = Arc::new(
        AgentControlClient::from_channel(
            tonic::transport::Endpoint::from_shared(format!("http://{control_address}"))
                .unwrap()
                .connect_lazy(),
            crate::transport::control_grpc::ControlGrpcConfig {
                deadline: Duration::from_secs(5),
                workload_session_id: "workload-1".into(),
                producer_id: "worker-1".into(),
            },
        )
        .unwrap(),
    );
    let channel = tonic::transport::Endpoint::from_shared(format!("http://{supervisor_address}"))
        .unwrap()
        .connect_lazy();
    let client = |config: &SandboxRuntimeConfig| {
        SandboxClient::from_channel(
            channel.clone(),
            config.audience.clone(),
            Duration::from_secs(5),
        )
        .unwrap()
    };
    let (pure, broker) = (runtime_config(false), runtime_config(true));
    let factory = CodeRuntimeFactory::new(
        control,
        vec![(pure.clone(), client(&pure), None)],
        database.pool.clone(),
    )
    .with_platform_client(
        broker.clone(),
        client(&broker),
        None,
        PlatformClientPolicy::new(32, 1_048_576).unwrap(),
        None,
    )
    .unwrap()
    .with_code_intents(content);
    Harness {
        runtime: factory.bind(authority.clone()),
        factory,
        authority,
        dispatch: DispatchJournal::new(database.pool.clone()),
        effects,
        _database: database,
        scope,
        main,
        supervisor,
    }
}

struct Run {
    node: RecoverableNode,
    context: NodeContext,
    factory: Arc<Factory>,
    receiver: crate::agents::graph::node_events::PipelineNodeEventReceiver,
    node_digest: [u8; 32],
    dispatch_activation: [u8; 32],
}
async fn run(h: &Harness, yaml: &str, recovering: bool) -> Run {
    let pipeline = crate::agents::graph::compiler::PipelineDefinition::from_yaml(yaml).unwrap();
    let definition = pipeline.original_code_definition_fixture("code").unwrap();
    let digest = definition.validated_digest();
    let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
    let body = Arc::new(
        CodeNode::new(definition, types(), h.runtime.clone())
            .unwrap()
            .with_events(Some(sender)),
    );
    let factory = factory();
    let context = ctx();
    let activation = NodeAttemptActivation::from_context("code", digest, &context).unwrap();
    let journal = factory.open(&activation, &policy()).await.unwrap();
    if recovering {
        // The crash window: Started is durable, no outcome was ever recorded.
        let snapshot = journal.load().await.unwrap();
        let started = snapshot.ledger.start_attempt(100).unwrap();
        journal.append(&snapshot, started, None).await.unwrap();
    }
    let dispatch_activation =
        NodeAttemptAuthority::committed(&activation, journal.activation_id, 1, true)
            .dispatch_activation();
    let mut node = RecoverableNode::new(body, digest, policy(), factory.clone(), None);
    node.clock = Arc::new(Clock(AtomicU64::new(200)));
    Run {
        node,
        context,
        factory,
        receiver,
        node_digest: digest,
        dispatch_activation,
    }
}
impl Run {
    /// The codes of the typed failure events the node published, in order.
    async fn events(&self) -> Vec<String> {
        let mut drain = self.receiver.drain("inv", "agent", "root").await.unwrap();
        let mut codes = Vec::new();
        while let Some(item) = drain.try_recv() {
            // Trace events are not settlements; only a failure event is.
            if let Err(error) = item {
                codes.push(error.code.to_string());
            }
        }
        codes
    }
    async fn failure(&self) -> NodeFailure {
        let activation =
            NodeAttemptActivation::from_context("code", self.node_digest, &self.context).unwrap();
        let journal = self.factory.open(&activation, &policy()).await.unwrap();
        journal
            .load()
            .await
            .unwrap()
            .ledger
            .effective_failure()
            .expect("the attempt failed")
    }
}
const QUIET: MainScript = MainScript {
    refuse_visit: false,
    first_step_delay: None,
    effects: Effects::None,
};
const REFUSED_VISIT: MainScript = MainScript {
    refuse_visit: true,
    ..QUIET
};

// ------------------------------------------------- A: the dispatch-row proof

/// The dispatch decision comes from the dispatch journal, not from the error:
/// a recovered attempt whose earlier claim never sent a submission fails typed.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn recovered_attempt_without_a_dispatch_row_fails_typed_before_dispatch() {
    let h = harness(REFUSED_VISIT, Duration::ZERO).await;
    let run = run(&h, PLATFORM_YAML, true).await;
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(run.failure().await.replay, ReplaySafety::NoExternalEffect);
    // One readable typed settlement and no operator card.
    let events = run.events().await;
    assert_eq!(events.len(), 1, "{events:?}");
    assert!(events[0].starts_with("pipeline.code_"), "{events:?}");
    assert_eq!(h.supervisor.lock().unwrap().submits, 0);
}

/// A claim lost during hydration leaves a registered row with no submission
/// behind it (review L5): the row alone does not make the effect possible.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn recovered_attempt_with_a_registered_but_unsubmitted_row_fails_typed() {
    let h = harness(REFUSED_VISIT, Duration::ZERO).await;
    let run = run(&h, PLATFORM_YAML, true).await;
    h.dispatch
        .register_unsubmitted(
            &h.scope,
            &run.dispatch_activation,
            &[2; 32],
            "dns:sandbox-a",
        )
        .await
        .unwrap();
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(run.failure().await.replay, ReplaySafety::NoExternalEffect);
    assert_eq!(run.events().await.len(), 1);
}

/// Once the submission marker is durable, the same failure is a possible
/// dispatch: it keeps the unknown-effect classification and is never retried.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn recovered_attempt_with_a_submitted_row_keeps_the_effect_unknown() {
    let h = harness(REFUSED_VISIT, Duration::ZERO).await;
    let run = run(&h, PLATFORM_YAML, true).await;
    h.dispatch
        .register_unsubmitted(
            &h.scope,
            &run.dispatch_activation,
            &[2; 32],
            "dns:sandbox-a",
        )
        .await
        .unwrap();
    h.dispatch
        .mark_submission_started(
            &h.scope,
            &run.dispatch_activation,
            &[2; 32],
            "dns:sandbox-a",
        )
        .await
        .unwrap();
    assert!(run.node.execute(&run.context).await.is_err());
    assert_eq!(
        run.failure().await.replay,
        ReplaySafety::UnknownExternalEffect {
            effect_id: run.dispatch_activation
        }
    );
    assert_eq!(h.supervisor.lock().unwrap().submits, 0);
}

/// The marker's three states on the journal itself: a row from an earlier
/// Worker (plain `register`) reads as submitted; an opted-in row is pending
/// until the marker; the marker is idempotent, one way and identity-checked.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn the_submission_marker_reads_conservatively_and_moves_one_way() {
    let h = harness(QUIET, Duration::ZERO).await;
    let (earlier, hydrating, unknown) = ([1_u8; 32], [2_u8; 32], [3_u8; 32]);
    let digest = [9_u8; 32];
    h.dispatch
        .register(&h.scope, &earlier, &digest, "dns:a")
        .await
        .unwrap();
    assert!(
        h.dispatch
            .submission_started(&h.scope, &earlier)
            .await
            .unwrap()
    );
    h.dispatch
        .register_unsubmitted(&h.scope, &hydrating, &digest, "dns:a")
        .await
        .unwrap();
    assert!(
        !h.dispatch
            .submission_started(&h.scope, &hydrating)
            .await
            .unwrap()
    );
    assert!(
        !h.dispatch
            .submission_started(&h.scope, &unknown)
            .await
            .unwrap(),
        "no row"
    );
    // A second registration never turns a pending row into a submitted one, and
    // never turns a submitted one back.
    h.dispatch
        .register_unsubmitted(&h.scope, &hydrating, &digest, "dns:a")
        .await
        .unwrap();
    assert!(
        !h.dispatch
            .submission_started(&h.scope, &hydrating)
            .await
            .unwrap()
    );
    h.dispatch
        .mark_submission_started(&h.scope, &hydrating, &digest, "dns:a")
        .await
        .unwrap();
    h.dispatch
        .mark_submission_started(&h.scope, &hydrating, &digest, "dns:a")
        .await
        .unwrap();
    h.dispatch
        .register_unsubmitted(&h.scope, &hydrating, &digest, "dns:a")
        .await
        .unwrap();
    assert!(
        h.dispatch
            .submission_started(&h.scope, &hydrating)
            .await
            .unwrap()
    );
    // The marker needs the exact registered identity.
    assert!(
        h.dispatch
            .mark_submission_started(&h.scope, &hydrating, &[8; 32], "dns:a")
            .await
            .is_err()
    );
    assert!(
        h.dispatch
            .mark_submission_started(&h.scope, &unknown, &digest, "dns:a")
            .await
            .is_err()
    );
    assert!(
        h.dispatch
            .register_unsubmitted(&h.scope, &hydrating, &[8; 32], "dns:a")
            .await
            .is_err()
    );
}

/// The marker is durable before the effect, and registration stays unsubmitted
/// through hydration: both are read from the dispatch row by the peers.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn the_submission_marker_is_durable_before_the_supervisor_sees_the_job() {
    let h = harness(QUIET, Duration::from_millis(50)).await;
    let run = run(&h, PURE_YAML, false).await;
    let _ = run.node.execute(&run.context).await;
    // The row existed, unsubmitted, when the exact Execute intent was frozen.
    assert_eq!(
        h.main.lock().unwrap().pending_at_intent.first(),
        Some(&Some(true))
    );
    // And it was already marked submitted when the submission reached the supervisor.
    {
        let supervisor = h.supervisor.lock().unwrap();
        assert_eq!(supervisor.submits, 1);
        assert_eq!(supervisor.pending_at_submit, [Some(false)]);
    }
    assert!(
        h.dispatch
            .submission_started(&h.scope, &run.dispatch_activation)
            .await
            .unwrap()
    );
}

// ------------------------------- H: a terminal Failed runtime and the journal

/// The goal: a thrown error or comment-only source, with no unresolved platform
/// call in Main's journal, settles typed with no operator card.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn failed_runtime_with_a_clean_journal_settles_typed_without_an_operator_card() {
    let h = harness(QUIET, Duration::from_millis(100)).await;
    let run = run(&h, PLATFORM_YAML, false).await;
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(run.failure().await.replay, ReplaySafety::NoExternalEffect);
    assert_eq!(run.events().await, ["pipeline.code_failed"]);
    let main = h.main.lock().unwrap();
    assert_eq!(main.effects, 1, "Main's journal was asked exactly once");
    let body = main.effects_body.as_ref().unwrap();
    assert_eq!(
        body["schema"],
        "elitea.runtime.code-platform-effects-request.v1"
    );
    assert_eq!(
        body["dispatch_activation"],
        crate::sandbox::code_recovery::hex(&run.dispatch_activation)
    );
}

/// Claim 1 reaches the supervisor (its dispatch row is marked) and its runtime
/// fails; claim 1 then dies. The runs below are claim 2 on a fresh recovery
/// journal, answered by the same Main journal.
async fn claim_one_submitted(h: &Harness) {
    let first = run(h, PLATFORM_YAML, false).await;
    assert!(first.node.execute(&first.context).await.is_err());
    assert!(
        h.dispatch
            .submission_started(&h.scope, &first.dispatch_activation)
            .await
            .unwrap()
    );
    *h.main.lock().unwrap() = MainCalls::default();
}

/// After a takeover the journal holds claim 1's platform call: claim 2's own
/// pump never saw it, but Main's journal does, so the effect stays possible.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn takeover_with_an_unresolved_journal_call_reconciles() {
    let h = harness(QUIET, Duration::from_millis(100)).await;
    claim_one_submitted(&h).await;
    *h.effects.lock().unwrap() = Effects::Possible;
    let run = run(&h, PLATFORM_YAML, true).await;
    let output = run.node.execute(&run.context).await.unwrap();
    assert!(output.interrupt.is_some());
    assert_eq!(
        run.failure().await.replay,
        ReplaySafety::UnknownExternalEffect {
            effect_id: run.dispatch_activation
        }
    );
    assert!(run.events().await.is_empty());
    assert_eq!(h.main.lock().unwrap().effects, 1);
}

/// The same takeover settles typed once the journal proves nothing unresolved,
/// even though the earlier claim had submitted (the dispatch row is not the proof).
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn takeover_with_a_clean_journal_settles_typed() {
    let h = harness(QUIET, Duration::from_millis(100)).await;
    claim_one_submitted(&h).await;
    let run = run(&h, PLATFORM_YAML, true).await;
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(run.failure().await.replay, ReplaySafety::NoExternalEffect);
    assert_eq!(run.events().await, ["pipeline.code_failed"]);
    assert_eq!(h.main.lock().unwrap().effects, 1);
}

/// A refusal of the journal query proves nothing.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn a_refused_journal_query_keeps_the_effect_possible() {
    let h = harness(
        MainScript {
            effects: Effects::Refused,
            ..QUIET
        },
        Duration::from_millis(100),
    )
    .await;
    let run = run(&h, PLATFORM_YAML, false).await;
    assert!(
        run.node
            .execute(&run.context)
            .await
            .unwrap()
            .interrupt
            .is_some()
    );
    assert_eq!(
        run.failure().await.replay,
        ReplaySafety::UnknownExternalEffect {
            effect_id: run.dispatch_activation
        }
    );
}

/// A pump dropped inside a Main step may have reached an effect: the journal is
/// not asked (it cannot settle a step still running on Main), the effect stays
/// possible. The step outlasts the settle grace.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn a_platform_step_dropped_mid_flight_keeps_the_effect_possible() {
    let h = harness(
        MainScript {
            first_step_delay: Some(Duration::from_mins(1)),
            effects: Effects::None,
            ..QUIET
        },
        Duration::from_millis(300),
    )
    .await;
    let run = run(&h, PLATFORM_YAML, false).await;
    let started = std::time::Instant::now();
    assert!(
        run.node
            .execute(&run.context)
            .await
            .unwrap()
            .interrupt
            .is_some()
    );
    // The receipt waited for the in-flight step, then gave up on it.
    assert!(started.elapsed() >= crate::agents::graph::code_timing::PLATFORM_STEP_SETTLE_GRACE);
    assert_eq!(h.main.lock().unwrap().effects, 0);
    assert_eq!(
        run.failure().await.replay,
        ReplaySafety::UnknownExternalEffect {
            effect_id: run.dispatch_activation
        }
    );
}

/// A step that finishes inside the grace is a step boundary: the journal then
/// proves the outcome and the node settles typed.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn a_platform_step_finishing_inside_the_grace_lets_the_journal_decide() {
    let h = harness(
        MainScript {
            first_step_delay: Some(Duration::from_millis(900)),
            effects: Effects::None,
            ..QUIET
        },
        Duration::from_millis(300),
    )
    .await;
    let run = run(&h, PLATFORM_YAML, false).await;
    assert!(run.node.execute(&run.context).await.is_err());
    assert_eq!(h.main.lock().unwrap().effects, 1);
    assert_eq!(run.failure().await.replay, ReplaySafety::NoExternalEffect);
}

/// A runtime without the platform client has no network and nothing to ask.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn failed_runtime_without_a_platform_client_never_asks_the_journal() {
    let h = harness(
        MainScript {
            effects: Effects::Possible,
            ..QUIET
        },
        Duration::from_millis(100),
    )
    .await;
    let run = run(&h, PURE_YAML, false).await;
    assert!(run.node.execute(&run.context).await.is_err());
    assert_eq!(h.main.lock().unwrap().effects, 0);
    assert_eq!(h.main.lock().unwrap().steps, 0);
    assert_eq!(run.failure().await.replay, ReplaySafety::NoExternalEffect);
}

// ------------------------------------------- F: release on the raw hydration

/// A hydration that fails on the raw path sends no submission, so the job it
/// hydrated is released with the claim-bound stop and its dispatch row resolved.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn raw_path_hydration_failure_releases_the_hydrated_job() {
    let h = harness(QUIET, Duration::ZERO).await;
    let digest = [9_u8; 32];
    let activation = [7_u8; 32];
    h.dispatch
        .register(&h.scope, &activation, &digest, "pure-owner")
        .await
        .unwrap();
    let result = h
        .factory
        .raw_hydration_failure_fixture(h.authority.clone(), activation, digest)
        .await;
    assert!(
        result.is_err(),
        "no preparation is configured: hydration fails"
    );
    assert_eq!(
        h.supervisor.lock().unwrap().cancels,
        1,
        "the job was released"
    );
    assert!(
        h.dispatch.pending(&h.scope).await.unwrap().is_empty(),
        "the dispatch row is resolved"
    );
    assert_eq!(h.supervisor.lock().unwrap().submits, 0);
}
