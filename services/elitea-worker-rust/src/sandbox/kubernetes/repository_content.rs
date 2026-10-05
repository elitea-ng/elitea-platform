//! Fixed repository init helper. Every command observes the original Pod UID.
use super::{AttachParams, ControlError, KubernetesClient};
use crate::sandbox::{
    kubernetes::{PodIdentity, PodPolicy, workspace},
    runtime_workspace::WorkspaceOperation,
};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

impl KubernetesClient {
    pub(in crate::sandbox::kubernetes) async fn repository_hydrated(
        &self,
        policy: &PodPolicy,
        identity: &PodIdentity,
        uid: &str,
        root: &str,
    ) -> Result<bool, ControlError> {
        let pod = self
            .observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        let value = serde_json::to_value(&pod).map_err(|_| ControlError::Identity)?;
        workspace::validate(&value, identity, &policy.image, root)?;
        Ok(workspace::helper_completed(&value))
    }

    #[allow(
        clippy::too_many_arguments,
        reason = "Keep the existing authenticated identity fields explicit."
    )]
    pub(in crate::sandbox::kubernetes) async fn repository_command(
        &self,
        policy: &PodPolicy,
        identity: &PodIdentity,
        uid: &str,
        root: &str,
        operation: WorkspaceOperation,
        header: &[u8],
        payload: &[u8],
    ) -> Result<Vec<u8>, ControlError> {
        if header.len() > 4096 || payload.len() > 4 << 20 {
            return Err(ControlError::Identity);
        }
        let pod = self
            .observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        let value = serde_json::to_value(&pod).map_err(|_| ControlError::Identity)?;
        workspace::validate(&value, identity, &policy.image, root)?;
        if workspace::helper_completed(&value) {
            return if matches!(operation, WorkspaceOperation::Release) {
                Ok(Vec::new())
            } else {
                Err(ControlError::Helper)
            };
        }
        let active = pod
            .status
            .as_ref()
            .and_then(|v| v.init_container_statuses.as_ref())
            .is_some_and(|values| {
                values.len() == 1
                    && values[0].name == "repository-hydrator"
                    && values[0].restart_count == 0
                    && values[0]
                        .state
                        .as_ref()
                        .is_some_and(|v| v.running.is_some())
            });
        if !active {
            return Err(ControlError::Running);
        }
        let transfer = async {
            let params = AttachParams::default()
                .container("repository-hydrator")
                .stdin(true)
                .stdout(true)
                .stderr(true);
            let mut process = self
                .pods
                .exec(
                    &identity.name,
                    ["/usr/local/bin/elitea-code-runner", operation.command()],
                    &params,
                )
                .await
                .map_err(|_| ControlError::Api)?;
            let mut stdin = process.stdin().ok_or(ControlError::Helper)?;
            let stdout = process.stdout().ok_or(ControlError::Helper)?;
            let stderr = process.stderr().ok_or(ControlError::Helper)?;
            let status = process.take_status().ok_or(ControlError::Helper)?;
            let write = async {
                stdin.write_all(header).await?;
                stdin.write_all(payload).await?;
                stdin.shutdown().await
            };
            let mut out = Vec::new();
            let mut err = Vec::new();
            let mut stdout = stdout.take(1025);
            let mut stderr = stderr.take(1025);
            tokio::try_join!(
                write,
                stdout.read_to_end(&mut out),
                stderr.read_to_end(&mut err)
            )
            .map_err(|_| ControlError::Helper)?;
            let result = status.await.ok_or(ControlError::Helper)?;
            if result.status.as_deref() != Some("Success") || out.len() > 1024 || !err.is_empty() {
                return Err(ControlError::Helper);
            }
            drop(process);
            let current = self
                .observe(identity, Some(uid))
                .await?
                .ok_or(ControlError::Identity)?;
            workspace::validate(
                &serde_json::to_value(&current).map_err(|_| ControlError::Identity)?,
                identity,
                &policy.image,
                root,
            )?;
            Ok(out)
        };
        tokio::time::timeout(std::time::Duration::from_secs(30), transfer)
            .await
            .map_err(|_| ControlError::Timeout)?
    }
}
