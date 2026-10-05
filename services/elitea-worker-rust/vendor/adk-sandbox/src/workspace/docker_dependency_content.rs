//! Fixed, identity-bound binary helpers. Package bytes do not use text capture.
use super::CodeJobIdentity;
use super::*;
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite, AsyncWriteExt as _};

const FILE_LIMIT: u64 = 32 * 1024 * 1024;

fn failed() -> SandboxError {
    SandboxError::ExecutionFailed(
        "Code dependency transfer failed; reconcile the same runtime and content".into(),
    )
}

fn header(name: &str, bytes: u64) -> Result<Vec<u8>, SandboxError> {
    if name.is_empty()
        || name.len() > 256
        || matches!(name, "." | "..")
        || !name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'+' | b'-'))
        || bytes > FILE_LIMIT
    {
        return Err(failed());
    }
    let mut header = format!("{{\"name\":\"{name}\",\"bytes\":{bytes}}}").into_bytes();
    header.push(b'\n');
    Ok(header)
}

impl DockerClient {
    /// Read the bounded success marker while the original preparer retains its files.
    pub async fn read_code_preparation_marker(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        if identity.runtime_id().is_none() {
            return Err(failed());
        }
        let job = self.observe_code_job(identity).await?.ok_or_else(failed)?;
        if !job.running {
            return Ok(None);
        }
        let (out, err, status) = tokio::time::timeout(
            Duration::from_secs(10),
            self.exec_in_container(
                &job.container_id,
                vec!["cat", "/workspace/.elitea-python-preparation.json"],
                None,
                None,
            ),
        )
        .await
        .map_err(|_| failed())??;
        if status == 1 {
            return Ok(None);
        }
        if status != 0 || !err.is_empty() || out.len() > 256 * 1024 {
            return Err(failed());
        }
        Ok(Some(out.into_bytes()))
    }

    /// Export one recorded regular file into the supervisor's private staging writer.
    pub async fn export_code_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        let header = header(name, bytes)?;
        self.dependency_transfer(
            identity,
            "--dependency-read",
            &header,
            bytes,
            &mut tokio::io::empty(),
            writer,
            false,
        )
        .await
    }

    /// Export imported execution files from the fixed execution directory.
    pub async fn export_code_execution_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        writer: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        let header = header(name, bytes)?;
        self.dependency_transfer(
            identity,
            "--execution-dependency-read",
            &header,
            bytes,
            &mut tokio::io::empty(),
            writer,
            false,
        )
        .await
    }

    /// Import verified bytes before dispatch. The helper does not replace existing files.
    pub async fn import_code_dependency(
        &self,
        identity: &CodeJobIdentity,
        name: &str,
        bytes: u64,
        reader: &mut (dyn AsyncRead + Unpin + Send),
    ) -> Result<(), SandboxError> {
        let header = header(name, bytes)?;
        self.dependency_transfer(
            identity,
            "--dependency-write",
            &header,
            bytes,
            reader,
            &mut tokio::io::sink(),
            true,
        )
        .await
    }

    /// Release only the original preparer after shared publication succeeds.
    pub async fn release_code_preparation(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<(), SandboxError> {
        self.dependency_transfer(
            identity,
            "--preparation-release",
            &[],
            0,
            &mut tokio::io::empty(),
            &mut tokio::io::sink(),
            false,
        )
        .await
    }

    pub async fn native_code_dependency(
        &self,
        identity: &CodeJobIdentity,
        kind: &str,
        root: &str,
        name: &str,
        bytes: u64,
        command: &str,
        importing: bool,
        reader: &mut (dyn AsyncRead + Unpin + Send),
        writer: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<(), SandboxError> {
        if !matches!(kind, "deno" | "cargo")
            || root.len() != 64
            || !root
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            || bytes > 128 * 1024 * 1024
            || !(name == "elitea-native-bundle-v2.json"
                || name == "elitea-native-ready-v2.json"
                || name.strip_suffix(".blob").is_some_and(|v| {
                    v.len() == 64
                        && v.bytes()
                            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                }))
        {
            return Err(failed());
        }
        let mut header = serde_json::to_vec(
            &serde_json::json!({"revision":2,"kind":kind,"root":root,"name":name,"bytes":bytes}),
        )
        .map_err(|_| failed())?;
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
            return Err(failed());
        }
        self.dependency_transfer(identity, command, &header, bytes, reader, writer, importing)
            .await
    }
    pub async fn read_native_preparation_marker(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        if identity.runtime_id().is_none() {
            return Err(failed());
        }
        let job = self.observe_code_job(identity).await?.ok_or_else(failed)?;
        if !job.running {
            return Ok(None);
        }
        let (out, err, status) = tokio::time::timeout(
            Duration::from_secs(10),
            self.exec_in_container(
                &job.container_id,
                vec!["cat", "/workspace/.elitea-native-preparation.json"],
                None,
                None,
            ),
        )
        .await
        .map_err(|_| failed())??;
        if status == 1 {
            return Ok(None);
        }
        if status != 0 || !err.is_empty() || out.len() > 256 * 1024 {
            return Err(failed());
        }
        self.observe_code_job(identity).await?.ok_or_else(failed)?;
        Ok(Some(out.into_bytes()))
    }
    #[allow(clippy::too_many_arguments)] // One fixed transport owns both stream directions.
    pub(super) async fn dependency_transfer(
        &self,
        identity: &CodeJobIdentity,
        command: &str,
        header: &[u8],
        bytes: u64,
        reader: &mut (dyn AsyncRead + Unpin + Send),
        writer: &mut (dyn AsyncWrite + Unpin + Send),
        importing: bool,
    ) -> Result<(), SandboxError> {
        if identity.runtime_id().is_none() {
            return Err(failed());
        }
        let job = self.observe_code_job(identity).await?.ok_or_else(failed)?;
        if !job.running {
            return Err(failed());
        }
        let operation = async {
            let exec = self
                .client
                .create_exec(
                    &job.container_id,
                    CreateExecOptions {
                        cmd: Some(vec!["/usr/local/bin/elitea-code-runner", command]),
                        attach_stdin: Some(true),
                        attach_stdout: Some(true),
                        attach_stderr: Some(true),
                        ..Default::default()
                    },
                )
                .await
                .map_err(|_| failed())?;
            let StartExecResults::Attached {
                mut input,
                mut output,
            } = self
                .client
                .start_exec(&exec.id, None)
                .await
                .map_err(|_| failed())?
            else {
                return Err(failed());
            };
            let send = async {
                input.write_all(header).await.map_err(|_| failed())?;
                if importing {
                    let mut remaining = bytes;
                    let mut buffer = vec![0; 64 * 1024];
                    while remaining != 0 {
                        let bound = buffer
                            .len()
                            .min(usize::try_from(remaining).unwrap_or(usize::MAX));
                        let count = reader
                            .read(&mut buffer[..bound])
                            .await
                            .map_err(|_| failed())?;
                        if count == 0 {
                            return Err(failed());
                        }
                        input
                            .write_all(&buffer[..count])
                            .await
                            .map_err(|_| failed())?;
                        remaining -= count as u64;
                    }
                    if reader.read(&mut buffer[..1]).await.map_err(|_| failed())? != 0 {
                        return Err(failed());
                    }
                }
                input.shutdown().await.map_err(|_| failed())
            };
            let receive = async {
                let limit = if importing { 0 } else { bytes };
                let mut received = 0_u64;
                let mut errors = 0_usize;
                while let Some(message) = output.next().await {
                    match message.map_err(|_| failed())? {
                        bollard::container::LogOutput::StdOut { message } => {
                            received = received
                                .checked_add(message.len() as u64)
                                .ok_or_else(failed)?;
                            if received > limit {
                                return Err(failed());
                            }
                            writer.write_all(&message).await.map_err(|_| failed())?;
                        }
                        bollard::container::LogOutput::StdErr { message } => {
                            errors = errors.saturating_add(message.len());
                            if errors > 8192 {
                                return Err(failed());
                            }
                        }
                        _ => return Err(failed()),
                    }
                }
                if received != limit || errors != 0 {
                    return Err(failed());
                }
                writer.flush().await.map_err(|_| failed())
            };
            tokio::try_join!(send, receive)?;
            let status = self
                .client
                .inspect_exec(&exec.id)
                .await
                .map_err(|_| failed())?;
            if status.running != Some(false) || status.exit_code != Some(0) {
                return Err(failed());
            }
            self.observe_code_job(identity).await?.ok_or_else(failed)?;
            Ok(())
        };
        tokio::time::timeout(Duration::from_secs(30), operation)
            .await
            .map_err(|_| failed())?
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn binary_header_has_no_path_or_unbounded_length() {
        assert_eq!(
            header("sample.whl", 3).unwrap(),
            b"{\"name\":\"sample.whl\",\"bytes\":3}\n"
        );
        for name in ["", ".", "..", "../file", "/file", "a/b", "a\"b", "a\nb"] {
            assert!(header(name, 3).is_err());
        }
        assert!(header("file.whl", FILE_LIMIT + 1).is_err());
    }
}
