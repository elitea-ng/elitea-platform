//! Workspace runs on Kubernetes (ADR-0034 decision 9): one Pod per run from
//! `WorkspaceRunContainer::kubernetes_pod`, its workspace an `emptyDir`
//! sized by `sizeLimit` and the container's `ephemeral-storage` limit (the
//! kubelet evicts a Pod over either), the runner's operations as execs.
//! The binding a run carries is in annotations (a 64-hex value does not fit
//! a label).

use std::collections::BTreeMap;
use std::time::Duration;

use async_trait::async_trait;
use k8s_openapi::api::core::v1::Pod;
use kube::Api;
use kube::api::{AttachParams, DeleteParams, ListParams, PostParams};
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite, AsyncWriteExt as _};

use super::runtime_class::{self, RuntimeClassRefusal};
use crate::sandbox::workspace_run::{
    ACTIVATION_BINDING, CONTAINER_NAME, EXPIRES_BINDING, KIND_LABEL, KIND_WORKSPACE_RUN,
    REQUEST_BINDING,
};
use crate::sandbox::workspace_run_service::{
    BackendError, ExecExit, RunBinding, WorkspaceRunBackend, WorkspaceRunLaunch, failure_code,
};

const API_TIMEOUT: Duration = Duration::from_secs(15);
/// How long a new Pod may take to run (image pull included).
const READY_TIMEOUT: Duration = Duration::from_mins(3);
const MAX_STDERR_BYTES: usize = 8 * 1024;
/// How long a verified `RuntimeClass` is trusted before it is read again. A
/// `RuntimeClass`'s handler is immutable, but the object can be deleted and
/// recreated with another one; a run never starts on a verdict older than
/// this.
const RUNTIME_CLASS_TTL: Duration = Duration::from_mins(1);

/// The Kubernetes backend of one workspace profile.
pub struct KubernetesWorkspaceRuns {
    client: kube::Client,
    pods: Api<Pod>,
    namespace: String,
    runtime_class: String,
    node_selector: BTreeMap<String, String>,
    /// When the `RuntimeClass` was last verified as isolating.
    verified_at: tokio::sync::Mutex<Option<tokio::time::Instant>>,
}

impl KubernetesWorkspaceRuns {
    #[must_use]
    pub fn new(
        client: kube::Client,
        namespace: String,
        runtime_class: String,
        node_selector: BTreeMap<String, String>,
    ) -> Self {
        Self {
            pods: Api::namespaced(client.clone(), &namespace),
            client,
            namespace,
            runtime_class,
            node_selector,
            verified_at: tokio::sync::Mutex::new(None),
        }
    }

    /// The backend, once its `RuntimeClass` is verified to isolate
    /// (`runsc`, `gvisor` or `kata*`). The supervisor does not start on any other.
    ///
    /// # Errors
    ///
    /// [`RuntimeClassRefusal`] when the `RuntimeClass` is missing, unreadable
    /// or not isolating.
    pub async fn connect(
        client: kube::Client,
        namespace: String,
        runtime_class: String,
        node_selector: BTreeMap<String, String>,
    ) -> Result<Self, RuntimeClassRefusal> {
        let backend = Self::new(client, namespace, runtime_class, node_selector);
        backend.isolating().await?;
        Ok(backend)
    }

    /// Verify the profile's `RuntimeClass`, from a verdict at most
    /// [`RUNTIME_CLASS_TTL`] old. Only success is cached: a refusal is read
    /// again on the next run, so a fixed `RuntimeClass` is picked up without a
    /// restart, and a broken one never is trusted.
    async fn isolating(&self) -> Result<(), RuntimeClassRefusal> {
        let mut verified_at = self.verified_at.lock().await;
        if verified_at.is_some_and(|at| at.elapsed() < RUNTIME_CLASS_TTL) {
            return Ok(());
        }
        *verified_at = None;
        match runtime_class::verify(self.client.clone(), &self.runtime_class).await {
            Ok(handler) => {
                tracing::info!(
                    event = "workspace_run_runtime_class_verified",
                    runtime_class = %self.runtime_class,
                    handler = %handler,
                    "The workspace RuntimeClass isolates"
                );
                *verified_at = Some(tokio::time::Instant::now());
                Ok(())
            }
            Err(refusal) => {
                tracing::error!(
                    event = "workspace_run_runtime_class_refused",
                    runtime_class = %self.runtime_class,
                    error = %refusal,
                    "Refusing workspace runs: the RuntimeClass does not isolate"
                );
                Err(refusal)
            }
        }
    }

    /// The Pod of one run, binding annotations included.
    ///
    /// # Errors
    ///
    /// As `WorkspaceRunContainer::kubernetes_pod`.
    pub fn pod(&self, launch: &WorkspaceRunLaunch) -> Result<serde_json::Value, BackendError> {
        render_pod(
            launch,
            &self.namespace,
            &self.runtime_class,
            &self.node_selector,
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

/// [`KubernetesWorkspaceRuns::pod`] without a client (tests).
///
/// # Errors
///
/// As `WorkspaceRunContainer::kubernetes_pod`.
pub fn render_pod(
    launch: &WorkspaceRunLaunch,
    namespace: &str,
    runtime_class: &str,
    node_selector: &BTreeMap<String, String>,
) -> Result<serde_json::Value, BackendError> {
    let mut pod = launch
        .container
        .kubernetes_pod(namespace, runtime_class, node_selector)
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
    Ok(pod)
}

fn binding_of(pod: &Pod) -> Option<RunBinding> {
    let labels = pod.metadata.labels.as_ref()?;
    if labels.get(KIND_LABEL).map(String::as_str) != Some(KIND_WORKSPACE_RUN) {
        return None;
    }
    let annotations = pod.metadata.annotations.as_ref()?;
    Some(RunBinding {
        request: annotations.get(REQUEST_BINDING)?.clone(),
        owner: annotations.get(ACTIVATION_BINDING)?.clone(),
        expires_at: annotations.get(EXPIRES_BINDING)?.parse().ok()?,
    })
}

#[async_trait]
impl WorkspaceRunBackend for KubernetesWorkspaceRuns {
    async fn start(&self, launch: &WorkspaceRunLaunch) -> Result<bool, BackendError> {
        // Fail-closed, before any Pod exists: never a run on runc.
        self.isolating().await.map_err(|_| BackendError::Refused)?;
        let pod: Pod =
            serde_json::from_value(self.pod(launch)?).map_err(|_| BackendError::Refused)?;
        let created =
            match tokio::time::timeout(API_TIMEOUT, self.pods.create(&PostParams::default(), &pod))
                .await
                .map_err(|_| BackendError::Timeout)?
            {
                Ok(_) => true,
                Err(kube::Error::Api(error)) if error.code == 409 => {
                    match self.binding(&launch.container.name).await? {
                        Some(existing)
                            if existing.request == launch.binding.request
                                && existing.owner == launch.binding.owner => {}
                        _ => return Err(BackendError::Conflict),
                    }
                    false
                }
                Err(_) => return Err(BackendError::Unavailable),
            };
        if let Err(error) = self.running(&launch.container.name).await {
            if created {
                let _ = self.destroy(&launch.container.name).await;
            }
            return Err(error);
        }
        Ok(created)
    }

    async fn binding(&self, name: &str) -> Result<Option<RunBinding>, BackendError> {
        let pod = tokio::time::timeout(API_TIMEOUT, self.pods.get_opt(name))
            .await
            .map_err(|_| BackendError::Timeout)?
            .map_err(|_| BackendError::Unavailable)?;
        Ok(pod.as_ref().and_then(binding_of))
    }

    async fn exec(
        &self,
        name: &str,
        argv: &[String],
        stdin: &mut (dyn AsyncRead + Unpin + Send),
        stdout: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<ExecExit, BackendError> {
        let params = AttachParams::default()
            .container(CONTAINER_NAME)
            .stdin(true)
            .stdout(true)
            .stderr(true);
        let mut process = self
            .pods
            .exec(name, argv.to_vec(), &params)
            .await
            .map_err(|_| BackendError::Unavailable)?;
        let mut input = process.stdin().ok_or(BackendError::Unavailable)?;
        let mut output = process.stdout().ok_or(BackendError::Unavailable)?;
        let mut errors = process.stderr().ok_or(BackendError::Unavailable)?;
        let status = process.take_status().ok_or(BackendError::Unavailable)?;
        let send = async {
            let mut buffer = vec![0_u8; 32 * 1024];
            loop {
                let count = stdin.read(&mut buffer).await?;
                if count == 0 {
                    break;
                }
                input.write_all(&buffer[..count]).await?;
                input.flush().await?;
            }
            input.shutdown().await
        };
        let receive = async {
            let mut buffer = vec![0_u8; 32 * 1024];
            loop {
                let count = output.read(&mut buffer).await?;
                if count == 0 {
                    return stdout.flush().await;
                }
                stdout.write_all(&buffer[..count]).await?;
                stdout.flush().await?;
            }
        };
        let read_errors = async {
            let mut stderr = Vec::new();
            let mut buffer = vec![0_u8; 4 * 1024];
            while let Ok(count) = errors.read(&mut buffer).await {
                if count == 0 {
                    break;
                }
                let room = MAX_STDERR_BYTES.saturating_sub(stderr.len());
                stderr.extend_from_slice(&buffer[..count.min(room)]);
            }
            stderr
        };
        let (_, received, stderr) = tokio::join!(send, receive, read_errors);
        received.map_err(|_| BackendError::Unavailable)?;
        let status = status.await.ok_or(BackendError::Unavailable)?;
        // Keep the attached process alive until the status is read.
        drop(process);
        let exit_code = if status.status.as_deref() == Some("Success") {
            0
        } else {
            status
                .details
                .and_then(|details| details.causes)
                .unwrap_or_default()
                .into_iter()
                .find(|cause| cause.reason.as_deref() == Some("ExitCode"))
                .and_then(|cause| cause.message)
                .and_then(|code| code.parse().ok())
                .unwrap_or(-1)
        };
        Ok(ExecExit {
            exit_code,
            failure_code: failure_code(&stderr),
        })
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

    async fn runs(&self) -> Result<Vec<(String, RunBinding)>, BackendError> {
        let pods = tokio::time::timeout(
            API_TIMEOUT,
            self.pods
                .list(&ListParams::default().labels(&format!("{KIND_LABEL}={KIND_WORKSPACE_RUN}"))),
        )
        .await
        .map_err(|_| BackendError::Timeout)?
        .map_err(|_| BackendError::Unavailable)?;
        Ok(pods
            .items
            .iter()
            .filter_map(|pod| Some((pod.metadata.name.clone()?, binding_of(pod)?)))
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::workspace_run::WorkspaceRunContainer;

    fn launch() -> WorkspaceRunLaunch {
        WorkspaceRunLaunch {
            container: WorkspaceRunContainer {
                name: "elitea-ws-0123456789abcdef01234567".into(),
                image: format!("registry.example/runner@sha256:{}", "c".repeat(64)),
                workspace_bytes: 3 << 30,
                egress: false,
                egress_proxy: false,
                cpu_millis: 1_000,
                memory_bytes: 2 << 30,
                pids_limit: 4_096,
                wall_seconds: 600,
            },
            binding: RunBinding {
                request: "a".repeat(64),
                owner: "b".repeat(64),
                expires_at: 1_700_000_720,
            },
        }
    }

    /// A fake API server: the `RuntimeClass` `gvisor` with `handler`, and a
    /// Pod that is Running as soon as it is created. Counts `RuntimeClass`
    /// reads and Pod creations.
    fn api(
        handler: &'static str,
    ) -> (
        kube::Client,
        std::sync::Arc<std::sync::atomic::AtomicUsize>,
        std::sync::Arc<std::sync::atomic::AtomicUsize>,
    ) {
        use std::sync::atomic::{AtomicUsize, Ordering};
        let reads = std::sync::Arc::new(AtomicUsize::new(0));
        let creates = std::sync::Arc::new(AtomicUsize::new(0));
        let (seen_reads, seen_creates) = (reads.clone(), creates.clone());
        let client = kube::Client::new(
            tower::service_fn(move |request: http::Request<kube::client::Body>| {
                let path = request.uri().path().to_owned();
                let body = if path == "/apis/node.k8s.io/v1/runtimeclasses/gvisor" {
                    seen_reads.fetch_add(1, Ordering::SeqCst);
                    serde_json::json!({"apiVersion":"node.k8s.io/v1","kind":"RuntimeClass",
                        "metadata":{"name":"gvisor"},"handler":handler})
                } else {
                    assert!(
                        path.starts_with("/api/v1/namespaces/elitea-execution/pods"),
                        "{path}"
                    );
                    if request.method() == http::Method::POST {
                        seen_creates.fetch_add(1, Ordering::SeqCst);
                    }
                    serde_json::json!({"apiVersion":"v1","kind":"Pod",
                        "metadata":{"name":launch().container.name,"namespace":"elitea-execution"},
                        "status":{"phase":"Running"}})
                };
                async move {
                    Ok::<_, std::convert::Infallible>(
                        http::Response::builder()
                            .status(200)
                            .body(http_body_util::Full::new(bytes::Bytes::from(
                                serde_json::to_vec(&body).unwrap(),
                            )))
                            .unwrap(),
                    )
                }
            }),
            "elitea-execution",
        );
        (client, reads, creates)
    }

    fn backend(client: kube::Client) -> KubernetesWorkspaceRuns {
        KubernetesWorkspaceRuns::new(
            client,
            "elitea-execution".into(),
            "gvisor".into(),
            BTreeMap::from([("elitea.ai/sandbox".to_owned(), "true".to_owned())]),
        )
    }

    /// A `RuntimeClass` on runc (or crun) refuses before any Pod exists, at
    /// startup and on every run; runsc starts the run, and the verdict is
    /// cached, not read per run.
    #[tokio::test]
    async fn a_non_isolating_runtime_class_refuses_before_any_pod() {
        use std::sync::atomic::Ordering;
        for handler in ["runc", "crun"] {
            let (client, _, creates) = api(handler);
            assert!(matches!(
                KubernetesWorkspaceRuns::connect(
                    client.clone(),
                    "elitea-execution".into(),
                    "gvisor".into(),
                    BTreeMap::from([("a".to_owned(), "b".to_owned())]),
                )
                .await,
                Err(RuntimeClassRefusal::NotIsolating { .. })
            ));
            assert!(matches!(
                backend(client).start(&launch()).await,
                Err(BackendError::Refused)
            ));
            assert_eq!(creates.load(Ordering::SeqCst), 0, "{handler}");
        }
        let (client, reads, creates) = api("runsc");
        let runs = backend(client);
        assert!(runs.start(&launch()).await.expect("starts"));
        assert!(runs.start(&launch()).await.expect("starts"));
        assert_eq!(creates.load(Ordering::SeqCst), 2);
        assert_eq!(reads.load(Ordering::SeqCst), 1);
    }

    /// Item 5 on Kubernetes: the declared workspace is the `emptyDir`
    /// `sizeLimit` and inside the container's `ephemeral-storage` limit, and
    /// the binding rides annotations the API server keeps.
    #[test]
    fn the_pod_is_sized_and_carries_its_binding() {
        let selector = BTreeMap::from([("sandbox".to_owned(), "true".to_owned())]);
        let pod = render_pod(&launch(), "elitea-execution", "gvisor", &selector).expect("pod");
        assert_eq!(
            pod["spec"]["volumes"][0]["emptyDir"]["sizeLimit"],
            (3_u64 << 30).to_string()
        );
        let limit: u64 = pod["spec"]["containers"][0]["resources"]["limits"]["ephemeral-storage"]
            .as_str()
            .expect("limit")
            .parse()
            .expect("bytes");
        assert!(limit >= 3 << 30);
        let annotations = &pod["metadata"]["annotations"];
        assert_eq!(annotations[REQUEST_BINDING], "a".repeat(64));
        assert_eq!(annotations[ACTIVATION_BINDING], "b".repeat(64));
        assert_eq!(annotations[EXPIRES_BINDING], "1700000720");
        let typed: Pod = serde_json::from_value(pod).expect("a valid Pod");
        assert_eq!(binding_of(&typed), Some(launch().binding));
    }
}
