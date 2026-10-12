//! Recorded-provider fixtures shared by the four Azure DevOps family suites.

// The response helpers are returned directly from fixture handlers.
#![allow(clippy::unnecessary_wraps)]

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::{BasicToolset, SimpleToolContext};
use async_trait::async_trait;
use elitea_connectors::transport::{Body, Method, Request, StatusCode};
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, IF_MATCH};
use serde_json::{Map, Value, json};

use super::families::ado::client::{
    AdoBody, AdoClient, AdoClientError, AdoHttpResponse, AdoTransport,
};
use super::families::ado::config::{AdoFamilySettings, AdoToolkitConfig};
use super::policy::ToolAdmissionPolicy;

pub(super) const TOKEN: &str = "pat-super-secret";

pub(super) fn settings(extra: &Value) -> Map<String, Value> {
    let mut settings = json!({
        "ado_configuration":{
            "organization_url":"https://dev.azure.com/contoso/",
            "token":TOKEN
        },
        "project":"Fabrikam Fiber",
        "limit":5,
        "selected_tools":[]
    })
    .as_object()
    .cloned()
    .expect("ADO fixture settings are an object");
    if let Some(extra) = extra.as_object() {
        for (key, value) in extra {
            settings.insert(key.clone(), value.clone());
        }
    }
    settings
}

pub(super) fn config(extra: &Value) -> AdoToolkitConfig {
    AdoToolkitConfig::parse(&settings(extra)).expect("valid ADO configuration")
}

pub(super) fn client(
    extra: &Value,
    transport: Arc<FixtureTransport>,
) -> (AdoClient, AdoFamilySettings) {
    let (connection, family) = config(extra).into_parts();
    (AdoClient::with_transport(connection, transport), family)
}

pub(super) fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("ADO policy fixture"))
}

pub(super) fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("ado-test").with_function_call_id("ado-call"))
}

pub(super) async fn tools(toolset: &BasicToolset) -> Vec<Arc<dyn Tool>> {
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("ADO tools")
}

pub(super) fn tool<'a>(tools: &'a [Arc<dyn Tool>], name: &str) -> &'a Arc<dyn Tool> {
    tools
        .iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("tool {name} is served"))
}

#[derive(Clone, Debug)]
pub(super) struct CapturedRequest {
    pub(super) method: Method,
    pub(super) path: String,
    pub(super) query: Vec<(String, String)>,
    pub(super) body: Option<Value>,
    pub(super) content_type: Option<String>,
    pub(super) authorization: Option<String>,
    pub(super) authorization_sensitive: bool,
    pub(super) if_match: Option<String>,
    pub(super) effect: bool,
}

impl CapturedRequest {
    pub(super) fn query_value(&self, name: &str) -> Option<&str> {
        self.query
            .iter()
            .find(|(key, _)| key == name)
            .map(|(_, value)| value.as_str())
    }
}

type Handler =
    dyn Fn(&CapturedRequest, usize) -> Result<AdoHttpResponse, AdoClientError> + Send + Sync;

pub(super) struct FixtureTransport {
    requests: Mutex<Vec<CapturedRequest>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    pub(super) fn new(
        handler: impl Fn(&CapturedRequest, usize) -> Result<AdoHttpResponse, AdoClientError>
        + Send
        + Sync
        + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        })
    }

    pub(super) fn requests(&self) -> Vec<CapturedRequest> {
        self.requests
            .lock()
            .expect("ADO request fixture lock")
            .clone()
    }
}

#[async_trait]
impl AdoTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<AdoHttpResponse, AdoClientError> {
        let header = |name| {
            request
                .headers()
                .get(name)
                .and_then(|value: &reqwest::header::HeaderValue| value.to_str().ok())
                .map(ToOwned::to_owned)
        };
        let captured = CapturedRequest {
            method: request.method().clone(),
            path: percent_decode(request.url().path()),
            query: request
                .url()
                .query_pairs()
                .map(|(key, value)| (key.into_owned(), value.into_owned()))
                .collect(),
            body: request
                .body()
                .and_then(Body::as_bytes)
                .and_then(|bytes| serde_json::from_slice(bytes).ok()),
            content_type: header(CONTENT_TYPE),
            authorization: header(AUTHORIZATION),
            authorization_sensitive: request
                .headers()
                .get(AUTHORIZATION)
                .is_some_and(reqwest::header::HeaderValue::is_sensitive),
            if_match: header(IF_MATCH),
            effect,
        };
        let mut requests = self.requests.lock().expect("ADO request fixture lock");
        let index = requests.len();
        requests.push(captured.clone());
        drop(requests);
        (self.handler)(&captured, index)
    }
}

fn percent_decode(path: &str) -> String {
    percent_encoding::percent_decode_str(path)
        .decode_utf8_lossy()
        .into_owned()
}

pub(super) fn ok(body: Value) -> Result<AdoHttpResponse, AdoClientError> {
    Ok(AdoHttpResponse::fixture(
        StatusCode::OK,
        AdoBody::Json(body),
    ))
}

pub(super) fn ok_with_etag(body: &Value, etag: &str) -> Result<AdoHttpResponse, AdoClientError> {
    Ok(AdoHttpResponse::fixture(StatusCode::OK, AdoBody::Json(body.clone())).with_etag(etag))
}

pub(super) fn text(body: &str) -> Result<AdoHttpResponse, AdoClientError> {
    Ok(AdoHttpResponse::fixture(
        StatusCode::OK,
        AdoBody::Text(body.to_owned()),
    ))
}

pub(super) fn status(
    code: StatusCode,
    message: Option<&str>,
) -> Result<AdoHttpResponse, AdoClientError> {
    Ok(AdoHttpResponse::fixture(
        code,
        message.map_or(AdoBody::Empty, |message| {
            AdoBody::Json(json!({
                "$id":"1","innerException":null,"message":message,
                "typeName":"Microsoft.VisualStudio.Services.Common.VssServiceException",
                "typeKey":"VssServiceException","errorCode":0,"eventId":3000
            }))
        }),
    ))
}

pub(super) fn collection(values: &Value) -> Result<AdoHttpResponse, AdoClientError> {
    ok(json!({"count":values.as_array().map_or(0, Vec::len),"value":values}))
}

/// The Basic credential the SDK's `BasicAuthentication('', token)` sends.
pub(super) fn expected_authorization() -> String {
    use base64::Engine as _;
    format!(
        "Basic {}",
        base64::engine::general_purpose::STANDARD.encode(format!(":{TOKEN}"))
    )
}
