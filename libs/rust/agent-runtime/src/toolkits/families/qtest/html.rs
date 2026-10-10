//! The SDK's `_clean_html_content`: `html.unescape`, embedded base64
//! `<img>` removal, then sklearn's `strip_tags` (every tag becomes a space).
//!
//! The SDK can instead transcribe embedded images with an LLM
//! (`extract_images=True`); this runtime has no in-family LLM, so embedded
//! images are always removed. The scanners below implement the SDK's
//! regular expressions by hand so nothing here can fail at runtime.

/// `_clean_html_content(content)` with images removed.
pub(super) fn clean_html(content: &str) -> String {
    if content.is_empty() {
        return String::new();
    }
    strip_tags(&remove_embedded_images(&unescape(content)))
}

/// sklearn's `strip_tags`: `re.sub(r"<([^>]+)>", " ", s)`.
pub(super) fn strip_tags(content: &str) -> String {
    let mut output = String::with_capacity(content.len());
    let mut rest = content;
    while let Some(open) = rest.find('<') {
        output.push_str(&rest[..open]);
        let after = &rest[open + 1..];
        match after.find('>') {
            Some(close) if close > 0 => {
                output.push(' ');
                rest = &after[close + 1..];
            }
            _ => {
                output.push('<');
                rest = after;
            }
        }
    }
    output.push_str(rest);
    output
}

/// `_process_image(content, extract=False)`: each `<img ...>` (or escaped
/// `&lt;img ...&gt;`) whose `src` is a base64 `data:image/` URI is removed;
/// other tags stay for `strip_tags`.
fn remove_embedded_images(content: &str) -> String {
    let mut output = String::with_capacity(content.len());
    let mut index = 0;
    while index < content.len() {
        let rest = &content[index..];
        let tag_end = if starts_with_ignore_case(rest, "<img") && word_boundary(rest, 4) {
            rest.find('>').map(|close| close + 1)
        } else if starts_with_ignore_case(rest, "&lt;img") && word_boundary(rest, 7) {
            rest.find("&gt;").map(|close| close + 4)
        } else {
            None
        };
        if let Some(end) = tag_end {
            let tag = &rest[..end];
            if !has_base64_image_source(&unescape(tag)) {
                output.push_str(tag);
            }
            index += end;
            continue;
        }
        let Some(character) = rest.chars().next() else {
            break;
        };
        output.push(character);
        index += character.len_utf8();
    }
    output
}

fn starts_with_ignore_case(text: &str, prefix: &str) -> bool {
    text.get(..prefix.len())
        .is_some_and(|head| head.eq_ignore_ascii_case(prefix))
}

/// Regex `\b` after an ASCII word: the next character is not a word one.
fn word_boundary(text: &str, at: usize) -> bool {
    text[at..]
        .chars()
        .next()
        .is_none_or(|next| !(next.is_alphanumeric() || next == '_'))
}

/// `\bsrc\s*=\s*["']data:image/[^;]+;base64,[^"']+["']`, case-insensitive.
fn has_base64_image_source(tag: &str) -> bool {
    let lower = tag.to_ascii_lowercase();
    let mut search = 0;
    while let Some(found) = lower[search..].find("src") {
        let start = search + found;
        search = start + 3;
        let preceded_by_word = lower[..start]
            .chars()
            .next_back()
            .is_some_and(|previous| previous.is_alphanumeric() || previous == '_');
        if preceded_by_word {
            continue;
        }
        let value = lower[start + 3..].trim_start();
        let Some(value) = value.strip_prefix('=') else {
            continue;
        };
        let value = value.trim_start();
        let Some(uri) = value
            .strip_prefix(['"', '\''])
            .and_then(|value| value.strip_prefix("data:image/"))
        else {
            continue;
        };
        let Some(semicolon) = uri.find(';').filter(|semicolon| *semicolon > 0) else {
            continue;
        };
        let Some(data) = uri[semicolon..].strip_prefix(";base64,") else {
            continue;
        };
        if data.find(['"', '\'']).is_some_and(|end| end > 0) {
            return true;
        }
    }
    false
}

/// `html.unescape` for numeric references and the named entities qTest's
/// rich-text editor writes; an unknown name is left as written.
pub(super) fn unescape(content: &str) -> String {
    if !content.contains('&') {
        return content.to_owned();
    }
    let mut output = String::with_capacity(content.len());
    let mut rest = content;
    while let Some(ampersand) = rest.find('&') {
        output.push_str(&rest[..ampersand]);
        let after = &rest[ampersand + 1..];
        if let Some((character, consumed)) = reference(after) {
            output.push(character);
            rest = &after[consumed..];
        } else {
            output.push('&');
            rest = after;
        }
    }
    output.push_str(rest);
    output
}

/// One character reference after `&`: the decoded character and the bytes
/// it used.
fn reference(text: &str) -> Option<(char, usize)> {
    if let Some(numeric) = text.strip_prefix('#') {
        let (digits, radix, offset) = match numeric.strip_prefix(['x', 'X']) {
            Some(hex) => (hex, 16, 2),
            None => (numeric, 10, 1),
        };
        let length = digits
            .bytes()
            .take_while(|byte| match radix {
                16 => byte.is_ascii_hexdigit(),
                _ => byte.is_ascii_digit(),
            })
            .count();
        if length == 0 || length > 7 {
            return None;
        }
        let character = u32::from_str_radix(&digits[..length], radix)
            .ok()
            .and_then(char::from_u32)?;
        let semicolon = usize::from(digits[length..].starts_with(';'));
        return Some((character, offset + length + semicolon));
    }
    let length = text.bytes().take_while(u8::is_ascii_alphanumeric).count();
    if length == 0 || !text[length..].starts_with(';') {
        return None;
    }
    named(&text[..length]).map(|character| (character, length + 1))
}

fn named(name: &str) -> Option<char> {
    Some(match name {
        "amp" | "AMP" => '&',
        "lt" | "LT" => '<',
        "gt" | "GT" => '>',
        "quot" | "QUOT" => '"',
        "apos" => '\'',
        "nbsp" => '\u{a0}',
        "copy" => '\u{a9}',
        "reg" => '\u{ae}',
        "trade" => '\u{2122}',
        "hellip" => '\u{2026}',
        "ndash" => '\u{2013}',
        "mdash" => '\u{2014}',
        "lsquo" => '\u{2018}',
        "rsquo" => '\u{2019}',
        "ldquo" => '\u{201c}',
        "rdquo" => '\u{201d}',
        "bull" => '\u{2022}',
        "middot" => '\u{b7}',
        "laquo" => '\u{ab}',
        "raquo" => '\u{bb}',
        "deg" => '\u{b0}',
        "times" => '\u{d7}',
        "divide" => '\u{f7}',
        "euro" => '\u{20ac}',
        "pound" => '\u{a3}',
        "yen" => '\u{a5}',
        "cent" => '\u{a2}',
        "sect" => '\u{a7}',
        "para" => '\u{b6}',
        "larr" => '\u{2190}',
        "rarr" => '\u{2192}',
        "uarr" => '\u{2191}',
        "darr" => '\u{2193}',
        "check" => '\u{2713}',
        _ => return None,
    })
}

#[cfg(test)]
mod tests {
    use super::{clean_html, strip_tags};

    #[test]
    fn cleans_like_the_sdk() {
        assert_eq!(
            clean_html("<p>Login &amp; logout</p>&lt;b&gt;x&lt;/b&gt;"),
            " Login & logout  x "
        );
        assert_eq!(
            clean_html(
                r#"a<img src="data:image/png;base64,AAAA" />b<IMG alt="x" src='https://x/y.png'>c<imgx>"#
            ),
            "ab c "
        );
        assert_eq!(
            clean_html("caf&#233; &#x41; &unknown; a&b"),
            "café A &unknown; a&b"
        );
        assert_eq!(strip_tags("a <> b < c"), "a <> b < c");
        assert_eq!(clean_html(""), "");
    }
}
