//! Workspace runs on Docker (ADR-0034 decision 9): one container per run
//! from `WorkspaceRunContainer::docker_plan`, its workspace a named local
//! volume, the runner's operations as attached execs.
//!
//! **Workspace size.** Docker's local volume driver cannot size a volume,
//! so the runner enforces the cap (`--max-workspace-bytes`: after each tool
//! that can grow the tree, and before `pack`). Where the storage driver can
//! cap a container's writable layer (`btrfs`, `zfs`, `devicemapper`, or
//! `overlay2` on `xfs` with project quotas), the container is also created
//! with `--storage-opt size=`; the driver is DETECTED from `docker info`,
//! and an `overlay2`-on-`xfs` daemon that refuses the option (no `pquota`)
//! is remembered and the run is created without it.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use async_trait::async_trait;
use bollard::Docker;
use bollard::container::{
    Config, CreateContainerOptions, ListContainersOptions, LogOutput, RemoveContainerOptions,
};
use bollard::exec::{CreateExecOptions, StartExecResults};
use bollard::models::{HostConfig, Mount, MountTypeEnum};
use bollard::volume::{CreateVolumeOptions, RemoveVolumeOptions};
use futures_util::StreamExt as _;
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWrite, AsyncWriteExt as _};

use super::workspace_run::{
    ACTIVATION_BINDING, EXPIRES_BINDING, KIND_LABEL, KIND_WORKSPACE_RUN, REQUEST_BINDING,
    WORKSPACE_MOUNT,
};
use super::workspace_run_service::{
    BackendError, ExecExit, RunBinding, WorkspaceRunBackend, WorkspaceRunLaunch, failure_code,
};

const API_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_STDERR_BYTES: usize = 8 * 1024;
/// The size headroom of the writable layer over the workspace (tools, git,
/// `/home`), when a quota can be set.
const LAYER_HEADROOM_BYTES: u64 = 1 << 30;

/// Whether a storage driver can size a container's writable layer.
#[must_use]
pub fn storage_opt_supported(driver: &str, backing_filesystem: Option<&str>) -> bool {
    match driver {
        "btrfs" | "zfs" | "devicemapper" | "windowsfilter" => true,
        "overlay2" => backing_filesystem == Some("xfs"),
        _ => false,
    }
}

/// The Docker backend of one workspace profile.
pub struct DockerWorkspaceRuns {
    docker: Docker,
    egress_network: Option<String>,
    egress_dns: Vec<String>,
    storage_opt: AtomicBool,
}

impl DockerWorkspaceRuns {
    /// Connect to the local daemon and detect whether it can size a
    /// container's writable layer.
    ///
    /// # Errors
    ///
    /// The daemon cannot be reached.
    pub async fn connect(
        egress_network: Option<String>,
        egress_dns: Vec<String>,
    ) -> Result<Self, BackendError> {
        let docker =
            Docker::connect_with_local_defaults().map_err(|_| BackendError::Unavailable)?;
        let info = tokio::time::timeout(API_TIMEOUT, docker.info())
            .await
            .map_err(|_| BackendError::Timeout)?
            .map_err(|_| BackendError::Unavailable)?;
        let driver = info.driver.unwrap_or_default();
        let backing = info
            .driver_status
            .unwrap_or_default()
            .into_iter()
            .find_map(|pair| {
                (pair.first().map(String::as_str) == Some("Backing Filesystem"))
                    .then(|| pair.get(1).cloned())
                    .flatten()
            });
        let supported = storage_opt_supported(&driver, backing.as_deref());
        tracing::info!(
            event = "workspace_run_docker_storage",
            driver = driver.as_str(),
            storage_opt = supported
        );
        Ok(Self {
            docker,
            egress_network,
            egress_dns,
            storage_opt: AtomicBool::new(supported),
        })
    }

    fn labels(
        launch: &WorkspaceRunLaunch,
        plan_labels: &std::collections::BTreeMap<String, String>,
    ) -> HashMap<String, String> {
        let mut labels: HashMap<String, String> = plan_labels
            .iter()
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect();
        labels.insert(REQUEST_BINDING.to_owned(), launch.binding.request.clone());
        labels.insert(ACTIVATION_BINDING.to_owned(), launch.binding.owner.clone());
        labels.insert(
            EXPIRES_BINDING.to_owned(),
            launch.binding.expires_at.to_string(),
        );
        labels
    }

    async fn create(
        &self,
        launch: &WorkspaceRunLaunch,
        sized: bool,
    ) -> Result<String, bollard::errors::Error> {
        let plan = launch
            .container
            .docker_plan(self.egress_network.as_deref(), &self.egress_dns)
            .map_err(|_| bollard::errors::Error::DockerResponseServerError {
                status_code: 400,
                message: "invalid workspace run".to_owned(),
            })?;
        let labels = Self::labels(launch, &plan.labels);
        let host = HostConfig {
            mounts: Some(vec![Mount {
                target: Some(WORKSPACE_MOUNT.to_owned()),
                source: Some(plan.workspace_volume.clone()),
                typ: Some(MountTypeEnum::VOLUME),
                read_only: Some(false),
                ..Default::default()
            }]),
            tmpfs: Some(plan.tmpfs.clone().into_iter().collect()),
            readonly_rootfs: Some(plan.readonly_rootfs),
            cap_drop: Some(plan.cap_drop.clone()),
            security_opt: Some(plan.security_opt.clone()),
            pids_limit: Some(plan.pids_limit),
            memory: i64::try_from(plan.memory_bytes).ok(),
            memory_swap: i64::try_from(plan.memory_bytes).ok(),
            nano_cpus: Some(plan.nano_cpus),
            network_mode: Some(plan.network_mode.clone()),
            dns: (!plan.dns.is_empty()).then(|| plan.dns.clone()),
            storage_opt: sized.then(|| {
                HashMap::from([(
                    "size".to_owned(),
                    (plan.workspace_bytes + LAYER_HEADROOM_BYTES).to_string(),
                )])
            }),
            init: Some(true),
            ..Default::default()
        };
        let config = Config {
            image: Some(plan.image.clone()),
            entrypoint: Some(plan.entrypoint.clone()),
            cmd: Some(Vec::new()),
            user: Some(plan.user.clone()),
            working_dir: Some(WORKSPACE_MOUNT.to_owned()),
            labels: Some(labels),
            env: Some(vec!["HOME=/home/elitea".to_owned()]),
            network_disabled: Some(plan.network_mode == "none"),
            host_config: Some(host),
            ..Default::default()
        };
        let created = self
            .docker
            .create_container(
                Some(CreateContainerOptions {
                    name: plan.name.clone(),
                    platform: None,
                }),
                config,
            )
            .await?;
        Ok(created.id)
    }

    async fn container_binding(&self, name: &str) -> Result<Option<RunBinding>, BackendError> {
        match tokio::time::timeout(API_TIMEOUT, self.docker.inspect_container(name, None))
            .await
            .map_err(|_| BackendError::Timeout)?
        {
            Ok(info) => {
                let labels = info
                    .config
                    .and_then(|config| config.labels)
                    .unwrap_or_default();
                Ok(binding_of(&labels))
            }
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => Ok(None),
            Err(_) => Err(BackendError::Unavailable),
        }
    }
}

fn binding_of(labels: &HashMap<String, String>) -> Option<RunBinding> {
    if labels.get(KIND_LABEL).map(String::as_str) != Some(KIND_WORKSPACE_RUN) {
        return None;
    }
    Some(RunBinding {
        request: labels.get(REQUEST_BINDING)?.clone(),
        owner: labels.get(ACTIVATION_BINDING)?.clone(),
        expires_at: labels.get(EXPIRES_BINDING)?.parse().ok()?,
    })
}

#[async_trait]
impl WorkspaceRunBackend for DockerWorkspaceRuns {
    async fn start(&self, launch: &WorkspaceRunLaunch) -> Result<bool, BackendError> {
        if let Some(existing) = self.container_binding(&launch.container.name).await? {
            return if existing.request == launch.binding.request
                && existing.owner == launch.binding.owner
            {
                Ok(false)
            } else {
                Err(BackendError::Conflict)
            };
        }
        let plan = launch
            .container
            .docker_plan(self.egress_network.as_deref(), &self.egress_dns)
            .map_err(|_| BackendError::Refused)?;
        let labels = Self::labels(launch, &plan.labels);
        self.docker
            .create_volume(CreateVolumeOptions {
                name: plan.workspace_volume.clone(),
                driver: "local".to_owned(),
                driver_opts: HashMap::new(),
                labels: labels.clone(),
            })
            .await
            .map_err(|_| BackendError::Unavailable)?;
        let sized = self.storage_opt.load(Ordering::Relaxed);
        let id = match self.create(launch, sized).await {
            Ok(id) => id,
            // overlay2 on xfs without `pquota` refuses the option: create
            // without it and stop asking (the runner's cap still holds).
            Err(bollard::errors::Error::DockerResponseServerError { message, .. })
                if sized && message.contains("storage-opt") =>
            {
                self.storage_opt.store(false, Ordering::Relaxed);
                tracing::warn!(
                    event = "workspace_run_storage_opt_unsupported",
                    "the daemon refused --storage-opt size; the runner's cap is the bound"
                );
                self.create(launch, false)
                    .await
                    .map_err(|_| BackendError::Unavailable)?
            }
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 409, ..
            }) => {
                return match self.container_binding(&launch.container.name).await? {
                    Some(existing)
                        if existing.request == launch.binding.request
                            && existing.owner == launch.binding.owner =>
                    {
                        Ok(false)
                    }
                    _ => Err(BackendError::Conflict),
                };
            }
            Err(_) => {
                let _ = self.destroy(&launch.container.name).await;
                return Err(BackendError::Unavailable);
            }
        };
        if self
            .docker
            .start_container::<String>(&id, None)
            .await
            .is_err()
        {
            let _ = self.destroy(&launch.container.name).await;
            return Err(BackendError::Unavailable);
        }
        Ok(true)
    }

    async fn binding(&self, name: &str) -> Result<Option<RunBinding>, BackendError> {
        self.container_binding(name).await
    }

    async fn exec(
        &self,
        name: &str,
        argv: &[String],
        stdin: &mut (dyn AsyncRead + Unpin + Send),
        stdout: &mut (dyn AsyncWrite + Unpin + Send),
    ) -> Result<ExecExit, BackendError> {
        let exec = self
            .docker
            .create_exec(
                name,
                CreateExecOptions {
                    cmd: Some(argv.to_vec()),
                    attach_stdin: Some(true),
                    attach_stdout: Some(true),
                    attach_stderr: Some(true),
                    working_dir: Some(WORKSPACE_MOUNT.to_owned()),
                    ..Default::default()
                },
            )
            .await
            .map_err(|_| BackendError::Unavailable)?;
        let StartExecResults::Attached {
            mut input,
            mut output,
        } = self
            .docker
            .start_exec(&exec.id, None)
            .await
            .map_err(|_| BackendError::Unavailable)?
        else {
            return Err(BackendError::Unavailable);
        };
        let send = async {
            let mut buffer = vec![0_u8; 32 * 1024];
            loop {
                let count = stdin
                    .read(&mut buffer)
                    .await
                    .map_err(|_| BackendError::Unavailable)?;
                if count == 0 {
                    break;
                }
                input
                    .write_all(&buffer[..count])
                    .await
                    .map_err(|_| BackendError::Unavailable)?;
                input.flush().await.map_err(|_| BackendError::Unavailable)?;
            }
            input
                .shutdown()
                .await
                .map_err(|_| BackendError::Unavailable)
        };
        let receive = async {
            let mut stderr = Vec::new();
            while let Some(message) = output.next().await {
                match message.map_err(|_| BackendError::Unavailable)? {
                    LogOutput::StdOut { message } => {
                        stdout
                            .write_all(&message)
                            .await
                            .map_err(|_| BackendError::Unavailable)?;
                        stdout
                            .flush()
                            .await
                            .map_err(|_| BackendError::Unavailable)?;
                    }
                    LogOutput::StdErr { message } => {
                        let room = MAX_STDERR_BYTES.saturating_sub(stderr.len());
                        stderr.extend_from_slice(&message[..message.len().min(room)]);
                    }
                    _ => {}
                }
            }
            Ok::<_, BackendError>(stderr)
        };
        // stdin may end before stdout (pack) or long after (serve's EOF ends
        // it); a send failure after the runner exited is not an error.
        let (delivered, stderr) = tokio::join!(send, receive);
        let stderr = stderr?;
        let status = self
            .docker
            .inspect_exec(&exec.id)
            .await
            .map_err(|_| BackendError::Unavailable)?;
        let exit_code = status
            .exit_code
            .and_then(|code| i32::try_from(code).ok())
            .unwrap_or(-1);
        if exit_code == 0 {
            delivered?;
        }
        Ok(ExecExit {
            exit_code,
            failure_code: failure_code(&stderr),
        })
    }

    async fn destroy(&self, name: &str) -> Result<bool, BackendError> {
        let removed = match self
            .docker
            .remove_container(
                name,
                Some(RemoveContainerOptions {
                    force: true,
                    v: false,
                    ..Default::default()
                }),
            )
            .await
        {
            Ok(()) => true,
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => false,
            Err(_) => return Err(BackendError::Unavailable),
        };
        match self
            .docker
            .remove_volume(
                &format!("{name}-workspace"),
                Some(RemoveVolumeOptions { force: true }),
            )
            .await
        {
            Ok(())
            | Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => Ok(removed),
            Err(_) => Err(BackendError::Unavailable),
        }
    }

    async fn runs(&self) -> Result<Vec<(String, RunBinding)>, BackendError> {
        let containers = self
            .docker
            .list_containers(Some(ListContainersOptions::<String> {
                all: true,
                filters: HashMap::from([(
                    "label".to_owned(),
                    vec![format!("{KIND_LABEL}={KIND_WORKSPACE_RUN}")],
                )]),
                ..Default::default()
            }))
            .await
            .map_err(|_| BackendError::Unavailable)?;
        Ok(containers
            .into_iter()
            .filter_map(|container| {
                let name = container.names?.first()?.trim_start_matches('/').to_owned();
                Some((name, binding_of(&container.labels.unwrap_or_default())?))
            })
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_layer_quota_is_used_only_where_the_driver_can_set_it() {
        assert!(storage_opt_supported("overlay2", Some("xfs")));
        assert!(!storage_opt_supported("overlay2", Some("extfs")));
        assert!(!storage_opt_supported("overlay2", None));
        assert!(!storage_opt_supported("vfs", None));
        for driver in ["btrfs", "zfs", "devicemapper"] {
            assert!(storage_opt_supported(driver, None), "{driver}");
        }
    }

    #[test]
    fn only_a_workspace_run_with_its_whole_binding_is_one() {
        let labels = HashMap::from([
            (KIND_LABEL.to_owned(), KIND_WORKSPACE_RUN.to_owned()),
            (REQUEST_BINDING.to_owned(), "a".repeat(64)),
            (ACTIVATION_BINDING.to_owned(), "b".repeat(64)),
            (EXPIRES_BINDING.to_owned(), "1700000000".to_owned()),
        ]);
        assert_eq!(
            binding_of(&labels).expect("binding").expires_at,
            1_700_000_000
        );
        let mut other = labels.clone();
        other.insert(KIND_LABEL.to_owned(), "code".to_owned());
        assert!(binding_of(&other).is_none());
        let mut partial = labels;
        partial.remove(ACTIVATION_BINDING);
        assert!(binding_of(&partial).is_none());
    }
}
