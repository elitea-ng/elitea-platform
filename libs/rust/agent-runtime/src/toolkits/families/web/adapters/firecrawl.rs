//! Firecrawl (`api.firecrawl.dev`, API v2): `/v2/search` (2 credits per 10
//! results) and `/v2/scrape` (1 page, billed even for a 403/404 page).
//! Every scrape sends `storeInCache:false` and `maxAge:0`: no vendor-side
//! cache of a tenant's URLs.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::{
    MAX_FETCH_RESPONSE_BYTES, MAX_QUERY_RESPONSE_BYTES, QUERY_TIMEOUT, host_of, object, post_json,
    text, web_url,
};
use crate::toolkits::families::web::capability::{
    FetchProvider, FetchRequest, FetchResult, ProviderFailure, SearchOptions, SearchProvider,
    SearchRequest, SearchResponse, SearchResult, Topic,
};
use crate::toolkits::families::web::config::ProviderConfig;
use crate::toolkits::families::web::meter::Units;
use crate::toolkits::families::web::registry::ProviderHandle;
use crate::toolkits::families::web::transport::{ProviderError, Transport};

pub(crate) const ORIGINS: &[&str] = &["https://api.firecrawl.dev"];
const SEARCH_URL: &str = "https://api.firecrawl.dev/v2/search";
const SCRAPE_URL: &str = "https://api.firecrawl.dev/v2/scrape";
/// The scrape timeout Firecrawl is told; we stop waiting a little later.
const SCRAPE_TIMEOUT_MS: u64 = 20_000;
const SCRAPE_WAIT: Duration = Duration::from_secs(25);

const OPTIONS: SearchOptions = SearchOptions {
    news: true,
    freshness: true,
    include_domains: true,
    exclude_domains: true,
    domains_exclusive: true,
    country: true,
    language: false,
    safe_search: false,
    max_results_option: true,
    max_results: 20,
};

struct Firecrawl {
    key: String,
    transport: Arc<dyn Transport>,
}

pub(crate) fn construct(config: &ProviderConfig, transport: Arc<dyn Transport>) -> ProviderHandle {
    let key = config.api_key().unwrap_or_default().to_owned();
    ProviderHandle {
        web_search: Some(Box::new(Firecrawl {
            key: key.clone(),
            transport: Arc::clone(&transport),
        })),
        fetch: Some(Box::new(Firecrawl { key, transport })),
        host_intel: None,
    }
}

/// Two credits per started block of ten results.
fn search_credits(limit: u32) -> u64 {
    u64::from(limit.div_ceil(10)) * 2
}

#[async_trait]
impl SearchProvider for Firecrawl {
    fn options(&self) -> &SearchOptions {
        &OPTIONS
    }

    fn estimate(&self, request: &SearchRequest) -> Units {
        Units::credits(search_credits(request.max_results))
    }

    async fn search(&self, request: &SearchRequest) -> Result<SearchResponse, ProviderFailure> {
        let news = request.topic == Some(Topic::News);
        let mut body = json!({
            "query": request.query,
            "limit": request.max_results,
            "sources": [if news { "news" } else { "web" }],
        });
        if let Some(freshness) = request.freshness {
            let code = match freshness.as_str() {
                "day" => "qdr:d",
                "week" => "qdr:w",
                "month" => "qdr:m",
                _ => "qdr:y",
            };
            body["tbs"] = Value::String(code.to_owned());
        }
        if let Some(country) = &request.country {
            body["country"] = Value::String(country.clone());
        }
        if !request.include_domains.is_empty() {
            body["includeDomains"] = json!(request.include_domains);
        } else if !request.exclude_domains.is_empty() {
            body["excludeDomains"] = json!(request.exclude_domains);
        }
        let outgoing = post_json(SEARCH_URL, &self.key, &body, QUERY_TIMEOUT)?;
        let answer = self
            .transport
            .send(outgoing, MAX_QUERY_RESPONSE_BYTES)
            .await?;
        if let Some(error) = answer.error() {
            return Err(error.into());
        }
        let value = answer.json()?;
        let answer = object(&value)?;
        let data = answer
            .get("data")
            .and_then(Value::as_object)
            .ok_or(ProviderError::InvalidResponse)?;
        let list = data
            .get(if news { "news" } else { "web" })
            .and_then(Value::as_array)
            .map(Vec::as_slice)
            .unwrap_or_default();
        let results = list
            .iter()
            .filter_map(Value::as_object)
            .filter_map(|result| {
                let url = text(result, "url").filter(|url| web_url(url))?;
                Some(SearchResult {
                    title: text(result, "title").unwrap_or_default(),
                    source: host_of(&url),
                    snippet: text(result, "description")
                        .or_else(|| text(result, "snippet"))
                        .unwrap_or_default(),
                    published_at: text(result, "date"),
                    score: None,
                    kind: if news { "news" } else { "web" },
                    extras: Map::new(),
                    url,
                })
            })
            .take(request.max_results as usize)
            .collect();
        let credits = answer
            .get("creditsUsed")
            .and_then(Value::as_u64)
            .unwrap_or_else(|| search_credits(request.max_results));
        Ok(SearchResponse {
            results,
            notice: None,
            units: Units::credits(credits),
        })
    }
}

#[async_trait]
impl FetchProvider for Firecrawl {
    fn estimate(&self) -> Units {
        Units::pages(1)
    }

    async fn fetch(&self, request: &FetchRequest) -> Result<FetchResult, ProviderFailure> {
        let body = json!({
            "url": request.url.as_str(),
            "formats": ["markdown"],
            "onlyMainContent": true,
            "timeout": SCRAPE_TIMEOUT_MS,
            "storeInCache": false,
            "maxAge": 0,
        });
        let outgoing = post_json(SCRAPE_URL, &self.key, &body, SCRAPE_WAIT)?;
        let answer = self
            .transport
            .send(outgoing, MAX_FETCH_RESPONSE_BYTES)
            .await?;
        if let Some(error) = answer.error() {
            // A page Firecrawl reached but that answered 403/404 still costs
            // its credit; a refusal of the call itself does not.
            let units = if error == ProviderError::NotFound {
                Units::pages(1)
            } else {
                Units::default()
            };
            return Err(ProviderFailure { error, units });
        }
        let value = answer.json()?;
        let data = object(&value)?
            .get("data")
            .and_then(Value::as_object)
            .ok_or(ProviderError::InvalidResponse)?;
        let metadata = data.get("metadata").and_then(Value::as_object);
        let status = metadata
            .and_then(|metadata| metadata.get("statusCode"))
            .and_then(Value::as_u64)
            .and_then(|status| u16::try_from(status).ok())
            .unwrap_or(200);
        let content = data
            .get("markdown")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        Ok(FetchResult {
            url: request.url.to_string(),
            final_url: metadata
                .and_then(|metadata| text(metadata, "url").or_else(|| text(metadata, "sourceURL")))
                .unwrap_or_else(|| request.url.to_string()),
            status,
            content_type: metadata.and_then(|metadata| text(metadata, "contentType")),
            title: metadata.and_then(|metadata| text(metadata, "title")),
            bytes: content.len(),
            content,
            format: "markdown",
            truncated: false,
            units: Units::pages(1),
        })
    }
}
