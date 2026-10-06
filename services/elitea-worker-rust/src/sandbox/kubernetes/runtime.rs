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
    code_platform_profile: bool,
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
            code_platform_profile: false,
        })
    }

    /// Enable only a measured operator-owned broker image/wrapper/key profile.
    /// Omission preserves the existing runtime configuration and denial.
    /// # Errors
    /// Returns `ControlError::Identity` if the current workload policy is invalid.
    pub fn with_code_platform_profile(mut self) -> Result<Self, ControlError> {
        self.policy
            .workload(&PodIdentity::new(&[0; 32], &[0; 32]))?;
        self.code_platform_profile = true;
        Ok(self)
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
    async fn prepare_inner(
        &self,
        job: &CodeJobIdentity,
        manifest: &Manifest,
        launch: Option<(&str, &str, &str)>,
    ) -> Result<(), SandboxError> {
        let (identity, bound) = self.identity(job).map_err(failure)?;
        if bound.is_some() {
            return Err(failure(ControlError::Identity));
        }
        let (code, runner) = files(manifest).map_err(failure)?;
        let operation = async {
            let mut pod = self
                .client
                .create_with_compiled_launch(&self.policy, &identity, launch)
                .await?;
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

    async fn prepare_repository(
        &self,
        job: &CodeJobIdentity,
        manifest: &Manifest,
        root: &str,
        launch: Option<(&str, &str, &str)>,
    ) -> Result<(), SandboxError> {
        crate::sandbox::runtime_workspace::validate_launch_manifest(manifest)?;
        let (identity, bound) = self.identity(job).map_err(failure)?;
        if bound.is_some() {
            return Err(failure(ControlError::Identity));
        }
        // Persist this Pod's UID before any repository bytes or launch files are sent.
        self.client
            .create_with_repository(&self.policy, &identity, launch, Some(root))
            .await
            .map_err(failure)?;
        Ok(())
    }
}

#[async_trait::async_trait]
impl CodeJobRuntime for KubernetesRuntime {
    fn retained_code_platform_kind(
        &self,
    ) -> Option<crate::sandbox::code_platform_owner::RetainedRuntimeKind> {
        self.code_platform_profile
            .then_some(crate::sandbox::code_platform_owner::RetainedRuntimeKind::Kubernetes)
    }
    async fn bind_code_platform_launch(
        &self,
        job: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<(), SandboxError> {
        if !self.code_platform_profile {
            return Err(failure(ControlError::Identity));
        }
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .bind_code_platform_launch(&identity, &uid, launch)
            .await
            .map_err(failure)
    }
    async fn validate_code_platform_launch(
        &self,
        job: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<(), SandboxError> {
        self.read_code_platform_call(job, launch).await.map(|_| ())
    }
    async fn read_code_platform_call(
        &self,
        job: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        if !self.code_platform_profile {
            return Err(failure(ControlError::Identity));
        }
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .read_code_platform_call(&identity, &uid, launch)
            .await
            .map_err(failure)
    }
    async fn publish_code_platform_reply(
        &self,
        job: &CodeJobIdentity,
        launch: &[u8],
        reply: &[u8],
    ) -> Result<(), SandboxError> {
        if !self.code_platform_profile {
            return Err(failure(ControlError::Identity));
        }
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .publish_code_platform_reply(&identity, &uid, launch, reply)
            .await
            .map_err(failure)
    }

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
    async fn preparation_marker(
        &self,
        job: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .preparation_marker(&identity, &uid)
            .await
            .map_err(failure)
    }
    async fn export_dependency(
        &self,
        job: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .export_dependency(&identity, &uid, name, bytes, writer)
            .await
            .map_err(failure)
    }
    async fn export_execution_dependency(
        &self,
        job: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .export_execution_dependency(&identity, &uid, name, bytes, writer)
            .await
            .map_err(failure)
    }
    async fn import_dependency(
        &self,
        job: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
    ) -> Result<(), SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .import_dependency(&identity, &uid, name, bytes, reader)
            .await
            .map_err(failure)
    }
    async fn bundle_preparation_marker(
        &self,
        job: &CodeJobIdentity,
        native: bool,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .preparation_marker_kind(&identity, &uid, native)
            .await
            .map_err(failure)
    }
    async fn native_transfer(
        &self,
        job: &CodeJobIdentity,
        kind: &str,
        root: &str,
        name: &str,
        bytes: u64,
        operation: crate::sandbox::runtime::NativeContentOperation,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .native_dependency(
                &identity,
                &uid,
                kind,
                root,
                name,
                bytes,
                operation.command(),
                matches!(
                    operation,
                    crate::sandbox::runtime::NativeContentOperation::Import
                ),
                reader,
                writer,
            )
            .await
            .map_err(failure)
    }
    async fn release_preparation(&self, job: &CodeJobIdentity) -> Result<(), SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .release_preparation(&identity, &uid)
            .await
            .map_err(failure)
    }
    async fn prepare(
        &self,
        job: &CodeJobIdentity,
        manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        self.prepare_inner(job, manifest, None).await
    }
    async fn prepare_workspace(
        &self,
        job: &CodeJobIdentity,
        manifest: &Manifest,
        root: &str,
    ) -> Result<(), SandboxError> {
        self.prepare_repository(job, manifest, root, None).await
    }
    async fn prepare_compiled_workspace(
        &self,
        job: &CodeJobIdentity,
        manifest: &Manifest,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
        root: &str,
    ) -> Result<(), SandboxError> {
        if !self.compilation {
            return Err(failure(ControlError::Identity));
        }
        self.prepare_repository(
            job,
            manifest,
            root,
            Some((purpose, control_sha256, policy_revision)),
        )
        .await
    }
    async fn workspace_command(
        &self,
        job: &CodeJobIdentity,
        root: &str,
        operation: crate::sandbox::runtime_workspace::WorkspaceOperation,
        index: Option<u32>,
        payload: &[u8],
    ) -> Result<Vec<u8>, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        let header = crate::sandbox::runtime_workspace::header(
            job,
            root,
            operation,
            index,
            payload.len(),
            Some(&uid),
        )?;
        self.client
            .repository_command(
                &self.policy,
                &identity,
                &uid,
                root,
                operation,
                &header,
                payload,
            )
            .await
            .map_err(failure)
    }
    async fn workspace_hydrated(
        &self,
        job: &CodeJobIdentity,
        root: &str,
    ) -> Result<bool, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .repository_hydrated(&self.policy, &identity, &uid, root)
            .await
            .map_err(failure)
    }
    async fn workspace_prepared(
        &self,
        job: &CodeJobIdentity,
        root: &str,
    ) -> Result<bool, SandboxError> {
        if !self.workspace_hydrated(job, root).await? {
            return Ok(false);
        }
        self.prepared(job).await
    }
    async fn complete_workspace(
        &self,
        job: &CodeJobIdentity,
        root: &str,
        manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        if !self.workspace_hydrated(job, root).await? {
            return Err(failure(ControlError::Running));
        }
        let (identity, uid) = self.bound(job).map_err(failure)?;
        let (code, runner) = files(manifest).map_err(failure)?;
        let operation = async {
            loop {
                let pod = self
                    .client
                    .observe(&identity, Some(&uid))
                    .await?
                    .ok_or(ControlError::Identity)?;
                let value = serde_json::to_value(&pod).map_err(|_| ControlError::Identity)?;
                super::workspace::validate(&value, &identity, &self.policy.image, root)?;
                if !super::workspace::helper_completed(&value) {
                    return Err(ControlError::Identity);
                }
                if pod
                    .status
                    .as_ref()
                    .and_then(|v| v.container_statuses.as_ref())
                    .is_some_and(|states| {
                        states.len() == 1
                            && states[0].name == "code"
                            && states[0]
                                .state
                                .as_ref()
                                .is_some_and(|state| state.running.is_some())
                    })
                {
                    break;
                }
                if self.client.terminated(&identity, &uid).await? {
                    return Err(ControlError::Helper);
                }
                tokio::time::sleep(Duration::from_millis(250)).await;
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
    async fn workspace_volume_identity(
        &self,
        job: &CodeJobIdentity,
        root: &str,
    ) -> Result<crate::sandbox::runtime_workspace::WorkspaceVolumeIdentity, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        let pod = self
            .client
            .observe(&identity, Some(&uid))
            .await
            .map_err(failure)?
            .ok_or_else(|| failure(ControlError::Identity))?;
        super::workspace::validate(
            &serde_json::to_value(&pod).map_err(|_| failure(ControlError::Identity))?,
            &identity,
            &self.policy.image,
            root,
        )
        .map_err(|_| failure(ControlError::Identity))?;
        Ok(crate::sandbox::runtime_workspace::WorkspaceVolumeIdentity {
            backend: crate::sandbox::ledger::WorkspaceBackend::Kubernetes,
            volume_name: "repository".into(),
            owner_token: uid.clone(),
            creation_token: uid,
        })
    }
    async fn cleanup_workspace(
        &self,
        job: &CodeJobIdentity,
        receipt: &crate::sandbox::ledger::WorkspaceRuntimeReceipt,
    ) -> Result<(), SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        if receipt.backend != crate::sandbox::ledger::WorkspaceBackend::Kubernetes
            || receipt.original_runtime_id
                != job
                    .runtime_id()
                    .ok_or_else(|| failure(ControlError::Identity))?
            || receipt.volume_name != "repository"
            || receipt.volume_owner_token != uid
            || receipt.volume_creation_token != uid
            || receipt.job_key != identity.job
            || receipt.request_digest != identity.request
        {
            return Err(failure(ControlError::Identity));
        }
        if let Some(pod) = self
            .client
            .observe(&identity, Some(&uid))
            .await
            .map_err(failure)?
        {
            super::workspace::validate(
                &serde_json::to_value(&pod).map_err(|_| failure(ControlError::Identity))?,
                &identity,
                &self.policy.image,
                &receipt.manifest_sha256,
            )
            .map_err(|_| failure(ControlError::Identity))?;
            self.client
                .cleanup(&identity, &uid)
                .await
                .map_err(failure)?;
        }
        Ok(())
    }
    async fn prepare_compiled(
        &self,
        job: &CodeJobIdentity,
        manifest: &Manifest,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
    ) -> Result<(), SandboxError> {
        if !self.compilation {
            return Err(failure(ControlError::Identity));
        }
        self.prepare_inner(
            job,
            manifest,
            Some((purpose, control_sha256, policy_revision)),
        )
        .await
    }
    async fn validate_compiled_launch(
        &self,
        job: &CodeJobIdentity,
        purpose: &str,
        control_sha256: &str,
        policy_revision: &str,
    ) -> Result<(), SandboxError> {
        if !self.compilation {
            return Err(failure(ControlError::Identity));
        }
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .verify_compiled_launch(
                &self.policy,
                &identity,
                &uid,
                (purpose, control_sha256, policy_revision),
            )
            .await
            .map_err(failure)
    }
    async fn compiled_descriptor_probe(
        &self,
        job: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        let (identity, uid) = self.bound(job).map_err(failure)?;
        self.client
            .compiled_descriptor_probe(&identity, &uid)
            .await
            .map_err(failure)
    }
    async fn compiled_transfer(
        &self,
        job: &CodeJobIdentity,
        operation: crate::sandbox::compiled_snapshot::SnapshotOperation,
        control_json: &[u8],
        bytes: u64,
        sha256: Option<&str>,
        reader: &mut (dyn tokio::io::AsyncRead + Unpin + Send),
        writer: &mut (dyn tokio::io::AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        use crate::sandbox::compiled_snapshot::{
            ContentSha256, Control, Purpose, SnapshotOperation,
        };
        let (identity, uid) = self.bound(job).map_err(failure)?;
        let control: Control =
            serde_json::from_slice(control_json).map_err(|_| failure(ControlError::Identity))?;
        control
            .validate(if control.descriptor_sha256.is_some() {
                Purpose::Execute
            } else {
                Purpose::Compile
            })
            .map_err(|_| failure(ControlError::Identity))?;
        self.validate_compiled_launch(
            job,
            if control.descriptor_sha256.is_some() {
                "execute"
            } else {
                "compile"
            },
            ContentSha256::of(control_json).as_str(),
            &control.binding.policy_revision,
        )
        .await?;
        let hash = sha256
            .map(|value| ContentSha256::parse(value.into()))
            .transpose()
            .map_err(|_| failure(ControlError::Identity))?;
        let header = operation
            .header(
                &control,
                if matches!(operation, SnapshotOperation::Status) {
                    0
                } else {
                    bytes
                },
                hash.as_ref(),
                Some((&uid, &identity.request)),
            )
            .map_err(|_| failure(ControlError::Identity))?;
        self.client
            .compiled_transfer(
                &identity,
                &uid,
                operation.command(),
                &header,
                bytes,
                reader,
                writer,
                operation.importing(),
            )
            .await
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
                workspace_bytes: 256 * 1024 * 1024,
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
