//! The platform's own artifact store: the object API client
//! ([`client::ArtifactPlatform`]) and, with `content-source`, a bucket
//! folder as a `ContentSource` ([`source::ArtifactSource`]).
//!
//! The agent-runtime `artifact` toolkit family does not use this client:
//! its tools act under the live execution claim over the worker's private
//! mTLS content listener (`PlatformWriter`). This is the bearer-authorized
//! object API an engine reads a folder through, the one
//! `elitea-repo-ingest`'s artifact-folder ingest speaks.

pub mod client;
#[cfg(feature = "content-source")]
pub mod source;
