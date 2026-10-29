//! A read-only fact sheet of a saved deck: what the pptx toolkit needs and
//! the library's own accessors do not give (ADR-0032 P3).
//!
//! `Deck::shapes` lists the `p:sp` text shapes only. A table or a chart is a
//! `p:graphicFrame`, and the geometry, font sizes and colours that a layout
//! check needs are not exposed at all. This module reads them from the
//! *saved* package: the bytes `Deck::save` returns have already passed the
//! validation gate (entry, XML-part and depth caps), so this is a streaming
//! read of a canonical archive, not a second parser of caller bytes. It
//! reads, never writes, and every part is read through a capped reader.
//!
//! Two index spaces matter and both are reported:
//!
//! * `ShapeFacts::index` counts every drawable of the slide tree (`p:sp`,
//!   `p:pic`, `p:graphicFrame`, `p:cxnSp`, `p:grpSp`) in document order. It
//!   is the space [`crate::Deck::set_shape_table_cell`] addresses a table in.
//! * `ShapeFacts::text_index` counts the `p:sp` shapes only. It is the space
//!   of [`crate::Deck::shapes`], [`crate::Deck::run_count`] and
//!   [`crate::Deck::set_run_text`].

use std::collections::HashMap;
use std::io::{Cursor, Read};

use quick_xml::Reader;
use quick_xml::events::{BytesStart, Event};
use zip::ZipArchive;

use crate::deck::Rect;
use crate::error::{OfficeError, Result};
use crate::limits::Limits;

/// What a drawable of the slide tree is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[non_exhaustive]
pub enum ShapeKind {
    /// A shape or text box (`p:sp`).
    Text,
    /// A picture (`p:pic`).
    Picture,
    /// A table (`p:graphicFrame` holding `a:tbl`).
    Table,
    /// A chart (`p:graphicFrame` holding a chart reference).
    Chart,
    /// Another graphic frame (`SmartArt`, an embedded object).
    Graphic,
    /// A connector or a group.
    Other,
}

/// One drawable of a slide.
#[derive(Debug, Clone, PartialEq)]
pub struct ShapeFacts {
    /// Index among all drawables (the table-editing space).
    pub index: usize,
    /// Index among the `p:sp` shapes (the text-editing space), for a
    /// [`ShapeKind::Text`] shape.
    pub text_index: Option<usize>,
    /// The shape name.
    pub name: String,
    /// What it is.
    pub kind: ShapeKind,
    /// The placeholder type (`title`, `body`, ...) when it is one.
    pub placeholder: Option<String>,
    /// Its own frame in EMU; `None` when it inherits one from the layout.
    pub frame: Option<Rect>,
    /// Its text, paragraphs joined by `\n`.
    pub text: String,
    /// Each distinct explicit run size, in points.
    pub font_sizes_pt: Vec<f32>,
    /// The first explicit run colour, `RRGGBB`.
    pub text_color: Option<String>,
    /// The shape's explicit solid fill, `RRGGBB`.
    pub fill_color: Option<String>,
}

/// One table: its drawable index and the text of every cell.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TableFacts {
    /// The drawable index (the table-editing space).
    pub shape: usize,
    /// Row by row, the cell text (paragraphs joined by `\n`).
    pub cells: Vec<Vec<String>>,
}

/// One chart, read back from its chart part.
#[derive(Debug, Clone, PartialEq)]
pub struct ChartFacts {
    /// The drawable index.
    pub shape: usize,
    /// `bar`, `column`, `line`, `pie`, `doughnut`, `area`, `scatter` or the
    /// chart part's own element name.
    pub kind: String,
    /// The title, when it has one.
    pub title: Option<String>,
    /// The category labels.
    pub categories: Vec<String>,
    /// Each series: its name and values (`None` for a missing point).
    pub series: Vec<(String, Vec<Option<f64>>)>,
}

/// Everything read from one slide.
#[derive(Debug, Clone, PartialEq)]
pub struct SlideFacts {
    /// The slide's position in the deck, from 0.
    pub slide: usize,
    /// The slide size in EMU.
    pub size: (i64, i64),
    /// Every drawable, in document order.
    pub shapes: Vec<ShapeFacts>,
    /// The tables.
    pub tables: Vec<TableFacts>,
    /// The charts.
    pub charts: Vec<ChartFacts>,
    /// The speaker notes' text (empty when the slide has none).
    pub notes: String,
}

/// The fact sheet of a pptx package given as bytes, without opening it in the
/// slide library: the package goes through the same validation gate as
/// [`crate::Deck::open`] and is then read directly. For a caller that only
/// needs to know what a file holds (before an edit, or after a save) this
/// is the cheap path; [`crate::Deck::facts`] must serialise the deck first.
///
/// # Errors
/// Whatever the gate refuses, [`OfficeError::Malformed`], [`OfficeError::Busy`].
pub fn facts_of_package(bytes: &[u8], limits: &Limits) -> Result<Vec<SlideFacts>> {
    use crate::validate::{Kind, Mode, precheck_len, validate_and_repack};
    crate::counters::bump(crate::counters::Op::PackageRead);
    precheck_len(bytes.len(), limits)?;
    let (data, l) = (bytes.to_vec(), limits.clone());
    crate::guard::run_guarded(limits, move || {
        let repacked = validate_and_repack(&data, Kind::Pptx, Mode::Keep, &l)?;
        read_facts(&repacked.bytes, &l)
    })
}

/// Read the fact sheet of every slide from a validated package.
pub(crate) fn read_facts(package: &[u8], limits: &Limits) -> Result<Vec<SlideFacts>> {
    let mut zip = ZipArchive::new(Cursor::new(package))
        .map_err(|e| OfficeError::Malformed(format!("package: {e}")))?;
    let cap = limits.max_xml_part_bytes;
    let mut part = |name: &str| -> Result<Option<Vec<u8>>> {
        let Ok(entry) = zip.by_name(name) else {
            return Ok(None);
        };
        let mut out = Vec::new();
        entry
            .take(cap + 1)
            .read_to_end(&mut out)
            .map_err(|e| OfficeError::Malformed(format!("part {name}: {e}")))?;
        if out.len() as u64 > cap {
            return Err(OfficeError::Malformed(format!("part {name} is too large")));
        }
        Ok(Some(out))
    };
    let presentation = part("ppt/presentation.xml")?
        .ok_or_else(|| OfficeError::Malformed("no ppt/presentation.xml".into()))?;
    let (size, slide_ids) = read_presentation(&presentation);
    let rels = part("ppt/_rels/presentation.xml.rels")?
        .map(|b| read_rels(&b))
        .unwrap_or_default();
    let mut out = Vec::new();
    for (slide, rid) in slide_ids.iter().enumerate() {
        let Some(target) = rels.get(rid) else {
            return Err(OfficeError::Malformed(format!(
                "slide {} has no relationship",
                slide + 1
            )));
        };
        let path = resolve("ppt", target);
        let xml = part(&path)?
            .ok_or_else(|| OfficeError::Malformed(format!("slide part {path} is missing")))?;
        let slide_rels_xml = part(&rels_path(&path))?;
        let slide_rels = slide_rels_xml.as_deref().map(read_rels).unwrap_or_default();
        let notes = match slide_rels_xml
            .as_deref()
            .and_then(|x| rel_target_of_type(x, "/notesSlide"))
        {
            Some(target) => part(&resolve(parent(&path), &target))?
                .map(|b| part_text(&b))
                .unwrap_or_default(),
            None => String::new(),
        };
        let mut facts = read_slide(&xml, slide, size);
        for chart in &mut facts.1 {
            let Some(target) = slide_rels.get(&chart.rid) else {
                continue;
            };
            let chart_path = resolve(parent(&path), target);
            if let Some(bytes) = part(&chart_path)? {
                let c = read_chart(&bytes);
                chart.facts.kind = c.kind;
                chart.facts.title = c.title;
                chart.facts.categories = c.categories;
                chart.facts.series = c.series;
            }
        }
        let mut s = facts.0;
        s.notes = notes;
        s.charts = facts.1.into_iter().map(|c| c.facts).collect();
        out.push(s);
    }
    Ok(out)
}

struct PendingChart {
    rid: String,
    facts: ChartFacts,
}

pub(crate) fn parent(path: &str) -> &str {
    path.rsplit_once('/').map_or("", |(dir, _)| dir)
}

pub(crate) fn rels_path(part: &str) -> String {
    match part.rsplit_once('/') {
        Some((dir, file)) => format!("{dir}/_rels/{file}.rels"),
        None => format!("_rels/{part}.rels"),
    }
}

/// `target` relative to `base_dir`, with `..` folded; an absolute target
/// (leading `/`) is taken from the package root.
pub(crate) fn resolve(base_dir: &str, target: &str) -> String {
    let mut parts: Vec<&str> = if target.starts_with('/') {
        Vec::new()
    } else {
        base_dir.split('/').filter(|s| !s.is_empty()).collect()
    };
    for seg in target.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            s => parts.push(s),
        }
    }
    parts.join("/")
}

pub(crate) fn attr(e: &BytesStart<'_>, local: &str) -> Option<String> {
    e.attributes()
        .flatten()
        .find(|a| a.key.local_name().as_ref() == local)
        .map(|a| a.value.to_string())
}

/// The value of the namespaced `r:id`-style attribute (not the numeric `id`).
pub(crate) fn rel_attr(e: &BytesStart<'_>, local: &str) -> Option<String> {
    e.attributes()
        .flatten()
        .find(|a| {
            let k = a.key.as_ref();
            a.key.local_name().as_ref() == local && k.len() > local.len()
        })
        .map(|a| a.value.to_string())
}

/// The target of a relationship whose type ends with `suffix`.
fn rel_target_of_type(xml: &[u8], suffix: &str) -> Option<String> {
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == "Relationship" => {
                if attr(&e, "Type").is_some_and(|t| t.ends_with(suffix)) {
                    return attr(&e, "Target");
                }
            }
            Ok(Event::Eof) | Err(_) => return None,
            Ok(_) => {}
        }
    }
}

/// All `a:t` text of a part, joined by `\n` per paragraph.
fn part_text(xml: &[u8]) -> String {
    let mut r = Reader::from_reader(xml);
    let (mut in_t, mut out) = (false, String::new());
    loop {
        match r.read_event() {
            Ok(Event::Start(e)) if e.local_name().as_ref() == "t" => in_t = true,
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                "t" => in_t = false,
                "p" if !out.is_empty() && !out.ends_with('\n') => out.push('\n'),
                _ => {}
            },
            Ok(Event::Text(t)) if in_t => {
                let s: &str = &t;
                out.push_str(s);
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    out.trim().to_owned()
}

pub(crate) fn read_rels(xml: &[u8]) -> HashMap<String, String> {
    let mut out = HashMap::new();
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == "Relationship" => {
                if let (Some(id), Some(target)) = (attr(&e, "Id"), attr(&e, "Target")) {
                    out.insert(id, target);
                }
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    out
}

fn read_presentation(xml: &[u8]) -> ((i64, i64), Vec<String>) {
    let mut size = (9_144_000, 6_858_000);
    let mut ids = Vec::new();
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) => match e.local_name().as_ref() {
                "sldSz" => {
                    let cx = attr(&e, "cx").and_then(|v| v.parse().ok());
                    let cy = attr(&e, "cy").and_then(|v| v.parse().ok());
                    if let (Some(cx), Some(cy)) = (cx, cy) {
                        size = (cx, cy);
                    }
                }
                "sldId" => {
                    if let Some(id) = rel_attr(&e, "id") {
                        ids.push(id);
                    }
                }
                _ => {}
            },
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    (size, ids)
}

fn unescape_general_ref(g: &quick_xml::events::BytesRef<'_>, into: &mut String) {
    if let Ok(Some(c)) = g.resolve_char_ref() {
        into.push(c);
    } else {
        let n: &str = g;
        match n {
            "amp" => into.push('&'),
            "lt" => into.push('<'),
            "gt" => into.push('>'),
            "quot" => into.push('"'),
            "apos" => into.push('\''),
            _ => {}
        }
    }
}

#[derive(Default)]
struct ShapeAcc {
    facts: Option<ShapeFacts>,
    paragraphs: Vec<String>,
    para: Option<String>,
    has_table: bool,
    has_chart: Option<String>,
    cells: Vec<Vec<String>>,
    row: Vec<String>,
    cell_paras: Vec<String>,
    in_cell: bool,
    depth_at_start: usize,
}

/// One slide's `SlideFacts` and its chart references (the charts' data is
/// filled in by the caller from the chart parts).
#[allow(clippy::too_many_lines)]
fn read_slide(xml: &[u8], slide: usize, size: (i64, i64)) -> (SlideFacts, Vec<PendingChart>) {
    let mut reader = Reader::from_reader(xml);
    let mut stack: Vec<Vec<u8>> = Vec::new();
    let mut out = SlideFacts {
        slide,
        size,
        shapes: Vec::new(),
        tables: Vec::new(),
        charts: Vec::new(),
        notes: String::new(),
    };
    let mut charts: Vec<PendingChart> = Vec::new();
    let mut acc = ShapeAcc::default();
    let mut text_index = 0usize;
    let mut in_t = false;
    let mut in_text_color = false; // inside a:rPr
    let mut in_sp_pr = false;
    let mut in_xfrm = false;
    let mut off: Option<(i64, i64)> = None;
    let mut ext: Option<(i64, i64)> = None;
    loop {
        let ev = reader.read_event();
        let is_start = matches!(ev, Ok(Event::Start(_)));
        match ev {
            Ok(Event::Start(e) | Event::Empty(e)) => {
                let name = e.local_name().as_ref().as_bytes().to_vec();
                let top_level = stack.last().is_some_and(|p| p == b"spTree");
                match name.as_slice() {
                    b"sp" | b"pic" | b"graphicFrame" | b"cxnSp" | b"grpSp" if top_level => {
                        let kind = match name.as_slice() {
                            b"sp" => ShapeKind::Text,
                            b"pic" => ShapeKind::Picture,
                            b"graphicFrame" => ShapeKind::Graphic,
                            _ => ShapeKind::Other,
                        };
                        let ti = (kind == ShapeKind::Text).then(|| {
                            let i = text_index;
                            text_index += 1;
                            i
                        });
                        acc = ShapeAcc {
                            facts: Some(ShapeFacts {
                                index: out.shapes.len(),
                                text_index: ti,
                                name: String::new(),
                                kind,
                                placeholder: None,
                                frame: None,
                                text: String::new(),
                                font_sizes_pt: Vec::new(),
                                text_color: None,
                                fill_color: None,
                            }),
                            depth_at_start: stack.len(),
                            ..ShapeAcc::default()
                        };
                        off = None;
                        ext = None;
                    }
                    b"cNvPr" if acc.facts.is_some() => {
                        if let Some(f) = acc.facts.as_mut()
                            && f.name.is_empty()
                        {
                            f.name = attr(&e, "name").unwrap_or_default();
                        }
                    }
                    b"ph" if acc.facts.is_some() => {
                        if let Some(f) = acc.facts.as_mut() {
                            f.placeholder = Some(attr(&e, "type").unwrap_or_else(|| "body".into()));
                        }
                    }
                    b"spPr" if acc.facts.is_some() => in_sp_pr = true,
                    b"xfrm" if acc.facts.is_some() => in_xfrm = true,
                    b"off" if in_xfrm => {
                        let x = attr(&e, "x").and_then(|v| v.parse().ok());
                        let y = attr(&e, "y").and_then(|v| v.parse().ok());
                        if let (Some(x), Some(y)) = (x, y) {
                            off = Some((x, y));
                        }
                    }
                    b"ext" if in_xfrm => {
                        let w = attr(&e, "cx").and_then(|v| v.parse().ok());
                        let h = attr(&e, "cy").and_then(|v| v.parse().ok());
                        if let (Some(w), Some(h)) = (w, h) {
                            ext = Some((w, h));
                        }
                    }
                    b"tbl" if acc.facts.is_some() => acc.has_table = true,
                    b"chart" if acc.facts.is_some() => {
                        if let Some(rid) = rel_attr(&e, "id") {
                            acc.has_chart = Some(rid);
                        }
                    }
                    b"tr" if acc.has_table => acc.row = Vec::new(),
                    b"tc" if acc.has_table => {
                        acc.in_cell = true;
                        acc.cell_paras = Vec::new();
                    }
                    b"p" if acc.facts.is_some() => acc.para = Some(String::new()),
                    b"rPr" if acc.facts.is_some() => {
                        in_text_color = true;
                        if let (Some(sz), Some(f)) = (
                            attr(&e, "sz").and_then(|v| v.parse::<f32>().ok()),
                            acc.facts.as_mut(),
                        ) {
                            let pt = sz / 100.0;
                            if !f
                                .font_sizes_pt
                                .iter()
                                .any(|s| (s - pt).abs() < f32::EPSILON)
                            {
                                f.font_sizes_pt.push(pt);
                            }
                        }
                    }
                    b"srgbClr" if acc.facts.is_some() => {
                        let in_solid = stack.last().is_some_and(|p| p == b"solidFill");
                        if in_solid
                            && let (Some(v), Some(f)) = (attr(&e, "val"), acc.facts.as_mut())
                        {
                            if in_text_color && stack.iter().any(|s| s == b"rPr") {
                                if f.text_color.is_none() {
                                    f.text_color = Some(v.to_ascii_uppercase());
                                }
                            } else if in_sp_pr
                                && !acc.has_table
                                && stack.iter().filter(|s| s.as_slice() == b"ln").count() == 0
                                && f.fill_color.is_none()
                            {
                                f.fill_color = Some(v.to_ascii_uppercase());
                            }
                        }
                    }
                    b"br" if acc.para.is_some() => {
                        if let Some(p) = acc.para.as_mut() {
                            p.push('\n');
                        }
                    }
                    b"t" => in_t = true,
                    _ => {}
                }
                if is_start {
                    stack.push(name);
                } else if name == b"rPr" {
                    in_text_color = false;
                }
            }
            Ok(Event::End(e)) => {
                stack.pop();
                match e.local_name().as_ref() {
                    "t" => in_t = false,
                    "rPr" => in_text_color = false,
                    "spPr" => in_sp_pr = false,
                    "xfrm" => in_xfrm = false,
                    "p" => {
                        if let Some(p) = acc.para.take() {
                            if acc.in_cell {
                                acc.cell_paras.push(p);
                            } else if acc.facts.is_some() && !acc.has_table {
                                acc.paragraphs.push(p);
                            }
                        }
                    }
                    "tc" if acc.has_table => {
                        acc.in_cell = false;
                        acc.row.push(acc.cell_paras.join("\n"));
                        acc.cell_paras = Vec::new();
                    }
                    "tr" if acc.has_table => {
                        let row = std::mem::take(&mut acc.row);
                        acc.cells.push(row);
                    }
                    "sp" | "pic" | "graphicFrame" | "cxnSp" | "grpSp"
                        if stack.len() == acc.depth_at_start && acc.facts.is_some() =>
                    {
                        if let Some(mut f) = acc.facts.take() {
                            f.text = acc.paragraphs.join("\n");
                            if let (Some((x, y)), Some((w, h))) = (off, ext) {
                                f.frame = Some(Rect { x, y, w, h });
                            }
                            if acc.has_table {
                                f.kind = ShapeKind::Table;
                                out.tables.push(TableFacts {
                                    shape: f.index,
                                    cells: std::mem::take(&mut acc.cells),
                                });
                            } else if let Some(rid) = acc.has_chart.take() {
                                f.kind = ShapeKind::Chart;
                                charts.push(PendingChart {
                                    rid,
                                    facts: ChartFacts {
                                        shape: f.index,
                                        kind: String::new(),
                                        title: None,
                                        categories: Vec::new(),
                                        series: Vec::new(),
                                    },
                                });
                            }
                            out.shapes.push(f);
                        }
                        acc = ShapeAcc::default();
                        off = None;
                        ext = None;
                    }
                    _ => {}
                }
            }
            Ok(Event::Text(t)) if in_t => {
                if let Some(p) = acc.para.as_mut() {
                    let s: &str = &t;
                    p.push_str(s);
                }
            }
            Ok(Event::GeneralRef(g)) if in_t => {
                if let Some(p) = acc.para.as_mut() {
                    unescape_general_ref(&g, p);
                }
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    (out, charts)
}

struct ChartRead {
    kind: String,
    title: Option<String>,
    categories: Vec<String>,
    series: Vec<(String, Vec<Option<f64>>)>,
}

/// A chart part: the kind from the plot element, the title, the categories
/// of the first series and each series' name and values.
#[allow(clippy::too_many_lines)]
fn read_chart(xml: &[u8]) -> ChartRead {
    let mut r = Reader::from_reader(xml);
    let mut stack: Vec<Vec<u8>> = Vec::new();
    let mut kind = String::new();
    let mut bar_dir = String::new();
    let mut title = String::new();
    let mut categories: Vec<String> = Vec::new();
    let mut series: Vec<(String, Vec<Option<f64>>)> = Vec::new();
    let mut cur_name = String::new();
    let mut cur_vals: Vec<Option<f64>> = Vec::new();
    let mut cur_cats: Vec<String> = Vec::new();
    let mut in_ser = false;
    let mut pt_idx: Option<usize> = None;
    let mut in_v = false;
    let mut grabbing = String::new();
    loop {
        let ev = r.read_event();
        let is_start = matches!(ev, Ok(Event::Start(_)));
        match ev {
            Ok(Event::Start(e) | Event::Empty(e)) => {
                let name = e.local_name().as_ref().as_bytes().to_vec();
                let n = name.as_slice();
                if kind.is_empty()
                    && n.ends_with(b"Chart")
                    && !matches!(n, b"chart")
                    && stack.last().is_some_and(|p| p == b"plotArea")
                {
                    kind = match n {
                        b"barChart" | b"bar3DChart" => "bar".into(),
                        b"lineChart" | b"line3DChart" => "line".into(),
                        b"pieChart" | b"pie3DChart" | b"ofPieChart" => "pie".into(),
                        b"doughnutChart" => "doughnut".into(),
                        b"areaChart" | b"area3DChart" => "area".into(),
                        b"scatterChart" => "scatter".into(),
                        other => String::from_utf8_lossy(other).into_owned(),
                    };
                }
                if n == b"barDir"
                    && let Some(v) = attr(&e, "val")
                {
                    bar_dir = v;
                }
                if n == b"ser" {
                    in_ser = true;
                    cur_name = String::new();
                    cur_vals = Vec::new();
                    cur_cats = Vec::new();
                }
                if n == b"pt" {
                    pt_idx = attr(&e, "idx").and_then(|v| v.parse().ok());
                }
                if n == b"v" {
                    in_v = true;
                    grabbing = String::new();
                }
                if is_start {
                    stack.push(name);
                }
            }
            Ok(Event::End(e)) => {
                let name = e.local_name().as_ref().as_bytes().to_vec();
                stack.pop();
                match name.as_slice() {
                    b"v" => {
                        in_v = false;
                        let text = std::mem::take(&mut grabbing);
                        let path = |n: &[u8]| stack.iter().any(|s| s == n);
                        if in_ser && path(b"tx") {
                            cur_name = text;
                        } else if in_ser && (path(b"cat") || path(b"xVal")) {
                            let i = pt_idx.unwrap_or(cur_cats.len());
                            if cur_cats.len() <= i {
                                cur_cats.resize(i + 1, String::new());
                            }
                            cur_cats[i] = text;
                        } else if in_ser && (path(b"val") || path(b"yVal")) {
                            let i = pt_idx.unwrap_or(cur_vals.len());
                            if cur_vals.len() <= i {
                                cur_vals.resize(i + 1, None);
                            }
                            cur_vals[i] = text.trim().parse().ok();
                        } else if !in_ser && path(b"title") {
                            title.push_str(&text);
                        }
                    }
                    b"ser" => {
                        in_ser = false;
                        if categories.is_empty() {
                            categories = std::mem::take(&mut cur_cats);
                        }
                        series.push((std::mem::take(&mut cur_name), std::mem::take(&mut cur_vals)));
                    }
                    b"t" if stack.iter().any(|s| s == b"title") && !in_ser => {}
                    _ => {}
                }
            }
            Ok(Event::Text(t)) => {
                let s: &str = &t;
                if in_v {
                    grabbing.push_str(s);
                } else if !in_ser
                    && stack.last().is_some_and(|p| p == b"t")
                    && stack.iter().any(|s| s == b"title")
                {
                    title.push_str(s);
                }
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    if kind == "bar" && bar_dir == "col" {
        kind = "column".into();
    }
    ChartRead {
        kind,
        title: (!title.trim().is_empty()).then(|| title.trim().to_owned()),
        categories,
        series,
    }
}
