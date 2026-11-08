//! The supervisor's `browser` profile configuration (ADR-0049 decisions 1
//! and 10): its own listener, audience, session image and ceilings, served
//! by [`super::browser_session_service`]. One process may run it beside the
//! `code` and `workspace` profiles; owners and ports stay distinct.
//!
//! ```json
//! {"revision": 1, "purpose": "browser",
//!  "listen_address": "0.0.0.0:9471", "owner": "browser-sessions",
//!  "audience": "dns:elitea-sandbox-browser",
//!  "ca_path": "...", "certificate_path": "...", "private_key_path": "...",
//!  "verification_keyring_path": "...",
//!  "image": "ghcr.io/.../elitea-browser-session@sha256:<64 hex>",
//!  "concurrency": 100, "sessions_per_run": 3, "max_pages": 3,
//!  "max_wall_seconds": 3600, "engine_sandbox": false,
//!  "full": {"max_cpu_millis": 2000, "max_memory_bytes": 2147483648},
//!  "lite": {"max_cpu_millis": 1000, "max_memory_bytes": 536870912},
//!  "lite_image": "ghcr.io/.../elitea-browser-session-lite@sha256:<64 hex>",
//!  "backend": {"kind": "kubernetes", "namespace": "elitea-execution",
//!              "runtime_class": "gvisor",
//!              "node_selector": {"elitea.ai/browser": "true"},
//!              "tolerations": [...]}}
//! ```
//!
//! On Docker the backend is `{"kind": "docker", "proxy_network":
//! "elitea-browser-proxy", "runtime": "runsc"}`. `engine_sandbox: false`
//! (the engine runs with `--no-sandbox`) is admitted only where the runtime
//! is gVisor: on Docker the profile is refused at startup without
//! `"runtime": "runsc"`, and on Kubernetes each start is refused unless the
//! `RuntimeClass`'s handler is gVisor's. `lite` and `lite_image` come
//! together or not at all: absent by default, and set they let the profile
//! run `auto` sessions (admitted by the bake-off, decision 9) and `lite` ones
//! (not admitted: Main never sends one) in the
//! `lite` image (Moli beside chrome-headless-shell). The chart renders the
//! profile from `sandboxKubernetes.browserSessions`.

use std::collections::BTreeMap;
use std::future::Future;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;
use tonic::transport::{Certificate, Identity, Server, ServerTlsConfig};
use zeroize::Zeroizing;

use super::browser_session::{Isolation, MAX_PAGES, MAX_WALL_SECONDS, MIN_MEMORY_BYTES};
use super::browser_session_lifetime::TokioClock;
use super::browser_session_service::{
    BrowserCeilings, BrowserSessionBackend, BrowserSessionSupervisor, EngineCeiling,
};
use super::process::StartupError;
use crate::config::read_regular_file;
use crate::protocol::sandbox_grant::GrantVerifier;
use crate::security::{load_ed25519_keyring_file, validate_tls_identity};

#[derive(Deserialize)]
#[serde(deny_unknown_fields, tag = "kind", rename_all = "lowercase")]
enum BackendConfig {
    Docker {
        /// The internal network on which only the egress proxy is
        /// reachable; required for a session with egress.
        #[serde(default)]
        proxy_network: Option<String>,
        /// The Docker runtime (`runsc` for gVisor); the daemon's default
        /// when absent.
        #[serde(default)]
        runtime: Option<String>,
    },
    Kubernetes {
        namespace: String,
        runtime_class: String,
        /// The browser node pool (decision 10).
        node_selector: BTreeMap<String, String>,
        #[serde(default)]
        tolerations: Vec<k8s_openapi::api::core::v1::Toleration>,
    },
}

#[derive(Clone, Copy, Deserialize)]
#[serde(deny_unknown_fields)]
struct EngineConfig {
    max_cpu_millis: u64,
    max_memory_bytes: u64,
}

impl EngineConfig {
    fn valid(self) -> bool {
        (1..=64_000).contains(&self.max_cpu_millis)
            && (MIN_MEMORY_BYTES..=64 << 30).contains(&self.max_memory_bytes)
    }
}

/// One `browser` profile.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BrowserProfileConfig {
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
    sessions_per_run: usize,
    max_pages: u32,
    max_wall_seconds: u64,
    /// Required: whether the engine keeps its own sandbox.
    engine_sandbox: bool,
    full: EngineConfig,
    #[serde(default)]
    lite: Option<EngineConfig>,
    /// The `lite` session image, by digest; required with `lite`.
    #[serde(default)]
    lite_image: Option<String>,
    backend: BackendConfig,
}

/// Whether a profile file is a `browser` profile.
#[must_use]
pub fn is_browser_profile(raw: &[u8]) -> bool {
    serde_json::from_slice::<serde_json::Value>(raw)
        .ok()
        .and_then(|value| {
            value
                .get("purpose")
                .and_then(|v| v.as_str())
                .map(str::to_owned)
        })
        .as_deref()
        == Some("browser")
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

impl BrowserProfileConfig {
    /// # Errors
    ///
    /// Not this shape, or out of bounds.
    pub fn parse(raw: &[u8]) -> Result<Self, StartupError> {
        let config: Self =
            serde_json::from_slice(raw).map_err(|_| StartupError("parse the browser profile"))?;
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
            || self.purpose != "browser"
            || !identity(&self.owner)
            || !identity(&self.audience)
            || !pinned(&self.image)
            || !(1..=1024).contains(&self.concurrency)
            || !(1..=16).contains(&self.sessions_per_run)
            || !(1..=MAX_PAGES).contains(&self.max_pages)
            || !(60..=MAX_WALL_SECONDS).contains(&self.max_wall_seconds)
            || !self.full.valid()
            || self.lite.is_some_and(|lite| !lite.valid())
            || self.lite.is_some() != self.lite_image.is_some()
            || self
                .lite_image
                .as_deref()
                .is_some_and(|image| !pinned(image) || image == self.image)
        {
            return Err(StartupError("validate the browser profile"));
        }
        match &self.backend {
            BackendConfig::Docker {
                proxy_network,
                runtime,
            } => {
                if proxy_network.as_deref().is_some_and(|network| {
                    network.is_empty() || matches!(network, "host" | "bridge" | "none" | "default")
                }) || runtime.as_deref().is_some_and(str::is_empty)
                {
                    return Err(StartupError("validate Docker browser placement"));
                }
                // The engine without its own sandbox on a shared kernel.
                if !self.engine_sandbox
                    && Isolation::of_handler(runtime.as_deref().unwrap_or_default())
                        != Isolation::Gvisor
                {
                    return Err(StartupError(
                        "engine_sandbox false needs the gVisor runtime (runsc)",
                    ));
                }
            }
            BackendConfig::Kubernetes {
                namespace,
                runtime_class,
                node_selector,
                tolerations,
            } => {
                if namespace.is_empty()
                    || runtime_class.is_empty()
                    || node_selector.is_empty()
                    || tolerations.len() > 16
                {
                    return Err(StartupError("validate Kubernetes browser placement"));
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

    fn ceilings(&self) -> BrowserCeilings {
        let ceiling = |engine: EngineConfig| EngineCeiling {
            max_cpu_millis: engine.max_cpu_millis,
            max_memory_bytes: engine.max_memory_bytes,
        };
        BrowserCeilings {
            image: self.image.clone(),
            lite_image: self.lite_image.clone(),
            full: ceiling(self.full),
            lite: self.lite.map(ceiling),
            max_wall_seconds: self.max_wall_seconds,
            max_pages: self.max_pages,
            sessions_per_run: self.sessions_per_run,
            concurrency: self.concurrency,
            engine_sandbox: self.engine_sandbox,
        }
    }
}

async fn backend(
    config: &BrowserProfileConfig,
) -> Result<Arc<dyn BrowserSessionBackend>, StartupError> {
    Ok(match &config.backend {
        BackendConfig::Docker {
            proxy_network,
            runtime,
        } => Arc::new(
            super::browser_session_docker::DockerBrowserSessions::connect(
                proxy_network.clone(),
                runtime.clone(),
            )
            .await
            .map_err(|_| StartupError("connect to Docker for browser sessions"))?,
        ),
        BackendConfig::Kubernetes {
            namespace,
            runtime_class,
            node_selector,
            tolerations,
        } => {
            let cluster = kube::Config::incluster()
                .map_err(|_| StartupError("load in-cluster Kubernetes identity"))?;
            let client = kube::Client::try_from(cluster)
                .map_err(|_| StartupError("construct Kubernetes TLS client"))?;
            // A RuntimeClass that does not (yet) isolate is a warning, not a
            // startup failure: the backend refuses each start until it does.
            Arc::new(
                super::kubernetes::browser_session::KubernetesBrowserSessions::connect(
                    client,
                    namespace.clone(),
                    runtime_class.clone(),
                    node_selector.clone(),
                    tolerations.clone(),
                )
                .await,
            )
        }
    })
}

/// Serve one `browser` profile until `shutdown`.
///
/// # Errors
///
/// Trust material, the backend, or the listener.
pub async fn run(
    config: BrowserProfileConfig,
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
    let supervisor = Arc::new(BrowserSessionSupervisor::new(
        verifier,
        backend,
        config.ceilings(),
        Arc::new(TokioClock::default()),
    ));
    // Sessions a previous process left are removed by the first sweep: their
    // agents died with that process's execs, and a session is never reused.
    let sweeper = tokio::spawn(supervisor.sweeper());
    let listener = tokio::net::TcpListener::bind(config.listen_address)
        .await
        .map_err(|_| StartupError("bind the TLS listener"))?;
    crate::diagnostics::install_tls_crypto_provider()
        .map_err(|_| StartupError("install the TLS crypto provider"))?;
    tracing::info!(
        event = "browser_session_supervisor_starting",
        "Starting the authenticated browser-session listener"
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

    fn profile(engine_sandbox: bool, backend: &str) -> String {
        format!(
            r#"{{"revision":1,"purpose":"browser","listen_address":"127.0.0.1:9471","owner":"browser-sessions","audience":"dns:elitea-sandbox-browser","ca_path":"/run/ca.pem","certificate_path":"/run/c.pem","private_key_path":"/run/k.pem","verification_keyring_path":"/run/keys.json","image":"registry.example/browser@sha256:{}","concurrency":100,"sessions_per_run":3,"max_pages":3,"max_wall_seconds":3600,"engine_sandbox":{engine_sandbox},"full":{{"max_cpu_millis":2000,"max_memory_bytes":2147483648}},"lite":{{"max_cpu_millis":1000,"max_memory_bytes":536870912}},"lite_image":"registry.example/browser-lite@sha256:{}","backend":{backend}}}"#,
            "f".repeat(64),
            "c".repeat(64)
        )
    }

    const KUBE: &str = r#"{"kind":"kubernetes","namespace":"elitea-execution","runtime_class":"gvisor","node_selector":{"elitea.ai/browser":"true"}}"#;

    #[test]
    fn a_browser_profile_is_recognised_with_the_adr_ceilings() {
        let raw = profile(false, KUBE);
        assert!(is_browser_profile(raw.as_bytes()));
        let config = BrowserProfileConfig::parse(raw.as_bytes()).expect("valid");
        assert_eq!(config.endpoint(), ("browser-sessions", 9471));
        let ceilings = config.ceilings();
        assert_eq!(
            ceilings.full,
            EngineCeiling {
                max_cpu_millis: 2_000,
                max_memory_bytes: 2 << 30
            }
        );
        assert_eq!(
            ceilings.lite,
            Some(EngineCeiling {
                max_cpu_millis: 1_000,
                max_memory_bytes: 512 << 20
            })
        );
        assert_eq!(
            (
                ceilings.max_wall_seconds,
                ceilings.max_pages,
                ceilings.sessions_per_run
            ),
            (3_600, 3, 3)
        );
        assert!(!is_browser_profile(
            br#"{"revision":1,"purpose":"workspace"}"#
        ));
    }

    #[test]
    fn no_sandbox_on_docker_needs_the_gvisor_runtime() {
        for runtime in [
            r#"{"kind":"docker"}"#,
            r#"{"kind":"docker","runtime":"runc"}"#,
        ] {
            assert!(
                BrowserProfileConfig::parse(profile(false, runtime).as_bytes()).is_err(),
                "{runtime}"
            );
            // The engine's own sandbox is fine anywhere.
            assert!(BrowserProfileConfig::parse(profile(true, runtime).as_bytes()).is_ok());
        }
        let gvisor =
            r#"{"kind":"docker","runtime":"runsc","proxy_network":"elitea-browser-proxy"}"#;
        assert!(BrowserProfileConfig::parse(profile(false, gvisor).as_bytes()).is_ok());
    }

    #[test]
    fn out_of_bounds_profiles_are_refused() {
        let base = profile(false, KUBE);
        for (from, to) in [
            (r#""max_pages":3"#, r#""max_pages":0"#),
            (r#""sessions_per_run":3"#, r#""sessions_per_run":0"#),
            (r#""concurrency":100"#, r#""concurrency":0"#),
            (r#""max_wall_seconds":3600"#, r#""max_wall_seconds":100000"#),
            (
                r#""max_memory_bytes":536870912"#,
                r#""max_memory_bytes":1024"#,
            ),
            (
                r#""node_selector":{"elitea.ai/browser":"true"}"#,
                r#""node_selector":{}"#,
            ),
        ] {
            let changed = base.replace(from, to);
            assert_ne!(changed, base, "{from}");
            assert!(
                BrowserProfileConfig::parse(changed.as_bytes()).is_err(),
                "{to}"
            );
        }
        let mutable = base.replace(&format!("@sha256:{}", "f".repeat(64)), ":latest");
        assert!(BrowserProfileConfig::parse(mutable.as_bytes()).is_err());
        let missing = base.replace(r#""engine_sandbox":false,"#, "");
        assert!(
            BrowserProfileConfig::parse(missing.as_bytes()).is_err(),
            "engine_sandbox is required"
        );
        let lite_image = format!(
            r#","lite_image":"registry.example/browser-lite@sha256:{}""#,
            "c".repeat(64)
        );
        let no_lite = base
            .replace(
                r#","lite":{"max_cpu_millis":1000,"max_memory_bytes":536870912}"#,
                "",
            )
            .replace(&lite_image, "");
        let ceilings = BrowserProfileConfig::parse(no_lite.as_bytes())
            .expect("lite is optional")
            .ceilings();
        assert!(ceilings.lite.is_none() && ceilings.lite_image.is_none());
        // `lite` and its image come together, the image pinned and its own.
        let lite_without_image = base.replace(&lite_image, "");
        assert!(BrowserProfileConfig::parse(lite_without_image.as_bytes()).is_err());
        let image_without_lite = base.replace(
            r#","lite":{"max_cpu_millis":1000,"max_memory_bytes":536870912}"#,
            "",
        );
        assert!(BrowserProfileConfig::parse(image_without_lite.as_bytes()).is_err());
        let mutable_lite = base.replace(&format!("@sha256:{}", "c".repeat(64)), ":latest");
        assert!(BrowserProfileConfig::parse(mutable_lite.as_bytes()).is_err());
        let same_image = base.replace(
            &format!("browser-lite@sha256:{}", "c".repeat(64)),
            &format!("browser@sha256:{}", "f".repeat(64)),
        );
        assert!(BrowserProfileConfig::parse(same_image.as_bytes()).is_err());
        let lite = BrowserProfileConfig::parse(base.as_bytes())
            .expect("valid")
            .ceilings()
            .lite_image;
        assert_eq!(
            lite,
            Some(format!(
                "registry.example/browser-lite@sha256:{}",
                "c".repeat(64)
            ))
        );
    }

    /// The profile the chart renders (deploy/helm/tests/render-browser-sessions.sh
    /// holds the chart's output equal to this file) is one the supervisor
    /// accepts.
    #[test]
    fn the_chart_rendered_profile_parses() {
        let raw =
            include_str!("../../../../deploy/runtime/sandbox-browser.kubernetes.example.json");
        assert!(is_browser_profile(raw.as_bytes()));
        let config = BrowserProfileConfig::parse(raw.as_bytes()).expect("the chart's profile");
        assert_eq!(config.endpoint(), ("browser-sessions", 9471));
        assert_eq!(config.audience, "dns:elitea-sandbox-browser");
        assert!(matches!(
            &config.backend,
            BackendConfig::Kubernetes { runtime_class, .. } if runtime_class == "gvisor"
        ));
    }
}
