use std::collections::BTreeMap;

use serde_json::{Map, Value, json};

use super::client::{
    ItemKind, PostmanClientError, collection_data, collection_items, invalid_input,
    invalid_response, resolve_item, resource_exhausted, sanitize_url,
};

const MAX_ANALYSIS_ITEMS: usize = 4_096;

pub(in crate::toolkits) fn analyze(
    response: &Value,
    configured_collection_id: &str,
    scope: &str,
    target_path: Option<&str>,
    include_improvements: bool,
) -> Result<Value, PostmanClientError> {
    let collection = collection_data(response)?;
    match scope {
        "request" => {
            let path = target_path.ok_or_else(invalid_input)?;
            let request =
                resolve_item(collection, path, ItemKind::Request)?.ok_or_else(invalid_response)?;
            let mut analysis = request_analysis(request)?;
            analysis.insert("request_path".to_owned(), Value::String(path.to_owned()));
            analysis.insert(
                "collection_id".to_owned(),
                Value::String(configured_collection_id.to_owned()),
            );
            if include_improvements {
                let improvements = request_improvements(&analysis);
                analysis.insert(
                    "improvement_count".to_owned(),
                    Value::from(improvements.len()),
                );
                analysis.insert("improvements".to_owned(), Value::Array(improvements));
            }
            Ok(Value::Object(analysis))
        }
        "folder" => {
            let path = target_path.ok_or_else(invalid_input)?;
            let Some(folder) = resolve_item(collection, path, ItemKind::Folder)? else {
                return Ok(json!({"error": format!("Folder '{path}' not found")}));
            };
            let mut analysis = folder_analysis(folder, path)?;
            if include_improvements {
                let improvements = folder_improvements(&analysis);
                analysis.insert(
                    "improvement_count".to_owned(),
                    Value::from(improvements.len()),
                );
                analysis.insert("improvements".to_owned(), Value::Array(improvements));
            }
            Ok(Value::Array(vec![Value::Object(analysis)]))
        }
        "collection" => collection_analysis(collection, include_improvements),
        _ => Err(invalid_input()),
    }
}

fn collection_analysis(
    collection: &Map<String, Value>,
    include_improvements: bool,
) -> Result<Value, PostmanClientError> {
    let info = collection
        .get("info")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    let items = collection_items(collection)?;
    let folders = all_folder_analyses(items)?;
    let issues = collection_issues(collection);
    let score = quality_score(&folders, &issues);
    let security = overall_average(&folders, "avg_security_score");
    let performance = overall_average(&folders, "avg_performance_score");
    let documentation = overall_average(&folders, "avg_documentation_quality");
    let recommendations = recommendations(&issues);
    let mut analysis = Map::from_iter([
        (
            "collection_id".to_owned(),
            info.get("_postman_id")
                .cloned()
                .unwrap_or_else(|| Value::String(String::new())),
        ),
        (
            "collection_name".to_owned(),
            info.get("name")
                .cloned()
                .unwrap_or_else(|| Value::String(String::new())),
        ),
        (
            "total_requests".to_owned(),
            Value::from(count_requests(items)?),
        ),
        ("folders".to_owned(), Value::Array(folders.clone())),
        ("issues".to_owned(), Value::Array(issues.clone())),
        ("recommendations".to_owned(), Value::Array(recommendations)),
        ("score".to_owned(), Value::from(score)),
        ("overall_security_score".to_owned(), Value::from(security)),
        (
            "overall_performance_score".to_owned(),
            Value::from(performance),
        ),
        (
            "overall_documentation_score".to_owned(),
            Value::from(documentation),
        ),
    ]);
    if include_improvements {
        let improvements = collection_improvements(&analysis);
        analysis.insert(
            "improvement_count".to_owned(),
            Value::from(improvements.len()),
        );
        analysis.insert("improvements".to_owned(), Value::Array(improvements));
    }
    Ok(Value::Object(analysis))
}

fn all_folder_analyses(items: &[Value]) -> Result<Vec<Value>, PostmanClientError> {
    let mut output = Vec::new();
    let mut stack = Vec::new();
    for item in items.iter().rev() {
        stack.push((String::new(), item));
    }
    while let Some((parent, item)) = stack.pop() {
        let Some(children) = item.get("item").and_then(Value::as_array) else {
            continue;
        };
        let name = item
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        let path = if parent.is_empty() {
            name.to_owned()
        } else {
            format!("{parent}/{name}")
        };
        output.push(Value::Object(folder_analysis(item, &path)?));
        if output.len() > MAX_ANALYSIS_ITEMS {
            return Err(resource_exhausted());
        }
        for child in children.iter().rev() {
            stack.push((path.clone(), child));
        }
    }
    Ok(output)
}

fn folder_analysis(folder: &Value, path: &str) -> Result<Map<String, Value>, PostmanClientError> {
    let object = folder.as_object().ok_or_else(invalid_response)?;
    let items = object
        .get("item")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    let requests = items
        .iter()
        .filter(|item| item.get("request").is_some())
        .map(request_analysis)
        .map(|result| result.map(Value::Object))
        .collect::<Result<Vec<_>, _>>()?;
    let issues = folder_issues(object, &requests);
    let auth_consistency = auth_consistency(&requests);
    Ok(Map::from_iter([
        (
            "name".to_owned(),
            object
                .get("name")
                .cloned()
                .unwrap_or_else(|| Value::String(String::new())),
        ),
        ("path".to_owned(), Value::String(path.to_owned())),
        (
            "request_count".to_owned(),
            Value::from(count_requests(items)?),
        ),
        ("requests".to_owned(), Value::Array(requests.clone())),
        ("issues".to_owned(), Value::Array(issues)),
        (
            "has_consistent_naming".to_owned(),
            Value::Bool(consistent_naming(items)),
        ),
        (
            "has_proper_structure".to_owned(),
            Value::Bool(
                object
                    .get("description")
                    .is_some_and(|value| !value.is_null())
                    && !items.is_empty(),
            ),
        ),
        (
            "auth_consistency".to_owned(),
            Value::String(auth_consistency.to_owned()),
        ),
        (
            "avg_documentation_quality".to_owned(),
            Value::from(average_quality(&requests)),
        ),
        (
            "avg_security_score".to_owned(),
            Value::from(average_number(&requests, "security_score")),
        ),
        (
            "avg_performance_score".to_owned(),
            Value::from(average_number(&requests, "performance_score")),
        ),
    ]))
}

fn request_analysis(item: &Value) -> Result<Map<String, Value>, PostmanClientError> {
    let object = item.as_object().ok_or_else(invalid_response)?;
    let request = object
        .get("request")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    let name = object
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let method = request
        .get("method")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let normalized_method = method.to_ascii_uppercase();
    let raw_url = request_url(request);
    let has_auth = request.get("auth").is_some_and(truthy) || has_auth_header(request);
    let has_description = object.get("description").is_some_and(truthy)
        || request.get("description").is_some_and(truthy);
    let has_tests = events(item, "test").next().is_some();
    let has_hardcoded_url = hardcoded_url(raw_url);
    let has_hardcoded_data = hardcoded_data(request);
    let has_proper_headers = proper_headers(request);
    let has_variables = variable_usage(request);
    let has_error_handling = error_handling(item);
    let follows_naming_convention = valid_request_name(name);
    let has_security_issues = security_issues(request);
    let has_performance_issues = performance_issues(request);
    let security_score = security_score(request, has_auth, has_security_issues);
    let performance_score = performance_score(request, has_performance_issues);
    let flags = RequestFlags::default()
        .with(RequestFlag::HardcodedUrl, has_hardcoded_url)
        .with(RequestFlag::HardcodedData, has_hardcoded_data)
        .with(RequestFlag::ProperHeaders, has_proper_headers)
        .with(RequestFlag::Variables, has_variables)
        .with(RequestFlag::ErrorHandling, has_error_handling)
        .with(RequestFlag::NamingConvention, follows_naming_convention)
        .with(RequestFlag::SecurityIssues, has_security_issues)
        .with(RequestFlag::PerformanceIssues, has_performance_issues)
        .with(RequestFlag::Description, has_description)
        .with(RequestFlag::Auth, has_auth)
        .with(RequestFlag::Tests, has_tests);
    let issues = request_issues(name, &normalized_method, flags);
    let mut output = Map::from_iter([
        ("name".to_owned(), Value::String(name.to_owned())),
        ("method".to_owned(), Value::String(method.to_owned())),
        ("url".to_owned(), Value::String(sanitize_url(raw_url)?)),
        ("has_auth".to_owned(), Value::Bool(has_auth)),
        ("has_description".to_owned(), Value::Bool(has_description)),
        ("has_tests".to_owned(), Value::Bool(has_tests)),
        (
            "has_examples".to_owned(),
            Value::Bool(
                object
                    .get("response")
                    .and_then(Value::as_array)
                    .is_some_and(|values| !values.is_empty()),
            ),
        ),
        ("issues".to_owned(), Value::Array(issues)),
    ]);
    append_request_metrics(
        &mut output,
        &RequestMetrics {
            item,
            object,
            request,
            flags,
            security_score,
            performance_score,
        },
    );
    Ok(output)
}

struct RequestMetrics<'a> {
    item: &'a Value,
    object: &'a Map<String, Value>,
    request: &'a Map<String, Value>,
    flags: RequestFlags,
    security_score: i64,
    performance_score: i64,
}

fn append_request_metrics(output: &mut Map<String, Value>, metrics: &RequestMetrics<'_>) {
    output.extend([
        (
            "has_hardcoded_url".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::HardcodedUrl)),
        ),
        (
            "has_hardcoded_data".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::HardcodedData)),
        ),
        (
            "has_proper_headers".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::ProperHeaders)),
        ),
        (
            "has_variables".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::Variables)),
        ),
        (
            "has_error_handling".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::ErrorHandling)),
        ),
        (
            "follows_naming_convention".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::NamingConvention)),
        ),
        (
            "has_security_issues".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::SecurityIssues)),
        ),
        (
            "has_performance_issues".to_owned(),
            Value::Bool(metrics.flags.has(RequestFlag::PerformanceIssues)),
        ),
        (
            "auth_type".to_owned(),
            metrics
                .request
                .get("auth")
                .and_then(|auth| auth.get("type"))
                .cloned()
                .unwrap_or(Value::Null),
        ),
        (
            "response_examples".to_owned(),
            Value::from(
                metrics
                    .object
                    .get("response")
                    .and_then(Value::as_array)
                    .map_or(0, Vec::len),
            ),
        ),
        (
            "test_coverage".to_owned(),
            Value::String(test_coverage(metrics.item).to_owned()),
        ),
        (
            "documentation_quality".to_owned(),
            Value::String(documentation_quality(metrics.item).to_owned()),
        ),
        (
            "security_score".to_owned(),
            Value::from(metrics.security_score),
        ),
        (
            "performance_score".to_owned(),
            Value::from(metrics.performance_score),
        ),
    ]);
}

fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(value) => *value,
        Value::String(value) => !value.is_empty(),
        Value::Array(value) => !value.is_empty(),
        Value::Object(value) => !value.is_empty(),
        Value::Number(_) => true,
    }
}

fn request_url(request: &Map<String, Value>) -> &str {
    match request.get("url") {
        Some(Value::String(value)) => value,
        Some(Value::Object(value)) => value.get("raw").and_then(Value::as_str).unwrap_or_default(),
        _ => "",
    }
}

fn headers(request: &Map<String, Value>) -> impl Iterator<Item = &Map<String, Value>> {
    request
        .get("header")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(Value::as_object)
}

fn events<'a>(item: &'a Value, kind: &'a str) -> impl Iterator<Item = &'a Value> {
    item.get("event")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter(move |event| event.get("listen").and_then(Value::as_str) == Some(kind))
}

fn has_auth_header(request: &Map<String, Value>) -> bool {
    headers(request).any(|header| {
        matches!(
            header
                .get("key")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_ascii_lowercase()
                .as_str(),
            "authorization" | "x-api-key" | "x-auth-token"
        )
    })
}

fn hardcoded_url(url: &str) -> bool {
    if url.contains("{{") {
        return false;
    }
    let lower = url.to_ascii_lowercase();
    lower.starts_with("http://localhost")
        || lower.starts_with("https://localhost")
        || lower.contains("api.example.com")
        || lower.contains("staging.")
        || lower.contains("dev.")
        || lower.contains("test.")
        || UrlParts::parse(url).is_some_and(|parts| {
            parts.ipv4_host || matches!(parts.tld, Some("com" | "org" | "net" | "io" | "dev"))
        })
}

struct UrlParts<'a> {
    ipv4_host: bool,
    tld: Option<&'a str>,
}

impl<'a> UrlParts<'a> {
    fn parse(value: &'a str) -> Option<Self> {
        let (_, rest) = value.split_once("://")?;
        let authority = rest.split(['/', '?', '#']).next()?;
        let host = authority.rsplit('@').next()?.split(':').next()?;
        let ipv4_host =
            host.split('.').count() == 4 && host.split('.').all(|part| part.parse::<u8>().is_ok());
        let tld = host.rsplit('.').next();
        Some(Self { ipv4_host, tld })
    }
}

fn hardcoded_data(request: &Map<String, Value>) -> bool {
    let header_secret = headers(request).any(|header| {
        let key = header
            .get("key")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_ascii_lowercase();
        (key.contains("token") || key.contains("key") || key.contains("secret"))
            && !header
                .get("value")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .contains("{{")
    });
    if header_secret {
        return true;
    }
    let Some(raw) = request
        .get("body")
        .and_then(|body| body.get("raw"))
        .and_then(Value::as_str)
    else {
        return false;
    };
    serde_json::from_str(raw).map_or_else(
        |_| {
            let compact = raw.to_ascii_lowercase();
            ["\"api_key\"", "\"token\"", "\"password\""]
                .iter()
                .any(|needle| compact.contains(needle) && !compact.contains("{{"))
        },
        |value| contains_hardcoded(&value),
    )
}

fn contains_hardcoded(value: &Value) -> bool {
    let Some(object) = value.as_object() else {
        return false;
    };
    object.iter().any(|(key, value)| match value {
        Value::String(value) => {
            let sensitive = matches!(
                key.to_ascii_lowercase().as_str(),
                "token" | "key" | "secret" | "password" | "api_key" | "client_id" | "client_secret"
            );
            !value.contains("{{")
                && (sensitive || value.starts_with("http") || looks_like_email(value))
        }
        Value::Object(_) => contains_hardcoded(value),
        _ => false,
    })
}

fn looks_like_email(value: &str) -> bool {
    let Some((_, domain)) = value.split_once('@') else {
        return false;
    };
    domain.contains('.') && domain.rsplit('.').next().is_some_and(|tld| tld.len() >= 2)
}

fn proper_headers(request: &Map<String, Value>) -> bool {
    let names = headers(request)
        .filter_map(|header| header.get("key").and_then(Value::as_str))
        .map(str::to_ascii_lowercase)
        .collect::<Vec<_>>();
    let method = request
        .get("method")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_ascii_uppercase();
    if matches!(method.as_str(), "POST" | "PUT" | "PATCH")
        && request.get("body").is_some_and(truthy)
        && !names.iter().any(|name| name == "content-type")
    {
        return false;
    }
    !matches!(method.as_str(), "GET" | "POST" | "PUT" | "PATCH")
        || names.iter().any(|name| name == "accept")
}

fn variable_usage(request: &Map<String, Value>) -> bool {
    request_url(request).contains("{{")
        || headers(request).any(|header| {
            header
                .get("value")
                .and_then(Value::as_str)
                .is_some_and(|value| value.contains("{{"))
        })
        || request
            .get("body")
            .and_then(|body| body.get("raw"))
            .and_then(Value::as_str)
            .is_some_and(|value| value.contains("{{"))
}

fn event_code(event: &Value) -> String {
    event
        .get("script")
        .and_then(|script| script.get("exec"))
        .and_then(Value::as_array)
        .map(|lines| {
            lines
                .iter()
                .filter_map(Value::as_str)
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default()
}

fn error_handling(item: &Value) -> bool {
    events(item, "test").any(|event| {
        let code = event_code(event).to_ascii_lowercase();
        code.contains('4') || code.contains('5') || code.contains("error") || code.contains("fail")
    })
}

fn valid_request_name(name: &str) -> bool {
    name.len() > 3
        && !name.to_ascii_lowercase().contains("test")
        && !name.to_ascii_lowercase().contains("temp")
        && name.bytes().enumerate().all(|(index, byte)| {
            if index == 0 {
                byte.is_ascii_alphabetic()
            } else {
                byte.is_ascii_alphanumeric() || matches!(byte, b' ' | b'-' | b'_')
            }
        })
}

fn security_issues(request: &Map<String, Value>) -> bool {
    let url = request_url(request);
    let lower = url.to_ascii_lowercase();
    let query_secret = lower.split_once('?').is_some_and(|(_, query)| {
        query.split('&').any(|pair| {
            pair.split_once('=').is_some_and(|(key, value)| {
                matches!(key, "token" | "key" | "password" | "secret") && !value.is_empty()
            })
        })
    });
    let weak_basic = request
        .get("auth")
        .and_then(|auth| auth.get("type"))
        .and_then(Value::as_str)
        == Some("basic")
        && !url.starts_with("https");
    query_secret
        || weak_basic
        || headers(request).any(|header| {
            let key = header
                .get("key")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_ascii_lowercase();
            key.contains("secret") || key.contains("password")
        })
}

fn performance_issues(request: &Map<String, Value>) -> bool {
    let body_large = request
        .get("body")
        .and_then(|body| body.get("raw"))
        .and_then(Value::as_str)
        .is_some_and(|body| body.len() > 10_000);
    let too_many_headers = headers(request).count() > 20;
    let too_many_query = request_url(request)
        .split_once('?')
        .is_some_and(|(_, query)| !query.is_empty() && query.split('&').count() > 15);
    body_large || too_many_headers || too_many_query
}

fn security_score(request: &Map<String, Value>, has_auth: bool, has_security_issues: bool) -> i64 {
    let method = request
        .get("method")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_ascii_uppercase();
    let mut score = 100i64;
    if !has_auth && matches!(method.as_str(), "POST" | "PUT" | "PATCH" | "DELETE") {
        score -= 40;
    }
    if has_security_issues {
        score -= 30;
    }
    if request_url(request).starts_with("http://") {
        score -= 20;
    }
    if request
        .get("auth")
        .and_then(|auth| auth.get("type"))
        .and_then(Value::as_str)
        == Some("basic")
    {
        score -= 10;
    }
    score.max(0)
}

fn performance_score(request: &Map<String, Value>, has_issues: bool) -> i64 {
    let mut score = if has_issues { 50i64 } else { 100i64 };
    let names = headers(request)
        .filter_map(|header| header.get("key").and_then(Value::as_str))
        .map(str::to_ascii_lowercase)
        .collect::<Vec<_>>();
    if !names.iter().any(|name| name == "cache-control") {
        score -= 10;
    }
    if !names.iter().any(|name| name == "accept-encoding") {
        score -= 10;
    }
    score.max(0)
}

fn test_coverage(item: &Value) -> &'static str {
    let events = events(item, "test").collect::<Vec<_>>();
    if events.is_empty() {
        return "none";
    }
    let code = events
        .iter()
        .map(|event| event_code(event))
        .collect::<Vec<_>>()
        .join("\n");
    let checks = [
        code.contains("pm.response.code") || code.contains("status"),
        code.contains("responseTime"),
        code.contains("pm.response.json") || code.contains("body"),
        code.contains('4') || code.contains('5'),
    ];
    match checks.into_iter().filter(|value| *value).count() {
        3.. => "comprehensive",
        1.. => "basic",
        _ => "none",
    }
}

fn documentation_quality(item: &Value) -> &'static str {
    let description = item
        .get("description")
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .or_else(|| {
            item.get("request")
                .and_then(|request| request.get("description"))
                .and_then(Value::as_str)
        })
        .unwrap_or_default();
    if description.is_empty() {
        return "none";
    }
    let lower = description.to_ascii_lowercase();
    let factors = ["parameter", "response", "example", "auth", "error"]
        .into_iter()
        .filter(|factor| lower.contains(factor))
        .count();
    match factors {
        4.. => "excellent",
        2.. => "good",
        1 => "minimal",
        _ if description.len() > 50 => "minimal",
        _ => "none",
    }
}

#[derive(Clone, Copy, Default)]
struct RequestFlags(u16);

#[derive(Clone, Copy)]
enum RequestFlag {
    Description,
    Auth,
    Tests,
    HardcodedUrl,
    HardcodedData,
    ProperHeaders,
    Variables,
    ErrorHandling,
    NamingConvention,
    SecurityIssues,
    PerformanceIssues,
}

impl RequestFlags {
    const fn with(mut self, flag: RequestFlag, enabled: bool) -> Self {
        if enabled {
            self.0 |= request_flag_mask(flag);
        }
        self
    }

    const fn has(self, flag: RequestFlag) -> bool {
        self.0 & request_flag_mask(flag) != 0
    }
}

const fn request_flag_mask(flag: RequestFlag) -> u16 {
    match flag {
        RequestFlag::Description => 1 << 0,
        RequestFlag::Auth => 1 << 1,
        RequestFlag::Tests => 1 << 2,
        RequestFlag::HardcodedUrl => 1 << 3,
        RequestFlag::HardcodedData => 1 << 4,
        RequestFlag::ProperHeaders => 1 << 5,
        RequestFlag::Variables => 1 << 6,
        RequestFlag::ErrorHandling => 1 << 7,
        RequestFlag::NamingConvention => 1 << 8,
        RequestFlag::SecurityIssues => 1 << 9,
        RequestFlag::PerformanceIssues => 1 << 10,
    }
}

fn request_issues(name: &str, method: &str, flags: RequestFlags) -> Vec<Value> {
    let mut issues = Vec::new();
    if !flags.has(RequestFlag::Description) {
        issues.push(issue(
            "warning",
            "medium",
            "Request lacks description",
            name,
            "Add a clear description explaining what this request does",
        ));
    }
    if !flags.has(RequestFlag::Auth) && matches!(method, "POST" | "PUT" | "PATCH" | "DELETE") {
        issues.push(issue(
            "warning",
            "high",
            "Sensitive operation without authentication",
            name,
            "Add authentication for this request",
        ));
    }
    if !flags.has(RequestFlag::Tests) {
        issues.push(issue(
            "info",
            "high",
            "Request lacks test scripts",
            name,
            "Add test scripts to validate response",
        ));
    }
    if flags.has(RequestFlag::HardcodedUrl) {
        issues.push(issue(
            "warning",
            "high",
            "Request contains hardcoded URL",
            name,
            "Replace hardcoded URLs with environment variables",
        ));
    }
    if flags.has(RequestFlag::SecurityIssues) {
        issues.push(issue(
            "error",
            "high",
            "Security vulnerabilities detected",
            name,
            "Address security issues such as exposed credentials",
        ));
    }
    issues
}

fn issue(kind: &str, severity: &str, message: &str, location: &str, suggestion: &str) -> Value {
    json!({"type":kind,"severity":severity,"message":message,"location":location,"suggestion":suggestion})
}

fn collection_issues(collection: &Map<String, Value>) -> Vec<Value> {
    let mut issues = Vec::new();
    if !collection
        .get("info")
        .and_then(|info| info.get("description"))
        .is_some_and(truthy)
    {
        issues.push(issue(
            "warning",
            "medium",
            "Collection lacks description",
            "Collection root",
            "Add a description explaining the purpose of this collection",
        ));
    }
    if !collection.get("auth").is_some_and(truthy) {
        issues.push(issue(
            "info",
            "low",
            "Collection lacks default authentication",
            "Collection root",
            "Consider setting up collection-level authentication",
        ));
    }
    issues
}

fn folder_issues(folder: &Map<String, Value>, requests: &[Value]) -> Vec<Value> {
    let name = folder
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let mut issues = Vec::new();
    if !folder.get("description").is_some_and(truthy) {
        issues.push(issue(
            "warning",
            "low",
            "Folder lacks description",
            name,
            "Add a description explaining the purpose of this folder",
        ));
    }
    if requests.is_empty()
        && folder
            .get("item")
            .and_then(Value::as_array)
            .is_none_or(Vec::is_empty)
    {
        issues.push(issue(
            "warning",
            "medium",
            "Empty folder",
            name,
            "Consider removing empty folders or adding requests",
        ));
    }
    issues
}

fn count_requests(items: &[Value]) -> Result<usize, PostmanClientError> {
    let mut count = 0usize;
    let mut stack = items.iter().collect::<Vec<_>>();
    while let Some(item) = stack.pop() {
        if item.get("request").is_some() {
            count = count.checked_add(1).ok_or_else(resource_exhausted)?;
        } else if let Some(children) = item.get("item").and_then(Value::as_array) {
            stack.extend(children);
        }
        if count > MAX_ANALYSIS_ITEMS {
            return Err(resource_exhausted());
        }
    }
    Ok(count)
}

fn consistent_naming(items: &[Value]) -> bool {
    if items.len() <= 1 {
        return true;
    }
    let patterns = items
        .iter()
        .map(|item| {
            let name = item
                .get("name")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_ascii_lowercase();
            if ascii_pattern(&name, b'_') {
                "snake"
            } else if camel_pattern(&name) {
                "camel"
            } else if ascii_pattern(&name, b'-') {
                "kebab"
            } else {
                "mixed"
            }
        })
        .collect::<Vec<_>>();
    patterns
        .first()
        .is_some_and(|first| *first != "mixed" && patterns.iter().all(|value| value == first))
}

fn ascii_pattern(name: &str, separator: u8) -> bool {
    let mut parts = name.split(char::from(separator));
    parts.next().is_some_and(|part| {
        !part.is_empty()
            && part
                .bytes()
                .next()
                .is_some_and(|byte| byte.is_ascii_lowercase())
            && part
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit())
    }) && parts.all(|part| {
        !part.is_empty()
            && part
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit())
    })
}

fn camel_pattern(name: &str) -> bool {
    name.bytes()
        .next()
        .is_some_and(|byte| byte.is_ascii_lowercase())
        && name.bytes().all(|byte| byte.is_ascii_alphanumeric())
}

fn auth_consistency(requests: &[Value]) -> &'static str {
    if requests.is_empty() {
        return "none";
    }
    let first = requests[0]
        .get("auth_type")
        .and_then(Value::as_str)
        .unwrap_or("none");
    if requests.iter().all(|request| {
        request
            .get("auth_type")
            .and_then(Value::as_str)
            .unwrap_or("none")
            == first
    }) {
        if first == "none" {
            "none"
        } else {
            "consistent"
        }
    } else {
        "mixed"
    }
}

fn average_quality(requests: &[Value]) -> i64 {
    let sum = requests
        .iter()
        .map(
            |request| match request.get("documentation_quality").and_then(Value::as_str) {
                Some("excellent") => 100,
                Some("good") => 75,
                Some("minimal") => 50,
                _ => 0,
            },
        )
        .sum();
    rounded_average(sum, requests.len())
}

fn average_number(requests: &[Value], field: &str) -> i64 {
    let sum = requests
        .iter()
        .filter_map(|request| request.get(field).and_then(Value::as_i64))
        .sum();
    rounded_average(sum, requests.len())
}

fn rounded_average(sum: i64, count: usize) -> i64 {
    if count == 0 {
        return 0;
    }
    let Ok(count) = i64::try_from(count) else {
        return 0;
    };
    let quotient = sum / count;
    let remainder = sum % count;
    match (remainder * 2).cmp(&count) {
        std::cmp::Ordering::Less => quotient,
        std::cmp::Ordering::Equal if quotient % 2 == 0 => quotient,
        std::cmp::Ordering::Greater | std::cmp::Ordering::Equal => quotient + 1,
    }
}

fn quality_score(folders: &[Value], issues: &[Value]) -> i64 {
    let mut score = 100i64;
    for issue in issues {
        score -= severity_cost(issue, 10, 5, 2);
    }
    for folder in folders {
        for issue in folder
            .get("issues")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            score -= severity_cost(issue, 5, 3, 1);
        }
        for request in folder
            .get("requests")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            for issue in request
                .get("issues")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                score -= severity_cost(issue, 3, 2, 1);
            }
        }
    }
    score.clamp(0, 100)
}

fn severity_cost(issue: &Value, high: i64, medium: i64, low: i64) -> i64 {
    match issue
        .get("severity")
        .and_then(Value::as_str)
        .unwrap_or("low")
    {
        "high" => high,
        "medium" => medium,
        _ => low,
    }
}

fn overall_average(folders: &[Value], field: &str) -> i64 {
    let scores = folders
        .iter()
        .filter_map(|folder| folder.get(field).and_then(Value::as_i64))
        .filter(|score| *score > 0)
        .collect::<Vec<_>>();
    rounded_average(scores.iter().sum(), scores.len())
}

fn recommendations(issues: &[Value]) -> Vec<Value> {
    let mut ordered = Vec::<(String, usize)>::new();
    for issue in issues {
        let suggestion = issue
            .get("suggestion")
            .and_then(Value::as_str)
            .unwrap_or_default();
        if suggestion.is_empty() {
            continue;
        }
        if let Some((_, count)) = ordered.iter_mut().find(|(value, _)| value == suggestion) {
            *count += 1;
        } else {
            ordered.push((suggestion.to_owned(), 1));
        }
    }
    ordered.sort_by_key(|(_, count)| std::cmp::Reverse(*count));
    ordered
        .into_iter()
        .take(10)
        .map(|(suggestion, count)| {
            Value::String(if count > 1 {
                format!("{suggestion} ({count} instances)")
            } else {
                suggestion
            })
        })
        .collect()
}

fn improvement(
    id: &str,
    title: &str,
    description: impl Into<String>,
    priority: &str,
    category: &str,
    impact: &str,
) -> Value {
    let description = description.into();
    json!({"id":id,"title":title,"description":description,"priority":priority,"category":category,"impact":impact})
}

fn collection_improvements(analysis: &Map<String, Value>) -> Vec<Value> {
    let mut output = Vec::new();
    let score = analysis
        .get("score")
        .and_then(Value::as_i64)
        .unwrap_or_default();
    let security = analysis
        .get("overall_security_score")
        .and_then(Value::as_i64)
        .unwrap_or_default();
    let documentation = analysis
        .get("overall_documentation_score")
        .and_then(Value::as_i64)
        .unwrap_or_default();
    if score < 80 {
        output.push(improvement(
            "collection-quality",
            "Improve Overall Collection Quality",
            format!(
                "Collection quality score is {score}/100. Focus on addressing high-priority issues."
            ),
            "high",
            "quality",
            "high",
        ));
    }
    if security < 70 {
        output.push(improvement(
            "security-enhancement",
            "Enhance Security Practices",
            format!("Security score is {security}/100. Review authentication and data handling."),
            "high",
            "security",
            "high",
        ));
    }
    if documentation < 60 {
        output.push(improvement(
            "documentation-improvement",
            "Improve Documentation",
            format!("Documentation score is {documentation}/100. Add descriptions and examples."),
            "medium",
            "documentation",
            "medium",
        ));
    }
    let issue_counts = request_issue_counts(analysis.get("folders"));
    if issue_counts
        .get("Request lacks test scripts")
        .copied()
        .unwrap_or_default()
        > 3
    {
        let count = issue_counts["Request lacks test scripts"];
        output.push(improvement(
            "add-test-scripts",
            "Add Test Scripts to Requests",
            format!("Found {count} requests without test scripts."),
            "medium",
            "testing",
            "medium",
        ));
    }
    if issue_counts
        .get("Request contains hardcoded URL")
        .copied()
        .unwrap_or_default()
        > 2
    {
        let count = issue_counts["Request contains hardcoded URL"];
        output.push(improvement(
            "use-environment-variables",
            "Use Environment Variables",
            format!("Found {count} requests with hardcoded URLs."),
            "high",
            "maintainability",
            "high",
        ));
    }
    output
}

fn folder_improvements(analysis: &Map<String, Value>) -> Vec<Value> {
    let mut output = Vec::new();
    let security = analysis
        .get("avg_security_score")
        .and_then(Value::as_i64)
        .unwrap_or_default();
    let documentation = analysis
        .get("avg_documentation_quality")
        .and_then(Value::as_i64)
        .unwrap_or_default();
    if security < 70 {
        output.push(improvement(
            "folder-security",
            "Improve Folder Security",
            format!(
                "Folder security score is {security}/100. Review authentication and data handling."
            ),
            "high",
            "security",
            "high",
        ));
    }
    if documentation < 60 {
        output.push(improvement(
            "folder-documentation",
            "Improve Folder Documentation",
            format!("Documentation quality is {documentation}/100. Add descriptions and examples."),
            "medium",
            "documentation",
            "medium",
        ));
    }
    if analysis
        .get("has_consistent_naming")
        .and_then(Value::as_bool)
        == Some(false)
    {
        output.push(improvement(
            "folder-naming-consistency",
            "Improve Naming Consistency",
            "Folder contains inconsistent naming patterns. Consider standardizing request names."
                .to_owned(),
            "low",
            "organization",
            "low",
        ));
    }
    let counts = issue_counts_for_requests(analysis.get("requests"));
    if let Some(count) = counts
        .get("Request lacks test scripts")
        .filter(|count| **count > 0)
    {
        output.push(improvement(
            "folder-add-tests",
            "Add Test Scripts",
            format!("Found {count} requests in this folder without test scripts."),
            "medium",
            "testing",
            "medium",
        ));
    }
    output
}

fn issue_counts_for_requests(requests: Option<&Value>) -> BTreeMap<String, usize> {
    let mut counts = BTreeMap::new();
    for request in requests.and_then(Value::as_array).into_iter().flatten() {
        for issue in request
            .get("issues")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            if let Some(message) = issue.get("message").and_then(Value::as_str) {
                *counts.entry(message.to_owned()).or_default() += 1;
            }
        }
    }
    counts
}

fn request_issue_counts(folders: Option<&Value>) -> BTreeMap<String, usize> {
    let mut counts = BTreeMap::new();
    for folder in folders.and_then(Value::as_array).into_iter().flatten() {
        for request in folder
            .get("requests")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            for issue in request
                .get("issues")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                if let Some(message) = issue.get("message").and_then(Value::as_str) {
                    *counts.entry(message.to_owned()).or_default() += 1;
                }
            }
        }
    }
    counts
}

fn request_improvements(analysis: &Map<String, Value>) -> Vec<Value> {
    let mut output = Vec::new();
    let security = analysis
        .get("security_score")
        .and_then(Value::as_i64)
        .unwrap_or(100);
    let performance = analysis
        .get("performance_score")
        .and_then(Value::as_i64)
        .unwrap_or(100);
    if security < 70 {
        output.push(improvement(
            "request-security",
            "Improve Request Security",
            format!("Security score is {security}/100. Review authentication and data handling."),
            "high",
            "security",
            "high",
        ));
    }
    if performance < 70 {
        output.push(improvement(
            "request-performance",
            "Optimize Request Performance",
            format!("Performance score is {performance}/100. Review request structure and size."),
            "medium",
            "performance",
            "medium",
        ));
    }
    extend_request_check_improvements(&mut output, analysis);
    output
}

fn extend_request_check_improvements(output: &mut Vec<Value>, analysis: &Map<String, Value>) {
    let checks = [
        (
            "has_description",
            false,
            "request-add-description",
            "Add Request Description",
            "Request lacks a description. Add documentation to explain its purpose.",
            "low",
            "documentation",
            "low",
        ),
        (
            "has_auth",
            false,
            "request-add-auth",
            "Add Authentication",
            "Request lacks authentication. Consider adding appropriate auth method.",
            "high",
            "security",
            "high",
        ),
        (
            "has_tests",
            false,
            "request-add-tests",
            "Add Test Scripts",
            "Request lacks test scripts. Add tests to validate responses.",
            "medium",
            "testing",
            "medium",
        ),
        (
            "has_hardcoded_url",
            true,
            "request-use-variables",
            "Use Environment Variables",
            "Request contains hardcoded URLs. Use environment variables for better maintainability.",
            "high",
            "maintainability",
            "high",
        ),
        (
            "has_hardcoded_data",
            true,
            "request-parameterize-data",
            "Parameterize Request Data",
            "Request contains hardcoded data. Consider using variables or dynamic values.",
            "medium",
            "maintainability",
            "medium",
        ),
        (
            "has_proper_headers",
            false,
            "request-fix-headers",
            "Fix Request Headers",
            "Request headers may be missing or incorrect. Review and add appropriate headers.",
            "medium",
            "correctness",
            "medium",
        ),
        (
            "follows_naming_convention",
            false,
            "request-naming-convention",
            "Follow Naming Convention",
            "Request name doesn't follow standard conventions. Consider renaming for consistency.",
            "low",
            "organization",
            "low",
        ),
    ];
    for (field, trigger, id, title, description, priority, category, impact) in checks {
        if analysis.get(field).and_then(Value::as_bool) == Some(trigger) {
            output.push(improvement(
                id,
                title,
                description.to_owned(),
                priority,
                category,
                impact,
            ));
        }
    }
}
