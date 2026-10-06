//! Dedicated sandbox supervisor process composition. No graph checkpoint ownership.
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

/// Maximum runtime profiles accepted by the process and its command line.
pub const MAX_RUNTIME_PROFILES: usize = 8;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    revision: u32,
    #[serde(default)]
    purpose: Purpose,
    #[serde(default)]
    native_platform: Option<super::native_bundle::NativePlatform>,
    #[serde(default)]
    native_workspace_bytes: Option<u64>,
    #[serde(default)]
    preparation_network: Option<String>,
    #[serde(default)]
    dependency_content: Option<ContentConfig>,
    #[serde(default)]
    #[serde(deserialize_with = "super::compiled_profile_config::non_null_config")]
    compiled_snapshot: Option<super::compiled_profile_config::RustCompiledSnapshotConfig>,
    #[serde(default)]
    backend: Backend,
    listen_address: SocketAddr,
    owner: String,
    audience: String,
    #[serde(default)]
    code_owner_requester: Option<String>,
    #[serde(default)]
    code_platform_profile: bool,
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

#[derive(Clone, Copy, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
enum Purpose {
    #[default]
    Execution,
    Preparation,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ContentConfig {
    origin: String,
    ca_path: PathBuf,
    certificate_path: PathBuf,
    private_key_path: PathBuf,
    staging_root: PathBuf,
    capacity: usize,
    timeout_seconds: u32,
}

#[derive(Default, Deserialize)]
#[serde(tag = "kind", rename_all = "lowercase", deny_unknown_fields)]
enum Backend {
    #[default]
    Docker,
    Kubernetes {
        cluster: String,
        namespace: String,
        image: String,
        runtime_class: String,
        node_selector: std::collections::BTreeMap<String, String>,
    },
}

#[derive(Debug, thiserror::Error)]
#[error("sandbox supervisor could not {0}")]
pub struct StartupError(&'static str);

impl Config {
    #[expect(
        clippy::too_many_lines,
        reason = "Keep coupled runtime, content and compiled startup validation together"
    )]
    fn validate(&self) -> Result<(), StartupError> {
        if self.code_owner_requester.as_ref().is_some_and(|v| {
            v.is_empty()
                || v.len() > 256
                || v.chars().any(|c| c.is_whitespace() || c.is_control())
                || self.purpose != Purpose::Execution
        }) {
            return Err(StartupError("validate Code owner requester"));
        }
        if self.code_platform_profile
            && (self.purpose != Purpose::Execution
                || self.code_owner_requester.is_none()
                || self.preparation_network.is_some()
                || (self.languages == [Language::Rust]
                    && self.policy_revision != "cargo-broker-execute-v1"))
        {
            return Err(StartupError(
                "select an offline original-owner Code platform profile",
            ));
        }
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
        match (&self.native_platform, self.native_workspace_bytes) {
            (Some(p), Some(bytes))
                if (512 * 1024 * 1024..=4 * 1024 * 1024 * 1024).contains(&bytes)
                    && self.memory_bytes >= bytes + 128 * 1024 * 1024
                    && !self.languages.contains(&Language::Python) =>
            {
                p.validate()
                    .map_err(|_| StartupError("validate native platform"))?;
            }
            (None, None) => {}
            _ => return Err(StartupError("bind native platform and workspace capacity")),
        }
        if self.purpose == Purpose::Preparation
            && self.languages != [Language::Python]
            && self.native_platform.is_none()
        {
            return Err(StartupError("select native preparation profile"));
        }
        if self.purpose == Purpose::Preparation {
            if !matches!(
                self.languages.as_slice(),
                [Language::Python | Language::JavaScript | Language::TypeScript | Language::Rust]
            ) || self.dependency_content.is_none()
            {
                return Err(StartupError("validate Python preparation content policy"));
            }
            if matches!(self.backend, Backend::Docker) && self.preparation_network.is_none() {
                return Err(StartupError("select an isolated preparation network"));
            }
        } else if self.preparation_network.is_some() {
            return Err(StartupError("preserve offline execution policy"));
        }
        if let Some(content) = &self.dependency_content
            && (!(1..=32).contains(&content.capacity)
                || !(1..=300).contains(&content.timeout_seconds)
                || !content.staging_root.is_absolute())
        {
            return Err(StartupError("validate dependency content bounds"));
        }
        if matches!(self.backend, Backend::Kubernetes { .. }) && self.preparation_network.is_some()
        {
            return Err(StartupError("use Kubernetes namespace egress policy"));
        }
        if let Backend::Kubernetes { cluster, image, .. } = &self.backend {
            if cluster.is_empty()
                || image.rsplit_once('@').map(|(_, digest)| digest)
                    != Some(self.image_digest.as_str())
            {
                return Err(StartupError("validate Kubernetes runtime image binding"));
            }
            self.kubernetes_policy()?
                .workload(&super::kubernetes::PodIdentity::new(&[0; 32], &[0; 32]))
                .map_err(|_| StartupError("validate Kubernetes isolation policy"))?;
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
        if let Some(compiled) = &self.compiled_snapshot {
            if self.purpose != Purpose::Execution
                || self.languages != [Language::Rust]
                || self.dependency_content.is_none()
            {
                return Err(StartupError(
                    "require Rust execution and content for compiled snapshots",
                ));
            }
            compiled
                .validate()
                .map_err(|error| StartupError(error.operation()))?;
        }
        Ok(())
    }
    fn compiled_profile(
        &self,
    ) -> Result<Option<Arc<super::compiled_snapshot::SnapshotProfile>>, StartupError> {
        self.compiled_snapshot
            .as_ref()
            .map(|config| {
                config.load(
                    &self.image_digest,
                    &self.policy_revision,
                    self.native_platform.as_ref(),
                )
            })
            .transpose()
            .map_err(|error| StartupError(error.operation()))
    }
    fn kubernetes_policy(&self) -> Result<super::kubernetes::PodPolicy, StartupError> {
        let Backend::Kubernetes {
            namespace,
            image,
            runtime_class,
            node_selector,
            ..
        } = &self.backend
        else {
            return Err(StartupError("select Kubernetes policy"));
        };
        // validate() bounds this finite positive value to at most 256 CPUs.
        #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
        let cpu_millis = (self.cpu_limit * 1000.0).ceil() as u32;
        Ok(super::kubernetes::PodPolicy {
            namespace: namespace.clone(),
            image: image.clone(),
            runtime_class: runtime_class.clone(),
            node_selector: node_selector.clone(),
            memory_bytes: self.memory_bytes,
            workspace_bytes: self.native_workspace_bytes.unwrap_or(256 * 1024 * 1024),
            cpu_millis,
            timeout_seconds: self.timeout_seconds,
        })
    }
}

/// Run bounded runtime profiles in one supervisor process.
/// Each profile retains its listener, authenticated audience, and durable owner.
/// # Errors
/// Rejects conflicting profiles before starting listeners. A profile failure
/// stops the process; existing sandbox jobs remain recoverable after restart.
pub async fn run_profiles(paths: &[PathBuf]) -> Result<(), StartupError> {
    if paths.is_empty() || paths.len() > MAX_RUNTIME_PROFILES {
        return Err(StartupError("validate runtime profile count"));
    }
    let mut configs = Vec::with_capacity(paths.len());
    for path in paths {
        let raw = read_regular_file(path, 64 * 1024, false, "sandbox configuration")
            .map_err(|_| StartupError("read configuration"))?;
        let config: Config =
            serde_json::from_slice(&raw).map_err(|_| StartupError("parse configuration"))?;
        config.validate()?;
        configs.push(config);
    }
    validate_profiles(&configs)?;
    let configs = configs
        .into_iter()
        .map(|config| {
            let compiled = config.compiled_profile()?;
            Ok((config, compiled))
        })
        .collect::<Result<Vec<_>, StartupError>>()?;
    let mut tasks = tokio::task::JoinSet::new();
    for (config, compiled) in configs {
        tasks.spawn(run_config(config, compiled));
    }
    while let Some(result) = tasks.join_next().await {
        result.map_err(|_| StartupError("join runtime profile"))??;
    }
    Ok(())
}

fn validate_profiles(configs: &[Config]) -> Result<(), StartupError> {
    if configs.is_empty() || configs.len() > MAX_RUNTIME_PROFILES {
        return Err(StartupError("validate runtime profile count"));
    }
    for (index, config) in configs.iter().enumerate() {
        if configs[..index].iter().any(|prior| {
            prior.owner == config.owner
                || prior.listen_address.port() == config.listen_address.port()
        }) {
            return Err(StartupError(
                "validate distinct runtime owners and listener ports",
            ));
        }
    }
    Ok(())
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
    let compiled = config.compiled_profile()?;
    run_config(config, compiled).await
}

async fn run_config(
    config: Config,
    compiled: Option<Arc<super::compiled_snapshot::SnapshotProfile>>,
) -> Result<(), StartupError> {
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
    let verifier = GrantVerifier::new(keys, config.audience.clone())
        .map_err(|_| StartupError("validate grant audience"))?;
    let verifier = match config.code_owner_requester.clone() {
        Some(peer) => verifier
            .with_code_owner_requester(peer)
            .map_err(|_| StartupError("validate Code owner requester"))?,
        None => verifier,
    };
    let content = connect_content(config.dependency_content.as_ref())?;
    let runtime = connect_runtime(&config).await?;
    let supervisor = DockerSupervisor::with_runtime(
        JobLedger::new(pool.clone()),
        runtime,
        config.owner,
        config.concurrency,
    )
    .and_then(|s| {
        let languages = config.languages;
        let s = match config.purpose {
            Purpose::Execution => {
                s.with_admission_policy(config.policy_revision, languages.clone())?
            }
            Purpose::Preparation => s.with_preparation_policy(config.policy_revision)?,
        };
        if let Some(platform) = config.native_platform {
            s.with_native_profile(platform, languages)
        } else {
            Ok(s)
        }
    })
    .map_err(|_| StartupError("construct runtime admission policy"))?;
    let supervisor = if let Some(profile) = compiled {
        supervisor.with_compiled_snapshots(profile)
    } else {
        supervisor
    };
    let listener = tokio::net::TcpListener::bind(config.listen_address)
        .await
        .map_err(|_| StartupError("bind the TLS listener"))?;
    let service = SupervisorService::new(verifier, Arc::new(supervisor));
    let service = if let Some(content) = content {
        service.with_dependency_content(content)
    } else {
        service
    };
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

async fn connect_runtime(
    config: &Config,
) -> Result<Box<dyn super::runtime::CodeJobRuntime>, StartupError> {
    let runtime: Box<dyn super::runtime::CodeJobRuntime> = match &config.backend {
        Backend::Docker => {
            let runtime = DockerClient::with_image(config.image_digest.clone())
                .await
                .map_err(|_| StartupError("connect to Docker"))?
                .with_resource_limits(Some(config.memory_bytes), Some(config.cpu_limit))
                .with_code_job_policy(Duration::from_secs(config.timeout_seconds.into()))
                .map_err(|_| StartupError("apply sandbox resource limits"))?;
            let runtime = if let Some(bytes) = config.native_workspace_bytes {
                runtime
                    .with_code_workspace_bytes(bytes)
                    .map_err(|_| StartupError("apply native workspace capacity"))?
            } else {
                runtime
            };
            let runtime = if config.languages == [Language::Rust]
                && matches!(config.purpose, Purpose::Execution)
            {
                runtime
                    .with_code_compilation()
                    .map_err(|_| StartupError("enable Rust compilation"))?
            } else {
                runtime
            };
            let runtime = if let Some(network) = &config.preparation_network {
                runtime
                    .with_code_preparation_network(network.clone())
                    .map_err(|_| StartupError("apply isolated preparation network policy"))?
            } else {
                runtime
            };
            let runtime = if config.code_platform_profile {
                runtime
                    .with_code_platform_profile()
                    .map_err(|_| StartupError("select Docker Code platform profile"))?
            } else {
                runtime
            };
            runtime
                .check_code_image_ready()
                .await
                .map_err(|_| StartupError("find the preloaded runtime image"))?;
            Box::new(runtime)
        }
        Backend::Kubernetes { cluster, .. } => {
            let cluster_config = kube::Config::incluster()
                .map_err(|_| StartupError("load in-cluster Kubernetes identity"))?;
            let client = kube::Client::try_from(cluster_config)
                .map_err(|_| StartupError("construct Kubernetes TLS client"))?;
            let runtime = super::kubernetes::runtime::KubernetesRuntime::new(
                client,
                config.kubernetes_policy()?,
                cluster.clone(),
                config.languages == [Language::Rust]
                    && matches!(config.purpose, Purpose::Execution),
            )
            .map_err(|_| StartupError("construct Kubernetes runtime"))?;
            let runtime = if config.code_platform_profile {
                runtime
                    .with_code_platform_profile()
                    .map_err(|_| StartupError("select Kubernetes Code platform profile"))?
            } else {
                runtime
            };
            Box::new(runtime)
        }
    };
    Ok(runtime)
}

fn connect_content(
    config: Option<&ContentConfig>,
) -> Result<Option<Arc<super::dependency_content::DependencyContentClient>>, StartupError> {
    let Some(config) = config else {
        return Ok(None);
    };
    let read = |path: &Path, limit, private, label| {
        read_regular_file(path, limit, private, label)
            .map_err(|_| StartupError("read dependency content trust material"))
    };
    let ca = read(&config.ca_path, 1024 * 1024, false, "dependency content CA")?;
    let certificate = read(
        &config.certificate_path,
        256 * 1024,
        false,
        "dependency content certificate",
    )?;
    let key = Zeroizing::new(read(
        &config.private_key_path,
        128 * 1024,
        true,
        "dependency content private key",
    )?);
    validate_tls_identity(&ca, &certificate, &key)
        .map_err(|_| StartupError("validate dependency content identity"))?;
    let mut identity = Zeroizing::new(certificate);
    identity.extend_from_slice(&key);
    super::dependency_content::DependencyContentClient::new(
        &config.origin,
        &ca,
        &identity,
        &config.staging_root,
        config.capacity,
        Duration::from_secs(u64::from(config.timeout_seconds)),
    )
    .map(Arc::new)
    .map(Some)
    .map_err(|_| StartupError("configure verified dependency content transport"))
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
    sqlx::query("SELECT runtime_id,runtime_bound_at,dispatched_at,preparation_bundle_json FROM elitea_runtime.sandbox_jobs LIMIT 0")
        .execute(&pool)
        .await
        .map_err(|_| StartupError("find the migrated runtime binding, deadline, and preparation columns"))?;
    if config.compiled_snapshot.is_some() {
        sqlx::query("SELECT compiled_purpose,compiled_snapshot_key,compiled_base_request_digest,compiled_descriptor_json,compiled_export_verified,compiled_export_lease_epoch,runtime_cleanup_confirmed_at FROM elitea_runtime.sandbox_jobs LIMIT 0")
            .execute(&pool)
            .await
            .map_err(|_| StartupError("find the migrated compiled intent and cleanup columns"))?;
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
    fn kubernetes_profile_binds_image_and_validates_isolation() {
        let mut input: serde_json::Value = serde_json::from_str(EXAMPLE).unwrap();
        let digest = input["image_digest"].as_str().unwrap().to_owned();
        input["backend"] = serde_json::json!({
            "kind":"kubernetes", "cluster":"rehearsal", "namespace":"code-execution",
            "image":format!("registry.example/code@{digest}"), "runtime_class":"sandbox",
            "node_selector":{"sandbox":"true"}
        });
        let config: Config = serde_json::from_value(input.clone()).unwrap();
        config.validate().unwrap();
        assert_eq!(config.kubernetes_policy().unwrap().cpu_millis, 1000);
        input["backend"]["image"] = serde_json::json!("registry.example/code:latest");
        let config: Config = serde_json::from_value(input).unwrap();
        assert!(config.validate().is_err());
    }

    #[test]
    fn compiled_startup_requires_rust_execution_and_existing_content_on_both_backends() {
        let mut input: serde_json::Value = serde_json::from_str(EXAMPLE).unwrap();
        input["languages"] = serde_json::json!(["rust"]);
        input["compiled_snapshot"] = serde_json::json!({
            "profiles_file":"/run/elitea-sandbox/compiled-profiles.json","profiles_sha256":"a".repeat(64),"dependency_bundle_sha256":""
        });
        let missing: Config = serde_json::from_value(input.clone()).unwrap();
        assert!(missing.validate().is_err());
        input["dependency_content"] = serde_json::json!({
            "origin":"https://elitea-main:9445","ca_path":"/run/elitea-sandbox/client-ca.pem",
            "certificate_path":"/run/elitea-sandbox/server.pem","private_key_path":"/run/elitea-sandbox/server.key",
            "staging_root":"/run/elitea-sandbox-content/rust/private","capacity":1,"timeout_seconds":30
        });
        for backend in [
            serde_json::json!({"kind":"docker"}),
            serde_json::json!({
                "kind":"kubernetes","cluster":"sandbox-cluster","namespace":"elitea-code-execution",
                "image":format!("registry.example/code@{}",input["image_digest"].as_str().unwrap()),
                "runtime_class":"sandbox","node_selector":{"elitea.ai/sandbox":"true"}
            }),
        ] {
            input["backend"] = backend;
            let configured: Config = serde_json::from_value(input.clone()).unwrap();
            configured.validate().unwrap();
            let mut other = input.clone();
            other["languages"] = serde_json::json!(["python"]);
            let rejected: Config = serde_json::from_value(other).unwrap();
            assert!(rejected.validate().is_err());
            let mut other = input.clone();
            other["purpose"] = serde_json::json!("preparation");
            let rejected: Config = serde_json::from_value(other).unwrap();
            assert!(rejected.validate().is_err());
        }
    }

    #[test]
    fn ordinary_supervisor_has_no_compiled_profile_or_file_read() {
        let config: Config = serde_json::from_str(EXAMPLE).unwrap();
        assert!(config.compiled_snapshot.is_none());
        assert!(config.compiled_profile().unwrap().is_none());
    }

    #[test]
    fn profiles_preserve_distinct_recovery_owners_and_listener_ports() {
        let first: Config = serde_json::from_str(EXAMPLE).unwrap();
        let mut second: Config = serde_json::from_str(EXAMPLE).unwrap();
        second.owner = "rust-runtime".into();
        second
            .listen_address
            .set_port(first.listen_address.port() + 1);
        let mut profiles = vec![first, second];
        assert!(validate_profiles(&profiles).is_ok());
        let duplicate_owner = profiles[0].owner.clone();
        profiles[1].owner = duplicate_owner;
        assert!(validate_profiles(&profiles).is_err());
        profiles[1].owner = "rust-runtime".into();
        profiles[1].listen_address = profiles[0].listen_address;
        assert!(validate_profiles(&profiles).is_err());
    }

    #[test]
    fn seven_and_eight_profiles_keep_bounded_independent_owners() {
        let mut profiles: Vec<Config> = (0..8u16)
            .map(|index| {
                let mut config: Config = serde_json::from_str(EXAMPLE).unwrap();
                config.owner = format!("profile-{index}");
                config.listen_address.set_port(9446 + index);
                config
            })
            .collect();
        assert!(validate_profiles(&[]).is_err());
        assert!(validate_profiles(&profiles[..7]).is_ok());
        assert!(validate_profiles(&profiles).is_ok());
        let mut ninth: Config = serde_json::from_str(EXAMPLE).unwrap();
        ninth.owner = "profile-8".into();
        ninth.listen_address.set_port(9454);
        profiles.push(ninth);
        assert!(validate_profiles(&profiles).is_err());
    }

    #[test]
    fn native_profiles_preserve_mixed_execution_and_single_language_preparation() {
        let mixed: Config = serde_json::from_str(EXAMPLE).unwrap();
        assert!(mixed.languages.contains(&Language::Python));
        mixed.validate().unwrap();
        let mut native: serde_json::Value = serde_json::from_str(EXAMPLE).unwrap();
        native["native_platform"] = serde_json::json!({"os":"linux", "arch":"arm64", "abi":"gnu"});
        native["native_workspace_bytes"] = serde_json::json!(512 * 1024 * 1024u64);
        native["memory_bytes"] = serde_json::json!(640 * 1024 * 1024u64);
        let config: Config = serde_json::from_value(native.clone()).unwrap();
        assert!(config.validate().is_err());
        for languages in [
            serde_json::json!(["javascript", "typescript"]),
            serde_json::json!(["rust"]),
        ] {
            native["languages"] = languages;
            let config: Config = serde_json::from_value(native.clone()).unwrap();
            config.validate().unwrap();
        }
        let python: Config = serde_json::from_value(preparation_config()).unwrap();
        python.validate().unwrap();
        let mut preparation = preparation_config();
        preparation["native_platform"] = native["native_platform"].clone();
        preparation["native_workspace_bytes"] = native["native_workspace_bytes"].clone();
        preparation["memory_bytes"] = native["memory_bytes"].clone();
        for language in ["javascript", "typescript", "rust"] {
            preparation["languages"] = serde_json::json!([language]);
            let config: Config = serde_json::from_value(preparation.clone()).unwrap();
            config.validate().unwrap();
        }
        for languages in [
            serde_json::json!(["javascript", "typescript"]),
            serde_json::json!(["python"]),
        ] {
            preparation["languages"] = languages;
            let config: Config = serde_json::from_value(preparation.clone()).unwrap();
            assert!(config.validate().is_err());
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

    fn preparation_config() -> serde_json::Value {
        let mut input: serde_json::Value = serde_json::from_str(EXAMPLE).unwrap();
        input["purpose"] = serde_json::json!("preparation");
        input["languages"] = serde_json::json!(["python"]);
        input["preparation_network"] = serde_json::json!("elitea-package-resolver");
        input["dependency_content"] = serde_json::json!({
            "origin": "https://elitea-main:9444", "ca_path": "/material/ca.pem",
            "certificate_path": "/material/client.pem", "private_key_path": "/material/client.key",
            "staging_root": "/private-staging", "capacity": 2, "timeout_seconds": 30
        });
        input
    }

    #[test]
    fn preparation_is_explicit_and_cannot_widen_execution_network_policy() {
        let original = preparation_config();
        let config: Config = serde_json::from_value(original.clone()).unwrap();
        config.validate().unwrap();
        for (field, value) in [
            ("languages", serde_json::json!(["javascript"])),
            ("languages", serde_json::json!(["python", "typescript"])),
            ("dependency_content", serde_json::Value::Null),
            ("preparation_network", serde_json::Value::Null),
            ("purpose", serde_json::json!("execution")),
        ] {
            let mut changed = original.clone();
            changed[field] = value;
            let config: Config = serde_json::from_value(changed).unwrap();
            assert!(config.validate().is_err(), "{field}");
        }
    }

    #[test]
    fn content_capacity_and_private_staging_configuration_remain_bounded() {
        for (field, value) in [
            ("capacity", serde_json::json!(0)),
            ("capacity", serde_json::json!(33)),
            ("timeout_seconds", serde_json::json!(0)),
            ("timeout_seconds", serde_json::json!(301)),
            ("staging_root", serde_json::json!("relative-path")),
        ] {
            let mut changed = preparation_config();
            changed["dependency_content"][field] = value;
            let config: Config = serde_json::from_value(changed).unwrap();
            assert!(config.validate().is_err(), "{field}");
        }
    }

    #[test]
    fn kubernetes_preparation_uses_its_namespace_policy() {
        let mut input = preparation_config();
        let digest = input["image_digest"].as_str().unwrap().to_owned();
        input["backend"] = serde_json::json!({
            "kind":"kubernetes", "cluster":"rehearsal", "namespace":"package-preparation",
            "image":format!("registry.example/code@{digest}"), "runtime_class":"sandbox",
            "node_selector":{"sandbox":"true"}
        });
        let config: Config = serde_json::from_value(input.clone()).unwrap();
        assert!(config.validate().is_err());
        input["preparation_network"] = serde_json::Value::Null;
        let config: Config = serde_json::from_value(input).unwrap();
        config.validate().unwrap();
    }
}

#[cfg(test)]
#[path = "process_code_platform_tests.rs"]
mod code_platform_tests;
