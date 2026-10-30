//! Output contracts (ADR-0043 decision 2, #1500).
//!
//! An agent version (`meta.output_contract`) or a pipeline LLM node
//! (`output_contract` in its YAML) can declare the shape its final answer must
//! have. The contract is one of:
//!
//! | `type` | Other field | The final answer must be |
//! | --- | --- | --- |
//! | `json_schema` | `schema` | a JSON document valid against the schema |
//! | `regex` | `pattern` | text that the pattern matches as a whole (Rust `regex` syntax; Main admits only patterns that compile here, see `libs/proto/contracts/output-contract-regex-v1.json`) |
//! | `format` | `format` | `json` (any JSON document), `markdown` (non-empty, every code fence closed) or `csv` (RFC 4180, the same column count on every row) |
//!
//! Main checks a contract when the version is saved (the same bounds, see
//! `services/elitea-main/internal/domain/outputcontract`). The worker admits it
//! again before a model is bound, sends a JSON Schema as the provider's
//! `response_format` where the provider takes one, validates the final
//! answer, retries once with the validation error as feedback, and ends the
//! run or node with [`VIOLATION_CODE`] when the second answer fails too.
//!
//! A [`Violation`] has two views: [`Violation::feedback`] is the message the
//! model sees on the retry (it may quote the answer), and
//! [`Violation::keyword`] with [`Violation::path`] is the data-free part a
//! step record carries.

use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use serde_json::Value;

/// The agent version meta key and the pipeline LLM node field.
pub const META_KEY: &str = "output_contract";
/// The upstream error code of a run or node that ended on a contract
/// violation. The worker maps it to the registered terminal failure that
/// Main shows as `OUTPUT_CONTRACT_VIOLATION`.
pub const VIOLATION_CODE: &str = "output_contract.violation";
/// The largest encoded JSON Schema.
pub const MAX_SCHEMA_BYTES: usize = 32 * 1024;
/// The deepest nesting of objects and arrays in a JSON Schema.
pub const MAX_SCHEMA_DEPTH: usize = 32;
/// The longest regex pattern, in bytes.
pub const MAX_PATTERN_BYTES: usize = 1024;
/// The compiled size limit of a regex pattern.
const MAX_REGEX_SIZE: usize = 1 << 20;
/// The longest feedback message sent to the model.
const MAX_FEEDBACK_BYTES: usize = 2 * 1024;
/// The longest instance path a step record carries.
const MAX_PATH_BYTES: usize = 256;
/// How many validation attempts a contract gets: the first answer and one
/// retry.
pub const MAX_ATTEMPTS: u32 = 2;

/// The contract types.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ContractType {
    JsonSchema,
    Regex,
    Format,
}

impl ContractType {
    /// The type as the stored contract spells it.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::JsonSchema => "json_schema",
            Self::Regex => "regex",
            Self::Format => "format",
        }
    }
}

/// The named formats.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NamedFormat {
    Json,
    Markdown,
    Csv,
}

impl NamedFormat {
    fn parse(value: &str) -> Option<Self> {
        match value {
            "json" => Some(Self::Json),
            "markdown" => Some(Self::Markdown),
            "csv" => Some(Self::Csv),
            _ => None,
        }
    }

    /// The format as the stored contract spells it.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Json => "json",
            Self::Markdown => "markdown",
            Self::Csv => "csv",
        }
    }
}

/// Why a stored contract is refused. The text is data-free.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum ContractError {
    #[error("the output contract must be an object with a known type")]
    Shape,
    #[error("the output contract JSON Schema is too large or too deep")]
    SchemaTooLarge,
    #[error("the output contract JSON Schema is not a valid schema")]
    SchemaInvalid,
    #[error("the output contract regex is too long")]
    PatternTooLong,
    #[error("the output contract regex does not compile")]
    PatternInvalid,
    #[error("the output contract format is not json, markdown or csv")]
    FormatUnknown,
}

#[derive(Clone)]
enum Rule {
    JsonSchema {
        schema: Value,
        validator: Arc<jsonschema::Validator>,
    },
    Regex(regex::Regex),
    Format(NamedFormat),
}

/// One admitted output contract.
#[derive(Clone)]
pub struct OutputContract {
    rule: Rule,
}

impl std::fmt::Debug for OutputContract {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("OutputContract")
            .field("type", &self.contract_type())
            .finish_non_exhaustive()
    }
}

/// Why one answer does not meet its contract.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Violation {
    keyword: String,
    path: String,
    feedback: String,
}

impl Violation {
    fn new(keyword: &str, path: &str, feedback: &str) -> Self {
        Self {
            keyword: cut(keyword, 64),
            path: cut(path, MAX_PATH_BYTES),
            feedback: cut(feedback, MAX_FEEDBACK_BYTES),
        }
    }

    /// An answer the provider stopped at its output token limit: the
    /// contract cannot be checked on a part of an answer.
    #[must_use]
    pub fn output_limit() -> Self {
        Self::new(
            "output_limit",
            "",
            "Your answer reached the output token limit before it was complete. Give a shorter answer that meets the required format.",
        )
    }

    /// The failed rule: a JSON Schema keyword (`required`, `type`, ...),
    /// `json` for an answer that is not JSON, `regex`, `markdown`, `csv` or
    /// `empty`. Data-free.
    #[must_use]
    pub fn keyword(&self) -> &str {
        &self.keyword
    }

    /// The JSON pointer of the failing value inside the answer (`""` is the
    /// whole answer); empty for the other types.
    #[must_use]
    pub fn path(&self) -> &str {
        &self.path
    }

    /// The message the model gets on the retry. It may quote the answer, so
    /// it never goes into a step record or a log.
    #[must_use]
    pub fn feedback(&self) -> &str {
        &self.feedback
    }
}

impl OutputContract {
    /// Admit a stored contract. `None` and `null` are no contract.
    ///
    /// # Errors
    ///
    /// [`ContractError`] when the value is not a contract this runtime can
    /// enforce.
    pub fn from_value(value: Option<&Value>) -> Result<Option<Self>, ContractError> {
        let Some(value) = value.filter(|value| !value.is_null()) else {
            return Ok(None);
        };
        let object = value.as_object().ok_or(ContractError::Shape)?;
        let kind = object
            .get("type")
            .and_then(Value::as_str)
            .ok_or(ContractError::Shape)?;
        let allowed: &[&str] = match kind {
            "json_schema" => &["type", "schema"],
            "regex" => &["type", "pattern"],
            "format" => &["type", "format"],
            _ => return Err(ContractError::Shape),
        };
        if object.keys().any(|key| !allowed.contains(&key.as_str())) || object.len() != 2 {
            return Err(ContractError::Shape);
        }
        let rule = match kind {
            "json_schema" => {
                let schema = object.get("schema").ok_or(ContractError::Shape)?;
                if !schema.is_object() {
                    return Err(ContractError::Shape);
                }
                let encoded = serde_json::to_vec(schema).map_err(|_| ContractError::Shape)?;
                if encoded.len() > MAX_SCHEMA_BYTES || depth(schema) > MAX_SCHEMA_DEPTH {
                    return Err(ContractError::SchemaTooLarge);
                }
                if refers_outside(schema) {
                    return Err(ContractError::SchemaInvalid);
                }
                jsonschema::meta::validate(schema).map_err(|_| ContractError::SchemaInvalid)?;
                let validator =
                    jsonschema::validator_for(schema).map_err(|_| ContractError::SchemaInvalid)?;
                Rule::JsonSchema {
                    schema: schema.clone(),
                    validator: Arc::new(validator),
                }
            }
            "regex" => {
                let pattern = object
                    .get("pattern")
                    .and_then(Value::as_str)
                    .ok_or(ContractError::Shape)?;
                if pattern.is_empty() || pattern.contains('\0') {
                    return Err(ContractError::PatternInvalid);
                }
                if pattern.len() > MAX_PATTERN_BYTES {
                    return Err(ContractError::PatternTooLong);
                }
                let regex = regex::RegexBuilder::new(&format!("^(?:{pattern})$"))
                    .size_limit(MAX_REGEX_SIZE)
                    .dfa_size_limit(MAX_REGEX_SIZE)
                    .build()
                    .map_err(|_| ContractError::PatternInvalid)?;
                Rule::Regex(regex)
            }
            _ => {
                let format = object
                    .get("format")
                    .and_then(Value::as_str)
                    .and_then(NamedFormat::parse)
                    .ok_or(ContractError::FormatUnknown)?;
                Rule::Format(format)
            }
        };
        Ok(Some(Self { rule }))
    }

    /// The contract's type.
    #[must_use]
    pub const fn contract_type(&self) -> ContractType {
        match self.rule {
            Rule::JsonSchema { .. } => ContractType::JsonSchema,
            Rule::Regex(_) => ContractType::Regex,
            Rule::Format(_) => ContractType::Format,
        }
    }

    /// The JSON Schema a provider takes as its `response_format`, for a
    /// `json_schema` contract.
    #[must_use]
    pub fn response_schema(&self) -> Option<&Value> {
        match &self.rule {
            Rule::JsonSchema { schema, .. } => Some(schema),
            _ => None,
        }
    }

    /// Validate one final answer.
    ///
    /// # Errors
    ///
    /// The [`Violation`] when the answer does not meet the contract.
    pub fn validate(&self, text: &str) -> Result<(), Violation> {
        if text.trim().is_empty() {
            return Err(Violation::new(
                "empty",
                "",
                "The answer is empty. Answer again with output that meets the required format.",
            ));
        }
        match &self.rule {
            Rule::JsonSchema { validator, .. } => {
                let value = parse_json(text)?;
                if let Some(error) = validator.iter_errors(&value).next() {
                    return Err(Violation::new(
                        error.kind().keyword(),
                        &error.instance_path().to_string(),
                        &format!(
                            "The answer does not match the required JSON Schema: {error} \
                             (at \"{}\"). Answer again with only a JSON document that matches \
                             the schema, without any other text.",
                            error.instance_path()
                        ),
                    ));
                }
                Ok(())
            }
            Rule::Regex(regex) => {
                if regex.is_match(text) {
                    Ok(())
                } else {
                    Err(Violation::new(
                        "regex",
                        "",
                        &format!(
                            "The answer does not match the required pattern {}. Answer again \
                             with only text that matches the whole pattern.",
                            regex.as_str()
                        ),
                    ))
                }
            }
            Rule::Format(NamedFormat::Json) => parse_json(text).map(|_| ()),
            Rule::Format(NamedFormat::Markdown) => validate_markdown(text),
            Rule::Format(NamedFormat::Csv) => validate_csv(text),
        }
    }
}

/// Whether a JSON Schema meets the strict structured output rules providers
/// apply (`OpenAI` `strict: true`, Anthropic `output_config.format`): every
/// object schema lists all its properties in `required` and sets
/// `additionalProperties: false`. A schema that does not is still enforced
/// by validation; it is sent to an `OpenAI`-compatible provider without
/// `strict` and not sent to Anthropic.
#[must_use]
pub fn is_strict_compatible(schema: &Value) -> bool {
    let mut stack = vec![schema];
    while let Some(value) = stack.pop() {
        match value {
            Value::Object(object) => {
                if let Some(Value::Object(properties)) = object.get("properties") {
                    let required = object
                        .get("required")
                        .and_then(Value::as_array)
                        .map(|names| names.iter().filter_map(Value::as_str).collect::<Vec<_>>())
                        .unwrap_or_default();
                    if object.get("additionalProperties") != Some(&Value::Bool(false))
                        || properties
                            .keys()
                            .any(|key| !required.contains(&key.as_str()))
                    {
                        return false;
                    }
                }
                if object.get("type").and_then(Value::as_str) == Some("object")
                    && !object.contains_key("properties")
                {
                    return false;
                }
                stack.extend(object.values());
            }
            Value::Array(items) => stack.extend(items),
            _ => {}
        }
    }
    true
}

/// The error a run or node ends with on a second failed answer.
#[must_use]
pub fn violation_error() -> AdkError {
    AdkError::new(
        ErrorComponent::Guardrail,
        ErrorCategory::InvalidInput,
        VIOLATION_CODE,
        "the final answer did not meet the output contract after one retry",
    )
    .with_retry(RetryHint {
        should_retry: false,
        retry_after_ms: None,
        max_attempts: Some(1),
    })
}

fn parse_json(text: &str) -> Result<Value, Violation> {
    serde_json::from_str::<Value>(text.trim()).map_err(|error| {
        Violation::new(
            "json",
            "",
            &format!(
                "The answer is not a JSON document ({error}). Answer again with only a JSON \
                 document, without any other text or code fence."
            ),
        )
    })
}

fn validate_markdown(text: &str) -> Result<(), Violation> {
    let mut open: Option<(char, usize)> = None;
    for line in text.lines() {
        let trimmed = line.trim_start();
        for fence in ['`', '~'] {
            let run = trimmed.chars().take_while(|c| *c == fence).count();
            if run < 3 {
                continue;
            }
            match open {
                None => open = Some((fence, run)),
                Some((current, length))
                    if current == fence && run >= length && trimmed[run..].trim().is_empty() =>
                {
                    open = None;
                }
                Some(_) => {}
            }
        }
    }
    if open.is_some() {
        return Err(Violation::new(
            "markdown",
            "",
            "The answer has a code fence that is not closed. Answer again as Markdown with \
             every code fence closed.",
        ));
    }
    Ok(())
}

fn validate_csv(text: &str) -> Result<(), Violation> {
    let failure = |reason: &str| {
        Violation::new(
            "csv",
            "",
            &format!(
                "The answer is not valid CSV: {reason}. Answer again with only CSV text: one \
                 record per line, the same number of fields on every line, and fields that \
                 hold a comma, a quote or a line break in double quotes."
            ),
        )
    };
    let mut columns: Option<usize> = None;
    let mut fields = 1usize;
    let mut quoted = false;
    let mut field_start = true;
    let mut row = 1usize;
    let mut chars = text.trim_end_matches(['\r', '\n']).chars().peekable();
    let mut end_row = |fields: usize, row: usize| -> Result<(), Violation> {
        match columns {
            None => columns = Some(fields),
            Some(expected) if expected != fields => {
                return Err(failure(&format!(
                    "line {row} has {fields} fields where the first line has {expected}"
                )));
            }
            Some(_) => {}
        }
        Ok(())
    };
    while let Some(c) = chars.next() {
        if quoted {
            if c == '"' {
                if chars.peek() == Some(&'"') {
                    chars.next();
                } else {
                    quoted = false;
                }
            }
            continue;
        }
        match c {
            '"' if field_start => {
                quoted = true;
                field_start = false;
            }
            '"' => return Err(failure(&format!("line {row} has a quote inside a field"))),
            ',' => {
                fields += 1;
                field_start = true;
            }
            '\r' if chars.peek() == Some(&'\n') => {}
            '\n' | '\r' => {
                end_row(fields, row)?;
                row += 1;
                fields = 1;
                field_start = true;
            }
            _ => field_start = false,
        }
    }
    if quoted {
        return Err(failure("a quoted field is not closed"));
    }
    end_row(fields, row)
}

/// The nesting depth of objects and arrays.
fn depth(value: &Value) -> usize {
    let mut deepest = 0;
    let mut stack = vec![(value, 1usize)];
    while let Some((value, level)) = stack.pop() {
        deepest = deepest.max(level);
        if level > MAX_SCHEMA_DEPTH {
            break;
        }
        match value {
            Value::Object(object) => stack.extend(object.values().map(|child| (child, level + 1))),
            Value::Array(items) => stack.extend(items.iter().map(|child| (child, level + 1))),
            _ => {}
        }
    }
    deepest
}

/// Whether a `$ref` or `$dynamicRef` points outside the schema: a contract
/// only refers to its own definitions (`#...`).
fn refers_outside(value: &Value) -> bool {
    let mut stack = vec![value];
    while let Some(value) = stack.pop() {
        match value {
            Value::Object(object) => {
                for (key, child) in object {
                    if matches!(key.as_str(), "$ref" | "$dynamicRef" | "$recursiveRef")
                        && child.as_str().is_none_or(|target| !target.starts_with('#'))
                    {
                        return true;
                    }
                    stack.push(child);
                }
            }
            Value::Array(items) => stack.extend(items),
            _ => {}
        }
    }
    false
}

fn cut(text: &str, max: usize) -> String {
    if text.len() <= max {
        return text.to_owned();
    }
    let mut end = max;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].to_owned()
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use serde_json::json;

    use super::*;

    fn contract(value: &Value) -> OutputContract {
        OutputContract::from_value(Some(value)).unwrap().unwrap()
    }

    #[test]
    fn absent_and_null_are_no_contract() {
        assert!(OutputContract::from_value(None).unwrap().is_none());
        assert!(
            OutputContract::from_value(Some(&Value::Null))
                .unwrap()
                .is_none()
        );
    }

    #[test]
    fn a_json_schema_contract_validates_the_answer() {
        let contract = contract(&json!({
            "type": "json_schema",
            "schema": {
                "type": "object",
                "properties": {
                    "answer": {"type": "string"},
                    "confidence": {"enum": ["low", "medium", "high"]}
                },
                "required": ["answer", "confidence"]
            }
        }));
        assert_eq!(contract.contract_type(), ContractType::JsonSchema);
        assert!(contract.response_schema().is_some());
        assert!(
            contract
                .validate(r#"{"answer": "4", "confidence": "high"}"#)
                .is_ok()
        );
        let prose = contract.validate("The answer is four.").unwrap_err();
        assert_eq!(prose.keyword(), "json");
        let missing = contract.validate(r#"{"answer": "4"}"#).unwrap_err();
        assert_eq!(missing.keyword(), "required");
        assert_eq!(missing.path(), "");
        let wrong = contract
            .validate(r#"{"answer": "4", "confidence": "sure"}"#)
            .unwrap_err();
        assert_eq!(wrong.keyword(), "enum");
        assert_eq!(wrong.path(), "/confidence");
        assert!(wrong.feedback().contains("sure"));
    }

    #[test]
    fn a_schema_is_checked_before_it_is_admitted() {
        for refused in [
            json!({"type": "json_schema", "schema": {"type": 7}}),
            json!({"type": "json_schema", "schema": {"$ref": "https://example.com/s.json"}}),
            json!({"type": "json_schema", "schema": "object"}),
            json!({"type": "json_schema", "schema": {}, "extra": 1}),
            json!({"type": "xml"}),
        ] {
            assert!(
                OutputContract::from_value(Some(&refused)).is_err(),
                "{refused}"
            );
        }
        let mut deep = json!({"type": "string"});
        for _ in 0..MAX_SCHEMA_DEPTH {
            deep = json!({"type": "array", "items": deep});
        }
        assert_eq!(
            OutputContract::from_value(Some(&json!({"type": "json_schema", "schema": deep})))
                .unwrap_err(),
            ContractError::SchemaTooLarge
        );
        assert!(
            OutputContract::from_value(Some(&json!({
                "type": "json_schema",
                "schema": {"$defs": {"a": {"type": "string"}}, "$ref": "#/$defs/a"}
            })))
            .is_ok()
        );
    }

    #[test]
    fn a_regex_contract_matches_the_whole_answer() {
        let contract = contract(&json!({"type": "regex", "pattern": "[A-Z]{3}-\\d+"}));
        assert!(contract.validate("ABC-12").is_ok());
        assert_eq!(
            contract.validate("see ABC-12").unwrap_err().keyword(),
            "regex"
        );
        assert_eq!(
            OutputContract::from_value(Some(&json!({"type": "regex", "pattern": "(a"})))
                .unwrap_err(),
            ContractError::PatternInvalid
        );
        assert_eq!(
            OutputContract::from_value(Some(&json!({"type": "regex", "pattern": "(a)\\1"})))
                .unwrap_err(),
            ContractError::PatternInvalid
        );
        let long = "a".repeat(MAX_PATTERN_BYTES + 1);
        assert_eq!(
            OutputContract::from_value(Some(&json!({"type": "regex", "pattern": long})))
                .unwrap_err(),
            ContractError::PatternTooLong
        );
    }

    /// The patterns of `libs/proto/contracts/output-contract-regex-v1.json`.
    /// Main's test (`services/elitea-main/internal/domain/outputcontract`)
    /// reads the same file: a pattern that Main accepts on save compiles
    /// here.
    #[test]
    fn regex_contracts_match_the_shared_fixture() {
        let fixture: Value = serde_json::from_str(include_str!(
            "../../../proto/contracts/output-contract-regex-v1.json"
        ))
        .unwrap();
        assert_eq!(fixture["schema"], "elitea.output-contract-regex.v1");
        let cases = fixture["cases"].as_array().unwrap();
        assert_ne!(cases.len(), 0);
        for case in cases {
            let pattern = case["pattern"].as_str().unwrap();
            let worker = case["worker"].as_bool().unwrap();
            let main = case["main"].as_bool().unwrap();
            assert!(
                worker || !main,
                "Main must not accept {pattern:?}: the worker refuses it"
            );
            let compiled =
                OutputContract::from_value(Some(&json!({"type": "regex", "pattern": pattern})));
            assert_eq!(
                compiled.is_ok(),
                worker,
                "{pattern:?} ({}): {:?}",
                case["note"],
                compiled.as_ref().err()
            );
            if let Err(error) = compiled {
                assert_eq!(error, ContractError::PatternInvalid, "{pattern:?}");
            }
        }
    }

    #[test]
    fn named_formats() {
        let json_format = contract(&json!({"type": "format", "format": "json"}));
        assert!(json_format.validate("[1, 2]").is_ok());
        assert!(json_format.validate("```json\n[1]\n```").is_err());
        let markdown = contract(&json!({"type": "format", "format": "markdown"}));
        assert!(
            markdown
                .validate("# Title\n\n```rust\nfn x() {}\n```\n")
                .is_ok()
        );
        assert_eq!(
            markdown
                .validate("# Title\n```rust\nfn x() {}\n")
                .unwrap_err()
                .keyword(),
            "markdown"
        );
        assert_eq!(markdown.validate("  \n").unwrap_err().keyword(), "empty");
        let csv = contract(&json!({"type": "format", "format": "csv"}));
        assert!(
            csv.validate("name,note\r\nann,\"a, \"\"b\"\"\"\nbob,\"two\nlines\"\n")
                .is_ok()
        );
        assert!(csv.validate("a,b\n1\n").is_err());
        assert!(csv.validate("a,b\n\"1,2\n").is_err());
        assert!(csv.validate("a,b\n1x\"y,2\n").is_err());
        assert_eq!(
            OutputContract::from_value(Some(&json!({"type": "format", "format": "xml"})))
                .unwrap_err(),
            ContractError::FormatUnknown
        );
    }

    #[test]
    fn strict_compatibility() {
        assert!(is_strict_compatible(&json!({
            "type": "object",
            "properties": {"a": {"type": "string"}},
            "required": ["a"],
            "additionalProperties": false
        })));
        assert!(!is_strict_compatible(&json!({
            "type": "object",
            "properties": {"a": {"type": "string"}},
            "required": ["a"]
        })));
        assert!(!is_strict_compatible(
            &json!({"type": "object", "required": ["a"]})
        ));
        assert!(is_strict_compatible(
            &json!({"type": "array", "items": {"type": "string"}})
        ));
    }

    #[test]
    fn the_violation_error_is_typed_and_not_retryable() {
        let error = violation_error();
        assert_eq!(error.code, VIOLATION_CODE);
        assert!(!error.is_retryable());
    }
}
