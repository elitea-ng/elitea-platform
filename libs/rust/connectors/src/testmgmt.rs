//! What the test-management connectors share (ADR-0030 I3): TestRail, qTest,
//! Xray Cloud and the three Zephyr products.
//!
//! * the configuration rules they all apply (an HTTPS base URL, a bounded
//!   credential that is always a valid header value, bounded `index_data`
//!   arguments), reported through [`AtlassianConfigError`] (the type is the
//!   Atlassian pair's, but it is a provider-named, data-free refusal and
//!   nothing in it is Atlassian-specific);
//! * the paging helpers: a JSON list taken out of a page, a bounded
//!   continuation loop's bookkeeping;
//! * the attachment-extension rule of the SDK's `skip_attachment_extensions`.
//!
//! The listing scaffolding (the credential-naming JSON read, the remembered
//! listing with its per-document details) is [`crate::atlassian`]'s.

use serde_json::{Map, Value};
use url::Url;

use crate::atlassian::{AtlassianConfigError, AtlassianConfigErrorCode, MAX_SECRET_BYTES};
use crate::base_url::{self, BasePath, BaseUrlError};
use crate::transport::header::HeaderValue;

/// The longest identity (user name, client id) a configuration carries.
pub const MAX_IDENTITY_BYTES: usize = 1_024;
/// The longest `index_data` text argument (a query, a folder path).
pub const MAX_QUERY_BYTES: usize = 8 * 1_024;
/// The most attachment extensions an argument lists.
pub const MAX_EXTENSIONS: usize = 256;

/// A refusal naming `provider`.
#[must_use]
pub const fn refuse(
    provider: &'static str,
    code: AtlassianConfigErrorCode,
) -> AtlassianConfigError {
    AtlassianConfigError::new(provider, code)
}

/// An HTTPS base URL under the shared rule, with a data-free reason.
///
/// # Errors
///
/// Plain HTTP, an over-long or malformed URL, or (for
/// [`BasePath::OriginOnly`]) a path.
pub fn https_base(
    provider: &'static str,
    value: &str,
    path: BasePath,
) -> Result<Url, AtlassianConfigError> {
    base_url::parse(value, path).map_err(|error| {
        refuse(
            provider,
            match error {
                BaseUrlError::TooLong => AtlassianConfigErrorCode::ResourceExhausted,
                BaseUrlError::PlainHttp => AtlassianConfigErrorCode::PlainHttp,
                BaseUrlError::Invalid => AtlassianConfigErrorCode::InvalidConfiguration,
            },
        )
    })
}

/// A required, non-blank text setting of at most `limit` bytes without a
/// control character.
///
/// # Errors
///
/// Absent, not a string, blank, over the bound, or holding a control
/// character.
pub fn required_text<'a>(
    provider: &'static str,
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, AtlassianConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or(refuse(
            provider,
            AtlassianConfigErrorCode::InvalidConfiguration,
        ))?
        .trim();
    if value.len() > limit {
        return Err(refuse(
            provider,
            AtlassianConfigErrorCode::ResourceExhausted,
        ));
    }
    if value.is_empty() || value.chars().any(char::is_control) {
        return Err(refuse(
            provider,
            AtlassianConfigErrorCode::InvalidConfiguration,
        ));
    }
    Ok(value)
}

/// A required secret that is printable ASCII, so it is always a valid header
/// value and cannot smuggle a second header.
///
/// # Errors
///
/// Absent, blank, over [`MAX_SECRET_BYTES`], or not printable ASCII.
pub fn printable_secret<'a>(
    provider: &'static str,
    object: &'a Map<String, Value>,
    name: &str,
) -> Result<&'a str, AtlassianConfigError> {
    let value = required_text(provider, object, name, MAX_SECRET_BYTES)?;
    if value.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
        Ok(value)
    } else {
        Err(refuse(
            provider,
            AtlassianConfigErrorCode::InvalidConfiguration,
        ))
    }
}

/// `Authorization: Bearer {token}`, marked sensitive.
///
/// # Errors
///
/// The token is not a header value.
pub fn bearer(provider: &'static str, token: &str) -> Result<HeaderValue, AtlassianConfigError> {
    sensitive(provider, &format!("Bearer {token}"))
}

/// `Authorization: Basic base64(user:password)`, marked sensitive.
///
/// # Errors
///
/// The pair is not a header value, or the user holds a `:`.
pub fn basic(
    provider: &'static str,
    user: &str,
    password: &str,
) -> Result<HeaderValue, AtlassianConfigError> {
    use base64::Engine as _;
    if user.contains(':') {
        return Err(refuse(
            provider,
            AtlassianConfigErrorCode::InvalidConfiguration,
        ));
    }
    let pair = zeroize::Zeroizing::new(format!("{user}:{password}"));
    sensitive(
        provider,
        &format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode(pair.as_bytes())
        ),
    )
}

fn sensitive(provider: &'static str, value: &str) -> Result<HeaderValue, AtlassianConfigError> {
    let mut value = HeaderValue::from_str(value)
        .map_err(|_| refuse(provider, AtlassianConfigErrorCode::InvalidConfiguration))?;
    value.set_sensitive(true);
    Ok(value)
}

/// An optional identifier argument spelled as a string or a number
/// (`project_id: 12`, `"12"`): trimmed, URL-path safe.
///
/// # Errors
///
/// Another type, or text that is not an identifier.
pub fn identifier_argument(
    provider: &'static str,
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<String>, AtlassianConfigError> {
    let invalid = || refuse(provider, AtlassianConfigErrorCode::InvalidConfiguration);
    let text = match arguments.get(name) {
        None | Some(Value::Null) => return Ok(None),
        Some(Value::String(text)) => text.trim().to_owned(),
        Some(Value::Number(number)) if number.as_u64().is_some() => number.to_string(),
        Some(_) => return Err(invalid()),
    };
    if text.is_empty() {
        return Ok(None);
    }
    if !crate::atlassian::safe_identifier(&text) {
        return Err(invalid());
    }
    Ok(Some(text))
}

/// An optional free-text argument, trimmed; blank is absent.
///
/// # Errors
///
/// Another type, a control character, or over [`MAX_QUERY_BYTES`].
pub fn text_argument(
    provider: &'static str,
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<String>, AtlassianConfigError> {
    Ok(
        crate::atlassian::optional_text(arguments, name, MAX_QUERY_BYTES, provider)?
            .map(str::trim)
            .filter(|text| !text.is_empty())
            .map(ToOwned::to_owned),
    )
}

/// The SDK's `skip_attachment_extensions`: lower-case dotted extensions.
///
/// # Errors
///
/// Another type, or more than [`MAX_EXTENSIONS`].
pub fn extension_list(
    provider: &'static str,
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Vec<String>, AtlassianConfigError> {
    let list = crate::atlassian::text_list(arguments, name, provider)?;
    if list.len() > MAX_EXTENSIONS {
        return Err(refuse(
            provider,
            AtlassianConfigErrorCode::ResourceExhausted,
        ));
    }
    Ok(list
        .into_iter()
        .map(|extension| {
            let lower = extension.to_ascii_lowercase();
            if lower.starts_with('.') {
                lower
            } else {
                format!(".{lower}")
            }
        })
        .collect())
}

/// Whether `filename`'s lower-case dotted extension is in `skipped`.
#[must_use]
pub fn extension_skipped(skipped: &[String], filename: &str) -> bool {
    filename
        .rsplit_once('.')
        .map(|(_, extension)| format!(".{}", extension.to_ascii_lowercase()))
        .is_some_and(|extension| skipped.contains(&extension))
}

/// The list at `page[name]`, taken (not cloned); absent or null is empty.
///
/// # Errors
///
/// The field is another type.
#[cfg(feature = "content-source")]
pub fn take_list(
    page: &mut Value,
    name: &str,
    why: &'static str,
) -> Result<Vec<Value>, crate::source::Failure> {
    match page.get_mut(name).map(Value::take) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) => Ok(items),
        Some(_) => Err(crate::source::Failure::InvalidResponse(why)),
    }
}

/// An id spelled as a number or a string, as text.
#[must_use]
pub fn scalar_text(value: &Value) -> Option<String> {
    match value {
        Value::String(text) if !text.trim().is_empty() => Some(text.trim().to_owned()),
        Value::Number(number) => Some(number.to_string()),
        _ => None,
    }
}

/// `{scheme}://{host}[:port]` of `url`, for display in metadata (never with
/// credentials: [`base_url::parse`] refuses a URL that carries them).
#[must_use]
pub fn origin_text(url: &Url) -> String {
    url.as_str().trim_end_matches('/').to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn extensions_are_lower_case_and_dotted() {
        let arguments = json!({"skip": [".PNG", "zip", ""]});
        let list = extension_list("T", arguments.as_object().unwrap(), "skip").unwrap();
        assert_eq!(list, vec![".png", ".zip"]);
        assert!(extension_skipped(&list, "Shot.PNG"));
        assert!(!extension_skipped(&list, "notes.txt"));
        assert!(!extension_skipped(&list, "noextension"));
    }

    #[test]
    fn identifiers_are_strings_or_numbers_and_path_safe() {
        let arguments = json!({"a": 12, "b": " 7 ", "c": "x/../y", "d": 1.5, "e": ""});
        let object = arguments.as_object().unwrap();
        assert_eq!(
            identifier_argument("T", object, "a").unwrap().as_deref(),
            Some("12")
        );
        assert_eq!(
            identifier_argument("T", object, "b").unwrap().as_deref(),
            Some("7")
        );
        assert!(identifier_argument("T", object, "c").is_err());
        assert!(identifier_argument("T", object, "d").is_err());
        assert_eq!(identifier_argument("T", object, "e").unwrap(), None);
        assert_eq!(identifier_argument("T", object, "missing").unwrap(), None);
    }

    #[test]
    fn secrets_must_be_printable_ascii() {
        let object = json!({"t": "abc123", "bad": "a b", "nl": "a\nb"});
        let object = object.as_object().unwrap();
        assert!(printable_secret("T", object, "t").is_ok());
        assert!(printable_secret("T", object, "bad").is_err());
        assert!(printable_secret("T", object, "nl").is_err());
        assert!(printable_secret("T", object, "missing").is_err());
    }

    #[test]
    fn credentials_are_sensitive_headers() {
        assert!(bearer("T", "tok").unwrap().is_sensitive());
        let header = basic("T", "u@example.com", "pw").unwrap();
        assert!(header.is_sensitive());
        assert_eq!(header.to_str().unwrap(), "Basic dUBleGFtcGxlLmNvbTpwdw==");
        assert!(basic("T", "a:b", "pw").is_err());
    }
}
