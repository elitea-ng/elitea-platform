//! Verified package content. Staging is disposable; the recorded root is authoritative.
//! Package bytes never enter a control RPC, graph checkpoint, or shared queue.
use std::os::unix::fs::PermissionsExt as _;
use std::{
    future::Future,
    path::{Path, PathBuf},
    time::Duration,
};

use base64::{Engine as _, engine::general_purpose::STANDARD};
use prost::Message as _;
use reqwest::{
    Client, Response, StatusCode,
    header::{CONTENT_LENGTH, CONTENT_TYPE, HeaderValue},
};
use ring::digest;
use tokio::{
    fs,
    io::{AsyncReadExt as _, AsyncSeekExt as _, AsyncWriteExt as _},
    sync::Semaphore,
};

use crate::protocol::{
    elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1},
    wire::{Schema, scan_message},
};

#[path = "dependency_content_cache.rs"]
mod cache;
#[path = "dependency_compiled_content.rs"]
mod compiled_transfer;
#[path = "dependency_content_tests.rs"]
#[cfg(test)]
mod tests;
#[path = "dependency_content_transfer.rs"]
mod transfer;
#[path = "workspace_content_transfer.rs"]
mod workspace_transfer;
pub(crate) use workspace_transfer::{WorkspaceContentError, WorkspaceReadProof};

const METADATA_LIMIT: usize = 128 * 1024;
#[cfg(test)]
const LOCK_NAME: &str = "elitea-python-lock.json";
const RECORD_NAME: &str = "elitea-python-bundle.json";
const GRANT_HEADER: &str = "x-elitea-sandbox-bundle-grant";

/// Stable messages contain no response bodies, package content, grants, or private URLs.
#[derive(Debug, thiserror::Error)]
pub enum DependencyContentError {
    #[error(
        "Package transfer configuration is invalid. Check TLS material, origin, staging permissions, and capacity."
    )]
    Configuration,
    #[error(
        "Package metadata or file content does not match the recorded bundle. Execution cannot start."
    )]
    Integrity,
    #[error(
        "Package storage authority is invalid or expired. Acquire a fresh grant for the same bundle."
    )]
    Authority,
    #[error(
        "Package transfer capacity is full. Retry the same bundle without resolving packages again."
    )]
    Busy,
    #[error(
        "Shared package storage returned HTTP {status}. Retry the same bundle without resolving packages again."
    )]
    Unavailable { status: u16 },
    #[error("Package transfer timed out. Retry the same bundle without resolving packages again.")]
    Timeout,
    #[error("Package transfer failed. Check connectivity and retry the same bundle.")]
    Transport(#[source] reqwest::Error),
    #[error("Private package staging failed. Check storage space and permissions.")]
    Staging(#[source] std::io::Error),
}

#[cfg(test)]
use super::dependency_bundle::{BundleContent, safe_name};
pub use super::dependency_bundle::{DependencyBundle, PythonDependencyBundle};
use super::dependency_bundle::{DependencyBundleFile as BundleFile, hex, valid_digest};

impl From<super::dependency_bundle::InvalidDependencyBundle> for DependencyContentError {
    fn from(_: super::dependency_bundle::InvalidDependencyBundle) -> Self {
        Self::Integrity
    }
}

/// Private files live until the consumer releases this staging owner.
/// The runtime must mount these files read-only. They are never a receipt database.
pub struct StagedPythonDependencies {
    directory: tempfile::TempDir,
    bundle: PythonDependencyBundle,
}
impl StagedPythonDependencies {
    #[must_use]
    pub fn directory(&self) -> &Path {
        self.directory.path()
    }
    #[must_use]
    pub fn bundle(&self) -> &PythonDependencyBundle {
        &self.bundle
    }

    /// Observe cleanup failures after the runtime releases its package mount.
    /// # Errors
    /// Returns `Staging` if the owned directory cannot be removed.
    pub fn close(self) -> Result<(), DependencyContentError> {
        self.directory
            .close()
            .map_err(DependencyContentError::Staging)
    }
}

/// Supervisor-only data plane. TLS, redirect policy, deadlines, and capacity are fixed here.
pub struct DependencyContentClient {
    client: Client,
    origin: String,
    staging: PathBuf,
    slots: Semaphore,
    deadline: Duration,
    exports: std::sync::Mutex<cache::ExportCache>,
}

impl DependencyContentClient {
    /// Construct with the private CA and the supervisor's client certificate and key.
    /// # Errors
    /// Rejects insecure origins, invalid TLS, non-private staging, and unbounded capacity.
    pub fn new(
        origin: &str,
        ca: &[u8],
        certificate_and_key: &[u8],
        staging: &Path,
        capacity: usize,
        deadline: Duration,
    ) -> Result<Self, DependencyContentError> {
        let origin = canonical_origin(origin)?;
        validate_staging(staging, capacity, deadline)?;
        let client = Client::builder()
            .https_only(true)
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .tls_built_in_root_certs(false)
            .add_root_certificate(
                reqwest::Certificate::from_pem(ca)
                    .map_err(|_| DependencyContentError::Configuration)?,
            )
            .identity(
                reqwest::Identity::from_pem(certificate_and_key)
                    .map_err(|_| DependencyContentError::Configuration)?,
            )
            .connect_timeout(Duration::from_secs(10))
            .timeout(Duration::from_secs(30))
            .pool_max_idle_per_host(capacity)
            .build()
            .map_err(|_| DependencyContentError::Configuration)?;
        Ok(Self {
            client,
            origin,
            staging: staging.to_owned(),
            slots: Semaphore::new(capacity),
            deadline,
            exports: std::sync::Mutex::new(cache::ExportCache::default()),
        })
    }

    /// Obtain a fresh root-bound grant for each transfer. Interrupted transfers reuse the root.
    /// # Errors
    /// Returns a typed failure before returning any directory with unverified content.
    pub async fn download<G, F>(
        &self,
        root: &str,
        mut grant: G,
    ) -> Result<StagedPythonDependencies, DependencyContentError>
    where
        G: FnMut() -> F,
        F: Future<Output = Result<SignedSandboxJobGrantV1, DependencyContentError>>,
    {
        if !valid_digest(root) {
            return Err(DependencyContentError::Integrity);
        }
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            let authority = grant_header(&grant().await?, root)?;
            let mut response = self
                .client
                .get(format!("{}/sandbox-bundles/{root}", self.origin))
                .header(GRANT_HEADER, authority)
                .send()
                .await
                .map_err(transport)?;
            check_response(&response, StatusCode::OK, "application/json", None)?;
            let metadata = bounded_metadata(&mut response).await?;
            let bundle = PythonDependencyBundle::parse(&metadata, root)?;
            let directory = tempfile::Builder::new()
                .prefix("dependencies-")
                .permissions(std::fs::Permissions::from_mode(0o700))
                .tempdir_in(&self.staging)
                .map_err(DependencyContentError::Staging)?;
            for file in &bundle.files {
                let authority = grant_header(&grant().await?, root)?;
                let response = self
                    .client
                    .get(format!(
                        "{}/sandbox-bundles/{root}/files/{}",
                        self.origin, file.name
                    ))
                    .header(GRANT_HEADER, authority)
                    .send()
                    .await
                    .map_err(transport)?;
                write_verified_file(response, file, directory.path()).await?;
            }
            let mut metadata = fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(directory.path().join(RECORD_NAME))
                .await
                .map_err(DependencyContentError::Staging)?;
            metadata
                .write_all(bundle.record_json())
                .await
                .map_err(DependencyContentError::Staging)?;
            metadata
                .flush()
                .await
                .map_err(DependencyContentError::Staging)?;
            Ok(StagedPythonDependencies { directory, bundle })
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| DependencyContentError::Timeout)?
    }

    /// Upload exact native files. Publish metadata only after all uploads succeed.
    /// The directory must remain private to the trusted preparer and supervisor.
    /// # Errors
    /// Rejects unsafe local content and observes transfer or publication failures.
    pub async fn publish<G, F>(
        &self,
        bundle: &PythonDependencyBundle,
        directory: &Path,
        mut grant: G,
    ) -> Result<(), DependencyContentError>
    where
        G: FnMut() -> F,
        F: Future<Output = Result<SignedSandboxJobGrantV1, DependencyContentError>>,
    {
        validate_staging(directory, 1, self.deadline)?;
        let _slot = self
            .slots
            .try_acquire()
            .map_err(|_| DependencyContentError::Busy)?;
        let operation = async {
            for file in &bundle.files {
                let mut opened = open_verified_file(directory, file).await?;
                opened
                    .rewind()
                    .await
                    .map_err(DependencyContentError::Staging)?;
                let authority = grant_header(&grant().await?, bundle.root())?;
                let body = reqwest::Body::wrap_stream(file_stream(opened, file.bytes));
                let form = reqwest::multipart::Form::new()
                    .part(
                        "bundle",
                        reqwest::multipart::Part::bytes(bundle.canonical.clone())
                            .mime_str("application/json")
                            .map_err(transport)?,
                    )
                    .part(
                        "file",
                        reqwest::multipart::Part::stream_with_length(body, file.bytes)
                            .file_name(file.name.clone())
                            .mime_str("application/octet-stream")
                            .map_err(transport)?,
                    );
                let response = self
                    .client
                    .put(format!(
                        "{}/sandbox-bundles/{}/files/{}",
                        self.origin,
                        bundle.root(),
                        file.name
                    ))
                    .header(GRANT_HEADER, authority)
                    .multipart(form)
                    .send()
                    .await
                    .map_err(transport)?;
                check_status(&response, StatusCode::NO_CONTENT)?;
            }
            let authority = grant_header(&grant().await?, bundle.root())?;
            let response = self
                .client
                .post(format!("{}/sandbox-bundles/{}", self.origin, bundle.root()))
                .header(GRANT_HEADER, authority)
                .header(CONTENT_TYPE, "application/json")
                .body(bundle.canonical.clone())
                .send()
                .await
                .map_err(transport)?;
            check_status(&response, StatusCode::NO_CONTENT)
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| DependencyContentError::Timeout)?
    }
}

fn canonical_origin(origin: &str) -> Result<String, DependencyContentError> {
    if origin.is_empty() || origin.len() > 2048 || !origin.is_ascii() || origin.contains('#') {
        return Err(DependencyContentError::Configuration);
    }
    let url = reqwest::Url::parse(origin).map_err(|_| DependencyContentError::Configuration)?;
    if url.scheme() != "https"
        || url.path() != "/"
        || url.query().is_some()
        || url.fragment().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.host_str().is_none()
        || url.port() == Some(0)
    {
        return Err(DependencyContentError::Configuration);
    }
    Ok(url.origin().ascii_serialization())
}
fn validate_staging(
    directory: &Path,
    capacity: usize,
    deadline: Duration,
) -> Result<(), DependencyContentError> {
    let metadata =
        std::fs::symlink_metadata(directory).map_err(|_| DependencyContentError::Configuration)?;
    if !directory.is_absolute()
        || !metadata.is_dir()
        || metadata.permissions().mode() & 0o077 != 0
        || !(1..=32).contains(&capacity)
        || deadline.is_zero()
        || deadline > Duration::from_mins(5)
    {
        return Err(DependencyContentError::Configuration);
    }
    Ok(())
}
fn transport(error: reqwest::Error) -> DependencyContentError {
    if error.is_timeout() {
        DependencyContentError::Timeout
    } else {
        DependencyContentError::Transport(error.without_url())
    }
}
fn grant_header(
    grant: &SignedSandboxJobGrantV1,
    root: &str,
) -> Result<HeaderValue, DependencyContentError> {
    if grant.key_id.is_empty()
        || grant.key_id.len() > 256
        || grant.signature.len() != 64
        || grant.claims_bytes.len() > 4096
    {
        return Err(DependencyContentError::Authority);
    }
    scan_message(&grant.claims_bytes, Schema::SandboxGrant)
        .map_err(|_| DependencyContentError::Authority)?;
    // This is a purpose check, not authorization. Main verifies the signature and TLS recipient.
    let claims = SandboxJobGrantClaimsV1::decode(grant.claims_bytes.as_slice())
        .map_err(|_| DependencyContentError::Authority)?;
    if claims.revision != 3
        || claims.cancel_only
        || claims.dependency_bundle_sha256.len() != 32
        || hex(&claims.dependency_bundle_sha256) != root
    {
        return Err(DependencyContentError::Authority);
    }
    let mut value = HeaderValue::from_str(&STANDARD.encode(grant.encode_to_vec()))
        .map_err(|_| DependencyContentError::Authority)?;
    value.set_sensitive(true);
    Ok(value)
}
fn check_status(response: &Response, expected: StatusCode) -> Result<(), DependencyContentError> {
    if response.status() == expected {
        return Ok(());
    }
    match response.status() {
        StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => Err(DependencyContentError::Authority),
        StatusCode::UNPROCESSABLE_ENTITY | StatusCode::PAYLOAD_TOO_LARGE => {
            Err(DependencyContentError::Integrity)
        }
        other => Err(DependencyContentError::Unavailable {
            status: other.as_u16(),
        }),
    }
}
fn check_response(
    response: &Response,
    expected: StatusCode,
    media: &str,
    length: Option<u64>,
) -> Result<(), DependencyContentError> {
    check_status(response, expected)?;
    let headers = response.headers();
    let mut media_values = headers.get_all(CONTENT_TYPE).iter();
    if media_values.next().is_none_or(|value| value != media)
        || media_values.next().is_some()
        || headers.contains_key("content-encoding")
    {
        return Err(DependencyContentError::Integrity);
    }
    let actual = declared_length(response)?;
    if length.is_some_and(|length| actual != length)
        || length.is_none() && actual > METADATA_LIMIT as u64
    {
        return Err(DependencyContentError::Integrity);
    }
    Ok(())
}
async fn bounded_metadata(response: &mut Response) -> Result<Vec<u8>, DependencyContentError> {
    let expected = declared_length(response)?;
    let mut bytes = Vec::new();
    while let Some(chunk) = response.chunk().await.map_err(transport)? {
        if chunk.len() > METADATA_LIMIT.saturating_sub(bytes.len()) {
            return Err(DependencyContentError::Integrity);
        }
        bytes.extend_from_slice(&chunk);
    }
    if bytes.len() as u64 != expected {
        return Err(DependencyContentError::Integrity);
    }
    Ok(bytes)
}
fn declared_length(response: &Response) -> Result<u64, DependencyContentError> {
    let mut values = response.headers().get_all(CONTENT_LENGTH).iter();
    let length = values
        .next()
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse().ok())
        .ok_or(DependencyContentError::Integrity)?;
    if values.next().is_some() {
        return Err(DependencyContentError::Integrity);
    }
    Ok(length)
}
async fn write_verified_file(
    mut response: Response,
    expected: &BundleFile,
    directory: &Path,
) -> Result<(), DependencyContentError> {
    check_response(
        &response,
        StatusCode::OK,
        "application/octet-stream",
        Some(expected.bytes),
    )?;
    let mut hash = digest::Context::new(&digest::SHA256);
    let mut length = 0;
    let mut output = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(directory.join(&expected.name))
        .await
        .map_err(DependencyContentError::Staging)?;
    while let Some(chunk) = response.chunk().await.map_err(transport)? {
        length += chunk.len() as u64;
        if length > expected.bytes {
            return Err(DependencyContentError::Integrity);
        }
        hash.update(&chunk);
        output
            .write_all(&chunk)
            .await
            .map_err(DependencyContentError::Staging)?;
    }
    if length != expected.bytes || hex(hash.finish().as_ref()) != expected.sha256 {
        return Err(DependencyContentError::Integrity);
    }
    output
        .flush()
        .await
        .map_err(DependencyContentError::Staging)
}
async fn open_verified_file(
    directory: &Path,
    expected: &BundleFile,
) -> Result<fs::File, DependencyContentError> {
    let mut file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(
            (rustix::fs::OFlags::NOFOLLOW | rustix::fs::OFlags::NONBLOCK)
                .bits()
                .cast_signed(),
        )
        .open(directory.join(&expected.name))
        .await
        .map_err(DependencyContentError::Staging)?;
    let metadata = file
        .metadata()
        .await
        .map_err(DependencyContentError::Staging)?;
    if !metadata.is_file() || metadata.len() != expected.bytes {
        return Err(DependencyContentError::Integrity);
    }
    let mut hash = digest::Context::new(&digest::SHA256);
    let mut bytes = vec![0; 64 * 1024];
    let mut length = 0;
    loop {
        let count = file
            .read(&mut bytes)
            .await
            .map_err(DependencyContentError::Staging)?;
        if count == 0 {
            break;
        }
        length += count as u64;
        if length > expected.bytes {
            return Err(DependencyContentError::Integrity);
        }
        hash.update(&bytes[..count]);
    }
    if length != expected.bytes || hex(hash.finish().as_ref()) != expected.sha256 {
        return Err(DependencyContentError::Integrity);
    }
    Ok(file)
}
fn file_stream(
    mut file: fs::File,
    mut remaining: u64,
) -> impl futures_util::Stream<Item = Result<Vec<u8>, std::io::Error>> {
    async_stream::try_stream! {
        let mut buffer = vec![0; 64 * 1024];
        while remaining > 0 {
            let size = usize::try_from(remaining.min(buffer.len() as u64)).map_err(std::io::Error::other)?;
            let count = file.read(&mut buffer[..size]).await?;
            if count == 0 { Err(std::io::Error::new(std::io::ErrorKind::UnexpectedEof, "Prepared package file changed during upload"))?; }
            remaining -= count as u64;
            yield buffer[..count].to_vec();
        }
    }
}
