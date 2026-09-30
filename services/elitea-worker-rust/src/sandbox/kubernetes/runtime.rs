//! Kubernetes implementation of the shared durable supervisor runtime boundary.
use super::{
    PodIdentity, PodPolicy,
    client::{ControlError, KubernetesClient, LifecycleCommand},
};
use crate::sandbox::runtime::CodeJobRuntime;
use adk_sandbox::{
    SandboxError,
    workspace::{Manifest, ManifestEntry, docker::CodeJobIdentity},
};
use std::time::Duration;

pub struct KubernetesRuntime {
    client: KubernetesClient,
    policy: PodPolicy,
    cluster: String,
    digest: String,
    compilation: bool,
}

fn failure(error: ControlError) -> SandboxError {
    SandboxError::ExecutionFailed(error.to_string())
}

impl KubernetesRuntime {
    /// Use a stable deployment cluster identity across supervisor restarts.
    /// # Errors
    /// Rejects invalid placement, mutable images, and ambiguous runtime namespaces.
    pub fn new(
        client: kube::Client,
        policy: PodPolicy,
        cluster: String,
        compilation: bool,
    ) -> Result<Self, ControlError> {
        policy.workload(&PodIdentity::new(&[0; 32], &[0; 32]))?;
        if !super::dns_label(&cluster) {
            return Err(ControlError::Identity);
        }
        let digest = policy
            .image
            .rsplit_once('@')
            .ok_or(ControlError::Identity)?
            .1
            .to_owned();
        Ok(Self {
            client: KubernetesClient::new(client, policy.namespace.clone()),
            policy,
            cluster,
            digest,
            compilation,
        })
    }

    fn identity(
        &self,
        job: &CodeJobIdentity,
    ) -> Result<(PodIdentity, Option<String>), ControlError> {
        let identity = PodIdentity {
            name: format!("elitea-code-{}", &job.job_key()[..48]),
            job: job.job_key().into(),
            request: job.request_digest().into(),
        };
        let prefix = format!("kube:{}:{}:", self.cluster, self.policy.namespace);
        let uid = job
            .runtime_id()
            .map(|value| {
                value
                    .strip_prefix(&prefix)
                    .filter(|uid| !uid.is_empty() && !uid.contains(':'))
                    .map(str::to_owned)
                    .ok_or(ControlError::Identity)
            })
            .transpose()?;
        Ok((identity, uid))
    }

    fn bound(&self, job: &CodeJobIdentity) -> Result<(PodIdentity, String), ControlError> {
        let (identity, uid) = self.identity(job)?;
        Ok((identity, uid.ok_or(ControlError::Identity)?))
    }
}

#[async_trait::async_trait]
impl CodeJobRuntime for KubernetesRuntime {
    fn image_digest(&self) -> &str {
        &self.digest
    }
    fn code_compilation_enabled(&self) -> bool {
        self.compilation
    }
    fn code_job_timeout(&self) -> Duration {
        Duration::from_secs(self.policy.timeout_seconds.into())
    }

    async fn instance(&self, job: &CodeJobIdentity) -> Result<Option<String>, SandboxError> {
        let (identity, uid) = self.identity(job).map_err(failure)?;
        let pod = self
            .client
            .observe(&identity, uid.as_deref())
            .await
            .map_err(failure)?;
        Ok(pod
            .and_then(|pod| pod.metadata.uid)
            .map(|uid| format!("kube:{}:{}:{uid}", self.cluster, self.policy.namespace)))
    }
    async fn exists(&self, job: &CodeJobIdentity) -> Result<bool, SandboxError> {
        Ok(self.instance(job).await?.is_some())
    }
    async fn prepare(
        &self,
        job: &CodeJobIdentity,
        manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        let (identity, bound) = self.identity(job).map_err(failure)?;
        if bound.is_some() {
            return Err(failure(ControlError::Identity));
        }
        let (code, runner) = files(manifest).map_err(failure)?;
        let operation = async {
            let mut pod = self.client.create(&self.policy, &identity).await?;
            let uid = pod.metadata.uid.clone().ok_or(ControlError::Identity)?;
            loop {
                if pod
                    .status
                    .as_ref()
                    .and_then(|status| status.container_statuses.as_ref())
                    .is_some_and(|states| {
                        states.iter().any(|state| {
                            state.name == "code"
                                && state
                                    .state
                                    .as_ref()
                                    .is_some_and(|state| state.running.is_some())
                        })
                    })
                {
                    break;
                }
                if self.client.terminated(&identity, &uid).await? {
                    return Err(ControlError::Helper);
                }
                tokio::time::sleep(Duration::from_millis(500)).await;
                pod = self
                    .client
                    .observe(&identity, Some(&uid))
                    .await?
                    .ok_or(ControlError::Identity)?;
            }
            self.client
                .lifecycle(
                    &identity,
                    &uid,
                    LifecycleCommand::Prepare,
                    Some(code),
                    Some(runner),
                )
                .await?;
            Ok(())
        };
        tokio::time::timeout(Duration::from_mins(1), operation)
            .await
            .map_err(|_| failure(ControlError::Timeout))?
            .map_err(failure)
    }
    async fn prepared(&self, job: &CodeJobIdentity) -> Result<bool, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .lifecycle(&identity, &uid, LifecycleCommand::Prepared, None, None)
            .await
            .map_err(failure)
    }
    async fn dispatch(&self, job: &CodeJobIdentity) -> Result<(), SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        if self
            .client
            .terminated(&identity, &uid)
            .await
            .map_err(failure)?
        {
            return Ok(());
        }
        self.client
            .lifecycle(&identity, &uid, LifecycleCommand::Dispatch, None, None)
            .await
            .map_err(failure)?;
        Ok(())
    }
    async fn receipt(&self, job: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client.receipt(&identity, &uid).await.map_err(failure)
    }
    async fn terminate(&self, job: &CodeJobIdentity) -> Result<(), SandboxError> {
        let (identity, bound_uid) = self.identity(job).map_err(failure)?;
        let Some(pod) = self
            .client
            .observe(&identity, bound_uid.as_deref())
            .await
            .map_err(failure)?
        else {
            // No bound runtime means dispatch was never permitted. A known UID
            // disappearing, however, cannot prove that its remote process stopped.
            return if bound_uid.is_none() {
                Ok(())
            } else {
                Err(failure(ControlError::Running))
            };
        };
        let uid = pod
            .metadata
            .uid
            .ok_or_else(|| failure(ControlError::Identity))?;
        self.client
            .request_stop(&identity, &uid)
            .await
            .map_err(failure)?;
        let wait = async {
            while !self.client.terminated(&identity, &uid).await? {
                tokio::time::sleep(Duration::from_millis(250)).await;
            }
            Ok(())
        };
        tokio::time::timeout(Duration::from_secs(15), wait)
            .await
            .map_err(|_| failure(ControlError::Timeout))?
            .map_err(failure)
    }
    async fn cleanup(&self, job: &CodeJobIdentity) -> Result<(), SandboxError> {
        let (identity, bound_uid) = self.identity(job).map_err(failure)?;
        let Some(pod) = self
            .client
            .observe(&identity, bound_uid.as_deref())
            .await
            .map_err(failure)?
        else {
            return Ok(());
        };
        let uid = pod
            .metadata
            .uid
            .ok_or_else(|| failure(ControlError::Identity))?;
        self.client.cleanup(&identity, &uid).await.map_err(failure)
    }
}

fn files(manifest: &Manifest) -> Result<(&str, &str), ControlError> {
    if manifest.entries.len() != 2 {
        return Err(ControlError::Identity);
    }
    let mut code = None;
    let mut job = None;
    for entry in &manifest.entries {
        let ManifestEntry::File { path, content } = entry else {
            return Err(ControlError::Identity);
        };
        let value = std::str::from_utf8(content).map_err(|_| ControlError::Identity)?;
        match path.as_str() {
            ".elitea-code.json" if code.is_none() && value.len() <= 1024 * 1024 => {
                code = Some(value);
            }
            ".elitea-job.json" if job.is_none() && value.len() <= 64 * 1024 => job = Some(value),
            _ => return Err(ControlError::Identity),
        }
    }
    Ok((
        code.ok_or(ControlError::Identity)?,
        job.ok_or(ControlError::Identity)?,
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn runtime() -> KubernetesRuntime {
        let client = kube::Client::new(
            tower::service_fn(|_: http::Request<kube::client::Body>| async {
                Ok::<_, std::convert::Infallible>(http::Response::new(http_body_util::Full::new(
                    bytes::Bytes::from_static(b"{}"),
                )))
            }),
            "execution",
        );
        KubernetesRuntime::new(
            client,
            PodPolicy {
                namespace: "execution".into(),
                image: format!("registry/code@sha256:{}", "a".repeat(64)),
                runtime_class: "sandbox".into(),
                node_selector: std::collections::BTreeMap::from([(
                    "sandbox".into(),
                    "true".into(),
                )]),
                memory_bytes: 512 * 1024 * 1024,
                cpu_millis: 1000,
                timeout_seconds: 60,
            },
            "cluster-a".into(),
            false,
        )
        .unwrap()
    }

    #[tokio::test]
    async fn durable_identity_cannot_move_to_another_cluster_or_namespace() {
        let runtime = runtime();
        let job = CodeJobIdentity::new("a".repeat(64), "b".repeat(64)).unwrap();
        for identity in [
            "kube:cluster-b:execution:uid",
            "kube:cluster-a:other:uid",
            "kube:cluster-a:execution:",
        ] {
            let bound = job.clone().with_runtime_id(identity.into()).unwrap();
            assert!(runtime.identity(&bound).is_err());
        }
        let bound = job
            .with_runtime_id("kube:cluster-a:execution:original".into())
            .unwrap();
        assert_eq!(runtime.bound(&bound).unwrap().1, "original");
    }

    #[test]
    fn preparation_accepts_only_the_two_fixed_files() {
        let manifest = Manifest::new(vec![
            ManifestEntry::File {
                path: ".elitea-code.json".into(),
                content: serde_json::to_vec(&json!({"source":"private"})).unwrap(),
            },
            ManifestEntry::File {
                path: ".elitea-job.json".into(),
                content: b"{}".to_vec(),
            },
        ]);
        assert!(files(&manifest).is_ok());
        let mut malicious = manifest.clone();
        malicious.entries[0] = ManifestEntry::File {
            path: "../escape".into(),
            content: vec![],
        };
        assert!(files(&malicious).is_err());
        let mut duplicate = manifest;
        duplicate.entries[1] = duplicate.entries[0].clone();
        assert!(files(&duplicate).is_err());
    }
}
