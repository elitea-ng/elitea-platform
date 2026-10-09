use std::collections::VecDeque;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

use reqwest::StatusCode;
use serde_json::json;

use super::test_support::{RejectedTokenTransport, context, operation_tool};
use super::*;
use crate::toolkits::delegated_authorization_requirement;

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
