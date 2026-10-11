//! `configuration.validate.v1`: schema validation of one SDK configuration.
//!
//! The Python worker answers this capability with a pydantic
//! `model_validate` of the pinned SDK's registered configuration model. It
//! never opens a connection: the SDK's `check_connection` hooks belong to the
//! separate `toolkit.call_tool.v1` test path. "Valid" therefore means exactly
//! what pydantic accepts for that model, and the result carries only a closed
//! set of issue codes with fixed messages (Main refuses any other text).
//!
//! The Rust worker cannot import pydantic. It carries a data table derived
//! from each model's JSON schema, plus the two `model_validator(mode="before")`
//! hooks (`github`, `openapi`) as code.
//! `tests/fixtures/generate_configuration_validation_rules.py` regenerates the
//! table and refuses to run unless it reproduces Main's catalog digests, and
//! `tests.rs` replays the pydantic outcomes it records against this
//! implementation case by case.

use std::collections::BTreeSet;
use std::sync::LazyLock;

use serde::Deserialize;
use serde_json::{Map, Value};

use crate::protocol::elitea::runtime::v1::ConfigurationValidationCommandV1;
use crate::protocol::output::RuntimeFailureKind;

mod lax;
mod strict_json;

#[cfg(test)]
mod tests;

const RULES_JSON: &str = include_str!("configuration_rules.json");
const RULES_SCHEMA_VERSION: &str = "elitea.worker-configuration-validation-rules.v1";

/// Bounds shared with the Python worker's `constants.py`.
const MAX_ISSUES: usize = 64;
const MAX_JSON_DEPTH: usize = 64;
const MAX_STRING_BYTES: usize = 64 * 1024;
const MAX_SETTINGS_BYTES: usize = 256 * 1024;

/// The closed issue vocabulary Main accepts, with its canonical messages.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) enum IssueCode {
    InvalidConfiguration,
    InvalidValue,
    RequiredField,
    ValueNotAllowed,
}

impl IssueCode {
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "INVALID_CONFIGURATION",
            Self::InvalidValue => "INVALID_VALUE",
            Self::RequiredField => "REQUIRED_FIELD",
            Self::ValueNotAllowed => "VALUE_NOT_ALLOWED",
        }
    }

    /// The codes this worker emits. Main also admits `UNKNOWN_FIELD` and
    /// `VALUE_OUT_OF_RANGE`, which no SDK configuration model produces here
    /// (none forbids extras or bounds a number).
    pub(crate) fn from_wire(code: &str) -> Option<Self> {
        [
            Self::InvalidConfiguration,
            Self::InvalidValue,
            Self::RequiredField,
            Self::ValueNotAllowed,
        ]
        .into_iter()
        .find(|candidate| candidate.code() == code)
    }

    pub(crate) const fn safe_message(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "Configuration fields are inconsistent.",
            Self::InvalidValue => "Value does not satisfy the configuration schema.",
            Self::RequiredField => "A required value is missing.",
            Self::ValueNotAllowed => "Value is not one of the allowed choices.",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Issue {
    pub(crate) code: IssueCode,
    pub(crate) json_pointer: String,
}

/// Why a command was refused before any settings were read.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum BindingRefusal {
    /// The catalog or per-type schema is not the one this worker carries.
    IncompatibleVersion,
    /// The type is outside the table this worker can validate. The name is
    /// kept for logs; the wire failure is the registered
    /// `UNSUPPORTED_CAPABILITY`, whose fixed text cannot carry it.
    UnsupportedConfigurationType { configuration_type: String },
}

impl BindingRefusal {
    pub(crate) const fn failure(&self) -> RuntimeFailureKind {
        match self {
            Self::IncompatibleVersion => RuntimeFailureKind::IncompatibleVersion,
            Self::UnsupportedConfigurationType { .. } => RuntimeFailureKind::UnsupportedCapability,
        }
    }
}

/// Why the settings document itself could not be evaluated.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SettingsRefusal {
    InvalidInput,
    ResourceExhausted,
}

impl SettingsRefusal {
    pub(crate) const fn failure(self) -> RuntimeFailureKind {
        match self {
            Self::InvalidInput => RuntimeFailureKind::InvalidInput,
            Self::ResourceExhausted => RuntimeFailureKind::ResourceExhausted,
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
enum FieldKind {
    String,
    Enum,
    Integer,
    Boolean,
    StringList,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct FieldRule {
    name: String,
    kind: FieldKind,
    nullable: bool,
    required: bool,
    #[serde(default)]
    values: Vec<String>,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
enum Hook {
    GithubAuth,
    OpenapiAuth,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct TypeRules {
    #[serde(rename = "type")]
    configuration_type: String,
    #[allow(dead_code)]
    section: String,
    schema_id: String,
    schema_digest: String,
    hook: Option<Hook>,
    fields: Vec<FieldRule>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RulesDocument {
    schema_version: String,
    catalog_revision: String,
    catalog_digest: String,
    types: Vec<TypeRules>,
}

pub(crate) struct ConfigurationCatalog {
    revision: String,
    digest: [u8; 32],
    types: Vec<(TypeRules, [u8; 32])>,
}

static PINNED: LazyLock<Option<ConfigurationCatalog>> =
    LazyLock::new(|| ConfigurationCatalog::from_json(RULES_JSON));

impl ConfigurationCatalog {
    /// The catalog compiled into this worker; `None` only if the embedded table
    /// is malformed, which a unit test rules out for every build.
    pub(crate) fn pinned() -> Option<&'static Self> {
        PINNED.as_ref()
    }

    fn from_json(raw: &str) -> Option<Self> {
        let document: RulesDocument = serde_json::from_str(raw).ok()?;
        if document.schema_version != RULES_SCHEMA_VERSION {
            return None;
        }
        let mut seen = BTreeSet::new();
        let mut types = Vec::with_capacity(document.types.len());
        for rules in document.types {
            if !seen.insert(rules.configuration_type.clone())
                || rules.schema_id != format!("elitea.configuration.{}", rules.configuration_type)
            {
                return None;
            }
            let digest = decode_digest(&rules.schema_digest)?;
            types.push((rules, digest));
        }
        Some(Self {
            revision: document.catalog_revision,
            digest: decode_digest(&document.catalog_digest)?,
            types,
        })
    }

    #[cfg(test)]
    pub(crate) fn type_names(&self) -> impl Iterator<Item = &str> {
        self.types
            .iter()
            .map(|(rules, _)| rules.configuration_type.as_str())
    }

    /// A command that binds cleanly to `configuration_type`, as Main builds it.
    #[cfg(test)]
    pub(crate) fn command_for(
        &self,
        configuration_type: &str,
    ) -> Option<ConfigurationValidationCommandV1> {
        use crate::protocol::elitea::runtime::v1::{DigestAlgorithmV1, DigestV1};
        let digest = |bytes: &[u8; 32]| {
            Some(DigestV1 {
                algorithm: DigestAlgorithmV1::Sha256 as i32,
                value: bytes.to_vec(),
            })
        };
        let (rules, schema_digest) = self
            .types
            .iter()
            .find(|(rules, _)| rules.configuration_type == configuration_type)?;
        Some(ConfigurationValidationCommandV1 {
            configuration_revision_id: "revision-1".into(),
            configuration_type: configuration_type.into(),
            catalog_revision: self.revision.clone(),
            catalog_digest: digest(&self.digest),
            schema_id: rules.schema_id.clone(),
            schema_revision: self.revision.clone(),
            schema_digest: digest(schema_digest),
            settings_entry_id: "settings".into(),
        })
    }

    /// Bind one command to the admitted catalog and the selected type's
    /// schema, in the order the Python handler does.
    pub(crate) fn bind(
        &self,
        command: &ConfigurationValidationCommandV1,
    ) -> Result<&TypeRules, BindingRefusal> {
        // Input shape (non-empty identities, 32-byte digests) is verified by
        // command verification (`configuration_validation_identities` in
        // protocol/command.rs) before a command reaches this point; it is not
        // re-checked here. A missing or short digest still fails closed below,
        // as an incompatible version, because the comparison cannot match.
        let catalog_digest = command.catalog_digest.as_ref().map(|d| d.value.as_slice());
        let schema_digest = command.schema_digest.as_ref().map(|d| d.value.as_slice());
        if command.catalog_revision != self.revision
            || !constant_time_eq(catalog_digest.unwrap_or_default(), &self.digest)
        {
            return Err(BindingRefusal::IncompatibleVersion);
        }
        let Some((rules, digest)) = self
            .types
            .iter()
            .find(|(rules, _)| rules.configuration_type == command.configuration_type)
        else {
            return Err(BindingRefusal::UnsupportedConfigurationType {
                configuration_type: command.configuration_type.clone(),
            });
        };
        if command.schema_id != rules.schema_id
            || command.schema_revision != self.revision
            || !constant_time_eq(schema_digest.unwrap_or_default(), digest)
        {
            return Err(BindingRefusal::IncompatibleVersion);
        }
        Ok(rules)
    }
}

impl TypeRules {
    /// Evaluate one settings document. `Ok(vec![])` is a valid configuration;
    /// otherwise the issues are deduplicated, capped and ordered exactly as
    /// the Python handler orders them (pointer, then code).
    pub(crate) fn evaluate(&self, raw: &[u8]) -> Result<Vec<Issue>, SettingsRefusal> {
        let settings = parse_settings(raw)?;
        if let Some(hook) = self.hook
            && !hook.accepts(&settings)
        {
            // A `before` model validator raises ValueError, which pydantic
            // reports alone (loc == ()) before any field is looked at.
            return Ok(vec![Issue {
                code: IssueCode::InvalidConfiguration,
                json_pointer: String::new(),
            }]);
        }
        let mut raised: Vec<Issue> = Vec::new();
        for field in &self.fields {
            field.check(settings.get(&field.name), &mut raised);
        }
        let mut seen = BTreeSet::new();
        let mut issues = Vec::new();
        for issue in raised {
            if !seen.insert((issue.code, issue.json_pointer.clone())) {
                continue;
            }
            issues.push(issue);
            if issues.len() == MAX_ISSUES {
                break;
            }
        }
        issues.sort_by(|a, b| (&a.json_pointer, a.code).cmp(&(&b.json_pointer, b.code)));
        Ok(issues)
    }
}

impl FieldRule {
    fn check(&self, value: Option<&Value>, raised: &mut Vec<Issue>) {
        let pointer = format!("/{}", escape_pointer(&self.name));
        let Some(value) = value else {
            if self.required {
                raised.push(Issue {
                    code: IssueCode::RequiredField,
                    json_pointer: pointer,
                });
            }
            return;
        };
        if value.is_null() {
            if !self.nullable {
                let code = if self.kind == FieldKind::Enum {
                    IssueCode::ValueNotAllowed
                } else {
                    IssueCode::InvalidValue
                };
                raised.push(Issue {
                    code,
                    json_pointer: pointer,
                });
            }
            return;
        }
        match self.kind {
            FieldKind::String => {
                if !value.is_string() {
                    raised.push(invalid(pointer));
                }
            }
            FieldKind::Enum => {
                let allowed = value
                    .as_str()
                    .is_some_and(|text| self.values.iter().any(|allowed| allowed == text));
                if !allowed {
                    raised.push(Issue {
                        code: IssueCode::ValueNotAllowed,
                        json_pointer: pointer,
                    });
                }
            }
            FieldKind::Integer => {
                if !lax::accepts_integer(value) {
                    raised.push(invalid(pointer));
                }
            }
            FieldKind::Boolean => {
                if !lax::accepts_boolean(value) {
                    raised.push(invalid(pointer));
                }
            }
            FieldKind::StringList => match value {
                Value::Array(items) => {
                    for (index, item) in items.iter().enumerate() {
                        if !item.is_string() {
                            raised.push(invalid(format!("{pointer}/{index}")));
                        }
                    }
                }
                _ => raised.push(invalid(pointer)),
            },
        }
    }
}

impl Hook {
    /// `false` when the SDK's `model_validator(mode="before")` would raise.
    fn accepts(self, settings: &Map<String, Value>) -> bool {
        let get = |name: &str| settings.get(name);
        match self {
            // github.py `validate_auth_sections`: only a half-configured pair
            // raises; its trailing "misconfigured" raise is unreachable.
            Self::GithubAuth => {
                let (username, password) = (truthy(get("username")), truthy(get("password")));
                let (app_id, key) = (truthy(get("app_id")), truthy(get("app_private_key")));
                username == password && app_id == key
            }
            // openapi.py `_validate_auth_consistency`.
            Self::OpenapiAuth => {
                let (client_id, client_secret) =
                    (truthy(get("client_id")), truthy(get("client_secret")));
                if truthy(get("oauth_discovery_endpoint")) {
                    if !(client_id && client_secret) {
                        return false;
                    }
                } else if (client_id || client_secret || truthy(get("token_url")))
                    && !(client_id && client_secret && truthy(get("token_url")))
                {
                    return false;
                }
                let custom = get("auth_type")
                    .and_then(Value::as_str)
                    .is_some_and(|text| py_strip(text).to_lowercase() == "custom");
                !(custom && truthy(get("api_key")) && !truthy(get("custom_header_name")))
            }
        }
    }
}

fn invalid(json_pointer: String) -> Issue {
    Issue {
        code: IssueCode::InvalidValue,
        json_pointer,
    }
}

fn escape_pointer(segment: &str) -> String {
    segment.replace('~', "~0").replace('/', "~1")
}

/// Python truthiness of a decoded JSON value.
fn truthy(value: Option<&Value>) -> bool {
    match value {
        None | Some(Value::Null) => false,
        Some(Value::Bool(flag)) => *flag,
        Some(Value::Number(number)) => number.as_f64().is_none_or(|float| float != 0.0),
        Some(Value::String(text)) => !text.is_empty(),
        Some(Value::Array(items)) => !items.is_empty(),
        Some(Value::Object(members)) => !members.is_empty(),
    }
}

/// `str.strip()`: Unicode whitespace plus the four ASCII separators Python
/// also counts.
fn py_strip(text: &str) -> &str {
    text.trim_matches(|c: char| c.is_whitespace() || ('\u{1c}'..='\u{1f}').contains(&c))
}

fn parse_settings(raw: &[u8]) -> Result<Map<String, Value>, SettingsRefusal> {
    if raw.len() > MAX_SETTINGS_BYTES {
        return Err(SettingsRefusal::ResourceExhausted);
    }
    // One parse: duplicate members and syntax errors are malformed input.
    let value =
        strict_json::parse_without_duplicates(raw).map_err(|()| SettingsRefusal::InvalidInput)?;
    let Value::Object(_) = &value else {
        return Err(SettingsRefusal::InvalidInput);
    };
    check_limits(&value, 0)?;
    match value {
        Value::Object(settings) => Ok(settings),
        _ => Err(SettingsRefusal::InvalidInput),
    }
}

/// Python's `sys.get_int_max_str_digits()` default: `json.loads` raises
/// `ValueError` (reported as malformed input) for an integer literal with more
/// digits than this.
const MAX_INTEGER_LITERAL_DIGITS: usize = 4300;

fn check_limits(value: &Value, depth: usize) -> Result<(), SettingsRefusal> {
    if depth > MAX_JSON_DEPTH {
        return Err(SettingsRefusal::ResourceExhausted);
    }
    match value {
        Value::Number(number) => {
            let text = number.to_string();
            if text.contains(['.', 'e', 'E']) {
                // A float: Python's json yields inf for an overflowing one.
                if number.as_f64().is_none_or(|float| !float.is_finite()) {
                    return Err(SettingsRefusal::InvalidInput);
                }
            } else if text.trim_start_matches('-').len() > MAX_INTEGER_LITERAL_DIGITS {
                // An integer literal of any size that fits the digit limit is
                // a valid Python int, whether or not it fits an f64.
                return Err(SettingsRefusal::InvalidInput);
            }
        }
        Value::String(text) if text.len() > MAX_STRING_BYTES => {
            return Err(SettingsRefusal::ResourceExhausted);
        }
        Value::Array(items) => {
            for item in items {
                check_limits(item, depth + 1)?;
            }
        }
        Value::Object(members) => {
            for (key, item) in members {
                if key.len() > MAX_STRING_BYTES {
                    return Err(SettingsRefusal::ResourceExhausted);
                }
                check_limits(item, depth + 1)?;
            }
        }
        _ => {}
    }
    Ok(())
}

fn decode_digest(hex: &str) -> Option<[u8; 32]> {
    let hex = hex.strip_prefix("sha256:").unwrap_or(hex);
    if hex.len() != 64 || !hex.is_ascii() {
        return None;
    }
    let mut out = [0_u8; 32];
    for (slot, pair) in out.iter_mut().zip(hex.as_bytes().chunks_exact(2)) {
        let pair = std::str::from_utf8(pair).ok()?;
        *slot = u8::from_str_radix(pair, 16).ok()?;
    }
    Some(out)
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    use subtle::ConstantTimeEq as _;
    left.len() == right.len() && left.ct_eq(right).unwrap_u8() == 1
}
