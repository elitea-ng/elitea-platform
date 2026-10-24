//! `DuckDuckGo` Instant Answers (`api.duckduckgo.com`): NOT a web search API.
//! It answers entity lookups and definitions (an abstract, an answer, a
//! definition, related topics) and nothing for other queries. Degraded:
//! never the default, never chosen implicitly, no options; an empty answer
//! says `instant_answers_only` so the model does not read it as "nothing on
//! the web". No key; the app is named with `t=elitea`.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::{Map, Value};

use super::{MAX_QUERY_RESPONSE_BYTES, QUERY_TIMEOUT, get, host_of, text, web_url};
use crate::toolkits::families::web::capability::{
    ProviderFailure, SearchOptions, SearchProvider, SearchRequest, SearchResponse, SearchResult,
};
use crate::toolkits::families::web::config::ProviderConfig;
use crate::toolkits::families::web::meter::Units;
use crate::toolkits::families::web::registry::ProviderHandle;
use crate::toolkits::families::web::transport::{ProviderError, Transport};

pub(crate) const ORIGINS: &[&str] = &["https://api.duckduckgo.com"];
const URL: &str = "https://api.duckduckgo.com/";
/// The notice an empty instant answer carries.
pub(crate) const NOTICE: &str = "instant_answers_only";
const MAX_RELATED: usize = 10;

const OPTIONS: SearchOptions = SearchOptions {
    news: false,
    freshness: false,
    include_domains: false,
    exclude_domains: false,
    domains_exclusive: false,
    country: false,
    language: false,
    safe_search: false,
    max_results_option: false,
    max_results: 10,
};

struct DdgInstant {
    transport: Arc<dyn Transport>,
}

pub(crate) fn construct(_config: &ProviderConfig, transport: Arc<dyn Transport>) -> ProviderHandle {
    ProviderHandle {
        web_search: Some(Box::new(DdgInstant { transport })),
        fetch: None,
        host_intel: None,
    }
}

fn result(
    title: String,
    url: String,
    snippet: String,
    source: Option<String>,
    kind: &'static str,
) -> SearchResult {
    SearchResult {
        title,
        source: source.or_else(|| host_of(&url)),
        url,
        snippet,
        published_at: None,
        score: None,
        kind,
        extras: Map::new(),
    }
}

/// `RelatedTopics`, with grouped topics (`{Name, Topics:[…]}`) flattened.
fn related(topics: &[Value], out: &mut Vec<SearchResult>) {
    for topic in topics.iter().filter_map(Value::as_object) {
        if out.len() >= MAX_RELATED {
            return;
        }
        if let Some(Value::Array(group)) = topic.get("Topics") {
            related(group, out);
            continue;
        }
        if let (Some(text_value), Some(url)) = (
            text(topic, "Text"),
            text(topic, "FirstURL").filter(|url| web_url(url)),
        ) {
            out.push(result(text_value.clone(), url, text_value, None, "related"));
        }
    }
}

#[async_trait]
impl SearchProvider for DdgInstant {
    fn options(&self) -> &SearchOptions {
        &OPTIONS
    }

    fn estimate(&self, _request: &SearchRequest) -> Units {
        Units::requests(1)
    }

    async fn search(&self, request: &SearchRequest) -> Result<SearchResponse, ProviderFailure> {
        let outgoing = get(
            URL,
            &[
                ("q", request.query.as_str()),
                ("format", "json"),
                ("no_html", "1"),
                ("skip_disambig", "1"),
                ("t", "elitea"),
            ],
            None,
            QUERY_TIMEOUT,
        )?;
        let answer = self
            .transport
            .send(outgoing, MAX_QUERY_RESPONSE_BYTES)
            .await?;
        if let Some(error) = answer.error() {
            return Err(error.into());
        }
        // DuckDuckGo labels its JSON `application/x-javascript`: read the
        // body as JSON whatever the label says.
        let value: Value =
            serde_json::from_slice(&answer.body).map_err(|_| ProviderError::InvalidResponse)?;
        let answer = value.as_object().ok_or(ProviderError::InvalidResponse)?;
        let mut results = Vec::new();
        if let (Some(abstract_text), Some(url)) = (
            text(answer, "AbstractText"),
            text(answer, "AbstractURL").filter(|url| web_url(url)),
        ) {
            results.push(result(
                text(answer, "Heading").unwrap_or_default(),
                url,
                abstract_text,
                text(answer, "AbstractSource"),
                "web",
            ));
        }
        if let Some(answer_text) = text(answer, "Answer") {
            results.push(result(
                text(answer, "AnswerType").unwrap_or_else(|| "Answer".to_owned()),
                String::new(),
                answer_text,
                Some("DuckDuckGo".to_owned()),
                "answer",
            ));
        }
        if let Some(definition) = text(answer, "Definition") {
            results.push(result(
                text(answer, "Heading").unwrap_or_else(|| "Definition".to_owned()),
                text(answer, "DefinitionURL").unwrap_or_default(),
                definition,
                text(answer, "DefinitionSource"),
                "definition",
            ));
        }
        if let Some(Value::Array(topics)) = answer.get("RelatedTopics") {
            related(topics, &mut results);
        }
        let notice = results.is_empty().then_some(NOTICE);
        Ok(SearchResponse {
            results,
            notice,
            units: Units::requests(1),
        })
    }
}
