//! Open Packaging Convention (OPC) reader/writer for PresentationML packages.
//!
//! This crate handles the ZIP archive layer of .pptx files, including:
//! - Reading and writing ZIP-based OPC packages
//! - Parsing `[Content_Types].xml`
//! - Parsing `.rels` relationship files
//! - Navigating parts by URI and resolving relationships

mod content_types;
mod error;
mod package;
pub mod relationship;

pub use content_types::{ContentType, ContentTypes};
pub use error::{OpcError, Result};
pub use package::{OpcPackage, PackagePart};
pub use relationship::{Relationship, Relationships, rel_types};
