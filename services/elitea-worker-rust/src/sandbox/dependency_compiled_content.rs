//! Fixed compiled content RPCs. Selected entries never become an ordinary miss.
use super::*;
use crate::{
    protocol::elitea::runtime::v1::{
        FetchCompiledSnapshotFileRequestV1, FetchCompiledSnapshotFileResponseV1,
        GetCompiledSnapshotRequestV1, PublishCompiledSnapshotRequestV1,
        RustCompiledSnapshotGrantClaimsV1, RustCompiledSnapshotPurposeV1,
        UploadCompiledSnapshotFileHeaderV1, UploadCompiledSnapshotFileRequestV1,
        upload_compiled_snapshot_file_request_v1,
    },
    sandbox::compiled_snapshot::{ContentSha256, DESCRIPTOR_LIMIT, Descriptor},
};
use std::os::unix::fs::MetadataExt as _;
/// The one file a compiled snapshot holds.
const EXECUTABLE_NAME: &str = "elitea-code-job";
/// The protobuf encoding of a compiled-snapshot grant for `root`, after a purpose check.
fn authority(
    grant: &SignedSandboxJobGrantV1,
    root: &str,
    purpose: RustCompiledSnapshotPurposeV1,
) -> Result<Vec<u8>, DependencyContentError> {
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
    Ok(grant.encode_to_vec())
}
fn file(descriptor: &Descriptor) -> BundleFile {
    BundleFile {
        name: EXECUTABLE_NAME.into(),
        bytes: descriptor.executable_bytes,
        sha256: descriptor.executable_sha256.as_str().into(),
    }
}
fn compiled_parts(
    stream: tonic::Streaming<FetchCompiledSnapshotFileResponseV1>,
) -> impl Stream<Item = Result<Option<FilePart>, Status>> {
    stream.map(|message| message.map(|message| message.part.map(FilePart::from)))
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
        let signed_grant = authority(grant, root.as_str(), RustCompiledSnapshotPurposeV1::Read)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        tokio::time::timeout(self.deadline, async {
            let metadata = self
                .rpc()
                .get_compiled_snapshot(GetCompiledSnapshotRequestV1 {
                    root: root.as_str().to_owned(),
                    signed_grant: signed_grant.clone(),
                })
                .await
                .map_err(rpc_error)?
                .into_inner()
                .descriptor_json;
            if metadata.len() > DESCRIPTOR_LIMIT || metadata != canonical {
                return Err(DependencyContentError::Integrity);
            }
            let directory = self.stage_compiled()?;
            let parts = self
                .rpc()
                .fetch_compiled_snapshot_file(FetchCompiledSnapshotFileRequestV1 {
                    root: root.as_str().to_owned(),
                    name: EXECUTABLE_NAME.to_owned(),
                    signed_grant,
                })
                .await
                .map_err(rpc_error)?
                .into_inner();
            let expected = file(descriptor);
            write_verified_file(compiled_parts(parts), &expected, directory.path()).await?;
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
        let signed_grant = authority(grant, root.as_str(), RustCompiledSnapshotPurposeV1::Publish)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        tokio::time::timeout(self.deadline, async {
            let opened = self.verify_compiled_export(directory, descriptor).await?;
            let failure = Arc::new(Mutex::new(None));
            let chunks = file_chunks(opened, descriptor.executable_bytes, Arc::clone(&failure))
                .map(|chunk| UploadCompiledSnapshotFileRequestV1 {
                    part: Some(upload_compiled_snapshot_file_request_v1::Part::Chunk(chunk)),
                });
            let first = UploadCompiledSnapshotFileRequestV1 {
                part: Some(upload_compiled_snapshot_file_request_v1::Part::Header(
                    UploadCompiledSnapshotFileHeaderV1 {
                        root: root.as_str().to_owned(),
                        name: EXECUTABLE_NAME.to_owned(),
                        signed_grant,
                        descriptor_json: canonical.to_vec(),
                    },
                )),
            };
            let result = self
                .rpc()
                .upload_compiled_snapshot_file(tokio_stream::once(first).chain(chunks))
                .await;
            take_read_failure(&failure)?;
            result.map(|_| ()).map_err(rpc_error)
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
        let signed_grant = authority(grant, root.as_str(), RustCompiledSnapshotPurposeV1::Publish)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        tokio::time::timeout(self.deadline, async {
            self.rpc()
                .publish_compiled_snapshot(PublishCompiledSnapshotRequestV1 {
                    root: root.as_str().to_owned(),
                    signed_grant,
                    descriptor_json: canonical.to_vec(),
                })
                .await
                .map(|_| ())
                .map_err(rpc_error)
        })
        .await
        .map_err(|_| DependencyContentError::Timeout)?
    }
}
