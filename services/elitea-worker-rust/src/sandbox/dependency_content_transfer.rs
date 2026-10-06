//! One root-bound transfer per worker RPC. Retries never resolve new packages.
use super::*;

impl DependencyContentClient {
    /// Confirm exact published metadata before accessing a retained preparation runtime.
    /// # Errors
    /// Rejects changed metadata, invalid authority, and storage failures.
    pub async fn confirm_publication(
        &self,
        bundle: &DependencyBundle,
        grant: &SignedSandboxJobGrantV1,
    ) -> Result<bool, DependencyContentError> {
        let authority = grant_header(grant, bundle.root())?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            let mut response = self
                .client
                .get(format!(
                    "{}/{}/{}",
                    self.origin,
                    bundle.route(),
                    bundle.root()
                ))
                .header(GRANT_HEADER, authority)
                .send()
                .await
                .map_err(transport)?;
            if response.status() == StatusCode::NOT_FOUND {
                return Ok(false);
            }
            check_response(&response, StatusCode::OK, "application/json", None)?;
            let metadata = bounded_metadata(&mut response).await?;
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
        let authority = grant_header(grant, bundle.root())?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            let directory = self.stage_file(bundle, index)?;
            let response = self
                .client
                .get(format!(
                    "{}/{}/{}/files/{}",
                    self.origin,
                    bundle.route(),
                    bundle.root(),
                    file.name()
                ))
                .header(GRANT_HEADER, authority)
                .send()
                .await
                .map_err(transport)?;
            write_verified_file(response, file, directory.path()).await?;
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
        let authority = grant_header(grant, bundle.root())?;
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
                let body = reqwest::Body::wrap_stream(file_stream(opened, file.bytes()));
                let form = reqwest::multipart::Form::new()
                    .part(
                        "bundle",
                        reqwest::multipart::Part::bytes(bundle.record_json().to_vec())
                            .mime_str("application/json")
                            .map_err(transport)?,
                    )
                    .part(
                        "file",
                        reqwest::multipart::Part::stream_with_length(body, file.bytes())
                            .file_name(file.name().to_owned())
                            .mime_str("application/octet-stream")
                            .map_err(transport)?,
                    );
                let response = self
                    .client
                    .put(format!(
                        "{}/{}/{}/files/{}",
                        self.origin,
                        bundle.route(),
                        bundle.root(),
                        file.name()
                    ))
                    .header(GRANT_HEADER, authority)
                    .multipart(form)
                    .send()
                    .await
                    .map_err(transport)?;
                check_status(&response, StatusCode::NO_CONTENT)
            } else {
                let response = self
                    .client
                    .post(format!(
                        "{}/{}/{}",
                        self.origin,
                        bundle.route(),
                        bundle.root()
                    ))
                    .header(GRANT_HEADER, authority)
                    .header(CONTENT_TYPE, "application/json")
                    .body(bundle.record_json().to_vec())
                    .send()
                    .await
                    .map_err(transport)?;
                check_status(&response, StatusCode::NO_CONTENT)
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
