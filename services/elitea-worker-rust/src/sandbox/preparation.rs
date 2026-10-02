//! Python dependency preparation identity. This contract does not dispatch a runner.
use std::collections::BTreeMap;

use ring::digest;
use serde::{Deserialize, Serialize};

use super::request::{InvalidRequest, Language, PreparedJob};

const MAX_TRANSPORT_BYTES: usize = 1024 * 1024;

/// Literal source and deployment-selected preparation policy, without execution state.
/// No Debug: the source can contain private user data.
#[derive(Serialize)]
pub struct PreparationJob {
    revision: u8,
    language: Language,
    source: String,
    preparer_image_digest: String,
    policy_revision: String,
    timeout_seconds: u32,
}

impl PreparationJob {
    /// Select the immutable preparer image and policy before requesting Main authority.
    /// This request does not authorize source execution or supply a package list.
    ///
    /// # Errors
    /// Returns `InvalidRequest` for invalid source, runtime identity, or bounded limits.
    pub fn new(
        source: String,
        preparer_image_digest: String,
        policy_revision: String,
        timeout_seconds: u32,
    ) -> Result<Self, InvalidRequest> {
        // Keep source, image, policy, and timeout rules consistent with execution.
        // This validation job never receives invocation state or leaves this constructor.
        PreparedJob::new(
            Language::Python,
            source.clone(),
            BTreeMap::new(),
            preparer_image_digest.clone(),
            policy_revision.clone(),
            timeout_seconds,
        )?;
        let job = Self {
            revision: 1,
            language: Language::Python,
            source,
            preparer_image_digest,
            policy_revision,
            timeout_seconds,
        };
        job.to_transport()?;
        Ok(job)
    }

    /// Decode strict versioned JSON and reapply constructor validation.
    ///
    /// # Errors
    /// Returns `InvalidRequest` for malformed, unknown, duplicate, or invalid fields.
    pub fn from_transport(bytes: &[u8]) -> Result<Self, InvalidRequest> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct WireJob {
            revision: u8,
            language: Language,
            source: String,
            preparer_image_digest: String,
            policy_revision: String,
            timeout_seconds: u32,
        }
        if bytes.len() > MAX_TRANSPORT_BYTES {
            return Err(InvalidRequest);
        }
        let wire: WireJob = serde_json::from_slice(bytes).map_err(|_| InvalidRequest)?;
        if wire.revision != 1 || wire.language != Language::Python {
            return Err(InvalidRequest);
        }
        Self::new(
            wire.source,
            wire.preparer_image_digest,
            wire.policy_revision,
            wire.timeout_seconds,
        )
    }

    /// Serialize the bounded preparation request without execution state.
    ///
    /// # Errors
    /// Returns `InvalidRequest` if serialization exceeds the transport limit.
    pub fn to_transport(&self) -> Result<Vec<u8>, InvalidRequest> {
        let bytes = serde_json::to_vec(self).map_err(|_| InvalidRequest)?;
        if bytes.len() > MAX_TRANSPORT_BYTES {
            return Err(InvalidRequest);
        }
        Ok(bytes)
    }

    /// Bind Main authority to the complete immutable preparation request.
    /// The preparation domain cannot identify an execution request.
    ///
    /// # Errors
    /// Returns `InvalidRequest` if bounded serialization fails.
    pub fn fingerprint(&self) -> Result<[u8; 32], InvalidRequest> {
        let bytes = self.to_transport()?;
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"elitea.sandbox.preparation-job.v1\0");
        hash.update(&(bytes.len() as u64).to_be_bytes());
        hash.update(&bytes);
        let mut fingerprint = [0; 32];
        fingerprint.copy_from_slice(hash.finish().as_ref());
        Ok(fingerprint)
    }
}

/// Derive stable preparation identity from the original graph Code activation.
/// Worker replacement and grant renewal do not change this identity.
#[must_use]
pub fn preparation_activation(code_activation: &[u8; 32]) -> [u8; 32] {
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(b"elitea.graph.code.preparation-activation.v1\0");
    hash.update(&(code_activation.len() as u64).to_be_bytes());
    hash.update(code_activation);
    let mut activation = [0; 32];
    activation.copy_from_slice(hash.finish().as_ref());
    activation
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fmt::Write as _;

    fn job() -> PreparationJob {
        PreparationJob::new(
            "print(42)".into(),
            format!("sha256:{}", "a".repeat(64)),
            "python-v1".into(),
            30,
        )
        .unwrap()
    }

    fn hex(bytes: &[u8]) -> String {
        bytes.iter().fold(String::new(), |mut output, byte| {
            write!(output, "{byte:02x}").unwrap();
            output
        })
    }

    #[test]
    fn transport_roundtrip_has_stable_preparation_identity() {
        let original = job();
        let bytes = original.to_transport().unwrap();
        let expected = format!(
            r#"{{"revision":1,"language":"python","source":"print(42)","preparer_image_digest":"sha256:{}","policy_revision":"python-v1","timeout_seconds":30}}"#,
            "a".repeat(64)
        );
        assert_eq!(bytes, expected.as_bytes());
        let decoded = PreparationJob::from_transport(&bytes).unwrap();
        assert_eq!(decoded.to_transport().unwrap(), bytes);
        assert_eq!(
            decoded.fingerprint().unwrap(),
            original.fingerprint().unwrap()
        );
        assert_eq!(
            hex(&original.fingerprint().unwrap()),
            "7b2089599c4d49bc6d286dadad08944ad78384b873d43776d5f81421ae78dbb0"
        );
        let execution = PreparedJob::new(
            Language::Python,
            "print(42)".into(),
            BTreeMap::new(),
            format!("sha256:{}", "a".repeat(64)),
            "python-v1".into(),
            30,
        )
        .unwrap();
        assert_ne!(
            original.fingerprint().unwrap(),
            execution.fingerprint().unwrap()
        );
        assert!(PreparedJob::from_transport(&bytes).is_err());
        assert!(PreparationJob::from_transport(&execution.to_transport().unwrap()).is_err());
        let source = "text = r\"first\\nsecond\"\nprint(text)\n";
        let literal = PreparationJob::new(
            source.into(),
            format!("sha256:{}", "a".repeat(64)),
            "python-v1".into(),
            30,
        )
        .unwrap();
        let wire: serde_json::Value =
            serde_json::from_slice(&literal.to_transport().unwrap()).unwrap();
        assert_eq!(wire["source"], source);
        let reordered =
            PreparationJob::from_transport(&serde_json::to_vec(&wire).unwrap()).unwrap();
        assert_eq!(
            literal.fingerprint().unwrap(),
            reordered.fingerprint().unwrap()
        );
    }

    #[test]
    fn transport_rejects_execution_material_and_invalid_schema() {
        let bytes = job().to_transport().unwrap();
        for (field, value) in [
            ("revision", serde_json::json!(0)),
            ("revision", serde_json::json!(2)),
            ("language", serde_json::json!("javascript")),
            ("language", serde_json::json!("typescript")),
            ("language", serde_json::json!("rust")),
            ("language", serde_json::json!("shell")),
            ("source", serde_json::json!(null)),
            ("source", serde_json::json!("")),
            ("input", serde_json::json!({})),
            ("argv", serde_json::json!(["python", "-c", "pass"])),
            ("packages", serde_json::json!(["humanize"])),
            ("root", serde_json::json!("b".repeat(64))),
            (
                "dependency_bundle_sha256",
                serde_json::json!("b".repeat(64)),
            ),
            ("extra", serde_json::json!(true)),
        ] {
            let mut wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            wire[field] = value;
            assert!(PreparationJob::from_transport(&serde_json::to_vec(&wire).unwrap()).is_err());
        }
        let wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        let text = String::from_utf8(bytes).unwrap();
        for (field, value) in wire.as_object().unwrap() {
            let duplicate = text.replacen('{', &format!("{{\"{field}\":{value},"), 1);
            assert!(PreparationJob::from_transport(duplicate.as_bytes()).is_err());
        }
        assert!(PreparationJob::from_transport(&vec![b' '; MAX_TRANSPORT_BYTES + 1]).is_err());
    }

    #[test]
    fn constructor_and_transport_enforce_the_same_bounds() {
        let bytes = job().to_transport().unwrap();
        for (field, value) in [
            ("source", serde_json::json!(" ")),
            ("source", serde_json::json!("print(42)\0")),
            ("source", serde_json::json!("x".repeat(256 * 1024 + 1))),
            ("source", serde_json::json!("\u{1}".repeat(256 * 1024))),
            ("preparer_image_digest", serde_json::json!("python:latest")),
            (
                "preparer_image_digest",
                serde_json::json!(format!("sha256:{}", "A".repeat(64))),
            ),
            (
                "preparer_image_digest",
                serde_json::json!(format!("sha256:{}", "a".repeat(63))),
            ),
            ("policy_revision", serde_json::json!("")),
            ("policy_revision", serde_json::json!("x".repeat(129))),
            ("policy_revision", serde_json::json!("python v1")),
            ("timeout_seconds", serde_json::json!(0)),
            ("timeout_seconds", serde_json::json!(3601)),
        ] {
            let mut wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            wire[field] = value;
            assert!(PreparationJob::from_transport(&serde_json::to_vec(&wire).unwrap()).is_err());
        }
        for timeout in [1, 3600] {
            assert!(
                PreparationJob::new(
                    "x".repeat(256 * 1024),
                    format!("sha256:{}", "a".repeat(64)),
                    "x".repeat(128),
                    timeout,
                )
                .is_ok()
            );
        }
    }

    #[test]
    fn every_preparation_field_binds_the_fingerprint() {
        let bytes = job().to_transport().unwrap();
        let fingerprint = job().fingerprint().unwrap();
        for (field, value) in [
            ("source", serde_json::json!("print(43)")),
            (
                "preparer_image_digest",
                serde_json::json!(format!("sha256:{}", "b".repeat(64))),
            ),
            ("policy_revision", serde_json::json!("python-v2")),
            ("timeout_seconds", serde_json::json!(31)),
        ] {
            let mut wire: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            wire[field] = value;
            let changed =
                PreparationJob::from_transport(&serde_json::to_vec(&wire).unwrap()).unwrap();
            assert_ne!(changed.fingerprint().unwrap(), fingerprint);
        }
    }

    #[test]
    fn preparation_activation_is_stable_and_separate_from_execution() {
        let code_activation = [7; 32];
        let prepared = preparation_activation(&code_activation);
        assert_eq!(prepared, preparation_activation(&code_activation));
        assert_ne!(prepared, code_activation);
        assert_ne!(prepared, preparation_activation(&[8; 32]));
        assert_eq!(
            hex(&prepared),
            "ed5b8382186f15a8104e2f09c549fc09f147ea2aa2507ecd673ff616a7b5e093"
        );
    }
}
