//! Immutable repository volume and original init-container boundary.
use super::{InvalidWorkload, PodIdentity, PodPolicy};
use serde_json::{Value, json};
const ROOT: &str = "/workspace/repository";
const ANNOTATION: &str = "sandbox.elitea.ai/workspace-manifest";
const HELPER: &str = "repository-hydrator";
fn valid(root: &str) -> bool {
    root.len() == 64
        && root
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
}
fn environment(identity: &PodIdentity, root: &str) -> Value {
    json!([
     {"name":"ELITEA_CODE_WORKSPACE_JOB_KEY","value":identity.job},
     {"name":"ELITEA_CODE_WORKSPACE_REQUEST_DIGEST","value":identity.request},
     {"name":"ELITEA_CODE_WORKSPACE_ROOT_SHA256","value":root},
     {"name":"ELITEA_SANDBOX_POD_UID","valueFrom":{"fieldRef":{"fieldPath":"metadata.uid"}}}
    ])
}
impl PodPolicy {
    pub(super) fn workload_repository(
        &self,
        identity: &PodIdentity,
        root: &str,
    ) -> Result<Value, InvalidWorkload> {
        if !valid(root) {
            return Err(InvalidWorkload);
        }
        let mut pod = self.workload(identity)?;
        pod["metadata"]["annotations"][ANNOTATION] = json!(root);
        pod["spec"]["volumes"]
            .as_array_mut()
            .ok_or(InvalidWorkload)?
            .push(json!({"name":"repository","emptyDir":{"medium":"Memory","sizeLimit":"144Mi"}}));
        pod["spec"]["containers"][0]["volumeMounts"]
            .as_array_mut()
            .ok_or(InvalidWorkload)?
            .push(json!({"name":"repository","mountPath":ROOT,"readOnly":true}));
        pod["spec"]["containers"][0]["env"]
            .as_array_mut()
            .ok_or(InvalidWorkload)?
            .extend(
                environment(identity, root)
                    .as_array()
                    .ok_or(InvalidWorkload)?[..3]
                    .iter()
                    .cloned(),
            );
        pod["spec"]["initContainers"] = json!([{
         "name":HELPER,"image":self.image,"imagePullPolicy":"Never",
         "command":["/bin/sh","-c","while [ ! -f /workspace/repository/.elitea-workspace-release ]; do sleep 0.1; done"],
         "resources":pod["spec"]["containers"][0]["resources"],"env":environment(identity,root),
         "securityContext":{"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}},
         "volumeMounts":[{"name":"repository","mountPath":ROOT},{"name":"tmp","mountPath":"/tmp"}]
        }]);
        Ok(pod)
    }
}

pub(super) fn validate(
    pod: &Value,
    identity: &PodIdentity,
    image: &str,
    root: &str,
) -> Result<(), InvalidWorkload> {
    if !valid(root)
        || pod
            .pointer("/metadata/annotations/sandbox.elitea.ai~1workspace-manifest")
            .and_then(Value::as_str)
            != Some(root)
        || pod.pointer("/spec/restartPolicy").and_then(Value::as_str) != Some("Never")
        || pod
            .pointer("/spec/securityContext/runAsUser")
            .and_then(Value::as_u64)
            != Some(10001)
        || pod
            .pointer("/spec/securityContext/runAsGroup")
            .and_then(Value::as_u64)
            != Some(10001)
        || pod
            .pointer("/spec/automountServiceAccountToken")
            .and_then(Value::as_bool)
            != Some(false)
    {
        return Err(InvalidWorkload);
    }
    let helpers = pod
        .pointer("/spec/initContainers")
        .and_then(Value::as_array)
        .ok_or(InvalidWorkload)?;
    if helpers.len() != 1 {
        return Err(InvalidWorkload);
    }
    let helper = &helpers[0];
    if helper["name"] != HELPER
        || helper["image"] != image
        || helper["imagePullPolicy"] != "Never"
        || helper["env"] != environment(identity, root)
        || helper["command"]
            != json!([
                "/bin/sh",
                "-c",
                "while [ ! -f /workspace/repository/.elitea-workspace-release ]; do sleep 0.1; done"
            ])
        || helper["securityContext"]
            != json!({"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}})
        || helper["volumeMounts"]
            != json!([{"name":"repository","mountPath":ROOT},{"name":"tmp","mountPath":"/tmp"}])
    {
        return Err(InvalidWorkload);
    }
    let mounts = pod
        .pointer("/spec/containers/0/volumeMounts")
        .and_then(Value::as_array)
        .ok_or(InvalidWorkload)?;
    let code_env = pod
        .pointer("/spec/containers/0/env")
        .and_then(Value::as_array)
        .ok_or(InvalidWorkload)?;
    for expected in &environment(identity, root)
        .as_array()
        .ok_or(InvalidWorkload)?[..3]
    {
        if code_env
            .iter()
            .filter(|value| value["name"] == expected["name"])
            .count()
            != 1
            || !code_env.contains(expected)
        {
            return Err(InvalidWorkload);
        }
    }
    if mounts
        .iter()
        .filter(|v| v["name"] == "repository" && v["mountPath"] == ROOT && v["readOnly"] == true)
        .count()
        != 1
        || mounts.iter().any(|v| {
            v["mountPath"]
                .as_str()
                .is_some_and(|p| p.starts_with(&format!("{ROOT}/")))
        })
    {
        return Err(InvalidWorkload);
    }
    let volumes = pod
        .pointer("/spec/volumes")
        .and_then(Value::as_array)
        .ok_or(InvalidWorkload)?;
    if volumes
        .iter()
        .filter(|v| {
            **v == json!({"name":"repository","emptyDir":{"medium":"Memory","sizeLimit":"144Mi"}})
        })
        .count()
        != 1
    {
        return Err(InvalidWorkload);
    }
    Ok(())
}

pub(super) fn helper_completed(pod: &Value) -> bool {
    pod.pointer("/status/initContainerStatuses")
        .and_then(Value::as_array)
        .is_some_and(|v| {
            v.len() == 1
                && v[0]["name"] == HELPER
                && v[0]["restartCount"] == 0
                && v[0]["lastState"]
                    .as_object()
                    .is_some_and(serde_json::Map::is_empty)
                && v[0]
                    .pointer("/state/terminated/exitCode")
                    .and_then(Value::as_i64)
                    == Some(0)
        })
}

// Failed Never-restarting init plus Initialized=false proves Code never started.
// A missing Pod or incomplete status remains unknown.
pub(super) fn stopped_before_code(pod: &Value) -> bool {
    let Some(root) = pod
        .pointer("/metadata/annotations/sandbox.elitea.ai~1workspace-manifest")
        .and_then(Value::as_str)
    else {
        return false;
    };
    if !valid(root) || pod.pointer("/spec/restartPolicy").and_then(Value::as_str) != Some("Never") {
        return false;
    }
    let init_failed = pod
        .pointer("/status/initContainerStatuses")
        .and_then(Value::as_array)
        .is_some_and(|v| {
            v.len() == 1
                && v[0]["name"] == HELPER
                && v[0]
                    .pointer("/state/terminated/exitCode")
                    .and_then(Value::as_i64)
                    .is_some_and(|n| n != 0)
                && v[0]["restartCount"] == 0
        });
    let never_initialized = pod
        .pointer("/status/conditions")
        .and_then(Value::as_array)
        .is_some_and(|v| {
            v.iter()
                .any(|v| v["type"] == "Initialized" && v["status"] == "False")
        });
    let no_code = pod
        .pointer("/status/containerStatuses")
        .and_then(Value::as_array)
        .is_none_or(|v| {
            v.is_empty()
                || v.len() == 1
                    && v[0]["name"] == "code"
                    && v[0]["restartCount"] == 0
                    && v[0]
                        .pointer("/state/waiting/reason")
                        .and_then(Value::as_str)
                        == Some("PodInitializing")
                    && v[0]["lastState"]
                        .as_object()
                        .is_some_and(serde_json::Map::is_empty)
        });
    init_failed
        && never_initialized
        && no_code
        && pod.pointer("/status/phase").and_then(Value::as_str) == Some("Failed")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;
    fn policy() -> PodPolicy {
        PodPolicy {
            namespace: "sandbox".into(),
            image: format!("registry.example/runner@sha256:{}", "a".repeat(64)),
            runtime_class: "isolated".into(),
            node_selector: BTreeMap::from([("sandbox".into(), "true".into())]),
            memory_bytes: 512 << 20,
            workspace_bytes: 256 << 20,
            cpu_millis: 1000,
            timeout_seconds: 120,
        }
    }
    #[test]
    fn original_pod_has_private_bounded_init_writable_and_code_readonly_mounts() {
        let p = policy();
        let identity = PodIdentity::new(&[1; 32], &[2; 32]);
        let root = "c".repeat(64);
        let mut pod = p.workload_repository(&identity, &root).unwrap();
        validate(&pod, &identity, &p.image, &root).unwrap();
        pod["spec"]["containers"][0]["volumeMounts"][2]["readOnly"] = json!(false);
        assert!(validate(&pod, &identity, &p.image, &root).is_err());
        let mut pod = p.workload_repository(&identity, &root).unwrap();
        pod["spec"]["initContainers"][0]["env"][0]["value"] = json!("other-job");
        assert!(validate(&pod, &identity, &p.image, &root).is_err());
    }
    #[test]
    fn init_stop_proof_requires_kubelet_failure_and_never_initialized_code() {
        let p = policy();
        let identity = PodIdentity::new(&[1; 32], &[2; 32]);
        let mut pod = p.workload_repository(&identity, &"c".repeat(64)).unwrap();
        pod["status"] = json!({"phase":"Failed","conditions":[{"type":"Initialized","status":"False"}],"initContainerStatuses":[{"name":HELPER,"restartCount":0,"state":{"terminated":{"exitCode":137}}}],"containerStatuses":[]});
        assert!(stopped_before_code(&pod));
        pod["status"]["conditions"][0]["status"] = json!("True");
        assert!(!stopped_before_code(&pod));
        assert!(!stopped_before_code(&Value::Null));
    }

    #[test]
    fn helper_success_requires_the_original_never_restarted_init() {
        let mut pod = json!({"status":{"initContainerStatuses":[{
            "name":HELPER,"restartCount":0,"lastState":{},
            "state":{"terminated":{"exitCode":0}}
        }]}});
        assert!(helper_completed(&pod));
        pod["status"]["initContainerStatuses"][0]["restartCount"] = json!(1);
        assert!(!helper_completed(&pod));
        pod["status"]["initContainerStatuses"][0]["restartCount"] = json!(0);
        pod["status"]["initContainerStatuses"][0]["lastState"] =
            json!({"terminated":{"exitCode":137}});
        assert!(!helper_completed(&pod));
        assert!(!helper_completed(&Value::Null));
    }
}
