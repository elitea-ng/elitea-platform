//! Capability-disabled production dependency composition.
//!
//! This module constructs the exact private-plane transports and shared
//! `agentstate` pool used by the agent runtime. It does not start Redis intake
//! or register a capability: the caller must still supply an authoritative
//! frozen toolkit-security policy and own process-wide stop/drain ordering.

#![allow(dead_code)] // Activated only after the remaining production gates close.

use std::fmt;
use std::path::PathBuf;
use std::str::FromStr as _;
use std::sync::Arc;
use std::time::Duration;

use sqlx::PgPool;
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};
use zeroize::Zeroizing;

use crate::config::{
    RuntimeConfigError, RuntimeDeployConfig, read_regular_file, validate_private_directory,
};
use crate::protocol::command::SignedCommandAuthenticator;
use crate::protocol::control::{AgentControlClient, AgentControlError};
use crate::security::{RuntimeTrustError, RuntimeTrustMaterial};
use crate::spool::SpoolMasterKey;
use crate::transport::input_content::InputContentConfig;
use crate::transport::redis_connector::ProductionRedisConnector;
use crate::transport::redis_generation::RedisStreamsHandle;
use crate::transport::redis_streams::{RedisStreamsError, RedisStreamsErrorKind};
use crate::transport::runtime_context::{
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextError,
};
use crate::transport::{
    ControlGrpcConfig, ControlGrpcError, InputContentClient, InputContentError, TonicControlRpc,
};
use crate::transport::{model_facade::ModelFacade, openai_compatible_facade::ModelGatewayConfig};
use crate::transport::{
    openai_compatible_facade::ModelFacadeError, platform_client::PlatformClient,
};

const MAX_AGENTSTATE_CONNECTION_BYTES: usize = 16 * 1024;
const MAX_RUNTIME_CONTEXT_BYTES: usize = 32 * 1024;
const MAX_APPLICATION_VERSION_BYTES: usize = 1024 * 1024;
// The attachment ENVELOPE, not the object: main caps the file at 128 KiB and
// its JSON envelope at 1 MiB, because the content travels as a JSON string.
const MAX_ATTACHMENT_OBJECT_BYTES: usize = 1024 * 1024;
const MAX_MODEL_REQUEST_BYTES: usize = 1024 * 1024;
const MAX_MODEL_SSE_EVENT_BYTES: usize = 256 * 1024;
const MAX_MODEL_STREAM_BYTES: usize = 8 * 1024 * 1024;
const MAX_MODEL_SSE_EVENTS: usize = 4_096;

/// Stable, redacted startup failure before capability registration.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProductionBootstrapError {
    InvalidConfiguration,
    ResourceExhausted,
    AuthenticationFailed,
    DependencyUnavailable,
}

impl ProductionBootstrapError {
    #[must_use]
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "worker_bootstrap.invalid_configuration",
            Self::ResourceExhausted => "worker_bootstrap.resource_exhausted",
            Self::AuthenticationFailed => "worker_bootstrap.authentication_failed",
            Self::DependencyUnavailable => "worker_bootstrap.dependency_unavailable",
        }
    }

    #[must_use]
    pub(crate) const fn retryable(self) -> bool {
        matches!(self, Self::DependencyUnavailable)
    }
}

impl fmt::Display for ProductionBootstrapError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::InvalidConfiguration => "the worker deployment configuration is invalid",
            Self::ResourceExhausted => "the worker deployment exceeds an approved limit",
            Self::AuthenticationFailed => "the worker dependency rejected its identity",
            Self::DependencyUnavailable => "a worker dependency is unavailable",
        })
    }
}

impl std::error::Error for ProductionBootstrapError {}

/// One connected dependency generation. No field contains a raw claim, fence
/// or execution actor credential.
pub(crate) struct ProductionTransportBundle {
    pub(crate) deployment: Arc<RuntimeDeployConfig>,
    pub(crate) command_authenticator: Arc<dyn SignedCommandAuthenticator>,
    pub(crate) spool_root: PathBuf,
    pub(crate) spool_master_key: SpoolMasterKey,
    pub(crate) redis: Arc<RedisStreamsHandle<ProductionRedisConnector>>,
    pub(crate) control: Arc<AgentControlClient<TonicControlRpc>>,
    pub(crate) output: Channel,
    pub(crate) input: Arc<InputContentClient>,
    pub(crate) platform: Arc<PlatformClient>,
    pub(crate) model_facade: Arc<ModelFacade>,
    pub(crate) agentstate: PgPool,
}

impl ProductionTransportBundle {
    /// Load trust, validate local state ownership and connect every required
    /// private-plane dependency without starting command intake.
    pub(crate) async fn connect(
        deployment: Arc<RuntimeDeployConfig>,
    ) -> Result<Self, ProductionBootstrapError> {
        let trust = RuntimeTrustMaterial::load(&deployment).map_err(map_trust_error)?;
        let spool_root = validate_private_directory(&deployment.spool_root, "output spool root")
            .map_err(|error| map_config_error(&error))?;
        let agentstate_options = load_agentstate_options(&deployment)?;
        let profiles = ProductionProfiles::from_deployment(&deployment);
        let redis = ProductionRedisConnector::from_deployment(Arc::clone(&deployment))
            .map_err(|error| map_redis_error(&error))?
            .handle();

        let control = connect_private_grpc(
            &deployment.control_target,
            profiles.grpc_connect_timeout,
            trust.private_ca(),
            trust.client_identity(),
        );
        let output = connect_private_grpc(
            &deployment.output_target,
            profiles.grpc_connect_timeout,
            trust.private_ca(),
            trust.client_identity(),
        );
        let input = InputContentClient::connect(
            profiles.input,
            trust.private_ca(),
            trust.client_identity(),
        );
        let runtime_context = RuntimeContextClient::connect(
            profiles.runtime_context,
            trust.private_ca(),
            trust.client_identity(),
        );
        let model_facade =
            ModelFacade::connect(profiles.model, trust.private_ca(), trust.client_identity());
        let agentstate = connect_agentstate(
            agentstate_options,
            deployment.limits.delivery_max_concurrency,
            profiles.grpc_connect_timeout,
        );
        let redis_connect = redis.connect();

        let (control, output, input, runtime_context, model_facade, agentstate, _redis_generation) =
            tokio::try_join!(
                control,
                output,
                async { input.await.map_err(|error| map_input_error(&error)) },
                async {
                    runtime_context
                        .await
                        .map_err(|error| map_runtime_context_error(&error))
                },
                async { model_facade.await.map_err(map_model_error) },
                agentstate,
                async { redis_connect.await.map_err(|error| map_redis_error(&error)) },
            )?;

        let control = AgentControlClient::from_channel(control, profiles.control)
            .map_err(|error| map_agent_control_error(&error))?;
        let platform = Arc::new(PlatformClient::new(Arc::new(runtime_context)));
        Ok(Self {
            command_authenticator: trust.command_authenticator(),
            spool_master_key: trust.spool_master_key(),
            deployment,
            spool_root,
            redis,
            control: Arc::new(control),
            output,
            input: Arc::new(input),
            platform,
            model_facade: Arc::new(model_facade),
            agentstate,
        })
    }
}

struct ProductionProfiles {
    grpc_connect_timeout: Duration,
    control: ControlGrpcConfig,
    input: InputContentConfig,
    runtime_context: RuntimeContextConfig,
    model: ModelGatewayConfig,
}

impl ProductionProfiles {
    fn from_deployment(deployment: &RuntimeDeployConfig) -> Self {
        let limits = deployment.limits;
        let grpc_connect_timeout = Duration::from_millis(limits.grpc_deadline_millis);
        Self {
            grpc_connect_timeout,
            control: ControlGrpcConfig {
                deadline: grpc_connect_timeout,
                workload_session_id: deployment.workload_session_id.clone(),
                producer_id: deployment.producer_id.clone(),
            },
            input: InputContentConfig {
                origin: deployment.content_origin.clone(),
                deadline: Duration::from_millis(limits.content_timeout_millis),
                max_materialized_bytes: limits.content_max_body_bytes(),
            },
            runtime_context: RuntimeContextConfig {
                origin: deployment.content_origin.clone(),
                deadline: Duration::from_millis(limits.content_timeout_millis),
                max_response_bytes: MAX_RUNTIME_CONTEXT_BYTES,
                max_application_response_bytes: MAX_APPLICATION_VERSION_BYTES,
                max_attachment_response_bytes: MAX_ATTACHMENT_OBJECT_BYTES,
            },
            model: ModelGatewayConfig {
                origin: deployment.platform_origin.clone(),
                connect_timeout: grpc_connect_timeout,
                response_header_timeout: Duration::from_millis(limits.content_timeout_millis),
                stream_idle_timeout: Duration::from_millis(limits.content_timeout_millis),
                max_request_bytes: MAX_MODEL_REQUEST_BYTES,
                max_sse_event_bytes: MAX_MODEL_SSE_EVENT_BYTES,
                max_stream_bytes: MAX_MODEL_STREAM_BYTES,
                max_sse_events: MAX_MODEL_SSE_EVENTS,
            },
        }
    }
}

async fn connect_private_grpc(
    target: &str,
    deadline: Duration,
    private_ca: tonic::transport::Certificate,
    client_identity: tonic::transport::Identity,
) -> Result<Channel, ProductionBootstrapError> {
    let endpoint = Endpoint::from_shared(format!("https://{target}"))
        .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?
        .tls_config(
            ClientTlsConfig::new()
                .ca_certificate(private_ca)
                .identity(client_identity),
        )
        .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?
        .connect_timeout(deadline)
        .tcp_nodelay(true);
    tokio::time::timeout(deadline, endpoint.connect())
        .await
        .map_err(|_| ProductionBootstrapError::DependencyUnavailable)?
        .map_err(|_| ProductionBootstrapError::DependencyUnavailable)
}

fn load_agentstate_options(
    deployment: &RuntimeDeployConfig,
) -> Result<PgConnectOptions, ProductionBootstrapError> {
    let path = deployment
        .agent_checkpoint_connection_path
        .as_ref()
        .ok_or(ProductionBootstrapError::InvalidConfiguration)?;
    let mut raw = Zeroizing::new(
        read_regular_file(
            path,
            MAX_AGENTSTATE_CONNECTION_BYTES,
            true,
            "agentstate connection",
        )
        .map_err(|error| map_config_error(&error))?,
    );
    if raw.ends_with(b"\n") {
        raw.pop();
        if raw.ends_with(b"\r") {
            raw.pop();
        }
    }
    if raw.is_empty() || raw.iter().any(|byte| matches!(byte, b'\r' | b'\n' | b'\0')) {
        return Err(ProductionBootstrapError::InvalidConfiguration);
    }
    let connection = Zeroizing::new(
        String::from_utf8(raw.to_vec())
            .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?,
    );
    PgConnectOptions::from_str(connection.as_str())
        .map(sqlx::ConnectOptions::disable_statement_logging)
        .map_err(|_| ProductionBootstrapError::InvalidConfiguration)
}

async fn connect_agentstate(
    options: PgConnectOptions,
    max_connections: usize,
    deadline: Duration,
) -> Result<PgPool, ProductionBootstrapError> {
    let max_connections = u32::try_from(max_connections)
        .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?;
    PgPoolOptions::new()
        .max_connections(max_connections)
        .acquire_timeout(deadline)
        .connect_with(options)
        .await
        .map_err(|_| ProductionBootstrapError::DependencyUnavailable)
}

const fn map_config_error(error: &RuntimeConfigError) -> ProductionBootstrapError {
    match error {
        RuntimeConfigError::InvalidConfiguration(_) => {
            ProductionBootstrapError::InvalidConfiguration
        }
        RuntimeConfigError::ResourceExhausted(_) => ProductionBootstrapError::ResourceExhausted,
        RuntimeConfigError::Unavailable { .. } => ProductionBootstrapError::DependencyUnavailable,
    }
}

fn map_trust_error(error: RuntimeTrustError) -> ProductionBootstrapError {
    match error {
        RuntimeTrustError::Material(error) => map_config_error(&error),
        RuntimeTrustError::InvalidTlsIdentity
        | RuntimeTrustError::InvalidSpoolKey
        | RuntimeTrustError::InvalidRedisPassword
        | RuntimeTrustError::InvalidSigningKeyring
        | RuntimeTrustError::InvalidRedisTls => ProductionBootstrapError::InvalidConfiguration,
    }
}

fn map_redis_error(error: &RedisStreamsError) -> ProductionBootstrapError {
    match error.kind() {
        RedisStreamsErrorKind::Authentication => ProductionBootstrapError::AuthenticationFailed,
        RedisStreamsErrorKind::DependencyUnavailable | RedisStreamsErrorKind::Timeout => {
            ProductionBootstrapError::DependencyUnavailable
        }
        RedisStreamsErrorKind::ResourceExhausted => ProductionBootstrapError::ResourceExhausted,
        RedisStreamsErrorKind::Configuration
        | RedisStreamsErrorKind::Protocol
        | RedisStreamsErrorKind::Closed => ProductionBootstrapError::InvalidConfiguration,
    }
}

fn map_control_error(error: ControlGrpcError) -> ProductionBootstrapError {
    match error {
        ControlGrpcError::InvalidConfiguration(_) => ProductionBootstrapError::InvalidConfiguration,
        ControlGrpcError::ResourceExhausted(_) => ProductionBootstrapError::ResourceExhausted,
        ControlGrpcError::Unavailable(_) => ProductionBootstrapError::DependencyUnavailable,
    }
}

fn map_agent_control_error(error: &AgentControlError) -> ProductionBootstrapError {
    match error {
        AgentControlError::Transport(error) => map_control_error(*error),
        AgentControlError::Semantic(_) if error.retryable() => {
            ProductionBootstrapError::DependencyUnavailable
        }
        AgentControlError::Semantic(_) => ProductionBootstrapError::InvalidConfiguration,
    }
}

const fn map_input_error(error: &InputContentError) -> ProductionBootstrapError {
    match error {
        InputContentError::InvalidConfiguration(_) | InputContentError::InvalidInput(_) => {
            ProductionBootstrapError::InvalidConfiguration
        }
        InputContentError::ResourceExhausted(_) => ProductionBootstrapError::ResourceExhausted,
        InputContentError::AuthorizationFailed(_) => ProductionBootstrapError::AuthenticationFailed,
        InputContentError::DependencyUnavailable(_)
        | InputContentError::Transport(_)
        | InputContentError::Timeout(_) => ProductionBootstrapError::DependencyUnavailable,
    }
}

fn map_runtime_context_error(error: &RuntimeContextError) -> ProductionBootstrapError {
    match error {
        RuntimeContextError::InvalidConfiguration(_) | RuntimeContextError::InvalidResponse(_) => {
            ProductionBootstrapError::InvalidConfiguration
        }
        RuntimeContextError::ResourceExhausted(_) => ProductionBootstrapError::ResourceExhausted,
        RuntimeContextError::AuthorizationFailed(_) => {
            ProductionBootstrapError::AuthenticationFailed
        }
        // A resource the claim was allowed to read but that no longer exists
        // is a stale reference, not a transient dependency failure: it must
        // not re-enter the retrying bucket.
        RuntimeContextError::NotFound(_) => ProductionBootstrapError::InvalidConfiguration,
        RuntimeContextError::DependencyUnavailable(_)
        | RuntimeContextError::Transport(_)
        | RuntimeContextError::Timeout(_) => ProductionBootstrapError::DependencyUnavailable,
    }
}

fn map_model_error(error: ModelFacadeError) -> ProductionBootstrapError {
    match error {
        ModelFacadeError::InvalidConfiguration | ModelFacadeError::InvalidInvocation => {
            ProductionBootstrapError::InvalidConfiguration
        }
        ModelFacadeError::ResourceExhausted => ProductionBootstrapError::ResourceExhausted,
        ModelFacadeError::DependencyUnavailable => ProductionBootstrapError::DependencyUnavailable,
    }
}

#[cfg(test)]
mod tests {
    use super::{ProductionBootstrapError, map_config_error, map_redis_error};
    use crate::config::RuntimeConfigError;
    use crate::transport::redis_streams::RedisStreamsError;

    #[test]
    fn startup_errors_are_low_cardinality_and_retry_only_dependency_availability() {
        let cases = [
            ProductionBootstrapError::InvalidConfiguration,
            ProductionBootstrapError::ResourceExhausted,
            ProductionBootstrapError::AuthenticationFailed,
            ProductionBootstrapError::DependencyUnavailable,
        ];
        for error in cases {
            assert!(!error.code().contains(' '));
            assert_eq!(
                error.retryable(),
                error == ProductionBootstrapError::DependencyUnavailable
            );
        }
    }

    #[test]
    fn local_and_redis_failures_preserve_only_safe_startup_categories() {
        assert_eq!(
            map_config_error(&RuntimeConfigError::ResourceExhausted("secret path")),
            ProductionBootstrapError::ResourceExhausted
        );
        assert_eq!(
            map_redis_error(&RedisStreamsError::authentication("provider text")),
            ProductionBootstrapError::AuthenticationFailed
        );
        assert_eq!(
            map_redis_error(&RedisStreamsError::unavailable("provider text")),
            ProductionBootstrapError::DependencyUnavailable
        );
    }
}
