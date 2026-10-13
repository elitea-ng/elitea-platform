//! `Document`: a docx (over `zavora-docx`).

use zavora_docx::{Document as ZDocument, Error as ZError, Length};

use crate::deck::{ChartKind, ChartSpec, Rect};
use crate::error::{Limit, OfficeError, Result, limit};
use crate::guard::{STATE_LOST, Shared, run_guarded};
use std::sync::atomic::{AtomicBool, Ordering};

/// Test hook: the next `set_paragraph_text` fails after tagging the paragraph.
pub(crate) static PARAGRAPH_FAULT: AtomicBool = AtomicBool::new(false);
use crate::limits::Limits;
use crate::validate::{Kind, Mode, check_new_image, finish_output, validate_and_repack};

fn map_err(e: ZError) -> OfficeError {
    match e {
        ZError::Opc(e) => OfficeError::Malformed(e.to_string()),
        ZError::Oxml(e) => OfficeError::Malformed(e.to_string()),
        ZError::NoDocumentPart => {
            OfficeError::Malformed("document has no main document part".into())
        }
        ZError::Layout(e) => OfficeError::Unsupported(format!("layout: {e}")),
        other => OfficeError::Internal(other.to_string()),
    }
}

/// The text of a table, row by row.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TableData {
    /// `rows[r][c]` is the text of the cell at row `r`, column `c`.
    pub rows: Vec<Vec<String>>,
}

/// A docx document.
///
/// Parse, save, export and render calls run on the dedicated parse thread;
/// small edits run on the caller's thread.
pub struct Document {
    doc: Shared<ZDocument>,
    limits: Limits,
    images: u64,
    cells: u64,
    /// Whether some text written since open carries a line break, which is
    /// held as [`LINE_BREAK`] in the library and turned into a break run when
    /// the package is serialised.
    breaks: bool,
}

/// What a `\n` is while the library holds the text: the library writes a
/// string into a single `w:t`, where a raw newline is only a space, so a line
/// break is carried as this private-use character and made a `w:br` in the
/// saved parts (see `lines_to_breaks`).
const LINE_BREAK: char = '\u{E002}';
/// Private-use characters this module uses as markers; text that holds one is
/// refused rather than misread.
const RESERVED: std::ops::RangeInclusive<char> = '\u{E000}'..='\u{E4FF}';

/// `text` as the library will hold it: line breaks as [`LINE_BREAK`].
fn encode(text: &str) -> Result<String> {
    if text.chars().any(|c| RESERVED.contains(&c)) {
        return Err(OfficeError::InvalidInput(
            "the text contains a reserved private-use character (U+E000 to U+E4FF)".into(),
        ));
    }
    Ok(text
        .replace("\r\n", "\n")
        .replace('\r', "\n")
        .replace('\n', &LINE_BREAK.to_string()))
}

impl std::fmt::Debug for Document {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Document").finish_non_exhaustive()
    }
}

impl Document {
    /// An empty document.
    #[must_use]
    pub fn new(limits: &Limits) -> Self {
        Self {
            doc: Shared::new(ZDocument::new(), limits),
            limits: limits.clone(),
            images: 0,
            cells: 0,
            breaks: false,
        }
    }

    /// Validate `bytes` (see [`crate::Limits`]) and open them.
    ///
    /// # Errors
    /// [`OfficeError::LimitExceeded`], [`OfficeError::Malformed`],
    /// [`OfficeError::Unsupported`], [`OfficeError::Busy`],
    /// [`OfficeError::DeadlineExceeded`].
    pub fn open(bytes: &[u8], limits: &Limits) -> Result<Self> {
        crate::counters::bump(crate::counters::Op::Open);
        crate::validate::precheck_len(bytes.len(), limits)?;
        let (data, l) = (bytes.to_vec(), limits.clone());
        let (doc, images, cells) = run_guarded(limits, move || {
            let r = validate_and_repack(&data, Kind::Docx, Mode::Keep, &l)?;
            Ok((
                ZDocument::from_bytes(&r.bytes).map_err(map_err)?,
                r.images,
                r.cells,
            ))
        })?;
        Ok(Self {
            doc: Shared::new(doc, limits),
            limits: limits.clone(),
            images,
            cells,
            breaks: false,
        })
    }

    fn charge_cells(&self, more: u64) -> Result<u64> {
        let total = self.cells.saturating_add(more);
        if total > self.limits.max_cells {
            return Err(limit(Limit::Cells, total));
        }
        Ok(total)
    }

    /// Serialise to a validated, canonical docx.
    ///
    /// # Errors
    /// Any limit violation of the produced file.
    pub fn save(&self) -> Result<Vec<u8>> {
        crate::counters::bump(crate::counters::Op::Save);
        let breaks = self.breaks;
        self.doc.guarded(move |d, l| {
            let raw = d.to_bytes().map_err(map_err)?;
            let raw = if breaks {
                lines_to_breaks(&raw, l)?
            } else {
                raw
            };
            finish_output(&raw, Kind::Docx, l)
        })
    }

    /// Append a paragraph.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn add_paragraph(&mut self, text: &str) -> Result<()> {
        let held = encode(text)?;
        self.doc.lock()?.add_paragraph(&held);
        self.breaks |= held != text;
        Ok(())
    }

    /// Append a heading of `level` 1 to 9.
    ///
    /// # Errors
    /// [`OfficeError::InvalidInput`] for a level outside 1 to 9.
    pub fn add_heading(&mut self, text: &str, level: u8) -> Result<()> {
        if !(1..=9).contains(&level) {
            return Err(OfficeError::InvalidInput(
                "heading level must be 1 to 9".into(),
            ));
        }
        let held = encode(text)?;
        self.doc
            .lock()?
            .add_paragraph(&held)
            .style(&format!("Heading{level}"));
        self.breaks |= held != text;
        Ok(())
    }

    /// Replace the text of the `index`th paragraph (0-based, tables and
    /// other body content do not count). The first run's formatting is kept.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad index.
    pub fn set_paragraph_text(&mut self, index: usize, text: &str) -> Result<()> {
        let text = encode(text)?;
        let has_breaks = text.contains(LINE_BREAK);
        self.doc.guarded_mut(move |d, _| {
            const MARKER: &str = "\u{E000}office-core-marker-9c1d\u{E000}";
            if d.find_content_index(MARKER).is_some() {
                return Err(OfficeError::Unsupported(
                    "the document contains the internal marker text".into(),
                ));
            }
            // Snapshot before the tag goes in: the library cannot remove a run
            // it added, so every failure after tagging restores this.
            let snapshot = d.to_bytes().map_err(map_err)?;
            let outcome = (|| {
                d.paragraph_mut(index)
                    .ok_or_else(|| OfficeError::NotFound(format!("paragraph {index}")))?
                    .add_run(MARKER);
                if PARAGRAPH_FAULT.swap(false, Ordering::SeqCst) {
                    return Err(OfficeError::Internal("injected failure".into()));
                }
                // The library addresses body content (paragraphs, tables, raw
                // blocks) by one index. The tag tells which one is ours;
                // setting the text drops it.
                match d.find_content_index(MARKER) {
                    Some(c) if d.set_paragraph_text(c, &text) => Ok(()),
                    _ => Err(OfficeError::Internal(
                        "paragraph index mapping failed".into(),
                    )),
                }
            })();
            if let Err(e) = outcome {
                if d.find_content_index(MARKER).is_some() {
                    return match ZDocument::from_bytes(&snapshot) {
                        Ok(restored) => {
                            *d = restored;
                            Err(e)
                        }
                        Err(_) => Err(OfficeError::Internal(STATE_LOST.into())),
                    };
                }
                return Err(e);
            }
            Ok(())
        })?;
        self.breaks |= has_breaks;
        Ok(())
    }

    /// Append a `rows` by `cols` table; returns its index among the tables.
    ///
    /// # Errors
    /// [`Limit::Cells`] when `rows * cols` exceeds `max_cells`.
    pub fn add_table(&mut self, rows: usize, cols: usize) -> Result<usize> {
        if rows == 0 || cols == 0 {
            return Err(OfficeError::InvalidInput(
                "a table needs at least one row and column".into(),
            ));
        }
        let total = self.charge_cells((rows as u64).saturating_mul(cols as u64))?;
        let idx = {
            let mut d = self.doc.lock()?;
            let idx = d.table_count();
            // Thin grey rules on every edge: a table with no borders is
            // hard to read in a page render.
            d.add_table(rows, cols)
                .borders(zavora_docx::BorderStyle::Single, 4, "808080");
            idx
        };
        self.cells = total;
        Ok(idx)
    }

    /// Set the text of a table cell.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad table or cell.
    pub fn set_table_cell(
        &mut self,
        table: usize,
        row: usize,
        col: usize,
        text: &str,
    ) -> Result<()> {
        let held = encode(text)?;
        let mut d = self.doc.lock()?;
        let mut t = d
            .table_mut(table)
            .ok_or_else(|| OfficeError::NotFound(format!("table {table}")))?;
        let mut c = t
            .cell(row, col)
            .ok_or_else(|| OfficeError::NotFound(format!("cell ({row},{col}) of table {table}")))?;
        c.set_text(&held);
        drop(d);
        self.breaks |= held != text;
        Ok(())
    }

    /// Style a table's first row as its header: bold text on a light fill,
    /// repeated at the top of each page. Call it after the cells have text.
    ///
    /// # Errors
    /// [`OfficeError::NotFound`] for a bad table.
    pub fn style_table_header(&mut self, table: usize) -> Result<()> {
        let mut d = self.doc.lock()?;
        let mut t = d
            .table_mut(table)
            .ok_or_else(|| OfficeError::NotFound(format!("table {table}")))?;
        t.header_row_style("D9E2F3", "000000");
        Ok(())
    }

    /// The text of every table.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn tables(&self) -> Result<Vec<TableData>> {
        let d = self.doc.lock()?;
        Ok(d.tables()
            .iter()
            .map(|t| TableData {
                rows: (0..t.row_count())
                    .map(|r| {
                        (0..t.column_count())
                            .map(|c| t.cell(r, c).map(|c| c.text()).unwrap_or_default())
                            .collect()
                    })
                    .collect(),
            })
            .collect())
    }

    /// Replace every occurrence of `find` (also when it is split across runs)
    /// in the body, tables, headers, footers and text boxes; returns the
    /// count. The replacement is never searched again, so a replacement that
    /// contains `find` does not loop.
    ///
    /// # Errors
    /// [`OfficeError::InvalidInput`] for an empty `find`.
    pub fn replace_text(&mut self, find: &str, replacement: &str) -> Result<usize> {
        let counts = self.replace_all(&[(find.to_owned(), replacement.to_owned())])?;
        Ok(counts.first().copied().unwrap_or(0))
    }

    /// Replace many texts in one pass over the ORIGINAL text: at each place the
    /// longest key that starts there wins (leftmost-longest), and a
    /// replacement is never searched again, so one key's output cannot feed
    /// another. Matches across runs keep the first run's formatting; a `\n` in
    /// a replacement becomes a line break. Returns, per pair in the order
    /// given, how many times it matched (body, tables, headers, footers and
    /// text boxes).
    ///
    /// Two keys may not overlap partially (a suffix of one being a prefix of
    /// the other, neither containing the other): which of them wins would
    /// depend on the order the library visits runs in, so it is refused before
    /// anything is edited. On any error the document is left as it was.
    ///
    /// # Errors
    /// [`OfficeError::InvalidInput`] for an empty or repeated key, more than
    /// 100 pairs, a reserved character, or overlapping keys.
    pub fn replace_all(&mut self, pairs: &[(String, String)]) -> Result<Vec<usize>> {
        if pairs.len() > MAX_REPLACEMENTS {
            return Err(OfficeError::InvalidInput(format!(
                "at most {MAX_REPLACEMENTS} replacements at a time"
            )));
        }
        plan_replacements(pairs)?;
        let values: Vec<String> = pairs
            .iter()
            .map(|(_, v)| encode(v))
            .collect::<Result<_>>()?;
        let keys: Vec<String> = pairs.iter().map(|(k, _)| k.clone()).collect();
        let has_breaks = values.iter().any(|v| v.contains(LINE_BREAK));
        let counts = self.doc.guarded_mut(move |d, _| {
            // Longest first; ties in byte order, so the result is the same
            // however the pairs were listed.
            let mut order: Vec<usize> = (0..keys.len()).collect();
            order.sort_by(|&a, &b| {
                keys[b]
                    .chars()
                    .count()
                    .cmp(&keys[a].chars().count())
                    .then_with(|| keys[a].cmp(&keys[b]))
            });
            let snapshot = d.to_bytes().map_err(map_err)?;
            let outcome = (|| {
                let mut counts = vec![0usize; keys.len()];
                for &i in &order {
                    counts[i] = d.replace_text(&keys[i], &sentinel(i));
                }
                for &i in &order {
                    if counts[i] == 0 {
                        continue;
                    }
                    let n = d.replace_text(&sentinel(i), &values[i]);
                    if n != counts[i] {
                        return Err(OfficeError::Internal(format!(
                            "replacement bookkeeping failed ({} of {n})",
                            counts[i]
                        )));
                    }
                }
                Ok(counts)
            })();
            match outcome {
                Ok(counts) => Ok(counts),
                Err(e) => match ZDocument::from_bytes(&snapshot) {
                    Ok(restored) => {
                        *d = restored;
                        Err(e)
                    }
                    Err(_) => Err(OfficeError::Internal(STATE_LOST.into())),
                },
            }
        })?;
        self.breaks |= has_breaks;
        Ok(counts)
    }

    /// The heading outline as `(level, text)`.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn headings(&self) -> Result<Vec<(u32, String)>> {
        Ok(self.doc.lock()?.headings())
    }

    /// Insert an inline PNG, JPEG or GIF. The signature and header are
    /// checked first, and the part name and content type come from the
    /// sniffed format, never from the caller.
    ///
    /// # Errors
    /// [`Limit::ImagePixels`], [`Limit::Images`], [`Limit::MediaPartBytes`],
    /// [`OfficeError::InvalidInput`].
    pub fn add_image(&mut self, data: &[u8], size: Rect) -> Result<()> {
        let ext = check_new_image(data, self.images, &self.limits)?;
        self.doc.lock()?.add_picture(
            data,
            &format!("image.{ext}"),
            Length::emu(size.w),
            Length::emu(size.h),
        );
        self.images += 1;
        Ok(())
    }

    /// Append a chart (pptx-only kinds return `Unsupported`). The page
    /// renderer may not draw it (ADR-0032 Consequences); it is real in the
    /// saved file.
    ///
    /// # Errors
    /// [`OfficeError::Unsupported`] for stacked bars and doughnuts.
    pub fn add_chart(&mut self, spec: &ChartSpec, size: Rect) -> Result<()> {
        let points = spec.check(&self.limits)?;
        let total = self.charge_cells(points)?;
        let kind = match spec.kind {
            ChartKind::ClusteredBar => zavora_docx::ChartKind::Bar,
            ChartKind::ClusteredColumn => zavora_docx::ChartKind::Column,
            ChartKind::Line => zavora_docx::ChartKind::Line,
            ChartKind::Pie => zavora_docx::ChartKind::Pie,
            ChartKind::Area => zavora_docx::ChartKind::Area,
            ChartKind::Scatter => zavora_docx::ChartKind::Scatter,
            ChartKind::StackedBar | ChartKind::StackedColumn | ChartKind::Doughnut => {
                return Err(OfficeError::Unsupported(
                    "docx charts support bar, column, line, pie, area and scatter".into(),
                ));
            }
        };
        let chart = zavora_docx::Chart {
            kind,
            title: spec.title.clone(),
            categories: spec.categories.clone(),
            series: spec
                .series
                .iter()
                .map(|s| zavora_docx::Series {
                    name: s.name.clone(),
                    values: s.values.clone(),
                })
                .collect(),
            labels: None,
        };
        self.doc
            .lock()?
            .add_chart(&chart, Length::emu(size.w), Length::emu(size.h));
        self.cells = total;
        Ok(())
    }

    /// All text in document order, table cells one per line.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn to_plain_text(&self) -> Result<String> {
        Ok(self.doc.lock()?.to_plain_text())
    }

    /// The document as Markdown.
    ///
    /// # Errors
    /// [`OfficeError::Busy`].
    pub fn to_markdown(&self) -> Result<String> {
        self.doc.guarded(|d, _| Ok(d.to_markdown()))
    }

    /// Export to PDF. Uses the bundled font set only (Carlito, Caladea,
    /// Liberation): embedded document fonts are emptied before the library
    /// runs, and `zavora-docx-layout` loads no system fonts.
    ///
    /// # Errors
    /// [`Limit::RenderPixels`].
    pub fn export_pdf(&self) -> Result<Vec<u8>> {
        self.doc
            .guarded(|d, l| render_copy(d, l, 0.0)?.to_pdf().map_err(map_err))
    }

    /// Render one page (0-based) to PNG at `dpi` (36 to 300).
    ///
    /// # Errors
    /// [`Limit::RenderPixels`] when the page would exceed the pixel budget,
    /// [`OfficeError::NotFound`] for a page past the end.
    pub fn render_page_png(&self, page: usize, dpi: f64) -> Result<Vec<u8>> {
        if !(36.0..=300.0).contains(&dpi) {
            return Err(OfficeError::InvalidInput("dpi must be 36 to 300".into()));
        }
        self.doc.guarded(move |d, l| {
            render_copy(d, l, dpi)?
                .render_page_to_png(page, dpi)
                .map_err(map_err)?
                .ok_or_else(|| OfficeError::NotFound(format!("page {page}")))
        })
    }
}

/// The most pairs one [`Document::replace_all`] takes. Each pair is two passes (about 20 ms each on a 3000-paragraph document)
/// over the document in the library.
const MAX_REPLACEMENTS: usize = 100;

/// The marker that stands for pair `i` between the two passes: reserved
/// characters only, so no key can match any part of it.
fn sentinel(i: usize) -> String {
    let id = char::from_u32(0xE100 + u32::try_from(i).unwrap_or(0)).unwrap_or('\u{E100}');
    format!("\u{E000}{id}\u{E001}")
}

/// Check the keys of a replacement set before anything is edited.
fn plan_replacements(pairs: &[(String, String)]) -> Result<()> {
    let mut seen = std::collections::HashSet::new();
    for (key, value) in pairs {
        if key.is_empty() {
            return Err(OfficeError::InvalidInput(
                "the text to find is empty".into(),
            ));
        }
        if key
            .chars()
            .chain(value.chars())
            .any(|c| RESERVED.contains(&c))
        {
            return Err(OfficeError::InvalidInput(
                "the text contains a reserved private-use character (U+E000 to U+E4FF)".into(),
            ));
        }
        if !seen.insert(key.as_str()) {
            return Err(OfficeError::InvalidInput(format!("`{key}` is given twice")));
        }
    }
    for (i, (a, _)) in pairs.iter().enumerate() {
        for (b, _) in &pairs[i + 1..] {
            if partly_overlap(a, b) {
                return Err(OfficeError::InvalidInput(format!(
                    "the texts `{a}` and `{b}` overlap (the end of one is the start of the other), so which would win is not defined; replace them in separate calls"
                )));
            }
        }
    }
    Ok(())
}

/// Whether a suffix of one text is a prefix of the other, neither containing
/// the other.
fn partly_overlap(a: &str, b: &str) -> bool {
    if a.contains(b) || b.contains(a) {
        return false;
    }
    let tail_head = |x: &str, y: &str| {
        x.char_indices()
            .skip(1)
            .any(|(at, _)| y.starts_with(&x[at..]))
    };
    tail_head(a, b) || tail_head(b, a)
}

/// Turn every [`LINE_BREAK`] inside a `w:t` of the package's `word/*.xml`
/// parts into a break run: `<w:t>a<LB>b</w:t>` becomes
/// `<w:t>a</w:t><w:br/><w:t>b</w:t>`, which is valid wherever the text was.
fn lines_to_breaks(package: &[u8], l: &Limits) -> Result<Vec<u8>> {
    use std::io::{Cursor, Read, Write};
    use zip::{CompressionMethod, ZipArchive, ZipWriter, write::SimpleFileOptions};
    let mut input = ZipArchive::new(Cursor::new(package))
        .map_err(|e| OfficeError::Malformed(format!("package: {e}")))?;
    let mut out = ZipWriter::new(Cursor::new(Vec::new()));
    let marker = LINE_BREAK.to_string();
    for i in 0..input.len() {
        let entry = input
            .by_index(i)
            .map_err(|e| OfficeError::Malformed(format!("package: {e}")))?;
        if entry.is_dir() {
            continue;
        }
        let name = entry.name().to_owned();
        let mut data = Vec::new();
        entry
            .take(l.max_input_bytes.saturating_mul(4))
            .read_to_end(&mut data)
            .map_err(|e| OfficeError::Malformed(format!("part {name}: {e}")))?;
        if name.starts_with("word/")
            && std::path::Path::new(&name)
                .extension()
                .is_some_and(|ext| ext.eq_ignore_ascii_case("xml"))
            && let Ok(text) = std::str::from_utf8(&data)
            && text.contains(&marker)
        {
            data = split_text_nodes(text).into_bytes();
        }
        out.start_file(
            name.as_str(),
            SimpleFileOptions::default().compression_method(CompressionMethod::Deflated),
        )
        .map_err(|e| OfficeError::Internal(format!("repack: {e}")))?;
        out.write_all(&data)
            .map_err(|e| OfficeError::Internal(format!("repack: {e}")))?;
    }
    out.finish()
        .map(Cursor::into_inner)
        .map_err(|e| OfficeError::Internal(format!("repack: {e}")))
}

/// The XML of a part with each `<w:t ...>text</w:t>` that holds a
/// [`LINE_BREAK`] split around a `<w:br/>`.
fn split_text_nodes(xml: &str) -> String {
    let mut out = String::with_capacity(xml.len() + 64);
    let mut rest = xml;
    while let Some(at) = rest.find("<w:t") {
        let after = &rest[at + 4..];
        let boundary = after.chars().next();
        // `<w:t>` or `<w:t ...>`, not `<w:tab/>`, `<w:tbl>` and the like.
        if !matches!(boundary, Some('>' | ' ')) {
            out.push_str(&rest[..at + 4]);
            rest = after;
            continue;
        }
        let Some(open_end) = after.find('>') else {
            break;
        };
        let body_from = at + 4 + open_end + 1;
        let start_tag = &rest[at..body_from];
        if start_tag.ends_with("/>") {
            out.push_str(&rest[..body_from]);
            rest = &rest[body_from..];
            continue;
        }
        let Some(close) = rest[body_from..].find("</w:t>") else {
            break;
        };
        let text = &rest[body_from..body_from + close];
        out.push_str(&rest[..at]);
        if text.contains(LINE_BREAK) {
            let open = if start_tag.contains("xml:space") {
                start_tag.to_owned()
            } else {
                "<w:t xml:space=\"preserve\">".to_owned()
            };
            let parts: Vec<&str> = text.split(LINE_BREAK).collect();
            for (n, part) in parts.iter().enumerate() {
                if n > 0 {
                    out.push_str("<w:br/>");
                }
                out.push_str(&open);
                out.push_str(part);
                out.push_str("</w:t>");
            }
        } else {
            out.push_str(start_tag);
            out.push_str(text);
            out.push_str("</w:t>");
        }
        rest = &rest[body_from + close + "</w:t>".len()..];
    }
    out.push_str(rest);
    out
}

fn render_copy(d: &mut ZDocument, l: &Limits, dpi: f64) -> Result<ZDocument> {
    let raw = d.to_bytes().map_err(map_err)?;
    let r = validate_and_repack(&raw, Kind::Docx, Mode::Render { dpi }, l)?;
    ZDocument::from_bytes(&r.bytes).map_err(map_err)
}
