use super::*;
use std::os::unix::fs::{PermissionsExt, symlink};
fn deadline() -> Instant {
    Instant::now() + Duration::from_secs(30)
}
fn platform() -> Platform {
    Platform {
        os: "linux".into(),
        arch: if std::env::consts::ARCH == "aarch64" {
            "arm64"
        } else {
            "amd64"
        }
        .into(),
        abi: "gnu".into(),
    }
}
fn fixture_image(directory: &Path) -> CargoImage {
    let cargo = directory.join("cargo-fixture");
    std::fs::write(&cargo,r#"#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$(dirname "$0")/commands"
case "$*" in
 generate-lockfile)
  printf 'version = 4\n[[package]]\nname = "elitea-code-job"\nversion = "0.1.0"\n' > Cargo.lock
  ;;
 'vendor --locked --versioned-dirs vendor')
  mkdir -p vendor/fixture-1.0.0/src
  printf 'pub struct Fixture;\n' > vendor/fixture-1.0.0/src/lib.rs
  printf '[source.crates-io]\nreplace-with = "vendored-sources"\n[source.vendored-sources]\ndirectory = "vendor"\n'
  ;;
 *) exit 90 ;;
esac
"#).unwrap();
    std::fs::set_permissions(&cargo, std::fs::Permissions::from_mode(0o700)).unwrap();
    // Constructors keep all paths in the trusted fixture. The production command exposes no path options.
    CargoImage::fixture(cargo, directory.join("rustup"), "/usr/bin:/bin".into())
}
fn prep() -> NativePreparation {
    NativePreparation {
        revision: 2,
        language: "rust".into(),
        source: "[dependencies]\nfixture='1'\n".into(),
        preparer_image_digest: format!("sha256:{}", "a".repeat(64)),
        policy_revision: "cargo-acquire-v1".into(),
        timeout_seconds: 30,
        platform: platform(),
        execution_image_digest: format!("sha256:{}", "b".repeat(64)),
        execution_policy_revision: "cargo-execute-v1".into(),
    }
}
fn envelope(
    job: &NativePreparation,
    record: &PreparationRecord,
    record_bytes: &[u8],
) -> NativeBundle {
    let record_hash = digest(record_bytes);
    let payload = CargoPayload {
        revision: 1,
        preparation_sha256: job.fingerprint().unwrap(),
        declaration_sha256: digest(job.source.as_bytes()),
        profile: CargoRuntime {
            preparation_image: job.preparer_image_digest.clone(),
            execution_image: job.execution_image_digest.clone(),
            rust_revision: "1.97.1".into(),
            os: "linux".into(),
            arch: job.platform.arch.clone(),
            target: job.platform.target().unwrap().into(),
            policy_revision: job.execution_policy_revision.clone(),
        },
        objects: [
            CargoObject {
                role: "record".into(),
                name: format!("{record_hash}.blob"),
                bytes: record_bytes.len() as u64,
                sha256: record_hash,
            },
            CargoObject {
                role: "archive".into(),
                name: format!("{}.blob", record.content.sha256),
                bytes: record.content.compressed_bytes,
                sha256: record.content.sha256.clone(),
            },
        ],
    };
    let mut bundle = NativeBundle {
        revision: 2,
        kind: "cargo".into(),
        language: "rust".into(),
        platform: job.platform.clone(),
        preparation_sha256: job.fingerprint().unwrap(),
        source_sha256: digest(job.source.as_bytes()),
        execution_image_digest: job.execution_image_digest.clone(),
        execution_policy_revision: job.execution_policy_revision.clone(),
        payload: serde_json::to_value(payload).unwrap(),
        digest: String::new(),
    };
    bundle.digest = bundle.computed_root().unwrap();
    bundle
}
fn execution(job: &NativePreparation, bundle: &NativeBundle) -> NativeExecution {
    NativeExecution {revision:3,language:"rust".into(),source:"pub fn run(input:serde_json::Value)->Result<serde_json::Value,Box<dyn std::error::Error>> {Ok(input)}".into(),input:BTreeMap::from([("ordinary_state".into(),serde_json::json!({"nested":[1,true]}))]),image_digest:job.execution_image_digest.clone(),policy_revision:job.execution_policy_revision.clone(),timeout_seconds:30,dependency_bundle_sha256:bundle.digest.clone(),native_dependencies:NativeDependencies {kind:"cargo".into(),platform:job.platform.clone(),preparation_sha256:job.fingerprint().unwrap(),source_sha256:digest(job.source.as_bytes()),dependencies_toml:job.source.clone()},workspace:None,platform_client:None}
}
async fn imported(workspace: &Path) -> (NativeExecution, Vec<u8>, PreparationRecord) {
    imported_job(workspace, prep()).await
}
async fn imported_job(
    workspace: &Path,
    job: NativePreparation,
) -> (NativeExecution, Vec<u8>, PreparationRecord) {
    let image = fixture_image(workspace);
    let output = workspace.join("component");
    let record = prepare_profile(
        job.profile().unwrap(),
        job.source.as_bytes(),
        &output,
        Duration::from_secs(10),
        &image,
    )
    .await
    .unwrap();
    let record_bytes = std::fs::read(output.join("record.json")).unwrap();
    let bundle = envelope(&job, &record, &record_bytes);
    let objects = workspace.join("native-bundle/objects");
    std::fs::create_dir_all(&objects).unwrap();
    std::fs::create_dir(workspace.join("native-bundle/.elitea-native-finalizing")).unwrap();
    let payload = bundle.validate().unwrap();
    std::fs::copy(
        output.join("record.json"),
        objects.join(&payload.objects[0].name),
    )
    .unwrap();
    std::fs::copy(
        output.join("content.tar.gz"),
        objects.join(&payload.objects[1].name),
    )
    .unwrap();
    let mut request = execution(&job, &bundle);
    if job.execution_policy_revision == crate::rust_profile::BROKER_POLICY {
        request.revision = 5;
        request.platform_client = Some(
            serde_json::json!({"revision":1,"policy_sha256":"a".repeat(64),"max_calls":128,"max_total_bytes":16777216}),
        );
    }
    (request, bundle.canonical().unwrap(), record)
}

#[tokio::test]
async fn hydration_preserves_exact_native_profile_and_does_not_resolve_again() {
    let workspace = tempfile::tempdir().unwrap();
    let (request, metadata, record) = imported(workspace.path()).await;
    let commands = std::fs::read(workspace.path().join("commands")).unwrap();
    hydrate(workspace.path(), &request, &metadata, deadline()).unwrap();
    hydrate(workspace.path(), &request, &metadata, deadline()).unwrap();
    let root = workspace.path().join("rust-job/profile");
    assert_eq!(
        digest(&std::fs::read(root.join("Cargo.lock")).unwrap()),
        record.lock_sha256
    );
    assert_eq!(
        digest(&std::fs::read(root.join("src/main.rs")).unwrap()),
        record.wrapper_sha256
    );
    assert_eq!(
        std::fs::read(workspace.path().join("commands")).unwrap(),
        commands
    );
    assert!(
        std::fs::read_dir(
            workspace
                .path()
                .join("native-bundle/.elitea-native-finalizing")
        )
        .unwrap()
        .next()
        .is_none()
    );
    // Metadata alone does not prove final readiness or authorize execution.
    exclusive_file(
        &workspace.path().join("native-bundle").join(METADATA),
        &metadata,
    )
    .unwrap();
    assert!(execution_profile(workspace.path(), &request, false, deadline()).is_err());
    exclusive_file(
        &workspace
            .path()
            .join("native-bundle/elitea-native-ready-v2.json"),
        &metadata,
    )
    .unwrap();
    assert!(execution_profile(workspace.path(), &request, false, deadline()).is_ok());
    std::fs::write(root.join("src/user.rs"), "admitted source").unwrap();
    assert!(execution_profile(workspace.path(), &request, false, deadline()).is_err());
    assert!(execution_profile(workspace.path(), &request, true, deadline()).is_ok());
    std::fs::write(root.join("Cargo.lock"), "changed").unwrap();
    assert!(execution_profile(workspace.path(), &request, true, deadline()).is_err());
}
#[tokio::test]
async fn rejects_linked_roots_corrupt_objects_and_changed_request_before_ready() {
    let workspace = tempfile::tempdir().unwrap();
    let (mut request, metadata, _) = imported(workspace.path()).await;
    request
        .native_dependencies
        .dependencies_toml
        .push_str("# changed");
    assert!(hydrate(workspace.path(), &request, &metadata, deadline()).is_err());
    assert!(!workspace.path().join("rust-job").exists());
    request.native_dependencies.dependencies_toml = prep().source;
    let outside = tempfile::tempdir().unwrap();
    symlink(outside.path(), workspace.path().join("rust-job")).unwrap();
    assert!(hydrate(workspace.path(), &request, &metadata, deadline()).is_err());
    assert!(std::fs::read_dir(outside.path()).unwrap().next().is_none());
    std::fs::remove_file(workspace.path().join("rust-job")).unwrap();
    let bundle = NativeBundle::from_bytes(&metadata).unwrap();
    let payload = bundle.validate().unwrap();
    std::fs::write(
        workspace
            .path()
            .join("native-bundle/objects")
            .join(&payload.objects[1].name),
        "corrupt archive",
    )
    .unwrap();
    assert!(hydrate(workspace.path(), &request, &metadata, deadline()).is_err());
    assert!(!workspace.path().join("rust-job").exists());
    assert!(
        !workspace
            .path()
            .join("native-bundle/elitea-native-ready-v2.json")
            .exists()
    );
}
#[test]
fn envelope_and_payload_bind_every_profile_and_object_field() {
    let mut job = prep();
    assert!(job.validate().is_ok());
    job.source = "fn main() { panic!(\"never run source\") }".into();
    assert!(job.validate().is_err());
    job = prep();
    job.platform.abi = "musl".into();
    assert!(job.validate().is_err());
    assert!(!hash_shape(&"A".repeat(64)));
}
#[tokio::test]
async fn retained_acquisition_waits_for_release_and_reuses_the_original_objects() {
    let directory = tempfile::tempdir().unwrap();
    let workspace = directory.path().to_path_buf();
    let image = fixture_image(&workspace);
    std::fs::write(
        workspace.join(".elitea-code.json"),
        serde_json::to_vec(&prep()).unwrap(),
    )
    .unwrap();
    let retained = retain_in(&workspace, &image);
    let release = async {
        let limit = deadline();
        while !workspace.join(".elitea-native-preparation.json").exists() {
            assert!(Instant::now() < limit);
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert!(!workspace.join("result.json").exists());
        exclusive_file(&workspace.join(".elitea-native-preparation-release"), b"").unwrap();
    };
    let (record, _) = tokio::join!(retained, release);
    record.unwrap();
    let original = std::fs::read(workspace.join("commands")).unwrap();
    retain_in(&workspace, &image).await.unwrap();
    assert_eq!(std::fs::read(workspace.join("commands")).unwrap(), original);
    let mut marker: NativeMarker = serde_json::from_slice(
        &std::fs::read(workspace.join(".elitea-native-preparation.json")).unwrap(),
    )
    .unwrap();
    marker.deadline_unix_ms = marker.started_unix_ms - 1;
    std::fs::write(
        workspace.join(".elitea-native-preparation.json"),
        serde_json::to_vec(&marker).unwrap(),
    )
    .unwrap();
    assert!(retain_in(&workspace, &image).await.is_err());
    assert_eq!(std::fs::read(workspace.join("commands")).unwrap(), original);
}

#[test]
fn exact_cross_language_native_and_preparation_fixtures_match() {
    let bytes = include_bytes!("../fixtures/cargo-native-v2.json");
    let bundle: NativeBundle = serde_json::from_slice(bytes).unwrap();
    assert_eq!(bundle.canonical().unwrap(), bytes);
    assert_eq!(
        bundle.computed_root().unwrap(),
        include_str!("../fixtures/cargo-native-v2.sha256").trim()
    );
    let job: NativePreparation =
        serde_json::from_slice(include_bytes!("../fixtures/cargo-preparation-v2.json")).unwrap();
    assert_eq!(job.fingerprint().unwrap(), bundle.preparation_sha256);
    let duplicate = String::from_utf8(bytes.to_vec()).unwrap().replacen(
        "\"revision\":1",
        "\"revision\":1,\"revision\":1",
        1,
    );
    assert!(NativeBundle::from_bytes(duplicate.as_bytes()).is_err());
}

#[tokio::test]
async fn broker_native_import_binds_original_policy_modules_and_profile() {
    let workspace = tempfile::tempdir().unwrap();
    let mut job = prep();
    job.execution_policy_revision = crate::rust_profile::BROKER_POLICY.into();
    let (mut request, metadata, record) = imported_job(workspace.path(), job).await;
    assert_eq!(request.profile().unwrap(), Profile::Broker);
    hydrate(workspace.path(), &request, &metadata, deadline()).unwrap();
    let root = workspace.path().join("rust-job/profile");
    for (path, bytes) in Profile::Broker.sources() {
        assert_eq!(std::fs::read(root.join(path)).unwrap(), bytes.as_bytes());
    }
    exclusive_file(
        &workspace.path().join("native-bundle").join(METADATA),
        &metadata,
    )
    .unwrap();
    exclusive_file(
        &workspace
            .path()
            .join("native-bundle/elitea-native-ready-v2.json"),
        &metadata,
    )
    .unwrap();
    assert!(execution_profile(workspace.path(), &request, false, deadline()).is_ok());
    request.policy_revision = "cargo-execute-v1".into();
    assert!(execution_profile(workspace.path(), &request, false, deadline()).is_err());
    request.policy_revision = crate::rust_profile::BROKER_POLICY.into();
    request.image_digest = format!("sha256:{}", "c".repeat(64));
    assert!(execution_profile(workspace.path(), &request, false, deadline()).is_err());
    request.image_digest = prep().execution_image_digest;
    request.platform_client = None;
    request.revision = 3;
    assert!(execution_profile(workspace.path(), &request, false, deadline()).is_err());
    request.platform_client = Some(
        serde_json::json!({"revision":1,"policy_sha256":"a".repeat(64),"max_calls":128,"max_total_bytes":16777216}),
    );
    request.revision = 5;
    std::fs::write(root.join("src/platform_pipe.rs"), "forged pipe transport").unwrap();
    assert!(execution_profile(workspace.path(), &request, true, deadline()).is_err());
    assert_eq!(
        record.wrapper_sha256,
        crate::rust_profile::rust_prepare_archive::digest(Profile::Broker.wrapper().as_bytes())
    );
}

#[test]
fn broker_preparation_keeps_revision_two_and_rejects_forged_selector_fields() {
    let legacy = prep();
    let bytes = serde_json::to_vec(&legacy).unwrap();
    let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(value["revision"], 2);
    assert_eq!(value.as_object().unwrap().len(), 9);
    assert_eq!(
        serde_json::to_vec(&serde_json::from_slice::<NativePreparation>(&bytes).unwrap()).unwrap(),
        bytes
    );
    for field in [
        "profile",
        "profile_path",
        "argv",
        "template",
        "platform_client",
    ] {
        let mut forged = value.clone();
        forged[field] = serde_json::json!("caller-owned");
        assert!(serde_json::from_value::<NativePreparation>(forged).is_err());
    }
    let mut broker = legacy;
    broker.execution_policy_revision = crate::rust_profile::BROKER_POLICY.into();
    assert_eq!(broker.profile().unwrap(), Profile::Broker);
    broker.source = "[dependencies]\nserde='1'\n".into();
    assert!(broker.validate().is_err());
}

#[test]
fn broker_execution_parser_rejects_capability_policy_and_profile_forgery() {
    let job = prep();
    let capability = serde_json::json!({"revision":1,"policy_sha256":"a".repeat(64),"max_calls":128,"max_total_bytes":16777216});
    let request = serde_json::json!({
        "revision":5,"language":"rust","source":"pub fn run(input: serde_json::Value)->Result<serde_json::Value,Box<dyn std::error::Error>> { Ok(input) }",
        "input":{"selected":"value"},"image_digest":job.execution_image_digest,
        "policy_revision":crate::rust_profile::BROKER_POLICY,"timeout_seconds":30,
        "dependency_bundle_sha256":"f".repeat(64),
        "native_dependencies":{"kind":"cargo","platform":job.platform,
            "preparation_sha256":job.fingerprint().unwrap(),"source_sha256":digest(job.source.as_bytes()),"dependencies_toml":job.source},
        "platform_client":capability,
    });
    let decode = |value: &serde_json::Value| {
        NativeExecution::from_bytes(&serde_json::to_vec(value).unwrap())
    };
    assert_eq!(
        decode(&request).unwrap().profile().unwrap(),
        Profile::Broker
    );
    for field in ["profile", "profile_path", "wrapper", "template", "argv"] {
        let mut forged = request.clone();
        forged[field] = serde_json::json!("caller-owned");
        assert!(decode(&forged).is_err());
    }
    for policy in [
        "cargo-execute-v1",
        "cargo-broker-execute-v10",
        "caller-profile-v1",
    ] {
        let mut forged = request.clone();
        forged["policy_revision"] = serde_json::json!(policy);
        assert!(decode(&forged).is_err());
    }
    for revision in [1, 2, 3, 4] {
        let mut forged = request.clone();
        forged["revision"] = serde_json::json!(revision);
        assert!(decode(&forged).is_err());
    }
    let mut forged = request.clone();
    forged.as_object_mut().unwrap().remove("platform_client");
    assert!(decode(&forged).is_err());
    forged["revision"] = serde_json::json!(3);
    assert!(decode(&forged).is_err());
    forged["policy_revision"] = serde_json::json!("cargo-execute-v1");
    assert_eq!(decode(&forged).unwrap().profile().unwrap(), Profile::Legacy);
    forged = request;
    forged["platform_client"]["actor_id"] = serde_json::json!(77);
    assert!(decode(&forged).is_err());
}
