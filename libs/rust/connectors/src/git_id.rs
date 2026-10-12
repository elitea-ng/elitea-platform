//! The identifier and path checks every provider module shares. Public through
//! [`crate::source`] (`valid_git_object_id`, `valid_key`); they live here so
//! the REST clients, which build without the `content-source` feature, can
//! call them. Nothing here is compiled for a crate that wants the egress rule
//! alone (`elitea-repo-ingest`).

/// A full git object id: 40 (SHA-1) or 64 (SHA-256) hex digits.
#[must_use]
pub fn valid_git_object_id(id: &str) -> bool {
    matches!(id.len(), 40 | 64) && id.bytes().all(|byte| byte.is_ascii_hexdigit())
}

/// A repository-relative path a connector may key a document by: no empty,
/// `.` or `..` segment, no NUL or backslash, no leading `/`.
#[must_use]
pub fn valid_key(key: &str) -> bool {
    !key.is_empty()
        && !key.contains(['\0', '\\'])
        && key
            .split('/')
            .all(|segment| !matches!(segment, "" | "." | ".."))
}
