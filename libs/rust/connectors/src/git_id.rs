//! The one git object-id check every provider module shares. Public through
//! [`crate::source::valid_git_object_id`]; it lives here so the REST clients,
//! which build without the `content-source` feature, can call it.

/// A full git object id: 40 (SHA-1) or 64 (SHA-256) hex digits.
#[must_use]
pub fn valid_git_object_id(id: &str) -> bool {
    matches!(id.len(), 40 | 64) && id.bytes().all(|byte| byte.is_ascii_hexdigit())
}
