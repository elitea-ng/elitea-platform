//! The clients of two services on elitea-main's platform gRPC listener
//! (`ELITEA_RUNTIME_PLATFORM_ADDRESS`, `:9446`, #1359 U5):
//!
//! * [`artifacts`]: `elitea.runtime.v1.ArtifactObjectService`, the project
//!   artifact store the worker's Office host and indexing source, the
//!   engines' artifact-folder ingest and the sub-application host reach. It
//!   replaced their HTTP calls to `/api/v2/artifacts/...`.
//! * [`internal_tools`]: `elitea.runtime.v1.InternalToolService`, the
//!   platform's internal MCP tool categories the worker calls. It replaced
//!   the worker's MCP streamable-HTTP client to `/app/{project}/mcp/{scope}`.
//!
//! The crate opens no connection. The caller builds the mutual-TLS
//! [`tonic::transport::Channel`] to the platform listener with its own
//! crypto provider and client certificate (the same channel its
//! `elitea.llm.v1.ModelService` calls use) and passes it in.
//!
//! A refusal elitea-main decides (a bad bearer, a missing permission, an
//! unknown object) arrives as a gRPC status carrying the refusal itself in a
//! `-bin` trailer, with the HTTP status the route answered, so a caller
//! classifies it exactly as it classified the HTTP answer. A status without
//! that trailer is the hop failing. This mirrors `elitea-model-rpc`.
//!
//! Cancellation is dropping the call or its stream: the RPC is reset and
//! elitea-main cancels the request it serves.

use tonic::metadata::{AsciiMetadataValue, MetadataValue};
use zeroize::Zeroizing;

/// The generated `elitea.runtime.v1` bindings of the two services.
#[allow(clippy::all, clippy::pedantic, missing_docs)]
pub mod pb {
    tonic::include_proto!("elitea.runtime.v1");
}

pub mod artifacts;
pub mod internal_tools;

#[cfg(feature = "testing")]
pub mod testing;

/// One gRPC message on the platform listener, either way (its own cap).
pub const MAX_MESSAGE_BYTES: usize = 16 * 1024 * 1024;

/// The gRPC code a refusal's HTTP status maps to (elitea-main's
/// `modelgrpc.CodeForHTTPStatus`, which both services share).
#[must_use]
pub fn code_for_http_status(status: u16) -> tonic::Code {
    match status {
        400 | 413 | 422 => tonic::Code::InvalidArgument,
        401 => tonic::Code::Unauthenticated,
        402 | 429 => tonic::Code::ResourceExhausted,
        403 => tonic::Code::PermissionDenied,
        404 => tonic::Code::NotFound,
        408 | 504 => tonic::Code::DeadlineExceeded,
        409 => tonic::Code::Aborted,
        501 => tonic::Code::Unimplemented,
        502 | 503 => tonic::Code::Unavailable,
        500..=599 => tonic::Code::Internal,
        _ => tonic::Code::FailedPrecondition,
    }
}

/// The credential a call is made under, sent as `authorization: Bearer <t>`
/// metadata (sensitive: never logged by tonic). `Debug` never shows it.
#[derive(Clone)]
pub struct Bearer(Zeroizing<String>);

impl Bearer {
    /// `token` (without the `Bearer ` scheme), or `None` when it is empty or
    /// cannot travel as metadata.
    #[must_use]
    pub fn new(token: &str) -> Option<Self> {
        let token = token.trim();
        (!token.is_empty() && token.bytes().all(|byte| (0x21..=0x7e).contains(&byte)))
            .then(|| Self(Zeroizing::new(token.to_owned())))
    }

    /// The token, for a caller that must compare it (never log it).
    #[must_use]
    pub fn expose(&self) -> &str {
        &self.0
    }

    fn metadata(&self) -> Option<AsciiMetadataValue> {
        let mut value = MetadataValue::try_from(format!("Bearer {}", self.0.as_str())).ok()?;
        value.set_sensitive(true);
        Some(value)
    }

    /// `message` as a request carrying this bearer.
    pub(crate) fn request<T>(&self, message: T) -> tonic::Request<T> {
        let mut request = tonic::Request::new(message);
        if let Some(value) = self.metadata() {
            request.metadata_mut().insert("authorization", value);
        }
        request
    }
}

impl std::fmt::Debug for Bearer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Bearer(<redacted>)")
    }
}

/// The decoded `-bin` trailer `name` of `status`, when it carries one.
pub(crate) fn trailer<M: prost::Message + Default>(
    status: &tonic::Status,
    name: &str,
) -> Option<M> {
    let raw = status.metadata().get_bin(name)?.to_bytes().ok()?;
    M::decode(raw).ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_bearer_is_checked_and_never_shown() {
        assert!(Bearer::new("").is_none());
        assert!(Bearer::new("a b").is_none());
        let bearer = Bearer::new(" tok-1 ").expect("bearer");
        assert_eq!(bearer.expose(), "tok-1");
        assert!(!format!("{bearer:?}").contains("tok-1"));
        let request = bearer.request(());
        let value = request.metadata().get("authorization").expect("metadata");
        assert!(value.is_sensitive());
        assert_eq!(value.to_str().ok(), Some("Bearer tok-1"));
    }

    #[test]
    fn codes_follow_the_http_status() {
        assert_eq!(code_for_http_status(404), tonic::Code::NotFound);
        assert_eq!(code_for_http_status(403), tonic::Code::PermissionDenied);
        assert_eq!(code_for_http_status(409), tonic::Code::Aborted);
        assert_eq!(code_for_http_status(500), tonic::Code::Internal);
    }
}
