//! Content identity for an admitted sandbox job. This is not authorization.
use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

use super::ledger::{JobScope, LedgerError};

#[derive(Clone, Copy, Deserialize, Serialize, PartialEq, Eq)]
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
}

#[derive(Debug, thiserror::Error)]
#[error("sandbox request has invalid code, input, runtime identity, or limits")]
pub struct InvalidRequest;

impl PreparedJob {
    /// Construct only after runtime selection and invocation authorization.
    /// Dependencies and wrappers must be fixed by the runtime image/policy revision.
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
        };
        job.bytes()?;
        Ok(job)
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
        }
        if bytes.len() > 1024 * 1024 {
            return Err(InvalidRequest);
        }
        let wire: WireJob = serde_json::from_slice(bytes).map_err(|_| InvalidRequest)?;
        if wire.revision != 1 {
            return Err(InvalidRequest);
        }
        Self::new(
            wire.language,
            wire.source,
            wire.input,
            wire.image_digest,
            wire.policy_revision,
            wire.timeout_seconds,
        )
    }

    /// Serialize the exact prepared request for authenticated supervisor transport.
    /// # Errors
    /// Returns `InvalidRequest` if serialization exceeds the request limit.
    pub fn to_transport(&self) -> Result<Vec<u8>, InvalidRequest> {
        self.bytes()
    }

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

    /// The image owns the language adapter; no caller supplies executable argv.
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
    pub fn scope(
        &self,
        tenant: String,
        project: i32,
        activation_key: [u8; 32],
    ) -> Result<JobScope, LedgerError> {
        JobScope::new(tenant, project, activation_key, self.fingerprint()?)
    }

    /// # Errors
    /// Returns `Invalid` if the bounded serialization fails.
    pub fn fingerprint(&self) -> Result<[u8; 32], LedgerError> {
        let bytes = self.bytes().map_err(|_| LedgerError::Invalid)?;
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

#[cfg(test)]
mod tests {
    use super::*;

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
    fn transport_rejects_schema_and_constructor_bypasses() {
        let original = job().to_transport().unwrap();
        for (field, value) in [
            ("revision", serde_json::json!(2)),
            ("language", serde_json::json!("shell")),
            ("source", serde_json::json!("")),
            ("timeout_seconds", serde_json::json!(3601)),
            ("extra", serde_json::json!(true)),
        ] {
            let mut value_map: serde_json::Value = serde_json::from_slice(&original).unwrap();
            value_map[field] = value;
            assert!(PreparedJob::from_transport(&serde_json::to_vec(&value_map).unwrap()).is_err());
        }
        let duplicate = String::from_utf8(original)
            .unwrap()
            .replacen("{", "{\"revision\":1,", 1);
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
