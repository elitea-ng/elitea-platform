//! The supervisor's `workspace` profile configuration (ADR-0034 decision 9):
//! its own listener, audience, runner image and ceilings, served by
//! [`super::workspace_run_service`]. One process may run it beside Code
//! profiles; owners and ports stay distinct.
//!
//! ```json
//! {"revision": 1, "purpose": "workspace",
//!  "listen_address": "0.0.0.0:9470", "owner": "workspace-runs",
//!  "audience": "dns:elitea-sandbox-workspace",
//!  "ca_path": "...", "certificate_path": "...", "private_key_path": "...",
//!  "verification_keyring_path": "...",
//!  "image": "ghcr.io/.../elitea-workspace-runner@sha256:<64 hex>",
//!  "concurrency": 16, "max_workspace_bytes": 10737418240,
//!  "max_cpu_millis": 4000, "max_memory_bytes": 8589934592,
//!  "max_wall_seconds": 3600,
//!  "backend": {"kind": "docker", "egress_network": "elitea-workspace-egress",
//!              "egress_dns": ["1.1.1.1"],
//!              "proxy_network": "elitea-workspace-proxy"}}
//! ```

use std::collections::BTreeMap;
use std::future::Future;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;
use tonic::transport::{Certificate, Identity, Server, ServerTlsConfig};
use zeroize::Zeroizing;

use super::process::StartupError;
use super::workspace_run_service::{
    WorkspaceRunBackend, WorkspaceRunCeilings, WorkspaceRunSupervisor,
};
use crate::config::read_regular_file;
use crate::protocol::sandbox_grant::GrantVerifier;
use crate::security::{load_ed25519_keyring_file, validate_tls_identity};

#[derive(Deserialize)]
#[serde(deny_unknown_fields, tag = "kind", rename_all = "lowercase")]
enum BackendConfig {
    Docker {
        /// The dedicated egress network (`deploy/docker/workspace-egress.sh`);
        /// required for a run with the project's network on.
        #[serde(default)]
        egress_network: Option<String>,
        /// The resolvers that network's rules admit.
        #[serde(default)]
        egress_dns: Vec<String>,
        /// The internal network of host-allowlist mode, on which only the
        /// egress proxy is reachable (`deploy/docker/workspace-egress.sh`);
        /// required for a run in proxy mode.
        #[serde(default)]
        proxy_network: Option<String>,
    },
    Kubernetes {
        namespace: String,
        runtime_class: String,
        node_selector: BTreeMap<String, String>,
    },
}

/// One `workspace` profile.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WorkspaceProfileConfig {
    revision: u32,
    purpose: String,
    listen_address: SocketAddr,
    owner: String,
    audience: String,
    ca_path: PathBuf,
    certificate_path: PathBuf,
    private_key_path: PathBuf,
    verification_keyring_path: PathBuf,
    image: String,
    concurrency: usize,
    max_workspace_bytes: u64,
    max_cpu_millis: u64,
    max_memory_bytes: u64,
    max_wall_seconds: u64,
    backend: BackendConfig,
}

/// Whether a profile file is a `workspace` profile.
#[must_use]
pub fn is_workspace_profile(raw: &[u8]) -> bool {
    serde_json::from_slice::<serde_json::Value>(raw)
        .ok()
        .and_then(|value| {
            value
                .get("purpose")
                .and_then(|v| v.as_str())
                .map(str::to_owned)
        })
        .as_deref()
        == Some("workspace")
}

fn pinned(image: &str) -> bool {
    image.rsplit_once("@sha256:").is_some_and(|(name, digest)| {
        !name.is_empty()
            && digest.len() == 64
            && digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    })
}

impl WorkspaceProfileConfig {
    /// # Errors
    ///
    /// Not this shape, or out of bounds.
    pub fn parse(raw: &[u8]) -> Result<Self, StartupError> {
        let config: Self =
            serde_json::from_slice(raw).map_err(|_| StartupError("parse the workspace profile"))?;
        config.validate()?;
        Ok(config)
    }

    fn validate(&self) -> Result<(), StartupError> {
        let identity = |value: &str| {
            !value.is_empty()
                && value.len() <= 256
                && !value.chars().any(|c| c.is_whitespace() || c.is_control())
        };
        if self.revision != 1
            || self.purpose != "workspace"
            || !identity(&self.owner)
            || !identity(&self.audience)
            || !pinned(&self.image)
            || !(1..=1024).contains(&self.concurrency)
            || !(256 << 20..=512 << 30).contains(&self.max_workspace_bytes)
            || !(1..=256_000).contains(&self.max_cpu_millis)
            || self.max_memory_bytes < 64 << 20
            || !(1..=86_400).contains(&self.max_wall_seconds)
        {
            return Err(StartupError("validate the workspace profile"));
        }
        match &self.backend {
            BackendConfig::Docker {
                egress_network,
                egress_dns,
                proxy_network,
            } => {
                if proxy_network.is_some() && proxy_network == egress_network {
                    return Err(StartupError(
                        "the proxy network must not be the egress network",
                    ));
                }
                if egress_network.is_some()
                    && (egress_dns.is_empty()
                        || egress_dns
                            .iter()
                            .any(|server| server.parse::<std::net::IpAddr>().is_err()))
                {
                    return Err(StartupError("name the egress network's resolvers"));
                }
            }
            BackendConfig::Kubernetes {
                namespace,
                runtime_class,
                node_selector,
            } => {
                if namespace.is_empty() || runtime_class.is_empty() || node_selector.is_empty() {
                    return Err(StartupError("validate Kubernetes workspace placement"));
                }
            }
        }
        Ok(())
    }

    /// The owner and port, for the process's uniqueness check.
    #[must_use]
    pub fn endpoint(&self) -> (&str, u16) {
        (self.owner.as_str(), self.listen_address.port())
    }

    fn ceilings(&self) -> WorkspaceRunCeilings {
        WorkspaceRunCeilings {
            image: self.image.clone(),
            max_workspace_bytes: self.max_workspace_bytes,
            max_cpu_millis: self.max_cpu_millis,
            max_memory_bytes: self.max_memory_bytes,
            max_wall_seconds: self.max_wall_seconds,
            concurrency: self.concurrency,
        }
    }
}

async fn backend(
    config: &WorkspaceProfileConfig,
) -> Result<Arc<dyn WorkspaceRunBackend>, StartupError> {
    Ok(match &config.backend {
        BackendConfig::Docker {
            egress_network,
            egress_dns,
            proxy_network,
        } => Arc::new(
            super::workspace_run_docker::DockerWorkspaceRuns::connect(
                egress_network.clone(),
                egress_dns.clone(),
                proxy_network.clone(),
            )
            .await
            .map_err(|_| StartupError("connect to Docker for workspace runs"))?,
        ),
        BackendConfig::Kubernetes {
            namespace,
            runtime_class,
            node_selector,
        } => {
            let cluster = kube::Config::incluster()
                .map_err(|_| StartupError("load in-cluster Kubernetes identity"))?;
            let client = kube::Client::try_from(cluster)
                .map_err(|_| StartupError("construct Kubernetes TLS client"))?;
            Arc::new(
                super::kubernetes::workspace_run::KubernetesWorkspaceRuns::connect(
                    client,
                    namespace.clone(),
                    runtime_class.clone(),
                    node_selector.clone(),
                )
                .await
                .map_err(|_| {
                    StartupError(
                        "verify the workspace RuntimeClass isolates (handler runsc, gvisor or kata*; see workspace_run_runtime_class_refused)",
                    )
                })?,
            )
        }
    })
}

/// Serve one `workspace` profile until `shutdown`.
///
/// # Errors
///
/// Trust material, the backend, or the listener.
pub async fn run(
    config: WorkspaceProfileConfig,
    shutdown: impl Future<Output = Result<(), StartupError>> + Send + 'static,
) -> Result<(), StartupError> {
    let read = |path: &PathBuf, limit, private, label| {
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
    let verifier = GrantVerifier::new(keys, config.audience.clone())
        .map_err(|_| StartupError("validate grant audience"))?;
    let backend = backend(&config).await?;
    let supervisor = Arc::new(WorkspaceRunSupervisor::new(
        verifier,
        backend,
        config.ceilings(),
    ));
    // Runs a previous process left are swept by their own expiry.
    let sweeper = tokio::spawn(supervisor.sweeper());
    let listener = tokio::net::TcpListener::bind(config.listen_address)
        .await
        .map_err(|_| StartupError("bind the TLS listener"))?;
    crate::diagnostics::install_tls_crypto_provider()
        .map_err(|_| StartupError("install the TLS crypto provider"))?;
    tracing::info!(
        event = "workspace_run_supervisor_starting",
        "Starting the authenticated workspace-run listener"
    );
    let (stop, stopped) = tokio::sync::oneshot::channel::<()>();
    let serving = Server::builder()
        .tls_config(
            ServerTlsConfig::new()
                .identity(Identity::from_pem(certificate, key.as_slice()))
                .client_ca_root(Certificate::from_pem(ca))
                .timeout(Duration::from_secs(10)),
        )
        .map_err(|_| StartupError("configure the TLS listener"))?
        .concurrency_limit_per_connection(64)
        .add_service(Arc::clone(&supervisor).into_service())
        .serve_with_incoming_shutdown(
            tokio_stream::wrappers::TcpListenerStream::new(listener),
            async move {
                let _ = stopped.await;
            },
        );
    tokio::pin!(serving);
    let result = tokio::select! {
        result = &mut serving => result.map_err(|_| StartupError("serve authenticated requests")),
        result = shutdown => {
            result?;
            let _ = stop.send(());
            let _ = tokio::time::timeout(Duration::from_secs(15), &mut serving).await;
            Ok(())
        }
    };
    sweeper.abort();
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    fn profile(backend: &str) -> String {
        format!(
            r#"{{"revision":1,"purpose":"workspace","listen_address":"127.0.0.1:9470","owner":"workspace-runs","audience":"dns:elitea-sandbox-workspace","ca_path":"/run/ca.pem","certificate_path":"/run/c.pem","private_key_path":"/run/k.pem","verification_keyring_path":"/run/keys.json","image":"registry.example/runner@sha256:{}","concurrency":4,"max_workspace_bytes":10737418240,"max_cpu_millis":4000,"max_memory_bytes":8589934592,"max_wall_seconds":3600,"backend":{backend}}}"#,
            "d".repeat(64)
        )
    }

    #[test]
    fn a_workspace_profile_is_recognised_and_validated() {
        let docker = profile(
            r#"{"kind":"docker","egress_network":"elitea-workspace-egress","egress_dns":["1.1.1.1"]}"#,
        );
        assert!(is_workspace_profile(docker.as_bytes()));
        assert_eq!(
            WorkspaceProfileConfig::parse(docker.as_bytes())
                .expect("valid")
                .endpoint(),
            ("workspace-runs", 9470)
        );
        let kube = profile(
            r#"{"kind":"kubernetes","namespace":"elitea-execution","runtime_class":"gvisor","node_selector":{"sandbox":"true"}}"#,
        );
        assert!(WorkspaceProfileConfig::parse(kube.as_bytes()).is_ok());
        // An egress network without resolvers, or a mutable image, is refused.
        let no_dns = profile(r#"{"kind":"docker","egress_network":"elitea-workspace-egress"}"#);
        assert!(WorkspaceProfileConfig::parse(no_dns.as_bytes()).is_err());
        let mutable = docker.replace(&format!("@sha256:{}", "d".repeat(64)), ":latest");
        assert!(WorkspaceProfileConfig::parse(mutable.as_bytes()).is_err());
        assert!(!is_workspace_profile(
            br#"{"revision":1,"purpose":"execution"}"#
        ));
        // The proxy network (host-allowlist mode) is its own network.
        let proxy = profile(
            r#"{"kind":"docker","egress_network":"elitea-workspace-egress","egress_dns":["1.1.1.1"],"proxy_network":"elitea-workspace-proxy"}"#,
        );
        assert!(WorkspaceProfileConfig::parse(proxy.as_bytes()).is_ok());
        let same = profile(
            r#"{"kind":"docker","egress_network":"elitea-workspace-egress","egress_dns":["1.1.1.1"],"proxy_network":"elitea-workspace-egress"}"#,
        );
        assert!(WorkspaceProfileConfig::parse(same.as_bytes()).is_err());
    }
}
