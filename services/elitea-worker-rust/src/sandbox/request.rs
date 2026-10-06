//! Content identity for an admitted sandbox job. This is not authorization.
use std::collections::BTreeMap;

use super::dependency_bundle::{hex, valid_digest};
use super::native_bundle::{NativeKind, NativePlatform};
use ring::digest;
use serde::{Deserialize, Serialize};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct NativeDependencies {
    pub kind: NativeKind,
    pub platform: NativePlatform,
    pub preparation_sha256: String,
    pub source_sha256: String,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        deserialize_with = "dependency_digest"
    )]
    pub dependencies_toml: Option<String>,
}

#[cfg(feature = "sandbox-supervisor")]
use super::ledger::{JobScope, LedgerError};

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Language {
    Python,
    JavaScript,
    TypeScript,
    Rust,
}

/// Deployment-selected runtime, never an image or command selected by code.
/// No Debug: the input and source may contain private user data.
#[derive(Serialize)]
pub struct PreparedJob {
    revision: u8,
    language: Language,
    source: String,
    input: BTreeMap<String, serde_json::Value>,
    image_digest: String,
    policy_revision: String,
    timeout_seconds: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    dependency_bundle_sha256: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    native_dependencies: Option<NativeDependencies>,
    #[serde(skip_serializing_if = "Option::is_none")]
    workspace: Option<super::workspace::WorkspaceBinding>,
    #[serde(skip_serializing_if = "Option::is_none")]
    platform_client: Option<super::platform_client_binding::PlatformClientBinding>,
}

#[derive(Debug, thiserror::Error)]
#[error("sandbox request has invalid code, input, runtime identity, or limits")]
pub struct InvalidRequest;

impl PreparedJob {
    /// Check the independently signed whole-Code intent against this admitted request.
    pub(crate) fn matches_code_binding(
        &self,
        binding: &super::code_recovery::WholeCodeBinding,
    ) -> bool {
        let language = serde_json::to_value(self.language).ok();
        let input = serde_json::to_vec(&self.input).ok();
        binding.valid()
            && language.as_ref().and_then(serde_json::Value::as_str)
                == Some(binding.language.as_str())
            && binding.source_sha256 == super::code_recovery::sha256(self.source.as_bytes())
            && input
                .is_some_and(|input| binding.input_sha256 == super::code_recovery::sha256(&input))
            && self.to_transport().is_ok_and(|wire| {
                binding.prepared_job_sha256 == super::code_recovery::sha256(&wire)
            })
    }
    /// Construct only after runtime selection and invocation authorization.
    /// Wrappers and baseline dependencies belong to the runtime image/policy revision.
    ///
    /// # Errors
    /// Returns `InvalidRequest` if a field or serialized request exceeds its bound.
    pub fn new(
        language: Language,
        source: String,
        mut input: BTreeMap<String, serde_json::Value>,
        image_digest: String,
        policy_revision: String,
        timeout_seconds: u32,
    ) -> Result<Self, InvalidRequest> {
        let digest = image_digest.strip_prefix("sha256:").ok_or(InvalidRequest)?;
        if source.trim().is_empty()
            || source.len() > 256 * 1024
            || source.contains('\0')
            || input.len() > 256
            || digest.len() != 64
            || !digest
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            || policy_revision.is_empty()
            || policy_revision.len() > 128
            || !policy_revision
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
            || !(1..=3600).contains(&timeout_seconds)
        {
            return Err(InvalidRequest);
        }
        let mut remaining = 100_000;
        for value in input.values_mut() {
            validate_shape(value, 0, &mut remaining)?;
            value.sort_all_objects();
        }
        let job = Self {
            revision: 1,
            language,
            source,
            input,
            image_digest,
            policy_revision,
            timeout_seconds,
            dependency_bundle_sha256: None,
            native_dependencies: None,
            platform_client: None,
            workspace: None,
        };
        job.bytes()?;
        Ok(job)
    }

    /// Bind resolved Python content before requesting an authorization grant.
    /// The digest comes from a durable preparation receipt, not cache metadata.
    /// # Errors
    /// Returns `InvalidRequest` for another language, an invalid digest, or excessive bytes.
    pub fn with_python_dependency_bundle(mut self, digest: String) -> Result<Self, InvalidRequest> {
        if self.language != Language::Python
            || digest.len() != 64
            || !digest
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return Err(InvalidRequest);
        }
        self.revision = self.revision.max(2);
        self.dependency_bundle_sha256 = Some(digest);
        self.bytes()?;
        Ok(self)
    }

    /// Bind native content to its acquisition source and immutable preparation identity.
    /// # Errors
    /// Rejects unsupported language bindings, invalid digests, excessive declarations, and source mismatches.
    pub fn with_native_dependency_bundle(
        mut self,
        root: String,
        native: NativeDependencies,
    ) -> Result<Self, InvalidRequest> {
        native.platform.validate().map_err(|_| InvalidRequest)?;
        let acquisition = match (
            self.language,
            native.kind,
            native.dependencies_toml.as_deref(),
        ) {
            (Language::JavaScript | Language::TypeScript, NativeKind::Deno, None) => {
                self.source.as_str()
            }
            (Language::Rust, NativeKind::Cargo, Some(v))
                if !v.is_empty() && v.len() <= 64 * 1024 && !v.contains('\0') =>
            {
                v
            }
            _ => return Err(InvalidRequest),
        };
        if !valid_digest(&root)
            || !valid_digest(&native.preparation_sha256)
            || !valid_digest(&native.source_sha256)
            || hex(digest::digest(&digest::SHA256, acquisition.as_bytes()).as_ref())
                != native.source_sha256
        {
            return Err(InvalidRequest);
        }
        self.revision = self.revision.max(3);
        self.dependency_bundle_sha256 = Some(root);
        self.native_dependencies = Some(native);
        self.bytes()?;
        Ok(self)
    }
    /// Bind only an immutable Main snapshot before requesting execution authority.
    /// # Errors
    /// Rejects invalid workspace identity and requests beyond the existing size limit.
    pub fn with_workspace(
        mut self,
        workspace: super::workspace::WorkspaceBinding,
    ) -> Result<Self, InvalidRequest> {
        workspace.validate().map_err(|_| InvalidRequest)?;
        self.revision = self.revision.max(4);
        self.workspace = Some(workspace);
        self.bytes()?;
        Ok(self)
    }
    /// Reconstruct the exact admitted dependency/broker base. Remove only workspace.
    /// # Errors
    /// Rejects any base which exceeds the existing prepared-request limits.
    pub fn pre_workspace(&self) -> Result<Self, InvalidRequest> {
        let revision = if self.platform_client.is_some() {
            5
        } else if self.native_dependencies.is_some() {
            3
        } else if self.dependency_bundle_sha256.is_some() {
            2
        } else {
            1
        };
        let base = Self {
            revision,
            language: self.language,
            source: self.source.clone(),
            input: self.input.clone(),
            image_digest: self.image_digest.clone(),
            policy_revision: self.policy_revision.clone(),
            timeout_seconds: self.timeout_seconds,
            dependency_bundle_sha256: self.dependency_bundle_sha256.clone(),
            native_dependencies: self.native_dependencies.clone(),
            workspace: None,
            platform_client: self.platform_client.clone(),
        };
        base.bytes()?;
        Ok(base)
    }
    #[must_use]
    pub fn workspace(&self) -> Option<&super::workspace::WorkspaceBinding> {
        self.workspace.as_ref()
    }
    #[must_use]
    pub fn native_dependencies(&self) -> Option<&NativeDependencies> {
        self.native_dependencies.as_ref()
    }
    #[must_use]
    pub fn matches_bundle(&self, bundle: &super::dependency_bundle::DependencyBundle) -> bool {
        if self.dependency_bundle_root() != Some(bundle.root()) {
            return false;
        }
        match (self.native_dependencies(), bundle.native()) {
            (None, None) => self.language == Language::Python,
            (Some(n), Some(b)) => {
                n.kind == b.record.kind
                    && n.platform == b.record.platform
                    && self.language == b.record.language
                    && n.preparation_sha256 == b.record.preparation_sha256
                    && n.source_sha256 == b.record.source_sha256
                    && self.image_digest == b.record.execution_image_digest
                    && self.policy_revision == b.record.execution_policy_revision
            }
            _ => false,
        }
    }
    /// Decode bounded transport input and reapply every constructor invariant.
    /// # Errors
    /// Returns `InvalidRequest` for malformed, unknown, duplicate top-level, or invalid fields.
    pub fn from_transport(bytes: &[u8]) -> Result<Self, InvalidRequest> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct WireJob {
            revision: u8,
            language: Language,
            source: String,
            input: BTreeMap<String, serde_json::Value>,
            image_digest: String,
            policy_revision: String,
            timeout_seconds: u32,
            #[serde(default, deserialize_with = "dependency_digest")]
            dependency_bundle_sha256: Option<String>,
            #[serde(default, deserialize_with = "native_identity_field")]
            native_dependencies: Option<NativeDependencies>,
            #[serde(default, deserialize_with = "workspace_identity_field")]
            workspace: Option<super::workspace::WorkspaceBinding>,
            #[serde(default, deserialize_with = "platform_client_identity_field")]
            platform_client: Option<super::platform_client_binding::PlatformClientBinding>,
        }
        if bytes.len() > 1024 * 1024 {
            return Err(InvalidRequest);
        }
        let wire: WireJob = serde_json::from_slice(bytes).map_err(|_| InvalidRequest)?;
        let job = Self::new(
            wire.language,
            wire.source,
            wire.input,
            wire.image_digest,
            wire.policy_revision,
            wire.timeout_seconds,
        )?;
        let base = match (wire.dependency_bundle_sha256, wire.native_dependencies) {
            (None, None) => job,
            (Some(root), None) => job.with_python_dependency_bundle(root)?,
            (Some(root), Some(native)) => job.with_native_dependency_bundle(root, native)?,
            _ => return Err(InvalidRequest),
        };
        match (wire.revision, wire.workspace, wire.platform_client) {
            (5, workspace, Some(platform)) => {
                let base = match workspace {
                    Some(value) => base.with_workspace(value)?,
                    None => base,
                };
                base.with_platform_client(platform)
            }
            (4, Some(workspace), None) => base.with_workspace(workspace),
            (revision, None, None) if revision == base.revision => Ok(base),
            _ => Err(InvalidRequest),
        }
    }

    /// Bind only the deployment-selected broker policy before grant creation.
    /// The signed prepared fingerprint binds this exact optional capability.
    pub(crate) fn with_platform_client(
        mut self,
        binding: super::platform_client_binding::PlatformClientBinding,
    ) -> Result<Self, InvalidRequest> {
        binding.validate()?;
        self.platform_client = Some(binding);
        self.revision = 5;
        self.bytes()?;
        Ok(self)
    }

    pub(crate) fn platform_client(
        &self,
    ) -> Option<&super::platform_client_binding::PlatformClientBinding> {
        self.platform_client.as_ref()
    }

    /// Serialize the exact prepared request for authenticated supervisor transport.
    /// # Errors
    /// Returns `InvalidRequest` if serialization exceeds the request limit.
    pub fn to_transport(&self) -> Result<Vec<u8>, InvalidRequest> {
        self.bytes()
    }

    #[cfg(feature = "sandbox-supervisor")]
    pub(crate) fn within_timeout(&self, maximum: std::time::Duration) -> bool {
        std::time::Duration::from_secs(self.timeout_seconds.into()) <= maximum
    }

    #[cfg(feature = "sandbox-supervisor")]
    pub(crate) fn matches_runtime(
        &self,
        image: &str,
        policy: &str,
        languages: &[Language],
    ) -> bool {
        let configured_digest = image.rsplit_once('@').map_or(image, |(_, digest)| digest);
        self.image_digest == configured_digest
            && self.policy_revision == policy
            && languages.contains(&self.language)
    }

    #[must_use]
    pub fn dependency_bundle_root(&self) -> Option<&str> {
        self.dependency_bundle_sha256.as_deref()
    }

    /// The image owns the language adapter; no caller supplies executable argv.
    #[cfg(feature = "sandbox-supervisor")]
    pub(crate) fn manifest(&self) -> Result<adk_sandbox::workspace::Manifest, InvalidRequest> {
        use adk_sandbox::workspace::{Manifest, ManifestEntry};
        let runner = serde_json::json!({
            "argv": ["/usr/local/bin/elitea-code-execute", "/workspace/.elitea-code.json"],
            "timeout_seconds": self.timeout_seconds,
        });
        Ok(Manifest::new(vec![
            ManifestEntry::File {
                path: ".elitea-code.json".into(),
                content: self.bytes()?,
            },
            ManifestEntry::File {
                path: ".elitea-job.json".into(),
                content: serde_json::to_vec(&runner).map_err(|_| InvalidRequest)?,
            },
        ]))
    }

    fn bytes(&self) -> Result<Vec<u8>, InvalidRequest> {
        let mut writer = CappedBytes(Vec::new());
        serde_json::to_writer(&mut writer, self).map_err(|_| InvalidRequest)?;
        Ok(writer.0)
    }

    /// Bind a caller-authorized scope to the actual request content. The caller
    /// supplies the stable activation key, but cannot supply its request digest.
    ///
    /// # Errors
    /// Returns `Invalid` for an invalid scope or serialization failure.
    #[cfg(feature = "sandbox-supervisor")]
    pub fn scope(
        &self,
        tenant: String,
        project: i32,
        activation_key: [u8; 32],
    ) -> Result<JobScope, LedgerError> {
        JobScope::new(
            tenant,
            project,
            activation_key,
            self.fingerprint().map_err(|_| LedgerError::Invalid)?,
        )
    }

    /// # Errors
    /// Returns `Invalid` if the bounded serialization fails.
    pub fn fingerprint(&self) -> Result<[u8; 32], InvalidRequest> {
        let bytes = self.bytes()?;
        let mut hash = ring::digest::Context::new(&ring::digest::SHA256);
        hash.update(b"elitea.sandbox.prepared-job.v1\0");
        hash.update(&(bytes.len() as u64).to_be_bytes());
        hash.update(&bytes);
        let digest = hash.finish();
        let mut content_digest = [0; 32];
        content_digest.copy_from_slice(digest.as_ref());
        Ok(content_digest)
    }
}

fn dependency_digest<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<String>, D::Error> {
    String::deserialize(deserializer).map(Some)
}

fn validate_shape(
    value: &serde_json::Value,
    depth: usize,
    remaining: &mut usize,
) -> Result<(), InvalidRequest> {
    if depth > 64 || *remaining == 0 {
        return Err(InvalidRequest);
    }
    *remaining -= 1;
    match value {
        serde_json::Value::Array(values) => {
            for value in values {
                validate_shape(value, depth + 1, remaining)?;
            }
        }
        serde_json::Value::Object(values) => {
            for value in values.values() {
                validate_shape(value, depth + 1, remaining)?;
            }
        }
        _ => {}
    }
    Ok(())
}

struct CappedBytes(Vec<u8>);
impl std::io::Write for CappedBytes {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if bytes.len() > (1024 * 1024_usize).saturating_sub(self.0.len()) {
            return Err(std::io::Error::other(
                "sandbox request exceeds its size bound",
            ));
        }
        self.0.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

#[cfg(all(test, feature = "sandbox-supervisor"))]
mod tests {
    use super::*;
    use std::fmt::Write as _;

    fn job() -> PreparedJob {
        PreparedJob::new(
            Language::Python,
            "print(42)".into(),
            BTreeMap::new(),
            format!("sha256:{}", "a".repeat(64)),
            "python-v1".into(),
            30,
        )
        .unwrap()
    }

    #[test]
    fn transport_roundtrip_keeps_signed_fingerprint() {
        let original = job();
        let decoded = PreparedJob::from_transport(&original.to_transport().unwrap()).unwrap();
        assert_eq!(
            original.fingerprint().unwrap(),
            decoded.fingerprint().unwrap()
        );
    }

    #[test]
    fn legacy_request_keeps_exact_bytes_and_receipt_identity() {
        let expected = format!(
            r#"{{"revision":1,"language":"python","source":"print(42)","input":{{}},"image_digest":"sha256:{}","policy_revision":"python-v1","timeout_seconds":30}}"#,
            "a".repeat(64)
        );
        assert_eq!(job().to_transport().unwrap(), expected.as_bytes());
        let fingerprint = job().fingerprint().unwrap();
        let hex = fingerprint
            .iter()
            .fold(String::with_capacity(64), |mut hex, byte| {
                write!(hex, "{byte:02x}").unwrap();
                hex
            });
        assert_eq!(
            hex,
            "fea55852b5400fa8b79583d698826a8ea1807af6ccf7a5cd40dddca99cfae067"
        );
    }

    #[test]
    fn prepared_python_content_binds_authorization_and_receipt_reuse() {
        let original = job().with_python_dependency_bundle("b".repeat(64)).unwrap();
        let decoded = PreparedJob::from_transport(&original.to_transport().unwrap()).unwrap();
        assert_eq!(identity(&original), identity(&decoded));
        assert_ne!(identity(&original), identity(&job()));
        let changed = job().with_python_dependency_bundle("c".repeat(64)).unwrap();
        assert_ne!(identity(&original), identity(&changed));
        assert!(original.to_transport().unwrap().len() < 1024 * 1024);
    }

    #[test]
    fn dependency_transport_rejects_downgrades_and_invalid_content_identity() {
        let original = job().with_python_dependency_bundle("a".repeat(64)).unwrap();
        for (field, value) in [
            ("revision", serde_json::json!(1)),
            ("revision", serde_json::json!(3)),
            ("language", serde_json::json!("javascript")),
            ("dependency_bundle_sha256", serde_json::json!(null)),
            (
                "dependency_bundle_sha256",
                serde_json::json!("A".repeat(64)),
            ),
            (
                "dependency_bundle_sha256",
                serde_json::json!("a".repeat(63)),
            ),
            (
                "dependency_bundle_sha256",
                serde_json::json!(format!("sha256:{}", "a".repeat(64))),
            ),
        ] {
            let mut wire: serde_json::Value =
                serde_json::from_slice(&original.to_transport().unwrap()).unwrap();
            wire[field] = value;
            assert!(PreparedJob::from_transport(&serde_json::to_vec(&wire).unwrap()).is_err());
        }
        let mut wrong_language = job();
        wrong_language.language = Language::Rust;
        assert!(
            wrong_language
                .with_python_dependency_bundle("a".repeat(64))
                .is_err()
        );
    }

    #[test]
    fn transport_rejects_schema_and_constructor_bypasses() {
        let original = job().to_transport().unwrap();
        for (field, value) in [
            ("revision", serde_json::json!(2)),
            ("language", serde_json::json!("shell")),
            ("source", serde_json::json!("")),
            ("timeout_seconds", serde_json::json!(3601)),
            ("dependency_bundle_sha256", serde_json::json!(null)),
            ("extra", serde_json::json!(true)),
        ] {
            let mut value_map: serde_json::Value = serde_json::from_slice(&original).unwrap();
            value_map[field] = value;
            assert!(PreparedJob::from_transport(&serde_json::to_vec(&value_map).unwrap()).is_err());
        }
        let duplicate = String::from_utf8(original)
            .unwrap()
            .replacen('{', "{\"revision\":1,", 1);
        assert!(PreparedJob::from_transport(duplicate.as_bytes()).is_err());
        assert!(PreparedJob::from_transport(&vec![b' '; 1024 * 1024 + 1]).is_err());
    }

    fn identity(job: &PreparedJob) -> adk_sandbox::workspace::docker::CodeJobIdentity {
        job.scope("tenant".into(), 2, [1; 32])
            .unwrap()
            .runtime_identity()
            .unwrap()
    }

    #[test]
    fn runtime_profile_binds_language_image_and_policy() {
        let request = job();
        let image = request.image_digest.clone();
        assert!(request.matches_runtime(&image, "python-v1", &[Language::Python]));
        assert!(!request.matches_runtime(&image, "python-v1", &[Language::Rust]));
        assert!(!request.matches_runtime(&image, "python-v2", &[Language::Python]));
        assert!(!request.matches_runtime("other", "python-v1", &[Language::Python]));
    }

    #[test]
    fn changed_execution_material_cannot_reuse_a_cached_receipt() {
        let original = identity(&job());
        for field in 0..6 {
            let mut changed = job();
            match field {
                0 => changed.source = "print(43)".into(),
                1 => {
                    changed.input.insert("value".into(), serde_json::json!(42));
                }
                2 => changed.language = Language::Rust,
                3 => changed.image_digest = format!("sha256:{}", "b".repeat(64)),
                4 => changed.policy_revision = "python-v2".into(),
                _ => changed.timeout_seconds = 31,
            }
            assert_ne!(identity(&changed), original);
        }
        assert_eq!(identity(&job()), original);
    }

    #[test]
    fn map_insertion_order_does_not_change_request_identity() {
        let mut a = job();
        a.input.insert("a".into(), serde_json::json!({"z":1,"a":2}));
        a.input.insert("b".into(), serde_json::json!(3));
        let mut b = job();
        b.input.insert("b".into(), serde_json::json!(3));
        b.input.insert("a".into(), serde_json::json!({"a":2,"z":1}));
        assert_eq!(identity(&a), identity(&b));
    }

    #[test]
    fn rejects_oversized_and_deep_input_before_fingerprinting() {
        let make = |value| {
            PreparedJob::new(
                Language::Python,
                "pass".into(),
                BTreeMap::from([("value".into(), value)]),
                format!("sha256:{}", "a".repeat(64)),
                "v1".into(),
                30,
            )
        };
        assert!(make(serde_json::Value::String("x".repeat(1024 * 1024))).is_err());
        let mut deep = serde_json::Value::Null;
        for _ in 0..66 {
            deep = serde_json::Value::Array(vec![deep]);
        }
        assert!(make(deep).is_err());
    }

    #[test]
    fn rejects_mutable_image_and_unbounded_timeout() {
        assert!(
            PreparedJob::new(
                Language::Python,
                "pass".into(),
                BTreeMap::new(),
                "python:latest".into(),
                "v1".into(),
                30
            )
            .is_err()
        );
        assert!(
            PreparedJob::new(
                Language::Python,
                "pass".into(),
                BTreeMap::new(),
                format!("sha256:{}", "a".repeat(64)),
                "v1".into(),
                3601
            )
            .is_err()
        );
    }
}

fn native_identity_field<'de, D: serde::Deserializer<'de>>(
    d: D,
) -> Result<Option<NativeDependencies>, D::Error> {
    NativeDependencies::deserialize(d).map(Some)
}

#[cfg(test)]
mod native_tests {
    use super::*;
    #[test]
    fn native_identity_binds_execution_without_changing_python_bytes() {
        let raw = include_bytes!("native-deno-v2.json");
        let v: serde_json::Value = serde_json::from_slice(raw).unwrap();
        let bundle = super::super::dependency_bundle::DependencyBundle::parse(
            raw,
            v["digest"].as_str().unwrap(),
        )
        .unwrap();
        let job = PreparedJob::new(
            Language::TypeScript,
            "export default 42;".into(),
            BTreeMap::new(),
            format!("sha256:{}", "b".repeat(64)),
            "js-offline-v2".into(),
            30,
        )
        .unwrap();
        let native = NativeDependencies {
            kind: NativeKind::Deno,
            platform: NativePlatform {
                os: "linux".into(),
                arch: "arm64".into(),
                abi: "gnu".into(),
            },
            preparation_sha256: v["preparation_sha256"].as_str().unwrap().into(),
            source_sha256: v["source_sha256"].as_str().unwrap().into(),
            dependencies_toml: None,
        };
        let job = job
            .with_native_dependency_bundle(bundle.root().into(), native)
            .unwrap();
        assert!(job.matches_bundle(&bundle));
        let bytes = job.to_transport().unwrap();
        assert!(
            PreparedJob::from_transport(&bytes)
                .unwrap()
                .matches_bundle(&bundle)
        );
        let mut wrong: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        wrong["source"] = serde_json::json!("export default 41;");
        assert!(PreparedJob::from_transport(&serde_json::to_vec(&wrong).unwrap()).is_err());
        let legacy = PreparedJob::new(
            Language::Python,
            "print(42)".into(),
            BTreeMap::new(),
            format!("sha256:{}", "a".repeat(64)),
            "python-v1".into(),
            30,
        )
        .unwrap();
        let mut value: serde_json::Value =
            serde_json::from_slice(&legacy.to_transport().unwrap()).unwrap();
        value["native_dependencies"] = serde_json::Value::Null;
        assert!(PreparedJob::from_transport(&serde_json::to_vec(&value).unwrap()).is_err());
    }
}

fn workspace_identity_field<'de, D>(
    deserializer: D,
) -> Result<Option<super::workspace::WorkspaceBinding>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    super::workspace::WorkspaceBinding::deserialize(deserializer).map(Some)
}

fn platform_client_identity_field<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<super::platform_client_binding::PlatformClientBinding>, D::Error> {
    super::platform_client_binding::PlatformClientBinding::deserialize(deserializer).map(Some)
}

#[cfg(test)]
#[path = "request_workspace_projection_tests.rs"]
mod workspace_projection_tests;
