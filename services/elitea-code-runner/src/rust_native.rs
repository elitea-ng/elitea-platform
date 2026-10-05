//! Cargo language ownership for retained acquisition and inert supplied-profile import.
//! Outer grant, runtime fencing, cancellation and publication belong to the supervisor.
#![allow(dead_code)]
use crate::rust_profile::rust_prepare_archive::{
    digest, extract_archive, verify_archive, verify_tree,
};
use crate::rust_profile::{
    CargoImage, ErrorCode, PreparationRecord, PrepareError, Profile, Result, check_vendor_config,
    deadline_check, io_error, parse_dependencies, prepare_profile, read_regular,
    verify_record_profile,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    io::Write,
    path::{Path, PathBuf},
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::time::Instant;

// The standalone preparer also embeds this parser through its native owner.
#[allow(clippy::duplicate_mod)]
#[path = "code_prepared_extensions.rs"]
mod code_prepared_extensions;

const RECORD_LIMIT: u64 = 8 * 1024 * 1024;
const ARCHIVE_LIMIT: u64 = 128 * 1024 * 1024;
const METADATA_LIMIT: u64 = 128 * 1024;
const REQUEST_LIMIT: u64 = 1024 * 1024;
const METADATA: &str = "elitea-native-bundle-v2.json";
const PROFILE_MARKER: &str = ".elitea-cargo-profile.json";

fn invalid() -> PrepareError {
    PrepareError::new(
        ErrorCode::IntegrityMismatch,
        "Cargo native profile identity is invalid",
    )
}
fn hash_shape(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
fn image_shape(value: &str) -> bool {
    value.strip_prefix("sha256:").is_some_and(hash_shape)
}
fn policy_shape(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
}

#[derive(Clone, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct Platform {
    pub(crate) os: String,
    pub(crate) arch: String,
    pub(crate) abi: String,
}
impl Platform {
    fn target(&self) -> Result<&'static str> {
        if self.os != "linux" || self.abi != "gnu" {
            return Err(invalid());
        }
        match self.arch.as_str() {
            "amd64" => Ok("x86_64-unknown-linux-gnu"),
            "arm64" => Ok("aarch64-unknown-linux-gnu"),
            _ => Err(invalid()),
        }
    }
    fn matches_image(&self) -> Result<()> {
        self.target()?;
        let expected = match std::env::consts::ARCH {
            "x86_64" => "amd64",
            "aarch64" => "arm64",
            _ => return Err(invalid()),
        };
        if std::env::consts::OS != "linux" || self.arch != expected {
            return Err(invalid());
        }
        Ok(())
    }
}

// Preserve the worker's serialized field order. No execution state or user Rust source enters acquisition.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct NativePreparation {
    revision: u8,
    language: String,
    source: String,
    preparer_image_digest: String,
    policy_revision: String,
    timeout_seconds: u32,
    platform: Platform,
    execution_image_digest: String,
    execution_policy_revision: String,
}
impl NativePreparation {
    fn validate(&self) -> Result<()> {
        if self.revision != 2
            || self.language != "rust"
            || !image_shape(&self.preparer_image_digest)
            || !image_shape(&self.execution_image_digest)
            || !policy_shape(&self.policy_revision)
            || !policy_shape(&self.execution_policy_revision)
            || !(1..=600).contains(&self.timeout_seconds)
        {
            return Err(invalid());
        }
        self.platform.matches_image()?;
        self.profile()?
            .validate_dependencies(&parse_dependencies(self.source.as_bytes())?)?;
        Ok(())
    }
    fn profile(&self) -> Result<Profile> {
        Profile::for_policy(&self.execution_policy_revision)
    }
    fn fingerprint(&self) -> Result<String> {
        let bytes = serde_json::to_vec(self).map_err(|_| invalid())?;
        let mut hash = Sha256::new();
        hash.update(b"elitea.sandbox.preparation-job.v1\0");
        hash.update((bytes.len() as u64).to_be_bytes());
        hash.update(bytes);
        Ok(format!("{:x}", hash.finalize()))
    }
}

#[derive(Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct CargoRuntime {
    preparation_image: String,
    execution_image: String,
    rust_revision: String,
    os: String,
    arch: String,
    target: String,
    policy_revision: String,
}
#[derive(Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct CargoObject {
    role: String,
    name: String,
    bytes: u64,
    sha256: String,
}
#[derive(Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct CargoPayload {
    revision: u8,
    preparation_sha256: String,
    declaration_sha256: String,
    profile: CargoRuntime,
    objects: [CargoObject; 2],
}
impl CargoPayload {
    fn validate(&self) -> Result<()> {
        let target = Platform {
            os: self.profile.os.clone(),
            arch: self.profile.arch.clone(),
            abi: "gnu".into(),
        }
        .target()?;
        if self.revision != 1
            || !hash_shape(&self.preparation_sha256)
            || !hash_shape(&self.declaration_sha256)
            || !image_shape(&self.profile.preparation_image)
            || !image_shape(&self.profile.execution_image)
            || self.profile.rust_revision != "1.97.1"
            || self.profile.target != target
            || !policy_shape(&self.profile.policy_revision)
        {
            return Err(invalid());
        }
        Profile::for_policy(&self.profile.policy_revision)?;
        for (index, object) in self.objects.iter().enumerate() {
            let (role, limit) = if index == 0 {
                ("record", RECORD_LIMIT)
            } else {
                ("archive", ARCHIVE_LIMIT)
            };
            if object.role != role
                || object.bytes == 0
                || object.bytes > limit
                || !hash_shape(&object.sha256)
                || object.name != format!("{}.blob", object.sha256)
            {
                return Err(invalid());
            }
        }
        if self.objects[0].name == self.objects[1].name {
            return Err(invalid());
        }
        Ok(())
    }
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NativeBundle {
    revision: u8,
    kind: String,
    language: String,
    platform: Platform,
    preparation_sha256: String,
    source_sha256: String,
    execution_image_digest: String,
    execution_policy_revision: String,
    payload: serde_json::Value,
    digest: String,
}
#[derive(Serialize)]
struct NativeContent<'a> {
    revision: u8,
    kind: &'a str,
    language: &'a str,
    platform: &'a Platform,
    preparation_sha256: &'a str,
    source_sha256: &'a str,
    execution_image_digest: &'a str,
    execution_policy_revision: &'a str,
    payload: &'a serde_json::Value,
}
impl NativeBundle {
    fn computed_root(&self) -> Result<String> {
        let content = NativeContent {
            revision: self.revision,
            kind: &self.kind,
            language: &self.language,
            platform: &self.platform,
            preparation_sha256: &self.preparation_sha256,
            source_sha256: &self.source_sha256,
            execution_image_digest: &self.execution_image_digest,
            execution_policy_revision: &self.execution_policy_revision,
            payload: &self.payload,
        };
        let mut value = serde_json::to_value(&content).map_err(|_| invalid())?;
        value.sort_all_objects();
        Ok(digest(&serde_json::to_vec(&value).map_err(|_| invalid())?))
    }
    fn validate(&self) -> Result<CargoPayload> {
        if self.revision != 2
            || self.kind != "cargo"
            || self.language != "rust"
            || !hash_shape(&self.digest)
            || self.computed_root()? != self.digest
        {
            return Err(invalid());
        }
        self.platform.matches_image()?;
        let payload: CargoPayload =
            serde_json::from_value(self.payload.clone()).map_err(|_| invalid())?;
        payload.validate()?;
        if payload.preparation_sha256 != self.preparation_sha256
            || payload.declaration_sha256 != self.source_sha256
            || payload.profile.execution_image != self.execution_image_digest
            || payload.profile.policy_revision != self.execution_policy_revision
            || payload.profile.os != self.platform.os
            || payload.profile.arch != self.platform.arch
        {
            return Err(invalid());
        }
        Ok(payload)
    }
    fn canonical(&self) -> Result<Vec<u8>> {
        let mut value = serde_json::to_value(self).map_err(|_| invalid())?;
        value.sort_all_objects();
        serde_json::to_vec(&value).map_err(|_| invalid())
    }
    fn from_bytes(bytes: &[u8]) -> Result<Self> {
        if bytes.len() as u64 > METADATA_LIMIT {
            return Err(invalid());
        }
        let bundle: Self = serde_json::from_slice(bytes).map_err(|_| invalid())?;
        // Native metadata is canonical. This also rejects duplicate payload fields lost by Value.
        if bundle.canonical()? != bytes {
            return Err(invalid());
        }
        bundle.validate()?;
        Ok(bundle)
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct NativeDependencies {
    kind: String,
    platform: Platform,
    preparation_sha256: String,
    source_sha256: String,
    dependencies_toml: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct NativeExecution {
    revision: u8,
    language: String,
    pub(crate) source: String,
    pub(crate) input: BTreeMap<String, serde_json::Value>,
    image_digest: String,
    policy_revision: String,
    timeout_seconds: u32,
    dependency_bundle_sha256: String,
    native_dependencies: NativeDependencies,
    #[serde(default)]
    workspace: Option<serde_json::Value>,
    #[serde(default)]
    platform_client: Option<serde_json::Value>,
}
impl NativeExecution {
    pub(crate) fn from_bytes(bytes: &[u8]) -> Result<Self> {
        if bytes.len() as u64 > REQUEST_LIMIT {
            return Err(invalid());
        }
        let value: serde_json::Value = serde_json::from_slice(bytes).map_err(|_| invalid())?;
        if code_prepared_extensions::base_revision(&value).map_err(|_| invalid())? != 3 {
            return Err(invalid());
        }
        let request: Self = serde_json::from_slice(bytes).map_err(|_| invalid())?;
        if !matches!(request.revision, 3..=5)
            || request.language != "rust"
            || request.source.trim().is_empty()
            || request.source.len() > 256 * 1024
            || request.source.contains('\0')
            || request.input.len() > 256
            || !(1..=3600).contains(&request.timeout_seconds)
            || request.native_dependencies.kind != "cargo"
            || !hash_shape(&request.dependency_bundle_sha256)
            || !hash_shape(&request.native_dependencies.preparation_sha256)
            || !hash_shape(&request.native_dependencies.source_sha256)
            || !image_shape(&request.image_digest)
            || !policy_shape(&request.policy_revision)
            || digest(request.native_dependencies.dependencies_toml.as_bytes())
                != request.native_dependencies.source_sha256
        {
            return Err(invalid());
        }
        request.native_dependencies.platform.matches_image()?;
        request
            .profile()?
            .validate_dependencies(&parse_dependencies(
                request.native_dependencies.dependencies_toml.as_bytes(),
            )?)?;
        Ok(request)
    }
    pub(crate) fn profile(&self) -> Result<Profile> {
        if let Some(value) = &self.platform_client {
            let broker: code_prepared_extensions::Broker =
                serde_json::from_value(value.clone()).map_err(|_| invalid())?;
            broker.validate().map_err(|_| invalid())?;
        }
        Profile::for_execution(&self.policy_revision, self.platform_client.is_some())
    }
    fn matches(&self, bundle: &NativeBundle) -> Result<CargoPayload> {
        self.profile()?;
        let payload = bundle.validate()?;
        if self.dependency_bundle_sha256 != bundle.digest
            || self.native_dependencies.platform != bundle.platform
            || self.native_dependencies.preparation_sha256 != bundle.preparation_sha256
            || self.native_dependencies.source_sha256 != bundle.source_sha256
            || self.image_digest != bundle.execution_image_digest
            || self.policy_revision != bundle.execution_policy_revision
        {
            return Err(invalid());
        }
        Ok(payload)
    }
    pub(crate) fn deadline(&self) -> Instant {
        Instant::now() + Duration::from_secs(self.timeout_seconds.into())
    }
}

fn exclusive_file(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path.parent().ok_or_else(invalid)?;
    let mut temporary = tempfile::NamedTempFile::new_in(parent).map_err(io_error)?;
    temporary.write_all(bytes).map_err(io_error)?;
    temporary.as_file().sync_all().map_err(io_error)?;
    temporary
        .persist_noclobber(path)
        .map_err(|error| io_error(error.error))?;
    Ok(())
}
fn verify_objects(
    objects: &Path,
    payload: &CargoPayload,
    deadline: Instant,
) -> Result<PreparationRecord> {
    payload.validate()?;
    if !std::fs::symlink_metadata(objects)
        .map_err(io_error)?
        .file_type()
        .is_dir()
    {
        return Err(invalid());
    }
    let record_bytes = read_regular(&objects.join(&payload.objects[0].name), RECORD_LIMIT)?;
    if record_bytes.len() as u64 != payload.objects[0].bytes
        || digest(&record_bytes) != payload.objects[0].sha256
    {
        return Err(invalid());
    }
    let record: PreparationRecord = serde_json::from_slice(&record_bytes).map_err(|_| invalid())?;
    if record.content.sha256 != payload.objects[1].sha256
        || record.content.compressed_bytes != payload.objects[1].bytes
    {
        return Err(invalid());
    }
    verify_archive(
        &objects.join(&payload.objects[1].name),
        &record.content,
        deadline,
    )?;
    Ok(record)
}
fn verify_config_and_lock(root: &Path, record: &PreparationRecord) -> Result<()> {
    let config = read_regular(&root.join(".cargo/config.toml"), 128 * 1024)?;
    check_vendor_config(&config)?;
    let lock = read_regular(&root.join("Cargo.lock"), 4 * 1024 * 1024)?;
    if digest(&config) != record.vendor_config_sha256 || digest(&lock) != record.lock_sha256 {
        return Err(invalid());
    }
    let value: toml::Value = toml::from_str(std::str::from_utf8(&lock).map_err(|_| invalid())?)
        .map_err(|_| invalid())?;
    if value.get("version").and_then(toml::Value::as_integer) != Some(4) {
        return Err(invalid());
    }
    let packages = value
        .get("package")
        .and_then(toml::Value::as_array)
        .ok_or_else(invalid)?;
    if packages.is_empty() || packages.len() > 16_384 {
        return Err(invalid());
    }
    for package in packages {
        let source = package.get("source");
        if let Some(source) = source {
            if source.as_str() != Some("registry+https://github.com/rust-lang/crates.io-index") {
                return Err(invalid());
            }
        } else if package.get("name").and_then(toml::Value::as_str) != Some("elitea-code-job")
            || package.get("version").and_then(toml::Value::as_str) != Some("0.1.0")
        {
            return Err(invalid());
        }
    }
    Ok(())
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct NativeMarker {
    revision: u8,
    status: String,
    source_sha256: String,
    preparer_image_digest: String,
    policy_revision: String,
    timeout_seconds: u32,
    started_unix_ms: i64,
    deadline_unix_ms: i64,
    #[serde(serialize_with = "serialize_bundle")]
    bundle: NativeBundle,
}
fn serialize_bundle<S: serde::Serializer>(
    bundle: &NativeBundle,
    serializer: S,
) -> std::result::Result<S::Ok, S::Error> {
    let mut value = serde_json::to_value(bundle).map_err(serde::ser::Error::custom)?;
    value.sort_all_objects();
    value.serialize(serializer)
}
fn now_ms() -> Result<i64> {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| invalid())?
            .as_millis(),
    )
    .map_err(|_| invalid())
}

/// The fixed retained mode acquires at most once, then holds the original runtime for publication.
pub(crate) async fn retain() -> Result<PreparationRecord> {
    retain_in(Path::new("/workspace"), &CargoImage::container()).await
}
async fn retain_in(workspace: &Path, image: &CargoImage) -> Result<PreparationRecord> {
    let monotonic = Instant::now();
    let started = now_ms()?;
    let job: NativePreparation = serde_json::from_slice(&read_regular(
        &workspace.join(".elitea-code.json"),
        REQUEST_LIMIT,
    )?)
    .map_err(|_| invalid())?;
    job.validate()?;
    let profile = job.profile()?;
    let mut deadline = monotonic + Duration::from_secs(job.timeout_seconds.into());
    let marker_path = workspace.join(".elitea-native-preparation.json");
    let release_path = workspace.join(".elitea-native-preparation-release");
    let directory = workspace.join("native-dependencies");
    let objects = directory.join("objects");
    let output = workspace.join("rust-prepared");
    let marker = match std::fs::symlink_metadata(&marker_path) {
        Ok(_) => {
            let marker: NativeMarker =
                serde_json::from_slice(&read_regular(&marker_path, 256 * 1024)?)
                    .map_err(|_| invalid())?;
            if marker.revision != 2
                || marker.status != "resolved"
                || marker.source_sha256 != digest(job.source.as_bytes())
                || marker.preparer_image_digest != job.preparer_image_digest
                || marker.policy_revision != job.policy_revision
                || marker.timeout_seconds != job.timeout_seconds
                || marker.started_unix_ms <= 0
                || marker.started_unix_ms > started
                || marker.deadline_unix_ms.checked_sub(marker.started_unix_ms)
                    != Some(i64::from(job.timeout_seconds) * 1000)
                || marker.deadline_unix_ms <= started
                || marker.bundle.preparation_sha256 != job.fingerprint()?
            {
                return Err(invalid());
            }
            deadline = std::cmp::min(
                deadline,
                monotonic + Duration::from_millis((marker.deadline_unix_ms - started) as u64),
            );
            let payload = marker.bundle.validate()?;
            if marker.bundle.execution_image_digest != job.execution_image_digest
                || marker.bundle.execution_policy_revision != job.execution_policy_revision
                || marker.bundle.platform != job.platform
                || payload.profile.preparation_image != job.preparer_image_digest
            {
                return Err(invalid());
            }
            marker
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            // Existing output or release means an interrupted or different acquisition. Never resolve again.
            if std::fs::symlink_metadata(&directory).is_ok()
                || std::fs::symlink_metadata(&output).is_ok()
                || std::fs::symlink_metadata(&release_path).is_ok()
            {
                return Err(invalid());
            }
            let record = prepare_profile(
                profile,
                job.source.as_bytes(),
                &output,
                deadline.saturating_duration_since(Instant::now()),
                image,
            )
            .await?;
            verify_record_profile(profile, job.source.as_bytes(), &record)?;
            let record_bytes = read_regular(&output.join("record.json"), RECORD_LIMIT)?;
            let record_hash = digest(&record_bytes);
            let payload = CargoPayload {
                revision: 1,
                preparation_sha256: job.fingerprint()?,
                declaration_sha256: digest(job.source.as_bytes()),
                profile: CargoRuntime {
                    preparation_image: job.preparer_image_digest.clone(),
                    execution_image: job.execution_image_digest.clone(),
                    rust_revision: "1.97.1".into(),
                    os: job.platform.os.clone(),
                    arch: job.platform.arch.clone(),
                    target: job.platform.target()?.into(),
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
            payload.validate()?;
            let mut bundle = NativeBundle {
                revision: 2,
                kind: "cargo".into(),
                language: "rust".into(),
                platform: job.platform.clone(),
                preparation_sha256: job.fingerprint()?,
                source_sha256: digest(job.source.as_bytes()),
                execution_image_digest: job.execution_image_digest.clone(),
                execution_policy_revision: job.execution_policy_revision.clone(),
                payload: serde_json::to_value(&payload).map_err(|_| invalid())?,
                digest: String::new(),
            };
            bundle.digest = bundle.computed_root()?;
            bundle.validate()?;
            std::fs::create_dir(&directory).map_err(io_error)?;
            std::fs::create_dir(&objects).map_err(io_error)?;
            // Move original component objects. Do not retain duplicate archive copies in the workspace.
            std::fs::rename(
                output.join("record.json"),
                objects.join(&payload.objects[0].name),
            )
            .map_err(io_error)?;
            std::fs::rename(
                output.join("content.tar.gz"),
                objects.join(&payload.objects[1].name),
            )
            .map_err(io_error)?;
            std::fs::remove_dir(&output).map_err(io_error)?;
            let metadata = bundle.canonical()?;
            if metadata.len() as u64 > METADATA_LIMIT {
                return Err(invalid());
            }
            exclusive_file(&directory.join(METADATA), &metadata)?;
            let marker = NativeMarker {
                revision: 2,
                status: "resolved".into(),
                source_sha256: digest(job.source.as_bytes()),
                preparer_image_digest: job.preparer_image_digest.clone(),
                policy_revision: job.policy_revision.clone(),
                timeout_seconds: job.timeout_seconds,
                started_unix_ms: started,
                deadline_unix_ms: started + i64::from(job.timeout_seconds) * 1000,
                bundle,
            };
            deadline_check(deadline)?;
            exclusive_file(
                &marker_path,
                &serde_json::to_vec(&marker).map_err(|_| invalid())?,
            )?;
            marker
        }
        Err(error) => return Err(io_error(error)),
    };
    if read_regular(&directory.join(METADATA), METADATA_LIMIT)? != marker.bundle.canonical()? {
        return Err(invalid());
    }
    let payload = marker.bundle.validate()?;
    let record = verify_objects(&objects, &payload, deadline)?;
    verify_record_profile(profile, job.source.as_bytes(), &record)?;
    loop {
        deadline_check(deadline)?;
        match std::fs::symlink_metadata(&release_path) {
            Ok(metadata) => {
                if !metadata.file_type().is_file() || metadata.len() != 0 {
                    return Err(invalid());
                }
                return Ok(record);
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(io_error(error)),
        }
        tokio::time::sleep_until(std::cmp::min(
            deadline,
            Instant::now() + Duration::from_millis(50),
        ))
        .await;
    }
}

/// The shared content helper stages metadata, calls this fixed command, then publishes metadata last.
pub(crate) fn hydrate_fixed() -> Result<PreparationRecord> {
    let workspace = Path::new("/workspace");
    let request = NativeExecution::from_bytes(&read_regular(
        &workspace.join(".elitea-code.json"),
        REQUEST_LIMIT,
    )?)?;
    let metadata = read_regular(
        &workspace.join("native-bundle").join(METADATA),
        METADATA_LIMIT,
    )?;
    let record = hydrate(workspace, &request, &metadata, request.deadline())?;
    let ready = workspace.join("native-bundle/elitea-native-ready-v2.json");
    match read_regular(&ready, METADATA_LIMIT) {
        Ok(existing) if existing == metadata => {}
        Ok(_) => return Err(invalid()),
        Err(_) if std::fs::symlink_metadata(&ready).is_err() => exclusive_file(&ready, &metadata)?,
        Err(error) => return Err(error),
    }
    Ok(record)
}
fn hydrate(
    workspace: &Path,
    request: &NativeExecution,
    metadata: &[u8],
    deadline: Instant,
) -> Result<PreparationRecord> {
    let bundle = NativeBundle::from_bytes(metadata)?;
    let payload = request.matches(&bundle)?;
    let objects = workspace.join("native-bundle/objects");
    let record = verify_objects(&objects, &payload, deadline)?;
    verify_record_profile(
        request.profile()?,
        request.native_dependencies.dependencies_toml.as_bytes(),
        &record,
    )?;
    let destination = workspace.join("rust-job");
    let profile = destination.join("profile");
    match std::fs::symlink_metadata(&destination) {
        Ok(meta) => {
            if !meta.file_type().is_dir()
                || read_regular(&destination.join(PROFILE_MARKER), METADATA_LIMIT)? != metadata
            {
                return Err(invalid());
            }
            verify_tree(&profile, &record.content, false, deadline)?;
            verify_config_and_lock(&profile, &record)?;
            return Ok(record);
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(io_error(error)),
    }
    // The shared group owner holds this fixed directory until every descendant exits.
    // It removes interrupted scratch after reaping. Published profile content stays outside it.
    let finalizing = workspace.join("native-bundle/.elitea-native-finalizing");
    if !std::fs::symlink_metadata(&finalizing)
        .map_err(io_error)?
        .file_type()
        .is_dir()
    {
        return Err(invalid());
    }
    let staging = tempfile::Builder::new()
        .prefix(".elitea-cargo-import-")
        .tempdir_in(&finalizing)
        .map_err(io_error)?;
    let extracted = staging.path().join("profile");
    std::fs::create_dir(&extracted).map_err(io_error)?;
    extract_archive(
        &objects.join(&payload.objects[1].name),
        &record.content,
        &extracted,
        deadline,
    )?;
    verify_tree(&extracted, &record.content, false, deadline)?;
    verify_config_and_lock(&extracted, &record)?;
    deadline_check(deadline)?;
    // The exclusive parent claim avoids replacing an existing root, including an empty root.
    std::fs::create_dir(&destination).map_err(io_error)?;
    let publish = (|| {
        std::fs::rename(&extracted, &profile).map_err(io_error)?;
        exclusive_file(&destination.join(PROFILE_MARKER), metadata)?;
        deadline_check(deadline)
    })();
    if publish.is_err() {
        let _ = std::fs::remove_dir_all(&destination);
    }
    publish?;
    Ok(record)
}

/// Require exact final metadata and the original imported profile before compiling.
pub(crate) fn execution_profile(
    workspace: &Path,
    request: &NativeExecution,
    allow_source: bool,
    deadline: Instant,
) -> Result<(PathBuf, PreparationRecord, String)> {
    let metadata = read_regular(
        &workspace.join("native-bundle").join(METADATA),
        METADATA_LIMIT,
    )?;
    if read_regular(
        &workspace.join("native-bundle/elitea-native-ready-v2.json"),
        METADATA_LIMIT,
    )? != metadata
    {
        return Err(invalid());
    }
    let bundle = NativeBundle::from_bytes(&metadata)?;
    let payload = request.matches(&bundle)?;
    let record = verify_objects(&workspace.join("native-bundle/objects"), &payload, deadline)?;
    verify_record_profile(
        request.profile()?,
        request.native_dependencies.dependencies_toml.as_bytes(),
        &record,
    )?;
    if read_regular(
        &workspace.join("rust-job").join(PROFILE_MARKER),
        METADATA_LIMIT,
    )? != metadata
    {
        return Err(invalid());
    }
    let root = workspace.join("rust-job/profile");
    verify_tree(&root, &record.content, allow_source, deadline)?;
    verify_config_and_lock(&root, &record)?;
    Ok((root, record, payload.profile.target))
}

#[cfg(all(test, target_os = "linux"))]
#[path = "rust_native_tests.rs"]
mod tests;
