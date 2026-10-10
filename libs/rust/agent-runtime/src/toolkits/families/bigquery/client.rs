use std::fmt;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use chrono::DateTime;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::time::timeout;
use zeroize::Zeroizing;

use super::config::BigQueryToolkitConfig;
use super::format::Cell;
use crate::toolkits::families::gcp::client::{
    GcpClientError, GcpClientErrorCode, access_token_from_response, service_account_token_request,
};

const API_ORIGIN: &str = "https://bigquery.googleapis.com/";
const BIGQUERY_SCOPE: &str = "https://www.googleapis.com/auth/bigquery";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
const TOKEN_TIMEOUT: Duration = Duration::from_secs(10);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
/// Server-side wait per jobs.query / jobs.getQueryResults call.
const QUERY_WAIT_MS: u64 = 10_000;
/// Incomplete-job polls after the initial jobs.query: about one minute.
const MAX_INCOMPLETE_POLLS: usize = 6;
/// Result pages after the first; with [`PAGE_ROWS`] this is the row bound.
const MAX_PAGES: usize = 10;
const PAGE_ROWS: u64 = 1_000;
const MAX_ROWS: usize = 1_000;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_TOKEN_RESPONSE_BYTES: usize = 64 * 1_024;
const MAX_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const MAX_RECORD_DEPTH: usize = 15;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BigQueryClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    Authorization,
    NotFound,
    RateLimited,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
    UnknownOutcome,
}

/// Stable `BigQuery` failure without account, key, SQL, row or provider data.
pub(crate) struct BigQueryClientError {
    code: BigQueryClientErrorCode,
    retryable: bool,
}

impl BigQueryClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> BigQueryClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            BigQueryClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "bigquery.configuration.invalid",
                "the BigQuery toolkit configuration is invalid",
            ),
            BigQueryClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "bigquery.request.rejected",
                "BigQuery rejected the request; check the filter, identifiers and arguments",
            ),
            BigQueryClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "bigquery.authentication.failed",
                "BigQuery service-account authentication failed",
            ),
            BigQueryClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "bigquery.authorization.failed",
                "BigQuery did not authorize the request",
            ),
            BigQueryClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "bigquery.resource.not_found",
                "the requested BigQuery project, dataset, table or job was not found",
            ),
            BigQueryClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "bigquery.rate_limited",
                "BigQuery rate limited the request",
            ),
            BigQueryClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "bigquery.timeout",
                "the BigQuery request timed out",
            ),
            BigQueryClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "bigquery.unavailable",
                "BigQuery is unavailable",
            ),
            BigQueryClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "bigquery.response.invalid",
                "BigQuery returned an invalid response",
            ),
            BigQueryClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "bigquery.result.resource_exhausted",
                "the BigQuery request or result exceeds the approved limit; narrow the filter or lower k",
            ),
            BigQueryClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "bigquery.effect.unknown_outcome",
                "BigQuery may have applied the requested change; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(
        code: BigQueryClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for BigQueryClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BigQueryClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for BigQueryClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            BigQueryClientErrorCode::InvalidConfiguration => {
                "the BigQuery client configuration is invalid"
            }
            BigQueryClientErrorCode::InvalidInput => "BigQuery rejected the request",
            BigQueryClientErrorCode::Authentication => "BigQuery authentication failed",
            BigQueryClientErrorCode::Authorization => "BigQuery authorization failed",
            BigQueryClientErrorCode::NotFound => "the BigQuery resource was not found",
            BigQueryClientErrorCode::RateLimited => "BigQuery rate limited the request",
            BigQueryClientErrorCode::Timeout => "the BigQuery request timed out",
            BigQueryClientErrorCode::DependencyUnavailable => "BigQuery is unavailable",
            BigQueryClientErrorCode::InvalidResponse => "BigQuery returned an invalid response",
            BigQueryClientErrorCode::ResourceExhausted => {
                "the BigQuery result exceeds its approved limit"
            }
            BigQueryClientErrorCode::UnknownOutcome => {
                "the BigQuery effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for BigQueryClientError {}

impl From<GcpClientError> for BigQueryClientError {
    fn from(source: GcpClientError) -> Self {
        let code = match source.code() {
            GcpClientErrorCode::InvalidConfiguration => {
                BigQueryClientErrorCode::InvalidConfiguration
            }
            GcpClientErrorCode::InvalidInput => BigQueryClientErrorCode::InvalidInput,
            GcpClientErrorCode::Authentication => BigQueryClientErrorCode::Authentication,
            GcpClientErrorCode::Authorization => BigQueryClientErrorCode::Authorization,
            GcpClientErrorCode::NotFound => BigQueryClientErrorCode::NotFound,
            GcpClientErrorCode::RateLimited => BigQueryClientErrorCode::RateLimited,
            GcpClientErrorCode::Timeout => BigQueryClientErrorCode::Timeout,
            GcpClientErrorCode::DependencyUnavailable => {
                BigQueryClientErrorCode::DependencyUnavailable
            }
            GcpClientErrorCode::InvalidResponse => BigQueryClientErrorCode::InvalidResponse,
            GcpClientErrorCode::ResourceExhausted => BigQueryClientErrorCode::ResourceExhausted,
            GcpClientErrorCode::UnknownOutcome => BigQueryClientErrorCode::UnknownOutcome,
        };
        Self {
            code,
            retryable: source.retryable(),
        }
    }
}

/// One query job: `GoogleSQL` text, its named parameters and whether it can
/// change `BigQuery` state (DDL), which decides how an ambiguous failure reads.
pub(in crate::toolkits) struct QueryJob {
    pub(in crate::toolkits) sql: String,
    pub(in crate::toolkits) parameters: Vec<Value>,
    pub(in crate::toolkits) effect: bool,
}

/// The `BigQuery` operations the tools need, behind one seam for fixtures.
#[async_trait]
pub(in crate::toolkits) trait BigQueryApi: Send + Sync {
    /// Run one query job to completion and decode every result row.
    async fn query(&self, job: QueryJob) -> Result<Vec<Cell>, BigQueryClientError>;

    /// jobs.get, projected to the job's `statistics` object.
    async fn job_statistics(&self, job_id: &str) -> Result<Value, BigQueryClientError>;

    /// tables.insert with `exists_ok=True`: an existing table is read back.
    async fn create_table(
        &self,
        project: &str,
        dataset: &str,
        table: &Value,
    ) -> Result<Value, BigQueryClientError>;
}

pub(in crate::toolkits) struct BigQueryHttpResponse {
    status: StatusCode,
    body: Vec<u8>,
}

impl BigQueryHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) fn fixture(status: StatusCode, body: impl Into<Vec<u8>>) -> Self {
        Self {
            status,
            body: body.into(),
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait BigQueryTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<BigQueryHttpResponse, BigQueryClientError>;
}

struct ReqwestBigQueryTransport {
    http: reqwest::Client,
}

#[async_trait]
impl BigQueryTransport for ReqwestBigQueryTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<BigQueryHttpResponse, BigQueryClientError> {
        let mut response = self
            .http
            .execute(request)
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?;
        if response
            .headers()
            .get(CONTENT_LENGTH)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|length| length > response_limit)
        {
            return Err(bound_failure(effect));
        }
        let status = response.status();
        let mut body = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            let next = body
                .len()
                .checked_add(chunk.len())
                .ok_or_else(|| bound_failure(effect))?;
            if next > response_limit {
                return Err(bound_failure(effect));
            }
            body.extend_from_slice(&chunk);
        }
        Ok(BigQueryHttpResponse { status, body })
    }
}

/// Invocation-owned `BigQuery` REST client over one sealed service account.
pub(crate) struct BigQueryClient {
    config: BigQueryToolkitConfig,
    transport: Arc<dyn BigQueryTransport>,
}

impl BigQueryClient {
    pub(crate) fn new(config: BigQueryToolkitConfig) -> Result<Self, BigQueryClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT_VALUE)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self {
            config,
            transport: Arc::new(ReqwestBigQueryTransport { http }),
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: BigQueryToolkitConfig,
        transport: Arc<dyn BigQueryTransport>,
    ) -> Self {
        Self { config, transport }
    }

    async fn access_token(&self) -> Result<Zeroizing<String>, BigQueryClientError> {
        let request = service_account_token_request(
            self.config.credentials(),
            &[BIGQUERY_SCOPE.to_owned()],
            SystemTime::now(),
        )?;
        let response = timeout(
            TOKEN_TIMEOUT,
            self.transport
                .execute(request, false, MAX_TOKEN_RESPONSE_BYTES),
        )
        .await
        .map_err(|_| timeout_error())??;
        Ok(access_token_from_response(response.status, response.body)?)
    }

    fn job_project(&self) -> Result<&str, BigQueryClientError> {
        self.config.job_project().ok_or_else(invalid_configuration)
    }

    /// One authorized call; a non-2xx status is returned for the caller to
    /// classify, because 409 is an answer for tables.insert.
    async fn send(
        &self,
        token: &str,
        method: Method,
        url: Url,
        body: Option<&Value>,
        effect: bool,
    ) -> Result<(StatusCode, Vec<u8>), BigQueryClientError> {
        let mut request = Request::new(method, url);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        headers.insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        let mut bearer = Zeroizing::new(String::from("Bearer "));
        bearer.push_str(token);
        let mut authorization = HeaderValue::from_str(&bearer).map_err(|_| invalid_response())?;
        authorization.set_sensitive(true);
        headers.insert(AUTHORIZATION, authorization);
        if let Some(body) = body {
            let bytes = serde_json::to_vec(body).map_err(|_| invalid_input())?;
            if bytes.len() > MAX_REQUEST_BYTES {
                return Err(resource_exhausted());
            }
            let headers = request.headers_mut();
            headers.insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
            headers.insert(
                CONTENT_LENGTH,
                HeaderValue::from_str(&bytes.len().to_string()).map_err(|_| invalid_input())?,
            );
            *request.body_mut() = Some(bytes.into());
        }
        let response = timeout(
            REQUEST_TIMEOUT + Duration::from_secs(5),
            self.transport.execute(request, effect, MAX_RESPONSE_BYTES),
        )
        .await
        .map_err(|_| {
            if effect {
                unknown_outcome()
            } else {
                timeout_error()
            }
        })??;
        Ok((response.status, response.body))
    }

    async fn send_json(
        &self,
        token: &str,
        method: Method,
        url: Url,
        body: Option<&Value>,
        effect: bool,
    ) -> Result<Map<String, Value>, BigQueryClientError> {
        let (status, body) = self.send(token, method, url, body, effect).await?;
        if !status.is_success() {
            return Err(map_status(status, effect));
        }
        parse_object(&body, effect)
    }

    fn query_results_url(
        project: &str,
        job_id: &str,
        location: Option<&str>,
        page_token: Option<&str>,
    ) -> Result<Url, BigQueryClientError> {
        let mut url = api_url(&["projects", project, "queries", job_id])?;
        {
            let mut query = url.query_pairs_mut();
            query.append_pair("maxResults", &PAGE_ROWS.to_string());
            query.append_pair("timeoutMs", &QUERY_WAIT_MS.to_string());
            query.append_pair("formatOptions.useInt64Timestamp", "true");
            if let Some(location) = location {
                query.append_pair("location", location);
            }
            if let Some(page_token) = page_token {
                query.append_pair("pageToken", page_token);
            }
        }
        Ok(url)
    }
}

#[async_trait]
impl BigQueryApi for BigQueryClient {
    async fn query(&self, job: QueryJob) -> Result<Vec<Cell>, BigQueryClientError> {
        let project = self.job_project()?.to_owned();
        let token = self.access_token().await?;
        let mut body = json!({
            "query": job.sql,
            "useLegacySql": false,
            "maxResults": PAGE_ROWS,
            "timeoutMs": QUERY_WAIT_MS,
            "formatOptions": {"useInt64Timestamp": true}
        });
        if let Some(location) = self.config.location() {
            body["location"] = Value::String(location.to_owned());
        }
        if !job.parameters.is_empty() {
            body["parameterMode"] = Value::String("NAMED".to_owned());
            body["queryParameters"] = Value::Array(job.parameters);
        }
        let mut response = self
            .send_json(
                &token,
                Method::POST,
                api_url(&["projects", &project, "queries"])?,
                Some(&body),
                job.effect,
            )
            .await?;
        // Once jobs.query has answered, the job exists: later waits and pages
        // are reads of its results, and the job id identifies it for
        // reconciliation.
        let reference = response
            .get("jobReference")
            .and_then(Value::as_object)
            .cloned();
        let job_id = reference
            .as_ref()
            .and_then(|reference| reference.get("jobId"))
            .and_then(Value::as_str)
            .map(str::to_owned);
        let location = reference
            .as_ref()
            .and_then(|reference| reference.get("location"))
            .and_then(Value::as_str)
            .map(str::to_owned)
            .or_else(|| self.config.location().map(str::to_owned));
        let mut polls = 0;
        while response.get("jobComplete") != Some(&Value::Bool(true)) {
            polls += 1;
            let Some(job_id) = job_id.as_deref() else {
                return Err(post_accept_failure(job.effect));
            };
            if polls > MAX_INCOMPLETE_POLLS {
                return Err(if job.effect {
                    unknown_outcome()
                } else {
                    timeout_error()
                });
            }
            let url = Self::query_results_url(&project, job_id, location.as_deref(), None)?;
            response = self
                .send_json(&token, Method::GET, url, None, false)
                .await
                .map_err(|error| effect_aware(error, job.effect))?;
        }
        let schema = response
            .get("schema")
            .and_then(|schema| schema.get("fields"))
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        let mut rows = Vec::new();
        decode_rows(&schema, response.get("rows"), &mut rows)
            .map_err(|error| effect_aware(error, job.effect))?;
        let mut page_token = next_page_token(&response);
        let mut pages = 0;
        while let Some(token_value) = page_token {
            pages += 1;
            let Some(job_id) = job_id.as_deref() else {
                return Err(post_accept_failure(job.effect));
            };
            if pages > MAX_PAGES {
                return Err(post_accept_bound_failure(job.effect));
            }
            let url =
                Self::query_results_url(&project, job_id, location.as_deref(), Some(&token_value))?;
            let page = self
                .send_json(&token, Method::GET, url, None, false)
                .await
                .map_err(|error| effect_aware(error, job.effect))?;
            decode_rows(&schema, page.get("rows"), &mut rows)
                .map_err(|error| effect_aware(error, job.effect))?;
            page_token = next_page_token(&page);
        }
        Ok(rows)
    }

    async fn job_statistics(&self, job_id: &str) -> Result<Value, BigQueryClientError> {
        let project = self.job_project()?.to_owned();
        let token = self.access_token().await?;
        let mut url = api_url(&["projects", &project, "jobs", job_id])?;
        if let Some(location) = self.config.location() {
            url.query_pairs_mut().append_pair("location", location);
        }
        let mut job = self
            .send_json(&token, Method::GET, url, None, false)
            .await?;
        // `Job._properties.get("statistics", {})`.
        Ok(job.remove("statistics").unwrap_or_else(|| json!({})))
    }

    async fn create_table(
        &self,
        project: &str,
        dataset: &str,
        table: &Value,
    ) -> Result<Value, BigQueryClientError> {
        let token = self.access_token().await?;
        let url = api_url(&["projects", project, "datasets", dataset, "tables"])?;
        let (status, body) = self
            .send(&token, Method::POST, url, Some(table), true)
            .await?;
        if status.is_success() {
            return parse_object(&body, true).map(Value::Object);
        }
        if status != StatusCode::CONFLICT {
            return Err(map_status(status, true));
        }
        // `create_table(table, exists_ok=True)`: the existing table is the answer.
        let table_id = table
            .pointer("/tableReference/tableId")
            .and_then(Value::as_str)
            .ok_or_else(invalid_input)?;
        let url = api_url(&["projects", project, "datasets", dataset, "tables", table_id])?;
        self.send_json(&token, Method::GET, url, None, false)
            .await
            .map(Value::Object)
    }
}

fn api_url(segments: &[&str]) -> Result<Url, BigQueryClientError> {
    let mut url = Url::parse(API_ORIGIN).map_err(|_| invalid_configuration())?;
    {
        let mut path = url
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?;
        path.clear();
        path.push("bigquery");
        path.push("v2");
        for segment in segments {
            if segment.is_empty() || matches!(*segment, "." | "..") {
                return Err(invalid_input());
            }
            path.push(segment);
        }
    }
    Ok(url)
}

fn next_page_token(response: &Map<String, Value>) -> Option<String> {
    response
        .get("pageToken")
        .and_then(Value::as_str)
        .filter(|token| !token.is_empty())
        .map(str::to_owned)
}

fn parse_object(body: &[u8], effect: bool) -> Result<Map<String, Value>, BigQueryClientError> {
    match serde_json::from_slice::<Value>(body) {
        Ok(Value::Object(object)) => Ok(object),
        _ => Err(post_accept_failure(effect)),
    }
}

/// Decode a page of `rows` (`{"f":[{"v":...}]}`) against the result schema.
fn decode_rows(
    schema: &[Value],
    rows: Option<&Value>,
    out: &mut Vec<Cell>,
) -> Result<(), BigQueryClientError> {
    let Some(rows) = rows else {
        return Ok(());
    };
    let rows = rows.as_array().ok_or_else(invalid_response)?;
    for row in rows {
        if out.len() >= MAX_ROWS {
            return Err(resource_exhausted());
        }
        out.push(decode_record(schema, row, 0)?);
    }
    Ok(())
}

fn decode_record(
    fields: &[Value],
    record: &Value,
    depth: usize,
) -> Result<Cell, BigQueryClientError> {
    if depth > MAX_RECORD_DEPTH {
        return Err(invalid_response());
    }
    let values = record
        .get("f")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if values.len() != fields.len() {
        return Err(invalid_response());
    }
    let mut decoded = Vec::with_capacity(fields.len());
    for (field, value) in fields.iter().zip(values) {
        let name = field
            .get("name")
            .and_then(Value::as_str)
            .ok_or_else(invalid_response)?;
        let value = value.get("v").ok_or_else(invalid_response)?;
        decoded.push((name.to_owned(), decode_field(field, value, depth)?));
    }
    Ok(Cell::Record(decoded))
}

fn decode_field(field: &Value, value: &Value, depth: usize) -> Result<Cell, BigQueryClientError> {
    if field.get("mode").and_then(Value::as_str) == Some("REPEATED") {
        let Some(items) = value.as_array() else {
            // A NULL repeated field decodes to an empty list in the client.
            return if value.is_null() {
                Ok(Cell::List(Vec::new()))
            } else {
                Err(invalid_response())
            };
        };
        return items
            .iter()
            .map(|item| {
                let item = item.get("v").ok_or_else(invalid_response)?;
                decode_scalar(field, item, depth)
            })
            .collect::<Result<Vec<_>, _>>()
            .map(Cell::List);
    }
    decode_scalar(field, value, depth)
}

fn decode_scalar(field: &Value, value: &Value, depth: usize) -> Result<Cell, BigQueryClientError> {
    if value.is_null() {
        return Ok(Cell::Null);
    }
    let kind = field
        .get("type")
        .and_then(Value::as_str)
        .unwrap_or("STRING");
    if matches!(kind, "RECORD" | "STRUCT") {
        let fields = field
            .get("fields")
            .and_then(Value::as_array)
            .ok_or_else(invalid_response)?;
        return decode_record(fields, value, depth + 1);
    }
    let text = value.as_str().ok_or_else(invalid_response)?;
    Ok(match kind {
        "INTEGER" | "INT64" => Cell::Int(text.parse().map_err(|_| invalid_response())?),
        "FLOAT" | "FLOAT64" => Cell::Float(match text {
            "NaN" => f64::NAN,
            "Infinity" => f64::INFINITY,
            "-Infinity" => f64::NEG_INFINITY,
            other => other.parse().map_err(|_| invalid_response())?,
        }),
        "BOOLEAN" | "BOOL" => Cell::Bool(match text {
            "true" => true,
            "false" => false,
            _ => return Err(invalid_response()),
        }),
        "TIMESTAMP" => Cell::Text(python_timestamp(text)?),
        "DATETIME" => Cell::Text(python_time_text(&text.replacen('T', " ", 1))),
        "TIME" => Cell::Text(python_time_text(text)),
        "JSON" => {
            serde_json::from_str(text).map_or_else(|_| Cell::Text(text.to_owned()), Cell::Json)
        }
        // STRING, BYTES (base64, as the REST API returns it), NUMERIC and
        // BIGNUMERIC (`str(Decimal)`), DATE, GEOGRAPHY, INTERVAL, RANGE.
        _ => Cell::Text(text.to_owned()),
    })
}

/// `str(datetime)` of the UTC `datetime` the client builds from INT64
/// microseconds: `YYYY-MM-DD HH:MM:SS[.ffffff]+00:00`.
fn python_timestamp(micros: &str) -> Result<String, BigQueryClientError> {
    let micros: i64 = micros.parse().map_err(|_| invalid_response())?;
    let instant = DateTime::from_timestamp_micros(micros).ok_or_else(invalid_response)?;
    let mut text = instant.format("%Y-%m-%d %H:%M:%S").to_string();
    if micros.rem_euclid(1_000_000) != 0 {
        text.push_str(&instant.format("%.6f").to_string());
    }
    text.push_str("+00:00");
    Ok(text)
}

/// `str(time)` / `str(datetime)`: a fractional second has exactly six digits.
fn python_time_text(value: &str) -> String {
    let Some((whole, fraction)) = value.split_once('.') else {
        return value.to_owned();
    };
    let digits: String = fraction.chars().take(6).collect();
    if digits.chars().all(|digit| digit == '0') {
        return whole.to_owned();
    }
    format!("{whole}.{digits:0<6}")
}

fn map_status(status: StatusCode, effect: bool) -> BigQueryClientError {
    if effect
        && (status == StatusCode::REQUEST_TIMEOUT
            || status == StatusCode::TOO_MANY_REQUESTS
            || status.is_server_error()
            || status.is_redirection())
    {
        return unknown_outcome();
    }
    match status {
        StatusCode::BAD_REQUEST => invalid_input(),
        StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::NOT_FOUND => not_found(),
        StatusCode::REQUEST_TIMEOUT => timeout_error(),
        StatusCode::TOO_MANY_REQUESTS => rate_limited(),
        status if status.is_server_error() => dependency_unavailable(),
        _ => invalid_response(),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> BigQueryClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        timeout_error()
    } else if source.is_connect() {
        dependency_unavailable()
    } else {
        invalid_response()
    }
}

/// After an effectful job was accepted, any failure to read it back leaves
/// its outcome unknown.
fn effect_aware(error: BigQueryClientError, effect: bool) -> BigQueryClientError {
    if effect && !matches!(error.code, BigQueryClientErrorCode::InvalidInput) {
        unknown_outcome()
    } else {
        error
    }
}

fn post_accept_failure(effect: bool) -> BigQueryClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

fn post_accept_bound_failure(effect: bool) -> BigQueryClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

fn bound_failure(effect: bool) -> BigQueryClientError {
    post_accept_bound_failure(effect)
}

const fn invalid_configuration() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn rate_limited() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::RateLimited,
        retryable: true,
    }
}

const fn timeout_error() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::Timeout,
        retryable: true,
    }
}

const fn dependency_unavailable() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::DependencyUnavailable,
        retryable: true,
    }
}

const fn invalid_response() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

pub(in crate::toolkits) const fn resource_exhausted() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> BigQueryClientError {
    BigQueryClientError {
        code: BigQueryClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
