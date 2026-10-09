use std::fmt;
use std::net::IpAddr;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_HOST_BYTES: usize = 253;
const MAX_USERNAME_BYTES: usize = 256;
const MAX_PASSWORD_BYTES: usize = 16 * 1_024;
const MAX_DATABASE_BYTES: usize = 256;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SqlDialect {
    Postgres,
    MySql,
}

impl SqlDialect {
    #[must_use]
    pub(crate) const fn default_port(self) -> u16 {
        match self {
            Self::Postgres => 5432,
            Self::MySql => 3306,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SqlConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that retains no endpoint or credential data.
pub(crate) struct SqlConfigError {
    code: SqlConfigErrorCode,
}

impl SqlConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> SqlConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for SqlConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SqlConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SqlConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SqlConfigErrorCode::InvalidConfiguration => "the SQL toolkit configuration is invalid",
            SqlConfigErrorCode::ResourceExhausted => {
                "the SQL toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for SqlConfigError {}

/// One claim-materialized SQL authority.
///
/// The password is intentionally neither cloneable nor debug-printable. No
/// connection is opened while this value or the ADK tool definitions are built.
pub(crate) struct SqlToolkitConfig {
    dialect: SqlDialect,
    host: Box<str>,
    port: u16,
    username: Box<str>,
    password: Zeroizing<String>,
    database: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl SqlToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, SqlConfigError> {
        let dialect = match settings.get("dialect") {
            None | Some(Value::Null) => SqlDialect::Postgres,
            Some(Value::String(value)) if value == "postgres" => SqlDialect::Postgres,
            Some(Value::String(value)) if value == "mysql" => SqlDialect::MySql,
            _ => return Err(invalid_configuration()),
        };
        let configuration = settings
            .get("sql_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let host = required_text(configuration, "host", MAX_HOST_BYTES)?;
        validate_host(host)?;
        let username = required_text(configuration, "username", MAX_USERNAME_BYTES)?;
        let password = configuration
            .get("password")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_secret(password)?;
        let database = required_text(settings, "database_name", MAX_DATABASE_BYTES)?;
        let port = parse_port(configuration.get("port"), dialect.default_port())?;
        Ok(Self {
            dialect,
            host: host.into(),
            port,
            username: username.into(),
            password: Zeroizing::new(password.to_owned()),
            database: database.into(),
            selected_tools: selected_tools(settings)?,
        })
    }

    #[must_use]
    pub(crate) const fn dialect(&self) -> SqlDialect {
        self.dialect
    }

    #[must_use]
    pub(super) fn host(&self) -> &str {
        &self.host
    }

    #[must_use]
    pub(super) const fn port(&self) -> u16 {
        self.port
    }

    #[must_use]
    pub(super) fn username(&self) -> &str {
        &self.username
    }

    #[must_use]
    pub(super) fn password(&self) -> &str {
        &self.password
    }

    #[must_use]
    pub(super) fn database(&self) -> &str {
        &self.database
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_port(value: Option<&Value>, default: u16) -> Result<u16, SqlConfigError> {
    let port = match value {
        None | Some(Value::Null) => return Ok(default),
        Some(Value::Number(value)) => value.as_u64().ok_or_else(invalid_configuration)?,
        _ => return Err(invalid_configuration()),
    };
    u16::try_from(port)
        .ok()
        .filter(|port| *port != 0)
        .ok_or_else(invalid_configuration)
}

fn validate_host(value: &str) -> Result<(), SqlConfigError> {
    if value.parse::<IpAddr>().is_ok() {
        return Ok(());
    }
    if value.is_empty()
        || value.ends_with('.')
        || value.split('.').any(|label| {
            label.is_empty()
                || label.len() > 63
                || label.starts_with('-')
                || label.ends_with('-')
                || !label
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn validate_secret(value: &str) -> Result<(), SqlConfigError> {
    if value.len() > MAX_PASSWORD_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.contains('\0') {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, SqlConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, SqlConfigError> {
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
    let mut selected = Vec::with_capacity(values.len().min(2));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?;
        if name.is_empty() || name.len() > MAX_TOOL_NAME_BYTES || name.chars().any(char::is_control)
        {
            return Err(invalid_configuration());
        }
        if !selected
            .iter()
            .map(AsRef::as_ref)
            .any(|existing: &str| existing == name)
        {
            selected.push(name.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> SqlConfigError {
    SqlConfigError {
        code: SqlConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> SqlConfigError {
    SqlConfigError {
        code: SqlConfigErrorCode::ResourceExhausted,
    }
}
