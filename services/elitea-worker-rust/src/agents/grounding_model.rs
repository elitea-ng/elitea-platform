//! Source numbering for grounding indicators (ADR-0043 decision 5, #1500).
//!
//! [`GroundingModel`] wraps the bound model of a root agent whose version sets
//! `meta.citations` to `required`. Before each model call it numbers the tool
//! results in the request, 1, 2, 3 in request order, and writes the number in
//! front of each result's tool name
//! ([`elitea_agent_runtime::grounding::numbered_source`]). The facades put that
//! name in the untrusted-content envelope, so the model sees
//! `<untrusted source="3:search_index">` and can cite `[3]`. The numbering
//! depends only on the request, so the same history gives the same bytes on
//! every call of the run.
//!
//! The wrapper streams the answer through unchanged. It records the source
//! count of the run's latest call ([`record_sources`]); the event projector
//! takes it when it builds the final message ([`take_sources`]) and computes
//! the indicator from the final answer text
//! ([`elitea_agent_runtime::grounding::indicator`]).
//!
//! The renamed tool name changes only the request copy that goes to the
//! facade. The session history, the step records and the chat keep the real
//! tool name.

use std::collections::HashMap;
use std::sync::{Arc, LazyLock, Mutex};

use adk_rust::{Llm, LlmRequest, LlmResponseStream, Part, SchemaAdapter};
use async_trait::async_trait;
use elitea_agent_runtime::grounding::numbered_source;

/// The most executions whose source count waits for its projector. A run
/// that ends before its final message leaves its entry; the oldest entries
/// go first when the table is full.
const MAX_PENDING_EXECUTIONS: usize = 1_024;

#[derive(Default)]
struct Pending {
    counts: HashMap<String, (u64, usize)>,
    next: u64,
}

static PENDING: LazyLock<Mutex<Pending>> = LazyLock::new(Mutex::default);

fn with_pending<T>(apply: impl FnOnce(&mut Pending) -> T) -> T {
    let mut pending = PENDING
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    apply(&mut pending)
}

/// Record that the latest model call of `execution_id` saw `sources`
/// numbered sources.
pub(crate) fn record_sources(execution_id: &str, sources: usize) {
    if execution_id.is_empty() {
        return;
    }
    with_pending(|pending| {
        pending.next += 1;
        let order = pending.next;
        if pending.counts.len() >= MAX_PENDING_EXECUTIONS
            && !pending.counts.contains_key(execution_id)
            && let Some(oldest) = pending
                .counts
                .iter()
                .min_by_key(|(_, (order, _))| *order)
                .map(|(key, _)| key.clone())
        {
            pending.counts.remove(&oldest);
        }
        pending
            .counts
            .insert(execution_id.to_owned(), (order, sources));
    });
}

/// The source count of the latest model call of `execution_id`, removed from
/// the table. `None` when the run's agent does not cite.
pub(crate) fn take_sources(execution_id: &str) -> Option<usize> {
    with_pending(|pending| pending.counts.remove(execution_id)).map(|(_, sources)| sources)
}

/// Number the tool results of `request` in place; returns their count.
pub(crate) fn number_sources(request: &mut LlmRequest) -> usize {
    let mut ordinal = 0;
    for content in &mut request.contents {
        for part in &mut content.parts {
            if let Part::FunctionResponse {
                function_response, ..
            } = part
            {
                ordinal += 1;
                function_response.name = numbered_source(ordinal, &function_response.name);
            }
        }
    }
    ordinal
}

/// The bound model of a root agent that must cite.
pub(crate) struct GroundingModel {
    inner: Arc<dyn Llm>,
    execution_id: String,
}

impl GroundingModel {
    pub(crate) fn new(inner: Arc<dyn Llm>, execution_id: String) -> Self {
        Self {
            inner,
            execution_id,
        }
    }
}

#[async_trait]
impl Llm for GroundingModel {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        self.inner.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.inner.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream_response: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let mut request = request;
        let sources = number_sources(&mut request);
        record_sources(&self.execution_id, sources);
        self.inner.generate_content(request, stream_response).await
    }
}

#[cfg(test)]
mod tests {
    use adk_rust::{Content, FunctionResponseData};
    use serde_json::json;

    use super::*;

    fn tool_result(name: &str) -> Content {
        let mut content = Content::new("function");
        content.parts.push(Part::FunctionResponse {
            function_response: FunctionResponseData::new(name, json!({"ok": true})),
            id: Some(format!("call-{name}")),
            annotations: None,
        });
        content
    }

    #[test]
    fn tool_results_are_numbered_in_request_order() {
        let mut request = LlmRequest::new(
            "model",
            vec![
                Content::new("user").with_text("Where is it?"),
                tool_result("search_index"),
                tool_result("jira_get_issue"),
                Content::new("model").with_text("Checking."),
                tool_result("search_index"),
            ],
        );
        assert_eq!(number_sources(&mut request), 3);
        let names: Vec<&str> = request
            .contents
            .iter()
            .flat_map(|content| content.parts.iter())
            .filter_map(|part| match part {
                Part::FunctionResponse {
                    function_response, ..
                } => Some(function_response.name.as_str()),
                _ => None,
            })
            .collect();
        assert_eq!(
            names,
            ["1:search_index", "2:jira_get_issue", "3:search_index"]
        );
    }

    #[test]
    fn the_profile_reads_meta_citations() {
        use crate::agents::assembly::OrdinaryNoToolProfile;
        use crate::agents::request::AgentExecutionKind;
        use elitea_agent_runtime::grounding::CitationMode;

        let with_meta = |citations: serde_json::Value| {
            let mut request =
                crate::agents::assembly_tests::ordinary_request(AgentExecutionKind::Application);
            request.payload.application["version_details"]["meta"]["citations"] = citations;
            OrdinaryNoToolProfile::validate(&request).map(|profile| profile.citations())
        };
        assert_eq!(
            with_meta(json!("required")).ok(),
            Some(CitationMode::Required)
        );
        assert_eq!(with_meta(json!("off")).ok(), Some(CitationMode::Off));
        assert_eq!(
            with_meta(serde_json::Value::Null).ok(),
            Some(CitationMode::Off)
        );
        assert!(with_meta(json!("always")).is_err());
        let adhoc = crate::agents::assembly_tests::ordinary_request(AgentExecutionKind::Adhoc);
        assert_eq!(
            OrdinaryNoToolProfile::validate(&adhoc)
                .expect("adhoc profile")
                .citations(),
            CitationMode::Off
        );
    }

    #[test]
    fn the_latest_count_is_taken_once() {
        record_sources("grounding-test-execution", 2);
        record_sources("grounding-test-execution", 5);
        assert_eq!(take_sources("grounding-test-execution"), Some(5));
        assert_eq!(take_sources("grounding-test-execution"), None);
        record_sources("", 3);
        assert_eq!(take_sources(""), None);
    }
}
