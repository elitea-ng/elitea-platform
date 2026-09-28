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
