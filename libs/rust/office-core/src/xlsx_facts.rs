//! A read-only fact sheet of an xlsx package (ADR-0032 P4): the named cells,
//! the sheets and their charts, read from the *saved* bytes without the xlsx
//! library.
//!
//! `Workbook::open` parses every sheet. A tool that must check that its edit is
//! in the file, where it was aimed, would pay for a second full parse if it
//! reopened the result. This reads the canonical archive (it has passed the
//! validation gate) as a stream and keeps only what was asked for, and it reads
//! inline strings (`t="inlineStr"`), which the library reads back as empty.

use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
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
    /// The parts only this sheet reaches (its own part, and through its
    /// relationships the drawings, charts, comments, tables and media no other
    /// sheet uses), counted by kind. What deleting the sheet takes with it.
    pub owned_parts: BTreeMap<String, usize>,
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
    /// Whether the sheet named by [`XlsxQuery::sheet_mention`] (compared
    /// ignoring case) is referred to anywhere: a cell formula, a defined name,
    /// a data validation, a conditional format, a hyperlink location, a chart
    /// reference or a pivot cache source. Quoted names (`'My Sheet'!A1`, a
    /// doubled quote for an apostrophe) and 3-D ranges (`Sheet1:Sheet3!A1`,
    /// which names every sheet in the span by position) are understood.
    pub sheet_mentioned: bool,
    /// The same, not counting what belongs to the named sheet itself (its own
    /// cells, validations, hyperlinks and charts, which a delete takes with
    /// it): defined names, other sheets and pivot caches only.
    pub sheet_mentioned_elsewhere: bool,
    /// The `localSheetId` of every defined name that has one (a name or print
    /// area scoped to a sheet, by position).
    pub local_name_sheet_ids: Vec<usize>,
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

/// Read one range of one sheet in a SINGLE validate-and-scan: `choose` is given
/// the sheet names and returns the sheet and the range to read (`None`: the
/// sheet's value cells in file order, at most `cap` of them, which holds every
/// cell of the first `cap / width` rows). The facts carry those cells in
/// [`XlsxFacts::cells`]. Returns the chosen sheet index with them.
///
/// # Errors
/// Whatever `choose` returns, whatever the gate refuses.
pub fn facts_of_xlsx_package_choosing<F>(
    bytes: &[u8],
    limits: &Limits,
    cap: usize,
    choose: F,
) -> Result<(usize, XlsxFacts)>
where
    F: FnOnce(&[String]) -> Result<(usize, Option<CellRange>)> + Send + 'static,
{
    use crate::validate::{Kind, Mode, precheck_len, validate_and_repack};
    crate::counters::bump(crate::counters::Op::PackageRead);
    precheck_len(bytes.len(), limits)?;
    let (data, l) = (bytes.to_vec(), limits.clone());
    crate::guard::run_guarded(limits, move || {
        let repacked = validate_and_repack(&data, Kind::Xlsx, Mode::Keep, &l)?;
        let (facts, sheet) = read_xlsx_facts_with(
            &repacked.bytes,
            &l,
            &XlsxQuery::default(),
            Some((cap, Box::new(choose))),
        )?;
        Ok((sheet.unwrap_or(0), facts))
    })
}

type Choose = Box<dyn FnOnce(&[String]) -> Result<(usize, Option<CellRange>)>>;

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

pub(crate) fn read_xlsx_facts(
    package: &[u8],
    limits: &Limits,
    query: &XlsxQuery<'_>,
) -> Result<XlsxFacts> {
    read_xlsx_facts_with(package, limits, query, None).map(|(facts, _)| facts)
}

#[allow(clippy::too_many_lines)]
fn read_xlsx_facts_with(
    package: &[u8],
    limits: &Limits,
    query: &XlsxQuery<'_>,
    choose: Option<(usize, Choose)>,
) -> Result<(XlsxFacts, Option<usize>)> {
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
    let mut defined: Vec<String> = Vec::new();
    let mut local_ids: Vec<usize> = Vec::new();
    {
        let mut r = Reader::from_reader(workbook.as_slice());
        let mut in_name: Option<String> = None;
        loop {
            match r.read_event() {
                Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == b"sheet" => {
                    let name = attr(&e, b"name").unwrap_or_default();
                    let rid = rel_attr(&e, b"id").unwrap_or_default();
                    names.push((name, rid));
                }
                Ok(Event::Start(e)) if e.local_name().as_ref() == b"definedName" => {
                    if let Some(id) = attr(&e, b"localSheetId").and_then(|v| v.trim().parse().ok())
                    {
                        local_ids.push(id);
                    }
                    in_name = Some(String::new());
                }
                Ok(Event::Empty(e)) if e.local_name().as_ref() == b"definedName" => {
                    if let Some(id) = attr(&e, b"localSheetId").and_then(|v| v.trim().parse().ok())
                    {
                        local_ids.push(id);
                    }
                }
                Ok(Event::Text(t)) => {
                    if let (Some(n), Ok(s)) = (in_name.as_mut(), t.decode()) {
                        n.push_str(&s);
                    }
                }
                Ok(Event::GeneralRef(t)) => {
                    if let (Some(n), Ok(name)) = (in_name.as_mut(), t.decode())
                        && let Some(c) = crate::docx_facts::entity(&name)
                    {
                        n.push(c);
                    }
                }
                Ok(Event::End(e)) if e.local_name().as_ref() == b"definedName" => {
                    defined.extend(in_name.take());
                }
                Ok(Event::Eof) | Err(_) => break,
                Ok(_) => {}
            }
        }
    }
    let sheet_names: Vec<String> = names.iter().map(|(n, _)| n.clone()).collect();
    let target = query.sheet_mention.and_then(|m| {
        sheet_names
            .iter()
            .position(|n| n.to_lowercase() == m.to_lowercase())
    });
    let mention = target.map(|target| Mention {
        names: &sheet_names,
        target,
    });
    let mut out = XlsxFacts {
        local_name_sheet_ids: local_ids,
        ..XlsxFacts::default()
    };
    if let Some(m) = &mention {
        let hit = defined.iter().any(|d| m.refers(d));
        out.sheet_mentioned |= hit;
        out.sheet_mentioned_elsewhere |= hit;
    }
    // One read: the caller picks the sheet and range once the names are known.
    let mut chosen: Option<usize> = None;
    let mut chosen_rect: Vec<(usize, CellRange)> = Vec::new();
    let mut range_cap: Option<usize> = None;
    if let Some((cap, pick)) = choose {
        let (sheet, rect) = pick(&sheet_names)?;
        let rect = rect.unwrap_or(CellRange {
            first_row: 0,
            first_col: 0,
            last_row: 1_048_575,
            last_col: 16_383,
        });
        chosen = Some(sheet);
        chosen_rect.push((sheet, rect));
        range_cap = Some(cap.saturating_add(1));
    }
    let mut reach_sets: Vec<BTreeSet<String>> = Vec::new();
    let wanted: HashSet<(usize, u32, u16)> = query.cells.iter().copied().collect();
    let mut shared_wanted: Vec<SharedRef> = Vec::new();
    for (index, (name, rid)) in names.iter().enumerate() {
        let mut sheet = XlsxSheet {
            name: name.clone(),
            charts: Vec::new(),
            value_cells: 0,
            used_range: None,
            owned_parts: BTreeMap::new(),
        };
        let Some(target_rel) = wb_rels.get(rid) else {
            out.sheets.push(sheet);
            reach_sets.push(BTreeSet::new());
            continue;
        };
        let path = resolve(&dir, target_rel);
        let Some(xml) = entry(&mut zip, &path, large)? else {
            out.sheets.push(sheet);
            reach_sets.push(BTreeSet::new());
            continue;
        };
        reach_sets.push(reach(&mut zip, &path, small)?);
        let rects: Vec<CellRange> = query
            .ranges
            .iter()
            .chain(chosen_rect.iter())
            .filter(|(i, _)| *i == index)
            .map(|(_, r)| *r)
            .collect();
        let scan = scan_sheet(
            &xml,
            index,
            &Scan {
                wanted: &wanted,
                rects: &rects,
                range_cap,
                mention: mention.as_ref(),
            },
        );
        sheet.value_cells = scan.value_cells;
        sheet.used_range = scan.used;
        out.sheet_mentioned |= scan.mentioned;
        out.sheet_mentioned_elsewhere |= scan.mentioned && target != Some(index);
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
                        if let Some(m) = &mention {
                            let hit = chart.refs.iter().any(|f| m.refers(f));
                            out.sheet_mentioned |= hit;
                            out.sheet_mentioned_elsewhere |= hit && target != Some(index);
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
    for (i, sheet) in out.sheets.iter_mut().enumerate() {
        for part in &reach_sets[i] {
            let shared = reach_sets
                .iter()
                .enumerate()
                .any(|(j, other)| j != i && other.contains(part));
            if !shared && let Some(kind) = crate::docx_facts::kind_of(part) {
                *sheet.owned_parts.entry(kind).or_insert(0) += 1;
            }
        }
    }
    if let Some(m) = &mention {
        let pivots: Vec<String> = zip
            .file_names()
            .filter(|n| n.starts_with("xl/pivotCache/pivotCacheDefinition"))
            .map(str::to_owned)
            .collect();
        for name in pivots {
            if let Some(bytes) = entry(&mut zip, &name, small)? {
                let hit = pivot_names_sheet(&bytes, &m.names[m.target]);
                out.sheet_mentioned |= hit;
                out.sheet_mentioned_elsewhere |= hit;
            }
        }
    }
    Ok((out, chosen))
}

/// The sheets the workbook's formulas can name, and the one a call asks about.
struct Mention<'a> {
    names: &'a [String],
    target: usize,
}

impl Mention<'_> {
    /// Whether `text` (a formula, a defined name's value, a hyperlink
    /// location) refers to the target sheet: `Name!A1`, `'Name'!A1` with a
    /// doubled quote for an apostrophe, or a 3-D range `A:C!A1` that spans it
    /// by position. Names compare ignoring case.
    fn refers(&self, text: &str) -> bool {
        let chars: Vec<char> = text.chars().collect();
        for (i, &c) in chars.iter().enumerate() {
            if c != '!' || i == 0 {
                continue;
            }
            let token = if chars[i - 1] == '\'' {
                let mut out = Vec::new();
                let mut p = i - 1;
                let mut closed = false;
                while p > 0 {
                    p -= 1;
                    if chars[p] == '\'' {
                        if p > 0 && chars[p - 1] == '\'' {
                            out.push('\'');
                            p -= 1;
                        } else {
                            closed = true;
                            break;
                        }
                    } else {
                        out.push(chars[p]);
                    }
                }
                if !closed {
                    continue;
                }
                out.iter().rev().collect::<String>()
            } else {
                let mut p = i;
                while p > 0
                    && (chars[p - 1].is_alphanumeric() || matches!(chars[p - 1], '_' | '.' | ':'))
                {
                    p -= 1;
                }
                chars[p..i].iter().collect::<String>()
            };
            let index_of = |name: &str| {
                self.names
                    .iter()
                    .position(|n| n.to_lowercase() == name.to_lowercase())
            };
            let parts: Vec<&str> = token.split(':').collect();
            let hit = match parts[..] {
                [one] => index_of(one) == Some(self.target),
                [a, b] => match (index_of(a), index_of(b)) {
                    (Some(x), Some(y)) => (x.min(y)..=x.max(y)).contains(&self.target),
                    (x, y) => x == Some(self.target) || y == Some(self.target),
                },
                _ => false,
            };
            if hit {
                return true;
            }
        }
        false
    }
}

fn pivot_names_sheet(xml: &[u8], sheet: &str) -> bool {
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e))
                if e.local_name().as_ref() == b"worksheetSource" =>
            {
                if attr(&e, b"sheet").is_some_and(|s| s.to_lowercase() == sheet.to_lowercase()) {
                    return true;
                }
            }
            Ok(Event::Eof) | Err(_) => return false,
            Ok(_) => {}
        }
    }
}

/// Every part a sheet reaches through relationships (its drawings, charts,
/// comments, tables, media, ...), itself included. Other sheets, shared
/// workbook parts and external targets are not followed.
fn reach(zip: &mut Zip<'_>, sheet_path: &str, cap: u64) -> Result<BTreeSet<String>> {
    let mut seen: BTreeSet<String> = BTreeSet::new();
    let mut queue = vec![sheet_path.to_owned()];
    while let Some(part) = queue.pop() {
        if !seen.insert(part.clone()) || seen.len() > 500 {
            continue;
        }
        let Some(rels) = entry(zip, &rels_path(&part), cap)? else {
            continue;
        };
        let mut r = Reader::from_reader(rels.as_slice());
        loop {
            match r.read_event() {
                Ok(Event::Empty(e) | Event::Start(e))
                    if e.local_name().as_ref() == b"Relationship" =>
                {
                    let kind = attr(&e, b"Type").unwrap_or_default();
                    let external = attr(&e, b"TargetMode").is_some_and(|m| m == "External");
                    let skip = [
                        "/worksheet",
                        "/chartsheet",
                        "/externalLink",
                        "/theme",
                        "/styles",
                        "/sharedStrings",
                        "/calcChain",
                        "/hyperlink",
                    ]
                    .iter()
                    .any(|s| kind.ends_with(s));
                    if let Some(t) = attr(&e, b"Target")
                        && !external
                        && !skip
                    {
                        queue.push(resolve(parent(&part), &t));
                    }
                }
                Ok(Event::Eof) | Err(_) => break,
                Ok(_) => {}
            }
        }
    }
    Ok(seen)
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

/// What a sheet scan collects.
struct Scan<'a> {
    wanted: &'a HashSet<(usize, u32, u16)>,
    rects: &'a [CellRange],
    /// At most this many cells are taken for the rectangles.
    range_cap: Option<usize>,
    mention: Option<&'a Mention<'a>>,
}

#[allow(clippy::too_many_lines)]
fn scan_sheet(xml: &[u8], sheet: usize, scan_for: &Scan<'_>) -> SheetScan {
    let (wanted, rects, mention) = (scan_for.wanted, scan_for.rects, scan_for.mention);
    let mut taken = 0usize;
    let mut mtext: Option<String> = None;
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
                b"formula" | b"formula1" | b"formula2" => mtext = Some(String::new()),
                b"hyperlink" => note_location(&e, mention, &mut scan.mentioned),
                _ => {}
            },
            Ok(Event::Empty(e)) => match e.local_name().as_ref() {
                b"f" if in_c => f = Some(String::new()),
                b"drawing" => scan.drawing = rel_attr(&e, b"id").or(Some(String::new())),
                b"hyperlink" => note_location(&e, mention, &mut scan.mentioned),
                _ => {}
            },
            Ok(Event::Text(t)) => {
                if let Ok(s) = t.decode() {
                    if let Some(m) = mtext.as_mut() {
                        m.push_str(&s);
                    }
                    push(&mut f, &mut v, &mut is, into, &s);
                }
            }
            Ok(Event::GeneralRef(t)) => {
                if let Ok(name) = t.decode()
                    && let Some(c) = crate::docx_facts::entity(&name)
                {
                    if let Some(m) = mtext.as_mut() {
                        m.push(c);
                    }
                    push(&mut f, &mut v, &mut is, into, &c.to_string());
                }
            }
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                b"f" | b"v" | b"t" => into = Target::Nowhere,
                b"formula" | b"formula1" | b"formula2" => {
                    if let (Some(m), Some(text)) = (mention, mtext.take())
                        && m.refers(&text)
                    {
                        scan.mentioned = true;
                    }
                }
                b"rPh" => in_phonetic = false,
                b"c" => {
                    in_c = false;
                    if let (Some(m), Some(formula)) = (mention, f.as_deref())
                        && m.refers(formula)
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
                    let in_rect = coord.is_some_and(|(row, col)| {
                        rects.iter().any(|r| {
                            (r.first_row..=r.last_row).contains(&row)
                                && (r.first_col..=r.last_col).contains(&col)
                        })
                    });
                    let in_rect = in_rect
                        && holds
                        && scan_for.range_cap.is_none_or(|cap| {
                            taken += 1;
                            taken <= cap
                        });
                    if let Some((row, col)) = coord
                        && (wanted.contains(&(sheet, row, col)) || in_rect)
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

fn note_location(e: &quick_xml::events::BytesStart<'_>, m: Option<&Mention<'_>>, hit: &mut bool) {
    if let (Some(m), Some(loc)) = (m, attr(e, b"location"))
        && m.refers(&loc)
    {
        *hit = true;
    }
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
            // An empty string is an `<si/>`: it still takes an index.
            Ok(Event::Empty(e)) if e.local_name().as_ref() == b"si" => {
                if need.contains(&index) {
                    out.insert(index, String::new());
                }
                index += 1;
            }
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
            // An empty element never opens text: `<c:f/>` is a reference that is
            // empty, and `in_f` must not outlive it.
            Ok(Event::Empty(e)) => {
                let name = e.local_name();
                let name = name.as_ref();
                match name {
                    b"f" => refs.push(String::new()),
                    b"barDir" => bar_dir = attr(&e, b"val"),
                    n if kind.is_none() && n.ends_with(b"Chart") && n != b"chart" => {
                        let raw = String::from_utf8_lossy(&n[..n.len() - 5]).into_owned();
                        kind = Some(raw);
                    }
                    _ => {}
                }
            }
            Ok(Event::Start(e)) => {
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

#[cfg(test)]
mod tests {
    use super::*;

    fn mention(names: &[String], target: usize) -> Mention<'_> {
        Mention { names, target }
    }

    fn names(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| (*s).to_owned()).collect()
    }

    #[test]
    fn references_are_found_quoted_doubled_case_folded_and_three_d() {
        let n = names(&["Sheet1", "My Sheet", "O'Brien", "Sheet3", "Last"]);
        // Unquoted, and ignoring case.
        assert!(mention(&n, 0).refers("SUM(sheet1!A1:A3)"));
        assert!(mention(&n, 0).refers("Sheet1!A1+1"));
        // Quoted, with spaces.
        assert!(mention(&n, 1).refers("'my sheet'!B2*2"));
        assert!(mention(&n, 1).refers("SUM('My Sheet'!A:A)"));
        // A doubled quote is an apostrophe in the name.
        assert!(mention(&n, 2).refers("'O''Brien'!A1"));
        assert!(!mention(&n, 2).refers("'O'Brien'!A1"));
        // Another sheet is not this one.
        assert!(!mention(&n, 1).refers("Sheet1!A1+Sheet3!A1"));
        assert!(!mention(&n, 3).refers("A1+B2"));
        // A 3-D range names every sheet in the span, by position, either way round.
        assert!(mention(&n, 1).refers("SUM(Sheet1:Sheet3!A1)"));
        assert!(mention(&n, 2).refers("SUM(Sheet3:Sheet1!A1)"));
        assert!(mention(&n, 1).refers("SUM('Sheet1:Sheet3'!A1)"));
        assert!(!mention(&n, 4).refers("SUM(Sheet1:Sheet3!A1)"));
        // An unterminated quote is not a reference.
        assert!(!mention(&n, 1).refers("'My Sheet!A1"));
    }

    #[test]
    fn an_empty_shared_string_takes_an_index() {
        let xml = b"<sst xmlns=\"x\"><si><t>a</t></si><si/><si><t>c</t></si></sst>";
        let need: HashSet<usize> = [0, 1, 2].into_iter().collect();
        let got = read_shared(xml, &need);
        assert_eq!(got[&0], "a");
        assert_eq!(got[&1], "");
        assert_eq!(got[&2], "c", "an `<si/>` shifted every later index");
    }

    #[test]
    fn an_empty_chart_reference_does_not_swallow_what_follows() {
        let xml = b"<c:chartSpace xmlns:c=\"c\" xmlns:a=\"a\"><c:chart><c:title/><c:plotArea><c:barChart><c:barDir val=\"col\"/><c:ser><c:tx><c:strRef><c:f/></c:strRef></c:tx><c:cat><c:strLit><c:pt><c:v>stray</c:v></c:pt></c:strLit></c:cat><c:val><c:numRef><c:f>Sheet1!$B$1:$B$2</c:f></c:numRef></c:val></c:ser></c:barChart></c:plotArea></c:chart></c:chartSpace>";
        let chart = read_chart(xml);
        assert_eq!(chart.refs, ["", "Sheet1!$B$1:$B$2"]);
        assert_eq!(chart.kind, "column");
        // `<c:title/>` is an empty title, not an open one.
        assert_eq!(chart.title, None);
    }
}
