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

/// The one normalization of a chunker name (trim, ASCII lower-case) shared
/// by the router and every predicate over a config.
#[must_use]
pub fn normalize_chunker_name(name: &str) -> String {
    name.trim().to_ascii_lowercase()
}

/// One chunker's settings. A field left out takes the chunker's SDK default.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ChunkParams {
    /// `chunking_tool` / `chunker`: `markdown`, `text`, `json`, `code`,
    /// `universal`, or a model-calling one (`statistical`, `proposal`).
    pub chunker: Option<String>,
    /// Tokens per chunk (markdown, text) at any level. The code and JSON
    /// chunkers count characters, so they read it only from their own
    /// extension's block (the SDK's per-extension override), never from the
    /// top level. `chunk_size` is the code chunker's character setting. Zero or negative means "default" (the
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
    /// The JSON chunker's maximum chunk size (its own key, at any level).
    pub max_chunk_size: Option<i64>,
    /// The code chunker's token split for languages it has no grammar for.
    pub unknown_chunk_size: Option<i64>,
    pub unknown_chunk_overlap: Option<i64>,
    /// A loader step that calls a model (image and PDF-image descriptions).
    pub use_llm: Option<bool>,
    /// Image files: `true` sends the SDK default prompt, `false` the
    /// extension's `prompt` (see `plan_image`).
    pub use_default_prompt: Option<bool>,
    /// Image files: the prompt sent with the image.
    pub prompt: Option<String>,
    /// The settings of the statistical and proposal chunkers.
    pub model_steps: ModelStepParams,
}

/// The settings of the two chunkers that call a model, under the SDK's key
/// names (`statistical_chunker`, `proposal_chunker`). A field left out takes
/// the SDK's default (see `StatisticalSettings` and `PROPOSAL_WINDOW_TOKENS`).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct ModelStepParams {
    /// Proposal: tokens per model window (`max_doc_tokens`, SDK 1024).
    pub max_doc_tokens: Option<i64>,
    /// Statistical: tokens per sentence-like split (`max_doc_size`, SDK 300).
    pub max_doc_size: Option<i64>,
    /// Statistical: splits per embeddings request (`batch_size`, SDK 64).
    pub batch_size: Option<i64>,
    /// Statistical: splits the running context averages (`window_size`, 5).
    pub window_size: Option<i64>,
    pub dynamic_threshold: Option<bool>,
    pub min_split_tokens: Option<i64>,
    pub max_split_tokens: Option<i64>,
    pub split_tokens_tolerance: Option<i64>,
    pub threshold_adjustment: Option<f64>,
    pub score_threshold: Option<f64>,
}

// Every float is finite: `float` refuses NaN and the infinities.
impl Eq for ModelStepParams {}

/// `over`'s value when set, else `base`'s.
fn pick<T: Clone>(over: Option<&T>, base: Option<&T>) -> Option<T> {
    over.or(base).cloned()
}

/// Builds `Type` from `base` with `over`'s set fields taking precedence, one
/// field list for every settings struct (a new setting is named once here).
macro_rules! merge_fields {
    ($base:expr, $over:expr, $ty:ident { $($field:ident),* $(,)? } $(, $($extra:tt)*)?) => {
        $ty {
            $($field: pick($over.$field.as_ref(), $base.$field.as_ref()),)*
            $($($extra)*)?
        }
    };
}

impl ModelStepParams {
    fn merged(&self, over: &Self) -> Self {
        merge_fields!(
            self,
            over,
            ModelStepParams {
                max_doc_tokens,
                max_doc_size,
                batch_size,
                window_size,
                dynamic_threshold,
                min_split_tokens,
                max_split_tokens,
                split_tokens_tolerance,
                threshold_adjustment,
                score_threshold,
            }
        )
    }
}

impl ChunkParams {
    /// `self` with `over`'s set fields taking precedence.
    fn merged(&self, over: &Self) -> Self {
        merge_fields!(
            self,
            over,
            ChunkParams {
                chunker,
                max_tokens,
                chunk_size,
                token_overlap,
                chunk_overlap,
                min_chunk_chars,
                strip_header,
                return_each_line,
                headers_to_split_on,
                max_chunk_size,
                unknown_chunk_size,
                unknown_chunk_overlap,
                use_llm,
                use_default_prompt,
                prompt,
            },
            model_steps: self.model_steps.merged(&over.model_steps),
        )
    }

    /// Read one settings object. A value of the wrong type or out of range
    /// is dropped (that key takes the default) and recorded in `warnings`,
    /// prefixed with `scope` (`""` or `".md"`), so one bad key does not
    /// cost the whole config.
    #[allow(clippy::too_many_lines)] // One key per setting.
    fn from_object(
        object: &serde_json::Map<String, Value>,
        scope: &str,
        warnings: &mut Vec<String>,
    ) -> Self {
        let mut read = Reader {
            object,
            scope,
            warnings,
        };
        let int =
            |r: &mut Reader<'_>, key: &str| r.typed(key, "a number or a numeric string", number);
        let max_tokens = int(&mut read, "max_tokens");
        let chunk_size = int(&mut read, "chunk_size");
        let token_overlap = int(&mut read, "token_overlap");
        let chunk_overlap = int(&mut read, "chunk_overlap");
        let max_chunk_size = int(&mut read, "max_chunk_size");
        let min_chunk_chars =
            int(&mut read, "min_chunk_chars").map(|n| usize::try_from(n).unwrap_or(0));
        let unknown_chunk_size = int(&mut read, "unknown_chunk_size");
        // `token_chunk_size` is the SDK model's name for it
        // (`CodeChunkerConfig`).
        let unknown_chunk_size = unknown_chunk_size.or_else(|| int(&mut read, "token_chunk_size"));
        let unknown_chunk_overlap = int(&mut read, "unknown_chunk_overlap");
        let max_doc_tokens = int(&mut read, "max_doc_tokens");
        let max_doc_size = int(&mut read, "max_doc_size");
        let batch_size = int(&mut read, "batch_size");
        let window_size = int(&mut read, "window_size");
        let min_split_tokens = int(&mut read, "min_split_tokens");
        let max_split_tokens = int(&mut read, "max_split_tokens");
        let split_tokens_tolerance = int(&mut read, "split_tokens_tolerance");
        let threshold_adjustment = read.typed(
            "threshold_adjustment",
            "a finite number or a numeric string",
            float,
        );
        let score_threshold = read.typed(
            "score_threshold",
            "a finite number or a numeric string",
            float,
        );
        let mut flag = |key: &str| {
            read.typed(key, "a boolean", |v| match v {
                Value::Bool(b) => Some(*b),
                _ => None,
            })
        };
        let strip_header = flag("strip_header");
        let return_each_line = flag("return_each_line");
        let use_llm = flag("use_llm");
        let use_default_prompt = flag("use_default_prompt");
        let dynamic_threshold = flag("dynamic_threshold");
        let mut text = |key: &str| {
            read.typed(key, "a string", |v| match v {
                Value::String(s) => Some(s.clone()),
                _ => None,
            })
            .filter(|s| !s.is_empty())
        };
        let chunker = text("chunking_tool").or_else(|| text("chunker"));
        let prompt = text("prompt");
        let headers_to_split_on = match headers(object.get("headers_to_split_on")) {
            Ok(headers) => headers,
            Err(value) => {
                read.warn(
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
            max_chunk_size,
            unknown_chunk_size,
            unknown_chunk_overlap,
            use_llm,
            use_default_prompt,
            prompt,
            model_steps: ModelStepParams {
                max_doc_tokens,
                max_doc_size,
                batch_size,
                window_size,
                dynamic_threshold,
                min_split_tokens,
                max_split_tokens,
                split_tokens_tolerance,
                threshold_adjustment,
                score_threshold,
            },
        }
    }
}

/// One settings object being read: a typed getter per value kind, each
/// dropping a wrong-typed value to the default with one warning.
struct Reader<'a> {
    object: &'a serde_json::Map<String, Value>,
    scope: &'a str,
    warnings: &'a mut Vec<String>,
}

impl Reader<'_> {
    fn warn(&mut self, key: &str, expected: &str, value: &Value) {
        let at = if self.scope.is_empty() {
            key.to_owned()
        } else {
            format!("{}.{key}", self.scope)
        };
        self.warnings.push(format!(
            "chunking_config `{at}` must be {expected}, got {value}; using the default"
        ));
    }

    /// `key` parsed by `parse`; absent or `null` is `None` without a
    /// warning, a value `parse` refuses is `None` with one.
    fn typed<T>(
        &mut self,
        key: &str,
        expected: &str,
        parse: impl Fn(&Value) -> Option<T>,
    ) -> Option<T> {
        match self.object.get(key) {
            None | Some(Value::Null) => None,
            Some(value) => {
                let parsed = parse(value);
                if parsed.is_none() {
                    self.warn(key, expected, value);
                }
                parsed
            }
        }
    }
}

/// A finite number from a JSON number or a numeric string.
fn float(value: &Value) -> Option<f64> {
    match value {
        Value::Number(n) => n.as_f64(),
        Value::String(s) => s.trim().parse::<f64>().ok(),
        _ => None,
    }
    .filter(|f| f.is_finite())
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
    /// Whether any selected chunker is `name` (normalized as the router
    /// does), at the top level or for any extension.
    fn selects(&self, name: &str) -> bool {
        std::iter::once(&self.base)
            .chain(self.by_extension.values())
            .any(|p| p.chunker.as_deref().map(normalize_chunker_name).as_deref() == Some(name))
    }

    /// Whether any step this config can select calls a model at all: the
    /// `proposal` and `statistical` chunkers, `use_llm` at the top level or
    /// for any extension, or the image-file vision step (on by default, so
    /// true unless the config switches it off everywhere).
    #[must_use]
    pub fn needs_model_step(&self) -> bool {
        self.needs_chat_model()
            || self.selects("statistical")
            || std::iter::once(&self.base)
                .chain(self.by_extension.values())
                .any(|p| p.use_llm == Some(true))
    }

    /// Whether the run uses the CHAT model (`index.llm_model`): the
    /// `proposal` chunker, or the image-file vision step (ADR-0030 I4 part
    /// B), which is ON by default for `.png .jpg .jpeg .gif .webp .bmp .svg`
    /// and so applies unless `use_llm: false` switches it off for all of
    /// them. The statistical chunker uses embeddings only. `use_llm` on
    /// another extension (images inside PDF/Office files) is not served and
    /// does not need the model.
    #[must_use]
    pub fn needs_chat_model(&self) -> bool {
        self.needs_proposal_model() || crate::vision::vision_enabled_anywhere(self)
    }

    /// The non-image extensions whose block sets `use_llm: true`: the
    /// embedded-image step they ask for is not ported, so it has no effect.
    #[must_use]
    pub fn unserved_use_llm_extensions(&self) -> Vec<String> {
        self.by_extension
            .iter()
            .filter(|(extension, params)| {
                params.use_llm == Some(true) && !crate::vision::is_image_extension(extension)
            })
            .map(|(extension, _)| extension.clone())
            .collect()
    }

    /// Whether the `proposal` chunker is selected: the one case where a
    /// chat model that cannot be read must fail the run, not just skip
    /// image documents.
    #[must_use]
    pub fn needs_proposal_model(&self) -> bool {
        self.selects("proposal")
    }

    /// Whether the config EXPLICITLY selects a chunker that calls a model
    /// (`proposal` or `statistical`), as opposed to the image-file vision
    /// step that is on by default. A run that selects one and has no model
    /// loses every document that chunker would have handled.
    #[must_use]
    pub fn selects_model_chunker(&self) -> bool {
        self.selects("proposal") || self.selects("statistical")
    }

    /// Whether the config EXPLICITLY opts into the picture description
    /// (`use_llm: true` at the top level or on an image extension), as
    /// opposed to the default-on step. A run that asks for it and has no
    /// model loses the descriptions it asked for.
    #[must_use]
    pub fn opts_into_vision(&self) -> bool {
        self.base.use_llm == Some(true)
            || self.by_extension.iter().any(|(extension, params)| {
                params.use_llm == Some(true) && crate::vision::is_image_extension(extension)
            })
    }

    /// Whether the config explicitly asks for any step that needs a model:
    /// a model chunker or the picture description.
    #[must_use]
    pub fn explicitly_needs_model(&self) -> bool {
        self.selects_model_chunker() || self.opts_into_vision()
    }

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

    /// The settings object of `extension`'s own block, if the config has one
    /// (`".md"` or `"md"`).
    #[must_use]
    pub fn own_params_for(&self, extension: &str) -> Option<&ChunkParams> {
        let key = if extension.starts_with('.') {
            extension.to_ascii_lowercase()
        } else {
            format!(".{}", extension.to_ascii_lowercase())
        };
        self.by_extension.get(&key)
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
    #[test]
    fn a_config_needs_a_model_step_when_a_selected_step_calls_one() {
        let from = |v: Value| ChunkingConfig::from_value(&v).unwrap();
        // The image-file vision step is on by default (SDK parity).
        assert!(from(Value::Null).needs_model_step());
        assert!(from(serde_json::json!({"chunker": "markdown"})).needs_model_step());
        assert!(!from(serde_json::json!({"use_llm": false})).needs_model_step());
        assert!(from(serde_json::json!({"chunker": "proposal"})).needs_model_step());
        assert!(
            from(serde_json::json!({"use_llm": false, ".md": {"chunker": "statistical"}}))
                .needs_model_step()
        );
        assert!(
            from(serde_json::json!({"use_llm": false, ".pdf": {"use_llm": true}}))
                .needs_model_step()
        );
        for spelled in ["Proposal", " proposal", "PROPOSAL "] {
            let config = from(serde_json::json!({ "chunker": spelled }));
            assert!(config.needs_model_step(), "{spelled:?}");
            assert!(config.needs_chat_model(), "{spelled:?}");
            assert!(config.needs_proposal_model(), "{spelled:?}");
        }
    }

    #[test]
    fn an_explicit_vision_opt_in_is_not_the_default() {
        let from = |v: Value| ChunkingConfig::from_value(&v).unwrap();
        assert!(!from(Value::Null).opts_into_vision());
        assert!(!from(serde_json::json!({"use_llm": false})).opts_into_vision());
        assert!(from(serde_json::json!({"use_llm": true})).opts_into_vision());
        assert!(from(serde_json::json!({".png": {"use_llm": true}})).opts_into_vision());
        // A document extension's use_llm is not served: not an opt-in.
        assert!(!from(serde_json::json!({".pdf": {"use_llm": true}})).opts_into_vision());
        assert!(from(serde_json::json!({".md": {"chunker": "proposal"}})).explicitly_needs_model());
        assert!(!from(Value::Null).explicitly_needs_model());
    }

    #[test]
    fn use_llm_on_a_document_extension_is_reported_as_unserved() {
        let from = |v: Value| ChunkingConfig::from_value(&v).unwrap();
        let config = from(serde_json::json!({
            "use_llm": true, ".pdf": {"use_llm": true}, ".png": {"use_llm": true},
            ".docx": {"use_llm": false}
        }));
        assert_eq!(config.unserved_use_llm_extensions(), vec![".pdf"]);
        assert!(from(Value::Null).unserved_use_llm_extensions().is_empty());
    }

    #[test]
    fn the_chat_model_is_needed_by_the_proposal_chunker_and_the_vision_step() {
        let from = |v: Value| ChunkingConfig::from_value(&v).unwrap();
        let off = serde_json::json!({"use_llm": false});
        assert!(!from(off.clone()).needs_chat_model());
        assert!(
            !from(serde_json::json!({"use_llm": false, "chunker": "statistical"}))
                .needs_chat_model()
        );
        // use_llm on a non-image extension is the embedded-image step, not served.
        assert!(
            !from(serde_json::json!({"use_llm": false, ".pdf": {"use_llm": true}}))
                .needs_chat_model()
        );
        // Default on; one image extension left on is enough.
        assert!(from(Value::Null).needs_chat_model());
        assert!(!from(Value::Null).needs_proposal_model());
        let mut one = serde_json::json!({"use_llm": false, ".png": {"use_llm": true}});
        assert!(from(one.clone()).needs_chat_model());
        one[".png"]["use_llm"] = Value::Bool(false);
        assert!(!from(one).needs_chat_model());
        assert!(
            from(serde_json::json!({"use_llm": false, ".md": {"chunking_tool": "Proposal"}}))
                .needs_chat_model()
        );
    }

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
