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
//! Caladea and Liberation fonts. `zavora-slide-render` and `zavora-slide-pdf`
//! 0.1.1 called `fontdb::Database::load_system_fonts()` unconditionally; they
//! are vendored under `libs/rust/vendor` with those calls removed (ADR-0032
//! Risks; `tests/vendor_patches.rs` pins it), so a pptx preview or PDF uses the
//! bundled Liberation Sans on every host. A deck that names a font we do not
//! bundle (Calibri, Arial, others) renders with that substitute: previews are
//! approximate, and the same on every host.
//!
//! # Vendored library fixes
//!
//! The same vendored `zavora-slide` carries the fix that lets a table, a chart
//! or a picture added to a deck opened from bytes survive the save beside what
//! the file already held (see `libs/rust/vendor/README.md`). Two library save
//! paths remain lossy for an opened deck: the first speaker notes added to a
//! slide, and [`Deck::apply_theme`], make the library rewrite the whole file
//! from its build model. A caller that edits opened decks must reopen the saved
//! bytes and compare [`Deck::facts`] before and after, as the pptx toolkit does.
//!
//! # Not covered
//!
//! * There is no deadline cancellation: past [`Limits::call_deadline`] the
//!   caller gets [`OfficeError::DeadlineExceeded`] and the thread keeps its
//!   permit until it ends.
//! * `SmartArt` and other parts the libraries do not model are kept as opaque
//!   bytes by the surgical pptx save, not edited.

mod a1;
mod counters;
mod deck;
mod document;
mod docx_facts;
mod error;
mod guard;
mod image;
mod inspect;
mod limits;
mod validate;
mod workbook;
mod xlsx_facts;

pub use a1::{cell_ref, column_name, parse_cell_ref, parse_range, range_ref};
pub use deck::{
    Bullet, ChartKind, ChartSpec, Deck, ParagraphInfo, PlaceholderRef, Rect, RunInfo, Series,
    ShapeInfo, SlideLayout, Theme,
};
pub use document::{Document, TableData};
pub use docx_facts::{DocBlock, DocFacts, DocParagraph, DocTable, facts_of_docx_package};
pub use error::{Limit, OfficeError, Result};
pub use guard::{Health, health};
pub use inspect::{ChartFacts, ShapeFacts, ShapeKind, SlideFacts, TableFacts, facts_of_package};
pub use limits::{Limits, PARSE_STACK_BYTES};
pub use workbook::{CellRange, CellValue, SheetChartSpec, SheetSeries, Workbook};
pub use xlsx_facts::{
    XlsxChart, XlsxFacts, XlsxQuery, XlsxSheet, facts_of_xlsx_package,
    facts_of_xlsx_package_choosing,
};

/// The pixel size of a raster image (PNG, GIF, JPEG, BMP, WebP, TIFF) from its
/// header alone, or `None` when the bytes are not one or the header is
/// inconsistent. Nothing is decoded; the same probe the gate uses.
#[must_use]
pub fn image_size(bytes: &[u8]) -> Option<(u64, u64)> {
    match image::probe(bytes) {
        image::Probe::Raster { width, height, .. } => Some((width, height)),
        _ => None,
    }
}

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

    /// Decoded pixel counts of the raster images in the copy a renderer would
    /// get (after fitting): proves the render budget is met by fitting.
    ///
    /// # Errors
    /// Whatever the gate refuses.
    pub fn render_copy_pixels(
        bytes: &[u8],
        kind: PackageKind,
        limits: &Limits,
    ) -> Result<Vec<u64>> {
        let k = match kind {
            PackageKind::Pptx => Kind::Pptx,
            PackageKind::Docx => Kind::Docx,
            PackageKind::Xlsx => Kind::Xlsx,
        };
        validate_and_repack(bytes, k, Mode::Render { dpi: 72.0 }, limits).map(|r| r.raster_pixels)
    }

    /// The raster images' bytes of the render copy, for decoding in tests.
    ///
    /// # Errors
    /// Whatever the gate refuses.
    pub fn render_copy_bytes(bytes: &[u8], kind: PackageKind, limits: &Limits) -> Result<Vec<u8>> {
        let k = match kind {
            PackageKind::Pptx => Kind::Pptx,
            PackageKind::Docx => Kind::Docx,
            PackageKind::Xlsx => Kind::Xlsx,
        };
        validate_and_repack(bytes, k, Mode::Render { dpi: 72.0 }, limits).map(|r| r.bytes)
    }

    /// The text of one part of a package, for a test that looks at the file.
    #[must_use]
    pub fn part_text(package: &[u8], name: &str) -> Option<String> {
        use std::io::Read;
        let mut zip = zip::ZipArchive::new(std::io::Cursor::new(package)).ok()?;
        let mut out = String::new();
        zip.by_name(name).ok()?.read_to_string(&mut out).ok()?;
        Some(out)
    }

    /// A copy of `package` with the first `from` of the part `name` replaced
    /// by `to`, for a test that needs a file shaped the way a producer shapes it.
    ///
    /// # Panics
    /// When the part or the text is not there.
    #[must_use]
    pub fn replace_in_part(package: &[u8], name: &str, from: &str, to: &str) -> Vec<u8> {
        use std::io::{Read, Write};
        let mut input = zip::ZipArchive::new(std::io::Cursor::new(package)).expect("a zip");
        let mut out = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
        for i in 0..input.len() {
            let mut entry = input.by_index(i).expect("an entry");
            let mut data = Vec::new();
            entry.read_to_end(&mut data).expect("a readable entry");
            if entry.name() == name {
                let text = String::from_utf8(data).expect("a text part");
                assert!(text.contains(from), "{name} has no {from}");
                data = text.replacen(from, to, 1).into_bytes();
            }
            out.start_file(
                entry.name(),
                zip::write::SimpleFileOptions::default()
                    .compression_method(zip::CompressionMethod::Deflated),
            )
            .expect("a new entry");
            out.write_all(&data).expect("a written entry");
        }
        out.finish().expect("a finished zip").into_inner()
    }

    /// `(opens, saves, package_reads)` made on the calling thread since
    /// [`reset_counters`]: what a test asserts a tool does not exceed.
    #[must_use]
    pub fn counters() -> (u64, u64, u64) {
        crate::counters::read()
    }

    /// Zero this thread's counters.
    pub fn reset_counters() {
        crate::counters::reset();
    }

    /// Hold one parse permit for `how_long` on a parse thread, to test
    /// `Busy` and the permit lifetime.
    ///
    /// # Errors
    /// [`crate::OfficeError::Busy`] when no permit is free.
    pub fn hold_permit(
        limits: &Limits,
        how_long: std::time::Duration,
        deadline: std::time::Duration,
    ) -> Result<std::thread::JoinHandle<()>> {
        let (tx, rx) = std::sync::mpsc::channel();
        let l = limits.clone();
        let h = std::thread::spawn(move || {
            let r = crate::guard::run_guarded(
                &Limits {
                    call_deadline: deadline,
                    ..l
                },
                move || {
                    let _ = tx.send(());
                    std::thread::sleep(how_long);
                    Ok(())
                },
            );
            drop(r);
        });
        rx.recv_timeout(std::time::Duration::from_secs(2))
            .map_err(|_| crate::OfficeError::Busy)?;
        Ok(h)
    }

    /// Make the next `Deck::replace_text` fail midway (`restore_fails`: and
    /// fail to restore the snapshot too).
    pub fn fail_next_replace(restore_fails: bool) {
        crate::deck::REPLACE_FAULT.store(
            if restore_fails { 2 } else { 1 },
            std::sync::atomic::Ordering::SeqCst,
        );
    }

    /// Make the next `Document::set_paragraph_text` fail after tagging.
    pub fn fail_next_paragraph_mapping() {
        crate::document::PARAGRAPH_FAULT.store(true, std::sync::atomic::Ordering::SeqCst);
    }

    /// Make the next guarded call sleep `ms` first on its parse thread.
    pub fn delay_next_guarded_call(ms: u64) {
        crate::guard::TEST_DELAY_MS.store(ms, std::sync::atomic::Ordering::SeqCst);
    }

    /// Panic inside a guarded call on the deck, poisoning its state.
    pub fn poison_deck(deck: &crate::Deck) {
        let _ = deck.poison_for_test();
    }

    /// Limits for fuzzing and corpus replay: small enough that one input
    /// cannot take a fuzzer worker down, with room on the permit gate.
    #[must_use]
    pub fn fuzz_limits() -> Limits {
        Limits {
            max_input_bytes: 2 << 20,
            max_xml_part_bytes: 1 << 20,
            max_xml_total_bytes: 2 << 20,
            max_xml_large_part_bytes_xlsx: 2 << 20,
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
        finding(
            "replace_all",
            d.replace_all(&[("e".to_owned(), "a\nb".to_owned())]),
        );
        finding("facts", crate::facts_of_docx_package(data, &l));
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
        finding(
            "facts",
            crate::facts_of_xlsx_package(
                data,
                &l,
                &crate::XlsxQuery {
                    cells: &[(0, 0, 0), (0, 1, 1)],
                    ranges: &[],
                    sheet_mention: Some("Sheet1"),
                },
            ),
        );
    }
}
