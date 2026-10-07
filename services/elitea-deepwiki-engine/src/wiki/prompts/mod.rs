//! The page-generation prompts, verbatim (ADR-0026 decision 8).
//!
//! Each prompt is a data file next to this module, byte for byte the
//! Python value; `PROMPTS_MANIFEST.json` records where it came from and
//! the SHA-256 of the Python value, and `tests/wiki_prompts.rs` re-derives
//! every hash from the Python source in this repository.
//!
//! MERGE NOTE: the structure-planner port keeps its prompts in its own
//! manifest; the two manifests merge into one list.

use crate::errors::{EngineError, ErrorType};

/// `ENHANCED_CONTENT_GENERATION_PROMPT_V3_TONE_ADJUSTED`: the human
/// template `_generate_simple` formats.
pub const PAGE_CONTENT_V3_TONE_ADJUSTED: &str = include_str!("page_content_v3_tone_adjusted.txt");

/// `_generate_simple`'s system message.
pub const PAGE_CONTENT_SYSTEM: &str = include_str!("page_content_system.txt");

/// `TARGET_AUDIENCES["mixed"]` (the agent's `TargetAudience.MIXED`).
pub const TARGET_AUDIENCE_MIXED: &str = include_str!("target_audience_mixed.txt");

/// The manifest, for the tests.
pub const MANIFEST: &str = include_str!("PROMPTS_MANIFEST.json");

/// Format a `LangChain` f-string template (`ChatPromptTemplate`):
/// `{name}` is replaced by the value, `{{` / `}}` are literal braces, and
/// the values are inserted as they are (never re-formatted).
///
/// # Errors
///
/// An unknown or malformed field, or a single `}` (`str.format`'s errors).
pub fn format_template(template: &str, values: &[(&str, &str)]) -> Result<String, EngineError> {
    let invalid = |message: String| EngineError::new(ErrorType::Value, message);
    let mut out =
        String::with_capacity(template.len() + values.iter().map(|(_, v)| v.len()).sum::<usize>());
    let mut chars = template.char_indices().peekable();
    while let Some((at, c)) = chars.next() {
        match c {
            '{' if chars.peek().map(|&(_, n)| n) == Some('{') => {
                chars.next();
                out.push('{');
            }
            '{' => {
                let rest = &template[at + 1..];
                let Some(close) = rest.find('}') else {
                    return Err(invalid(
                        "Single '{' encountered in format string".to_owned(),
                    ));
                };
                let name = &rest[..close];
                let Some((_, value)) = values.iter().find(|(n, _)| *n == name) else {
                    return Err(invalid(format!("missing template variable '{name}'")));
                };
                out.push_str(value);
                for _ in 0..=name.chars().count() {
                    chars.next();
                }
            }
            '}' if chars.peek().map(|&(_, n)| n) == Some('}') => {
                chars.next();
                out.push('}');
            }
            '}' => {
                return Err(invalid(
                    "Single '}' encountered in format string".to_owned(),
                ));
            }
            other => out.push(other),
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formats_like_str_format() {
        assert_eq!(
            format_template("a {x} {{y}} {x}", &[("x", "{z}")]).unwrap(),
            "a {z} {y} {z}"
        );
        assert!(format_template("{nope}", &[]).is_err());
        assert!(format_template("}", &[]).is_err());
    }

    #[test]
    fn the_page_template_has_exactly_the_python_fields() {
        let values = [
            ("section_name", ""),
            ("page_name", ""),
            ("page_description", ""),
            ("content_focus", ""),
            ("repository_url", ""),
            ("wiki_style", ""),
            ("repository_context", ""),
            ("relevant_content", ""),
            ("related_files", ""),
            ("target_audience", ""),
        ];
        assert!(format_template(PAGE_CONTENT_V3_TONE_ADJUSTED, &values).is_ok());
    }
}
