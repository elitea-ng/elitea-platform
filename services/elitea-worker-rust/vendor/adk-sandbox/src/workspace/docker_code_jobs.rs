//! Runtime identities survive the client's in-memory session map.
//! They supplement, never replace, the supervisor's durable invocation receipt.
use super::*;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CodeJobIdentity {
    pub(super) job_key: String,
    pub(super) request_digest: String,
}

impl CodeJobIdentity {
    /// Caller derives both values from an authorized, durably recorded invocation.
    /// The request digest must cover code/input, runtime, policy and dependency revisions.
    pub fn new(job_key: String, request_digest: String) -> Result<Self, SandboxError> {
        fn valid(value: &str) -> bool {
            value.len() == 64
                && value
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        }
        if !valid(&job_key) || !valid(&request_digest) {
            return Err(SandboxError::ExecutionFailed(
                "invalid Code job identity".into(),
            ));
        }
        Ok(Self {
            job_key,
            request_digest,
        })
    }

    pub(super) fn container_name(&self) -> String {
        format!("elitea-code-{}", self.job_key)
    }
}

/// A container state observation is not proof that user code completed.
/// Even a stopped container requires a durable terminal receipt before replay.
#[derive(Clone, Debug)]
pub struct CodeJobObservation {
    pub container_id: String,
    pub running: bool,
}

impl DockerClient {
    /// Create a named workload once. An existing name is a reconciliation signal,
    /// not permission to restart it or execute its code a second time.
    pub async fn provision_code_job(
        &self,
        identity: &CodeJobIdentity,
        manifest: &Manifest,
    ) -> Result<SessionHandle, SandboxError> {
        for entry in &manifest.entries {
            let path = match entry {
                ManifestEntry::File { path, .. }
                | ManifestEntry::Directory { path }
                | ManifestEntry::GitRepo { path, .. } => path,
            };
            if path
                .split('/')
                .any(|part| matches!(part, ".elitea-dispatch" | ".elitea-ready"))
            {
                return Err(SandboxError::ExecutionFailed(
                    "Code manifest contains a reserved lifecycle path".into(),
                ));
            }
        }
        if !self.code_job_policy {
            return Err(SandboxError::ExecutionFailed(
                "Code jobs require the isolated resource policy".into(),
            ));
        }
        if self.observe_code_job(identity).await?.is_some() {
            return Err(SandboxError::ExecutionFailed(
                "Code job already exists; reconcile its durable receipt before continuing".into(),
            ));
        }
        // Docker's unique name constraint also arbitrates concurrent submissions.
        self.provision_inner(manifest, Some(identity)).await
    }

    /// Inspect by stable name after reconnect/restart; never inserts a new session
    /// or launches a command. A mismatched fingerprint is a hard conflict.
    pub async fn observe_code_job(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<CodeJobObservation>, SandboxError> {
        let inspected = tokio::time::timeout(
            Duration::from_secs(10),
            self.client
                .inspect_container(&identity.container_name(), None),
        )
        .await
        .map_err(|_| {
            SandboxError::ExecutionFailed(
                "Code job observation timed out; state remains unknown".into(),
            )
        })?;
        let info = match inspected {
            Ok(info) => info,
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => return Ok(None),
            Err(_) => {
                return Err(SandboxError::ExecutionFailed(
                    "Code job observation failed; state remains unknown".into(),
                ));
            }
        };
        let labels = info
            .config
            .and_then(|config| config.labels)
            .unwrap_or_default();
        if labels.get("io.elitea.code.job") != Some(&identity.job_key)
            || labels.get("io.elitea.code.request") != Some(&identity.request_digest)
        {
            return Err(SandboxError::ExecutionFailed(
                "Code job identity conflicts with the existing workload".into(),
            ));
        }
        let container_id = info.id.ok_or_else(|| {
            SandboxError::ExecutionFailed("Code job observation has no runtime identity".into())
        })?;
        let running = info.state.and_then(|state| state.running).ok_or_else(|| {
            SandboxError::ExecutionFailed("Code job runtime state is unknown".into())
        })?;
        Ok(Some(CodeJobObservation {
            container_id,
            running,
        }))
    }
}

impl DockerClient {
    /// The caller must hold the current durable lease. Keep the container and
    /// its logs until the terminal ledger write is acknowledged.
    pub async fn terminate_code_job(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        let Some(job) = self.observe_code_job(identity).await? else {
            return Ok(());
        };
        let terminate = async {
            if job.running {
                // Target the inspected immutable ID, never a name that can be reused.
                let _ = self
                    .client
                    .kill_container(
                        &job.container_id,
                        Some(KillContainerOptions { signal: "SIGKILL" }),
                    )
                    .await;
            }
            match self.client.inspect_container(&job.container_id, None).await {
                Ok(info) if info.state.as_ref().and_then(|state| state.running) == Some(false) => {
                    Ok(())
                }
                Err(bollard::errors::Error::DockerResponseServerError {
                    status_code: 404, ..
                }) => Ok(()),
                _ => Err(SandboxError::ExecutionFailed(
                    "Code job termination is unconfirmed; reconciliation required".into(),
                )),
            }
        };
        tokio::time::timeout(Duration::from_secs(10), terminate)
            .await
            .map_err(|_| {
                SandboxError::ExecutionFailed(
                    "Code job termination timed out; reconciliation required".into(),
                )
            })?
    }

    /// Remove a stopped runtime only after the supervisor persists its terminal
    /// receipt. This does not force termination or depend on an in-memory session.
    pub async fn remove_code_job(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        let Some(job) = self.observe_code_job(identity).await? else {
            self.sessions
                .write()
                .await
                .remove(&identity.container_name());
            return Ok(());
        };
        if job.running {
            return Err(SandboxError::ExecutionFailed(
                "Code job is still running; termination must be confirmed before cleanup".into(),
            ));
        }
        let removed = tokio::time::timeout(
            Duration::from_secs(10),
            self.client.remove_container(
                &job.container_id,
                Some(RemoveContainerOptions {
                    force: false,
                    v: true,
                    ..Default::default()
                }),
            ),
        )
        .await
        .map_err(|_| {
            SandboxError::ExecutionFailed(
                "Code job cleanup timed out; reconciliation required".into(),
            )
        })?;
        match removed {
            Ok(())
            | Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => {
                self.sessions
                    .write()
                    .await
                    .remove(&identity.container_name());
                Ok(())
            }
            Err(_) => Err(SandboxError::ExecutionFailed(
                "Code job cleanup failed; retry cleanup without replaying code".into(),
            )),
        }
    }

    /// A prepared marker is written only after every manifest entry succeeds.
    /// A new supervisor can inspect it before crossing the durable dispatch fence.
    pub async fn code_job_prepared(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<bool, SandboxError> {
        let Some(job) = self.observe_code_job(identity).await? else {
            return Ok(false);
        };
        if !job.running {
            return Ok(false);
        }
        let result = tokio::time::timeout(
            Duration::from_secs(10),
            self.exec_in_container(
                &job.container_id,
                vec![
                    "sh",
                    "-c",
                    "test -f /workspace/.elitea-ready && cat /workspace/.elitea-ready",
                ],
                None,
                None,
            ),
        )
        .await
        .map_err(|_| {
            SandboxError::ExecutionFailed(
                "Code preparation observation timed out; reconciliation required".into(),
            )
        })??;
        Ok(result.2 == 0 && result.0 == identity.request_digest)
    }

    /// Call only after the supervisor durably records dispatch. Re-signaling
    /// cannot execute code twice: the container's main process consumes the
    /// marker once and execs the runner; an exited container is never restarted.
    pub async fn dispatch_code_job(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        let job = self.observe_code_job(identity).await?.ok_or_else(|| {
            SandboxError::ExecutionFailed("Code job is absent; reconcile before dispatch".into())
        })?;
        if !job.running {
            return Err(SandboxError::ExecutionFailed(
                "Code job already stopped; read its receipt".into(),
            ));
        }
        let result = tokio::time::timeout(
            Duration::from_secs(10),
            self.exec_in_container(
                &job.container_id,
                vec![
                    "sh",
                    "-c",
                    "test -f /workspace/.elitea-job.json && test \"$(cat /workspace/.elitea-ready)\" = \"$1\" && : > /workspace/.elitea-dispatch",
                    "sandbox-dispatch",
                    &identity.request_digest,
                ],
                None,
                None,
            ),
        )
        .await
        .map_err(|_| {
            SandboxError::ExecutionFailed(
                "Code dispatch acknowledgement timed out; reconcile the existing job".into(),
            )
        })??;
        if result.2 != 0 {
            return Err(SandboxError::ExecutionFailed(
                "Code job request is not prepared".into(),
            ));
        }
        Ok(())
    }

    /// Read one terminal envelope after reconnect. No user code is launched.
    /// Runtime absence or malformed/missing output is uncertainty, not success.
    /// The returned envelope is untrusted and must pass state projection validation.
    pub async fn read_code_job_receipt(
        &self,
        identity: &CodeJobIdentity,
    ) -> Result<Option<Vec<u8>>, SandboxError> {
        let job = self.observe_code_job(identity).await?.ok_or_else(|| {
            SandboxError::ExecutionFailed(
                "Code job is absent; durable receipt reconciliation required".into(),
            )
        })?;
        if job.running {
            return Ok(None);
        }
        let read = async {
            let mut stream = self.client.logs(
                &job.container_id,
                Some(bollard::container::LogsOptions::<String> {
                    stdout: true,
                    stderr: false,
                    follow: false,
                    tail: "all".into(),
                    ..Default::default()
                }),
            );
            let mut bytes = Vec::new();
            while let Some(chunk) = stream.next().await {
                let chunk = chunk.map_err(|_| {
                    SandboxError::ExecutionFailed("Code receipt log read failed".into())
                })?;
                let data = chunk.into_bytes();
                if bytes.len().saturating_add(data.len()) > 4 * 1024 * 1024 {
                    return Err(SandboxError::ExecutionFailed(
                        "Code receipt envelope exceeds its limit".into(),
                    ));
                }
                bytes.extend_from_slice(&data);
            }
            let receipt: ReceiptEnvelope = serde_json::from_slice(&bytes).map_err(|_| {
                SandboxError::ExecutionFailed(
                    "Code terminal receipt is missing or invalid; completion is unconfirmed".into(),
                )
            })?;
            if receipt.revision != 1
                || receipt.stdout.len().saturating_add(receipt.stderr.len()) > 3 * 512 * 1024
                || !matches!(
                    receipt.status.as_str(),
                    "completed"
                        | "failed"
                        | "timeout"
                        | "output_limit"
                        | "capture_failed"
                        | "launch_failed"
                        | "invalid_request"
                )
                || (receipt.status == "completed" && receipt.exit_code != Some(0))
            {
                return Err(SandboxError::ExecutionFailed(
                    "Code terminal receipt violates its contract".into(),
                ));
            }
            Ok(Some(bytes))
        };
        tokio::time::timeout(Duration::from_secs(10), read)
            .await
            .map_err(|_| {
                SandboxError::ExecutionFailed(
                    "Code receipt read timed out; reconciliation required".into(),
                )
            })?
    }
}

#[derive(serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct ReceiptEnvelope {
    revision: u8,
    status: String,
    exit_code: Option<i32>,
    stdout: String,
    stderr: String,
}
