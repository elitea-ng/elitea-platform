//! Fixed compiled content routes. Selected entries never become an ordinary miss.
use super::*;
use crate::{
    protocol::elitea::runtime::v1::{
        RustCompiledSnapshotGrantClaimsV1, RustCompiledSnapshotPurposeV1,
    },
    sandbox::compiled_snapshot::{ContentSha256, DESCRIPTOR_LIMIT, Descriptor},
};
use std::os::unix::fs::MetadataExt as _;
const HEADER: &str = "X-Elitea-Sandbox-Compiled-Grant";
fn authority(
    grant: &SignedSandboxJobGrantV1,
    root: &str,
    purpose: RustCompiledSnapshotPurposeV1,
) -> Result<HeaderValue, DependencyContentError> {
    if grant.signature.len() != 64 || grant.claims_bytes.len() > 4096 || grant.key_id.is_empty() {
        return Err(DependencyContentError::Authority);
    }
    scan_message(&grant.claims_bytes, Schema::CompiledSnapshotGrant)
        .map_err(|_| DependencyContentError::Authority)?;
    let claims = RustCompiledSnapshotGrantClaimsV1::decode(grant.claims_bytes.as_slice())
        .map_err(|_| DependencyContentError::Authority)?;
    if claims.revision != 4
        || claims.purpose != purpose as i32
        || hex(&claims.descriptor_sha256) != root
        || chrono::Utc::now().timestamp_millis() >= claims.expires_at_unix_millis
    {
        return Err(DependencyContentError::Authority);
    }
    HeaderValue::from_str(&STANDARD.encode(grant.encode_to_vec()))
        .map_err(|_| DependencyContentError::Authority)
}
fn file(descriptor: &Descriptor) -> BundleFile {
    BundleFile {
        name: "elitea-code-job".into(),
        bytes: descriptor.executable_bytes,
        sha256: descriptor.executable_sha256.as_str().into(),
    }
}
impl DependencyContentClient {
    pub(crate) fn stage_compiled(&self) -> Result<tempfile::TempDir, DependencyContentError> {
        tempfile::Builder::new()
            .prefix("compiled-")
            .permissions(std::fs::Permissions::from_mode(0o700))
            .tempdir_in(&self.staging)
            .map_err(DependencyContentError::Staging)
    }
    pub(crate) async fn download_compiled(
        &self,
        descriptor: &Descriptor,
        canonical: &[u8],
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<(tempfile::TempDir, fs::File), DependencyContentError> {
        let root = ContentSha256::of(canonical);
        let header = authority(grant, root.as_str(), RustCompiledSnapshotPurposeV1::Read)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        tokio::time::timeout(self.deadline, async {
            let mut metadata = self
                .client
                .get(format!(
                    "{}/sandbox-compiled-snapshots/{}",
                    self.origin,
                    root.as_str()
                ))
                .header(HEADER, header.clone())
                .send()
                .await
                .map_err(transport)?;
            check_response(&metadata, StatusCode::OK, "application/json", None)?;
            let mut bytes = Vec::new();
            while let Some(chunk) = metadata.chunk().await.map_err(transport)? {
                if bytes.len().saturating_add(chunk.len()) > DESCRIPTOR_LIMIT {
                    return Err(DependencyContentError::Integrity);
                }
                bytes.extend_from_slice(&chunk);
            }
            if bytes != canonical {
                return Err(DependencyContentError::Integrity);
            }
            let directory = self.stage_compiled()?;
            let response = self
                .client
                .get(format!(
                    "{}/sandbox-compiled-snapshots/{}/files/elitea-code-job",
                    self.origin,
                    root.as_str()
                ))
                .header(HEADER, header)
                .send()
                .await
                .map_err(transport)?;
            let expected = file(descriptor);
            write_verified_file(response, &expected, directory.path()).await?;
            let mut opened = open_verified_file(directory.path(), &expected).await?;
            opened
                .rewind()
                .await
                .map_err(DependencyContentError::Staging)?;
            Ok((directory, opened))
        })
        .await
        .map_err(|_| DependencyContentError::Timeout)?
    }
    pub(crate) async fn verify_compiled_export(
        &self,
        directory: &Path,
        descriptor: &Descriptor,
    ) -> Result<fs::File, DependencyContentError> {
        let mut opened = open_verified_file(directory, &file(descriptor)).await?;
        if opened
            .metadata()
            .await
            .map_err(DependencyContentError::Staging)?
            .nlink()
            != 1
        {
            return Err(DependencyContentError::Integrity);
        }
        opened
            .rewind()
            .await
            .map_err(DependencyContentError::Staging)?;
        Ok(opened)
    }
    pub(crate) async fn stage_compiled_publication(
        &self,
        directory: &Path,
        descriptor: &Descriptor,
        canonical: &[u8],
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<(), DependencyContentError> {
        let root = ContentSha256::of(canonical);
        let header = authority(grant, root.as_str(), RustCompiledSnapshotPurposeV1::Publish)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        tokio::time::timeout(self.deadline, async {
            let opened = self.verify_compiled_export(directory, descriptor).await?;
            let body = reqwest::Body::wrap_stream(file_stream(opened, descriptor.executable_bytes));
            let form = reqwest::multipart::Form::new()
                .part(
                    "descriptor",
                    reqwest::multipart::Part::bytes(canonical.to_vec())
                        .mime_str("application/json")
                        .map_err(transport)?,
                )
                .part(
                    "file",
                    reqwest::multipart::Part::stream_with_length(body, descriptor.executable_bytes)
                        .file_name("elitea-code-job")
                        .mime_str("application/octet-stream")
                        .map_err(transport)?,
                );
            let response = self
                .client
                .put(format!(
                    "{}/sandbox-compiled-snapshots/{}/files/elitea-code-job",
                    self.origin,
                    root.as_str()
                ))
                .header(HEADER, header)
                .multipart(form)
                .send()
                .await
                .map_err(transport)?;
            check_status(&response, StatusCode::NO_CONTENT)
        })
        .await
        .map_err(|_| DependencyContentError::Timeout)?
    }
    pub(crate) async fn publish_compiled_ready(
        &self,
        canonical: &[u8],
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<(), DependencyContentError> {
        let root = ContentSha256::of(canonical);
        let header = authority(grant, root.as_str(), RustCompiledSnapshotPurposeV1::Publish)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        tokio::time::timeout(self.deadline, async {
            let response = self
                .client
                .post(format!(
                    "{}/sandbox-compiled-snapshots/{}",
                    self.origin,
                    root.as_str()
                ))
                .header(HEADER, header)
                .header(CONTENT_TYPE, "application/json")
                .body(canonical.to_vec())
                .send()
                .await
                .map_err(transport)?;
            check_status(&response, StatusCode::NO_CONTENT)
        })
        .await
        .map_err(|_| DependencyContentError::Timeout)?
    }
}
