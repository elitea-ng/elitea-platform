//! Path safety utilities for workspace operations.
//!
//! All file paths in sandbox sessions are relative to the workspace root.
//! This module provides validation to reject absolute paths and traversal
//! attempts that would escape the workspace boundary.

use crate::SandboxError;

/// Validates that a path is relative and does not escape the workspace root.
///
/// The function checks that:
/// - The path is non-empty
/// - The path is not absolute (does not start with `/` or `\`)
/// - The path does not contain `..` components that would escape the root
///
/// Paths with `..` that stay within bounds are allowed (e.g., `a/b/../c`
/// resolves to `a/c` which is still inside the workspace).
///
/// # Errors
///
/// Returns `SandboxError::PathTraversal` if the path is absolute or
/// contains traversal components that would escape the workspace root.
///
/// # Examples
///
/// ```
/// use adk_sandbox::workspace::path_safety::validate_relative_path;
///
/// // Valid paths
/// assert!(validate_relative_path("file.txt").is_ok());
/// assert!(validate_relative_path("src/main.rs").is_ok());
/// assert!(validate_relative_path("a/b/../c/file.txt").is_ok());
/// assert!(validate_relative_path("./file.txt").is_ok());
///
/// // Invalid paths
/// assert!(validate_relative_path("").is_err());
/// assert!(validate_relative_path("/etc/passwd").is_err());
/// assert!(validate_relative_path("../escape").is_err());
/// assert!(validate_relative_path("a/../../escape").is_err());
/// ```
pub fn validate_relative_path(path: &str) -> Result<(), SandboxError> {
    // Reject empty paths
    if path.is_empty() {
        return Err(SandboxError::PathTraversal { path: path.to_string() });
    }

    // Reject absolute paths (Unix `/` or Windows `\` prefix)
    if path.starts_with('/') || path.starts_with('\\') {
        return Err(SandboxError::PathTraversal { path: path.to_string() });
    }

    // Also reject Windows-style drive letters (e.g., "C:\...")
    if path.len() >= 2 {
        let bytes = path.as_bytes();
        if bytes[0].is_ascii_alphabetic() && (bytes[1] == b':') {
            return Err(SandboxError::PathTraversal { path: path.to_string() });
        }
    }

    // Normalize and check for traversal beyond root.
    // We simulate path resolution by tracking depth relative to root.
    // Split on both `/` and `\` to handle mixed separators.
    let mut depth: i32 = 0;

    for component in path.split(['/', '\\']) {
        match component {
            // Empty components from consecutive separators or trailing slash
            "" => continue,
            // Current directory — no-op
            "." => continue,
            // Parent directory — decrease depth
            ".." => {
                depth -= 1;
                if depth < 0 {
                    return Err(SandboxError::PathTraversal { path: path.to_string() });
                }
            }
            // Normal component — increase depth
            _ => {
                depth += 1;
            }
        }
    }

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_valid_simple_file() {
        assert!(validate_relative_path("file.txt").is_ok());
    }

    #[test]
    fn test_valid_nested_path() {
        assert!(validate_relative_path("src/main.rs").is_ok());
    }

    #[test]
    fn test_valid_dotdot_within_bounds() {
        assert!(validate_relative_path("a/b/../c/file.txt").is_ok());
    }

    #[test]
    fn test_valid_dot_component() {
        assert!(validate_relative_path("./file.txt").is_ok());
    }

    #[test]
    fn test_valid_multiple_dots() {
        assert!(validate_relative_path("./src/./main.rs").is_ok());
    }

    #[test]
    fn test_valid_deep_path() {
        assert!(validate_relative_path("a/b/c/d/e/f.txt").is_ok());
    }

    #[test]
    fn test_invalid_empty_path() {
        let result = validate_relative_path("");
        assert!(result.is_err());
        match result.unwrap_err() {
            SandboxError::PathTraversal { path } => assert_eq!(path, ""),
            other => panic!("expected PathTraversal, got: {other:?}"),
        }
    }

    #[test]
    fn test_invalid_absolute_unix() {
        let result = validate_relative_path("/etc/passwd");
        assert!(result.is_err());
        match result.unwrap_err() {
            SandboxError::PathTraversal { path } => assert_eq!(path, "/etc/passwd"),
            other => panic!("expected PathTraversal, got: {other:?}"),
        }
    }

    #[test]
    fn test_invalid_parent_escape() {
        assert!(validate_relative_path("../escape").is_err());
    }

    #[test]
    fn test_invalid_deep_parent_escape() {
        assert!(validate_relative_path("a/../../escape").is_err());
    }

    #[test]
    fn test_invalid_triple_parent_escape() {
        assert!(validate_relative_path("a/b/../../../escape").is_err());
    }

    #[test]
    fn test_invalid_backslash_absolute() {
        assert!(validate_relative_path("\\Windows\\System32").is_err());
    }

    #[test]
    fn test_invalid_windows_drive() {
        assert!(validate_relative_path("C:\\Users\\file.txt").is_err());
    }

    #[test]
    fn test_valid_dotdot_exact_boundary() {
        // a/b/.. resolves to a/ — still within workspace
        assert!(validate_relative_path("a/b/..").is_ok());
    }

    #[test]
    fn test_valid_multiple_dotdot_within_bounds() {
        // a/b/c/../../d resolves to a/d — still within workspace
        assert!(validate_relative_path("a/b/c/../../d").is_ok());
    }

    #[test]
    fn test_invalid_just_dotdot() {
        assert!(validate_relative_path("..").is_err());
    }
}
