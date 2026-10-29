//! A read-only fact sheet of an xlsx package (ADR-0032 P4): the named cells,
//! the sheets and their charts, read from the *saved* bytes without the xlsx
//! library.
//!
//! `Workbook::open` parses every sheet. A tool that must check that its edit is
//! in the file, where it was aimed, would pay for a second full parse if it
//! reopened the result. This reads the canonical archive (it has passed the
//! validation gate) as a stream and keeps only what was asked for, and it reads
//! inline strings (`t="inlineStr"`), which the library reads back as empty.
//!
//! What a rename or a delete of a sheet would break is found by a generic scan:
//! every text node and every attribute value of the workbook's XML parts is
//! matched against the sheet-reference grammar (see [`XlsxFacts::sheet_mentioned`]),
//! so a producer's extension element is covered without being listed. The
//! relationship walks (what only one sheet reaches) run only when the query
//! names a sheet.

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
}

/// A defined name of the workbook.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DefinedName {
    /// Its name (`_xlnm.Print_Area`).
    pub name: String,
    /// The sheet it is scoped to, by position (`localSheetId`).
    pub local: Option<usize>,
    /// What it refers to.
    pub text: String,
}

/// What to read besides the sheets and charts.
#[derive(Debug, Clone, Copy, Default)]
pub struct XlsxQuery<'a> {
    /// `(sheet, row, col)` of cells to read, 0-based.
    pub cells: &'a [(usize, u32, u16)],
    /// `(sheet, range)` rectangles whose every value cell is read.
    pub ranges: &'a [(usize, CellRange)],
    /// A sheet name to look for references to (compared ignoring case).
    pub sheet_mention: Option<&'a str>,
    /// With `sheet_mention`: the sheet is about to be deleted, so work out the
    /// parts only it reaches ([`XlsxFacts::delete_exempt`]).
    pub for_delete: bool,
}

/// What an xlsx package holds.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct XlsxFacts {
    /// The sheets, in order.
    pub sheets: Vec<XlsxSheet>,
    /// The queried cells that hold something (absent means empty). A formula
    /// carries its cached value.
    pub cells: HashMap<(usize, u32, u16), CellValue>,
    /// Whether the sheet named by [`XlsxQuery::sheet_mention`] is referred to
    /// anywhere: any text node or attribute value of the workbook's XML parts
    /// (worksheets, charts, drawings, pivot caches and tables, tables, and
    /// whatever an extension list holds) that names it as `Name!A1` or
    /// `'Name'!A1` (a doubled quote for an apostrophe), or as a 3-D span
    /// `Sheet1:Sheet3!A1` that covers it by position; a pivot cache sourced by
    /// the sheet's name, by one of its tables or by a defined name that refers
    /// to it; a defined name that is not scoped to the sheet itself. Cell
    /// values are not scanned.
    pub sheet_mentioned: bool,
    /// The same, not counting what only the named sheet reaches (its own cells
    /// and charts, which a delete takes with it). Only with
    /// [`XlsxQuery::for_delete`]; otherwise the same as `sheet_mentioned`.
    pub sheet_mentioned_elsewhere: bool,
    /// The workbook's defined names.
    pub defined_names: Vec<DefinedName>,
    /// How many package parts of each kind the file holds (`xl/charts/chart`,
    /// `xl/media/image`, ...). What a save must not lose.
    pub parts: BTreeMap<String, usize>,
    /// With [`XlsxQuery::for_delete`]: the parts, counted by kind, that only
    /// the named sheet reaches: reachable from it, and from no other sheet and
    /// not from the workbook's own relationships (so a pivot cache, shared
    /// media and everything else the workbook holds are never in it).
    pub delete_exempt: BTreeMap<String, usize>,
    /// How many parts the relationship walks visited: 0 unless the query names
    /// a sheet.
    pub walked_parts: usize,
}

/// The gate shared by every read: count it, check the length, copy the bytes
/// onto the guarded parse thread, validate and re-pack them, and run `read` on
/// the canonical archive.
fn gated<R, F>(bytes: &[u8], limits: &Limits, read: F) -> Result<R>
where
    R: Send + 'static,
    F: FnOnce(&[u8], &Limits) -> Result<R> + Send + 'static,
{
    use crate::validate::{Kind, Mode, precheck_len, validate_and_repack};
    crate::counters::bump(crate::counters::Op::PackageRead);
    precheck_len(bytes.len(), limits)?;
    let (data, l) = (bytes.to_vec(), limits.clone());
    crate::guard::run_guarded(limits, move || {
        let repacked = validate_and_repack(&data, Kind::Xlsx, Mode::Keep, &l)?;
        read(&repacked.bytes, &l)
    })
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
    let cells = query.cells.to_vec();
    let ranges = query.ranges.to_vec();
    let mention = query.sheet_mention.map(str::to_owned);
    let for_delete = query.for_delete;
    gated(bytes, limits, move |package, l| {
        read_xlsx_facts(
            package,
            l,
            &XlsxQuery {
                cells: &cells,
                ranges: &ranges,
                sheet_mention: mention.as_deref(),
                for_delete,
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
    gated(bytes, limits, move |package, l| {
        let (facts, sheet) = read_xlsx_facts_with(
            package,
            l,
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
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == "Relationship" => {
                if attr(&e, "Type").is_some_and(|t| t.ends_with("/officeDocument"))
                    && let Some(target) = attr(&e, "Target")
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

fn rel_targets(xml: &[u8], suffix: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == "Relationship" => {
                if attr(&e, "Type").is_some_and(|t| t.ends_with(suffix))
                    && let Some(t) = attr(&e, "Target")
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

/// The workbook part: its sheets (name, relationship id) and defined names.
fn read_workbook(xml: &[u8]) -> (Vec<(String, String)>, Vec<DefinedName>) {
    let (mut sheets, mut defined) = (Vec::new(), Vec::new());
    let mut r = Reader::from_reader(xml);
    let mut open: Option<DefinedName> = None;
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e)) if e.local_name().as_ref() == "sheet" => {
                sheets.push((
                    attr(&e, "name").unwrap_or_default(),
                    rel_attr(&e, "id").unwrap_or_default(),
                ));
            }
            Ok(Event::Start(e)) if e.local_name().as_ref() == "definedName" => {
                open = Some(DefinedName {
                    name: attr(&e, "name").unwrap_or_default(),
                    local: attr(&e, "localSheetId").and_then(|v| v.trim().parse().ok()),
                    text: String::new(),
                });
            }
            Ok(Event::Empty(e)) if e.local_name().as_ref() == "definedName" => {
                defined.push(DefinedName {
                    name: attr(&e, "name").unwrap_or_default(),
                    local: attr(&e, "localSheetId").and_then(|v| v.trim().parse().ok()),
                    text: String::new(),
                });
            }
            Ok(Event::Text(t)) => {
                if let Some(n) = open.as_mut() {
                    let s: &str = &t;
                    n.text.push_str(s);
                }
            }
            Ok(Event::GeneralRef(t)) => {
                if let Some(n) = open.as_mut()
                    && let Some(c) = crate::docx_facts::entity(&t)
                {
                    n.text.push(c);
                }
            }
            Ok(Event::End(e)) if e.local_name().as_ref() == "definedName" => {
                defined.extend(open.take());
            }
            Ok(Event::Eof) | Err(_) => break,
            Ok(_) => {}
        }
    }
    (sheets, defined)
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
    let (names, defined) = read_workbook(&workbook);
    let sheet_names: Vec<String> = names.iter().map(|(n, _)| n.clone()).collect();
    let target = query.sheet_mention.and_then(|m| {
        sheet_names
            .iter()
            .position(|n| n.to_lowercase() == m.to_lowercase())
    });
    let mut out = XlsxFacts {
        defined_names: defined,
        ..XlsxFacts::default()
    };
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
    let wanted: HashSet<(usize, u32, u16)> = query.cells.iter().copied().collect();
    let mut shared_wanted: Vec<SharedRef> = Vec::new();
    let mut sheet_paths: Vec<Option<String>> = Vec::new();
    for (index, (name, rid)) in names.iter().enumerate() {
        let mut sheet = XlsxSheet {
            name: name.clone(),
            charts: Vec::new(),
            value_cells: 0,
            used_range: None,
        };
        let Some(target_rel) = wb_rels.get(rid) else {
            out.sheets.push(sheet);
            sheet_paths.push(None);
            continue;
        };
        let path = resolve(&dir, target_rel);
        sheet_paths.push(Some(path.clone()));
        let Some(xml) = entry(&mut zip, &path, large)? else {
            out.sheets.push(sheet);
            continue;
        };
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
            },
        );
        sheet.value_cells = scan.value_cells;
        sheet.used_range = scan.used;
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
                        sheet.charts.push(read_chart(&bytes));
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
    if let Some(t) = target {
        find_mentions(
            &mut zip,
            &mut out,
            &Walk {
                main: &main,
                names: &sheet_names,
                target: t,
                sheet_paths: &sheet_paths,
                for_delete: query.for_delete,
                small,
                large,
            },
        )?;
    }
    Ok((out, chosen))
}

/// What the relationship walks and the mention scan need to know.
struct Walk<'a> {
    main: &'a str,
    names: &'a [String],
    target: usize,
    sheet_paths: &'a [Option<String>],
    for_delete: bool,
    small: u64,
    large: u64,
}

/// Fill in the mention facts for the named sheet: what refers to it, and (for a
/// delete) which parts only it reaches. Runs only when the query names a sheet.
fn find_mentions(zip: &mut Zip<'_>, out: &mut XlsxFacts, w: &Walk<'_>) -> Result<()> {
    let none: BTreeSet<String> = BTreeSet::new();
    let mut walked = 0usize;
    let mut target_reach = BTreeSet::new();
    let mut exempt: BTreeSet<String> = BTreeSet::new();
    if let Some(Some(path)) = w.sheet_paths.get(w.target) {
        target_reach = reach(zip, path, w.small, false, &none)?;
        walked += target_reach.len();
        if w.for_delete {
            let mut others = BTreeSet::new();
            for (i, p) in w.sheet_paths.iter().enumerate() {
                if i != w.target
                    && let Some(p) = p
                {
                    others.extend(reach(zip, p, w.small, false, &none)?);
                }
            }
            // The workbook's own relationships, not through the deleted sheet:
            // pivot caches and anything else the workbook holds stay.
            let skip = BTreeSet::from([path.clone()]);
            let workbook = reach(zip, w.main, w.small, true, &skip)?;
            walked += others.len() + workbook.len();
            exempt = target_reach
                .iter()
                .filter(|p| !others.contains(*p) && !workbook.contains(*p))
                .cloned()
                .collect();
            for part in &exempt {
                if let Some(kind) = crate::docx_facts::kind_of(part) {
                    *out.delete_exempt.entry(kind).or_insert(0) += 1;
                }
            }
        }
    }
    out.walked_parts = walked;
    let mention = Mention {
        names: w.names,
        target: w.target,
    };
    let hit = |out: &mut XlsxFacts, part: Option<&str>| {
        out.sheet_mentioned = true;
        out.sheet_mentioned_elsewhere |= part.is_none_or(|p| !exempt.contains(p));
    };
    // Defined names: one scoped to the sheet itself is rewritten or dropped
    // with it, so only the others count.
    let named: Vec<(String, bool)> = out
        .defined_names
        .iter()
        .map(|d| (d.name.clone(), mention.refers(&d.text)))
        .collect();
    if out
        .defined_names
        .iter()
        .any(|d| d.local != Some(w.target) && mention.refers(&d.text))
    {
        hit(out, None);
    }
    // The tables on the sheet, for a pivot cache sourced by table name.
    let mut tables: Vec<String> = Vec::new();
    for part in &target_reach {
        if crate::docx_facts::kind_of(part).as_deref() == Some("xl/tables/table")
            && let Some(xml) = entry(zip, part, w.small)?
        {
            tables.extend(table_names(&xml));
        }
    }
    let sheet = w.names[w.target].to_lowercase();
    let candidates: Vec<String> = zip
        .file_names()
        .filter_map(std::result::Result::ok)
        .filter(|n| scannable(n))
        .map(std::borrow::Cow::into_owned)
        .collect();
    for name in candidates {
        let cap = if name.starts_with("xl/worksheets/") {
            w.large
        } else {
            w.small
        };
        let Some(xml) = entry(zip, &name, cap)? else {
            continue;
        };
        let mut found = scan_mentions(&xml, &mention);
        if name.starts_with("xl/pivotCache/pivotCacheDefinition") {
            found |= pivot_sources(&xml, &sheet, &tables, &named);
        }
        if found {
            hit(out, Some(&name));
        }
    }
    Ok(())
}

/// A part the mention scan reads: workbook XML other than the value tables.
fn scannable(name: &str) -> bool {
    name.starts_with("xl/")
        && crate::defined_names::ext_is(name, "xml")
        && !name.contains("/_rels/")
        && !name.starts_with("xl/theme/")
        && !matches!(
            name,
            "xl/sharedStrings.xml" | "xl/styles.xml" | "xl/calcChain.xml"
        )
}

/// The sheets the workbook's formulas can name, and the one a call asks about.
struct Mention<'a> {
    names: &'a [String],
    target: usize,
}

impl Mention<'_> {
    /// Whether `text` (a formula, a defined name's value, a hyperlink
    /// location, any attribute) refers to the target sheet: `Name!A1`,
    /// `'Name'!A1` with a doubled quote for an apostrophe, or a 3-D range
    /// `A:C!A1` that spans it by position. Names compare ignoring case.
    fn refers(&self, text: &str) -> bool {
        let chars: Vec<char> = text.chars().collect();
        let index_of = |name: &str| {
            self.names
                .iter()
                .position(|n| n.to_lowercase() == name.to_lowercase())
        };
        crate::sheetrefs::prefixes(&chars).iter().any(|p| {
            let parts: Vec<&str> = p.token.split(':').collect();
            match parts[..] {
                [one] => index_of(one) == Some(self.target),
                [a, b] => match (index_of(a), index_of(b)) {
                    (Some(x), Some(y)) => (x.min(y)..=x.max(y)).contains(&self.target),
                    (x, y) => x == Some(self.target) || y == Some(self.target),
                },
                _ => false,
            }
        })
    }
}

/// Whether any text node or attribute value of `xml` refers to the sheet. Cell
/// values (`v`, inline strings) and defined names (handled by scope) are not
/// read. Text is gathered across entity references, so `&apos;My Sheet&apos;!A1`
/// is one token.
fn scan_mentions(xml: &[u8], m: &Mention<'_>) -> bool {
    let mut r = Reader::from_reader(xml);
    let mut skip = 0usize;
    let mut buf = String::new();
    let flush = |buf: &mut String| -> bool {
        let hit = buf.contains('!') && m.refers(buf);
        buf.clear();
        hit
    };
    let skipped = |name: &str| matches!(name, "v" | "is" | "definedName");
    loop {
        match r.read_event() {
            Ok(Event::Start(e)) => {
                if flush(&mut buf) {
                    return true;
                }
                if skip > 0 || skipped(e.local_name().as_ref()) {
                    skip += 1;
                } else if attrs_mention(&e, m) {
                    return true;
                }
            }
            Ok(Event::Empty(e)) => {
                if flush(&mut buf) {
                    return true;
                }
                if skip == 0 && !skipped(e.local_name().as_ref()) && attrs_mention(&e, m) {
                    return true;
                }
            }
            Ok(Event::End(_)) => {
                if flush(&mut buf) {
                    return true;
                }
                skip = skip.saturating_sub(1);
            }
            Ok(Event::Text(t)) if skip == 0 => {
                let s: &str = &t;
                buf.push_str(s);
            }
            Ok(Event::GeneralRef(t)) if skip == 0 => {
                if let Some(c) = crate::docx_facts::entity(&t) {
                    buf.push(c);
                }
            }
            Ok(Event::Eof) | Err(_) => return flush(&mut buf),
            Ok(_) => {}
        }
    }
}

fn attrs_mention(e: &quick_xml::events::BytesStart<'_>, m: &Mention<'_>) -> bool {
    e.attributes().flatten().any(|a| {
        a.value.contains('!') && {
            let raw = a.value.clone();
            let text = quick_xml::escape::unescape(&raw).unwrap_or_else(|_| raw.clone());
            m.refers(&text)
        }
    })
}

fn table_names(xml: &[u8]) -> Vec<String> {
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Start(e) | Event::Empty(e)) => {
                return ["name", "displayName"]
                    .iter()
                    .filter_map(|k| attr(&e, k))
                    .map(|n| n.to_lowercase())
                    .collect();
            }
            Ok(Event::Eof) | Err(_) => return Vec::new(),
            Ok(_) => {}
        }
    }
}

/// Whether a pivot cache is sourced from the sheet: by `sheet=` (its name), or
/// by `name=` when that is a table on the sheet or a defined name that refers
/// to it.
fn pivot_sources(xml: &[u8], sheet: &str, tables: &[String], named: &[(String, bool)]) -> bool {
    let mut r = Reader::from_reader(xml);
    loop {
        match r.read_event() {
            Ok(Event::Empty(e) | Event::Start(e))
                if e.local_name().as_ref() == "worksheetSource" =>
            {
                if attr(&e, "sheet").is_some_and(|s| s.to_lowercase() == sheet) {
                    return true;
                }
                if let Some(n) = attr(&e, "name") {
                    let n = n.to_lowercase();
                    if tables.contains(&n)
                        || named
                            .iter()
                            .any(|(dn, mentions)| *mentions && dn.to_lowercase() == n)
                    {
                        return true;
                    }
                }
            }
            Ok(Event::Eof) | Err(_) => return false,
            Ok(_) => {}
        }
    }
}

/// Every part reachable from `start` through relationships (itself included).
/// Other sheets (unless `worksheets`), shared workbook parts and external
/// targets are not followed; `skip` parts are never entered.
fn reach(
    zip: &mut Zip<'_>,
    start: &str,
    cap: u64,
    worksheets: bool,
    skip: &BTreeSet<String>,
) -> Result<BTreeSet<String>> {
    let mut seen: BTreeSet<String> = BTreeSet::new();
    let mut queue = vec![start.to_owned()];
    while let Some(part) = queue.pop() {
        if skip.contains(&part) || !seen.insert(part.clone()) || seen.len() > 1000 {
            continue;
        }
        let Some(rels) = entry(zip, &rels_path(&part), cap)? else {
            continue;
        };
        let mut r = Reader::from_reader(rels.as_slice());
        loop {
            match r.read_event() {
                Ok(Event::Empty(e) | Event::Start(e))
                    if e.local_name().as_ref() == "Relationship" =>
                {
                    let kind = attr(&e, "Type").unwrap_or_default();
                    let external = attr(&e, "TargetMode").is_some_and(|m| m == "External");
                    let mut skipped = [
                        "/externalLink",
                        "/theme",
                        "/styles",
                        "/sharedStrings",
                        "/calcChain",
                        "/hyperlink",
                    ]
                    .iter()
                    .any(|s| kind.ends_with(s));
                    if !worksheets {
                        skipped |= kind.ends_with("/worksheet") || kind.ends_with("/chartsheet");
                    }
                    if let Some(t) = attr(&e, "Target")
                        && !external
                        && !skipped
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
}

#[allow(clippy::too_many_lines)]
fn scan_sheet(xml: &[u8], sheet: usize, scan_for: &Scan<'_>) -> SheetScan {
    let (wanted, rects) = (scan_for.wanted, scan_for.rects);
    let mut taken = 0usize;
    let mut scan = SheetScan {
        value_cells: 0,
        used: None,
        cells: Vec::new(),
        shared: Vec::new(),
        drawing: None,
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
                "c" => {
                    in_c = true;
                    coord = attr(&e, "r").and_then(|a| parse_cell_ref(&a));
                    kind = attr(&e, "t").unwrap_or_default();
                    (f, v, is) = (None, None, None);
                }
                "f" if in_c => {
                    into = Target::Formula;
                    f = Some(String::new());
                }
                "v" if in_c => {
                    into = Target::Value;
                    v = Some(String::new());
                }
                "rPh" if in_c => in_phonetic = true,
                "t" if in_c && !in_phonetic => {
                    into = Target::Inline;
                    is.get_or_insert_with(String::new);
                }
                "drawing" => scan.drawing = rel_attr(&e, "id").or(Some(String::new())),
                _ => {}
            },
            Ok(Event::Empty(e)) => match e.local_name().as_ref() {
                "f" if in_c => f = Some(String::new()),
                "drawing" => scan.drawing = rel_attr(&e, "id").or(Some(String::new())),
                _ => {}
            },
            Ok(Event::Text(t)) => push(&mut f, &mut v, &mut is, into, &t),
            Ok(Event::GeneralRef(t)) => {
                if let Some(c) = crate::docx_facts::entity(&t) {
                    push(&mut f, &mut v, &mut is, into, &c.to_string());
                }
            }
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                "f" | "v" | "t" => into = Target::Nowhere,
                "rPh" => in_phonetic = false,
                "c" => {
                    in_c = false;
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
                "si" => {
                    in_si = true;
                    text.clear();
                }
                "rPh" => in_phonetic = true,
                "t" if in_si && !in_phonetic => in_t = true,
                _ => {}
            },
            // An empty string is an `<si/>`: it still takes an index.
            Ok(Event::Empty(e)) if e.local_name().as_ref() == "si" => {
                if need.contains(&index) {
                    out.insert(index, String::new());
                }
                index += 1;
            }
            Ok(Event::Text(t)) if in_t => {
                let s: &str = &t;
                text.push_str(s);
            }
            Ok(Event::GeneralRef(t)) if in_t => {
                if let Some(c) = crate::docx_facts::entity(&t) {
                    text.push(c);
                }
            }
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                "t" => in_t = false,
                "rPh" => in_phonetic = false,
                "si" => {
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
                    "f" => refs.push(String::new()),
                    "barDir" => bar_dir = attr(&e, "val"),
                    n if kind.is_none() && n.ends_with("Chart") && n != "chart" => {
                        let raw = n[..n.len() - 5].to_owned();
                        kind = Some(raw);
                    }
                    _ => {}
                }
            }
            Ok(Event::Start(e)) => {
                let name = e.local_name();
                let name = name.as_ref();
                match name {
                    "plotArea" => seen_plot = true,
                    "title" if !seen_plot => in_title = true,
                    "t" if in_title => in_t = true,
                    "f" => {
                        in_f = true;
                        refs.push(String::new());
                    }
                    "barDir" => bar_dir = attr(&e, "val"),
                    n if kind.is_none() && n.ends_with("Chart") && n != "chart" => {
                        let raw = n[..n.len() - 5].to_owned();
                        kind = Some(raw);
                    }
                    _ => {}
                }
            }
            Ok(Event::Text(t)) => {
                let s: &str = &t;
                if in_t {
                    title.push_str(s);
                    have_title = true;
                } else if in_f && let Some(last) = refs.last_mut() {
                    last.push_str(s);
                }
            }
            Ok(Event::End(e)) => match e.local_name().as_ref() {
                "title" => in_title = false,
                "t" => in_t = false,
                "f" => in_f = false,
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
