use std::collections::{BTreeMap, VecDeque};
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_rust::Toolset;
use adk_rust::tool::SimpleToolContext;
use reqwest::{Request, StatusCode};
use serde_json::{Map, json};

use super::*;
use crate::toolkits::delegated_authorization_requirement;
use crate::toolkits::families::openapi::client::{
    OpenApiAccessToken, OpenApiRequest, OpenApiResponse, OpenApiTransport,
};

struct RejectedTokenTransport {
    responses: Mutex<VecDeque<StatusCode>>,
    calls: AtomicUsize,
    requests: Mutex<Vec<(String, String)>>,
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

fn settings(delegated: bool) -> Map<String, Value> {
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

async fn operation_tool(
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

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("openapi-test")
            .with_session_id("session")
            .with_function_call_id("original-call"),
    )
}

#[tokio::test]
async fn delegated_openapi_expiry_preserves_bound_authorization_without_automatic_retry() {
    let transport = Arc::new(RejectedTokenTransport {
        responses: Mutex::new(VecDeque::from([
            StatusCode::OK,
            StatusCode::UNAUTHORIZED,
            StatusCode::OK,
        ])),
        calls: AtomicUsize::new(0),
        requests: Mutex::new(Vec::new()),
    });
    let context = context();
    let tool = operation_tool(true, "private-expired-token", Arc::clone(&transport)).await;
    assert!(tool.execute(context.clone(), json!({})).await.is_ok());
    let error = tool.execute(context.clone(), json!({})).await.unwrap_err();
    let requirement = delegated_authorization_requirement(&error)
        .expect("typed delegated authorization after resource 401");
    assert_eq!(requirement.toolkit_name(), "Records API");
    assert_eq!(requirement.toolkit_type(), "openapi");
    assert_eq!(requirement.server_url(), "https://api.example.test/v1");
    let metadata = requirement.resource_metadata().unwrap();
    assert_eq!(metadata["configuration_uuid"], "config-1");
    assert_eq!(metadata["toolkit_id"], "27");
    let visible = format!("{error:?} {error} {metadata}");
    for secret in [
        "private-expired-token",
        "private-stored-secret",
        "private-provider-body",
    ] {
        assert!(!visible.contains(secret));
    }
    assert_eq!(
        transport.calls.load(Ordering::SeqCst),
        2,
        "401 must not retry the protected operation"
    );
    let rebuilt = operation_tool(true, "private-rotated-token", Arc::clone(&transport)).await;
    assert!(rebuilt.execute(context, json!({})).await.is_ok());
    assert_eq!(transport.calls.load(Ordering::SeqCst), 3);
    assert_eq!(
        transport.requests.lock().unwrap()[2].1,
        "Bearer private-rotated-token"
    );
}

#[tokio::test]
async fn other_openapi_failures_do_not_become_delegated_authorization() {
    for (delegated, status) in [
        (false, StatusCode::UNAUTHORIZED),
        (true, StatusCode::FORBIDDEN),
        (true, StatusCode::TOO_MANY_REQUESTS),
        (true, StatusCode::INTERNAL_SERVER_ERROR),
    ] {
        let transport = Arc::new(RejectedTokenTransport {
            responses: Mutex::new(VecDeque::from([status])),
            calls: AtomicUsize::new(0),
            requests: Mutex::new(Vec::new()),
        });
        let tool = operation_tool(delegated, "private-token", Arc::clone(&transport)).await;
        let error = tool.execute(context(), json!({})).await.unwrap_err();
        assert!(delegated_authorization_requirement(&error).is_none());
        assert_eq!(transport.calls.load(Ordering::SeqCst), 1);
    }
}

#[path = "tools_pipeline_tests.rs"]
mod pipeline_tests;
