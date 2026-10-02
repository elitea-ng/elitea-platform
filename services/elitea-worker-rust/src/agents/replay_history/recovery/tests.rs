use std::collections::VecDeque;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_rust::{AdkError, LlmResponse, Part};
use async_trait::async_trait;
use serde_json::json;

use super::*;

#[derive(Default)]
struct ScriptedModel {
    responses: Mutex<VecDeque<Vec<adk_rust::Result<LlmResponse>>>>,
    requests: Mutex<Vec<LlmRequest>>,
    dropped: Arc<AtomicUsize>,
}

struct ResponseOwner(Arc<AtomicUsize>);
impl Drop for ResponseOwner {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::AcqRel);
    }
}

#[async_trait]
impl Llm for ScriptedModel {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.requests.lock().unwrap().push(request);
        let response = self.responses.lock().unwrap().pop_front();
        let Some(response) = response else {
            return std::future::pending().await;
        };
        let owner = ResponseOwner(Arc::clone(&self.dropped));
        Ok(Box::pin(async_stream::stream! {
            let _owner = owner;
            for item in response { yield item; }
        }))
    }
}

fn rejection(code: &'static str) -> adk_rust::Result<LlmResponse> {
    Err(AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::Unsupported,
        code,
        "fixture rejection",
    ))
}

fn model(responses: Vec<Vec<adk_rust::Result<LlmResponse>>>) -> Arc<ScriptedModel> {
    Arc::new(ScriptedModel {
        responses: Mutex::new(responses.into()),
        ..Default::default()
    })
}

fn request() -> LlmRequest {
    let mut request = LlmRequest::new(
        "fixture",
        vec![Content::new("user").with_text("Use the attached toolkit")],
    );
    request.tools.insert(
        "authorize_fixture".into(),
        json!({"name": "authorize_fixture", "parameters": {"type": "object"}}),
    );
    request
}

fn auth_call() -> LlmResponse {
    LlmResponse::new(Content {
        role: "model".into(),
        parts: vec![Part::FunctionCall {
            name: "authorize_fixture".into(),
            args: json!({}),
            id: Some("call-new".into()),
            thought_signature: None,
        }],
    })
}

#[tokio::test]
async fn rejected_selection_gets_one_repair_with_the_same_admitted_tools() {
    let inner = model(vec![
        vec![rejection(TOOL_NOT_ADMITTED_CODE)],
        vec![Ok(auth_call())],
    ]);
    let original = request();
    let mut stream = generate(inner.clone(), original.clone(), true)
        .await
        .unwrap();
    assert_eq!(
        json!(stream.next().await.unwrap().unwrap().content),
        json!(auth_call().content)
    );
    assert!(stream.next().await.is_none());
    let requests = inner.requests.lock().unwrap();
    assert_eq!(requests.len(), 2);
    assert_eq!(json!(requests[0].contents), json!(original.contents));
    assert_eq!(requests[1].tools, original.tools);
    assert_eq!(requests[1].model, original.model);
    assert_eq!(
        json!(requests[1].contents[..original.contents.len()]),
        json!(original.contents)
    );
    assert_eq!(
        requests[1].contents.last().unwrap().parts,
        Content::new("user").with_text(CORRECTION).parts
    );
    assert_eq!(inner.dropped.load(Ordering::Acquire), 2);
}

#[tokio::test]
async fn repeated_rejection_is_bounded_and_other_errors_are_not_retried() {
    for code in [
        TOOL_NOT_ADMITTED_CODE,
        "model_gateway.invalid_sse",
        "model_gateway.max_turns",
    ] {
        let inner = model(vec![vec![rejection(code)], vec![rejection(code)]]);
        let mut stream = generate(inner.clone(), request(), true).await.unwrap();
        assert_eq!(stream.next().await.unwrap().unwrap_err().code, code);
        assert!(stream.next().await.is_none());
        assert_eq!(
            inner.requests.lock().unwrap().len(),
            if code == TOOL_NOT_ADMITTED_CODE { 2 } else { 1 }
        );
    }
}

#[tokio::test]
async fn semantic_output_disables_repair_to_prevent_duplicate_output_or_dispatch() {
    let partial = LlmResponse {
        partial: true,
        content: Some(Content::new("model").with_text("partial answer")),
        ..Default::default()
    };
    for response in [
        partial,
        auth_call(),
        LlmResponse::new(Content::new("model").with_text("done")),
    ] {
        let inner = model(vec![vec![Ok(response), rejection(TOOL_NOT_ADMITTED_CODE)]]);
        let mut stream = generate(inner.clone(), request(), true).await.unwrap();
        assert!(stream.next().await.unwrap().is_ok());
        assert_eq!(
            stream.next().await.unwrap().unwrap_err().code,
            TOOL_NOT_ADMITTED_CODE
        );
        assert_eq!(inner.requests.lock().unwrap().len(), 1);
    }
}

#[tokio::test]
async fn dropping_an_unpolled_response_does_not_start_repair() {
    let inner = model(vec![vec![rejection(TOOL_NOT_ADMITTED_CODE)]]);
    let stream = generate(inner.clone(), request(), true).await.unwrap();
    drop(stream);
    assert_eq!(inner.requests.lock().unwrap().len(), 1);
    assert_eq!(inner.dropped.load(Ordering::Acquire), 1);
}

#[tokio::test]
async fn repair_drops_failed_response_and_cancellation_does_not_start_more_work() {
    let inner = model(vec![vec![rejection(TOOL_NOT_ADMITTED_CODE)]]);
    let mut stream = generate(inner.clone(), request(), true).await.unwrap();
    assert!(adk_rust::futures::poll!(stream.next()).is_pending());
    assert_eq!(inner.requests.lock().unwrap().len(), 2);
    assert_eq!(inner.dropped.load(Ordering::Acquire), 1);
    drop(stream);
    assert_eq!(inner.requests.lock().unwrap().len(), 2);
}
