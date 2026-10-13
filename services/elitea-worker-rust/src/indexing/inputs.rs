//! The index bundle's entries, read strictly.
//!
//! * The toolkit configuration (`index.toolkit_configuration`): what Main's
//!   authoritative input resolver wrote, `{id, type, toolkit_name, settings}`
//!   with the settings expanded (credentials resolved).
//! * The tool parameters (`index.tool_parameters`): the `index_data`
//!   arguments as the request carried them.
//! * The embedding binding (`index.embedding_binding`): Main's
//!   `EmbeddingBinding.MarshalCanonical`, of which only `model_name` is read.
//! * The chat model (`index.llm_model`, a JSON string) and its settings
//!   (`index.llm_configuration`, an object of which `temperature` and
//!   `max_tokens` are read), for the steps that call a chat model (ADR-0030
//!   I4). Both are optional; an entry that is present and malformed is
//!   refused, a setting that is unusable is left out.
//!
//! Every read refuses an entry that is not an object; a wrong type on an
//! optional `index_data` argument is a run failure with a stated reason, not
//! a silent default, because the SDK's form validated the same arguments.

use serde_json::{Map, Value};

/// The toolkit configuration entry.
pub(crate) struct ToolkitConfiguration {
    /// The toolkit's `type` (`github`, `artifact`, …).
    pub(crate) toolkit_type: String,
    pub(crate) toolkit_name: String,
    /// The expanded settings, as the connectors' `parse` read them.
    pub(crate) settings: Map<String, Value>,
}

/// The `index_data` arguments the run honours.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct IndexParameters {
    pub(crate) index_name: String,
    pub(crate) clean_index: bool,
    /// `chunking_tool`, when given: the chunker's name (`universal`,
    /// `markdown`, …). It becomes the config's base `chunker`.
    pub(crate) chunking_tool: Option<String>,
    /// `chunking_config` as given (an object), or `Null`.
    pub(crate) chunking_config: Value,
    /// The code families' `branch`.
    pub(crate) branch: Option<String>,
    pub(crate) whitelist: Vec<String>,
    pub(crate) blacklist: Vec<String>,
    /// Every `index_data` argument as given, for a family whose connector
    /// reads its own (Confluence's `cql`, Jira's `jql`, …): each connector's
    /// argument parser reads and validates what it honours.
    pub(crate) arguments: Map<String, Value>,
}

/// Why an entry was refused. The text names a key, never a value.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct InputError(pub(crate) String);

impl std::fmt::Display for InputError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl std::error::Error for InputError {}

fn object(raw: &[u8], what: &str) -> Result<Map<String, Value>, InputError> {
    match serde_json::from_slice::<Value>(raw) {
        Ok(Value::Object(map)) => Ok(map),
        _ => Err(InputError(format!("the {what} entry is not a JSON object"))),
    }
}

fn text(map: &Map<String, Value>, key: &str, what: &str) -> Result<String, InputError> {
    match map.get(key) {
        Some(Value::String(value)) if !value.trim().is_empty() => Ok(value.clone()),
        _ => Err(InputError(format!("the {what} entry has no `{key}`"))),
    }
}

/// Reads the toolkit configuration entry.
///
/// # Errors
///
/// Not an object, or without `type`, `toolkit_name` or an object `settings`.
pub(crate) fn toolkit_configuration(raw: &[u8]) -> Result<ToolkitConfiguration, InputError> {
    let map = object(raw, "toolkit configuration")?;
    let settings = match map.get("settings") {
        Some(Value::Object(settings)) => settings.clone(),
        _ => {
            return Err(InputError(
                "the toolkit configuration entry has no object `settings`".to_owned(),
            ));
        }
    };
    Ok(ToolkitConfiguration {
        toolkit_type: text(&map, "type", "toolkit configuration")?,
        toolkit_name: text(&map, "toolkit_name", "toolkit configuration")?,
        settings,
    })
}

/// Reads the `index_data` arguments.
///
/// # Errors
///
/// Not an object, no `index_name`, or an optional argument of the wrong
/// type.
pub(crate) fn index_parameters(raw: &[u8]) -> Result<IndexParameters, InputError> {
    let map = object(raw, "tool parameters")?;
    let index_name = text(&map, "index_name", "tool parameters")?;
    let clean_index = match map.get("clean_index") {
        None | Some(Value::Null) => false,
        Some(Value::Bool(flag)) => *flag,
        Some(_) => return Err(InputError("`clean_index` must be a boolean".to_owned())),
    };
    let chunking_tool = optional_text(&map, "chunking_tool")?;
    let chunking_config = match map.get("chunking_config") {
        None | Some(Value::Null) => Value::Null,
        Some(value @ Value::Object(_)) => value.clone(),
        Some(_) => {
            return Err(InputError("`chunking_config` must be an object".to_owned()));
        }
    };
    Ok(IndexParameters {
        index_name,
        clean_index,
        chunking_tool,
        chunking_config,
        branch: optional_text(&map, "branch")?,
        whitelist: text_list(&map, "whitelist")?,
        blacklist: text_list(&map, "blacklist")?,
        arguments: map,
    })
}

fn optional_text(map: &Map<String, Value>, key: &str) -> Result<Option<String>, InputError> {
    match map.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            let trimmed = value.trim();
            Ok((!trimmed.is_empty()).then(|| trimmed.to_owned()))
        }
        Some(_) => Err(InputError(format!("`{key}` must be a string"))),
    }
}

fn text_list(map: &Map<String, Value>, key: &str) -> Result<Vec<String>, InputError> {
    match map.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| match item {
                Value::String(pattern) => Ok(pattern.clone()),
                _ => Err(InputError(format!("`{key}` must hold strings"))),
            })
            .collect(),
        Some(_) => Err(InputError(format!("`{key}` must be a list of strings"))),
    }
}

/// The settings of the chat model an index step calls, from
/// `index.llm_configuration`. A key that is missing or unusable is `None`:
/// the SDK's `llm_settings` carry many keys the gateway call does not take.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub(crate) struct LlmSettings {
    pub(crate) max_tokens: Option<u32>,
    pub(crate) temperature: Option<f32>,
}

/// The chat model name of the `index.llm_model` entry.
///
/// # Errors
///
/// Not a JSON string, or an empty one.
pub(crate) fn llm_model(raw: &[u8]) -> Result<String, InputError> {
    match serde_json::from_slice::<Value>(raw) {
        Ok(Value::String(name)) if !name.trim().is_empty() => Ok(name.trim().to_owned()),
        _ => Err(InputError(
            "the llm model entry is not a model name".to_owned(),
        )),
    }
}

/// The settings of the `index.llm_configuration` entry. A usable
/// `temperature` is 0.0 to 2.0, a usable `max_tokens` a positive integer (an
/// integral float such as `4096.0` is one); anything else is warned about and
/// left to the model's default.
///
/// # Errors
///
/// Not a JSON object.
#[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
pub(crate) fn llm_settings(raw: &[u8]) -> Result<LlmSettings, InputError> {
    let map = object(raw, "llm configuration")?;
    let max_tokens = match map.get("max_tokens") {
        None | Some(Value::Null) => None,
        Some(value) => {
            let parsed = value
                .as_u64()
                .or_else(|| {
                    value
                        .as_f64()
                        .filter(|f| {
                            f.is_finite() && f.fract() == 0.0 && *f >= 1.0 && *f <= 2_147_483_647.0
                        })
                        .map(|f| f as u64)
                })
                .and_then(|n| u32::try_from(n).ok())
                .filter(|n| *n > 0 && i32::try_from(*n).is_ok());
            if parsed.is_none() {
                tracing::warn!(
                    event = "index_llm_setting_dropped",
                    key = "max_tokens",
                    "the llm configuration's `max_tokens` is not a positive integer; the model's default applies"
                );
            }
            parsed
        }
    };
    let temperature = match map.get("temperature") {
        None | Some(Value::Null) => None,
        Some(value) => {
            let parsed = value
                .as_f64()
                .filter(|t| t.is_finite() && (0.0..=2.0).contains(t))
                .map(|t| t as f32);
            if parsed.is_none() {
                tracing::warn!(
                    event = "index_llm_setting_dropped",
                    key = "temperature",
                    "the llm configuration's `temperature` is not between 0.0 and 2.0; the model's default applies"
                );
            }
            parsed
        }
    };
    Ok(LlmSettings {
        max_tokens,
        temperature,
    })
}

/// The run's chunking config: `chunking_config` with the `chunking_tool`
/// argument as its base chunker.
///
/// # Errors
///
/// A `chunking_config` that is not an object.
pub(crate) fn chunking_config(
    parameters: &IndexParameters,
) -> Result<elitea_doc_chunk::ChunkingConfig, elitea_doc_chunk::ConfigError> {
    let mut chunking = elitea_doc_chunk::ChunkingConfig::from_value(&parameters.chunking_config)?;
    if let Some(tool) = &parameters.chunking_tool {
        chunking.base.chunker = Some(tool.clone());
    }
    Ok(chunking)
}

/// The SDK's per-page loader settings (`allowed_to_override` in
/// `document_loaders/constants.py`) that this worker does not read, per
/// extension: spreadsheets, CSV, presentations and PDFs are extracted as a
/// whole by the extraction sidecar and chunked by the document chunkers, so
/// a header row, a sheet name, formula evaluation, pages per chunk or image
/// extraction cannot be honoured. Deliberate (`SOURCE_PARITY.md`).
const IGNORED_LOADER_KEYS: &[(&str, &[&str])] = &[
    (
        ".xlsx",
        &[
            "add_header_to_chunks",
            "header_row_number",
            "sheet_name",
            "evaluate_formulas",
            "skip_size_check",
            "excel_by_sheets",
        ],
    ),
    (
        ".xls",
        &[
            "add_header_to_chunks",
            "header_row_number",
            "sheet_name",
            "evaluate_formulas",
            "skip_size_check",
            "excel_by_sheets",
        ],
    ),
    (
        ".csv",
        &[
            "add_header_to_chunks",
            "header_row_number",
            "encoding",
            "raw_content",
            "cleanse",
        ],
    ),
    (".pptx", &["mode", "pages_per_chunk", "extract_images"]),
    (".pdf", &["extract_images"]),
];

/// The `<extension>.<key>` names of the loader settings a `chunking_config`
/// sets that the run ignores, in a fixed order, each once.
pub(crate) fn ignored_loader_keys(chunking_config: &Value) -> Vec<String> {
    let Some(config) = chunking_config.as_object() else {
        return Vec::new();
    };
    let mut ignored = Vec::new();
    for (extension, keys) in IGNORED_LOADER_KEYS {
        let block = config
            .iter()
            .find(|(name, _)| {
                name.trim().to_ascii_lowercase().trim_start_matches('.')
                    == extension.trim_start_matches('.')
            })
            .and_then(|(_, value)| value.as_object());
        let Some(block) = block else { continue };
        for key in *keys {
            if block.get(*key).is_some_and(|value| !value.is_null()) {
                ignored.push(format!("{extension}.{key}"));
            }
        }
    }
    ignored
}

/// The `index_data` arguments, parsed once per claim: the typed parameters
/// and the chunking config they select. The delivery processor parses to
/// decide which entries to fetch and hands the same value to the run.
#[derive(Debug, Clone)]
pub(crate) struct ParsedArgs {
    pub(crate) parameters: IndexParameters,
    /// The chunking config, or why it is unusable (the run reports that).
    pub(crate) chunking: Result<elitea_doc_chunk::ChunkingConfig, elitea_doc_chunk::ConfigError>,
}

impl ParsedArgs {
    /// # Errors
    ///
    /// The arguments are malformed ([`index_parameters`]).
    pub(crate) fn parse(tool_parameters: &[u8]) -> Result<Self, InputError> {
        let parameters = index_parameters(tool_parameters)?;
        let chunking = chunking_config(&parameters);
        Ok(Self {
            parameters,
            chunking,
        })
    }

    /// Whether the run uses the chat model (`index.llm_model` and
    /// `index.llm_configuration`): the proposal chunker, or the image-file
    /// vision step (on by default). A run that does neither (statistical
    /// only, vision off) neither fetches nor parses those entries.
    /// Unreadable arguments answer no: the run refuses them itself.
    pub(crate) fn needs_chat_model(&self) -> bool {
        self.chunking
            .as_ref()
            .is_ok_and(elitea_doc_chunk::ChunkingConfig::needs_chat_model)
    }
}

/// The embedding model of the binding entry (`model_name`).
///
/// # Errors
///
/// Not an object, or without a `model_name`.
pub(crate) fn embedding_model(raw: &[u8]) -> Result<String, InputError> {
    let map = object(raw, "embedding binding")?;
    text(&map, "model_name", "embedding binding")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_toolkit_configuration_is_read_strictly() {
        let raw = json!({"id": 7, "type": "github", "toolkit_name": "repo", "settings": {"repository": "a/b"}});
        let configuration = toolkit_configuration(raw.to_string().as_bytes()).expect("ok");
        assert_eq!(configuration.toolkit_type, "github");
        assert_eq!(configuration.toolkit_name, "repo");
        assert_eq!(configuration.settings["repository"], "a/b");
        assert!(toolkit_configuration(b"[]").is_err());
        assert!(toolkit_configuration(br#"{"type":"github","toolkit_name":"x"}"#).is_err());
    }

    #[test]
    fn index_parameters_keep_the_sdk_arguments_and_refuse_wrong_types() {
        let raw = json!({
            "index_name": "docs", "clean_index": true, "chunking_tool": "markdown",
            "chunking_config": {"max_tokens": 256}, "branch": "dev",
            "whitelist": ["*.md"], "blacklist": [], "progress_step": 5
        });
        let parameters = index_parameters(raw.to_string().as_bytes()).expect("ok");
        assert_eq!(parameters.index_name, "docs");
        assert!(parameters.clean_index);
        assert_eq!(parameters.chunking_tool.as_deref(), Some("markdown"));
        assert_eq!(parameters.chunking_config["max_tokens"], 256);
        assert_eq!(parameters.branch.as_deref(), Some("dev"));
        assert_eq!(parameters.whitelist, vec!["*.md"]);
        // Every argument stays readable for a family's own connector.
        assert_eq!(parameters.arguments["progress_step"], 5);
        assert_eq!(parameters.arguments["branch"], "dev");
        let minimal = index_parameters(br#"{"index_name":"n"}"#).expect("ok");
        assert_eq!(minimal.chunking_config, Value::Null);
        assert!(!minimal.clean_index);
        assert!(index_parameters(br#"{"index_name":""}"#).is_err());
        assert!(index_parameters(br#"{"index_name":"n","clean_index":"yes"}"#).is_err());
        assert!(index_parameters(br#"{"index_name":"n","whitelist":"*.md"}"#).is_err());
        assert!(index_parameters(br#"{"index_name":"n","chunking_config":[]}"#).is_err());
    }

    #[test]
    fn the_chat_model_entries_are_a_name_and_usable_settings() {
        assert_eq!(llm_model(br#""gpt-4o-mini""#).unwrap(), "gpt-4o-mini");
        assert!(llm_model(b"{}").is_err());
        assert!(llm_model(br#""  ""#).is_err());
        let settings = llm_settings(
            br#"{"temperature": 0.5, "max_tokens": 4096, "top_p": 0.9, "openai_compatible": true}"#,
        )
        .unwrap();
        assert_eq!(settings.max_tokens, Some(4096));
        assert_eq!(settings.temperature, Some(0.5));
        let unusable = llm_settings(br#"{"temperature": 7, "max_tokens": -1}"#).unwrap();
        assert_eq!(unusable, LlmSettings::default());
        let wide = llm_settings(br#"{"temperature": 1.7, "max_tokens": 4096.0}"#).unwrap();
        assert_eq!(wide.temperature, Some(1.7));
        assert_eq!(wide.max_tokens, Some(4096));
        let edges = llm_settings(br#"{"temperature": 2.0001, "max_tokens": 10.5}"#).unwrap();
        assert_eq!(edges, LlmSettings::default(), "warned about, model default");
        assert_eq!(
            llm_settings(br#"{"temperature": 0.0, "max_tokens": 1e20}"#).unwrap(),
            LlmSettings {
                max_tokens: None,
                temperature: Some(0.0)
            }
        );
        assert!(llm_settings(b"[]").is_err());
    }

    #[test]
    fn only_the_proposal_chunker_makes_the_chat_entries_worth_fetching() {
        let needs = |extra: Value| {
            let mut m = json!({"index_name": "n"});
            m.as_object_mut()
                .unwrap()
                .extend(extra.as_object().unwrap().clone());
            ParsedArgs::parse(m.to_string().as_bytes())
                .unwrap()
                .needs_chat_model()
        };
        // The image-file vision step is on by default, so it needs the chat
        // model unless `use_llm: false` switches it off.
        assert!(needs(json!({})));
        assert!(needs(json!({"chunking_tool": "markdown"})));
        assert!(!needs(json!({"chunking_config": {"use_llm": false}})));
        assert!(needs(json!({"chunking_tool": "proposal"})));
        assert!(needs(json!({"chunking_tool": " Proposal"})));
        // Statistical embeds only.
        assert!(!needs(
            json!({"chunking_config": {"use_llm": false, ".md": {"chunker": "statistical"}}})
        ));
        assert!(needs(json!({"chunking_config": {"use_llm": true}})));
        assert!(ParsedArgs::parse(b"not json").is_err());
    }

    #[test]
    fn the_embedding_binding_gives_its_model_name() {
        let raw = br#"{"schema_version":"v2","model_name":"text-embedding-3-small","resolved_model_group":"text-embedding-3-small","route":"raw"}"#;
        assert_eq!(embedding_model(raw).unwrap(), "text-embedding-3-small");
        assert!(embedding_model(br#"{"schema_version":"v2"}"#).is_err());
    }
}
