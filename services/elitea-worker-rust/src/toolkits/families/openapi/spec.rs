use std::collections::{BTreeMap, BTreeSet, HashSet};
use std::fmt;

use reqwest::{Method, Url};
use serde_json::{Map, Value, json};

const MAX_SPEC_BYTES: usize = 1024 * 1024;
const MAX_SPEC_NODES: usize = 131_072;
const MAX_SPEC_DEPTH: usize = 64;
const MAX_SPEC_STRING_BYTES: usize = 256 * 1024;
const MAX_OPERATIONS: usize = 1_024;
const MAX_PARAMETERS: usize = 256;
const MAX_OPERATION_NAME_BYTES: usize = 64;
const MAX_OPERATION_DESCRIPTION_BYTES: usize = 16 * 1024;
const MAX_PATH_BYTES: usize = 8 * 1024;
const MAX_PARAMETER_NAME_BYTES: usize = 1_024;
const MAX_PARAMETER_SCHEMA_BYTES: usize = 64 * 1024;
const MAX_URL_BYTES: usize = 8 * 1024;
const MAX_RESPONSE_COLLECTION_DEPTH: usize = 4;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum OpenApiSpecErrorCode {
    InvalidSpecification,
    ResourceExhausted,
    UnsupportedSource,
}

pub(crate) struct OpenApiSpecError {
    code: OpenApiSpecErrorCode,
}

impl OpenApiSpecError {
    #[must_use]
    pub(crate) const fn code(&self) -> OpenApiSpecErrorCode {
        self.code
    }
}

impl fmt::Debug for OpenApiSpecError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OpenApiSpecError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for OpenApiSpecError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            OpenApiSpecErrorCode::InvalidSpecification => {
                "the inline OpenAPI specification is invalid"
            }
            OpenApiSpecErrorCode::ResourceExhausted => {
                "the inline OpenAPI specification exceeds its approved limit"
            }
            OpenApiSpecErrorCode::UnsupportedSource => {
                "resolve the OpenAPI specification URL before parsing"
            }
        })
    }
}

impl std::error::Error for OpenApiSpecError {}

pub(crate) struct ParsedOpenApiSpec {
    pub(crate) base_url: Url,
    pub(crate) operations: Vec<OpenApiOperation>,
}

#[derive(Clone)]
pub(crate) struct OpenApiOperation {
    name: Box<str>,
    method: Method,
    path: Box<str>,
    description: Box<str>,
    parameters: Vec<OpenApiParameter>,
    body: Option<OpenApiRequestBody>,
    schema: Value,
    response_collection_paths: Vec<Vec<String>>,
}

impl OpenApiOperation {
    #[must_use]
    pub(crate) fn name(&self) -> &str {
        &self.name
    }

    #[must_use]
    pub(crate) fn method(&self) -> &Method {
        &self.method
    }

    #[must_use]
    pub(crate) fn path(&self) -> &str {
        &self.path
    }

    #[must_use]
    pub(crate) fn description(&self) -> &str {
        &self.description
    }

    #[must_use]
    pub(crate) fn parameters(&self) -> &[OpenApiParameter] {
        &self.parameters
    }

    #[must_use]
    pub(crate) const fn body(&self) -> Option<&OpenApiRequestBody> {
        self.body.as_ref()
    }

    #[must_use]
    pub(crate) fn parameters_schema(&self) -> Value {
        self.schema.clone()
    }

    #[must_use]
    pub(crate) fn response_collection_paths(&self) -> &[Vec<String>] {
        &self.response_collection_paths
    }

    #[must_use]
    pub(crate) fn is_read_only(&self) -> bool {
        matches!(*self.method(), Method::GET | Method::HEAD | Method::OPTIONS)
    }
}

#[derive(Clone)]
pub(crate) struct OpenApiParameter {
    name: Box<str>,
    location: OpenApiParameterLocation,
    required: bool,
    style: Box<str>,
    explode: bool,
    allow_reserved: bool,
    schema: Value,
    description: Option<Box<str>>,
}

impl OpenApiParameter {
    #[must_use]
    pub(crate) fn name(&self) -> &str {
        &self.name
    }

    #[must_use]
    pub(crate) const fn location(&self) -> OpenApiParameterLocation {
        self.location
    }

    #[must_use]
    pub(crate) const fn required(&self) -> bool {
        self.required
    }

    #[must_use]
    pub(crate) fn style(&self) -> &str {
        &self.style
    }

    #[must_use]
    pub(crate) const fn explode(&self) -> bool {
        self.explode
    }

    #[must_use]
    pub(crate) const fn allow_reserved(&self) -> bool {
        self.allow_reserved
    }

    #[must_use]
    pub(crate) fn schema(&self) -> &Value {
        &self.schema
    }

    #[must_use]
    pub(crate) fn description(&self) -> Option<&str> {
        self.description.as_deref()
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) enum OpenApiParameterLocation {
    Path,
    Query,
    Header,
    Cookie,
}

#[derive(Clone)]
pub(crate) struct OpenApiRequestBody {
    required: bool,
}

impl OpenApiRequestBody {
    #[must_use]
    pub(crate) const fn required(&self) -> bool {
        self.required
    }
}

pub(crate) fn parse_operations(
    source: &Value,
    base_override: Option<&str>,
    selected_tools: &[String],
) -> Result<ParsedOpenApiSpec, OpenApiSpecError> {
    let spec = parse_source(source)?;
    validate_tree(&spec)?;
    let root = spec.as_object().ok_or_else(invalid_specification)?;
    if !root
        .get("openapi")
        .and_then(Value::as_str)
        .is_some_and(|version| version.starts_with("3."))
    {
        return Err(invalid_specification());
    }
    let base_url = parse_base_url(root, base_override)?;
    let paths = root
        .get("paths")
        .and_then(Value::as_object)
        .ok_or_else(invalid_specification)?;
    if paths.len() > MAX_OPERATIONS {
        return Err(resource_exhausted());
    }
    let selected = selected_tools
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    let mut seen_names = HashSet::new();
    let mut generated_names = HashSet::new();
    collect_existing_operation_names(paths, &mut generated_names)?;
    let mut operations = Vec::new();
    for (path, path_item) in paths {
        validate_path(path)?;
        let path_item = path_item.as_object().ok_or_else(invalid_specification)?;
        let shared_parameters = raw_parameters(root, path_item.get("parameters"))?;
        for (method_name, raw_operation) in path_item {
            let Some(method) = openapi_method(method_name) else {
                continue;
            };
            let raw_operation = raw_operation
                .as_object()
                .ok_or_else(invalid_specification)?;
            let name = match raw_operation.get("operationId") {
                Some(Value::String(name)) => validate_operation_name(name)?.to_owned(),
                Some(_) => return Err(invalid_specification()),
                None => generate_operation_name(method_name, path, &mut generated_names),
            };
            if !seen_names.insert(name.clone()) {
                return Err(invalid_specification());
            }
            if !selected.is_empty() && !selected.contains(name.as_str()) {
                continue;
            }
            let parameters = merge_parameters(
                root,
                shared_parameters.clone(),
                raw_operation.get("parameters"),
            )?;
            let body = parse_request_body(root, raw_operation.get("requestBody"))?;
            let description = operation_description(&method, path, raw_operation)?;
            let schema = operation_schema(&parameters, body.as_ref())?;
            let response_collection_paths = response_collection_paths(root, raw_operation)?;
            operations.push(OpenApiOperation {
                name: name.into(),
                method,
                path: path.as_str().into(),
                description: description.into(),
                parameters,
                body,
                schema,
                response_collection_paths,
            });
            if operations.len() > MAX_OPERATIONS {
                return Err(resource_exhausted());
            }
        }
    }
    if operations.is_empty() || (!selected.is_empty() && operations.len() != selected.len()) {
        return Err(invalid_specification());
    }
    Ok(ParsedOpenApiSpec {
        base_url,
        operations,
    })
}

pub(super) fn parse_source(source: &Value) -> Result<Value, OpenApiSpecError> {
    match source {
        Value::Object(_) => Ok(source.clone()),
        Value::String(raw) => {
            if raw.len() > MAX_SPEC_BYTES {
                return Err(resource_exhausted());
            }
            let trimmed = raw.trim();
            if trimmed.starts_with("https://") || trimmed.starts_with("http://") {
                return Err(unsupported_source());
            }
            serde_json::from_str(trimmed)
                .or_else(|_| serde_yaml_ng::from_str(trimmed))
                .map_err(|_| invalid_specification())
        }
        _ => Err(invalid_specification()),
    }
}

fn validate_tree(value: &Value) -> Result<(), OpenApiSpecError> {
    let encoded = serde_json::to_vec(value).map_err(|_| invalid_specification())?;
    if encoded.len() > MAX_SPEC_BYTES {
        return Err(resource_exhausted());
    }
    let mut nodes = 0usize;
    let mut stack = vec![(value, 1usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_SPEC_NODES || depth > MAX_SPEC_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(value) => {
                if value.len() > MAX_SPEC_STRING_BYTES {
                    return Err(resource_exhausted());
                }
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                for (key, value) in values {
                    if key.is_empty()
                        || key.len() > MAX_PARAMETER_NAME_BYTES
                        || key.chars().any(char::is_control)
                    {
                        return Err(invalid_specification());
                    }
                    stack.push((value, depth + 1));
                }
            }
            Value::Null | Value::Bool(_) | Value::Number(_) => {}
        }
    }
    Ok(())
}

fn parse_base_url(
    root: &Map<String, Value>,
    base_override: Option<&str>,
) -> Result<Url, OpenApiSpecError> {
    if let Some(base_override) = base_override {
        return parse_https_base(base_override);
    }
    let server = root
        .get("servers")
        .and_then(Value::as_array)
        .and_then(|servers| servers.first())
        .and_then(Value::as_object)
        .ok_or_else(invalid_specification)?;
    let template = server
        .get("url")
        .and_then(Value::as_str)
        .ok_or_else(invalid_specification)?;
    if template.len() > MAX_URL_BYTES {
        return Err(resource_exhausted());
    }
    let variables = server.get("variables").and_then(Value::as_object);
    let mut expanded = template.to_owned();
    while let Some(start) = expanded.find('{') {
        let end = expanded[start + 1..]
            .find('}')
            .map(|offset| start + 1 + offset)
            .ok_or_else(invalid_specification)?;
        let name = &expanded[start + 1..end];
        let default = variables
            .and_then(|variables| variables.get(name))
            .and_then(Value::as_object)
            .and_then(|variable| variable.get("default"))
            .and_then(Value::as_str)
            .ok_or_else(invalid_specification)?;
        if default.is_empty()
            || default.len() > MAX_URL_BYTES
            || default.chars().any(char::is_control)
        {
            return Err(invalid_specification());
        }
        expanded.replace_range(start..=end, default);
        if expanded.len() > MAX_URL_BYTES {
            return Err(resource_exhausted());
        }
    }
    if expanded.contains('}') {
        return Err(invalid_specification());
    }
    parse_https_base(&expanded)
}

fn parse_https_base(value: &str) -> Result<Url, OpenApiSpecError> {
    if value.len() > MAX_URL_BYTES || value.contains('\\') {
        return Err(invalid_specification());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_specification())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_specification());
    }
    if url.path().ends_with('/') && url.path() != "/" {
        let path = url.path().trim_end_matches('/').to_owned();
        url.set_path(&path);
    }
    Ok(url)
}

fn collect_existing_operation_names(
    paths: &Map<String, Value>,
    names: &mut HashSet<String>,
) -> Result<(), OpenApiSpecError> {
    for path_item in paths.values() {
        let path_item = path_item.as_object().ok_or_else(invalid_specification)?;
        for (method, operation) in path_item {
            if openapi_method(method).is_none() {
                continue;
            }
            let operation = operation.as_object().ok_or_else(invalid_specification)?;
            if let Some(name) = operation.get("operationId") {
                let name = name.as_str().ok_or_else(invalid_specification)?;
                validate_operation_name(name)?;
                if !names.insert(name.to_owned()) {
                    return Err(invalid_specification());
                }
            }
        }
    }
    Ok(())
}

fn openapi_method(value: &str) -> Option<Method> {
    match value.to_ascii_lowercase().as_str() {
        "get" => Some(Method::GET),
        "post" => Some(Method::POST),
        "put" => Some(Method::PUT),
        "patch" => Some(Method::PATCH),
        "delete" => Some(Method::DELETE),
        "head" => Some(Method::HEAD),
        "options" => Some(Method::OPTIONS),
        "trace" => Some(Method::TRACE),
        _ => None,
    }
}

fn generate_operation_name(method: &str, path: &str, used: &mut HashSet<String>) -> String {
    let normalized_method = method.to_ascii_lowercase();
    let action = match normalized_method.as_str() {
        "post" => "create",
        "put" | "patch" => "update",
        method => method,
    };
    let mut pieces = vec![action.to_owned()];
    for segment in path.split('/').filter(|segment| !segment.is_empty()) {
        if segment.starts_with('{') && segment.ends_with('}') {
            pieces.push(format!(
                "by_{}",
                sanitize_name(&segment[1..segment.len() - 1])
            ));
        } else {
            pieces.push(sanitize_name(segment));
        }
    }
    if pieces.len() == 1 {
        pieces.push("root".to_owned());
    }
    let base = truncate_name(&pieces.join("_"));
    let mut candidate = base.clone();
    let mut index = 2usize;
    while used.contains(&candidate) {
        let suffix = format!("_{index}");
        let keep = MAX_OPERATION_NAME_BYTES.saturating_sub(suffix.len());
        candidate = format!("{}{}", truncate_at_boundary(&base, keep), suffix);
        index += 1;
    }
    used.insert(candidate.clone());
    candidate
}

fn sanitize_name(value: &str) -> String {
    let mut result = String::with_capacity(value.len());
    let mut previous_underscore = false;
    for character in value.chars() {
        if character.is_ascii_alphanumeric() {
            result.push(character);
            previous_underscore = false;
        } else if !previous_underscore {
            result.push('_');
            previous_underscore = true;
        }
    }
    result.trim_matches('_').to_owned()
}

fn truncate_name(value: &str) -> String {
    let candidate = truncate_at_boundary(value, MAX_OPERATION_NAME_BYTES)
        .trim_end_matches('_')
        .to_owned();
    if candidate.is_empty() {
        "operation".to_owned()
    } else {
        candidate
    }
}

fn truncate_at_boundary(value: &str, maximum: usize) -> &str {
    if value.len() <= maximum {
        return value;
    }
    let boundary = value
        .char_indices()
        .map(|(index, _)| index)
        .take_while(|index| *index <= maximum)
        .last()
        .unwrap_or(0);
    &value[..boundary]
}

fn validate_operation_name(value: &str) -> Result<&str, OpenApiSpecError> {
    if value.is_empty()
        || value.len() > MAX_OPERATION_NAME_BYTES
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
        || !value
            .as_bytes()
            .first()
            .is_some_and(u8::is_ascii_alphabetic)
    {
        return Err(invalid_specification());
    }
    Ok(value)
}

fn validate_path(value: &str) -> Result<(), OpenApiSpecError> {
    if !value.starts_with('/')
        || value.len() > MAX_PATH_BYTES
        || value.contains(['\\', '#', '?'])
        || value.split('/').any(|segment| segment == "..")
        || value.chars().any(char::is_control)
    {
        return Err(invalid_specification());
    }
    Ok(())
}

#[derive(Clone)]
struct RawParameter {
    name: String,
    location: OpenApiParameterLocation,
    required: bool,
    style: String,
    explode: bool,
    allow_reserved: bool,
    schema: Value,
    description: Option<String>,
}

fn raw_parameters(
    root: &Map<String, Value>,
    value: Option<&Value>,
) -> Result<Vec<RawParameter>, OpenApiSpecError> {
    let Some(value) = value else {
        return Ok(Vec::new());
    };
    let parameters = value.as_array().ok_or_else(invalid_specification)?;
    if parameters.len() > MAX_PARAMETERS {
        return Err(resource_exhausted());
    }
    parameters
        .iter()
        .map(|parameter| parse_parameter(root, parameter))
        .collect()
}

fn parse_parameter(
    root: &Map<String, Value>,
    value: &Value,
) -> Result<RawParameter, OpenApiSpecError> {
    let resolved = resolve_object(root, value)?;
    let name = resolved
        .get("name")
        .and_then(Value::as_str)
        .ok_or_else(invalid_specification)?;
    if name.is_empty()
        || name.len() > MAX_PARAMETER_NAME_BYTES
        || name.chars().any(char::is_control)
    {
        return Err(invalid_specification());
    }
    let location = match resolved.get("in").and_then(Value::as_str) {
        Some("path") => OpenApiParameterLocation::Path,
        Some("query") => OpenApiParameterLocation::Query,
        Some("header") => OpenApiParameterLocation::Header,
        Some("cookie") => OpenApiParameterLocation::Cookie,
        _ => return Err(invalid_specification()),
    };
    let required = resolved
        .get("required")
        .map_or(Ok(location == OpenApiParameterLocation::Path), |value| {
            value.as_bool().ok_or_else(invalid_specification)
        })?;
    if location == OpenApiParameterLocation::Path && !required {
        return Err(invalid_specification());
    }
    let style = resolved
        .get("style")
        .and_then(Value::as_str)
        .unwrap_or(match location {
            OpenApiParameterLocation::Path | OpenApiParameterLocation::Header => "simple",
            OpenApiParameterLocation::Query | OpenApiParameterLocation::Cookie => "form",
        });
    if !matches!(
        style,
        "form" | "simple" | "spaceDelimited" | "pipeDelimited"
    ) {
        return Err(invalid_specification());
    }
    let explode = resolved
        .get("explode")
        .map_or(Ok(style == "form"), |value| {
            value.as_bool().ok_or_else(invalid_specification)
        })?;
    let allow_reserved = resolved.get("allowReserved").map_or(Ok(false), |value| {
        value.as_bool().ok_or_else(invalid_specification)
    })?;
    let schema = resolved
        .get("schema")
        .map(|schema| resolve_schema(root, schema, 0, &mut BTreeSet::new()))
        .transpose()?
        .unwrap_or_else(|| json!({"type":"string"}));
    if serde_json::to_vec(&schema)
        .map_err(|_| invalid_specification())?
        .len()
        > MAX_PARAMETER_SCHEMA_BYTES
    {
        return Err(resource_exhausted());
    }
    let description = resolved
        .get("description")
        .and_then(Value::as_str)
        .map(|value| truncate_at_boundary(value, MAX_OPERATION_DESCRIPTION_BYTES).to_owned());
    Ok(RawParameter {
        name: name.to_owned(),
        location,
        required,
        style: style.to_owned(),
        explode,
        allow_reserved,
        schema,
        description,
    })
}

fn merge_parameters(
    root: &Map<String, Value>,
    shared: Vec<RawParameter>,
    operation: Option<&Value>,
) -> Result<Vec<OpenApiParameter>, OpenApiSpecError> {
    let mut merged = Vec::new();
    let mut positions = BTreeMap::new();
    for parameter in shared.into_iter().chain(raw_parameters(root, operation)?) {
        let key = (parameter.location, parameter.name.clone());
        if let Some(index) = positions.get(&key).copied() {
            merged[index] = parameter;
        } else {
            positions.insert(key, merged.len());
            merged.push(parameter);
        }
    }
    if merged.len() > MAX_PARAMETERS {
        return Err(resource_exhausted());
    }
    Ok(merged
        .into_iter()
        .map(|parameter| OpenApiParameter {
            name: parameter.name.into(),
            location: parameter.location,
            required: parameter.required,
            style: parameter.style.into(),
            explode: parameter.explode,
            allow_reserved: parameter.allow_reserved,
            schema: parameter.schema,
            description: parameter.description.map(Into::into),
        })
        .collect())
}

fn operation_schema(
    parameters: &[OpenApiParameter],
    body: Option<&OpenApiRequestBody>,
) -> Result<Value, OpenApiSpecError> {
    let mut properties = Map::new();
    let mut required = Vec::new();
    for parameter in parameters {
        let mut parameter_schema = parameter.schema().clone();
        let schema_object = parameter_schema
            .as_object_mut()
            .ok_or_else(invalid_specification)?;
        schema_object.insert(
            "description".to_owned(),
            Value::String(parameter.description().map_or_else(
                || {
                    format!(
                        "OpenAPI {} parameter.",
                        match parameter.location() {
                            OpenApiParameterLocation::Path => "path",
                            OpenApiParameterLocation::Query => "query",
                            OpenApiParameterLocation::Header => "header",
                            OpenApiParameterLocation::Cookie => "cookie",
                        }
                    )
                },
                str::to_owned,
            )),
        );
        properties.insert(parameter.name().to_owned(), parameter_schema);
        if parameter.required() {
            required.push(Value::String(parameter.name().to_owned()));
        }
    }
    properties.insert(
        "response_search".to_owned(),
        json!({
            "type":["string","null"],
            "minLength":1,
            "maxLength":4096,
            "default":null,
            "description":"Optional words, quoted phrases, and -excluded terms used to select matching objects from a large response collection."
        }),
    );
    properties.insert(
        "response_limit".to_owned(),
        json!({
            "type":["integer","null"],
            "minimum":1,
            "maximum":200,
            "default":null,
            "description":"Maximum collection items returned after response_search. Defaults to 50 when selection is enabled."
        }),
    );
    properties.insert(
        "headers".to_owned(),
        json!({
            "type":["object","null"],
            "additionalProperties":{"type":"string","maxLength":16_384},
            "maxProperties":128,
            "default":null,
            "description":"Optional per-call headers. Credential, host, cookie, length and hop-by-hop headers cannot be overridden."
        }),
    );
    properties.insert(
        "regexp".to_owned(),
        json!({
            "type":["string","null"],
            "minLength":1,
            "maxLength":4096,
            "default":null,
            "description":"Optional Rust regular expression removed from the bounded UTF-8 response."
        }),
    );
    if let Some(body) = body {
        let mut body_schema = json!({
            "type": if body.required() { json!("string") } else { json!(["string","null"]) },
            "minLength":1,
            "maxLength":262_144,
            "description":"JSON request body encoded as a string."
        });
        if !body.required() {
            let Some(body_schema) = body_schema.as_object_mut() else {
                return Err(invalid_specification());
            };
            body_schema.insert("default".to_owned(), Value::Null);
        }
        properties.insert("body_json".to_owned(), body_schema);
        if body.required() {
            required.push(Value::String("body_json".to_owned()));
        }
    }
    Ok(json!({
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    }))
}

fn response_collection_paths(
    root: &Map<String, Value>,
    operation: &Map<String, Value>,
) -> Result<Vec<Vec<String>>, OpenApiSpecError> {
    let Some(responses) = operation.get("responses") else {
        return Ok(Vec::new());
    };
    let responses = responses.as_object().ok_or_else(invalid_specification)?;
    let mut successful = responses
        .iter()
        .filter(|(status, _)| status.starts_with('2'))
        .collect::<Vec<_>>();
    successful.sort_by(|(left, _), (right, _)| {
        (left.as_str() != "200", left.as_str()).cmp(&(right.as_str() != "200", right.as_str()))
    });
    for (_, raw_response) in successful {
        let response = resolve_object(root, raw_response)?;
        let mut schemas = Vec::new();
        if let Some(content) = response.get("content") {
            let content = content.as_object().ok_or_else(invalid_specification)?;
            let mut media_types = content.iter().collect::<Vec<_>>();
            media_types.sort_by(|(left, _), (right, _)| {
                let priority = |media_type: &str| {
                    let media_type = media_type.to_ascii_lowercase();
                    (
                        media_type != "application/json",
                        !media_type.contains("+json"),
                    )
                };
                priority(left)
                    .cmp(&priority(right))
                    .then_with(|| left.cmp(right))
            });
            for (_, media) in media_types {
                if let Some(schema) = media.as_object().and_then(|media| media.get("schema")) {
                    schemas.push(schema);
                }
            }
        }
        if let Some(schema) = response.get("schema") {
            schemas.push(schema);
        }
        for schema in schemas {
            let resolved = resolve_schema(root, schema, 0, &mut BTreeSet::new())?;
            let mut paths = Vec::new();
            collect_response_collection_paths(&resolved, &mut Vec::new(), 0, &mut paths);
            paths.dedup();
            if !paths.is_empty() {
                return Ok(paths);
            }
        }
    }
    Ok(Vec::new())
}

fn collect_response_collection_paths(
    schema: &Value,
    path: &mut Vec<String>,
    depth: usize,
    paths: &mut Vec<Vec<String>>,
) {
    if depth > MAX_RESPONSE_COLLECTION_DEPTH {
        return;
    }
    let Some(schema) = schema.as_object() else {
        return;
    };
    let schema_type = schema.get("type").and_then(Value::as_str);
    if schema_type == Some("array")
        || (schema.contains_key("items") && schema_type != Some("object"))
    {
        if !paths.contains(path) {
            paths.push(path.clone());
        }
        return;
    }
    if schema_type == Some("object")
        && matches!(
            schema.get("additionalProperties"),
            Some(Value::Object(_) | Value::Bool(true))
        )
    {
        if !paths.contains(path) {
            paths.push(path.clone());
        }
        return;
    }
    if let Some(properties) = schema.get("properties").and_then(Value::as_object) {
        for (name, child) in properties {
            path.push(name.clone());
            collect_response_collection_paths(child, path, depth + 1, paths);
            path.pop();
        }
    }
    for composition in ["allOf", "oneOf", "anyOf"] {
        if let Some(branches) = schema.get(composition).and_then(Value::as_array) {
            for branch in branches {
                collect_response_collection_paths(branch, path, depth + 1, paths);
            }
        }
    }
}

fn parse_request_body(
    root: &Map<String, Value>,
    value: Option<&Value>,
) -> Result<Option<OpenApiRequestBody>, OpenApiSpecError> {
    let Some(value) = value else {
        return Ok(None);
    };
    let body = resolve_object(root, value)?;
    let content = body
        .get("content")
        .and_then(Value::as_object)
        .ok_or_else(invalid_specification)?;
    if !content.keys().any(|media_type| {
        media_type.eq_ignore_ascii_case("application/json")
            || media_type.to_ascii_lowercase().ends_with("+json")
    }) {
        return Err(invalid_specification());
    }
    let required = body.get("required").map_or(Ok(false), |value| {
        value.as_bool().ok_or_else(invalid_specification)
    })?;
    Ok(Some(OpenApiRequestBody { required }))
}

fn operation_description(
    method: &Method,
    path: &str,
    operation: &Map<String, Value>,
) -> Result<String, OpenApiSpecError> {
    let mut description = format!("{method} {path}");
    for key in ["summary", "description"] {
        if let Some(value) = operation.get(key) {
            let value = value.as_str().ok_or_else(invalid_specification)?;
            if !value.is_empty() {
                description.push('\n');
                description.push_str(value);
            }
        }
    }
    description.push_str(
        "\nProvide path, query, header, and cookie parameters by their exact OpenAPI names. Use body_json for JSON request bodies.",
    );
    if !matches!(*method, Method::GET | Method::HEAD | Method::OPTIONS) {
        description.push_str(" This operation can cause a remote effect. After an unknown outcome, reconcile provider state before retrying.");
    }
    Ok(truncate_at_boundary(&description, MAX_OPERATION_DESCRIPTION_BYTES).to_owned())
}

fn resolve_object<'a>(
    root: &'a Map<String, Value>,
    value: &'a Value,
) -> Result<&'a Map<String, Value>, OpenApiSpecError> {
    let object = value.as_object().ok_or_else(invalid_specification)?;
    if let Some(reference) = object.get("$ref") {
        let reference = reference.as_str().ok_or_else(invalid_specification)?;
        return resolve_local_ref(root, reference)?
            .as_object()
            .ok_or_else(invalid_specification);
    }
    Ok(object)
}

fn resolve_schema(
    root: &Map<String, Value>,
    value: &Value,
    depth: usize,
    visited: &mut BTreeSet<String>,
) -> Result<Value, OpenApiSpecError> {
    if depth > 32 {
        return Err(resource_exhausted());
    }
    match value {
        Value::Object(object) => {
            if let Some(reference) = object.get("$ref") {
                let reference = reference.as_str().ok_or_else(invalid_specification)?;
                if !visited.insert(reference.to_owned()) {
                    return Err(invalid_specification());
                }
                let resolved = resolve_local_ref(root, reference)?;
                let result = resolve_schema(root, resolved, depth + 1, visited);
                visited.remove(reference);
                return result;
            }
            let mut resolved = Map::new();
            for (key, child) in object {
                resolved.insert(
                    key.clone(),
                    resolve_schema(root, child, depth + 1, visited)?,
                );
            }
            Ok(Value::Object(resolved))
        }
        Value::Array(values) => values
            .iter()
            .map(|value| resolve_schema(root, value, depth + 1, visited))
            .collect::<Result<Vec<_>, _>>()
            .map(Value::Array),
        Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => Ok(value.clone()),
    }
}

fn resolve_local_ref<'a>(
    root: &'a Map<String, Value>,
    reference: &str,
) -> Result<&'a Value, OpenApiSpecError> {
    let path = reference
        .strip_prefix("#/")
        .ok_or_else(invalid_specification)?;
    let mut current = root
        .get(path.split('/').next().ok_or_else(invalid_specification)?)
        .ok_or_else(invalid_specification)?;
    for part in path.split('/').skip(1) {
        let key = part.replace("~1", "/").replace("~0", "~");
        current = current
            .as_object()
            .and_then(|object| object.get(&key))
            .ok_or_else(invalid_specification)?;
    }
    Ok(current)
}

const fn invalid_specification() -> OpenApiSpecError {
    OpenApiSpecError {
        code: OpenApiSpecErrorCode::InvalidSpecification,
    }
}

const fn resource_exhausted() -> OpenApiSpecError {
    OpenApiSpecError {
        code: OpenApiSpecErrorCode::ResourceExhausted,
    }
}

const fn unsupported_source() -> OpenApiSpecError {
    OpenApiSpecError {
        code: OpenApiSpecErrorCode::UnsupportedSource,
    }
}
