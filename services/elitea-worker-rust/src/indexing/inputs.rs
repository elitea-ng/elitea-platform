//! The index bundle's entries, read strictly.
//!
//! * The toolkit configuration (`index.toolkit_configuration`): what Main's
//!   authoritative input resolver wrote, `{id, type, toolkit_name, settings}`
//!   with the settings expanded (credentials resolved).
//! * The tool parameters (`index.tool_parameters`): the `index_data`
//!   arguments as the request carried them.
//! * The embedding binding (`index.embedding_binding`): Main's
//!   `EmbeddingBinding.MarshalCanonical`, of which only `model_name` is read.
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
        let minimal = index_parameters(br#"{"index_name":"n"}"#).expect("ok");
        assert_eq!(minimal.chunking_config, Value::Null);
        assert!(!minimal.clean_index);
        assert!(index_parameters(br#"{"index_name":""}"#).is_err());
        assert!(index_parameters(br#"{"index_name":"n","clean_index":"yes"}"#).is_err());
        assert!(index_parameters(br#"{"index_name":"n","whitelist":"*.md"}"#).is_err());
        assert!(index_parameters(br#"{"index_name":"n","chunking_config":[]}"#).is_err());
    }

    #[test]
    fn the_embedding_binding_gives_its_model_name() {
        let raw = br#"{"schema_version":"v2","model_name":"text-embedding-3-small","resolved_model_group":"text-embedding-3-small","route":"raw"}"#;
        assert_eq!(embedding_model(raw).unwrap(), "text-embedding-3-small");
        assert!(embedding_model(br#"{"schema_version":"v2"}"#).is_err());
    }
}
