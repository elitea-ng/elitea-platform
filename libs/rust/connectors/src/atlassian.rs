//! What the Confluence and Jira connectors share (ADR-0030 I2): the SDK's
//! credential model (a bearer token, a `JSESSIONID` cookie string, or a
//! username and API key sent as Basic), the Cloud-versus-Server decision, the
//! bounded custom headers, the text helpers both configurations read
//! settings with, and (feature `content-source`) the listing scaffolding
//! both connectors run on: the credential-naming JSON read, the remembered
//! listing with its per-document details, the key segment rule.
//!
//! The agent-runtime families keep their own copies of this logic for now
//! (`toolkits/families/{confluence,jira}/config.rs`); they are to be pointed
//! here as a follow-up, the way the git families were in #1249.

use std::fmt;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use crate::base_url::{self, BasePath, BaseUrlError, host_matches_domain};
use crate::transport::Request;
use crate::transport::header::{ACCEPT, AUTHORIZATION, COOKIE, HeaderName, HeaderValue};

#[cfg(feature = "content-source")]
pub use self::listing::{
    AttachmentDownloader, COMMENT_LISTING_CONCURRENCY, DetailedListing, Details,
    MAX_DOWNLOAD_REDIRECTS, Snapshot, authorized_json, refused_credential, same_origin,
};

/// The longest secret a configuration carries.
pub const MAX_SECRET_BYTES: usize = 16 * 1_024;
/// The longest identity (username, hosting) a configuration carries.
pub const MAX_IDENTITY_BYTES: usize = 1_024;
const MAX_CUSTOM_HEADERS: usize = 32;
const MAX_HEADER_VALUE_BYTES: usize = 8 * 1_024;
/// The platform's own header namespace; a toolkit may not set it.
const RESERVED_PLATFORM_HEADER_PREFIX: &str = "x-elitea-";

/// Why an Atlassian configuration was refused. Data-free.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AtlassianConfigErrorCode {
    /// A required setting is missing or of the wrong shape.
    InvalidConfiguration,
    /// A setting is longer than its bound.
    ResourceExhausted,
    /// A plain `http://` base URL.
    PlainHttp,
    /// `verify_ssl: false`: this runtime always verifies TLS.
    VerifySslDisabled,
    /// A custom header that would replace the credential or the transport's
    /// own headers.
    ReservedHeader,
}

/// A refused configuration, naming the provider and the reason, never a
/// value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AtlassianConfigError {
    provider: &'static str,
    code: AtlassianConfigErrorCode,
}

impl AtlassianConfigError {
    #[must_use]
    pub const fn new(provider: &'static str, code: AtlassianConfigErrorCode) -> Self {
        Self { provider, code }
    }

    #[must_use]
    pub const fn code(&self) -> AtlassianConfigErrorCode {
        self.code
    }
}

impl fmt::Display for AtlassianConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        let provider = self.provider;
        match self.code {
            AtlassianConfigErrorCode::InvalidConfiguration => {
                write!(formatter, "the {provider} toolkit configuration is invalid")
            }
            AtlassianConfigErrorCode::ResourceExhausted => write!(
                formatter,
                "the {provider} toolkit configuration exceeds its approved limit"
            ),
            AtlassianConfigErrorCode::PlainHttp => write!(
                formatter,
                "the {provider} base URL is plain HTTP, which this runtime does not support"
            ),
            AtlassianConfigErrorCode::VerifySslDisabled => write!(
                formatter,
                "the {provider} toolkit disables TLS verification, which this runtime does not support"
            ),
            AtlassianConfigErrorCode::ReservedHeader => write!(
                formatter,
                "the {provider} toolkit sets a custom header this runtime reserves"
            ),
        }
    }
}

impl std::error::Error for AtlassianConfigError {}

/// How the SDK authenticates: `token` (a bearer, or session cookies when it
/// carries `JSESSIONID`), else `username` with `api_key` as Basic.
pub enum Credential {
    Bearer(Zeroizing<String>),
    /// A token carrying `JSESSIONID`: sent as `Cookie`, `name=value` pairs
    /// separated by `; `.
    Cookie(Zeroizing<String>),
    Basic {
        username: Box<str>,
        password: Zeroizing<String>,
    },
}

impl Credential {
    /// `_credential` of both SDK wrappers: either `token`, or both `api_key`
    /// and `username`, must be provided for authentication.
    ///
    /// # Errors
    ///
    /// Neither form is present, a value is of the wrong shape, or a secret
    /// is over its bound.
    pub fn parse(
        configuration: &Map<String, Value>,
        provider: &'static str,
    ) -> Result<Self, AtlassianConfigError> {
        let refused = |code| AtlassianConfigError::new(provider, code);
        if let Some(token) = optional_secret(configuration, "token", provider)? {
            let token = token.trim();
            if token.is_empty() {
                return Err(refused(AtlassianConfigErrorCode::InvalidConfiguration));
            }
            return Ok(if token.contains("JSESSIONID") {
                Self::Cookie(Zeroizing::new(token.to_owned()))
            } else {
                Self::Bearer(Zeroizing::new(token.to_owned()))
            });
        }
        let username = optional_text(configuration, "username", MAX_IDENTITY_BYTES, provider)?
            .map(str::trim)
            .filter(|value| !value.is_empty());
        let api_key = optional_secret(configuration, "api_key", provider)?;
        match (username, api_key) {
            (Some(username), Some(api_key)) if !username.contains(':') => Ok(Self::Basic {
                username: username.into(),
                password: Zeroizing::new(api_key.to_owned()),
            }),
            _ => Err(refused(AtlassianConfigErrorCode::InvalidConfiguration)),
        }
    }

    /// The header this credential goes out as, marked sensitive; `None` when
    /// the value is not a header value (control bytes in a token).
    #[must_use]
    pub fn header(&self) -> Option<(HeaderName, HeaderValue)> {
        let (name, value) = match self {
            Self::Bearer(token) => (
                AUTHORIZATION,
                Zeroizing::new(format!("Bearer {}", token.as_str())),
            ),
            Self::Cookie(token) => (
                COOKIE,
                Zeroizing::new(
                    token
                        .split("; ")
                        .filter(|pair| pair.contains('='))
                        .collect::<Vec<_>>()
                        .join("; "),
                ),
            ),
            Self::Basic { username, password } => {
                let mut source = Zeroizing::new(String::with_capacity(
                    username
                        .len()
                        .saturating_add(password.len())
                        .saturating_add(1),
                ));
                source.push_str(username);
                source.push(':');
                source.push_str(password);
                (
                    AUTHORIZATION,
                    Zeroizing::new(format!("Basic {}", STANDARD.encode(source.as_bytes()))),
                )
            }
        };
        let mut value = HeaderValue::from_str(&value).ok()?;
        value.set_sensitive(true);
        Some((name, value))
    }
}

/// What both configurations read from the toolkit settings beyond their
/// own keys: the credential, the Cloud decision, the custom headers.
pub struct Common {
    pub credential: Credential,
    /// Atlassian Cloud (`*.atlassian.net`, or `hosting: cloud`, or the
    /// toolkit's `cloud: true`).
    pub cloud: bool,
    /// Whether the base URL's host is `atlassian.net` (the SDK's
    /// auto-detection, read apart from the final `cloud` decision).
    pub host_is_cloud: bool,
    pub custom_headers: Vec<(HeaderName, HeaderValue)>,
}

impl Common {
    /// The base URL (`<provider>_configuration.base_url`) under the shared
    /// rule, and the shared settings.
    ///
    /// # Errors
    ///
    /// See [`AtlassianConfigErrorCode`].
    pub fn parse(
        settings: &Map<String, Value>,
        configuration: &Map<String, Value>,
        provider: &'static str,
    ) -> Result<(url::Url, Self), AtlassianConfigError> {
        let refused = |code| AtlassianConfigError::new(provider, code);
        let raw_url = optional_text(
            configuration,
            "base_url",
            base_url::MAX_BASE_URL_BYTES,
            provider,
        )?
        .ok_or_else(|| refused(AtlassianConfigErrorCode::InvalidConfiguration))?;
        let base = base_url::parse(raw_url, BasePath::Prefix).map_err(|error| match error {
            BaseUrlError::PlainHttp => refused(AtlassianConfigErrorCode::PlainHttp),
            BaseUrlError::TooLong => refused(AtlassianConfigErrorCode::ResourceExhausted),
            BaseUrlError::Invalid => refused(AtlassianConfigErrorCode::InvalidConfiguration),
        })?;
        let host_is_cloud = host_matches_domain(&base, "atlassian.net");
        // The toolkit-level `cloud` wins; otherwise the credential's hosting,
        // auto-detected from the host (`_hosting_to_cloud`).
        let cloud = match settings.get("cloud") {
            None | Some(Value::Null) => {
                match optional_text(configuration, "hosting", MAX_IDENTITY_BYTES, provider)?
                    .map(|value| value.trim().to_ascii_lowercase())
                    .as_deref()
                {
                    Some("cloud") => true,
                    Some("server") => false,
                    _ => host_is_cloud,
                }
            }
            Some(Value::Bool(value)) => *value,
            Some(_) => return Err(refused(AtlassianConfigErrorCode::InvalidConfiguration)),
        };
        match settings.get("verify_ssl") {
            None | Some(Value::Null | Value::Bool(true)) => {}
            Some(Value::Bool(false)) => {
                return Err(refused(AtlassianConfigErrorCode::VerifySslDisabled));
            }
            Some(_) => return Err(refused(AtlassianConfigErrorCode::InvalidConfiguration)),
        }
        Ok((
            base,
            Self {
                credential: Credential::parse(configuration, provider)?,
                cloud,
                host_is_cloud,
                custom_headers: custom_headers(settings, provider)?,
            },
        ))
    }

    /// Put the custom headers, `Accept: application/json` and the credential
    /// on `request`.
    ///
    /// # Errors
    ///
    /// The credential is not a header value.
    pub fn authorize(&self, request: &mut Request) -> Result<(), AtlassianConfigError> {
        let headers = request.headers_mut();
        for (name, value) in &self.custom_headers {
            headers.insert(name.clone(), value.clone());
        }
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        let (name, value) = self.credential.header().ok_or_else(|| {
            AtlassianConfigError::new("Atlassian", AtlassianConfigErrorCode::InvalidConfiguration)
        })?;
        headers.insert(name, value);
        Ok(())
    }
}

fn custom_headers(
    settings: &Map<String, Value>,
    provider: &'static str,
) -> Result<Vec<(HeaderName, HeaderValue)>, AtlassianConfigError> {
    let refused = |code| AtlassianConfigError::new(provider, code);
    let headers = match settings.get("custom_headers") {
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::Object(headers)) => headers,
        Some(_) => return Err(refused(AtlassianConfigErrorCode::InvalidConfiguration)),
    };
    if headers.len() > MAX_CUSTOM_HEADERS {
        return Err(refused(AtlassianConfigErrorCode::ResourceExhausted));
    }
    let mut out = Vec::with_capacity(headers.len());
    for (name, value) in headers {
        let value = value
            .as_str()
            .ok_or_else(|| refused(AtlassianConfigErrorCode::InvalidConfiguration))?;
        if value.len() > MAX_HEADER_VALUE_BYTES {
            return Err(refused(AtlassianConfigErrorCode::ResourceExhausted));
        }
        let name = HeaderName::from_bytes(name.as_bytes())
            .map_err(|_| refused(AtlassianConfigErrorCode::InvalidConfiguration))?;
        if name.as_str().starts_with(RESERVED_PLATFORM_HEADER_PREFIX)
            || matches!(
                name.as_str(),
                "authorization"
                    | "proxy-authorization"
                    | "cookie"
                    | "host"
                    | "content-length"
                    | "transfer-encoding"
                    | "connection"
                    | "content-type"
                    | "accept"
            )
        {
            return Err(refused(AtlassianConfigErrorCode::ReservedHeader));
        }
        let mut value = HeaderValue::from_str(value)
            .map_err(|_| refused(AtlassianConfigErrorCode::InvalidConfiguration))?;
        value.set_sensitive(true);
        out.push((name, value));
    }
    Ok(out)
}

/// A string setting of at most `limit` bytes without control characters;
/// `None` when absent or null.
///
/// # Errors
///
/// Another type, a control character, or over the bound.
pub fn optional_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
    provider: &'static str,
) -> Result<Option<&'a str>, AtlassianConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            if value.len() > limit {
                return Err(AtlassianConfigError::new(
                    provider,
                    AtlassianConfigErrorCode::ResourceExhausted,
                ));
            }
            if value.chars().any(char::is_control) {
                return Err(AtlassianConfigError::new(
                    provider,
                    AtlassianConfigErrorCode::InvalidConfiguration,
                ));
            }
            Ok(Some(value))
        }
        Some(_) => Err(AtlassianConfigError::new(
            provider,
            AtlassianConfigErrorCode::InvalidConfiguration,
        )),
    }
}

fn optional_secret<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    provider: &'static str,
) -> Result<Option<&'a str>, AtlassianConfigError> {
    optional_text(object, name, MAX_SECRET_BYTES, provider)
        .map(|value| value.filter(|value| !value.is_empty()))
}

/// The SDK's `parse_list`: a comma/semicolon string or a string array;
/// blank items are dropped.
///
/// # Errors
///
/// Another type, or a non-string item.
pub fn text_list(
    object: &Map<String, Value>,
    name: &str,
    provider: &'static str,
) -> Result<Vec<String>, AtlassianConfigError> {
    let items: Vec<&str> = match object.get(name) {
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::String(value)) => value.split([',', ';']).collect(),
        Some(Value::Array(values)) => values
            .iter()
            .map(|value| {
                value.as_str().ok_or_else(|| {
                    AtlassianConfigError::new(
                        provider,
                        AtlassianConfigErrorCode::InvalidConfiguration,
                    )
                })
            })
            .collect::<Result<_, _>>()?,
        Some(_) => {
            return Err(AtlassianConfigError::new(
                provider,
                AtlassianConfigErrorCode::InvalidConfiguration,
            ));
        }
    };
    let mut out: Vec<String> = Vec::new();
    for item in items {
        let item = item.trim();
        if item.is_empty() || item.chars().any(char::is_control) {
            continue;
        }
        if !out.iter().any(|existing| existing == item) {
            out.push(item.to_owned());
        }
    }
    Ok(out)
}

/// An optional boolean `index_data` argument.
///
/// # Errors
///
/// Another type.
pub fn optional_flag(
    object: &Map<String, Value>,
    name: &str,
    provider: &'static str,
) -> Result<Option<bool>, AtlassianConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(flag)) => Ok(Some(*flag)),
        Some(_) => Err(AtlassianConfigError::new(
            provider,
            AtlassianConfigErrorCode::InvalidConfiguration,
        )),
    }
}

/// The SDK's attachment-name filter: a pattern matches the whole name,
/// ASCII case-insensitively, with `*` standing for any run and every other
/// character literal (`re.escape(pattern).replace(r'\*', '.*') + '$'`).
///
/// Linear in the pattern and the name (review round 1, finding 6): the
/// two-pointer walk backtracks only to the last `*`, so a pattern such as
/// `a*a*a*a*a*b` against a long run of `a` cannot go exponential.
#[must_use]
pub fn name_matches(pattern: &str, name: &str) -> bool {
    let pattern: Vec<char> = pattern.chars().flat_map(char::to_lowercase).collect();
    let name: Vec<char> = name.chars().flat_map(char::to_lowercase).collect();
    let (mut at_pattern, mut at_name) = (0_usize, 0_usize);
    // The pattern position after the last `*` and the name position it
    // currently covers up to.
    let mut star: Option<(usize, usize)> = None;
    while at_name < name.len() {
        match pattern.get(at_pattern) {
            Some('*') => {
                star = Some((at_pattern + 1, at_name));
                at_pattern += 1;
            }
            Some(head) if *head == name[at_name] => {
                at_pattern += 1;
                at_name += 1;
            }
            _ => match star {
                Some((after_star, covered)) => {
                    at_pattern = after_star;
                    at_name = covered + 1;
                    star = Some((after_star, covered + 1));
                }
                None => return false,
            },
        }
    }
    pattern[at_pattern..]
        .iter()
        .all(|character| *character == '*')
}

/// Whether an attachment named `name` passes the SDK's include/skip lists:
/// skipped when any skip pattern matches, else kept when the include list is
/// empty or any include pattern matches.
#[must_use]
pub fn attachment_passes(name: &str, include: &[String], skip: &[String]) -> bool {
    if skip.iter().any(|pattern| name_matches(pattern, name)) {
        return false;
    }
    include.is_empty() || include.iter().any(|pattern| name_matches(pattern, name))
}

/// The Atlassian Document Format (Jira Cloud descriptions and comments) as
/// readable text, the way the SDK's `_extract_image_data` renders it:
/// paragraphs and headings on their own lines (`#` per heading level), list
/// items as `- ` lines, table cells separated by ` | `, text marks dropped,
/// and a media node as the wiki-style `!id!` reference the SDK emits.
/// A string is returned as it is (Server and Data Center wiki markup).
#[must_use]
pub fn adf_text(value: &Value) -> String {
    let mut out = String::new();
    adf_into(value, &mut out);
    out
}

fn adf_into(value: &Value, out: &mut String) {
    match value {
        Value::String(text) => out.push_str(text),
        Value::Array(nodes) => nodes.iter().for_each(|node| adf_into(node, out)),
        Value::Object(node) => adf_node(node, out),
        _ => {}
    }
}

fn adf_children(node: &Map<String, Value>, out: &mut String) {
    if let Some(content) = node.get("content") {
        adf_into(content, out);
    }
}

/// `attrs.{name}` of an ADF node.
fn attr<'a>(node: &'a Map<String, Value>, name: &str) -> Option<&'a Value> {
    node.get("attrs").and_then(|attrs| attrs.get(name))
}

fn adf_node(node: &Map<String, Value>, out: &mut String) {
    let kind = node.get("type").and_then(Value::as_str).unwrap_or("");
    match kind {
        "text" => {
            let text = node.get("text").and_then(Value::as_str).unwrap_or("");
            out.push_str(text);
            if let Some(href) = node
                .get("marks")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .find(|mark| mark.get("type").and_then(Value::as_str) == Some("link"))
                .and_then(|mark| mark.pointer("/attrs/href"))
                .and_then(Value::as_str)
                && let Some(id) = attachment_id_in(href)
            {
                let _ = std::fmt::Write::write_fmt(out, format_args!(" !{id}!"));
            }
        }
        "hardBreak" => out.push('\n'),
        "paragraph" => {
            adf_children(node, out);
            out.push('\n');
        }
        "heading" => {
            let level = attr(node, "level")
                .and_then(Value::as_u64)
                .unwrap_or(1)
                .clamp(1, 6);
            for _ in 0..level {
                out.push('#');
            }
            out.push(' ');
            adf_children(node, out);
            out.push('\n');
        }
        "listItem" => {
            out.push_str("- ");
            adf_children(node, out);
        }
        "tableRow" => {
            let mut cells = Vec::new();
            for cell in node
                .get("content")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                let mut text = String::new();
                adf_into(cell, &mut text);
                cells.push(text.trim().to_owned());
            }
            out.push_str(&cells.join(" | "));
            out.push('\n');
        }
        "media" | "mediaInline" => {
            let alt = attr(node, "alt").and_then(Value::as_str);
            let id = attr(node, "id").and_then(Value::as_str);
            match (alt.filter(|alt| !alt.is_empty()), id) {
                (Some(alt), _) => {
                    let _ = std::fmt::Write::write_fmt(out, format_args!("!{alt}|alt=\"{alt}\"!"));
                }
                (None, Some(id)) => {
                    let _ = std::fmt::Write::write_fmt(out, format_args!("!{id}!"));
                }
                (None, None) => {}
            }
        }
        "mention" => {
            let text = attr(node, "text").and_then(Value::as_str).unwrap_or("");
            out.push_str(text);
        }
        "emoji" => {
            let text = attr(node, "text")
                .or_else(|| attr(node, "shortName"))
                .and_then(Value::as_str)
                .unwrap_or("");
            out.push_str(text);
        }
        "inlineCard" | "blockCard" | "embedCard" => {
            let url = attr(node, "url").and_then(Value::as_str).unwrap_or("");
            out.push_str(url);
            if kind != "inlineCard" {
                out.push('\n');
            }
        }
        "codeBlock" => {
            out.push_str("```\n");
            adf_children(node, out);
            out.push_str("\n```\n");
        }
        "rule" => out.push_str("---\n"),
        // Containers whose text is their children's: doc, bulletList,
        // orderedList, blockquote, panel, table, tableCell, tableHeader,
        // mediaSingle, mediaGroup, expand, nestedExpand, layoutSection, ….
        _ => adf_children(node, out),
    }
}

/// The attachment id an ADF link points at: `/rest/api/3/attachment/content/{id}`
/// (Cloud) or `/secure/attachment/{id}/...` (Server and Data Center).
fn attachment_id_in(href: &str) -> Option<&str> {
    let after = href
        .split_once("/attachment/content/")
        .or_else(|| href.split_once("/secure/attachment/"))
        .map(|(_, rest)| rest)?;
    let id: &str = after.split(['/', '?', '#']).next().unwrap_or("");
    (!id.is_empty() && id.bytes().all(|byte| byte.is_ascii_digit())).then_some(id)
}

/// `_strip_base64_images`: an inline `data:image/{png,jpeg,gif};base64,…`
/// becomes `[Image Removed]`, so a pasted screenshot does not fill a chunk
/// with base64.
#[must_use]
pub fn strip_base64_images(content: &str) -> String {
    const MARKER: &str = "data:image/";
    let mut out = String::with_capacity(content.len());
    let mut rest = content;
    while let Some(position) = rest.find(MARKER) {
        out.push_str(&rest[..position]);
        let candidate = &rest[position + MARKER.len()..];
        let matched = ["png", "jpeg", "gif"].iter().find_map(|kind| {
            candidate
                .strip_prefix(kind)
                .and_then(|tail| tail.strip_prefix(";base64,"))
        });
        if let Some(tail) = matched {
            let data = tail
                .bytes()
                .take_while(|byte| {
                    byte.is_ascii_alphanumeric() || matches!(byte, b'+' | b'/' | b'=')
                })
                .count();
            if data == 0 {
                out.push_str(MARKER);
                rest = candidate;
            } else {
                out.push_str("[Image Removed]");
                rest = &tail[data..];
            }
        } else {
            out.push_str(MARKER);
            rest = candidate;
        }
    }
    out.push_str(rest);
    out
}

/// A JSON object's string field, trimmed, when present and not empty.
#[must_use]
pub fn text_of<'a>(value: &'a Value, pointer: &str) -> Option<&'a str> {
    value
        .pointer(pointer)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|text| !text.is_empty())
}

/// An id field that Atlassian spells as a string or a number.
#[must_use]
pub fn id_of(value: &Value, pointer: &str) -> Option<String> {
    match value.pointer(pointer)? {
        Value::String(text) if !text.trim().is_empty() => Some(text.trim().to_owned()),
        Value::Number(number) => Some(number.to_string()),
        _ => None,
    }
}

/// A size field spelled as a number or a numeric string; `0` otherwise.
#[must_use]
pub fn size_of(value: &Value, pointer: &str) -> u64 {
    match value.pointer(pointer) {
        Some(Value::Number(number)) => number.as_u64().unwrap_or(0),
        Some(Value::String(text)) => text.trim().parse().unwrap_or(0),
        _ => 0,
    }
}

/// Whether an identifier is safe to place in a URL path: digits, letters,
/// `-`, `_` and `.`, bounded.
#[must_use]
pub fn safe_identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 255
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
}

/// A file name or title as one key segment: no `/` or `\\`, no control
/// characters, never empty (`attachment` when nothing is left).
#[must_use]
pub fn key_segment(name: &str) -> String {
    let segment: String = name
        .trim()
        .chars()
        .map(|character| {
            if character == '/' || character == '\\' || character.is_control() {
                '_'
            } else {
                character
            }
        })
        .collect();
    if segment.is_empty() {
        "attachment".to_owned()
    } else {
        segment
    }
}

/// The listing scaffolding the two `ContentSource` connectors share.
#[cfg(feature = "content-source")]
mod listing {
    use std::collections::BTreeMap;
    use std::future::Future;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::{Arc, Mutex, PoisonError};

    use elitea_content_source::SourceError;
    use serde_json::{Map, Value};

    use crate::egress::HostAllowlist;
    use crate::source::{Failure, Listed, Listing, SourceHttp};
    use crate::transport::header::{CONTENT_TYPE, LOCATION};
    use crate::transport::{Method, Request, StatusCode, Url};

    /// How many per-issue or per-page dependent reads run at once: a Jira
    /// issue's comment overflow, a Confluence page's attachments, comments
    /// and ancestor restrictions (review rounds 1 and 2).
    pub const COMMENT_LISTING_CONCURRENCY: usize = 8;

    /// The redirects one attachment download follows (Cloud answers with
    /// one, to its media host; a Server may add a hop or two).
    pub const MAX_DOWNLOAD_REDIRECTS: usize = 5;

    /// The host Atlassian Cloud redirects attachment downloads to.
    const ATLASSIAN_MEDIA_HOST: &str = "api.media.atlassian.com";

    /// What a listing learnt about each document beyond its reference: the
    /// metadata its fetch reports and whatever the fetch needs (a download
    /// path, a comment body already listed).
    pub type Details = BTreeMap<String, Map<String, Value>>;

    /// A listing page read with the credential; a 401 or 403 is an error
    /// naming the credential, never folded into "HTTP 401" and never an
    /// empty listing.
    ///
    /// # Errors
    ///
    /// See [`SourceHttp::json`].
    pub async fn authorized_json(
        http: &SourceHttp,
        request: Request,
        provider: &str,
    ) -> Result<Value, Failure> {
        http.json(request)
            .await
            .map(|(_, value)| value)
            .map_err(|failure| refused_credential(failure, provider))
    }

    /// A 401 or 403 as an error naming the credential (never "HTTP 401"
    /// and never an empty result); any other failure unchanged. The one
    /// mapping listing pages and downloads share.
    #[must_use]
    pub fn refused_credential(failure: Failure, provider: &str) -> Failure {
        match failure {
            Failure::Status(status @ (StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN)) => {
                Failure::Client(format!(
                    "{provider} refused the credential (HTTP {}); check the toolkit's token or API key",
                    status.as_u16()
                ))
            }
            other => other,
        }
    }

    /// Whether two URLs share an origin: scheme, host and (default) port.
    #[must_use]
    pub fn same_origin(left: &Url, right: &Url) -> bool {
        left.scheme() == right.scheme()
            && left.host_str() == right.host_str()
            && left.port_or_known_default() == right.port_or_known_default()
    }

    /// One listing generation: the listed documents and what the same
    /// listing learnt about them. Swapped in as a whole, so a fetch never
    /// pairs one listing's documents with another's details.
    pub struct Snapshot {
        pub listing: Listing,
        pub details: Details,
    }

    impl Snapshot {
        /// The details this listing recorded for `key` (empty when none).
        #[must_use]
        pub fn details_of(&self, key: &str) -> Map<String, Value> {
            self.details.get(key).cloned().unwrap_or_default()
        }
    }

    /// The remembered listing with its per-document details, in one cell
    /// (review round 2, finding 7), so a fetch reads the version it was
    /// listed with and a fetch before any listing lists once. The sharing
    /// rules are [`crate::source::ListingCache`]'s: callers that arrive while
    /// a listing runs wait for it, and a failed listing is not remembered.
    #[derive(Default)]
    pub struct DetailedListing {
        current: Mutex<Option<Arc<Snapshot>>>,
        /// Held while a listing runs, so concurrent callers share it.
        flight: tokio::sync::Mutex<()>,
        /// Bumped each time a listing is remembered.
        generation: AtomicU64,
    }

    impl DetailedListing {
        fn current(&self) -> Option<Arc<Snapshot>> {
            self.current
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .clone()
        }

        /// The remembered listing, or `list` run once.
        ///
        /// # Errors
        ///
        /// Whatever `list` fails with, as the provider's source error.
        pub async fn get_or_list<F, Fut>(
            &self,
            provider: &str,
            list: F,
        ) -> Result<Arc<Snapshot>, SourceError>
        where
            F: FnOnce() -> Fut,
            Fut: Future<Output = Result<(Vec<Listed>, Details), Failure>>,
        {
            if let Some(snapshot) = self.current() {
                return Ok(snapshot);
            }
            let _flight = self.flight.lock().await;
            if let Some(snapshot) = self.current() {
                return Ok(snapshot);
            }
            self.remember(provider, list).await
        }

        /// A fresh listing (an explicit `list()`), shared with callers that
        /// arrive while it runs.
        ///
        /// # Errors
        ///
        /// Whatever `list` fails with, as the provider's source error.
        pub async fn list_shared<F, Fut>(
            &self,
            provider: &str,
            list: F,
        ) -> Result<Arc<Snapshot>, SourceError>
        where
            F: FnOnce() -> Fut,
            Fut: Future<Output = Result<(Vec<Listed>, Details), Failure>>,
        {
            let seen = self.generation.load(Ordering::SeqCst);
            let _flight = self.flight.lock().await;
            if self.generation.load(Ordering::SeqCst) != seen
                && let Some(snapshot) = self.current()
            {
                return Ok(snapshot);
            }
            self.remember(provider, list).await
        }

        async fn remember<F, Fut>(
            &self,
            provider: &str,
            list: F,
        ) -> Result<Arc<Snapshot>, SourceError>
        where
            F: FnOnce() -> Fut,
            Fut: Future<Output = Result<(Vec<Listed>, Details), Failure>>,
        {
            let (listed, details) = list()
                .await
                .map_err(|failure| failure.into_source_error(provider, None))?;
            let snapshot = Arc::new(Snapshot {
                listing: listed
                    .into_iter()
                    .map(|listed| (listed.reference.key.clone(), listed))
                    .collect(),
                details,
            });
            *self.current.lock().unwrap_or_else(PoisonError::into_inner) =
                Some(Arc::clone(&snapshot));
            self.generation.fetch_add(1, Ordering::SeqCst);
            Ok(snapshot)
        }
    }

    /// The one attachment download both connectors use (review round 2,
    /// findings 1 and 4).
    ///
    /// The credential goes to the configured origin only. A redirect is
    /// followed (up to [`MAX_DOWNLOAD_REDIRECTS`]) when its target is
    /// `https` and is either
    ///
    /// * on the configured origin, at a download path (`/download/…`,
    ///   `/attachment/…`, `/attachments/…`; never a login or SSO page), still
    ///   with the credential; or
    /// * on an Atlassian media host: `api.media.atlassian.com`, or a host the
    ///   egress allowlist names (the `*` opt-out does not widen this), requested
    ///   WITHOUT the credential or any custom header. Once a download has
    ///   left the origin it never returns to it.
    ///
    /// A 200 that is an HTML page for an attachment that is not HTML (an
    /// SSO or error page served with a success status) is refused.
    pub struct AttachmentDownloader {
        origin: Url,
        media_hosts: HostAllowlist,
    }

    impl AttachmentDownloader {
        /// A downloader for the credential origin `origin`; `allowlist` is
        /// the deployment's egress allowlist.
        #[must_use]
        pub fn new(origin: &Url, allowlist: &HostAllowlist) -> Self {
            let named: Vec<&str> = allowlist
                .entries()
                .iter()
                .map(String::as_str)
                .filter(|entry| *entry != "*")
                .collect();
            Self {
                origin: origin.clone(),
                media_hosts: HostAllowlist::parse(Some(&named.join(" "))),
            }
        }

        fn is_media_host(&self, target: &Url) -> bool {
            target
                .host_str()
                .is_some_and(|host| host.eq_ignore_ascii_case(ATLASSIAN_MEDIA_HOST))
                || self.media_hosts.permits_url(target)
        }

        /// Whether `target` may be requested after a redirect.
        fn check_redirect(&self, target: &Url, left_origin: bool) -> Result<(), Failure> {
            let refuse = |message: &str| Err(Failure::Client(message.to_owned()));
            if target.scheme() != "https" {
                return refuse(
                    "the attachment redirect points at a plain-HTTP location, which this runtime does not follow",
                );
            }
            if same_origin(&self.origin, target) {
                if left_origin {
                    return refuse(
                        "the attachment download came back to the configured origin after leaving it, which this runtime does not follow",
                    );
                }
                let path = target.path().to_ascii_lowercase();
                if path.contains("/download/")
                    || path.contains("/attachment/")
                    || path.contains("/attachments/")
                {
                    return Ok(());
                }
                if [
                    "login",
                    "logout",
                    "signin",
                    "sign-in",
                    "sso",
                    "saml",
                    "authenticate",
                ]
                .iter()
                .any(|word| path.contains(word))
                {
                    return refuse(
                        "the attachment download was redirected to a login page, so it was refused (check the credential)",
                    );
                }
                return refuse(
                    "the attachment redirect leaves the download path on the configured origin, so it was refused",
                );
            }
            if self.is_media_host(target) {
                return Ok(());
            }
            refuse(
                "the attachment redirect points at a host that is not an allowed Atlassian media host, so it was refused",
            )
        }

        /// The bytes of the attachment at `url` (on the configured origin).
        /// `authorize` builds a request to a URL on that origin with the
        /// credential; `name` and `media_type` say whether an HTML answer
        /// is the file itself.
        ///
        /// # Errors
        ///
        /// A link off the origin, a refused or looping redirect, a non-2xx
        /// status, an HTML page where a file was listed, a body over the
        /// document cap; see [`SourceHttp::send`].
        pub async fn download<F>(
            &self,
            http: &SourceHttp,
            url: Url,
            authorize: F,
            name: &str,
            media_type: Option<&str>,
        ) -> Result<Vec<u8>, Failure>
        where
            F: Fn(Url) -> Result<Request, Failure>,
        {
            if !same_origin(&self.origin, &url) {
                return Err(Failure::Client(
                    "the attachment's link is not on the configured origin, so the download was refused (the credential is sent only there)"
                        .to_owned(),
                ));
            }
            let mut url = url;
            let mut credentialed = true;
            let mut left_origin = false;
            let mut hops = 0_usize;
            loop {
                let request = if credentialed {
                    authorize(url.clone())?
                } else {
                    Request::new(Method::GET, url.clone())
                };
                let mut response = http.send(request).await?;
                let status = response.status();
                if matches!(
                    status,
                    StatusCode::MOVED_PERMANENTLY
                        | StatusCode::FOUND
                        | StatusCode::SEE_OTHER
                        | StatusCode::TEMPORARY_REDIRECT
                        | StatusCode::PERMANENT_REDIRECT
                ) {
                    hops += 1;
                    if hops > MAX_DOWNLOAD_REDIRECTS {
                        return Err(Failure::Client(
                            "the attachment download redirected too many times".to_owned(),
                        ));
                    }
                    let location = response
                        .headers()
                        .get(LOCATION)
                        .and_then(|value| value.to_str().ok())
                        .ok_or(Failure::InvalidResponse("a redirect carries no Location"))?;
                    let target = url.join(location).map_err(|_| {
                        Failure::InvalidResponse("a redirect Location is not a URL")
                    })?;
                    self.check_redirect(&target, left_origin)?;
                    credentialed = same_origin(&self.origin, &target);
                    left_origin |= !credentialed;
                    url = target;
                    continue;
                }
                if !status.is_success() {
                    return Err(Failure::Status(status));
                }
                if answers_with_html(&response) && !html_expected(name, media_type) {
                    return Err(Failure::InvalidResponse(
                        "the attachment download answered an HTML page, not the file (a login or error page)",
                    ));
                }
                return SourceHttp::body(&mut response, http.limits().max_document_bytes).await;
            }
        }
    }

    fn answers_with_html(response: &crate::transport::Response) -> bool {
        response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .is_some_and(|value| {
                let kind = value.split(';').next().unwrap_or("").trim();
                kind.eq_ignore_ascii_case("text/html")
                    || kind.eq_ignore_ascii_case("application/xhtml+xml")
            })
    }

    /// Whether an attachment listed as `name` / `media_type` is itself an
    /// HTML document.
    fn html_expected(name: &str, media_type: Option<&str>) -> bool {
        let extension = name.rsplit_once('.').map(|(_, extension)| extension);
        extension.is_some_and(|extension| {
            ["html", "htm", "xhtml"]
                .iter()
                .any(|html| extension.eq_ignore_ascii_case(html))
        }) || media_type.is_some_and(|media| {
            let kind = media.split(';').next().unwrap_or("").trim();
            kind.eq_ignore_ascii_case("text/html")
                || kind.eq_ignore_ascii_case("application/xhtml+xml")
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn credentials_follow_the_sdk() {
        let bearer = Credential::parse(json!({"token": "abc"}).as_object().unwrap(), "Confluence")
            .expect("bearer");
        let (name, value) = bearer.header().unwrap();
        assert_eq!(name, AUTHORIZATION);
        assert_eq!(value, "Bearer abc");
        assert!(value.is_sensitive());

        let cookie = Credential::parse(
            json!({"token": "JSESSIONID=1; other=2; junk"})
                .as_object()
                .unwrap(),
            "Jira",
        )
        .expect("cookie");
        let (name, value) = cookie.header().unwrap();
        assert_eq!(name, COOKIE);
        assert_eq!(value, "JSESSIONID=1; other=2");

        let basic = Credential::parse(
            json!({"username": "me@x", "api_key": "k"})
                .as_object()
                .unwrap(),
            "Jira",
        )
        .expect("basic");
        let (_, value) = basic.header().unwrap();
        assert_eq!(value, "Basic bWVAeDpr");

        for bad in [
            json!({}),
            json!({"username": "me"}),
            json!({"api_key": "k"}),
            json!({"username": "a:b", "api_key": "k"}),
            json!({"token": "   "}),
        ] {
            assert!(
                Credential::parse(bad.as_object().unwrap(), "Jira").is_err(),
                "{bad}"
            );
        }
    }

    #[test]
    fn the_cloud_decision_and_the_refusals() {
        let settings = |extra: Value, configuration: Value| {
            let mut settings = extra.as_object().cloned().unwrap_or_default();
            settings.insert("jira_configuration".into(), configuration);
            settings
        };
        let (base, common) = Common::parse(
            &settings(
                json!({}),
                json!({"base_url": "https://acme.atlassian.net/", "token": "t"}),
            ),
            settings(
                json!({}),
                json!({"base_url": "https://acme.atlassian.net/", "token": "t"}),
            )["jira_configuration"]
                .as_object()
                .unwrap(),
            "Jira",
        )
        .expect("cloud");
        assert_eq!(base.as_str(), "https://acme.atlassian.net/");
        assert!(common.cloud && common.host_is_cloud);

        let server = json!({"base_url": "https://jira.corp.example/jira", "token": "t", "hosting": "server"});
        let (base, common) = Common::parse(
            &settings(json!({}), server.clone()),
            server.as_object().unwrap(),
            "Jira",
        )
        .expect("server");
        assert_eq!(base.as_str(), "https://jira.corp.example/jira");
        assert!(!common.cloud);
        let forced = json!({"base_url": "https://jira.corp.example", "token": "t"});
        let (_, common) = Common::parse(
            &settings(json!({"cloud": true}), forced.clone()),
            forced.as_object().unwrap(),
            "Jira",
        )
        .expect("forced");
        assert!(common.cloud && !common.host_is_cloud);

        let plain = json!({"base_url": "http://jira.corp.example", "token": "t"});
        assert_eq!(
            Common::parse(
                &settings(json!({}), plain.clone()),
                plain.as_object().unwrap(),
                "Jira"
            )
            .err()
            .map(|e| e.code()),
            Some(AtlassianConfigErrorCode::PlainHttp)
        );
        let ok = json!({"base_url": "https://jira.corp.example", "token": "t"});
        assert_eq!(
            Common::parse(
                &settings(json!({"verify_ssl": false}), ok.clone()),
                ok.as_object().unwrap(),
                "Jira"
            )
            .err()
            .map(|e| e.code()),
            Some(AtlassianConfigErrorCode::VerifySslDisabled)
        );
        assert_eq!(
            Common::parse(
                &settings(
                    json!({"custom_headers": {"Authorization": "x"}}),
                    ok.clone()
                ),
                ok.as_object().unwrap(),
                "Jira"
            )
            .err()
            .map(|e| e.code()),
            Some(AtlassianConfigErrorCode::ReservedHeader)
        );
        assert_eq!(
            Common::parse(
                &settings(
                    json!({"custom_headers": {"X-Elitea-Project": "x"}}),
                    ok.clone()
                ),
                ok.as_object().unwrap(),
                "Jira"
            )
            .err()
            .map(|e| e.code()),
            Some(AtlassianConfigErrorCode::ReservedHeader)
        );
        let (_, common) = Common::parse(
            &settings(
                json!({"custom_headers": {"X-Proxy-Tenant": "acme"}}),
                ok.clone(),
            ),
            ok.as_object().unwrap(),
            "Jira",
        )
        .expect("custom header");
        assert_eq!(common.custom_headers.len(), 1);
        assert!(common.custom_headers[0].1.is_sensitive());
    }

    #[test]
    fn attachment_name_filters_follow_the_sdk() {
        assert!(name_matches("*.PDF", "Spec.pdf"));
        assert!(!name_matches("*.pdf", "Spec.pdf.bak"));
        assert!(name_matches("*", "anything"));
        assert!(name_matches("a*b*c", "aXXbYYc"));
        assert!(!name_matches("a?c", "abc"), "`?` is literal");
        let include = vec!["*.md".to_owned(), "*.pdf".to_owned()];
        let skip = vec!["draft*".to_owned()];
        assert!(attachment_passes("Guide.pdf", &include, &skip));
        assert!(!attachment_passes("draft-guide.pdf", &include, &skip));
        assert!(!attachment_passes("photo.png", &include, &skip));
        assert!(attachment_passes("photo.png", &[], &skip));
        assert!(name_matches("*a", "a"));
        assert!(name_matches("**", ""));
        assert!(!name_matches("a*", ""));
        assert!(name_matches("a*c*", "abcabc"));
        assert!(!name_matches("a*c*d", "abcabc"));
    }

    /// Review round 1, finding 6: the backtracking matcher took exponential
    /// time on this pattern; the two-pointer walk is linear.
    #[test]
    fn a_pathological_pattern_is_matched_in_linear_time() {
        let pattern = "a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*b";
        let name = "a".repeat(4_000);
        let started = std::time::Instant::now();
        assert!(!name_matches(pattern, &name));
        assert!(name_matches(pattern, &format!("{name}b")));
        assert!(
            started.elapsed() < std::time::Duration::from_secs(1),
            "{:?}",
            started.elapsed()
        );
    }

    #[test]
    fn key_segments_carry_no_separators() {
        assert_eq!(key_segment("notes/with slash.txt"), "notes_with slash.txt");
        assert_eq!(key_segment("a\\b\tc"), "a_b_c");
        assert_eq!(key_segment("  "), "attachment");
    }

    #[cfg(feature = "content-source")]
    #[test]
    fn origins_compare_scheme_host_and_port() {
        let base = url::Url::parse("https://acme.atlassian.net/").unwrap();
        assert!(same_origin(
            &base,
            &url::Url::parse("https://acme.atlassian.net:443/rest/x").unwrap()
        ));
        assert!(!same_origin(
            &base,
            &url::Url::parse("http://acme.atlassian.net/rest/x").unwrap()
        ));
        assert!(!same_origin(
            &base,
            &url::Url::parse("https://evil.example/rest/x").unwrap()
        ));
        assert!(!same_origin(
            &base,
            &url::Url::parse("https://acme.atlassian.net:8443/").unwrap()
        ));
    }

    /// Review round 2, finding 7: the details live in the same generation as
    /// the listing, so a fetch that holds listing A never reads listing B's
    /// details, however a relist interleaves.
    #[cfg(feature = "content-source")]
    #[tokio::test]
    async fn a_relist_never_pairs_one_listing_with_anothers_details() {
        use crate::source::{Failure, Listed};
        use std::sync::Arc;

        fn generation(version: &str, note: &str) -> (Vec<Listed>, Details) {
            let mut details = Details::new();
            details.insert(
                "doc".into(),
                json!({"note": note}).as_object().cloned().unwrap(),
            );
            (
                vec![Listed::new("doc".into(), version.into(), 0, "h".into())],
                details,
            )
        }
        let listing = Arc::new(DetailedListing::default());
        let first = listing
            .get_or_list("Test", || async { Ok::<_, Failure>(generation("1", "A")) })
            .await
            .expect("first listing");

        // A fetch holds the first snapshot across an await while a relist
        // replaces the remembered one.
        let held = Arc::clone(&first);
        let gate = Arc::new(tokio::sync::Notify::new());
        let reader = tokio::spawn({
            let gate = Arc::clone(&gate);
            async move {
                gate.notified().await;
                (
                    held.listing["doc"].reference.version.clone(),
                    held.details_of("doc")["note"].clone(),
                )
            }
        });
        let second = listing
            .list_shared("Test", || async { Ok::<_, Failure>(generation("2", "B")) })
            .await
            .expect("relist");
        gate.notify_one();
        let (version, note) = reader.await.expect("reader");
        assert_eq!(version, "1");
        assert_eq!(note, "A");
        assert_eq!(second.listing["doc"].reference.version, "2");
        assert_eq!(second.details_of("doc")["note"], "B");
        // The remembered pair is the new one, whole.
        let now = listing
            .get_or_list("Test", || async { Ok::<_, Failure>(generation("3", "C")) })
            .await
            .expect("remembered");
        assert_eq!(now.listing["doc"].reference.version, "2");
        assert_eq!(now.details_of("doc")["note"], "B");
    }

    #[test]
    fn adf_becomes_readable_text() {
        let adf = json!({
            "type": "doc", "version": 1, "content": [
                {"type": "heading", "attrs": {"level": 2}, "content": [{"type": "text", "text": "Goal"}]},
                {"type": "paragraph", "content": [
                    {"type": "text", "text": "Ship ", "marks": [{"type": "strong"}]},
                    {"type": "text", "text": "it"},
                    {"type": "hardBreak"},
                    {"type": "text", "text": "spec", "marks": [{"type": "link", "attrs": {"href": "https://x.atlassian.net/rest/api/3/attachment/content/10042"}}]},
                ]},
                {"type": "bulletList", "content": [
                    {"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "one"}]}]},
                    {"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "two"}]}]},
                ]},
                {"type": "table", "content": [
                    {"type": "tableRow", "content": [
                        {"type": "tableHeader", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "a"}]}]},
                        {"type": "tableHeader", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "b"}]}]},
                    ]},
                ]},
                {"type": "mediaSingle", "content": [{"type": "media", "attrs": {"id": "m-1", "alt": "diagram.png"}}]},
                {"type": "mediaSingle", "content": [{"type": "media", "attrs": {"id": "m-2"}}]},
            ]
        });
        assert_eq!(
            adf_text(&adf),
            "## Goal\nShip it\nspec !10042!\n- one\n- two\na | b\n!diagram.png|alt=\"diagram.png\"!!m-2!"
        );
        assert_eq!(adf_text(&json!("h1. Wiki *markup*")), "h1. Wiki *markup*");
        assert_eq!(adf_text(&Value::Null), "");
    }

    #[test]
    fn base64_images_are_removed() {
        assert_eq!(
            strip_base64_images("a <img src=\"data:image/png;base64,iVBORw0KGgo=\"> b"),
            "a <img src=\"[Image Removed]\"> b"
        );
        assert_eq!(
            strip_base64_images("data:image/svg+xml,x"),
            "data:image/svg+xml,x"
        );
    }

    #[test]
    fn identifiers_and_fields() {
        assert!(safe_identifier("12345"));
        assert!(safe_identifier("PROJ-1"));
        assert!(!safe_identifier("a/b"));
        assert!(!safe_identifier(""));
        let value = json!({"id": 7, "size": "12", "t": " x "});
        assert_eq!(id_of(&value, "/id").as_deref(), Some("7"));
        assert_eq!(size_of(&value, "/size"), 12);
        assert_eq!(text_of(&value, "/t"), Some("x"));
        assert_eq!(text_of(&value, "/missing"), None);
    }
}
