//! `diagram_sanitizer.py`: regex-only repair of the Mermaid blocks in a
//! generated page.
//!
//! A line-for-line port, in the Python order, with every quirk kept —
//! including the ones that look like bugs, because the gate is byte
//! equality with the Python output:
//!
//! * `_normalize_inner_quotes` replaces `\"x\"` with the LITERAL text
//!   `'\1'` (its template is `r"'\\1'"`), and drops the fallback pass's
//!   result when `normalize_inner_quotes` was already recorded;
//! * `_collapse_duplicate_deactivate` runs for any diagram that contains
//!   `deactivate ` and `alt `, and then removes every empty line;
//! * a `subgraph` representative that starts with a digit makes the
//!   template `\1<digits>` a missing group reference, which fails the whole
//!   sanitisation (Python raised; the page kept its text);
//! * `_ensure_flowchart_direction` keeps the bad direction token after the
//!   default one (`flowchart XY` → `flowchart TD XY`);
//! * the blank line after a diagram's closing fence is dropped (the fence
//!   pattern eats one newline and `sanitize_content` adds one back only
//!   when the next character is not a newline).
//!
//! Lengths and indices are in characters (Python `str`), never bytes.
//! The regular expressions go through [`super::pyregex`]. A failure
//! (a regex error, the backtrack cap) is returned as an error: the caller
//! keeps the page unsanitised, as `generate_page_content`'s `except` did.

use super::pyregex::{Flags, PyRe, ReError, escape};
use crate::graph::pystr;
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::fmt::Write as _;
use std::sync::LazyLock;
use unicode_normalization::UnicodeNormalization;

/// A pattern compiled once. `return`s the compile error from the
/// enclosing function (a transcription bug the tests catch).
macro_rules! re {
    ($pattern:expr) => {
        re!($pattern, Flags::NONE)
    };
    ($pattern:expr, $flags:expr) => {{
        static CELL: LazyLock<Result<PyRe, ReError>> =
            LazyLock::new(|| PyRe::new($pattern, $flags));
        match &*CELL {
            Ok(compiled) => compiled,
            Err(error) => return Err(error.clone()),
        }
    }};
}

const MERMAID_FENCE: &str = r"^```mermaid[^\n]*\n(.*?)^\s*```[ \t]*\n?";
const HEADER_TYPES: [&str; 10] = [
    "graph",
    "flowchart",
    "sequenceDiagram",
    "classDiagram",
    "stateDiagram",
    "erDiagram",
    "gantt",
    "journey",
    "mindmap",
    "pie",
];
const SEQUENCE: &str = "sequenceDiagram";
const PID: &str = r"[A-Za-z0-9_](?:[A-Za-z0-9_\-]*[A-Za-z0-9_])?";
const SMART_QUOTES: [(char, char); 4] = [
    ('\u{201c}', '"'),
    ('\u{201d}', '"'),
    ('\u{2018}', '\''),
    ('\u{2019}', '\''),
];
const UNICODE_ARROWS: [(char, &str); 3] =
    [('\u{2192}', "->"), ('\u{21d2}', "->"), ('\u{27f6}', "->")];
const RESERVED_SEQ_KEYWORDS: [&str; 12] = [
    "link",
    "click",
    "note",
    "over",
    "alt",
    "loop",
    "par",
    "and",
    "opt",
    "actor",
    "participant",
    "end",
];
/// `RESERVED_FLOW_KEYWORDS`.
const RESERVED_FLOW: &str = "end";

/// `SanitizerConfig` (the live caller uses the defaults).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SanitizerConfig {
    pub default_direction: String,
    pub enforce_single_flowchart_arrow: bool,
    pub auto_add_sequence_participants: bool,
    pub max_diagram_chars: usize,
    pub enable_repairs: bool,
}

impl Default for SanitizerConfig {
    fn default() -> Self {
        Self {
            default_direction: "TD".to_owned(),
            enforce_single_flowchart_arrow: true,
            auto_add_sequence_participants: true,
            max_diagram_chars: 8000,
            enable_repairs: true,
        }
    }
}

/// `DiagramRecord.status`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DiagramStatus {
    Valid,
    Fixed,
    Failed,
}

/// `DiagramRecord`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DiagramRecord {
    pub index: usize,
    pub original: String,
    pub sanitized: String,
    pub was_modified: bool,
    pub errors: Vec<String>,
    pub fixes: Vec<String>,
    pub status: DiagramStatus,
    pub hash: String,
}

/// `SanitizationSummary`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SanitizationSummary {
    pub total: usize,
    pub valid: usize,
    pub fixed: usize,
    pub failed: usize,
    pub records: Vec<DiagramRecord>,
}

/// The fixes applied, in order; `dict.fromkeys` deduplicates at the end.
#[derive(Debug, Default)]
struct Fixes(Vec<String>);

impl Fixes {
    fn push(&mut self, fix: &str) {
        self.0.push(fix.to_owned());
    }

    fn contains(&self, fix: &str) -> bool {
        self.0.iter().any(|f| f == fix)
    }

    /// `if fix not in fixes: fixes.append(fix)`.
    fn push_once(&mut self, fix: &str) {
        if !self.contains(fix) {
            self.push(fix);
        }
    }

    fn deduplicated(self) -> Vec<String> {
        let mut seen = HashSet::new();
        self.0
            .into_iter()
            .filter(|f| seen.insert(f.clone()))
            .collect()
    }
}

fn chars(text: &str) -> Vec<char> {
    text.chars().collect()
}

fn char_len(text: &str) -> usize {
    text.chars().count()
}

/// `text.split('\n')`.
fn split_lines(text: &str) -> Vec<String> {
    text.split('\n').map(str::to_owned).collect()
}

/// `str.rstrip()` with no argument.
fn rstrip(text: &str) -> &str {
    text.trim_end_matches(pystr::is_space)
}

/// `str.split()` with no argument.
fn split_ws(text: &str) -> Vec<&str> {
    pystr::split_whitespace(text).collect()
}

fn sha12(text: &str) -> String {
    let digest = Sha256::digest(text.as_bytes());
    let mut out = String::with_capacity(12);
    for byte in digest.iter().take(6) {
        let _ = write!(out, "{byte:02x}");
    }
    out
}

/// `_normalize`.
fn normalize(text: &str) -> String {
    let t = text.replace("\r\n", "\n").replace('\r', "\n");
    let mut lines: Vec<&str> = t.split('\n').collect();
    while lines.first().is_some_and(|l| pystr::strip(l).is_empty()) {
        lines.remove(0);
    }
    while lines.last().is_some_and(|l| pystr::strip(l).is_empty()) {
        lines.pop();
    }
    let lines: Vec<String> = lines.iter().map(|l| l.replace('\t', "  ")).collect();
    lines.join("\n") + "\n"
}

/// `_replace_smart_chars`.
fn replace_smart_chars(text: &str, fixes: &mut Fixes) -> String {
    let mut text = text.to_owned();
    for (bad, good) in SMART_QUOTES {
        if text.contains(bad) {
            text = text.replace(bad, &good.to_string());
            fixes.push("smart_quotes");
        }
    }
    for (bad, good) in UNICODE_ARROWS {
        if text.contains(bad) {
            text = text.replace(bad, good);
            fixes.push("unicode_arrows");
        }
    }
    text
}

/// `_detect_header`: the first word of the first line, when it is a
/// diagram type.
fn detect_header(lines: &[String]) -> String {
    let Some(first) = lines.first() else {
        return String::new();
    };
    let first = pystr::strip(first);
    let base = split_ws(first).first().copied().unwrap_or("");
    if HEADER_TYPES.contains(&base) {
        base.to_owned()
    } else {
        String::new()
    }
}

/// `_repair_missing_header`.
fn repair_missing_header(
    lines: &mut Vec<String>,
    cfg: &SanitizerConfig,
    fixes: &mut Fixes,
) -> Result<(), ReError> {
    let joined = lines.iter().take(5).cloned().collect::<Vec<_>>().join("\n");
    if re!(r"^[A-Za-z0-9_\-]+\[.+\]", Flags::M).is_match(&joined)? {
        lines.insert(0, format!("flowchart {}", cfg.default_direction));
        fixes.push("add_header_flowchart");
    } else if re!(r"^[A-Za-z0-9_\-]+-{1,2}>>", Flags::M).is_match(&joined)? {
        lines.insert(0, SEQUENCE.to_owned());
        fixes.push("add_header_sequence");
    }
    Ok(())
}

/// `_ensure_flowchart_direction`.
fn ensure_flowchart_direction(line: &str, cfg: &SanitizerConfig, fixes: &mut Fixes) -> String {
    let parts = split_ws(line);
    if parts.len() == 1 {
        fixes.push("add_direction");
        return format!("{} {}", parts[0], cfg.default_direction);
    }
    if parts.len() > 1
        && (parts[0] == "flowchart" || parts[0] == "graph")
        && !["TD", "LR", "RL", "BT", "TB"].contains(&parts[1])
    {
        fixes.push("replace_direction");
        return format!(
            "{} {} {}",
            parts[0],
            cfg.default_direction,
            parts[1..].join(" ")
        );
    }
    line.to_owned()
}

/// `_quote_labels`.
fn quote_labels(flow_text: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    re!(r"^(?P<id>[A-Za-z0-9_\-]+)\[(?P<label>[^\[]+?)\]", Flags::M).sub_with(flow_text, |m| {
        let node_id = m.name("id");
        let label = rstrip(m.name("label"));
        if label.starts_with('"') && label.ends_with('"') {
            return Ok(m.whole().to_owned());
        }
        if label
            .chars()
            .any(|c| [' ', '(', ')', ':', '.', '/', '\''].contains(&c))
        {
            fixes.push("quote_labels");
            return Ok(format!("{node_id}[\"{label}\"]"));
        }
        Ok(m.whole().to_owned())
    })
}

/// `_quote_arrow_labels`.
fn quote_arrow_labels(flow_text: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    re!(r"-->\s*\|(?P<label>[^\|]+)\|").sub_with(flow_text, |m| {
        let raw = m.name("label");
        let trimmed = pystr::strip(raw);
        if raw != trimmed {
            fixes.push("arrow_label_trim");
        }
        if !(trimmed.starts_with('"') && trimmed.ends_with('"')) {
            fixes.push("arrow_label_quotes");
            return Ok(format!("--> |\"{trimmed}\"|"));
        }
        Ok(format!("--> |{trimmed}|"))
    })
}

fn participant_use_re() -> Result<&'static PyRe, ReError> {
    static CELL: LazyLock<Result<PyRe, ReError>> = LazyLock::new(|| {
        PyRe::new(
            &format!(r"^\s*(?P<lhs>{PID})\s*-{{1,2}}>>\s*(?P<rhs>{PID})"),
            Flags::M,
        )
    });
    CELL.as_ref().map_err(Clone::clone)
}

fn used_participants(text: &str) -> Result<Vec<String>, ReError> {
    let mut used: Vec<String> = Vec::new();
    for m in participant_use_re()?.find_all(text)? {
        for side in ["lhs", "rhs"] {
            let id = m.name(side).to_owned();
            if !used.contains(&id) {
                used.push(id);
            }
        }
    }
    used.sort();
    Ok(used)
}

/// `_add_sequence_participants`.
fn add_sequence_participants(
    text: &str,
    fixes: &mut Fixes,
    cfg: &SanitizerConfig,
) -> Result<String, ReError> {
    if !cfg.auto_add_sequence_participants {
        return Ok(text.to_owned());
    }
    let mut lines = split_lines(text);
    if lines.first().map(|l| pystr::strip(l)) != Some(SEQUENCE) {
        return Ok(text.to_owned());
    }
    let def = re!(r"^\s*participant\s+(?P<id>[A-Za-z0-9_\-]+)\b", Flags::M);
    let declared: HashSet<String> = def
        .find_all(text)?
        .iter()
        .map(|m| m.name("id").to_owned())
        .collect();
    let mut used = used_participants(text)?;
    let mut near_miss: HashMap<String, String> = HashMap::new();
    for u in &used {
        if declared.contains(u) {
            continue;
        }
        let mut candidates: Vec<String> = Vec::new();
        let with = format!("{u}_");
        if declared.contains(&with) {
            candidates.push(with);
        }
        if let Some(base) = u.strip_suffix('_')
            && declared.contains(base)
            && !candidates.iter().any(|c| c == base)
        {
            candidates.push(base.to_owned());
        }
        if candidates.len() == 1 {
            near_miss.insert(u.clone(), candidates.remove(0));
        }
    }
    let mut text = text.to_owned();
    if !near_miss.is_empty() {
        let msg = msg_pattern()?;
        let mut new_lines = Vec::new();
        let mut changed_any = false;
        for ln in text.split('\n') {
            let Some(m) = msg.match_start(ln)? else {
                new_lines.push(ln.to_owned());
                continue;
            };
            let (src, dst) = (m.name("src"), m.name("dst"));
            let new_src = near_miss.get(src).map_or(src, String::as_str);
            let new_dst = near_miss.get(dst).map_or(dst, String::as_str);
            if new_src != src || new_dst != dst {
                changed_any = true;
            }
            new_lines.push(format!(
                "{}{}{}{}{}",
                m.name("indent"),
                new_src,
                m.name("arrow"),
                new_dst,
                m.name("rest")
            ));
        }
        if changed_any {
            text = new_lines.join("\n");
        }
        fixes.push_once("sequence_correct_near_miss_participant");
        used = used_participants(&text)?;
        lines = split_lines(&text);
    }
    let missing: Vec<&String> = used.iter().filter(|u| !declared.contains(*u)).collect();
    if !missing.is_empty() {
        for p in missing.iter().rev() {
            lines.insert(1, format!("participant {p} as {p}"));
        }
        fixes.push("add_participants");
    }
    Ok(lines.join("\n"))
}

fn msg_pattern() -> Result<&'static PyRe, ReError> {
    static CELL: LazyLock<Result<PyRe, ReError>> = LazyLock::new(|| {
        PyRe::new(
            &format!(
                r"^(?P<indent>\s*)(?P<src>{PID})\s*(?P<arrow>-{{1,2}}>>|-->>|->>)\s*(?P<dst>{PID})(?P<rest>\s*:.*)$"
            ),
            Flags::NONE,
        )
    });
    CELL.as_ref().map_err(Clone::clone)
}

/// The two `_rename_flow_reserved_ids` passes (in `_post_normalize` and
/// the flowchart branch): `end["` at a line start becomes `end_["`, with
/// more underscores while the new id is taken.
fn rename_flow_reserved_ids(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let pattern = re!(r#"(^|\n)(?P<indent>\s*)(?P<id>end)\[""#);
    let mut used: HashSet<String> = re!(r"(^|\n)\s*([A-Za-z0-9_\-]+)\[")
        .find_all(t)?
        .iter()
        .map(|m| m.group(2).to_owned())
        .collect();
    let mut changed = false;
    let new_t = pattern.sub_with(t, |m| {
        let mut new_id = format!("{}_", m.name("id"));
        while used.contains(&new_id) || new_id == RESERVED_FLOW {
            new_id.push('_');
        }
        used.insert(new_id.clone());
        changed = true;
        Ok(format!("{}{}{}[\"", m.group(1), m.name("indent"), new_id))
    })?;
    if changed {
        fixes.push("flow_reserved_node_rename");
    }
    Ok(new_t)
}

/// `_post_normalize`.
#[allow(clippy::too_many_lines)]
fn post_normalize(text: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut text = rename_flow_reserved_ids(text, fixes)?;
    for (pattern, template) in [
        (re!(r#"\[""([^"\]]+?)""\]"#), r#"["\1"]"#),
        (re!(r#"\|""([^"|]+?)""\|"#), r#"|"\1"|"#),
        (re!(r#"\|""([^"|]+?)"\|"#), r#"|"\1"|"#),
        (re!(r#"\|"([^"|]+?)""\|"#), r#"|"\1"|"#),
    ] {
        let new_text = pattern.sub(&text, template)?;
        if new_text != text {
            fixes.push("collapse_double_quotes");
            text = new_text;
        }
    }
    // Literal `\n` inside quoted node labels → <br/>.
    let new_text = re!(r#"(\[["])([^\]]*?)(\])"#).sub_with(&text, |m| {
        let inner = m.group(2);
        if inner.contains("\\n") {
            let replaced = inner.replace("\\n", "<br/>");
            if replaced != inner {
                return Ok(format!("{}{}{}", m.group(1), replaced, m.group(3)));
            }
        }
        Ok(m.whole().to_owned())
    })?;
    if new_text != text {
        fixes.push("label_newline_br");
        text = new_text;
    }
    // Arrow label inner quote cleanup.
    text = re!(r#"\|"(.*?)"\|"#).sub_with(&text, |m| {
        let inner = m.group(1);
        let mut cleaned = re!(r#"^"+"#).sub(inner, "")?;
        cleaned = re!(r#""+$"#).sub(&cleaned, "")?;
        cleaned = re!(r#""([?!.,;])"#).sub(&cleaned, r"\1")?;
        if cleaned != inner {
            fixes.push("arrow_label_inner_quote_cleanup");
            return Ok(format!("|\"{cleaned}\"|"));
        }
        Ok(m.whole().to_owned())
    })?;
    // Node label inner quote cleanup.
    text = re!(r#"\["((?:[^"]|"(?!\]))*)"\s*\]"#).sub_with(&text, |m| {
        let inner = m.group(1);
        let special = re!(r#"^"+Return\s+\[\"\]"?$"#).sub(inner, "Return []")?;
        if special != inner {
            fixes.push_once("collapse_extra_label_quotes");
            fixes.push("node_label_inner_quote_cleanup");
            return Ok(format!("[\"{special}\"]"));
        }
        let mut cleaned = re!(r#"^"+"#).sub(inner, "")?;
        cleaned = re!(r#""+$"#).sub(&cleaned, "")?;
        cleaned = re!(r#""([?!.,;])"#).sub(&cleaned, r"\1")?;
        let cleaned2 = re!(r#"Return\s+\[(?:"\"|"|\"|)\]"#).sub(&cleaned, "Return []")?;
        if cleaned2 != cleaned {
            cleaned = cleaned2;
            fixes.push_once("collapse_extra_label_quotes");
        }
        if cleaned != inner {
            fixes.push("node_label_inner_quote_cleanup");
            fixes.push_once("collapse_extra_label_quotes");
            return Ok(format!("[\"{cleaned}\"]"));
        }
        Ok(m.whole().to_owned())
    })?;
    // Second stage: Return ["] inside a quoted label.
    text = re!(r#"\["((?:[^"]|"(?!\]))*)"\]"#).sub_with(&text, |m| {
        let inner = m.group(1);
        let new_inner = re!(r#"Return\s+\["\]"#).sub(inner, "Return []")?;
        if new_inner != inner {
            fixes.push_once("collapse_extra_label_quotes");
            return Ok(format!("[\"{new_inner}\"]"));
        }
        Ok(m.whole().to_owned())
    })?;
    if text.contains("sequenceDiagram") {
        text = split_multi_deactivate(&text, fixes)?;
        text = remove_unmatched_deactivates(&text, fixes)?;
        let new_text = re!(r"(?m)^\s*return\s*$").sub(&text, "")?;
        if new_text != text {
            fixes.push("sequence_remove_standalone_return");
            text = non_blank_lines(&new_text);
        }
        let new_text = re!(r"(?m)^\s*break\s*$").sub(&text, "")?;
        if new_text != text {
            fixes.push("sequence_remove_standalone_break");
            text = non_blank_lines(&new_text);
        }
        let new_lines: Vec<String> = text
            .split('\n')
            .map(|ln| seq_semicolon(ln, fixes))
            .collect();
        text = new_lines.join("\n");
    }
    if text.contains("deactivate ") && text.contains("alt ") {
        text = collapse_duplicate_deactivate(&text, fixes)?;
    }
    Ok(text)
}

/// `'\n'.join([ln for ln in t.split('\n') if ln.strip() != ''])`.
fn non_blank_lines(text: &str) -> String {
    text.split('\n')
        .filter(|ln| !pystr::strip(ln).is_empty())
        .collect::<Vec<_>>()
        .join("\n")
}

/// `_split_multi_deactivate`.
fn split_multi_deactivate(text: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let pattern = re!(
        r"^\s*deactivate\s+([A-Za-z0-9_\-]+\s*,\s*[A-Za-z0-9_\-]+(?:\s*,\s*[A-Za-z0-9_\-]+)*)\s*$"
    );
    let mut out = Vec::new();
    let mut changed = false;
    for ln in text.split('\n') {
        if let Some(m) = pattern.match_start(ln)? {
            for part in m.group(1).split(',') {
                let part = pystr::strip(part);
                if !part.is_empty() {
                    out.push(format!("deactivate {part}"));
                }
            }
            changed = true;
        } else {
            out.push(ln.to_owned());
        }
    }
    if changed {
        fixes.push("sequence_split_multi_deactivate");
    }
    Ok(out.join("\n"))
}

/// `_remove_unmatched_deactivates`.
fn remove_unmatched_deactivates(text: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let act = re!(r"^\s*activate\s+([A-Za-z0-9_\-]+)\s*$");
    let de = re!(r"^\s*deactivate\s+([A-Za-z0-9_\-]+)\s*$");
    let mut active: HashSet<String> = HashSet::new();
    let mut result = Vec::new();
    let mut changed = false;
    for ln in text.split('\n') {
        if let Some(am) = act.match_start(ln)? {
            active.insert(am.group(1).to_owned());
            result.push(ln);
            continue;
        }
        if let Some(dm) = de.match_start(ln)?
            && !active.contains(dm.group(1))
        {
            changed = true;
            continue;
        }
        result.push(ln);
    }
    if changed {
        fixes.push("sequence_remove_unmatched_deactivate");
    }
    Ok(result.join("\n"))
}

/// Split `text` on `;` outside double quotes, trimming and dropping empty
/// segments (`_seq_semicolon` / `_split_note_semicolons`).
fn semicolon_segments(body: &str) -> Vec<String> {
    let mut segments = Vec::new();
    let mut current = String::new();
    let mut in_quotes = false;
    for ch in body.chars() {
        if ch == '"' {
            in_quotes = !in_quotes;
        }
        if ch == ';' && !in_quotes {
            let trimmed = pystr::strip(&current);
            if !trimmed.is_empty() {
                segments.push(trimmed.to_owned());
            }
            current.clear();
        } else {
            current.push(ch);
        }
    }
    let trimmed = pystr::strip(&current);
    if !trimmed.is_empty() {
        segments.push(trimmed.to_owned());
    }
    segments
}

/// `_seq_semicolon`.
fn seq_semicolon(line: &str, fixes: &mut Fixes) -> String {
    // `':' not in line or '->' not in line and …` — the other two arrows
    // contain `->`.
    if !line.contains(':') || !line.contains("->") {
        return line.to_owned();
    }
    let Some((prefix, msg)) = line.split_once(':') else {
        return line.to_owned();
    };
    if !msg.contains(';') {
        return line.to_owned();
    }
    let segments = semicolon_segments(msg);
    if segments.len() > 1 {
        let replaced = segments.join("<br/>");
        if replaced != pystr::strip(msg) {
            fixes.push("sequence_semicolon_linebreak");
            return format!("{prefix}: {replaced}");
        }
    }
    line.to_owned()
}

/// The duplicate-`deactivate` collapse at the end of `_post_normalize`.
fn collapse_duplicate_deactivate(text: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let pattern = re!(r"^\s*deactivate\s+([A-Za-z0-9_\-]+)\s*$");
    let mut lines = split_lines(text);
    let mut last: HashMap<String, usize> = HashMap::new();
    for (i, l) in lines.iter().enumerate() {
        if let Some(m) = pattern.match_start(l)? {
            last.insert(m.group(1).to_owned(), i);
        }
    }
    if last.is_empty() {
        return Ok(text.to_owned());
    }
    let mut changed_any = false;
    for (i, line) in lines.iter_mut().enumerate() {
        let target = pattern.match_start(line)?.map(|m| m.group(1).to_owned());
        if let Some(name) = target
            && last.get(&name) != Some(&i)
        {
            line.clear();
            changed_any = true;
        }
    }
    if changed_any {
        fixes.push("collapse_duplicate_deactivate");
        return Ok(lines
            .into_iter()
            .filter(|l| !l.is_empty())
            .collect::<Vec<_>>()
            .join("\n"));
    }
    Ok(text.to_owned())
}

/// `_auto_nodes_for_orphan_labels`: `A --> |"Yes"| "Some Node"` gets a
/// node id slugged from the label.
fn auto_nodes_for_orphan_labels(flow: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut lines = split_lines(flow);
    let mut generated: HashSet<String> = HashSet::new();
    let id_pattern = re!(r"^[A-Za-z0-9_]+$");
    let existing: HashSet<String> = re!(r#"(^|\n)\s*([A-Za-z0-9_]+)\[""#)
        .find_all(flow)?
        .iter()
        .map(|m| m.group(2).to_owned())
        .collect();
    let orphan = re!(
        r#"^(?P<indent>\s*)(?P<src>[A-Za-z0-9_]+)\s*-->\s*\|"(?P<edge>[^"]+)"\|\s*"(?P<label>[^"\n]+)"\s*$"#
    );
    let mut changed = false;
    for line in &mut lines {
        let Some(m) = orphan.match_start(line)? else {
            continue;
        };
        let node_label = pystr::strip(m.name("label")).to_owned();
        let source = node_label.replace("()", "");
        let decomposed: String = source.nfkd().collect();
        let underscored = re!(r"[^A-Za-z0-9]+").sub(&decomposed, "_")?;
        let mut base = pystr::strip_chars(&underscored, "_").to_lowercase();
        if base.is_empty() {
            "auto".clone_into(&mut base);
        }
        let base: String = base.chars().take(20).collect();
        let mut candidate = base.clone();
        let mut i = 1;
        while existing.contains(&candidate)
            || generated.contains(&candidate)
            || !id_pattern.is_match(&candidate)?
        {
            candidate = format!("{base}_{i}");
            i += 1;
        }
        generated.insert(candidate.clone());
        let new_line = format!(
            "{}{} --> |\"{}\"| {}[\"{}\"]",
            m.name("indent"),
            m.name("src"),
            m.name("edge"),
            candidate,
            node_label
        );
        *line = new_line;
        fixes.push("auto_node_from_orphan_label");
        changed = true;
    }
    Ok(if changed {
        lines.join("\n")
    } else {
        flow.to_owned()
    })
}

/// `_unwrap_decision_quotes`: `id{"Label"}` → `id{Label}`.
fn unwrap_decision_quotes(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut changed = false;
    let new_t =
        re!(r#"(?P<id>\b[A-Za-z0-9_\-]+)\{\s*"(?P<label>[^"\n]+)"\s*\}"#).sub_with(t, |m| {
            changed = true;
            Ok(format!("{}{{{}}}", m.name("id"), m.name("label")))
        })?;
    if changed {
        fixes.push("flow_decision_unwrap_quotes");
    }
    Ok(new_t)
}

/// `_avoid_reserved_aliases`.
fn avoid_reserved_aliases(seq_text: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut lines = split_lines(seq_text);
    // Insertion-ordered (a Python dict).
    let mut alias_map: Vec<(String, String)> = Vec::new();
    for l in &mut lines {
        let ls = pystr::strip(l);
        if !ls.to_lowercase().starts_with("participant ") {
            continue;
        }
        let rest: String = ls.chars().skip("participant ".len()).collect();
        if rest.is_empty() {
            continue;
        }
        let Some(first) = split_ws(&rest).first().map(|f| (*f).to_owned()) else {
            continue;
        };
        if !RESERVED_SEQ_KEYWORDS.contains(&first.to_lowercase().as_str()) {
            continue;
        }
        let mut base = format!("{first}_");
        while RESERVED_SEQ_KEYWORDS.contains(&base.to_lowercase().as_str())
            || alias_map.iter().any(|(_, v)| *v == base)
        {
            base.push('_');
        }
        match alias_map.iter_mut().find(|(k, _)| *k == first) {
            Some(entry) => entry.1.clone_from(&base),
            None => alias_map.push((first.clone(), base.clone())),
        }
        *l = l.replacen(
            &format!("participant {first}"),
            &format!("participant {base}"),
            1,
        );
    }
    if alias_map.is_empty() {
        return Ok(seq_text.to_owned());
    }
    let mut new_text = lines.join("\n");
    for (old, new) in &alias_map {
        let sender = PyRe::new(
            &format!(r"(?m)(^|\n)([^\n]*?)\b{old}\b(?=-{{1,2}}>>)"),
            Flags::I,
        )?;
        let inner = PyRe::new(&format!(r"\b{old}\b(?=-{{1,2}}>>)"), Flags::NONE)?;
        new_text = sender.sub_with(&new_text, |m| inner.sub(m.whole(), new))?;
        let receiver = PyRe::new(&format!(r"(-{{1,2}}>>\s*){old}(?=\s*:)"), Flags::I)?;
        new_text = receiver.sub(&new_text, &format!(r"\1{new}"))?;
    }
    fixes.push("reserved_participant_alias");
    Ok(new_text)
}

/// `_split_note_semicolons`.
fn split_note_semicolons(seq_t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let note = re!(
        r"^(?P<prefix>\s*Note\s+(?:right|left|over)\s+[^:]+:\s*)(?P<body>.+)$",
        Flags::I
    );
    let mut out = Vec::new();
    let mut changed = false;
    for ln in seq_t.split('\n') {
        let Some(m) = note.match_start(ln)? else {
            out.push(ln.to_owned());
            continue;
        };
        let segments = semicolon_segments(m.name("body"));
        if segments.len() > 1 {
            out.push(format!("{}{}", m.name("prefix"), segments.join("<br/>")));
            fixes.push("sequence_note_semicolon_linebreak");
            changed = true;
        } else {
            out.push(ln.to_owned());
        }
    }
    Ok(if changed {
        out.join("\n")
    } else {
        seq_t.to_owned()
    })
}

/// `_align_near_miss_aliases`.
fn align_near_miss_aliases(seq_t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let declared: Vec<String> = re!(r"^\s*participant\s+(?P<id>[A-Za-z0-9_\-]+)\b", Flags::M)
        .find_all(seq_t)?
        .iter()
        .map(|m| m.name("id").to_owned())
        .collect();
    let mut base_to_declared: HashMap<String, String> = HashMap::new();
    for did in &declared {
        if did.ends_with('_') {
            let base = did.trim_end_matches('_').to_owned();
            if !declared.contains(&base) && !base_to_declared.contains_key(&base) {
                base_to_declared.insert(base, did.clone());
            }
        }
    }
    if base_to_declared.is_empty() {
        return Ok(seq_t.to_owned());
    }
    let line_re = re!(
        r"^(?P<indent>\s*)(?P<src>[A-Za-z0-9_\-]+)\s*(?P<arrow>-{1,2}>>|-->>|->>)\s*(?P<dst>[A-Za-z0-9_\-]+)(?P<rest>\s*:.*)$"
    );
    let mut changed = false;
    let mut new_lines = Vec::new();
    for ln in seq_t.split('\n') {
        let Some(m) = line_re.match_start(ln)? else {
            new_lines.push(ln.to_owned());
            continue;
        };
        let (src, dst) = (m.name("src"), m.name("dst"));
        let src2 = base_to_declared.get(src).map_or(src, String::as_str);
        let dst2 = base_to_declared.get(dst).map_or(dst, String::as_str);
        if src2 != src || dst2 != dst {
            changed = true;
        }
        new_lines.push(format!(
            "{}{}{}{}{}",
            m.name("indent"),
            src2,
            m.name("arrow"),
            dst2,
            m.name("rest")
        ));
    }
    if changed {
        fixes.push_once("sequence_correct_near_miss_participant");
    }
    Ok(new_lines.join("\n"))
}

/// `strip_outer_quotes` / `stripq`: `^["']?(.*?)["']?$` → `\1`.
fn strip_outer_quotes(tok: &str) -> Result<String, ReError> {
    re!(r#"^["\']?(.*?)["\']?$"#).sub(tok, r"\1")
}

/// Split at top-level commas (outside brackets and quotes), each part
/// stripped — the scanner of `_normalize_generics` and `fix_group`.
fn top_level_commas(content: &[char]) -> Vec<String> {
    let mut tokens = Vec::new();
    let mut buf = String::new();
    let mut d = 0usize;
    let mut inq = false;
    let mut q = ' ';
    for &ch in content {
        if inq {
            if ch == q {
                inq = false;
            }
            buf.push(ch);
            continue;
        }
        if ch == '"' || ch == '\'' {
            inq = true;
            q = ch;
            buf.push(ch);
            continue;
        }
        if ch == '[' {
            d += 1;
            buf.push(ch);
            continue;
        }
        if ch == ']' {
            d = d.saturating_sub(1);
            buf.push(ch);
            continue;
        }
        if ch == ',' && d == 0 {
            tokens.push(pystr::strip(&buf).to_owned());
            buf.clear();
            continue;
        }
        buf.push(ch);
    }
    if !pystr::strip(&buf).is_empty() {
        tokens.push(pystr::strip(&buf).to_owned());
    }
    tokens
}

/// `_normalize_generics` inside `fix_inner`.
#[allow(clippy::many_single_char_names)] // Python's `s`, `n`, `i`, `j`, `m`
fn normalize_generics(s: &str) -> Result<(String, bool), ReError> {
    let chars = chars(s);
    let n = chars.len();
    // `re.match(p, s[i:])` is `p.match(s, i)` here: the pattern has no
    // `^` and no look-behind.
    let offsets: Vec<usize> = s.char_indices().map(|(at, _)| at).collect();
    let head = re!(r"([A-Za-z0-9_]+)\[");
    let mut out = String::new();
    let mut changed = false;
    let mut i = 0;
    while i < n {
        let Some(m) = head.match_at(s, offsets[i])? else {
            out.push(chars[i]);
            i += 1;
            continue;
        };
        let type_name = m.group(1).to_owned();
        let start = i + char_len(m.whole());
        let mut j = start;
        let mut depth = 1i64;
        let mut in_q = false;
        let mut qch = ' ';
        while j < n {
            let ch = chars[j];
            if in_q {
                if ch == qch {
                    in_q = false;
                } else if ch == '\\' && j + 1 < n {
                    j += 1;
                }
                j += 1;
                continue;
            }
            if ch == '"' || ch == '\'' {
                in_q = true;
                qch = ch;
                j += 1;
                continue;
            }
            if ch == '[' {
                depth += 1;
                j += 1;
                continue;
            }
            if ch == ']' {
                depth -= 1;
                j += 1;
                if depth == 0 {
                    break;
                }
                continue;
            }
            j += 1;
        }
        if depth != 0 {
            out.push(chars[i]);
            i += 1;
            continue;
        }
        let content: &[char] = if j > start { &chars[start..j - 1] } else { &[] };
        let tokens = top_level_commas(content);
        if tokens.len() >= 2 {
            let cleaned: Result<Vec<String>, ReError> = tokens
                .iter()
                .map(|t| strip_outer_quotes(pystr::strip(t)))
                .collect();
            let _ = write!(out, "{type_name}[{}]", cleaned?.join(", "));
            i = j;
            changed = true;
        } else {
            out.push(chars[i]);
            i += 1;
        }
    }
    Ok((out, changed))
}

/// `fix_inner` of `_label_inner_normalize`.
fn fix_inner(inner: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut inner = inner.to_owned();
    let mut changed_local = false;
    for (pattern, template) in [
        (re!(r#"\[\s*\"\s*\'([^\']+)\'\s*\]"#), r"['\1']"),
        (re!(r#"\[\s*\"([^\"\]\n]+)\"\s*\]"#), r"['\1']"),
        (re!(r#"\[\s*\\"([^"\\\]]+)\\"\s*\]"#), r"['\1']"),
        (re!(r#"\[([^\]"\n]+)"\]"#), r"[\1]"),
        (re!(r#"\["([^\]"\n]+)\]"#), r"[\1]"),
        (re!(r#"\\"([^"\\]+)\\""#), r"'\1'"),
        (re!(r#""\s*(\[\s*\'[^\]]+\'\s*\])"#), r"\1"),
    ] {
        let new_inner = pattern.sub(&inner, template)?;
        if new_inner != inner {
            changed_local = true;
        }
        inner = new_inner;
    }
    let (new_inner, gen_changed) = normalize_generics(&inner)?;
    if gen_changed {
        inner = new_inner;
        changed_local = true;
        fixes.push_once("label_generic_bracket_tokens_unquote");
    }
    let new_inner = re!(r#"(\[|,\s*)"([^"\[\],]+)"(?=\s*(?:,|\]))"#).sub(&inner, r"\1\2")?;
    if new_inner != inner {
        inner = new_inner;
        changed_local = true;
        fixes.push_once("label_generic_bracket_tokens_unquote");
    }
    if changed_local {
        fixes.push_once("label_index_quote_single");
    }
    Ok(inner)
}

/// `_label_inner_normalize`.
#[allow(clippy::many_single_char_names)] // Python's `t`, `n`, `i`, `j`, `k`
fn label_inner_normalize(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let chars = chars(t);
    let n = chars.len();
    let mut res = String::with_capacity(t.len());
    let mut i = 0;
    while i < n {
        if chars[i] == '[' && i + 1 < n && chars[i + 1] == '"' {
            let mut j = i + 2;
            let mut found = false;
            let mut depth = 0usize;
            while j < n {
                let ch = chars[j];
                let prev = chars[j - 1];
                if ch == '[' && prev != '\\' {
                    depth += 1;
                    j += 1;
                    continue;
                }
                if ch == ']' && prev != '\\' && depth > 0 {
                    depth -= 1;
                    j += 1;
                    continue;
                }
                if ch == '"' && prev != '\\' && depth == 0 {
                    let mut k = j + 1;
                    while k < n && (chars[k] == ' ' || chars[k] == '\t') {
                        k += 1;
                    }
                    if k < n && chars[k] == ']' {
                        let inner: String = chars[i + 2..j].iter().collect();
                        let fixed = fix_inner(&inner, fixes)?;
                        res.push_str("[\"");
                        res.push_str(&fixed);
                        res.push_str("\"]");
                        i = k + 1;
                        found = true;
                        break;
                    }
                }
                j += 1;
            }
            if !found {
                res.push(chars[i]);
                i += 1;
            }
        } else {
            res.push(chars[i]);
            i += 1;
        }
    }
    let t3 = re!(r#"\|\"(.+?)\"\|"#).sub_with(&res, |m| {
        let mut inner = m.group(1).to_owned();
        let mut changed = false;
        for (pattern, template) in [
            // The template is the LITERAL text '\1' (Python quirk, kept).
            (re!(r#"\\"([^\\"]+?)\\""#), r"'\\1'"),
            (re!(r#"\[(\s)*"([^"\]]+?)"(\s*)\]"#), r"[\1'\2'\3]"),
            (re!(r#"\[\s*\"\s*\'([^\']+)\'\s*\"\s*\]"#), r"['\1']"),
            (re!(r#"\[([^\"\n]+)\"\]"#), r"[\1]"),
            (re!(r#"\[\"([^\"\n]+)\]"#), r"[\1]"),
        ] {
            let inner2 = pattern.sub(&inner, template)?;
            if inner2 != inner {
                changed = true;
            }
            inner = inner2;
        }
        if changed {
            fixes.push_once("label_escaped_dquote_to_single");
            return Ok(format!("|\"{inner}\"|"));
        }
        Ok(m.whole().to_owned())
    })?;
    re!(r#"\[\s*\"\s*\'([^\']+)\'\s*\"\s*\]"#).sub(&t3, r"['\1']")
}

/// Unquote the comma-separated parts of a generic's brackets
/// (`gen_repl` / `_generic_repl`): strip each part, then one leading and
/// one trailing quote.
fn unquote_generic(type_name: &str, content: &str) -> Result<String, ReError> {
    let mut cleaned = Vec::new();
    for part in content.split(',') {
        let p = pystr::strip(part);
        let p2 = re!(r#"^["\']"#).sub(p, "")?;
        let p2 = re!(r#"["\']$"#).sub(&p2, "")?;
        cleaned.push(p2);
    }
    Ok(format!("{type_name}[{}]", cleaned.join(", ")))
}

/// `_normalize_label_generics`.
fn normalize_label_generics(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    re!(r#"(?P<id>[A-Za-z0-9_\-]+)\["(?P<inner>(?:[^"\\]|\\.(?!\]))*)"\]"#).sub_with(t, |m| {
        let inner = m.name("inner").replace("\\\"", "\"");
        let inner = re!(r#"([A-Za-z0-9_])"\["#).sub(&inner, r"\1[")?;
        let new_inner = re!(r"([A-Za-z0-9_]+)\[\s*([^\[\]]*?,[^\[\]]*?)\s*\]")
            .sub_with(&inner, |g| {
                unquote_generic(g.group(1), &g.group(2).replace("\\\"", "\""))
            })?;
        if new_inner != inner {
            fixes.push_once("label_generic_bracket_tokens_unquote");
        }
        Ok(format!("{}[\"{}\"]", m.name("id"), new_inner))
    })
}

/// `_late_label_quote`: quote `id[label]` outside double quotes when the
/// label has a space or punctuation.
fn late_label_quote(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let pat = re!(r#"(?P<id>[A-Za-z0-9_\-]+)\[(?P<label>(?!")[^"\]\n]+)\]"#);
    let triggers: Vec<char> = " ()/:.<>{}!?\"".chars().collect();
    let mut changed = false;
    let mut out_lines = Vec::new();
    for line in t.split('\n') {
        let mut res = String::with_capacity(line.len());
        let mut pos = 0;
        let mut in_q = false;
        while pos < line.len() {
            let Some(ch) = line[pos..].chars().next() else {
                break;
            };
            if ch == '"' {
                in_q = !in_q;
                res.push(ch);
                pos += 1;
                continue;
            }
            if !in_q && let Some(m) = pat.match_at(line, pos)? {
                let lab = m.name("label");
                if lab.chars().any(|c| triggers.contains(&c)) {
                    let _ = write!(res, "{}[\"{}\"]", m.name("id"), lab);
                    pos = m.end();
                    changed = true;
                    continue;
                }
            }
            res.push(ch);
            pos += ch.len_utf8();
        }
        out_lines.push(res);
    }
    if changed {
        fixes.push("late_label_quote");
    }
    Ok(out_lines.join("\n"))
}

/// `_merge_fragments`.
fn merge_fragments(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut changed = false;
    let new_t = re!(r#"\["([^"\]]+)"<br/>"([^"\]]+)"\]"#).sub_with(t, |m| {
        changed = true;
        Ok(format!("[\"{}<br/>{}\"]", m.group(1), m.group(2)))
    })?;
    let new_t = re!(r#"\|"([^"|]+)"<br/>"([^"|]+)"\|"#).sub_with(&new_t, |m| {
        changed = true;
        Ok(format!("|\"{}<br/>{}\"|", m.group(1), m.group(2)))
    })?;
    if changed {
        fixes.push("merge_fragmented_multiline_label");
    }
    Ok(new_t)
}

/// `_normalize_inner_quotes`, quirks included (see the module comment).
fn normalize_inner_quotes(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut changed = false;
    let t2 = re!(r#"\["((?:[^\"]|\"(?!\]))*)"\]"#).sub_with(t, |m| {
        let inner = m.group(1);
        let mut new_inner = re!(r#"\\"([^\\"]+?)\\""#).sub(inner, r"'\\1'")?;
        let converted =
            re!(r#"(\{'[^']+'\s*:\s*)\\\"([^\\\"]+)\\\""#).sub(&new_inner, r"\1'\2'")?;
        let converted2 = re!(r#"(\{'[^']+'\s*:\s*)\"([^\"\n]+)\""#).sub(&converted, r"\1'\2'")?;
        if converted2 != new_inner {
            new_inner = converted2;
        }
        if converted != new_inner {
            new_inner = converted;
            let new_inner2 = re!(r"([A-Za-z0-9_]+)\[\s*([^\[\]]*?,[^\[\]]*?)\s*\]")
                .sub_with(&new_inner, |g| unquote_generic(g.group(1), g.group(2)))?;
            if new_inner2 != new_inner {
                new_inner = new_inner2;
                changed = true;
                fixes.push_once("label_generic_bracket_tokens_unquote");
            }
        }
        if new_inner != inner {
            changed = true;
            return Ok(format!("[\"{new_inner}\"]"));
        }
        Ok(m.whole().to_owned())
    })?;
    let t3 = re!(r#"\|"([^"|]+)"\|"#).sub_with(&t2, |m| {
        let inner = m.group(1);
        let new_inner = re!(r#"\\"([^\\"]+?)\\""#).sub(inner, r"'\\1'")?;
        if new_inner != inner {
            changed = true;
            return Ok(format!("|\"{new_inner}\"|"));
        }
        Ok(m.whole().to_owned())
    })?;
    let t4 = re!(r#""([^"\n]*\{[^}]*\}[^"\n]*?)""#).sub_with(&t3, |m| {
        let converted = re!(r#"(\{'[^']+'\s*:\s*)\"([^\"\n]+)\""#).sub(m.group(1), r"\1'\2'")?;
        Ok(format!("\"{converted}\""))
    })?;
    if t4 != t3 && !fixes.contains("normalize_inner_quotes") {
        fixes.push("normalize_inner_quotes");
        return Ok(t4);
    }
    if changed {
        fixes.push_once("normalize_inner_quotes");
    }
    Ok(t3)
}

/// `_subgraph_rewrite`: edges that name a subgraph go to its first node
/// (or `{id}_entry`).
fn subgraph_rewrite(flow: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let lines = split_lines(flow);
    let start = re!(r#"^\s*subgraph\s+([A-Za-z0-9_\-]+)(?:\["[^\]]+"\])?"#);
    let end = re!(r"^\s*end\s*$");
    let node = re!(r#"^\s*([A-Za-z0-9_\-]+)\[""#);
    let mut reps: Vec<(String, Option<String>)> = Vec::new();
    let mut inside: Option<String> = None;
    for ln in &lines {
        if let Some(m) = start.match_start(ln)? {
            let id = m.group(1).to_owned();
            if !reps.iter().any(|(k, _)| *k == id) {
                reps.push((id.clone(), None));
            }
            inside = Some(id);
            continue;
        }
        if inside.is_some() && end.match_start(ln)?.is_some() {
            inside = None;
            continue;
        }
        if let Some(current) = &inside
            && let Some(entry) = reps.iter_mut().find(|(k, _)| k == current)
            && entry.1.is_none()
            && let Some(n) = node.match_start(ln)?
        {
            entry.1 = Some(n.group(1).to_owned());
        }
    }
    let reps: Vec<(String, String)> = reps
        .into_iter()
        .map(|(sg, rep)| {
            let rep = rep
                .filter(|r| !r.is_empty())
                .unwrap_or_else(|| format!("{sg}_entry"));
            (sg, rep)
        })
        .collect();
    let mut compiled = Vec::new();
    for (sg, rep) in &reps {
        let sg_escaped = escape(sg);
        compiled.push((
            PyRe::new(&format!(r"(^|\s)({sg_escaped})(\s*-->)"), Flags::NONE)?,
            PyRe::new(
                &format!(r#"(-->|\|"[^"|]+"\|\s*)({sg_escaped})(\s*\[)"#),
                Flags::NONE,
            )?,
            format!(r"\1{rep}\3"),
        ));
    }
    let mut changed = false;
    let mut out = Vec::new();
    for ln in lines {
        let mut ln = ln;
        for (src, dst, template) in &compiled {
            let ln2 = src.sub(&ln, template)?;
            let ln2 = dst.sub(&ln2, template)?;
            if ln2 != ln {
                changed = true;
            }
            ln = ln2;
        }
        out.push(ln);
    }
    if changed {
        fixes.push("subgraph_id_edge_rewrite");
    }
    Ok(out.join("\n"))
}

/// `_opt_to_alt_if_else`.
fn opt_to_alt_if_else(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let lines = split_lines(t);
    let opt = re!(r"^\s*opt\b(.*)$");
    let end = re!(r"^\s*end\s*$");
    let else_ = re!(r"^\s*else\b");
    let mut out = Vec::new();
    let mut changed = false;
    for (i, ln) in lines.iter().enumerate() {
        if let Some(m) = opt.match_start(ln)? {
            let mut saw_else = false;
            let mut j = i + 1;
            while j < lines.len() && end.match_start(&lines[j])?.is_none() {
                if else_.match_start(&lines[j])?.is_some() {
                    saw_else = true;
                    break;
                }
                j += 1;
            }
            if saw_else {
                out.push(format!("alt{}", m.group(1)));
                changed = true;
            } else {
                out.push(ln.clone());
            }
        } else {
            out.push(ln.clone());
        }
    }
    if changed {
        fixes.push("sequence_opt_to_alt_for_else");
    }
    Ok(out.join("\n"))
}

/// The comma / parenthesis spacing of a sequence message.
fn message_spacing(text: &str) -> Result<String, ReError> {
    let joined = re!(r"\s+,").sub(text, ",")?;
    let joined = re!(r",\s*").sub(&joined, ", ")?;
    let joined = re!(r"\(\s+").sub(&joined, "(")?;
    re!(r"\s+\)").sub(&joined, ")")
}

/// The sequence-only passes after `_subgraph_rewrite`.
fn sequence_tail(text2: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let mut text2 = opt_to_alt_if_else(text2, fixes)?;
    // _collapse_line_continuations
    let mut changed = false;
    let new_t = re!(
        r"^(?P<lhs>[A-Za-z0-9_\-]+)\s*(?P<arrow>-{1,2}>>|-->>|->>)\s*(?P<rhs>[A-Za-z0-9_\-]+):\s*(?P<msg>.+)$",
        Flags::M
    )
    .sub_with(&text2, |m| {
        let msg = m.name("msg");
        let joined = re!(r"\\\s*\n\s*").sub(msg, " ")?;
        let joined = re!(r"\s+,").sub(&joined, ",")?;
        let joined = re!(r",\s*").sub(&joined, ", ")?;
        let joined = re!(r"\(\s+").sub(&joined, "(")?;
        let joined = re!(r"\s+\)").sub(&joined, ")")?;
        if joined != msg {
            changed = true;
        }
        Ok(format!(
            "{}{}{}: {}",
            m.name("lhs"),
            m.name("arrow"),
            m.name("rhs"),
            joined
        ))
    })?;
    if changed {
        fixes.push("sequence_label_line_continuation_collapse");
    }
    text2 = new_t;
    let seq_collapse = re!(r"\\\s*\n\s*").sub(&text2, " ")?;
    if seq_collapse != text2 {
        text2 = seq_collapse;
        fixes.push_once("sequence_label_line_continuation_collapse");
    }
    // _norm_msg_spaces
    text2 = re!(r"^(.*?->>.*?:\s*)(.+)$", Flags::M).sub_with(&text2, |m| {
        let msg2 = re!(r"\(\s+").sub(m.group(2), "(")?;
        let msg2 = re!(r"\s+\)").sub(&msg2, ")")?;
        let msg2 = re!(r"\s+,").sub(&msg2, ",")?;
        let msg2 = re!(r",\s*").sub(&msg2, ", ")?;
        Ok(format!("{}{}", m.group(1), msg2))
    })?;
    join_multiline_paren(&text2)
}

/// `_join_multiline_paren`.
fn join_multiline_paren(seq_t: &str) -> Result<String, ReError> {
    let lines = split_lines(seq_t);
    let msg_re = re!(r"^(?P<head>\s*[A-Za-z0-9_\-]+\s*-{1,2}>>\s*[A-Za-z0-9_\-]+:\s*)(?P<msg>.+)$");
    let mut out = Vec::new();
    let mut i = 0;
    let mut changed = false;
    while i < lines.len() {
        let ln = &lines[i];
        let Some(m) = msg_re.match_start(ln)? else {
            out.push(ln.clone());
            i += 1;
            continue;
        };
        let msg = m.name("msg");
        let opens = msg.matches('(').count();
        let closes = msg.matches(')').count();
        if msg.contains('(') && opens > closes {
            let mut buf = vec![msg.to_owned()];
            let mut bal =
                i64::try_from(opens).unwrap_or(i64::MAX) - i64::try_from(closes).unwrap_or(0);
            let mut j = i + 1;
            while j < lines.len() && bal > 0 {
                let seg = pystr::strip(&lines[j]).to_owned();
                bal += i64::try_from(seg.matches('(').count()).unwrap_or(0)
                    - i64::try_from(seg.matches(')').count()).unwrap_or(0);
                buf.push(seg);
                j += 1;
            }
            let joined = message_spacing(&buf.join(" "))?;
            out.push(format!("{}{}", m.name("head"), joined));
            i = j;
            changed = true;
        } else {
            out.push(ln.clone());
            i += 1;
        }
    }
    Ok(if changed {
        out.join("\n")
    } else {
        seq_t.to_owned()
    })
}

/// `_final_flow_label_cleanup`.
fn final_flow_label_cleanup(t: &str, fixes: &mut Fixes) -> Result<String, ReError> {
    let before = t.to_owned();
    let mut t = re!(r#"(\[[ \t]*)"{2,}"#).sub(t, r#"\1""#)?;
    t = re!(r#""{2,}(\])"#).sub(&t, r#""\1"#)?;
    let t2 = re!(r#"\["Return\s+\["\]"\]"#).sub(&t, r#"["Return []"]"#)?;
    if t2 != t {
        t = t2;
    }
    let t3 = re!(r#"(?P<id>[A-Za-z0-9_\-]+)\["(?P<inner>[^"\]\n]+)"\]"#).sub_with(&t, |m| {
        let inner = m.name("inner");
        let new_inner = re!(r#"Return\s+\["\]"#).sub(inner, "Return []")?;
        if new_inner != inner {
            return Ok(format!("{}[\"{}\"]", m.name("id"), new_inner));
        }
        Ok(m.whole().to_owned())
    })?;
    if t3 != t {
        t = t3;
        fixes.push_once("collapse_extra_label_quotes");
    }
    let t3 = re!(r#"(?P<id>[A-Za-z0-9_\-]+)\["(?P<label>[^"\]\n]+)\]"#).sub_with(&t, |m| {
        Ok(format!("{}[\"{}\"]", m.name("id"), m.name("label")))
    })?;
    if t3 != t {
        t = t3;
        fixes.push_once("restore_label_trailing_quote");
    }
    if t != before {
        fixes.push_once("collapse_extra_label_quotes");
    }
    Ok(t)
}

/// `fix_group` of `_final_generics_fix`.
fn fix_group(content: &str) -> Result<String, ReError> {
    let content = content.replace("\\\"", "\"");
    let parts = top_level_commas(&chars(&content));
    if parts.is_empty() {
        return Ok(format!("[{content}]"));
    }
    let parts2: Result<Vec<String>, ReError> =
        parts.iter().map(|p| strip_outer_quotes(p)).collect();
    Ok(format!("[{}]", parts2?.join(", ")))
}

/// `sanitize_mermaid_diagram`: `(sanitized, fixes, errors)`.
///
/// # Errors
///
/// A regular-expression failure (see the module comment).
#[allow(clippy::too_many_lines)]
pub fn sanitize_mermaid_diagram(
    diagram: &str,
    cfg: &SanitizerConfig,
) -> Result<(String, Vec<String>, Vec<String>), ReError> {
    let mut fixes = Fixes::default();
    if char_len(diagram) > cfg.max_diagram_chars {
        return Ok((diagram.to_owned(), Vec::new(), vec!["too_large".to_owned()]));
    }
    let text = normalize(diagram);
    let text = replace_smart_chars(&text, &mut fixes);
    let mut lines = split_lines(&text);
    if lines.last().is_some_and(String::is_empty) {
        lines.pop();
    }
    let mut header = detect_header(&lines);
    if header.is_empty() && cfg.enable_repairs {
        let before = lines.len();
        repair_missing_header(&mut lines, cfg, &mut fixes)?;
        if lines.len() != before {
            header = detect_header(&lines);
        }
    }
    let flowchart_like = header == "graph" || header == "flowchart";
    if flowchart_like && let Some(first) = lines.first_mut() {
        *first = ensure_flowchart_direction(first, cfg, &mut fixes);
    }
    let mut text2 = lines.join("\n") + "\n";
    if flowchart_like {
        let reserved = re!(r#"(?<![A-Za-z0-9_])end\[""#);
        if reserved.is_match(&text2)? {
            text2 = reserved.sub(&text2, r#"end_[""#)?;
            fixes.push("flow_reserved_node_rename");
        }
        if cfg.enforce_single_flowchart_arrow {
            let preliminary = re!(r"-+>+").sub(&text2, "-->")?;
            if preliminary != text2 {
                fixes.push("normalize_arrows");
                text2 = preliminary;
            }
            let strict = re!(r"-[.oxX-]*>+").sub(&text2, "-->")?;
            if strict != text2 {
                fixes.push("strict_flow_arrow");
                text2 = strict;
            }
            let cleaned = re!(r"-+#>+").sub(&text2, "-->")?;
            if cleaned != text2 {
                fixes.push_once("strict_flow_arrow");
                text2 = cleaned;
            }
        }
        let pre_fix = re!(r#"\[\s*\"\s*\'([^\']+)\'\s*\"\s*\]"#).sub(&text2, r"['\1']")?;
        if pre_fix != text2 {
            text2 = pre_fix;
            fixes.push_once("label_index_quote_single");
        }
        text2 = auto_nodes_for_orphan_labels(&text2, &mut fixes)?;
        text2 = quote_labels(&text2, &mut fixes)?;
        text2 = quote_arrow_labels(&text2, &mut fixes)?;
        text2 = rename_flow_reserved_ids(&text2, &mut fixes)?;
        if re!(r#"(^|\n)\s*end\[""#).is_match(&text2)? {
            text2 = re!(r#"(^|\n)(\s*)end\[""#).sub_with(&text2, |m| {
                Ok(format!("{}{}end_[\"", m.group(1), m.group(2)))
            })?;
            fixes.push_once("flow_reserved_node_rename");
        }
        text2 = unwrap_decision_quotes(&text2, &mut fixes)?;
    }
    if header == SEQUENCE {
        text2 = add_sequence_participants(&text2, &mut fixes, cfg)?;
        text2 = avoid_reserved_aliases(&text2, &mut fixes)?;
        text2 = split_note_semicolons(&text2, &mut fixes)?;
        text2 = align_near_miss_aliases(&text2, &mut fixes)?;
    }
    text2 = post_normalize(&text2, &mut fixes)?;
    text2 = label_inner_normalize(&text2, &mut fixes)?;
    text2 = normalize_label_generics(&text2, &mut fixes)?;
    let mut text2_new = re!(r#"\\"([^\\"]+?)\\""#).sub(&text2, r"'\1'")?;
    let text2_new2 = re!(r#"\((\s*)"([^"\n]+)"(\s*)\)"#).sub(&text2_new, r"(\1'\2'\3)")?;
    if text2_new2 != text2_new {
        text2_new = text2_new2;
    }
    if text2_new != text2 {
        text2 = text2_new;
        fixes.push_once("label_escaped_dquote_to_single");
    }
    let fix2 = re!(r#"\[\s*\"\s*\'([^\']+)\'\s*\"\s*\]"#).sub(&text2, r"['\1']")?;
    if fix2 != text2 {
        text2 = fix2;
        fixes.push_once("label_index_quote_single");
    }
    let fix = re!(r#""\s*(\[\s*\'[^\]]+\'\s*\])"#).sub(&text2, r"\1")?;
    if fix != text2 {
        text2 = fix;
        fixes.push_once("label_index_quote_single");
    }
    text2 = late_label_quote(&text2, &mut fixes)?;
    text2 = merge_fragments(&text2, &mut fixes)?;
    text2 = normalize_inner_quotes(&text2, &mut fixes)?;
    if flowchart_like {
        text2 = subgraph_rewrite(&text2, &mut fixes)?;
    }
    if header == SEQUENCE {
        text2 = sequence_tail(&text2, &mut fixes)?;
    }
    text2 = final_flow_label_cleanup(&text2, &mut fixes)?;
    let g = re!(r#"\[([A-Za-z0-9_\.]+)"\]"#).sub(&text2, r"[\1]")?;
    if g != text2 {
        text2 = g;
        fixes.push_once("label_bracket_adjacent_quote_trim");
    }
    let trimmed = text2.replace("\"]\"]", "]]");
    if trimmed != text2 {
        text2 = trimmed;
        fixes.push_once("label_array_close_quote_trim");
    }
    let mut collapse_return = false;
    let final1 = re!(r#"\["((?:[^"]|"(?!\]))*)"\]"#).sub_with(&text2, |m| {
        let inner = m.group(1);
        let mut new_inner = re!(r#"Return\s+\[(?:\\?"\s*)+\]"#).sub(inner, "Return []")?;
        let dangling = re!(r"Return\s+\[$");
        if new_inner == inner && dangling.is_match(inner)? {
            new_inner = dangling.sub(inner, "Return []")?;
        }
        if new_inner != inner {
            collapse_return = true;
            return Ok(format!("[\"{new_inner}\"]"));
        }
        Ok(m.whole().to_owned())
    })?;
    if collapse_return {
        fixes.push_once("collapse_extra_label_quotes");
    }
    if final1 != text2 {
        text2 = final1;
    }
    let final2 = re!(r#"\[\s*\"\s*\'([^\']+)\'\s*\"\s*\]"#).sub(&text2, r"['\1']")?;
    if final2 != text2 {
        text2 = final2;
        fixes.push_once("label_index_quote_single");
    }
    let final3 = re!(r#"\[\s*\"\s*\'([^\']+)\'\s*\]"#).sub(&text2, r"['\1']")?;
    if final3 != text2 {
        text2 = final3;
        fixes.push_once("label_index_quote_single");
    }
    let node_label = re!(r#"(?P<id>[A-Za-z0-9_\-]+)\["(?P<inner>(?:[^"\\]|\\.(?!\]))*)"\]"#);
    let mut strip_hit = false;
    let late = node_label.sub_with(&text2, |m| {
        let inner = m.name("inner");
        let fixed = re!(r#""\s*(\[\s*\'[^\]]+\'\s*\])"#).sub(inner, r"\1")?;
        if fixed != inner {
            strip_hit = true;
        }
        Ok(format!("{}[\"{}\"]", m.name("id"), fixed))
    })?;
    if strip_hit {
        fixes.push_once("label_index_quote_single");
    }
    if late != text2 {
        text2 = late;
    }
    let final_gen = node_label.sub_with(&text2, |m| {
        let inner2 = re!(r#"([A-Za-z0-9_])"\["#).sub(m.name("inner"), r"\1[")?;
        let inner3 = re!(r"\[([^\[\]]*?)\]").sub_with(&inner2, |g| fix_group(g.group(1)))?;
        Ok(format!("{}[\"{}\"]", m.name("id"), inner3))
    })?;
    if final_gen != text2 {
        text2 = final_gen;
    }
    Ok((text2, fixes.deduplicated(), Vec::new()))
}

/// `_find_ticks_outside_quotes`: the char index of the first three
/// backticks outside unescaped double quotes.
fn find_ticks_outside_quotes(s: &[char]) -> Option<usize> {
    let mut in_quotes = false;
    let mut i = 0;
    while i + 3 <= s.len() {
        let ch = s[i];
        if ch == '"' {
            if i == 0 || s[i - 1] != '\\' {
                in_quotes = !in_quotes;
            }
            i += 1;
            continue;
        }
        if !in_quotes && s[i..i + 3] == ['`', '`', '`'] {
            return Some(i);
        }
        i += 1;
    }
    None
}

/// `_normalize_fences`: inside a mermaid block, a closing fence on a line
/// with other text is moved to its own line; an unclosed block is closed.
fn normalize_fences(md: &str) -> Result<(String, Vec<String>), ReError> {
    let mut fx = Vec::new();
    let mut out: Vec<String> = Vec::new();
    let mut inside = false;
    let opener = re!(r"^```mermaid\b", Flags::I);
    let opener_line = re!(r"^\s*```mermaid\b", Flags::I);
    for ln in md.split('\n') {
        let stripped = pystr::strip(ln);
        if !inside {
            if opener.match_start(stripped)?.is_some() {
                inside = true;
            }
            out.push(ln.to_owned());
            continue;
        }
        if stripped == "```" {
            inside = false;
            out.push(ln.to_owned());
            continue;
        }
        let line_chars = chars(ln);
        if let Some(tick) = find_ticks_outside_quotes(&line_chars)
            && opener_line.match_start(ln)?.is_none()
        {
            let pre: String = line_chars[..tick].iter().collect();
            let pre = rstrip(&pre).to_owned();
            let post: String = line_chars[tick + 3..].iter().collect();
            if !pre.is_empty() {
                out.push(pre);
            }
            out.push("```".to_owned());
            inside = false;
            if !post.is_empty() {
                out.push(post);
            }
            fx.push("fence_split_inline_closer".to_owned());
            continue;
        }
        out.push(ln.to_owned());
    }
    if inside {
        out.push("```".to_owned());
        fx.push("fence_autoclose_unmatched".to_owned());
    }
    Ok((out.join("\n"), fx))
}

/// `sanitize_content`: sanitise every fenced Mermaid block of a page.
///
/// The page path replaces the content only when `summary.total > 0`
/// (a page without a Mermaid block keeps its text, fences untouched).
///
/// # Errors
///
/// A regular-expression failure (the caller keeps the page as it was).
pub fn sanitize_content(
    content: &str,
    cfg: &SanitizerConfig,
) -> Result<(String, SanitizationSummary), ReError> {
    let (content, fence_fixes) = normalize_fences(content)?;
    let fence = re!(
        MERMAID_FENCE,
        Flags {
            multiline: true,
            dotall: true,
            ignorecase: true,
        }
    );
    let mut output = String::with_capacity(content.len());
    let mut records: Vec<DiagramRecord> = Vec::new();
    let mut last = 0;
    for (index, m) in fence.find_all(&content)?.iter().enumerate() {
        let (start, end) = (m.start(), m.end());
        let body = m.group(1);
        output.push_str(&content[last..start]);
        let (sanitized, fixes, errors) = sanitize_mermaid_diagram(body, cfg)?;
        let mut was_modified = sanitized != body;
        if was_modified && sanitized.trim_end_matches('\n') == body.trim_end_matches('\n') {
            was_modified = false;
        }
        let status = if !errors.is_empty() {
            DiagramStatus::Failed
        } else if was_modified {
            DiagramStatus::Fixed
        } else {
            DiagramStatus::Valid
        };
        let hash = sha12(if errors.is_empty() { &sanitized } else { body });
        let mut block = sanitized.clone();
        if !block.ends_with('\n') {
            block.push('\n');
        }
        // A newline after the fence only if the original text did NOT
        // have one right after the match (the match already ate one).
        let needs_newline = !(end < content.len() && content.as_bytes()[end] == b'\n');
        output.push_str("```mermaid\n");
        output.push_str(&block);
        output.push_str("```");
        if needs_newline {
            output.push('\n');
        }
        records.push(DiagramRecord {
            index,
            original: body.to_owned(),
            sanitized,
            was_modified,
            errors,
            fixes,
            status,
            hash,
        });
        last = end;
    }
    output.push_str(&content[last..]);
    if !fence_fixes.is_empty()
        && let Some(first) = records.first_mut()
    {
        let mut merged = std::mem::take(&mut first.fixes);
        merged.extend(fence_fixes);
        let mut seen = HashSet::new();
        merged.retain(|f| seen.insert(f.clone()));
        first.fixes = merged;
    }
    let count = |status| records.iter().filter(|r| r.status == status).count();
    let summary = SanitizationSummary {
        total: records.len(),
        valid: count(DiagramStatus::Valid),
        fixed: count(DiagramStatus::Fixed),
        failed: count(DiagramStatus::Failed),
        records,
    };
    Ok((output, summary))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(content: &str) -> String {
        sanitize_content(content, &SanitizerConfig::default())
            .map(|(text, _)| text)
            .unwrap()
    }

    #[test]
    fn a_page_without_diagrams_is_unchanged() {
        assert_eq!(run("# Title\n\nText.\n"), "# Title\n\nText.\n");
    }

    #[test]
    fn a_valid_diagram_loses_the_blank_line_after_its_fence() {
        // Python: the fence pattern eats the newline after the closing
        // fence and re-adds one only when the NEXT character is not a
        // newline, so a blank line after a diagram disappears (kept).
        let page = "## Overview\n\n```mermaid\nflowchart LR\n  api --> store\n```\n\nSee.\n";
        let (text, summary) = sanitize_content(page, &SanitizerConfig::default()).unwrap();
        assert_eq!(
            text,
            "## Overview\n\n```mermaid\nflowchart LR\n  api --> store\n```\nSee.\n"
        );
        assert_eq!((summary.total, summary.valid), (1, 1));
    }
}
