use super::*;
use crate::sandbox::{
    compiled_snapshot::Control, native_bundle::NativePlatform, request::Language,
};
use std::{
    fs,
    os::unix::fs::{PermissionsExt, symlink},
};

fn binding() -> Binding {
    serde_json::from_slice::<Control>(include_bytes!(
        "../../tests/fixtures/compiled-snapshot-v1/compile-control.json"
    ))
    .unwrap()
    .binding
}

fn manifest(bindings: Vec<(Binding, String)>) -> Vec<u8> {
    serde_json::to_vec(&ReleaseProfiles {
        revision: 1,
        profiles: bindings
            .into_iter()
            .map(|(binding, dependency_bundle_sha256)| ReleaseProfile {
                binding,
                dependency_bundle_sha256,
            })
            .collect(),
    })
    .unwrap()
}

fn config(raw: &[u8], bundle: String) -> RustCompiledSnapshotConfig {
    RustCompiledSnapshotConfig {
        profiles_file: "/run/elitea-runtime/compiled-profiles.json".into(),
        profiles_sha256: ContentSha256::of(raw).as_str().into(),
        dependency_bundle_sha256: bundle,
    }
}

fn parse(raw: &[u8]) -> Result<Arc<SnapshotProfile>, CompiledProfileError> {
    let template = binding();
    config(raw, String::new()).parse(
        raw,
        &template.execution_image_digest,
        &template.policy_revision,
        None,
    )
}

#[test]
fn exact_main_manifest_selects_one_profile_and_preserves_every_digest() {
    let template = binding();
    let mut other = template.clone();
    other.compilation_image_digest = format!("sha256:{}", "e".repeat(64));
    other
        .execution_image_digest
        .clone_from(&other.compilation_image_digest);
    let raw = manifest(vec![
        (other, String::new()),
        (template.clone(), String::new()),
    ]);
    let selected = parse(&raw).unwrap();
    let job = crate::sandbox::request::PreparedJob::new(
        Language::Rust,
        "pub fn run() {}".into(),
        std::collections::BTreeMap::new(),
        template.execution_image_digest.clone(),
        template.policy_revision.clone(),
        30,
    )
    .unwrap();
    let mut expected = template;
    expected.tenant_id = "tenant-release".into();
    expected.project_id = 7;
    expected.base_prepared_request_sha256 =
        crate::sandbox::compiled_snapshot::prepared_fingerprint(&job.to_transport().unwrap());
    expected.source_sha256 = ContentSha256::of(b"pub fn run() {}");
    assert_eq!(
        selected.binding(&job, "tenant-release", 7).unwrap(),
        expected
    );
}

#[test]
fn rejects_changed_file_bytes_even_with_valid_json() {
    let raw = manifest(vec![(binding(), String::new())]);
    let selected = config(&raw, String::new());
    let mut changed = raw.clone();
    changed.push(b'\n');
    assert!(matches!(
        selected.parse(
            &changed,
            &binding().execution_image_digest,
            &binding().policy_revision,
            None
        ),
        Err(CompiledProfileError::Pin)
    ));
}

#[test]
fn rejects_noncanonical_unknown_duplicate_and_trailing_json() {
    let raw = manifest(vec![(binding(), String::new())]);
    let mut newline = raw.clone();
    newline.push(b'\n');
    let mut extra = raw.clone();
    extra.extend_from_slice(b"{}");
    let unknown = String::from_utf8(raw.clone())
        .unwrap()
        .replacen("{\"revision\":1,", "{\"revision\":1,\"unknown\":0,", 1)
        .into_bytes();
    let duplicate = String::from_utf8(raw)
        .unwrap()
        .replacen("{\"revision\":1,", "{\"revision\":1,\"revision\":1,", 1)
        .into_bytes();
    for bytes in [newline, extra, unknown, duplicate] {
        assert!(matches!(parse(&bytes), Err(CompiledProfileError::Contract)));
    }
}

#[test]
fn rejects_empty_oversized_and_unversioned_manifests() {
    let raw = vec![b' '; PROFILE_BYTES + 1];
    assert!(matches!(parse(&raw), Err(CompiledProfileError::File)));
    assert!(matches!(parse(b""), Err(CompiledProfileError::File)));
    let empty = manifest(vec![]);
    assert!(matches!(parse(&empty), Err(CompiledProfileError::Contract)));
    let many = manifest((0..65).map(|_| (binding(), String::new())).collect());
    assert!(matches!(parse(&many), Err(CompiledProfileError::Contract)));
    let wrong = manifest(vec![(binding(), String::new())]);
    let wrong = String::from_utf8(wrong)
        .unwrap()
        .replacen("{\"revision\":1,", "{\"revision\":2,", 1)
        .into_bytes();
    assert!(matches!(parse(&wrong), Err(CompiledProfileError::Contract)));
}

#[test]
fn rejects_duplicate_ambiguous_and_invalid_release_records() {
    let template = binding();
    let duplicate = manifest(vec![
        (template.clone(), String::new()),
        (template.clone(), String::new()),
    ]);
    assert!(matches!(
        parse(&duplicate),
        Err(CompiledProfileError::Profiles)
    ));
    let mut ambiguous = template.clone();
    ambiguous.cargo_lock_sha256 = ContentSha256::of(b"different exact Cargo lock");
    let ambiguous = manifest(vec![
        (template.clone(), String::new()),
        (ambiguous, String::new()),
    ]);
    assert!(matches!(
        parse(&ambiguous),
        Err(CompiledProfileError::Selection)
    ));
    let mut invalid = template;
    invalid.reuse_policy = "unchecked".into();
    let invalid = manifest(vec![(invalid, String::new())]);
    assert!(matches!(
        parse(&invalid),
        Err(CompiledProfileError::Profiles)
    ));
}

#[test]
fn rejects_unattested_image_policy_or_bundle_selection() {
    let template = binding();
    let raw = manifest(vec![(template.clone(), String::new())]);
    let setting = config(&raw, String::new());
    for (image, policy) in [
        ("sha256:unknown", template.policy_revision.as_str()),
        (template.execution_image_digest.as_str(), "different-policy"),
    ] {
        assert!(matches!(
            setting.parse(&raw, image, policy, None),
            Err(CompiledProfileError::Selection)
        ));
    }
    let setting = config(&raw, "c".repeat(64));
    assert!(matches!(
        setting.parse(
            &raw,
            &template.execution_image_digest,
            &template.policy_revision,
            None
        ),
        Err(CompiledProfileError::Selection)
    ));
}

#[test]
fn native_bundle_selection_requires_exact_platform() {
    let template = binding();
    let raw = manifest(vec![(template.clone(), "c".repeat(64))]);
    let setting = config(&raw, "c".repeat(64));
    let platform = NativePlatform {
        os: "linux".into(),
        arch: "arm64".into(),
        abi: "gnu".into(),
    };
    assert!(
        setting
            .parse(
                &raw,
                &template.execution_image_digest,
                &template.policy_revision,
                Some(&platform)
            )
            .is_ok()
    );
    assert!(matches!(
        setting.parse(
            &raw,
            &template.execution_image_digest,
            &template.policy_revision,
            None
        ),
        Err(CompiledProfileError::Platform)
    ));
    let wrong = NativePlatform {
        arch: "amd64".into(),
        ..platform
    };
    assert!(matches!(
        setting.parse(
            &raw,
            &template.execution_image_digest,
            &template.policy_revision,
            Some(&wrong)
        ),
        Err(CompiledProfileError::Platform)
    ));
}

#[test]
fn optional_settings_refuse_missing_fields_and_unsafe_paths_or_pins() {
    for raw in [
        "{}",
        r#"{"profiles_file":"/run/profiles.json"}"#,
        r#"{"profiles_file":"/run/profiles.json","profiles_sha256":"a"}"#,
    ] {
        assert!(serde_json::from_str::<RustCompiledSnapshotConfig>(raw).is_err());
    }
    let raw = manifest(vec![(binding(), String::new())]);
    for path in ["relative.json", "/run/../profiles.json"] {
        let mut setting = config(&raw, String::new());
        setting.profiles_file = path.into();
        assert!(setting.validate().is_err());
    }
    let mut setting = config(&raw, String::new());
    setting.profiles_sha256 = "A".repeat(64);
    assert!(setting.validate().is_err());
}

#[test]
fn secure_loader_accepts_regular_files_and_rejects_symlinks_or_mutable_files() {
    let directory = tempfile::tempdir().unwrap();
    let directory = directory.path().canonicalize().unwrap();
    let raw = manifest(vec![(binding(), String::new())]);
    let path = directory.join("profiles.json");
    fs::write(&path, &raw).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
    let mut setting = config(&raw, String::new());
    setting.profiles_file = path.clone();
    let template = binding();
    assert!(
        setting
            .load(
                &template.execution_image_digest,
                &template.policy_revision,
                None
            )
            .is_ok()
    );
    let linked = directory.join("linked.json");
    symlink(&path, &linked).unwrap();
    setting.profiles_file = linked;
    assert!(matches!(
        setting.load(
            &template.execution_image_digest,
            &template.policy_revision,
            None
        ),
        Err(CompiledProfileError::File)
    ));
    setting.profiles_file = path.clone();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o622)).unwrap();
    assert!(matches!(
        setting.load(
            &template.execution_image_digest,
            &template.policy_revision,
            None
        ),
        Err(CompiledProfileError::File)
    ));
}

#[test]
fn worker_factory_loader_stays_disabled_and_rejects_non_rust_selection() {
    let template = binding();
    let runtime: crate::config::SandboxRuntimeConfig = serde_json::from_value(serde_json::json!({
        "language":"rust","target":"sandbox.internal:9447","audience":"dns:sandbox.internal",
        "image_digest":template.execution_image_digest,"policy_revision":template.policy_revision,"timeout_seconds":30
    })).unwrap();
    assert!(
        load_worker_profile(std::slice::from_ref(&runtime))
            .unwrap()
            .is_none()
    );
    let mut runtime = runtime;
    runtime.language = Language::Python;
    runtime.compiled_snapshot = Some(config(
        &manifest(vec![(binding(), String::new())]),
        String::new(),
    ));
    assert!(matches!(
        load_worker_profile(&[runtime]),
        Err(CompiledProfileError::Configuration)
    ));
}
