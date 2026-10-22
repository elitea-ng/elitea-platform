//! The bearer an engine invocation carries for its callbacks, shared by the
//! `DeepWiki` and Inventory engines.

use serde_json::{Map, Value};

/// The callback bearer of an engine invocation's `arguments`:
/// `llm_settings.api_key`, or `llm_settings.openai_api_key` when that is
/// absent or blank (the two spellings the model settings accept), trimmed.
/// It is the per-invocation callback token elitea-main minted and recorded
/// the engine's grant for; elitea-vector admits it for the invocation's
/// project and the engine's source only. `None` without a non-blank one.
#[must_use]
pub fn callback_bearer(arguments: &Map<String, Value>) -> Option<String> {
    let settings = arguments.get("llm_settings")?.as_object()?;
    ["api_key", "openai_api_key"]
        .iter()
        .filter_map(|key| settings.get(*key).and_then(Value::as_str))
        .map(str::trim)
        .find(|value| !value.is_empty())
        .map(str::to_owned)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn arguments(value: &Value) -> Map<String, Value> {
        value.as_object().cloned().unwrap_or_default()
    }

    #[test]
    fn the_bearer_is_the_callback_api_key_in_either_spelling() {
        let bearer = |value: Value| callback_bearer(&arguments(&value));
        assert_eq!(
            bearer(json!({"llm_settings": {"api_key": " t "}})),
            Some("t".to_owned())
        );
        assert_eq!(
            bearer(json!({"llm_settings": {"openai_api_key": "o"}})),
            Some("o".to_owned())
        );
        assert_eq!(
            bearer(json!({"llm_settings": {"api_key": "a", "openai_api_key": "o"}})),
            Some("a".to_owned()),
            "api_key first"
        );
        assert_eq!(
            bearer(json!({"llm_settings": {"api_key": "  ", "openai_api_key": "o"}})),
            Some("o".to_owned()),
            "a blank api_key falls through"
        );
        assert_eq!(bearer(json!({"llm_settings": {"api_key": 7}})), None);
        assert_eq!(bearer(json!({"llm_settings": {}})), None);
        assert_eq!(bearer(json!({"llm_settings": "x"})), None);
        assert_eq!(bearer(json!({})), None);
    }
}
