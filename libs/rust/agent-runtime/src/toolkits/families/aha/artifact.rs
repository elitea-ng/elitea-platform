use std::fmt;
use std::pin::Pin;
use std::sync::Arc;
#[cfg(test)]
use std::sync::Mutex;

use async_trait::async_trait;
use bytes::Bytes;
use reqwest::multipart::{Form, Part};
use ring::digest::{Context, SHA256};
use tokio::io::{AsyncSeekExt as _, AsyncWriteExt as _};
use tokio_stream::{Stream, StreamExt as _};

use crate::toolkits::families::attachment_host::{
    AttachmentError, AttachmentHost, MAX_ATTACH_BYTES, Step,
};
use crate::toolkits::families::office_host::OfficeFailure;

/// Aha documents files strictly below 300 MB and requires upload within 40s.
pub(super) const MAX_ARTIFACT_BYTES: u64 = 300_000_000 - 1;
const MAX_ARTIFACT_PATH_BYTES: usize = 2_048;
const MAX_BUCKET_BYTES: usize = 128;
const MAX_FILENAME_BYTES: usize = 255;
const MAX_MEDIA_TYPE_BYTES: usize = 128;
const MAX_OPAQUE_REFERENCE_BYTES: usize = 2_048;
const MAX_CLAIMS: usize = 128;
const MAX_SOURCE_CHUNK_BYTES: usize = 1_024 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AhaArtifactErrorCode {
    InvalidInput,
    Authorization,
    NotFound,
    Timeout,
    DependencyUnavailable,
    ResourceExhausted,
    InvalidResponse,
}

/// Stable artifact-plane failure without path, grant, URL, or file data.
pub(crate) struct AhaArtifactError {
    code: AhaArtifactErrorCode,
    /// The sentence the model reads, set only by the host-backed source (a
    /// platform read failure: path, empty, too large, refused...). It carries
    /// no path, name or credential.
    sentence: Option<Box<str>>,
}

impl AhaArtifactError {
    #[must_use]
    pub(crate) const fn code(&self) -> AhaArtifactErrorCode {
        self.code
    }

    pub(in crate::toolkits) const fn fixture(code: AhaArtifactErrorCode) -> Self {
        Self {
            code,
            sentence: None,
        }
    }

    /// The sentence the model reads for a failure of the host-backed source.
    #[must_use]
    pub(super) fn sentence(&self) -> Option<&str> {
        self.sentence.as_deref()
    }

    fn with_sentence(mut self, sentence: String) -> Self {
        self.sentence = Some(sentence.into());
        self
    }
}

impl fmt::Debug for AhaArtifactError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AhaArtifactError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AhaArtifactError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AhaArtifactErrorCode::InvalidInput => "the artifact reference is invalid",
            AhaArtifactErrorCode::Authorization => "the artifact is not authorized",
            AhaArtifactErrorCode::NotFound => "the artifact was not found",
            AhaArtifactErrorCode::Timeout => "the artifact read timed out",
            AhaArtifactErrorCode::DependencyUnavailable => "the artifact service is unavailable",
            AhaArtifactErrorCode::ResourceExhausted => "the artifact exceeds its approved limit",
            AhaArtifactErrorCode::InvalidResponse => {
                "the artifact service returned an invalid response"
            }
        })
    }
}

impl std::error::Error for AhaArtifactError {}

#[derive(Clone, Eq, PartialEq)]
struct ArtifactPath {
    canonical: Box<str>,
}

impl ArtifactPath {
    fn parse(value: &str) -> Result<Self, AhaArtifactError> {
        if value.len() > MAX_ARTIFACT_PATH_BYTES {
            return Err(resource_exhausted());
        }
        if value.trim() != value
            || value.contains(['\\', '?', '#'])
            || value.chars().any(char::is_control)
        {
            return Err(invalid_input());
        }
        let mut segments = value.split('/');
        if segments.next() != Some("") {
            return Err(invalid_input());
        }
        let bucket = segments.next().ok_or_else(invalid_input)?;
        let filename = segments.next().ok_or_else(invalid_input)?;
        if segments.next().is_some()
            || bucket.is_empty()
            || bucket.len() > MAX_BUCKET_BYTES
            || !bucket
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
            || matches!(bucket, "." | "..")
        {
            return Err(invalid_input());
        }
        validate_filename(filename)?;
        Ok(Self {
            canonical: value.into(),
        })
    }
}

/// One artifact read authority already admitted for the current invocation.
///
/// The opaque reference is redeemed by the artifact-plane adapter; it is never
/// interpreted as a URL or local filesystem path by this family.
struct AhaArtifactClaim {
    path: ArtifactPath,
    opaque_reference: Box<str>,
    filename: Box<str>,
    media_type: Box<str>,
    byte_length: u64,
    immutable_version: Box<str>,
    sha256: [u8; 32],
}

impl AhaArtifactClaim {
    fn new(
        path: &str,
        opaque_reference: &str,
        filename: &str,
        media_type: &str,
        byte_length: u64,
        immutable_version: &str,
        sha256: [u8; 32],
    ) -> Result<Self, AhaArtifactError> {
        if opaque_reference.is_empty()
            || opaque_reference.len() > MAX_OPAQUE_REFERENCE_BYTES
            || opaque_reference.chars().any(char::is_control)
        {
            return Err(invalid_input());
        }
        validate_filename(filename)?;
        validate_media_type(media_type)?;
        if byte_length == 0 || byte_length > MAX_ARTIFACT_BYTES {
            return Err(resource_exhausted());
        }
        if immutable_version.is_empty()
            || immutable_version.len() > MAX_OPAQUE_REFERENCE_BYTES
            || immutable_version.chars().any(char::is_control)
        {
            return Err(invalid_input());
        }
        Ok(Self {
            path: ArtifactPath::parse(path)?,
            opaque_reference: opaque_reference.into(),
            filename: filename.into(),
            media_type: media_type.into(),
            byte_length,
            immutable_version: immutable_version.into(),
            sha256,
        })
    }
}

impl fmt::Debug for AhaArtifactClaim {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AhaArtifactClaim")
            .field("byte_length", &self.byte_length)
            .finish_non_exhaustive()
    }
}

type ArtifactChunkStream = Pin<Box<dyn Stream<Item = Result<Bytes, AhaArtifactError>> + Send>>;

struct GrantedArtifactStream {
    immutable_version: Box<str>,
    chunks: ArtifactChunkStream,
}

/// Adapter for redeeming one opaque artifact-read grant.
///
/// Implementations must authenticate to the artifact plane with the current
/// claim and must not follow redirects or accept caller-supplied URLs.
#[async_trait]
trait ArtifactClaimSource: Send + Sync {
    async fn open(
        &self,
        claim: &AhaArtifactClaim,
    ) -> Result<GrantedArtifactStream, AhaArtifactError>;
}

/// Sealed, exact-path resolver for artifact authorities on one invocation.
///
/// Resolution uses a private auto-cleaned spool so length and digest are
/// verified before Aha sees any effect request. Production activation also
/// requires a shared disk and concurrent-spool budget outside this family.
pub(in crate::toolkits) struct AhaArtifactResolver {
    claims: Vec<AhaArtifactClaim>,
    source: Arc<dyn ArtifactClaimSource>,
    /// The run's artifact store. When set, a path is read through it (at most
    /// [`MAX_ATTACH_BYTES`]) instead of being matched against a claim.
    host: Option<AttachmentHost>,
}

impl AhaArtifactResolver {
    fn new(
        claims: Vec<AhaArtifactClaim>,
        source: Arc<dyn ArtifactClaimSource>,
    ) -> Result<Self, AhaArtifactError> {
        if claims.len() > MAX_CLAIMS {
            return Err(resource_exhausted());
        }
        for (index, claim) in claims.iter().enumerate() {
            if claims[..index]
                .iter()
                .any(|existing| existing.path == claim.path)
            {
                return Err(invalid_input());
            }
        }
        Ok(Self {
            claims,
            source,
            host: None,
        })
    }

    /// The resolver a runtime without an artifact-read grant plane holds: no
    /// claims, so every path is refused as unauthorized before any source is
    /// opened. The production toolset also omits `attach_file` (see
    /// `tools::build_aha_toolset`), so this is a second, fail-closed line.
    pub(in crate::toolkits) fn unavailable() -> Self {
        Self {
            claims: Vec::new(),
            source: Arc::new(UnavailableSource),
            host: None,
        }
    }

    /// The resolver of a run that lends its artifact store: `attach_file`
    /// reads the file through [`AttachmentHost::read_artifact`], whose cap
    /// ([`MAX_ATTACH_BYTES`], 50 MiB) is below Aha's own.
    pub(in crate::toolkits) fn from_host(host: AttachmentHost) -> Self {
        Self {
            claims: Vec::new(),
            source: Arc::new(UnavailableSource),
            host: Some(host),
        }
    }

    async fn multipart_from_host(
        &self,
        host: &AttachmentHost,
        requested_path: &str,
        filename_override: Option<&str>,
    ) -> Result<Form, AhaArtifactError> {
        // Aha's tool takes exactly /{bucket}/{filename}.
        let requested = ArtifactPath::parse(requested_path)
            .map_err(|error| error.with_sentence(AttachmentError::Path.sentence()))?;
        let file = host
            .read_artifact(&requested.canonical, MAX_ATTACH_BYTES)
            .await
            .map_err(|error| from_attachment(&error))?;
        let filename = filename_override.unwrap_or(&file.name);
        validate_filename(filename).map_err(|error| {
            error.with_sentence(
                "The file name cannot be sent to Aha (a plain name of at most 255 characters is needed); nothing was attached. Pass `filename` with a plain name.".to_owned(),
            )
        })?;
        validate_media_type(file.media_type)?;
        let part = Part::bytes(file.bytes)
            .file_name(filename.to_owned())
            .mime_str(file.media_type)
            .map_err(|_| invalid_input())?;
        Ok(Form::new().part("attachment[data]", part))
    }

    pub(super) async fn multipart(
        &self,
        requested_path: &str,
        filename_override: Option<&str>,
    ) -> Result<Form, AhaArtifactError> {
        if let Some(host) = &self.host {
            return self
                .multipart_from_host(host, requested_path, filename_override)
                .await;
        }
        let requested = ArtifactPath::parse(requested_path)?;
        let claim = self
            .claims
            .iter()
            .find(|claim| claim.path == requested)
            .ok_or_else(authorization)?;
        let granted = self.source.open(claim).await?;
        if granted.immutable_version.as_ref() != claim.immutable_version.as_ref() {
            return Err(authorization());
        }
        let filename = filename_override.unwrap_or(&claim.filename);
        validate_filename(filename)?;

        let file = spool_verified(granted.chunks, claim.byte_length, claim.sha256).await?;
        let part = Part::stream_with_length(file, claim.byte_length)
            .file_name(filename.to_owned())
            .mime_str(&claim.media_type)
            .map_err(|_| invalid_input())?;
        Ok(Form::new().part("attachment[data]", part))
    }
}

/// The claim source of [`AhaArtifactResolver::unavailable`]: nothing is
/// granted, so nothing opens.
struct UnavailableSource;

#[async_trait]
impl ArtifactClaimSource for UnavailableSource {
    async fn open(
        &self,
        _claim: &AhaArtifactClaim,
    ) -> Result<GrantedArtifactStream, AhaArtifactError> {
        Err(authorization())
    }
}

#[cfg(test)]
pub(in crate::toolkits) struct AhaArtifactFixture {
    path: Box<str>,
    opaque_reference: Box<str>,
    filename: Box<str>,
    media_type: Box<str>,
    byte_length: u64,
    immutable_version: Box<str>,
    returned_version: Box<str>,
    sha256: [u8; 32],
    chunks: Vec<Result<Bytes, AhaArtifactErrorCode>>,
}

#[cfg(test)]
impl AhaArtifactFixture {
    pub(in crate::toolkits) fn new(path: &str, filename: &str, bytes: Bytes) -> Self {
        let digest = ring::digest::digest(&SHA256, &bytes);
        let mut sha256 = [0_u8; 32];
        sha256.copy_from_slice(digest.as_ref());
        Self {
            path: path.into(),
            opaque_reference: "fixture-grant".into(),
            filename: filename.into(),
            media_type: "application/octet-stream".into(),
            byte_length: u64::try_from(bytes.len()).unwrap_or(u64::MAX),
            immutable_version: "v1".into(),
            returned_version: "v1".into(),
            sha256,
            chunks: vec![Ok(bytes)],
        }
    }

    pub(in crate::toolkits) fn declared_length(mut self, byte_length: u64) -> Self {
        self.byte_length = byte_length;
        self
    }

    pub(in crate::toolkits) fn expected_digest(mut self, sha256: [u8; 32]) -> Self {
        self.sha256 = sha256;
        self
    }

    pub(in crate::toolkits) fn returned_version(mut self, version: &str) -> Self {
        self.returned_version = version.into();
        self
    }
}

#[cfg(test)]
struct FixtureSource {
    streams: Mutex<Vec<(Box<str>, Option<GrantedArtifactStream>)>>,
}

#[cfg(test)]
#[async_trait]
impl ArtifactClaimSource for FixtureSource {
    async fn open(
        &self,
        claim: &AhaArtifactClaim,
    ) -> Result<GrantedArtifactStream, AhaArtifactError> {
        let mut streams = self.streams.lock().map_err(|_| dependency_unavailable())?;
        let (_, stream) = streams
            .iter_mut()
            .find(|(reference, _)| reference.as_ref() == claim.opaque_reference.as_ref())
            .ok_or_else(authorization)?;
        stream.take().ok_or_else(authorization)
    }
}

#[cfg(test)]
impl AhaArtifactResolver {
    pub(in crate::toolkits) fn fixture(
        fixtures: Vec<AhaArtifactFixture>,
    ) -> Result<Self, AhaArtifactError> {
        let mut claims = Vec::with_capacity(fixtures.len());
        let mut streams = Vec::with_capacity(fixtures.len());
        for (index, fixture) in fixtures.into_iter().enumerate() {
            let opaque_reference = format!("{}-{index}", fixture.opaque_reference);
            claims.push(AhaArtifactClaim::new(
                &fixture.path,
                &opaque_reference,
                &fixture.filename,
                &fixture.media_type,
                fixture.byte_length,
                &fixture.immutable_version,
                fixture.sha256,
            )?);
            let chunks = fixture
                .chunks
                .into_iter()
                .map(|chunk| chunk.map_err(AhaArtifactError::fixture));
            streams.push((
                opaque_reference.into(),
                Some(GrantedArtifactStream {
                    immutable_version: fixture.returned_version,
                    chunks: Box::pin(tokio_stream::iter(chunks)),
                }),
            ));
        }
        Self::new(
            claims,
            Arc::new(FixtureSource {
                streams: Mutex::new(streams),
            }),
        )
    }
}

async fn spool_verified(
    mut source: ArtifactChunkStream,
    expected_length: u64,
    expected_sha256: [u8; 32],
) -> Result<tokio::fs::File, AhaArtifactError> {
    let std_file = tokio::task::spawn_blocking(tempfile::tempfile)
        .await
        .map_err(|_| dependency_unavailable())?
        .map_err(|_| dependency_unavailable())?;
    let mut file = tokio::fs::File::from_std(std_file);
    let mut written = 0_u64;
    let mut digest = Context::new(&SHA256);
    while let Some(chunk) = source.next().await {
        let chunk = chunk?;
        if chunk.len() > MAX_SOURCE_CHUNK_BYTES {
            return Err(resource_exhausted());
        }
        written = written
            .checked_add(u64::try_from(chunk.len()).map_err(|_| resource_exhausted())?)
            .ok_or_else(resource_exhausted)?;
        if written > expected_length {
            return Err(invalid_response());
        }
        digest.update(&chunk);
        file.write_all(&chunk)
            .await
            .map_err(|_| dependency_unavailable())?;
    }
    file.flush().await.map_err(|_| dependency_unavailable())?;
    if written != expected_length || digest.finish().as_ref() != expected_sha256 {
        return Err(invalid_response());
    }
    file.seek(std::io::SeekFrom::Start(0))
        .await
        .map_err(|_| dependency_unavailable())?;
    Ok(file)
}

/// A host failure as an artifact-plane failure that keeps its sentence.
fn from_attachment(error: &AttachmentError) -> AhaArtifactError {
    let code = match error {
        AttachmentError::Path | AttachmentError::Empty => AhaArtifactErrorCode::InvalidInput,
        AttachmentError::TooLarge(_) => AhaArtifactErrorCode::ResourceExhausted,
        AttachmentError::Host(step, failure) => match (step, failure) {
            (_, OfficeFailure::NotFound) => AhaArtifactErrorCode::NotFound,
            (_, OfficeFailure::AccessRefused) => AhaArtifactErrorCode::Authorization,
            (_, OfficeFailure::TooLarge | OfficeFailure::ReplyTooLarge) => {
                AhaArtifactErrorCode::ResourceExhausted
            }
            (_, OfficeFailure::InvalidRequest | OfficeFailure::UnsupportedType) => {
                AhaArtifactErrorCode::InvalidInput
            }
            (_, OfficeFailure::Timeout) => AhaArtifactErrorCode::Timeout,
            (Step::Read | Step::Write | Step::Model, _) => {
                AhaArtifactErrorCode::DependencyUnavailable
            }
        },
    };
    AhaArtifactError {
        code,
        sentence: None,
    }
    .with_sentence(error.sentence())
}

fn validate_filename(value: &str) -> Result<(), AhaArtifactError> {
    if value.is_empty()
        || value.len() > MAX_FILENAME_BYTES
        || value.trim() != value
        || value.contains(['/', '\\'])
        || value.chars().any(char::is_control)
        || matches!(value, "." | "..")
    {
        return Err(if value.len() > MAX_FILENAME_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    Ok(())
}

fn validate_media_type(value: &str) -> Result<(), AhaArtifactError> {
    if value.is_empty()
        || value.len() > MAX_MEDIA_TYPE_BYTES
        || value.matches('/').count() != 1
        || !value
            .bytes()
            .all(|byte| (0x21..=0x7e).contains(&byte) && !matches!(byte, b'"' | b'\\' | b';'))
    {
        return Err(invalid_input());
    }
    Ok(())
}

const fn invalid_input() -> AhaArtifactError {
    AhaArtifactError {
        code: AhaArtifactErrorCode::InvalidInput,
        sentence: None,
    }
}

const fn authorization() -> AhaArtifactError {
    AhaArtifactError {
        code: AhaArtifactErrorCode::Authorization,
        sentence: None,
    }
}

const fn resource_exhausted() -> AhaArtifactError {
    AhaArtifactError {
        code: AhaArtifactErrorCode::ResourceExhausted,
        sentence: None,
    }
}

const fn invalid_response() -> AhaArtifactError {
    AhaArtifactError {
        code: AhaArtifactErrorCode::InvalidResponse,
        sentence: None,
    }
}

const fn dependency_unavailable() -> AhaArtifactError {
    AhaArtifactError {
        code: AhaArtifactErrorCode::DependencyUnavailable,
        sentence: None,
    }
}
