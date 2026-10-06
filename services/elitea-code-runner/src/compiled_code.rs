//! Strict, bounded Rust output snapshot contract. No storage or grant authority.
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
use std::{
    fs,
    io::{self, Read, Write},
    path::Path,
};

pub(crate) const DESCRIPTOR_LIMIT: usize = 16 * 1024;
pub(crate) const EXECUTABLE_LIMIT: u64 = 32 * 1024 * 1024;
pub(crate) const DIRECTORY: &str = "compiled-snapshot";
pub(crate) const EXECUTABLE: &str = "elitea-code-job";
pub(crate) const DESCRIPTOR: &str = "descriptor.json";
pub(crate) const CONTROL: &str = ".elitea-compiled-control.json";
pub(crate) const CAPTURED: &str = ".elitea-compiled-captured";
pub(crate) const READY: &str = ".elitea-compiled-ready";
pub(crate) const RELEASE: &str = ".elitea-compiled-release";

pub(crate) fn invalid() -> io::Error {
    io::Error::other("invalid Rust snapshot contract")
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(transparent)]
pub(crate) struct ContentSha256(String);
impl ContentSha256 {
    pub(crate) fn parse(value: String) -> io::Result<Self> {
        if value.len() != 64
            || !value
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return Err(invalid());
        }
        Ok(Self(value))
    }
    pub(crate) fn of(bytes: &[u8]) -> Self {
        Self(format!("{:x}", Sha256::digest(bytes)))
    }
    pub(crate) fn as_str(&self) -> &str {
        &self.0
    }
}
impl<'de> Deserialize<'de> for ContentSha256 {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        Self::parse(String::deserialize(deserializer)?).map_err(serde::de::Error::custom)
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Binding {
    pub(crate) revision: u8,
    pub(crate) reuse_policy: String,
    pub(crate) tenant_id: String,
    pub(crate) project_id: i32,
    pub(crate) base_prepared_request_sha256: ContentSha256,
    pub(crate) source_sha256: ContentSha256,
    pub(crate) compilation_image_digest: String,
    pub(crate) execution_image_digest: String,
    pub(crate) platform: String,
    pub(crate) target: String,
    pub(crate) policy_revision: String,
    pub(crate) cargo_manifest_sha256: ContentSha256,
    pub(crate) cargo_lock_sha256: ContentSha256,
    pub(crate) cargo_config_sha256: ContentSha256,
    pub(crate) vendor_sha256: ContentSha256,
    pub(crate) toolchain_sha256: ContentSha256,
    pub(crate) adapter_sha256: ContentSha256,
    pub(crate) wrapper_sha256: ContentSha256,
    pub(crate) compiler_flags_sha256: ContentSha256,
}
impl Binding {
    pub(crate) fn validate(&self) -> io::Result<()> {
        let image = self
            .compilation_image_digest
            .strip_prefix("sha256:")
            .ok_or_else(invalid)?;
        ContentSha256::parse(image.to_owned())?;
        let platform_target = matches!(
            (self.platform.as_str(), self.target.as_str()),
            ("linux/arm64/gnu", "aarch64-unknown-linux-gnu")
                | ("linux/amd64/gnu", "x86_64-unknown-linux-gnu")
        );
        if self.revision != 1
            || self.reuse_policy != "snapshot_v1"
            || !platform_target
            || self.compilation_image_digest != self.execution_image_digest
            || self.project_id <= 0
            || self.tenant_id.is_empty()
            || self.tenant_id.len() > 128
            || !self
                .tenant_id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-'))
            || self.policy_revision.is_empty()
            || self.policy_revision.len() > 128
            || !self
                .policy_revision
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-' | b'.'))
        {
            return Err(invalid());
        }
        Ok(())
    }
    pub(crate) fn key(&self) -> io::Result<ContentSha256> {
        self.validate()?;
        let bytes = serde_json::to_vec(self).map_err(|_| invalid())?;
        Ok(domain_hash(
            b"elitea.rust.compiled-snapshot-key.v1\0",
            &bytes,
        ))
    }
}
pub(crate) fn domain_hash(domain: &[u8], bytes: &[u8]) -> ContentSha256 {
    let mut hash = Sha256::new();
    hash.update(domain);
    hash.update((bytes.len() as u64).to_be_bytes());
    hash.update(bytes);
    ContentSha256(format!("{:x}", hash.finalize()))
}
#[allow(dead_code)] // Shared with PID 1 contract validation; used by the Rust adapter.
pub(crate) fn prepared_fingerprint(bytes: &[u8]) -> ContentSha256 {
    domain_hash(b"elitea.sandbox.prepared-job.v1\0", bytes)
}

pub(crate) fn non_null_sha256<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<ContentSha256>, D::Error> {
    ContentSha256::deserialize(deserializer).map(Some)
}
#[allow(dead_code)] // Fixed transfer header used by the PID 1 binary.
pub(crate) fn non_null_string<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<String>, D::Error> {
    String::deserialize(deserializer).map(Some)
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Control {
    pub(crate) revision: u8,
    pub(crate) binding: Binding,
    pub(crate) snapshot_key_sha256: ContentSha256,
    // Absent for compilation; required for cached execution.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    #[serde(deserialize_with = "non_null_sha256")]
    pub(crate) descriptor_sha256: Option<ContentSha256>,
}
impl Control {
    pub(crate) fn validate(&self, purpose: Purpose) -> io::Result<()> {
        if self.revision != 1
            || self.binding.key()? != self.snapshot_key_sha256
            || (self.descriptor_sha256.is_some() != (purpose == Purpose::Execute))
        {
            return Err(invalid());
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Descriptor {
    pub(crate) revision: u8,
    pub(crate) binding: Binding,
    pub(crate) snapshot_key_sha256: ContentSha256,
    pub(crate) executable_sha256: ContentSha256,
    pub(crate) executable_bytes: u64,
}
impl Descriptor {
    pub(crate) fn validate(&self, control: &Control) -> io::Result<()> {
        if self.revision != 1
            || self.executable_bytes == 0
            || self.executable_bytes > EXECUTABLE_LIMIT
            || self.binding != control.binding
            || self.snapshot_key_sha256 != control.snapshot_key_sha256
            || self.binding.key()? != self.snapshot_key_sha256
        {
            return Err(invalid());
        }
        Ok(())
    }
    #[allow(dead_code)] // Descriptor publication is owned by the Rust adapter.
    pub(crate) fn bytes(&self) -> io::Result<Vec<u8>> {
        let bytes = serde_json::to_vec(self).map_err(|_| invalid())?;
        if bytes.len() > DESCRIPTOR_LIMIT {
            return Err(invalid());
        }
        Ok(bytes)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Purpose {
    Compile,
    Execute,
}
#[cfg(any(test, target_os = "linux"))]
fn launch_value(bytes: &[u8], name: &str) -> io::Result<Option<String>> {
    if bytes.len() > 64 * 1024 || (!bytes.is_empty() && !bytes.ends_with(&[0])) {
        return Err(invalid());
    }
    let prefix = format!("{name}=");
    let mut found = None;
    for entry in bytes.split(|byte| *byte == 0) {
        if let Some(value) = entry.strip_prefix(prefix.as_bytes()) {
            if found.is_some() || value.len() > 4096 {
                return Err(invalid());
            }
            found = Some(
                std::str::from_utf8(value)
                    .map_err(|_| invalid())?
                    .to_owned(),
            );
        }
    }
    Ok(found)
}
pub(crate) fn trusted_launch_value(name: &str) -> io::Result<Option<String>> {
    // A user child can choose its own env. Only PID 1's supervisor-issued launch
    // configuration fixes purpose. The Linux sandbox must deny ptrace,
    // process_vm_writev and nested PID namespaces. No caller env fallback.
    #[cfg(target_os = "linux")]
    {
        let mut bytes = Vec::new();
        fs::File::open("/proc/1/environ")?
            .take(64 * 1024 + 1)
            .read_to_end(&mut bytes)?;
        launch_value(&bytes, name)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = name;
        Err(invalid())
    }
}
fn purpose_from_launch_values(enabled: Option<&str>, purpose: Option<&str>) -> io::Result<Purpose> {
    if enabled != Some("1") {
        return Err(invalid());
    }
    match purpose {
        Some("compile") => Ok(Purpose::Compile),
        Some("execute") => Ok(Purpose::Execute),
        _ => Err(invalid()),
    }
}
pub(crate) fn enabled_purpose() -> io::Result<Purpose> {
    purpose_from_launch_values(
        trusted_launch_value("ELITEA_COMPILED_CODE_SNAPSHOTS")?.as_deref(),
        trusted_launch_value("ELITEA_COMPILED_CODE_JOB_PURPOSE")?.as_deref(),
    )
}
pub(crate) fn regular_directory(path: &Path) -> io::Result<()> {
    if !fs::symlink_metadata(path)?.is_dir() {
        return Err(invalid());
    }
    Ok(())
}
pub(crate) fn regular_file(path: &Path, limit: u64) -> io::Result<fs::File> {
    let before = fs::symlink_metadata(path)?;
    if !before.is_file() || before.nlink() != 1 || before.len() > limit {
        return Err(invalid());
    }
    #[cfg(target_os = "linux")]
    let flags = 0x20000 | 0x800; // O_NOFOLLOW | O_NONBLOCK
    #[cfg(target_os = "macos")]
    let flags = 0x100 | 0x4;
    let file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(flags)
        .open(path)?;
    let actual = file.metadata()?;
    if !actual.is_file()
        || actual.dev() != before.dev()
        || actual.ino() != before.ino()
        || actual.len() != before.len()
        || actual.nlink() != 1
    {
        return Err(invalid());
    }
    Ok(file)
}
pub(crate) fn read_regular(path: &Path, limit: usize) -> io::Result<Vec<u8>> {
    let mut bytes = Vec::new();
    regular_file(path, limit as u64)?
        .take(limit as u64 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(invalid());
    }
    Ok(bytes)
}
pub(crate) fn hash_regular(path: &Path, limit: u64) -> io::Result<(ContentSha256, u64)> {
    let mut file = regular_file(path, limit)?;
    let expected = file.metadata()?.len();
    let mut bytes = 0u64;
    let mut hash = Sha256::new();
    let mut chunk = [0u8; 64 * 1024];
    loop {
        let count = file.read(&mut chunk)?;
        if count == 0 {
            break;
        }
        bytes = bytes.checked_add(count as u64).ok_or_else(invalid)?;
        if bytes > limit {
            return Err(invalid());
        }
        hash.update(&chunk[..count]);
    }
    if bytes != expected {
        return Err(invalid());
    }
    Ok((ContentSha256(format!("{:x}", hash.finalize())), bytes))
}
pub(crate) fn immutable_file(path: &Path, bytes: &[u8], executable: bool) -> io::Result<()> {
    let parent = path.parent().ok_or_else(invalid)?;
    regular_directory(parent)?;
    let mut temporary = tempfile::NamedTempFile::new_in(parent)?;
    temporary.write_all(bytes)?;
    temporary
        .as_file()
        .set_permissions(fs::Permissions::from_mode(if executable {
            0o500
        } else {
            0o400
        }))?;
    temporary.as_file().sync_all()?;
    match temporary.persist_noclobber(path) {
        Ok(_) => {}
        Err(error) if error.error.kind() == io::ErrorKind::AlreadyExists => {
            if read_regular(path, bytes.len())? != bytes {
                return Err(invalid());
            }
        }
        Err(error) => return Err(error.error),
    }
    fs::File::open(parent)?.sync_all()
}
pub(crate) fn load_control(root: &Path, purpose: Purpose) -> io::Result<Control> {
    let bytes = read_regular(&root.join(CONTROL), DESCRIPTOR_LIMIT)?;
    let pinned = ContentSha256::parse(
        trusted_launch_value("ELITEA_COMPILED_CODE_CONTROL_SHA256")?.ok_or_else(invalid)?,
    )?;
    if ContentSha256::of(&bytes) != pinned {
        return Err(invalid());
    }
    let control: Control = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
    control.validate(purpose)?;
    if serde_json::to_vec(&control).map_err(|_| invalid())? != bytes {
        return Err(invalid());
    }
    Ok(control)
}
pub(crate) fn load_descriptor(root: &Path, control: &Control) -> io::Result<Descriptor> {
    let base = root.join(DIRECTORY);
    regular_directory(&base)?;
    let bytes = read_regular(&base.join(DESCRIPTOR), DESCRIPTOR_LIMIT)?;
    if control
        .descriptor_sha256
        .as_ref()
        .is_some_and(|sha| *sha != ContentSha256::of(&bytes))
    {
        return Err(invalid());
    }
    let descriptor: Descriptor = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
    descriptor.validate(control)?;
    if descriptor.bytes()? != bytes {
        return Err(invalid());
    }
    let (hash, size) = hash_regular(&base.join(EXECUTABLE), EXECUTABLE_LIMIT)?;
    if hash != descriptor.executable_sha256 || size != descriptor.executable_bytes {
        return Err(invalid());
    }
    Ok(descriptor)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn authoritative_v1_fixtures_match_exact_key_and_descriptor_encoding() {
        let compile_bytes = include_bytes!("../fixtures/compiled-snapshot-v1/compile-control.json");
        let execute_bytes = include_bytes!("../fixtures/compiled-snapshot-v1/execute-control.json");
        let descriptor_bytes = include_bytes!("../fixtures/compiled-snapshot-v1/descriptor.json");
        let expected: serde_json::Value = serde_json::from_slice(include_bytes!(
            "../fixtures/compiled-snapshot-v1/expected.json"
        ))
        .unwrap();
        let compile: Control = serde_json::from_slice(compile_bytes).unwrap();
        compile.validate(Purpose::Compile).unwrap();
        let execute: Control = serde_json::from_slice(execute_bytes).unwrap();
        execute.validate(Purpose::Execute).unwrap();
        let descriptor: Descriptor = serde_json::from_slice(descriptor_bytes).unwrap();
        descriptor.validate(&execute).unwrap();
        assert_eq!(
            compile.binding.key().unwrap().as_str(),
            expected["snapshot_key_sha256"].as_str().unwrap()
        );
        assert_eq!(descriptor.bytes().unwrap(), descriptor_bytes.as_slice());
        assert_eq!(
            ContentSha256::of(descriptor_bytes).as_str(),
            expected["descriptor_sha256"].as_str().unwrap()
        );
        assert_eq!(
            serde_json::to_vec(&compile).unwrap(),
            compile_bytes.as_slice()
        );
        assert_eq!(
            serde_json::to_vec(&execute).unwrap(),
            execute_bytes.as_slice()
        );
    }
    pub(super) fn binding() -> Binding {
        let sha = ContentSha256::of(b"identity");
        Binding {
            revision: 1,
            reuse_policy: "snapshot_v1".into(),
            tenant_id: "tenant".into(),
            project_id: 1,
            base_prepared_request_sha256: sha.clone(),
            source_sha256: sha.clone(),
            compilation_image_digest: format!("sha256:{}", sha.as_str()),
            execution_image_digest: format!("sha256:{}", sha.as_str()),
            platform: "linux/arm64/gnu".into(),
            target: "aarch64-unknown-linux-gnu".into(),
            policy_revision: "p1".into(),
            cargo_manifest_sha256: sha.clone(),
            cargo_lock_sha256: sha.clone(),
            cargo_config_sha256: sha.clone(),
            vendor_sha256: sha.clone(),
            toolchain_sha256: sha.clone(),
            adapter_sha256: sha.clone(),
            wrapper_sha256: sha.clone(),
            compiler_flags_sha256: sha,
        }
    }
    #[test]
    fn snapshots_are_default_off_and_roles_do_not_substitute() {
        for enabled in [None, Some("0"), Some("true")] {
            for purpose in [None, Some("compile"), Some("execute")] {
                assert!(purpose_from_launch_values(enabled, purpose).is_err());
            }
        }
        assert!(purpose_from_launch_values(Some("1"), None).is_err());
        assert!(purpose_from_launch_values(Some("1"), Some("publish")).is_err());
        assert_eq!(
            purpose_from_launch_values(Some("1"), Some("compile")).unwrap(),
            Purpose::Compile
        );
        assert_eq!(
            purpose_from_launch_values(Some("1"), Some("execute")).unwrap(),
            Purpose::Execute
        );
    }
    #[test]
    fn launch_configuration_is_bounded_unique_and_has_no_child_env_fallback() {
        let bytes = b"OTHER=unrelated\0ELITEA_COMPILED_CODE_JOB_PURPOSE=execute\0";
        assert_eq!(
            launch_value(bytes, "ELITEA_COMPILED_CODE_JOB_PURPOSE")
                .unwrap()
                .as_deref(),
            Some("execute")
        );
        assert_eq!(
            launch_value(bytes, "ELITEA_COMPILED_CODE_SNAPSHOTS").unwrap(),
            None
        );
        assert!(launch_value(b"X=one\0X=two\0", "X").is_err());
        assert!(launch_value(b"X=unterminated", "X").is_err());
        assert!(launch_value(&vec![0; 64 * 1024 + 1], "X").is_err());
    }
    #[test]
    fn snapshot_key_binds_every_field_and_exact_prepared_bytes() {
        let initial = binding();
        let key = initial.key().unwrap();
        let encoded = serde_json::to_value(&initial).unwrap();
        for field in encoded.as_object().unwrap().keys() {
            let mut changed = encoded.clone();
            changed[field] = match field.as_str() {
                "revision" => serde_json::json!(2),
                "project_id" => serde_json::json!(2),
                "compilation_image_digest" | "execution_image_digest" => {
                    serde_json::json!(format!("sha256:{}", "a".repeat(64)))
                }
                key if key.ends_with("sha256") => serde_json::json!("a".repeat(64)),
                _ => serde_json::json!("changed"),
            };
            let changed: Binding = serde_json::from_value(changed).unwrap();
            assert!(
                changed.key().is_err() || changed.key().unwrap() != key,
                "{field}"
            );
        }
        for bytes in [
            b"{\"source\":\"a\",\"input\":{},\"timeout_seconds\":1}".as_slice(),
            b"{\"source\":\"b\",\"input\":{},\"timeout_seconds\":1}",
            b"{\"source\":\"a\",\"input\":{\"x\":1},\"timeout_seconds\":1}",
            b"{\"source\":\"a\",\"input\":{},\"timeout_seconds\":2}",
        ] {
            let mut changed = initial.clone();
            changed.base_prepared_request_sha256 = prepared_fingerprint(bytes);
            assert_ne!(changed.key().unwrap(), key);
        }
    }
    #[test]
    fn descriptors_reject_unknown_duplicate_identity_and_size() {
        let binding = binding();
        let control = Control {
            revision: 1,
            snapshot_key_sha256: binding.key().unwrap(),
            binding: binding.clone(),
            descriptor_sha256: None,
        };
        let mut descriptor = Descriptor {
            revision: 1,
            binding,
            snapshot_key_sha256: control.snapshot_key_sha256.clone(),
            executable_sha256: ContentSha256::of(b"binary"),
            executable_bytes: 6,
        };
        descriptor.validate(&control).unwrap();
        for count in [0, EXECUTABLE_LIMIT + 1] {
            descriptor.executable_bytes = count;
            assert!(descriptor.validate(&control).is_err());
        }
        assert!(serde_json::from_slice::<ContentSha256>(b"\"ABC\"").is_err());
        assert!(serde_json::from_slice::<Control>(b"{\"revision\":1,\"revision\":1}").is_err());
        let mut value = serde_json::to_value(&control).unwrap();
        value["argv"] = serde_json::json!(["malicious"]);
        assert!(serde_json::from_value::<Control>(value).is_err());
    }
    #[test]
    fn regular_immutable_files_reject_substitution_and_symlinks() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("fixed");
        immutable_file(&path, b"abc", false).unwrap();
        immutable_file(&path, b"abc", false).unwrap();
        assert!(immutable_file(&path, b"other", false).is_err());
        let link = temp.path().join("link");
        std::os::unix::fs::symlink(&path, &link).unwrap();
        assert!(read_regular(&link, 3).is_err());
        fs::hard_link(&path, temp.path().join("hard")).unwrap();
        assert!(read_regular(&path, 3).is_err());
    }
}
