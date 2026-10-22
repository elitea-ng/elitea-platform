//! Verified package content. Staging is disposable; the recorded root is authoritative.
//! Package bytes never enter a control RPC, graph checkpoint, or shared queue.
//!
//! The supervisor reaches Main's content plane (`RuntimeContentService` on the
//! private content listener, #1359 U4) over one mTLS gRPC channel. File bytes
//! ride bounded chunk streams in both directions; metadata is one message.
use std::os::unix::fs::PermissionsExt as _;
use std::{
    future::Future,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use prost::Message as _;
use ring::digest;
use tokio::{
    fs,
    io::{AsyncReadExt as _, AsyncSeekExt as _, AsyncWriteExt as _},
    sync::Semaphore,
};
use tokio_stream::{Stream, StreamExt as _};
use tonic::{
    Code, Status,
    transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity},
};

use crate::protocol::{
    elitea::runtime::v1::{
        FetchSandboxBundleFileRequestV1, GetSandboxBundleRequestV1, PublishSandboxBundleRequestV1,
        SandboxBundleKindV1, SandboxContentFileHeaderV1, SandboxJobGrantClaimsV1,
        SignedSandboxJobGrantV1, UploadSandboxBundleFileHeaderV1, UploadSandboxBundleFileRequestV1,
        fetch_compiled_snapshot_file_response_v1, fetch_sandbox_bundle_file_response_v1,
        runtime_content_service_client::RuntimeContentServiceClient,
        upload_sandbox_bundle_file_request_v1,
    },
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
/// One upload chunk. The stream never holds more than one in memory.
const CHUNK_BYTES: usize = 64 * 1024;
/// Main's content listener caps one gRPC message at 16 MiB in either direction.
const MAX_MESSAGE_BYTES: usize = 16 << 20;

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
        "Shared package storage refused the transfer ({code:?}). Retry the same bundle without resolving packages again."
    )]
    Unavailable { code: Code },
    #[error("Package transfer timed out. Retry the same bundle without resolving packages again.")]
    Timeout,
    #[error("Package transfer failed. Check connectivity and retry the same bundle.")]
    Transport(#[source] Status),
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

/// Supervisor-only data plane. TLS, deadlines, and capacity are fixed here.
pub struct DependencyContentClient {
    client: RuntimeContentServiceClient<Channel>,
    staging: PathBuf,
    slots: Semaphore,
    deadline: Duration,
    exports: std::sync::Mutex<cache::ExportCache>,
}

impl DependencyContentClient {
    /// Construct with the private CA and the supervisor's client certificate and key.
    /// The channel connects on the first transfer; construct inside the Tokio runtime.
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
        let channel = content_endpoint(&origin, ca, certificate_and_key)?.connect_lazy();
        Ok(Self {
            client: content_client(channel),
            staging: staging.to_owned(),
            slots: Semaphore::new(capacity),
            deadline,
            exports: std::sync::Mutex::new(cache::ExportCache::default()),
        })
    }

    /// A handle on the shared channel; clones share its connection.
    fn rpc(&self) -> RuntimeContentServiceClient<Channel> {
        self.client.clone()
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
            let authority = grant_bytes(&grant().await?, root)?;
            let metadata = self
                .rpc()
                .get_sandbox_bundle(GetSandboxBundleRequestV1 {
                    kind: SandboxBundleKindV1::Python.into(),
                    root: root.to_owned(),
                    signed_grant: authority,
                })
                .await
                .map_err(rpc_error)?
                .into_inner()
                .bundle_json;
            bounded_metadata(&metadata)?;
            let bundle = PythonDependencyBundle::parse(&metadata, root)?;
            let directory = tempfile::Builder::new()
                .prefix("dependencies-")
                .permissions(std::fs::Permissions::from_mode(0o700))
                .tempdir_in(&self.staging)
                .map_err(DependencyContentError::Staging)?;
            for file in &bundle.files {
                let authority = grant_bytes(&grant().await?, root)?;
                let parts = self
                    .rpc()
                    .fetch_sandbox_bundle_file(FetchSandboxBundleFileRequestV1 {
                        kind: SandboxBundleKindV1::Python.into(),
                        root: root.to_owned(),
                        name: file.name.clone(),
                        signed_grant: authority,
                    })
                    .await
                    .map_err(rpc_error)?
                    .into_inner();
                write_verified_file(bundle_parts(parts), file, directory.path()).await?;
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
                let authority = grant_bytes(&grant().await?, bundle.root())?;
                self.upload_bundle_file(
                    UploadSandboxBundleFileHeaderV1 {
                        kind: SandboxBundleKindV1::Python.into(),
                        root: bundle.root().to_owned(),
                        name: file.name.clone(),
                        signed_grant: authority,
                        bundle_json: bundle.canonical.clone(),
                    },
                    opened,
                    file.bytes,
                )
                .await?;
            }
            let authority = grant_bytes(&grant().await?, bundle.root())?;
            self.rpc()
                .publish_sandbox_bundle(PublishSandboxBundleRequestV1 {
                    kind: SandboxBundleKindV1::Python.into(),
                    root: bundle.root().to_owned(),
                    signed_grant: authority,
                    bundle_json: bundle.canonical.clone(),
                })
                .await
                .map_err(rpc_error)?;
            Ok(())
        };
        tokio::time::timeout(self.deadline, operation)
            .await
            .map_err(|_| DependencyContentError::Timeout)?
    }

    /// Stream one verified local file after its header. A local read failure
    /// ends the stream early, so Main never commits a short file, and is
    /// reported as the staging failure it is rather than as Main's refusal.
    async fn upload_bundle_file(
        &self,
        header: UploadSandboxBundleFileHeaderV1,
        file: fs::File,
        bytes: u64,
    ) -> Result<(), DependencyContentError> {
        let failure = Arc::new(Mutex::new(None));
        let chunks = file_chunks(file, bytes, Arc::clone(&failure)).map(|chunk| {
            UploadSandboxBundleFileRequestV1 {
                part: Some(upload_sandbox_bundle_file_request_v1::Part::Chunk(chunk)),
            }
        });
        let first = UploadSandboxBundleFileRequestV1 {
            part: Some(upload_sandbox_bundle_file_request_v1::Part::Header(header)),
        };
        let result = self
            .rpc()
            .upload_sandbox_bundle_file(tokio_stream::once(first).chain(chunks))
            .await;
        take_read_failure(&failure)?;
        result.map(|_| ()).map_err(rpc_error)
    }
}

/// The mTLS endpoint for Main's content listener: the private CA, the
/// supervisor's client identity, and the shared keepalive schedule.
fn content_endpoint(
    origin: &str,
    ca: &[u8],
    certificate_and_key: &[u8],
) -> Result<Endpoint, DependencyContentError> {
    // The combined buffer holds the chain and the key; tonic reads the
    // certificates from the first argument and the key from the second.
    let tls = ClientTlsConfig::new()
        .ca_certificate(Certificate::from_pem(ca))
        .identity(Identity::from_pem(certificate_and_key, certificate_and_key));
    let endpoint = Endpoint::from_shared(origin.to_owned())
        .map_err(|_| DependencyContentError::Configuration)?
        .tls_config(tls)
        .map_err(|_| DependencyContentError::Configuration)?
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(30))
        .tcp_nodelay(true);
    Ok(crate::transport::keepalive::with_keepalive(endpoint))
}

fn content_client(channel: Channel) -> RuntimeContentServiceClient<Channel> {
    RuntimeContentServiceClient::new(channel)
        .max_decoding_message_size(MAX_MESSAGE_BYTES)
        .max_encoding_message_size(MAX_MESSAGE_BYTES)
}

/// Main's refusals by meaning (the content service's status table). A status
/// that carries a transport source never reached Main.
fn rpc_error(status: Status) -> DependencyContentError {
    match status.code() {
        Code::Unauthenticated | Code::PermissionDenied => DependencyContentError::Authority,
        Code::FailedPrecondition | Code::ResourceExhausted => DependencyContentError::Integrity,
        _ if std::error::Error::source(&status).is_some() => {
            DependencyContentError::Transport(status)
        }
        code => DependencyContentError::Unavailable { code },
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
/// The protobuf encoding of a bundle grant for `root`, after a purpose check.
fn grant_bytes(
    grant: &SignedSandboxJobGrantV1,
    root: &str,
) -> Result<Vec<u8>, DependencyContentError> {
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
    Ok(grant.encode_to_vec())
}
fn bounded_metadata(metadata: &[u8]) -> Result<(), DependencyContentError> {
    if metadata.is_empty() || metadata.len() > METADATA_LIMIT {
        return Err(DependencyContentError::Integrity);
    }
    Ok(())
}

/// One message of a file download stream, whichever RPC carried it.
enum FilePart {
    Header(SandboxContentFileHeaderV1),
    Chunk(Vec<u8>),
}
impl From<fetch_sandbox_bundle_file_response_v1::Part> for FilePart {
    fn from(part: fetch_sandbox_bundle_file_response_v1::Part) -> Self {
        match part {
            fetch_sandbox_bundle_file_response_v1::Part::Header(header) => Self::Header(header),
            fetch_sandbox_bundle_file_response_v1::Part::Chunk(chunk) => Self::Chunk(chunk),
        }
    }
}
impl From<fetch_compiled_snapshot_file_response_v1::Part> for FilePart {
    fn from(part: fetch_compiled_snapshot_file_response_v1::Part) -> Self {
        match part {
            fetch_compiled_snapshot_file_response_v1::Part::Header(header) => Self::Header(header),
            fetch_compiled_snapshot_file_response_v1::Part::Chunk(chunk) => Self::Chunk(chunk),
        }
    }
}
fn bundle_parts(
    stream: tonic::Streaming<
        crate::protocol::elitea::runtime::v1::FetchSandboxBundleFileResponseV1,
    >,
) -> impl Stream<Item = Result<Option<FilePart>, Status>> {
    stream.map(|message| message.map(|message| message.part.map(FilePart::from)))
}

/// Write one downloaded file: exactly one header first, naming the recorded
/// length and digest, then chunks that add up to exactly those.
async fn write_verified_file(
    parts: impl Stream<Item = Result<Option<FilePart>, Status>>,
    expected: &BundleFile,
    directory: &Path,
) -> Result<(), DependencyContentError> {
    let mut parts = std::pin::pin!(parts);
    let Some(Some(FilePart::Header(header))) = parts.next().await.transpose().map_err(rpc_error)?
    else {
        return Err(DependencyContentError::Integrity);
    };
    if header.length != expected.bytes || hex(&header.sha256) != expected.sha256 {
        return Err(DependencyContentError::Integrity);
    }
    let mut hash = digest::Context::new(&digest::SHA256);
    let mut length = 0;
    let mut output = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(directory.join(&expected.name))
        .await
        .map_err(DependencyContentError::Staging)?;
    while let Some(part) = parts.next().await {
        let Some(FilePart::Chunk(chunk)) = part.map_err(rpc_error)? else {
            return Err(DependencyContentError::Integrity);
        };
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

/// The chunks of one opened file, exactly `remaining` bytes. A read failure or
/// a file that shrank ends the stream and is recorded in `failure`.
fn file_chunks(
    mut file: fs::File,
    mut remaining: u64,
    failure: Arc<Mutex<Option<std::io::Error>>>,
) -> impl Stream<Item = Vec<u8>> {
    async_stream::stream! {
        let mut buffer = vec![0; CHUNK_BYTES];
        while remaining > 0 {
            let size = usize::try_from(remaining.min(buffer.len() as u64)).unwrap_or(CHUNK_BYTES);
            let error = match file.read(&mut buffer[..size]).await {
                Ok(0) => std::io::Error::new(
                    std::io::ErrorKind::UnexpectedEof,
                    "Prepared package file changed during upload",
                ),
                Ok(count) => {
                    remaining -= count as u64;
                    yield buffer[..count].to_vec();
                    continue;
                }
                Err(error) => error,
            };
            if let Ok(mut slot) = failure.lock() {
                *slot = Some(error);
            }
            break;
        }
    }
}
fn take_read_failure(
    failure: &Mutex<Option<std::io::Error>>,
) -> Result<(), DependencyContentError> {
    match failure.lock().map(|mut slot| slot.take()) {
        Ok(None) => Ok(()),
        Ok(Some(error)) => Err(DependencyContentError::Staging(error)),
        Err(_) => Err(DependencyContentError::Staging(std::io::Error::other(
            "package upload state is unavailable",
        ))),
    }
}
