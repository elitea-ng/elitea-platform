//! Kubernetes workload contract. The API adapter must preserve this identity
//! and must not treat Pod disappearance as proof that execution stopped.
pub mod client;
#[cfg(test)]
mod live_tests;
pub mod runtime;

use serde_json::{Value, json};
use std::collections::BTreeMap;

const FINALIZER: &str = "sandbox.elitea.ai/receipt";

/// Deployment policy, never populated from model-generated Code arguments.
pub struct PodPolicy {
    pub namespace: String,
    pub image: String,
    pub runtime_class: String,
    pub node_selector: BTreeMap<String, String>,
    pub memory_bytes: u64,
    pub cpu_millis: u32,
    pub timeout_seconds: u32,
}

#[derive(Debug, thiserror::Error)]
#[error("Kubernetes sandbox policy or workload identity is invalid")]
pub struct InvalidWorkload;

/// Keep full fingerprints in annotations. Label values cannot hold 64 hex digits.
pub struct PodIdentity {
    name: String,
    job: String,
    request: String,
}

impl PodIdentity {
    #[must_use]
    pub fn new(job: &[u8; 32], request: &[u8; 32]) -> Self {
        let job = hex(job);
        // The full fingerprint below detects the unlikely truncated-name collision.
        let name = format!("elitea-code-{}", &job[..48]);
        Self {
            name,
            job,
            request: hex(request),
        }
    }
}

fn hex(bytes: &[u8; 32]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    bytes
        .iter()
        .flat_map(|b| {
            [
                char::from(DIGITS[usize::from(b >> 4)]),
                char::from(DIGITS[usize::from(b & 15)]),
            ]
        })
        .collect()
}

fn dns_label(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 63
        && value.as_bytes()[0].is_ascii_lowercase()
        && value.as_bytes()[value.len() - 1].is_ascii_alphanumeric()
        && value
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

impl PodPolicy {
    /// Build an inert Pod. Code and input travel through authenticated exec stdin,
    /// never Pod metadata, environment variables, command arguments, or `ConfigMaps`.
    /// # Errors
    /// Rejects mutable images, missing isolation placement, and unbounded resources.
    pub fn workload(&self, identity: &PodIdentity) -> Result<Value, InvalidWorkload> {
        let pinned = self
            .image
            .rsplit_once("@sha256:")
            .is_some_and(|(name, digest)| {
                !name.is_empty()
                    && !name.chars().any(char::is_whitespace)
                    && digest.len() == 64
                    && digest
                        .bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            });
        if !pinned
            || !dns_label(&self.namespace)
            || !dns_label(&self.runtime_class)
            || self.node_selector.is_empty()
            || self.node_selector.len() > 16
            || self
                .node_selector
                .iter()
                .any(|(k, v)| k.is_empty() || k.len() > 253 || v.is_empty() || v.len() > 63)
            || self.memory_bytes < 64 * 1024 * 1024
            || self.memory_bytes > 64 * 1024 * 1024 * 1024
            || self.cpu_millis == 0
            || self.cpu_millis > 256_000
            || self.timeout_seconds == 0
            || self.timeout_seconds > 3600
        {
            return Err(InvalidWorkload);
        }
        let resources = json!({"cpu": format!("{}m", self.cpu_millis), "memory": self.memory_bytes.to_string()});
        Ok(json!({
            "apiVersion":"v1", "kind":"Pod",
            "metadata": {
                "name":identity.name, "namespace":self.namespace,
                "labels":{"app.kubernetes.io/component":"sandbox-execution"},
                "annotations":{"sandbox.elitea.ai/job":identity.job,"sandbox.elitea.ai/request":identity.request},
                "finalizers":[FINALIZER]
            },
            "spec": {
                "restartPolicy":"Never", "automountServiceAccountToken":false,
                "serviceAccountName":"elitea-code",
                "enableServiceLinks":false, "hostNetwork":false, "hostPID":false, "hostIPC":false,
                "runtimeClassName":self.runtime_class, "nodeSelector":self.node_selector,
                "terminationGracePeriodSeconds":5,
                "activeDeadlineSeconds":self.timeout_seconds + 60,
                "securityContext":{"runAsNonRoot":true,"runAsUser":10001,"runAsGroup":10001,
                    "fsGroup":10001,"seccompProfile":{"type":"RuntimeDefault"}},
                "containers":[{
                    "name":"code", "image":self.image, "imagePullPolicy":"Never",
                    "command":["/bin/sh","-c","while [ ! -f /workspace/.elitea-dispatch ]; do sleep 0.1; done; exec /usr/local/bin/elitea-code-runner"],
                    "resources":{"requests":resources,"limits":resources},
                    "env":[
                        {"name":"ELITEA_SANDBOX_POD_UID","valueFrom":{"fieldRef":{"fieldPath":"metadata.uid"}}},
                        {"name":"ELITEA_SANDBOX_REQUEST","valueFrom":{"fieldRef":{"fieldPath":"metadata.annotations['sandbox.elitea.ai/request']"}}}
                    ],
                    "securityContext":{"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}},
                    "volumeMounts":[{"name":"workspace","mountPath":"/workspace"},{"name":"tmp","mountPath":"/tmp"}]
                }],
                "volumes":[{"name":"workspace","emptyDir":{"medium":"Memory","sizeLimit":"256Mi"}},
                    {"name":"tmp","emptyDir":{"medium":"Memory","sizeLimit":"64Mi"}}]
            }
        }))
    }
}

/// Retain the original UID across observation and deletion. A same-name Pod is
/// not an interchangeable execution, even when a controller recreates it.
/// # Errors
/// Rejects a missing or mismatched runtime identity.
pub fn confirmed_terminated(
    pod: &Value,
    identity: &PodIdentity,
    uid: &str,
) -> Result<bool, InvalidWorkload> {
    if uid.is_empty()
        || pod.pointer("/metadata/uid").and_then(Value::as_str) != Some(uid)
        || pod.pointer("/metadata/name").and_then(Value::as_str) != Some(identity.name.as_str())
        || pod
            .pointer("/metadata/annotations/sandbox.elitea.ai~1job")
            .and_then(Value::as_str)
            != Some(identity.job.as_str())
        || pod
            .pointer("/metadata/annotations/sandbox.elitea.ai~1request")
            .and_then(Value::as_str)
            != Some(identity.request.as_str())
    {
        return Err(InvalidWorkload);
    }
    let Some(statuses) = pod
        .pointer("/status/containerStatuses")
        .and_then(Value::as_array)
    else {
        return Ok(false);
    };
    Ok(statuses.len() == 1
        && statuses[0].get("name").and_then(Value::as_str) == Some("code")
        && statuses[0]
            .pointer("/state/terminated/exitCode")
            .and_then(Value::as_i64)
            .is_some())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn policy() -> PodPolicy {
        PodPolicy {
            namespace: "sandbox".into(),
            image: format!("registry.example/runtime@sha256:{}", "a".repeat(64)),
            runtime_class: "sandbox".into(),
            node_selector: BTreeMap::from([("sandbox".into(), "true".into())]),
            memory_bytes: 512 * 1024 * 1024,
            cpu_millis: 1000,
            timeout_seconds: 120,
        }
    }
    #[test]
    fn pod_has_no_retry_controller_or_credential_mount_and_requires_warm_image() {
        let pod = policy()
            .workload(&PodIdentity::new(&[1; 32], &[2; 32]))
            .unwrap();
        assert_eq!(pod["kind"], "Pod");
        assert_eq!(pod["spec"]["restartPolicy"], "Never");
        assert_eq!(pod["spec"]["automountServiceAccountToken"], false);
        assert_eq!(pod["spec"]["serviceAccountName"], "elitea-code");
        assert_eq!(pod["spec"]["containers"][0]["imagePullPolicy"], "Never");
        assert_eq!(
            pod["spec"]["containers"][0]["resources"]["limits"]["cpu"],
            "1000m"
        );
        assert!(pod["metadata"]["ownerReferences"].is_null());
        assert_eq!(pod["metadata"]["finalizers"][0], FINALIZER);
    }
    #[test]
    fn rejects_missing_placement_mutable_image_and_unbounded_resources() {
        let id = PodIdentity::new(&[1; 32], &[2; 32]);
        let mut p = policy();
        p.image = "runtime:latest".into();
        assert!(p.workload(&id).is_err());
        let mut p = policy();
        p.node_selector.clear();
        assert!(p.workload(&id).is_err());
        let mut p = policy();
        p.cpu_millis = 0;
        assert!(p.workload(&id).is_err());
        let mut p = policy();
        p.timeout_seconds = 3601;
        assert!(p.workload(&id).is_err());
    }
    #[test]
    fn deletion_phase_and_missing_status_do_not_confirm_termination() {
        let id = PodIdentity::new(&[1; 32], &[2; 32]);
        let mut pod = policy().workload(&id).unwrap();
        pod["metadata"]["uid"] = json!("original");
        pod["metadata"]["deletionTimestamp"] = json!("2026-09-29T00:00:00Z");
        pod["status"] = json!({"phase":"Failed"});
        assert!(!confirmed_terminated(&pod, &id, "original").unwrap());
        pod["status"]["containerStatuses"] =
            json!([{"name":"code","state":{"terminated":{"exitCode":137}}}]);
        assert!(confirmed_terminated(&pod, &id, "original").unwrap());
        assert!(confirmed_terminated(&pod, &id, "replacement").is_err());
        assert!(confirmed_terminated(&Value::Null, &id, "original").is_err());
    }
}
