//! Browser sessions on Kubernetes (ADR-0049 decisions 1 and 10): one Pod
//! per session from `BrowserSessionContainer::kubernetes_pod`, on the
//! browser node pool under the profile's `RuntimeClass`, the session agent
//! as an attached exec. The `RuntimeClass` is verified from the API server
//! (as for workspace runs): a start is refused, fail-closed, before any Pod
//! exists unless its handler isolates, and the handler decides whether the
//! engine may run without its own sandbox (gVisor only).

use std::collections::BTreeMap;
use std::sync::{Arc, PoisonError};
use std::time::Duration;

use async_trait::async_trait;
use k8s_openapi::api::core::v1::Pod;
use kube::Api;
use kube::api::{AttachParams, DeleteParams, ListParams, PostParams};
use tokio::io::AsyncWriteExt as _;

use super::runtime_class;
use crate::sandbox::browser_session::{
    ACTIVATION_BINDING, AGENT_PATH, CONTAINER_NAME, EXPIRES_BINDING, Isolation,
    KIND_BROWSER_SESSION, KIND_LABEL, REQUEST_BINDING,
};
use crate::sandbox::browser_session_agent::{LineAgent, SessionAgent};
use crate::sandbox::browser_session_service::{BrowserSessionBackend, BrowserSessionLaunch};
use crate::sandbox::workspace_run_service::BackendError;

const API_TIMEOUT: Duration = Duration::from_secs(15);
/// How long a new Pod may take to run (image pull included).
const READY_TIMEOUT: Duration = Duration::from_mins(3);
const RUNTIME_CLASS_TTL: Duration = Duration::from_mins(1);
const REFUSAL_TTL: Duration = Duration::from_secs(5);
const PIPE_BYTES: usize = 1 << 20;

struct Verdict {
    at: tokio::time::Instant,
    /// The handler's isolation, or `None` for a refusal.
    isolation: Option<Isolation>,
}

impl Verdict {
    fn fresh(&self) -> bool {
        let ttl = if self.isolation.is_some() {
            RUNTIME_CLASS_TTL
        } else {
            REFUSAL_TTL
        };
        self.at.elapsed() < ttl
    }
}

/// The Kubernetes backend of one browser profile.
pub struct KubernetesBrowserSessions {
    client: kube::Client,
    pods: Api<Pod>,
    namespace: String,
    runtime_class: String,
    node_selector: BTreeMap<String, String>,
    tolerations: Vec<k8s_openapi::api::core::v1::Toleration>,
    verdict: std::sync::Mutex<Arc<tokio::sync::OnceCell<Verdict>>>,
}

impl KubernetesBrowserSessions {
    #[must_use]
    pub fn new(
        client: kube::Client,
        namespace: String,
        runtime_class: String,
        node_selector: BTreeMap<String, String>,
        tolerations: Vec<k8s_openapi::api::core::v1::Toleration>,
    ) -> Self {
        Self {
            pods: Api::namespaced(client.clone(), &namespace),
            client,
            namespace,
            runtime_class,
            node_selector,
            tolerations,
            verdict: std::sync::Mutex::new(Arc::new(tokio::sync::OnceCell::new())),
        }
    }

    /// The backend, its `RuntimeClass` checked once up front (a refusal is
    /// a warning: every start is refused until it isolates).
    pub async fn connect(
        client: kube::Client,
        namespace: String,
        runtime_class: String,
        node_selector: BTreeMap<String, String>,
        tolerations: Vec<k8s_openapi::api::core::v1::Toleration>,
    ) -> Self {
        let backend = Self::new(client, namespace, runtime_class, node_selector, tolerations);
        if backend.verified().await.is_none() {
            tracing::warn!(
                event = "browser_session_runtime_class_unverified",
                runtime_class = %backend.runtime_class,
                "Starting without a verified browser RuntimeClass; browser sessions are refused until it isolates"
            );
        }
        backend
    }

    async fn verified(&self) -> Option<Isolation> {
        let cell = {
            let mut current = self.verdict.lock().unwrap_or_else(PoisonError::into_inner);
            if current.get().is_some_and(|verdict| !verdict.fresh()) {
                *current = Arc::new(tokio::sync::OnceCell::new());
            }
            Arc::clone(&current)
        };
        cell.get_or_init(|| async {
            let isolation =
                match runtime_class::verify(self.client.clone(), &self.runtime_class).await {
                    Ok(handler) => Some(Isolation::of_handler(&handler)),
                    Err(refusal) => {
                        tracing::error!(
                            event = "browser_session_runtime_class_refused",
                            runtime_class = %self.runtime_class,
                            error = %refusal,
                            "Refusing browser sessions: the RuntimeClass does not isolate"
                        );
                        None
                    }
                };
            Verdict {
                at: tokio::time::Instant::now(),
                isolation,
            }
        })
        .await
        .isolation
    }

    /// The Pod of one session, binding annotations and tolerations included.
    ///
    /// # Errors
    ///
    /// As `BrowserSessionContainer::kubernetes_pod`.
    pub fn pod(&self, launch: &BrowserSessionLaunch) -> Result<serde_json::Value, BackendError> {
        render_pod(
            launch,
            &self.namespace,
            &self.runtime_class,
            &self.node_selector,
            &self.tolerations,
        )
    }

    async fn running(&self, name: &str) -> Result<(), BackendError> {
        let deadline = tokio::time::Instant::now() + READY_TIMEOUT;
        loop {
            let pod = tokio::time::timeout(API_TIMEOUT, self.pods.get_opt(name))
                .await
                .map_err(|_| BackendError::Timeout)?
                .map_err(|_| BackendError::Unavailable)?
                .ok_or(BackendError::NotFound)?;
            match pod
                .status
                .as_ref()
                .and_then(|status| status.phase.as_deref())
            {
                Some("Running") => return Ok(()),
                Some("Failed" | "Succeeded") => return Err(BackendError::Refused),
                _ => {}
            }
            if tokio::time::Instant::now() > deadline {
                return Err(BackendError::Timeout);
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
    }
}

/// [`KubernetesBrowserSessions::pod`] without a client (tests).
///
/// # Errors
///
/// As `BrowserSessionContainer::kubernetes_pod`.
pub fn render_pod(
    launch: &BrowserSessionLaunch,
    namespace: &str,
    runtime_class: &str,
    node_selector: &BTreeMap<String, String>,
    tolerations: &[k8s_openapi::api::core::v1::Toleration],
) -> Result<serde_json::Value, BackendError> {
    let mut pod = launch
        .container
        .kubernetes_pod(
            namespace,
            runtime_class,
            node_selector,
            launch.backstop_seconds,
        )
        .map_err(|_| BackendError::Refused)?;
    let annotations = pod["metadata"]["annotations"]
        .as_object_mut()
        .ok_or(BackendError::Refused)?;
    annotations.insert(
        REQUEST_BINDING.to_owned(),
        launch.binding.request.clone().into(),
    );
    annotations.insert(
        ACTIVATION_BINDING.to_owned(),
        launch.binding.owner.clone().into(),
    );
    annotations.insert(
        EXPIRES_BINDING.to_owned(),
        launch.binding.expires_at.to_string().into(),
    );
    if !tolerations.is_empty() {
        pod["spec"]["tolerations"] =
            serde_json::to_value(tolerations).map_err(|_| BackendError::Refused)?;
    }
    Ok(pod)
}

#[async_trait]
impl BrowserSessionBackend for KubernetesBrowserSessions {
    async fn isolation(&self) -> Result<Isolation, BackendError> {
        self.verified().await.ok_or(BackendError::Refused)
    }

    async fn create(&self, launch: &BrowserSessionLaunch) -> Result<(), BackendError> {
        // Fail-closed, before any Pod exists: never a session on runc.
        self.verified().await.ok_or(BackendError::Refused)?;
        let pod: Pod =
            serde_json::from_value(self.pod(launch)?).map_err(|_| BackendError::Refused)?;
        match tokio::time::timeout(API_TIMEOUT, self.pods.create(&PostParams::default(), &pod))
            .await
            .map_err(|_| BackendError::Timeout)?
        {
            Ok(_) => {}
            // Never reused: an existing Pod of that name is not adopted.
            Err(kube::Error::Api(error)) if error.code == 409 => {
                return Err(BackendError::Conflict);
            }
            Err(_) => return Err(BackendError::Unavailable),
        }
        if let Err(error) = self.running(&launch.container.name).await {
            let _ = self.destroy(&launch.container.name).await;
            return Err(error);
        }
        Ok(())
    }

    async fn attach(&self, name: &str) -> Result<Arc<dyn SessionAgent>, BackendError> {
        let params = AttachParams::default()
            .container(CONTAINER_NAME)
            .stdin(true)
            .stdout(true)
            .stderr(false);
        let mut process = self
            .pods
            .exec(
                name,
                vec![AGENT_PATH.to_owned(), "serve".to_owned()],
                &params,
            )
            .await
            .map_err(|_| BackendError::Unavailable)?;
        let mut input = process.stdin().ok_or(BackendError::Unavailable)?;
        let mut output = process.stdout().ok_or(BackendError::Unavailable)?;
        let (agent_stdin, mut to_exec) = tokio::io::duplex(PIPE_BYTES);
        let (mut from_exec, agent_stdout) = tokio::io::duplex(PIPE_BYTES);
        tokio::spawn(async move {
            let _ = tokio::io::copy(&mut to_exec, &mut input).await;
            let _ = input.shutdown().await;
        });
        tokio::spawn(async move {
            let _ = tokio::io::copy(&mut output, &mut from_exec).await;
            let _ = from_exec.shutdown().await;
            // The attached process lives as long as its output does.
            drop(process);
        });
        Ok(LineAgent::new(agent_stdin, agent_stdout))
    }

    async fn destroy(&self, name: &str) -> Result<bool, BackendError> {
        match tokio::time::timeout(
            API_TIMEOUT,
            self.pods.delete(
                name,
                &DeleteParams {
                    grace_period_seconds: Some(0),
                    ..DeleteParams::default()
                },
            ),
        )
        .await
        .map_err(|_| BackendError::Timeout)?
        {
            Ok(_) => Ok(true),
            Err(kube::Error::Api(error)) if error.code == 404 => Ok(false),
            Err(_) => Err(BackendError::Unavailable),
        }
    }

    async fn sessions(&self) -> Result<Vec<String>, BackendError> {
        let pods = tokio::time::timeout(
            API_TIMEOUT,
            self.pods.list(
                &ListParams::default().labels(&format!("{KIND_LABEL}={KIND_BROWSER_SESSION}")),
            ),
        )
        .await
        .map_err(|_| BackendError::Timeout)?
        .map_err(|_| BackendError::Unavailable)?;
        Ok(pods
            .items
            .iter()
            .filter_map(|pod| pod.metadata.name.clone())
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::browser_session::{Engine, tests::session};
    use crate::sandbox::kubernetes::workspace_run::tests::{Class, api_with};
    use crate::sandbox::workspace_run_service::RunBinding;

    fn launch() -> BrowserSessionLaunch {
        BrowserSessionLaunch {
            container: session(Engine::Full),
            binding: RunBinding {
                request: "a".repeat(64),
                owner: "b".repeat(64),
                expires_at: 1_700_004_320,
            },
            backstop_seconds: 4_320,
        }
    }

    fn selector() -> BTreeMap<String, String> {
        BTreeMap::from([("elitea.ai/browser".to_owned(), "true".to_owned())])
    }

    #[test]
    fn the_pod_lands_on_the_browser_pool_with_its_binding_and_backstop() {
        let toleration: k8s_openapi::api::core::v1::Toleration = serde_json::from_value(
            serde_json::json!({"key":"elitea.ai/browser","operator":"Exists","effect":"NoSchedule"}),
        )
        .expect("toleration");
        let pod = render_pod(
            &launch(),
            "elitea-execution",
            "gvisor",
            &selector(),
            &[toleration],
        )
        .expect("pod");
        assert_eq!(pod["spec"]["runtimeClassName"], "gvisor");
        assert_eq!(pod["spec"]["nodeSelector"]["elitea.ai/browser"], "true");
        assert_eq!(pod["spec"]["tolerations"][0]["key"], "elitea.ai/browser");
        assert_eq!(pod["spec"]["activeDeadlineSeconds"], 4_320);
        assert_eq!(pod["metadata"]["labels"][KIND_LABEL], KIND_BROWSER_SESSION);
        let annotations = &pod["metadata"]["annotations"];
        assert_eq!(annotations[REQUEST_BINDING], "a".repeat(64));
        assert_eq!(annotations[ACTIVATION_BINDING], "b".repeat(64));
        let _typed: Pod = serde_json::from_value(pod).expect("a valid Pod");
    }

    /// A `RuntimeClass` that does not isolate refuses before any Pod; gVisor
    /// and Kata both isolate, and only gVisor reports `Isolation::Gvisor`
    /// (the `--no-sandbox` rule is the service's).
    #[tokio::test]
    async fn the_runtime_class_decides_isolation_before_any_pod() {
        use std::sync::atomic::Ordering;
        for (handler, expected) in [
            (Some("runc"), None),
            (None, None),
            (Some("runsc"), Some(Isolation::Gvisor)),
            (Some("gvisor"), Some(Isolation::Gvisor)),
            (Some("kata-qemu"), Some(Isolation::Other)),
        ] {
            let class: Class = Arc::new(std::sync::Mutex::new(handler));
            let (client, _, creates) = api_with(class, Duration::ZERO);
            let backend = KubernetesBrowserSessions::new(
                client,
                "elitea-execution".into(),
                "gvisor".into(),
                selector(),
                Vec::new(),
            );
            assert_eq!(backend.isolation().await.ok(), expected, "{handler:?}");
            if expected.is_none() {
                assert!(matches!(
                    backend.create(&launch()).await,
                    Err(BackendError::Refused)
                ));
                assert_eq!(creates.load(Ordering::SeqCst), 0, "{handler:?}");
            }
        }
    }
}
