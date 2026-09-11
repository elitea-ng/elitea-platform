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
use super::execution_delivery_processor::ExecutionDeliveryProcessor;
use super::invocation_admission::{InvocationAdmission, InvocationAdmissionConfig};
use super::native_agent_lifecycle::NativeAuthorizedAgentLifecycle;
use super::output_delivery::{AgentOutputPreflight, AgentTerminalRecoveryConfig};
use super::redis_delivery::{
    RedisDeliveryIntakeConfig, RedisDeliveryRuntime, RedisDeliveryRuntimeConfig,
};
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
use crate::transport::redis_commands::{RedisCommandRetirer, RedisRetirementConfig};
use crate::transport::redis_connector::ProductionRedisConnector;
use crate::transport::redis_generation::RedisStreamsHandle;
use crate::transport::{OutputGrpcConfig, TonicControlRpc};

type ProductionAssembler =
    NativeRuntimeAssembler<OrdinaryNativeAgentAssembler, PipelineNativeAgentAssembler>;
type ProductionRedis = Arc<RedisStreamsHandle<ProductionRedisConnector>>;
type ProductionLifecycle = NativeAuthorizedAgentLifecycle<
    ProductionAssembler,
    Channel,
    TonicControlRpc,
    ProductionRedis,
    SystemUnixMillisClock,
>;
type ProductionAgentProcessor = AgentDeliveryProcessor<
    TonicControlRpc,
    ProductionRedis,
    Channel,
    SystemUnixMillisClock,
    ProductionLifecycle,
    crate::transport::InputContentClient,
>;
type ProductionProcessor = ExecutionDeliveryProcessor<
    TonicControlRpc,
    ProductionRedis,
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
    let mut running = Box::pin(runtime.run(stopped));
    tracing::info!(event = "production_intake_started");

    tokio::select! {
        result = &mut running => result.map_err(map_runtime_error),
        requested = signals.receive() => {
            requested?;
            tracing::info!(event = "production_stop_requested");
            if stop.send(true).is_err() {
                return running.await.map_err(map_runtime_error);
            }
            match tokio::time::timeout(shutdown_timeout, running).await {
                Ok(result) => result.map_err(map_runtime_error),
                Err(_) => Err(shutdown_timeout_error()),
            }
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

/// Stable composition failure before Redis intake or command authority exists.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct ProductionRuntimeBuildError;

impl fmt::Display for ProductionRuntimeBuildError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the production agent runtime configuration is invalid")
    }
}

impl std::error::Error for ProductionRuntimeBuildError {}

/// One process-owned Redis delivery runtime plus its native invocation drain
/// owner. Construction requires the authoritative policy explicitly; there is
/// no default or missing-policy branch.
pub(crate) struct ProductionAgentRuntime {
    delivery: RedisDeliveryRuntime<ProductionRedisConnector, ProductionProcessor>,
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
            redis,
            control,
            output,
            input,
            platform,
            model_facade,
            agentstate,
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
            RedisCommandRetirer::new(
                Arc::clone(&redis),
                RedisRetirementConfig {
                    stream: deployment.redis_stream.clone(),
                    group: deployment.redis_group.clone(),
                    consumer: deployment.consumer_id.clone(),
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
        let assembler = Arc::new(NativeRuntimeAssembler::postgres(
            platform,
            model_facade,
            tool_policy,
            agentstate,
            SessionLimits::default(),
            CheckpointLimits::default(),
        ));
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
        );
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
            DirectToolkitRuntime::default(),
        );
        let processor = Arc::new(ExecutionDeliveryProcessor::new(
            command_authenticator,
            agent,
            toolkit,
        ));
        let intake = RedisDeliveryIntakeConfig::new(
            limits.delivery_max_concurrency,
            limits.delivery_queue_capacity,
            limits.redis_block_millis,
            limits.redis_reclaim_idle_millis,
            limits.redis_reclaim_interval_millis,
        )
        .map_err(|_| ProductionRuntimeBuildError)?;
        let runtime_config = RedisDeliveryRuntimeConfig::new(
            intake,
            limits.dependency_retry_millis,
            limits.shutdown_timeout_millis,
        )
        .map_err(|_| ProductionRuntimeBuildError)?;
        let delivery = RedisDeliveryRuntime::new(redis, Arc::clone(&processor), runtime_config);
        Ok(Self {
            delivery,
            processor,
        })
    }

    /// Run intake until Stop, drain all PEL-owned processing, then stop and
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
        ProductionAgentRuntimeError, ProductionRuntimeBuildError, load_tool_policy,
        shutdown_timeout_error,
    };

    #[test]
    fn production_runtime_failures_are_data_free() {
        assert_eq!(
            ProductionRuntimeBuildError.to_string(),
            "the production agent runtime configuration is invalid"
        );
        let runtime = ProductionAgentRuntimeError {
            code: "redis_delivery.drain_timeout",
            retryable: true,
        };
        assert_eq!(runtime.code(), "redis_delivery.drain_timeout");
        assert!(runtime.retryable());
        assert!(!runtime.to_string().contains("redis_delivery"));
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
}
