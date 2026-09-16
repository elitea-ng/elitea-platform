//! File-reloading production connector for the restricted Redis command plane.

#![allow(dead_code)] // Consumed by the capability-disabled production bootstrap.

use std::sync::Arc;
use std::time::Duration;

use crate::config::{RuntimeConfigError, RuntimeDeployConfig};
use crate::security::{RuntimeTrustError, load_redis_tls_material};

use super::redis_commands::RedisCommandLimits;
use super::redis_generation::{RedisGenerationFuture, RedisStreamsConnector, RedisStreamsHandle};
use super::redis_streams::{RedisStreamsClient, RedisStreamsConfig, RedisStreamsError};

/// Process-owned Redis connector whose every generation reloads its credential
/// files before creating the two restricted command-plane connections.
pub(crate) struct ProductionRedisConnector {
    deployment: Arc<RuntimeDeployConfig>,
    transport: RedisStreamsConfig,
}

impl ProductionRedisConnector {
    pub(crate) fn from_deployment(
        deployment: Arc<RuntimeDeployConfig>,
    ) -> Result<Self, RedisStreamsError> {
        let limits = deployment.limits;
        let control_concurrency =
            limits
                .delivery_max_concurrency
                .checked_add(4)
                .ok_or_else(|| {
                    RedisStreamsError::configuration(
                        "the Redis control concurrency limit is malformed",
                    )
                })?;
        let transport = RedisStreamsConfig {
            url: deployment.redis_url.clone(),
            stream: deployment.redis_stream.clone(),
            group: deployment.redis_group.clone(),
            consumer: deployment.consumer_id.clone(),
            command_limits: RedisCommandLimits {
                max_entry_bytes: limits.redis_max_entry_bytes(),
                max_field_bytes: limits.redis_max_field_bytes(),
            },
            read_count: limits.redis_read_batch as u64,
            block_millis: limits.redis_block_millis,
            reclaim_idle_millis: limits.redis_reclaim_idle_millis,
            connection_timeout: Duration::from_millis(limits.grpc_deadline_millis),
            control_timeout: Duration::from_millis(limits.grpc_deadline_millis),
            control_concurrency,
        };
        Ok(Self {
            deployment,
            transport,
        })
    }

    #[must_use]
    pub(crate) fn handle(self) -> Arc<RedisStreamsHandle<Self>> {
        Arc::new(RedisStreamsHandle::new(Arc::new(self)))
    }

    #[cfg(test)]
    fn transport(&self) -> &RedisStreamsConfig {
        &self.transport
    }
}

impl RedisStreamsConnector for ProductionRedisConnector {
    type Connection = RedisStreamsClient;

    fn connect(
        self: Arc<Self>,
    ) -> RedisGenerationFuture<Result<Arc<Self::Connection>, RedisStreamsError>> {
        Box::pin(async move {
            let tls = load_redis_tls_material(&self.deployment)
                .map_err(|error| map_trust_error(&error))?;
            let connection = RedisStreamsClient::connect(self.transport.clone(), tls).await?;
            connection.ping().await?;
            Ok(Arc::new(connection))
        })
    }
}

fn map_trust_error(error: &RuntimeTrustError) -> RedisStreamsError {
    match error {
        RuntimeTrustError::Material(RuntimeConfigError::Unavailable { .. }) => {
            RedisStreamsError::unavailable("the Redis credential files are unavailable")
        }
        RuntimeTrustError::Material(
            RuntimeConfigError::InvalidConfiguration(_) | RuntimeConfigError::ResourceExhausted(_),
        )
        | RuntimeTrustError::InvalidTlsIdentity
        | RuntimeTrustError::InvalidSpoolKey
        | RuntimeTrustError::InvalidRedisPassword
        | RuntimeTrustError::InvalidSigningKeyring
        | RuntimeTrustError::InvalidRedisTls => {
            RedisStreamsError::configuration("the Redis credential files are invalid")
        }
    }
}

#[cfg(test)]
mod tests {
    use std::io;
    use std::path::PathBuf;
    use std::sync::Arc;

    use super::{ProductionRedisConnector, map_trust_error};
    use crate::config::{RuntimeConfigError, RuntimeDeployConfig, RuntimeLimits};
    use crate::security::RuntimeTrustError;
    use crate::transport::redis_streams::RedisStreamsErrorKind;

    fn deployment() -> RuntimeDeployConfig {
        RuntimeDeployConfig {
            schema_version: "elitea.runtime-deploy.v1".to_owned(),
            limits_revision: crate::protocol::command::LIMITS_REVISION.to_owned(),
            workload_session_id: "session-1".to_owned(),
            producer_id: "worker-1".to_owned(),
            consumer_id: "worker-1-consumer".to_owned(),
            redis_url: "rediss://worker@redis.internal:6379/0".to_owned(),
            redis_password_path: PathBuf::from("/runtime/redis-password"),
            redis_stream: "commands.v1.agent.shared.1.0".to_owned(),
            redis_group: "elitea-rust-workers".to_owned(),
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
            agent_model_checkpoint_recovery: false,
            agent_checkpoint_connection_path: Some(PathBuf::from("/runtime/agentstate")),
            limits: RuntimeLimits {
                redis_read_batch: 8,
                redis_block_millis: 1_000,
                redis_reclaim_idle_millis: 60_000,
                redis_reclaim_interval_millis: 5_000,
                dependency_retry_millis: 250,
                delivery_max_concurrency: 128,
                delivery_queue_capacity: 128,
                sync_max_workers: 8,
                sync_max_in_flight: 16,
                admission_timeout_millis: 1_000,
                grpc_deadline_millis: 5_000,
                content_timeout_millis: 15_000,
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
    fn deployment_projects_the_exact_restricted_redis_profile() {
        let connector = ProductionRedisConnector::from_deployment(Arc::new(deployment()))
            .expect("valid deployment profile");
        let transport = connector.transport();

        assert_eq!(transport.read_count, 8);
        assert_eq!(transport.block_millis, 1_000);
        assert_eq!(transport.reclaim_idle_millis, 60_000);
        assert_eq!(transport.control_concurrency, 132);
        assert_eq!(transport.command_limits.max_entry_bytes, 64 * 1_024);
        assert_eq!(transport.command_limits.max_field_bytes, 48 * 1_024);
        assert_eq!(
            transport.connection_timeout,
            std::time::Duration::from_secs(5)
        );
        assert_eq!(transport.control_timeout, std::time::Duration::from_secs(5));
    }

    #[test]
    fn credential_file_availability_is_retryable_but_malformed_material_is_fatal() {
        let unavailable_error = RuntimeTrustError::Material(RuntimeConfigError::Unavailable {
            message: "redacted fixture",
            source: io::Error::new(io::ErrorKind::NotFound, "secret path"),
        });
        let unavailable = map_trust_error(&unavailable_error);
        assert_eq!(
            unavailable.kind(),
            RedisStreamsErrorKind::DependencyUnavailable
        );
        assert!(unavailable.retryable());

        for error in [
            RuntimeTrustError::InvalidRedisPassword,
            RuntimeTrustError::InvalidRedisTls,
            RuntimeTrustError::InvalidTlsIdentity,
        ] {
            let mapped = map_trust_error(&error);
            assert_eq!(mapped.kind(), RedisStreamsErrorKind::Configuration);
            assert!(!mapped.retryable());
        }
    }
}
