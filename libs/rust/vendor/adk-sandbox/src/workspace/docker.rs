//! Docker sandbox client implementation.
//!
//! Provisions workspaces inside Docker containers from a configurable base
//! image. Provides stronger isolation via container boundaries.
//!
//! This module is gated behind the `workspace-docker` feature flag.
//!
//! ## Container Defaults
//!
//! | Setting | Default | Override |
//! |---------|---------|----------|
//! | Network | `none` — no network interface besides loopback | [`DockerClient::with_network_mode`] |
//! | Capabilities | All dropped (`cap_drop: ALL`) | — |
//! | Privilege escalation | `no-new-privileges` | — |
//! | Process count | [`DEFAULT_PIDS_LIMIT`] | [`DockerClient::with_pids_limit`] |
//! | Memory, CPU | Unlimited | [`DockerClient::with_resource_limits`] |
//!
//! `GitRepo` manifest entries clone from inside the container, so they need a network mode
//! other than `none`.
//!
//! Paths, URLs, and branch names are passed to commands as separate arguments, never
//! interpolated into a shell script. A command that exceeds its timeout is killed inside the
//! container together with its descendants.
//!
//! Elitea patch: [`DockerClient::with_code_job_policy`] layers a stricter, non-overridable
//! policy on top of these defaults for Code jobs: finite memory/CPU, no extra swap, at most
//! 128 processes, a read-only root, noexec tmpfs scratch, network `none` (or one
//! operator-admitted resolver network during preparation), a 1 MiB output bound, and a
//! container kill on command failure or timeout.

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

// Elitea patch: Code-job modules (see libs/rust/vendor/README.md).
#[path = "docker_compiled_content.rs"]
mod compiled_content;
#[path = "docker_dependency_content.rs"]
mod dependency_content;
#[path = "docker_repository.rs"]
mod repository;
pub use repository::CodeRepositoryMount;

/// The workspace root directory inside Docker containers.
const CONTAINER_WORKSPACE_ROOT: &str = "/workspace";

/// Default command timeout (120 seconds).
const DEFAULT_COMMAND_TIMEOUT: Duration = Duration::from_secs(120);

/// Elitea patch: combined stdout+stderr bound for every exec, after UTF-8 replacement.
const MAX_CAPTURE_BYTES: usize = 1024 * 1024;

/// Elitea patch: process limit for Code-job containers; never raised by `with_pids_limit`.
const CODE_JOB_PIDS_LIMIT: i64 = 128;

// Elitea patch: a Code runtime image must be pinned by a full SHA-256 digest.
fn immutable_image_reference(image: &str) -> bool {
    let digest = image.strip_prefix("sha256:").or_else(|| {
        let (name, digest) = image.split_once("@sha256:")?;
        (!name.is_empty() && !name.chars().any(char::is_whitespace)).then_some(digest)
    });
    digest.is_some_and(|value| {
        value.len() == 64
            && value.bytes().all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    })
}

fn capture_limit_exceeded() -> SandboxError {
    SandboxError::ExecutionFailed("sandbox output exceeded the 1 MiB capture limit".into())
}

// Elitea patch: bound combined stdout/stderr, including replacement characters for invalid
// UTF-8; nothing is appended when the bound would be exceeded.
fn append_capture(target: &mut String, other: &str, bytes: &[u8]) -> Result<(), SandboxError> {
    let remaining = MAX_CAPTURE_BYTES.saturating_sub(target.len().saturating_add(other.len()));
    if bytes.len() > remaining {
        return Err(capture_limit_exceeded());
    }
    let text = String::from_utf8_lossy(bytes);
    if text.len() > remaining {
        return Err(capture_limit_exceeded());
    }
    target.push_str(&text);
    Ok(())
}

// Elitea patch: decode raw exec output to text under the same combined bound.
fn bounded_text(stdout: &[u8], stderr: &[u8]) -> Result<(String, String), SandboxError> {
    let mut out = String::new();
    append_capture(&mut out, "", stdout)?;
    let mut err = String::new();
    append_capture(&mut err, &out, stderr)?;
    Ok((out, err))
}

/// Network mode for new containers unless [`DockerClient::with_network_mode`] changes it.
pub const DEFAULT_NETWORK_MODE: &str = "none";

/// Maximum number of processes in a container unless [`DockerClient::with_pids_limit`]
/// changes it. Bounds fork bombs without constraining ordinary builds.
pub const DEFAULT_PIDS_LIMIT: i64 = 512;

/// Upper bound on the in-container cleanup that follows a command timeout.
const KILL_TIMEOUT: Duration = Duration::from_secs(10);

/// Runs `$1` as a child of a shell whose PID is recorded in `$2`, so a timeout can find and
/// kill the whole tree. The PID file is removed once the command finishes.
const TIMED_EXEC_SCRIPT: &str =
    r#"{ echo $$ > "$2"; } 2>/dev/null; sh -c "$1"; status=$?; rm -f "$2"; exit $status"#;

/// Kills the process tree whose root PID is recorded in `$1`: its process group, and every
/// descendant found through `/proc`, stopped first so none can fork during the sweep.
const KILL_TREE_SCRIPT: &str = r#"pid=$(cat "$1" 2>/dev/null) || exit 0
rm -f "$1"
tree() { echo "$1"; for child in $(cat /proc/"$1"/task/*/children 2>/dev/null); do tree "$child"; done; }
kill -s STOP $(tree "$pid") 2>/dev/null
kill -s KILL -- -"$pid" $(tree "$pid") 2>/dev/null
exit 0"#;

/// SandboxClient implementation using Docker containers.
///
/// Provisions workspaces inside containers from a configurable base image.
/// Containers start with no network, all capabilities dropped,
/// `no-new-privileges`, and a process limit; see the
/// [module documentation](self) for the defaults and how to change them.
/// Resource limits (memory, CPU) can be configured and are applied to
/// containers on provisioning.
///
/// # Example
///
/// ```rust,ignore
/// use adk_sandbox::workspace::DockerClient;
///
/// let client = DockerClient::new().await?;
/// let client = client
///     .with_resource_limits(Some(512 * 1024 * 1024), Some(1.5))
///     .with_network_mode("bridge");
/// ```
pub struct DockerClient {
    /// Docker base image for new containers.
    pub base_image: String,
    /// Optional memory limit for containers (in bytes).
    pub memory_limit_bytes: Option<u64>,
    /// Optional CPU limit (fractional cores, e.g., 1.5).
    pub cpu_limit: Option<f64>,
    /// Docker network mode for new containers. Default: [`DEFAULT_NETWORK_MODE`] (`"none"`).
    pub network_mode: String,
    /// Maximum number of processes per container; `None` removes the limit.
    /// Default: [`DEFAULT_PIDS_LIMIT`].
    pub pids_limit: Option<i64>,
    /// Elitea patch: apply the offline Code-job container policy.
    code_job_policy: bool,
    code_workspace_execution: bool,
    code_platform_profile: bool,
    code_workspace_bytes: u64,
    code_preparation_network: Option<String>,
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
        docker.ping().await.map_err(|e| SandboxError::DockerUnavailable {
            reason: format!("Docker daemon not responding: {e}"),
        })?;

        Ok(Self {
            base_image: "ubuntu:22.04".to_string(),
            memory_limit_bytes: None,
            cpu_limit: None,
            network_mode: DEFAULT_NETWORK_MODE.to_string(),
            pids_limit: Some(DEFAULT_PIDS_LIMIT),
            // Elitea patch: Code-job policy is off until `with_code_job_policy`.
            code_job_policy: false,
            code_workspace_execution: false,
            code_platform_profile: false,
            code_workspace_bytes: 256 * 1024 * 1024,
            code_preparation_network: None,
            command_timeout: DEFAULT_COMMAND_TIMEOUT,
            client: docker,
            sessions: RwLock::new(HashMap::new()),
        })
    }

    /// Elitea patch: size the Code workspace tmpfs within the memory limit.
    pub fn with_code_workspace_bytes(mut self, bytes: u64) -> Result<Self, SandboxError> {
        if !self.code_job_policy
            || !(512 * 1024 * 1024..=4 * 1024 * 1024 * 1024).contains(&bytes)
            || self.memory_limit_bytes.is_none_or(|m| m < bytes + 128 * 1024 * 1024)
        {
            return Err(SandboxError::ExecutionFailed(
                "native workspace capacity is invalid".into(),
            ));
        }
        self.code_workspace_bytes = bytes;
        Ok(self)
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

    /// Sets the Docker network mode for new containers, returning the modified client.
    ///
    /// The default, `"none"`, gives containers only a loopback interface. Use `"bridge"`
    /// (Docker's default network) or a user-defined network name when commands or `GitRepo`
    /// manifest entries need network access.
    ///
    /// # Example
    ///
    /// ```rust,ignore
    /// use adk_sandbox::workspace::DockerClient;
    ///
    /// let client = DockerClient::new().await?.with_network_mode("bridge");
    /// ```
    pub fn with_network_mode(mut self, network_mode: impl Into<String>) -> Self {
        self.network_mode = network_mode.into();
        self
    }

    /// Sets the per-container process limit, returning the modified client.
    ///
    /// `None` removes the limit. The default is [`DEFAULT_PIDS_LIMIT`].
    pub fn with_pids_limit(mut self, pids_limit: Option<i64>) -> Self {
        self.pids_limit = pids_limit;
        self
    }

    // Elitea patch: Code-job policy builders and image readiness.
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
        if !self.code_job_policy || self.code_preparation_network.is_some() {
            return Err(SandboxError::ExecutionFailed(
                "Code resource policy is required before compilation".into(),
            ));
        }
        self.validate_code_job_policy()?;
        self.code_workspace_execution = true;
        Ok(self)
    }

    /// Select only an operator-admitted immutable broker image/wrapper/key profile.
    /// No YAML, Code source, or prepared metadata may call this constructor.
    /// # Errors
    /// Refuses resolver-network or unbounded/nonimmutable execution profiles.
    pub fn with_code_platform_profile(mut self) -> Result<Self, SandboxError> {
        self.validate_code_job_policy()?;
        if !self.code_job_policy
            || self.code_preparation_network.is_some()
            || !immutable_image_reference(&self.base_image)
        {
            return Err(SandboxError::ExecutionFailed(
                "Code platform profile is not admitted".into(),
            ));
        }
        self.code_platform_profile = true;
        Ok(self)
    }
    pub fn code_platform_profile_enabled(&self) -> bool {
        self.code_job_policy && self.code_platform_profile
    }

    pub fn code_compilation_enabled(&self) -> bool {
        self.code_job_policy && self.code_workspace_execution
    }

    /// Select an operator-created network only for the trusted resolver profile.
    /// Execution profiles retain network=none; source cannot select this name.
    /// # Errors
    /// Rejects invalid names and profiles without bounded non-root isolation.
    pub fn with_code_preparation_network(mut self, network: String) -> Result<Self, SandboxError> {
        if !self.code_job_policy
            || self.code_workspace_execution
            || network.is_empty()
            || network.len() > 128
            || matches!(network.as_str(), "host" | "none" | "bridge")
            || !network
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'-'))
        {
            return Err(SandboxError::ExecutionFailed(
                "Invalid dependency preparation network policy".into(),
            ));
        }
        self.code_preparation_network = Some(network);
        Ok(self)
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

    /// Return the deployment timeout bound for Code execution.
    pub fn code_job_timeout(&self) -> Duration {
        self.command_timeout
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

    /// Builds the `HostConfig` for a new container from this client's settings.
    fn build_host_config(&self) -> HostConfig {
        let mut host_config =
            host_config(self.memory_limit_bytes, self.cpu_limit, &self.network_mode, self.pids_limit);

        // Elitea patch: the Code-job policy overrides every caller-tunable setting with a
        // stricter one; `with_network_mode` / `with_pids_limit` cannot loosen it. cap_drop
        // ALL comes from upstream; `no-new-privileges:true` is kept verbatim because the
        // repository helper's reconciliation check matches that exact string.
        if self.code_job_policy {
            host_config.memory_swap = host_config.memory;
            host_config.pids_limit = Some(
                self.pids_limit.map_or(CODE_JOB_PIDS_LIMIT, |limit| limit.min(CODE_JOB_PIDS_LIMIT)),
            );
            host_config.readonly_rootfs = Some(true);
            host_config.security_opt = Some(vec!["no-new-privileges:true".into()]);
            host_config.network_mode =
                Some(self.code_preparation_network.clone().unwrap_or_else(|| "none".into()));
            host_config.tmpfs = Some(HashMap::from([
                (
                    "/workspace".into(),
                    format!(
                        "rw,{},nosuid,nodev,size={},uid=10001,gid=10001,mode=0700",
                        if self.code_workspace_execution { "exec" } else { "noexec" },
                        if self.code_workspace_bytes == 256 * 1024 * 1024 {
                            "256m".into()
                        } else {
                            self.code_workspace_bytes.to_string()
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
        let (stdout, stderr, exit_code) =
            exec_collect(&self.client, container_id, cmd, working_dir, stdin_content).await?;
        // Elitea patch: the decoded text stays within the capture bound.
        let (stdout, stderr) = bounded_text(&stdout, &stderr)?;
        Ok((stdout, stderr, exit_code))
    }
}

/// Builds the `HostConfig` for a sandbox container.
///
/// Every container drops all capabilities and sets `no-new-privileges`; the network mode,
/// process limit, and resource limits come from the caller.
fn host_config(
    memory_limit_bytes: Option<u64>,
    cpu_limit: Option<f64>,
    network_mode: &str,
    pids_limit: Option<i64>,
) -> HostConfig {
    HostConfig {
        // Docker takes a signed byte count; saturate rather than wrap for absurd limits.
        memory: memory_limit_bytes.map(|bytes| i64::try_from(bytes).unwrap_or(i64::MAX)),
        // Docker uses NanoCPUs (1e9 = 1 core)
        nano_cpus: cpu_limit.map(|cpu| (cpu * 1_000_000_000.0) as i64),
        network_mode: Some(network_mode.to_string()),
        cap_drop: Some(vec!["ALL".to_string()]),
        security_opt: Some(vec!["no-new-privileges".to_string()]),
        pids_limit,
        ..Default::default()
    }
}

/// Command that writes its stdin to `path` with the path passed as `$1`, so no character
/// in it is interpreted by the shell.
fn write_file_command(path: &str) -> Vec<String> {
    ["sh", "-c", r#"cat > "$1""#, "adk-write", path].map(str::to_string).to_vec()
}

/// Commands that clone `url` into `path` and optionally check out `branch`.
///
/// # Errors
///
/// Returns `ProvisionFailed` when `branch` starts with `-`, which git would parse as an
/// option and which `git check-ref-format --branch` rejects anyway.
fn git_clone_commands(
    url: &str,
    branch: Option<&str>,
    path: &str,
) -> Result<Vec<Vec<String>>, SandboxError> {
    let mut commands = vec![["git", "clone", "--", url, path].map(str::to_string).to_vec()];
    if let Some(branch) = branch {
        if branch.starts_with('-') {
            return Err(SandboxError::ProvisionFailed {
                resource: path.to_string(),
                reason: format!("branch name '{branch}' starts with '-'"),
                suggestion: "Use a valid branch name; git rejects names beginning with a dash."
                    .to_string(),
            });
        }
        commands.push(["git", "-C", path, "checkout", branch].map(str::to_string).to_vec());
    }
    Ok(commands)
}

/// Command that runs `command` under `sh -c` and records the wrapper's PID in `pid_file`.
fn timed_exec_command(command: &str, pid_file: &str) -> Vec<String> {
    ["sh", "-c", TIMED_EXEC_SCRIPT, "adk-exec", command, pid_file].map(str::to_string).to_vec()
}

/// Command that kills the process tree recorded in `pid_file` by [`timed_exec_command`].
fn kill_tree_command(pid_file: &str) -> Vec<String> {
    ["sh", "-c", KILL_TREE_SCRIPT, "adk-kill", pid_file].map(str::to_string).to_vec()
}

/// Runs `cmd` in `container_id` and returns raw stdout, raw stderr, and the exit code.
async fn exec_collect(
    client: &Docker,
    container_id: &str,
    cmd: Vec<&str>,
    working_dir: Option<&str>,
    stdin_content: Option<&[u8]>,
) -> Result<(Vec<u8>, Vec<u8>, i64), SandboxError> {
    let exec_options = CreateExecOptions {
        cmd: Some(cmd.iter().map(|s| s.to_string()).collect()),
        attach_stdout: Some(true),
        attach_stderr: Some(true),
        attach_stdin: stdin_content.is_some().then_some(true),
        working_dir: working_dir.map(|d| d.to_string()),
        ..Default::default()
    };

    let exec = client.create_exec(container_id, exec_options).await.map_err(|e| {
        SandboxError::ExecutionFailed(format!("failed to create exec instance: {e}"))
    })?;

    let start_result = client
        .start_exec(&exec.id, None)
        .await
        .map_err(|e| SandboxError::ExecutionFailed(format!("failed to start exec: {e}")))?;

    let mut stdout = Vec::new();
    let mut stderr = Vec::new();
    let mut captured = 0usize;

    match start_result {
        StartExecResults::Attached { mut output, mut input } => {
            // If we have stdin content, write it and close
            // Elitea patch: undelivered input fails the exec instead of running on partial
            // input.
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
                // Elitea patch: raw stdout+stderr are bounded while streaming, and a stream
                // error fails the exec because completion is unconfirmed.
                let (target, message) = match msg {
                    Ok(bollard::container::LogOutput::StdOut { message }) => (&mut stdout, message),
                    Ok(bollard::container::LogOutput::StdErr { message }) => (&mut stderr, message),
                    Ok(_) => continue,
                    Err(_) => {
                        return Err(SandboxError::ExecutionFailed(
                            "sandbox output stream failed; completion is unconfirmed".into(),
                        ));
                    }
                };
                if captured + message.len() > MAX_CAPTURE_BYTES {
                    return Err(capture_limit_exceeded());
                }
                captured += message.len();
                target.extend_from_slice(&message);
            }
        }
        StartExecResults::Detached => {}
    }

    // Get the exit code from the exec inspect
    let inspect = client
        .inspect_exec(&exec.id)
        .await
        .map_err(|e| SandboxError::ExecutionFailed(format!("failed to inspect exec: {e}")))?;

    let exit_code = inspect.exit_code.unwrap_or(-1);

    Ok((stdout, stderr, exit_code))
}

// Elitea patch: provisioning takes an optional Code-job identity (named container, labels,
// PID-1 runner launch, readiness marker), runs under the command timeout, and never leaves a
// half-prepared anonymous container behind.
impl DockerClient {
    async fn provision_inner(
        &self,
        manifest: &Manifest,
        identity: Option<&CodeJobIdentity>,
    ) -> Result<SessionHandle, SandboxError> {
        self.provision_with_launch(manifest, identity, None).await
    }

    async fn provision_with_launch(
        &self,
        manifest: &Manifest,
        identity: Option<&CodeJobIdentity>,
        launch: Option<Vec<String>>,
    ) -> Result<SessionHandle, SandboxError> {
        self.provision_with_repository(manifest, identity, launch, None).await
    }

    async fn provision_with_repository(
        &self,
        manifest: &Manifest,
        identity: Option<&CodeJobIdentity>,
        launch: Option<Vec<String>>,
        repository: Option<&CodeRepositoryMount>,
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

        if let Some(repository) = repository {
            host_config.mounts = Some(vec![repository.mount(true)]);
        }
        let launch = if let (Some(repository), Some(identity)) = (repository, identity) {
            let mut env = launch.unwrap_or_default();
            env.extend(repository.environment(identity));
            Some(env)
        } else {
            launch
        };
        let config = Config {
            image: Some(image),
            env: launch,
            labels: identity.map(|identity| {
                if let Some(repository) = repository {
                    return repository.labels(identity, "execution");
                }
                HashMap::from([
                    ("io.elitea.code.job".into(), identity.job_key.clone()),
                    ("io.elitea.code.request".into(), identity.request_digest.clone()),
                ])
            }),
            user: self.code_job_policy.then(|| "10001:10001".to_owned()),
            // Named jobs execute exactly once as PID 1 after durable dispatch.
            entrypoint: identity.map(|_| Vec::<String>::new()),
            cmd: Some(if identity.is_some() {
                vec![
                    "sh".into(),
                    "-c".into(),
                    "while [ ! -f /workspace/.elitea-dispatch ]; do sleep 0.1; done; exec /usr/local/bin/elitea-code-runner".into(),
                ]
            } else {
                // Keep container running with a long-lived process
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
            self.client.start_container::<String>(&container_id, None).await.map_err(|e| {
                SandboxError::ProvisionFailed {
                    resource: container_id.clone(),
                    reason: format!("failed to start container: {e}"),
                    suggestion: "Check Docker daemon status and resource availability.".to_string(),
                }
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
                self.provision_entry(&container_id, entry).await?;
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
            Err(_) => Some(SandboxError::ExecutionFailed("sandbox preparation timed out".into())),
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
                    Some(RemoveContainerOptions { force: true, ..Default::default() }),
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

    /// Materializes one manifest entry inside `container_id` (upstream 2.3.0 logic).
    async fn provision_entry(
        &self,
        container_id: &str,
        entry: &ManifestEntry,
    ) -> Result<(), SandboxError> {
        match entry {
            ManifestEntry::File { path, content } => {
                validate_relative_path(path)?;
                let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

                // Create parent directories
                if let Some(parent_idx) = full_path.rfind('/') {
                    let parent = &full_path[..parent_idx];
                    let (_, _, code) = self
                        .exec_in_container(
                            container_id,
                            vec!["mkdir", "-p", parent],
                            None,
                            None,
                        )
                        .await?;
                    if code != 0 {
                        return Err(SandboxError::ProvisionFailed {
                            resource: path.clone(),
                            reason: "failed to create parent directories".to_string(),
                            suggestion: "Check container filesystem permissions.".to_string(),
                        });
                    }
                }

                // Write file content from stdin; the path is an argument, never shell text.
                let write_cmd = write_file_command(&full_path);
                let (_, stderr, code) = self
                    .exec_in_container(
                        container_id,
                        write_cmd.iter().map(String::as_str).collect(),
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
                        container_id,
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

                for git_cmd in git_clone_commands(url, branch.as_deref(), &full_path)? {
                    let (_, stderr, code) = self
                        .exec_in_container(
                            container_id,
                            git_cmd.iter().map(String::as_str).collect(),
                            None,
                            None,
                        )
                        .await?;
                    if code != 0 {
                        return Err(SandboxError::ProvisionFailed {
                            resource: path.clone(),
                            reason: format!("`{}` failed: {stderr}", git_cmd.join(" ")),
                            suggestion: "Check the repository URL and branch, ensure git is \
                                         installed in the image, and enable network access \
                                         with `DockerClient::with_network_mode` — the default \
                                         mode is `none`."
                                .to_string(),
                        });
                    }
                }
            }
        }
        Ok(())
    }
}

#[async_trait]
impl SandboxClient for DockerClient {
    async fn provision(&self, manifest: &Manifest) -> Result<SessionHandle, SandboxError> {
        self.provision_inner(manifest, None).await
    }

    async fn start(&self, handle: &SessionHandle) -> Result<Box<dyn SandboxSession>, SandboxError> {
        let sessions = self.sessions.read().await;
        let container_id = sessions
            .get(handle.as_str())
            .ok_or_else(|| SandboxError::SessionNotFound { handle: handle.as_str().to_string() })?;

        Ok(Box::new(DockerSession {
            container_id: container_id.clone(),
            client: self.client.clone(),
            // Elitea patch: sessions honour the configured (Code-job) timeout.
            command_timeout: self.command_timeout,
        }))
    }

    async fn stop(&self, handle: &SessionHandle) -> Result<(), SandboxError> {
        // Elitea patch: the session stays registered until removal is confirmed, so a failed
        // cleanup can be retried; an already-removed container counts as removed.
        let sessions = self.sessions.read().await;
        let container_id = sessions
            .get(handle.as_str())
            .cloned()
            .ok_or_else(|| SandboxError::SessionNotFound { handle: handle.as_str().to_string() })?;
        drop(sessions);

        // Stop the container (with a short grace period)
        let stop_options = StopContainerOptions { t: 5 };
        let _ = self.client.stop_container(&container_id, Some(stop_options)).await;

        // Remove the container
        let remove_options = RemoveContainerOptions { force: true, ..Default::default() };
        match self.client.remove_container(&container_id, Some(remove_options)).await {
            Ok(())
            | Err(bollard::errors::Error::DockerResponseServerError { status_code: 404, .. }) => {}
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
        // Elitea patch: Code-job state lives in tmpfs and durable receipts, not images.
        if self.code_job_policy {
            return Err(SandboxError::ExecutionFailed(
                "Code jobs require durable result receipts; container snapshots do not preserve tmpfs state".into(),
            ));
        }
        let sessions = self.sessions.read().await;
        let container_id = sessions
            .get(handle.as_str())
            .ok_or_else(|| SandboxError::SessionNotFound { handle: handle.as_str().to_string() })?
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

        self.client.commit_container(commit_options, Config::<String>::default()).await.map_err(
            |e| SandboxError::ExecutionFailed(format!("failed to commit container as image: {e}")),
        )?;

        let image_ref = format!("{repo}:{snapshot_tag}");
        Ok(SnapshotId::new(image_ref))
    }

    async fn resume(&self, snapshot_id: &SnapshotId) -> Result<SessionHandle, SandboxError> {
        // Elitea patch: Code-job state lives in tmpfs and durable receipts, not images.
        if self.code_job_policy {
            return Err(SandboxError::ExecutionFailed(
                "Code jobs require durable result receipts; container snapshots do not preserve tmpfs state".into(),
            ));
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
        self.client.start_container::<String>(&container_id, None).await.map_err(|e| {
            SandboxError::ProvisionFailed {
                resource: image_ref.to_string(),
                reason: format!("failed to start resumed container: {e}"),
                suggestion: "Check Docker daemon status.".to_string(),
            }
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
    /// Executes a command inside the container and captures output as text.
    async fn exec_cmd(
        &self,
        cmd: Vec<&str>,
        working_dir: Option<&str>,
        stdin_content: Option<&[u8]>,
    ) -> Result<(String, String, i64), SandboxError> {
        let (stdout, stderr, exit_code) =
            exec_collect(&self.client, &self.container_id, cmd, working_dir, stdin_content).await?;
        // Elitea patch: the decoded text stays within the capture bound.
        let (stdout, stderr) = bounded_text(&stdout, &stderr)?;
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

        let pid_file = format!("/tmp/.adk-exec-{}.pid", uuid::Uuid::new_v4());
        let exec_cmd = timed_exec_command(command, &pid_file);
        let result = tokio::time::timeout(
            self.command_timeout,
            self.exec_cmd(exec_cmd.iter().map(String::as_str).collect(), Some(&cwd), None),
        )
        .await;

        match result {
            Ok(Ok((stdout, stderr, exit_code))) => {
                let duration = start.elapsed();
                Ok(ExecOutput::new(stdout, stderr, exit_code as i32, duration, false))
            }
            // Elitea patch: an exec whose outcome is unknown (stream/input failure, capture
            // bound) kills the whole workload rather than leaving code running.
            Ok(Err(e)) => {
                self.kill_workload().await?;
                Err(e)
            }
            Err(_) => {
                // Dropping the attached stream does not stop a Docker exec, so the command
                // and its descendants are killed from a second exec.
                let kill_cmd = kill_tree_command(&pid_file);
                let killed = tokio::time::timeout(
                    KILL_TIMEOUT,
                    self.exec_cmd(kill_cmd.iter().map(String::as_str).collect(), None, None),
                )
                .await;
                match killed {
                    Ok(Ok(_)) => {}
                    Ok(Err(error)) => tracing::warn!(
                        container.id = %self.container_id,
                        error = %error,
                        "failed to kill a timed-out command"
                    ),
                    Err(_) => tracing::warn!(
                        container.id = %self.container_id,
                        "killing a timed-out command did not finish"
                    ),
                }
                // Elitea patch: the in-container tree kill is best effort; killing the
                // container is the guarantee, and its failure is reported to the caller.
                self.kill_workload().await?;
                let duration = start.elapsed();
                Ok(ExecOutput::new("", "", -1, duration, true))
            }
        }
    }

    async fn read_file(&self, path: &str) -> Result<Vec<u8>, SandboxError> {
        validate_relative_path(path)?;
        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

        // Raw bytes: decoding to text would replace every non-UTF-8 byte of a binary file.
        let (stdout, stderr, exit_code) = exec_collect(
            &self.client,
            &self.container_id,
            vec!["cat", "--", &full_path],
            None,
            None,
        )
        .await?;

        if exit_code != 0 {
            let stderr = String::from_utf8_lossy(&stderr);
            if stderr.contains("No such file") {
                return Err(SandboxError::ExecutionFailed(format!("file not found: {path}")));
            }
            return Err(SandboxError::ExecutionFailed(format!(
                "failed to read file '{path}': {stderr}"
            )));
        }

        Ok(stdout)
    }

    async fn write_file(&self, path: &str, content: &[u8]) -> Result<(), SandboxError> {
        validate_relative_path(path)?;
        let full_path = format!("{CONTAINER_WORKSPACE_ROOT}/{path}");

        // Create parent directories
        if let Some(parent_idx) = full_path.rfind('/') {
            let parent = &full_path[..parent_idx];
            let (_, _, code) = self.exec_cmd(vec!["mkdir", "-p", parent], None, None).await?;
            if code != 0 {
                return Err(SandboxError::ExecutionFailed(format!(
                    "failed to create parent directories for '{path}'"
                )));
            }
        }

        // Write content via stdin; the path is an argument, never shell text.
        let write_cmd = write_file_command(&full_path);
        let (_, stderr, exit_code) = self
            .exec_cmd(write_cmd.iter().map(String::as_str).collect(), None, Some(content))
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
        let (stdout, stderr, exit_code) =
            self.exec_cmd(vec!["ls", "-1F", &full_path], None, None).await?;

        if exit_code != 0 {
            if stderr.contains("No such file") || stderr.contains("cannot access") {
                return Err(SandboxError::ExecutionFailed(format!("directory not found: {path}")));
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
            return Err(SandboxError::ExecutionFailed(format!("patch failed: {stderr}")));
        }

        Ok(())
    }
}

// Elitea patch: SIGKILL the container; success when it is confirmed not running.
impl DockerSession {
    async fn kill_workload(&self) -> Result<(), SandboxError> {
        match self
            .client
            .kill_container(&self.container_id, Some(KillContainerOptions { signal: "SIGKILL" }))
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

    fn owned(args: &[&str]) -> Vec<String> {
        args.iter().map(|arg| (*arg).to_string()).collect()
    }

    #[test]
    fn host_config_defaults_drop_capabilities_and_network() {
        let config = host_config(None, None, DEFAULT_NETWORK_MODE, Some(DEFAULT_PIDS_LIMIT));

        assert_eq!(config.network_mode.as_deref(), Some("none"));
        assert_eq!(config.cap_drop, Some(owned(&["ALL"])));
        assert_eq!(config.security_opt, Some(owned(&["no-new-privileges"])));
        assert_eq!(config.pids_limit, Some(DEFAULT_PIDS_LIMIT));
        assert_eq!(config.memory, None);
        assert_eq!(config.nano_cpus, None);
        assert_eq!(config.cap_add, None, "no capability may be added back");
        assert_eq!(config.privileged, None);
    }

    #[test]
    fn host_config_applies_caller_choices() {
        let config = host_config(Some(512 * 1024 * 1024), Some(1.5), "bridge", None);

        assert_eq!(config.network_mode.as_deref(), Some("bridge"));
        assert_eq!(config.memory, Some(512 * 1024 * 1024));
        assert_eq!(config.nano_cpus, Some(1_500_000_000));
        assert_eq!(config.pids_limit, None);
        assert_eq!(config.cap_drop, Some(owned(&["ALL"])), "capabilities stay dropped");
    }

    #[test]
    fn host_config_saturates_an_out_of_range_memory_limit() {
        let config = host_config(Some(u64::MAX), None, DEFAULT_NETWORK_MODE, None);
        assert_eq!(config.memory, Some(i64::MAX));
    }

    #[test]
    fn write_file_command_passes_the_path_as_an_argument() {
        let path = "/workspace/x'; touch /pwned; echo '.txt";
        assert_eq!(
            write_file_command(path),
            owned(&["sh", "-c", r#"cat > "$1""#, "adk-write", path]),
            "the path must stay out of the script text"
        );
    }

    /// Runs the write command with the host's `sh`, which interprets it exactly as the
    /// container's would.
    #[cfg(unix)]
    #[test]
    fn write_file_command_writes_hostile_paths_literally() {
        use std::io::Write;

        let directory = tempfile::tempdir().unwrap();
        let canary = directory.path().join("pwned");
        // Relative, so an injected `touch pwned` would land in the directory under test.
        let name = "x'; touch pwned; echo '$(touch pwned) \"q\".txt";
        let hostile = directory.path().join(name);
        let argv = write_file_command(name);

        let mut child = std::process::Command::new(&argv[0])
            .args(&argv[1..])
            .current_dir(directory.path())
            .stdin(std::process::Stdio::piped())
            .spawn()
            .unwrap();
        child.stdin.take().unwrap().write_all(b"payload").unwrap();
        assert!(child.wait().unwrap().success());

        assert_eq!(std::fs::read(&hostile).unwrap(), b"payload");
        assert!(!canary.exists(), "the path was executed as shell code");
    }

    #[test]
    fn git_clone_commands_pass_url_path_and_branch_as_arguments() {
        let commands =
            git_clone_commands("https://x/'$(id)'.git", Some("main"), "/workspace/r'p").unwrap();

        assert_eq!(
            commands,
            vec![
                owned(&["git", "clone", "--", "https://x/'$(id)'.git", "/workspace/r'p"]),
                owned(&["git", "-C", "/workspace/r'p", "checkout", "main"]),
            ]
        );
        assert_eq!(git_clone_commands("u", None, "/workspace/p").unwrap().len(), 1);
    }

    #[test]
    fn git_clone_commands_reject_a_branch_that_parses_as_an_option() {
        let result = git_clone_commands("u", Some("--upload-pack=touch /pwned"), "/workspace/p");
        assert!(matches!(result, Err(SandboxError::ProvisionFailed { .. })), "{result:?}");
    }

    #[test]
    fn timed_exec_command_passes_the_command_as_an_argument() {
        assert_eq!(
            timed_exec_command("echo 'hi'", "/tmp/p.pid"),
            owned(&["sh", "-c", TIMED_EXEC_SCRIPT, "adk-exec", "echo 'hi'", "/tmp/p.pid"])
        );
        assert_eq!(
            kill_tree_command("/tmp/p.pid"),
            owned(&["sh", "-c", KILL_TREE_SCRIPT, "adk-kill", "/tmp/p.pid"])
        );
    }

    /// The wrapper preserves output and exit status and removes its PID file.
    #[cfg(unix)]
    #[test]
    fn timed_exec_script_reports_the_command_result() {
        let directory = tempfile::tempdir().unwrap();
        let pid_file = directory.path().join("exec.pid");
        let argv = timed_exec_command("echo out; echo err >&2; exit 7", pid_file.to_str().unwrap());

        let output = std::process::Command::new(&argv[0]).args(&argv[1..]).output().unwrap();

        assert_eq!(output.status.code(), Some(7));
        assert_eq!(String::from_utf8_lossy(&output.stdout), "out\n");
        assert_eq!(String::from_utf8_lossy(&output.stderr), "err\n");
        assert!(!pid_file.exists(), "the PID file must be removed after the command");
    }

    /// The kill script ends the wrapper and the command's background children.
    ///
    /// Containers run each exec as a session leader; the wrapper is placed in its own
    /// process group here to match.
    #[cfg(unix)]
    #[test]
    fn kill_tree_script_kills_the_command_and_its_children() {
        use std::os::unix::process::CommandExt;

        let directory = tempfile::tempdir().unwrap();
        let pid_file = directory.path().join("exec.pid");
        let marker = directory.path().join("survivor");
        let command = format!("(sleep 1; touch '{}') & sleep 30", marker.display());
        let argv = timed_exec_command(&command, pid_file.to_str().unwrap());

        let mut wrapper =
            std::process::Command::new(&argv[0]).args(&argv[1..]).process_group(0).spawn().unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while !pid_file.exists() {
            assert!(std::time::Instant::now() < deadline, "the wrapper never wrote its PID");
            std::thread::sleep(Duration::from_millis(10));
        }

        let kill = kill_tree_command(pid_file.to_str().unwrap());
        let status = std::process::Command::new(&kill[0]).args(&kill[1..]).status().unwrap();
        assert!(status.success());

        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        loop {
            if wrapper.try_wait().unwrap().is_some() {
                break;
            }
            assert!(std::time::Instant::now() < deadline, "the wrapper survived the kill");
            std::thread::sleep(Duration::from_millis(10));
        }
        std::thread::sleep(Duration::from_millis(1_500));
        assert!(!marker.exists(), "a background child survived the kill");
        assert!(!pid_file.exists(), "the kill script must remove the PID file");
    }

    // Elitea patch: tests for the Code-job policy and the capture bound.
    #[test]
    fn runtime_images_require_complete_immutable_identity() {
        let digest = "a".repeat(64);
        assert!(immutable_image_reference(&format!("sha256:{digest}")));
        assert!(immutable_image_reference(&format!("registry/runtime@sha256:{digest}")));
        for bad in ["runtime:latest", "runtime:1.2", "sha256:abc", "@sha256:abc", "sha256:"] {
            assert!(!immutable_image_reference(bad));
        }
        assert!(!immutable_image_reference(&format!("registry/ runtime@sha256:{digest}")));
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
        // Raw bytes that fit can still overflow once decoded.
        let raw = vec![255u8; MAX_CAPTURE_BYTES / 2];
        assert!(bounded_text(&raw, b"").is_err());
        assert!(bounded_text(b"out", b"err").is_ok());
    }

    fn bounded_policy() -> DockerClient {
        DockerClient {
            base_image: format!("sha256:{}", "a".repeat(64)),
            memory_limit_bytes: Some(536_870_912),
            cpu_limit: Some(1.0),
            network_mode: DEFAULT_NETWORK_MODE.to_string(),
            pids_limit: Some(DEFAULT_PIDS_LIMIT),
            code_job_policy: true,
            code_workspace_execution: false,
            code_platform_profile: false,
            code_workspace_bytes: 256 * 1024 * 1024,
            code_preparation_network: None,
            command_timeout: Duration::from_secs(60),
            client: Docker::connect_with_http_defaults().unwrap(),
            sessions: RwLock::new(HashMap::new()),
        }
    }

    #[test]
    fn code_job_policy_cannot_be_loosened_by_upstream_knobs() {
        let host = bounded_policy()
            .with_network_mode("bridge")
            .with_pids_limit(None)
            .build_host_config();
        assert_eq!(host.network_mode.as_deref(), Some("none"));
        assert_eq!(host.pids_limit, Some(CODE_JOB_PIDS_LIMIT));
        assert_eq!(host.cap_drop, Some(owned(&["ALL"])));
        assert_eq!(host.cap_add, None);
        assert_eq!(host.security_opt, Some(owned(&["no-new-privileges:true"])));
        assert_eq!(host.readonly_rootfs, Some(true));
        assert_eq!(host.memory_swap, host.memory);
        let tmpfs = host.tmpfs.unwrap();
        assert!(tmpfs["/workspace"].contains(",noexec,"));
        assert!(tmpfs["/tmp"].contains(",noexec,"));
        // A lower caller limit is kept.
        assert_eq!(bounded_policy().with_pids_limit(Some(32)).build_host_config().pids_limit, Some(32));
    }

    #[test]
    fn preparation_network_is_opt_in_and_cannot_enable_executable_scratch() {
        assert_eq!(bounded_policy().build_host_config().network_mode.as_deref(), Some("none"));
        for network in ["host", "none", "bridge", "", "network/escape", "x\n", "x y"] {
            assert!(bounded_policy().with_code_preparation_network(network.into()).is_err());
        }
        let preparation =
            bounded_policy().with_code_preparation_network("elitea-package-resolver".into()).unwrap();
        let host = preparation.build_host_config();
        assert_eq!(host.network_mode.as_deref(), Some("elitea-package-resolver"));
        assert_eq!(host.memory_swap, host.memory);
        assert_eq!(host.readonly_rootfs, Some(true));
        assert!(host.tmpfs.unwrap()["/workspace"].contains(",noexec,"));
        assert!(preparation.with_code_compilation().is_err());
        assert!(
            bounded_policy()
                .with_code_compilation()
                .unwrap()
                .with_code_preparation_network("elitea-package-resolver".into())
                .is_err()
        );
    }
}

// Elitea patch: Code-job modules (see libs/rust/vendor/README.md).
#[cfg(test)]
#[path = "docker_live_tests.rs"]
mod live_tests;

#[path = "docker_code_jobs.rs"]
mod code_jobs;
#[path = "docker_code_platform.rs"]
mod code_platform;
pub use code_jobs::{CodeJobExit, CodeJobIdentity, CodeJobObservation};
