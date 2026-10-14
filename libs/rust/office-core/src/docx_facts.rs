//! A read-only fact sheet of a docx package (ADR-0032 P4): what the docx
//! toolkit needs to plan an edit and to check, on the saved bytes, that the
//! edit is in the file where it was aimed.
//!
//! `Document` exposes paragraphs only as text and has no accessor for a
//! table's place among the body, a paragraph's style, or the pictures and
//! charts a body holds. This module reads them from the package, the way
//! `inspect` does for a deck: the bytes have passed the validation gate (entry,
//! XML-part and depth caps), and this is a streaming read of the canonical
//! archive, not a second library parse. It reads, never writes.
//!
//! The paragraph index space is the one [`crate::Document::set_paragraph_text`]
//! addresses: the `w:p` children of `w:body`, in order, tables and other body
//! content not counted. The table index space is the `w:tbl` children of
//! `w:body`, in order (nested tables belong to their cell's text).

use std::io::{Cursor, Read};

use quick_xml::Reader;
use quick_xml::events::{BytesStart, Event};
use zip::ZipArchive;

use crate::error::{OfficeError, Result};
use crate::inspect::attr;
use crate::limits::Limits;

/// One body paragraph.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocParagraph {
    /// Its text: runs joined, a tab as `\t`, a line break as `\n`.
    pub text: String,
    /// Its style id (`Heading1`), when it has one.
    pub style: Option<String>,
    /// Pictures drawn in it.
    pub pictures: usize,
    /// Charts drawn in it.
    pub charts: usize,
}

/// One body table: the text of every cell, paragraphs of a cell joined by `\n`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocTable {
    /// `rows[r][c]`.
    pub rows: Vec<Vec<String>>,
}

/// One block of the body, in document order.
#[derive(Debug, Clone, PartialEq, Eq)]
#[non_exhaustive]
pub enum DocBlock {
    /// A paragraph.
    Paragraph(DocParagraph),
    /// A table.
    Table(DocTable),
    /// Anything else (a content control, a section break, raw XML).
    Other,
}

/// What a docx body holds.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct DocFacts {
    /// The body's blocks, in order.
    pub blocks: Vec<DocBlock>,
    /// Every picture anywhere in the body (paragraphs and table cells).
    pub pictures: usize,
    /// Every chart anywhere in the body.
    pub charts: usize,
    /// How many package parts of each kind the file holds (`word/header`,
    /// `word/media/image`, ...): digits and the extension dropped from the
    /// name. What a save must not lose.
    pub parts: std::collections::BTreeMap<String, usize>,
}

impl DocFacts {
    /// The body paragraphs, in order (the `set_paragraph_text` index space).
    #[must_use]
    pub fn paragraphs(&self) -> Vec<&DocParagraph> {
        self.blocks
            .iter()
            .filter_map(|b| match b {
                DocBlock::Paragraph(p) => Some(p),
                _ => None,
            })
            .collect()
    }

    /// The body tables, in order.
    #[must_use]
    pub fn tables(&self) -> Vec<&DocTable> {
        self.blocks
            .iter()
            .filter_map(|b| match b {
                DocBlock::Table(t) => Some(t),
                _ => None,
            })
            .collect()
    }
}

/// The fact sheet of a docx package given as bytes, without opening it in the
/// docx library: the package goes through the same validation gate as
/// [`crate::Document::open`] and is then read directly.
///
/// # Errors
/// Whatever the gate refuses, [`OfficeError::Malformed`], [`OfficeError::Busy`].
pub fn facts_of_docx_package(bytes: &[u8], limits: &Limits) -> Result<DocFacts> {
    use crate::validate::{Kind, Mode, precheck_len, validate_and_repack};
    crate::counters::bump(crate::counters::Op::PackageRead);
    precheck_len(bytes.len(), limits)?;
    let (data, l) = (bytes.to_vec(), limits.clone());
    crate::guard::run_guarded(limits, move || {
        let repacked = validate_and_repack(&data, Kind::Docx, Mode::Keep, &l)?;
        read_docx_facts(&repacked.bytes, &l)
    })
}

pub(crate) fn read_docx_facts(package: &[u8], limits: &Limits) -> Result<DocFacts> {
    let mut zip = ZipArchive::new(Cursor::new(package))
        .map_err(|e| OfficeError::Malformed(format!("package: {e}")))?;
    let cap = limits.max_xml_part_bytes;
    let main = main_part_name(&mut zip, cap)?;
    let entry = zip
        .by_name(&main)
        .map_err(|_| OfficeError::Malformed(format!("no {main}")))?;
    let mut xml = Vec::new();
    entry
        .take(cap + 1)
        .read_to_end(&mut xml)
        .map_err(|e| OfficeError::Malformed(format!("part {main}: {e}")))?;
    if xml.len() as u64 > cap {
        return Err(OfficeError::Malformed(format!("part {main} is too large")));
    }
    let mut facts = read_body(&xml);
    facts.parts = part_kinds(&mut zip);
    Ok(facts)
}

/// Count the parts of a package by kind: the name without its extension and
/// without trailing digits (`word/header2.xml` is `word/header`).
pub(crate) fn part_kinds(
    zip: &mut ZipArchive<Cursor<&[u8]>>,
) -> std::collections::BTreeMap<String, usize> {
    let mut out = std::collections::BTreeMap::new();
    for i in 0..zip.len() {
        let Ok(entry) = zip.by_index_raw(i) else {
            continue;
        };
        if let Some(kind) = kind_of(entry.name()) {
            *out.entry(kind).or_insert(0) += 1;
        }
    }
    out
}

/// The kind of a part: its name without extension and trailing digits
/// (`None` for a directory or a relationships part).
pub(crate) fn kind_of(name: &str) -> Option<String> {
    if name.ends_with('/') || name.contains("/_rels/") || name.starts_with("_rels/") {
        return None;
    }
    let stem = name.rsplit_once('.').map_or(name, |(s, _)| s);
    Some(
        stem.trim_end_matches(|c: char| c.is_ascii_digit())
            .to_owned(),
    )
}

/// The main document part, from the package relationships (`word/document.xml`
/// when it cannot be read).
fn main_part_name(zip: &mut ZipArchive<Cursor<&[u8]>>, cap: u64) -> Result<String> {
    let default = "word/document.xml".to_owned();
    let Ok(entry) = zip.by_name("_rels/.rels") else {
        return Ok(default);
    };
    let mut xml = Vec::new();
    entry
        .take(cap + 1)
        .read_to_end(&mut xml)
        .map_err(|e| OfficeError::Malformed(format!("part _rels/.rels: {e}")))?;
    let mut r = Reader::from_reader(xml.as_slice());
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"Relationship" => {
                let kind = attr(&e, b"Type").unwrap_or_default();
                if kind.ends_with("/officeDocument")
                    && let Some(target) = attr(&e, b"Target")
                {
                    return Ok(target.trim_start_matches('/').to_owned());
                }
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    Ok(default)
}

pub(crate) fn entity(name: &str) -> Option<char> {
    match name {
        "lt" => Some('<'),
        "gt" => Some('>'),
        "amp" => Some('&'),
        "quot" => Some('"'),
        "apos" => Some('\''),
        _ => {
            let num = name.strip_prefix('#')?;
            let code = match num.strip_prefix(['x', 'X']) {
                Some(hex) => u32::from_str_radix(hex, 16).ok()?,
                None => num.parse().ok()?,
            };
            char::from_u32(code)
        }
    }
}

/// One paragraph being read (a body paragraph or a paragraph of a cell).
#[derive(Default)]
struct Para {
    text: String,
    style: Option<String>,
    pictures: usize,
    charts: usize,
}

/// A table being read: level 1 builds rows and cells, deeper levels only feed
/// their paragraphs into the level-1 cell.
#[derive(Default)]
struct Tbl {
    rows: Vec<Vec<String>>,
    row: Vec<String>,
    cell: Vec<String>,
}

#[allow(clippy::too_many_lines)]
fn read_body(xml: &[u8]) -> DocFacts {
    let mut out = DocFacts::default();
    let mut r = Reader::from_reader(xml);
    let mut depth = 0usize;
    let mut body_depth: Option<usize> = None;
    // Children of w:body that are not paragraphs or tables.
    let mut other_from: Option<usize> = None;
    let mut para: Option<Para> = None;
    // Depth of the current top-level paragraph's start (to pair its end).
    let mut para_at = 0usize;
    let mut tbl: Option<Tbl> = None;
    let mut tbl_at = 0usize;
    let mut tbl_level = 0usize;
    let mut in_t = false;
    // Only a tab or break INSIDE a run is text; `w:tab` also defines a tab stop
    // in `w:pPr/w:tabs`.
    // A depth, not a flag: a run can hold runs (ruby text and base).
    let mut run_depth = 0usize;
    // Text boxes and fallbacks hold paragraphs of their own, or copies.
    let mut skip_from: Option<usize> = None;
    // Whether the `w:p` being read is a direct child of the body.
    let mut para_is_body = false;
    loop {
        let event = r.read_event();
        match event {
            Ok(Event::Start(ref e)) => {
                depth += 1;
                let name = e.local_name();
                let name = name.as_ref();
                if skip_from.is_some() {
                    continue;
                }
                if body_depth.is_none() {
                    if name == b"body" {
                        body_depth = Some(depth);
                    }
                    continue;
                }
                if matches!(name, b"txbxContent" | b"Fallback") {
                    skip_from = Some(depth);
                    continue;
                }
                let at_body = body_depth.is_some_and(|b| depth == b + 1);
                match name {
                    b"p" => {
                        if tbl.is_some() || at_body {
                            para = Some(Para::default());
                            para_at = depth;
                            para_is_body = at_body && tbl.is_none();
                        }
                    }
                    b"tbl" => {
                        if at_body && tbl.is_none() {
                            tbl = Some(Tbl::default());
                            tbl_at = depth;
                            tbl_level = 1;
                        } else if tbl.is_some() {
                            tbl_level += 1;
                        }
                    }
                    b"tr" if tbl_level == 1 => {
                        if let Some(t) = tbl.as_mut() {
                            t.row = Vec::new();
                        }
                    }
                    b"tc" if tbl_level == 1 => {
                        if let Some(t) = tbl.as_mut() {
                            t.cell = Vec::new();
                        }
                    }
                    b"r" => run_depth += 1,
                    b"t" if para.is_some() => in_t = true,
                    b"drawing" | b"pict" | b"object" => {}
                    b"blip" => note_picture(&mut para, &mut out),
                    b"chart" => note_chart(e, &mut para, &mut out),
                    b"pStyle" if para_is_body => set_style(e, &mut para),
                    b"tab" if para.is_some() && run_depth > 0 && !in_t => {
                        push_char(&mut para, '\t');
                    }
                    b"br" | b"cr" if para.is_some() && run_depth > 0 => push_char(&mut para, '\n'),
                    _ => {
                        if at_body && tbl.is_none() && para.is_none() {
                            other_from = Some(depth);
                        }
                    }
                }
            }
            Ok(Event::Empty(ref e)) => {
                let name = e.local_name();
                let name = name.as_ref();
                if skip_from.is_some() || body_depth.is_none() {
                    continue;
                }
                let at_body = body_depth.is_some_and(|b| depth == b);
                match name {
                    b"p" if at_body && tbl.is_none() => {
                        out.blocks.push(DocBlock::Paragraph(DocParagraph {
                            text: String::new(),
                            style: None,
                            pictures: 0,
                            charts: 0,
                        }));
                    }
                    b"p" if tbl.is_some() && para.is_none() => {
                        if let Some(t) = tbl.as_mut() {
                            t.cell.push(String::new());
                        }
                    }
                    b"blip" => note_picture(&mut para, &mut out),
                    b"chart" => note_chart(e, &mut para, &mut out),
                    b"pStyle" if para_is_body => set_style(e, &mut para),
                    b"tab" if para.is_some() && run_depth > 0 && !in_t => {
                        push_char(&mut para, '\t');
                    }
                    b"br" | b"cr" if para.is_some() && run_depth > 0 => push_char(&mut para, '\n'),
                    _ => {
                        // A body-level empty element that is not a paragraph or
                        // a table (a bookmark, a section break, ...).
                        if at_body && tbl.is_none() && para.is_none() {
                            out.blocks.push(DocBlock::Other);
                        }
                    }
                }
            }
            Ok(Event::Text(ref t)) if in_t && skip_from.is_none() => {
                if let (Some(p), Ok(s)) = (para.as_mut(), t.decode()) {
                    p.text.push_str(&s);
                }
            }
            Ok(Event::GeneralRef(ref t)) if in_t && skip_from.is_none() => {
                if let (Some(p), Ok(name)) = (para.as_mut(), t.decode())
                    && let Some(c) = entity(&name)
                {
                    p.text.push(c);
                }
            }
            Ok(Event::End(ref e)) => {
                let name = e.local_name();
                let name = name.as_ref();
                if let Some(s) = skip_from {
                    if depth == s {
                        skip_from = None;
                    }
                    depth = depth.saturating_sub(1);
                    continue;
                }
                match name {
                    b"r" => run_depth = run_depth.saturating_sub(1),
                    b"t" => in_t = false,
                    b"p" if para.is_some() && depth == para_at => {
                        let p = para.take().unwrap_or_default();
                        if let Some(t) = tbl.as_mut() {
                            t.cell.push(p.text);
                        } else {
                            out.blocks.push(DocBlock::Paragraph(DocParagraph {
                                text: p.text,
                                style: p.style,
                                pictures: p.pictures,
                                charts: p.charts,
                            }));
                        }
                        para_is_body = false;
                    }
                    b"tc" if tbl_level == 1 => {
                        if let Some(t) = tbl.as_mut() {
                            let cell = std::mem::take(&mut t.cell).join("\n");
                            t.row.push(cell);
                        }
                    }
                    b"tr" if tbl_level == 1 => {
                        if let Some(t) = tbl.as_mut() {
                            let row = std::mem::take(&mut t.row);
                            t.rows.push(row);
                        }
                    }
                    b"tbl" if tbl.is_some() => {
                        if tbl_level > 1 {
                            tbl_level -= 1;
                        } else if depth == tbl_at {
                            let t = tbl.take().unwrap_or_default();
                            out.blocks.push(DocBlock::Table(DocTable { rows: t.rows }));
                            tbl_level = 0;
                        }
                    }
                    _ => {
                        if other_from == Some(depth) {
                            other_from = None;
                            out.blocks.push(DocBlock::Other);
                        }
                    }
                }
                depth = depth.saturating_sub(1);
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    out
}

fn push_char(para: &mut Option<Para>, c: char) {
    if let Some(p) = para.as_mut() {
        p.text.push(c);
    }
}

fn set_style(e: &BytesStart<'_>, para: &mut Option<Para>) {
    if let (Some(p), Some(v)) = (para.as_mut(), attr(e, b"val")) {
        p.style = Some(v);
    }
}

fn note_picture(para: &mut Option<Para>, out: &mut DocFacts) {
    out.pictures += 1;
    if let Some(p) = para.as_mut() {
        p.pictures += 1;
    }
}

/// `c:chart` (a reference to a chart part) counts; another namespace's element
/// of the same local name does not.
fn note_chart(e: &BytesStart<'_>, para: &mut Option<Para>, out: &mut DocFacts) {
    if e.name().as_ref() != b"c:chart" {
        return;
    }
    out.charts += 1;
    if let Some(p) = para.as_mut() {
        p.charts += 1;
    }
}
