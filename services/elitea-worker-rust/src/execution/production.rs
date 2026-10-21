//! Production composition of the already-proven agent delivery/runtime pieces.

use std::fmt;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;
use tokio::signal::unix::{Signal, SignalKind, signal};
use tokio::sync::watch;
use tonic::transport::Channel;

use super::agent_delivery_processor::{AgentDeliveryProcessor, native_agent_delivery_processor};
use super::agent_lease::SystemUnixMillisClock;
use super::agent_preparation::AgentPreparationConfig;
use super::command_delivery::{
    CommandDeliveryIntakeConfig, CommandDeliveryRuntime, CommandDeliveryRuntimeConfig,
};
use super::execution_delivery_processor::ExecutionDeliveryProcessor;
use super::invocation_admission::{InvocationAdmission, InvocationAdmissionConfig};
use super::native_agent_lifecycle::NativeAuthorizedAgentLifecycle;
use super::output_delivery::{AgentOutputPreflight, AgentTerminalRecoveryConfig};
use super::toolkit_delivery_processor::ToolkitDeliveryProcessor;
use crate::agents::native_runtime::NativeRuntimeAssembler;
use crate::agents::ordinary::OrdinaryNativeAgentAssembler;
use crate::agents::pipeline::PipelineNativeAgentAssembler;
use crate::bootstrap::ProductionTransportBundle;
use crate::config::{RuntimeConfigError, load_deploy_config, read_regular_file};
use crate::spool::SpoolLimits;
use crate::state::{CheckpointLimits, SessionLimits};
use crate::toolkits::{
    DirectToolkitRuntime, ToolAdmissionPolicy, ToolAdmissionPolicyError,
    ToolAdmissionPolicyErrorCode,
};
use crate::transport::command_bus::{CommandRetirementConfig, CommandRetirer};
use crate::transport::nats_jetstream::NatsCommandBus;
use crate::transport::{OutputGrpcConfig, TonicControlRpc};

type ProductionAssembler =
    NativeRuntimeAssembler<OrdinaryNativeAgentAssembler, PipelineNativeAgentAssembler>;
type ProductionCommandBus = Arc<NatsCommandBus>;
type ProductionLifecycle = NativeAuthorizedAgentLifecycle<
    ProductionAssembler,
    Channel,
    TonicControlRpc,
    ProductionCommandBus,
    SystemUnixMillisClock,
>;
type ProductionAgentProcessor = AgentDeliveryProcessor<
    TonicControlRpc,
    ProductionCommandBus,
    Channel,
    SystemUnixMillisClock,
    ProductionLifecycle,
    crate::transport::InputContentClient,
>;
type ProductionProcessor = ExecutionDeliveryProcessor<
    TonicControlRpc,
    ProductionCommandBus,
    Channel,
    SystemUnixMillisClock,
    ProductionLifecycle,
    crate::transport::InputContentClient,
>;

const MAX_TOOLKIT_SECURITY_SNAPSHOT_BYTES: usize = 1024 * 1024;

/// Stable process-level failure. It carries no path, endpoint, credential,
/// provider text or execution data and is therefore safe at the CLI boundary.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProductionServeError {
    code: &'static str,
    retryable: bool,
    message: &'static str,
}

impl ProductionServeError {
    #[must_use]
    pub const fn code(self) -> &'static str {
        self.code
    }

    #[must_use]
    pub const fn retryable(self) -> bool {
        self.retryable
    }
}

impl fmt::Display for ProductionServeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for ProductionServeError {}

struct ShutdownSignals {
    interrupt: Signal,
    terminate: Signal,
}

impl ShutdownSignals {
    fn install() -> Result<Self, ProductionServeError> {
        Ok(Self {
            interrupt: signal(SignalKind::interrupt()).map_err(|_| signal_unavailable())?,
            terminate: signal(SignalKind::terminate()).map_err(|_| signal_unavailable())?,
        })
    }

    async fn receive(&mut self) -> Result<(), ProductionServeError> {
        let received = tokio::select! {
            received = self.interrupt.recv() => received,
            received = self.terminate.recv() => received,
        };
        received.ok_or_else(signal_unavailable)
    }
}

/// Load the two independent production snapshots, connect dependencies and run
/// until SIGINT/SIGTERM requests one bounded drain. Toolkit policy is a
/// separate argument so the shared `elitea.runtime-deploy.v1` document remains
/// byte-compatible with the Python worker during parallel operation.
pub async fn serve_from_config(
    deployment_path: &Path,
    toolkit_security_path: &Path,
) -> Result<(), ProductionServeError> {
    let mut signals = ShutdownSignals::install()?;
    let deployment =
        Arc::new(load_deploy_config(deployment_path).map_err(|error| map_config_error(&error))?);
    let tool_policy = load_tool_policy(toolkit_security_path)?;
    tracing::info!(
        event = "production_startup_admitted",
        policy_generation = "startup_snapshot",
    );

    let connect = ProductionTransportBundle::connect(Arc::clone(&deployment));
    tokio::pin!(connect);
    let bundle = tokio::select! {
        result = &mut connect => result.map_err(map_bootstrap_error)?,
        requested = signals.receive() => {
            requested?;
            tracing::info!(event = "production_stop_before_intake");
            return Ok(());
        }
    };
    let runtime =
        ProductionAgentRuntime::from_bundle(bundle, tool_policy).map_err(|_| invalid_runtime())?;
    let shutdown_timeout = Duration::from_millis(deployment.limits.shutdown_timeout_millis);
    let (stop, stopped) = watch::channel(false);
    let running = Box::pin(runtime.run(stopped));
    tracing::info!(event = "production_intake_started");
    drive(
        running,
        &stop,
        signals.receive(),
        office_core_stuck(OFFICE_HEALTH_POLL, || elitea_office_core::health().stuck()),
        shutdown_timeout,
    )
    .await
}

/// How often the worker asks the Office engine whether a library call has
/// outlived its thread (ADR-0032 decision 3).
const OFFICE_HEALTH_POLL: Duration = Duration::from_secs(5);

/// Completes once `stuck` has reported a runaway library call.
///
/// The Office libraries cannot be cancelled: a call past its deadline returns
/// an error to the tool, but its thread keeps running and keeps its slot. A
/// slot held for more than three deadlines is a thread the library will not
/// give back, and the process cannot clean it up, so the worker drains and
/// exits non-zero and the orchestrator restarts the pod. (The Deployment has
/// no probe that would do it, and a readiness failure would not restart a
/// pull-based consumer.)
async fn office_core_stuck(poll: Duration, stuck: impl Fn() -> bool) {
    loop {
        tokio::time::sleep(poll).await;
        if stuck() {
            return;
        }
    }
}

/// Run intake until it ends, a stop signal arrives or the Office engine is
/// stuck. A signal and a stuck engine both stop claiming new work and give
/// the in-flight work the shutdown deadline to drain; the stuck engine then
/// ends the process with an error (non-zero exit) even when the drain
/// succeeded, so the pod restarts.
async fn drive<R>(
    mut running: std::pin::Pin<Box<R>>,
    stop: &watch::Sender<bool>,
    signal: impl std::future::Future<Output = Result<(), ProductionServeError>>,
    stuck: impl std::future::Future<Output = ()>,
    shutdown_timeout: Duration,
) -> Result<(), ProductionServeError>
where
    R: std::future::Future<Output = Result<(), ProductionAgentRuntimeError>>,
{
    tokio::select! {
        result = &mut running => result.map_err(map_runtime_error),
        requested = signal => {
            requested?;
            tracing::info!(event = "production_stop_requested");
            // Analyses queued for a slot are told the worker is shutting down;
            // the ones running finish within the drain.
            elitea_agent_runtime::toolkits::close_analysis_gate();
            if stop.send(true).is_err() {
                return running.await.map_err(map_runtime_error);
            }
            match tokio::time::timeout(shutdown_timeout, running).await {
                Ok(result) => result.map_err(map_runtime_error),
                Err(_) => Err(shutdown_timeout_error()),
            }
        }
        () = stuck => {
            tracing::error!(
                event = "office_core_stuck",
                error_code = "worker_serve.office_core_stuck",
                "a document-library call will not give its thread back: draining and exiting"
            );
            elitea_agent_runtime::toolkits::close_analysis_gate();
            if stop.send(true).is_ok() {
                // Whatever the drain says, the process exits with the error.
                let _ = tokio::time::timeout(shutdown_timeout, running).await;
            }
            Err(office_core_stuck_error())
        }
    }
}

fn load_tool_policy(path: &Path) -> Result<Arc<ToolAdmissionPolicy>, ProductionServeError> {
    let raw = read_regular_file(
        path,
        MAX_TOOLKIT_SECURITY_SNAPSHOT_BYTES,
        false,
        "toolkit security snapshot",
    )
    .map_err(|error| map_config_error(&error))?;
    let runtime = serde_json::from_slice::<Value>(&raw).map_err(|_| invalid_policy())?;
    let runtime = runtime.as_object().ok_or_else(invalid_policy)?;
    if !runtime.contains_key("toolkit_security") {
        return Err(invalid_policy());
    }
    ToolAdmissionPolicy::from_runtime_config(runtime)
        .map(Arc::new)
        .map_err(|error| map_policy_error(&error))
}

/// Stable composition failure before command intake or command authority exists.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct ProductionRuntimeBuildError;

impl fmt::Display for ProductionRuntimeBuildError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the production agent runtime configuration is invalid")
    }
}

impl std::error::Error for ProductionRuntimeBuildError {}

/// One process-owned command delivery runtime plus its native invocation drain
/// owner. Construction requires the authoritative policy explicitly; there is
/// no default or missing-policy branch.
pub(crate) struct ProductionAgentRuntime {
    delivery: CommandDeliveryRuntime<NatsCommandBus, ProductionProcessor>,
    processor: Arc<ProductionProcessor>,
}

impl ProductionAgentRuntime {
    /// Consume connected dependencies and bind both direct and graph agents to
    /// one exact frozen toolkit-security policy.
    #[allow(clippy::too_many_lines)] // Keep the security-significant ownership handoff linear.
    pub(crate) fn from_bundle(
        bundle: ProductionTransportBundle,
        tool_policy: Arc<ToolAdmissionPolicy>,
    ) -> Result<Self, ProductionRuntimeBuildError> {
        let ProductionTransportBundle {
            deployment,
            command_authenticator,
            spool_root,
            spool_master_key,
            command_bus,
            control,
            output,
            input,
            platform,
            model_facade,
            agentstate,
            sandbox,
            index_runtime,
            extractor,
            workspace_runs,
            platform_identity,
        } = bundle;
        let limits = deployment.limits;
        let output_config = OutputGrpcConfig {
            max_queued_frames: limits.output_max_queued_frames,
            max_queued_bytes: limits.output_max_queued_bytes,
            max_frame_bytes: limits.output_max_frame_bytes(),
            max_server_credit_frames: u32::try_from(limits.output_max_queued_frames)
                .map_err(|_| ProductionRuntimeBuildError)?,
            max_server_credit_bytes: u64::try_from(limits.output_max_queued_bytes)
                .map_err(|_| ProductionRuntimeBuildError)?,
            stream_deadline: Duration::from_millis(limits.output_stream_deadline_millis),
            ack_timeout: Duration::from_millis(limits.output_ack_timeout_millis),
            workload_session_id: deployment.workload_session_id.clone(),
            producer_id: deployment.producer_id.clone(),
        };
        let spool_overhead = limits
            .output_max_queued_frames
            .checked_mul(64)
            .and_then(|value| u64::try_from(value).ok())
            .ok_or(ProductionRuntimeBuildError)?;
        let max_encrypted_bytes = u64::try_from(limits.output_max_queued_bytes)
            .ok()
            .and_then(|value| value.checked_add(spool_overhead))
            .ok_or(ProductionRuntimeBuildError)?;
        let output_preflight = AgentOutputPreflight::new(
            spool_root,
            spool_master_key,
            SpoolLimits {
                max_frames: limits.output_max_queued_frames,
                max_encrypted_bytes,
                max_frame_bytes: limits.output_max_frame_bytes(),
            },
            output_config,
        );
        let retirer = Arc::new(
            CommandRetirer::new(
                Arc::clone(&command_bus),
                CommandRetirementConfig {
                    stream: deployment.nats_stream.clone(),
                    consumer: deployment.nats_consumer.clone(),
                },
            )
            .map_err(|_| ProductionRuntimeBuildError)?,
        );
        let admission = InvocationAdmission::new(
            InvocationAdmissionConfig::new(
                limits.delivery_max_concurrency,
                Duration::from_millis(limits.admission_timeout_millis),
            )
            .map_err(|_| ProductionRuntimeBuildError)?,
        );
        let preparation =
            AgentPreparationConfig::new(Duration::from_millis(limits.lease_poll_interval_millis))
                .map_err(|_| ProductionRuntimeBuildError)?;
        let terminal_recovery = AgentTerminalRecoveryConfig::new(limits.output_max_sessions)
            .map_err(|_| ProductionRuntimeBuildError)?;
        // The Office families (ADR-0032 P3) reach the platform object API at
        // `platform_origin`, and only it, presenting the worker's runtime
        // client certificate (elitea-main's platform listener requires one).
        let office_runtime = crate::transport::office_host::OfficeRuntime::new(
            &deployment.platform_origin,
            &platform_identity,
        )
        .map(|runtime| {
            Arc::new(
                runtime
                    .with_public_base_url(deployment.public_base_url.clone())
                    .with_extractor(extractor.clone()),
            )
        })
        .ok();
        if office_runtime.is_none() {
            tracing::warn!(
                event = "office_runtime_unavailable",
                "the Office families are left out: the platform origin is unusable"
            );
        }
        // The internal MCP builders live on the platform listener too
        // (`<platform_origin>/app/{project_id}/mcp/...`): an MCP endpoint on
        // that exact origin presents the same client certificate, and no
        // other endpoint does.
        let platform_mcp = crate::toolkits::PlatformMcpIdentity::new(
            &deployment.platform_origin,
            platform_identity.ca_pem(),
            platform_identity.identity_pem(),
        )
        .map(Arc::new)
        .ok();
        if platform_mcp.is_none() {
            tracing::warn!(
                event = "platform_mcp_identity_unavailable",
                "internal MCP tools cannot reach the platform listener: its client identity is unusable"
            );
        }
        let mcp_connector: Arc<dyn crate::toolkits::McpConnector> = Arc::new(
            crate::toolkits::AdkHttpMcpConnector::new().with_platform_identity(platform_mcp),
        );
        let assembler = Arc::new(
            NativeRuntimeAssembler::postgres(
                Arc::clone(&platform),
                model_facade,
                tool_policy,
                agentstate,
                SessionLimits::default(),
                CheckpointLimits::default(),
            )
            .with_sandbox(sandbox)
            .with_index_runtime(index_runtime.clone())
            .with_toolkit_model_call_cap(
                deployment
                    .toolkit_model_call_cap
                    .unwrap_or(crate::transport::model_text::DEFAULT_TOOLKIT_MODEL_CALLS),
            )
            .with_office_runtime(office_runtime.clone())
            .with_workspace_runs(workspace_runs)
            .with_mcp_connector(Arc::clone(&mcp_connector)),
        );
        let clock = Arc::new(SystemUnixMillisClock);
        let agent: ProductionAgentProcessor = native_agent_delivery_processor(
            Arc::clone(&command_authenticator),
            output_preflight.clone(),
            Arc::clone(&control),
            Arc::clone(&retirer),
            Arc::clone(&input),
            Arc::clone(&clock),
            admission.clone(),
            preparation,
            assembler,
            output.clone(),
            limits.output_max_sessions,
            terminal_recovery,
        )
        .with_checkpoint_recovery(deployment.agent_model_checkpoint_recovery)
        .with_node_recovery(deployment.agent_node_recovery);
        let toolkit = ToolkitDeliveryProcessor::new(
            output_preflight,
            control,
            retirer,
            Arc::new(output),
            input,
            clock,
            admission,
            preparation,
            terminal_recovery,
            DirectToolkitRuntime::new(mcp_connector),
            platform,
            index_runtime,
        )
        .with_office(office_runtime);
        let processor = Arc::new(ExecutionDeliveryProcessor::new(
            command_authenticator,
            agent,
            toolkit,
        ));
        let intake = CommandDeliveryIntakeConfig::new(
            limits.delivery_max_concurrency,
            limits.delivery_queue_capacity,
            limits.nats_fetch_batch,
            limits.nats_fetch_expires_millis,
            limits.nats_in_progress_interval_millis,
            limits.nats_retry_delay_millis,
        )
        .map_err(|_| ProductionRuntimeBuildError)?;
        let runtime_config = CommandDeliveryRuntimeConfig::new(
            intake,
            limits.dependency_retry_millis,
            limits.shutdown_timeout_millis,
        )
        .map_err(|_| ProductionRuntimeBuildError)?;
        let delivery =
            CommandDeliveryRuntime::new(command_bus, Arc::clone(&processor), runtime_config);
        Ok(Self {
            delivery,
            processor,
        })
    }

    /// Run intake until Stop, drain all owned processing, then stop and
    /// await the native invocation supervisor. The future remains one-shot.
    pub(crate) async fn run(
        self,
        stop: watch::Receiver<bool>,
    ) -> Result<(), ProductionAgentRuntimeError> {
        let Self {
            delivery,
            processor,
        } = self;
        let delivery_result = delivery.run(stop).await;
        let stop_result = processor.stop();
        let close_result = processor.close().await;
        if let Err(error) = delivery_result {
            return Err(ProductionAgentRuntimeError {
                code: error.code(),
                retryable: error.retryable(),
            });
        }
        if stop_result.is_err() || close_result.is_err() {
            return Err(ProductionAgentRuntimeError {
                code: "agent_runtime.drain_failed",
                retryable: true,
            });
        }
        Ok(())
    }
}

/// Stable, data-free runtime exit classification.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct ProductionAgentRuntimeError {
    code: &'static str,
    retryable: bool,
}

impl ProductionAgentRuntimeError {
    #[must_use]
    pub(crate) const fn code(self) -> &'static str {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(self) -> bool {
        self.retryable
    }
}

impl fmt::Display for ProductionAgentRuntimeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the production agent runtime stopped before a complete drain")
    }
}

impl std::error::Error for ProductionAgentRuntimeError {}

const fn map_config_error(error: &RuntimeConfigError) -> ProductionServeError {
    match error {
        RuntimeConfigError::InvalidConfiguration(_) => ProductionServeError {
            code: "worker_serve.invalid_configuration",
            retryable: false,
            message: "the worker serve configuration is invalid",
        },
        RuntimeConfigError::ResourceExhausted(_) => ProductionServeError {
            code: "worker_serve.resource_exhausted",
            retryable: false,
            message: "the worker serve configuration exceeds an approved limit",
        },
        RuntimeConfigError::Unavailable { .. } => ProductionServeError {
            code: "worker_serve.configuration_unavailable",
            retryable: true,
            message: "the worker serve configuration is unavailable",
        },
    }
}

const fn map_policy_error(error: &ToolAdmissionPolicyError) -> ProductionServeError {
    match error.code() {
        ToolAdmissionPolicyErrorCode::InvalidConfiguration => invalid_policy(),
        ToolAdmissionPolicyErrorCode::ResourceExhausted => ProductionServeError {
            code: "worker_serve.toolkit_policy_exhausted",
            retryable: false,
            message: "the toolkit security snapshot exceeds an approved limit",
        },
    }
}

const fn map_bootstrap_error(
    error: crate::bootstrap::ProductionBootstrapError,
) -> ProductionServeError {
    ProductionServeError {
        code: error.code(),
        retryable: error.retryable(),
        message: "the worker production dependencies could not be admitted",
    }
}

const fn map_runtime_error(error: ProductionAgentRuntimeError) -> ProductionServeError {
    ProductionServeError {
        code: error.code(),
        retryable: error.retryable(),
        message: "the production agent runtime stopped before a complete drain",
    }
}

const fn invalid_policy() -> ProductionServeError {
    ProductionServeError {
        code: "worker_serve.toolkit_policy_invalid",
        retryable: false,
        message: "the toolkit security snapshot is invalid",
    }
}

const fn invalid_runtime() -> ProductionServeError {
    ProductionServeError {
        code: "worker_serve.runtime_invalid",
        retryable: false,
        message: "the production agent runtime configuration is invalid",
    }
}

const fn signal_unavailable() -> ProductionServeError {
    ProductionServeError {
        code: "worker_serve.signal_unavailable",
        retryable: false,
        message: "the worker shutdown signal owner is unavailable",
    }
}

const fn office_core_stuck_error() -> ProductionServeError {
    ProductionServeError {
        code: "worker_serve.office_core_stuck",
        retryable: true,
        message: "a document-library call did not finish; the worker drained and exited to be restarted",
    }
}

const fn shutdown_timeout_error() -> ProductionServeError {
    ProductionServeError {
        code: "worker_serve.shutdown_timeout",
        retryable: true,
        message: "the worker did not drain before its shutdown deadline",
    }
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::PermissionsExt as _;

    use serde_json::json;
    use tempfile::tempdir;

    use super::{
        ProductionAgentRuntimeError, ProductionRuntimeBuildError, ProductionServeError, drive,
        load_tool_policy, office_core_stuck, shutdown_timeout_error,
    };
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use std::time::Duration;
    use tokio::sync::watch;

    #[test]
    fn production_runtime_failures_are_data_free() {
        assert_eq!(
            ProductionRuntimeBuildError.to_string(),
            "the production agent runtime configuration is invalid"
        );
        let runtime = ProductionAgentRuntimeError {
            code: "command_delivery.drain_timeout",
            retryable: true,
        };
        assert_eq!(runtime.code(), "command_delivery.drain_timeout");
        assert!(runtime.retryable());
        assert!(!runtime.to_string().contains("command_delivery"));
        let shutdown = shutdown_timeout_error();
        assert_eq!(shutdown.code(), "worker_serve.shutdown_timeout");
        assert!(shutdown.retryable());
    }

    #[test]
    fn production_policy_snapshot_is_explicit_strict_and_data_free() {
        let root = tempdir().expect("temporary policy directory");
        let root_path = root
            .path()
            .canonicalize()
            .expect("canonical temporary policy directory");
        let path = root_path.join("toolkit-security.json");
        let cases = [
            (
                json!({"toolkit_security": {}}),
                true,
                "explicit empty policy is an authoritative generation",
            ),
            (
                json!({}),
                false,
                "a missing dictionary must not become an empty policy",
            ),
            (
                json!({"toolkit_security": {"sensitve_tools": {"github": ["secret-value"]}}}),
                false,
                "a misspelled security field must fail closed",
            ),
        ];
        for (value, accepted, description) in cases {
            fs::write(&path, serde_json::to_vec(&value).expect("policy JSON"))
                .expect("write policy");
            fs::set_permissions(&path, fs::Permissions::from_mode(0o600))
                .expect("policy permissions");
            let result = load_tool_policy(&path);
            assert_eq!(result.is_ok(), accepted, "{description}");
            if let Err(error) = result {
                assert!(!format!("{error:?} {error}").contains("secret-value"));
            }
        }
    }

    /// ADR-0032 decision 3: a stuck Office engine stops intake, lets the
    /// in-flight work drain and ends the process with an error.
    #[tokio::test(start_paused = true)]
    async fn a_stuck_office_engine_drains_the_worker_and_exits_with_an_error() {
        let (stop, mut stopped) = watch::channel(false);
        let drained = Arc::new(AtomicBool::new(false));
        let flag = Arc::clone(&drained);
        // Intake that runs until told to stop, then takes a moment to drain.
        let running = Box::pin(async move {
            while !*stopped.borrow() {
                if stopped.changed().await.is_err() {
                    break;
                }
            }
            tokio::time::sleep(Duration::from_secs(3)).await;
            flag.store(true, Ordering::SeqCst);
            Ok::<(), ProductionAgentRuntimeError>(())
        });
        let polls = Arc::new(AtomicUsize::new(0));
        let seen = Arc::clone(&polls);
        // The engine reports healthy twice, then stuck.
        let stuck = office_core_stuck(Duration::from_secs(5), move || {
            seen.fetch_add(1, Ordering::SeqCst) >= 2
        });
        let never = std::future::pending::<Result<(), ProductionServeError>>();
        let result = drive(running, &stop, never, stuck, Duration::from_secs(30)).await;
        let error = result.expect_err("a stuck engine ends the process with an error");
        assert_eq!(error.code(), "worker_serve.office_core_stuck");
        assert!(error.retryable());
        assert_eq!(polls.load(Ordering::SeqCst), 3, "polled until stuck");
        assert!(*stop.borrow(), "intake was told to stop claiming");
        assert!(
            drained.load(Ordering::SeqCst),
            "in-flight work was allowed to drain"
        );
    }

    /// A drain that never finishes does not hold a stuck worker up: the exit
    /// comes at the shutdown deadline.
    #[tokio::test(start_paused = true)]
    async fn a_stuck_engine_does_not_wait_longer_than_the_shutdown_deadline() {
        let (stop, _stopped) = watch::channel(false);
        let running = Box::pin(std::future::pending::<
            Result<(), ProductionAgentRuntimeError>,
        >());
        let started = tokio::time::Instant::now();
        let result = drive(
            running,
            &stop,
            std::future::pending::<Result<(), ProductionServeError>>(),
            std::future::ready(()),
            Duration::from_secs(30),
        )
        .await;
        assert_eq!(
            result.expect_err("an error").code(),
            "worker_serve.office_core_stuck"
        );
        assert_eq!(started.elapsed(), Duration::from_secs(30));
    }

    /// A drain closes the worker-wide analysis gate, so a queued analysis is
    /// told the worker is shutting down instead of waiting for a slot.
    #[tokio::test(start_paused = true)]
    async fn a_stop_request_closes_the_analysis_gate() {
        use elitea_agent_runtime::toolkits::AnalysisGate;
        let (stop, mut stopped) = watch::channel(false);
        let running = Box::pin(async move {
            let _ = stopped.changed().await;
            Ok::<(), ProductionAgentRuntimeError>(())
        });
        let result = drive(
            running,
            &stop,
            std::future::ready(Ok(())),
            std::future::pending::<()>(),
            Duration::from_secs(30),
        )
        .await;
        assert!(result.is_ok());
        assert!(AnalysisGate::process().is_closed());
    }

    /// Without a stuck engine, a signal still drains and returns the drain's
    /// own result, and a healthy engine never ends the worker.
    #[tokio::test(start_paused = true)]
    async fn a_signal_drains_as_before_and_a_healthy_engine_never_stops_the_worker() {
        let (stop, mut stopped) = watch::channel(false);
        let running = Box::pin(async move {
            let _ = stopped.changed().await;
            Ok::<(), ProductionAgentRuntimeError>(())
        });
        let healthy = office_core_stuck(Duration::from_secs(5), || false);
        let result = drive(
            running,
            &stop,
            async {
                tokio::time::sleep(Duration::from_mins(1)).await;
                Ok::<(), ProductionServeError>(())
            },
            healthy,
            Duration::from_secs(30),
        )
        .await;
        assert!(result.is_ok(), "{result:?}");
        assert!(*stop.borrow());
    }

    /// The probe the worker polls is the engine's own: a permit held for
    /// more than three deadlines (a library call that will not return its
    /// thread) trips it, and the same `drive` ends the process.
    #[tokio::test]
    async fn the_real_engine_probe_trips_on_a_call_that_never_gives_its_thread_back() {
        let limits = elitea_office_core::Limits::default();
        assert!(
            !elitea_office_core::health().stuck(),
            "healthy at the start"
        );
        let held = elitea_office_core::testing::hold_permit(
            &limits,
            Duration::from_millis(600),
            Duration::from_millis(10),
        )
        .expect("a free permit");
        let (stop, _stopped) = watch::channel(false);
        let result = drive(
            Box::pin(std::future::pending::<
                Result<(), ProductionAgentRuntimeError>,
            >()),
            &stop,
            std::future::pending::<Result<(), ProductionServeError>>(),
            office_core_stuck(Duration::from_millis(5), || {
                elitea_office_core::health().stuck()
            }),
            Duration::from_millis(50),
        )
        .await;
        assert_eq!(
            result.expect_err("stuck").code(),
            "worker_serve.office_core_stuck"
        );
        assert!(*stop.borrow());
        held.join().expect("the caller of the held call returns");
        // The library thread keeps its permit until it exits; then it is healthy.
        let mut waited = 0;
        while elitea_office_core::health().stuck() && waited < 100 {
            tokio::time::sleep(Duration::from_millis(50)).await;
            waited += 1;
        }
        assert!(
            !elitea_office_core::health().stuck(),
            "the permit was released"
        );
    }
}
