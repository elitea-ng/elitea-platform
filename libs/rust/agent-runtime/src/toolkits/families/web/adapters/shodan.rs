//! Shodan (`api.shodan.io`), the `host_intel` capability: host search,
//! host count and host lookup. Read-only by construction: this adapter has
//! no path to `/shodan/scan` (which spends scan credits and probes hosts)
//! or `/dns/domain` (subdomain enumeration). Bring-your-own-key per project
//! only (the terms bar sharing one account); results carry Shodan's
//! attribution (terms of service 6.7). Raw banner text is never returned.
//!
//! The key rides in the query string (Shodan has no header auth), so no
//! request URL is ever logged or kept in an error. Shodan allows one request
//! per second per key: a limiter per key spaces this worker's calls.

use std::collections::HashMap;
use std::net::IpAddr;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Map, Value};
use tokio::time::Instant;

use super::{MAX_QUERY_RESPONSE_BYTES, QUERY_TIMEOUT, get, object, text};
use crate::toolkits::families::web::capability::{
    HostIntelOptions, HostIntelProvider, HostLocation, HostQuery, HostRecord, HostSearchResponse,
    HostService, ProviderFailure,
};
use crate::toolkits::families::web::config::ProviderConfig;
use crate::toolkits::families::web::meter::Units;
use crate::toolkits::families::web::registry::ProviderHandle;
use crate::toolkits::families::web::transport::{ProviderError, Transport};

pub(crate) const ORIGINS: &[&str] = &["https://api.shodan.io"];
const BASE: &str = "https://api.shodan.io";
pub(crate) const ATTRIBUTION: &str = "Data from Shodan (shodan.io)";
const MIN_SPACING: Duration = Duration::from_secs(1);
const MAX_LIST: usize = 64;

const OPTIONS: HostIntelOptions = HostIntelOptions {
    facets: true,
    count_only: true,
    max_results: 100,
};

type Limiter = Arc<tokio::sync::Mutex<Option<Instant>>>;

/// One limiter per key (by its SHA-256), shared by every toolset in the
/// worker.
static LIMITERS: LazyLock<Mutex<HashMap<[u8; 32], Limiter>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

fn limiter(key: &str) -> Limiter {
    let mut digest = [0_u8; 32];
    digest.copy_from_slice(ring::digest::digest(&ring::digest::SHA256, key.as_bytes()).as_ref());
    let mut limiters = LIMITERS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    Arc::clone(limiters.entry(digest).or_default())
}

struct Shodan {
    key: String,
    transport: Arc<dyn Transport>,
    limiter: Limiter,
}

pub(crate) fn construct(config: &ProviderConfig, transport: Arc<dyn Transport>) -> ProviderHandle {
    let key = config.api_key().unwrap_or_default().to_owned();
    ProviderHandle {
        web_search: None,
        fetch: None,
        host_intel: Some(Box::new(Shodan {
            limiter: limiter(&key),
            key,
            transport,
        })),
    }
}

/// A query uses a filter when it has a `name:value` term: Shodan charges a
/// query credit for those (an unfiltered first page is free).
fn uses_filter(query: &str) -> bool {
    query.split_whitespace().any(|term| {
        term.trim_start_matches('-')
            .split_once(':')
            .is_some_and(|(name, _)| !name.is_empty())
    })
}

impl Shodan {
    async fn call(&self, path: &str, query: &[(&str, &str)]) -> Result<Value, ProviderError> {
        {
            let mut last = self.limiter.lock().await;
            if let Some(at) = *last {
                tokio::time::sleep_until(at + MIN_SPACING).await;
            }
            *last = Some(Instant::now());
        }
        let mut pairs = vec![("key", self.key.as_str())];
        pairs.extend_from_slice(query);
        let outgoing = get(&format!("{BASE}{path}"), &pairs, None, QUERY_TIMEOUT)?;
        let answer = self
            .transport
            .send(outgoing, MAX_QUERY_RESPONSE_BYTES)
            .await?;
        if let Some(error) = answer.error() {
            return Err(error);
        }
        answer.json()
    }
}

fn strings(value: Option<&Value>) -> Vec<String> {
    match value {
        Some(Value::Array(items)) => items
            .iter()
            .filter_map(Value::as_str)
            .take(MAX_LIST)
            .map(str::to_owned)
            .collect(),
        // Banner `vulns` are an object keyed by CVE id.
        Some(Value::Object(map)) => map.keys().take(MAX_LIST).cloned().collect(),
        Some(Value::String(one)) => vec![one.clone()],
        _ => Vec::new(),
    }
}

/// One banner as a service; the banner text itself (`data`) is dropped.
fn service(banner: &Map<String, Value>) -> Option<HostService> {
    let port = banner
        .get("port")
        .and_then(Value::as_u64)
        .and_then(|port| u16::try_from(port).ok())?;
    let tls_subject_cn = banner
        .get("ssl")
        .and_then(|ssl| ssl.get("cert"))
        .and_then(|cert| cert.get("subject"))
        .and_then(|subject| subject.get("CN"))
        .and_then(Value::as_str)
        .map(str::to_owned);
    Some(HostService {
        port,
        transport: text(banner, "transport"),
        product: text(banner, "product"),
        version: text(banner, "version"),
        cpe: strings(banner.get("cpe23").or_else(|| banner.get("cpe"))),
        tls_subject_cn,
        vulns: strings(banner.get("vulns")),
    })
}

/// A host from a lookup answer, or from the banners of one IP in a search.
fn record(top: &Map<String, Value>, banners: &[&Map<String, Value>]) -> HostRecord {
    let location = top.get("location").and_then(Value::as_object).or_else(|| {
        banners
            .first()
            .and_then(|banner| banner.get("location"))
            .and_then(Value::as_object)
    });
    let services: Vec<HostService> = banners
        .iter()
        .filter_map(|banner| service(banner))
        .collect();
    let mut ports: Vec<u16> = match top.get("ports") {
        Some(Value::Array(ports)) => ports
            .iter()
            .filter_map(Value::as_u64)
            .filter_map(|port| u16::try_from(port).ok())
            .collect(),
        _ => services.iter().map(|service| service.port).collect(),
    };
    ports.sort_unstable();
    ports.dedup();
    let mut vulns = strings(top.get("vulns"));
    for service in &services {
        vulns.extend(service.vulns.iter().cloned());
    }
    vulns.sort_unstable();
    vulns.dedup();
    vulns.truncate(MAX_LIST);
    let first =
        |key: &str| text(top, key).or_else(|| banners.iter().find_map(|banner| text(banner, key)));
    HostRecord {
        ip: first("ip_str").unwrap_or_default(),
        hostnames: {
            let mut names = strings(top.get("hostnames"));
            if names.is_empty() {
                names = banners
                    .iter()
                    .flat_map(|banner| strings(banner.get("hostnames")))
                    .collect();
                names.sort_unstable();
                names.dedup();
            }
            names
        },
        org: first("org"),
        isp: first("isp"),
        asn: first("asn"),
        location: HostLocation {
            country: location.and_then(|location| text(location, "country_code")),
            city: location.and_then(|location| text(location, "city")),
        },
        os: first("os"),
        services,
        ports,
        vulns,
        last_seen: first("last_update").or_else(|| first("timestamp")),
        attribution: ATTRIBUTION,
    }
}

fn facets(answer: &Map<String, Value>) -> Map<String, Value> {
    answer
        .get("facets")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default()
}

#[async_trait]
impl HostIntelProvider for Shodan {
    fn options(&self) -> &HostIntelOptions {
        &OPTIONS
    }

    fn estimate_search(&self, query: &HostQuery) -> Units {
        if query.count_only {
            return Units::credits(0);
        }
        Units::credits(u64::from(uses_filter(&query.query)))
    }

    /// Shodan does not state a host lookup's credit cost; it is metered as
    /// one query credit until `/api-info` measurements say otherwise.
    fn estimate_lookup(&self) -> Units {
        Units::credits(1)
    }

    async fn search(&self, query: &HostQuery) -> Result<HostSearchResponse, ProviderFailure> {
        let facet_list = query.facets.join(",");
        let mut pairs = vec![("query", query.query.as_str())];
        if !facet_list.is_empty() {
            pairs.push(("facets", facet_list.as_str()));
        }
        if query.count_only {
            let value = self.call("/shodan/host/count", &pairs).await?;
            let answer = object(&value)?;
            return Ok(HostSearchResponse {
                total: answer
                    .get("total")
                    .and_then(Value::as_u64)
                    .unwrap_or_default(),
                results: Vec::new(),
                facets: facets(answer),
                units: Units::credits(0),
            });
        }
        pairs.push(("page", "1"));
        let value = self.call("/shodan/host/search", &pairs).await?;
        let answer = object(&value)?;
        let matches: Vec<&Map<String, Value>> = answer
            .get("matches")
            .and_then(Value::as_array)
            .ok_or(ProviderError::InvalidResponse)?
            .iter()
            .filter_map(Value::as_object)
            .collect();
        // Matches are banners, one per service: group them by host.
        let mut order: Vec<String> = Vec::new();
        let mut by_ip: HashMap<String, Vec<&Map<String, Value>>> = HashMap::new();
        for banner in matches {
            let Some(ip) = text(banner, "ip_str") else {
                continue;
            };
            if !by_ip.contains_key(&ip) {
                if order.len() as u64 >= u64::from(query.max_results) {
                    continue;
                }
                order.push(ip.clone());
            }
            by_ip.entry(ip).or_default().push(banner);
        }
        let empty = Map::new();
        let results = order
            .iter()
            .map(|ip| record(&empty, by_ip.get(ip).map(Vec::as_slice).unwrap_or_default()))
            .collect();
        Ok(HostSearchResponse {
            total: answer
                .get("total")
                .and_then(Value::as_u64)
                .unwrap_or_default(),
            results,
            facets: facets(answer),
            units: Units::credits(u64::from(uses_filter(&query.query))),
        })
    }

    async fn lookup(&self, ip: IpAddr) -> Result<HostRecord, ProviderFailure> {
        let value = self.call(&format!("/shodan/host/{ip}"), &[]).await?;
        let answer = object(&value)?;
        let banners: Vec<&Map<String, Value>> = answer
            .get("data")
            .and_then(Value::as_array)
            .map(|banners| banners.iter().filter_map(Value::as_object).collect())
            .unwrap_or_default();
        Ok(record(answer, &banners))
    }
}
