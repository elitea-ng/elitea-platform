//! Browser sessions on Docker (ADR-0049 decision 1): one container per
//! session from `BrowserSessionContainer::docker_plan`, the session agent as
//! an attached exec whose stdin and stdout carry the line protocol.
//!
//! **Isolation.** The profile names the Docker runtime. `runsc` (gVisor) is
//! the only one under which the engine may run with `--no-sandbox`; the
//! profile is refused at startup otherwise, and [`DockerBrowserSessions`]
//! reports the configured runtime's isolation to the service, which applies
//! the same rule per start.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use bollard::Docker;
use bollard::container::{
    Config, CreateContainerOptions, ListContainersOptions, LogOutput, RemoveContainerOptions,
};
use bollard::exec::{CreateExecOptions, StartExecResults};
use bollard::models::HostConfig;
use futures_util::StreamExt as _;
use tokio::io::AsyncWriteExt as _;

use super::browser_session::{
    ACTIVATION_BINDING, AGENT_PATH, EXPIRES_BINDING, Isolation, KIND_BROWSER_SESSION, KIND_LABEL,
    REQUEST_BINDING,
};
use super::browser_session_agent::{LineAgent, SessionAgent};
use super::browser_session_service::{BrowserSessionBackend, BrowserSessionLaunch};
use super::workspace_run_service::BackendError;

const API_TIMEOUT: Duration = Duration::from_mins(1);
const PIPE_BYTES: usize = 1 << 20;

/// The Docker backend of one browser profile.
pub struct DockerBrowserSessions {
    docker: Docker,
    proxy_network: Option<String>,
    runtime: Option<String>,
}

impl DockerBrowserSessions {
    /// Connect to the local daemon.
    ///
    /// # Errors
    ///
    /// The daemon cannot be reached.
    pub async fn connect(
        proxy_network: Option<String>,
        runtime: Option<String>,
    ) -> Result<Self, BackendError> {
        let docker =
            Docker::connect_with_local_defaults().map_err(|_| BackendError::Unavailable)?;
        tokio::time::timeout(API_TIMEOUT, docker.ping())
            .await
            .map_err(|_| BackendError::Timeout)?
            .map_err(|_| BackendError::Unavailable)?;
        Ok(Self {
            docker,
            proxy_network,
            runtime,
        })
    }
}

/// The labels of one session's container: its kind and its binding.
#[must_use]
pub fn labels(
    launch: &BrowserSessionLaunch,
    plan: &std::collections::BTreeMap<String, String>,
) -> HashMap<String, String> {
    let mut labels: HashMap<String, String> = plan
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

#[async_trait]
impl BrowserSessionBackend for DockerBrowserSessions {
    async fn isolation(&self) -> Result<Isolation, BackendError> {
        Ok(Isolation::of_handler(
            self.runtime.as_deref().unwrap_or_default(),
        ))
    }

    async fn create(&self, launch: &BrowserSessionLaunch) -> Result<(), BackendError> {
        let plan = launch
            .container
            .docker_plan(self.proxy_network.as_deref(), self.runtime.as_deref())
            .map_err(|_| BackendError::Refused)?;
        let host = HostConfig {
            tmpfs: Some(plan.tmpfs.clone().into_iter().collect()),
            shm_size: i64::try_from(plan.shm_bytes).ok(),
            readonly_rootfs: Some(plan.readonly_rootfs),
            cap_drop: Some(plan.cap_drop.clone()),
            security_opt: Some(plan.security_opt.clone()),
            pids_limit: Some(plan.pids_limit),
            memory: i64::try_from(plan.memory_bytes).ok(),
            memory_swap: i64::try_from(plan.memory_bytes).ok(),
            nano_cpus: Some(plan.nano_cpus),
            network_mode: Some(plan.network_mode.clone()),
            dns: (!plan.dns.is_empty()).then(|| plan.dns.clone()),
            runtime: plan.runtime.clone(),
            init: Some(true),
            ..Default::default()
        };
        let config = Config {
            image: Some(plan.image.clone()),
            entrypoint: Some(plan.entrypoint.clone()),
            cmd: Some(Vec::new()),
            user: Some(plan.user.clone()),
            working_dir: Some("/tmp".to_owned()),
            labels: Some(labels(launch, &plan.labels)),
            env: Some(vec!["HOME=/tmp".to_owned()]),
            network_disabled: Some(plan.network_mode == "none"),
            host_config: Some(host),
            ..Default::default()
        };
        let created = tokio::time::timeout(
            API_TIMEOUT,
            self.docker.create_container(
                Some(CreateContainerOptions {
                    name: plan.name.clone(),
                    platform: None,
                }),
                config,
            ),
        )
        .await
        .map_err(|_| BackendError::Timeout)?;
        let id = match created {
            Ok(created) => created.id,
            // A session is never reused: an existing name is never adopted.
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 409, ..
            }) => return Err(BackendError::Conflict),
            Err(_) => return Err(BackendError::Unavailable),
        };
        if self
            .docker
            .start_container::<String>(&id, None)
            .await
            .is_err()
        {
            let _ = self.destroy(&plan.name).await;
            return Err(BackendError::Unavailable);
        }
        Ok(())
    }

    async fn attach(&self, name: &str) -> Result<Arc<dyn SessionAgent>, BackendError> {
        let exec = self
            .docker
            .create_exec(
                name,
                CreateExecOptions {
                    cmd: Some(vec![AGENT_PATH.to_owned(), "serve".to_owned()]),
                    attach_stdin: Some(true),
                    attach_stdout: Some(true),
                    attach_stderr: Some(true),
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
        let (agent_stdin, mut to_exec) = tokio::io::duplex(PIPE_BYTES);
        let (mut from_exec, agent_stdout) = tokio::io::duplex(PIPE_BYTES);
        tokio::spawn(async move {
            let _ = tokio::io::copy(&mut to_exec, &mut input).await;
            let _ = input.shutdown().await;
        });
        tokio::spawn(async move {
            while let Some(Ok(message)) = output.next().await {
                if let LogOutput::StdOut { message } = message
                    && from_exec.write_all(&message).await.is_err()
                {
                    break;
                }
                // stderr is the agent's own log; it never carries a reply.
            }
            let _ = from_exec.shutdown().await;
        });
        Ok(LineAgent::new(agent_stdin, agent_stdout))
    }

    async fn destroy(&self, name: &str) -> Result<bool, BackendError> {
        match self
            .docker
            .remove_container(
                name,
                Some(RemoveContainerOptions {
                    force: true,
                    v: true,
                    ..Default::default()
                }),
            )
            .await
        {
            Ok(()) => Ok(true),
            Err(bollard::errors::Error::DockerResponseServerError {
                status_code: 404, ..
            }) => Ok(false),
            Err(_) => Err(BackendError::Unavailable),
        }
    }

    async fn sessions(&self) -> Result<Vec<String>, BackendError> {
        let containers = self
            .docker
            .list_containers(Some(ListContainersOptions::<String> {
                all: true,
                filters: HashMap::from([(
                    "label".to_owned(),
                    vec![format!("{KIND_LABEL}={KIND_BROWSER_SESSION}")],
                )]),
                ..Default::default()
            }))
            .await
            .map_err(|_| BackendError::Unavailable)?;
        Ok(containers
            .into_iter()
            .filter_map(|container| {
                Some(container.names?.first()?.trim_start_matches('/').to_owned())
            })
            .collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::browser_session::{Engine, tests::session};
    use crate::sandbox::workspace_run_service::RunBinding;

    #[test]
    fn the_container_carries_its_kind_and_binding() {
        let launch = BrowserSessionLaunch {
            container: session(Engine::Full),
            binding: RunBinding {
                request: "a".repeat(64),
                owner: "b".repeat(64),
                expires_at: 1_700_000_000,
            },
            backstop_seconds: 4_320,
        };
        let plan = launch
            .container
            .docker_plan(Some("elitea-browser-proxy"), Some("runsc"))
            .expect("plan");
        let labels = labels(&launch, &plan.labels);
        assert_eq!(labels[KIND_LABEL], KIND_BROWSER_SESSION);
        assert_eq!(labels[REQUEST_BINDING], "a".repeat(64));
        assert_eq!(labels[ACTIVATION_BINDING], "b".repeat(64));
        assert_eq!(labels[EXPIRES_BINDING], "1700000000");
    }
}
