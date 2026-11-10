//! The production `RemoteCodeRuntime::execute_recovery_attempt`, driven end to end
//! through the real Code node and recovery journal with trusted doubles for the
//! three peers it talks to: Main's claim-bound content service (a gRPC fixture),
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
use crate::transport::input_content::content_fake::{ContentFake, claim_of};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
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
    /// The intent answer takes this long: time spent inside the slot after
    /// admission and before the first submission (W3-2, N3).
    intent_delay: Option<Duration>,
    effects: Effects,
}
#[derive(Default)]
struct MainCalls {
    steps: usize,
    effects: usize,
    effects_body: Option<serde_json::Value>,
    /// `submission_pending` of the dispatch row when the intent route is called.
    pending_at_intent: Vec<Option<bool>>,
    /// The node attempt of every original visit Main admitted, in order.
    visit_attempts: Vec<u64>,
    /// The dispatch activation of every intent Main signed, in order.
    intent_dispatches: Vec<String>,
}
struct FakeMain {
    script: MainScript,
    effects: Arc<StdMutex<Effects>>,
    calls: Arc<StdMutex<MainCalls>>,
    pool: sqlx::PgPool,
}
/// One served answer: the document, or Main's refusal of the claim.
fn json_response(refused: bool, value: &serde_json::Value) -> Result<Vec<u8>, Status> {
    if refused {
        return Err(Status::permission_denied("refused"));
    }
    Ok(serde_json::to_vec(value).unwrap())
}
/// The call's claim, scope and JSON document.
fn unpack<T>(
    request: Request<T>,
    parts: impl FnOnce(T) -> (Option<pb::RuntimeContentScopeV1>, Vec<u8>),
) -> (String, u64, serde_json::Value) {
    let (claim, fence) = claim_of(&request);
    assert_ne!(claim, "");
    assert_eq!(fence.len(), 32);
    let (scope, body) = parts(request.into_inner());
    let scope = scope.unwrap();
    (
        scope.execution_id,
        scope.generation,
        serde_json::from_slice(&body).unwrap(),
    )
}
#[tonic::async_trait]
impl ContentFake for FakeMain {
    async fn register_code_visit(
        &self,
        request: Request<pb::RegisterCodeVisitRequestV1>,
    ) -> Result<Response<pb::RegisterCodeVisitResponseV1>, Status> {
        let (execution, generation, body) =
            unpack(request, |request| (request.scope, request.visit_json));
        let visit_json = self.answer("visits", execution, generation, body).await?;
        Ok(Response::new(pb::RegisterCodeVisitResponseV1 {
            visit_json,
        }))
    }
    async fn register_code_intent(
        &self,
        request: Request<pb::RegisterCodeIntentRequestV1>,
    ) -> Result<Response<pb::RegisterCodeIntentResponseV1>, Status> {
        let (execution, generation, body) =
            unpack(request, |request| (request.scope, request.intent_json));
        let response_json = self.answer("intents", execution, generation, body).await?;
        Ok(Response::new(pb::RegisterCodeIntentResponseV1 {
            response_json,
        }))
    }
    async fn step_code_platform(
        &self,
        request: Request<pb::StepCodePlatformRequestV1>,
    ) -> Result<Response<pb::StepCodePlatformResponseV1>, Status> {
        let (execution, generation, body) =
            unpack(request, |request| (request.scope, request.request_json));
        let response_json = self.answer("step", execution, generation, body).await?;
        Ok(Response::new(pb::StepCodePlatformResponseV1 {
            response_json,
        }))
    }
    async fn read_code_platform_effects(
        &self,
        request: Request<pb::ReadCodePlatformEffectsRequestV1>,
    ) -> Result<Response<pb::ReadCodePlatformEffectsResponseV1>, Status> {
        let (execution, generation, body) =
            unpack(request, |request| (request.scope, request.request_json));
        let response_json = self.answer("effects", execution, generation, body).await?;
        Ok(Response::new(pb::ReadCodePlatformEffectsResponseV1 {
            response_json,
        }))
    }
}
impl FakeMain {
    async fn answer(
        &self,
        path: &str,
        execution: String,
        generation: u64,
        body: serde_json::Value,
    ) -> Result<Vec<u8>, Status> {
        if path == "visits" {
            if let Some(attempt) = body["attempt"].as_u64() {
                self.calls.lock().unwrap().visit_attempts.push(attempt);
            }
            if self.script.refuse_visit {
                return json_response(true, &serde_json::json!({}));
            }
            let pre = URL_SAFE_NO_PAD
                .decode(
                    body["pre_workspace_prepared_job_json_base64url"]
                        .as_str()
                        .unwrap(),
                )
                .unwrap();
            return json_response(
                false,
                &serde_json::json!({
                    "schema":"elitea.sandbox.original-code-visit-response.v1",
                    "original_visit":{"visit_id":"1".repeat(64),"revision":1,"digest_sha256":"2".repeat(64)},
                    "execution_id":execution,"original_generation":generation,
                    "activation_id":body["activation_id"],"attempt":body["attempt"],
                    "node_digest":body["node_digest"],
                    "pre_workspace_prepared_sha256":crate::sandbox::code_recovery::sha256(&pre),
                }),
            );
        }
        if path == "intents" {
            if let Some(delay) = self.script.intent_delay {
                tokio::time::sleep(delay).await;
            }
            let pending: Option<bool> = sqlx::query_scalar(
                "SELECT submission_pending FROM elitea_runtime.sandbox_dispatches LIMIT 1",
            )
            .fetch_optional(&self.pool)
            .await
            .unwrap();
            {
                let mut calls = self.calls.lock().unwrap();
                calls.pending_at_intent.push(pending);
                if let Some(dispatch) = body["dispatch_activation"].as_str() {
                    calls.intent_dispatches.push(dispatch.to_owned());
                }
            }
            return json_response(
                false,
                &serde_json::json!({
                    "schema":"elitea.sandbox.original-code-intent-response.v1",
                    "intent":{
                        "schema":"elitea.sandbox.original-code-intent-signed.v1",
                        "key_id":"intent-key",
                        "claims_base64url":URL_SAFE_NO_PAD.encode(b"{}"),
                        "signature_base64url":URL_SAFE_NO_PAD.encode([0_u8; 64]),
                    },
                }),
            );
        }
        if path == "step" {
            let first = {
                let mut calls = self.calls.lock().unwrap();
                calls.steps += 1;
                calls.steps == 1
            };
            if first && let Some(delay) = self.script.first_step_delay {
                tokio::time::sleep(delay).await;
            }
            return json_response(
                false,
                &serde_json::json!({"schema":"elitea.runtime.code-platform-step-response.v1","revision":1,"disposition":"idle","effect_id":null}),
            );
        }
        if path == "effects" {
            {
                let mut calls = self.calls.lock().unwrap();
                calls.effects += 1;
                calls.effects_body = Some(body);
            }
            let answer = *self.effects.lock().unwrap();
            return match answer {
                Effects::Refused => json_response(true, &serde_json::json!({})),
                Effects::None => json_response(
                    false,
                    &serde_json::json!({"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"none"}),
                ),
                Effects::Possible => json_response(
                    false,
                    &serde_json::json!({"schema":"elitea.runtime.code-platform-effects-response.v1","revision":1,"disposition":"possible"}),
                ),
            };
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
    async fn claim_steering_messages(
        &self,
        _: Request<pb::ClaimSteeringMessagesRequestV1>,
    ) -> Result<Response<pb::ClaimSteeringMessagesResponseV1>, Status> {
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
    async fn report_egress_summary(
        &self,
        _: Request<pb::ReportEgressSummaryRequestV1>,
    ) -> Result<Response<pb::ReportEgressSummaryResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn report_run_steps(
        &self,
        _: Request<pb::ReportRunStepsRequestV1>,
    ) -> Result<Response<pb::ReportRunStepsResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn mint_delegation_assertion(
        &self,
        _: Request<pb::MintDelegationAssertionRequestV1>,
    ) -> Result<Response<pb::MintDelegationAssertionResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn report_browser_session(
        &self,
        _: Request<pb::ReportBrowserSessionRequestV1>,
    ) -> Result<Response<pb::ReportBrowserSessionResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn report_ephemeral_agents(
        &self,
        _: Request<pb::ReportEphemeralAgentsRequestV1>,
    ) -> Result<Response<pb::ReportEphemeralAgentsResponseV1>, Status> {
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
    /// Scripted terminal answers, one per submission; when empty, the runtime
    /// fails the way a thrown error does.
    answers: Arc<StdMutex<std::collections::VecDeque<pb::SubmitSandboxJobResponseV1>>>,
    /// Submissions answered Busy (`RESOURCE_EXHAUSTED`) before any answer.
    busy: Arc<StdMutex<usize>>,
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
        {
            let mut busy = self.busy.lock().unwrap();
            if *busy > 0 {
                *busy -= 1;
                return Err(Status::resource_exhausted("the supervisor is at capacity"));
            }
        }
        tokio::time::sleep(self.fail_after).await;
        if let Some(answer) = self.answers.lock().unwrap().pop_front() {
            return Ok(Response::new(answer));
        }
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
    async fn code_owner_recovery_read(
        &self,
        _: Request<pb::CodeOwnerRecoveryReadRequestV1>,
    ) -> Result<Response<pb::CodeOwnerRecoveryReadResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn code_owner_recovery_seal_no_effect(
        &self,
        _: Request<pb::CodeOwnerRecoverySealNoEffectRequestV1>,
    ) -> Result<Response<pb::CodeOwnerRecoverySealNoEffectResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn code_platform_owner_read_retained_runtime(
        &self,
        _: Request<pb::CodePlatformOwnerReadRetainedRuntimeRequestV1>,
    ) -> Result<Response<pb::CodePlatformOwnerReadRetainedRuntimeResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn code_platform_owner_read_pending_call(
        &self,
        _: Request<pb::CodePlatformOwnerReadPendingCallRequestV1>,
    ) -> Result<Response<pb::CodePlatformOwnerReadPendingCallResponseV1>, Status> {
        Err(Status::unimplemented(
            "not part of the Code attempt fixture",
        ))
    }
    async fn code_platform_owner_publish_reply(
        &self,
        _: Request<pb::CodePlatformOwnerPublishReplyRequestV1>,
    ) -> Result<Response<pb::CodePlatformOwnerPublishReplyResponseV1>, Status> {
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
    pool: sqlx::PgPool,
    factory: CodeRuntimeFactory,
    runtime: Arc<dyn CodeSandboxRuntime>,
    authority: Arc<ClaimBoundSandboxAuthority>,
    scope: DispatchScope,
    dispatch: DispatchJournal,
    effects: Arc<StdMutex<Effects>>,
    main: Arc<StdMutex<MainCalls>>,
    supervisor: Arc<StdMutex<SupervisorCalls>>,
    answers: Arc<StdMutex<std::collections::VecDeque<pb::SubmitSandboxJobResponseV1>>>,
    busy: Arc<StdMutex<usize>>,
}

async fn harness(script: MainScript, fail_after: Duration) -> Harness {
    harness_with(
        script,
        fail_after,
        None,
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await
}

/// The harness with the profiles' `code_slots` and the run's deadline (W3-1).
#[allow(clippy::too_many_lines)] // One harness wires every fake the code-recovery proofs share.
async fn harness_with(
    script: MainScript,
    fail_after: Duration,
    code_slots: Option<u32>,
    execution_deadline: tokio::time::Instant,
) -> Harness {
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
    let answers = Arc::new(StdMutex::new(std::collections::VecDeque::new()));
    let busy = Arc::new(StdMutex::new(0));
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
                answers: answers.clone(),
                busy: busy.clone(),
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
    let (mut pure, mut broker) = (runtime_config(false), runtime_config(true));
    pure.code_slots = code_slots;
    broker.code_slots = code_slots;
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
        runtime: factory.bind(authority.clone(), execution_deadline),
        factory,
        authority,
        dispatch: DispatchJournal::new(database.pool.clone()),
        effects,
        pool: database.pool.clone(),
        _database: database,
        scope,
        main,
        supervisor,
        answers,
        busy,
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
    intent_delay: None,
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
    assert_eq!(run.events().await, [] as [String; 0]);
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

// ------------------------------------- FU-214: a lost runtime versus user code

fn terminal(code: &str) -> pb::SubmitSandboxJobResponseV1 {
    pb::SubmitSandboxJobResponseV1 {
        status: pb::SandboxJobStatusV1::Failed.into(),
        result_json: Vec::new(),
        failure_code: code.into(),
        cleanup_pending: false,
    }
}
fn completed() -> pb::SubmitSandboxJobResponseV1 {
    pb::SubmitSandboxJobResponseV1 {
        status: pb::SandboxJobStatusV1::Completed.into(),
        result_json: br#"{"revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":41}","stderr":""}"#.to_vec(),
        failure_code: String::new(),
        cleanup_pending: false,
    }
}

/// A default-policy Code node, wrapped the way the production compiler wraps
/// a node without a `recovery` block.
struct DefaultRun {
    node: RecoverableNode,
    context: NodeContext,
    factory: Arc<Factory>,
    receiver: crate::agents::graph::node_events::PipelineNodeEventReceiver,
    activation: NodeAttemptActivation,
}
fn default_run(h: &Harness, yaml: &str) -> DefaultRun {
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
    let mut node = RecoverableNode::new(
        body,
        digest,
        NodeRecoveryPolicy::default(),
        factory.clone(),
        None,
    )
    .with_legacy_first_attempt();
    node.clock = Arc::new(Clock(AtomicU64::new(200)));
    DefaultRun {
        node,
        context,
        factory,
        receiver,
        activation,
    }
}
impl DefaultRun {
    async fn ledger(&self) -> crate::agents::graph::node_recovery::NodeAttemptLedger {
        self.factory
            .open(&self.activation, &NodeRecoveryPolicy::default())
            .await
            .unwrap()
            .load()
            .await
            .unwrap()
            .ledger
    }
    async fn dispatch(&self, attempt: u16) -> String {
        let journal = self
            .factory
            .open(&self.activation, &NodeRecoveryPolicy::default())
            .await
            .unwrap();
        crate::sandbox::code_recovery::hex(
            &NodeAttemptAuthority::committed(
                &self.activation,
                journal.activation_id,
                attempt,
                false,
            )
            .dispatch_activation(),
        )
    }
    async fn events(&self) -> Vec<String> {
        let mut drain = self.receiver.drain("inv", "agent", "root").await.unwrap();
        let mut codes = Vec::new();
        while let Some(item) = drain.try_recv() {
            if let Err(error) = item {
                codes.push(error.code.to_string());
            }
        }
        codes
    }
}
fn distinct(values: &[String]) -> Vec<String> {
    let mut seen = Vec::new();
    for value in values {
        if !seen.contains(value) {
            seen.push(value.clone());
        }
    }
    seen
}

/// The goal of FU-214: the supervisor proves the runtime lost, so effect-free
/// Code is re-run as a new attempt with its own identity and completes once.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn a_lost_runtime_is_rerun_with_a_new_identity_and_completes_once() {
    let h = harness(QUIET, Duration::ZERO).await;
    h.answers
        .lock()
        .unwrap()
        .extend([terminal("sandbox.runtime_lost"), completed()]);
    let run = default_run(&h, PURE_YAML);
    let output = run.node.execute(&run.context).await.unwrap();
    assert_eq!(output.updates["count"], serde_json::json!(41));
    assert!(output.interrupt.is_none());
    assert!(run.events().await.is_empty(), "no failure was published");
    assert_eq!(h.supervisor.lock().unwrap().submits, 2);
    let (visits, dispatches) = {
        let main = h.main.lock().unwrap();
        (
            main.visit_attempts.clone(),
            distinct(&main.intent_dispatches),
        )
    };
    assert_eq!(visits, [1, 2]);
    assert_eq!(dispatches.len(), 2, "one identity per attempt");
    // The re-run uses the ordinal identity Main derives for attempt 2.
    assert_eq!(dispatches[1], run.dispatch(2).await);
    let ledger = run.ledger().await;
    assert_eq!(ledger.history().len(), 2);
    assert!(matches!(
        ledger.phase(),
        crate::agents::graph::node_recovery::NodeAttemptPhase::Completed { .. }
    ));
    // Both dispatch rows are resolved: nothing is left for a Stop to cancel.
    assert!(h.dispatch.pending(&h.scope).await.unwrap().is_empty());
    // A replay projects the durable result and submits nothing.
    assert_eq!(
        run.node.execute(&run.context).await.unwrap().updates,
        output.updates
    );
    assert_eq!(h.supervisor.lock().unwrap().submits, 2);
}

/// A user-code failure is the result of the code: typed, never re-run.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn a_user_code_failure_settles_typed_and_is_never_rerun() {
    let h = harness(QUIET, Duration::ZERO).await;
    h.answers
        .lock()
        .unwrap()
        .push_back(terminal("sandbox.code_failed"));
    let run = default_run(&h, PURE_YAML);
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(run.events().await, ["pipeline.code_failed"]);
    assert_eq!(h.supervisor.lock().unwrap().submits, 1);
    assert_eq!(h.main.lock().unwrap().visit_attempts, [1]);
    let failure = run.ledger().await.effective_failure().unwrap();
    assert_eq!(failure.class, NodeFailureClass::InvalidResult);
    assert_eq!(failure.replay, ReplaySafety::NoExternalEffect);
}

/// The allowance is bounded: once it is spent the node settles typed.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn lost_runtimes_past_the_allowance_settle_typed() {
    use crate::agents::graph::node_recovery::MAX_RUNTIME_LOST_RETRIES;
    let h = harness(QUIET, Duration::ZERO).await;
    let attempts = usize::from(MAX_RUNTIME_LOST_RETRIES) + 1;
    h.answers
        .lock()
        .unwrap()
        .extend((0..attempts).map(|_| terminal("sandbox.runtime_lost")));
    let run = default_run(&h, PURE_YAML);
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(run.events().await, ["pipeline.code_failed"]);
    assert_eq!(h.supervisor.lock().unwrap().submits, attempts);
    assert_eq!(
        h.main.lock().unwrap().visit_attempts,
        (1..=u64::try_from(attempts).unwrap()).collect::<Vec<_>>()
    );
    let ledger = run.ledger().await;
    assert_eq!(ledger.history().len(), attempts);
    assert_eq!(
        ledger.phase(),
        crate::agents::graph::node_recovery::NodeAttemptPhase::Failed(
            crate::agents::graph::node_recovery::RecoveryDecision::Stop(
                crate::agents::graph::node_recovery::StopReason::RetryDisabled
            )
        )
    );
}

/// A lost runtime of a platform-client node re-runs only when Main's journal
/// proves no call unresolved; otherwise it reconciles and nothing re-runs.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn a_lost_runtime_with_a_possible_platform_effect_reconciles_and_never_reruns() {
    let h = harness(
        MainScript {
            effects: Effects::Possible,
            ..QUIET
        },
        Duration::ZERO,
    )
    .await;
    h.answers
        .lock()
        .unwrap()
        .push_back(terminal("sandbox.runtime_lost"));
    let run = default_run(&h, PLATFORM_YAML);
    let output = run.node.execute(&run.context).await.unwrap();
    assert!(output.interrupt.is_some(), "an operator card, not a re-run");
    assert_eq!(h.supervisor.lock().unwrap().submits, 1);
    let failure = run.ledger().await.effective_failure().unwrap();
    assert_eq!(failure.class, NodeFailureClass::InvalidResult);
    assert!(matches!(
        failure.replay,
        ReplaySafety::UnknownExternalEffect { .. }
    ));
}

/// A clean journal is not enough for a platform-client node: "nothing
/// unresolved" also covers calls that committed, which a re-run would repeat.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn a_lost_platform_client_runtime_with_a_clean_journal_settles_typed_and_never_reruns() {
    let h = harness(QUIET, Duration::ZERO).await;
    h.answers
        .lock()
        .unwrap()
        .extend([terminal("sandbox.runtime_lost"), completed()]);
    let run = default_run(&h, PLATFORM_YAML);
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(run.events().await, ["pipeline.code_failed"]);
    assert_eq!(h.supervisor.lock().unwrap().submits, 1);
    let failure = run.ledger().await.effective_failure().unwrap();
    assert_eq!(failure.class, NodeFailureClass::InvalidResult);
    assert_eq!(failure.replay, ReplaySafety::NoExternalEffect);
}

// ------------------------------------------- W3-1: Code slots and admission

/// Root Code with 30 s left and `timeout_seconds` 30 (needs 120 s): refused
/// readably before the original visit, with no dispatch row and no
/// submission; the node fails typed, once.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_root_code_with_too_little_time_is_refused_without_a_dispatch_row() {
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        None,
        tokio::time::Instant::now() + Duration::from_secs(30),
    )
    .await;
    let run = default_run(&h, PURE_YAML);
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(
        run.events().await,
        [crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE]
    );
    assert_eq!(h.supervisor.lock().unwrap().submits, 0);
    assert_eq!(
        h.main.lock().unwrap().visit_attempts,
        Vec::<u64>::new(),
        "no original visit"
    );
    let rows: i64 = sqlx::query_scalar("SELECT count(*) FROM elitea_runtime.sandbox_dispatches")
        .fetch_one(&h.pool)
        .await
        .unwrap();
    assert_eq!(rows, 0, "no sandbox dispatch row");
    let failure = run.ledger().await.effective_failure().unwrap();
    assert_eq!(failure.replay, ReplaySafety::NoExternalEffect);
    assert_eq!(run.ledger().await.history().len(), 1);
}

/// A Supervisor Busy/Pending caused by another Worker keeps the existing
/// backoff, and the attempt keeps its one slot through it until the
/// terminal receipt.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_busy_from_another_worker_backs_off_without_releasing_the_slot() {
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        Some(1),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    let pending = pb::SubmitSandboxJobResponseV1 {
        status: pb::SandboxJobStatusV1::Pending.into(),
        result_json: Vec::new(),
        failure_code: String::new(),
        cleanup_pending: false,
    };
    h.answers
        .lock()
        .unwrap()
        .extend([pending.clone(), pending, completed()]);
    let slots = h
        .factory
        .code_slots_for_tests("sandbox.internal:9446")
        .expect("the target's slots");
    let run = default_run(&h, PURE_YAML);
    let attempt = run.node.execute(&run.context);
    tokio::pin!(attempt);
    let mut saw_held = false;
    let output = loop {
        tokio::select! {
            output = &mut attempt => break output.unwrap(),
            () = tokio::time::sleep(Duration::from_millis(50)) => {
                if h.supervisor.lock().unwrap().submits >= 1 {
                    assert_eq!(slots.available(), 0, "the slot was released during backoff");
                    saw_held = true;
                }
            }
        }
    };
    assert!(saw_held, "the backoff was observed");
    assert_eq!(output.updates["count"], serde_json::json!(41));
    assert_eq!(h.supervisor.lock().unwrap().submits, 3);
    assert_eq!(slots.available(), 1);
}

/// The deadlock regression end to end: `code_slots = 1`, one attempt runs
/// its preparation and its job on one slot and completes.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_one_slot_runs_one_attempt_through_preparation_and_run() {
    let h = harness_with(
        QUIET,
        Duration::ZERO,
        Some(1),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    h.answers.lock().unwrap().push_back(completed());
    let run = default_run(&h, PURE_YAML);
    let output = tokio::time::timeout(Duration::from_secs(60), run.node.execute(&run.context))
        .await
        .expect("one attempt never waits on its own slot")
        .unwrap();
    assert_eq!(output.updates["count"], serde_json::json!(41));
    assert_eq!(h.supervisor.lock().unwrap().submits, 1);
}

const BUSY_CHILD_ENV: &str = "ELITEA_CODE_SUPERVISOR_BUSY_CHILD";

/// Supervisor Busy through the real submit loop: three Busy answers, then the
/// result. The attempt keeps its backoff and completes; the log has exactly
/// one INFO `code_supervisor_busy` line for it, and no slot-wait line (the
/// slot was free).
#[test]
#[ignore = "requires disposable PostgreSQL"]
fn real_pg_supervisor_busy_is_one_info_line_per_attempt() {
    let output = elitea_faults::logtest::run_child(
        "agents::graph::node_recovery_runtime::tests::code_owner_tests::wiring_tests::supervisor_busy_child",
        BUSY_CHILD_ENV,
    );
    let lines = output.operator_lines();
    let busy = elitea_faults::codes::PIPELINE_CODE_SUPERVISOR_BUSY;
    let busy_lines: Vec<_> = lines
        .iter()
        .filter(|line| line["code"] == busy.as_str())
        .collect();
    assert_eq!(busy_lines.len(), 1, "{}", output.stdout);
    assert_eq!(busy_lines[0]["level"], "INFO");
    assert_eq!(busy_lines[0]["supervisor_target"], "sandbox.internal:9446");
    assert!(
        !lines
            .iter()
            .any(|line| line["code"] == elitea_faults::codes::PIPELINE_CODE_SLOT_WAITED.as_str()),
        "an uncontended acquire logs nothing"
    );
}

#[test]
fn supervisor_busy_child() {
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;
    if !elitea_faults::logtest::is_child(BUSY_CHILD_ENV) {
        return;
    }
    tracing_subscriber::registry()
        .with(
            crate::diagnostics::log_layer(Some("info"), std::io::stdout).expect("operator stream"),
        )
        .init();
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(async {
            let h = harness(QUIET, Duration::ZERO).await;
            *h.busy.lock().unwrap() = 3;
            h.answers.lock().unwrap().push_back(completed());
            let run = default_run(&h, PURE_YAML);
            let output = run.node.execute(&run.context).await.unwrap();
            assert_eq!(output.updates["count"], serde_json::json!(41));
            assert_eq!(h.supervisor.lock().unwrap().submits, 4);
        });
    elitea_faults::logtest::child_ran();
}

// ------------------------------- W3-2: Code children, R1 and N3 (real PG)

/// One journaled root-path Code visit on its own thread and input, as one
/// fan-out child runs it: its own journal and its own dispatch identity.
fn sibling_run(h: &Harness, thread: &str, value: i64) -> DefaultRun {
    let mut run = default_run(h, PURE_YAML);
    run.context = NodeContext::new(
        State::from([
            ("value".into(), serde_json::json!(value)),
            ("input".into(), serde_json::json!("original user input")),
            ("count".into(), serde_json::json!(-1)),
        ]),
        ExecutionConfig::new(thread),
        2,
    );
    let digest = run.activation.node_digest;
    run.activation = NodeAttemptActivation::from_context("code", digest, &run.context).unwrap();
    run
}

/// R1 / C3: `code_slots = 1` and two sibling Code attempts polled by one
/// task, as two fan-out children are. The second waits for the first one's
/// slot; both complete, none is refused as a nested acquire.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_two_sibling_attempts_on_one_slot_both_complete_never_nested() {
    let h = harness_with(
        QUIET,
        Duration::from_millis(200),
        Some(1),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    h.answers.lock().unwrap().extend([completed(), completed()]);
    let slots = h
        .factory
        .code_slots_for_tests("sandbox.internal:9446")
        .expect("the target's slots");
    let first = sibling_run(&h, "child-a", 1);
    let second = sibling_run(&h, "child-b", 2);
    let (one, two) = tokio::time::timeout(Duration::from_secs(120), async {
        tokio::join!(
            first.node.execute(&first.context),
            second.node.execute(&second.context)
        )
    })
    .await
    .expect("one slot never deadlocks two siblings");
    assert_eq!(one.unwrap().updates["count"], serde_json::json!(41));
    assert_eq!(two.unwrap().updates["count"], serde_json::json!(41));
    assert_eq!(h.supervisor.lock().unwrap().submits, 2);
    assert!(first.events().await.is_empty() && second.events().await.is_empty());
    assert_eq!(slots.available(), 1);
}

/// N3: the window was there at admission, but the time spent inside the slot
/// before the first submission used it up. The fresh attempt is refused with
/// the same typed refusal before any submission, and its slot is free.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_preparation_past_the_window_is_refused_before_any_submission() {
    let required = Duration::from_secs(30)
        + elitea_agent_runtime::graph::fanout::admission::CODE_OBSERVATION_MARGIN;
    let h = harness_with(
        MainScript {
            intent_delay: Some(Duration::from_millis(1500)),
            ..QUIET
        },
        Duration::ZERO,
        Some(1),
        tokio::time::Instant::now() + Duration::from_secs(3600),
    )
    .await;
    h.answers.lock().unwrap().push_back(completed());
    let mut run = default_run(&h, PURE_YAML);
    // The run's deadline is bound just before the attempt, so a loaded
    // machine cannot spend the 600 ms margin before admission.
    let tight = h.factory.bind(
        h.authority.clone(),
        tokio::time::Instant::now() + required + Duration::from_millis(600),
    );
    let pipeline =
        crate::agents::graph::compiler::PipelineDefinition::from_yaml(PURE_YAML).unwrap();
    let definition = pipeline.original_code_definition_fixture("code").unwrap();
    let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
    run.receiver = receiver;
    run.node = RecoverableNode::new(
        Arc::new(
            CodeNode::new(definition, types(), tight)
                .unwrap()
                .with_events(Some(sender)),
        ),
        run.activation.node_digest,
        NodeRecoveryPolicy::default(),
        run.factory.clone(),
        None,
    )
    .with_legacy_first_attempt();
    run.node.clock = Arc::new(Clock(AtomicU64::new(200)));
    let error = run.node.execute(&run.context).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert_eq!(
        run.events().await,
        [crate::protocol::output::CODE_DEADLINE_INSUFFICIENT_UPSTREAM_CODE]
    );
    assert_eq!(h.main.lock().unwrap().visit_attempts, [1], "admitted first");
    assert_eq!(h.supervisor.lock().unwrap().submits, 0, "nothing submitted");
    let failure = run.ledger().await.effective_failure().unwrap();
    assert_eq!(failure.replay, ReplaySafety::NoExternalEffect);
    let slots = h
        .factory
        .code_slots_for_tests("sandbox.internal:9446")
        .expect("the target's slots");
    assert_eq!(slots.available(), 1, "the refused attempt holds no slot");
}

/// C2 for N3: an attempt that may already have started (claim 1 submitted,
/// then died) is reconciled, never refused after preparation, even with far
/// less than `timeout + margin` left on claim 2's run.
#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_possibly_started_attempt_is_never_refused_after_preparation() {
    let h = harness(QUIET, Duration::from_millis(100)).await;
    claim_one_submitted(&h).await;
    *h.effects.lock().unwrap() = Effects::Possible;
    let mut run = run(&h, PLATFORM_YAML, true).await;
    // Claim 2's run has 5 s left: a fresh attempt would be refused.
    let short = h.factory.bind(
        h.authority.clone(),
        tokio::time::Instant::now() + Duration::from_secs(5),
    );
    let pipeline =
        crate::agents::graph::compiler::PipelineDefinition::from_yaml(PLATFORM_YAML).unwrap();
    let definition = pipeline.original_code_definition_fixture("code").unwrap();
    let (sender, receiver) = crate::agents::graph::node_events::pipeline_node_event_channel();
    let body = Arc::new(
        CodeNode::new(definition, types(), short)
            .unwrap()
            .with_events(Some(sender)),
    );
    run.receiver = receiver;
    run.node = RecoverableNode::new(body, run.node_digest, policy(), run.factory.clone(), None);
    run.node.clock = Arc::new(Clock(AtomicU64::new(200)));
    let output = run.node.execute(&run.context).await.unwrap();
    assert!(
        output.interrupt.is_some(),
        "the possibly started attempt goes to reconciliation"
    );
    assert_eq!(run.events().await, [] as [String; 0], "never refused");
    assert_eq!(
        run.failure().await.replay,
        ReplaySafety::UnknownExternalEffect {
            effect_id: run.dispatch_activation
        }
    );
}

#[path = "node_recovery_code_fanout_pg_tests.rs"]
mod fanout_pg_tests;
