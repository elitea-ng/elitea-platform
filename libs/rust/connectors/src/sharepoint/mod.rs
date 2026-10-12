#![allow(clippy::doc_markdown)] // `SharePoint`, `OneNote` are product names.
//! SharePoint through Microsoft Graph: the indexing connector's
//! configuration (app-only credential), the Graph client and the
//! `ContentSource` over a site's document libraries (ADR-0030 I2).
//!
//! The agent toolkit's SharePoint family (agent-runtime) keeps its delegated
//! OAuth client: a tool call runs for a user, an index run does not.

pub mod config;
pub mod graph;
pub mod source;
