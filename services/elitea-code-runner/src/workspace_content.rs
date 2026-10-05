//! Exact repository content identity. Main owns resource authorization and acquisition.
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::BTreeMap;

#[derive(Clone, Copy, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct WorkspacePolicy {
    pub revision: u32,
    pub max_files: u32,
    pub max_file_bytes: u64,
    pub max_total_bytes: u64,
    pub max_manifest_bytes: u32,
    pub max_path_bytes: u32,
    pub max_depth: u32,
    pub max_projections: u32,
    pub max_acquisition_seconds: u32,
}
impl Default for WorkspacePolicy {
    fn default() -> Self {
        Self {
            revision: 1,
            max_files: 1024,
            max_file_bytes: 1 << 20,
            max_total_bytes: 16 << 20,
            max_manifest_bytes: 512 << 10,
            max_path_bytes: 255,
            max_depth: 16,
            max_projections: 64,
            max_acquisition_seconds: 300,
        }
    }
}

#[derive(Clone, Copy, Eq, PartialEq, Deserialize, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum WorkspaceMode {
    Read,
    Readwrite,
}

/// Frozen selection has no credential, URL, actor, or project field.
#[derive(Clone, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct WorkspaceSelection {
    pub toolkit_id: i32,
    pub toolkit_reference_sha256: String,
    pub repository_id: String,
    pub commit: String,
    pub mode: WorkspaceMode,
    pub include: Vec<String>,
}

#[derive(Debug, Eq, PartialEq)]
pub enum WorkspaceError {
    Invalid,
    Bound { bound: &'static str, limit: u64 },
}
impl std::fmt::Display for WorkspaceError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Invalid => f.write_str("Code workspace identity or content is invalid"),
            Self::Bound { bound, limit } => {
                write!(f, "Code workspace exceeds operator policy {bound}={limit}")
            }
        }
    }
}
impl std::error::Error for WorkspaceError {}
type Result<T> = std::result::Result<T, WorkspaceError>;

fn hex(value: &[u8]) -> String {
    const HEX: &[u8] = b"0123456789abcdef";
    value
        .iter()
        .flat_map(|v| {
            [
                char::from(HEX[usize::from(v >> 4)]),
                char::from(HEX[usize::from(v & 15)]),
            ]
        })
        .collect()
}
fn valid_hex(value: &str, lengths: &[usize]) -> bool {
    lengths.contains(&value.len())
        && value
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
}
fn identity(domain: &'static [u8], canonical: &[u8]) -> String {
    let mut hash = Sha256::new();
    hash.update(domain);
    hash.update((canonical.len() as u64).to_be_bytes());
    hash.update(canonical);
    hex(&hash.finalize())
}
fn bound(value: u64, limit: u64, name: &'static str) -> Result<()> {
    if value > limit {
        return Err(WorkspaceError::Bound { bound: name, limit });
    }
    Ok(())
}
impl WorkspacePolicy {
    pub fn validate(&self) -> Result<()> {
        if self.revision != 1
            || !(1..=4096).contains(&self.max_files)
            || !(1..=4 << 20).contains(&self.max_file_bytes)
            || self.max_total_bytes < self.max_file_bytes
            || self.max_total_bytes > 128 << 20
            || !(1024..=4 << 20).contains(&self.max_manifest_bytes)
            || !(1..=1024).contains(&self.max_path_bytes)
            || !(1..=64).contains(&self.max_depth)
            || !(1..=128).contains(&self.max_projections)
            || !(1..=3600).contains(&self.max_acquisition_seconds)
        {
            return Err(WorkspaceError::Invalid);
        }
        Ok(())
    }
}

pub fn validate_path(value: &str, policy: &WorkspacePolicy) -> Result<()> {
    if value.is_empty() || value.starts_with('/') || value.ends_with('/') {
        return Err(WorkspaceError::Invalid);
    }
    bound(
        value.len() as u64,
        u64::from(policy.max_path_bytes),
        "max_path_bytes",
    )?;
    let parts = value.split('/').collect::<Vec<_>>();
    bound(parts.len() as u64, u64::from(policy.max_depth), "max_depth")?;
    for part in parts {
        let lower = part.to_ascii_lowercase();
        if part.is_empty()
            || matches!(part, "." | "..")
            || lower == ".git"
            || lower.starts_with(".elitea")
            || !part
                .bytes()
                .all(|v| v.is_ascii_alphanumeric() || b"._-+@".contains(&v))
        {
            return Err(WorkspaceError::Invalid);
        }
    }
    Ok(())
}
impl WorkspaceSelection {
    pub fn validate(&self, policy: &WorkspacePolicy) -> Result<()> {
        policy.validate()?;
        if self.toolkit_id <= 0
            || !valid_hex(&self.toolkit_reference_sha256, &[64])
            || !valid_hex(&self.commit, &[40, 64])
            || self.repository_id.is_empty()
            || self.repository_id.len() > 128
            || !self
                .repository_id
                .bytes()
                .all(|v| (0x21..=0x7e).contains(&v) && !b"/\\".contains(&v))
            || self.include.is_empty()
        {
            return Err(WorkspaceError::Invalid);
        }
        bound(
            self.include.len() as u64,
            u64::from(policy.max_projections),
            "max_projections",
        )?;
        for (index, path) in self.include.iter().enumerate() {
            validate_path(path, policy)?;
            if index > 0 && path.as_str() <= self.include[index - 1].as_str()
                || self.include[..index]
                    .iter()
                    .any(|earlier| path.starts_with(&format!("{earlier}/")))
            {
                return Err(WorkspaceError::Invalid);
            }
        }
        Ok(())
    }
    fn contains(&self, path: &str) -> bool {
        self.include
            .iter()
            .any(|include| path == include || path.starts_with(&format!("{include}/")))
    }
}

#[derive(Clone, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct WorkspaceFile {
    pub path: String,
    pub bytes: u64,
    pub sha256: String,
    pub executable: bool,
}
#[derive(Serialize)]
struct Content<'a> {
    revision: u32,
    selection: &'a WorkspaceSelection,
    policy: &'a WorkspacePolicy,
    files: &'a [WorkspaceFile],
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Record {
    revision: u32,
    selection: WorkspaceSelection,
    policy: WorkspacePolicy,
    files: Vec<WorkspaceFile>,
    root: String,
}

/// No Debug. Metadata can contain private repository file names.
pub struct WorkspaceManifest {
    record: Record,
}
impl WorkspaceManifest {
    pub fn from_transport(bytes: &[u8], root: &str, policy: &WorkspacePolicy) -> Result<Self> {
        policy.validate()?;
        bound(
            bytes.len() as u64,
            u64::from(policy.max_manifest_bytes),
            "max_manifest_bytes",
        )?;
        if !valid_hex(root, &[64]) {
            return Err(WorkspaceError::Invalid);
        }
        let record: Record = serde_json::from_slice(bytes).map_err(|_| WorkspaceError::Invalid)?;
        let content = &record;
        if content.revision != 1
            || record.root != root
            || content.policy != *policy
            || content.files.is_empty()
        {
            return Err(WorkspaceError::Invalid);
        }
        content.selection.validate(policy)?;
        bound(
            content.files.len() as u64,
            u64::from(policy.max_files),
            "max_files",
        )?;
        let mut total = 0_u64;
        let mut content_sizes = BTreeMap::new();
        for (index, file) in content.files.iter().enumerate() {
            validate_path(&file.path, policy)?;
            if content_sizes
                .insert(file.sha256.as_str(), file.bytes)
                .is_some_and(|size| size != file.bytes)
            {
                return Err(WorkspaceError::Invalid);
            }
            bound(file.bytes, policy.max_file_bytes, "max_file_bytes")?;
            total = total
                .checked_add(file.bytes)
                .ok_or(WorkspaceError::Invalid)?;
            bound(total, policy.max_total_bytes, "max_total_bytes")?;
            if !content.selection.contains(&file.path)
                || !valid_hex(&file.sha256, &[64])
                || index > 0 && file.path.as_str() <= content.files[index - 1].path.as_str()
                || content.files[..index]
                    .iter()
                    .any(|earlier| file.path.starts_with(&format!("{}/", earlier.path)))
            {
                return Err(WorkspaceError::Invalid);
            }
        }
        let canonical = serde_json::to_vec(&Content {
            revision: content.revision,
            selection: &content.selection,
            policy: &content.policy,
            files: &content.files,
        })
        .map_err(|_| WorkspaceError::Invalid)?;
        if identity(b"elitea.code.workspace-manifest.v1\0", &canonical) != root
            || serde_json::to_vec(&record).map_err(|_| WorkspaceError::Invalid)? != bytes
        {
            return Err(WorkspaceError::Invalid);
        }
        Ok(Self { record })
    }
    pub fn root(&self) -> &str {
        &self.record.root
    }
    pub fn selection(&self) -> &WorkspaceSelection {
        &self.record.selection
    }
    pub fn files(&self) -> &[WorkspaceFile] {
        &self.record.files
    }
    // Hydration checks this policy; language binaries verify only retained entries.
    #[allow(dead_code)]
    pub fn policy(&self) -> &WorkspacePolicy {
        &self.record.policy
    }
    pub fn from_bound_transport(bytes: &[u8], root: &str) -> Result<Self> {
        if bytes.len() > 4 << 20 {
            return Err(WorkspaceError::Invalid);
        }
        let record: Record = serde_json::from_slice(bytes).map_err(|_| WorkspaceError::Invalid)?;
        record.policy.validate()?;
        Self::from_transport(bytes, root, &record.policy)
    }
}

// Language entrypoints use this binding after PID-1 completes hydration.
#[allow(dead_code)]
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WorkspaceBinding {
    pub revision: u32,
    pub selection: WorkspaceSelection,
    pub manifest_sha256: String,
    pub policy_sha256: String,
}
impl WorkspaceBinding {
    // PID-1 validates manifests directly; language entrypoints compare this binding.
    #[allow(dead_code)]
    pub fn matches(&self, manifest: &WorkspaceManifest) -> Result<bool> {
        if self.revision != 1
            || !valid_hex(&self.manifest_sha256, &[64])
            || !valid_hex(&self.policy_sha256, &[64])
        {
            return Err(WorkspaceError::Invalid);
        }
        let policy =
            serde_json::to_vec(&manifest.record.policy).map_err(|_| WorkspaceError::Invalid)?;
        Ok(self.selection == manifest.record.selection
            && self.manifest_sha256 == manifest.record.root
            && self.policy_sha256 == identity(b"elitea.code.workspace-policy.v1\0", &policy))
    }
}
