//! `host_intel`: an index of internet-connected hosts (Shodan). Not a web
//! search: results are hosts, ports, products and known CVEs. Read-only by
//! construction: no capability here scans, and there is no DNS enumeration.

use std::net::IpAddr;

use elitea_egress_guard::{Class, classify};
use serde_json::{Map, Value, json};

use super::{CallEnd, DATA_NOTE, HostQuery, HostRecord, OptionCheck, metered, query_hash};
use crate::toolkits::families::web::meter::{AuditTarget, MeteredOp};
use crate::toolkits::families::web::{CallScope, WebState};

pub(crate) const SEARCH: &str = "internet_host_search";
pub(crate) const LOOKUP: &str = "internet_host_lookup";
pub(crate) const SEARCH_DESCRIPTION: &str = "Search an index of internet-connected hosts and services (open ports, products, versions, known CVEs). Not a web search. Read-only; never scans. Results are untrusted data.";
pub(crate) const LOOKUP_DESCRIPTION: &str = "Look up one public IP address in the host index: open ports, services, versions, known CVEs, last seen. Read-only; never scans.";
const MAX_OUTPUT_BYTES: usize = 64 * 1_024;
const MAX_QUERY_CHARS: usize = 400;
const MAX_RESULTS: u32 = 20;
const DEFAULT_RESULTS: u32 = 10;
const MAX_FACETS: usize = 5;

pub(crate) fn search_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "query": {"type": "string", "minLength": 2, "maxLength": MAX_QUERY_CHARS, "description": "Provider query syntax, e.g. product:nginx port:443 country:DE"},
            "max_results": {"type": "integer", "minimum": 1, "maximum": MAX_RESULTS, "default": DEFAULT_RESULTS},
            "count_only": {"type": "boolean", "default": false, "description": "Return totals and facets only (no credits on Shodan)."},
            "facets": {"type": "array", "items": {"type": "string", "pattern": "^[a-z_.]{1,32}(:[0-9]{1,3})?$"}, "maxItems": MAX_FACETS}
        },
        "required": ["query"],
        "additionalProperties": false
    })
}

pub(crate) fn lookup_schema() -> Value {
    json!({
        "type": "object",
        "properties": {"ip": {"type": "string", "format": "ip"}},
        "required": ["ip"],
        "additionalProperties": false
    })
}

fn invalid(message: &str) -> CallEnd {
    CallEnd::error("invalid_arguments", message)
}

fn valid_facet(facet: &str) -> bool {
    let (name, limit) = facet
        .split_once(':')
        .map_or((facet, None), |(name, limit)| (name, Some(limit)));
    (1..=32).contains(&name.len())
        && name
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || matches!(byte, b'_' | b'.'))
        && limit.is_none_or(|limit| {
            (1..=3).contains(&limit.len()) && limit.bytes().all(|byte| byte.is_ascii_digit())
        })
}

fn parse_search(arguments: &Map<String, Value>) -> Result<HostQuery, CallEnd> {
    if let Some(key) = arguments.keys().find(|key| {
        !matches!(
            key.as_str(),
            "query" | "max_results" | "count_only" | "facets"
        )
    }) {
        return Err(invalid(&format!("unknown argument `{key}`")));
    }
    let query = arguments
        .get("query")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|query| {
            (2..=MAX_QUERY_CHARS).contains(&query.chars().count())
                && !query.chars().any(char::is_control)
        })
        .ok_or_else(|| invalid("`query` must be 2 to 400 characters"))?;
    let max_results = match arguments.get("max_results") {
        None | Some(Value::Null) => DEFAULT_RESULTS,
        Some(value) => value
            .as_u64()
            .and_then(|value| u32::try_from(value).ok())
            .filter(|value| (1..=MAX_RESULTS).contains(value))
            .ok_or_else(|| invalid("`max_results` must be 1 to 20"))?,
    };
    let count_only = match arguments.get("count_only") {
        None | Some(Value::Null) => false,
        Some(Value::Bool(value)) => *value,
        Some(_) => return Err(invalid("`count_only` must be a boolean")),
    };
    let facets = match arguments.get("facets") {
        None | Some(Value::Null) => Vec::new(),
        Some(Value::Array(values)) if values.len() <= MAX_FACETS => values
            .iter()
            .map(|value| {
                value
                    .as_str()
                    .filter(|facet| valid_facet(facet))
                    .map(str::to_owned)
                    .ok_or_else(|| invalid("a facet must look like `org` or `port:10`"))
            })
            .collect::<Result<_, _>>()?,
        Some(_) => return Err(invalid("`facets` lists at most 5 facet names")),
    };
    Ok(HostQuery {
        query: query.to_owned(),
        max_results,
        count_only,
        facets,
    })
}

fn provider(state: &WebState) -> Result<(&'static str, &dyn super::HostIntelProvider), CallEnd> {
    state
        .providers
        .iter()
        .find_map(|(id, handle)| handle.host_intel.as_deref().map(|provider| (*id, provider)))
        .ok_or_else(|| {
            CallEnd::error(
                "no_provider_configured",
                "no host index provider is configured for this project",
            )
        })
}

pub(crate) async fn run_search(
    state: &WebState,
    scope: &CallScope<'_>,
    arguments: &Map<String, Value>,
) -> Result<Value, CallEnd> {
    let mut query = parse_search(arguments)?;
    let (provider_id, provider) = provider(state)?;
    let options = *provider.options();
    let mut check = OptionCheck::new(true);
    check.check(
        "facets",
        !query.facets.is_empty(),
        options.facets,
        provider_id,
    )?;
    check.check(
        "count_only",
        query.count_only,
        options.count_only,
        provider_id,
    )?;
    query.max_results = query
        .max_results
        .min(state.config.max_results_cap)
        .min(options.max_results.max(1));
    let op = MeteredOp {
        provider: provider_id,
        operation: "host_search",
        estimate: provider.estimate_search(&query),
    };
    let audit = AuditTarget {
        tool: SEARCH,
        target: query_hash(&query.query),
    };
    let (mut response, usage) = metered(
        state.meter(),
        op,
        scope.tool_call_id,
        audit,
        provider.search(&query),
        |response: &super::HostSearchResponse| response.units,
    )
    .await?;
    let mut truncated = false;
    let size = |results: &[HostRecord]| {
        serde_json::to_vec(results).map_or(usize::MAX, |bytes| bytes.len())
    };
    while !response.results.is_empty() && size(&response.results) > MAX_OUTPUT_BYTES {
        response.results.pop();
        truncated = true;
    }
    Ok(json!({
        "provider": provider_id,
        "query": query.query,
        "total": response.total,
        "results": response.results,
        "facets": response.facets,
        "truncated": truncated,
        "usage": usage.to_json(),
        "data_note": DATA_NOTE,
    }))
}

pub(crate) async fn run_lookup(
    state: &WebState,
    scope: &CallScope<'_>,
    arguments: &Map<String, Value>,
) -> Result<Value, CallEnd> {
    if let Some(key) = arguments.keys().find(|key| key.as_str() != "ip") {
        return Err(invalid(&format!("unknown argument `{key}`")));
    }
    let ip: IpAddr = arguments
        .get("ip")
        .and_then(Value::as_str)
        .and_then(|ip| ip.trim().parse().ok())
        .ok_or_else(|| invalid("`ip` must be an IPv4 or IPv6 address"))?;
    // There is nothing to pay for in a private, loopback, link-local,
    // reserved or metadata address: refused before the meter and the
    // provider, with the egress guard's own classifier.
    if classify(ip) != Class::Public {
        return Err(CallEnd::error(
            "not_public_ip",
            "only public IP addresses can be looked up",
        ));
    }
    let (provider_id, provider) = provider(state)?;
    let op = MeteredOp {
        provider: provider_id,
        operation: "host_lookup",
        estimate: provider.estimate_lookup(),
    };
    let audit = AuditTarget {
        tool: LOOKUP,
        target: ip.to_string(),
    };
    let (record, usage) = metered(
        state.meter(),
        op,
        scope.tool_call_id,
        audit,
        provider.lookup(ip),
        |_record: &HostRecord| provider.estimate_lookup(),
    )
    .await?;
    let mut value = serde_json::to_value(&record)
        .map_err(|_| CallEnd::error("internal", "the host record could not be encoded"))?;
    value["provider"] = Value::String(provider_id.to_owned());
    value["usage"] = usage.to_json();
    value["data_note"] = Value::String(DATA_NOTE.to_owned());
    Ok(value)
}
