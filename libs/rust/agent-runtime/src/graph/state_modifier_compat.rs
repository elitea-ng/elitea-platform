//! Jinja2 compatibility for the `state_modifier` template environment.
//!
//! Templates written for the legacy Python runtime call Python string methods
//! (`text.split(',')`, `text.strip()`, `d.items()`) and use Jinja2's built-in
//! `wordcount`, `capitalize`, `truncate` and `center` filters. minijinja has
//! neither by default, so such a template failed with `unknown method` and the
//! node settled as a template failure. This module adds them without a new
//! dependency (`minijinja-contrib` would need a lock change): a bounded
//! `unknown_method_callback` for the common Python str and dict methods, plus
//! the four filters.
//!
//! Every method is pure and allocation-bounded by its input; the environment's
//! fuel and the node's rendered-size bound still apply.

use minijinja::value::{Value as JinjaValue, ValueKind};
use minijinja::{Error as JinjaError, ErrorKind, State as JinjaState};

/// The widest `center` may pad to, so a template cannot ask for a huge string.
const MAX_WIDTH: usize = 8 * 1024;

/// The longest `replace` result, in bytes (the node's own input bound).
const MAX_REPLACED: usize = 64 * 1024;

fn invalid(message: &'static str) -> JinjaError {
    JinjaError::new(ErrorKind::InvalidOperation, message)
}

fn unknown() -> JinjaError {
    JinjaError::from(ErrorKind::UnknownMethod)
}

fn absent(value: &JinjaValue) -> bool {
    value.is_none() || value.is_undefined()
}

/// The optional string argument at `index`; `None` and a missing argument both
/// read as absent, any other non-string is refused.
fn str_arg(args: &[JinjaValue], index: usize) -> Result<Option<&str>, JinjaError> {
    match args.get(index) {
        None => Ok(None),
        Some(value) if absent(value) => Ok(None),
        Some(value) => value
            .as_str()
            .map(Some)
            .ok_or_else(|| invalid("method argument must be a string")),
    }
}

fn required_str(args: &[JinjaValue], index: usize) -> Result<&str, JinjaError> {
    str_arg(args, index)?.ok_or_else(|| invalid("method needs a string argument"))
}

/// A Python `maxsplit` argument: absent or negative means no limit.
fn max_split(args: &[JinjaValue], index: usize) -> Result<Option<usize>, JinjaError> {
    match args.get(index) {
        None => Ok(None),
        Some(value) if absent(value) => Ok(None),
        Some(value) => value
            .as_i64()
            .map(|number| usize::try_from(number).ok())
            .ok_or_else(|| invalid("maxsplit must be an integer")),
    }
}

fn strings(parts: impl IntoIterator<Item = String>) -> JinjaValue {
    parts.into_iter().map(JinjaValue::from).collect()
}

fn split(text: &str, args: &[JinjaValue]) -> Result<JinjaValue, JinjaError> {
    let limit = max_split(args, 1)?;
    Ok(match str_arg(args, 0)? {
        // Python's `split()` with no separator splits on runs of whitespace.
        None => match limit {
            None => strings(text.split_whitespace().map(ToOwned::to_owned)),
            Some(limit) => strings(split_whitespace_n(text, limit)),
        },
        Some("") => return Err(invalid("empty separator")),
        Some(separator) => match limit {
            None => strings(text.split(separator).map(ToOwned::to_owned)),
            Some(limit) => strings(
                text.splitn(limit.saturating_add(1), separator)
                    .map(ToOwned::to_owned),
            ),
        },
    })
}

/// `text.split(None, limit)`: at most `limit` splits, the rest kept whole.
fn split_whitespace_n(text: &str, limit: usize) -> Vec<String> {
    let mut parts = Vec::new();
    let mut rest = text.trim_start();
    while !rest.is_empty() {
        if parts.len() == limit {
            parts.push(rest.to_owned());
            break;
        }
        let Some(end) = rest.find(char::is_whitespace) else {
            parts.push(rest.to_owned());
            break;
        };
        parts.push(rest[..end].to_owned());
        rest = rest[end..].trim_start();
    }
    parts
}

/// `text.rsplit(None, limit)`: at most `limit` splits from the right, the rest
/// kept whole.
fn rsplit_whitespace_n(text: &str, limit: usize) -> Vec<String> {
    let mut parts = Vec::new();
    let mut rest = text.trim_end();
    while !rest.is_empty() {
        if parts.len() == limit {
            parts.push(rest.to_owned());
            break;
        }
        let Some(start) = rest.rfind(char::is_whitespace) else {
            parts.push(rest.to_owned());
            break;
        };
        let word_start = start + rest[start..].chars().next().map_or(1, char::len_utf8);
        parts.push(rest[word_start..].to_owned());
        rest = rest[..start].trim_end();
    }
    parts.reverse();
    parts
}

fn rsplit(text: &str, args: &[JinjaValue]) -> Result<JinjaValue, JinjaError> {
    let limit = max_split(args, 1)?;
    let Some(separator) = str_arg(args, 0)? else {
        return Ok(match limit {
            None => strings(text.split_whitespace().map(ToOwned::to_owned)),
            Some(limit) => strings(rsplit_whitespace_n(text, limit)),
        });
    };
    if separator.is_empty() {
        return Err(invalid("empty separator"));
    }
    let mut parts: Vec<String> = match limit {
        None => text.rsplit(separator).map(ToOwned::to_owned).collect(),
        Some(limit) => text
            .rsplitn(limit.saturating_add(1), separator)
            .map(ToOwned::to_owned)
            .collect(),
    };
    parts.reverse();
    Ok(strings(parts))
}

fn strip(
    text: &str,
    args: &[JinjaValue],
    left: bool,
    right: bool,
) -> Result<JinjaValue, JinjaError> {
    let chars = str_arg(args, 0)?;
    let matcher = |character: char| {
        chars.map_or_else(|| character.is_whitespace(), |set| set.contains(character))
    };
    let mut stripped = text;
    if left {
        stripped = stripped.trim_start_matches(matcher);
    }
    if right {
        stripped = stripped.trim_end_matches(matcher);
    }
    Ok(JinjaValue::from(stripped))
}

fn capitalize(text: &str) -> String {
    let mut characters = text.chars();
    let Some(first) = characters.next() else {
        return String::new();
    };
    let mut out: String = first.to_uppercase().collect();
    out.extend(characters.flat_map(char::to_lowercase));
    out
}

fn title(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut at_start = true;
    for character in text.chars() {
        if character.is_alphabetic() {
            if at_start {
                out.extend(character.to_uppercase());
            } else {
                out.extend(character.to_lowercase());
            }
            at_start = false;
        } else {
            out.push(character);
            at_start = true;
        }
    }
    out
}

fn char_index(text: &str, byte: usize) -> i64 {
    i64::try_from(text[..byte].chars().count()).unwrap_or(i64::MAX)
}

/// `separator.join(items)`: every item must be a string, as in Python.
fn join(separator: &str, args: &[JinjaValue]) -> Result<JinjaValue, JinjaError> {
    let items = args
        .first()
        .ok_or_else(|| invalid("join needs an iterable"))?;
    let mut parts = Vec::new();
    for item in items.try_iter()? {
        parts.push(
            item.as_str()
                .map(ToOwned::to_owned)
                .ok_or_else(|| invalid("join expects strings"))?,
        );
    }
    Ok(JinjaValue::from(parts.join(separator)))
}

/// Refuses extra arguments rather than ignoring them: `s.find('x', 5)` must not
/// quietly search from the start.
fn at_most(args: &[JinjaValue], count: usize) -> Result<(), JinjaError> {
    if args.len() > count {
        Err(invalid("method does not take that many arguments"))
    } else {
        Ok(())
    }
}

/// `text.replace(old, new[, count])`, bounded: the result may not outgrow
/// `MAX_REPLACED` bytes, so a template cannot multiply a large input.
fn replace(text: &str, args: &[JinjaValue]) -> Result<JinjaValue, JinjaError> {
    at_most(args, 3)?;
    let old = required_str(args, 0)?;
    let new = required_str(args, 1)?;
    if old.is_empty() {
        return Err(invalid("empty substring"));
    }
    let limit = max_split(args, 2)?;
    let hits = text.matches(old).count().min(limit.unwrap_or(usize::MAX));
    let grown = new.len().saturating_sub(old.len()).saturating_mul(hits);
    if text.len().saturating_add(grown) > MAX_REPLACED {
        return Err(invalid("replace result is outside its bound"));
    }
    Ok(JinjaValue::from(match limit {
        None => text.replace(old, new),
        Some(limit) => text.replacen(old, new, limit),
    }))
}

fn string_method(text: &str, method: &str, args: &[JinjaValue]) -> Result<JinjaValue, JinjaError> {
    Ok(match method {
        "split" => return split(text, args),
        "rsplit" => return rsplit(text, args),
        "splitlines" => strings(text.lines().map(ToOwned::to_owned)),
        "strip" => return strip(text, args, true, true),
        "lstrip" => return strip(text, args, true, false),
        "rstrip" => return strip(text, args, false, true),
        "lower" => JinjaValue::from(text.to_lowercase()),
        "upper" => JinjaValue::from(text.to_uppercase()),
        "capitalize" => JinjaValue::from(capitalize(text)),
        "title" => JinjaValue::from(title(text)),
        "startswith" => {
            at_most(args, 1)?;
            JinjaValue::from(text.starts_with(required_str(args, 0)?))
        }
        "endswith" => {
            at_most(args, 1)?;
            JinjaValue::from(text.ends_with(required_str(args, 0)?))
        }
        "replace" => return replace(text, args),
        "find" => {
            at_most(args, 1)?;
            JinjaValue::from(
                text.find(required_str(args, 0)?)
                    .map_or(-1, |byte| char_index(text, byte)),
            )
        }
        "count" => {
            at_most(args, 1)?;
            let needle = required_str(args, 0)?;
            if needle.is_empty() {
                return Err(invalid("empty substring"));
            }
            JinjaValue::from(text.matches(needle).count())
        }
        "join" => return join(text, args),
        "isdigit" => JinjaValue::from(!text.is_empty() && text.chars().all(|c| c.is_ascii_digit())),
        "isalpha" => JinjaValue::from(!text.is_empty() && text.chars().all(char::is_alphabetic)),
        "isalnum" => JinjaValue::from(!text.is_empty() && text.chars().all(char::is_alphanumeric)),
        "isspace" => JinjaValue::from(!text.is_empty() && text.chars().all(char::is_whitespace)),
        _ => return Err(unknown()),
    })
}

fn map_method(
    map: &JinjaValue,
    method: &str,
    args: &[JinjaValue],
) -> Result<JinjaValue, JinjaError> {
    match method {
        "keys" => Ok(map.try_iter()?.collect()),
        "values" => map
            .try_iter()?
            .map(|key| map.get_item(&key))
            .collect::<Result<JinjaValue, JinjaError>>(),
        "items" => map
            .try_iter()?
            .map(|key| {
                let value = map.get_item(&key)?;
                Ok([key, value].into_iter().collect::<JinjaValue>())
            })
            .collect::<Result<JinjaValue, JinjaError>>(),
        "get" => {
            let key = args.first().ok_or_else(|| invalid("get needs a key"))?;
            match map.get_item(key) {
                Ok(found) if !found.is_undefined() => Ok(found),
                _ => Ok(args.get(1).cloned().unwrap_or_else(|| JinjaValue::from(()))),
            }
        }
        _ => Err(unknown()),
    }
}

/// The environment's `unknown_method_callback`: Python `str` and `dict` methods.
pub(super) fn unknown_method(
    _state: &mut JinjaState,
    value: &JinjaValue,
    method: &str,
    args: &[JinjaValue],
) -> Result<JinjaValue, JinjaError> {
    match (value.kind(), value.as_str()) {
        (ValueKind::String, Some(text)) => string_method(text, method, args),
        (ValueKind::Map, _) => map_method(value, method, args),
        _ => Err(unknown()),
    }
}

/// Jinja2's `wordcount`: runs of word characters.
pub(super) fn wordcount_filter(text: &str) -> usize {
    text.split(|c: char| !(c.is_alphanumeric() || c == '_'))
        .filter(|word| !word.is_empty())
        .count()
}

/// Jinja2's `capitalize`: first character upper, the rest lower.
pub(super) fn capitalize_filter(text: &str) -> String {
    capitalize(text)
}

/// Jinja2's `truncate(length=255, killwords=False, end='...', leeway=5)`.
pub(super) fn truncate_filter(
    text: &str,
    length: Option<usize>,
    killwords: Option<bool>,
    end: Option<&str>,
    leeway: Option<usize>,
) -> Result<String, JinjaError> {
    let length = length.unwrap_or(255);
    let end = end.unwrap_or("...");
    let leeway = leeway.unwrap_or(5);
    let end_len = end.chars().count();
    if length < end_len {
        return Err(invalid("truncate length is shorter than its end marker"));
    }
    if text.chars().count() <= length.saturating_add(leeway) {
        return Ok(text.to_owned());
    }
    let head: String = text.chars().take(length - end_len).collect();
    if killwords.unwrap_or(false) {
        return Ok(format!("{head}{end}"));
    }
    let head = head
        .rsplit_once(' ')
        .map_or(head.as_str(), |(before, _)| before);
    Ok(format!("{head}{end}"))
}

/// Jinja2's `center(width=80)`.
pub(super) fn center_filter(text: &str, width: Option<usize>) -> Result<String, JinjaError> {
    let width = width.unwrap_or(80);
    if width > MAX_WIDTH {
        return Err(invalid("center width is outside its bound"));
    }
    let length = text.chars().count();
    if length >= width {
        return Ok(text.to_owned());
    }
    let padding = width - length;
    // Python's `str.center` gives the odd column to the left when the width is odd.
    let left = padding / 2 + (padding & width & 1);
    let right = padding - left;
    Ok(format!("{}{text}{}", " ".repeat(left), " ".repeat(right)))
}
