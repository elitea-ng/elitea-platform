//! The `ado_boards` (Azure DevOps work items) toolkit family.
//!
//! Eleven of the SDK's thirteen non-index tools are always served: WIQL
//! search, work item reads/effects, links, relation types, comments,
//! wiki-page artifact links and work item type field discovery.
//! `get_image_by_url` (the run's chat model) and `attach_file_to_work_item`
//! (the artifact store) are served only when the run lends an
//! [`AttachmentHost`](super::attachment_host::AttachmentHost); the index
//! tools wait for indexing in Rust.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

mod attach;
pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
