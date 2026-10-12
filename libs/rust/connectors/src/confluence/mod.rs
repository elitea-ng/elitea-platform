//! Confluence (ADR-0030 I2): the toolkit configuration the connector reads,
//! the `index_data` arguments it honours, and the `ContentSource` over the
//! Confluence REST API v1 that lists pages (by space, CQL, label or id), their
//! attachments and their comments as separate documents.

pub mod config;
#[cfg(feature = "content-source")]
pub mod source;
