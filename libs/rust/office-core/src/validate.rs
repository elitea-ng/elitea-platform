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
//! Refused here, before any library runs: input and entry-count limits (the
//! count is read from the end-of-central-directory record, zip64 included,
//! before the central directory is parsed), duplicate entry names (compared
//! case-insensitively, as OPC part names are), unsafe names, a local header
//! that disagrees with the central directory, encrypted entries, methods other
//! than stored and deflate, trailing data, prefix data and overlapping entries,
//! per-part and total XML and media sizes, bytes beyond what a header
//! declared, XML nesting, DTDs, and raster pixel counts read from image headers.

use std::collections::HashSet;
use std::io::{Cursor, Read, Write};

use quick_xml::Reader;
use quick_xml::events::Event;
use zip::CompressionMethod;
use zip::ZipArchive;
use zip::ZipWriter;
use zip::write::SimpleFileOptions;

use crate::error::{Limit, OfficeError, Result, limit, malformed};
use crate::image::{Probe, probe};
use crate::limits::Limits;

/// Which package kind is being validated; picks the XML total cap.
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
    /// Hand to a renderer or exporter: font parts are emptied, SVG parts are
    /// replaced by an empty SVG, and the render pixel budget applies. `dpi` is
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
}

const EMPTY_SVG: &[u8] = b"<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"1\" height=\"1\"/>";

#[derive(Clone, Copy, PartialEq, Eq)]
enum Class {
    Xml,
    Opaque,
}

/// Case-insensitive file-extension test.
fn has_ext(name: &str, ext: &str) -> bool {
    std::path::Path::new(name)
        .extension()
        .is_some_and(|e| e.eq_ignore_ascii_case(ext))
}

fn classify(name: &str) -> Class {
    if has_ext(name, "xml") || has_ext(name, "rels") {
        Class::Xml
    } else {
        Class::Opaque
    }
}

fn is_font_part(name: &str) -> bool {
    let l = name.to_ascii_lowercase();
    l.starts_with("ppt/fonts/")
        || l.starts_with("word/fonts/")
        || ["odttf", "fntdata", "ttf", "otf"]
            .iter()
            .any(|e| has_ext(name, e))
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

struct Meta {
    index: usize,
    name: String,
    declared: u64,
    class: Class,
}

/// Count depth with quick-xml; refuse DTDs and unbalanced documents.
pub(crate) fn check_xml(bytes: &[u8], l: &Limits) -> Result<()> {
    let max_depth = l.max_xml_depth;
    let mut reader = Reader::from_reader(bytes);
    let mut depth = 0u64;
    loop {
        match reader.read_event() {
            Ok(Event::Start(e)) => {
                depth += 1;
                if depth > max_depth {
                    return Err(limit(Limit::XmlDepth, depth));
                }
                check_span(&e, l)?;
            }
            Ok(Event::Empty(e)) => check_span(&e, l)?,
            Ok(Event::End(_)) => depth = depth.saturating_sub(1),
            Ok(Event::DocType(_)) => {
                return Err(OfficeError::Malformed(
                    "a DTD is not allowed in an Office part".into(),
                ));
            }
            Ok(Event::Eof) => {
                return if depth == 0 {
                    Ok(())
                } else {
                    Err(OfficeError::Malformed("XML ends inside an element".into()))
                };
            }
            Ok(_) => {}
            Err(e) => return Err(malformed(format!("XML: {e}"))),
        }
    }
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

/// Validate `input` as a package of `kind` and return a canonical re-packed
/// archive. See the module documentation for what is refused.
#[allow(clippy::too_many_lines)]
pub(crate) fn validate_and_repack(
    input: &[u8],
    kind: Kind,
    mode: Mode,
    l: &Limits,
) -> Result<Repacked> {
    let len = input.len() as u64;
    if len > l.max_input_bytes {
        return Err(limit(Limit::InputBytes, len));
    }
    check_eocd(input, l)?;
    let mut archive = ZipArchive::new(Cursor::new(input)).map_err(malformed)?;
    let n = archive.len();
    if n as u64 > l.max_entries {
        return Err(limit(Limit::Entries, n as u64));
    }

    // Pass 1: metadata only. Nothing is decompressed.
    let xml_total_cap = if kind == Kind::Xlsx {
        l.max_xml_total_bytes_xlsx
    } else {
        l.max_xml_total_bytes
    };
    let mut seen = HashSet::new();
    let mut metas = Vec::with_capacity(n);
    let mut ranges: Vec<(u64, u64)> = Vec::with_capacity(n);
    let (mut xml_total, mut media_total) = (0u64, 0u64);
    for index in 0..n {
        let f = archive.by_index_raw(index).map_err(malformed)?;
        let name = f.name().to_owned();
        if !safe_name(&name) {
            return Err(OfficeError::Malformed(format!(
                "unsafe entry name {name:?}"
            )));
        }
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
        let class = classify(&name);
        let declared = f.size();
        let (part_cap, part_limit) = match class {
            Class::Xml => (l.max_xml_part_bytes, Limit::XmlPartBytes),
            Class::Opaque => (l.max_media_part_bytes, Limit::MediaPartBytes),
        };
        if declared > part_cap {
            return Err(limit(part_limit, declared));
        }
        match class {
            Class::Xml => {
                xml_total = xml_total.saturating_add(declared);
                if xml_total > xml_total_cap {
                    return Err(limit(Limit::XmlTotalBytes, xml_total));
                }
            }
            Class::Opaque => {
                media_total = media_total.saturating_add(declared);
                if media_total > l.max_media_total_bytes {
                    return Err(limit(Limit::MediaTotalBytes, media_total));
                }
            }
        }
        metas.push(Meta {
            index,
            name,
            declared,
            class,
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

    // Pass 2: read each entry once, capped at what its header declared.
    let mut out: Vec<(String, Class, Vec<u8>)> = Vec::with_capacity(metas.len());
    let (mut images, mut raster_pixels) = (0u64, 0u64);
    let mut render_extra = 0u64;
    let mut max_chart_index = 0u64;
    let mut inline_strings = false;
    for m in metas {
        let mut f = archive.by_index(m.index).map_err(malformed)?;
        let mut buf = Vec::with_capacity(usize::try_from(m.declared.min(1 << 20)).unwrap_or(0));
        (&mut f)
            .take(m.declared.saturating_add(1))
            .read_to_end(&mut buf)
            .map_err(|e| malformed(format!("entry {:?}: {e}", m.name)))?;
        if buf.len() as u64 > m.declared {
            return Err(limit(Limit::ActualBytes, buf.len() as u64));
        }
        drop(f);
        let lname = m.name.to_ascii_lowercase();
        if let Some(n) = lname
            .strip_prefix("ppt/charts/chart")
            .and_then(|r| r.strip_suffix(".xml"))
            .and_then(|r| r.parse::<u64>().ok())
        {
            max_chart_index = max_chart_index.max(n);
        }
        match m.class {
            Class::Xml => {
                check_xml(&buf, l)?;
                if kind == Kind::Xlsx
                    && lname.starts_with("xl/worksheets/")
                    && buf.windows(9).any(|w| w == b"inlineStr")
                {
                    inline_strings = true;
                }
                if let Mode::Render { dpi } = mode {
                    if kind == Kind::Pptx
                        && lname == "ppt/presentation.xml"
                        && let Some((cx, cy)) = pptx_slide_size(&buf)
                    {
                        // The renderer draws a fixed 1280 px wide page.
                        let h = 1280u64.saturating_mul(cy) / cx.max(1);
                        render_extra = render_extra.max(h.saturating_mul(1280));
                    }
                    if kind == Kind::Docx
                        && dpi > 0.0
                        && lname.ends_with("document.xml")
                        && let Some((w, h)) = docx_page_size(&buf)
                    {
                        let px = page_pixels(w, h, dpi);
                        if px > l.max_render_pixels {
                            return Err(limit(Limit::RenderPixels, px));
                        }
                    }
                }
            }
            Class::Opaque => match probe(&buf) {
                Probe::NotRaster => {}
                Probe::Unreadable => {
                    return Err(OfficeError::Malformed(format!(
                        "image {:?} has an unreadable header",
                        m.name
                    )));
                }
                Probe::Raster { width, height } => {
                    let px = width.saturating_mul(height);
                    if px > l.max_image_pixels {
                        return Err(limit(Limit::ImagePixels, px));
                    }
                    images += 1;
                    if images > l.max_images {
                        return Err(limit(Limit::Images, images));
                    }
                    raster_pixels = raster_pixels.saturating_add(px);
                }
            },
        }
        if let Mode::Render { .. } = mode
            && m.class == Class::Opaque
        {
            if is_font_part(&m.name) {
                buf.clear();
            } else if m.name.to_ascii_lowercase().ends_with(".svg") {
                buf = EMPTY_SVG.to_vec();
            }
        }
        out.push((m.name, m.class, buf));
    }
    if let Mode::Render { .. } = mode {
        let total = raster_pixels.saturating_add(render_extra);
        if total > l.max_render_pixels {
            return Err(limit(Limit::RenderPixels, total));
        }
    }

    // Write the canonical archive, in the original entry order.
    let mut w = ZipWriter::new(Cursor::new(Vec::new()));
    for (name, class, data) in out {
        let method = if class == Class::Xml {
            CompressionMethod::Deflated
        } else {
            CompressionMethod::Stored
        };
        let opts = SimpleFileOptions::default()
            .compression_method(method)
            .last_modified_time(zip::DateTime::default());
        w.start_file(name, opts).map_err(malformed)?;
        w.write_all(&data)
            .map_err(|e| OfficeError::Internal(format!("re-pack: {e}")))?;
    }
    let bytes = w.finish().map_err(malformed)?.into_inner();
    Ok(Repacked {
        bytes,
        images,
        max_chart_index,
        inline_strings,
    })
}

/// Check a bytes blob a library produced and canonicalise it (the output side
/// of the same gate), refusing anything larger than the input cap.
pub(crate) fn finish_output(bytes: &[u8], kind: Kind, l: &Limits) -> Result<Vec<u8>> {
    validate_and_repack(bytes, kind, Mode::Keep, l).map(|r| r.bytes)
}

/// Probe an image a caller wants to insert. Only formats the libraries embed.
pub(crate) fn check_new_image(data: &[u8], current_images: u64, l: &Limits) -> Result<()> {
    if data.len() as u64 > l.max_media_part_bytes {
        return Err(limit(Limit::MediaPartBytes, data.len() as u64));
    }
    match probe(data) {
        Probe::Raster { width, height } => {
            let px = width.saturating_mul(height);
            if px > l.max_image_pixels {
                return Err(limit(Limit::ImagePixels, px));
            }
        }
        Probe::Unreadable => {
            return Err(OfficeError::InvalidInput("unreadable image header".into()));
        }
        Probe::NotRaster => {
            return Err(OfficeError::InvalidInput(
                "unsupported image format (expected PNG, JPEG or GIF)".into(),
            ));
        }
    }
    if current_images + 1 > l.max_images {
        return Err(limit(Limit::Images, current_images + 1));
    }
    Ok(())
}
