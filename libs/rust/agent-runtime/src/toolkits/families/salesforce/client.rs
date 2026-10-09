use std::fmt;
use std::fmt::Write as _;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use percent_encoding::{NON_ALPHANUMERIC, utf8_percent_encode};
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;
use zeroize::Zeroizing;

use super::config::SalesforceToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 128 * 1_024;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_ACCESS_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_RELATIVE_URL_BYTES: usize = 4 * 1_024;
const MAX_QUERY_PARAMETERS: usize = 128;
const MAX_QUERY_KEY_BYTES: usize = 256;
const MAX_QUERY_VALUE_BYTES: usize = 16 * 1_024;
const SALESFORCE_ID_SHORT_BYTES: usize = 15;
const SALESFORCE_ID_LONG_BYTES: usize = 18;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const FORM_CONTENT_TYPE: &str = "application/x-www-form-urlencoded";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SalesforceClientErrorCode {
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

/// One stable Salesforce failure without credentials, provider data, or URLs.
pub struct SalesforceClientError {
    code: SalesforceClientErrorCode,
    retryable: bool,
}

impl SalesforceClientError {
    #[must_use]
    pub const fn code(&self) -> SalesforceClientErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        self.retryable
    }

    pub fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            SalesforceClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "salesforce.configuration.invalid",
                "the Salesforce toolkit configuration is invalid",
            ),
            SalesforceClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "salesforce.request.invalid",
                "the Salesforce request is invalid",
            ),
            SalesforceClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "salesforce.authentication.failed",
                "Salesforce authentication failed",
            ),
            SalesforceClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "salesforce.authorization.failed",
                "Salesforce did not authorize the request",
            ),
            SalesforceClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "salesforce.resource.not_found",
                "the requested Salesforce resource was not found",
            ),
            SalesforceClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "salesforce.rate_limited",
                "Salesforce rate limited the request",
            ),
            SalesforceClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "salesforce.timeout",
                "the Salesforce request timed out",
            ),
            SalesforceClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "salesforce.unavailable",
                "Salesforce is unavailable",
            ),
            SalesforceClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "salesforce.response.invalid",
                "Salesforce returned an invalid response",
            ),
            SalesforceClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "salesforce.response.resource_exhausted",
                "the Salesforce response exceeds the approved limit",
            ),
            SalesforceClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "salesforce.effect.unknown_outcome",
                "Salesforce may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(any(test, feature = "test-support"))]
    pub const fn fixture(code: SalesforceClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for SalesforceClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SalesforceClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SalesforceClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SalesforceClientErrorCode::InvalidConfiguration => {
                "the Salesforce client configuration is invalid"
            }
            SalesforceClientErrorCode::InvalidInput => "the Salesforce request is invalid",
            SalesforceClientErrorCode::Authentication => "Salesforce authentication failed",
            SalesforceClientErrorCode::Authorization => "Salesforce authorization failed",
            SalesforceClientErrorCode::NotFound => "the Salesforce resource was not found",
            SalesforceClientErrorCode::RateLimited => "Salesforce rate limited the request",
            SalesforceClientErrorCode::Timeout => "the Salesforce request timed out",
            SalesforceClientErrorCode::DependencyUnavailable => "Salesforce is unavailable",
            SalesforceClientErrorCode::InvalidResponse => "Salesforce returned an invalid response",
            SalesforceClientErrorCode::ResourceExhausted => {
                "the Salesforce response exceeds its approved limit"
            }
            SalesforceClientErrorCode::UnknownOutcome => {
                "the Salesforce effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for SalesforceClientError {}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SalesforceMethod {
    Get,
    Post,
    Patch,
    Delete,
}

impl SalesforceMethod {
    pub fn parse(value: &str) -> Result<Self, SalesforceClientError> {
        match value {
            "GET" => Ok(Self::Get),
            "POST" => Ok(Self::Post),
            "PATCH" => Ok(Self::Patch),
            "DELETE" => Ok(Self::Delete),
            _ => Err(invalid_input()),
        }
    }

    const fn as_str(self) -> &'static str {
        match self {
            Self::Get => "GET",
            Self::Post => "POST",
            Self::Patch => "PATCH",
            Self::Delete => "DELETE",
        }
    }

    const fn is_effect(self) -> bool {
        !matches!(self, Self::Get)
    }

    fn reqwest(self) -> Method {
        match self {
            Self::Get => Method::GET,
            Self::Post => Method::POST,
            Self::Patch => Method::PATCH,
            Self::Delete => Method::DELETE,
        }
    }
}

#[async_trait]
pub trait SalesforceApi: Send + Sync {
    async fn create_case(
        &self,
        subject: &str,
        description: &str,
        origin: &str,
        status: &str,
    ) -> Result<Value, SalesforceClientError>;

    async fn create_lead(
        &self,
        last_name: &str,
        company: &str,
        email: &str,
        phone: &str,
    ) -> Result<Value, SalesforceClientError>;

    async fn search_salesforce(
        &self,
        object_type: &str,
        query: &str,
    ) -> Result<Value, SalesforceClientError>;

    async fn update_case(
        &self,
        case_id: &str,
        status: &str,
        description: Option<&str>,
    ) -> Result<Value, SalesforceClientError>;

    async fn update_lead(
        &self,
        lead_id: &str,
        email: Option<&str>,
        phone: Option<&str>,
    ) -> Result<Value, SalesforceClientError>;

    async fn execute_generic(
        &self,
        method: SalesforceMethod,
        relative_url: &str,
        params: &Map<String, Value>,
    ) -> Result<Value, SalesforceClientError>;
}

#[derive(Clone, Copy)]
pub enum SalesforceRequestKind<'a> {
    CreateCase {
        subject: &'a str,
        description: &'a str,
        origin: &'a str,
        status: &'a str,
    },
    CreateLead {
        last_name: &'a str,
        company: &'a str,
        email: &'a str,
        phone: &'a str,
    },
    Search {
        query: &'a str,
    },
    UpdateCase {
        case_id: &'a str,
        status: &'a str,
        description: Option<&'a str>,
    },
    UpdateLead {
        lead_id: &'a str,
        email: Option<&'a str>,
        phone: Option<&'a str>,
    },
    Generic {
        method: SalesforceMethod,
        relative_url: &'a str,
        params: &'a Map<String, Value>,
    },
}

impl SalesforceRequestKind<'_> {
    const fn is_effect(self) -> bool {
        match self {
            Self::Search { .. } => false,
            Self::Generic { method, .. } => method.is_effect(),
            Self::CreateCase { .. }
            | Self::CreateLead { .. }
            | Self::UpdateCase { .. }
            | Self::UpdateLead { .. } => true,
        }
    }
}

pub struct SalesforceHttpResponse {
    status: StatusCode,
    body: Option<Value>,
    body_was_empty: bool,
    json_content_type: bool,
}

impl SalesforceHttpResponse {
    #[cfg(any(test, feature = "test-support"))]
    pub const fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self {
            status,
            body_was_empty: body.is_none(),
            body,
            json_content_type: true,
        }
    }
}

#[async_trait]
pub trait SalesforceTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<SalesforceHttpResponse, SalesforceClientError>;
}

struct ReqwestSalesforceTransport {
    http: reqwest::Client,
}

#[async_trait]
impl SalesforceTransport for ReqwestSalesforceTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<SalesforceHttpResponse, SalesforceClientError> {
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
            .is_some_and(|length| length > MAX_RESPONSE_BYTES)
        {
            return Err(response_bound_failure(effect));
        }
        let json_content_type = response
            .headers()
            .get(CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .is_some_and(|value| value.trim().eq_ignore_ascii_case(JSON_CONTENT_TYPE));
        let mut bytes = Vec::new();
        while let Some(chunk) = response
            .chunk()
            .await
            .map_err(|source| map_reqwest_error(&source, effect))?
        {
            let next = bytes
                .len()
                .checked_add(chunk.len())
                .ok_or_else(|| response_bound_failure(effect))?;
            if next > MAX_RESPONSE_BYTES {
                return Err(response_bound_failure(effect));
            }
            bytes.extend_from_slice(&chunk);
        }
        let body_was_empty = bytes.is_empty();
        let body = if body_was_empty {
            None
        } else {
            serde_json::from_slice(&bytes).ok()
        };
        Ok(SalesforceHttpResponse {
            status: response.status(),
            body,
            body_was_empty,
            json_content_type,
        })
    }
}

struct TokenState {
    value: Option<Zeroizing<String>>,
    generation: u64,
}

/// One invocation-scoped Salesforce client, token cache, and HTTP pool.
pub struct SalesforceClient {
    config: SalesforceToolkitConfig,
    transport: Arc<dyn SalesforceTransport>,
    token: Mutex<TokenState>,
}

impl SalesforceClient {
    pub fn new(config: SalesforceToolkitConfig) -> Result<Self, SalesforceClientError> {
        let http = reqwest::Client::builder()
            .https_only(true)
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(REQUEST_TIMEOUT)
            .pool_idle_timeout(POOL_IDLE_TIMEOUT)
            .pool_max_idle_per_host(MAX_IDLE_PER_HOST)
            .user_agent(USER_AGENT_VALUE)
            .build()
            .map_err(|_| invalid_configuration())?;
        Ok(Self::with_transport_inner(
            config,
            Arc::new(ReqwestSalesforceTransport { http }),
        ))
    }

    fn with_transport_inner(
        config: SalesforceToolkitConfig,
        transport: Arc<dyn SalesforceTransport>,
    ) -> Self {
        Self {
            config,
            transport,
            token: Mutex::new(TokenState {
                value: None,
                generation: 0,
            }),
        }
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn with_transport(
        config: SalesforceToolkitConfig,
        transport: Arc<dyn SalesforceTransport>,
    ) -> Self {
        Self::with_transport_inner(config, transport)
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn test_auth_request(&self) -> Result<Request, SalesforceClientError> {
        self.build_auth_request()
    }

    #[cfg(any(test, feature = "test-support"))]
    pub fn test_request(
        &self,
        kind: SalesforceRequestKind<'_>,
        token: &str,
    ) -> Result<Request, SalesforceClientError> {
        self.build_resource_request(kind, authorization_header(token)?)
    }

    fn build_auth_request(&self) -> Result<Request, SalesforceClientError> {
        let url = self
            .config
            .origin()
            .join("services/oauth2/token")
            .map_err(|_| invalid_configuration())?;
        let mut body = Zeroizing::new(String::new());
        body.push_str("grant_type=client_credentials&client_id=");
        write!(
            body,
            "{}",
            utf8_percent_encode(self.config.client_id(), NON_ALPHANUMERIC)
        )
        .map_err(|_| invalid_configuration())?;
        body.push_str("&client_secret=");
        write!(
            body,
            "{}",
            utf8_percent_encode(self.config.client_secret(), NON_ALPHANUMERIC)
        )
        .map_err(|_| invalid_configuration())?;
        if body.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        request_with_body(Method::POST, url, body.as_bytes(), FORM_CONTENT_TYPE, None)
    }

    fn api_root(&self) -> Result<Url, SalesforceClientError> {
        self.config
            .origin()
            .join(&format!("services/data/{}/", self.config.api_version()))
            .map_err(|_| invalid_configuration())
    }

    fn build_resource_request(
        &self,
        kind: SalesforceRequestKind<'_>,
        authorization: HeaderValue,
    ) -> Result<Request, SalesforceClientError> {
        let root = self.api_root()?;
        let RequestParts { method, url, body } = request_parts(&root, kind)?;

        let mut request = Request::new(method, url);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request
            .headers_mut()
            .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        request.headers_mut().insert(AUTHORIZATION, authorization);
        if let Some(body) = body {
            set_json_body(&mut request, body)?;
        }
        Ok(request)
    }

    async fn authorization(&self) -> Result<(HeaderValue, u64), SalesforceClientError> {
        let mut token = self.token.lock().await;
        if token.value.is_none() {
            let response = self
                .transport
                .execute(self.build_auth_request()?, false)
                .await?;
            map_auth_status(response.status)?;
            if !response.json_content_type {
                return Err(invalid_response());
            }
            let value = response
                .body
                .as_ref()
                .and_then(Value::as_object)
                .and_then(|object| object.get("access_token"))
                .and_then(Value::as_str)
                .ok_or_else(invalid_response)?;
            validate_access_token(value)?;
            token.generation = token
                .generation
                .checked_add(1)
                .ok_or_else(resource_exhausted)?;
            token.value = Some(Zeroizing::new(value.to_owned()));
        }
        let value = token.value.as_deref().ok_or_else(invalid_response)?;
        Ok((authorization_header(value)?, token.generation))
    }

    async fn invalidate_token(&self, generation: u64) {
        let mut token = self.token.lock().await;
        if token.generation == generation {
            token.value = None;
        }
    }

    async fn execute_resource(
        &self,
        kind: SalesforceRequestKind<'_>,
    ) -> Result<SalesforceHttpResponse, SalesforceClientError> {
        let effect = kind.is_effect();
        let (authorization, generation) = self.authorization().await?;
        let response = self
            .transport
            .execute(self.build_resource_request(kind, authorization)?, effect)
            .await?;
        if response.status != StatusCode::UNAUTHORIZED {
            return Ok(response);
        }
        self.invalidate_token(generation).await;
        let (authorization, _) = self.authorization().await?;
        self.transport
            .execute(self.build_resource_request(kind, authorization)?, effect)
            .await
    }
}

#[async_trait]
impl SalesforceApi for SalesforceClient {
    async fn create_case(
        &self,
        subject: &str,
        description: &str,
        origin: &str,
        status: &str,
    ) -> Result<Value, SalesforceClientError> {
        let response = self
            .execute_resource(SalesforceRequestKind::CreateCase {
                subject,
                description,
                origin,
                status,
            })
            .await?;
        json_success(response, &[StatusCode::OK, StatusCode::CREATED], true)
    }

    async fn create_lead(
        &self,
        last_name: &str,
        company: &str,
        email: &str,
        phone: &str,
    ) -> Result<Value, SalesforceClientError> {
        let response = self
            .execute_resource(SalesforceRequestKind::CreateLead {
                last_name,
                company,
                email,
                phone,
            })
            .await?;
        json_success(response, &[StatusCode::OK, StatusCode::CREATED], true)
    }

    async fn search_salesforce(
        &self,
        _object_type: &str,
        query: &str,
    ) -> Result<Value, SalesforceClientError> {
        let response = self
            .execute_resource(SalesforceRequestKind::Search { query })
            .await?;
        json_success(response, &[StatusCode::OK], false)
    }

    async fn update_case(
        &self,
        case_id: &str,
        status: &str,
        description: Option<&str>,
    ) -> Result<Value, SalesforceClientError> {
        let response = self
            .execute_resource(SalesforceRequestKind::UpdateCase {
                case_id,
                status,
                description,
            })
            .await?;
        let message = format!("Case {case_id} updated successfully.");
        no_content_success(&response, &message)
    }

    async fn update_lead(
        &self,
        lead_id: &str,
        email: Option<&str>,
        phone: Option<&str>,
    ) -> Result<Value, SalesforceClientError> {
        let response = self
            .execute_resource(SalesforceRequestKind::UpdateLead {
                lead_id,
                email,
                phone,
            })
            .await?;
        let message = format!("Lead {lead_id} updated successfully.");
        no_content_success(&response, &message)
    }

    async fn execute_generic(
        &self,
        method: SalesforceMethod,
        relative_url: &str,
        params: &Map<String, Value>,
    ) -> Result<Value, SalesforceClientError> {
        let response = self
            .execute_resource(SalesforceRequestKind::Generic {
                method,
                relative_url,
                params,
            })
            .await?;
        if response.status == StatusCode::NO_CONTENT {
            let message = format!(
                "{} request to {relative_url} executed successfully.",
                method.as_str()
            );
            return no_content_success(&response, &message);
        }
        json_success(
            response,
            &[StatusCode::OK, StatusCode::CREATED],
            method.is_effect(),
        )
    }
}

struct RequestParts {
    method: Method,
    url: Url,
    body: Option<Vec<u8>>,
}

fn request_parts(
    root: &Url,
    kind: SalesforceRequestKind<'_>,
) -> Result<RequestParts, SalesforceClientError> {
    match kind {
        SalesforceRequestKind::CreateCase {
            subject,
            description,
            origin,
            status,
        } => json_parts(
            Method::POST,
            root,
            "sobjects/Case/",
            &json!({
                "Subject": subject,
                "Description": description,
                "Origin": origin,
                "Status": status,
            }),
        ),
        SalesforceRequestKind::CreateLead {
            last_name,
            company,
            email,
            phone,
        } => json_parts(
            Method::POST,
            root,
            "sobjects/Lead/",
            &json!({
                "LastName": last_name,
                "Company": company,
                "Email": email,
                "Phone": phone,
            }),
        ),
        SalesforceRequestKind::Search { query } => search_parts(root, query),
        SalesforceRequestKind::UpdateCase {
            case_id,
            status,
            description,
        } => update_case_parts(root, case_id, status, description),
        SalesforceRequestKind::UpdateLead {
            lead_id,
            email,
            phone,
        } => update_lead_parts(root, lead_id, email, phone),
        SalesforceRequestKind::Generic {
            method,
            relative_url,
            params,
        } => generic_parts(root, method, relative_url, params),
    }
}

fn json_parts(
    method: Method,
    root: &Url,
    path: &str,
    value: &Value,
) -> Result<RequestParts, SalesforceClientError> {
    Ok(RequestParts {
        method,
        url: root.join(path).map_err(|_| invalid_configuration())?,
        body: Some(encode_json(value)?),
    })
}

fn search_parts(root: &Url, query: &str) -> Result<RequestParts, SalesforceClientError> {
    let mut url = root.join("query").map_err(|_| invalid_configuration())?;
    url.query_pairs_mut().append_pair("q", query);
    Ok(RequestParts {
        method: Method::GET,
        url,
        body: None,
    })
}

fn update_case_parts(
    root: &Url,
    case_id: &str,
    status: &str,
    description: Option<&str>,
) -> Result<RequestParts, SalesforceClientError> {
    validate_record_id(case_id)?;
    let mut fields = Map::new();
    fields.insert("Status".to_owned(), Value::String(status.to_owned()));
    if let Some(description) = description.filter(|value| !value.is_empty()) {
        fields.insert(
            "Description".to_owned(),
            Value::String(description.to_owned()),
        );
    }
    json_parts(
        Method::PATCH,
        root,
        &format!("sobjects/Case/{case_id}"),
        &Value::Object(fields),
    )
}

fn update_lead_parts(
    root: &Url,
    lead_id: &str,
    email: Option<&str>,
    phone: Option<&str>,
) -> Result<RequestParts, SalesforceClientError> {
    validate_record_id(lead_id)?;
    let mut fields = Map::new();
    if let Some(email) = email.filter(|value| !value.is_empty()) {
        fields.insert("Email".to_owned(), Value::String(email.to_owned()));
    }
    if let Some(phone) = phone.filter(|value| !value.is_empty()) {
        fields.insert("Phone".to_owned(), Value::String(phone.to_owned()));
    }
    if fields.is_empty() {
        return Err(invalid_input());
    }
    json_parts(
        Method::PATCH,
        root,
        &format!("sobjects/Lead/{lead_id}"),
        &Value::Object(fields),
    )
}

fn generic_parts(
    root: &Url,
    method: SalesforceMethod,
    relative_url: &str,
    params: &Map<String, Value>,
) -> Result<RequestParts, SalesforceClientError> {
    let mut url = generic_url(root, relative_url)?;
    let body = if method == SalesforceMethod::Get {
        append_generic_query(&mut url, params)?;
        None
    } else {
        Some(encode_json(&Value::Object(params.clone()))?)
    };
    Ok(RequestParts {
        method: method.reqwest(),
        url,
        body,
    })
}

fn request_with_body(
    method: Method,
    url: Url,
    body: &[u8],
    content_type: &'static str,
    authorization: Option<HeaderValue>,
) -> Result<Request, SalesforceClientError> {
    if body.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    let mut request = Request::new(method, url);
    request
        .headers_mut()
        .insert(CONTENT_TYPE, HeaderValue::from_static(content_type));
    request.headers_mut().insert(
        CONTENT_LENGTH,
        HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_configuration())?,
    );
    request
        .headers_mut()
        .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
    if let Some(authorization) = authorization {
        request.headers_mut().insert(AUTHORIZATION, authorization);
    }
    *request.body_mut() = Some(body.to_vec().into());
    Ok(request)
}

fn set_json_body(request: &mut Request, body: Vec<u8>) -> Result<(), SalesforceClientError> {
    if body.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    request
        .headers_mut()
        .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
    request.headers_mut().insert(
        CONTENT_LENGTH,
        HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_configuration())?,
    );
    *request.body_mut() = Some(body.into());
    Ok(())
}

fn authorization_header(token: &str) -> Result<HeaderValue, SalesforceClientError> {
    validate_access_token(token)?;
    let mut header = Zeroizing::new(String::with_capacity(token.len() + 7));
    header.push_str("Bearer ");
    header.push_str(token);
    let mut value = HeaderValue::from_str(&header).map_err(|_| invalid_response())?;
    value.set_sensitive(true);
    Ok(value)
}

fn validate_access_token(value: &str) -> Result<(), SalesforceClientError> {
    if value.len() > MAX_ACCESS_TOKEN_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_response());
    }
    Ok(())
}

fn validate_record_id(value: &str) -> Result<(), SalesforceClientError> {
    if !matches!(
        value.len(),
        SALESFORCE_ID_SHORT_BYTES | SALESFORCE_ID_LONG_BYTES
    ) || !value.bytes().all(|byte| byte.is_ascii_alphanumeric())
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn generic_url(root: &Url, relative: &str) -> Result<Url, SalesforceClientError> {
    if relative.is_empty()
        || relative.len() > MAX_RELATIVE_URL_BYTES
        || !relative.starts_with('/')
        || relative.starts_with("//")
        || relative.contains(['\\', '?', '#', '%'])
        || relative.chars().any(char::is_control)
    {
        return Err(if relative.len() > MAX_RELATIVE_URL_BYTES {
            resource_exhausted()
        } else {
            invalid_input()
        });
    }
    if relative
        .split('/')
        .any(|part| matches!(part, "." | "..") || !valid_path_part(part))
    {
        return Err(invalid_input());
    }
    root.join(relative.trim_start_matches('/'))
        .map_err(|_| invalid_input())
}

fn valid_path_part(part: &str) -> bool {
    part.is_empty()
        || part
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~'))
}

fn append_generic_query(
    url: &mut Url,
    params: &Map<String, Value>,
) -> Result<(), SalesforceClientError> {
    if params.len() > MAX_QUERY_PARAMETERS {
        return Err(resource_exhausted());
    }
    let mut query = url.query_pairs_mut();
    for (key, value) in params {
        if key.is_empty()
            || key.len() > MAX_QUERY_KEY_BYTES
            || !key
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b'-'))
        {
            return Err(if key.len() > MAX_QUERY_KEY_BYTES {
                resource_exhausted()
            } else {
                invalid_input()
            });
        }
        if value.is_null() {
            continue;
        }
        let value = scalar_query_value(value)?;
        if value.len() > MAX_QUERY_VALUE_BYTES {
            return Err(resource_exhausted());
        }
        query.append_pair(key, &value);
    }
    Ok(())
}

fn scalar_query_value(value: &Value) -> Result<String, SalesforceClientError> {
    match value {
        Value::String(value) => Ok(value.clone()),
        Value::Bool(value) => Ok(value.to_string()),
        Value::Number(value) => Ok(value.to_string()),
        Value::Null | Value::Array(_) | Value::Object(_) => Err(invalid_input()),
    }
}

fn encode_json(value: &Value) -> Result<Vec<u8>, SalesforceClientError> {
    let body = serde_json::to_vec(value).map_err(|_| invalid_input())?;
    if body.len() > MAX_REQUEST_BYTES {
        return Err(resource_exhausted());
    }
    Ok(body)
}

fn map_auth_status(status: StatusCode) -> Result<(), SalesforceClientError> {
    if status == StatusCode::OK {
        return Ok(());
    }
    Err(match status {
        StatusCode::BAD_REQUEST | StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::TOO_MANY_REQUESTS => rate_limited(true),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => timeout(true),
        status if status.is_server_error() => unavailable(true),
        _ => invalid_response(),
    })
}

fn map_resource_status(status: StatusCode, effect: bool) -> SalesforceClientError {
    match status {
        StatusCode::BAD_REQUEST | StatusCode::CONFLICT | StatusCode::UNPROCESSABLE_ENTITY => {
            invalid_input()
        }
        StatusCode::UNAUTHORIZED => authentication(),
        StatusCode::FORBIDDEN => authorization(),
        StatusCode::NOT_FOUND => not_found(),
        StatusCode::TOO_MANY_REQUESTS => rate_limited(!effect),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT if effect => unknown_outcome(),
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => timeout(true),
        status if status.is_server_error() && effect => unknown_outcome(),
        status if status.is_server_error() => unavailable(true),
        _ => invalid_response(),
    }
}

fn json_success(
    response: SalesforceHttpResponse,
    expected: &[StatusCode],
    effect: bool,
) -> Result<Value, SalesforceClientError> {
    if !expected.contains(&response.status) {
        return Err(map_resource_status(response.status, effect));
    }
    if !response.json_content_type {
        return Err(if effect {
            unknown_outcome()
        } else {
            invalid_response()
        });
    }
    let body = response.body.ok_or_else(|| {
        if effect {
            unknown_outcome()
        } else {
            invalid_response()
        }
    })?;
    bounded_output(body, effect)
}

fn no_content_success(
    response: &SalesforceHttpResponse,
    message: &str,
) -> Result<Value, SalesforceClientError> {
    if response.status != StatusCode::NO_CONTENT {
        return Err(map_resource_status(response.status, true));
    }
    if !response.body_was_empty {
        return Err(unknown_outcome());
    }
    bounded_output(json!({"success": true, "message": message}), true)
}

fn bounded_output(value: Value, effect: bool) -> Result<Value, SalesforceClientError> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_response())?
        .len()
        > MAX_OUTPUT_BYTES
    {
        return Err(if effect {
            unknown_outcome()
        } else {
            resource_exhausted()
        });
    }
    Ok(value)
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> SalesforceClientError {
    if effect {
        unknown_outcome()
    } else if source.is_timeout() {
        timeout(true)
    } else {
        unavailable(true)
    }
}

const fn response_bound_failure(effect: bool) -> SalesforceClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

const fn invalid_configuration() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn rate_limited(retryable: bool) -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::RateLimited,
        retryable,
    }
}

const fn timeout(retryable: bool) -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::Timeout,
        retryable,
    }
}

const fn unavailable(retryable: bool) -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::DependencyUnavailable,
        retryable,
    }
}

const fn invalid_response() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> SalesforceClientError {
    SalesforceClientError {
        code: SalesforceClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_map_resource_status(status: StatusCode, effect: bool) -> SalesforceClientError {
    map_resource_status(status, effect)
}
