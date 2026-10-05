//! What a wiki is named after: the repository string, an artifact-folder
//! source, and the canonical wiki id derived from them.
//!
//! Ports of `elitea_deepwiki.wiki_context.display_repository_for` /
//! `wiki_id_for` and `elitea_deepwiki.artifact_source.parse_artifact_source`.
//! The Go host carries a third copy (`run.DisplayRepositoryFor`); all of
//! them must agree, because a wiki id is an object-key prefix and the key
//! the browser matches a manifest on.

use crate::errors::{EngineError, ErrorType};
use regex::Regex;
use serde_json::Value;
use std::sync::LazyLock;

/// The scheme that marks an artifact folder rather than a git repository.
pub const ARTIFACT_SCHEME: &str = "artifact://";

/// elitea-main's bucket-name rule.
static BUCKET_PATTERN: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z][a-z0-9-]{1,62}$").unwrap_or_else(|_| unreachable!()));

/// The longest object key elitea-main accepts, in bytes.
pub const MAX_KEY_BYTES: usize = 1024;

/// One artifact folder: a bucket, and an optional folder prefix without a
/// trailing slash ("" is the whole bucket).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ArtifactSource {
    pub bucket: String,
    pub prefix: String,
}

/// Python's `str(value)` for the JSON values a repository field can hold.
#[must_use]
pub fn py_str(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        Value::Null => "None".to_owned(),
        Value::Bool(true) => "True".to_owned(),
        Value::Bool(false) => "False".to_owned(),
        Value::Number(number) => number.to_string(),
        other => other.to_string(),
    }
}

/// Python truthiness of a JSON value.
#[must_use]
pub fn py_truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|n| n != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(map) => !map.is_empty(),
    }
}

/// `str(mapping.get(key) or "")`.
fn str_or_empty(mapping: &serde_json::Map<String, Value>, key: &str) -> String {
    match mapping.get(key) {
        Some(value) if py_truthy(value) => py_str(value),
        _ => String::new(),
    }
}

/// Report whether a repository string names an artifact folder.
#[must_use]
pub fn is_artifact_source(repository: &str) -> bool {
    repository
        .trim()
        .to_lowercase()
        .starts_with(ARTIFACT_SCHEME)
}

fn source_error(message: String) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

/// Parse `artifact://bucket[/prefix]`, refusing anything else.
///
/// # Errors
///
/// A `ValueError` (Python's `ArtifactSourceError`) for a string that is not
/// an artifact source, an unusable bucket name or an unsafe prefix.
pub fn parse_artifact_source(repository: &str) -> Result<ArtifactSource, EngineError> {
    if !is_artifact_source(repository) {
        return Err(source_error(format!(
            "{} is not an artifact source; expected {ARTIFACT_SCHEME}bucket[/prefix]",
            py_repr(repository)
        )));
    }
    let stripped = repository.trim();
    let remainder: String = stripped
        .chars()
        .skip(ARTIFACT_SCHEME.chars().count())
        .collect();
    let (bucket, prefix) = remainder
        .split_once('/')
        .unwrap_or((remainder.as_str(), ""));
    let bucket = bucket.trim().to_lowercase();
    if !BUCKET_PATTERN.is_match(&bucket) {
        return Err(source_error(format!(
            "{} is not a usable bucket name: a bucket starts with a letter and holds 2 to 63 lowercase letters, digits or hyphens",
            py_repr(&bucket)
        )));
    }
    Ok(ArtifactSource {
        bucket,
        prefix: normalise_prefix(prefix)?,
    })
}

fn normalise_prefix(prefix: &str) -> Result<String, EngineError> {
    let prefix = prefix.trim().trim_matches('/');
    if prefix.is_empty() {
        return Ok(String::new());
    }
    refuse_unsafe_key(prefix, "folder prefix")?;
    Ok(prefix.to_owned())
}

fn refuse_unsafe_key(key: &str, what: &str) -> Result<(), EngineError> {
    if key.len() > MAX_KEY_BYTES {
        return Err(source_error(format!(
            "{what} is longer than {MAX_KEY_BYTES} bytes"
        )));
    }
    if key.contains('\0') || key.contains('\\') {
        return Err(source_error(format!(
            "{what} {} holds a character a key may not hold",
            py_repr(key)
        )));
    }
    if key
        .split('/')
        .any(|segment| matches!(segment, "" | "." | ".."))
    {
        return Err(source_error(format!(
            "{what} {} holds an empty or relative segment",
            py_repr(key)
        )));
    }
    Ok(())
}

/// Python's `repr()` of a plain string, close enough for messages: single
/// quotes unless the text holds one and no double quote.
#[must_use]
pub fn py_repr(text: &str) -> String {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    let mut out = String::from(quote);
    for character in text.chars() {
        match character {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c == quote => {
                out.push('\\');
                out.push(c);
            }
            c => out.push(c),
        }
    }
    out.push(quote);
    out
}

/// The repository a wiki is NAMED after.
///
/// For a git source it is the repository itself. For an artifact folder it
/// is `{bucket}/{prefix}`: the scheme is dropped because this string becomes
/// the wiki id.
///
/// # Errors
///
/// A `ValueError` when the repository is an unusable artifact source.
pub fn display_repository_for(repo_config: Option<&Value>) -> Result<String, EngineError> {
    let mut repository = String::new();
    if let Some(Value::Object(config)) = repo_config {
        repository = str_or_empty(config, "repository");
        if repository.is_empty()
            && let Some(Value::Object(provider)) = config.get("provider_config")
        {
            repository = str_or_empty(provider, "repository");
        }
    }
    if is_artifact_source(&repository) {
        let source = parse_artifact_source(&repository)?;
        return Ok(if source.prefix.is_empty() {
            source.bucket
        } else {
            format!("{}/{}", source.bucket, source.prefix)
        });
    }
    Ok(repository.trim().trim_matches('/').to_owned())
}

/// The canonical `{owner}--{repo}--{branch}`.
///
/// # Errors
///
/// A `ValueError` when the repository is an unusable artifact source.
pub fn wiki_id_for(
    repo_config: Option<&Value>,
    branch: Option<&str>,
) -> Result<String, EngineError> {
    let mut repository = display_repository_for(repo_config)?;
    if repository.is_empty() {
        "fixture/repository".clone_into(&mut repository);
    }
    let branch = branch
        .map(str::trim)
        .filter(|b| !b.is_empty())
        .unwrap_or("main");
    Ok(format!("{}--{branch}", repository.replace('/', "--")))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn a_git_repository_names_itself() {
        let config = json!({"repository": " acme/notes/ "});
        assert_eq!(
            display_repository_for(Some(&config)).ok().as_deref(),
            Some("acme/notes")
        );
        assert_eq!(
            wiki_id_for(Some(&config), Some("dev")).ok().as_deref(),
            Some("acme--notes--dev")
        );
        assert_eq!(
            wiki_id_for(Some(&config), Some("  ")).ok().as_deref(),
            Some("acme--notes--main")
        );
    }

    #[test]
    fn the_provider_block_is_the_fallback() {
        let config = json!({"repository": "", "provider_config": {"repository": "o/r"}});
        assert_eq!(
            display_repository_for(Some(&config)).ok().as_deref(),
            Some("o/r")
        );
    }

    #[test]
    fn no_repository_is_the_fixture_name() {
        assert_eq!(
            wiki_id_for(None, None).ok().as_deref(),
            Some("fixture--repository--main")
        );
    }

    #[test]
    fn an_artifact_folder_drops_its_scheme() {
        let config = json!({"repository": "artifact://Docs/handbook/"});
        assert_eq!(
            display_repository_for(Some(&config)).ok().as_deref(),
            Some("docs/handbook")
        );
        let whole = json!({"repository": "ARTIFACT://docs"});
        assert_eq!(
            display_repository_for(Some(&whole)).ok().as_deref(),
            Some("docs")
        );
    }

    #[test]
    fn unsafe_artifact_sources_are_value_errors() {
        for bad in [
            "artifact://x",
            "artifact://docs/a/../b",
            "artifact://docs/a\\b",
            "artifact://1docs",
        ] {
            let config = json!({"repository": bad});
            let error = display_repository_for(Some(&config)).err();
            assert_eq!(error.map(|e| e.error_type), Some(ErrorType::Value), "{bad}");
        }
    }
}
