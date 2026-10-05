//! Original-container mailbox access. No create, restart, dispatch or generic exec.
use super::*;
use bollard::exec::{CreateExecOptions, StartExecResults};
use futures::StreamExt;
use tokio::io::AsyncWriteExt;

const REQUEST_LIMIT: usize = 8 + 262_144 + 65_536;
const REPLY_LIMIT: usize = 8 + 4096 + 8 + 2_097_152 + 65_536;
fn refused() -> SandboxError {
    SandboxError::ExecutionFailed("Code platform original runtime requires reconciliation".into())
}
fn input(identity: &CodeJobIdentity, launch: &[u8], body: &[u8]) -> Result<Vec<u8>, SandboxError> {
    if launch.is_empty() || launch.len() > 4096 || body.len() > REPLY_LIMIT {
        return Err(refused());
    }
    let value: serde_json::Value = serde_json::from_slice(launch).map_err(|_| refused())?;
    if value["retained_runtime_id"].as_str() != identity.runtime_id()
        || launch_request(&value) != Some(identity.request_digest())
    {
        return Err(refused());
    }
    let mut out = Vec::with_capacity(8 + launch.len() + body.len());
    out.extend_from_slice(&(launch.len() as u32).to_be_bytes());
    out.extend_from_slice(&(body.len() as u32).to_be_bytes());
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
impl DockerClient {
    async fn platform_helper(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
        reply: Option<&[u8]>,
        bind: bool,
    ) -> Result<Vec<u8>, SandboxError> {
        if !self.code_platform_profile_enabled() {
            return Err(refused());
        }
        let expected = identity.runtime_id().ok_or_else(refused)?;
        let before = self.observe_code_job(identity).await?.ok_or_else(refused)?;
        if before.container_id != expected || !before.running {
            return Err(refused());
        }
        let payload = input(identity, launch, reply.unwrap_or_default())?;
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
            let exec = self
                .client
                .create_exec(
                    expected,
                    CreateExecOptions::<String> {
                        cmd: Some(vec![
                            "/usr/local/bin/elitea-code-execute".into(),
                            command.into(),
                        ]),
                        attach_stdin: Some(true),
                        attach_stdout: Some(true),
                        attach_stderr: Some(true),
                        ..Default::default()
                    },
                )
                .await
                .map_err(|_| refused())?;
            let StartExecResults::Attached {
                mut output,
                mut input,
            } = self
                .client
                .start_exec(&exec.id, None)
                .await
                .map_err(|_| refused())?
            else {
                return Err(refused());
            };
            let write = async {
                input.write_all(&payload).await.map_err(|_| refused())?;
                input.shutdown().await.map_err(|_| refused())
            };
            let read = async {
                let mut out = Vec::new();
                let mut error_bytes = 0_usize;
                while let Some(value) = output.next().await {
                    match value.map_err(|_| refused())? {
                        bollard::container::LogOutput::StdOut { message } => {
                            if message.len() > limit.saturating_sub(out.len()) {
                                return Err(refused());
                            }
                            out.extend_from_slice(&message);
                        }
                        bollard::container::LogOutput::StdErr { message } => {
                            if message.len() > 8192_usize.saturating_sub(error_bytes) {
                                return Err(refused());
                            }
                            error_bytes += message.len();
                        }
                        _ => return Err(refused()),
                    }
                }
                Ok(out)
            };
            let (_, out) = tokio::try_join!(write, read)?;
            let status = self
                .client
                .inspect_exec(&exec.id)
                .await
                .map_err(|_| refused())?;
            if status.running != Some(false)
                || status.exit_code != Some(0)
                || status.container_id.as_deref() != Some(expected)
            {
                return Err(refused());
            }
            Ok(out)
        };
        // One bounded attempt. A timeout does not authorize a reply resend/effect retry.
        let out = tokio::time::timeout(Duration::from_secs(15), operation)
            .await
            .map_err(|_| refused())??;
        let after = self.observe_code_job(identity).await?.ok_or_else(refused)?;
        if after.container_id != expected || !after.running {
            return Err(refused());
        }
        Ok(out)
    }
    pub async fn bind_code_platform_launch(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<(), SandboxError> {
        self.platform_helper(identity, launch, None, true)
            .await
            .map(|_| ())
    }
    pub async fn read_code_platform_call(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        let value = self.platform_helper(identity, launch, None, false).await?;
        Ok(if value.is_empty() { None } else { Some(value) })
    }
    pub async fn publish_code_platform_reply(
        &self,
        identity: &CodeJobIdentity,
        launch: &[u8],
        signed_reply: &[u8],
    ) -> Result<(), SandboxError> {
        self.platform_helper(identity, launch, Some(signed_reply), false)
            .await
            .map(|_| ())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn input_binds_exact_original_runtime_and_request_with_binary_bytes() {
        let identity = CodeJobIdentity::new("a".repeat(64), "b".repeat(64))
            .unwrap()
            .with_runtime_id("original".into())
            .unwrap();
        let launch = serde_json::to_vec(
            &serde_json::json!({"revision":1,"retained_runtime_id":"original","prepared_sha256":"b".repeat(64)}),
        )
        .unwrap();
        let frame = input(&identity, &launch, &[0, 255, 7]).unwrap();
        assert_eq!(&frame[8 + launch.len()..], &[0, 255, 7]);
        let compiled = serde_json::to_vec(&serde_json::json!({"revision":2,"retained_runtime_id":"original","prepared_sha256":"c".repeat(64),"request_digest":"b".repeat(64)})).unwrap();
        assert!(input(&identity, &compiled, &[]).is_ok());
        let foreign = serde_json::to_vec(&serde_json::json!({"revision":2,"retained_runtime_id":"original","prepared_sha256":"b".repeat(64),"request_digest":"c".repeat(64)})).unwrap();
        assert!(input(&identity, &foreign, &[]).is_err());
        let replacement = identity.clone().with_runtime_id("replacement".into());
        assert!(replacement.is_err());
        let wrong=serde_json::to_vec(&serde_json::json!({"revision":1,"retained_runtime_id":"replacement","prepared_sha256":"b".repeat(64)})).unwrap();
        assert!(input(&identity, &wrong, &[]).is_err());
        assert!(input(&identity, &launch, &vec![0; REPLY_LIMIT + 1]).is_err());
    }
}
