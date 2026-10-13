//! The Zephyr products as `ContentSource`s (ADR-0030 decision 3, stage I3):
//! Zephyr Scale (Cloud), Zephyr Essential and Zephyr Enterprise. The three
//! share a bearer-token REST client; each has its own configuration key,
//! entities and version rule.
//!
//! The SDK's two other Zephyr toolkits, `zephyr` and `zephyr_squad`, have
//! no indexer and are not served.

pub mod enterprise;
pub mod essential;
pub mod scale;

use serde_json::Value;
use url::Url;

use crate::atlassian::authorized_json;
use crate::source::{Failure, SourceHttp, web_url};
use crate::transport::header::{ACCEPT, AUTHORIZATION, HeaderValue};
use crate::transport::{Method, Request, StatusCode};

/// A paged envelope's page size (`maxResults`; Zephyr caps it at 1000).
pub(crate) const PAGE_SIZE: usize = 200;

/// The bearer-token REST client the three Zephyr sources send through: one
/// origin, one credential, every call through [`SourceHttp`].
pub(crate) struct ZephyrClient {
    provider: &'static str,
    base: Url,
    authorization: HeaderValue,
    pub(crate) http: SourceHttp,
}

impl ZephyrClient {
    pub(crate) fn new(
        provider: &'static str,
        base: Url,
        authorization: HeaderValue,
        http: SourceHttp,
    ) -> Self {
        Self {
            provider,
            base,
            authorization,
            http,
        }
    }

    /// `{base}/{segments...}?{params}`, each segment percent-encoded.
    pub(crate) fn url(&self, segments: &[&str], params: &[(&str, String)]) -> Result<Url, Failure> {
        let mut url = web_url(&self.base, segments.iter().copied()).ok_or(Failure::Client(
            "the base URL cannot carry a path".to_owned(),
        ))?;
        if !params.is_empty() {
            let mut query = url.query_pairs_mut();
            for (name, value) in params {
                query.append_pair(name, value);
            }
        }
        Ok(url)
    }

    pub(crate) fn get(&self, url: Url) -> Request {
        let mut request = Request::new(Method::GET, url);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(AUTHORIZATION, self.authorization.clone());
        request
    }

    pub(crate) async fn json(
        &self,
        segments: &[&str],
        params: &[(&str, String)],
    ) -> Result<Value, Failure> {
        let request = self.get(self.url(segments, params)?);
        authorized_json(&self.http, request, self.provider).await
    }

    /// Like [`Self::json`], with a 404 read as "there is none".
    pub(crate) async fn json_or_absent(
        &self,
        segments: &[&str],
        params: &[(&str, String)],
    ) -> Result<Option<Value>, Failure> {
        match self.json(segments, params).await {
            Ok(value) => Ok(Some(value)),
            Err(Failure::Status(StatusCode::NOT_FOUND)) => Ok(None),
            Err(other) => Err(other),
        }
    }

    /// Every item of a Zephyr Scale/Essential paged envelope
    /// (`{"startAt", "maxResults", "total", "isLast", "values": [...]}`),
    /// under this listing's own page cap.
    pub(crate) async fn values(
        &self,
        segments: &[&str],
        params: &[(&str, String)],
    ) -> Result<Vec<Value>, Failure> {
        self.values_where(segments, params, true, |_| true).await
    }

    /// Like [`Self::values`], keeping only the items `keep` accepts. The
    /// document cap counts the kept items; the rows read are bounded by the
    /// page cap, so a narrow filter over a large project is not refused for
    /// the project's size. With `capped` false the document cap is left to
    /// the caller (a later cut, such as a `limit`, decides what is kept).
    pub(crate) async fn values_where(
        &self,
        segments: &[&str],
        params: &[(&str, String)],
        capped: bool,
        keep: impl Fn(&Value) -> bool,
    ) -> Result<Vec<Value>, Failure> {
        let mut items: Vec<Value> = Vec::new();
        let mut start = 0_usize;
        let mut pages_seen = 0_usize;
        loop {
            pages_seen += 1;
            self.http.check_pages(pages_seen)?;
            let mut query = params.to_vec();
            query.push(("maxResults", PAGE_SIZE.to_string()));
            query.push(("startAt", start.to_string()));
            let mut page = self.json(segments, &query).await?;
            let batch = if let Value::Array(all) = page {
                // A bare list is the whole answer.
                items.extend(all.into_iter().filter(|item| keep(item)));
                if capped {
                    self.http.check_count(items.len())?;
                }
                return Ok(items);
            } else {
                crate::testmgmt::take_list(&mut page, "values", "a page holds no `values` list")?
            };
            let got = batch.len();
            items.extend(batch.into_iter().filter(|item| keep(item)));
            if capped {
                self.http.check_count(items.len())?;
            }
            let is_last = page.get("isLast").and_then(Value::as_bool);
            let total = page.get("total").and_then(Value::as_u64);
            start += got;
            // An empty page while the API says rows are owed (`isLast`
            // false, or `total` not reached) is a failure, never a silently
            // short listing.
            if got == 0 && (is_last == Some(false) || total.is_some_and(|t| (start as u64) < t)) {
                return Err(Failure::InvalidResponse(
                    "a page was empty while the reported total is not reached",
                ));
            }
            let done = got == 0
                || is_last == Some(true)
                || (is_last.is_none() && total.is_some_and(|total| start as u64 >= total))
                || (is_last.is_none() && total.is_none() && got < PAGE_SIZE);
            if done {
                return Ok(items);
            }
        }
    }
}

impl ZephyrClient {
    /// `_process_test_case`: the case's steps, else its script, else
    /// neither (`("empty", null)`). Zephyr Scale and Essential serve both
    /// under `/testcases/{key}`; a 404 on the script is "none".
    pub(crate) async fn case_content(&self, key: &str) -> Result<(&'static str, Value), Failure> {
        let steps = self.values(&["testcases", key, "teststeps"], &[]).await?;
        if !steps.is_empty() {
            return Ok(("steps", Value::Array(steps)));
        }
        match self
            .json_or_absent(&["testcases", key, "testscript"], &[])
            .await?
        {
            Some(Value::Object(script)) if !script.is_empty() => {
                Ok(("script", Value::Object(script)))
            }
            _ => Ok(("empty", Value::Null)),
        }
    }
}

/// The metadata the SDK copies from a test case: every value that is a
/// string, a number, a boolean, a list or an object.
pub(crate) fn case_metadata(case: &Value) -> serde_json::Map<String, Value> {
    case.as_object()
        .into_iter()
        .flatten()
        .filter(|(_, value)| !value.is_null())
        .map(|(name, value)| (name.clone(), value.clone()))
        .collect()
}
