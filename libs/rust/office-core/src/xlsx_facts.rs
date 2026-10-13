//! A read-only fact sheet of an xlsx package (ADR-0032 P4): the named cells,
//! the sheets and their charts, read from the *saved* bytes without the xlsx
//! library.
//!
//! `Workbook::open` parses every sheet. A tool that must check that its edit is
//! in the file, where it was aimed, would pay for a second full parse if it
//! reopened the result. This reads the canonical archive (it has passed the
//! validation gate) as a stream and keeps only what was asked for, and it reads
//! inline strings (`t="inlineStr"`), which the library reads back as empty.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::io::{Cursor, Read};

use quick_xml::Reader;
use quick_xml::events::Event;
use zip::ZipArchive;

use crate::a1::parse_cell_ref;
use crate::error::{OfficeError, Result};
use crate::inspect::{attr, parent, read_rels, rel_attr, rels_path, resolve};
use crate::limits::Limits;
use crate::workbook::{CellRange, CellValue};

/// One chart of a sheet, read back from its chart part.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct XlsxChart {
    /// `bar`, `column`, `line`, `pie`, `doughnut`, `area`, `scatter` or the
    /// plot element's own name.
    pub kind: String,
    /// The chart title, when it has one.
    pub title: Option<String>,
    /// Every `c:f` reference, in file order (series name, categories, values).
    pub refs: Vec<String>,
}

/// One sheet.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct XlsxSheet {
    /// The sheet name.
    pub name: String,
    /// The charts drawn on it.
    pub charts: Vec<XlsxChart>,
    /// How many cells hold a value or a formula.
    pub value_cells: u64,
    /// The smallest rectangle holding them, `None` for an empty sheet.
    pub used_range: Option<CellRange>,
}

/// What to read besides the sheets and charts.
#[derive(Debug, Clone, Copy, Default)]
pub struct XlsxQuery<'a> {
    /// `(sheet, row, col)` of cells to read, 0-based.
    pub cells: &'a [(usize, u32, u16)],
    /// `(sheet, range)` rectangles whose every value cell is read.
    pub ranges: &'a [(usize, CellRange)],
    /// A sheet name to look for in every formula and chart reference.
    pub sheet_mention: Option<&'a str>,
}

/// What an xlsx package holds.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct XlsxFacts {
    /// The sheets, in order.
    pub sheets: Vec<XlsxSheet>,
    /// The queried cells that hold something (absent means empty). A formula
    /// carries its cached value.
    pub cells: HashMap<(usize, u32, u16), CellValue>,
    /// Whether [`XlsxQuery::sheet_mention`] occurs in a formula or a chart
    /// reference.
    pub sheet_mentioned: bool,
    /// How many package parts of each kind the file holds (`xl/charts/chart`,
    /// `xl/media/image`, ...). What a save must not lose.
    pub parts: BTreeMap<String, usize>,
}

/// The fact sheet of an xlsx package given as bytes, without opening it in the
/// xlsx library.
///
/// # Errors
/// Whatever the gate refuses, [`OfficeError::Malformed`], [`OfficeError::Busy`].
pub fn facts_of_xlsx_package(
    bytes: &[u8],
    limits: &Limits,
    query: &XlsxQuery<'_>,
) -> Result<XlsxFacts> {
    use crate::validate::{Kind, Mode, precheck_len, validate_and_repack};
    crate::counters::bump(crate::counters::Op::PackageRead);
    precheck_len(bytes.len(), limits)?;
    let (data, l) = (bytes.to_vec(), limits.clone());
    let cells = query.cells.to_vec();
    let ranges = query.ranges.to_vec();
    let mention = query.sheet_mention.map(str::to_owned);
    crate::guard::run_guarded(limits, move || {
        let repacked = validate_and_repack(&data, Kind::Xlsx, Mode::Keep, &l)?;
        read_xlsx_facts(
            &repacked.bytes,
            &l,
            &XlsxQuery {
                cells: &cells,
                ranges: &ranges,
                sheet_mention: mention.as_deref(),
            },
        )
    })
}

type Zip<'a> = ZipArchive<Cursor<&'a [u8]>>;

fn entry(zip: &mut Zip<'_>, name: &str, cap: u64) -> Result<Option<Vec<u8>>> {
    let Ok(e) = zip.by_name(name) else {
        return Ok(None);
    };
    let mut out = Vec::new();
    e.take(cap + 1)
        .read_to_end(&mut out)
        .map_err(|e| OfficeError::Malformed(format!("part {name}: {e}")))?;
    if out.len() as u64 > cap {
        return Err(OfficeError::Malformed(format!("part {name} is too large")));
    }
    Ok(Some(out))
}

fn main_part_name(zip: &mut Zip<'_>, cap: u64) -> Result<String> {
    let default = "xl/workbook.xml".to_owned();
    let Some(xml) = entry(zip, "_rels/.rels", cap)? else {
        return Ok(default);
    };
    let mut r = Reader::from_reader(xml.as_slice());
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"Relationship" => {
                if attr(&e, b"Type").is_some_and(|t| t.ends_with("/officeDocument"))
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

fn rel_target(xml: &[u8], suffix: &str) -> Option<String> {
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"Relationship" => {
                if attr(&e, b"Type").is_some_and(|t| t.ends_with(suffix)) {
                    return attr(&e, b"Target");
                }
            }
            Ok(Event::Eof) | Err(_) => return None,
            Ok(_) => {}
        }
    }
}

fn rel_targets(xml: &[u8], suffix: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"Relationship" => {
                if attr(&e, b"Type").is_some_and(|t| t.ends_with(suffix))
                    && let Some(t) = attr(&e, b"Target")
                {
                    out.push(t);
                }
            }
            Ok(Event::Eof) | Err(_) => return out,
            Ok(_) => {}
        }
    }
}

#[allow(clippy::too_many_lines)]
pub(crate) fn read_xlsx_facts(
    package: &[u8],
    limits: &Limits,
    query: &XlsxQuery<'_>,
) -> Result<XlsxFacts> {
    let mut zip = ZipArchive::new(Cursor::new(package))
        .map_err(|e| OfficeError::Malformed(format!("package: {e}")))?;
    let small = limits.max_xml_part_bytes;
    let large = limits.max_xml_large_part_bytes_xlsx;
    let main = main_part_name(&mut zip, small)?;
    let dir = parent(&main).to_owned();
    let workbook = entry(&mut zip, &main, small)?
        .ok_or_else(|| OfficeError::Malformed(format!("no {main}")))?;
    let wb_rels = entry(&mut zip, &rels_path(&main), small)?
        .map(|b| read_rels(&b))
        .unwrap_or_default();
    let mut names: Vec<(String, String)> = Vec::new();
    {
        let mut r = Reader::from_reader(workbook.as_slice());
        loop {
            match r.read_event() {
                Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"sheet" => {
                    let name = attr(&e, b"name").unwrap_or_default();
                    let rid = rel_attr(&e, b"id").unwrap_or_default();
                    names.push((name, rid));
                }
                Ok(Event::Eof) | Err(_) => break,
                Ok(_) => {}
            }
        }
    }
    let mut out = XlsxFacts::default();
    let wanted: HashSet<(usize, u32, u16)> = query.cells.iter().copied().collect();
    let mut shared_wanted: Vec<SharedRef> = Vec::new();
    for (index, (name, rid)) in names.iter().enumerate() {
        let mut sheet = XlsxSheet {
            name: name.clone(),
            charts: Vec::new(),
            value_cells: 0,
            used_range: None,
        };
        let Some(target) = wb_rels.get(rid) else {
            out.sheets.push(sheet);
            continue;
        };
        let path = resolve(&dir, target);
        let Some(xml) = entry(&mut zip, &path, large)? else {
            out.sheets.push(sheet);
            continue;
        };
        let rects: Vec<CellRange> = query
            .ranges
            .iter()
            .filter(|(i, _)| *i == index)
            .map(|(_, r)| *r)
            .collect();
        let scan = scan_sheet(&xml, index, &wanted, &rects, query.sheet_mention);
        sheet.value_cells = scan.value_cells;
        sheet.used_range = scan.used;
        out.sheet_mentioned |= scan.mentioned;
        out.cells.extend(scan.cells);
        shared_wanted.extend(scan.shared);
        if scan.drawing.is_some() {
            let rels = entry(&mut zip, &rels_path(&path), small)?;
            let drawing = rels
                .as_deref()
                .and_then(|r| rel_target(r, "/drawing"))
                .map(|t| resolve(parent(&path), &t));
            if let Some(drawing) = drawing {
                let drels = entry(&mut zip, &rels_path(&drawing), small)?;
                for t in drels
                    .as_deref()
                    .map(|r| rel_targets(r, "/chart"))
                    .unwrap_or_default()
                {
                    let chart_path = resolve(parent(&drawing), &t);
                    if let Some(bytes) = entry(&mut zip, &chart_path, small)? {
                        let chart = read_chart(&bytes);
                        if let Some(m) = query.sheet_mention {
                            out.sheet_mentioned |= chart.refs.iter().any(|f| mentions(f, m));
                        }
                        sheet.charts.push(chart);
                    }
                }
            }
        }
        out.sheets.push(sheet);
    }
    if !shared_wanted.is_empty() {
        let shared_path = wb_rels
            .values()
            .find(|t| t.ends_with("sharedStrings.xml"))
            .map_or_else(|| format!("{dir}/sharedStrings.xml"), |t| resolve(&dir, t));
        let need: HashSet<usize> = shared_wanted.iter().map(|(_, i, _)| *i).collect();
        let strings = match entry(&mut zip, &shared_path, large)? {
            Some(xml) => read_shared(&xml, &need),
            None => HashMap::new(),
        };
        for (key, index, formula) in shared_wanted {
            let text = strings.get(&index).cloned().unwrap_or_default();
            out.cells
                .insert(key, with_formula(CellValue::Text(text), formula));
        }
    }
    out.parts = crate::docx_facts::part_kinds(&mut zip);
    Ok(out)
}

fn with_formula(value: CellValue, formula: Option<String>) -> CellValue {
    match formula {
        Some(formula) => CellValue::Formula {
            formula,
            cached: (value != CellValue::Empty).then(|| Box::new(value)),
        },
        None => value,
    }
}

/// Whether a formula or reference names `sheet` (`Name!A1` or `'Name'!A1`).
fn mentions(formula: &str, sheet: &str) -> bool {
    formula.contains(&format!("{sheet}!"))
        || formula.contains(&format!("'{}'!", sheet.replace('\'', "''")))
}

/// A cell whose text is a shared string: the cell, the string's index and the
/// formula the cell also carries.
type SharedRef = ((usize, u32, u16), usize, Option<String>);

struct SheetScan {
    value_cells: u64,
    used: Option<CellRange>,
    cells: Vec<((usize, u32, u16), CellValue)>,
    shared: Vec<SharedRef>,
    drawing: Option<String>,
    mentioned: bool,
}

#[derive(Clone, Copy, PartialEq)]
enum Target {
    Nowhere,
    Formula,
    Value,
    Inline,
}

#[allow(clippy::too_many_lines)]
fn scan_sheet(
    xml: &[u8],
    sheet: usize,
    wanted: &HashSet<(usize, u32, u16)>,
    rects: &[CellRange],
    mention: Option<&str>,
) -> SheetScan {
    let mut scan = SheetScan {
        value_cells: 0,
        used: None,
        cells: Vec::new(),
        shared: Vec::new(),
        drawing: None,
        mentioned: false,
    };
    let mut r = Reader::from_reader(xml);
    let mut coord: Option<(u32, u16)> = None;
    let (mut kind, mut f, mut v, mut is) = (String::new(), None::<String>, None::<String>, None);
    let mut into = Target::Nowhere;
    let mut in_c = false;
    let mut in_phonetic = false;
    loop {
        match r.read_event() {
            Ok(Event::Start(e)) => match e.local_name().as_ref() {
                b"c" => {
                    in_c = true;
                    coord = attr(&e, b"r").and_then(|a| parse_cell_ref(&a));
                    kind = attr(&e, b"t").unwrap_or_default();
                    (f, v, is) = (None, None, None);
                }
                b"f" if in_c => {
                    into = Target::Formula;
                    f = Some(String::new());
                }
                b"v" if in_c => {
                    into = Target::Value;
                    v = Some(String::new());
                }
                b"rPh" if in_c => in_phonetic = true,
                b"t" if in_c && !in_phonetic => {
                    into = Target::Inline;
                    is.get_or_insert_with(String::new);
                }
                b"drawing" => scan.drawing = rel_attr(&e, b"id").or(Some(String::new())),
                _ => {}
            },
            Ok(Event::Empty(e)) => match e.local_name().as_ref() {
                b"f" if in_c => f = Some(String::new()),
                b"drawing" => scan.drawing = rel_attr(&e, b"id").or(Some(String::new())),
                _ => {}
            },
            Ok(Event::Text(t)) => {
                if let Ok(s) = t.decode() {
                    push(&mut f, &mut v, &mut is, into, &s);
                }
            }
            Ok(Event::GeneralRef(t)) => {
                if let Ok(name) = t.decode()
                    && let Some(c) = crate::docx_facts::entity(&name)
                {
                    push(&mut f, &mut v, &mut is, into, &c.to_string());
                }
            }
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                b"f" | b"v" | b"t" => into = Target::Nowhere,
                b"rPh" => in_phonetic = false,
                b"c" => {
                    in_c = false;
                    if let (Some(mention), Some(formula)) = (mention, f.as_deref())
                        && mentions(formula, mention)
                    {
                        scan.mentioned = true;
                    }
                    let holds = f.as_deref().is_some_and(|t| !t.is_empty())
                        || v.as_deref().is_some_and(|t| !t.is_empty())
                        || is.as_deref().is_some_and(|t| !t.is_empty());
                    if holds {
                        scan.value_cells += 1;
                        if let Some((row, col)) = coord {
                            scan.used = Some(scan.used.map_or(
                                CellRange {
                                    first_row: row,
                                    first_col: col,
                                    last_row: row,
                                    last_col: col,
                                },
                                |u| CellRange {
                                    first_row: u.first_row.min(row),
                                    first_col: u.first_col.min(col),
                                    last_row: u.last_row.max(row),
                                    last_col: u.last_col.max(col),
                                },
                            ));
                        }
                    }
                    if let Some((row, col)) = coord
                        && (wanted.contains(&(sheet, row, col))
                            || rects.iter().any(|r| {
                                (r.first_row..=r.last_row).contains(&row)
                                    && (r.first_col..=r.last_col).contains(&col)
                            }))
                    {
                        let key = (sheet, row, col);
                        let formula = f.take().filter(|s| !s.is_empty());
                        match kind.as_str() {
                            "s" => {
                                if let Some(i) = v.as_deref().and_then(|s| s.trim().parse().ok()) {
                                    scan.shared.push((key, i, formula));
                                }
                            }
                            other => {
                                let value = match other {
                                    "inlineStr" => is.take().map(CellValue::Text),
                                    "str" | "d" => v.take().map(CellValue::Text),
                                    "b" => v.take().map(|s| CellValue::Bool(s.trim() == "1")),
                                    "e" => v.take().map(CellValue::Error),
                                    _ => v
                                        .take()
                                        .and_then(|s| s.trim().parse::<f64>().ok())
                                        .map(CellValue::Number),
                                };
                                if value.is_some() || formula.is_some() {
                                    scan.cells.push((
                                        key,
                                        with_formula(value.unwrap_or(CellValue::Empty), formula),
                                    ));
                                }
                            }
                        }
                    }
                }
                _ => {}
            },
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    scan
}

fn push(
    f: &mut Option<String>,
    v: &mut Option<String>,
    is: &mut Option<String>,
    into: Target,
    s: &str,
) {
    let target = match into {
        Target::Formula => f,
        Target::Value => v,
        Target::Inline => is,
        Target::Nowhere => return,
    };
    if let Some(t) = target.as_mut() {
        t.push_str(s);
    }
}

fn read_shared(xml: &[u8], need: &HashSet<usize>) -> HashMap<usize, String> {
    let mut out = HashMap::new();
    let mut r = Reader::from_reader(xml);
    let mut index = 0usize;
    let (mut in_si, mut in_t, mut in_phonetic) = (false, false, false);
    let mut text = String::new();
    loop {
        match r.read_event() {
            Ok(Event::Start(e)) => match e.local_name().as_ref() {
                b"si" => {
                    in_si = true;
                    text.clear();
                }
                b"rPh" => in_phonetic = true,
                b"t" if in_si && !in_phonetic => in_t = true,
                _ => {}
            },
            Ok(Event::Text(t)) if in_t => {
                if let Ok(s) = t.decode() {
                    text.push_str(&s);
                }
            }
            Ok(Event::GeneralRef(t)) if in_t => {
                if let Ok(name) = t.decode()
                    && let Some(c) = crate::docx_facts::entity(&name)
                {
                    text.push(c);
                }
            }
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                b"t" => in_t = false,
                b"rPh" => in_phonetic = false,
                b"si" => {
                    if need.contains(&index) {
                        out.insert(index, std::mem::take(&mut text));
                    }
                    index += 1;
                    in_si = false;
                }
                _ => {}
            },
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    out
}

fn read_chart(xml: &[u8]) -> XlsxChart {
    let mut r = Reader::from_reader(xml);
    let mut kind: Option<String> = None;
    let mut bar_dir: Option<String> = None;
    let (mut in_title, mut in_t, mut in_f, mut seen_plot) = (false, false, false, false);
    let mut title = String::new();
    let mut have_title = false;
    let mut refs: Vec<String> = Vec::new();
    loop {
        match r.read_event() {
            Ok(Event::Start(e) | Event::Empty(e)) => {
                let name = e.local_name();
                let name = name.as_ref();
                match name {
                    b"plotArea" => seen_plot = true,
                    b"title" if !seen_plot => in_title = true,
                    b"t" if in_title => in_t = true,
                    b"f" => {
                        in_f = true;
                        refs.push(String::new());
                    }
                    b"barDir" => bar_dir = attr(&e, b"val"),
                    n if kind.is_none() && n.ends_with(b"Chart") && n != b"chart" => {
                        let raw = String::from_utf8_lossy(&n[..n.len() - 5]).into_owned();
                        kind = Some(raw);
                    }
                    _ => {}
                }
            }
            Ok(Event::Text(t)) => {
                if let Ok(s) = t.decode() {
                    if in_t {
                        title.push_str(&s);
                        have_title = true;
                    } else if in_f && let Some(last) = refs.last_mut() {
                        last.push_str(&s);
                    }
                }
            }
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                b"title" => in_title = false,
                b"t" => in_t = false,
                b"f" => in_f = false,
                _ => {}
            },
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    let kind = match (kind.as_deref(), bar_dir.as_deref()) {
        (Some("bar"), Some("col")) => "column".to_owned(),
        (Some(k), _) => k.to_owned(),
        (None, _) => "unknown".to_owned(),
    };
    XlsxChart {
        kind,
        title: have_title.then_some(title),
        refs,
    }
}
