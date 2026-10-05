//! Identity-bound binary exec. The helper rechecks its own Pod UID before file access.
use super::{API_TIMEOUT, ControlError, KubernetesClient, PodIdentity};
use crate::sandbox::dependency_bundle::valid_digest;
use kube::api::AttachParams;
use std::time::Duration;
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite, AsyncWriteExt as _};

const FILE_LIMIT: u64 = 32 * 1024 * 1024;

fn content_header(
    identity: &PodIdentity,
    uid: &str,
    name: &str,
    bytes: u64,
) -> Result<Vec<u8>, ControlError> {
    if uid.is_empty()
        || uid.len() > 512
        || name.is_empty()
        || name.len() > 256
        || matches!(name, "." | "..")
        || !name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'+' | b'-'))
        || bytes > FILE_LIMIT
    {
        return Err(ControlError::Identity);
    }
    let mut bytes = serde_json::to_vec(&serde_json::json!({
        "name": name, "bytes": bytes, "pod_uid": uid, "request_digest": identity.request,
    }))
    .map_err(|_| ControlError::Identity)?;
    bytes.push(b'\n');
    Ok(bytes)
}

impl KubernetesClient {
    pub(crate) async fn preparation_marker(
        &self,
        identity: &PodIdentity,
        uid: &str,
    ) -> Result<Option<Vec<u8>>, ControlError> {
        self.preparation_marker_kind(identity, uid, false).await
    }
    pub(crate) async fn preparation_marker_kind(
        &self,
        identity: &PodIdentity,
        uid: &str,
        native: bool,
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
                    [
                        "cat",
                        if native {
                            "/workspace/.elitea-native-preparation.json"
                        } else {
                            "/workspace/.elitea-python-preparation.json"
                        },
                    ],
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
                .take(256 * 1024 + 1);
            let mut stderr = process.stderr().ok_or(ControlError::Helper)?.take(8193);
            let status = process.take_status().ok_or(ControlError::Helper)?;
            let mut out = Vec::new();
            let mut err = Vec::new();
            tokio::try_join!(stdout.read_to_end(&mut out), stderr.read_to_end(&mut err))
                .map_err(|_| ControlError::Helper)?;
            let status = status.await.ok_or(ControlError::Helper)?;
            if out.len() > 256 * 1024 || err.len() > 8192 {
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

    pub(crate) async fn export_dependency(
        &self,
        identity: &PodIdentity,
        uid: &str,
        name: &str,
        bytes: u64,
        writer: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<(), ControlError> {
        let header = content_header(identity, uid, name, bytes)?;
        self.content_exec(
            identity,
            uid,
            "--dependency-read",
            &header,
            bytes,
            &mut tokio::io::empty(),
            writer,
            false,
        )
        .await
    }
    pub(crate) async fn export_execution_dependency(
        &self,
        identity: &PodIdentity,
        uid: &str,
        name: &str,
        bytes: u64,
        writer: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<(), ControlError> {
        let header = content_header(identity, uid, name, bytes)?;
        self.content_exec(
            identity,
            uid,
            "--execution-dependency-read",
            &header,
            bytes,
            &mut tokio::io::empty(),
            writer,
            false,
        )
        .await
    }
    pub(crate) async fn import_dependency(
        &self,
        identity: &PodIdentity,
        uid: &str,
        name: &str,
        bytes: u64,
        reader: &mut (dyn AsyncRead + Unpin + Send),
    ) -> Result<(), ControlError> {
        let header = content_header(identity, uid, name, bytes)?;
        self.content_exec(
            identity,
            uid,
            "--dependency-write",
            &header,
            bytes,
            reader,
            &mut tokio::io::sink(),
            true,
        )
        .await
    }
    pub(crate) async fn release_preparation(
        &self,
        identity: &PodIdentity,
        uid: &str,
    ) -> Result<(), ControlError> {
        let mut header = serde_json::to_vec(
            &serde_json::json!({"pod_uid":uid,"request_digest":identity.request}),
        )
        .map_err(|_| ControlError::Identity)?;
        header.push(b'\n');
        self.content_exec(
            identity,
            uid,
            "--preparation-release",
            &header,
            0,
            &mut tokio::io::empty(),
            &mut tokio::io::sink(),
            false,
        )
        .await
    }

    #[allow(clippy::too_many_arguments)] // Bind the original Pod and both bounded binary streams.
    pub(crate) async fn native_dependency(
        &self,
        identity: &PodIdentity,
        uid: &str,
        kind: &str,
        root: &str,
        name: &str,
        bytes: u64,
        command: &str,
        importing: bool,
        reader: &mut (dyn AsyncRead + Unpin + Send),
        writer: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<(), ControlError> {
        if !matches!(kind, "deno" | "cargo")
            || !valid_digest(root)
            || bytes > 128 * 1024 * 1024
            || !(name == "elitea-native-bundle-v2.json"
                || name == "elitea-native-ready-v2.json"
                || name.strip_suffix(".blob").is_some_and(valid_digest))
        {
            return Err(ControlError::Identity);
        }
        let mut header=serde_json::to_vec(&serde_json::json!({"revision":2,"kind":kind,"root":root,"name":name,"bytes":bytes,"pod_uid":uid,"request_digest":identity.request})).map_err(|_|ControlError::Identity)?;
        header.push(b'\n');
        if !matches!(
            command,
            "--native-dependency-read"
                | "--native-execution-dependency-read"
                | "--native-dependency-write"
                | "--native-dependency-finalize"
                | "--native-preparation-release"
        ) || importing != (command == "--native-dependency-write")
        {
            return Err(ControlError::Identity);
        }
        self.content_exec(
            identity, uid, command, &header, bytes, reader, writer, importing,
        )
        .await
    }
    #[allow(clippy::too_many_arguments)] // The fixed helper owns both binary stream directions.
    pub(super) async fn content_exec(
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
        self.observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
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
                    ["/usr/local/bin/elitea-code-runner", command],
                    &params,
                )
                .await
                .map_err(|_| ControlError::Api)?;
            let mut stdin = process.stdin().ok_or(ControlError::Helper)?;
            let mut stdout = process.stdout().ok_or(ControlError::Helper)?;
            let mut stderr = process.stderr().ok_or(ControlError::Helper)?.take(8193);
            let status = process.take_status().ok_or(ControlError::Helper)?;
            let send = async {
                stdin.write_all(header).await?;
                if importing {
                    let mut remaining = bytes;
                    let mut buffer = vec![0; 64 * 1024];
                    while remaining != 0 {
                        let bound = buffer
                            .len()
                            .min(usize::try_from(remaining).unwrap_or(usize::MAX));
                        let count = reader.read(&mut buffer[..bound]).await?;
                        if count == 0 {
                            return Err(std::io::Error::other("dependency stream is truncated"));
                        }
                        stdin.write_all(&buffer[..count]).await?;
                        remaining -= count as u64;
                    }
                    if reader.read(&mut buffer[..1]).await? != 0 {
                        return Err(std::io::Error::other("dependency stream exceeds its bound"));
                    }
                }
                stdin.shutdown().await
            };
            let receive = async {
                let mut remaining = if importing { 0 } else { bytes };
                let mut buffer = vec![0; 64 * 1024];
                loop {
                    let bound = buffer
                        .len()
                        .min(usize::try_from(remaining.saturating_add(1)).unwrap_or(usize::MAX));
                    let count = stdout.read(&mut buffer[..bound]).await?;
                    if count == 0 {
                        break;
                    }
                    if count as u64 > remaining {
                        return Err(std::io::Error::other("dependency output exceeds its bound"));
                    }
                    writer.write_all(&buffer[..count]).await?;
                    remaining -= count as u64;
                }
                if remaining != 0 {
                    return Err(std::io::Error::other("dependency output is truncated"));
                }
                writer.flush().await
            };
            let mut err = Vec::new();
            tokio::try_join!(send, receive, stderr.read_to_end(&mut err))
                .map_err(|_| ControlError::Helper)?;
            let status = status.await.ok_or(ControlError::Helper)?;
            if !err.is_empty() || status.status.as_deref() != Some("Success") {
                return Err(ControlError::Helper);
            }
            self.observe(identity, Some(uid))
                .await?
                .ok_or(ControlError::Identity)?;
            drop(process); // Own the transport until its status is observed.
            Ok(())
        };
        tokio::time::timeout(Duration::from_secs(30), operation)
            .await
            .map_err(|_| ControlError::Timeout)?
    }
}

#[cfg(test)]
mod tests {
    use super::{FILE_LIMIT, PodIdentity, content_header};
    #[test]
    fn binary_header_carries_exact_uid_without_caller_paths() {
        let identity = PodIdentity::new(&[1; 32], &[2; 32]);
        let bytes = content_header(&identity, "original", "package.whl", 12).unwrap();
        let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(value["pod_uid"], "original");
        assert_eq!(value["request_digest"], identity.request);
        for name in ["../file", "/file", "a/b", "..", "a\"b"] {
            assert!(content_header(&identity, "original", name, 12).is_err());
        }
        assert!(content_header(&identity, "original", "package.whl", FILE_LIMIT + 1).is_err());
    }
}
