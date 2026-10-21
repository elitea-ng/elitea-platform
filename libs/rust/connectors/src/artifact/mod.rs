//! The platform's own artifact store: its naming rules ([`client`]) and,
//! with `content-source`, a bucket folder as a `ContentSource`
//! ([`source::ArtifactSource`]) over the [`source::ArtifactObjects`] seam
//! the host implements on its platform channel.
//!
//! The agent-runtime `artifact` toolkit family does not use this module:
//! its tools act under the live execution claim over the worker's private
//! mTLS content listener (`PlatformWriter`).

pub mod client;
#[cfg(feature = "content-source")]
pub mod source;
