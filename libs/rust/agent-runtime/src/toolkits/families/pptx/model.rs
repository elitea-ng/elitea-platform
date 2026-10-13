//! The prompts and reply parsing of the two model-calling tools, ported from
//! the SDK (`pptx_wrapper.py`: `_generate_content_batch`,
//! `_translate_batch_single`). The prompt text is the SDK's, so a model
//! tuned against it behaves the same; the parsing keeps the SDK's two
//! fallbacks and its "never lose a placeholder" rule, and keys a translation
//! by its `index` where the SDK trusted the model's order.

use std::collections::HashMap;
use std::sync::LazyLock;

use regex::Regex;
use serde_json::Value;

#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static FENCED_ARRAY: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?s)```(?:json)?\s*(\[.*\])\s*```").expect("a fixed pattern"));
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static BARE_ARRAY: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?s)(\[.*\])").expect("a fixed pattern"));
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static CONTENT_LINE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r#""content":\s*"([^"]*)""#).expect("a fixed pattern"));
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static TRANSLATION_LINE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r#""translation":\s*"([^"]*)""#).expect("a fixed pattern"));
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static NUMBERED_LINE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^\d+[\.\)]\s*(.*)").expect("a fixed pattern"));

/// The array text of a reply: the fenced block, else the first `[` to the
/// last `]`, else the whole reply.
fn json_text(reply: &str) -> &str {
    if let Some(m) = FENCED_ARRAY.captures(reply) {
        return m.get(1).map_or(reply, |g| g.as_str());
    }
    BARE_ARRAY
        .captures(reply)
        .and_then(|m| m.get(1))
        .map_or(reply, |g| g.as_str())
}

/// `json.dumps(value, ensure_ascii=False, indent=2)`. Field order is the
/// struct's own, which the SDK's prompt fixes (`placeholder` before `key`):
/// `serde_json::Value` would sort the keys.
fn pretty<T: serde::Serialize>(value: &T) -> String {
    serde_json::to_string_pretty(value).unwrap_or_else(|_| "[]".to_owned())
}

#[derive(serde::Serialize)]
struct PlaceholderEntry<'a> {
    placeholder: &'a str,
    key: &'a str,
}

#[derive(serde::Serialize)]
struct TextEntry<'a> {
    index: usize,
    text: &'a str,
}

fn text_of(value: Option<&Value>) -> Option<String> {
    match value? {
        Value::String(s) => Some(s.clone()),
        Value::Null => None,
        other => Some(other.to_string()),
    }
}

/// The SDK's fill prompt for one batch.
pub(crate) fn fill_prompt(placeholders: &[String], content_description: &str) -> String {
    let list: Vec<PlaceholderEntry<'_>> = placeholders
        .iter()
        .map(|t| PlaceholderEntry {
            placeholder: t,
            key: t,
        })
        .collect();
    format!(
        "I need content for {} placeholders in a PowerPoint presentation.\n\n\
<Data Available for use>\n{content_description}\n</Data Available for use>\n\n\
<Placeholders to fill>\n{}\n</Placeholders to fill>\n\n\
Please provide content for ALL placeholders listed above.\n\
Return your response as a JSON array matching each placeholder by its key:\n\
[{{\"key\": \"{{{{placeholder_name}}}}\", \"content\": \"generated content\"}}, ...]\n\n\
IMPORTANT:\n\
- Use the exact \"key\" value from the input to identify each placeholder\n\
- Preserve any newlines (\\n) if needed in the content\n\
- Match the tone and style suggested by each placeholder\n\
- Base your content on the provided data description\n\
- Keep content concise and appropriate for PowerPoint slides",
        placeholders.len(),
        pretty(&list),
    )
}

/// The generated content for each of `placeholders`. A placeholder the reply
/// does not cover gets `[Content for: <placeholder>]`, as in the SDK, so a
/// template never keeps its marker silently.
pub(crate) fn parse_fill_reply(reply: &str, placeholders: &[String]) -> HashMap<String, String> {
    let wanted: std::collections::HashSet<&str> = placeholders.iter().map(String::as_str).collect();
    let reply = reply.trim();
    let mut map: HashMap<String, String> = HashMap::new();
    #[allow(
        clippy::single_match_else,
        reason = "the else is the SDK's line fallback"
    )]
    match serde_json::from_str::<Value>(json_text(reply)) {
        Ok(Value::Array(items)) => {
            for item in &items {
                let key = text_of(item.get("key").or_else(|| item.get("placeholder")))
                    .unwrap_or_default();
                let content =
                    text_of(item.get("content").or_else(|| item.get("text"))).unwrap_or_default();
                // Only keys of this batch: a key the model invents, or takes from
                // another batch, is not ours to keep.
                if !key.is_empty() && wanted.contains(key.as_str()) {
                    map.insert(key, content);
                }
            }
        }
        _ => {
            let values: Vec<String> = reply
                .lines()
                .map(str::trim)
                .filter(|l| !l.is_empty())
                .filter_map(|line| {
                    if let Some(c) = CONTENT_LINE.captures(line) {
                        return c.get(1).map(|g| g.as_str().to_owned());
                    }
                    (!line.starts_with('{') && !line.starts_with('['))
                        .then(|| line.trim_matches(['"', '\'']).to_owned())
                })
                .collect();
            for (placeholder, value) in placeholders.iter().zip(values) {
                map.insert(placeholder.clone(), value);
            }
        }
    }
    for placeholder in placeholders {
        map.entry(placeholder.clone())
            .or_insert_with(|| format!("[Content for: {placeholder}]"));
    }
    map
}

/// The SDK's translation prompt for one batch; `start` is the number of
/// texts in earlier batches (indexes continue across batches).
pub(crate) fn translate_prompt(texts: &[String], language: &str, start: usize) -> String {
    let list: Vec<TextEntry<'_>> = texts
        .iter()
        .enumerate()
        .map(|(i, t)| TextEntry {
            index: start + i + 1,
            text: t,
        })
        .collect();
    format!(
        "Please translate the following {} texts to {language}.\n\
The texts are provided in JSON format below. Some texts may contain newlines or special characters.\n\n\
Return your response as a JSON array with the same structure, keeping the same index numbers.\n\
Format: [{{\"index\": {}, \"translation\": \"translated text\"}}, {{\"index\": {}, \"translation\": \"translated text\"}}, ...]\n\n\
IMPORTANT: Preserve any newlines (\\n) in the translated text. Only translate the content, keep the formatting.\n\n\
Texts to translate:\n{}",
        texts.len(),
        start + 1,
        start + 2,
        pretty(&list),
    )
}

/// The translation of each of `texts`, `None` for one the reply does not give. Replies are matched by `index`
/// when they carry valid ones (in a mixed reply only those are used);
/// otherwise by position, only when the reply has exactly one entry per text.
/// The caller asks again for the `None`s and reports what is still missing.
pub(crate) fn parse_translation_reply(
    reply: &str,
    texts: &[String],
    start: usize,
) -> Vec<Option<String>> {
    let reply = reply.trim();
    let mut out: Vec<Option<String>> = vec![None; texts.len()];
    let mut positional: Vec<String> = Vec::new();
    #[allow(
        clippy::single_match_else,
        reason = "the else is the SDK's line fallback"
    )]
    match serde_json::from_str::<Value>(json_text(reply)) {
        Ok(Value::Array(items)) => {
            let mut indexed = false;
            for item in &items {
                let translated = text_of(item.get("translation").or_else(|| item.get("text")))
                    .unwrap_or_default();
                if let Some(i) = item.get("index").and_then(Value::as_u64).and_then(|i| {
                    usize::try_from(i)
                        .ok()?
                        .checked_sub(start + 1)
                        .filter(|i| *i < texts.len())
                }) {
                    // In a mixed reply the indexed items are the ones we can
                    // place; the rest are ignored, not guessed at.
                    out[i] = Some(translated);
                    indexed = true;
                } else {
                    positional.push(translated);
                }
            }
            if !indexed {
                positional_if_complete(&mut out, positional);
            }
        }
        _ => {
            for line in reply.lines().map(str::trim).filter(|l| !l.is_empty()) {
                if let Some(m) = NUMBERED_LINE.captures(line) {
                    positional.push(
                        m.get(1)
                            .map_or("", |g| g.as_str())
                            .trim()
                            .trim_matches(['"', '\''])
                            .to_owned(),
                    );
                } else if let Some(m) = TRANSLATION_LINE.captures(line) {
                    positional.push(m.get(1).map_or("", |g| g.as_str()).to_owned());
                }
            }
            positional_if_complete(&mut out, positional);
        }
    }
    // A blank translation of a non-blank text is a missing one.
    for (slot, original) in out.iter_mut().zip(texts) {
        if slot.as_deref().is_some_and(|t| t.trim().is_empty()) && !original.trim().is_empty() {
            *slot = None;
        }
    }
    out
}

/// Position is only trusted when the reply has exactly one entry per text: a
/// shorter or longer list cannot be lined up, so none of it is used and the
/// texts are asked for again.
fn positional_if_complete(out: &mut [Option<String>], positional: Vec<String>) {
    if positional.len() == out.len() {
        for (slot, t) in out.iter_mut().zip(positional) {
            *slot = Some(t);
        }
    }
}

/// The SDK's language-code table; a code it does not list is used as the
/// language name.
pub(crate) fn language_name(code: &str) -> String {
    match code.to_ascii_lowercase().as_str() {
        "en" => "English",
        "es" => "Spanish",
        "fr" => "French",
        "de" => "German",
        "it" => "Italian",
        "pt" => "Portuguese",
        "ru" => "Russian",
        "ja" => "Japanese",
        "zh" => "Chinese",
        "ar" => "Arabic",
        "hi" => "Hindi",
        "ko" => "Korean",
        "ua" => "Ukrainian",
        _ => return code.to_owned(),
    }
    .to_owned()
}
