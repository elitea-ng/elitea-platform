//! The one error type of the crate. No vendor error type appears in it: every
//! library error is turned into a message here, so a library bump cannot change
//! a signature (ADR-0032 verification item 4).

use std::fmt;

/// Which limit a [`OfficeError::LimitExceeded`] names. See [`crate::Limits`]
/// for the default of each.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum Limit {
    /// Compressed input (or produced output) size, in bytes.
    InputBytes,
    /// Number of archive entries.
    Entries,
    /// Uncompressed size of one XML part, in bytes.
    XmlPartBytes,
    /// Total uncompressed XML, in bytes.
    XmlTotalBytes,
    /// Uncompressed size of one media or opaque part, in bytes.
    MediaPartBytes,
    /// Total uncompressed media and opaque parts, in bytes.
    MediaTotalBytes,
    /// Bytes actually read from an entry exceeded what its header declared.
    ActualBytes,
    /// XML element nesting depth.
    XmlDepth,
    /// Pixels of one raster image.
    ImagePixels,
    /// Pixels decoded or drawn by one render or export call.
    RenderPixels,
    /// Raster images in one document.
    Images,
    /// Slides in one deck.
    Slides,
    /// Sheets in one workbook.
    Sheets,
    /// Cells (table cells, spreadsheet cells, chart points) in one document.
    Cells,
    /// A table cell's `gridSpan` or `rowSpan`.
    TableSpan,
}

impl Limit {
    /// Stable machine name, for tool errors and metrics.
    #[must_use]
    pub fn name(self) -> &'static str {
        match self {
            Limit::InputBytes => "input_bytes",
            Limit::Entries => "entries",
            Limit::XmlPartBytes => "xml_part_bytes",
            Limit::XmlTotalBytes => "xml_total_bytes",
            Limit::MediaPartBytes => "media_part_bytes",
            Limit::MediaTotalBytes => "media_total_bytes",
            Limit::ActualBytes => "actual_bytes",
            Limit::XmlDepth => "xml_depth",
            Limit::ImagePixels => "image_pixels",
            Limit::RenderPixels => "render_pixels",
            Limit::Images => "images",
            Limit::Slides => "slides",
            Limit::Sheets => "sheets",
            Limit::Cells => "cells",
            Limit::TableSpan => "table_span",
        }
    }
}

impl fmt::Display for Limit {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.name())
    }
}

/// Everything `elitea-office-core` can refuse or fail at.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum OfficeError {
    /// A configured [`crate::Limits`] value was exceeded. `value` is the
    /// offending measurement, in the unit of `limit`.
    #[error("limit `{limit}` exceeded (value {value})")]
    LimitExceeded {
        /// The limit that was hit.
        limit: Limit,
        /// The measured value that exceeded it.
        value: u64,
    },
    /// The bytes are not a well-formed package of the expected kind.
    #[error("malformed document: {0}")]
    Malformed(String),
    /// The operation is not offered by the underlying library (or by this
    /// crate yet). Never silently emulated.
    #[error("unsupported: {0}")]
    Unsupported(String),
    /// An index or name does not exist.
    #[error("not found: {0}")]
    NotFound(String),
    /// An argument is invalid.
    #[error("invalid input: {0}")]
    InvalidInput(String),
    /// The call outlived [`crate::Limits::call_deadline`]. The library keeps
    /// running on its thread and keeps its concurrency permit until it exits.
    #[error("call deadline exceeded")]
    DeadlineExceeded,
    /// All concurrency permits are held by running (or abandoned) calls.
    #[error("busy: all document-processing permits are in use")]
    Busy,
    /// A panic in a library, a failed thread spawn, or another bug.
    #[error("internal error: {0}")]
    Internal(String),
}

/// `Result` with [`OfficeError`].
pub type Result<T> = std::result::Result<T, OfficeError>;

pub(crate) fn limit(limit: Limit, value: u64) -> OfficeError {
    OfficeError::LimitExceeded { limit, value }
}

pub(crate) fn malformed(e: impl fmt::Display) -> OfficeError {
    OfficeError::Malformed(e.to_string())
}
