//! Fixtures for the delegated-authorization behaviour of `openapi` operation
//! tools, shared by this crate's tests and the worker's pipeline suite
//! (`test-support`; never in a production build).

use std::collections::{BTreeMap, VecDeque};
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_core::Toolset;
use adk_tool::SimpleToolContext;
use reqwest::{Request, StatusCode};
use serde_json::{Map, json};

use std::sync::Arc;

use adk_core::Tool;
use async_trait::async_trait;
use serde_json::Value;

use super::{OpenApiOperationTool, admit_materialized_toolset};
use crate::toolkits::families::openapi::client::{
    OpenApiAccessToken, OpenApiRequest, OpenApiResponse, OpenApiTransport,
};
use crate::toolkits::families::openapi::client::{OpenApiClient, OpenApiClientError};
use crate::toolkits::families::openapi::config::OpenApiToolkitConfig;
use crate::toolkits::policy::ToolAdmissionPolicy;

pub struct RejectedTokenTransport {
    pub responses: Mutex<VecDeque<StatusCode>>,
    pub calls: AtomicUsize,
    pub requests: Mutex<Vec<(String, String)>>,
}

#[async_trait]
impl OpenApiTransport for RejectedTokenTransport {
    async fn execute(
        &self,
        request: OpenApiRequest,
    ) -> Result<OpenApiResponse, OpenApiClientError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.requests.lock().unwrap().push((
            request.uri().to_string(),
            request
                .headers()
                .get(reqwest::header::AUTHORIZATION)
                .map_or("", |value| value.to_str().unwrap())
                .to_owned(),
        ));
        Ok(OpenApiResponse {
            status: self.responses.lock().unwrap().pop_front().unwrap(),
            body: b"private-provider-body".to_vec(),
        })
    }

    async fn token(&self, _request: Request) -> Result<OpenApiAccessToken, OpenApiClientError> {
        panic!("delegated resource calls must not exchange credentials");
    }
}

pub fn settings(delegated: bool) -> Map<String, Value> {
    let mut settings = json!({
        "spec": {
            "openapi": "3.0.3",
            "servers": [{"url": "https://api.example.test/v1"}],
            "paths": {"/records": {"get": {
                "operationId": "list_records",
                "parameters": [{"name":"marker", "in":"query", "schema":{"type":"string"}}],
                "responses": {"200": {"description": "Records"}}
            }}}
        },
        "selected_tools": ["list_records"]
    });
    if delegated {
        settings["openapi_configuration"] = json!({
            "configuration_uuid": "config-1",
            "client_id": "stored-client",
            "client_secret": "private-stored-secret",
            "oauth_discovery_endpoint": "https://issuer.example.test/tenant",
            "scope": "records.read"
        });
    }
    settings.as_object().unwrap().clone()
}

pub async fn operation_tool(
    delegated: bool,
    token: &str,
    transport: Arc<RejectedTokenTransport>,
) -> Arc<dyn Tool> {
    let tokens = json!({
        "config-1:https://issuer.example.test/tenant": {"access_token": token}
    });
    let config = OpenApiToolkitConfig::parse(
        "Records API",
        &settings(delegated),
        tokens.as_object().unwrap(),
    )
    .unwrap()
    .with_toolkit_id(Some(27));
    let operation = Arc::new(config.operations()[0].clone());
    let client = Arc::new(OpenApiClient::with_transport(
        config.into_client_parts(),
        transport,
    ));
    let tool = Arc::new(OpenApiOperationTool::new(
        operation,
        "Records API",
        "https://api.example.test/v1",
        client,
    ));
    let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).unwrap());
    admit_materialized_toolset("Records API", "openapi", &policy, vec![tool])
        .unwrap()
        .tools(context())
        .await
        .unwrap()
        .pop()
        .unwrap()
}

pub fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("openapi-test")
            .with_session_id("session")
            .with_function_call_id("original-call"),
    )
}
