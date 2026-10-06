//! Python dependency preparation identity. This contract does not dispatch a runner.
use std::collections::BTreeMap;

use ring::digest;
use serde::{Deserialize, Serialize};

use super::dependency_bundle::{DependencyBundle, hex};
use super::native_bundle::NativePlatform;
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
    #[serde(skip_serializing_if = "Option::is_none")]
    platform: Option<NativePlatform>,
    #[serde(skip_serializing_if = "Option::is_none")]
    execution_image_digest: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    execution_policy_revision: Option<String>,
}

impl PreparationJob {
    #[cfg(feature = "sandbox-supervisor")]
    #[must_use]
    pub(crate) fn source(&self) -> &str {
        &self.source
    }

    #[cfg(feature = "sandbox-supervisor")]
    #[must_use]
    pub(crate) fn image_digest(&self) -> &str {
        &self.preparer_image_digest
    }

    #[cfg(feature = "sandbox-supervisor")]
    #[must_use]
    pub(crate) fn policy_revision(&self) -> &str {
        &self.policy_revision
    }

    #[cfg(feature = "sandbox-supervisor")]
    #[must_use]
    pub(crate) fn timeout_seconds(&self) -> u32 {
        self.timeout_seconds
    }

    #[cfg(feature = "sandbox-supervisor")]
    pub(crate) fn within_timeout(&self, maximum: std::time::Duration) -> bool {
        std::time::Duration::from_secs(self.timeout_seconds.into()) <= maximum
    }

    #[cfg(feature = "sandbox-supervisor")]
    pub(crate) fn matches_runtime(&self, image: &str, policy: &str) -> bool {
        let configured_digest = image.rsplit_once('@').map_or(image, |(_, digest)| digest);
        self.preparer_image_digest == configured_digest && self.policy_revision == policy
    }

    /// Select the image-owned holding preparer. Source never supplies command arguments.
    #[cfg(feature = "sandbox-supervisor")]
    pub(crate) fn manifest(&self) -> Result<adk_sandbox::workspace::Manifest, InvalidRequest> {
        use adk_sandbox::workspace::{Manifest, ManifestEntry};
        let runner = if self.revision == 2 {
            let argv = match self.language {
                Language::JavaScript | Language::TypeScript => vec![
                    "/usr/local/bin/deno",
                    "run",
                    "--no-config",
                    "--cached-only",
                    "--no-prompt",
                    "--deny-ffi",
                    "--allow-run=/usr/local/bin/deno",
                    "--allow-read=/opt/elitea-code,/opt/deno-cache,/workspace",
                    "--allow-write=/workspace",
                    "--allow-env",
                    "--allow-net=jsr.io,registry.npmjs.org",
                    "/opt/elitea-code/javascript_preparation_job.mjs",
                ],
                Language::Rust => vec!["/usr/local/bin/elitea-code-rust-prepare", "--retain"],
                Language::Python => return Err(InvalidRequest),
            };
            serde_json::json!({"argv":argv,"timeout_seconds":self.timeout_seconds})
        } else {
            serde_json::json!({
                "argv": [
                    "/usr/local/bin/deno", "run", "--no-config", "--frozen",
                "--lock=/opt/elitea-code/deno.lock", "--cached-only", "--no-prompt",
                    "--deny-run", "--deny-ffi",
                    "--allow-read=/opt/elitea-code,/opt/deno-cache,/workspace",
                    "--allow-write=/workspace", "--allow-env=NODE_DEBUG",
                    "--allow-net=cdn.jsdelivr.net,pypi.org,files.pythonhosted.org",
                    "/opt/elitea-code/python_preparation_job.mjs"
                ],
                "timeout_seconds": self.timeout_seconds,
            })
        };
        Ok(Manifest::new(vec![
            ManifestEntry::File {
                path: ".elitea-code.json".into(),
                content: self.to_transport()?,
            },
            ManifestEntry::File {
                path: ".elitea-job.json".into(),
                content: serde_json::to_vec(&runner).map_err(|_| InvalidRequest)?,
            },
        ]))
    }

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
            platform: None,
            execution_image_digest: None,
            execution_policy_revision: None,
        };
        job.to_transport()?;
        Ok(job)
    }

    /// Bind acquisition to the selected platform and both immutable runtime profiles.
    /// # Errors
    /// Rejects unsupported languages, invalid profiles, excessive source, and unsafe time limits.
    #[allow(clippy::too_many_arguments)] // Preserve both complete profile bindings at this boundary.
    pub fn new_native(
        language: Language,
        source: String,
        preparer_image_digest: String,
        policy_revision: String,
        timeout_seconds: u32,
        platform: NativePlatform,
        execution_image_digest: String,
        execution_policy_revision: String,
    ) -> Result<Self, InvalidRequest> {
        if language == Language::Python
            || timeout_seconds > if language == Language::Rust { 600 } else { 120 }
            || language == Language::Rust && source.len() > 64 * 1024
        {
            return Err(InvalidRequest);
        }
        platform.validate().map_err(|_| InvalidRequest)?;
        PreparedJob::new(
            language,
            source.clone(),
            BTreeMap::new(),
            execution_image_digest.clone(),
            execution_policy_revision.clone(),
            timeout_seconds,
        )?;
        let mut job = Self::new(
            source,
            preparer_image_digest,
            policy_revision,
            timeout_seconds,
        )?;
        job.revision = 2;
        job.language = language;
        job.platform = Some(platform);
        job.execution_image_digest = Some(execution_image_digest);
        job.execution_policy_revision = Some(execution_policy_revision);
        job.to_transport()?;
        Ok(job)
    }
    #[must_use]
    pub fn language(&self) -> Language {
        self.language
    }
    #[must_use]
    pub fn platform(&self) -> Option<&NativePlatform> {
        self.platform.as_ref()
    }
    #[must_use]
    pub fn native(&self) -> bool {
        self.revision == 2
    }
    #[must_use]
    pub fn matches_bundle(&self, bundle: &DependencyBundle) -> bool {
        match (self.platform.as_ref(), bundle.native()) {
            (None, None) => self.language == Language::Python,
            (Some(p), Some(b)) => {
                *p == b.record.platform
                    && self.language == b.record.language
                    && (self.language != Language::Rust
                        || b.record.payload["profile"]["preparation_image"].as_str()
                            == Some(self.preparer_image_digest.as_str()))
                    && self
                        .fingerprint()
                        .is_ok_and(|v| hex(&v) == b.record.preparation_sha256)
                    && hex(digest::digest(&digest::SHA256, self.source.as_bytes()).as_ref())
                        == b.record.source_sha256
                    && self.execution_image_digest.as_deref()
                        == Some(b.record.execution_image_digest.as_str())
                    && self.execution_policy_revision.as_deref()
                        == Some(b.record.execution_policy_revision.as_str())
            }
            _ => false,
        }
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
            #[serde(default, deserialize_with = "platform_field")]
            platform: Option<NativePlatform>,
            #[serde(default, deserialize_with = "string_field")]
            execution_image_digest: Option<String>,
            #[serde(default, deserialize_with = "string_field")]
            execution_policy_revision: Option<String>,
        }
        if bytes.len() > MAX_TRANSPORT_BYTES {
            return Err(InvalidRequest);
        }
        let wire: WireJob = serde_json::from_slice(bytes).map_err(|_| InvalidRequest)?;
        match (
            wire.revision,
            wire.language,
            wire.platform,
            wire.execution_image_digest,
            wire.execution_policy_revision,
        ) {
            (1, Language::Python, None, None, None) => Self::new(
                wire.source,
                wire.preparer_image_digest,
                wire.policy_revision,
                wire.timeout_seconds,
            ),
            (2, language, Some(platform), Some(image), Some(policy)) => Self::new_native(
                language,
                wire.source,
                wire.preparer_image_digest,
                wire.policy_revision,
                wire.timeout_seconds,
                platform,
                image,
                policy,
            ),
            _ => Err(InvalidRequest),
        }
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

    #[cfg(feature = "sandbox-supervisor")]
    #[test]
    fn manifest_uses_the_image_lock_for_cached_only_preparation() {
        use adk_sandbox::workspace::ManifestEntry;

        let image_cache = include_str!("../../../elitea-code-runner/Containerfile")
            .lines()
            .find(|line| line.contains("deno cache --frozen"))
            .expect("runner image must cache its pinned preparation dependencies");
        let image_lock = image_cache
            .split_ascii_whitespace()
            .find(|argument| argument.starts_with("--lock="))
            .expect("runner image must select its dependency lock");
        let manifest = job().manifest().unwrap();
        let runner = manifest
            .entries
            .iter()
            .find_map(|entry| match entry {
                ManifestEntry::File { path, content } if path == ".elitea-job.json" => {
                    Some(serde_json::from_slice::<serde_json::Value>(content).unwrap())
                }
                _ => None,
            })
            .expect("preparation manifest must contain the trusted runner request");
        let argv: Vec<&str> = runner["argv"]
            .as_array()
            .unwrap()
            .iter()
            .map(|argument| argument.as_str().unwrap())
            .collect();
        assert_eq!(argv[0], "/usr/local/bin/deno");
        assert_eq!(argv[1], "run");
        assert_eq!(
            argv.iter()
                .filter(|argument| **argument == image_lock)
                .count(),
            1
        );
        for restriction in [
            "--no-config",
            "--frozen",
            "--cached-only",
            "--no-prompt",
            "--deny-run",
            "--deny-ffi",
        ] {
            assert!(argv.contains(&restriction), "missing {restriction}");
        }
        assert_eq!(runner["timeout_seconds"], 30);
        assert_eq!(
            argv.last(),
            Some(&"/opt/elitea-code/python_preparation_job.mjs")
        );
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

fn platform_field<'de, D: serde::Deserializer<'de>>(
    d: D,
) -> Result<Option<NativePlatform>, D::Error> {
    NativePlatform::deserialize(d).map(Some)
}
fn string_field<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<String>, D::Error> {
    String::deserialize(d).map(Some)
}

#[cfg(test)]
mod native_tests {
    use super::*;
    #[test]
    fn native_preparation_fixture_retains_exact_typed_request_domain() {
        let job = PreparationJob::new_native(
            Language::TypeScript,
            "export default 42;".into(),
            format!("sha256:{}", "a".repeat(64)),
            "js-preparation-v2".into(),
            30,
            NativePlatform {
                os: "linux".into(),
                arch: "arm64".into(),
                abi: "gnu".into(),
            },
            format!("sha256:{}", "b".repeat(64)),
            "js-offline-v2".into(),
        )
        .unwrap();
        assert_eq!(
            hex(&job.fingerprint().unwrap()),
            "b43355889c53e4f8019d5997effa97027a82cd63d18f151faafc6294e99c5e1d"
        );
        let raw = include_bytes!("native-deno-v2.json");
        let bundle = DependencyBundle::parse_record(raw).unwrap();
        assert!(job.matches_bundle(&bundle));
        assert!(
            PreparationJob::from_transport(&job.to_transport().unwrap())
                .unwrap()
                .matches_bundle(&bundle)
        );
        let old = PreparationJob::new(
            "print(42)".into(),
            format!("sha256:{}", "a".repeat(64)),
            "python-v1".into(),
            30,
        )
        .unwrap();
        let mut v: serde_json::Value =
            serde_json::from_slice(&old.to_transport().unwrap()).unwrap();
        v["platform"] = serde_json::Value::Null;
        assert!(PreparationJob::from_transport(&serde_json::to_vec(&v).unwrap()).is_err());
    }
}
