//! The model settings one invocation carries in its `llm_settings` argument.
//!
//! The facade in elitea-main writes that block (`material.CallbackSettings`):
//! `api_base` (`{platform}/llm/v1`), `api_key` (a short-lived callback
//! bearer), `organization` (the project id, sent as `X-Project-Id` — the
//! header the worker sends, ADR-0018's primary selector), `execution_id`
//! (the run the gateway attributes spend to, `X-Elitea-Execution-Id`) and
//! the caller's `model_name`; `LiftToolLLMSettings` may add `max_tokens`,
//! `temperature` and `reasoning_effort`. The headers and their rules are the
//! `/llm` caller contract the worker follows too
//! (`docs/llm-caller-contract.md` in this crate). The Python workers also read the legacy
//! `openai_api_base` / `openai_api_key` spellings, `provider`,
//! `max_retries` and `streaming`, so those are accepted here too.
//!
//! Parsing is STRICT where the Python engine was lenient in a way that hid
//! a failure until much later:
//!
//! * a missing `model_name` is refused. Python fell back to `gpt-4o-mini`,
//!   a model the platform gateway usually does not serve, so the run failed
//!   minutes later at the first page instead of at the start;
//! * a missing embedding model is refused. Python fell back to
//!   `text-embedding-3-large` for the same effect;
//! * a value of the wrong JSON type is refused instead of being coerced.
//!
//! The missing-transport messages are the Python ones, word for word
//! (`llm_settings.api_base is required`), because the facade's own tests and
//! runbooks quote them.
//!
//! `temperature` is accepted and ignored: the Python workers never read it
//! (each call site fixed its own value), and ADR-0026 decision 8 keeps their
//! values (see [`super::chat::Sampling`]).

use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::pyvalue::py_truthy;
use elitea_engine_core::secret::Secret;
use serde_json::Value;
use std::fmt;

/// Python's `max_retries` default (`LangChain` and the `openai` SDK).
pub const DEFAULT_MAX_RETRIES: u32 = 2;

/// The `max_tokens` default (ADR-0026 decision 8; `wiki_subprocess_worker`).
pub const DEFAULT_MAX_TOKENS: u32 = 64_000;

/// The longest execution id the `/llm` edge keeps (`maxExecutionIDLen` in
/// elitea-main `internal/llmproxy`); a longer one is dropped there, so it is
/// refused here, where the mistake is.
pub const MAX_EXECUTION_ID_BYTES: usize = 128;

/// Whether `id` passes the edge's shape rule: 1–128 bytes of ASCII letters,
/// digits, `-`, `_` and `.`.
#[must_use]
pub fn valid_execution_id(id: &str) -> bool {
    (1..=MAX_EXECUTION_ID_BYTES).contains(&id.len())
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
}

/// A reasoning model's effort, sent as `reasoning_effort` when set. The
/// worker's values (`ModelReasoningEffort`): `none` turns reasoning off on a
/// model that reasons by default.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ReasoningEffort {
    None,
    Low,
    Medium,
    High,
}

impl ReasoningEffort {
    /// The wire value.
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::None => "none",
            Self::Low => "low",
            Self::Medium => "medium",
            Self::High => "high",
        }
    }
}

/// A retry count above this is a configuration mistake, not a policy: with
/// the backoff cap of 8 s it would keep a dead gateway busy for minutes.
const MAX_RETRIES_CEILING: u32 = 10;

/// The provider family the toolkit names. Both reach the model through the
/// gateway's OpenAI-compatible surface (ADR-0026, owner decision 4); the
/// family only changes how the base URL is normalised.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Provider {
    OpenAi,
    Anthropic,
}

/// One invocation's model transport and tuning.
///
/// `Debug` is safe to print: the key is a [`Secret`].
#[derive(Clone, PartialEq, Eq)]
pub struct ModelSettings {
    pub provider: Provider,
    /// The OpenAI-compatible base URL, without a trailing slash; requests
    /// go to `{api_base}/chat/completions` and `{api_base}/embeddings`.
    pub api_base: String,
    pub api_key: Secret,
    /// The project the call bills, sent as `X-Project-Id`. The callback
    /// bearer is bound to it, so the edge refuses any other.
    pub organization: Option<String>,
    /// The execution the gateway attributes the call's spend to, sent as
    /// `X-Elitea-Execution-Id`. Absent, the call is billed but belongs to no
    /// run.
    pub execution_id: Option<String>,
    /// Sent as `reasoning_effort` when set; unset, the body is unchanged.
    pub reasoning_effort: Option<ReasoningEffort>,
    pub model_name: String,
    pub max_retries: u32,
    pub max_tokens: u32,
    /// Whether chat answers stream (Python `llm_settings.streaming`,
    /// default on). A deployment whose endpoint cannot stream turns it off.
    pub streaming: bool,
}

impl fmt::Debug for ModelSettings {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ModelSettings")
            .field("provider", &self.provider)
            .field("api_base", &self.api_base)
            .field("api_key", &self.api_key)
            .field("organization", &self.organization)
            .field("execution_id", &self.execution_id)
            .field("reasoning_effort", &self.reasoning_effort)
            .field("model_name", &self.model_name)
            .field("max_retries", &self.max_retries)
            .field("max_tokens", &self.max_tokens)
            .field("streaming", &self.streaming)
            .finish()
    }
}

fn invalid(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

/// Python's `settings.get(a) or settings.get(b)`, refusing a truthy value
/// that is not a string.
fn first_string(
    settings: &serde_json::Map<String, Value>,
    keys: &[&str],
) -> Result<Option<String>, EngineError> {
    for key in keys {
        match settings.get(*key) {
            Some(value) if py_truthy(value) => {
                return match value {
                    Value::String(text) => Ok(Some(text.clone())),
                    _ => Err(invalid(format!("llm_settings.{key} must be a string"))),
                };
            }
            _ => {}
        }
    }
    Ok(None)
}

/// A non-negative whole number, or the default when absent or null.
fn count(
    settings: &serde_json::Map<String, Value>,
    key: &str,
    default: u32,
) -> Result<u32, EngineError> {
    match settings.get(key) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Number(number)) => number
            .as_u64()
            .and_then(|value| u32::try_from(value).ok())
            .ok_or_else(|| invalid(format!("llm_settings.{key} must be a whole number >= 0"))),
        Some(_) => Err(invalid(format!(
            "llm_settings.{key} must be a whole number >= 0"
        ))),
    }
}

/// Normalise and check a base URL. A URL with userinfo is refused: the key
/// travels in the `Authorization` header only, and a credential in the URL
/// would reach every error message that names the URL.
fn base_url(raw: &str, provider: Provider) -> Result<String, EngineError> {
    let trimmed = raw.trim().trim_end_matches('/');
    let parsed = reqwest::Url::parse(trimmed)
        .map_err(|_| invalid("llm_settings.api_base must be an absolute http(s) URL"))?;
    if !matches!(parsed.scheme(), "http" | "https") || parsed.host_str().is_none() {
        return Err(invalid(
            "llm_settings.api_base must be an absolute http(s) URL",
        ));
    }
    if !parsed.username().is_empty() || parsed.password().is_some() {
        return Err(invalid(
            "llm_settings.api_base must not carry credentials; send the key as llm_settings.api_key",
        ));
    }
    if parsed.query().is_some() || parsed.fragment().is_some() {
        return Err(invalid(
            "llm_settings.api_base must not carry a query or a fragment",
        ));
    }
    let mut base = trimmed.to_owned();
    // The Python anthropic branch sent embeddings to `{api_base}/v1` when
    // the base lacked it; through the OpenAI-compatible surface the chat
    // calls go there too.
    if provider == Provider::Anthropic && !base.ends_with("/v1") {
        base.push_str("/v1");
    }
    Ok(base)
}

impl ModelSettings {
    /// Read the `llm_settings` argument.
    ///
    /// # Errors
    ///
    /// A `ValueError` naming the key that is missing or malformed.
    pub fn from_llm_settings(llm_settings: &Value) -> Result<Self, EngineError> {
        let empty = serde_json::Map::new();
        let settings = match llm_settings {
            Value::Object(map) => map,
            Value::Null => &empty,
            _ => return Err(invalid("llm_settings must be an object")),
        };
        let provider = match first_string(settings, &["provider"])?.as_deref() {
            None | Some("openai") => Provider::OpenAi,
            Some("anthropic") => Provider::Anthropic,
            Some(other) => {
                return Err(invalid(format!(
                    "llm_settings.provider must be 'openai' or 'anthropic', got '{other}'"
                )));
            }
        };
        let api_base = first_string(settings, &["api_base", "openai_api_base"])?
            .ok_or_else(|| invalid("llm_settings.api_base is required"))?;
        let api_key = first_string(settings, &["api_key", "openai_api_key"])?
            .and_then(Secret::new)
            .ok_or_else(|| invalid("llm_settings.api_key is required"))?;
        let model_name = first_string(settings, &["model_name"])?
            .map(|name| name.trim().to_owned())
            .filter(|name| !name.is_empty())
            .ok_or_else(|| invalid("llm_settings.model_name is required"))?;
        let organization = match settings.get("organization") {
            None | Some(Value::Null) => None,
            Some(Value::String(text)) if text.is_empty() => None,
            Some(Value::String(text)) => Some(text.clone()),
            // The facade writes the project id as a string; a number is the
            // same id and is what an older caller sent.
            Some(Value::Number(number)) => Some(number.to_string()),
            Some(_) => {
                return Err(invalid(
                    "llm_settings.organization must be a string or a number",
                ));
            }
        };
        let execution_id = match settings.get("execution_id") {
            None | Some(Value::Null) => None,
            Some(Value::String(id)) if valid_execution_id(id) => Some(id.clone()),
            Some(_) => {
                return Err(invalid(format!(
                    "llm_settings.execution_id must be 1-{MAX_EXECUTION_ID_BYTES} letters, digits, '-', '_' or '.'"
                )));
            }
        };
        let reasoning_effort = match first_string(settings, &["reasoning_effort"])?.as_deref() {
            None => None,
            Some("none") => Some(ReasoningEffort::None),
            Some("low") => Some(ReasoningEffort::Low),
            Some("medium") => Some(ReasoningEffort::Medium),
            Some("high") => Some(ReasoningEffort::High),
            Some(other) => {
                return Err(invalid(format!(
                    "llm_settings.reasoning_effort must be none, low, medium or high, got '{other}'"
                )));
            }
        };
        let streaming = match settings.get("streaming") {
            None | Some(Value::Null) => true,
            Some(Value::Bool(flag)) => *flag,
            Some(_) => return Err(invalid("llm_settings.streaming must be true or false")),
        };
        let max_retries = count(settings, "max_retries", DEFAULT_MAX_RETRIES)?;
        if max_retries > MAX_RETRIES_CEILING {
            return Err(invalid(format!(
                "llm_settings.max_retries must be at most {MAX_RETRIES_CEILING}"
            )));
        }
        let max_tokens = count(settings, "max_tokens", DEFAULT_MAX_TOKENS)?;
        if max_tokens == 0 {
            return Err(invalid("llm_settings.max_tokens must be at least 1"));
        }
        Ok(Self {
            provider,
            api_base: base_url(&api_base, provider)?,
            api_key,
            organization,
            execution_id,
            reasoning_effort,
            model_name,
            max_retries,
            max_tokens,
            streaming,
        })
    }
}

/// The embedding model the `embedding_model` argument names.
///
/// The wiki worker takes a string; the ask and deep-research workers also
/// take `{"model_name": …}`. Both shapes are accepted.
///
/// # Errors
///
/// A `ValueError` when it is missing, empty or of another shape.
pub fn embedding_model_name(embedding_model: &Value) -> Result<String, EngineError> {
    let name = match embedding_model {
        Value::String(text) => Some(text.as_str()),
        Value::Object(map) => match map.get("model_name") {
            Some(Value::String(text)) => Some(text.as_str()),
            None | Some(Value::Null) => None,
            Some(_) => return Err(invalid("embedding_model.model_name must be a string")),
        },
        Value::Null => None,
        _ => {
            return Err(invalid(
                "embedding_model must be a model name or an object with model_name",
            ));
        }
    };
    name.map(str::trim)
        .filter(|name| !name.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| invalid("embedding_model is required"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[allow(clippy::needless_pass_by_value)]
    fn parse(value: Value) -> Result<ModelSettings, String> {
        ModelSettings::from_llm_settings(&value).map_err(|e| e.wire_message().to_owned())
    }

    #[test]
    fn the_facade_block_parses() {
        let settings = parse(json!({
            "api_base": "https://platform.example/llm/v1/",
            "api_key": "callback-bearer",
            "organization": "42",
            "model_name": "gpt-4o",
            "max_tokens": 4096,
            "temperature": 0.7,
        }));
        let Ok(settings) = settings else {
            panic!("{settings:?}");
        };
        assert_eq!(settings.api_base, "https://platform.example/llm/v1");
        assert_eq!(settings.api_key.expose(), "callback-bearer");
        assert_eq!(settings.organization.as_deref(), Some("42"));
        assert_eq!(settings.max_tokens, 4096);
        assert_eq!(settings.max_retries, DEFAULT_MAX_RETRIES);
        assert!(settings.streaming);
        assert_eq!(settings.provider, Provider::OpenAi);
        assert_eq!(settings.execution_id, None);
        assert_eq!(settings.reasoning_effort, None);
    }

    #[test]
    fn the_execution_id_and_the_effort_are_read_and_checked() {
        let base = json!({"api_base": "http://gw/llm/v1", "api_key": "k", "model_name": "m"});
        let with = |key: &str, value: Value| {
            let mut block = base.clone();
            block[key] = value;
            parse(block)
        };
        let ok = with("execution_id", json!("callback-7f3a.b_c"));
        assert_eq!(
            ok.ok().and_then(|s| s.execution_id).as_deref(),
            Some("callback-7f3a.b_c")
        );
        for bad in [json!(""), json!("a:b"), json!(7), json!("x".repeat(129))] {
            assert!(
                with("execution_id", bad.clone()).is_err_and(|e| e.contains("execution_id")),
                "{bad}"
            );
        }
        assert_eq!(
            with("reasoning_effort", json!("low"))
                .ok()
                .and_then(|s| s.reasoning_effort),
            Some(ReasoningEffort::Low)
        );
        assert!(with("reasoning_effort", json!("max")).is_err());
    }

    #[test]
    fn the_legacy_spellings_and_defaults_apply() {
        let Ok(settings) = parse(json!({
            "openai_api_base": "http://gw:8080/llm/v1",
            "openai_api_key": "k",
            "model_name": "m",
            "organization": 7,
            "streaming": false,
            "max_retries": 0,
        })) else {
            panic!("legacy spellings refused");
        };
        assert_eq!(settings.api_base, "http://gw:8080/llm/v1");
        assert_eq!(settings.organization.as_deref(), Some("7"));
        assert!(!settings.streaming);
        assert_eq!(settings.max_retries, 0);
        assert_eq!(settings.max_tokens, DEFAULT_MAX_TOKENS);
    }

    #[test]
    fn the_missing_transport_reads_as_python() {
        assert_eq!(
            parse(json!({"api_key": "k", "model_name": "m"}))
                .err()
                .as_deref(),
            Some("llm_settings.api_base is required")
        );
        assert_eq!(
            parse(json!({"api_base": "http://gw/v1", "api_key": "", "model_name": "m"}))
                .err()
                .as_deref(),
            Some("llm_settings.api_key is required")
        );
        assert_eq!(
            parse(json!({"api_base": "http://gw/v1", "api_key": "k"}))
                .err()
                .as_deref(),
            Some("llm_settings.model_name is required")
        );
        assert_eq!(
            parse(Value::Null).err().as_deref(),
            Some("llm_settings.api_base is required")
        );
    }

    #[test]
    fn a_refusal_is_a_value_error() {
        let error = ModelSettings::from_llm_settings(&json!({}));
        assert_eq!(
            error.map(|_| ()).map_err(|e| e.category()),
            Err("invalid_input")
        );
    }

    #[test]
    fn a_credential_in_the_url_is_refused_and_not_echoed() {
        let error = parse(json!({
            "api_base": "https://user:hunter2@gw/v1", "api_key": "k", "model_name": "m"
        }))
        .err()
        .unwrap_or_default();
        assert!(error.contains("must not carry credentials"), "{error}");
        assert!(!error.contains("hunter2"), "{error}");
    }

    #[test]
    fn wrong_types_are_refused() {
        for (settings, expected) in [
            (
                json!({"api_base": 5, "api_key": "k", "model_name": "m"}),
                "llm_settings.api_base must be a string",
            ),
            (
                json!({"api_base": "ftp://gw", "api_key": "k", "model_name": "m"}),
                "llm_settings.api_base must be an absolute http(s) URL",
            ),
            (
                json!({"api_base": "http://gw", "api_key": "k", "model_name": "m", "max_tokens": -1}),
                "llm_settings.max_tokens must be a whole number >= 0",
            ),
            (
                json!({"api_base": "http://gw", "api_key": "k", "model_name": "m", "streaming": "yes"}),
                "llm_settings.streaming must be true or false",
            ),
            (
                json!({"api_base": "http://gw", "api_key": "k", "model_name": "m", "provider": "bedrock"}),
                "llm_settings.provider must be 'openai' or 'anthropic', got 'bedrock'",
            ),
        ] {
            assert_eq!(parse(settings).err().as_deref(), Some(expected));
        }
    }

    #[test]
    fn the_anthropic_family_goes_to_the_v1_surface() {
        let Ok(settings) = parse(json!({
            "provider": "anthropic", "api_base": "https://gw/llm", "api_key": "k", "model_name": "claude"
        })) else {
            panic!("anthropic refused");
        };
        assert_eq!(settings.api_base, "https://gw/llm/v1");
    }

    #[test]
    fn debug_never_shows_the_key() {
        let Ok(settings) = parse(json!({
            "api_base": "https://gw/v1", "api_key": "sk-live-very-secret", "model_name": "m"
        })) else {
            panic!("refused");
        };
        let shown = format!("{settings:?}");
        assert!(!shown.contains("very-secret"), "{shown}");
    }

    #[test]
    fn the_embedding_model_takes_both_shapes() {
        assert_eq!(
            embedding_model_name(&json!("text-embedding-3-large")).ok(),
            Some("text-embedding-3-large".to_owned())
        );
        assert_eq!(
            embedding_model_name(&json!({"model_name": "nomic"})).ok(),
            Some("nomic".to_owned())
        );
        assert_eq!(
            embedding_model_name(&Value::Null)
                .err()
                .map(|e| e.wire_message().to_owned()),
            Some("embedding_model is required".to_owned())
        );
        assert!(embedding_model_name(&json!(3)).is_err());
    }
}
