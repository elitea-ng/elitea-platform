//! Typed Kubernetes control operations. The receipt owner decides when cleanup is safe.
use super::{InvalidWorkload, PodIdentity, PodPolicy, confirmed_terminated};
use k8s_openapi::api::core::v1::Pod;
use kube::{
    Api, Client,
    api::{DeleteParams, PostParams, Preconditions},
};
use std::time::Duration;

const API_TIMEOUT: Duration = Duration::from_secs(15);

#[derive(Debug, thiserror::Error)]
pub enum ControlError {
    #[error("Kubernetes sandbox workload identity does not match the durable execution")]
    Identity,
    #[error("Kubernetes sandbox API request failed")]
    Api,
    #[error("Kubernetes sandbox API request timed out; its outcome must be reconciled")]
    Timeout,
    #[error("Kubernetes sandbox termination is not confirmed")]
    Running,
}

impl From<InvalidWorkload> for ControlError {
    fn from(_: InvalidWorkload) -> Self {
        Self::Identity
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
        if policy.namespace != self.namespace {
            return Err(ControlError::Identity);
        }
        let pod: Pod = serde_json::from_value(policy.workload(identity)?)
            .map_err(|_| ControlError::Identity)?;
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
        Ok(created)
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
}
