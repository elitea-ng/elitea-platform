//! The one [`Limits`] struct (ADR-0032 decision 3).

use std::time::Duration;

const MIB: u64 = 1024 * 1024;

/// Size of the dedicated parse and render thread's stack. Fixed, never taken
/// from a caller (ADR-0032 decision 2). Depth is bounded by
/// [`Limits::max_xml_depth`] long before 16 MiB matters; the stack is the
/// backstop for library recursion that depth does not model.
pub const PARSE_STACK_BYTES: usize = 16 * 1024 * 1024;

/// Every bound `office-core` enforces. Defaults are the ADR-0032 table.
/// Deployment config may override fields per toolkit; a model never can,
/// because no tool argument maps to this struct.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Limits {
    /// Compressed input (and produced output), bytes. Default 50 MiB.
    pub max_input_bytes: u64,
    /// Archive entries. Default 10 000.
    pub max_entries: u64,
    /// One XML part, uncompressed. Default 8 MiB.
    pub max_xml_part_bytes: u64,
    /// All XML parts of a docx or pptx. Default 16 MiB.
    pub max_xml_total_bytes: u64,
    /// All XML parts of an xlsx. Default 64 MiB.
    pub max_xml_total_bytes_xlsx: u64,
    /// One media or opaque part, uncompressed. Default 32 MiB.
    pub max_media_part_bytes: u64,
    /// All media and opaque parts. Default 128 MiB.
    pub max_media_total_bytes: u64,
    /// XML element nesting. Default 256.
    pub max_xml_depth: u64,
    /// Pixels of one raster image, from its header. Default 40 MP.
    pub max_image_pixels: u64,
    /// Pixels of every raster image in a document, charged to one render or
    /// export call, plus the pixels of the page being drawn. Default 100 MP.
    pub max_render_pixels: u64,
    /// Raster images per document. Default 500.
    pub max_images: u64,
    /// Slides per deck. Default 500.
    pub max_slides: u64,
    /// Sheets per workbook. Default 64.
    pub max_sheets: u64,
    /// Cells per document (table cells, sheet cells, chart points).
    /// Default 1 000 000.
    pub max_cells: u64,
    /// A table cell's `gridSpan` or `rowSpan`. Default 1000 (Word allows 63
    /// columns). A span of 2 billion costs a second of layout in a release
    /// build and tens of seconds in a debug build.
    pub max_table_span: u64,
    /// Wall-clock deadline of one call. Default 20 s.
    pub call_deadline: Duration,
    /// Concurrent parse threads per process, counted until each thread
    /// *exits*. Default 2.
    pub max_concurrent: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            max_input_bytes: 50 * MIB,
            max_entries: 10_000,
            max_xml_part_bytes: 8 * MIB,
            max_xml_total_bytes: 16 * MIB,
            max_xml_total_bytes_xlsx: 64 * MIB,
            max_media_part_bytes: 32 * MIB,
            max_media_total_bytes: 128 * MIB,
            max_xml_depth: 256,
            max_image_pixels: 40_000_000,
            max_render_pixels: 100_000_000,
            max_images: 500,
            max_slides: 500,
            max_sheets: 64,
            max_cells: 1_000_000,
            max_table_span: 1000,
            call_deadline: Duration::from_secs(20),
            max_concurrent: 2,
        }
    }
}
