//! Repair one rejected tool selection before any semantic output reaches ADK.

use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::{Content, ErrorCategory, ErrorComponent, Llm, LlmRequest, LlmResponseStream};

use crate::transport::model_facade::TOOL_NOT_ADMITTED_CODE;

const CORRECTION: &str = "[elitea:tool-selection-repair:v1] The runtime rejected an unavailable tool name. No tool was executed. Continue the user's current task using only the tools declared in this request. If authorization is needed, call the available authorization tool. Never ask the user to name an internal tool. Prior tool availability and Skip decisions do not grant current authorization.";

pub(super) async fn generate(
    inner: Arc<dyn Llm>,
    request: LlmRequest,
    stream: bool,
) -> adk_rust::Result<LlmResponseStream> {
    // The independent owner retains one bounded request for one possible repair.
    // The facade still charges every request against the invocation's turn limit.
    let mut upstream = inner.generate_content(request.clone(), stream).await?;
    let mut repair = Some(request);
    Ok(Box::pin(async_stream::try_stream! {
        loop {
            let Some(item) = upstream.next().await else { return; };
            match item {
                Ok(response) => {
                    // Do not replay partial text, reasoning, tool calls or terminal
                    // output. Once visible, an error must retain normal semantics.
                    if !response.partial || response.content.as_ref().is_some_and(|c| !c.parts.is_empty()) {
                        repair = None;
                    }
                    yield response;
                }
                Err(error) => {
                    let retry = (error.component == ErrorComponent::Model
                        && error.category == ErrorCategory::Unsupported
                        && error.code == TOOL_NOT_ADMITTED_CODE)
                        .then(|| repair.take()).flatten();
                    let Some(mut request) = retry else { Err(error)?; return; };
                    // Drop the failed response before opening the next request.
                    drop(std::mem::replace(&mut upstream, Box::pin(adk_rust::futures::stream::empty())));
                    tracing::warn!(event = "agent_model_tool_selection_repair", upstream_error_code = TOOL_NOT_ADMITTED_CODE,
                        "repairing one rejected model tool selection before dispatch");
                    request.contents.push(Content::new("user").with_text(CORRECTION));
                    upstream = inner.generate_content(request, stream).await?;
                }
            }
        }
    }))
}

#[cfg(test)]
mod tests;
