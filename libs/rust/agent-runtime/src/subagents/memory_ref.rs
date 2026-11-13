//! How a brief cites an agent memory entry (#1900, ADR-0050 decision 9d).
//!
//! Main shows each entry it recalled into the instructions with a mark,
//! `[memory N·TAG, ...]` (`agentmemory.Render`): N the entry id and TAG a
//! tag Main draws for every turn (`AgentExecutionInputV1.recalled_memory_tag`).
//! The author's instructions and skill bodies share the instructions with
//! those marks and are never rewritten, so a copied `[memory N, ...]` line
//! stays there; since nobody can know a turn's tag in advance, a host
//! accepts a `context.memory_entries` reference only as `N·TAG` with the
//! run's tag ([`cited_entry`]), so a brief cannot cite a copied mark.
//!
//! The tag's shape is Main's (`agentmemory.MarkTagLength`, its alphabet);
//! change both together.

use ring::rand::{SecureRandom, SystemRandom};

/// The characters of a tag.
pub const MEMORY_TAG_LENGTH: usize = 10;
/// Lowercase base32: 5 bits a character, 50 a tag.
const MEMORY_TAG_ALPHABET: &[u8; 32] = b"abcdefghijklmnopqrstuvwxyz234567";
/// Joins an entry id and the tag: a middle dot, which no decimal id holds.
pub const MEMORY_REF_SEPARATOR: char = '\u{00B7}';
/// The most digits of an entry id (a positive `bigint`).
const MAX_ENTRY_ID_DIGITS: usize = 19;

/// A fresh tag from the operating system's CSPRNG, for a run Main sent
/// none: the entries `recall` returns are then cited with it. `None` only
/// when the system's random source fails, and then no entry can be cited.
#[must_use]
pub fn new_memory_tag() -> Option<String> {
    let mut random = [0_u8; MEMORY_TAG_LENGTH];
    SystemRandom::new().fill(&mut random).ok()?;
    // 256 is a multiple of 32: masking keeps every character equally likely.
    Some(
        random
            .iter()
            .map(|byte| char::from(MEMORY_TAG_ALPHABET[usize::from(byte & 31)]))
            .collect(),
    )
}

/// Whether `tag` has Main's shape.
#[must_use]
pub fn valid_memory_tag(tag: &str) -> bool {
    tag.len() == MEMORY_TAG_LENGTH && tag.bytes().all(|byte| MEMORY_TAG_ALPHABET.contains(&byte))
}

/// How a brief cites entry `entry_id` under `tag`: `N·TAG`.
#[must_use]
pub fn memory_ref(entry_id: &str, tag: &str) -> String {
    format!("{entry_id}{MEMORY_REF_SEPARATOR}{tag}")
}

/// The entry id `reference` cites under the run's `tag`, or `None`: a bare
/// id, another tag, or anything but `N·TAG` with N a positive decimal id
/// without a leading zero.
#[must_use]
pub fn cited_entry<'a>(reference: &'a str, tag: &str) -> Option<&'a str> {
    if !valid_memory_tag(tag) {
        return None;
    }
    let (id, cited) = reference.split_once(MEMORY_REF_SEPARATOR)?;
    let numeric = !id.is_empty()
        && id.len() <= MAX_ENTRY_ID_DIGITS
        && !id.starts_with('0')
        && id.bytes().all(|byte| byte.is_ascii_digit());
    (numeric && cited == tag).then_some(id)
}

#[cfg(test)]
mod tests {
    use super::*;

    const TAG: &str = "k7f2m6q3xz";

    #[test]
    fn a_reference_cites_an_entry_only_with_the_run_tag() {
        assert_eq!(memory_ref("42", TAG), "42·k7f2m6q3xz");
        assert_eq!(cited_entry("42·k7f2m6q3xz", TAG), Some("42"));
        for refused in [
            // A bare id: what a copied `[memory 42, ...]` line shows.
            "42",
            "[memory 42]",
            // Another run's tag, or a guess.
            "42·aaaaaaaaaa",
            "42·K7F2M6Q3XZ",
            "42·k7f2m6q3x",
            "42·k7f2m6q3xz·k7f2m6q3xz",
            "042·k7f2m6q3xz",
            "·k7f2m6q3xz",
            "4a·k7f2m6q3xz",
            "42.k7f2m6q3xz",
        ] {
            assert_eq!(cited_entry(refused, TAG), None, "{refused}");
        }
        // No tag for the run: nothing is citable.
        assert_eq!(cited_entry("42·", ""), None);
    }

    #[test]
    fn fresh_tags_have_the_shape_and_differ() {
        let first = new_memory_tag().expect("tag");
        let second = new_memory_tag().expect("tag");
        assert!(valid_memory_tag(&first) && valid_memory_tag(&second));
        assert_ne!(first, second);
        for invalid in ["", "k7f2m6q3x", "k7f2m6q3xz1", "k7f2m6q3x0", "k7f2m6q3x-"] {
            assert!(!valid_memory_tag(invalid), "{invalid}");
        }
    }
}
