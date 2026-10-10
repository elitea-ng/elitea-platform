//! Aha! toolkit family.
//!
//! The family owns one claim-scoped Aha origin and bearer credential.
//! `materialize` serves 32 of the SDK's 33 tools: `attach_file` uploads an
//! artifact through a sealed, digest-verified resolver, and no runtime host
//! has the artifact-read grant plane that resolver needs, so production builds
//! the family without one and omits that tool.

// Artifact claims are constructed only by the test fixture until a grant
// plane exists; the production resolver holds none.
#![allow(dead_code)]

pub(in crate::toolkits) mod artifact;
pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod format;
pub(in crate::toolkits) mod tools;
