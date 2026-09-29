//! Docker sandbox client implementation.
//!
//! Provisions workspaces inside Docker containers from a configurable base
//! image. Provides stronger isolation via container boundaries.
//!
//! This module is gated behind the `workspace-docker` feature flag.

use std::collections::HashMap;
use std::time::Duration;

use async_trait::async_trait;
use bollard::Docker;
use bollard::container::{
    Config, CreateContainerOptions, KillContainerOptions, RemoveContainerOptions,
    StopContainerOptions,
};
use bollard::exec::{CreateExecOptions, StartExecResults};
use bollard::image::CommitContainerOptions;
use bollard::models::HostConfig;
use futures::StreamExt;
use tokio::sync::RwLock;

use super::client::SandboxClient;
use super::manifest::{Manifest, ManifestEntry};
use super::path_safety::validate_relative_path;
use super::session::SandboxSession;
use super::types::{DirEntry, EntryType, ExecOutput, SessionHandle, SnapshotId};
use crate::SandboxError;

/// The workspace root directory inside Docker containers.
const CONTAINER_WORKSPACE_ROOT: &str = "/workspace";

/// Default command timeout (120 seconds).
const DEFAULT_COMMAND_TIMEOUT: Duration = Duration::from_secs(120);
const MAX_CAPTURE_BYTES: usize = 1024 * 1024;

fn immutable_image_reference(image: &str) -> bool {
    let digest = image.strip_prefix("sha256:").or_else(|| {
        let (name, digest) = image.split_once("@sha256:")?;
        (!name.is_empty() && !name.chars().any(char::is_whitespace)).then_some(digest)
    });
    digest.is_some_and(|value| {
        value.len() == 64
            && value
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    })
}

// Bound combined stdout/stderr, including replacement characters for invalid UTF-8.
fn append_capture(target: &mut String, other: &str, bytes: &[u8]) -> Result<(), SandboxError> {
    let remaining = MAX_CAPTURE_BYTES.saturating_sub(target.len().saturating_add(other.len()));
    if bytes.len() > remaining {
        return Err(SandboxError::ExecutionFailed(
            "sandbox output exceeded the 1 MiB capture limit".into(),
        ));
    }
    let text = String::from_utf8_lossy(bytes);
    if text.len() > remaining {
        return Err(SandboxError::ExecutionFailed(
            "sandbox output exceeded the 1 MiB capture limit".into(),
        ));
    }
    target.push_str(&text);
    Ok(())
}

/// SandboxClient implementation using Docker containers.
///
/// Provisions workspaces inside containers from a configurable base image.
/// Provides stronger isolation via container boundaries. Resource limits
/// (memory, CPU) can be configured and are applied to containers on
/// provisioning.
///
/// # Example
///
/// ```rust,ignore
/// use adk_sandbox::workspace::DockerClient;
///
/// let client = DockerClient::new().await?;
/// let client = client.with_resource_limits(Some(512 * 1024 * 1024), Some(1.5));
/// ```
pub struct DockerClient {
    /// Docker base image for new containers.
    pub base_image: String,
    /// Optional memory limit for containers (in bytes).
    pub memory_limit_bytes: Option<u64>,
    /// Optional CPU limit (fractional cores, e.g., 1.5).
    pub cpu_limit: Option<f64>,
    /// Apply Elitea's offline Code-job container policy.
    code_job_policy: bool,
    code_workspace_execution: bool,
    command_timeout: Duration,
    /// Bollard Docker client for API communication.
    client: Docker,
    /// Active sessions mapping handle IDs to container IDs.
    sessions: RwLock<HashMap<String, String>>,
}

impl DockerClient {
    /// Creates a new `DockerClient` connected to the local Docker daemon.
    ///
    /// Uses the default Docker socket connection (typically
    /// `/var/run/docker.sock` on Unix). The default base image is
    /// `ubuntu:22.04`.
    ///
    /// # Errors
    ///
    /// Returns `SandboxError::DockerUnavailable` if the Docker daemon
    /// is not accessible.
    pub async fn new() -> Result<Self, SandboxError> {
        let docker =
            Docker::connect_with_local_defaults().map_err(|e| SandboxError::DockerUnavailable {
                reason: format!("failed to connect to Docker daemon: {e}"),
            })?;

        // Verify the connection by pinging the daemon
        docker
            .ping()
            .await
            .map_err(|e| SandboxError::DockerUnavailable {
                reason: format!("Docker daemon not responding: {e}"),
            })?;

        Ok(Self {
            base_image: "ubuntu:22.04".to_string(),
            memory_limit_bytes: None,
            cpu_limit: None,
            code_job_policy: false,
            code_workspace_execution: false,
            command_timeout: DEFAULT_COMMAND_TIMEOUT,
            client: docker,
            sessions: RwLock::new(HashMap::new()),
        })
    }

    /// Creates a new `DockerClient` with a custom base image.
    ///
    /// # Errors
    ///
    /// Returns `SandboxError::DockerUnavailable` if the Docker daemon
    /// is not accessible.
    pub async fn with_image(base_image: impl Into<String>) -> Result<Self, SandboxError> {
        let mut client = Self::new().await?;
        client.base_image = base_image.into();
        Ok(client)
    }

    /// Sets resource limits on the client, returning the modified client.
    ///
    /// # Arguments
    ///
    /// * `memory_limit_bytes` - Optional memory limit in bytes for containers.
    /// * `cpu_limit` - Optional CPU limit as fractional cores (e.g., 1.5 = 1.5 cores).
    pub fn with_resource_limits(
        mut self,
        memory_limit_bytes: Option<u64>,
        cpu_limit: Option<f64>,
    ) -> Self {
        self.memory_limit_bytes = memory_limit_bytes;
        self.cpu_limit = cpu_limit;
        self
    }

    /// Require finite resource limits and isolate a non-root, offline Code job.
    /// Dependency acquisition needs a separate authorized preparation phase.
    pub fn with_code_job_policy(mut self, timeout: Duration) -> Result<Self, SandboxError> {
        self.command_timeout = timeout;
        self.validate_code_job_policy()?;
        self.code_job_policy = true;
        Ok(self)
    }

    /// Enable executable scratch only for a trusted compiled-language profile.
    /// This must never be selected from untrusted Code source or arguments.
    /// # Errors
    /// Returns an error unless a valid finite Code resource policy is configured.
    pub fn with_code_compilation(mut self) -> Result<Self, SandboxError> {
        if !self.code_job_policy {
            return Err(SandboxError::ExecutionFailed(
                "Code resource policy is required before compilation".into(),
            ));
        }
        self.validate_code_job_policy()?;
        self.code_workspace_execution = true;
        Ok(self)
    }

    pub fn code_compilation_enabled(&self) -> bool {
        self.code_job_policy && self.code_workspace_execution
    }

    fn validate_code_job_policy(&self) -> Result<(), SandboxError> {
        let timeout = self.command_timeout;
        if !matches!(self.memory_limit_bytes, Some(value) if value > 0 && value <= i64::MAX as u64)
            || !self
                .cpu_limit
                .is_some_and(|value| value.is_finite() && value > 0.0 && value <= 256.0)
            || timeout.is_zero()
            || timeout > Duration::from_secs(3600)
        {
            return Err(SandboxError::ExecutionFailed(
                "invalid Code-job resource policy".into(),
            ));
        }
        Ok(())
    }

    /// Check the local image cache without registry access or an implicit pull.
    /// A deployment preparation step must preload the pinned image first.
    /// Call this for readiness and again at provisioning, since images can be evicted.
    pub async fn check_code_image_ready(&self) -> Result<String, SandboxError> {
        if !immutable_image_reference(&self.base_image) {
            return Err(SandboxError::ExecutionFailed(
                "Code runtime image must be pinned to a SHA-256 digest".into(),
            ));
        }
        let image = tokio::time::timeout(
            Duration::from_secs(10),
            self.client.inspect_image(&self.base_image),
        )
        .await
        .map_err(|_| {
            SandboxError::ExecutionFailed("Code runtime image readiness check timed out".into())
        })?
        .map_err(|_| {
            SandboxError::ExecutionFailed(
                "Code runtime image is unavailable locally; preload it before accepting jobs"
                    .into(),
            )
        })?;
        image
            .id
            .filter(|id| immutable_image_reference(id))
            .ok_or_else(|| {
                SandboxError::ExecutionFailed(
                    "Code runtime image has no immutable local identity".into(),
                )
            })
    }

    /// Generates a unique session handle ID.
    fn generate_session_id() -> String {
        format!("docker-session-{}", uuid::Uuid::new_v4())
    }

    /// Builds the `HostConfig` with resource limits applied.
    fn build_host_config(&self) -> HostConfig {
        let mut host_config = HostConfig::default();

        if let Some(memory) = self.memory_limit_bytes {
            host_config.memory = Some(memory as i64);
        }

        if let Some(cpu) = self.cpu_limit {
            // Docker uses NanoCPUs (1e9 = 1 core)
            host_config.nano_cpus = Some((cpu * 1_000_000_000.0) as i64);
        }

        if self.code_job_policy {
            host_config.memory_swap = host_config.memory;
            host_config.pids_limit = Some(128);
            host_config.readonly_rootfs = Some(true);
            host_config.cap_drop = Some(vec!["ALL".into()]);
            host_config.security_opt = Some(vec!["no-new-privileges:true".into()]);
            host_config.network_mode = Some("none".into());
            host_config.tmpfs = Some(HashMap::from([
                (
                    "/workspace".into(),
                    format!(
                        "rw,{},nosuid,nodev,size=256m,uid=10001,gid=10001,mode=0700",
                        if self.code_workspace_execution {
                            "exec"
                        } else {
                            "noexec"
                        }
                    ),
                ),
                (
                    "/tmp".into(),
                    "rw,nosuid,nodev,noexec,size=64m,uid=10001,gid=10001,mode=0700".into(),
                ),
            ]));
        }

        host_config
    }

    /// Executes a command inside a container and returns stdout/stderr.
    async fn exec_in_container(
        &self,
        container_id: &str,
        cmd: Vec<&str>,
        working_dir: Option<&str>,
        stdin_content: Option<&[u8]>,
    ) -> Result<(String, String, i64), SandboxError> {
        let exec_options = CreateExecOptions {
            cmd: Some(cmd.iter().map(|s| s.to_string()).collect()),
            attach_stdout: Some(true),
            attach_stderr: Some(true),
            attach_stdin: stdin_content.is_some().then_some(true),
            working_dir: working_dir.map(|d| d.to_string()),
            ..Default::default()
        };

        let exec = self
            .client
            .create_exec(container_id, exec_options)
            .await
            .map_err(|e| {
                SandboxError::ExecutionFailed(format!("failed to create exec instance: {e}"))
            })?;

        let start_result = self
            .client
            .start_exec(&exec.id, None)
            .await
            .map_err(|e| SandboxError::ExecutionFailed(format!("failed to start exec: {e}")))?;

        let mut stdout = String::new();
        let mut stderr = String::new();

        match start_result {
            StartExecResults::Attached {
                mut output,
                mut input,
            } => {
                // If we have stdin content, write it and close
                if let Some(content) = stdin_content {
                    use tokio::io::AsyncWriteExt;
                    input.write_all(content).await.map_err(|_| {
                        SandboxError::ExecutionFailed("sandbox input delivery failed".into())
                    })?;
                    input.shutdown().await.map_err(|_| {
                        SandboxError::ExecutionFailed("sandbox input close failed".into())
                    })?;
                }

                while let Some(msg) = output.next().await {
                    match msg {
                        Ok(bollard::container::LogOutput::StdOut { message }) => {
                            append_capture(&mut stdout, &stderr, &message)?;
                        }
                        Ok(bollard::container::LogOutput::StdErr { message }) => {
                            append_capture(&mut stderr, &stdout, &message)?;
                        }
                        Ok(_) => {}
                        Err(_) => {
                            return Err(SandboxError::ExecutionFailed(
                                "sandbox output stream failed; completion is unconfirmed".into(),
                            ));
                        }
                    }
                }
            }
            StartExecResults::Detached => {}
        }

        // Get the exit code from the exec inspect
        let inspect =
            self.client.inspect_exec(&exec.id).await.map_err(|e| {
                SandboxError::ExecutionFailed(format!("failed to inspect exec: {e}"))
            })?;

        let exit_code = inspect.exit_code.unwrap_or(-1);

        Ok((stdout, stderr, exit_code))
    }
}

impl DockerClient {
    async fn provision_inner(
        &self,
        manifest: &Manifest,
        identity: Option<&CodeJobIdentity>,
    ) -> Result<SessionHandle, SandboxError> {
        let image = if self.code_job_policy {
            self.validate_code_job_policy()?;
            self.check_code_image_ready().await?
        } else {
            self.base_image.clone()
        };
        // Create container from base image with resource limits
        let mut host_config = self.build_host_config();
        if identity.is_some() {
            host_config.log_config = Some(bollard::models::HostConfigLogConfig {
                typ: Some("local".into()),
                config: Some(HashMap::from([
                    ("max-size".into(), "8m".into()),
                    ("max-file".into(), "1".into()),
                    ("compress".into(), "false".into()),
                ])),
            });
        }

        let config = Config {
            image: Some(image),
            labels: identity.map(|identity| {
                HashMap::from([
                    ("io.elitea.code.job".into(), identity.job_key.clone()),
                    (
                        "io.elitea.code.request".into(),
                        identity.request_digest.clone(),
                    ),
                ])
            }),
            user: self.code_job_policy.then(|| "10001:10001".to_owned()),
            // Named jobs execute exactly once as PID 1 after durable dispatch.
            entrypoint: identity.map(|_| Vec::<String>::new()),
            cmd: Some(if identity.is_some() {
                vec!["sh".into(), "-c".into(),
                    "while [ ! -f /workspace/.elitea-dispatch ]; do sleep 0.1; done; exec /usr/local/bin/elitea-code-runner".into()]
            } else {
                vec!["sleep".into(), "infinity".into()]
            }),
            working_dir: Some(CONTAINER_WORKSPACE_ROOT.to_string()),
            host_config: Some(host_config),
            ..Default::default()
        };

        let container = self
            .client
            .create_container(
                identity.map(|identity| CreateContainerOptions {
                    name: identity.container_name(),
                    platform: None,
                }),
                config,
            )
            .await
            .map_err(|e| SandboxError::ProvisionFailed {
                resource: self.base_image.clone(),
                reason: format!("failed to create container: {e}"),
                suggestion: "Ensure the base image exists locally or can be pulled.".to_string(),
            })?;

        let container_id = container.id;

        let provision = async {
            // Start the container so we can exec into it
            self.client
                .start_container::<String>(&container_id, None)
                .await
                .map_err(|e| SandboxError::ProvisionFailed {
                    resource: container_id.clone(),
                    reason: format!("failed to start container: {e}"),
                    suggestion: "Check Docker daemon status and resource availability.".to_string(),
                })?;

            // Create the workspace directory inside the container
            let (_, stderr, exit_code) = self
                .exec_in_container(
                    &container_id,
                    vec!["mkdir", "-p", CONTAINER_WORKSPACE_ROOT],
                    None,
                    None,
                )
                .await?;

            if exit_code != 0 {
                return Err(SandboxError::ProvisionFailed {
                    resource: CONTAINER_WORKSPACE_ROOT.to_string(),
                    reason: format!("failed to create workspace dir: {stderr}"),
                    suggestion: "Check container filesystem permissions.".to_string(),
                });
            }

            // Process each manifest entry
            for entry in &manifest.entries {
                match entry {
                    ManifestEntry::File { path, content } => {
                        validate_relative_path(path)?;
                        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

                        // Create parent directories
                        if let Some(parent_idx) = full_path.rfind('/') {
                            let parent = &full_path[..parent_idx];
                            let (_, _, code) = self
                                .exec_in_container(
                                    &container_id,
                                    vec!["mkdir", "-p", parent],
                                    None,
                                    None,
                                )
                                .await?;
                            if code != 0 {
                                return Err(SandboxError::ProvisionFailed {
                                    resource: path.clone(),
                                    reason: "failed to create parent directories".to_string(),
                                    suggestion: "Check container filesystem permissions."
                                        .to_string(),
                                });
                            }
                        }

                        // Write file content using sh -c with stdin
                        let cmd_str = r#"cat > "$1""#;
                        let (_, stderr, code) = self
                            .exec_in_container(
                                &container_id,
                                vec!["sh", "-c", cmd_str, "sandbox-write", &full_path],
                                None,
                                Some(content),
                            )
                            .await?;
                        if code != 0 {
                            return Err(SandboxError::ProvisionFailed {
                                resource: path.clone(),
                                reason: format!("failed to write file: {stderr}"),
                                suggestion: "Check container filesystem permissions.".to_string(),
                            });
                        }
                    }

                    ManifestEntry::Directory { path } => {
                        validate_relative_path(path)?;
                        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");
                        let (_, stderr, code) = self
                            .exec_in_container(
                                &container_id,
                                vec!["mkdir", "-p", &full_path],
                                None,
                                None,
                            )
                            .await?;
                        if code != 0 {
                            return Err(SandboxError::ProvisionFailed {
                                resource: path.clone(),
                                reason: format!("failed to create directory: {stderr}"),
                                suggestion: "Check container filesystem permissions.".to_string(),
                            });
                        }
                    }

                    ManifestEntry::GitRepo { url, branch, path } => {
                        validate_relative_path(path)?;
                        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

                        // Clone the repository

                        let (_, stderr, code) = self
                            .exec_in_container(
                                &container_id,
                                vec!["git", "clone", "--", url, &full_path],
                                None,
                                None,
                            )
                            .await?;
                        if code != 0 {
                            return Err(SandboxError::ProvisionFailed {
                            resource: path.clone(),
                            reason: format!("git clone failed: {stderr}"),
                            suggestion:
                                "Check the repository URL and ensure git is installed in the container."
                                    .to_string(),
                        });
                        }

                        // Checkout branch if specified
                        if let Some(branch_name) = branch {
                            let (_, stderr, code) = self
                                .exec_in_container(
                                    &container_id,
                                    vec!["git", "checkout", branch_name],
                                    Some(&full_path),
                                    None,
                                )
                                .await?;
                            if code != 0 {
                                return Err(SandboxError::ProvisionFailed {
                                    resource: path.clone(),
                                    reason: format!(
                                        "git checkout '{branch_name}' failed: {stderr}"
                                    ),
                                    suggestion: "Check that the branch exists in the repository."
                                        .to_string(),
                                });
                            }
                        }
                    }
                }
            }

            if let Some(identity) = identity {
                let (_, _, status) = self
                    .exec_in_container(
                        &container_id,
                        vec![
                            "sh",
                            "-c",
                            "printf '%s' \"$1\" > /workspace/.elitea-ready",
                            "sandbox-ready",
                            &identity.request_digest,
                        ],
                        None,
                        None,
                    )
                    .await?;
                if status != 0 {
                    return Err(SandboxError::ExecutionFailed(
                        "Code preparation readiness could not be recorded".into(),
                    ));
                }
            }
            Ok::<(), SandboxError>(())
        };
        let outcome = tokio::time::timeout(self.command_timeout, provision).await;
        let failure = match outcome {
            Ok(Ok(())) => None,
            Ok(Err(error)) => Some(error),
            Err(_) => Some(SandboxError::ExecutionFailed(
                "sandbox preparation timed out".into(),
            )),
        };
        if let Some(error) = failure {
            if identity.is_some() {
                // Keep the name as a reconciliation tombstone until the supervisor
                // persists a terminal receipt and explicitly removes the workload.
                DockerSession {
                    container_id: container_id.clone(),
                    client: self.client.clone(),
                    command_timeout: self.command_timeout,
                }
                .kill_workload()
                .await?;
                return Err(error);
            }
            let removal = self
                .client
                .remove_container(
                    &container_id,
                    Some(RemoveContainerOptions {
                        force: true,
                        ..Default::default()
                    }),
                )
                .await;
            if removal.is_err() {
                return Err(SandboxError::ProvisionFailed {
                    resource: container_id,
                    reason: "sandbox preparation failed and cleanup remains pending".into(),
                    suggestion: "Reconcile this container before admitting another attempt.".into(),
                });
            }
            return Err(error);
        }

        // Generate session handle and store the mapping
        let session_id =
            identity.map_or_else(Self::generate_session_id, CodeJobIdentity::container_name);
        let handle = SessionHandle::new(&session_id);

        let mut sessions = self.sessions.write().await;
        sessions.insert(session_id, container_id);

        Ok(handle)
    }
}

#[async_trait]
impl SandboxClient for DockerClient {
    async fn provision(&self, manifest: &Manifest) -> Result<SessionHandle, SandboxError> {
        self.provision_inner(manifest, None).await
    }

    async fn start(&self, handle: &SessionHandle) -> Result<Box<dyn SandboxSession>, SandboxError> {
        let sessions = self.sessions.read().await;
        let container_id =
            sessions
                .get(handle.as_str())
                .ok_or_else(|| SandboxError::SessionNotFound {
                    handle: handle.as_str().to_string(),
                })?;

        Ok(Box::new(DockerSession {
            container_id: container_id.clone(),
            client: self.client.clone(),
            command_timeout: self.command_timeout,
        }))
    }

    async fn stop(&self, handle: &SessionHandle) -> Result<(), SandboxError> {
        let sessions = self.sessions.read().await;
        let container_id = sessions.get(handle.as_str()).cloned().ok_or_else(|| {
            SandboxError::SessionNotFound {
                handle: handle.as_str().to_string(),
            }
        })?;
        drop(sessions);

        // Stop the container (with a short grace period)
        let stop_options = StopContainerOptions { t: 5 };
        let _ = self
            .client
            .stop_container(&container_id, Some(stop_options))
            .await;

        // Remove the container
        let remove_options = RemoveContainerOptions {
            force: true,
            ..Default::default()
        };
        match self
            .client
            .remove_container(&container_id, Some(remove_options))
            .await
        {
            Ok(())
            | Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => {}
            Err(_) => {
                return Err(SandboxError::ExecutionFailed(
                    "sandbox removal failed; cleanup remains pending".into(),
                ));
            }
        }
        self.sessions.write().await.remove(handle.as_str());
        Ok(())
    }

    async fn snapshot(&self, handle: &SessionHandle) -> Result<SnapshotId, SandboxError> {
        if self.code_job_policy {
            return Err(SandboxError::ExecutionFailed("Code jobs require durable result receipts; container snapshots do not preserve tmpfs state".into()));
        }
        let sessions = self.sessions.read().await;
        let container_id = sessions
            .get(handle.as_str())
            .ok_or_else(|| SandboxError::SessionNotFound {
                handle: handle.as_str().to_string(),
            })?
            .clone();
        drop(sessions);

        // Generate a unique image tag for the snapshot
        let snapshot_tag = format!("adk-snapshot-{}", uuid::Uuid::new_v4());
        let repo = "adk-sandbox";

        let commit_options = CommitContainerOptions {
            container: container_id.clone(),
            repo: repo.to_string(),
            tag: snapshot_tag.clone(),
            pause: true,
            ..Default::default()
        };

        self.client
            .commit_container(commit_options, Config::<String>::default())
            .await
            .map_err(|e| {
                SandboxError::ExecutionFailed(format!("failed to commit container as image: {e}"))
            })?;

        let image_ref = format!("{repo}:{snapshot_tag}");
        Ok(SnapshotId::new(image_ref))
    }

    async fn resume(&self, snapshot_id: &SnapshotId) -> Result<SessionHandle, SandboxError> {
        if self.code_job_policy {
            return Err(SandboxError::ExecutionFailed("Code jobs require durable result receipts; container snapshots do not preserve tmpfs state".into()));
        }
        let image_ref = snapshot_id.as_str();

        // Create a new container from the committed image
        let host_config = self.build_host_config();

        let config = Config {
            image: Some(image_ref.to_string()),
            cmd: Some(vec!["sleep".to_string(), "infinity".to_string()]),
            working_dir: Some(CONTAINER_WORKSPACE_ROOT.to_string()),
            host_config: Some(host_config),
            ..Default::default()
        };

        let container = self
            .client
            .create_container(None::<CreateContainerOptions<String>>, config)
            .await
            .map_err(|e| SandboxError::SnapshotNotFound {
                id: format!("failed to create container from snapshot '{image_ref}': {e}"),
            })?;

        let container_id = container.id;

        // Start the container
        self.client
            .start_container::<String>(&container_id, None)
            .await
            .map_err(|e| SandboxError::ProvisionFailed {
                resource: image_ref.to_string(),
                reason: format!("failed to start resumed container: {e}"),
                suggestion: "Check Docker daemon status.".to_string(),
            })?;

        // Generate session handle and store the mapping
        let session_id = Self::generate_session_id();
        let handle = SessionHandle::new(&session_id);

        let mut sessions = self.sessions.write().await;
        sessions.insert(session_id, container_id);

        Ok(handle)
    }
}

/// A live sandbox session backed by a Docker container.
///
/// Provides workspace operations (exec, read, write, list, patch) against
/// a running Docker container. Commands are executed via `docker exec`
/// with configurable timeouts.
///
/// # Example
///
/// ```rust,ignore
/// use adk_sandbox::workspace::{DockerClient, Manifest, SandboxClient};
///
/// let client = DockerClient::new().await?;
/// let handle = client.provision(&Manifest { entries: vec![] }).await?;
/// let session = client.start(&handle).await?;
///
/// let output = session.exec_command("echo hello", None).await?;
/// assert_eq!(output.stdout.trim(), "hello");
/// ```
pub struct DockerSession {
    /// The Docker container ID for this session.
    pub container_id: String,
    /// Bollard Docker client for API communication.
    client: Docker,
    /// Maximum duration for individual command executions.
    pub command_timeout: Duration,
}

impl DockerSession {
    /// Executes a command inside the container and captures output.
    async fn exec_cmd(
        &self,
        cmd: Vec<&str>,
        working_dir: Option<&str>,
        stdin_content: Option<&[u8]>,
    ) -> Result<(String, String, i64), SandboxError> {
        let exec_options = CreateExecOptions {
            cmd: Some(cmd.iter().map(|s| s.to_string()).collect()),
            attach_stdout: Some(true),
            attach_stderr: Some(true),
            attach_stdin: stdin_content.is_some().then_some(true),
            working_dir: working_dir.map(|d| d.to_string()),
            ..Default::default()
        };

        let exec = self
            .client
            .create_exec(&self.container_id, exec_options)
            .await
            .map_err(|e| {
                SandboxError::ExecutionFailed(format!("failed to create exec instance: {e}"))
            })?;

        let start_result = self
            .client
            .start_exec(&exec.id, None)
            .await
            .map_err(|e| SandboxError::ExecutionFailed(format!("failed to start exec: {e}")))?;

        let mut stdout = String::new();
        let mut stderr = String::new();

        match start_result {
            StartExecResults::Attached {
                mut output,
                mut input,
            } => {
                if let Some(content) = stdin_content {
                    use tokio::io::AsyncWriteExt;
                    input.write_all(content).await.map_err(|_| {
                        SandboxError::ExecutionFailed("sandbox input delivery failed".into())
                    })?;
                    input.shutdown().await.map_err(|_| {
                        SandboxError::ExecutionFailed("sandbox input close failed".into())
                    })?;
                }

                while let Some(msg) = output.next().await {
                    match msg {
                        Ok(bollard::container::LogOutput::StdOut { message }) => {
                            append_capture(&mut stdout, &stderr, &message)?;
                        }
                        Ok(bollard::container::LogOutput::StdErr { message }) => {
                            append_capture(&mut stderr, &stdout, &message)?;
                        }
                        Ok(_) => {}
                        Err(_) => {
                            return Err(SandboxError::ExecutionFailed(
                                "sandbox output stream failed; completion is unconfirmed".into(),
                            ));
                        }
                    }
                }
            }
            StartExecResults::Detached => {}
        }

        // Get the exit code
        let inspect =
            self.client.inspect_exec(&exec.id).await.map_err(|e| {
                SandboxError::ExecutionFailed(format!("failed to inspect exec: {e}"))
            })?;

        let exit_code = inspect.exit_code.unwrap_or(-1);
        Ok((stdout, stderr, exit_code))
    }
}

#[async_trait]
impl SandboxSession for DockerSession {
    async fn exec_command(
        &self,
        command: &str,
        working_dir: Option<&str>,
    ) -> Result<ExecOutput, SandboxError> {
        // Validate working_dir if provided
        let cwd = match working_dir {
            Some(dir) => {
                validate_relative_path(dir)?;
                format!("{CONTAINER_WORKSPACE_ROOT}/{dir}")
            }
            None => CONTAINER_WORKSPACE_ROOT.to_string(),
        };

        let start = std::time::Instant::now();

        let result = tokio::time::timeout(
            self.command_timeout,
            self.exec_cmd(vec!["sh", "-c", command], Some(&cwd), None),
        )
        .await;

        match result {
            Ok(Ok((stdout, stderr, exit_code))) => {
                let duration = start.elapsed();
                Ok(ExecOutput::new(
                    stdout,
                    stderr,
                    exit_code as i32,
                    duration,
                    false,
                ))
            }
            Ok(Err(e)) => {
                self.kill_workload().await?;
                Err(e)
            }
            Err(_) => {
                // Dropping the attached output stream does not stop remote code.
                self.kill_workload().await?;
                let duration = start.elapsed();
                Ok(ExecOutput::new("", "", -1, duration, true))
            }
        }
    }

    async fn read_file(&self, path: &str) -> Result<Vec<u8>, SandboxError> {
        validate_relative_path(path)?;
        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

        let (stdout, stderr, exit_code) =
            self.exec_cmd(vec!["cat", &full_path], None, None).await?;

        if exit_code != 0 {
            if stderr.contains("No such file") {
                return Err(SandboxError::ExecutionFailed(format!(
                    "file not found: {path}"
                )));
            }
            return Err(SandboxError::ExecutionFailed(format!(
                "failed to read file '{path}': {stderr}"
            )));
        }

        Ok(stdout.into_bytes())
    }

    async fn write_file(&self, path: &str, content: &[u8]) -> Result<(), SandboxError> {
        validate_relative_path(path)?;
        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

        // Create parent directories
        if let Some(parent_idx) = full_path.rfind('/') {
            let parent = &full_path[..parent_idx];
            let (_, _, code) = self
                .exec_cmd(vec!["mkdir", "-p", parent], None, None)
                .await?;
            if code != 0 {
                return Err(SandboxError::ExecutionFailed(format!(
                    "failed to create parent directories for '{path}'"
                )));
            }
        }

        // Write content via stdin to cat
        let cmd_str = r#"cat > "$1""#;
        let (_, stderr, exit_code) = self
            .exec_cmd(
                vec!["sh", "-c", cmd_str, "sandbox-write", &full_path],
                None,
                Some(content),
            )
            .await?;

        if exit_code != 0 {
            return Err(SandboxError::ExecutionFailed(format!(
                "failed to write file '{path}': {stderr}"
            )));
        }

        Ok(())
    }

    async fn list_dir(&self, path: &str) -> Result<Vec<DirEntry>, SandboxError> {
        validate_relative_path(path)?;
        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

        // Use ls -1F to get entries with type indicators (/ suffix = directory)
        let (stdout, stderr, exit_code) = self
            .exec_cmd(vec!["ls", "-1F", &full_path], None, None)
            .await?;

        if exit_code != 0 {
            if stderr.contains("No such file") || stderr.contains("cannot access") {
                return Err(SandboxError::ExecutionFailed(format!(
                    "directory not found: {path}"
                )));
            }
            return Err(SandboxError::ExecutionFailed(format!(
                "failed to list directory '{path}': {stderr}"
            )));
        }

        let entries = stdout
            .lines()
            .filter(|line| !line.is_empty())
            .map(|line| {
                if let Some(name) = line.strip_suffix('/') {
                    DirEntry::new(name, EntryType::Directory)
                } else {
                    // Strip other type indicators (* for executable, @ for symlink, etc.)
                    let name = line
                        .strip_suffix('*')
                        .or_else(|| line.strip_suffix('@'))
                        .or_else(|| line.strip_suffix('|'))
                        .or_else(|| line.strip_suffix('='))
                        .unwrap_or(line);
                    DirEntry::new(name, EntryType::File)
                }
            })
            .collect();

        Ok(entries)
    }

    async fn apply_patch(&self, patch: &str) -> Result<(), SandboxError> {
        // Apply patch via stdin to the patch command
        let (_, stderr, exit_code) = self
            .exec_cmd(
                vec!["patch", "-p0", "--no-backup-if-mismatch"],
                Some(CONTAINER_WORKSPACE_ROOT),
                Some(patch.as_bytes()),
            )
            .await?;

        if exit_code != 0 {
            return Err(SandboxError::ExecutionFailed(format!(
                "patch failed: {stderr}"
            )));
        }

        Ok(())
    }
}

impl DockerSession {
    async fn kill_workload(&self) -> Result<(), SandboxError> {
        match self
            .client
            .kill_container(
                &self.container_id,
                Some(KillContainerOptions { signal: "SIGKILL" }),
            )
            .await
        {
            Ok(()) => Ok(()),
            Err(_) => {
                let stopped = self
                    .client
                    .inspect_container(&self.container_id, None)
                    .await
                    .ok()
                    .and_then(|info| info.state)
                    .and_then(|state| state.running)
                    == Some(false);
                if stopped {
                    Ok(())
                } else {
                    Err(SandboxError::ExecutionFailed(
                        "sandbox workload termination failed; reconciliation required".into(),
                    ))
                }
            }
        }
    }
}

impl std::fmt::Debug for DockerClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DockerClient")
            .field("base_image", &self.base_image)
            .field("memory_limit_bytes", &self.memory_limit_bytes)
            .field("cpu_limit", &self.cpu_limit)
            .finish_non_exhaustive()
    }
}

impl std::fmt::Debug for DockerSession {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DockerSession")
            .field("container_id", &self.container_id)
            .field("command_timeout", &self.command_timeout)
            .finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fmt::Debug;

    #[test]
    fn runtime_images_require_complete_immutable_identity() {
        let digest = "a".repeat(64);
        assert!(immutable_image_reference(&format!("sha256:{digest}")));
        assert!(immutable_image_reference(&format!(
            "registry/runtime@sha256:{digest}"
        )));
        for bad in [
            "runtime:latest",
            "runtime:1.2",
            "sha256:abc",
            "@sha256:abc",
            "sha256:",
        ] {
            assert!(!immutable_image_reference(bad));
        }
        assert!(!immutable_image_reference(&format!(
            "registry/ runtime@sha256:{digest}"
        )));
    }

    #[test]
    fn capture_enforces_combined_limit_without_partial_append() {
        let other = "x".repeat(MAX_CAPTURE_BYTES - 2);
        let mut target = String::new();
        append_capture(&mut target, &other, b"ok").unwrap();
        assert!(append_capture(&mut target, &other, b"!").is_err());
        assert_eq!(target, "ok");
    }

    #[test]
    fn capture_counts_utf8_replacement_expansion() {
        let other = "x".repeat(MAX_CAPTURE_BYTES - 2);
        let mut target = String::new();
        assert!(append_capture(&mut target, &other, &[255]).is_err());
        assert!(target.is_empty());
    }

    #[test]
    fn debug_does_not_expose_client_internals() {
        // Validates the Debug impl compiles and doesn't include
        // the Docker client field.
        let _: fn(&DockerClient, &mut std::fmt::Formatter<'_>) -> std::fmt::Result =
            <DockerClient as Debug>::fmt;
    }

    #[test]
    fn debug_session_does_not_expose_client() {
        let _: fn(&DockerSession, &mut std::fmt::Formatter<'_>) -> std::fmt::Result =
            <DockerSession as Debug>::fmt;
    }

    #[test]
    fn container_workspace_root_is_absolute() {
        assert!(CONTAINER_WORKSPACE_ROOT.starts_with('/'));
    }
}

#[cfg(test)]
#[path = "docker_live_tests.rs"]
mod live_tests;

#[path = "docker_code_jobs.rs"]
mod code_jobs;
pub use code_jobs::{CodeJobIdentity, CodeJobObservation};
