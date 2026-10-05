//! Fixed Rust snapshot content helpers, fenced to the original Pod UID.
use super::{API_TIMEOUT, ControlError, KubernetesClient, PodIdentity};
use kube::api::AttachParams;
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite};
impl KubernetesClient {
    pub(crate) async fn compiled_descriptor_probe(
        &self,
        identity: &PodIdentity,
        uid: &str,
    ) -> Result<Option<Vec<u8>>, ControlError> {
        self.observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        if self.terminated(identity, uid).await? {
            return Ok(None);
        }
        let operation = async {
            let mut process = self
                .pods
                .exec(
                    &identity.name,
                    ["cat", "/workspace/compiled-snapshot/descriptor.json"],
                    &AttachParams::default()
                        .container("code")
                        .stdout(true)
                        .stderr(true),
                )
                .await
                .map_err(|_| ControlError::Api)?;
            let mut stdout = process
                .stdout()
                .ok_or(ControlError::Helper)?
                .take(16 * 1024 + 1);
            let mut stderr = process.stderr().ok_or(ControlError::Helper)?.take(8193);
            let status = process.take_status().ok_or(ControlError::Helper)?;
            let mut out = Vec::new();
            let mut err = Vec::new();
            tokio::try_join!(stdout.read_to_end(&mut out), stderr.read_to_end(&mut err))
                .map_err(|_| ControlError::Helper)?;
            let status = status.await.ok_or(ControlError::Helper)?;
            if out.len() > 16 * 1024 || err.len() > 8192 {
                return Err(ControlError::Helper);
            }
            self.observe(identity, Some(uid))
                .await?
                .ok_or(ControlError::Identity)?;
            if status.status.as_deref() != Some("Success") {
                return Ok(None);
            }
            if !err.is_empty() {
                return Err(ControlError::Helper);
            }
            drop(process);
            Ok(Some(out))
        };
        tokio::time::timeout(API_TIMEOUT, operation)
            .await
            .map_err(|_| ControlError::Timeout)?
    }

    #[allow(clippy::too_many_arguments)]
    pub(crate) async fn compiled_transfer(
        &self,
        identity: &PodIdentity,
        uid: &str,
        command: &str,
        header: &[u8],
        bytes: u64,
        reader: &mut (dyn AsyncRead + Unpin + Send),
        writer: &mut (dyn AsyncWrite + Unpin + Send),
        importing: bool,
    ) -> Result<(), ControlError> {
        if header.len() > 4096
            || bytes > 32 * 1024 * 1024
            || !matches!(
                command,
                "--compiled-control-write"
                    | "--compiled-artifact-write"
                    | "--compiled-artifact-finalize"
                    | "--compiled-artifact-status"
                    | "--compiled-artifact-read"
                    | "--compiled-artifact-release"
            )
            || importing
                != matches!(
                    command,
                    "--compiled-control-write" | "--compiled-artifact-write"
                )
        {
            return Err(ControlError::Identity);
        }
        self.content_exec(
            identity, uid, command, header, bytes, reader, writer, importing,
        )
        .await
    }
}
