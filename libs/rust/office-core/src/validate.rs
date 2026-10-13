//! Validate, then re-pack (ADR-0032 decision 3).
//!
//! The libraries never see the caller's bytes. [`validate_and_repack`] reads
//! every entry exactly once through a capped reader, with the platform's `zip`
//! 8.6, measures it, and writes a fresh canonical archive from the validated
//! bytes. Only that archive is handed to a zavora library. A second zip parser
//! (zavora-xlsx links `zip` 2.4.2) therefore reads an archive whose every entry
//! was already measured, so a parser differential cannot reach a different
//! entry than the one checked.
//!
//! A part is classified by its bytes, never by its name: it is XML (and gets
//! the XML caps, depth, DTD and span checks) unless its first bytes are a known
//! binary signature, and a part that is neither XML nor a known binary is
//! opaque (fonts, printer settings). A renamed part cannot pick the larger
//! media cap, and a slide stored as `ppt/media/x.png` is still depth-checked.
//!
//! Refused here, before any library runs: input and entry-count limits (the
//! count is read from the end-of-central-directory record, zip64 included,
//! before the central directory is parsed), duplicate entry names (compared
//! case-insensitively, as OPC part names are), unsafe names, a local header
//! that disagrees with the central directory, encrypted entries, methods other
//! than stored and deflate, trailing data, prefix data and overlapping entries,
//! per-part and total XML and media sizes, bytes beyond what a header
//! declared, XML nesting, DTDs, table spans, table cells, chart points and
//! worksheet cells beyond `max_cells`, and raster pixel counts read from image
//! headers.
//!
//! In render mode the copy for a renderer is fitted rather than refused:
//! raster images are downscaled one at a time so that together they fit the
//! decoded-pixel budget, SVG parts are replaced by an empty SVG, and opaque
//! (font-like) parts are emptied. Saved files keep the originals.

use std::collections::HashSet;
use std::io::{Cursor, Read, Write};

use quick_xml::Reader;
use quick_xml::events::Event;
use zip::CompressionMethod;
use zip::ZipArchive;
use zip::ZipWriter;
use zip::write::SimpleFileOptions;

use crate::error::{Limit, OfficeError, Result, limit, malformed};
use crate::image::{Format, Probe, is_known_binary, probe};
use crate::limits::Limits;

/// Which package kind is being validated; picks the XML caps.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Kind {
    Pptx,
    Docx,
    Xlsx,
}

/// What the validated archive is for.
#[derive(Debug, Clone, Copy, PartialEq)]
pub(crate) enum Mode {
    /// Open for editing or saving: everything is kept byte for byte.
    Keep,
    /// Hand to a renderer or exporter (see the module documentation). `dpi` is
    /// the raster density of a docx page PNG (0 for vector output).
    Render { dpi: f64 },
}

/// The validated, canonical archive and what was measured while reading it.
pub(crate) struct Repacked {
    pub bytes: Vec<u8>,
    /// Raster images found in the package.
    pub images: u64,
    /// Highest `ppt/charts/chartN.xml` number present (0 when none).
    pub max_chart_index: u64,
    /// A worksheet holds `t="inlineStr"` cells, which zavora-xlsx 0.1.2 reads
    /// back as empty strings.
    pub inline_strings: bool,
    /// Table cells and chart points (pptx, docx) or worksheet cells (xlsx).
    pub cells: u64,
    /// Decoded size of every raster image in the returned archive; in render
    /// mode this is after fitting.
    pub raster_pixels: Vec<u64>,
}

const EMPTY_SVG: &[u8] = b"<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"1\" height=\"1\"/>";

/// A real 1x1 transparent PNG, standing in for an image that is over budget
/// and cannot be decoded.
const PLACEHOLDER_PNG: &[u8] = &[
    0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
    0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
    0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
    0x00, 0x05, 0xFE, 0x02, 0xFE, 0xA7, 0x35, 0x81, 0x84, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
    0x44, 0xAE, 0x42, 0x60, 0x82,
];

/// What a part is, decided from its declared type first and its bytes second.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Class {
    /// Declared XML (content type `xml` or `+xml`, or named `.xml`/`.rels`):
    /// strict XML checks whatever its first bytes are.
    Xml,
    /// Not declared as anything, not a known binary: still depth, DTD and span
    /// checked, by a tolerant scanner that needs no well-formed bytes.
    XmlTolerant,
    /// A known binary signature, not declared as XML.
    Binary,
    /// Declared as a font by content type or relationship.
    Font,
}

impl Class {
    fn is_xml(self) -> bool {
        matches!(self, Class::Xml | Class::XmlTolerant)
    }
}

/// Case-insensitive file-extension test.
fn has_ext(name: &str, ext: &str) -> bool {
    // Not `Path::extension`: for `_rels/.rels` the file name is a bare dot-name
    // with no extension at all.
    let (n, e) = (name.len(), ext.len());
    n > e
        && name.as_bytes()[n - e - 1] == b'.'
        && name.as_bytes()[n - e..].eq_ignore_ascii_case(ext.as_bytes())
}

/// `[Content_Types].xml`: Default (by extension) and Override (by part name).
#[derive(Default)]
struct ContentTypes {
    defaults: std::collections::HashMap<String, String>,
    overrides: std::collections::HashMap<String, String>,
}

impl ContentTypes {
    fn parse(xml: &[u8]) -> Self {
        let mut ct = ContentTypes::default();
        let mut r = Reader::from_reader(xml);
        loop {
            match r.read_event() {
                Ok(Event::Empty(e) | Event::Start(e)) => {
                    let name = e.local_name();
                    let kind = name.as_ref();
                    if kind != b"Default" && kind != b"Override" {
                        continue;
                    }
                    let (mut key, mut ty) = (None, None);
                    for a in e.attributes().flatten() {
                        let v = String::from_utf8_lossy(&a.value).to_ascii_lowercase();
                        match a.key.local_name().as_ref() {
                            b"Extension" | b"PartName" => key = Some(v),
                            b"ContentType" => ty = Some(v),
                            _ => {}
                        }
                    }
                    if let (Some(k), Some(t)) = (key, ty) {
                        if kind == b"Default" {
                            ct.defaults.insert(k, t);
                        } else {
                            ct.overrides.insert(k.trim_start_matches('/').to_owned(), t);
                        }
                    }
                }
                Ok(Event::Eof) | Err(_) => break,
                Ok(_) => {}
            }
        }
        ct
    }

    /// The declared content type of a part (lowercase part name), if any.
    fn of(&self, lname: &str) -> Option<&str> {
        if let Some(t) = self.overrides.get(lname) {
            return Some(t);
        }
        let ext = lname.rsplit_once('.')?.1;
        self.defaults.get(ext).map(String::as_str)
    }
}

/// One `Relationship` of a `.rels` part.
struct Rel {
    kind: String,
    /// Resolved, lowercase part name without a leading slash.
    target: String,
}

fn parse_rels(rels_name: &str, xml: &[u8]) -> Vec<Rel> {
    // `a/b/_rels/c.xml.rels` belongs to `a/b/c.xml`; relative targets resolve
    // against `a/b/`. `_rels/.rels` belongs to the package root.
    let base = match rels_name.rfind("_rels/") {
        Some(i) => &rels_name[..i],
        None => "",
    };
    let mut out = Vec::new();
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"Relationship" => {
                let (mut kind, mut target, mut external) = (String::new(), String::new(), false);
                for a in e.attributes().flatten() {
                    let v = String::from_utf8_lossy(&a.value).into_owned();
                    match a.key.local_name().as_ref() {
                        b"Type" => kind = v,
                        b"Target" => target = v,
                        b"TargetMode" => external = v.eq_ignore_ascii_case("external"),
                        _ => {}
                    }
                }
                if !external && !target.is_empty() {
                    out.push(Rel {
                        kind,
                        target: resolve(base, &target),
                    });
                }
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    out
}

/// Resolve a relationship target against a base directory, lowercase.
fn resolve(base: &str, target: &str) -> String {
    let joined = if let Some(abs) = target.strip_prefix('/') {
        abs.to_owned()
    } else {
        format!("{base}{target}")
    };
    let mut parts: Vec<&str> = Vec::new();
    for seg in joined.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            s => parts.push(s),
        }
    }
    parts.join("/").to_ascii_lowercase()
}

/// What the relationships say about the package.
#[derive(Default)]
struct Package {
    content_types: ContentTypes,
    /// The main part (`officeDocument` relationship of `_rels/.rels`).
    main: Option<String>,
    /// Worksheets and shared strings of an xlsx, from the workbook's rels.
    worksheets: HashSet<String>,
    shared_strings: HashSet<String>,
    /// Font parts named by any relationship.
    fonts: HashSet<String>,
}

impl Package {
    fn build(
        content_types: ContentTypes,
        rels: &std::collections::HashMap<String, Vec<Rel>>,
    ) -> Self {
        let mut p = Package {
            content_types,
            ..Package::default()
        };
        for list in rels.values() {
            for r in list {
                if r.kind.ends_with("/font") {
                    p.fonts.insert(r.target.clone());
                }
            }
        }
        p.main = rels
            .get("_rels/.rels")
            .and_then(|l| l.iter().find(|r| r.kind.ends_with("/officeDocument")))
            .map(|r| r.target.clone());
        if let Some(main) = &p.main {
            let (dir, file) = main
                .rsplit_once('/')
                .map_or(("", main.as_str()), |(d, f)| (d, f));
            let rels_name = if dir.is_empty() {
                format!("_rels/{file}.rels")
            } else {
                format!("{dir}/_rels/{file}.rels")
            };
            if let Some(list) = rels.get(&rels_name) {
                for r in list {
                    if r.kind.ends_with("/worksheet") {
                        p.worksheets.insert(r.target.clone());
                    } else if r.kind.ends_with("/sharedStrings") {
                        p.shared_strings.insert(r.target.clone());
                    }
                }
            }
        }
        p
    }

    fn classify(&self, lname: &str, buf: &[u8]) -> Result<Class> {
        let ctype = self.content_types.of(lname).unwrap_or("");
        if self.fonts.contains(lname) || ctype.contains("font") {
            return Ok(Class::Font);
        }
        let named_xml = has_ext(lname, "xml") || has_ext(lname, "rels");
        let declared_xml = ctype.ends_with("xml");
        let class = if named_xml || declared_xml {
            Class::Xml
        } else if is_known_binary(buf) {
            Class::Binary
        } else {
            Class::XmlTolerant
        };
        if class.is_xml() && (buf.starts_with(&[0xFF, 0xFE]) || buf.starts_with(&[0xFE, 0xFF])) {
            return Err(OfficeError::Unsupported("UTF-16 XML parts".into()));
        }
        Ok(class)
    }
}

/// The number at the end of a part's file stem (`slide12.xml` is 12), refused
/// beyond a sane bound so no library can overflow on it.
fn check_part_number(name: &str) -> Result<()> {
    let file = name.rsplit('/').next().unwrap_or(name);
    let stem = file.rsplit_once('.').map_or(file, |(s, _)| s);
    let digits: &str = {
        let n = stem.bytes().rev().take_while(u8::is_ascii_digit).count();
        &stem[stem.len() - n..]
    };
    let trimmed = digits.trim_start_matches('0');
    if trimmed.len() > 7 || trimmed.parse::<u64>().is_ok_and(|v| v > 1_000_000) {
        return Err(OfficeError::Malformed(format!(
            "part number of {name:?} is beyond 1000000"
        )));
    }
    Ok(())
}

fn u16_at(b: &[u8], i: usize) -> Option<u16> {
    Some(u16::from_le_bytes([*b.get(i)?, *b.get(i + 1)?]))
}
fn u32_at(b: &[u8], i: usize) -> Option<u32> {
    Some(u32::from_le_bytes([
        *b.get(i)?,
        *b.get(i + 1)?,
        *b.get(i + 2)?,
        *b.get(i + 3)?,
    ]))
}
fn u64_at(b: &[u8], i: usize) -> Option<u64> {
    let lo = u64::from(u32_at(b, i)?);
    let hi = u64::from(u32_at(b, i + 4)?);
    Some(lo | (hi << 32))
}

/// Read the entry count from the end-of-central-directory record (and its
/// zip64 counterpart) without parsing the central directory, and refuse
/// trailing data after the record.
fn check_eocd(input: &[u8], l: &Limits) -> Result<()> {
    const EOCD: &[u8] = b"PK\x05\x06";
    if input.len() < 22 {
        return Err(OfficeError::Malformed(
            "too short to be a zip archive".into(),
        ));
    }
    let lowest = input.len().saturating_sub(22 + usize::from(u16::MAX));
    let mut pos = None;
    let mut i = input.len() - 22;
    loop {
        if input.get(i..i + 4) == Some(EOCD)
            && let Some(clen) = u16_at(input, i + 20)
            && i + 22 + usize::from(clen) == input.len()
        {
            pos = Some(i);
            break;
        }
        if i == lowest {
            break;
        }
        i -= 1;
    }
    let Some(pos) = pos else {
        return Err(OfficeError::Malformed(
            "no end-of-central-directory record at the end of the file (truncated, or trailing data)".into(),
        ));
    };
    let mut total = u64::from(u16_at(input, pos + 10).unwrap_or(0));
    if total == u64::from(u16::MAX)
        && pos >= 20
        && input.get(pos - 20..pos - 16) == Some(b"PK\x06\x07")
        && let Some(off) = u64_at(input, pos - 12)
        && let Ok(off) = usize::try_from(off)
        && input.get(off..off.saturating_add(4)) == Some(b"PK\x06\x06")
        && let Some(t) = u64_at(input, off + 32)
    {
        total = t;
    }
    if total > l.max_entries {
        return Err(limit(Limit::Entries, total));
    }
    Ok(())
}

fn safe_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 512
        && !name.contains('\0')
        && !name.contains('\\')
        && !name.starts_with('/')
        && !name.split('/').any(|s| s == "..")
}

/// What the streaming pass found in one XML part.
#[derive(Default)]
pub(crate) struct XmlInfo {
    /// The root element is `svg`.
    pub svg: bool,
    /// Table cells and chart points: `tc` and `pt` elements.
    pub table_cells: u64,
    /// Spreadsheet cells: `c` elements (counted only for worksheets).
    pub sheet_cells: u64,
}

fn note_element(name: &[u8], info: &mut XmlInfo) {
    match name {
        b"tc" | b"pt" => info.table_cells += 1,
        b"c" => info.sheet_cells += 1,
        _ => {}
    }
}

/// Stream an XML part: nesting depth, DTDs, spans, balance, and the counts the
/// cell cap needs.
pub(crate) fn check_xml(bytes: &[u8], l: &Limits) -> Result<XmlInfo> {
    let max_depth = l.max_xml_depth;
    let mut reader = Reader::from_reader(bytes);
    let mut depth = 0u64;
    let mut info = XmlInfo::default();
    let mut seen_root = false;
    loop {
        match reader.read_event() {
            Ok(Event::Start(e)) => {
                depth += 1;
                if depth > max_depth {
                    return Err(limit(Limit::XmlDepth, depth));
                }
                if !seen_root {
                    seen_root = true;
                    info.svg = e.local_name().as_ref() == b"svg";
                }
                note_element(e.local_name().as_ref(), &mut info);
                check_span(&e, l)?;
            }
            Ok(Event::Empty(e)) => {
                if !seen_root {
                    seen_root = true;
                    info.svg = e.local_name().as_ref() == b"svg";
                }
                note_element(e.local_name().as_ref(), &mut info);
                check_span(&e, l)?;
            }
            Ok(Event::End(_)) => depth = depth.saturating_sub(1),
            Ok(Event::DocType(_)) => {
                return Err(OfficeError::Malformed(
                    "a DTD is not allowed in an Office part".into(),
                ));
            }
            Ok(Event::Eof) => {
                return if depth == 0 {
                    Ok(info)
                } else {
                    Err(OfficeError::Malformed("XML ends inside an element".into()))
                };
            }
            Ok(_) => {}
            Err(e) => return Err(malformed(format!("XML: {e}"))),
        }
    }
}

/// Attribute `key="value"` pairs of a tag body, loosely.
fn loose_attrs(tag: &[u8]) -> Vec<(Vec<u8>, String)> {
    let mut out = Vec::new();
    let mut i = 0;
    while i < tag.len() {
        let Some(eq) = tag[i..].iter().position(|&b| b == b'=') else {
            break;
        };
        let key_end = i + eq;
        let key_start = tag[i..key_end]
            .iter()
            .rposition(u8::is_ascii_whitespace)
            .map_or(i, |p| i + p + 1);
        let key = &tag[key_start..key_end];
        let local = key.rsplit(|&b| b == b':').next().unwrap_or(key).to_vec();
        let rest = &tag[key_end + 1..];
        let Some(&q) = rest.first().filter(|&&q| q == b'"' || q == b'\'') else {
            i = key_end + 1;
            continue;
        };
        let Some(end) = rest[1..].iter().position(|&b| b == q) else {
            break;
        };
        out.push((local, String::from_utf8_lossy(&rest[1..=end]).into_owned()));
        i = key_end + 1 + end + 2;
    }
    out
}

/// A scanner for parts that are not declared XML and not a known binary: it
/// needs no well-formed bytes (`x<p:sld>` is scanned from the `<`), counts
/// element nesting, refuses DTDs and large spans, and tallies cells. Plain
/// binary data yields little: a `<` counts as a tag only before a name
/// character and a `>` within 64 KiB.
pub(crate) fn scan_tolerant(b: &[u8], l: &Limits) -> Result<XmlInfo> {
    const WINDOW: usize = 64 * 1024;
    let mut info = XmlInfo::default();
    let (mut depth, mut i) = (0u64, 0usize);
    let mut next_gt: Option<usize> = None;
    let mut gt_exhausted = false;
    let mut seen_root = false;
    let find_gt = |from: usize, cache: &mut Option<usize>, done: &mut bool| -> Option<usize> {
        if *done {
            return None;
        }
        if let Some(p) = *cache
            && p >= from
        {
            return Some(p);
        }
        if let Some(p) = b[from..].iter().position(|&c| c == b'>') {
            *cache = Some(from + p);
            *cache
        } else {
            *done = true;
            None
        }
    };
    while let Some(p) = b[i..].iter().position(|&c| c == b'<') {
        let at = i + p;
        i = at + 1;
        let Some(&first) = b.get(i) else { break };
        if first == b'/' {
            depth = depth.saturating_sub(1);
            continue;
        }
        if first == b'!' {
            // A DOCTYPE alone is harmless (macOS printer settings carry a
            // plist one); entity declarations are what make a DTD a weapon.
            if b[i..].starts_with(b"!ENTITY") {
                return Err(OfficeError::Malformed(
                    "entity declarations are not allowed in an Office part".into(),
                ));
            }
            continue;
        }
        if !(first.is_ascii_alphabetic() || first == b'_' || first == b':') {
            continue;
        }
        let Some(gt) = find_gt(i, &mut next_gt, &mut gt_exhausted) else {
            continue;
        };
        if gt - i > WINDOW {
            continue;
        }
        let tag = &b[i..gt];
        let self_closing = tag.last() == Some(&b'/');
        let name_end = tag
            .iter()
            .position(|c| c.is_ascii_whitespace() || *c == b'/')
            .unwrap_or(tag.len());
        let name = &tag[..name_end];
        let local = name.rsplit(|&c| c == b':').next().unwrap_or(name);
        if !seen_root {
            seen_root = true;
            info.svg = local == b"svg";
        }
        if !self_closing {
            depth += 1;
            if depth > l.max_xml_depth {
                return Err(limit(Limit::XmlDepth, depth));
            }
        }
        note_element(local, &mut info);
        if local == b"gridSpan" || local == b"tc" {
            for (key, value) in loose_attrs(&tag[name_end..]) {
                let wanted = if local == b"gridSpan" {
                    key == b"val"
                } else {
                    key == b"gridSpan" || key == b"rowSpan"
                };
                if wanted
                    && let Ok(v) = value.trim().parse::<u64>()
                    && v > l.max_table_span
                {
                    return Err(limit(Limit::TableSpan, v));
                }
            }
        }
    }
    Ok(info)
}

/// `w:gridSpan w:val`, and `gridSpan`/`rowSpan` attributes of `a:tc`.
fn check_span(e: &quick_xml::events::BytesStart<'_>, l: &Limits) -> Result<()> {
    let name = e.local_name();
    let is_docx_span = name.as_ref() == b"gridSpan";
    if !is_docx_span && name.as_ref() != b"tc" {
        return Ok(());
    }
    for a in e.attributes().flatten() {
        let key = a.key.local_name();
        let wanted = if is_docx_span {
            key.as_ref() == b"val"
        } else {
            matches!(key.as_ref(), b"gridSpan" | b"rowSpan")
        };
        if wanted
            && let Some(v) = std::str::from_utf8(&a.value)
                .ok()
                .and_then(|v| v.trim().parse::<u64>().ok())
            && v > l.max_table_span
        {
            return Err(limit(Limit::TableSpan, v));
        }
    }
    Ok(())
}

/// `sldSz` of a presentation part: (cx, cy) in EMU.
fn pptx_slide_size(xml: &[u8]) -> Option<(u64, u64)> {
    let mut reader = Reader::from_reader(xml);
    loop {
        match reader.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"sldSz" => {
                let mut cx = None;
                let mut cy = None;
                for a in e.attributes().flatten() {
                    let v = std::str::from_utf8(&a.value).ok()?.parse::<u64>().ok();
                    match a.key.local_name().as_ref() {
                        b"cx" => cx = v,
                        b"cy" => cy = v,
                        _ => {}
                    }
                }
                return Some((cx?, cy?));
            }
            Ok(Event::Eof) | Err(_) => return None,
            Ok(_) => {}
        }
    }
}

/// Pixels of a `w` by `h` twips page drawn at `dpi` (saturating).
#[allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss
)]
fn page_pixels(w: u64, h: u64, dpi: f64) -> u64 {
    let area = (w as f64 / 1440.0 * dpi) * (h as f64 / 1440.0 * dpi);
    if area >= u64::MAX as f64 {
        u64::MAX
    } else {
        area as u64
    }
}

/// Largest `pgSz` of a document part: (w, h) in twips.
fn docx_page_size(xml: &[u8]) -> Option<(u64, u64)> {
    let mut reader = Reader::from_reader(xml);
    let mut best: Option<(u64, u64)> = None;
    loop {
        match reader.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"pgSz" => {
                let mut w = None;
                let mut h = None;
                for a in e.attributes().flatten() {
                    let v = std::str::from_utf8(&a.value)
                        .ok()
                        .and_then(|s| s.parse::<u64>().ok());
                    match a.key.local_name().as_ref() {
                        b"w" => w = v,
                        b"h" => h = v,
                        _ => {}
                    }
                }
                if let (Some(w), Some(h)) = (w, h)
                    && best.is_none_or(|(bw, bh)| w.saturating_mul(h) > bw.saturating_mul(bh))
                {
                    best = Some((w, h));
                }
            }
            Ok(Event::Eof) | Err(_) => return best,
            Ok(_) => {}
        }
    }
}

/// Refuse an oversized input before it is copied anywhere.
pub(crate) fn precheck_len(len: usize, l: &Limits) -> Result<()> {
    if len as u64 > l.max_input_bytes {
        return Err(limit(Limit::InputBytes, len as u64));
    }
    Ok(())
}

/// Shrink one raster image to at most `budget` pixels (aspect kept) and
/// re-encode it as PNG; an image that cannot be decoded becomes a placeholder.
#[allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss
)]
fn fit_image(data: &[u8], w: u64, h: u64, budget: u64) -> Vec<u8> {
    use image::{ImageFormat, ImageReader};
    let mut reader = ImageReader::new(Cursor::new(data));
    let mut lim = image::Limits::default();
    lim.max_alloc = Some(768 << 20);
    reader.limits(lim);
    let Ok(reader) = reader.with_guessed_format() else {
        return PLACEHOLDER_PNG.to_vec();
    };
    let Ok(img) = reader.decode() else {
        return PLACEHOLDER_PNG.to_vec();
    };
    let scale = (budget as f64 / (w.saturating_mul(h)).max(1) as f64).sqrt();
    let nw = ((w as f64 * scale).floor() as u32).max(1);
    let nh = ((h as f64 * scale).floor() as u32).max(1);
    let small = img.thumbnail_exact(nw, nh);
    drop(img);
    let mut out = Cursor::new(Vec::new());
    if small.write_to(&mut out, ImageFormat::Png).is_err() {
        return PLACEHOLDER_PNG.to_vec();
    }
    out.into_inner()
}

/// The per-image pixel allowance of a render copy holding `n` rasters.
fn image_allowance(n: u64, l: &Limits) -> u64 {
    if n <= 16 {
        l.render_image_max_pixels
    } else {
        (l.render_decoded_pixels / n)
            .max(l.render_image_min_pixels)
            .min(l.render_image_max_pixels)
    }
}

struct Meta {
    index: usize,
    name: String,
    declared: u64,
}

struct Part {
    name: String,
    class: Class,
    data: Vec<u8>,
}

/// A raster found in a package: its position, and its size when readable.
struct Raster {
    pos: usize,
    dims: Option<(u64, u64)>,
}

/// Validate `input` as a package of `kind` and return a canonical re-packed
/// archive. See the module documentation for what is refused.
#[allow(clippy::too_many_lines, clippy::many_single_char_names)]
pub(crate) fn validate_and_repack(
    input: &[u8],
    kind: Kind,
    mode: Mode,
    l: &Limits,
) -> Result<Repacked> {
    precheck_len(input.len(), l)?;
    check_eocd(input, l)?;
    let mut archive = ZipArchive::new(Cursor::new(input)).map_err(malformed)?;
    let n = archive.len();
    if n as u64 > l.max_entries {
        return Err(limit(Limit::Entries, n as u64));
    }

    // Pass 1: metadata only. Nothing is decompressed. The class of a part is
    // not known yet, so each entry is held to the largest cap any class could
    // give it; the class caps are applied in pass 2.
    let xlsx = kind == Kind::Xlsx;
    let xml_total_cap = if xlsx {
        l.max_xml_total_bytes_xlsx
    } else {
        l.max_xml_total_bytes
    };
    let largest_xml = if xlsx {
        l.max_xml_large_part_bytes_xlsx
    } else {
        l.max_xml_part_bytes
    };
    let generic_cap = largest_xml.max(l.max_media_part_bytes);
    let mut seen = HashSet::new();
    let mut metas = Vec::with_capacity(n);
    let mut ranges: Vec<(u64, u64)> = Vec::with_capacity(n);
    let mut declared_total = 0u64;
    for index in 0..n {
        let f = archive.by_index_raw(index).map_err(malformed)?;
        let name = f.name().to_owned();
        if !safe_name(&name) {
            return Err(OfficeError::Malformed(format!(
                "unsafe entry name {name:?}"
            )));
        }
        check_part_number(&name)?;
        if f.encrypted() {
            return Err(OfficeError::Unsupported("encrypted archive entry".into()));
        }
        if !matches!(
            f.compression(),
            CompressionMethod::Stored | CompressionMethod::Deflated
        ) {
            return Err(OfficeError::Unsupported(format!(
                "compression method {:?} (only stored and deflate are accepted)",
                f.compression()
            )));
        }
        if !seen.insert(name.to_ascii_lowercase()) {
            return Err(OfficeError::Malformed(format!("duplicate entry {name:?}")));
        }
        // The local header must agree with the central directory.
        let hs = f.header_start();
        let local = usize::try_from(hs)
            .ok()
            .and_then(|h| input.get(h..h.checked_add(30)?));
        let Some(local) = local else {
            return Err(OfficeError::Malformed(
                "local header outside the file".into(),
            ));
        };
        let (nlen, elen) = (
            usize::from(u16_at(local, 26).unwrap_or(0)),
            usize::from(u16_at(local, 28).unwrap_or(0)),
        );
        let flags = u16_at(local, 6).unwrap_or(0);
        let Some(data_start) = f.data_start() else {
            return Err(OfficeError::Malformed("entry data offset unknown".into()));
        };
        let ok_sig = local.starts_with(b"PK\x03\x04");
        let h = usize::try_from(hs).unwrap_or(usize::MAX);
        let local_name = input.get(h + 30..h + 30 + nlen);
        let method_ok = u16_at(local, 8)
            == Some(match f.compression() {
                CompressionMethod::Stored => 0,
                _ => 8,
            });
        let sizes_ok = flags & 8 != 0
            || ((u32_at(local, 18) == Some(0xFFFF_FFFF)
                || u64::from(u32_at(local, 18).unwrap_or(0)) == f.compressed_size())
                && (u32_at(local, 22) == Some(0xFFFF_FFFF)
                    || u64::from(u32_at(local, 22).unwrap_or(0)) == f.size()));
        if !ok_sig
            || local_name != Some(f.name_raw())
            || !method_ok
            || !sizes_ok
            || data_start != hs + 30 + nlen as u64 + elen as u64
        {
            return Err(OfficeError::Malformed(format!(
                "local header of {name:?} disagrees with the central directory"
            )));
        }
        ranges.push((hs, data_start.saturating_add(f.compressed_size())));
        if f.is_dir() || name.ends_with('/') {
            continue;
        }
        let declared = f.size();
        if declared > generic_cap {
            return Err(limit(Limit::XmlPartBytes, declared));
        }
        declared_total = declared_total.saturating_add(declared);
        if declared_total > xml_total_cap.saturating_add(l.max_media_total_bytes) {
            return Err(limit(Limit::MediaTotalBytes, declared_total));
        }
        metas.push(Meta {
            index,
            name,
            declared,
        });
    }
    // No prefix data, no overlap between entries.
    ranges.sort_unstable();
    if ranges.first().is_some_and(|r| r.0 != 0) {
        return Err(OfficeError::Malformed("data before the first entry".into()));
    }
    if ranges.windows(2).any(|w| w[1].0 < w[0].1) {
        return Err(OfficeError::Malformed("overlapping archive entries".into()));
    }

    // Pass 2a: the parts that say what the other parts are (content types and
    // relationships), read first. Pass 2b: every other part, classified by its
    // declared type, then by its bytes.
    let mut state = Pass2 {
        xlsx,
        xml_total: 0,
        media_total: 0,
        images: 0,
        cells: 0,
        max_chart_index: 0,
        inline_strings: false,
        rasters: Vec::new(),
    };
    let mut slots: Vec<Option<Part>> = (0..metas.len()).map(|_| None).collect();
    let is_declaring = |name: &str| {
        let l = name.to_ascii_lowercase();
        l == "[content_types].xml" || has_ext(&l, "rels")
    };
    let mut content_types = ContentTypes::default();
    let mut rels: std::collections::HashMap<String, Vec<Rel>> = std::collections::HashMap::new();
    for (pos, m) in metas.iter().enumerate() {
        if !is_declaring(&m.name) {
            continue;
        }
        let buf = read_entry(&mut archive, m)?;
        let lname = m.name.to_ascii_lowercase();
        if lname == "[content_types].xml" {
            content_types = ContentTypes::parse(&buf);
        } else {
            let parsed = parse_rels(&lname, &buf);
            rels.insert(lname.clone(), parsed);
        }
        // Declaring parts are XML by name; their bytes are checked in 2b's
        // way below through the shared routine, after the package is known.
        slots[pos] = Some(Part {
            name: m.name.clone(),
            class: Class::Xml,
            data: buf,
        });
    }
    let package = Package::build(content_types, &rels);
    if matches!(mode, Mode::Render { .. }) && package.main.is_none() {
        return Err(OfficeError::Malformed(
            "no officeDocument relationship in _rels/.rels".into(),
        ));
    }
    for (pos, m) in metas.iter().enumerate() {
        let buf = match slots[pos].take() {
            Some(part) => part.data,
            None => read_entry(&mut archive, m)?,
        };
        let part = state.take(pos, m, buf, &package, mode, kind, l)?;
        slots[pos] = Some(part);
    }
    let Pass2 {
        images,
        cells,
        max_chart_index,
        inline_strings,
        rasters,
        ..
    } = state;
    let mut parts: Vec<Part> = slots.into_iter().flatten().collect();

    // Render mode: fit, do not refuse. One image at a time, so the peak is one
    // decode (under the per-image cap) plus the small results. Over the whole
    // package the decoded total never passes the budget: the allowance per
    // image shrinks with their number, and past what fits at the floor, the
    // later images (in document order) become placeholders.
    let mut raster_pixels = Vec::with_capacity(rasters.len());
    let allowance = image_allowance(images, l);
    // Images that fit at the floor, leaving room for a 1 px placeholder each.
    let fits_at_floor =
        l.render_decoded_pixels.saturating_sub(images) / l.render_image_min_pixels.max(1);
    for (rank, r) in rasters.iter().enumerate() {
        let px = r.dims.map(|(w, h)| w.saturating_mul(h));
        if matches!(mode, Mode::Render { .. }) {
            let over = rank as u64 >= fits_at_floor;
            let (replace, fitted) = match (r.dims, px) {
                _ if over => (true, None),
                (None, _) => (true, None),
                (Some((w, h)), Some(p)) if p > allowance => {
                    (true, Some(fit_image(&parts[r.pos].data, w, h, allowance)))
                }
                _ => (false, None),
            };
            if replace {
                parts[r.pos].data = fitted.unwrap_or_else(|| PLACEHOLDER_PNG.to_vec());
                raster_pixels.push(match probe(&parts[r.pos].data) {
                    Probe::Raster { width, height, .. } => width * height,
                    _ => 0,
                });
                continue;
            }
        }
        raster_pixels.push(px.unwrap_or(0));
    }

    // Write the canonical archive, in the original entry order.
    let mut w = ZipWriter::new(Cursor::new(Vec::new()));
    for part in parts {
        let method = if part.class.is_xml() {
            CompressionMethod::Deflated
        } else {
            CompressionMethod::Stored
        };
        let opts = SimpleFileOptions::default()
            .compression_method(method)
            .last_modified_time(zip::DateTime::default());
        w.start_file(part.name, opts).map_err(malformed)?;
        w.write_all(&part.data)
            .map_err(|e| OfficeError::Internal(format!("re-pack: {e}")))?;
    }
    let bytes = w.finish().map_err(malformed)?.into_inner();
    Ok(Repacked {
        bytes,
        images,
        max_chart_index,
        inline_strings,
        cells,
        raster_pixels,
    })
}

fn read_entry(archive: &mut ZipArchive<Cursor<&[u8]>>, m: &Meta) -> Result<Vec<u8>> {
    let mut f = archive.by_index(m.index).map_err(malformed)?;
    let mut buf = Vec::with_capacity(usize::try_from(m.declared.min(1 << 20)).unwrap_or(0));
    (&mut f)
        .take(m.declared.saturating_add(1))
        .read_to_end(&mut buf)
        .map_err(|e| malformed(format!("entry {:?}: {e}", m.name)))?;
    if buf.len() as u64 > m.declared {
        return Err(limit(Limit::ActualBytes, buf.len() as u64));
    }
    Ok(buf)
}

/// Running totals of pass 2.
struct Pass2 {
    xlsx: bool,
    xml_total: u64,
    media_total: u64,
    images: u64,
    cells: u64,
    max_chart_index: u64,
    inline_strings: bool,
    rasters: Vec<Raster>,
}

impl Pass2 {
    /// Classify one part, apply its class's caps and checks, and return it
    /// (changed for a render copy).
    #[allow(clippy::too_many_lines, clippy::too_many_arguments)]
    fn take(
        &mut self,
        pos: usize,
        m: &Meta,
        mut buf: Vec<u8>,
        package: &Package,
        mode: Mode,
        kind: Kind,
        l: &Limits,
    ) -> Result<Part> {
        let lname = m.name.to_ascii_lowercase();
        if let Some(num) = lname
            .strip_prefix("ppt/charts/chart")
            .and_then(|r| r.strip_suffix(".xml"))
            .and_then(|r| r.parse::<u64>().ok())
        {
            self.max_chart_index = self.max_chart_index.max(num);
        }
        let declaring = lname == "[content_types].xml" || has_ext(&lname, "rels");
        let class = if declaring {
            Class::Xml
        } else {
            package.classify(&lname, &buf)?
        };
        let is_sheet = self.xlsx && package.worksheets.contains(&lname);
        match class {
            Class::Xml | Class::XmlTolerant => {
                let part_cap = if self.xlsx && (is_sheet || package.shared_strings.contains(&lname))
                {
                    l.max_xml_large_part_bytes_xlsx
                } else {
                    l.max_xml_part_bytes
                };
                if buf.len() as u64 > part_cap {
                    return Err(limit(Limit::XmlPartBytes, buf.len() as u64));
                }
                self.xml_total = self.xml_total.saturating_add(buf.len() as u64);
                let total_cap = if self.xlsx {
                    l.max_xml_total_bytes_xlsx
                } else {
                    l.max_xml_total_bytes
                };
                if self.xml_total > total_cap {
                    return Err(limit(Limit::XmlTotalBytes, self.xml_total));
                }
                let info = if class == Class::Xml {
                    check_xml(&buf, l)
                } else {
                    scan_tolerant(&buf, l)
                }
                .map_err(|e| match e {
                    OfficeError::Malformed(m) => OfficeError::Malformed(format!("{lname}: {m}")),
                    other => other,
                })?;
                self.cells = self.cells.saturating_add(if self.xlsx {
                    if is_sheet { info.sheet_cells } else { 0 }
                } else {
                    info.table_cells
                });
                if self.cells > l.max_cells {
                    return Err(limit(Limit::Cells, self.cells));
                }
                if is_sheet && buf.windows(9).any(|w| w == b"inlineStr") {
                    self.inline_strings = true;
                }
                if let Mode::Render { dpi } = mode {
                    // The libraries open their main part from a fixed path, the
                    // relationships name it too: check both, so neither can carry
                    // the huge size while the other is read.
                    let is_main = package.main.as_deref() == Some(lname.as_str())
                        || (kind == Kind::Pptx && lname == "ppt/presentation.xml")
                        || (kind == Kind::Docx && lname == "word/document.xml");
                    if kind == Kind::Pptx
                        && is_main
                        && let Some((cx, cy)) = pptx_slide_size(&buf)
                    {
                        // The renderer draws a fixed 1280 px wide page.
                        let px = 1280u64.saturating_mul(1280u64.saturating_mul(cy) / cx.max(1));
                        if px > l.max_render_pixels {
                            return Err(limit(Limit::RenderPixels, px));
                        }
                    }
                    if kind == Kind::Docx
                        && dpi > 0.0
                        && is_main
                        && let Some((w, h)) = docx_page_size(&buf)
                    {
                        let px = page_pixels(w, h, dpi);
                        if px > l.max_render_pixels {
                            return Err(limit(Limit::RenderPixels, px));
                        }
                    }
                    if info.svg {
                        buf = EMPTY_SVG.to_vec();
                    }
                }
            }
            Class::Binary | Class::Font => {
                if buf.len() as u64 > l.max_media_part_bytes {
                    return Err(limit(Limit::MediaPartBytes, buf.len() as u64));
                }
                self.media_total = self.media_total.saturating_add(buf.len() as u64);
                if self.media_total > l.max_media_total_bytes {
                    return Err(limit(Limit::MediaTotalBytes, self.media_total));
                }
                if class == Class::Binary {
                    // A raster counts as an image whether or not its size can
                    // be read; an unreadable one never reaches a renderer.
                    let dims = match probe(&buf) {
                        Probe::NotRaster => None,
                        Probe::Unreadable => Some(None),
                        Probe::Raster { width, height, .. } => {
                            let px = width.saturating_mul(height);
                            if px > l.max_image_pixels {
                                return Err(limit(Limit::ImagePixels, px));
                            }
                            Some(Some((width, height)))
                        }
                    };
                    if let Some(dims) = dims {
                        self.images += 1;
                        if self.images > l.max_images {
                            return Err(limit(Limit::Images, self.images));
                        }
                        self.rasters.push(Raster { pos, dims });
                    }
                } else if matches!(mode, Mode::Render { .. }) {
                    buf.clear();
                }
            }
        }
        Ok(Part {
            name: m.name.clone(),
            class,
            data: buf,
        })
    }
}

/// Check a package a library produced and canonicalise it (the output side of
/// the same gate); the result may not exceed the input cap either.
pub(crate) fn finish_output(bytes: &[u8], kind: Kind, l: &Limits) -> Result<Vec<u8>> {
    let out = validate_and_repack(bytes, kind, Mode::Keep, l)?.bytes;
    precheck_len(out.len(), l)?;
    Ok(out)
}

/// What an image inserted by a caller is: the extension that matches its
/// signature (never the caller's), after the signature and pixel checks.
pub(crate) fn check_new_image(
    data: &[u8],
    current_images: u64,
    l: &Limits,
) -> Result<&'static str> {
    if data.len() as u64 > l.max_media_part_bytes {
        return Err(limit(Limit::MediaPartBytes, data.len() as u64));
    }
    let ext = match probe(data) {
        Probe::Raster {
            width,
            height,
            format,
        } => {
            let px = width.saturating_mul(height);
            if px > l.max_image_pixels {
                return Err(limit(Limit::ImagePixels, px));
            }
            if format == Format::Bmp {
                return Err(OfficeError::InvalidInput(
                    "unsupported image format (expected PNG, JPEG or GIF)".into(),
                ));
            }
            format.extension()
        }
        Probe::Unreadable => {
            return Err(OfficeError::InvalidInput("unreadable image header".into()));
        }
        Probe::NotRaster => {
            return Err(OfficeError::InvalidInput(
                "unsupported image format (expected PNG, JPEG or GIF)".into(),
            ));
        }
    };
    if current_images + 1 > l.max_images {
        return Err(limit(Limit::Images, current_images + 1));
    }
    Ok(ext)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pkg(ct: &str) -> Package {
        Package {
            content_types: ContentTypes::parse(ct.as_bytes()),
            ..Package::default()
        }
    }

    #[test]
    fn declared_type_beats_bytes_and_unknown_bytes_are_xml_checked() {
        let p = pkg(
            r#"<Types><Default Extension="xml" ContentType="application/xml"/><Default Extension="vml" ContentType="application/vnd.openxmlformats-officedocument.vmlDrawing"/><Default Extension="gif" ContentType="image/gif"/><Default Extension="fntdata" ContentType="application/x-fontdata"/><Default Extension="odttf" ContentType="application/vnd.openxmlformats-officedocument.obfuscatedFont"/><Override PartName="/ppt/odd.dat" ContentType="application/vnd.foo+xml"/></Types>"#,
        );
        // Named or declared XML: strict, whatever the first bytes.
        assert_eq!(p.classify("a/b.xml", b"GIF89a<a/>").unwrap(), Class::Xml);
        assert_eq!(p.classify("a/b.rels", b"\x89PNG").unwrap(), Class::Xml);
        assert_eq!(p.classify("ppt/odd.dat", b"\x89PNG").unwrap(), Class::Xml);
        // Not declared XML: a known binary is binary...
        assert_eq!(p.classify("a/b.gif", b"GIF89a....").unwrap(), Class::Binary);
        // ...and everything else is scanned tolerantly, even leading junk.
        assert_eq!(
            p.classify("a/x.vml", b"x<p:sld>").unwrap(),
            Class::XmlTolerant
        );
        assert_eq!(
            p.classify("a/x.bin", b"\0\x01junk").unwrap(),
            Class::XmlTolerant
        );
        assert_eq!(p.classify("a/empty.bin", b"").unwrap(), Class::XmlTolerant);
        // Fonts: by content type, and by relationship.
        assert_eq!(
            p.classify("ppt/fonts/f.fntdata", b"\0\x01\0\0").unwrap(),
            Class::Font
        );
        assert_eq!(
            p.classify("word/fonts/f.odttf", b"<not really xml>")
                .unwrap(),
            Class::Font
        );
        let mut p2 = pkg("<Types/>");
        p2.fonts.insert("ppt/weird/f1.dat".into());
        assert_eq!(
            p2.classify("ppt/weird/f1.dat", b"\0\0\0").unwrap(),
            Class::Font
        );
        assert!(matches!(
            p.classify("a/b.xml", &[0xFF, 0xFE, b'<', 0]),
            Err(OfficeError::Unsupported(_))
        ));
    }

    #[test]
    fn the_tolerant_scanner_needs_no_well_formed_bytes() {
        let l = Limits::default();
        let mut deep = b"x<p:sld>".to_vec();
        deep.extend(std::iter::repeat_n(&b"<a:b>"[..], 300).flatten());
        assert!(matches!(
            scan_tolerant(&deep, &l),
            Err(OfficeError::LimitExceeded {
                limit: Limit::XmlDepth,
                ..
            })
        ));
        assert!(
            scan_tolerant(b"junk<!DOCTYPE plist PUBLIC \"x\" \"y\">", &l).is_ok(),
            "a plist DOCTYPE in printer settings is fine"
        );
        assert!(matches!(
            scan_tolerant(b"junk<!DOCTYPE a [<!ENTITY e \"x\">]>", &l),
            Err(OfficeError::Malformed(_))
        ));
        assert!(matches!(
            scan_tolerant(b"\0\0<w:gridSpan w:val=\"5000\"/>", &l),
            Err(OfficeError::LimitExceeded {
                limit: Limit::TableSpan,
                value: 5000
            })
        ));
        let i = scan_tolerant(b"<svg><tc/><tc/><c/></svg>", &l).unwrap();
        assert!(i.svg && i.table_cells == 2 && i.sheet_cells == 1);
        // Binary noise and a lone '<' do not read as nesting.
        let noise: Vec<u8> = (0..200_000u32)
            .map(|i| (i.wrapping_mul(2_654_435_761) >> 24) as u8)
            .collect();
        assert!(scan_tolerant(&noise, &l).is_ok());
        assert!(
            scan_tolerant(&b"<a".repeat(100_000), &l).is_ok(),
            "no '>' anywhere: no tags, and no quadratic scan"
        );
    }

    #[test]
    fn xml_pass_counts_and_finds_the_root() {
        let l = Limits::default();
        let i = check_xml(b"<svg><a><tc/><tc/><pt/></a></svg>", &l).unwrap();
        assert!(i.svg && i.table_cells == 3);
        let i = check_xml(b"<worksheet><c/><c/><x/></worksheet>", &l).unwrap();
        assert!(!i.svg && i.sheet_cells == 2);
        assert!(matches!(
            check_xml(b"<a><gridSpan val=\"5000\"/></a>", &l),
            Err(OfficeError::LimitExceeded {
                limit: Limit::TableSpan,
                value: 5000
            })
        ));
    }

    #[test]
    fn relationships_resolve_relative_to_their_source() {
        let rels = parse_rels(
            "ppt/_rels/presentation.xml.rels",
            br#"<Relationships><Relationship Id="1" Type="x/font" Target="fonts/f.fntdata"/><Relationship Id="2" Type="x/slide" Target="../ppt/./slides/s.xml"/><Relationship Id="3" Type="x/font" Target="http://e/x" TargetMode="External"/><Relationship Id="4" Type="x/font" Target="/Abs/F.TTF"/></Relationships>"#,
        );
        let t: Vec<&str> = rels.iter().map(|r| r.target.as_str()).collect();
        assert_eq!(t, ["ppt/fonts/f.fntdata", "ppt/slides/s.xml", "abs/f.ttf"]);
    }

    #[test]
    fn extension_test_sees_the_dot_name() {
        assert!(has_ext("_rels/.rels", "rels"));
        assert!(has_ext("ppt/_rels/presentation.xml.RELS", "rels"));
        assert!(has_ext("a.XML", "xml"));
        assert!(!has_ext("xml", "xml"));
        assert!(!has_ext("a.xmlx", "xml"));
        assert!(!has_ext("é", "xml"));
    }

    #[test]
    fn part_numbers_are_bounded() {
        assert!(check_part_number("ppt/slides/slide12.xml").is_ok());
        assert!(check_part_number("ppt/slides/slide1000000.xml").is_ok());
        assert!(check_part_number("ppt/slides/slide0000000012.xml").is_ok());
        assert!(check_part_number("ppt/media/image.png").is_ok());
        for n in [
            "ppt/slides/slide1000001.xml",
            "ppt/charts/chart18446744073709551615.xml",
            "x/y99999999999999999999999.bin",
        ] {
            assert!(
                matches!(check_part_number(n), Err(OfficeError::Malformed(_))),
                "{n}"
            );
        }
    }

    #[test]
    fn the_image_allowance_follows_the_adr() {
        let l = Limits::default();
        assert_eq!(image_allowance(1, &l), 2_000_000);
        assert_eq!(image_allowance(16, &l), 2_000_000);
        assert_eq!(image_allowance(20, &l), 1_600_000);
        assert_eq!(image_allowance(500, &l), 250_000);
    }
}
