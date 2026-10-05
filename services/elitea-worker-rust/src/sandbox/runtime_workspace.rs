//! Fixed helper protocol. The signed prepared request and supervisor lease authorize use.
use adk_sandbox::{
    SandboxError,
    workspace::{Manifest, ManifestEntry, docker::CodeJobIdentity},
};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy)]
pub enum WorkspaceOperation {
    Manifest,
    Write,
    Probe,
    Finalize,
    Release,
}
impl WorkspaceOperation {
    #[must_use]
    pub fn command(self) -> &'static str {
        match self {
            Self::Manifest => "--workspace-manifest",
            Self::Write => "--workspace-write",
            Self::Probe => "--workspace-probe",
            Self::Finalize => "--workspace-finalize",
            Self::Release => "--workspace-release",
        }
    }
}
#[derive(Serialize)]
struct Header<'a> {
    revision: u8,
    job_key: &'a str,
    request_digest: &'a str,
    manifest_sha256: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pod_uid: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    index: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    bytes: Option<u64>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WorkspaceProbe {
    pub revision: u8,
    pub next_index: u32,
    pub ready: bool,
}
pub const MAX_WORKSPACE_BATCH_FILES: usize = 32;
pub const MAX_WORKSPACE_BATCH_BYTES: u64 = 4 << 20;
pub struct WorkspaceBatch {
    pub start: usize,
    pub end: usize,
    pub finalize: bool,
}
impl WorkspaceBatch {
    /// # Errors
    /// Returns `SandboxError::ExecutionFailed` for an invalid workspace plan, frame, or launch binding.
    pub fn plan(
        manifest: &super::workspace::WorkspaceManifest,
        requested: u32,
        confirmed: u32,
    ) -> Result<Self, SandboxError> {
        let count = manifest.files().len();
        let start = confirmed as usize;
        let requested = requested as usize;
        if count == 0 || start > count || requested > start {
            return Err(unavailable());
        }
        if requested < start {
            return Ok(Self {
                start,
                end: start,
                finalize: false,
            });
        }
        if start == count {
            return Ok(Self {
                start,
                end: start,
                finalize: true,
            });
        }
        let mut end = start;
        let mut bytes = 0u64;
        for file in manifest
            .files()
            .iter()
            .skip(start)
            .take(MAX_WORKSPACE_BATCH_FILES)
        {
            let next = bytes.checked_add(file.bytes).ok_or_else(unavailable)?;
            if next > MAX_WORKSPACE_BATCH_BYTES {
                break;
            }
            end += 1;
            bytes = next;
        }
        if end == start {
            return Err(unavailable());
        }
        Ok(Self {
            start,
            end,
            finalize: false,
        })
    }
}
pub struct WorkspaceVolumeIdentity {
    pub backend: super::ledger::WorkspaceBackend,
    pub volume_name: String,
    pub owner_token: String,
    pub creation_token: String,
}
impl WorkspaceVolumeIdentity {
    /// # Errors
    /// Returns `LedgerError::Invalid` if the original runtime or receipt scope is invalid.
    pub fn receipt(
        self,
        scope: &super::ledger::JobScope,
        identity: &CodeJobIdentity,
        activation: &str,
        root: &str,
    ) -> Result<super::ledger::WorkspaceRuntimeReceipt, super::ledger::LedgerError> {
        let value = super::ledger::WorkspaceRuntimeReceipt {
            revision: 1,
            backend: self.backend,
            project_id: scope.project,
            job_key: identity.job_key().into(),
            request_digest: identity.request_digest().into(),
            activation_id: activation.into(),
            original_runtime_id: identity
                .runtime_id()
                .ok_or(super::ledger::LedgerError::Invalid)?
                .into(),
            volume_name: self.volume_name,
            volume_owner_token: self.owner_token,
            volume_creation_token: self.creation_token,
            manifest_sha256: root.into(),
        };
        value.validate_scope(scope, &value.original_runtime_id)?;
        Ok(value)
    }
}
pub(crate) fn unavailable() -> SandboxError {
    SandboxError::ExecutionFailed("Code repository hydration is unavailable or unconfirmed".into())
}
fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
}
/// # Errors
/// Returns `SandboxError::ExecutionFailed` for an invalid workspace plan, frame, or launch binding.
pub fn header(
    identity: &CodeJobIdentity,
    root: &str,
    operation: WorkspaceOperation,
    index: Option<u32>,
    payload_bytes: usize,
    pod_uid: Option<&str>,
) -> Result<Vec<u8>, SandboxError> {
    if !valid_digest(root)
        || identity.runtime_id().is_none()
        || payload_bytes > 4 << 20
        || pod_uid
            .is_some_and(|v| v.is_empty() || v.len() > 512 || v.chars().any(char::is_whitespace))
    {
        return Err(unavailable());
    }
    let bytes = match operation {
        WorkspaceOperation::Manifest if index.is_none() && payload_bytes > 0 => {
            Some(payload_bytes as u64)
        }
        WorkspaceOperation::Write if index.is_some_and(|v| v < 4096) => Some(payload_bytes as u64),
        WorkspaceOperation::Probe | WorkspaceOperation::Finalize | WorkspaceOperation::Release
            if index.is_none() && payload_bytes == 0 =>
        {
            None
        }
        _ => return Err(unavailable()),
    };
    let mut output = serde_json::to_vec(&Header {
        revision: 1,
        job_key: identity.job_key(),
        request_digest: identity.request_digest(),
        manifest_sha256: root,
        pod_uid,
        index,
        bytes,
    })
    .map_err(|_| unavailable())?;
    if output.len() > 4095 {
        return Err(unavailable());
    }
    output.push(b'\n');
    Ok(output)
}
/// # Errors
/// Returns `SandboxError::ExecutionFailed` for an invalid workspace plan, frame, or launch binding.
pub fn validate_launch_manifest(manifest: &Manifest) -> Result<(), SandboxError> {
    if manifest.entries.len() != 2 {
        return Err(unavailable());
    }
    let mut code = false;
    let mut job = false;
    for entry in &manifest.entries {
        match entry {
            ManifestEntry::File { path, content }
                if path == ".elitea-code.json"
                    && !code
                    && !content.is_empty()
                    && content.len() <= 1024 * 1024 =>
            {
                code = true;
            }
            ManifestEntry::File { path, content }
                if path == ".elitea-job.json"
                    && !job
                    && !content.is_empty()
                    && content.len() <= 64 * 1024 =>
            {
                job = true;
            }
            _ => return Err(unavailable()),
        }
    }
    if !code || !job {
        return Err(unavailable());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn manifest(count: usize, bytes: u64) -> super::super::workspace::WorkspaceManifest {
        use super::super::workspace::*;
        WorkspaceManifest::fixture(
            WorkspaceSelection {
                toolkit_id: 12,
                toolkit_reference_sha256: "1".repeat(64),
                repository_id: "123".into(),
                commit: "2".repeat(40),
                mode: WorkspaceMode::Read,
                include: vec!["src".into()],
            },
            WorkspacePolicy::default(),
            (0..count)
                .map(|i| WorkspaceFile {
                    path: format!("src/{i:04}.txt"),
                    bytes,
                    sha256: "a".repeat(64),
                    executable: false,
                })
                .collect(),
        )
        .unwrap()
    }
    #[test]
    fn batches_bound_files_bytes_and_require_exact_confirmed_cursor_and_explicit_finalization() {
        let empty_files = manifest(33, 0);
        let first = WorkspaceBatch::plan(&empty_files, 0, 0).unwrap();
        assert_eq!((first.start, first.end, first.finalize), (0, 32, false));
        let last = WorkspaceBatch::plan(&empty_files, 32, 32).unwrap();
        assert_eq!((last.start, last.end, last.finalize), (32, 33, false));
        let replay = WorkspaceBatch::plan(&empty_files, 0, 33).unwrap();
        assert_eq!((replay.start, replay.end, replay.finalize), (33, 33, false));
        assert!(WorkspaceBatch::plan(&empty_files, 33, 32).is_err());
        assert!(WorkspaceBatch::plan(&empty_files, 34, 34).is_err());
        assert!(WorkspaceBatch::plan(&empty_files, 33, 33).unwrap().finalize);
        let bounded = manifest(5, 1 << 20);
        assert_eq!(WorkspaceBatch::plan(&bounded, 0, 0).unwrap().end, 4);
        assert_eq!(WorkspaceBatch::plan(&bounded, 4, 4).unwrap().end, 5);
    }
    #[test]
    fn fixed_header_cannot_select_paths_credentials_or_skip_invalid_operations() {
        let identity = CodeJobIdentity::new("a".repeat(64), "b".repeat(64))
            .unwrap()
            .with_runtime_id("original-runtime".into())
            .unwrap();
        let result = header(
            &identity,
            &"c".repeat(64),
            WorkspaceOperation::Write,
            Some(0),
            5,
            None,
        )
        .unwrap();
        assert_eq!(result,format!("{{\"revision\":1,\"job_key\":\"{}\",\"request_digest\":\"{}\",\"manifest_sha256\":\"{}\",\"index\":0,\"bytes\":5}}\n","a".repeat(64),"b".repeat(64),"c".repeat(64)).into_bytes());
        assert!(
            header(
                &identity,
                &"c".repeat(64),
                WorkspaceOperation::Write,
                Some(4096),
                5,
                None
            )
            .is_err()
        );
        assert!(
            header(
                &identity,
                &"c".repeat(64),
                WorkspaceOperation::Probe,
                None,
                1,
                None
            )
            .is_err()
        );
        assert!(
            header(
                &identity,
                &"c".repeat(64),
                WorkspaceOperation::Manifest,
                None,
                0,
                None
            )
            .is_err()
        );
    }
}
