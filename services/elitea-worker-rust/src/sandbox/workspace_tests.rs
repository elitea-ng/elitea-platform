use super::*;
use crate::sandbox::request::{Language, PreparedJob};

const FIXTURE: &[u8] =
    include_bytes!("../../../../libs/proto/elitea/runtime/v1/code_workspace_manifest_v1.json");
const ROOT: &str = "ccbf5e3ba22d5a09f07b063e4b8e65b7a7b1c6e6b4d0c6c4a5bb285e519c21c1";
fn manifest() -> WorkspaceManifest {
    WorkspaceManifest::from_transport(FIXTURE, ROOT, &WorkspacePolicy::default()).unwrap()
}
fn job() -> PreparedJob {
    PreparedJob::new(
        Language::Python,
        "7".into(),
        std::collections::BTreeMap::new(),
        format!("sha256:{}", "a".repeat(64)),
        "isolated-v1".into(),
        10,
    )
    .unwrap()
}
#[test]
fn exact_go_and_rust_manifest_policy_and_content_identities_match() {
    let manifest = manifest();
    assert_eq!(manifest.root(), ROOT);
    assert_eq!(
        WorkspacePolicy::default().sha256().unwrap(),
        "1ea826e498cca8dccb804c8e4dd93a0c0cb73f8ae23e34ef43fcab22034d93c9"
    );
    assert_eq!(manifest.files()[0].path, "src/greeting.txt");
    assert_eq!(manifest.files()[0].bytes, 5);
    assert!(manifest.binding().unwrap().matches(&manifest).unwrap());
}
#[test]
fn omitted_workspace_preserves_exact_existing_job_bytes_and_fingerprint() {
    let original = job();
    let bytes = original.to_transport().unwrap();
    assert!(!std::str::from_utf8(&bytes).unwrap().contains("workspace"));
    assert!(bytes.starts_with(b"{\"revision\":1,"));
    let rebuilt = PreparedJob::from_transport(&bytes).unwrap();
    assert_eq!(rebuilt.to_transport().unwrap(), bytes);
    assert_eq!(
        rebuilt.fingerprint().unwrap(),
        original.fingerprint().unwrap()
    );
}
#[test]
fn workspace_root_mode_policy_and_resource_bind_actual_request_identity() {
    let original = job().with_workspace(manifest().binding().unwrap()).unwrap();
    let bytes = original.to_transport().unwrap();
    assert!(bytes.starts_with(b"{\"revision\":4,"));
    let rebuilt = PreparedJob::from_transport(&bytes).unwrap();
    assert_eq!(rebuilt.to_transport().unwrap(), bytes);
    let binding = manifest().binding().unwrap();
    let mut changed = Vec::new();
    let mut root = binding.clone();
    root.manifest_sha256 = "3".repeat(64);
    changed.push(root);
    let mut mode = binding.clone();
    mode.selection.mode = WorkspaceMode::Readwrite;
    changed.push(mode);
    let mut policy = binding.clone();
    policy.policy_sha256 = "4".repeat(64);
    changed.push(policy);
    let mut repository = binding;
    repository.selection.repository_id = "124".into();
    changed.push(repository);
    for binding in changed {
        assert_ne!(
            original.fingerprint().unwrap(),
            job()
                .with_workspace(binding)
                .unwrap()
                .fingerprint()
                .unwrap()
        );
    }
}
#[test]
fn invalid_workspace_request_revision_null_extra_fields_and_stale_root_are_refused() {
    let bytes = job()
        .with_workspace(manifest().binding().unwrap())
        .unwrap()
        .to_transport()
        .unwrap();
    let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    for revision in [1, 2, 3] {
        let mut v = value.clone();
        v["revision"] = revision.into();
        assert!(PreparedJob::from_transport(&serde_json::to_vec(&v).unwrap()).is_err());
    }
    let mut null = value.clone();
    null["workspace"] = serde_json::Value::Null;
    assert!(PreparedJob::from_transport(&serde_json::to_vec(&null).unwrap()).is_err());
    let mut extra = value;
    extra["workspace"]["host_path"] = "/private/source".into();
    assert!(PreparedJob::from_transport(&serde_json::to_vec(&extra).unwrap()).is_err());
    assert!(
        WorkspaceManifest::from_transport(FIXTURE, &"f".repeat(64), &WorkspacePolicy::default())
            .is_err()
    );
}
#[test]
fn unsafe_paths_overlapping_projections_and_operator_bounds_fail_before_hydration() {
    let p = WorkspacePolicy::default();
    for path in [
        "../outside",
        "/root",
        "src/../file",
        "src//file",
        "src\\file",
        ".git/config",
        "src/.ELITEA-platform/file",
        "src/name:stream",
    ] {
        assert!(validate_path(path, &p).is_err(), "accepted {path}");
    }
    let mut selected = manifest().selection().clone();
    selected.include = vec!["src".into(), "src/nested".into()];
    assert!(selected.validate(&p).is_err());
    let mut policy = p;
    policy.max_path_bytes = 3;
    assert!(matches!(
        selected.validate(&policy),
        Err(WorkspaceError::Bound {
            bound: "max_path_bytes",
            limit: 3
        })
    ));
}
