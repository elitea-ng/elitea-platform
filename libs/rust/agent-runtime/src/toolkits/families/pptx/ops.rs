//! The blocking document operations of the `pptx` family: everything that
//! touches a [`Deck`]. Each function is synchronous and returns the sentence
//! the model reads on failure, so `tools.rs` runs them on a blocking thread
//! and never maps an `OfficeError` itself. No path appears: bytes in, bytes
//! out (ADR-0032 decision 2).

#![allow(
    clippy::cast_precision_loss,
    clippy::cast_possible_truncation,
    clippy::cast_sign_loss,
    reason = "EMU and point arithmetic on small bounded values"
)]

use std::collections::{BTreeMap, HashSet};

use aho_corasick::{AhoCorasick, MatchKind};

use elitea_office_core::{
    Bullet, ChartKind, ChartSpec, Deck, OfficeError, ParagraphInfo, Rect, Series, ShapeInfo,
    ShapeKind, SlideFacts, SlideLayout, Theme,
};
use serde_json::{Map, Value};

use super::schema::PptxTool;

/// A failure as the model reads it.
pub(super) type Sentence = String;

/// The text that marks a placeholder, as the SDK tests it
/// (`"{{" in text or "[PLACEHOLDER]" in text`).
pub(super) fn is_placeholder_text(text: &str) -> bool {
    text.contains("{{") || text.contains("[PLACEHOLDER]")
}

/// The sentence for a busy engine; the caller retries on exactly this.
pub(super) const BUSY: &str = "The document engine is busy with other work. Try again in a moment.";

/// An [`OfficeError`] as a sentence the model can act on. Internal detail
/// stays in the log, not in the sentence.
pub(super) fn office_sentence(error: &OfficeError) -> Sentence {
    match error {
        OfficeError::LimitExceeded { limit, value } => format!(
            "The document is over a safety limit ({limit}, measured {value}). Use a smaller file or fewer elements."
        ),
        OfficeError::Malformed(_) => {
            "The file is not a valid PowerPoint (.pptx) package, or it is damaged.".to_owned()
        }
        OfficeError::Unsupported(message)
        | OfficeError::NotFound(message)
        | OfficeError::InvalidInput(message) => message.clone(),
        OfficeError::DeadlineExceeded => {
            "Processing the document took too long and was abandoned. Try a smaller deck."
                .to_owned()
        }
        OfficeError::Indeterminate => {
            "An edit timed out and may have half-applied; nothing was saved. Try again.".to_owned()
        }
        OfficeError::Busy => BUSY.to_owned(),
        OfficeError::Internal(_) | _ => {
            tracing::warn!(
                event = "pptx_tool_internal_error",
                "the document engine failed internally"
            );
            "The document engine failed on this file.".to_owned()
        }
    }
}

#[allow(clippy::needless_pass_by_value, reason = "used as a map_err function")]
fn oe(error: OfficeError) -> Sentence {
    office_sentence(&error)
}

pub(super) fn open(bytes: &[u8], limits: &elitea_office_core::Limits) -> Result<Deck, Sentence> {
    Deck::open(bytes, limits).map_err(oe)
}

pub(super) fn save(deck: &Deck) -> Result<Vec<u8>, Sentence> {
    deck.save().map_err(oe)
}

// ---------------------------------------------------------------------------
// Arguments
// ---------------------------------------------------------------------------

pub(super) fn req_str(args: &Value, key: &str) -> Result<String, Sentence> {
    opt_str(args, key).ok_or_else(|| format!("`{key}` is required and must be a non-empty string."))
}

pub(super) fn opt_str(args: &Value, key: &str) -> Option<String> {
    let v = args.get(key)?.as_str()?;
    (!v.trim().is_empty()).then(|| v.to_owned())
}

pub(super) fn opt_bool(args: &Value, key: &str) -> Option<bool> {
    args.get(key)?.as_bool()
}

fn opt_f64(args: &Value, key: &str) -> Option<f64> {
    args.get(key)?.as_f64().filter(|v| v.is_finite())
}

fn opt_uint(args: &Value, key: &str) -> Option<u64> {
    args.get(key)?.as_u64()
}

/// A 1-based slide number as a 0-based index into a deck of `count` slides.
pub(super) fn slide_arg(args: &Value, key: &str, count: usize) -> Result<usize, Sentence> {
    let n = opt_uint(args, key)
        .ok_or_else(|| format!("`{key}` is required: a slide number from 1."))?;
    slide_index(n, count)
}

pub(super) fn slide_index(n: u64, count: usize) -> Result<usize, Sentence> {
    if n == 0 || n > count as u64 {
        return Err(format!(
            "Slide {n} does not exist: the deck has {count} slide{}.",
            if count == 1 { "" } else { "s" }
        ));
    }
    Ok(usize::try_from(n - 1).unwrap_or(0))
}

/// What a file holds that a save must not lose: the drawables, tables, charts
/// and pictures across all slides.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) struct Inventory {
    shapes: usize,
    tables: usize,
    charts: usize,
    pictures: usize,
}

impl Inventory {
    pub(crate) fn of(facts: &[SlideFacts]) -> Self {
        let mut total = Self::default();
        for slide in facts {
            total.shapes += slide.shapes.len();
            total.tables += slide.tables.len();
            total.charts += slide.charts.len();
            total.pictures += slide
                .shapes
                .iter()
                .filter(|s| s.kind == ShapeKind::Picture)
                .count();
        }
        total
    }
}

/// A deck opened from bytes, with what its file held read once: the one fact
/// sheet every step of a call works from, so a call opens the deck once,
/// reads the package without the library, and serialises once (at the save).
pub(crate) struct Opened {
    pub(crate) deck: Deck,
    pub(crate) facts: Vec<SlideFacts>,
    pub(crate) before: Inventory,
}

pub(crate) fn open_with_facts(
    bytes: &[u8],
    limits: &elitea_office_core::Limits,
) -> Result<Opened, Sentence> {
    let deck = open(bytes, limits)?;
    let facts = elitea_office_core::facts_of_package(bytes, limits).map_err(oe)?;
    let before = Inventory::of(&facts);
    Ok(Opened {
        deck,
        facts,
        before,
    })
}

/// What a finished edit must satisfy besides losing nothing.
#[derive(Default)]
pub(crate) struct Check<'a> {
    /// Fewer shapes, tables, charts or pictures is the point of the edit.
    pub(crate) allow_fewer: bool,
    /// What the call must have put in the file.
    pub(crate) expect: Option<&'a Expect>,
    /// The slide count the saved file must have.
    pub(crate) slides: Option<usize>,
}

/// Save the deck (the call's one serialisation), read the saved package back
/// (no second library parse) and refuse the save if it holds less than the
/// file did, lacks what the call added, or has the wrong number of slides.
///
/// The slide library has save paths that silently drop what it did not model
/// when the file was opened from bytes: adding speaker notes to a slide that
/// has none, or replacing the theme, makes it rewrite the whole file from its
/// build model, and tables, charts and pictures read from the file are not in
/// that model. Rather than ship a deck that lost them, the edit is refused
/// and nothing is written.
pub(crate) fn finish_edit(
    opened: &Opened,
    limits: &elitea_office_core::Limits,
    check: &Check<'_>,
) -> Result<Vec<u8>, Sentence> {
    let saved = save(&opened.deck)?;
    let after_facts = elitea_office_core::facts_of_package(&saved, limits).map_err(oe)?;
    if let Some(expected) = check.slides
        && after_facts.len() != expected
    {
        return Err("This file has no slide list, so a slide cannot be added to it. Create a new deck with create_presentation and copy the content over.".to_owned());
    }
    let after = Inventory::of(&after_facts);
    if !check.allow_fewer {
        loss_check(&opened.before, &after)?;
    }
    if let Some(expect) = check.expect {
        check_expect(&opened.facts, &after_facts, expect)?;
    }
    Ok(saved)
}

fn loss_check(before: &Inventory, after: &Inventory) -> Result<(), Sentence> {
    let lost = |a: usize, b: usize| b.saturating_sub(a);
    let (shapes, tables, charts, pictures) = (
        lost(after.shapes, before.shapes),
        lost(after.tables, before.tables),
        lost(after.charts, before.charts),
        lost(after.pictures, before.pictures),
    );
    if shapes + tables + charts + pictures == 0 {
        return Ok(());
    }
    tracing::warn!(
        event = "pptx_save_would_lose_content",
        shapes,
        tables,
        charts,
        pictures,
        "a save would have dropped content; refused"
    );
    Err(format!(
        "Nothing was saved: this edit would have removed {} from the file (the document library rewrites the whole file when speaker notes are first added to a slide or the theme is replaced, and cannot carry tables, charts or pictures through that). Make this change in PowerPoint, or apply it to a copy of the deck before adding those elements.",
        [
            (tables, "table"),
            (charts, "chart"),
            (pictures, "picture"),
            (
                shapes.saturating_sub(tables + charts + pictures),
                "other shape"
            ),
        ]
        .iter()
        .filter(|(n, _)| *n > 0)
        .map(|(n, what)| format!("{n} {what}{}", if *n == 1 { "" } else { "s" }))
        .collect::<Vec<_>>()
        .join(", ")
    ))
}

/// What an authoring call must have put in the file, and where: the check that
/// a content-adding edit was not a silent no-op in the library (an
/// `add_bullets` on a slide with no content placeholder reports success and
/// adds nothing). It is judged on the TARGET slide: the same words elsewhere
/// in the deck prove nothing.
#[derive(Debug, Default)]
pub(crate) struct Expect {
    text: Vec<String>,
    tables: usize,
    charts: usize,
    pictures: usize,
    /// The slide (0-based) the call acts on; `None` for a call with no target.
    slide: Option<usize>,
    /// The text shape the call writes into, where the call names one.
    shape: Option<ShapeSelector>,
    /// The text must be NEW on the slide (a slide or a text box is added);
    /// otherwise it replaces what was there, and being present is the proof.
    newly: bool,
    /// The target slide is one the call adds: there is no "before" for it.
    fresh: bool,
}

#[derive(Clone, Debug)]
enum ShapeSelector {
    /// The 0-based index among the slide's text shapes.
    Index(usize),
    Name(String),
}

/// The text a call put on a slide, and the objects it added. `slides_before`
/// is the deck's slide count before the call (the target of `add_slide`).
pub(crate) fn expected_gain(tool: PptxTool, args: &Value, slides_before: usize) -> Expect {
    let mut e = Expect::default();
    let text_of = |v: &Value| -> Option<String> {
        match v {
            Value::String(s) => Some(s.clone()),
            Value::Object(o) => o.get("text").and_then(Value::as_str).map(str::to_owned),
            _ => None,
        }
    };
    let slide_arg = || {
        opt_uint(args, "slide")
            .and_then(|n| n.checked_sub(1))
            .and_then(|n| usize::try_from(n).ok())
    };
    match tool {
        PptxTool::AddSlide => {
            e.text.extend(opt_str(args, "title"));
            if let Some(items) = args.get("bullets").and_then(Value::as_array) {
                e.text.extend(items.iter().filter_map(text_of));
            }
            e.text.extend(opt_str(args, "notes"));
            e.slide = Some(
                opt_uint(args, "position")
                    .and_then(|p| p.checked_sub(1))
                    .and_then(|p| usize::try_from(p).ok())
                    .unwrap_or(slides_before),
            );
            e.newly = true;
            e.fresh = true;
        }
        PptxTool::SetTitle => {
            e.text.extend(opt_str(args, "title"));
            e.slide = slide_arg();
        }
        PptxTool::SetText => {
            e.text.extend(opt_str(args, "text"));
            e.slide = slide_arg();
            match args.get("shape") {
                Some(Value::Number(n)) => {
                    e.shape = n
                        .as_u64()
                        .and_then(|n| n.checked_sub(1))
                        .and_then(|n| usize::try_from(n).ok())
                        .map(ShapeSelector::Index);
                }
                Some(Value::String(name)) => e.shape = Some(ShapeSelector::Name(name.clone())),
                // No shape named: a new text box is added.
                _ => e.newly = true,
            }
        }
        PptxTool::SetNotes => {
            e.text.extend(opt_str(args, "notes"));
            e.slide = slide_arg();
        }
        PptxTool::AddBullets => {
            if let Some(items) = args.get("items").and_then(Value::as_array) {
                e.text.extend(items.iter().filter_map(text_of));
            }
            e.slide = slide_arg();
        }
        PptxTool::AddTable => {
            e.tables = 1;
            e.slide = slide_arg();
        }
        PptxTool::AddChart => {
            e.charts = 1;
            e.slide = slide_arg();
        }
        PptxTool::AddImage => {
            e.pictures = 1;
            e.slide = slide_arg();
        }
        _ => {}
    }
    e
}

/// A comparison key: letters and digits only, so Markdown escaping and
/// whitespace cannot make present text look absent.
fn key_of(text: &str) -> String {
    text.chars()
        .filter(|c| c.is_alphanumeric())
        .flat_map(char::to_lowercase)
        .collect()
}

/// The comparison text of one slide: its shapes (or only `shape`), table cells
/// and notes.
fn slide_haystack(slide: &SlideFacts, shape: Option<&ShapeSelector>) -> String {
    let mut out = String::new();
    if let Some(selector) = shape {
        for s in &slide.shapes {
            let hit = match selector {
                ShapeSelector::Index(i) => s.text_index == Some(*i),
                ShapeSelector::Name(n) => s.name.eq_ignore_ascii_case(n.trim()),
            };
            if hit {
                out.push_str(&key_of(&s.text));
            }
        }
        return out;
    }
    for s in &slide.shapes {
        out.push_str(&key_of(&s.text));
    }
    for cell in slide.tables.iter().flat_map(|t| t.cells.iter().flatten()) {
        out.push_str(&key_of(cell));
    }
    out.push_str(&key_of(&slide.notes));
    out
}

fn slide_objects(slide: Option<&SlideFacts>) -> (usize, usize, usize) {
    slide.map_or((0, 0, 0), |s| {
        (
            s.tables.len(),
            s.charts.len(),
            s.shapes
                .iter()
                .filter(|x| x.kind == ShapeKind::Picture)
                .count(),
        )
    })
}

/// Confirm the call's additions are on its target slide in the saved file,
/// comparing the slide before the call with the slide after it.
pub(crate) fn check_expect(
    before_facts: &[SlideFacts],
    after_facts: &[SlideFacts],
    expect: &Expect,
) -> Result<(), Sentence> {
    let Some(target) = expect.slide else {
        return Ok(());
    };
    let Some(after) = after_facts.get(target) else {
        return Err("The slide is not in the saved file, so nothing was saved.".to_owned());
    };
    // A slide the call adds has no "before"; every other call edits one that
    // was there.
    let before = if expect.fresh {
        None
    } else {
        before_facts.get(target)
    };
    let (bt, bc, bp) = slide_objects(before);
    let (at, ac, ap) = slide_objects(Some(after));
    if at < bt + expect.tables || ac < bc + expect.charts || ap < bp + expect.pictures {
        return Err("The library did not save what was added, so nothing was saved. The slide may not accept it; try another slide.".to_owned());
    }
    let needles: Vec<String> = expect
        .text
        .iter()
        .map(|t| key_of(t))
        .filter(|k| !k.is_empty())
        .collect();
    if needles.is_empty() {
        return Ok(());
    }
    let after_text = slide_haystack(after, expect.shape.as_ref());
    let before_text = before.map_or_else(String::new, |b| slide_haystack(b, expect.shape.as_ref()));
    let ok = needles.iter().all(|n| {
        let now = after_text.matches(n.as_str()).count();
        if expect.newly {
            now > before_text.matches(n.as_str()).count()
        } else {
            now >= 1
        }
    });
    if ok {
        return Ok(());
    }
    Err("The text did not appear on the slide, so nothing was saved. The slide probably has no placeholder for it: bullets and titles need a title_content (or title, section_header, two_content) slide; use set_text to put text on a blank slide.".to_owned())
}

/// One authoring call end to end on a blocking thread: open the deck once,
/// read what the file holds once, apply the tool, serialise once and verify
/// the saved package. Returns the sentence and the new file's bytes.
pub(crate) fn run_edit(
    tool: PptxTool,
    args: &Value,
    bytes: &[u8],
    image: Option<&[u8]>,
    limits: &elitea_office_core::Limits,
) -> Result<(String, Vec<u8>), Sentence> {
    let mut opened = open_with_facts(bytes, limits)?;
    let slides_before = opened.facts.len();
    let message = apply_authoring(tool, args, &mut opened.deck, &opened.facts, image)?;
    let expect = expected_gain(tool, args, slides_before);
    let saved = finish_edit(
        &opened,
        limits,
        &Check {
            allow_fewer: tool == PptxTool::DeleteSlide,
            expect: Some(&expect),
            slides: (tool == PptxTool::AddSlide).then_some(slides_before + 1),
        },
    )?;
    Ok((message, saved))
}

fn count(deck: &Deck) -> Result<usize, Sentence> {
    deck.slide_count().map_err(oe)
}

/// A box in inches from the optional x, y, width, height arguments, with the
/// given defaults.
fn rect(args: &Value, default: (f64, f64, f64, f64)) -> Rect {
    Rect::inches(
        opt_f64(args, "x").unwrap_or(default.0),
        opt_f64(args, "y").unwrap_or(default.1),
        opt_f64(args, "width").unwrap_or(default.2),
        opt_f64(args, "height").unwrap_or(default.3),
    )
}

const EMU_PER_INCH: f64 = 914_400.0;

/// The slide size in inches (default 10 x 7.5).
fn slide_inches(facts: &[SlideFacts]) -> (f64, f64) {
    facts.first().map_or((10.0, 7.5), |s| {
        (
            s.size.0 as f64 / EMU_PER_INCH,
            s.size.1 as f64 / EMU_PER_INCH,
        )
    })
}

fn default_box(facts: &[SlideFacts]) -> (f64, f64, f64, f64) {
    let (w, h) = slide_inches(facts);
    (0.5, 1.5, (w - 1.0).max(1.0), (h - 2.0).max(1.0))
}

// ---------------------------------------------------------------------------
// Authoring
// ---------------------------------------------------------------------------

fn layout_of(name: Option<&str>) -> Result<SlideLayout, Sentence> {
    Ok(match name.unwrap_or("title_content") {
        "title" => SlideLayout::Title,
        "title_content" => SlideLayout::TitleContent,
        "section_header" => SlideLayout::SectionHeader,
        "two_content" => SlideLayout::TwoContent,
        "blank" => SlideLayout::Blank,
        other => {
            return Err(format!(
                "Unknown layout `{other}`. Use title, title_content, section_header, two_content or blank."
            ));
        }
    })
}

fn bullets_of(value: &Value) -> Result<Vec<Bullet>, Sentence> {
    let items = value
        .as_array()
        .ok_or_else(|| "The bullets must be a list.".to_owned())?;
    items
        .iter()
        .map(|item| match item {
            Value::String(text) => Ok(Bullet::new(text.clone())),
            Value::Object(o) => {
                let text = o
                    .get("text")
                    .and_then(Value::as_str)
                    .ok_or_else(|| "Every bullet object needs a `text`.".to_owned())?;
                Ok(Bullet {
                    text: text.to_owned(),
                    level: o
                        .get("level")
                        .and_then(Value::as_u64)
                        .map_or(0, |l| u8::try_from(l.min(8)).unwrap_or(0)),
                    bold: o.get("bold").and_then(Value::as_bool).unwrap_or(false),
                })
            }
            _ => Err("A bullet is a string or an object with `text`.".to_owned()),
        })
        .collect()
}

fn chart_kind(name: Option<&str>) -> Result<ChartKind, Sentence> {
    Ok(match name.unwrap_or("column") {
        "bar" => ChartKind::ClusteredBar,
        "stacked_bar" => ChartKind::StackedBar,
        "column" => ChartKind::ClusteredColumn,
        "stacked_column" => ChartKind::StackedColumn,
        "line" => ChartKind::Line,
        "pie" => ChartKind::Pie,
        "doughnut" => ChartKind::Doughnut,
        "area" => ChartKind::Area,
        "scatter" => ChartKind::Scatter,
        other => return Err(format!("Unknown chart_type `{other}`.")),
    })
}

fn chart_spec(args: &Value) -> Result<ChartSpec, Sentence> {
    let categories: Vec<String> = args
        .get("categories")
        .and_then(Value::as_array)
        .ok_or_else(|| "`categories` is required: a list of labels.".to_owned())?
        .iter()
        .map(|c| match c {
            Value::String(s) => s.clone(),
            other => other.to_string(),
        })
        .collect();
    let series = args
        .get("series")
        .and_then(Value::as_array)
        .ok_or_else(|| "`series` is required: a list of {name, values}.".to_owned())?
        .iter()
        .map(|s| {
            let name = s
                .get("name")
                .and_then(Value::as_str)
                .ok_or_else(|| "Every series needs a `name`.".to_owned())?;
            let values = s
                .get("values")
                .and_then(Value::as_array)
                .ok_or_else(|| format!("Series `{name}` needs `values`."))?
                .iter()
                .map(|v| {
                    v.as_f64()
                        .filter(|f| f.is_finite())
                        .ok_or_else(|| format!("Series `{name}` has a value that is not a number."))
                })
                .collect::<Result<Vec<f64>, _>>()?;
            if values.len() != categories.len() {
                return Err(format!(
                    "Series `{name}` has {} values for {} categories: give exactly one per category.",
                    values.len(),
                    categories.len()
                ));
            }
            Ok(Series {
                name: name.to_owned(),
                values,
            })
        })
        .collect::<Result<Vec<_>, Sentence>>()?;
    Ok(ChartSpec {
        kind: chart_kind(args.get("chart_type").and_then(Value::as_str))?,
        categories,
        series,
        title: opt_str(args, "title"),
        legend_position: opt_str(args, "legend_position"),
        data_labels: opt_bool(args, "data_labels").unwrap_or(false),
    })
}

/// The shape of a slide a text edit addresses: a number (1-based, among the
/// slide's text shapes) or a name.
fn find_shape(deck: &Deck, slide: usize, selector: &Value) -> Result<ShapeInfo, Sentence> {
    let shapes = deck.shapes(slide).map_err(oe)?;
    let found = match selector {
        Value::Number(n) => n
            .as_u64()
            .and_then(|n| n.checked_sub(1))
            .and_then(|i| shapes.iter().find(|s| s.index as u64 == i)),
        Value::String(name) => shapes
            .iter()
            .find(|s| s.name.eq_ignore_ascii_case(name.trim())),
        _ => None,
    };
    found.cloned().ok_or_else(|| {
        format!(
            "Slide {} has no text shape `{}`. list_placeholders with include_all lists them.",
            slide + 1,
            selector
                .as_str()
                .map_or_else(|| selector.to_string(), str::to_owned)
        )
    })
}

/// Apply one authoring tool to `deck`. `image` is the picture bytes
/// `add_image` needs, read by the caller. Returns the sentence reported on
/// success.
#[allow(clippy::too_many_lines)]
pub(super) fn apply_authoring(
    tool: PptxTool,
    args: &Value,
    deck: &mut Deck,
    facts: &[SlideFacts],
    image: Option<&[u8]>,
) -> Result<String, Sentence> {
    let slides = count(deck)?;
    match tool {
        PptxTool::AddSlide => {
            let layout = layout_of(args.get("layout").and_then(Value::as_str))?;
            let position = match opt_uint(args, "position") {
                Some(p) if p == 0 || p > slides as u64 + 1 => {
                    return Err(format!(
                        "`position` must be between 1 and {} for this deck.",
                        slides + 1
                    ));
                }
                other => other,
            };
            let at = deck.add_slide(layout).map_err(oe)?;
            if let Some(title) = opt_str(args, "title") {
                deck.set_title(at, &title).map_err(oe)?;
            }
            if let Some(bullets) = args.get("bullets") {
                let bullets = bullets_of(bullets)?;
                if !bullets.is_empty() {
                    deck.add_bullets(at, &bullets).map_err(oe)?;
                }
            }
            if let Some(notes) = opt_str(args, "notes") {
                deck.set_notes(at, &notes).map_err(oe)?;
            }
            let final_pos = match position {
                Some(p) => {
                    let to = usize::try_from(p - 1).unwrap_or(at);
                    if to != at {
                        deck.move_slide(at, to).map_err(oe)?;
                    }
                    to
                }
                None => at,
            };
            Ok(format!(
                "Added slide {} ({}). The deck has {} slides.",
                final_pos + 1,
                args.get("layout")
                    .and_then(Value::as_str)
                    .unwrap_or("title_content"),
                slides + 1
            ))
        }
        PptxTool::DeleteSlide => {
            let slide = slide_arg(args, "slide", slides)?;
            deck.delete_slide(slide).map_err(oe)?;
            Ok(format!(
                "Deleted slide {}. The deck has {} slides.",
                slide + 1,
                slides - 1
            ))
        }
        PptxTool::MoveSlide => {
            let from = slide_arg(args, "slide", slides)?;
            let to = slide_arg(args, "to_slide", slides)?;
            deck.move_slide(from, to).map_err(oe)?;
            Ok(format!("Moved slide {} to position {}.", from + 1, to + 1))
        }
        PptxTool::SetTitle => {
            let slide = slide_arg(args, "slide", slides)?;
            let title = args
                .get("title")
                .and_then(Value::as_str)
                .ok_or_else(|| "`title` is required.".to_owned())?;
            deck.set_title(slide, title).map_err(oe)?;
            Ok(format!("Set the title of slide {}.", slide + 1))
        }
        PptxTool::SetText => {
            let slide = slide_arg(args, "slide", slides)?;
            let text = args
                .get("text")
                .and_then(Value::as_str)
                .ok_or_else(|| "`text` is required.".to_owned())?;
            if let Some(selector) = args.get("shape").filter(|v| !v.is_null()) {
                let shape = find_shape(deck, slide, selector)?;
                write_shape_text(deck, slide, &shape, text).map_err(oe)?;
                Ok(format!(
                    "Replaced the text of shape {} (`{}`) on slide {}.",
                    shape.index + 1,
                    shape.name,
                    slide + 1
                ))
            } else {
                deck.add_text(slide, text, rect(args, (0.5, 1.5, 9.0, 1.0)))
                    .map_err(oe)?;
                Ok(format!("Added a text box to slide {}.", slide + 1))
            }
        }
        PptxTool::AddBullets => {
            let slide = slide_arg(args, "slide", slides)?;
            let bullets = bullets_of(
                args.get("items")
                    .ok_or_else(|| "`items` is required.".to_owned())?,
            )?;
            if bullets.is_empty() {
                return Err("`items` is empty.".to_owned());
            }
            deck.add_bullets(slide, &bullets).map_err(oe)?;
            Ok(format!(
                "Added {} bullet{} to slide {}.",
                bullets.len(),
                if bullets.len() == 1 { "" } else { "s" },
                slide + 1
            ))
        }
        PptxTool::AddTable => {
            let slide = slide_arg(args, "slide", slides)?;
            let rows = args
                .get("rows")
                .and_then(Value::as_array)
                .ok_or_else(|| "`rows` is required: a list of rows.".to_owned())?;
            let grid: Vec<Vec<String>> = rows
                .iter()
                .map(|row| {
                    row.as_array()
                        .ok_or_else(|| "Every row is a list of cell strings.".to_owned())?
                        .iter()
                        .map(|c| match c {
                            Value::String(s) => Ok(s.clone()),
                            Value::Null => Ok(String::new()),
                            other => Ok(other.to_string()),
                        })
                        .collect::<Result<Vec<String>, Sentence>>()
                })
                .collect::<Result<_, _>>()?;
            let cols = grid.first().map_or(0, Vec::len);
            if grid.is_empty() || cols == 0 {
                return Err("`rows` needs at least one row with one cell.".to_owned());
            }
            if grid.iter().any(|r| r.len() != cols) {
                return Err(format!(
                    "Every row needs the same number of cells ({cols}, as the first row has)."
                ));
            }
            let (x, y, w, _) = default_box(facts);
            let at = rect(args, (x, y, w, 0.4 * grid.len() as f64));
            let table = deck.add_table(slide, grid.len(), cols, at).map_err(oe)?;
            for (r, row) in grid.iter().enumerate() {
                for (c, cell) in row.iter().enumerate() {
                    if !cell.is_empty() {
                        deck.set_table_cell(slide, table, r, c, cell).map_err(oe)?;
                    }
                }
            }
            Ok(format!(
                "Added a {} x {} table to slide {} (a table's number on a slide is its order among the slide's tables).",
                grid.len(),
                cols,
                slide + 1
            ))
        }
        PptxTool::SetTableCell => {
            let slide = slide_arg(args, "slide", slides)?;
            let ordinal = opt_uint(args, "table").unwrap_or(1).max(1);
            let row = opt_uint(args, "row").filter(|r| *r >= 1);
            let col = opt_uint(args, "column").filter(|c| *c >= 1);
            let (Some(row), Some(col)) = (row, col) else {
                return Err("`row` and `column` are required, from 1.".to_owned());
            };
            let text = args
                .get("text")
                .and_then(Value::as_str)
                .ok_or_else(|| "`text` is required.".to_owned())?;
            let tables = &facts[slide].tables;
            let table = tables
                .get(usize::try_from(ordinal - 1).unwrap_or(usize::MAX))
                .ok_or_else(|| {
                    format!(
                        "Slide {} has {} table{}; there is no table {ordinal}.",
                        slide + 1,
                        tables.len(),
                        if tables.len() == 1 { "" } else { "s" }
                    )
                })?;
            let (r, c) = (
                usize::try_from(row - 1).unwrap_or(usize::MAX),
                usize::try_from(col - 1).unwrap_or(usize::MAX),
            );
            let rows = table.cells.len();
            let cols = table.cells.first().map_or(0, Vec::len);
            if r >= rows || c >= cols {
                return Err(format!(
                    "The table is {rows} x {cols}; row {row}, column {col} is outside it."
                ));
            }
            deck.set_cell_paragraphs(slide, table.shape, r, c, &cell_lines(text))
                .map_err(oe)?;
            Ok(format!(
                "Set row {row}, column {col} of table {ordinal} on slide {}.",
                slide + 1
            ))
        }
        PptxTool::AddChart => {
            let slide = slide_arg(args, "slide", slides)?;
            let spec = chart_spec(args)?;
            let (x, y, w, h) = default_box(facts);
            let at = rect(args, (x, y, w, h));
            deck.add_chart(slide, &spec, at).map_err(oe)?;
            Ok(format!(
                "Added a {} chart with {} series and {} categories to slide {}. PowerPoint draws it; render_preview does not.",
                args.get("chart_type")
                    .and_then(Value::as_str)
                    .unwrap_or("column"),
                spec.series.len(),
                spec.categories.len(),
                slide + 1
            ))
        }
        PptxTool::AddImage => {
            let slide = slide_arg(args, "slide", slides)?;
            let bytes = image.ok_or_else(|| "The picture could not be read.".to_owned())?;
            let (x, y, w, h) = default_box(facts);
            let given = rect(args, (x, y, w, h));
            let at = match elitea_office_core::image_size(bytes) {
                Some((iw, ih)) if iw > 0 && ih > 0 => fit(given, iw, ih),
                _ => given,
            };
            deck.add_image(slide, bytes, at).map_err(oe)?;
            Ok(format!("Added a picture to slide {}.", slide + 1))
        }
        PptxTool::SetNotes => {
            let slide = slide_arg(args, "slide", slides)?;
            let notes = args
                .get("notes")
                .and_then(Value::as_str)
                .ok_or_else(|| "`notes` is required.".to_owned())?;
            deck.set_notes(slide, notes).map_err(oe)?;
            Ok(format!("Set the speaker notes of slide {}.", slide + 1))
        }
        PptxTool::ApplyTheme => {
            let colors: std::collections::HashMap<String, String> = args
                .get("colors")
                .and_then(Value::as_object)
                .map(|o| {
                    o.iter()
                        .filter_map(|(k, v)| v.as_str().map(|s| (k.clone(), s.to_owned())))
                        .collect()
                })
                .unwrap_or_default();
            let theme = Theme {
                accent: opt_str(args, "accent"),
                heading_font: opt_str(args, "heading_font"),
                body_font: opt_str(args, "body_font"),
                colors,
            };
            if theme == Theme::default() {
                return Err(
                    "Give at least one of accent, heading_font, body_font or colors.".to_owned(),
                );
            }
            deck.apply_theme(&theme).map_err(oe)?;
            Ok("Applied the theme.".to_owned())
        }
        _ => Err("This tool does not edit the deck.".to_owned()),
    }
}

/// `given` shrunk to the picture's proportions and centred in the box.
fn fit(given: Rect, iw: u64, ih: u64) -> Rect {
    let (iw, ih) = (iw as f64, ih as f64);
    let scale = (given.w as f64 / iw).min(given.h as f64 / ih);
    let (w, h) = ((iw * scale) as i64, (ih * scale) as i64);
    Rect {
        x: given.x + (given.w - w) / 2,
        y: given.y + (given.h - h) / 2,
        w,
        h,
    }
}

// ---------------------------------------------------------------------------
// Text writing (shared by set_text, replace_placeholders, fill, translate)
// ---------------------------------------------------------------------------

/// The lines of a cell's text: one paragraph each, an empty line kept.
fn cell_lines(text: &str) -> Vec<String> {
    text.replace("\r\n", "\n")
        .replace('\r', "\n")
        .split('\n')
        .map(str::to_owned)
        .collect()
}

/// Replace the text of a whole text shape. The lines of `text` go to the
/// shape's paragraphs in order (the paragraph structure is kept, where the
/// SDK flattens a frame to one paragraph); surplus lines stay in the last
/// paragraph as line breaks, and missing ones leave the rest empty.
///
/// The frame is written all or nothing: the only refusal (a shape with no
/// paragraph at all) is decided before the first write, and a paragraph with
/// no text run takes its text in a new run, so no frame is ever left half
/// written. A library error after the first write fails the whole call, which
/// saves nothing.
pub(super) fn write_shape_text(
    deck: &mut Deck,
    slide: usize,
    shape: &ShapeInfo,
    text: &str,
) -> Result<(), OfficeError> {
    let normalised = text.replace("\r\n", "\n").replace('\r', "\n");
    let lines: Vec<&str> = normalised.split('\n').collect();
    let n = shape.paragraphs.len();
    if n == 0 {
        return Err(OfficeError::Unsupported(format!(
            "Shape `{}` has no paragraph to write text into.",
            shape.name
        )));
    }
    for index in 0..n {
        let line = if index + 1 < n {
            lines.get(index).copied().unwrap_or("").to_owned()
        } else {
            lines
                .get(index..)
                .map(|rest| rest.join("\n"))
                .unwrap_or_default()
        };
        deck.set_paragraph_text(slide, shape.index, index, &line)?;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Locations: text frames and table cells
// ---------------------------------------------------------------------------

/// Where a piece of text lives.
#[derive(Clone, Debug)]
pub(crate) enum Place {
    /// A text shape; `shape` is its `ShapeInfo`.
    Frame(ShapeInfo),
    /// A table cell.
    Cell {
        shape: usize,
        row: usize,
        col: usize,
    },
}

#[derive(Clone, Debug)]
pub(crate) struct Location {
    pub(crate) slide: usize,
    pub(crate) place: Place,
    /// The frame's text (paragraphs joined by a line break) or the cell's.
    pub(crate) text: String,
    /// The shape's name (a cell: the table's).
    pub(crate) name: String,
}

/// Every text frame and non-empty table cell of the deck, in slide order,
/// from the fact sheet read once.
pub(super) fn locations(deck: &Deck, facts: &[SlideFacts]) -> Result<Vec<Location>, Sentence> {
    let mut out = Vec::new();
    for (slide, f) in facts.iter().enumerate() {
        for shape in deck.shapes(slide).map_err(oe)? {
            out.push(Location {
                slide,
                text: shape.text.clone(),
                name: shape.name.clone(),
                place: Place::Frame(shape),
            });
        }
        for table in &f.tables {
            let name = f
                .shapes
                .get(table.shape)
                .map(|s| s.name.clone())
                .unwrap_or_default();
            for (row, cells) in table.cells.iter().enumerate() {
                for (col, text) in cells.iter().enumerate() {
                    if !text.is_empty() {
                        out.push(Location {
                            slide,
                            text: text.clone(),
                            name: name.clone(),
                            place: Place::Cell {
                                shape: table.shape,
                                row,
                                col,
                            },
                        });
                    }
                }
            }
        }
    }
    Ok(out)
}

/// The locations the SDK counts as placeholders.
pub(super) fn placeholder_locations(
    deck: &Deck,
    facts: &[SlideFacts],
) -> Result<Vec<Location>, Sentence> {
    Ok(locations(deck, facts)?
        .into_iter()
        .filter(|l| is_placeholder_text(&l.text))
        .collect())
}

/// The distinct texts of `locations`, in first-seen order, or `None` once
/// there are more than `cap` (the call is refused before anything is
/// collected beyond that).
pub(crate) fn distinct_texts<'a>(
    texts: impl Iterator<Item = &'a str>,
    cap: usize,
) -> Option<Vec<String>> {
    let mut seen: HashSet<&str> = HashSet::new();
    let mut out = Vec::new();
    for text in texts {
        if seen.insert(text) {
            if out.len() >= cap {
                return None;
            }
            out.push(text.to_owned());
        }
    }
    Some(out)
}

/// Write `text` to a location. `Ok(false)` only when the place cannot take
/// any text at all (a frame with no paragraph), decided before anything is
/// written; every other failure is an error that fails the call.
pub(crate) fn write_location(
    deck: &mut Deck,
    location: &Location,
    text: &str,
) -> Result<bool, Sentence> {
    match &location.place {
        Place::Frame(shape) => match write_shape_text(deck, location.slide, shape, text) {
            Ok(()) => Ok(true),
            Err(OfficeError::Unsupported(_)) if shape.paragraphs.is_empty() => Ok(false),
            Err(e) => Err(oe(e)),
        },
        Place::Cell { shape, row, col } => {
            deck.set_cell_paragraphs(location.slide, *shape, *row, *col, &cell_lines(text))
                .map_err(oe)?;
            Ok(true)
        }
    }
}

/// `list_placeholders`: the report as JSON.
pub(super) fn list_report(
    deck: &Deck,
    facts: &[SlideFacts],
    include_all: bool,
) -> Result<Value, Sentence> {
    let all = if include_all {
        locations(deck, facts)?
    } else {
        placeholder_locations(deck, facts)?
    };
    let unique: HashSet<&str> = all.iter().map(|l| l.text.as_str()).collect();
    let rows: Vec<Value> = all
        .iter()
        .map(|l| {
            let mut row = Map::new();
            row.insert("slide".into(), Value::from(l.slide + 1));
            match &l.place {
                Place::Frame(shape) => {
                    row.insert("location".into(), "text_frame".into());
                    row.insert("shape".into(), Value::from(shape.index + 1));
                }
                Place::Cell { row: r, col, .. } => {
                    row.insert("location".into(), "table_cell".into());
                    row.insert("row".into(), Value::from(r + 1));
                    row.insert("column".into(), Value::from(col + 1));
                }
            }
            row.insert("name".into(), l.name.clone().into());
            row.insert("text".into(), l.text.clone().into());
            Value::Object(row)
        })
        .collect();
    let mut out = Map::new();
    out.insert("total_locations".into(), Value::from(all.len()));
    out.insert("unique_texts".into(), Value::from(unique.len()));
    out.insert("placeholders".into(), Value::Array(rows));
    out.insert(
        "note".into(),
        "Text frames and table cells are listed. SmartArt text is not read by this runtime.".into(),
    );
    Ok(Value::Object(out))
}

/// The replacement of every key in `text` in ONE pass over the original: at
/// each position the longest key wins (leftmost-longest), and a replacement is
/// never searched again, so one key's output cannot feed another. Returns the
/// new text and how many times each key matched.
fn substitute(finder: &AhoCorasick, values: &[&str], text: &str, counts: &mut [usize]) -> String {
    let mut out = String::with_capacity(text.len());
    let mut last = 0;
    for m in finder.find_iter(text) {
        out.push_str(&text[last..m.start()]);
        out.push_str(values[m.pattern().as_usize()]);
        counts[m.pattern().as_usize()] += 1;
        last = m.end();
    }
    out.push_str(&text[last..]);
    out
}

/// `replace_placeholders` end to end on a blocking thread.
pub(crate) fn run_replace(
    bytes: &[u8],
    replacements: &BTreeMap<String, String>,
    limits: &elitea_office_core::Limits,
) -> Result<(usize, Vec<String>, Vec<u8>), Sentence> {
    let mut opened = open_with_facts(bytes, limits)?;
    let (changed, unmatched) = replace_all_keys(&mut opened.deck, &opened.facts, replacements)?;
    let saved = finish_edit(&opened, limits, &Check::default())?;
    Ok((changed, unmatched, saved))
}

/// `replace_placeholders`: every key replaced in one pass over the original
/// text of each frame and cell. A match inside one run is replaced in that run
/// (its formatting stays); a match across runs rewrites its paragraph into the
/// first run; a match across paragraphs rewrites the frame. Returns the number
/// of matches in the original and the keys that matched nothing.
pub(super) fn replace_all_keys(
    deck: &mut Deck,
    facts: &[SlideFacts],
    replacements: &BTreeMap<String, String>,
) -> Result<(usize, Vec<String>), Sentence> {
    let keys: Vec<&str> = replacements.keys().map(String::as_str).collect();
    let values: Vec<&str> = replacements.values().map(String::as_str).collect();
    let finder = AhoCorasick::builder()
        .match_kind(MatchKind::LeftmostLongest)
        .build(&keys)
        .map_err(|_| "The replacement keys could not be used.".to_owned())?;
    let mut counts = vec![0usize; keys.len()];
    for (slide, f) in facts.iter().enumerate() {
        for shape in deck.shapes(slide).map_err(oe)? {
            if finder.find(&shape.text).is_none() {
                continue;
            }
            // A match that contains a line break reaches over paragraphs (or a
            // break inside one): the frame is rewritten as a whole.
            let reaches = finder
                .find_iter(&shape.text)
                .any(|m| shape.text[m.range()].contains('\n'));
            if reaches {
                let new = substitute(&finder, &values, &shape.text, &mut counts);
                write_shape_text(deck, slide, &shape, &new).map_err(oe)?;
                continue;
            }
            for (pi, paragraph) in shape.paragraphs.iter().enumerate() {
                replace_in_paragraph(
                    deck,
                    &finder,
                    &values,
                    &mut counts,
                    slide,
                    &shape,
                    pi,
                    paragraph,
                )?;
            }
        }
        for table in &f.tables {
            for (row, cells) in table.cells.iter().enumerate() {
                for (col, text) in cells.iter().enumerate() {
                    if finder.find(text).is_none() {
                        continue;
                    }
                    let new = substitute(&finder, &values, text, &mut counts);
                    deck.set_cell_paragraphs(slide, table.shape, row, col, &cell_lines(&new))
                        .map_err(oe)?;
                }
            }
        }
    }
    let unmatched = keys
        .iter()
        .zip(&counts)
        .filter(|(_, n)| **n == 0)
        .map(|(k, _)| (*k).to_owned())
        .collect();
    Ok((counts.iter().sum(), unmatched))
}

#[allow(
    clippy::too_many_arguments,
    reason = "one paragraph's replacement context"
)]
fn replace_in_paragraph(
    deck: &mut Deck,
    finder: &AhoCorasick,
    values: &[&str],
    counts: &mut [usize],
    slide: usize,
    shape: &ShapeInfo,
    pi: usize,
    paragraph: &ParagraphInfo,
) -> Result<(), Sentence> {
    let matches: Vec<_> = finder.find_iter(&paragraph.text).collect();
    if matches.is_empty() {
        return Ok(());
    }
    // Byte range of each run inside the paragraph text (a break is one `\n`).
    let mut ranges = Vec::with_capacity(paragraph.runs.len());
    let mut at = 0;
    for run in &paragraph.runs {
        let len = if run.line_break { 1 } else { run.text.len() };
        ranges.push((at, at + len));
        at += len;
    }
    let inside_one_run = |m: &aho_corasick::Match| {
        paragraph
            .runs
            .iter()
            .zip(&ranges)
            .any(|(r, (s, e))| !r.line_break && m.start() >= *s && m.end() <= *e)
    };
    if matches.iter().all(inside_one_run) {
        // Matches counted once, from the paragraph; each run is then rewritten
        // from its own text (the same matches, run-local).
        for m in &matches {
            counts[m.pattern().as_usize()] += 1;
        }
        let mut scratch = vec![0usize; counts.len()];
        for (ri, run) in paragraph.runs.iter().enumerate() {
            if run.line_break || finder.find(&run.text).is_none() {
                continue;
            }
            let new = substitute(finder, values, &run.text, &mut scratch);
            deck.set_run_text(slide, shape.index, pi, ri, &new)
                .map_err(oe)?;
        }
    } else {
        let new = substitute(finder, values, &paragraph.text, counts);
        deck.set_paragraph_text(slide, shape.index, pi, &new)
            .map_err(oe)?;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Translation
// ---------------------------------------------------------------------------

/// One paragraph to translate and where to put it back.
#[derive(Clone, Debug)]
pub(super) struct Item {
    pub(super) slide: usize,
    pub(super) target: ItemTarget,
    pub(super) text: String,
}

#[derive(Clone, Debug)]
pub(super) enum ItemTarget {
    Paragraph {
        shape: ShapeInfo,
        index: usize,
    },
    /// One paragraph of a table cell; the cell is rewritten once, keeping its
    /// paragraph structure.
    CellParagraph {
        shape: usize,
        row: usize,
        col: usize,
        index: usize,
    },
}

/// Every non-empty paragraph of every text shape and table cell, as the
/// SDK's `translate_presentation` collects them. `None` when the deck holds
/// more than `cap` distinct paragraphs (the call is refused before the rest
/// is collected).
pub(super) fn translation_items(
    deck: &Deck,
    facts: &[SlideFacts],
    cap: usize,
) -> Result<Option<Vec<Item>>, Sentence> {
    let mut out = Vec::new();
    let mut distinct: HashSet<String> = HashSet::new();
    let mut take = |text: &str| -> bool {
        distinct.insert(text.to_owned());
        distinct.len() <= cap
    };
    for (slide, f) in facts.iter().enumerate() {
        for shape in deck.shapes(slide).map_err(oe)? {
            for (index, p) in shape.paragraphs.iter().enumerate() {
                if !p.text.trim().is_empty() {
                    if !take(&p.text) {
                        return Ok(None);
                    }
                    out.push(Item {
                        slide,
                        text: p.text.clone(),
                        target: ItemTarget::Paragraph {
                            shape: shape.clone(),
                            index,
                        },
                    });
                }
            }
        }
        for table in &f.tables {
            for (row, cells) in table.cells.iter().enumerate() {
                for (col, cell) in cells.iter().enumerate() {
                    for (index, line) in cell.split('\n').enumerate() {
                        if !line.trim().is_empty() {
                            if !take(line) {
                                return Ok(None);
                            }
                            out.push(Item {
                                slide,
                                text: line.to_owned(),
                                target: ItemTarget::CellParagraph {
                                    shape: table.shape,
                                    row,
                                    col,
                                    index,
                                },
                            });
                        }
                    }
                }
            }
        }
    }
    Ok(Some(out))
}

/// The translated paragraphs of each cell, keyed by slide, table, row, column.
type CellParagraphs = BTreeMap<(usize, usize, usize, usize), Vec<(usize, String)>>;

/// Put the translations back; an entry of `None` leaves its paragraph as it
/// is. A cell is rewritten once, one paragraph per original line, so an empty
/// line and an untranslated one stay where they were. Returns how many
/// paragraphs were written.
pub(super) fn apply_translations(
    deck: &mut Deck,
    facts: &[SlideFacts],
    items: &[Item],
    translated: &[Option<String>],
) -> Result<usize, Sentence> {
    let mut written = 0usize;
    let mut cells: CellParagraphs = BTreeMap::new();
    for (item, text) in items.iter().zip(translated) {
        let Some(text) = text else { continue };
        match &item.target {
            ItemTarget::Paragraph { shape, index } => {
                deck.set_paragraph_text(item.slide, shape.index, *index, text)
                    .map_err(oe)?;
                written += 1;
            }
            ItemTarget::CellParagraph {
                shape,
                row,
                col,
                index,
            } => cells
                .entry((item.slide, *shape, *row, *col))
                .or_default()
                .push((*index, text.clone())),
        }
    }
    for ((slide, shape, row, col), parts) in cells {
        let current = facts
            .get(slide)
            .and_then(|f| f.tables.iter().find(|t| t.shape == shape))
            .and_then(|t| t.cells.get(row))
            .and_then(|r| r.get(col))
            .cloned()
            .unwrap_or_default();
        let mut lines: Vec<String> = cell_lines(&current);
        for (i, text) in &parts {
            if let Some(line) = lines.get_mut(*i) {
                // One paragraph stays one paragraph: a translation's own
                // line breaks do not add paragraphs to the cell.
                *line = text.replace(['\r', '\n'], " ");
            }
        }
        deck.set_cell_paragraphs(slide, shape, row, col, &lines)
            .map_err(oe)?;
        written += parts.len();
    }
    Ok(written)
}

// ---------------------------------------------------------------------------
// Design check
// ---------------------------------------------------------------------------

const DEFAULT_FONT_PT: f64 = 18.0;
const EMU_PER_PT: f64 = 12_700.0;
const MIN_BODY_PT: f32 = 12.0;
const DENSE_CHARS: usize = 900;

/// The WCAG relative luminance of an `RRGGBB` colour.
fn luminance(hex: &str) -> Option<f64> {
    if hex.len() != 6 {
        return None;
    }
    let channel = |range: std::ops::Range<usize>| -> Option<f64> {
        let v = f64::from(u8::from_str_radix(hex.get(range)?, 16).ok()?) / 255.0;
        Some(if v <= 0.03928 {
            v / 12.92
        } else {
            ((v + 0.055) / 1.055).powf(2.4)
        })
    };
    Some(0.2126 * channel(0..2)? + 0.7152 * channel(2..4)? + 0.0722 * channel(4..6)?)
}

fn contrast(a: &str, b: &str) -> Option<f64> {
    let (la, lb) = (luminance(a)?, luminance(b)?);
    let (hi, lo) = if la > lb { (la, lb) } else { (lb, la) };
    Some((hi + 0.05) / (lo + 0.05))
}

struct Issue {
    slide: usize,
    shape: Option<String>,
    code: &'static str,
    severity: &'static str,
    message: String,
}

/// The heuristics of `design_check` over a fact sheet read from the deck's own
/// geometry and text. `only` limits it to one slide (0-based).
#[allow(clippy::too_many_lines)]
pub(crate) fn design_from_facts(facts: &[SlideFacts], only: Option<usize>) -> Value {
    let mut issues: Vec<Issue> = Vec::new();
    let mut checked = 0usize;
    for f in facts {
        if only.is_some_and(|o| o != f.slide) {
            continue;
        }
        checked += 1;
        let slide = f.slide;
        let (sw, sh) = (f.size.0 as f64, f.size.1 as f64);
        let has_title = f.shapes.iter().any(|s| {
            matches!(s.placeholder.as_deref(), Some("title" | "ctrTitle"))
                && !s.text.trim().is_empty()
        });
        if !has_title {
            issues.push(Issue {
                slide,
                shape: None,
                code: "missing_title",
                severity: "info",
                message: "The slide has no title (a titled slide is navigable and accessible)."
                    .to_owned(),
            });
        }
        let mut chars_total = 0usize;
        for s in &f.shapes {
            let name = Some(s.name.clone());
            chars_total += s.text.chars().count();
            if s.kind == ShapeKind::Text
                && s.text.trim().is_empty()
                && s.placeholder
                    .as_deref()
                    .is_some_and(|p| !matches!(p, "dt" | "ftr" | "sldNum"))
            {
                issues.push(Issue {
                    slide,
                    shape: name.clone(),
                    code: "empty_placeholder",
                    severity: "warning",
                    message: "An empty placeholder shows prompt text in the editor and nothing when presented; fill or delete it.".to_owned(),
                });
            }
            if let Some(small) = s
                .font_sizes_pt
                .iter()
                .copied()
                .filter(|p| *p < MIN_BODY_PT)
                .reduce(f32::min)
                && !s.text.trim().is_empty()
            {
                issues.push(Issue {
                    slide,
                    shape: name.clone(),
                    code: "small_font",
                    severity: "warning",
                    message: format!("Text at {small} pt is hard to read when projected (minimum advised: {MIN_BODY_PT} pt)."),
                });
            }
            if let (Some(fg), Some(bg)) = (s.text_color.as_deref(), s.fill_color.as_deref())
                && let Some(ratio) = contrast(fg, bg)
                && ratio < 4.5
                && !s.text.trim().is_empty()
            {
                issues.push(Issue {
                    slide,
                    shape: name.clone(),
                    code: "low_contrast",
                    severity: "warning",
                    message: format!("Text #{fg} on fill #{bg} has contrast {ratio:.1}:1 (WCAG AA asks 4.5:1 for normal text)."),
                });
            }
            if let Some(r) = s.frame {
                let off = r.x < -(sw * 0.01) as i64
                    || r.y < -(sh * 0.01) as i64
                    || (r.x + r.w) as f64 > sw * 1.01
                    || (r.y + r.h) as f64 > sh * 1.01;
                if off {
                    issues.push(Issue {
                        slide,
                        shape: name.clone(),
                        code: "off_slide",
                        severity: "warning",
                        message: "The shape extends beyond the slide edge.".to_owned(),
                    });
                }
                if s.kind == ShapeKind::Text
                    && !s.text.trim().is_empty()
                    && let Some(msg) = overflow(s, r)
                {
                    issues.push(Issue {
                        slide,
                        shape: name,
                        code: "text_overflow",
                        severity: "warning",
                        message: msg,
                    });
                }
            }
        }
        // Overlaps between shapes that both carry their own frame.
        let framed: Vec<_> = f.shapes.iter().filter(|s| s.frame.is_some()).collect();
        for (i, a) in framed.iter().enumerate() {
            for b in &framed[i + 1..] {
                if let (Some(ra), Some(rb)) = (a.frame, b.frame)
                    && overlap_ratio(ra, rb) > 0.3
                {
                    issues.push(Issue {
                        slide,
                        shape: Some(a.name.clone()),
                        code: "overlap",
                        severity: "info",
                        message: format!(
                            "Overlaps `{}` by more than 30% of the smaller shape.",
                            b.name
                        ),
                    });
                }
            }
        }
        if chars_total > DENSE_CHARS {
            issues.push(Issue {
                slide,
                shape: None,
                code: "dense_slide",
                severity: "info",
                message: format!(
                    "The slide holds {chars_total} characters of text; consider splitting it."
                ),
            });
        }
    }
    let warnings = issues.iter().filter(|i| i.severity == "warning").count();
    let rows: Vec<Value> = issues
        .iter()
        .map(|i| {
            let mut o = Map::new();
            o.insert("slide".into(), Value::from(i.slide + 1));
            if let Some(s) = &i.shape {
                o.insert("shape".into(), s.clone().into());
            }
            o.insert("code".into(), i.code.into());
            o.insert("severity".into(), i.severity.into());
            o.insert("message".into(), i.message.clone().into());
            Value::Object(o)
        })
        .collect();
    let mut out = Map::new();
    out.insert("slides_checked".into(), Value::from(checked));
    out.insert("warnings".into(), Value::from(warnings));
    out.insert("infos".into(), Value::from(issues.len() - warnings));
    out.insert("issues".into(), Value::Array(rows));
    out.insert(
        "note".into(),
        "Heuristics over explicit geometry, font sizes and colours. Shapes that inherit their position or font from the layout, theme colours and the slide background are not measured, and nothing is rendered."
            .into(),
    );
    Value::Object(out)
}

fn overlap_ratio(a: Rect, b: Rect) -> f64 {
    let w = ((a.x + a.w).min(b.x + b.w) - a.x.max(b.x)).max(0) as f64;
    let h = ((a.y + a.h).min(b.y + b.h) - a.y.max(b.y)).max(0) as f64;
    let smaller = ((a.w * a.h).min(b.w * b.h)).max(1) as f64;
    w * h / smaller
}

/// The text-fit estimate: an average glyph is half an em wide and a line is
/// 1.2 em tall, inside the default 0.1 in left/right and 0.05 in top/bottom
/// insets.
fn overflow(shape: &elitea_office_core::ShapeFacts, frame: Rect) -> Option<String> {
    let pt = shape
        .font_sizes_pt
        .iter()
        .copied()
        .reduce(f32::max)
        .map_or(DEFAULT_FONT_PT, f64::from);
    let width = frame.w as f64 / EMU_PER_PT - 14.4;
    let height = frame.h as f64 / EMU_PER_PT - 7.2;
    if width <= pt || height <= 0.0 {
        return None;
    }
    let per_line = (width / (pt * 0.5)).floor().max(1.0);
    let lines: f64 = shape
        .text
        .split('\n')
        .map(|p| (p.chars().count() as f64 / per_line).ceil().max(1.0))
        .sum();
    let needed = lines * pt * 1.2;
    (needed > height * 1.05).then(|| {
        format!(
            "The text needs about {needed:.0} pt of height at {pt:.0} pt but the box is {height:.0} pt; it will likely overflow."
        )
    })
}

// ---------------------------------------------------------------------------
// Preview data
// ---------------------------------------------------------------------------

/// The charts of one slide as JSON, for `render_preview` (a preview draws
/// them as grey placeholders).
pub(super) fn chart_data(facts: &SlideFacts) -> Vec<Value> {
    facts
        .charts
        .iter()
        .map(|c| {
            let mut o = Map::new();
            o.insert("type".into(), c.kind.clone().into());
            if let Some(t) = &c.title {
                o.insert("title".into(), t.clone().into());
            }
            o.insert(
                "categories".into(),
                Value::Array(c.categories.iter().cloned().map(Value::from).collect()),
            );
            o.insert(
                "series".into(),
                Value::Array(
                    c.series
                        .iter()
                        .map(|(name, values)| {
                            let mut s = Map::new();
                            s.insert("name".into(), name.clone().into());
                            s.insert(
                                "values".into(),
                                Value::Array(
                                    values
                                        .iter()
                                        .map(|v| v.map_or(Value::Null, Value::from))
                                        .collect(),
                                ),
                            );
                            Value::Object(s)
                        })
                        .collect(),
                ),
            );
            Value::Object(o)
        })
        .collect()
}

pub(super) fn slide_count(deck: &Deck) -> Result<usize, Sentence> {
    count(deck)
}
