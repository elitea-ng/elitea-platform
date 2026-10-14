//! The blocking document operations of the `docx` family: everything that
//! touches a [`Document`]. Each function is synchronous and returns the
//! sentence the model reads on failure, so `tools.rs` runs them on a blocking
//! thread and never maps an `OfficeError` itself. No path appears: bytes in,
//! bytes out (ADR-0032 decision 2).
//!
//! An edit is planned, applied once and checked once:
//!
//! 1. the file's fact sheet is read from the package (no library parse) and
//!    the call is *planned* against it: every argument, index and size is
//!    checked, and the text that must end up in the file is worked out, before
//!    the document is touched;
//! 2. the document is opened (the call's one parse), edited and serialised
//!    (its one serialisation);
//! 3. the saved bytes' fact sheet is read back and compared with the plan *at
//!    the place the edit was aimed* (the new paragraph, the named cell, every
//!    paragraph and cell a replacement matched), and with the file as it was
//!    for anything an edit must not lose. A save that does not satisfy the
//!    plan is refused and nothing is written.

#![allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss,
    reason = "EMU arithmetic on small bounded values"
)]

use std::collections::{BTreeMap, HashSet};

use aho_corasick::{AhoCorasick, MatchKind};
use elitea_office_core::{ChartSpec, DocFacts, Document, Limits, OfficeError, Rect};
use serde_json::{Map, Value, json};

use super::super::office_common::{OfficeKind, table_rows};
pub(super) use super::super::office_common::{
    Sentence, opt_bool, opt_f64, opt_str, opt_uint, req_str,
};
use super::schema::{CHART_TYPES, DocxTool};

/// What this family's sentences call the file.
pub(super) const KIND: OfficeKind = OfficeKind {
    family: "docx",
    label: "Word (.docx)",
    noun: "document",
    accepts: "a .docx or an image",
};

/// The most replacement keys one call takes (each is two passes in the
/// document library).
pub(super) const MAX_REPLACEMENTS: usize = 100;

/// The text that marks a placeholder, as the SDK's `pptx` toolkit tests it
/// (`"{{" in text or "[PLACEHOLDER]" in text`).
pub(super) fn is_placeholder_text(text: &str) -> bool {
    text.contains("{{") || text.contains("[PLACEHOLDER]")
}

/// An [`OfficeError`] as a sentence the model can act on.
pub(super) fn office_sentence(error: &OfficeError) -> Sentence {
    super::super::office_common::office_sentence(error, &KIND)
}

#[allow(clippy::needless_pass_by_value, reason = "used as a map_err function")]
fn oe(error: OfficeError) -> Sentence {
    office_sentence(&error)
}

pub(crate) fn open(bytes: &[u8], limits: &Limits) -> Result<Document, Sentence> {
    Document::open(bytes, limits).map_err(oe)
}

/// The fact sheet of a package, read without the document library.
pub(crate) fn facts(bytes: &[u8], limits: &Limits) -> Result<DocFacts, Sentence> {
    elitea_office_core::facts_of_docx_package(bytes, limits).map_err(oe)
}

// ---------------------------------------------------------------------------
// Text
// ---------------------------------------------------------------------------

/// `text` with every line ending a `\n`.
fn norm(text: &str) -> String {
    text.replace("\r\n", "\n").replace('\r', "\n")
}

/// Refuse a control character the file format cannot hold (a line break and a
/// tab are fine).
fn check_text(what: &str, text: &str) -> Result<(), Sentence> {
    if text
        .chars()
        .any(|c| c.is_control() && !matches!(c, '\n' | '\r' | '\t'))
    {
        return Err(format!(
            "{what} contains a control character, which a Word document cannot hold."
        ));
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

/// What an authoring call will do and what the saved file must then hold.
pub(crate) enum Plan {
    Heading {
        text: String,
        level: u8,
    },
    Paragraph {
        text: String,
    },
    SetParagraph {
        index: usize,
        text: String,
    },
    Table {
        rows: Vec<Vec<String>>,
    },
    Cell {
        table: usize,
        row: usize,
        col: usize,
        text: String,
    },
    Image {
        data: Vec<u8>,
        size: Rect,
    },
    Chart {
        spec: ChartSpec,
        size: Rect,
    },
    Replace(ReplacePlan),
}

/// A replacement worked out on the file as it is.
pub(crate) struct ReplacePlan {
    pairs: Vec<(String, String)>,
    /// The text every body paragraph must have afterwards.
    paragraphs: Vec<String>,
    /// The text every table cell must have afterwards, table by table.
    tables: Vec<Vec<Vec<String>>>,
}

const EMU_PER_INCH: f64 = 914_400.0;

fn inches(value: f64) -> i64 {
    (value * EMU_PER_INCH).round() as i64
}

/// The box a picture takes: width and/or height in inches, kept in the
/// picture's proportions.
fn image_box(args: &Value, iw: u64, ih: u64) -> Rect {
    let ratio = ih as f64 / iw.max(1) as f64;
    let (w, h) = match (opt_f64(args, "width"), opt_f64(args, "height")) {
        (Some(w), Some(h)) => {
            let scale = (w / iw.max(1) as f64).min(h / ih.max(1) as f64);
            (iw as f64 * scale, ih as f64 * scale)
        }
        (Some(w), None) => (w, w * ratio),
        (None, Some(h)) => (h / ratio.max(f64::MIN_POSITIVE), h),
        (None, None) => {
            let w = 6.0_f64.min(9.0 / ratio.max(f64::MIN_POSITIVE));
            (w, w * ratio)
        }
    };
    Rect {
        x: 0,
        y: 0,
        w: inches(w).max(1),
        h: inches(h).max(1),
    }
}

/// 1-based argument as a 0-based index below `count`.
fn index_arg(
    args: &Value,
    key: &str,
    count: usize,
    what: &str,
    default: Option<u64>,
) -> Result<usize, Sentence> {
    let n = opt_uint(args, key)
        .or(default)
        .ok_or_else(|| format!("`{key}` is required: a {what} number from 1."))?;
    if n == 0 || n > count as u64 {
        return Err(format!(
            "{} {n} does not exist: the document has {count} {what}{}.",
            capitalise(what),
            if count == 1 { "" } else { "s" }
        ));
    }
    Ok(usize::try_from(n - 1).unwrap_or(0))
}

fn capitalise(word: &str) -> String {
    let mut c = word.chars();
    c.next().map_or_else(String::new, |f| {
        f.to_uppercase().collect::<String>() + c.as_str()
    })
}

/// Work out the call on the file as it is. Nothing is edited here: every
/// refusal that can be decided from the arguments and the file is decided now.
pub(crate) fn plan(
    tool: DocxTool,
    args: &Value,
    before: &DocFacts,
    image: Option<&[u8]>,
) -> Result<Plan, Sentence> {
    match tool {
        DocxTool::AddHeading => {
            let text = req_str(args, "text")?;
            check_text("The heading", &text)?;
            let level = opt_uint(args, "level").unwrap_or(1);
            if !(1..=9).contains(&level) {
                return Err("`level` must be 1 to 9.".to_owned());
            }
            Ok(Plan::Heading {
                text: norm(&text),
                level: u8::try_from(level).unwrap_or(1),
            })
        }
        DocxTool::AddParagraph => {
            let text = req_str(args, "text")?;
            check_text("The paragraph", &text)?;
            Ok(Plan::Paragraph { text: norm(&text) })
        }
        DocxTool::SetParagraphText => {
            let paragraphs = before.paragraphs();
            let index = index_arg(args, "paragraph", paragraphs.len(), "paragraph", None)?;
            // An empty text is a valid replacement: it empties the paragraph.
            let text = args
                .get("text")
                .and_then(Value::as_str)
                .ok_or_else(|| "`text` is required.".to_owned())?;
            check_text("The text", text)?;
            let target = paragraphs[index];
            if target.pictures + target.charts > 0 {
                return Err(format!(
                    "Paragraph {} holds a picture or a chart, and setting its text would remove it. Edit a paragraph without one.",
                    index + 1
                ));
            }
            Ok(Plan::SetParagraph {
                index,
                text: norm(text),
            })
        }
        DocxTool::AddTable => {
            let rows = table_rows(args, 200, 30)?;
            for row in &rows {
                for cell in row {
                    check_text("A cell", cell)?;
                }
            }
            Ok(Plan::Table {
                rows: rows
                    .iter()
                    .map(|r| r.iter().map(|c| norm(c)).collect())
                    .collect(),
            })
        }
        DocxTool::SetTableCell => {
            let tables = before.tables();
            let table = index_arg(args, "table", tables.len(), "table", Some(1))?;
            let rows = &tables[table].rows;
            let row = index_arg(args, "row", rows.len(), "row", None)?;
            let width = rows.get(row).map_or(0, Vec::len);
            let col = index_arg(args, "column", width, "column", None)?;
            let text = args
                .get("text")
                .and_then(Value::as_str)
                .ok_or_else(|| "`text` is required.".to_owned())?;
            check_text("The cell", text)?;
            Ok(Plan::Cell {
                table,
                row,
                col,
                text: norm(text),
            })
        }
        DocxTool::AddImage => {
            let data = image.ok_or_else(|| "The picture could not be read.".to_owned())?;
            let (iw, ih) = elitea_office_core::image_size(data).ok_or_else(|| {
                "The picture is not a PNG, JPEG or GIF image, or its header is damaged.".to_owned()
            })?;
            Ok(Plan::Image {
                data: data.to_vec(),
                size: image_box(args, iw, ih),
            })
        }
        DocxTool::AddChart => {
            let spec = super::super::office_common::chart_spec(args, CHART_TYPES)?;
            let w = opt_f64(args, "width").unwrap_or(6.0);
            let h = opt_f64(args, "height").unwrap_or(3.5);
            Ok(Plan::Chart {
                spec,
                size: Rect {
                    x: 0,
                    y: 0,
                    w: inches(w).max(1),
                    h: inches(h).max(1),
                },
            })
        }
        DocxTool::ReplacePlaceholders => plan_replace(args, before).map(Plan::Replace),
        _ => Err("This tool does not edit the document.".to_owned()),
    }
}

/// The replacements of a call, and the text each paragraph and cell must hold
/// afterwards. The keys are matched in ONE pass over each text, the longest
/// key at each position winning (the same rule `Document::replace_all` follows).
fn plan_replace(args: &Value, before: &DocFacts) -> Result<ReplacePlan, Sentence> {
    let replacements: BTreeMap<String, String> = args
        .get("replacements")
        .and_then(Value::as_object)
        .ok_or_else(|| "`replacements` is required: a map from text to replacement.".to_owned())?
        .iter()
        .map(|(k, v)| {
            (
                k.clone(),
                norm(&v.as_str().map_or_else(|| v.to_string(), str::to_owned)),
            )
        })
        .collect();
    if replacements.is_empty() || replacements.keys().any(String::is_empty) {
        return Err("`replacements` needs at least one non-empty key.".to_owned());
    }
    if replacements.len() > MAX_REPLACEMENTS {
        return Err(format!(
            "Too many replacements in one call ({MAX_REPLACEMENTS} at most)."
        ));
    }
    for (key, value) in &replacements {
        if key.contains(['\n', '\r', '\t']) {
            return Err(format!(
                "The key `{}` contains a line break or a tab. A key is matched within one paragraph's text; use a shorter key.",
                key.escape_debug()
            ));
        }
        check_text("A replacement", value)?;
        check_text("A key", key)?;
    }
    let pairs: Vec<(String, String)> = replacements.into_iter().collect();
    let keys: Vec<&str> = pairs.iter().map(|(k, _)| k.as_str()).collect();
    let values: Vec<&str> = pairs.iter().map(|(_, v)| v.as_str()).collect();
    let finder = AhoCorasick::builder()
        .match_kind(MatchKind::LeftmostLongest)
        .build(&keys)
        .map_err(|_| "The replacement keys could not be used.".to_owned())?;
    let sub = |text: &str| -> String {
        if finder.find(text).is_none() {
            return text.to_owned();
        }
        let mut out = String::with_capacity(text.len());
        let mut last = 0;
        for m in finder.find_iter(text) {
            out.push_str(&text[last..m.start()]);
            out.push_str(values[m.pattern().as_usize()]);
            last = m.end();
        }
        out.push_str(&text[last..]);
        out
    };
    let paragraphs = before.paragraphs().iter().map(|p| sub(&p.text)).collect();
    let tables = before
        .tables()
        .iter()
        .map(|t| {
            t.rows
                .iter()
                .map(|r| r.iter().map(|c| sub(c)).collect())
                .collect()
        })
        .collect();
    Ok(ReplacePlan {
        pairs,
        paragraphs,
        tables,
    })
}

// ---------------------------------------------------------------------------
// Apply, save, check
// ---------------------------------------------------------------------------

/// The result of an applied plan, for the message.
pub(crate) struct Applied {
    pub(crate) message: String,
    /// Per key of a replacement, how many times it matched.
    pub(crate) counts: Option<Vec<(String, usize)>>,
}

fn apply(doc: &mut Document, plan: &Plan) -> Result<Option<Vec<usize>>, Sentence> {
    match plan {
        Plan::Heading { text, level } => doc.add_heading(text, *level).map_err(oe)?,
        Plan::Paragraph { text } => doc.add_paragraph(text).map_err(oe)?,
        Plan::SetParagraph { index, text } => doc.set_paragraph_text(*index, text).map_err(oe)?,
        Plan::Table { rows } => {
            let table = doc
                .add_table(rows.len(), rows.first().map_or(0, Vec::len))
                .map_err(oe)?;
            for (r, row) in rows.iter().enumerate() {
                for (c, cell) in row.iter().enumerate() {
                    if !cell.is_empty() {
                        doc.set_table_cell(table, r, c, cell).map_err(oe)?;
                    }
                }
            }
            doc.style_table_header(table).map_err(oe)?;
        }
        Plan::Cell {
            table,
            row,
            col,
            text,
        } => doc.set_table_cell(*table, *row, *col, text).map_err(oe)?,
        Plan::Image { data, size } => doc.add_image(data, *size).map_err(oe)?,
        Plan::Chart { spec, size } => doc.add_chart(spec, *size).map_err(oe)?,
        Plan::Replace(r) => return doc.replace_all(&r.pairs).map(Some).map_err(oe),
    }
    Ok(None)
}

/// Why a save was refused, in the model's words.
fn unplanned(what: &str) -> Sentence {
    format!(
        "Nothing was saved: after the edit {what}, which is not what was asked. The document library did not carry the edit through as planned."
    )
}

/// Compare the saved file with the file as it was and with the plan.
#[allow(clippy::too_many_lines)]
pub(crate) fn verify(before: &DocFacts, after: &DocFacts, plan: &Plan) -> Result<(), Sentence> {
    // Nothing the file held may be gone: no paragraph or table fewer than
    // before, no picture or chart fewer, no part of a kind fewer.
    let (bp, ap) = (before.paragraphs(), after.paragraphs());
    let (bt, at) = (before.tables(), after.tables());
    let lost = |b: usize, a: usize, what: &str| -> Result<(), Sentence> {
        if a < b {
            tracing::warn!(
                event = "docx_save_would_lose_content",
                what,
                before = b,
                after = a,
                "a save would have dropped content; refused"
            );
            return Err(format!(
                "Nothing was saved: this edit would have removed {} {what}{} from the file.",
                b - a,
                if b - a == 1 { "" } else { "s" }
            ));
        }
        Ok(())
    };
    lost(bp.len(), ap.len(), "paragraph")?;
    lost(bt.len(), at.len(), "table")?;
    lost(before.pictures, after.pictures, "picture")?;
    lost(before.charts, after.charts, "chart")?;
    for (kind, n) in &before.parts {
        let now = after.parts.get(kind).copied().unwrap_or(0);
        lost(*n, now, &format!("`{kind}` part"))?;
    }
    // Everything before the target is as it was.
    let same_paragraphs = |upto: usize| -> Result<(), Sentence> {
        for i in 0..upto.min(bp.len()) {
            if bp[i].text != ap[i].text || bp[i].style != ap[i].style {
                return Err(unplanned(&format!("paragraph {} had changed", i + 1)));
            }
        }
        Ok(())
    };
    let same_tables = |upto: usize| -> Result<(), Sentence> {
        for i in 0..upto.min(bt.len()) {
            if bt[i].rows != at[i].rows {
                return Err(unplanned(&format!("table {} had changed", i + 1)));
            }
        }
        Ok(())
    };
    match plan {
        Plan::Heading { text, level } => {
            let n = bp.len();
            if ap.len() != n + 1 || at.len() != bt.len() {
                return Err(unplanned("the heading was not added as one new paragraph"));
            }
            same_paragraphs(n)?;
            same_tables(bt.len())?;
            let new = ap[n];
            if new.text != *text || new.style.as_deref() != Some(&format!("Heading{level}")) {
                return Err(unplanned(&format!(
                    "the new paragraph {} does not hold the heading",
                    n + 1
                )));
            }
        }
        Plan::Paragraph { text } => {
            let n = bp.len();
            if ap.len() != n + 1 || at.len() != bt.len() {
                return Err(unplanned(
                    "the paragraph was not added as one new paragraph",
                ));
            }
            same_paragraphs(n)?;
            same_tables(bt.len())?;
            if ap[n].text != *text {
                return Err(unplanned(&format!(
                    "the new paragraph {} does not hold the text",
                    n + 1
                )));
            }
        }
        Plan::SetParagraph { index, text } => {
            if ap.len() != bp.len() || at.len() != bt.len() {
                return Err(unplanned("the number of paragraphs or tables had changed"));
            }
            for i in 0..bp.len() {
                if i == *index {
                    continue;
                }
                if bp[i].text != ap[i].text || bp[i].style != ap[i].style {
                    return Err(unplanned(&format!("paragraph {} had changed", i + 1)));
                }
            }
            same_tables(bt.len())?;
            if ap[*index].text != *text {
                return Err(unplanned(&format!(
                    "paragraph {} does not hold the new text",
                    index + 1
                )));
            }
        }
        Plan::Table { rows } => {
            let n = bt.len();
            if at.len() != n + 1 || ap.len() != bp.len() {
                return Err(unplanned("the table was not added as one new table"));
            }
            same_tables(n)?;
            same_paragraphs(bp.len())?;
            if at[n].rows != *rows {
                return Err(unplanned(&format!(
                    "the new table {} does not hold the cell text",
                    n + 1
                )));
            }
        }
        Plan::Cell {
            table,
            row,
            col,
            text,
        } => {
            if at.len() != bt.len() || ap.len() != bp.len() {
                return Err(unplanned("the number of paragraphs or tables had changed"));
            }
            for i in 0..bt.len() {
                if i == *table {
                    continue;
                }
                if bt[i].rows != at[i].rows {
                    return Err(unplanned(&format!("table {} had changed", i + 1)));
                }
            }
            same_paragraphs(bp.len())?;
            let mut want = bt[*table].rows.clone();
            want[*row][*col].clone_from(text);
            if at[*table].rows != want {
                return Err(unplanned(&format!(
                    "row {} column {} of table {} does not hold the new text, or another cell changed",
                    row + 1,
                    col + 1,
                    table + 1
                )));
            }
        }
        Plan::Image { .. } | Plan::Chart { .. } => {
            let chart = matches!(plan, Plan::Chart { .. });
            let n = bp.len();
            same_paragraphs(n)?;
            same_tables(bt.len())?;
            let (b, a) = if chart {
                (before.charts, after.charts)
            } else {
                (before.pictures, after.pictures)
            };
            // The new object sits in a paragraph after the old ones.
            let placed: usize = ap[n.min(ap.len())..]
                .iter()
                .map(|p| if chart { p.charts } else { p.pictures })
                .sum();
            if a != b + 1 || placed != 1 {
                return Err(unplanned(if chart {
                    "the chart is not in the file as one new chart at the end"
                } else {
                    "the picture is not in the file as one new picture at the end"
                }));
            }
        }
        Plan::Replace(r) => {
            if ap.len() != bp.len() || at.len() != bt.len() {
                return Err(unplanned("the number of paragraphs or tables had changed"));
            }
            for (i, want) in r.paragraphs.iter().enumerate() {
                if ap[i].text != *want || ap[i].style != bp[i].style {
                    return Err(unplanned(&format!(
                        "paragraph {} does not hold the planned text (a text run split by a tab or a line break?)",
                        i + 1
                    )));
                }
            }
            for (t, rows) in r.tables.iter().enumerate() {
                if at[t].rows != *rows {
                    return Err(unplanned(&format!(
                        "a cell of table {} does not hold the planned text (a text run split by a tab or a line break?)",
                        t + 1
                    )));
                }
            }
        }
    }
    Ok(())
}

/// A message for what the edit did, from the saved file.
fn message(plan: &Plan, after: &DocFacts) -> String {
    match plan {
        Plan::Heading { level, .. } => format!(
            "Added a level {level} heading as paragraph {}.",
            after.paragraphs().len()
        ),
        Plan::Paragraph { .. } => format!("Added paragraph {}.", after.paragraphs().len()),
        Plan::SetParagraph { index, .. } => format!("Set the text of paragraph {}.", index + 1),
        Plan::Table { rows } => format!(
            "Added table {} ({} rows by {} columns).",
            after.tables().len(),
            rows.len(),
            rows.first().map_or(0, Vec::len)
        ),
        Plan::Cell {
            table, row, col, ..
        } => format!(
            "Set row {} column {} of table {}.",
            row + 1,
            col + 1,
            table + 1
        ),
        Plan::Image { .. } => format!(
            "Added a picture after paragraph {}.",
            after.paragraphs().len().saturating_sub(1)
        ),
        Plan::Chart { .. } => format!(
            "Added a chart after paragraph {}.",
            after.paragraphs().len().saturating_sub(1)
        ),
        Plan::Replace(_) => String::new(),
    }
}

/// An authoring call end to end on a blocking thread: plan against the file,
/// open (the one parse), edit, save (the one serialisation), check the saved
/// bytes, and return the message and the bytes to store.
pub(crate) fn run_edit(
    tool: DocxTool,
    args: &Value,
    bytes: &[u8],
    image: Option<&[u8]>,
    limits: &Limits,
) -> Result<(Applied, Vec<u8>), Sentence> {
    let before = facts(bytes, limits)?;
    let plan = plan(tool, args, &before, image)?;
    let mut doc = open(bytes, limits)?;
    let counts = apply(&mut doc, &plan)?;
    let saved = doc.save().map_err(oe)?;
    let after = facts(&saved, limits)?;
    verify(&before, &after, &plan)?;
    let counts = match (&plan, counts) {
        (Plan::Replace(r), Some(counts)) => Some(
            r.pairs
                .iter()
                .map(|(k, _)| k.clone())
                .zip(counts)
                .collect::<Vec<_>>(),
        ),
        _ => None,
    };
    Ok((
        Applied {
            message: message(&plan, &after),
            counts,
        },
        saved,
    ))
}

/// A new document, with a title paragraph when asked, checked on the saved
/// bytes.
pub(crate) fn create(title: Option<&str>, limits: &Limits) -> Result<Vec<u8>, Sentence> {
    let mut doc = Document::new(limits);
    if let Some(title) = title {
        check_text("The title", title)?;
        doc.add_heading(&norm(title), 1).map_err(oe)?;
    }
    let saved = doc.save().map_err(oe)?;
    if let Some(title) = title {
        let after = facts(&saved, limits)?;
        let first = after
            .paragraphs()
            .first()
            .map(|p| (p.text.clone(), p.style.clone()));
        if first != Some((norm(title), Some("Heading1".to_owned()))) {
            return Err(unplanned("the title is not the first paragraph"));
        }
    }
    Ok(saved)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

/// `list_placeholders`: the body paragraphs and table cells as JSON.
pub(crate) fn list_report(facts: &DocFacts, include_all: bool) -> Value {
    let mut rows: Vec<Value> = Vec::new();
    let mut texts: HashSet<&str> = HashSet::new();
    let keep = |text: &str| {
        if include_all {
            !text.trim().is_empty()
        } else {
            is_placeholder_text(text)
        }
    };
    for (i, p) in facts.paragraphs().iter().enumerate() {
        if keep(&p.text) {
            texts.insert(&p.text);
            let mut row = Map::new();
            row.insert("location".into(), "paragraph".into());
            row.insert("paragraph".into(), Value::from(i + 1));
            if let Some(style) = &p.style {
                row.insert("style".into(), style.clone().into());
            }
            row.insert("text".into(), p.text.clone().into());
            rows.push(Value::Object(row));
        }
    }
    for (t, table) in facts.tables().iter().enumerate() {
        for (r, cells) in table.rows.iter().enumerate() {
            for (c, text) in cells.iter().enumerate() {
                if keep(text) {
                    texts.insert(text);
                    rows.push(json!({
                        "location": "table_cell",
                        "table": t + 1,
                        "row": r + 1,
                        "column": c + 1,
                        "text": text,
                    }));
                }
            }
        }
    }
    json!({
        "total_locations": rows.len(),
        "unique_texts": texts.len(),
        "placeholders": rows,
        "note": "Body paragraphs and table cells are listed. Headers, footers and text boxes are not listed, but replace_placeholders does replace in them.",
    })
}

/// A replacement call's report from what the edit did.
pub(super) fn replace_report(applied: &Applied) -> (usize, Vec<String>) {
    let counts = applied.counts.as_deref().unwrap_or_default();
    (
        counts.iter().map(|(_, n)| n).sum(),
        counts
            .iter()
            .filter(|(_, n)| *n == 0)
            .map(|(k, _)| k.clone())
            .collect(),
    )
}
