//! Aha! toolkit family.
//!
//! The family owns one claim-scoped Aha origin and bearer credential.
//! `materialize` serves 32 of the SDK's 33 tools always, and `attach_file`
//! as the 33rd when the run lends its artifact store: the file is read through
//! the [`AttachmentHost`](super::attachment_host::AttachmentHost), at most
//! 50 MiB. The sealed, digest-verified claim resolver stays for a runtime that
//! has an artifact-read grant plane; none does yet.

// Artifact claims are constructed only by the test fixture until a grant
// plane exists.
#![allow(dead_code)]

pub(in crate::toolkits) mod artifact;
pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod format;
pub(in crate::toolkits) mod tools;
