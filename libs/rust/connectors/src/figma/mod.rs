//! Figma (ADR-0030 I3): the toolkit configuration the connector reads, the
//! `index_data` arguments it honours, and the `ContentSource` over the Figma
//! REST API that lists a file's frames (and, on request, its comments) as
//! documents.

pub mod config;
#[cfg(feature = "content-source")]
pub mod source;
#[cfg(all(test, feature = "content-source"))]
mod source_tests;
#[cfg(feature = "content-source")]
mod text;
