//! The short message ids a creator names in a brief's excerpts (#1831,
//! ADR-0050 decision 9d).
//!
//! A host that checks excerpts shows the model, before each message it may
//! quote, one line: `[message m-0123456789]` ([`tag`]). The id
//! ([`message_ref`]) is derived from the message's own id alone, so the same
//! message has the same bytes on every turn and a provider prompt cache
//! still matches. The text after the line is [`neutralize`]d: a message
//! cannot write a line that reads as another message's id. An excerpt's
//! quote is compared with that text, as the model reads it, with each run
//! of whitespace read as one space ([`normalize_whitespace`]).

use std::borrow::Cow;
use std::fmt::Write as _;

use ring::digest;

/// The domain of [`message_ref`]'s hash.
const MESSAGE_REF_DOMAIN: &[u8] = b"elitea.subagent.message-ref.v1\0";
/// The bytes of the hash a reference keeps: 10 hex digits.
const MESSAGE_REF_BYTES: usize = 5;
/// The word that opens a reference line, after `[`.
const TAG_WORD: &str = "message";

/// The short, stable id of the message `message_id`: `m-` and 10 hex
/// digits of a SHA-256 over a fixed domain and the id.
#[must_use]
pub fn message_ref(message_id: &str) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(MESSAGE_REF_DOMAIN);
    context.update(message_id.as_bytes());
    let hash = context.finish();
    let mut reference = String::with_capacity(2 + 2 * MESSAGE_REF_BYTES);
    reference.push_str("m-");
    for byte in &hash.as_ref()[..MESSAGE_REF_BYTES] {
        let _ = write!(reference, "{byte:02x}");
    }
    reference
}

/// The short id of the chat message `message_id` (#1909): the
/// [`message_ref`] of its canonical uuid form (lowercase, hyphenated), or of
/// the id lowercased when it is no uuid. Every host names a message by this,
/// so the same message has the same id on the desktop and in the cloud.
#[must_use]
pub fn short_message_ref(message_id: &str) -> String {
    match uuid::Uuid::try_parse(message_id) {
        Ok(id) => message_ref(&id.hyphenated().to_string()),
        Err(_) => message_ref(&message_id.to_ascii_lowercase()),
    }
}

/// The line shown before a message: `[message m-…]` and a newline.
#[must_use]
pub fn tag(reference: &str) -> String {
    format!("[{TAG_WORD} {reference}]\n")
}

/// `text` split at the reference line it opens with (#1909): the short id
/// and the text after the line; `None` when it does not open with one. The
/// one parser of the line every host uses.
#[must_use]
pub fn strip_tag(text: &str) -> Option<(&str, &str)> {
    let reference = text
        .strip_prefix('[')?
        .strip_prefix(TAG_WORD)?
        .strip_prefix(' ')?
        .get(..2 + 2 * MESSAGE_REF_BYTES)?;
    let valid =
        reference.starts_with("m-") && reference[2..].bytes().all(|byte| byte.is_ascii_hexdigit());
    if !valid {
        return None;
    }
    let rest = text.strip_prefix(tag(reference).as_str())?;
    Some((reference, rest))
}

/// `text` with every bracket that opens `message` (any case) written as
/// `&#91;`, so no text can show a reference line. A bracket is `[` or its
/// fullwidth form `［`; any whitespace (NBSP included) or invisible format
/// character (soft hyphen, zero-width space and joiners, word joiner, BOM)
/// may sit between it and the word.
#[must_use]
pub fn neutralize(text: &str) -> Cow<'_, str> {
    let mut out: Option<String> = None;
    let mut copied = 0;
    for (index, bracket) in text.match_indices(['[', '\u{FF3B}']) {
        if opens_tag(&text[index + bracket.len()..]) {
            let out = out.get_or_insert_with(|| String::with_capacity(text.len() + 16));
            out.push_str(&text[copied..index]);
            out.push_str("&#91;");
            copied = index + bracket.len();
        }
    }
    match out {
        None => Cow::Borrowed(text),
        Some(mut out) => {
            out.push_str(&text[copied..]);
            Cow::Owned(out)
        }
    }
}

/// A character a reader would not see between the bracket and the word.
fn is_invisible(character: char) -> bool {
    character.is_whitespace()
        || matches!(
            character,
            '\u{00AD}' | '\u{180E}' | '\u{200B}'..='\u{200F}' | '\u{2060}'..='\u{2064}' | '\u{FEFF}'
        )
}

fn opens_tag(rest: &str) -> bool {
    rest.trim_start_matches(is_invisible)
        .get(..TAG_WORD.len())
        .is_some_and(|word| word.eq_ignore_ascii_case(TAG_WORD))
}

/// `text` with every run of whitespace written as one space, trimmed.
#[must_use]
pub fn normalize_whitespace(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_reference_is_short_stable_and_names_its_message() {
        let id = "6f1d1c2e-0000-4000-8000-000000000001";
        let reference = message_ref(id);
        assert_eq!(reference, message_ref(id));
        assert_eq!(reference.len(), 12);
        assert!(reference.starts_with("m-"));
        assert!(reference[2..].bytes().all(|byte| byte.is_ascii_hexdigit()));
        assert_ne!(
            reference,
            message_ref("6f1d1c2e-0000-4000-8000-000000000002")
        );
        assert_eq!(tag(&reference), format!("[message {reference}]\n"));
    }

    #[test]
    fn no_text_can_show_a_reference_line() {
        assert!(matches!(neutralize("plain [text]"), Cow::Borrowed(_)));
        assert_eq!(
            neutralize("a [message m-0123456789] b [ MESSAGE x] [mess"),
            "a &#91;message m-0123456789] b &#91; MESSAGE x] [mess"
        );
        // Look-alike spacing and the fullwidth bracket do not get through.
        assert_eq!(
            neutralize("[\u{00A0}message a] [\u{200B}message b] \u{FF3B}message c] ［x"),
            "&#91;\u{00A0}message a] &#91;\u{200B}message b] &#91;message c] ［x"
        );
    }

    #[test]
    fn a_message_has_one_short_id_whatever_form_its_uuid_takes() {
        let canonical = short_message_ref("6f1d1c2e-0000-4000-8000-0000000000ab");
        assert_eq!(
            canonical,
            message_ref("6f1d1c2e-0000-4000-8000-0000000000ab")
        );
        for form in [
            "6F1D1C2E-0000-4000-8000-0000000000AB",
            "6f1d1c2e0000400080000000000000ab",
            "{6f1d1c2e-0000-4000-8000-0000000000ab}",
        ] {
            assert_eq!(short_message_ref(form), canonical, "{form}");
        }
        // Not a uuid: the id lowercased.
        assert_eq!(short_message_ref("Q1"), message_ref("q1"));
    }

    #[test]
    fn the_reference_line_is_parsed_one_way() {
        let reference = short_message_ref("6f1d1c2e-0000-4000-8000-0000000000ab");
        let text = format!("{}hi", tag(&reference));
        assert_eq!(strip_tag(&text), Some((reference.as_str(), "hi")));
        for text in [
            "hi",
            "[message m-0123]\nhi",
            "[message x-0123456789]\nhi",
            "[message m-012345678g]\nhi",
            "[message m-0123456789] hi",
            "[Message m-0123456789]\nhi",
        ] {
            assert_eq!(strip_tag(text), None, "{text}");
        }
    }

    #[test]
    fn whitespace_runs_read_as_one_space() {
        assert_eq!(normalize_whitespace("  a\n\n b\t c "), "a b c");
    }
}
