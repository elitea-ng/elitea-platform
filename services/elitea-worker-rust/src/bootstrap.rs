//! Capability-disabled production dependency composition.
//!
//! This module constructs the exact private-plane transports and shared
//! `agentstate` pool used by the agent runtime. It does not start command intake
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
use crate::transport::command_bus::CommandBusLimits;
use crate::transport::input_content::InputContentConfig;
use crate::transport::nats_jetstream::{
    NatsCommandBus, NatsJetStreamConfig, NatsJetStreamError, NatsJetStreamErrorKind, NatsTlsPaths,
};
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
// The attachment ENVELOPE, not the text: main serves at most 2 MiB of
// extracted text in an envelope of at most 6 MiB (see the transport's own
// constant, which this one restates so the two cannot drift).
const MAX_ATTACHMENT_OBJECT_BYTES: usize =
    crate::transport::runtime_context::MAX_ATTACHMENT_OBJECT_BYTES;
// The artifact ENVELOPE, likewise: main serves at most 200_000 CHARACTERS of
// file content and caps the envelope at 2 MiB, because a control-character-
// dense file escapes to six characters per byte inside a JSON string.
const MAX_ARTIFACT_OBJECT_BYTES: usize = 2 * 1024 * 1024;
const MAX_MODEL_REQUEST_BYTES: usize = 8 * 1024 * 1024;
const MAX_MODEL_SSE_EVENT_BYTES: usize = 256 * 1024;
const MAX_MODEL_STREAM_BYTES: usize = 8 * 1024 * 1024;
const MAX_MODEL_SSE_EVENTS: usize = 4_096;

/// Stable, redacted startup failure before capability registration.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ProductionBootstrapError {
    InvalidConfiguration,
    ResourceExhausted,
    AuthenticationFailed,
    ConsumerMissing,
    DependencyUnavailable,
}

impl ProductionBootstrapError {
    #[must_use]
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "worker_bootstrap.invalid_configuration",
            Self::ResourceExhausted => "worker_bootstrap.resource_exhausted",
            Self::AuthenticationFailed => "worker_bootstrap.authentication_failed",
            Self::ConsumerMissing => "worker_bootstrap.consumer_missing",
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
            Self::ConsumerMissing => {
                "the NATS command stream or durable requires the nats-bootstrap job"
            }
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
    pub(crate) command_bus: Arc<NatsCommandBus>,
    pub(crate) control: Arc<AgentControlClient<TonicControlRpc>>,
    pub(crate) output: Channel,
    pub(crate) input: Arc<InputContentClient>,
    pub(crate) platform: Arc<PlatformClient>,
    pub(crate) model_facade: Arc<ModelFacade>,
    pub(crate) agentstate: PgPool,
    pub(crate) sandbox: Option<Arc<crate::agents::graph::CodeRuntimeFactory>>,
}

impl ProductionTransportBundle {
    /// Load trust, validate local state ownership and connect every required
    /// private-plane dependency without starting command intake.
    #[expect(
        clippy::too_many_lines,
        reason = "Keep ordered trust loading and transport composition together"
    )]
    pub(crate) async fn connect(
        deployment: Arc<RuntimeDeployConfig>,
    ) -> Result<Self, ProductionBootstrapError> {
        let compiled_profile = crate::sandbox::compiled_profile_config::load_worker_profile(
            &deployment.sandbox_runtimes,
        )
        .map_err(|error| {
            tracing::error!(event = "compiled_snapshot_profile_invalid", reason = %error);
            ProductionBootstrapError::InvalidConfiguration
        })?;
        let platform_compiled_profiles = deployment.sandbox_runtimes.iter().map(|runtime| {
            let Some(platform) = &runtime.platform_client else { return Ok(None); };
            platform.compiled_snapshot.as_ref().map(|settings| settings.load(
                &platform.image_digest,
                &platform.policy_revision,
                runtime.preparation.as_ref().and_then(|p| p.native_platform.as_ref()),
            )).transpose()
        }).collect::<Result<Vec<_>, crate::sandbox::compiled_profile_config::CompiledProfileError>>()
            .map_err(|error| {
                tracing::error!(event = "code_platform_compiled_profile_invalid", reason = %error);
                ProductionBootstrapError::InvalidConfiguration
            })?;
        let trust = RuntimeTrustMaterial::load(&deployment).map_err(map_trust_error)?;
        let spool_root = validate_private_directory(&deployment.spool_root, "output spool root")
            .map_err(|error| map_config_error(&error))?;
        let agentstate_options = load_agentstate_options(&deployment)?;
        let profiles = ProductionProfiles::from_deployment(&deployment);
        let command_bus = NatsCommandBus::connect(nats_transport_config(&deployment)?);

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
        let (control, output, input, runtime_context, model_facade, agentstate, command_bus) = tokio::try_join!(
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
            async { command_bus.await.map_err(|error| map_nats_error(&error)) },
        )?;

        let control = AgentControlClient::from_channel(control, profiles.control)
            .map_err(|error| map_agent_control_error(&error))?;
        let control = Arc::new(control);
        let input = Arc::new(input);
        let runtime_context = Arc::new(runtime_context);
        if compiled_profile.is_some() {
            sqlx::query(
                "SELECT compiled_descriptor_json FROM elitea_runtime.sandbox_dispatches LIMIT 0",
            )
            .execute(&agentstate)
            .await
            .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?;
        }
        let mut sandbox_profiles = Vec::with_capacity(deployment.sandbox_runtimes.len());
        let mut platform_profiles = Vec::new();
        for (profile, compiled) in deployment
            .sandbox_runtimes
            .iter()
            .zip(platform_compiled_profiles)
        {
            let channel = connect_private_grpc(
                &profile.target,
                profiles.grpc_connect_timeout,
                trust.private_ca(),
                trust.client_identity(),
            )
            .await?;
            let client = crate::sandbox::client::SandboxClient::from_channel(
                channel,
                profile.audience.clone(),
                Duration::from_secs(u64::from(profile.timeout_seconds) + 60),
            )
            .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?;
            let preparation_client = if let Some(preparation) = &profile.preparation {
                let channel = connect_private_grpc(
                    &preparation.target,
                    profiles.grpc_connect_timeout,
                    trust.private_ca(),
                    trust.client_identity(),
                )
                .await?;
                Some(
                    crate::sandbox::client::SandboxClient::from_channel(
                        channel,
                        preparation.audience.clone(),
                        Duration::from_secs(u64::from(preparation.timeout_seconds) + 60),
                    )
                    .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?,
                )
            } else {
                None
            };
            if let Some(platform) = &profile.platform_client {
                let execution = platform.execution_config(profile);
                let channel = connect_private_grpc(
                    &execution.target,
                    profiles.grpc_connect_timeout,
                    trust.private_ca(),
                    trust.client_identity(),
                )
                .await?;
                let platform_client = crate::sandbox::client::SandboxClient::from_channel(
                    channel,
                    execution.audience.clone(),
                    Duration::from_secs(u64::from(execution.timeout_seconds) + 60),
                )
                .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?;
                let policy = platform
                    .policy()
                    .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?;
                platform_profiles.push((
                    execution,
                    platform_client,
                    preparation_client.clone(),
                    policy,
                    compiled,
                ));
            }
            sandbox_profiles.push((profile.clone(), client, preparation_client));
        }
        let sandbox = if sandbox_profiles.is_empty() {
            None
        } else {
            let mut factory = crate::agents::graph::CodeRuntimeFactory::new(
                control.clone(),
                sandbox_profiles,
                agentstate.clone(),
            )
            .with_code_intents(input.clone())
            .with_debug_artifacts(runtime_context.clone());
            for (config, client, preparation, policy, compiled) in platform_profiles {
                factory = factory
                    .with_platform_client(config, client, preparation, policy, compiled)
                    .map_err(|_| ProductionBootstrapError::InvalidConfiguration)?;
            }
            Some(Arc::new(if let Some(profile) = compiled_profile {
                factory.with_compiled_snapshots(profile)
            } else {
                factory
            }))
        };
        let platform = Arc::new(PlatformClient::new(runtime_context));
        Ok(Self {
            command_authenticator: trust.command_authenticator(),
            spool_master_key: trust.spool_master_key(),
            deployment,
            spool_root,
            command_bus: Arc::new(command_bus),
            control,
            sandbox,
            output,
            input,
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
                max_artifact_response_bytes: MAX_ARTIFACT_OBJECT_BYTES,
            },
            model: ModelGatewayConfig {
                origin: deployment.platform_origin.clone(),
                connect_timeout: grpc_connect_timeout,
                response_header_timeout: Duration::from_millis(
                    limits.model_response_header_timeout_millis,
                ),
                stream_idle_timeout: Duration::from_millis(limits.model_stream_idle_timeout_millis),
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
        | RuntimeTrustError::InvalidSigningKeyring => {
            ProductionBootstrapError::InvalidConfiguration
        }
    }
}

/// Project the deployment onto the restricted command-bus consumer profile.
pub(crate) fn nats_transport_config(
    deployment: &RuntimeDeployConfig,
) -> Result<NatsJetStreamConfig, ProductionBootstrapError> {
    let limits = deployment.limits;
    let tls = match (
        &deployment.nats_ca_path,
        &deployment.nats_certificate_path,
        &deployment.nats_private_key_path,
    ) {
        (Some(ca_path), Some(certificate_path), Some(private_key_path)) => Some(NatsTlsPaths {
            ca_path: ca_path.clone(),
            certificate_path: certificate_path.clone(),
            private_key_path: private_key_path.clone(),
        }),
        (None, None, None) => None,
        _ => return Err(ProductionBootstrapError::InvalidConfiguration),
    };
    let operation_timeout = Duration::from_millis(limits.grpc_deadline_millis);
    Ok(NatsJetStreamConfig {
        url: deployment.nats_url.clone(),
        tls,
        stream: deployment.nats_stream.clone(),
        consumer: deployment.nats_consumer.clone(),
        client_name: deployment.consumer_id.clone(),
        limits: CommandBusLimits {
            max_message_bytes: limits.max_transport_message_bytes(),
            max_payload_bytes: limits.max_transport_payload_bytes(),
        },
        fetch_batch: limits.nats_fetch_batch,
        fetch_expires: Duration::from_millis(limits.nats_fetch_expires_millis),
        connection_timeout: operation_timeout,
        request_timeout: operation_timeout,
    })
}

const fn map_nats_error(error: &NatsJetStreamError) -> ProductionBootstrapError {
    match error.kind() {
        NatsJetStreamErrorKind::Authentication => ProductionBootstrapError::AuthenticationFailed,
        NatsJetStreamErrorKind::ConsumerMissing => ProductionBootstrapError::ConsumerMissing,
        NatsJetStreamErrorKind::DependencyUnavailable | NatsJetStreamErrorKind::Timeout => {
            ProductionBootstrapError::DependencyUnavailable
        }
        NatsJetStreamErrorKind::ResourceExhausted => ProductionBootstrapError::ResourceExhausted,
        NatsJetStreamErrorKind::Configuration
        | NatsJetStreamErrorKind::Protocol
        | NatsJetStreamErrorKind::Closed => ProductionBootstrapError::InvalidConfiguration,
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
        // NotFound and Rejected share this arm rather than carrying one of
        // their own, and the sharing is the point: a resource the claim was
        // allowed to read but that no longer exists is a stale REFERENCE, and
        // a refused builder document is bad BYTES — different causes, same
        // terminal verdict. Neither may re-enter the retrying bucket, because
        // the identical request retried is the identical failure. (Two arms
        // with identical bodies is also a clippy error, so the grouping is
        // enforced as well as intended.)
        RuntimeContextError::InvalidConfiguration(_)
        | RuntimeContextError::InvalidResponse(_)
        | RuntimeContextError::NotFound(_)
        | RuntimeContextError::Rejected(_) => ProductionBootstrapError::InvalidConfiguration,
        RuntimeContextError::ResourceExhausted(_) => ProductionBootstrapError::ResourceExhausted,
        RuntimeContextError::AuthorizationFailed(_) => {
            ProductionBootstrapError::AuthenticationFailed
        }
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
    use std::path::PathBuf;
    use std::time::Duration;

    use super::{
        ProductionBootstrapError, map_config_error, map_nats_error, nats_transport_config,
    };
    use crate::config::{RuntimeConfigError, RuntimeDeployConfig, RuntimeLimits};
    use crate::transport::nats_jetstream::NatsJetStreamError;

    fn deployment() -> RuntimeDeployConfig {
        RuntimeDeployConfig {
            schema_version: "elitea.runtime-deploy.v1".to_owned(),
            limits_revision: crate::protocol::command::LIMITS_REVISION.to_owned(),
            workload_session_id: "session-1".to_owned(),
            producer_id: "worker-1".to_owned(),
            consumer_id: "worker-1-consumer".to_owned(),
            nats_url: "tls://nats.internal:4222".to_owned(),
            nats_ca_path: Some(PathBuf::from("/runtime/nats/ca.crt")),
            nats_certificate_path: Some(PathBuf::from("/runtime/nats/tls.crt")),
            nats_private_key_path: Some(PathBuf::from("/runtime/nats/tls.key")),
            nats_stream: "ELITEA_RT_V1_AGENT".to_owned(),
            nats_consumer: "elitea-agent-worker-v1".to_owned(),
            control_target: "control.internal:9443".to_owned(),
            output_target: "output.internal:9444".to_owned(),
            content_origin: "https://content.internal:9445".to_owned(),
            platform_origin: "https://platform.internal:9446".to_owned(),
            ca_path: PathBuf::from("/runtime/ca.pem"),
            certificate_path: PathBuf::from("/runtime/worker.pem"),
            private_key_path: PathBuf::from("/runtime/worker-key.pem"),
            ed25519_keyring_path: PathBuf::from("/runtime/keyring.json"),
            spool_root: PathBuf::from("/runtime/spool"),
            spool_key_path: PathBuf::from("/runtime/spool.key"),
            sandbox_runtimes: Vec::new(),
            agent_model_checkpoint_recovery: false,
            agent_node_recovery: false,
            agent_checkpoint_connection_path: Some(PathBuf::from("/runtime/agentstate")),
            limits: RuntimeLimits {
                nats_fetch_batch: 8,
                nats_fetch_expires_millis: 1_000,
                nats_in_progress_interval_millis: 5_000,
                nats_retry_delay_millis: 60_000,
                dependency_retry_millis: 250,
                delivery_max_concurrency: 128,
                delivery_queue_capacity: 128,
                sync_max_workers: 8,
                sync_max_in_flight: 16,
                admission_timeout_millis: 1_000,
                grpc_deadline_millis: 5_000,
                content_timeout_millis: 15_000,
                model_response_header_timeout_millis: 120_000,
                model_stream_idle_timeout_millis: 120_000,
                http_max_connections: 32,
                http_max_keepalive_connections: 16,
                output_max_queued_frames: 4,
                output_max_queued_bytes: 256 * 1_024,
                output_max_sessions: 2,
                output_ack_timeout_millis: 15_000,
                output_stream_deadline_millis: 300_000,
                lease_poll_interval_millis: 10_000,
                shutdown_timeout_millis: 30_000,
            },
        }
    }

    #[test]
    fn deployment_projects_the_exact_restricted_nats_profile() {
        let config = nats_transport_config(&deployment()).expect("valid deployment profile");
        assert_eq!(config.url, "tls://nats.internal:4222");
        assert_eq!(config.stream, "ELITEA_RT_V1_AGENT");
        assert_eq!(config.consumer, "elitea-agent-worker-v1");
        assert_eq!(config.client_name, "worker-1-consumer");
        assert_eq!(config.fetch_batch, 8);
        assert_eq!(config.fetch_expires, Duration::from_secs(1));
        assert_eq!(config.limits.max_message_bytes, 64 * 1_024);
        assert_eq!(config.limits.max_payload_bytes, 48 * 1_024);
        assert_eq!(config.connection_timeout, Duration::from_secs(5));
        assert_eq!(config.request_timeout, Duration::from_secs(5));
        let tls = config.tls.expect("mTLS material");
        assert_eq!(tls.private_key_path, PathBuf::from("/runtime/nats/tls.key"));

        let mut partial = deployment();
        partial.nats_ca_path = None;
        assert_eq!(
            nats_transport_config(&partial).err(),
            Some(ProductionBootstrapError::InvalidConfiguration)
        );
        let mut plain = deployment();
        plain.nats_url = "nats://nats:4222".to_owned();
        plain.nats_ca_path = None;
        plain.nats_certificate_path = None;
        plain.nats_private_key_path = None;
        assert!(
            nats_transport_config(&plain)
                .expect("compose profile")
                .tls
                .is_none()
        );
    }

    #[test]
    fn startup_errors_are_low_cardinality_and_retry_only_dependency_availability() {
        let cases = [
            ProductionBootstrapError::InvalidConfiguration,
            ProductionBootstrapError::ResourceExhausted,
            ProductionBootstrapError::AuthenticationFailed,
            ProductionBootstrapError::ConsumerMissing,
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
    fn local_and_nats_failures_preserve_only_safe_startup_categories() {
        assert_eq!(
            map_config_error(&RuntimeConfigError::ResourceExhausted("secret path")),
            ProductionBootstrapError::ResourceExhausted
        );
        assert_eq!(
            map_nats_error(&NatsJetStreamError::authentication("provider text")),
            ProductionBootstrapError::AuthenticationFailed
        );
        assert_eq!(
            map_nats_error(&NatsJetStreamError::consumer_missing("provider text")),
            ProductionBootstrapError::ConsumerMissing
        );
        assert_eq!(
            map_nats_error(&NatsJetStreamError::unavailable("provider text")),
            ProductionBootstrapError::DependencyUnavailable
        );
    }
}
