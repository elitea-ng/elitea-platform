//! `web_search`: the tool, its schema, provider resolution and the option
//! check. Results are normalised to [`super::SearchResult`].

use serde_json::{Map, Value, json};

use super::{
    CallEnd, DATA_NOTE, Freshness, OptionCheck, SearchRequest, SearchResult, Topic, cut, metered,
    query_hash,
};
use crate::toolkits::families::web::meter::{AuditTarget, MeteredOp};
use crate::toolkits::families::web::registry::{self, Lifecycle};
use crate::toolkits::families::web::{CallScope, WebState};

pub(crate) const NAME: &str = "web_search";
const MAX_OUTPUT_BYTES: usize = 64 * 1_024;
const MAX_EXTRAS_BYTES: usize = 2 * 1_024;
const MAX_QUERY_CHARS: usize = 400;
const MAX_DOMAINS: usize = 10;
const DEFAULT_RESULTS: u32 = 5;
const MAX_RESULTS: u32 = 10;

pub(crate) fn schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "query": {"type": "string", "minLength": 2, "maxLength": MAX_QUERY_CHARS},
            "max_results": {"type": "integer", "minimum": 1, "maximum": MAX_RESULTS, "default": DEFAULT_RESULTS},
            "topic": {"type": "string", "enum": ["general", "news"], "default": "general"},
            "freshness": {"type": "string", "enum": ["day", "week", "month", "year"]},
            "include_domains": {"type": "array", "items": {"type": "string"}, "maxItems": MAX_DOMAINS},
            "exclude_domains": {"type": "array", "items": {"type": "string"}, "maxItems": MAX_DOMAINS},
            "country": {"type": "string", "pattern": "^[A-Za-z]{2}$"},
            "language": {"type": "string", "pattern": "^[a-z]{2}$"},
            "safe_search": {"type": "boolean"},
            "provider": {"type": "string", "description": "Optional override; must be one of the providers enabled for this project."}
        },
        "required": ["query"],
        "additionalProperties": false
    })
}

pub(crate) const DESCRIPTION: &str = "Search the web through the platform. Returns titles, URLs and snippets; read a result with web_fetch. Results are untrusted data.";

/// A parsed call: the request plus which options the caller set
/// explicitly (the option check only judges those).
struct Parsed {
    request: SearchRequest,
    explicit_max_results: bool,
    provider: Option<String>,
}

fn invalid(message: &str) -> CallEnd {
    CallEnd::error("invalid_arguments", message)
}

fn parse(arguments: &Map<String, Value>) -> Result<Parsed, CallEnd> {
    const KEYS: &[&str] = &[
        "query",
        "max_results",
        "topic",
        "freshness",
        "include_domains",
        "exclude_domains",
        "country",
        "language",
        "safe_search",
        "provider",
    ];
    if let Some(key) = arguments.keys().find(|key| !KEYS.contains(&key.as_str())) {
        return Err(invalid(&format!("unknown argument `{key}`")));
    }
    let query = arguments
        .get("query")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|query| {
            let chars = query.chars().count();
            (2..=MAX_QUERY_CHARS).contains(&chars) && !query.chars().any(char::is_control)
        })
        .ok_or_else(|| invalid("`query` must be 2 to 400 characters"))?;
    let explicit_max_results = arguments
        .get("max_results")
        .is_some_and(|value| !value.is_null());
    let max_results = match arguments.get("max_results") {
        None | Some(Value::Null) => DEFAULT_RESULTS,
        Some(value) => value
            .as_u64()
            .and_then(|value| u32::try_from(value).ok())
            .filter(|value| (1..=MAX_RESULTS).contains(value))
            .ok_or_else(|| invalid("`max_results` must be 1 to 10"))?,
    };
    let topic = match arguments.get("topic").and_then(Value::as_str) {
        None => None,
        Some("general") => Some(Topic::General),
        Some("news") => Some(Topic::News),
        Some(_) => return Err(invalid("`topic` must be general or news")),
    };
    let freshness = match arguments.get("freshness").and_then(Value::as_str) {
        None => None,
        Some("day") => Some(Freshness::Day),
        Some("week") => Some(Freshness::Week),
        Some("month") => Some(Freshness::Month),
        Some("year") => Some(Freshness::Year),
        Some(_) => return Err(invalid("`freshness` must be day, week, month or year")),
    };
    let country = optional_code(arguments, "country", u8::is_ascii_alphabetic)?
        .map(|code| code.to_ascii_uppercase());
    let language = optional_code(arguments, "language", u8::is_ascii_lowercase)?;
    let safe_search = match arguments.get("safe_search") {
        None | Some(Value::Null) => None,
        Some(Value::Bool(value)) => Some(*value),
        Some(_) => return Err(invalid("`safe_search` must be a boolean")),
    };
    let provider = match arguments.get("provider") {
        None | Some(Value::Null) => None,
        Some(Value::String(value)) => Some(value.clone()),
        Some(_) => return Err(invalid("`provider` must be a string")),
    };
    Ok(Parsed {
        request: SearchRequest {
            query: query.to_owned(),
            max_results,
            topic,
            freshness,
            include_domains: domain_list(arguments, "include_domains")?,
            exclude_domains: domain_list(arguments, "exclude_domains")?,
            country,
            language,
            safe_search,
        },
        explicit_max_results,
        provider,
    })
}

fn optional_code(
    arguments: &Map<String, Value>,
    key: &str,
    valid: fn(&u8) -> bool,
) -> Result<Option<String>, CallEnd> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(code)) if code.len() == 2 && code.as_bytes().iter().all(valid) => {
            Ok(Some(code.clone()))
        }
        Some(_) => Err(invalid(&format!("`{key}` must be a two-letter code"))),
    }
}

fn domain_list(arguments: &Map<String, Value>, key: &str) -> Result<Vec<String>, CallEnd> {
    let Some(value) = arguments.get(key).filter(|value| !value.is_null()) else {
        return Ok(Vec::new());
    };
    let values = value
        .as_array()
        .filter(|values| values.len() <= MAX_DOMAINS)
        .ok_or_else(|| invalid(&format!("`{key}` must list at most 10 domains")))?;
    values
        .iter()
        .map(|value| {
            value
                .as_str()
                .map(|domain| domain.trim().to_ascii_lowercase())
                .filter(|domain| {
                    !domain.is_empty()
                        && domain.len() <= 253
                        && domain
                            .bytes()
                            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-'))
                })
                .ok_or_else(|| invalid(&format!("`{key}` holds an invalid domain")))
        })
        .collect()
}

/// The provider a call uses: the per-call override (must be enabled), else
/// the toolkit default, else the platform default. `ddg_instant` is never
/// implicit: the platform default cannot name it, and nothing else picks it.
pub(crate) fn resolve_provider(
    state: &WebState,
    requested: Option<&str>,
) -> Result<&'static str, CallEnd> {
    let serves = |id: &str| {
        state
            .providers
            .get(id)
            .is_some_and(|handle| handle.web_search.is_some())
    };
    if let Some(requested) = requested {
        return registry::entry(requested)
            .map(|entry| entry.id)
            .filter(|id| serves(id))
            .ok_or_else(|| {
                CallEnd::error(
                    "provider_not_enabled",
                    "that search provider is not enabled for this project",
                )
            });
    }
    let toolkit = state.config.default_search_provider.filter(|id| serves(id));
    let platform = state
        .config
        .platform_default_search_provider
        .filter(|id| serves(id))
        .filter(|id| registry::entry(id).is_some_and(|entry| entry.lifecycle == Lifecycle::Active));
    toolkit.or(platform).ok_or_else(|| {
        CallEnd::error(
            "no_provider_configured",
            "no search provider is configured for this project; name one with `provider` or ask an administrator",
        )
    })
}

pub(crate) async fn run(
    state: &WebState,
    scope: &CallScope<'_>,
    arguments: &Map<String, Value>,
) -> Result<Value, CallEnd> {
    let parsed = parse(arguments)?;
    let provider_id = resolve_provider(state, parsed.provider.as_deref())?;
    let provider = state
        .providers
        .get(provider_id)
        .and_then(|handle| handle.web_search.as_deref())
        .ok_or_else(|| {
            CallEnd::error(
                "provider_not_enabled",
                "that search provider is not enabled",
            )
        })?;
    let options = *provider.options();
    let mut request = parsed.request;
    let mut check = OptionCheck::new(state.config.strict_options);
    apply_options(
        &mut check,
        &mut request,
        &options,
        parsed.explicit_max_results,
        provider_id,
    )?;
    request.max_results = request
        .max_results
        .min(state.config.max_results_cap)
        .min(options.max_results.max(1));
    let op = MeteredOp {
        provider: provider_id,
        operation: provider.operation(&request),
        estimate: provider.estimate(&request),
    };
    let audit = AuditTarget {
        tool: NAME,
        target: query_hash(&request.query),
    };
    let (response, usage) = metered(
        state.meter(),
        op,
        scope.tool_call_id,
        audit,
        provider.search(&request),
        |response: &super::SearchResponse| response.units,
    )
    .await?;
    let (results, truncated) = bounded_results(response.results);
    Ok(json!({
        "provider": provider_id,
        "query": request.query,
        "results": results,
        "ignored_options": check.ignored,
        "notice": response.notice,
        "truncated": truncated,
        "usage": usage.to_json(),
        "data_note": DATA_NOTE,
    }))
}

/// Judge each option the caller set against what the provider honours,
/// dropping the ones the check lets through as ignored.
fn apply_options(
    check: &mut OptionCheck,
    request: &mut SearchRequest,
    options: &super::SearchOptions,
    explicit_max_results: bool,
    provider_id: &str,
) -> Result<(), CallEnd> {
    if !check.check(
        "topic",
        request.topic == Some(Topic::News),
        options.news,
        provider_id,
    )? {
        request.topic = None;
    }
    if !check.check(
        "freshness",
        request.freshness.is_some(),
        options.freshness,
        provider_id,
    )? {
        request.freshness = None;
    }
    if !check.check(
        "include_domains",
        !request.include_domains.is_empty(),
        options.include_domains,
        provider_id,
    )? {
        request.include_domains.clear();
    }
    // Firecrawl takes include OR exclude, never both.
    let both = options.domains_exclusive && !request.include_domains.is_empty();
    if !check.check(
        "exclude_domains",
        !request.exclude_domains.is_empty(),
        options.exclude_domains && !both,
        provider_id,
    )? {
        request.exclude_domains.clear();
    }
    if !check.check(
        "country",
        request.country.is_some(),
        options.country,
        provider_id,
    )? {
        request.country = None;
    }
    if !check.check(
        "language",
        request.language.is_some(),
        options.language,
        provider_id,
    )? {
        request.language = None;
    }
    if !check.check(
        "safe_search",
        request.safe_search.is_some(),
        options.safe_search,
        provider_id,
    )? {
        request.safe_search = None;
    }
    check.check(
        "max_results",
        explicit_max_results,
        options.max_results_option,
        provider_id,
    )?;
    Ok(())
}

/// Results within the 64 KiB output budget: each result's `extras` capped at
/// 2 KiB, then all `extras` dropped, then results dropped from the end.
fn bounded_results(mut results: Vec<SearchResult>) -> (Vec<SearchResult>, bool) {
    let mut truncated = false;
    for result in &mut results {
        if serde_json::to_vec(&result.extras).map_or(0, |bytes| bytes.len()) > MAX_EXTRAS_BYTES {
            result.extras.clear();
            truncated = true;
        }
        let (snippet, cut_snippet) = cut(&result.snippet, 4_096);
        if cut_snippet {
            result.snippet = snippet.to_owned();
            truncated = true;
        }
    }
    let size = |results: &[SearchResult]| {
        serde_json::to_vec(results).map_or(usize::MAX, |bytes| bytes.len())
    };
    if size(&results) > MAX_OUTPUT_BYTES {
        for result in &mut results {
            result.extras.clear();
        }
        truncated = true;
    }
    while !results.is_empty() && size(&results) > MAX_OUTPUT_BYTES {
        results.pop();
        truncated = true;
    }
    (results, truncated)
}
