//! Fixed original-Pod broker helpers. Never provisions or restarts Code.
#[allow(
    clippy::wildcard_imports,
    reason = "Share the owner module imports with runtime code and its existing tests."
)]
use super::*;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
const REQUEST_LIMIT: usize = 8 + 262_144 + 65_536;
const REPLY_LIMIT: usize = 8 + 4096 + 8 + 2_097_152 + 65_536;
fn input(
    identity: &PodIdentity,
    uid: &str,
    launch: &[u8],
    body: &[u8],
) -> Result<Vec<u8>, ControlError> {
    if uid.is_empty() || launch.is_empty() || launch.len() > 4096 || body.len() > REPLY_LIMIT {
        return Err(ControlError::Identity);
    }
    let value: serde_json::Value =
        serde_json::from_slice(launch).map_err(|_| ControlError::Identity)?;
    if value["retained_runtime_id"].as_str().is_none_or(|runtime| {
        !runtime.starts_with("kube:")
            || runtime.rsplit(':').next() != Some(uid)
            || runtime.split(':').count() != 4
    }) || launch_request(&value) != Some(identity.request.as_str())
    {
        return Err(ControlError::Identity);
    }
    let launch_length = u32::try_from(launch.len()).map_err(|_| ControlError::Identity)?;
    let body_length = u32::try_from(body.len()).map_err(|_| ControlError::Identity)?;
    let mut out = Vec::with_capacity(8 + launch.len() + body.len());
    out.extend_from_slice(&launch_length.to_be_bytes());
    out.extend_from_slice(&body_length.to_be_bytes());
    out.extend_from_slice(launch);
    out.extend_from_slice(body);
    Ok(out)
}
// Plain launch bytes stay revision1. Compiled revision2 separates the
// actual original Execute request from its prepared-job fingerprint.
fn launch_request(value: &serde_json::Value) -> Option<&str> {
    match value["revision"].as_u64()? {
        1 if value.get("request_digest").is_none() => value["prepared_sha256"].as_str(),
        2 => {
            let request = value["request_digest"].as_str()?;
            let prepared = value["prepared_sha256"].as_str()?;
            let hex = |v: &str| {
                v.len() == 64
                    && v.bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            };
            if !hex(request) || !hex(prepared) || request == prepared {
                return None;
            }
            Some(request)
        }
        _ => None,
    }
}
impl KubernetesClient {
    async fn platform_helper(
        &self,
        identity: &PodIdentity,
        uid: &str,
        launch: &[u8],
        reply: Option<&[u8]>,
        bind: bool,
    ) -> Result<Vec<u8>, ControlError> {
        self.observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        let payload = input(identity, uid, launch, reply.unwrap_or_default())?;
        let command = if bind {
            "--platform-bind"
        } else if reply.is_some() {
            "--platform-reply"
        } else {
            "--platform-read"
        };
        let limit = if command == "--platform-read" {
            REQUEST_LIMIT
        } else {
            0
        };
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
                    ["/usr/local/bin/elitea-code-execute", command],
                    &params,
                )
                .await
                .map_err(|_| ControlError::Api)?;
            let mut stdin = process.stdin().ok_or(ControlError::Helper)?;
            let mut stdout = process
                .stdout()
                .ok_or(ControlError::Helper)?
                .take(limit as u64 + 1);
            let mut stderr = process.stderr().ok_or(ControlError::Helper)?.take(8193);
            let status = process.take_status().ok_or(ControlError::Helper)?;
            let write = async {
                stdin.write_all(&payload).await?;
                stdin.shutdown().await
            };
            let mut out = Vec::new();
            let mut error = Vec::new();
            tokio::try_join!(
                write,
                stdout.read_to_end(&mut out),
                stderr.read_to_end(&mut error)
            )
            .map_err(|_| ControlError::Helper)?;
            if out.len() > limit || error.len() > 8192 {
                return Err(ControlError::Helper);
            }
            let status = status.await.ok_or(ControlError::Helper)?;
            if status.status.as_deref() != Some("Success") {
                return Err(ControlError::Helper);
            }
            drop(process);
            Ok(out)
        };
        let out = tokio::time::timeout(API_TIMEOUT, operation)
            .await
            .map_err(|_| ControlError::Timeout)??;
        self.observe(identity, Some(uid))
            .await?
            .ok_or(ControlError::Identity)?;
        Ok(out)
    }
    pub(crate) async fn bind_code_platform_launch(
        &self,
        identity: &PodIdentity,
        uid: &str,
        launch: &[u8],
    ) -> Result<(), ControlError> {
        self.platform_helper(identity, uid, launch, None, true)
            .await
            .map(|_| ())
    }
    pub(crate) async fn read_code_platform_call(
        &self,
        identity: &PodIdentity,
        uid: &str,
        launch: &[u8],
    ) -> Result<Option<Vec<u8>>, ControlError> {
        let out = self
            .platform_helper(identity, uid, launch, None, false)
            .await?;
        Ok(if out.is_empty() { None } else { Some(out) })
    }
    pub(crate) async fn publish_code_platform_reply(
        &self,
        identity: &PodIdentity,
        uid: &str,
        launch: &[u8],
        signed_reply: &[u8],
    ) -> Result<(), ControlError> {
        self.platform_helper(identity, uid, launch, Some(signed_reply), false)
            .await
            .map(|_| ())
    }
}

#[cfg(test)]
mod launch_tests {
    use super::*;
    #[test]
    fn plain_and_compiled_launch_keep_distinct_request_identity() {
        let request = "a".repeat(64);
        let prepared = "b".repeat(64);
        let legacy = serde_json::json!({"revision":1,"prepared_sha256":request});
        assert_eq!(launch_request(&legacy), Some(request.as_str()));
        let compiled =
            serde_json::json!({"revision":2,"prepared_sha256":prepared,"request_digest":request});
        assert_eq!(launch_request(&compiled), Some(request.as_str()));
        for invalid in [
            serde_json::json!({"revision":1,"prepared_sha256":prepared,"request_digest":request}),
            serde_json::json!({"revision":2,"prepared_sha256":prepared}),
            serde_json::json!({"revision":2,"prepared_sha256":request,"request_digest":request}),
            serde_json::json!({"revision":2,"prepared_sha256":prepared,"request_digest":null}),
        ] {
            assert!(launch_request(&invalid).is_none());
        }
    }
}
