//! The cloud executor's workspace-run container, rendered for each backend
//! (ADR-0034 decision 9).
//!
//! A workspace run is NOT a toolkit Code job, and nothing here changes one:
//! a Code job keeps its memory-backed `noexec` `/workspace`, its pids limit
//! of 128, network `none` and read-only workspace mode
//! (`kubernetes::PodPolicy::workload`, the vendored adk-sandbox
//! `build_host_config` with `code_job_policy`). The tests at the bottom hold
//! both side by side.
//!
//! What a workspace run gets instead, and only it:
//!
//! * a **disk-backed** workspace (a Kubernetes `emptyDir` without
//!   `medium: Memory`, a Docker named volume), mounted with exec and sized
//!   from the declared unpacked tree plus build headroom;
//! * a raised pids limit for builds;
//! * its own image (`libs/rust/workspace-runner/Containerfile`), which idles
//!   until the supervisor execs `elitea-workspace-runner unpack|serve|pack`;
//! * the label `sandbox.elitea.ai/kind: workspace-run`, and
//!   `sandbox.elitea.ai/egress: public` only when the project's network is
//!   on. The chart's label-selected NetworkPolicy opens egress for exactly
//!   those pods (minus private, link-local, loopback, metadata and cluster
//!   ranges, DNS to the cluster resolver only); every other pod in the
//!   execution namespace stays under the deny-all policy. Network off is
//!   no egress label (Kubernetes) and network mode `none` (Docker).

use std::collections::BTreeMap;

use serde_json::{Value, json};

pub const KIND_LABEL: &str = "sandbox.elitea.ai/kind";
pub const KIND_WORKSPACE_RUN: &str = "workspace-run";
pub const EGRESS_LABEL: &str = "sandbox.elitea.ai/egress";
pub const EGRESS_PUBLIC: &str = "public";
/// The pod's single container.
pub const CONTAINER_NAME: &str = "workspace";
/// The runner binary inside the workspace image.
pub const RUNNER_PATH: &str = "/usr/local/bin/elitea-workspace-runner";
pub const WORKSPACE_MOUNT: &str = "/workspace";
const TMP_BYTES: u64 = 512 * 1024 * 1024;
const MIN_WORKSPACE_BYTES: u64 = 256 * 1024 * 1024;
const MAX_WORKSPACE_BYTES: u64 = 512 * 1024 * 1024 * 1024;
const MAX_PIDS: i64 = 32_768;

/// One workspace run's container, as the deployment and the policy decide it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkspaceRunContainer {
    /// A DNS label, stable for the claim.
    pub name: String,
    /// The workspace image, pinned by digest.
    pub image: String,
    pub workspace_bytes: u64,
    /// The project's network switch.
    pub egress: bool,
    pub cpu_millis: u64,
    pub memory_bytes: u64,
    pub pids_limit: i64,
    pub wall_seconds: u64,
}

/// A workspace run the policy or the deployment cannot render.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum InvalidWorkspaceRun {
    #[error("the workspace run's container is invalid")]
    Invalid,
    /// Egress is on and the deployment names no egress network.
    #[error("the deployment has no workspace egress network")]
    NoEgressNetwork,
}

fn pinned(image: &str) -> bool {
    image.rsplit_once("@sha256:").is_some_and(|(name, digest)| {
        !name.is_empty()
            && !name.chars().any(char::is_whitespace)
            && digest.len() == 64
            && digest
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    })
}

fn dns_label(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 63
        && value.as_bytes()[0].is_ascii_lowercase()
        && value.as_bytes()[value.len() - 1].is_ascii_alphanumeric()
        && value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
}

impl WorkspaceRunContainer {
    /// # Errors
    ///
    /// A mutable image, a bad name, or an unbounded resource.
    pub fn validate(&self) -> Result<(), InvalidWorkspaceRun> {
        if !pinned(&self.image)
            || !dns_label(&self.name)
            || !(MIN_WORKSPACE_BYTES..=MAX_WORKSPACE_BYTES).contains(&self.workspace_bytes)
            || self.memory_bytes < 64 * 1024 * 1024
            || self.cpu_millis == 0
            || self.cpu_millis > 256_000
            || self.pids_limit <= 128
            || self.pids_limit > MAX_PIDS
            || self.wall_seconds == 0
            || self.wall_seconds > 24 * 3600
        {
            return Err(InvalidWorkspaceRun::Invalid);
        }
        Ok(())
    }

    fn labels(&self) -> BTreeMap<&'static str, &'static str> {
        let mut labels = BTreeMap::from([
            ("app.kubernetes.io/component", "sandbox-execution"),
            (KIND_LABEL, KIND_WORKSPACE_RUN),
        ]);
        if self.egress {
            labels.insert(EGRESS_LABEL, EGRESS_PUBLIC);
        }
        labels
    }

    /// The Pod in the execution namespace. It idles; the supervisor execs the
    /// runner. No credential, token or store route is in its spec.
    ///
    /// # Errors
    ///
    /// As [`Self::validate`], or a bad namespace, runtime class or placement.
    pub fn kubernetes_pod(
        &self,
        namespace: &str,
        runtime_class: &str,
        node_selector: &BTreeMap<String, String>,
    ) -> Result<Value, InvalidWorkspaceRun> {
        self.validate()?;
        if !dns_label(namespace)
            || !dns_label(runtime_class)
            || node_selector.is_empty()
            || node_selector.len() > 16
        {
            return Err(InvalidWorkspaceRun::Invalid);
        }
        let mut pod = self.pod_body(namespace, runtime_class, node_selector);
        if !self.egress {
            pod["spec"]["dnsConfig"] = json!({"nameservers": ["127.0.0.1"]});
        }
        Ok(pod)
    }

    fn pod_body(
        &self,
        namespace: &str,
        runtime_class: &str,
        node_selector: &BTreeMap<String, String>,
    ) -> Value {
        let resources = json!({
            "cpu": format!("{}m", self.cpu_millis),
            "memory": self.memory_bytes.to_string(),
            "ephemeral-storage": (self.workspace_bytes + TMP_BYTES).to_string(),
        });
        json!({
            "apiVersion": "v1", "kind": "Pod",
            "metadata": {
                "name": self.name, "namespace": namespace,
                "labels": self.labels(),
                "annotations": {"sandbox.elitea.ai/pids-limit": self.pids_limit.to_string()},
            },
            "spec": {
                "restartPolicy": "Never", "automountServiceAccountToken": false,
                "serviceAccountName": "elitea-code",
                "enableServiceLinks": false, "hostNetwork": false, "hostPID": false, "hostIPC": false,
                // With egress on, names resolve through the cluster resolver
                // only (the NetworkPolicy allows DNS to it and nothing else
                // private). With it off, no resolver is configured at all.
                "dnsPolicy": if self.egress { "ClusterFirst" } else { "None" },
                "runtimeClassName": runtime_class, "nodeSelector": node_selector,
                "terminationGracePeriodSeconds": 5,
                "activeDeadlineSeconds": self.wall_seconds + 60,
                "securityContext": {"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001,
                    "fsGroup": 10001, "seccompProfile": {"type": "RuntimeDefault"}},
                "containers": [{
                    "name": CONTAINER_NAME, "image": self.image, "imagePullPolicy": "IfNotPresent",
                    "command": ["sleep", "infinity"],
                    "workingDir": WORKSPACE_MOUNT,
                    "resources": {"requests": resources, "limits": resources},
                    "securityContext": {"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
                        "capabilities": {"drop": ["ALL"]}},
                    "volumeMounts": [{"name": "workspace", "mountPath": WORKSPACE_MOUNT},
                        {"name": "tmp", "mountPath": "/tmp"}]
                }],
                "volumes": [
                    // Disk-backed: no `medium: Memory`.
                    {"name": "workspace", "emptyDir": {"sizeLimit": self.workspace_bytes.to_string()}},
                    {"name": "tmp", "emptyDir": {"medium": "Memory", "sizeLimit": TMP_BYTES.to_string()}}
                ]
            }
        })
    }

    /// The Docker container: a named volume for the workspace, the dedicated
    /// egress network when the project's network is on, network `none`
    /// otherwise.
    ///
    /// # Errors
    ///
    /// As [`Self::validate`]; [`InvalidWorkspaceRun::NoEgressNetwork`] when
    /// egress is on and the deployment names no egress network.
    pub fn docker_plan(
        &self,
        egress_network: Option<&str>,
        egress_dns: &[String],
    ) -> Result<DockerRunPlan, InvalidWorkspaceRun> {
        self.validate()?;
        let network_mode = if self.egress {
            let network = egress_network
                .filter(|name| {
                    !name.is_empty()
                        && !matches!(*name, "host" | "bridge" | "none" | "default")
                        && name
                            .bytes()
                            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'-'))
                })
                .ok_or(InvalidWorkspaceRun::NoEgressNetwork)?;
            network.to_owned()
        } else {
            "none".to_owned()
        };
        if self.egress
            && (egress_dns.is_empty()
                || egress_dns
                    .iter()
                    .any(|server| server.parse::<std::net::IpAddr>().is_err()))
        {
            return Err(InvalidWorkspaceRun::NoEgressNetwork);
        }
        Ok(DockerRunPlan {
            dns: if self.egress { egress_dns.to_vec() } else { Vec::new() },
            name: self.name.clone(),
            image: self.image.clone(),
            command: vec!["sleep".to_owned(), "infinity".to_owned()],
            user: "10001:10001".to_owned(),
            readonly_rootfs: true,
            cap_drop: vec!["ALL".to_owned()],
            security_opt: vec!["no-new-privileges:true".to_owned()],
            pids_limit: self.pids_limit,
            memory_bytes: self.memory_bytes,
            nano_cpus: i64::try_from(self.cpu_millis.saturating_mul(1_000_000)).unwrap_or(i64::MAX),
            network_mode,
            workspace_volume: format!("{}-workspace", self.name),
            workspace_bytes: self.workspace_bytes,
            tmpfs: BTreeMap::from([(
                "/tmp".to_owned(),
                format!("rw,nosuid,nodev,noexec,size={TMP_BYTES},uid=10001,gid=10001,mode=0700"),
            )]),
            labels: self
                .labels()
                .into_iter()
                .map(|(key, value)| (key.to_owned(), value.to_owned()))
                .collect(),
        })
    }
}

/// A Docker container, as the supervisor creates it (the fields of bollard's
/// `HostConfig` and `Config` it sets).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DockerRunPlan {
    pub name: String,
    pub image: String,
    pub command: Vec<String>,
    pub user: String,
    pub readonly_rootfs: bool,
    pub cap_drop: Vec<String>,
    pub security_opt: Vec<String>,
    pub pids_limit: i64,
    pub memory_bytes: u64,
    pub nano_cpus: i64,
    pub network_mode: String,
    /// The resolvers the egress network's rules admit (`deploy/docker/workspace-egress.sh`).
    pub dns: Vec<String>,
    /// A named, disk-backed volume mounted at `/workspace` (exec). Docker's
    /// local driver cannot cap its size; the runner's archive limits and the
    /// declared size bound what is written into it.
    pub workspace_volume: String,
    pub workspace_bytes: u64,
    pub tmpfs: BTreeMap<String, String>,
    pub labels: BTreeMap<String, String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(egress: bool) -> WorkspaceRunContainer {
        WorkspaceRunContainer {
            name: "elitea-ws-0123456789abcdef01234567".into(),
            image: format!("registry.example/workspace-runner@sha256:{}", "b".repeat(64)),
            workspace_bytes: 6 << 30,
            egress,
            cpu_millis: 2_000,
            memory_bytes: 4 << 30,
            pids_limit: 4_096,
            wall_seconds: 1_800,
        }
    }

    fn dns() -> Vec<String> {
        vec!["1.1.1.1".to_owned()]
    }

    fn selector() -> BTreeMap<String, String> {
        BTreeMap::from([("sandbox".into(), "true".into())])
    }

    #[test]
    fn a_workspace_run_pod_is_disk_backed_writable_labelled_and_without_credentials() {
        let pod = run(true)
            .kubernetes_pod("elitea-execution", "gvisor", &selector())
            .expect("pod");
        let workspace = &pod["spec"]["volumes"][0];
        assert_eq!(workspace["name"], "workspace");
        assert!(workspace["emptyDir"].get("medium").is_none(), "disk, not tmpfs");
        assert_eq!(workspace["emptyDir"]["sizeLimit"], (6_u64 << 30).to_string());
        let mount = &pod["spec"]["containers"][0]["volumeMounts"][0];
        assert_eq!(mount["mountPath"], "/workspace");
        assert!(mount.get("readOnly").is_none());
        assert_eq!(pod["metadata"]["labels"][KIND_LABEL], KIND_WORKSPACE_RUN);
        assert_eq!(pod["metadata"]["labels"][EGRESS_LABEL], EGRESS_PUBLIC);
        assert_eq!(pod["spec"]["automountServiceAccountToken"], false);
        assert_eq!(pod["spec"]["dnsPolicy"], "ClusterFirst");
        let text = pod.to_string();
        for forbidden in ["token", "secret", "presign", "X-Amz", "Authorization"] {
            assert!(!text.contains(forbidden), "{forbidden}");
        }
    }

    #[test]
    fn network_off_carries_no_egress_label_and_no_resolver() {
        let pod = run(false)
            .kubernetes_pod("elitea-execution", "gvisor", &selector())
            .expect("pod");
        assert!(pod["metadata"]["labels"].get(EGRESS_LABEL).is_none());
        assert_eq!(pod["spec"]["dnsPolicy"], "None");
        let docker = run(false).docker_plan(Some("elitea-workspace-egress"), &dns()).expect("plan");
        assert_eq!(docker.network_mode, "none");
    }

    #[test]
    fn docker_egress_uses_the_dedicated_network_or_refuses() {
        let plan = run(true).docker_plan(Some("elitea-workspace-egress"), &dns()).expect("plan");
        assert_eq!(plan.network_mode, "elitea-workspace-egress");
        assert_eq!(plan.pids_limit, 4_096);
        assert!(plan.readonly_rootfs);
        assert_eq!(plan.cap_drop, ["ALL"]);
        assert!(plan.tmpfs["/tmp"].contains("noexec"));
        assert!(!plan.tmpfs.contains_key("/workspace"), "the workspace is a volume, with exec");
        assert_eq!(plan.dns, dns());
        assert_eq!(
            run(true).docker_plan(Some("elitea-workspace-egress"), &[]),
            Err(InvalidWorkspaceRun::NoEgressNetwork)
        );
        for bad in [None, Some("host"), Some("bridge"), Some("")] {
            assert_eq!(
                run(true).docker_plan(bad, &dns()),
                Err(InvalidWorkspaceRun::NoEgressNetwork),
                "{bad:?}"
            );
        }
    }

    #[test]
    fn a_mutable_image_or_a_code_job_pids_limit_is_refused() {
        let mut mutable = run(true);
        mutable.image = "registry.example/workspace-runner:latest".into();
        assert!(mutable.validate().is_err());
        let mut small = run(true);
        small.pids_limit = 128;
        assert!(small.validate().is_err());
    }

    /// The toolkit Code job is unchanged: memory-backed, and no workspace
    /// label the egress policy could select.
    #[cfg(feature = "sandbox-supervisor")]
    #[test]
    fn toolkit_code_jobs_keep_their_policy() {
        use crate::sandbox::kubernetes::{PodIdentity, PodPolicy};
        let policy = PodPolicy {
            namespace: "elitea-execution".into(),
            image: format!("registry.example/runtime@sha256:{}", "a".repeat(64)),
            runtime_class: "gvisor".into(),
            node_selector: selector(),
            memory_bytes: 512 * 1024 * 1024,
            workspace_bytes: 256 * 1024 * 1024,
            cpu_millis: 1000,
            timeout_seconds: 120,
        };
        let pod = policy
            .workload(&PodIdentity::new(&[1; 32], &[2; 32]))
            .expect("code pod");
        assert_eq!(pod["spec"]["volumes"][0]["emptyDir"]["medium"], "Memory");
        assert!(pod["metadata"]["labels"].get(KIND_LABEL).is_none());
        assert!(pod["metadata"]["labels"].get(EGRESS_LABEL).is_none());
        // The Docker side of a Code job (pids 128, network none, a noexec
        // tmpfs /workspace) is asserted where its policy lives, in the
        // vendored adk-sandbox's `docker.rs` tests, which this change does not
        // touch.
    }
}
