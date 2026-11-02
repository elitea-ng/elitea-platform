//! Toolkit-instance image settings of `ado_boards` and `ado_wiki` (#985).
//!
//! `process_images` and `image_description_prompt` are tool arguments in the
//! SDK. An instance can also set them in its settings. The instance value is
//! the default for each call that does not name the argument (or sends
//! `null`), and an argument that the call names overrides it. Without an
//! instance value, each tool keeps its own default.

use serde_json::{Map, Value};

use crate::toolkits::families::ado::config::AdoConfigError;
use crate::toolkits::families::ado::toolset::{MAX_TEXT_BYTES, invalid_arguments, optional_str};

/// The settings key that sets the instance's `process_images` default.
pub(crate) const PROCESS_IMAGES: &str = "process_images";
/// The settings key that sets the instance's `image_description_prompt`.
pub(crate) const IMAGE_DESCRIPTION_PROMPT: &str = "image_description_prompt";

/// The image defaults of one toolkit instance.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub(crate) struct AdoImageDefaults {
    process_images: Option<bool>,
    image_description_prompt: Option<Box<str>>,
}

impl AdoImageDefaults {
    /// Read the two settings. An absent or `null` value sets no default. An
    /// empty or blank prompt also sets none. A value of the wrong type, or a
    /// prompt longer than a tool argument may be, is an invalid
    /// configuration.
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AdoConfigError> {
        let process_images = match settings.get(PROCESS_IMAGES) {
            None | Some(Value::Null) => None,
            Some(Value::Bool(value)) => Some(*value),
            Some(_) => return Err(AdoConfigError::invalid()),
        };
        let image_description_prompt = match settings.get(IMAGE_DESCRIPTION_PROMPT) {
            None | Some(Value::Null) => None,
            Some(Value::String(prompt))
                if prompt.len() > MAX_TEXT_BYTES || prompt.contains('\0') =>
            {
                return Err(AdoConfigError::invalid());
            }
            Some(Value::String(prompt)) if prompt.trim().is_empty() => None,
            Some(Value::String(prompt)) => Some(prompt.as_str().into()),
            Some(_) => return Err(AdoConfigError::invalid()),
        };
        Ok(Self {
            process_images,
            image_description_prompt,
        })
    }

    /// The `process_images` value of one call: the call's argument, else the
    /// instance setting, else `tool_default`.
    pub(crate) fn process_images(
        &self,
        arguments: &Map<String, Value>,
        tool_default: bool,
    ) -> adk_core::Result<bool> {
        match arguments.get(PROCESS_IMAGES) {
            None | Some(Value::Null) => Ok(self.process_images.unwrap_or(tool_default)),
            Some(Value::Bool(value)) => Ok(*value),
            Some(_) => Err(invalid_arguments()),
        }
    }

    /// The `image_description_prompt` of one call: the call's argument, else
    /// the instance setting, else none (the tool's own default prompt).
    pub(crate) fn image_description_prompt<'a>(
        &'a self,
        arguments: &'a Map<String, Value>,
    ) -> adk_core::Result<Option<&'a str>> {
        if matches!(
            arguments.get(IMAGE_DESCRIPTION_PROMPT),
            None | Some(Value::Null)
        ) {
            return Ok(self.image_description_prompt.as_deref());
        }
        optional_str(arguments, IMAGE_DESCRIPTION_PROMPT, MAX_TEXT_BYTES)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn map(value: &Value) -> Map<String, Value> {
        value.as_object().expect("an object").clone()
    }

    #[test]
    fn no_setting_keeps_each_tools_own_default() {
        let defaults = AdoImageDefaults::parse(&map(&json!({}))).expect("parses");
        let empty = Map::new();
        assert!(defaults.process_images(&empty, true).expect("valid"));
        assert!(!defaults.process_images(&empty, false).expect("valid"));
        assert_eq!(
            defaults.image_description_prompt(&empty).expect("valid"),
            None
        );
    }

    #[test]
    fn the_instance_setting_is_the_default_of_a_call_that_omits_the_argument() {
        let defaults = AdoImageDefaults::parse(&map(
            &json!({"process_images": true, "image_description_prompt": "short caption"}),
        ))
        .expect("parses");
        let empty = Map::new();
        assert!(defaults.process_images(&empty, false).expect("valid"));
        assert_eq!(
            defaults.image_description_prompt(&empty).expect("valid"),
            Some("short caption")
        );
        // A `null` argument is an omitted one.
        let null = map(&json!({"process_images": null, "image_description_prompt": null}));
        assert!(defaults.process_images(&null, false).expect("valid"));
        assert_eq!(
            defaults.image_description_prompt(&null).expect("valid"),
            Some("short caption")
        );
    }

    #[test]
    fn the_call_argument_overrides_the_instance_setting() {
        let defaults = AdoImageDefaults::parse(&map(
            &json!({"process_images": true, "image_description_prompt": "instance"}),
        ))
        .expect("parses");
        let call = map(&json!({"process_images": false, "image_description_prompt": "call"}));
        assert!(!defaults.process_images(&call, true).expect("valid"));
        assert_eq!(
            defaults.image_description_prompt(&call).expect("valid"),
            Some("call")
        );
        let off = AdoImageDefaults::parse(&map(&json!({"process_images": false}))).expect("parses");
        assert!(
            off.process_images(&map(&json!({"process_images": true})), false)
                .expect("valid")
        );
    }

    #[test]
    fn null_and_blank_settings_set_no_default() {
        let defaults = AdoImageDefaults::parse(&map(
            &json!({"process_images": null, "image_description_prompt": "  "}),
        ))
        .expect("parses");
        assert_eq!(defaults, AdoImageDefaults::default());
    }

    #[test]
    fn a_wrongly_typed_or_oversized_setting_is_refused() {
        for settings in [
            json!({"process_images": "yes"}),
            json!({"process_images": 1}),
            json!({"image_description_prompt": 7}),
            json!({"image_description_prompt": "x".repeat(MAX_TEXT_BYTES + 1)}),
        ] {
            assert!(
                AdoImageDefaults::parse(&map(&settings)).is_err(),
                "{settings} must be refused"
            );
        }
    }

    #[test]
    fn a_wrongly_typed_argument_is_refused() {
        let defaults = AdoImageDefaults::default();
        assert!(
            defaults
                .process_images(&map(&json!({"process_images": "true"})), false)
                .is_err()
        );
        assert!(
            defaults
                .image_description_prompt(&map(&json!({"image_description_prompt": 1})))
                .is_err()
        );
    }
}
