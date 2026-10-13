//! `elitea-office-core`: the one crate that names a zavora type (ADR-0032).
//!
//! `pptx`, `docx` and `xlsx` authoring for the Rust worker and the desktop
//! client sits on three young, single-vendor libraries (`zavora-slide`,
//! `zavora-docx`, `zavora-xlsx`). This crate is the boundary around them:
//!
//! * the public API uses only our types ([`Deck`], [`Document`], [`Workbook`],
//!   [`Limits`], [`OfficeError`]) and bytes in, bytes out; no path appears;
//! * the caller's bytes never reach a library. They are validated and
//!   re-packed first (see [`Limits`] and the `validate` module), so the
//!   libraries parse a canonical archive in which every entry was measured;
//! * every call that parses, saves or renders runs on a dedicated thread with
//!   a fixed 16 MiB stack, holding a process-wide permit until the thread
//!   exits ([`health`] reports a runaway);
//! * a library bump changes no signature here; `tests/isolation.rs` asserts it.
//!
//! # Determinism of renders
//!
//! Embedded fonts are emptied in the copy handed to a renderer, so no user
//! font is parsed. `zavora-docx-layout` loads only its bundled Carlito,
//! Caladea and Liberation fonts. **`zavora-slide-render` 0.1.1 calls
//! `fontdb::Database::load_system_fonts()` unconditionally and has no API to
//! turn that off**, so a pptx preview or PDF can differ between hosts with
//! different installed fonts. This crate cannot fix that from outside; the fix
//! is a patch of that crate under `libs/rust/vendor` (ADR-0032 Risks), left to
//! the pptx toolkit phase. Golden previews must therefore run in an image
//! with a fixed font set, or compare structure instead of pixels.
//!
//! # Not covered
//!
//! * There is no deadline cancellation: past [`Limits::call_deadline`] the
//!   caller gets [`OfficeError::DeadlineExceeded`] and the thread keeps its
//!   permit until it ends.
//! * `SmartArt` and other parts the libraries do not model are kept as opaque
//!   bytes by the surgical pptx save, not edited.

mod deck;
mod document;
mod error;
mod guard;
mod image;
mod limits;
mod validate;
mod workbook;

pub use deck::{
    Bullet, ChartKind, ChartSpec, Deck, ParagraphInfo, PlaceholderRef, Rect, RunInfo, Series,
    ShapeInfo, SlideLayout, Theme,
};
pub use document::{Document, TableData};
pub use error::{Limit, OfficeError, Result};
pub use guard::{Health, health};
pub use limits::{Limits, PARSE_STACK_BYTES};
pub use workbook::{CellRange, CellValue, Workbook};

/// Hooks for the fuzz targets and the corpus tests: the validation gate
/// alone, without a library behind it.
#[doc(hidden)]
pub mod testing {
    use crate::error::Result;
    use crate::limits::Limits;
    use crate::validate::{Kind, Mode, validate_and_repack};

    /// The package kinds the gate knows.
    #[derive(Debug, Clone, Copy)]
    pub enum PackageKind {
        /// pptx
        Pptx,
        /// docx
        Docx,
        /// xlsx
        Xlsx,
    }

    /// Run only the validate-and-repack gate; returns the canonical archive.
    ///
    /// # Errors
    /// Whatever the gate refuses.
    pub fn validate(bytes: &[u8], kind: PackageKind, limits: &Limits) -> Result<Vec<u8>> {
        let k = match kind {
            PackageKind::Pptx => Kind::Pptx,
            PackageKind::Docx => Kind::Docx,
            PackageKind::Xlsx => Kind::Xlsx,
        };
        validate_and_repack(bytes, k, Mode::Keep, limits).map(|r| r.bytes)
    }

    /// Limits for fuzzing and corpus replay: small enough that one input
    /// cannot take a fuzzer worker down, with room on the permit gate.
    #[must_use]
    pub fn fuzz_limits() -> Limits {
        Limits {
            max_input_bytes: 2 << 20,
            max_xml_part_bytes: 1 << 20,
            max_xml_total_bytes: 2 << 20,
            max_xml_total_bytes_xlsx: 4 << 20,
            max_media_part_bytes: 1 << 20,
            max_media_total_bytes: 4 << 20,
            call_deadline: std::time::Duration::from_secs(10),
            max_concurrent: 16,
            ..Limits::default()
        }
    }

    /// A refusal is fine; a panic inside a library, a missed deadline or a
    /// stuck permit is a finding, so it panics here (which a fuzzer reports).
    fn finding<T>(what: &str, r: crate::Result<T>) -> Option<T> {
        use crate::OfficeError::{Busy, DeadlineExceeded, Internal};
        match r {
            Ok(v) => Some(v),
            Err(e @ (Internal(_) | DeadlineExceeded | Busy)) => panic!("{what}: {e}"),
            Err(_) => None,
        }
    }

    /// Fuzz entry: the validation gate alone, as every format.
    pub fn fuzz_gate(data: &[u8]) {
        let l = fuzz_limits();
        for k in [PackageKind::Pptx, PackageKind::Docx, PackageKind::Xlsx] {
            let _ = validate(data, k, &l);
        }
    }

    /// Fuzz entry: open as a deck and use it.
    pub fn fuzz_deck(data: &[u8]) {
        let l = fuzz_limits();
        let Some(mut d) = finding("open", crate::Deck::open(data, &l)) else {
            return;
        };
        finding("markdown", d.to_markdown());
        finding("save", d.save());
        if let Some(n) = finding("count", d.slide_count())
            && n > 0
        {
            finding("shapes", d.shapes(0));
            finding("replace", d.replace_text("e", "E"));
            finding("preview", d.render_preview(0));
            finding("pdf", d.export_pdf());
        }
    }

    /// Fuzz entry: open as a document and use it.
    pub fn fuzz_document(data: &[u8]) {
        let l = fuzz_limits();
        let Some(mut d) = finding("open", crate::Document::open(data, &l)) else {
            return;
        };
        finding("text", d.to_plain_text());
        finding("markdown", d.to_markdown());
        finding("tables", d.tables());
        finding("replace", d.replace_text("e", "E"));
        finding("save", d.save());
        finding("pdf", d.export_pdf());
        finding("png", d.render_page_png(0, 48.0));
    }

    /// Fuzz entry: open as a workbook and use it.
    pub fn fuzz_workbook(data: &[u8]) {
        let l = fuzz_limits();
        let Some(mut w) = finding("open", crate::Workbook::open(data, &l)) else {
            return;
        };
        if let Some(names) = finding("names", w.sheet_names()) {
            for i in 0..names.len() {
                finding("markdown", w.sheet_to_markdown(i));
            }
        }
        finding(
            "write",
            w.write_cell(0, 0, 0, &crate::CellValue::Number(1.0)),
        );
        finding("save", w.save());
    }
}
