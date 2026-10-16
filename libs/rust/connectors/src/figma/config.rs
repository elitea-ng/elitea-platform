//! The Figma toolkit configuration as the indexing connector reads it
//! (`figma_configuration.token`, sent as `X-Figma-Token`, the SDK's
//! `EliteAFigmaPy`), and the `index_data` arguments the connector honours
//! (the SDK's `FigmaApiWrapper._index_tool_params`, plus three additions
//! recorded in the worker's `SOURCE_PARITY.md`).

use std::fmt;

use serde_json::{Map, Value};
use url::Url;
use zeroize::Zeroizing;

pub(crate) const PROVIDER: &str = "Figma";
/// The one origin the connector talks to, and the only one the token is sent
/// to.
pub const API_ORIGIN: &str = "https://api.figma.com";
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_ENTRIES: usize = 1_000;
const MAX_ENTRY_BYTES: usize = 4 * 1_024;
pub(crate) const MAX_ID_BYTES: usize = 128;
/// The SDK's `SUBFRAME_EXTRACT_THRESHOLD`.
pub const DEFAULT_SUBFRAME_EXTRACT_THRESHOLD: u64 = 15_000;

/// Why a Figma configuration or argument was refused. Names the setting,
/// never a value.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FigmaConfigError(String);

impl FigmaConfigError {
    fn new(reason: impl Into<String>) -> Self {
        Self(reason.into())
    }
}

impl fmt::Display for FigmaConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{PROVIDER}: {}", self.0)
    }
}

impl std::error::Error for FigmaConfigError {}

/// Invocation-scoped Figma authority for one index run. Non-`Clone` and
/// non-`Debug`: the token never prints.
pub struct FigmaConnectorConfig {
    // Read only by the `content-source` connector; the `providers`-only build
    // (the desktop host) parses the settings without indexing.
    #[cfg_attr(not(feature = "content-source"), allow(dead_code))]
    token: Zeroizing<String>,
}

impl FigmaConnectorConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// No `figma_configuration.token`, or one that is empty, too long or not
    /// a header value.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, FigmaConfigError> {
        let token = settings
            .get("figma_configuration")
            .and_then(Value::as_object)
            .and_then(|configuration| configuration.get("token"))
            .and_then(Value::as_str)
            .ok_or_else(|| {
                FigmaConfigError::new("the toolkit has no `figma_configuration.token`")
            })?;
        if token.len() > MAX_TOKEN_BYTES {
            return Err(FigmaConfigError::new("the token is longer than its bound"));
        }
        if token.trim().is_empty() || token.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
            return Err(FigmaConfigError::new(
                "the token is not a valid header value",
            ));
        }
        Ok(Self {
            token: Zeroizing::new(token.to_owned()),
        })
    }

    #[cfg_attr(not(feature = "content-source"), allow(dead_code))]
    pub(crate) fn token(&self) -> &str {
        &self.token
    }
}

/// One file the run indexes: its key and the nodes (pages or frames) a URL
/// named.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FileSelection {
    pub key: String,
    /// The `node-id` query values of the URL, as the REST API spells node
    /// ids (`1:2`); empty for a bare key.
    pub node_ids: Vec<String>,
}

impl FileSelection {
    /// Fold another selection of the same file into this one: the node ids
    /// are unioned, and a selection with none (a bare key) names the whole
    /// file, which then stays whole.
    pub fn merge(&mut self, other: Self) {
        if self.node_ids.is_empty() || other.node_ids.is_empty() {
            self.node_ids.clear();
            return;
        }
        for id in other.node_ids {
            if !self.node_ids.contains(&id) {
                self.node_ids.push(id);
            }
        }
    }
}

/// The `index_data` arguments the connector honours.
///
/// Accepted and ignored (they steer the SDK's image and thread handling,
/// which wait for I4 or have no counterpart): `number_of_threads`,
/// `image_max_dimension`, `frame_spatial_order`, `scale_to_limit_threshold`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FigmaIndexArguments {
    /// `urls_or_file_keys`: Figma `file`/`design` URLs and bare file keys.
    pub files: Vec<FileSelection>,
    /// Addition: every file of these projects.
    pub project_ids: Vec<String>,
    /// Addition: every file of every project of these teams.
    pub team_ids: Vec<String>,
    /// Pages (or frames) to index when the entry's URL names none.
    pub node_ids_include: Vec<String>,
    /// Pages left out when no include list applies.
    pub node_ids_exclude: Vec<String>,
    /// Lower-case node types kept among a page's top-level nodes.
    pub node_types_include: Vec<String>,
    /// Lower-case node types dropped when no include list is given.
    pub node_types_exclude: Vec<String>,
    /// A top-level frame larger than this (either side, in pixels) is
    /// indexed through its children instead.
    pub subframe_extract_threshold: u64,
    /// Addition: the file's comments, one document each.
    pub include_comments: bool,
}

impl FigmaIndexArguments {
    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// An argument of the wrong type, a list over its bound, a file key or
    /// URL that is not one, or nothing to index.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, FigmaConfigError> {
        let mut files: Vec<FileSelection> = Vec::new();
        for entry in text_list(arguments, "urls_or_file_keys")? {
            let selection = file_selection(&entry)?;
            match files.iter_mut().find(|known| known.key == selection.key) {
                Some(known) => known.merge(selection),
                None => files.push(selection),
            }
        }
        let project_ids = id_list(arguments, "project_ids")?;
        let team_ids = id_list(arguments, "team_ids")?;
        if files.is_empty() && project_ids.is_empty() && team_ids.is_empty() {
            return Err(FigmaConfigError::new(
                "`urls_or_file_keys` must name at least one Figma file URL or file key",
            ));
        }
        let threshold = match arguments.get("subframe_extract_threshold") {
            None | Some(Value::Null) => DEFAULT_SUBFRAME_EXTRACT_THRESHOLD,
            Some(Value::Number(number)) => number
                .as_u64()
                .filter(|value| *value > 0)
                .ok_or_else(|| invalid("subframe_extract_threshold"))?,
            Some(_) => return Err(invalid("subframe_extract_threshold")),
        };
        let include_comments = match arguments.get("include_comments") {
            None | Some(Value::Null) => false,
            Some(Value::Bool(flag)) => *flag,
            Some(_) => return Err(invalid("include_comments")),
        };
        Ok(Self {
            files,
            project_ids,
            team_ids,
            node_ids_include: node_ids(arguments, "node_ids_include")?,
            node_ids_exclude: node_ids(arguments, "node_ids_exclude")?,
            node_types_include: lower_list(arguments, "node_types_include")?,
            node_types_exclude: lower_list(arguments, "node_types_exclude")?,
            subframe_extract_threshold: threshold,
            include_comments,
        })
    }
}

fn invalid(name: &str) -> FigmaConfigError {
    FigmaConfigError::new(format!("the `{name}` argument is invalid"))
}

/// A list of strings, or one comma-separated string (the SDK splits
/// `urls_or_file_keys` on commas); `[]` and null are "none given".
fn text_list(arguments: &Map<String, Value>, name: &str) -> Result<Vec<String>, FigmaConfigError> {
    let items: Vec<&str> = match arguments.get(name) {
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::String(text)) => text.split(',').collect(),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| item.as_str().ok_or_else(|| invalid(name)))
            .collect::<Result<_, _>>()?,
        Some(_) => return Err(invalid(name)),
    };
    if items.len() > MAX_ENTRIES || items.iter().any(|item| item.len() > MAX_ENTRY_BYTES) {
        return Err(FigmaConfigError::new(format!(
            "the `{name}` argument is longer than its bound"
        )));
    }
    Ok(items
        .into_iter()
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .map(ToOwned::to_owned)
        .collect())
}

fn lower_list(arguments: &Map<String, Value>, name: &str) -> Result<Vec<String>, FigmaConfigError> {
    Ok(text_list(arguments, name)?
        .into_iter()
        .map(|item| item.to_ascii_lowercase())
        .collect())
}

fn id_list(arguments: &Map<String, Value>, name: &str) -> Result<Vec<String>, FigmaConfigError> {
    let ids = text_list(arguments, name)?;
    if ids.iter().any(|id| !plain_identifier(id)) {
        return Err(invalid(name));
    }
    Ok(ids)
}

fn node_ids(arguments: &Map<String, Value>, name: &str) -> Result<Vec<String>, FigmaConfigError> {
    text_list(arguments, name)?
        .iter()
        .map(|id| node_id(id).ok_or_else(|| invalid(name)))
        .collect()
}

/// A project, team, file or comment identifier: letters, digits, `-` and
/// `_`. The one validator of every identifier that becomes a URL path
/// segment or a key.
pub(crate) fn plain_identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_ID_BYTES
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
}

/// A node id as the REST API spells it. A URL spells `1:2` as `1-2` and an
/// instance sublayer `I5:10;20:30` as `I5-10;20-30`; both are converted back,
/// one `;`-separated segment at a time. `None` for anything that is not an id.
#[must_use]
pub fn node_id(raw: &str) -> Option<String> {
    let raw = raw.trim();
    if raw.is_empty()
        || raw.len() > MAX_ID_BYTES
        || !raw
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b':' | b';'))
    {
        return None;
    }
    Some(
        raw.split(';')
            .map(|segment| {
                let digits = segment.strip_prefix('I').unwrap_or(segment);
                let url_form = digits.split_once('-').is_some_and(|(left, right)| {
                    !left.is_empty()
                        && !right.is_empty()
                        && left.bytes().all(|byte| byte.is_ascii_digit())
                        && right.bytes().all(|byte| byte.is_ascii_digit())
                });
                if url_form {
                    segment.replacen('-', ":", 1)
                } else {
                    segment.to_owned()
                }
            })
            .collect::<Vec<_>>()
            .join(";"),
    )
}

/// The SDK's `_parse_figma_url` for an `http(s)` entry on a `figma.com`
/// host, a bare key otherwise. A branch URL
/// (`/design/<FILE_KEY>/branch/<BRANCH_KEY>/...`) names the branch's own
/// file: the branch key is the file key the REST API takes.
fn file_selection(entry: &str) -> Result<FileSelection, FigmaConfigError> {
    if !(entry.starts_with("http://") || entry.starts_with("https://")) {
        if !plain_identifier(entry) {
            return Err(FigmaConfigError::new(
                "an entry of `urls_or_file_keys` is neither a Figma URL nor a file key",
            ));
        }
        return Ok(FileSelection {
            key: entry.to_owned(),
            node_ids: Vec::new(),
        });
    }
    let bad_url = || {
        FigmaConfigError::new(
            "a Figma URL must look like https://www.figma.com/file/<FILE_KEY>/... or /design/<FILE_KEY>/...",
        )
    };
    let url = Url::parse(entry).map_err(|_| bad_url())?;
    if !matches!(url.host_str(), Some("figma.com" | "www.figma.com")) {
        return Err(FigmaConfigError::new(
            "a Figma URL must be on figma.com or www.figma.com",
        ));
    }
    let mut segments = url.path_segments().ok_or_else(bad_url)?;
    if !matches!(segments.next(), Some("file" | "design")) {
        return Err(bad_url());
    }
    let mut key = segments
        .next()
        .filter(|key| plain_identifier(key))
        .ok_or_else(bad_url)?;
    if segments.next() == Some("branch") {
        key = segments
            .next()
            .filter(|branch| plain_identifier(branch))
            .ok_or_else(bad_url)?;
    }
    let node_ids = url
        .query_pairs()
        .filter(|(name, _)| name == "node-id")
        .map(|(_, value)| node_id(&value).ok_or_else(bad_url))
        .collect::<Result<Vec<_>, _>>()?;
    Ok(FileSelection {
        key: key.to_owned(),
        node_ids,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn arguments(value: &Value) -> Result<FigmaIndexArguments, FigmaConfigError> {
        FigmaIndexArguments::parse(value.as_object().unwrap())
    }

    #[test]
    fn the_token_is_required_and_never_printed() {
        let parse = |settings: Value| FigmaConnectorConfig::parse(settings.as_object().unwrap());
        let config = parse(json!({"figma_configuration": {"token": "figd_secret"}})).expect("ok");
        assert_eq!(config.token(), "figd_secret");
        for bad in [
            json!({}),
            json!({"figma_configuration": {}}),
            json!({"figma_configuration": {"token": "  "}}),
            json!({"figma_configuration": {"token": "a\nb"}}),
        ] {
            let error = parse(bad).err().expect("refused");
            assert!(!error.to_string().contains("figd_"), "{error}");
        }
    }

    #[test]
    fn urls_and_keys_follow_the_sdk_forms() {
        let parsed = arguments(&json!({
            "urls_or_file_keys":
                "https://www.figma.com/design/AbC123/Name?node-id=12-34&node-id=5:6, XYZ789 ,AbC123",
            "node_ids_include": ["7-8"], "node_ids_exclude": "9:10",
            "node_types_include": ["FRAME"], "number_of_threads": 3,
            "image_max_dimension": 2000,
        }))
        .expect("ok");
        assert_eq!(
            parsed.files,
            vec![
                FileSelection {
                    key: "AbC123".into(),
                    node_ids: Vec::new()
                },
                FileSelection {
                    key: "XYZ789".into(),
                    node_ids: Vec::new()
                },
            ],
            "the repeated bare key is one file, widened to all of it"
        );
        assert_eq!(parsed.node_ids_include, vec!["7:8"]);
        assert_eq!(
            node_id("I5-10;20-30").as_deref(),
            Some("I5:10;20:30"),
            "an instance sublayer in its URL form"
        );
        assert_eq!(node_id("I5:10;20:30").as_deref(), Some("I5:10;20:30"));
        assert_eq!(node_id("1:2").as_deref(), Some("1:2"));
        assert_eq!(node_id("a-b").as_deref(), Some("a-b"), "not an id pair");
        let encoded = arguments(&json!({"urls_or_file_keys":
            "https://www.figma.com/design/K1/x?node-id=I5-10%3B20-30"}))
        .expect("percent-encoded sublayer");
        assert_eq!(encoded.files[0].node_ids, vec!["I5:10;20:30"]);
        assert_eq!(parsed.node_ids_exclude, vec!["9:10"]);
        assert_eq!(parsed.node_types_include, vec!["frame"]);
        assert_eq!(
            parsed.subframe_extract_threshold,
            DEFAULT_SUBFRAME_EXTRACT_THRESHOLD
        );
        assert!(!parsed.include_comments);
        let file = arguments(&json!({"urls_or_file_keys": ["https://www.figma.com/file/K1/x"]}))
            .expect("file url");
        assert_eq!(file.files[0].key, "K1");
    }

    #[test]
    fn something_to_index_is_required_and_bad_input_is_refused() {
        assert!(arguments(&json!({})).is_err());
        assert!(arguments(&json!({"project_ids": ["123"]})).is_ok());
        assert!(arguments(&json!({"team_ids": "55,56"})).is_ok());
        for bad in [
            json!({"urls_or_file_keys": "https://www.figma.com/board/K1/x"}),
            json!({"urls_or_file_keys": "https://www.figma.com/file/"}),
            json!({"urls_or_file_keys": "https://evil.example/design/K1/x"}),
            json!({"urls_or_file_keys": "https://figma.com.evil.example/design/K1/x"}),
            json!({"urls_or_file_keys": "https://www.figma.com/design/K1/branch/"}),
            json!({"urls_or_file_keys": "../etc"}),
            json!({"urls_or_file_keys": "K1", "include_comments": "yes"}),
            json!({"urls_or_file_keys": "K1", "subframe_extract_threshold": 0}),
            json!({"urls_or_file_keys": "K1", "node_ids_include": [3]}),
            json!({"urls_or_file_keys": "K1", "project_ids": ["a/b"]}),
        ] {
            assert!(arguments(&bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn selections_of_one_file_merge_and_a_bare_key_widens_it() {
        let files = |value: Value| arguments(&value).expect("ok").files;
        let selection = |key: &str, ids: &[&str]| FileSelection {
            key: key.into(),
            node_ids: ids.iter().map(|id| (*id).to_owned()).collect(),
        };
        assert_eq!(
            files(json!({"urls_or_file_keys": [
                "https://www.figma.com/design/K1/a?node-id=1-2",
                "https://www.figma.com/design/K1/b?node-id=3-4&node-id=1-2",
            ]})),
            vec![selection("K1", &["1:2", "3:4"])],
            "node ids are unioned"
        );
        for order in [
            json!(["K1", "https://www.figma.com/design/K1/a?node-id=1-2"]),
            json!(["https://www.figma.com/design/K1/a?node-id=1-2", "K1"]),
            json!([
                "https://www.figma.com/design/K1/a?node-id=1-2",
                "https://www.figma.com/design/K1/a",
                "https://www.figma.com/design/K1/a?node-id=5-6"
            ]),
        ] {
            assert_eq!(
                files(json!({"urls_or_file_keys": order})),
                vec![selection("K1", &[])],
                "a selection without node ids is the whole file, in any order: {order}"
            );
        }
    }

    #[test]
    fn urls_are_figma_hosts_and_a_branch_url_names_the_branch() {
        let key = |url: &str| {
            arguments(&json!({"urls_or_file_keys": url}))
                .expect("ok")
                .files
                .remove(0)
        };
        for host in ["figma.com", "www.figma.com"] {
            assert_eq!(key(&format!("https://{host}/file/K1/x")).key, "K1");
        }
        let branch = key("https://www.figma.com/design/MAIN1/branch/BR2/Name?node-id=7-8");
        assert_eq!(branch.key, "BR2");
        assert_eq!(branch.node_ids, vec!["7:8"]);
        assert_eq!(
            key("https://www.figma.com/file/MAIN1/branch/BR2/x").key,
            "BR2"
        );
    }
}
