//! Exact runner snapshot JSON. Content identity does not grant execution authority.
use ring::digest;
use serde::{Deserialize, Serialize};
use std::io;
pub(crate) const DESCRIPTOR_LIMIT: usize = 16 * 1024;
pub(crate) const EXECUTABLE_LIMIT: u64 = 32 * 1024 * 1024;
fn invalid() -> io::Error {
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
        Self(super::dependency_bundle::hex(
            digest::digest(&digest::SHA256, bytes).as_ref(),
        ))
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
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(domain);
    hash.update(&(bytes.len() as u64).to_be_bytes());
    hash.update(bytes);
    ContentSha256(super::dependency_bundle::hex(hash.finish().as_ref()))
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

#[derive(Serialize)]
struct CompiledIntent<'a> {
    revision: u8,
    purpose: &'a str,
    base_prepared_request_sha256: &'a ContentSha256,
    snapshot_key_sha256: &'a ContentSha256,
    #[serde(skip_serializing_if = "Option::is_none")]
    descriptor_sha256: Option<&'a ContentSha256>,
}
impl Control {
    pub(crate) fn bytes(&self, purpose: Purpose) -> io::Result<Vec<u8>> {
        self.validate(purpose)?;
        let bytes = serde_json::to_vec(self).map_err(|_| invalid())?;
        if bytes.len() > DESCRIPTOR_LIMIT {
            return Err(invalid());
        }
        Ok(bytes)
    }
    pub(crate) fn intent_digest(&self, purpose: Purpose) -> io::Result<[u8; 32]> {
        self.validate(purpose)?;
        let bytes = serde_json::to_vec(&CompiledIntent {
            revision: 1,
            purpose: match purpose {
                Purpose::Compile => "compile",
                Purpose::Execute => "execute",
            },
            base_prepared_request_sha256: &self.binding.base_prepared_request_sha256,
            snapshot_key_sha256: &self.snapshot_key_sha256,
            descriptor_sha256: self.descriptor_sha256.as_ref(),
        })
        .map_err(|_| invalid())?;
        domain_hash(b"elitea.sandbox.compiled-job.v1\0", &bytes).raw()
    }
    #[cfg(any(feature = "sandbox-supervisor", test))]
    pub(crate) fn from_bytes(bytes: &[u8], purpose: Purpose) -> io::Result<Self> {
        if bytes.len() > DESCRIPTOR_LIMIT {
            return Err(invalid());
        }
        let value: Self = serde_json::from_slice(bytes).map_err(|_| invalid())?;
        if value.bytes(purpose)? != bytes {
            return Err(invalid());
        }
        Ok(value)
    }
}
impl ContentSha256 {
    pub(crate) fn raw(&self) -> io::Result<[u8; 32]> {
        let mut out = [0; 32];
        for (index, pair) in self.0.as_bytes().chunks_exact(2).enumerate() {
            out[index] = u8::from_str_radix(std::str::from_utf8(pair).map_err(|_| invalid())?, 16)
                .map_err(|_| invalid())?;
        }
        Ok(out)
    }
}
impl Binding {
    pub(crate) fn validate_job(&self, job: &super::request::PreparedJob) -> io::Result<()> {
        self.validate()?;
        let bytes = job.to_transport().map_err(|_| invalid())?;
        let value: serde_json::Value = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        let source = value["source"].as_str().ok_or_else(invalid)?;
        if value["language"] != "rust"
            || self.base_prepared_request_sha256 != prepared_fingerprint(&bytes)
            || self.source_sha256 != ContentSha256::of(source.as_bytes())
            || value["image_digest"] != self.execution_image_digest
            || value["policy_revision"] != self.policy_revision
        {
            return Err(invalid());
        }
        Ok(())
    }
}
/// Main must verify this deployment attestation before issuing any snapshot grant.
/// There is no environment or model-selected profile fallback.
pub(crate) struct SnapshotProfile {
    template: Binding,
}
impl SnapshotProfile {
    #[allow(dead_code)] // Deployment assembly remains gated on verified profile producers.
    pub(crate) fn new(template: Binding) -> io::Result<Self> {
        template.validate()?;
        Ok(Self { template })
    }
    pub(crate) fn binding(
        &self,
        job: &super::request::PreparedJob,
        tenant: &str,
        project: i32,
    ) -> io::Result<Binding> {
        let bytes = job.to_transport().map_err(|_| invalid())?;
        let value: serde_json::Value = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
        let mut binding = self.template.clone();
        tenant.clone_into(&mut binding.tenant_id);
        binding.project_id = project;
        binding.base_prepared_request_sha256 = prepared_fingerprint(&bytes);
        binding.source_sha256 =
            ContentSha256::of(value["source"].as_str().ok_or_else(invalid)?.as_bytes());
        binding.validate_job(job)?;
        Ok(binding)
    }
}
/// A selected descriptor cannot become an ordinary compilation miss.
pub(crate) struct SelectedSnapshot {
    pub(crate) control: Control,
    pub(crate) descriptor: Descriptor,
    pub(crate) descriptor_bytes: Vec<u8>,
    pub(crate) recovery: bool,
}
impl SelectedSnapshot {
    pub(crate) fn select(binding: Binding, descriptor_bytes: Vec<u8>) -> io::Result<Self> {
        if descriptor_bytes.is_empty() || descriptor_bytes.len() > DESCRIPTOR_LIMIT {
            return Err(invalid());
        }
        let descriptor: Descriptor =
            serde_json::from_slice(&descriptor_bytes).map_err(|_| invalid())?;
        let control = Control {
            revision: 1,
            snapshot_key_sha256: binding.key()?,
            binding,
            descriptor_sha256: Some(ContentSha256::of(&descriptor_bytes)),
        };
        control.validate(Purpose::Execute)?;
        descriptor.validate(&control)?;
        if descriptor.bytes()? != descriptor_bytes {
            return Err(invalid());
        }
        Ok(Self {
            control,
            descriptor,
            descriptor_bytes,
            recovery: false,
        })
    }
    pub(crate) fn require_same_descriptor(&self, bytes: &[u8]) -> io::Result<()> {
        if self.descriptor_bytes != bytes {
            return Err(invalid());
        }
        Ok(())
    }
}

#[cfg(feature = "sandbox-supervisor")]
#[derive(Clone, Copy)]
pub enum SnapshotOperation {
    Control,
    Descriptor,
    Executable,
    Status,
    ReadDescriptor,
    ReadExecutable,
    Finalize,
    Release,
}
#[cfg(feature = "sandbox-supervisor")]
impl SnapshotOperation {
    pub(crate) fn command(self) -> &'static str {
        match self {
            Self::Control => "--compiled-control-write",
            Self::Descriptor | Self::Executable => "--compiled-artifact-write",
            Self::Status => "--compiled-artifact-status",
            Self::ReadDescriptor | Self::ReadExecutable => "--compiled-artifact-read",
            Self::Finalize => "--compiled-artifact-finalize",
            Self::Release => "--compiled-artifact-release",
        }
    }
    pub(crate) fn importing(self) -> bool {
        matches!(self, Self::Control | Self::Descriptor | Self::Executable)
    }
    pub(crate) fn header(
        self,
        control: &Control,
        bytes: u64,
        sha256: Option<&ContentSha256>,
        pod: Option<(&str, &str)>,
    ) -> io::Result<Vec<u8>> {
        let empty = matches!(self, Self::Status | Self::Finalize | Self::Release);
        if bytes > EXECUTABLE_LIMIT
            || (empty && (bytes != 0 || sha256.is_some()))
            || (!empty && sha256.is_none())
        {
            return Err(invalid());
        }
        let role = match self {
            Self::Control => "control",
            Self::Descriptor | Self::ReadDescriptor => "descriptor",
            Self::Executable | Self::ReadExecutable => "executable",
            Self::Status => "status",
            Self::Finalize => "finalize",
            Self::Release => "release",
        };
        let mut value = serde_json::json!({"revision":1,"snapshot_key_sha256":control.snapshot_key_sha256,"role":role,"bytes":bytes});
        if let Some(hash) = sha256 {
            value["sha256"] = serde_json::to_value(hash).map_err(|_| invalid())?;
        }
        if let Some((uid, request)) = pod {
            value["pod_uid"] = uid.into();
            value["request_digest"] = request.into();
        }
        let mut header = serde_json::to_vec(&value).map_err(|_| invalid())?;
        header.push(b'\n');
        if header.len() > 4096 {
            return Err(invalid());
        }
        Ok(header)
    }
}
#[cfg(feature = "sandbox-supervisor")]
impl super::request::PreparedJob {
    pub(crate) fn compiled_manifest(
        &self,
        purpose: Purpose,
    ) -> io::Result<adk_sandbox::workspace::Manifest> {
        let mut manifest = self.manifest().map_err(|_| invalid())?;
        let value: serde_json::Value =
            serde_json::from_slice(&self.to_transport().map_err(|_| invalid())?)
                .map_err(|_| invalid())?;
        for entry in &mut manifest.entries {
            if let adk_sandbox::workspace::ManifestEntry::File { path, content } = entry
                && path == ".elitea-job.json"
            {
                *content = serde_json::to_vec(&serde_json::json!({"argv":["/usr/local/bin/elitea-code-rust", match purpose {Purpose::Compile=>"--compile-snapshot",Purpose::Execute=>"--execute-snapshot"}],"timeout_seconds":value["timeout_seconds"]})).map_err(|_| invalid())?;
            }
        }
        Ok(manifest)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    const COMPILE: &[u8] =
        include_bytes!("../../tests/fixtures/compiled-snapshot-v1/compile-control.json");
    const EXECUTE: &[u8] =
        include_bytes!("../../tests/fixtures/compiled-snapshot-v1/execute-control.json");
    const DESCRIPTOR: &[u8] =
        include_bytes!("../../tests/fixtures/compiled-snapshot-v1/descriptor.json");
    fn expectation() -> serde_json::Value {
        serde_json::from_slice(include_bytes!(
            "../../tests/fixtures/compiled-snapshot-v1/expected.json"
        ))
        .unwrap()
    }
    #[test]
    fn runner_fixture_bytes_and_hash_domains_match_exactly() {
        let compile = Control::from_bytes(COMPILE, Purpose::Compile).unwrap();
        let execute = Control::from_bytes(EXECUTE, Purpose::Execute).unwrap();
        let expected = expectation();
        assert_eq!(
            compile.binding.key().unwrap().as_str(),
            expected["snapshot_key_sha256"].as_str().unwrap()
        );
        assert_eq!(
            ContentSha256::of(COMPILE).as_str(),
            expected["compile_control_sha256"].as_str().unwrap()
        );
        assert_eq!(
            ContentSha256::of(EXECUTE).as_str(),
            expected["execute_control_sha256"].as_str().unwrap()
        );
        assert_eq!(
            ContentSha256::of(DESCRIPTOR).as_str(),
            expected["descriptor_sha256"].as_str().unwrap()
        );
        let selected =
            SelectedSnapshot::select(compile.binding.clone(), DESCRIPTOR.to_vec()).unwrap();
        assert_eq!(selected.control.bytes(Purpose::Execute).unwrap(), EXECUTE);
        assert_ne!(
            compile.intent_digest(Purpose::Compile).unwrap(),
            execute.intent_digest(Purpose::Execute).unwrap()
        );
    }
    #[test]
    fn role_null_unknown_and_noncanonical_controls_fail() {
        assert!(Control::from_bytes(COMPILE, Purpose::Execute).is_err());
        assert!(Control::from_bytes(EXECUTE, Purpose::Compile).is_err());
        let mut value: serde_json::Value = serde_json::from_slice(COMPILE).unwrap();
        value["descriptor_sha256"] = serde_json::Value::Null;
        assert!(
            Control::from_bytes(&serde_json::to_vec(&value).unwrap(), Purpose::Compile).is_err()
        );
        value.as_object_mut().unwrap().remove("descriptor_sha256");
        value["command"] = "cargo".into();
        assert!(
            Control::from_bytes(&serde_json::to_vec(&value).unwrap(), Purpose::Compile).is_err()
        );
        let mut padded = COMPILE.to_vec();
        padded.push(b'\n');
        assert!(Control::from_bytes(&padded, Purpose::Compile).is_err());
    }
    #[test]
    fn immutable_descriptor_selection_rejects_change_absence_and_excess() {
        let control = Control::from_bytes(COMPILE, Purpose::Compile).unwrap();
        let selected =
            SelectedSnapshot::select(control.binding.clone(), DESCRIPTOR.to_vec()).unwrap();
        assert!(selected.require_same_descriptor(b"{}").is_err());
        assert!(SelectedSnapshot::select(control.binding.clone(), Vec::new()).is_err());
        let mut value: serde_json::Value = serde_json::from_slice(DESCRIPTOR).unwrap();
        value["executable_bytes"] = serde_json::json!(EXECUTABLE_LIMIT + 1);
        assert!(
            SelectedSnapshot::select(control.binding.clone(), serde_json::to_vec(&value).unwrap())
                .is_err()
        );
        value["executable_bytes"] = 0.into();
        assert!(
            SelectedSnapshot::select(control.binding, serde_json::to_vec(&value).unwrap()).is_err()
        );
    }
    #[test]
    fn exact_input_source_image_and_scope_change_snapshot_identity() {
        use crate::sandbox::request::{Language, PreparedJob};
        use std::collections::BTreeMap;
        let template = Control::from_bytes(COMPILE, Purpose::Compile)
            .unwrap()
            .binding;
        let profile = SnapshotProfile::new(template.clone()).unwrap();
        let job = PreparedJob::new(
            Language::Rust,
            "pub fn run() {}".into(),
            BTreeMap::new(),
            template.execution_image_digest.clone(),
            template.policy_revision.clone(),
            30,
        )
        .unwrap();
        let binding = profile.binding(&job, "tenant", 2).unwrap();
        assert!(binding.validate_job(&job).is_ok());
        let mut state = BTreeMap::new();
        state.insert("input".into(), serde_json::json!(2));
        let changed = PreparedJob::new(
            Language::Rust,
            "pub fn run() {}".into(),
            state,
            template.execution_image_digest.clone(),
            template.policy_revision.clone(),
            30,
        )
        .unwrap();
        assert!(binding.validate_job(&changed).is_err());
        assert_ne!(
            binding.key().unwrap(),
            profile
                .binding(&changed, "tenant", 2)
                .unwrap()
                .key()
                .unwrap()
        );
        assert_ne!(
            binding.key().unwrap(),
            profile.binding(&job, "tenant", 3).unwrap().key().unwrap()
        );
        let source = PreparedJob::new(
            Language::Rust,
            "pub fn run() {panic!()}".into(),
            BTreeMap::new(),
            template.execution_image_digest.clone(),
            template.policy_revision.clone(),
            30,
        )
        .unwrap();
        assert!(binding.validate_job(&source).is_err());
        let image = PreparedJob::new(
            Language::Rust,
            "pub fn run() {}".into(),
            BTreeMap::new(),
            format!("sha256:{}", "b".repeat(64)),
            template.policy_revision,
            30,
        )
        .unwrap();
        assert!(profile.binding(&image, "tenant", 2).is_err());
    }
    #[cfg(feature = "sandbox-supervisor")]
    #[test]
    fn cached_manifest_never_selects_cargo_or_normal_execution_adapter() {
        use crate::sandbox::request::{Language, PreparedJob};
        let control = Control::from_bytes(COMPILE, Purpose::Compile).unwrap();
        let job = PreparedJob::new(
            Language::Rust,
            "pub fn run() {}".into(),
            std::collections::BTreeMap::new(),
            control.binding.execution_image_digest,
            control.binding.policy_revision,
            30,
        )
        .unwrap();
        let manifest = job.compiled_manifest(Purpose::Execute).unwrap();
        let adk_sandbox::workspace::ManifestEntry::File { content, .. } = &manifest.entries[1]
        else {
            panic!()
        };
        let value: serde_json::Value = serde_json::from_slice(content).unwrap();
        assert_eq!(
            value["argv"],
            serde_json::json!(["/usr/local/bin/elitea-code-rust", "--execute-snapshot"])
        );
        assert_eq!(manifest.entries.len(), 2);
    }
    #[cfg(feature = "sandbox-supervisor")]
    #[test]
    fn fixed_transfer_roles_keep_bounds_uid_and_zero_byte_control_operations() {
        let control = Control::from_bytes(COMPILE, Purpose::Compile).unwrap();
        let bytes = SnapshotOperation::Status
            .header(
                &control,
                0,
                None,
                Some(("original-uid", "original-request")),
            )
            .unwrap();
        let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(value["role"], "status");
        assert_eq!(value["pod_uid"], "original-uid");
        assert_eq!(value["request_digest"], "original-request");
        assert_eq!(value["bytes"], 0);
        assert!(value.get("sha256").is_none());
        assert!(
            SnapshotOperation::Status
                .header(&control, 1, None, None)
                .is_err()
        );
        assert!(
            SnapshotOperation::Executable
                .header(&control, 1, None, None)
                .is_err()
        );
        assert!(
            SnapshotOperation::Executable
                .header(
                    &control,
                    EXECUTABLE_LIMIT + 1,
                    Some(&ContentSha256::of(b"x")),
                    None
                )
                .is_err()
        );
    }
}
