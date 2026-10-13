//! Error type for the high-level presentation API.

use thiserror::Error;

#[derive(Debug, Error)]
pub enum SlideError {
    #[error("OPC package error: {0}")]
    Opc(#[from] zavora_slide_opc::OpcError),

    #[error("OOXML error: {0}")]
    Oxml(#[from] zavora_slide_oxml::OxmlError),

    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),

    #[error("not found: {0}")]
    NotFound(String),

    #[error("invalid input: {0}")]
    InvalidInput(String),

    #[error("unsupported: {0}")]
    Unsupported(String),
}

pub type Result<T> = std::result::Result<T, SlideError>;
