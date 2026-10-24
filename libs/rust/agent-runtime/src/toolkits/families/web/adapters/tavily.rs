//! Tavily (`api.tavily.com`): `/search` and `/extract`, billed in credits
//! (basic search 1; one extracted URL 1).

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::{
    FETCH_TIMEOUT, MAX_FETCH_RESPONSE_BYTES, MAX_QUERY_RESPONSE_BYTES, QUERY_TIMEOUT, host_of,
    object, post_json, text, web_url,
};
use crate::toolkits::families::web::capability::{
    FetchFormat, FetchProvider, FetchRequest, FetchResult, ProviderFailure, SearchOptions,
    SearchProvider, SearchRequest, SearchResponse, SearchResult, Topic,
};
use crate::toolkits::families::web::config::ProviderConfig;
use crate::toolkits::families::web::meter::Units;
use crate::toolkits::families::web::registry::ProviderHandle;
use crate::toolkits::families::web::transport::{ProviderError, Transport};

pub(crate) const ORIGINS: &[&str] = &["https://api.tavily.com"];
const SEARCH_URL: &str = "https://api.tavily.com/search";
const EXTRACT_URL: &str = "https://api.tavily.com/extract";

const OPTIONS: SearchOptions = SearchOptions {
    news: true,
    freshness: true,
    include_domains: true,
    exclude_domains: true,
    domains_exclusive: false,
    country: false,
    language: false,
    safe_search: true,
    max_results_option: true,
    max_results: 20,
};

struct Tavily {
    key: String,
    transport: Arc<dyn Transport>,
}

pub(crate) fn construct(config: &ProviderConfig, transport: Arc<dyn Transport>) -> ProviderHandle {
    let key = config.api_key().unwrap_or_default().to_owned();
    ProviderHandle {
        web_search: Some(Box::new(Tavily {
            key: key.clone(),
            transport: Arc::clone(&transport),
        })),
        fetch: Some(Box::new(Tavily { key, transport })),
        host_intel: None,
    }
}

/// The credits an answer reports (`usage.credits`), else `fallback`.
fn reported_credits(answer: &Map<String, Value>, fallback: u64) -> u64 {
    answer
        .get("usage")
        .and_then(|usage| usage.get("credits"))
        .and_then(Value::as_u64)
        .unwrap_or(fallback)
}

#[async_trait]
impl SearchProvider for Tavily {
    fn options(&self) -> &SearchOptions {
        &OPTIONS
    }

    fn estimate(&self, _request: &SearchRequest) -> Units {
        Units::credits(1)
    }

    async fn search(&self, request: &SearchRequest) -> Result<SearchResponse, ProviderFailure> {
        let mut body = json!({
            "query": request.query,
            "max_results": request.max_results,
            "search_depth": "basic",
            "topic": if request.topic == Some(Topic::News) { "news" } else { "general" },
            "include_answer": false,
            "include_raw_content": false,
            "include_usage": true,
        });
        if let Some(freshness) = request.freshness {
            body["time_range"] = Value::String(freshness.as_str().to_owned());
        }
        if !request.include_domains.is_empty() {
            body["include_domains"] = json!(request.include_domains);
        }
        if !request.exclude_domains.is_empty() {
            body["exclude_domains"] = json!(request.exclude_domains);
        }
        if let Some(safe) = request.safe_search {
            body["safe_search"] = Value::Bool(safe);
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
        let results = answer
            .get("results")
            .and_then(Value::as_array)
            .ok_or(ProviderError::InvalidResponse)?
            .iter()
            .filter_map(Value::as_object)
            .filter_map(|result| {
                let url = text(result, "url").filter(|url| web_url(url))?;
                Some(SearchResult {
                    title: text(result, "title").unwrap_or_default(),
                    source: host_of(&url),
                    snippet: text(result, "content").unwrap_or_default(),
                    published_at: text(result, "published_date"),
                    score: result
                        .get("score")
                        .and_then(Value::as_f64)
                        .map(|score| score.clamp(0.0, 1.0)),
                    kind: if request.topic == Some(Topic::News) {
                        "news"
                    } else {
                        "web"
                    },
                    extras: Map::new(),
                    url,
                })
            })
            .take(request.max_results as usize)
            .collect();
        Ok(SearchResponse {
            results,
            notice: None,
            units: Units::credits(reported_credits(answer, 1)),
        })
    }
}

#[async_trait]
impl FetchProvider for Tavily {
    fn estimate(&self) -> Units {
        Units::credits(1)
    }

    async fn fetch(&self, request: &FetchRequest) -> Result<FetchResult, ProviderFailure> {
        let body = json!({
            "urls": [request.url.as_str()],
            "format": request.format.as_str(),
            "extract_depth": "basic",
            "include_usage": true,
        });
        let outgoing = post_json(EXTRACT_URL, &self.key, &body, FETCH_TIMEOUT)?;
        let answer = self
            .transport
            .send(outgoing, MAX_FETCH_RESPONSE_BYTES)
            .await?;
        if let Some(error) = answer.error() {
            return Err(error.into());
        }
        let value = answer.json()?;
        let answer = object(&value)?;
        // Tavily answers 200 with the URL in `failed_results`: still read.
        let first = answer
            .get("results")
            .and_then(Value::as_array)
            .and_then(|results| results.first())
            .and_then(Value::as_object);
        let Some(first) = first else {
            let failed = answer
                .get("failed_results")
                .and_then(Value::as_array)
                .is_some_and(|failed| !failed.is_empty());
            return Err(if failed {
                ProviderError::NotFound
            } else {
                ProviderError::InvalidResponse
            }
            .into());
        };
        let content = first
            .get("raw_content")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        Ok(FetchResult {
            url: request.url.to_string(),
            final_url: text(first, "url").unwrap_or_else(|| request.url.to_string()),
            status: 200,
            content_type: None,
            title: None,
            bytes: content.len(),
            content,
            format: match request.format {
                FetchFormat::Markdown => "markdown",
                FetchFormat::Text => "text",
            },
            truncated: false,
            units: Units::credits(reported_credits(answer, 1)),
        })
    }
}
