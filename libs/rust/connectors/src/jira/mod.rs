//! Jira (ADR-0030 I2): the toolkit configuration the connector reads, the
//! `index_data` arguments it honours, and the `ContentSource` over the Jira
//! REST API (v2 `search` or v3 `search/jql`) that lists issues, their
//! attachments and their comments as separate documents.

pub mod config;
#[cfg(feature = "content-source")]
pub mod source;
