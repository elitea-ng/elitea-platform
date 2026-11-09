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

/// The line shown before a message: `[message m-…]` and a newline.
#[must_use]
pub fn tag(reference: &str) -> String {
    format!("[{TAG_WORD} {reference}]\n")
}

/// `text` with the `[` of every `[message` (any case, spaces allowed after
/// the bracket) written as `&#91;`, so no text can show a reference line.
#[must_use]
pub fn neutralize(text: &str) -> Cow<'_, str> {
    let bytes = text.as_bytes();
    let mut out: Option<String> = None;
    let mut copied = 0;
    for (index, _) in text.match_indices('[') {
        if opens_tag(&bytes[index + 1..]) {
            let out = out.get_or_insert_with(|| String::with_capacity(text.len() + 16));
            out.push_str(&text[copied..index]);
            out.push_str("&#91;");
            copied = index + 1;
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

fn opens_tag(rest: &[u8]) -> bool {
    let start = rest
        .iter()
        .position(|byte| !byte.is_ascii_whitespace())
        .unwrap_or(rest.len());
    rest.get(start..start + TAG_WORD.len())
        .is_some_and(|word| word.eq_ignore_ascii_case(TAG_WORD.as_bytes()))
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
    }

    #[test]
    fn whitespace_runs_read_as_one_space() {
        assert_eq!(normalize_whitespace("  a\n\n b\t c "), "a b c");
    }
}
