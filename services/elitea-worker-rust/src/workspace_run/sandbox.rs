//! The workspace sandbox as the turn orchestrator sees it (ADR-0034
//! decision 9): one container per turn, disk-backed and writable, running the
//! workspace runner. The worker moves the tree in and out through the
//! sandbox helper protocol; the sandbox itself never reaches the store.
//!
//! [`WorkspaceRunSpec`] is the deployment-independent description of that
//! container; `sandbox::workspace_run` renders it for Docker and Kubernetes.

use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::Value;

use super::context::WorkspaceRuntimeContext;

/// Bytes added to the declared unpacked size for builds, at least.
pub(crate) const MIN_BUILD_HEADROOM_BYTES: u64 = 1 << 30;
/// The pids limit of a workspace run (a toolkit Code job has 128).
pub(crate) const WORKSPACE_RUN_PIDS_LIMIT: i64 = 4_096;

/// The network a workspace run gets.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum WorkspaceNetwork {
    /// Network mode `none`, as a toolkit Code job: no interface but loopback,
    /// no DNS.
    None,
    /// The public internet minus the deny ranges (RFC 1918, link-local,
    /// loopback, metadata, the cluster's ranges), DNS to the resolver only.
    PublicEgress,
}

/// The deployment's defaults and ceilings for workspace runs.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct WorkspaceRunDefaults {
    pub(crate) cpu_millis: u64,
    pub(crate) memory_bytes: u64,
    pub(crate) max_disk_bytes: u64,
    pub(crate) turn_wall: Duration,
    /// Whether the deployment has an egress proxy that enforces a host
    /// allowlist (ADR-0011). None exists yet, so a project allowlist is
    /// refused rather than silently widened to the public internet.
    pub(crate) host_allowlist_proxy: bool,
}

/// One workspace run's container, independent of the backend.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct WorkspaceRunSpec {
    /// Stable for one claim: names the container or pod.
    pub(crate) run_id: String,
    pub(crate) workspace_bytes: u64,
    pub(crate) network: WorkspaceNetwork,
    pub(crate) shell: bool,
    pub(crate) cpu_millis: u64,
    pub(crate) memory_bytes: u64,
    pub(crate) pids_limit: i64,
    pub(crate) turn_wall: Duration,
    /// The runner's archive limits (the policy's file caps).
    pub(crate) max_files: u64,
    pub(crate) max_file_bytes: u64,
    pub(crate) max_total_bytes: u64,
}

/// Why a workspace run cannot be specified or driven. Data-free.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SandboxRunError {
    /// The policy asks for something this deployment cannot enforce.
    PolicyUnenforceable,
    /// The declared tree does not fit the deployment's disk ceiling.
    TooLarge,
    /// The sandbox backend is unavailable.
    Unavailable,
    /// The helper or the runner answered out of protocol.
    Protocol,
    /// The run hit its wall time.
    Timeout,
    /// The workspace grew past its cap (the runner refused to pack it).
    WorkspaceOverCap,
}

impl SandboxRunError {
    #[must_use]
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::PolicyUnenforceable => "workspace_run.policy_unenforceable",
            Self::TooLarge => "workspace_run.workspace_too_large",
            Self::Unavailable => "workspace_run.sandbox_unavailable",
            Self::Protocol => "workspace_run.sandbox_protocol",
            Self::Timeout => "workspace_run.timeout",
            Self::WorkspaceOverCap => "workspace_run.workspace_over_cap",
        }
    }
}

impl WorkspaceRunSpec {
    /// The spec for one turn: policy from the runtime context, limits from
    /// the context where it sets them and the deployment otherwise.
    ///
    /// # Errors
    ///
    /// [`SandboxRunError::PolicyUnenforceable`] for a host allowlist without
    /// an egress proxy; [`SandboxRunError::TooLarge`] when the declared tree
    /// plus headroom exceeds the disk ceiling.
    pub(crate) fn from_context(
        run_id: String,
        context: &WorkspaceRuntimeContext,
        defaults: &WorkspaceRunDefaults,
    ) -> Result<Self, SandboxRunError> {
        if context.network
            && !context.network_allowlist.is_empty()
            && !defaults.host_allowlist_proxy
        {
            return Err(SandboxRunError::PolicyUnenforceable);
        }
        let limits = &context.limits;
        let disk_ceiling = if limits.disk_bytes == 0 {
            defaults.max_disk_bytes
        } else {
            limits.disk_bytes.min(defaults.max_disk_bytes)
        };
        let headroom = limits.unpacked_bytes.max(MIN_BUILD_HEADROOM_BYTES);
        let workspace_bytes = limits
            .unpacked_bytes
            .checked_add(headroom)
            .ok_or(SandboxRunError::TooLarge)?
            .min(disk_ceiling);
        if workspace_bytes < limits.unpacked_bytes || workspace_bytes == 0 {
            return Err(SandboxRunError::TooLarge);
        }
        let pick = |context_value: u64, default: u64| {
            if context_value == 0 {
                default
            } else {
                context_value.min(default)
            }
        };
        Ok(Self {
            run_id,
            workspace_bytes,
            network: if context.network {
                WorkspaceNetwork::PublicEgress
            } else {
                WorkspaceNetwork::None
            },
            shell: context.shell,
            cpu_millis: pick(limits.cpu_millis, defaults.cpu_millis),
            memory_bytes: pick(limits.memory_bytes, defaults.memory_bytes),
            pids_limit: WORKSPACE_RUN_PIDS_LIMIT,
            turn_wall: if limits.turn_wall_seconds == 0 {
                defaults.turn_wall
            } else {
                Duration::from_secs(limits.turn_wall_seconds).min(defaults.turn_wall)
            },
            max_files: limits.max_files,
            max_file_bytes: limits.max_file_bytes,
            max_total_bytes: limits.max_unpacked_bytes.min(workspace_bytes),
        })
    }
}

impl WorkspaceRunSpec {
    /// The wire spec the supervisor renders (its own image).
    #[must_use]
    pub(crate) fn to_wire(&self) -> crate::protocol::elitea::runtime::v1::WorkspaceRunSpecV1 {
        crate::protocol::elitea::runtime::v1::WorkspaceRunSpecV1 {
            revision: 1,
            name: self.run_id.clone(),
            workspace_bytes: self.workspace_bytes,
            egress: self.network == WorkspaceNetwork::PublicEgress,
            cpu_millis: self.cpu_millis,
            memory_bytes: self.memory_bytes,
            pids_limit: self.pids_limit,
            wall_seconds: self.turn_wall.as_secs().max(1),
        }
    }

    /// The backend-independent container of this run, in `image` (the
    /// supervisor renders it; tests check the rendering here).
    #[cfg(test)]
    #[must_use]
    pub(crate) fn container(
        &self,
        image: &str,
    ) -> crate::sandbox::workspace_run::WorkspaceRunContainer {
        crate::sandbox::workspace_run::WorkspaceRunContainer {
            name: self.run_id.clone(),
            image: image.to_owned(),
            workspace_bytes: self.workspace_bytes,
            egress: self.network == WorkspaceNetwork::PublicEgress,
            cpu_millis: self.cpu_millis,
            memory_bytes: self.memory_bytes,
            pids_limit: self.pids_limit,
            wall_seconds: self.turn_wall.as_secs().max(1),
        }
    }
}

/// One running workspace sandbox.
#[async_trait]
pub(crate) trait WorkspaceSandbox: Send + Sync {
    /// Move the checked-out tree into the container's workspace volume.
    async fn push_tree(&self, worktree: &Path) -> Result<(), SandboxRunError>;

    /// Run one tool call in the workspace runner; the JSON result the model
    /// reads (failures included).
    async fn call_tool(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
    ) -> Result<Value, SandboxRunError>;

    /// As [`Self::call_tool`] for a call a person approved through the
    /// server's HITL interrupt: the runner runs it although its rules ask.
    async fn call_tool_approved(
        &self,
        tool: &str,
        call_id: &str,
        args: Value,
        approved: bool,
    ) -> Result<Value, SandboxRunError> {
        let _ = approved;
        self.call_tool(tool, call_id, args).await
    }

    /// Whether `command` must be approved by a person before it runs (the
    /// runner's classification, ADR-0029 addendum item 3). The default
    /// asks: a sandbox that cannot classify never runs a command unasked.
    async fn classify_command(
        &self,
        command: &str,
        cwd: Option<&str>,
    ) -> Result<bool, SandboxRunError> {
        let _ = (command, cwd);
        Ok(true)
    }

    /// Move the container's tree out into `worktree` (an empty directory).
    async fn pull_tree(&self, worktree: &Path) -> Result<(), SandboxRunError>;

    /// Destroy the container and its volume. Idempotent.
    async fn destroy(&self) -> Result<(), SandboxRunError>;
}

/// Starts workspace sandboxes on the deployment's backend.
#[async_trait]
pub(crate) trait WorkspaceSandboxLauncher: Send + Sync {
    /// `env` is the thread's harvested env (contract 6.4), for the runner's
    /// `run_command` environment only (`serve --env-stdin`); empty when the
    /// thread has none. A launcher never logs, labels or annotates it.
    async fn launch(
        &self,
        spec: &WorkspaceRunSpec,
        env: &super::redact::SecretEnv,
        grants: Arc<dyn WorkspaceRunGrants>,
    ) -> Result<Arc<dyn WorkspaceSandbox>, SandboxRunError>;
}

/// Main's sandbox grants for one turn's workspace run, bound to the claim.
#[async_trait]
pub(crate) trait WorkspaceRunGrants: Send + Sync {
    /// A grant for `digest` (`sandbox::workspace_run::spec_digest`) with
    /// the run's name as its activation.
    async fn grant(
        &self,
        activation_id: &str,
        digest: [u8; 32],
    ) -> Result<crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1, SandboxRunError>;
}

/// No claim authority: every grant is refused.
pub(crate) struct NoGrants;

#[async_trait]
impl WorkspaceRunGrants for NoGrants {
    async fn grant(
        &self,
        _activation_id: &str,
        _digest: [u8; 32],
    ) -> Result<crate::protocol::elitea::runtime::v1::SignedSandboxJobGrantV1, SandboxRunError>
    {
        Err(SandboxRunError::Unavailable)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::workspace_run::context::fixture;

    fn defaults() -> WorkspaceRunDefaults {
        WorkspaceRunDefaults {
            cpu_millis: 2_000,
            memory_bytes: 4 << 30,
            max_disk_bytes: 8 << 30,
            turn_wall: Duration::from_mins(30),
            host_allowlist_proxy: false,
        }
    }

    #[test]
    fn the_volume_is_sized_from_the_declared_tree_plus_headroom() {
        let mut context = fixture();
        context.limits.unpacked_bytes = 3 << 30;
        let spec =
            WorkspaceRunSpec::from_context("run".into(), &context, &defaults()).expect("spec");
        assert_eq!(spec.workspace_bytes, 6 << 30);
        assert_eq!(spec.pids_limit, WORKSPACE_RUN_PIDS_LIMIT);
        assert_eq!(spec.network, WorkspaceNetwork::PublicEgress);

        context.limits.unpacked_bytes = 1 << 20;
        let spec =
            WorkspaceRunSpec::from_context("run".into(), &context, &defaults()).expect("spec");
        assert_eq!(spec.workspace_bytes, (1 << 20) + MIN_BUILD_HEADROOM_BYTES);

        context.limits.unpacked_bytes = 9 << 30;
        context.limits.max_unpacked_bytes = 10 << 30;
        assert_eq!(
            WorkspaceRunSpec::from_context("run".into(), &context, &defaults()),
            Err(SandboxRunError::TooLarge)
        );
    }

    #[test]
    fn network_off_is_mode_none_and_an_allowlist_needs_the_proxy() {
        let mut context = fixture();
        context.network = false;
        let spec =
            WorkspaceRunSpec::from_context("run".into(), &context, &defaults()).expect("spec");
        assert_eq!(spec.network, WorkspaceNetwork::None);

        let mut context = fixture();
        context.network_allowlist = vec!["pypi.org".into()];
        assert_eq!(
            WorkspaceRunSpec::from_context("run".into(), &context, &defaults()),
            Err(SandboxRunError::PolicyUnenforceable)
        );
        let mut with_proxy = defaults();
        with_proxy.host_allowlist_proxy = true;
        assert!(WorkspaceRunSpec::from_context("run".into(), &context, &with_proxy).is_ok());
    }

    #[test]
    fn the_spec_renders_a_writable_disk_backed_container_with_the_policy_network() {
        let image = format!(
            "registry.example/workspace-runner@sha256:{}",
            "c".repeat(64)
        );
        let selector =
            std::collections::BTreeMap::from([("sandbox".to_owned(), "true".to_owned())]);
        let spec = WorkspaceRunSpec::from_context("elitea-ws-abc".into(), &fixture(), &defaults())
            .expect("spec");
        let pod = spec
            .container(&image)
            .kubernetes_pod("elitea-execution", "gvisor", &selector)
            .expect("pod");
        assert!(
            pod["spec"]["volumes"][0]["emptyDir"]
                .get("medium")
                .is_none()
        );
        assert_eq!(
            pod["metadata"]["labels"]["sandbox.elitea.ai/egress"],
            "public"
        );
        let mut off = fixture();
        off.network = false;
        let spec = WorkspaceRunSpec::from_context("elitea-ws-abc".into(), &off, &defaults())
            .expect("spec");
        let plan = spec
            .container(&image)
            .docker_plan(Some("elitea-workspace-egress"), &["1.1.1.1".to_owned()])
            .expect("plan");
        assert_eq!(plan.network_mode, "none");
        assert_eq!(plan.pids_limit, WORKSPACE_RUN_PIDS_LIMIT);
    }

    #[test]
    fn context_limits_only_narrow_the_deployment() {
        let mut context = fixture();
        context.limits.cpu_millis = 8_000;
        context.limits.memory_bytes = 1 << 30;
        context.limits.turn_wall_seconds = 60;
        let spec =
            WorkspaceRunSpec::from_context("run".into(), &context, &defaults()).expect("spec");
        assert_eq!(spec.cpu_millis, 2_000);
        assert_eq!(spec.memory_bytes, 1 << 30);
        assert_eq!(spec.turn_wall, Duration::from_mins(1));
    }
}
