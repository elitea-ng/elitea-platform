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

    /// Read one settings object. A value of the wrong type or out of range
    /// is dropped (that key takes the default) and recorded in `warnings`,
    /// prefixed with `scope` (`""` or `".md"`), so one bad key does not
    /// cost the whole config.
    fn from_object(
        object: &serde_json::Map<String, Value>,
        scope: &str,
        warnings: &mut Vec<String>,
    ) -> Self {
        let mut warn = |key: &str, expected: &str, value: &Value| {
            let at = if scope.is_empty() {
                key.to_owned()
            } else {
                format!("{scope}.{key}")
            };
            warnings.push(format!(
                "chunking_config `{at}` must be {expected}, got {value}; using the default"
            ));
        };
        let mut int = |key: &str| -> Option<i64> {
            match object.get(key) {
                None | Some(Value::Null) => None,
                Some(value) => {
                    let parsed = number(value);
                    if parsed.is_none() {
                        warn(key, "a number or a numeric string", value);
                    }
                    parsed
                }
            }
        };
        let max_tokens = int("max_tokens");
        let chunk_size = int("chunk_size");
        let token_overlap = int("token_overlap");
        let chunk_overlap = int("chunk_overlap");
        let min_chunk_chars = int("min_chunk_chars").map(|n| usize::try_from(n).unwrap_or(0));
        let unknown_chunk_size = int("unknown_chunk_size");
        // `token_chunk_size` is the SDK model's name for it
        // (`CodeChunkerConfig`).
        let unknown_chunk_size = unknown_chunk_size.or_else(|| int("token_chunk_size"));
        let unknown_chunk_overlap = int("unknown_chunk_overlap");
        let mut flag = |key: &str| -> Option<bool> {
            match object.get(key) {
                None | Some(Value::Null) => None,
                Some(Value::Bool(b)) => Some(*b),
                Some(value) => {
                    warn(key, "a boolean", value);
                    None
                }
            }
        };
        let strip_header = flag("strip_header");
        let return_each_line = flag("return_each_line");
        let use_llm = flag("use_llm");
        let mut text = |key: &str| -> Option<String> {
            match object.get(key) {
                None | Some(Value::Null) => None,
                Some(Value::String(s)) if s.is_empty() => None,
                Some(Value::String(s)) => Some(s.clone()),
                Some(value) => {
                    warn(key, "a string", value);
                    None
                }
            }
        };
        let chunker = text("chunking_tool").or_else(|| text("chunker"));
        let headers_to_split_on = match headers(object.get("headers_to_split_on")) {
            Ok(headers) => headers,
            Err(value) => {
                warn(
                    "headers_to_split_on",
                    "a list of [marker, name] pairs",
                    &value,
                );
                None
            }
        };
        Self {
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
            use_llm,
        }
    }
}

/// An integer from a JSON number (`512.0` is 512) or a numeric string
/// (`"512"`, `" 512.0 "`). `None` for anything else, including a NaN or an
/// infinite value.
#[allow(clippy::cast_possible_truncation)]
fn number(value: &Value) -> Option<i64> {
    let from_float = |f: f64| f.is_finite().then_some(f as i64);
    match value {
        Value::Number(n) => n.as_i64().or_else(|| n.as_f64().and_then(from_float)),
        Value::String(s) => {
            let s = s.trim();
            s.parse::<i64>()
                .ok()
                .or_else(|| s.parse::<f64>().ok().and_then(from_float))
        }
        _ => None,
    }
}

/// `[["#", "Header 1"], …]`, or `"# Header 1"` strings (the form the
/// by-headers chunker reads). The offending value on failure.
fn headers(value: Option<&Value>) -> Result<Option<Vec<(String, String)>>, Value> {
    let Some(value) = value else {
        return Ok(None);
    };
    let items = match value {
        Value::Null => return Ok(None),
        Value::Array(items) => items,
        other => return Err(other.clone()),
    };
    let mut out = Vec::new();
    for item in items {
        match item {
            Value::Array(pair) => match pair.as_slice() {
                [Value::String(marker), Value::String(name)] => {
                    out.push((marker.clone(), name.clone()));
                }
                _ => return Err(value.clone()),
            },
            Value::String(joined) => {
                let (marker, name) = joined.split_once(' ').ok_or_else(|| value.clone())?;
                out.push((marker.to_owned(), name.to_owned()));
            }
            _ => return Err(value.clone()),
        }
    }
    Ok(Some(out))
}

/// A `chunking_config` that is not usable at all (not an object).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConfigError(String);

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
    /// Settings that were dropped for the default, one line each (a wrong
    /// type, a non-numeric string). Callers log these.
    pub warnings: Vec<String>,
}

impl ChunkingConfig {
    /// Read the SDK's JSON form. `null` is the empty config.
    ///
    /// A setting of the wrong type is dropped for that key's default and
    /// recorded in [`ChunkingConfig::warnings`]; numbers may be given as
    /// numeric strings.
    ///
    /// # Errors
    /// Only a config that is not an object.
    pub fn from_value(value: &Value) -> Result<Self, ConfigError> {
        let object = match value {
            Value::Null => return Ok(Self::default()),
            Value::Object(object) => object,
            _ => return Err(ConfigError("chunking_config must be an object".to_owned())),
        };
        let mut warnings = Vec::new();
        let mut config = Self {
            base: ChunkParams::from_object(object, "", &mut warnings),
            by_extension: BTreeMap::new(),
            warnings: Vec::new(),
        };
        for (key, nested) in object {
            // A key that looks like an extension and holds an object.
            if let (true, Value::Object(inner)) = (key.starts_with('.'), nested) {
                let key = key.to_ascii_lowercase();
                let params = ChunkParams::from_object(inner, &key, &mut warnings);
                config.by_extension.insert(key, params);
            }
        }
        config.warnings = warnings;
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
    fn only_an_unusable_document_is_an_error_and_null_is_empty() {
        assert!(ChunkingConfig::from_value(&json!([1])).is_err());
        assert!(ChunkingConfig::from_value(&json!("text")).is_err());
        assert_eq!(
            ChunkingConfig::from_value(&Value::Null),
            Ok(ChunkingConfig::default())
        );
    }

    #[test]
    fn numbers_may_be_numeric_strings() {
        let config = ChunkingConfig::from_value(&json!({
            "max_tokens": "300", "token_overlap": 20.0, "chunk_size": " 64 ",
            ".md": { "max_tokens": "128.0" }
        }))
        .unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(config.base.max_tokens, Some(300));
        assert_eq!(config.base.token_overlap, Some(20));
        assert_eq!(config.base.chunk_size, Some(64));
        assert_eq!(config.params_for(".md").max_tokens, Some(128));
        assert!(config.warnings.is_empty(), "{:?}", config.warnings);
    }

    #[test]
    fn a_bad_value_costs_only_its_own_key() {
        let config = ChunkingConfig::from_value(&json!({
            "max_tokens": "big",
            "token_overlap": 15,
            "strip_header": "yes",
            "headers_to_split_on": 7,
            "chunking_tool": ["markdown"],
            ".md": { "max_tokens": [1], "min_chunk_chars": 50 }
        }))
        .unwrap_or_else(|e| panic!("{e}"));
        // The bad keys are defaults; the good ones survive.
        assert_eq!(config.base.max_tokens, None);
        assert_eq!(config.base.strip_header, None);
        assert_eq!(config.base.headers_to_split_on, None);
        assert_eq!(config.base.chunker, None);
        assert_eq!(config.base.token_overlap, Some(15));
        let md = config.params_for(".md");
        assert_eq!(md.max_tokens, None, "the bad extension key is the default");
        assert_eq!(md.min_chunk_chars, Some(50));
        assert_eq!(md.token_overlap, Some(15));
        assert_eq!(config.warnings.len(), 5, "{:?}", config.warnings);
        assert!(
            config
                .warnings
                .iter()
                .any(|w| w.contains("`.md.max_tokens`")),
            "{:?}",
            config.warnings
        );
        assert!(
            config
                .warnings
                .iter()
                .any(|w| w.contains("`max_tokens`") && w.contains("\"big\"")),
            "{:?}",
            config.warnings
        );
    }
}
