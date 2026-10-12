//! Entity ids: `IngestionPipeline._generate_entity_id`, exactly.
//!
//! The id decides which entities MERGE: two entities with one id are one
//! node with two citations. A graph built here and one built by the Python
//! engine must agree on every id, or a cutover re-ingest splits and merges
//! nodes differently. `tests/fixtures/ingest/entity_ids.json` holds ids the
//! Python function computed.

use elitea_engine_core::pystr;
use md5::{Digest, Md5};
use std::fmt::Write as _;

/// `CONTEXT_DEPENDENT_TYPES`: always scoped to their file.
const CONTEXT_DEPENDENT_TYPES: [&str; 22] = [
    "tool",
    "property",
    "properties",
    "parameter",
    "argument",
    "field",
    "column",
    "attribute",
    "option",
    "setting",
    "step",
    "test_step",
    "ui_field",
    "endpoint",
    "method",
    "mcp_tool",
    "mcp_resource",
    "file",
    "source_file",
    "document_file",
    "config_file",
    "web_file",
];

/// Code-structural types: scoped to their file only when the name is short.
const CODE_STRUCTURAL_TYPES: [&str; 10] = [
    "function",
    "variable",
    "constant",
    "class",
    "import",
    "module",
    "interface",
    "enum",
    "struct",
    "trait",
];

/// `re.split(r'[\s_]+|(?<=[a-z])(?=[A-Z])', name.strip())` without the
/// empty parts: whitespace and underscore runs, and every ASCII
/// lower-to-upper boundary.
fn semantic_word_count(name: &str) -> usize {
    let mut count = 0;
    let mut in_word = false;
    let mut previous: Option<char> = None;
    for c in pystr::strip(name).chars() {
        if c == '_' || pystr::is_space(c) {
            in_word = false;
        } else {
            let boundary =
                previous.is_some_and(|p| p.is_ascii_lowercase()) && c.is_ascii_uppercase();
            if !in_word || boundary {
                count += 1;
            }
            in_word = true;
        }
        previous = Some(c);
    }
    count
}

/// The id of an entity of `entity_type` named `name`, found in `file_path`.
#[must_use]
pub fn entity_id(entity_type: &str, name: &str, file_path: Option<&str>) -> String {
    let normalized_name = pystr::strip(&name.to_lowercase()).to_owned();
    let normalized_type = pystr::strip(&entity_type.to_lowercase()).to_owned();
    let file_scoped = CONTEXT_DEPENDENT_TYPES.contains(&normalized_type.as_str())
        || (CODE_STRUCTURAL_TYPES.contains(&normalized_type.as_str())
            && (semantic_word_count(name) <= 2 || normalized_name.chars().count() <= 15));
    let content = match file_path.filter(|path| file_scoped && !path.is_empty()) {
        Some(path) => format!("{normalized_type}:{normalized_name}:{path}"),
        None => format!("{normalized_type}:{normalized_name}"),
    };
    let digest = Md5::digest(content.as_bytes());
    let mut hex = String::with_capacity(32);
    for byte in digest {
        let _ = write!(hex, "{byte:02x}");
    }
    hex.truncate(12);
    hex
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn words_split_on_separators_and_camel_case() {
        assert_eq!(semantic_word_count("UserAuthenticationService"), 3);
        assert_eq!(semantic_word_count("get_user_by_id"), 4);
        assert_eq!(semantic_word_count("  spaced  out  "), 2);
        assert_eq!(semantic_word_count("HTTPServer"), 1);
        assert_eq!(semantic_word_count("parseHTTP"), 2);
        assert_eq!(semantic_word_count("__init__"), 1);
        assert_eq!(semantic_word_count(""), 0);
    }
}
