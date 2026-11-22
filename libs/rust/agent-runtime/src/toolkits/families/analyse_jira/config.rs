//! The `analyse_jira` toolkit's settings: the Jira credential and connection
//! settings the `jira` family already reads, and the analysis settings
//! (`artifact_bucket_path`, `closed_status`, `defects_name`, `custom_fields`,
//! `project_keys`, `max_issues`).

use std::fmt;

use serde_json::{Map, Value};

use crate::toolkits::families::UnsupportedSetting;
use crate::toolkits::families::jira::config::{JiraConfigErrorCode, JiraToolkitConfig};
use crate::toolkits::invocation::MaterializedToolsetError;

use super::dataset::is_reserved_column;

const DEFAULT_BUCKET: &str = "analyse-jira";
const DEFAULT_CLOSED_STATUS: &str = "Done";
const DEFAULT_DEFECTS: &str = "Defect";
const DEFAULT_MAX_ISSUES: usize = 5_000;
const MAX_MAX_ISSUES: u64 = 50_000;
const MAX_TEXT_BYTES: usize = 512;
const MAX_CUSTOM_FIELDS: usize = 32;
const MAX_COLUMN_BYTES: usize = 64;
const MAX_FIELD_NAMES: usize = 8;
const MAX_PROJECT_KEYS: usize = 64;
const MAX_PROJECT_KEY_BYTES: usize = 64;
const MAX_BUCKET_BYTES: usize = 63;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AnalyseJiraErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    /// A setting the runtime deliberately refuses (`verify_ssl: false`, a
    /// plain-HTTP origin, a bucket that is no bucket name).
    UnsupportedCapability(UnsupportedSetting),
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// A configuration or build failure that never carries a credential or an
/// origin.
pub(crate) struct AnalyseJiraError {
    code: AnalyseJiraErrorCode,
}

impl AnalyseJiraError {
    pub(super) const fn new(code: AnalyseJiraErrorCode) -> Self {
        Self { code }
    }

    #[must_use]
    pub(crate) const fn code(&self) -> AnalyseJiraErrorCode {
        self.code
    }
}

impl fmt::Debug for AnalyseJiraError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AnalyseJiraError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AnalyseJiraError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AnalyseJiraErrorCode::InvalidConfiguration => {
                "the Jira analysis toolkit configuration is invalid"
            }
            AnalyseJiraErrorCode::ResourceExhausted => {
                "the Jira analysis toolkit configuration exceeds its approved limit"
            }
            AnalyseJiraErrorCode::UnsupportedCapability(_) => {
                "the Jira analysis toolkit configuration requires a capability this runtime does not provide"
            }
            AnalyseJiraErrorCode::UnsupportedSelection => {
                "the selected Jira analysis tool profile is not supported"
            }
            AnalyseJiraErrorCode::Client => "the Jira client could not be created",
            AnalyseJiraErrorCode::InvalidDefinition => {
                "the Jira analysis ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for AnalyseJiraError {}

impl From<MaterializedToolsetError> for AnalyseJiraError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self::new(AnalyseJiraErrorCode::InvalidDefinition)
    }
}

/// The analysis settings.
pub(super) struct AnalysisSettings {
    pub(super) bucket: String,
    pub(super) closed_status: String,
    pub(super) defect_types: Vec<String>,
    /// Dataset column and the Jira field names it reads. `team` is always
    /// first; a column with no names stays empty.
    pub(super) custom_fields: Vec<(String, Vec<String>)>,
    pub(super) project_keys: Vec<String>,
    pub(super) max_issues: usize,
}

/// The toolkit's whole configuration.
pub(crate) struct AnalyseJiraConfig {
    pub(super) jira: JiraToolkitConfig,
    pub(super) analysis: AnalysisSettings,
}

impl AnalyseJiraConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AnalyseJiraError> {
        let jira = JiraToolkitConfig::parse(settings).map_err(|error| {
            AnalyseJiraError::new(match error.code() {
                JiraConfigErrorCode::InvalidConfiguration => {
                    AnalyseJiraErrorCode::InvalidConfiguration
                }
                JiraConfigErrorCode::ResourceExhausted => AnalyseJiraErrorCode::ResourceExhausted,
                JiraConfigErrorCode::UnsupportedCapability(setting) => {
                    AnalyseJiraErrorCode::UnsupportedCapability(setting)
                }
            })
        })?;
        Ok(Self {
            jira,
            analysis: AnalysisSettings::parse(settings)?,
        })
    }
}

impl AnalysisSettings {
    fn parse(settings: &Map<String, Value>) -> Result<Self, AnalyseJiraError> {
        let bucket = text(settings, "artifact_bucket_path", DEFAULT_BUCKET)?;
        let bucket = bucket.trim().to_owned();
        if !valid_bucket(&bucket) {
            return Err(AnalyseJiraError::new(
                AnalyseJiraErrorCode::UnsupportedCapability(UnsupportedSetting::BucketName),
            ));
        }
        let defects = text(settings, "defects_name", DEFAULT_DEFECTS)?;
        let max_issues = match settings.get("max_issues") {
            None | Some(Value::Null) => DEFAULT_MAX_ISSUES,
            Some(Value::Number(number)) => number
                .as_u64()
                .filter(|value| (1..=MAX_MAX_ISSUES).contains(value))
                .and_then(|value| usize::try_from(value).ok())
                .ok_or_else(invalid)?,
            Some(_) => return Err(invalid()),
        };
        Ok(Self {
            bucket,
            closed_status: text(settings, "closed_status", DEFAULT_CLOSED_STATUS)?
                .trim()
                .to_owned(),
            defect_types: defects
                .split(',')
                .map(str::trim)
                .filter(|name| !name.is_empty())
                .map(ToOwned::to_owned)
                .collect(),
            custom_fields: custom_fields(settings)?,
            project_keys: project_keys(&text(settings, "project_keys", "")?)
                .map_err(|()| invalid())?,
            max_issues,
        })
    }
}

fn invalid() -> AnalyseJiraError {
    AnalyseJiraError::new(AnalyseJiraErrorCode::InvalidConfiguration)
}

fn exhausted() -> AnalyseJiraError {
    AnalyseJiraError::new(AnalyseJiraErrorCode::ResourceExhausted)
}

fn text(
    settings: &Map<String, Value>,
    name: &str,
    default: &str,
) -> Result<String, AnalyseJiraError> {
    match settings.get(name) {
        None | Some(Value::Null) => Ok(default.to_owned()),
        Some(Value::String(value)) => {
            if value.len() > MAX_TEXT_BYTES {
                return Err(exhausted());
            }
            if value.chars().any(char::is_control) {
                return Err(invalid());
            }
            Ok(value.clone())
        }
        Some(_) => Err(invalid()),
    }
}

/// The bucket-name shape the platform enforces: lowercase letters, digits
/// and hyphens, starting with a letter.
fn valid_bucket(bucket: &str) -> bool {
    let bytes = bucket.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= MAX_BUCKET_BYTES
        && bytes[0].is_ascii_lowercase()
        && bytes
            .iter()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || *byte == b'-')
}

fn custom_fields(
    settings: &Map<String, Value>,
) -> Result<Vec<(String, Vec<String>)>, AnalyseJiraError> {
    let mut fields: Vec<(String, Vec<String>)> = vec![("team".to_owned(), Vec::new())];
    let configured = match settings.get("custom_fields") {
        None | Some(Value::Null) => return Ok(fields),
        Some(Value::Object(configured)) => configured,
        Some(_) => return Err(invalid()),
    };
    if configured.len() > MAX_CUSTOM_FIELDS {
        return Err(exhausted());
    }
    for (column, names) in configured {
        let column = column.trim();
        let well_formed = !column.is_empty()
            && column.len() <= MAX_COLUMN_BYTES
            && column
                .chars()
                .all(|character| character.is_ascii_alphanumeric() || character == '_')
            && !column.starts_with(|character: char| character.is_ascii_digit());
        if !well_formed
            || (column != "team" && is_reserved_column(column))
            || column == "year_month"
        {
            return Err(invalid());
        }
        let names: Vec<String> = match names {
            Value::Null => Vec::new(),
            Value::String(names) => names
                .split(',')
                .map(str::trim)
                .filter(|name| !name.is_empty())
                .map(ToOwned::to_owned)
                .collect(),
            _ => return Err(invalid()),
        };
        if names.len() > MAX_FIELD_NAMES || names.iter().any(|name| name.len() > MAX_TEXT_BYTES) {
            return Err(exhausted());
        }
        if names.iter().any(|name| name.chars().any(char::is_control)) {
            return Err(invalid());
        }
        match fields.iter_mut().find(|(existing, _)| existing == column) {
            Some(existing) => existing.1 = names,
            None => fields.push((column.to_owned(), names)),
        }
    }
    // `team` first, the rest by name: the same column order whichever order
    // the settings object keeps its keys in.
    fields[1..].sort_by(|a, b| a.0.cmp(&b.0));
    Ok(fields)
}

/// Comma, semicolon or space separated project keys: letters, digits and
/// underscores, so a key can never alter a JQL query.
pub(super) fn project_keys(text: &str) -> Result<Vec<String>, ()> {
    let mut keys: Vec<String> = Vec::new();
    for key in text
        .split([',', ';', ' '])
        .map(str::trim)
        .filter(|key| !key.is_empty())
    {
        if key.len() > MAX_PROJECT_KEY_BYTES
            || !key
                .chars()
                .all(|character| character.is_ascii_alphanumeric() || character == '_')
        {
            return Err(());
        }
        if !keys.iter().any(|existing| existing == key) {
            keys.push(key.to_owned());
        }
    }
    if keys.len() > MAX_PROJECT_KEYS {
        return Err(());
    }
    Ok(keys)
}

#[cfg(test)]
mod tests {
    use serde_json::{Value, json};

    use super::{AnalyseJiraConfig, AnalyseJiraErrorCode, UnsupportedSetting, project_keys};

    fn settings(extra: &Value) -> serde_json::Map<String, Value> {
        let mut base = json!({
            "jira_configuration": {
                "base_url": "https://tenant.atlassian.net/",
                "hosting": "Cloud",
                "token": "fixture"
            }
        });
        for (name, value) in extra.as_object().expect("object") {
            base[name] = value.clone();
        }
        base.as_object().cloned().expect("object")
    }

    fn failure(extra: &Value) -> AnalyseJiraErrorCode {
        match AnalyseJiraConfig::parse(&settings(extra)) {
            Err(error) => error.code(),
            Ok(_) => panic!("{extra} was accepted"),
        }
    }

    #[test]
    fn the_defaults_are_the_documented_ones() {
        let config = AnalyseJiraConfig::parse(&settings(&json!({}))).expect("config");
        let analysis = &config.analysis;
        assert_eq!(analysis.bucket, "analyse-jira");
        assert_eq!(analysis.closed_status, "Done");
        assert_eq!(analysis.defect_types, ["Defect"]);
        assert_eq!(analysis.custom_fields, [("team".to_owned(), Vec::new())]);
        assert_eq!(analysis.project_keys.len(), 0);
        assert_eq!(analysis.max_issues, 5_000);
    }

    #[test]
    fn configured_values_are_read() {
        let config = AnalyseJiraConfig::parse(&settings(&json!({
            "artifact_bucket_path": "reports",
            "closed_status": " Closed ",
            "defects_name": "Bug, Defect,",
            "custom_fields": {"team": "Team", "points": "Story Points, Story point estimate", "env": ""},
            "project_keys": "ALP, BET;GAM",
            "max_issues": 100
        })))
        .expect("config");
        let analysis = &config.analysis;
        assert_eq!(analysis.bucket, "reports");
        assert_eq!(analysis.closed_status, "Closed");
        assert_eq!(analysis.defect_types, ["Bug", "Defect"]);
        assert_eq!(analysis.project_keys, ["ALP", "BET", "GAM"]);
        assert_eq!(analysis.max_issues, 100);
        assert_eq!(
            analysis.custom_fields[0],
            ("team".to_owned(), vec!["Team".to_owned()])
        );
        assert_eq!(analysis.custom_fields[1], ("env".to_owned(), Vec::new()));
        assert_eq!(
            analysis.custom_fields[2],
            (
                "points".to_owned(),
                vec!["Story Points".to_owned(), "Story point estimate".to_owned()]
            )
        );
    }

    #[test]
    fn a_bad_bucket_names_the_bucket_setting() {
        let long = "x".repeat(64);
        for bucket in ["Reports", "1data", "a_b", "", "a b", long.as_str()] {
            assert_eq!(
                failure(&json!({"artifact_bucket_path": bucket})),
                AnalyseJiraErrorCode::UnsupportedCapability(UnsupportedSetting::BucketName),
                "{bucket:?}"
            );
        }
    }

    #[test]
    fn unusable_settings_are_refused() {
        for extra in [
            json!({"max_issues": 0}),
            json!({"max_issues": 50_001}),
            json!({"max_issues": "many"}),
            json!({"closed_status": 5}),
            json!({"project_keys": "AL P!"}),
            json!({"custom_fields": []}),
            json!({"custom_fields": {"status": "Status"}}),
            json!({"custom_fields": {"time_in_status": "x"}}),
            json!({"custom_fields": {"1x": "x"}}),
            json!({"custom_fields": {"bad name": "x"}}),
            json!({"custom_fields": {"year_month": "x"}}),
            json!({"custom_fields": {"a": 3}}),
        ] {
            assert_eq!(
                failure(&extra),
                AnalyseJiraErrorCode::InvalidConfiguration,
                "{extra}"
            );
        }
    }

    #[test]
    fn the_jira_settings_are_the_jira_familys() {
        assert_eq!(
            failure(&json!({"verify_ssl": false})),
            AnalyseJiraErrorCode::UnsupportedCapability(UnsupportedSetting::VerifySslDisabled)
        );
        assert!(AnalyseJiraConfig::parse(&serde_json::Map::new()).is_err());
    }

    #[test]
    fn project_keys_are_letters_digits_and_underscores() {
        assert_eq!(
            project_keys("ALP,ALP, b_2"),
            Ok(vec!["ALP".to_owned(), "b_2".to_owned()])
        );
        assert_eq!(project_keys(""), Ok(Vec::new()));
        assert_eq!(project_keys("A\" OR 1=1"), Err(()));
        assert_eq!(project_keys("A-B"), Err(()));
    }
}
