//! The SDK's design-token extraction (`FigmaApiWrapper.extract_design_tokens`
//! and its `_extract_node_styles_recursive` / `_dedup_*` helpers) and the
//! batch output formats of `extract_design_tokens_batch`.
//!
//! Python's `round` is reproduced: `round(x)` is ties-to-even and returns an
//! integer, `round(x, n)` keeps an integer an integer and rounds a float to
//! `n` decimals. Deduplication keys compare numbers by value, as Python
//! compares `1` and `1.0` equal.

use std::collections::{BTreeMap, BTreeSet, HashSet};

use serde_json::{Map, Number, Value, json};

/// Recursion bound for a node tree: Figma's `depth` is at most 8 levels of
/// requested children, but INSTANCE subtrees can nest further.
const MAX_TREE_DEPTH: usize = 256;
const GRADIENTS: [&str; 4] = [
    "GRADIENT_LINEAR",
    "GRADIENT_RADIAL",
    "GRADIENT_ANGULAR",
    "GRADIENT_DIAMOND",
];

fn number(value: Option<&Value>, default: f64) -> f64 {
    value.and_then(Value::as_f64).unwrap_or(default)
}

fn empty_object() -> Value {
    Value::Object(Map::new())
}

/// `round(x)`: ties to even, an integer.
fn py_round_int(value: f64) -> i64 {
    // Colour channels are within [0, 255]; the cast saturates outside it.
    #[expect(clippy::cast_possible_truncation, reason = "a rounded colour channel")]
    let rounded = value.round_ties_even() as i64;
    rounded
}

/// `round(x, digits)` as a JSON number: an integer input stays an integer.
fn py_round(value: Option<&Value>, default: f64, digits: usize) -> Value {
    if let Some(Value::Number(number)) = value
        && (number.is_i64() || number.is_u64())
    {
        return Value::Number(number.clone());
    }
    let value = number(value, default);
    let rounded = format!("{value:.digits$}").parse::<f64>().unwrap_or(value);
    Number::from_f64(rounded).map_or(Value::Null, Value::Number)
}

/// A dedup key component that compares numbers by value.
fn key_part(value: Option<&Value>) -> String {
    match value {
        None | Some(Value::Null) => "None".to_owned(),
        Some(Value::Number(number)) => number
            .as_f64()
            .map_or_else(|| number.to_string(), |value| format!("{value:?}")),
        Some(other) => other.to_string(),
    }
}

fn hex(value: Option<&Value>) -> String {
    let channel = |key: &str| {
        let value = py_round_int(number(value.and_then(|color| color.get(key)), 0.0) * 255.0);
        if value < 0 {
            format!("-{:X}", value.unsigned_abs())
        } else {
            format!("{value:02X}")
        }
    };
    format!("#{}{}{}", channel("r"), channel("g"), channel("b"))
}

fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(value) => *value,
        Value::Number(number) => number.as_f64().is_some_and(|value| value != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(values) => !values.is_empty(),
        Value::Object(values) => !values.is_empty(),
    }
}

fn array<'a>(node: &'a Value, key: &str) -> &'a [Value] {
    node.get(key)
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
}

/// `_extract_node_styles_recursive`: one entry per node carrying a style.
pub(super) fn style_entries(node: &Value) -> Vec<Value> {
    let mut entries = Vec::new();
    collect(node, "", &mut entries, 0);
    entries
}

fn collect(node: &Value, path: &str, entries: &mut Vec<Value>, depth: usize) {
    if depth > MAX_TREE_DEPTH {
        return;
    }
    let name = node.get("name").and_then(Value::as_str).unwrap_or_default();
    let current = if path.is_empty() {
        name.to_owned()
    } else {
        format!("{path}/{name}")
    };
    let fills = array(node, "fills")
        .iter()
        .filter(|fill| {
            let kind = fill.get("type").and_then(Value::as_str);
            kind != Some("IMAGE")
                && (number(fill.get("color").and_then(|color| color.get("a")), 1.0) > 0.0
                    || kind != Some("SOLID"))
        })
        .cloned()
        .collect::<Vec<_>>();
    let strokes = array(node, "strokes").to_vec();
    let effects = array(node, "effects")
        .iter()
        .filter(|effect| {
            effect
                .get("visible")
                .is_none_or(|visible| visible.as_bool().unwrap_or(true))
        })
        .cloned()
        .collect::<Vec<_>>();
    let style = node.get("style");
    let mut typography = Map::new();
    if style
        .and_then(|style| style.get("fontFamily"))
        .is_some_and(truthy)
    {
        for key in [
            "fontFamily",
            "fontSize",
            "fontWeight",
            "lineHeightPx",
            "letterSpacing",
            "textAlignHorizontal",
        ] {
            if let Some(value) = style.and_then(|style| style.get(key)) {
                typography.insert(key.to_owned(), value.clone());
            }
        }
    }
    let style_refs = node.get("styles").cloned().unwrap_or_else(empty_object);
    if !fills.is_empty()
        || !strokes.is_empty()
        || !effects.is_empty()
        || !typography.is_empty()
        || truthy(&style_refs)
    {
        entries.push(json!({
            "path":current,
            "type":node.get("type").cloned().unwrap_or_else(|| json!("")),
            "fills":fills,
            "strokes":strokes,
            "effects":effects,
            "typography":typography,
            "style_refs":style_refs,
        }));
    }
    for child in array(node, "children") {
        collect(child, &current, entries, depth + 1);
    }
}

fn entry_path(entry: &Value) -> String {
    entry
        .get("path")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned()
}

/// `_dedup_colors` (`with_opacity`) and `_dedup_strokes`.
fn dedup_paints(entries: &[Value], field: &str, with_opacity: bool) -> Vec<Value> {
    let mut seen = HashSet::new();
    let mut tokens = Vec::new();
    for entry in entries {
        let path = entry_path(entry);
        for paint in array(entry, field) {
            let kind = paint
                .get("type")
                .and_then(Value::as_str)
                .unwrap_or_default();
            let mut candidates = Vec::new();
            if kind == "SOLID" {
                candidates.push((paint.get("color"), path.clone(), paint.get("opacity")));
            } else if GRADIENTS.contains(&kind) {
                for stop in array(paint, "gradientStops") {
                    candidates.push((stop.get("color"), format!("{path} [{kind}]"), None));
                }
            }
            for (color, source_path, opacity) in candidates {
                let alpha = color.and_then(|color| color.get("a"));
                if number(alpha, 1.0) == 0.0 {
                    continue;
                }
                let hex = hex(color);
                let key = (hex.clone(), key_part(Some(&py_round(alpha, 1.0, 2))));
                if !seen.insert(key) {
                    continue;
                }
                let mut token = Map::new();
                token.insert("hex".to_owned(), json!(hex));
                token.insert("alpha".to_owned(), py_round(alpha, 1.0, 4));
                if with_opacity {
                    let opacity = if kind == "SOLID" {
                        py_round(opacity, 1.0, 4)
                    } else {
                        json!(1.0)
                    };
                    token.insert("opacity".to_owned(), opacity);
                }
                token.insert("source_path".to_owned(), json!(source_path));
                tokens.push(Value::Object(token));
            }
        }
    }
    tokens
}

/// `_dedup_typography`: family, size and weight.
fn dedup_typography(entries: &[Value]) -> Vec<Value> {
    let mut seen = HashSet::new();
    let mut tokens = Vec::new();
    for entry in entries {
        let Some(typography) = entry.get("typography").and_then(Value::as_object) else {
            continue;
        };
        if !typography.get("fontFamily").is_some_and(truthy) {
            continue;
        }
        let key = [
            key_part(typography.get("fontFamily")),
            key_part(typography.get("fontSize")),
            key_part(typography.get("fontWeight")),
        ];
        if seen.insert(key) {
            let mut token = typography.clone();
            token.insert("source_path".to_owned(), json!(entry_path(entry)));
            tokens.push(Value::Object(token));
        }
    }
    tokens
}

/// `_dedup_effects`: type, offset, radius, spread and colour.
fn dedup_effects(entries: &[Value]) -> Vec<Value> {
    let mut seen = HashSet::new();
    let mut tokens = Vec::new();
    for entry in entries {
        for effect in array(entry, "effects") {
            let color = effect.get("color").filter(|value| truthy(value));
            let offset = effect.get("offset").filter(|value| truthy(value));
            let rounded = |source: Option<&Value>, key: &str, default: f64| {
                key_part(Some(&py_round(
                    source.and_then(|value| value.get(key)),
                    default,
                    2,
                )))
            };
            let signature = [
                key_part(effect.get("type")),
                rounded(offset, "x", 0.0),
                rounded(offset, "y", 0.0),
                key_part(effect.get("radius")),
                key_part(effect.get("spread")),
                rounded(color, "r", 0.0),
                rounded(color, "g", 0.0),
                rounded(color, "b", 0.0),
                rounded(color, "a", 0.0),
            ];
            if seen.insert(signature) {
                let mut token = effect.as_object().cloned().unwrap_or_default();
                token.insert("source_path".to_owned(), json!(entry_path(entry)));
                tokens.push(Value::Object(token));
            }
        }
    }
    tokens
}

/// The `extract_design_tokens` result for one fetched node document.
pub(super) fn design_tokens(node_id: &str, document: &Value) -> Value {
    let entries = style_entries(document);
    let colors = dedup_paints(&entries, "fills", true);
    let strokes = dedup_paints(&entries, "strokes", false);
    let typography = dedup_typography(&entries);
    let effects = dedup_effects(&entries);
    json!({
        "node_id":node_id,
        "node_name":document.get("name").cloned().unwrap_or_else(|| json!("")),
        "node_type":document.get("type").cloned().unwrap_or_else(|| json!("")),
        "summary":{
            "total_style_entries":entries.len(),
            "unique_colors":colors.len(),
            "unique_fonts":typography.len(),
            "unique_effects":effects.len(),
            "unique_strokes":strokes.len(),
        },
        "colors":colors,
        "strokes":strokes,
        "typography":typography,
        "effects":effects,
        "raw_entries":entries,
    })
}

/// One processed batch entry, success or error, in the SDK's shape.
pub(super) struct BatchResult {
    pub(super) index: usize,
    pub(super) value: Value,
}

fn is_success(result: &Value) -> bool {
    result.get("status").and_then(Value::as_str) == Some("success")
}

fn count(result: &Value, key: &str) -> usize {
    result
        .get(key)
        .and_then(Value::as_array)
        .map_or(0, Vec::len)
}

/// The batch answer in one of the SDK's four `output_format`s; anything
/// else is `full`.
pub(super) fn batch_output(
    total_entries: usize,
    mut results: Vec<BatchResult>,
    output_format: &str,
) -> Value {
    results.sort_by_key(|result| result.index);
    let results = results
        .into_iter()
        .map(|result| result.value)
        .collect::<Vec<_>>();
    let successful = results
        .iter()
        .filter(|result| is_success(result))
        .collect::<Vec<_>>();
    let total = |key: &str| {
        successful
            .iter()
            .map(|result| count(result, key))
            .sum::<usize>()
    };
    let batch_summary = json!({
        "total_entries":total_entries,
        "processed_entries":results.len(),
        "successful":successful.len(),
        "failed":results.len() - successful.len(),
        "total_colors":total("colors"),
        "total_strokes":total("strokes"),
        "total_fonts":total("typography"),
        "total_effects":total("effects"),
    });
    match output_format {
        "summary" => json!({"batch_summary":batch_summary}),
        "compact" => {
            let compact = results
                .iter()
                .map(|result| {
                    let field = |key: &str| result.get(key).cloned().unwrap_or(Value::Null);
                    if is_success(result) {
                        json!({
                            "entry_index":field("entry_index"),
                            "node_id":field("node_id"),
                            "node_name":field("node_name"),
                            "node_type":field("node_type"),
                            "status":"success",
                            "color_count":count(result, "colors"),
                            "stroke_count":count(result, "strokes"),
                            "font_count":count(result, "typography"),
                            "effect_count":count(result, "effects"),
                        })
                    } else {
                        json!({
                            "entry_index":field("entry_index"),
                            "node_id":field("node_id"),
                            "status":"error",
                            "error":field("error"),
                        })
                    }
                })
                .collect::<Vec<_>>();
            json!({"results":compact,"batch_summary":batch_summary})
        }
        "style_guide" => json!({
            "style_guide":style_guide(&results),
            "batch_summary":batch_summary,
        }),
        _ => json!({"results":results,"batch_summary":batch_summary}),
    }
}

/// One token registry of the `style_guide` format: definitions in first-seen
/// order with sequential ids, and how many components use each.
struct Registry {
    prefix: &'static str,
    ids: BTreeMap<String, String>,
    definitions: Vec<Map<String, Value>>,
    usage: BTreeMap<String, usize>,
}

impl Registry {
    const fn new(prefix: &'static str) -> Self {
        Self {
            prefix,
            ids: BTreeMap::new(),
            definitions: Vec::new(),
            usage: BTreeMap::new(),
        }
    }

    fn register(&mut self, key: String, definition: Map<String, Value>) -> String {
        if let Some(id) = self.ids.get(&key) {
            return id.clone();
        }
        let id = format!("{}_{:03}", self.prefix, self.definitions.len() + 1);
        let mut entry = Map::new();
        entry.insert("id".to_owned(), json!(id));
        entry.extend(definition);
        self.definitions.push(entry);
        self.ids.insert(key, id.clone());
        self.usage.insert(id.clone(), 0);
        id
    }

    fn mark_used(&mut self, ids: &BTreeSet<String>) {
        for id in ids {
            if let Some(count) = self.usage.get_mut(id) {
                *count += 1;
            }
        }
    }

    fn finish(self) -> Vec<Value> {
        self.definitions
            .into_iter()
            .map(|mut definition| {
                let id = definition
                    .get("id")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned();
                definition.insert(
                    "components_count".to_owned(),
                    json!(self.usage.get(&id).copied().unwrap_or(0)),
                );
                Value::Object(definition)
            })
            .collect()
    }
}

fn field_or(token: &Value, key: &str, default: Value) -> Value {
    token.get(key).cloned().unwrap_or(default)
}

#[expect(
    clippy::too_many_lines,
    reason = "the SDK's four registries, side by side"
)]
fn style_guide(results: &[Value]) -> Value {
    let mut colors = Registry::new("color");
    let mut strokes = Registry::new("stroke");
    let mut typography = Registry::new("type");
    let mut effects = Registry::new("effect");
    let mut components = Vec::new();
    for result in results {
        let field = |key: &str| result.get(key).cloned().unwrap_or(Value::Null);
        if !is_success(result) {
            components.push(json!({
                "entry_index":field("entry_index"),
                "file_key":field("file_key"),
                "node_id":field("node_id"),
                "status":"error",
                "error":field("error"),
            }));
            continue;
        }
        let color_ids = array(result, "colors")
            .iter()
            .map(|token| {
                let hex = field_or(token, "hex", json!(""));
                let alpha = field_or(token, "alpha", json!(1.0));
                let opacity = field_or(token, "opacity", json!(1.0));
                let key = format!(
                    "{}|{}|{}",
                    key_part(Some(&hex)),
                    key_part(Some(&alpha)),
                    key_part(Some(&opacity))
                );
                let mut definition = Map::new();
                definition.insert("hex".to_owned(), hex);
                definition.insert("alpha".to_owned(), alpha);
                definition.insert("opacity".to_owned(), opacity);
                definition.insert(
                    "source_path".to_owned(),
                    field_or(token, "source_path", json!("")),
                );
                colors.register(key, definition)
            })
            .collect::<BTreeSet<_>>();
        let stroke_ids = array(result, "strokes")
            .iter()
            .map(|token| {
                let hex = field_or(token, "hex", json!(""));
                let alpha = field_or(token, "alpha", json!(1.0));
                let key = format!("{}|{}", key_part(Some(&hex)), key_part(Some(&alpha)));
                let mut definition = Map::new();
                definition.insert("hex".to_owned(), hex);
                definition.insert("alpha".to_owned(), alpha);
                definition.insert(
                    "source_path".to_owned(),
                    field_or(token, "source_path", json!("")),
                );
                strokes.register(key, definition)
            })
            .collect::<BTreeSet<_>>();
        let typography_ids = array(result, "typography")
            .iter()
            .map(|token| {
                let keys = [
                    "fontFamily",
                    "fontSize",
                    "fontWeight",
                    "lineHeightPx",
                    "letterSpacing",
                    "textAlignHorizontal",
                ];
                let mut definition = Map::new();
                let mut key = Vec::with_capacity(keys.len());
                for name in keys {
                    let default = if name == "fontFamily" {
                        json!("")
                    } else {
                        Value::Null
                    };
                    let value = field_or(token, name, default);
                    key.push(key_part(Some(&value)));
                    definition.insert(name.to_owned(), value);
                }
                definition.insert(
                    "source_path".to_owned(),
                    field_or(token, "source_path", json!("")),
                );
                typography.register(key.join("|"), definition)
            })
            .collect::<BTreeSet<_>>();
        let effect_ids = array(result, "effects")
            .iter()
            .map(|token| {
                let mut payload = Map::new();
                payload.insert("type".to_owned(), field_or(token, "type", json!("")));
                payload.insert(
                    "visible".to_owned(),
                    field_or(token, "visible", json!(true)),
                );
                payload.insert("radius".to_owned(), field_or(token, "radius", Value::Null));
                payload.insert("spread".to_owned(), field_or(token, "spread", Value::Null));
                payload.insert(
                    "offset".to_owned(),
                    field_or(token, "offset", empty_object()),
                );
                payload.insert("color".to_owned(), field_or(token, "color", empty_object()));
                // `json.dumps(payload, sort_keys=True)`.
                let key = crate::canonical::to_string(&Value::Object(payload.clone()))
                    .unwrap_or_default();
                let mut definition = payload;
                definition.insert(
                    "source_path".to_owned(),
                    field_or(token, "source_path", json!("")),
                );
                effects.register(key, definition)
            })
            .collect::<BTreeSet<_>>();
        colors.mark_used(&color_ids);
        strokes.mark_used(&stroke_ids);
        typography.mark_used(&typography_ids);
        effects.mark_used(&effect_ids);
        components.push(json!({
            "entry_index":field("entry_index"),
            "file_key":field("file_key"),
            "node_id":field("node_id"),
            "node_name":field("node_name"),
            "node_type":field("node_type"),
            "status":"success",
            "tokens":{
                "colors":color_ids,
                "strokes":stroke_ids,
                "typography":typography_ids,
                "effects":effect_ids,
            },
        }));
    }
    json!({
        "tokens":{
            "colors":colors.finish(),
            "strokes":strokes.finish(),
            "typography":typography.finish(),
            "effects":effects.finish(),
        },
        "components":components,
    })
}
