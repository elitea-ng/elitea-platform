//! The `index_data` `chunking_config` form.
//!
//! The SDK reads one dict two ways: top-level keys are the chunking tool's
//! own settings (`max_tokens`, `token_overlap`, `min_chunk_chars`,
//! `headers_to_split_on`, …), and a key that is a file extension (`".md"`)
//! holds that extension's settings, which win for files of that extension
//! (`content_parser.process_document_by_type`). Keys this crate does not use
//! (the SDK injects `embedding` and `llm`) are ignored.

use serde::{Deserialize, Deserializer};
use serde_json::Value;
use std::collections::BTreeMap;

/// One chunker's settings. A field left out takes the chunker's SDK default.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ChunkParams {
    /// `chunking_tool` / `chunker`: `markdown`, `text`, `json`, `code`,
    /// `universal`, or a model-calling one (`statistical`, `proposal`).
    pub chunker: Option<String>,
    /// Tokens per chunk (markdown, text); characters for the code chunker's
    /// known languages, where the SDK reads the same key. `chunk_size` is the
    /// code chunker's other spelling. Zero or negative means "default" (the
    /// CSV and Excel loaders use -1 for "no limit").
    pub max_tokens: Option<i64>,
    pub chunk_size: Option<i64>,
    pub token_overlap: Option<i64>,
    pub chunk_overlap: Option<i64>,
    /// Markdown: merge chunks under this many characters into the next.
    pub min_chunk_chars: Option<usize>,
    pub strip_header: Option<bool>,
    pub return_each_line: Option<bool>,
    /// Markdown header markers and their metadata names, e.g.
    /// `("##", "Header 2")`. Empty means the default H1-H4.
    pub headers_to_split_on: Option<Vec<(String, String)>>,
    /// The code chunker's token split for languages it has no grammar for.
    pub unknown_chunk_size: Option<i64>,
    pub unknown_chunk_overlap: Option<i64>,
    /// A loader step that calls a model (image and PDF-image descriptions).
    pub use_llm: Option<bool>,
}

impl ChunkParams {
    /// `self` with `over`'s set fields taking precedence.
    fn merged(&self, over: &Self) -> Self {
        macro_rules! pick {
            ($($field:ident),*) => {
                Self { $($field: over.$field.clone().or_else(|| self.$field.clone())),* }
            };
        }
        pick!(
            chunker,
            max_tokens,
            chunk_size,
            token_overlap,
            chunk_overlap,
            min_chunk_chars,
            strip_header,
            return_each_line,
            headers_to_split_on,
            unknown_chunk_size,
            unknown_chunk_overlap,
            use_llm
        )
    }

    // JSON numbers like `512.0` are accepted as integers.
    #[allow(clippy::cast_possible_truncation)]
    fn from_object(object: &serde_json::Map<String, Value>) -> Result<Self, ConfigError> {
        let int = |key: &str| -> Result<Option<i64>, ConfigError> {
            match object.get(key) {
                None | Some(Value::Null) => Ok(None),
                Some(Value::Number(n)) => n
                    .as_i64()
                    .or_else(|| n.as_f64().map(|f| f as i64))
                    .map(Some)
                    .ok_or_else(|| ConfigError::wrong(key, "an integer")),
                Some(_) => Err(ConfigError::wrong(key, "an integer")),
            }
        };
        let flag = |key: &str| -> Result<Option<bool>, ConfigError> {
            match object.get(key) {
                None | Some(Value::Null) => Ok(None),
                Some(Value::Bool(b)) => Ok(Some(*b)),
                Some(_) => Err(ConfigError::wrong(key, "a boolean")),
            }
        };
        let text = |key: &str| -> Result<Option<String>, ConfigError> {
            match object.get(key) {
                None | Some(Value::Null) => Ok(None),
                Some(Value::String(s)) if s.is_empty() => Ok(None),
                Some(Value::String(s)) => Ok(Some(s.clone())),
                Some(_) => Err(ConfigError::wrong(key, "a string")),
            }
        };
        let chunker = match text("chunking_tool")? {
            Some(tool) => Some(tool),
            None => text("chunker")?,
        };
        Ok(Self {
            chunker,
            max_tokens: int("max_tokens")?,
            chunk_size: int("chunk_size")?,
            token_overlap: int("token_overlap")?,
            chunk_overlap: int("chunk_overlap")?,
            min_chunk_chars: int("min_chunk_chars")?.map(|n| usize::try_from(n).unwrap_or(0)),
            strip_header: flag("strip_header")?,
            return_each_line: flag("return_each_line")?,
            headers_to_split_on: headers(object.get("headers_to_split_on"))?,
            // `token_chunk_size` / `token_overlap` are the SDK model's
            // names for these (`CodeChunkerConfig`).
            unknown_chunk_size: match int("unknown_chunk_size")? {
                Some(n) => Some(n),
                None => int("token_chunk_size")?,
            },
            unknown_chunk_overlap: int("unknown_chunk_overlap")?,
            use_llm: flag("use_llm")?,
        })
    }
}

/// `[["#", "Header 1"], …]`, or `"# Header 1"` strings (the form the
/// by-headers chunker reads).
fn headers(value: Option<&Value>) -> Result<Option<Vec<(String, String)>>, ConfigError> {
    let bad = || ConfigError::wrong("headers_to_split_on", "a list of [marker, name] pairs");
    let Some(Value::Array(items)) = value else {
        return match value {
            None | Some(Value::Null) => Ok(None),
            Some(_) => Err(bad()),
        };
    };
    let mut out = Vec::new();
    for item in items {
        match item {
            Value::Array(pair) => match pair.as_slice() {
                [Value::String(marker), Value::String(name)] => {
                    out.push((marker.clone(), name.clone()));
                }
                _ => return Err(bad()),
            },
            Value::String(joined) => {
                let (marker, name) = joined.split_once(' ').ok_or_else(bad)?;
                out.push((marker.to_owned(), name.to_owned()));
            }
            _ => return Err(bad()),
        }
    }
    Ok(Some(out))
}

/// A `chunking_config` value of the wrong shape.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConfigError(String);

impl ConfigError {
    fn wrong(key: &str, expected: &str) -> Self {
        Self(format!("chunking_config `{key}` must be {expected}"))
    }
}

impl std::fmt::Display for ConfigError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ConfigError {}

/// The whole `chunking_config`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ChunkingConfig {
    /// The top-level keys.
    pub base: ChunkParams,
    /// Per extension, keyed lower-case with a leading dot (`".md"`).
    pub by_extension: BTreeMap<String, ChunkParams>,
}

impl ChunkingConfig {
    /// Read the SDK's JSON form. `null` is the empty config.
    ///
    /// # Errors
    /// A recognised key of the wrong type, or a config that is not an object.
    pub fn from_value(value: &Value) -> Result<Self, ConfigError> {
        let object = match value {
            Value::Null => return Ok(Self::default()),
            Value::Object(object) => object,
            _ => return Err(ConfigError("chunking_config must be an object".to_owned())),
        };
        let mut config = Self {
            base: ChunkParams::from_object(object)?,
            by_extension: BTreeMap::new(),
        };
        for (key, nested) in object {
            // A key that looks like an extension and holds an object.
            if let (true, Value::Object(inner)) = (key.starts_with('.'), nested) {
                config
                    .by_extension
                    .insert(key.to_ascii_lowercase(), ChunkParams::from_object(inner)?);
            }
        }
        Ok(config)
    }

    /// The settings for files of `extension` (`".md"` or `"md"`): that
    /// extension's keys over the top-level ones.
    #[must_use]
    pub fn params_for(&self, extension: &str) -> ChunkParams {
        let key = if extension.starts_with('.') {
            extension.to_ascii_lowercase()
        } else {
            format!(".{}", extension.to_ascii_lowercase())
        };
        match self.by_extension.get(&key) {
            Some(own) => self.base.merged(own),
            None => self.base.clone(),
        }
    }
}

impl<'de> Deserialize<'de> for ChunkingConfig {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let value = Value::deserialize(deserializer)?;
        Self::from_value(&value).map_err(serde::de::Error::custom)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_sdk_shape_reads_top_level_and_per_extension_keys() {
        let config: ChunkingConfig = serde_json::from_value(json!({
            "max_tokens": 300,
            "token_overlap": 20,
            "embedding": "ignored",
            ".md": { "max_tokens": 128, "headers_to_split_on": [["#", "H1"], ["##", "H2"]] },
            ".PDF": { "use_llm": true }
        }))
        .unwrap_or_else(|e| panic!("{e}"));
        let md = config.params_for("md");
        assert_eq!(md.max_tokens, Some(128), "the extension wins");
        assert_eq!(md.token_overlap, Some(20), "the top level fills in");
        assert_eq!(
            md.headers_to_split_on,
            Some(vec![("#".into(), "H1".into()), ("##".into(), "H2".into())])
        );
        assert_eq!(config.params_for(".txt").max_tokens, Some(300));
        assert_eq!(config.params_for(".pdf").use_llm, Some(true));
    }

    #[test]
    fn header_strings_and_aliases_are_read() {
        let config = ChunkingConfig::from_value(&json!({
            "chunking_tool": "markdown",
            "headers_to_split_on": ["# Header 1", "## Header 2"],
            "token_chunk_size": 99
        }))
        .unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(config.base.chunker.as_deref(), Some("markdown"));
        assert_eq!(config.base.unknown_chunk_size, Some(99));
        assert_eq!(
            config.base.headers_to_split_on.as_deref().map(<[_]>::len),
            Some(2)
        );
    }

    #[test]
    fn a_wrong_type_is_an_error_and_null_is_empty() {
        assert!(ChunkingConfig::from_value(&json!({"max_tokens": "big"})).is_err());
        assert!(ChunkingConfig::from_value(&json!([1])).is_err());
        assert_eq!(
            ChunkingConfig::from_value(&Value::Null),
            Ok(ChunkingConfig::default())
        );
    }
}
