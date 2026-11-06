//! The cloud browser session's container, rendered for each backend
//! (ADR-0049 decision 1).
//!
//! A browser session is its own sandbox: one Pod (Kubernetes, gVisor
//! runtime class) or container (Docker) running one engine and the in-pod
//! session agent of #1594. It holds no workspace, no token and no platform
//! credential, and it is never the worker or the workspace runner. What it
//! gets, and only it:
//!
//! * the label `sandbox.elitea.ai/kind: browser-session`, which the chart's
//!   `elitea-browser-session-egress` `NetworkPolicy` selects to let the Pod
//!   reach the egress proxy's pods on the proxy port and nothing else (the
//!   execution namespace's deny-all covers the rest); no resolver
//!   (`dnsPolicy: None`, a loopback nameserver): the proxy resolves;
//! * on Docker, the profile's internal proxy network when the start carries
//!   an egress proxy, network `none` otherwise;
//! * a memory-backed `/tmp` (the engine's profile directory: cookies and
//!   storage die with the session) and `/dev/shm`, both counted in its
//!   memory;
//! * an idle entrypoint (`elitea-browser-session idle`): the supervisor
//!   execs `elitea-browser-session serve` and speaks the line protocol of
//!   `libs/proto/elitea/runtime/v1/browser_session_agent_v1.md` over its
//!   stdin and stdout. CDP stays on the Pod's loopback.
//!
//! The engine's own sandbox needs user namespaces. Under gVisor the
//! container is the boundary and the engine may run with `--no-sandbox`; on
//! any other runtime (runc on a shared kernel, and Kata, which this profile
//! does not treat as gVisor) the engine keeps its sandbox and a profile that
//! would turn it off is refused ([`engine_sandbox`]).

use std::collections::BTreeMap;

use serde_json::{Value, json};

use crate::protocol::elitea::runtime::v1::{BrowserEngineV1, BrowserSessionSpecV1};

pub use super::workspace_run::{
    ACTIVATION_BINDING, EXPIRES_BINDING, KIND_LABEL, NO_RESOLVER, REQUEST_BINDING, hex,
};

pub const KIND_BROWSER_SESSION: &str = "browser-session";
/// The request digest domain of a session's start, call and destroy grants.
pub const DIGEST_DOMAIN: &[u8] = b"elitea.sandbox.browser-session.v1\0";
/// The request digest domain of a view stream's grant.
pub const VIEW_DIGEST_DOMAIN: &[u8] = b"elitea.sandbox.browser-view.v1\0";
/// The session agent inside the session image (#1594).
pub const AGENT_PATH: &str = "/usr/local/bin/elitea-browser-session";
/// The Pod's single container.
pub const CONTAINER_NAME: &str = "browser";
/// The session's memory-backed scratch: the engine's profile directory.
pub const TMP_BYTES: u64 = 128 * 1024 * 1024;
/// The engine's shared memory.
pub const SHM_BYTES: u64 = 128 * 1024 * 1024;
const NAME_PREFIX: &str = "elitea-br-";
pub const MAX_PAGES: u32 = 16;
pub const MAX_WALL_SECONDS: u64 = 24 * 3600;
/// The least memory a session may have: the scratch and shared memory it
/// mounts, plus room for the engine.
pub const MIN_MEMORY_BYTES: u64 = TMP_BYTES + SHM_BYTES;

/// The engine a session runs (ADR-0049 decision 2).
#[derive(Clone, Copy, Debug, Eq, PartialEq, Ord, PartialOrd)]
pub enum Engine {
    /// chrome-headless-shell.
    Full,
    /// Moli, after the bake-off admits it.
    Lite,
}

impl Engine {
    /// The name the profile and the session agent use.
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Full => "full",
            Self::Lite => "lite",
        }
    }

    fn from_wire(value: i32) -> Option<Self> {
        match BrowserEngineV1::try_from(value).ok()? {
            BrowserEngineV1::Full => Some(Self::Full),
            BrowserEngineV1::Lite => Some(Self::Lite),
            BrowserEngineV1::Unspecified => None,
        }
    }

    fn to_wire(self) -> BrowserEngineV1 {
        match self {
            Self::Full => BrowserEngineV1::Full,
            Self::Lite => BrowserEngineV1::Lite,
        }
    }
}

/// What isolates a session's container from the node kernel.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Isolation {
    /// gVisor (`runsc`, or GKE Sandbox's `gvisor` handler).
    Gvisor,
    /// Any other runtime: runc or crun on a shared kernel, or Kata.
    Other,
}

impl Isolation {
    /// The isolation a container runtime or `RuntimeClass` handler gives.
    #[must_use]
    pub fn of_handler(handler: &str) -> Self {
        if matches!(handler, "runsc" | "gvisor") {
            Self::Gvisor
        } else {
            Self::Other
        }
    }
}

/// The engine would run without its own sandbox on a runtime that is not
/// gVisor.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
#[error("--no-sandbox is refused on a runtime that is not gVisor")]
pub struct IsolationRefused;

/// Whether the engine keeps its own sandbox: `engine_sandbox` is the
/// profile's setting, `isolation` the runtime's. `false` (the engine runs
/// with `--no-sandbox`) is admitted only under gVisor, where the container
/// is the boundary.
///
/// # Errors
///
/// [`IsolationRefused`] for `--no-sandbox` on any other runtime.
pub fn engine_sandbox(
    engine_sandbox: bool,
    isolation: Isolation,
) -> Result<bool, IsolationRefused> {
    if engine_sandbox || isolation == Isolation::Gvisor {
        Ok(engine_sandbox)
    } else {
        Err(IsolationRefused)
    }
}

/// The SHA-256 a session grant's `request_digest` must equal: the domain,
/// then every spec field in order, fixed-width big-endian, the name
/// length-prefixed.
#[must_use]
pub fn spec_digest(spec: &BrowserSessionSpecV1) -> [u8; 32] {
    use sha2::Digest as _;
    let mut hash = sha2::Sha256::new();
    hash.update(DIGEST_DOMAIN);
    hash.update(spec.revision.to_be_bytes());
    hash.update((spec.name.len() as u64).to_be_bytes());
    hash.update(spec.name.as_bytes());
    hash.update(spec.engine.to_be_bytes());
    hash.update(spec.cpu_millis.to_be_bytes());
    hash.update(spec.memory_bytes.to_be_bytes());
    hash.update(spec.wall_seconds.to_be_bytes());
    hash.update(spec.max_pages.to_be_bytes());
    hash.finalize().into()
}

/// The digest of a view stream's grant: the session's digest and whether
/// the stream may drive. A watch-only grant cannot open a driving stream.
#[must_use]
pub fn view_digest(spec: &BrowserSessionSpecV1, control: bool) -> [u8; 32] {
    use sha2::Digest as _;
    let mut hash = sha2::Sha256::new();
    hash.update(VIEW_DIGEST_DOMAIN);
    hash.update(spec_digest(spec));
    hash.update([u8::from(control)]);
    hash.finalize().into()
}

/// A session the policy or the deployment cannot render.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum InvalidBrowserSession {
    #[error("the browser session's container is invalid")]
    Invalid,
    /// An egress proxy and the deployment names no proxy network.
    #[error("the deployment has no browser proxy network")]
    NoProxyNetwork,
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

/// Whether `name` is a browser-session name, `elitea-br-<24 hex>`.
#[must_use]
pub fn session_name(name: &str) -> bool {
    name.strip_prefix(NAME_PREFIX).is_some_and(|suffix| {
        suffix.len() == 24
            && suffix
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    })
}

/// One browser session's container, as the deployment and the policy decide.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BrowserSessionContainer {
    /// A DNS label, `elitea-br-<24 hex>`.
    pub name: String,
    /// The session image, pinned by digest.
    pub image: String,
    pub engine: Engine,
    pub cpu_millis: u64,
    pub memory_bytes: u64,
    pub wall_seconds: u64,
    pub max_pages: u32,
    /// The start carries an egress proxy (Docker: join the proxy network).
    pub egress: bool,
}

impl BrowserSessionContainer {
    /// The wire spec of this session (the image is the supervisor's).
    #[must_use]
    pub fn to_wire(&self) -> BrowserSessionSpecV1 {
        BrowserSessionSpecV1 {
            revision: 1,
            name: self.name.clone(),
            engine: self.engine.to_wire().into(),
            cpu_millis: self.cpu_millis,
            memory_bytes: self.memory_bytes,
            wall_seconds: self.wall_seconds,
            max_pages: self.max_pages,
        }
    }

    /// The session a supervisor renders from a wire spec in its own `image`.
    ///
    /// # Errors
    ///
    /// Another revision, a name that is not a session name, an unknown
    /// engine, or as [`Self::validate`].
    pub fn from_wire(
        spec: &BrowserSessionSpecV1,
        image: &str,
        egress: bool,
    ) -> Result<Self, InvalidBrowserSession> {
        let engine = Engine::from_wire(spec.engine).ok_or(InvalidBrowserSession::Invalid)?;
        if spec.revision != 1 || !session_name(&spec.name) {
            return Err(InvalidBrowserSession::Invalid);
        }
        let container = Self {
            name: spec.name.clone(),
            image: image.to_owned(),
            engine,
            cpu_millis: spec.cpu_millis,
            memory_bytes: spec.memory_bytes,
            wall_seconds: spec.wall_seconds,
            max_pages: spec.max_pages,
            egress,
        };
        container.validate()?;
        Ok(container)
    }

    /// # Errors
    ///
    /// A mutable image, a bad name, or an unbounded resource.
    pub fn validate(&self) -> Result<(), InvalidBrowserSession> {
        if !pinned(&self.image)
            || !dns_label(&self.name)
            || self.memory_bytes < MIN_MEMORY_BYTES
            || self.cpu_millis == 0
            || self.cpu_millis > 64_000
            || self.wall_seconds == 0
            || self.wall_seconds > MAX_WALL_SECONDS
            || self.max_pages == 0
            || self.max_pages > MAX_PAGES
        {
            return Err(InvalidBrowserSession::Invalid);
        }
        Ok(())
    }

    fn labels() -> BTreeMap<&'static str, &'static str> {
        BTreeMap::from([
            ("app.kubernetes.io/component", "sandbox-execution"),
            (KIND_LABEL, KIND_BROWSER_SESSION),
        ])
    }

    /// The Pod in the execution namespace. It idles; the supervisor execs the
    /// session agent. No credential, token or proxy address is in its spec.
    /// `active_deadline_seconds` is the backstop that ends the Pod even when
    /// no supervisor is left to destroy it.
    ///
    /// # Errors
    ///
    /// As [`Self::validate`], or a bad namespace, runtime class or placement.
    pub fn kubernetes_pod(
        &self,
        namespace: &str,
        runtime_class: &str,
        node_selector: &BTreeMap<String, String>,
        active_deadline_seconds: u64,
    ) -> Result<Value, InvalidBrowserSession> {
        self.validate()?;
        if !dns_label(namespace)
            || !dns_label(runtime_class)
            || node_selector.is_empty()
            || node_selector.len() > 16
        {
            return Err(InvalidBrowserSession::Invalid);
        }
        let resources = json!({
            "cpu": format!("{}m", self.cpu_millis),
            "memory": self.memory_bytes.to_string(),
            "ephemeral-storage": "64Mi",
        });
        Ok(json!({
            "apiVersion": "v1", "kind": "Pod",
            "metadata": {
                "name": self.name, "namespace": namespace,
                "labels": Self::labels(),
                "annotations": {
                    "sandbox.elitea.ai/browser-engine": self.engine.as_str(),
                    "sandbox.elitea.ai/browser-max-pages": self.max_pages.to_string(),
                },
            },
            "spec": {
                "restartPolicy": "Never", "automountServiceAccountToken": false,
                "serviceAccountName": "elitea-code",
                "enableServiceLinks": false, "hostNetwork": false, "hostPID": false, "hostIPC": false,
                // No resolver: the engine's only route is the egress proxy,
                // which resolves for it.
                "dnsPolicy": "None",
                "dnsConfig": {"nameservers": [NO_RESOLVER]},
                "runtimeClassName": runtime_class, "nodeSelector": node_selector,
                "terminationGracePeriodSeconds": 5,
                "activeDeadlineSeconds": active_deadline_seconds,
                "securityContext": {"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001,
                    "fsGroup": 10001, "seccompProfile": {"type": "RuntimeDefault"}},
                "containers": [{
                    "name": CONTAINER_NAME, "image": self.image, "imagePullPolicy": "IfNotPresent",
                    "command": [AGENT_PATH, "idle"],
                    "workingDir": "/tmp",
                    "env": [{"name": "HOME", "value": "/tmp"}],
                    "resources": {"requests": resources, "limits": resources},
                    "securityContext": {"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
                        "capabilities": {"drop": ["ALL"]}},
                    "volumeMounts": [{"name": "tmp", "mountPath": "/tmp"},
                        {"name": "shm", "mountPath": "/dev/shm"}]
                }],
                "volumes": [
                    {"name": "tmp", "emptyDir": {"medium": "Memory", "sizeLimit": TMP_BYTES.to_string()}},
                    {"name": "shm", "emptyDir": {"medium": "Memory", "sizeLimit": SHM_BYTES.to_string()}}
                ]
            }
        }))
    }

    /// The Docker container: the profile's internal proxy network when the
    /// start carries an egress proxy, network `none` otherwise; `runtime`
    /// is the profile's (`runsc` for gVisor).
    ///
    /// # Errors
    ///
    /// As [`Self::validate`]; [`InvalidBrowserSession::NoProxyNetwork`] when
    /// the start has an egress proxy and the profile no proxy network.
    pub fn docker_plan(
        &self,
        proxy_network: Option<&str>,
        runtime: Option<&str>,
    ) -> Result<DockerBrowserPlan, InvalidBrowserSession> {
        self.validate()?;
        let usable = |name: &&str| {
            !name.is_empty()
                && !matches!(*name, "host" | "bridge" | "none" | "default")
                && name
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'-'))
        };
        let network_mode = if self.egress {
            proxy_network
                .filter(usable)
                .ok_or(InvalidBrowserSession::NoProxyNetwork)?
                .to_owned()
        } else {
            "none".to_owned()
        };
        Ok(DockerBrowserPlan {
            name: self.name.clone(),
            image: self.image.clone(),
            entrypoint: vec![AGENT_PATH.to_owned(), "idle".to_owned()],
            user: "10001:10001".to_owned(),
            readonly_rootfs: true,
            cap_drop: vec!["ALL".to_owned()],
            security_opt: vec!["no-new-privileges:true".to_owned()],
            pids_limit: 1024,
            memory_bytes: self.memory_bytes,
            nano_cpus: i64::try_from(self.cpu_millis.saturating_mul(1_000_000)).unwrap_or(i64::MAX),
            dns: if network_mode == "none" {
                Vec::new()
            } else {
                vec![NO_RESOLVER.to_owned()]
            },
            network_mode,
            runtime: runtime.map(str::to_owned),
            shm_bytes: SHM_BYTES,
            tmpfs: BTreeMap::from([(
                "/tmp".to_owned(),
                format!("rw,nosuid,nodev,size={TMP_BYTES},uid=10001,gid=10001,mode=0700"),
            )]),
            labels: Self::labels()
                .into_iter()
                .map(|(key, value)| (key.to_owned(), value.to_owned()))
                .collect(),
        })
    }
}

/// A Docker container, as the supervisor creates it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DockerBrowserPlan {
    pub name: String,
    pub image: String,
    pub entrypoint: Vec<String>,
    pub user: String,
    pub readonly_rootfs: bool,
    pub cap_drop: Vec<String>,
    pub security_opt: Vec<String>,
    pub pids_limit: i64,
    pub memory_bytes: u64,
    pub nano_cpus: i64,
    pub network_mode: String,
    pub dns: Vec<String>,
    /// The container runtime (`runsc` for gVisor); `None` is the daemon's.
    pub runtime: Option<String>,
    pub shm_bytes: u64,
    pub tmpfs: BTreeMap<String, String>,
    pub labels: BTreeMap<String, String>,
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// Main computes the view grant's digest from its session row
    /// (internal/domain/browsersession, #1597). These vectors are pinned on
    /// both sides: a drift refuses every view.
    #[test]
    fn the_digests_match_mains_vectors() {
        let name = crate::browser_run::session::session_name("0123456789abcdef0123456789abcdef");
        assert_eq!(name, "elitea-br-86b40f0fcfcf8dcc546fcf51");
        let spec = BrowserSessionSpecV1 {
            revision: 1,
            name,
            engine: BrowserEngineV1::Full as i32,
            cpu_millis: 1000,
            memory_bytes: 1 << 30,
            wall_seconds: 600,
            max_pages: 3,
        };
        let hex = |bytes: [u8; 32]| crate::sandbox::workspace_run::hex(&bytes);
        assert_eq!(
            hex(spec_digest(&spec)),
            "6a3b67da63b9c3426d70c3de912d6697a7537b4bbd5eb2bbe9375ef1bb4f667e"
        );
        assert_eq!(
            hex(view_digest(&spec, false)),
            "a8380436929e55a5975f12fd62f73d709ae5ccd2e39acbd5456ac0ea16a9b93f"
        );
        assert_eq!(
            hex(view_digest(&spec, true)),
            "a86dce642c3645438769b87f6d7e9d5074d667b9933649151d45437c4a2584ff"
        );
    }

    pub(crate) fn session(engine: Engine) -> BrowserSessionContainer {
        BrowserSessionContainer {
            name: "elitea-br-0123456789abcdef01234567".into(),
            image: format!("registry.example/browser@sha256:{}", "e".repeat(64)),
            engine,
            cpu_millis: 2_000,
            memory_bytes: 2 << 30,
            wall_seconds: 3_600,
            max_pages: 3,
            egress: true,
        }
    }

    fn selector() -> BTreeMap<String, String> {
        BTreeMap::from([("elitea.ai/browser".into(), "true".into())])
    }

    /// The `--no-sandbox` rule: only gVisor admits it; the engine's own
    /// sandbox is admitted everywhere.
    #[test]
    fn no_sandbox_is_admitted_only_under_gvisor() {
        assert_eq!(engine_sandbox(false, Isolation::Gvisor), Ok(false));
        assert_eq!(
            engine_sandbox(false, Isolation::Other),
            Err(IsolationRefused)
        );
        assert_eq!(engine_sandbox(true, Isolation::Other), Ok(true));
        assert_eq!(engine_sandbox(true, Isolation::Gvisor), Ok(true));
        assert_eq!(Isolation::of_handler("runsc"), Isolation::Gvisor);
        assert_eq!(Isolation::of_handler("gvisor"), Isolation::Gvisor);
        for other in ["runc", "crun", "kata", "kata-vm-isolation", ""] {
            assert_eq!(Isolation::of_handler(other), Isolation::Other, "{other}");
        }
    }

    #[test]
    fn the_pod_has_no_credential_no_resolver_and_a_memory_scratch() {
        let pod = session(Engine::Full)
            .kubernetes_pod("elitea-execution", "gvisor", &selector(), 4_500)
            .expect("pod");
        assert_eq!(pod["metadata"]["labels"][KIND_LABEL], KIND_BROWSER_SESSION);
        assert_eq!(pod["spec"]["runtimeClassName"], "gvisor");
        assert_eq!(pod["spec"]["automountServiceAccountToken"], false);
        assert_eq!(pod["spec"]["dnsPolicy"], "None");
        assert_eq!(pod["spec"]["dnsConfig"]["nameservers"][0], NO_RESOLVER);
        assert_eq!(pod["spec"]["activeDeadlineSeconds"], 4_500);
        let container = &pod["spec"]["containers"][0];
        assert_eq!(container["command"], json!([AGENT_PATH, "idle"]));
        assert_eq!(
            container["resources"]["limits"]["memory"],
            (2_u64 << 30).to_string()
        );
        assert_eq!(container["resources"]["limits"]["cpu"], "2000m");
        assert_eq!(container["securityContext"]["readOnlyRootFilesystem"], true);
        for volume in pod["spec"]["volumes"].as_array().expect("volumes") {
            assert_eq!(volume["emptyDir"]["medium"], "Memory", "{volume}");
        }
        let text = pod.to_string();
        for forbidden in ["token", "secret", "proxy", "Authorization", "workspace"] {
            assert!(!text.contains(forbidden), "{forbidden}");
        }
    }

    #[test]
    fn docker_joins_the_proxy_network_only_with_egress() {
        let plan = session(Engine::Full)
            .docker_plan(Some("elitea-browser-proxy"), Some("runsc"))
            .expect("plan");
        assert_eq!(plan.network_mode, "elitea-browser-proxy");
        assert_eq!(plan.dns, [NO_RESOLVER]);
        assert_eq!(plan.runtime.as_deref(), Some("runsc"));
        assert_eq!(plan.cap_drop, ["ALL"]);
        assert!(plan.readonly_rootfs);
        let mut offline = session(Engine::Full);
        offline.egress = false;
        let plan = offline.docker_plan(None, None).expect("plan");
        assert_eq!(plan.network_mode, "none");
        assert_eq!(plan.dns.len(), 0);
        for bad in [None, Some("bridge"), Some("host"), Some("")] {
            assert_eq!(
                session(Engine::Full).docker_plan(bad, None),
                Err(InvalidBrowserSession::NoProxyNetwork),
                "{bad:?}"
            );
        }
    }

    #[test]
    fn the_wire_spec_round_trips_and_the_digest_binds_every_field() {
        let image = session(Engine::Lite).image;
        let wire = session(Engine::Lite).to_wire();
        let back = BrowserSessionContainer::from_wire(&wire, &image, true).expect("round trip");
        assert_eq!(back, session(Engine::Lite));
        let base = spec_digest(&wire);
        let mut changed = wire.clone();
        changed.max_pages = 2;
        assert_ne!(spec_digest(&changed), base);
        let mut changed = wire.clone();
        changed.engine = BrowserEngineV1::Full.into();
        assert_ne!(spec_digest(&changed), base);
        assert_ne!(view_digest(&wire, true), view_digest(&wire, false));
        assert_ne!(view_digest(&wire, false), base);
    }

    #[test]
    fn a_bad_name_engine_image_or_resource_is_refused() {
        let image = session(Engine::Full).image;
        let mut wire = session(Engine::Full).to_wire();
        wire.name = "elitea-ws-0123456789abcdef01234567".into();
        assert!(BrowserSessionContainer::from_wire(&wire, &image, false).is_err());
        let mut wire = session(Engine::Full).to_wire();
        wire.engine = 0;
        assert!(BrowserSessionContainer::from_wire(&wire, &image, false).is_err());
        let wire = session(Engine::Full).to_wire();
        assert!(
            BrowserSessionContainer::from_wire(&wire, "registry.example/browser:latest", false)
                .is_err()
        );
        for mutate in [
            |s: &mut BrowserSessionContainer| s.max_pages = 0,
            |s: &mut BrowserSessionContainer| s.memory_bytes = 128 << 20,
            |s: &mut BrowserSessionContainer| s.wall_seconds = 0,
            |s: &mut BrowserSessionContainer| s.cpu_millis = 0,
        ] {
            let mut bad = session(Engine::Full);
            mutate(&mut bad);
            assert_eq!(bad.validate(), Err(InvalidBrowserSession::Invalid));
        }
    }
}
