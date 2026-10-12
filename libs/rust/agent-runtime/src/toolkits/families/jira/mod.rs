//! Jira toolkit family: the SDK's twelve Jira REST tools over one
//! claim-scoped HTTPS origin and credential (Bearer, session cookie or
//! Basic), on REST v2 or v3 as the SDK resolves it.
//!
//! The SDK's image-description, attachment-content and artifact-upload
//! tools and its six index tools are not served (`tools::UNSERVED_TOOLS`).

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
