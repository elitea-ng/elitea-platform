//! A tool result that carries an image (`read_file` on a PNG, JPEG, WebP or
//! GIF when the model can see images).
//!
//! The result is one flat JSON object, so it travels through every tool
//! path as any other (the local session, the cloud executor's runner):
//!
//! ```json
//! {"kind":"image","path":"docs/diagram.png","mime":"image/png",
//!  "bytes":48211,"width":1280,"height":720,"image_base64":"iVBORw0..."}
//! ```
//!
//! Chat completions' tool messages are text-only, so the model adapter
//! sends the object without `image_base64` as the tool message and the
//! image itself as a following user message. Everything else that reads a
//! tool result (the thread history, the commit, the session store, the
//! context accounting) must leave the base64 out: [`without_image`] and
//! [`TOKEN_ESTIMATE`] are for them.

use std::borrow::Cow;

use serde_json::Value;

/// The member holding the image's bytes, standard base64.
pub const IMAGE_FIELD: &str = "image_base64";
/// The `kind` of an image result.
pub const IMAGE_KIND: &str = "image";
/// What one image costs a request, whatever its bytes: providers bill an
/// image by its dimensions (a few hundred to ~1.6k tokens at the 2048 px
/// longest side the tools allow), not by its size.
pub const TOKEN_ESTIMATE: u64 = 1_600;

/// An image taken out of a tool result.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ToolImage {
    pub mime: String,
    pub base64: String,
}

fn is_image_result(value: &Value) -> bool {
    value.get("kind").and_then(Value::as_str) == Some(IMAGE_KIND)
        && value.get(IMAGE_FIELD).is_some_and(Value::is_string)
}

/// Take the image out of `result`, leaving the rest (its metadata) in place;
/// `None`, and `result` untouched, when it is not an image result.
pub fn take_image(result: &mut Value) -> Option<ToolImage> {
    if !is_image_result(result) {
        return None;
    }
    let mime = result.get("mime").and_then(Value::as_str)?.to_owned();
    let Value::String(base64) = result.as_object_mut()?.remove(IMAGE_FIELD)? else {
        return None;
    };
    Some(ToolImage { mime, base64 })
}

/// `result` without its image bytes (borrowed when it has none).
#[must_use]
pub fn without_image(result: &Value) -> Cow<'_, Value> {
    if !is_image_result(result) {
        return Cow::Borrowed(result);
    }
    let mut copy = result.clone();
    if let Some(map) = copy.as_object_mut() {
        map.remove(IMAGE_FIELD);
    }
    Cow::Owned(copy)
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    fn image() -> Value {
        json!({"kind": "image", "mime": "image/png", "bytes": 3, "image_base64": "AAEC"})
    }

    #[test]
    fn the_image_is_taken_out_and_the_metadata_stays() {
        let mut result = image();
        let taken = take_image(&mut result).expect("an image");
        assert_eq!(taken.mime, "image/png");
        assert_eq!(taken.base64, "AAEC");
        assert!(result.get(IMAGE_FIELD).is_none());
        assert_eq!(result["bytes"], 3);
    }

    #[test]
    fn other_results_are_left_alone() {
        let mut text = json!({"kind": "text", "image_base64": "AAEC"});
        assert!(take_image(&mut text).is_none());
        assert!(text.get(IMAGE_FIELD).is_some());
        assert!(matches!(without_image(&text), Cow::Borrowed(_)));
        assert!(without_image(&image()).get(IMAGE_FIELD).is_none());
    }
}
