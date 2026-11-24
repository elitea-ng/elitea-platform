//! Admitted HTTP Request node (`type: http`, `revision: 2`) contract,
//! canonical input binding, deterministic identity derivation, and reference
//! renderer (T-5D, step 5d-2).
//!
//! This module performs no I/O, holds no credentials, and never touches the
//! network. Main owns the live rule, credentials, rendering, egress guard, and
//! effect ledger; the Worker uses this module to parse and validate node YAML,
//! resolve canonical inputs from checkpointed state, derive `activation_id` and
//! `effect_id` v3, decode v2 receipts, and prove byte-for-byte parity against
//! `libs/jsonschema/runtime/v1/fixtures/http-action-render-vectors-v1.json`.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use thiserror::Error;

use super::PIPELINE_YAML_BUDGET;
use super::yaml::{valid_graph_id, valid_output_key};
use crate::bounded_yaml;
use crate::canonical;

/// Node YAML schema `$id`.
pub const HTTP_ACTION_NODE_SCHEMA: &str = "elitea.pipeline.http-action-node.v2";
/// Worker-to-Main invocation schema version (v3).
pub const HTTP_ACTION_INVOCATION_SCHEMA: &str = "elitea.runtime.http-action.v3";
/// Main-to-Worker receipt schema version (v2).
pub const HTTP_ACTION_RECEIPT_SCHEMA: &str = "elitea.runtime.http-action-receipt.v2";
/// Start-time frozen snapshot schema version (v2).
pub const HTTP_ACTION_SNAPSHOT_SCHEMA: &str = "elitea.runtime.http-action-snapshot.v2";
/// Admin-authored rule body schema version (v1).
pub const HTTP_ACTION_RULE_SCHEMA: &str = "elitea.runtime.http-action-rule.v1";

/// Named bounds matching Go `services/elitea-main/internal/application/httpaction`
/// and T-5D §3.4.
pub const MAX_HTTP_INPUTS_BYTES: usize = 256 * 1024;
pub const MAX_HTTP_INVOCATION_BYTES: usize = 768 * 1024;
pub const MAX_HTTP_REQUEST_BYTES: usize = 384 * 1024;
pub const MAX_HTTP_RESPONSE_BYTES: u64 = 2 * 1024 * 1024;
pub const MAX_HTTP_INLINE_BYTES: u64 = 512 * 1024;
pub const MAX_HTTP_RECEIPT_BYTES: usize = 3 * 1024 * 1024;
pub const MAX_HTTP_TIMEOUT_MS: u32 = 30_000;
pub const MAX_HTTP_HEADERS: usize = 32;
pub const MAX_HTTP_HEADER_NAME_BYTES: usize = 128;
pub const MAX_HTTP_HEADER_VALUE_BYTES: usize = 8_192;
pub const MAX_HTTP_HEADERS_TOTAL_BYTES: usize = 16_384;
pub const MAX_HTTP_QUERY_NAMES: usize = 32;
pub const MAX_HTTP_PATH_PLACEHOLDERS: usize = 32;
pub const MAX_HTTP_SEGMENT_BYTES: usize = 1_024;
pub const MAX_HTTP_TEMPLATE_BYTES: usize = 64 * 1024;
pub const MAX_HTTP_RAW_NODE_BYTES: usize = 2 * MAX_HTTP_TEMPLATE_BYTES;
pub const MAX_HTTP_ATTEMPTS: u32 = 16;
pub const MAX_HTTP_IDEMPOTENCY_KEY_BYTES: usize = 128;
pub const MAX_HTTP_ACCEPTED_STATUSES: usize = 16;
pub const MAX_HTTP_BODY_BYTES: usize = 256 * 1024;
pub const MAX_HTTP_LITERAL_STRING_BYTES: usize = 1_024;
pub const MAX_HTTP_RULE_NAME_BYTES: usize = 128;
pub const MAX_HTTP_RULES_PER_PROJECT: usize = 256;
pub const MAX_HTTP_URL_BYTES: usize = 8_192;
pub const MAX_HTTP_INPUT_KEYS: usize = 64;
const MAX_HTTP_JSON_DEPTH: usize = 32;
const MAX_HTTP_JSON_NODES: usize = 16_384;
const MAX_HTTP_ARTIFACT_REF_BYTES: usize = 1_024;
const MAX_HTTP_PATH_TEMPLATE_BYTES: usize = 2_048;

const CONFIG_DIGEST_DOMAIN: &[u8] = b"elitea.graph.http.config.v2\0";
const ACTIVATION_DOMAIN: &[u8] = b"elitea.graph.http.activation.v1\0";
const EFFECT_V3_DOMAIN: &[u8] = b"elitea.http.effect.v3\0";
const FROZEN_NODE_V2_DOMAIN: &[u8] = b"elitea.runtime.http-frozen-node.v2\0";

/// Shared error vocabulary for HTTP action runtime and saved-family authority ports.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HttpActionError {
    InvalidInput,
    ResourceExhausted,
    Authentication,
    Authorization,
    RedirectRefused,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    UnexpectedStatus(u16),
    PolicyDenied,
    ReconciliationRequired,
}

impl fmt::Display for HttpActionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::InvalidInput => "the HTTP action input is invalid",
            Self::ResourceExhausted => "the HTTP action exceeds its approved bounds",
            Self::Authentication => "HTTP endpoint authentication failed",
            Self::Authorization => "the HTTP endpoint refused authorization",
            Self::RedirectRefused => "the HTTP action refuses redirects",
            Self::RateLimited => "the HTTP endpoint rate limited the request",
            Self::Timeout => "the HTTP action timed out",
            Self::DependencyUnavailable => "the HTTP endpoint is unavailable",
            Self::InvalidResponse => "the HTTP endpoint returned an invalid response",
            Self::UnexpectedStatus(_) => "the HTTP status does not meet the response contract",
            Self::PolicyDenied => "the HTTP action is refused by platform policy",
            Self::ReconciliationRequired => {
                "HTTP completion is uncertain; reconcile the existing effect before continuing"
            }
        })
    }
}

impl std::error::Error for HttpActionError {}

/// Typed node-definition parsing and admission failure.
#[derive(Debug, Error)]
pub enum HttpActionConfigurationError {
    #[error("the HTTP action node exceeds its size bound")]
    ResourceExhausted,
    #[error("the HTTP action node YAML is malformed")]
    MalformedYaml {
        #[source]
        source: serde_yaml_ng::Error,
    },
    #[error("the HTTP action node is invalid: {0}")]
    Invalid(&'static str),
}

/// Scalar literal permitted in path, query, header, or idempotency key bindings.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
#[serde(untagged)]
pub enum HttpScalarLiteral {
    String(String),
    Int(i64),
    Bool(bool),
}

impl HttpScalarLiteral {
    fn to_json_value(&self) -> Value {
        match self {
            Self::String(s) => Value::String(s.clone()),
            Self::Int(n) => Value::Number((*n).into()),
            Self::Bool(b) => Value::Bool(*b),
        }
    }

    fn as_scalar_text(&self) -> String {
        match self {
            Self::String(s) => s.clone(),
            Self::Int(n) => n.to_string(),
            Self::Bool(b) => b.to_string(),
        }
    }
}

/// `{from: state_key, optional?: bool}` or `{value: scalar}`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum HttpBinding {
    From { key: String, optional: bool },
    Literal(HttpScalarLiteral),
}

/// `{name, from, optional?}` or `{name, value}`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct HttpHeaderBinding {
    pub name: String,
    pub binding: HttpBinding,
}

/// `{kind: empty}`, `{kind: json|text, from|value, content_type?}`, or
/// `{kind: artifact, from, content_type}`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum HttpBodyTemplate {
    Empty,
    JsonFrom {
        key: String,
        content_type: Option<String>,
    },
    JsonLiteral {
        value: Value,
        content_type: Option<String>,
    },
    TextFrom {
        key: String,
        content_type: Option<String>,
    },
    TextLiteral {
        value: String,
        content_type: Option<String>,
    },
    ArtifactFrom {
        key: String,
        content_type: String,
    },
}

/// Inclusive HTTP status code range `[first, last]` inside `200..=599`
/// excluding `3xx`, `401`, and `403`.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpStatusRange {
    pub first: u16,
    pub last: u16,
}

/// Response projection mode.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum HttpResponseMode {
    Json,
    Text,
    Artifact,
}

impl HttpResponseMode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Json => "json",
            Self::Text => "text",
            Self::Artifact => "artifact",
        }
    }
}

/// Response contract of one HTTP action node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct HttpResponseContract {
    pub mode: HttpResponseMode,
    pub accepted_statuses: Option<Vec<HttpStatusRange>>,
    pub max_bytes: Option<u64>,
}

/// Request bindings template of one HTTP action node.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct HttpRequestTemplate {
    pub path: BTreeMap<String, HttpBinding>,
    pub query: BTreeMap<String, HttpBinding>,
    pub headers: Vec<HttpHeaderBinding>,
    pub body: Option<HttpBodyTemplate>,
    pub idempotency_key: Option<HttpBinding>,
}

/// Validated `type: http`, `revision: 2` node definition.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct HttpActionNodeDefinition {
    id: String,
    operation: String,
    request: HttpRequestTemplate,
    response: HttpResponseContract,
    timeout_ms: Option<u32>,
    input_keys: Vec<String>,
    output: Vec<String>,
    transition: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawHttpNode {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    revision: u64,
    operation: String,
    request: RawHttpRequest,
    response: RawHttpResponse,
    timeout_ms: Option<u64>,
    #[serde(default)]
    input: Vec<String>,
    output: Vec<String>,
    transition: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawHttpRequest {
    #[serde(default)]
    path: BTreeMap<String, RawHttpBinding>,
    #[serde(default)]
    query: BTreeMap<String, RawHttpBinding>,
    #[serde(default)]
    headers: Vec<RawHttpHeaderBinding>,
    body: Option<RawHttpBody>,
    idempotency_key: Option<RawHttpBinding>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawHttpBinding {
    from: Option<String>,
    optional: Option<bool>,
    value: Option<Value>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawHttpHeaderBinding {
    name: String,
    from: Option<String>,
    optional: Option<bool>,
    value: Option<Value>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawHttpBody {
    kind: String,
    from: Option<String>,
    value: Option<Value>,
    content_type: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawHttpResponse {
    mode: String,
    accepted_statuses: Option<Vec<HttpStatusRange>>,
    max_bytes: Option<u64>,
}

impl HttpActionNodeDefinition {
    /// Parse and validate one `type: http`, `revision: 2` node from YAML.
    pub fn from_yaml(yaml: &str) -> Result<Self, HttpActionConfigurationError> {
        if yaml.is_empty() || yaml.len() > MAX_HTTP_RAW_NODE_BYTES {
            return Err(HttpActionConfigurationError::ResourceExhausted);
        }
        let raw = bounded_yaml::from_str_as_yaml_error::<RawHttpNode>(yaml, PIPELINE_YAML_BUDGET)
            .map_err(|source| HttpActionConfigurationError::MalformedYaml { source })?;
        Self::from_raw(raw)
    }

    /// Parse and validate one `type: http`, `revision: 2` node from JSON.
    pub fn from_json_str(json: &str) -> Result<Self, HttpActionConfigurationError> {
        if json.is_empty() || json.len() > MAX_HTTP_RAW_NODE_BYTES {
            return Err(HttpActionConfigurationError::ResourceExhausted);
        }
        let raw: RawHttpNode = serde_json::from_str(json)
            .map_err(|_| HttpActionConfigurationError::Invalid("the HTTP node JSON is invalid"))?;
        Self::from_raw(raw)
    }

    #[allow(
        clippy::too_many_lines,
        reason = "Keep strict schema and limit validation in one linear pass."
    )]
    fn from_raw(raw: RawHttpNode) -> Result<Self, HttpActionConfigurationError> {
        if raw.node_type != "http" {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP node type must be 'http'",
            ));
        }
        if raw.revision != 2 {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP node revision must be 2",
            ));
        }
        if !valid_graph_id(&raw.id) {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP node ID is malformed",
            ));
        }
        if !valid_operation_name(&raw.operation) {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP node operation name is malformed",
            ));
        }
        if raw.output.len() != 1 || !valid_http_output_key(&raw.output[0]) {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP node must declare exactly one valid output key",
            ));
        }
        if raw
            .transition
            .as_deref()
            .is_some_and(|target| target != "END" && !valid_graph_id(target))
        {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP node transition is malformed",
            ));
        }
        let timeout_ms = match raw.timeout_ms {
            None => None,
            Some(ms) if (1..=u64::from(MAX_HTTP_TIMEOUT_MS)).contains(&ms) => {
                Some(u32::try_from(ms).map_err(|_| {
                    HttpActionConfigurationError::Invalid("the HTTP timeout_ms is out of bounds")
                })?)
            }
            Some(_) => {
                return Err(HttpActionConfigurationError::Invalid(
                    "the HTTP node timeout_ms must be between 1 and 30000",
                ));
            }
        };
        if raw.input.len() > MAX_HTTP_INPUT_KEYS {
            return Err(HttpActionConfigurationError::ResourceExhausted);
        }
        let mut input_set = BTreeSet::new();
        for key in &raw.input {
            if !valid_output_key(key) || !input_set.insert(key.clone()) {
                return Err(HttpActionConfigurationError::Invalid(
                    "the HTTP node input keys must be unique valid state identifiers",
                ));
            }
        }
        let request = parse_request_template(raw.request)?;
        let response = parse_response_contract(raw.response)?;

        // Every `from` binding reads a state key; include all of them in `input_keys`
        // so compiler state validation catches undeclared bindings before execution.
        for binding in request.path.values().chain(request.query.values()) {
            if let HttpBinding::From { key, .. } = binding {
                input_set.insert(key.clone());
            }
        }
        for header in &request.headers {
            if let HttpBinding::From { key, .. } = &header.binding {
                input_set.insert(key.clone());
            }
        }
        if let Some(body) = &request.body {
            match body {
                HttpBodyTemplate::JsonFrom { key, .. }
                | HttpBodyTemplate::TextFrom { key, .. }
                | HttpBodyTemplate::ArtifactFrom { key, .. } => {
                    input_set.insert(key.clone());
                }
                HttpBodyTemplate::Empty
                | HttpBodyTemplate::JsonLiteral { .. }
                | HttpBodyTemplate::TextLiteral { .. } => {}
            }
        }
        if let Some(HttpBinding::From { key, .. }) = &request.idempotency_key {
            input_set.insert(key.clone());
        }

        let definition = Self {
            id: raw.id,
            operation: raw.operation,
            request,
            response,
            timeout_ms,
            input_keys: input_set.into_iter().collect(),
            output: raw.output,
            transition: raw.transition,
        };
        if definition.template_wire().len() > MAX_HTTP_TEMPLATE_BYTES {
            return Err(HttpActionConfigurationError::ResourceExhausted);
        }
        Ok(definition)
    }

    #[must_use]
    pub fn id(&self) -> &str {
        &self.id
    }

    #[must_use]
    pub fn operation(&self) -> &str {
        &self.operation
    }

    #[must_use]
    pub const fn request(&self) -> &HttpRequestTemplate {
        &self.request
    }

    #[must_use]
    pub const fn response(&self) -> &HttpResponseContract {
        &self.response
    }

    #[must_use]
    pub const fn timeout_ms(&self) -> Option<u32> {
        self.timeout_ms
    }

    #[must_use]
    pub fn input_keys(&self) -> &[String] {
        &self.input_keys
    }

    #[must_use]
    pub fn output_keys(&self) -> &[String] {
        &self.output
    }

    #[must_use]
    pub fn output_key(&self) -> &str {
        &self.output[0]
    }

    #[must_use]
    pub fn transition(&self) -> Option<&str> {
        self.transition.as_deref()
    }

    /// Canonical template JSON (`{operation, request, response, timeout_ms?}`)
    /// matching Go `httpaction/node.go` `Node.TemplateWire`.
    #[must_use]
    pub fn template_wire(&self) -> Vec<u8> {
        let mut root = Map::new();
        root.insert(
            "operation".to_owned(),
            Value::String(self.operation.clone()),
        );
        root.insert("request".to_owned(), request_template_value(&self.request));
        root.insert(
            "response".to_owned(),
            response_contract_value(&self.response),
        );
        if let Some(timeout_ms) = self.timeout_ms {
            root.insert("timeout_ms".to_owned(), Value::Number(timeout_ms.into()));
        }
        canonical::to_vec(&Value::Object(root)).unwrap_or_default()
    }

    /// Domain-separated configuration digest for pipeline checkpoint lineage.
    #[must_use]
    pub fn config_digest(&self) -> [u8; 32] {
        let mut context = digest::Context::new(&digest::SHA256);
        context.update(CONFIG_DIGEST_DOMAIN);
        digest_field(&mut context, self.id.as_bytes());
        digest_field(&mut context, self.operation.as_bytes());
        digest_field(&mut context, &self.template_wire());
        for key in &self.input_keys {
            digest_field(&mut context, key.as_bytes());
        }
        for key in &self.output {
            digest_field(&mut context, key.as_bytes());
        }
        digest_field(
            &mut context,
            self.transition.as_deref().unwrap_or_default().as_bytes(),
        );
        copy_sha256(context.finish().as_ref())
    }

    /// Resolve all `from` bindings from checkpointed pipeline state into
    /// canonical JSON bytes matching Go `httpaction.DecodeInputs`.
    ///
    /// Literals stay in the frozen template and are never included here.
    /// Optional `from` bindings whose state value is absent or `null` are
    /// omitted. Non-scalar path/query/header values, dot path segments,
    /// CR/LF/NUL headers, and payloads over [`MAX_HTTP_INPUTS_BYTES`] fail closed.
    pub fn resolve_canonical_inputs(
        &self,
        state: &BTreeMap<String, Value>,
    ) -> Result<ResolvedHttpInputs, HttpActionError> {
        let mut inputs = Map::new();

        if let Some(path_map) = self.resolve_path_inputs(state)? {
            inputs.insert("path".to_owned(), Value::Object(path_map));
        }
        if let Some(query_map) = self.resolve_query_inputs(state)? {
            inputs.insert("query".to_owned(), Value::Object(query_map));
        }
        if let Some(headers_map) = self.resolve_header_inputs(state)? {
            inputs.insert("headers".to_owned(), Value::Object(headers_map));
        }
        if let Some(body_value) = self.resolve_body_input(state)? {
            inputs.insert("body".to_owned(), body_value);
        }
        if let Some(idempotency_key) = self.resolve_idempotency_key_input(state)? {
            inputs.insert("idempotency_key".to_owned(), Value::String(idempotency_key));
        }

        let canonical_bytes =
            canonical::to_vec(&Value::Object(inputs)).map_err(|_| HttpActionError::InvalidInput)?;
        if canonical_bytes.len() > MAX_HTTP_INPUTS_BYTES {
            return Err(HttpActionError::ResourceExhausted);
        }
        let inputs_digest = sha256(&canonical_bytes);
        let inputs_digest_hex = hex_encode(&inputs_digest);
        let inputs_b64 = BASE64_STANDARD.encode(&canonical_bytes);
        Ok(ResolvedHttpInputs {
            canonical_bytes,
            inputs_b64,
            inputs_digest,
            inputs_digest_hex,
        })
    }

    fn resolve_path_inputs(
        &self,
        state: &BTreeMap<String, Value>,
    ) -> Result<Option<Map<String, Value>>, HttpActionError> {
        let mut path_map = Map::new();
        for (name, binding) in &self.request.path {
            if let HttpBinding::From { key, optional } = binding {
                match state.get(key) {
                    None | Some(Value::Null) if *optional => {}
                    None | Some(Value::Null) => return Err(HttpActionError::InvalidInput),
                    Some(value) => {
                        let text = scalar_text_from_value(value)?;
                        if text.is_empty()
                            || text == "."
                            || text == ".."
                            || text.len() > MAX_HTTP_SEGMENT_BYTES
                        {
                            return Err(HttpActionError::InvalidInput);
                        }
                        path_map.insert(name.clone(), value.clone());
                    }
                }
            }
        }
        Ok((!path_map.is_empty()).then_some(path_map))
    }

    fn resolve_query_inputs(
        &self,
        state: &BTreeMap<String, Value>,
    ) -> Result<Option<Map<String, Value>>, HttpActionError> {
        let mut query_map = Map::new();
        for (name, binding) in &self.request.query {
            if let HttpBinding::From { key, optional } = binding {
                match state.get(key) {
                    None | Some(Value::Null) if *optional => {}
                    None | Some(Value::Null) => return Err(HttpActionError::InvalidInput),
                    Some(value) => {
                        let _ = scalar_text_from_value(value)?;
                        query_map.insert(name.clone(), value.clone());
                    }
                }
            }
        }
        Ok((!query_map.is_empty()).then_some(query_map))
    }

    fn resolve_header_inputs(
        &self,
        state: &BTreeMap<String, Value>,
    ) -> Result<Option<Map<String, Value>>, HttpActionError> {
        let mut headers_map = Map::new();
        for header in &self.request.headers {
            if let HttpBinding::From { key, optional } = &header.binding {
                let lower = header.name.to_ascii_lowercase();
                if is_restricted_header(&lower) {
                    return Err(HttpActionError::InvalidInput);
                }
                match state.get(key) {
                    None | Some(Value::Null) if *optional => {}
                    None | Some(Value::Null) => return Err(HttpActionError::InvalidInput),
                    Some(value) => {
                        let text = scalar_text_from_value(value)?;
                        if text.len() > MAX_HTTP_HEADER_VALUE_BYTES
                            || !valid_header_field_value(&text)
                        {
                            return Err(HttpActionError::InvalidInput);
                        }
                        headers_map.insert(lower, value.clone());
                    }
                }
            }
        }
        Ok((!headers_map.is_empty()).then_some(headers_map))
    }

    fn resolve_body_input(
        &self,
        state: &BTreeMap<String, Value>,
    ) -> Result<Option<Value>, HttpActionError> {
        let Some(body) = &self.request.body else {
            return Ok(None);
        };
        match body {
            HttpBodyTemplate::JsonFrom { key, .. } => {
                let value = state.get(key).ok_or(HttpActionError::InvalidInput)?;
                if !is_bounded_json_value(value) {
                    return Err(HttpActionError::ResourceExhausted);
                }
                let encoded =
                    canonical::to_vec(value).map_err(|_| HttpActionError::InvalidInput)?;
                if encoded.len() > MAX_HTTP_BODY_BYTES {
                    return Err(HttpActionError::ResourceExhausted);
                }
                Ok(Some(value.clone()))
            }
            HttpBodyTemplate::TextFrom { key, .. } => {
                let Value::String(text) = state.get(key).ok_or(HttpActionError::InvalidInput)?
                else {
                    return Err(HttpActionError::InvalidInput);
                };
                if text.len() > MAX_HTTP_BODY_BYTES {
                    return Err(HttpActionError::ResourceExhausted);
                }
                Ok(Some(Value::String(text.clone())))
            }
            HttpBodyTemplate::ArtifactFrom { key, .. } => {
                let value = state.get(key).ok_or(HttpActionError::InvalidInput)?;
                let artifact: HttpActionArtifact = serde_json::from_value(value.clone())
                    .map_err(|_| HttpActionError::InvalidInput)?;
                if !artifact.valid(MAX_HTTP_RESPONSE_BYTES) {
                    return Err(HttpActionError::InvalidInput);
                }
                Ok(Some(value.clone()))
            }
            HttpBodyTemplate::Empty
            | HttpBodyTemplate::JsonLiteral { .. }
            | HttpBodyTemplate::TextLiteral { .. } => Ok(None),
        }
    }

    fn resolve_idempotency_key_input(
        &self,
        state: &BTreeMap<String, Value>,
    ) -> Result<Option<String>, HttpActionError> {
        let Some(HttpBinding::From { key, optional }) = &self.request.idempotency_key else {
            return Ok(None);
        };
        match state.get(key) {
            None | Some(Value::Null) if *optional => Ok(None),
            None | Some(Value::Null) => Err(HttpActionError::InvalidInput),
            Some(value) => {
                let text = scalar_text_from_value(value)?;
                if !valid_idempotency_key(&text) {
                    return Err(HttpActionError::InvalidInput);
                }
                Ok(Some(text))
            }
        }
    }
}

/// Canonical inputs resolved from checkpointed state for one HTTP invocation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedHttpInputs {
    pub canonical_bytes: Vec<u8>,
    pub inputs_b64: String,
    pub inputs_digest: [u8; 32],
    pub inputs_digest_hex: String,
}

/// Saved-child scope reference (5d-3; always `None` on root graph in 5d-2).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpActionScopeRef {
    pub scope_id: String,
    pub revision: u64,
    pub digest_sha256: String,
}

/// Worker-to-Main HTTP action invocation v3 (`elitea.runtime.http-action.v3`).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpActionInvocationV3 {
    pub schema_version: String,
    pub node_id: String,
    pub thread_id: String,
    pub step: u64,
    pub attempt: u32,
    pub activation_id: String,
    pub binding_digest: String,
    pub inputs_b64: String,
    pub inputs_digest: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub scope_ref: Option<HttpActionScopeRef>,
}

impl HttpActionInvocationV3 {
    /// Serialize the invocation to JSON and enforce [`MAX_HTTP_INVOCATION_BYTES`].
    pub fn to_bounded_json(&self) -> Result<Vec<u8>, HttpActionError> {
        if self.schema_version != HTTP_ACTION_INVOCATION_SCHEMA
            || self.attempt >= MAX_HTTP_ATTEMPTS
            || !valid_graph_id(&self.node_id)
            || self.thread_id.is_empty()
            || self.thread_id.len() > 512
            || !valid_hex_digest(&self.activation_id)
            || !valid_hex_digest(&self.binding_digest)
            || !valid_hex_digest(&self.inputs_digest)
        {
            return Err(HttpActionError::InvalidInput);
        }
        let encoded = serde_json::to_vec(self).map_err(|_| HttpActionError::InvalidInput)?;
        if encoded.len() > MAX_HTTP_INVOCATION_BYTES {
            return Err(HttpActionError::ResourceExhausted);
        }
        Ok(encoded)
    }
}

/// Immutable artifact descriptor carried in HTTP bodies and receipts.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpActionArtifact {
    pub reference: String,
    pub immutable_version: String,
    pub sha256: String,
    pub byte_length: u64,
}

impl HttpActionArtifact {
    #[must_use]
    pub fn valid(&self, max_bytes: u64) -> bool {
        !self.reference.trim().is_empty()
            && self.reference.len() <= MAX_HTTP_ARTIFACT_REF_BYTES
            && !self.immutable_version.trim().is_empty()
            && self.immutable_version.len() <= MAX_HTTP_ARTIFACT_REF_BYTES
            && !self.reference.chars().any(char::is_control)
            && !self.immutable_version.chars().any(char::is_control)
            && valid_hex_digest(&self.sha256)
            && self.byte_length <= max_bytes
    }
}

/// Projected response payload inside a completed HTTP receipt v2.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum HttpActionData {
    Empty,
    Json { value: Value },
    Text { value: String },
    Artifact { reference: HttpActionArtifact },
}

/// Completed HTTP response projection written into the node's single output key.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpActionProjection {
    pub status: u16,
    pub content_type: Option<String>,
    pub byte_length: u64,
    pub data: HttpActionData,
}

/// Receipt terminal state.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum HttpActionReceiptState {
    Completed,
    Failed,
    Uncertain,
}

/// Main-to-Worker HTTP action receipt v2 (`elitea.runtime.http-action-receipt.v2`).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpActionReceiptV2 {
    pub schema_version: String,
    pub activation_id: String,
    pub request_digest: String,
    pub binding_digest: String,
    pub effect_id: String,
    pub state: HttpActionReceiptState,
    pub failure_code: Option<String>,
    pub result: Option<HttpActionProjection>,
}

/// Decode and validate a Main-owned HTTP action receipt v2.
pub fn decode_receipt_v2(raw: &[u8]) -> Result<HttpActionReceiptV2, HttpActionError> {
    if raw.is_empty() || raw.len() > MAX_HTTP_RECEIPT_BYTES {
        return Err(HttpActionError::InvalidResponse);
    }
    let receipt: HttpActionReceiptV2 =
        serde_json::from_slice(raw).map_err(|_| HttpActionError::InvalidResponse)?;
    if receipt.schema_version != HTTP_ACTION_RECEIPT_SCHEMA
        || !valid_hex_digest(&receipt.activation_id)
        || !valid_hex_digest(&receipt.request_digest)
        || !valid_hex_digest(&receipt.binding_digest)
        || !valid_hex_digest(&receipt.effect_id)
    {
        return Err(HttpActionError::InvalidResponse);
    }
    match receipt.state {
        HttpActionReceiptState::Completed => {
            if receipt.failure_code.is_some() || receipt.result.is_none() {
                return Err(HttpActionError::InvalidResponse);
            }
            if let Some(projection) = &receipt.result {
                validate_projection(projection)?;
            }
        }
        HttpActionReceiptState::Failed | HttpActionReceiptState::Uncertain => {
            if receipt.result.is_some() {
                return Err(HttpActionError::InvalidResponse);
            }
            let Some(code) = receipt.failure_code.as_deref() else {
                return Err(HttpActionError::InvalidResponse);
            };
            if !valid_failure_code(code) {
                return Err(HttpActionError::InvalidResponse);
            }
        }
    }
    Ok(receipt)
}

fn validate_projection(projection: &HttpActionProjection) -> Result<(), HttpActionError> {
    if !(200..=599).contains(&projection.status) || projection.byte_length > MAX_HTTP_RESPONSE_BYTES
    {
        return Err(HttpActionError::InvalidResponse);
    }
    if projection
        .content_type
        .as_deref()
        .is_some_and(|ct| ct.len() > 128)
    {
        return Err(HttpActionError::InvalidResponse);
    }
    match &projection.data {
        HttpActionData::Empty => Ok(()),
        HttpActionData::Json { value } => {
            if !is_bounded_json_value(value) {
                return Err(HttpActionError::InvalidResponse);
            }
            Ok(())
        }
        HttpActionData::Text { value } => {
            if value.len() as u64 > MAX_HTTP_INLINE_BYTES {
                return Err(HttpActionError::InvalidResponse);
            }
            Ok(())
        }
        HttpActionData::Artifact { reference } => {
            if !reference.valid(MAX_HTTP_RESPONSE_BYTES) {
                return Err(HttpActionError::InvalidResponse);
            }
            Ok(())
        }
    }
}

/// Start-time frozen snapshot emitted by Main (`version_details.http_action_snapshot`).
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpActionFrozenSnapshotV2 {
    pub schema_version: String,
    pub application_id: String,
    pub version_id: String,
    pub instructions_digest: String,
    pub nodes: Vec<HttpActionFrozenNodeV2>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HttpActionFrozenNodeV2 {
    pub node_id: String,
    pub operation: String,
    pub template_digest: String,
    pub template_wire_b64: String,
    pub binding_digest: String,
    pub output: Vec<String>,
    pub transition: Option<String>,
}

impl HttpActionFrozenSnapshotV2 {
    /// Parse and structurally verify the start-time snapshot from `version_details`.
    pub fn from_value(value: &Value) -> Result<Self, HttpActionError> {
        let snapshot: Self =
            serde_json::from_value(value.clone()).map_err(|_| HttpActionError::PolicyDenied)?;
        if snapshot.schema_version != HTTP_ACTION_SNAPSHOT_SCHEMA
            || snapshot.application_id.is_empty()
            || snapshot.version_id.is_empty()
            || !valid_hex_digest(&snapshot.instructions_digest)
            || snapshot.nodes.is_empty()
            || snapshot.nodes.len() > 128
        {
            return Err(HttpActionError::PolicyDenied);
        }
        let mut seen = BTreeSet::new();
        for node in &snapshot.nodes {
            if !seen.insert(node.node_id.as_str())
                || !valid_graph_id(&node.node_id)
                || !valid_operation_name(&node.operation)
                || !valid_hex_digest(&node.template_digest)
                || !valid_hex_digest(&node.binding_digest)
                || node.output.len() != 1
                || !valid_http_output_key(&node.output[0])
                || node
                    .transition
                    .as_deref()
                    .is_some_and(|t| t != "END" && !valid_graph_id(t))
                || node.binding_digest != frozen_binding_digest(&snapshot, node)
            {
                return Err(HttpActionError::PolicyDenied);
            }
        }
        Ok(snapshot)
    }

    /// Look up the frozen `binding_digest` for one HTTP node and verify it
    /// matches the compiled node's `operation`, `output`, and `transition`.
    pub fn binding_digest_for_node(
        &self,
        definition: &HttpActionNodeDefinition,
    ) -> Result<([u8; 32], String), HttpActionError> {
        let node = self
            .nodes
            .iter()
            .find(|candidate| candidate.node_id == definition.id())
            .ok_or(HttpActionError::PolicyDenied)?;
        if node.operation != definition.operation()
            || node.output.first().map(String::as_str) != Some(definition.output_key())
            || node.transition.as_deref() != definition.transition()
        {
            return Err(HttpActionError::PolicyDenied);
        }
        let bytes = decode_hex_32(&node.binding_digest).ok_or(HttpActionError::PolicyDenied)?;
        Ok((bytes, node.binding_digest.clone()))
    }
}

/// Exact Go `FrozenBindingDigest` (`services/elitea-main/internal/application/httpaction/frozen.go`).
#[must_use]
pub fn frozen_binding_digest(
    snapshot: &HttpActionFrozenSnapshotV2,
    node: &HttpActionFrozenNodeV2,
) -> String {
    let output = node.output.first().map_or("", String::as_str);
    let transition = node.transition.as_deref().unwrap_or("");
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(FROZEN_NODE_V2_DOMAIN);
    for part in [
        snapshot.application_id.as_str(),
        snapshot.version_id.as_str(),
        snapshot.instructions_digest.as_str(),
        node.node_id.as_str(),
        node.operation.as_str(),
        node.template_digest.as_str(),
        output,
        transition,
    ] {
        digest_field(&mut context, part.as_bytes());
    }
    hex_encode(&copy_sha256(context.finish().as_ref()))
}

/// Exact Go `VisitID` (`elitea.graph.http.activation.v1\0`).
#[must_use]
pub fn visit_id(thread_id: &str, node_id: &str, step: u64, binding_digest: [u8; 32]) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(ACTIVATION_DOMAIN);
    let step_bytes = step.to_be_bytes();
    for part in [
        thread_id.as_bytes(),
        node_id.as_bytes(),
        step_bytes.as_slice(),
        binding_digest.as_slice(),
    ] {
        digest_field(&mut context, part);
    }
    hex_encode(&copy_sha256(context.finish().as_ref()))
}

/// Exact Go `EffectID` v3 (`elitea.http.effect.v3\0`).
#[must_use]
pub fn effect_id_v3(
    execution_id: &str,
    generation: u64,
    activation_id: &str,
    attempt: u32,
    binding_digest: &str,
    inputs_digest: &str,
) -> [u8; 32] {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(EFFECT_V3_DOMAIN);
    let gen_bytes = generation.to_be_bytes();
    let attempt_bytes = u64::from(attempt).to_be_bytes();
    for part in [
        execution_id.as_bytes(),
        gen_bytes.as_slice(),
        activation_id.as_bytes(),
        attempt_bytes.as_slice(),
        binding_digest.as_bytes(),
        inputs_digest.as_bytes(),
    ] {
        digest_field(&mut context, part);
    }
    copy_sha256(context.finish().as_ref())
}

/// Exact Go `EffectID` v3 as 64-character lowercase hex.
#[must_use]
pub fn effect_id_v3_hex(
    execution_id: &str,
    generation: u64,
    activation_id: &str,
    attempt: u32,
    binding_digest: &str,
    inputs_digest: &str,
) -> String {
    hex_encode(&effect_id_v3(
        execution_id,
        generation,
        activation_id,
        attempt,
        binding_digest,
        inputs_digest,
    ))
}

/// Percent-encode a path segment or query key/value using the RFC 3986
/// unreserved set `A-Z a-z 0-9 - . _ ~` with uppercase hex. Space is `%20`,
/// `/` is `%2F`. Matches Go `httpaction.encodeComponent`.
#[must_use]
pub fn encode_component(value: &str) -> String {
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    let mut out = String::with_capacity(value.len());
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~') {
            out.push(byte as char);
        } else {
            out.push('%');
            out.push(HEX[usize::from(byte >> 4)] as char);
            out.push(HEX[usize::from(byte & 0x0f)] as char);
        }
    }
    out
}

/// Whether `name` (already lowercased) is forbidden as a user-supplied HTTP header.
#[must_use]
pub fn is_restricted_header(lower_name: &str) -> bool {
    lower_name.starts_with("x-elitea-")
        || matches!(
            lower_name,
            "authorization"
                | "proxy-authorization"
                | "cookie"
                | "set-cookie"
                | "x-api-key"
                | "api-key"
                | "host"
                | "connection"
                | "content-length"
                | "content-type"
                | "transfer-encoding"
                | "te"
                | "trailer"
                | "upgrade"
                | "proxy-connection"
                | "keep-alive"
                | "idempotency-key"
        )
}

/// Safe HTTP methods (class I on unknown outcome).
#[must_use]
pub fn is_safe_method(method: &str) -> bool {
    matches!(method, "GET" | "HEAD" | "OPTIONS")
}

/// Admin-authored rule contract (`elitea.runtime.http-action-rule.v1`), used
/// for cross-language render-vector verification.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct HttpActionRuleV1 {
    pub schema_version: String,
    pub name: String,
    pub origin: String,
    pub path_template: String,
    pub method: String,
    pub query_names: Vec<String>,
    pub configuration_id: i32,
    pub toolkit: String,
    pub tool: String,
    #[serde(default)]
    pub output_bucket: String,
    #[serde(default)]
    pub replay: String,
    pub enabled: bool,
}

/// Rendered HTTP header pair.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct RenderedHttpHeader {
    pub name: String,
    pub value: String,
}

/// Deterministic render output matching Go `httpaction.Rendered`.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RenderedHttpRequest {
    pub method: String,
    pub path: String,
    pub query: String,
    pub url: String,
    pub headers: Vec<RenderedHttpHeader>,
    pub body: Vec<u8>,
    pub body_b64: String,
    pub body_sha256: String,
    pub request_wire: String,
    pub request_digest: String,
}

/// Typed render error code matching `http-action-render-vectors-v1.json`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RenderFailureCode {
    InvalidInput,
    PolicyDenied,
}

impl RenderFailureCode {
    #[must_use]
    pub const fn vector_code(self) -> &'static str {
        match self {
            Self::InvalidInput => "invalid_input",
            Self::PolicyDenied => "policy_denied",
        }
    }
}

/// Pure reference renderer mirroring Go `httpaction.Render` for cross-language
/// contract vector verification.
#[allow(
    clippy::too_many_lines,
    reason = "Keep step-by-step parity with Go httpaction/render.go explicit."
)]
pub fn render_request(
    rule: &HttpActionRuleV1,
    node: &HttpActionNodeDefinition,
    raw_inputs: &[u8],
) -> Result<RenderedHttpRequest, RenderFailureCode> {
    if raw_inputs.is_empty() || raw_inputs.len() > MAX_HTTP_INPUTS_BYTES {
        return Err(RenderFailureCode::InvalidInput);
    }
    let inputs_value: Value =
        serde_json::from_slice(raw_inputs).map_err(|_| RenderFailureCode::InvalidInput)?;
    if !is_bounded_json_value(&inputs_value) {
        return Err(RenderFailureCode::InvalidInput);
    }
    let canonical_inputs =
        canonical::to_vec(&inputs_value).map_err(|_| RenderFailureCode::InvalidInput)?;
    if canonical_inputs != raw_inputs {
        return Err(RenderFailureCode::InvalidInput);
    }
    let inputs_obj = inputs_value
        .as_object()
        .ok_or(RenderFailureCode::InvalidInput)?;
    for key in inputs_obj.keys() {
        if !matches!(
            key.as_str(),
            "path" | "query" | "headers" | "body" | "idempotency_key"
        ) {
            return Err(RenderFailureCode::InvalidInput);
        }
    }

    if !validate_rule(rule) || !rule.enabled || node.operation() != rule.name {
        return Err(RenderFailureCode::PolicyDenied);
    }

    let path_inputs = optional_input_object(inputs_obj, "path")?;
    let query_inputs = optional_input_object(inputs_obj, "query")?;
    let header_inputs = optional_input_object(inputs_obj, "headers")?;
    let body_input = inputs_obj.get("body");
    let idempotency_input = match inputs_obj.get("idempotency_key") {
        None => None,
        Some(Value::String(s)) => Some(s.as_str()),
        Some(_) => return Err(RenderFailureCode::InvalidInput),
    };

    let path = render_path(rule, &node.request.path, path_inputs)?;
    let query = render_query(rule, &node.request.query, query_inputs)?;
    let headers = render_headers(&node.request.headers, header_inputs)?;
    let (body_wire, body_bytes) =
        render_body(&rule.method, node.request.body.as_ref(), body_input)?;
    let idempotency_key =
        render_idempotency_key(node.request.idempotency_key.as_ref(), idempotency_input)?;

    let url = if query.is_empty() {
        format!("{}{}", rule.origin, path)
    } else {
        format!("{}{}?{}", rule.origin, path, query)
    };
    if url.len() > MAX_HTTP_URL_BYTES {
        return Err(RenderFailureCode::InvalidInput);
    }

    let mut wire = Map::new();
    wire.insert("method".to_owned(), Value::String(rule.method.clone()));
    wire.insert("url".to_owned(), Value::String(url.clone()));
    if rule.configuration_id > 0 {
        let mut cred = Map::new();
        cred.insert(
            "configuration_id".to_owned(),
            Value::Number(rule.configuration_id.into()),
        );
        wire.insert("credential".to_owned(), Value::Object(cred));
    }
    if !headers.is_empty() {
        let mut total = 0_usize;
        let mut list = Vec::with_capacity(headers.len());
        for header in &headers {
            total = total
                .saturating_add(header.name.len())
                .saturating_add(header.value.len());
            if total > MAX_HTTP_HEADERS_TOTAL_BYTES {
                return Err(RenderFailureCode::InvalidInput);
            }
            let mut item = Map::new();
            item.insert("name".to_owned(), Value::String(header.name.clone()));
            item.insert("value".to_owned(), Value::String(header.value.clone()));
            list.push(Value::Object(item));
        }
        wire.insert("headers".to_owned(), Value::Array(list));
    }
    if let Some(body) = body_wire {
        wire.insert("body".to_owned(), body);
    }
    if let Some(key) = idempotency_key {
        if !valid_idempotency_key(&key) {
            return Err(RenderFailureCode::InvalidInput);
        }
        wire.insert("idempotency_key".to_owned(), Value::String(key));
    }
    wire.insert(
        "response".to_owned(),
        response_contract_value(&node.response),
    );
    if let Some(timeout_ms) = node.timeout_ms {
        wire.insert("timeout_ms".to_owned(), Value::Number(timeout_ms.into()));
    }

    let request_wire =
        canonical::to_string(&Value::Object(wire)).map_err(|_| RenderFailureCode::InvalidInput)?;
    if request_wire.len() > MAX_HTTP_REQUEST_BYTES {
        return Err(RenderFailureCode::InvalidInput);
    }
    let request_digest = hex_encode(&sha256(request_wire.as_bytes()));
    let body_b64 = BASE64_STANDARD.encode(&body_bytes);
    let body_sha256 = hex_encode(&sha256(&body_bytes));

    Ok(RenderedHttpRequest {
        method: rule.method.clone(),
        path,
        query,
        url,
        headers,
        body: body_bytes,
        body_b64,
        body_sha256,
        request_wire,
        request_digest,
    })
}

fn optional_input_object<'a>(
    inputs: &'a Map<String, Value>,
    key: &str,
) -> Result<Option<&'a Map<String, Value>>, RenderFailureCode> {
    match inputs.get(key) {
        None => Ok(None),
        Some(Value::Object(map)) if map.len() <= MAX_HTTP_PATH_PLACEHOLDERS => Ok(Some(map)),
        Some(_) => Err(RenderFailureCode::InvalidInput),
    }
}

fn render_path(
    rule: &HttpActionRuleV1,
    bindings: &BTreeMap<String, HttpBinding>,
    inputs: Option<&Map<String, Value>>,
) -> Result<String, RenderFailureCode> {
    let placeholders =
        rule_placeholders(&rule.path_template).ok_or(RenderFailureCode::PolicyDenied)?;
    if bindings.len() != placeholders.len() {
        return Err(RenderFailureCode::PolicyDenied);
    }
    let mut values = BTreeMap::new();
    let mut used = 0_usize;
    for name in &placeholders {
        let binding = bindings.get(name).ok_or(RenderFailureCode::PolicyDenied)?;
        let text = match binding {
            HttpBinding::Literal(lit) => lit.as_scalar_text(),
            HttpBinding::From { .. } => {
                let raw = inputs
                    .and_then(|map| map.get(name))
                    .ok_or(RenderFailureCode::InvalidInput)?;
                used += 1;
                scalar_text_from_value(raw).map_err(|_| RenderFailureCode::InvalidInput)?
            }
        };
        if text.is_empty() || text == "." || text == ".." || text.len() > MAX_HTTP_SEGMENT_BYTES {
            return Err(RenderFailureCode::InvalidInput);
        }
        values.insert(name.as_str(), encode_component(&text));
    }
    if used != inputs.map_or(0, Map::len) {
        return Err(RenderFailureCode::InvalidInput);
    }
    if rule.path_template == "/" {
        return Ok("/".to_owned());
    }
    let mut out = String::new();
    for segment in rule.path_template[1..].split('/') {
        out.push('/');
        if let Some(name) = placeholder_name(segment) {
            out.push_str(values.get(name).ok_or(RenderFailureCode::PolicyDenied)?);
        } else {
            out.push_str(segment);
        }
    }
    Ok(out)
}

fn render_query(
    rule: &HttpActionRuleV1,
    bindings: &BTreeMap<String, HttpBinding>,
    inputs: Option<&Map<String, Value>>,
) -> Result<String, RenderFailureCode> {
    let allowed: BTreeSet<&str> = rule.query_names.iter().map(String::as_str).collect();
    for name in bindings.keys() {
        if !allowed.contains(name.as_str()) {
            return Err(RenderFailureCode::PolicyDenied);
        }
    }
    let mut pairs = Vec::with_capacity(bindings.len());
    let mut used = 0_usize;
    // BTreeMap iterates sorted by raw UTF-8 bytes.
    for (name, binding) in bindings {
        let text = match binding {
            HttpBinding::Literal(lit) => Some(lit.as_scalar_text()),
            HttpBinding::From { optional, .. } => match inputs.and_then(|map| map.get(name)) {
                None if *optional => None,
                None => return Err(RenderFailureCode::InvalidInput),
                Some(raw) => {
                    used += 1;
                    Some(scalar_text_from_value(raw).map_err(|_| RenderFailureCode::InvalidInput)?)
                }
            },
        };
        if let Some(text) = text {
            pairs.push(format!(
                "{}={}",
                encode_component(name),
                encode_component(&text)
            ));
        }
    }
    if used != inputs.map_or(0, Map::len) {
        return Err(RenderFailureCode::InvalidInput);
    }
    Ok(pairs.join("&"))
}

fn render_headers(
    bindings: &[HttpHeaderBinding],
    inputs: Option<&Map<String, Value>>,
) -> Result<Vec<RenderedHttpHeader>, RenderFailureCode> {
    let mut headers = Vec::with_capacity(bindings.len());
    let mut seen = BTreeSet::new();
    let mut used = 0_usize;
    for header in bindings {
        let lower = header.name.to_ascii_lowercase();
        if !valid_header_name(&header.name)
            || is_restricted_header(&lower)
            || !seen.insert(lower.clone())
        {
            return Err(RenderFailureCode::InvalidInput);
        }
        let text = match &header.binding {
            HttpBinding::Literal(lit) => Some(lit.as_scalar_text()),
            HttpBinding::From { optional, .. } => match inputs.and_then(|map| map.get(&lower)) {
                None if *optional => None,
                None => return Err(RenderFailureCode::InvalidInput),
                Some(raw) => {
                    used += 1;
                    Some(scalar_text_from_value(raw).map_err(|_| RenderFailureCode::InvalidInput)?)
                }
            },
        };
        if let Some(text) = text {
            if text.len() > MAX_HTTP_HEADER_VALUE_BYTES || !valid_header_field_value(&text) {
                return Err(RenderFailureCode::InvalidInput);
            }
            headers.push(RenderedHttpHeader {
                name: lower,
                value: text,
            });
        }
    }
    if used != inputs.map_or(0, Map::len) {
        return Err(RenderFailureCode::InvalidInput);
    }
    headers.sort_by(|left, right| left.name.as_bytes().cmp(right.name.as_bytes()));
    Ok(headers)
}

fn render_body(
    method: &str,
    template: Option<&HttpBodyTemplate>,
    input: Option<&Value>,
) -> Result<(Option<Value>, Vec<u8>), RenderFailureCode> {
    let Some(template) = template else {
        return if input.is_some() {
            Err(RenderFailureCode::InvalidInput)
        } else {
            Ok((None, Vec::new()))
        };
    };
    if matches!(template, HttpBodyTemplate::Empty) {
        return if input.is_some() {
            Err(RenderFailureCode::InvalidInput)
        } else {
            Ok((None, Vec::new()))
        };
    }
    if is_safe_method(method) {
        return Err(RenderFailureCode::InvalidInput);
    }
    let mut body_obj = Map::new();
    match template {
        HttpBodyTemplate::Empty => unreachable!(),
        HttpBodyTemplate::JsonFrom { content_type, .. } => {
            let value = input.ok_or(RenderFailureCode::InvalidInput)?;
            let bytes = canonical::to_vec(value).map_err(|_| RenderFailureCode::InvalidInput)?;
            if bytes.len() > MAX_HTTP_BODY_BYTES {
                return Err(RenderFailureCode::InvalidInput);
            }
            body_obj.insert("kind".to_owned(), Value::String("json".to_owned()));
            if let Some(ct) = content_type {
                body_obj.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
            body_obj.insert("value".to_owned(), value.clone());
            Ok((Some(Value::Object(body_obj)), bytes))
        }
        HttpBodyTemplate::JsonLiteral {
            value,
            content_type,
        } => {
            if input.is_some() {
                return Err(RenderFailureCode::InvalidInput);
            }
            let bytes = canonical::to_vec(value).map_err(|_| RenderFailureCode::InvalidInput)?;
            if bytes.len() > MAX_HTTP_BODY_BYTES {
                return Err(RenderFailureCode::InvalidInput);
            }
            body_obj.insert("kind".to_owned(), Value::String("json".to_owned()));
            if let Some(ct) = content_type {
                body_obj.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
            body_obj.insert("value".to_owned(), value.clone());
            Ok((Some(Value::Object(body_obj)), bytes))
        }
        HttpBodyTemplate::TextFrom { content_type, .. } => {
            let Some(Value::String(text)) = input else {
                return Err(RenderFailureCode::InvalidInput);
            };
            if text.len() > MAX_HTTP_BODY_BYTES {
                return Err(RenderFailureCode::InvalidInput);
            }
            body_obj.insert("kind".to_owned(), Value::String("text".to_owned()));
            if let Some(ct) = content_type {
                body_obj.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
            body_obj.insert("value".to_owned(), Value::String(text.clone()));
            Ok((Some(Value::Object(body_obj)), text.as_bytes().to_vec()))
        }
        HttpBodyTemplate::TextLiteral {
            value,
            content_type,
        } => {
            if input.is_some() {
                return Err(RenderFailureCode::InvalidInput);
            }
            if value.len() > MAX_HTTP_BODY_BYTES {
                return Err(RenderFailureCode::InvalidInput);
            }
            body_obj.insert("kind".to_owned(), Value::String("text".to_owned()));
            if let Some(ct) = content_type {
                body_obj.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
            body_obj.insert("value".to_owned(), Value::String(value.clone()));
            Ok((Some(Value::Object(body_obj)), value.as_bytes().to_vec()))
        }
        HttpBodyTemplate::ArtifactFrom { content_type, .. } => {
            let value = input.ok_or(RenderFailureCode::InvalidInput)?;
            let artifact: HttpActionArtifact = serde_json::from_value(value.clone())
                .map_err(|_| RenderFailureCode::InvalidInput)?;
            if !artifact.valid(MAX_HTTP_RESPONSE_BYTES) {
                return Err(RenderFailureCode::InvalidInput);
            }
            body_obj.insert("kind".to_owned(), Value::String("artifact".to_owned()));
            body_obj.insert(
                "content_type".to_owned(),
                Value::String(content_type.clone()),
            );
            body_obj.insert("reference".to_owned(), value.clone());
            Ok((Some(Value::Object(body_obj)), Vec::new()))
        }
    }
}

fn render_idempotency_key(
    binding: Option<&HttpBinding>,
    input: Option<&str>,
) -> Result<Option<String>, RenderFailureCode> {
    let Some(binding) = binding else {
        return if input.is_some() {
            Err(RenderFailureCode::InvalidInput)
        } else {
            Ok(None)
        };
    };
    match binding {
        HttpBinding::From { optional, .. } => match input {
            None if *optional => Ok(None),
            None => Err(RenderFailureCode::InvalidInput),
            Some(key) => Ok(Some(key.to_owned())),
        },
        HttpBinding::Literal(lit) => {
            if input.is_some() {
                return Err(RenderFailureCode::InvalidInput);
            }
            Ok(Some(lit.as_scalar_text()))
        }
    }
}

fn parse_request_template(
    raw: RawHttpRequest,
) -> Result<HttpRequestTemplate, HttpActionConfigurationError> {
    let path = parse_binding_map(raw.path)?;
    let query = parse_binding_map(raw.query)?;
    if raw.headers.len() > MAX_HTTP_HEADERS {
        return Err(HttpActionConfigurationError::ResourceExhausted);
    }
    let mut headers = Vec::with_capacity(raw.headers.len());
    for entry in raw.headers {
        headers.push(parse_header_binding(entry)?);
    }
    let body = raw.body.map(parse_body_template).transpose()?;
    let idempotency_key = raw
        .idempotency_key
        .map(|entry| parse_binding(entry, MAX_HTTP_LITERAL_STRING_BYTES))
        .transpose()?;
    Ok(HttpRequestTemplate {
        path,
        query,
        headers,
        body,
        idempotency_key,
    })
}

fn parse_binding_map(
    raw: BTreeMap<String, RawHttpBinding>,
) -> Result<BTreeMap<String, HttpBinding>, HttpActionConfigurationError> {
    if raw.len() > MAX_HTTP_PATH_PLACEHOLDERS {
        return Err(HttpActionConfigurationError::ResourceExhausted);
    }
    let mut out = BTreeMap::new();
    for (name, value) in raw {
        if !valid_binding_key(&name) {
            return Err(HttpActionConfigurationError::Invalid(
                "an HTTP binding key is malformed",
            ));
        }
        out.insert(name, parse_binding(value, MAX_HTTP_LITERAL_STRING_BYTES)?);
    }
    Ok(out)
}

fn parse_binding(
    raw: RawHttpBinding,
    max_string: usize,
) -> Result<HttpBinding, HttpActionConfigurationError> {
    match (raw.from, raw.value, raw.optional) {
        (Some(key), None, optional) => {
            if !valid_state_key(&key) {
                return Err(HttpActionConfigurationError::Invalid(
                    "an HTTP binding state key is malformed",
                ));
            }
            Ok(HttpBinding::From {
                key,
                optional: optional.unwrap_or(false),
            })
        }
        (None, Some(value), None) => Ok(HttpBinding::Literal(parse_scalar_literal(
            value, max_string,
        )?)),
        _ => Err(HttpActionConfigurationError::Invalid(
            "an HTTP binding must declare either 'from' or 'value'",
        )),
    }
}

fn parse_header_binding(
    raw: RawHttpHeaderBinding,
) -> Result<HttpHeaderBinding, HttpActionConfigurationError> {
    if !valid_header_name(&raw.name) {
        return Err(HttpActionConfigurationError::Invalid(
            "an HTTP header name is malformed",
        ));
    }
    if is_restricted_header(&raw.name.to_ascii_lowercase()) {
        return Err(HttpActionConfigurationError::Invalid(
            "an HTTP header name is restricted",
        ));
    }
    if let Some(value) = &raw.value {
        let Some(text) = value.as_str() else {
            return Err(HttpActionConfigurationError::Invalid(
                "a literal HTTP header value must be a string",
            ));
        };
        if text.len() > MAX_HTTP_HEADER_VALUE_BYTES || !valid_header_field_value(text) {
            return Err(HttpActionConfigurationError::Invalid(
                "a literal HTTP header value is invalid",
            ));
        }
    }
    let binding = parse_binding(
        RawHttpBinding {
            from: raw.from,
            optional: raw.optional,
            value: raw.value,
        },
        MAX_HTTP_HEADER_VALUE_BYTES,
    )?;
    Ok(HttpHeaderBinding {
        name: raw.name,
        binding,
    })
}

fn parse_body_template(raw: RawHttpBody) -> Result<HttpBodyTemplate, HttpActionConfigurationError> {
    if let Some(ct) = &raw.content_type
        && !valid_media_type(ct)
    {
        return Err(HttpActionConfigurationError::Invalid(
            "the HTTP body content_type is malformed",
        ));
    }
    if let Some(key) = &raw.from
        && !valid_state_key(key)
    {
        return Err(HttpActionConfigurationError::Invalid(
            "the HTTP body state key is malformed",
        ));
    }
    match raw.kind.as_str() {
        "empty" => {
            if raw.from.is_some() || raw.value.is_some() || raw.content_type.is_some() {
                return Err(HttpActionConfigurationError::Invalid(
                    "an empty HTTP body cannot carry from, value, or content_type",
                ));
            }
            Ok(HttpBodyTemplate::Empty)
        }
        "json" => {
            if let Some(ct) = &raw.content_type
                && !valid_json_media_type(ct)
            {
                return Err(HttpActionConfigurationError::Invalid(
                    "a JSON HTTP body content_type must be application/json or *+json",
                ));
            }
            match (raw.from, raw.value) {
                (Some(key), None) => Ok(HttpBodyTemplate::JsonFrom {
                    key,
                    content_type: raw.content_type,
                }),
                (None, Some(value)) => {
                    if !is_bounded_json_value(&value) {
                        return Err(HttpActionConfigurationError::ResourceExhausted);
                    }
                    let encoded = canonical::to_vec(&value).map_err(|_| {
                        HttpActionConfigurationError::Invalid("the literal JSON body is invalid")
                    })?;
                    if encoded.len() > MAX_HTTP_BODY_BYTES {
                        return Err(HttpActionConfigurationError::ResourceExhausted);
                    }
                    Ok(HttpBodyTemplate::JsonLiteral {
                        value,
                        content_type: raw.content_type,
                    })
                }
                _ => Err(HttpActionConfigurationError::Invalid(
                    "a JSON HTTP body must specify either 'from' or 'value'",
                )),
            }
        }
        "text" => match (raw.from, raw.value) {
            (Some(key), None) => Ok(HttpBodyTemplate::TextFrom {
                key,
                content_type: raw.content_type,
            }),
            (None, Some(Value::String(value))) => {
                if value.len() > MAX_HTTP_BODY_BYTES {
                    return Err(HttpActionConfigurationError::ResourceExhausted);
                }
                Ok(HttpBodyTemplate::TextLiteral {
                    value,
                    content_type: raw.content_type,
                })
            }
            _ => Err(HttpActionConfigurationError::Invalid(
                "a text HTTP body must specify 'from' or a string 'value'",
            )),
        },
        "artifact" => match (raw.from, raw.value, raw.content_type) {
            (Some(key), None, Some(content_type)) => {
                Ok(HttpBodyTemplate::ArtifactFrom { key, content_type })
            }
            _ => Err(HttpActionConfigurationError::Invalid(
                "an artifact HTTP body requires 'from' and 'content_type'",
            )),
        },
        _ => Err(HttpActionConfigurationError::Invalid(
            "the HTTP body kind must be empty, json, text, or artifact",
        )),
    }
}

fn parse_response_contract(
    raw: RawHttpResponse,
) -> Result<HttpResponseContract, HttpActionConfigurationError> {
    let mode = match raw.mode.as_str() {
        "json" => HttpResponseMode::Json,
        "text" => HttpResponseMode::Text,
        "artifact" => HttpResponseMode::Artifact,
        _ => {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP response mode must be json, text, or artifact",
            ));
        }
    };
    if let Some(max_bytes) = raw.max_bytes
        && (max_bytes == 0 || max_bytes > MAX_HTTP_RESPONSE_BYTES)
    {
        return Err(HttpActionConfigurationError::Invalid(
            "the HTTP response max_bytes must be between 1 and 2097152",
        ));
    }
    if let Some(statuses) = &raw.accepted_statuses {
        if statuses.is_empty() || statuses.len() > MAX_HTTP_ACCEPTED_STATUSES {
            return Err(HttpActionConfigurationError::Invalid(
                "the HTTP response accepted_statuses must have 1 to 16 ranges",
            ));
        }
        for range in statuses {
            if range.first < 200
                || range.last > 599
                || range.first > range.last
                || (range.first <= 399 && range.last >= 300)
                || (range.first <= 401 && range.last >= 401)
                || (range.first <= 403 && range.last >= 403)
            {
                return Err(HttpActionConfigurationError::Invalid(
                    "the HTTP response accepted_statuses cannot include 3xx, 401, or 403",
                ));
            }
        }
    }
    Ok(HttpResponseContract {
        mode,
        accepted_statuses: raw.accepted_statuses,
        max_bytes: raw.max_bytes,
    })
}

fn parse_scalar_literal(
    value: Value,
    max_string: usize,
) -> Result<HttpScalarLiteral, HttpActionConfigurationError> {
    match value {
        Value::String(s) => {
            if s.len() > max_string {
                return Err(HttpActionConfigurationError::ResourceExhausted);
            }
            Ok(HttpScalarLiteral::String(s))
        }
        Value::Bool(b) => Ok(HttpScalarLiteral::Bool(b)),
        Value::Number(n) => {
            let Some(i) = n.as_i64() else {
                return Err(HttpActionConfigurationError::Invalid(
                    "an HTTP scalar literal must be a string, 64-bit integer, or boolean",
                ));
            };
            Ok(HttpScalarLiteral::Int(i))
        }
        _ => Err(HttpActionConfigurationError::Invalid(
            "an HTTP scalar literal must be a string, 64-bit integer, or boolean",
        )),
    }
}

fn request_template_value(request: &HttpRequestTemplate) -> Value {
    let mut map = Map::new();
    if !request.path.is_empty() {
        let mut path = Map::new();
        for (k, v) in &request.path {
            path.insert(k.clone(), binding_value(v));
        }
        map.insert("path".to_owned(), Value::Object(path));
    }
    if !request.query.is_empty() {
        let mut query = Map::new();
        for (k, v) in &request.query {
            query.insert(k.clone(), binding_value(v));
        }
        map.insert("query".to_owned(), Value::Object(query));
    }
    if !request.headers.is_empty() {
        let mut headers = Vec::with_capacity(request.headers.len());
        for header in &request.headers {
            let mut item = match binding_value(&header.binding) {
                Value::Object(m) => m,
                _ => Map::new(),
            };
            item.insert("name".to_owned(), Value::String(header.name.clone()));
            headers.push(Value::Object(item));
        }
        map.insert("headers".to_owned(), Value::Array(headers));
    }
    if let Some(body) = &request.body {
        map.insert("body".to_owned(), body_template_value(body));
    }
    if let Some(key) = &request.idempotency_key {
        map.insert("idempotency_key".to_owned(), binding_value(key));
    }
    Value::Object(map)
}

fn binding_value(binding: &HttpBinding) -> Value {
    let mut map = Map::new();
    match binding {
        HttpBinding::From { key, optional } => {
            map.insert("from".to_owned(), Value::String(key.clone()));
            if *optional {
                map.insert("optional".to_owned(), Value::Bool(true));
            }
        }
        HttpBinding::Literal(lit) => {
            map.insert("value".to_owned(), lit.to_json_value());
        }
    }
    Value::Object(map)
}

fn body_template_value(body: &HttpBodyTemplate) -> Value {
    let mut map = Map::new();
    match body {
        HttpBodyTemplate::Empty => {
            map.insert("kind".to_owned(), Value::String("empty".to_owned()));
        }
        HttpBodyTemplate::JsonFrom { key, content_type } => {
            map.insert("kind".to_owned(), Value::String("json".to_owned()));
            map.insert("from".to_owned(), Value::String(key.clone()));
            if let Some(ct) = content_type {
                map.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
        }
        HttpBodyTemplate::JsonLiteral {
            value,
            content_type,
        } => {
            map.insert("kind".to_owned(), Value::String("json".to_owned()));
            map.insert("value".to_owned(), value.clone());
            if let Some(ct) = content_type {
                map.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
        }
        HttpBodyTemplate::TextFrom { key, content_type } => {
            map.insert("kind".to_owned(), Value::String("text".to_owned()));
            map.insert("from".to_owned(), Value::String(key.clone()));
            if let Some(ct) = content_type {
                map.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
        }
        HttpBodyTemplate::TextLiteral {
            value,
            content_type,
        } => {
            map.insert("kind".to_owned(), Value::String("text".to_owned()));
            map.insert("value".to_owned(), Value::String(value.clone()));
            if let Some(ct) = content_type {
                map.insert("content_type".to_owned(), Value::String(ct.clone()));
            }
        }
        HttpBodyTemplate::ArtifactFrom { key, content_type } => {
            map.insert("kind".to_owned(), Value::String("artifact".to_owned()));
            map.insert("from".to_owned(), Value::String(key.clone()));
            map.insert(
                "content_type".to_owned(),
                Value::String(content_type.clone()),
            );
        }
    }
    Value::Object(map)
}

fn response_contract_value(response: &HttpResponseContract) -> Value {
    let mut map = Map::new();
    map.insert(
        "mode".to_owned(),
        Value::String(response.mode.as_str().to_owned()),
    );
    if let Some(statuses) = &response.accepted_statuses {
        let list = statuses
            .iter()
            .map(|r| {
                let mut item = Map::new();
                item.insert("first".to_owned(), Value::Number(r.first.into()));
                item.insert("last".to_owned(), Value::Number(r.last.into()));
                Value::Object(item)
            })
            .collect();
        map.insert("accepted_statuses".to_owned(), Value::Array(list));
    }
    if let Some(max_bytes) = response.max_bytes {
        map.insert("max_bytes".to_owned(), Value::Number(max_bytes.into()));
    }
    Value::Object(map)
}

fn scalar_text_from_value(value: &Value) -> Result<String, HttpActionError> {
    match value {
        Value::String(s) => Ok(s.clone()),
        Value::Bool(b) => Ok(b.to_string()),
        Value::Number(n) => {
            let text = n.to_string();
            // Refuse floats (including 1.0 or scientific notation) matching Go `integerToken`.
            if !valid_integer_token(&text) {
                return Err(HttpActionError::InvalidInput);
            }
            let _ = text
                .parse::<i64>()
                .map_err(|_| HttpActionError::InvalidInput)?;
            Ok(text)
        }
        _ => Err(HttpActionError::InvalidInput),
    }
}

fn validate_rule(rule: &HttpActionRuleV1) -> bool {
    if rule.schema_version != HTTP_ACTION_RULE_SCHEMA
        || !valid_operation_name(&rule.name)
        || !valid_rule_origin(&rule.origin)
        || rule_placeholders(&rule.path_template).is_none()
        || !matches!(
            rule.method.as_str(),
            "GET" | "HEAD" | "OPTIONS" | "POST" | "PUT" | "PATCH" | "DELETE"
        )
        || rule.query_names.len() > MAX_HTTP_QUERY_NAMES
        || rule.configuration_id < 0
        || !valid_graph_id(&rule.toolkit)
        || !valid_graph_id(&rule.tool)
    {
        return false;
    }
    let mut seen = BTreeSet::new();
    for name in &rule.query_names {
        if !valid_binding_key(name) || !seen.insert(name.as_str()) || is_secret_query_name(name) {
            return false;
        }
    }
    if !rule.replay.is_empty() && !matches!(rule.replay.as_str(), "never" | "idempotency_key") {
        return false;
    }
    true
}

fn valid_rule_origin(origin: &str) -> bool {
    let Some(rest) = origin.strip_prefix("https://") else {
        return false;
    };
    if origin.len() > 262 || rest.is_empty() || rest.contains('/') {
        return false;
    }
    let (host, port) = match rest.split_once(':') {
        Some((h, p)) => (h, Some(p)),
        None => (rest, None),
    };
    if host.is_empty()
        || host.len() > 253
        || !host.as_bytes()[0].is_ascii_alphanumeric()
        || !host.as_bytes()[host.len() - 1].is_ascii_alphanumeric()
        || !host
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
    {
        return false;
    }
    if let Some(p) = port {
        let Ok(num) = p.parse::<u16>() else {
            return false;
        };
        if num == 0 {
            return false;
        }
    }
    true
}

fn rule_placeholders(path_template: &str) -> Option<Vec<String>> {
    if path_template.is_empty()
        || path_template.len() > MAX_HTTP_PATH_TEMPLATE_BYTES
        || !path_template.starts_with('/')
    {
        return None;
    }
    if path_template == "/" {
        return Some(Vec::new());
    }
    let mut names = Vec::new();
    let mut seen = BTreeSet::new();
    for segment in path_template[1..].split('/') {
        if segment.is_empty() || segment == "." || segment == ".." {
            return None;
        }
        if let Some(name) = placeholder_name(segment) {
            if !seen.insert(name) {
                return None;
            }
            names.push(name.to_owned());
        } else if !segment
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'~' | b'-'))
        {
            return None;
        }
    }
    if names.len() > MAX_HTTP_PATH_PLACEHOLDERS {
        return None;
    }
    Some(names)
}

fn placeholder_name(segment: &str) -> Option<&str> {
    let inner = segment.strip_prefix('{')?.strip_suffix('}')?;
    if inner.is_empty() || inner.len() > 64 {
        return None;
    }
    let mut bytes = inner.bytes();
    if !bytes.next()?.is_ascii_lowercase() {
        return None;
    }
    if !bytes.all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_') {
        return None;
    }
    Some(inner)
}

fn is_secret_query_name(name: &str) -> bool {
    matches!(
        name.to_ascii_lowercase().as_str(),
        "access_token"
            | "token"
            | "api_key"
            | "apikey"
            | "password"
            | "client_secret"
            | "authorization"
    )
}

/// `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`, max 128 bytes.
#[must_use]
pub fn valid_operation_name(name: &str) -> bool {
    if name.is_empty() || name.len() > MAX_HTTP_RULE_NAME_BYTES {
        return false;
    }
    for segment in name.split('.') {
        let mut bytes = segment.bytes();
        if !bytes.next().is_some_and(|b| b.is_ascii_lowercase()) {
            return false;
        }
        if !bytes.all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_') {
            return false;
        }
    }
    true
}

fn valid_state_key(key: &str) -> bool {
    if key.is_empty() || key.len() > 128 {
        return false;
    }
    let mut bytes = key.bytes();
    if !bytes
        .next()
        .is_some_and(|b| b.is_ascii_alphabetic() || b == b'_')
    {
        return false;
    }
    bytes.all(|b| b.is_ascii_alphanumeric() || b == b'_')
}

fn valid_binding_key(key: &str) -> bool {
    !key.is_empty()
        && key.len() <= 64
        && key
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'~' | b'-'))
}

fn valid_header_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= MAX_HTTP_HEADER_NAME_BYTES
        && name.bytes().all(|b| {
            b.is_ascii_alphanumeric()
                || matches!(
                    b,
                    b'!' | b'#'
                        | b'$'
                        | b'%'
                        | b'&'
                        | b'\''
                        | b'*'
                        | b'+'
                        | b'-'
                        | b'.'
                        | b'^'
                        | b'_'
                        | b'`'
                        | b'|'
                        | b'~'
                )
        })
}

/// Mirrors Go `httpguts.ValidHeaderFieldValue`: printable ASCII + HTAB + obs-text (`0x80..=0xfe`),
/// refusing `\r`, `\n`, `\0`, DEL (`0x7f`), and other C0 controls.
fn valid_header_field_value(value: &str) -> bool {
    value
        .bytes()
        .all(|b| b == b'\t' || (0x20..=0x7e).contains(&b) || (0x80..=0xfe).contains(&b))
}

fn valid_idempotency_key(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_HTTP_IDEMPOTENCY_KEY_BYTES
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b':' | b'-'))
}

fn valid_media_type(value: &str) -> bool {
    if value.len() > 128 {
        return false;
    }
    let Some((kind, subtype)) = value.split_once('/') else {
        return false;
    };
    let part_ok = |part: &str| {
        !part.is_empty()
            && part.bytes().all(|b| {
                b.is_ascii_alphanumeric()
                    || matches!(
                        b,
                        b'!' | b'#' | b'$' | b'&' | b'^' | b'_' | b'.' | b'+' | b'-'
                    )
            })
    };
    part_ok(kind) && part_ok(subtype)
}

fn valid_json_media_type(value: &str) -> bool {
    if !valid_media_type(value) {
        return false;
    }
    let lower = value.to_ascii_lowercase();
    lower == "application/json" || lower.ends_with("+json")
}

fn valid_http_output_key(key: &str) -> bool {
    valid_output_key(key)
        && key.len() <= 256
        && !key.bytes().any(|b| matches!(b, 0 | b'\r' | b'\n'))
}

fn valid_failure_code(code: &str) -> bool {
    !code.is_empty()
        && code.len() <= 64
        && code.bytes().all(|b| b.is_ascii_lowercase() || b == b'_')
}

fn valid_integer_token(text: &str) -> bool {
    let digits = text.strip_prefix('-').unwrap_or(text);
    if digits.is_empty() || digits.len() > 19 {
        return false;
    }
    if digits == "0" {
        return true;
    }
    let bytes = digits.as_bytes();
    (b'1'..=b'9').contains(&bytes[0]) && bytes[1..].iter().all(u8::is_ascii_digit)
}

/// 64 lowercase hex characters.
#[must_use]
pub fn valid_hex_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

/// Decode a 64-character lowercase hex digest into 32 bytes.
#[must_use]
pub fn decode_hex_32(value: &str) -> Option<[u8; 32]> {
    if !valid_hex_digest(value) {
        return None;
    }
    let mut out = [0_u8; 32];
    for (index, slot) in out.iter_mut().enumerate() {
        let pair = value.get(index * 2..index * 2 + 2)?;
        *slot = u8::from_str_radix(pair, 16).ok()?;
    }
    Some(out)
}

/// Encode 32 bytes as 64 lowercase hex characters.
#[must_use]
pub fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for &byte in bytes {
        out.push(HEX[usize::from(byte >> 4)] as char);
        out.push(HEX[usize::from(byte & 0x0f)] as char);
    }
    out
}

fn is_bounded_json_value(value: &Value) -> bool {
    fn walk(value: &Value, depth: usize, count: &mut usize) -> bool {
        *count += 1;
        if depth > MAX_HTTP_JSON_DEPTH || *count > MAX_HTTP_JSON_NODES {
            return false;
        }
        match value {
            Value::Array(items) => items.iter().all(|item| walk(item, depth + 1, count)),
            Value::Object(map) => map.values().all(|item| walk(item, depth + 1, count)),
            _ => true,
        }
    }
    let mut count = 0;
    walk(value, 0, &mut count)
}

fn sha256(bytes: &[u8]) -> [u8; 32] {
    copy_sha256(digest::digest(&digest::SHA256, bytes).as_ref())
}

fn copy_sha256(slice: &[u8]) -> [u8; 32] {
    let mut out = [0_u8; 32];
    out.copy_from_slice(slice);
    out
}

fn digest_field(context: &mut digest::Context, value: &[u8]) {
    context.update(&(value.len() as u64).to_be_bytes());
    context.update(value);
}

#[cfg(test)]
#[path = "http_action_tests.rs"]
mod tests;
