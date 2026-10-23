//! One root-bound transfer per worker RPC. Retries never resolve new packages.
use super::*;

impl DependencyContentClient {
    /// Read canonical native metadata at one signed immutable Cargo root.
    /// # Errors
    /// Returns authority, integrity, or storage errors. Only `NotFound` is absent.
    pub(crate) async fn lookup_native(
        &self,
        root: &str,
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<Option<DependencyBundle>, DependencyContentError> {
        let authority = grant_bytes(grant, root)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            let response = self
                .rpc()
                .get_sandbox_bundle(GetSandboxBundleRequestV1 {
                    kind: SandboxBundleKindV1::Native.into(),
                    root: root.to_owned(),
                    signed_grant: authority,
                })
                .await
                .map(|response| response.into_inner().bundle_json);
            decode_native_lookup(response, root)
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| DependencyContentError::Timeout)?
    }

    /// Confirm exact published metadata before accessing a retained preparation runtime.
    /// # Errors
    /// Rejects changed metadata, invalid authority, and storage failures.
    pub async fn confirm_publication(
        &self,
        bundle: &DependencyBundle,
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<bool, DependencyContentError> {
        let authority = grant_bytes(grant, bundle.root())?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            let metadata = match self
                .rpc()
                .get_sandbox_bundle(GetSandboxBundleRequestV1 {
                    kind: bundle.kind().into(),
                    root: bundle.root().to_owned(),
                    signed_grant: authority,
                })
                .await
            {
                Ok(response) => response.into_inner().bundle_json,
                Err(status) if status.code() == Code::NotFound => return Ok(false),
                Err(status) => return Err(rpc_error(status)),
            };
            bounded_metadata(&metadata)?;
            let published = DependencyBundle::parse(&metadata, bundle.root())?;
            if published.record_json() != bundle.record_json() {
                return Err(DependencyContentError::Integrity);
            }
            Ok(true)
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| DependencyContentError::Timeout)?
    }

    /// Download one recorded file and return its verified, rewound descriptor.
    /// The caller retains the private staging owner until import ends.
    /// # Errors
    /// Rejects invalid indexes, changed content, invalid authority, and storage failures.
    pub(crate) async fn download_index(
        &self,
        bundle: &DependencyBundle,
        index: usize,
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<(tempfile::TempDir, fs::File), DependencyContentError> {
        let file = bundle
            .files()
            .get(index)
            .ok_or(DependencyContentError::Integrity)?;
        let authority = grant_bytes(grant, bundle.root())?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            let directory = self.stage_file(bundle, index)?;
            let parts = self
                .rpc()
                .fetch_sandbox_bundle_file(FetchSandboxBundleFileRequestV1 {
                    kind: bundle.kind().into(),
                    root: bundle.root().to_owned(),
                    name: file.name().to_owned(),
                    signed_grant: authority,
                })
                .await
                .map_err(rpc_error)?
                .into_inner();
            write_verified_file(bundle_parts(parts), file, directory.path()).await?;
            let mut opened = open_verified_file(directory.path(), file).await?;
            opened
                .rewind()
                .await
                .map_err(DependencyContentError::Staging)?;
            Ok((directory, opened))
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| DependencyContentError::Timeout)?
    }

    /// Create disposable private export staging. Its owner removes all staged bytes.
    /// # Errors
    /// Returns a staging error if a private directory cannot be created.
    pub fn stage_file(
        &self,
        bundle: &DependencyBundle,
        index: usize,
    ) -> Result<tempfile::TempDir, DependencyContentError> {
        if index > bundle.file_count() {
            return Err(DependencyContentError::Integrity);
        }
        tempfile::Builder::new()
            .prefix("publication-")
            .permissions(std::fs::Permissions::from_mode(0o700))
            .tempdir_in(&self.staging)
            .map_err(DependencyContentError::Staging)
    }

    /// Publish one exact recorded file, or metadata at the file-count index.
    /// Main verifies all referenced files before accepting the final metadata.
    /// # Errors
    /// Returns a typed authority, integrity, capacity, staging, or transport failure.
    pub async fn publish_index(
        &self,
        bundle: &DependencyBundle,
        directory: &Path,
        index: usize,
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<(), DependencyContentError> {
        if index > bundle.file_count() {
            return Err(DependencyContentError::Integrity);
        }
        validate_staging(directory, 1, self.deadline)?;
        let authority = grant_bytes(grant, bundle.root())?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            if let Some(file) = bundle.files().get(index) {
                let mut opened = open_verified_file(directory, file).await?;
                opened
                    .rewind()
                    .await
                    .map_err(DependencyContentError::Staging)?;
                self.upload_bundle_file(
                    UploadSandboxBundleFileHeaderV1 {
                        kind: bundle.kind().into(),
                        root: bundle.root().to_owned(),
                        name: file.name().to_owned(),
                        signed_grant: authority,
                        bundle_json: bundle.record_json().to_vec(),
                    },
                    opened,
                    file.bytes(),
                )
                .await
            } else {
                self.rpc()
                    .publish_sandbox_bundle(PublishSandboxBundleRequestV1 {
                        kind: bundle.kind().into(),
                        root: bundle.root().to_owned(),
                        signed_grant: authority,
                        bundle_json: bundle.record_json().to_vec(),
                    })
                    .await
                    .map(|_| ())
                    .map_err(rpc_error)
            }
        };
        let result = tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| DependencyContentError::Timeout)?;
        if result.is_ok() {
            self.discard_export(bundle, index);
        }
        result
    }
}

/// Only `NotFound` is absent; every other refusal keeps its meaning.
fn decode_native_lookup(
    response: Result<Vec<u8>, Status>,
    root: &str,
) -> Result<Option<DependencyBundle>, DependencyContentError> {
    let metadata = match response {
        Ok(metadata) => metadata,
        Err(status) if status.code() == Code::NotFound => return Ok(None),
        Err(status) => return Err(rpc_error(status)),
    };
    bounded_metadata(&metadata)?;
    let bundle = DependencyBundle::parse(&metadata, root)?;
    if !bundle.native().is_some_and(|native| {
        native.record.kind == super::super::native_bundle::NativeKind::Cargo
            && native.record.language == super::super::request::Language::Rust
    }) {
        return Err(DependencyContentError::Integrity);
    }
    Ok(Some(bundle))
}

#[cfg(test)]
mod frozen_lookup_tests {
    use super::*;

    // Mirrors the RPC result shape decode_native_lookup consumes.
    #[allow(clippy::unnecessary_wraps)]
    fn found(body: &[u8]) -> Result<Vec<u8>, Status> {
        Ok(body.to_vec())
    }

    #[test]
    fn frozen_lookup_accepts_exact_cargo_and_only_not_found_is_absent() {
        let bytes = include_bytes!("native-cargo-v2.json");
        let record: serde_json::Value = serde_json::from_slice(bytes).unwrap();
        let root = record["digest"].as_str().unwrap();
        let found = decode_native_lookup(found(bytes), root).unwrap().unwrap();
        assert_eq!(found.record_json(), bytes);
        assert!(
            decode_native_lookup(Err(Status::not_found("absent")), root)
                .unwrap()
                .is_none()
        );
        for code in [
            Code::Unauthenticated,
            Code::PermissionDenied,
            Code::Internal,
            Code::Unimplemented,
        ] {
            assert!(decode_native_lookup(Err(Status::new(code, "refused")), root).is_err());
        }
    }

    #[test]
    fn frozen_lookup_refuses_changed_root_wrong_kind_and_unbounded_metadata() {
        let cargo = include_bytes!("native-cargo-v2.json");
        assert!(matches!(
            decode_native_lookup(found(cargo), &"a".repeat(64)),
            Err(DependencyContentError::Integrity)
        ));
        let deno = include_bytes!("native-deno-v2.json");
        let record: serde_json::Value = serde_json::from_slice(deno).unwrap();
        assert!(matches!(
            decode_native_lookup(found(deno), record["digest"].as_str().unwrap()),
            Err(DependencyContentError::Integrity)
        ));
        assert!(matches!(
            decode_native_lookup(found(&vec![b' '; METADATA_LIMIT + 1]), &"a".repeat(64)),
            Err(DependencyContentError::Integrity)
        ));
        assert!(matches!(
            decode_native_lookup(found(b"{}"), &"a".repeat(64)),
            Err(DependencyContentError::Integrity)
        ));
    }
}
