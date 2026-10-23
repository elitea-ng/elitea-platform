//! The naming rules of the platform's artifact store, shared by every
//! caller that builds a bucket name or an object key (the indexing source,
//! the worker's Office host, the image toolkit).
//!
//! The store itself is reached over elitea-main's
//! `elitea.runtime.v1.ArtifactObjectService` (#1359 U5), by a host that
//! holds the platform channel; this crate stays transport-free.

const MAX_NAME_BYTES: usize = 1_024;

/// A bucket (or project) name: one non-empty path segment.
#[must_use]
pub fn valid_segment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_NAME_BYTES
        && !matches!(value, "." | "..")
        && !value.contains(['/', '\\', '\0'])
        && !value.chars().any(char::is_control)
}

/// elitea-main's object-key rules: no NUL, no backslash, no empty, `.` or
/// `..` segment (so no absolute path).
#[must_use]
pub fn valid_object_key(key: &str) -> bool {
    crate::git_id::valid_key(key)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bucket_names_are_one_segment() {
        assert!(valid_segment("docs"));
        for name in ["", ".", "..", "a/b", "a\\b", "a\0b", "a\nb"] {
            assert!(!valid_segment(name), "{name:?}");
        }
    }

    #[test]
    fn object_keys_follow_the_platform_rules() {
        assert!(valid_object_key("docs/a.md"));
        for key in ["", "/a", "a//b", "a/../b", "a\\b", "a\0b", "."] {
            assert!(!valid_object_key(key), "{key:?}");
        }
    }
}
