//! Brave Search API (`api.search.brave.com`): web and news search, one
//! request each. No domain filter: the model may write `site:` in the query
//! itself, and the query is never rewritten. Results are transient (Brave's
//! terms forbid storing them) and go to the model only.

use std::sync::Arc;

use async_trait::async_trait;
use reqwest::header::HeaderName;
use serde_json::{Map, Value};

use super::{MAX_QUERY_RESPONSE_BYTES, QUERY_TIMEOUT, get, host_of, object, text, web_url};
use crate::toolkits::families::web::capability::{
    ProviderFailure, SearchOptions, SearchProvider, SearchRequest, SearchResponse, SearchResult,
    Topic,
};
use crate::toolkits::families::web::config::ProviderConfig;
use crate::toolkits::families::web::meter::Units;
use crate::toolkits::families::web::registry::ProviderHandle;
use crate::toolkits::families::web::transport::{ProviderError, Transport};

pub(crate) const ORIGINS: &[&str] = &["https://api.search.brave.com"];
const WEB_URL: &str = "https://api.search.brave.com/res/v1/web/search";
const NEWS_URL: &str = "https://api.search.brave.com/res/v1/news/search";
const TOKEN: HeaderName = HeaderName::from_static("x-subscription-token");

const OPTIONS: SearchOptions = SearchOptions {
    news: true,
    freshness: true,
    include_domains: false,
    exclude_domains: false,
    domains_exclusive: false,
    country: true,
    language: true,
    safe_search: true,
    max_results_option: true,
    max_results: 20,
};

struct Brave {
    key: String,
    transport: Arc<dyn Transport>,
}

pub(crate) fn construct(config: &ProviderConfig, transport: Arc<dyn Transport>) -> ProviderHandle {
    ProviderHandle {
        web_search: Some(Box::new(Brave {
            key: config.api_key().unwrap_or_default().to_owned(),
            transport,
        })),
        fetch: None,
        host_intel: None,
    }
}

#[async_trait]
impl SearchProvider for Brave {
    fn options(&self) -> &SearchOptions {
        &OPTIONS
    }

    fn estimate(&self, _request: &SearchRequest) -> Units {
        Units::requests(1)
    }

    async fn search(&self, request: &SearchRequest) -> Result<SearchResponse, ProviderFailure> {
        let news = request.topic == Some(Topic::News);
        let count = request.max_results.to_string();
        let mut query: Vec<(&str, &str)> = vec![("q", &request.query), ("count", &count)];
        let freshness = request.freshness.map(|freshness| match freshness.as_str() {
            "day" => "pd",
            "week" => "pw",
            "month" => "pm",
            _ => "py",
        });
        if let Some(freshness) = freshness {
            query.push(("freshness", freshness));
        }
        let country = request.country.as_deref().map(str::to_ascii_lowercase);
        if let Some(country) = country.as_deref() {
            query.push(("country", country));
        }
        if let Some(language) = request.language.as_deref() {
            query.push(("search_lang", language));
        }
        if let Some(safe) = request.safe_search {
            query.push(("safesearch", if safe { "strict" } else { "off" }));
        }
        let outgoing = get(
            if news { NEWS_URL } else { WEB_URL },
            &query,
            Some((TOKEN, &self.key)),
            QUERY_TIMEOUT,
        )?;
        let answer = self
            .transport
            .send(outgoing, MAX_QUERY_RESPONSE_BYTES)
            .await?;
        if let Some(error) = answer.error() {
            return Err(error.into());
        }
        let value = answer.json()?;
        let answer = object(&value)?;
        // Web results nest under `web`; news results are top level.
        let list = if news {
            answer.get("results")
        } else {
            answer.get("web").and_then(|web| web.get("results"))
        };
        let list = match list {
            Some(Value::Array(list)) => list.as_slice(),
            None => &[],
            Some(_) => return Err(ProviderError::InvalidResponse.into()),
        };
        let results = list
            .iter()
            .filter_map(Value::as_object)
            .filter_map(|result| {
                let url = text(result, "url").filter(|url| web_url(url))?;
                let mut extras = Map::new();
                if let Some(snippets) = result
                    .get("extra_snippets")
                    .filter(|value| value.is_array())
                {
                    extras.insert("extra_snippets".to_owned(), snippets.clone());
                }
                Some(SearchResult {
                    title: text(result, "title").unwrap_or_default(),
                    source: host_of(&url),
                    snippet: text(result, "description").unwrap_or_default(),
                    published_at: text(result, "page_age").or_else(|| text(result, "age")),
                    score: None,
                    kind: if news { "news" } else { "web" },
                    extras,
                    url,
                })
            })
            .take(request.max_results as usize)
            .collect();
        Ok(SearchResponse {
            results,
            notice: None,
            units: Units::requests(1),
        })
    }
}
