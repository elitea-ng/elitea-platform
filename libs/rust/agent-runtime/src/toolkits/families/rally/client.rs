use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use reqwest::header::{
    ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE, HeaderName, HeaderValue, USER_AGENT,
};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value};
use tokio::sync::Mutex;
use zeroize::Zeroizing;

use super::config::RallyToolkitConfig;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(20);
const POOL_IDLE_TIMEOUT: Duration = Duration::from_mins(1);
const MAX_IDLE_PER_HOST: usize = 4;
const MAX_REQUEST_BYTES: usize = 256 * 1_024;
const MAX_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_SECURITY_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_RESULTS: usize = 100;
const USER_AGENT_VALUE: &str = "elitea-worker-rust/0.1";
const JSON_CONTENT_TYPE: &str = "application/json";
const ZSESSIONID: HeaderName = HeaderName::from_static("zsessionid");

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RallyClientErrorCode {
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

/// Stable Rally failure without provider bodies, queries, URLs or credentials.
pub(crate) struct RallyClientError {
    code: RallyClientErrorCode,
    retryable: bool,
}

impl RallyClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> RallyClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            RallyClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "rally.configuration.invalid",
                "the Rally toolkit configuration is invalid",
            ),
            RallyClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "rally.request.invalid",
                "the Rally request is invalid",
            ),
            RallyClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "rally.authentication.failed",
                "Rally authentication failed",
            ),
            RallyClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "rally.authorization.failed",
                "Rally did not authorize the request",
            ),
            RallyClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "rally.resource.not_found",
                "the requested Rally resource was not found",
            ),
            RallyClientErrorCode::RateLimited => (
                ErrorCategory::RateLimited,
                "rally.rate_limited",
                "Rally rate limited the request",
            ),
            RallyClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "rally.timeout",
                "the Rally request timed out",
            ),
            RallyClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "rally.unavailable",
                "Rally is unavailable",
            ),
            RallyClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "rally.response.invalid",
                "Rally returned an invalid response",
            ),
            RallyClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "rally.response.resource_exhausted",
                "the Rally response exceeds the approved limit",
            ),
            RallyClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "rally.effect.unknown_outcome",
                "Rally may have applied the requested effect; reconcile it before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture_for_test(
        code: RallyClientErrorCode,
        retryable: bool,
    ) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for RallyClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RallyClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for RallyClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            RallyClientErrorCode::InvalidConfiguration => {
                "the Rally client configuration is invalid"
            }
            RallyClientErrorCode::InvalidInput => "the Rally request is invalid",
            RallyClientErrorCode::Authentication => "Rally authentication failed",
            RallyClientErrorCode::Authorization => "Rally authorization failed",
            RallyClientErrorCode::NotFound => "the Rally resource was not found",
            RallyClientErrorCode::RateLimited => "Rally rate limited the request",
            RallyClientErrorCode::Timeout => "the Rally request timed out",
            RallyClientErrorCode::DependencyUnavailable => "Rally is unavailable",
            RallyClientErrorCode::InvalidResponse => "Rally returned an invalid response",
            RallyClientErrorCode::ResourceExhausted => {
                "the Rally response exceeds its approved limit"
            }
            RallyClientErrorCode::UnknownOutcome => {
                "the Rally effect outcome is unknown and must be reconciled"
            }
        })
    }
}

impl std::error::Error for RallyClientError {}

#[async_trait]
pub(in crate::toolkits) trait RallyApi: Send + Sync {
    async fn get_types(&self) -> Result<Value, RallyClientError>;
    async fn get_entities(
        &self,
        entity_type: &str,
        query: Option<&str>,
        fetch: bool,
        limit: usize,
    ) -> Result<Value, RallyClientError>;
    async fn get_project(&self, project_name: Option<&str>) -> Result<Value, RallyClientError>;
    async fn get_workspace(&self, workspace_name: Option<&str>) -> Result<Value, RallyClientError>;
    async fn get_user(&self, user_name: Option<&str>) -> Result<Value, RallyClientError>;
    async fn get_context(&self) -> Result<Value, RallyClientError>;
    async fn create_artifact(
        &self,
        entity_type: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, RallyClientError>;
    async fn update_artifact(
        &self,
        entity_type: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, RallyClientError>;
}

pub(in crate::toolkits) struct RallyHttpResponse {
    status: StatusCode,
    body: Option<Value>,
    json_content_type: bool,
}

impl RallyHttpResponse {
    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(status: StatusCode, body: Option<Value>) -> Self {
        Self {
            status,
            body,
            json_content_type: true,
        }
    }
}

#[async_trait]
pub(in crate::toolkits) trait RallyTransport: Send + Sync {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<RallyHttpResponse, RallyClientError>;
}

struct ReqwestRallyTransport {
    http: reqwest::Client,
}

#[async_trait]
impl RallyTransport for ReqwestRallyTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<RallyHttpResponse, RallyClientError> {
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
        let body = if bytes.is_empty() {
            None
        } else {
            serde_json::from_slice(&bytes).ok()
        };
        Ok(RallyHttpResponse {
            status: response.status(),
            body,
            json_content_type,
        })
    }
}

#[derive(Clone, Default)]
struct ContextRefs {
    workspace: Option<Box<str>>,
    project: Option<Box<str>>,
}

/// One lazy, invocation-scoped Rally WSAPI client.
pub(crate) struct RallyClient {
    config: RallyToolkitConfig,
    transport: Arc<dyn RallyTransport>,
    security_token: Mutex<Option<Zeroizing<String>>>,
    context: Mutex<Option<ContextRefs>>,
}

impl RallyClient {
    pub(crate) fn new(config: RallyToolkitConfig) -> Result<Self, RallyClientError> {
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
            Arc::new(ReqwestRallyTransport { http }),
        ))
    }

    fn with_transport_inner(
        config: RallyToolkitConfig,
        transport: Arc<dyn RallyTransport>,
    ) -> Self {
        Self {
            config,
            transport,
            security_token: Mutex::new(None),
            context: Mutex::new(None),
        }
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn with_transport(
        config: RallyToolkitConfig,
        transport: Arc<dyn RallyTransport>,
    ) -> Self {
        Self::with_transport_inner(config, transport)
    }

    fn service_root(&self) -> Result<Url, RallyClientError> {
        self.config
            .origin()
            .join("slm/webservice/v2.0/")
            .map_err(|_| invalid_configuration())
    }

    fn authenticated_request(&self, method: Method, url: Url) -> Result<Request, RallyClientError> {
        let mut request = Request::new(method, url);
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request
            .headers_mut()
            .insert(USER_AGENT, HeaderValue::from_static(USER_AGENT_VALUE));
        if let Some(api_key) = self.config.api_key() {
            let mut value = HeaderValue::from_str(api_key).map_err(|_| invalid_configuration())?;
            value.set_sensitive(true);
            request.headers_mut().insert(ZSESSIONID, value);
        } else if let Some((username, password)) = self.config.basic() {
            let mut source = Zeroizing::new(String::with_capacity(
                username
                    .len()
                    .saturating_add(password.len())
                    .saturating_add(1),
            ));
            source.push_str(username);
            source.push(':');
            source.push_str(password);
            let encoded = Zeroizing::new(BASE64_STANDARD.encode(source.as_bytes()));
            let mut header = Zeroizing::new(String::with_capacity(encoded.len() + 6));
            header.push_str("Basic ");
            header.push_str(&encoded);
            let mut value = HeaderValue::from_str(&header).map_err(|_| invalid_configuration())?;
            value.set_sensitive(true);
            request.headers_mut().insert(AUTHORIZATION, value);
        } else {
            return Err(invalid_configuration());
        }
        Ok(request)
    }

    async fn security_token(&self) -> Result<Option<Zeroizing<String>>, RallyClientError> {
        if self.config.api_key().is_some() {
            return Ok(None);
        }
        let mut token = self.security_token.lock().await;
        if token.is_none() {
            let url = self
                .service_root()?
                .join("security/authorize")
                .map_err(|_| invalid_configuration())?;
            let response = self
                .transport
                .execute(self.authenticated_request(Method::GET, url)?, false)
                .await?;
            let document = response_document(response, false)?;
            let value = document
                .get("OperationResult")
                .and_then(Value::as_object)
                .and_then(|result| result.get("SecurityToken"))
                .and_then(Value::as_str)
                .ok_or_else(invalid_response)?;
            validate_security_token(value)?;
            *token = Some(Zeroizing::new(value.to_owned()));
        }
        Ok(token
            .as_ref()
            .map(|value| Zeroizing::new(value.to_string())))
    }

    async fn context_refs(&self) -> Result<ContextRefs, RallyClientError> {
        let mut state = self.context.lock().await;
        if let Some(context) = state.as_ref() {
            return Ok(context.clone());
        }

        let mut context = ContextRefs::default();
        if let Some(workspace) = self.config.workspace() {
            let query = equality_query("Name", workspace)?;
            let values = self
                .query_unscoped("Workspace", Some(&query), "_ref,Name", 2, None, None)
                .await?;
            context.workspace = Some(exact_reference(&values)?.into());
        }
        if let Some(project) = self.config.project() {
            let query = equality_query("Name", project)?;
            let values = self
                .query_unscoped(
                    "Project",
                    Some(&query),
                    "_ref,Name",
                    2,
                    context.workspace.as_deref(),
                    None,
                )
                .await?;
            context.project = Some(exact_reference(&values)?.into());
        }
        *state = Some(context.clone());
        Ok(context)
    }

    async fn query(
        &self,
        entity: &str,
        query: Option<&str>,
        fetch: &str,
        limit: usize,
    ) -> Result<Vec<Value>, RallyClientError> {
        let context = self.context_refs().await?;
        self.query_with_context(entity, query, fetch, limit, &context)
            .await
    }

    async fn query_with_context(
        &self,
        entity: &str,
        query: Option<&str>,
        fetch: &str,
        limit: usize,
        context: &ContextRefs,
    ) -> Result<Vec<Value>, RallyClientError> {
        self.query_unscoped(
            entity,
            query,
            fetch,
            limit,
            context.workspace.as_deref(),
            context.project.as_deref(),
        )
        .await
    }

    async fn query_unscoped(
        &self,
        entity: &str,
        query: Option<&str>,
        fetch: &str,
        limit: usize,
        workspace: Option<&str>,
        project: Option<&str>,
    ) -> Result<Vec<Value>, RallyClientError> {
        let entity = normalized_entity_type(entity)?;
        if !(1..=MAX_RESULTS).contains(&limit) {
            return Err(invalid_input());
        }
        let mut url = self
            .service_root()?
            .join(&entity)
            .map_err(|_| invalid_input())?;
        {
            let mut pairs = url.query_pairs_mut();
            pairs.append_pair("fetch", fetch);
            if let Some(query) = query {
                pairs.append_pair("query", &format!("({query})"));
            }
            if let Some(workspace) = workspace {
                pairs.append_pair("workspace", workspace);
            }
            if let Some(project) = project {
                pairs.append_pair("project", project);
            }
            pairs.append_pair("projectScopeUp", "false");
            pairs.append_pair("projectScopeDown", "false");
            pairs.append_pair("pagesize", &limit.to_string());
            pairs.append_pair("start", "1");
        }
        let response = self
            .transport
            .execute(self.authenticated_request(Method::GET, url)?, false)
            .await?;
        let document = response_document(response, false)?;
        let mut values = document
            .get("QueryResult")
            .and_then(Value::as_object)
            .and_then(|result| result.get("Results"))
            .and_then(Value::as_array)
            .cloned()
            .ok_or_else(invalid_response)?;
        if values.len() > limit {
            values.truncate(limit);
        }
        bounded_value(&Value::Array(values.clone()), false)?;
        Ok(values)
    }

    async fn effect_request(
        &self,
        method: Method,
        entity_type: &str,
        suffix: &str,
        fields: &Map<String, Value>,
    ) -> Result<Map<String, Value>, RallyClientError> {
        let context = self.context_refs().await?;
        let mut url = self
            .service_root()?
            .join(&format!("{}/{suffix}", entity_type.to_ascii_lowercase()))
            .map_err(|_| invalid_input())?;
        let security_token = self.security_token().await?;
        {
            let mut query = url.query_pairs_mut();
            if let Some(token) = security_token.as_deref() {
                query.append_pair("key", token);
            }
            if let Some(workspace) = context.workspace.as_deref() {
                query.append_pair("workspace", workspace);
            }
            if let Some(project) = context.project.as_deref() {
                query.append_pair("project", project);
            }
            query.append_pair("projectScopeUp", "false");
            query.append_pair("projectScopeDown", "false");
        }
        let mut wrapper = Map::new();
        wrapper.insert(
            entity_type.to_owned(),
            Value::Object(greased_fields(fields)),
        );
        let body = serde_json::to_vec(&Value::Object(wrapper)).map_err(|_| invalid_input())?;
        if body.len() > MAX_REQUEST_BYTES {
            return Err(resource_exhausted());
        }
        let mut request = self.authenticated_request(method, url)?;
        request
            .headers_mut()
            .insert(CONTENT_TYPE, HeaderValue::from_static(JSON_CONTENT_TYPE));
        request.headers_mut().insert(
            CONTENT_LENGTH,
            HeaderValue::from_str(&body.len().to_string()).map_err(|_| invalid_configuration())?,
        );
        *request.body_mut() = Some(body.into());
        let response = self.transport.execute(request, true).await?;
        response_document(response, true)
    }
}

#[async_trait]
impl RallyApi for RallyClient {
    async fn get_types(&self) -> Result<Value, RallyClientError> {
        let values = self
            .query("TypeDefinition", None, "ElementName", MAX_RESULTS)
            .await?;
        let names = values
            .iter()
            .filter_map(Value::as_object)
            .filter_map(|value| value.get("ElementName"))
            .filter_map(Value::as_str)
            .filter(|value| !value.is_empty())
            .map(|value| Value::String(value.to_owned()))
            .collect::<Vec<_>>();
        bounded_value(&Value::Array(names.clone()), false)?;
        Ok(Value::Array(names))
    }

    async fn get_entities(
        &self,
        entity_type: &str,
        query: Option<&str>,
        fetch: bool,
        limit: usize,
    ) -> Result<Value, RallyClientError> {
        let values = self
            .query(
                entity_type,
                query,
                if fetch { "true" } else { "false" },
                limit,
            )
            .await?;
        Ok(Value::Array(values))
    }

    async fn get_project(&self, project_name: Option<&str>) -> Result<Value, RallyClientError> {
        let name = project_name.or_else(|| self.config.project());
        let query = name
            .map(|value| equality_query("Name", value))
            .transpose()?;
        Ok(Value::Array(
            self.query("Project", query.as_deref(), "true", MAX_RESULTS)
                .await?,
        ))
    }

    async fn get_workspace(&self, workspace_name: Option<&str>) -> Result<Value, RallyClientError> {
        let name = workspace_name.or_else(|| self.config.workspace());
        let query = name
            .map(|value| equality_query("Name", value))
            .transpose()?;
        Ok(Value::Array(
            self.query("Workspace", query.as_deref(), "true", MAX_RESULTS)
                .await?,
        ))
    }

    async fn get_user(&self, user_name: Option<&str>) -> Result<Value, RallyClientError> {
        let query = user_name
            .map(|value| equality_query("UserName", value))
            .transpose()?;
        Ok(Value::Array(
            self.query("User", query.as_deref(), "true", MAX_RESULTS)
                .await?,
        ))
    }

    async fn get_context(&self) -> Result<Value, RallyClientError> {
        let (project, workspace, user) = tokio::try_join!(
            self.get_project(None),
            self.get_workspace(None),
            self.get_user(None)
        )?;
        let mut result = Map::new();
        result.insert("project".to_owned(), project);
        result.insert("workspace".to_owned(), workspace);
        result.insert("user".to_owned(), user);
        let value = Value::Object(result);
        bounded_value(&value, false)?;
        Ok(value)
    }

    async fn create_artifact(
        &self,
        entity_type: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, RallyClientError> {
        let entity_type = normalized_entity_type(entity_type)?;
        let document = self
            .effect_request(Method::PUT, &entity_type, "create", fields)
            .await?;
        let object = nested_result_object(&document, "CreateResult")?;
        let identifier = if let Some(value) = object.get("FormattedID").and_then(Value::as_str) {
            validate_identifier(value).map_err(|_| unknown_outcome())?;
            value.into()
        } else {
            reference_oid(object)?
        };
        effect_success_message("Entity", &identifier, "created")
    }

    async fn update_artifact(
        &self,
        entity_type: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, RallyClientError> {
        let entity_type = normalized_entity_type(entity_type)?;
        let formatted_input = fields.get("FormattedID").and_then(Value::as_str);
        if let Some(value) = formatted_input {
            validate_identifier(value)?;
        }
        let object_input = fields.get("ObjectID");
        if object_input.is_some() == formatted_input.is_some() {
            return Err(invalid_input());
        }
        let oid = if let Some(value) = object_input {
            object_id(value)?
        } else if let Some(formatted) = formatted_input {
            let query = equality_query("FormattedID", formatted)?;
            let values = self
                .query(&entity_type, Some(&query), "ObjectID", 2)
                .await?;
            exact_object_id(&values)?
        } else {
            return Err(invalid_input());
        };
        if fields
            .keys()
            .all(|key| matches!(key.as_str(), "ObjectID" | "FormattedID"))
        {
            return Err(invalid_input());
        }
        let document = self
            .effect_request(Method::POST, &entity_type, &oid, fields)
            .await?;
        let object = nested_result_object(&document, "UpdateResult")?;
        let identifier = if let Some(value) = object
            .get("FormattedID")
            .and_then(Value::as_str)
            .or(formatted_input)
        {
            validate_identifier(value).map_err(|_| unknown_outcome())?;
            value.into()
        } else {
            oid
        };
        effect_success_message("Artifact", &identifier, "updated")
    }
}

fn response_document(
    response: RallyHttpResponse,
    effect: bool,
) -> Result<Map<String, Value>, RallyClientError> {
    map_status(response.status, effect)?;
    if !response.json_content_type {
        return Err(response_shape_failure(effect));
    }
    let document = response
        .body
        .and_then(|value| value.as_object().cloned())
        .ok_or_else(|| response_shape_failure(effect))?;
    for name in [
        "QueryResult",
        "CreateResult",
        "UpdateResult",
        "OperationResult",
    ] {
        if document
            .get(name)
            .and_then(Value::as_object)
            .and_then(|value| value.get("Errors"))
            .and_then(Value::as_array)
            .is_some_and(|errors| !errors.is_empty())
        {
            return Err(invalid_input());
        }
    }
    Ok(document)
}

fn greased_fields(fields: &Map<String, Value>) -> Map<String, Value> {
    fields
        .iter()
        .map(|(name, value)| {
            let value = match value {
                Value::Array(_) if !is_collection_field(name) => value.clone(),
                Value::Array(values) => Value::Array(
                    values
                        .iter()
                        .map(|value| match value {
                            Value::String(reference) if is_short_reference(reference) => {
                                let mut object = Map::new();
                                object.insert("_ref".to_owned(), Value::String(reference.clone()));
                                Value::Object(object)
                            }
                            value => value.clone(),
                        })
                        .collect(),
                ),
                value => value.clone(),
            };
            (name.clone(), value)
        })
        .collect()
}

fn is_collection_field(name: &str) -> bool {
    name.eq_ignore_ascii_case("children") || name.ends_with('s')
}

fn is_short_reference(value: &str) -> bool {
    value.contains('/')
        && value
            .rsplit('/')
            .next()
            .is_some_and(|oid| !oid.is_empty() && oid.bytes().all(|byte| byte.is_ascii_digit()))
}

fn nested_result_object<'a>(
    document: &'a Map<String, Value>,
    result_name: &str,
) -> Result<&'a Map<String, Value>, RallyClientError> {
    document
        .get(result_name)
        .and_then(Value::as_object)
        .and_then(|value| value.get("Object"))
        .and_then(Value::as_object)
        .ok_or_else(unknown_outcome)
}

fn exact_reference(values: &[Value]) -> Result<&str, RallyClientError> {
    if values.len() != 1 {
        return Err(invalid_input());
    }
    values[0]
        .as_object()
        .and_then(|value| value.get("_ref"))
        .and_then(Value::as_str)
        .filter(|value| value.starts_with('/'))
        .ok_or_else(invalid_response)
}

fn exact_object_id(values: &[Value]) -> Result<Box<str>, RallyClientError> {
    if values.len() != 1 {
        return Err(invalid_input());
    }
    let value = values[0]
        .as_object()
        .and_then(|value| value.get("ObjectID"))
        .ok_or_else(invalid_response)?;
    object_id(value)
}

fn object_id(value: &Value) -> Result<Box<str>, RallyClientError> {
    let value = match value {
        Value::Number(value) => value.as_u64().map(|value| value.to_string()),
        Value::String(value) => Some(value.clone()),
        _ => None,
    }
    .ok_or_else(invalid_input)?;
    if value.is_empty() || value.len() > 32 || !value.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(invalid_input());
    }
    Ok(value.into())
}

fn reference_oid(object: &Map<String, Value>) -> Result<Box<str>, RallyClientError> {
    let reference = object
        .get("_ref")
        .and_then(Value::as_str)
        .ok_or_else(unknown_outcome)?;
    let oid = reference.rsplit('/').next().ok_or_else(unknown_outcome)?;
    object_id(&Value::String(oid.to_owned())).map_err(|_| unknown_outcome())
}

fn equality_query(field: &str, value: &str) -> Result<String, RallyClientError> {
    if value.is_empty()
        || value.len() > 4 * 1_024
        || value.contains(['"', '\\'])
        || value.chars().any(char::is_control)
    {
        return Err(invalid_input());
    }
    Ok(format!(r#"{field} = "{value}""#))
}

pub(in crate::toolkits) fn normalized_entity_type(value: &str) -> Result<String, RallyClientError> {
    let value = match value {
        "Story" | "UserStory" | "User Story" => "HierarchicalRequirement",
        value => value,
    };
    let parts = value.split('/').collect::<Vec<_>>();
    let valid_route = parts.len() == 1 || (parts.len() == 2 && parts[0] == "PortfolioItem");
    if value.is_empty()
        || value.len() > 128
        || !valid_route
        || parts.iter().any(|part| {
            part.is_empty()
                || !part
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
        })
    {
        return Err(invalid_input());
    }
    Ok(value.to_owned())
}

fn validate_identifier(value: &str) -> Result<(), RallyClientError> {
    if value.is_empty()
        || value.len() > 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err(invalid_input());
    }
    Ok(())
}

fn validate_security_token(value: &str) -> Result<(), RallyClientError> {
    if value.len() > MAX_SECURITY_TOKEN_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_response());
    }
    Ok(())
}

fn bounded_value(value: &Value, effect: bool) -> Result<(), RallyClientError> {
    let size = serde_json::to_vec(value)
        .map_err(|_| response_shape_failure(effect))?
        .len();
    if size > MAX_OUTPUT_BYTES {
        return Err(response_bound_failure(effect));
    }
    Ok(())
}

fn effect_success_message(
    subject: &str,
    identifier: &str,
    action: &str,
) -> Result<Value, RallyClientError> {
    let value = Value::String(format!("{subject} {identifier} {action} successfully."));
    bounded_value(&value, true)?;
    Ok(value)
}

fn map_status(status: StatusCode, effect: bool) -> Result<(), RallyClientError> {
    match status {
        StatusCode::OK | StatusCode::CREATED => Ok(()),
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => Err(invalid_input()),
        StatusCode::UNAUTHORIZED => Err(authentication()),
        StatusCode::FORBIDDEN => Err(authorization()),
        StatusCode::NOT_FOUND => Err(not_found()),
        StatusCode::TOO_MANY_REQUESTS => Err(rate_limited()),
        status if status.is_server_error() => Err(if effect {
            unknown_outcome()
        } else {
            unavailable()
        }),
        _ => Err(response_shape_failure(effect)),
    }
}

fn map_reqwest_error(source: &reqwest::Error, effect: bool) -> RallyClientError {
    if effect {
        return unknown_outcome();
    }
    if source.is_timeout() {
        timeout()
    } else if source.is_connect() || source.is_request() || source.is_body() {
        unavailable()
    } else {
        invalid_response()
    }
}

const fn response_shape_failure(effect: bool) -> RallyClientError {
    if effect {
        unknown_outcome()
    } else {
        invalid_response()
    }
}

const fn response_bound_failure(effect: bool) -> RallyClientError {
    if effect {
        unknown_outcome()
    } else {
        resource_exhausted()
    }
}

const fn invalid_configuration() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn rate_limited() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::RateLimited,
        retryable: true,
    }
}

const fn timeout() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::Timeout,
        retryable: true,
    }
}

const fn unavailable() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::DependencyUnavailable,
        retryable: true,
    }
}

const fn invalid_response() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unknown_outcome() -> RallyClientError {
    RallyClientError {
        code: RallyClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}
