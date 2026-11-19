//! The claim-scoped authority of one `delta_lake` toolkit: the AWS key pair
//! (and optional session token) of `delta_lake_configuration`, its region and
//! the one table it names.

use std::collections::HashSet;
use std::fmt;

use percent_encoding::{AsciiSet, NON_ALPHANUMERIC, utf8_percent_encode};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_SECRET_BYTES: usize = 4_096;
const MAX_REGION_BYTES: usize = 25;
const MAX_PATH_BYTES: usize = 1_024;
/// A configured path is shown in a tool description cut to this many bytes, so
/// the description never loses the usage text behind it.
const MAX_DESCRIBED_PATH_BYTES: usize = 90;
/// The URL path segment alphabet: everything but the unreserved characters is
/// percent-encoded, so a prefix is the literal key the user wrote (a `%` in a
/// key is `%25`, which the object store decodes back to `%`).
const SEGMENT: &AsciiSet = &NON_ALPHANUMERIC
    .remove(b'-')
    .remove(b'.')
    .remove(b'_')
    .remove(b'~');
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DeltaLakeConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries a key, a token or a path.
pub(crate) struct DeltaLakeConfigError {
    code: DeltaLakeConfigErrorCode,
}

impl DeltaLakeConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> DeltaLakeConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for DeltaLakeConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DeltaLakeConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for DeltaLakeConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            DeltaLakeConfigErrorCode::InvalidConfiguration => {
                "the Delta Lake toolkit configuration is invalid"
            }
            DeltaLakeConfigErrorCode::ResourceExhausted => {
                "the Delta Lake toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for DeltaLakeConfigError {}

/// An S3 table location: the bucket and the key prefix of the table root
/// (no leading or trailing slash; empty for a table at the bucket root).
#[derive(Clone, Debug, Eq, PartialEq)]
pub(in crate::toolkits) struct TableLocation {
    bucket: Box<str>,
    prefix: Box<str>,
}

impl TableLocation {
    /// Read `s3://bucket/prefix` (also `s3a://` and `s3n://`, the Hadoop
    /// spellings Delta tables are commonly written with). A local path or any
    /// other scheme is refused: the worker reads no filesystem.
    pub(in crate::toolkits) fn parse(raw: &str) -> Result<Self, DeltaLakeConfigError> {
        let raw = raw.trim();
        if raw.len() > MAX_PATH_BYTES {
            return Err(resource_exhausted());
        }
        let lower = raw.to_ascii_lowercase();
        let rest = ["s3://", "s3a://", "s3n://"]
            .iter()
            .find(|scheme| lower.starts_with(**scheme))
            .map(|scheme| &raw[scheme.len()..])
            .ok_or_else(invalid_configuration)?;
        if rest
            .bytes()
            .any(|byte| byte.is_ascii_control() || matches!(byte, b'?' | b'#' | b'\\' | b' '))
        {
            return Err(invalid_configuration());
        }
        let (bucket, prefix) = rest.split_once('/').unwrap_or((rest, ""));
        if !valid_bucket(bucket) {
            return Err(invalid_configuration());
        }
        let mut segments = Vec::new();
        for segment in prefix.split('/') {
            match segment {
                "" => {}
                "." | ".." => return Err(invalid_configuration()),
                other => segments.push(other),
            }
        }
        Ok(Self {
            bucket: bucket.into(),
            prefix: segments.join("/").into(),
        })
    }

    pub(in crate::toolkits) fn bucket(&self) -> &str {
        &self.bucket
    }

    /// The table root as the URL the Delta reader opens, with the trailing
    /// slash it needs to join the log path.
    pub(in crate::toolkits) fn url(&self) -> String {
        if self.prefix.is_empty() {
            format!("s3://{}/", self.bucket)
        } else {
            let encoded = self
                .prefix
                .split('/')
                .map(|segment| utf8_percent_encode(segment, SEGMENT).to_string())
                .collect::<Vec<_>>()
                .join("/");
            format!("s3://{}/{encoded}/", self.bucket)
        }
    }
}

/// S3 bucket naming rules (3 to 63 characters of lowercase letters, digits,
/// dots and hyphens, starting and ending alphanumeric, no `..`, not an IPv4
/// address). The bucket becomes a path segment of a fixed regional host, so
/// this is a correctness check, not the host's allowlist.
fn valid_bucket(bucket: &str) -> bool {
    let bytes = bucket.as_bytes();
    (3..=63).contains(&bytes.len())
        && bytes.iter().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'.' | b'-')
        })
        && bytes.first().is_some_and(u8::is_ascii_alphanumeric)
        && bytes.last().is_some_and(u8::is_ascii_alphanumeric)
        && !bucket.contains("..")
        && bucket.parse::<std::net::Ipv4Addr>().is_err()
}

/// An AWS region id such as `us-east-1`, `us-gov-west-1` or `ap-southeast-12`:
/// a two-letter area, one or more lowercase words, and a one or two digit
/// number. Mirrored by elitea-main's egress derivation, and pinned against it
/// by `endpoint_cases.json`.
fn valid_region(region: &str) -> bool {
    let parts: Vec<&str> = region.split('-').collect();
    let lowercase =
        |part: &str| !part.is_empty() && part.bytes().all(|byte| byte.is_ascii_lowercase());
    let Some((number, words)) = parts.split_last() else {
        return false;
    };
    region.len() <= MAX_REGION_BYTES
        && parts.len() >= 3
        && parts[0].len() == 2
        && words.iter().all(|word| lowercase(word))
        && (1..=2).contains(&number.len())
        && number.bytes().all(|byte| byte.is_ascii_digit())
}

/// The S3 endpoint of a region, the only host the family calls. China
/// regions live under `amazonaws.com.cn`. Mirrored by elitea-main's egress
/// derivation (`workeregress.deltaLakeHosts`).
pub(in crate::toolkits) fn s3_endpoint(region: &str) -> String {
    let domain = if region.starts_with("cn-") {
        "amazonaws.com.cn"
    } else {
        "amazonaws.com"
    };
    format!("https://s3.{region}.{domain}")
}

/// Claim-scoped Delta Lake authority. Deliberately non-cloneable and
/// non-debuggable: it holds the AWS secret key.
pub(crate) struct DeltaLakeToolkitConfig {
    access_key_id: Zeroizing<String>,
    secret_access_key: Zeroizing<String>,
    session_token: Option<Zeroizing<String>>,
    region: Box<str>,
    location: TableLocation,
    /// The configured `s3_path` and `table_path` strings, for the tool
    /// descriptions (the SDK prefixes each with both).
    s3_path: Option<Box<str>>,
    table_path: Option<Box<str>>,
    selected_tools: Vec<Box<str>>,
}

impl DeltaLakeToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, DeltaLakeConfigError> {
        let configuration = settings
            .get("delta_lake_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        // The SDK's `validate_auth`: "You must provide AWS credentials and
        // region." and "You must provide either s3_path or table_path."
        let access_key_id = required_secret(configuration, "aws_access_key_id")?;
        let secret_access_key = required_secret(configuration, "aws_secret_access_key")?;
        let session_token = optional_text(configuration, "aws_session_token", MAX_SECRET_BYTES)?
            .map(|value| Zeroizing::new(value.to_owned()));
        let region = optional_text(configuration, "aws_region", MAX_REGION_BYTES)?
            .ok_or_else(invalid_configuration)?;
        if !valid_region(region) {
            return Err(invalid_configuration());
        }
        let s3_path = optional_text(configuration, "s3_path", MAX_PATH_BYTES)?;
        let table_path = optional_text(configuration, "table_path", MAX_PATH_BYTES)?;
        // The SDK reads `table_path or s3_path`.
        let chosen = table_path.or(s3_path).ok_or_else(invalid_configuration)?;
        Ok(Self {
            access_key_id: Zeroizing::new(access_key_id.to_owned()),
            secret_access_key: Zeroizing::new(secret_access_key.to_owned()),
            session_token,
            region: region.into(),
            location: TableLocation::parse(chosen)?,
            s3_path: s3_path.map(Into::into),
            table_path: table_path.map(Into::into),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(in crate::toolkits) fn access_key_id(&self) -> &str {
        &self.access_key_id
    }

    pub(in crate::toolkits) fn secret_access_key(&self) -> &str {
        &self.secret_access_key
    }

    pub(in crate::toolkits) fn session_token(&self) -> Option<&str> {
        self.session_token.as_deref().map(String::as_str)
    }

    pub(in crate::toolkits) fn region(&self) -> &str {
        &self.region
    }

    pub(in crate::toolkits) const fn location(&self) -> &TableLocation {
        &self.location
    }

    /// `S3 Path: <s3_path> Table Path: <table_path>`, the SDK's description
    /// prefix (an unset path prints as empty).
    pub(in crate::toolkits) fn description_prefix(&self) -> String {
        let shown = |path: &Option<Box<str>>| {
            let path = path.as_deref().unwrap_or("");
            let mut end = path.len().min(MAX_DESCRIBED_PATH_BYTES);
            while !path.is_char_boundary(end) {
                end -= 1;
            }
            path[..end].to_owned()
        };
        format!(
            "S3 Path: {} Table Path: {}",
            shown(&self.s3_path),
            shown(&self.table_path)
        )
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn required_secret<'a>(
    configuration: &'a Map<String, Value>,
    name: &str,
) -> Result<&'a str, DeltaLakeConfigError> {
    optional_text(configuration, name, MAX_SECRET_BYTES)?.ok_or_else(invalid_configuration)
}

/// A string field that may be absent, null or empty (all read as unset, as
/// the SDK's falsy check does).
fn optional_text<'a>(
    configuration: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, DeltaLakeConfigError> {
    match configuration.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            let value = value.trim();
            if value.is_empty() {
                return Ok(None);
            }
            if value.len() > limit {
                return Err(resource_exhausted());
            }
            if value.bytes().any(|byte| byte.is_ascii_control()) {
                return Err(invalid_configuration());
            }
            Ok(Some(value))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, DeltaLakeConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut seen = HashSet::with_capacity(values.len());
    let mut selected = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().ok_or_else(invalid_configuration)?;
        if value.is_empty()
            || value.len() > MAX_TOOL_NAME_BYTES
            || value.bytes().any(|byte| byte.is_ascii_control())
        {
            return Err(invalid_configuration());
        }
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> DeltaLakeConfigError {
    DeltaLakeConfigError {
        code: DeltaLakeConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> DeltaLakeConfigError {
    DeltaLakeConfigError {
        code: DeltaLakeConfigErrorCode::ResourceExhausted,
    }
}
