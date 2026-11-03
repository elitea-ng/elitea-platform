//! The `ado_wiki` (Azure DevOps Wiki) toolkit family.
//!
//! All eight non-index SDK tools: wiki and page reads, page deletes, page
//! create/update (creating the wiki when missing) and page moves over the
//! `WikiClient` v7.0 routes. With `process_images`, the page reads describe
//! the page's attachment pictures with the run's chat model, when the run
//! lends an [`AttachmentHost`](super::attachment_host::AttachmentHost).

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
mod images;
pub(in crate::toolkits) mod tools;
