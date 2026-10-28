//! The governed execution security context (ADR-0039 decision 6, #1115).
//!
//! Main builds an `ExecutionSecurityContextV1` at admission, stores its
//! deterministic bytes beside the execution and names them on the signed
//! command as `WorkerCommandV1.security_context_ref` (field 65): a content id
//! `elitea.security-context/<execution_id>` and the SHA-256 of the bytes. A
//! worker that opted in fetches the bytes under its claim through
//! `RuntimeContentService.GetInput` and trusts no field before [`verify`]
//! passed: the digest, the size, an unknown field anywhere, the schema
//! revision and the profile. Go's reference is
//! `services/elitea-main/internal/domain/runtime/securitycontext`; the shared
//! corpus is `testdata/proto/runtime/v1/security-context/vectors.json`.
//!
//! The context holds references, epochs and digests only. Its fields are
//! never logged: [`ExecutionSecurityContext`]'s `Debug` shows the content id
//! and digest alone.

use std::fmt;
use std::sync::Arc;

use prost::Message as _;
use ring::digest;
use subtle::ConstantTimeEq;

use super::ProtocolError;
use super::elitea::runtime::v1::{
    DigestAlgorithmV1, ExecutionSecurityContextReferenceV1, ExecutionSecurityContextV1,
    WorkerCommandV1,
};
use super::wire::{Schema, scan_message};

/// The only context revision this worker reads.
pub const SCHEMA_REVISION_V1: &str = "elitea.runtime.security-context.v1";
/// The profile every context names.
pub const GOVERNED_PROFILE: &str = "elitea.credential-profile.governed.v1";
/// The `GetInput` media type of the context bytes.
pub const MEDIA_TYPE: &str = "application/x-protobuf";
/// The `GetInput` content id namespace of a context.
pub const CONTENT_ID_PREFIX: &str = "elitea.security-context/";
/// The bound of the marshalled context.
pub const MAX_BYTES: usize = 16 << 10;
/// The bound of the reference's content id.
const MAX_CONTENT_ID_BYTES: usize = 128;

/// The `GetInput` content id of an execution's context.
#[must_use]
pub fn content_id(execution_id: &str) -> String {
    format!("{CONTENT_ID_PREFIX}{execution_id}")
}

/// Field 65 of a verified command, validated: a context content id and a
/// SHA-256 digest.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SecurityContextReference {
    content_id: String,
    digest: [u8; 32],
}

impl SecurityContextReference {
    /// Read field 65 of `command`. `None` is a legacy command.
    ///
    /// # Errors
    ///
    /// Returns [`ProtocolError::InvalidInput`] for a reference that is not
    /// the context of this command's execution, another schema revision, or
    /// a digest that is not SHA-256.
    pub fn from_command(command: &WorkerCommandV1) -> Result<Option<Self>, ProtocolError> {
        command
            .security_context_ref
            .as_ref()
            .map(|reference| Self::from_proto(reference, &command.execution_id))
            .transpose()
    }

    fn from_proto(
        reference: &ExecutionSecurityContextReferenceV1,
        execution_id: &str,
    ) -> Result<Self, ProtocolError> {
        let malformed =
            || ProtocolError::InvalidInput("the security context reference is malformed");
        if execution_id.is_empty()
            || reference.content_entry_id != content_id(execution_id)
            || reference.content_entry_id.len() > MAX_CONTENT_ID_BYTES
            || reference.schema_revision != SCHEMA_REVISION_V1
        {
            return Err(malformed());
        }
        let digest = reference.digest.as_ref().ok_or_else(malformed)?;
        if digest.algorithm != DigestAlgorithmV1::Sha256 as i32 {
            return Err(malformed());
        }
        let digest: [u8; 32] = digest
            .value
            .as_slice()
            .try_into()
            .map_err(|_| malformed())?;
        Ok(Self {
            content_id: reference.content_entry_id.clone(),
            digest,
        })
    }

    #[must_use]
    pub fn content_id(&self) -> &str {
        &self.content_id
    }

    #[must_use]
    pub const fn digest(&self) -> &[u8; 32] {
        &self.digest
    }

    /// The `GetInput` immutable version: the digest as `sha256:<hex>`
    /// (`runtimedomain.Digest.String()` on Main).
    #[must_use]
    pub fn immutable_version(&self) -> String {
        format!("sha256:{}", lower_hex(&self.digest))
    }
}

/// A claim's governed state: the command's reference and, once preparation
/// fetched and verified it, the context. Both empty for a legacy command.
#[derive(Clone, Debug, Default)]
pub(crate) struct GovernedContext {
    pub(crate) reference: Option<SecurityContextReference>,
    pub(crate) verified: Option<Arc<ExecutionSecurityContext>>,
}

impl GovernedContext {
    /// The claim state of `command`, before the context is fetched.
    pub(crate) fn from_command(command: &WorkerCommandV1) -> Result<Self, ProtocolError> {
        Ok(Self {
            reference: SecurityContextReference::from_command(command)?,
            verified: None,
        })
    }
}

/// A verified context, read-only for the execution that fetched it.
pub struct ExecutionSecurityContext {
    content_id: String,
    digest: [u8; 32],
    message: ExecutionSecurityContextV1,
}

impl ExecutionSecurityContext {
    /// The decoded context. Its fields are references and epochs; none may
    /// be logged.
    #[must_use]
    pub const fn message(&self) -> &ExecutionSecurityContextV1 {
        &self.message
    }

    #[must_use]
    pub fn content_id(&self) -> &str {
        &self.content_id
    }

    #[must_use]
    pub const fn digest(&self) -> &[u8; 32] {
        &self.digest
    }
}

impl fmt::Debug for ExecutionSecurityContext {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ExecutionSecurityContext")
            .field("content_id", &self.content_id)
            .field("digest", &lower_hex(&self.digest))
            .finish_non_exhaustive()
    }
}

/// Why a context was refused. Every case fails the execution closed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SecurityContextError {
    /// The served content is not the context the command names.
    ContentMismatch,
    /// Empty or larger than [`MAX_BYTES`].
    Size,
    /// The bytes do not hash to the reference digest.
    DigestMismatch,
    /// Not a strict `ExecutionSecurityContextV1` of this revision.
    Invalid,
}

impl SecurityContextError {
    #[must_use]
    pub const fn message(self) -> &'static str {
        match self {
            Self::ContentMismatch => "the security context is not the command's",
            Self::Size => "the security context is empty or too large",
            Self::DigestMismatch => "the security context does not match its reference digest",
            Self::Invalid => "the security context is not a valid governed context",
        }
    }

    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::ContentMismatch => "security_context.content_mismatch",
            Self::Size => "security_context.size",
            Self::DigestMismatch => "security_context.digest_mismatch",
            Self::Invalid => "security_context.invalid",
        }
    }
}

impl fmt::Display for SecurityContextError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message())
    }
}

impl std::error::Error for SecurityContextError {}

/// Verify served bytes against the command's reference for `execution_id`
/// and decode them strictly.
///
/// # Errors
///
/// Returns the [`SecurityContextError`] of the first check that fails.
pub fn verify(
    encoded: &[u8],
    reference: &SecurityContextReference,
    execution_id: &str,
) -> Result<ExecutionSecurityContext, SecurityContextError> {
    if execution_id.is_empty() || reference.content_id != content_id(execution_id) {
        return Err(SecurityContextError::ContentMismatch);
    }
    if encoded.is_empty() || encoded.len() > MAX_BYTES {
        return Err(SecurityContextError::Size);
    }
    let actual = digest::digest(&digest::SHA256, encoded);
    if actual.as_ref().ct_eq(&reference.digest).unwrap_u8() != 1 {
        return Err(SecurityContextError::DigestMismatch);
    }
    scan_context(encoded).map_err(|_| SecurityContextError::Invalid)?;
    let message =
        ExecutionSecurityContextV1::decode(encoded).map_err(|_| SecurityContextError::Invalid)?;
    if message.schema_revision != SCHEMA_REVISION_V1 || message.profile != GOVERNED_PROFILE {
        return Err(SecurityContextError::Invalid);
    }
    Ok(ExecutionSecurityContext {
        content_id: reference.content_id.clone(),
        digest: reference.digest,
        message,
    })
}

/// The strict wire scan: prost drops unknown fields, so every message of the
/// tree is scanned against its field rules before decoding.
fn scan_context(encoded: &[u8]) -> Result<(), ProtocolError> {
    let fields = scan_message(encoded, Schema::SecurityContext)?;
    for digest in [4, 14] {
        if fields.contains(digest) {
            scan_message(
                fields.length_field(digest, "the context digest is missing")?,
                Schema::Digest,
            )?;
        }
    }
    if fields.contains(13) {
        scan_message(
            fields.length_field(13, "the context caller is missing")?,
            Schema::CallerContext,
        )?;
    }
    for link in fields.repeated_field(15) {
        scan_message(link, Schema::ActorLink)?;
    }
    for reference in fields.repeated_field(34) {
        let reference = scan_message(reference, Schema::GovernedCredentialReference)?;
        if reference.contains(5) {
            scan_message(
                reference.length_field(5, "the toolkit digest is missing")?,
                Schema::Digest,
            )?;
        }
    }
    Ok(())
}

/// The strict wire scan of a command's field 65, when present.
pub(crate) fn scan_reference(raw: &[u8]) -> Result<(), ProtocolError> {
    let fields = scan_message(raw, Schema::SecurityContextReference)?;
    if fields.contains(2) {
        scan_message(
            fields.length_field(2, "the security context digest is missing")?,
            Schema::Digest,
        )?;
    }
    Ok(())
}

fn lower_hex(bytes: &[u8]) -> String {
    use fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut out, byte| {
            let _ = write!(out, "{byte:02x}");
            out
        })
}

#[cfg(test)]
#[path = "security_context_tests.rs"]
mod tests;
