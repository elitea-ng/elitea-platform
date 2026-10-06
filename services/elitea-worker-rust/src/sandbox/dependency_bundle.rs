//! Shared metadata identity; package bytes stay on the supervisor data plane.
use ring::digest;
use serde::{Deserialize, Serialize};
const METADATA_LIMIT: usize = 128 * 1024;
const FILE_LIMIT: u64 = 32 * 1024 * 1024;
const CONTENT_LIMIT: u64 = 128 * 1024 * 1024;
const LOCK_NAME: &str = "elitea-python-lock.json";
#[derive(Debug, thiserror::Error)]
#[error("Dependency metadata does not match its recorded content identity.")]
pub struct InvalidDependencyBundle;

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct DependencyBundleFile {
    pub(crate) name: String,
    pub(crate) bytes: u64,
    pub(crate) sha256: String,
}

#[derive(Serialize)]
pub(crate) struct BundleContent<'a> {
    pub(crate) revision: u32,
    pub(crate) runtime: &'a str,
    pub(crate) requirements: &'a [String],
    pub(crate) files: &'a [DependencyBundleFile],
}

/// Only metadata matching an independently recorded root can construct this value.
pub struct PythonDependencyBundle {
    pub(crate) files: Vec<DependencyBundleFile>,
    root: String,
    pub(crate) canonical: Vec<u8>,
}

impl PythonDependencyBundle {
    /// # Errors
    /// Rejects malformed metadata, unsafe names, excessive content, and root mismatches.
    pub fn parse(bytes: &[u8], expected_root: &str) -> Result<Self, InvalidDependencyBundle> {
        #[derive(Clone, Deserialize, Serialize)]
        #[serde(deny_unknown_fields)]
        struct Record {
            revision: u32,
            runtime: String,
            requirements: Vec<String>,
            files: Vec<DependencyBundleFile>,
            digest: String,
        }
        if bytes.len() > METADATA_LIMIT || !valid_digest(expected_root) {
            return Err(InvalidDependencyBundle);
        }
        let record: Record = serde_json::from_slice(bytes).map_err(|_| InvalidDependencyBundle)?;
        if record.revision != 1
            || record.runtime != "pyodide-0.29.0"
            || record.digest != expected_root
            || record.requirements.len() > 128
            || record.files.is_empty()
            || record.files.len() > 257
            || record.requirements.iter().any(|value| {
                value.is_empty()
                    || value.len() > 256
                    || !value.bytes().all(|byte| (0x20..=0x7e).contains(&byte))
            })
        {
            return Err(InvalidDependencyBundle);
        }
        let mut total = 0;
        let mut lock_found = false;
        for (index, file) in record.files.iter().enumerate() {
            let limit = if file.name == LOCK_NAME {
                lock_found = true;
                1024 * 1024
            } else {
                FILE_LIMIT
            };
            if !safe_name(&file.name)
                || !valid_digest(&file.sha256)
                || file.bytes > limit
                || index > 0 && file.name <= record.files[index - 1].name
            {
                return Err(InvalidDependencyBundle);
            }
            total += file.bytes;
        }
        if !lock_found || total > CONTENT_LIMIT {
            return Err(InvalidDependencyBundle);
        }
        let content = serde_json::to_vec(&BundleContent {
            revision: record.revision,
            runtime: &record.runtime,
            requirements: &record.requirements,
            files: &record.files,
        })
        .map_err(|_| InvalidDependencyBundle)?;
        if hex(digest::digest(&digest::SHA256, &content).as_ref()) != expected_root {
            return Err(InvalidDependencyBundle);
        }
        let canonical = serde_json::to_vec(&record).map_err(|_| InvalidDependencyBundle)?;
        if canonical.len() > METADATA_LIMIT {
            return Err(InvalidDependencyBundle);
        }
        Ok(Self {
            files: record.files,
            root: record.digest,
            canonical,
        })
    }

    #[must_use]
    pub fn root(&self) -> &str {
        &self.root
    }

    /// Canonical metadata only. Package bytes remain on the data plane.
    #[must_use]
    pub fn record_json(&self) -> &[u8] {
        &self.canonical
    }
}

impl DependencyBundleFile {
    #[must_use]
    pub fn name(&self) -> &str {
        &self.name
    }
    #[must_use]
    pub fn bytes(&self) -> u64 {
        self.bytes
    }
    #[must_use]
    pub fn sha256(&self) -> &str {
        &self.sha256
    }
}
impl PythonDependencyBundle {
    /// Validate the initial authenticated preparation receipt, including its content digest.
    /// Subsequent calls must retain and compare this recorded identity.
    /// # Errors
    /// Returns an invalid bundle for malformed or mismatched metadata.
    pub fn parse_record(bytes: &[u8]) -> Result<Self, InvalidDependencyBundle> {
        #[derive(Deserialize)]
        struct RecordedRoot {
            digest: String,
        }
        if bytes.len() > METADATA_LIMIT {
            return Err(InvalidDependencyBundle);
        }
        let record: RecordedRoot =
            serde_json::from_slice(bytes).map_err(|_| InvalidDependencyBundle)?;
        Self::parse(bytes, &record.digest)
    }
    #[must_use]
    pub fn files(&self) -> &[DependencyBundleFile] {
        &self.files
    }
    #[must_use]
    pub fn file_count(&self) -> usize {
        self.files.len()
    }
}
pub(crate) fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}
pub(crate) fn safe_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && (value == LOCK_NAME || value.strip_suffix(".whl").is_some())
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'+' | b'-'))
}
pub(crate) fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(char::from(DIGITS[usize::from(byte >> 4)]));
        output.push(char::from(DIGITS[usize::from(byte & 15)]));
    }
    output
}

/// Native metadata is additive. Python construction and canonical bytes stay unchanged.
pub enum DependencyBundle {
    PythonV1(PythonDependencyBundle),
    NativeV2(Box<super::native_bundle::NativeDependencyBundle>),
}
impl DependencyBundle {
    /// Parse bounded metadata and verify its recorded content identity.
    /// # Errors
    /// Rejects unsupported revisions, malformed metadata, and root mismatches.
    pub fn parse(bytes: &[u8], root: &str) -> Result<Self, InvalidDependencyBundle> {
        #[derive(Deserialize)]
        struct Version {
            revision: u8,
        }
        if bytes.len() > METADATA_LIMIT {
            return Err(InvalidDependencyBundle);
        }
        let v: Version = serde_json::from_slice(bytes).map_err(|_| InvalidDependencyBundle)?;
        match v.revision {
            1 => PythonDependencyBundle::parse(bytes, root).map(Self::PythonV1),
            2 => super::native_bundle::NativeDependencyBundle::parse(bytes, root)
                .map(Box::new)
                .map(Self::NativeV2),
            _ => Err(InvalidDependencyBundle),
        }
    }
    /// Parse the initial authenticated receipt and retain its content identity.
    /// # Errors
    /// Rejects excessive or invalid metadata and inconsistent recorded roots.
    pub fn parse_record(bytes: &[u8]) -> Result<Self, InvalidDependencyBundle> {
        #[derive(Deserialize)]
        struct Root {
            digest: String,
        }
        if bytes.len() > METADATA_LIMIT {
            return Err(InvalidDependencyBundle);
        }
        let r: Root = serde_json::from_slice(bytes).map_err(|_| InvalidDependencyBundle)?;
        Self::parse(bytes, &r.digest)
    }
    #[must_use]
    pub fn root(&self) -> &str {
        match self {
            Self::PythonV1(v) => v.root(),
            Self::NativeV2(v) => v.root(),
        }
    }
    #[must_use]
    pub fn files(&self) -> &[DependencyBundleFile] {
        match self {
            Self::PythonV1(v) => v.files(),
            Self::NativeV2(v) => v.files(),
        }
    }
    #[must_use]
    pub fn file_count(&self) -> usize {
        self.files().len()
    }
    #[must_use]
    pub fn record_json(&self) -> &[u8] {
        match self {
            Self::PythonV1(v) => v.record_json(),
            Self::NativeV2(v) => v.record_json(),
        }
    }
    #[must_use]
    pub fn route(&self) -> &str {
        match self {
            Self::PythonV1(_) => "sandbox-bundles",
            Self::NativeV2(_) => "sandbox-native-bundles",
        }
    }
    #[must_use]
    pub fn metadata_name(&self) -> &str {
        match self {
            Self::PythonV1(_) => "elitea-python-bundle.json",
            Self::NativeV2(_) => "elitea-native-bundle-v2.json",
        }
    }
    #[must_use]
    pub fn proof_name(&self) -> &str {
        match self {
            Self::PythonV1(_) => "elitea-python-bundle.json",
            Self::NativeV2(_) => "elitea-native-ready-v2.json",
        }
    }
    #[must_use]
    pub fn native(&self) -> Option<&super::native_bundle::NativeDependencyBundle> {
        match self {
            Self::NativeV2(v) => Some(v),
            Self::PythonV1(_) => None,
        }
    }
}
impl From<PythonDependencyBundle> for DependencyBundle {
    fn from(v: PythonDependencyBundle) -> Self {
        Self::PythonV1(v)
    }
}
