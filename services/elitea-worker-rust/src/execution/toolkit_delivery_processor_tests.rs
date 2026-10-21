//! One whole `toolkit.call_tool.v1` delivery through the toolkit processor:
//! claim, begin, input, authorization, the tool, the terminal, settlement and
//! retirement, over in-memory control, input, output and runtime-context
//! transports.
//!
//! Live 4.45.0 (D4): an `artifact` toolkit's `read_file` / `list_files` test
//! call answered `unsupported_toolkit`, because a direct call lent the family
//! no artifact authority. The read tools now run under the claim's artifact
//! routes; a write tool is still refused before it runs.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use prost::Message;
use ring::{digest, hmac};
use serde_json::{Value, json};
use tonic::Status;

use super::agent_preparation::{AgentInputMaterializer, AgentPreparationConfig};
use super::invocation_admission::{InvocationAdmission, InvocationAdmissionConfig};
use super::output_delivery::{
    AgentOutputPreflight, AgentTerminalRecoveryConfig, ToolkitTerminalReplay,
};
use super::toolkit_delivery_processor::ToolkitDeliveryProcessor;
use crate::protocol::command::{
    TestOnlyConformanceHmacAuthenticator, VerifiedToolkitExecuteReadCommand,
    parse_and_verify_toolkit_execute_read_command,
};
use crate::protocol::control::{AgentControlClient, LeaseMonitoredAgentExecution};
use crate::protocol::elitea::runtime::v1::{
    AuthorizeInvocationDispositionV1, AuthorizeInvocationRequestV1, AuthorizeInvocationResponseV1,
    BeginExecutionDispositionV1, BeginExecutionRequestV1, BeginExecutionResponseV1,
    ClaimCommandRequestV1, ClaimCommandResponseV1, DesiredExecutionStateV1, DigestAlgorithmV1,
    DigestV1, ExecutionOutputFrameV1, ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1,
    PrepareSettlementRequestV1, PrepareSettlementResponseV1, RenewLeaseRequestV1,
    RenewLeaseResponseV1, SignedWorkerCommandEnvelopeV1, ToolkitCallToolCommandV1,
    ToolkitCallToolStatusV1, WorkerCommandTypeV1, WorkerCommandV1, execution_output_frame_v1,
    worker_command_v1,
};
use crate::spool::{SpoolLimits, SpoolMasterKey};
use crate::toolkits::DirectToolkitRuntime;
use crate::transport::command_bus::{
    CommandRetirementClient, CommandRetirementClientError, CommandRetirementConfig,
    CommandRetirementRequest, CommandRetirer,
};
use crate::transport::input_content::content_fake::{
    ContextAnswer, ContextCall, ContextRoutes, Routes,
};
use crate::transport::output_grpc::test_acknowledged_terminal;
use crate::transport::platform_client::PlatformClient;
use crate::transport::runtime_context::{RuntimeContextClient, RuntimeContextConfig};
use crate::transport::{
    ControlGrpcConfig, ControlRpc, DurablyAckedTerminal, InputContentError, MaterializedInput,
    OutputGrpcConfig, OutputGrpcError, PreparedOutputSpool,
};

const NOW: i64 = 1_700_000_000_000;

fn vector(name: &str) -> Vec<u8> {
    let (_, hex) = include_str!("../../tests/fixtures/agent_control_vectors.txt")
        .lines()
        .filter_map(|line| line.split_once('='))
        .find(|(key, _)| *key == name)
        .expect("fixture");
    hex.as_bytes()
        .chunks_exact(2)
        .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).expect("hex"), 16).expect("byte"))
        .collect()
}

fn sha256(raw: &[u8]) -> DigestV1 {
    DigestV1 {
        algorithm: DigestAlgorithmV1::Sha256 as i32,
        value: digest::digest(&digest::SHA256, raw).as_ref().to_vec(),
    }
}

/// The three entries one call reads: the saved toolkit, the arguments and the
/// runtime context.
struct CallInputs {
    settings: Vec<u8>,
    arguments: Vec<u8>,
    context: Vec<u8>,
}

impl CallInputs {
    fn artifact(tool_arguments: &Value) -> Self {
        Self {
            settings: serde_json::to_vec(&json!({
                "id": 12, "type": "artifact", "toolkit_name": "files",
                "settings": {"bucket": "agent-artifacts", "selected_tools": []}
            }))
            .expect("settings"),
            arguments: serde_json::to_vec(tool_arguments).expect("arguments"),
            context: br#"{"toolkit_security":{}}"#.to_vec(),
        }
    }
}

/// A signed `toolkit.call_tool.v1` command for `artifact.<tool>` and the
/// claim Main answers it with, its manifest bound to the three entries.
fn call_command(tool: &str, inputs: &CallInputs) -> (Vec<u8>, ClaimCommandResponseV1) {
    let mut signed = SignedWorkerCommandEnvelopeV1::decode(vector("signed_command").as_slice())
        .expect("signed command");
    let mut command =
        WorkerCommandV1::decode(signed.worker_command_bytes.as_slice()).expect("command");
    let mut response =
        ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).expect("claim");
    let receipt = response.receipt.as_mut().expect("receipt");
    let manifest = receipt.input_bundle.as_mut().expect("manifest");
    let entry = |id: &str, role: &str, content_id: &str, raw: &[u8]| {
        let mut entry = manifest.entries[0].clone();
        entry.entry_id = id.to_owned();
        entry.semantic_role = role.to_owned();
        let content = entry.content.as_mut().expect("content");
        content.media_type = "application/json".to_owned();
        content.content_id = content_id.to_owned();
        content.byte_length = raw.len() as u64;
        content.digest = Some(sha256(raw));
        entry
    };
    let settings = entry(
        "toolkit-settings",
        "toolkit.call_tool.settings",
        "settings-content",
        &inputs.settings,
    );
    let arguments = entry(
        "toolkit-arguments",
        "toolkit.call_tool.arguments",
        "argument-content",
        &inputs.arguments,
    );
    let context = entry(
        "toolkit-runtime-context",
        "toolkit.call_tool.runtime_context",
        "context-content",
        &inputs.context,
    );
    manifest.entries = vec![arguments, context, settings];
    command.command_type = WorkerCommandTypeV1::ToolkitCallTool as i32;
    command.capability_id = "toolkit.call_tool.v1".to_owned();
    command.capability_command = Some(worker_command_v1::CapabilityCommand::ToolkitCallTool(
        ToolkitCallToolCommandV1 {
            toolkit_type: "artifact".to_owned(),
            settings_entry_id: "toolkit-settings".to_owned(),
            tool_name: tool.to_owned(),
            arguments_entry_id: "toolkit-arguments".to_owned(),
            toolkit_id: "12".to_owned(),
            toolkit_version: "1".to_owned(),
        },
    ));
    command.deadline_unix_millis = NOW + 600_000;
    let raw = manifest.encode_to_vec();
    let reference = command.input_bundle_ref.as_mut().expect("reference");
    reference.digest = Some(sha256(&raw));
    reference.byte_length = raw.len() as u64;
    receipt.input_bundle_ref = Some(reference.clone());
    receipt.claim_started_at_unix_micros = NOW * 1_000;
    receipt.lease_expires_at_unix_millis = NOW + 600_000;
    signed.worker_command_bytes = command.encode_to_vec();
    signed.worker_command_digest = Some(sha256(&signed.worker_command_bytes));
    signed.signature = hmac::sign(
        &hmac::Key::new(
            hmac::HMAC_SHA256,
            b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET",
        ),
        &signed.worker_command_bytes,
    )
    .as_ref()
    .to_vec();
    (signed.encode_to_vec(), response)
}

/// Main's control plane: one accepted claim, a fresh begin, an authorized
/// invocation, a running lease and a settlement receipt.
struct CallControl {
    claim: ClaimCommandResponseV1,
    trace: Arc<Mutex<Vec<&'static str>>>,
}

#[async_trait]
impl ControlRpc for CallControl {
    async fn claim_command(
        &self,
        _: tonic::Request<ClaimCommandRequestV1>,
    ) -> Result<tonic::Response<ClaimCommandResponseV1>, Status> {
        self.trace.lock().expect("trace").push("claim");
        Ok(tonic::Response::new(self.claim.clone()))
    }

    async fn begin_execution(
        &self,
        _: tonic::Request<BeginExecutionRequestV1>,
    ) -> Result<tonic::Response<BeginExecutionResponseV1>, Status> {
        self.trace.lock().expect("trace").push("begin");
        Ok(tonic::Response::new(BeginExecutionResponseV1 {
            disposition: BeginExecutionDispositionV1::StartedNow as i32,
            rejection: None,
        }))
    }

    async fn authorize_invocation(
        &self,
        _: tonic::Request<AuthorizeInvocationRequestV1>,
    ) -> Result<tonic::Response<AuthorizeInvocationResponseV1>, Status> {
        self.trace.lock().expect("trace").push("authorize");
        Ok(tonic::Response::new(AuthorizeInvocationResponseV1 {
            disposition: AuthorizeInvocationDispositionV1::AuthorizedNow as i32,
            rejection: None,
        }))
    }

    async fn renew_lease(
        &self,
        _: tonic::Request<RenewLeaseRequestV1>,
    ) -> Result<tonic::Response<RenewLeaseResponseV1>, Status> {
        Ok(tonic::Response::new(RenewLeaseResponseV1 {
            workspace_wrap_up: false,
            lease_expires_at_unix_millis: NOW + 600_000,
            desired_state: DesiredExecutionStateV1::Running as i32,
            rejection: None,
        }))
    }

    async fn observe_desired_state(
        &self,
        _: tonic::Request<ObserveDesiredStateRequestV1>,
    ) -> Result<tonic::Response<ObserveDesiredStateResponseV1>, Status> {
        Ok(tonic::Response::new(ObserveDesiredStateResponseV1 {
            workspace_wrap_up: false,
            desired_state: DesiredExecutionStateV1::Running as i32,
            rejection: None,
        }))
    }

    async fn prepare_settlement(
        &self,
        request: tonic::Request<PrepareSettlementRequestV1>,
    ) -> Result<tonic::Response<PrepareSettlementResponseV1>, Status> {
        self.trace.lock().expect("trace").push("settlement");
        let outcome = request
            .into_inner()
            .proposal
            .expect("settlement proposal")
            .requested_outcome;
        Ok(tonic::Response::new(PrepareSettlementResponseV1 {
            settlement_receipt_id: "settlement-receipt-1".to_owned(),
            outcome,
            rejection: None,
        }))
    }
}

/// The content plane: each entry's bytes by id.
struct CallInput(CallInputs);

#[async_trait]
impl AgentInputMaterializer for CallInput {
    async fn materialize(
        &self,
        _: &LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError> {
        Ok(MaterializedInput::for_test(self.0.settings.clone()))
    }

    async fn materialize_entry(
        &self,
        _: &LeaseMonitoredAgentExecution,
        entry_id: &str,
    ) -> Result<MaterializedInput, InputContentError> {
        let bytes = match entry_id {
            "toolkit-settings" => &self.0.settings,
            "toolkit-arguments" => &self.0.arguments,
            "toolkit-runtime-context" => &self.0.context,
            _ => {
                return Err(InputContentError::InvalidInput(
                    "the input entry is not available",
                ));
            }
        };
        Ok(MaterializedInput::for_test(bytes.clone()))
    }
}

/// The output plane: Main durably acknowledges the terminal frame.
#[derive(Default)]
struct AckingReplay {
    frames: Mutex<Vec<ExecutionOutputFrameV1>>,
}

#[async_trait]
impl ToolkitTerminalReplay for AckingReplay {
    async fn replay_toolkit_terminal(
        &self,
        mut spool: PreparedOutputSpool,
        verified: &VerifiedToolkitExecuteReadCommand,
        expected: &ExecutionOutputFrameV1,
    ) -> Result<DurablyAckedTerminal, OutputGrpcError> {
        assert!(spool.replays(expected));
        self.frames.lock().expect("frames").push(expected.clone());
        spool.reconcile_pending_through(expected.sequence)?;
        test_acknowledged_terminal(verified, expected)
    }
}

struct CallBus {
    trace: Arc<Mutex<Vec<&'static str>>>,
}

#[async_trait]
impl CommandRetirementClient for CallBus {
    async fn retire_delivery(
        &self,
        _: CommandRetirementRequest,
    ) -> Result<(), CommandRetirementClientError> {
        self.trace.lock().expect("trace").push("retire");
        Ok(())
    }
}

/// Main's runtime-context service: the artifact calls, answered from one
/// file, recording every call the worker made.
struct ArtifactRoutes {
    /// The claim's resource project: Main answers inside it, and the worker
    /// refuses an answer about any other.
    project_id: u64,
    paths: Mutex<Vec<String>>,
    claim_headers: Mutex<Vec<bool>>,
}

#[tonic::async_trait]
impl ContextRoutes for ArtifactRoutes {
    async fn answer(&self, call: ContextCall) -> Result<ContextAnswer, Status> {
        let path = call.route;
        self.claim_headers
            .lock()
            .expect("headers")
            .push(!call.claim.is_empty() && call.fence.len() == 32);
        let sent: Value = serde_json::from_slice(&call.body).unwrap_or(Value::Null);
        self.paths.lock().expect("paths").push(path.clone());
        let body = if path == "artifacts/read" {
            assert_eq!(sent["bucket"], "agent-artifacts", "the toolkit's bucket");
            json!({
                "schema_version": "elitea.runtime.artifact-read.v1",
                "project_id": self.project_id,
                "bucket": "agent-artifacts",
                "name": "notes.txt",
                "media_type": "text/plain",
                "byte_length": 11,
                "char_length": 11,
                "total_lines": 1,
                "max_chars": 200_000,
                "over_limit": false,
                "content": "hello world",
            })
        } else if path == "artifacts/list" {
            json!({
                "schema_version": "elitea.runtime.artifact-list.v1",
                "project_id": self.project_id,
                "bucket": "agent-artifacts",
                "files": [{"name": "notes.txt", "media_type": "text/plain",
                           "byte_length": 11, "modified_at": "2026-10-15T10:00:00Z"}],
                "truncated": false,
            })
        } else {
            panic!("the direct path posted to {path}");
        };
        Ok(ContextAnswer::from(&body))
    }
}

struct ArtifactRoutesHandle(Arc<ArtifactRoutes>);

#[tonic::async_trait]
impl ContextRoutes for ArtifactRoutesHandle {
    async fn answer(&self, call: ContextCall) -> Result<ContextAnswer, Status> {
        self.0.answer(call).await
    }
}

fn preflight(root: std::path::PathBuf) -> AgentOutputPreflight {
    AgentOutputPreflight::new(
        root,
        SpoolMasterKey::new([b'p'; 32]),
        SpoolLimits {
            max_frames: 1,
            max_encrypted_bytes: 65 * 1024,
            max_frame_bytes: 64 * 1024,
        },
        OutputGrpcConfig {
            max_queued_frames: 1,
            max_queued_bytes: 64 * 1024,
            max_frame_bytes: 64 * 1024,
            max_server_credit_frames: 1,
            max_server_credit_bytes: 64 * 1024,
            stream_deadline: Duration::from_mins(1),
            ack_timeout: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    )
}

/// What one delivery produced.
struct CallRun {
    summary: crate::protocol::elitea::runtime::v1::ToolkitCallToolSummaryV1,
    routes: Vec<String>,
    claim_headers: Vec<bool>,
    trace: Vec<&'static str>,
}

fn runtime_context_client(routes: &Arc<ArtifactRoutes>) -> RuntimeContextClient {
    RuntimeContextClient::with_rpc(
        Routes(ArtifactRoutesHandle(Arc::clone(routes))),
        RuntimeContextConfig {
            origin: "https://content.internal:443".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 2 * 1_024 * 1_024,
        },
    )
    .expect("runtime-context client")
}

async fn run_call(tool: &str, arguments: &Value) -> CallRun {
    let temporary = tempfile::tempdir_in(std::env::temp_dir().canonicalize().expect("temp root"))
        .expect("private root");
    fs::set_permissions(temporary.path(), fs::Permissions::from_mode(0o700)).expect("permissions");
    let (raw, claim) = call_command(tool, &CallInputs::artifact(arguments));
    let project_id = claim
        .receipt
        .as_ref()
        .and_then(|receipt| receipt.identity.as_ref())
        .map(|identity| {
            identity
                .resource_project_id
                .parse()
                .expect("numeric project")
        })
        .expect("claim project");
    let trace = Arc::new(Mutex::new(Vec::new()));
    let control = Arc::new(
        AgentControlClient::new(
            CallControl {
                claim,
                trace: Arc::clone(&trace),
            },
            ControlGrpcConfig {
                deadline: Duration::from_secs(1),
                workload_session_id: "workload-1".to_owned(),
                producer_id: "worker-1".to_owned(),
            },
        )
        .expect("control"),
    );
    let retirer = Arc::new(
        CommandRetirer::new(
            CallBus {
                trace: Arc::clone(&trace),
            },
            CommandRetirementConfig {
                stream: "ELITEA_RT_V1_AGENT".to_owned(),
                consumer: "elitea-agent-worker-v1".to_owned(),
            },
        )
        .expect("retirer"),
    );
    let routes = Arc::new(ArtifactRoutes {
        project_id,
        paths: Mutex::new(Vec::new()),
        claim_headers: Mutex::new(Vec::new()),
    });
    let runtime_context = runtime_context_client(&routes);
    let replay = Arc::new(AckingReplay::default());
    let processor = ToolkitDeliveryProcessor::new(
        preflight(temporary.path().to_path_buf()),
        control,
        retirer,
        Arc::clone(&replay),
        Arc::new(CallInput(CallInputs::artifact(arguments))),
        Arc::new(|| NOW),
        InvocationAdmission::new(
            InvocationAdmissionConfig::new(1, Duration::from_secs(1)).expect("admission"),
        ),
        AgentPreparationConfig::new(Duration::from_secs(10)).expect("preparation"),
        AgentTerminalRecoveryConfig::new(1).expect("recovery"),
        DirectToolkitRuntime::default(),
        Arc::new(PlatformClient::new(Arc::new(runtime_context))),
        None,
    );
    let verified = parse_and_verify_toolkit_execute_read_command(
        &raw,
        Some(&TestOnlyConformanceHmacAuthenticator),
    )
    .expect("authenticated toolkit command");
    let delivery = crate::transport::command_bus::test_agent_delivery(raw);
    processor
        .process_verified(delivery, verified)
        .await
        .unwrap_or_else(|error| panic!("the delivery failed: {}", error.code()));
    let frames = replay.frames.lock().expect("frames");
    assert_eq!(frames.len(), 1, "one terminal");
    let Some(execution_output_frame_v1::Payload::ToolkitCallTool(result)) =
        frames[0].payload.clone()
    else {
        panic!(
            "the terminal is not a call-tool result: {:?}",
            frames[0].payload
        );
    };
    CallRun {
        summary: result.result_summary.expect("summary"),
        routes: routes.paths.lock().expect("paths").clone(),
        claim_headers: routes.claim_headers.lock().expect("headers").clone(),
        trace: trace.lock().expect("trace").clone(),
    }
}

#[tokio::test]
async fn an_artifact_read_tool_runs_on_the_direct_path_under_the_claim() {
    let run = run_call("read_file", &json!({"filename": "notes.txt"})).await;
    assert_eq!(
        run.summary.status,
        ToolkitCallToolStatusV1::Ok as i32,
        "{}",
        run.summary.error_message
    );
    assert!(
        run.summary.result_json.contains("hello world"),
        "{}",
        run.summary.result_json
    );
    assert_eq!(run.routes.len(), 1);
    assert_eq!(run.routes[0], "artifacts/read", "{:?}", run.routes);
    assert!(
        run.claim_headers.iter().all(|present| *present),
        "the claim authorizes the read"
    );
    assert_eq!(
        run.trace,
        ["claim", "begin", "authorize", "settlement", "retire"]
    );

    let listed = run_call("list_files", &json!({})).await;
    assert_eq!(
        listed.summary.status,
        ToolkitCallToolStatusV1::Ok as i32,
        "{}",
        listed.summary.error_message
    );
    assert!(
        listed.summary.result_json.contains("notes.txt"),
        "{}",
        listed.summary.result_json
    );
    assert_eq!(listed.routes[0], "artifacts/list");
}

/// The direct path never widens past read-only: an artifact write tool is
/// refused as an effect before it runs, and no write route is called.
#[tokio::test]
async fn an_artifact_write_tool_is_refused_on_the_direct_path() {
    let run = run_call(
        "create_file",
        &json!({"filename": "notes.txt", "filedata": "x"}),
    )
    .await;
    // This claim's Main did not advertise typed refusals, so the refusal
    // arrives in the round-1 shape (`terminal_for_main`): UNKNOWN_TOOL with
    // the fixed effect sentence.
    assert_eq!(
        run.summary.status,
        ToolkitCallToolStatusV1::UnknownTool as i32
    );
    assert_eq!(
        run.summary.error_message,
        super::toolkit_delivery_processor::EFFECTFUL_TOOL_MESSAGE
    );
    assert!(
        run.routes.is_empty(),
        "nothing reached the platform: {:?}",
        run.routes
    );
}
