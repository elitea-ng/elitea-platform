//! Error types for the xlsx builder.

use std::io;

/// Errors that can occur when building an `.xlsx` workbook.
#[derive(Debug, thiserror::Error)]
pub enum XlsxError {
    /// An I/O error occurred during ZIP writing.
    #[error("I/O error: {0}")]
    Io(#[from] io::Error),

    /// A ZIP archive error occurred.
    #[error("ZIP error: {0}")]
    Zip(#[from] zip::result::ZipError),
}
