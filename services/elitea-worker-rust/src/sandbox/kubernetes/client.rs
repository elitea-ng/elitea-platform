//! Typed Kubernetes control operations. The receipt owner decides when cleanup is safe.
use super::{InvalidWorkload, PodIdentity, PodPolicy, confirmed_terminated};
use k8s_openapi::api::core::v1::Pod;
use kube::{
    Api, Client,
    api::{AttachParams, DeleteParams, LogParams, Patch, PatchParams, PostParams, Preconditions},
};
use std::time::Duration;

const API_TIMEOUT: Duration = Duration::from_secs(15);

#[path = "dependency_content.rs"]
mod dependency_content;
#[path = "platform_client.rs"]
mod platform_client;
#[path = "repository_content.rs"]
mod repository_content;

#[derive(Clone, Copy, Debug, thiserror::Error)]
pub enum ControlError {
    #[error("Kubernetes sandbox workload identity does not match the durable execution")]
    Identity,
    #[error("Kubernetes sandbox API request failed")]
    Api,
    #[error("Kubernetes sandbox API request timed out; its outcome must be reconciled")]
    Timeout,
    #[error("Kubernetes sandbox termination is not confirmed")]
    Running,
    #[error("Kubernetes sandbox helper failed or exceeded its output bound")]
    Helper,
    #[error("Kubernetes sandbox receipt is malformed or exceeds its size limit")]
    Receipt,
}

impl From<InvalidWorkload> for ControlError {
    fn from(_: InvalidWorkload) -> Self {
        Self::Identity
    }
}

#[derive(Clone, Copy)]
pub enum LifecycleCommand {
    Prepare,
    Prepared,
    Dispatch,
}
impl LifecycleCommand {
    fn argument(self) -> &'static str {
        match self {
            Self::Prepare => "--prepare",
            Self::Prepared => "--prepared",
            Self::Dispatch => "--dispatch",
        }
    }
}

/// One pooled client per supervisor profile. Credentials never enter execution Pods.
pub struct KubernetesClient {
    pods: Api<Pod>,
    namespace: String,
}

impl KubernetesClient {
    #[must_use]
    pub fn new(client: Client, namespace: String) -> Self {
        Self {
            pods: Api::namespaced(client, &namespace),
            namespace,
        }
    }

    /// Observe a workload by name and verify its immutable execution fingerprints.
    /// # Errors
    /// Rejects replacement Pods and unknown API outcomes. Only Kubernetes `NotFound` means absent.
    pub async fn observe(
        &self,
        identity: &PodIdentity,
        uid: Option<&str>,
    ) -> Result<Option<Pod>, ControlError> {
        let pod = tokio::time::timeout(API_TIMEOUT, self.pods.get_opt(&identity.name))
            .await
            .map_err(|_| ControlError::Timeout)?
            .map_err(|_| ControlError::Api)?;
        if let Some(pod) = &pod {
            validate(pod, identity, &self.namespace, uid)?;
        }
        Ok(pod)
    }

    /// Create an inert Pod. A conflict requires observation, never replacement.
    /// # Errors
    /// Timeout has an unknown outcome. Reconcile the same name before another attempt.
    pub async fn create(
        &self,
        policy: &PodPolicy,
        identity: &PodIdentity,
    ) -> Result<Pod, ControlError> {
        self.create_with_compiled_launch(policy, identity, None)
            .await
    }
    pub(crate) async fn create_with_compiled_launch(
        &self,
        policy: &PodPolicy,
        identity: &PodIdentity,
        launch: Option<(&str, &str, &str)>,
    ) -> Result<Pod, ControlError> {
        self.create_with_repository(policy, identity, launch, None)
            .await
    }
    pub(super) async fn create_with_repository(
        &self,
        policy: &PodPolicy,
        identity: &PodIdentity,
        launch: Option<(&str, &str, &str)>,
        repository: Option<&str>,
    ) -> Result<Pod, ControlError> {
        if policy.namespace != self.namespace {
            return Err(ControlError::Identity);
        }
        let mut workload = match repository {
            Some(root) => policy.workload_repository(identity, root)?,
            None => policy.workload(identity)?,
        };
        let expected = launch
            .map(|launch| compiled_launch_values(&policy.image, launch))
            .transpose()?;
        if let Some(env) = &expected {
            workload["spec"]["containers"][0]["env"]
                .as_array_mut()
                .ok_or(ControlError::Identity)?
                .extend(env.iter().cloned());
        }
        let pod: Pod = serde_json::from_value(workload).map_err(|_| ControlError::Identity)?;
        let result =
            tokio::time::timeout(API_TIMEOUT, self.pods.create(&PostParams::default(), &pod))
                .await
                .map_err(|_| ControlError::Timeout)?;
        let created = match result {
            Ok(pod) => pod,
            Err(kube::Error::Api(error)) if error.code == 409 => self
                .observe(identity, None)
                .await?
                .ok_or(ControlError::Identity)?,
            Err(_) => return Err(ControlError::Api),
        };
        validate(&created, identity, &self.namespace, None)?;
        if let Some(expected) = expected {
            validate_compiled_values(&created, &policy.image, &expected)?;
        }
        if let Some(root) = repository {
            let value = serde_json::to_value(&created).map_err(|_| ControlError::Identity)?;
            super::workspace::validate(&value, identity, &policy.image, root)?;
        }
        Ok(created)
    }

    pub(super) async fn verify_compiled_launch(
        &self,
        policy: &PodPolicy,
        identity: &PodIdentity,
        uid: &str,
        launch: (&str, &str, &str),
    ) -> Result<(), ControlError> {
        if policy.namespace != self.namespace {
            return Err(ControlError::Identity);
        }
        let expected = compiled_launch_values(&policy.image, launch)?;
        let pod = self
            .observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        validate_compiled_values(&pod, &policy.image, &expected)
    }

    /// Request graceful deletion of the original Pod. This does not confirm termination.
    /// # Errors
    /// Missing Pods and replacement UIDs leave termination unconfirmed.
    pub async fn request_stop(
        &self,
        identity: &PodIdentity,
        uid: &str,
    ) -> Result<(), ControlError> {
        let pod = self
            .observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Running)?;
        let params = DeleteParams {
            grace_period_seconds: Some(5),
            preconditions: Some(Preconditions {
                uid: Some(uid.to_owned()),
                resource_version: pod.metadata.resource_version,
            }),
            ..DeleteParams::default()
        };
        tokio::time::timeout(API_TIMEOUT, self.pods.delete(&identity.name, &params))
            .await
            .map_err(|_| ControlError::Timeout)?
            .map_err(|_| ControlError::Api)?;
        Ok(())
    }

    /// Send bounded lifecycle input through exec stdin, never command arguments.
    /// # Errors
    /// A timeout leaves the remote outcome unknown. The helper verifies its own Pod UID.
    pub async fn lifecycle(
        &self,
        identity: &PodIdentity,
        uid: &str,
        command: LifecycleCommand,
        code: Option<&str>,
        job: Option<&str>,
    ) -> Result<bool, ControlError> {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        self.observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        let payload = serde_json::to_vec(&serde_json::json!({
            "pod_uid":uid, "request_digest":identity.request, "code":code, "job":job
        }))
        .map_err(|_| ControlError::Identity)?;
        if payload.len() > 8 * 1024 * 1024 {
            return Err(ControlError::Identity);
        }
        let operation = async {
            let params = AttachParams::default()
                .container("code")
                .stdin(true)
                .stdout(true)
                .stderr(true);
            let mut process = self
                .pods
                .exec(
                    &identity.name,
                    ["/usr/local/bin/elitea-code-runner", command.argument()],
                    &params,
                )
                .await
                .map_err(|_| ControlError::Api)?;
            let mut stdin = process.stdin().ok_or(ControlError::Helper)?;
            let stdout = process.stdout().ok_or(ControlError::Helper)?;
            let stderr = process.stderr().ok_or(ControlError::Helper)?;
            let status = process.take_status().ok_or(ControlError::Helper)?;
            let write = async {
                stdin.write_all(&payload).await?;
                stdin.shutdown().await
            };
            let mut out = Vec::new();
            let mut err = Vec::new();
            let mut stdout = stdout.take(8193);
            let mut stderr = stderr.take(8193);
            tokio::try_join!(
                write,
                stdout.read_to_end(&mut out),
                stderr.read_to_end(&mut err)
            )
            .map_err(|_| ControlError::Helper)?;
            if out.len() > 8192 || err.len() > 8192 {
                return Err(ControlError::Helper);
            }
            let status = status.await.ok_or(ControlError::Helper)?;
            // Keep AttachedProcess alive while awaiting completion. Its Drop aborts
            // the transport task if this operation times out or is cancelled.
            let success = status.status.as_deref() == Some("Success");
            drop(process);
            if !success && !matches!(command, LifecycleCommand::Prepared) {
                return Err(ControlError::Helper);
            }
            Ok(success)
        };
        tokio::time::timeout(API_TIMEOUT, operation)
            .await
            .map_err(|_| ControlError::Timeout)?
    }

    /// Read bounded runner output only after the original container terminates.
    /// # Errors
    /// Missing or replacement Pods cannot supply a receipt for this execution.
    pub async fn receipt(
        &self,
        identity: &PodIdentity,
        uid: &str,
    ) -> Result<Option<Vec<u8>>, ControlError> {
        use futures_util::io::AsyncReadExt;
        let pod = self
            .observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        let value = serde_json::to_value(&pod).map_err(|_| ControlError::Identity)?;
        if !confirmed_terminated(&value, identity, uid)? {
            return Ok(None);
        }
        // The kernel can kill PID 1 before it writes a receipt. Only the original,
        // identity-checked terminal Pod can establish this failure; never infer it
        // from a transport error, a missing Pod, or untrusted code output.
        if let Some(receipt) = termination_receipt(&pod)? {
            return Ok(Some(receipt));
        }
        let read = async {
            let params = LogParams {
                container: Some("code".into()),
                ..LogParams::default()
            };
            let stream = self
                .pods
                .log_stream(&identity.name, &params)
                .await
                .map_err(|_| ControlError::Api)?;
            let mut bytes = Vec::new();
            stream
                .take(4 * 1024 * 1024 + 1)
                .read_to_end(&mut bytes)
                .await
                .map_err(|_| ControlError::Api)?;
            if bytes.len() > 4 * 1024 * 1024 {
                return Err(ControlError::Receipt);
            }
            self.observe(identity, Some(uid))
                .await?
                .ok_or(ControlError::Identity)?;
            validate_receipt(&bytes)?;
            Ok(Some(bytes))
        };
        tokio::time::timeout(API_TIMEOUT, read)
            .await
            .map_err(|_| ControlError::Timeout)?
    }

    /// Release the receipt finalizer only after durable receipt persistence.
    /// # Errors
    /// Requires confirmed termination and protects concurrent metadata changes.
    pub async fn cleanup(&self, identity: &PodIdentity, uid: &str) -> Result<(), ControlError> {
        let Some(pod) = self.observe(identity, Some(uid)).await? else {
            return Ok(());
        };
        let value = serde_json::to_value(&pod).map_err(|_| ControlError::Identity)?;
        if !confirmed_terminated(&value, identity, uid)? {
            return Err(ControlError::Running);
        }
        // Delete first while the receipt finalizer still retains the Pod.
        self.request_stop(identity, uid).await?;
        let pod = self
            .observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        let version = pod
            .metadata
            .resource_version
            .ok_or(ControlError::Identity)?;
        let finalizers: Vec<_> = pod
            .metadata
            .finalizers
            .unwrap_or_default()
            .into_iter()
            .filter(|value| value != super::FINALIZER)
            .collect();
        let patch = serde_json::json!({"metadata":{"uid":uid,"resourceVersion":version,"finalizers":finalizers}});
        tokio::time::timeout(
            API_TIMEOUT,
            self.pods.patch(
                &identity.name,
                &PatchParams::default(),
                &Patch::Merge(&patch),
            ),
        )
        .await
        .map_err(|_| ControlError::Timeout)?
        .map_err(|_| ControlError::Api)?;
        Ok(())
    }

    /// Require a kubelet-reported terminal container state on the original Pod.
    /// # Errors
    /// Deletion acknowledgement and Pod absence are not termination evidence.
    pub async fn terminated(
        &self,
        identity: &PodIdentity,
        uid: &str,
    ) -> Result<bool, ControlError> {
        let pod = self
            .observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Running)?;
        let value = serde_json::to_value(pod).map_err(|_| ControlError::Identity)?;
        Ok(confirmed_terminated(&value, identity, uid)?)
    }
}

fn termination_receipt(pod: &Pod) -> Result<Option<Vec<u8>>, ControlError> {
    let terminated = pod
        .status
        .as_ref()
        .and_then(|status| status.container_statuses.as_ref())
        .and_then(|states| states.iter().find(|state| state.name == "code"))
        .and_then(|state| state.state.as_ref())
        .and_then(|state| state.terminated.as_ref())
        .ok_or(ControlError::Running)?;
    let status = if terminated.reason.as_deref() == Some("OOMKilled") {
        "memory_limit"
    } else if pod
        .status
        .as_ref()
        .and_then(|status| status.reason.as_deref())
        == Some("DeadlineExceeded")
    {
        "timeout"
    } else if terminated.exit_code != 0 {
        "failed"
    } else {
        return Ok(None);
    };
    serde_json::to_vec(&serde_json::json!({
        "revision": 1, "status": status, "exit_code": terminated.exit_code,
        "stdout": "", "stderr": ""
    }))
    .map(Some)
    .map_err(|_| ControlError::Receipt)
}

fn validate_receipt(bytes: &[u8]) -> Result<(), ControlError> {
    #[derive(serde::Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Receipt {
        revision: u8,
        status: String,
        exit_code: Option<i32>,
        stdout: String,
        stderr: String,
    }
    let receipt: Receipt = serde_json::from_slice(bytes).map_err(|_| ControlError::Receipt)?;
    if receipt.revision != 1
        || receipt.stdout.len().saturating_add(receipt.stderr.len()) > 3 * 512 * 1024
        || !matches!(
            receipt.status.as_str(),
            "completed"
                | "failed"
                | "memory_limit"
                | "timeout"
                | "output_limit"
                | "capture_failed"
                | "launch_failed"
                | "invalid_request"
        )
        || (receipt.status == "completed" && receipt.exit_code != Some(0))
    {
        return Err(ControlError::Receipt);
    }
    Ok(())
}

fn compiled_launch_values(
    image: &str,
    (purpose, hash, policy): (&str, &str, &str),
) -> Result<Vec<serde_json::Value>, ControlError> {
    let (name, digest) = image.rsplit_once('@').ok_or(ControlError::Identity)?;
    let image_hash = digest
        .strip_prefix("sha256:")
        .ok_or(ControlError::Identity)?;
    if name.is_empty()
        || name.chars().any(char::is_whitespace)
        || !crate::sandbox::dependency_bundle::valid_digest(image_hash)
        || !matches!(purpose, "compile" | "execute")
        || !crate::sandbox::dependency_bundle::valid_digest(hash)
        || policy.is_empty()
        || policy.len() > 128
        || !policy
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
    {
        return Err(ControlError::Identity);
    }
    Ok(vec![
        serde_json::json!({"name":"ELITEA_COMPILED_CODE_SNAPSHOTS","value":"1"}),
        serde_json::json!({"name":"ELITEA_COMPILED_CODE_JOB_PURPOSE","value":purpose}),
        serde_json::json!({"name":"ELITEA_COMPILED_CODE_CONTROL_SHA256","value":hash}),
        serde_json::json!({"name":"ELITEA_COMPILED_CODE_IMAGE_DIGEST","value":digest}),
        serde_json::json!({"name":"ELITEA_COMPILED_CODE_POLICY_REVISION","value":policy}),
    ])
}
fn validate_compiled_values(
    pod: &Pod,
    image: &str,
    expected: &[serde_json::Value],
) -> Result<(), ControlError> {
    let spec = pod.spec.as_ref().ok_or(ControlError::Identity)?;
    let mut code = spec
        .containers
        .iter()
        .filter(|container| container.name == "code");
    let container = code.next().ok_or(ControlError::Identity)?;
    if code.next().is_some() || container.image.as_deref() != Some(image) {
        return Err(ControlError::Identity);
    }
    let env = container.env.as_ref().ok_or(ControlError::Identity)?;
    for binding in expected {
        let name = binding["name"].as_str().ok_or(ControlError::Identity)?;
        let mut matches = env.iter().filter(|entry| entry.name == name);
        let entry = matches.next().ok_or(ControlError::Identity)?;
        if matches.next().is_some()
            || entry.value.as_deref() != binding["value"].as_str()
            || entry.value_from.is_some()
        {
            return Err(ControlError::Identity);
        }
    }
    Ok(())
}

fn validate(
    pod: &Pod,
    identity: &PodIdentity,
    namespace: &str,
    uid: Option<&str>,
) -> Result<(), ControlError> {
    let actual_uid = pod
        .metadata
        .uid
        .as_deref()
        .filter(|value| !value.is_empty())
        .ok_or(ControlError::Identity)?;
    if pod.metadata.namespace.as_deref() != Some(namespace)
        || uid.is_some_and(|expected| expected != actual_uid)
    {
        return Err(ControlError::Identity);
    }
    let value = serde_json::to_value(pod).map_err(|_| ControlError::Identity)?;
    confirmed_terminated(&value, identity, actual_uid)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[tokio::test]
    async fn killed_original_pod_returns_failure_without_runner_logs() {
        for (reason, exit, expected) in [("OOMKilled", 137, "memory_limit"), ("Error", 1, "failed")]
        {
            let identity = PodIdentity::new(&[1; 32], &[2; 32]);
            let pod = json!({"apiVersion":"v1", "kind":"Pod", "metadata": {
                "name":identity.name,"namespace":"execution","uid":"original",
                "annotations":{"sandbox.elitea.ai/job":identity.job,"sandbox.elitea.ai/request":identity.request}
            }, "status":{"containerStatuses":[{"name":"code","image":"test","imageID":"test","ready":false,"restartCount":0,
                "state":{"terminated":{"exitCode":exit,"reason":reason}}}]}});
            let client = Client::new(
                tower::service_fn(move |request: http::Request<kube::client::Body>| {
                    assert!(!request.uri().path().ends_with("/log"));
                    let pod = pod.clone();
                    async move { Ok::<_, std::convert::Infallible>(response(200, &pod)) }
                }),
                "execution",
            );
            let control = KubernetesClient::new(client, "execution".into());
            let receipt = control
                .receipt(&identity, "original")
                .await
                .unwrap()
                .unwrap();
            validate_receipt(&receipt).unwrap();
            let envelope: serde_json::Value = serde_json::from_slice(&receipt).unwrap();
            assert_eq!(envelope["status"], expected);
            assert_eq!(envelope["exit_code"], exit);
            assert!(matches!(
                control.receipt(&identity, "replacement").await,
                Err(ControlError::Identity)
            ));
        }
    }

    #[test]
    fn receipt_requires_a_complete_runner_envelope_and_successful_exit() {
        assert!(validate_receipt(br#"{"status":"completed"}"#).is_err());
        for (status, code, valid) in [
            ("completed", 0, true),
            ("completed", 1, false),
            ("failed", 1, true),
            ("unknown", 0, false),
        ] {
            let bytes = serde_json::to_vec(&json!({"revision":1,"status":status,"exit_code":code,"stdout":"result","stderr":""})).unwrap();
            assert_eq!(validate_receipt(&bytes).is_ok(), valid);
        }
    }

    fn response(
        status: u16,
        value: &serde_json::Value,
    ) -> http::Response<http_body_util::Full<bytes::Bytes>> {
        http::Response::builder()
            .status(status)
            .body(http_body_util::Full::new(bytes::Bytes::from(
                serde_json::to_vec(value).unwrap(),
            )))
            .unwrap()
    }

    #[tokio::test]
    async fn only_not_found_is_absence_and_forbidden_is_not_replay_permission() {
        for (code, reason, absent) in [
            (404, "NotFound", true),
            (403, "Forbidden", false),
            (500, "InternalError", false),
        ] {
            let client = Client::new(
                tower::service_fn(move |_: http::Request<kube::client::Body>| async move {
                    Ok::<_, std::convert::Infallible>(response(
                        code,
                        &json!({
                            "kind":"Status", "apiVersion":"v1", "status":"Failure",
                            "reason":reason, "message":"Test", "code":code
                        }),
                    ))
                }),
                "execution",
            );
            let result = KubernetesClient::new(client, "execution".into())
                .observe(&PodIdentity::new(&[1; 32], &[2; 32]), None)
                .await;
            if absent {
                assert!(result.unwrap().is_none());
            } else {
                assert!(matches!(result, Err(ControlError::Api)));
            }
        }
    }

    #[tokio::test]
    async fn stop_uses_uid_and_resource_version_preconditions_without_claiming_termination() {
        use http_body_util::BodyExt;
        let identity = PodIdentity::new(&[1; 32], &[2; 32]);
        let pod = json!({"apiVersion":"v1", "kind":"Pod", "metadata": {
            "name": identity.name, "namespace":"execution", "uid":"original", "resourceVersion":"12",
            "annotations":{"sandbox.elitea.ai/job":identity.job,"sandbox.elitea.ai/request":identity.request}
        }, "status":{"phase":"Running"}});
        let deletes = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let seen = deletes.clone();
        let client = Client::new(
            tower::service_fn(move |request: http::Request<kube::client::Body>| {
                let pod = pod.clone();
                let seen = seen.clone();
                async move {
                    if request.method() == http::Method::DELETE {
                        let body = request.into_body().collect().await.unwrap().to_bytes();
                        let params: serde_json::Value = serde_json::from_slice(&body).unwrap();
                        assert_eq!(params["preconditions"]["uid"], "original");
                        assert_eq!(params["preconditions"]["resourceVersion"], "12");
                        assert_eq!(params["gracePeriodSeconds"], 5);
                        seen.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                    }
                    Ok::<_, std::convert::Infallible>(response(200, &pod))
                }
            }),
            "execution",
        );
        let control = KubernetesClient::new(client, "execution".into());
        control.request_stop(&identity, "original").await.unwrap();
        assert_eq!(deletes.load(std::sync::atomic::Ordering::SeqCst), 1);
        assert!(!control.terminated(&identity, "original").await.unwrap());
        assert!(
            control
                .request_stop(&identity, "replacement")
                .await
                .is_err()
        );
        assert_eq!(deletes.load(std::sync::atomic::Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn cleanup_preserves_other_finalizers_and_requires_terminal_state() {
        use http_body_util::BodyExt;
        for terminated in [false, true] {
            let identity = PodIdentity::new(&[1; 32], &[2; 32]);
            let mut pod = json!({"apiVersion":"v1","kind":"Pod","metadata":{
                "name":identity.name,"namespace":"execution","uid":"original","resourceVersion":"12",
                "annotations":{"sandbox.elitea.ai/job":identity.job,"sandbox.elitea.ai/request":identity.request},
                "finalizers":[super::super::FINALIZER,"other.example/retained"]
            }});
            if terminated {
                pod["status"] = json!({"containerStatuses":[{"name":"code","state":{"terminated":{"exitCode":0}}}]});
            }
            let patches = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
            let seen = patches.clone();
            let client = Client::new(
                tower::service_fn(move |request: http::Request<kube::client::Body>| {
                    let pod = pod.clone();
                    let seen = seen.clone();
                    async move {
                        if request.method() != http::Method::GET {
                            assert!(terminated);
                        }
                        if request.method() == http::Method::PATCH {
                            let bytes = request.into_body().collect().await.unwrap().to_bytes();
                            let patch: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
                            assert_eq!(patch["metadata"]["uid"], "original");
                            assert_eq!(patch["metadata"]["resourceVersion"], "12");
                            assert_eq!(
                                patch["metadata"]["finalizers"],
                                json!(["other.example/retained"])
                            );
                            seen.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                        }
                        Ok::<_, std::convert::Infallible>(response(200, &pod))
                    }
                }),
                "execution",
            );
            let result = KubernetesClient::new(client, "execution".into())
                .cleanup(&identity, "original")
                .await;
            assert_eq!(result.is_ok(), terminated);
            assert_eq!(
                patches.load(std::sync::atomic::Ordering::SeqCst),
                usize::from(terminated)
            );
        }
    }

    #[test]
    fn observation_rejects_cross_namespace_replacements_and_missing_uids() {
        let identity = PodIdentity::new(&[1; 32], &[2; 32]);
        let mut pod: Pod = serde_json::from_value(json!({
            "metadata": {"name": identity.name, "namespace": "execution", "uid": "original",
                "annotations": {"sandbox.elitea.ai/job": identity.job, "sandbox.elitea.ai/request": identity.request}}
        })).unwrap();
        assert!(validate(&pod, &identity, "execution", Some("original")).is_ok());
        assert!(validate(&pod, &identity, "other", Some("original")).is_err());
        assert!(validate(&pod, &identity, "execution", Some("replacement")).is_err());
        pod.metadata.uid = None;
        assert!(validate(&pod, &identity, "execution", None).is_err());
    }

    #[test]
    fn compiled_launch_binds_configured_image_and_policy_for_both_roles() {
        let image = format!("registry/runner@sha256:{}", "a".repeat(64));
        for purpose in ["compile", "execute"] {
            let values =
                compiled_launch_values(&image, (purpose, &"b".repeat(64), "rust-v2")).unwrap();
            assert_eq!(values.len(), 5);
            assert!(values.contains(&json!({"name":"ELITEA_COMPILED_CODE_IMAGE_DIGEST","value":format!("sha256:{}", "a".repeat(64))})));
            assert!(values.contains(
                &json!({"name":"ELITEA_COMPILED_CODE_POLICY_REVISION","value":"rust-v2"})
            ));
        }
        assert!(
            compiled_launch_values("runner:latest", ("compile", &"b".repeat(64), "rust-v1"))
                .is_err()
        );
        for policy in ["", "x y", "x\n", "x=override"] {
            assert!(compiled_launch_values(&image, ("compile", &"b".repeat(64), policy)).is_err());
        }
    }

    #[test]
    fn compiled_recovery_rejects_missing_duplicate_changed_or_indirect_values() {
        let image = format!("registry/runner@sha256:{}", "a".repeat(64));
        let expected =
            compiled_launch_values(&image, ("compile", &"b".repeat(64), "rust-v1")).unwrap();
        let original =
            json!({"spec":{"containers":[{"name":"code", "image":image, "env":expected}]}});
        let pod: Pod = serde_json::from_value(original.clone()).unwrap();
        assert!(validate_compiled_values(&pod, &image, &expected).is_ok());
        for index in 0..expected.len() {
            for mode in 0..4 {
                let mut changed = original.clone();
                let values = changed["spec"]["containers"][0]["env"]
                    .as_array_mut()
                    .unwrap();
                match mode {
                    0 => {
                        values.remove(index);
                    }
                    1 => values.push(expected[index].clone()),
                    2 => values[index]["value"] = "changed".into(),
                    3 => {
                        values[index]["valueFrom"] =
                            json!({"fieldRef":{"fieldPath":"metadata.name"}});
                    }
                    _ => unreachable!(),
                }
                let pod: Pod = serde_json::from_value(changed).unwrap();
                assert!(validate_compiled_values(&pod, &image, &expected).is_err());
            }
        }
        let mut changed = original;
        changed["spec"]["containers"][0]["image"] =
            format!("registry/runner@sha256:{}", "c".repeat(64)).into();
        let pod: Pod = serde_json::from_value(changed).unwrap();
        assert!(validate_compiled_values(&pod, &image, &expected).is_err());
    }

    #[tokio::test]
    async fn compiled_creation_sets_attested_values_and_refuses_conflicting_original_launch() {
        use http_body_util::BodyExt;
        for conflict in [false, true] {
            let image = format!("registry/runner@sha256:{}", "a".repeat(64));
            let hash = "b".repeat(64);
            let identity = PodIdentity::new(&[1; 32], &[2; 32]);
            let expected = compiled_launch_values(&image, ("compile", &hash, "rust-v1")).unwrap();
            let policy = PodPolicy {
                namespace: "execution".into(),
                image: image.clone(),
                runtime_class: "gvisor".into(),
                node_selector: std::collections::BTreeMap::from([(
                    "sandbox".into(),
                    "true".into(),
                )]),
                memory_bytes: 512 * 1024 * 1024,
                workspace_bytes: 256 * 1024 * 1024,
                cpu_millis: 1000,
                timeout_seconds: 60,
            };
            let mut original = policy.workload(&identity).unwrap();
            original["metadata"]["uid"] = "original".into();
            original["spec"]["containers"][0]["env"]
                .as_array_mut()
                .unwrap()
                .extend(expected.clone());
            if conflict {
                original["spec"]["containers"][0]["env"][6]["value"] = "rust-v2".into();
            }
            let client = Client::new(
                tower::service_fn(move |request: http::Request<kube::client::Body>| {
                    let original = original.clone();
                    let expected = expected.clone();
                    let image = image.clone();
                    async move {
                        if request.method() == http::Method::POST {
                            let bytes = request.into_body().collect().await.unwrap().to_bytes();
                            let created: serde_json::Value =
                                serde_json::from_slice(&bytes).unwrap();
                            assert_eq!(created["spec"]["containers"][0]["image"], image);
                            for value in expected {
                                assert!(
                                    created["spec"]["containers"][0]["env"]
                                        .as_array()
                                        .unwrap()
                                        .contains(&value)
                                );
                            }
                            if conflict {
                                return Ok::<_, std::convert::Infallible>(response(
                                    409,
                                    &json!({"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"already exists","reason":"AlreadyExists","code":409}),
                                ));
                            }
                        } else {
                            assert_eq!(request.method(), http::Method::GET);
                        }
                        Ok::<_, std::convert::Infallible>(response(200, &original))
                    }
                }),
                "execution",
            );
            let control = KubernetesClient::new(client, "execution".into());
            assert_eq!(
                control
                    .create_with_compiled_launch(
                        &policy,
                        &identity,
                        Some(("compile", &hash, "rust-v1"))
                    )
                    .await
                    .is_ok(),
                !conflict
            );
        }
    }

    #[tokio::test]
    async fn compiled_recovery_verifies_original_uid_without_mutating_the_pod() {
        let image = format!("registry/runner@sha256:{}", "a".repeat(64));
        let hash = "b".repeat(64);
        let identity = PodIdentity::new(&[1; 32], &[2; 32]);
        let expected = compiled_launch_values(&image, ("execute", &hash, "rust-v1")).unwrap();
        let pod = json!({"apiVersion":"v1","kind":"Pod","metadata":{
            "name":identity.name,"namespace":"execution","uid":"original",
            "annotations":{"sandbox.elitea.ai/job":identity.job,"sandbox.elitea.ai/request":identity.request}
        },"spec":{"containers":[{"name":"code","image":image,"env":expected}]}});
        let client = Client::new(
            tower::service_fn(move |request: http::Request<kube::client::Body>| {
                assert_eq!(request.method(), http::Method::GET);
                let pod = pod.clone();
                async move { Ok::<_, std::convert::Infallible>(response(200, &pod)) }
            }),
            "execution",
        );
        let control = KubernetesClient::new(client, "execution".into());
        let policy = PodPolicy {
            namespace: "execution".into(),
            image,
            runtime_class: "gvisor".into(),
            node_selector: std::collections::BTreeMap::from([("sandbox".into(), "true".into())]),
            memory_bytes: 512 * 1024 * 1024,
            workspace_bytes: 256 * 1024 * 1024,
            cpu_millis: 1000,
            timeout_seconds: 60,
        };
        assert!(
            control
                .verify_compiled_launch(
                    &policy,
                    &identity,
                    "original",
                    ("execute", &hash, "rust-v1")
                )
                .await
                .is_ok()
        );
        assert!(
            control
                .verify_compiled_launch(
                    &policy,
                    &identity,
                    "replacement",
                    ("execute", &hash, "rust-v1")
                )
                .await
                .is_err()
        );
        assert!(
            control
                .verify_compiled_launch(
                    &policy,
                    &identity,
                    "original",
                    ("execute", &hash, "rust-v2")
                )
                .await
                .is_err()
        );
    }
}

#[path = "compiled_content.rs"]
mod compiled_content;
