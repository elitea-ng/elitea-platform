//! Dedicated Docker supervisor process composition. No graph checkpoint ownership.
use super::{
    docker_supervisor::DockerSupervisor,
    ledger::JobLedger,
    request::{Language, PreparedJob},
    service::SupervisorService,
};
use crate::{
    config::read_regular_file,
    protocol::sandbox_grant::GrantVerifier,
    security::{load_ed25519_keyring_file, validate_tls_identity},
};
use adk_sandbox::workspace::DockerClient;
use serde::Deserialize;
use sqlx::{
    ConnectOptions,
    postgres::{PgConnectOptions, PgPoolOptions, PgSslMode},
};
use std::{
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};
use tonic::transport::{Certificate, Identity};
use zeroize::Zeroizing;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    revision: u32,
    listen_address: SocketAddr,
    owner: String,
    audience: String,
    ca_path: PathBuf,
    certificate_path: PathBuf,
    private_key_path: PathBuf,
    verification_keyring_path: PathBuf,
    database_url_path: PathBuf,
    database_ca_path: PathBuf,
    database_connections: u32,
    image_digest: String,
    policy_revision: String,
    languages: Vec<Language>,
    concurrency: usize,
    memory_bytes: u64,
    cpu_limit: f64,
    timeout_seconds: u32,
}

#[derive(Debug, thiserror::Error)]
#[error("sandbox supervisor could not {0}")]
pub struct StartupError(&'static str);

impl Config {
    fn validate(&self) -> Result<(), StartupError> {
        if self.revision != 1
            || !(1..=64).contains(&self.database_connections)
            || !(1..=1024).contains(&self.concurrency)
            || self.memory_bytes == 0
            || self.memory_bytes > i64::MAX as u64
            || !self.cpu_limit.is_finite()
            || self.cpu_limit <= 0.0
            || self.cpu_limit > 256.0
            || self.owner.is_empty()
            || self.owner.len() > 128
            || self.owner.contains('\0')
            || self.audience.is_empty()
            || self.audience.len() > 256
            || self.audience.chars().any(char::is_whitespace)
            || self.languages.is_empty()
            || self.languages.len() > 4
            || self
                .languages
                .iter()
                .enumerate()
                .any(|(i, l)| self.languages[..i].contains(l))
            || (self.languages.contains(&Language::Rust) && self.languages != [Language::Rust])
        {
            return Err(StartupError("validate bounded configuration"));
        }
        PreparedJob::new(
            self.languages[0],
            "validate".into(),
            std::collections::BTreeMap::new(),
            self.image_digest.clone(),
            self.policy_revision.clone(),
            self.timeout_seconds,
        )
        .map_err(|_| StartupError("validate runtime profile"))?;
        Ok(())
    }
}

/// Run the dedicated supervisor with file-backed secrets and mandatory TLS.
/// # Errors
/// Returns a redacted operation error. Secrets and user code never enter errors.
pub async fn run(path: &Path) -> Result<(), StartupError> {
    let raw = read_regular_file(path, 64 * 1024, false, "sandbox configuration")
        .map_err(|_| StartupError("read configuration"))?;
    let config: Config =
        serde_json::from_slice(&raw).map_err(|_| StartupError("parse configuration"))?;
    config.validate()?;
    let read = |path: &Path, limit, private, label| {
        read_regular_file(path, limit, private, label)
            .map_err(|_| StartupError("read required trust material"))
    };
    let ca = read(&config.ca_path, 1024 * 1024, false, "sandbox client CA")?;
    let certificate = read(
        &config.certificate_path,
        256 * 1024,
        false,
        "sandbox certificate",
    )?;
    let key = Zeroizing::new(read(
        &config.private_key_path,
        128 * 1024,
        true,
        "sandbox private key",
    )?);
    validate_tls_identity(&ca, &certificate, &key)
        .map_err(|_| StartupError("validate TLS identity"))?;
    let keys = load_ed25519_keyring_file(&config.verification_keyring_path)
        .map_err(|_| StartupError("load signing keyring"))?;
    let pool = connect_receipts(&config).await?;
    let verifier = GrantVerifier::new(keys, config.audience)
        .map_err(|_| StartupError("validate grant audience"))?;
    let runtime = DockerClient::with_image(config.image_digest)
        .await
        .map_err(|_| StartupError("connect to Docker"))?
        .with_resource_limits(Some(config.memory_bytes), Some(config.cpu_limit))
        .with_code_job_policy(Duration::from_secs(config.timeout_seconds.into()))
        .map_err(|_| StartupError("apply sandbox resource limits"))?;
    let runtime = if config.languages == [Language::Rust] {
        runtime
            .with_code_compilation()
            .map_err(|_| StartupError("enable Rust compilation"))?
    } else {
        runtime
    };
    runtime
        .check_code_image_ready()
        .await
        .map_err(|_| StartupError("find the preloaded runtime image"))?;
    let supervisor = DockerSupervisor::new(
        JobLedger::new(pool.clone()),
        runtime,
        config.owner,
        config.concurrency,
    )
    .and_then(|s| s.with_admission_policy(config.policy_revision, config.languages))
    .map_err(|_| StartupError("construct runtime admission policy"))?;
    let listener = tokio::net::TcpListener::bind(config.listen_address)
        .await
        .map_err(|_| StartupError("bind the TLS listener"))?;
    let service = SupervisorService::new(verifier, Arc::new(supervisor));
    let (stop, stopped) = tokio::sync::oneshot::channel();
    let result = {
        let serving = service.serve(
            listener,
            Identity::from_pem(certificate, key.as_slice()),
            Certificate::from_pem(ca),
            async {
                let _ = stopped.await;
            },
        );
        tokio::pin!(serving);
        tracing::info!(
            event = "sandbox_supervisor_starting",
            "Starting authenticated sandbox listener"
        );
        tokio::select! {
            result = &mut serving => result.map_err(|_| StartupError("serve authenticated requests")),
            result = shutdown_signal() => {
                result?;
                let _ = stop.send(());
                // Dropping handlers leaves runtime identities and receipts recoverable.
                if let Ok(result) = tokio::time::timeout(Duration::from_secs(15), &mut serving).await {
                    result.map_err(|_| StartupError("drain authenticated requests"))
                } else {
                    tracing::warn!(event="sandbox_shutdown_drain_expired", "Active jobs remain recoverable through durable receipts");
                    Ok(())
                }
            }
        }
    };
    pool.close().await;
    result
}

async fn connect_receipts(config: &Config) -> Result<sqlx::PgPool, StartupError> {
    let read = |path: &Path, limit, private, label| {
        read_regular_file(path, limit, private, label)
            .map_err(|_| StartupError("read database trust material"))
    };
    let database_url = Zeroizing::new(read(
        &config.database_url_path,
        8192,
        true,
        "sandbox database URL",
    )?);
    let options: PgConnectOptions = std::str::from_utf8(&database_url)
        .map_err(|_| StartupError("parse database configuration"))?
        .trim()
        .parse()
        .map_err(|_| StartupError("parse database configuration"))?;
    let options = options
        .ssl_mode(PgSslMode::VerifyFull)
        .ssl_root_cert_from_pem(read(
            &config.database_ca_path,
            1024 * 1024,
            false,
            "sandbox database CA",
        )?)
        .disable_statement_logging();
    let pool = PgPoolOptions::new()
        .max_connections(config.database_connections)
        .acquire_timeout(Duration::from_secs(10))
        .connect_with(options)
        .await
        .map_err(|_| StartupError("connect to receipt database"))?;
    // Migrations remain deployment-owned; do not mutate the product database.
    let table: Option<String> =
        sqlx::query_scalar("SELECT to_regclass('elitea_runtime.sandbox_jobs')::text")
            .fetch_one(&pool)
            .await
            .map_err(|_| StartupError("check receipt schema"))?;
    if table.is_none() {
        return Err(StartupError("find the migrated receipt schema"));
    }
    Ok(pool)
}

async fn shutdown_signal() -> Result<(), StartupError> {
    let mut terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        .map_err(|_| StartupError("register termination signal"))?;
    tokio::select! {
        result = tokio::signal::ctrl_c() => result.map_err(|_| StartupError("receive interrupt signal")),
        _ = terminate.recv() => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    const EXAMPLE: &str =
        include_str!("../../../../deploy/runtime/sandbox-supervisor.example.json");

    #[test]
    fn validates_example_and_rejects_unbounded_profiles() {
        let config: Config = serde_json::from_str(EXAMPLE).unwrap();
        config.validate().unwrap();
        for (field, value) in [
            ("revision", serde_json::json!(2)),
            ("concurrency", serde_json::json!(0)),
            ("database_connections", serde_json::json!(65)),
            ("memory_bytes", serde_json::json!(0)),
            ("cpu_limit", serde_json::json!(0)),
            ("timeout_seconds", serde_json::json!(3601)),
            ("languages", serde_json::json!(["rust", "python"])),
            ("languages", serde_json::json!(["python", "python"])),
            ("image_digest", serde_json::json!("runtime:latest")),
        ] {
            let mut input: serde_json::Value = serde_json::from_str(EXAMPLE).unwrap();
            input[field] = value;
            let config: Config = serde_json::from_value(input).unwrap();
            assert!(config.validate().is_err(), "{field}");
        }
    }

    #[test]
    fn request_timeout_cannot_exceed_deployment_profile() {
        let config: Config = serde_json::from_str(EXAMPLE).unwrap();
        let job = PreparedJob::new(
            Language::Python,
            "42".into(),
            std::collections::BTreeMap::new(),
            config.image_digest,
            config.policy_revision,
            61,
        )
        .unwrap();
        assert!(!job.within_timeout(Duration::from_mins(1)));
        assert!(job.within_timeout(Duration::from_secs(61)));
    }
}
