//! Source numbering for grounding indicators (ADR-0043 decision 5, #1500).
//!
//! [`GroundingModel`] wraps the bound model of a root agent whose version sets
//! `meta.citations` to `required`. Before each model call it numbers the tool
//! results of the current turn, 1, 2, 3 in request order, and writes the
//! number in front of each result's tool name
//! ([`elitea_agent_runtime::grounding::numbered_source`]). The facades put that
//! name in the untrusted-content envelope, so the model sees
//! `<untrusted source="3:search_index">` and can cite `[3]`.
//!
//! The current turn starts at the last user message of the request. A tool
//! result before that message is from an earlier turn. It keeps its real tool
//! name, so its envelope has no number, and the citation rule tells the model
//! to cite only numbered blocks. Thus the source count does not grow over a
//! conversation. The numbering depends only on the request, so the same
//! history gives the same bytes on every call of the run.
//!
//! The wrapper streams the answer through unchanged. It writes the source
//! count of the run's latest call into the run's own [`GroundingSources`].
//! The root event projector of the same run holds a clone of that handle and
//! reads it when it builds the final message. The handle lives only as long
//! as the run's binding and projector, so nothing stays behind when a run
//! pauses or fails.
//!
//! The renamed tool name changes only the request copy that goes to the
//! facade. The session history, the step records and the chat keep the real
//! tool name.

use std::sync::{Arc, Mutex, PoisonError};

use adk_rust::{Content, Llm, LlmRequest, LlmResponseStream, Part, SchemaAdapter};
use async_trait::async_trait;
use elitea_agent_runtime::grounding::numbered_source;

/// The source count of one run's latest model call. The model binding writes
/// it and the root event projector reads it. Clones share one value.
#[derive(Clone, Debug, Default)]
pub(crate) struct GroundingSources(Arc<Mutex<Option<usize>>>);

impl GroundingSources {
    /// Record that the latest model call saw `sources` numbered sources.
    pub(crate) fn record(&self, sources: usize) {
        *self.0.lock().unwrap_or_else(PoisonError::into_inner) = Some(sources);
    }

    /// The source count of the latest model call; `None` before the first
    /// call.
    #[must_use]
    pub(crate) fn latest(&self) -> Option<usize> {
        *self.0.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// The shared value, so a test can watch when the last owner drops it.
    #[cfg(test)]
    pub(crate) fn handle_for_tests(&self) -> Arc<Mutex<Option<usize>>> {
        Arc::clone(&self.0)
    }
}

/// Whether `content` is a user message: a `user` content with a part that is
/// not a tool result. An empty `user` content (a resume input) is not one.
fn is_user_message(content: &Content) -> bool {
    content.role == "user"
        && content
            .parts
            .iter()
            .any(|part| !matches!(part, Part::FunctionResponse { .. }))
}

/// Number the tool results of the current turn of `request` in place, and
/// return their count. The current turn is everything after the last user
/// message (all of the request when it has none).
pub(crate) fn number_sources(request: &mut LlmRequest) -> usize {
    let turn_start = request
        .contents
        .iter()
        .rposition(is_user_message)
        .map_or(0, |index| index + 1);
    let mut ordinal = 0;
    for content in &mut request.contents[turn_start..] {
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
    sources: GroundingSources,
}

impl GroundingModel {
    pub(crate) fn new(inner: Arc<dyn Llm>, sources: GroundingSources) -> Self {
        Self { inner, sources }
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
        self.sources.record(number_sources(&mut request));
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

    fn numbered_names(request: &LlmRequest) -> Vec<&str> {
        request
            .contents
            .iter()
            .flat_map(|content| content.parts.iter())
            .filter_map(|part| match part {
                Part::FunctionResponse {
                    function_response, ..
                } => Some(function_response.name.as_str()),
                _ => None,
            })
            .collect()
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
        assert_eq!(
            numbered_names(&request),
            ["1:search_index", "2:jira_get_issue", "3:search_index"]
        );
    }

    #[test]
    fn only_the_current_turn_is_numbered() {
        // Two earlier turns with tool results, then the current turn. An
        // empty user content (a resume input) does not start a turn.
        let mut request = LlmRequest::new(
            "model",
            vec![
                Content::new("user").with_text("First question"),
                tool_result("search_index"),
                Content::new("model").with_text("First answer [1]."),
                Content::new("user").with_text("Second question"),
                tool_result("jira_get_issue"),
                tool_result("search_index"),
                Content::new("model").with_text("Second answer [1, 2]."),
                Content::new("user").with_text("Third question"),
                tool_result("confluence_search"),
                Content::new("user"),
                tool_result("search_index"),
            ],
        );
        assert_eq!(number_sources(&mut request), 2);
        assert_eq!(
            numbered_names(&request),
            [
                "search_index",
                "jira_get_issue",
                "search_index",
                "1:confluence_search",
                "2:search_index",
            ]
        );
        // The same history gives the same count on the next call of the turn.
        let mut again = LlmRequest::new(
            "model",
            vec![
                Content::new("user").with_text("First question"),
                tool_result("search_index"),
                Content::new("user").with_text("Third question"),
            ],
        );
        assert_eq!(number_sources(&mut again), 0);
        assert_eq!(numbered_names(&again), ["search_index"]);
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
        // A stored value that Main would refuse today (an older row, or a
        // write path that did not check) turns citations off. It does not
        // refuse the turn.
        assert_eq!(with_meta(json!("always")).ok(), Some(CitationMode::Off));
        assert_eq!(with_meta(json!(true)).ok(), Some(CitationMode::Off));
        let adhoc = crate::agents::assembly_tests::ordinary_request(AgentExecutionKind::Adhoc);
        assert_eq!(
            OrdinaryNoToolProfile::validate(&adhoc)
                .expect("adhoc profile")
                .citations(),
            CitationMode::Off
        );
    }

    #[test]
    fn clones_share_the_latest_count() {
        let sources = GroundingSources::default();
        let projector_side = sources.clone();
        assert_eq!(projector_side.latest(), None);
        sources.record(2);
        sources.record(5);
        assert_eq!(projector_side.latest(), Some(5));
    }
}
