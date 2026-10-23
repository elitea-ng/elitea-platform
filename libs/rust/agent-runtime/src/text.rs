//! Small text helpers the runtime and its hosts share.

/// The longest prefix of `text` that is at most `max` bytes and ends at a
/// character boundary: a byte bound that never splits a character.
#[must_use]
pub fn clip_bytes(text: &str, max: usize) -> &str {
    &text[..text.floor_char_boundary(max)]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clips_at_a_character_boundary_within_the_bound() {
        assert_eq!(clip_bytes("short", 10), "short");
        assert_eq!(clip_bytes("abcdef", 3), "abc");
        // "é" is two bytes: three bytes keep one and a half, so one.
        assert_eq!(clip_bytes("éé", 3), "é");
        assert_eq!(clip_bytes("é", 1), "");
        assert_eq!(clip_bytes("", 0), "");
    }
}
