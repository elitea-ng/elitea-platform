//! The admitted profile of a toolkit-internal completion.
//!
//! A toolkit tool that needs a model (the Office `fill_template` and
//! `translate_presentation`, `describe_image`, the step-back index tools) asks
//! for ONE text reply to ONE user message: no tools, no schema, no streaming
//! of its own. The authorized providers admit only the streaming profile of an
//! agent turn (`validate_llm_request`: `stream == true` and a generation
//! config equal to the frozen invocation's), so asking them directly was
//! refused as "outside the admitted ordinary profile" on every live call
//! (`model_gateway.invalid_request`, `tool_count=0`, `streaming=false`).
//!
//! This adapter is that profile, stated once and checked: it admits exactly
//! the shape above, stamps the invocation's own generation config on it, runs
//! it through the provider's streaming path, and folds the stream into the one
//! complete reply the caller asked for. Admission of every other caller is
//! unchanged.

use std::sync::Arc;

use adk_rust::futures::{StreamExt as _, stream};
use adk_rust::{
    AdkError, Content, ErrorCategory, ErrorComponent, FinishReason, GenerateContentConfig, Llm,
    LlmRequest, LlmResponse, LlmResponseStream, Part,
};
use async_trait::async_trait;

use super::openai_compatible_facade::ModelFacadeInvocation;

/// The most reply text one toolkit completion folds.
const MAX_REPLY_BYTES: usize = 256 * 1024;

/// Wrap a bound provider model so a toolkit tool can ask it for one reply.
pub(crate) fn toolkit_completion_model(
    inner: Arc<dyn Llm>,
    invocation: &ModelFacadeInvocation,
) -> Arc<dyn Llm> {
    Arc::new(ToolkitCompletionModel {
        inner,
        model_name: invocation.model_name.clone(),
        temperature: invocation.temperature,
        max_output_tokens: invocation
            .max_tokens
            .and_then(|value| i32::try_from(value).ok()),
    })
}

struct ToolkitCompletionModel {
    inner: Arc<dyn Llm>,
    model_name: String,
    temperature: Option<f32>,
    max_output_tokens: Option<i32>,
}

#[async_trait]
impl Llm for ToolkitCompletionModel {
    fn name(&self) -> &str {
        &self.model_name
    }

    async fn generate_content(
        &self,
        mut request: LlmRequest,
        streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        if streaming
            || request.model != self.model_name
            || request.config.is_some()
            || !request.tools.is_empty()
            || request.previous_response_id.is_some()
            || request.contents.len() != 1
            || request.contents[0].role != "user"
        {
            return Err(refused(
                "toolkit_completion.request",
                "the toolkit completion request is outside its admitted profile",
            ));
        }
        request.config = Some(GenerateContentConfig {
            temperature: self.temperature,
            max_output_tokens: self.max_output_tokens,
            ..GenerateContentConfig::default()
        });
        let mut responses = self.inner.generate_content(request, true).await?;
        let mut text = String::new();
        let mut terminal: Option<LlmResponse> = None;
        while let Some(response) = responses.next().await {
            let response = response?;
            if terminal.is_some() {
                return Err(refused(
                    "toolkit_completion.after_terminal",
                    "the model replied after its reply was complete",
                ));
            }
            if let Some(content) = &response.content {
                for part in &content.parts {
                    match part {
                        Part::Text { text: delta } => {
                            if text.len().saturating_add(delta.len()) > MAX_REPLY_BYTES {
                                return Err(refused(
                                    "toolkit_completion.reply_size",
                                    "the model's reply exceeds the toolkit completion bound",
                                ));
                            }
                            text.push_str(delta);
                        }
                        // Provider reasoning is not the reply.
                        Part::Thinking { .. } => {}
                        _ => {
                            return Err(refused(
                                "toolkit_completion.reply_part",
                                "the model's reply carries a part a toolkit completion cannot use",
                            ));
                        }
                    }
                }
            }
            if !response.partial {
                terminal = Some(response);
            }
        }
        let mut response = terminal.ok_or_else(|| {
            refused(
                "toolkit_completion.no_terminal",
                "the model returned no complete reply",
            )
        })?;
        if response.error_code.is_some() || response.finish_reason != Some(FinishReason::Stop) {
            // A reply cut short (length, safety) is not a reply; the caller
            // names the reason it reads from the finish.
            let reason = response.finish_reason;
            return Err(refused(
                "toolkit_completion.finish",
                match reason {
                    Some(FinishReason::MaxTokens) => {
                        "the model's reply was cut off at its length limit"
                    }
                    Some(FinishReason::Safety) => "the model declined to reply",
                    _ => "the model ended the reply abnormally",
                },
            ));
        }
        response.content = Some(Content::new("model").with_text(text));
        response.partial = false;
        Ok(Box::pin(stream::once(async move { Ok(response) })))
    }
}

fn refused(code: &'static str, message: &'static str) -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::InvalidInput,
        code,
        message,
    )
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use adk_rust::{Content, GenerateContentConfig, LlmRequest, LlmResponse};

    use super::*;
    use crate::transport::openai_compatible_facade::{
        test_model_facade_invocation, validate_llm_request,
    };

    /// Records what reached the provider and streams two deltas then a
    /// terminal, as the real facade does (the terminal carries no text).
    struct Provider {
        seen: Mutex<Vec<(LlmRequest, bool)>>,
        finish: FinishReason,
    }

    #[async_trait]
    impl Llm for Provider {
        fn name(&self) -> &'static str {
            "fixture-model"
        }

        async fn generate_content(
            &self,
            request: LlmRequest,
            streaming: bool,
        ) -> adk_rust::Result<LlmResponseStream> {
            self.seen.lock().expect("seen").push((request, streaming));
            let delta = |text: &str| LlmResponse {
                content: Some(Content::new("model").with_text(text)),
                partial: true,
                ..LlmResponse::default()
            };
            let terminal = LlmResponse {
                finish_reason: Some(self.finish),
                turn_complete: true,
                ..LlmResponse::default()
            };
            Ok(Box::pin(stream::iter(vec![
                Ok(delta("Hello, ")),
                Ok(delta("world")),
                Ok(terminal),
            ])))
        }
    }

    fn request() -> LlmRequest {
        LlmRequest {
            model: "fixture-model".to_owned(),
            contents: vec![Content::new("user").with_text("Fill the template.")],
            config: None,
            tools: std::collections::HashMap::new(),
            previous_response_id: None,
        }
    }

    async fn run(model: &Arc<dyn Llm>, request: LlmRequest) -> adk_rust::Result<String> {
        let mut responses = model.generate_content(request, false).await?;
        let mut text = String::new();
        while let Some(response) = responses.next().await {
            let response = response?;
            assert!(!response.partial, "the caller sees one complete reply");
            for part in response.content.iter().flat_map(|content| &content.parts) {
                if let Part::Text { text: piece } = part {
                    text.push_str(piece);
                }
            }
        }
        Ok(text)
    }

    #[test]
    fn the_bare_toolkit_request_is_outside_the_ordinary_profile() {
        // The live defect: no config and no streaming.
        let invocation = test_model_facade_invocation();
        assert!(validate_llm_request(&request(), false, &invocation).is_err());
        assert!(validate_llm_request(&request(), true, &invocation).is_err());
    }

    #[tokio::test]
    async fn a_toolkit_completion_reaches_the_provider_inside_the_admitted_profile() {
        let invocation = test_model_facade_invocation();
        let provider = Arc::new(Provider {
            seen: Mutex::new(Vec::new()),
            finish: FinishReason::Stop,
        });
        let model = toolkit_completion_model(provider.clone() as Arc<dyn Llm>, &invocation);
        assert_eq!(run(&model, request()).await.expect("reply"), "Hello, world");
        let seen = provider.seen.lock().expect("seen");
        let (sent, streaming) = &seen[0];
        assert!(*streaming);
        // The very check that refused the bare request now admits it.
        assert!(validate_llm_request(sent, *streaming, &invocation).is_ok());
    }

    #[tokio::test]
    async fn anything_but_one_plain_user_message_is_refused_before_the_provider() {
        let invocation = test_model_facade_invocation();
        let provider = Arc::new(Provider {
            seen: Mutex::new(Vec::new()),
            finish: FinishReason::Stop,
        });
        let model = toolkit_completion_model(provider.clone() as Arc<dyn Llm>, &invocation);
        let mut with_config = request();
        with_config.config = Some(GenerateContentConfig::default());
        let mut with_tool = request();
        with_tool
            .tools
            .insert("t".to_owned(), serde_json::json!({"name": "t"}));
        let mut two = request();
        two.contents.push(Content::new("user").with_text("more"));
        let mut other_model = request();
        other_model.model = "other".to_owned();
        for bad in [with_config, with_tool, two, other_model] {
            assert!(run(&model, bad).await.is_err());
        }
        // A streaming ask is not this profile either.
        assert!(model.generate_content(request(), true).await.is_err());
        assert!(provider.seen.lock().expect("seen").is_empty());
    }

    #[tokio::test]
    async fn a_reply_cut_short_is_not_a_reply() {
        let invocation = test_model_facade_invocation();
        let provider = Arc::new(Provider {
            seen: Mutex::new(Vec::new()),
            finish: FinishReason::MaxTokens,
        });
        let model = toolkit_completion_model(provider as Arc<dyn Llm>, &invocation);
        let error = run(&model, request()).await.expect_err("cut off");
        assert!(error.to_string().contains("length limit"), "{error}");
    }
}
